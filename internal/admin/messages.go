package admin

import (
	"bytes"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/emersion/go-imap/v2"

	"shardpost/internal/adminapi"
	"shardpost/internal/model"
)

// Support tools on messages (roadmap 8d): metadata listing, envelope and
// header view, export, flags, copy and move. Messages are named by
// mailbox id, UIDVALIDITY and UID, so a recreated mailbox cannot answer
// with another message.

// Roles of the support tools.
const (
	// RoleContent reads message content (headers, bodies, Sieve scripts).
	RoleContent = "content"
	// RoleSupport changes messages and Sieve scripts.
	RoleSupport = "support"
)

// Message listing and batch bounds.
const (
	defaultMessagePage = 100
	maxMessagePage     = 1000
	maxHeaderBytes     = 64 << 10
)

// errUIDValidity refuses a request for another incarnation of a mailbox.
var errUIDValidity = errors.New("uidvalidity does not match the mailbox")

// canonicalFlags maps the lower-cased system flags and common keywords
// to their usual spelling (the store keeps flags lower-case).
var canonicalFlags = func() map[string]string {
	m := map[string]string{}
	for _, f := range []string{`\Seen`, `\Answered`, `\Flagged`, `\Deleted`, `\Draft`,
		"$Forwarded", "$MDNSent", "$Junk", "$NotJunk", "$Phishing"} {
		m[strings.ToLower(f)] = f
	}

	return m
}()

// apiFlags returns flags in their canonical spelling.
func apiFlags(flags []string) []string {
	out := make([]string, len(flags))

	for i, f := range flags {
		if c, ok := canonicalFlags[strings.ToLower(f)]; ok {
			f = c
		}

		out[i] = f
	}

	return out
}

func apiMeta(m *model.Meta) adminapi.MessageMeta {
	return adminapi.MessageMeta{Uid: int64(m.UID), Guid: m.GUID, Size: m.Size, Flags: apiFlags(m.Flags),
		InternalDate: m.InternalDate.UTC(), Modseq: int64(m.ModSeq)}
}

func apiEnvelope(e *imap.Envelope) *adminapi.MessageEnvelope {
	if e == nil {
		return nil
	}

	out := &adminapi.MessageEnvelope{Subject: e.Subject, From: addresses(e.From), To: addresses(e.To), Cc: addresses(e.Cc),
		MessageId: e.MessageID}

	if !e.Date.IsZero() {
		d := e.Date.UTC()
		out.Date = &d
	}

	return out
}

func addresses(list []imap.Address) []string {
	out := make([]string, 0, len(list))

	for _, a := range list {
		addr := a.Addr()
		if a.Name != "" {
			addr = a.Name + " <" + addr + ">"
		}

		out = append(out, addr)
	}

	return out
}

// ListMessages lists the message metadata of a mailbox by UID.
func (s *Server) ListMessages(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID,
	p adminapi.ListMessagesParams,
) {
	u, info, ok := s.openMailbox(w, r, RoleDiagnose, account, id)
	if !ok {
		return
	}
	defer u.Close()

	limit := defaultMessagePage
	if p.Limit != nil {
		limit = *p.Limit
	}

	if limit < 1 || limit > maxMessagePage {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "limit must be 1 to 1000")
		return
	}

	msgs, err := u.ListMessages(info.ID)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	after := uint32(0)
	if p.AfterUid != nil {
		after = uint32(min(max(*p.AfterUid, 0), int64(^uint32(0))))
	}

	i := sort.Search(len(msgs), func(i int) bool { return msgs[i].UID > after })
	page := adminapi.MessagePage{Uidvalidity: int64(info.UIDValidity), Items: []adminapi.MessageMeta{}}

	for ; i < len(msgs) && len(page.Items) < limit; i++ {
		page.Items = append(page.Items, apiMeta(&msgs[i]))
	}

	if i < len(msgs) && len(page.Items) > 0 {
		next := page.Items[len(page.Items)-1].Uid
		page.NextUid = &next
	}

	writeJSON(w, http.StatusOK, page)
}

// openMessage resolves mailbox, UIDVALIDITY and UID with the role; ok
// false means the response is written. The caller closes the user.
func (s *Server) openMessage(w http.ResponseWriter, r *http.Request, account, id string, uid, uidValidity int64) (model.User, *model.MailboxInfo, *model.Meta, bool) {
	u, info, ok := s.openMailbox(w, r, RoleContent, account, id)
	if !ok {
		return nil, nil, nil, false
	}

	auditf(r, "uid", uid)

	if !s.checkUIDValidity(w, r, info, uidValidity) {
		u.Close()
		return nil, nil, nil, false
	}

	metas, err := u.GetMessages(info.ID, []uint32{uint32(uid)})

	switch {
	case err != nil:
		s.importError(w, r, err)
	case len(metas) == 0 || int64(metas[0].UID) != uid:
		writeError(w, http.StatusNotFound, adminapi.NoSuchMessage, "no such message")
	default:
		return u, info, &metas[0], true
	}

	u.Close()

	return nil, nil, nil, false
}

