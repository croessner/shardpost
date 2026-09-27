package jmap

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"

	"shardpost/internal/model"
	"shardpost/internal/sharenotice"
)

// Push for the accounts other namespaces share with the user (RFC 8620
// 7.1: a StateChange names every account the user has access to in which
// something changed).
//
// A follower (sharedPush) keeps the shared accounts of one user — the
// ones its session resource lists, with the same bounds — and for each
// of them where the owner's log stands as the user sees it: the cursor
// of sharedstate.go, the same one the method responses advance, so a
// pushed state is the one a /get answers. The owner's account object is
// watched through a watchHub, which holds one watch per namespace
// however many followers want it; a follower holds at most a configured
// number of watches, and the accounts beyond them, and any whose watch
// cannot be set up, are checked when the follower polls.
//
// A change in a mailbox the user cannot see wakes the follower, which
// reads the log and finds nothing it may report; the state stays, and
// nothing is sent.

// typeShareNotification is the type of the share notifications of the
// personal account (RFC 9670 3).
const typeShareNotification = "ShareNotification"

// stateRevoked is the state a StateChange names for every type of a
// shared account the user lost: no /get returns it, so a client that
// compares asks for changes, is answered accountNotFound, and fetches
// the session resource again (RFC 8620 2).
const stateRevoked = "revoked"

// watchHub shares the watches on account objects between followers: one
// store view and one watch per namespace, and a bound on how many
// namespaces it watches at once (0: none).
type watchHub struct {
	store Store
	max   int
	log   *slog.Logger

	mu      sync.Mutex
	watches map[string]*hubWatch
	next    int
}

// hubWatch is the watch on one namespace and the followers that want it.
type hubWatch struct {
	u    model.User
	feed *accountFeed
	subs map[int]func()
}

// newWatchHub makes a hub over a store.
func newWatchHub(store Store, maxWatches int, log *slog.Logger) *watchHub {
	return &watchHub{store: store, max: maxWatches, log: log, watches: map[string]*hubWatch{}}
}

// subscribe asks for a callback whenever the account object of a
// namespace changes. It returns the release function, or nil when the
// hub is at its bound or the namespace cannot be opened: the caller
// polls that account instead. fn must not block.
func (h *watchHub) subscribe(ns string, fn func()) func() {
	if h == nil {
		return nil
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	w := h.watches[ns]
	if w == nil {
		if h.max > 0 && len(h.watches) >= h.max {
			return nil
		}

		u, err := h.store.User(ns)
		if err != nil {
			h.log.Warn("jmap push: shared account not watched", "shared", ns, "err", err)

			return nil
		}

		w = &hubWatch{u: u, subs: map[int]func(){}}
		w.feed = newNotifyingFeed(u, func() { h.fire(ns) })
		h.watches[ns] = w
	}

	h.next++
	id := h.next
	w.subs[id] = fn

	return func() { h.release(ns, id) }
}

// fire calls the followers of a namespace.
func (h *watchHub) fire(ns string) {
	h.mu.Lock()

	var fns []func()

	if w := h.watches[ns]; w != nil {
		for _, fn := range w.subs {
			fns = append(fns, fn)
		}
	}

	h.mu.Unlock()

	for _, fn := range fns {
		fn()
	}
}

// release drops one follower, and the watch with the last one.
func (h *watchHub) release(ns string, id int) {
	h.mu.Lock()

	w := h.watches[ns]
	if w == nil {
		h.mu.Unlock()

		return
	}

	delete(w.subs, id)

	if len(w.subs) > 0 {
		h.mu.Unlock()

		return
	}

	delete(h.watches, ns)
	h.mu.Unlock()

	// Outside the lock: closing waits for the feed, whose callback takes
	// the lock.
	w.feed.Close()
	w.u.Close()
}

// size is how many namespaces the hub watches.
func (h *watchHub) size() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.watches)
}

// sharedTrack is one shared account a follower keeps.
type sharedTrack struct {
	a   *account
	cur sharedCursor
	// saved is the Scanned of the cursor as last stored.
	saved uint64
	// sent is the state last reported per type, fps the fingerprints of
	// the two kinds of token it was built with.
	sent map[string]string
	fps  [2]string
	// release gives the watch back; nil when the account is polled.
	release func()
}

