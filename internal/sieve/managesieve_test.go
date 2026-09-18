package sieve

import (
	"bufio"
	"bytes"
	"shardpost/internal/memstore"
	"strings"
	"testing"
)

func TestSetActiveRejectsIMAPSieve(t *testing.T) {
	store := memstore.New()

	user, _ := store.User("test")
	if err := user.PutScript("delivery", "keep;"); err != nil {
		t.Fatal(err)
	}

	if err := user.PutScript("events", `require "imapsieve"; keep;`); err != nil {
		t.Fatal(err)
	}

	if err := user.SetActiveScript("delivery"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer

	c := &conn{s: New(Options{Config: map[string]string{"imapsieve_enabled": "yes"}}), user: user, bw: bufio.NewWriter(&out)}
	handleLine(t, c, `SETACTIVE "events"`)

	if !strings.HasPrefix(out.String(), "NO ") {
		t.Fatalf("activation accepted: %s", out.String())
	}

	out.Reset()
	handleLine(t, c, `SETACTIVE "delivery"`)

	if !strings.HasPrefix(out.String(), "OK ") {
		t.Fatalf("delivery activation rejected: %s", out.String())
	}
}

// handleLine runs one command line and flushes the response.
func handleLine(t *testing.T, c *conn, line string) {
	t.Helper()

	if _, err := c.handle(line); err != nil {
		t.Fatal(err)
	}

	if err := c.bw.Flush(); err != nil {
		t.Fatal(err)
	}
}
