package jmap

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// emailServer is a front-end over a store the test keeps a handle on.
func emailServer(t *testing.T) (*Server, model.User) {
	t.Helper()

	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []string{inboxName, "Archive"} {
		if err := u.CreateMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	return newTestServer(t, func(o *Options) { o.Store = store }), u
}

// deliver stores a message and returns its index record.
func deliver(t *testing.T, u model.User, mailbox, raw string, flags ...string) *model.Meta {
	t.Helper()

	m, err := u.Deliver(mailbox, []byte(raw), flags, time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	return m
}

// getEmail is Email/get of one id with the given property list (a JSON
// array, or "" for the default), failing the test when it is not found.
func getEmail(t *testing.T, s *Server, id, props string, extra ...string) map[string]any {
	t.Helper()

	args := `{"accountId":"test","ids":["` + id + `"]`
	if props != "" {
		args += `,"properties":` + props
	}

	for _, e := range extra {
		args += "," + e
	}

	res := mailCall(t, s, "Email/get", args+"}")

	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get of %s gave %#v (notFound %#v)", id, list, res["notFound"])
	}

	obj, _ := list[0].(map[string]any)

	return obj
}

const plainMessage = "From: Alice <alice@example.org>\r\n" +
	"To: Bob <bob@example.org>, carol@example.org\r\n" +
	"Subject: =?UTF-8?Q?Gr=C3=BC=C3=9Fe?=\r\n" +
	"Date: Sun, 20 Sep 2026 12:00:00 +0200\r\n" +
	"Message-ID: <one@example.org>\r\n" +
	"In-Reply-To: <zero@example.org>\r\n" +
	"X-Tag: first\r\n" +
	"X-Tag: second\r\n" +
	"\r\n" +
	"Hello Bob\r\n"

// TestEmailGetFromTheIndex: the list-view properties come from the index
// record without the message being read, and match what the message
// says.
func TestEmailGetFromTheIndex(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage, `\Seen`, "$Forwarded", `\Deleted`)

	got := getEmail(t, s, m.GUID, `["subject","from","to","sentAt","receivedAt","messageId",
		"inReplyTo","keywords","mailboxIds","size","blobId","threadId","preview"]`)

	want := map[string]any{
		propSubject:    "Grüße",
		propSentAt:     "2026-09-20T12:00:00+02:00",
		propReceivedAt: "2026-09-20T10:00:00Z",
		propBlobID:     m.GUID,
	}

	for p, v := range want {
		if got[p] != v {
			t.Errorf("%s is %v, want %v", p, got[p], v)
		}
	}

	checkAddresses(t, got)
	checkIDs(t, got)

	kw, _ := got[propKeywords].(map[string]any)
	if kw["$seen"] != true || kw["$forwarded"] != true || len(kw) != 2 {
		t.Errorf("keywords are %#v, want $seen and $forwarded and not \\Deleted", kw)
	}
}

// checkAddresses checks the address properties of plainMessage.
func checkAddresses(t *testing.T, got map[string]any) {
	t.Helper()

	from, _ := got[propFrom].([]any)
	if len(from) != 1 {
		t.Fatalf("from is %#v", got[propFrom])
	}

	if a, _ := from[0].(map[string]any); a["email"] != "alice@example.org" || a["name"] != "Alice" {
		t.Errorf("from is %#v", a)
	}

	if to, _ := got[propTo].([]any); len(to) != 2 {
		t.Errorf("to is %#v, want two addresses", got[propTo])
	}
}

// checkIDs checks the Message-ID properties of plainMessage.
func checkIDs(t *testing.T, got map[string]any) {
	t.Helper()

	for p, want := range map[string]string{propMessageID: "one@example.org", propInReplyTo: "zero@example.org"} {
		if ids, _ := got[p].([]any); len(ids) != 1 || ids[0] != want {
			t.Errorf("%s is %#v, want [%s]", p, got[p], want)
		}
	}
}

