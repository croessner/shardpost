package jmap

import "testing"

// TestEmailQueryConvertedText: the text conditions find words of body
// parts in Latin-1, Windows-1252 and unlabelled 8-bit, of an HTML part
// (but not its markup), and of encoded or raw 8-bit header words, by
// their UTF-8 spelling.
func TestEmailQueryConvertedText(t *testing.T) {
	s, u := emailServer(t)

	qp := deliverAt(t, u, inboxName, "From: a@example.org\r\nSubject: qp\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: text/plain; charset=ISO-8859-1\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n"+
		"Gr=FC=DFe aus K=F6ln\r\n", 1)
	cp := deliverAt(t, u, inboxName, "From: a@example.org\r\nSubject: cp\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: text/plain; charset=windows-1252\r\n\r\n\x84Bestellbest\xe4tigung\x93\r\n", 2)
	raw := deliverAt(t, u, inboxName, "From: a@example.org\r\nSubject: raw\r\n\r\nM\xfcller hat angerufen\r\n", 3)
	html := deliverAt(t, u, inboxName, "From: a@example.org\r\nSubject: html\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: text/html\r\n\r\n<style>p{color:red}</style><script>var hiddenscript=1;</script>"+
		"<p>K&auml;se&nbsp;und&nbsp;Wurst</p>\r\n", 4)
	hdr := deliverAt(t, u, inboxName, "From: =?iso-8859-1?q?J=FCrgen?= <j@example.org>\r\n"+
		"Subject: =?windows-1252?Q?Angebot_f=FCr_Stra=DFe?=\r\nX-Note: \xc4rger\r\n\r\nbody\r\n", 5)

	cases := []struct {
		name, filter string
		want         []string
	}{
		{"latin-1 body", `{"body":"grüße"}`, []string{qp.GUID}},
		{"windows-1252 body", `{"body":"bestellbestätigung"}`, []string{cp.GUID}},
		{"unlabelled body", `{"text":"müller"}`, []string{raw.GUID}},
		{"html text", `{"body":"käse wurst"}`, []string{html.GUID}},
		{"html markup", `{"body":"hiddenscript"}`, nil},
		{"html style", `{"text":"color"}`, nil},
		{"encoded subject", `{"subject":"straße"}`, []string{hdr.GUID}},
		{"encoded from", `{"from":"jürgen"}`, []string{hdr.GUID}},
		{"raw 8-bit header", `{"header":["X-Note","ärger"]}`, []string{hdr.GUID}},
	}

	for _, c := range cases {
		sameIDs(t, c.name, queryIDs(t, s, `{"accountId":"test","filter":`+c.filter+`}`), c.want...)
	}
}
