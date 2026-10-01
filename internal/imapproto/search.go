package imapproto

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"

	"shardpost/internal/fts"
	"shardpost/internal/mailmeta"
	"shardpost/internal/model"
)

func extractMeta(raw []byte) (*imap.Envelope, []string, string) {
	return mailmeta.Extract(raw)
}

// criteria is a parsed SEARCH key tree. Text criteria are resolved
// against the full-text index; everything else against the snapshot.
type criteria struct {
	seqSets               []*numSet
	uidSets               []*numSet
	flags                 []string
	notFlags              []string
	since, before         time.Time
	sentSince, sentBefore time.Time
	younger, older        int64 // WITHIN (RFC 5032): seconds relative to now
	recent                int   // 1 = RECENT/NEW, -1 = OLD
	larger, smaller       int64
	modseq                uint64
	emailIDs              []string
	threadIDs             []string
	text                  []textCrit // narrowed by the index, checked per message (searchtext.go)
	not                   []*criteria
	or                    [][2]*criteria
}

type textCrit struct {
	fields []string // fts fields
	key    string   // header key for HEADER <key> <value>
	value  string

	// Set by prepareText. hdrCand and bodyCand are supersets of the
	// messages whose header or body can match (nil: not narrowed);
	// exact is the index's own answer in word search (Options.WordSearch).
	hdrCand, bodyCand map[string]bool
	exact             map[string]bool
	byIndex           bool
}

// parseSearchKeys parses keys until end of line or closing paren.
func (c *conn) parseSearchKeys(r *reader, depth int) (*criteria, error) {
	cr := &criteria{}

	for !r.eol() {
		if r.peek() == ')' {
			if depth == 0 {
				return nil, fmt.Errorf("%w: unexpected )", errSyntax)
			}

			return cr, nil
		}

		if err := c.parseSearchKey(r, cr, depth); err != nil {
			return nil, err
		}
	}

	return cr, nil
}

func (c *conn) parseSearchKey(r *reader, cr *criteria, depth int) error {
	r.skipSpaces()

	if r.peek() == '(' {
		return c.parseSearchGroup(r, cr, depth)
	}

	key, err := r.atom()
	if err != nil {
		return err
	}

	up := strings.ToUpper(key)

	switch up {
	case "NOT":
		sub := &criteria{}
		if err := c.parseSearchKey(r, sub, depth); err != nil {
			return err
		}

		cr.not = append(cr.not, sub)

		return nil
	case "OR":
		a, b := &criteria{}, &criteria{}
		if err := c.parseSearchKey(r, a, depth); err != nil {
			return err
		}

		if err := c.parseSearchKey(r, b, depth); err != nil {
			return err
		}

		cr.or = append(cr.or, [2]*criteria{a, b})

		return nil
	}

	if p, ok := searchKeys[up]; ok {
		return p(c, r, cr, up)
	}

	return c.parseSearchSet(cr, key, up)
}

// parseSearchGroup parses a parenthesised key list (an AND group).
func (c *conn) parseSearchGroup(r *reader, cr *criteria, depth int) error {
	r.pos++

	sub, err := c.parseSearchKeys(r, depth+1)
	if err != nil {
		return err
	}

	r.skipSpaces()

	if r.peek() != ')' {
		return fmt.Errorf("%w: expected )", errSyntax)
	}

	r.pos++

	cr.not = append(cr.not, &criteria{not: []*criteria{sub}}) // double negation = AND group

	return nil
}

// parseSearchSet parses a sequence set key ("$" is the saved result).
func (c *conn) parseSearchSet(cr *criteria, key, up string) error {
	if up == "$" || up[0] == '*' || (up[0] >= '0' && up[0] <= '9') {
		set, err := parseSet(key)
		if err != nil {
			return err
		}

		if set.searchRes {
			cr.uidSets = append(cr.uidSets, c.concrete(set))
		} else {
			cr.seqSets = append(cr.seqSets, set)
		}

		return nil
	}

	return fmt.Errorf("%w: unknown search key %q", errSyntax, key)
}

// searchKeyParser reads the arguments of the search key up from r into cr.
type searchKeyParser func(c *conn, r *reader, cr *criteria, up string) error

