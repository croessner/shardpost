package jmap

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// The sharing fixture: "test" is the user under test, "alice" shares
// mailboxes with it, "bob" shares one with "anyone" only (no pointer, so
// not an account of test's), and the public namespace holds folders with
// rights of every kind.
type sharing struct {
	s      *Server
	store  *memstore.Store
	test   model.User
	alice  model.User
	bob    model.User
	public model.User
	ids    map[string]string // "alice:Team" / "~public:News" -> mailbox id
}

// sharingOpts switches both namespaces on, as a default deployment does.
func sharingOpts(o *Options) {
	o.Sharing = SharingOptions{SharedPrefix: "Shared/", PublicPrefix: "Public/"}
}

// newSharing builds the fixture. The ACLs are the table of what test may
// do where.
func newSharing(t *testing.T, customize ...func(*Options)) *sharing {
	t.Helper()

	store := memstore.New()
	f := &sharing{store: store, ids: map[string]string{}}

	open := func(name string) model.User {
		u, err := store.User(name)
		if err != nil {
			t.Fatal(err)
		}

		return u
	}

	f.test, f.alice, f.bob, f.public = open("test"), open("alice"), open("bob"), open(model.PublicNamespace)

	for _, n := range []string{inboxName, "Mine"} {
		mustCreate(t, f.test, n)
	}

	for _, b := range fixtureBoxes {
		u := map[string]model.User{"alice": f.alice, "bob": f.bob, model.PublicNamespace: f.public}[b.owner]
		f.ids[b.owner+":"+b.name] = createShared(t, u, b.name, b.rights)
	}

	// bob shares with everyone, which leaves no pointer; News is readable
	// by everyone and writable by nobody.
	if err := f.bob.SetACL("Everyone", "anyone", "lr"); err != nil {
		t.Fatal(err)
	}

	if err := f.public.SetACL("News", "anyone", "lrs"); err != nil {
		t.Fatal(err)
	}

	opts := append([]func(*Options){sharingOpts, func(o *Options) {
		o.Store = store
		o.Auth = auth.Static{"test": "secret", "alice": "secret", "carol": "secret"}
	}}, customize...)
	f.s = newTestServer(t, opts...)

	return f
}

// fixtureBoxes are the mailboxes of the other namespaces, with test's
// rights on each ("" shares nothing).
var fixtureBoxes = []struct{ owner, name, rights string }{
	{"alice", inboxName, ""},
	{"alice", "Team", "lr"},
	{"alice", "Team/Sub", "lrswitekx"},
	{"alice", "Drop", "li"},
	{"alice", "Listed", "l"},
	{"alice", "Hidden", ""},
	{"alice", "Hidden/Child", "lr"},
	{"alice", "Secret", "r"}, // r without l: invisible
	{"bob", inboxName, ""},
	{"bob", "Everyone", ""},
	{model.PublicNamespace, "News", ""},
	{model.PublicNamespace, "Staff", "lrswite"},
	{model.PublicNamespace, "Closed", ""},
}

// createShared creates a mailbox, grants test the rights and returns
// the id.
func createShared(t *testing.T, u model.User, name, rights string) string {
	t.Helper()

	mustCreate(t, u, name)

	if rights != "" {
		if err := u.SetACL(name, "test", rights); err != nil {
			t.Fatal(err)
		}
	}

	mb, err := u.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}

	return mb.ID
}

// mustCreate creates a mailbox.
func mustCreate(t *testing.T, u model.User, name string) {
	t.Helper()

	if err := u.CreateMailbox(name, nil); err != nil {
		t.Fatal(err)
	}
}

// id is the mailbox id of "owner:name".
func (f *sharing) id(t *testing.T, key string) string {
	t.Helper()

	id, ok := f.ids[key]
	if !ok {
		t.Fatalf("no mailbox %s in the fixture", key)
	}

	return id
}

