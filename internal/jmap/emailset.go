package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"shardpost/internal/model"
)

// Email/set (RFC 8621 4.6). An Email is a GUID with one or more
// placements, and what a client may change about it is where it is and
// which keywords it has. Both are carried out on the placements with the
// IMAP operations the store already has, so an IMAP client sees a JMAP
// change as it would see another IMAP client's: a move is a MOVE, a
// keyword change a STORE on every copy, a destruction an EXPUNGE of every
// copy.

// The set error types Email/set adds (RFC 8620 5.3, RFC 8621 4.6).
const (
	setInvalidPatch = "invalidPatch"
	setOverQuota    = "overQuota"
	setBlobNotFound = "blobNotFound"
	setTooLarge     = "tooLarge"
)

// emailSet implements Email/set: creations first, then updates, then
// destructions, each reported on its own.
func emailSet(_ context.Context, sess *session, args json.RawMessage) (any, *methodError) {
	var req setRequest
	if merr := forAccount(sess, args, &req); merr != nil {
		return nil, merr
	}

	if n := len(req.Create) + len(req.Update) + len(req.Destroy); n > sess.limits.MaxObjectsInSet {
		return nil, &methodError{Type: errRequestTooLarge, Description: descTooManyObjects}
	}

	state, _, merr := emailState(sess)
	if merr != nil {
		return nil, merr
	}

	if req.IfInState != nil && *req.IfInState != state {
		return nil, &methodError{Type: errStateMismatch}
	}

	out := newSetResponse(sess.accountID(), state)
	expect := map[string]expectation{}

	emailCreates(sess, req.Create, out, expect)

	// The locator is brought up to what the creations wrote, so that an
	// update or a destruction can name a message created by this call.
	_, loc, merr := emailState(sess)
	if merr != nil {
		return nil, merr
	}

	emailUpdates(sess, loc, req.Update, out, expect)
	emailDestroys(sess, loc, req.Destroy, out)

	out.NewState = emailStateAfter(sess, state, out, expect)

	return out, nil
}

// emailStateAfter is the state a client may adopt after a write, by the
// rule stateAfter explains for mailboxes: the log is read back, and the
// new state is taken only as far as everything in it names this call's
// own messages. See reconcile for what makes that safe.
func emailStateAfter(sess *session, before string, out *setResponse, expect map[string]expectation) string {
	mine := map[string]bool{}

	for _, obj := range out.Created {
		if id, ok := obj[propID].(string); ok {
			mine[id] = true
		}
	}

	for id := range out.Updated {
		mine[id] = true
	}

	for _, id := range out.Destroyed {
		mine[id] = true
	}

	return reconcile(sess, before, mine, out.Created, out.Updated, out.Destroyed, expect)
}

// expectation is what a write means a message to have afterwards.
type expectation struct {
	keywords map[string]bool
	boxes    map[string]bool
}

// reconcile advances the state over the entries that name only the
// call's own messages, and then makes that honest. An entry naming one of
// them may still be another session's — an IMAP STORE on the message
// this call just moved — and a client that adopts the new state never
// hears of it through /changes. So the messages are read back as they
// are now, and whatever differs from what the call meant is reported in
// created or updated (RFC 8620 5.3 lets the server report properties it
// changed beyond the request); a destroyed message that is still there
// keeps the old state. What is read back is at least as new as the new
// state, so the client ends up ahead, never behind.
func reconcile(sess *session, before string, mine map[string]bool, created map[string]map[string]any,
	updated map[string]any, destroyed []string, expect map[string]expectation,
) string {
	after := ownState(sess, before, mine)
	if after == before {
		return before
	}

	_, loc, merr := emailState(sess)
	if merr != nil {
		return before
	}

	hits, err := loc.resolve(sortedKeys(mine))
	if err != nil {
		return before
	}

	for _, id := range destroyed {
		if hits[id] != nil {
			return before
		}
	}

	if !report(hits, created, updated, expect) {
		return before
	}

	return after
}

// report puts what differs from the expectations into created and
// updated. It is false when a message the call wrote is gone.
func report(hits map[string]*found, created map[string]map[string]any, updated map[string]any,
	expect map[string]expectation,
) bool {
	byID := map[string]map[string]any{}

	for _, obj := range created {
		if id, ok := obj[propID].(string); ok {
			byID[id] = obj
		}
	}

	for id, exp := range expect {
		f := hits[id]
		if f == nil {
			return false
		}

		diff := differences(f, exp)

		switch obj, ok := byID[id]; {
		case len(diff) == 0:
		case ok:
			for k, v := range diff {
				obj[k] = v
			}
		case updated != nil:
			updated[id] = diff
		}
	}

	return true
}

