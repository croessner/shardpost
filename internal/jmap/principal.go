package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"shardpost/internal/auth"
	"shardpost/internal/model"
	"shardpost/internal/sharenotice"
)

// Principals (RFC 9670 2). A principal is an account of this server, an
// individual; its id is the canonical account name, the same string that
// is its account id (see accountID), the ACL identifier of RFC 4314 and,
// where it is an address, its email. So a principal id says nothing a
// caller that holds it does not already know.
//
// There is no directory listing behind this, deliberately. The
// authentication backend resolves one address at a time (Lookup), and
// the principals a user can see are:
//
//   - the user itself;
//   - the owners who share mailboxes with it (its pointers, as far as the
//     session lists shared accounts);
//   - the identifiers in the ACLs of its own mailboxes;
//   - an account named exactly, by an email condition of Principal/query
//     or an id of Principal/get or shareWith, that the backend resolves
//     to that very account on this shard. An alias, an unknown address
//     and an account of another shard are all no result, after the same
//     lookup. Such lookups are bounded per request.
//
// Nothing searches the directory by a part of a name. The Principal and
// ShareNotification objects are kept in the personal account, which is
// the account of the principals capability.

// The capabilities of RFC 9670 and of JMAP Mail Sharing
// (draft-ietf-jmap-mail-sharing): principals is offered in the session;
// principals:owner only ever names the owner of an account; mail:share
// says that Mailbox has shareWith and myRights has mayShare.
const (
	CapPrincipals      = "urn:ietf:params:jmap:principals"
	CapPrincipalsOwner = "urn:ietf:params:jmap:principals:owner"
	CapMailShare       = "urn:ietf:params:jmap:mail:share"
)

// PrincipalOptions configure principals and share notifications.
type PrincipalOptions struct {
	// MaxLookups bounds the accounts one API request may look up in the
	// authentication backend by an exact address.
	MaxLookups int
	// MaxVisible bounds the principals a user sees without a lookup.
	MaxVisible int
	// MissPerMinute is how many lookups of addresses that are no
	// principal here one account may make per minute, across requests.
	MissPerMinute int
	// Notices writes the share notifications of the grantees whose
	// rights a shareWith changes, and bounds what an account keeps.
	Notices *sharenotice.Recorder
}

// The bounds a configuration that says nothing gets.
const (
	defaultMaxPrincipalLookups = 5
	defaultMaxPrincipals       = 1000
	// principalLookupTimeout bounds one lookup of the backend.
	principalLookupTimeout = 5 * time.Second
	// maxPrincipalID is the longest id RFC 8620 1.2 allows.
	maxPrincipalID = 255
)

// principalDefaults fills in the bounds left at zero; the notices are
// written into the grantees' namespaces through the front-end's store.
func principalDefaults(o PrincipalOptions, store Store) PrincipalOptions {
	if o.MaxLookups <= 0 {
		o.MaxLookups = defaultMaxPrincipalLookups
	}

	if o.MaxVisible <= 0 {
		o.MaxVisible = defaultMaxPrincipals
	}

	if o.Notices == nil && store != nil {
		o.Notices = &sharenotice.Recorder{Store: store}
	}

	return o
}

// The Principal properties.
const (
	propPrincipalType = "type"
	propTimeZone      = "timeZone"
	propCapabilities  = "capabilities"
	propAccounts      = "accounts"
	// principalIndividual is the one type of principal here: an account.
	principalIndividual = "individual"
)

// principalProperties are the properties of a Principal.
var principalProperties = map[string]bool{
	propID: true, propPrincipalType: true, propName: true, propDescription: true, propEmail: true,
	propTimeZone: true, propCapabilities: true, propAccounts: true,
}

