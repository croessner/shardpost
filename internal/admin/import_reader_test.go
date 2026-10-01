package admin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// serveOptions is an admin socket over a memstore with the given
// delivery service settings.
func serveOptions(t *testing.T, users auth.Static, tune func(*delivery.Service), roles ...string) (*memstore.Store, *adminapi.ClientWithResponses) {
	t.Helper()

	store := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	svc := &delivery.Service{Store: store, Log: log, PublicPrefix: "Public/"}
	if tune != nil {
		tune(svc)
	}

	s := New(Options{Delivery: svc, Auth: users, Log: log, Version: "test", MaxMessageBytes: 4096, SocketRoles: roles,
		Accounts: store, Import: store, ImportMaxBatchMessages: 10, ImportMaxBatchBytes: 1 << 20, SharedPrefix: "Shared/", PublicPrefix: "Public/"})

	path := filepath.Join(socketDir(t), "admin.sock")

	ln, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}

	srv := s.UnixServer()

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	client, err := adminapi.NewSocketClient(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	return store, client
}

// publicFixture is an import session on the public namespace with one
// folder of three messages; the third carries the shared \Seen.
func publicFixture(t *testing.T) (*memstore.Store, *adminapi.ClientWithResponses, string, string) {
	t.Helper()

	store, c := serveOptions(t, auth.Static{"user": "pw", "other": "pw"}, nil, RoleImport)
	ctx := context.Background()
	session := openSession(t, c, model.PublicNamespace)

	mb, err := c.PutImportMailboxWithResponse(ctx, model.PublicNamespace, &adminapi.PutImportMailboxParams{XImportSession: session},
		adminapi.ImportMailboxSpec{Name: "Lists/A", Uidvalidity: 77})
	if err != nil || mb.JSON200 == nil {
		t.Fatalf("mailbox: %v %d %v", err, mb.StatusCode(), mb.JSONDefault)
	}

	var body bytes.Buffer

	ct, err := adminapi.WriteImportBody(&body, []adminapi.ImportPart{part(1, 5), part(2, 6), part(3, 7, `\Seen`)})
	if err != nil {
		t.Fatal(err)
	}

	res, err := c.ImportMessagesWithBodyWithResponse(ctx, model.PublicNamespace, mb.JSON200.Id, &adminapi.ImportMessagesParams{XImportSession: session}, ct, &body)
	if err != nil || res.JSON200 == nil || res.JSON200.Imported != 3 {
		t.Fatalf("import: %v %d %v", err, res.StatusCode(), res.JSONDefault)
	}

	return store, c, session, mb.JSON200.Id
}

func putReader(t *testing.T, c *adminapi.ClientWithResponses, account, session, id, reader string, req adminapi.ImportReader) *adminapi.PutImportReaderResponse {
	t.Helper()

	resp, err := c.PutImportReaderWithResponse(context.Background(), account, id, reader, &adminapi.PutImportReaderParams{XImportSession: session}, req)
	if err != nil {
		t.Fatal(err)
	}

	return resp
}

// TestImportReaderSeen: the reader's \Seen becomes exactly the given
// set, written as private marks where it differs from what the reader
// saw, under a new MODSEQ of the mailbox; nothing changes for anybody
// else, and a repeat changes nothing.
func TestImportReaderSeen(t *testing.T) {
	store, c, session, id := publicFixture(t)

	pub, _ := store.User(model.PublicNamespace)
	before, _ := pub.Info("Lists/A")

	sub := true

	resp := putReader(t, c, model.PublicNamespace, session, id, "user", adminapi.ImportReader{Uidvalidity: 77, Seen: []int64{1, 2, 99}, Subscribed: &sub})
	if resp.JSON200 == nil || resp.JSON200.Changed != 3 {
		t.Fatalf("reader: %d %v %v", resp.StatusCode(), resp.JSON200, resp.JSONDefault)
	}

	user, _ := store.User("user")
	after, _ := pub.Info("Lists/A")

	checkReaderMarks(t, user, id, before.HighestModSeq, after.HighestModSeq)
	checkOthersUntouched(t, store, pub, id)

	if subs, _ := user.PublicSubscriptions(); !subs[id] {
		t.Fatalf("not subscribed: %v", subs)
	}

	resp = putReader(t, c, model.PublicNamespace, session, id, "user", adminapi.ImportReader{Uidvalidity: 77, Seen: []int64{1, 2}})
	if resp.JSON200 == nil || resp.JSON200.Changed != 0 {
		t.Fatalf("repeat: %d %v", resp.StatusCode(), resp.JSON200)
	}

	if subs, _ := user.PublicSubscriptions(); !subs[id] {
		t.Fatal("a request without subscribed changed the subscription")
	}
}

