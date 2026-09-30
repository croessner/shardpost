package migrate

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"shardpost/internal/auth"
	"shardpost/internal/imapproto"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// publicEnv is newEnv with a source that serves a public namespace
// ("Public/") to alice and bob.
func publicEnv(t *testing.T) *env {
	t.Helper()

	e := newEnv(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := imapproto.New(imapproto.Options{Store: e.src, Auth: auth.Static{"alice": "pw", "bob": "pw"}, InsecureAuth: true, Log: log,
		PublicPrefix: "Public/"})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	e.addr = ln.Addr().String()

	return e
}

func named(t *testing.T, s *memstore.Store, name string) model.User {
	t.Helper()

	u, err := s.User(name)
	if err != nil {
		t.Fatal(err)
	}

	return u
}

// fillPublic creates two public folders at the source. Lists/A holds
// three messages (the third carries the shared \Seen) and is open to
// alice and bob; Lists/B is alice's, bob only sees that it exists. Alice has read 1, bob 2.
func fillPublic(t *testing.T, e *env) {
	t.Helper()

	pub := named(t, e.src, model.PublicNamespace)

	for _, name := range []string{"Lists/A", "Lists/B"} {
		if err := pub.CreateMailbox(name, nil); err != nil {
			t.Fatal(err)
		}

		if err := pub.SetACL(name, "alice", "lrswipkxte"); err != nil {
			t.Fatal(err)
		}
	}

	if err := pub.SetACL("Lists/A", "bob", "lrs"); err != nil {
		t.Fatal(err)
	}

	// Bob can list Lists/B but not open it.
	if err := pub.SetACL("Lists/B", "bob", "l"); err != nil {
		t.Fatal(err)
	}

	for _, s := range []string{"a", "b", "c"} {
		if _, err := pub.Deliver("Lists/A", msg(s), nil, time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := pub.Deliver("Lists/B", msg("x"), []string{`\Flagged`}, time.Time{}); err != nil {
		t.Fatal(err)
	}

	if _, err := pub.SetFlags("Lists/A", []uint32{3}, 0, func([]string) []string { return []string{`\Seen`} }); err != nil {
		t.Fatal(err)
	}

	markSeen(t, pub, named(t, e.src, "alice"), "Lists/A", 1)
	markSeen(t, pub, named(t, e.src, "bob"), "Lists/A", 2)

	a := mustInfo(t, pub, "Lists/A")
	if err := named(t, e.src, "bob").SetPublicSubscribed(a.ID, true); err != nil {
		t.Fatal(err)
	}
}

// markSeen sets a reader's own \Seen the way a STORE does.
func markSeen(t *testing.T, pub, reader model.User, name string, uid uint32) {
	t.Helper()

	info := mustInfo(t, pub, name)

	if _, err := pub.TouchModSeq(name, []uint32{uid}, func(modseq uint64) error {
		return reader.SetPrivateSeen(info.ID, info.UIDValidity, []uint32{uid}, true, modseq)
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) runPublic(t *testing.T, readers ...string) *Report {
	t.Helper()

	rep, err := Run(context.Background(), Options{Account: model.PublicNamespace, SourceUser: "alice", Password: "pw",
		IMAP:      Endpoint{Addr: e.addr, TLS: "none", Timeout: 5 * time.Second},
		Namespace: "Public/", TargetPrefix: "Archive/", Readers: readers, Prune: true, ACL: true, BatchMessages: 2}, &APITarget{C: e.client})
	if err != nil {
		t.Fatalf("run: %v (%+v)", err, rep)
	}

	return rep
}

// seenFor is what reader sees as \Seen in a public folder of the store:
// its own mark, or the shared flag without one.
func seenFor(t *testing.T, pub, reader model.User, name string) map[uint32]bool {
	t.Helper()

	info := mustInfo(t, pub, name)

	marks, err := reader.PrivateSeen(info.ID, info.UIDValidity)
	if err != nil {
		t.Fatal(err)
	}

	metas, _ := pub.ListMessages(info.ID)
	out := map[uint32]bool{}

	for _, m := range metas {
		seen := model.HasFlag(m.Flags, `\Seen`)
		if mark, ok := marks[m.UID]; ok {
			seen = mark.Seen
		}

		if seen {
			out[m.UID] = true
		}
	}

	return out
}

func sameSet(a map[uint32]bool, uids ...uint32) bool {
	if len(a) != len(uids) {
		return false
	}

	for _, uid := range uids {
		if !a[uid] {
			return false
		}
	}

	return true
}

// TestPublicNamespaceImport: a public namespace of the source lands in
// the public folders with its identities; the messages carry the source
// user's \Seen, a reader gets its own, and its subscription.
func TestPublicNamespaceImport(t *testing.T) {
	e := publicEnv(t)
	fillPublic(t, e)

	tpub := named(t, e.tgt, model.PublicNamespace)
	for _, name := range []string{"Other", "Archive", "Archive/Stale"} {
		if err := tpub.CreateMailbox(name, nil); err != nil {
			t.Fatal(err)
		}
	}

	rep := e.runPublic(t, "bob")
	if rep.Failed != 0 || !allVerified(rep) || len(rep.Mailboxes) != 2 || len(rep.Pruned) != 1 || rep.Pruned[0] != "Archive/Stale" {
		t.Fatalf("report: %+v", rep)
	}

	spub := named(t, e.src, model.PublicNamespace)
	samePublicFolders(t, spub, tpub)

	tbob := named(t, e.tgt, "bob")
	checkFirstPublicRun(t, rep, tpub, tbob, named(t, e.tgt, "carol"))

	// A second run changes nothing, for the folder or the reader.
	idleRun(t, e.runPublic(t, "bob"))

	// The delta: bob reads 1 and alice reads 2 at the source.
	markSeen(t, spub, named(t, e.src, "bob"), "Lists/A", 1)
	markSeen(t, spub, named(t, e.src, "alice"), "Lists/A", 2)

	e.runPublic(t, "bob")

	wantSeen(t, tpub, named(t, e.tgt, "carol"), "shared after the delta", 1, 2, 3)
	wantSeen(t, tpub, tbob, "bob after the delta", 1, 2, 3)
}

// checkFirstPublicRun: what lies outside the target prefix and the ACL
// are left alone; the messages carry what alice saw (her mark on 1, the
// shared flag on 3), bob has his own state and his subscription.
func checkFirstPublicRun(t *testing.T, rep *Report, tpub, tbob, anyone model.User) {
	t.Helper()

	checkLeftAlone(t, tpub)

	wantSeen(t, tpub, anyone, "shared", 1, 3)
	wantSeen(t, tpub, tbob, "bob", 2, 3)

	rd := rep.Readers[0]
	if rd.Account != "bob" || rd.Mailboxes != 1 || rd.Subscribed != 1 || rd.Changed != 2 || rd.Error != "" {
		t.Fatalf("reader %+v", rd)
	}

	// The folder bob cannot open is skipped with a warning, not a failure.
	if len(rep.Warnings) != 1 || !strings.Contains(rep.Warnings[0], "Archive/Lists/B: bob cannot open it") {
		t.Fatalf("warnings %v", rep.Warnings)
	}

	subs, _ := tbob.PublicSubscriptions()
	if !subs[mustInfo(t, tpub, "Archive/Lists/A").ID] || len(subs) != 1 {
		t.Fatalf("bob's subscriptions %v", subs)
	}
}

// checkLeftAlone: folders that are not below the target prefix survive
// the prune, and no ACL was copied.
func checkLeftAlone(t *testing.T, tpub model.User) {
	t.Helper()

	for _, name := range []string{"Other", "Archive"} {
		if _, err := tpub.Lookup(name); err != nil {
			t.Fatalf("pruned %s, which is not below the target prefix: %v", name, err)
		}
	}

	if acl, _ := tpub.GetACL("Archive/Lists/A"); len(acl) != 0 {
		t.Fatalf("acl copied: %v", acl)
	}
}

// samePublicFolders: both folders arrived below the target prefix with
// the source identities and without a subscription of their own.
func samePublicFolders(t *testing.T, spub, tpub model.User) {
	t.Helper()

	for _, name := range []string{"Lists/A", "Lists/B"} {
		sinfo, tinfo := mustInfo(t, spub, name), mustInfo(t, tpub, "Archive/"+name)
		if tinfo.UIDValidity != sinfo.UIDValidity || tinfo.UIDNext < sinfo.UIDNext || tinfo.Subscribed {
			t.Fatalf("%s: target %+v, source %+v", name, tinfo, sinfo)
		}

		smsgs, _ := spub.ListMessages(sinfo.ID)
		tmsgs, _ := tpub.ListMessages(tinfo.ID)

		if len(smsgs) != len(tmsgs) {
			t.Fatalf("%s: %d messages, source %d", name, len(tmsgs), len(smsgs))
		}

		for i := range smsgs {
			if smsgs[i].UID != tmsgs[i].UID || smsgs[i].Size != tmsgs[i].Size {
				t.Fatalf("%s: target %+v, source %+v", name, tmsgs[i], smsgs[i])
			}
		}
	}
}

func wantSeen(t *testing.T, pub, reader model.User, who string, uids ...uint32) {
	t.Helper()

	if got := seenFor(t, pub, reader, "Archive/Lists/A"); !sameSet(got, uids...) {
		t.Fatalf("%s sees %v, want %v", who, got, uids)
	}
}

// idleRun: a run over an unchanged source changes nothing.
func idleRun(t *testing.T, rep *Report) {
	t.Helper()

	if rep.Readers[0].Changed != 0 {
		t.Fatalf("idle run changed the reader: %+v", rep.Readers[0])
	}

	for _, m := range rep.Mailboxes {
		if m.Imported+m.FlagChanges+m.Expunged != 0 {
			t.Fatalf("idle run changed %+v", m)
		}
	}
}

// TestNamespaceOptions: what a namespace import needs and refuses.
func TestNamespaceOptions(t *testing.T) {
	base := Options{IMAP: Endpoint{Addr: "x:1", TLS: TLSNone}}

	for _, c := range []struct {
		name string
		set  func(*Options)
		ok   bool
	}{
		{"public needs a namespace", func(o *Options) { o.Account, o.SourceUser = model.PublicNamespace, "alice" }, false},
		{"public needs a source user", func(o *Options) { o.Account, o.Namespace = model.PublicNamespace, "Public/" }, false},
		{"public import", func(o *Options) { o.Account, o.SourceUser, o.Namespace = model.PublicNamespace, "alice", "Public/" }, true},
		{"readers need the public namespace", func(o *Options) { o.Account, o.Namespace, o.Readers = "alice", "Public/", []string{"bob"} }, false},
		{"a prefix needs a namespace", func(o *Options) { o.Account, o.TargetPrefix = "alice", "X/" }, false},
		{"a prefix ends in a slash", func(o *Options) { o.Account, o.Namespace, o.TargetPrefix = "alice", "Public/", "X" }, false},
		{"a namespace into an account", func(o *Options) { o.Account, o.Namespace, o.TargetPrefix = "alice", "Public/", "X/" }, true},
		{"a reader is an account", func(o *Options) {
			o.Account, o.SourceUser, o.Namespace, o.Readers = model.PublicNamespace, "alice", "Public/", []string{model.PublicNamespace}
		}, false},
	} {
		o := base
		o.ACL, o.Sieve = true, &Endpoint{Addr: "x:2", TLS: TLSNone}
		c.set(&o)

		err := o.defaults()
		if (err == nil) != c.ok {
			t.Fatalf("%s: %v", c.name, err)
		}

		if err == nil && (o.ACL || o.Sieve != nil) {
			t.Fatalf("%s: a namespace import copies neither ACL nor Sieve", c.name)
		}
	}
}

// TestNamespaceNames: names below the target prefix, the root mailbox of
// a namespace, another delimiter.
func TestNamespaceNames(t *testing.T) {
	for _, c := range []struct{ raw, prefix, root, delim, target, want string }{
		{"Mailinglists/Dovecot", "Mailinglists/", "Mailinglists", "/", "Lists/", "Lists/Dovecot"},
		{"Mailinglists/Dovecot", "Mailinglists/", "Mailinglists", "/", "", "Dovecot"},
		{"Zabbix", "Zabbix/", "Zabbix", "/", "Zabbix/", "Zabbix"},
		{"Zabbix", "Zabbix/", "Zabbix", "/", "", ""},
		{"Public.A.B", "Public.", "Public", ".", "P/", "P/A/B"},
		{"Public.A/B", "Public.", "Public", ".", "P/", ""},
		{"Public/&AMQ-rger", "Public/", "Public", "/", "", "Ärger"},
	} {
		got, err := mapNamespaceName(c.raw, c.prefix, c.root, c.delim, c.target)
		if got != c.want || (err == nil) != (c.want != "") {
			t.Fatalf("%q: %q, %v; want %q", c.raw, got, err, c.want)
		}
	}
}

// TestListNamespaceWithoutListExtended: a source without LIST-EXTENDED
// is listed with LIST and LSUB, and the subscriptions still arrive.
func TestListNamespaceWithoutListExtended(t *testing.T) {
	e := publicEnv(t)
	fillPublic(t, e)

	c, err := dialIMAP(context.Background(), Endpoint{Addr: e.addr, TLS: TLSNone, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.login("bob", "pw"); err != nil {
		t.Fatal(err)
	}

	delete(c.caps, "LIST-EXTENDED")

	boxes, err := c.listNamespace("Public/", "P/")
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}

	for _, mb := range boxes {
		if mb.Selectable {
			got[mb.Name] = mb.Subscribed
		}
	}

	if sub, ok := got["P/Lists/A"]; !ok || !sub || len(got) != 2 || got["P/Lists/B"] {
		t.Fatalf("listed %v", got)
	}

	if _, err := c.listNamespace("Nope/", ""); err == nil || !strings.Contains(err.Error(), `"Public/"`) {
		t.Fatalf("unknown namespace: %v", err)
	}
}