// differences are the keywords and mailboxes a message has that differ
// from what was expected.
func differences(f *found, exp expectation) map[string]any {
	diff := map[string]any{}

	if kw := keywordsOfFound(f); !sameSet(kw, exp.keywords) {
		diff[propKeywords] = kw
	}

	if !sameSet(f.boxes, exp.boxes) {
		diff[propMailboxIDs] = f.boxes
	}

	return diff
}

// sameSet reports whether two sets have the same members.
func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}

	for k := range a {
		if !b[k] {
			return false
		}
	}

	return true
}

// ownState reads the log from before and returns the state after the
// longest run of entries that touch only the given messages.
func ownState(sess *session, before string, mine map[string]bool) string {
	since, ok := seqOf(before)
	if !ok || len(mine) == 0 {
		return before
	}

	entries, ok, err := sess.user.AccountChanges(since, maxMailboxChanges)
	if err != nil || !ok {
		return before
	}

	last := since

	for i := range entries {
		e := &entries[i]
		if e.Kind != model.KindEmail || e.Partial || len(e.Msgs) == 0 {
			break
		}

		for _, m := range e.Msgs {
			if !mine[m.GUID] {
				return stateAt(sess, before, since, last)
			}
		}

		last = e.Seq
	}

	return stateAt(sess, before, since, last)
}

// stateAt is the token of the account at a log sequence; the one the
// call started from when it got no further.
func stateAt(sess *session, before string, since, last uint64) string {
	if last == since {
		return before
	}

	return sess.current().state(stateEmails, last)
}

// emailPatch is a validated update: the keywords and the mailboxes the
// message ends up with, nil where the update leaves them alone.
type emailPatch struct {
	keywords map[string]bool
	boxes    map[string]bool
}

// emailUpdates runs the updates of a /set.
func emailUpdates(sess *session, loc *locator, update map[string]map[string]json.RawMessage, out *setResponse,
	expect map[string]expectation,
) {
	if len(update) == 0 {
		return
	}

	ids := map[string]string{} // the id as the client wrote it -> the GUID
	guids := make([]string, 0, len(update))

	for ref := range update {
		id, _ := resolveCreationID(sess, ref)
		ids[ref] = id
		guids = append(guids, id)
	}

	hits, err := loc.resolve(guids)
	if err != nil {
		for ref := range update {
			out.NotUpdated[ref] = &setError{Type: setServerFail, Description: err.Error()}
		}

		return
	}

	for ref, raw := range update {
		f, ok := hits[ids[ref]]
		if !ok {
			out.NotUpdated[ref] = &setError{Type: setNotFound}
			continue
		}

		exp, serr := updateEmail(sess, f, raw)
		if serr != nil {
			out.NotUpdated[ref] = serr
			continue
		}

		out.Updated[ids[ref]] = nil
		expect[ids[ref]] = exp
	}
}

// updateEmail validates one update, carries it out and returns what the
// message is meant to have afterwards. When copies are removed and the
// keywords are not part of the update, the survivors are given the
// keywords the message had: under the union rule, dropping the one copy
// that was \Seen would otherwise make the message unread.
func updateEmail(sess *session, f *found, raw map[string]json.RawMessage) (expectation, *setError) {
	t, err := sess.current().tree()
	if err != nil {
		return expectation{}, &setError{Type: setServerFail, Description: err.Error()}
	}

	p, serr := readEmailPatch(sess, t, f, raw)
	if serr != nil {
		return expectation{}, serr
	}

	exp := expectation{keywords: p.keywords, boxes: p.boxes}
	if exp.keywords == nil {
		exp.keywords = keywordsOfFound(f)
	}

	if exp.boxes == nil {
		exp.boxes = f.boxes
	}

	if serr := mayUpdateEmail(sess.current(), t, f, p, exp.keywords); serr != nil {
		return expectation{}, serr
	}

	copies, serr := applyBoxes(sess, t, f, p.boxes)
	if serr != nil {
		return expectation{}, serr
	}

	if p.keywords == nil && p.boxes == nil {
		return exp, nil
	}

	return exp, applyKeywords(sess, copies, exp.keywords)
}

