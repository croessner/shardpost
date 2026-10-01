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
		!strings.Contains(got, "\"IMPLEMENTATION\" \"ShardPost\"\r\n") || !strings.Contains(got, "\"NOTIFY\" \"mailto\"\r\n") {
		t.Errorf("the capability is %q", got)
	}

	if opts := options(Hooks{}); opts.Interp.MaxRedirects != MaxRedirects {
		t.Errorf("the interpreter allows %d redirects", opts.Interp.MaxRedirects)
	}
}

// TestNoopTag: NOOP echoes its tag in the TAG response code (RFC 5804
// 2.13), before and after authentication.
func TestNoopTag(t *testing.T) {
	var out bytes.Buffer

	c := &conn{s: New(Options{}), bw: bufio.NewWriter(&out)}

	for line, want := range map[string]string{
		"NOOP":                    "OK \"Done\"\r\n",
		`NOOP "STARTTLS-SYNC-42"`: "OK (TAG \"STARTTLS-SYNC-42\") \"Done\"\r\n",
		`NOOP "a" "b"`:            "NO ",
	} {
		out.Reset()
		handleLine(t, c, line)

		if !strings.HasPrefix(out.String(), want) {
			t.Errorf("%s answered %q, want %q", line, out.String(), want)
		}
	}
}

// TestLongScriptNames: a name of 512 bytes — 128 characters of four
// bytes each, what RFC 5804 allows — is stored and activated; one byte
// more is refused.
func TestLongScriptNames(t *testing.T) {
	store := memstore.New()
	user, _ := store.User("test")
	long := strings.Repeat("\U0001F600", 128)

	var out bytes.Buffer

	c := &conn{s: New(Options{}), user: user, bw: bufio.NewWriter(&out)}

	for _, step := range [][2]string{
		{`PUTSCRIPT "` + long + `" "keep;"`, "OK "},
		{`SETACTIVE "` + long + `"`, "OK "},
		{`PUTSCRIPT "` + long + `x" "keep;"`, "NO "},
	} {
		line, want := step[0], step[1]

		out.Reset()
		handleLine(t, c, line)

		if !strings.HasPrefix(out.String(), want) {
			t.Errorf("%.40s… answered %s", line, out.String())
		}
	}

	if _, active, _ := user.ListScripts(); active != long {
		t.Errorf("the active script is %q", active)
	}
}
