package imapproto

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"shardpost/internal/model"
)

// NOTIFY (RFC 5465). The client registers event groups; a notifier
// goroutine watches the mailboxes and pushes untagged responses between
// commands (and during IDLE): for the selected mailbox the same
// EXISTS/EXPUNGE/FETCH updates a poll produces, optionally followed by a
// FETCH with the requested attributes for new messages; for other
// mailboxes a STATUS line when MESSAGES, UIDNEXT or HIGHESTMODSEQ changed.
//
// Supported events: MessageNew, MessageExpunge, FlagChange. MailboxName,
// SubscriptionChange and AnnotationChange are refused with BADEVENT.
// Filters address the account's own namespace; the number of watched
// mailboxes is bounded (notifyMaxMailboxes).

const notifyMaxMailboxes = 64

// notifyBadEvent is the response code for an event this server does not
// report; it lists the ones it does (RFC 5465 5).
const notifyBadEvent = "BADEVENT (MessageNew MessageExpunge FlagChange)"

type notifyState struct {
	selected      bool // SELECTED or SELECTED-DELAYED group present
	selectedNone  bool
	delayed       bool
	selectedAtts  string          // raw fetch-atts for MessageNew, "" = none
	flagChange    bool            // any group asked for FlagChange (STATUS carries HIGHESTMODSEQ)
	boxes         map[string]bool // own mailbox names with message events
	events        chan string     // mailbox name ("" = selected) that changed
	stop          chan struct{}
	wg            sync.WaitGroup
	mu            sync.Mutex
	watches       map[string]model.Watch
	selectedWatch model.Watch
	last          map[string]string // last STATUS line per mailbox
}

type notifyGroup struct {
	filter    string
	mailboxes []string
	none      bool
	events    map[string]bool
	fetchAtts string
}

// NOTIFY SET [STATUS] group+ | NOTIFY NONE
func (c *conn) cmdNotify(tag string) error {
	kw, err := c.r.atom()
	if err != nil {
		return err
	}

	switch strings.ToUpper(kw) {
	case "NONE":
		c.notifyStop()
		c.ok(tag, "", "NOTIFY completed")

		return nil
	case "SET":
	default:
		return fmt.Errorf("%w: expected SET or NONE", errSyntax)
	}

	sendStatus, groups, err := c.readNotifySet()
	if err != nil {
		return err
	}

	if err := c.notifyNames(groups); err != nil {
		return err
	}

	st, ok := c.buildNotifyState(tag, groups)
	if !ok {
		return nil
	}

	if len(st.boxes) > notifyMaxMailboxes {
		c.no(tag, "LIMIT", fmt.Sprintf("At most %d mailboxes can be watched", notifyMaxMailboxes))
		return nil
	}

	c.notifyStop()

	c.notify = st
	c.startNotifyWatches(st, sendStatus)
	c.notifyReselect()

	st.wg.Add(1)
	go c.notifyLoop(st)

	c.ok(tag, "", "NOTIFY completed")

	return nil
}

// notifyNames converts the mailboxes of SUBTREE and MAILBOXES from the
// session's wire form; a name not in NFC also names the NFC mailbox
// (see target).
func (c *conn) notifyNames(groups []notifyGroup) error {
	for i := range groups {
		for j := range groups[i].mailboxes {
			name, err := c.fromWire(groups[i].mailboxes[j])
			if err != nil {
				return err
			}

			groups[i].mailboxes[j] = name
		}

		for _, n := range groups[i].mailboxes {
			if nfc, changed := nfcName(n); changed && !slices.Contains(groups[i].mailboxes, nfc) {
				groups[i].mailboxes = append(groups[i].mailboxes, nfc)
			}
		}
	}

	return nil
}

// readNotifySet reads the arguments of NOTIFY SET: the optional STATUS
// keyword and the event groups.
func (c *conn) readNotifySet() (sendStatus bool, groups []notifyGroup, err error) {
	c.r.skipSpaces()

	if c.r.peek() != '(' {
		a, err := c.r.atom()
		if err != nil || !strings.EqualFold(a, kwStatus) {
			return false, nil, fmt.Errorf("%w: expected STATUS or event group", errSyntax)
		}

		sendStatus = true
	}

	for !c.r.eol() {
		raw, err := c.r.balanced()
		if err != nil {
			return false, nil, err
		}

		g, err := parseNotifyGroup(raw)
		if err != nil {
			return false, nil, err
		}

		groups = append(groups, *g)
	}

	if len(groups) == 0 {
		return false, nil, fmt.Errorf("%w: NOTIFY SET needs an event group", errSyntax)
	}

	return sendStatus, groups, nil
}

