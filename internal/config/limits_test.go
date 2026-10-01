package config

import "testing"

// TestLimitsDefaultsAndChecks: every account limit has a default far
// above normal use, 0 switches one off, and a negative value is refused.
func TestLimitsDefaultsAndChecks(t *testing.T) {
	c, err := Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil {
		t.Fatal(err)
	}

	want := Limits{Mailboxes: 10000, MailboxNames: 50000, KeywordsPerMailbox: 1000, MetadataEntries: 1000, SharedPointers: 10000}
	if c.Limits != want {
		t.Errorf("defaults %+v", c.Limits)
	}

	c, err = Load(configFile(t, "limits:\n  mailboxes: 0\n  keywords_per_mailbox: 64\n"))
	if err != nil || c.Limits.Mailboxes != 0 || c.Limits.KeywordsPerMailbox != 64 || c.Limits.MailboxNames != 50000 {
		t.Errorf("set: %+v, %v", c.Limits, err)
	}

	t.Setenv("LIMITS_SHARED_POINTERS", "7")

	c, err = Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil || c.Limits.SharedPointers != 7 {
		t.Errorf("env: %+v, %v", c.Limits, err)
	}

	for _, body := range []string{
		"limits:\n  mailboxes: -1\n",
		"limits:\n  metadata_entries: -5\n",
		"limits:\n  no_such_limit: 1\n",
	} {
		if _, err := Load(configFile(t, body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}