// searchKeys are the search keys other than NOT, OR, groups and sets.
var searchKeys = map[string]searchKeyParser{
	kwAll:        func(_ *conn, _ *reader, _ *criteria, _ string) error { return nil },
	"ANSWERED":   flagKey(`\answered`, false),
	kwDeleted:    flagKey(flagDeletedLower, false),
	"DRAFT":      flagKey(`\draft`, false),
	"FLAGGED":    flagKey(`\flagged`, false),
	"SEEN":       flagKey(flagSeenLower, false),
	"UNANSWERED": flagKey(`\answered`, true),
	"UNDELETED":  flagKey(flagDeletedLower, true),
	"UNDRAFT":    flagKey(`\draft`, true),
	"UNFLAGGED":  flagKey(`\flagged`, true),
	kwUnseen:     flagKey(flagSeenLower, true),
	"NEW": func(_ *conn, _ *reader, cr *criteria, _ string) error {
		cr.notFlags = append(cr.notFlags, flagSeenLower)
		cr.recent = 1

		return nil
	},
	kwRecent: func(_ *conn, _ *reader, cr *criteria, _ string) error { cr.recent = 1; return nil },
	"OLD":    func(_ *conn, _ *reader, cr *criteria, _ string) error { cr.recent = -1; return nil },
	"KEYWORD": func(_ *conn, r *reader, cr *criteria, _ string) error {
		k, err := r.atom()
		if err != nil {
			return err
		}

		cr.flags = append(cr.flags, strings.ToLower(k))

		return nil
	},
	"UNKEYWORD": func(_ *conn, r *reader, cr *criteria, _ string) error {
		k, err := r.atom()
		if err != nil {
			return err
		}

		cr.notFlags = append(cr.notFlags, strings.ToLower(k))

		return nil
	},
	"BEFORE":     dateKey(func(cr *criteria, t time.Time) { cr.before = t }),
	"SINCE":      dateKey(func(cr *criteria, t time.Time) { cr.since = t }),
	"ON":         dateKey(func(cr *criteria, t time.Time) { cr.since, cr.before = t, t.AddDate(0, 0, 1) }),
	"SENTBEFORE": dateKey(func(cr *criteria, t time.Time) { cr.sentBefore = t }),
	"SENTSINCE":  dateKey(func(cr *criteria, t time.Time) { cr.sentSince = t }),
	"SENTON":     dateKey(func(cr *criteria, t time.Time) { cr.sentSince, cr.sentBefore = t, t.AddDate(0, 0, 1) }),
	"LARGER":     numKey(func(cr *criteria, n int64) { cr.larger = n }),
	"SMALLER":    numKey(func(cr *criteria, n int64) { cr.smaller = n }),
	"YOUNGER":    numKey(func(cr *criteria, n int64) { cr.younger = n }),
	"OLDER":      numKey(func(cr *criteria, n int64) { cr.older = n }),
	kwUID: func(c *conn, r *reader, cr *criteria, _ string) error {
		s, err := r.atom()
		if err != nil {
			return err
		}

		set, err := parseSet(s)
		if err != nil {
			return err
		}

		cr.uidSets = append(cr.uidSets, c.concrete(set))

		return nil
	},
	kwModSeq:   (*conn).parseSearchModSeq,
	kwEmailID:  idKey(func(cr *criteria, id string) { cr.emailIDs = append(cr.emailIDs, id) }),
	kwThreadID: idKey(func(cr *criteria, id string) { cr.threadIDs = append(cr.threadIDs, id) }),
	// one index field for the three, the header name decides
	kwBcc:     textKeyKey(fts.FieldTo),
	kwCc:      textKeyKey(fts.FieldTo),
	kwTo:      textKeyKey(fts.FieldTo),
	kwFrom:    textKeyKey(fts.FieldFrom),
	kwSubject: textFieldKey(fts.FieldSubject),
	kwBody:    textFieldKey(fts.FieldBody),
	kwText:    textFieldKey(fts.FieldHeader, fts.FieldBody),
	kwHeader: func(_ *conn, r *reader, cr *criteria, _ string) error {
		k, err := r.astring()
		if err != nil {
			return err
		}

		v, err := r.astring()
		if err != nil {
			return err
		}

		cr.text = append(cr.text, textCrit{fields: headerFields(k), key: k, value: v})

		return nil
	},
}

