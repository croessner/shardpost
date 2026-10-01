package jmap

import (
	"shardpost/internal/mimepart"
)

// The hasAttachment condition of Email/query. Every message stored since
// the index record carries the field (model.Meta.HasAttachment) is
// answered from the record, computed at delivery, append, import or
// Email/set by the same code Email/get renders the property with. A
// record written before has none; for such a message the query reads and
// parses the message once and remembers the answer in the locator
// ("a:<guid>"), which is the derived index of the JMAP front-end anyway.
// A message's content never changes under its GUID, so the answer never
// goes stale. How many messages one query may read this way is bounded
// (Limits.MaxAttachmentReads): a query over an account full of old
// records answers serverUnavailable once the bound is reached, after it
// has stored what it learned, so that the next attempt gets further.

// locAttach is the locator key prefix of a remembered hasAttachment.
const locAttach = "a:"

// The values a remembered hasAttachment is written as.
const (
	attachYes = "1"
	attachNo  = "0"
)

// attachState is what one query knows about hasAttachment beyond the
// index records.
type attachState struct {
	known map[string]bool // remembered in the locator, or read here
	fresh map[string]bool // read here, to be remembered
	reads int
	// exhausted is set when a message would have had to be read past
	// the bound.
	exhausted bool
}

// attachments reads the remembered hasAttachment of messages.
func (l *locator) attachments(guids []string) (map[string]bool, error) {
	out := map[string]bool{}
	if len(guids) == 0 {
		return out, nil
	}

	keys := make([]string, len(guids))
	for i, g := range guids {
		keys[i] = locAttach + g
	}

	vals, err := l.ix.IndexGet(locSpace, keys)
	if err != nil {
		return nil, err
	}

	for k, v := range vals {
		out[k[len(locAttach):]] = string(v) == attachYes
	}

	return out, nil
}

// rememberAttachments writes the hasAttachment of messages that were
// read. A key that is already there is left alone: it says the same.
func (l *locator) rememberAttachments(vals map[string]bool) error {
	if len(vals) == 0 {
		return nil
	}

	keys := make([]string, 0, len(vals))
	for g := range vals {
		keys = append(keys, locAttach+g)
	}

	return l.ix.IndexApply(locSpace, keys, func(keys []string, cur map[string][]byte) (map[string][]byte, []string) {
		set := map[string][]byte{}

		for _, k := range keys {
			if _, ok := cur[k]; ok {
				continue
			}

			v := attachNo
			if vals[k[len(locAttach):]] {
				v = attachYes
			}

			set[k] = []byte(v)
		}

		return set, nil
	})
}

// prefetchAttachments reads what the locator remembers for the
// candidates whose index record does not say.
func (q *emailQuery) prefetchAttachments(items []*emailItem) error {
	var unknown []string

	for _, it := range items {
		if it.meta.HasAttachment == nil {
			unknown = append(unknown, it.guid)
		}
	}

	known, err := q.loc.attachments(unknown)
	if err != nil {
		return err
	}

	q.attach = &attachState{known: known, fresh: map[string]bool{}}

	return nil
}

// hasAttachment answers the hasAttachment of one candidate: from its
// index record, from the locator, or by reading the message.
func (q *emailQuery) hasAttachment(it *emailItem) bool {
	if v := it.meta.HasAttachment; v != nil {
		return *v
	}

	st := q.attach
	if st == nil {
		st = &attachState{known: map[string]bool{}, fresh: map[string]bool{}}
		q.attach = st
	}

	if v, ok := st.known[it.guid]; ok {
		return v
	}

	if st.reads >= q.sess.limits.MaxAttachmentReads {
		st.exhausted = true

		return false
	}

	st.reads++

	raw, err := q.sess.user.Read(it.meta)
	if err != nil {
		// A message whose data cannot be read has no body properties in
		// Email/get either; it is not a message with an attachment.
		q.sess.log.Warn("jmap: message body unreadable", "guid", it.guid, "err", err)

		return false
	}

	v := mimepart.MessageHasAttachment(raw)
	st.known[it.guid], st.fresh[it.guid] = v, v

	return v
}

// finishAttachments remembers what the query read, and answers
// serverUnavailable when it had to stop reading.
func (q *emailQuery) finishAttachments() *methodError {
	if q.attach == nil {
		return nil
	}

	if err := q.loc.rememberAttachments(q.attach.fresh); err != nil {
		// Only a cache: the next query reads the messages again.
		q.sess.log.Warn("jmap: hasAttachment not remembered", "err", err)
	}

	if q.attach.exhausted {
		return &methodError{Type: errServerUnavailable,
			Description: "hasAttachment needs more messages read than one query may read; try again"}
	}

	return nil
}
