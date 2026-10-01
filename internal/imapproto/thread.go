package imapproto

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"

	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
)

// --- SORT (RFC 5256) --------------------------------------------------------

// cmdSort: SORT (criteria) charset search-keys. Everything it needs comes
// from the cached envelope in the index record, so no blob is read.
func (c *conn) cmdSort(tag string, uid bool) error {
	t, err := c.r.next()
	if err != nil {
		return err
	}

	if t.kind != tokList || len(t.list) == 0 {
		return fmt.Errorf("%w: SORT needs a criteria list", errSyntax)
	}

	var keys []string
	for _, k := range t.list {
		keys = append(keys, strings.ToUpper(k.str))
	}

	if _, err := c.r.astring(); err != nil { // charset
		return err
	}

	cr, err := c.parseSearchKeys(c.r, 0)
	if err != nil {
		return err
	}

	seqs, metas, err := c.runSearch(cr)
	if err != nil {
		return c.searchFailed(tag, err)
	}

	idx := make([]int, len(metas))
	for i := range idx {
		idx[i] = i
	}

	sort.SliceStable(idx, func(a, b int) bool { return sortLess(keys, &metas[idx[a]], &metas[idx[b]]) })

	line := kwSort

	for _, i := range idx {
		if uid {
			line += fmt.Sprintf(" %d", metas[i].UID)
		} else {
			line += fmt.Sprintf(" %d", seqs[i])
		}
	}

	c.w.untagged("%s", line)
	c.ok(tag, "", "SORT completed")

	return nil
}

func sortLess(keys []string, a, b *model.Meta) bool {
	reverse := false

	for _, k := range keys {
		if k == "REVERSE" {
			reverse = true
			continue
		}

		cmp := compareKey(k, a, b)
		if cmp == 0 {
			reverse = false
			continue
		}

		if reverse {
			return cmp > 0
		}

		return cmp < 0
	}

	return a.UID < b.UID
}

func compareKey(k string, a, b *model.Meta) int {
	switch k {
	case "ARRIVAL":
		return cmpTime(a.InternalDate, b.InternalDate)
	case "DATE":
		return cmpTime(envDate(a), envDate(b))
	case kwSize:
		return cmpInt(a.Size, b.Size)
	case kwSubject:
		return strings.Compare(baseSubject(envSubject(a)), baseSubject(envSubject(b)))
	case kwFrom:
		return strings.Compare(firstLocalpart(envAddrs(a, hdrFrom)), firstLocalpart(envAddrs(b, hdrFrom)))
	case kwTo:
		return strings.Compare(firstLocalpart(envAddrs(a, hdrTo)), firstLocalpart(envAddrs(b, hdrTo)))
	case kwCc:
		return strings.Compare(firstLocalpart(envAddrs(a, hdrCc)), firstLocalpart(envAddrs(b, hdrCc)))
	case "DISPLAYFROM":
		return strings.Compare(displayName(envAddrs(a, hdrFrom)), displayName(envAddrs(b, hdrFrom)))
	case "DISPLAYTO":
		return strings.Compare(displayName(envAddrs(a, hdrTo)), displayName(envAddrs(b, hdrTo)))
	}

	return 0
}

func cmpTime(a, b time.Time) int {
	switch {
	case a.Before(b):
		return -1
	case a.After(b):
		return 1
	}

	return 0
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}

func envDate(m *model.Meta) time.Time {
	if m.Envelope != nil && !m.Envelope.Date.IsZero() {
		return m.Envelope.Date
	}

	return m.InternalDate
}

func envSubject(m *model.Meta) string {
	if m.Envelope == nil {
		return ""
	}

	return m.Envelope.Subject
}

func envAddrs(m *model.Meta, which string) []imap.Address {
	if m.Envelope == nil {
		return nil
	}

	switch which {
	case hdrFrom:
		return m.Envelope.From
	case hdrTo:
		return m.Envelope.To
	case hdrCc:
		return m.Envelope.Cc
	}

	return nil
}

func firstMailbox(l []imap.Address) string {
	if len(l) == 0 {
		return ""
	}

	return strings.ToLower(l[0].Mailbox + "@" + l[0].Host)
}

// firstLocalpart is the RFC 5256 sort key for FROM/TO/CC: the addr-mailbox
// (local part) of the first address, compared case-insensitively.
func firstLocalpart(l []imap.Address) string {
	if len(l) == 0 {
		return ""
	}

	return strings.ToLower(l[0].Mailbox)
}

// envMessageID returns the message id without brackets ("" if none).
func envMessageID(m *model.Meta) string {
	if m.Envelope == nil {
		return ""
	}

	if ids := mailmeta.MessageIDs(m.Envelope.MessageID); len(ids) > 0 {
		return ids[0]
	}

	return ""
}

func displayName(l []imap.Address) string {
	if len(l) == 0 {
		return ""
	}

	if l[0].Name != "" {
		return strings.ToLower(l[0].Name)
	}

	return firstMailbox(l)
}

// baseSubject is the RFC 5256 2.1 base subject; see mailmeta.BaseSubject.
func baseSubject(s string) string { return mailmeta.BaseSubject(s) }

