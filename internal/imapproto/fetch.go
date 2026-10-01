package imapproto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/textproto"

	"shardpost/internal/mailmeta"
	"shardpost/internal/membudget"
	"shardpost/internal/model"
)

// fetchItem is one parsed FETCH data item.
type fetchItem struct {
	name    string // FLAGS, ENVELOPE, ... BODY[] items use "BODY", BINARY items "BINARY"
	section *imap.FetchItemBodySection
	binary  *imap.FetchItemBinarySection
	binSize *imap.FetchItemBinarySectionSize
	raw     string // as written by the client, for the response label
}

type fetchOptions struct {
	items        []fetchItem
	changedSince uint64
	vanished     bool
}

// cmdFetch handles FETCH and UID FETCH, including CONDSTORE's CHANGEDSINCE
// and QRESYNC's VANISHED modifier.
func (c *conn) cmdFetch(tag string, uid bool) error {
	setStr, err := c.r.atom()
	if err != nil {
		return err
	}

	set, err := parseSet(setStr)
	if err != nil {
		return err
	}

	opts, err := c.parseFetchItems()
	if err != nil {
		return err
	}

	if hasItem(opts, kwModSeq) {
		c.enableCondstore()
	}

	if !c.r.eol() { // modifiers
		if ok, err := c.parseFetchModifiers(tag, uid, opts); err != nil || !ok {
			return err
		}
	}

	if opts.vanished {
		c.reportVanished(set, opts.changedSince)
	}

	seqs, metas := c.resolve(set, uid)

	flagEvents, failed, err := c.fetchMessages(seqs, metas, opts, uid)
	if err != nil {
		return err
	}

	c.sieveEvents("FLAG", c.selectedTarget(), flagEvents, metas)

	switch {
	case errors.Is(failed, errUnknownCTE):
		// RFC 3516 4.2: a BINARY section that cannot be decoded fails
		// the request.
		c.no(tag, "UNKNOWN-CTE", "Content-Transfer-Encoding of a BINARY section is unknown")
		return nil
	case failed != nil:
		c.no(tag, "SERVERBUG", "Some messages could not be read")
		return nil
	}

	c.ok(tag, "", "FETCH completed")

	return nil
}

// parseFetchModifiers parses the FETCH modifier list (CHANGEDSINCE,
// VANISHED) into opts. ok is false when the command was answered already.
func (c *conn) parseFetchModifiers(tag string, uid bool, opts *fetchOptions) (bool, error) {
	t, err := c.r.next()
	if err != nil {
		return false, err
	}

	for i := 0; i < len(t.list); i++ {
		switch strings.ToUpper(t.list[i].str) {
		case "CHANGEDSINCE":
			i++
			if i >= len(t.list) {
				return false, fmt.Errorf("%w: CHANGEDSINCE needs a modseq", errSyntax)
			}

			opts.changedSince, _ = strconv.ParseUint(t.list[i].str, 10, 64)

			c.enableCondstore()
			c.condstore = true
		case "VANISHED":
			if !uid || !c.qresync {
				c.bad(tag, "VANISHED requires UID FETCH and QRESYNC")
				return false, nil
			}

			opts.vanished = true
		}
	}

	return true, nil
}

// reportVanished sends VANISHED (EARLIER) for the UIDs of set expunged
// since the given modseq.
func (c *conn) reportVanished(set *numSet, since uint64) {
	vanished, err := c.mu.Vanished(c.mbox.ID, since)
	if err != nil {
		return
	}

	var in []uint32

	set = c.concrete(set)

	for _, u := range vanished {
		if set.contains(u, c.lastUID()) {
			in = append(in, u)
		}
	}

	if len(in) > 0 {
		c.w.untagged("VANISHED (EARLIER) %s", formatSet(in))
	}
}

// fetchSetsSeen reports whether a non-PEEK body item implies \Seen.
func fetchSetsSeen(opts *fetchOptions) bool {
	markSeen := false

	for _, it := range opts.items {
		if (it.section != nil && !it.section.Peek) || (it.binary != nil && !it.binary.Peek) {
			markSeen = true
		}
	}

	return markSeen
}

