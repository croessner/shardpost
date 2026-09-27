package jmap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"shardpost/internal/provision"
)

// Account is one account of the session resource (RFC 8620 2).
type Account struct {
	AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
	Name                string                     `json:"name"`
	IsPersonal          bool                       `json:"isPersonal"`
	IsReadOnly          bool                       `json:"isReadOnly"`
}

// Session is the session resource (RFC 8620 2).
type Session struct {
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	Accounts        map[string]Account         `json:"accounts"`
	PrimaryAccounts map[string]string          `json:"primaryAccounts"`
	Username        string                     `json:"username"`
	APIURL          string                     `json:"apiUrl"`
	DownloadURL     string                     `json:"downloadUrl"`
	UploadURL       string                     `json:"uploadUrl"`
	EventSourceURL  string                     `json:"eventSourceUrl"`
	State           string                     `json:"state"`
}

// coreCapability is the value of the core capability in the session
// resource: the limits, and the collations the server can sort by.
type coreCapability struct {
	CollationAlgorithms   []string `json:"collationAlgorithms"`
	MaxSizeUpload         int64    `json:"maxSizeUpload"`
	MaxSizeRequest        int64    `json:"maxSizeRequest"`
	MaxConcurrentUpload   int      `json:"maxConcurrentUpload"`
	MaxConcurrentRequests int      `json:"maxConcurrentRequests"`
	MaxCallsInRequest     int      `json:"maxCallsInRequest"`
	MaxObjectsInGet       int      `json:"maxObjectsInGet"`
	MaxObjectsInSet       int      `json:"maxObjectsInSet"`
}

// handleSession answers the session resource.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess := sessionOf(r)
	s.provision(sess)

	out := s.session(sess.account, s.baseURL(r))

	w.Header().Set(fieldContentType, "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.o.Log.Warn("jmap: session response not written", "err", err)
	}
}

// session builds the session resource of an account.
func (s *Server) session(account, base string) *Session {
	id := accountID(account)

	out := &Session{
		Capabilities: map[string]json.RawMessage{
			CapCore: mustRaw(s.core()),
			CapMail: mustRaw(s.mail()),
			// RFC 9425 3: the quota capability has no properties, in
			// the session or in an account.
			CapQuota: json.RawMessage(`{}`),
			// RFC 9404 3.1: empty in the session, the limits are the
			// account's.
			CapBlob: json.RawMessage(`{}`),
		},
		Accounts:        map[string]Account{id: s.account(account)},
		PrimaryAccounts: map[string]string{CapMail: id, CapQuota: id, CapBlob: id},
		Username:        account,
		APIURL:          base + PathAPI,
		DownloadURL:     base + PathDownload + "{accountId}/{blobId}/{name}?accept={type}",
		UploadURL:       base + PathUpload + "{accountId}/",
		EventSourceURL:  base + PathEvents + "?types={types}&closeafter={closeafter}&ping={ping}",
	}

	if s.o.Submission != nil {
		out.Capabilities[CapSubmission] = mustRaw(submissionCapability())
		out.PrimaryAccounts[CapSubmission] = id
	}

	if s.o.VacationResponse {
		out.Capabilities[CapVacationResponse] = json.RawMessage(`{}`)
		out.PrimaryAccounts[CapVacationResponse] = id
	}

	if s.o.Push.KV != nil && s.o.Push.VAPIDPublicKey != "" {
		out.Capabilities[CapWebPushVAPID] = mustRaw(map[string]string{"applicationServerKey": s.o.Push.VAPIDPublicKey})
	}

	out.State = sessionState(out)

	return out
}

// core is the core capability of this server.
func (s *Server) core() *coreCapability {
	l := s.o.Limits

	return &coreCapability{
		MaxSizeUpload:         l.MaxSizeUpload,
		MaxConcurrentUpload:   l.MaxConcurrentUpload,
		MaxSizeRequest:        l.MaxSizeRequest,
		MaxConcurrentRequests: l.MaxConcurrentRequests,
		MaxCallsInRequest:     l.MaxCallsInRequest,
		MaxObjectsInGet:       l.MaxObjectsInGet,
		MaxObjectsInSet:       l.MaxObjectsInSet,
		// Only the one the sorts actually use: comparing the bytes.
		// Advertising more would be a promise this server does not keep.
		CollationAlgorithms: []string{collationCasemap, collationOctet},
	}
}