// TestEmailGetReadsTheHeaderWhereTheEnvelopeIsWrong: an IMAP envelope
// fills an absent Sender and Reply-To from From and merges References
// with In-Reply-To. JMAP says null and keeps them apart, so these three
// come from the header itself.
func TestEmailGetReadsTheHeaderWhereTheEnvelopeIsWrong(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage)

	got := getEmail(t, s, m.GUID, `["sender","replyTo","references"]`)

	for _, p := range []string{propSender, propReplyTo, propReferences} {
		if got[p] != nil {
			t.Errorf("%s is %#v, want null for a message without that header", p, got[p])
		}
	}

	withAll := "Sender: Desk <desk@example.org>\r\nReply-To: list@example.org\r\n" +
		"References: <a@example.org> <b@example.org>\r\n" + plainMessage
	m2 := deliver(t, u, inboxName, withAll)

	got = getEmail(t, s, m2.GUID, `["sender","replyTo","references"]`)

	if sender, _ := got[propSender].([]any); len(sender) != 1 {
		t.Errorf("sender is %#v", got[propSender])
	}

	if refs, _ := got[propReferences].([]any); len(refs) != 2 {
		t.Errorf("references is %#v, want the References header alone", got[propReferences])
	}
}

// TestEmailGetHeaderForms: header:*: in every form, the last instance
// without :all and all of them with it, and a form the field does not
// allow refused.
func TestEmailGetHeaderForms(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage)

	got := getEmail(t, s, m.GUID, `["header:X-Tag","header:x-tag:all","header:Subject:asText",
		"header:From:asAddresses","header:To:asGroupedAddresses","header:Date:asDate",
		"header:Message-ID:asMessageIds","header:X-Missing","header:X-Missing:all"]`)

	if got["header:X-Tag"] != " second" {
		t.Errorf("header:X-Tag is %q, want the last instance, raw", got["header:X-Tag"])
	}

	if all, _ := got["header:x-tag:all"].([]any); len(all) != 2 || all[0] != " first" {
		t.Errorf("header:x-tag:all is %#v, want both in order", got["header:x-tag:all"])
	}

	if got["header:Subject:asText"] != "Grüße" {
		t.Errorf("asText is %v", got["header:Subject:asText"])
	}

	if groups, _ := got["header:To:asGroupedAddresses"].([]any); len(groups) != 1 {
		t.Errorf("asGroupedAddresses is %#v, want one unnamed group", groups)
	}

	if got["header:Date:asDate"] != "2026-09-20T12:00:00+02:00" {
		t.Errorf("asDate is %v", got["header:Date:asDate"])
	}

	if got["header:X-Missing"] != nil {
		t.Errorf("an absent header is %#v, want null", got["header:X-Missing"])
	}

	if all, ok := got["header:X-Missing:all"].([]any); !ok || len(all) != 0 {
		t.Errorf("an absent header with :all is %#v, want []", got["header:X-Missing:all"])
	}

	if got := mailError(t, s, "Email/get",
		`{"accountId":"test","ids":["`+m.GUID+`"],"properties":["header:From:asDate"]}`); got != errInvalidArguments {
		t.Errorf("a form From does not allow gave %s, want %s", got, errInvalidArguments)
	}
}

const alternativeMessage = "From: a@example.org\r\n" +
	"Subject: alt\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=\"outer\"\r\n" +
	"\r\n" +
	"preamble\r\n" +
	"--outer\r\n" +
	"Content-Type: multipart/alternative; boundary=\"inner\"\r\n" +
	"\r\n" +
	"--inner\r\n" +
	"Content-Type: text/plain; charset=iso-8859-1\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n" +
	"\r\n" +
	"Gr=FC=DFe aus K=F6ln\r\n" +
	"--inner\r\n" +
	"Content-Type: text/html; charset=utf-8\r\n" +
	"\r\n" +
	"<p>Grüße aus Köln</p>\r\n" +
	"--inner--\r\n" +
	"--outer\r\n" +
	"Content-Type: application/pdf; name=\"report.pdf\"\r\n" +
	"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--outer--\r\n" +
	"epilogue\r\n"