// fetchMessages writes the FETCH responses for the resolved messages. It
// returns the messages whose \Seen flag the FETCH set and, as failed,
// why a message was left out (errReadFailed, errUnknownCTE; the first
// read failure wins).
func (c *conn) fetchMessages(seqs []int, metas []model.Meta, opts *fetchOptions, uid bool) (flagEvents []model.Meta, failed, err error) {
	markSeen := fetchSetsSeen(opts)
	marked := c.markSeenBatch(markSeen, metas, opts)

	for i, m := range metas {
		if opts.changedSince != 0 && m.ModSeq <= opts.changedSince {
			continue
		}

		seenSet := false

		if markSeen {
			var changed bool

			m, seenSet, changed = c.fetchSetSeen(seqs[i], m, marked)
			if changed {
				flagEvents = append(flagEvents, m)
			}
		}

		if err := c.writeFetchSeen(seqs[i], &m, opts, uid, seenSet); err != nil {
			if errors.Is(err, errReadFailed) || errors.Is(err, errUnknownCTE) {
				if failed == nil || errors.Is(err, errReadFailed) {
					failed = err
				}

				continue
			}

			return flagEvents, failed, err
		}
	}

	return flagEvents, failed, nil
}

// fetchSetSeen sets \Seen on message seq when the session may. It
// returns the updated message, whether the response is to carry its
// flags, and whether this session is what changed them — another
// session of the same account may have set the mark of a public folder
// first, and then the client still has to be told, but no flag event
// belongs to this FETCH.
func (c *conn) fetchSetSeen(seq int, m model.Meta, marked map[uint32]bool) (meta model.Meta, show, changed bool) {
	if c.readOnly || !model.HasRight(c.mrights, 's') || model.HasFlag(m.Flags, flagSeen) {
		return m, false, false
	}

	// In a public folder "read" belongs to the account, so the implicit
	// \Seen of a FETCH writes the account's own mark, exactly like an
	// explicit STORE. Writing the shared flag here would mark the
	// message read for everyone who has no mark of their own — and most
	// clients fetch BODY[] rather than BODY.PEEK[]. The marks are
	// already written (markSeenBatch); what is left is the overlay.
	if c.mpublic {
		msgs := []model.Meta{m}
		c.privateOverlay(msgs)
		c.msgs[seq-1] = msgs[0]

		// The snapshot said unread, so a mark that reads as seen now is
		// news for the client, whoever wrote it.
		return msgs[0], model.HasFlag(msgs[0].Flags, flagSeen), marked[m.UID]
	}

	res, err := c.mu.SetFlags(c.mbox.Name, []uint32{m.UID}, 0, addFlags([]string{flagSeenLower}))
	if err != nil || len(res.Updated) != 1 {
		return m, false, false
	}

	m = res.Updated[0]
	c.msgs[seq-1] = m

	return m, true, true
}

// markSeenBatch writes the account's own \Seen marks for every message
// of a FETCH into a public folder, in one step. A mark takes a MODSEQ of
// the mailbox and announces a change, so one per message would turn a
// FETCH over a folder into as many announcements as it has messages. It
// returns the UIDs whose mark this FETCH changed.
func (c *conn) markSeenBatch(markSeen bool, metas []model.Meta, opts *fetchOptions) map[uint32]bool {
	if !markSeen || !c.mpublic || c.readOnly || !model.HasRight(c.mrights, 's') {
		return nil
	}

	var want []model.Meta

	for _, m := range metas {
		if opts.changedSince != 0 && m.ModSeq <= opts.changedSince {
			continue
		}

		if !model.HasFlag(m.Flags, flagSeen) {
			want = append(want, m)
		}
	}

	if len(want) == 0 {
		return nil
	}

	marked, err := c.markSeenAll(want)
	if err != nil {
		c.log.Warn("private seen marks of a fetch failed", "count", len(want), "err", err)

		return nil
	}

	return marked
}