// flagKey is a search key on a (lower-cased) flag, set or, with not, unset.
func flagKey(flag string, not bool) searchKeyParser {
	return func(_ *conn, _ *reader, cr *criteria, _ string) error {
		if not {
			cr.notFlags = append(cr.notFlags, flag)
		} else {
			cr.flags = append(cr.flags, flag)
		}

		return nil
	}
}

// dateKey is a search key with a date argument.
func dateKey(set func(cr *criteria, t time.Time)) searchKeyParser {
	return func(_ *conn, r *reader, cr *criteria, _ string) error {
		s, err := r.astring()
		if err != nil {
			return err
		}

		t, err := time.Parse("_2-Jan-2006", s)
		if err != nil {
			return fmt.Errorf("%w: bad date %q", errSyntax, s)
		}

		set(cr, t)

		return nil
	}
}

// numKey is a search key with a number argument.
func numKey(set func(cr *criteria, n int64)) searchKeyParser {
	return func(_ *conn, r *reader, cr *criteria, _ string) error {
		a, err := r.atom()
		if err != nil {
			return err
		}

		n, err := strconv.ParseInt(a, 10, 64)
		if err != nil {
			return fmt.Errorf("%w: expected number", errSyntax)
		}

		set(cr, n)

		return nil
	}
}

// idKey is a search key with an object id (astring) argument.
func idKey(add func(cr *criteria, id string)) searchKeyParser {
	return func(_ *conn, r *reader, cr *criteria, _ string) error {
		id, err := r.astring()
		if err != nil {
			return err
		}

		add(cr, id)

		return nil
	}
}

// textFieldKey is a text search key on the given index fields.
func textFieldKey(fields ...string) searchKeyParser {
	return func(_ *conn, r *reader, cr *criteria, _ string) error {
		v, err := r.astring()
		if err != nil {
			return err
		}

		cr.text = append(cr.text, textCrit{fields: fields, value: v})

		return nil
	}
}

// textKeyKey is an address search key: the lower-cased key names the
// header, field the index field.
func textKeyKey(field string) searchKeyParser {
	return func(_ *conn, r *reader, cr *criteria, up string) error {
		v, err := r.astring()
		if err != nil {
			return err
		}

		cr.text = append(cr.text, textCrit{fields: []string{field}, key: strings.ToLower(up), value: v})

		return nil
	}
}

// parseSearchModSeq parses MODSEQ [<entry-name> <entry-type>] <modseq>.
func (c *conn) parseSearchModSeq(r *reader, cr *criteria, _ string) error {
	c.enableCondstore()

	a, err := r.next()
	if err != nil {
		return err
	}

	if a.kind == tokString { // entry name given
		if _, err := r.atom(); err != nil { // entry type
			return err
		}

		if a, err = r.next(); err != nil {
			return err
		}
	}

	cr.modseq, err = strconv.ParseUint(a.str, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: bad modseq", errSyntax)
	}

	c.condstore = true

	return nil
}

func headerFields(key string) []string {
	switch strings.ToUpper(key) {
	case kwSubject:
		return []string{fts.FieldSubject}
	case kwFrom, "SENDER", "REPLY-TO":
		return []string{fts.FieldFrom}
	case kwTo, kwCc, kwBcc:
		return []string{fts.FieldTo}
	}

	return []string{fts.FieldHeader}
}

// searchReturn are the ESEARCH RETURN options.
type searchReturn struct {
	min, max, all, count, save bool
	esearch                    bool
}

