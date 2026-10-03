package fts

import (
	"slices"
	"strings"
	"testing"
)

// A forward with an attached message and a bounce with the header of
// the returned message, both with signature and trace fields.
const (
	forwardMsg = "From: a@example.org\r\nSubject: fwd\r\nDKIM-Signature: v=1; b=outersig0k9\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=b1\r\n\r\n" +
		"--b1\r\nContent-Type: text/plain\r\n\r\nsiehe anhang\r\n" +
		"--b1\r\nContent-Type: message/rfc822\r\n\r\n" +
		"Received: from mx.example.net by mx2 with ESMTPS id recvid7x2\r\n" +
		"DKIM-Signature: v=1; a=rsa-sha256; d=example.com;\r\n\tb=dkimsig4f8q\r\n" +
		"ARC-Seal: i=1; b=arcseal9z1\r\nX-Foo-Signature: xsig5k2\r\n" +
		"Authentication-Results: mx; dkim=pass authres3q\r\n" +
		"From: Bob <bob@example.com>\r\nSubject: Rechnung innen\r\nMessage-ID: <msgid8a@example.com>\r\n\r\n" +
		"inneres wort\r\n" +
		"--b1--\r\n"
	bounceMsg = "From: MAILER-DAEMON@example.org\r\nSubject: undeliverable\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/report; report-type=delivery-status; boundary=b2\r\n\r\n" +
		"--b2\r\nContent-Type: text/plain\r\n\r\nzustellung gescheitert\r\n" +
		"--b2\r\nContent-Type: text/rfc822-headers\r\n\r\n" +
		"Received: from a by b id bouncerecv1\r\nARC-Message-Signature: i=1;\r\n\tb=arcms2w9\r\n" +
		"Subject: Bestellung\r\nX-Google-DKIM-Signature: gsig6t\r\nTo: carol@example.com\r\n" +
		"--b2--\r\n"
)

// TestSignatureField: the list of temp/13 §3 c3 and X-*-Signature, in
// any case; other fields are kept.
func TestSignatureField(t *testing.T) {
	for _, f := range []string{
		"DKIM-Signature", "dkim-signature", "ARC-Seal", "ARC-Message-Signature", "ARC-Authentication-Results",
		"Authentication-Results", "Received", "Received-SPF", "X-Google-DKIM-Signature", "X-MS-Exchange-Signature",
	} {
		if !SignatureField(f) {
			t.Errorf("%s is a signature field", f)
		}
	}

	for _, f := range []string{"From", "Subject", "Message-ID", "References", "X-Mailer", "X-Signature-Note", "Signature"} {
		if SignatureField(f) {
			t.Errorf("%s is not a signature field", f)
		}
	}
}

// TestAttachedHeadersNoSig (c3): "nosig" leaves the signature and trace
// fields of an attached message and of a text/rfc822-headers part out
// of the body, keeps their other fields and text, and does not touch the
// header of the message itself; "all" (the default) keeps everything as
// before.
func TestAttachedHeadersNoSig(t *testing.T) {
	sigTerms := []string{"b:recvid7x2", "b:dkimsig4f8q", "b:arcseal9z1", "b:xsig5k2", "b:authres3q", "b:bouncerecv1", "b:arcms2w9", "b:gsig6t"}
	kept := []string{"b:rechnung", "b:inneres", "b:msgid8a", "b:bob", "b:bestellung", "b:carol", "b:anhang", "b:gescheitert", "h:outersig0k9"}

	all := append(TermsWith([]byte(forwardMsg), TextOptions{}), TermsWith([]byte(bounceMsg), TextOptions{})...)
	nosig := append(TermsWith([]byte(forwardMsg), TextOptions{AttachedHeaders: AttachedHeadersNoSig}),
		TermsWith([]byte(bounceMsg), TextOptions{AttachedHeaders: AttachedHeadersNoSig})...)

	for _, term := range sigTerms {
		if !slices.Contains(all, term) {
			t.Errorf("all: %s missing", term)
		}

		if slices.Contains(nosig, term) {
			t.Errorf("nosig: %s indexed", term)
		}
	}

	for _, term := range kept {
		if !slices.Contains(nosig, term) || !slices.Contains(all, term) {
			t.Errorf("%s must stay (all %v, nosig %v)", term, slices.Contains(all, term), slices.Contains(nosig, term))
		}
	}

	// The default of the process is "all" and gives the same terms.
	if got := Terms([]byte(forwardMsg)); !slices.Equal(got, TermsWith([]byte(forwardMsg), TextOptions{AttachedHeaders: AttachedHeadersAll})) {
		t.Error("the default text options differ from all")
	}
}

// TestSetAttachedHeaders: the process setting moves BodyText (the IMAP
// check) and Terms with it; an unknown mode changes nothing.
func TestSetAttachedHeaders(t *testing.T) {
	t.Cleanup(func() { _ = SetAttachedHeaders(AttachedHeadersAll) })

	if err := SetAttachedHeaders("basic"); err == nil {
		t.Fatal("unknown mode accepted")
	}

	if !strings.Contains(BodyText([]byte(forwardMsg)), "dkimsig4f8q") {
		t.Fatal("all: the signature is not in the body text")
	}

	if err := SetAttachedHeaders(AttachedHeadersNoSig); err != nil {
		t.Fatal(err)
	}

	if body := BodyText([]byte(forwardMsg)); strings.Contains(body, "dkimsig4f8q") || !strings.Contains(body, "Rechnung innen") {
		t.Fatalf("nosig body text %q", body)
	}

	if !DefaultTextOptions().NoSig() || slices.Contains(Terms([]byte(forwardMsg)), "b:dkimsig4f8q") {
		t.Fatal("Terms does not follow the setting")
	}
}

// TestWithoutSignatureFields: continuation lines go with their field.
func TestWithoutSignatureFields(t *testing.T) {
	in := "Received: a\r\n\tb\r\nSubject: s\r\n x\r\nDKIM-Signature: v=1\r\n b=c\r\nTo: t\r\n"
	if got, want := withoutSignatureFields(in), "Subject: s\r\n x\r\nTo: t\r\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