// isPrincipalID reports whether an id can name a principal: an ACL
// identifier of an account — not "anyone", not a negative or reserved
// one — within the length of a JMAP id and without spaces or control
// characters.
func isPrincipalID(id string) bool {
	return id != "" && len(id) <= maxPrincipalID && sharenotice.Notifies(id) &&
		strings.IndexFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

// emailOf is the email of a principal: its account name where that is an
// address, null otherwise.
func emailOf(id string) any {
	if strings.Contains(id, "@") {
		return id
	}

	return nil
}

// principalCache is what one API request learned about principals: the
// lookups it made, the principals the user sees, and the shared accounts
// opened for Principal.accounts.
type principalCache struct {
	lookups int
	found   map[string]bool
	base    *visibleSet
	opened  map[string]*account
}

// pcache is the request's principal cache.
func (s *session) pcache() *principalCache {
	if s.principals == nil {
		s.principals = &principalCache{}
	}

	if s.principals.found == nil {
		s.principals.found = map[string]bool{}
		s.principals.opened = map[string]*account{}
	}

	return s.principals
}

// ownView is the store view of the user's own namespace.
func (s *session) ownView() model.User {
	if s.own != nil {
		return s.own
	}

	return s.user
}

// visibleSet is the principals a user sees without a lookup, in id order
// with the user first.
type visibleSet struct {
	ids  []string
	has  map[string]bool
	more bool
}

// visiblePrincipals reads the principals the user sees without a lookup,
// once per request.
func (s *Server) visiblePrincipals(sess *session) (*visibleSet, error) {
	c := sess.pcache()
	if c.base != nil {
		return c.base, nil
	}

	cand := map[string]bool{}
	own := sess.ownView()

	if s.o.Sharing.SharedPrefix != "" {
		if err := s.sharingOwners(own, sess.account, cand); err != nil {
			return nil, err
		}
	}

	boxes, err := own.ListMailboxes()
	if err != nil {
		return nil, err
	}

	for i := range boxes {
		for id := range boxes[i].ACL {
			if id != sess.account && isPrincipalID(id) {
				cand[id] = true
			}
		}
	}

	others := make([]string, 0, len(cand))
	for id := range cand {
		others = append(others, id)
	}

	sort.Strings(others)

	vs := &visibleSet{has: map[string]bool{sess.account: true}}

	if limit := s.o.Principals.MaxVisible - 1; len(others) > limit {
		s.logOnce("principals:"+sess.account, "jmap: principals cut at jmap.max_principals",
			"limit", s.o.Principals.MaxVisible)

		others, vs.more = others[:max(limit, 0)], true
	}

	vs.ids = append([]string{sess.account}, others...)

	for _, id := range others {
		vs.has[id] = true
	}

	c.base = vs

	return vs, nil
}

// sharingOwners adds the owners who share mailboxes with the user — the
// ones its pointers name that still have a pointer — as far as the
// session resource looks for shared accounts.
func (s *Server) sharingOwners(own model.User, login string, into map[string]bool) error {
	for owner, steps := "", 0; steps < s.o.Sharing.MaxAccounts; steps++ {
		next, ok, err := model.NextSharedOwner(own, owner)
		if err != nil {
			return err
		}

		if !ok || next == owner {
			return nil
		}

		owner = next

		if owner == login || !isPrincipalID(owner) {
			continue
		}

		refs, _, err := model.ListSharedN(own, owner, 1)
		if err != nil {
			return err
		}

		if len(refs) > 0 {
			into[owner] = true
		}
	}

	return nil
}

// withFound is the visible set and the principals this request found by
// a lookup, in id order with the user first.
func (c *principalCache) withFound(vs *visibleSet) []string {
	out := append([]string(nil), vs.ids...)

	var extra []string

	for id, ok := range c.found {
		if ok && !vs.has[id] {
			extra = append(extra, id)
		}
	}

	sort.Strings(extra)

	return append(out, extra...)
}

// principalState is the state of the principals: a hash of the visible
// set and of what the shared accounts are made of (the accounts property
// follows them). It changes whenever the set can; a /changes between two
// states is not computed.
func (s *Server) principalState(sess *session, vs *visibleSet) string {
	h := sha256.New()

	for _, id := range vs.ids {
		_, _ = h.Write([]byte(id + "\n"))
	}

	_, _ = h.Write([]byte(strconv.FormatBool(vs.more) + "\n" + s.sharingFingerprint(sess)))

	return "p" + hex.EncodeToString(h.Sum(nil)[:8])
}

// descDirectoryDown is the answer when the backend cannot be asked.
const descDirectoryDown = "the directory cannot be asked right now"

// errLookupBudget is a lookup beyond jmap.max_principal_lookups.
var errLookupBudget = errors.New("the request made more principal lookups than jmap.max_principal_lookups")

// errMissBudget is a lookup refused because the account missed too often
// lately (see missBudget).
var errMissBudget = errors.New("too many lookups of addresses that are no principal here; try again later")

// lookupPrincipal resolves an id the user does not see in the
// authentication backend: it is a principal when the backend knows the
// address as that very account and the account lives on this shard. An
// alias, an unknown address and an account of another shard all make
// the same lookup and are no result. The answer is kept for the request.
//
// A lookup is bounded per request (jmap.max_principal_lookups) and its
// misses per account across requests (missBudget); it goes to the
// backend with the client's address, so that Nauthilus can account for
// it, and it is logged.
func (s *Server) lookupPrincipal(ctx context.Context, sess *session, id string) (bool, error) {
	c := sess.pcache()
	if found, ok := c.found[id]; ok {
		return found, nil
	}

	if !isPrincipalID(id) || id != strings.ToLower(id) || !strings.Contains(id, "@") {
		return false, nil
	}

	if c.lookups >= s.o.Principals.MaxLookups {
		return false, errLookupBudget
	}

	if !s.misses.allow(sess.account) {
		s.logLookup(sess, id, "refused: miss budget")

		return false, errMissBudget
	}

	c.lookups++

	ctx, cancel := context.WithTimeout(ctx, principalLookupTimeout)
	defer cancel()

	var client net.Addr
	if sess.remote != "" {
		client = &addr{s: sess.remote}
	}

	res, err := s.o.Auth.Lookup(ctx, id, "jmap", client)

	result := s.lookupResult(id, res, err)
	if result == lookupFailed {
		s.logLookup(sess, id, result)

		return false, err
	}

	found := result == lookupFound
	if !found {
		s.misses.miss(sess.account)
	}

	s.logLookup(sess, id, result)
	c.found[id] = found

	return found, nil
}

// The results of a principal lookup, as they are logged.
const (
	lookupFound   = "found"
	lookupUnknown = "unknown"
	lookupAlias   = "alias"
	lookupForeign = "other shard"
	lookupFailed  = "failed"
)

// lookupResult classifies the backend's answer to a lookup of id.
func (s *Server) lookupResult(id string, res *auth.Result, err error) string {
	switch {
	case errors.Is(err, auth.ErrInvalid):
		return lookupUnknown
	case err != nil:
		return lookupFailed
	case strings.ToLower(strings.TrimSpace(res.Account)) != id:
		return lookupAlias
	}

	if here, _ := s.o.Shard.serves(res); !here {
		return lookupForeign
	}

	return lookupFound
}

// logLookup logs one principal lookup: at Info with a hash of the
// address — addresses are personal data; the hash lets an operator tell
// repeated lookups of one address apart from many, and compare with an
// address it suspects — and at Debug with the address itself.
func (s *Server) logLookup(sess *session, address, result string) {
	sum := sha256.Sum256([]byte(address))
	hash := hex.EncodeToString(sum[:8])

	sess.log.Info("jmap: principal lookup", "address_sha256", hash, "result", result)
	sess.log.Debug("jmap: principal lookup address", "address_sha256", hash, "address", address)
}

// knownPrincipal reports whether an id names a principal the user may
// see, looking it up when it is not in the visible set.
func (s *Server) knownPrincipal(ctx context.Context, sess *session, id string) (bool, error) {
	vs, err := s.visiblePrincipals(sess)
	if err != nil {
		return false, err
	}

	if vs.has[id] {
		return true, nil
	}

	return s.lookupPrincipal(ctx, sess, id)
}

// lookupError is the method error of a failed lookup.
func lookupError(sess *session, err error) *methodError {
	switch {
	case errors.Is(err, errLookupBudget):
		return &methodError{Type: errRequestTooLarge, Description: err.Error()}
	case errors.Is(err, errMissBudget):
		// RFC 8620 has no method-level rateLimit; serverUnavailable is
		// the error a client retries later.
		return &methodError{Type: errServerUnavailable, Description: err.Error()}
	}

	sess.log.Warn("jmap: principal lookup failed", "err", err)

	return &methodError{Type: errServerUnavailable, Description: descDirectoryDown}
}

// principalAccounts is Principal.accounts: the accounts of the principal
// the user can reach — its own personal account, or the shared account
// of an owner who shares with it — or nil.
func (s *Server) principalAccounts(sess *session, id string) (map[string]Account, error) {
	if id == sess.account {
		return map[string]Account{accountID(id): s.account(id)}, nil
	}

	if s.o.Sharing.SharedPrefix == "" {
		return nil, nil
	}

	c := sess.pcache()

	a, seen := c.opened[id]
	if !seen {
		var err error
		if a, err = s.openShared(sess, id); err != nil {
			return nil, err
		}

		c.opened[id] = a
	}

	if a == nil {
		return nil, nil
	}

	return map[string]Account{a.id: s.sharedAccountObject(a)}, nil
}

// principalObject renders a principal with the properties asked for. Its
// name is its account name: display names are the directory's, and a
// principal looks the same whoever asks and however it was found.
func (s *Server) principalObject(sess *session, id string, props *[]string) (map[string]any, error) {
	out := map[string]any{propID: id}

	put := func(key string, value any) {
		if selected(props, key) {
			out[key] = value
		}
	}

	put(propPrincipalType, principalIndividual)
	put(propName, id)
	put(propDescription, nil)
	put(propEmail, emailOf(id))
	put(propTimeZone, nil)
	put(propCapabilities, map[string]any{})

	if selected(props, propAccounts) {
		accounts, err := s.principalAccounts(sess, id)
		if err != nil {
			return nil, err
		}

		if accounts == nil {
			out[propAccounts] = nil
		} else {
			out[propAccounts] = accounts
		}
	}

	return out, nil
}

// principalGet implements Principal/get (RFC 9670 2.1). Without ids it
// returns the principals the user sees; an id it does not see is looked
// up, as an exact email condition of Principal/query is.
func principalGet(ctx context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req getRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if merr := checkRequested(req.Properties, principalProperties); merr != nil {
		return nil, merr
	}

	s := sess.srv

	vs, err := s.visiblePrincipals(sess)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	want := vs.ids
	if req.IDs != nil {
		want = *req.IDs
	}

	if len(want) > sess.limits.MaxObjectsInGet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyIDs}
	}

	out := &getResponse{AccountID: sess.accountID(), State: s.principalState(sess, vs),
		List: []map[string]any{}, NotFound: []string{}}
	done := map[string]bool{}

	for _, id := range want {
		if done[id] {
			continue
		}

		done[id] = true

		known, err := s.knownPrincipal(ctx, sess, id)
		if err != nil {
			return nil, lookupError(sess, err)
		}

		if !known {
			out.NotFound = append(out.NotFound, id)
			continue
		}

		obj, err := s.principalObject(sess, id, req.Properties)
		if err != nil {
			return nil, &methodError{Type: errServerFail, Description: err.Error()}
		}

		out.List = append(out.List, obj)
	}

	return out, nil
}

