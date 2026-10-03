package jmap

import (
	"testing"
	"time"

	"shardpost/internal/model"
)

// The mailbox lock of mailbox_delete (stage B17): a creation, copy or
// mailboxIds patch into a locked mailbox makes mailboxIds invalid, a
// keyword update or destruction of a message there and an update or
// destruction of the mailbox are forbidden; reading goes on.
func TestMailboxLockSetErrors(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, "Archive", plainMessage)
	in := deliver(t, u, inboxName, plainMessage)

	archive, _ := u.Lookup("Archive")
	if err := u.(model.MailboxLocker).LockMailboxes("job-1", []model.MailboxLockTarget{{ID: archive.ID, Name: archive.Name}}, time.Minute); err != nil {
		t.Fatal(err)
	}

	// create into the locked mailbox
	res := mailCall(t, s, "Email/set", `{"accountId":"test","create":{"c":{"mailboxIds":{"`+archive.ID+`":true},
		"from":[{"email":"a@example.org"}],"subject":"x","bodyStructure":{"type":"text/plain","partId":"1"},"bodyValues":{"1":{"value":"hi"}}}}}`)
	if got := setErrorType(res, "notCreated", "c"); got != setInvalidProperties {
		t.Errorf("create: %q (%#v)", got, res["notCreated"])
	}

	// mailboxIds patch of a message into the locked mailbox
	res = mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+in.GUID+`":{"mailboxIds/`+archive.ID+`":true}}}`)
	if got := setErrorType(res, "notUpdated", in.GUID); got != setInvalidProperties {
		t.Errorf("patch: %q", got)
	}

	// keywords of a message in the locked mailbox
	res = mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+m.GUID+`":{"keywords/$seen":true}}}`)
	if got := setErrorType(res, "notUpdated", m.GUID); got != setForbidden {
		t.Errorf("keywords: %q", got)
	}

	// destroy
	res = mailCall(t, s, "Email/set", `{"accountId":"test","destroy":["`+m.GUID+`"]}`)
	if got := setErrorType(res, "notDestroyed", m.GUID); got != setForbidden {
		t.Errorf("destroy: %q", got)
	}

	// Mailbox/set destroy and rename
	res = mailCall(t, s, "Mailbox/set", `{"accountId":"test","destroy":["`+archive.ID+`"],"onDestroyRemoveEmails":true}`)
	if got := setErrorType(res, "notDestroyed", archive.ID); got != setForbidden {
		t.Errorf("mailbox destroy: %q", got)
	}

	res = mailCall(t, s, "Mailbox/set", `{"accountId":"test","update":{"`+archive.ID+`":{"name":"Elsewhere"}}}`)
	if got := setErrorType(res, "notUpdated", archive.ID); got != setForbidden {
		t.Errorf("mailbox rename: %q", got)
	}

	// reading goes on
	if got := getEmail(t, s, m.GUID, `["subject"]`); got[propSubject] == nil {
		t.Errorf("read: %#v", got)
	}

	// INBOX is untouched
	res = mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+in.GUID+`":{"keywords/$seen":true}}}`)
	if _, ok := res["updated"].(map[string]any)[in.GUID]; !ok {
		t.Errorf("INBOX update failed: %#v", res)
	}
}
