package cephstore

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/fts"
	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// Layout 3 of the full-text index (stage B14b)
//
// Layout 2 is the fixed set of objects of every version before 0.45.0:
// server.fts_shards posting objects "fts/<nn>", 256 dictionary objects
// "ftst/<nnn>" and server.fts_shards header objects "ftsh/<nn>". A
// delivery touches nearly all of them. Layout 3 keeps the same keys and
// values in an index generation whose object counts follow the size of
// the account:
//
//	"ftx/<gen>/p<i>"  postings, shard fnv1a(term) mod S, with the markers
//	                  "!g" (generation), "!s" (live|frozen:<tok>), "!n"
//	                  (terms) and "!dn" (dictionary keys of the terms)
//	"ftx/<gen>/d<j>"  dictionary, shard fnv1a(trigram) mod D, marker "!g"
//	"ftx/<gen>/h<k>"  header texts, shard fnv1a(GUID) mod H, marker "!g"
//
// <gen> is a random token. The layout record "L" in the id map names the
// current generation, a conversion in progress ("next") and sources a
// conversion has not removed yet ("old"); without "L" an account has
// layout 2. Objects of a generation are created only when the
// generation is created (a fresh index, a rebuild, a conversion); a
// writer never creates one, and never writes into a generation the
// record no longer names: an anomaly on any object (a freeze, a missing
// object, a foreign "!g") makes it read the record and either repeat the
// whole indexing in the generation named there, wait for a conversion,
// lift a freeze no conversion names any more, or fail (§4.3 of the
// plan). Every full enumeration of an object skips the keys that start
// with "!"; no prefix scan can reach them.
const (
	ftxPrefix    = "ftx/"
	ftsLayoutKey = "L"
	// ftsVersionKey3 is the version marker of a layout 3 index (layout 2
	// keeps ftsVersionKey): a version before 0.45.0 that reads a layout 3
	// account by mistake finds no marker and narrows nothing.
	ftsVersionKey3 = "V"
	// ftsCountKey is the number of documents of a layout 3 index.
	ftsCountKey = "c"

	markGen   = "!g"
	markState = "!s"
	markTerms = "!n"
	markDict  = "!dn"

	stateLive    = "live"
	frozenPrefix = "frozen:"

	// postingsPerCAS is how many terms one posting compare-and-swap
	// reads and writes: a page of keys less the markers.
	postingsPerCAS = omapPageSize - 4

	// reshardFactor: a shard holding this many times the target asks
	// for a reshard.
	reshardFactor = 1.5
)

// markerPrefix is the first byte of every marker key.
const markerPrefix = "!"

// isMarker reports whether an omap key is a marker, not a term or text.
func isMarker(key string) bool { return strings.HasPrefix(key, markerPrefix) }

// Errors of the layout rules.
var (
	// errFTSShardMissing: an object of the generation the layout record
	// names is missing (or carries another generation); the scrub reports
	// it, fts_rebuild repairs it.
	errFTSShardMissing = fmt.Errorf("%w: object missing, repair with fts_rebuild", model.ErrIndexUnavailable)
	// errFTSFrozen: a conversion holds the account's posting objects
	// frozen longer than fts.frozen_wait_seconds.
	errFTSFrozen = fmt.Errorf("%w: it is being converted, try again later", model.ErrIndexUnavailable)
	// errFTSRetries: the index generation changed more often than one
	// operation follows it.
	errFTSRetries = fmt.Errorf("%w: its generation changed too often, try again later", model.ErrIndexUnavailable)
)

// ftsGen is one generation in the layout record: the current one, a
// conversion target ("next") or a source not removed yet ("old").
type ftsGen struct {
	F int    `json:"f"`
	G string `json:"g,omitempty"`
	S int    `json:"s,omitempty"`
	D int    `json:"d,omitempty"`
	H int    `json:"h,omitempty"`
	// Tok is the run that writes it (next) or removes it (old).
	Tok string `json:"tok,omitempty"`
	// At is the last heartbeat of the import reshard endpoint's run
	// (Unix seconds; 0 for a job), in "next" and, after the switch, in
	// "old".
	At int64 `json:"at,omitempty"`
	// DX and AH are the generation's properties (see ftsLayout).
	DX *ftsDX `json:"dx,omitempty"`
	AH string `json:"ah,omitempty"`
	// On, M and Hold belong to the "next" of an online run (stage B14i,
	// ftsonline.go): On makes writers record what they index for the run
	// (the keys "w:<guid>" and "w#" of the id map) and makes its freeze
	// one they write through; M is its mode (model.FTSModeCopy or
	// model.FTSModeReindex); Hold (Unix milliseconds, 0 none) makes new
	// id allocations wait until then, so that the run's switch can catch
	// up. A version before 0.51.0 drops all three when it rewrites the
	// record: the run notices the missing On at its switch and stops.
	On   bool   `json:"on,omitempty"`
	M    string `json:"m,omitempty"`
	Hold int64  `json:"hold,omitempty"`
}

