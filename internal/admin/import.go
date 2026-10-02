package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"

	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/model"
	"shardpost/internal/sieve"
)

// RoleImport may run account imports (roadmap 8e).
const RoleImport = "import"

// Bounds of import requests besides the configured batch limits.
const (
	maxImportMeta   = 64 << 10 // X-Import-Meta header of one part
	maxImportJSON   = 16 << 20 // JSON bodies (changes, ACL, Sieve)
	maxSieveScript  = 1 << 20
	maxSieveName    = model.MaxScriptName
	maxIdentifier   = 320
	defaultPageSize = 5000
)

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// importCall is an import request whose session holds the lock.
type importCall struct {
	u   model.User
	imp model.Importer
}

// beginImport checks the role and the parameters, opens the account and
// renews the session's lock; ok false means the response is written.
// The caller closes c.u.
func (s *Server) beginImport(w http.ResponseWriter, r *http.Request, account, session string) (*importCall, bool) {
	auditf(r, "account", account)

	if !requireAccount(w, r, RoleImport, account) {
		return nil, false
	}

	if s.o.Import == nil {
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "import is not enabled")
		return nil, false
	}

	if !validAccount(account) || !sessionPattern.MatchString(session) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid account or session")
		return nil, false
	}

	u, err := s.o.Import.User(account)
	if err != nil {
		s.importError(w, r, err)
		return nil, false
	}

	imp, ok := u.(model.Importer)
	if !ok {
		u.Close()
		writeError(w, http.StatusInternalServerError, adminapi.Internal, "the store cannot import")

		return nil, false
	}

	if _, err := imp.ImportRenew(session, s.o.ImportLockTTL); err != nil {
		u.Close()
		s.importError(w, r, err)

		return nil, false
	}

	// The account is not live here yet: no quota warnings or over-quota
	// flag changes from what the import stores or removes.
	if qs, ok := u.(model.QuotaEventSilencer); ok {
		qs.SilenceQuotaEvents()
	}

	return &importCall{u: u, imp: imp}, true
}

// OpenImport takes the account's import lock for a session.
func (s *Server) OpenImport(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	auditf(r, "account", account)

	if !requireAccount(w, r, RoleImport, account) {
		return
	}

	if s.o.Import == nil {
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "import is not enabled")
		return
	}

	var req adminapi.ImportSessionRequest
	if err := decodeOptionalJSON(r, &req); err != nil || !validAccount(account) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	session := sessionID(deref(req.Session))
	if !sessionPattern.MatchString(session) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid session")
		return
	}

	// The public namespace has no user database entry (see openAccount).
	if !model.IsPublic(account) && !s.canonical(w, r, account) {
		return
	}

	u, err := s.o.Import.User(account)
	if err != nil {
		s.importError(w, r, err)
		return
	}
	defer u.Close()

	imp, ok := u.(model.Importer)
	if !ok {
		writeError(w, http.StatusInternalServerError, adminapi.Internal, "the store cannot import")
		return
	}

	exp, err := imp.ImportLock(session, s.o.ImportLockTTL)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	if err := s.afterImportLock(r, account, session, u, req.Messages); err != nil {
		s.importError(w, r, err)
		return
	}

	auditf(r, "session", session)
	writeJSON(w, http.StatusOK, adminapi.ImportSession{Account: account, Session: session, ExpiresAt: exp.UTC()})
}

// sessionID is the requested session or a new random one.
func sessionID(requested string) string {
	if requested != "" {
		return requested
	}

	var b [16]byte

	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}

// canonical checks that account is the canonical account of the user
// database: an alias would import into a namespace nobody logs into.
func (s *Server) canonical(w http.ResponseWriter, r *http.Request, account string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), lookupTimeout)
	defer cancel()

	res, err := s.o.Auth.Lookup(auth.AdminLookup(ctx), account, "lmtp", clientAddr(r))
	if err != nil {
		s.importError(w, r, err)
		return false
	}

	if res.Account != "" && res.Account != account {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "not the canonical account, use "+res.Account)
		return false
	}

	return true
}

// CloseImport releases the lock of a session.
func (s *Server) CloseImport(w http.ResponseWriter, r *http.Request, account adminapi.Account, p adminapi.CloseImportParams) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	if err := c.imp.ImportUnlock(p.XImportSession); err != nil {
		s.importError(w, r, err)
		return
	}

	s.rs.forgetSession(account, p.XImportSession)

	w.WriteHeader(http.StatusNoContent)
}