// TestEmailGetBody: the part tree, the three lists RFC 8621 4.1.4
// derives from it, and the body values decoded from their charset.
func TestEmailGetBody(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, alternativeMessage)

	got := getEmail(t, s, m.GUID, `["bodyStructure","textBody","htmlBody","attachments","hasAttachment","bodyValues"]`,
		`"fetchAllBodyValues":true`)

	text := onePart(t, got, propTextBody, mediaTextPlain)
	onePart(t, got, propHTMLBody, mediaTextHTML)
	att := onePart(t, got, propAttachments, "application/pdf")

	if att[partName] != "report.pdf" {
		t.Errorf("the attachment is named %v, want report.pdf", att[partName])
	}

	if att[partSize] != float64(len("%PDF-1.4\n")) {
		t.Errorf("the attachment size is %v, want its decoded size", att[partSize])
	}

	if got[propHasAttachment] != true {
		t.Error("hasAttachment is false with a pdf attached")
	}

	values, _ := got[propBodyValues].(map[string]any)
	plain, _ := values[text[partPartID].(string)].(map[string]any)

	// The line break before the next boundary belongs to the boundary
	// (RFC 2046 5.1.1), not to the text.
	if plain["value"] != "Grüße aus Köln" || plain["isEncodingProblem"] != false {
		t.Errorf("the plain body value is %#v, want the ISO-8859-1 text in UTF-8", plain)
	}

	root, _ := got[propBodyStructure].(map[string]any)
	if root[partType] != "multipart/mixed" || root[partPartID] != nil || root[partBlobID] != nil {
		t.Errorf("the root is %#v, want a multipart without partId and blobId", root)
	}
}

// onePart returns the single part of a body list, failing the test when
// the list does not hold exactly one part of the given type.
func onePart(t *testing.T, got map[string]any, prop, mediaType string) map[string]any {
	t.Helper()

	list, _ := got[prop].([]any)
	if len(list) != 1 {
		t.Fatalf("%s is %#v, want one part", prop, list)
	}

	part, _ := list[0].(map[string]any)
	if part[partType] != mediaType {
		t.Errorf("%s holds a %v part, want %s", prop, part[partType], mediaType)
	}

	return part
}

// TestEmailGetBodyValueTruncation: maxBodyValueBytes cuts on a character
// boundary and says it did.
func TestEmailGetBodyValueTruncation(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, "Subject: x\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nüüüü\r\n")

	got := getEmail(t, s, m.GUID, `["textBody","bodyValues"]`, `"fetchTextBodyValues":true`, `"maxBodyValueBytes":3`)

	values, _ := got[propBodyValues].(map[string]any)
	v, _ := values["1"].(map[string]any)

	if v["value"] != "ü" || v["isTruncated"] != true {
		t.Errorf("the truncated value is %#v, want one whole ü", v)
	}
}

// TestEmailPartDownload: a body part's blob is its bytes with the
// transfer encoding undone, which is what a client saves as the file.
func TestEmailPartDownload(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, alternativeMessage)

	att := getEmail(t, s, m.GUID, `["attachments"]`)[propAttachments].([]any)[0].(map[string]any)
	blobID := att[partBlobID].(string)

	r := httptest.NewRequest(http.MethodGet, PathDownload+"test/"+blobID+"/report.pdf?accept=application/pdf", nil)
	r.SetBasicAuth("test", "secret")

	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	body, _ := io.ReadAll(w.Body)
	if w.Code != http.StatusOK || string(body) != "%PDF-1.4\n" {
		t.Errorf("the part download answered %d with %q", w.Code, body)
	}
}

