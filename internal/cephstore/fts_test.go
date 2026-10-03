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
	s := newDictScan(dictShardOID("voi"), "voi", "s")

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

	if finishedScan([]*dictScan{open}, nil) != nil {
		t.Error("an unfinished scan ended the search")
	}

	if a := finishedScan([]*dictScan{open, full}, nil); a == nil || a.complete || len(a.found) != maxCandidates {
		t.Error("the candidate limit did not end the search")
	}

	if a := finishedScan([]*dictScan{full, open, done}, nil); a == nil || !a.complete || len(a.found) != 0 {
		t.Error("a complete range must win over a truncated one")
	}
}

// TestFinishedScanStart (B14h, temp/13 §4.5): with a start entry range
// the answer needs a trigram range and the start range both read to
// their end and is their union. An empty start range (a generation
// without the filter) changes nothing.
func TestFinishedScanStart(t *testing.T) {
	done := &dictScan{done: true, found: []string{"b:abc1", "b:xabc"}}
	startOpen := &dictScan{start: true, found: []string{"b:abc1234567890123456789012345"}}
	startDone := &dictScan{start: true, done: true, found: []string{"b:abc1234567890123456789012345", "b:abc1"}}
	emptyStart := &dictScan{start: true, done: true}

	if finishedScan([]*dictScan{done}, startOpen) != nil {
		t.Error("the start range was not read to its end")
	}

	a := finishedScan([]*dictScan{done}, startDone)
	if a == nil || !a.complete || !reflect.DeepEqual(a.found, []string{"b:abc1", "b:xabc", "b:abc1234567890123456789012345"}) {
		t.Fatalf("union %+v", a)
	}

	if a := finishedScan([]*dictScan{done}, emptyStart); a == nil || !a.complete || !reflect.DeepEqual(a.found, done.found) {
		t.Fatalf("empty start range %+v", a)
	}
}

// TestFinishedScanStartLimit: the candidate limit counts the trigram and
// the start range together; a start range over the limit makes the word
// incomplete, also beside a finished trigram range.
func TestFinishedScanStartLimit(t *testing.T) {
	done := &dictScan{done: true, found: []string{"b:abc1", "b:xabc"}}
	half := &dictScan{found: make([]string, maxCandidates/2)}
	startHalf := &dictScan{start: true, found: make([]string, maxCandidates/2)}

	if a := finishedScan([]*dictScan{half}, startHalf); a == nil || a.complete {
		t.Fatalf("both together at the limit: %+v", a)
	}

	big := &dictScan{start: true, found: make([]string, maxCandidates)}
	if a := finishedScan([]*dictScan{done}, big); a == nil || a.complete {
		t.Fatalf("start range over the limit beside a finished one: %+v", a)
	}

	if a := finishedScan([]*dictScan{{}}, big); a == nil || a.complete {
		t.Fatalf("start range over the limit: %+v", a)
	}
}

// TestStartScan: the start entry scan takes every key of its exact
// range, as the term behind "#<tri0>:".
func TestStartScan(t *testing.T) {
	s := newStartScan("d0", "abc", "b", "abc12")
	if s.prefix != "#abc:b:abc12" || !s.start {
		t.Fatalf("scan %+v", s)
	}

	s.take("#abc:b:abc1234567890123456789012345", "abc12")
	s.take("#abc:b:abc12zz", "abc12")

	if want := []string{"b:abc1234567890123456789012345", "b:abc12zz"}; !reflect.DeepEqual(s.found, want) {
		t.Fatalf("found %q, want %q", s.found, want)
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