// buildNotifyState validates the event groups and collects them into a
// new notifier state; ok is false when the client was answered.
func (c *conn) buildNotifyState(tag string, groups []notifyGroup) (*notifyState, bool) {
	st := &notifyState{boxes: map[string]bool{}, events: make(chan string, 64), stop: make(chan struct{}),
		watches: map[string]model.Watch{}, last: map[string]string{}}
	seenSelected := false

	for _, g := range groups {
		if !c.checkNotifyGroup(tag, g) {
			return nil, false
		}

		if g.events["FLAGCHANGE"] {
			st.flagChange = true
		}

		switch g.filter {
		case "SELECTED", kwSelectedDelay:
			if seenSelected {
				c.bad(tag, "Only one SELECTED group allowed")
				return nil, false
			}

			seenSelected = true
			st.selected, st.selectedNone = true, g.none
			st.delayed = g.filter == kwSelectedDelay
			st.selectedAtts = g.fetchAtts
		default:
			if g.none {
				continue
			}

			names, err := c.notifyMailboxes(g)
			if err != nil {
				c.storeErr(tag, err)
				return nil, false
			}

			for _, n := range names {
				st.boxes[n] = true
			}
		}
	}

	return st, true
}

// checkNotifyGroup answers unsupported or inconsistent events of a group
// and returns false then.
func (c *conn) checkNotifyGroup(tag string, g notifyGroup) bool {
	for ev := range g.events {
		switch ev {
		case kwMessageNew, "MESSAGEEXPUNGE", "FLAGCHANGE":
		case "MAILBOXNAME", "SUBSCRIPTIONCHANGE", "ANNOTATIONCHANGE", "MAILBOXMETADATACHANGE", "SERVERMETADATACHANGE":
			c.no(tag, notifyBadEvent, "Event "+ev+" is not supported")
			return false
		default:
			c.bad(tag, "Unknown event "+ev)
			return false
		}
	}

	if !g.none && (g.events[kwMessageNew] != g.events["MESSAGEEXPUNGE"]) {
		c.bad(tag, "MessageNew and MessageExpunge must be specified together")
		return false
	}

	if !g.none && g.events["FLAGCHANGE"] && !g.events[kwMessageNew] {
		c.bad(tag, "FlagChange requires MessageNew and MessageExpunge")
		return false
	}

	return true
}

// startNotifyWatches arms the watches of the notified mailboxes and, with
// sendStatus, reports their initial STATUS (except the selected one).
func (c *conn) startNotifyWatches(st *notifyState, sendStatus bool) {
	for name := range st.boxes {
		mb, err := c.user.Lookup(name)
		if err != nil {
			continue
		}

		w, err := c.user.Watch(mb.ID)
		if err != nil {
			c.log.Warn("notify watch failed", "mailbox", name, "err", err)
			continue
		}

		st.watches[name] = w
		st.forward(w, name)

		if sendStatus && !c.isSelectedOwn(name) {
			if line, err := c.statusLine(&target{u: c.user, name: name, full: name}, c.notifyStatusItems()); err == nil {
				st.last[name] = line
				c.w.untagged("%s", line)
			}
		}
	}
}

// isSelectedOwn reports whether name is the selected mailbox of the
// account's own namespace.
func (c *conn) isSelectedOwn(name string) bool {
	return c.mbox != nil && c.mu == c.user && c.mbox.Name == name
}

func (c *conn) notifyStatusItems() []string {
	items := []string{kwMessages, kwUIDNext, kwUIDValidity}
	if c.condstore || (c.notify != nil && c.notify.flagChange) {
		items = append(items, "HIGHESTMODSEQ")
	}

	return items
}

// notifyMailboxes resolves a non-selected filter to own mailbox names.
func (c *conn) notifyMailboxes(g notifyGroup) ([]string, error) {
	boxes, err := c.user.ListMailboxes()
	if err != nil {
		return nil, err
	}

	var out []string

	switch g.filter {
	case "INBOXES":
		out = append(out, "INBOX")
	case "PERSONAL":
		for _, b := range boxes {
			out = append(out, b.Name)
		}
	case kwSubscribed:
		out = subscribedNames(boxes)
	case kwSubtree:
		out = subtreeNames(boxes, g.mailboxes)
	case kwMailboxes:
		out = namedBoxes(boxes, g.mailboxes)
	default:
		return nil, fmt.Errorf("%w: unknown filter %s", errSyntax, g.filter)
	}

	return out, nil
}

