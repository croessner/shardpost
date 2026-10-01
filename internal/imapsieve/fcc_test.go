package imapsieve

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"shardpost/internal/config"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// recordingSender keeps what it was asked to send.
type recordingSender struct {
	mu   sync.Mutex
	sent [][]byte
}

func (r *recordingSender) Send(_ context.Context, _ string, _ []string, raw []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sent = append(r.sent, raw)

	return nil
}

// TestNotifyFccInIMAPSieve: a notification sent from an IMAPSieve
// script is also stored in the owner's existing mailbox (RFC 8580); a
// missing mailbox is not created there, as for fileinto, and the run
// still succeeds.
func TestNotifyFccInIMAPSieve(t *testing.T) {
	const owner = "owner@example.org"

	for _, tc := range []struct {
		name, mailbox string
		stored        int
	}{
		{"existing", "Notes", 1},
		{"missing", "Missing", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := memstore.New()
			u, _ := s.User(owner)
			_ = u.CreateMailbox("INBOX", nil)
			_ = u.CreateMailbox("Notes", nil)

			m, err := u.Deliver("INBOX", []byte("Subject: example\r\n\r\nbody"), nil, time.Now())
			if err != nil {
				t.Fatal(err)
			}

			_ = u.PutScript("events", `require ["imapsieve", "enotify", "fcc", "mailbox", "imap4flags"];
notify :fcc "`+tc.mailbox+`" :create :flags "\\Seen" :message "new" "mailto:watch@example.org";`)
			name := "events"
			_ = u.SetMetadata("INBOX", "", map[string]*string{"/shared/imapsieve/script": &name})
			info, _ := u.Info("INBOX")

			sender := &recordingSender{}
			e := New(config.IMAPSieve{Enabled: true, UserScripts: true, TimeoutMS: 2000, MaxParallel: 1}, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
			e.Sender = sender

			e.Handle(context.Background(), Event{Operation: "APPEND", Actor: owner, Owner: owner, Mailbox: "INBOX", MailboxID: info.ID, Message: *m})

			if len(sender.sent) != 1 {
				t.Fatalf("%d notifications sent", len(sender.sent))
			}

			if tc.stored == 0 {
				if _, err := u.Lookup(tc.mailbox); err == nil {
					t.Fatal("the mailbox was created")
				}

				return
			}

			notes, _ := u.Info("Notes")

			copies, _ := u.ListMessages(notes.ID)
			if len(copies) != 1 || !model.HasFlag(copies[0].Flags, `\Seen`) {
				t.Fatalf("copies %+v", copies)
			}

			raw, _ := u.Read(&copies[0])
			if !strings.Contains(string(raw), "new") || !strings.Contains(string(raw), "Auto-Submitted: auto-notified") {
				t.Fatalf("stored %q", raw)
			}
		})
	}
}
