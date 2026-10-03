//go:build tentacle

package cephstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// Tests of the online runs (stage B14i, temp/14 §8).

// --- without the cluster ------------------------------------------------------

// TestBuildKeys: the record of a GUID carries the run's token and the
// id; "w#" counts the writes of the run and starts again for another
// token; a GUID recorded with the same id is not written again, one with
// a new id is (R2.4).
func TestBuildKeys(t *testing.T) {
	if tok, n, ok := parseBuildValue(buildValue("job-x/3", 42)); !ok || tok != "job-x/3" || n != 42 {
		t.Errorf("round trip %q %d %v", tok, n, ok)
	}

	for _, v := range []string{"", "x", ":4", "a:b"} {
		if _, _, ok := parseBuildValue([]byte(v)); ok {
			t.Errorf("parsed %q", v)
		}
	}

	ids := map[string]uint64{"g1": 1, "g2": 2, "g3": 7}
	idOf := func(g string) (uint64, bool) { id, ok := ids[g]; return id, ok }
	cur := map[string][]byte{buildKey("g1"): buildValue("A", 1), buildKey("g3"): buildValue("A", 3), buildCountKey: buildValue("A", 5)}

	for _, c := range []struct {
		name, tok string
		guids     []string
		want      map[string]string
	}{
		{"same run", "A", []string{"g1", "g2"}, map[string]string{buildKey("g2"): "A:2", buildCountKey: "A:6"}},
		{"another run", "B", []string{"g1", "g2"}, map[string]string{buildKey("g1"): "B:1", buildKey("g2"): "B:2", buildCountKey: "B:2"}},
		{"new id", "A", []string{"g3"}, map[string]string{buildKey("g3"): "A:7", buildCountKey: "A:6"}},
		{"recorded", "A", []string{"g1"}, nil},
	} {
		set, n := captureKeys(cur, c.tok, c.guids, idOf)

		got := map[string]string{}
		for k, v := range set {
			got[k] = string(v)
		}

		if !maps.Equal(got, c.want) || n != max(len(c.want)-1, 0) {
			t.Errorf("%s: %v %d, want %v", c.name, got, n, c.want)
		}
	}
}

// ftsGenBefore051 is the "next" of a layout record as a version before
// 0.51.0 decodes it.
type ftsGenBefore051 struct {
	F   int    `json:"f"`
	G   string `json:"g,omitempty"`
	S   int    `json:"s,omitempty"`
	D   int    `json:"d,omitempty"`
	H   int    `json:"h,omitempty"`
	Tok string `json:"tok,omitempty"`
	At  int64  `json:"at,omitempty"`
}

// TestLayoutRecordOnline: "on", "m" and "hold" are in the record; a
// version before 0.51.0 that rewrites it drops them, and the record is
// then no online run any more (O7 notices that, §5.4).
func TestLayoutRecordOnline(t *testing.T) {
	l := &ftsLayout{F: 3, G: "g", S: 1, D: 1, H: 1, Dict: true,
		Next: &ftsGen{F: 3, G: "n", S: 2, D: 2, H: 2, Tok: "job-a/1", On: true, M: model.FTSModeReindex, Hold: 123}}

	got, err := parseLayout(l.encode())
	if err != nil || onlineNext(got) == nil || got.Next.M != model.FTSModeReindex || got.Next.Hold != 123 {
		t.Fatalf("record %+v %v", got, err)
	}

	var old struct {
		F    int              `json:"f"`
		G    string           `json:"g"`
		S    int              `json:"s"`
		D    int              `json:"d"`
		Dict bool             `json:"dict"`
		Next *ftsGenBefore051 `json:"next,omitempty"`
	}

	if err := json.Unmarshal(l.encode(), &old); err != nil {
		t.Fatal(err)
	}

	b, _ := json.Marshal(old)

	if got, err = parseLayout(b); err != nil || got.Next == nil || onlineNext(got) != nil || got.Next.Tok != "job-a/1" {
		t.Errorf("rewritten by an older version: %+v %v", got, err)
	}
}

// TestHolding: a hold is one until its time.
func TestHolding(t *testing.T) {
	now := time.Now()

	if !holding(&ftsGen{Hold: now.Add(time.Second).UnixMilli()}, now) || holding(&ftsGen{Hold: now.Add(-time.Second).UnixMilli()}, now) ||
		holding(&ftsGen{}, now) {
		t.Error("holding")
	}
}