// callAs sends one method call as a user and returns the triple of its
// first response: the method name (or "error") and the arguments.
func callAs(t *testing.T, s *Server, user, using, name, args string) (string, map[string]any) {
	t.Helper()

	body := `{"using":["urn:ietf:params:jmap:core"` + using + `],"methodCalls":[["` + name + `",` + args + `,"c1"]]}`

	r := httptest.NewRequest(http.MethodPost, PathAPI, strings.NewReader(body))
	r.SetBasicAuth(user, "secret")

	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("api answered %d: %s", w.Code, w.Body.String())
	}

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	got, _ := responses(t, out)[0].([]any)
	res, _ := got[1].(map[string]any)

	return got[0].(string), res
}

// testMail is one call of test with the mail capability, failing on an
// error response.
func (f *sharing) mail(t *testing.T, name, args string) map[string]any {
	t.Helper()

	got, res := callAs(t, f.s, "test", `,"urn:ietf:params:jmap:mail"`, name, args)
	if got != name {
		t.Fatalf("%s answered %s: %#v", name, got, res)
	}

	checkAccountID(t, name, args, res)

	return res
}

// checkAccountID: a response names the account the call named — a
// shared call must not echo the personal account.
func checkAccountID(t *testing.T, name, args string, res map[string]any) {
	t.Helper()

	var named struct {
		AccountID string `json:"accountId"`
	}

	if err := json.Unmarshal([]byte(args), &named); err != nil || named.AccountID == "" {
		return
	}

	if res["accountId"] != named.AccountID {
		t.Errorf("%s in %s answered for account %v", name, named.AccountID, res["accountId"])
	}
}

// mailErr is one call of test that has to fail; it returns the error
// type.
func (f *sharing) mailErr(t *testing.T, name, args string) string {
	t.Helper()

	got, res := callAs(t, f.s, "test", `,"urn:ietf:params:jmap:mail"`, name, args)
	if got != kwError {
		t.Fatalf("%s answered %s, want an error: %#v", name, got, res)
	}

	typ, _ := res["type"].(string)

	return typ
}

// sessionAs fetches the session resource as a user.
func sessionAs(t *testing.T, s *Server, user string) *Session {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, PathSession, nil)
	r.SetBasicAuth(user, "secret")

	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("session answered %d: %s", w.Code, w.Body.String())
	}

	var out Session
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	return &out
}

// TestSharingSessionAccounts: the session lists an account per owner who
// shares a visible mailbox and one for the public folders, with the mail
// capability alone; primary stays personal.
func TestSharingSessionAccounts(t *testing.T) {
	f := newSharing(t)

	sess := sessionAs(t, f.s, "test")

	if got := len(sess.Accounts); got != 3 {
		t.Fatalf("the session lists %d accounts, want test, alice and ~public: %v", got, sess.Accounts)
	}

	alice, pub := sess.Accounts["alice"], sess.Accounts[model.PublicNamespace]
	if alice.IsPersonal || alice.Name != "alice" || alice.IsReadOnly {
		t.Errorf("the account of alice is %+v", alice)
	}

	if pub.IsPersonal || pub.Name != "Public" {
		t.Errorf("the public account is %+v", pub)
	}

	for id, a := range map[string]Account{"alice": alice, model.PublicNamespace: pub} {
		checkSharedCapabilities(t, id, a)
	}

	if sess.PrimaryAccounts[CapMail] != "test" {
		t.Errorf("the primary mail account is %q", sess.PrimaryAccounts[CapMail])
	}

	if _, ok := sess.Accounts["bob"]; ok {
		t.Error("an account shared only with anyone is listed: it has no pointer, as in IMAP LIST")
	}
}

// checkSharedCapabilities: a shared account offers core and mail, and no
// top-level mailbox.
func checkSharedCapabilities(t *testing.T, id string, a Account) {
	t.Helper()

	if len(a.AccountCapabilities) != 2 || a.AccountCapabilities[CapMail] == nil || a.AccountCapabilities[CapCore] == nil {
		t.Errorf("%s offers %v, want core and mail only", id, a.AccountCapabilities)
	}

	var mail mailCapability
	if err := json.Unmarshal(a.AccountCapabilities[CapMail], &mail); err != nil || mail.MayCreateTopLevelMailbox {
		t.Errorf("%s: mayCreateTopLevelMailbox is not false: %s", id, a.AccountCapabilities[CapMail])
	}
}

