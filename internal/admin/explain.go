package admin

import (
	"fmt"
	"net/http"
	"strings"

	"shardpost/internal/adminapi"
	"shardpost/internal/model"
)

// ExplainMailboxACL explains the access of a principal to a mailbox: its
// canonical account, the effective rights (model.EffectiveRights, as in
// IMAP) and the pointer in its registry that lists the mailbox under
// "Other Users/".
func (s *Server) ExplainMailboxACL(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID, p adminapi.ExplainMailboxACLParams) {
	if !validAccount(p.Principal) {
		auditf(r, "account", account)
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid principal")

		return
	}

	u, info, ok := s.openMailbox(w, r, RoleDiagnose, account, id)
	if !ok {
		return
	}
	defer u.Close()

	auditf(r, "principal", p.Principal)

	state, canonical := s.lookupState(r, p.Principal)
	out := adminapi.ACLExplanation{Owner: account, Mailbox: info.Name, Principal: p.Principal,
		Lookup: adminapi.ACLExplanationLookup(state), AnyoneEntry: info.ACL["anyone"], Findings: []string{}}

	subject := strings.ToLower(p.Principal)
	if canonical != "" {
		subject = canonical
		out.CanonicalPrincipal = &canonical
	}

	// The principal's registry is data of another account: the scope
	// applies to it as well.
	if !principalOf(r.Context()).inScope(subject) {
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "principal outside the client's scope")
		return
	}

	out.IsOwner = subject == account
	out.OwnEntry = info.ACL[subject]
	out.Rights = model.EffectiveRights(info.ACL, subject)

	if out.IsOwner {
		out.Rights = model.AllRights
	}

	out.Visible = model.HasRight(out.Rights, 'l')

	if !out.IsOwner && state != string(adminapi.ACLExplanationLookupUnknown) {
		if err := s.pointer(&out, subject, info); err != nil {
			s.importError(w, r, err)
			return
		}
	}

	out.Findings = findings(&out, info)
	writeJSON(w, http.StatusOK, out)
}

// pointer fills in the principal's shared-mailbox pointer.
func (s *Server) pointer(out *adminapi.ACLExplanation, subject string, info *model.MailboxInfo) error {
	pu, err := s.o.Accounts.User(subject)
	if err != nil {
		return err
	}
	defer pu.Close()

	refs, err := pu.ListShared()
	if err != nil {
		return err
	}

	for _, ref := range refs {
		if ref.Owner == out.Owner && ref.Mailbox == info.Name {
			gen, sub := int64(ref.Gen), ref.Subscribed
			out.Pointer = adminapi.SharedPointer{Present: true, Generation: &gen, Subscribed: &sub}

			return nil
		}
	}

	return nil
}

// findings states every reason for the outcome in plain text.
func findings(out *adminapi.ACLExplanation, info *model.MailboxInfo) []string {
	var f []string

	add := func(format string, a ...any) { f = append(f, fmt.Sprintf(format, a...)) }

	switch out.Lookup {
	case adminapi.ACLExplanationLookupUnknown:
		add("%s is unknown to Nauthilus: it cannot log in", out.Principal)
	case adminapi.ACLExplanationLookupUnavailable:
		add("Nauthilus lookup failed: the result assumes %s is canonical", out.Principal)
	case adminapi.ACLExplanationLookupAlias:
		add("%s is an alias of %s: sessions and ACL entries use the canonical account", out.Principal, *out.CanonicalPrincipal)

		if r, ok := info.ACL[strings.ToLower(out.Principal)]; ok {
			add("the ACL names the alias %s (rights %q): that entry never applies", out.Principal, r)
		}
	}

	if out.IsOwner {
		add("the principal owns the mailbox: all rights, listed in its own namespace")
		return f
	}

	switch {
	case out.Rights == "":
		add("no ACL entry for the principal or anyone: the mailbox does not exist for it (NONEXISTENT)")
	case !out.Visible:
		add("rights %q lack l: the mailbox is not listed, but can be addressed by name", out.Rights)
	}

	if out.Lookup == adminapi.ACLExplanationLookupUnknown {
		return f
	}

	return append(f, pointerFindings(out, info)...)
}

// pointerFindings explains the shared-mailbox pointer.
func pointerFindings(out *adminapi.ACLExplanation, info *model.MailboxInfo) []string {
	shown := "Other Users/" + out.Owner + "/" + info.Name

	switch ptr := out.Pointer; {
	case out.Visible && !ptr.Present && out.OwnEntry == "":
		return []string{"rights only through anyone, which writes no pointers: LIST does not show " + shown + "; it can be addressed by name"}
	case out.Visible && !ptr.Present:
		return []string{"no pointer in the principal's registry: LIST does not show " + shown + "; a SETACL for the principal on the mailbox writes it"}
	case !out.Visible && ptr.Present:
		return []string{"pointer present but no right l: LIST hides " + shown + "; the pointer stays until the mailbox is deleted or a SETACL for the principal withdraws it"}
	case out.Visible:
		return []string{"visible as " + shown}
	default:
		return nil
	}
}
