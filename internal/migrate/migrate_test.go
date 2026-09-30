package migrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shardpost/internal/admin"
	"shardpost/internal/adminapi"
	"shardpost/internal/archive"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/imapproto"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

func msg(subject string) []byte {
	return []byte("From: a@example.org\r\nTo: alice@example.org\r\nSubject: " + subject + "\r\nMessage-ID: <" + subject + "@example.org>\r\n\r\nbody " + subject + "\r\n")
}

// env is a source IMAP server and a target admin API, both on memstores.
type env struct {
	src, tgt *memstore.Store
	addr     string
	client   *adminapi.ClientWithResponses
}

func newEnv(t *testing.T) *env {
	t.Helper()

	e := &env{src: memstore.New(), tgt: memstore.New()}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := imapproto.New(imapproto.Options{Store: e.src, Auth: auth.Static{"alice": "pw"}, InsecureAuth: true, Log: log})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	e.addr = ln.Addr().String()

	a := admin.New(admin.Options{Delivery: &delivery.Service{Store: e.tgt, Log: log}, Auth: auth.Static{"alice": "pw", "bob": "pw"}, Log: log,
		MaxMessageBytes: 1 << 20, SocketRoles: []string{admin.RoleImport}, Import: e.tgt, ImportMaxBatchMessages: 3})

	dir, err := os.MkdirTemp("", "spmig")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "admin.sock")

	aln, err := admin.ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}

	hs := a.UnixServer()

	go func() { _ = hs.Serve(aln) }()

	t.Cleanup(func() { _ = hs.Close() })

	if e.client, err = adminapi.NewSocketClient(path, 10*time.Second); err != nil {
		t.Fatal(err)
	}

	return e
}

func (e *env) run(t *testing.T) *Report {
	t.Helper()

	rep, err := Run(context.Background(), Options{Account: "alice", Password: "pw", IMAP: Endpoint{Addr: e.addr, TLS: "none", Timeout: 5 * time.Second},
		Prune: true, ACL: true, BatchMessages: 2, POP3UIDLFormat: "%08Xu%08Xv"}, &APITarget{C: e.client})
	if err != nil {
		t.Fatalf("run: %v (%+v)", err, rep)
	}

	return rep
}

func user(t *testing.T, s *memstore.Store) model.User {
	t.Helper()

	u, err := s.User("alice")
	if err != nil {
		t.Fatal(err)
	}

	return u
}

// sameMailboxes asserts that the target mirrors the source: names,
// UIDVALIDITY, UIDs, flags and modseqs.
func sameMailboxes(t *testing.T, src, tgt model.User) {
	t.Helper()

	sboxes, _ := src.ListMailboxes()

	for _, sb := range sboxes {
		sinfo, _ := src.Info(sb.Name)

		tinfo, err := tgt.Info(sb.Name)
		if err != nil {
			t.Fatalf("%s missing in the target: %v", sb.Name, err)
		}

		sameHeader(t, sinfo, tinfo)

		smsgs, _ := src.ListMessages(sinfo.ID)
		tmsgs, _ := tgt.ListMessages(tinfo.ID)

		sameMessages(t, sb.Name, smsgs, tmsgs)
	}
}

func sameHeader(t *testing.T, sinfo, tinfo *model.MailboxInfo) {
	t.Helper()

	if tinfo.UIDValidity != sinfo.UIDValidity || tinfo.UIDNext < sinfo.UIDNext || tinfo.HighestModSeq < sinfo.HighestModSeq {
		t.Fatalf("%s: target %d/%d/%d, source %d/%d/%d", sinfo.Name, tinfo.UIDValidity, tinfo.UIDNext, tinfo.HighestModSeq,
			sinfo.UIDValidity, sinfo.UIDNext, sinfo.HighestModSeq)
	}

	if tinfo.LogStart <= sinfo.HighestModSeq {
		t.Fatalf("%s: log start %d does not exclude source modseq %d", sinfo.Name, tinfo.LogStart, sinfo.HighestModSeq)
	}

	if !sameFlags(tinfo.SpecialUse, sinfo.SpecialUse) || tinfo.Subscribed != sinfo.Subscribed {
		t.Fatalf("%s: special-use/subscription %v/%v, source %v/%v", sinfo.Name, tinfo.SpecialUse, tinfo.Subscribed, sinfo.SpecialUse, sinfo.Subscribed)
	}
}

