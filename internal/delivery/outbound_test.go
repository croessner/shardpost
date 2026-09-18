package delivery

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"shardpost/internal/outbound"
)

func TestRedirectLoopGuardPreservesMessage(t *testing.T) {
	// A deliberately unreachable relay makes an attempted send fail the test.
	r := &run{
		svc: &Service{Relay: &outbound.Relay{Addr: "127.0.0.1:1"}},
		req: Request{Account: "me@example.org", Rcpt: "alias@example.org"},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	for _, header := range []string{
		"X-Sieve-Redirected-From: <me@example.org>\r\n",
		"X-Sieve-Redirected-From: other@example.org\r\nX-Sieve-Redirected-From: ME@example.org\r\n",
		"X-Sieve-Redirected-From: alias@example.org\r\n",
		strings.Repeat("Received: test\r\n", 26),
	} {
		raw := []byte(header + "From: sender@example.org\r\nTo: me@example.org\r\n\r\noriginal")
		before := string(raw)

		sent, err := r.redirect(context.Background(), raw, []string{"target@example.org"})
		if err != nil || sent || string(raw) != before {
			t.Fatalf("loop guard: sent=%v err=%v", sent, err)
		}
	}
}
