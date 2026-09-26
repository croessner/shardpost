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