// markSeenAll writes the marks of the given messages and returns the
// UIDs whose mark changed.
func (c *conn) markSeenAll(want []model.Meta) (map[uint32]bool, error) {
	plan, err := c.planPrivateSeen(storeArgs{op: kwAddFlags, flags: []string{flagSeenLower}}, want)
	if err != nil {
		return nil, err
	}

	ps, err := c.writePrivateSeen(plan, plan.changed)
	if err != nil {
		return nil, err
	}

	marked := make(map[uint32]bool, len(ps.changed))
	for _, uid := range ps.changed {
		marked[uid] = true
	}

	return marked, nil
}

var (
	errReadFailed = errors.New("message read failed")
	errUnknownCTE = errors.New("unknown content transfer encoding")
)

// parseFetchItems parses the data item list, a single item, or a macro.
func (c *conn) parseFetchItems() (*fetchOptions, error) {
	return parseFetchItemsFrom(c.r)
}

// parseFetchItemsFrom parses fetch items from r (the command reader, or
// a private reader for NOTIFY fetch attributes).
func parseFetchItemsFrom(r *reader) (*fetchOptions, error) {
	opts := &fetchOptions{}

	items, err := readFetchItems(r)
	if err != nil {
		return nil, err
	}

	expanded := expandFetchMacros(items)
	if len(expanded) > 64 {
		return nil, fmt.Errorf("%w: too many fetch items", errSyntax)
	}

	for i := 0; i < len(expanded); i++ {
		it, err := parseFetchItem(expanded[i])
		if err != nil {
			return nil, err
		}

		// PREVIEW may carry its modifiers (RFC 8970 4.1). LAZY asks for
		// the preview only when it costs no delay; it is computed at
		// delivery and stored with the message, so it never does, and
		// PREVIEW (LAZY) answers like PREVIEW.
		if it.name == kwPreview && i+1 < len(expanded) && strings.HasPrefix(expanded[i+1], "(") {
			if err := checkPreviewModifiers(expanded[i+1]); err != nil {
				return nil, err
			}

			i++
		}

		opts.items = append(opts.items, it)
	}

	return opts, nil
}

// readFetchItems reads the item list, or the single item, of FETCH as
// raw strings.
func readFetchItems(r *reader) ([]string, error) {
	r.skipSpaces()

	if r.peek() == '(' {
		// Items may contain brackets and spaces (HEADER.FIELDS (...)), so
		// take the balanced parenthesised group verbatim and split it.
		group, err := r.balanced()
		if err != nil {
			return nil, err
		}

		return splitItems(group[1 : len(group)-1]), nil
	}

	a, err := r.balanced()
	if err != nil {
		return nil, err
	}

	if strings.EqualFold(a, kwPreview) {
		return append([]string{a}, previewModifiers(r)...), nil
	}

	return []string{a}, nil
}

// expandFetchMacros replaces ALL, FAST and FULL by their items. The
// macros are expanded into a new list: appending while ranging would
// never visit the added items.
func expandFetchMacros(items []string) []string {
	var expanded []string

	for _, raw := range items {
		switch strings.ToUpper(raw) {
		case kwAll:
			expanded = append(expanded, kwFlags, kwInternalDate, kwRFC822Size, kwEnvelope)
		case "FAST":
			expanded = append(expanded, kwFlags, kwInternalDate, kwRFC822Size)
		case "FULL":
			expanded = append(expanded, kwFlags, kwInternalDate, kwRFC822Size, kwEnvelope, kwBody)
		default:
			expanded = append(expanded, raw)
		}
	}

	return expanded
}

// previewModifiers returns the parenthesised group after a lone PREVIEW
// item ("FETCH 1 PREVIEW (LAZY)") when it holds preview modifiers only;
// any other group (the FETCH modifiers of RFC 4466) is left unread.
func previewModifiers(r *reader) []string {
	save := r.pos

	r.skipSpaces()

	if r.peek() != '(' {
		r.pos = save
		return nil
	}

	group, err := r.balanced()
	if err != nil || len(group) < 2 {
		r.pos = save
		return nil
	}

	for _, m := range strings.Fields(group[1 : len(group)-1]) {
		if !strings.EqualFold(m, kwLazy) {
			r.pos = save
			return nil
		}
	}

	return []string{group}
}

