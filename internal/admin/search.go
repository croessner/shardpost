package admin

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"shardpost/internal/adminapi"
	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// Bounded message search (roadmap 8d). Criteria values are personal data
// and are not logged; the audit line names only the criteria used.

// Search bounds.
const (
	defaultSearchPage      = 50
	maxSearchPage          = 200
	defaultSearchScanLimit = 50000
)

// searchCursor is where a search continues: mailbox id and last UID.
type searchCursor struct {
	mailbox string
	uid     uint32
}

func parseCursor(s string) (searchCursor, bool) {
	if s == "" {
		return searchCursor{}, true
	}

	id, uid, ok := strings.Cut(s, ":")
	n, err := strconv.ParseUint(uid, 10, 32)

	return searchCursor{mailbox: id, uid: uint32(n)}, ok && err == nil && id != ""
}

func (c searchCursor) String() string {
	return c.mailbox + ":" + strconv.FormatUint(uint64(c.uid), 10)
}

// matcher is a compiled search request.
type matcher struct {
	req        adminapi.MessageSearch
	candidates map[string]bool // GUIDs of the text criterion, nil = any
}

// SearchMessages searches one or all mailboxes of the account, bounded
// by the page limit and the scan limit.
func (s *Server) SearchMessages(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	var req adminapi.MessageSearch
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	limit, cursor, ok := searchPaging(w, req)
	if !ok {
		return
	}

	u, ok := s.openAccount(w, r, RoleContent, account, true)
	if !ok {
		return
	}
	defer u.Close()

	auditf(r, "criteria", strings.Join(searchCriteria(req), ","))

	m, err := newMatcher(u, req)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	boxes, err := searchMailboxes(u, deref(req.MailboxId))
	if err != nil {
		s.importError(w, r, err)
		return
	}

	res, err := s.scan(u, boxes, m, cursor, limit)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	auditf(r, "hits", len(res.Items), "scanned", res.Scanned)
	writeJSON(w, http.StatusOK, res)
}

// searchPaging checks limit and cursor; ok false means the response is
// written.
func searchPaging(w http.ResponseWriter, req adminapi.MessageSearch) (int, searchCursor, bool) {
	limit := defaultSearchPage
	if req.Limit != nil {
		limit = *req.Limit
	}

	cursor, ok := parseCursor(deref(req.After))
	if limit < 1 || limit > maxSearchPage || !ok {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid limit (1 to 200) or cursor")
		return 0, searchCursor{}, false
	}

	return limit, cursor, true
}

// newMatcher compiles the request; the text criterion is resolved against
// the full-text index.
func newMatcher(u model.User, req adminapi.MessageSearch) (*matcher, error) {
	m := &matcher{req: req}

	text := deref(req.Text)
	if text == "" {
		return m, nil
	}

	hits, err := u.SearchFields([]string{fts.FieldHeader, fts.FieldBody}, text)
	if err != nil {
		return nil, err
	}

	if hits == nil && len(fts.Tokenize(text)) > 0 {
		hits = map[string]bool{} // no message has the words
	}

	m.candidates = hits

	return m, nil
}

// searchCriteria names the criteria of a request (for the audit line).
func searchCriteria(req adminapi.MessageSearch) []string {
	var out []string

	for name, set := range map[string]bool{"mailbox_id": req.MailboxId != nil, "from": req.From != nil, "to": req.To != nil,
		"subject": req.Subject != nil, "message_id": req.MessageId != nil, "text": req.Text != nil, "since": req.Since != nil,
		"before": req.Before != nil, "min_size": req.MinSize != nil, "max_size": req.MaxSize != nil, "flags": req.Flags != nil,
		"not_flags": req.NotFlags != nil} {
		if set {
			out = append(out, name)
		}
	}

	sort.Strings(out)

	return out
}

// searchMailboxes returns the mailboxes to search in id order (the cursor
// order): one, or all.
func searchMailboxes(u model.User, id string) ([]model.Mailbox, error) {
	if id != "" {
		mb, err := mailboxByID(u, id)
		if err != nil {
			return nil, err
		}

		return []model.Mailbox{*mb}, nil
	}

	all, err := u.ListMailboxes()
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })

	return all, err
}

// scan walks the mailboxes from the cursor until the page is full or the
// scan limit is reached.
func (s *Server) scan(u model.User, boxes []model.Mailbox, m *matcher, cursor searchCursor, limit int) (adminapi.SearchResult, error) {
	res := adminapi.SearchResult{Items: []adminapi.SearchHit{}}
	budget := int64(s.o.SearchScanLimit)

	if budget <= 0 {
		budget = defaultSearchScanLimit
	}

	pos := cursor // the last message looked at

	for i := range boxes {
		mb := &boxes[i]
		if mb.ID < cursor.mailbox {
			continue
		}

		msgs, err := u.ListMessages(mb.ID)
		if err != nil {
			return res, err
		}

		for j := range msgs {
			msg := &msgs[j]
			if mb.ID == cursor.mailbox && msg.UID <= cursor.uid {
				continue
			}

			if len(res.Items) == limit || res.Scanned == budget {
				res.Next = optional(pos.String())
				return res, nil
			}

			res.Scanned++
			pos = searchCursor{mb.ID, msg.UID}

			if m.match(msg) {
				res.Items = append(res.Items, adminapi.SearchHit{MailboxId: mb.ID, Mailbox: mb.Name,
					Uidvalidity: int64(mb.UIDValidity), Message: apiMeta(msg), Envelope: apiEnvelope(msg.Envelope)})
			}
		}
	}

	return res, nil
}

// match reports whether a message meets every criterion.
func (m *matcher) match(msg *model.Meta) bool {
	if m.candidates != nil && !m.candidates[msg.GUID] {
		return false
	}

	return m.matchMeta(msg) && m.matchEnvelope(msg)
}

func (m *matcher) matchMeta(msg *model.Meta) bool {
	r := &m.req

	switch {
	case r.Since != nil && msg.InternalDate.Before(*r.Since),
		r.Before != nil && !msg.InternalDate.Before(*r.Before),
		r.MinSize != nil && msg.Size < *r.MinSize,
		r.MaxSize != nil && msg.Size > *r.MaxSize:
		return false
	}

	for _, f := range derefSlice(r.Flags) {
		if !model.HasFlag(msg.Flags, f) {
			return false
		}
	}

	for _, f := range derefSlice(r.NotFlags) {
		if model.HasFlag(msg.Flags, f) {
			return false
		}
	}

	return true
}

func (m *matcher) matchEnvelope(msg *model.Meta) bool {
	r := &m.req
	if r.From == nil && r.To == nil && r.Subject == nil && r.MessageId == nil {
		return true
	}

	e := apiEnvelope(msg.Envelope)
	if e == nil {
		return false
	}

	return contains(e.From, r.From) && contains(append(e.To, e.Cc...), r.To) && containsFold(e.Subject, r.Subject) &&
		(r.MessageId == nil || strings.Trim(*r.MessageId, "<> ") == strings.Trim(e.MessageId, "<> "))
}

func contains(list []string, want *string) bool {
	if want == nil {
		return true
	}

	for _, s := range list {
		if containsFold(s, want) {
			return true
		}
	}

	return false
}

func containsFold(s string, want *string) bool {
	return want == nil || strings.Contains(strings.ToLower(s), strings.ToLower(*want))
}
