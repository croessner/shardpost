package jmap

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// TestMissBudget: a bucket of misses per account that refills at its
// rate; the map of buckets stays bounded, full buckets going first and
// then the least recently used.
func TestMissBudget(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	m := newMissBudget(2)
	m.now = func() time.Time { return now }

	m.miss("a")
	m.miss("a")

	if m.allow("a") {
		t.Fatal("a third miss within the minute is allowed")
	}

	if !m.allow("b") {
		t.Fatal("another account is held back")
	}

	now = now.Add(30 * time.Second)

	if !m.allow("a") {
		t.Fatal("half a minute later the bucket has not refilled one miss")
	}

	m.buckets = map[string]*missBucket{}
	for i := range maxMissBuckets {
		m.buckets["u"+strconv.Itoa(i)] = &missBucket{tokens: 0, last: now, used: now.Add(time.Duration(i) * time.Millisecond)}
	}

	m.miss("new")

	if len(m.buckets) != maxMissBuckets || m.buckets["u0"] != nil || m.buckets["new"] == nil {
		t.Errorf("after one more: %d buckets, u0 kept %v", len(m.buckets), m.buckets["u0"] != nil)
	}

	now = now.Add(time.Hour)

	m.miss("newer")

	if len(m.buckets) != 1 {
		t.Errorf("full buckets were not dropped first: %d left", len(m.buckets))
	}
}

// TestPrincipalMissBudget: misses are bounded across requests; beyond the
// budget no lookup is made and the call is serverUnavailable (a shareWith
// rateLimit), while the principals a user sees still answer. Each lookup
// carries the client's address and is logged with a hash of the address,
// not the address.
func TestPrincipalMissBudget(t *testing.T) {
	var logs bytes.Buffer

	f := newPrincipals(t, func(o *Options) {
		o.Principals.MissPerMinute = 2
		o.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	})

	for _, a := range []string{pNone, pFar} {
		f.ok(t, pBob, "Principal/query", `{"filter":{"email":"`+a+`"}}`)
	}

	if f.dir.client != "192.0.2.1" {
		t.Errorf("the lookup carried the client address %q", f.dir.client)
	}

	checkBeyondBudget(t, f)

	if f.ok(t, pAlice, "Principal/query", `{"filter":{"email":"`+pErin+`"}}`); f.dir.lookups != 3 {
		t.Errorf("another account is held back: %d lookups", f.dir.lookups)
	}

	checkLookupLogs(t, logs.String())
}

// checkLookupLogs: every lookup is logged with a hash and its result,
// never with the address at Info.
func checkLookupLogs(t *testing.T, out string) {
	t.Helper()

	if !strings.Contains(out, "principal lookup") || !strings.Contains(out, "address_sha256=") ||
		!strings.Contains(out, "result=unknown") || !strings.Contains(out, "result=\"other shard\"") {
		t.Errorf("the lookups are not logged: %s", out)
	}

	if strings.Contains(out, pNone) || strings.Contains(out, pFar) {
		t.Errorf("an address is logged at Info: %s", out)
	}
}

// checkBeyondBudget: bob's budget is used up; lookups are refused, the
// principals he sees are not.
func checkBeyondBudget(t *testing.T, f *principals) {
	t.Helper()

	got, res := f.call(t, pBob, "Principal/query", `{"filter":{"email":"x1@example.org"}}`)
	if got != kwError || res["type"] != errServerUnavailable || f.dir.lookups != 2 {
		t.Errorf("a third miss answered %s %v after %d lookups", got, res, f.dir.lookups)
	}

	if res := f.ok(t, pBob, "Principal/get", `{"ids":["`+pAlice+`"]}`); len(resultIDs(res)) != 1 {
		t.Errorf("a visible principal is not served: %v", res)
	}

	if err := f.bob.CreateMailbox("Mine", nil); err != nil {
		t.Fatal(err)
	}

	mb, _ := f.bob.Lookup("Mine")

	res = f.ok(t, pBob, "Mailbox/set", `{"update":{"`+mb.ID+`":{"shareWith/x2@example.org":{"mayReadItems":true}}}}`)
	if serr, _ := res["notUpdated"].(map[string]any)[mb.ID].(map[string]any); serr["type"] != "rateLimit" {
		t.Errorf("a shareWith beyond the budget answered %v", res)
	}
}

// noACLStore hides ModifyACL, so that sharing a mailbox cannot be
// written.
type noACLStore struct{ *memstore.Store }

// noACLUser is a user view without ModifyACL.
type noACLUser struct{ model.User }

func (s noACLStore) User(name string) (model.User, error) {
	u, err := s.Store.User(name)

	return noACLUser{u}, err
}

// TestShareWithCreateRollsBack: a creation whose shareWith cannot be
// written is not created (RFC 8620 5.3: all or nothing).
func TestShareWithCreateRollsBack(t *testing.T) {
	f := newPrincipals(t, func(o *Options) { o.Store = noACLStore{o.Store.(*memstore.Store)} })

	res := f.ok(t, pAlice, "Mailbox/set", `{"create":{"n":{"name":"New","shareWith":{"bob@example.org":{"mayReadItems":true}}}}}`)
	if res["notCreated"].(map[string]any)["n"] == nil {
		t.Fatalf("created although the share failed: %v", res)
	}

	if _, err := f.alice.Lookup("New"); err == nil {
		t.Error("the mailbox was left behind")
	}
}
