package interp

import (
	"context"
	"encoding/gob"
	"fmt"
	"mime"
	"strings"

	"github.com/emersion/go-message/mail"
)

type Test interface {
	Check(ctx context.Context, d *RuntimeData) (bool, error)
}

type AddressTest struct {
	matcherTest

	AddressPart AddressPart
	Header      []string
	Index       int  // :index (RFC 5260 6), 0 = all instances
	Last        bool // :last
	Mime        mimeOpts
}

// pickIndex applies :index/:last to the instances of one header field.
func pickIndex(values []string, index int, last bool) []string {
	if index <= 0 {
		return values
	}
	if index > len(values) {
		return nil
	}
	if last {
		return values[len(values)-index : len(values)-index+1]
	}
	return values[index-1 : index]
}

// indexTags adds :index and :last when the index extension is active.
func indexTags(s *Script, spec *Spec, index *int, last *bool) {
	if !s.RequiresExtension("index") {
		return
	}
	spec.Tags["index"] = SpecTag{NeedsValue: true, MatchNum: func(v int) { *index = v }}
	spec.Tags["last"] = SpecTag{MatchBool: func() { *last = true }}
}

var allowedAddrHeaders = map[string]struct{}{
	// Required by Sieve.
	"from":        {},
	"to":          {},
	"cc":          {},
	"bcc":         {},
	"sender":      {},
	"resent-from": {},
	"resent-to":   {},
	// Misc (RFC 2822)
	"reply-to":        {},
	"resent-reply-to": {},
	"resent-sender":   {},
	"resent-cc":       {},
	"resent-bcc":      {},
	// Non-standard (RFC 2076, draft-palme-mailext-headers-08.txt)
	"for-approval":                       {},
	"for-handling":                       {},
	"for-comment":                        {},
	"apparently-to":                      {},
	"errors-to":                          {},
	"delivered-to":                       {},
	"return-receipt-to":                  {},
	"x-admin":                            {},
	"read-receipt-to":                    {},
	"x-confirm-reading-to":               {},
	"return-receipt-requested":           {},
	"registered-mail-reply-requested-by": {},
	"mail-followup-to":                   {},
	"mail-reply-to":                      {},
	"abuse-reports-to":                   {},
	"x-complaints-to":                    {},
	"x-report-abuse-to":                  {},
	"x-beenthere":                        {},
	"x-original-from":                    {},
	"x-original-to":                      {},
}

func (a AddressTest) Check(_ context.Context, d *RuntimeData) (bool, error) {
	if !a.regexKeysValid(d) {
		return false, nil
	}
	entryCount := uint64(0)
	for _, hdr := range a.Header {
		hdr = strings.ToLower(hdr)
		hdr = expandVars(d, hdr)

		if _, ok := allowedAddrHeaders[hdr]; !ok {
			continue
		}

		values, err := a.Mime.headerValues(d, hdr)
		if err != nil {
			return false, err
		}

		for _, value := range pickIndex(values, a.Index, a.Last) {
			addrList, err := mail.ParseAddressList(stripComments(value))
			if err != nil {
				return false, nil
			}

			for _, addr := range addrList {
				if a.isCount() {
					entryCount++
					continue
				}

				ok, err := testAddress(d, a.matcherTest, a.AddressPart, addr.Address)
				if err != nil {
					return false, err
				}
				if ok {
					return true, nil
				}
			}
		}
	}

	if a.isCount() {
		return a.countMatches(d, entryCount), nil
	}

	return false, nil
}

type AllOfTest struct {
	Tests []Test
}