// ListImportMailboxes lists the account's mailboxes with their state.
func (s *Server) ListImportMailboxes(w http.ResponseWriter, r *http.Request, account adminapi.Account, p adminapi.ListImportMailboxesParams) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	all, err := c.u.ListMailboxes()
	if err != nil {
		s.importError(w, r, err)
		return
	}

	out := adminapi.ImportMailboxList{Mailboxes: []adminapi.ImportMailboxState{}}

	for _, mb := range all {
		info, err := c.u.Info(mb.Name)
		if errors.Is(err, model.ErrNoMailbox) {
			continue // removed meanwhile
		}

		if err != nil {
			s.importError(w, r, err)
			return
		}

		out.Mailboxes = append(out.Mailboxes, mailboxState(info))
	}

	writeJSON(w, http.StatusOK, out)
}

// PutImportMailbox creates or adopts a mailbox with the source identity.
func (s *Server) PutImportMailbox(w http.ResponseWriter, r *http.Request, account adminapi.Account, p adminapi.PutImportMailboxParams) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	var spec adminapi.ImportMailboxSpec
	if err := decodeJSON(r, &spec); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid mailbox: "+err.Error())
		return
	}

	uses := derefSlice(spec.SpecialUse)
	if msg := checkMailboxSpec(spec.Name, spec.Uidvalidity, uses); msg != "" {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, msg)
		return
	}

	// An imported mailbox below a namespace prefix would be as
	// unreachable as a newly created one: the name belongs to another
	// namespace. Rename it at the source (see checkNewName).
	if shadowsNamespace(spec.Name, s.o.SharedPrefix, s.o.PublicPrefix) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest,
			"the name lies below a namespace prefix, where no client could open it: "+spec.Name)

		return
	}

	auditf(r, "mailbox", spec.Name, "uidvalidity", spec.Uidvalidity)

	mb, err := c.imp.ImportMailbox(model.ImportMailbox{Name: spec.Name, UIDValidity: uint32(spec.Uidvalidity), ID: strings.ToLower(deref(spec.Id)),
		SpecialUse: uses, Subscribed: spec.Subscribed != nil && *spec.Subscribed, ShardSize: s.o.ImportShardSize,
		Replace: spec.Replace != nil && *spec.Replace})
	if err != nil {
		s.importError(w, r, err)
		return
	}

	s.writeMailboxState(w, r, c, mb.ID)
}

// DeleteImportMailbox deletes a mailbox the source no longer has.
func (s *Server) DeleteImportMailbox(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, p adminapi.DeleteImportMailboxParams) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	mb, err := c.imp.MailboxByID(id)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	auditf(r, "mailbox", mb.Name)

	if mb.Name == "INBOX" {
		writeError(w, http.StatusConflict, adminapi.Conflict, "INBOX cannot be deleted")
		return
	}

	if err := c.u.DeleteMailbox(mb.Name); err != nil {
		s.importError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// ListImportMessages returns one page of a mailbox inventory.
func (s *Server) ListImportMessages(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, p adminapi.ListImportMessagesParams) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	after := int64(0)
	if p.After != nil {
		after = *p.After
	}

	limit := s.o.ImportMaxInventory
	if p.Limit != nil && *p.Limit < limit {
		limit = *p.Limit
	}

	if after < 0 || after > math.MaxUint32 || limit < 1 {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid after or limit")
		return
	}

	metas, err := c.imp.ListMessagesAfter(id, uint32(after), limit)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	out := adminapi.ImportInventory{Messages: make([]adminapi.ImportInventoryEntry, 0, len(metas))}
	for _, m := range metas {
		flags := m.Flags
		if flags == nil {
			flags = []string{}
		}

		out.Messages = append(out.Messages, adminapi.ImportInventoryEntry{Uid: int64(m.UID), Guid: m.GUID, Size: m.Size, Modseq: int64(m.ModSeq), Flags: flags})
	}

	if len(metas) == limit {
		next := int64(metas[len(metas)-1].UID)
		out.NextAfter = &next
	}

	writeJSON(w, http.StatusOK, out)
}

