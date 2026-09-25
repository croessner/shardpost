package jmap

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// mailServer is a front-end whose store already holds an account.
func mailServer(t *testing.T, prepare func(model.User)) *Server {
	t.Helper()

	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	if prepare != nil {
		prepare(u)
	}

	return newTestServer(t, func(o *Options) { o.Store = store })
}

// mailCall sends one method call with the mail capability and returns
// the arguments of its response, failing the test on an error response.
func mailCall(t *testing.T, s *Server, name, args string) map[string]any {
	t.Helper()

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls":[["`+name+`",`+args+`,"c1"]]}`)

	got, _ := responses(t, out)[0].([]any)
	if got[0] != name {
		t.Fatalf("%s answered %v: %#v", name, got[0], got[1])
	}

	res, _ := got[1].(map[string]any)

	return res
}

// mailError sends one method call and returns the error type it gave.
func mailError(t *testing.T, s *Server, name, args string) string {
	t.Helper()

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"methodCalls":[["`+name+`",`+args+`,"c1"]]}`)

	got, _ := responses(t, out)[0].([]any)
	if got[0] != kwError {
		t.Fatalf("%s answered %v, want an error: %#v", name, got[0], got[1])
	}

	args2, _ := got[1].(map[string]any)
	typ, _ := args2["type"].(string)

	return typ
}

// objects is the list of a /get response, keyed by name.
func objects(t *testing.T, res map[string]any) map[string]map[string]any {
	t.Helper()

	list, ok := res["list"].([]any)
	if !ok {
		t.Fatalf("the response has no list: %#v", res)
	}

	out := map[string]map[string]any{}

	for _, item := range list {
		obj, _ := item.(map[string]any)
		name, _ := obj[propName].(string)
		out[name] = obj
	}

	return out
}

// withMailboxes prepares an account with a few mailboxes and a message.
func withMailboxes(u model.User) {
	for _, n := range []string{inboxName, "Archive", "Work", "Work/Bills"} {
		if err := u.CreateMailbox(n, useFor(n)); err != nil {
			panic(err)
		}
	}

	if _, err := u.Deliver(inboxName, []byte("Subject: one\r\n\r\nbody\r\n"), nil, time.Now()); err != nil {
		panic(err)
	}
}

// useFor is the special-use of the prepared mailboxes.
func useFor(name string) []string {
	if name == "Archive" {
		return []string{`\Archive`}
	}

	return nil
}

// TestMailboxGet: the mailboxes of the account, with the JMAP name and
// parent rather than the IMAP path, and the role that special-use says.
func TestMailboxGet(t *testing.T) {
	s := mailServer(t, withMailboxes)

	res := mailCall(t, s, "Mailbox/get", `{"accountId":"test"}`)

	got := objects(t, res)
	if len(got) != 4 {
		t.Fatalf("got %d mailboxes, want 4: %#v", len(got), got)
	}

	if got[inboxName][propRole] != "inbox" {
		t.Errorf("INBOX has the role %v, want inbox", got[inboxName][propRole])
	}

	if got["Archive"][propRole] != "archive" {
		t.Errorf("Archive has the role %v, want archive", got["Archive"][propRole])
	}

	if got["Work"][propRole] != nil {
		t.Errorf("Work has the role %v, want none", got["Work"][propRole])
	}

	// Work/Bills is named by its leaf and points at Work.
	if got["Bills"] == nil {
		t.Fatalf("the child is not named by its leaf: %#v", got)
	}

	if got["Bills"][propParentID] != got["Work"][propID] {
		t.Errorf("Bills points at %v, want Work %v", got["Bills"][propParentID], got["Work"][propID])
	}

	if got["Work"][propParentID] != nil {
		t.Errorf("Work points at %v, want the top level", got["Work"][propParentID])
	}

	if got[inboxName][propTotalEmails] != float64(1) || got[inboxName][propUnreadEmails] != float64(1) {
		t.Errorf("INBOX counts are %v/%v, want 1 unread of 1",
			got[inboxName][propTotalEmails], got[inboxName][propUnreadEmails])
	}
}

// TestMailboxGetProperties: a client that names properties gets those
// and the id, and the counts are not computed behind its back.
func TestMailboxGetProperties(t *testing.T) {
	s := mailServer(t, withMailboxes)

	res := mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":["name","role"]}`)

	for _, obj := range objects(t, res) {
		if _, ok := obj[propID]; !ok {
			t.Error("an object came back without its id")
		}

		if _, ok := obj[propTotalEmails]; ok {
			t.Error("totalEmails was returned although it was not asked for")
		}

		if len(obj) != 3 {
			t.Errorf("an object has %d properties, want id, name and role: %#v", len(obj), obj)
		}
	}
}

