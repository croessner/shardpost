package jmap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"shardpost/internal/model"
)

// Push (RFC 8620 7): a client learns that something changed, not what.
// Both ways of telling it — the event source here and the push
// subscriptions a worker serves (pushworker.go) — compute the same
// StateChange from the account change log.

// The data types a StateChange names. Every type's state is the account
// counter, so the value is the same for all; which types appear says what
// kind of change happened.
const (
	typeMailbox       = "Mailbox"
	typeEmail         = "Email"
	typeThread        = "Thread"
	typeEmailDelivery = "EmailDelivery"
)

// pushTypes are the types this server pushes changes of.
var pushTypes = []string{typeMailbox, typeEmail, typeThread, typeEmailDelivery}

// stateChange is the StateChange object (RFC 8620 7.1).
type stateChange struct {
	Type    string                       `json:"@type"`
	Changed map[string]map[string]string `json:"changed"`
}

// typesOf are the types a page of log entries changed. A change to a
// message changes the message, its thread and the counts of its mailbox;
// a new message is also a delivery, which is as near as the log gets to
// RFC 8621 1.5's "new mail arrived" — an IMAP APPEND counts as well. A
// Partial entry says too little to narrow it down.
func typesOf(entries []model.AccountChange) map[string]bool {
	out := map[string]bool{}

	for i := range entries {
		e := &entries[i]

		switch {
		case e.Partial:
			for _, t := range pushTypes {
				out[t] = true
			}
		case e.Kind == model.KindMailbox:
			out[typeMailbox] = true
		case e.Kind == model.KindEmail:
			out[typeEmail], out[typeThread], out[typeMailbox] = true, true, true
			if e.Op == model.OpCreated {
				out[typeEmailDelivery] = true
			}
		}
	}

	return out
}

// pushPage is how many log entries one step of a push reads.
const pushPage = 1000

// changedTypes reads the log from since and returns the types that
// changed, filtered by want (nil: all), and the state to report: the Seq
// of the last entry read, by the reader rule of model/account.go. A log
// that no longer reaches back to since changed everything.
func changedTypes(u model.User, since uint64, want map[string]bool) (map[string]bool, uint64, error) {
	entries, ok, err := u.AccountChanges(since, pushPage)
	if err != nil {
		return nil, since, err
	}

	var types map[string]bool

	switch {
	case !ok:
		seq, err := u.AccountState()
		if err != nil {
			return nil, since, err
		}

		types = map[string]bool{}
		for _, t := range pushTypes {
			types[t] = true
		}

		return filterTypes(types, want), seq, nil
	case len(entries) == 0:
		return nil, since, nil
	}

	types = typesOf(entries)

	return filterTypes(types, want), entries[len(entries)-1].Seq, nil
}

// filterTypes keeps the types a listener asked for.
func filterTypes(types, want map[string]bool) map[string]bool {
	if want == nil {
		return types
	}

	out := map[string]bool{}

	for t := range types {
		if want[t] {
			out[t] = true
		}
	}

	return out
}

// newStateChange builds the StateChange of one account.
func newStateChange(account string, types map[string]bool, seq uint64) *stateChange {
	ts := map[string]string{}
	for t := range types {
		ts[t] = stateString(seq)
	}

	return &stateChange{Type: "StateChange", Changed: map[string]map[string]string{accountID(account): ts}}
}

// parseTypes reads a list of type names ("*" or empty: all).
func parseTypes(list []string) map[string]bool {
	if len(list) == 0 || (len(list) == 1 && list[0] == "*") {
		return nil
	}

	out := map[string]bool{}
	for _, t := range list {
		out[strings.TrimSpace(t)] = true
	}

	return out
}

// The bounds of the ping interval. RFC 8620 7.3 lets a server clamp it,
// but not to a minimum above 30 seconds or a maximum below 300.
const (
	minPing = 15
	maxPing = 900
)

// eventParams are the variables of the event source URL.
type eventParams struct {
	types      map[string]bool
	closeAfter bool
	ping       int
}

// readEventParams reads and checks the query of an event source request.
func readEventParams(r *http.Request) (*eventParams, error) {
	q := r.URL.Query()
	p := &eventParams{}

	types := q.Get("types")
	if types == "" {
		return nil, fmt.Errorf("types is required")
	}

	p.types = parseTypes(strings.Split(types, ","))

	switch q.Get("closeafter") {
	case "state":
		p.closeAfter = true
	case "no", "":
	default:
		return nil, fmt.Errorf("closeafter is state or no")
	}

	if v := q.Get("ping"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("ping is a number of seconds")
		}

		if n > 0 {
			n = min(max(n, minPing), maxPing)
		}

		p.ping = n
	}

	return p, nil
}

// handleEvents serves the event source (RFC 8620 7.3): a text/event-stream
// that carries a "state" event whenever the account changes, with the
// account state as its event id, and "ping" events in between. A client
// that reconnects with Last-Event-ID is sent what it missed at once.
//
// Changes are learnt from a watch on the account, which the store fires
// cluster-wide, and from a poll that covers a lost notification.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sess := sessionOf(r)

	p, err := readEventParams(r)
	if err != nil {
		writeProblem(w, http.StatusBadRequest, errURNNotRequest, err.Error(), "")

		return
	}

	if !s.enter(s.inEvents, sess.account, s.o.Limits.MaxConcurrentRequests) {
		writeProblem(w, http.StatusTooManyRequests, errURNLimit,
			"too many event sources open for this account", "maxConcurrentRequests")

		return
	}
	defer s.leave(s.inEvents, sess.account)

	last, err := sess.user.AccountState()
	if err != nil {
		writeProblem(w, http.StatusServiceUnavailable, errURNNotRequest, "account state unavailable", "")

		return
	}

	if id, perr := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64); perr == nil && id <= last {
		last = id
	}

	w.Header().Set(fieldContentType, "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	stream := &eventStream{w: w, rc: http.NewResponseController(w), sess: sess, p: p, last: last}
	stream.flush()

	s.runEvents(r, stream)
}

