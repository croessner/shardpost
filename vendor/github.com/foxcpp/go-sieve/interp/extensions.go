package interp

import (
	"context"
	"encoding/gob"
	"fmt"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/foxcpp/go-sieve/parser"
)

// Extensions added by the shardpost fork: date (RFC 5260), duplicate
// (RFC 7352), editheader (RFC 5293) and mailbox (RFC 5490, :create and
// mailboxexists).

// --- date / currentdate ---------------------------------------------------

// DateTest is "date" (header date) or "currentdate" (Current=true).
type DateTest struct {
	matcherTest
	Current      bool
	Header       string
	Part         string
	Zone         string // "" = keep the message's zone (:originalzone) or local
	OriginalZone bool
	Index        int // :index / :last (index extension)
	Last         bool
}

var dateParts = map[string]bool{"year": true, "month": true, "day": true, "date": true, "julian": true,
	"hour": true, "minute": true, "second": true, "time": true, "iso8601": true, "std11": true, "zone": true, "weekday": true}

func loadDateTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("date") {
		return nil, parser.ErrorAt(test.Position, "missing require 'date'")
	}
	loaded := DateTest{matcherTest: newMatcherTest()}
	var key []string
	spec := loaded.addSpecTags(&Spec{
		Pos: []SpecPosArg{
			{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.Header = v[0] }},
			{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.Part = strings.ToLower(v[0]) }},
			{MinStrCount: 1, MatchStr: func(v []string) { key = v }},
		},
	})
	spec.Tags["zone"] = SpecTag{NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.Zone = v[0] }}
	spec.Tags["originalzone"] = SpecTag{MatchBool: func() { loaded.OriginalZone = true }}
	indexTags(s, spec, &loaded.Index, &loaded.Last)
	if err := LoadSpec(s, spec, test.Position, test.Args, test.Tests, nil); err != nil {
		return nil, err
	}
	if !dateParts[loaded.Part] {
		return nil, parser.ErrorAt(test.Position, "unknown date part %q", loaded.Part)
	}
	if err := loaded.setKey(s, key); err != nil {
		return nil, err
	}
	return loaded, nil
}

func loadCurrentDateTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("date") {
		return nil, parser.ErrorAt(test.Position, "missing require 'date'")
	}
	loaded := DateTest{matcherTest: newMatcherTest(), Current: true}
	var key []string
	spec := loaded.addSpecTags(&Spec{
		Pos: []SpecPosArg{
			{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.Part = strings.ToLower(v[0]) }},
			{MinStrCount: 1, MatchStr: func(v []string) { key = v }},
		},
	})
	spec.Tags["zone"] = SpecTag{NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.Zone = v[0] }}
	if err := LoadSpec(s, spec, test.Position, test.Args, test.Tests, nil); err != nil {
		return nil, err
	}
	if !dateParts[loaded.Part] {
		return nil, parser.ErrorAt(test.Position, "unknown date part %q", loaded.Part)
	}
	if err := loaded.setKey(s, key); err != nil {
		return nil, err
	}
	return loaded, nil
}

// nowFunc can be replaced in tests.
var nowFunc = time.Now

