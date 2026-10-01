package memstore

import (
	"testing"
	"time"

	"shardpost/internal/model"
)

const rawAttached = "Subject: s\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
	"--b\r\nContent-Type: text/plain\r\n\r\ntext\r\n" +
	"--b\r\nContent-Type: application/pdf\r\n\r\n%PDF\r\n--b--\r\n"

// TestHasAttachmentRecorded: delivery and import both record
// hasAttachment in the index record.
func TestHasAttachmentRecorded(t *testing.T) {
	s := New()
	u, _ := s.User("a")

	if err := u.CreateMailbox("INBOX", nil); err != nil {
		t.Fatal(err)
	}

	m, err := u.Deliver("INBOX", []byte(rawAttached), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	if m.HasAttachment == nil || !*m.HasAttachment {
		t.Errorf("delivery recorded hasAttachment %v", m.HasAttachment)
	}

	imp := u.(interface {
		ImportMailbox(model.ImportMailbox) (*model.Mailbox, error)
		ImportMessages(string, []model.ImportMessage) (*model.ImportResult, error)
	})

	mb, err := imp.ImportMailbox(model.ImportMailbox{Name: "Old", UIDValidity: 7})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := imp.ImportMessages(mb.ID, []model.ImportMessage{{UID: 1, Raw: []byte("Subject: x\r\n\r\nplain\r\n")}}); err != nil {
		t.Fatal(err)
	}

	metas, err := u.ListMessages(mb.ID)
	if err != nil || len(metas) != 1 {
		t.Fatalf("imported %v (%v)", metas, err)
	}

	if a := metas[0].HasAttachment; a == nil || *a {
		t.Errorf("import recorded hasAttachment %v, want false", a)
	}
}
