package jmap

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
	"shardpost/internal/model"
)

// blobUsing is the "using" of the blob tests: the blob methods, and mail
// for the lookup and the import.
const blobUsing = `"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:blob","urn:ietf:params:jmap:mail"]`

// blobCalls sends a batch of method calls and returns the arguments of
// each response, in order, failing the test on an error response.
func blobCalls(t *testing.T, s *Server, calls string) []map[string]any {
	t.Helper()

	out := call(t, s, `{`+blobUsing+`,"methodCalls":[`+calls+`]}`)

	var list []map[string]any

	for _, r := range responses(t, out) {
		inv, _ := r.([]any)
		if inv[0] == kwError {
			t.Fatalf("a call failed: %#v", inv[1])
		}

		args, _ := inv[1].(map[string]any)
		list = append(list, args)
	}

	return list
}

// blobError sends one method call and returns the error type it gave.
func blobError(t *testing.T, s *Server, name, args string) string {
	t.Helper()

	out := call(t, s, `{`+blobUsing+`,"methodCalls":[["`+name+`",`+args+`,"c1"]]}`)

	inv, _ := responses(t, out)[0].([]any)
	if inv[0] != kwError {
		t.Fatalf("%s answered %v, want an error: %#v", name, inv[0], inv[1])
	}

	e, _ := inv[1].(map[string]any)
	typ, _ := e["type"].(string)

	return typ
}

// blobList is the list of a Blob/get response, keyed by id.
func blobList(t *testing.T, res map[string]any) map[string]map[string]any {
	t.Helper()

	out := map[string]map[string]any{}

	list, _ := res["list"].([]any)
	for _, item := range list {
		obj, _ := item.(map[string]any)
		out[obj[propID].(string)] = obj
	}

	return out
}

// createdID is the id of one creation of a /set-like response.
func createdID(t *testing.T, res map[string]any, cid string) string {
	t.Helper()

	c, _ := res["created"].(map[string]any)[cid].(map[string]any)
	if c == nil {
		t.Fatalf("%s was not created: %#v", cid, res["notCreated"])
	}

	id, _ := c[propID].(string)

	return id
}

// TestBlobUploadChaining is the example of RFC 9404 4.1.2: a blob made of
// text, base64 and ranges of another blob, named by its creation id from
// the calls after it — and from a creation in the same call.
func TestBlobUploadChaining(t *testing.T) {
	s := newTestServer(t)

	res := blobCalls(t, s, `
		["Blob/upload",{"create":{"b4":{"data":[{"data:asText":"The quick brown fox jumped over the lazy dog."}]}}},"S4"],
		["Blob/upload",{"create":{
			"cat":{"data":[{"data:asText":"How"},{"blobId":"#b4","length":7,"offset":3},{"data:asText":"was t"},
				{"blobId":"#b4","length":1,"offset":1},{"data:asBase64":"YXQ/"}],"type":"text/plain"},
			"again":{"data":[{"blobId":"#cat","offset":4}]}}},"CAT"],
		["Blob/get",{"properties":["data:asText","size"],"ids":["#cat","#again"]},"G4"]`)

	b4 := res[0]["created"].(map[string]any)["b4"].(map[string]any)
	if b4[propSize] != float64(45) || b4[partType] != defaultBlobType || b4[propBlobID] != b4[propID] {
		t.Errorf("b4 is %#v", b4)
	}

	cat := res[1]["created"].(map[string]any)["cat"].(map[string]any)
	if cat[propSize] != float64(19) || cat[partType] != "text/plain" {
		t.Errorf("cat is %#v", cat)
	}

	got := blobList(t, res[2])
	if got[cat[propID].(string)][propDataAsText] != "How quick was that?" ||
		got[createdID(t, res[1], "again")][propDataAsText] != "quick was that?" {
		t.Errorf("Blob/get answered %#v", res[2]["list"])
	}
}

