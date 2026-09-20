package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"shardpost/internal/adminapi"
	"shardpost/internal/model"
)

// Provisioning and permissions (roadmap 8b, role provision). Every change
// is idempotent by its semantics: absolute values, mailboxes addressed by
// their stable id. Relative ACL changes are computed on the server in one
// versioned step; If-Match makes a change conditional.

// RoleProvision may create, rename and subscribe mailboxes and change
// their ACLs.
const RoleProvision = "provision"

// inboxName is the mailbox that cannot be renamed through the admin API
// (in IMAP, RENAME INBOX moves its messages; that is a different
// operation).
const inboxName = "INBOX"

// identAnyone is the RFC 4314 identifier for everybody.
const identAnyone = "anyone"

// mailboxETag is the version of a mailbox: its registry record and name.
func mailboxETag(info *model.MailboxInfo) string {
	b, _ := json.Marshal(info.MailboxRec)
	sum := sha256.Sum256(append(append(b, 0), info.Name...))

	return `"m-` + hex.EncodeToString(sum[:8]) + `"`
}

// aclETag is the version of a mailbox's ACL.
func aclETag(gen uint64) string { return `"acl-` + strconv.FormatUint(gen, 10) + `"` }

// parseACLETag returns the generation an If-Match names; -1 for none or
// "*". ok is false for a malformed value.
func parseACLETag(ifMatch *string) (int64, bool) {
	v := strings.TrimSpace(deref(ifMatch))
	if v == "" || v == "*" {
		return -1, true
	}

	n, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(v, `"acl-`), `"`), 10, 63)
	if err != nil || !strings.HasPrefix(v, `"acl-`) {
		return 0, false
	}

	return int64(n), true
}

// validMailboxETag reports whether an If-Match is absent, "*" or shaped
// like a mailbox ETag.
func validMailboxETag(ifMatch *string) bool {
	v := strings.TrimSpace(deref(ifMatch))
	return v == "" || v == "*" || (strings.HasPrefix(v, `"m-`) && strings.HasSuffix(v, `"`) && len(v) == 20)
}

// matches reports whether an If-Match header allows the current ETag.
func matches(ifMatch *string, etag string) bool {
	v := strings.TrimSpace(deref(ifMatch))
	return v == "" || v == "*" || v == etag
}

// provisionError maps a failed change to the error document.
func (s *Server) provisionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, model.ErrMailboxExists):
		writeError(w, http.StatusConflict, adminapi.Conflict, "the mailbox exists")
	case errors.Is(err, model.ErrVersionConflict):
		writeError(w, http.StatusPreconditionFailed, adminapi.PreconditionFailed, "changed meanwhile, read it again")
	default:
		s.importError(w, r, err)
	}
}

// checkNewName validates a mailbox name to create or rename to; it
// returns the problem or "".
//
// A name below one of the client-visible namespace prefixes is refused:
// the mailbox would exist in the store but no client could address it,
// because that name resolves to another namespace.
func (s *Server) checkNewName(name string) string {
	if strings.HasSuffix(name, "/") || shadowsNamespace(name, s.o.SharedPrefix, s.o.PublicPrefix) {
		return msgInvalidMailbox
	}

	return checkMailboxSpec(name, 1, nil)
}

// shadowsNamespace reports whether name lies below one of the given
// namespace prefixes (empty prefixes are switched off).
func shadowsNamespace(name string, prefixes ...string) bool {
	lower := strings.ToLower(name)

	for _, p := range prefixes {
		if p != "" && strings.HasPrefix(lower, strings.ToLower(p)) {
			return true
		}
	}

	return false
}

// CreateMailbox creates a mailbox or confirms an identical one.
func (s *Server) CreateMailbox(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	var req adminapi.MailboxCreate

	u, ok := s.openAccount(w, r, RoleProvision, account, true)
	if !ok {
		return
	}
	defer u.Close()

	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	uses, msg := specialUses(req.SpecialUse)
	if msg != "" {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, msg)
		return
	}

	auditf(r, "mailbox", req.Name)

	if msg := s.checkNewName(req.Name); msg != "" {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, msg)
		return
	}

	status := http.StatusCreated

	err := u.CreateMailbox(req.Name, uses)
	if errors.Is(err, model.ErrMailboxExists) {
		status, err = http.StatusOK, sameSpecialUse(u, req.Name, uses)
	}

	if err != nil {
		s.provisionError(w, r, err)
		return
	}

	s.writeMailbox(w, r, u, req.Name, status)
}

