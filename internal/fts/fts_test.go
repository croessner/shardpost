package fts

import (
	"reflect"
	"testing"
)

func TestTokenizeAndStems(t *testing.T) {
	toks := Tokenize("Please find the Invoices attached! Rechnungen, 4711.")

	want := []string{"please", "find", "the", "invoices", "attached", "rechnungen", "4711"}
	if !reflect.DeepEqual(toks, want) {
		t.Fatalf("tokens = %v", toks)
	}

	if s := Stems("invoices"); len(s) == 0 || s[0] != "invoic" {
		t.Fatalf("english stem = %v", s)
	}

	if s := Stems("rechnungen"); len(s) == 0 {
		t.Fatalf("german stem missing")
	}

	if got := Trigrams("voice"); !reflect.DeepEqual(got, []string{"voi", "oic", "ice"}) {
		t.Fatalf("trigrams = %v", got)
	}

	if Trigrams("ab") != nil {
		t.Fatal("short token must have no trigrams")
	}
}

func TestTerms(t *testing.T) {
	raw := []byte("From: Alice <alice@example.org>\r\nSubject: Invoices 4711\r\nContent-Type: text/plain\r\n\r\nPaid yesterday.\r\n")

	terms := map[string]bool{}
	for _, x := range Terms(raw) {
		terms[x] = true
	}

	for _, want := range []string{"s:invoices", "s:invoic", "s:4711", "f:alice", "b:paid", "b:yesterday", "h:example"} {
		if !terms[want] {
			t.Errorf("missing term %s in %v", want, Terms(raw))
		}
	}
}
