package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/model"
)

// Identity (RFC 8621 6). The addresses an account may send from are the
// user database's business, and this server asks for them the way it asks
// everything else about an account: through the authentication backend's
// identity lookup (Nauthilus LookupIdentity), without a password, so that
// it works the same whatever the backend behind Nauthilus is. Which
// attributes of the answer carry the addresses is configured.
//
// Every address gets an identity of its own that cannot be deleted. A
// client may change its name, reply-to, bcc and signatures, and may add
// further identities for the same addresses; what a client changed is
// kept in the account's own metadata (METADATA server entries).

// CapSubmission is the capability Identity and EmailSubmission belong
// to (RFC 8621 1.3.2).
const CapSubmission = "urn:ietf:params:jmap:submission"

// SubmissionOptions configure the submission capability. Without them it
// is not offered.
type SubmissionOptions struct {
	// Relay takes the messages; KV keeps the submission records.
	Relay Submitter
	KV    model.AdminKV
	// AddressAttributes are the attributes of the identity lookup that
	// carry the addresses the account may send from.
	AddressAttributes []string
	// XClient names the user to the relay with XCLIENT; without it the
	// relay's own credentials are used and CheckSender is all there is.
	XClient bool
	// CheckSender refuses an envelope sender or a From that is not one
	// of the account's addresses, before the relay is asked.
	CheckSender bool
	// MaxRecipients bounds the recipients of one submission, MaxSize the
	// size of the message sent.
	MaxRecipients int
	MaxSize       int64
	// Retention and MaxKept bound the submission records an account keeps.
	Retention time.Duration
	MaxKept   int
}

// DefaultSubmission are the bounds a configuration that says nothing gets.
var DefaultSubmission = SubmissionOptions{XClient: true, CheckSender: true, MaxRecipients: 100,
	Retention: 7 * 24 * time.Hour, MaxKept: 500}

// submissionDefaults fills in the bounds left at zero.
func submissionDefaults(o *SubmissionOptions, uploadMax int64) *SubmissionOptions {
	if o == nil {
		return nil
	}

	c := *o
	if c.MaxRecipients <= 0 {
		c.MaxRecipients = DefaultSubmission.MaxRecipients
	}

	if c.MaxSize <= 0 {
		c.MaxSize = uploadMax
	}

	if c.Retention <= 0 {
		c.Retention = DefaultSubmission.Retention
	}

	if c.MaxKept <= 0 {
		c.MaxKept = DefaultSubmission.MaxKept
	}

	if len(c.AddressAttributes) == 0 {
		c.AddressAttributes = []string{"mail"}
	}

	return &c
}

// identityEntry is the metadata entry the identities a client changed or
// added are kept in.
const identityEntry = "/private/vendor/shardpost/jmap/identities"

// Bounds of what a client may keep: identities per account, and the size
// of one signature. The whole record has to fit a metadata entry.
const (
	maxIdentities     = 32
	maxSignatureBytes = 16 << 10
	maxIdentityName   = 255
	// maxIdentityRecord keeps the stored record below the 64 KiB a
	// metadata entry may hold, with room to spare.
	maxIdentityRecord = 48 << 10
)

// descIdentitiesFull is the error of a change that would not fit.
const descIdentitiesFull = "the identities of this account would not fit what it may keep"

// The properties of an Identity.
const (
	propTextSignature = "textSignature"
	propHTMLSignature = "htmlSignature"
	propMayDelete     = "mayDelete"
	propEmail         = "email"
)

// identity is an Identity object, and what is kept of one.
type identity struct {
	ReplyTo       []emailAddress `json:"replyTo"`
	Bcc           []emailAddress `json:"bcc"`
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Email         string         `json:"email"`
	TextSignature string         `json:"textSignature"`
	HTMLSignature string         `json:"htmlSignature"`
	MayDelete     bool           `json:"mayDelete"`
}

// identityBook is the account's identities: the ones its addresses give
// it, with what a client changed, and the ones a client added.
type identityBook struct {
	list    []*identity
	allowed map[string]bool // lower-cased addresses the account may use
	stored  map[string]*identity
}

// submissionOf is the submission configuration of the session's server.
func submissionOf(sess *session) (*SubmissionOptions, *methodError) {
	if sess.srv == nil || sess.srv.o.Submission == nil {
		return nil, &methodError{Type: errUnknownMethod}
	}

	return sess.srv.o.Submission, nil
}

// baseIdentityID is the id of the identity an address gives an account;
// it stays the same as long as the address does.
func baseIdentityID(addr string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(addr)))

	return "I" + hex.EncodeToString(sum[:8])
}