// TestSharingSessionFollowsGrants: an account disappears with the last
// revocation and comes back, read-only, with l alone; the session state
// changes each time.
func TestSharingSessionFollowsGrants(t *testing.T) {
	f := newSharing(t)
	sess := sessionAs(t, f.s, "test")

	for _, n := range []string{"Team", "Team/Sub", "Drop", "Listed", "Hidden/Child", "Secret"} {
		if err := f.alice.SetACL(n, "test", ""); err != nil {
			t.Fatal(err)
		}
	}

	after := sessionAs(t, f.s, "test")
	if _, ok := after.Accounts["alice"]; ok || after.State == sess.State {
		t.Errorf("after the revocation: accounts %v, state %s (was %s)", after.Accounts, after.State, sess.State)
	}

	if err := f.alice.SetACL("Listed", "test", "l"); err != nil {
		t.Fatal(err)
	}

	again := sessionAs(t, f.s, "test")
	if a, ok := again.Accounts["alice"]; !ok || !a.IsReadOnly || again.State == after.State {
		t.Errorf("with l alone the account is %+v (listed %v), want it read-only", a, ok)
	}
}

// TestSharingSwitchedOff: an empty prefix switches a namespace off for
// JMAP as it does for IMAP, and the bound on shared accounts holds.
func TestSharingSwitchedOff(t *testing.T) {
	off := newSharing(t, func(o *Options) { o.Sharing = SharingOptions{} })

	if got := sessionAs(t, off.s, "test").Accounts; len(got) != 1 {
		t.Errorf("with both namespaces off the session lists %v", got)
	}

	if typ := off.mailErr(t, "Mailbox/get", `{"accountId":"alice"}`); typ != errAccountNotFound {
		t.Errorf("a shared account with the namespace off answered %s", typ)
	}

	publicOnly := newSharing(t, func(o *Options) { o.Sharing.SharedPrefix = "" })
	if got := sessionAs(t, publicOnly.s, "test").Accounts; len(got) != 2 || got["alice"].Name != "" {
		t.Errorf("with shared_prefix empty the session lists %v", got)
	}

	// A second owner, and a bound of one: the first owner by name is
	// listed, and the other is still reachable by its id.
	bounded := newSharing(t, func(o *Options) { o.Sharing.MaxAccounts = 1 })

	carol, _ := bounded.store.User("carol")
	mustCreate(t, carol, "Shared")

	if err := carol.SetACL("Shared", "test", "lr"); err != nil {
		t.Fatal(err)
	}

	got := sessionAs(t, bounded.s, "test").Accounts
	if _, ok := got["alice"]; !ok || len(got) != 3 {
		t.Errorf("with a bound of one the session lists %v, want test, alice and ~public", got)
	}

	if res := bounded.mail(t, "Mailbox/get", `{"accountId":"carol"}`); len(res["list"].([]any)) != 1 {
		t.Errorf("the account beyond the bound is not reachable: %#v", res)
	}
}

// TestSharingAccountNotFound: an account that shares nothing with the
// user, one that does not exist, a reserved namespace and one shared
// with anyone only are all accountNotFound; so are the methods whose
// capability a shared account does not have.
func TestSharingAccountNotFound(t *testing.T) {
	f := newSharing(t, func(o *Options) {
		o.VacationResponse = true
		o.Submission = &SubmissionOptions{AddressAttributes: []string{"mail"}}
	})

	for _, id := range []string{"bob", "nobody", "~admin", "test2", "ALICE"} {
		if typ := f.mailErr(t, "Mailbox/get", `{"accountId":"`+id+`"}`); typ != errAccountNotFound {
			t.Errorf("Mailbox/get of %s answered %s", id, typ)
		}
	}

	calls := []struct{ using, name string }{
		{`,"urn:ietf:params:jmap:quota"`, "Quota/get"},
		{`,"urn:ietf:params:jmap:blob"`, "Blob/get"},
		{`,"urn:ietf:params:jmap:vacationresponse"`, "VacationResponse/get"},
		{`,"urn:ietf:params:jmap:submission"`, "Identity/get"},
	}

	for _, c := range calls {
		for _, id := range []string{"alice", model.PublicNamespace} {
			got, res := callAs(t, f.s, "test", c.using, c.name, `{"accountId":"`+id+`","ids":[]}`)
			if got != kwError || res["type"] != errAccountNotFound {
				t.Errorf("%s in %s answered %s %v, want accountNotFound", c.name, id, got, res)
			}
		}
	}
}

