package model

import (
	"errors"
	"testing"
	"time"
)

// cacheFixture is a cache on a test clock with a counted loader.
type cacheFixture struct {
	t     *testing.T
	c     *ServerMetadataCache
	now   time.Time
	reads int
	value string
}

func newCacheFixture(t *testing.T) *cacheFixture {
	f := &cacheFixture{t: t, now: time.Unix(1_000_000, 0), value: "a"}
	f.c = &ServerMetadataCache{Now: func() time.Time { return f.now }}

	return f
}

func (f *cacheFixture) load() (map[string]string, error) {
	f.reads++

	return map[string]string{"/shared/imapsieve/script": f.value}, nil
}

// get returns the entry and checks the number of reads so far.
func (f *cacheFixture) get(want string, reads int) {
	f.t.Helper()

	v, err := f.c.Get(f.load)
	if err != nil {
		f.t.Fatal(err)
	}

	if got := v["/shared/imapsieve/script"]; got != want || f.reads != reads {
		f.t.Fatalf("got %q after %d reads, want %q after %d", got, f.reads, want, reads)
	}
}

func TestServerMetadataCache(t *testing.T) {
	f := newCacheFixture(t)

	// A hit within the TTL reads nothing.
	f.get("a", 1)
	f.get("a", 1)

	// The caller's map is its own.
	v, _ := f.c.Get(f.load)
	v["/shared/imapsieve/script"] = "changed"

	f.get("a", 1)

	// Expired after ServerMetadataTTL: read again.
	f.value = "b"
	f.now = f.now.Add(ServerMetadataTTL - time.Second)

	f.get("a", 1)

	f.now = f.now.Add(time.Second)

	f.get("b", 2)

	// A local change drops it at once.
	f.value = "c"

	f.c.Invalidate()
	f.get("c", 3)
}

func TestServerMetadataCacheErrors(t *testing.T) {
	f := newCacheFixture(t)
	down := func() (map[string]string, error) { return nil, errors.New("down") }

	f.get("a", 1)

	// A cached value answers within the TTL whatever the cluster does.
	if _, err := f.c.Get(down); err != nil {
		t.Fatal(err)
	}

	// An error is reported and not cached.
	f.c.Invalidate()

	if _, err := f.c.Get(down); err == nil {
		t.Fatal("the error of a read was lost")
	}

	f.get("a", 2)
}

// TestServerMetadataCacheInvalidateDuringRead: a read that started
// before a local change does not refill the cache with the old value.
func TestServerMetadataCacheInvalidateDuringRead(t *testing.T) {
	var c ServerMetadataCache

	_, _ = c.Get(func() (map[string]string, error) {
		c.Invalidate() // the change lands while the read is under way

		return map[string]string{"k": "old"}, nil
	})

	v, _ := c.Get(func() (map[string]string, error) { return map[string]string{"k": "new"}, nil })
	if v["k"] != "new" {
		t.Fatalf("a read from before the change was kept: %v", v)
	}
}

func TestSystemNamespace(t *testing.T) {
	for _, ns := range SystemNamespaces() {
		if !SystemNamespace(ns) {
			t.Errorf("%s is a system namespace", ns)
		}
	}

	for _, ns := range []string{PublicNamespace, "", "alice@example.org", "~bench", "~admin.moved/run1"} {
		if SystemNamespace(ns) {
			t.Errorf("%q is no system namespace", ns)
		}
	}
}

func TestServerRecordRunning(t *testing.T) {
	now := time.Now()

	for _, tc := range []struct {
		rec  ServerRecord
		want bool
	}{
		{ServerRecord{Seen: now}, true},
		{ServerRecord{Seen: now.Add(-ServerFresh)}, true},
		{ServerRecord{Seen: now.Add(-ServerFresh - time.Second)}, false},
		{ServerRecord{Seen: now, StoppedAt: now}, false},
	} {
		if got := tc.rec.Running(now); got != tc.want {
			t.Errorf("%+v: running %v", tc.rec, got)
		}
	}

	if !(ServerRecord{}).IsServe() || !(ServerRecord{Role: RoleServe}).IsServe() || (ServerRecord{Role: RolePush}).IsServe() {
		t.Error("IsServe")
	}
}
