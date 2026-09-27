package jmap

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// The principal fixture: a directory of accounts named by address, each
// on a shard, and an alias. alice, bob, carol and erin live here, far on
// another shard; al is an alias of alice.
const (
	pAlice = "alice@example.org"
	pBob   = "bob@example.org"
	pCarol = "carol@example.org"
	pErin  = "erin@example.org"
	pFar   = "far@example.org"
	pAlias = "al@example.org"
	pNone  = "nobody@example.org"
	pDave  = "dave@example.org" // in an ACL of alice's, not in the directory
)

// directory is an authenticator over the fixture's accounts that counts
// its lookups.
type directory struct {
	shard   map[string]string // account -> shard
	alias   map[string]string // alias -> account
	lookups int
}

func newDirectory() *directory {
	return &directory{
		shard: map[string]string{pAlice: "here", pBob: "here", pCarol: "here", pErin: "here", pFar: "there"},
		alias: map[string]string{pAlias: pAlice},
	}
}

func (d *directory) result(account string) *auth.Result {
	return &auth.Result{Account: account, Attributes: map[string][]string{"mailShard": {d.shard[account]}}}
}

func (d *directory) Authenticate(_ context.Context, r auth.Request) (*auth.Result, error) {
	if _, ok := d.shard[r.Username]; !ok || r.Password != "secret" {
		return nil, auth.ErrInvalid
	}

	return d.result(r.Username), nil
}

func (d *directory) Lookup(_ context.Context, address, _ string, _ net.Addr) (*auth.Result, error) {
	d.lookups++

	if a, ok := d.alias[address]; ok {
		return d.result(a), nil
	}

	if _, ok := d.shard[address]; ok {
		return d.result(address), nil
	}

	return nil, auth.ErrInvalid
}

// principals is the fixture: alice owns Team (shared with bob, lrsp) and
// Solo (shared with dave, who is not in the directory, with "anyone" and
// with a negative entry); carol shares Carol with alice.
type principals struct {
	s     *Server
	dir   *directory
	store *memstore.Store
	alice model.User
	bob   model.User
	carol model.User
	team  string
}

// capsPrincipals are the capabilities the principal tests use.
const capsPrincipals = `,"urn:ietf:params:jmap:mail","urn:ietf:params:jmap:principals"`

func newPrincipals(t *testing.T, customize ...func(*Options)) *principals {
	t.Helper()

	f := &principals{dir: newDirectory(), store: memstore.New()}

	open := func(name string) model.User {
		u, err := f.store.User(name)
		if err != nil {
			t.Fatal(err)
		}

		t.Cleanup(u.Close)

		return u
	}

	f.alice, f.bob, f.carol = open(pAlice), open(pBob), open(pCarol)

	for _, u := range []model.User{f.alice, f.bob, f.carol} {
		mustCreate(t, u, inboxName)
	}

	f.team = createShared(t, f.alice, "Team", "")
	mustCreate(t, f.alice, "Solo")
	mustCreate(t, f.carol, "Carol")

	for _, g := range []struct {
		u               model.User
		box, id, rights string
	}{
		{f.alice, "Team", pBob, "lrsp"},
		{f.alice, "Solo", pDave, "lr"},
		{f.alice, "Solo", "anyone", "l"},
		{f.alice, "Solo", "-" + pBob, "r"},
		{f.carol, "Carol", pAlice, "lr"},
	} {
		if err := g.u.SetACL(g.box, g.id, g.rights); err != nil {
			t.Fatal(err)
		}
	}

	opts := append([]func(*Options){sharingOpts, func(o *Options) {
		o.Store = f.store
		o.Auth = f.dir
		o.Shard = ShardGate{Attribute: "mailShard", Value: "here"}
	}}, customize...)
	f.s = newTestServer(t, opts...)

	return f
}

// call is one method call as a user with the principal capabilities.
func (f *principals) call(t *testing.T, user, name, args string) (string, map[string]any) {
	t.Helper()

	return callAs(t, f.s, user, capsPrincipals, name, args)
}

// ok is a call that must not fail.
func (f *principals) ok(t *testing.T, user, name, args string) map[string]any {
	t.Helper()

	got, res := f.call(t, user, name, args)
	if got != name {
		t.Fatalf("%s answered %s: %v", name, got, res)
	}

	return res
}

// resultIDs are the ids of a /get list, or of a /query.
func resultIDs(res map[string]any) []string {
	var out []string

	if list, ok := res["list"].([]any); ok {
		for _, o := range list {
			out = append(out, o.(map[string]any)[propID].(string))
		}

		return out
	}

	for _, id := range res["ids"].([]any) {
		out = append(out, id.(string))
	}

	return out
}

