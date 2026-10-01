package jmap

import (
	"testing"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// limitServer is emailServer with the store's account limits set.
func limitServer(t *testing.T, l model.Limits) (*Server, model.User) {
	t.Helper()

	store := memstore.New()
	store.SetLimits(l)

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []string{inboxName, "Archive"} {
		if err := u.CreateMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	return newTestServer(t, func(o *Options) { o.Store = store }), u
}

// TestMailboxSetCreateAtLimit: a creation beyond limits.mailboxes is
// overQuota (RFC 8620 5.3).
func TestMailboxSetCreateAtLimit(t *testing.T) {
	s, _ := limitServer(t, model.Limits{Mailboxes: 3})

	res := mailCall(t, s, "Mailbox/set", `{"accountId":"test","create":{"a":{"name":"One"},"b":{"name":"Two"}}}`)

	created, _ := res["created"].(map[string]any)
	if len(created) != 1 {
		t.Fatalf("created %#v", res)
	}

	failed := "a"
	if created["a"] != nil {
		failed = "b"
	}

	if typ := setErrorType(res, "notCreated", failed); typ != setOverQuota {
		t.Fatalf("the creation beyond the limit gave %q: %#v", typ, res["notCreated"])
	}
}

// TestEmailSetKeywordsAtLimit: a keyword the mailboxes of the email may
// not take any more makes the update tooLarge; known keywords pass.
func TestEmailSetKeywordsAtLimit(t *testing.T) {
	s, u := limitServer(t, model.Limits{KeywordsPerMailbox: 1})
	m := deliver(t, u, inboxName, plainMessage, "known")

	res := mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+m.GUID+`":{"keywords/other":true}}}`)
	if typ := setErrorType(res, "notUpdated", m.GUID); typ != setTooLarge {
		t.Fatalf("the update gave %q: %#v", typ, res)
	}

	res = mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+m.GUID+`":{"keywords/$flagged":true,"keywords/known":true}}}`)
	if _, ok := res["updated"].(map[string]any)[m.GUID]; !ok {
		t.Fatalf("the update of known keywords failed: %#v", res)
	}
}

// TestShareWithAtLimit: a shareWith that would share one mailbox more
// with a principal than limits.shared_pointers is overQuota, and the ACL
// stays as it was.
func TestShareWithAtLimit(t *testing.T) {
	f := newPrincipals(t)
	f.store.SetLimits(model.Limits{SharedPointers: 1}) // bob already sees Team

	mb, err := f.carol.Lookup("Carol")
	if err != nil {
		t.Fatal(err)
	}

	res := f.ok(t, pCarol, "Mailbox/set", `{"accountId":"`+pCarol+`","update":{"`+mb.ID+`":{"shareWith/`+pBob+`":{"mayReadItems":true}}}}`)
	if typ := setErrorType(res, "notUpdated", mb.ID); typ != setOverQuota {
		t.Fatalf("the share gave %q: %#v", typ, res)
	}

	if acl := aclOf(t, f.carol, "Carol"); acl[pBob] != "" {
		t.Fatalf("bob was granted %q", acl[pBob])
	}
}
