//go:build tentacle

package cephstore

import (
	"errors"
	"log/slog"
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

	for _, want := range []string{"listUsers", "listScrubOrphans", "listScrubUnknown", "listPurgeObjects", "listAdminKV", "listAdminBackfill",
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

	// The admin store's queues and their worker lock, and the account
	// directory lock taken before R1b, keep their names.
	for got, want := range map[string]string{
		sortedindex.ShardOID(model.JobsClaimableQueue, 0): "ix.jobs/claimable/00",
		sortedindex.HeadOID(model.JobsExpiryQueue):        "ix.jobs/expiry/head",
		sortedindex.LockOID(model.JobsExpiryQueue):        "ix.jobs/expiry/lock",
		sortedindex.LockOID(model.AccountIndexName):       "ix.accounts/lock",
		sortedindex.ShardOID("jmap/push/acct", 63):        "ix.jmap/push/acct/63",
	} {
		if got != want {
			t.Errorf("object %q, want %q", got, want)
		}
	}
}

// TestRotateReleasesWorkerLocks: a key rotation unlocks the tracked
// worker locks through their own handles and marks them lost; a later
// release does not unlock twice.
func TestRotateReleasesWorkerLocks(t *testing.T) {
	s := &Store{log: slog.New(slog.DiscardHandler)}

	unlocked := 0
	l := &lease{stop: make(chan struct{})}
	l.unlock = func() { unlocked++; s.forgetHeld(l) }

	s.rememberHeld(l)
	s.releaseHeld()

	if !l.Lost() || unlocked != 1 || len(s.held) != 0 {
		t.Fatalf("after rotation: lost=%v unlocked=%d held=%d", l.Lost(), unlocked, len(s.held))
	}

	l.Release()

	if unlocked != 1 {
		t.Fatalf("unlocked %d times", unlocked)
	}

	s.releaseHeld() // nothing left
}
