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
// The notifier follows the account object of the account through the
// process-wide watch hub (one watch per account and process, not one per
// mailbox and session) and reads what changed from the account change
// log: the mailboxes the new entries name get their STATUS (one header
// read each), the selected one its poll. When the log no longer covers
// the position the notifier read up to, every notified mailbox is looked
// at. A selected mailbox of another namespace (shared or public) is
// followed through its owner's account object the same way. Where an
// account object cannot be watched, the notifier falls back to watching
// the mailbox objects, as it did before.
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
	selected     bool // SELECTED or SELECTED-DELAYED group present
	selectedNone bool
	delayed      bool
	selectedAtts string          // raw fetch-atts for MessageNew, "" = none
	flagChange   bool            // any group asked for FlagChange (STATUS carries HIGHESTMODSEQ)
	boxes        map[string]bool // own mailbox names with message events
	// ids maps the ids of the notified own mailboxes to their names.
	ids    map[string]string
	events chan string // mailbox name ("" = selected) that changed
	// own follows the account's change log, sel the one of the owner of
	// a selected mailbox in another namespace; ownWake and selWake fire
	// when their account object changed.
	own, sel         *acctFollow
	ownWake, selWake chan struct{}
	stop             chan struct{}
	wg               sync.WaitGroup
	mu               sync.Mutex
	// watches (per notified mailbox) and selectedWatch are the mailbox
	// watches of the fallback without account watch.
	watches       map[string]model.Watch
	selectedWatch model.Watch
	last          map[string]string // last STATUS line per mailbox
}

// acctFollow is a notifier's position in an account change log.
type acctFollow struct {
	w   model.Watch
	seq uint64
}

// notifyChangePage is how many account log entries one read takes.
const notifyChangePage = 256

// changed reads the entries after the position and returns the ids of
// the mailboxes they name; all is true when an entry names none or the
// log no longer covers the position (or cannot be read): then every
// mailbox has to be looked at. The position moves to the last entry read.
func (f *acctFollow) changed(u model.User) (ids map[string]bool, all bool) {
	ids = map[string]bool{}

	for {
		changes, ok, err := u.AccountChanges(f.seq, notifyChangePage)
		if err != nil || !ok {
			if seq, err := u.AccountState(); err == nil {
				f.seq = seq
			}

			return nil, true
		}

		for i := range changes {
			if changes[i].Mailbox == "" {
				all = true
			} else {
				ids[changes[i].Mailbox] = true
			}

			f.seq = changes[i].Seq
		}

		if len(changes) < notifyChangePage {
			return ids, all
		}
	}
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
	st := &notifyState{boxes: map[string]bool{}, ids: map[string]string{}, events: make(chan string, 64), stop: make(chan struct{}),
		ownWake: make(chan struct{}, 1), selWake: make(chan struct{}, 1), watches: map[string]model.Watch{}, last: map[string]string{}}
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
		default:
			// Every other event, a known one or an extension atom, is
			// one this server does not report (RFC 5465 3.1: NO, not BAD).
			c.no(tag, notifyBadEvent, "Event "+ev+" is not supported")
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

// startNotifyWatches follows the account (or, without an account watch,
// the notified mailboxes) and, with sendStatus, reports the initial
// STATUS of the notified mailboxes (except the selected one).
func (c *conn) startNotifyWatches(st *notifyState, sendStatus bool) {
	for name := range st.boxes {
		if mb, err := c.user.Lookup(name); err == nil {
			st.ids[mb.ID] = name
		}
	}

	if st.selected || len(st.ids) > 0 {
		if f := c.followAccount(c.name, c.user); f != nil {
			st.own = f
			st.wake(f.w, st.ownWake)
		} else {
			for id, name := range st.ids {
				if w := c.watchMailbox(c.name, id); w != nil {
					st.watches[name] = w
					st.forward(w, name)
				}
			}
		}
	}

	if !sendStatus {
		return
	}

	for name := range st.boxes {
		if c.isSelectedOwn(name) {
			continue
		}

		if line, err := c.statusLine(&target{u: c.user, name: name, full: name}, c.notifyStatusItems()); err == nil {
			st.last[name] = line
			c.w.untagged("%s", line)
		}
	}
}

// followAccount follows the change log of namespace ns (u is a view of
// it) from its current state on; nil when the account object cannot be
// watched. The watch is armed before the state is read, so no change
// falls between them.
func (c *conn) followAccount(ns string, u model.User) *acctFollow {
	w, err := c.watchAccount(ns)
	if err != nil {
		return nil
	}

	seq, err := u.AccountState()
	if err != nil {
		w.Close()
		return nil
	}

	return &acctFollow{w: w, seq: seq}
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

// wake pumps the events of an account watch into wake (coalesced) until
// stop.
func (st *notifyState) wake(w model.Watch, wake chan<- struct{}) {
	st.wg.Add(1)
	go func() {
		defer st.wg.Done()

		for {
			select {
			case <-st.stop:
				return
			case <-w.Events():
				select {
				case wake <- struct{}{}:
				default: // one pending wake-up reads every change
				}
			case <-w.Errors():
				return
			}
		}
	}()
}

// notifyReselect follows the selected mailbox; called on SELECT and
// UNSELECT. One of the account's own namespace is covered by the
// account's change log; one of another namespace is followed through
// its owner's change log (or, failing that, its mailbox object).
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

	if st.sel != nil {
		st.sel.w.Close()
		st.sel = nil
	}

	if !st.selected || st.selectedNone || c.mbox == nil || (c.mu == c.user && st.own != nil) {
		return
	}

	if c.mu != c.user {
		if f := c.followAccount(c.selectedNS(), c.mu); f != nil {
			st.sel = f
			st.wake(f.w, st.selWake)

			return
		}
	}

	if w := c.watchMailbox(c.selectedNS(), c.mbox.ID); w != nil {
		st.selectedWatch = w
		st.forward(w, "")
	}
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

	for _, f := range []*acctFollow{st.own, st.sel} {
		if f != nil {
			f.w.Close()
		}
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
			if !c.notifyLocked(st, func() { c.notifyName(st, name) }) {
				return
			}
		case <-st.ownWake:
			if !c.notifyLocked(st, func() { c.notifyOwnChanges(st) }) {
				return
			}
		case <-st.selWake:
			if !c.notifyLocked(st, func() { c.notifySelectedChanges(st) }) {
				return
			}
		}
	}
}

