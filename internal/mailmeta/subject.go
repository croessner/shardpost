package mailmeta

import (
	"mime"
	"strings"
)

// BaseSubject implements the RFC 5256 2.1 "base subject" extraction:
// RFC 2047 decoding, whitespace folding, removal of subj-trailer
// ("(fwd)" and blanks), subj-leader (blobs "[...]" and "re"/"fwd"/"fw"
// prefixes with optional blob and ":"), and "[fwd: ... ]" wrapping,
// repeated until nothing changes. The result is compared with
// i;ascii-casemap, which maps to upper case (so "[" and "_" sort after
// the letters, as the RFC's examples expect). IMAP SORT and the JMAP
// subject sort both use it, so the two protocols order alike.
func BaseSubject(s string) string {
	// (1) decode, fold whitespace
	if d, err := (&mime.WordDecoder{}).DecodeHeader(s); err == nil {
		s = d
	}

	s = strings.Join(strings.Fields(strings.ReplaceAll(s, "\t", " ")), " ")

	for {
		// (2) trailers: "(fwd)" and trailing blanks
		s = stripTrailers(s)
		// (3) leaders: blobs and re/fwd/fw prefixes
		before := s
		s = stripLeaders(s)
		// (4) "[fwd: ... ]"
		if l := strings.ToLower(s); strings.HasPrefix(l, "[fwd:") && strings.HasSuffix(l, "]") {
			s = strings.TrimSpace(s[5 : len(s)-1])
			continue
		}
		// (5) a leading blob that is not the whole subject
		if t, ok := stripLeadingBlob(s); ok {
			s = t
			continue
		}

		if s == before {
			break
		}
	}

	return strings.ToUpper(s)
}

// stripTrailers removes "(fwd)" trailers and trailing blanks.
func stripTrailers(s string) string {
	for {
		t := strings.TrimRight(s, " ")
		if l := strings.ToLower(t); strings.HasSuffix(l, "(fwd)") {
			t = t[:len(t)-5]
		}

		if t == s {
			return s
		}

		s = t
	}
}

// stripLeaders removes subj-leaders until none is left.
func stripLeaders(s string) string {
	for {
		t := stripLeader(s)
		if t == s {
			return s
		}

		s = t
	}
}

// stripLeadingBlob removes a leading "[...]" blob that is not the whole
// subject; ok is false when there is none.
func stripLeadingBlob(s string) (string, bool) {
	if !strings.HasPrefix(s, "[") {
		return s, false
	}

	if i := strings.Index(s, "]"); i > 0 && i < len(s)-1 && !strings.ContainsAny(s[1:i], "[]") {
		return strings.TrimLeft(s[i+1:], " "), true
	}

	return s, false
}

// stripLeader removes one subj-leader: *WSP subj-blob or subj-refwd
// ("re" / "fwd" / "fw", optional WSP, optional blob, ":").
func stripLeader(s string) string {
	t := strings.TrimLeft(s, " ")

	l := strings.ToLower(t)
	for _, p := range []string{"re", "fwd", "fw"} {
		if !strings.HasPrefix(l, p) {
			continue
		}

		rest := t[len(p):]

		r := strings.TrimLeft(rest, " ")
		if strings.HasPrefix(r, "[") {
			if i := strings.Index(r, "]"); i > 0 && !strings.ContainsAny(r[1:i], "[]") {
				r = strings.TrimLeft(r[i+1:], " ")
			}
		}

		if strings.HasPrefix(r, ":") {
			return strings.TrimLeft(r[1:], " ")
		}
	}

	return s
}
