package interp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// matcherTest contains code shared between tests
// such as 'header', 'address', 'envelope', 'string' -
// all tests that compare some values from message
// with pre-defined "key"
type matcherTest struct {
	Comparator Comparator
	Match      Match
	Relational Relational
	Key        []string

	// Used for keys without variables.
	keyCompiled []CompiledMatcher

	matchCnt int
}

func newMatcherTest() matcherTest {
	return matcherTest{
		Comparator: "",
		Match:      MatchIs,
	}
}

func (t *matcherTest) addSpecTags(s *Spec) *Spec {
	if s.Tags == nil {
		s.Tags = make(map[string]SpecTag, 4)
	}
	s.Tags["comparator"] = SpecTag{
		NeedsValue:  true,
		MinStrCount: 1,
		MaxStrCount: 1,
		MatchStr: func(val []string) {
			t.Comparator = Comparator(val[0])
		},
		NoVariables: true,
	}
	s.Tags["is"] = SpecTag{
		MatchBool: func() {
			t.Match = MatchIs
			t.matchCnt++
		},
	}
	s.Tags["contains"] = SpecTag{
		MatchBool: func() {
			t.Match = MatchContains
			t.matchCnt++
		},
	}
	s.Tags["matches"] = SpecTag{
		MatchBool: func() {
			t.Match = MatchMatches
			t.matchCnt++
		},
	}
	s.Tags["regex"] = SpecTag{
		MatchBool: func() {
			t.Match = MatchRegex
			t.matchCnt++
		},
	}
	s.Tags["value"] = SpecTag{
		NeedsValue:  true,
		MinStrCount: 1,
		MaxStrCount: 1,
		NoVariables: true,
		MatchStr: func(val []string) {
			t.Match = MatchValue
			t.matchCnt++
			t.Relational = Relational(val[0])
		},
	}
	s.Tags["count"] = SpecTag{
		NeedsValue:  true,
		MinStrCount: 1,
		MaxStrCount: 1,
		NoVariables: true,
		MatchStr: func(val []string) {
			t.Match = MatchCount
			t.matchCnt++
			t.Relational = Relational(val[0])
		},
	}
	return s
}

func (t *matcherTest) setKey(s *Script, k []string) error {
	t.Key = k

	if t.matchCnt > 1 {
		return fmt.Errorf("multiple match-types are not allowed")
	}

	if t.Match == MatchRegex && !s.RequiresExtension("regex") {
		return fmt.Errorf("missing require 'regex'")
	}
	if t.Match == MatchCount || t.Match == MatchValue {
		if !s.RequiresExtension("relational") {
			return fmt.Errorf("missing require 'relational'")
		}
		switch t.Relational {
		case RelGreaterThan, RelGreaterOrEqual,
			RelLessThan, RelLessOrEqual, RelEqual,
			RelNotEqual:
		default:
			return fmt.Errorf("unknown relational operator: %v", t.Relational)
		}
	}

	if t.Comparator == "" {
		if t.Match == MatchCount {
			t.Comparator = ComparatorASCIINumeric
		} else {
			t.Comparator = DefaultComparator
		}
	}

	caseFold := false
	octet := false
	switch t.Comparator {
	case ComparatorOctet, ComparatorUnicodeCaseMap, ComparatorASCIICaseMap:
		octet = t.Comparator.IsOctet()
		caseFold = t.Comparator.IsCaseMap()
	case ComparatorASCIINumeric:
		if t.Match == MatchContains || t.Match == MatchMatches || t.Match == MatchRegex {
			return fmt.Errorf("match type unavailable for i;ascii-numeric")
		}
	case "":
	default:
		return fmt.Errorf("unsupported comparator: %v", t.Comparator)
	}

	if t.Match == MatchMatches || t.Match == MatchRegex {
		t.keyCompiled = make([]CompiledMatcher, len(t.Key))
		for i := range t.Key {
			if len(usedVars(s, t.Key[i])) > 0 {
				continue
			}

			var err error
			if t.Match == MatchRegex {
				if t.Comparator == ComparatorASCIINumeric {
					return fmt.Errorf("regex cannot be used with i;ascii-numeric")
				}
				t.keyCompiled[i], err = compileMatcherRegex(regexKey(t.Key[i], caseFold), octet)
			} else {
				t.keyCompiled[i], err = compileMatcher(t.Key[i], octet, caseFold)
			}
			if err != nil {
				return fmt.Errorf("malformed pattern (%v): %v", t.Key[i], err)
			}
		}
	}

	if t.Match == MatchCount && t.Comparator != ComparatorASCIINumeric {
		return fmt.Errorf("non-numeric comparators cannot be used with :count")
	}

	return nil
}