// TestBlobGetDigests: the digests of RFC 9404 4.2.1, of the whole blob
// and of a range, and the size of the whole blob either way.
func TestBlobGetDigests(t *testing.T) {
	s := newTestServer(t)
	id := upload(t, s, "The quick brown fox jumped over the lazy dog.", "text/plain")

	res := blobCalls(t, s, `
		["Blob/get",{"ids":["`+id+`","not-a-blob"],"properties":["data:asText","digest:sha","size"]},"R1"],
		["Blob/get",{"ids":["`+id+`"],"properties":["digest:sha","digest:sha-256","size"],"offset":4,"length":9},"R2"]`)

	whole := blobList(t, res[0])[id]
	if whole[propDataAsText] != "The quick brown fox jumped over the lazy dog." || whole[digestPrefix+digestSHA] != "wIVPufsDxBzOOALLDSIFKebu+U4=" ||
		whole[propSize] != float64(45) {
		t.Errorf("R1 is %#v", whole)
	}

	if nf, _ := res[0]["notFound"].([]any); len(nf) != 1 || nf[0] != "not-a-blob" {
		t.Errorf("notFound is %#v", res[0]["notFound"])
	}

	part := blobList(t, res[1])[id]
	if part[digestPrefix+digestSHA] != "QiRAPtfyX8K6tm1iOAtZ87Xj3Ww=" ||
		part[digestPrefix+digestSHA256] != "gdg9INW7lwHK6OQ9u0dwDz2ZY/gubi0En0xlFpKt0OA=" || part[propSize] != float64(45) {
		t.Errorf("R2 is %#v", part)
	}

	if _, ok := part[propDataAsText]; ok {
		t.Errorf("R2 returned data it was not asked for: %#v", part)
	}
}

// TestBlobGetEncodingAndTruncation is the example of RFC 9404 4.2.2:
// octets that are not UTF-8, and ranges that run past the end.
func TestBlobGetEncodingAndTruncation(t *testing.T) {
	s := newTestServer(t)

	res := blobCalls(t, s, `
		["Blob/upload",{"create":{"b1":{"data":[{"data:asBase64":"VGhlIHF1aWNrIGJyb3duIGZveCBqdW1wZWQgb3ZlciB0aGUggYEgZG9nLg=="}]},
			"b2":{"data":[{"data:asText":"hello world"}],"type":"text/plain"}}},"S1"],
		["Blob/get",{"ids":["#b1","#b2"]},"G1"],
		["Blob/get",{"ids":["#b1","#b2"],"properties":["data:asText","size"]},"G2"],
		["Blob/get",{"ids":["#b1","#b2"],"properties":["data:asBase64","size"]},"G3"],
		["Blob/get",{"offset":0,"length":5,"ids":["#b1","#b2"]},"G4"],
		["Blob/get",{"offset":20,"length":100,"ids":["#b1","#b2"]},"G5"],
		["Blob/get",{"offset":50,"ids":["#b1"]},"G6"]`)

	b1, b2 := createdID(t, res[0], "b1"), createdID(t, res[0], "b2")
	b1b64 := "VGhlIHF1aWNrIGJyb3duIGZveCBqdW1wZWQgb3ZlciB0aGUggYEgZG9nLg=="

	for i, want := range []map[string]map[string]any{
		{b1: {propIsEncodingProblem: true, propDataAsBase64: b1b64, propSize: float64(43)}, b2: {propDataAsText: "hello world", propSize: float64(11)}},
		{b1: {propIsEncodingProblem: true, propDataAsText: nil, propSize: float64(43)}, b2: {propDataAsText: "hello world", propSize: float64(11)}},
		{b1: {propDataAsBase64: b1b64, propSize: float64(43)}, b2: {propDataAsBase64: "aGVsbG8gd29ybGQ=", propSize: float64(11)}},
		{b1: {propDataAsText: "The q", propSize: float64(43)}, b2: {propDataAsText: "hello", propSize: float64(11)}},
		{b1: {propIsTruncated: true, propIsEncodingProblem: true, propDataAsBase64: "anVtcGVkIG92ZXIgdGhlIIGBIGRvZy4=", propSize: float64(43)},
			b2: {propIsTruncated: true, propDataAsText: "", propSize: float64(11)}},
		{b1: {propIsTruncated: true, propDataAsText: "", propSize: float64(43)}},
	} {
		got := blobList(t, res[i+1])
		for id, props := range want {
			hasValues(t, "G"+string(rune('1'+i))+" "+id, got[id], props)

			if len(got[id]) != len(props)+1 {
				t.Errorf("G%d %s has other properties: %#v", i+1, id, got[id])
			}
		}
	}
}

