package jmap

import (
	"strconv"
	"strings"
	"testing"
)

// snippets runs SearchSnippet/get and returns the snippets by email id.
func snippets(t *testing.T, s *Server, args string) (map[string]map[string]any, []any) {
	t.Helper()

	res := mailCall(t, s, "SearchSnippet/get", args)

	list, _ := res["list"].([]any)
	out := map[string]map[string]any{}

	for _, item := range list {
		obj, _ := item.(map[string]any)
		out[obj["emailId"].(string)] = obj
	}

	notFound, _ := res["notFound"].([]any)

	return out, notFound
}

// TestSearchSnippetGet: the subject and a piece of the body, with the
// matched words marked and the rest escaped; null where nothing matched.
func TestSearchSnippetGet(t *testing.T) {
	s, u := emailServer(t)

	report := deliver(t, u, inboxName, "From: a@example.org\r\nSubject: Reports <Q3> & more\r\n\r\n"+
		strings.Repeat("filler words here ", 20)+"the quarterly report is attached & signed\r\n")
	other := deliver(t, u, inboxName, "From: a@example.org\r\nSubject: lunch\r\n\r\nnothing to see\r\n")

	got, notFound := snippets(t, s, `{"accountId":"test","filter":{"text":"report"},"emailIds":["`+
		report.GUID+`","`+other.GUID+`","Mnope"]}`)

	if len(notFound) != 1 || notFound[0] != "Mnope" {
		t.Errorf("notFound is %v, want the unknown id", notFound)
	}

	r := got[report.GUID]
	if r["subject"] != "<mark>Reports</mark> &lt;Q3&gt; &amp; more" {
		t.Errorf("the subject snippet is %q", r["subject"])
	}

	preview, _ := r["preview"].(string)
	if !strings.Contains(preview, "quarterly <mark>report</mark> is attached &amp; signed") {
		t.Errorf("the preview is %q", preview)
	}

	if len(preview) > snippetPreviewMax || strings.HasPrefix(preview, "filler words here filler") {
		t.Errorf("the preview is %d octets and starts at %q", len(preview), preview[:20])
	}

	o := got[other.GUID]
	if o["subject"] != nil || o["preview"] != nil {
		t.Errorf("a message that does not match has the snippet %#v", o)
	}
}

// TestSearchSnippetConditions: subject only marks the subject, body only
// the body, and a word under NOT is not marked.
func TestSearchSnippetConditions(t *testing.T) {
	s, u := emailServer(t)

	m := deliver(t, u, inboxName, "From: a@example.org\r\nSubject: budget plan\r\n\r\nthe budget plan for next year\r\n")
	ids := `"emailIds":["` + m.GUID + `"]`

	got, _ := snippets(t, s, `{"accountId":"test","filter":{"subject":"budget"},`+ids+`}`)
	if got[m.GUID]["subject"] != "<mark>budget</mark> plan" || got[m.GUID]["preview"] != nil {
		t.Errorf("a subject condition gave %#v", got[m.GUID])
	}

	got, _ = snippets(t, s, `{"accountId":"test","filter":{"body":"plan"},`+ids+`}`)
	if got[m.GUID]["subject"] != nil || got[m.GUID]["preview"] != "the budget <mark>plan</mark> for next year" {
		t.Errorf("a body condition gave %#v", got[m.GUID])
	}

	got, _ = snippets(t, s, `{"accountId":"test","filter":{"operator":"AND","conditions":[{"body":"budget"},`+
		`{"operator":"NOT","conditions":[{"body":"year"}]}]},`+ids+`}`)
	if got[m.GUID]["preview"] != "the <mark>budget</mark> plan for next year" {
		t.Errorf("a NOT condition was marked: %#v", got[m.GUID])
	}

	got, _ = snippets(t, s, `{"accountId":"test",`+ids+`}`)
	if got[m.GUID]["subject"] != nil || got[m.GUID]["preview"] != nil {
		t.Errorf("no filter gave %#v", got[m.GUID])
	}
}

// TestSearchSnippetHTML: an HTML body is shown as text, with its entities
// decoded and escaped again.
func TestSearchSnippetHTML(t *testing.T) {
	s, u := emailServer(t)

	m := deliver(t, u, inboxName, "From: a@example.org\r\nSubject: s\r\nContent-Type: text/html\r\n\r\n"+
		"<html><head><style>p{color:red}</style></head><body><p>Fish &amp; <b>chips</b> tonight</p></body></html>\r\n")

	got, _ := snippets(t, s, `{"accountId":"test","filter":{"body":"chips"},"emailIds":["`+m.GUID+`"]}`)
	if got[m.GUID]["preview"] != "Fish &amp; <mark>chips</mark> tonight" {
		t.Errorf("the HTML preview is %q", got[m.GUID]["preview"])
	}
}

