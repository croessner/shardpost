//go:build tentacle

package cephstore

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"shardpost/internal/config"
	"shardpost/internal/model"
)

// Cluster tests of stage B14c: fts.layout defaults to 3, which takes
// effect only with the cluster mark; fts_dict_convert is a legacy kind.

// defaultFTSLayout is fts.layout of a configuration that does not set
// it: the built-in default.
func defaultFTSLayout(t *testing.T) int {
	t.Helper()

	path := filepath.Join(t.TempDir(), "shardpost.yaml")
	if err := os.WriteFile(path, []byte("logging:\n  level: info\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	return c.FTS.Layout
}

// layoutStoreOf opens a store whose fts.layout is layout.
func layoutStoreOf(t *testing.T, layout int) *Store {
	t.Helper()

	return storeWith(t, layout3Config(func(c *FTSConfig) { c.Layout = layout }))
}

// freshLayout delivers one message into a new account of the store and
// returns the layout of the index that delivery created.
func freshLayout(t *testing.T, s *Store, subject string) int {
	t.Helper()

	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")

	if _, err := u.Deliver("INBOX", testMessage(subject, 300), nil, time.Time{}); err != nil {
		t.Fatal(err)
	}

	st, err := u.FTSLayout()
	if err != nil {
		t.Fatal(err)
	}

	if (st.Current.Layout == model.FTSLayout3) != inPresenceIndex(t, s, u.name) {
		t.Errorf("layout %d, presence index %v", st.Current.Layout, inPresenceIndex(t, s, u.name))
	}

	return st.Current.Layout
}

// gauge reads a gauge of the default registry by its full name.
func gauge(t *testing.T, name string) float64 {
	t.Helper()

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}

	for _, mf := range mfs {
		if mf.GetName() == name && len(mf.GetMetric()) == 1 {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}

	t.Fatalf("no gauge %s", name)

	return 0
}

const (
	gaugeConfigured = "imapceph_fts_layout_configured"
	gaugeEffective  = "imapceph_fts_layout_effective"
)

// TestDefaultLayoutNeedsMark: a server with the built-in default
// (fts.layout 3) creates layout 2 indexes while the cluster mark is
// absent and layout 3 indexes once it is set; the gauges show the
// configured and the effective layout. fts.layout 2 keeps layout 2 even
// with the mark.
func TestDefaultLayoutNeedsMark(t *testing.T) {
	def := defaultFTSLayout(t)
	if def != model.FTSLayout3 {
		t.Fatalf("built-in default of fts.layout is %d, want 3", def)
	}

	s := layoutStoreOf(t, def)
	setLayoutMark(t, s, false)
	t.Cleanup(func() { setLayoutMark(t, s, false) })

	if err := s.LogFTSLayout(); err != nil {
		t.Fatal(err)
	}

	if c, e := gauge(t, gaugeConfigured), gauge(t, gaugeEffective); c != 3 || e != 2 {
		t.Errorf("gauges without the mark: configured %v, effective %v", c, e)
	}

	if l := freshLayout(t, s, "default without mark"); l != model.FTSLayout2 {
		t.Errorf("default without the mark created layout %d", l)
	}

	setLayoutMark(t, s, true)

	if l := freshLayout(t, s, "default with mark"); l != model.FTSLayout3 {
		t.Errorf("default with the mark created layout %d", l)
	}

	if e := gauge(t, gaugeEffective); e != 3 {
		t.Errorf("effective layout gauge %v after an index creation with the mark", e)
	}

	s2 := layoutStoreOf(t, model.FTSLayout2)

	if l := freshLayout(t, s2, "explicit 2 with mark"); l != model.FTSLayout2 {
		t.Errorf("fts.layout 2 with the mark created layout %d", l)
	}
}

// TestDictConvertLayout3Untouched: fts_dict_convert on a layout 3 account
// reports why it skipped and writes nothing: no layout 2 object appears
// and the layout record stays as it was.
func TestDictConvertLayout3Untouched(t *testing.T) {
	s := layout3Store(t, nil)
	u := testUser(t, s)

	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(30))

	before := layoutOf(t, u)
	if before == nil || before.F != model.FTSLayout3 {
		t.Fatalf("record %+v", before)
	}

	l2 := layout2Loc(u.ftsShards)

	rep, err := u.ConvertFTSDict(func() error { return nil }, func(int, int) {})
	if err != nil || rep.Skipped != model.FTSDictSkippedLayout3 || rep.Entries != 0 || rep.LegacyObjects != 0 {
		t.Fatalf("convert on layout 3: %+v, %v", rep, err)
	}

	after := layoutOf(t, u)
	if shapeOf(after) != shapeOf(before) || after.G != before.G {
		t.Errorf("record changed: %+v, was %+v", after, before)
	}

	mustExist(t, u, false, l2.postingOIDs()...)
	mustExist(t, u, false, l2.dictOIDs()...)
	mustExist(t, u, false, l2.headerOIDs()...)
}

// TestDictConvertStopsMidRun: a conversion to layout 3 that starts
// while fts_dict_convert runs ends the run as skipped, not failed: no
// layout 2 dictionary object is written after it, and the legacy
// objects are not deleted by the dictionary run.
func TestDictConvertStopsMidRun(t *testing.T) {
	s := layoutStoreOf(t, model.FTSLayout2)
	setLayoutMark(t, s, true)
	t.Cleanup(func() { setLayoutMark(t, s, false) })

	u := testUser(t, s)
	mustMailbox(t, u, "INBOX")
	deliverDocs(t, u, layoutDocs(30))

	if l := layoutOf(t, u); l != nil {
		t.Fatalf("layout 2 account has a record %+v", l)
	}

	converted := false
	fence := func() error {
		if !converted {
			converted = true

			convert(t, u, model.FTSLayoutRequest{Layout: model.FTSLayout3})
		}

		return nil
	}

	rep, err := u.ConvertFTSDict(fence, func(int, int) {})
	if err != nil || rep.Skipped != model.FTSDictSkippedLayout3 || rep.Entries != 0 || rep.LegacyObjects != 0 {
		t.Fatalf("dictionary run across a conversion: %+v, %v", rep, err)
	}

	if l := layoutOf(t, u); l == nil || l.F != model.FTSLayout3 {
		t.Fatalf("record %+v", l)
	}

	mustExist(t, u, false, layout2Loc(u.ftsShards).dictOIDs()...)
}

// TestFTSMarkReadsSerialized: concurrent reads of the mark leave the
// gauge at the answer of the last read (the read and the update happen
// under one lock); run with -race.
func TestFTSMarkReadsSerialized(t *testing.T) {
	s := layoutStoreOf(t, model.FTSLayout3)
	setLayoutMark(t, s, false)
	t.Cleanup(func() { setLayoutMark(t, s, false) })

	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			if _, err := s.readFTSMark(); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()

	setLayoutMark(t, s, true)

	if ok, err := s.readFTSMark(); err != nil || !ok {
		t.Fatalf("mark %v, %v", ok, err)
	}

	if e := gauge(t, gaugeEffective); e != 3 {
		t.Errorf("effective layout gauge %v after the mark was read as set", e)
	}

	s.ftsMarkMu.Lock()
	defer s.ftsMarkMu.Unlock()

	if s.ftsMarkSeen != ftsMarkOn {
		t.Errorf("mark seen %d", s.ftsMarkSeen)
	}
}
