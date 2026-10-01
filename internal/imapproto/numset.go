package imapproto

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// numSet is a parsed sequence set. start/stop of 0 means "*".
type numSet struct {
	ranges    [][2]uint32
	searchRes bool // the "$" marker (SEARCHRES)
}

func parseSet(s string) (*numSet, error) {
	if s == "$" {
		return &numSet{searchRes: true}, nil
	}

	set := &numSet{}

	for _, part := range strings.Split(s, ",") {
		lo, hi, ok := strings.Cut(part, ":")
		if !ok {
			hi = lo
		}

		a, err := parseSetNum(lo)
		if err != nil {
			return nil, err
		}

		b, err := parseSetNum(hi)
		if err != nil {
			return nil, err
		}

		set.ranges = append(set.ranges, [2]uint32{a, b})
	}

	return set, nil
}

func parseSetNum(s string) (uint32, error) {
	if s == "*" {
		return 0, nil
	}

	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("%w: bad sequence set %q", errSyntax, s)
	}

	return uint32(n), nil
}

func fromUIDs(uids []uint32) *numSet {
	set := &numSet{}
	for _, u := range uids {
		set.ranges = append(set.ranges, [2]uint32{u, u})
	}

	return set
}

// concrete replaces the "$" marker by the saved SEARCH result (UIDs,
// RFC 5182); any other set is returned as it is.
func (c *conn) concrete(set *numSet) *numSet {
	if set.searchRes {
		return fromUIDs(c.searchRes)
	}

	return set
}

// contains reports whether n is in the set, with maxN standing in for "*".
func (s *numSet) contains(n, maxN uint32) bool {
	for _, r := range s.ranges {
		start, stop := r[0], r[1]
		if start == 0 {
			start = maxN
		}

		if stop == 0 {
			stop = maxN
		}

		if start > stop {
			start, stop = stop, start
		}

		if n >= start && n <= stop {
			return true
		}
	}

	return false
}

// expand lists the members of a set with concrete bounds (used for
// QRESYNC known UIDs; "*" is replaced by maxN).
func (s *numSet) expand(maxN uint32) []uint32 {
	var out []uint32

	for _, r := range s.ranges {
		start, stop := r[0], r[1]
		if start == 0 {
			start = maxN
		}

		if stop == 0 {
			stop = maxN
		}

		if start > stop {
			start, stop = stop, start
		}

		if stop-start > 1<<20 {
			stop = start + 1<<20
		}

		for n := start; n <= stop && n != 0; n++ {
			out = append(out, n)
		}
	}

	return out
}

// formatSet renders numbers as a compact sequence set ("1:3,5").
func formatSet(nums []uint32) string {
	if len(nums) == 0 {
		return ""
	}

	l := append([]uint32{}, nums...)
	sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })

	var b strings.Builder

	start, prev := l[0], l[0]
	flush := func() {
		if b.Len() > 0 {
			b.WriteByte(',')
		}

		if start == prev {
			fmt.Fprintf(&b, "%d", start)
		} else {
			fmt.Fprintf(&b, "%d:%d", start, prev)
		}
	}

	for _, n := range l[1:] {
		if n == prev || n == prev+1 {
			prev = n
			continue
		}

		flush()

		start, prev = n, n
	}

	flush()

	return b.String()
}

// formatOrderedSet renders numbers as a sequence set that keeps their
// order, as the ALL of an ESORT result does (RFC 5267 3.1). Only runs of
// consecutive ascending numbers become ranges ("4:6,2,9"); a descending
// run stays a list, since a range expands in increasing order (3.2).
func formatOrderedSet(nums []uint32) string {
	var b strings.Builder

	for i := 0; i < len(nums); {
		j := i
		for j+1 < len(nums) && nums[j+1] == nums[j]+1 {
			j++
		}

		if b.Len() > 0 {
			b.WriteByte(',')
		}

		if j == i {
			fmt.Fprintf(&b, "%d", nums[i])
		} else {
			fmt.Fprintf(&b, "%d:%d", nums[i], nums[j])
		}

		i = j + 1
	}

	return b.String()
}
