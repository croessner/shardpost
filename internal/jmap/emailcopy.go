package jmap

import (
	"context"
	"encoding/json"
	"time"
)

// Email/copy (RFC 8621 4.7, RFC 8620 5.4) copies messages between two
// accounts the user can reach: the personal one and the accounts other
// namespaces share with it, in any pair. A message the user may read in
// the source (right r where it is) is read through the source's view and
// stored again in the destination — a namespace keeps its own blobs, so
// the copy is a new message with an id of its own, as an IMAP COPY into
// another namespace is. The destination checks what any new message
// there needs: i in every mailbox, and the right for each keyword ($seen
// s, every other w).

// emailCopyRequest is the argument shape of Email/copy.
type emailCopyRequest struct {
	Create                   map[string]json.RawMessage `json:"create"`
	IfFromInState            *string                    `json:"ifFromInState"`
	IfInState                *string                    `json:"ifInState"`
	DestroyFromIfInState     *string                    `json:"destroyFromIfInState"`
	FromAccountID            string                     `json:"fromAccountId"`
	AccountID                string                     `json:"accountId"`
	OnSuccessDestroyOriginal bool                       `json:"onSuccessDestroyOriginal"`
}

// copyResponse is what Email/copy answers, followed by the implicit
// Email/set of onSuccessDestroyOriginal when there is one.
type copyResponse struct {
	Created       map[string]map[string]any `json:"created"`
	NotCreated    map[string]*setError      `json:"notCreated"`
	FromAccountID string                    `json:"fromAccountId"`
	AccountID     string                    `json:"accountId"`
	OldState      string                    `json:"oldState"`
	NewState      string                    `json:"newState"`
	then          []invocation
}

func (r *copyResponse) followUps() []invocation { return r.then }

// copyable are the properties a copy may set (RFC 8621 4.7); everything
// else comes from the original.
var copyable = map[string]bool{propID: true, propMailboxIDs: true, propKeywords: true, propReceivedAt: true}

// emailCopy implements Email/copy. The call runs with the session of the
// destination (accountId); the source is resolved like any account, and
// one the user cannot reach is fromAccountNotFound.
func emailCopy(ctx context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req emailCopyRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if req.FromAccountID == "" || req.FromAccountID == sess.accountID() {
		return nil, &methodError{Type: errInvalidArguments, Arguments: []string{"fromAccountId"},
			Description: "fromAccountId names another account; within one account, change mailboxIds with Email/set"}
	}

	if len(req.Create) > sess.limits.MaxObjectsInSet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyObjects}
	}

	from, merr := sess.srv.resolveAccount(sess, req.FromAccountID)
	if merr != nil {
		if merr.Type == errAccountNotFound {
			merr = &methodError{Type: errFromAccountNotFound}
		}

		return nil, merr
	}

	c, merr := startCopy(sess, sess.with(from), &req)
	if merr != nil {
		return nil, merr
	}

	c.run(req.Create)
	c.out.NewState = reconcile(sess, c.out.OldState, c.mine, c.out.Created, nil, nil, c.expect)

	if req.OnSuccessDestroyOriginal && len(c.originals) > 0 {
		c.out.then = c.destroyOriginals(ctx, req.DestroyFromIfInState)
	}

	return c.out, nil
}

// copyRun is one Email/copy being carried out.
type copyRun struct {
	sess, from *session
	loc        *locator
	out        *copyResponse
	mine       map[string]bool
	expect     map[string]expectation
	originals  []string
}

// startCopy reads both states and checks the conditions on them.
func startCopy(sess, from *session, req *emailCopyRequest) (*copyRun, *methodError) {
	fromState, loc, merr := emailState(from)
	if merr != nil {
		return nil, merr
	}

	if req.IfFromInState != nil && *req.IfFromInState != fromState {
		return nil, &methodError{Type: errStateMismatch, Description: "ifFromInState"}
	}

	state, _, merr := emailState(sess)
	if merr != nil {
		return nil, merr
	}

	if req.IfInState != nil && *req.IfInState != state {
		return nil, &methodError{Type: errStateMismatch}
	}

	return &copyRun{sess: sess, from: from, loc: loc, mine: map[string]bool{}, expect: map[string]expectation{},
		out: &copyResponse{FromAccountID: from.accountID(), AccountID: sess.accountID(), OldState: state, NewState: state,
			Created: map[string]map[string]any{}, NotCreated: map[string]*setError{}}}, nil
}