// TestEmailIDSurvivesAMove: the id is the GUID, so a move changes the
// mailboxes of the message, not the message.
func TestEmailIDSurvivesAMove(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage)

	archive, err := u.Lookup("Archive")
	if err != nil {
		t.Fatal(err)
	}

	getEmail(t, s, m.GUID, `["mailboxIds"]`) // builds the locator

	if _, err := u.Move(inboxName, []model.Meta{*m}, "Archive"); err != nil {
		t.Fatal(err)
	}

	got := getEmail(t, s, m.GUID, `["mailboxIds"]`)

	boxes, _ := got[propMailboxIDs].(map[string]any)
	if len(boxes) != 1 || boxes[archive.ID] != true {
		t.Errorf("after the move mailboxIds is %#v, want only Archive", boxes)
	}
}

// TestEmailCopyIsOneMessageInTwoMailboxes: an IMAP COPY keeps the GUID,
// so the copy is the same message in a second mailbox, with the keywords
// of both copies.
func TestEmailCopyIsOneMessageInTwoMailboxes(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage)

	getEmail(t, s, m.GUID, `["mailboxIds"]`)

	copies, err := u.Copy([]model.Meta{*m}, "Archive")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := u.SetFlags("Archive", []uint32{copies[0].UID}, 0, func([]string) []string {
		return []string{`\Seen`}
	}); err != nil {
		t.Fatal(err)
	}

	got := getEmail(t, s, m.GUID, `["mailboxIds","keywords"]`)

	if boxes, _ := got[propMailboxIDs].(map[string]any); len(boxes) != 2 {
		t.Errorf("mailboxIds is %#v, want both mailboxes", boxes)
	}

	if kw, _ := got[propKeywords].(map[string]any); kw["$seen"] != true {
		t.Errorf("keywords are %#v, want $seen from the copy that was read", kw)
	}
}

// TestEmailGetNotFound: an id the account does not have, and one whose
// message was expunged since the locator last saw it.
func TestEmailGetNotFound(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage)

	getEmail(t, s, m.GUID, `["size"]`)

	if err := u.Expunge(inboxName, []model.Meta{*m}); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Email/get", `{"accountId":"test","ids":["`+m.GUID+`","nope"]}`)

	notFound, _ := res["notFound"].([]any)
	if len(notFound) != 2 {
		t.Errorf("notFound is %#v, want both ids", notFound)
	}
}

// TestEmailGetRefuses: no ids, an unknown property.
func TestEmailGetRefuses(t *testing.T) {
	s, _ := emailServer(t)

	if got := mailError(t, s, "Email/get", `{"accountId":"test"}`); got != errRequestTooLarge {
		t.Errorf("Email/get without ids gave %s, want %s", got, errRequestTooLarge)
	}

	if got := mailError(t, s, "Email/get", `{"accountId":"test","ids":[],"properties":["subjct"]}`); got != errInvalidArguments {
		t.Errorf("an unknown property gave %s, want %s", got, errInvalidArguments)
	}

	if got := mailError(t, s, "Email/get",
		`{"accountId":"test","ids":[],"bodyProperties":["nonsense"]}`); got != errInvalidArguments {
		t.Errorf("an unknown body property gave %s, want %s", got, errInvalidArguments)
	}
}

// emailState is the current Email state of the account.
func currentState(t *testing.T, s *Server) string {
	t.Helper()

	return mailCall(t, s, "Email/get", `{"accountId":"test","ids":[]}`)["state"].(string)
}

