package jmap

import (
	"testing"

	"shardpost/internal/model"
)

// withTree prepares a deeper tree:
//
//	Alpha, Alpha/Zed, Alpha/Bee, Beta, Beta/Mid, Beta/Mid/Leaf, INBOX
func withTree(u model.User) {
	for _, n := range []string{inboxName, "Alpha", "Alpha/Zed", "Alpha/Bee", "Beta", "Beta/Mid", "Beta/Mid/Leaf"} {
		if err := u.CreateMailbox(n, nil); err != nil {
			panic(err)
		}
	}
}

// treeNames runs a Mailbox/query and names the result in order.
func treeNames(t *testing.T, s *Server, args string) []string {
	t.Helper()

	names := map[string]string{}
	for name, obj := range objects(t, mailCall(t, s, "Mailbox/get", `{"accountId":"test","properties":["name"]}`)) {
		names[obj[propID].(string)] = name
	}

	res := mailCall(t, s, "Mailbox/query", args)
	list, _ := res["ids"].([]any)

	out := make([]string, 0, len(list))
	for _, id := range list {
		out = append(out, names[id.(string)])
	}

	return out
}

// TestMailboxQuerySortAsTree: a parent comes before its children, and
// siblings are in comparator order at every level (RFC 8621 2.3).
func TestMailboxQuerySortAsTree(t *testing.T) {
	s := mailServer(t, withTree)

	sameIDs(t, "flat by name", treeNames(t, s, `{"accountId":"test","sort":[{"property":"name"}]}`),
		"Alpha", "Bee", "Beta", "INBOX", "Leaf", "Mid", "Zed")
	sameIDs(t, "as a tree", treeNames(t, s, `{"accountId":"test","sort":[{"property":"name"}],"sortAsTree":true}`),
		"Alpha", "Bee", "Zed", "Beta", "Mid", "Leaf", "INBOX")
	sameIDs(t, "as a tree, descending",
		treeNames(t, s, `{"accountId":"test","sort":[{"property":"name","isAscending":false}],"sortAsTree":true}`),
		"INBOX", "Beta", "Mid", "Leaf", "Alpha", "Zed", "Bee")
	// The ancestors that decide the order need not be in the result.
	sameIDs(t, "a filtered tree",
		treeNames(t, s, `{"accountId":"test","filter":{"name":"e"},"sort":[{"property":"name","isAscending":false}],"sortAsTree":true}`),
		"Beta", "Leaf", "Zed", "Bee")
}

// TestMailboxQueryFilterAsTree: a mailbox is in the result only when its
// whole parent chain matches as well.
func TestMailboxQueryFilterAsTree(t *testing.T) {
	s := mailServer(t, withTree)

	sameIDs(t, "flat", treeNames(t, s, `{"accountId":"test","filter":{"name":"e"},"sort":[{"property":"name"}]}`),
		"Bee", "Beta", "Leaf", "Zed")
	sameIDs(t, "as a tree",
		treeNames(t, s, `{"accountId":"test","filter":{"name":"e"},"sort":[{"property":"name"}],"filterAsTree":true}`),
		"Beta")
	sameIDs(t, "both",
		treeNames(t, s, `{"accountId":"test","filter":{"operator":"NOT","conditions":[{"name":"mid"}]},`+
			`"sort":[{"property":"name"}],"filterAsTree":true,"sortAsTree":true}`),
		"Alpha", "Bee", "Zed", "Beta", "INBOX")
}
