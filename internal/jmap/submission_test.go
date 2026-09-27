package jmap

import (
	"context"
	"strings"
	"sync"
	"testing"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/outbound"
)

// fakeRelay records what it is handed and answers with a report.
type fakeRelay struct {
	mu     sync.Mutex
	as     *outbound.XClient
	from   string
	to     []string
	raw    string
	report *outbound.Report
	err    error
	calls  int
}

func (f *fakeRelay) Submit(_ context.Context, as *outbound.XClient, from string, to []string, raw []byte) (*outbound.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.as, f.from, f.to, f.raw = as, from, to, string(raw)

	if f.err != nil {
		return nil, f.err
	}

	if f.report != nil {
		return f.report, nil
	}

	return &outbound.Report{Final: outbound.Reply{Code: 250, Message: "2.0.0 queued"}}, nil
}

// submissionServer is a front-end that can send, over a fake relay.
func submissionServer(t *testing.T) (*Server, model.User, *fakeRelay) {
	t.Helper()

	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []string{inboxName, "Drafts", "Sent"} {
		if err := u.CreateMailbox(n, nil); err != nil {
			t.Fatal(err)
		}
	}

	relay := &fakeRelay{}
	s := newTestServer(t, func(o *Options) {
		o.Store = store
		o.Auth = identityAuth{Static: auth.Static{"test": "secret"},
			attrs: map[string][]string{"mail": {"test@example.org"}}}
		o.Submission = &SubmissionOptions{Relay: relay, KV: memstore.NewAdminKV(), XClient: true, XClientAddr: true, CheckSender: true,
			MaxRecipients: 4}
	})

	return s, u, relay
}

// draftMessage is a draft with a Bcc.
const draftMessage = "From: Test <test@example.org>\r\nTo: bob@example.net\r\nCc: carol@example.net\r\n" +
	"Bcc: dave@example.net,\r\n eve@example.net\r\nSubject: hello\r\nMessage-ID: <d1@example.org>\r\n\r\nbody\r\n"

// identityID is the id of the account's identity.
func identityID(t *testing.T, s *Server) string {
	t.Helper()

	res := submissionCall(t, s, "Identity/get", `{"accountId":"test"}`)

	return res["list"].([]any)[0].(map[string]any)[propID].(string)
}