// loadIdentities asks the backend for the account's addresses and puts
// the stored changes on top.
func loadIdentities(ctx context.Context, sess *session) (*identityBook, *methodError) {
	opts, merr := submissionOf(sess)
	if merr != nil {
		return nil, merr
	}

	res, err := sess.srv.o.Auth.Lookup(ctx, sess.account, "jmap", nil)

	switch {
	case errors.Is(err, auth.ErrInvalid):
		sess.log.Warn("jmap: the identity lookup does not know the account; it has no addresses to send from")

		res = &auth.Result{Account: sess.account}
	case err != nil:
		return nil, &methodError{Type: errServerFail, Description: "the identity lookup failed: " + err.Error()}
	}

	b := &identityBook{allowed: map[string]bool{}, stored: map[string]*identity{}}

	for _, addr := range addressesOf(res, opts.AddressAttributes) {
		b.allowed[strings.ToLower(addr)] = true
		b.list = append(b.list, &identity{ID: baseIdentityID(addr), Name: res.DisplayName, Email: addr})
	}

	if err := b.readStored(sess); err != nil {
		return nil, &methodError{Type: errServerFail, Description: err.Error()}
	}

	return b, nil
}

// addressesOf are the addresses in the configured attributes, each once,
// in the order the backend gave them.
func addressesOf(res *auth.Result, attrs []string) []string {
	var out []string

	seen := map[string]bool{}

	for _, a := range attrs {
		// The attributes keep their configured order, so the primary
		// address comes first; the values of one attribute are sorted,
		// because a directory keeps no order among them.
		vals := append([]string(nil), res.Attributes[a]...)
		sort.Strings(vals)

		for _, v := range vals {
			v = strings.TrimSpace(v)
			if !strings.Contains(v, "@") || seen[strings.ToLower(v)] {
				continue
			}

			seen[strings.ToLower(v)] = true
			out = append(out, v)
		}
	}

	return out
}

// readStored applies the stored identities: changes to the ones the
// addresses give, and the added ones whose address is still allowed.
func (b *identityBook) readStored(sess *session) error {
	md, err := sess.user.GetMetadata("", sess.account)
	if err != nil {
		return err
	}

	var stored []*identity
	if raw := md[identityEntry]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &stored)
	}

	byID := map[string]*identity{}
	for _, id := range b.list {
		byID[id.ID] = id
	}

	for _, st := range stored {
		if !b.allowed[strings.ToLower(st.Email)] {
			continue
		}

		b.stored[st.ID] = st

		if base, ok := byID[st.ID]; ok {
			*base = *st
			base.MayDelete = false

			continue
		}

		st.MayDelete = true
		b.list = append(b.list, st)
	}

	sort.SliceStable(b.list, func(i, j int) bool { return !b.list[i].MayDelete && b.list[j].MayDelete })

	return nil
}

// record is the stored identities as they are written.
func (b *identityBook) record() ([]byte, error) {
	list := make([]*identity, 0, len(b.stored))
	for _, id := range sortedIdentityIDs(b.stored) {
		list = append(list, b.stored[id])
	}

	return json.Marshal(list)
}

// fits reports whether the stored identities, with i in place of its
// id's, still fit the metadata entry.
func (b *identityBook) fits(i *identity) bool {
	prev, had := b.stored[i.ID]
	b.stored[i.ID] = i

	raw, err := b.record()

	if had {
		b.stored[i.ID] = prev
	} else {
		delete(b.stored, i.ID)
	}

	return err == nil && len(raw) <= maxIdentityRecord
}

// save writes the stored identities back.
func (b *identityBook) save(sess *session) error {
	raw, err := b.record()
	if err != nil {
		return err
	}

	v := string(raw)

	return sess.user.SetMetadata("", sess.account, map[string]*string{identityEntry: &v})
}

// sortedIdentityIDs are the ids of a set of identities in order.
func sortedIdentityIDs(m map[string]*identity) []string {
	set := map[string]bool{}
	for id := range m {
		set[id] = true
	}

	return sortedKeys(set)
}

// state is the state of the identities: a hash of what Identity/get
// returns, since part of it lives in the user database and changes there
// without this server hearing of it.
func (b *identityBook) state() string {
	raw, _ := json.Marshal(b.list)
	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:12])
}

// find is the identity with the id.
func (b *identityBook) find(id string) *identity {
	for _, i := range b.list {
		if i.ID == id {
			return i
		}
	}

	return nil
}