// ftsDX is the identifier filter of a generation's dictionary (B14h,
// temp/13 §4.3): a body term that fts.DictIdentifier names for N runes
// has one start entry "#<tri0>:b:<token>" instead of its trigram keys.
// V is the version of the rule (1).
type ftsDX struct {
	V int `json:"v"`
	N int `json:"n"`
}

// dxRule is the version of the identifier rule ftsDX.V names.
const dxRule = 1

// newDX is the filter of n runes (nil for 0).
func newDX(n int) *ftsDX {
	if n <= 0 {
		return nil
	}

	return &ftsDX{V: dxRule, N: n}
}

// dxRunes is the identifier length of a filter (0: none). A filter of a
// rule this version does not know counts as none here: its writers then
// write trigram keys (a superset), and its readers read the start
// entries anyway.
func dxRunes(d *ftsDX) int {
	if d == nil || d.V != dxRule {
		return 0
	}

	return d.N
}

// ftsLayout is the layout record "L" of the id map.
type ftsLayout struct {
	F int    `json:"f"`
	G string `json:"g,omitempty"`
	S int    `json:"s,omitempty"`
	D int    `json:"d,omitempty"`
	H int    `json:"h,omitempty"`
	// Dict is whether the generation has a substring dictionary (always
	// so far; B14f may create generations without).
	Dict bool `json:"dict"`
	// LG is set once the layout 2 objects of the account are gone.
	LG   bool     `json:"lg,omitempty"`
	Next *ftsGen  `json:"next,omitempty"`
	Old  []ftsGen `json:"old,omitempty"`
	// DX is the identifier filter of the generation's dictionary (nil:
	// none). AH is its text preparation: "" every header field of
	// attached messages, fts.AttachedHeadersNoSig without the signature
	// and trace fields. Both are fixed for the life of the generation:
	// writers, removals and conversions take them from here, never from
	// the configuration (temp/13 §2, §4.3). A version before 0.50.0
	// drops both when it rewrites the record; the version marker
	// ftsVersionText keeps "nosig" known to the readers then.
	DX *ftsDX `json:"dx,omitempty"`
	AH string `json:"ah,omitempty"`
}

// parseLayout decodes the layout record; nil for none.
func parseLayout(v []byte) (*ftsLayout, error) {
	if len(v) == 0 {
		return nil, nil
	}

	var l ftsLayout
	if err := json.Unmarshal(v, &l); err != nil {
		return nil, fmt.Errorf("fts layout record: %w", err)
	}

	if l.F != model.FTSLayout2 && l.F != model.FTSLayout3 {
		return nil, fmt.Errorf("fts layout record: unknown layout %d", l.F)
	}

	return &l, nil
}

func (l *ftsLayout) encode() []byte {
	b, _ := json.Marshal(l)
	return b
}

// ftsLoc is one generation as an operation reads and writes it.
type ftsLoc struct {
	f       int    // layout
	g       string // generation (layout 3)
	s, d, h int    // posting, dictionary and header objects
	// dx is the identifier filter of the dictionary in runes (0: none),
	// ah the text preparation ("" or fts.AttachedHeadersNoSig); layout 2
	// has neither.
	dx int
	ah string
}

// layout2Loc is layout 2 with n posting and header objects.
func layout2Loc(n int) ftsLoc {
	return ftsLoc{f: model.FTSLayout2, s: n, d: dictShards, h: n}
}

// genLoc is the location of a generation of the record.
func genLoc(g ftsGen, n int) ftsLoc {
	if g.F != model.FTSLayout3 {
		return layout2Loc(n)
	}

	return ftsLoc{f: model.FTSLayout3, g: g.G, s: g.S, d: g.D, h: g.H, dx: dxRunes(g.DX), ah: g.AH}
}

// curLoc is the current generation of a record (nil: layout 2).
func curLoc(l *ftsLayout, n int) ftsLoc {
	if l == nil || l.F != model.FTSLayout3 {
		return layout2Loc(n)
	}

	return ftsLoc{f: model.FTSLayout3, g: l.G, s: l.S, d: l.D, h: l.H, dx: dxRunes(l.DX), ah: l.AH}
}

// same reports whether two locations are the same generation.
func (l ftsLoc) same(o ftsLoc) bool { return l.f == o.f && l.g == o.g }

func (l ftsLoc) v3() bool { return l.f == model.FTSLayout3 }

func (l ftsLoc) shape() model.FTSLayoutShape {
	if !l.v3() {
		return model.FTSLayoutShape{Layout: model.FTSLayout2, S: l.s, D: l.d, H: l.h}
	}

	return model.FTSLayoutShape{Layout: l.f, Gen: l.g, S: l.s, D: l.d, H: l.h, DictIDMinRunes: l.dx, AttachedHeaders: l.ah}
}

// text are the text options of the generation's terms.
func (l ftsLoc) text() fts.TextOptions { return fts.TextOptions{AttachedHeaders: l.ah} }

// newLayoutRecord is the layout record of a fresh layout 3 generation
// (a fresh index, a rebuild).
func newLayoutRecord(loc ftsLoc) *ftsLayout {
	return &ftsLayout{F: model.FTSLayout3, G: loc.g, S: loc.s, D: loc.d, H: loc.h, Dict: true, LG: true, DX: newDX(loc.dx), AH: loc.ah}
}

