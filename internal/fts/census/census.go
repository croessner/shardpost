// Package census classifies the terms of a full-text index for
// "shardpostctl fts stats" on test accounts (temp/13 §7.1, §7.4): by
// field, class and length, and the dictionary keys the identifier rule
// would leave for every bound n. Only counts leave it, never a term.
package census

import (
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"shardpost/internal/fts"
	"shardpost/internal/model"
)

// Classes of a token.
const (
	ClassLetters = "letters"
	ClassDigits  = "digits"
	ClassMixed   = "mixed"
	// ClassOther are tokens with neither letters nor digits: the long
	// word markers.
	ClassOther = "other"
)

// lengths are the length ranges in runes (inclusive upper bounds); 40
// is the longest term, a window of a long word.
var lengths = []struct {
	upTo  int
	label string
}{
	{3, "2-3"}, {8, "4-8"}, {12, "9-12"}, {16, "13-16"}, {24, "17-24"}, {32, "25-32"}, {39, "33-39"}, {1 << 30, "40"},
}

type rowKey struct{ field, class, length string }

// Census counts terms.
type Census struct {
	rows  map[rowKey]*model.FTSTermClass
	terms int64
	dict  int64
	// byIdent are the dictionary keys and identifiers per bound.
	byIdent [fts.IdentMinRunesHigh + 1]struct{ dict, ids int64 }
}

// New returns an empty census.
func New() *Census { return &Census{rows: map[rowKey]*model.FTSTermClass{}} }

// Add counts one term ("<field>:<token>").
func (c *Census) Add(term string) {
	field, tok, _ := strings.Cut(term, ":")
	n := utf8.RuneCountInString(tok)
	keys := int64(fts.DictKeys(term, 0))

	k := rowKey{field: field, class: Class(tok), length: lengthOf(n)}

	r := c.rows[k]
	if r == nil {
		r = &model.FTSTermClass{Field: k.field, Class: k.class, Length: k.length}
		c.rows[k] = r
	}

	r.Terms++
	r.DictKeys += keys
	c.terms++
	c.dict += keys

	// The same function the dictionary would use with the rule.
	body := field == fts.FieldBody

	for b := fts.IdentMinRunesLow; b <= fts.IdentMinRunesHigh; b++ {
		c.byIdent[b].dict += int64(fts.DictKeys(term, b))

		if body && fts.IsIdentifier(tok, b) {
			c.byIdent[b].ids++
		}
	}
}

// Result returns the counts: the rows sorted by field, class and length.
func (c *Census) Result() *model.FTSTermClasses {
	out := &model.FTSTermClasses{Terms: c.terms, DictKeys: c.dict, Rows: make([]model.FTSTermClass, 0, len(c.rows))}

	for _, r := range c.rows {
		out.Rows = append(out.Rows, *r)
	}

	slices.SortFunc(out.Rows, func(a, b model.FTSTermClass) int {
		return cmp.Or(cmp.Compare(a.Field, b.Field), cmp.Compare(a.Class, b.Class), cmp.Compare(lengthIndex(a.Length), lengthIndex(b.Length)))
	})

	for b := fts.IdentMinRunesLow; b <= fts.IdentMinRunesHigh; b++ {
		out.ByIdent = append(out.ByIdent, model.FTSIdentBound{MinRunes: b, DictKeys: c.byIdent[b].dict, Identifiers: c.byIdent[b].ids})
	}

	return out
}

// Class is the class of a token.
func Class(tok string) string {
	var letter, digit bool

	for _, r := range tok {
		switch {
		case unicode.IsLetter(r):
			letter = true
		case unicode.IsDigit(r):
			digit = true
		}
	}

	switch {
	case letter && digit:
		return ClassMixed
	case letter:
		return ClassLetters
	case digit:
		return ClassDigits
	default:
		return ClassOther
	}
}

func lengthOf(n int) string {
	for _, l := range lengths {
		if n <= l.upTo {
			return l.label
		}
	}

	return lengths[len(lengths)-1].label
}

func lengthIndex(label string) int {
	return slices.IndexFunc(lengths, func(l struct {
		upTo  int
		label string
	}) bool {
		return l.label == label
	})
}
