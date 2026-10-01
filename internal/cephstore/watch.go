package cephstore

import (
	"sync"
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

// notifyAccount wakes all watchers of the account object: the account
// change log has a new record. Like notify, it is best effort.
func (u *User) notifyAccount() {
	u.inflight.Add(1)
	go func() {
		defer u.inflight.Done()

		if _, _, err := u.idx.NotifyWithTimeout(oidAccount, nil, notifyTimeout); err != nil {
			u.log.Debug("account notify failed", "err", err)
		}
	}()
}

// Watch registers a watch on the mailbox object. Notifies are acknowledged
// internally; Close must be called when done. The IOContext stays alive
// until the watch is closed.
func (u *User) Watch(id string) (model.Watch, error) { return u.watchObject(mboxOID(id)) }

// WatchAccount registers a watch on the account object, which is notified
// whenever the account change log records a change.
func (u *User) WatchAccount() (model.Watch, error) { return u.watchObject(oidAccount) }

// watchObject registers a watch on one object of the index pool.
func (u *User) watchObject(oid string) (model.Watch, error) {
	w, err := u.watchObj(oid)
	if err != nil {
		return nil, err
	}

	return w, nil
}

// watchObj is watchObject with the concrete type.
func (u *User) watchObj(oid string) (*Watch, error) {
	rw, err := u.idx.Watch(oid)
	if err != nil {
		return nil, err
	}

	u.inflight.Add(1)

	w := &Watch{w: rw, events: make(chan struct{}, 1), errs: make(chan error, 1), done: make(chan struct{})}
	go w.pump(u)

	return w, nil
}

// Watch is a live subscription to changes of one object.
type Watch struct {
	w      *rados.Watcher
	events chan struct{}
	// errs carries the watch errors, coalesced into one pending error,
	// so that the librados callback thread that reports them never
	// blocks on a reader that does not read.
	errs chan error
	done chan struct{}
	once sync.Once
	// onClose (if set) runs once when the watch is closed.
	onClose func()
}

func (w *Watch) pump(u *User) {
	defer u.inflight.Done()

	errs := w.w.Errors()

	for {
		select {
		case <-w.done:
			return
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}

			w.fail(err)
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

// fail reports err on the watch's error channel unless one is pending.
func (w *Watch) fail(err error) {
	select {
	case w.errs <- err:
	default: // one pending error is enough to re-watch
	}
}

// Events yields one value per (coalesced) notify.
func (w *Watch) Events() <-chan struct{} { return w.events }

// Errors yields watch errors (the OSD dropped the watch, or the store
// rotated its connection): the holder closes the watch and watches
// again.
func (w *Watch) Errors() <-chan error { return w.errs }

// Close removes the watch.
func (w *Watch) Close() {
	w.once.Do(func() {
		close(w.done)
		_ = w.w.Delete()

		if w.onClose != nil {
			w.onClose()
		}
	})
}
