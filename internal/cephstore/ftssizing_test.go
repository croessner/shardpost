//go:build tentacle

package cephstore

import (
	"fmt"
	"slices"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// TestSizingSeparateCaps (B14g): the dictionary has a cap and a target
// of its own; withDefaults fills both caps.
func TestSizingSeparateCaps(t *testing.T) {
	if z := (ftsSizing{}).withDefaults(); z != (ftsSizing{min: 1, max: 32, target: 100000, dictMax: 512, dictTarget: 100000}) {
		t.Errorf("defaults %+v", z)
	}

	if z := (ftsSizing{min: 600, max: 4, dictMax: 8}).withDefaults(); z != (ftsSizing{min: 600, max: 600, target: 100000, dictMax: 600, dictTarget: 100000}) {
		t.Errorf("caps below the lower bound %+v", z)
	}

	z := ftsSizing{min: 1, max: 128, target: 100000, dictMax: 512, dictTarget: 100000}.withDefaults()
	old := ftsSizing{min: 1, max: 32, target: 100000, dictMax: 32, dictTarget: 100000}

	for _, c := range []struct {
		name      string
		got, want countResult
	}{
		// The counts of the largest account on croessner (temp/13 §1.1).
		{"S of 2069828 terms", resultOf(z.count(2069828)), countResult{32, false}},
		{"D of 10138532 keys", resultOf(z.dictCount(10138532)), countResult{128, false}},
		{"D of 84 million keys", resultOf(z.dictCount(84_000_000)), countResult{512, true}},
		// The same counts under the single cap of 32 before B14g.
		{"D under the single cap", resultOf(old.dictCount(10138532)), countResult{32, true}},
	} {
		if c.got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, c.got, c.want)
		}
	}

	// A smaller dictionary target asks for more dictionary objects only.
	z.dictTarget = 50000

	if s, d, h := z.shapeFor(36246); [3]int{s, d, h} != [3]int{32, 256, 1} {
		t.Errorf("shape for 36246 messages with T_d 50000: %d/%d/%d", s, d, h)
	}
}

// countResult is a count and whether a cap cut it.
type countResult struct {
	n      int
	capped bool
}

func resultOf(n int, capped bool) countResult { return countResult{n, capped} }

// TestEstimateKeysCalibration (R2.4): the measured point is met, beyond
// it the slope of the last section continues (≈ 63.5 terms and ≈ 310.9
// dictionary keys per message), and the knee at 5000 bridges to it.
func TestEstimateKeysCalibration(t *testing.T) {
	if terms, dict := estimateKeys(36246); terms != 2069828 || dict != 10138532 {
		t.Errorf("at the measured point: %d %d", terms, dict)
	}

	if terms, dict := estimateKeys(5000); terms != 86693 || dict != 424610 {
		t.Errorf("at the knee: %d %d", terms, dict)
	}

	t0, d0 := estimateKeys(36246)
	t1, d1 := estimateKeys(46246)

	if per := float64(t1-t0) / 10000; per < 63.4 || per > 63.6 {
		t.Errorf("terms per message beyond the last point %.2f", per)
	}

	if per := float64(d1-d0) / 10000; per < 310.8 || per > 311.0 {
		t.Errorf("dictionary keys per message beyond the last point %.2f", per)
	}

	if _, d := estimateKeys(10000); d < 1_970_000 || d > 1_990_000 {
		t.Errorf("10000 messages: %d dictionary keys, want about 1.98 million", d)
	}
}