// specialUses returns the requested special-use attributes in canonical
// spelling (as IMAP CREATE stores them); msg names an unknown one.
func specialUses(req *[]string) ([]string, string) {
	uses := []string{}

	if req == nil {
		return uses, ""
	}

	for _, a := range *req {
		c, ok := model.CanonicalSpecialUse(a)
		if !ok {
			return nil, "unknown special-use " + a
		}

		if !slices.Contains(uses, c) {
			uses = append(uses, c)
		}
	}

	return uses, ""
}

// sameSpecialUse accepts an existing mailbox when its special-use equals
// the requested one (idempotent create), else it is a conflict.
func sameSpecialUse(u model.User, name string, uses []string) error {
	mb, err := u.Lookup(name)
	if err != nil {
		return err
	}

	have, want := slices.Clone(mb.SpecialUse), slices.Clone(uses)
	slices.Sort(have)
	slices.Sort(want)

	if !slices.Equal(have, want) {
		return model.ErrMailboxExists
	}

	return nil
}

// writeMailbox answers with the mailbox summary and its ETag.
func (s *Server) writeMailbox(w http.ResponseWriter, r *http.Request, u model.User, name string, status int) {
	info, err := u.Info(name)
	if err != nil {
		s.provisionError(w, r, err)
		return
	}

	w.Header().Set("ETag", mailboxETag(info))
	writeJSON(w, status, mailboxSummary(info))
}

// checkMailboxVersion checks If-Match against the mailbox ETag; false
// means the response is written.
func (s *Server) checkMailboxVersion(w http.ResponseWriter, r *http.Request, ifMatch *string, info *model.MailboxInfo) bool {
	if !validMailboxETag(ifMatch) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, `If-Match must be a mailbox ETag ("m-…") or *`)
		return false
	}

	if !matches(ifMatch, mailboxETag(info)) {
		s.provisionError(w, r, model.ErrVersionConflict)
		return false
	}

	return true
}

// renameMailbox renames old to name with its children; false means the
// response is written.
func (s *Server) renameMailbox(w http.ResponseWriter, r *http.Request, u model.User, old, name string) bool {
	auditf(r, "mailbox", old, "new_name", name)

	switch msg := s.checkNewName(name); {
	case strings.EqualFold(old, inboxName):
		writeError(w, http.StatusConflict, adminapi.Conflict, "INBOX cannot be renamed")
	case msg != "":
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, msg)
	default:
		if err := u.RenameMailbox(old, name); err != nil {
			s.provisionError(w, r, err)
			return false
		}

		return true
	}

	return false
}

// UpdateMailbox renames a mailbox or changes its subscription.
func (s *Server) UpdateMailbox(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, p adminapi.UpdateMailboxParams) {
	u, info, ok := s.openMailbox(w, r, RoleProvision, account, id)
	if !ok {
		return
	}
	defer u.Close()

	var req adminapi.MailboxUpdate
	if err := decodeJSON(r, &req); err != nil || (req.Name == nil && req.Subscribed == nil) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request: name or subscribed required")
		return
	}

	if !s.checkMailboxVersion(w, r, p.IfMatch, info) {
		return
	}

	name := info.Name

	if req.Name != nil && *req.Name != name {
		if !s.renameMailbox(w, r, u, name, *req.Name) {
			return
		}

		name = *req.Name
	}

	if req.Subscribed != nil {
		auditf(r, "subscribed", *req.Subscribed)

		if err := u.SetSubscribed(name, *req.Subscribed); err != nil {
			s.provisionError(w, r, err)
			return
		}
	}

	s.writeMailbox(w, r, u, name, http.StatusOK)
}

// SetACLEntry changes the rights of one identifier.
func (s *Server) SetACLEntry(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, identifier adminapi.Identifier, p adminapi.SetACLEntryParams) {
	var req adminapi.ACLChange

	u, info, ok := s.openMailbox(w, r, RoleProvision, account, id)
	if !ok {
		return
	}
	defer u.Close()

	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid request")
		return
	}

	change, msg := aclChange(req)
	if msg != "" {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, msg)
		return
	}

	s.changeACL(w, r, u, account, info, identifier, p.IfMatch, change)
}