// sharedPush follows the shared accounts and the share notifications of
// one user.
type sharedPush struct {
	srv  *Server
	sess *session
	hub  *watchHub
	// maxWatches is how many watches the follower holds at most.
	maxWatches int

	fp      string
	accts   map[string]*sharedTrack
	notices string

	mu    sync.Mutex
	dirty map[string]bool
	wake  chan struct{}
}

// newSharedPush starts a follower for a session. It reads what the user
// has access to now, which is what later changes are measured against.
func newSharedPush(srv *Server, sess *session, hub *watchHub, maxWatches int) *sharedPush {
	sp := &sharedPush{srv: srv, sess: sess, hub: hub, maxWatches: maxWatches,
		accts: map[string]*sharedTrack{}, dirty: map[string]bool{}, wake: make(chan struct{}, 1)}

	sp.rebuild(false)
	sp.notices, _ = sp.noticeState()

	return sp
}

// Wake fires when a watched shared account may have changed.
func (sp *sharedPush) Wake() <-chan struct{} {
	if sp == nil {
		return nil
	}

	return sp.wake
}

// close gives every watch back.
func (sp *sharedPush) close() {
	if sp == nil {
		return
	}

	for id, t := range sp.accts {
		if t.release != nil {
			t.release()
		}

		delete(sp.accts, id)
	}
}

// sharingOn reports whether any kind of shared account is offered.
func (sp *sharedPush) sharingOn() bool {
	sh := sp.srv.o.Sharing

	return sh.SharedPrefix != "" || sh.PublicPrefix != ""
}

// pushFingerprint is what the set of shared accounts and the
// subscriptions in them are made of: the sharing fingerprint of the
// session state, and the user's subscriptions to shared and public
// mailboxes, which a shared account's Mailbox state carries and which
// change in the user's own registry, not in any log a follower watches.
func (sp *sharedPush) pushFingerprint() string {
	if !sp.sharingOn() {
		return ""
	}

	h := sha256.New()
	_, _ = h.Write([]byte(sp.srv.sharingFingerprint(sp.sess) + "\n"))

	if sp.srv.o.Sharing.SharedPrefix != "" {
		refs, _, err := model.ListSharedN(sp.sess.own, "", sp.srv.sessionPointers())
		if err != nil {
			_, _ = h.Write([]byte("unreadable\n"))
		}

		for _, r := range refs {
			if r.Subscribed {
				_, _ = h.Write([]byte(r.Owner + "/" + r.Mailbox + "\n"))
			}
		}
	}

	if sp.srv.o.Sharing.PublicPrefix != "" {
		subs, err := sp.sess.own.PublicSubscriptions()
		if err != nil {
			_, _ = h.Write([]byte("public:unreadable\n"))
		}

		for _, id := range sortedKeys(subs) {
			if subs[id] {
				_, _ = h.Write([]byte("public/" + id + "\n"))
			}
		}
	}

	return hex.EncodeToString(h.Sum(nil)[:8])
}

// rebuild reads the shared accounts again, as the session resource does,
// and returns the StateChange entries of the accounts that appeared and
// went (report false: none, for the first read).
func (sp *sharedPush) rebuild(report bool) map[string]map[string]string {
	sp.fp = sp.pushFingerprint()

	out := map[string]map[string]string{}

	if !sp.sharingOn() {
		return out
	}

	now := map[string]*account{}
	for _, a := range sp.srv.sharedAccounts(sp.sess) {
		now[a.id] = a
	}

	for id, t := range sp.accts {
		if _, ok := now[id]; ok {
			continue
		}

		sp.drop(id, t)

		if report {
			out[id] = revokedStates()
		}
	}

	for _, id := range sortedAccountIDs(now) {
		t := sp.accts[id]
		if t == nil {
			t = &sharedTrack{sent: map[string]string{}}
			sp.accts[id] = t
			t.a = now[id]

			if err := sp.adopt(t); err != nil {
				sp.sess.log.Warn("jmap push: shared account not followed", "shared", id, "err", err)
				delete(sp.accts, id)

				continue
			}

			sp.watch(id, t)
			states := t.states()

			if report {
				out[id] = states
			}

			t.remember(states)

			continue
		}

		t.a = now[id]
		if changed := sp.advance(id, t); len(changed) > 0 {
			out[id] = changed
		}
	}

	return out
}

