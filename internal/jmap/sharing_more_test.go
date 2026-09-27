package jmap

import (
	"strings"
	"sync"
	"testing"

	"shardpost/internal/model"
)

// openCounter counts which namespaces a server opens.
type openCounter struct {
	Store
	mu     sync.Mutex
	opened map[string]int
}

func (c *openCounter) User(name string) (model.User, error) {
	c.mu.Lock()
	c.opened[name]++
	c.mu.Unlock()

	return c.Store.User(name)
}

// count is how often a namespace was opened.
func (c *openCounter) count(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.opened[name]
}

// TestSharingSessionStateIsCheap: the session state every API response
// carries opens no owner's namespace, and still changes when a grant
// changes what the user may do.
func TestSharingSessionStateIsCheap(t *testing.T) {
	var counter *openCounter

	f := newSharing(t, func(o *Options) {
		counter = &openCounter{Store: o.Store, opened: map[string]int{}}
		o.Store = counter
	})

	echo := func() string {
		out := call(t, f.s, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c1"]]}`)
		state, _ := out["sessionState"].(string)

		return state
	}

	before := echo()

	if n := counter.count("alice"); n != 0 {
		t.Fatalf("an API response opened alice's namespace %d times", n)
	}

	if sessionAs(t, f.s, "test").State != before {
		t.Error("the session resource and the API response disagree on the state")
	}

	// A write right on one mailbox makes alice's account writable: the
	// pointer is rewritten with the new generation, and the state moves.
	if err := f.alice.SetACL("Listed", "test", "lw"); err != nil {
		t.Fatal(err)
	}

	if echo() == before {
		t.Error("the session state did not change with a grant")
	}

	if n := counter.count("alice"); n != 1 {
		t.Errorf("alice's namespace was opened %d times, want once (by the session resource)", n)
	}
}

// TestSharingMailboxCap: a shared account examines at most
// max_shared_mailboxes pointers, the first by name, every time; the
// session resource examines that many in all, and an owner beyond it is
// left out of the resource but reachable by its id.
func TestSharingMailboxCap(t *testing.T) {
	f := newSharing(t, func(o *Options) { o.Sharing.MaxMailboxes = 2 })

	carol, _ := f.store.User("carol")
	createShared(t, carol, "Shared", "lr")

	// alice's pointers by name: Drop, Hidden/Child, Listed, Secret, ...
	got := objects(t, f.mail(t, "Mailbox/get", `{"accountId":"alice","properties":["name"]}`))
	if len(got) != 2 || got["Drop"] == nil || got["Hidden/Child"] == nil {
		t.Errorf("the capped account shows %v", got)
	}

	accounts := sessionAs(t, f.s, "test").Accounts
	if _, ok := accounts["carol"]; ok || accounts["alice"].Name != "alice" {
		t.Errorf("the session lists %v, want alice and not carol beyond the budget", accounts)
	}

	if res := f.mail(t, "Mailbox/get", `{"accountId":"carol","properties":["name"]}`); len(res["list"].([]any)) != 1 {
		t.Errorf("carol is not reachable by id: %#v", res)
	}

	// The public account is capped by name as well: Closed has no l for
	// test, so News and Staff are the first two it sees.
	pub := objects(t, f.mail(t, "Mailbox/get", `{"accountId":"~public","properties":["name"]}`))
	if len(pub) != 2 {
		t.Errorf("the public account shows %v", pub)
	}
}

// TestSharingMailboxQueryAccount: Mailbox/query answers for the shared
// account it was asked about.
func TestSharingMailboxQueryAccount(t *testing.T) {
	f := newSharing(t)

	res := f.mail(t, "Mailbox/query", `{"accountId":"alice"}`)
	if res["accountId"] != "alice" || len(res["ids"].([]any)) != 5 {
		t.Errorf("Mailbox/query in alice: %#v", res)
	}
}

// TestSharingDestroyWithHiddenChildren: a mailbox whose only children are
// invisible is destroyed as IMAP DELETE destroys it, the children stay;
// without r the destruction needs onDestroyRemoveEmails, so that whether
// the mailbox is empty is never revealed.
func TestSharingDestroyWithHiddenChildren(t *testing.T) {
	f := newSharing(t)
	parent := createShared(t, f.alice, "Parent", "lx")
	createShared(t, f.alice, "Parent/Inner", "")

	res := f.mail(t, "Mailbox/set", `{"accountId":"alice","destroy":["`+parent+`"]}`)
	if typ := setErrorType(res, "notDestroyed", parent); typ != setForbidden {
		t.Errorf("destroying without r and without onDestroyRemoveEmails gave %q", typ)
	}

	res = f.mail(t, "Mailbox/set", `{"accountId":"alice","destroy":["`+parent+`"],"onDestroyRemoveEmails":true}`)
	if len(strList(res["destroyed"])) != 1 {
		t.Fatalf("destroying with onDestroyRemoveEmails: %#v", res)
	}

	if _, err := f.alice.Lookup("Parent/Inner"); err != nil {
		t.Errorf("the invisible child went with its parent: %v", err)
	}
}

// TestSharingEmailQueryUnreadable: a text that only an unreadable
// mailbox holds finds nothing, and inMailbox with an unreadable id is
// empty.
func TestSharingEmailQueryUnreadable(t *testing.T) {
	f := newSharing(t)

	deliver(t, f.alice, "Hidden", "Subject: unicorn\r\n\r\nbody\r\n")
	deliver(t, f.alice, "Listed", "Subject: unicorn\r\n\r\nbody\r\n")
	deliver(t, f.alice, "Team", "Subject: other\r\n\r\nbody\r\n")

	for _, filter := range []string{`{"text":"unicorn"}`, `{"subject":"unicorn"}`,
		`{"inMailbox":"` + f.id(t, "alice:Listed") + `"}`, `{"inMailbox":"` + f.id(t, "alice:Secret") + `"}`} {
		if got := strList(f.mail(t, "Email/query", `{"accountId":"alice","filter":`+filter+`}`)["ids"]); len(got) != 0 {
			t.Errorf("%s found %v", filter, got)
		}
	}
}

// TestSharingPublicKeywordRights: with w and without s a reader may flag
// a message of a public folder but not mark it read; two readers' $seen
// queries diverge.
func TestSharingPublicKeywordRights(t *testing.T) {
	f := newSharing(t)

	if err := f.public.SetACL("Staff", "test", "lrwi"); err != nil {
		t.Fatal(err)
	}

	if err := f.public.SetACL("News", "carol", "lrs"); err != nil {
		t.Fatal(err)
	}

	staff := deliver(t, f.public, "Staff", plainMessage)
	news := deliver(t, f.public, "News", plainMessage)

	res := f.mail(t, "Email/set", `{"accountId":"~public","update":{"`+staff.GUID+`":{"keywords/$flagged":true},`+
		`"`+news.GUID+`":{"keywords/$seen":true}}}`)
	if _, ok := res["updated"].(map[string]any)[staff.GUID]; !ok {
		t.Errorf("$flagged with w failed: %#v", res["notUpdated"])
	}

	if _, ok := res["updated"].(map[string]any)[news.GUID]; !ok {
		t.Errorf("$seen with s failed: %#v", res["notUpdated"])
	}

	res = f.mail(t, "Email/set", `{"accountId":"~public","update":{"`+staff.GUID+`":{"keywords/$seen":true}}}`)
	if typ := setErrorType(res, "notUpdated", staff.GUID); typ != setForbidden {
		t.Errorf("$seen without s gave %q", typ)
	}

	query := `{"accountId":"~public","filter":{"inMailbox":"` + f.id(t, "~public:News") + `","notKeyword":"$seen"}}`

	if got := strList(f.mail(t, "Email/query", query)["ids"]); len(got) != 0 {
		t.Errorf("test's unseen News: %v", got)
	}

	name, carol := callAs(t, f.s, "carol", `,"urn:ietf:params:jmap:mail"`, "Email/query", query)
	if got := strList(carol["ids"]); name != "Email/query" || len(got) != 1 || !strings.EqualFold(got[0], news.GUID) {
		t.Errorf("carol's unseen News: %s %v", name, carol)
	}
}