// checkReaderMarks: 1 and 2 seen, 3 (shared \Seen) cleared for the
// reader, each under a modseq the request allocated.
func checkReaderMarks(t *testing.T, user model.User, id string, before, after uint64) {
	t.Helper()

	marks, err := user.PrivateSeen(id, 77)
	if err != nil || len(marks) != 3 || !marks[1].Seen || !marks[2].Seen || marks[3].Seen {
		t.Fatalf("marks: %v %v", marks, err)
	}

	for uid, m := range marks {
		if m.ModSeq <= before || m.ModSeq > after {
			t.Fatalf("uid %d: mark modseq %d outside (%d, %d]", uid, m.ModSeq, before, after)
		}
	}
}

// checkOthersUntouched: the shared flags and another account's view
// stay as they were.
func checkOthersUntouched(t *testing.T, store *memstore.Store, pub model.User, id string) {
	t.Helper()

	metas, _ := pub.ListMessages(id)
	if model.HasFlag(metas[0].Flags, `\Seen`) || !model.HasFlag(metas[2].Flags, `\Seen`) {
		t.Fatalf("shared flags changed: %+v", metas)
	}

	other, _ := store.User("other")
	if marks, _ := other.PrivateSeen(id, 77); len(marks) != 0 {
		t.Fatalf("another account got marks: %v", marks)
	}
}

// TestImportReaderRefusals: only the public namespace has readers, the
// reader is a canonical account, the UIDs belong to the UIDVALIDITY.
func TestImportReaderRefusals(t *testing.T) {
	_, c, session, id := publicFixture(t)
	ok := adminapi.ImportReader{Uidvalidity: 77, Seen: []int64{1}}

	for _, tc := range []struct {
		name, reader string
		req          adminapi.ImportReader
		status       int
	}{
		{"unknown reader", "nobody", ok, http.StatusNotFound},
		{"alias", "User@Example.org", ok, http.StatusBadRequest},
		{"reserved namespace", model.PublicNamespace, ok, http.StatusBadRequest},
		{"other uidvalidity", "user", adminapi.ImportReader{Uidvalidity: 78, Seen: []int64{1}}, http.StatusConflict},
		{"bad uid", "user", adminapi.ImportReader{Uidvalidity: 77, Seen: []int64{0}}, http.StatusBadRequest},
		{"bad uidvalidity", "user", adminapi.ImportReader{Seen: []int64{1}}, http.StatusBadRequest},
	} {
		if resp := putReader(t, c, model.PublicNamespace, session, id, tc.reader, tc.req); resp.StatusCode() != tc.status {
			t.Fatalf("%s: %d %v", tc.name, resp.StatusCode(), resp.JSONDefault)
		}
	}

	if resp := putReader(t, c, model.PublicNamespace, session, "00000000000000000000000000000000", "user", ok); resp.StatusCode() != http.StatusNotFound {
		t.Fatalf("unknown mailbox: %d", resp.StatusCode())
	}

	if resp := putReader(t, c, model.PublicNamespace, "ffffffffffffffffffffffff", id, "user", ok); resp.StatusCode() != http.StatusConflict {
		t.Fatalf("foreign session: %d", resp.StatusCode())
	}

	// An account has no readers.
	own := openSession(t, c, "user")

	mb, _ := c.PutImportMailboxWithResponse(context.Background(), "user", &adminapi.PutImportMailboxParams{XImportSession: own},
		adminapi.ImportMailboxSpec{Name: "INBOX", Uidvalidity: 77})
	if resp := putReader(t, c, "user", own, mb.JSON200.Id, "other", ok); resp.StatusCode() != http.StatusBadRequest {
		t.Fatalf("reader of an account: %d", resp.StatusCode())
	}
}