// drop stops following an account.
func (sp *sharedPush) drop(id string, t *sharedTrack) {
	if t.release != nil {
		t.release()
	}

	delete(sp.accts, id)
}

// sortedAccountIDs are the ids of a set of accounts in order: the owners
// by name, the public account last, as the session resource lists them.
func sortedAccountIDs(m map[string]*account) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}

	sort.Slice(ids, func(i, j int) bool {
		pi, pj := ids[i] == model.PublicNamespace, ids[j] == model.PublicNamespace
		if pi != pj {
			return pj
		}

		return ids[i] < ids[j]
	})

	return ids
}

// revokedStates are the states named for an account the user lost.
func revokedStates() map[string]string {
	out := map[string]string{}
	for _, t := range pushTypes {
		out[t] = stateRevoked
	}

	return out
}

// watch sets a watch up for a track while the follower has one left.
func (sp *sharedPush) watch(id string, t *sharedTrack) {
	held := 0

	for _, o := range sp.accts {
		if o.release != nil {
			held++
		}
	}

	if held >= sp.maxWatches {
		sp.srv.logOnce("pushwatch:"+sp.sess.account, "jmap push: shared accounts beyond jmap.push.max_shared_watches are polled",
			"account", sp.sess.account, "limit", sp.maxWatches)

		return
	}

	t.release = sp.hub.subscribe(t.a.owner, func() { sp.mark(id) })
}

// mark records that a watched account may have changed.
func (sp *sharedPush) mark(id string) {
	sp.mu.Lock()
	sp.dirty[id] = true
	sp.mu.Unlock()

	select {
	case sp.wake <- struct{}{}:
	default:
	}
}

// takeDirty returns the accounts marked since the last call.
func (sp *sharedPush) takeDirty() map[string]bool {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	out := sp.dirty
	sp.dirty = map[string]bool{}

	return out
}

// adopt starts a track from the stored cursor, as a method response
// would read it.
func (t *sharedTrack) adoptFrom(a *account) error {
	c, err := a.storedCursor()
	if err != nil {
		return err
	}

	t.cur, t.saved = c, c.Scanned

	return nil
}

// adopt is adoptFrom for a follower's track.
func (sp *sharedPush) adopt(t *sharedTrack) error { return t.adoptFrom(t.a) }

// states are the current states of every pushed type of the account.
func (t *sharedTrack) states() map[string]string {
	out := map[string]string{}

	for _, typ := range pushTypes {
		out[typ] = t.a.state(typ != typeMailbox, t.cur.LV)
	}

	return out
}

// remember records what was reported.
func (t *sharedTrack) remember(states map[string]string) {
	for typ, s := range states {
		t.sent[typ] = s
	}

	t.fps = [2]string{t.a.fingerprint(stateMailboxes), t.a.fingerprint(stateEmails)}
}

// advance reads the owner's log for one account and returns the types
// whose state moved, with the new states: the types the visible entries
// changed, and those whose token names another visible set. A cursor
// that had to start again takes the stored one, which a method response
// would answer as well, and names every type.
func (sp *sharedPush) advance(id string, t *sharedTrack) map[string]string {
	types, restarted, err := advanceCursor(t.a, &t.cur, t.a.load)
	if err != nil {
		sp.sess.log.Warn("jmap push: shared account not read", "shared", id, "err", err)

		return nil
	}

	if restarted {
		if err := t.adoptFrom(t.a); err != nil {
			sp.sess.log.Warn("jmap push: shared account not read", "shared", id, "err", err)

			return nil
		}
	} else if t.cur.Scanned-t.saved >= cursorSaveStride {
		t.save()
	}

	fps := [2]string{t.a.fingerprint(stateMailboxes), t.a.fingerprint(stateEmails)}
	if fps[0] != t.fps[0] {
		types[typeMailbox] = true
	}

	if fps[1] != t.fps[1] {
		types[typeEmail], types[typeThread], types[typeMailbox] = true, true, true
	}

	states := t.states()
	out := map[string]string{}

	for typ := range types {
		if s, ok := states[typ]; ok && s != t.sent[typ] {
			out[typ] = s
		}
	}

	t.remember(out)
	t.fps = fps

	return out
}