// TestBlobUploadRefuses: what RFC 9404 4.1 asks to be refused, each with
// its own error, and the limits.
func TestBlobUploadRefuses(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Limits.MaxSizeBlobSet = 16 })
	stored := upload(t, s, "0123456789", "text/plain")

	res := blobCalls(t, s, `["Blob/upload",{"create":{
		"b64":{"data":[{"data:asBase64":"not base64!"}]},
		"two":{"data":[{"data:asText":"a","data:asBase64":"YQ=="}]},
		"none":{"data":[{}]},
		"range":{"data":[{"data:asText":"a","offset":1}]},
		"past":{"data":[{"blobId":"`+stored+`","offset":4,"length":7}]},
		"after":{"data":[{"blobId":"`+stored+`","offset":11}]},
		"missing":{"data":[{"blobId":"0123456789abcdef0123456789abcdef"},{"blobId":"#nothing"}]},
		"large":{"data":[{"blobId":"`+stored+`"},{"blobId":"`+stored+`"}]},
		"surrogate":{"data":[{"data:asText":"\ud800"}]},
		"extra":{"data":[],"name":"x"},
		"nodata":{"type":"text/plain"},
		"cycle":{"data":[{"blobId":"#cycle"}]},
		"empty":{"data":[]}}},"c1"]`)[0]

	for cid, want := range map[string]string{
		"b64": setInvalidProperties, "two": setInvalidProperties, "none": setInvalidProperties, "range": setInvalidProperties,
		"past": setInvalidProperties, "after": setInvalidProperties, "missing": setBlobNotFound, "large": setTooLarge,
		"surrogate": setInvalidProperties, "extra": setInvalidProperties, "nodata": setInvalidProperties, "cycle": setBlobNotFound,
	} {
		if got := setErrorType(res, "notCreated", cid); got != want {
			t.Errorf("%s gave %q, want %s", cid, got, want)
		}
	}

	missing := res["notCreated"].(map[string]any)["missing"].(map[string]any)["notFound"].([]any)
	if len(missing) != 2 {
		t.Errorf("blobNotFound names %#v", missing)
	}

	if createdID(t, res, "empty") == "" {
		t.Error("an empty blob was not created")
	}

	// Invalid UTF-8 in the request itself: encoding/json would make a
	// U+FFFD of it, which is not what the client sent.
	bad := blobCalls(t, s, `["Blob/upload",{"create":{"x":{"data":[{"data:asText":"a`+"\x81"+`b"}]},
		"ok":{"data":[{"data:asText":"`+escapedRuneError+string(utf8.RuneError)+`"}]}}},"c1"]`)[0]
	if got := setErrorType(bad, "notCreated", "x"); got != setInvalidProperties {
		t.Errorf("invalid UTF-8 gave %q", got)
	}

	if createdID(t, bad, "ok") == "" {
		t.Error("a U+FFFD the client wrote was refused")
	}
}

// TestBlobUploadLimits: maxDataSources, maxObjectsInSet, and an account
// out of quota.
func TestBlobUploadLimits(t *testing.T) {
	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	if err := u.SetQuotaLimit(model.Quota{Bytes: 4}); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t, func(o *Options) { o.Store = store; o.Limits.MaxObjectsInSet = 2 })

	sources := strings.Repeat(`{"data:asText":"a"},`, minDataSources)

	res := blobCalls(t, s, `["Blob/upload",{"create":{"many":{"data":[`+sources+`{"data:asText":"a"}]},
		"big":{"data":[{"data:asText":"12345"}]}}},"c1"]`)[0]
	if got := setErrorType(res, "notCreated", "many"); got != setInvalidProperties {
		t.Errorf("65 data sources gave %q", got)
	}

	if got := setErrorType(res, "notCreated", "big"); got != setOverQuota {
		t.Errorf("a blob beyond the quota gave %q", got)
	}

	if got := blobError(t, s, "Blob/upload", `{"create":{"a":{"data":[]},"b":{"data":[]},"c":{"data":[]}}}`); got != errRequestTooLarge {
		t.Errorf("more creations than maxObjectsInSet gave %q", got)
	}
}

