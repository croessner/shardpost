package jmap

import (
	"bufio"
	"bytes"
	"errors"

	"github.com/emersion/go-message/textproto"

	"shardpost/internal/model"
)

// The index keeps an IMAP envelope per message, and most of what JMAP
// asks about a message's header is in it exactly: the subject (decoded),
// the address lists (parsed from the real headers), the date, the
// Message-ID and In-Reply-To. Three properties are not: an IMAP envelope
// fills in an absent Sender and Reply-To from From (RFC 3501 7.4.2), where
// JMAP says null, and the index merges References with In-Reply-To for
// threading. For those the header block of the message is read — only
// the header, which sits at the front of the first chunk.

// headerProbe is how much of a message is read first to find the end of
// its header; headerMax is where the search gives up. A header of more
// than a megabyte is a message this server will not parse the header of.
const (
	headerProbe = 16 << 10
	headerMax   = 1 << 20
)

// errNoHeaderEnd is what a message without an end to its header gets.
var errNoHeaderEnd = errors.New("the header of the message does not end")

// readHeader reads and parses the header block of a stored message.
func readHeader(blobs model.Blobs, guid string, size int64) (textproto.Header, error) {
	want := int64(headerProbe)

	for {
		raw, err := blobs.ReadBlob(guid, size, 0, min(want, size))
		if err != nil {
			return textproto.Header{}, err
		}

		if end := headerEnd(raw); end >= 0 || int64(len(raw)) >= size {
			if end < 0 {
				end = len(raw)
			}

			return textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw[:end])))
		}

		if want >= headerMax {
			return textproto.Header{}, errNoHeaderEnd
		}

		want *= 4
	}
}
