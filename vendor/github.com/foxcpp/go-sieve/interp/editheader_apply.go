package interp

import (
	"bytes"
	"strings"
)

// ApplyHeaderEdits applies addheader/deleteheader outcomes to a raw
// message (RFC 5293): fields are matched by name, unfolded values are
// tested against the delete key, the body is untouched.
func ApplyHeaderEdits(raw []byte, edits []HeaderEdit) []byte {
	sep := bytes.Index(raw, []byte("\r\n\r\n"))
	sepLen := 4
	if sep < 0 {
		sep = bytes.Index(raw, []byte("\n\n"))
		sepLen = 2
	}
	var head, body []byte
	if sep < 0 {
		head, body = raw, nil
	} else {
		head, body = raw[:sep], raw[sep:]
	}
	// split into logical fields
	type field struct {
		name string
		text string // full raw text without trailing CRLF
	}
	var fields []field
	for _, line := range strings.Split(strings.ReplaceAll(string(head), "\r\n", "\n"), "\n") {
		if line == "" {
			continue
		}
		if (line[0] == ' ' || line[0] == '\t') && len(fields) > 0 {
			fields[len(fields)-1].text += "\r\n" + line
			continue
		}
		name, _, _ := strings.Cut(line, ":")
		fields = append(fields, field{name: strings.TrimSpace(name), text: line})
	}
	for _, e := range edits {
		if e.Add {
			f := field{name: e.Name, text: e.Name + ": " + e.Value}
			if e.Last {
				fields = append(fields, f)
			} else {
				fields = append([]field{f}, fields...)
			}
			continue
		}
		var idx []int
		for i, f := range fields {
			if strings.EqualFold(f.name, e.Name) {
				idx = append(idx, i)
			}
		}
		value := func(i int) string {
			_, v, _ := strings.Cut(fields[i].text, ":")
			return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(v, "\r\n", ""), "\t", " "))
		}
		del := map[int]bool{}
		if e.Index > 0 {
			j := e.Index - 1
			if e.Last {
				j = len(idx) - e.Index
			}
			if j >= 0 && j < len(idx) && (e.Match == nil || e.Match(value(idx[j]))) {
				del[idx[j]] = true
			}
		} else {
			for _, i := range idx {
				if e.Match == nil || e.Match(value(i)) {
					del[i] = true
				}
			}
		}
		kept := fields[:0:0]
		for i, f := range fields {
			if !del[i] {
				kept = append(kept, f)
			}
		}
		fields = kept
	}
	var b bytes.Buffer
	for _, f := range fields {
		b.WriteString(f.text)
		b.WriteString("\r\n")
	}
	if sep >= 0 {
		b.Write(body[sepLen-2:]) // keep the blank line + body as they were
	}
	return b.Bytes()
}