// TestEmailSubmission: the message goes to the relay for the user,
// without its Bcc field, to every recipient of the header; the record
// can be read back; onSuccessUpdateEmail moves the draft.
func TestEmailSubmission(t *testing.T) {
	s, u, relay := submissionServer(t)
	m := deliver(t, u, "Drafts", draftMessage, `\Draft`)
	boxes := boxIDs(t, u)
	ident := identityID(t, s)

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],
		"methodCalls":[["EmailSubmission/set",{"accountId":"test","create":{"k":{"identityId":"`+ident+`","emailId":"`+m.GUID+`"}},
		"onSuccessUpdateEmail":{"#k":{"mailboxIds/`+boxes["Drafts"]+`":null,"mailboxIds/`+boxes["Sent"]+`":true,"keywords/$draft":null}}},"c1"]]}`)

	rs := responses(t, out)
	if len(rs) != 2 {
		t.Fatalf("responses %#v", rs)
	}

	first, second := rs[0].([]any), rs[1].([]any)
	if first[0] != "EmailSubmission/set" || second[0] != "Email/set" || second[2] != "c1" {
		t.Fatalf("responses %#v", rs)
	}

	created, _ := first[1].(map[string]any)["created"].(map[string]any)["k"].(map[string]any)
	if created == nil {
		t.Fatalf("not created: %#v", first[1])
	}

	checkRelayCall(t, relay)

	got := getEmail(t, s, m.GUID, `["mailboxIds","keywords"]`)
	if mb := got[propMailboxIDs].(map[string]any); len(mb) != 1 || mb[boxes["Sent"]] != true {
		t.Errorf("after sending mailboxIds is %#v", mb)
	}

	checkSubmissionRecord(t, s, created[propID].(string), m.GUID)
}

// checkRelayCall checks what the relay was handed for the draft.
func checkRelayCall(t *testing.T, relay *fakeRelay) {
	t.Helper()

	if relay.as == nil || relay.as.Login != "test" || relay.as.Addr != "192.0.2.1" || relay.from != "test@example.org" ||
		strings.Join(relay.to, ",") != "bob@example.net,carol@example.net,dave@example.net,eve@example.net" {
		t.Errorf("relay got as=%+v from=%q to=%v", relay.as, relay.from, relay.to)
	}

	if strings.Contains(relay.raw, "Bcc") || strings.Contains(relay.raw, "eve@") || !strings.Contains(relay.raw, "Subject: hello") {
		t.Errorf("the relay got\n%s", relay.raw)
	}
}

// checkSubmissionRecord reads a submission back through get, changes
// and query.
func checkSubmissionRecord(t *testing.T, s *Server, id, emailID string) {
	t.Helper()

	res := submissionCall(t, s, "EmailSubmission/get", `{"accountId":"test","ids":["`+id+`"]}`)

	obj, _ := res["list"].([]any)[0].(map[string]any)
	if obj[propEmailID] != emailID || obj[propUndoStatus] != undoFinal || obj[propDeliveryStatus] == nil {
		t.Errorf("get gave %#v", obj)
	}

	ch := submissionCall(t, s, "EmailSubmission/changes", `{"accountId":"test","sinceState":"0"}`)
	if c, _ := ch["created"].([]any); len(c) != 1 || c[0] != id {
		t.Errorf("changes gave %#v", ch)
	}

	q := submissionCall(t, s, "EmailSubmission/query", `{"accountId":"test","filter":{"emailIds":["`+emailID+`"]},"sort":[{"property":"sentAt"}]}`)
	if ids, _ := q["ids"].([]any); len(ids) != 1 || ids[0] != id {
		t.Errorf("query gave %#v", q)
	}

	upd := submissionCall(t, s, "EmailSubmission/set", `{"accountId":"test","update":{"`+id+`":{"undoStatus":"canceled"}}}`)
	if got := setErrorType(upd, "notUpdated", id); got != setCannotUnsend {
		t.Errorf("cancelling gave %q", got)
	}
}

// TestEmailSubmissionRefuses: what is refused before the relay is asked,
// and how the relay's refusals come back.
func TestEmailSubmissionRefuses(t *testing.T) {
	s, u, relay := submissionServer(t)
	ident := identityID(t, s)
	good := deliver(t, u, "Drafts", draftMessage)
	foreign := deliver(t, u, "Drafts", strings.Replace(draftMessage, "test@example.org", "boss@example.org", 1))
	lonely := deliver(t, u, "Drafts", "From: test@example.org\r\nSubject: nobody\r\n\r\nx\r\n")
	crowded := deliver(t, u, "Drafts", "From: test@example.org\r\nTo: a@x.net, b@x.net, c@x.net, d@x.net, e@x.net\r\n\r\nx\r\n")

	create := func(body string) map[string]any {
		return submissionCall(t, s, "EmailSubmission/set", `{"accountId":"test","create":{"k":`+body+`}}`)
	}

	cases := []struct{ body, want string }{
		{`{"identityId":"nope","emailId":"` + good.GUID + `"}`, setInvalidProperties},
		{`{"identityId":"` + ident + `","emailId":"nope"}`, setInvalidProperties},
		{`{"identityId":"` + ident + `","emailId":"` + foreign.GUID + `"}`, setForbiddenFrom},
		{`{"identityId":"` + ident + `","emailId":"` + lonely.GUID + `"}`, setNoRecipients},
		{`{"identityId":"` + ident + `","emailId":"` + crowded.GUID + `"}`, setTooManyRecipients},
		{`{"identityId":"` + ident + `","emailId":"` + good.GUID + `","envelope":{"mailFrom":{"email":"boss@example.org"},"rcptTo":[{"email":"a@example.net"}]}}`, setForbiddenMailFrom},
		{`{"identityId":"` + ident + `","emailId":"` + good.GUID + `","envelope":{"mailFrom":{"email":"Test <test@example.org>"},"rcptTo":[{"email":"a@example.net"}]}}`, setForbiddenMailFrom},
		{`{"identityId":"` + ident + `","emailId":"` + good.GUID + `","envelope":{"mailFrom":{"email":"test@example.org","parameters":{"HOLDFOR":"60"}},"rcptTo":[{"email":"a@example.net"}]}}`, setInvalidProperties},
		{`{"identityId":"` + ident + `","emailId":"` + good.GUID + `","envelope":{"mailFrom":{"email":"test@example.org"},"rcptTo":[{"email":"not an address"}]}}`, setInvalidRecipients},
		{`{"identityId":"` + ident + `","emailId":"` + good.GUID + `","sendAt":"2030-01-01T00:00:00Z"}`, setInvalidProperties},
	}

	for i, c := range cases {
		if got := setErrorType(create(c.body), "notCreated", "k"); got != c.want {
			t.Errorf("case %d gave %q, want %s", i, got, c.want)
		}
	}

	if relay.calls != 0 {
		t.Fatalf("the relay was asked %d times for refused submissions", relay.calls)
	}

	env := `,"envelope":{"mailFrom":{"email":"test@example.org"},"rcptTo":[{"email":"a@example.net"}]}}`

	relay.report = &outbound.Report{Stage: "RCPT", Refused: map[string]outbound.Reply{
		"a@example.net": {Code: 553, Message: "5.7.1 <test@example.org>: Sender address rejected: not owned by user"}}}

	if got := setErrorType(create(`{"identityId":"`+ident+`","emailId":"`+good.GUID+`"`+env), "notCreated", "k"); got != setForbiddenMailFrom {
		t.Errorf("a sender refused by the relay gave %q", got)
	}

	relay.report = &outbound.Report{Stage: "RCPT", Refused: map[string]outbound.Reply{
		"a@example.net": {Code: 550, Message: "5.1.1 no such user"}}}

	res := create(`{"identityId":"` + ident + `","emailId":"` + good.GUID + `"` + env)
	if got := setErrorType(res, "notCreated", "k"); got != setInvalidRecipients {
		t.Errorf("a recipient refused by the relay gave %q", got)
	}

	relay.report = &outbound.Report{Stage: "RCPT", Refused: map[string]outbound.Reply{"a@example.net": {Code: 450, Message: "4.2.0 later"}}}

	if got := setErrorType(create(`{"identityId":"`+ident+`","emailId":"`+good.GUID+`"`+env), "notCreated", "k"); got != setServerFail {
		t.Errorf("a temporary refusal gave %q", got)
	}
}

// TestStripBcc: the Bcc field and its continuation go, nothing else.
func TestStripBcc(t *testing.T) {
	got := string(stripBcc([]byte("To: a\r\nBCC: b,\r\n c\r\nSubject: s\r\n\r\nBcc: in the body\r\n")))
	if got != "To: a\r\nSubject: s\r\n\r\nBcc: in the body\r\n" {
		t.Errorf("got %q", got)
	}
}

// TestComposeAndSendInOneRequest: Email/set creates the draft, and
// EmailSubmission/set sends it by its creation id and destroys it after.
func TestComposeAndSendInOneRequest(t *testing.T) {
	s, u, relay := submissionServer(t)
	boxes := boxIDs(t, u)
	ident := identityID(t, s)

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail","urn:ietf:params:jmap:submission"],
		"methodCalls":[
		["Email/set",{"accountId":"test","create":{"d":{"mailboxIds":{"`+boxes["Drafts"]+`":true},
			"from":[{"email":"test@example.org"}],"to":[{"email":"bob@example.net"}],"subject":"hi",
			"textBody":[{"partId":"t"}],"bodyValues":{"t":{"value":"hello"}}}}},"a"],
		["EmailSubmission/set",{"accountId":"test","create":{"k":{"identityId":"`+ident+`","emailId":"#d"}},
			"onSuccessDestroyEmail":["#k"]},"b"]]}`)

	rs := responses(t, out)
	if len(rs) != 3 || rs[2].([]any)[0] != "Email/set" {
		t.Fatalf("responses %#v", rs)
	}

	destroyed, _ := rs[2].([]any)[1].(map[string]any)["destroyed"].([]any)
	if len(destroyed) != 1 || relay.calls != 1 || strings.Join(relay.to, ",") != "bob@example.net" {
		t.Errorf("destroyed %#v, relay calls %d to %v", destroyed, relay.calls, relay.to)
	}
}

// TestEmailSubmissionRefusesHeaderTricks: a second From, a foreign
// Sender, or several From addresses without a Sender do not get past the
// check, although the envelope sender is allowed.
func TestEmailSubmissionRefusesHeaderTricks(t *testing.T) {
	s, u, relay := submissionServer(t)
	ident := identityID(t, s)

	cases := map[string]string{
		"From: test@example.org\r\nFrom: ceo@example.org\r\nTo: a@x.net\r\n\r\nx\r\n":        setInvalidEmail,
		"From: test@example.org\r\nSender: ceo@example.org\r\nTo: a@x.net\r\n\r\nx\r\n":      setForbiddenFrom,
		"From: test@example.org, ceo@example.org\r\nTo: a@x.net\r\n\r\nx\r\n":                setInvalidEmail,
		"From: test@example.org\r\nTo: a@x.net\r\nCc: this is not, an <address\r\n\r\nx\r\n": setInvalidEmail,
		"From: =?utf-8?q?Test?= <ceo@example.org>\r\nTo: a@x.net\r\n\r\nx\r\n":               setForbiddenFrom,
		"From: \"test@example.org\" <ceo@example.org>\r\nTo: a@x.net\r\n\r\nx\r\n":           setForbiddenFrom,
	}

	for raw, want := range cases {
		m := deliver(t, u, "Drafts", raw)

		res := submissionCall(t, s, "EmailSubmission/set", `{"accountId":"test","create":{"k":{"identityId":"`+ident+`","emailId":"`+m.GUID+`"}}}`)
		if got := setErrorType(res, "notCreated", "k"); got != want {
			t.Errorf("%q gave %q, want %s", raw, got, want)
		}
	}

	if relay.calls != 0 {
		t.Errorf("the relay was asked %d times", relay.calls)
	}

	if got := string(stripBcc([]byte("From: a@b\r\nBcc: x@y\r\n"))); strings.Contains(got, "Bcc") {
		t.Errorf("a header-only message keeps its Bcc: %q", got)
	}
}

// TestEmailSubmissionWithoutXClient: a relay that will not let this host
// name users is a permanent refusal, not "try again later".
func TestEmailSubmissionWithoutXClient(t *testing.T) {
	s, u, relay := submissionServer(t)
	ident := identityID(t, s)
	m := deliver(t, u, "Drafts", draftMessage)

	relay.err = outbound.ErrNoXClient

	res := submissionCall(t, s, "EmailSubmission/set", `{"accountId":"test","create":{"k":{"identityId":"`+ident+`","emailId":"`+m.GUID+`"}}}`)
	if got := setErrorType(res, "notCreated", "k"); got != setForbiddenToSend {
		t.Errorf("a relay without XCLIENT gave %q", got)
	}
}
