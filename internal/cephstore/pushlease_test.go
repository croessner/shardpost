//go:build tentacle

package cephstore

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// The push worker's leases against a real cluster (stage B18a, §8.2):
// a LockQueueKeep lease renews and reports its deadline, expires after
// its holder died without an unlock, stands through a key rotation
// and moves to the new connection with Retake, while a LockQueue lease
// is lost by the rotation as before; a notify carries its payload;
// ListShard reads what ListOrdered reads.

// pushTestLock is the lease name of a test.
func pushTestLock(t *testing.T) string { return "test/push/" + t.Name() }

// abandon ends the renewal of a lease without an unlock, as the death
// of the holder's process would, and closes its view; Release becomes
// a no-op.
func abandon(t *testing.T, ml model.Lease) {
	t.Helper()

	l, ok := ml.(*lease)
	if !ok {
		t.Fatal("not a cephstore lease")
	}

	l.mu.Lock()
	l.released = true
	stop, done := l.stop, l.done
	l.mu.Unlock()

	close(stop)
	<-done

	l.once.Do(func() {})

	u, _ := l.view()
	u.Close()
}

// TestPushLeaseKeepRenewsAndExpires: a LockQueueKeep lease renews itself
// and its deadline moves on; another store gets ErrLocked while it is
// held; once its holder is gone without an unlock, the other store
// takes it within the TTL and a cadence.
func TestPushLeaseKeepRenewsAndExpires(t *testing.T) {
	kv, kv2 := twoAdminKVs(t)

	const ttl = 3 * time.Second

	l, err := kv.LockQueueKeep(pushTestLock(t), ttl)
	if err != nil {
		t.Fatal(err)
	}

	first := deadlineOf(t, l)
	if first.IsZero() || time.Until(first) > ttl {
		t.Fatalf("deadline %s after the take", first)
	}

	time.Sleep(ttl/3 + 500*time.Millisecond)

	if l.Lost() || !deadlineOf(t, l).After(first) {
		t.Fatalf("not renewed: lost %v, deadline %s then %s", l.Lost(), first, deadlineOf(t, l))
	}

	if _, err := kv2.LockQueueKeep(pushTestLock(t), ttl); !errors.Is(err, ErrLocked) {
		t.Fatalf("a held lease was taken by another store: %v", err)
	}

	// The holder dies.
	died := time.Now()

	abandon(t, l)

	taken := takeWhenFree(t, kv2, pushTestLock(t), ttl, ttl+2*time.Second)
	defer taken.Release()

	if took := time.Since(died); took < ttl/2 {
		t.Errorf("the take-over took %s, want about the TTL of %s", took, ttl)
	}
}

// twoAdminKVs opens the admin stores of two stores of their own.
func twoAdminKVs(t *testing.T) (*AdminKV, *AdminKV) {
	t.Helper()

	return adminKVOf(t, storeWith(t, func(*Config) {})), adminKVOf(t, storeWith(t, func(*Config) {}))
}

// adminKVOf opens the admin store of s (closed with the test).
func adminKVOf(t *testing.T, s *Store) *AdminKV {
	t.Helper()

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(kv.Close)

	return kv
}

// deadlineOf is the deadline of a lease.
func deadlineOf(t *testing.T, l model.Lease) time.Time {
	t.Helper()

	dl, ok := l.(model.LeaseDeadline)
	if !ok {
		t.Fatal("the lease has no deadline")
	}

	return dl.ValidUntil()
}