// TestSharingMailboxVisibility: only mailboxes with l are there; a
// visible one below an invisible level keeps the level in its name; the
// rights map onto myRights as documented; counts need r.
func TestSharingMailboxVisibility(t *testing.T) {
	f := newSharing(t)

	deliver(t, f.alice, "Team", plainMessage)
	deliver(t, f.alice, "Listed", plainMessage)

	got := objects(t, f.mail(t, "Mailbox/get", `{"accountId":"alice"}`))

	want := []string{"Team", "Sub", "Drop", "Listed", "Hidden/Child"}
	if len(got) != len(want) {
		t.Fatalf("alice's account shows %d mailboxes, want %v: %v", len(got), want, got)
	}

	for _, n := range want {
		if got[n] == nil {
			t.Errorf("%s is missing", n)
		}
	}

	if got["Sub"][propParentID] != f.id(t, "alice:Team") || got["Hidden/Child"][propParentID] != nil {
		t.Errorf("the tree: Sub under %v, Hidden/Child under %v", got["Sub"][propParentID], got["Hidden/Child"][propParentID])
	}

	if got["Team"][propTotalEmails] != float64(1) || got["Listed"][propTotalEmails] != float64(0) {
		t.Errorf("counts: Team %v, Listed (no r) %v", got["Team"][propTotalEmails], got["Listed"][propTotalEmails])
	}

	// A mailbox without l is not found, by id either.
	res := f.mail(t, "Mailbox/get", `{"accountId":"alice","ids":["`+f.id(t, "alice:Secret")+`","`+
		f.id(t, "alice:Hidden")+`","`+f.id(t, "alice:INBOX")+`"]}`)
	if nf, _ := res["notFound"].([]any); len(nf) != 3 || len(res["list"].([]any)) != 0 {
		t.Errorf("invisible mailboxes: %#v", res)
	}
}

// TestSharingMyRights: the RFC 4314 rights map onto myRights as
// documented; mayCreateChild stays false (see descNoSharedCreate).
func TestSharingMyRights(t *testing.T) {
	f := newSharing(t)
	got := objects(t, f.mail(t, "Mailbox/get", `{"accountId":"alice","properties":["name","myRights"]}`))

	rights := []struct {
		box  string
		want mailboxRights
	}{
		{"Team", mailboxRights{MayReadItems: true}},
		{"Sub", mailboxRights{MayReadItems: true, MayAddItems: true, MayRemoveItems: true, MaySetSeen: true,
			MaySetKeywords: true, MayRename: true, MayDelete: true}},
		{"Drop", mailboxRights{MayAddItems: true}},
		{"Listed", mailboxRights{}},
	}

	for _, r := range rights {
		var have mailboxRights

		raw, _ := json.Marshal(got[r.box][propMyRights])
		if err := json.Unmarshal(raw, &have); err != nil || have != r.want {
			t.Errorf("myRights of %s are %+v, want %+v", r.box, have, r.want)
		}
	}
}

