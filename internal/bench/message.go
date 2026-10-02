package bench

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"time"
)

// Message generation. A bench message is deterministic in (seed,
// account, number): a re-run produces the same bytes. Its text is drawn
// from a Zipf distribution over a vocabulary of common words and
// synthetic terms, so the full-text index sees a realistic mix of
// frequent and rare terms; a message above textMax carries a base64
// attachment for the rest of its size, as large mail usually does.

const (
	// minMessageSize is the smallest size a bucket may ask for (the
	// header alone is about this long).
	minMessageSize = 512
	// textMax is the largest message that is text only.
	textMax = 32 << 10
	// attachmentText is the text part of a message with attachment.
	attachmentText = 4 << 10
	// vocabSize is the number of distinct terms the text draws from.
	vocabSize = 50000
	// zipfS is the exponent of the term distribution.
	zipfS = 1.1
	crlf  = "\r\n"
)

// common are frequent words; the rest of the vocabulary is synthetic.
var common = []string{
	"the", "and", "for", "you", "with", "this", "that", "have", "from", "your",
	"meeting", "please", "invoice", "order", "report", "project", "update", "thanks", "regards", "team",
	"der", "die", "und", "nicht", "mit", "eine", "bitte", "danke", "gruss", "termin",
	"contract", "payment", "delivery", "schedule", "review", "release", "server", "customer", "question", "answer",
}

var syllables = []string{
	"ka", "lo", "mi", "ne", "ru", "sa", "te", "vi", "zo", "ber", "dan", "fel", "gor", "han", "jis",
	"kel", "lun", "mor", "nap", "pel", "quo", "ris", "sto", "tur", "ulm", "ven", "wex", "yar", "zen", "ox",
}

// term returns term i of the vocabulary.
func term(i uint64) string {
	if i < uint64(len(common)) {
		return common[i]
	}

	var b []byte

	n := i - uint64(len(common))
	for {
		b = append(b, syllables[n%uint64(len(syllables))]...)

		n /= uint64(len(syllables))
		if n == 0 {
			break
		}
	}

	return string(b)
}

// Generator produces the bench messages.
type Generator struct {
	Seed  uint64
	Sizes SizeDist
}

// rng is the random source of message n of account.
func (g Generator) rng(account string, n int) *rand.Rand {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s/%d", account, n)

	return rand.New(rand.NewPCG(g.Seed, h.Sum64()))
}

// Message returns message n of account and the random source that
// produced it (the caller draws its target mailbox from it).
func (g Generator) Message(account string, n int) ([]byte, *rand.Rand) {
	r := g.rng(account, n)
	size := g.Sizes.Pick(r)
	zipf := rand.NewZipf(r, zipfS, 1, vocabSize-1)

	var b bytes.Buffer

	b.Grow(size + 256)

	date := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Hour)

	fmt.Fprintf(&b, "From: %s <%s@example.org>%s", term(zipf.Uint64()), term(zipf.Uint64()), crlf)
	fmt.Fprintf(&b, "To: <%s@bench.invalid>%s", account, crlf)
	fmt.Fprintf(&b, "Subject: %s%s", words(zipf, 3+r.IntN(6)), crlf)
	fmt.Fprintf(&b, "Date: %s%s", date.Format(time.RFC1123Z), crlf)
	fmt.Fprintf(&b, "Message-ID: <%016x.%d@bench.invalid>%s", r.Uint64(), n, crlf)
	b.WriteString("MIME-Version: 1.0" + crlf)

	if size <= textMax {
		b.WriteString("Content-Type: text/plain; charset=utf-8" + crlf + crlf)
		text(&b, r, zipf, size)

		return b.Bytes(), r
	}

	const boundary = "bench-boundary"

	b.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"" + crlf + crlf)
	b.WriteString("--" + boundary + crlf + "Content-Type: text/plain; charset=utf-8" + crlf + crlf)
	text(&b, r, zipf, b.Len()+attachmentText)
	b.WriteString(crlf + "--" + boundary + crlf +
		"Content-Type: application/octet-stream; name=\"data.bin\"" + crlf +
		"Content-Transfer-Encoding: base64" + crlf +
		"Content-Disposition: attachment; filename=\"data.bin\"" + crlf + crlf)
	attachment(&b, r, size-b.Len()-len(boundary)-8)
	b.WriteString("--" + boundary + "--" + crlf)

	return b.Bytes(), r
}

// words returns n terms separated by spaces.
func words(zipf *rand.Zipf, n int) string {
	var b bytes.Buffer

	for i := range n {
		if i > 0 {
			b.WriteByte(' ')
		}

		b.WriteString(term(zipf.Uint64()))
	}

	return b.String()
}

// text appends lines of terms until b holds size bytes.
func text(b *bytes.Buffer, r *rand.Rand, zipf *rand.Zipf, size int) {
	for b.Len() < size {
		b.WriteString(words(zipf, 6+r.IntN(8)))
		b.WriteString(crlf)
	}
}

// attachment appends base64 lines of random data, about n bytes.
func attachment(b *bytes.Buffer, r *rand.Rand, n int) {
	const lineRaw = 57 // 76 base64 characters

	raw := make([]byte, lineRaw)
	line := make([]byte, base64.StdEncoding.EncodedLen(lineRaw))

	for written := 0; written < n; written += len(line) + len(crlf) {
		for i := range raw {
			raw[i] = byte(r.Uint32())
		}

		base64.StdEncoding.Encode(line, raw)
		b.Write(line)
		b.WriteString(crlf)
	}
}
