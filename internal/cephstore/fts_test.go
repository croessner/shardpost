//go:build tentacle

package cephstore

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// TestDictEntries: a term has one dictionary key per trigram of its
// token, in the shard of the trigram; terms of the header field and
// tokens shorter than a trigram have none.
func TestDictEntries(t *testing.T) {
	byShard, n := dictEntries([]string{"s:invoice", "b:ab", "h:4711abcdef", "b:übung"})

	var got []string

	for oid, keys := range byShard {
		for _, k := range keys {
			tri, _, _ := strings.Cut(k, dictSep)
			if oid != dictShardOID(tri) || !strings.HasPrefix(oid, dictPrefix) {
				t.Errorf("key %q in %s, want %s", k, oid, dictShardOID(tri))
			}

			got = append(got, k)
		}
	}

	slices.Sort(got)

	want := []string{
		"bun:b:übung", "ice:s:invoice", "inv:s:invoice", "nvo:s:invoice", "oic:s:invoice",
		"ung:b:übung", "voi:s:invoice", "übu:b:übung",
	}
	if !reflect.DeepEqual(got, want) || n != len(want) {
		t.Fatalf("entries %q (%d), want %q", got, n, want)
	}
}

// TestDictCostPerTerm: what a new term writes depends on its token alone,
// however many terms there are already.
func TestDictCostPerTerm(t *testing.T) {
	if _, n := dictEntries([]string{"b:rechnung"}); n != 6 {
		t.Fatalf("%d entries for an 8 letter token, want 6", n)
	}
}

// TestDictScan: a scan keeps the terms of its field that contain the
// token, as terms (without the trigram), and remembers where it stopped.
func TestDictScan(t *testing.T) {
	s := newDictScan("voi", "s")

	if s.oid != dictShardOID("voi") || s.prefix != "voi:s:" {
		t.Fatalf("scan %+v", s)
	}

	for _, k := range []string{"voi:s:invoice", "voi:s:invoices", "voi:s:voicemail", "voi:s:avoid"} {
		s.take(k, "voice")
	}

	if want := []string{"s:invoice", "s:invoices", "s:voicemail"}; !reflect.DeepEqual(s.found, want) || s.after != "voi:s:avoid" {
		t.Fatalf("found %q after %q, want %q", s.found, s.after, want)
	}
}

// TestFinishedScan: the first range read to its end has the answer; while
// none is, a scan that collected the candidate limit ends the search.
func TestFinishedScan(t *testing.T) {
	open := &dictScan{found: []string{"b:one"}}
	full := &dictScan{found: make([]string, maxCandidates)}
	done := &dictScan{done: true}

	if finishedScan([]*dictScan{open}) != nil {
		t.Error("an unfinished scan ended the search")
	}

	if finishedScan([]*dictScan{open, full}) != full {
		t.Error("the candidate limit did not end the search")
	}

	if finishedScan([]*dictScan{full, open, done}) != done {
		t.Error("a complete range must win over a truncated one")
	}
}

// TestLegacyDictTerms: term lists of the old layout are still read.
func TestLegacyDictTerms(t *testing.T) {
	set := map[string]struct{}{}
	addSubstringTerms(set, splitTerms([]byte("b:invoice\ns:invoice\ns:voicemail\ns:avoid")), "s", "voice")

	if got := keys(set); !reflect.DeepEqual(got, []string{"s:invoice", "s:voicemail"}) {
		t.Fatalf("legacy terms %q", got)
	}
}