// checkUIDValidity answers 412 when the mailbox is another incarnation.
func (s *Server) checkUIDValidity(w http.ResponseWriter, r *http.Request, info *model.MailboxInfo, uidValidity int64) bool {
	if int64(info.UIDValidity) != uidValidity {
		auditf(r, "uidvalidity", info.UIDValidity)
		writeError(w, http.StatusPreconditionFailed, adminapi.PreconditionFailed, errUIDValidity.Error())

		return false
	}

	return true
}

// GetMessage shows one message with envelope and header section.
func (s *Server) GetMessage(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, uid adminapi.UID,
	p adminapi.GetMessageParams,
) {
	u, info, m, ok := s.openMessage(w, r, account, id, uid, p.Uidvalidity)
	if !ok {
		return
	}
	defer u.Close()

	raw, err := u.Read(m)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, adminapi.MessageDetail{MailboxId: info.ID, Uidvalidity: int64(info.UIDValidity),
		Message: apiMeta(m), Envelope: apiEnvelope(m.Envelope), Header: headerSection(raw)})
}

// headerSection returns the header section of raw (up to the empty line),
// at most maxHeaderBytes.
func headerSection(raw []byte) string {
	end := len(raw)
	if i := bytes.Index(raw, []byte("\r\n\r\n")); i >= 0 {
		end = i + 2
	} else if i := bytes.Index(raw, []byte("\n\n")); i >= 0 {
		end = i + 1
	}

	return string(raw[:min(end, maxHeaderBytes)])
}

// ExportMessage returns one message as stored.
func (s *Server) ExportMessage(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, uid adminapi.UID,
	p adminapi.ExportMessageParams,
) {
	u, _, m, ok := s.openMessage(w, r, account, id, uid, p.Uidvalidity)
	if !ok {
		return
	}
	defer u.Close()

	raw, err := u.Read(m)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	auditf(r, "bytes", len(raw))
	w.Header().Set("Content-Type", "message/rfc822")
	w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// openBatch opens a mailbox for a support change of uids: role support,
// UIDVALIDITY and the UID list are checked. It returns the existing
// messages and the missing UIDs.
func (s *Server) openBatch(w http.ResponseWriter, r *http.Request, account, id string, uidValidity int64, uids []int64) (model.User, *model.MailboxInfo, []model.Meta, []int64, bool) {
	u, info, ok := s.openMailbox(w, r, RoleSupport, account, id)
	if !ok {
		return nil, nil, nil, nil, false
	}

	auditf(r, "uids", len(uids))

	want, msg := batchUIDs(uids)
	if msg != "" {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, msg)
	} else if s.checkUIDValidity(w, r, info, uidValidity) {
		metas, err := u.GetMessages(info.ID, want)
		if err == nil {
			return u, info, metas, missingUIDs(want, metas), true
		}

		s.importError(w, r, err)
	}

	u.Close()

	return nil, nil, nil, nil, false
}

// batchUIDs validates, sorts and deduplicates the UIDs of a batch.
func batchUIDs(uids []int64) ([]uint32, string) {
	if len(uids) == 0 || len(uids) > maxMessagePage {
		return nil, "uids must name 1 to 1000 messages"
	}

	seen := map[uint32]bool{}
	out := make([]uint32, 0, len(uids))

	for _, n := range uids {
		if n < 1 || n > int64(^uint32(0)) {
			return nil, "invalid uid " + strconv.FormatInt(n, 10)
		}

		if !seen[uint32(n)] {
			seen[uint32(n)] = true
			out = append(out, uint32(n))
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })

	return out, ""
}

func missingUIDs(want []uint32, metas []model.Meta) []int64 {
	have := map[uint32]bool{}
	for i := range metas {
		have[metas[i].UID] = true
	}

	out := []int64{}

	for _, uid := range want {
		if !have[uid] {
			out = append(out, int64(uid))
		}
	}

	return out
}