// TestMailboxGetNotFound: an id the account does not have is reported,
// not silently dropped.
func TestMailboxGetNotFound(t *testing.T) {
	s := mailServer(t, withMailboxes)

	res := mailCall(t, s, "Mailbox/get", `{"accountId":"test","ids":["nope"]}`)

	notFound, _ := res["notFound"].([]any)
	if len(notFound) != 1 || notFound[0] != "nope" {
		t.Errorf("notFound is %#v, want the unknown id", res["notFound"])
	}

	if list, _ := res["list"].([]any); len(list) != 0 {
		t.Errorf("the list is %#v, want it empty", list)
	}
}

// TestMailboxGetRefusesAnotherAccount: the account in the arguments has
// to be the one the request authenticated as.
func TestMailboxGetRefusesAnotherAccount(t *testing.T) {
	s := mailServer(t, withMailboxes)

	if got := mailError(t, s, "Mailbox/get", `{"accountId":"someone"}`); got != errAccountNotFound {
		t.Errorf("another account gave %s, want %s", got, errAccountNotFound)
	}
}

// TestMailboxChanges: what happened between two states, classified into
// the three lists, and a state that moves with them.
func TestMailboxChanges(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	withMailboxes(u)

	s := newTestServer(t, func(o *Options) { o.Store = store })

	before := mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":[]}`)["state"].(string)

	if err := u.CreateMailbox("Later", nil); err != nil {
		t.Fatal(err)
	}

	if err := u.SetSubscribed("Archive", true); err != nil {
		t.Fatal(err)
	}

	if err := u.DeleteMailbox("Work/Bills"); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Mailbox/changes", `{"accountId":"test","sinceState":"`+before+`"}`)

	if n := len(res["created"].([]any)); n != 1 {
		t.Errorf("created is %#v, want the new mailbox", res["created"])
	}

	if n := len(res["updated"].([]any)); n != 1 {
		t.Errorf("updated is %#v, want the subscribed mailbox", res["updated"])
	}

	if n := len(res["destroyed"].([]any)); n != 1 {
		t.Errorf("destroyed is %#v, want the deleted mailbox", res["destroyed"])
	}

	if res["newState"] == before {
		t.Error("the state did not move")
	}

	if res["hasMoreChanges"] != false {
		t.Errorf("hasMoreChanges is %v, want false", res["hasMoreChanges"])
	}
}

// TestMailboxChangesIsEmptyWhenNothingHappened: a client that asks again
// with the state it was given gets nothing and the same state back.
func TestMailboxChangesIsEmptyWhenNothingHappened(t *testing.T) {
	s := mailServer(t, withMailboxes)

	state := mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":[]}`)["state"].(string)

	res := mailCall(t, s, "Mailbox/changes", `{"accountId":"test","sinceState":"`+state+`"}`)
	if res["newState"] != state {
		t.Errorf("the state moved from %v to %v with nothing in between", state, res["newState"])
	}

	for _, key := range []string{"created", "updated", "destroyed"} {
		if n := len(res[key].([]any)); n != 0 {
			t.Errorf("%s is %#v, want it empty", key, res[key])
		}
	}
}

