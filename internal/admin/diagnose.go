package admin

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"sort"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
	"shardpost/internal/model"
)

// Read-only diagnosis (roadmap 8a, role diagnose). The handlers only call
// reading store operations; a test wraps the store in a view that fails
// every write.

// Mailbox page sizes.
const (
	defaultMailboxPage = 100
	maxMailboxPage     = 1000
)

// flagSeen and flagDeleted are the stored (lower-case) system flags.
const (
	flagSeen    = `\seen`
	flagDeleted = `\deleted`
)

// errStoreStatus is reported when no store ping is configured.
var errStoreStatus = errors.New("store status not available")

// GetServerStatus reports version, instance, configuration fingerprint
// and store reachability.
func (s *Server) GetServerStatus(w http.ResponseWriter, r *http.Request) {
	if !require(w, r, RoleDiagnose) {
		return
	}

	st := adminapi.ServerStatus{Version: s.o.Version, GoVersion: runtime.Version(), Instance: s.o.Instance,
		StartedAt: s.o.StartedAt.UTC(), ConfigFingerprint: s.o.ConfigFingerprint, Store: adminapi.StoreStatus{Ok: true}}

	err := errStoreStatus
	if s.o.StorePing != nil {
		err = s.o.StorePing()
	}

	if err != nil {
		msg := err.Error()
		st.Store = adminapi.StoreStatus{Ok: false, Error: &msg}
	}

	writeJSON(w, http.StatusOK, st)
}

// lookupState resolves an account through Nauthilus for the diagnosis;
// it never fails the request.
func (s *Server) lookupState(r *http.Request, account string) (state, canonical string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), lookupTimeout)
	defer cancel()

	res, err := s.o.Auth.Lookup(ctx, account, "lmtp", clientAddr(r))

	switch {
	case errors.Is(err, auth.ErrInvalid):
		return string(adminapi.AccountStatusLookupUnknown), ""
	case err != nil:
		return string(adminapi.AccountStatusLookupUnavailable), ""
	case res.Account == "" || res.Account == account:
		return string(adminapi.AccountStatusLookupCanonical), account
	default:
		return string(adminapi.AccountStatusLookupAlias), res.Account
	}
}

// openAccount checks role, account and scope and opens the account for
// reading; with canonical, the account must be the canonical one (store
// data lives under it). The caller closes the user.
func (s *Server) openAccount(w http.ResponseWriter, r *http.Request, account string, canonical bool) (model.User, bool) {
	auditf(r, "account", account)

	if !requireAccount(w, r, RoleDiagnose, account) {
		return nil, false
	}

	if s.o.Accounts == nil {
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "diagnosis is not enabled")
		return nil, false
	}

	if !validAccount(account) {
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "invalid account")
		return nil, false
	}

	if canonical && !s.canonical(w, r, account) {
		return nil, false
	}

	u, err := s.o.Accounts.User(account)
	if err != nil {
		s.importError(w, r, err)
		return nil, false
	}

	return u, true
}

