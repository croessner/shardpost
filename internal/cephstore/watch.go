package cephstore

import (
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
)

// Change notification
//
// RADOS has watch/notify built in: a client registers a watch on an object
// and every notify on that object is delivered to all watchers, cluster
// wide. The store sends a notify on the mailbox index object after every
// successful index write, and an IMAP session in IDLE watches the object of
// its selected mailbox. That is how a session on pod A learns within
// milliseconds that pod B delivered a message, without any message bus.
//
// Notifies are best effort and asynchronous: a lost notify only means the
// client sees the change at its next command instead of immediately.

const notifyTimeout = time.Second

// notify wakes all watchers of the mailbox object. It runs in the
// background; User.Close waits for it before destroying the IOContext.
func (u *User) notify(id string) {
	u.inflight.Add(1)
	go func() {
		defer u.inflight.Done()

		if _, _, err := u.idx.NotifyWithTimeout(mboxOID(id), nil, notifyTimeout); err != nil {
			u.log.Debug("notify failed", "mailbox", id, "err", err)
		}
	}()
}

// Watch registers a watch on the mailbox object. Notifies are acknowledged
// internally; Close must be called when done. The IOContext stays alive
// until the watch is closed.
func (u *User) Watch(id string) (model.Watch, error) {
	rw, err := u.idx.Watch(mboxOID(id))
	if err != nil {
		return nil, err
	}

	u.inflight.Add(1)

	w := &Watch{w: rw, events: make(chan struct{}, 1), done: make(chan struct{})}
	go w.pump(u)

	return w, nil
}

// Watch is a live subscription to changes of one mailbox.
type Watch struct {
	w      *rados.Watcher
	events chan struct{}
	done   chan struct{}
}

func (w *Watch) pump(u *User) {
	defer u.inflight.Done()

	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.w.Events():
			if !ok {
				return
			}

			_ = ev.Ack(nil)

			select {
			case w.events <- struct{}{}:
			default: // coalesce
			}
		}
	}
}

// Events yields one value per (coalesced) notify.
func (w *Watch) Events() <-chan struct{} { return w.events }

// Errors yields watch errors (e.g. the OSD dropped the watch).
func (w *Watch) Errors() <-chan error { return w.w.Errors() }

// Close removes the watch.
func (w *Watch) Close() {
	close(w.done)
	_ = w.w.Delete()
}