// TestMailboxChangesReportsCounts: a delivery changes the counts of a
// mailbox, and the counts are properties of the mailbox, so the client
// is told which mailbox to refetch and that only the counts moved.
func TestMailboxChangesReportsCounts(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	withMailboxes(u)

	s := newTestServer(t, func(o *Options) { o.Store = store })

	byName := objects(t, mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":["name"]}`))
	inbox := byName[inboxName][propID].(string)
	state := mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":[]}`)["state"].(string)

	if _, err := u.Deliver(inboxName, []byte("Subject: two\r\n\r\nbody\r\n"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Mailbox/changes", `{"accountId":"test","sinceState":"`+state+`"}`)

	updated, _ := res["updated"].([]any)
	if len(updated) != 1 || updated[0] != inbox {
		t.Fatalf("the delivery gave updated %#v, want the INBOX %s", updated, inbox)
	}

	props, ok := res["updatedProperties"].([]any)
	if !ok || len(props) != 4 {
		t.Fatalf("updatedProperties is %#v, want the four counts", res["updatedProperties"])
	}

	if res["newState"] == state {
		t.Error("the state did not move past the delivery")
	}

	// A change that is not a count makes the shortcut impossible.
	state = res["newState"].(string)

	if err := u.SetSubscribed("Archive", true); err != nil {
		t.Fatal(err)
	}

	res = mailCall(t, s, "Mailbox/changes", `{"accountId":"test","sinceState":"`+state+`"}`)
	if res["updatedProperties"] != nil {
		t.Errorf("updatedProperties is %#v, want null after a real mailbox change", res["updatedProperties"])
	}
}

// TestMailboxChangesReportsRemappedChildren: a mailbox's JMAP name and
// parent are read off the mailboxes around it, so a level that appears
// renames every mailbox under it without any of them being written. The
// client has to hear about them.
func TestMailboxChangesReportsRemappedChildren(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	// Work/Bills without a Work: IMAP allows it, and JMAP then has to
	// call the child "Work/Bills" at the top level.
	if err := u.CreateMailbox("Work/Bills", nil); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, func(o *Options) { o.Store = store })

	byName := objects(t, mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":["name"]}`))
	if byName["Work/Bills"] == nil {
		t.Fatalf("the orphan is not named by its path: %#v", byName)
	}

	bills := byName["Work/Bills"][propID].(string)
	state := mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":[]}`)["state"].(string)

	if err := u.CreateMailbox("Work", nil); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Mailbox/changes", `{"accountId":"test","sinceState":"`+state+`"}`)

	updated, _ := res["updated"].([]any)
	if len(updated) != 1 || updated[0] != bills {
		t.Errorf("creating the parent gave updated %#v, want the child %s", updated, bills)
	}

	if res["updatedProperties"] != nil {
		t.Errorf("updatedProperties is %#v, want null: the name and the parent moved", res["updatedProperties"])
	}

	if byName := objects(t, mailCall(t, s, "Mailbox/get",
		`{"accountId":"test","properties":["name"]}`)); byName["Bills"] == nil {
		t.Errorf("the child is still not under its parent: %#v", byName)
	}
}

// TestMailboxChangesUnknownState: a token this server did not write, and
// one the log no longer covers, both mean the client resynchronises.
func TestMailboxChangesUnknownState(t *testing.T) {
	s := mailServer(t, withMailboxes)

	if got := mailError(t, s, "Mailbox/changes", `{"accountId":"test","sinceState":"nonsense"}`); got != errCannotCalculateChanges {
		t.Errorf("an unreadable token gave %s, want %s", got, errCannotCalculateChanges)
	}

	if got := mailError(t, s, "Mailbox/changes", `{"accountId":"test","sinceState":"99999"}`); got != errCannotCalculateChanges {
		t.Errorf("a token from the future gave %s, want %s", got, errCannotCalculateChanges)
	}
}

// TestMailboxQuery: the filter and the sort of RFC 8621 2.3, over a list
// the server holds in memory.
func TestMailboxQuery(t *testing.T) {
	s := mailServer(t, withMailboxes)

	all := mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":["name"]}`)
	byName := objects(t, all)

	res := mailCall(t, s, "Mailbox/query", `{"accountId":"test","filter":{"hasAnyRole":true}}`)

	got, _ := res["ids"].([]any)
	if len(got) != 2 {
		t.Errorf("the role filter matched %#v, want INBOX and Archive", got)
	}

	res = mailCall(t, s, "Mailbox/query",
		`{"accountId":"test","filter":{"parentId":"`+byName["Work"][propID].(string)+`"}}`)
	if got, _ := res["ids"].([]any); len(got) != 1 || got[0] != byName["Bills"][propID] {
		t.Errorf("the parent filter matched %#v, want Bills", got)
	}

	res = mailCall(t, s, "Mailbox/query",
		`{"accountId":"test","sort":[{"property":"name","isAscending":false}],"calculateTotal":true}`)
	if got, _ := res["ids"].([]any); len(got) != 4 || got[0] != byName["Work"][propID] {
		t.Errorf("the descending name sort gave %#v, want Work first", got)
	}

	if res["total"] != float64(4) {
		t.Errorf("total is %v, want 4", res["total"])
	}

	if res["canCalculateChanges"] != false {
		t.Error("the query claims a delta it cannot compute")
	}
}

// TestMailboxQueryPaging: position and limit cut a window, and an anchor
// the result does not hold is an error rather than the first page.
func TestMailboxQueryPaging(t *testing.T) {
	s := mailServer(t, withMailboxes)

	res := mailCall(t, s, "Mailbox/query", `{"accountId":"test","position":2,"limit":1}`)
	if got, _ := res["ids"].([]any); len(got) != 1 {
		t.Errorf("the window is %#v, want one id", got)
	}

	if res["position"] != float64(2) {
		t.Errorf("position is %v, want 2", res["position"])
	}

	if got := mailError(t, s, "Mailbox/query", `{"accountId":"test","anchor":"nope"}`); got != errAnchorNotFound {
		t.Errorf("an unknown anchor gave %s, want %s", got, errAnchorNotFound)
	}
}

// TestMailboxQueryChanges: the query result is not a list this server
// computes a delta against, and it says so rather than guessing.
func TestMailboxQueryChanges(t *testing.T) {
	s := mailServer(t, withMailboxes)

	got := mailError(t, s, "Mailbox/queryChanges", `{"accountId":"test","sinceQueryState":"1"}`)
	if got != errCannotCalculateChanges {
		t.Errorf("queryChanges gave %s, want %s", got, errCannotCalculateChanges)
	}
}

// TestMailboxSetCreate: a creation places the mailbox under its parent,
// gives it its role and its sort order, and IMAP sees the path.
func TestMailboxSetCreate(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	withMailboxes(u)

	s := newTestServer(t, func(o *Options) { o.Store = store })

	parent := objects(t, mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":["name"]}`))["Work"][propID]

	res := mailCall(t, s, "Mailbox/set", `{"accountId":"test","create":{"a":{
		"name":"Taxes","parentId":"`+parent.(string)+`","sortOrder":7,"isSubscribed":true}}}`)

	created, _ := res["created"].(map[string]any)
	if created["a"] == nil {
		t.Fatalf("the creation failed: %#v", res["notCreated"])
	}

	mb, err := u.Lookup("Work/Taxes")
	if err != nil {
		t.Fatalf("IMAP does not see the mailbox: %v", err)
	}

	if mb.SortOrder != 7 || !mb.Subscribed {
		t.Errorf("the mailbox is %+v, want sort order 7 and subscribed", mb.MailboxRec)
	}

	// A second one under the same name is a conflict, not a silent merge.
	// Sibling names have to be unique (RFC 8621 2), which is a property
	// the client got wrong rather than a /set error type of its own.
	res = mailCall(t, s, "Mailbox/set", `{"accountId":"test","create":{"a":{
		"name":"Taxes","parentId":"`+parent.(string)+`"}}}`)

	notCreated, _ := res["notCreated"].(map[string]any)

	fail, _ := notCreated["a"].(map[string]any)
	if fail == nil || fail["type"] != setInvalidProperties {
		t.Fatalf("the duplicate gave %#v, want invalidProperties", notCreated)
	}

	if props, _ := fail["properties"].([]any); len(props) != 1 || props[0] != propName {
		t.Errorf("the duplicate blames %#v, want the name", fail["properties"])
	}
}

// TestMailboxSetCreateRefuses: the properties a client may not write,
// and a name that is not one level of the tree.
func TestMailboxSetCreateRefuses(t *testing.T) {
	s := mailServer(t, withMailboxes)

	cases := map[string]string{
		`{"name":"X","totalEmails":3}`: setInvalidProperties,
		`{"name":"X","nonsense":1}`:    setInvalidProperties,
		`{"name":""}`:                  setInvalidProperties,
		`{"name":"A/B"}`:               setInvalidProperties,
		`{"name":"X","role":"inbox"}`:  setInvalidProperties,
		`{"name":"X","parentId":"no"}`: setNotFound,
	}

	for create, want := range cases {
		res := mailCall(t, s, "Mailbox/set", `{"accountId":"test","create":{"a":`+create+`}}`)

		notCreated, _ := res["notCreated"].(map[string]any)

		fail, _ := notCreated["a"].(map[string]any)
		if fail == nil || fail["type"] != want {
			t.Errorf("%s gave %#v, want %s", create, notCreated, want)
		}
	}
}

// TestMailboxSetUpdate: a rename, a move and the properties that are not
// renames; the role is not among them.
func TestMailboxSetUpdate(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	withMailboxes(u)

	s := newTestServer(t, func(o *Options) { o.Store = store })
	byName := objects(t, mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":["name"]}`))
	bills := byName["Bills"][propID].(string)

	res := mailCall(t, s, "Mailbox/set",
		`{"accountId":"test","update":{"`+bills+`":{"name":"Invoices","sortOrder":3}}}`)
	if _, ok := res["updated"].(map[string]any)[bills]; !ok {
		t.Fatalf("the update failed: %#v", res["notUpdated"])
	}

	mb, err := u.Lookup("Work/Invoices")
	if err != nil {
		t.Fatalf("IMAP does not see the rename: %v", err)
	}

	if mb.SortOrder != 3 {
		t.Errorf("the sort order is %d, want 3", mb.SortOrder)
	}

	// Moving it to the top level.
	res = mailCall(t, s, "Mailbox/set", `{"accountId":"test","update":{"`+bills+`":{"parentId":null}}}`)
	if _, ok := res["updated"].(map[string]any)[bills]; !ok {
		t.Fatalf("the move failed: %#v", res["notUpdated"])
	}

	if _, err := u.Lookup("Invoices"); err != nil {
		t.Errorf("the mailbox is not at the top level: %v", err)
	}

	// The role is fixed at creation.
	res = mailCall(t, s, "Mailbox/set", `{"accountId":"test","update":{"`+bills+`":{"role":"junk"}}}`)
	if fail, _ := res["notUpdated"].(map[string]any)[bills].(map[string]any); fail["type"] != setInvalidProperties {
		t.Errorf("a role change gave %#v, want invalidProperties", res["notUpdated"])
	}
}

// TestMailboxSetProtectsInbox: INBOX can be neither renamed nor deleted,
// and myRights says so before a client tries.
func TestMailboxSetProtectsInbox(t *testing.T) {
	s := mailServer(t, withMailboxes)

	byName := objects(t, mailCall(t, s, "Mailbox/get", `{"accountId":"test"}`))

	rights, _ := byName[inboxName][propMyRights].(map[string]any)
	if rights["mayRename"] != false || rights["mayDelete"] != false {
		t.Errorf("INBOX claims %#v, want neither rename nor delete", rights)
	}

	id := byName[inboxName][propID].(string)

	res := mailCall(t, s, "Mailbox/set", `{"accountId":"test","update":{"`+id+`":{"name":"Elsewhere"}}}`)
	if fail, _ := res["notUpdated"].(map[string]any)[id].(map[string]any); fail["type"] != setForbidden {
		t.Errorf("renaming INBOX gave %#v, want forbidden", res["notUpdated"])
	}

	res = mailCall(t, s, "Mailbox/set", `{"accountId":"test","destroy":["`+id+`"]}`)
	if fail, _ := res["notDestroyed"].(map[string]any)[id].(map[string]any); fail["type"] != setForbidden {
		t.Errorf("deleting INBOX gave %#v, want forbidden", res["notDestroyed"])
	}
}

// TestMailboxSetDestroy: a mailbox with children or with messages is not
// deleted by accident; onDestroyRemoveEmails is the client saying it
// means it.
func TestMailboxSetDestroy(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	withMailboxes(u)

	s := newTestServer(t, func(o *Options) { o.Store = store })
	byName := objects(t, mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":["name"]}`))
	work := byName["Work"][propID].(string)

	res := mailCall(t, s, "Mailbox/set", `{"accountId":"test","destroy":["`+work+`"]}`)
	if fail, _ := res["notDestroyed"].(map[string]any)[work].(map[string]any); fail["type"] != setMailboxHasChild {
		t.Errorf("deleting a parent gave %#v, want mailboxHasChild", res["notDestroyed"])
	}

	// INBOX holds a message, but INBOX is refused before that is looked
	// at, so the message check needs a mailbox of its own.
	if err := u.CreateMailbox("Full", nil); err != nil {
		t.Fatal(err)
	}

	if _, err := u.Deliver("Full", []byte("Subject: x\r\n\r\nbody\r\n"), nil, time.Now()); err != nil {
		t.Fatal(err)
	}

	full := objects(t, mailCall(t, s, "Mailbox/get",
		`{"accountId":"test","properties":["name"]}`))["Full"][propID].(string)

	res = mailCall(t, s, "Mailbox/set", `{"accountId":"test","destroy":["`+full+`"]}`)
	if fail, _ := res["notDestroyed"].(map[string]any)[full].(map[string]any); fail["type"] != setMailboxHasEmail {
		t.Errorf("deleting a full mailbox gave %#v, want mailboxHasEmail", res["notDestroyed"])
	}

	res = mailCall(t, s, "Mailbox/set",
		`{"accountId":"test","onDestroyRemoveEmails":true,"destroy":["`+full+`"]}`)
	if got, _ := res["destroyed"].([]any); len(got) != 1 {
		t.Errorf("the deletion gave %#v / %#v", res["destroyed"], res["notDestroyed"])
	}

	if _, err := u.Lookup("Full"); err == nil {
		t.Error("IMAP still sees the deleted mailbox")
	}
}

// TestMailboxSetIfInState: a client that says which state it saw is told
// when something changed under it, instead of writing over it.
func TestMailboxSetIfInState(t *testing.T) {
	s := mailServer(t, withMailboxes)

	if got := mailError(t, s, "Mailbox/set",
		`{"accountId":"test","ifInState":"1","create":{"a":{"name":"X"}}}`); got != errStateMismatch {
		t.Errorf("a stale state gave %s, want %s", got, errStateMismatch)
	}
}

// TestMailboxSetMovesState: a /set reports the state before and after,
// and the two differ exactly when something changed.
func TestMailboxSetMovesState(t *testing.T) {
	s := mailServer(t, withMailboxes)

	res := mailCall(t, s, "Mailbox/set", `{"accountId":"test","create":{"a":{"name":"Fresh"}}}`)
	if res["oldState"] == res["newState"] {
		t.Errorf("the state stayed at %v although a mailbox was created", res["oldState"])
	}

	res = mailCall(t, s, "Mailbox/set", `{"accountId":"test","destroy":["nope"]}`)
	if res["oldState"] != res["newState"] {
		t.Errorf("the state moved from %v to %v although nothing happened", res["oldState"], res["newState"])
	}
}

// TestSessionOffersMail: the session advertises the mail capability and
// names the account as its primary one, so a client can reach
// Mailbox/*.
func TestSessionOffersMail(t *testing.T) {
	s := mailServer(t, withMailboxes)

	w := do(t, s, http.MethodGet, PathSession, "")

	var got Session
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if _, ok := got.Capabilities[CapMail]; !ok {
		t.Fatalf("the session does not offer %s: %v", CapMail, got.Capabilities)
	}

	if got.PrimaryAccounts[CapMail] != "test" {
		t.Errorf("the primary mail account is %q, want test", got.PrimaryAccounts[CapMail])
	}

	if _, ok := got.Accounts["test"].AccountCapabilities[CapMail]; !ok {
		t.Error("the account does not carry the mail capability")
	}
}

// TestMailboxChangesNamesTheRightIds: the three lists have to hold the
// right ids, not just the right number of them.
func TestMailboxChangesNamesTheRightIds(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	withMailboxes(u)

	s := newTestServer(t, func(o *Options) { o.Store = store })
	byName := objects(t, mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":["name"]}`))
	archive := byName["Archive"][propID].(string)
	bills := byName["Bills"][propID].(string)

	state := mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":[]}`)["state"].(string)

	if err := u.CreateMailbox("Later", nil); err != nil {
		t.Fatal(err)
	}

	if err := u.SetSubscribed("Archive", true); err != nil {
		t.Fatal(err)
	}

	if err := u.DeleteMailbox("Work/Bills"); err != nil {
		t.Fatal(err)
	}

	later, err := u.Lookup("Later")
	if err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Mailbox/changes", `{"accountId":"test","sinceState":"`+state+`"}`)

	want := map[string][]string{
		"created":   {later.ID},
		"updated":   {archive},
		"destroyed": {bills},
	}

	for key, ids := range want {
		got, _ := res[key].([]any)
		if len(got) != 1 || got[0] != ids[0] {
			t.Errorf("%s is %#v, want %v", key, got, ids)
		}
	}
}

// TestMailboxChangesPaging: maxChanges cuts the window, says that there
// is more, and the next call continues where it stopped. A maxChanges of
// zero is outside the range RFC 8620 5.2 allows.
func TestMailboxChangesPaging(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, func(o *Options) { o.Store = store })

	for _, n := range []string{"A", "B", "C", "D"} {
		if err := u.CreateMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	res := mailCall(t, s, "Mailbox/changes", `{"accountId":"test","sinceState":"0","maxChanges":2}`)
	if res["hasMoreChanges"] != true {
		t.Errorf("hasMoreChanges is %v, want true", res["hasMoreChanges"])
	}

	if n := len(res["created"].([]any)); n != 2 {
		t.Errorf("the first page holds %d, want 2", n)
	}

	res = mailCall(t, s, "Mailbox/changes",
		`{"accountId":"test","sinceState":"`+res["newState"].(string)+`","maxChanges":10}`)
	if n := len(res["created"].([]any)); n != 2 {
		t.Errorf("the second page holds %d, want the other 2", n)
	}

	if res["hasMoreChanges"] != false {
		t.Errorf("hasMoreChanges is %v at the end, want false", res["hasMoreChanges"])
	}

	if got := mailError(t, s, "Mailbox/changes",
		`{"accountId":"test","sinceState":"0","maxChanges":0}`); got != errInvalidArguments {
		t.Errorf("maxChanges 0 gave %s, want %s", got, errInvalidArguments)
	}
}

// TestMailboxChangesFoldsACreationThatWentAgain: a mailbox that came and
// went inside one window is in none of the three lists — the client
// never knew it.
func TestMailboxChangesFoldsACreationThatWentAgain(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	withMailboxes(u)

	s := newTestServer(t, func(o *Options) { o.Store = store })
	state := mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":[]}`)["state"].(string)

	if err := u.CreateMailbox("Brief", nil); err != nil {
		t.Fatal(err)
	}

	if err := u.DeleteMailbox("Brief"); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Mailbox/changes", `{"accountId":"test","sinceState":"`+state+`"}`)
	for _, key := range []string{"created", "updated", "destroyed"} {
		if n := len(res[key].([]any)); n != 0 {
			t.Errorf("%s is %#v, want it empty", key, res[key])
		}
	}
}

// TestMailboxQueryFilterNull: a null parentId is the top level and a
// null role is "no role" — both are values, not an absent condition.
func TestMailboxQueryFilterNull(t *testing.T) {
	s := mailServer(t, withMailboxes)

	res := mailCall(t, s, "Mailbox/query", `{"accountId":"test","filter":{"parentId":null}}`)
	if got, _ := res["ids"].([]any); len(got) != 3 {
		t.Errorf("parentId null matched %d, want the three top-level mailboxes", len(got))
	}

	res = mailCall(t, s, "Mailbox/query", `{"accountId":"test","filter":{"role":null}}`)
	if got, _ := res["ids"].([]any); len(got) != 2 {
		t.Errorf("role null matched %d, want Work and Bills", len(got))
	}
}

// TestMailboxQueryRefusesWhatItCannotDo: a filter that is not an object,
// a condition this server does not know, an unknown sort key and an
// unknown collation are all said out loud rather than ignored.
func TestMailboxQueryRefusesWhatItCannotDo(t *testing.T) {
	s := mailServer(t, withMailboxes)

	cases := map[string]string{
		`{"accountId":"test","filter":5}`:                                   errInvalidArguments,
		`{"accountId":"test","filter":{"names":"x"}}`:                       errUnsupportedFilter,
		`{"accountId":"test","filter":{"parentId":5}}`:                      errUnsupportedFilter,
		`{"accountId":"test","sort":[{"property":"totalEmails"}]}`:          errUnsupportedSort,
		`{"accountId":"test","sort":[{"property":"name","collation":"x"}]}`: errUnsupportedSort,
		`{"accountId":"test","sort":[{"property":"name"},{"property":"name"},
			{"property":"name"},{"property":"name"}]}`: errUnsupportedSort,
	}

	for args, want := range cases {
		if got := mailError(t, s, "Mailbox/query", args); got != want {
			t.Errorf("%.50s gave %s, want %s", args, got, want)
		}
	}
}

// TestMailboxGetRefusesUnknownProperty: a misspelled property is an
// error, not a quietly shorter object (RFC 8620 5.1).
func TestMailboxGetRefusesUnknownProperty(t *testing.T) {
	s := mailServer(t, withMailboxes)

	if got := mailError(t, s, "Mailbox/get",
		`{"accountId":"test","properties":["nmae"]}`); got != errInvalidArguments {
		t.Errorf("an unknown property gave %s, want %s", got, errInvalidArguments)
	}
}

// TestMailboxSetCreationIDParent: a creation may name its parent by the
// creation id of another one in the same call, in whatever order the
// JSON object happens to be read in.
func TestMailboxSetCreationIDParent(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, func(o *Options) { o.Store = store })

	res := mailCall(t, s, "Mailbox/set", `{"accountId":"test","create":{
		"child":{"name":"Bills","parentId":"#parent"},
		"parent":{"name":"Work"}}}`)

	created, _ := res["created"].(map[string]any)
	if len(created) != 2 {
		t.Fatalf("created %#v, notCreated %#v", created, res["notCreated"])
	}

	if _, err := u.Lookup("Work/Bills"); err != nil {
		t.Errorf("the child was not placed under its parent: %v", err)
	}

	// A reference to a creation that never happened is not a silent
	// top-level mailbox.
	res = mailCall(t, s, "Mailbox/set", `{"accountId":"test","create":{
		"child":{"name":"Orphan","parentId":"#missing"}}}`)
	if fail, _ := res["notCreated"].(map[string]any)["child"].(map[string]any); fail["type"] != setNotFound {
		t.Errorf("a dangling creation reference gave %#v, want notFound", res["notCreated"])
	}
}

// TestMailboxSetReturnsCreatedIds: the ids of this request's creations
// go back in createdIds, so a later call of the same request can use
// them (RFC 8620 3.3).
func TestMailboxSetReturnsCreatedIds(t *testing.T) {
	s := mailServer(t, nil)

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],
		"createdIds":{},"methodCalls":[["Mailbox/set",{"accountId":"test","create":{
		"a":{"name":"Fresh"}}},"c1"]]}`)

	ids, ok := out["createdIds"].(map[string]any)
	if !ok || ids["a"] == nil {
		t.Errorf("createdIds is %#v, want the id of the creation", out["createdIds"])
	}
}

// TestMailboxSetRoleIsUnique: an account has at most one mailbox per
// role (RFC 8621 2).
func TestMailboxSetRoleIsUnique(t *testing.T) {
	s := mailServer(t, withMailboxes)

	res := mailCall(t, s, "Mailbox/set", `{"accountId":"test","create":{"a":{"name":"Old","role":"archive"}}}`)
	if fail, _ := res["notCreated"].(map[string]any)["a"].(map[string]any); fail["type"] != setInvalidProperties {
		t.Errorf("a second archive gave %#v, want invalidProperties", res["notCreated"])
	}

	res = mailCall(t, s, "Mailbox/set", `{"accountId":"test","create":{"a":{"name":"Bin","role":"trash"}}}`)
	if _, ok := res["created"].(map[string]any)["a"]; !ok {
		t.Errorf("a free role was refused: %#v", res["notCreated"])
	}
}

// TestMailboxSetUpdateIsAllOrNothing: a patch whose rename fails leaves
// the properties before it unwritten (RFC 8620 5.3).
func TestMailboxSetUpdateIsAllOrNothing(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	withMailboxes(u)

	s := newTestServer(t, func(o *Options) { o.Store = store })
	bills := objects(t, mailCall(t, s, "Mailbox/get",
		`{"accountId":"test","properties":["name"]}`))["Bills"][propID].(string)

	// INBOX exists, so renaming Bills to it at the top level fails.
	res := mailCall(t, s, "Mailbox/set",
		`{"accountId":"test","update":{"`+bills+`":{"sortOrder":9,"name":"`+inboxName+`","parentId":null}}}`)
	if _, bad := res["notUpdated"].(map[string]any)[bills]; !bad {
		t.Fatalf("the update was accepted: %#v", res["updated"])
	}

	mb, err := u.Lookup("Work/Bills")
	if err != nil {
		t.Fatalf("the mailbox moved although the update failed: %v", err)
	}

	if mb.SortOrder != 0 {
		t.Errorf("the sort order is %d, want it unwritten after the failed rename", mb.SortOrder)
	}
}

// TestMailboxSetRefusesABadName: a name that is not Net-Unicode, or too
// long, or out of range as a sort order.
func TestMailboxSetRefusesABadName(t *testing.T) {
	s := mailServer(t, withMailboxes)

	cases := []string{
		`{"name":"a\u0007b"}`,
		`{"name":"` + strings.Repeat("x", 256) + `"}`,
		`{"name":"X","sortOrder":2147483648}`,
		`{"name":"X","parentId":""}`,
	}

	for _, create := range cases {
		res := mailCall(t, s, "Mailbox/set", `{"accountId":"test","create":{"a":`+create+`}}`)
		if fail, _ := res["notCreated"].(map[string]any)["a"].(map[string]any); fail["type"] != setInvalidProperties {
			t.Errorf("%.40s gave %#v, want invalidProperties", create, res["notCreated"])
		}
	}
}

// TestMailboxSetIfInStateMatches: the state the client saw lets the /set
// through, and the state it gets back is usable as the next sinceState.
func TestMailboxSetIfInStateMatches(t *testing.T) {
	s := mailServer(t, withMailboxes)

	state := mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":[]}`)["state"].(string)

	res := mailCall(t, s, "Mailbox/set",
		`{"accountId":"test","ifInState":"`+state+`","create":{"a":{"name":"Fresh"}}}`)
	if _, ok := res["created"].(map[string]any)["a"]; !ok {
		t.Fatalf("the matching state was refused: %#v", res)
	}

	if res["oldState"] != state {
		t.Errorf("oldState is %v, want the state the client held", res["oldState"])
	}

	changes := mailCall(t, s, "Mailbox/changes",
		`{"accountId":"test","sinceState":"`+res["newState"].(string)+`"}`)
	for _, key := range []string{"created", "updated", "destroyed"} {
		if n := len(changes[key].([]any)); n != 0 {
			t.Errorf("the state after the /set still reports %s: %#v", key, changes[key])
		}
	}
}

// TestMailboxCountsIgnoreDrafts: a draft is not unread (RFC 8621 2).
func TestMailboxCountsIgnoreDrafts(t *testing.T) {
	s := mailServer(t, func(u model.User) {
		if err := u.CreateMailbox("Drafts", []string{`\Drafts`}); err != nil {
			panic(err)
		}

		if _, err := u.Deliver("Drafts", []byte("Subject: d\r\n\r\nbody\r\n"),
			[]string{`\Draft`}, time.Now()); err != nil {
			panic(err)
		}
	})

	got := objects(t, mailCall(t, s, "Mailbox/get", `{"accountId":"test"}`))["Drafts"]
	if got[propTotalEmails] != float64(1) {
		t.Errorf("the draft is not counted: %v", got[propTotalEmails])
	}

	if got[propUnreadEmails] != float64(0) || got[propUnreadThreads] != float64(0) {
		t.Errorf("a draft counts as unread: %v/%v", got[propUnreadEmails], got[propUnreadThreads])
	}
}

// TestMailboxThreadCounts: two messages of one conversation are two
// emails and one thread.
func TestMailboxThreadCounts(t *testing.T) {
	s := mailServer(t, func(u model.User) {
		if err := u.CreateMailbox(inboxName, nil); err != nil {
			panic(err)
		}

		first := "Message-ID: <a@example.org>\r\nSubject: one\r\n\r\nbody\r\n"
		reply := "Message-ID: <b@example.org>\r\nReferences: <a@example.org>\r\nSubject: Re: one\r\n\r\nbody\r\n"

		for _, raw := range []string{first, reply} {
			if _, err := u.Deliver(inboxName, []byte(raw), nil, time.Now()); err != nil {
				panic(err)
			}
		}
	})

	got := objects(t, mailCall(t, s, "Mailbox/get", `{"accountId":"test"}`))[inboxName]
	if got[propTotalEmails] != float64(2) || got[propTotalThreads] != float64(1) {
		t.Errorf("counts are %v emails and %v threads, want 2 and 1",
			got[propTotalEmails], got[propTotalThreads])
	}

	if got[propUnreadThreads] != float64(1) {
		t.Errorf("unreadThreads is %v, want 1", got[propUnreadThreads])
	}
}