// principalChanges implements Principal/changes (RFC 9670 2.2). The set
// a user sees is computed, not logged, so there is no delta to give: the
// current state has none, any other is cannotCalculateChanges, which the
// RFC foresees for a server backed by a directory.
func principalChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req changesRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if _, merr := changeLimit(req.MaxChanges); merr != nil {
		return nil, merr
	}

	s := sess.srv

	vs, err := s.visiblePrincipals(sess)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	state := s.principalState(sess, vs)
	if req.SinceState != state {
		return nil, &methodError{Type: errCannotCalculateChanges,
			Description: "principals are not logged; fetch them again"}
	}

	return &changesResponse{AccountID: sess.accountID(), OldState: state, NewState: state,
		Created: []string{}, Updated: []string{}, Destroyed: []string{}}, nil
}

// principalSet implements Principal/set (RFC 9670 2.3): principals
// are the directory's, and every change is forbidden.
func principalSet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req setRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	s := sess.srv

	vs, err := s.visiblePrincipals(sess)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	state := s.principalState(sess, vs)
	if req.IfInState != nil && *req.IfInState != state {
		return nil, &methodError{Type: errStateMismatch}
	}

	out := newSetResponse(sess.accountID(), state)
	refuse := &setError{Type: setForbidden, Description: "principals are managed in the directory"}

	for cid := range req.Create {
		out.NotCreated[cid] = refuse
	}

	for id := range req.Update {
		out.NotUpdated[id] = refuse
	}

	for _, id := range req.Destroy {
		out.NotDestroyed[id] = refuse
	}

	return out, nil
}

