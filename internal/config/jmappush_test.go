package config

import "testing"

// TestJMAPPushFromEnvironment: the nested push settings come from the
// environment, and a VAPID subject that is no URI is refused.
func TestJMAPPushFromEnvironment(t *testing.T) {
	t.Setenv("JMAP_PUSH_VAPID_KEY_FILE", "/k.pem")
	t.Setenv("JMAP_PUSH_VAPID_SUBJECT", "mailto:a@b")
	t.Setenv("JMAP_PUSH_SCAN_SECONDS", "7")

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	if c.JMAP.Push.VAPIDKeyFile != "/k.pem" || c.JMAP.Push.ScanSeconds != 7 {
		t.Fatalf("%+v", c.JMAP.Push)
	}

	t.Setenv("JMAP_PUSH_VAPID_SUBJECT", "nope")

	if _, err := Load(""); err == nil {
		t.Fatal("bad subject accepted")
	}
}

// TestJMAPPushPartitionSettings: the partition settings of the worker
// (stage B18a) have their defaults, come from the environment, and the
// relations between them are checked.
func TestJMAPPushPartitionSettings(t *testing.T) {
	t.Setenv("JMAP_PUSH_VAPID_KEY_FILE", "/k.pem")
	t.Setenv("JMAP_PUSH_VAPID_SUBJECT", "mailto:a@b")

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	p := c.JMAP.Push
	if p.LeaseSeconds != 30 || p.TakeSeconds != 5 || p.RetrySeconds != 2 || p.OrphanHoldSeconds != 600 ||
		p.FullScanSeconds != 3600 || p.RebalanceSeconds != 10 || p.RebalanceSettleSeconds != 60 ||
		p.HandoffSeconds != 5 || p.MaxWatchedAccounts != 50000 || p.StartRate != 5000 || p.MaxPartitions != 0 {
		t.Fatalf("defaults %+v", p)
	}

	t.Setenv("JMAP_PUSH_MAX_PARTITIONS", "8")
	t.Setenv("JMAP_PUSH_START_RATE", "10000")
	t.Setenv("JMAP_PUSH_FULL_SCAN_SECONDS", "0")

	if c, err = Load(""); err != nil || c.JMAP.Push.MaxPartitions != 8 || c.JMAP.Push.StartRate != 10000 || c.JMAP.Push.FullScanSeconds != 0 {
		t.Fatalf("from the environment: %+v %v", c.JMAP.Push, err)
	}

	for _, bad := range []struct{ key, value string }{
		{"JMAP_PUSH_LEASE_SECONDS", "14"},            // below 3 × take
		{"JMAP_PUSH_TAKE_SECONDS", "11"},             // lease 30 < 33
		{"JMAP_PUSH_RETRY_SECONDS", "6"},             // above take
		{"JMAP_PUSH_REBALANCE_SETTLE_SECONDS", "20"}, // below the lease
		{"JMAP_PUSH_HANDOFF_SECONDS", "25"},          // not below lease - 5
		{"JMAP_PUSH_FULL_SCAN_SECONDS", "10"},        // below the scan
		{"JMAP_PUSH_MAX_WATCHED_ACCOUNTS", "50"},     // below 100
		{"JMAP_PUSH_MAX_PARTITIONS", "65"},           // above 64
	} {
		t.Run(bad.key, func(t *testing.T) {
			t.Setenv(bad.key, bad.value)

			if _, err := Load(""); err == nil {
				t.Fatalf("%s=%s accepted", bad.key, bad.value)
			}
		})
	}
}

// TestJMAPSubmissionDefaults: XCLIENT and the sender check are on unless
// switched off, and both off is refused.
func TestJMAPSubmissionDefaults(t *testing.T) {
	t.Setenv("JMAP_SUBMISSION_RELAY_ADDR", "relay.example.org:587")
	t.Setenv("JMAP_SUBMISSION_ADDRESS_ATTRIBUTES", "mail, mailAlternateAddress")

	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}

	s := c.JMAP.Submission
	if !s.Enabled() || !s.XClient || !s.XClientAddr || !s.CheckSender || len(s.Attributes()) != 2 || s.Attributes()[1] != "mailAlternateAddress" {
		t.Fatalf("%+v %v", s, s.Attributes())
	}

	t.Setenv("JMAP_SUBMISSION_XCLIENT", "false")
	t.Setenv("JMAP_SUBMISSION_CHECK_SENDER", "false")

	if _, err := Load(""); err == nil {
		t.Fatal("a relay without XCLIENT and without the sender check was accepted")
	}
}