// ImportMessages stores one batch of messages at their source UIDs.
func (s *Server) ImportMessages(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, p adminapi.ImportMessagesParams) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	if s.refuseDuringReshard(w, account, p.XImportSession, c.u) {
		return
	}

	s.applyHint(account, p.XImportSession, c.u)

	msgs, release, err := s.readImportBatch(r)
	defer release()

	if err != nil {
		s.importError(w, r, err)
		return
	}

	if qe, ok := c.u.(model.QuotaExemptable); ok {
		qe.SetQuotaEnforced(false) // counted, not enforced: the source accepted the mail
	}

	res, err := c.imp.ImportMessages(id, msgs)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	auditf(r, "mailbox_id", id, "messages", len(msgs), "imported", res.Imported, "existing", res.Existing, "shared", res.Shared, "bytes", res.Bytes)
	writeJSON(w, http.StatusOK, adminapi.ImportResult{Imported: res.Imported, Existing: res.Existing, Shared: res.Shared, Bytes: res.Bytes})
}

// errBadBatch marks an invalid import body.
type errBadBatch struct{ msg string }

func (e *errBadBatch) Error() string { return e.msg }

func badBatch(msg string) error { return &errBadBatch{msg: msg} }

// readImportBatch parses the multipart body of importMessages within the
// configured bounds; the messages are reserved against the memory budget
// until release.
func (s *Server) readImportBatch(r *http.Request) ([]model.ImportMessage, func(), error) {
	var releases []func()

	release := func() {
		for _, rel := range releases {
			rel()
		}
	}

	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/mixed" || params["boundary"] == "" {
		return nil, release, badBatch("the body must be multipart/mixed")
	}

	mr := multipart.NewReader(r.Body, params["boundary"])

	var (
		msgs  []model.ImportMessage
		total int64
	)

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, release, badBatch("malformed multipart body")
		}

		if len(msgs) >= s.o.ImportMaxBatchMessages {
			return nil, release, badBatch("too many messages in one batch")
		}

		m, rel, err := s.readPart(part, msgs, total)
		releases = append(releases, rel)

		if err != nil {
			return nil, release, err
		}

		total += int64(len(m.Raw))
		msgs = append(msgs, m)
	}

	if len(msgs) == 0 {
		return nil, release, badBatch("no messages")
	}

	return msgs, release, nil
}

// readPart reads one message part after the batch so far (UIDs
// ascending, total bytes within the batch limit).
func (s *Server) readPart(part *multipart.Part, before []model.ImportMessage, total int64) (model.ImportMessage, func(), error) {
	var last uint32
	if len(before) > 0 {
		last = before[len(before)-1].UID
	}

	m, err := importPartMeta(part, last)
	if err != nil {
		return m, func() {}, err
	}

	room := s.o.ImportMaxBatchBytes - total
	if room <= 0 {
		return m, func() {}, model.ErrTooLarge
	}

	raw, rel, err := delivery.ReadMessage(part, min(room, s.o.MaxMessageBytes))
	m.Raw = raw

	return m, rel, err
}

// importPartMeta validates the identity of one part (UIDs ascending after
// last).
func importPartMeta(part *multipart.Part, last uint32) (model.ImportMessage, error) {
	if mt, _, err := mime.ParseMediaType(part.Header.Get("Content-Type")); err != nil || mt != mimeRFC822 {
		return model.ImportMessage{}, badBatch("every part must be message/rfc822")
	}

	raw := part.Header.Get(adminapi.ImportMetaHeader)
	if raw == "" || len(raw) > maxImportMeta {
		return model.ImportMessage{}, badBatch("missing or oversized " + adminapi.ImportMetaHeader)
	}

	var meta adminapi.ImportMessageMeta
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return model.ImportMessage{}, badBatch("invalid " + adminapi.ImportMetaHeader)
	}

	if msg := checkPartMeta(&meta, last); msg != "" {
		return model.ImportMessage{}, badBatch(msg)
	}

	return model.ImportMessage{UID: meta.UID, ModSeq: meta.ModSeq, Flags: meta.Flags, InternalDate: meta.InternalDate,
		SaveDate: meta.SaveDate, GUID: strings.ToLower(meta.GUID), POP3UIDL: meta.POP3UIDL}, nil
}

// checkPartMeta validates a message identity; it returns the problem or
// "".
func checkPartMeta(meta *adminapi.ImportMessageMeta, last uint32) string {
	switch {
	case meta.UID == 0 || meta.UID <= last:
		return "uids must be ascending and non-zero"
	case meta.POP3UIDL != "" && !model.ValidPOP3UIDL(meta.POP3UIDL):
		return "invalid pop3_uidl"
	case meta.GUID != "" && !model.ValidGUID(strings.ToLower(meta.GUID)):
		return "invalid guid"
	}

	return checkFlags(meta.Flags)
}