func (t DateTest) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	var times []time.Time
	if t.Current {
		times = []time.Time{nowFunc()}
	} else {
		vals, err := d.Msg.HeaderGet(expandVars(d, t.Header))
		if err != nil {
			return false, err
		}
		for _, v := range pickIndex(vals, t.Index, t.Last) {
			if i := strings.LastIndex(v, ";"); i >= 0 && strings.EqualFold(strings.TrimSpace(t.Header), "received") {
				v = v[i+1:] // RFC 5260 4.1: the date of a Received field
			}
			if tm, err := mail.ParseDate(strings.TrimSpace(v)); err == nil {
				times = append(times, tm)
			}
		}
	}
	if t.isCount() {
		return t.countMatches(d, uint64(len(times))), nil
	}
	if len(times) == 0 {
		return false, nil
	}
	loc := time.Local
	if t.Zone != "" {
		if l, ok := parseZone(expandVars(d, t.Zone)); ok {
			loc = l
		}
	}
	if t.isCount() {
		return t.countMatches(d, uint64(len(times))), nil
	}
	for _, tm := range times {
		if !t.OriginalZone || t.Current {
			tm = tm.In(loc)
		}
		ok, err := t.tryMatch(d, datePart(tm, t.Part))
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

func parseZone(z string) (*time.Location, bool) {
	if len(z) != 5 || (z[0] != '+' && z[0] != '-') {
		return nil, false
	}
	h, err1 := strconv.Atoi(z[1:3])
	m, err2 := strconv.Atoi(z[3:5])
	if err1 != nil || err2 != nil {
		return nil, false
	}
	off := h*3600 + m*60
	if z[0] == '-' {
		off = -off
	}
	return time.FixedZone(z, off), true
}

func datePart(tm time.Time, part string) string {
	switch part {
	case "year":
		return fmt.Sprintf("%04d", tm.Year())
	case "month":
		return fmt.Sprintf("%02d", int(tm.Month()))
	case "day":
		return fmt.Sprintf("%02d", tm.Day())
	case "date":
		return tm.Format("2006-01-02")
	case "julian":
		// days since 1970-01-01 UTC (RFC 5260: modified Julian day)
		return strconv.FormatInt(tm.Unix()/86400+40587, 10)
	case "hour":
		return fmt.Sprintf("%02d", tm.Hour())
	case "minute":
		return fmt.Sprintf("%02d", tm.Minute())
	case "second":
		return fmt.Sprintf("%02d", tm.Second())
	case "time":
		return tm.Format("15:04:05")
	case "iso8601":
		return tm.Format("2006-01-02T15:04:05-07:00")
	case "std11":
		return tm.Format("Mon, 02 Jan 2006 15:04:05 -0700")
	case "zone":
		return tm.Format("-0700")
	case "weekday":
		return strconv.Itoa(int(tm.Weekday()))
	}
	return ""
}

// --- duplicate ---------------------------------------------------------------

type DuplicateTest struct {
	Handle   string
	Header   string
	UniqueID string
	Seconds  int
	Last     bool
}

func loadDuplicateTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("duplicate") {
		return nil, parser.ErrorAt(test.Position, "missing require 'duplicate'")
	}
	loaded := DuplicateTest{Seconds: 7 * 86400}
	err := LoadSpec(s, &Spec{
		Tags: map[string]SpecTag{
			"handle":   {NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.Handle = v[0] }},
			"header":   {NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.Header = v[0] }},
			"uniqueid": {NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { loaded.UniqueID = v[0] }},
			"seconds":  {NeedsValue: true, MatchNum: func(v int) { loaded.Seconds = v }},
			"last":     {MatchBool: func() { loaded.Last = true }},
		},
	}, test.Position, test.Args, test.Tests, nil)
	if err != nil {
		return nil, err
	}
	if loaded.Header != "" && loaded.UniqueID != "" {
		return nil, parser.ErrorAt(test.Position, ":header and :uniqueid are exclusive")
	}
	return loaded, nil
}

func (t DuplicateTest) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	if d.Duplicate == nil {
		return false, nil
	}
	id := expandVars(d, t.UniqueID)
	if t.UniqueID == "" {
		hdr := "Message-ID"
		if t.Header != "" {
			hdr = expandVars(d, t.Header)
		}
		vals, err := d.Msg.HeaderGet(hdr)
		if err != nil {
			return false, err
		}
		if len(vals) == 0 || strings.TrimSpace(vals[0]) == "" {
			return false, nil
		}
		id = strings.TrimSpace(vals[0])
	}
	key := expandVars(d, t.Handle) + "\x00" + id
	return d.Duplicate(ctx, key, t.Seconds)
}

// --- mailboxexists -----------------------------------------------------------

type MailboxExistsTest struct{ Names []string }

func loadMailboxExistsTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("mailbox") {
		return nil, parser.ErrorAt(test.Position, "missing require 'mailbox'")
	}
	loaded := MailboxExistsTest{}
	err := LoadSpec(s, &Spec{
		Pos: []SpecPosArg{{MinStrCount: 1, MatchStr: func(v []string) { loaded.Names = v }}},
	}, test.Position, test.Args, test.Tests, nil)
	if err != nil {
		return nil, err
	}
	return loaded, nil
}

func (t MailboxExistsTest) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	if d.MailboxExists == nil {
		return false, nil
	}
	for _, n := range t.Names {
		ok, err := d.MailboxExists(ctx, expandVars(d, n))
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// --- editheader ------------------------------------------------------------

// HeaderEdit is one addheader/deleteheader outcome.
type HeaderEdit struct {
	Add   bool // false: delete
	Name  string
	Value string // addheader
	Last  bool   // addheader: append at the end; deleteheader: count :index from the end
	Index int    // deleteheader: 1-based, 0 = all matching
	Match func(value string) bool
}

// HeaderEditor is implemented by messages that can apply edits so that
// later tests in the same script see them.
type HeaderEditor interface {
	ApplyHeaderEdit(e HeaderEdit)
}

// protectedHeaders may not be edited (RFC 5293 §7 lets implementations
// restrict; these are trace and control fields).
var protectedHeaders = map[string]bool{"received": true, "auto-submitted": true}

// headerSizeLimit is sieve_editheader_max_header_size (default 2048).
func headerSizeLimit(o *Options) int {
	if v := o.Conf("sieve_editheader_max_header_size"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 2048
}

// unfoldValue turns a multi-line header value (folded with CRLF+WSP or a
// trailing line break, as "text:" arguments have) into one line.
func unfoldValue(v string) string {
	v = strings.TrimRight(v, "\r\n")
	v = strings.ReplaceAll(v, "\r\n", "\n")
	var b strings.Builder
	for i, line := range strings.Split(v, "\n") {
		if i > 0 && line != "" && line[0] != ' ' && line[0] != '\t' {
			b.WriteString(" ") // a bare line break is folded
		}
		b.WriteString(line)
	}
	return b.String()
}

// headerForbidden tells whether a header may not be added (add=true) or
// deleted: the built-in protected set plus the configuration
// (sieve_editheader_header/<name>/forbid_add|forbid_delete = yes, as in
// Pigeonhole). Subject can never be protected.
func headerForbidden(d *RuntimeData, name string, add bool) bool {
	n := strings.ToLower(name)
	if n == "subject" {
		return false
	}
	configured := d.Script.opts.Conf("sieve_editheader_header")
	if configured == "" && protectedHeaders[n] {
		return true // the built-in default list, replaced by the configuration
	}
	key := "forbid_delete"
	if add {
		key = "forbid_add"
	}
	for _, h := range strings.Fields(configured) {
		if strings.EqualFold(h, n) {
			v := strings.ToLower(d.Script.opts.Conf("sieve_editheader_header/" + h + "/" + key))
			return v == "yes" || v == "true"
		}
	}
	return false
}

type CmdAddHeader struct {
	Name, Value string
	Last        bool
}

func loadAddHeader(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("editheader") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'editheader'")
	}
	cmd := CmdAddHeader{}
	err := LoadSpec(s, &Spec{
		Tags: map[string]SpecTag{"last": {MatchBool: func() { cmd.Last = true }}},
		Pos: []SpecPosArg{
			{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.Name = v[0] }},
			{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.Value = v[0] }},
		},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	if err != nil {
		return nil, err
	}
	if len(usedVars(s, cmd.Name)) == 0 && !validHeaderName(cmd.Name) {
		return nil, parser.ErrorAt(pcmd.Position, "invalid header name %q", cmd.Name)
	}
	if len(usedVars(s, cmd.Value)) == 0 {
		v := unfoldValue(cmd.Value)
		for _, r := range v {
			if r == 0 || r == '\r' || r == '\n' {
				return nil, parser.ErrorAt(pcmd.Position, "invalid header value")
			}
		}
		if max := headerSizeLimit(s.opts); len(v) > max {
			return nil, parser.ErrorAt(pcmd.Position, "header value exceeds the size limit (%d)", max)
		}
	}
	return cmd, nil
}

func (c CmdAddHeader) Execute(ctx context.Context, d *RuntimeData) error {
	name := expandVars(d, c.Name)
	if !validHeaderName(name) {
		return fmt.Errorf("addheader: field name %q is invalid", name)
	}
	if headerForbidden(d, name, true) {
		return nil // RFC 5293 7: implementations may refuse; the edit is ignored
	}
	e := HeaderEdit{Add: true, Name: name, Value: unfoldValue(expandVars(d, c.Value)), Last: c.Last}
	if strings.ContainsAny(e.Value, "\r\n") {
		return fmt.Errorf("addheader: value must not contain line breaks")
	}
	if max := headerSizeLimit(d.Script.opts); len(e.Value) > max {
		return fmt.Errorf("addheader: header value exceeds the size limit (%d)", max)
	}
	d.HeaderEdits = append(d.HeaderEdits, e)
	if he, ok := d.Msg.(HeaderEditor); ok {
		he.ApplyHeaderEdit(e)
	}
	return nil
}

type CmdDeleteHeader struct {
	matcherTest
	Name   string
	Index  int
	Last   bool
	hasKey bool
}

func loadDeleteHeader(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("editheader") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'editheader'")
	}
	cmd := CmdDeleteHeader{matcherTest: newMatcherTest()}
	var key []string
	spec := cmd.addSpecTags(&Spec{
		Pos: []SpecPosArg{
			{MinStrCount: 1, MaxStrCount: 1, MatchStr: func(v []string) { cmd.Name = v[0] }},
			{Optional: true, MinStrCount: 1, MatchStr: func(v []string) { key = v }},
		},
	})
	spec.Tags["index"] = SpecTag{NeedsValue: true, MatchNum: func(v int) { cmd.Index = v }}
	spec.Tags["last"] = SpecTag{MatchBool: func() { cmd.Last = true }}
	if err := LoadSpec(s, spec, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block); err != nil {
		return nil, err
	}
	if len(usedVars(s, cmd.Name)) == 0 && !validHeaderName(cmd.Name) {
		return nil, parser.ErrorAt(pcmd.Position, "invalid header name %q", cmd.Name)
	}
	if cmd.Last && cmd.Index == 0 {
		return nil, parser.ErrorAt(pcmd.Position, ":last requires :index")
	}
	if len(key) > 0 {
		cmd.hasKey = true
		if err := cmd.setKey(s, key); err != nil {
			return nil, err
		}
	}
	return cmd, nil
}

func (c CmdDeleteHeader) Execute(ctx context.Context, d *RuntimeData) error {
	name := expandVars(d, c.Name)
	if !validHeaderName(name) {
		return fmt.Errorf("deleteheader: field name %q is invalid", name)
	}
	if headerForbidden(d, name, false) {
		return nil
	}
	e := HeaderEdit{Name: name, Index: c.Index, Last: c.Last}
	if c.hasKey {
		mt := c.matcherTest
		e.Match = func(v string) bool {
			ok, _ := mt.tryMatch(d, v)
			return ok
		}
	}
	d.HeaderEdits = append(d.HeaderEdits, e)
	if he, ok := d.Msg.(HeaderEditor); ok {
		he.ApplyHeaderEdit(e)
	}
	return nil
}

func validHeaderName(n string) bool {
	if n == "" {
		return false
	}
	for i := 0; i < len(n); i++ {
		ch := n[i]
		if ch < 33 || ch > 126 || ch == ':' {
			return false
		}
	}
	return true
}

func init() {
	gob.Register(DateTest{})
	gob.Register(DuplicateTest{})
	gob.Register(MailboxExistsTest{})
	gob.Register(CmdAddHeader{})
	gob.Register(CmdDeleteHeader{})
}