// checkPreviewModifiers checks the modifier list of a PREVIEW item; LAZY
// is the only modifier (RFC 8970 6).
func checkPreviewModifiers(group string) error {
	mods := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(group, "("), ")"))
	if len(mods) == 0 {
		return fmt.Errorf("%w: empty PREVIEW modifier list", errSyntax)
	}

	for _, m := range mods {
		if !strings.EqualFold(m, kwLazy) {
			return fmt.Errorf("%w: unknown PREVIEW modifier %q", errSyntax, m)
		}
	}

	return nil
}

// balanced reads a token that may contain balanced parentheses and
// brackets, e.g. "BODY.PEEK[HEADER.FIELDS (FROM TO)]<0.100>".
func (r *reader) balanced() (string, error) {
	r.skipSpaces()
	start := r.pos
	depth := 0

	for r.pos < len(r.line) {
		ch := r.line[r.pos]
		switch ch {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ' ':
			if depth == 0 {
				return string(r.line[start:r.pos]), nil
			}
		}

		r.pos++
		if depth == 0 && (ch == ')') && start < r.pos-1 && r.line[start] == '(' {
			return string(r.line[start:r.pos]), nil
		}
	}

	if depth != 0 {
		return "", fmt.Errorf("%w: unbalanced fetch item", errSyntax)
	}

	return string(r.line[start:r.pos]), nil
}

// splitItems splits "FLAGS BODY[HEADER.FIELDS (A B)] UID" at top-level spaces.
func splitItems(s string) []string {
	var out []string

	depth, start := 0, 0

	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(', '[':
			depth++
		case ')', ']':
			depth--
		case ' ':
			if depth == 0 {
				if i > start {
					out = append(out, s[start:i])
				}

				start = i + 1
			}
		}
	}

	if start < len(s) {
		out = append(out, s[start:])
	}

	return out
}

func parseFetchItem(raw string) (fetchItem, error) {
	it := fetchItem{raw: raw}

	up := strings.ToUpper(raw)
	if sec := rfc822Section(up); sec != nil {
		it.name, it.section = kwBody, sec
		return it, nil
	}

	if handled, err := parseSectionItem(&it, up, raw); handled {
		return it, err
	}

	switch up {
	case kwUID, kwFlags, kwInternalDate, kwRFC822Size, kwEnvelope, kwBody, kwBodyStructure,
		kwModSeq, kwEmailID, kwThreadID, kwPreview, kwSaveDate:
		it.name = up
		return it, nil
	}

	return it, fmt.Errorf("%w: unknown fetch item %q", errSyntax, raw)
}

// rfc822Section maps the RFC822, RFC822.HEADER and RFC822.TEXT items to
// their body section; nil for any other item.
func rfc822Section(up string) *imap.FetchItemBodySection {
	switch up {
	case kwRFC822:
		return &imap.FetchItemBodySection{}
	case kwRFC822Header:
		return &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	case kwRFC822Text:
		return &imap.FetchItemBodySection{Specifier: imap.PartSpecifierText}
	}

	return nil
}

// parseSectionItem parses the BODY[...], BINARY[...] and BINARY.SIZE[...]
// items into it. handled is false for any other item; on error it is left
// without name.
func parseSectionItem(it *fetchItem, up, raw string) (handled bool, err error) {
	switch {
	case strings.HasPrefix(up, "BODY[") || strings.HasPrefix(up, "BODY.PEEK["):
		sec, err := parseSection(raw)
		if err != nil {
			return true, err
		}

		sec.Peek = strings.HasPrefix(up, "BODY.PEEK[")
		it.name, it.section = kwBody, sec

		return true, nil
	case strings.HasPrefix(up, "BINARY.SIZE["):
		part, _, err := parseBracket(raw)
		if err != nil {
			return true, err
		}

		it.name, it.binSize = kwBinarySize, &imap.FetchItemBinarySectionSize{Part: parsePart(part)}

		return true, nil
	case strings.HasPrefix(up, "BINARY[") || strings.HasPrefix(up, "BINARY.PEEK["):
		part, partial, err := parseBracket(raw)
		if err != nil {
			return true, err
		}

		it.name, it.binary = kwBinary, &imap.FetchItemBinarySection{Part: parsePart(part), Partial: partial, Peek: strings.HasPrefix(up, "BINARY.PEEK[")}

		return true, nil
	}

	return false, nil
}