// principalFilter is the filter of a Principal/query (RFC 9670 2.4.1).
type principalFilter struct {
	AccountIDs *[]string         `json:"accountIds"`
	Email      *string           `json:"email"`
	Name       *string           `json:"name"`
	Text       *string           `json:"text"`
	Type       *string           `json:"type"`
	TimeZone   *string           `json:"timeZone"`
	Operator   string            `json:"operator"`
	Conditions []principalFilter `json:"conditions"`
}

// principalFilterKeys are the properties a principal filter may have.
var principalFilterKeys = map[string]bool{
	"accountIds": true, propEmail: true, propName: true, "text": true, propPrincipalType: true,
	propTimeZone: true, filterOperator: true, filterConditions: true,
}

// principalSortKeys are the properties a Principal/query sorts by. RFC
// 9670 names none; these are the ones a principal here has.
var principalSortKeys = map[string]bool{propName: true, propEmail: true}

// operator implements opFilter.
func (f *principalFilter) operator() string { return f.Operator }

// conditionCount implements opFilter.
func (f *principalFilter) conditionCount() int { return len(f.Conditions) }

// principalChild is the i-th filter below an operator.
func principalChild(f *principalFilter, i int) *principalFilter { return &f.Conditions[i] }

// emails collects the email conditions of a filter, the addresses a
// query looks up.
func (f *principalFilter) emails(into map[string]bool) {
	if f.Email != nil {
		into[strings.ToLower(strings.TrimSpace(*f.Email))] = true
	}

	for i := range f.Conditions {
		f.Conditions[i].emails(into)
	}
}