// eventStream is one open event source.
type eventStream struct {
	w    http.ResponseWriter
	rc   *http.ResponseController
	sess *session
	p    *eventParams
	ping *pingTimer
	last uint64
}

// pingTimer fires when the ping interval has passed since the last event.
type pingTimer struct {
	t        *time.Timer
	interval time.Duration
}

// newPingTimer starts a ping timer; an interval of 0 never fires.
func newPingTimer(seconds int) *pingTimer {
	if seconds <= 0 {
		return &pingTimer{}
	}

	d := time.Duration(seconds) * time.Second

	return &pingTimer{t: time.NewTimer(d), interval: d}
}

// c is the channel the timer fires on (nil when it never fires).
func (p *pingTimer) c() <-chan time.Time {
	if p == nil || p.t == nil {
		return nil
	}

	return p.t.C
}

// reset starts the interval again.
func (p *pingTimer) reset() {
	if p != nil && p.t != nil {
		p.t.Reset(p.interval)
	}
}

// stop stops the timer.
func (p *pingTimer) stop() {
	if p != nil && p.t != nil {
		p.t.Stop()
	}
}

// runEvents is the loop of an event source: it ends when the client
// goes, the server shuts down, a write fails, or closeafter=state was
// asked for and a state was sent.
func (s *Server) runEvents(r *http.Request, st *eventStream) {
	feed := newAccountFeed(st.sess.user)
	defer feed.Close()

	poll := time.NewTicker(s.o.Push.EventPoll)
	defer poll.Stop()

	// RFC 8620 7.3: a ping when the interval has passed since the last
	// event of any kind, so the timer starts again after every event.
	st.ping = newPingTimer(st.p.ping)
	defer st.ping.stop()

	// Whatever happened between Last-Event-ID and now is sent first.
	if done, ok := st.check(); !ok || done {
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			return
		case <-st.ping.c():
			if !st.write("ping", "", fmt.Sprintf(`{"interval":%d}`, st.p.ping)) {
				return
			}
		case <-feed.Events():
		case <-poll.C:
		}

		if done, ok := st.check(); !ok || done {
			return
		}
	}
}

// accountFeed turns the store's account watch into a channel that keeps
// working: a watch the store reports broken (an OSD restart drops RADOS
// watches) is closed and set up again, and its error channel is always
// read — a watch whose errors nobody reads blocks the store's callback
// thread, and with it every other watch of the process.
type accountFeed struct {
	events chan struct{}
	stop   chan struct{}
	done   chan struct{}
}

// rewatchDelay is how long the feed waits before it sets a broken watch
// up again.
const rewatchDelay = time.Second

// newAccountFeed starts the feed of an account. A store without account
// watches gives a feed that never fires; the poll covers that.
func newAccountFeed(u model.User) *accountFeed {
	f := &accountFeed{events: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}

	aw, ok := u.(model.AccountWatcher)
	if !ok {
		close(f.done)

		return f
	}

	go f.run(aw)

	return f
}

// run keeps a watch up until the feed is closed.
func (f *accountFeed) run(aw model.AccountWatcher) {
	defer close(f.done)

	for {
		watch, err := aw.WatchAccount()
		if err == nil {
			broken := f.follow(watch)
			watch.Close()

			if !broken {
				return
			}

			f.fire() // what the broken watch may have missed
		}

		select {
		case <-f.stop:
			return
		case <-time.After(rewatchDelay):
		}
	}
}

// follow forwards the events of one watch; it reports whether the watch
// broke (true) or the feed was closed (false).
func (f *accountFeed) follow(watch model.Watch) bool {
	for {
		select {
		case <-f.stop:
			return false
		case <-watch.Events():
			f.fire()
		case <-watch.Errors():
			return true
		}
	}
}

// fire signals a change, coalescing with one not yet taken.
func (f *accountFeed) fire() {
	select {
	case f.events <- struct{}{}:
	default:
	}
}

// Events fires when the account may have changed.
func (f *accountFeed) Events() <-chan struct{} { return f.events }

// Close stops the feed and waits for it.
func (f *accountFeed) Close() {
	select {
	case <-f.stop:
	default:
		close(f.stop)
	}

	<-f.done
}

// check sends a state event when the account has changed. done reports
// that the stream is to end after it (closeafter=state); ok is false when
// the client is gone.
func (st *eventStream) check() (done, ok bool) {
	for {
		types, seq, err := changedTypes(st.sess.user, st.last, st.p.types)
		if err != nil {
			st.sess.log.Warn("jmap: event source", "err", err)

			return false, true
		}

		if seq == st.last {
			return false, true
		}

		st.last = seq

		if len(types) == 0 {
			continue
		}

		data, _ := json.Marshal(newStateChange(st.sess.account, types, seq))
		if !st.write("state", stateString(seq), string(data)) {
			return false, false
		}

		if st.p.closeAfter {
			return true, true
		}
	}
}

// write sends one event and flushes it.
func (st *eventStream) write(event, id, data string) bool {
	var b strings.Builder

	b.WriteString("event: " + event + "\n")

	if id != "" {
		b.WriteString("id: " + id + "\n")
	}

	b.WriteString("data: " + data + "\n\n")

	if _, err := st.w.Write([]byte(b.String())); err != nil {
		return false
	}

	st.ping.reset()

	return st.flush()
}

// flush pushes what was written to the client.
func (st *eventStream) flush() bool { return st.rc.Flush() == nil }