// sameProps reports whether two generations have the same identifier
// filter and text preparation.
func (l ftsLoc) sameProps(o ftsLoc) bool { return l.dx == o.dx && l.ah == o.ah }

// gen is the generation as a record entry (without a run token).
func (l ftsLoc) gen() ftsGen {
	return ftsGen{F: l.f, G: l.g, S: l.s, D: l.d, H: l.h, DX: newDX(l.dx), AH: l.ah}
}

func (l ftsLoc) postingOIDn(i int) string {
	if l.v3() {
		return fmt.Sprintf("%s%s/p%d", ftxPrefix, l.g, i)
	}

	return fmt.Sprintf("%s%02d", ftsPrefix, i)
}

func (l ftsLoc) dictOIDn(i int) string {
	if l.v3() {
		return fmt.Sprintf("%s%s/d%d", ftxPrefix, l.g, i)
	}

	return fmt.Sprintf("%s%03d", dictPrefix, i)
}

func (l ftsLoc) headerOIDn(i int) string {
	if l.v3() {
		return fmt.Sprintf("%s%s/h%d", ftxPrefix, l.g, i)
	}

	return fmt.Sprintf("%s%02d", headerPrefix, i)
}

func (l ftsLoc) postingOID(term string) string { return l.postingOIDn(shardNo(term, l.s)) }
func (l ftsLoc) dictOID(tri string) string     { return l.dictOIDn(shardNo(tri, l.d)) }
func (l ftsLoc) headerOID(guid string) string  { return l.headerOIDn(shardNo(guid, l.h)) }

func (l ftsLoc) postingOIDs() []string { return oidsOf(l.s, l.postingOIDn) }
func (l ftsLoc) dictOIDs() []string    { return oidsOf(l.d, l.dictOIDn) }
func (l ftsLoc) headerOIDs() []string  { return oidsOf(l.h, l.headerOIDn) }

// objects are every object of the generation (layout 2: with the
// dictionary of the layout before 0.35.0).
func (l ftsLoc) objects() []string {
	out := append(append(l.postingOIDs(), l.dictOIDs()...), l.headerOIDs()...)
	if !l.v3() {
		out = append(out, oidsOf(l.s, func(i int) string { return fmt.Sprintf("%s%02d", legacyDictPrefix, i) })...)
	}

	return out
}

func oidsOf(n int, name func(int) string) []string {
	out := make([]string, n)
	for i := range n {
		out[i] = name(i)
	}

	return out
}

// newFTSGen returns a fresh generation token: 16 hex characters.
func newFTSGen() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}

	return hex.EncodeToString(b[:])
}

// nameGens returns every generation the record names: current, next and
// old (layout 2 as one entry), without duplicates.
func nameGens(l *ftsLayout, n int) []ftsLoc {
	var out []ftsLoc

	add := func(loc ftsLoc) {
		for _, o := range out {
			if o.same(loc) {
				return
			}
		}

		out = append(out, loc)
	}

	add(curLoc(l, n))

	if l == nil {
		return out
	}

	if l.Next != nil {
		add(genLoc(*l.Next, n))
	}

	for _, o := range l.Old {
		add(genLoc(o, n))
	}

	if !l.LG {
		add(layout2Loc(n))
	}

	return out
}

// --- anomalies ----------------------------------------------------------------

// Kinds of anomaly an operation finds on an index object.
const (
	anomFrozen  = iota // "!s" is frozen:<tok>
	anomMissing        // the object does not exist (or a layout 3 object lacks "!g")
	anomForeign        // "!g" names another generation
	anomMoved          // a reader's look at the record found another generation
)

// ftsAnomaly is what an operation found on an object: it reads the
// layout record and follows the rules of §4.3.
type ftsAnomaly struct {
	kind  int
	oid   string
	state string // the "!s" value of a frozen object
}

func (a *ftsAnomaly) Error() string {
	switch a.kind {
	case anomFrozen:
		return "fts object " + a.oid + " is frozen (" + a.state + ")"
	case anomForeign:
		return "fts object " + a.oid + " belongs to another generation"
	case anomMoved:
		return "fts index moved to another generation"
	default:
		return "fts object " + a.oid + " is missing"
	}
}

func asAnomaly(err error) *ftsAnomaly {
	var a *ftsAnomaly
	if errors.As(err, &a) {
		return a
	}

	return nil
}

// checkMarkers applies the reader and writer checks to the markers of an
// object read in loc: a missing layout 3 object (no "!g"), a foreign
// generation and a frozen state are anomalies. exists is whether the
// object exists.
func checkMarkers(loc ftsLoc, oid string, exists bool, cur map[string][]byte) *ftsAnomaly {
	if loc.v3() {
		g, ok := cur[markGen]

		switch {
		case !exists || !ok:
			return &ftsAnomaly{kind: anomMissing, oid: oid}
		case string(g) != loc.g:
			return &ftsAnomaly{kind: anomForeign, oid: oid}
		}
	}

	if s := string(cur[markState]); strings.HasPrefix(s, frozenPrefix) {
		return &ftsAnomaly{kind: anomFrozen, oid: oid, state: s}
	}

	return nil
}