// parseBracket splits "X[inner]<a.b>" into inner and the partial range.
func parseBracket(raw string) (string, *imap.SectionPartial, error) {
	i := strings.IndexByte(raw, '[')

	j := strings.LastIndexByte(raw, ']')
	if i < 0 || j < i {
		return "", nil, fmt.Errorf("%w: bad section", errSyntax)
	}

	var partial *imap.SectionPartial

	if rest := raw[j+1:]; strings.HasPrefix(rest, "<") && strings.HasSuffix(rest, ">") {
		a, b, ok := strings.Cut(rest[1:len(rest)-1], ".")
		off, err1 := strconv.ParseInt(a, 10, 64)

		size, err2 := strconv.ParseInt(b, 10, 64)
		if !ok || err1 != nil || err2 != nil || off < 0 || size < 0 {
			return "", nil, fmt.Errorf("%w: bad partial", errSyntax)
		}

		if size > math.MaxInt64-off { // no overflow in offset+size
			size = math.MaxInt64 - off
		}

		partial = &imap.SectionPartial{Offset: off, Size: size}
	}

	return raw[i+1 : j], partial, nil
}

func parsePart(s string) []int {
	var part []int

	for _, p := range strings.Split(s, ".") {
		if n, err := strconv.Atoi(p); err == nil {
			part = append(part, n)
		}
	}

	return part
}

// parseSection parses BODY[<part>.<specifier> (fields)]<partial>.
func parseSection(raw string) (*imap.FetchItemBodySection, error) {
	inner, partial, err := parseBracket(raw)
	if err != nil {
		return nil, err
	}

	sec := &imap.FetchItemBodySection{Partial: partial}
	sec.Part, inner = splitPartPrefix(inner)

	up := strings.ToUpper(inner)
	switch {
	case up == "":
	case up == kwHeader:
		sec.Specifier = imap.PartSpecifierHeader
	case up == kwText:
		sec.Specifier = imap.PartSpecifierText
	case up == "MIME":
		sec.Specifier = imap.PartSpecifierMIME
	case strings.HasPrefix(up, "HEADER.FIELDS.NOT"), strings.HasPrefix(up, "HEADER.FIELDS"):
		if err := parseHeaderFields(sec, inner, up); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: unknown section %q", errSyntax, inner)
	}

	return sec, nil
}

// splitPartPrefix splits the leading part numbers ("1.2.") off a section
// spec and returns them with the remaining specifier.
func splitPartPrefix(inner string) ([]int, string) {
	var part []int

	for {
		k := strings.IndexByte(inner, '.')

		var head string
		if k < 0 {
			head = inner
		} else {
			head = inner[:k]
		}

		n, err := strconv.Atoi(head)
		if err != nil || head == "" {
			return part, inner
		}

		part = append(part, n)

		if k < 0 {
			return part, ""
		}

		inner = inner[k+1:]
	}
}

// parseHeaderFields fills sec from a HEADER.FIELDS or HEADER.FIELDS.NOT
// specifier with its field list; up is inner upper-cased.
func parseHeaderFields(sec *imap.FetchItemBodySection, inner, up string) error {
	sec.Specifier = imap.PartSpecifierHeader
	l := strings.IndexByte(inner, '(')

	r := strings.LastIndexByte(inner, ')')
	if l < 0 || r < l {
		return fmt.Errorf("%w: HEADER.FIELDS needs a list", errSyntax)
	}

	fields := strings.Fields(inner[l+1 : r])
	for i := range fields {
		fields[i] = strings.Trim(fields[i], `"`)
	}

	if strings.HasPrefix(up, "HEADER.FIELDS.NOT") {
		sec.HeaderFieldsNot = fields
	} else {
		sec.HeaderFields = fields
	}

	return nil
}

// writeFetch renders one FETCH response.
func (c *conn) writeFetch(seq int, m *model.Meta, opts *fetchOptions, uidCmd bool) error {
	return c.writeFetchSeen(seq, m, opts, uidCmd, false)
}