func (t *matcherTest) isCount() bool {
	return t.Match == MatchCount
}

func (t *matcherTest) countMatches(d *RuntimeData, value uint64) bool {
	if !t.isCount() {
		panic("countMatches can be called only with MatchCount matcher")
	}

	for _, k := range t.Key {
		kNum, err := strconv.ParseUint(expandVars(d, k), 10, 64)
		if err != nil {
			continue
		}

		if t.Relational.CompareUint64(value, kNum) {
			return true
		}
	}

	return false
}

func (t *matcherTest) tryMatchBodyPart(ctx context.Context, d *RuntimeData, part BodyPart, stripHTML bool) (bool, error) {
	for i, key := range t.Key {
		partReader, err := part.Open(ctx)
		if err != nil {
			if errors.Is(err, ErrNoBody) {
				return false, nil
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return false, nil
			}
			return false, fmt.Errorf("open part: %w", err)
		}

		r := io.Reader(&contextReader{ctx: ctx, r: bufio.NewReader(partReader)})
		if stripHTML {
			br, ok := r.(io.ByteReader)
			if !ok {
				br = bufio.NewReader(r)
			}
			r = &htmlStripper{BR: br}
		}

		var ok bool
		if t.keyCompiled != nil && t.keyCompiled[i].IsLoaded() {
			ok, err = t.keyCompiled[i].MatchReader(r)
		} else {
			key = expandVars(d, key)
			ok, err = testReader(t.Comparator, t.Match, r, expandVars(d, key))
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			_ = partReader.Close()
			return false, err
		}
		if ok {
			_ = partReader.Close()
			return true, nil
		}
		_ = partReader.Close()
	}
	return false, nil
}

// regexKeysValid compiles regex keys that come from variables; an invalid
// one is a reported runtime error and makes the test false (RFC draft
// regex: "an error is reported"), the script continues.
func (t *matcherTest) regexKeysValid(d *RuntimeData) bool {
	if t.Match != MatchRegex {
		return true
	}
	for i, key := range t.Key {
		if t.keyCompiled != nil && t.keyCompiled[i].IsLoaded() {
			continue
		}
		if _, err := compileMatcherRegex(regexKey(expandVars(d, key), t.Comparator.IsCaseMap()), t.Comparator.IsOctet()); err != nil {
			d.RuntimeErrors++
			return false
		}
	}
	return true
}

func (t *matcherTest) tryMatch(d *RuntimeData, source string) (bool, error) {
	for i, key := range t.Key {
		if err := d.executionContext().Err(); err != nil {
			return false, err
		}
		var (
			ok      bool
			matches []string
			err     error
		)
		if t.keyCompiled != nil && t.keyCompiled[i].IsLoaded() {
			ok, matches, err = t.keyCompiled[i].matchContext(d.executionContext(), source)
		} else {
			key = expandVars(d, key)
			if t.Match == MatchRegex || t.Match == MatchMatches {
				var cm CompiledMatcher
				if t.Match == MatchRegex {
					cm, err = compileMatcherRegex(regexKey(key, t.Comparator.IsCaseMap()), t.Comparator.IsOctet())
				} else {
					cm, err = compileMatcher(key, t.Comparator.IsOctet(), t.Comparator.IsCaseMap())
				}
				if err == nil {
					ok, matches, err = cm.matchContext(d.executionContext(), source)
				}
			} else {
				ok, matches, err = testString(t.Comparator, t.Match, t.Relational, source, key)
			}
		}
		if d.executionContext().Err() != nil {
			return false, d.executionContext().Err()
		}
		if err != nil {
			if t.Match == MatchRegex {
				// a regex from a variable that does not compile: the test
				// is false and the error is reported, the script goes on
				d.RuntimeErrors++
				continue
			}
			return false, err
		}
		if ok {
			if t.Match == MatchMatches || t.Match == MatchRegex {
				d.MatchVariables = matches
			}
			return true, nil
		}
	}
	return false, nil
}