// save stores the follower's cursor, over the stored one when that is
// behind it: a follower that is live keeps the stored cursor near the
// head, so a method response has little to read.
func (t *sharedTrack) save() {
	was, raw, md, err := t.a.loadCursor()
	if err != nil || was.Key != t.cur.Key || was.Scanned >= t.cur.Scanned {
		t.saved = t.cur.Scanned

		return
	}

	if t.a.saveCursor(t.cur, was, raw, md) {
		t.saved = t.cur.Scanned
	}
}

// noticeState is the state of the user's share notifications, "" when
// it has none.
func (sp *sharedPush) noticeState() (string, error) {
	seq, err := sharenotice.State(sp.sess.ownView(), sp.sess.account)
	if err != nil || seq == 0 {
		return "", err
	}

	return stateString(seq), nil
}

// check finds what changed: in the accounts that were marked, in every
// account when full (a poll), and in the set of accounts and the share
// notifications when full or when the set may have changed. It returns
// the StateChange entries by account id.
func (sp *sharedPush) check(full bool) map[string]map[string]string {
	dirty := sp.takeDirty()

	var out map[string]map[string]string

	if full && sp.sharingOn() && sp.pushFingerprint() != sp.fp {
		out = sp.rebuild(true)
	} else {
		out = sp.checkAccounts(full, dirty)
	}

	if full {
		sp.checkNotices(out)
	}

	return out
}

// checkAccounts reads the accounts that are due: every one when full, the
// watched ones that fired otherwise.
func (sp *sharedPush) checkAccounts(full bool, dirty map[string]bool) map[string]map[string]string {
	out := map[string]map[string]string{}

	for _, id := range sortedTrackIDs(sp.accts) {
		// A polled account is read on the poll, a watched one when its
		// watch fired as well.
		if !full && !dirty[id] {
			continue
		}

		t := sp.accts[id]
		if changed := sp.advance(id, t); len(changed) > 0 {
			out[id] = changed
		}

		// A reload that left no mailbox the user can see: the account
		// is gone, as the session resource will say.
		if len(t.a.boxes) == 0 {
			sp.drop(id, t)
			out[id] = revokedStates()
		}
	}

	return out
}

// checkNotices adds the share notifications to out when their state
// moved.
func (sp *sharedPush) checkNotices(out map[string]map[string]string) {
	n, err := sp.noticeState()
	if err != nil || n == sp.notices {
		return
	}

	sp.notices = n
	if n == "" {
		n = stateString(0)
	}

	out[accountID(sp.sess.account)] = map[string]string{typeShareNotification: n}
}

// sortedTrackIDs are the ids of the tracks in order.
func sortedTrackIDs(m map[string]*sharedTrack) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}

	sort.Strings(ids)

	return ids
}

// all is the current state of everything the follower keeps, for a
// client that reconnects without knowing it.
func (sp *sharedPush) all() map[string]map[string]string {
	out := map[string]map[string]string{}

	for id, t := range sp.accts {
		out[id] = t.states()
	}

	if sp.notices != "" {
		out[accountID(sp.sess.account)] = map[string]string{typeShareNotification: sp.notices}
	}

	return out
}

// digest summarises the states of all() for the event id: it changes
// whenever one of them does. "" when there is nothing to summarise.
func (sp *sharedPush) digest() string {
	if sp == nil || (len(sp.accts) == 0 && sp.notices == "") {
		return ""
	}

	h := sha256.New()

	for _, id := range sortedTrackIDs(sp.accts) {
		t := sp.accts[id]
		_, _ = h.Write([]byte(id + ":" + t.a.state(stateMailboxes, t.cur.LV) + ":" + t.a.state(stateEmails, t.cur.LV) + "\n"))
	}

	_, _ = h.Write([]byte("notices:" + sp.notices + "\n"))

	return hex.EncodeToString(h.Sum(nil)[:6])
}

// pushStateSep separates the personal state from the digest of the rest
// in an event id.
const pushStateSep = "."

// pushState is the event id (RFC 8620 7.3: it encodes all the state the
// user sees): the personal account's state, and the digest of the shared
// accounts and the share notifications when there are any.
func pushState(seq uint64, digest string) string {
	if digest == "" {
		return stateString(seq)
	}

	return stateString(seq) + pushStateSep + digest
}

// parsePushState reads an event id back.
func parsePushState(s string) (seq uint64, digest string, ok bool) {
	head, digest, _ := strings.Cut(s, pushStateSep)
	seq, err := strconv.ParseUint(head, 10, 64)

	return seq, digest, err == nil
}
