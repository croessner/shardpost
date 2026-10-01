package delivery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"shardpost/internal/model"
)

// scriptDir writes scripts (file name to text) into a fresh directory.
func scriptDir(t *testing.T, scripts map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, text := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

const listMessage = "From: sender@example.org\r\nTo: user@example.org\r\nList-Id: <dovecot.dovecot.org>\r\nSubject: hi\r\n\r\nbody\r\n"

const afterLists = `require "fileinto"; if header :contains "List-Id" "dovecot.dovecot.org" { fileinto "Lists"; stop; }`

// TestGlobalScriptsAroundThePersonalOne: the sequence is before,
// personal, after; a script runs only while the message is still kept,
// and that keep is handed on instead of being carried out.
func TestGlobalScriptsAroundThePersonalOne(t *testing.T) {
	for _, c := range []struct {
		name            string
		personal        string
		before          string
		raw             string
		inbox, lists, x int
	}{
		{"no personal script: the after script files", "", "", listMessage, 0, 1, 0},
		{"after script does not match: the keep is carried out once", "", "", message, 1, 0, 0},
		// An explicit keep is an action: the after script's fileinto
		// does not take it away.
		{"a personal explicit keep stays, the after script files too", `keep;`, "", listMessage, 1, 1, 0},
		{"a personal implicit keep is handed on", `if false { discard; }`, "", listMessage, 0, 1, 0},
		{"a before explicit keep survives a personal discard", `discard;`, `keep;`, message, 1, 0, 0},
		{"a before explicit keep survives a personal fileinto", `require "fileinto"; fileinto "X";`, `keep;`, message, 1, 0, 1},
		{"a personal fileinto ends the sequence", `require "fileinto"; fileinto "X";`, "", listMessage, 0, 0, 1},
		{"a personal copy and the after script add up", `require ["fileinto", "copy"]; fileinto :copy "X";`, "", listMessage, 0, 1, 1},
		{"a personal discard ends the sequence", `discard;`, "", listMessage, 0, 0, 0},
		{"a before script that discards wins over everything", `keep;`, `discard;`, listMessage, 0, 0, 0},
		{"a broken personal script is skipped, the after script still runs", `require "nope"; keep;`, "", listMessage, 0, 1, 0},
		{"a broken before script is skipped", `keep;`, `this is not sieve`, message, 1, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, store := testService(t, c.personal)
			svc.ScriptsAfter = scriptDir(t, map[string]string{"50-lists.sieve": afterLists, "ignored.txt": "discard;", ".hidden.sieve": "discard;"})

			if c.before != "" {
				svc.ScriptsBefore = scriptDir(t, map[string]string{"10-first.sieve": c.before})
			}

			req := request(c.raw)
			if _, err := svc.Deliver(context.Background(), req); err != nil {
				t.Fatal(err)
			}

			if in, lists, x := count(t, store, "INBOX"), count(t, store, "Lists"), count(t, store, "X"); in != c.inbox || lists != c.lists || x != c.x {
				t.Fatalf("INBOX %d, Lists %d, X %d; want %d, %d, %d", in, lists, x, c.inbox, c.lists, c.x)
			}
		})
	}
}

// TestGlobalScriptsRunInNameOrder: the scripts of a directory run in
// name order, and a header edit of one is what the next one sees.
func TestGlobalScriptsRunInNameOrder(t *testing.T) {
	svc, store := testService(t, `require "fileinto"; if header :is "X-Step" "one" { fileinto "Seen"; }`)
	svc.ScriptsBefore = scriptDir(t, map[string]string{
		"20-second.sieve": `require ["editheader", "fileinto"]; if not header :is "X-Step" "one" { fileinto "Wrong"; }`,
		"10-first.sieve":  `require "editheader"; addheader "X-Step" "one";`,
	})

	if _, err := svc.Deliver(context.Background(), request(message)); err != nil {
		t.Fatal(err)
	}

	if seen, wrong := count(t, store, "Seen"), count(t, store, "Wrong"); seen != 1 || wrong != 0 {
		t.Fatalf("Seen %d, Wrong %d", seen, wrong)
	}
}

// TestGlobalScriptFilesIntoPublicFolder: the after script files list
// mail into a public folder for an account with the post right and
// leaves it in INBOX for one without.
func TestGlobalScriptFilesIntoPublicFolder(t *testing.T) {
	for _, c := range []struct {
		rights        string
		inbox, public int
	}{{"lrp", 0, 1}, {"lr", 1, 0}} {
		svc, store := testService(t, "")
		svc.PublicPrefix = "Public/"
		svc.ScriptsAfter = scriptDir(t, map[string]string{"lists.sieve": `require "fileinto"; if exists "List-Id" { fileinto "Public/Lists/Dovecot"; stop; }`})

		pub, err := store.User(model.PublicNamespace)
		if err != nil {
			t.Fatal(err)
		}

		if err := pub.CreateMailbox("Lists/Dovecot", nil); err != nil {
			t.Fatal(err)
		}

		if err := pub.SetACL("Lists/Dovecot", account, c.rights); err != nil {
			t.Fatal(err)
		}

		if _, err := svc.Deliver(context.Background(), request(listMessage)); err != nil {
			t.Fatal(err)
		}

		mb, _ := pub.Lookup("Lists/Dovecot")
		metas, _ := pub.ListMessages(mb.ID)

		if in := count(t, store, "INBOX"); in != c.inbox || len(metas) != c.public {
			t.Fatalf("rights %q: INBOX %d, public %d", c.rights, in, len(metas))
		}
	}
}