// TestSoftFreezeBoundToRun (R2.1): an operation writes through the
// freezes of the run it is recorded for and the ones it met, nothing
// else: the freeze of a following run is an anomaly again.
func TestSoftFreezeBoundToRun(t *testing.T) {
	loc := ftsLoc{f: 3, g: "g", s: 1, d: 1, h: 1}
	w := &ftsWrite{}
	w.follow(&ftsLayout{F: 3, G: "g", S: 1, D: 1, H: 1}, 1)

	marks := func(state string) map[string][]byte {
		return map[string][]byte{markGen: []byte("g"), markState: []byte(state)}
	}

	w.softFor("A", frozenPrefix+"late")

	for _, c := range []struct {
		state string
		pass  bool
	}{{frozenPrefix + "A", true}, {frozenPrefix + "late", true}, {frozenPrefix + "B", false}, {stateLive, true}} {
		_, err := w.check(loc.postingOIDn(0), 1, marks(c.state))
		if (err == nil) != c.pass {
			t.Errorf("%s: %v", c.state, err)
		}
	}

	// another run starts the state afresh
	w.softFor("B", frozenPrefix+"B")

	if _, err := w.check(loc.postingOIDn(0), 1, marks(frozenPrefix+"A")); asAnomaly(err) == nil {
		t.Error("the freeze of run A passes after run B")
	}

	// a change of generation empties it
	w.follow(&ftsLayout{F: 3, G: "g2", S: 1, D: 1, H: 1}, 1)

	if len(w.soft) != 0 {
		t.Error("soft state kept across generations")
	}
}

// TestSwitchOnline (Befund 3): a rebuild sets the marker of its target
// and removes the rebuild key whatever the source had; a copy keeps the
// rule of switchTo3 (an untrusted source gives an untrusted target). The
// document count is not touched.
func TestSwitchOnline(t *testing.T) {
	src := ftsLoc{f: 3, g: "s", s: 1, d: 1, h: 1}
	target := ftsLoc{f: 3, g: "t", s: 2, d: 2, h: 2, ah: fts.AttachedHeadersNoSig}
	c := &ftsLayout{F: 3, G: "s", S: 1, D: 1, H: 1, Dict: true, LG: true}

	set, rm := switchOnline(map[string][]byte{ftsRebuildKey: []byte("x")}, c, src, target, "job-a/1", model.FTSModeReindex)
	if string(set[ftsVersionKey3]) != ftsVersionText || !slices.Contains(rm, ftsRebuildKey) || set[ftsCountKey] != nil {
		t.Errorf("reindex: %v %v", set, rm)
	}

	set, rm = switchOnline(map[string][]byte{}, c, src, target, "job-a/1", model.FTSModeCopy)
	if set[ftsVersionKey3] != nil || !slices.Contains(rm, ftsVersionKey3) {
		t.Errorf("copy of an untrusted source: %v %v", set, rm)
	}

	set, _ = switchOnline(map[string][]byte{ftsVersionKey3: []byte(ftsVersion)}, c, src, target, "job-a/1", model.FTSModeCopy)

	l, _ := parseLayout(set[ftsLayoutKey])
	if string(set[ftsVersionKey3]) != ftsVersionText || l.G != "t" || l.Next != nil || len(l.Old) != 1 || l.Old[0].Tok != "job-a/1" || !l.LG {
		t.Errorf("copy: %v %+v", set, l)
	}
}

// --- with the cluster -----------------------------------------------------------

// onlineStore is a layout 3 store with the cluster mark of online runs.
func onlineStore(t *testing.T, fn func(*FTSConfig)) *Store {
	t.Helper()

	s := layout3Store(t, fn)
	setMark(t, s, model.FTSOnlineMarkKey, true)
	t.Cleanup(func() { setMark(t, s, model.FTSOnlineMarkKey, false) })

	return s
}

// onlineDocs are the documents of the online tests: the layout
// documents and a few more (GUIDs from 100 on, beyond the header texts
// searchState compares) that the tests deliver or expunge during a run.
func onlineDocs() (base, extra map[string]string) {
	all := layoutDocs(110)
	base, extra = map[string]string{}, map[string]string{}

	for k, v := range all {
		var i int

		_, _ = fmt.Sscanf(k, "g%d", &i)

		switch {
		case i < 30 || (i >= 100 && i < 103):
			base[k] = v
		case i >= 103:
			extra[k] = v
		}
	}

	return base, extra
}

