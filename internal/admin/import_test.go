package admin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// serveImport is an admin socket with the given roles and imports into a
// memstore enabled.
func serveImport(t *testing.T, roles ...string) (*memstore.Store, *adminapi.ClientWithResponses) {
	t.Helper()

	store := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Options{Delivery: &delivery.Service{Store: store, Log: log}, Auth: auth.Static{"user": "pw"}, Log: log, Version: "test",
		MaxMessageBytes: 4096, SocketRoles: roles, Import: store, ImportMaxBatchMessages: 2, ImportMaxBatchBytes: 1 << 20})

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

func openSession(t *testing.T, c *adminapi.ClientWithResponses, account string) string {
	t.Helper()

	resp, err := c.OpenImportWithResponse(context.Background(), account, adminapi.ImportSessionRequest{})
	if err != nil || resp.JSON200 == nil {
		t.Fatalf("open: %v %d %v", err, resp.StatusCode(), resp.JSONDefault)
	}

	return resp.JSON200.Session
}

func TestImportNeedsRoleAndCanonicalAccount(t *testing.T) {
	_, c := serveImport(t, RoleDeliver)

	resp, err := c.OpenImportWithResponse(context.Background(), "user", adminapi.ImportSessionRequest{})
	if err != nil || resp.StatusCode() != http.StatusForbidden {
		t.Fatalf("without role: %v %d", err, resp.StatusCode())
	}

	_, c = serveImport(t, RoleImport)

	resp, _ = c.OpenImportWithResponse(context.Background(), "User@Example.org", adminapi.ImportSessionRequest{})
	if resp.StatusCode() != http.StatusBadRequest || !strings.Contains(resp.JSONDefault.Message, "use user") {
		t.Fatalf("alias: %d %v", resp.StatusCode(), resp.JSONDefault)
	}

	resp, _ = c.OpenImportWithResponse(context.Background(), "nobody", adminapi.ImportSessionRequest{})
	if resp.StatusCode() != http.StatusNotFound {
		t.Fatalf("unknown account: %d", resp.StatusCode())
	}
}

func TestImportSessions(t *testing.T) {
	store, c := serveImport(t, RoleImport)
	ctx := context.Background()

	session := openSession(t, c, "user")

	// A second session cannot take the account, a foreign one cannot use it.
	resp, _ := c.OpenImportWithResponse(ctx, "user", adminapi.ImportSessionRequest{})
	if resp.StatusCode() != http.StatusConflict || resp.JSONDefault.Code != adminapi.ImportLocked {
		t.Fatalf("second session: %d %v", resp.StatusCode(), resp.JSONDefault)
	}

	list, _ := c.ListImportMailboxesWithResponse(ctx, "user", &adminapi.ListImportMailboxesParams{XImportSession: "ffffffffffffffffffffffff"})
	if list.StatusCode() != http.StatusConflict || list.JSONDefault.Code != adminapi.NotLocked {
		t.Fatalf("foreign session: %d %v", list.StatusCode(), list.JSONDefault)
	}

	u, _ := store.User("user")
	if err := model.RefuseDuringImport(u); err != model.ErrImportLocked {
		t.Fatalf("protocols not refused: %v", err)
	}

	cl, _ := c.CloseImportWithResponse(ctx, "user", &adminapi.CloseImportParams{XImportSession: session})
	if cl.StatusCode() != http.StatusNoContent || model.RefuseDuringImport(u) != nil {
		t.Fatalf("close: %d", cl.StatusCode())
	}
}

// importFixture is an open import session with one mailbox.
type importFixture struct {
	t       *testing.T
	store   *memstore.Store
	c       *adminapi.ClientWithResponses
	session string
	id      string
}

func newImportFixture(t *testing.T) *importFixture {
	t.Helper()

	store, c := serveImport(t, RoleImport)
	f := &importFixture{t: t, store: store, c: c, session: openSession(t, c, "user")}

	sub := true

	mb, err := c.PutImportMailboxWithResponse(context.Background(), "user", f.mailboxParams(), adminapi.ImportMailboxSpec{Name: "INBOX", Uidvalidity: 1234, Subscribed: &sub})
	if err != nil || mb.JSON200 == nil || mb.JSON200.Uidvalidity != 1234 {
		t.Fatalf("mailbox: %v %d %v", err, mb.StatusCode(), mb.JSONDefault)
	}

	f.id = mb.JSON200.Id

	return f
}

