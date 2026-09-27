package jmap

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"shardpost/internal/model"
)

// idList is a JSON list of ids.
func idList(list ...string) string { return `["` + strings.Join(list, `","`) + `"]` }

// found are the ids of a /get list.
func foundIDs(res map[string]any) []string {
	var out []string

	list, _ := res["list"].([]any)
	for _, item := range list {
		obj, _ := item.(map[string]any)
		id, _ := obj[propID].(string)
		out = append(out, id)
	}

	sort.Strings(out)

	return out
}

// list is a JSON list of strings from a response.
func strList(v any) []string {
	var out []string

	list, _ := v.([]any)
	for _, x := range list {
		s, _ := x.(string)
		out = append(out, s)
	}

	sort.Strings(out)

	return out
}

// TestSharingEmailGet: a message is an Email of a shared account only
// where the user may read it, and its mailboxIds are those places.
func TestSharingEmailGet(t *testing.T) {
	f := newSharing(t)

	team := deliver(t, f.alice, "Team", plainMessage)
	listed := deliver(t, f.alice, "Listed", "Subject: listed\r\n\r\nbody\r\n")
	secret := deliver(t, f.alice, "Secret", "Subject: secret\r\n\r\nbody\r\n")
	hidden := deliver(t, f.alice, "Hidden", "Subject: hidden\r\n\r\nbody\r\n")

	// The Team message has a second copy where test sees nothing.
	if _, err := f.alice.Copy([]model.Meta{*team}, "Hidden"); err != nil {
		t.Fatal(err)
	}

	res := f.mail(t, "Email/get", `{"accountId":"alice","ids":`+idList(team.GUID, listed.GUID, secret.GUID, hidden.GUID)+
		`,"properties":["mailboxIds","subject"]}`)

	if got := foundIDs(res); len(got) != 1 || got[0] != team.GUID {
		t.Fatalf("found %v, want the Team message alone: %#v", got, res)
	}

	if nf := strList(res["notFound"]); len(nf) != 3 {
		t.Errorf("notFound is %v", nf)
	}

	obj, _ := res["list"].([]any)[0].(map[string]any)
	if boxes, _ := obj[propMailboxIDs].(map[string]any); len(boxes) != 1 || boxes[f.id(t, "alice:Team")] != true {
		t.Errorf("mailboxIds %v name a mailbox test may not read", obj[propMailboxIDs])
	}

	// The personal account does not find a message of alice's.
	res = f.mail(t, "Email/get", `{"accountId":"test","ids":`+idList(team.GUID)+`}`)
	if len(foundIDs(res)) != 0 {
		t.Errorf("the personal account finds alice's message: %#v", res)
	}
}