// deliverDoc delivers one document of docs into INBOX.
func deliverDoc(t *testing.T, u *User, docs map[string]string, key string) error {
	t.Helper()

	var i int

	_, _ = fmt.Sscanf(key, "g%d", &i)

	_, err := u.DeliverGUID("INBOX", docGUID(i), []byte(docs[key]), nil, time.Time{})

	return err
}

// expungeGUID expunges the message of the GUID from INBOX.
func expungeGUID(t *testing.T, u *User, guid string) {
	t.Helper()

	metas, err := u.ListMessagesByName("INBOX")
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range metas {
		if m.GUID == guid {
			if err := u.Expunge("INBOX", []Meta{m}); err != nil {
				t.Fatal(err)
			}

			return
		}
	}

	t.Fatalf("no message %s", guid)
}

// hookPhases calls the functions of the phases of the account's runs
// (ftsLayoutHook) until the test ends; an error is a crash.
func hookPhases(t *testing.T, account string, fns map[string]func() error) {
	t.Helper()

	ftsLayoutHook = func(a, p string) error {
		if a != account || fns[p] == nil {
			return nil
		}

		return fns[p]()
	}

	t.Cleanup(func() { ftsLayoutHook = nil })
}

// noBuildKeys fails when the id map holds records of online runs.
func noBuildKeys(t *testing.T, u *User) {
	t.Helper()

	if keys := omapKeys(t, u, oidFTSMap, "w"); len(keys) != 0 {
		t.Errorf("records of online runs left: %v", slices.Sorted(maps.Keys(keys)))
	}
}

// cleanRecord fails when the record has a run or sources left.
func cleanRecord(t *testing.T, u *User) *ftsLayout {
	t.Helper()

	l := layoutOf(t, u)
	if l == nil || l.Next != nil || len(l.Old) != 0 {
		t.Errorf("record %+v", l)
	}

	return l
}

// deliverAll delivers the documents of docs under keys.
func deliverAll(t *testing.T, u *User, docs map[string]string, keys []string) error {
	t.Helper()

	for _, k := range keys {
		if err := deliverDoc(t, u, docs, k); err != nil {
			return err
		}
	}

	return nil
}

// sameAfterRun checks what a run leaves: a clean record, no records of
// the run, the searches of the reference account.
func sameAfterRun(t *testing.T, stage string, ref, u *User) *ftsLayout {
	t.Helper()

	l := cleanRecord(t, u)
	noBuildKeys(t, u)
	sameSearches(t, stage, searchState(t, ref), searchState(t, u))

	return l
}

// TestOnlineCopySameAsOffline (§8 result equality): an online reshard
// with deliveries and an expunge in its phases answers every search as
// an offline reshard of an account that got the same changes before it;
// the deliveries never fail, the run leaves no records, and the count is
// the live documents.
func TestOnlineCopySameAsOffline(t *testing.T) {
	s := onlineStore(t, nil)
	base, extra := onlineDocs()
	us := filledUsers(t, base, s, s)
	ref, u := us[0], us[1]
	keys := slices.Sorted(maps.Keys(extra))

	if err := deliverAll(t, ref, extra, keys); err != nil {
		t.Fatal(err)
	}

	expungeGUID(t, ref, docGUID(100))

	if _, err := ref.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 4, Tok: "job-ref/1"}); err != nil {
		t.Fatal(err)
	}

	rounds := 0

	hookPhases(t, u.name, map[string]func() error{
		phaseO2: func() error { return deliverAll(t, u, extra, keys[:1]) },
		phaseO3: func() error { return deliverAll(t, u, extra, keys[1:2]) },
		phaseO5: func() error {
			expungeGUID(t, u, docGUID(100))
			return deliverAll(t, u, extra, keys[2:3])
		},
		phaseO6: func() error {
			if rounds++; rounds > 1 {
				return nil
			}

			return deliverAll(t, u, extra, keys[3:])
		},
	})

	rep, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Layout: 3, Shards: 4, Tok: "job-online/1"})
	if err != nil {
		t.Fatal(err)
	}

	ftsLayoutHook = nil

	if rep.Mode != model.FTSModeCopy || rep.CaughtUp == 0 || rep.Deferred != "" || rep.To.S != 4 {
		t.Errorf("report %+v", rep)
	}

	if l := sameAfterRun(t, "online copy", ref, u); l.S != 4 {
		t.Errorf("record %+v", l)
	}

	if c := omapKeys(t, u, oidFTSMap, ftsCountKey)[ftsCountKey]; string(c) != string(omapKeys(t, ref, oidFTSMap, ftsCountKey)[ftsCountKey]) {
		t.Errorf("document count %q", c)
	}
}

