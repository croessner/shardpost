package imapsieve

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"shardpost/internal/config"
	"shardpost/internal/extprograms"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, script string) (*Engine, model.User, Event) {
	t.Helper()

	s := memstore.New()
	u, _ := s.User("test")
	_ = u.CreateMailbox("INBOX", nil)
	_ = u.CreateMailbox("Archive", nil)

	m, err := u.Deliver("INBOX", []byte("Subject: example\r\n\r\nbody"), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	_ = u.PutScript("events", script)
	name := "events"
	_ = u.SetMetadata("INBOX", "", map[string]*string{"/shared/imapsieve/script": &name})
	info, _ := u.Info("INBOX")
	e := New(config.IMAPSieve{Enabled: true, UserScripts: true, TimeoutMS: 500, MaxParallel: 1}, s, slog.New(slog.NewTextHandler(io.Discard, nil)))

	return e, u, Event{Operation: "APPEND", Actor: "test", Owner: "test", Mailbox: "INBOX", MailboxID: info.ID, Message: *m}
}
func TestIMAPActions(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		deleted      bool
		copies       int
		flag         string
	}{
		{"keep", "keep;", false, 0, ""},
		{"flags", `require ["imapsieve","imap4flags"]; addflag "\\Flagged";`, false, 0, `\Flagged`},
		{"discard", "discard;", true, 0, ""},
		{"fileinto", `require "fileinto"; fileinto "Archive";`, true, 1, ""},
		{"fileinto-copy", `require ["fileinto","copy"]; fileinto :copy "Archive";`, false, 1, ""},
		{"runtime-error", `require "fileinto"; fileinto "Missing";`, false, 0, ""},
		{"blocked-redirect", `redirect "other@example.org";`, false, 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, u, ev := fixture(t, tc.script)
			e.Handle(context.Background(), ev)

			m, _ := u.GetMessages(ev.MailboxID, []uint32{ev.Message.UID})
			if len(m) != 1 || model.HasFlag(m[0].Flags, `\Deleted`) != tc.deleted {
				t.Fatal("wrong original state")
			}

			if tc.flag != "" && !model.HasFlag(m[0].Flags, tc.flag) {
				t.Fatal("missing flag")
			}

			a, _ := u.Info("Archive")

			copies, _ := u.ListMessages(a.ID)
			if len(copies) != tc.copies {
				t.Fatalf("copies=%d", len(copies))
			}
		})
	}
}
func TestMetadataEmptySuppressesFallback(t *testing.T) {
	e, u, ev := fixture(t, `discard;`)
	server, _ := e.Store.User("~server")
	name := "events"
	_ = server.SetMetadata("", "", map[string]*string{"/shared/imapsieve/script": &name})
	empty := ""
	_ = u.SetMetadata("INBOX", "", map[string]*string{"/shared/imapsieve/script": &empty})

	e.Handle(context.Background(), ev)

	m, _ := u.GetMessages(ev.MailboxID, []uint32{ev.Message.UID})
	if model.HasFlag(m[0].Flags, `\Deleted`) {
		t.Fatal("empty mailbox mapping used fallback")
	}
}
func TestStaleEventDoesNotOverwrite(t *testing.T) {
	e, u, ev := fixture(t, `discard;`)
	_, _ = u.SetFlags("INBOX", []uint32{ev.Message.UID}, 0, func([]string) []string { return []string{`\Seen`} })
	e.Handle(context.Background(), ev)

	m, _ := u.GetMessages(ev.MailboxID, []uint32{ev.Message.UID})
	if model.HasFlag(m[0].Flags, `\Deleted`) {
		t.Fatal("stale mutation")
	}
}
func TestActorRightsBoundScripts(t *testing.T) {
	e, u, ev := fixture(t, `discard;`)
	ev.Actor = "guest"
	_ = u.SetACL("INBOX", "guest", "lri")

	e.Handle(context.Background(), ev)

	m, _ := u.GetMessages(ev.MailboxID, []uint32{ev.Message.UID})
	if model.HasFlag(m[0].Flags, `\Deleted`) {
		t.Fatal("script escalated rights")
	}
}
func TestChangedFlagsEnvironment(t *testing.T) {
	e, u, ev := fixture(t, `require ["imapsieve","environment","imap4flags"]; if environment :contains "imap.changedflags" "\\Seen" { addflag "processed"; }`)
	res, _ := u.SetFlags("INBOX", []uint32{ev.Message.UID}, 0, func([]string) []string { return []string{`\Seen`} })
	ev.Operation = "FLAG"
	ev.Message = res.Updated[0]
	e.Handle(context.Background(), ev)

	m, _ := u.GetMessages(ev.MailboxID, []uint32{ev.Message.UID})
	if !model.HasFlag(m[0].Flags, "processed") {
		t.Fatal("FLAG environment missing")
	}
}
func TestWhitespaceScriptKeepsOriginal(t *testing.T) {
	e, u, ev := fixture(t, " \n\t")
	e.Handle(context.Background(), ev)

	m, _ := u.ListMessages(ev.MailboxID)
	if len(m) != 1 || model.HasFlag(m[0].Flags, `\Deleted`) {
		t.Fatal("whitespace script mutated original")
	}
}
func TestIMAPRejectsDeliveryExtensions(t *testing.T) {
	for _, script := range []string{`require "vacation";keep;`, `require "reject";keep;`, `require "envelope";if envelope :is "from" "x"{discard;}`} {
		e, u, ev := fixture(t, script)
		e.Handle(context.Background(), ev)

		m, _ := u.GetMessages(ev.MailboxID, []uint32{ev.Message.UID})
		if model.HasFlag(m[0].Flags, `\Deleted`) {
			t.Fatal("invalid delivery extension applied")
		}
	}
}
func TestTransientEditsOnlyAffectCopies(t *testing.T) {
	e, u, ev := fixture(t, `require ["fileinto","copy","editheader"];fileinto :copy "Archive";addheader "X-Edited" "yes";fileinto :copy "INBOX";`)
	e.Handle(context.Background(), ev)

	original, _ := u.Read(&ev.Message)
	if strings.Contains(string(original), "X-Edited") {
		t.Fatal("original changed")
	}

	a, _ := u.Info("Archive")

	copies, _ := u.ListMessages(a.ID)
	if len(copies) != 1 {
		t.Fatal("missing copy")
	}

	raw, _ := u.Read(&copies[0])
	if strings.Contains(string(raw), "X-Edited") {
		t.Fatal("earlier action saw later edit")
	}

	in, _ := u.ListMessages(ev.MailboxID)
	if len(in) != 2 {
		t.Fatal("missing later copy")
	}

	raw, _ = u.Read(&in[1])
	if !strings.Contains(string(raw), "X-Edited") {
		t.Fatal("later copy missing edit")
	}
}

