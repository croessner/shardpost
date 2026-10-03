package bench

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"strings"
	"time"

	"shardpost/internal/fts"
)

// The identifier corpus (temp/13 §7.2, §7.4). The plain corpus is
// natural vocabulary only; real mail carries identifiers that are new in
// every message — tracking tokens, hashes, base64 runs, the signatures
// and trace fields of attached or bounced messages — and they make the
// dictionary grow linearly with the messages. The corpus "ids" adds
// them; "ids,nosig" is the same messages without the signature and
// trace fields of the attached headers (the deny list of temp/13 §3 c3),
// drawn from the same random source, so the difference of the two
// measures what those fields cost. Both are deterministic in (seed,
// account, number), like the plain corpus.

// Corpus names (populate, measure and shardpost-load -corpus).
const (
	CorpusPlain    = "plain"
	CorpusIDs      = "ids"
	CorpusIDsNoSig = "ids,nosig"
)

// Corpus selects the content of the bench messages.
type Corpus struct {
	// IDs adds identifiers; NoSig leaves the signature and trace fields
	// out of the attached headers (only with IDs).
	IDs, NoSig bool
}

// ParseCorpus parses a corpus name ("" is plain).
func ParseCorpus(s string) (Corpus, error) {
	switch strings.ReplaceAll(s, " ", "") {
	case "", CorpusPlain:
		return Corpus{}, nil
	case CorpusIDs:
		return Corpus{IDs: true}, nil
	case CorpusIDsNoSig:
		return Corpus{IDs: true, NoSig: true}, nil
	}

	return Corpus{}, fmt.Errorf("unknown corpus %q (plain, ids, ids,nosig)", s)
}

// String is the corpus name.
func (c Corpus) String() string {
	switch {
	case c.IDs && c.NoSig:
		return CorpusIDsNoSig
	case c.IDs:
		return CorpusIDs
	default:
		return CorpusPlain
	}
}

// Shares of the identifier corpus, in percent of the messages.
const (
	// attachedShare of the messages carry a forwarded message
	// (message/rfc822) or a bounce (text/rfc822-headers) with full
	// headers.
	attachedShare = 5
	// base64Share carry a base64 block in their text.
	base64Share = 2
)

// SignatureField reports whether a header field is a signature or trace
// field that the corpus "ids,nosig" leaves out of attached headers: the
// fields fts.attached_headers "nosig" leaves out of the body text
// (fts.SignatureField), so that the corpus measures that setting.
func SignatureField(name string) bool { return fts.SignatureField(name) }

// RefID is the reference of message n of account in the identifier
// corpus: 32 hex digits with letters and digits, in the text of the
// message as "Ref: <id>". The search probe finds a message by it.
func (g Generator) RefID(account string, n int) string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "ref/%d/%s/%d", g.Seed, account, n)
	r := rand.New(rand.NewPCG(h.Sum64(), g.Seed)) //nolint:gosec // bench data, not a secret

	b := make([]byte, 16)
	for i := range b {
		b[i] = byte(r.Uint32())
	}

	// a letter first and a digit last: always an identifier
	id := []byte(hex.EncodeToString(b))
	id[0] = "abcdef"[r.IntN(6)]
	id[len(id)-1] = "0123456789"[r.IntN(10)]

	return string(id)
}

// idMessage writes message n of account of the identifier corpus.
func (g Generator) idMessage(account string, n int) ([]byte, *rand.Rand) {
	r := g.rng(account, n)
	size := g.Sizes.Pick(r)
	zipf := rand.NewZipf(r, zipfS, 1, vocabSize-1)

	var b bytes.Buffer

	b.Grow(size + 4096)
	header(&b, r, zipf, account, n)

	// What the message carries, drawn before any of it is written.
	attached := r.IntN(100) < attachedShare
	bounce := attached && r.IntN(2) == 0
	block := r.IntN(100) < base64Share

	var ids bytes.Buffer

	g.idLines(&ids, r, account, n, block)

	if !attached && size <= textMax {
		b.WriteString("Content-Type: text/plain; charset=utf-8" + crlf + crlf)
		b.Write(ids.Bytes())
		text(&b, r, zipf, size)

		return b.Bytes(), r
	}

	const boundary = "bench-boundary"

	if bounce {
		b.WriteString("Content-Type: multipart/report; report-type=delivery-status; boundary=\"" + boundary + "\"" + crlf + crlf)
	} else {
		b.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"" + crlf + crlf)
	}

	b.WriteString("--" + boundary + crlf + "Content-Type: text/plain; charset=utf-8" + crlf + crlf)
	b.Write(ids.Bytes())

	textEnd := size
	if size > textMax {
		textEnd = b.Len() + attachmentText
	}

	text(&b, r, zipf, textEnd)

	// The binary attachment's size depends on nothing that NoSig leaves
	// out: it comes before the attached headers.
	if size > textMax && !bounce {
		b.WriteString(crlf + "--" + boundary + crlf +
			"Content-Type: application/octet-stream; name=\"data.bin\"" + crlf +
			"Content-Transfer-Encoding: base64" + crlf +
			"Content-Disposition: attachment; filename=\"data.bin\"" + crlf + crlf)
		attachment(&b, r, size-b.Len()-len(boundary)-8)
	}

	if attached {
		g.attachedPart(&b, r, zipf, boundary, account, n, bounce)
	}

	b.WriteString(crlf + "--" + boundary + "--" + crlf)

	return b.Bytes(), r
}