// takeWhenFree tries the lease every 200 ms until it is taken, at most
// for patience.
func takeWhenFree(t *testing.T, kv *AdminKV, name string, ttl, patience time.Duration) model.Lease {
	t.Helper()

	deadline := time.Now().Add(patience)

	for {
		l, err := kv.LockQueueKeep(name, ttl)

		switch {
		case err == nil:
			return l
		case !errors.Is(err, ErrLocked):
			t.Fatal(err)
		case time.Now().After(deadline):
			t.Fatal("the abandoned lease was not taken after its TTL")
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// TestPushLeaseRetakeOnRotate: a key rotation leaves a LockQueueKeep
// lease standing, runs the rotation hooks, and Retake moves the lease
// to the new connection with a new cookie, after which it renews
// there and another store still cannot take it; a LockQueue lease of
// the same store is lost by the rotation, as before.
func TestPushLeaseRetakeOnRotate(t *testing.T) {
	cfg := testConfig()
	s := storeWith(t, func(*Config) {})
	kv, kv2 := adminKVOf(t, s), adminKVOf(t, storeWith(t, func(*Config) {}))
	keep, plain := keepAndPlain(t, kv, pushTestLock(t))

	hooks := 0

	defer s.OnRotate(func() { hooks++ })()

	kl := keep.(*lease)
	oldView, oldCookie := kl.view()

	if err := s.Rotate(cfg.Key, ""); err != nil {
		t.Fatal(err)
	}

	if hooks != 1 || keep.Lost() || !plain.Lost() {
		t.Fatalf("after the rotation: hooks %d, keep lost %v, plain lost %v", hooks, keep.Lost(), plain.Lost())
	}

	if err := keep.(model.LeaseRetaker).Retake(); err != nil {
		t.Fatal(err)
	}

	newView, newCookie := kl.view()
	if newView == oldView || newCookie == oldCookie {
		t.Fatal("the retake kept the old view or cookie")
	}

	if info := lockersOf(t, s, sortedindex.LockOID(pushTestLock(t))); info.NumLockers != 1 || len(info.Cookies) != 1 || info.Cookies[0] != newCookie {
		t.Fatalf("lockers after the retake: %+v, want one with cookie %s", info, newCookie)
	}

	if _, err := kv2.LockQueueKeep(pushTestLock(t), 10*time.Second); !errors.Is(err, ErrLocked) {
		t.Fatalf("another store took the retaken lease: %v", err)
	}

	expectRenewal(t, keep, 10*time.Second)
}

// TestPushLeaseRetakeFailsThenRelease: when another store holds the
// lock by the time Retake takes it again, the Retake fails, the lease is
// lost and holds no view any more, and the Release that follows neither
// unlocks through the closed view nor closes it a second time; the
// other store keeps its lock.
func TestPushLeaseRetakeFailsThenRelease(t *testing.T) {
	s := storeWith(t, func(*Config) {})
	kv, kv2 := adminKVOf(t, s), adminKVOf(t, storeWith(t, func(*Config) {}))
	name := pushTestLock(t)

	keep, err := kv.LockQueueKeep(name, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	// The lock is freed behind the lease's back (the renewal comes every
	// ten seconds) and the other store takes it.
	kl := keep.(*lease)
	u, cookie := kl.view()

	if _, err := u.idx.Unlock(sortedindex.LockOID(name), "worker", cookie); err != nil {
		t.Fatal(err)
	}

	other, err := kv2.LockQueueKeep(name, 30*time.Second)
	if err != nil {
		t.Fatalf("the other store could not take the freed lock: %v", err)
	}
	defer other.Release()

	if err := keep.(model.LeaseRetaker).Retake(); !errors.Is(err, ErrLocked) {
		t.Fatalf("Retake against another holder: %v, want ErrLocked", err)
	}

	if v, _ := kl.view(); v != nil || !keep.Lost() {
		t.Fatalf("after the failed retake: view %p, lost %v", v, keep.Lost())
	}

	keep.Release()
	keep.Release()

	_, otherCookie := other.(*lease).view()
	if info := lockersOf(t, s, sortedindex.LockOID(name)); info.NumLockers != 1 || len(info.Cookies) != 1 || info.Cookies[0] != otherCookie {
		t.Fatalf("lockers after the release: %+v, want the other store's only", info)
	}
}

// keepAndPlain takes a LockQueueKeep lease on name and a LockQueue lease
// beside it (both released with the test).
func keepAndPlain(t *testing.T, kv *AdminKV, name string) (keep, plain model.Lease) {
	t.Helper()

	keep, err := kv.LockQueueKeep(name, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(keep.Release)

	plain, err = kv.LockQueue(name+"/plain", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(plain.Release)

	return keep, plain
}

// expectRenewal waits a renewal interval and checks that the lease
// renewed.
func expectRenewal(t *testing.T, l model.Lease, ttl time.Duration) {
	t.Helper()

	before := deadlineOf(t, l)

	time.Sleep(ttl/3 + 500*time.Millisecond)

	if l.Lost() || !deadlineOf(t, l).After(before) {
		t.Fatalf("not renewed: lost %v", l.Lost())
	}
}

// lockersOf lists the lockers of the worker lock on oid.
func lockersOf(t *testing.T, s *Store, oid string) *rados.LockInfo {
	t.Helper()

	u, err := s.User(adminNamespace)
	if err != nil {
		t.Fatal(err)
	}

	defer u.Close()

	info, err := u.idx.ListLockers(oid, "worker")
	if err != nil {
		t.Fatal(err)
	}

	return info
}

// TestPushPayloadNotify: a payload watch gets the data of a notify, two
// watchers both, a plain watch an event; a notify on an object nobody
// watches is no error.
func TestPushPayloadNotify(t *testing.T) {
	s := storeWith(t, func(*Config) {})

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	oid := "ix.test/push/" + t.Name() + "/lock"
	om := kv.Omap()

	w1, err := om.WatchPayload(oid)
	if err != nil {
		t.Fatal(err)
	}
	defer w1.Close()

	w2, err := om.WatchPayload(oid)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	plain, err := om.Watch(oid)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()

	om.NotifyPayload(oid, []byte(`{"a":"alice"}`))

	for i, w := range []model.PayloadWatch{w1, w2} {
		select {
		case data := <-w.Events():
			if string(data) != `{"a":"alice"}` {
				t.Fatalf("watcher %d got %q", i, data)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("watcher %d got nothing", i)
		}
	}

	select {
	case <-plain.Events():
	case <-time.After(10 * time.Second):
		t.Fatal("the plain watch got nothing")
	}

	om.NotifyPayload("ix.test/push/nobody-watches", []byte("x")) // best effort, no error to see
}

// TestPushListShard: the union of ListShard over every shard is what
// ListOrdered lists, and a shard page never reads another shard.
func TestPushListShard(t *testing.T) {
	s := storeWith(t, func(*Config) {})

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	ix := sortedindex.New(kv.omap, "test/push/listshard/"+t.Name(), 64)

	want := make([]string, 0, 500)

	for i := range 500 {
		id := fmt.Sprintf("user%03d@example.org", i)
		want = append(want, id)

		if err := ix.Set(id, []byte(`{"verified":true}`)); err != nil {
			t.Fatal(err)
		}
	}

	got := listAllShards(t, ix)

	ordered, err := ix.ListOrdered("", 1000)
	if err != nil {
		t.Fatal(err)
	}

	viaOrdered := make([]string, 0, len(ordered))
	for _, e := range ordered {
		viaOrdered = append(viaOrdered, e.ID)
	}

	sort.Strings(got)
	sort.Strings(want)

	if !slices.Equal(got, want) || !slices.Equal(viaOrdered, want) {
		t.Fatalf("ListShard %d, ListOrdered %d entries, want %d (the same ids)", len(got), len(viaOrdered), len(want))
	}
}

// listAllShards pages through every shard with ListShard and checks
// that a shard lists its own ids only.
func listAllShards(t *testing.T, ix *sortedindex.Index) []string {
	t.Helper()

	var got []string

	for shard := range 64 {
		after := ""

		for {
			page, more, err := ix.ListShard(shard, after, 7)
			if err != nil {
				t.Fatal(err)
			}

			for _, e := range page {
				if model.IndexShard(e.ID, 64) != shard {
					t.Fatalf("shard %d listed %s of shard %d", shard, e.ID, model.IndexShard(e.ID, 64))
				}

				got = append(got, e.ID)
				after = e.ID
			}

			if !more {
				break
			}
		}
	}

	return got
}
