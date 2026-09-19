// Package archive is the account export format of ShardPost (roadmap 8e):
// a tar stream with a header, per mailbox its description and per message
// a JSON description followed by the message (RFC 5322 as stored), the
// Sieve scripts and, last, a manifest with the SHA-256 of every entry.
//
//	shardpost-export.json            Header
//	mailboxes/<id>/mailbox.json      Mailbox (identity, state, ACL, metadata)
//	mailboxes/<id>/<uid>.json        Message (identity, flags, MODSEQ, dates, POP3 UIDL, size, sha256)
//	mailboxes/<id>/<uid>.eml         the message
//	sieve/<name>.sieve               a Sieve script
//	manifest.json                    Manifest (complete, counts, every file with size and sha256)
//
// The reader verifies each message against its description and, at the
// end, every entry against the manifest; an archive without a complete
// manifest is refused. The full-text index is not part of the archive
// (it is rebuilt from the messages).
package archive

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Format and Version identify the archive format.
const (
	Format  = "shardpost-account-export"
	Version = 1
)

// Entry names.
const (
	headerName   = "shardpost-export.json"
	manifestName = "manifest.json"
	mailboxDir   = "mailboxes/"
	sieveDir     = "sieve/"
	mailboxFile  = "mailbox.json"
)

// MaxMessage bounds one message (and any other entry) when reading.
const MaxMessage = 256 << 20

// Header opens an archive.
type Header struct {
	Format        string    `json:"format"`
	Version       int       `json:"version"`
	Account       string    `json:"account"`
	Created       time.Time `json:"created"`
	ServerVersion string    `json:"server_version"`
}

