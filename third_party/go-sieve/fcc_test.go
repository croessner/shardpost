package sieve

import (
	"strings"
	"testing"

	"github.com/foxcpp/go-sieve/interp"
)

// TestFcc: :fcc (RFC 8580) of vacation and notify reaches the applied
// action with its companions, also after a binary save and restore; a
// vacation without :days/:seconds has no period set.
func TestFcc(t *testing.T) {
	testExecute(t, `require ["vacation", "fcc", "mailbox", "imap4flags", "special-use", "enotify"];
vacation :days 3 :fcc "Sent" :create :flags ["\\Seen"] :specialuse "\\Sent" "away";
notify :fcc "Notes" "mailto:n@example.org";`, eml, []interp.AppliedAction{
		interp.ActionVacation{Reason: "away", Seconds: 3 * 86400, PeriodSet: true,
			Fcc: &interp.FccTarget{Mailbox: "Sent", Create: true, Flags: interp.Flags{`\Seen`}, SpecialUse: `\Sent`}},
		interp.ActionNotify{Method: "mailto:n@example.org", Importance: "2", Mailto: &interp.MailtoURI{To: []string{"n@example.org"}},
			Fcc: &interp.FccTarget{Mailbox: "Notes"}},
		interp.ActionKeep{Implicit: true},
	})

	testExecute(t, `require "vacation"; vacation "away";`, eml, []interp.AppliedAction{
		interp.ActionVacation{Reason: "away"},
		interp.ActionKeep{Implicit: true},
	})
}

// TestFccLoadErrors: the companions need :fcc and their extensions.
func TestFccLoadErrors(t *testing.T) {
	for _, script := range []string{
		`require "vacation"; vacation :fcc "Sent" "away";`,
		`require ["vacation", "fcc"]; vacation :fcc "Sent" :create "away";`,
		`require ["vacation", "fcc", "mailbox"]; vacation :create "away";`,
		`require ["enotify", "fcc", "imap4flags"]; notify :flags "\\Seen" "mailto:n@example.org";`,
	} {
		if _, err := Load(strings.NewReader(script), DefaultOptions()); err == nil {
			t.Errorf("%s loaded", script)
		}
	}
}