// writeFetchSeen is writeFetch; with seenSet the FLAGS are included even
// if not requested (RFC 3501 6.4.5: the server SHOULD report a \Seen
// change made by the FETCH).
func (c *conn) writeFetchSeen(seq int, m *model.Meta, opts *fetchOptions, uidCmd bool, seenSet bool) error {
	var parts []string

	if seenSet && !hasItem(opts, kwFlags) {
		parts = append(parts, "FLAGS ("+c.flagsOut(m)+")")
	}

	var raw []byte

	if needsRaw(opts, m) {
		rel, err := membudget.Acquire(m.Size)
		if err != nil {
			return errReadFailed
		}
		defer rel()

		if raw, err = c.mu.Read(m); err != nil {
			c.log.Error("read message", "guid", m.GUID, "err", err)
			return errReadFailed
		}
	}

	if binaryUndecodable(opts, raw) {
		return errUnknownCTE
	}

	for _, it := range opts.items {
		if part, ok := c.fetchItemOut(it, m, raw); ok {
			parts = append(parts, part)
		}
	}

	if uidCmd && !hasItem(opts, kwUID) {
		parts = append([]string{fmt.Sprintf("UID %d", m.UID)}, parts...)
	}

	if c.implicitModSeq(opts) {
		parts = append(parts, fmt.Sprintf("MODSEQ (%d)", m.ModSeq))
	}

	c.w.untagged("%d FETCH (%s)", seq, strings.Join(parts, " "))

	return nil
}

// needsRaw reports whether any item needs the message content.
func needsRaw(opts *fetchOptions, m *model.Meta) bool {
	needRaw := false

	for _, it := range opts.items {
		if it.section != nil || it.binary != nil || it.binSize != nil || it.name == kwBodyStructure || it.name == kwBody || (it.name == kwEnvelope && m.Envelope == nil) {
			needRaw = true
		}
	}

	return needRaw
}

// implicitModSeq reports whether MODSEQ is added to every FETCH response
// although not requested (CONDSTORE with CHANGEDSINCE).
func (c *conn) implicitModSeq(opts *fetchOptions) bool {
	return c.condstore && opts.changedSince != 0 && !hasItem(opts, kwModSeq)
}

// fetchDateLayout is the INTERNALDATE/SAVEDATE format.
const fetchDateLayout = "02-Jan-2006 15:04:05 -0700"

// fetchAttrs render the FETCH items that only need the message metadata.
var fetchAttrs = map[string]func(c *conn, m *model.Meta) string{
	kwUID:   func(_ *conn, m *model.Meta) string { return fmt.Sprintf("UID %d", m.UID) },
	kwFlags: func(c *conn, m *model.Meta) string { return fmt.Sprintf("FLAGS (%s)", c.flagsOut(m)) },
	kwInternalDate: func(_ *conn, m *model.Meta) string {
		return fmt.Sprintf("INTERNALDATE %s", quote(m.InternalDate.Format(fetchDateLayout)))
	},
	kwSaveDate: func(_ *conn, m *model.Meta) string {
		return fmt.Sprintf("SAVEDATE %s", quote(m.SaveDate.Format(fetchDateLayout)))
	},
	kwRFC822Size: func(_ *conn, m *model.Meta) string { return fmt.Sprintf("RFC822.SIZE %d", m.Size) },
	kwModSeq:     func(_ *conn, m *model.Meta) string { return fmt.Sprintf("MODSEQ (%d)", m.ModSeq) },
	kwEmailID:    func(_ *conn, m *model.Meta) string { return fmt.Sprintf("EMAILID (%s)", m.GUID) },
	kwThreadID:   func(_ *conn, m *model.Meta) string { return fmt.Sprintf("THREADID (%s)", threadID(m)) },
	// RFC 8970 3.2: a message without preview text has the empty
	// string; NIL is only for PREVIEW (LAZY), and never needed here.
	kwPreview: func(_ *conn, m *model.Meta) string { return "PREVIEW " + quote(m.Preview) },
}

