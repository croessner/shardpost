package jmap

import "shardpost/internal/mimepart"

// A message is taken apart by internal/mimepart, which the store uses as
// well to record hasAttachment in the index record: the two have to
// agree on what a part and an attachment are. The names here keep the
// JMAP code short.

// The media types and the charset the MIME defaults are made of.
const (
	mediaTextPlain   = mimepart.MediaTextPlain
	mediaTextHTML    = mimepart.MediaTextHTML
	mediaOctetStream = mimepart.MediaOctetStream
	charsetASCII     = mimepart.CharsetASCII
)

// Limits of the walk: a message nested deeper than this, or with more
// parts, is one whose structure this server does not describe in full.
const (
	maxPartDepth = mimepart.MaxDepth
	maxParts     = mimepart.MaxParts
)

// mimePart is one part of a message.
type mimePart = mimepart.Part

// parseMessage takes a message apart.
func parseMessage(raw []byte) (*mimePart, error) { return mimepart.Parse(raw) }

// opaqueMessage is a message described as one part.
func opaqueMessage(raw []byte) *mimePart { return mimepart.Opaque(raw) }

// splitHeader separates a part's header from its body.
var splitHeader = mimepart.SplitHeader

// headerEnd is the offset just past the empty line that ends a header,
// or -1.
var headerEnd = mimepart.HeaderEnd

// splitMultipart cuts a multipart body into its parts.
var splitMultipart = mimepart.SplitMultipart

// flatten derives textBody, htmlBody and attachments (RFC 8621 4.1.4).
var flatten = mimepart.Flatten

// hasAttachment reports whether a list of attachments holds one that is
// not inline.
var hasAttachment = mimepart.HasAttachment

// leaves are the parts of a message that are not multiparts, in order.
var leaves = mimepart.Leaves

// partFileName is the file name of a part.
var partFileName = mimepart.FileName

// dispositionOf is the lower-cased disposition type of a part.
var dispositionOf = mimepart.Disposition