// TestSharingEmailQuery: queries and full-text search see only readable
// mailboxes.
func TestSharingEmailQuery(t *testing.T) {
	f := newSharing(t)

	team := deliver(t, f.alice, "Team", "Subject: quarterly zebra\r\n\r\nbody\r\n")
	deliver(t, f.alice, "Listed", "Subject: quarterly zebra\r\n\r\nbody\r\n")
	deliver(t, f.alice, "Hidden", "Subject: quarterly zebra\r\n\r\nbody\r\n")
	deliver(t, f.alice, "Secret", "Subject: quarterly zebra\r\n\r\nbody\r\n")

	cases := []struct {
		name, filter string
		want         []string
	}{
		{"everything", `null`, []string{team.GUID}},
		{"text", `{"text":"zebra"}`, []string{team.GUID}},
		{"subject", `{"subject":"quarterly"}`, []string{team.GUID}},
		{"in a mailbox without r", `{"inMailbox":"` + f.id(t, "alice:Listed") + `"}`, nil},
		{"in an invisible mailbox", `{"inMailbox":"` + f.id(t, "alice:Hidden") + `"}`, nil},
		{"in Team", `{"inMailbox":"` + f.id(t, "alice:Team") + `"}`, []string{team.GUID}},
	}

	for _, c := range cases {
		res := f.mail(t, "Email/query", `{"accountId":"alice","filter":`+c.filter+`}`)
		if got := strList(res["ids"]); strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestSharingEmailSetRights: every write needs its right, a missing one
// is forbidden, and nothing is written.
func TestSharingEmailSetRights(t *testing.T) {
	cases := []struct {
		name  string
		box   string // where the message is
		patch string // "%s" is replaced by mailbox ids
		want  string // "" = updated
	}{
		{"$flagged without w", "Team", `{"keywords/$flagged":true}`, setForbidden},
		{"$seen without s", "Team", `{"keywords/$seen":true}`, setForbidden},
		{"move out without t and e", "Team", `{"mailboxIds":{"SUB":true}}`, setForbidden},
		{"copy into a mailbox with i", "Team", `{"mailboxIds/SUB":true}`, ""},
		{"copy into a mailbox without i", "Sub", `{"mailboxIds/TEAM":true}`, setForbidden},
		{"copy of a flagged message into a mailbox without w", "Sub", `{"mailboxIds/DROP":true,"keywords/$flagged":true}`, setForbidden},
		{"keywords with s and w", "Sub", `{"keywords":{"$seen":true,"$flagged":true}}`, ""},
		{"move with t, e and i", "Sub", `{"mailboxIds":{"DROP":true}}`, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newSharing(t)
			m := deliver(t, f.alice, "Team/Sub", plainMessage)

			if c.box == "Team" {
				m = deliver(t, f.alice, "Team", plainMessage)
			}

			patch := strings.NewReplacer("SUB", f.id(t, "alice:Team/Sub"), "TEAM", f.id(t, "alice:Team"),
				"DROP", f.id(t, "alice:Drop")).Replace(c.patch)

			before := flagsAndBoxes(t, f.alice, m.GUID)
			res := f.mail(t, "Email/set", `{"accountId":"alice","update":{"`+m.GUID+`":`+patch+`}}`)

			if got := setErrorType(res, "notUpdated", m.GUID); got != c.want {
				t.Errorf("got %q, want %q: %#v", got, c.want, res)
			}

			if c.want != "" && flagsAndBoxes(t, f.alice, m.GUID) != before {
				t.Errorf("a refused update changed the message: %s -> %s", before, flagsAndBoxes(t, f.alice, m.GUID))
			}
		})
	}
}

// flagsAndBoxes describes where a message is in the owner's namespace
// and with which flags.
func flagsAndBoxes(t *testing.T, u model.User, guid string) string {
	t.Helper()

	boxes, err := u.ListMailboxes()
	if err != nil {
		t.Fatal(err)
	}

	var out []string

	for _, mb := range boxes {
		msgs, _ := u.ListMessages(mb.ID)
		for _, m := range msgs {
			if m.GUID == guid {
				flags := append([]string(nil), m.Flags...)
				sort.Strings(flags)
				out = append(out, mb.Name+":"+strings.Join(flags, " "))
			}
		}
	}

	sort.Strings(out)

	return strings.Join(out, "; ")
}

// TestSharingEmailCreateAndDestroy: a creation needs i and the rights of
// its keywords; a destruction needs t and e and leaves the copies the
// user cannot see.
func TestSharingEmailCreateAndDestroy(t *testing.T) {
	f := newSharing(t)
	drop, team := f.id(t, "alice:Drop"), f.id(t, "alice:Team")

	create := func(boxes, keywords string) map[string]any {
		return f.mail(t, "Email/set", `{"accountId":"alice","create":{"c":{"mailboxIds":`+boxes+
			`,"keywords":`+keywords+`,"subject":"hi","bodyValues":{"b":{"value":"x"}},"textBody":[{"partId":"b","type":"text/plain"}]}}}`)
	}

	if typ := setErrorType(create(`{"`+drop+`":true}`, `{"$seen":true}`), "notCreated", "c"); typ != setForbidden {
		t.Errorf("$seen into a mailbox without s gave %q", typ)
	}

	if typ := setErrorType(create(`{"`+team+`":true}`, `{}`), "notCreated", "c"); typ != setForbidden {
		t.Errorf("a creation without i gave %q", typ)
	}

	if msgs, _ := f.alice.ListMessages(drop); len(msgs) != 0 {
		t.Fatalf("a refused creation stored %d messages", len(msgs))
	}

	if res := create(`{"`+drop+`":true}`, `{}`); res["created"].(map[string]any)["c"] == nil {
		t.Errorf("a creation into a mailbox with i failed: %#v", res)
	}

	// Destroying: Team has no t and e; Sub has both, and the copy in a
	// mailbox test cannot see stays.
	inTeam := deliver(t, f.alice, "Team", plainMessage)
	inSub := deliver(t, f.alice, "Team/Sub", "Subject: two\r\n\r\nbody\r\n")

	if _, err := f.alice.Copy([]model.Meta{*inSub}, "Hidden"); err != nil {
		t.Fatal(err)
	}

	res := f.mail(t, "Email/set", `{"accountId":"alice","destroy":`+idList(inTeam.GUID, inSub.GUID)+`}`)
	if typ := setErrorType(res, "notDestroyed", inTeam.GUID); typ != setForbidden {
		t.Errorf("destroying without t and e gave %q", typ)
	}

	if got := strList(res["destroyed"]); len(got) != 1 || got[0] != inSub.GUID {
		t.Errorf("destroyed %v", got)
	}

	if got := flagsAndBoxes(t, f.alice, inSub.GUID); !strings.HasPrefix(got, "Hidden:") || strings.Contains(got, "Sub") {
		t.Errorf("after the destruction the message is at %q, want the hidden copy alone", got)
	}
}

// TestSharingEmailChanges: the owner's log filtered to what the user may
// read, and a change of rights is cannotCalculateChanges.
func TestSharingEmailChanges(t *testing.T) {
	f := newSharing(t)

	state := f.mail(t, "Email/get", `{"accountId":"alice","ids":[]}`)["state"].(string)

	deliver(t, f.alice, "Listed", "Subject: listed\r\n\r\nbody\r\n")
	deliver(t, f.alice, "Hidden", "Subject: hidden\r\n\r\nbody\r\n")
	seen := deliver(t, f.alice, "Team", plainMessage)

	res := f.mail(t, "Email/changes", `{"accountId":"alice","sinceState":"`+state+`"}`)
	if got := strList(res["created"]); len(got) != 1 || got[0] != seen.GUID || len(strList(res["updated"])) != 0 {
		t.Errorf("changes: %#v", res)
	}

	q := f.mail(t, "Email/queryChanges", `{"accountId":"alice","sinceQueryState":"`+state+`"}`)
	if got := strList(q["removed"]); len(got) != 1 || got[0] != seen.GUID {
		t.Errorf("queryChanges names %v as changed, want the readable message alone", got)
	}

	next := res["newState"].(string)

	if err := f.alice.SetACL("Listed", "test", "lr"); err != nil {
		t.Fatal(err)
	}

	for _, m := range []string{"Email/changes", "Thread/changes"} {
		if typ := f.mailErr(t, m, `{"accountId":"alice","sinceState":"`+next+`"}`); typ != errCannotCalculateChanges {
			t.Errorf("%s across a grant gave %s", m, typ)
		}
	}

	if typ := f.mailErr(t, "Email/queryChanges", `{"accountId":"alice","sinceQueryState":"`+next+`"}`); typ != errCannotCalculateChanges {
		t.Errorf("Email/queryChanges across a grant gave %s", typ)
	}
}

// TestSharingThread: a thread lists only the messages the user can read.
func TestSharingThread(t *testing.T) {
	f := newSharing(t)

	first := deliver(t, f.alice, "Team", "Message-ID: <root@x>\r\nSubject: a\r\n\r\nbody\r\n")
	deliver(t, f.alice, "Hidden", "Message-ID: <reply@x>\r\nIn-Reply-To: <root@x>\r\nReferences: <root@x>\r\nSubject: b\r\n\r\nbody\r\n")

	got := getEmailIn(t, f, "alice", first.GUID, `["threadId"]`)

	res := f.mail(t, "Thread/get", `{"accountId":"alice","ids":["`+got["threadId"].(string)+`"]}`)

	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Thread/get: %#v", res)
	}

	if emails := strList(list[0].(map[string]any)["emailIds"]); len(emails) != 1 || emails[0] != first.GUID {
		t.Errorf("the thread lists %v", emails)
	}
}