// TestSharingMailboxINBOX: the owner's INBOX, shared with every right,
// can be neither renamed nor deleted.
func TestSharingMailboxINBOX(t *testing.T) {
	f := newSharing(t)

	if err := f.alice.SetACL(inboxName, "test", "lrswipkxtea"); err != nil {
		t.Fatal(err)
	}

	got := objects(t, f.mail(t, "Mailbox/get", `{"accountId":"alice"}`))[inboxName]

	rights, _ := got[propMyRights].(map[string]any)
	if rights["mayRename"] != false || rights["mayDelete"] != false || rights["mayReadItems"] != true ||
		got[propRole] != "inbox" {
		t.Errorf("alice's INBOX: %#v", got)
	}

	res := f.mail(t, "Mailbox/set", `{"accountId":"alice","destroy":["`+f.id(t, "alice:INBOX")+`"]}`)
	if typ := setErrorType(res, "notDestroyed", f.id(t, "alice:INBOX")); typ != setForbidden {
		t.Errorf("destroying alice's INBOX gave %q", typ)
	}
}

// TestSharingMailboxSubscriptions: isSubscribed is the user's own
// subscription, and setting it writes the user's pointer (shared) or its
// public subscription, never the owner's flag.
func TestSharingMailboxSubscriptions(t *testing.T) {
	f := newSharing(t)
	team := f.id(t, "alice:Team")

	if err := f.alice.SetSubscribed("Team", true); err != nil {
		t.Fatal(err)
	}

	got := objects(t, f.mail(t, "Mailbox/get", `{"accountId":"alice","properties":["name","isSubscribed"]}`))
	if got["Team"][propIsSubscribed] != false {
		t.Errorf("the owner's subscription shows through: %#v", got["Team"])
	}

	before := f.mail(t, "Mailbox/get", `{"accountId":"alice","ids":[]}`)["state"].(string)

	res := f.mail(t, "Mailbox/set", `{"accountId":"alice","update":{"`+team+`":{"isSubscribed":true}}}`)
	if _, ok := res["updated"].(map[string]any)[team]; !ok {
		t.Fatalf("subscribing failed: %#v", res)
	}

	refs, _ := f.test.ListShared()
	subscribed := false

	for _, r := range refs {
		if r.Owner == "alice" && r.Mailbox == "Team" {
			subscribed = r.Subscribed
		}
	}

	if !subscribed {
		t.Error("the pointer is not subscribed")
	}

	if mb, _ := f.alice.Lookup("Team"); !mb.Subscribed {
		t.Error("the owner's own subscription changed")
	}

	// The subscription is in no log, so the state token changes with it
	// and a client that held the old one resynchronises.
	if typ := f.mailErr(t, "Mailbox/changes", `{"accountId":"alice","sinceState":"`+before+`"}`); typ != errCannotCalculateChanges {
		t.Errorf("Mailbox/changes across a subscription gave %s", typ)
	}
}

// TestSharingPublicSubscriptions: a public folder is subscribed in the
// user's own list of public subscriptions.
func TestSharingPublicSubscriptions(t *testing.T) {
	f := newSharing(t)
	staff := f.id(t, "~public:Staff")

	res := f.mail(t, "Mailbox/set", `{"accountId":"~public","update":{"`+staff+`":{"isSubscribed":true}}}`)
	if _, ok := res["updated"].(map[string]any)[staff]; !ok {
		t.Fatalf("subscribing to a public folder failed: %#v", res)
	}

	if subs, _ := f.test.PublicSubscriptions(); !subs[staff] {
		t.Error("the public subscription is not the user's")
	}

	q := f.mail(t, "Mailbox/query", `{"accountId":"~public","filter":{"isSubscribed":true}}`)
	if ids, _ := q["ids"].([]any); len(ids) != 1 || ids[0] != staff {
		t.Errorf("the query by subscription found %v", q["ids"])
	}
}

