package jmap

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// upload stores a blob and returns its id.
func upload(t *testing.T, s *Server, body, mediaType string) string {
	t.Helper()

	r := httptest.NewRequest(http.MethodPost, PathUpload+"test/", strings.NewReader(body))
	r.SetBasicAuth("test", "secret")
	r.Header.Set("Content-Type", mediaType)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	var up uploadResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &up) != nil {
		t.Fatalf("the upload answered %d: %s", w.Code, w.Body.String())
	}

	return up.BlobID
}

// flagsIn are the flags of every copy of a message in a mailbox.
func flagsIn(t *testing.T, u model.User, mailbox, guid string) [][]string {
	t.Helper()

	mb, err := u.Lookup(mailbox)
	if err != nil {
		t.Fatal(err)
	}

	metas, err := u.ListMessages(mb.ID)
	if err != nil {
		t.Fatal(err)
	}

	var out [][]string

	for _, m := range metas {
		if m.GUID == guid {
			out = append(out, m.Flags)
		}
	}

	return out
}

// setErrorType is the error type of one entry of a notX map.
func setErrorType(res map[string]any, key, id string) string {
	m, _ := res[key].(map[string]any)
	e, _ := m[id].(map[string]any)
	typ, _ := e["type"].(string)

	return typ
}

// TestEmailSetKeywords: a keyword change reaches every copy, and the
// flags that are not keywords stay.
func TestEmailSetKeywords(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage, `\Seen`, `\Deleted`)

	if _, err := u.Copy([]model.Meta{*m}, "Archive"); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+m.GUID+
		`":{"keywords/$flagged":true,"keywords/$seen":null}}}`)
	if _, ok := res["updated"].(map[string]any)[m.GUID]; !ok {
		t.Fatalf("the update failed: %#v", res)
	}

	for _, box := range []string{inboxName, "Archive"} {
		for _, flags := range flagsIn(t, u, box, m.GUID) {
			if !slices.Contains(flags, `\Flagged`) || slices.Contains(flags, `\Seen`) {
				t.Errorf("%s has flags %v", box, flags)
			}

			if box == inboxName && !slices.Contains(flags, `\Deleted`) {
				t.Errorf("the update dropped \\Deleted: %v", flags)
			}
		}
	}

	mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+m.GUID+`":{"keywords":{"$Answered":true,"custom":true}}}}`)

	got := getEmail(t, s, m.GUID, `["keywords"]`)[propKeywords].(map[string]any)
	if len(got) != 2 || got["$answered"] != true || got["custom"] != true {
		t.Errorf("keywords are %#v after replacing them", got)
	}
}

// TestEmailSetMailboxes: moving, adding and refusing.
func TestEmailSetMailboxes(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage)
	boxes := boxIDs(t, u)
	inbox, archive := boxes[inboxName], boxes["Archive"]

	mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+m.GUID+
		`":{"mailboxIds/`+inbox+`":null,"mailboxIds/`+archive+`":true}}}`)

	got := getEmail(t, s, m.GUID, `["mailboxIds"]`)[propMailboxIDs].(map[string]any)
	if len(got) != 1 || got[archive] != true {
		t.Fatalf("after the move mailboxIds is %#v", got)
	}

	mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+m.GUID+
		`":{"mailboxIds":{"`+inbox+`":true,"`+archive+`":true}}}}`)

	if got := getEmail(t, s, m.GUID, `["mailboxIds"]`)[propMailboxIDs].(map[string]any); len(got) != 2 {
		t.Fatalf("after adding the inbox mailboxIds is %#v", got)
	}

	cases := []struct{ patch, want string }{
		{`{"mailboxIds":{}}`, setInvalidProperties},
		{`{"mailboxIds/nope":true}`, setInvalidProperties},
		{`{"mailboxIds":{"` + inbox + `":false}}`, setInvalidProperties},
		{`{"keywords":{},"keywords/$seen":true}`, setInvalidPatch},
		{`{"subject":"new"}`, setInvalidProperties},
		{`{"keywords/a(b":true}`, setInvalidProperties},
	}

	for _, c := range cases {
		res := mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+m.GUID+`":`+c.patch+`}}`)
		if got := setErrorType(res, "notUpdated", m.GUID); got != c.want {
			t.Errorf("%s gave %q, want %s", c.patch, got, c.want)
		}
	}

	res := mailCall(t, s, "Email/set", `{"accountId":"test","update":{"nope":{"keywords":{}}}}`)
	if got := setErrorType(res, "notUpdated", "nope"); got != setNotFound {
		t.Errorf("an unknown id gave %q", got)
	}
}

