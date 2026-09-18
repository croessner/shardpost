package imapproto

// Command names as they appear on the wire (upper-cased by the parser).
const (
	kwAppend       = "APPEND"
	kwAuthenticate = "AUTHENTICATE"
	kwCapability   = "CAPABILITY"
	kwCheck        = "CHECK"
	kwClose        = "CLOSE"
	kwCopy         = "COPY"
	kwCreate       = "CREATE"
	kwDelete       = "DELETE"
	kwDeleteACL    = "DELETEACL"
	kwEnable       = "ENABLE"
	kwExamine      = "EXAMINE"
	kwExpunge      = "EXPUNGE"
	kwFetch        = "FETCH"
	kwID           = "ID"
	kwGetQuota     = "GETQUOTA"
	kwList         = "LIST"
	kwLogin        = "LOGIN"
	kwLsub         = "LSUB"
	kwMove         = "MOVE"
	kwMyRights     = "MYRIGHTS"
	kwReplace      = "REPLACE"
	kwSearch       = "SEARCH"
	kwSelect       = "SELECT"
	kwSort         = "SORT"
	kwStatus       = "STATUS"
	kwStore        = "STORE"
	kwSubscribe    = "SUBSCRIBE"
	kwThread       = "THREAD"
	kwUID          = "UID"
	kwUnselect     = "UNSELECT"
	kwUnsubscribe  = "UNSUBSCRIBE"
)

// Data items, search keys and response keywords.
const (
	kwAll           = "ALL"
	kwBcc           = "BCC"
	kwBinary        = "BINARY"
	kwBinarySize    = "BINARY.SIZE"
	kwBody          = "BODY"
	kwBodyStructure = "BODYSTRUCTURE"
	kwCc            = "CC"
	kwCondstore     = "CONDSTORE"
	kwDeleted       = "DELETED"
	kwEmailID       = "EMAILID"
	kwEnvelope      = "ENVELOPE"
	kwFlags         = "FLAGS"
	kwFrom          = "FROM"
	kwHeader        = "HEADER"
	kwInternalDate  = "INTERNALDATE"
	kwMailboxes     = "MAILBOXES"
	kwMessageNew    = "MESSAGENEW"
	kwMessages      = "MESSAGES"
	kwModSeq        = "MODSEQ"
	kwNIL           = "NIL"
	kwPreview       = "PREVIEW"
	kwQresync       = "QRESYNC"
	kwRecent        = "RECENT"
	kwRFC822        = "RFC822"
	kwRFC822Header  = "RFC822.HEADER"
	kwRFC822Size    = "RFC822.SIZE"
	kwRFC822Text    = "RFC822.TEXT"
	kwSaveDate      = "SAVEDATE"
	kwSelectedDelay = "SELECTED-DELAYED"
	kwSize          = "SIZE"
	kwSpecialUse    = "SPECIAL-USE"
	kwSubject       = "SUBJECT"
	kwSubscribed    = "SUBSCRIBED"
	kwSubtree       = "SUBTREE"
	kwText          = "TEXT"
	kwThreadID      = "THREADID"
	kwTo            = "TO"
	kwUIDNext       = "UIDNEXT"
	kwUIDValidity   = "UIDVALIDITY"
	kwUnseen        = "UNSEEN"
	kwUTF8Accept    = "UTF8=ACCEPT"
)

const (
	// respCodeTooBig is the response code for oversized messages (RFC 7889).
	respCodeTooBig = "TOOBIG"
	// serverName is the server's name in ID responses.
	serverName = "ShardPost"
	// childInfoSubscribed is the LIST-EXTENDED CHILDINFO extended data item
	// for a name listed only for its subscribed children (RFC 5258 3.5).
	childInfoSubscribed = ` ("CHILDINFO" ("SUBSCRIBED"))`
)

// Special-use attributes (RFC 6154) and system flags.
const (
	useArchive   = `\Archive`
	useDrafts    = `\Drafts`
	useJunk      = `\Junk`
	useSent      = `\Sent`
	useTrash     = `\Trash`
	flagAnswered = `\Answered`
	flagDeleted  = `\Deleted`
	flagDraft    = `\Draft`
	flagFlagged  = `\Flagged`
	flagSeen     = `\Seen`
)

// Lower-cased address header names.
const (
	hdrFrom = "from"
	hdrTo   = "to"
	hdrCc   = "cc"
)

// SASL mechanism names.
const (
	kwOAuthBearer = "OAUTHBEARER"
	kwPlain       = "PLAIN"
	kwXOAuth2     = "XOAUTH2"
)

// protoIMAP is the protocol name used for authentication requests and
// metric labels.
const protoIMAP = "imap"

// Lower-cased system flags, for case-insensitive comparisons.
const (
	flagSeenLower    = `\seen`
	flagDeletedLower = `\deleted`
)

// systemFlags returns the system flags a mailbox always offers, in the
// order FLAGS and PERMANENTFLAGS list them.
func systemFlags() []string {
	return []string{flagSeen, flagAnswered, flagFlagged, flagDeleted, flagDraft}
}
