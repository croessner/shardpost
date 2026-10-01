package fts

import (
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
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
		{"<script/>hidden</script>vis", "vis"},
		{"<style />p{}</style>vis", "vis"},
		{"a<!-->b<!--->c<!-- x -->d", "abcd"},
		{"<script>x</scripts>hidden</script >vis", "vis"},
		{"<script>x</script\n>vis", "vis"},
		{"<script>x</script/>vis", "vis"},
	}

	for _, c := range cases {
		if got := HTMLText(c.in); got != c.want {
			t.Errorf("HTMLText(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBodyTextBounded(t *testing.T) {
	// Latin-1 doubles in UTF-8: the text is cut, not the input read whole
	raw := "Subject: big\r\nContent-Type: text/plain; charset=iso-8859-1\r\n\r\n" + strings.Repeat("\xe4", MaxBodyBytes)

	got := BodyText([]byte(raw))
	if len(got) > MaxBodyBytes+1 || !strings.HasPrefix(got, "ääää") || !utf8.ValidString(got) {
		t.Errorf("body text of %d bytes", len(got))
	}

	// two parts: the second gets only the room the first left
	half := strings.Repeat("a", MaxBodyBytes/2+10)

	raw = "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n\r\n" + half + "\r\n--b\r\n\r\n" + half + "\r\n--b--\r\n"
	if got := BodyText([]byte(raw)); len(got) > MaxBodyBytes+1 {
		t.Errorf("two parts: %d bytes", len(got))
	}

	// a message go-message cannot parse: bounded after conversion
	raw = "a header line without a colon\r\n\r\n" + strings.Repeat("\xe4", MaxBodyBytes)
	if got := BodyText([]byte(raw)); len(got) > MaxBodyBytes || !utf8.ValidString(got) {
		t.Errorf("unparsable: %d bytes", len(got))
	}
}

func TestMetaCharset(t *testing.T) {
	for _, label := range []string{"", "; charset=us-ascii", "; charset=x-unknown"} {
		for _, meta := range []string{
			`<meta charset="iso-8859-1">`,
			`<meta http-equiv="Content-Type" content="text/html; charset=iso-8859-1">`,
		} {
			raw := "Content-Type: text/html" + label + "\r\n\r\n<html><head>" + meta + "</head><p>K\xe4se</p></html>"
			if got := BodyText([]byte(raw)); !strings.Contains(got, "Käse") {
				t.Errorf("label %q, %s: %q", label, meta, got)
			}
		}
	}

	// a declared charset wins over the meta element
	raw := "Content-Type: text/html; charset=iso-8859-15\r\n\r\n<meta charset=\"koi8-r\"><p>\xa4</p>"
	if got := BodyText([]byte(raw)); !strings.Contains(got, "€") {
		t.Errorf("declared charset: %q", got)
	}
}

func TestUTF16AndMislabelledUTF8(t *testing.T) {
	utf16 := "Content-Type: text/plain; charset=utf-16le\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		"RwByAPwA3wBlAA==\r\n" // "Grüße" in UTF-16LE
	if got := BodyText([]byte(utf16)); !strings.Contains(got, "Grüße") {
		t.Errorf("utf-16le: %q", got)
	}

	mislabelled := "Content-Type: text/plain; charset=iso-8859-1\r\n\r\nGrüße\r\n"
	if got := BodyText([]byte(mislabelled)); !strings.Contains(got, "Grüße") {
		t.Errorf("UTF-8 labelled Latin-1: %q", got)
	}
}

func TestAttachmentNamesAndMessages(t *testing.T) {
	raw := "Content-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
		"--b\r\nContent-Type: application/pdf; name=\"=?utf-8?q?Angebot_K=C3=B6ln.pdf?=\"\r\n" +
		"Content-Disposition: attachment;\r\n filename*0*=iso-8859-1''Rechnung%20M%E4rz;\r\n filename*1=\".pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\nJVBERi0xLjQK\r\n" +
		"--b\r\nContent-Type: message/rfc822\r\n\r\n" +
		"From: Inner <inner@example.org>\r\nSubject: =?iso-8859-1?q?Gr=FC=DFe?=\r\n\r\nquarterly numbers\r\n" +
		"--b--\r\n"

	got := BodyText([]byte(raw))
	for _, want := range []string{"see attached", "Angebot Köln.pdf", "Rechnung März.pdf", "Subject: Grüße", "inner@example.org", "quarterly numbers"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}

	if strings.Contains(got, "JVBER") {
		t.Errorf("attachment content in %q", got)
	}

	if r := ReadableText([]byte(raw)); r != "see attached " {
		t.Errorf("readable text %q", r)
	}
}

func TestMediaParams(t *testing.T) {
	cases := []struct {
		in, typ, key, want string
	}{
		{`Text/HTML; Charset="UTF-8"`, "text/html", "charset", "UTF-8"},
		{`attachment; filename="a;b \"c\".txt"`, "attachment", "filename", `a;b "c".txt`},
		{`attachment; filename*=utf-8'de'%C3%A4.txt`, "attachment", "filename", "ä.txt"},
		{`attachment; filename*1="b.txt"; filename*0="a"`, "attachment", "filename", "ab.txt"},
		{"attachment; filename=M\xfcller.txt", "attachment", "filename", "Müller.txt"},
		{`text/plain; charset=utf-8; charset=utf-8; broken`, "text/plain", "charset", "utf-8"},
	}

	for _, c := range cases {
		typ, params := mediaParams(c.in)
		if typ != c.typ || params[c.key] != c.want {
			t.Errorf("%q: %q %q", c.in, typ, params[c.key])
		}
	}
}

func TestSpans(t *testing.T) {
	got := Spans(`eautiful "head" foo-ba x`)
	want := []Span{{"eautiful", MatchInfix}, {"head", MatchWord}, {"foo", MatchWord}, {"ba", MatchWord}}

	if len(got) != len(want) {
		t.Fatalf("spans %v", got)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("span %d: %v, want %v", i, got[i], want[i])
		}
	}

	if got := Spans("Hello wor"); len(got) != 2 || got[1] != (Span{"wor", MatchPrefix}) {
		t.Errorf("last word: %v", got)
	}

	if got := Spans("x"); got != nil {
		t.Errorf("one letter: %v", got)
	}
}

func TestLongWordMarker(t *testing.T) {
	long := strings.Repeat("a", maxToken+1)

	terms := termSet("Subject: s\r\nX-Id: " + long + "\r\n\r\nshort " + long + "\r\n")
	if !terms["b:"+LongWord] || terms["s:"+LongWord] || terms["b:"+HugeWord] {
		t.Errorf("terms %v", terms)
	}

	// the header field has the first window only, and no markers
	if !terms["h:"+long[:maxToken]] || terms["h:"+LongWord] {
		t.Errorf("header terms %v", terms)
	}
}

// TestLongWordWindows proves what Spans relies on, for every length of
// a long word up to well past maxWindows windows: every prefix is a
// prefix of a term, every inner part of up to coveredInfix runes is in
// a term unless the field has HugeWord, every longer one has LongWord.
func TestLongWordWindows(t *testing.T) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

	for n := maxToken + 1; n <= maxToken+windowStep*(maxWindows+2); n++ {
		w := make([]rune, n)
		for i := range w {
			w[i] = rune(alphabet[(i*7+i/len(alphabet))%len(alphabet)])
		}

		checkWindows(t, w)
	}

	if got := Markers(Span{"abc", MatchInfix}); !slices.Equal(got, []string{HugeWord}) {
		t.Errorf("short infix markers %v", got)
	}

	if got := Markers(Span{strings.Repeat("a", coveredInfix+1), MatchInfix}); !slices.Equal(got, []string{LongWord}) {
		t.Errorf("long infix markers %v", got)
	}

	if Markers(Span{"abc", MatchPrefix}) != nil || Markers(Span{"abc", MatchWord}) != nil {
		t.Error("prefix and word need no markers")
	}
}