// notifyLocked runs fn under the command mutex and flushes; false when
// the notifier is replaced or the connection broke.
func (c *conn) notifyLocked(st *notifyState, fn func()) bool {
	c.cmdMu.Lock()
	defer c.cmdMu.Unlock()

	if c.notify != st {
		return false
	}

	fn()

	return c.w.flush() == nil
}

// notifyName reports a change of a mailbox watched by its object.
func (c *conn) notifyName(st *notifyState, name string) {
	if name == "" {
		c.notifySelected(st)
	} else {
		c.notifyStatus(st, name)
	}
}

// notifyOwnChanges reports the mailboxes the new entries of the account's
// change log name: STATUS for the notified ones, a poll for the selected
// one of the own namespace.
func (c *conn) notifyOwnChanges(st *notifyState) {
	if st.own == nil {
		return
	}

	ids, all := st.own.changed(c.user)
	selectedOwn := st.selected && !st.selectedNone && c.mbox != nil && c.mu == c.user

	if all {
		for name := range st.boxes {
			c.notifyStatus(st, name)
		}

		if selectedOwn {
			c.notifySelected(st)
		}

		return
	}

	for id := range ids {
		if name, ok := st.ids[id]; ok {
			c.notifyStatus(st, name)
		}
	}

	if selectedOwn && ids[c.mbox.ID] {
		c.notifySelected(st)
	}
}

// notifySelectedChanges polls the selected mailbox of another namespace
// when its owner's change log names it.
func (c *conn) notifySelectedChanges(st *notifyState) {
	st.mu.Lock()
	f := st.sel
	st.mu.Unlock()

	if f == nil || c.mbox == nil || c.mu == c.user {
		return
	}

	if ids, all := f.changed(c.mu); all || ids[c.mbox.ID] {
		c.notifySelected(st)
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