// usesAccounts reports whether a filter has an accountIds condition,
// which needs Principal.accounts.
func (f *principalFilter) usesAccounts() bool {
	if f.AccountIDs != nil {
		return true
	}

	for i := range f.Conditions {
		if f.Conditions[i].usesAccounts() {
			return true
		}
	}

	return false
}

// principalView is what a filter condition reads of a principal.
type principalView struct {
	id       string
	accounts map[string]Account
}

// containsFold is a substring match without regard to ASCII case.
func containsFold(s, sub string) bool {
	return strings.Contains(collate(collationCasemap, s), collate(collationCasemap, sub))
}

// condition evaluates a filter condition: every property given has to
// hold. email, name and text are substring matches over what the user
// sees (RFC 9670 2.4.1); description and timeZone are null here.
func (f *principalFilter) condition(p *principalView) bool {
	email, _ := emailOf(p.id).(string)

	switch {
	case f.Email != nil && (email == "" || !containsFold(email, *f.Email)):
		return false
	case f.Name != nil && !containsFold(p.id, *f.Name):
		return false
	case f.Text != nil && !containsFold(p.id, *f.Text):
		return false
	case f.Type != nil && *f.Type != principalIndividual:
		return false
	case f.TimeZone != nil:
		return false
	}

	return f.AccountIDs == nil || p.hasAccount(*f.AccountIDs)
}

