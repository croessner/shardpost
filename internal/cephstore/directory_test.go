//go:build tentacle

package cephstore

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// TestListingsAreCounted: every RADOS object listing of the store goes
// through listObjects, which counts it by call site; a new Iter() call
// elsewhere would list the pool unseen.
func TestListingsAreCounted(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	iter := regexp.MustCompile(`\.Iter\(\)`)
	sites := map[string]bool{}
	site := regexp.MustCompile(`listObjects\([^,]+, (list[A-Za-z]+)\)|q\.all\((list[A-Za-z]+)\)`)

	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}

		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}

		if f != "metrics.go" && iter.Match(b) {
			t.Errorf("%s calls Iter() directly; use listObjects", f)
		}

		for _, m := range site.FindAllStringSubmatch(string(b), -1) {
			sites[m[1]+m[2]] = true
		}
	}

	for _, want := range []string{"listUsers", "listScrubOrphans", "listScrubUnknown", "listPurgeObjects", "listAdminKV",
		"listLearningAll", "listLearningPurge"} {
		if !sites[want] {
			t.Errorf("listing site %s is not used", want)
		}
	}

	value := func() float64 {
		var m dto.Metric
		if err := metrics.RadosListings.WithLabelValues(listAdminKV).Write(&m); err != nil {
			t.Fatal(err)
		}

		return m.GetCounter().GetValue()
	}

	before := value()

	mh.listing(listAdminKV)

	if got := value(); got != before+1 {
		t.Fatalf("counter %v, want %v", got, before+1)
	}
}

func TestStoreUserRejectsInvalidNamespaces(t *testing.T) {
	s := &Store{}

	for _, bad := range []string{"", "~jmap", "~"} {
		if _, err := s.User(bad); !errors.Is(err, model.ErrNamespace) {
			t.Errorf("User(%q): %v", bad, err)
		}
	}
}

// TestHookSkipsWithoutStore: a view without a store (the views of
// tests) neither registers nor checks grants.
func TestHookSkipsWithoutStore(t *testing.T) {
	u := &User{name: "alice"}

	if err := u.registerAccount(""); err != nil {
		t.Fatalf("no store: %v", err)
	}

	if blocked, err := u.grantBlocked("bob"); blocked || err != nil {
		t.Fatalf("no store: %v %v", blocked, err)
	}
}

func TestIndexObjectNames(t *testing.T) {
	if got := sortedindex.HeadOID(model.AccountIndexName); got != "ix.accounts/head" {
		t.Errorf("head %q", got)
	}

	if got := sortedindex.ShardOID(model.AccountIndexName, 7); got != "ix.accounts/07" {
		t.Errorf("shard %q", got)
	}

	if got := sortedindex.ShardOID(model.AccountIndexName, 255); got != "ix.accounts/255" {
		t.Errorf("shard %q", got)
	}

	// Never under kv. (older servers read those as admin records), never
	// the per-account "ix/<space>/<nn>".
	if strings.HasPrefix(sortedindex.HeadOID("x"), "kv.") || strings.HasPrefix(sortedindex.ShardOID("x", 0), "ix/") {
		t.Error("index objects collide with other layouts")
	}
}
