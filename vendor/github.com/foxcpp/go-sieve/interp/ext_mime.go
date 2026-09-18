package interp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/gob"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/textproto"
	"strings"

	"github.com/foxcpp/go-sieve/parser"
)

// foreverypart, mime and extracttext (RFC 5703 §3, §4, §7), shardpost
// fork. replace and enclose (§5, §6) are not implemented.

// MimePart is one part of the message's MIME tree.
type MimePart struct {
	Header   textproto.MIMEHeader
	Body     []byte // undecoded body
	Children []*MimePart
}

// RawMessage is implemented by messages that can hand out their raw bytes
// (needed for the MIME extensions).
type RawMessage interface{ Raw() []byte }

// Raw implements RawMessage.
func (m MessageStatic) Raw() []byte { return m.RawMessage }

// MIME resource limits bound tree allocation and recursion independently of
// the script's foreverypart loop depth. Bodies remain slices of the input.
const maxMimeParts = 1000
const maxMimeDepth = 20
const maxMimeHeaderBytes = 256 << 10

// ParseMime retains the legacy API; the interpreter uses parseMimeBudget and
// propagates errors instead of acting on a truncated tree.
func ParseMime(raw []byte) *MimePart {
	p, _ := parseMimeBudget(context.Background(), raw)
	return p
}
func parseMimeBudget(ctx context.Context, raw []byte) (*MimePart, error) {
	remaining := maxMimeParts
	return parsePartBudget(ctx, raw, 0, &remaining)
}
func parsePartBudget(ctx context.Context, raw []byte, depth int, remaining *int) (*MimePart, error) {
	p := &MimePart{Header: textproto.MIMEHeader{}}
	if err := ctx.Err(); err != nil {
		return p, err
	}
	if depth > maxMimeDepth || *remaining <= 0 {
		return p, fmt.Errorf("MIME structure limit exceeded")
	}
	*remaining--
	sep := bytes.Index(raw, []byte("\r\n\r\n"))
	n := 4
	if i := bytes.Index(raw, []byte("\n\n")); i >= 0 && (sep < 0 || i < sep) {
		sep, n = i, 2
	}
	head := raw
	if sep >= 0 {
		head = raw[:sep]
		p.Body = raw[sep+n:]
	}
	if len(head) > maxMimeHeaderBytes {
		return p, fmt.Errorf("MIME header limit exceeded")
	}
	p.Header = parseTestMessage(string(head))
	ct, params, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
	if err != nil {
		ct = "text/plain"
	}
	add := func(sub []byte) error {
		child, err := parsePartBudget(ctx, sub, depth+1, remaining)
		if err != nil {
			return err
		}
		p.Children = append(p.Children, child)
		return nil
	}
	switch {
	case strings.HasPrefix(ct, "multipart/") && params["boundary"] != "":
		marker := []byte("--" + params["boundary"])
		closing := append(append([]byte(nil), marker...), '-', '-')
		start := -1
		for off := 0; off < len(p.Body); {
			if err := ctx.Err(); err != nil {
				return p, err
			}
			end := off + bytes.IndexByte(p.Body[off:], '\n') + 1
			if end <= off {
				end = len(p.Body)
			}
			line := bytes.TrimRight(p.Body[off:end], "\r\n")
			closed := bytes.Equal(line, closing)
			if bytes.Equal(line, marker) || closed {
				if start >= 0 {
					sub := bytes.TrimSuffix(bytes.TrimSuffix(p.Body[start:off], []byte("\n")), []byte("\r"))
					if err := add(sub); err != nil {
						return p, err
					}
				}
				start = end
				if closed {
					return p, nil
				}
			}
			off = end
		}
	case ct == "message/rfc822":
		if err := add(p.Body); err != nil {
			return p, err
		}
	}
	return p, nil
}

// walk visits p and its descendants in pre-order.
func (p *MimePart) walk(fn func(*MimePart)) {
	fn(p)
	for _, c := range p.Children {
		c.walk(fn)
	}
}

// descendants are all parts below p in pre-order.
func (p *MimePart) descendants() []*MimePart {
	var out []*MimePart
	for _, c := range p.Children {
		c.walk(func(x *MimePart) { out = append(out, x) })
	}
	return out
}

// mimeRoot returns the MIME tree of the current message (parsed on
// demand, cached per message bytes).
func (d *RuntimeData) mimeRoot() *MimePart {
	rm, ok := d.Msg.(RawMessage)
	if !ok {
		return &MimePart{Header: textproto.MIMEHeader{}}
	}
	raw := rm.Raw()
	if d.mimeCache != nil && d.mimeCacheLen == len(raw) && (len(raw) == 0 || &raw[0] == d.mimeCachePtr) {
		return d.mimeCache
	}
	d.mimeCache, d.mimeErr = parseMimeBudget(d.executionContext(), raw)
	d.mimeCacheLen = len(raw)
	if len(raw) > 0 {
		d.mimeCachePtr = &raw[0]
	}
	return d.mimeCache
}