// --- the layout record --------------------------------------------------------

// readLayout reads the layout record (one read, no cache).
func (u *User) readLayout() (*ftsLayout, error) {
	vals, err := getOmapKeys(u.idx, oidFTSMap, []string{ftsLayoutKey})
	if err != nil {
		if errCode(err) == syscall.ENOENT {
			return nil, nil
		}

		return nil, fmt.Errorf("fts layout record: %w", err)
	}

	return parseLayout(vals[ftsLayoutKey])
}

// FTSLayout implements model.FTSLayouter.
func (u *User) FTSLayout() (model.FTSLayoutState, error) {
	l, err := u.readLayout()
	if err != nil {
		return model.FTSLayoutState{}, err
	}

	return layoutState(l, u.ftsShards), nil
}

// layoutState is the record as model.FTSLayoutState.
func layoutState(l *ftsLayout, n int) model.FTSLayoutState {
	st := model.FTSLayoutState{Current: curLoc(l, n).shape()}
	if l == nil {
		return st
	}

	if l.Next != nil {
		sh := genLoc(*l.Next, n).shape()
		st.Next, st.NextTok, st.NextOnline, st.NextMode = &sh, l.Next.Tok, l.Next.On, l.Next.M

		if l.Next.At > 0 {
			st.NextAt = time.Unix(l.Next.At, 0).UTC()
		}
	}

	for _, o := range l.Old {
		st.Old = append(st.Old, genLoc(o, n).shape())
		st.OldToks = append(st.OldToks, o.Tok)

		var at time.Time
		if o.At > 0 {
			at = time.Unix(o.At, 0).UTC()
		}

		st.OldAt = append(st.OldAt, at)
	}

	return st
}

// --- markers and counters -------------------------------------------------------

func atoiMarker(v []byte) int64 {
	n, _ := strconv.ParseInt(string(v), 10, 64)
	return n
}

func itoaMarker(n int64) []byte { return []byte(strconv.FormatInt(max(n, 0), 10)) }

// dictKeysOf is the number of dictionary keys a term has in the
// generation loc: one per distinct trigram of its token, none in a field
// without substring matching, one (the start entry) for an identifier
// in a generation with the filter (fts.DictKeys).
func dictKeysOf(loc ftsLoc, term string) int { return fts.DictKeys(term, loc.dx) }

// --- sizes --------------------------------------------------------------------

// Kinds of layout 3 objects (metric labels, plan results) and their
// index in objectKinds.
const (
	kindPostings   = "postings"
	kindDictionary = "dictionary"
	kindHeaders    = "headers"

	kindIndexPostings   = 0
	kindIndexDictionary = 1
	kindIndexHeaders    = 2
)

// objectKinds are the kinds of layout 3 objects.
var objectKinds = [...]string{kindPostings, kindDictionary, kindHeaders}

// The caps withDefaults gives a sizing whose cap is below its lower
// bound (a configuration that sets none): the default of fts.shards_max
// before 0.49.0 for the postings and header texts, the default of
// fts.dict_shards_max for the dictionary; and the default target.
const (
	defaultShardsCap     = 32
	defaultDictShardsCap = 512
	defaultTargetKeys    = 100000
)

// ftsSizing are the bounds of the layout 3 object counts (configuration
// fts.shards_min; fts.shards_max and fts.shard_target_keys for the
// postings and header texts; fts.dict_shards_max and
// fts.dict_target_keys for the dictionary).
type ftsSizing struct {
	min, max, target    int
	dictMax, dictTarget int
}

func (z ftsSizing) withDefaults() ftsSizing {
	if z.min <= 0 {
		z.min = 1
	}

	if z.max < z.min {
		z.max = max(defaultShardsCap, z.min)
	}

	if z.dictMax < z.min {
		z.dictMax = max(defaultDictShardsCap, z.min)
	}

	if z.target <= 0 {
		z.target = defaultTargetKeys
	}

	if z.dictTarget <= 0 {
		z.dictTarget = defaultTargetKeys
	}

	return z
}

// count is the posting (or header) object count for keys keys: the next
// power of two of keys/target, within the bounds; capped reports a count
// that the upper bound cut.
func (z ftsSizing) count(keys int64) (n int, capped bool) {
	return countWithin(keys, z.target, z.min, z.max)
}

// dictCount is count for the dictionary objects.
func (z ftsSizing) dictCount(keys int64) (n int, capped bool) {
	return countWithin(keys, z.dictTarget, z.min, z.dictMax)
}

// countWithin is the next power of two of keys/target within [lo, hi].
func countWithin(keys int64, target, lo, hi int) (n int, capped bool) {
	need := (keys + int64(target) - 1) / int64(target)
	n = pow2(need)

	if n > hi {
		return hi, true
	}

	return max(n, lo), false
}

// pow2 is the smallest power of two at least n (1 for n <= 1).
func pow2(n int64) int {
	if n <= 1 {
		return 1
	}

	return 1 << bits.Len64(uint64(n-1))
}