// getEmailIn is Email/get of one message in an account.
func getEmailIn(t *testing.T, f *sharing, account, id, props string) map[string]any {
	t.Helper()

	res := f.mail(t, "Email/get", `{"accountId":"`+account+`","ids":["`+id+`"],"properties":`+props+`}`)

	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("Email/get of %s in %s: %#v", id, account, res)
	}

	obj, _ := list[0].(map[string]any)

	return obj
}

// TestSharingPublicPrivateSeen: in a public folder $seen is each
// reader's own, every other keyword is shared, and the unread count is
// the reader's.
func TestSharingPublicPrivateSeen(t *testing.T) {
	f := newSharing(t)

	if err := f.public.SetACL("Staff", "carol", "lrs"); err != nil {
		t.Fatal(err)
	}

	m := deliver(t, f.public, "Staff", plainMessage)

	res := f.mail(t, "Email/set", `{"accountId":"~public","update":{"`+m.GUID+`":{"keywords/$seen":true,"keywords/$flagged":true}}}`)
	if _, ok := res["updated"].(map[string]any)[m.GUID]; !ok {
		t.Fatalf("the update failed: %#v", res)
	}

	mine := keywordNames(getEmailIn(t, f, model.PublicNamespace, m.GUID, `["keywords"]`))
	if mine != "$flagged $seen" {
		t.Errorf("test sees %q", mine)
	}

	got, carol := callAs(t, f.s, "carol", `,"urn:ietf:params:jmap:mail"`, "Email/get",
		`{"accountId":"~public","ids":["`+m.GUID+`"],"properties":["keywords"]}`)
	if got != "Email/get" {
		t.Fatalf("carol: %s %#v", got, carol)
	}

	if kw := keywordNames(carol["list"].([]any)[0].(map[string]any)); kw != "$flagged" {
		t.Errorf("carol sees %q: test's $seen leaked or the shared $flagged is missing", kw)
	}

	if flags := flagsAndBoxes(t, f.public, m.GUID); strings.Contains(flags, `\Seen`) {
		t.Errorf("the shared record carries \\Seen: %s", flags)
	}

	boxes := objects(t, f.mail(t, "Mailbox/get", `{"accountId":"~public","properties":["name","unreadEmails"]}`))
	if boxes["Staff"][propUnreadEmails] != float64(0) {
		t.Errorf("test's unread count of Staff is %v", boxes["Staff"][propUnreadEmails])
	}

	_, cb := callAs(t, f.s, "carol", `,"urn:ietf:params:jmap:mail"`, "Mailbox/get",
		`{"accountId":"~public","properties":["name","unreadEmails"]}`)
	if objects(t, cb)["Staff"][propUnreadEmails] != float64(1) {
		t.Errorf("carol's unread count of Staff is %v", objects(t, cb)["Staff"][propUnreadEmails])
	}

	// test's query by $seen sees its own mark.
	q := f.mail(t, "Email/query", `{"accountId":"~public","filter":{"notKeyword":"$seen"}}`)
	if len(strList(q["ids"])) != 0 {
		t.Errorf("test's unseen query finds %v", q["ids"])
	}
}