// ImportChanges applies source flag changes and expunges.
func (s *Server) ImportChanges(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, p adminapi.ImportChangesParams) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	if s.refuseDuringReshard(w, account, p.XImportSession, c.u) {
		return
	}

	var req adminapi.ImportChanges
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid changes: "+err.Error())
		return
	}

	flags, expunge, msg := s.importChanges(&req)
	if msg != "" {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, msg)
		return
	}

	auditf(r, "mailbox_id", id, "flag_changes", len(flags), "expunged", len(expunge))

	if err := c.imp.ImportChanges(id, flags, expunge); err != nil {
		s.importError(w, r, err)
		return
	}

	s.writeMailboxState(w, r, c, id)
}

// importChanges validates a changes request; it returns the problem or
// "".
func (s *Server) importChanges(req *adminapi.ImportChanges) ([]model.ImportFlags, []uint32, string) {
	if len(derefSlice(req.Flags))+len(derefSlice(req.Expunge)) > s.o.ImportMaxChanges {
		return nil, nil, "too many changes in one request"
	}

	flags := make([]model.ImportFlags, 0, len(derefSlice(req.Flags)))

	for _, f := range derefSlice(req.Flags) {
		if f.Uid < 1 || f.Uid > math.MaxUint32 || f.Modseq < 0 {
			return nil, nil, "invalid uid or modseq"
		}

		if msg := checkFlags(f.Flags); msg != "" {
			return nil, nil, msg
		}

		flags = append(flags, model.ImportFlags{UID: uint32(f.Uid), ModSeq: uint64(f.Modseq), Flags: f.Flags})
	}

	expunge := make([]uint32, 0, len(derefSlice(req.Expunge)))

	for _, uid := range derefSlice(req.Expunge) {
		if uid < 1 || uid > math.MaxUint32 {
			return nil, nil, msgInvalidUID
		}

		expunge = append(expunge, uint32(uid))
	}

	return flags, expunge, ""
}

// FinalizeImportMailbox sets UIDNEXT and HIGHESTMODSEQ to the source.
func (s *Server) FinalizeImportMailbox(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, p adminapi.FinalizeImportMailboxParams) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	var req adminapi.ImportFinalize
	if err := decodeJSON(r, &req); err != nil || req.UidNext < 1 || req.UidNext > math.MaxUint32 || req.HighestModseq < 0 {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid uid_next or highest_modseq")
		return
	}

	auditf(r, "mailbox_id", id, "uid_next", req.UidNext, "highest_modseq", req.HighestModseq)

	info, err := c.imp.ImportFinalize(id, uint32(req.UidNext), uint64(req.HighestModseq))
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, mailboxState(info))
}

// PutImportACL replaces the explicit ACL entries of a mailbox.
func (s *Server) PutImportACL(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, p adminapi.PutImportACLParams) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	var req adminapi.ImportACL
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid acl: "+err.Error())
		return
	}

	want, msg := s.desiredACL(r, req.Entries, account)
	if msg != "" {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, msg)
		return
	}

	mb, err := c.imp.MailboxByID(id)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	auditf(r, "mailbox", mb.Name, "entries", len(want))

	if err := replaceACL(c.u, mb.Name, want); err != nil {
		s.importError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// desiredACL validates and normalizes ACL entries: identifiers are
// "anyone" or accounts of the user database (resolved to the canonical
// account); the owner's own entry is dropped (implicit). Dovecot pseudo
// identifiers (owner, authenticated, $group, -negative, #global) have no
// counterpart here and are refused. It returns the entries or the
// problem.
func (s *Server) desiredACL(r *http.Request, entries map[string]string, owner string) (map[string]string, string) {
	want := map[string]string{}

	for ident, rights := range entries {
		norm, ok := model.NormalizeRights(rights)
		if !ok || !validIdentifier(ident) {
			return nil, "invalid identifier or rights: " + ident
		}

		account, err := s.aclAccount(r, ident)
		if err != nil {
			return nil, "unknown identifier " + ident
		}

		if account != owner {
			want[account] = norm
		}
	}

	return want, ""
}

// aclAccount resolves an ACL identifier to the canonical account
// ("anyone" stays).
func (s *Server) aclAccount(r *http.Request, ident string) (string, error) {
	if ident == identAnyone {
		return ident, nil
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), lookupTimeout)
	defer cancel()

	res, err := s.o.Auth.Lookup(auth.AdminLookup(ctx), ident, "lmtp", clientAddr(r))
	if err != nil {
		return "", err
	}

	if res.Account == "" {
		return ident, nil
	}

	return res.Account, nil
}