// currentPart is the part a foreverypart loop is at (the whole message
// outside a loop).
func (d *RuntimeData) currentPart() *MimePart {
	if n := len(d.partStack); n > 0 {
		return d.partStack[n-1]
	}
	return d.mimeRoot()
}

// --- foreverypart / break --------------------------------------------------

type breakError struct{ name string }

func (e breakError) Error() string { return "break " + e.name }

type CmdForEveryPart struct {
	Name  string
	Block []Cmd
}

const maxPartLoopDepth = 4

func (c CmdForEveryPart) Execute(ctx context.Context, d *RuntimeData) error {
	if err := d.checkExecution(); err != nil {
		return err
	}
	if len(d.partStack) >= maxPartLoopDepth {
		return fmt.Errorf("foreverypart nesting limit exceeded")
	}
	var parts []*MimePart
	if len(d.partStack) == 0 {
		d.mimeRoot().walk(func(p *MimePart) { parts = append(parts, p) })
	} else {
		parts = d.currentPart().descendants()
	}
	for _, p := range parts {
		if err := d.checkExecution(); err != nil {
			return err
		}
		d.partStack = append(d.partStack, p)
		err := runBlock(ctx, c.Block, d)
		d.partStack = d.partStack[:len(d.partStack)-1]
		if err != nil {
			if be, ok := err.(breakError); ok && (be.name == "" || be.name == c.Name) {
				return nil
			}
			return err
		}
	}
	return nil
}

func runBlock(ctx context.Context, block []Cmd, d *RuntimeData) error {
	for _, cmd := range block {
		if err := d.checkExecution(); err != nil {
			return err
		}
		if err := cmd.Execute(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func loadForEveryPart(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("foreverypart") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'foreverypart'")
	}
	cmd := CmdForEveryPart{}
	if s.loopDepth >= maxPartLoopDepth {
		return nil, parser.ErrorAt(pcmd.Position, "foreverypart nesting limit exceeded")
	}
	s.loopDepth++
	defer func() { s.loopDepth-- }()
	err := LoadSpec(s, &Spec{
		Tags:     map[string]SpecTag{"name": {NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, NoVariables: true, MatchStr: func(v []string) { cmd.Name = v[0] }}},
		AddBlock: func(cmds []Cmd) { cmd.Block = cmds },
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	return cmd, err
}

type CmdBreak struct{ Name string }

func (c CmdBreak) Execute(context.Context, *RuntimeData) error { return breakError{c.Name} }

func loadBreak(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("foreverypart") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'foreverypart'")
	}
	if s.loopDepth == 0 {
		return nil, parser.ErrorAt(pcmd.Position, "break outside foreverypart")
	}
	cmd := CmdBreak{}
	err := LoadSpec(s, &Spec{
		Tags: map[string]SpecTag{"name": {NeedsValue: true, MinStrCount: 1, MaxStrCount: 1, NoVariables: true, MatchStr: func(v []string) { cmd.Name = v[0] }}},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	return cmd, err
}

// --- :mime for header, address, exists -------------------------------------

// mimeOpts are the RFC 5703 §4 options of the header/address/exists tests.
type mimeOpts struct {
	Mime     bool
	AnyChild bool
	Opt      string // "", "type", "subtype", "contenttype", "param"
	Params   []string
}

// mimeTags registers :mime, :anychild and the MIME option tags when the
// mime extension is active. exists supports only :mime/:anychild.
func mimeTags(s *Script, spec *Spec, o *mimeOpts, withOpts bool) {
	if !s.RequiresExtension("mime") {
		return
	}
	spec.Tags["mime"] = SpecTag{MatchBool: func() { o.Mime = true }}
	spec.Tags["anychild"] = SpecTag{MatchBool: func() { o.AnyChild = true }}
	if !withOpts {
		return
	}
	spec.Tags["type"] = SpecTag{MatchBool: func() { o.Opt = "type" }}
	spec.Tags["subtype"] = SpecTag{MatchBool: func() { o.Opt = "subtype" }}
	spec.Tags["contenttype"] = SpecTag{MatchBool: func() { o.Opt = "contenttype" }}
	spec.Tags["param"] = SpecTag{NeedsValue: true, MinStrCount: 1, MatchStr: func(v []string) { o.Opt = "param"; o.Params = v }}
}

func (o *mimeOpts) validate(pos parser.Cmd) error { return nil }

// headerValues returns the values of a header for a test: the message
// header, or with :mime the current part's (and with :anychild its
// descendants'), transformed by the MIME option.
func (o mimeOpts) headerValues(d *RuntimeData, name string) ([]string, error) {
	if !o.Mime {
		return d.Msg.HeaderGet(name)
	}
	parts := []*MimePart{d.currentPart()}
	if o.AnyChild {
		parts = append(parts, d.currentPart().descendants()...)
	}
	var out []string
	for _, p := range parts {
		if err := d.checkExecution(); err != nil {
			return nil, err
		}
		for _, v := range p.Header.Values(name) {
			out = append(out, o.transform(v)...)
		}
	}
	return out, nil
}

func (o mimeOpts) transform(v string) []string {
	if o.Opt == "" {
		return []string{v}
	}
	ct, params, err := mime.ParseMediaType(v)
	if err != nil {
		ct = strings.TrimSpace(strings.ToLower(strings.SplitN(v, ";", 2)[0]))
	}
	typ, sub, _ := strings.Cut(ct, "/")
	switch o.Opt {
	case "type":
		return []string{typ}
	case "subtype":
		return []string{sub}
	case "contenttype":
		return []string{ct}
	case "param":
		var out []string
		for _, name := range o.Params {
			if pv, ok := params[strings.ToLower(name)]; ok {
				out = append(out, pv)
			}
		}
		return out
	}
	return []string{v}
}

// --- extracttext -----------------------------------------------------------

type CmdExtractText struct {
	First     int
	Modifiers []string
	Name      string
}

func (c CmdExtractText) Execute(_ context.Context, d *RuntimeData) error {
	text := PartText(d.currentPart())
	if c.First > 0 {
		r := []rune(text)
		if len(r) > c.First {
			text = string(r[:c.First])
		}
	}
	return d.SetVar(c.Name, applySetModifiers(d.Script.opts.MaxVariableLen, c.Modifiers, text))
}

// PartText is the decoded text of a leaf part: text/plain as is, text/html
// with the markup stripped, anything else empty.
func PartText(p *MimePart) string {
	if p == nil || len(p.Children) > 0 {
		return ""
	}
	ct, _, err := mime.ParseMediaType(p.Header.Get("Content-Type"))
	if err != nil {
		ct = "text/plain"
	}
	if !strings.HasPrefix(ct, "text/") {
		return ""
	}
	body := p.Body
	switch strings.ToLower(strings.TrimSpace(p.Header.Get("Content-Transfer-Encoding"))) {
	case "base64":
		if b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(body)), "")); err == nil {
			body = b
		}
	case "quoted-printable":
		if b, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body))); err == nil {
			body = b
		}
	}
	text := string(body)
	if ct == "text/html" || ct == "application/xhtml+xml" {
		text = stripHTMLTags(text)
	}
	return text
}

