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

// TestReservedScriptIsReadOnly: the script of the JMAP VacationResponse
// is listed and can be read, and nothing may change it.
func TestReservedScriptIsReadOnly(t *testing.T) {
	store := memstore.New()

	user, _ := store.User("test")
	if err := user.PutScript(JMAPVacationScript, `require "vacation"; vacation "away";`); err != nil {
		t.Fatal(err)
	}

	if err := user.PutScript("main", "keep;"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer

	c := &conn{s: New(Options{}), user: user, bw: bufio.NewWriter(&out)}

	for _, line := range []string{
		`PUTSCRIPT "jmap-vacation" "keep;"`,
		`DELETESCRIPT "jmap-vacation"`,
		`SETACTIVE "jmap-vacation"`,
		`RENAMESCRIPT "jmap-vacation" "mine"`,
		`RENAMESCRIPT "main" "jmap-vacation"`,
	} {
		out.Reset()
		handleLine(t, c, line)

		if !strings.HasPrefix(out.String(), "NO ") || !strings.Contains(out.String(), "reserved") {
			t.Errorf("%s answered %s", line, out.String())
		}
	}

	if text, _ := user.GetScript(JMAPVacationScript); !strings.Contains(text, "away") {
		t.Errorf("the script changed: %q", text)
	}

	if _, active, _ := user.ListScripts(); active != "" {
		t.Errorf("active script %q", active)
	}

	out.Reset()
	handleLine(t, c, "LISTSCRIPTS")
	handleLine(t, c, `GETSCRIPT "jmap-vacation"`)

	if got := out.String(); !strings.Contains(got, `"jmap-vacation"`) || !strings.Contains(got, `vacation "away"`) {
		t.Errorf("listing and reading: %s", got)
	}
}

// TestCapabilityMatchesTheInterpreter: MAXREDIRECTS is what a run of a
// script may take, and the SIEVE list grows with the settings.
func TestCapabilityMatchesTheInterpreter(t *testing.T) {
	var out bytes.Buffer

	c := &conn{s: New(Options{Config: map[string]string{"imapsieve_enabled": "yes"}}), bw: bufio.NewWriter(&out)}
	c.capabilities()

	if err := c.bw.Flush(); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	if !strings.Contains(got, "\"MAXREDIRECTS\" \"4\"\r\n") || !strings.Contains(got, " imapsieve\"\r\n") ||
		!strings.Contains(got, "\"IMPLEMENTATION\" \"ShardPost\"\r\n") {
		t.Errorf("the capability is %q", got)
	}

	if opts := options(Hooks{}); opts.Interp.MaxRedirects != MaxRedirects {
		t.Errorf("the interpreter allows %d redirects", opts.Interp.MaxRedirects)
	}
}
