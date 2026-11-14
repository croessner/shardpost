//go:build tentacle

package cephstore

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"shardpost/internal/metrics"
)

// notified reads the count and the sum of imapceph_rados_notify_seconds
// for an object.
func notified(t *testing.T, object string) (uint64, float64) {
	t.Helper()

	var m dto.Metric
	if err := metrics.NotifyLatency.WithLabelValues(object).(prometheus.Metric).Write(&m); err != nil {
		t.Fatal(err)
	}

	return m.GetHistogram().GetSampleCount(), m.GetHistogram().GetSampleSum()
}

// settled is notified once the notifies of earlier writes of the test
// (creating the mailbox notifies too) have been observed: the count
// has not moved for a while. Their watchers' events are drained.
func settled(t *testing.T, object string) (uint64, float64) {
	t.Helper()

	count, sum := notified(t, object)

	for quiet := 0; quiet < 20; {
		time.Sleep(25 * time.Millisecond)

		n, total := notified(t, object)
		if n == count {
			quiet++

			continue
		}

		count, sum, quiet = n, total, 0
	}

	return count, sum
}

// drain empties the events a watch got before the test's own notify.
func drain(events <-chan struct{}) {
	for {
		select {
		case <-events:
		default:
			return
		}
	}
}

// observeOne sends one notify and checks that the watcher gets it and
// that it is observed once under object, acknowledged below the timeout.
func observeOne(t *testing.T, object string, send func(), events <-chan struct{}) {
	t.Helper()

	count, sum := settled(t, object)

	drain(events)
	send()

	select {
	case <-events:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s: the watcher got no notify", object)
	}

	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		n, total := notified(t, object)
		if n == count+1 {
			if took := total - sum; took <= 0 || took >= notifyTimeout.Seconds() {
				t.Errorf("%s: the notify took %v s, want above 0 and below the timeout", object, took)
			}

			return
		}

		if n > count+1 || time.Now().After(deadline) {
			t.Fatalf("%s: %d notifies observed, want 1", object, n-count)
		}
	}
}

// TestNotifyIsMeasured: every notify the store sends — on a mailbox, on
// the account, on an object of its own namespaces — is observed once,
// under its object, with the time until the watcher acknowledged it
// (well below the timeout).
func TestNotifyIsMeasured(t *testing.T) {
	s := testStore(t)
	u := testUser(t, s)

	must(t, u.CreateMailbox("INBOX", nil))

	mb, err := u.Lookup("INBOX")
	if err != nil {
		t.Fatal(err)
	}

	kv, err := s.AdminKV()
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()

	oid := "ix.test/notify/" + t.Name()

	mailboxWatch, err := u.Watch(mb.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer mailboxWatch.Close()

	accountWatch, err := u.WatchAccount()
	if err != nil {
		t.Fatal(err)
	}
	defer accountWatch.Close()

	queueWatch, err := kv.Omap().Watch(oid)
	if err != nil {
		t.Fatal(err)
	}
	defer queueWatch.Close()

	for _, c := range []struct {
		object string
		send   func()
		events <-chan struct{}
	}{
		{notifiedMailbox, func() { u.notify(mb.ID) }, mailboxWatch.Events()},
		{notifiedAccount, u.notifyAccount, accountWatch.Events()},
		{notifiedQueue, func() { kv.Omap().Notify(oid) }, queueWatch.Events()},
	} {
		observeOne(t, c.object, c.send, c.events)
	}
}