// TestSearchSnippetRefuses: the limits and the filter checks of
// Email/query.
func TestSearchSnippetRefuses(t *testing.T) {
	s, _ := emailServer(t)

	for _, c := range []struct{ args, want string }{
		{`{"accountId":"test","filter":{"text":"x"}}`, errInvalidArguments},
		{`{"accountId":"test","filter":{"unknown":"x"},"emailIds":[]}`, errUnsupportedFilter},
		{`{"accountId":"test","filter":{"before":"yesterday"},"emailIds":[]}`, errUnsupportedFilter},
		{`{"accountId":"test","filter":[],"emailIds":[]}`, errInvalidArguments},
		{`{"accountId":"test","emailIds":[` + manyIDs(501) + `]}`, errRequestTooLarge},
		{`{"accountId":"other","emailIds":[]}`, errAccountNotFound},
	} {
		if got := mailError(t, s, "SearchSnippet/get", c.args); got != c.want {
			t.Errorf("%s answered %q, want %q", c.args, got, c.want)
		}
	}
}

// TestSearchSnippetReadBound: a match beyond the part of the message
// that is read gives no preview.
func TestSearchSnippetReadBound(t *testing.T) {
	s, u := emailServer(t)
	s.o.Limits.SnippetReadBytes = 200

	m := deliver(t, u, inboxName, "From: a@example.org\r\nSubject: s\r\n\r\n"+strings.Repeat("padding ", 50)+"needle\r\n")

	got, _ := snippets(t, s, `{"accountId":"test","filter":{"body":"needle"},"emailIds":["`+m.GUID+`"]}`)
	if got[m.GUID]["preview"] != nil {
		t.Errorf("a match past the read bound gave the preview %q", got[m.GUID]["preview"])
	}
}

// TestSearchSnippetBackReference: the usual call, with the ids of an
// Email/query in the same request.
func TestSearchSnippetBackReference(t *testing.T) {
	s, u := emailServer(t)

	m := deliver(t, u, inboxName, "From: a@example.org\r\nSubject: invoice\r\n\r\nplease pay\r\n")

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:mail"],"methodCalls":[
		["Email/query",{"accountId":"test","filter":{"text":"invoice"}},"q"],
		["SearchSnippet/get",{"accountId":"test","filter":{"text":"invoice"},
			"#emailIds":{"resultOf":"q","name":"Email/query","path":"/ids"}},"s"]]}`)

	got, _ := responses(t, out)[1].([]any)
	res, _ := got[1].(map[string]any)
	list, _ := res["list"].([]any)

	if got[0] != "SearchSnippet/get" || len(list) != 1 {
		t.Fatalf("the snippet call answered %#v", got)
	}

	if sn, _ := list[0].(map[string]any); sn["emailId"] != m.GUID || sn["subject"] != "<mark>invoice</mark>" {
		t.Errorf("the snippet is %#v", sn)
	}
}

// manyIDs is a list of n distinct ids, as JSON array elements.
func manyIDs(n int) string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = `"M` + strconv.Itoa(i) + `"`
	}

	return strings.Join(ids, ",")
}

// TestSearchSnippetDedupes: an id given twice is one snippet, in the
// place of its first mention, and many repeats of one id are not too
// many.
func TestSearchSnippetDedupes(t *testing.T) {
	s, u := emailServer(t)

	a := deliver(t, u, inboxName, "From: a@example.org\r\nSubject: alpha\r\n\r\nx\r\n")
	b := deliver(t, u, inboxName, "From: a@example.org\r\nSubject: beta\r\n\r\nx\r\n")

	res := mailCall(t, s, "SearchSnippet/get", `{"accountId":"test","filter":{"subject":"alpha"},"emailIds":["`+
		b.GUID+`","`+a.GUID+`","`+b.GUID+`"`+strings.Repeat(`,"`+a.GUID+`"`, 600)+`]}`)

	list, _ := res["list"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["emailId"] != b.GUID || list[1].(map[string]any)["emailId"] != a.GUID {
		t.Errorf("the snippets are %#v", list)
	}
}

// TestHTMLTextKeepsComparisons: a "<" that starts no tag is text.
func TestHTMLTextKeepsComparisons(t *testing.T) {
	if got := collapseSpace(htmlText("<p>if a < b and c<5 then</p>")); got != "if a < b and c<5 then" {
		t.Errorf("htmlText gave %q", got)
	}
}