// identityGet implements Identity/get.
func identityGet(ctx context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req getRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if merr := checkIdentityGet(sess, &req); merr != nil {
		return nil, merr
	}

	b, merr := loadIdentities(ctx, sess)
	if merr != nil {
		return nil, merr
	}

	out := &getResponse{AccountID: accountID(sess.account), State: b.state(), List: []map[string]any{}, NotFound: []string{}}

	ids := make([]string, 0, len(b.list))
	for _, i := range b.list {
		ids = append(ids, i.ID)
	}

	if req.IDs != nil {
		ids = *req.IDs
	}

	for _, id := range ids {
		i := b.find(id)
		if i == nil {
			out.NotFound = append(out.NotFound, id)
			continue
		}

		out.List = append(out.List, renderIdentity(i, req.Properties))
	}

	return out, nil
}

// identityProperties are the properties of an Identity.
var identityProperties = map[string]bool{
	propID: true, propName: true, propEmail: true, propReplyTo: true, propBcc: true,
	propTextSignature: true, propHTMLSignature: true, propMayDelete: true,
}

// checkIdentityGet bounds an Identity/get and refuses unknown properties.
func checkIdentityGet(sess *session, req *getRequest) *methodError {
	if req.IDs != nil && len(*req.IDs) > sess.limits.MaxObjectsInGet {
		return &methodError{Type: errRequestTooLarge, Description: descTooManyIDs}
	}

	if req.Properties != nil {
		for _, p := range *req.Properties {
			if !identityProperties[p] {
				return &methodError{Type: errInvalidArguments, Arguments: []string{argProperties}, Description: "no such property: " + p}
			}
		}
	}

	return nil
}

// renderIdentity is an identity with the properties asked for.
func renderIdentity(i *identity, props *[]string) map[string]any {
	all := map[string]any{
		propID: i.ID, propName: i.Name, propEmail: i.Email, propReplyTo: addressesOrNull(i.ReplyTo),
		propBcc: addressesOrNull(i.Bcc), propTextSignature: i.TextSignature, propHTMLSignature: i.HTMLSignature,
		propMayDelete: i.MayDelete,
	}

	if props == nil {
		return all
	}

	out := map[string]any{propID: i.ID}

	for _, p := range *props {
		if v, ok := all[p]; ok {
			out[p] = v
		}
	}

	return out
}

// addressesOrNull is an address list, or null for none.
func addressesOrNull(list []emailAddress) any {
	if len(list) == 0 {
		return nil
	}

	return list
}

// identityChanges implements Identity/changes. The state is a hash, so a
// client whose state is not the current one is told to fetch the list
// again; the list is short.
func identityChanges(ctx context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req changesRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	b, merr := loadIdentities(ctx, sess)
	if merr != nil {
		return nil, merr
	}

	if req.SinceState != b.state() {
		return nil, &methodError{Type: errCannotCalculateChanges,
			Description: "identity changes are not recorded; fetch the identities again"}
	}

	return &changesResponse{AccountID: accountID(sess.account), OldState: req.SinceState, NewState: req.SinceState,
		Created: []string{}, Updated: []string{}, Destroyed: []string{}}, nil
}

// identitySetRequest is Identity/set: a /set.
type identitySetRequest = setRequest

// identitySet implements Identity/set. A new identity has to use one of
// the account's addresses (forbiddenFrom otherwise); an update may change
// everything but the address; only an added identity may be destroyed.
func identitySet(ctx context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req identitySetRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if n := len(req.Create) + len(req.Update) + len(req.Destroy); n > sess.limits.MaxObjectsInSet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyObjects}
	}

	b, merr := loadIdentities(ctx, sess)
	if merr != nil {
		return nil, merr
	}

	state := b.state()
	if req.IfInState != nil && *req.IfInState != state {
		return nil, &methodError{Type: errStateMismatch}
	}

	out := newSetResponse(accountID(sess.account), state)

	b.apply(sess, &req, out)

	if len(out.Created)+len(out.Updated)+len(out.Destroyed) > 0 {
		if err := b.save(sess); err != nil {
			return nil, &methodError{Type: errServerFail, Description: err.Error()}
		}
	}

	out.NewState = b.state()

	return out, nil
}