// TestEmailChanges: a delivery is a created message, a flag change an
// updated one, an expunge a destroyed one — and a move or a copy is an
// update of the message, not a new one.
func TestEmailChanges(t *testing.T) {
	s, u := emailServer(t)
	kept := deliver(t, u, inboxName, plainMessage)
	gone := deliver(t, u, inboxName, "Subject: gone\r\n\r\nbody\r\n")
	moved := deliver(t, u, inboxName, "Subject: moved\r\n\r\nbody\r\n")

	state := currentState(t, s)

	fresh := deliver(t, u, inboxName, "Subject: fresh\r\n\r\nbody\r\n")

	if _, err := u.SetFlags(inboxName, []uint32{kept.UID}, 0, func([]string) []string { return []string{`\Flagged`} }); err != nil {
		t.Fatal(err)
	}

	if err := u.Expunge(inboxName, []model.Meta{*gone}); err != nil {
		t.Fatal(err)
	}

	if _, err := u.Move(inboxName, []model.Meta{*moved}, "Archive"); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Email/changes", `{"accountId":"test","sinceState":"`+state+`"}`)

	want := map[string][]string{
		"created":   {fresh.GUID},
		"updated":   sortedIDs(kept.GUID, moved.GUID),
		"destroyed": {gone.GUID},
	}

	for key, ids := range want {
		got, _ := res[key].([]any)
		if len(got) != len(ids) {
			t.Errorf("%s is %#v, want %v", key, got, ids)
			continue
		}

		for i := range ids {
			if got[i] != ids[i] {
				t.Errorf("%s is %#v, want %v", key, got, ids)
			}
		}
	}

	if _, ok := res["updatedProperties"]; ok {
		t.Error("Email/changes carries updatedProperties, which belongs to Mailbox/changes")
	}
}

// sortedIDs is the ids in the order a /changes response lists them.
func sortedIDs(ids ...string) []string {
	out := append([]string(nil), ids...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}

	return out
}

// TestEmailChangesFoldsWhatCameAndWent: a message delivered and
// expunged inside one window is in none of the lists.
func TestEmailChangesFoldsWhatCameAndWent(t *testing.T) {
	s, u := emailServer(t)
	state := currentState(t, s)

	m := deliver(t, u, inboxName, "Subject: brief\r\n\r\nbody\r\n")
	if err := u.Expunge(inboxName, []model.Meta{*m}); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Email/changes", `{"accountId":"test","sinceState":"`+state+`"}`)
	for _, key := range []string{"created", "updated", "destroyed"} {
		if n := len(res[key].([]any)); n != 0 {
			t.Errorf("%s is %#v, want it empty", key, res[key])
		}
	}
}

// TestThreads: a reply joins the thread of the message it answers, the
// thread lists its messages oldest first, and Thread/changes reports the
// thread a new message joined.
func TestThreads(t *testing.T) {
	s, u := emailServer(t)

	first := deliver(t, u, inboxName, "Message-ID: <root@example.org>\r\nSubject: talk\r\n\r\nbody\r\n")

	thread := getEmail(t, s, first.GUID, `["threadId"]`)[propThreadID].(string)
	state := currentState(t, s)

	reply, err := u.Deliver(inboxName, []byte("Message-ID: <re@example.org>\r\nReferences: <root@example.org>\r\n"+
		"Subject: Re: talk\r\n\r\nbody\r\n"), nil, time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	if got := getEmail(t, s, reply.GUID, `["threadId"]`)[propThreadID]; got != thread {
		t.Fatalf("the reply is in thread %v, want %v", got, thread)
	}

	res := mailCall(t, s, "Thread/get", `{"accountId":"test","ids":["`+thread+`"]}`)

	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Thread/get gave %#v", res)
	}

	ids, _ := list[0].(map[string]any)["emailIds"].([]any)
	if len(ids) != 2 || ids[0] != first.GUID || ids[1] != reply.GUID {
		t.Errorf("emailIds is %#v, want the root then the reply", ids)
	}

	changes := mailCall(t, s, "Thread/changes", `{"accountId":"test","sinceState":"`+state+`"}`)
	if updated, _ := changes["updated"].([]any); len(updated) != 1 || updated[0] != thread {
		t.Errorf("Thread/changes gave updated %#v, want the thread the reply joined", changes["updated"])
	}
}