// TestOnlineRebuildSameAsOffline (§8, Befund 3): an online rebuild with
// the text preparation nosig answers every search as an offline rebuild
// with it; it heals an index that an offline rebuild left without its
// marker and with the rebuild key: the marker of the target is set, the
// key gone, the searches trust it.
func TestOnlineRebuildSameAsOffline(t *testing.T) {
	s := dictFilterStore(t, true, func(c *FTSConfig) { c.AttachedHeaders = fts.AttachedHeadersNoSig })
	setMark(t, s, model.FTSOnlineMarkKey, true)
	t.Cleanup(func() { setMark(t, s, model.FTSOnlineMarkKey, false) })

	base, extra := onlineDocs()
	us := filledUsers(t, base, s, s)
	ref, u := us[0], us[1]
	keys := slices.Sorted(maps.Keys(extra))

	if err := deliverAll(t, ref, extra, keys); err != nil {
		t.Fatal(err)
	}

	if err := ref.RebuildFTS(func() error { return nil }, func(int, int) {}); err != nil {
		t.Fatal(err)
	}

	// an offline rebuild that stopped: the rebuild key, no marker
	if err := casOmap(u, u.idx, oidFTSMap, nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{ftsRebuildKey: []byte("stopped")}, []string{ftsVersionKey3}
	}); err != nil {
		t.Fatal(err)
	}

	hookPhases(t, u.name, map[string]func() error{
		phaseO3: func() error { return deliverAll(t, u, extra, keys[:1]) },
		phaseO5: func() error { return deliverAll(t, u, extra, keys[1:]) },
	})

	rep, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Rebuild: true, Tok: "job-rebuild/1", DropOrphanIDs: true})
	if err != nil {
		t.Fatal(err)
	}

	ftsLayoutHook = nil

	if rep.Mode != model.FTSModeReindex || rep.To.AttachedHeaders != fts.AttachedHeadersNoSig || rep.Indexed == 0 {
		t.Errorf("report %+v", rep)
	}

	if l := sameAfterRun(t, "online rebuild", ref, u); l.AH != fts.AttachedHeadersNoSig {
		t.Errorf("record %+v", l)
	}

	if vals := omapKeys(t, u, oidFTSMap, ""); string(vals[ftsVersionKey3]) != ftsVersionText || vals[ftsRebuildKey] != nil {
		t.Errorf("marker %q, rebuild key %q", vals[ftsVersionKey3], vals[ftsRebuildKey])
	}
}

// TestOnlineCrash (§8 crash hooks): a run that stops after any of its
// phases leaves the account usable — deliveries succeed — and the run of
// the job that takes over (another token) clears it and switches; no
// record of the crashed run stays.
func TestOnlineCrash(t *testing.T) {
	base, extra := onlineDocs()
	keys := slices.Sorted(maps.Keys(extra))

	for _, phase := range []string{phaseO2, phaseO3, phaseO5, phaseO6, phaseO7, phaseO8} {
		t.Run(phase, func(t *testing.T) {
			s := onlineStore(t, nil)
			us := filledUsers(t, base, s, s)
			ref, u := us[0], us[1]

			if err := deliverAll(t, ref, extra, keys); err != nil {
				t.Fatal(err)
			}

			crashAt(t, u.name, phase)

			if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 4, Tok: "job-crash/1"}); err == nil {
				t.Fatal("no crash")
			}

			ftsLayoutHook = nil

			// the account works with what the crash left
			if err := deliverAll(t, u, extra, keys); err != nil {
				t.Fatalf("delivery after the crash: %v", err)
			}

			rep, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 2, Tok: "job-crash/2",
				ForeignAlive: func(string, time.Time) (bool, error) { return false, nil }})
			if err != nil || rep.Deferred != "" {
				t.Fatalf("takeover: %+v %v", rep, err)
			}

			sameAfterRun(t, "after "+phase, ref, u)
		})
	}
}

