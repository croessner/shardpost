package config

import (
	"strings"
	"testing"
)

// TestQuotaRecountDefaults: the online recount and mailbox lock keys
// have their defaults.
func TestQuotaRecountDefaults(t *testing.T) {
	c, err := Load(configFile(t, "server: {}\n"))
	if err != nil {
		t.Fatal(err)
	}

	got := []any{c.Quota.OnlineRecount, c.Quota.RecountBarrierSeconds, c.Quota.RecountDeadIntentSeconds, c.Quota.RecountSwitchAttempts,
		c.Quota.RecountHoldMS, c.Quota.RecountMaxCaptureKeys, c.Quota.RecountMaxCaptureEntries, c.Quota.RecountReleaseChunk,
		c.Jobs.MailboxDeleteOnline, c.Jobs.MailboxLockBarrierSeconds}
	want := []any{OnlineOff, 120, 600, 10, 2000, 1000, 1000000, 10000, OnlineOff, 120}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("default %d: %v, want %v", i, got[i], want[i])
		}
	}
}

// TestQuotaRecountValidation: the dead-intent time is bounded by the
// intent grace of the scrub, the mailbox lock needs a settle time IMAP
// sessions can notice, and the enumerations and ranges hold.
func TestQuotaRecountValidation(t *testing.T) {
	for body, want := range map[string]string{
		"quota:\n  recount_dead_intent_seconds: 1200\nscrub:\n  intent_grace_seconds: 900\n": "must not exceed scrub.intent_grace_seconds",
		"jobs:\n  mailbox_delete_online: auto\n  settle_seconds: 5\n":                        "must be at least 7",
		"quota:\n  online_recount: maybe\n":                                                  "OnlineRecount",
		"quota:\n  recount_hold_ms: 9000\n":                                                  "RecountHoldMS",
	} {
		_, err := Load(configFile(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", body, err, want)
		}
	}

	if _, err := Load(configFile(t, "jobs:\n  mailbox_delete_online: auto\nquota:\n  online_recount: auto\n")); err != nil {
		t.Errorf("auto settings: %v", err)
	}
}
