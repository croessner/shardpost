package imapproto

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
)

// appendMbox appends every message of an mbox fixture (imaptest format).
func appendMbox(t *testing.T, c *client, mailbox, path string) int {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	n := 0

	for _, msg := range bytes.Split(data, []byte("\nFrom ")) {
		msg = bytes.TrimPrefix(msg, []byte("From "))
		date := "" // the From_ line date is the INTERNALDATE (as imaptest does)

		if i := bytes.IndexByte(msg, '\n'); i >= 0 {
			if f := strings.Fields(string(msg[:i])); len(f) >= 7 {
				if d, err := time.Parse("Mon Jan _2 15:04:05 2006 -0700", strings.Join(f[1:7], " ")); err == nil {
					date = " " + quote(d.Format("02-Jan-2006 15:04:05 -0700"))
				}
			}

			msg = msg[i+1:]
		}

		msg = bytes.TrimSpace(msg)
		if len(msg) == 0 {
			continue
		}

		raw := strings.ReplaceAll(string(msg), "\n", "\r\n") + "\r\n"
		c.ok("APPEND %s%s {%d+}\r\n%s", mailbox, date, len(raw), raw)

		n++
	}

	return n
}

func TestSortSubjectImaptest(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE sortbox")

	n := appendMbox(t, c, "sortbox", "testdata/sort-subject.mbox")
	if n != 15 {
		t.Fatalf("appended %d", n)
	}

	c.ok("SELECT sortbox")

	cases := map[string]string{
		"SORT (SUBJECT) US-ASCII ALL":              "* SORT 9 10 1 14 3 5 11 6 15 2 7 12 13 8 4",
		"SORT (REVERSE SUBJECT) US-ASCII ALL":      "* SORT 4 8 2 7 12 13 15 6 3 5 11 14 1 9 10",
		"SORT (SUBJECT REVERSE SIZE) US-ASCII ALL": "* SORT 10 9 1 14 11 5 3 6 15 13 12 7 2 8 4",
	}
	for cmd, want := range cases {
		lines := c.ok("%s", cmd)
		if !has(lines, want) {
			t.Errorf("%s: got %v want %q", cmd, lines, want)
		}
	}
}

func TestThreadReferencesImaptest(t *testing.T) {
	c, _ := newTestServer(t)
	c.ok("LOGIN test test")
	c.ok("CREATE t3")
	// thread3: duplicate Message-IDs, grouped by subject
	c.ok("APPEND t3 {40+}\r\nMessage-Id: <a@b>\r\nSubject: foo\r\n\r\nbody\r\n")
	c.ok("APPEND t3 {44+}\r\nMessage-Id: <a@b>\r\nSubject: Re: foo\r\n\r\nbody\r\n")
	c.ok("SELECT t3")

	mustHave(t, c.ok("THREAD REFERENCES US-ASCII ALL"), "* THREAD (1 2)")

	c.ok("CREATE t4")
	// thread4: a dangling reference plus subject grouping
	c.ok("APPEND t4 {40+}\r\nMessage-Id: <a@b>\r\nSubject: foo\r\n\r\nbody\r\n")
	c.ok("APPEND t4 {64+}\r\nMessage-Id: <b@b>\r\nReferences: <c@b>\r\nSubject: Re: foo\r\n\r\nbody\r\n")
	c.ok("SELECT t4")

	mustHave(t, c.ok("THREAD REFERENCES US-ASCII ALL"), "* THREAD (1 2)")
	// envelope keeps the brackets of the ids
	mustHave(t, c.ok("FETCH 2 ENVELOPE"), `"<b@b>"`)
}
