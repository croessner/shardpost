package main

import (
	"testing"
	"time"

	"shardpost/internal/config"
)

// TestLongestConversation: the defaults (30 s steps, 50 MiB) stay below
// the default sending timeout of an hour, and the bound grows with the
// size.
func TestLongestConversation(t *testing.T) {
	if got := longestConversation(30*time.Second, 50<<20); got != 1100*time.Second {
		t.Fatalf("defaults: %v", got)
	}

	if longestConversation(30*time.Second, 1<<30) <= time.Hour {
		t.Fatal("a 1 GiB message fits an hour")
	}
}

// TestJMAPQuotaWarnings: the warnLimit is the lowest warning on the way
// up, per resource; warnings on the way down do not count.
func TestJMAPQuotaWarnings(t *testing.T) {
	got := jmapQuota(config.Quota{Warnings: []config.QuotaWarning{
		{Name: "a", Resource: "storage", Percent: 95, Direction: "over"},
		{Name: "b", Resource: "storage", Percent: 80},
		{Name: "c", Resource: "storage", Percent: 50, Direction: "under"},
		{Name: "d", Resource: "messages", Percent: 90, Direction: "over"},
	}})

	if got.StorageWarnPercent != 80 || got.MessagesWarnPercent != 90 {
		t.Errorf("warnings %+v", got)
	}
}