// fetchItemOut renders one FETCH item of m; raw is the message content
// when needsRaw asked for it. ok is false for an unknown item.
func (c *conn) fetchItemOut(it fetchItem, m *model.Meta, raw []byte) (string, bool) {
	if f, ok := fetchAttrs[it.name]; ok {
		return f(c, m), true
	}

	switch it.name {
	case kwEnvelope:
		env, routes := m.Envelope, m.Routes
		if env == nil {
			p := mailmeta.Parse(raw)
			env, routes = p.Envelope, p.Routes
		}

		return "ENVELOPE " + encodeEnvelope(env, routes), true
	case kwBodyStructure:
		return "BODYSTRUCTURE " + encodeBodyStructure(imapserver.ExtractBodyStructure(bytes.NewReader(raw)), true), true
	case kwBody:
		if it.section == nil {
			return "BODY " + encodeBodyStructure(imapserver.ExtractBodyStructure(bytes.NewReader(raw)), false), true
		}

		b := imapserver.ExtractBodySection(bytes.NewReader(raw), it.section)

		return sectionLabel(it) + " " + literalString(b), true
	case kwBinary:
		b := imapserver.ExtractBinarySection(bytes.NewReader(raw), it.binary)
		return sectionLabel(it) + " ~" + literalString(b), true
	case kwBinarySize:
		return fmt.Sprintf("%s %d", it.raw, imapserver.ExtractBinarySectionSize(bytes.NewReader(raw), it.binSize)), true
	}

	return "", false
}

// binaryUndecodable reports whether a BINARY or BINARY.SIZE item asks
// for a section whose Content-Transfer-Encoding is not one the server
// decodes (RFC 3516 4.2).
func binaryUndecodable(opts *fetchOptions, raw []byte) bool {
	for _, it := range opts.items {
		var part []int

		switch {
		case it.binary != nil:
			part = it.binary.Part
		case it.binSize != nil:
			part = it.binSize.Part
		default:
			continue
		}

		if !knownCTE(sectionCTE(raw, part)) {
			return true
		}
	}

	return false
}

// sectionCTE is the Content-Transfer-Encoding of a section: of the
// message for the empty part, else of that MIME part. A section that
// does not exist has none.
func sectionCTE(raw []byte, part []int) string {
	sec := &imap.FetchItemBodySection{Part: part, Specifier: imap.PartSpecifierMIME}
	if len(part) == 0 {
		sec.Specifier = imap.PartSpecifierHeader
	}

	hdr, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(imapserver.ExtractBodySection(bytes.NewReader(raw), sec))))
	if err != nil {
		return ""
	}

	return hdr.Get("Content-Transfer-Encoding")
}

// knownCTE reports whether the BINARY items can decode an encoding: the
// identity encodings, base64 and quoted-printable (what go-message reads).
func knownCTE(cte string) bool {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "", "7bit", "8bit", cteBinary, "base64", "quoted-printable":
		return true
	}

	return false
}

func hasItem(o *fetchOptions, name string) bool {
	for _, it := range o.items {
		if it.name == name {
			return true
		}
	}

	return false
}

// sectionLabel is the response label for a BODY[]/BINARY[] item: the
// request without .PEEK and with the partial offset only.
func sectionLabel(it fetchItem) string {
	label := it.raw
	label = strings.Replace(label, ".PEEK[", "[", 1)
	label = strings.Replace(label, ".peek[", "[", 1)

	up := strings.ToUpper(it.raw)
	switch up {
	case kwRFC822:
		return kwRFC822
	case kwRFC822Header:
		return kwRFC822Header
	case kwRFC822Text:
		return kwRFC822Text
	}

	if i := strings.LastIndexByte(label, '<'); i > 0 {
		off, _, _ := strings.Cut(label[i+1:], ".")
		label = label[:i] + "<" + off + ">"
	}

	return label
}

func literalString(b []byte) string {
	return fmt.Sprintf("{%d}\r\n%s", len(b), b)
}

// threadID derives a stable thread identifier from the root of the
// References chain (or the message's own Message-ID).
func threadID(m *model.Meta) string { return mailmeta.ThreadID(m) }

// --- encoding of ENVELOPE and BODYSTRUCTURE ---------------------------------

