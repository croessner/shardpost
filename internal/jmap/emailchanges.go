package jmap

import (
	"context"
	"encoding/json"
	"sort"

	"shardpost/internal/model"
)

// emailFate is what one window of the account log did to one message.
type emailFate struct {
	net     int  // placements added minus placements removed
	placed  bool // the window added or removed a placement
	touched bool // the window changed the message at all
}

// emailWindow folds a page of log entries into what happened to each
// message. A change the log records without listing its messages cannot
// be turned into a delta and is reported as such.
func emailWindow(entries []model.AccountChange) (map[string]*emailFate, *methodError) {
	fates := map[string]*emailFate{}

	fate := func(guid string) *emailFate {
		f := fates[guid]
		if f == nil {
			f = &emailFate{}
			fates[guid] = f
		}

		return f
	}

	for i := range entries {
		e := &entries[i]
		if e.Kind != model.KindEmail {
			continue
		}

		if e.Partial && len(e.Msgs) == 0 {
			return nil, &methodError{Type: errCannotCalculateChanges,
				Description: "a change was recorded without its messages"}
		}

		for _, m := range e.Msgs {
			f := fate(m.GUID)
			f.touched = true

			switch e.Op {
			case model.OpCreated:
				f.net++
				f.placed = true
			case model.OpDestroyed:
				f.net--
				f.placed = true
			}
		}
	}

	return fates, nil
}

// emailClass is the list a message belongs in.
type emailClass int

const (
	classNone emailClass = iota
	classCreated
	classUpdated
	classDestroyed
)

// classifyEmails sorts the messages of a window into created, updated
// and destroyed. A new placement is not a new message: an IMAP COPY of a
// message that already exists puts it into a second mailbox, which is a
// change of the message. So the placements are counted — the number there
// is now, checked against the mailboxes, less what the window added,
// gives the number there was before — and a message is created when it
// had none before, destroyed when it has none now.
//
// A window cut short by maxChanges is counted against the placements of
// now, which includes changes after the window. That can mislabel within
// the window — a creation reported as an update, an update left out when
// the message is gone by now — but it cannot report a message destroyed
// that exists, or leave out a message that exists and is new, and the
// pages that follow bring the client to the right end state.
func classifyEmails(loc *locator, fates map[string]*emailFate) (map[string]emailClass, error) {
	var placed []string

	for guid, f := range fates {
		if f.placed {
			placed = append(placed, guid)
		}
	}

	hits, err := loc.resolve(placed)
	if err != nil {
		return nil, err
	}

	out := make(map[string]emailClass, len(fates))

	for guid, f := range fates {
		if !f.placed {
			// Only its keywords changed; it existed when they did.
			out[guid] = classUpdated
			continue
		}

		now := 0
		if h, ok := hits[guid]; ok {
			now = len(h.metas)
		}

		out[guid] = classOf(max(now-f.net, 0), now)
	}

	return out, nil
}

// classOf is the class of a message from its placements before and now.
func classOf(before, now int) emailClass {
	switch {
	case before == 0 && now > 0:
		return classCreated
	case before > 0 && now == 0:
		return classDestroyed
	case before > 0:
		return classUpdated
	}

	return classNone
}

// emailChanges implements Email/changes (RFC 8621 4.3).
func emailChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	w, merr := readWindow(sess, args)
	if merr != nil {
		return nil, merr
	}

	out := w.response(sess)

	for guid, c := range w.classes {
		out.add(guid, c)
	}

	out.sorted()

	return out.changesResponse, nil
}

// window is one page of the account log, read for a /changes of the
// mail data types, with what it did to each message.
type window struct {
	loc        *locator
	fates      map[string]*emailFate
	classes    map[string]emailClass
	sinceState string
	last       uint64
	full       bool
}

// readWindow reads the page a /changes asks for and classifies what it
// did to each message.
func readWindow(sess *session, args json.RawMessage) (*window, *methodError) {
	var req changesRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	since, ok := parseState(req.SinceState)
	if !ok {
		return nil, &methodError{Type: errCannotCalculateChanges, Description: descUnknownState}
	}

	limit, merr := changeLimit(req.MaxChanges)
	if merr != nil {
		return nil, merr
	}

	entries, ok, err := sess.user.AccountChanges(since, limit)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	if !ok {
		return nil, &methodError{Type: errCannotCalculateChanges, Description: descLogTrimmed}
	}

	w := &window{sinceState: req.SinceState, last: since, full: len(entries) == limit}
	if len(entries) > 0 {
		w.last = entries[len(entries)-1].Seq
	}

	if w.fates, merr = emailWindow(entries); merr != nil {
		return nil, merr
	}

	_, w.loc, merr = emailState(sess)
	if merr != nil {
		return nil, merr
	}

	w.classes, err = classifyEmails(w.loc, w.fates)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	return w, nil
}

// changeLists is a /changes response being filled.
type changeLists struct{ *changesResponse }

// response starts the response of a window.
func (w *window) response(sess *session) changeLists {
	return changeLists{&changesResponse{
		AccountID: accountID(sess.account), OldState: w.sinceState, NewState: stateString(w.last),
		HasMoreChanges: w.full,
		Created:        []string{}, Updated: []string{}, Destroyed: []string{},
	}}
}

// add puts an id in the list of its class.
func (l changeLists) add(id string, c emailClass) {
	switch c {
	case classCreated:
		l.Created = append(l.Created, id)
	case classUpdated:
		l.Updated = append(l.Updated, id)
	case classDestroyed:
		l.Destroyed = append(l.Destroyed, id)
	case classNone:
	}
}

// sorted orders the three lists, so that a response does not depend on
// the order of a map.
func (l changeLists) sorted() {
	sort.Strings(l.Created)
	sort.Strings(l.Updated)
	sort.Strings(l.Destroyed)
}