func (a AllOfTest) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	for _, t := range a.Tests {
		ok, err := t.Check(ctx, d)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

type AnyOfTest struct {
	Tests []Test
}

func (a AnyOfTest) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	for _, t := range a.Tests {
		ok, err := t.Check(ctx, d)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

type EnvelopeTest struct {
	matcherTest

	AddressPart AddressPart
	Field       []string
}

func (e EnvelopeTest) Check(_ context.Context, d *RuntimeData) (bool, error) {
	if d.Script.Options().Conf("imapsieve_context") == "yes" {
		return false, fmt.Errorf("envelope test unavailable in IMAP context")
	}
	if !e.regexKeysValid(d) {
		return false, nil
	}
	entryCount := uint64(0)
	for _, field := range e.Field {
		var value string
		switch strings.ToLower(expandVars(d, field)) {
		case "from":
			value = d.Envelope.EnvelopeFrom()
		case "to":
			value = d.Envelope.EnvelopeTo()
		case "auth":
			value = d.Envelope.AuthUsername()
		default:
			return false, fmt.Errorf("envelope: unsupported envelope-part: %v", field)
		}
		if strings.ToLower(expandVars(d, field)) != "auth" {
			norm, ok := normalizeEnvelopePath(value)
			if !ok {
				continue // RFC 5228 5.4: a syntactically invalid path matches nothing
			}
			value = norm
		}
		if e.isCount() {
			if value != "" {
				entryCount++
			}
			continue
		}

		ok, err := testAddress(d, e.matcherTest, e.AddressPart, value)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	if e.isCount() {
		return e.countMatches(d, entryCount), nil
	}
	return false, nil
}

type ExistsTest struct {
	Fields []string
	Mime   mimeOpts
}

func (e ExistsTest) Check(_ context.Context, d *RuntimeData) (bool, error) {
	for _, field := range e.Fields {
		values, err := e.Mime.headerValues(d, expandVars(d, field))
		if err != nil {
			return false, err
		}
		if len(values) == 0 {
			return false, nil
		}
	}
	return true, nil
}

type FalseTest struct{}

func (f FalseTest) Check(context.Context, *RuntimeData) (bool, error) {
	return false, nil
}

type TrueTest struct{}

func (t TrueTest) Check(context.Context, *RuntimeData) (bool, error) {
	return true, nil
}

type HeaderTest struct {
	matcherTest

	Header []string
	Index  int
	Last   bool
	Mime   mimeOpts
}

func (h HeaderTest) Check(_ context.Context, d *RuntimeData) (bool, error) {
	if !h.regexKeysValid(d) {
		return false, nil
	}
	entryCount := uint64(0)
	for _, hdr := range h.Header {
		values, err := h.Mime.headerValues(d, expandVars(d, hdr))
		if err != nil {
			return false, err
		}

		for _, value := range pickIndex(values, h.Index, h.Last) {
			if h.isCount() {
				entryCount++
				continue
			}

			ok, err := h.matcherTest.tryMatch(d, decodeHeaderValue(value))
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
	}

	if h.isCount() {
		return h.countMatches(d, entryCount), nil
	}

	return false, nil
}

type NotTest struct {
	Test Test
}

func (n NotTest) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	ok, err := n.Test.Check(ctx, d)
	if err != nil {
		return false, err
	}
	return !ok, nil
}

type SizeTest struct {
	Size  int
	Over  bool
	Under bool
}

func (s SizeTest) Check(_ context.Context, d *RuntimeData) (bool, error) {
	if s.Over && d.Msg.MessageSize() > s.Size {
		return true, nil
	}
	if s.Under && d.Msg.MessageSize() < s.Size {
		return true, nil
	}
	return false, nil
}

// EnvironmentTest implements the Sieve environment test (RFC 5183).
// It checks the value of a named environment item against a key list.
type EnvironmentTest struct {
	matcherTest
	Name []string // The environment item name(s) to test
}

func (e EnvironmentTest) Check(_ context.Context, d *RuntimeData) (bool, error) {
	entryCount := uint64(0)
	anyKnown := false
	for _, name := range e.Name {
		name = strings.ToLower(expandVars(d, name))

		var value string
		if d.Env != nil {
			v, ok := d.Env.GetEnvironment(name)
			if !ok {
				// RFC 5183 §4: MUST fail unconditionally for unsupported items.
				// For :count, unsupported items contribute 0 but we only count
				// known items; if NO names are known, return false unconditionally.
				continue
			}
			anyKnown = true
			value = v
		} else {
			// No environment provider: treat all items as unsupported.
			continue
		}

		if e.isCount() {
			if value != "" {
				entryCount++
			}
			continue
		}

		ok, err := e.matcherTest.tryMatch(d, value)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}

	if e.isCount() {
		// If none of the named items are known, fail unconditionally per RFC 5183 §4.
		if !anyKnown {
			return false, nil
		}
		return e.countMatches(d, entryCount), nil
	}
	return false, nil
}

type HasFlagTest struct {
	matcherTest
	Variables []string
}

func (h HasFlagTest) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	if h.isCount() {
		count := uint64(0)
		if len(h.Variables) == 0 {
			count += uint64(len(d.Flags))
		}
		for _, v := range h.Variables {
			value, err := d.Var(v)
			if err != nil {
				return false, err
			}

			varFlags := canonicalFlags(strings.Fields(value), nil, d.FlagAliases)
			count += uint64(len(varFlags))
		}

		return h.countMatches(d, count), nil
	}

	if len(h.Variables) == 0 {
		for _, internalFlag := range d.Flags {
			ok, err := h.tryMatch(d, internalFlag)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
	}
	for _, v := range h.Variables {
		value, err := d.Var(v)
		if err != nil {
			return false, err
		}

		varFlags := canonicalFlags(strings.Fields(value), nil, d.FlagAliases)
		for _, varFlag := range varFlags {
			ok, err := h.tryMatch(d, varFlag)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
	}
	return false, nil
}

// BodyTransform specifies how the body is extracted for matching (RFC 5173 §5).
type BodyTransform string

const (
	BodyTransformRaw     BodyTransform = "raw"
	BodyTransformText    BodyTransform = "text"
	BodyTransformContent BodyTransform = "content"
)

// BodyTest implements the body test from RFC 5173.
type BodyTest struct {
	matcherTest

	// Transform is the body transform: raw, text, or content.
	Transform BodyTransform
	// ContentTypes is the list of content-type patterns for :content transform.
	ContentTypes []string
}

func (b BodyTest) Check(ctx context.Context, d *RuntimeData) (bool, error) {
	bm, ok := d.Msg.(BodyMessage)
	if !ok {
		// No body access: return false for all body tests
		return false, nil
	}

	var parts []BodyPart

	var err error
	switch b.Transform {
	case BodyTransformRaw:
		parts = append(parts, BodyPartRaw{BodyMessage: bm})
	case BodyTransformContent:
		parts, err = bm.BodyParts(ctx, b.ContentTypes)
	case BodyTransformText:
		// RFC 5173 §5.3: :text is implementation's best effort at extracting
		// UTF-8 text. Simple implementations MAY treat it as :content "text".
		// Sophisticated ones MAY strip markup.
		// We use :content "text" with HTML stripping applied to all parts.
		// Applying stripHTMLTags to plain text is a no-op, so this is safe.
		parts, err = bm.BodyParts(ctx, []string{"text", "application/xhtml+xml"})
	}
	if err != nil {
		return false, err
	}

	if len(parts) == 0 {
		if b.isCount() {
			return b.countMatches(d, 0), nil
		}
		return false, nil
	}

	if b.isCount() {
		return b.countMatches(d, uint64(len(parts))), nil
	}

	for _, part := range parts {
		stripHTML := false
		ct := strings.ToLower(part.ContentType())
		if b.Transform == BodyTransformText && (strings.HasPrefix(ct, "text/html") ||
			strings.HasPrefix(ct, "application/xhtml")) {
			stripHTML = true
		}

		ok, err := b.matcherTest.tryMatchBodyPart(ctx, d, part, stripHTML)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}

	return false, nil
}

func init() {
	gob.Register(AddressTest{})
	gob.Register(AllOfTest{})
	gob.Register(AnyOfTest{})
	gob.Register(BodyTest{})
	gob.Register(EnvelopeTest{})
	gob.Register(EnvironmentTest{})
	gob.Register(ExistsTest{})
	gob.Register(FalseTest{})
	gob.Register(TrueTest{})
	gob.Register(HeaderTest{})
	gob.Register(NotTest{})
	gob.Register(SizeTest{})
	gob.Register(HasFlagTest{})
}

var headerDecoder = mime.WordDecoder{}

// decodeHeaderValue decodes RFC 2047 encoded words (RFC 5228 2.7.2:
// comparisons are made on the decoded text); undecodable input is used
// as it is.
func decodeHeaderValue(v string) string {
	if !strings.Contains(v, "=?") {
		return v
	}
	if d, err := headerDecoder.DecodeHeader(v); err == nil {
		return d
	}
	return v
}

// validEnvelopePath accepts the null path, a bare addr-spec or an
// angle-addr with an optional source route; unbalanced brackets or a
// missing local part or domain are invalid.
func normalizeEnvelopePath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || p == "<>" {
		return "", true
	}
	if strings.HasPrefix(p, "<") {
		if !strings.HasSuffix(p, ">") {
			return "", false
		}
		p = p[1 : len(p)-1]
		if i := strings.Index(p, ":"); i >= 0 && !strings.HasPrefix(p, "\"") { // source route "@a,@b:"
			for _, hop := range strings.Split(p[:i], ",") {
				if !strings.HasPrefix(hop, "@") || !validDomain(hop[1:]) {
					return "", false
				}
			}
			p = p[i+1:]
		}
		if strings.ContainsAny(p, "<>,") {
			return "", false
		}
		if !strings.Contains(p, "@") { // local part only (RFC 5321 4.1.2 allows it for e.g. MAILER-DAEMON)
			if p == "" {
				return "", false
			}
			return p, true
		}
	}
	if strings.ContainsAny(p, "<>") {
		return "", false
	}
	local, domain, ok := strings.Cut(p, "@")
	if !ok || local == "" || !validDomain(domain) {
		return "", false
	}
	return p, true
}

func validDomain(dom string) bool {
	if dom == "" || strings.Contains(dom, "@") {
		return false
	}
	if strings.HasPrefix(dom, "[") && strings.HasSuffix(dom, "]") {
		return len(dom) > 2
	}
	for _, label := range strings.Split(dom, ".") {
		if label == "" {
			return false
		}
	}
	return true
}

// stripComments removes RFC 5322 comments outside quoted strings (Go's
// address parser rejects comments inside an addr-spec).
func stripComments(v string) string {
	var b strings.Builder
	depth, quoted := 0, false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case quoted:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(v) {
				i++
				b.WriteByte(v[i])
			} else if c == '"' {
				quoted = false
			}
		case c == '"' && depth == 0:
			quoted = true
			b.WriteByte(c)
		case c == '(':
			depth++
		case c == ')' && depth > 0:
			depth--
		case depth == 0:
			b.WriteByte(c)
		}
	}
	return b.String()
}
