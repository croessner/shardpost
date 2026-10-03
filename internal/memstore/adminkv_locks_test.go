package memstore

import (
	"errors"
	"testing"
	"time"

	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
)

// The test hooks of the in-memory worker locks (stage B18a, §8.1): the
// renewal goroutine with its deadline, the lock hook that blocks or
// refuses, the rotation that spares LockQueueKeep leases and counts
// handles, the retake, and the payload watch.

// TestMemLeaseRenewsAndExpires: a lease renews itself and reports its
// deadline; one whose holder is gone (Abandon) expires and can be
// taken by another.
func TestMemLeaseRenewsAndExpires(t *testing.T) {
	kv := NewAdminKV()

	l, err := kv.LockQueueKeep("q", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	dl, ok := l.(model.LeaseDeadline)
	if !ok {
		t.Fatal("no deadline")
	}

	first := dl.ValidUntil()

	time.Sleep(450 * time.Millisecond)

	if l.Lost() || !dl.ValidUntil().After(first) {
		t.Fatalf("not renewed: lost %v, deadline %s then %s", l.Lost(), first, dl.ValidUntil())
	}

	if _, err := kv.LockQueueKeep("q", 300*time.Millisecond); !errors.Is(err, model.ErrLocked) {
		t.Fatalf("a held lease was taken: %v", err)
	}

	l.(interface{ Abandon() }).Abandon()

	time.Sleep(400 * time.Millisecond)

	other, err := kv.LockQueueKeep("q", 300*time.Millisecond)
	if err != nil {
		t.Fatalf("an abandoned lease was not taken after its TTL: %v", err)
	}

	defer other.Release()

	// The old holder's lease is lost once it tries to renew against the
	// new holder; Release does not unlock the other's lock.
	l.Release()

	if !kv.Locks().Held("q") {
		t.Fatal("the release of the old lease unlocked the new one")
	}
}

// TestMemLockHook: the hook refuses a lock and loses a lease on a
// refused renewal.
func TestMemLockHook(t *testing.T) {
	kv := NewAdminKV()

	kv.Locks().SetHook(func(op, name string) error {
		if op == LockOpLock && name == "refused" {
			return errors.New("refused")
		}

		return nil
	})

	if _, err := kv.LockQueue("refused", time.Second); err == nil {
		t.Fatal("the hook did not refuse the lock")
	}

	l, err := kv.LockQueue("renewed", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	kv.Locks().SetHook(func(op, _ string) error {
		if op == LockOpRenew {
			return errors.New("renewal refused")
		}

		return nil
	})

	time.Sleep(200 * time.Millisecond)

	if !l.Lost() {
		t.Fatal("a refused renewal did not lose the lease")
	}

	l.Release()
}

// TestMemLockHookHoldsRenewal: a hook that holds a renewal makes
// Release wait for it.
func TestMemLockHookHoldsRenewal(t *testing.T) {
	kv := NewAdminKV()
	gate := make(chan struct{})

	kv.Locks().SetHook(func(op, _ string) error {
		if op == LockOpRenew {
			<-gate
		}

		return nil
	})

	held, err := kv.LockQueue("held", 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(150 * time.Millisecond) // the renewal hangs now

	released := make(chan struct{})

	go func() {
		held.Release()
		close(released)
	}()

	select {
	case <-released:
		t.Fatal("Release did not wait for the renewal in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(gate)

	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("Release did not come back after the renewal")
	}
}

// TestMemRotate: a rotation loses the LockQueue leases and spares the
// LockQueueKeep ones, runs the hooks, and counts the handles; Retake
// moves a lease to the new handle; RevokeLocks loses both kinds.
func TestMemRotate(t *testing.T) {
	kv := NewAdminKV()
	plain, keep := twoLeases(t, kv)

	rotated := 0
	off := kv.OnRotate(func() { rotated++ })

	kv.Rotate()

	if rotated != 1 || !plain.Lost() || keep.Lost() {
		t.Fatalf("after the rotation: hooks %d, plain lost %v, keep lost %v", rotated, plain.Lost(), keep.Lost())
	}

	expectHandles(t, kv, 0, 1)

	if err := keep.(model.LeaseRetaker).Retake(); err != nil {
		t.Fatal(err)
	}

	expectHandles(t, kv, 1, 0)

	off()
	kv.Rotate()

	if rotated != 1 {
		t.Fatal("a removed hook ran")
	}

	kv.RevokeLocks()

	if !keep.Lost() {
		t.Fatal("RevokeLocks spared a LockQueueKeep lease")
	}

	plain.Release()
	keep.Release()
}

// twoLeases takes a LockQueue and a LockQueueKeep lease.
func twoLeases(t *testing.T, kv *AdminKV) (plain, keep model.Lease) {
	t.Helper()

	plain, err := kv.LockQueue("plain", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	keep, err = kv.LockQueueKeep("keep", time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	return plain, keep
}

// expectHandles checks the leases per handle.
func expectHandles(t *testing.T, kv *AdminKV, current, old int) {
	t.Helper()

	if cur, o := kv.LockHandles(); cur != current || o != old {
		t.Fatalf("handles: current %d, old %d; want %d and %d", cur, o, current, old)
	}
}

// TestMemBackendHook: the omap hook holds a read and refuses a write.
func TestMemBackendHook(t *testing.T) {
	kv := NewAdminKV()
	om := kv.Omap()

	if err := om.Write("obj", model.OmapAnyVersion, map[string][]byte{"k": []byte("v")}, nil); err != nil {
		t.Fatal(err)
	}

	gate := make(chan struct{})

	kv.Backend().SetHook(func(op, oid string) error {
		switch {
		case oid == "obj" && op == "read_page":
			<-gate
		case oid == "obj" && op == "write":
			return errors.New("refused")
		}

		return nil
	})

	if err := om.Write("obj", model.OmapAnyVersion, map[string][]byte{"k": []byte("w")}, nil); err == nil {
		t.Fatal("the hook did not refuse the write")
	}

	read := make(chan struct{})

	go func() {
		_, _, _ = om.ReadPage("obj", "", 10)

		close(read)
	}()

	select {
	case <-read:
		t.Fatal("the hook did not hold the read")
	case <-time.After(100 * time.Millisecond):
	}

	close(gate)
	<-read

	kv.Backend().SetHook(nil)

	if vals, _, err := om.ReadKeys("obj", []string{"k"}); err != nil || string(vals["k"]) != "v" {
		t.Fatalf("after the hook: %s %v", vals["k"], err)
	}
}

// TestMemPayloadWatch: a payload watch gets the data of every notify,
// a plain watch an event; an overflow becomes one empty event.
func TestMemPayloadWatch(t *testing.T) {
	kv := NewAdminKV()
	om := kv.Omap()

	pw, err := om.WatchPayload("obj")
	if err != nil {
		t.Fatal(err)
	}

	defer pw.Close()

	plain, err := om.Watch("obj")
	if err != nil {
		t.Fatal(err)
	}

	defer plain.Close()

	om.NotifyPayload("obj", []byte("one"))
	expectPayload(t, pw, "one")

	select {
	case <-plain.Events():
	case <-time.After(time.Second):
		t.Fatal("the plain watch saw nothing")
	}

	for i := range 2 * sortedindex.PayloadBuffer {
		om.NotifyPayload("obj", []byte{byte(i)})
	}

	// The queue holds PayloadBuffer events, and the pump may hold one more
	// in its hand.
	if got := untilOverflow(t, pw); got > sortedindex.PayloadBuffer+1 || got == 0 {
		t.Fatalf("%d events before the overflow event, want up to %d", got, sortedindex.PayloadBuffer+1)
	}
}

// expectPayload reads one payload.
func expectPayload(t *testing.T, pw model.PayloadWatch, want string) {
	t.Helper()

	select {
	case data := <-pw.Events():
		if string(data) != want {
			t.Fatalf("payload %q, want %q", data, want)
		}
	case <-time.After(time.Second):
		t.Fatal("no payload")
	}
}

// untilOverflow counts the events with data before the empty one.
func untilOverflow(t *testing.T, pw model.PayloadWatch) int {
	t.Helper()

	got := 0

	for {
		select {
		case data := <-pw.Events():
			if len(data) == 0 {
				return got
			}

			got++
		case <-time.After(time.Second):
			t.Fatalf("after %d events no overflow event", got)
		}
	}
}