// TestLocatorHealsAStalePlace: a place that no longer holds its message
// is dropped when it is used, and the message is found where it is.
func TestLocatorHealsAStalePlace(t *testing.T) {
	_, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage)

	loc, err := newLocator(u)
	if err != nil {
		t.Fatal(err)
	}

	state, err := u.AccountState()
	if err != nil {
		t.Fatal(err)
	}

	if err := loc.catchUp(state); err != nil {
		t.Fatal(err)
	}

	// A place the locator believes in that holds nothing.
	ix, _ := u.(model.Index)
	if err := ix.IndexApply(locSpace, []string{locEmail + m.GUID}, func(_ []string, cur map[string][]byte) (map[string][]byte, []string) {
		rec := decodeEmailRec(cur[locEmail+m.GUID])
		rec.Places = append(rec.Places, place{Mailbox: "nowhere", UID: 99})

		return map[string][]byte{locEmail + m.GUID: encode(&rec)}, nil
	}); err != nil {
		t.Fatal(err)
	}

	hits, err := loc.resolve([]string{m.GUID})
	if err != nil {
		t.Fatal(err)
	}

	if f := hits[m.GUID]; f == nil || len(f.metas) != 1 {
		t.Fatalf("the message was not found where it is: %#v", hits)
	}

	recs, err := loc.emailRecs([]string{m.GUID})
	if err != nil {
		t.Fatal(err)
	}

	if n := len(recs[m.GUID].Places); n != 1 {
		t.Errorf("the record still has %d places, want the stale one dropped", n)
	}
}

// TestKeywordMapping: the system flags that have a keyword, the ones that
// do not, and an IMAP keyword keeping its name in lower case.
func TestKeywordMapping(t *testing.T) {
	cases := map[string]string{
		`\Seen`: "$seen", `\flagged`: "$flagged", `\Answered`: "$answered", `\Draft`: "$draft",
		"$Forwarded": "$forwarded", "Work": "work",
	}

	for flag, want := range cases {
		if got, ok := keywordOf(flag); !ok || got != want {
			t.Errorf("keywordOf(%q) = %q, %v; want %q", flag, got, ok, want)
		}
	}

	for _, flag := range []string{`\Deleted`, `\Recent`, "bad word", "a(b"} {
		if got, ok := keywordOf(flag); ok {
			t.Errorf("keywordOf(%q) = %q, want no keyword", flag, got)
		}
	}

	for keyword, want := range map[string]string{"$seen": `\Seen`, "$draft": `\Draft`, "work": "work"} {
		if got := flagOf(keyword); got != want {
			t.Errorf("flagOf(%q) = %q, want %q", keyword, got, want)
		}
	}
}

// TestSplitMultipart: the line break before a boundary belongs to the
// boundary, and the preamble and epilogue are dropped.
func TestSplitMultipart(t *testing.T) {
	body := "pre\r\n--b\r\nA\r\n--b\r\nB\r\n\r\n--b--\r\npost"

	parts := splitMultipart([]byte(body), "b")
	if len(parts) != 2 || string(parts[0]) != "A" || string(parts[1]) != "B\r\n" {
		t.Errorf("parts are %q, want A and B with its own trailing line", parts)
	}

	if got := splitMultipart([]byte(strings.ReplaceAll(body, "\r\n", "\n")), "b"); len(got) != 2 || string(got[0]) != "A" {
		t.Errorf("bare line feeds gave %q", got)
	}
}

// TestContentTypeWithABadParameter: a boundary with "=" in it and no
// quotes is refused by the strict parser but written by real mailers.
// The message keeps its structure, and its attachment stays one.
func TestContentTypeWithABadParameter(t *testing.T) {
	s, u := emailServer(t)
	raw := strings.ReplaceAll(alternativeMessage, `boundary="outer"`, `boundary=----=_Part_0_1.2`)
	raw = strings.ReplaceAll(raw, "--outer", "------=_Part_0_1.2")
	m := deliver(t, u, inboxName, raw)

	got := getEmail(t, s, m.GUID, `["attachments","hasAttachment"]`)
	if att, _ := got[propAttachments].([]any); len(att) != 1 || got[propHasAttachment] != true {
		t.Errorf("the attachment was lost: %#v", got)
	}
}

