package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shardpost/internal/admin"
	"shardpost/internal/adminapi"
	"shardpost/internal/auth"
	"shardpost/internal/delivery"
	"shardpost/internal/memstore"
)

// serve starts an admin API on a Unix socket with the given roles and
// returns the socket path.
func serve(t *testing.T, roles ...string) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "spctl")
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memstore.New()
	s := admin.New(admin.Options{Delivery: &delivery.Service{Store: store, Log: log}, Auth: auth.Static{"user": "pw"},
		Log: log, Version: "test", MaxMessageBytes: 1 << 20, SocketRoles: roles, Accounts: store})

	path := filepath.Join(dir, "admin.sock")

	ln, err := admin.ListenUnix(path)
	if err != nil {
		t.Fatal(err)
	}

	srv := s.UnixServer()

	go func() { _ = srv.Serve(ln) }()

	t.Cleanup(func() { _ = srv.Close() })

	return path
}

func call(socket, stdin string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer

	code := run(append([]string{"-socket", socket}, args...), strings.NewReader(stdin), &out, &errOut)

	return code, out.String(), errOut.String()
}

func TestDeliverCommand(t *testing.T) {
	socket := serve(t, admin.RoleDeliver)
	msg := "From: a@example.org\r\nSubject: x\r\n\r\nbody\r\n"

	if code, out, errOut := call(socket, msg, "deliver", "-d", "user@example.org", "-f", "a@example.org"); code != exOK || !strings.Contains(out, "stored in INBOX uid 1") {
		t.Fatalf("deliver: %d %q %q", code, out, errOut)
	}

	if code, out, errOut := call(socket, msg, "deliver", "-d", "user@example.org", "--json"); code != exOK || !strings.Contains(out, `"disposition":"stored"`) {
		t.Fatalf("deliver --json: %d %q %q", code, out, errOut)
	}

	cases := []struct {
		args []string
		code int
	}{
		{[]string{"deliver", "-d", "nobody@example.org"}, exNoUser},
		{[]string{"deliver", "-d", "user@example.org", "-m", "Missing"}, exNoUser},
		{[]string{"deliver", "-d", "user@example.org", "--no-enforce-quota"}, exNoPerm},
		{[]string{"deliver"}, exUsage},
		{[]string{"version"}, exNoPerm},
		{[]string{"frobnicate"}, exUsage},
	}

	for _, c := range cases {
		if code, _, errOut := call(socket, msg, c.args...); code != c.code {
			t.Errorf("%v: %d, want %d (%s)", c.args, code, c.code, errOut)
		}
	}

	if code, _, _ := call(filepath.Join(filepath.Dir(socket), "absent.sock"), msg, "deliver", "-d", "user@example.org"); code != exTempFail {
		t.Fatalf("unreachable socket: %d", code)
	}
}

// TestPrintDelivery: the text output names stored copies, a rejection
// and a discard; an answer without a disposition (an older server) with
// nothing stored is a discard.
func TestPrintDelivery(t *testing.T) {
	stored := []adminapi.StoredMessage{{Mailbox: "INBOX", Uid: 7}}

	for _, c := range []struct {
		res  adminapi.DeliveryResult
		want string
	}{
		{adminapi.DeliveryResult{Account: "u", Stored: stored, Disposition: adminapi.DispositionStored}, "u: stored in INBOX uid 7\n"},
		{adminapi.DeliveryResult{Account: "u", Disposition: adminapi.DispositionRejected}, "u: rejected by sieve, rejection message sent to the sender\n"},
		{adminapi.DeliveryResult{Account: "u", Disposition: adminapi.DispositionDiscarded}, "u: discarded by sieve\n"},
		{adminapi.DeliveryResult{Account: "u"}, "u: discarded by sieve\n"},
		{adminapi.DeliveryResult{Account: "u", Stored: stored}, "u: stored in INBOX uid 7\n"},
	} {
		var out bytes.Buffer
		if printDelivery(&out, &c.res); out.String() != c.want {
			t.Errorf("%+v: %q", c.res, out.String())
		}
	}
}

func TestVersionCommand(t *testing.T) {
	socket := serve(t, admin.RoleDiagnose)

	if code, out, _ := call(socket, "", "version"); code != exOK || !strings.HasPrefix(out, "test (") {
		t.Fatalf("version: %d %q", code, out)
	}
}

func TestConnectionAndFailures(t *testing.T) {
	if code, _, _ := call("/nonexistent", "", "-url", "https://127.0.0.1:1", "version"); code != exUsage {
		t.Fatalf("-url without certificate: %d", code)
	}

	var errOut bytes.Buffer
	if code := failure(&errOut, nil, 404); code != exSoftware || !strings.Contains(errOut.String(), "404") {
		t.Fatalf("answer without error document: %d %q", code, errOut.String())
	}

	if code := failure(&errOut, &adminapi.Error{Code: adminapi.LimitExceeded, Message: "account limit reached"}, 409); code != exCantCreate {
		t.Fatalf("limit_exceeded: %d", code)
	}
}

func TestTraceCommand(t *testing.T) {
	socket := serve(t, admin.RoleTrace)

	if code, out, _ := call(socket, "", "trace", "status", "user"); code != exOK || out != "user: trace off\n" {
		t.Fatalf("status: %d %q", code, out)
	}

	code, out, errOut := call(socket, "", "trace", "on", "-for", "15m", "user")
	if code != exOK || !strings.Contains(out, "user: trace on until ") || !strings.Contains(out, "set by uid:") {
		t.Fatalf("on: %d %q %q", code, out, errOut)
	}

	if code, out, _ := call(socket, "", "trace", "status", "--json", "user"); code != exOK || !strings.Contains(out, `"active":true`) {
		t.Fatalf("status json: %d %q", code, out)
	}

	if code, out, _ := call(socket, "", "trace", "off", "user"); code != exOK || out != "user: trace off\n" {
		t.Fatalf("off: %d %q", code, out)
	}
}

func TestTraceCommandErrors(t *testing.T) {
	socket := serve(t, admin.RoleTrace)

	if code, _, errOut := call(socket, "", "trace", "on", "User@Example.org"); code != exUsage || !strings.Contains(errOut, "use user") {
		t.Fatalf("alias: %d %q", code, errOut)
	}

	for _, args := range [][]string{{"trace"}, {"trace", "on"}, {"trace", "off", "-for", "1m", "user"}, {"trace", "maybe", "user"}} {
		if code, _, _ := call(socket, "", args...); code != exUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
}