// readEmailPatch decodes and checks an update without writing anything.
// A whole property and a path into it cannot both be given: one would be
// a prefix of the other, which RFC 8620 5.3 makes an invalid patch.
func readEmailPatch(sess *session, t *tree, f *found, raw map[string]json.RawMessage) (*emailPatch, *setError) {
	p := &emailPatch{}

	for key, v := range raw {
		prop, sub, isPath := strings.Cut(key, "/")

		var serr *setError

		switch {
		case isPath && (sub == "" || strings.Contains(sub, "/")):
			serr = &setError{Type: setInvalidPatch, Description: key + " is not a path into a set"}
		case prop == propKeywords:
			p.keywords, serr = patchKeywords(p.keywords, keywordsOfFound(f), raw, key, sub, isPath, v)
		case prop == propMailboxIDs:
			p.boxes, serr = patchBoxes(sess, t, p.boxes, f.boxes, raw, key, sub, isPath, v)
		case !isPath && unchanged(prop, f, v):
			// RFC 8620 5.3: a property sent back with the value it has is
			// not a change, even one the client may not write.
		default:
			serr = invalidProperty(prop, "only keywords and mailboxIds of an email can change")
		}

		if serr != nil {
			return nil, serr
		}
	}

	if p.boxes != nil && len(p.boxes) == 0 {
		return nil, invalidProperty(propMailboxIDs, "an email is in at least one mailbox")
	}

	return p, nil
}

// patchKeywords applies one patch key to the keyword set being built.
func patchKeywords(cur, start map[string]bool, raw map[string]json.RawMessage,
	key, sub string, isPath bool, v json.RawMessage,
) (map[string]bool, *setError) {
	if isPath && raw[propKeywords] != nil {
		return nil, &setError{Type: setInvalidPatch, Description: key + " and keywords in one patch"}
	}

	if !isPath {
		set, serr := readKeywordSet(v)

		return set, serr
	}

	if cur == nil {
		cur = copySet(start)
	}

	k := strings.ToLower(unescapeToken(sub))
	if !validKeyword(k) {
		return nil, invalidProperty(propKeywords, "not a keyword: "+sub)
	}

	on, serr := readPatchValue(v, propKeywords)
	if serr != nil {
		return nil, serr
	}

	setMember(cur, k, on)

	return cur, nil
}

// patchBoxes applies one patch key to the mailbox set being built.
func patchBoxes(sess *session, t *tree, cur, start map[string]bool, raw map[string]json.RawMessage,
	key, sub string, isPath bool, v json.RawMessage,
) (map[string]bool, *setError) {
	if isPath && raw[propMailboxIDs] != nil {
		return nil, &setError{Type: setInvalidPatch, Description: key + " and mailboxIds in one patch"}
	}

	if !isPath {
		return readMailboxSet(sess, t, v)
	}

	if cur == nil {
		cur = copySet(start)
	}

	id, serr := mailboxRef(sess, t, unescapeToken(sub))
	if serr != nil {
		return nil, serr
	}

	on, serr := readPatchValue(v, propMailboxIDs)
	if serr != nil {
		return nil, serr
	}

	setMember(cur, id, on)

	return cur, nil
}

// readPatchValue reads the value of a path into a set: true adds, null
// removes, and nothing else is a value such a path can take.
func readPatchValue(v json.RawMessage, prop string) (bool, *setError) {
	switch strings.TrimSpace(string(v)) {
	case "true":
		return true, nil
	case jsonNull:
		return false, nil
	}

	return false, &setError{Type: setInvalidPatch, Description: "a set member of " + prop + " is true, or null to remove it"}
}

// unchanged reports whether a property of the index record is given with
// the value it has.
func unchanged(prop string, f *found, v json.RawMessage) bool {
	have, ok := metaProperty(prop, f.metas[0].GUID, &f.metas[0], f)
	if !ok {
		return false
	}

	a, okA := canonicalJSON(have)
	b, okB := canonicalJSON(v)

	return okA && okB && a == b
}

// canonicalJSON renders a value as JSON with the keys of every object in
// order, so that two renderings of one value compare equal.
func canonicalJSON(v any) (string, bool) {
	raw, ok := v.(json.RawMessage)
	if !ok {
		b, err := json.Marshal(v)
		if err != nil {
			return "", false
		}

		raw = b
	}

	var generic any
	if json.Unmarshal(raw, &generic) != nil {
		return "", false
	}

	b, err := json.Marshal(generic)

	return string(b), err == nil
}