// TestTooManyPartsIsOneOpaquePart: a message beyond the limits of the
// walk is described as one part a client can download, and the other
// messages of the same call are not held back by it.
func TestTooManyPartsIsOneOpaquePart(t *testing.T) {
	s, u := emailServer(t)

	var b strings.Builder
	b.WriteString("Subject: many\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n")

	for i := 0; i <= maxParts; i++ {
		b.WriteString("--b\r\nContent-Type: text/plain\r\n\r\nx\r\n")
	}

	b.WriteString("--b--\r\n")

	big := deliver(t, u, inboxName, b.String())
	small := deliver(t, u, inboxName, plainMessage)

	res := mailCall(t, s, "Email/get", `{"accountId":"test","ids":["`+big.GUID+`","`+small.GUID+`"],
		"properties":["bodyStructure","attachments"]}`)

	list, _ := res["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("the call gave %d messages, want both", len(list))
	}

	root, _ := list[0].(map[string]any)[propBodyStructure].(map[string]any)
	if root[partType] != mediaOctetStream || root[partBlobID] == nil {
		t.Errorf("the big message is %#v, want one downloadable opaque part", root)
	}
}

// TestHeaderEndIsTheFirstEmptyLine: a header in bare LF followed by a
// body that contains a CRLF blank line ends at the LF one.
func TestHeaderEndIsTheFirstEmptyLine(t *testing.T) {
	raw := []byte("Subject: lf\nX: y\n\nbody\r\n\r\nmore")
	if got := headerEnd(raw); got != len("Subject: lf\nX: y\n\n") {
		t.Errorf("headerEnd is %d, want the end of the LF blank line", got)
	}
}

// TestMIMEEdges: a multipart without a close delimiter, transport
// padding after a boundary, a part without an empty line, and a
// multipart without a boundary.
func TestMIMEEdges(t *testing.T) {
	parts := splitMultipart([]byte("--b  \r\nA\r\n--b\t\r\nB"), "b")
	if len(parts) != 2 || string(parts[0]) != "A" || string(parts[1]) != "B" {
		t.Errorf("padding and a missing close gave %q", parts)
	}

	root, err := parseMessage([]byte("Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\njust text\r\n--b--\r\n"))
	if err != nil {
		t.Fatal(err)
	}

	if len(root.children) != 1 || string(root.children[0].body) != "just text" {
		t.Errorf("a part without an empty line is %#v, want its line as body", root.children)
	}

	noBoundary, err := parseMessage([]byte("Content-Type: multipart/mixed\r\n\r\ncontent\r\n"))
	if err != nil {
		t.Fatal(err)
	}

	if noBoundary.mediaType != mediaOctetStream || noBoundary.children != nil || len(noBoundary.body) == 0 {
		t.Errorf("a multipart without a boundary is %#v, want its content kept as one part", noBoundary)
	}
}

// TestBodyValueEncodingProblem: a part in a charset this server does not
// know, and bytes that are not UTF-8 in a UTF-8 part, both say so.
func TestBodyValueEncodingProblem(t *testing.T) {
	s, u := emailServer(t)

	for _, raw := range []string{
		"Content-Type: text/plain; charset=x-unknown\r\n\r\nabc\r\n",
		"Content-Type: text/plain; charset=utf-8\r\n\r\nab\xffc\r\n",
	} {
		m := deliver(t, u, inboxName, raw)

		got := getEmail(t, s, m.GUID, `["bodyValues"]`, `"fetchTextBodyValues":true`)
		values, _ := got[propBodyValues].(map[string]any)

		if v, _ := values["1"].(map[string]any); v["isEncodingProblem"] != true {
			t.Errorf("%q gave %#v, want isEncodingProblem", raw, v)
		}
	}
}

// TestEmailChangesAfterDeletingAnEmptyMailbox: deleting an empty mailbox
// is ordinary and must not make every later Email/changes answer
// cannotCalculateChanges.
func TestEmailChangesAfterDeletingAnEmptyMailbox(t *testing.T) {
	s, u := emailServer(t)
	state := currentState(t, s)

	if err := u.CreateMailbox("Empty", nil); err != nil {
		t.Fatal(err)
	}

	if err := u.DeleteMailbox("Empty"); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Email/changes", `{"accountId":"test","sinceState":"`+state+`"}`)
	if res["newState"] == state {
		t.Error("the state did not move past the deletion")
	}
}