// TestBlobGetRefuses: the arguments Blob/get does not take, and the
// bound on the data one call returns.
func TestBlobGetRefuses(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Limits.MaxSizeBlobGet = 16 })
	id := upload(t, s, "0123456789", "text/plain")

	for args, want := range map[string]string{
		`{}`: errRequestTooLarge,
		`{"ids":["` + id + `"],"properties":["name"]}`:                              errInvalidArguments,
		`{"ids":["` + id + `"],"properties":["digest:md5"]}`:                        errInvalidArguments,
		`{"ids":["` + id + `"],"offset":-1}`:                                        errInvalidArguments,
		`{"ids":["` + id + `"],"accountId":"other"}`:                                errAccountNotFound,
		`{"ids":["` + id + `","` + upload(t, s, "abcdefghij", "text/plain") + `"]}`: errRequestTooLarge,
	} {
		if got := blobError(t, s, "Blob/get", args); got != want {
			t.Errorf("%s gave %q, want %s", args, got, want)
		}
	}

	// The bound is on the data: a digest of the same blobs is fine, and
	// so is a range.
	res := blobCalls(t, s, `["Blob/get",{"ids":["`+id+`"],"properties":["digest:sha-256","size"]},"c1"],
		["Blob/get",{"ids":["`+id+`"],"offset":8},"c2"]`)
	if blobList(t, res[1])[id][propDataAsText] != "89" || blobList(t, res[0])[id][digestPrefix+digestSHA256] == nil {
		t.Errorf("Blob/get answered %#v", res)
	}
}

// TestBlobGetReadsEveryBlob: a message's blob, a body part's and an
// upload's all read, and so does a body part's range.
func TestBlobGetReadsEveryBlob(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, alternativeMessage)

	att := getEmail(t, s, m.GUID, `["attachments"]`)[propAttachments].([]any)[0].(map[string]any)
	part := att[partBlobID].(string)

	res := blobCalls(t, s, `["Blob/get",{"ids":["`+m.GUID+`","`+part+`"],"properties":["data:asText","size","digest:sha"]},"c1"],
		["Blob/get",{"ids":["`+part+`"],"offset":1,"length":3},"c2"]`)

	got := blobList(t, res[0])
	if got[m.GUID][propDataAsText] != alternativeMessage || got[m.GUID][propSize] != float64(len(alternativeMessage)) {
		t.Errorf("the message blob is %#v", got[m.GUID])
	}

	if got[part][propDataAsText] != "%PDF-1.4\n" || got[part][digestPrefix+digestSHA] == nil {
		t.Errorf("the part blob is %#v", got[part])
	}

	if blobList(t, res[1])[part][propDataAsText] != "PDF" {
		t.Errorf("the part range is %#v", res[1])
	}
}

// TestBlobOfAnotherAccount: a blob belongs to the account whose store it
// is in; another account does not find it, whoever knows its id.
func TestBlobOfAnotherAccount(t *testing.T) {
	store := memstore.New()
	s := newTestServer(t, func(o *Options) {
		o.Store = store
		o.Auth = auth.Static{"test": "secret", "other": "secret"}
	})

	id := upload(t, s, "private", "text/plain")

	r := httptest.NewRequest(http.MethodPost, PathAPI, strings.NewReader(`{`+blobUsing+`,"methodCalls":[
		["Blob/get",{"accountId":"other","ids":["`+id+`"]},"c1"],
		["Blob/upload",{"accountId":"other","create":{"x":{"data":[{"blobId":"`+id+`"}]}}},"c2"],
		["Blob/get",{"accountId":"test","ids":["`+id+`"]},"c3"]]}`))
	r.SetBasicAuth("other", "secret")

	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if !strings.Contains(w.Body.String(), `"notFound":["`+id+`"]`) || !strings.Contains(w.Body.String(), `"type":"blobNotFound"`) ||
		!strings.Contains(w.Body.String(), `"type":"accountNotFound"`) || strings.Contains(w.Body.String(), "private") {
		t.Errorf("another account reading the blob got %s", w.Body.String())
	}
}