func (f *importFixture) mailboxParams() *adminapi.PutImportMailboxParams {
	return &adminapi.PutImportMailboxParams{XImportSession: f.session}
}

var importDate = time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)

func part(uid uint32, modseq uint64, flags ...string) adminapi.ImportPart {
	return adminapi.ImportPart{Raw: []byte(message), Meta: adminapi.ImportMessageMeta{UID: uid, ModSeq: modseq, Flags: flags, InternalDate: importDate}}
}

func (f *importFixture) post(parts ...adminapi.ImportPart) *adminapi.ImportMessagesResponse {
	f.t.Helper()

	var body bytes.Buffer

	ct, err := adminapi.WriteImportBody(&body, parts)
	if err != nil {
		f.t.Fatal(err)
	}

	resp, err := f.c.ImportMessagesWithBodyWithResponse(context.Background(), "user", f.id, &adminapi.ImportMessagesParams{XImportSession: f.session}, ct, &body)
	if err != nil {
		f.t.Fatal(err)
	}

	return resp
}

func TestImportMessagesKeepIdentities(t *testing.T) {
	f := newImportFixture(t)

	if r := f.post(part(7, 40, `\Seen`), part(90, 41, "$Work")); r.JSON200 == nil || r.JSON200.Imported != 2 {
		t.Fatalf("import: %d %v", r.StatusCode(), r.JSONDefault)
	}

	// Resuming is a no-op.
	if r := f.post(part(7, 40, `\Seen`)); r.JSON200 == nil || r.JSON200.Existing != 1 {
		t.Fatalf("resume: %d %v", r.StatusCode(), r.JSONDefault)
	}

	f.finalizeAndCheck()
}

// finalizeAndCheck finalizes after the import of UIDs 7 and 90 and checks
// the stored state and the inventory paging.
func (f *importFixture) finalizeAndCheck() {
	t := f.t
	t.Helper()

	fin, _ := f.c.FinalizeImportMailboxWithResponse(context.Background(), "user", f.id, &adminapi.FinalizeImportMailboxParams{XImportSession: f.session},
		adminapi.ImportFinalize{UidNext: 120, HighestModseq: 60})
	if fin.JSON200 == nil || fin.JSON200.UidNext != 120 || fin.JSON200.HighestModseq != 60 {
		t.Fatalf("finalize: %d %v", fin.StatusCode(), fin.JSONDefault)
	}

	f.checkStored()
	f.checkInventory()
}

// checkStored checks entries and header after finalizeAndCheck.
func (f *importFixture) checkStored() {
	t := f.t
	t.Helper()

	u, _ := f.store.User("user")
	info, _ := u.Info("INBOX")
	msgs, _ := u.ListMessages(info.ID)

	if len(msgs) != 2 || msgs[0].UID != 7 || msgs[1].UID != 90 || msgs[1].ModSeq != 41 || !msgs[0].InternalDate.Equal(importDate) {
		t.Fatalf("messages: %+v", msgs)
	}

	if info.RecentFrom != 120 || info.LogStart != 61 || !model.HasFlag(info.Keywords, "$Work") {
		t.Fatalf("header: %+v", info)
	}
}

// checkInventory checks the paging of the inventory.
func (f *importFixture) checkInventory() {
	t := f.t
	t.Helper()

	inv, _ := f.c.ListImportMessagesWithResponse(context.Background(), "user", f.id, &adminapi.ListImportMessagesParams{XImportSession: f.session, Limit: new(1)})
	if inv.JSON200 == nil || len(inv.JSON200.Messages) != 1 || inv.JSON200.NextAfter == nil || *inv.JSON200.NextAfter != 7 {
		t.Fatalf("inventory: %d %+v", inv.StatusCode(), inv.JSON200)
	}
}

