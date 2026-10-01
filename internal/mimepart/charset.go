package mimepart

import "shardpost/internal/charset"

// WordDecoder decodes RFC 2047 encoded words in every charset of the
// WHATWG list; see charset.WordDecoder.
var WordDecoder = charset.WordDecoder