// GetAccount shows the lookup result and the store state of an account.
func (s *Server) GetAccount(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	u, ok := s.openAccount(w, r, account, false)
	if !ok {
		return
	}
	defer u.Close()

	state, canonical := s.lookupState(r, account)
	if canonical != "" && !principalOf(r.Context()).inScope(canonical) {
		writeError(w, http.StatusForbidden, adminapi.Forbidden, "canonical account outside the client's scope")
		return
	}

	out := adminapi.AccountStatus{Account: account, Lookup: adminapi.AccountStatusLookup(state)}

	if canonical != "" {
		out.CanonicalAccount = &canonical
	}

	boxes, err := u.ListMailboxes()
	if err != nil {
		s.importError(w, r, err)
		return
	}

	out.StoreExists, out.Mailboxes = len(boxes) > 0, len(boxes)

	if out.Quota, err = quotaStatus(u); err != nil {
		s.importError(w, r, err)
		return
	}

	if imp, ok := u.(model.Importer); ok {
		if out.ImportLocked, err = imp.ImportLocked(); err != nil {
			s.importError(w, r, err)
			return
		}
	}

	if t, ok := u.(model.Tracer); ok {
		st, err := t.Trace()
		if err != nil {
			s.importError(w, r, err)
			return
		}

		if out.TraceActive = st.Active(time.Now()); out.TraceActive {
			until := st.Until.UTC()
			out.TraceUntil = &until
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// quotaStatus reads usage and limit.
func quotaStatus(u model.User) (adminapi.QuotaStatus, error) {
	usage, err := u.QuotaUsage()
	if err != nil {
		return adminapi.QuotaStatus{}, err
	}

	limit, err := u.QuotaLimit()
	if err != nil {
		return adminapi.QuotaStatus{}, err
	}

	return adminapi.QuotaStatus{UsageBytes: usage.Bytes, UsageMessages: usage.Msgs, LimitBytes: limit.Bytes, LimitMessages: limit.Msgs}, nil
}

// GetQuota shows usage and limit of an account.
func (s *Server) GetQuota(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	u, ok := s.openAccount(w, r, account, true)
	if !ok {
		return
	}
	defer u.Close()

	q, err := quotaStatus(u)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	writeJSON(w, http.StatusOK, q)
}

// ListMailboxes lists the mailboxes of an account page by page.
func (s *Server) ListMailboxes(w http.ResponseWriter, r *http.Request, account adminapi.Account, p adminapi.ListMailboxesParams) {
	limit := defaultMailboxPage
	if p.Limit != nil {
		limit = *p.Limit
	}

	if limit < 1 || limit > maxMailboxPage {
		auditf(r, "account", account)
		writeError(w, http.StatusBadRequest, adminapi.BadRequest, "limit must be 1 to 1000")

		return
	}

	u, ok := s.openAccount(w, r, account, true)
	if !ok {
		return
	}
	defer u.Close()

	boxes, scanned, err := mailboxPage(u, p, limit+1)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	page := adminapi.MailboxPage{Items: []adminapi.MailboxSummary{}}

	// The page ends at the limit-th registry entry read, readable or not,
	// so an unreadable entry neither ends the listing nor shifts pages.
	if len(scanned) > limit {
		next := scanned[limit-1]
		page.Next = &next

		i := sort.Search(len(boxes), func(i int) bool { return boxes[i].Name > next })
		boxes = boxes[:i]
	}

	for i := range boxes {
		info, err := u.Info(boxes[i].Name)
		if errors.Is(err, model.ErrNoMailbox) {
			continue // deleted meanwhile
		}

		if err != nil {
			s.importError(w, r, err)
			return
		}

		page.Items = append(page.Items, mailboxSummary(info))
	}

	writeJSON(w, http.StatusOK, page)
}

// mailboxPage reads up to n registry entries after p.After (or the one
// named p.Name) and returns the readable mailboxes and the names read.
func mailboxPage(u model.User, p adminapi.ListMailboxesParams, n int) ([]model.Mailbox, []string, error) {
	if p.Name != nil {
		mb, err := u.Lookup(*p.Name)
		if errors.Is(err, model.ErrNoMailbox) {
			return nil, nil, nil
		}

		if err != nil {
			return nil, nil, err
		}

		return []model.Mailbox{*mb}, []string{mb.Name}, nil
	}

	after := deref(p.After)

	if pager, ok := u.(model.MailboxPager); ok {
		return pager.ListMailboxesAfter(after, n)
	}

	all, err := u.ListMailboxes()
	if err != nil {
		return nil, nil, err
	}

	i := sort.Search(len(all), func(i int) bool { return all[i].Name > after })
	all = all[i:]
	all = all[:min(n, len(all))]

	names := make([]string, len(all))
	for i := range all {
		names[i] = all[i].Name
	}

	return all, names, nil
}

// mailboxSummary is the API view of a mailbox header.
func mailboxSummary(info *model.MailboxInfo) adminapi.MailboxSummary {
	uses := info.SpecialUse
	if uses == nil {
		uses = []string{}
	}

	return adminapi.MailboxSummary{Id: info.ID, Name: info.Name, Uidvalidity: int64(info.UIDValidity), UidNext: int64(info.UIDNext),
		HighestModseq: int64(info.HighestModSeq), ShardSize: int64(info.ShardSize), Subscribed: info.Subscribed,
		SpecialUse: uses, AclEntries: len(info.ACL)}
}

// mailboxByID resolves a mailbox of u by its stable id.
func mailboxByID(u model.User, id string) (*model.Mailbox, error) {
	if imp, ok := u.(model.Importer); ok {
		return imp.MailboxByID(id)
	}

	all, err := u.ListMailboxes()
	if err != nil {
		return nil, err
	}

	for i := range all {
		if all[i].ID == id {
			return &all[i], nil
		}
	}

	return nil, model.ErrNoMailbox
}

// openMailbox opens the canonical account and resolves the mailbox id;
// the caller closes the user.
func (s *Server) openMailbox(w http.ResponseWriter, r *http.Request, account, id string) (model.User, *model.MailboxInfo, bool) {
	u, ok := s.openAccount(w, r, account, true)
	if !ok {
		return nil, nil, false
	}

	auditf(r, "mailbox_id", id)

	mb, err := mailboxByID(u, id)
	if err == nil {
		var info *model.MailboxInfo
		if info, err = u.Info(mb.Name); err == nil {
			return u, info, true
		}
	}

	u.Close()
	s.importError(w, r, err)

	return nil, nil, false
}

// GetMailbox shows one mailbox with counts over all its messages.
func (s *Server) GetMailbox(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID) {
	u, info, ok := s.openMailbox(w, r, account, id)
	if !ok {
		return
	}
	defer u.Close()

	msgs, err := u.ListMessages(info.ID)
	if err != nil {
		s.importError(w, r, err)
		return
	}

	out := adminapi.MailboxDetail{Mailbox: mailboxSummary(info), Messages: int64(len(msgs)), LogStart: int64(info.LogStart),
		RecentFrom: int64(info.RecentFrom), Keywords: info.Keywords}

	if out.Keywords == nil {
		out.Keywords = []string{}
	}

	for i := range msgs {
		out.SizeBytes += msgs[i].Size

		if !model.HasFlag(msgs[i].Flags, flagSeen) {
			out.Unseen++
		}

		if model.HasFlag(msgs[i].Flags, flagDeleted) {
			out.Deleted++
		}
	}

	if info.ShardSize > 0 && info.UIDNext > 1 {
		out.Shards = int64((info.UIDNext-2)/info.ShardSize) + 1
	}

	writeJSON(w, http.StatusOK, out)
}

// GetMailboxACL shows the explicit ACL entries of a mailbox.
func (s *Server) GetMailboxACL(w http.ResponseWriter, r *http.Request, account adminapi.Account, id adminapi.MailboxID) {
	u, info, ok := s.openMailbox(w, r, account, id)
	if !ok {
		return
	}
	defer u.Close()

	out := adminapi.MailboxACL{Owner: account, Mailbox: info.Name, Generation: int64(info.ACLGen), Entries: []adminapi.ACLEntry{}}

	for _, ident := range sortedIdentifiers(info.ACL) {
		out.Entries = append(out.Entries, adminapi.ACLEntry{Identifier: ident, Rights: info.ACL[ident]})
	}

	writeJSON(w, http.StatusOK, out)
}

func sortedIdentifiers(acl map[string]string) []string {
	out := make([]string, 0, len(acl))
	for k := range acl {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// ListSharedMailboxes lists the shared-mailbox pointers of an account.
func (s *Server) ListSharedMailboxes(w http.ResponseWriter, r *http.Request, account adminapi.Account) {
	u, ok := s.openAccount(w, r, account, true)
	if !ok {
		return
	}
	defer u.Close()

	refs, err := u.ListShared()
	if err != nil {
		s.importError(w, r, err)
		return
	}

	out := adminapi.SharedList{Account: account, Items: []adminapi.SharedRef{}}
	for _, ref := range refs {
		out.Items = append(out.Items, adminapi.SharedRef{Owner: ref.Owner, Mailbox: ref.Mailbox, Subscribed: ref.Subscribed, Generation: int64(ref.Gen)})
	}

	sort.Slice(out.Items, func(i, j int) bool {
		a, b := out.Items[i], out.Items[j]
		return a.Owner < b.Owner || (a.Owner == b.Owner && a.Mailbox < b.Mailbox)
	})

	writeJSON(w, http.StatusOK, out)
}
