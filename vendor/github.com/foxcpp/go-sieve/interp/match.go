package interp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"io"
	"regexp"
	"strings"

	"rsc.io/binaryregexp"
)

func foldASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}

func patternToRegex(pattern string, caseFold bool) string {
	result := strings.Builder{}
	if caseFold {
		result.WriteString(`(?i)`)
	}
	result.WriteRune('^')
	escaped := false
	for _, chr := range pattern {
		if !escaped {
			switch chr {
			case '\\':
				escaped = true
			case '?':
				result.WriteString(`(.)`)
			case '*':
				result.WriteString(`((?s:.*?))`)
			case '.', '+', '(', ')', '|', '[', ']', '{', '}', '^', '$':
				result.WriteRune('\\')
				fallthrough
			default:
				result.WriteRune(chr)
			}
		} else {
			switch chr {
			case '\\', '?', '*', '.', '+', '(', ')', '|', '[', ']', '{', '}', '^', '$':
				result.WriteRune('\\')
				fallthrough
			default:
				result.WriteRune(chr)
			}

			escaped = false
		}
	}

	// Such regex won't compile.
	if escaped {
		return result.String()
	}

	result.WriteRune('$')

	return result.String()
}

type compiledMatcherData struct {
	Regexp string
	Octet  bool
}

type CompiledMatcher struct {
	compiledMatcherData
	loaded bool
	binary *binaryregexp.Regexp
	string *regexp.Regexp
}

func (cm *CompiledMatcher) IsLoaded() bool {
	return cm.loaded
}

func (cm *CompiledMatcher) GobDecode(i []byte) error {
	err := gob.NewDecoder(bytes.NewBuffer(i)).Decode(&cm.compiledMatcherData)
	if err != nil {
		return err
	}

	return cm.restore()
}

func (cm *CompiledMatcher) restore() error {
	var err error
	if cm.Octet {
		cm.binary, err = binaryregexp.Compile(octetPattern(cm.Regexp))
	} else {
		cm.string, err = regexp.Compile(cm.Regexp)
	}
	if err != nil {
		return err
	}

	cm.loaded = true

	return nil
}

func (cm *CompiledMatcher) GobEncode() ([]byte, error) {
	var buf bytes.Buffer
	err := gob.NewEncoder(&buf).Encode(cm.compiledMatcherData)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (cm *CompiledMatcher) Match(value string) (bool, []string, error) {
	if cm.binary != nil {
		matches := cm.binary.FindStringSubmatch(value)
		return len(matches) != 0, matches, nil
	}

	matches := cm.string.FindStringSubmatch(value)
	return len(matches) != 0, matches, nil
}

func (cm *CompiledMatcher) MatchReader(r io.Reader) (bool, error) {
	if cm.binary != nil {
		br, ok := r.(io.ByteReader)
		if !ok {
			br = bufio.NewReader(r)
		}

		return cm.binary.MatchReader(br), nil
	}

	rr, ok := r.(io.RuneReader)
	if !ok {
		rr = bufio.NewReader(r)
	}

	return cm.string.MatchReader(rr), nil
}

// compileMatcher returns a function that will check whether pre-defined pattern matches the passed
// value. It is preferable to use compileMatcher over matchOctet, matchUnicode if
// pattern does not change often (e.g. does not depend on any variables).
func compileMatcher(pattern string, octet bool, caseFold bool) (CompiledMatcher, error) {
	res := CompiledMatcher{}
	res.Regexp = patternToRegex(pattern, caseFold)
	res.Octet = octet

	if err := res.restore(); err != nil {
		return CompiledMatcher{}, err
	}
	return res, nil
}

// regexKey turns a Sieve regex key into the pattern the matcher compiles:
// case folding for the case-map comparators. The regex is not anchored
// (RFC draft-ietf-sieve-regex: it matches anywhere in the value).
func regexKey(key string, caseFold bool) string {
	if caseFold {
		return "(?i)" + key
	}
	return key
}

// comppileMatcherRegex is compileMatcher that accepts regular expression as input
// instead of Sieve patterns.
func compileMatcherRegex(regex string, octet bool) (CompiledMatcher, error) {
	res := CompiledMatcher{}
	res.Regexp = regex
	res.Octet = octet

	if err := res.restore(); err != nil {
		return CompiledMatcher{}, err
	}
	return res, nil
}

func matchOctet(pattern, value string, caseFold bool) (bool, []string, error) {
	regex, err := binaryregexp.Compile(octetPattern(patternToRegex(pattern, caseFold)))
	if err != nil {
		return false, nil, err
	}

	matches := regex.FindStringSubmatch(value)
	return len(matches) != 0, matches, nil
}

func matchUnicode(pattern, value string, caseFold bool) (bool, []string, error) {
	regex, err := regexp.Compile(patternToRegex(pattern, caseFold))
	if err != nil {
		return false, nil, err
	}

	matches := regex.FindStringSubmatch(value)
	return len(matches) != 0, matches, nil
}

// contextReader checks cancellation within the regex engine, not just between
// matches. It owns no goroutine: returning an error stops the actual work.
type contextReader struct {
	ctx context.Context
	r   *bufio.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func (r *contextReader) ReadByte() (byte, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.ReadByte()
}
func (r *contextReader) ReadRune() (rune, int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, 0, err
	}
	return r.r.ReadRune()
}
func (cm *CompiledMatcher) matchContext(ctx context.Context, value string) (bool, []string, error) {
	r := &contextReader{ctx: ctx, r: bufio.NewReader(strings.NewReader(value))}
	var indexes []int
	if cm.binary != nil {
		indexes = cm.binary.FindReaderSubmatchIndex(r)
	} else {
		indexes = cm.string.FindReaderSubmatchIndex(r)
	}
	if err := ctx.Err(); err != nil {
		return false, nil, err
	}
	if indexes == nil {
		return false, nil, nil
	}
	matches := make([]string, len(indexes)/2)
	for i := range matches {
		if indexes[2*i] >= 0 {
			matches[i] = value[indexes[2*i]:indexes[2*i+1]]
		}
	}
	return true, matches, nil
}

// binaryregexp is byte-oriented; encode non-ASCII pattern literals as bytes.
func octetPattern(pattern string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		if pattern[i] >= 128 {
			fmt.Fprintf(&b, "\\x%02x", pattern[i])
		} else {
			b.WriteByte(pattern[i])
		}
	}
	return b.String()
}
