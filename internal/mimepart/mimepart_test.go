package mimepart

import "testing"

// TestMessageHasAttachment: the answer of RFC 8621 4.1.4 for the shapes
// that decide it.
func TestMessageHasAttachment(t *testing.T) {
	for _, c := range []struct {
		name, raw string
		want      bool
	}{
		{"plain", "Subject: s\r\n\r\ntext\r\n", false},
		{"pdf", "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\ntext\r\n" +
			"--b\r\nContent-Type: application/pdf\r\n\r\n%PDF\r\n--b--\r\n", true},
		{"inline image", "Content-Type: multipart/related; boundary=b\r\n\r\n--b\r\nContent-Type: text/html\r\n\r\n<p>\r\n" +
			"--b\r\nContent-Type: image/png\r\nContent-Disposition: inline\r\n\r\nPNG\r\n--b--\r\n", false},
		{"image attachment", "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\ntext\r\n" +
			"--b\r\nContent-Type: image/png\r\nContent-Disposition: attachment; filename=a.png\r\n\r\nPNG\r\n--b--\r\n", true},
		{"alternative", "Content-Type: multipart/alternative; boundary=b\r\n\r\n--b\r\n\r\ntext\r\n" +
			"--b\r\nContent-Type: text/html\r\n\r\n<p>text</p>\r\n--b--\r\n", false},
		{"no boundary", "Content-Type: multipart/mixed\r\n\r\ncontent\r\n", true},
	} {
		if got := MessageHasAttachment([]byte(c.raw)); got != c.want {
			t.Errorf("%s: hasAttachment %v, want %v", c.name, got, c.want)
		}
	}
}