// TestBlobLookup: a message's blob and a body part's belong to the
// message, its thread and its mailboxes; an unknown blob to nothing; and
// a type the request did not use is unknownDataType.
func TestBlobLookup(t *testing.T) {
	s, u := emailServer(t)
	m := deliver(t, u, inboxName, alternativeMessage)
	boxes := boxIDs(t, u)
	thread := getEmail(t, s, m.GUID, `["threadId"]`)[propThreadID]

	res := blobCalls(t, s, `["Blob/lookup",{"typeNames":["Mailbox","Thread","Email"],
		"ids":["`+m.GUID+`","`+m.GUID+`_2","0123456789abcdef0123456789abcdef","not-a-blob"]},"c1"]`)[0]

	list, _ := res["list"].([]any)
	if len(list) != 4 {
		t.Fatalf("lookup %#v", res)
	}

	for i, item := range list {
		matched := item.(map[string]any)["matchedIds"].(map[string]any)

		switch {
		case i < 2:
			if !idsAre(matched[typeEmail], m.GUID) || !idsAre(matched[typeThread], thread.(string)) ||
				!idsAre(matched[typeMailbox], boxes[inboxName]) {
				t.Errorf("entry %d matched %#v", i, matched)
			}
		case !idsAre(matched[typeEmail]) || !idsAre(matched[typeMailbox]) || !idsAre(matched[typeThread]):
			t.Errorf("entry %d matched %#v", i, matched)
		}
	}

	if got := blobError(t, s, "Blob/lookup", `{"typeNames":["Calendar"],"ids":[]}`); got != errUnknownDataType {
		t.Errorf("an unknown type gave %q", got)
	}

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:blob"],"methodCalls":[
		["Blob/lookup",{"typeNames":["Email"],"ids":[]},"c1"]]}`)
	if e := responses(t, out)[0].([]any)[1].(map[string]any); e["type"] != errUnknownDataType {
		t.Errorf("Email without the mail capability gave %#v", e)
	}
}

// TestBlobUploadFeedsEmailImport: a blob built by Blob/upload is a blob
// like any other, and a later call names it by its creation id.
func TestBlobUploadFeedsEmailImport(t *testing.T) {
	s, u := emailServer(t)
	boxes := boxIDs(t, u)

	res := blobCalls(t, s, `["Blob/upload",{"create":{"m":{"data":[{"data:asText":"Subject: built\r\n\r\n"},
			{"data:asText":"body\r\n"}],"type":"message/rfc822"}}},"c1"],
		["Email/import",{"emails":{"e":{"blobId":"#m","mailboxIds":{"`+boxes[inboxName]+`":true}}}},"c2"]`)

	id := createdID(t, res[1], "e")
	if got := getEmail(t, s, id, `["subject","size"]`); got[propSubject] != "built" || got[propSize] != float64(24) {
		t.Errorf("the imported email is %#v", got)
	}
}

// TestBlobCapability: the session lists the capability empty, the
// account with its limits and what Blob/lookup and Blob/get support.
func TestBlobCapability(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Limits.MaxSizeUpload = 1000; o.Limits.MaxSizeBlobSet = 5000 })

	got := s.session("test", "https://mail.example.org")
	if string(got.Capabilities[CapBlob]) != `{}` || got.PrimaryAccounts[CapBlob] != "test" {
		t.Errorf("the session lists %s", got.Capabilities[CapBlob])
	}

	want := `{"supportedTypeNames":["Email","Mailbox","Thread"],"supportedDigestAlgorithms":["sha-256","sha"],` +
		`"maxSizeBlobSet":1000,"maxDataSources":64}`
	if acc := string(got.Accounts["test"].AccountCapabilities[CapBlob]); acc != want {
		t.Errorf("the account lists %s, want %s", acc, want)
	}
}