// DeleteACLEntry removes the entry of one identifier.
func (s *Server) DeleteACLEntry(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, identifier adminapi.Identifier, p adminapi.DeleteACLEntryParams) {
	u, info, ok := s.openMailbox(w, r, RoleProvision, account, id)
	if !ok {
		return
	}
	defer u.Close()

	s.changeACL(w, r, u, account, info, identifier, p.IfMatch, func(string) string { return "" })
}

// aclChange turns a request into the change function; msg names an
// invalid request.
func aclChange(req adminapi.ACLChange) (func(old string) string, string) {
	rights, ok := model.NormalizeRights(req.Rights)
	if !ok {
		return nil, fmt.Sprintf("invalid rights %q", req.Rights)
	}

	mode := adminapi.Replace
	if req.Mode != nil {
		mode = *req.Mode
	}

	switch mode {
	case adminapi.Replace:
		return func(string) string { return rights }, ""
	case adminapi.Add:
		return func(old string) string { return old + rights }, ""
	case adminapi.Remove:
		return func(old string) string {
			return strings.Map(func(r rune) rune {
				if strings.ContainsRune(rights, r) {
					return -1
				}

				return r
			}, old)
		}, ""
	default:
		return nil, "invalid mode"
	}
}

// changeACL checks the identifier and If-Match and applies the change in
// one versioned step.
func (s *Server) changeACL(w http.ResponseWriter, r *http.Request, u model.User, owner string, info *model.MailboxInfo,
	identifier string, ifMatch *string, change func(string) string) {
	identifier = strings.ToLower(strings.TrimSpace(identifier))
	auditf(r, "mailbox", info.Name, "identifier", identifier)

	expect, ok := parseACLETag(ifMatch)
	if !ok {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, `If-Match must be an ACL ETag ("acl-<n>") or *`)
		return
	}

	if !s.checkIdentifier(w, r, owner, identifier, info) {
		return
	}

	mod, ok := u.(model.ACLModifier)
	if !ok {
		writeError(w, http.StatusInternalServerError, adminapi.Internal, "the store cannot change ACLs")
		return
	}

	rights, _, err := mod.ModifyACL(info.Name, identifier, expect, change)
	if errors.Is(err, model.ErrPointerSync) {
		// The ACL is stored and counts; the pointer only lists the mailbox.
		s.o.Log.Warn("admin: shared pointer not updated", "mailbox", info.Name, "identifier", identifier, "err", err)
		auditf(r, "pointer_sync", "failed")
	} else if err != nil {
		s.provisionError(w, r, err)
		return
	}

	auditf(r, "rights", rights)

	after, err := u.Info(info.Name)
	if err != nil {
		s.provisionError(w, r, err)
		return
	}

	w.Header().Set("ETag", aclETag(after.ACLGen))
	writeJSON(w, http.StatusOK, mailboxACL(owner, after))
}

// checkIdentifier accepts "anyone" or a canonical account other than the
// owner that the client may address (its registry gets the pointer).
func (s *Server) checkIdentifier(w http.ResponseWriter, r *http.Request, owner, identifier string, info *model.MailboxInfo) bool {
	if identifier == identAnyone {
		return true
	}

	if !validAccount(identifier) || strings.HasPrefix(identifier, "-") {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid identifier")
		return false
	}

	switch state, canonical := s.lookupState(r, identifier); {
	case state == string(adminapi.AccountStatusLookupUnknown):
		writeError(w, http.StatusNotFound, adminapi.NoSuchUser, "no such user "+identifier)
	case state == string(adminapi.AccountStatusLookupUnavailable):
		writeError(w, http.StatusServiceUnavailable, adminapi.Temporary, "account lookup unavailable, try again later")
	case state == string(adminapi.AccountStatusLookupAlias):
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "not the canonical account, use "+canonical)
	case canonical == owner:
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "the owner of "+info.Name+" has all rights")
	case !principalOf(r.Context()).inScope(canonical):
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "identifier outside the client's scope")
	default:
		return true
	}

	return false
}

// mailboxACL is the API view of a mailbox ACL.
func mailboxACL(owner string, info *model.MailboxInfo) adminapi.MailboxACL {
	out := adminapi.MailboxACL{Owner: owner, Mailbox: info.Name, Generation: int64(info.ACLGen), Entries: []adminapi.ACLEntry{}}

	for _, ident := range sortedIdentifiers(info.ACL) {
		out.Entries = append(out.Entries, adminapi.ACLEntry{Identifier: ident, Rights: info.ACL[ident]})
	}

	return out
}