// subscribedNames returns the names of the subscribed boxes.
func subscribedNames(boxes []model.Mailbox) []string {
	var out []string

	for _, b := range boxes {
		if b.Subscribed {
			out = append(out, b.Name)
		}
	}

	return out
}

// subtreeNames returns the names of the boxes at or below each root.
func subtreeNames(boxes []model.Mailbox, roots []string) []string {
	var out []string

	for _, root := range roots {
		for _, b := range boxes {
			if b.Name == root || strings.HasPrefix(b.Name, root+"/") {
				out = append(out, b.Name)
			}
		}
	}

	return out
}

// namedBoxes returns the given names that exist among boxes.
func namedBoxes(boxes []model.Mailbox, names []string) []string {
	var out []string

	for _, n := range names {
		for _, b := range boxes {
			if b.Name == n {
				out = append(out, b.Name)
			}
		}
	}

	return out
}

// parseNotifyGroup parses "(filter [mailboxes] (events)|NONE)".
func parseNotifyGroup(raw string) (*notifyGroup, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "(") || !strings.HasSuffix(raw, ")") {
		return nil, fmt.Errorf("%w: expected event group", errSyntax)
	}

	toks := splitGroup(raw[1 : len(raw)-1])
	if len(toks) < 2 {
		return nil, fmt.Errorf("%w: incomplete event group", errSyntax)
	}

	g := &notifyGroup{filter: strings.ToUpper(toks[0]), events: map[string]bool{}}

	i := 1
	if g.filter == kwSubtree || g.filter == kwMailboxes {
		if i >= len(toks) {
			return nil, fmt.Errorf("%w: %s needs mailboxes", errSyntax, g.filter)
		}

		g.mailboxes = parseNotifyMailboxes(toks[i])
		i++
	}

	if i >= len(toks) {
		return nil, fmt.Errorf("%w: event group without events", errSyntax)
	}

	ev := toks[i]
	if strings.EqualFold(ev, "NONE") {
		g.none = true
		return g, nil
	}

	if !strings.HasPrefix(ev, "(") {
		return nil, fmt.Errorf("%w: expected event list", errSyntax)
	}

	g.parseEvents(splitGroup(ev[1 : len(ev)-1]))

	return g, nil
}

// parseNotifyMailboxes parses the one-or-list mailbox argument of the
// SUBTREE and MAILBOXES filters.
func parseNotifyMailboxes(tok string) []string {
	if !strings.HasPrefix(tok, "(") {
		return []string{unquote(tok)}
	}

	var out []string
	for _, m := range splitGroup(tok[1 : len(tok)-1]) {
		out = append(out, unquote(m))
	}

	return out
}

// parseEvents records the event names of a group; the fetch attributes
// following MessageNew are kept raw.
func (g *notifyGroup) parseEvents(items []string) {
	for j := 0; j < len(items); j++ {
		name := strings.ToUpper(items[j])

		g.events[name] = true
		if name == kwMessageNew && j+1 < len(items) && strings.HasPrefix(items[j+1], "(") {
			g.fetchAtts = items[j+1]
			j++
		}
	}
}

// splitGroup splits on spaces outside parentheses, brackets and quotes.
func splitGroup(s string) []string {
	var out []string

	depth := 0
	quoted := false
	start := -1

	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case quoted:
			i, quoted = quotedStep(s, i)
		case ch == '"':
			quoted = true
		case ch == ' ' && depth == 0:
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}

			continue
		default:
			depth += nesting(ch)
		}

		if start < 0 {
			start = i
		}
	}

	if start >= 0 {
		out = append(out, s[start:])
	}

	return out
}

// quotedStep advances over the character at i inside a quoted string (a
// backslash escapes the next one) and returns the index of the last
// character consumed and whether the string is still open.
func quotedStep(s string, i int) (int, bool) {
	switch s[i] {
	case '\\':
		return i + 1, true
	case '"':
		return i, false
	}

	return i, true
}