// keywordNames are the keywords of an Email object, sorted and joined.
func keywordNames(obj map[string]any) string {
	kw, _ := obj[propKeywords].(map[string]any)

	var out []string
	for k := range kw {
		out = append(out, k)
	}

	sort.Strings(out)

	return strings.Join(out, " ")
}

// TestSharingBlobs: a download from a shared account serves a message
// the user can read, and nothing else of the owner's blob store; an
// upload into a shared account is refused; import and parse read only
// such blobs.
func TestSharingBlobs(t *testing.T) {
	f := newSharing(t)

	readable := deliver(t, f.alice, "Team", plainMessage)
	listed := deliver(t, f.alice, "Listed", plainMessage)
	hidden := deliver(t, f.alice, "Hidden", plainMessage)

	cases := []struct {
		path string
		want int
	}{
		{"alice/" + readable.GUID + "/m.eml", http.StatusOK},
		{"alice/" + readable.GUID + "_1/part", http.StatusOK},
		{"alice/" + listed.GUID + "/m.eml", http.StatusNotFound},
		{"alice/" + hidden.GUID + "/m.eml", http.StatusNotFound},
		{"alice/" + hidden.GUID + "_1/part", http.StatusNotFound},
		{"bob/" + readable.GUID + "/m.eml", http.StatusNotFound},
		{"test/" + readable.GUID + "/m.eml", http.StatusNotFound},
	}

	for _, c := range cases {
		if w := do(t, f.s, http.MethodGet, PathDownload+c.path, ""); w.Code != c.want {
			t.Errorf("GET %s: %d, want %d", c.path, w.Code, c.want)
		}
	}

	r := httptest.NewRequest(http.MethodPost, PathUpload+"alice/", strings.NewReader("data"))
	r.SetBasicAuth("test", "secret")

	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Errorf("an upload into alice's account answered %d", w.Code)
	}

	drop := f.id(t, "alice:Drop")

	res := f.mail(t, "Email/import", `{"accountId":"alice","emails":{"h":{"blobId":"`+hidden.GUID+
		`","mailboxIds":{"`+drop+`":true}},"r":{"blobId":"`+readable.GUID+`","mailboxIds":{"`+drop+`":true}}}}`)
	if typ := setErrorType(res, "notCreated", "h"); typ != setInvalidProperties {
		t.Errorf("importing an unreadable message gave %q", typ)
	}

	if res["created"].(map[string]any)["r"] == nil {
		t.Errorf("importing a readable message failed: %#v", res)
	}

	res = f.mail(t, "Email/parse", `{"accountId":"alice","blobIds":`+idList(hidden.GUID, readable.GUID)+`}`)
	if nf := strList(res["notFound"]); len(nf) != 1 || nf[0] != hidden.GUID {
		t.Errorf("Email/parse notFound %v", nf)
	}
}