// replaceACL sets the entries of want and removes all others.
func replaceACL(u model.User, name string, want map[string]string) error {
	have, err := u.GetACL(name)
	if err != nil {
		return err
	}

	for ident, rights := range want {
		if have[ident] != rights {
			if err := u.SetACL(name, ident, rights); err != nil {
				return err
			}
		}
	}

	for ident := range have {
		if _, keep := want[ident]; !keep {
			if err := u.SetACL(name, ident, ""); err != nil {
				return err
			}
		}
	}

	return nil
}

// PutImportSieve replaces the Sieve scripts. All scripts are compiled
// first (includes resolve among the imported scripts), so an invalid
// set changes nothing.
func (s *Server) PutImportSieve(w http.ResponseWriter, r *http.Request, account adminapi.Account, p adminapi.PutImportSieveParams) {
	c, ok := s.beginImport(w, r, account, p.XImportSession)
	if !ok {
		return
	}
	defer c.u.Close()

	var req adminapi.ImportSieve
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid sieve: "+err.Error())
		return
	}

	scripts := map[string]string{}

	for _, sc := range req.Scripts {
		if refuseReserved(w, sc.Name) {
			return
		}

		if !validScriptName(sc.Name) || len(sc.Script) > maxSieveScript || scripts[sc.Name] != "" {
			writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid, oversized or duplicate script "+sc.Name)
			return
		}

		scripts[sc.Name] = sc.Script
	}

	if _, ok := scripts[req.Active]; req.Active != "" && !ok {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "the active script is not in the set")
		return
	}

	if msg := s.compileScripts(scripts); msg != "" {
		writeError(w, http.StatusUnprocessableEntity, adminapi.Rejected, msg)
		return
	}

	auditf(r, "scripts", len(scripts), "active", req.Active)

	if err := replaceScripts(c.u, scripts, req.Active); err != nil {
		s.importError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// compileScripts validates every script like ManageSieve PUTSCRIPT does;
// it returns the first problem or "".
func (s *Server) compileScripts(scripts map[string]string) string {
	global := sieve.IncludeLoader(nil, s.globalScripts())
	loader := func(name string, isGlobal bool) ([]byte, bool, error) {
		if isGlobal {
			return global(name, true)
		}

		text, ok := scripts[name]

		return []byte(text), ok, nil
	}

	for name, text := range scripts {
		if err := sieve.CompileWith(text, sieve.Hooks{Include: loader, Config: s.sieveConfig()}); err != nil {
			return "script " + name + ": " + err.Error()
		}
	}

	return ""
}

func (s *Server) globalScripts() string {
	if s.o.Delivery == nil {
		return ""
	}

	return s.o.Delivery.GlobalScripts
}

func (s *Server) sieveConfig() map[string]string {
	if s.o.Delivery == nil {
		return nil
	}

	return s.o.Delivery.SieveConfig
}

// replaceScripts stores the scripts, sets the active one and removes the
// others — except the script of the JMAP VacationResponse, which follows
// the account's metadata rather than the set: a set without it leaves it
// alone.
func replaceScripts(u model.User, scripts map[string]string, active string) error {
	for name, text := range scripts {
		if err := u.PutScript(name, text); err != nil {
			return err
		}
	}

	if err := u.SetActiveScript(active); err != nil {
		return err
	}

	names, _, err := u.ListScripts()
	if err != nil {
		return err
	}

	for _, name := range names {
		if _, keep := scripts[name]; !keep && !sieve.ReservedScript(name) {
			if err := u.DeleteScript(name); err != nil {
				return err
			}
		}
	}

	return nil
}

// writeMailboxState answers with the state of the mailbox id.
func (s *Server) writeMailboxState(w http.ResponseWriter, r *http.Request, c *importCall, id string) {
	mb, err := c.imp.MailboxByID(id)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	info, err := c.u.Info(mb.Name)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, mailboxState(info))
}

func mailboxState(info *model.MailboxInfo) adminapi.ImportMailboxState {
	uses := info.SpecialUse
	if uses == nil {
		uses = []string{}
	}

	return adminapi.ImportMailboxState{Id: info.ID, Name: info.Name, Uidvalidity: int64(info.UIDValidity), UidNext: int64(info.UIDNext),
		HighestModseq: int64(info.HighestModSeq), Subscribed: info.Subscribed, SpecialUse: uses}
}