// --- THREAD (RFC 5256) ------------------------------------------------------

type threadNode struct {
	num      uint32 // seq or uid, 0 for a missing parent
	children []*threadNode
	date     time.Time
}

// cmdThread: THREAD algorithm charset search-keys.
func (c *conn) cmdThread(tag string, uid bool) error {
	algo, err := c.r.atom()
	if err != nil {
		return err
	}

	if _, err := c.r.astring(); err != nil { // charset
		return err
	}

	cr, err := c.parseSearchKeys(c.r, 0)
	if err != nil {
		return err
	}

	seqs, metas, err := c.runSearch(cr)
	if err != nil {
		return c.searchFailed(tag, err)
	}

	num := func(i int) uint32 {
		if uid {
			return metas[i].UID
		}

		return uint32(seqs[i])
	}

	var roots []*threadNode

	switch strings.ToUpper(algo) {
	case "REFERENCES":
		roots = threadReferences(metas, num)
	case "ORDEREDSUBJECT":
		roots = threadOrderedSubject(metas, num)
	default:
		c.bad(tag, "Unknown THREAD algorithm")
		return nil
	}

	var sb strings.Builder
	sb.WriteString("THREAD")

	for i, r := range roots {
		if i == 0 {
			sb.WriteByte(' ')
		}

		writeThread(&sb, r)
	}

	c.w.untagged("%s", sb.String())
	c.ok(tag, "", "THREAD completed")

	return nil
}

// writeThread renders RFC 5256 thread-list syntax: members separated by
// spaces, nested sibling lists adjacent without spaces.
func writeThread(sb *strings.Builder, n *threadNode) {
	sb.WriteByte('(')

	cur := n
	first := true

	for {
		if cur.num != 0 {
			if !first {
				sb.WriteByte(' ')
			}

			fmt.Fprintf(sb, "%d", cur.num)

			first = false
		}

		if len(cur.children) == 1 {
			cur = cur.children[0]
			continue
		}

		if len(cur.children) > 1 && !first {
			sb.WriteByte(' ')
		}

		for _, ch := range cur.children {
			writeThread(sb, ch)
		}

		break
	}

	sb.WriteByte(')')
}

// threadReferences links messages by Message-ID / References (JWZ,
// simplified: parent = last referenced id that we know, else the root of
// the chain becomes a dummy container).
func threadReferences(metas []model.Meta, num func(int) uint32) []*threadNode {
	// (1) containers by Message-ID; duplicates get a private id
	byID, nodes, subjects := threadContainers(metas, num)
	parentOf := linkReferences(metas, nodes, byID)

	all := map[*threadNode]bool{}
	for _, n := range byID {
		all[n] = true
	}

	var roots []*threadNode

	for n := range all {
		if p := parentOf[n]; p != nil {
			p.children = append(p.children, n)
		} else {
			roots = append(roots, n)
		}
	}
	// (3) prune empty containers
	var out []*threadNode
	for _, r := range roots {
		out = append(out, pruneThread(r, true)...)
	}
	// (4)+(5) group the root set by base subject
	subjectOf := make(map[*threadNode]string, len(nodes))
	for i, n := range nodes {
		subjectOf[n] = subjects[i]
	}

	out = groupBySubject(out, subjectOf)
	// (6) sort all levels by date
	sortThreads(out)

	return out
}

// threadContainers creates the container of every message, indexed by
// Message-ID (a missing or duplicate id gets a private one), and returns
// the containers and subjects in message order.
func threadContainers(metas []model.Meta, num func(int) uint32) (map[string]*threadNode, []*threadNode, []string) {
	byID := map[string]*threadNode{}
	nodes := make([]*threadNode, len(metas))

	subjects := make([]string, len(metas))
	for i, m := range metas {
		n := &threadNode{num: num(i), date: envDate(&m)}
		nodes[i] = n
		subjects[i] = envSubject(&m)

		id := envMessageID(&m)
		if id == "" {
			id = fmt.Sprintf("\x00missing-%d", i)
		}

		if _, dup := byID[id]; dup {
			id = fmt.Sprintf("\x00dup-%d", i)
		}

		byID[id] = n
	}

	return byID, nodes, subjects
}

// linkReferences links the containers along each message's References
// chain (creating empty containers for unknown ids in byID) and returns
// the parent of every linked container.
func linkReferences(metas []model.Meta, nodes []*threadNode, byID map[string]*threadNode) map[*threadNode]*threadNode {
	container := func(id string) *threadNode {
		if n, ok := byID[id]; ok {
			return n
		}

		n := &threadNode{}
		byID[id] = n

		return n
	}
	parentOf := map[*threadNode]*threadNode{}

	for i, m := range metas {
		var prev *threadNode

		for _, ref := range m.References {
			n := container(ref)
			if prev != nil && parentOf[n] == nil && n != prev && !isAncestor(n, prev, parentOf) {
				parentOf[n] = prev
			}

			prev = n
		}
		// the message's own container becomes a child of the last reference
		delete(parentOf, nodes[i])

		if prev != nil && prev != nodes[i] && !isAncestor(nodes[i], prev, parentOf) {
			parentOf[nodes[i]] = prev
		}
	}

	return parentOf
}

