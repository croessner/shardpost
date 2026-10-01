package fts

import (
	"strings"
	"testing"
)

// Messages in the charsets and forms German mail arrives in.
const (
	latin1QP = "From: a@example.org\r\nSubject: qp\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=ISO-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"Gr=FC=DFe aus K=F6ln, die Rechnung f=FCr M=E4rz.\r\n"
	cp1252 = "From: a@example.org\r\nSubject: cp\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=windows-1252\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" +
		"\x84Bestellbest\xe4tigung\x93 \x80 99, Stra\xdfe\r\n"
	unlabelled = "From: a@example.org\r\nSubject: raw\r\n\r\nM\xfcller hat angerufen\r\n"
	multipart  = "From: a@example.org\r\nSubject: mp\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=b1\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain; charset=iso-8859-1\r\n\r\nSch\xf6ne Gr\xfc\xdfe\r\n" +
		"--b1\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" +
		"<html><head><title>Hiddentitle</title><style>.hiddenstyle{color:red}</style>" +
		"<script type=\"text/javascript\">var hiddenscript = \"</p>\";</script></head>" +
		"<body><!-- hiddencomment --><p>Sch&ouml;ne&nbsp;Gr&uuml;&szlig;e</p><div>Zeile<br>zwei</div>" +
		"<b>Rech</b>nung &amp; &#x20AC;&#8364; &lt;tag&gt; <a href=\"http://x.example/hiddenurl\">Link</a></body></html>\r\n" +
		"--b1--\r\n"
	headers = "From: =?iso-8859-1?q?J=FCrgen?= <j@example.org>\r\n" +
		"Subject: =?windows-1252?Q?Angebot_f=FCr_Stra=DFe_=80?=\r\nX-Note: \xc4rger\r\n\r\nbody\r\n"
	metaCharset = "From: a@example.org\r\nSubject: meta\r\nContent-Type: text/html\r\n\r\n" +
		"<html><head><meta http-equiv=\"Content-Type\" content=\"text/html; charset=iso-8859-1\"></head><p>K\xe4se</p></html>\r\n"
)

func termSet(raw string) map[string]bool {
	out := map[string]bool{}
	for _, t := range Terms([]byte(raw)) {
		out[t] = true
	}

	return out
}

func TestTermsCharsets(t *testing.T) {
	cases := []struct {
		name, raw string
		want      []string
	}{
		{"latin-1 quoted-printable", latin1QP, []string{"b:grüße", "b:köln", "b:für", "b:märz", "b:rechnung"}},
		{"windows-1252", cp1252, []string{"b:bestellbestätigung", "b:straße", "b:99"}},
		{"unlabelled 8-bit", unlabelled, []string{"b:müller", "b:angerufen"}},
		{"multipart, latin-1 part first", multipart, []string{"b:schöne", "b:grüße", "b:zeile", "b:zwei", "b:rechnung", "b:link"}},
		{"encoded words and raw 8-bit header", headers, []string{"s:angebot", "s:für", "s:straße", "f:jürgen", "h:ärger", "h:jürgen"}},
		{"meta charset", metaCharset, []string{"b:käse"}},
	}

	for _, c := range cases {
		terms := termSet(c.raw)
		for _, w := range c.want {
			if !terms[w] {
				t.Errorf("%s: missing %s in %v", c.name, w, Terms([]byte(c.raw)))
			}
		}
	}
}

func TestTermsHTMLVisibleTextOnly(t *testing.T) {
	terms := termSet(multipart)

	for _, bad := range []string{
		"b:hiddentitle", "b:hiddenstyle", "b:color", "b:hiddenscript", "b:var", "b:hiddencomment",
		"b:hiddenurl", "b:href", "b:html", "b:div", "b:zeilezwei", "b:nbsp", "b:ouml", "b:amp", "b:20ac",
	} {
		if terms[bad] {
			t.Errorf("%s indexed", bad)
		}
	}

	if !terms["b:tag"] {
		t.Error("the escaped <tag> is text and must be indexed")
	}
}

func TestHTMLModes(t *testing.T) {
	t.Cleanup(func() { _ = SetHTMLMode(HTMLModeText) })

	if err := SetHTMLMode("strip"); err == nil {
		t.Error("unknown mode accepted")
	}

	if err := SetHTMLMode(HTMLModeRaw); err != nil {
		t.Fatal(err)
	}

	if terms := termSet(multipart); !terms["b:hiddenscript"] || !terms["b:div"] {
		t.Error("raw mode must index the markup")
	}

	if err := SetHTMLMode(HTMLModeSkip); err != nil {
		t.Fatal(err)
	}

	terms := termSet(multipart)
	if terms["b:zeile"] || terms["b:rechnung"] {
		t.Error("skip mode indexed the HTML part")
	}

	if !terms["b:schöne"] {
		t.Error("skip mode lost the plain text part")
	}
}

func TestHTMLText(t *testing.T) {
	cases := []struct{ in, want string }{
		{"<p>a</p><p>b</p>", "\na\n\nb\n"},
		{"x<b>y</b>z", "xyz"},
		{"1 < 2 &amp;&amp; 3 > 2", "1 < 2 && 3 > 2"},
		{"<SCRIPT>alert('</p>')</ScRiPt>after", "after"},
		{"<img alt='a>b' src=x>c", "\nc"},
		{"<!DOCTYPE html><?xml version=\"1.0\"?>t", "t"},
		{"open <b unclosed", "open "},
		{"<style>never closed", ""},
		{"<p class=\"x\">&eacute;t&#233;</p>", "\nété\n"},
	}

	for _, c := range cases {
		if got := HTMLText(c.in); got != c.want {
			t.Errorf("HTMLText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBodyTextBounded(t *testing.T) {
	raw := "Subject: big\r\nContent-Type: text/plain; charset=iso-8859-1\r\n\r\n" + strings.Repeat("\xe4", MaxBodyBytes)

	got := BodyText([]byte(raw))
	if len(got) > MaxBodyBytes+1 || !strings.HasPrefix(got, "ääää") {
		t.Errorf("body text of %d bytes", len(got))
	}
}
