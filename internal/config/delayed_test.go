package config

import "testing"

// TestDelayedSend: delayed sending is off by default with a worker that
// runs; it needs a relay and the admin store indexes, and its bounds
// hold.
func TestDelayedSend(t *testing.T) {
	c, err := Load(configFile(t, "logging:\n  level: info\n"))
	if err != nil {
		t.Fatal(err)
	}

	want := JMAPDelayedSend{Worker: true, Partitions: 1, PollSeconds: 30, SendingTimeoutSeconds: 3600, Concurrency: 4, RetryAttempts: 5}
	if d := c.JMAP.Submission.Delayed; d != want {
		t.Fatalf("defaults %+v", d)
	}

	const relay = "jmap:\n  submission:\n    relay_addr: relay.example.org:587\n    delayed:\n"

	c, err = Load(configFile(t, relay+"      max_delay_seconds: 86400\n      partitions: 4\n      max_partitions: 2\n"))
	if err != nil || !c.JMAP.Submission.Delayed.Enabled() || c.JMAP.Submission.Delayed.MaxPartitions != 2 {
		t.Fatalf("on: %+v %v", c.JMAP.Submission.Delayed, err)
	}

	for _, bad := range []string{
		"jmap:\n  submission:\n    delayed:\n      max_delay_seconds: 60\n",  // no relay
		relay + "      max_delay_seconds: 60\nadmin:\n  kv_index: \"off\"\n", // no queue
		relay + "      partitions: 2\n      max_partitions: 3\n",             // more than there are
		relay + "      sending_timeout_seconds: 60\n",                        // shorter than a conversation
		relay + "      partitions: 65\n", relay + "      max_delay_seconds: -1\n",
		relay + "      concurrency: 0\n", relay + "      retry_attempts: 21\n",
	} {
		if _, err := Load(configFile(t, bad)); err == nil {
			t.Errorf("accepted: %q", bad)
		}
	}
}