// TestPrincipalSession: the session offers principals and mail:share, the
// personal account names the user as its principal and owner, and the
// principals are in the personal account.
func TestPrincipalSession(t *testing.T) {
	f := newPrincipals(t)
	sess := sessionAs(t, f.s, pAlice)

	for _, c := range []string{CapPrincipals, CapMailShare} {
		if string(sess.Capabilities[c]) != "{}" {
			t.Errorf("session capability %s is %s", c, sess.Capabilities[c])
		}
	}

	if sess.PrimaryAccounts[CapPrincipals] != pAlice {
		t.Errorf("the primary principals account is %q", sess.PrimaryAccounts[CapPrincipals])
	}

	own := sess.Accounts[pAlice].AccountCapabilities

	var p, o map[string]string

	if json.Unmarshal(own[CapPrincipals], &p) != nil || p["currentUserPrincipalId"] != pAlice {
		t.Errorf("principals capability: %s", own[CapPrincipals])
	}

	if json.Unmarshal(own[CapPrincipalsOwner], &o) != nil || o["principalId"] != pAlice || o["accountIdForPrincipal"] != pAlice {
		t.Errorf("principals:owner capability: %s", own[CapPrincipalsOwner])
	}

	carol := sess.Accounts[pCarol].AccountCapabilities
	if json.Unmarshal(carol[CapPrincipalsOwner], &o) != nil || o["principalId"] != pCarol || o["accountIdForPrincipal"] != pAlice {
		t.Errorf("carol's shared account names its owner as %s", carol[CapPrincipalsOwner])
	}
}

// TestPrincipalVisibility: without a lookup a user sees itself, the
// owners who share with it and the identifiers of its own ACLs — never
// "anyone", a negative entry or anybody else of the directory.
func TestPrincipalVisibility(t *testing.T) {
	f := newPrincipals(t)

	for _, c := range []struct {
		user string
		want []string
	}{
		{pAlice, []string{pAlice, pBob, pCarol, pDave}},
		{pBob, []string{pBob, pAlice}},
		{pCarol, []string{pCarol, pAlice}},
	} {
		res := f.ok(t, c.user, "Principal/get", `{"ids":null}`)
		if got := resultIDs(res); strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s sees %v, want %v", c.user, got, c.want)
		}
	}

	if f.dir.lookups != 0 {
		t.Errorf("listing the visible principals made %d lookups", f.dir.lookups)
	}

	res := f.ok(t, pAlice, "Principal/get", `{"ids":["`+pCarol+`","`+pAlice+`"]}`)

	for _, o := range res["list"].([]any) {
		p := o.(map[string]any)
		id := p[propID].(string)

		if p[propPrincipalType] != principalIndividual || p[propName] != id || p[propEmail] != id ||
			p[propDescription] != nil || p[propTimeZone] != nil {
			t.Errorf("principal %s is %v", id, p)
		}

		accounts, _ := p[propAccounts].(map[string]any)
		if _, ok := accounts[id]; !ok {
			t.Errorf("principal %s does not name its account: %v", id, p[propAccounts])
		}
	}
}

// TestPrincipalExactLookup: an email condition finds an account the user
// does not see only when the backend resolves the address to that very
// account on this shard; an unknown address, an alias and an account of
// another shard all find nothing. A part of an address looks nothing up.
func TestPrincipalExactLookup(t *testing.T) {
	for _, c := range []struct {
		name, filter string
		want         []string
		lookups      int
	}{
		{"visible", `{"email":"` + pBob + `"}`, []string{pBob}, 0},
		{"found", `{"email":"` + pErin + `"}`, []string{pErin}, 1},
		{"found in another case", `{"email":"ERIN@example.org"}`, []string{pErin}, 1},
		{"unknown", `{"email":"` + pNone + `"}`, nil, 1},
		{"other shard", `{"email":"` + pFar + `"}`, nil, 1},
		{"alias", `{"email":"` + pAlias + `"}`, nil, 1},
		{"part of an address", `{"email":"example.org"}`, []string{pAlice, pBob, pCarol, pDave}, 0},
		{"name part", `{"name":"erin"}`, nil, 0},
		{"or of two", `{"operator":"OR","conditions":[{"email":"` + pErin + `"},{"email":"` + pCarol + `"}]}`,
			[]string{pCarol, pErin}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newPrincipals(t)

			res := f.ok(t, pAlice, "Principal/query", `{"filter":`+c.filter+`,"sort":[{"property":"email"}]}`)
			got := resultIDs(res)

			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("found %v, want %v", got, c.want)
			}

			if f.dir.lookups != c.lookups {
				t.Errorf("%d lookups, want %d", f.dir.lookups, c.lookups)
			}
		})
	}
}