// TestSieveCheckRunsGlobalScripts: the check shows what a delivery does,
// global scripts included — also for an account without a script of its
// own — and names a failed global script.
func TestSieveCheckRunsGlobalScripts(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"10-lists.sieve":  `require "fileinto"; if exists "List-Id" { fileinto "Lists"; stop; }`,
		"20-broken.sieve": `require "nope";`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	_, c := serveOptions(t, auth.Static{"user": "pw"}, func(s *delivery.Service) { s.ScriptsAfter = dir }, RoleDiagnose, RoleContent)
	list, plain := "List-Id: <x.example.org>\r\nSubject: x\r\n\r\nx\r\n", "Subject: x\r\n\r\nx\r\n"
	keep, mine := `keep;`, `require "fileinto"; fileinto "Mine";`

	for _, tc := range []struct {
		name            string
		req             adminapi.SieveCheck
		keep            bool
		mailbox         string
		scripts, failed int
	}{
		// The after script files list mail; the script behind it never runs.
		{"no script of its own", adminapi.SieveCheck{Raw: &list}, false, "Lists", 2, 0},
		// Other mail is kept once; the broken script is reported, not fatal.
		{"kept once", adminapi.SieveCheck{Script: &keep, Raw: &plain}, true, "INBOX", 2, 1},
		{"a personal fileinto ends the sequence", adminapi.SieveCheck{Script: &mine, Raw: &list}, false, "Mine", 1, 0},
	} {
		a := checkActions(t, c, tc.req)
		if a.Keep != tc.keep || len(a.Fileinto) != 1 || a.Fileinto[0].Mailbox != tc.mailbox ||
			len(derefSlice(a.Scripts)) != tc.scripts || len(derefSlice(a.FailedScripts)) != tc.failed {
			t.Fatalf("%s: %+v (scripts %v, failed %v)", tc.name, a, derefSlice(a.Scripts), derefSlice(a.FailedScripts))
		}
	}
}

// TestSieveCheckSeesDeliveryHeaders: the dry run gives the script the
// Return-Path and Delivered-To a delivery adds, and reports the
// effective vacation period in days and seconds.
func TestSieveCheckSeesDeliveryHeaders(t *testing.T) {
	_, c := serveOptions(t, auth.Static{"user": "pw"}, func(s *delivery.Service) {
		s.ReturnPath, s.DeliveredTo = true, delivery.DeliveredToFinal
		s.SieveConfig = map[string]string{"sieve_vacation_min_period": "0"}
	}, RoleDiagnose)

	script := `require ["fileinto", "vacation-seconds"];
if allof (header :is "Return-Path" "<s@example.org>", header :is "Delivered-To" "user@example.org") { fileinto "Seen"; }
vacation :seconds 3600 "away";`
	raw := "Return-Path: <forged@example.net>\r\nFrom: s@example.org\r\nTo: user@example.org\r\nSubject: x\r\n\r\nx\r\n"
	from, to := "s@example.org", "user@example.org"

	a := checkActions(t, c, adminapi.SieveCheck{Script: &script, Raw: &raw, EnvelopeFrom: &from, EnvelopeTo: &to})
	if len(a.Fileinto) != 1 || a.Fileinto[0].Mailbox != "Seen" {
		t.Fatalf("the script did not see the delivery headers: %+v", a.Fileinto)
	}

	if a.Vacation == nil || a.Vacation.Seconds != 3600 || a.Vacation.Days != 0 {
		t.Fatalf("vacation %+v", a.Vacation)
	}

	if a.HeaderEdited == nil || *a.HeaderEdited {
		t.Fatal("the added delivery headers count as a header edit")
	}
}

func checkActions(t *testing.T, c *adminapi.ClientWithResponses, req adminapi.SieveCheck) *adminapi.SieveActions {
	t.Helper()

	res, err := c.CheckSieveWithResponse(context.Background(), "user", req)
	if err != nil || res.JSON200 == nil || !res.JSON200.Valid || res.JSON200.Actions == nil {
		t.Fatalf("check: %v %d %+v %v", err, res.StatusCode(), res.JSON200, res.JSONDefault)
	}

	return res.JSON200.Actions
}