func sameMessages(t *testing.T, name string, smsgs, tmsgs []model.Meta) {
	t.Helper()

	if len(smsgs) != len(tmsgs) {
		t.Fatalf("%s: %d messages, source %d", name, len(tmsgs), len(smsgs))
	}

	for i := range smsgs {
		s, d := smsgs[i], tmsgs[i]
		if s.UID != d.UID || s.ModSeq != d.ModSeq || !sameFlags(s.Flags, d.Flags) || s.Size != d.Size || !s.InternalDate.Truncate(time.Second).Equal(d.InternalDate) {
			t.Fatalf("%s uid %d: target %+v, source %+v", name, s.UID, d, s)
		}
	}
}

// fillSource creates mailboxes (hierarchy, non-ASCII, special-use) and
// messages with UID gaps, flags and keywords.
func fillSource(t *testing.T, src model.User) {
	t.Helper()

	for _, name := range []string{"INBOX", "Archiv/2025", "Ärger"} {
		if err := src.CreateMailbox(name, nil); err != nil {
			t.Fatal(err)
		}
	}

	if err := src.CreateMailbox("Sent", []string{`\Sent`}); err != nil {
		t.Fatal(err)
	}

	_ = src.SetSubscribed("Sent", true)

	for i, s := range []string{"a", "b", "c", "d", "e"} {
		m, err := src.Deliver("INBOX", msg(s), nil, time.Date(2025, 1, i+1, 10, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}

		if s == "b" || s == "d" { // gaps in the UIDs
			_ = src.Expunge("INBOX", []model.Meta{*m})
		}
	}

	if _, err := src.SetFlags("INBOX", []uint32{1, 3}, 0, func([]string) []string { return []string{`\Seen`, "$Label1"} }); err != nil {
		t.Fatal(err)
	}

	for _, s := range []string{"x", "y"} {
		if _, err := src.Deliver("Ärger", msg(s), []string{`\Flagged`}, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
}

// changeSource adds a message, changes flags and expunges one.
func changeSource(t *testing.T, src model.User) {
	t.Helper()

	if _, err := src.Deliver("INBOX", msg("f"), nil, time.Time{}); err != nil {
		t.Fatal(err)
	}

	if _, err := src.SetFlags("INBOX", []uint32{5}, 0, func([]string) []string { return []string{`\Answered`} }); err != nil {
		t.Fatal(err)
	}

	inbox, _ := src.Lookup("INBOX")
	metas, _ := src.GetMessages(inbox.ID, []uint32{1})
	_ = src.Expunge("INBOX", metas)
}

func mailboxReport(rep *Report, name string) MailboxReport {
	for _, m := range rep.Mailboxes {
		if m.Name == name {
			return m
		}
	}

	return MailboxReport{}
}

func TestMigrateKeepsIdentities(t *testing.T) {
	e := newEnv(t)
	src := user(t, e.src)
	fillSource(t, src)

	// A target INBOX from a first login: empty, other UIDVALIDITY.
	tgt := user(t, e.tgt)
	_ = tgt.CreateMailbox("INBOX", nil)
	_ = tgt.CreateMailbox("Stale", nil)

	rep := e.run(t)
	if rep.Failed != 0 || len(rep.Pruned) != 1 || rep.Pruned[0] != "Stale" || !allVerified(rep) {
		t.Fatalf("report: %+v", rep)
	}

	sameMailboxes(t, src, tgt)
	checkUIDLAndLock(t, src, tgt)

	changeSource(t, src)

	rep = e.run(t)
	if in := mailboxReport(rep, "INBOX"); in.Imported != 1 || in.FlagChanges != 1 || in.Expunged != 1 || !in.Verified {
		t.Fatalf("delta: %+v", in)
	}

	sameMailboxes(t, src, tgt)

	// A third run finds nothing to do.
	rep = e.run(t)
	for _, m := range rep.Mailboxes {
		if m.Imported+m.FlagChanges+m.Expunged != 0 {
			t.Fatalf("idle run changed %+v", m)
		}
	}
}

// checkUIDLAndLock checks the Dovecot-style POP3 UIDL of the first
// INBOX message and that the import lock is gone.
func checkUIDLAndLock(t *testing.T, src, tgt model.User) {
	t.Helper()

	inbox := mustInfo(t, src, "INBOX")
	tmsgs, _ := tgt.ListMessages(mustInfo(t, tgt, "INBOX").ID)

	if tmsgs[0].POP3UIDL != "00000001"+hex8u(inbox.UIDValidity) {
		t.Fatalf("pop3 uidl %q", tmsgs[0].POP3UIDL)
	}

	if locked, _ := tgt.(model.Importer).ImportLocked(); locked {
		t.Fatal("import lock not released")
	}
}

// TestClientsResumeIncrementally checks the point of the exercise: a
// client that was in sync with the source resumes on the target without
// any change; one that was behind gets exactly the changes.
func TestClientsResumeIncrementally(t *testing.T) {
	e := newEnv(t)
	src := user(t, e.src)
	fillSource(t, src)
	e.run(t)
	changeSource(t, src)
	e.run(t)

	sinfo := mustInfo(t, src, "INBOX")

	for _, l := range qresync(t, e.tgt, sinfo.UIDValidity, sinfo.HighestModSeq) {
		if strings.Contains(l, "FETCH") || strings.Contains(l, "VANISHED") {
			t.Fatalf("up-to-date client got %q", l)
		}
	}

	behind := strings.Join(qresync(t, e.tgt, sinfo.UIDValidity, sinfo.HighestModSeq-3), "\n")
	if !strings.Contains(behind, "VANISHED (EARLIER)") || !strings.Contains(behind, "UID 6 ") || !strings.Contains(behind, "UID 5 ") {
		t.Fatalf("client behind got %s", behind)
	}
}

func allVerified(rep *Report) bool {
	for _, m := range rep.Mailboxes {
		if !m.Verified {
			return false
		}
	}

	return true
}

// qresync selects INBOX on a target IMAP server with a QRESYNC state and
// returns the untagged responses.
func qresync(t *testing.T, store *memstore.Store, uidValidity uint32, modseq uint64) []string {
	t.Helper()

	srv := imapproto.New(imapproto.Options{Store: store, Auth: auth.Static{"alice": "pw"}, InsecureAuth: true,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go func() { _ = srv.Serve(ln) }()

	defer func() { _ = srv.Close() }()

	c, err := dialIMAP(context.Background(), Endpoint{Addr: ln.Addr().String(), TLS: "none", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.login("alice", "pw"); err != nil {
		t.Fatal(err)
	}

	if _, err := c.cmd("ENABLE QRESYNC"); err != nil {
		t.Fatal(err)
	}

	rs, err := c.cmd(fmt.Sprintf("SELECT INBOX (QRESYNC (%d %d))", uidValidity, modseq))
	if err != nil {
		t.Fatal(err)
	}

	var out []string
	for _, r := range rs {
		out = append(out, r.text)
	}

	return out
}

func mustInfo(t *testing.T, u model.User, name string) *model.MailboxInfo {
	t.Helper()

	info, err := u.Info(name)
	if err != nil {
		t.Fatal(err)
	}

	return info
}

func hex8u(n uint32) string {
	const digits = "0123456789ABCDEF"

	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = digits[n&0xf]
		n >>= 4
	}

	return string(out)
}

func TestImportLockRefusesProtocols(t *testing.T) {
	e := newEnv(t)
	tgt := user(t, e.tgt)

	if _, err := tgt.(model.Importer).ImportLock("0123456789abcdef", time.Minute); err != nil {
		t.Fatal(err)
	}

	if err := model.RefuseDuringImport(tgt); err != model.ErrImportLocked {
		t.Fatalf("refuse: %v", err)
	}

	// Another session cannot take the account.
	_, err := Run(context.Background(), Options{Account: "alice", Password: "pw", IMAP: Endpoint{Addr: e.addr, TLS: "none", Timeout: time.Second}},
		&APITarget{C: e.client})

	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != string(adminapi.ImportLocked) {
		t.Fatalf("second session: %v", err)
	}
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}

		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}

		err = u.Unwrap()
	}

	return false
}

func TestUIDLFormats(t *testing.T) {
	for _, tc := range []struct{ format, want string }{
		{"%08Xu%08Xv", "0000002A000000FF"},
		{"%u.%v", "42.255"},
		{"%{uid | hex(8) | upper}%{uidvalidity | hex(8) | upper}", "0000002A000000FF"},
		{"%{uid | hex(8)}%{uidvalidity | hex(8)}", "0000002a000000ff"},
		{"%g", "abc"},
		{"UID%u-%%", "UID42-%"},
	} {
		f, err := parseUIDLFormat(tc.format)
		if err != nil {
			t.Fatalf("%s: %v", tc.format, err)
		}

		if got := f.format(42, 255, "abc"); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.format, got, tc.want)
		}
	}

	if _, err := parseUIDLFormat("%m"); err == nil {
		t.Fatal("%m accepted")
	}
}

func TestMUTF7(t *testing.T) {
	for in, want := range map[string]string{"&AMQ-rger": "Ärger", "a&-b": "a&b", "Entw&APw-rfe": "Entwürfe", "plain": "plain"} {
		if got, err := decodeMUTF7(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
}

func TestReimportAfterUIDValidityChange(t *testing.T) {
	e := newEnv(t)
	src := user(t, e.src)
	fillSource(t, src)
	e.run(t)

	// The source recreates a mailbox: new UIDVALIDITY, other messages.
	_ = src.DeleteMailbox("Ärger")
	_ = src.CreateMailbox("Ärger", nil)
	_, _ = src.Deliver("Ärger", msg("z"), nil, time.Time{})

	opts := Options{Account: "alice", Password: "pw", IMAP: Endpoint{Addr: e.addr, TLS: TLSNone, Timeout: 5 * time.Second}}

	rep, err := Run(context.Background(), opts, &APITarget{C: e.client})
	if !errors.Is(err, ErrPartial) || mailboxReport(rep, "Ärger").Error == "" {
		t.Fatalf("without reimport: %v %+v", err, rep)
	}

	opts.Reimport = true

	rep, err = Run(context.Background(), opts, &APITarget{C: e.client})
	if err != nil || !mailboxReport(rep, "Ärger").Verified {
		t.Fatalf("reimport: %v %+v", err, rep)
	}

	sameMailboxes(t, src, user(t, e.tgt))
}

func TestNamespaces(t *testing.T) {
	r := &response{text: `* NAMESPACE (("INBOX." ".")) (("Other Users." ".")) (("Shared." "."))`}
	ns := parseNamespaces([]*response{r})

	if ns.prefix != "INBOX." || ns.delim != "." || len(ns.others) != 2 {
		t.Fatalf("%+v", ns)
	}

	flat := namespaces{others: []string{"Shared/"}}
	if !flat.foreign("Shared/team") || flat.foreign("Sent") {
		t.Fatal("foreign")
	}

	for raw, want := range map[string]string{"INBOX": "INBOX", "INBOX.Sent": "Sent", "INBOX.a.b": "a/b"} {
		if got, err := mapName(raw, "INBOX.", "."); err != nil || got != want {
			t.Errorf("%s: %q %v", raw, got, err)
		}
	}

	if _, err := mapName("INBOX.a/b", "INBOX.", "."); err == nil {
		t.Fatal(`"/" in a name accepted`)
	}
}

func TestDiffConflictsAndReimport(t *testing.T) {
	src := []srcMessage{{UID: 1, Size: 10}, {UID: 2, Size: 20, Flags: []string{`\Seen`}}, {UID: 4, Size: 40}}
	have := map[uint32]adminapi.ImportInventoryEntry{
		1: {Uid: 1, Size: 10, Flags: []string{}},
		2: {Uid: 2, Size: 21, Flags: []string{}},
		3: {Uid: 3, Size: 30, Flags: []string{}},
	}

	p := diff(src, have)
	if len(p.conflicts) != 1 || p.conflicts[0].UID != 2 || len(p.missing) != 1 || len(p.expunge) != 1 || len(p.flags) != 0 {
		t.Fatalf("diff: %+v", p)
	}

	p.reimport()

	if len(p.conflicts) != 0 || len(p.expunge) != 2 || p.expunge[0] != 2 || len(p.missing) != 2 || p.missing[0].UID != 2 {
		t.Fatalf("reimport: %+v", p)
	}
}

// TestRestoreSkipsTheJMAPVacationScript: an archive that holds the JMAP
// vacation script restores the user's scripts without it; the target
// would refuse it.
func TestRestoreSkipsTheJMAPVacationScript(t *testing.T) {
	x := &restore{rep: &RestoreReport{}}

	for _, name := range []string{"main", model.JMAPVacationScript} {
		if err := x.entry(context.Background(), &archive.Entry{Kind: archive.KindSieve, Name: name, Raw: []byte("keep;")}); err != nil {
			t.Fatal(err)
		}
	}

	if len(x.scripts) != 1 || x.scripts[0].Name != "main" {
		t.Errorf("scripts %+v", x.scripts)
	}
}

// sieveTarget records the scripts and the active name a restore stores.
type sieveTarget struct {
	Target
	active string
	calls  int
}

func (s *sieveTarget) PutSieve(_ context.Context, _ []adminapi.ImportSieveScript, active string) error {
	s.active, s.calls = active, s.calls+1

	return nil
}

// TestRestoreNeverActivatesTheJMAPVacationScript: an archive from before
// the reserved name may name the JMAP vacation copy as active; the copy is
// not restored, so nothing is activated rather than the restore failing.
func TestRestoreNeverActivatesTheJMAPVacationScript(t *testing.T) {
	tgt := &sieveTarget{}
	x := &restore{tgt: tgt, o: RestoreOptions{ActivateSieve: true}, rep: &RestoreReport{},
		scripts: []adminapi.ImportSieveScript{{Name: "main", Script: "keep;"}}}

	if err := x.sieve(context.Background(), model.JMAPVacationScript); err != nil {
		t.Fatal(err)
	}

	if tgt.calls != 1 || tgt.active != "" || x.rep.SieveActivated {
		t.Fatalf("active %q, activated %v", tgt.active, x.rep.SieveActivated)
	}
}