// run carries out the creations, each on its own.
func (c *copyRun) run(create map[string]json.RawMessage) {
	for _, cid := range sortedRawKeys(create) {
		created, exp, original, serr := c.one(create[cid])
		if serr != nil {
			c.out.NotCreated[cid] = serr
			continue
		}

		id, _ := created[propID].(string)
		c.out.Created[cid] = created
		c.sess.created[cid] = id
		c.mine[id] = true
		c.expect[id] = exp
		c.originals = append(c.originals, original)
	}
}

// one copies one message and returns what a creation reports, what the
// copy is meant to have, and the id of the original.
func (c *copyRun) one(raw json.RawMessage) (map[string]any, expectation, string, *setError) {
	var given map[string]json.RawMessage
	if err := json.Unmarshal(raw, &given); err != nil {
		return nil, expectation{}, "", &setError{Type: setInvalidProperties, Description: err.Error()}
	}

	for key := range given {
		if !copyable[key] {
			return nil, expectation{}, "", invalidProperty(key, "a copy sets only mailboxIds, keywords and receivedAt")
		}
	}

	var ref string
	if err := json.Unmarshal(given[propID], &ref); err != nil || ref == "" {
		return nil, expectation{}, "", invalidProperty(propID, "a copy names the email it copies")
	}

	id, _ := resolveCreationID(c.from, ref)

	hits, err := c.loc.resolve([]string{id})
	if err != nil {
		return nil, expectation{}, "", &setError{Type: setServerFail, Description: err.Error()}
	}

	f := hits[id]
	if f == nil {
		return nil, expectation{}, "", &setError{Type: setNotFound, Description: "no email " + ref + " in the source account"}
	}

	d, t, serr := c.placement(given, f)
	if serr != nil {
		return nil, expectation{}, "", serr
	}

	msg, err := c.from.user.Read(&f.metas[0])
	if err != nil {
		return nil, expectation{}, "", &setError{Type: setServerFail, Description: err.Error()}
	}

	created, serr := storeEmail(c.sess, t, msg, d.boxes, d.keywords, d.receivedAt)
	if serr != nil {
		return nil, expectation{}, "", serr
	}

	return created, expectation{keywords: d.keywords, boxes: d.boxes}, id, nil
}

// placement reads where the copy goes and what it carries: the keywords
// and the arrival of the original as the user sees it, unless the
// creation says otherwise, and the rights the destination needs for
// them.
func (c *copyRun) placement(given map[string]json.RawMessage, f *found) (*draft, *tree, *setError) {
	t, err := c.sess.current().tree()
	if err != nil {
		return nil, nil, &setError{Type: setServerFail, Description: err.Error()}
	}

	if _, ok := given[propKeywords]; !ok {
		given[propKeywords] = mustRaw(keywordsOfFound(f))
	}

	if _, ok := given[propReceivedAt]; !ok {
		given[propReceivedAt] = mustRaw(f.metas[0].InternalDate.UTC().Format(time.RFC3339))
	}

	d := &draft{}
	if serr := readPlacement(c.sess, t, given, d); serr != nil {
		return nil, nil, serr
	}

	return d, t, nil
}

// destroyOriginals is the implicit Email/set of onSuccessDestroyOriginal
// on the source account, with the rights a destruction needs there.
func (c *copyRun) destroyOriginals(ctx context.Context, ifInState *string) []invocation {
	args := setRequest{AccountID: c.from.accountID(), Destroy: c.originals, IfInState: ifInState}

	res, merr := emailSet(ctx, c.from, mustRaw(&args))
	if merr != nil {
		return []invocation{{Name: kwError, Args: mustRaw(merr)}}
	}

	return []invocation{{Name: methodEmailSet, Args: mustRaw(res)}}
}