// encodeAddrs renders an address list of the ENVELOPE; routes, parallel
// to l, are the at-domain-lists (source routes) of its addresses.
func encodeAddrs(l []imap.Address, routes []string) string {
	if len(l) == 0 {
		return kwNIL
	}

	var parts []string

	for i, a := range l {
		adl := ""
		if i < len(routes) {
			adl = routes[i]
		}

		parts = append(parts, fmt.Sprintf("(%s %s %s %s)", nstring(a.Name), nstring(adl), nstring(a.Mailbox), nstring(a.Host)))
	}

	return "(" + strings.Join(parts, "") + ")"
}

// encodeEnvelope renders an ENVELOPE (RFC 3501 7.4.2) with the source
// routes of its addresses (model.Meta.Routes; nil for none).
func encodeEnvelope(e *imap.Envelope, routes map[string][]string) string {
	if e == nil {
		return kwNIL
	}

	date := kwNIL
	if !e.Date.IsZero() {
		date = quote(e.Date.Format(time.RFC1123Z))
	}

	return fmt.Sprintf("(%s %s %s %s %s %s %s %s %s %s)", date, nstring(e.Subject),
		encodeAddrs(e.From, routes[model.RouteFrom]), encodeAddrs(e.Sender, routes[model.RouteSender]),
		encodeAddrs(e.ReplyTo, routes[model.RouteReplyTo]), encodeAddrs(e.To, routes[model.RouteTo]),
		encodeAddrs(e.Cc, routes[model.RouteCc]), encodeAddrs(e.Bcc, routes[model.RouteBcc]),
		nstring(strings.Join(e.InReplyTo, " ")), nstring(e.MessageID))
}

func encodeParams(p map[string]string) string {
	if len(p) == 0 {
		return kwNIL
	}

	var parts []string
	for k, v := range p {
		parts = append(parts, quote(k), quote(v))
	}

	return "(" + strings.Join(parts, " ") + ")"
}

func encodeDisposition(d *imap.BodyStructureDisposition) string {
	if d == nil {
		return kwNIL
	}

	return fmt.Sprintf("(%s %s)", quote(d.Value), encodeParams(d.Params))
}

func encodeLang(l []string) string {
	switch len(l) {
	case 0:
		return kwNIL
	case 1:
		return quote(l[0])
	}

	var q []string
	for _, x := range l {
		q = append(q, quote(x))
	}

	return "(" + strings.Join(q, " ") + ")"
}

func encodeBodyStructure(bs imap.BodyStructure, extended bool) string {
	switch b := bs.(type) {
	case *imap.BodyStructureSinglePart:
		var sb strings.Builder

		enc := b.Encoding
		if enc == "" {
			enc = "7BIT"
		}

		fmt.Fprintf(&sb, "(%s %s %s %s %s %s %d", quote(b.Type), quote(b.Subtype), encodeParams(b.Params),
			nstring(b.ID), nstring(b.Description), quote(enc), b.Size)

		if b.MessageRFC822 != nil {
			fmt.Fprintf(&sb, " %s %s %d", encodeEnvelope(b.MessageRFC822.Envelope, nil),
				encodeBodyStructure(b.MessageRFC822.BodyStructure, extended), b.MessageRFC822.NumLines)
		} else if b.Text != nil {
			fmt.Fprintf(&sb, " %d", b.Text.NumLines)
		}

		if extended {
			ext := b.Extended
			if ext == nil {
				ext = &imap.BodyStructureSinglePartExt{}
			}

			fmt.Fprintf(&sb, " NIL %s %s %s", encodeDisposition(ext.Disposition), encodeLang(ext.Language), nstring(ext.Location))
		}

		sb.WriteByte(')')

		return sb.String()
	case *imap.BodyStructureMultiPart:
		var sb strings.Builder
		sb.WriteByte('(')

		for _, ch := range b.Children {
			sb.WriteString(encodeBodyStructure(ch, extended))
		}

		fmt.Fprintf(&sb, " %s", quote(b.Subtype))

		if extended {
			ext := b.Extended
			if ext == nil {
				ext = &imap.BodyStructureMultiPartExt{}
			}

			fmt.Fprintf(&sb, " %s %s %s %s", encodeParams(ext.Params), encodeDisposition(ext.Disposition), encodeLang(ext.Language), nstring(ext.Location))
		}

		sb.WriteByte(')')

		return sb.String()
	}

	return kwNIL
}
