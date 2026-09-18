package imapproto

import (
	"strings"
	"testing"
)

func TestShardPostIdentity(t *testing.T) {
	c, _ := newTestServer(t)
	defer func() { _ = c.conn.Close() }()

	lines := strings.Join(c.cmd("ID NIL"), "\n")
	if !strings.Contains(lines, `"name" "ShardPost"`) || !strings.Contains(lines, `"vendor" "ShardPost"`) {
		t.Fatalf("unexpected server ID: %s", lines)
	}

	c.cmd("LOGOUT")
}