// readKeywordSet reads a whole keywords value.
func readKeywordSet(v json.RawMessage) (map[string]bool, *setError) {
	var in map[string]bool
	if err := json.Unmarshal(v, &in); err != nil {
		return nil, invalidProperty(propKeywords, err.Error())
	}

	out := map[string]bool{}

	for k, on := range in {
		lower := strings.ToLower(k)
		if !on || !validKeyword(lower) {
			return nil, invalidProperty(propKeywords, "not a keyword, or not true: "+k)
		}

		out[lower] = true
	}

	return out, nil
}

// readMailboxSet reads a whole mailboxIds value.
func readMailboxSet(sess *session, t *tree, v json.RawMessage) (map[string]bool, *setError) {
	var in map[string]bool
	if err := json.Unmarshal(v, &in); err != nil {
		return nil, invalidProperty(propMailboxIDs, err.Error())
	}

	out := map[string]bool{}

	for ref, on := range in {
		if !on {
			return nil, invalidProperty(propMailboxIDs, "a mailbox is listed with true")
		}

		id, serr := mailboxRef(sess, t, ref)
		if serr != nil {
			return nil, serr
		}

		out[id] = true
	}

	return out, nil
}

// mailboxRef resolves a mailbox id, which may name a mailbox created
// earlier in the same request.
func mailboxRef(sess *session, t *tree, ref string) (string, *setError) {
	id, pending := resolveCreationID(sess, ref)
	if _, ok := t.byID[id]; pending || !ok {
		return "", invalidProperty(propMailboxIDs, "no mailbox "+ref)
	}

	return id, nil
}

// copySet copies a set.
func copySet(in map[string]bool) map[string]bool {
	out := make(map[string]bool, len(in))
	for k := range in {
		out[k] = true
	}

	return out
}

// setMember adds or removes one member.
func setMember(set map[string]bool, k string, on bool) {
	if on {
		set[k] = true
	} else {
		delete(set, k)
	}
}

// placement is one copy of a message: the mailbox it is in, by name, and
// its index record there.
type placement struct {
	mailbox string
	meta    model.Meta
}

// placementsOf are the copies of a found message, by mailbox id.
func placementsOf(t *tree, f *found) map[string][]placement {
	out := map[string][]placement{}

	for i, pl := range f.places {
		if mb, ok := t.byID[pl.Mailbox]; ok {
			out[pl.Mailbox] = append(out[pl.Mailbox], placement{mailbox: mb.Name, meta: f.metas[i]})
		}
	}

	return out
}

// applyBoxes puts the message into the mailboxes of the patch and takes
// it out of the others, and returns where it is afterwards. A change
// from one mailbox to another is a move, which IMAP sees as one; any
// other change copies first and removes afterwards, so that a failure
// half-way leaves the message in too many mailboxes, never in none.
func applyBoxes(sess *session, t *tree, f *found, target map[string]bool) ([]placement, *setError) {
	byBox := placementsOf(t, f)
	if target == nil {
		return flattenPlacements(byBox), nil
	}

	add, rm := boxDiff(byBox, target)

	if len(add) == 1 && len(rm) > 0 {
		src := byBox[rm[0]]

		metas := metasOf(src)
		for i := range metas {
			metas[i].Flags = allowedFlags(sess.current().rights(add[0]), metas[i].Flags)
		}

		moved, err := sess.user.Move(src[0].mailbox, metas, t.byID[add[0]].Name)
		if err != nil {
			return nil, storeFailed(err)
		}

		byBox[add[0]] = placed(t.byID[add[0]].Name, moved)
		delete(byBox, rm[0])
		add, rm = nil, rm[1:]
	}

	for _, id := range add {
		// The copy takes the flags the user may set there, as IMAP COPY
		// does; the keywords are written afterwards, and the rights for
		// them were checked before anything was written.
		src := f.metas[0]
		src.Flags = allowedFlags(sess.current().rights(id), src.Flags)

		copied, err := sess.user.Copy([]model.Meta{src}, t.byID[id].Name)
		if err != nil {
			return nil, storeFailed(err)
		}

		byBox[id] = placed(t.byID[id].Name, copied)
	}

	for _, id := range rm {
		if err := sess.user.Expunge(byBox[id][0].mailbox, metasOf(byBox[id])); err != nil {
			return nil, storeFailed(err)
		}

		delete(byBox, id)
	}

	return flattenPlacements(byBox), nil
}

