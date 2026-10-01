package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shardpost/internal/adminapi"
)

// recFlusher records the flushes it is asked for.
type recFlusher struct {
	flushed []string
	err     error
}

func (f *recFlusher) Flush(account string) error {
	f.flushed = append(f.flushed, account)
	return f.err
}

func flushServer(f AuthFlusher) *Server {
	return New(Options{MaxMessageBytes: 1, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), AuthFlush: f})
}

// flushRequest posts a flush as a socket caller with roles and returns
// the response, its decoded body and the log.
func flushRequest(t *testing.T, s *Server, body string, roles ...string) (*httptest.ResponseRecorder, adminapi.AuthFlush, string) {
	t.Helper()

	var logs bytes.Buffer

	s.o.Log = slog.New(slog.NewTextHandler(&logs, nil))

	r := httptest.NewRequest(http.MethodPost, "/admin/v1/auth/flush", strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}

	r = r.WithContext(withPrincipal(r.Context(), newPrincipal("root", "socket", roles)))

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	var out adminapi.AuthFlush
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}

	return w, out, logs.String()
}

func TestFlushAuthCacheNeedsRepair(t *testing.T) {
	f := &recFlusher{}

	if w, _, _ := flushRequest(t, flushServer(f), `{"account":"alice"}`, RoleDiagnose); w.Code != http.StatusForbidden || len(f.flushed) != 0 {
		t.Fatalf("without the repair role: %d %v", w.Code, f.flushed)
	}
}

func TestFlushAuthCache(t *testing.T) {
	f := &recFlusher{}
	s := flushServer(f)

	w, out, logs := flushRequest(t, s, `{"account":"<Alice@Example.ORG>"}`, RoleRepair)
	if w.Code != http.StatusOK || !out.Cached || out.Account != "alice@example.org" {
		t.Fatalf("answered %d %s", w.Code, w.Body.String())
	}

	if len(f.flushed) != 1 || f.flushed[0] != "alice@example.org" {
		t.Fatalf("flushed %v", f.flushed)
	}

	if !strings.Contains(logs, `msg="admin request"`) || !strings.Contains(logs, "flush_account=alice@example.org") {
		t.Fatalf("no audit line: %s", logs)
	}

	// Without an account: everything.
	if w, _, _ := flushRequest(t, s, "", RoleRepair); w.Code != http.StatusOK || f.flushed[1] != "*" {
		t.Fatalf("flush all: %d %v", w.Code, f.flushed)
	}

	// The log could not be written: this server is flushed, the caller
	// is told to try again.
	f.err = errors.New("rados down")

	if w, _, _ := flushRequest(t, s, `{"account":"bob"}`, RoleRepair); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unrecorded flush answered %d", w.Code)
	}
}

func TestFlushAuthCacheWithoutCache(t *testing.T) {
	w, out, _ := flushRequest(t, flushServer(nil), `{}`, RoleRepair)
	if w.Code != http.StatusOK || out.Cached || out.Account != "*" {
		t.Fatalf("without a cache: %d %s", w.Code, w.Body.String())
	}
}