// TestOnlineRegisterCrossing (§8 barrier): a writer that allocated its
// ids before the run's O2 and writes its postings after the copy read
// the source meets the soft freeze, records itself and writes through;
// its document is in the target after the switch. The delivery during
// the run costs at most one anomaly round (Befund 1).
func TestOnlineRegisterCrossing(t *testing.T) {
	s := onlineStore(t, nil)
	base, extra := onlineDocs()
	u := filledUsers(t, base, s)[0]

	key := slices.Sorted(maps.Keys(extra))[0]

	var i int

	_, _ = fmt.Sscanf(key, "g%d", &i)
	guid := docGUID(i)
	raw := []byte(extra[key])

	if _, err := u.writeBlob(guid, raw); err != nil {
		t.Fatal(err)
	}

	a, err := u.allocDocIDs([]string{guid})
	if err != nil || a.built != "" {
		t.Fatalf("alloc before the run: %+v %v", a, err)
	}

	var anomalies []int

	ftsAnomalyHook = func(acct string, an *ftsAnomaly) {
		if acct == u.name {
			anomalies = append(anomalies, an.kind)
		}
	}

	t.Cleanup(func() { ftsAnomalyHook = nil })

	var w *ftsWrite

	hookPhases(t, u.name, map[string]func() error{
		phaseO5: func() error {
			w = &ftsWrite{wait: u.ftsCfg.frozenWait, built: a.built, ids: a.out}
			w.follow(a.lay, u.ftsShards)

			return u.indexWith(w, &ftsBatch{docs: []ftsDoc{{guid: guid, raw: raw}}, ids: a.out})
		},
	})

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 4, Tok: "job-cross/1"}); err != nil {
		t.Fatal(err)
	}

	if w == nil || w.built != "job-cross/1" || len(anomalies) != 1 || anomalies[0] != anomFrozen {
		t.Errorf("writer built %q, anomalies %v", w.built, anomalies)
	}

	hits, err := u.SearchFields([]string{fts.FieldBody}, fmt.Sprintf("unique%dword", i))
	if err != nil || !hits[guid] {
		t.Errorf("the crossing document is not in the target: %v %v", hits, err)
	}
}

// TestOnlineBudget (Befund 1): a delivery whose terms reach every
// posting object of a frozen source, during the run, costs one anomaly
// round, never a failure.
func TestOnlineBudget(t *testing.T) {
	s := onlineStore(t, nil)
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Layout: 3, Shards: 8, Tok: "job-wide/1"}); err != nil {
		t.Fatal(err)
	}

	var words []string
	for j := range 400 {
		words = append(words, fmt.Sprintf("wideword%dx", j))
	}

	raw := "Subject: wide\r\n\r\n" + strings.Join(words, " ") + "\r\n"

	var (
		mu        sync.Mutex
		anomalies int
	)

	ftsAnomalyHook = func(acct string, _ *ftsAnomaly) {
		if acct == u.name {
			mu.Lock()
			anomalies++
			mu.Unlock()
		}
	}

	t.Cleanup(func() { ftsAnomalyHook = nil })

	var derr error

	hookPhases(t, u.name, map[string]func() error{
		phaseO3: func() error {
			_, derr = u.DeliverGUID("INBOX", docGUID(200), []byte(raw), nil, time.Time{})
			return nil
		},
	})

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 4, Tok: "job-wide/2"}); err != nil {
		t.Fatal(err)
	}

	if derr != nil || anomalies != 1 {
		t.Errorf("delivery %v after %d anomaly rounds", derr, anomalies)
	}
}

// TestOnlineFixedWriteLost (Befund 2): when the targets go during the
// rebuild (another run cleared it), the run stops with
// errConversionLost and writes nothing into the current generation.
func TestOnlineFixedWriteLost(t *testing.T) {
	s := onlineStore(t, nil)
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]

	src := curLoc(layoutOf(t, u), u.ftsShards)
	before := map[string]int{}

	for _, oid := range src.objects() {
		before[oid] = len(omapKeys(t, u, oid, ""))
	}

	hookPhases(t, u.name, map[string]func() error{
		phaseO3: func() error {
			l := layoutOf(t, u)
			return u.deleteObjects(genLoc(*l.Next, u.ftsShards).objects())
		},
	})

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Rebuild: true, Tok: "job-lost/1"}); !errors.Is(err, errConversionLost) {
		t.Fatalf("run: %v", err)
	}

	for _, oid := range src.objects() {
		if n := len(omapKeys(t, u, oid, "")); n != before[oid] {
			t.Errorf("%s: %d keys, %d before", oid, n, before[oid])
		}
	}

	if !curLoc(layoutOf(t, u), u.ftsShards).same(src) {
		t.Error("switched to a lost target")
	}
}