// copyCall runs Email/copy as test and returns its response and the
// implicit Email/set that follows it, if any.
func (f *sharing) copyCall(t *testing.T, args string) (copied, implicit map[string]any) {
	t.Helper()

	body := `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[["Email/copy",` + args + `,"c1"]]}`

	r := httptest.NewRequest(http.MethodPost, PathAPI, strings.NewReader(body))
	r.SetBasicAuth("test", "secret")

	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, r)

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	list := responses(t, out)

	first, _ := list[0].([]any)
	if first[0] != "Email/copy" {
		t.Fatalf("Email/copy answered %v: %#v", first[0], first[1])
	}

	copied, _ = first[1].(map[string]any)
	checkAccountID(t, "Email/copy", args, copied)

	if len(list) > 1 {
		second, _ := list[1].([]any)
		if second[0] != methodEmailSet || second[2] != "c1" {
			t.Fatalf("the implicit call is %v under %v", second[0], second[2])
		}

		implicit, _ = second[1].(map[string]any)
	}

	return copied, implicit
}

// createdEmailID is the id a creation got.
func createdEmailID(res map[string]any, cid string) string {
	created, _ := res["created"].(map[string]any)
	obj, _ := created[cid].(map[string]any)
	id, _ := obj[propID].(string)

	return id
}