// TestUnreadableScriptDirStillDelivers: a directory that went away does
// not cost the mail.
func TestUnreadableScriptDirStillDelivers(t *testing.T) {
	svc, store := testService(t, "")
	svc.ScriptsAfter = filepath.Join(t.TempDir(), "gone")

	if _, err := svc.Deliver(context.Background(), request(message)); err != nil {
		t.Fatal(err)
	}

	if count(t, store, "INBOX") != 1 {
		t.Fatal("not in INBOX")
	}
}

// TestKeepFlagsAreHandedOn: the flags of a keep are what the next
// script starts with. A mark survives a later script that just keeps the
// message, and a later script can take it away or replace it.
func TestKeepFlagsAreHandedOn(t *testing.T) {
	for _, c := range []struct {
		personal, after string
		want, not       []string
	}{
		{`require "imap4flags"; addflag "\\Seen";`, `require "imap4flags"; addflag "$later";`, []string{`\Seen`, "$later"}, nil},
		{`require "imap4flags"; keep :flags "\\Seen";`, `keep;`, []string{`\Seen`}, nil},
		{`require "imap4flags"; addflag ["\\Seen", "$junk"];`, `require "imap4flags"; removeflag "$junk";`, []string{`\Seen`}, []string{"$junk"}},
		{`require "imap4flags"; addflag ["\\Seen", "$Label1"];`, `require "imap4flags"; removeflag ["\\Seen", "$Label1"];`, nil, []string{`\Seen`, "$Label1"}},
		{`require "imap4flags"; addflag "\\Seen";`, ``, []string{`\Seen`}, nil},
		{`require "imap4flags"; addflag "\\Seen";`, `require "imap4flags"; setflag "\\Flagged";`, []string{`\Flagged`}, []string{`\Seen`}},
		{`require "imap4flags"; addflag "\\Seen";`, `require "imap4flags"; keep :flags "\\Flagged";`, []string{`\Flagged`}, []string{`\Seen`}},
		{`require "imap4flags"; addflag "\\Seen";`, `this is not sieve`, []string{`\Seen`}, nil},
	} {
		svc, store := testService(t, c.personal)
		svc.ScriptsAfter = scriptDir(t, map[string]string{"50-lists.sieve": afterLists, "60-flag.sieve": c.after})

		if _, err := svc.Deliver(context.Background(), request(message)); err != nil {
			t.Fatal(err)
		}

		u, _ := store.User(account)
		mb, _ := u.Lookup("INBOX")

		metas, _ := u.ListMessages(mb.ID)
		if len(metas) != 1 {
			t.Fatalf("%s / %s: %d messages", c.personal, c.after, len(metas))
		}

		for _, f := range c.want {
			if !model.HasFlag(metas[0].Flags, f) {
				t.Fatalf("%s / %s: %s missing in %v", c.personal, c.after, f, metas[0].Flags)
			}
		}

		for _, f := range c.not {
			if model.HasFlag(metas[0].Flags, f) {
				t.Fatalf("%s / %s: %s still in %v", c.personal, c.after, f, metas[0].Flags)
			}
		}
	}
}

// TestRejectInASequence: a before script that rejects ends the sequence
// (the personal script does not run); a later script that rejects after
// an earlier script's delivering action fails with an action conflict
// (RFC 5429 2.1), and the message stays kept with what the earlier
// script did.
func TestRejectInASequence(t *testing.T) {
	const reject = `require "ereject"; ereject "no";`

	for _, c := range []struct {
		name, before, personal, after string
		rejected                      bool
		inbox, x                      int
	}{
		{"a before reject ends the sequence", reject, `require "fileinto"; fileinto "X";`, "", true, 0, 0},
		{"a reject after an implicit keep", `require "fileinto"; if false { discard; }`, reject, "", true, 0, 0},
		{"a reject after fileinto :copy", `require ["fileinto", "copy"]; fileinto :copy "X";`, reject, "", false, 1, 1},
		{"a reject after an explicit keep", `keep;`, reject, "", false, 1, 0},
		{"a reject after a redirect :copy", `require "copy"; redirect :copy "other@example.org";`, reject, "", false, 1, 0},
		{"a reject after a vacation", `require "vacation"; vacation "away";`, reject, "", false, 1, 0},
		{"an explicit keep stays explicit across scripts", `keep;`, `if false { discard; }`, reject, false, 1, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			svc, store := testService(t, c.personal)
			svc.ScriptsBefore = scriptDir(t, map[string]string{"10-first.sieve": c.before})

			if c.after != "" {
				svc.ScriptsAfter = scriptDir(t, map[string]string{"90-last.sieve": c.after})
			}

			_, err := svc.Deliver(context.Background(), request(message))

			var rej *RejectedError
			if errors.As(err, &rej) != c.rejected || (!c.rejected && err != nil) {
				t.Fatalf("rejected %v: %v", c.rejected, err)
			}

			if in, x := count(t, store, "INBOX"), count(t, store, "X"); in != c.inbox || x != c.x {
				t.Fatalf("INBOX %d, X %d; want %d, %d", in, x, c.inbox, c.x)
			}
		})
	}
}

// TestUnreadableScriptDoesNotCostTheOthers: one file of a directory that
// cannot be read (a dangling link) leaves the other scripts running.
func TestUnreadableScriptDoesNotCostTheOthers(t *testing.T) {
	svc, store := testService(t, "")
	svc.ScriptsAfter = scriptDir(t, map[string]string{"50-lists.sieve": afterLists})

	if err := os.Symlink(filepath.Join(svc.ScriptsAfter, "gone"), filepath.Join(svc.ScriptsAfter, "10-dangling.sieve")); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Deliver(context.Background(), request(listMessage)); err != nil {
		t.Fatal(err)
	}

	if count(t, store, "Lists") != 1 {
		t.Fatal("the readable script did not run")
	}
}