// TestOnlineOrphanAndReuse (R2.3, R2.4): an id without blob and mailbox
// entry is skipped, counted and dropped; a GUID expunged and delivered
// again during the rebuild is recorded with its new id and found once.
func TestOnlineOrphanAndReuse(t *testing.T) {
	s := onlineStore(t, nil)
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]

	orphan := newGUID()
	if _, _, err := u.ensureDocIDs([]string{orphan}); err != nil {
		t.Fatal(err)
	}

	guid := docGUID(101)

	hookPhases(t, u.name, map[string]func() error{
		phaseO5: func() error {
			expungeGUID(t, u, guid)
			return deliverDoc(t, u, base, "g101")
		},
	})

	rep, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Rebuild: true, Tok: "job-orphan/1", DropOrphanIDs: true})
	if err != nil {
		t.Fatal(err)
	}

	ftsLayoutHook = nil

	if rep.OrphanIDs != 1 {
		t.Errorf("report %+v", rep)
	}

	if _, ok, _, err := u.docIDOf(orphan); ok || err != nil {
		t.Errorf("orphan id kept: %v", err)
	}

	cleanRecord(t, u)

	hits, err := u.SearchFields([]string{fts.FieldBody}, "unique101word")
	if err != nil || len(hits) != 1 || !hits[guid] {
		t.Errorf("redelivered GUID: %v %v", hits, err)
	}
}

// TestOnlineHold (§4.8): a run that keeps finding new deliveries holds
// new id allocations once its catch-up rounds are used up, catches up
// and switches; the deliveries succeed.
func TestOnlineHold(t *testing.T) {
	s := onlineStore(t, func(c *FTSConfig) { c.BuildCatchupRounds, c.BuildHold = 2, 500*time.Millisecond })
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]

	n := 300

	hookPhases(t, u.name, map[string]func() error{
		phaseO6: func() error {
			l := layoutOf(t, u)
			if l.Next != nil && l.Next.Hold > 0 {
				return nil // held: no delivery would get through
			}

			n++

			_, err := u.DeliverGUID("INBOX", docGUID(n), []byte(fmt.Sprintf("Subject: flood %d\r\n\r\nflood%dword\r\n", n, n)), nil, time.Time{})

			return err
		},
	})

	rep, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 2, Tok: "job-hold/1"})
	if err != nil {
		t.Fatal(err)
	}

	ftsLayoutHook = nil

	if rep.HoldMS == 0 {
		t.Errorf("no hold: %+v", rep)
	}

	cleanRecord(t, u)

	hits, err := u.SearchFields([]string{fts.FieldBody}, fmt.Sprintf("flood%dword", n))
	if err != nil || len(hits) != 1 {
		t.Errorf("last delivery: %v %v", hits, err)
	}
}

// TestOnlineHoldAllocation (§4.3, Befund 11): an allocation that meets a
// hold waits for it once outside the compare-and-swap and then writes
// its ids (a round without a write is not done).
func TestOnlineHoldAllocation(t *testing.T) {
	s := onlineStore(t, nil)
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]

	crashAt(t, u.name, phaseO2)

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 2, Tok: "job-held/1"}); err == nil {
		t.Fatal("no crash")
	}

	ftsLayoutHook = nil

	until := time.Now().Add(400 * time.Millisecond)

	if err := casOmap(u, u.idx, oidFTSMap, []string{ftsLayoutKey}, func(cur map[string][]byte) (map[string][]byte, []string) {
		l, _ := parseLayout(cur[ftsLayoutKey])
		l.Next.Hold = until.UnixMilli()

		return map[string][]byte{ftsLayoutKey: l.encode()}, nil
	}); err != nil {
		t.Fatal(err)
	}

	guid := newGUID()

	start := time.Now()

	a, err := u.allocDocIDs([]string{guid})
	if err != nil {
		t.Fatal(err)
	}

	if time.Since(start) < 300*time.Millisecond || a.built != "job-held/1" || !a.held {
		t.Errorf("allocation after %s, built %q, held %v", time.Since(start), a.built, a.held)
	}

	if id, ok, _, err := u.docIDOf(guid); !ok || err != nil || id != a.out[guid] {
		t.Errorf("id not written: %d %v %v", id, ok, err)
	}

	if v := omapKeys(t, u, oidFTSMap, buildKey(guid))[buildKey(guid)]; string(v) != string(buildValue("job-held/1", a.out[guid])) {
		t.Errorf("record %q", v)
	}
}

