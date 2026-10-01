package jmap

import (
	"errors"
	"sync"
	"testing"

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
	fail := u.s.failRead
	u.s.mu.Unlock()

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

// TestEmailQueryHasAttachmentLegacy: a record without the field is
// answered by reading the message once; the answer is remembered in the
// locator, and a query that would read more than the bound answers
// serverUnavailable after remembering what it read.
func TestEmailQueryHasAttachmentLegacy(t *testing.T) {
	inner := memstore.New()
	store := &legacyStore{inner: inner}
	s := newTestServer(t, func(o *Options) {
		o.Store = store
		o.Limits.MaxAttachmentReads = 2
	})

	u, _ := inner.User("test")
	if err := u.CreateMailbox(inboxName, nil); err != nil {
		t.Fatal(err)
	}

	pdf := deliverAt(t, u, inboxName, rawWithPDF, 1)
	deliverAt(t, u, inboxName, rawInlineImage, 2)
	deliverAt(t, u, inboxName, rawPlain, 3)

	args := `{"accountId":"test","filter":{"hasAttachment":true}}`
	if got := mailError(t, s, "Email/query", args); got != errServerUnavailable {
		t.Fatalf("a query past the read bound answered %q, want serverUnavailable", got)
	}

	if store.readCount() != 2 {
		t.Errorf("the first query read %d messages, want the bound of 2", store.readCount())
	}

	sameIDs(t, "second attempt", queryIDs(t, s, args), pdf.GUID)

	if store.readCount() != 3 {
		t.Errorf("the queries read %d messages in all, want each one once", store.readCount())
	}

	sameIDs(t, "third attempt", queryIDs(t, s, args), pdf.GUID)

	if store.readCount() != 3 {
		t.Errorf("a remembered answer was read again (%d reads)", store.readCount())
	}
}