// cmdSearch handles SEARCH / UID SEARCH with RETURN options, CHARSET and
// the extended keys (MODSEQ, EMAILID, THREADID, SEARCHRES).
func (c *conn) cmdSearch(tag string, uid bool) error {
	ret, cr, err := c.parseSearchCommand()
	if err != nil {
		var bc *badCharset
		if errors.As(err, &bc) {
			c.no(tag, "BADCHARSET (UTF-8)", err.Error())
			return nil
		}

		return err
	}

	seqs, metas, err := c.runSearch(cr)
	if err != nil {
		c.storeErr(tag, err)
		return nil
	}

	nums, highest := searchNums(seqs, metas, uid)

	if ret.save {
		uids := make([]uint32, len(metas))
		for i, m := range metas {
			uids[i] = m.UID
		}

		c.searchRes = uids
	}

	if !ret.esearch {
		line := kwSearch
		for _, n := range nums {
			line += fmt.Sprintf(" %d", n)
		}

		if hasModseq(cr) && len(nums) > 0 {
			line += fmt.Sprintf(" (MODSEQ %d)", highest)
		}

		c.w.untagged("%s", line)
		c.ok(tag, "", "SEARCH completed")

		return nil
	}

	c.w.untagged("ESEARCH (TAG %s) %s", quote(tag), strings.Join(esearchParts(ret, cr, nums, metas, highest, uid), " "))
	c.ok(tag, "", "SEARCH completed")

	return nil
}

// searchNums returns the UIDs or sequence numbers of the matches and
// their highest modseq.
func searchNums(seqs []int, metas []model.Meta, uid bool) ([]uint32, uint64) {
	nums := make([]uint32, len(metas))

	var highest uint64

	for i, m := range metas {
		if uid {
			nums[i] = m.UID
		} else {
			nums[i] = uint32(seqs[i])
		}

		if m.ModSeq > highest {
			highest = m.ModSeq
		}
	}

	return nums, highest
}

// esearchParts renders the ESEARCH result items requested with RETURN.
func esearchParts(ret searchReturn, cr *criteria, nums []uint32, metas []model.Meta, highest uint64, uid bool) []string {
	var parts []string
	if uid {
		parts = append(parts, kwUID)
	}

	if len(nums) > 0 {
		highest = esearchHighest(ret, metas, highest)

		if ret.min {
			parts = append(parts, fmt.Sprintf("MIN %d", nums[0]))
		}

		if ret.max {
			parts = append(parts, fmt.Sprintf("MAX %d", nums[len(nums)-1]))
		}

		if ret.all {
			parts = append(parts, "ALL "+formatSet(nums))
		}

		if hasModseq(cr) {
			parts = append(parts, fmt.Sprintf("MODSEQ %d", highest))
		}
	}

	if ret.count {
		parts = append(parts, fmt.Sprintf("COUNT %d", len(nums)))
	}

	return parts
}

// esearchHighest is the MODSEQ of an ESEARCH result. RFC 7162 3.1.5: the
// MODSEQ is the highest of the messages actually returned, with only MIN
// and/or MAX requested only those.
func esearchHighest(ret searchReturn, metas []model.Meta, highest uint64) uint64 {
	if ret.all || ret.count || (!ret.min && !ret.max) {
		return highest
	}

	highest = 0
	if ret.min {
		highest = metas[0].ModSeq
	}

	if ret.max && metas[len(metas)-1].ModSeq > highest {
		highest = metas[len(metas)-1].ModSeq
	}

	return highest
}

// hasModseq tells whether a MODSEQ key occurs anywhere in the criteria
// (RFC 7162 3.1.5: the response then carries MODSEQ).
func hasModseq(cr *criteria) bool {
	if cr.modseq != 0 {
		return true
	}

	for _, n := range cr.not {
		if hasModseq(n) {
			return true
		}
	}

	for _, o := range cr.or {
		if hasModseq(o[0]) || hasModseq(o[1]) {
			return true
		}
	}

	return false
}

// badCharset makes the server answer NO [BADCHARSET (UTF-8)].
type badCharset struct{ name string }

func (e *badCharset) Error() string { return "unsupported charset " + e.name }

func (c *conn) parseSearchCommand() (searchReturn, *criteria, error) {
	var (
		ret     searchReturn
		charset *encoding.Decoder
	)

	c.r.skipSpaces()

	for {
		save := c.r.pos
		if c.r.peek() == '(' { // a parenthesised key list starts the criteria
			break
		}

		kw, err := c.r.atom()
		if err != nil {
			return ret, nil, err
		}

		switch strings.ToUpper(kw) {
		case "RETURN":
			if err := c.parseSearchReturn(&ret); err != nil {
				return ret, nil, err
			}

			continue
		case "CHARSET":
			dec, err := c.readSearchCharset()
			if err != nil {
				return ret, nil, err
			}

			charset = dec

			continue
		}

		c.r.pos = save

		break
	}

	cr, err := c.parseSearchKeys(c.r, 0)
	if err == nil && charset != nil {
		convertCriteria(cr, charset)
	}

	return ret, cr, err
}

