package jmap

// Spellings that several files of the package share: filter operators,
// header field names, transfer encodings and the like.

// The operators of a FilterOperator (RFC 8620 5.5).
const (
	opAND = "AND"
	opOR  = "OR"
	opNOT = "NOT"
)

// Header field names, in the canonical form net/textproto gives them.
const (
	fieldFrom        = "From"
	fieldTo          = "To"
	fieldCc          = "Cc"
	fieldBcc         = "Bcc"
	fieldSender      = "Sender"
	fieldReplyTo     = "Reply-To"
	fieldSubject     = "Subject"
	fieldDate        = "Date"
	fieldMessageID   = "Message-Id"
	fieldInReplyTo   = "In-Reply-To"
	fieldReferences  = "References"
	fieldContentType = "Content-Type"
	fieldCTE         = "Content-Transfer-Encoding"
	fieldDisposition = "Content-Disposition"
	fieldMIMEVersion = "Mime-Version"
)

// Transfer encodings (RFC 2045 6), charsets and dispositions.
const (
	enc7bit        = "7bit"
	enc8bit        = "8bit"
	encBase64      = "base64"
	encQP          = "quoted-printable"
	charsetUTF8    = "utf-8"
	dispInline     = "inline"
	dispAttachment = "attachment"
	mediaMixed     = "multipart/mixed"
)

// Argument names and descriptions more than one method uses.
const (
	schemeHTTPS          = "https"
	argFilter            = "filter"
	descFilterObject     = "a filter is an object"
	descTooManyObjects   = "more objects than maxObjectsInSet"
	descTooLarge         = "the message is larger than maxSizeAttachmentsPerEmail"
	descTooManyIDs       = "more ids than maxObjectsInGet"
	descPartialNoMsgs    = "a change was recorded without its messages"
	propAllInThreadHave  = "allInThreadHaveKeyword"
	propSomeInThreadHave = "someInThreadHaveKeyword"
)