// TestReshardTriggerAtCap (§4.6): the trigger asks for a reshard below
// a cap and is silent at it, per kind; the kind at its cap is reported.
func TestReshardTriggerAtCap(t *testing.T) {
	u := &User{ftsCfg: ftsUserCfg{sizing: ftsSizing{min: 1, max: 32, target: 100000, dictMax: 128, dictTarget: 100000}}}

	for _, c := range []struct {
		name        string
		s, d        int
		terms, dict int64
		want        growth
	}{
		{"below both", 32, 32, 10, 10, growth{}},
		{"postings below the cap", 16, 32, 160000, 0, growth{due: true}},
		{"postings at the cap", 32, 32, 160000, 0, growth{capped: cappedOf(kindIndexPostings)}},
		{"dictionary below its cap", 32, 32, 0, 160000, growth{due: true}},
		{"dictionary at its cap", 32, 128, 0, 160000 * 4, growth{capped: cappedOf(kindIndexDictionary)}},
		{"postings capped, dictionary due", 32, 64, 160000, 160000 * 2, growth{due: true, capped: cappedOf(kindIndexPostings)}},
		{"above both caps (forced)", 64, 256, 160000, 160000 * 4, growth{capped: [len(objectKinds)]bool{true, true, false}}},
	} {
		if got := u.reshardDue(ftsLoc{f: 3, g: "g", s: c.s, d: c.d, h: 1}, c.terms, c.dict); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}

	if g := u.headersDue(1, 160000); g != (growth{due: true}) {
		t.Errorf("headers below the cap: %+v", g)
	}

	if g := u.headersDue(32, 32*160000); g != (growth{capped: cappedOf(kindIndexHeaders)}) {
		t.Errorf("headers at the cap: %+v", g)
	}
}

// cappedOf is the capped field with kind k set.
func cappedOf(k int) [len(objectKinds)]bool {
	var c [len(objectKinds)]bool

	c[k] = true

	return c
}

// TestCappedMemo (R2.6): a generation is reported once, without a TTL;
// past ftsCappedMax entries the oldest goes first.
func TestCappedMemo(t *testing.T) {
	var m ftsCappedMemo

	if !m.once("a") || m.once("a") {
		t.Fatal("not once")
	}

	for i := range ftsCappedMax - 1 {
		m.once(fmt.Sprint(i))
	}

	if m.once("a") {
		t.Fatal("pushed out before the memo is full")
	}

	m.once("one more")

	if !m.once("a") {
		t.Error("the oldest entry was not pushed out")
	}

	if len(m.order) != ftsCappedMax || len(m.seen) != ftsCappedMax {
		t.Errorf("memo size %d %d", len(m.order), len(m.seen))
	}
}

// cappedCount reads imapceph_fts_shards_capped_total of a kind.
func cappedCount(t *testing.T, kind string) float64 {
	t.Helper()

	var m dto.Metric
	if err := metrics.FTSShardsCapped.WithLabelValues(kind).Write(&m); err != nil {
		t.Fatal(err)
	}

	return m.GetCounter().GetValue()
}

// dueEntries are the reshard queue's entries of an account.
func dueEntries(t *testing.T, s *Store, account string) []model.DueEntry {
	t.Helper()

	q, err := s.ftsDueQueue()
	if err != nil {
		t.Fatal(err)
	}

	all, err := q.Due(time.Now().Add(time.Hour), 10000)
	if err != nil {
		t.Fatal(err)
	}

	var out []model.DueEntry

	for _, e := range all {
		if e.ID == account {
			out = append(out, e)
		}
	}

	return out
}

// TestLayoutTriggerSilentAtCap (§4.6, §10): with every kind at its cap
// the writers queue nothing and report each kind once per generation;
// FTSLayoutDue (the path of plan -due-only) targets the shape the index
// has and names the capped kinds.
func TestLayoutTriggerSilentAtCap(t *testing.T) {
	s := layout3Store(t, func(c *FTSConfig) {
		c.ShardsMax, c.DictShardsMax, c.ShardTargetKeys, c.DictTargetKeys, c.AutoReshard = 1, 1, 10, 10, true
	})
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	before := map[string]float64{}
	for _, k := range []string{kindPostings, kindDictionary, kindHeaders} {
		before[k] = cappedCount(t, k)
	}

	deliverDocs(t, u, layoutDocs(20))

	if e := dueEntries(t, s, u.name); len(e) != 0 {
		t.Errorf("queued at the cap: %+v", e)
	}

	for _, k := range []string{kindPostings, kindDictionary, kindHeaders} {
		if d := cappedCount(t, k) - before[k]; d != 1 {
			t.Errorf("%s reported %v times, want once per generation", k, d)
		}
	}

	target, due, err := u.FTSLayoutDue()
	if err != nil || due || target.S != 1 || target.D != 1 || target.H != 1 {
		t.Fatalf("due at the cap: %+v %v %v", target, due, err)
	}

	if !slices.Equal(target.Capped, []string{kindPostings, kindDictionary, kindHeaders}) {
		t.Errorf("capped kinds %v", target.Capped)
	}

	if rep := convert(t, u, model.FTSLayoutRequest{DueOnly: true}); !rep.Noop {
		t.Errorf("due_only at the cap: %+v", rep)
	}
}