// parseSearchReturn parses the RETURN (options) of an extended SEARCH.
func (c *conn) parseSearchReturn(ret *searchReturn) error {
	t, err := c.r.next()
	if err != nil {
		return err
	}

	ret.esearch = true
	if len(t.list) == 0 {
		ret.all = true
	}

	for _, o := range t.list {
		switch strings.ToUpper(o.str) {
		case "MIN":
			ret.min = true
		case "MAX":
			ret.max = true
		case kwAll:
			ret.all = true
		case "COUNT":
			ret.count = true
		case "SAVE":
			ret.save = true
		}
	}

	return nil
}

// readSearchCharset reads the CHARSET argument and returns its decoder
// (nil for UTF-8/US-ASCII); an unknown charset is a badCharset error.
func (c *conn) readSearchCharset() (*encoding.Decoder, error) {
	cs, err := c.r.astring()
	if err != nil {
		return nil, err
	}

	dec, err := charsetDecoder(cs)
	if err != nil {
		return nil, &badCharset{cs}
	}

	return dec, nil
}

// charsetDecoder returns nil for UTF-8/US-ASCII (nothing to do) and a
// decoder for every other IANA charset Go knows.
func charsetDecoder(name string) (*encoding.Decoder, error) {
	switch strings.ToUpper(name) {
	case "UTF-8", "UTF8", "US-ASCII", "ASCII":
		return nil, nil
	}

	enc, err := htmlindex.Get(name)
	if err != nil {
		return nil, err
	}

	return enc.NewDecoder(), nil
}

// convertCriteria decodes all text values of the criteria tree to UTF-8.
func convertCriteria(cr *criteria, dec *encoding.Decoder) {
	for i := range cr.text {
		if v, err := dec.String(cr.text[i].value); err == nil {
			cr.text[i].value = v
		}
	}

	for _, n := range cr.not {
		convertCriteria(n, dec)
	}

	for _, or := range cr.or {
		convertCriteria(or[0], dec)
		convertCriteria(or[1], dec)
	}
}

// headerFieldContains checks the header lines that belong to an fts field
// (subject, from-like, to-like, or any) for a substring.
func headerFieldContains(hdr, field, v string) bool {
	var names []string

	switch field {
	case fts.FieldSubject:
		names = []string{"subject:"}
	case fts.FieldFrom:
		names = []string{"from:", "sender:", "reply-to:"}
	case fts.FieldTo:
		names = []string{"to:", "cc:", "bcc:"}
	default:
		return strings.Contains(hdr, v)
	}

	for _, line := range strings.Split(hdr, "\r\n") {
		for _, n := range names {
			if strings.HasPrefix(line, n) && strings.Contains(line[len(n):], v) {
				return true
			}
		}
	}

	return false
}

