package jmap

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"shardpost/internal/attcache"
	"shardpost/internal/model"
)

// The hasAttachment condition of Email/query. Every message stored since
// the index record carries the field (model.Meta.HasAttachment) is
// answered from the record, computed at delivery, append, import or
// Email/set by the same code Email/get renders the property with. A
// record written before has none; for such a message the answer is
// looked up in the locator (attcache, "a:<guid>"), or the message is read
// once and the answer remembered there.
//
// The filter is evaluated in three-valued logic: first with what the
// records say, so that only the messages whose match still depends on an
// unknown hasAttachment are looked up, in batches, and read. How many
// messages one query may read is bounded (Limits.MaxAttachmentReads); a
// query that reaches the bound stores what it learned, starts a
// background fill of the namespace (Server.backfill) and answers
// serverUnavailable, so that a later attempt finds the answers.

// attachState is what one query learned about hasAttachment beyond the
// index records.
type attachState struct {
	known map[string]attcache.Value // remembered, or read here
	fresh map[string]attcache.Value // read here, to be remembered
	reads int
	// exhausted is set when a message would have had to be read past
	// the bound.
	exhausted bool
}

// match filters the candidates, in their order.
func (q *emailQuery) match(items []*emailItem) ([]*emailItem, *methodError) {
	if !q.needAttach {
		matched := make([]*emailItem, 0, len(items))

		for _, it := range items {
			if q.filter.eval(it, nil) {
				matched = append(matched, it)
			}
		}

		return matched, nil
	}

	return q.matchAttach(items)
}

// matchAttach filters candidates with a hasAttachment condition: once
// with what is known, then the undecided ones in batches.
func (q *emailQuery) matchAttach(items []*emailItem) ([]*emailItem, *methodError) {
	q.attach = &attachState{known: map[string]attcache.Value{}, fresh: map[string]attcache.Value{}}

	verdict := make([]tri, len(items))

	var pending []int

	for i, it := range items {
		if verdict[i] = q.filter.eval3(it, q.knownAttach); verdict[i] == triUnknown {
			pending = append(pending, i)
		}
	}

	for start := 0; start < len(pending) && !q.attach.exhausted; start += attcache.Batch {
		batch := pending[start:min(start+attcache.Batch, len(pending))]
		if err := q.resolveAttachments(items, batch); err != nil {
			return nil, &methodError{Type: errServerFail, Description: err.Error()}
		}

		for _, i := range batch {
			verdict[i] = q.filter.eval3(items[i], q.knownAttach)
		}
	}

	if merr := q.finishAttachments(); merr != nil {
		return nil, merr
	}

	matched := make([]*emailItem, 0, len(items))

	for i, it := range items {
		if verdict[i] == triTrue {
			matched = append(matched, it)
		}
	}

	return matched, nil
}

// knownAttach answers hasAttachment from the index record or from what
// the query learned.
func (q *emailQuery) knownAttach(it *emailItem) (has, known bool) {
	if v := it.meta.HasAttachment; v != nil {
		return *v, true
	}

	if v, ok := q.attach.known[it.guid]; ok {
		return v.Has(), true
	}

	return false, false
}

// resolveAttachments looks one batch of undecided messages up in the
// locator and reads the ones it does not know, up to the bound.
func (q *emailQuery) resolveAttachments(items []*emailItem, batch []int) error {
	ix, ok := q.sess.user.(model.Index)
	if !ok {
		return errNoIndex
	}

	guids := make([]string, 0, len(batch))
	for _, i := range batch {
		guids = append(guids, items[i].guid)
	}

	known, err := attcache.Lookup(ix, guids)
	if err != nil {
		return err
	}

	st := q.attach

	for g, v := range known {
		st.known[g] = v
	}

	for _, i := range batch {
		it := items[i]
		if _, ok := st.known[it.guid]; ok {
			continue
		}

		if st.reads >= q.sess.limits.MaxAttachmentReads {
			st.exhausted = true

			return nil
		}

		st.known[it.guid] = q.readAttachment(it.meta)
	}

	return nil
}

// readAttachment reads one message for its hasAttachment. A message that
// cannot be read counts as one without: Email/get has no body properties
// for it either. It is not remembered in the locator, since the failure
// may pass, but the server does not read it again for a while.
func (q *emailQuery) readAttachment(m *model.Meta) attcache.Value {
	srv := q.sess.srv
	if srv != nil && srv.unreadable.recent(m.GUID) {
		return attcache.No
	}

	q.attach.reads++

	v, err := attcache.Compute(q.sess.user, m)
	if err != nil {
		if srv != nil {
			srv.unreadable.note(q.sess.log, m.GUID, err)
		}

		return attcache.No
	}

	q.attach.fresh[m.GUID] = v

	return v
}

// finishAttachments remembers what the query read, and answers
// serverUnavailable when it had to stop reading.
func (q *emailQuery) finishAttachments() *methodError {
	if ix, ok := q.sess.user.(model.Index); ok {
		if err := attcache.Remember(ix, q.attach.fresh); err != nil {
			// Only a cache: the next query reads the messages again.
			q.sess.log.Warn("jmap: hasAttachment not remembered", "err", err)
		}
	}

	if !q.attach.exhausted {
		return nil
	}

	if q.sess.srv != nil {
		q.sess.srv.backfill(q.sess.current().owner)
	}

	return &methodError{Type: errServerUnavailable,
		Description: "hasAttachment needs older messages read; they are being read now, try again shortly"}
}