func TestFailedPipePreservesOriginal(t *testing.T) {
	for _, tag := range []string{"", ":try "} {
		e, u, ev := fixture(t, `require "vnd.dovecot.pipe"; pipe `+tag+`"missing";`)
		e.Handle(context.Background(), ev)

		m, _ := u.GetMessages(ev.MailboxID, []uint32{ev.Message.UID})
		if len(m) != 1 || model.HasFlag(m[0].Flags, `\Deleted`) {
			t.Fatal("failed pipe lost original")
		}
	}
}

func TestPipeRealProcessAndOwnerBoundary(t *testing.T) {
	for _, tc := range []struct {
		exit, actor string
		deleted     bool
	}{{"0", "test", true}, {"1", "test", false}, {"0", "guest", false}} {
		e, u, ev := fixture(t, `require "vnd.dovecot.pipe"; pipe "sink";`)
		e.Config.TimeoutMS = 5000
		dir := t.TempDir()

		marker := filepath.Join(dir, "ran")
		if err := os.WriteFile(filepath.Join(dir, "sink"), []byte("#!/bin/sh\ncat >/dev/null\nprintf yes > '"+marker+"'\nexit "+tc.exit+"\n"), 0700); err != nil {
			t.Fatal(err)
		}

		e.Programs = extprograms.New(config.ExtPrograms{BinDir: dir, Programs: map[string]config.Program{"sink": {Command: "sink", Kinds: []string{"pipe"}}}})

		ev.Actor = tc.actor
		if tc.actor != "test" {
			_ = u.SetACL("INBOX", tc.actor, model.AllRights)
		}

		e.Handle(context.Background(), ev)

		m, _ := u.GetMessages(ev.MailboxID, []uint32{ev.Message.UID})
		if len(m) != 1 || model.HasFlag(m[0].Flags, `\Deleted`) != tc.deleted {
			t.Fatalf("wrong disposition: exit=%s actor=%s flags=%v", tc.exit, tc.actor, m[0].Flags)
		}

		_, err := os.Stat(marker)
		if (err == nil) != (tc.actor == "test") {
			t.Fatal("external program crossed owner boundary")
		}
	}
}