// TestOnlineBusy (R2.6, R2.9): an account whose sources a living run
// has still to remove, and one an import locks between O1 and O2, end
// busy without a change; the run's own "next" goes.
func TestOnlineBusy(t *testing.T) {
	s := onlineStore(t, nil)
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]

	// a run in its O8
	crashAt(t, u.name, phaseO7)

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 2, Tok: "job-a/1"}); err == nil {
		t.Fatal("no crash")
	}

	ftsLayoutHook = nil

	alive := func(tok string, _ time.Time) (bool, error) { return tok == "job-a/1", nil }

	rep, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 4, Tok: "job-b/1", ForeignAlive: alive})
	if err != nil || rep.Deferred != model.FTSDeferredBusy {
		t.Fatalf("B beside A's O8: %+v %v", rep, err)
	}

	if rep, err = u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 4, Tok: "job-a/1", ForeignAlive: alive}); err != nil || !rep.Resumed {
		t.Fatalf("A finishes its O8: %+v %v", rep, err)
	}

	cleanRecord(t, u)

	// an import locks after O1
	hookPhases(t, u.name, map[string]func() error{
		phaseO2: func() error {
			_, err := u.ImportLock("importsession", time.Minute)
			return err
		},
	})

	rep, err = u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 4, Tok: "job-c/1"})
	if err != nil || rep.Deferred != model.FTSDeferredBusy {
		t.Fatalf("import after O1: %+v %v", rep, err)
	}

	cleanRecord(t, u)
	noBuildKeys(t, u)

	if err := u.ImportUnlock("importsession"); err != nil {
		t.Fatal(err)
	}
}

// TestOnlineOlderWriter (§5.4, Befund 8): documents a writer of a version
// before 0.51.0 indexes after the snapshot without recording them (and
// without postings that meet the freeze) are caught up by the one
// comparison of the whole id map before the switch, whatever their ids
// (999 and 1000 sort apart).
func TestOnlineOlderWriter(t *testing.T) {
	s := onlineStore(t, nil)
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]

	if err := casOmap(u, u.idx, oidFTSMap, []string{"n"}, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{"n": []byte("999")}, nil
	}); err != nil {
		t.Fatal(err)
	}

	var guids []string

	hookPhases(t, u.name, map[string]func() error{
		phaseO5: func() error {
			for j := range 2 {
				guid := docGUID(300 + j)
				raw := fmt.Sprintf("Subject: older\r\n\r\nolderwriter%dword\r\n", j)

				if _, err := u.writeBlob(guid, []byte(raw)); err != nil {
					return err
				}

				// the id allocation of an older version: no record
				err := casOmap(u, u.idx, oidFTSMap, []string{docKey(guid), "n"}, func(cur map[string][]byte) (map[string][]byte, []string) {
					set, next := assignDocIDs(cur, []string{guid}, map[string]uint64{})
					set["n"] = []byte(strconv.FormatUint(next, 10))

					return set, nil
				})
				if err != nil {
					return err
				}

				guids = append(guids, guid)
			}

			return nil
		},
	})

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 2, Tok: "job-older/1"}); err != nil {
		t.Fatal(err)
	}

	ftsLayoutHook = nil

	for j, guid := range guids {
		id, _, _, _ := u.docIDOf(guid)

		hits, err := u.SearchFields([]string{fts.FieldBody}, fmt.Sprintf("olderwriter%dword", j))
		if err != nil || !hits[guid] {
			t.Errorf("document of id %d not caught up: %v %v", id, hits, err)
		}
	}
}

// busyDeliveries delivers into INBOX of the view (another session) in a
// loop, expunging every third message again, until the returned function
// is called; it returns the numbers delivered and the first error.
func busyDeliveries(other *User) func() ([]int, error) {
	stop := make(chan struct{})

	var (
		wg        sync.WaitGroup
		delivered []int
		failed    error
	)

	wg.Go(func() {
		for j := 400; ; j++ {
			select {
			case <-stop:
				return
			default:
			}

			raw := fmt.Sprintf("Subject: busy %d\r\n\r\nbusy%dword\r\n", j, j)

			m, err := other.DeliverGUID("INBOX", docGUID(j), []byte(raw), nil, time.Time{})
			if err == nil && j%3 == 0 {
				err = other.Expunge("INBOX", []Meta{*m})
			}

			if err != nil {
				failed = err
				return
			}

			delivered = append(delivered, j)
		}
	})

	return func() ([]int, error) {
		close(stop)
		wg.Wait()

		return delivered, failed
	}
}