// unreadableTTL is how long a message that could not be read is not read
// again for hasAttachment; maxUnreadable bounds how many are kept.
const (
	unreadableTTL = 10 * time.Minute
	maxUnreadable = 10000
)

// unreadableSet remembers, per process, the messages a query could not
// read, so that every query does not read and log them again.
type unreadableSet struct {
	mu   sync.Mutex
	seen map[string]time.Time // guid -> when it failed
	once map[string]bool      // logged at warning level already
	now  func() time.Time     // the clock; nil is time.Now
}

// clock is the time now.
func (u *unreadableSet) clock() time.Time {
	if u.now != nil {
		return u.now()
	}

	return time.Now()
}

// recent reports whether the message failed within the TTL.
func (u *unreadableSet) recent(guid string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()

	at, ok := u.seen[guid]

	return ok && u.clock().Sub(at) < unreadableTTL
}

// note records a failure; the first one of a message is a warning, the
// ones after it are debug.
func (u *unreadableSet) note(log *slog.Logger, guid string, err error) {
	u.mu.Lock()

	if u.seen == nil || len(u.seen) >= maxUnreadable {
		u.seen, u.once = map[string]time.Time{}, map[string]bool{}
	}

	u.seen[guid] = u.clock()
	first := !u.once[guid]
	u.once[guid] = true

	u.mu.Unlock()

	if first {
		log.Warn("jmap: message body unreadable", "guid", guid, "err", err)
	} else {
		log.Debug("jmap: message body still unreadable", "guid", guid, "err", err)
	}
}

// backfill fills the remembered hasAttachment of a namespace in the
// background: one fill per namespace at a time, at most
// Limits.AttachmentBackfills at once in the process, each for at most
// Limits.AttachmentBackfillTime, and all of them stopped when the server
// shuts down. A namespace that finds no free slot is filled when a later
// query asks again.
func (s *Server) backfill(namespace string) {
	if _, running := s.fills.LoadOrStore(namespace, struct{}{}); running {
		return
	}

	select {
	case s.fillSlots <- struct{}{}:
	default:
		s.fills.Delete(namespace)

		return
	}

	s.fillMu.Lock()
	defer s.fillMu.Unlock()

	if s.fillCtx.Err() != nil { // shutting down
		<-s.fillSlots
		s.fills.Delete(namespace)

		return
	}

	s.fillWG.Add(1)

	go func() {
		defer s.fillWG.Done()
		defer s.fills.Delete(namespace)
		defer func() { <-s.fillSlots }()
		defer func() {
			if r := recover(); r != nil {
				s.o.Log.Error("jmap: hasAttachment fill failed", "namespace", namespace, "panic", r)
			}
		}()

		s.runBackfill(namespace)
	}()
}

// runBackfill is one background fill.
func (s *Server) runBackfill(namespace string) {
	ctx, cancel := context.WithTimeout(s.fillCtx, s.o.Limits.AttachmentBackfillTime)
	defer cancel()

	u, err := s.o.Store.User(namespace)
	if err != nil {
		s.o.Log.Warn("jmap: hasAttachment fill not started", "namespace", namespace, "err", err)

		return
	}
	defer u.Close()

	start := time.Now()
	rep, err := attcache.Backfill(ctx, u, nil, nil)

	s.o.Log.Info("jmap: hasAttachment filled", "namespace", namespace, "messages", rep.Messages,
		"known", rep.Known, "computed", rep.Computed, "unreadable", rep.Unreadable, "failed", rep.Failed,
		"skipped_mailboxes", rep.SkippedMailboxes, "errors", rep.Errors,
		"duration_ms", time.Since(start).Milliseconds(), "err", err)
}

// rememberLearned writes what an Email/get parsed of older records to the
// locator, so that the next call does not parse them again.
func rememberLearned(sess *session, learned map[string]attcache.Value) {
	if len(learned) == 0 {
		return
	}

	ix, ok := sess.user.(model.Index)
	if !ok {
		return
	}

	if err := attcache.Remember(ix, learned); err != nil {
		// Only a cache: the next call parses the messages again.
		sess.log.Warn("jmap: hasAttachment not remembered", "err", err)
	}
}

// attachmentsOf looks up the remembered hasAttachment of the messages of
// an Email/get whose index record does not carry it, when the call asks
// for the property.
func attachmentsOf(sess *session, hits map[string]*found, props []string) map[string]attcache.Value {
	ix, ok := sess.user.(model.Index)
	if !ok || !containsString(props, propHasAttachment) {
		return nil
	}

	var guids []string

	for _, f := range hits {
		if f.metas[0].HasAttachment == nil {
			guids = append(guids, f.metas[0].GUID)
		}
	}

	if len(guids) == 0 {
		return nil
	}

	known, err := attcache.Lookup(ix, guids)
	if err != nil {
		// Only a cache: the messages are parsed instead.
		sess.log.Warn("jmap: hasAttachment lookup failed", "err", err)

		return nil
	}

	return known
}