func dateOnly(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// matchPlain checks the criteria that need no message content, in order:
// sets, flags, size, \Recent, WITHIN, dates, modseq and object ids.
func (c *conn) matchPlain(seq int, m *model.Meta, cr *criteria) bool {
	return c.matchSets(seq, m, cr) && matchFlags(m, cr) && matchSize(m, cr) && c.matchAge(m, cr) &&
		matchDates(m, cr) && matchSentDates(m, cr) && matchIDs(m, cr)
}

// matchSets checks the sequence and UID set criteria.
func (c *conn) matchSets(seq int, m *model.Meta, cr *criteria) bool {
	for _, s := range cr.seqSets {
		if !s.contains(uint32(seq), uint32(len(c.msgs))) {
			return false
		}
	}

	for _, s := range cr.uidSets {
		if !s.contains(m.UID, c.lastUID()) {
			return false
		}
	}

	return true
}

// matchFlags checks the flag criteria.
func matchFlags(m *model.Meta, cr *criteria) bool {
	for _, f := range cr.flags {
		if !model.HasFlag(m.Flags, f) {
			return false
		}
	}

	for _, f := range cr.notFlags {
		if model.HasFlag(m.Flags, f) {
			return false
		}
	}

	return true
}

// matchSize checks the LARGER and SMALLER criteria.
func matchSize(m *model.Meta, cr *criteria) bool {
	if cr.larger != 0 && m.Size <= cr.larger {
		return false
	}

	if cr.smaller != 0 && m.Size >= cr.smaller {
		return false
	}

	return true
}

// matchAge checks the \Recent and WITHIN criteria.
func (c *conn) matchAge(m *model.Meta, cr *criteria) bool {
	if cr.recent == 1 && !c.isRecent(m) {
		return false
	}

	if cr.recent == -1 && c.isRecent(m) {
		return false
	}

	if cr.younger != 0 && time.Since(m.InternalDate) > time.Duration(cr.younger)*time.Second {
		return false
	}

	if cr.older != 0 && time.Since(m.InternalDate) < time.Duration(cr.older)*time.Second {
		return false
	}

	return true
}

// matchDates checks the internal date criteria.
func matchDates(m *model.Meta, cr *criteria) bool {
	d := dateOnly(m.InternalDate)
	if !cr.since.IsZero() && d.Before(cr.since) {
		return false
	}

	if !cr.before.IsZero() && !d.Before(cr.before) {
		return false
	}

	return true
}

// matchSentDates checks the sent date criteria.
func matchSentDates(m *model.Meta, cr *criteria) bool {
	if cr.sentSince.IsZero() && cr.sentBefore.IsZero() {
		return true
	}

	if m.Envelope == nil || m.Envelope.Date.IsZero() {
		return false
	}

	sd := dateOnly(m.Envelope.Date)
	if !cr.sentSince.IsZero() && sd.Before(cr.sentSince) {
		return false
	}

	if !cr.sentBefore.IsZero() && !sd.Before(cr.sentBefore) {
		return false
	}

	return true
}

// matchIDs checks the MODSEQ, EMAILID and THREADID criteria.
func matchIDs(m *model.Meta, cr *criteria) bool {
	if cr.modseq != 0 && m.ModSeq < cr.modseq {
		return false
	}

	for _, id := range cr.emailIDs {
		if m.GUID != id {
			return false
		}
	}

	for _, id := range cr.threadIDs {
		if threadID(m) != id {
			return false
		}
	}

	return true
}

// envAddrContains matches an address search key against the parsed
// addresses (comments and folding removed, "name <mailbox@host>"), the
// form clients expect "FROM user@host" to hit even with obsolete syntax.
// field is an fts field (FROM/TO search keys) or a lower-case header
// name (HEADER <name>), which selects that header only.
func envAddrContains(m *model.Meta, field, v string) bool {
	if m.Envelope == nil {
		return false
	}

	sel, ok := envAddrFields[field]
	if !ok {
		return false
	}

	for _, l := range sel(m.Envelope) {
		for _, a := range l {
			if a.Host == "" {
				continue
			}

			s := strings.ToLower(a.Mailbox + "@" + a.Host)
			if a.Name != "" {
				s = strings.ToLower(a.Name) + " <" + s + ">"
			}

			if strings.Contains(s, v) {
				return true
			}
		}
	}

	return false
}

// envAddrFields select the envelope address lists of an fts field or a
// lower-case header name.
var envAddrFields = map[string]func(e *imap.Envelope) [][]imap.Address{
	fts.FieldFrom: func(e *imap.Envelope) [][]imap.Address { return [][]imap.Address{e.From, e.Sender, e.ReplyTo} },
	fts.FieldTo:   func(e *imap.Envelope) [][]imap.Address { return [][]imap.Address{e.To, e.Cc, e.Bcc} },
	hdrFrom:       func(e *imap.Envelope) [][]imap.Address { return [][]imap.Address{e.From} },
	"sender":      func(e *imap.Envelope) [][]imap.Address { return [][]imap.Address{e.Sender} },
	"reply-to":    func(e *imap.Envelope) [][]imap.Address { return [][]imap.Address{e.ReplyTo} },
	hdrTo:         func(e *imap.Envelope) [][]imap.Address { return [][]imap.Address{e.To} },
	hdrCc:         func(e *imap.Envelope) [][]imap.Address { return [][]imap.Address{e.Cc} },
	"bcc":         func(e *imap.Envelope) [][]imap.Address { return [][]imap.Address{e.Bcc} },
}