// importError maps a failed import operation to the error document.
func (s *Server) importError(w http.ResponseWriter, r *http.Request, err error) {
	var (
		conflict *model.ImportConflictError
		bad      *errBadBatch
	)

	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, bad.msg)
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, adminapi.Conflict, conflict.Reason)
	case errors.Is(err, model.ErrAccountPurging):
		writeError(w, http.StatusConflict, adminapi.AccountPurging, "the account is being purged; if no purge job runs any more, run shardpostctl account repair")
	case errors.Is(err, model.ErrImportLocked):
		writeError(w, http.StatusConflict, adminapi.ImportLocked, "another import session holds the account")
	case errors.Is(err, model.ErrImportNotHeld):
		writeError(w, http.StatusConflict, adminapi.NotLocked, "the session does not hold the import lock")
	case errors.Is(err, model.ErrNoMailbox):
		writeError(w, http.StatusNotFound, adminapi.NoSuchMailbox, "no such mailbox")
	case errors.Is(err, auth.ErrInvalid):
		writeError(w, http.StatusNotFound, adminapi.NoSuchUser, "no such user")
	case errors.Is(err, model.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, adminapi.TooLarge, "message or batch too large")
	case errors.Is(err, model.ErrLimit):
		writeError(w, http.StatusConflict, adminapi.LimitExceeded, err.Error())
	case errors.Is(err, auth.ErrTemporary), errors.Is(err, delivery.ErrBusy):
		writeError(w, http.StatusServiceUnavailable, adminapi.Temporary, "temporary failure, try again later")
	default:
		s.o.Log.Error("admin import failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, adminapi.Temporary, "temporary failure, try again later")
	}
}

// checkMailboxSpec validates a mailbox specification; it returns the
// problem or "".
// msgInvalidMailbox answers an unusable mailbox name, the other two a
// UID or a UIDVALIDITY outside 1..2^32-1.
const (
	msgInvalidMailbox     = "invalid mailbox name"
	msgInvalidUID         = "invalid uid"
	msgInvalidUIDValidity = "invalid uidvalidity"
)

func checkMailboxSpec(name string, uidValidity int64, uses []string) string {
	switch {
	case name == "" || len(name) > maxMailbox || hasControl(name) || strings.Contains(name, "//"):
		return msgInvalidMailbox
	case uidValidity < 1 || uidValidity > math.MaxUint32:
		return msgInvalidUIDValidity
	}

	for _, u := range uses {
		if !strings.HasPrefix(u, `\`) || !model.ValidFlag(u) {
			return "invalid special-use " + u
		}
	}

	return ""
}

// checkFlags validates flags and keywords; it returns the problem or "".
func checkFlags(flags []string) string {
	for _, f := range flags {
		if !model.ValidFlag(f) {
			return "invalid flag " + f
		}
	}

	return ""
}

// validAccount accepts a bounded account name without control
// characters, spaces or path separators.
func validAccount(a string) bool {
	return a != "" && len(a) <= maxAddress && !hasControl(a) && !strings.ContainsAny(a, " /\\") &&
		(!model.ReservedNamespace(a) || model.IsPublic(a))
}

// validIdentifier accepts an ACL identifier that can be an account or
// "anyone" (no Dovecot pseudo identifiers).
func validIdentifier(id string) bool {
	switch {
	case !model.ValidACLIdentifier(id) || len(id) > maxIdentifier || hasControl(id) || strings.ContainsAny(id, " /\\"):
		return false
	case id == "owner" || id == "authenticated" || strings.ContainsAny(id[:1], "$-#"):
		return false
	}

	return true
}

// validScriptName accepts a Sieve script name like ManageSieve does.
func validScriptName(n string) bool {
	return n != "" && len(n) <= maxSieveName && !hasControl(n) && !strings.ContainsAny(n, "/\\") && n != "." && n != ".."
}

// decodeJSON decodes a bounded JSON body strictly.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxImportJSON))
	dec.DisallowUnknownFields()

	return dec.Decode(v)
}

// decodeOptionalJSON is decodeJSON for a body that may be empty.
func decodeOptionalJSON(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}

	err := decodeJSON(r, v)
	if errors.Is(err, io.EOF) {
		return nil
	}

	return err
}

func derefSlice[T any](p *[]T) []T {
	if p == nil {
		return nil
	}

	return *p
}
