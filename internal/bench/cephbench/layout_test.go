//go:build tentacle

package cephbench

import (
	"context"
	"strings"
	"testing"

	"shardpost/internal/bench"
	"shardpost/internal/cephstore"
	"shardpost/internal/model"
)

// TestPopulateLayout2ThenConvert (§7): accounts populated in layout 2
// are converted by the convert probe; the second (counted) round
// reshards them, the inventory then finds one generation of S = D = H
// = 1 per account (classes ftx-p, ftx-d, ftx-h) and no layout 2
// object, and the searches after the conversion find what they found
// before.
func TestPopulateLayout2ThenConvert(t *testing.T) {
	s, cfg := benchStore(t)
	prefix := testPrefix(t)
	ctx := context.Background()

	if _, err := Populate(ctx, guard(prefix), s, cfg, deliverer(s), populateOptions(prefix)); err != nil {
		t.Fatal(err)
	}

	if err := MarkLayout3(s); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { unmark(t, s) })

	m, err := Measure(ctx, MeasureOptions{Guard: guard(prefix), Config: cfg, Store: s, Every: 1, Parallel: 4,
		ProbeAccounts: testAccts, Search: searchQueries, Convert: true, TraceDir: t.TempDir(), Log: discard()})
	if err != nil {
		t.Fatal(err)
	}

	c := m.Convert
	if c == nil || c.Count != testAccts || c.Ops[benchIndex][bench.ClassFTXP].Writes < 1 {
		t.Fatalf("convert probe %+v", c)
	}

	if m.SearchAfter == nil || m.SearchAfter.Fingerprint != m.Search.Fingerprint {
		t.Errorf("search after the conversion %+v, before %s", m.SearchAfter, m.Search.Fingerprint)
	}

	checkConverted(t, s, cfg, prefix)

	var out strings.Builder
	if err := bench.WriteMeasureTable(&out, m); err != nil || !strings.Contains(out.String(), "fts_layout conversion") {
		t.Errorf("table %v:\n%s", err, out.String())
	}
}

// checkConverted: the inventory finds one layout 3 generation of S = D =
// H = 1 per account and no layout 2 object.
func checkConverted(t *testing.T, s *cephstore.Store, cfg cephstore.Config, prefix string) {
	t.Helper()

	after, err := Measure(context.Background(), MeasureOptions{Guard: guard(prefix), Config: cfg, Store: s, Every: 1, Parallel: 4,
		TraceDir: t.TempDir(), Log: discard()})
	if err != nil {
		t.Fatal(err)
	}

	for _, class := range []string{bench.ClassFTXP, bench.ClassFTXD, bench.ClassFTXH} {
		exactly(t, after, benchIndex, class, 1)
	}

	for _, class := range []string{bench.ClassFTS, bench.ClassFTST, bench.ClassFTSH} {
		if n := after.Inventory.PerAccount[benchIndex][class].Objects.Max; n != 0 {
			t.Errorf("%s objects left after the conversion: %d", class, n)
		}
	}
}

// unmark removes the layout 3 mark of the bench pools.
func unmark(t *testing.T, s *cephstore.Store) {
	t.Helper()

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	if err := kv.Update(model.FTSLayoutMarkKey, func([]byte) ([]byte, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
}