// boxDiff are the mailboxes a message has to go into and the ones it has
// to leave, sorted so that a move always takes the same source.
func boxDiff(byBox map[string][]placement, target map[string]bool) (add, rm []string) {
	for _, id := range sortedKeys(target) {
		if len(byBox[id]) == 0 {
			add = append(add, id)
		}
	}

	for id := range byBox {
		if !target[id] {
			rm = append(rm, id)
		}
	}

	sort.Strings(rm)

	return add, rm
}

// applyKeywords gives every copy of a message the keyword set. Flags that
// are not keywords — \Deleted, \Recent, an atom JMAP cannot spell — are
// left as they are, and a keyword the copy keeps keeps its spelling.
//
// In a public folder $seen is the reader's own (see the private \Seen
// marks in docs/development/public-folders.md): the shared record keeps
// its \Seen, and the reader's mark is written instead.
func applyKeywords(sess *session, copies []placement, want map[string]bool) *setError {
	a := sess.current()
	byName, marks := keywordWrites(a, copies, want)

	apply := func(old []string) []string {
		out := make([]string, 0, len(old)+len(want))
		have := map[string]bool{}

		for _, f := range old {
			k, isKeyword := keywordOf(f)
			if !isKeyword || want[k] {
				out = append(out, f)
				have[k] = isKeyword
			}
		}

		for k := range want {
			if !have[k] {
				out = append(out, flagOf(k))
			}
		}

		return out
	}

	if a.public {
		apply = keepSeen(apply)
	}

	for name, uids := range byName {
		if _, err := sess.user.SetFlags(name, uids, 0, apply); err != nil {
			return storeFailed(err)
		}
	}

	for name, uids := range marks {
		if err := a.setPrivateSeen(name, uids, want[keywordSeen]); err != nil {
			return storeFailed(err)
		}
	}

	return nil
}

// keywordWrites sorts the copies whose keywords differ from want into
// those whose shared flags change and, in a public folder, those whose
// reader's mark changes, by mailbox.
func keywordWrites(a *account, copies []placement, want map[string]bool) (flags, marks map[string][]uint32) {
	flags, marks = map[string][]uint32{}, map[string][]uint32{}

	for _, c := range copies {
		have := keywordsOf(c.meta.Flags)

		switch {
		case !a.public:
			if !sameSet(have, want) {
				flags[c.mailbox] = append(flags[c.mailbox], c.meta.UID)
			}
		default:
			if have[keywordSeen] != want[keywordSeen] {
				marks[c.mailbox] = append(marks[c.mailbox], c.meta.UID)
			}

			if !sameSetExcept(have, want, keywordSeen) {
				flags[c.mailbox] = append(flags[c.mailbox], c.meta.UID)
			}
		}
	}

	return flags, marks
}

// sameSetExcept reports whether two sets have the same members apart
// from one.
func sameSetExcept(a, b map[string]bool, except string) bool {
	for k := range a {
		if k != except && !b[k] {
			return false
		}
	}

	for k := range b {
		if k != except && !a[k] {
			return false
		}
	}

	return true
}

// keepSeen leaves the shared \Seen of a public folder as it is: the
// reader's own mark carries its $seen.
func keepSeen(apply func([]string) []string) func([]string) []string {
	return func(old []string) []string {
		return withSeen(apply(old), model.HasFlag(old, flagSeen))
	}
}

// mayUpdateEmail checks, before anything is written, every right an
// update of a message in a shared account needs, as IMAP checks them:
// a mailbox the message goes into needs i, one it leaves t and e (a
// MOVE, or a \Deleted and an EXPUNGE), and every keyword that changes on
// a copy — or that a new copy carries — the right for it ($seen s, every
// other keyword w). A right that is missing refuses the whole update
// with forbidden; nothing is dropped silently.
func mayUpdateEmail(a *account, t *tree, f *found, p *emailPatch, want map[string]bool) *setError {
	if a.personal {
		return nil
	}

	byBox := placementsOf(t, f)

	if p.boxes != nil {
		add, rm := boxDiff(byBox, p.boxes)

		if serr := mayStore(a, setOf(add), want); serr != nil {
			return serr
		}

		for _, id := range rm {
			if !a.may(id, 't') || !a.may(id, 'e') {
				return &setError{Type: setForbidden, Properties: []string{propMailboxIDs},
					Description: "no right to remove messages from " + id}
			}
		}
	}

	if p.keywords == nil && p.boxes == nil {
		return nil
	}

	return mayRewrite(a, byBox, p.boxes, want)
}

