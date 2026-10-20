package jmap

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"shardpost/internal/model"
)

// uploadResponse is what an upload answers (RFC 8620 6.1).
type uploadResponse struct {
	AccountID string `json:"accountId"`
	BlobID    string `json:"blobId"`
	Type      string `json:"type"`
	Size      int64  `json:"size"`
}

// defaultBlobType is what an upload without a usable Content-Type is
// recorded as; RFC 8620 6.1 names it explicitly.
const defaultBlobType = "application/octet-stream"

// handleUpload stores the request body as a blob of the account and
// returns the id it can be referenced by. Nothing points at the blob
// yet: until something does, the store's garbage collection removes it,
// which is the expiry RFC 8620 6.1 allows a server.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	sess, blobs, ok := s.blobsOf(w, r, false)
	if !ok {
		return
	}

	release, ok := s.slot(w, r, s.inUpload, sess.account, true, "too many uploads in flight for this account", "maxConcurrentUpload")
	if !ok {
		return
	}
	defer release()

	raw, err := io.ReadAll(io.LimitReader(r.Body, s.o.Limits.MaxSizeUpload+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, errURNNotRequest, "the upload could not be read", "")

		return
	}

	if int64(len(raw)) > s.o.Limits.MaxSizeUpload {
		writeProblem(w, http.StatusRequestEntityTooLarge, errURNLimit,
			"the upload is larger than maxSizeUpload", "maxSizeUpload")

		return
	}

	if !withinQuota(sess.user, int64(len(raw))) {
		writeProblem(w, http.StatusRequestEntityTooLarge, errURNLimit,
			"the account is out of quota", "maxSizeUpload")

		return
	}

	guid, err := blobs.PutBlob(raw)
	if err != nil {
		s.o.Log.Error("jmap: upload", "account", sess.account, "err", err)
		writeProblem(w, http.StatusServiceUnavailable, errURNNotRequest, "the upload could not be stored", "")

		return
	}

	out := uploadResponse{
		AccountID: sess.accountID(),
		BlobID:    guid,
		Type:      blobType(r.Header.Get(fieldContentType)),
		Size:      int64(len(raw)),
	}

	w.Header().Set(fieldContentType, "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)

	if err := json.NewEncoder(w).Encode(&out); err != nil {
		s.o.Log.Warn("jmap: upload response not written", "err", err)
	}
}

// handleDownload serves a blob of the account. The media type is the one
// the client asks for: the server stores bytes, not types, and a client
// that downloads an attachment knows from the message what it is.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	sess, blobs, ok := s.blobsOf(w, r, true)
	if !ok {
		return
	}

	// A download can read and parse a whole message, so it is bounded per
	// account, on a limit of its own (jmap.max_concurrent_downloads): a
	// client streaming an attachment must not lock itself out of the API.
	// The session advertises no such limit, so the error names the one
	// that comes closest.
	release, ok := s.slot(w, r, s.inDown, sess.account, true, "too many downloads in flight for this account", "")
	if !ok {
		return
	}
	defer release()

	blobID := r.PathValue("blobId")

	if err := mayReadBlob(sess, blobID); err != nil {
		s.blobFailed(w, r, blobID, err)

		return
	}

	if guid, path, ok := parsePartBlob(blobID); ok {
		s.servePart(w, r, blobs, guid, path)

		return
	}

	if isScriptBlob(blobID) {
		s.serveScript(w, r, sess, blobID)

		return
	}

	size, err := blobs.BlobSize(blobID)
	if err != nil {
		s.blobFailed(w, r, blobID, err)

		return
	}

	downloadHeaders(w, r)
	http.ServeContent(w, r, "", time.Time{}, &blobReader{blobs: blobs, guid: blobID, size: size})
}

// downloadHeaders are the headers every download carries.
func downloadHeaders(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(fieldContentType, blobType(r.URL.Query().Get("accept")))
	w.Header().Set(fieldDisposition, "attachment; filename*=UTF-8''"+urlEscape(r.PathValue(partName)))
	// The media type is whatever the client asked for, so a blob must
	// never be interpreted as a document of this origin: no sniffing,
	// and a sandbox for the types a browser would render anyway.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	// A blob is written once and never changes, so a client that has it
	// once never has to ask again.
	w.Header().Set("Cache-Control", "private, immutable, max-age=31536000")
}

// servePart serves one body part of a message: its bytes with the
// transfer encoding undone and the charset left as it is, which is what
// RFC 8621 4.1.4 says a part's blob is. The message is parsed to find it;
// a part is not stored on its own.
func (s *Server) servePart(w http.ResponseWriter, r *http.Request, blobs model.Blobs, guid, path string) {
	body, found, err := readPart(blobs, guid, path, 0)
	if err != nil {
		s.blobFailed(w, r, guid, err)

		return
	}

	if !found {
		writeProblem(w, http.StatusNotFound, errURNNotRequest, "no such body part", "")

		return
	}

	downloadHeaders(w, r)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}

// serveScript serves the content of a Sieve script by its blob id, which
// is what ManageSieve's GETSCRIPT returns (RFC 9661 2.2).
func (s *Server) serveScript(w http.ResponseWriter, r *http.Request, sess *session, blobID string) {
	text, err := scriptContent(sess, blobID, 0)
	if err != nil {
		s.blobFailed(w, r, blobID, err)

		return
	}

	downloadHeaders(w, r)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(text))
}

