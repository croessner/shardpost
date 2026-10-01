package admin

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/membudget"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

const message = "From: sender@example.org\r\nTo: user@example.org\r\nSubject: quota\r\n\r\nbody\r\n"

// testSetup is a server on a Unix socket with a memstore and a static
// user database containing "user".
type testSetup struct {
	store  *memstore.Store
	client *adminapi.ClientWithResponses
}

func newTestServer(t *testing.T, o Options) *Server {
	t.Helper()

	store := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	if o.Delivery == nil {
		o.Delivery = &delivery.Service{Store: store, Log: log}
	}

	o.Auth, o.Log, o.Version = auth.Static{"user": "pw"}, log, "test"
	if o.MaxMessageBytes == 0 {
		o.MaxMessageBytes = 1 << 20
	}

	return New(o)
}

// socketDir is short: Unix socket paths are limited to about 100 bytes.
func socketDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "spadm")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

func serveSocket(t *testing.T, roles ...string) testSetup {
	t.Helper()

	store := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := newTestServer(t, Options{Delivery: &delivery.Service{Store: store, Log: log, ReturnPath: true, DeliveredTo: delivery.DeliveredToFinal},
		SocketRoles: roles, MaxMessageBytes: 4096})

	path := filepath.Join(socketDir(t), "admin.sock")

	ln, err := ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}

	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode: %v %v", st.Mode(), err)
	}

	srv := s.UnixServer()

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	client, err := adminapi.NewSocketClient(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	return testSetup{store: store, client: client}
}

func deliver(t *testing.T, ts testSetup, p adminapi.DeliverParams, contentType, body string) *adminapi.DeliverResponse {
	t.Helper()

	resp, err := ts.client.DeliverWithBodyWithResponse(context.Background(), &p, contentType, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	return resp
}

func errorCode(r *adminapi.DeliverResponse) adminapi.ErrorCode {
	if r.JSONDefault == nil {
		return ""
	}

	return r.JSONDefault.Code
}

func TestVersionNeedsRole(t *testing.T) {
	ts := serveSocket(t, RoleDeliver)

	resp, err := ts.client.GetVersionWithResponse(context.Background())
	if err != nil || resp.StatusCode() != http.StatusForbidden || resp.JSONDefault == nil || resp.JSONDefault.Code != adminapi.Forbidden {
		t.Fatalf("without role: %v %d", err, resp.StatusCode())
	}

	ts = serveSocket(t, RoleDiagnose)

	resp, err = ts.client.GetVersionWithResponse(context.Background())
	if err != nil || resp.StatusCode() != http.StatusOK || resp.JSON200.Version != "test" {
		t.Fatalf("with role: %v %d", err, resp.StatusCode())
	}
}

func TestDeliver(t *testing.T) {
	ts := serveSocket(t, RoleDeliver)

	resp := deliver(t, ts, adminapi.DeliverParams{Recipient: "user@example.org"}, "message/rfc822", message)
	if resp.StatusCode() != http.StatusOK || resp.JSON200.Account != "user" || len(resp.JSON200.Stored) != 1 || resp.JSON200.Stored[0].Mailbox != "INBOX" {
		t.Fatalf("deliver: %d %s", resp.StatusCode(), resp.Body)
	}

	cases := []struct {
		name        string
		p           adminapi.DeliverParams
		contentType string
		body        string
		status      int
		code        adminapi.ErrorCode
	}{
		{"unknown user", adminapi.DeliverParams{Recipient: "nobody@example.org"}, "message/rfc822", message, http.StatusNotFound, adminapi.NoSuchUser},
		{"media type", adminapi.DeliverParams{Recipient: "user@example.org"}, "text/plain", message, http.StatusBadRequest, adminapi.BadRequest},
		{"header injection", adminapi.DeliverParams{Recipient: "user@example.org\r\nX: y"}, "message/rfc822", message, http.StatusBadRequest, adminapi.BadRequest},
		{"too large", adminapi.DeliverParams{Recipient: "user@example.org"}, "message/rfc822", strings.Repeat("x", 5000), http.StatusRequestEntityTooLarge, adminapi.TooLarge},
		{"missing mailbox", adminapi.DeliverParams{Recipient: "user@example.org", Mailbox: ptr("Nope")}, "message/rfc822", message, http.StatusNotFound, adminapi.NoSuchMailbox},
		{"noenforce without role", adminapi.DeliverParams{Recipient: "user@example.org", EnforceQuota: ptr(false)}, "message/rfc822", message, http.StatusForbidden, adminapi.Forbidden},
	}

	for _, c := range cases {
		resp := deliver(t, ts, c.p, c.contentType, c.body)
		if resp.StatusCode() != c.status || errorCode(resp) != c.code {
			t.Errorf("%s: %d %s", c.name, resp.StatusCode(), resp.Body)
		}
	}
}

func TestDeliverQuota(t *testing.T) {
	ts := serveSocket(t, RoleDeliver, RoleDeliverNoEnforce)

	u, _ := ts.store.User("user")
	if err := u.SetQuotaLimit(model.Quota{Bytes: 10}); err != nil {
		t.Fatal(err)
	}

	resp := deliver(t, ts, adminapi.DeliverParams{Recipient: "user@example.org"}, "message/rfc822", message)
	if resp.StatusCode() != http.StatusInsufficientStorage || errorCode(resp) != adminapi.OverQuota {
		t.Fatalf("enforced: %d %s", resp.StatusCode(), resp.Body)
	}

	resp = deliver(t, ts, adminapi.DeliverParams{Recipient: "user@example.org", EnforceQuota: ptr(false)}, "message/rfc822", message)
	if resp.StatusCode() != http.StatusOK || len(resp.JSON200.Stored) != 1 {
		t.Fatalf("not enforced: %d %s", resp.StatusCode(), resp.Body)
	}
}

func TestSocketPath(t *testing.T) {
	dir := socketDir(t)
	path := filepath.Join(dir, "admin.sock")

	// A stale socket of an earlier run is replaced.
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}

	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()

	ln, err := ListenUnix(path)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}

	_ = ln.Close()

	regular := filepath.Join(dir, "file")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ListenUnix(regular); err == nil {
		t.Fatal("regular file replaced")
	}
}