// hasAccount reports whether any of the ids is one of the principal's
// accounts.
func (p *principalView) hasAccount(ids []string) bool {
	for _, id := range ids {
		if _, ok := p.accounts[id]; ok {
			return true
		}
	}

	return false
}

// readPrincipalFilter decodes and checks a Principal/query filter; null
// is none.
func readPrincipalFilter(raw json.RawMessage) (*principalFilter, *methodError) {
	if len(raw) == 0 || string(raw) == jsonNull {
		return nil, nil
	}

	var f principalFilter
	if merr := decodeOpFilter(raw, &f, principalFilterKeys, "a principal", principalChild); merr != nil {
		return nil, merr
	}

	return &f, nil
}

// principalQuery implements Principal/query (RFC 9670 2.4) over the
// principals the user sees and the ones its email conditions name: each
// such address that the user does not see already is looked up, exactly
// and nothing else.
func principalQuery(ctx context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req queryRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	filter, merr := readPrincipalFilter(req.Filter)
	if merr != nil {
		return nil, merr
	}

	if merr := checkSort(req.Sort, principalSortKeys); merr != nil {
		return nil, merr
	}

	s := sess.srv

	vs, err := s.visiblePrincipals(sess)
	if err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	if filter != nil {
		addrs := map[string]bool{}
		filter.emails(addrs)

		for _, a := range sortedKeys(addrs) {
			if _, err := s.knownPrincipal(ctx, sess, a); err != nil {
				return nil, lookupError(sess, err)
			}
		}
	}

	matched, merr := s.matchPrincipals(sess, sess.pcache().withFound(vs), filter)
	if merr != nil {
		return nil, merr
	}

	sortPrincipals(matched, req.Sort)

	return page(sess.accountID(), s.principalState(sess, vs), matched, &req.paging)
}

// matchPrincipals evaluates a filter over the candidates.
func (s *Server) matchPrincipals(sess *session, cand []string, filter *principalFilter) ([]string, *methodError) {
	if filter == nil {
		return cand, nil
	}

	accounts := filter.usesAccounts()
	out := []string{}

	for _, id := range cand {
		p := &principalView{id: id}

		if accounts {
			a, err := s.principalAccounts(sess, id)
			if err != nil {
				return nil, &methodError{Type: errServerFail, Description: err.Error()}
			}

			p.accounts = a
		}

		if matchOpFilter(filter, func(c *principalFilter) bool { return c.condition(p) }, principalChild) {
			out = append(out, id)
		}
	}

	return out, nil
}

// sortPrincipals applies the comparators, last key first, over the ids
// in their order. Name and email are both the account name; a principal
// without an address sorts first by email.
func sortPrincipals(ids []string, by []comparator) {
	for i := len(by) - 1; i >= 0; i-- {
		c := by[i]

		key := func(id string) string {
			if c.Property == propEmail {
				e, _ := emailOf(id).(string)

				return collate(c.Collation, e)
			}

			return collate(c.Collation, id)
		}

		sort.SliceStable(ids, func(a, b int) bool {
			if !c.ascending() {
				return key(ids[b]) < key(ids[a])
			}

			return key(ids[a]) < key(ids[b])
		})
	}
}

// principalQueryChanges implements Principal/queryChanges (RFC 9670 2.5):
// as for Principal/changes, there is no delta to compute, which the query
// announced with canCalculateChanges false.
func principalQueryChanges(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req queryRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	return nil, &methodError{Type: errCannotCalculateChanges,
		Description: "principal queries are not logged; run the query again"}
}