// TestEmailChangesCopyAndPaging: an IMAP COPY is an update of the
// message, and a window cut by maxChanges says there is more and
// continues where it stopped.
func TestEmailChangesCopyAndPaging(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage)
	state := currentState(t, s)

	if _, err := u.Copy([]model.Meta{*m}, "Archive"); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Email/changes", `{"accountId":"test","sinceState":"`+state+`"}`)
	if updated, _ := res["updated"].([]any); len(updated) != 1 || updated[0] != m.GUID {
		t.Errorf("a COPY gave %#v, want the message updated", res)
	}

	if created, _ := res["created"].([]any); len(created) != 0 {
		t.Errorf("a COPY created %#v", created)
	}

	state = res["newState"].(string)
	a := deliver(t, u, inboxName, "Subject: a\r\n\r\nbody\r\n")
	b := deliver(t, u, inboxName, "Subject: b\r\n\r\nbody\r\n")

	first := mailCall(t, s, "Email/changes", `{"accountId":"test","sinceState":"`+state+`","maxChanges":1}`)
	if first["hasMoreChanges"] != true || len(first["created"].([]any)) != 1 {
		t.Fatalf("the first page is %#v", first)
	}

	second := mailCall(t, s, "Email/changes",
		`{"accountId":"test","sinceState":"`+first["newState"].(string)+`","maxChanges":1}`)

	seen := map[any]bool{first["created"].([]any)[0]: true}
	for _, id := range second["created"].([]any) {
		seen[id] = true
	}

	if !seen[a.GUID] || !seen[b.GUID] {
		t.Errorf("the two pages created %v, want both messages", seen)
	}
}

// TestThreadIgnoresAStaleMember: a thread lists only messages that are
// still there, the same answer Email/get gives.
func TestThreadIgnoresAStaleMember(t *testing.T) {
	s, u := emailServer(t)

	root := deliver(t, u, inboxName, "Message-ID: <r@example.org>\r\nSubject: t\r\n\r\nbody\r\n")
	reply := deliver(t, u, inboxName, "Message-ID: <s@example.org>\r\nReferences: <r@example.org>\r\nSubject: Re\r\n\r\nbody\r\n")
	thread := getEmail(t, s, root.GUID, `["threadId"]`)[propThreadID].(string)

	// The reply goes away behind the locator's back: its record keeps a
	// place that holds nothing.
	ix, _ := u.(model.Index)
	if err := ix.IndexApply(locSpace, []string{locEmail + reply.GUID}, func(_ []string, cur map[string][]byte) (map[string][]byte, []string) {
		rec := decodeEmailRec(cur[locEmail+reply.GUID])
		rec.Places = []place{{Mailbox: "nowhere", UID: 7}}

		return map[string][]byte{locEmail + reply.GUID: encode(&rec)}, nil
	}); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Thread/get", `{"accountId":"test","ids":["`+thread+`"]}`)

	ids, _ := res["list"].([]any)[0].(map[string]any)["emailIds"].([]any)
	if len(ids) != 1 || ids[0] != root.GUID {
		t.Errorf("the thread lists %#v, want only the message that is there", ids)
	}
}

// TestIndexRefusesAWriteOutsideItsShard: an index function may only
// write the keys of the shard it was handed.
func TestIndexRefusesAWriteOutsideItsShard(t *testing.T) {
	_, u := emailServer(t)
	ix, _ := u.(model.Index)

	err := ix.IndexApply("probe", []string{"a", "b", "c", "d", "e"}, func(_ []string, _ map[string][]byte) (map[string][]byte, []string) {
		return map[string][]byte{"a": {1}, "b": {1}, "c": {1}, "d": {1}, "e": {1}}, nil
	})
	if err != model.ErrIndexKey {
		t.Errorf("a write across shards gave %v, want ErrIndexKey", err)
	}
}