// TestEmailSetDestroy: every copy goes.
func TestEmailSetDestroy(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage)

	if _, err := u.Copy([]model.Meta{*m}, "Archive"); err != nil {
		t.Fatal(err)
	}

	state := currentState(t, s)

	res := mailCall(t, s, "Email/set", `{"accountId":"test","destroy":["`+m.GUID+`","nope"]}`)
	if d, _ := res["destroyed"].([]any); len(d) != 1 || d[0] != m.GUID {
		t.Fatalf("destroyed is %#v", res["destroyed"])
	}

	if got := setErrorType(res, "notDestroyed", "nope"); got != setNotFound {
		t.Errorf("an unknown id gave %q", got)
	}

	for _, box := range []string{inboxName, "Archive"} {
		if left := flagsIn(t, u, box, m.GUID); len(left) != 0 {
			t.Errorf("%s still has the message", box)
		}
	}

	if res["newState"] == state {
		t.Error("the state did not move after a destruction")
	}

	changes := mailCall(t, s, "Email/changes", `{"accountId":"test","sinceState":"`+res["newState"].(string)+`"}`)
	if d, _ := changes["destroyed"].([]any); len(d) != 0 {
		t.Errorf("the new state still reports the call's own destruction: %#v", changes)
	}

	if got := mailError(t, s, "Email/set", `{"accountId":"test","ifInState":"`+state+`","destroy":[]}`); got != errStateMismatch {
		t.Errorf("a stale ifInState gave %s", got)
	}
}