func TestUnknownCaller(t *testing.T) {
	s := newTestServer(t, Options{SocketRoles: []string{RoleDiagnose}})

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/version", nil))

	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"unauthenticated"`) {
		t.Fatalf("no principal: %d %s", rec.Code, rec.Body)
	}
}

func ptr[T any](v T) *T { return &v }

// TestDeliverRejectOfNullSender: mail from the null sender gets no
// rejection at all (RFC 5429 2.1): it is discarded, and the delivery
// succeeds with nothing stored.
func TestDeliverRejectOfNullSender(t *testing.T) {
	ts := serveSocket(t, RoleDeliver)

	u, _ := ts.store.User("user")
	if err := u.PutScript("main", `require "reject"; reject "no";`); err != nil {
		t.Fatal(err)
	}

	if err := u.SetActiveScript("main"); err != nil {
		t.Fatal(err)
	}

	resp := deliver(t, ts, adminapi.DeliverParams{Recipient: "user@example.org"}, "message/rfc822", message)
	if resp.StatusCode() != http.StatusOK || len(resp.JSON200.Stored) != 0 {
		t.Fatalf("sieve reject of the null sender: %d %s", resp.StatusCode(), resp.Body)
	}
}

func TestDeliverOptionsAndTrace(t *testing.T) {
	ts := serveSocket(t, RoleDeliver)

	u, _ := ts.store.User("user")
	if err := u.PutScript("main", `require "reject"; reject "no";`); err != nil {
		t.Fatal(err)
	}

	if err := u.SetActiveScript("main"); err != nil {
		t.Fatal(err)
	}

	// Without a relay a reject is refused in the protocol.
	resp := deliver(t, ts, adminapi.DeliverParams{Recipient: "user@example.org", From: ptr("sender@example.org")}, "message/rfc822", message)
	if resp.StatusCode() != http.StatusUnprocessableEntity || errorCode(resp) != adminapi.Rejected {
		t.Fatalf("sieve reject: %d %s", resp.StatusCode(), resp.Body)
	}

	resp = deliver(t, ts, adminapi.DeliverParams{Recipient: "<user@example.org>", From: ptr(""), Sieve: ptr(false)}, "message/rfc822", message)
	if resp.StatusCode() != http.StatusOK || len(resp.JSON200.Stored) != 1 {
		t.Fatalf("sieve=false: %d %s", resp.StatusCode(), resp.Body)
	}

	mb, _ := u.Lookup("INBOX")
	metas, _ := u.ListMessages(mb.ID)

	raw, err := u.Read(&metas[0])
	// The null sender is "<>"; Return-Path and Delivered-To lie above the
	// LDA's Received field.
	if err != nil || !strings.HasPrefix(string(raw), "Return-Path: <>\r\nDelivered-To: user@example.org\r\nReceived: from unix uid:") ||
		!strings.Contains(string(raw), "with LDA") {
		t.Fatalf("trace header: %v %q", err, raw)
	}

	for _, from := range []string{"a b@example.org", "a@example.org> SIZE=1", "<<a@example.org>>"} {
		resp = deliver(t, ts, adminapi.DeliverParams{Recipient: "user@example.org", From: ptr(from)}, "message/rfc822", message)
		if resp.StatusCode() != http.StatusBadRequest {
			t.Errorf("sender %q accepted: %d", from, resp.StatusCode())
		}
	}
}

func TestDeliverBusy(t *testing.T) {
	ts := serveSocket(t, RoleDeliver)

	old := membudget.Limit()

	membudget.SetLimit(1)

	t.Cleanup(func() { membudget.SetLimit(old) })

	resp := deliver(t, ts, adminapi.DeliverParams{Recipient: "user@example.org"}, "message/rfc822", message)
	if resp.StatusCode() != http.StatusServiceUnavailable || errorCode(resp) != adminapi.Temporary {
		t.Fatalf("busy: %d %s", resp.StatusCode(), resp.Body)
	}
}

func TestForeignUIDRefused(t *testing.T) {
	old := ownUID
	ownUID = func() uint32 { return uint32(os.Getuid()) + 1 }

	t.Cleanup(func() { ownUID = old })

	ts := serveSocket(t, RoleDiagnose)

	resp, err := ts.client.GetVersionWithResponse(context.Background())
	if err != nil || resp.StatusCode() != http.StatusUnauthorized {
		t.Fatalf("foreign uid: %v %d", err, resp.StatusCode())
	}
}

func TestAuditLine(t *testing.T) {
	var logs bytes.Buffer

	log := slog.New(slog.NewTextHandler(&logs, nil))
	s := New(Options{Delivery: &delivery.Service{Store: memstore.New(), Log: log}, Auth: auth.Static{"user": "pw"}, Log: log,
		MaxMessageBytes: 1 << 20, SocketRoles: []string{RoleDiagnose}})

	req := httptest.NewRequest(http.MethodPost, "/admin/v1/deliveries?recipient=user@example.org&from=a@example.org", strings.NewReader(message))
	req.Header.Set("Content-Type", "message/rfc822")
	req = req.WithContext(withPrincipal(req.Context(), newPrincipal("uid:1", "unix", []string{RoleDiagnose})))

	s.Handler().ServeHTTP(httptest.NewRecorder(), req)

	for _, want := range []string{"event=admin_request", "principal=uid:1", "transport=unix", "status=403", "code=forbidden", "recipient=user@example.org", "from=a@example.org"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("audit line lacks %s: %s", want, logs.String())
		}
	}

	if strings.Contains(logs.String(), "body") {
		t.Error("message content in the audit line")
	}
}
