package admin

import (
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

// serveTrace is an admin socket with the given roles and the trace
// switch of a memstore enabled (maximum one hour).
func serveTrace(t *testing.T, roles ...string) (*memstore.Store, *adminapi.ClientWithResponses) {
	t.Helper()

	store := memstore.New()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Options{Delivery: &delivery.Service{Store: store, Log: log}, Auth: auth.Static{"user": "pw"}, Log: log, Version: "test",
		MaxMessageBytes: 4096, SocketRoles: roles, Accounts: store, TraceMaxDuration: time.Hour, TraceDefaultDuration: 10 * time.Minute})

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

func storedTrace(t *testing.T, store *memstore.Store, account string) model.TraceState {
	t.Helper()

	u, _ := store.User(account)

	st, err := u.(model.Tracer).Trace()
	if err != nil {
		t.Fatal(err)
	}

	return st
}

func TestTraceNeedsRole(t *testing.T) {
	_, c := serveTrace(t, RoleDiagnose, RoleImport)

	get, err := c.GetTraceWithResponse(context.Background(), "user")
	if err != nil || get.StatusCode() != http.StatusForbidden {
		t.Fatalf("get without role: %v %d", err, get.StatusCode())
	}

	set, _ := c.SetTraceWithResponse(context.Background(), "user", adminapi.TraceRequest{})
	if set.StatusCode() != http.StatusForbidden {
		t.Fatalf("set without role: %d", set.StatusCode())
	}
}

// traceOnWithDefault switches the trace of "user" on without a duration
// and checks the answer.
func traceOnWithDefault(t *testing.T, c *adminapi.ClientWithResponses) {
	t.Helper()

	before := time.Now()

	set, _ := c.SetTraceWithResponse(context.Background(), "user", adminapi.TraceRequest{})
	if set.JSON200 == nil || !set.JSON200.Active || set.JSON200.SetBy == nil || !strings.HasPrefix(*set.JSON200.SetBy, "uid:") {
		t.Fatalf("set: %d %+v %v", set.StatusCode(), set.JSON200, set.JSONDefault)
	}

	if d := set.JSON200.Until.Sub(before); d < 9*time.Minute || d > 11*time.Minute {
		t.Fatalf("default duration: %v", d)
	}
}

func TestTraceOnStatusOff(t *testing.T) {
	store, c := serveTrace(t, RoleTrace)
	ctx := context.Background()

	get, _ := c.GetTraceWithResponse(ctx, "user")
	if get.JSON200 == nil || get.JSON200.Active || get.JSON200.Until != nil {
		t.Fatalf("initial: %d %+v", get.StatusCode(), get.JSON200)
	}

	traceOnWithDefault(t, c)

	get, _ = c.GetTraceWithResponse(ctx, "user")
	if get.JSON200 == nil || !get.JSON200.Active {
		t.Fatalf("status: %+v", get.JSON200)
	}

	off, _ := c.ClearTraceWithResponse(ctx, "user")
	if off.StatusCode() != http.StatusNoContent {
		t.Fatalf("clear: %d", off.StatusCode())
	}

	if st := storedTrace(t, store, "user"); st.Active(time.Now()) || !st.Until.IsZero() {
		t.Fatalf("after clear: %+v", st)
	}
}

func TestTraceRefusesAliasAndBadDuration(t *testing.T) {
	_, c := serveTrace(t, RoleTrace)
	ctx := context.Background()

	set, _ := c.SetTraceWithResponse(ctx, "User@Example.org", adminapi.TraceRequest{})
	if set.StatusCode() != http.StatusBadRequest || !strings.Contains(set.JSONDefault.Message, "use user") {
		t.Fatalf("alias: %d %v", set.StatusCode(), set.JSONDefault)
	}

	zero := 0

	set, _ = c.SetTraceWithResponse(ctx, "user", adminapi.TraceRequest{DurationSeconds: &zero})
	if set.StatusCode() != http.StatusBadRequest {
		t.Fatalf("zero duration: %d", set.StatusCode())
	}

	set, _ = c.SetTraceWithResponse(ctx, "nobody", adminapi.TraceRequest{})
	if set.StatusCode() != http.StatusNotFound {
		t.Fatalf("unknown account: %d", set.StatusCode())
	}

	// Status and off refuse aliases too: they would report or clear a
	// switch that no session reads.
	get, _ := c.GetTraceWithResponse(ctx, "User@Example.org")
	off, _ := c.ClearTraceWithResponse(ctx, "User@Example.org")

	if get.StatusCode() != http.StatusBadRequest || off.StatusCode() != http.StatusBadRequest {
		t.Fatalf("alias status/off: %d %d", get.StatusCode(), off.StatusCode())
	}
}

func TestTraceOperationLabel(t *testing.T) {
	if got := operation("/admin/v1/accounts/user@example.org/trace"); got != "trace" {
		t.Fatalf("label: %q", got)
	}
}

func TestTraceDurationIsCapped(t *testing.T) {
	store, c := serveTrace(t, RoleTrace)
	ctx := context.Background()

	// The maximum caps a longer request.
	week := 7 * 24 * 3600
	set, _ := c.SetTraceWithResponse(ctx, "user", adminapi.TraceRequest{DurationSeconds: &week})

	if st := storedTrace(t, store, "user"); !st.Active(time.Now()) || time.Until(st.Until) > time.Hour {
		t.Fatalf("stored: %+v (%d)", st, set.StatusCode())
	}

	// No overflow for absurd durations.
	huge := int(^uint(0) >> 1)
	set, _ = c.SetTraceWithResponse(ctx, "user", adminapi.TraceRequest{DurationSeconds: &huge})

	if st := storedTrace(t, store, "user"); !st.Active(time.Now()) || time.Until(st.Until) > time.Hour {
		t.Fatalf("stored: %+v (%d)", st, set.StatusCode())
	}
}
