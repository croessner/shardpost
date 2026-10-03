package cephstore

import (
	"sync"
	"time"

	"github.com/ceph/go-ceph/rados"

	"shardpost/internal/model"
	"shardpost/internal/sortedindex"
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
func (u *User) watchObj(oid string) (*Watch, error) { return u.watchObjData(oid, nil) }

// watchObjData is watchObj that hands the data of every notify to data
// when it is set (a payload watch) instead of firing the event channel.
func (u *User) watchObjData(oid string, data *sortedindex.PayloadPump) (*Watch, error) {
	rw, err := u.idx.Watch(oid)
	if err != nil {
		return nil, err
	}

	u.inflight.Add(1)

	w := &Watch{w: rw, unwatch: rw.Delete, events: make(chan struct{}, 1), errs: make(chan error, 1), done: make(chan struct{}), data: data}
	go w.pump(u)

	return w, nil
}

// Watch is a live subscription to changes of one object.
type Watch struct {
	w *rados.Watcher
	// unwatch removes the watch (w.Delete; tests replace it).
	unwatch func() error
	events  chan struct{}
	// errs carries the watch errors, coalesced into one pending error,
	// so that the librados callback thread that reports them never
	// blocks on a reader that does not read.
	errs chan error
	done chan struct{}
	once sync.Once
	// onClose (if set) runs once when the watch is closed.
	onClose func()
	// data, when set, takes the data of every notify (a payload watch,
	// see payloadWatch); the event channel is not fired then.
	data *sortedindex.PayloadPump
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

			if w.data != nil {
				w.data.Push(ev.Data)

				continue
			}

			select {
			case w.events <- struct{}{}:
			default: // coalesce
			}
		}
	}
}

// payloadWatch is a Watch that delivers the data of every notify
// (model.PayloadWatch).
type payloadWatch struct{ *Watch }

var _ model.PayloadWatch = payloadWatch{}

// Events yields the data of every notify (an empty one after an
// overflow, see sortedindex.PayloadPump).
func (w payloadWatch) Events() <-chan []byte { return w.data.Events() }

// Close removes the watch and ends the pump of its data.
func (w payloadWatch) Close() {
	w.Watch.Close()
	w.data.Close()
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
//
// The unwatch comes first, while the pump still runs: the pump holds an
// inflight reference on the view, and User.Close destroys the view's
// IOContext only after inflight drained. Ending the pump first would let
// a closing view (after a key rotation, or a session that closes its
// account) destroy the IOContext concurrently with rados_unwatch2. The
// pump ends when the unwatch closes the event channel, or at done when
// the unwatch failed and left it open.
func (w *Watch) Close() {
	w.once.Do(func() {
		_ = w.unwatch()

		close(w.done)

		if w.onClose != nil {
			w.onClose()
		}
	})
}
