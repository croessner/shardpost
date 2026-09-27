package jmap

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/provision"
)

// shardAuth is an authenticator whose result carries the shard attribute
// the backend assigns the account.
type shardAuth struct {
	auth.Static
	attrs map[string][]string
}

func (a shardAuth) Authenticate(ctx context.Context, r auth.Request) (*auth.Result, error) {
	res, err := a.Static.Authenticate(ctx, r)
	if err != nil {
		return nil, err
	}

	res.Attributes = a.attrs

	return res, nil
}

// TestShardGate: with a shard configured, only accounts the backend
// assigns to it are served; another shard or none is refused with 403,
// not 401, so a client does not ask for the password again. Without a
// shard configured every authenticated account is served.
func TestShardGate(t *testing.T) {
	cases := []struct {
		name  string
		gate  ShardGate
		attrs map[string][]string
		want  int
	}{
		{"no gate", ShardGate{}, nil, http.StatusOK},
		{"own shard", ShardGate{Attribute: "mailShard", Value: "shardpost"}, map[string][]string{"mailShard": {"shardpost"}}, http.StatusOK},
		{"own shard, other case", ShardGate{Attribute: "mailShard", Value: "shardpost"}, map[string][]string{"mailShard": {"ShardPost"}}, http.StatusOK},
		{"other shard", ShardGate{Attribute: "mailShard", Value: "shardpost"}, map[string][]string{"mailShard": {"mailstack"}}, http.StatusForbidden},
		{"no attribute", ShardGate{Attribute: "mailShard", Value: "shardpost"}, nil, http.StatusForbidden},
		{"empty value", ShardGate{Attribute: "mailShard", Value: "shardpost"}, map[string][]string{"mailShard": {""}}, http.StatusForbidden},
		{"several values, one ours", ShardGate{Attribute: "mailShard", Value: "shardpost"}, map[string][]string{"mailShard": {"mailstack", "shardpost"}}, http.StatusOK},
		{"attribute name in other case", ShardGate{Attribute: "mailShard", Value: "shardpost"}, map[string][]string{"mailshard": {"shardpost"}}, http.StatusOK},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &countingStore{Store: memstore.New()}
			s := newTestServer(t, func(o *Options) {
				o.Store = store
				o.Auth = shardAuth{Static: auth.Static{"test": "secret"}, attrs: c.attrs}
				o.Shard = c.gate
			})

			// A refused account is refused on every endpoint; an event
			// source that is let through would stay open, so only the
			// refused cases ask for one.
			if c.want != http.StatusOK {
				if w := do(t, s, http.MethodGet, PathEvents, ""); w.Code != c.want {
					t.Fatalf("event source: %d, want %d", w.Code, c.want)
				}
			}

			if w := do(t, s, http.MethodGet, PathSession, ""); w.Code != c.want {
				t.Fatalf("session: %d, want %d", w.Code, c.want)
			}

			if c.want == http.StatusForbidden && store.opened.Load() != 0 {
				t.Fatalf("the store was opened %d times for a refused account", store.opened.Load())
			}
		})
	}
}

// countingStore counts how often an account's store view is opened.
type countingStore struct {
	Store
	opened atomic.Int64
}

func (c *countingStore) User(name string) (model.User, error) {
	c.opened.Add(1)

	return c.Store.User(name)
}

// TestSessionProvisions: fetching the session gives an account that never
// logged in over IMAP its INBOX and the configured mailboxes, once.
func TestSessionProvisions(t *testing.T) {
	store := memstore.New()
	s := newTestServer(t, func(o *Options) {
		o.Store = store
		o.Autocreate = []provision.Folder{{Name: "Drafts", Use: `\Drafts`, Subscribe: true}, {Name: "Sent", Use: `\Sent`}}
	})

	if w := do(t, s, http.MethodGet, PathSession, ""); w.Code != http.StatusOK {
		t.Fatalf("session: %d", w.Code)
	}

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"INBOX", "Drafts", "Sent"} {
		if _, err := u.Lookup(name); err != nil {
			t.Fatalf("%s after the session: %v", name, err)
		}
	}

	subscribed := subscriptions(t, u)
	if !subscribed["INBOX"] || !subscribed["Drafts"] || subscribed["Sent"] {
		t.Fatalf("subscriptions after provisioning: %v", subscribed)
	}

	// An account that unsubscribed a mailbox keeps it that way: the
	// second session fetch creates nothing and subscribes nothing.
	if err := u.SetSubscribed("Drafts", false); err != nil {
		t.Fatal(err)
	}

	before := subscriptions(t, u)

	if w := do(t, s, http.MethodGet, PathSession, ""); w.Code != http.StatusOK {
		t.Fatalf("second session: %d", w.Code)
	}

	if after := subscriptions(t, u); len(after) != len(before) || after["Drafts"] {
		t.Fatalf("the second session fetch changed the account: %v, then %v", before, after)
	}
}

// subscriptions maps every mailbox of an account to whether it is
// subscribed.
func subscriptions(t *testing.T, u model.User) map[string]bool {
	t.Helper()

	boxes, err := u.ListMailboxes()
	if err != nil {
		t.Fatal(err)
	}

	out := map[string]bool{}
	for _, m := range boxes {
		out[m.Name] = m.Subscribed
	}

	return out
}