// apply runs the creations, updates and destructions of a /set.
func (b *identityBook) apply(sess *session, req *identitySetRequest, out *setResponse) {
	for cid, raw := range req.Create {
		i, serr := b.create(raw)
		if serr != nil {
			out.NotCreated[cid] = serr
			continue
		}

		out.Created[cid] = map[string]any{propID: i.ID, propMayDelete: true}
		sess.created[cid] = i.ID
	}

	for ref, patch := range req.Update {
		id, _ := resolveCreationID(sess, ref)
		if serr := b.update(id, patch); serr != nil {
			out.NotUpdated[ref] = serr
			continue
		}

		out.Updated[id] = nil
	}

	for _, ref := range req.Destroy {
		id, _ := resolveCreationID(sess, ref)
		if serr := b.destroy(id); serr != nil {
			out.NotDestroyed[ref] = serr
			continue
		}

		out.Destroyed = append(out.Destroyed, id)
	}
}

// setForbiddenFrom is the set error of an address the account may not
// send from (RFC 8621 6.3).
const setForbiddenFrom = "forbiddenFrom"

// create adds an identity.
func (b *identityBook) create(raw json.RawMessage) (*identity, *setError) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	if _, ok := given[propEmail]; !ok {
		return nil, invalidProperty(propEmail, "an identity has an address")
	}

	i := &identity{ID: "I" + randomHex(8), MayDelete: true}

	if serr := applyIdentity(i, given, true); serr != nil {
		return nil, serr
	}

	if !b.allowed[strings.ToLower(i.Email)] {
		return nil, &setError{Type: setForbiddenFrom, Description: "this account may not send from " + i.Email}
	}

	if len(b.stored) >= maxIdentities {
		return nil, &setError{Type: setForbidden, Description: "this account has as many identities as it may keep"}
	}

	if !b.fits(i) {
		return nil, &setError{Type: setTooLarge, Description: descIdentitiesFull}
	}

	b.stored[i.ID] = i
	b.list = append(b.list, i)

	return i, nil
}

// update changes an identity.
func (b *identityBook) update(id string, patch map[string]json.RawMessage) *setError {
	i := b.find(id)
	if i == nil {
		return &setError{Type: setNotFound}
	}

	next := *i
	if serr := applyIdentity(&next, patch, false); serr != nil {
		return serr
	}

	if !b.fits(&next) {
		return &setError{Type: setTooLarge, Description: descIdentitiesFull}
	}

	*i = next
	b.stored[id] = i

	return nil
}

// destroy removes an added identity.
func (b *identityBook) destroy(id string) *setError {
	i := b.find(id)

	switch {
	case i == nil:
		return &setError{Type: setNotFound}
	case !i.MayDelete:
		return &setError{Type: setForbidden, Description: "an identity of one of the account's addresses stays"}
	}

	delete(b.stored, id)

	for n, x := range b.list {
		if x.ID == id {
			b.list = append(b.list[:n], b.list[n+1:]...)

			break
		}
	}

	return nil
}

// applyIdentity sets the properties given on an identity; email only on
// creation.
func applyIdentity(i *identity, given map[string]json.RawMessage, creating bool) *setError {
	for k, v := range given {
		var err error

		switch k {
		case propEmail:
			if !creating {
				return invalidProperty(k, "the address of an identity cannot change")
			}

			err = json.Unmarshal(v, &i.Email)
		case propName:
			err = json.Unmarshal(v, &i.Name)
		case propReplyTo:
			err = json.Unmarshal(v, &i.ReplyTo)
		case propBcc:
			err = json.Unmarshal(v, &i.Bcc)
		case propTextSignature:
			err = json.Unmarshal(v, &i.TextSignature)
		case propHTMLSignature:
			err = json.Unmarshal(v, &i.HTMLSignature)
		case propID, propMayDelete:
			return invalidProperty(k, "the server sets this property")
		default:
			return invalidProperty(k, "an identity has no property "+k)
		}

		if err != nil {
			return invalidProperty(k, err.Error())
		}
	}

	return checkIdentity(i)
}

// checkIdentity bounds what an identity may hold.
func checkIdentity(i *identity) *setError {
	switch {
	case len(i.Name) > maxIdentityName:
		return invalidProperty(propName, "the name is too long")
	case len(i.TextSignature) > maxSignatureBytes || len(i.HTMLSignature) > maxSignatureBytes:
		return invalidProperty(propTextSignature, "a signature is at most 16 KiB")
	case len(i.ReplyTo) > maxIdentities || len(i.Bcc) > maxIdentities:
		return invalidProperty(propReplyTo, "too many addresses")
	}

	for _, a := range append(append([]emailAddress(nil), i.ReplyTo...), i.Bcc...) {
		if !strings.Contains(a.Email, "@") || len(a.Email) > maxIdentityName || (a.Name != nil && len(*a.Name) > maxIdentityName) {
			return invalidProperty(propReplyTo, "not an address: "+a.Email)
		}
	}

	return nil
}