// pruneThread removes empty containers below n: one without children
// goes, one with children is replaced by them (at the root only if it
// has a single child).
func pruneThread(n *threadNode, root bool) []*threadNode {
	var kids []*threadNode
	for _, ch := range n.children {
		kids = append(kids, pruneThread(ch, false)...)
	}

	n.children = kids
	if n.num == 0 {
		if len(kids) == 0 {
			return nil
		}

		if len(kids) == 1 || !root {
			return kids // promote
		}
	}

	return []*threadNode{n}
}

// sortThreads sorts all levels of the thread forest by date.
func sortThreads(l []*threadNode) {
	sortNodes(l)

	for _, n := range l {
		sortThreads(n.children)
	}
}

// threadSubject returns the base subject of a root container (of its
// first child for an empty one) and whether it counts as a reply.
func threadSubject(n *threadNode, subjectOf map[*threadNode]string) (base string, reply bool) {
	m := n
	if m.num == 0 {
		if len(m.children) == 0 {
			return "", false
		}

		m = m.children[0]
	}

	raw := subjectOf[m]
	b := baseSubject(raw)

	return b, b != strings.ToUpper(strings.Join(strings.Fields(raw), " ")) || n.num == 0
}

// groupBySubject merges root containers with the same base subject
// (RFC 5256 REFERENCES step 5): the non-reply wins the root position,
// replies become its children; two replies (or two non-replies) end up
// under a new empty container.
func groupBySubject(roots []*threadNode, subjectOf map[*threadNode]string) []*threadNode {
	sortNodes(roots)

	table := map[string]*threadNode{}

	var out []*threadNode

	for _, r := range roots {
		b, reply := threadSubject(r, subjectOf)
		if b == "" {
			out = append(out, r)
			continue
		}

		existing, ok := table[b]
		if !ok {
			table[b] = r
			out = append(out, r)

			continue
		}

		_, exReply := threadSubject(existing, subjectOf)
		out = mergeSameSubject(out, table, b, existing, r, exReply, reply)
	}

	return out
}

// mergeSameSubject merges root r into the root existing that has the same
// base subject b, updating table and the root list out.
func mergeSameSubject(out []*threadNode, table map[string]*threadNode, b string, existing, r *threadNode, exReply, reply bool) []*threadNode {
	switch {
	case existing.num == 0 && r.num != 0:
		// keep the empty container as root, r joins it
		existing.children = append(existing.children, r)
	case r.num == 0 && existing.num != 0:
		// r is empty: its children join, or r replaces existing
		r.children = append(r.children, existing)
		table[b] = r
		out = replaceNode(out, existing, r)
	case !exReply && reply:
		existing.children = append(existing.children, r)
	case exReply && !reply:
		r.children = append(r.children, existing)
		table[b] = r
		out = replaceNode(out, existing, r)
	default:
		// both replies or both originals: new empty container
		nc := &threadNode{children: []*threadNode{existing, r}}
		table[b] = nc
		out = replaceNode(out, existing, nc)
	}

	return out
}

func replaceNode(l []*threadNode, old, nw *threadNode) []*threadNode {
	for i, n := range l {
		if n == old {
			l[i] = nw
			return l
		}
	}

	return append(l, nw)
}

func isAncestor(a, n *threadNode, parentOf map[*threadNode]*threadNode) bool {
	for p := parentOf[n]; p != nil; p = parentOf[p] {
		if p == a {
			return true
		}
	}

	return false
}

func nodeDate(n *threadNode) time.Time {
	if n.num != 0 || len(n.children) == 0 {
		return n.date
	}

	return nodeDate(n.children[0])
}

func nodeNum(n *threadNode) uint32 {
	if n.num != 0 || len(n.children) == 0 {
		return n.num
	}

	return nodeNum(n.children[0])
}

func sortNodes(l []*threadNode) {
	sort.SliceStable(l, func(i, j int) bool {
		di, dj := nodeDate(l[i]), nodeDate(l[j])
		if di.Equal(dj) {
			return nodeNum(l[i]) < nodeNum(l[j])
		}

		return di.Before(dj)
	})
}

// threadOrderedSubject groups by base subject, ordered by date.
func threadOrderedSubject(metas []model.Meta, num func(int) uint32) []*threadNode {
	groups := map[string][]*threadNode{}

	var order []string

	for i, m := range metas {
		subj := baseSubject(envSubject(&m))
		if _, ok := groups[subj]; !ok {
			order = append(order, subj)
		}

		groups[subj] = append(groups[subj], &threadNode{num: num(i), date: envDate(&m)})
	}
	// RFC 5256: within a subject group the messages are ordered by date,
	// the earliest is the parent of all the others
	var roots []*threadNode

	for _, s := range order {
		g := groups[s]
		sortNodes(g)
		root := g[0]
		root.children = g[1:]
		roots = append(roots, root)
	}

	sortNodes(roots)

	return roots
}