// TestOnlineConcurrentDeliveries (§8 users notice nothing): deliveries
// and expunges in parallel to an online reshard and an online rebuild
// all succeed, and afterwards every delivered message is found and no
// expunged one.
func TestOnlineConcurrentDeliveries(t *testing.T) {
	for _, rebuild := range []bool{false, true} {
		t.Run(fmt.Sprint("rebuild ", rebuild), func(t *testing.T) {
			s := onlineStore(t, nil)
			base, _ := onlineDocs()
			u := filledUsers(t, base, s)[0]

			other, err := s.User(u.name)
			if err != nil {
				t.Fatal(err)
			}

			t.Cleanup(other.Close)

			stop := busyDeliveries(other)

			_, err = u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Rebuild: rebuild, Shards: 4, Tok: "job-busy/1"})

			delivered, failed := stop()

			if err != nil || failed != nil {
				t.Fatalf("run %v, a delivery during it: %v", err, failed)
			}

			cleanRecord(t, u)
			noBuildKeys(t, u)

			for _, j := range delivered {
				hits, err := u.SearchFields([]string{fts.FieldBody}, fmt.Sprintf("busy%dword", j))
				if err != nil || hits[docGUID(j)] == (j%3 == 0) {
					t.Errorf("delivery %d: %v %v", j, hits, err)
				}
			}
		})
	}
}

// TestOnlineScrubLeftover (Befund 5): the scrub reports and removes the
// records of runs the record no longer names, and keeps those of the run
// "next" names.
func TestOnlineScrubLeftover(t *testing.T) {
	s := onlineStore(t, nil)
	base, _ := onlineDocs()
	u := filledUsers(t, base, s)[0]

	crashAt(t, u.name, phaseO2)

	if _, err := u.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 2, Tok: "job-live/1"}); err == nil {
		t.Fatal("no crash")
	}

	ftsLayoutHook = nil

	if err := casOmap(u, u.idx, oidFTSMap, nil, func(map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{buildKey("dead1"): buildValue("job-dead/1", 1), buildKey("dead2"): buildValue("job-dead/1", 2),
			buildKey("live"): buildValue("job-live/1", 3), buildCountKey: buildValue("job-dead/1", 2)}, nil
	}); err != nil {
		t.Fatal(err)
	}

	rep, err := u.Scrub(ScrubOptions{})
	if err != nil {
		t.Fatal(err)
	}

	keys := omapKeys(t, u, oidFTSMap, "w")
	if rep.FTSBuildLeftover != 3 || keys[buildKey("dead1")] != nil || keys[buildCountKey] != nil || keys[buildKey("live")] == nil {
		t.Errorf("leftover %d, keys %v", rep.FTSBuildLeftover, slices.Sorted(maps.Keys(keys)))
	}

	if _, err := u.ClearFTSLayoutNext("job-live/1"); err != nil {
		t.Fatal(err)
	}

	noBuildKeys(t, u)
}

// TestHotPathOpsOnline (§8, §6.1) counts a delivery while an online run
// is recorded (after its O2: the records come with the id allocation,
// no operation more) and while its freeze is set (after O3: one more
// read of the layout record per delivery, the anomaly round), against
// no run. Run with SHARDPOST_TEST_OPCOUNT=1 inside
// hack/ceph-test-cluster.sh.
func TestHotPathOpsOnline(t *testing.T) {
	if os.Getenv("SHARDPOST_TEST_OPCOUNT") == "" {
		t.Skip("set SHARDPOST_TEST_OPCOUNT=1 to count")
	}

	for _, phase := range []string{"", phaseO2, phaseO3} {
		t.Run("after "+phase, func(t *testing.T) {
			s := onlineStore(t, func(c *FTSConfig) { c.ShardsMin = 4 })
			name := testAccount(t)
			msg := testMessage("ops", 500)

			setup, err := s.User(name)
			must(t, err)

			mustMailbox(t, setup, "INBOX")

			_, err = setup.Deliver("INBOX", msg, nil, time.Time{})
			must(t, err)

			if phase != "" {
				crashAt(t, name, phase)

				if _, err := setup.ConvertFTSLayout(model.FTSLayoutRequest{Online: true, Shards: 4, Tok: "job-ops/1"}); err == nil {
					t.Fatal("no crash")
				}

				ftsLayoutHook = nil
			}

			setup.Close()

			countOps(t, "online run after "+phase+": LMTP delivery", 10, func() {
				for range 10 {
					u, err := s.User(name)
					must(t, err)

					_, err = u.Deliver("INBOX", msg, nil, time.Time{})
					must(t, err)
					u.Close()
				}
			})
		})
	}
}