// TestSharingMailboxSet: creation in another namespace is refused; a
// rename needs x and k on the new parent; the sort order needs x; a
// deletion needs x. Nothing is written when a right is missing.
func TestSharingMailboxSet(t *testing.T) {
	f := newSharing(t)
	team, sub, listed := f.id(t, "alice:Team"), f.id(t, "alice:Team/Sub"), f.id(t, "alice:Listed")

	res := f.mail(t, "Mailbox/set", `{"accountId":"alice","create":{"n":{"name":"New","parentId":"`+sub+`"}}}`)
	if typ := setErrorType(res, "notCreated", "n"); typ != setForbidden {
		t.Errorf("creating in alice's account gave %q", typ)
	}

	cases := []struct {
		name  string
		id    string
		patch string
		want  string // "" = updated
	}{
		{"rename without x", team, `{"name":"Renamed"}`, setForbidden},
		{"sort order without x", team, `{"sortOrder":5}`, setForbidden},
		{"rename to the top level", sub, `{"parentId":null}`, setForbidden},
		{"move below a mailbox without k", sub, `{"parentId":"` + listed + `"}`, setForbidden},
		{"rename in place without k on the parent", sub, `{"name":"Sub2"}`, setForbidden},
		{"sort order with x", sub, `{"sortOrder":7}`, ""},
	}

	for _, c := range cases {
		res := f.mail(t, "Mailbox/set", `{"accountId":"alice","update":{"`+c.id+`":`+c.patch+`}}`)

		if got := setErrorType(res, "notUpdated", c.id); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}

	if _, err := f.alice.Lookup("Team/Sub"); err != nil {
		t.Errorf("a refused rename moved the mailbox: %v", err)
	}

	// With k on Team, Sub can be renamed below it.
	if err := f.alice.SetACL("Team", "test", "lrk"); err != nil {
		t.Fatal(err)
	}

	res = f.mail(t, "Mailbox/set", `{"accountId":"alice","update":{"`+sub+`":{"name":"Sub2"}}}`)
	if _, ok := res["updated"].(map[string]any)[sub]; !ok {
		t.Fatalf("a rename with x and k failed: %#v", res)
	}

	if _, err := f.alice.Lookup("Team/Sub2"); err != nil {
		t.Errorf("the rename did not happen: %v", err)
	}

	res = f.mail(t, "Mailbox/set", `{"accountId":"alice","destroy":["`+team+`"]}`)
	if typ := setErrorType(res, "notDestroyed", team); typ != setForbidden {
		t.Errorf("destroying without x gave %q", typ)
	}

	res = f.mail(t, "Mailbox/set", `{"accountId":"alice","destroy":["`+sub+`"]}`)
	if len(res["destroyed"].([]any)) != 1 {
		t.Errorf("destroying with x failed: %#v", res)
	}
}

// TestSharingMailboxChanges: the changes of a shared account are the
// owner's log filtered to what the user sees; a change of the user's
// rights makes the token stale instead of producing a wrong delta.
func TestSharingMailboxChanges(t *testing.T) {
	f := newSharing(t)
	team := f.id(t, "alice:Team")

	state := f.mail(t, "Mailbox/get", `{"accountId":"alice","ids":[]}`)["state"].(string)

	// Changes the user must not hear of: an invisible mailbox, messages
	// in a mailbox without r.
	mustCreate(t, f.alice, "Other")
	deliver(t, f.alice, "Listed", plainMessage)
	// One it must: a message in Team.
	deliver(t, f.alice, "Team", plainMessage)

	res := f.mail(t, "Mailbox/changes", `{"accountId":"alice","sinceState":"`+state+`"}`)
	if upd, _ := res["updated"].([]any); len(upd) != 1 || upd[0] != team || len(res["created"].([]any)) != 0 {
		t.Errorf("changes: %#v", res)
	}

	next := res["newState"].(string)

	// A grant is a change of what the user sees.
	if err := f.alice.SetACL("Other", "test", "lr"); err != nil {
		t.Fatal(err)
	}

	if typ := f.mailErr(t, "Mailbox/changes", `{"accountId":"alice","sinceState":"`+next+`"}`); typ != errCannotCalculateChanges {
		t.Errorf("changes across a grant gave %s", typ)
	}

	// A token of the personal account is no token of a shared one.
	own := f.mail(t, "Mailbox/get", `{"accountId":"test","ids":[]}`)["state"].(string)
	if typ := f.mailErr(t, "Mailbox/changes", `{"accountId":"alice","sinceState":"`+own+`"}`); typ != errCannotCalculateChanges {
		t.Errorf("a personal token in a shared account gave %s", typ)
	}
}