// estimatePoints are the calibration points of estimateKeys: messages,
// terms, dictionary keys. Up to 5000 messages they come from the
// simulation of the bench corpus without identifiers (plan B14 §1.2:
// about 85 dictionary keys per message at 5000); the last one is the
// count of the largest account on croessner (2026-10-02: 36246
// messages, 2069828 terms, 10138532 dictionary keys, about 280 per
// message; key counts only, temp/13 §1.1).
var estimatePoints = [...][3]float64{
	{0, 0, 0}, {50, 12731, 68045}, {500, 45007, 258988}, {5000, 86693, 424610}, {36246, 2069828, 10138532},
}

// estimateKeys estimates the vocabulary (terms) and the dictionary keys
// of an account of m messages, interpolated linearly between the
// calibration points and beyond the last with the slope of the last
// section: (2069828 − 86693)/31246 ≈ 63.5 terms and (10138532 −
// 424610)/31246 ≈ 310.9 dictionary keys per message, above the 57 and
// 280 of the account itself, so that a fresh index rather starts too
// large. The section from 5000 to 36246 messages bridges the bench
// corpus without identifiers and a real account (the knee at 5000;
// 10000 messages come out at about 1.98 million dictionary keys, 198
// per message): an estimate, not a measurement. Below 5000 messages
// the function underestimates accounts with identifiers; their trigger
// corrects it after the first growth.
func estimateKeys(m int) (terms, dict int64) {
	pts := estimatePoints
	x := float64(max(m, 0))

	for i := 1; i < len(pts); i++ {
		if x <= pts[i][0] || i == len(pts)-1 {
			a, b := pts[i-1], pts[i]
			f := (x - a[0]) / (b[0] - a[0])

			return int64(a[1] + f*(b[1]-a[1])), int64(a[2] + f*(b[2]-a[2]))
		}
	}

	return 0, 0
}

// shapeFor returns the object counts of a fresh generation for an
// account of m messages (m 0: the lower bound).
func (z ftsSizing) shapeFor(m int) (s, d, h int) {
	if m <= 0 {
		return z.min, z.min, z.min
	}

	terms, dict := estimateKeys(m)
	s, _ = z.count(terms)
	d, _ = z.dictCount(dict)
	h, _ = z.count(int64(m))

	return s, d, h
}

// --- creating a generation ------------------------------------------------------

// createGen creates the objects of a layout 3 generation exclusively with
// their markers: the only place besides a conversion where layout 3
// objects come into being.
func (u *User) createGen(loc ftsLoc) error {
	g := []byte(loc.g)

	create := func(oid string, marks map[string][]byte) error {
		op := rados.CreateWriteOp()
		defer op.Release()

		op.Create(rados.CreateExclusive)
		op.SetOmap(marks)

		if err := op.Operate(u.idx, oid, rados.OperationNoFlag); err != nil {
			return fmt.Errorf("create %s: %w", oid, err)
		}

		return nil
	}

	for _, oid := range loc.postingOIDs() {
		if err := create(oid, map[string][]byte{markGen: g, markState: []byte(stateLive), markTerms: []byte("0"), markDict: []byte("0")}); err != nil {
			return err
		}
	}

	for _, oid := range append(loc.dictOIDs(), loc.headerOIDs()...) {
		if err := create(oid, map[string][]byte{markGen: g}); err != nil {
			return err
		}
	}

	return nil
}

// deleteObjects removes objects (absent ones are fine), eight at a time.
func (u *User) deleteObjects(oids []string) error {
	groups := make(map[string][]string, len(oids))
	for _, oid := range oids {
		groups[oid] = nil
	}

	return parallelN(groups, dictWriters, func(oid string, _ []string) error {
		if err := u.idx.Delete(oid); err != nil && errCode(err) != syscall.ENOENT {
			return fmt.Errorf("delete %s: %w", oid, err)
		}

		return nil
	})
}

// --- the cluster mark and the presence index ----------------------------------

// FTSLayout3Enabled implements model.FTSLayoutMarked: whether the
// cluster mark "fts/layout3" of the admin store exists. It is read
// without a cache (one stat).
func (s *Store) FTSLayout3Enabled() (bool, error) {
	return s.markSet(model.FTSLayoutMarkKey, "fts layout mark")
}

// FTSOnlineEnabled implements model.FTSOnlineMarked: whether the cluster
// mark "fts/online" of the admin store exists (one stat, no cache).
func (s *Store) FTSOnlineEnabled() (bool, error) {
	return s.markSet(model.FTSOnlineMarkKey, "fts online mark")
}

// FTSDictFilterEnabled implements model.FTSDictFilterMarked: whether the
// cluster mark "fts/dictfilter" of the admin store exists (one stat, no
// cache).
func (s *Store) FTSDictFilterEnabled() (bool, error) {
	return s.markSet(model.FTSDictFilterMarkKey, "fts dictionary filter mark")
}

// markSet reports whether the admin store record key exists.
func (s *Store) markSet(key, what string) (bool, error) {
	u, err := s.User(adminNamespace)
	if err != nil {
		return false, err
	}
	defer u.Close()

	_, err = u.idx.Stat(adminOID(key))

	switch {
	case err == nil:
		return true, nil
	case errCode(err) == syscall.ENOENT:
		return false, nil
	default:
		return false, fmt.Errorf("%s: %w", what, err)
	}
}