// withinQuota reports whether the account has room for another upload.
// An uploaded blob that nothing references is not counted anywhere until
// something does, so this is not an exact reservation; it is the bound
// that keeps an account which is already full from filling the pool with
// blobs it can never reference. A store without a quota says yes. The
// upload endpoint and Blob/upload ask the same question.
func withinQuota(u model.User, size int64) bool {
	limit, err := u.QuotaLimit()
	if err != nil || limit.Bytes <= 0 {
		return err == nil
	}

	usage, err := u.QuotaUsage()
	if err != nil {
		return false
	}

	return usage.Bytes+size <= limit.Bytes
}

// blobFailed answers a blob that is not there or could not be read.
func (s *Server) blobFailed(w http.ResponseWriter, r *http.Request, blobID string, err error) {
	if errors.Is(err, model.ErrNoBlob) {
		writeProblem(w, http.StatusNotFound, errURNNotRequest, "no blob "+blobID, "")

		return
	}

	s.o.Log.Error("jmap: download", "blob", blobID, "remote", r.RemoteAddr, "err", err)
	writeProblem(w, http.StatusServiceUnavailable, errURNNotRequest, "the blob could not be read", "")
}

// blobsOf returns the session of the account in the path and its blob
// store. Uploads go to the personal account only: a blob uploaded into a
// shared account could not be told apart from the owner's blobs, which
// the user may not read. A download may name a shared account (shared is
// true), and then serves only what blobReadable allows. An account the
// user has no access to is not found, as one that does not exist.
func (s *Server) blobsOf(w http.ResponseWriter, r *http.Request, shared bool) (*session, model.Blobs, bool) {
	sess := sessionOf(r)
	id := r.PathValue("accountId")

	if id != accountID(sess.account) {
		var a *account
		if shared {
			a, _ = s.resolveAccount(sess, id)
		}

		if a == nil {
			writeProblem(w, http.StatusNotFound, errURNNotRequest, "no account "+id, "")

			return nil, nil, false
		}

		sess = sess.with(a)
	}

	blobs, ok := sess.user.(model.Blobs)
	if !ok {
		writeProblem(w, http.StatusNotImplemented, errURNNotRequest, "this store does not serve blobs", "")

		return nil, nil, false
	}

	return sess, blobs, true
}

// blobType is the media type of a blob: what the client asked for, or
// the neutral default. A type the client made up is its own problem, but
// it must not become a header this server injects into a browser, so
// anything that is not a type/subtype with harmless parameters is
// refused.
func blobType(want string) string {
	if !validMediaType(want) {
		return defaultBlobType
	}

	return want
}

// mediaTypeChars are the characters a media type may consist of here:
// the RFC 9110 token set this server needs, plus the separators of a
// parameter. Anything else — a quote, a control character, a newline —
// makes the value unusable as a header and is refused.
var mediaTypeChars = func() [256]bool {
	var ok [256]bool
	for _, c := range []byte(`abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/;= .-+_"*'()`) {
		ok[c] = true
	}

	return ok
}()

// validMediaType accepts "type/subtype" with optional parameters and
// nothing else.
func validMediaType(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}

	slash := 0

	for i := 0; i < len(s); i++ {
		if !mediaTypeChars[s[i]] {
			return false
		}

		if s[i] == '/' {
			slash++
		}
	}

	return slash == 1
}

// urlEscape percent-encodes a file name for the Content-Disposition
// header (RFC 6266 / RFC 5987).
func urlEscape(name string) string {
	const hex = "0123456789ABCDEF"

	out := make([]byte, 0, len(name))

	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' {
			out = append(out, c)
			continue
		}

		out = append(out, '%', hex[c>>4], hex[c&0x0f])
	}

	return string(out)
}

// blobReader reads a stored blob as a seekable stream, so that the HTTP
// layer can answer a Range request without the whole blob being held in
// memory. RFC 9404 asks for byte ranges of blobs; this is where they
// come from.
type blobReader struct {
	blobs model.Blobs
	guid  string
	size  int64
	pos   int64
}

// Read implements io.Reader.
func (b *blobReader) Read(p []byte) (int, error) {
	if b.pos >= b.size {
		return 0, io.EOF
	}

	want := min(int64(len(p)), b.size-b.pos)

	raw, err := b.blobs.ReadBlob(b.guid, b.size, b.pos, want)
	if err != nil {
		return 0, err
	}

	if len(raw) == 0 {
		return 0, io.EOF
	}

	b.pos += int64(len(raw))

	return copy(p, raw), nil
}

// Seek implements io.Seeker.
func (b *blobReader) Seek(offset int64, whence int) (int64, error) {
	var pos int64

	switch whence {
	case io.SeekStart:
		pos = offset
	case io.SeekCurrent:
		pos = b.pos + offset
	case io.SeekEnd:
		pos = b.size + offset
	default:
		return 0, errors.New("blob: unknown seek origin")
	}

	if pos < 0 {
		return 0, errors.New("blob: seek before the start")
	}

	b.pos = pos

	return pos, nil
}
