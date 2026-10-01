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