// genProps returns the identifier filter and the text preparation a new
// layout 3 generation gets (a fresh index, a rebuild, a conversion that
// follows the configuration): those of fts.dict_filter and
// fts.attached_headers while the cluster mark of the filter is set,
// read now; none without it. A configuration that asks for neither
// reads no mark.
func (u *User) genProps() (dx int, ah string, err error) {
	if !u.ftsCfg.dictFilter && u.ftsCfg.ah == "" {
		return 0, "", nil
	}

	if u.store == nil {
		return 0, "", nil
	}

	ok, err := u.store.FTSDictFilterEnabled()
	if err != nil || !ok {
		return 0, "", err
	}

	if u.ftsCfg.dictFilter {
		dx = u.ftsCfg.dictID
	}

	return dx, u.ftsCfg.ah, nil
}

// FTSLayoutIndex returns the presence index of the accounts with a
// layout 3 index ("ix.fts_layout" in "~accounts"); release closes it.
func (s *Store) FTSLayoutIndex() (model.SortedIndex, func(), error) {
	return s.openIndex(model.AccountsNamespace, model.FTSLayoutIndexName, model.FTSLayoutIndexShards)
}

// markLayout3 enters the account in the presence index (before its
// layout 3 objects exist); an existing entry is kept.
func (u *User) markLayout3() error {
	if u.store == nil {
		return nil
	}

	ix, release, err := u.store.FTSLayoutIndex()
	if err != nil {
		return fmt.Errorf("fts layout index: %w", err)
	}
	defer release()

	err = ix.Update(u.name, func(cur []byte) ([]byte, error) {
		if cur != nil {
			return cur, nil
		}

		b, _ := json.Marshal(model.FTSLayoutEntry{Since: time.Now().UTC()})

		return b, nil
	})
	if err != nil {
		return fmt.Errorf("fts layout index: %w", err)
	}

	return nil
}

// unmarkLayout3 removes the account from the presence index (after its
// layout 3 objects are gone).
func (u *User) unmarkLayout3() error {
	if u.store == nil {
		return nil
	}

	ix, release, err := u.store.FTSLayoutIndex()
	if err != nil {
		return fmt.Errorf("fts layout index: %w", err)
	}
	defer release()

	if err := ix.Remove(u.name); err != nil {
		return fmt.Errorf("fts layout index: %w", err)
	}

	return nil
}

// hasProps reports whether a generation has the identifier filter or
// the text preparation "nosig" (the properties a version before 0.50.0
// must not search).
func (l ftsLoc) hasProps() bool { return l.dx > 0 || l.ah != "" }

// markLayoutProps sets (on) or clears the presence entry's Props: set
// before a generation with properties comes into being, cleared after
// the account switched to one without (temp/13 §5.3; "fts dictfilter
// disable" reads the presence index only). An account without an entry
// gets one only when on. A creator that loses its race may leave Props
// set for a generation that never came; the scrub clears it after its
// grace period.
func (u *User) markLayoutProps(on bool) error {
	if u.store == nil {
		return nil
	}

	ix, release, err := u.store.FTSLayoutIndex()
	if err != nil {
		return fmt.Errorf("fts layout index: %w", err)
	}
	defer release()

	err = ix.Update(u.name, func(cur []byte) ([]byte, error) {
		if cur == nil && !on {
			return nil, nil
		}

		var e model.FTSLayoutEntry
		if cur != nil {
			_ = json.Unmarshal(cur, &e)
		}

		switch {
		case e.Since.IsZero():
			e.Since = time.Now().UTC()
		case on == !e.Props.IsZero():
			return cur, nil
		}

		e.Props = time.Time{}
		if on {
			e.Props = time.Now().UTC()
		}

		b, _ := json.Marshal(e)

		return b, nil
	})
	if err != nil {
		return fmt.Errorf("fts layout index: %w", err)
	}

	return nil
}

// layout3Wanted reports whether a new index of this account gets layout
// 3: fts.layout is 3 and the cluster mark is set (read now).
func (u *User) layout3Wanted() (bool, error) {
	if u.ftsCfg.layout != model.FTSLayout3 || u.store == nil {
		return false, nil
	}

	return u.store.readFTSMark()
}

// readFTSMark reads the cluster mark and records the answer
// (noteFTSLayout), both under ftsMarkMu: of two concurrent reads the
// later one is also recorded last.
func (s *Store) readFTSMark() (bool, error) {
	s.ftsMarkMu.Lock()
	defer s.ftsMarkMu.Unlock()

	marked, err := s.FTSLayout3Enabled()
	if err != nil {
		return false, err
	}

	s.noteFTSLayout(marked)

	return marked, nil
}

// States of Store.ftsMarkSeen.
const (
	ftsMarkUnknown int32 = iota
	ftsMarkOff
	ftsMarkOn
)