// idLines writes the identifiers of the text: the reference, hex ids of
// 16, 32 or 64 digits, tracking links and sometimes a base64 block.
func (g Generator) idLines(b *bytes.Buffer, r *rand.Rand, account string, n int, block bool) {
	b.WriteString("Ref: " + g.RefID(account, n) + crlf)

	for range r.IntN(2) {
		b.WriteString("Id: " + hexOf(r, []int{16, 32, 64}[r.IntN(3)]) + crlf)
	}

	for range r.IntN(2) {
		b.WriteString("https://t.example.net/c/" + base64URLOf(r, 20+r.IntN(101)) + crlf)
	}

	toks := make([]string, 0, 50)
	for range 40 + r.IntN(31) {
		toks = append(toks, shortToken(r))
	}

	b.WriteString("Details: " + strings.Join(toks, " ") + crlf)

	if block {
		for range 5 + r.IntN(16) {
			b.WriteString(base64Of(r, 76) + crlf)
		}
	}
}

// attachedPart writes a forwarded message (message/rfc822) or the
// headers of a bounced one (text/rfc822-headers, after the delivery
// status) with full headers; NoSig leaves the signature and trace fields
// out, after drawing them.
func (g Generator) attachedPart(b *bytes.Buffer, r *rand.Rand, zipf *rand.Zipf, boundary, account string, n int, bounce bool) {
	if bounce {
		b.WriteString(crlf + "--" + boundary + crlf + "Content-Type: message/delivery-status" + crlf + crlf +
			"Reporting-MTA: dns; mx.example.net" + crlf + crlf +
			"Final-Recipient: rfc822; " + term(zipf.Uint64()) + "@example.com" + crlf + "Action: failed" + crlf + "Status: 5.1.1" + crlf)
		b.WriteString(crlf + "--" + boundary + crlf + "Content-Type: text/rfc822-headers" + crlf + crlf)
	} else {
		b.WriteString(crlf + "--" + boundary + crlf + "Content-Type: message/rfc822" + crlf + crlf)
	}

	for _, f := range g.attachedHeaders(r, zipf, account, n) {
		if g.Corpus.NoSig && SignatureField(f.name) {
			continue
		}

		b.WriteString(f.name + ": " + f.value + crlf)
	}

	if bounce {
		return
	}

	b.WriteString(crlf)
	text(b, r, zipf, b.Len()+512+r.IntN(2048))
}

type headerField struct{ name, value string }