// setOf is a list as a set.
func setOf(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}

	return out
}

// mayRewrite checks the keyword rights of the copies that stay, whose
// keywords an update writes to want.
func mayRewrite(a *account, byBox map[string][]placement, target, want map[string]bool) *setError {
	for id, list := range byBox {
		if target != nil && !target[id] {
			continue
		}

		for _, c := range list {
			if k := missingKeywordRight(a.rights(id), changedKeywords(keywordsOf(c.meta.Flags), want)); k != "" {
				return &setError{Type: setForbidden, Properties: []string{propKeywords},
					Description: "no right to change " + k + " in " + id}
			}
		}
	}

	return nil
}

// changedKeywords are the keywords one set has and the other has not.
func changedKeywords(a, b map[string]bool) map[string]bool {
	out := map[string]bool{}

	for k := range a {
		if !b[k] {
			out[k] = true
		}
	}

	for k := range b {
		if !a[k] {
			out[k] = true
		}
	}

	return out
}

// mayStore checks the rights a new message in a shared account needs in
// each of its mailboxes: i, and the rights for its keywords.
func mayStore(a *account, boxes, keywords map[string]bool) *setError {
	if a.personal {
		return nil
	}

	for _, id := range sortedKeys(boxes) {
		if !a.may(id, 'i') {
			return &setError{Type: setForbidden, Properties: []string{propMailboxIDs},
				Description: "no right to add messages to " + id}
		}

		if k := missingKeywordRight(a.rights(id), keywords); k != "" {
			return &setError{Type: setForbidden, Properties: []string{propKeywords},
				Description: "no right to set " + k + " in " + id}
		}
	}

	return nil
}

// metasOf are the index records of placements.
func metasOf(list []placement) []model.Meta {
	out := make([]model.Meta, len(list))
	for i := range list {
		out[i] = list[i].meta
	}

	return out
}

// placed are the placements of records in one mailbox.
func placed(name string, metas []model.Meta) []placement {
	out := make([]placement, len(metas))
	for i := range metas {
		out[i] = placement{mailbox: name, meta: metas[i]}
	}

	return out
}

// flattenPlacements lists the placements of every mailbox.
func flattenPlacements(byBox map[string][]placement) []placement {
	var out []placement
	for _, list := range byBox {
		out = append(out, list...)
	}

	return out
}

// storeFailed maps a store error onto the /set error a client sees.
func storeFailed(err error) *setError {
	if errors.Is(err, model.ErrOverQuota) {
		return &setError{Type: setOverQuota}
	}

	return &setError{Type: setServerFail, Description: err.Error()}
}

// emailDestroys runs the destructions of a /set: every copy of the
// message is removed.
func emailDestroys(sess *session, loc *locator, destroy []string, out *setResponse) {
	if len(destroy) == 0 {
		return
	}

	t, err := sess.current().tree()
	if err != nil {
		for _, ref := range destroy {
			out.NotDestroyed[ref] = &setError{Type: setServerFail, Description: err.Error()}
		}

		return
	}

	for _, ref := range destroy {
		id, _ := resolveCreationID(sess, ref)

		hits, err := loc.resolve([]string{id})
		if err != nil {
			out.NotDestroyed[ref] = &setError{Type: setServerFail, Description: err.Error()}
			continue
		}

		f, ok := hits[id]
		if !ok {
			out.NotDestroyed[ref] = &setError{Type: setNotFound}
			continue
		}

		if serr := destroyEmail(sess, t, f); serr != nil {
			out.NotDestroyed[ref] = serr
			continue
		}

		out.Destroyed = append(out.Destroyed, id)
	}
}

// destroyEmail expunges every copy of a message the user can see. In a
// shared account that needs t and e in each of those mailboxes, checked
// for all of them before the first is expunged; a copy in a mailbox the
// user may not read is not part of the message there and stays.
func destroyEmail(sess *session, t *tree, f *found) *setError {
	a := sess.current()
	byBox := placementsOf(t, f)

	for id := range byBox {
		if !a.may(id, 't') || !a.may(id, 'e') {
			return &setError{Type: setForbidden, Description: "no right to remove messages from " + id}
		}
	}

	for _, list := range byBox {
		if err := sess.user.Expunge(list[0].mailbox, metasOf(list)); err != nil {
			return storeFailed(err)
		}
	}

	return nil
}
