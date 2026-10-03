package bench

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"shardpost/internal/fts"
)

// stripSignatures removes the header fields SignatureField names, with
// their continuation lines, and returns what is left and the names it
// removed.
func stripSignatures(raw []byte) ([]byte, []string) {
	var (
		out     bytes.Buffer
		removed []string
		skip    bool
	)

	for _, line := range strings.SplitAfter(string(raw), "\r\n") {
		if strings.HasPrefix(line, "\t") || strings.HasPrefix(line, " ") {
			if !skip {
				out.WriteString(line)
			}

			continue
		}

		name, _, ok := strings.Cut(line, ":")
		skip = ok && !strings.Contains(name, " ") && SignatureField(name)

		if skip {
			removed = append(removed, name)
			continue
		}

		out.WriteString(line)
	}

	return out.Bytes(), removed
}

// TestCorpusVariants (temp/13 §7.4, §10): ids and ids,nosig are the same
// messages from the same seed; nosig leaves out exactly the signature
// and trace fields of the attached headers; the plain corpus is
// unchanged by the corpus switch.
func TestCorpusVariants(t *testing.T) {
	sizes, err := ParseSizes(DefaultSizes)
	if err != nil {
		t.Fatal(err)
	}

	ids := Generator{Seed: 3, Sizes: sizes, Corpus: Corpus{IDs: true}}
	nosig := Generator{Seed: 3, Sizes: sizes, Corpus: Corpus{IDs: true, NoSig: true}}
	attached := 0

	for n := range 400 {
		a, _ := ids.Message("bench-000007", n)
		b, _ := nosig.Message("bench-000007", n)

		stripped, removed := stripSignatures(a)
		if !bytes.Equal(stripped, b) {
			t.Fatalf("message %d: ids without its signature fields differs from ids,nosig", n)
		}

		if again, _ := ids.Message("bench-000007", n); !bytes.Equal(a, again) {
			t.Fatalf("message %d is not deterministic", n)
		}

		if !bytes.Contains(a, []byte("Ref: "+ids.RefID("bench-000007", n)+"\r\n")) {
			t.Fatalf("message %d lacks its reference", n)
		}

		if len(removed) > 0 {
			attached++

			slices.Sort(removed)

			want := []string{"ARC-Authentication-Results", "ARC-Message-Signature", "ARC-Seal", "Authentication-Results",
				"DKIM-Signature", "Received", "Received", "Received", "Received-SPF", "X-Google-DKIM-Signature"}
			if !slices.Equal(removed, want) {
				t.Fatalf("message %d: removed %v", n, removed)
			}
		}
	}

	if attached < 5 || attached > 60 {
		t.Errorf("%d of 400 messages with attached headers", attached)
	}

	// The plain corpus does not change with the identifier corpus.
	plain := Generator{Seed: 3, Sizes: sizes}
	if a, _ := plain.Message("bench-000007", 1); bytes.Contains(a, []byte("Ref: ")) {
		t.Error("the plain corpus carries identifiers")
	}
}

// TestCorpusIdentifiers: the identifier corpus has many more dictionary
// keys per message than the plain one, and nosig fewer than ids (the
// plausibility of temp/13 §7.2, by counts only).
func TestCorpusIdentifiers(t *testing.T) {
	sizes, _ := ParseSizes(DefaultSizes)

	perMessage := func(c Corpus) float64 {
		g := Generator{Seed: 1, Sizes: sizes, Corpus: c}
		seen := map[string]bool{}

		var dict int

		for n := range 300 {
			raw, _ := g.Message("bench-000001", n)
			for _, term := range fts.Terms(raw) {
				if !seen[term] {
					seen[term] = true
					dict += fts.DictKeys(term, 0)
				}
			}
		}

		return float64(dict) / 300
	}

	plain, ids, nosig := perMessage(Corpus{}), perMessage(Corpus{IDs: true}), perMessage(Corpus{IDs: true, NoSig: true})
	if ids < plain+200 || nosig >= ids || nosig < plain+100 {
		t.Errorf("dictionary keys per message: plain %.0f, ids %.0f, ids,nosig %.0f", plain, ids, nosig)
	}
}

// TestParseCorpus: the names round-trip; unknown ones are refused.
func TestParseCorpus(t *testing.T) {
	for _, name := range []string{CorpusPlain, CorpusIDs, CorpusIDsNoSig} {
		c, err := ParseCorpus(name)
		if err != nil || c.String() != name {
			t.Errorf("%s: %+v %v", name, c, err)
		}
	}

	for _, bad := range []string{"nosig", "ids,foo", "id"} {
		if _, err := ParseCorpus(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}

	for _, f := range []string{"DKIM-Signature", "received", "X-Example-Signature", "ARC-Seal"} {
		if !SignatureField(f) {
			t.Errorf("%s is no signature field", f)
		}
	}

	for _, f := range []string{"From", "Message-ID", "X-Signature-Count", "Subject"} {
		if SignatureField(f) {
			t.Errorf("%s is a signature field", f)
		}
	}
}

// TestIdentifierQueries: with the identifier corpus every second query
// is an identifier query; resolved in an account it is the reference of
// a message, whole or cut.
func TestIdentifierQueries(t *testing.T) {
	g := Generator{Seed: 5, Corpus: Corpus{IDs: true}}

	if !slices.Equal(SearchQueriesFor(12, Corpus{}), SearchQueries(12)) {
		t.Error("the plain corpus changed its queries")
	}

	kinds := map[string]int{}

	for _, q := range SearchQueriesFor(12, g.Corpus) {
		kinds[q.Kind]++

		r := ResolveQuery(g, "bench-000002", q)
		if want := expectedText(g, "bench-000002", q); r.Text != want {
			t.Errorf("%s: %q, want %q", q.Kind, r.Text, want)
		}
	}

	if got := [3]int{kinds[QueryIDWhole], kinds[QueryIDStart], kinds[QueryIDInner]}; got != [3]int{2, 2, 2} {
		t.Errorf("kinds %v", kinds)
	}

	if id := g.RefID("bench-000002", 0); len(id) != 32 || !fts.IsIdentifier(id, 32) {
		t.Errorf("reference %q", id)
	}
}

// expectedText is the text a query has in account: the reference of its
// message whole, its first 8 characters, 8 inner characters; a word
// query keeps its text.
func expectedText(g Generator, account string, q SearchQuery) string {
	id := g.RefID(account, q.Msg)

	switch q.Kind {
	case QueryIDWhole:
		return id
	case QueryIDStart:
		return id[:8]
	case QueryIDInner:
		return id[12:20]
	default:
		return q.Text
	}
}
