package mailmeta

import (
	"testing"

	"shardpost/internal/fts"
)

// TestPreviewHTMLModes: the preview of an HTML-only message follows
// fts.html — its visible text, its markup, or nothing — and never holds
// the file names that the search text has.
func TestPreviewHTMLModes(t *testing.T) {
	t.Cleanup(func() { _ = fts.SetHTMLMode(fts.HTMLModeText) })

	raw := []byte("Subject: s\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/html; charset=iso-8859-1\r\n\r\n" +
		"<html><head><title>T</title><style>p{}</style></head><p>Gr&uuml;&szlig;e aus K\xf6ln</p></html>\r\n" +
		"--b\r\nContent-Type: application/pdf; name=report.pdf\r\n\r\nx\r\n--b--\r\n")

	cases := []struct{ mode, want string }{
		{fts.HTMLModeText, "Grüße aus Köln"},
		{fts.HTMLModeRaw, "<html><head><title>T</title><style>p{}</style></head><p>Gr&uuml;&szlig;e aus Köln</p></html>"},
		{fts.HTMLModeSkip, ""},
	}

	for _, c := range cases {
		if err := fts.SetHTMLMode(c.mode); err != nil {
			t.Fatal(err)
		}

		if _, _, got := Extract(raw); got != c.want {
			t.Errorf("%s: preview %q, want %q", c.mode, got, c.want)
		}
	}
}

// TestPreviewAlternative: of a multipart/alternative the preview shows
// one part, the plain text where there is one, else the HTML text — also
// when the HTML part comes first or sits in a multipart/related.
func TestPreviewAlternative(t *testing.T) {
	plainHTML := "Content-Type: multipart/alternative; boundary=a\r\n\r\n" +
		"--a\r\nContent-Type: text/html\r\n\r\n<p>Hello from HTML</p>\r\n" +
		"--a\r\nContent-Type: text/plain\r\n\r\nHello from plain\r\n--a--\r\n"
	htmlOnly := "Content-Type: multipart/alternative; boundary=a\r\n\r\n" +
		"--a\r\nContent-Type: text/plain\r\n\r\n\r\n" +
		"--a\r\nContent-Type: multipart/related; boundary=r\r\n\r\n" +
		"--r\r\nContent-Type: text/html\r\n\r\n<p>Only HTML</p>\r\n" +
		"--r\r\nContent-Type: image/png; name=logo.png\r\n\r\nx\r\n--r--\r\n--a--\r\n"

	for raw, want := range map[string]string{plainHTML: "Hello from plain", htmlOnly: "Only HTML"} {
		if _, _, got := Extract([]byte(raw)); got != want {
			t.Errorf("preview %q, want %q", got, want)
		}
	}
}