// TestEmailSetCreate: a message composed from properties comes back as
// it was described.
func TestEmailSetCreate(t *testing.T) {
	s, u := emailServer(t)
	boxes := boxIDs(t, u)
	blob := upload(t, s, "PNG\x00\x01binary", "image/png")

	res := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/set",{"accountId":"test","create":{"c1":{
			"mailboxIds":{"`+boxes[inboxName]+`":true,"`+boxes["Archive"]+`":true},
			"keywords":{"$draft":true},
			"from":[{"name":"Jürgen Müller","email":"juergen@example.org"}],
			"to":[{"name":null,"email":"bob@example.org"}],
			"subject":"Grüße aus Köln, with a subject long enough to be folded somewhere along the way",
			"header:X-Mailer:asText":"test",
			"textBody":[{"partId":"t"}],
			"htmlBody":[{"partId":"h"}],
			"attachments":[{"blobId":"`+blob+`","type":"image/png","name":"bild ä.png"}],
			"bodyValues":{"t":{"value":"Hallo Bob,\nschöne Grüße"},"h":{"value":"<p>Hallo</p>"}}
		}}},"s1"],
		["Email/set",{"accountId":"test","update":{"#c1":{"keywords/$seen":true}}},"s2"]
	]}`)

	rs := responses(t, res)
	first, _ := rs[0].([]any)[1].(map[string]any)
	created, _ := first["created"].(map[string]any)["c1"].(map[string]any)

	id, _ := created[propID].(string)
	if id == "" || created[propBlobID] != id || created[propThreadID] == nil || created[propSize] == nil {
		t.Fatalf("created is %#v (notCreated %#v)", created, first["notCreated"])
	}

	if second, _ := rs[1].([]any)[1].(map[string]any); second["updated"].(map[string]any)[id] == nil &&
		len(second["notUpdated"].(map[string]any)) > 0 {
		t.Errorf("the update by creation id failed: %#v", second)
	}

	got := getEmail(t, s, id, `["subject","from","keywords","mailboxIds","textBody","htmlBody","attachments","bodyValues","messageId","sentAt","header:X-Mailer:asText"]`,
		`"fetchAllBodyValues":true`)

	checkCreated(t, s, got)
}

// checkCreated checks the Email TestEmailSetCreate made.
func checkCreated(t *testing.T, s *Server, got map[string]any) {
	t.Helper()

	if got[propSubject] != "Grüße aus Köln, with a subject long enough to be folded somewhere along the way" {
		t.Errorf("subject is %#v", got[propSubject])
	}

	from, _ := got[propFrom].([]any)
	if len(from) != 1 || from[0].(map[string]any)["name"] != "Jürgen Müller" {
		t.Errorf("from is %#v", got[propFrom])
	}

	if kw := got[propKeywords].(map[string]any); kw["$draft"] != true || kw["$seen"] != true {
		t.Errorf("keywords are %#v", kw)
	}

	if len(got[propMailboxIDs].(map[string]any)) != 2 || got[propMessageID] == nil || got[propSentAt] == nil {
		t.Errorf("mailboxIds, messageId or sentAt is wrong: %#v", got)
	}

	if got["header:X-Mailer:asText"] != "test" {
		t.Errorf("X-Mailer is %#v", got["header:X-Mailer:asText"])
	}

	checkCreatedBody(t, s, got)
}

// checkCreatedBody checks the body of the Email TestEmailSetCreate made.
func checkCreatedBody(t *testing.T, s *Server, got map[string]any) {
	t.Helper()

	text := onePart(t, got, propTextBody, mediaTextPlain)
	values, _ := got[propBodyValues].(map[string]any)

	if v, _ := values[text["partId"].(string)].(map[string]any); v["value"] != "Hallo Bob,\r\nschöne Grüße" {
		t.Errorf("the text is %#v", v)
	}

	att := onePart(t, got, propAttachments, "image/png")
	if att["name"] != "bild ä.png" || att["disposition"] != "attachment" {
		t.Errorf("the attachment is %#v", att)
	}

	dl := do(t, s, http.MethodGet, PathDownload+"test/"+att[propBlobID].(string)+"/x", "")
	if dl.Body.String() != "PNG\x00\x01binary" {
		t.Errorf("the attachment downloads as %q", dl.Body.String())
	}
}

// TestEmailSetCreateRefuses: a creation that is ambiguous or incomplete
// is refused with the property that is wrong.
func TestEmailSetCreateRefuses(t *testing.T) {
	s, u := emailServer(t)
	inbox := boxIDs(t, u)[inboxName]
	box := `"mailboxIds":{"` + inbox + `":true}`

	cases := []struct{ create, want string }{
		{`{"subject":"x"}`, setInvalidProperties},
		{`{` + box + `,"from":[{"email":"a@example.org"}],"header:From":"b@example.org"}`, setInvalidProperties},
		{`{` + box + `,"header:Content-Type":"text/plain"}`, setInvalidProperties},
		{`{` + box + `,"headers":[]}`, setInvalidProperties},
		{`{` + box + `,"id":"x"}`, setInvalidProperties},
		{`{` + box + `,"textBody":[{"partId":"missing"}]}`, setInvalidProperties},
		{`{` + box + `,"textBody":[{"partId":"t","charset":"latin1"}],"bodyValues":{"t":{"value":"x"}}}`, setInvalidProperties},
		{`{` + box + `,"textBody":[{"partId":"t","type":"text/html"}],"bodyValues":{"t":{"value":"x"}}}`, setInvalidProperties},
		{`{` + box + `,"bodyStructure":{"partId":"t"},"textBody":[{"partId":"t"}],"bodyValues":{"t":{"value":"x"}}}`, setInvalidProperties},
		{`{` + box + `,"bodyValues":{"t":{"value":"x","isTruncated":true}},"textBody":[{"partId":"t"}]}`, setInvalidProperties},
		{`{` + box + `,"attachments":[{"blobId":"0123456789abcdef0123456789abcdef"}]}`, setBlobNotFound},
		{`{` + box + `,"bodyStructure":{"type":"multipart/mixed","partId":"t"},"bodyValues":{"t":{"value":"x"}}}`, setInvalidProperties},
	}

	for i, c := range cases {
		res := mailCall(t, s, "Email/set", `{"accountId":"test","create":{"c":`+c.create+`}}`)
		if got := setErrorType(res, "notCreated", "c"); got != c.want {
			t.Errorf("case %d (%s) gave %q, want %s", i, c.create, got, c.want)
		}
	}

	res := mailCall(t, s, "Email/set", `{"accountId":"test","create":{"c":{`+box+
		`,"attachments":[{"blobId":"0123456789abcdef0123456789abcdef"}]}}}`)
	nf, _ := res["notCreated"].(map[string]any)["c"].(map[string]any)["notFound"].([]any)

	if len(nf) != 1 || nf[0] != "0123456789abcdef0123456789abcdef" {
		t.Errorf("blobNotFound names %#v", nf)
	}
}

// TestEmailSetCreateBodyStructure: a client that describes the tree gets
// that tree.
func TestEmailSetCreateBodyStructure(t *testing.T) {
	s, u := emailServer(t)
	inbox := boxIDs(t, u)[inboxName]

	res := mailCall(t, s, "Email/set", `{"accountId":"test","create":{"c":{"mailboxIds":{"`+inbox+`":true},
		"bodyStructure":{"type":"multipart/alternative","subParts":[
			{"partId":"a","type":"text/plain"},{"partId":"b","type":"text/html","header:X-Part:asText":"two"}]},
		"bodyValues":{"a":{"value":"plain"},"b":{"value":"<b>html</b>"}}}}}`)

	c, _ := res["created"].(map[string]any)["c"].(map[string]any)
	if c == nil {
		t.Fatalf("the creation failed: %#v", res["notCreated"])
	}

	got := getEmail(t, s, c[propID].(string), `["bodyStructure"]`, `"bodyProperties":["type","header:X-Part:asText","subParts"]`)

	root, _ := got[propBodyStructure].(map[string]any)
	subs, _ := root["subParts"].([]any)

	if root["type"] != "multipart/alternative" || len(subs) != 2 || subs[1].(map[string]any)["header:X-Part:asText"] != "two" {
		t.Errorf("bodyStructure is %#v", root)
	}
}

// TestEmailImport: a blob becomes a message, in every mailbox named, with
// the arrival of its newest Received field.
func TestEmailImport(t *testing.T) {
	s, u := emailServer(t)
	boxes := boxIDs(t, u)
	blob := upload(t, s, "Received: from a by b; Mon, 21 Sep 2026 08:00:00 +0000\r\n"+plainMessage, "message/rfc822")
	junk := upload(t, s, "no header here at all", "application/octet-stream")

	res := mailCall(t, s, "Email/import", `{"accountId":"test","emails":{
		"a":{"blobId":"`+blob+`","mailboxIds":{"`+boxes[inboxName]+`":true,"`+boxes["Archive"]+`":true},"keywords":{"$seen":true}},
		"b":{"blobId":"`+junk+`","mailboxIds":{"`+boxes[inboxName]+`":true}},
		"c":{"blobId":"0123456789abcdef0123456789abcdef","mailboxIds":{"`+boxes[inboxName]+`":true}},
		"d":{"blobId":"`+blob+`","mailboxIds":{}}}}`)

	a, _ := res["created"].(map[string]any)["a"].(map[string]any)
	if a == nil {
		t.Fatalf("the import failed: %#v", res["notCreated"])
	}

	for cid, want := range map[string]string{"b": setInvalidEmail, "c": setInvalidProperties, "d": setInvalidProperties} {
		if got := setErrorType(res, "notCreated", cid); got != want {
			t.Errorf("%s gave %q, want %s", cid, got, want)
		}
	}

	got := getEmail(t, s, a[propID].(string), `["receivedAt","keywords","mailboxIds","subject"]`)
	if got[propReceivedAt] != "2026-09-21T08:00:00Z" || got[propKeywords].(map[string]any)["$seen"] != true ||
		len(got[propMailboxIDs].(map[string]any)) != 2 || got[propSubject] != "Grüße" {
		t.Errorf("the imported email is %#v", got)
	}
}

// TestEmailParse: a blob, and a message attached to one, rendered without
// being stored.
func TestEmailParse(t *testing.T) {
	s, _ := emailServer(t)
	outer := "Subject: outer\r\nMessage-ID: <outer@example.org>\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nsee attached\r\n" +
		"--b\r\nContent-Type: message/rfc822\r\n\r\n" +
		"Subject: inner\r\nContent-Type: multipart/mixed; boundary=c\r\n\r\n" +
		"--c\r\nContent-Type: text/plain\r\n\r\ninner text\r\n" +
		"--c\r\nContent-Type: application/octet-stream\r\n\r\nDATA\r\n--c--\r\n" +
		"--b--\r\n"
	blob := upload(t, s, outer, "message/rfc822")

	res := mailCall(t, s, "Email/parse", `{"accountId":"test","blobIds":["`+blob+`","0123456789abcdef0123456789abcdef"],
		"properties":["id","subject","threadId","attachments","mailboxIds"]}`)

	parsed, _ := res["parsed"].(map[string]any)[blob].(map[string]any)
	if parsed == nil || parsed[propSubject] != "outer" || parsed[propID] != nil || parsed[propMailboxIDs] != nil ||
		parsed[propThreadID] == nil {
		t.Fatalf("parsed is %#v", res)
	}

	if nf, _ := res["notFound"].([]any); len(nf) != 1 {
		t.Errorf("notFound is %#v", res["notFound"])
	}

	att := onePart(t, parsed, propAttachments, "message/rfc822")
	inner := att[propBlobID].(string)

	res = mailCall(t, s, "Email/parse", `{"accountId":"test","blobIds":["`+inner+`"],"properties":["subject","attachments","threadId"]}`)

	in, _ := res["parsed"].(map[string]any)[inner].(map[string]any)
	if in == nil || in[propSubject] != "inner" || in[propThreadID] != nil {
		t.Fatalf("the attached message parsed as %#v", res)
	}

	data := onePart(t, in, propAttachments, mediaOctetStream)

	dl := do(t, s, http.MethodGet, PathDownload+"test/"+data[propBlobID].(string)+"/x", "")
	if dl.Code != http.StatusOK || dl.Body.String() != "DATA" {
		t.Errorf("the nested part downloads as %d %q", dl.Code, dl.Body.String())
	}
}

// TestEmailCopy: there is no other account to copy from.
func TestEmailCopy(t *testing.T) {
	s, _ := emailServer(t)

	if got := mailError(t, s, "Email/copy", `{"fromAccountId":"other","accountId":"test","create":{}}`); got != errFromAccountNotFound {
		t.Errorf("another account gave %s", got)
	}

	if got := mailError(t, s, "Email/copy", `{"fromAccountId":"test","accountId":"test","create":{}}`); got != errInvalidArguments {
		t.Errorf("the same account gave %s", got)
	}
}

// TestEmailSetRefusesHeaderInjection: a header property whose name is
// not a field name never reaches the message.
func TestEmailSetRefusesHeaderInjection(t *testing.T) {
	s, u := emailServer(t)
	box := `"mailboxIds":{"` + boxIDs(t, u)[inboxName] + `":true}`

	for _, prop := range []string{`header:X\r\nBcc`, `header:From `, `header:MIME-Version`} {
		res := mailCall(t, s, "Email/set", `{"accountId":"test","create":{"c":{`+box+`,"`+prop+`":"x"}}}`)
		if got := setErrorType(res, "notCreated", "c"); got != setInvalidProperties {
			t.Errorf("%q gave %q", prop, got)
		}
	}

	res := mailCall(t, s, "Email/set", `{"accountId":"test","create":{"c":{`+box+`,"subject":"a",
		"bodyStructure":{"partId":"t","header:Subject":"b"},"bodyValues":{"t":{"value":"x"}}}}}`)
	if got := setErrorType(res, "notCreated", "c"); got != setInvalidProperties {
		t.Errorf("a subject on the email and on the root part gave %q", got)
	}

	if got := mailError(t, s, "Email/get", `{"accountId":"test","ids":[],"properties":["header:X Y"]}`); got != errInvalidArguments {
		t.Errorf("Email/get of a bad field name gave %s", got)
	}
}

// TestEmailSetCreateSizeIsBoundedEarly: referencing a blob many times is
// refused as too large before the message is put together.
func TestEmailSetCreateSizeIsBoundedEarly(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.CreateMailbox(inboxName, nil); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, func(o *Options) { o.Store = store; o.Limits.MaxSizeUpload = 1000 })
	blob := upload(t, s, strings.Repeat("x", 400), "application/octet-stream")

	parts := strings.TrimSuffix(strings.Repeat(`{"blobId":"`+blob+`"},`, 3), ",")
	res := mailCall(t, s, "Email/set", `{"accountId":"test","create":{"c":{"mailboxIds":{"`+
		boxIDs(t, u)[inboxName]+`":true},"attachments":[`+parts+`]}}}`)

	if got := setErrorType(res, "notCreated", "c"); got != setTooLarge {
		t.Errorf("three references to a 400-byte blob under a 1000-byte limit gave %q", got)
	}
}

// TestEmailSetKeepsKeywordsWhenACopyGoes: under the union rule, removing
// the one copy that was read must not make the message unread.
func TestEmailSetKeepsKeywordsWhenACopyGoes(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage, `\Seen`)

	if _, err := u.Copy([]model.Meta{*m}, "Archive"); err != nil {
		t.Fatal(err)
	}

	if _, err := u.SetFlags("Archive", []uint32{1}, 0, func([]string) []string { return nil }); err != nil {
		t.Fatal(err)
	}

	res := mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+m.GUID+
		`":{"mailboxIds/`+boxIDs(t, u)[inboxName]+`":null}}}`)
	if _, ok := res["updated"].(map[string]any)[m.GUID]; !ok {
		t.Fatalf("the update failed: %#v", res)
	}

	if kw := getEmail(t, s, m.GUID, `["keywords"]`)[propKeywords].(map[string]any); kw["$seen"] != true {
		t.Errorf("keywords after removing the read copy: %#v", kw)
	}
}