// TestLayoutSeparateCaps (§4.6): postings at their cap and a dictionary
// below its own: the writers queue the account for the dictionary, and
// the target grows D only.
func TestLayoutSeparateCaps(t *testing.T) {
	s := layout3Store(t, func(c *FTSConfig) {
		c.ShardsMax, c.DictShardsMax, c.ShardTargetKeys, c.DictTargetKeys, c.AutoReshard = 1, 1024, 10, 100, true
	})
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	pBefore, dBefore := cappedCount(t, kindPostings), cappedCount(t, kindDictionary)

	deliverDocs(t, u, layoutDocs(20))

	if e := dueEntries(t, s, u.name); len(e) != 1 {
		t.Errorf("entries %+v, want one for the dictionary", e)
	}

	if got := [2]float64{cappedCount(t, kindPostings) - pBefore, cappedCount(t, kindDictionary) - dBefore}; got != [2]float64{1, 0} {
		t.Errorf("capped counts %v: postings once, dictionary never", got)
	}

	target, due, err := u.FTSLayoutDue()
	if err != nil || !due || [2]int{target.S, target.H} != [2]int{1, 1} || target.D < 2 {
		t.Fatalf("target %+v %v %v", target, due, err)
	}

	if !slices.Equal(target.Capped, []string{kindPostings, kindHeaders}) {
		t.Errorf("capped kinds %v", target.Capped)
	}

	rep := convert(t, u, model.FTSLayoutRequest{DueOnly: true})
	if rep.Noop || rep.To.S != 1 || rep.To.D != target.D {
		t.Errorf("reshard %+v, want D %d", rep, target.D)
	}
}

// TestLayoutForcedShape (§4.6, R2.8): shape forces each count, shards
// all three alike; both ignore the caps; the searches stay the same.
func TestLayoutForcedShape(t *testing.T) {
	s := layout3Store(t, func(c *FTSConfig) { c.ShardsMax, c.DictShardsMax = 1, 1 })
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(30))

	want := searchState(t, u)

	rep := convert(t, u, model.FTSLayoutRequest{Layout: 3, Shape: &model.FTSForcedShape{S: 2, D: 8, H: 3}})
	if rep.To.S != 2 || rep.To.D != 8 || rep.To.H != 3 {
		t.Fatalf("shape 2/8/3 above the caps: %+v", rep)
	}

	mustExist(t, u, true, curLoc(layoutOf(t, u), u.ftsShards).objects()...)
	sameSearches(t, "shape 2/8/3", want, searchState(t, u))

	reshardTo(t, u, 4, want)
}

// TestLayoutOlderVersionShrinks (temp/13 §5.4 R2.2, §10): a version
// before 0.49.0 bounds D by fts.shards_max; against a generation with
// more dictionary objects its due_only target shrinks D. With
// fts.shards_max at least the largest D (measure b) it does not.
func TestLayoutOlderVersionShrinks(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(30))

	// A small dictionary target asks for many dictionary objects: this
	// version reshards to them.
	u.ftsCfg.sizing.dictTarget = 10

	want, _, err := u.FTSLayoutDue()
	if err != nil || want.D < 8 || want.S != 1 || want.H != 1 {
		t.Fatalf("target %+v %v", want, err)
	}

	convert(t, u, model.FTSLayoutRequest{DueOnly: true})

	if _, due, err := u.FTSLayoutDue(); err != nil || due {
		t.Fatalf("this version after its reshard: due %v %v", due, err)
	}

	// The older version: one cap for all kinds, fts.shards_max 4.
	u.ftsCfg.sizing.dictMax, u.ftsCfg.sizing.max = 4, 4

	target, due, err := u.FTSLayoutDue()
	if err != nil || !due || target.D != 4 {
		t.Fatalf("an older version with fts.shards_max 4 would not shrink: %+v %v %v", target, due, err)
	}

	// Measure b: fts.shards_max at least the largest D.
	u.ftsCfg.sizing.dictMax, u.ftsCfg.sizing.max = want.D, want.D

	if target, due, err := u.FTSLayoutDue(); err != nil || due {
		t.Errorf("fts.shards_max 16: %+v due %v %v", target, due, err)
	}
}
