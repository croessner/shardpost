package imapproto

import (
	"sync"

	"shardpost/internal/metrics"
	"shardpost/internal/model"
)

// Watches of a session go through the process-wide watch hub (scaling
// plan B12): IDLE follows the mailbox object of the selected mailbox,
// NOTIFY the account object of the account (and, for a selected mailbox
// of another namespace, the account object of its owner) and reads what
// changed from the account change log. However many sessions follow an
// object, the process holds one watch on it. A connection holds at most
// Options.MaxWatches subscriptions; beyond that, and when the hub cannot
// set a watch up, it polls as it did without a watch.

// defaultMaxWatches is the subscription bound of a connection: IDLE
// takes one (always kept free for it), NOTIFY one or two, or one per
// notified mailbox without an account watch.
const defaultMaxWatches = 8

// selectedNS is the namespace of the selected mailbox.
func (c *conn) selectedNS() string {
	switch {
	case c.mpublic:
		return model.PublicNamespace
	case c.mowner != "":
		return c.mowner
	default:
		return c.name
	}
}

// watchMailbox follows the mailbox object of a mailbox in namespace ns
// for NOTIFY (notify) or IDLE; an error means polling only
// (errWatchLimit: the connection is at its bound).
func (c *conn) watchMailbox(ns, id string, notify bool) (model.Watch, error) {
	return c.subscribe(notify, func() (model.Watch, error) { return c.s.hub.Mailbox(ns, id) })
}

// watchAccount follows the account object of namespace ns for NOTIFY;
// an error means the caller has to do without.
func (c *conn) watchAccount(ns string) (model.Watch, error) {
	return c.subscribe(true, func() (model.Watch, error) { return c.s.hub.Account(ns) })
}

// notifyWatchLimit is how many watches NOTIFY may take: one of the
// connection's bound stays reserved for IDLE.
func (c *conn) notifyWatchLimit() int { return max(c.s.o.MaxWatches-1, 0) }

// errWatchLimit refuses a subscription beyond Options.MaxWatches.
type errWatchLimit struct{}

func (errWatchLimit) Error() string { return "the connection holds as many watches as it may" }

// subscribe takes one of the connection's subscriptions; NOTIFY leaves
// one for IDLE.
func (c *conn) subscribe(notify bool, sub func() (model.Watch, error)) (model.Watch, error) {
	limit, what := c.s.o.MaxWatches, "idle"
	if notify {
		limit, what = c.notifyWatchLimit(), "notify"
	}

	if int(c.watches.Add(1))-c.watchCredit > limit {
		c.watches.Add(-1)
		metrics.IMAPWatchRefused.Inc()
		c.log.Warn("watch refused: the connection is at its watch limit (NOTIFY leaves one for IDLE); "+
			"NOTIFY SET answers NO [LIMIT] when its mailbox watches do not fit, otherwise the session polls",
			"for", what, "limit", limit)

		return nil, errWatchLimit{}
	}

	w, err := sub()
	if err != nil {
		c.watches.Add(-1)
		c.log.Warn("watch failed, polling only", "err", err)

		return nil, err
	}

	return &connWatch{Watch: w, c: c}, nil
}

// connWatch gives the connection's subscription back on Close.
type connWatch struct {
	model.Watch
	c    *conn
	once sync.Once
}

func (w *connWatch) Close() {
	w.once.Do(func() {
		w.Watch.Close()
		w.c.watches.Add(-1)
	})
}
