package jmap

import (
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// The message with an attachment and the one without, as the query tests
// use them.
const (
	rawWithPDF = "From: a@example.org\r\nSubject: with file\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nsee the file\r\n" +
		"--b\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=a.pdf\r\n\r\n%PDF\r\n--b--\r\n"
	rawInlineImage = "From: a@example.org\r\nSubject: inline\r\nContent-Type: multipart/related; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/html\r\n\r\n<img src=cid:x>\r\n" +
		"--b\r\nContent-Type: image/png\r\nContent-Disposition: inline\r\nContent-Id: <x>\r\n\r\nPNG\r\n--b--\r\n"
	rawPlain = "From: a@example.org\r\nSubject: plain\r\n\r\njust text\r\n"
)

// legacyStore hands out views whose index records do not carry
// hasAttachment, as records written before the field existed do, and
// that count (or refuse) the messages read.
type legacyStore struct {
	inner    *memstore.Store
	mu       sync.Mutex
	reads    int
	failRead bool
	// gate, when set, holds every read until it is closed; entered gets
	// a value when a read starts waiting, if anyone listens.
	gate    chan struct{}
	entered chan struct{}
}

func (s *legacyStore) User(name string) (model.User, error) {
	u, err := s.inner.User(name)
	if err != nil {
		return nil, err
	}

	return &legacyUser{User: u, Index: u.(model.Index), Blobs: u.(model.Blobs), s: s}, nil
}

func (s *legacyStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.reads
}

type legacyUser struct {
	model.User
	model.Index
	model.Blobs
	s *legacyStore
}

func strip(metas []model.Meta) []model.Meta {
	for i := range metas {
		metas[i].HasAttachment = nil
	}

	return metas
}

func (u *legacyUser) ListMessages(id string) ([]model.Meta, error) {
	metas, err := u.User.ListMessages(id)

	return strip(metas), err
}

func (u *legacyUser) GetMessages(id string, uids []uint32) ([]model.Meta, error) {
	metas, err := u.User.GetMessages(id, uids)

	return strip(metas), err
}

func (u *legacyUser) Read(m *model.Meta) ([]byte, error) {
	u.s.mu.Lock()
	u.s.reads++
	fail, gate, entered := u.s.failRead, u.s.gate, u.s.entered
	u.s.mu.Unlock()

	if gate != nil {
		select {
		case entered <- struct{}{}:
		default:
		}

		<-gate
	}

	if fail {
		return nil, errors.New("no blob")
	}

	return u.User.Read(m)
}

// TestHasAttachmentStored: delivery records hasAttachment in the index
// record, with the answer Email/get gives.
func TestHasAttachmentStored(t *testing.T) {
	s, u := emailServer(t)

	for _, c := range []struct {
		raw  string
		want bool
	}{{rawWithPDF, true}, {rawInlineImage, false}, {rawPlain, false}} {
		m := deliver(t, u, inboxName, c.raw)
		if m.HasAttachment == nil || *m.HasAttachment != c.want {
			t.Errorf("stored hasAttachment of %q is %v, want %v", c.raw[:40], m.HasAttachment, c.want)
		}

		if got := getEmail(t, s, m.GUID, `["hasAttachment"]`)[propHasAttachment]; got != c.want {
			t.Errorf("Email/get hasAttachment is %v, want %v", got, c.want)
		}
	}
}

// TestHasAttachmentFromRecord: Email/get answers hasAttachment from the
// index record without reading the message.
func TestHasAttachmentFromRecord(t *testing.T) {
	inner := memstore.New()
	store := &legacyStore{inner: inner, failRead: true}
	s := newTestServer(t, func(o *Options) { o.Store = &recordStore{legacyStore: store} })

	u, _ := inner.User("test")
	if err := u.CreateMailbox(inboxName, nil); err != nil {
		t.Fatal(err)
	}

	m := deliver(t, u, inboxName, rawWithPDF)

	if got := getEmail(t, s, m.GUID, `["hasAttachment"]`)[propHasAttachment]; got != true {
		t.Errorf("hasAttachment is %v with the message unreadable, want true from the record", got)
	}

	if store.readCount() != 0 {
		t.Errorf("Email/get read %d messages for hasAttachment", store.readCount())
	}
}

// recordStore is a legacyStore whose records keep the field: only the
// reads are refused.
type recordStore struct{ *legacyStore }

func (s *recordStore) User(name string) (model.User, error) {
	u, err := s.inner.User(name)
	if err != nil {
		return nil, err
	}

	return &readCounter{legacyUser{User: u, Index: u.(model.Index), Blobs: u.(model.Blobs), s: s.legacyStore}}, nil
}

// readCounter keeps the records as they are and counts the reads.
type readCounter struct{ legacyUser }

func (u *readCounter) ListMessages(id string) ([]model.Meta, error) { return u.User.ListMessages(id) }

func (u *readCounter) GetMessages(id string, uids []uint32) ([]model.Meta, error) {
	return u.User.GetMessages(id, uids)
}

// TestEmailQueryHasAttachment: the condition, alone and with others.
func TestEmailQueryHasAttachment(t *testing.T) {
	s, u := emailServer(t)

	pdf := deliverAt(t, u, inboxName, rawWithPDF, 1)
	img := deliverAt(t, u, inboxName, rawInlineImage, 2)
	plain := deliverAt(t, u, "Archive", rawPlain, 3)
	inbox := boxIDs(t, u)[inboxName]

	sameIDs(t, "hasAttachment true", queryIDs(t, s, `{"accountId":"test","filter":{"hasAttachment":true}}`), pdf.GUID)
	sameIDs(t, "hasAttachment false", queryIDs(t, s, `{"accountId":"test","filter":{"hasAttachment":false}}`),
		plain.GUID, img.GUID)
	sameIDs(t, "in a mailbox", queryIDs(t, s, `{"accountId":"test","filter":{"inMailbox":"`+inbox+`","hasAttachment":false}}`),
		img.GUID)
	sameIDs(t, "NOT", queryIDs(t, s,
		`{"accountId":"test","filter":{"operator":"NOT","conditions":[{"hasAttachment":true}]}}`), plain.GUID, img.GUID)
	sameIDs(t, "with text", queryIDs(t, s, `{"accountId":"test","filter":{"text":"file","hasAttachment":true}}`), pdf.GUID)
}

// legacyServer is a server over a store whose records lack
// hasAttachment, with three messages in INBOX: one with a file, one with
// an inline image and a plain one.
func legacyServer(t *testing.T, bound int) (*Server, *legacyStore, *model.Meta) {
	t.Helper()

	inner := memstore.New()
	store := &legacyStore{inner: inner}
	s := newTestServer(t, func(o *Options) {
		o.Store = store
		o.Limits.MaxAttachmentReads = bound
	})

	t.Cleanup(s.Close)

	u, _ := inner.User("test")
	if err := u.CreateMailbox(inboxName, nil); err != nil {
		t.Fatal(err)
	}

	pdf := deliverAt(t, u, inboxName, rawWithPDF, 1)
	deliverAt(t, u, inboxName, rawInlineImage, 2)
	deliverAt(t, u, inboxName, rawPlain, 3)

	return s, store, pdf
}

// TestEmailQueryHasAttachmentLegacy: a record without the field is
// answered by reading the message once; the answer is remembered in the
// locator. A query that would read more than the bound answers
// serverUnavailable after remembering what it read and starts a fill of
// the namespace in the background, after which the query succeeds.
func TestEmailQueryHasAttachmentLegacy(t *testing.T) {
	s, store, pdf := legacyServer(t, 2)

	args := `{"accountId":"test","filter":{"hasAttachment":true}}`
	if got := mailError(t, s, "Email/query", args); got != errServerUnavailable {
		t.Fatalf("a query past the read bound answered %q, want serverUnavailable", got)
	}

	s.fillWG.Wait()

	if store.readCount() != 3 {
		t.Errorf("the query and the fill read %d messages, want each one once", store.readCount())
	}

	sameIDs(t, "after the fill", queryIDs(t, s, args), pdf.GUID)
	sameIDs(t, "false", queryIDs(t, s, `{"accountId":"test","filter":{"hasAttachment":false}}`), legacyOthers(t, s)...)

	if store.readCount() != 3 {
		t.Errorf("a remembered answer was read again (%d reads)", store.readCount())
	}
}

// legacyOthers are the ids of the messages without a file, newest first.
func legacyOthers(t *testing.T, s *Server) []string {
	t.Helper()

	all := queryIDs(t, s, `{"accountId":"test"}`)

	return all[:2]
}

// TestEmailQueryHasAttachmentLazy: a message the other conditions decide
// is neither looked up nor read.
func TestEmailQueryHasAttachmentLazy(t *testing.T) {
	s, store, pdf := legacyServer(t, 1000)

	sameIDs(t, "with text", queryIDs(t, s, `{"accountId":"test","filter":{"subject":"file","hasAttachment":true}}`), pdf.GUID)

	if store.readCount() != 1 {
		t.Errorf("the query read %d messages, want only the one the text matched", store.readCount())
	}

	sameIDs(t, "OR decided by text", queryIDs(t, s,
		`{"accountId":"test","filter":{"operator":"OR","conditions":[{"subject":"file"},{"hasAttachment":true}]}}`), pdf.GUID)

	if store.readCount() != 3 {
		t.Errorf("the OR read %d messages in all, want the two it could not decide", store.readCount())
	}
}

// TestEmailQueryHasAttachmentUnreadable: a message that cannot be read
// counts as one without an attachment and is not read again by every
// query.
func TestEmailQueryHasAttachmentUnreadable(t *testing.T) {
	s, store, _ := legacyServer(t, 1000)
	store.mu.Lock()
	store.failRead = true
	store.mu.Unlock()

	args := `{"accountId":"test","filter":{"hasAttachment":true}}`
	sameIDs(t, "unreadable", queryIDs(t, s, args))
	sameIDs(t, "again", queryIDs(t, s, args))

	if store.readCount() != 3 {
		t.Errorf("two queries read %d times, want each message once", store.readCount())
	}
}

// TestEmailGetHasAttachmentRemembered: Email/get answers hasAttachment of
// an old record from the locator, without the message.
func TestEmailGetHasAttachmentRemembered(t *testing.T) {
	s, store, pdf := legacyServer(t, 1000)

	queryIDs(t, s, `{"accountId":"test","filter":{"hasAttachment":true}}`)

	store.mu.Lock()
	store.failRead = true
	store.mu.Unlock()

	if got := getEmail(t, s, pdf.GUID, `["hasAttachment"]`)[propHasAttachment]; got != true {
		t.Errorf("hasAttachment is %v with the message unreadable, want true from the locator", got)
	}
}

// TestEmailGetRemembersParsed: Email/get remembers what it parsed of an
// older record, and the next call answers without the message.
func TestEmailGetRemembersParsed(t *testing.T) {
	s, store, pdf := legacyServer(t, 1000)

	if got := getEmail(t, s, pdf.GUID, `["hasAttachment"]`)[propHasAttachment]; got != true {
		t.Fatalf("hasAttachment is %v", got)
	}

	store.mu.Lock()
	store.failRead = true
	store.mu.Unlock()

	if got := getEmail(t, s, pdf.GUID, `["hasAttachment"]`)[propHasAttachment]; got != true {
		t.Errorf("the second Email/get gave %v with the message unreadable", got)
	}

	if store.readCount() != 1 {
		t.Errorf("Email/get read the message %d times, want once", store.readCount())
	}
}

// gatedServer is a legacy server whose reads wait at a gate.
func gatedServer(t *testing.T, slots int) (*Server, *legacyStore) {
	t.Helper()

	inner := memstore.New()
	store := &legacyStore{inner: inner, gate: make(chan struct{}), entered: make(chan struct{}, 1)}
	s := newTestServer(t, func(o *Options) {
		o.Store = store
		o.Limits.AttachmentBackfills = slots
	})

	for _, name := range []string{"test", "other"} {
		u, _ := inner.User(name)
		if err := u.CreateMailbox(inboxName, nil); err != nil {
			t.Fatal(err)
		}

		for h := 1; h <= 3; h++ {
			deliverAt(t, u, inboxName, rawPlain, h)
		}
	}

	return s, store
}

// fillRunning reports whether a fill of the namespace is running.
func fillRunning(s *Server, namespace string) bool {
	_, ok := s.fills.Load(namespace)

	return ok
}

// TestBackfillSingleFlightAndSlots: a second request for a namespace
// being filled starts nothing, a namespace that finds no free slot is not
// filled, and Close stops a running fill and waits for it.
func TestBackfillSingleFlightAndSlots(t *testing.T) {
	s, store := gatedServer(t, 1)

	s.backfill("test")
	<-store.entered

	s.backfill("test")  // the same namespace: already running
	s.backfill("other") // no free slot

	if !fillRunning(s, "test") || fillRunning(s, "other") {
		t.Fatal("the fills running are not the one that was started")
	}

	closed := make(chan struct{})

	go func() {
		s.Close()
		close(closed)
	}()

	for s.fillCtx.Err() == nil {
		time.Sleep(time.Millisecond)
	}

	select {
	case <-closed:
		t.Fatal("Close returned while a fill was still reading")
	default:
	}

	close(store.gate)
	<-closed

	if fillRunning(s, "test") || store.readCount() != 1 {
		t.Errorf("after Close: running %v, %d reads (want the fill stopped after one)", fillRunning(s, "test"), store.readCount())
	}

	s.backfill("other") // after Close nothing starts

	if fillRunning(s, "other") {
		t.Error("a fill started after Close")
	}
}

// TestUnreadableSet: a failure is remembered for the TTL, logged once at
// warning level, and the set starts over when it is full.
func TestUnreadableSet(t *testing.T) {
	var buf strings.Builder

	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	u := &unreadableSet{now: func() time.Time { return now }}

	u.note(log, "g1", errors.New("boom"))
	u.note(log, "g1", errors.New("boom"))

	if strings.Count(buf.String(), "level=WARN") != 1 || strings.Count(buf.String(), "level=DEBUG") != 1 {
		t.Errorf("logged:\n%s", buf.String())
	}

	if !u.recent("g1") || u.recent("g2") {
		t.Error("recent does not name the failed message alone")
	}

	now = now.Add(unreadableTTL)

	if u.recent("g1") {
		t.Error("a failure is remembered past the TTL")
	}

	for i := 0; i < maxUnreadable; i++ {
		u.note(slog.New(slog.DiscardHandler), strconv.Itoa(i), errors.New("x"))
	}

	if len(u.seen) != 1 {
		t.Errorf("a full set kept %d entries, want a fresh start", len(u.seen))
	}
}