// noteFTSLayout records a read of the cluster mark: the gauges show the
// configured and the effective layout, and the first read and every
// change of the mark are logged once (configured layout, mark, effective
// layout), so that the layout new indexes get is visible without
// reading the admin store. The caller holds ftsMarkMu.
func (s *Store) noteFTSLayout(marked bool) {
	configured := ftsUserConfig(s.cfg.FTS).layout
	effective := model.EffectiveFTSLayout(configured, marked)

	metrics.FTSLayoutConfigured.Set(float64(configured))
	metrics.FTSLayoutEffective.Set(float64(effective))

	state := ftsMarkOff
	if marked {
		state = ftsMarkOn
	}

	prev := s.ftsMarkSeen
	s.ftsMarkSeen = state

	if prev == state || s.log == nil {
		return
	}

	msg := "full-text layout of new indexes"

	switch {
	case prev == ftsMarkUnknown:
	case marked:
		msg = "full-text layout of new indexes changed: the layout 3 mark was set"
	default:
		msg = "full-text layout of new indexes changed: the layout 3 mark was removed"
	}

	s.log.Info(msg, "event", "fts_layout", "configured", configured, "layout3_mark", marked, "effective", effective)
}

// LogFTSLayout reads the cluster mark once and logs the layout new
// indexes of this server get (called at startup). A server with
// fts.layout 3 (the built-in default) and no mark creates layout 2
// indexes; later index creations read the mark again and log a change.
func (s *Store) LogFTSLayout() error {
	configured := ftsUserConfig(s.cfg.FTS).layout

	metrics.FTSLayoutConfigured.Set(float64(configured))

	// With fts.layout 2 the effective layout does not depend on the mark.
	if configured != model.FTSLayout3 {
		metrics.FTSLayoutEffective.Set(float64(model.FTSLayout2))
	}

	_, err := s.readFTSMark()

	return err
}

// --- the reshard queue ----------------------------------------------------------

// ftsDueQueue opens the due queue of reshards once per process.
func (s *Store) ftsDueQueue() (*sortedindex.Queue, error) {
	s.ftsDueOnce.Do(func() { s.ftsDueQ, s.ftsDueErr = s.accountsQueue(model.FTSDueQueue) })

	return s.ftsDueQ, s.ftsDueErr
}

// FTSDueQueue returns the reshard queue ("ix.fts_due" in "~accounts")
// and a function that takes the worker lease of it.
func (s *Store) FTSDueQueue() (model.DueQueue, func(ttl time.Duration) (model.Lease, error), error) {
	q, err := s.ftsDueQueue()
	if err != nil {
		return nil, nil, err
	}

	lock := func(ttl time.Duration) (model.Lease, error) {
		return s.holdLock(model.AccountsNamespace, sortedindex.LockOID(model.FTSDueQueue), "worker", "fts reshard worker", ttl, true)
	}

	return q, lock, nil
}

// ftsDuePodTTL is how long a server remembers that it queued an account
// generation, so that it queues it once instead of on every write.
const ftsDuePodTTL = 10 * time.Minute

// ftsDueMemo is the per-server memory of queued generations.
type ftsDueMemo struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// once reports whether key may be queued now and remembers it.
func (m *ftsDueMemo) once(key string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.seen == nil {
		m.seen = map[string]time.Time{}
	}

	if t, ok := m.seen[key]; ok && now.Sub(t) < ftsDuePodTTL {
		return false
	}

	if len(m.seen) > 4096 {
		for k, t := range m.seen {
			if now.Sub(t) >= ftsDuePodTTL {
				delete(m.seen, k)
			}
		}
	}

	m.seen[key] = now

	return true
}

// queueReshard queues the account for a reshard of generation gen (at
// most once per server and ftsDuePodTTL). Errors are logged: the next
// write that sees the size asks again.
func (u *User) queueReshard(gen string) {
	if u.store == nil || !u.ftsCfg.autoReshard {
		return
	}

	now := time.Now()
	if !u.store.ftsDueSeen.once(u.name+"\x00"+gen, now) {
		return
	}

	q, err := u.store.ftsDueQueue()
	if err == nil {
		err = q.Move(u.name, nil, &model.DueKey{Due: now, Gen: model.FTSDueGen(gen)})
	}

	if err != nil {
		u.log.Warn("fts reshard not queued", "gen", gen, "err", err)
		return
	}

	metrics.FTSReshardDue.Inc()
}

// ftsCappedMax bounds the generations ftsCappedMemo remembers.
const ftsCappedMax = 65536

// ftsCappedMemo is the per-server memory of the index generations whose
// objects of a kind outgrew their target at the cap, so that the metric
// and the log line come once per generation and kind (until the entry
// is pushed out, oldest first, or the server restarts; then at most once
// more). It has no TTL: unlike ftsDueMemo it does not stand for an
// action to repeat.
type ftsCappedMemo struct {
	mu    sync.Mutex
	seen  map[string]bool
	order []string
}

// once reports whether key is new and remembers it.
func (m *ftsCappedMemo) once(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.seen[key] {
		return false
	}

	if m.seen == nil {
		m.seen = map[string]bool{}
	}

	if len(m.order) >= ftsCappedMax {
		delete(m.seen, m.order[0])
		m.order = m.order[1:]
	}

	m.seen[key] = true
	m.order = append(m.order, key)

	return true
}

