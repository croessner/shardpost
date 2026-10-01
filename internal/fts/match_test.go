package fts

import "testing"

// TestMatcher: the words a query token matches are the ones the index
// finds it by — the word itself, a stem of either, a substring, and for
// a short token a prefix.
func TestMatcher(t *testing.T) {
	m := NewMatcher(Tokenize("invoice"))

	for word, want := range map[string]bool{
		"Invoice":      true,  // case
		"invoices":     true,  // stem of the word
		"proinvoicing": false, // neither it nor its stem contains the token
		"preinvoice":   true,  // substring
		"invoic":       true,  // the token's own stem
		"voice":        false, // part of the token only
		"x":            false, // not a token
	} {
		if got := m.Match(word); got != want {
			t.Errorf("Match(%q) = %v, want %v", word, got, want)
		}
	}

	short := NewMatcher(Tokenize("in"))
	if !short.Match("inbox") || short.Match("bin") {
		t.Error("a token shorter than a trigram matches by prefix only")
	}

	if NewMatcher(nil).Match("anything") || !NewMatcher(nil).Empty() {
		t.Error("an empty matcher matches")
	}
}

// TestWords: the words and their byte offsets, the same runs Tokenize
// makes its tokens of.
func TestWords(t *testing.T) {
	text := "Grüße, a b2c!"
	got := Words(text)

	want := []string{"Grüße", "a", "b2c"}
	if len(got) != len(want) {
		t.Fatalf("Words(%q) = %v", text, got)
	}

	for i, w := range got {
		if text[w.Start:w.End] != want[i] {
			t.Errorf("word %d is %q, want %q", i, text[w.Start:w.End], want[i])
		}
	}
}
