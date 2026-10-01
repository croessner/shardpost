package jmap

import (
	"reflect"
	"strings"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// A filter is compiled once per query into a tree of predicates. The
// text conditions are resolved against the full-text index at compile
// time — one index lookup per condition, wherever it sits in the tree —
// so that evaluating a message is set membership, not a search.

// cnode is a compiled filter node: an operator over children, or a
// condition whose predicates must all hold.
type cnode struct {
	op       string
	children []*cnode
	preds    []func(*emailItem) bool
	// inMailbox and exact are what candidates() looks at on the top node:
	// a mailbox the node is limited to, and the index hits of a text
	// condition the index answers exactly.
	inMailbox string
	exact     map[string]bool
	hasExact  bool
}

// eval evaluates the node against one message.
func (n *cnode) eval(it *emailItem) bool {
	if n == nil {
		return true
	}

	if n.op != "" {
		return n.evalOp(it)
	}

	for _, p := range n.preds {
		if !p(it) {
			return false
		}
	}

	return true
}

// evalOp evaluates an operator node: AND needs every child, OR one, NOT
// none.
func (n *cnode) evalOp(it *emailItem) bool {
	want := n.op != opAND // the child result that decides early

	for _, c := range n.children {
		if c.eval(it) == want {
			return n.op == opOR
		}
	}

	return n.op != opOR
}

// conjuncts are the condition nodes that must all hold for the node to
// hold: the node itself, or the children of a top-level AND.
func (n *cnode) conjuncts() []*cnode {
	switch {
	case n == nil:
		return nil
	case n.op == opAND:
		var out []*cnode
		for _, c := range n.children {
			out = append(out, c.conjuncts()...)
		}

		return out
	case n.op == "":
		return []*cnode{n}
	}

	return nil
}

// mailbox is a mailbox every match has to be in, if the filter names one
// at the top.
func (n *cnode) mailbox() (string, bool) {
	for _, c := range n.conjuncts() {
		if c.inMailbox != "" {
			return c.inMailbox, true
		}
	}

	return "", false
}

// textCandidates are the index hits of a text condition every match has
// to satisfy and the index answers without a check afterwards.
func (n *cnode) textCandidates() (map[string]bool, bool) {
	for _, c := range n.conjuncts() {
		if c.hasExact {
			return c.exact, true
		}
	}

	return nil, false
}

// compile turns a filter into predicates.
func (q *emailQuery) compile(f *emailFilter) (*cnode, *methodError) {
	if f == nil {
		return nil, nil
	}

	if mixed(f) {
		return nil, &methodError{Type: errUnsupportedFilter,
			Description: "a filter node is an operator with conditions or a condition, not both"}
	}

	if f.Operator != "" {
		return q.compileOperator(f)
	}

	n := &cnode{}

	for _, step := range []func(*emailFilter, *cnode) *methodError{
		q.compilePlace, q.compileDates, q.compileKeywords, q.compileText, q.compileHeader,
	} {
		if merr := step(f, n); merr != nil {
			return nil, merr
		}
	}

	if f.HasAttachment != nil {
		// Answering it means parsing every candidate message; the index
		// record does not carry it yet. Refused rather than made slow.
		return nil, &methodError{Type: errUnsupportedFilter, Description: "hasAttachment is not supported in a query"}
	}

	return n, nil
}

// mixed reports whether a node is both an operator and a condition, or
// has conditions without an operator (RFC 8620 5.5). Either half would be
// ignored, and an ignored condition matches everything.
func mixed(f *emailFilter) bool {
	rest := *f
	rest.Operator, rest.Conditions = "", nil

	if f.Operator == "" {
		return f.Conditions != nil
	}

	return !reflect.DeepEqual(rest, emailFilter{})
}

// compileOperator compiles an AND, OR or NOT node.
func (q *emailQuery) compileOperator(f *emailFilter) (*cnode, *methodError) {
	switch f.Operator {
	case opAND, opOR, opNOT:
	default:
		return nil, &methodError{Type: errUnsupportedFilter, Description: "unknown operator " + f.Operator}
	}

	n := &cnode{op: f.Operator}

	for i := range f.Conditions {
		c, merr := q.compile(&f.Conditions[i])
		if merr != nil {
			return nil, merr
		}

		if c != nil {
			n.children = append(n.children, c)
		}
	}

	return n, nil
}

// compilePlace compiles the mailbox and size conditions.
func (q *emailQuery) compilePlace(f *emailFilter, n *cnode) *methodError {
	if f.InMailbox != nil {
		box := *f.InMailbox
		n.inMailbox = box
		n.preds = append(n.preds, func(it *emailItem) bool { return it.boxes[box] })
	}

	if f.InMailboxOtherThan != nil {
		not := map[string]bool{}
		for _, b := range f.InMailboxOtherThan {
			not[b] = true
		}

		n.preds = append(n.preds, func(it *emailItem) bool {
			for b := range it.boxes {
				if !not[b] {
					return true
				}
			}

			return false
		})
	}

	if f.MinSize != nil {
		minSize := *f.MinSize

		n.preds = append(n.preds, func(it *emailItem) bool { return it.meta.Size >= minSize })
	}

	if f.MaxSize != nil {
		maxSize := *f.MaxSize

		n.preds = append(n.preds, func(it *emailItem) bool { return it.meta.Size < maxSize })
	}

	return nil
}

// compileDates compiles before and after, which compare receivedAt:
// before is strictly earlier, after is at or later (RFC 8621 4.4.1).
func (q *emailQuery) compileDates(f *emailFilter, n *cnode) *methodError {
	if f.Before != nil {
		t, ok := parseUTCDate(*f.Before)
		if !ok {
			return &methodError{Type: errUnsupportedFilter, Description: "before is not a UTCDate"}
		}

		n.preds = append(n.preds, func(it *emailItem) bool { return it.meta.InternalDate.Before(t) })
	}

	if f.After != nil {
		t, ok := parseUTCDate(*f.After)
		if !ok {
			return &methodError{Type: errUnsupportedFilter, Description: "after is not a UTCDate"}
		}

		n.preds = append(n.preds, func(it *emailItem) bool { return !it.meta.InternalDate.Before(t) })
	}

	return nil
}

// compileKeywords compiles the keyword conditions, of the message and of
// its thread.
func (q *emailQuery) compileKeywords(f *emailFilter, n *cnode) *methodError {
	conds := []struct {
		v  *string
		fn func(*emailItem, string) bool
	}{
		{f.HasKeyword, func(it *emailItem, k string) bool { return it.kw()[k] }},
		{f.NotKeyword, func(it *emailItem, k string) bool { return !it.kw()[k] }},
		{f.AllInThreadHaveKeyword, func(it *emailItem, k string) bool { return allHave(q.threadKeywords(it.thread), k) }},
		{f.SomeInThreadHaveKeyword, func(it *emailItem, k string) bool { return someHave(q.threadKeywords(it.thread), k) }},
		{f.NoneInThreadHaveKeyword, func(it *emailItem, k string) bool { return !someHave(q.threadKeywords(it.thread), k) }},
	}

	for _, c := range conds {
		if c.v == nil {
			continue
		}

		k := strings.ToLower(*c.v)
		if !validKeyword(k) {
			return &methodError{Type: errUnsupportedFilter, Description: "not a keyword: " + *c.v}
		}

		if c.v != f.HasKeyword && c.v != f.NotKeyword {
			q.threadDep = true
		}

		fn := c.fn

		n.preds = append(n.preds, func(it *emailItem) bool { return fn(it, k) })
	}

	return nil
}

// textCond is a text condition and the index fields it searches.
type textCond struct {
	v      *string
	fields []string
	verify func(*model.Meta, string) bool // nil: the index answer is exact
}

// compileText compiles the text conditions against the full-text index.
// The index answers text, subject and body alone, by word (RFC 8621
// leaves the matching to the server). From, To, Cc and Bcc share
// index fields, so for them the index only narrows the candidates and
// the address itself is checked.
func (q *emailQuery) compileText(f *emailFilter, n *cnode) *methodError {
	conds := []textCond{
		{f.Text, []string{fts.FieldSubject, fts.FieldFrom, fts.FieldTo, fts.FieldBody}, nil},
		{f.Subject, []string{fts.FieldSubject}, nil},
		{f.Body, []string{fts.FieldBody}, nil},
		{f.From, []string{fts.FieldFrom}, addrCheck(propFrom)},
		{f.To, []string{fts.FieldTo}, addrCheck(propTo)},
		{f.Cc, []string{fts.FieldTo}, addrCheck(propCc)},
		{f.Bcc, []string{fts.FieldTo}, addrCheck(propBcc)},
	}

	for _, c := range conds {
		if c.v == nil {
			continue
		}

		if merr := q.compileOneText(c, n); merr != nil {
			return merr
		}
	}

	return nil
}

// compileOneText compiles one text condition.
func (q *emailQuery) compileOneText(c textCond, n *cnode) *methodError {
	value := strings.ToLower(*c.v)

	if tokenless(value) {
		// Nothing the index can look for: an address condition checks
		// the address, and anything else matches, as an empty search
		// does.
		if c.verify != nil {
			verify := c.verify

			n.preds = append(n.preds, func(it *emailItem) bool { return verify(it.meta, value) })
		}

		return nil
	}

	hits, err := q.ftsSearch(c.fields, value)
	if err != nil {
		return &methodError{Type: errServerFail, Description: err.Error()}
	}

	if c.verify == nil && !n.hasExact {
		n.exact, n.hasExact = hits, true
	}

	verify := c.verify

	n.preds = append(n.preds, func(it *emailItem) bool {
		return hits[it.guid] && (verify == nil || verify(it.meta, value))
	})

	return nil
}

// addrCheck is the check of an address condition against the envelope:
// every word of the value is looked for, without regard to case, in
// "name <address>" of one address. The index narrowed by the same words,
// so the two agree on what a match is.
func addrCheck(prop string) func(*model.Meta, string) bool {
	return func(m *model.Meta, v string) bool {
		if m.Envelope == nil {
			return false
		}

		list := m.Envelope.From

		switch prop {
		case propTo:
			list = m.Envelope.To
		case propCc:
			list = m.Envelope.Cc
		case propBcc:
			list = m.Envelope.Bcc
		}

		for _, a := range list {
			if a.Host == "" {
				continue
			}

			if containsAll(strings.ToLower(a.Name+" <"+a.Mailbox+"@"+a.Host+">"), v) {
				return true
			}
		}

		return false
	}
}

// containsAll reports whether s contains every word of v.
func containsAll(s, v string) bool {
	for _, w := range strings.Fields(v) {
		if !strings.Contains(s, w) {
			return false
		}
	}

	return true
}

// compileHeader compiles the header condition: a header's presence, or a
// value in it. The index narrows the candidates for a value; the header
// itself decides, which means reading it.
func (q *emailQuery) compileHeader(f *emailFilter, n *cnode) *methodError {
	if f.Header == nil {
		return nil
	}

	if len(f.Header) < 1 || len(f.Header) > 2 {
		return &methodError{Type: errUnsupportedFilter, Description: "header takes a name and an optional value"}
	}

	name := f.Header[0]
	value := ""

	var hits map[string]bool

	if len(f.Header) == 2 {
		value = strings.ToLower(f.Header[1])

		if !tokenless(value) {
			var err error
			if hits, err = q.ftsSearch([]string{fts.FieldHeader}, value); err != nil {
				return &methodError{Type: errServerFail, Description: err.Error()}
			}
		}
	}

	n.preds = append(n.preds, func(it *emailItem) bool {
		if hits != nil && !hits[it.guid] {
			return false
		}

		return q.headerHas(it.meta, name, value)
	})

	return nil
}

// headerHas reads a message's header and reports whether it has the
// field, containing the value when one is given.
func (q *emailQuery) headerHas(m *model.Meta, name, value string) bool {
	h, err := readMessageHeader(q.sess, m)
	if err != nil {
		return false
	}

	for _, v := range h.Values(name) {
		// textForm decodes as the index does, raw 8-bit text included
		if value == "" || strings.Contains(strings.ToLower(textForm(v)), value) {
			return true
		}
	}

	return false
}