// TestSharingEmailCopy: copies between the personal account and shared
// ones in both directions, with the rights of both sides, and the
// destruction of the original under the source's rights.
func TestSharingEmailCopy(t *testing.T) {
	f := newSharing(t)
	mine, _ := f.test.Lookup("Mine")
	drop, staff := f.id(t, "alice:Drop"), f.id(t, "~public:Staff")

	plain := deliver(t, f.test, "Mine", plainMessage)
	seen := deliver(t, f.test, "Mine", "Subject: read\r\n\r\nbody\r\n", `\Seen`)

	// Personal to shared: a new message with an id of its own; $seen
	// needs s in the destination.
	res, _ := f.copyCall(t, `{"fromAccountId":"test","accountId":"alice","create":{"a":{"id":"`+plain.GUID+
		`","mailboxIds":{"`+drop+`":true}},"b":{"id":"`+seen.GUID+`","mailboxIds":{"`+drop+`":true}}}}`)

	if id := createdEmailID(res, "a"); id == "" || id == plain.GUID {
		t.Errorf("the copy into alice's Drop: %#v", res)
	}

	if typ := setErrorType(res, "notCreated", "b"); typ != setForbidden {
		t.Errorf("copying a read message into a mailbox without s gave %q", typ)
	}

	if msgs, _ := f.alice.ListMessages(drop); len(msgs) != 1 {
		t.Errorf("alice's Drop holds %d messages, want 1", len(msgs))
	}

	// Shared to personal, destroying the original: Team has no t and e,
	// so the copy is made and the destruction refused.
	team := deliver(t, f.alice, "Team", plainMessage)

	res, implicit := f.copyCall(t, `{"fromAccountId":"alice","accountId":"test","onSuccessDestroyOriginal":true,`+
		`"create":{"c":{"id":"`+team.GUID+`","mailboxIds":{"`+mine.ID+`":true}}}}`)
	if createdEmailID(res, "c") == "" || setErrorType(implicit, "notDestroyed", team.GUID) != setForbidden {
		t.Errorf("copy out of Team: %#v, then %#v", res, implicit)
	}

	// Out of Sub, which has t and e: the original goes.
	sub := deliver(t, f.alice, "Team/Sub", plainMessage)

	_, implicit = f.copyCall(t, `{"fromAccountId":"alice","accountId":"test","onSuccessDestroyOriginal":true,`+
		`"create":{"c":{"id":"`+sub.GUID+`","mailboxIds":{"`+mine.ID+`":true}}}}`)
	if got := strList(implicit["destroyed"]); len(got) != 1 || flagsAndBoxes(t, f.alice, sub.GUID) != "" {
		t.Errorf("the original in Sub was not destroyed: %#v", implicit)
	}

	// Shared to shared.
	res, _ = f.copyCall(t, `{"fromAccountId":"alice","accountId":"~public","create":{"d":{"id":"`+team.GUID+
		`","mailboxIds":{"`+staff+`":true}}}}`)
	if createdEmailID(res, "d") == "" {
		t.Errorf("copy from alice to the public folders: %#v", res)
	}
}

// TestSharingEmailCopyRefuses: a message the user may not read is not
// found, an account it cannot reach is not found on either side, and a
// copy within one account is a malformed call.
func TestSharingEmailCopyRefuses(t *testing.T) {
	f := newSharing(t)
	mine, _ := f.test.Lookup("Mine")

	hidden := deliver(t, f.alice, "Hidden", plainMessage)
	listed := deliver(t, f.alice, "Listed", plainMessage)

	res, _ := f.copyCall(t, `{"fromAccountId":"alice","accountId":"test","create":{"h":{"id":"`+hidden.GUID+
		`","mailboxIds":{"`+mine.ID+`":true}},"l":{"id":"`+listed.GUID+`","mailboxIds":{"`+mine.ID+`":true}}}}`)
	if setErrorType(res, "notCreated", "h") != setNotFound || setErrorType(res, "notCreated", "l") != setNotFound {
		t.Errorf("copying unreadable messages: %#v", res)
	}

	cases := []struct{ args, want string }{
		{`{"fromAccountId":"bob","accountId":"test","create":{}}`, errFromAccountNotFound},
		{`{"fromAccountId":"test","accountId":"bob","create":{}}`, errAccountNotFound},
		{`{"fromAccountId":"alice","accountId":"alice","create":{}}`, errInvalidArguments},
	}

	for _, c := range cases {
		if typ := f.mailErr(t, "Email/copy", c.args); typ != c.want {
			t.Errorf("%s gave %s, want %s", c.args, typ, c.want)
		}
	}
}
