package model

import (
	"errors"
	"strings"
	"testing"
)

// TestCheckNewMailboxName: what a new mailbox name may and may not hold.
func TestCheckNewMailboxName(t *testing.T) {
	level := strings.Repeat("x", MaxMailboxLevelBytes)

	for _, name := range []string{
		"INBOX", "Projekte/Übersicht", "Rechnungen: 2026", "Familie \U0001F468\u200d\U0001F469\u200d\U0001F467",
		"مرحبا\u200f", "Grüße & Co", "a.b", ".hidden", "~tilde", `"quoted"`, "a#b",
		"نا\u200cمه", level, strings.Repeat(strings.Repeat("y", 204)+"/", 4) + strings.Repeat("y", 204),
	} {
		if err := CheckNewMailboxName(name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}

	for name, reason := range map[string]string{
		"":                              "empty",
		"a\xffb":                        "UTF-8",
		"a\x00b":                        "control",
		"a\tb":                          "control",
		"a\u0085b":                      "control",
		"a\u2028b":                      "control",
		"a\u2029b":                      "control",
		"a*b":                           "wildcard",
		"a%b":                           "wildcard",
		`a\b`:                           "backslash",
		"a\u202eb":                      "bidirectional",
		"a\u2066b":                      "bidirectional",
		"a\ufeffb":                      "invisible",
		"a\ufdd0b":                      "invisible",
		"a\uffffb":                      "invisible",
		"a\U0001FFFEb":                  "invisible",
		"a\u200bb":                      "invisible",
		"a\u2060b":                      "invisible",
		"x/\u200d":                      "visible",
		"x/\u200e\u200f":                "visible",
		"x/\ufe0f":                      "visible",
		"x/\u0301":                      "visible",
		"a//b":                          "empty level",
		"/a":                            "empty level",
		"a/":                            "empty level",
		"a/./b":                         `"."`,
		"..":                            `"."`,
		" a":                            "white space",
		"a ":                            "white space",
		"a/ b":                          "white space",
		"a\u00a0":                       "white space",
		"\u3000a":                       "white space",
		level + "x":                     "level longer",
		strings.Repeat("a/", 513) + "a": "longer than 1024",
	} {
		err := CheckNewMailboxName(name)

		var bad *MailboxNameError
		if !errors.As(err, &bad) || !strings.Contains(bad.Reason, reason) {
			t.Errorf("%.40q: %v, want a refusal for %q", name, err, reason)
		}
	}
}

// lister is a fixed account for the checks that read one.
type lister []string

func (l lister) ListMailboxes() ([]Mailbox, error) {
	out := make([]Mailbox, len(l))
	for i, n := range l {
		out[i] = Mailbox{Name: n}
	}

	return out, nil
}

// TestCheckNewMailboxKeepsExistingLevels: levels that exist, as a
// mailbox or only above one, are not checked again; new levels are.
func TestCheckNewMailboxKeepsExistingLevels(t *testing.T) {
	u := lister{"INBOX", "50% off", "Alt*/Sub", "Lists /x"}

	for _, name := range []string{"50% off", "50% off/2026", "Alt*", "Alt*/Sub/neu", "Alt*/Neu", "Lists /y", "Fine"} {
		if err := CheckNewMailbox(u, name); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}

	for _, name := range []string{"50% off/x*", "50%", "Alt*x", "Lists /y /z", "Alt ", "Alt/x*"} {
		if err := CheckNewMailbox(u, name); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}

// TestCheckMailboxRename: the new levels are checked, and no mailbox
// moved along may end up longer than 1024 bytes.
func TestCheckMailboxRename(t *testing.T) {
	level := strings.Repeat("x", MaxMailboxLevelBytes)
	u := lister{"INBOX", "INBOX/" + level + "/" + level + "/" + level, "A", "A/" + level + "/" + level + "/" + level, "Alt*"}
	two := strings.Repeat("y", 255) + "/" + strings.Repeat("y", 255)

	if err := CheckMailboxRename(u, "A", two); err == nil || !strings.Contains(err.Error(), "below it") {
		t.Errorf("subtree past 1024 bytes: %v", err)
	}

	if err := CheckMailboxRename(u, "INBOX", two); err != nil {
		t.Errorf("INBOX moves no children: %v", err)
	}

	if err := CheckMailboxRename(u, "A", "B"); err != nil {
		t.Errorf("plain rename: %v", err)
	}

	if err := CheckMailboxRename(u, "A", "Alt*/A"); err != nil {
		t.Errorf("below an old name: %v", err)
	}

	if err := CheckMailboxRename(u, "A", "B*"); err == nil {
		t.Error("a new wildcard was accepted")
	}
}