// TestPrincipalGetLooksUp: an id the user does not see is resolved as an
// exact address; anything else is notFound.
func TestPrincipalGetLooksUp(t *testing.T) {
	f := newPrincipals(t)

	res := f.ok(t, pBob, "Principal/get", `{"ids":["`+pErin+`","`+pFar+`","`+pNone+`","anyone","`+pAlias+`"],"properties":["email"]}`)

	if got := resultIDs(res); len(got) != 1 || got[0] != pErin {
		t.Errorf("found %v, want erin alone", got)
	}

	nf, _ := res["notFound"].([]any)
	if len(nf) != 4 {
		t.Errorf("notFound is %v", nf)
	}
}

// TestPrincipalLookupBudget: a request makes at most
// jmap.max_principal_lookups lookups; the same address twice costs one.
func TestPrincipalLookupBudget(t *testing.T) {
	f := newPrincipals(t, func(o *Options) { o.Principals.MaxLookups = 2 })

	got, res := f.call(t, pAlice, "Principal/query",
		`{"filter":{"operator":"OR","conditions":[{"email":"a1@example.org"},{"email":"a2@example.org"},{"email":"a3@example.org"}]}}`)
	if got != kwError || res["type"] != errRequestTooLarge {
		t.Errorf("three lookups with a budget of two answered %s %v", got, res)
	}

	f.dir.lookups = 0

	f.ok(t, pAlice, "Principal/query",
		`{"filter":{"operator":"OR","conditions":[{"email":"`+pErin+`"},{"email":"`+pErin+`"}]}}`)

	if f.dir.lookups != 1 {
		t.Errorf("the same address twice made %d lookups", f.dir.lookups)
	}
}

// TestPrincipalChangesAndSet: the current state has no changes, any other
// cannot be computed; principals cannot be changed.
func TestPrincipalChangesAndSet(t *testing.T) {
	f := newPrincipals(t)

	state := f.ok(t, pAlice, "Principal/get", `{"ids":[]}`)["state"].(string)

	res := f.ok(t, pAlice, "Principal/changes", `{"sinceState":"`+state+`"}`)
	if res["newState"] != state || len(res["created"].([]any)) != 0 {
		t.Errorf("changes since the current state: %v", res)
	}

	if got, res := f.call(t, pAlice, "Principal/changes", `{"sinceState":"old"}`); got != kwError ||
		res["type"] != errCannotCalculateChanges {
		t.Errorf("changes since another state: %s %v", got, res)
	}

	res = f.ok(t, pAlice, "Principal/set", `{"update":{"`+pAlice+`":{"name":"Eve"}},"create":{"x":{}}}`)
	if res["notUpdated"].(map[string]any)[pAlice] == nil || res["notCreated"].(map[string]any)["x"] == nil {
		t.Errorf("a principal was changed: %v", res)
	}

	// A grant to alice changes what she sees, and so the state.
	if err := f.bob.CreateMailbox("ForAlice", nil); err != nil {
		t.Fatal(err)
	}

	if err := f.bob.SetACL("ForAlice", pAlice, "lr"); err != nil {
		t.Fatal(err)
	}

	if now := f.ok(t, pAlice, "Principal/get", `{"ids":[]}`)["state"]; now == state {
		t.Error("a new owner sharing with the user left the principal state as it was")
	}
}

// TestPrincipalCapabilityRequired: the methods need the principals
// capability, and a Mailbox has no shareWith without it.
func TestPrincipalCapabilityRequired(t *testing.T) {
	f := newPrincipals(t)

	if got, _ := callAs(t, f.s, pAlice, `,"urn:ietf:params:jmap:mail"`, "Principal/get", `{}`); got != kwError {
		t.Errorf("Principal/get without the capability answered %s", got)
	}

	_, res := callAs(t, f.s, pAlice, `,"urn:ietf:params:jmap:mail"`, "Mailbox/get", `{"ids":["`+f.team+`"]}`)
	box := res["list"].([]any)[0].(map[string]any)

	if _, ok := box[propShareWith]; ok {
		t.Error("shareWith without the capability")
	}

	if _, ok := box[propMyRights].(map[string]any)[propMayShare]; ok {
		t.Error("mayShare without the capability")
	}
}
