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