// Mailbox describes one mailbox at the time its export began.
type Mailbox struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	UIDValidity   uint32            `json:"uidvalidity"`
	UIDNext       uint32            `json:"uid_next"`
	HighestModSeq uint64            `json:"highest_modseq"`
	Subscribed    bool              `json:"subscribed"`
	SpecialUse    []string          `json:"special_use,omitempty"`
	ACL           map[string]string `json:"acl,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// Message describes one message; Size and SHA256 are those of the .eml.
type Message struct {
	UID          uint32    `json:"uid"`
	GUID         string    `json:"guid"`
	Size         int64     `json:"size"`
	SHA256       string    `json:"sha256"`
	Flags        []string  `json:"flags"`
	ModSeq       uint64    `json:"modseq"`
	InternalDate time.Time `json:"internal_date"`
	SaveDate     time.Time `json:"save_date,omitzero"`
	POP3UIDL     string    `json:"pop3_uidl,omitempty"`
}

// File is one entry of the manifest.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest closes an archive.
type Manifest struct {
	// Complete is false when the export stopped early (Error says why).
	Complete  bool   `json:"complete"`
	Error     string `json:"error,omitempty"`
	Mailboxes int    `json:"mailboxes"`
	Messages  int    `json:"messages"`
	Bytes     int64  `json:"bytes"`
	// Skipped are messages that vanished during the export ("<id>/<uid>"),
	// SkippedMailboxes mailboxes deleted meanwhile (by name).
	Skipped          []string `json:"skipped,omitempty"`
	SkippedMailboxes []string `json:"skipped_mailboxes,omitempty"`
	ActiveSieve      string   `json:"active_sieve,omitempty"`
	Files            []File   `json:"files"`
}

// Errors of the reader.
var (
	ErrFormat     = errors.New("not a ShardPost account archive")
	ErrChecksum   = errors.New("checksum mismatch")
	ErrIncomplete = errors.New("the archive is incomplete")
)

var (
	idPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	sievePatten = regexp.MustCompile(`^[^/\\\x00-\x1f]{1,128}$`)
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Writer writes an archive.
type Writer struct {
	tw    *tar.Writer
	now   time.Time
	files []File
	man   Manifest
}

// NewWriter starts an archive with its header.
func NewWriter(w io.Writer, h Header) (*Writer, error) {
	h.Format, h.Version = Format, Version

	aw := &Writer{tw: tar.NewWriter(w), now: h.Created}
	if aw.now.IsZero() {
		aw.now = time.Now()
	}

	return aw, aw.writeJSON(headerName, h)
}

func (w *Writer) writeJSON(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}

	return w.write(name, b)
}

func (w *Writer) write(name string, b []byte) error {
	hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: w.now, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return err
	}

	if _, err := w.tw.Write(b); err != nil {
		return err
	}

	w.files = append(w.files, File{Path: name, Size: int64(len(b)), SHA256: sum(b)})

	return nil
}

// Mailbox starts a mailbox; its messages follow.
func (w *Writer) Mailbox(m Mailbox) error {
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("archive: unusable mailbox id %q", m.ID)
	}

	w.man.Mailboxes++

	return w.writeJSON(mailboxDir+m.ID+"/"+mailboxFile, m)
}

// Message adds a message of the mailbox; size and checksum are set.
func (w *Writer) Message(mailboxID string, m Message, raw []byte) error {
	m.Size, m.SHA256 = int64(len(raw)), sum(raw)
	base := mailboxDir + mailboxID + "/" + strconv.FormatUint(uint64(m.UID), 10)

	if err := w.writeJSON(base+".json", m); err != nil {
		return err
	}

	w.man.Messages++
	w.man.Bytes += m.Size

	return w.write(base+".eml", raw)
}

// Skip records a message that vanished during the export.
func (w *Writer) Skip(mailboxID string, uid uint32) {
	w.man.Skipped = append(w.man.Skipped, mailboxID+"/"+strconv.FormatUint(uint64(uid), 10))
}

// SkipMailbox records a mailbox deleted during the export.
func (w *Writer) SkipMailbox(name string) {
	w.man.SkippedMailboxes = append(w.man.SkippedMailboxes, name)
}

// Sieve adds a script.
func (w *Writer) Sieve(name, text string) error {
	if !sievePatten.MatchString(name) {
		return fmt.Errorf("archive: unusable script name %q", name)
	}

	return w.write(sieveDir+name+".sieve", []byte(text))
}

// Close writes the manifest (complete when failure is nil) and ends the
// tar stream.
func (w *Writer) Close(activeSieve string, failure error) error {
	w.man.Complete, w.man.ActiveSieve, w.man.Files = failure == nil, activeSieve, w.files
	if failure != nil {
		w.man.Error = failure.Error()
	}

	b, err := json.MarshalIndent(w.man, "", "  ")
	if err != nil {
		return err
	}

	hdr := &tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(b)), ModTime: w.now, Typeflag: tar.TypeReg, Format: tar.FormatPAX}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return err
	}

	if _, err := w.tw.Write(b); err != nil {
		return err
	}

	return w.tw.Close()
}

// Kind is the kind of an entry read.
type Kind int

// Entry kinds.
const (
	KindMailbox Kind = iota + 1
	KindMessage
	KindSieve
	KindManifest
)

// Entry is one item read from an archive.
type Entry struct {
	Kind      Kind
	MailboxID string   // KindMailbox, KindMessage
	Mailbox   *Mailbox // KindMailbox
	Message   *Message // KindMessage
	Raw       []byte   // KindMessage: the message; KindSieve: the script
	Name      string   // KindSieve
	Manifest  *Manifest
}

// Reader reads and verifies an archive.
type Reader struct {
	tr      *tar.Reader
	Header  Header
	seen    map[string]File
	pending *Message // description read, message not yet
	pendID  string
	done    bool
}

// NewReader reads the header.
func NewReader(r io.Reader) (*Reader, error) {
	ar := &Reader{tr: tar.NewReader(r), seen: map[string]File{}}

	name, b, err := ar.entry()
	if err != nil || name != headerName {
		return nil, fmt.Errorf("%w: %v", ErrFormat, cmpErr(err, "first entry "+name))
	}

	if err := json.Unmarshal(b, &ar.Header); err != nil || ar.Header.Format != Format {
		return nil, ErrFormat
	}

	if ar.Header.Version != Version {
		return nil, fmt.Errorf("%w: version %d (this reader knows %d)", ErrFormat, ar.Header.Version, Version)
	}

	return ar, nil
}

func cmpErr(err error, msg string) any {
	if err != nil {
		return err
	}

	return msg
}

// entry reads the next tar entry whole and records its checksum.
func (r *Reader) entry() (string, []byte, error) {
	hdr, err := r.tr.Next()
	if err != nil {
		return "", nil, err
	}

	if hdr.Typeflag != tar.TypeReg || hdr.Size < 0 || hdr.Size > MaxMessage {
		return "", nil, fmt.Errorf("%w: entry %q", ErrFormat, hdr.Name)
	}

	b, err := io.ReadAll(io.LimitReader(r.tr, hdr.Size))
	if err != nil {
		return "", nil, err
	}

	if _, dup := r.seen[hdr.Name]; dup || hdr.Name == headerName && len(r.seen) > 0 {
		return "", nil, fmt.Errorf("%w: entry %q twice", ErrFormat, hdr.Name)
	}

	if hdr.Name != manifestName {
		r.seen[hdr.Name] = File{Path: hdr.Name, Size: int64(len(b)), SHA256: sum(b)}
	}

	return hdr.Name, b, nil
}

// Next returns the next item; io.EOF after the verified manifest.
func (r *Reader) Next() (*Entry, error) {
	if r.done {
		return nil, io.EOF
	}

	name, b, err := r.entry()
	if errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: no manifest", ErrIncomplete)
	}

	if err != nil {
		return nil, err
	}

	switch {
	case name == manifestName:
		return r.manifest(b)
	case strings.HasPrefix(name, sieveDir) && strings.HasSuffix(name, ".sieve"):
		script := strings.TrimSuffix(strings.TrimPrefix(name, sieveDir), ".sieve")
		if !sievePatten.MatchString(script) || script == "." || script == ".." {
			return nil, fmt.Errorf("%w: script name %q", ErrFormat, script)
		}

		return &Entry{Kind: KindSieve, Name: script, Raw: b}, nil
	case strings.HasPrefix(name, mailboxDir):
		return r.mailboxEntry(name, b)
	default:
		return nil, fmt.Errorf("%w: unexpected entry %q", ErrFormat, name)
	}
}

// mailboxEntry is a mailbox description or a message description (the
// message follows as the next entry).
func (r *Reader) mailboxEntry(name string, b []byte) (*Entry, error) {
	id, file, ok := strings.Cut(strings.TrimPrefix(name, mailboxDir), "/")
	if !ok || !idPattern.MatchString(id) {
		return nil, fmt.Errorf("%w: entry %q", ErrFormat, name)
	}

	if file == mailboxFile {
		var m Mailbox
		if err := json.Unmarshal(b, &m); err != nil || m.ID != id {
			return nil, fmt.Errorf("%w: %s", ErrFormat, name)
		}

		return &Entry{Kind: KindMailbox, MailboxID: id, Mailbox: &m}, nil
	}

	var m Message
	if err := json.Unmarshal(b, &m); err != nil || file != strconv.FormatUint(uint64(m.UID), 10)+".json" {
		return nil, fmt.Errorf("%w: %s", ErrFormat, name)
	}

	r.pending, r.pendID = &m, id

	next, raw, err := r.entry()
	if err != nil {
		return nil, fmt.Errorf("%w: message of %s: %v", ErrIncomplete, name, err)
	}

	return r.message(next, raw)
}

// message checks the message after its description.
func (r *Reader) message(name string, raw []byte) (*Entry, error) {
	m, id := r.pending, r.pendID
	r.pending = nil

	want := mailboxDir + id + "/" + strconv.FormatUint(uint64(m.UID), 10) + ".eml"

	switch {
	case name != want:
		return nil, fmt.Errorf("%w: expected %s, got %s", ErrFormat, want, name)
	case int64(len(raw)) != m.Size || sum(raw) != m.SHA256:
		return nil, fmt.Errorf("%w: %s", ErrChecksum, name)
	}

	return &Entry{Kind: KindMessage, MailboxID: id, Message: m, Raw: raw}, nil
}

// manifest checks every entry read against the manifest.
func (r *Reader) manifest(b []byte) (*Entry, error) {
	var man Manifest
	if err := json.Unmarshal(b, &man); err != nil {
		return nil, fmt.Errorf("%w: manifest: %v", ErrFormat, err)
	}

	if !man.Complete {
		return nil, fmt.Errorf("%w: %s", ErrIncomplete, cmpOr(man.Error, "the export did not finish"))
	}

	if len(man.Files) != len(r.seen) {
		return nil, fmt.Errorf("%w: %d entries, the manifest lists %d", ErrChecksum, len(r.seen), len(man.Files))
	}

	for _, f := range man.Files {
		if r.seen[f.Path] != f {
			return nil, fmt.Errorf("%w: %s", ErrChecksum, f.Path)
		}
	}

	r.done = true

	return &Entry{Kind: KindManifest, Manifest: &man}, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}

	return b
}