// mailCapability is the value of the mail capability in the session
// resource (RFC 8621 1.4).
type mailCapability struct {
	MaxMailboxesPerEmail       *uint64  `json:"maxMailboxesPerEmail"`
	MaxMailboxDepth            *uint64  `json:"maxMailboxDepth"`
	EmailQuerySortOptions      []string `json:"emailQuerySortOptions"`
	MaxSizeMailboxName         uint64   `json:"maxSizeMailboxName"`
	MaxSizeAttachmentsPerEmail int64    `json:"maxSizeAttachmentsPerEmail"`
	MayCreateTopLevelMailbox   bool     `json:"mayCreateTopLevelMailbox"`
}

// The collations this server sorts text with (RFC 4790): ASCII letters
// without regard to case, the default, and octet by octet.
const (
	collationCasemap = "i;ascii-casemap"
	collationOctet   = "i;octet"
)

// validCollation reports whether a comparator's collation is one this
// server has; none means the default.
func validCollation(c string) bool {
	return c == "" || c == collationCasemap || c == collationOctet
}

// collate is a text as a collation compares it: i;ascii-casemap folds the
// ASCII letters to lower case and leaves every other byte alone.
func collate(collation, s string) string {
	if collation == collationOctet {
		return s
	}

	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}

	return string(b)
}

// mail is the mail capability of this server. The sort options of
// Email/query are empty because there is no Email/query yet: a client
// reads the list and asks for nothing this server cannot do.
func (s *Server) mail() *mailCapability {
	return &mailCapability{
		// No limit. An IMAP COPY keeps the GUID of the message, which
		// is its JMAP id, so one message can already sit in any number
		// of mailboxes; a limit here would be a claim the store breaks.
		MaxMailboxesPerEmail:       nil,
		MaxMailboxDepth:            nil,
		MaxSizeMailboxName:         maxMailboxNameBytes,
		MaxSizeAttachmentsPerEmail: s.o.Limits.MaxSizeUpload,
		EmailQuerySortOptions:      emailSortOptions,
		MayCreateTopLevelMailbox:   true,
	}
}

// account describes the account itself. Only the account of the
// authenticated user is listed; the accounts of other users (shared
// mailboxes, the public namespace) belong to JMAP Sharing, which is not
// built yet, and listing them empty would be a lie about what works.
func (s *Server) account(name string) Account {
	a := Account{
		Name:       name,
		IsPersonal: true,
		IsReadOnly: false,
		AccountCapabilities: map[string]json.RawMessage{
			CapCore:  json.RawMessage(`{}`),
			CapMail:  mustRaw(s.mail()),
			CapQuota: json.RawMessage(`{}`),
			CapBlob:  mustRaw(s.blobCapability()),
		},
	}

	if s.o.Submission != nil {
		a.AccountCapabilities[CapSubmission] = mustRaw(submissionCapability())
	}

	if s.o.VacationResponse {
		a.AccountCapabilities[CapVacationResponse] = json.RawMessage(`{}`)
	}

	return a
}

// submissionCapability is the value of the submission capability (RFC
// 8621 1.3.2). Delayed sending is not offered.
func submissionCapability() map[string]any {
	return map[string]any{"maxDelayedSend": 0, "submissionExtensions": map[string][]string{}}
}

// accountID is the id an account is addressed by in URLs and arguments.
// The canonical account name is already stable, unique within the
// server and the thing every log line names, so it is the id as well; an
// opaque number would only add a mapping to keep correct.
func accountID(account string) string { return account }

// sessionState changes whenever anything in the session object changes,
// which is what RFC 8620 2 asks of it. It is a hash of the object rather
// than a counter: there is no state to keep, and two servers of the same
// deployment compute the same value for the same account.
func sessionState(s *Session) string {
	cp := *s
	cp.State = ""

	sum := sha256.Sum256(mustRaw(&cp))

	return hex.EncodeToString(sum[:8])
}

// baseURL is the origin (and path prefix) the session resource
// advertises. Behind the Director the server must advertise the
// Director's address, because its default mode rewrites nothing; without
// a configured base the request is the best available answer.
func (s *Server) baseURL(r *http.Request) string {
	if s.o.PublicBaseURL != "" {
		return strings.TrimSuffix(s.o.PublicBaseURL, "/")
	}

	scheme := "http"
	if r.TLS != nil {
		scheme = schemeHTTPS
	}

	return scheme + "://" + r.Host
}

// provision gives an account its INBOX and the configured mailboxes and
// refreshes its quota from the authentication result, as an IMAP login
// does: an account that only ever uses JMAP needs Drafts and Sent too. It
// runs when a client fetches the session resource, which a client does
// when it starts, not on every API request. A failure is logged and the
// session served anyway.
func (s *Server) provision(sess *session) {
	provision.Apply(sess.user, sess.auth, sess.log)

	if err := provision.Mailboxes(sess.user, s.o.Autocreate); err != nil {
		sess.log.Error("jmap: provision account", "err", err)
	}
}