// stripHTMLTags drops tags and decodes the common entities.
func stripHTMLTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	out := b.String()
	for k, v := range map[string]string{"&lt;": "<", "&gt;": ">", "&amp;": "&", "&quot;": "\"", "&nbsp;": " "} {
		out = strings.ReplaceAll(out, k, v)
	}
	return out
}

func loadExtractText(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("extracttext") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'extracttext'")
	}
	if !s.RequiresExtension("variables") {
		return nil, parser.ErrorAt(pcmd.Position, "extracttext requires 'variables'")
	}
	if s.loopDepth == 0 {
		return nil, parser.ErrorAt(pcmd.Position, "extracttext outside foreverypart")
	}
	cmd := CmdExtractText{}
	var mods []string
	mod := func(name string) SpecTag { return SpecTag{MatchBool: func() { mods = append(mods, name) }} }
	err := LoadSpec(s, &Spec{
		Tags: map[string]SpecTag{
			"first":         {NeedsValue: true, MatchNum: func(v int) { cmd.First = v }},
			"length":        mod("length"),
			"quotewildcard": mod("quotewildcard"),
			"upperfirst":    mod("upperfirst"),
			"lowerfirst":    mod("lowerfirst"),
			"upper":         mod("upper"),
			"lower":         mod("lower"),
		},
		Pos: []SpecPosArg{{MinStrCount: 1, MaxStrCount: 1, NoVariables: true, MatchStr: func(v []string) { cmd.Name = strings.ToLower(v[0]) }}},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, pcmd.Block)
	if err != nil {
		return nil, err
	}
	if settable, _ := s.IsVarUsable(cmd.Name); !settable {
		return nil, parser.ErrorAt(pcmd.Position, "extracttext: cannot set variable %q", cmd.Name)
	}
	cmd.Modifiers = mods
	return cmd, nil
}

var _ = fmt.Sprintf

func init() {
	gob.Register(CmdForEveryPart{})
	gob.Register(CmdBreak{})
	gob.Register(CmdExtractText{})
}