func TestImportRefusesBadBatches(t *testing.T) {
	f := newImportFixture(t)

	for name, r := range map[string]*adminapi.ImportMessagesResponse{
		"descending": f.post(part(95, 1), part(94, 1)),
		"flag":       f.post(part(96, 1, `\Recent`)),
		"batch":      f.post(part(97, 1), part(98, 1), part(99, 1)),
	} {
		if r.StatusCode() != http.StatusBadRequest {
			t.Fatalf("%s: %d", name, r.StatusCode())
		}
	}
}

func TestImportConflicts(t *testing.T) {
	f := newImportFixture(t)

	if r := f.post(part(90, 41)); r.JSON200 == nil {
		t.Fatalf("import: %d", r.StatusCode())
	}

	// A non-empty mailbox with another UIDVALIDITY is a conflict.
	mb, _ := f.c.PutImportMailboxWithResponse(context.Background(), "user", f.mailboxParams(), adminapi.ImportMailboxSpec{Name: "INBOX", Uidvalidity: 999})
	if mb.StatusCode() != http.StatusConflict || mb.JSONDefault.Code != adminapi.Conflict {
		t.Fatalf("uidvalidity conflict: %d %v", mb.StatusCode(), mb.JSONDefault)
	}

	// A different message at an existing UID is a conflict.
	other := part(90, 41)
	other.Raw = append([]byte("X-Other: 1\r\n"), other.Raw...)

	if r := f.post(other); r.StatusCode() != http.StatusConflict {
		t.Fatalf("uid conflict: %d", r.StatusCode())
	}
}

func TestImportSieveValidates(t *testing.T) {
	store, c := serveImport(t, RoleImport)
	ctx := context.Background()
	session := openSession(t, c, "user")
	p := &adminapi.PutImportSieveParams{XImportSession: session}

	bad, _ := c.PutImportSieveWithResponse(ctx, "user", p, adminapi.ImportSieve{Active: "main",
		Scripts: []adminapi.ImportSieveScript{{Name: "main", Script: `require "nonsense"; keep;`}}})
	if bad.StatusCode() != http.StatusUnprocessableEntity {
		t.Fatalf("invalid script: %d", bad.StatusCode())
	}

	ok, _ := c.PutImportSieveWithResponse(ctx, "user", p, adminapi.ImportSieve{Active: "main",
		Scripts: []adminapi.ImportSieveScript{{Name: "main", Script: `require "include"; include :personal "lib"; keep;`}, {Name: "lib", Script: "keep;"}}})
	if ok.StatusCode() != http.StatusNoContent {
		t.Fatalf("scripts: %d %s", ok.StatusCode(), ok.Body)
	}

	u, _ := store.User("user")
	if names, active, _ := u.ListScripts(); len(names) != 2 || active != "main" {
		t.Fatalf("stored: %v %q", names, active)
	}
}

func TestImportACLIdentifiers(t *testing.T) {
	f := newImportFixture(t)
	put := func(entries map[string]string) int {
		resp, err := f.c.PutImportACLWithResponse(context.Background(), "user", f.id, &adminapi.PutImportACLParams{XImportSession: f.session}, adminapi.ImportACL{Entries: entries})
		if err != nil {
			t.Fatal(err)
		}

		return resp.StatusCode()
	}

	for _, id := range []string{"owner", "authenticated", "$admins", "-user", "#global", "nobody"} {
		if code := put(map[string]string{id: "lr"}); code != http.StatusBadRequest {
			t.Fatalf("%s: %d", id, code)
		}
	}

	if code := put(map[string]string{"anyone": "lr", "user": "lrswipkxtea"}); code != http.StatusNoContent {
		t.Fatalf("valid: %d", code)
	}

	u, _ := f.store.User("user")
	if acl, _ := u.GetACL("INBOX"); len(acl) != 1 || acl["anyone"] != "lr" {
		t.Fatalf("acl %v", acl)
	}
}

func TestImportChangesBounded(t *testing.T) {
	f := newImportFixture(t)

	uids := make([]int64, 10001)
	for i := range uids {
		uids[i] = int64(i + 1)
	}

	resp, err := f.c.ImportChangesWithResponse(context.Background(), "user", f.id, &adminapi.ImportChangesParams{XImportSession: f.session}, adminapi.ImportChanges{Expunge: &uids})
	if err != nil || resp.StatusCode() != http.StatusBadRequest {
		t.Fatalf("%v %d", err, resp.StatusCode())
	}
}
