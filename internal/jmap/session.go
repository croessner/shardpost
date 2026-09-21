package jmap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
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

	out := s.session(sess.account, s.baseURL(r))

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.o.Log.Warn("jmap: session response not written", "err", err)
	}
}

// session builds the session resource of an account.
func (s *Server) session(account, base string) *Session {
	id := accountID(account)

	out := &Session{
		Capabilities:    map[string]json.RawMessage{CapCore: mustRaw(s.core())},
		Accounts:        map[string]Account{id: s.account(account)},
		PrimaryAccounts: map[string]string{},
		Username:        account,
		APIURL:          base + PathAPI,
		DownloadURL:     base + PathDownload + "{accountId}/{blobId}/{name}?accept={type}",
		UploadURL:       base + PathUpload + "{accountId}/",
		EventSourceURL:  base + PathEvents + "?types={types}&closeafter={closeafter}&ping={ping}",
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
		CollationAlgorithms:   []string{"i;ascii-numeric", "i;ascii-casemap", "i;octet"},
	}
}

// account describes the account itself. Only the account of the
// authenticated user is listed; the accounts of other users (shared
// mailboxes, the public namespace) belong to JMAP Sharing, which is not
// built yet, and listing them empty would be a lie about what works.
func (s *Server) account(name string) Account {
	return Account{
		Name:                name,
		IsPersonal:          true,
		IsReadOnly:          false,
		AccountCapabilities: map[string]json.RawMessage{CapCore: json.RawMessage(`{}`)},
	}
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
		scheme = "https"
	}

	return scheme + "://" + r.Host
}