// growth is what the counters of an index generation say: a reshard is
// due, or the objects of a kind (by its index in objectKinds) outgrew
// their target at the cap.
type growth struct {
	due    bool
	capped [len(objectKinds)]bool
}

// note records object kind k over its threshold: due below the cap,
// capped at it.
func (g *growth) note(below bool, k int) {
	if below {
		g.due = true
	} else {
		g.capped[k] = true
	}
}

// reshardDue reports what the counters of one posting object say: more
// than 1.5·T_p terms with fewer than fts.shards_max posting objects, or
// more than 1.5·T_d dictionary keys per dictionary object (!dn · S / D)
// with fewer than fts.dict_shards_max, ask for a reshard. At a cap the
// trigger is silent — no queue entry, no job, no lock — and the kind is
// reported as capped instead (temp/13 §4.6).
func (u *User) reshardDue(loc ftsLoc, terms, dict int64) growth {
	z := u.ftsCfg.sizing

	var g growth

	if float64(terms) > reshardFactor*float64(z.target) {
		g.note(loc.s < z.max, kindIndexPostings)
	}

	if float64(dict)*float64(loc.s)/float64(max(loc.d, 1)) > reshardFactor*float64(z.dictTarget) {
		g.note(loc.d < z.dictMax, kindIndexDictionary)
	}

	return g
}

// headersDue is reshardDue for the document count c over h header
// objects.
func (u *User) headersDue(h int, c int64) growth {
	z := u.ftsCfg.sizing

	var g growth

	if float64(c)/float64(max(h, 1)) > reshardFactor*float64(z.target) {
		g.note(h < z.max, kindIndexHeaders)
	}

	return g
}

// act queues the account for a reshard of generation gen when one is
// due and reports a kind at its cap once per generation and server.
func (u *User) act(gen string, g growth) {
	if g.due {
		u.queueReshard(gen)
	}

	for k, capped := range g.capped {
		if capped {
			u.reportCapped(gen, objectKinds[k])
		}
	}
}

// reportCapped counts and logs a generation whose objects of kind
// outgrew their target at the cap: once per generation, kind and server
// (ftsCappedMemo).
func (u *User) reportCapped(gen, kind string) {
	if u.store == nil || !u.store.ftsCappedSeen.once(u.name+"\x00"+gen+"\x00"+kind) {
		return
	}

	metrics.FTSShardsCapped.WithLabelValues(kind).Inc()

	z := u.ftsCfg.sizing

	key, limit := "fts.shards_max", z.max
	if kind == kindDictionary {
		key, limit = "fts.dict_shards_max", z.dictMax
	}

	u.log.Warn("fts objects outgrow their target at the shard cap; raise "+key+" and reshard with fts_layout",
		"gen", gen, "kind", kind, "cap", limit)
}

// defaultDictIDRunes is the default of fts.dict_id_min_runes.
const defaultDictIDRunes = 25

// ftsUserCfg are the layout settings of a view.
type ftsUserCfg struct {
	layout      int
	sizing      ftsSizing
	convert     convertCfg
	autoReshard bool
	frozenWait  time.Duration
	// dictFilter asks for the identifier filter of dictID runes and ah
	// for the text preparation ("" or fts.AttachedHeadersNoSig) that new
	// generations get while the cluster mark of the filter is set; ah
	// also decides which generations the searches trust (ftsTrusted).
	dictFilter bool
	dictID     int
	ah         string
	// build are the settings of the online runs (stage B14i).
	build buildCfg
}

// ftsUserConfig derives the view settings from the store configuration.
func ftsUserConfig(c FTSConfig) ftsUserCfg {
	layout := c.Layout
	if layout != model.FTSLayout3 {
		layout = model.FTSLayout2
	}

	ah := ""
	if c.AttachedHeaders == fts.AttachedHeadersNoSig {
		ah = fts.AttachedHeadersNoSig
	}

	// The configuration validates fts.dict_id_min_runes; a store
	// configured without it (tests, tools) gets the default 25.
	dictID := defaultDictIDRunes
	if c.DictIDMinRunes > 0 {
		dictID = min(max(c.DictIDMinRunes, fts.IdentMinRunesLow), fts.IdentMinRunesHigh)
	}

	return ftsUserCfg{layout: layout, autoReshard: c.AutoReshard, frozenWait: max(c.FrozenWait, 0),
		dictFilter: c.DictFilter, dictID: dictID, ah: ah,
		sizing: ftsSizing{min: c.ShardsMin, max: c.ShardsMax, target: c.ShardTargetKeys,
			dictMax: c.DictShardsMax, dictTarget: c.DictTargetKeys}.withDefaults(),
		convert: convertCfg{bufferKeys: c.ConvertBufferKeys, writers: c.ConvertWriters}.withDefaults(),
		build: buildCfg{catchupRounds: c.BuildCatchupRounds, hold: c.BuildHold, noHold: c.BuildNoHold,
			batchDocs: c.ReindexBatchDocs, batchBytes: c.ReindexBatchBytes}.withDefaults()}
}

// SetImportHint implements model.ImportHinter: a fresh index of this
// view is sized for h.Messages messages.
func (u *User) SetImportHint(h model.ImportHint) { u.ftsHint = max(h.Messages, 0) }