// attachedHeaders draws the header of an attached message: three
// Received fields, ARC, authentication results, two DKIM signatures and
// the usual fields.
func (g Generator) attachedHeaders(r *rand.Rand, zipf *rand.Zipf, account string, n int) []headerField {
	sender := term(zipf.Uint64())
	domain := term(zipf.Uint64()) + ".example.com"
	ip := fmt.Sprintf("192.0.2.%d", 1+r.IntN(254))
	date := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n)*time.Hour + time.Duration(r.IntN(3600))*time.Second)
	stamp := fmt.Sprint(date.Unix())

	var out []headerField

	add := func(name, value string) { out = append(out, headerField{name, value}) }

	for i := range 3 {
		add("Received", fmt.Sprintf("from mail-%s.%s (mail-%s.%s [%s])%s\tby mx%d.example.net with ESMTPS id %s-%s%s\tfor <%s@bench.invalid>; %s",
			alnumOf(r, 8), domain, alnumOf(r, 8), domain, ip, crlf, i+1, hexOf(r, 13), hexOf(r, 12), crlf, account, date.Format(time.RFC1123Z)))
	}

	sig := func() string { return folded(base64Of(r, 344)) }
	bh := base64Of(r, 44)

	add("ARC-Seal", "i=1; a=rsa-sha256; t="+stamp+"; cv=none; d="+domain+"; s=arc-20240605;"+crlf+"\tb="+sig())
	add("ARC-Message-Signature", "i=1; a=rsa-sha256; c=relaxed/relaxed; d="+domain+"; s=arc-20240605;"+crlf+
		"\th=from:to:subject:date:message-id:mime-version; bh="+bh+";"+crlf+"\tb="+sig())
	add("ARC-Authentication-Results", "i=1; mx.example.net; dkim=pass header.i=@"+domain+" header.s=s1 header.b="+base64Of(r, 8)+
		"; spf=pass smtp.mailfrom="+sender+"@"+domain+"; dmarc=pass header.from="+domain)
	add("Authentication-Results", "mx.example.net; dkim=pass header.i=@"+domain+" header.s=s1 header.b="+base64Of(r, 8)+
		"; spf=pass smtp.mailfrom="+sender+"@"+domain)
	add("Received-SPF", "pass (mx.example.net: domain of "+sender+"@"+domain+" designates "+ip+" as permitted sender) client-ip="+ip+";")
	add("DKIM-Signature", "v=1; a=rsa-sha256; c=relaxed/relaxed; d="+domain+"; s=s1; t="+stamp+";"+crlf+
		"\th=from:to:subject:date:message-id; bh="+bh+";"+crlf+"\tb="+sig())
	add("X-Google-DKIM-Signature", "v=1; a=rsa-sha256; c=relaxed/relaxed; d=1e100.net; s=20230601; t="+stamp+";"+crlf+
		"\th=from:to:subject:date:message-id:x-gm-message-state; bh="+base64Of(r, 44)+";"+crlf+"\tb="+sig())
	add("From", sender+" <"+sender+"@"+domain+">")
	add("To", "<"+account+"@bench.invalid>")
	add("Subject", words(zipf, 3+r.IntN(6)))
	add("Date", date.Format(time.RFC1123Z))
	add("Message-ID", "<"+alnumOf(r, 24)+"@mail."+domain+">")
	add("MIME-Version", "1.0")
	add("Content-Type", "text/plain; charset=utf-8")

	return out
}

// folded folds a long value at 72 characters.
func folded(s string) string {
	var b strings.Builder

	for len(s) > 72 {
		b.WriteString(s[:72] + crlf + "\t")
		s = s[72:]
	}

	b.WriteString(s)

	return b.String()
}

func randomBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}

	return b
}

// hexOf is n hex digits.
func hexOf(r *rand.Rand, n int) string { return hex.EncodeToString(randomBytes(r, (n+1)/2))[:n] }

// base64Of is n characters of standard base64.
func base64Of(r *rand.Rand, n int) string {
	return base64.StdEncoding.EncodeToString(randomBytes(r, n))[:n]
}

// base64URLOf is n characters of URL-safe base64.
func base64URLOf(r *rand.Rand, n int) string {
	return base64.RawURLEncoding.EncodeToString(randomBytes(r, n))[:n]
}

// shortToken is a short token, often new in a message, 3 to 5
// characters: a number, a code of letters and digits, or a name.
func shortToken(r *rand.Rand) string {
	n := 3 + r.IntN(3)

	switch k := r.IntN(10); {
	case k < 4:
		return digitsOf(r, n)
	case k < 7:
		return alnumOf(r, n)
	default:
		return lettersOf(r, n)
	}
}

func digitsOf(r *rand.Rand, n int) string { return charsOf(r, n, "0123456789") }

func lettersOf(r *rand.Rand, n int) string { return charsOf(r, n, "abcdefghijklmnopqrstuvwxyz") }

func charsOf(r *rand.Rand, n int, chars string) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[r.IntN(len(chars))]
	}

	return string(b)
}

// alnumOf is n lower-case letters and digits.
func alnumOf(r *rand.Rand, n int) string {
	return charsOf(r, n, "abcdefghijklmnopqrstuvwxyz0123456789")
}