// TestEmailSetReportsAForeignChange: a change another session makes to
// the same message between the write and the new state is reported in
// updated, so the client does not lose it by adopting the new state.
func TestEmailSetReportsAForeignChange(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage)

	f := &found{metas: []model.Meta{*m}, boxes: map[string]bool{boxIDs(t, u)[inboxName]: true}}
	exp := expectation{keywords: map[string]bool{"$seen": true}, boxes: f.boxes}

	if _, err := u.SetFlags(inboxName, []uint32{m.UID}, 0, func([]string) []string { return []string{`\Seen`} }); err != nil {
		t.Fatal(err)
	}

	before := currentState(t, s)

	// The foreign change: IMAP flags the message after the call's write.
	if _, err := u.SetFlags(inboxName, []uint32{m.UID}, 0,
		func(old []string) []string { return append(old, `\Flagged`) }); err != nil {
		t.Fatal(err)
	}

	sess := &session{account: "test", user: u, created: map[string]string{}, limits: DefaultLimits}
	updated := map[string]any{m.GUID: nil}
	after := reconcile(sess, before, map[string]bool{m.GUID: true}, nil, updated, nil,
		map[string]expectation{m.GUID: exp})

	diff, _ := updated[m.GUID].(map[string]any)
	if after == before || diff == nil || !keywordSetHas(diff[propKeywords], "$flagged") {
		t.Errorf("state %s -> %s, updated %#v: the foreign \\Flagged is not reported", before, after, updated)
	}
}

// keywordSetHas reports whether a keywords value has the keyword.
func keywordSetHas(v any, k string) bool {
	set, _ := v.(map[string]bool)

	return set[k]
}

// TestEmailSetPatchErrors: malformed paths are invalidPatch, a server-set
// property sent back unchanged is accepted.
func TestEmailSetPatchErrors(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, plainMessage)

	for _, patch := range []string{`{"keywords/":true}`, `{"mailboxIds/a/b":true}`, `{"keywords/$seen":false}`} {
		res := mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+m.GUID+`":`+patch+`}}`)
		if got := setErrorType(res, "notUpdated", m.GUID); got != setInvalidPatch {
			t.Errorf("%s gave %q", patch, got)
		}
	}

	res := mailCall(t, s, "Email/set", `{"accountId":"test","update":{"`+m.GUID+`":{"id":"`+m.GUID+
		`","size":`+strconv.FormatInt(m.Size, 10)+`}}}`)
	if _, ok := res["updated"].(map[string]any)[m.GUID]; !ok {
		t.Errorf("sending back id and size unchanged failed: %#v", res["notUpdated"])
	}
}
