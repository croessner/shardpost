package imapproto

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"shardpost/internal/logging"
	"shardpost/internal/memstore"
)

type logBuffer struct {
	sync.Mutex
	b bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) { b.Lock(); defer b.Unlock(); return b.b.Write(p) }
func (b *logBuffer) String() string              { b.Lock(); defer b.Unlock(); return b.b.String() }

// sessionEvents parses the JSON session log, checks that every line
// carries the same session id and both endpoints, and counts the events.
func sessionEvents(t *testing.T, raw string) map[string]int {
	t.Helper()

	events := map[string]int{}
	session := ""

	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}

		id, _ := e["session_id"].(string)
		if id == "" {
			t.Fatal("missing session id")
		}

		if session != "" && session != id {
			t.Fatal("session id changed across UNAUTHENTICATE")
		}

		session = id

		events[e["event"].(string)]++
		if e["remote"] == "" || e["local"] == "" {
			t.Fatal("missing endpoint")
		}
	}

	return events
}

func TestSessionLoggingAndSecretSafety(t *testing.T) {
	for _, level := range []string{"info", "debug"} {
		t.Run(level, func(t *testing.T) {
			var out logBuffer

			c, _ := newTestServer(t, func(o *Options, _ *memstore.Store) { o.Log = logging.New(&out, level) })
			defer func() { _ = c.conn.Close() }()

			c.cmd("LOGIN test password-sentinel")
			c.cmd("LOGIN test test")
			c.cmd("UNAUTHENTICATE")
			c.cmd("secret-command-sentinel")
			c.cmd("LOGIN test test")
			c.cmd("LOGOUT")

			deadline := time.Now().Add(time.Second)
			for !strings.Contains(out.String(), `"event":"connection_close"`) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}

			raw := out.String()
			for _, secret := range []string{"password-sentinel", "secret-command-sentinel"} {
				if strings.Contains(raw, secret) {
					t.Fatal("secret leaked")
				}
			}

			events := sessionEvents(t, raw)

			for _, event := range []string{"connection_open", "connection_close", "auth_failed", "auth_success", "auth_cleared"} {
				if events[event] == 0 {
					t.Fatalf("missing %s", event)
				}
			}

			if (events["command_complete"] > 0) != (level == "debug") {
				t.Fatal("debug filtering broken")
			}
		})
	}
}
