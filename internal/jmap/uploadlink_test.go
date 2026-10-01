package jmap

import (
	"testing"
)

// TestUploadBlobsAreNeverLinked holds the invariant the store's upload
// expiry relies on (scaling plan B6): an uploaded blob is never the blob
// of a message. Email/import and Email/set read the upload and store a
// new message under a GUID of its own, so expiring the upload (its
// "up:" intent and queue entry, cephstore/uploads.go) can never take a
// message's blob away.
func TestUploadBlobsAreNeverLinked(t *testing.T) {
	s, u := emailServer(t)
	boxes := boxIDs(t, u)

	imported := upload(t, s, plainMessage, "message/rfc822")
	attached := upload(t, s, "PNG\x00\x01binary", "image/png")

	res := mailCall(t, s, "Email/import", `{"accountId":"test","emails":{
		"a":{"blobId":"`+imported+`","mailboxIds":{"`+boxes[inboxName]+`":true,"`+boxes["Archive"]+`":true}}}}`)
	if res["created"] == nil {
		t.Fatalf("the import failed: %#v", res["notCreated"])
	}

	res = mailCall(t, s, "Email/set", `{"accountId":"test","create":{"c1":{
		"mailboxIds":{"`+boxes[inboxName]+`":true},
		"subject":"with attachment",
		"textBody":[{"partId":"t"}],
		"attachments":[{"blobId":"`+attached+`","type":"image/png"}],
		"bodyValues":{"t":{"value":"see attached"}}}}}`)
	if res["created"] == nil {
		t.Fatalf("the create failed: %#v", res["notCreated"])
	}

	boxesNow, err := u.ListMailboxes()
	if err != nil {
		t.Fatal(err)
	}

	n := 0

	for _, mb := range boxesNow {
		metas, err := u.ListMessages(mb.ID)
		if err != nil {
			t.Fatal(err)
		}

		for _, m := range metas {
			n++

			if m.GUID == imported || m.GUID == attached {
				t.Errorf("message %d in %s is stored under the upload blob %s", m.UID, mb.Name, m.GUID)
			}
		}
	}

	if n != 3 {
		t.Fatalf("%d messages stored, want 3", n)
	}
}
