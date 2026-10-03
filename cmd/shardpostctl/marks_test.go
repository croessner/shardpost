package main

import (
	"strings"
	"testing"
	"time"

	"shardpost/internal/jobs"
)

// TestStageMarkCommands: "quota online" and "mailbox lock" have the
// commands of the fts marks: enable needs -pods and the right number,
// status shows the registrations, disable removes the mark; the plain
// diagnosis commands of the nouns still work.
func TestStageMarkCommands(t *testing.T) {
	socket, kv, _ := serveLayout(t)

	if err := jobs.RegisterServer(kv, "pod-a", "0.52.0", time.Now()); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"quota", "online", "enable"}, exUsage, ""},
		{[]string{"quota", "online", "enable", "-pods", "2"}, exDataErr, ""},
		{[]string{"quota", "online", "enable", "-pods", "1"}, exOK, "enabled     true"},
		{[]string{"quota", "online", "status"}, exOK, "server pod-a  0.52.0  fresh"},
		{[]string{"quota", "online", "disable"}, exOK, "enabled  false"},
		{[]string{"mailbox", "lock", "enable", "-pods", "2"}, exDataErr, ""},
		{[]string{"mailbox", "lock", "enable", "-pods", "1", "-json"}, exOK, `"enabled": true`},
		{[]string{"mailbox", "lock", "status"}, exOK, "enabled     true"},
		{[]string{"mailbox", "lock", "disable"}, exOK, "enabled  false"},
		{[]string{"mailbox", "lock", "bogus"}, exUsage, ""},
		{[]string{"quota", "online"}, exUsage, ""},
	} {
		code, out, errOut := call(socket, "", c.args...)
		if code != c.code || !strings.Contains(out, c.out) {
			t.Errorf("%v: %d %q %q", c.args, code, out, errOut)
		}
	}
}