// checkWindows checks the window terms of one long word.
func checkWindows(t *testing.T, w []rune) {
	t.Helper()

	n := len(w)
	terms := longWordTerms(FieldBody, w)
	huge := slices.Contains(terms, HugeWord)

	if !slices.Contains(terms, LongWord) || len(terms) > maxWindows+2 {
		t.Fatalf("n=%d: terms %v", n, terms)
	}

	if want := n > maxToken+windowStep*(maxWindows-1); huge != want {
		t.Fatalf("n=%d: huge %v", n, huge)
	}

	if !strings.HasPrefix(string(w), terms[0]) || len([]rune(terms[0])) != maxToken {
		t.Fatalf("n=%d: window 0 %q", n, terms[0])
	}

	if !huge {
		checkInfixes(t, w, terms)
	}

	if h := longWordTerms(FieldHeader, w); len(h) != 1 || h[0] != string(w[:maxToken]) {
		t.Fatalf("n=%d: header terms %v", n, h)
	}
}

// checkInfixes checks that every inner part of up to coveredInfix runes
// of w is in a term.
func checkInfixes(t *testing.T, w []rune, terms []string) {
	t.Helper()

	for p := range w {
		for l := minToken; l <= coveredInfix && p+l <= len(w); l++ {
			if !inSomeTerm(terms, string(w[p:p+l])) {
				t.Fatalf("n=%d: %q at %d in no window", len(w), string(w[p:p+l]), p)
			}
		}
	}
}

func inSomeTerm(terms []string, sub string) bool {
	return slices.ContainsFunc(terms, func(term string) bool { return strings.Contains(term, sub) })
}

func TestHeaderNamesAndValues(t *testing.T) {
	terms := termSet("X-Extra: one\r\nX-Extra: two\r\nSubject: first\r\nSubject: second\r\n\r\nb\r\n")
	for _, w := range []string{"h:extra", "h:one", "h:two", "s:first", "s:second"} {
		if !terms[w] {
			t.Errorf("missing %s", w)
		}
	}
}