// nesting is the depth change of an opening or closing parenthesis or
// bracket, 0 for any other character.
func nesting(ch byte) int {
	switch ch {
	case '(', '[':
		return 1
	case ')', ']':
		return -1
	}

	return 0
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(strings.ReplaceAll(s[1:len(s)-1], `\"`, `"`), `\\`, `\`)
	}

	return s
}

// forward pumps a watch into the notifier channel until stop.
func (st *notifyState) forward(w model.Watch, name string) {
	st.wg.Add(1)
	go func() {
		defer st.wg.Done()

		for {
			select {
			case <-st.stop:
				return
			case <-w.Events():
				select {
				case st.events <- name:
				default: // coalesce
				}
			case <-w.Errors():
				return
			}
		}
	}()
}

// notifyReselect (re)arms the watch on the selected mailbox; called on
// SELECT and UNSELECT.
func (c *conn) notifyReselect() {
	st := c.notify
	if st == nil {
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	if st.selectedWatch != nil {
		st.selectedWatch.Close()
		st.selectedWatch = nil
	}

	if !st.selected || st.selectedNone || c.mbox == nil {
		return
	}

	w, err := c.mu.Watch(c.mbox.ID)
	if err != nil {
		c.log.Warn("notify watch failed", "mailbox", c.mbox.Name, "err", err)
		return
	}

	st.selectedWatch = w
	st.forward(w, "")
}

func (c *conn) notifyStop() {
	st := c.notify
	if st == nil {
		return
	}

	c.notify = nil

	close(st.stop)
	st.mu.Lock()
	for _, w := range st.watches {
		w.Close()
	}

	if st.selectedWatch != nil {
		st.selectedWatch.Close()
	}
	st.mu.Unlock()
}

// notifyLoop delivers events. It takes the command mutex so that its
// output never interleaves with a command's responses; the command loop
// releases the mutex while waiting for the next command and during IDLE.
func (c *conn) notifyLoop(st *notifyState) {
	defer st.wg.Done()

	for {
		select {
		case <-st.stop:
			return
		case name := <-st.events:
			c.cmdMu.Lock()
			if c.notify != st {
				c.cmdMu.Unlock()
				return
			}

			if name == "" {
				c.notifySelected(st)
			} else {
				c.notifyStatus(st, name)
			}

			err := c.w.flush()
			c.cmdMu.Unlock()

			if err != nil {
				return
			}
		}
	}
}

func (c *conn) notifySelected(st *notifyState) {
	if c.mbox == nil {
		return
	}
	// poll announces new messages and, with NOTIFY fetch attributes
	// active, their FETCH (see notifyNewMessages), whoever runs it.
	_ = c.poll(!st.delayed)
}

// notifyNewMessages is called by poll (under cmdMu) with the sequence
// numbers of messages that just appeared, so the fetch attributes the
// client ordered with MessageNew are sent regardless of whether the
// notifier, IDLE or a NOOP poll noticed the message first.
func (c *conn) notifyNewMessages(seqs []uint32) {
	st := c.notify
	if st == nil || !st.selected || st.selectedNone || st.selectedAtts == "" || len(seqs) == 0 {
		return
	}

	c.fetchAttributes(seqs, st.selectedAtts)
}

// fetchAttributes runs the FETCH machinery for the given sequence numbers
// with a raw item list (NOTIFY MessageNew fetch-atts).
func (c *conn) fetchAttributes(seqs []uint32, atts string) {
	r := newReader(strings.NewReader(atts+"\r\n"), c.s.o.MaxLiteral)
	if err := r.readLine(); err != nil {
		return
	}

	opts, err := parseFetchItemsFrom(r)
	if err != nil {
		return
	}

	for _, seq := range seqs {
		if int(seq) > len(c.msgs) {
			continue
		}

		// Unsolicited: with the UID for IMAP4rev2 (RFC 9051 appendix E 21).
		m := c.msgs[seq-1]
		_ = c.writeFetch(int(seq), &m, opts, c.rev2)
	}
}

func (c *conn) notifyStatus(st *notifyState, name string) {
	if c.isSelectedOwn(name) {
		return // the selected mailbox is reported through its own events
	}

	line, err := c.statusLine(&target{u: c.user, name: name, full: name}, c.notifyStatusItems())
	if err != nil {
		return
	}

	if st.last[name] == line {
		return
	}

	st.last[name] = line
	c.w.untagged("%s", line)
}

// notifyWait bounds how long a notifier burst may hold the writer.
var _ = time.Second