// SetMessageFlags adds or removes flags like IMAP STORE.
func (s *Server) SetMessageFlags(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID) {
	var req adminapi.FlagRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	add, errAdd := storeFlags(derefSlice(req.Add))
	remove, errRemove := storeFlags(derefSlice(req.Remove))

	if msg := firstError(errAdd, errRemove); msg != "" || len(add)+len(remove) == 0 {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request: "+cmpOr(msg, "add or remove required"))
		return
	}

	u, info, metas, missing, ok := s.openBatch(w, r, account, id, req.Uidvalidity, req.Uids)
	if !ok {
		return
	}
	defer u.Close()

	auditf(r, "add", strings.Join(add, " "), "remove", strings.Join(remove, " "))

	uids := make([]uint32, len(metas))
	for i := range metas {
		uids[i] = metas[i].UID
	}

	res := &model.FlagsResult{}

	var err error
	if len(uids) > 0 {
		res, err = u.SetFlags(info.Name, uids, unchangedSince(req.UnchangedSince), changeFlags(add, remove))
	}

	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, flagResult(res, missing))
}

func unchangedSince(p *int64) uint64 {
	if p == nil || *p < 0 {
		return 0
	}

	return uint64(*p)
}

func flagResult(res *model.FlagsResult, missing []int64) adminapi.FlagResult {
	out := adminapi.FlagResult{Updated: []adminapi.MessageMeta{}, Modified: []int64{}, Missing: missing}

	for i := range res.Updated {
		out.Updated = append(out.Updated, apiMeta(&res.Updated[i]))
	}

	for _, uid := range res.Modified {
		out.Modified = append(out.Modified, int64(uid))
	}

	return out
}

// storeFlags validates flags for the store (lower-case, no \Recent,
// only the known system flags, keywords as IMAP atoms).
func storeFlags(flags []string) ([]string, error) {
	out := make([]string, 0, len(flags))

	for _, f := range flags {
		lf := strings.ToLower(f)

		switch {
		case lf == `\recent`:
			return nil, errors.New(`\Recent cannot be set`)
		case strings.HasPrefix(lf, `\`) && canonicalFlags[lf] == "":
			return nil, errors.New("unknown system flag " + f)
		case !validKeyword(strings.TrimPrefix(lf, `\`)):
			return nil, errors.New("invalid flag " + strconv.Quote(f))
		}

		out = append(out, lf)
	}

	return out, nil
}

// validKeyword accepts an IMAP atom (RFC 9051 atom-specials excluded).
func validKeyword(k string) bool {
	if k == "" || len(k) > 128 {
		return false
	}

	for i := 0; i < len(k); i++ {
		c := k[i]
		if c <= ' ' || c >= 0x7f || strings.IndexByte(`(){%*"\]`, c) >= 0 {
			return false
		}
	}

	return true
}

func firstError(errs ...error) string {
	for _, err := range errs {
		if err != nil {
			return err.Error()
		}
	}

	return ""
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}

	return b
}

// changeFlags returns the flag update: add, then remove.
func changeFlags(add, remove []string) func(old []string) []string {
	return func(old []string) []string {
		out := make([]string, 0, len(old)+len(add))

		for _, f := range old {
			if !model.HasFlag(remove, f) {
				out = append(out, f)
			}
		}

		for _, f := range add {
			if !model.HasFlag(out, f) && !model.HasFlag(remove, f) {
				out = append(out, f)
			}
		}

		return out
	}
}

// CopyMessages copies or moves messages to another mailbox of the account.
func (s *Server) CopyMessages(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID) {
	var req adminapi.CopyRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	u, info, metas, missing, ok := s.openBatch(w, r, account, id, req.Uidvalidity, req.Uids)
	if !ok {
		return
	}
	defer u.Close()

	move := req.Move != nil && *req.Move
	auditf(r, "destination_id", req.DestinationId, "move", move)

	dest, err := mailboxByID(u, req.DestinationId)
	if err == nil && dest.ID == info.ID {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "destination is the source mailbox")
		return
	}

	var copied []model.Meta

	switch {
	case err != nil:
	case len(metas) == 0:
	case move:
		copied, err = u.Move(info.Name, metas, dest.Name)
	default:
		copied, err = u.Copy(metas, dest.Name)
	}

	if err != nil {
		s.copyError(w, r, err)
		return
	}

	out := adminapi.CopyResult{DestinationId: dest.ID, DestinationUidvalidity: int64(dest.UIDValidity), Missing: missing}
	out.Copied = make([]adminapi.CopiedMessage, 0, len(copied))

	for i := range copied {
		out.Copied = append(out.Copied, adminapi.CopiedMessage{NewUid: int64(copied[i].UID), Uid: int64(metas[i].UID)})
	}

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) copyError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, model.ErrOverQuota) {
		writeError(w, http.StatusInsufficientStorage, adminapi.OverQuota, "quota exceeded")
		return
	}

	s.importError(w, r, err)
}
