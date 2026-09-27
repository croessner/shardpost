package main

import (
	"testing"

	"shardpost/internal/config"
)

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
