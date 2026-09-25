package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
)

// newTestServer returns a JMAP front-end over an in-memory store.
func newTestServer(t *testing.T, customize ...func(*Options)) *Server {
	t.Helper()

	o := Options{
		Store:         memstore.New(),
		Auth:          auth.Static{"test": "secret"},
		InsecureAuth:  true,
		PublicBaseURL: "https://mail.example.org",
	}

	for _, fn := range customize {
		fn(&o)
	}

	return New(o)
}

// do sends a request with the test user's credentials.
func do(t *testing.T, s *Server, method, target string, body string) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.SetBasicAuth("test", "secret")

	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	return w
}

// call sends one API request and returns the decoded response.
func call(t *testing.T, s *Server, body string) map[string]any {
	t.Helper()

	w := do(t, s, http.MethodPost, PathAPI, body)
	if w.Code != http.StatusOK {
		t.Fatalf("api answered %d: %s", w.Code, w.Body.String())
	}

	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("api response: %v (%s)", err, w.Body.String())
	}

	return out
}

// responses is the methodResponses of a decoded API response.
func responses(t *testing.T, out map[string]any) []any {
	t.Helper()

	list, ok := out["methodResponses"].([]any)
	if !ok {
		t.Fatalf("the response has no methodResponses: %#v", out)
	}

	return list
}

// TestHealthNeedsNoCredentials: the backend check of a proxy in front
// must work without a login and must not touch the store.
func TestHealthNeedsNoCredentials(t *testing.T) {
	s := newTestServer(t)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, PathHealth, nil))

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("health answered %d: %q", w.Code, w.Body.String())
	}
}

// TestAuthentication: no credentials and wrong credentials are both
// refused, and the refusal asks for credentials.
func TestAuthentication(t *testing.T) {
	s := newTestServer(t)

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, PathSession, nil))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("a request without credentials answered %d, want 401", w.Code)
	}

	if w.Header().Get("WWW-Authenticate") == "" {
		t.Error("the refusal does not ask for credentials")
	}

	r := httptest.NewRequest(http.MethodGet, PathSession, nil)
	r.SetBasicAuth("test", "wrong")

	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("a wrong password answered %d, want 401", w.Code)
	}
}

// TestAuthenticationNeedsTLS: without insecure_auth a plaintext request
// is refused before any credential is looked at.
func TestAuthenticationNeedsTLS(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.InsecureAuth = false })

	w := do(t, s, http.MethodGet, PathSession, "")
	if w.Code != http.StatusForbidden {
		t.Errorf("a plaintext request answered %d, want 403", w.Code)
	}
}

// TestSessionResource: the session names the account, the limits and the
// URLs a client uses, built from the configured public base.
func TestSessionResource(t *testing.T) {
	s := newTestServer(t)

	w := do(t, s, http.MethodGet, PathSession, "")
	if w.Code != http.StatusOK {
		t.Fatalf("session answered %d: %s", w.Code, w.Body.String())
	}

	var got Session
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if got.Username != "test" {
		t.Errorf("the session names %q, want test", got.Username)
	}

	if _, ok := got.Capabilities[CapCore]; !ok {
		t.Errorf("the session does not offer %s: %v", CapCore, got.Capabilities)
	}

	if _, ok := got.Accounts["test"]; !ok {
		t.Errorf("the session has no account for the user: %v", got.Accounts)
	}

	if got.APIURL != "https://mail.example.org"+PathAPI {
		t.Errorf("apiUrl is %q, want the configured base", got.APIURL)
	}

	if got.State == "" {
		t.Error("the session has no state")
	}

	// The state is a function of the session, so a second read of an
	// unchanged session returns the same one.
	w2 := do(t, s, http.MethodGet, PathSession, "")

	var again Session
	if err := json.Unmarshal(w2.Body.Bytes(), &again); err != nil {
		t.Fatal(err)
	}

	if again.State != got.State {
		t.Errorf("the state changed from %q to %q although the session did not", got.State, again.State)
	}
}

// TestCoreEcho: the method that exists returns its arguments unchanged,
// under the call id it was given.
func TestCoreEcho(t *testing.T) {
	s := newTestServer(t)

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core"],
		"methodCalls":[["Core/echo",{"hello":["world",42]},"c1"]]}`)

	list := responses(t, out)
	if len(list) != 1 {
		t.Fatalf("got %d responses, want 1", len(list))
	}

	got, ok := list[0].([]any)
	if !ok || len(got) != 3 {
		t.Fatalf("the response is not a triple: %#v", list[0])
	}

	if got[0] != "Core/echo" || got[2] != "c1" {
		t.Errorf("the response is %v/%v, want Core/echo and c1", got[0], got[2])
	}

	args, ok := got[1].(map[string]any)
	if !ok || len(args["hello"].([]any)) != 2 {
		t.Errorf("the arguments came back as %#v", got[1])
	}

	if out["sessionState"] == "" {
		t.Error("the response carries no sessionState")
	}
}

// TestBatchingKeepsGoing: a failing call does not stop the ones after
// it; that is what makes a batch worth sending. The failing one here is
// a method the server has but the request did not ask for the
// capability of, which RFC 8620 3.2 says is unknownMethod.
func TestBatchingKeepsGoing(t *testing.T) {
	s := newTestServer(t)

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[
		["Core/echo",{"n":1},"c1"],
		["Mailbox/get",{},"c2"],
		["Core/echo",{"n":2},"c3"]]}`)

	list := responses(t, out)
	if len(list) != 3 {
		t.Fatalf("got %d responses, want one per call", len(list))
	}

	second, _ := list[1].([]any)
	if second[0] != "error" {
		t.Errorf("the unimplemented method answered %v, want error", second[0])
	}

	args, _ := second[1].(map[string]any)
	if args["type"] != errUnknownMethod {
		t.Errorf("the error is %v, want %s", args["type"], errUnknownMethod)
	}

	third, _ := list[2].([]any)
	if third[0] != "Core/echo" {
		t.Errorf("the call after the failing one answered %v", third[0])
	}
}

// TestBackReference: an argument taken from an earlier response is
// resolved before the method runs.
func TestBackReference(t *testing.T) {
	s := newTestServer(t)

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[
		["Core/echo",{"list":[{"id":"a"},{"id":"b"}]},"c1"],
		["Core/echo",{"#ids":{"resultOf":"c1","name":"Core/echo","path":"/list/*/id"}},"c2"]]}`)

	list := responses(t, out)

	second, _ := list[1].([]any)
	if second[0] != "Core/echo" {
		t.Fatalf("the referencing call answered %v: %#v", second[0], second[1])
	}

	args, _ := second[1].(map[string]any)

	ids, ok := args["ids"].([]any)
	if !ok || len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Errorf("the reference resolved to %#v, want the two ids", args["ids"])
	}

	if _, still := args["#ids"]; still {
		t.Error("the reference was left in the arguments")
	}
}

// TestBackReferenceFailures: a reference that does not resolve, and an
// argument given twice, are refused rather than guessed at.
func TestBackReferenceFailures(t *testing.T) {
	s := newTestServer(t)

	cases := map[string]string{
		errInvalidResultReference: `["Core/echo",{"#x":{"resultOf":"nope","name":"Core/echo","path":"/a"}},"c2"]`,
		errInvalidArguments:       `["Core/echo",{"x":1,"#x":{"resultOf":"c1","name":"Core/echo","path":"/n"}},"c2"]`,
	}

	for want, call2 := range cases {
		out := call(t, s, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[
			["Core/echo",{"n":1},"c1"],`+call2+`]}`)

		list := responses(t, out)
		second, _ := list[1].([]any)
		args, _ := second[1].(map[string]any)

		if second[0] != "error" || args["type"] != want {
			t.Errorf("got %v/%v, want the error %s", second[0], args["type"], want)
		}
	}
}

// TestRequestLevelErrors: a malformed request, an unknown capability and
// a request over a limit end the whole request with a problem document.
func TestRequestLevelErrors(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Limits.MaxCallsInRequest = 2 })

	cases := []struct {
		body   string
		urn    string
		status int
	}{
		{`not json`, errURNNotJSON, http.StatusBadRequest},
		{`{"methodCalls":[]}`, errURNNotRequest, http.StatusBadRequest},
		{`{"using":["urn:ietf:params:jmap:calendars"],"methodCalls":[]}`, errURNUnknownCapability, http.StatusBadRequest},
		{`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[
			["Core/echo",{},"a"],["Core/echo",{},"b"],["Core/echo",{},"c"]]}`, errURNLimit, http.StatusBadRequest},
	}

	for _, c := range cases {
		w := do(t, s, http.MethodPost, PathAPI, c.body)
		if w.Code != c.status {
			t.Errorf("%.30s answered %d, want %d", c.body, w.Code, c.status)
		}

		var p problem
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("%.30s: %v", c.body, err)
		}

		if p.Type != c.urn {
			t.Errorf("%.30s gave %s, want %s", c.body, p.Type, c.urn)
		}
	}
}

// TestRequestTooLarge: a body above maxSizeRequest is refused with the
// limit named, so the client knows which one it hit.
func TestRequestTooLarge(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Limits.MaxSizeRequest = 64 })

	w := do(t, s, http.MethodPost, PathAPI,
		`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{"pad":"`+strings.Repeat("x", 200)+`"},"c1"]]}`)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized request answered %d", w.Code)
	}

	var p problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}

	if p.Type != errURNLimit || p.Limit != "maxSizeRequest" {
		t.Errorf("the problem is %s/%s, want the maxSizeRequest limit", p.Type, p.Limit)
	}
}

// TestUploadAndDownload: what was uploaded comes back byte for byte,
// including a byte range, which is what RFC 9404 needs underneath.
func TestUploadAndDownload(t *testing.T) {
	s := newTestServer(t)

	payload := "Subject: one\r\n\r\n" + strings.Repeat("body ", 100)

	r := httptest.NewRequest(http.MethodPost, PathUpload+"test/", strings.NewReader(payload))
	r.SetBasicAuth("test", "secret")
	r.Header.Set("Content-Type", "message/rfc822")

	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("the upload answered %d: %s", w.Code, w.Body.String())
	}

	var up uploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}

	if up.Size != int64(len(payload)) || up.Type != "message/rfc822" || up.BlobID == "" {
		t.Fatalf("the upload was recorded as %+v", up)
	}

	got := do(t, s, http.MethodGet, PathDownload+"test/"+up.BlobID+"/one.eml", "")
	if got.Code != http.StatusOK || got.Body.String() != payload {
		t.Fatalf("the download answered %d with %d bytes", got.Code, got.Body.Len())
	}

	rr := httptest.NewRequest(http.MethodGet, PathDownload+"test/"+up.BlobID+"/one.eml", nil)
	rr.SetBasicAuth("test", "secret")
	rr.Header.Set("Range", "bytes=8-11")

	ranged := httptest.NewRecorder()
	s.ServeHTTP(ranged, rr)

	if ranged.Code != http.StatusPartialContent || ranged.Body.String() != payload[8:12] {
		t.Errorf("the range answered %d with %q", ranged.Code, ranged.Body.String())
	}
}

// TestDownloadRefusesAnotherAccount: the account in the path has to be
// the one the request authenticated as; other accounts are JMAP Sharing,
// which this server does not offer yet.
func TestDownloadRefusesAnotherAccount(t *testing.T) {
	s := newTestServer(t)

	w := do(t, s, http.MethodGet, PathDownload+"someone/00000000000000000000000000000001/x", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("a download from another account answered %d, want 404", w.Code)
	}

	w = do(t, s, http.MethodGet, PathDownload+"test/00000000000000000000000000000001/x", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("a download of a blob that is not there answered %d, want 404", w.Code)
	}
}

// TestUploadTooLarge: an upload above maxSizeUpload is refused with the
// limit named.
func TestUploadTooLarge(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Limits.MaxSizeUpload = 16 })

	r := httptest.NewRequest(http.MethodPost, PathUpload+"test/", bytes.NewReader(make([]byte, 64)))
	r.SetBasicAuth("test", "secret")

	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an oversized upload answered %d", w.Code)
	}

	var p problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}

	if p.Limit != "maxSizeUpload" {
		t.Errorf("the problem names the limit %q, want maxSizeUpload", p.Limit)
	}
}

// TestBlobTypeIsNotInjectable: a media type the client made up ends up
// in a response header, so anything that is not one is replaced.
func TestBlobTypeIsNotInjectable(t *testing.T) {
	for _, bad := range []string{"", "text", "text/html\r\nX-Evil: 1", "text/html\nX-Evil: 1", "a/b\x00", strings.Repeat("a/b", 200)} {
		if got := blobType(bad); got != defaultBlobType {
			t.Errorf("blobType(%q) = %q, want the default", bad, got)
		}
	}

	for _, good := range []string{"message/rfc822", "text/plain; charset=utf-8", `text/html; charset="utf-8"`, "application/vnd.oasis.opendocument.text"} {
		if got := blobType(good); got != good {
			t.Errorf("blobType(%q) = %q, want it unchanged", good, got)
		}
	}
}

// tokenAuth stands in for a backend that resolves a bearer token to an
// account on its own, which is what nauthilus does in introspect mode:
// the request carries no user name, only the token.
type tokenAuth struct{ token, account string }

func (a tokenAuth) Authenticate(_ context.Context, r auth.Request) (*auth.Result, error) {
	if r.Token != a.token {
		return nil, auth.ErrInvalid
	}

	return &auth.Result{Account: a.account}, nil
}

func (a tokenAuth) Lookup(_ context.Context, _, _ string, _ net.Addr) (*auth.Result, error) {
	return nil, auth.ErrInvalid
}

// TestBearerCredentials: a bearer token reaches the backend as a token,
// with no user name, and the account it resolves to is the one the
// request runs as. An empty or malformed header is not a credential.
func TestBearerCredentials(t *testing.T) {
	s := newTestServer(t, func(o *Options) { o.Auth = tokenAuth{token: "tok", account: "test"} })

	r := httptest.NewRequest(http.MethodGet, PathSession, nil)
	r.Header.Set("Authorization", "Bearer tok")

	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("a bearer token answered %d: %s", w.Code, w.Body.String())
	}

	var got Session
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}

	if got.Username != "test" {
		t.Errorf("the token resolved to %q, want test", got.Username)
	}

	for _, header := range []string{"Bearer", "Bearer ", "Basic", "Digest abc", "secret"} {
		r := httptest.NewRequest(http.MethodGet, PathSession, nil)
		r.Header.Set("Authorization", header)

		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("the header %q answered %d, want 401", header, w.Code)
		}
	}
}

// TestCreatedIdsComeBack: a client that sends the map gets it back, and
// one that does not is not handed an empty one.
func TestCreatedIdsComeBack(t *testing.T) {
	s := newTestServer(t)

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core"],"createdIds":{},
		"methodCalls":[["Core/echo",{},"c1"]]}`)
	if _, ok := out["createdIds"]; !ok {
		t.Error("an empty createdIds map was not returned")
	}

	out = call(t, s, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c1"]]}`)
	if _, ok := out["createdIds"]; ok {
		t.Error("createdIds was returned although none was sent")
	}
}

// TestBackReferenceTakesTheFirstResponse: RFC 8620 3.7 says the first
// response with that call id, and a name that does not match is an
// error rather than a search for another call.
func TestBackReferenceTakesTheFirstResponse(t *testing.T) {
	s := newTestServer(t)

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core"],"methodCalls":[
		["Mailbox/get",{},"c1"],
		["Core/echo",{"n":1},"c1"],
		["Core/echo",{"#x":{"resultOf":"c1","name":"Core/echo","path":"/n"}},"c2"]]}`)

	list := responses(t, out)
	third, _ := list[2].([]any)
	args, _ := third[1].(map[string]any)

	if third[0] != "error" || args["type"] != errInvalidResultReference {
		t.Errorf("the reference to the reused call id gave %v/%v, want %s",
			third[0], args["type"], errInvalidResultReference)
	}
}

// TestNotAJSONRequest: a body that is JSON but not a request is
// notRequest, which RFC 8620 3.6.1 keeps apart from notJSON.
func TestNotAJSONRequest(t *testing.T) {
	s := newTestServer(t)

	cases := map[string]string{
		`[1,2,3]`: errURNNotRequest,
		`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{}]]}`:     errURNNotRequest,
		`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",1,"c1"]]}`: errURNNotRequest,
		`{"using":"core","methodCalls":[]}`:                                            errURNNotRequest,
		`{"using":[`:                                                                   errURNNotJSON,
	}

	for body, want := range cases {
		w := do(t, s, http.MethodPost, PathAPI, body)

		var p problem
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatalf("%.40s: %v", body, err)
		}

		if p.Type != want {
			t.Errorf("%.40s gave %s, want %s", body, p.Type, want)
		}
	}
}

// TestConcurrencyCountersAreReleased: every path through the API and the
// upload gives its slot back, or the account is locked out after a few
// requests.
func TestConcurrencyCountersAreReleased(t *testing.T) {
	s := newTestServer(t, func(o *Options) {
		o.Limits.MaxConcurrentRequests = 1
		o.Limits.MaxConcurrentUpload = 1
	})

	bodies := []string{
		`{"using":["urn:ietf:params:jmap:core"],"methodCalls":[["Core/echo",{},"c1"]]}`,
		`not json`,
		`{"using":["urn:ietf:params:jmap:mail"],"methodCalls":[]}`,
	}

	for i := 0; i < 3; i++ {
		for _, b := range bodies {
			if w := do(t, s, http.MethodPost, PathAPI, b); w.Code == http.StatusTooManyRequests {
				t.Fatalf("round %d: the account was locked out after a released request", i)
			}
		}

		if w := do(t, s, http.MethodPost, PathUpload+"test/", "payload"); w.Code != http.StatusCreated {
			t.Fatalf("round %d: the upload answered %d", i, w.Code)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.inFlong) != 0 || len(s.inFlup) != 0 {
		t.Errorf("slots left behind: api=%v upload=%v", s.inFlong, s.inFlup)
	}
}

// TestDownloadRangePastTheEnd: a range that starts beyond the blob is
// refused with 416, not served as empty content.
func TestDownloadRangePastTheEnd(t *testing.T) {
	s := newTestServer(t)

	w := do(t, s, http.MethodPost, PathUpload+"test/", "0123456789")

	var up uploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, PathDownload+"test/"+up.BlobID+"/x", nil)
	r.SetBasicAuth("test", "secret")
	r.Header.Set("Range", "bytes=50-60")

	got := httptest.NewRecorder()
	s.ServeHTTP(got, r)

	if got.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("a range past the end answered %d, want 416", got.Code)
	}
}

// TestDownloadHeaders: the media type the client asked for is served,
// but never as a document of this origin, and the file name survives
// characters that would otherwise break the header.
func TestDownloadHeaders(t *testing.T) {
	s := newTestServer(t)

	w := do(t, s, http.MethodPost, PathUpload+"test/", "<p>hi</p>")

	var up uploadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &up); err != nil {
		t.Fatal(err)
	}

	got := do(t, s, http.MethodGet,
		PathDownload+"test/"+up.BlobID+"/re%20port%3B.html?accept=text%2Fhtml", "")

	if ct := got.Header().Get("Content-Type"); ct != "text/html" {
		t.Errorf("Content-Type is %q, want the requested text/html", ct)
	}

	if got.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the download may be sniffed")
	}

	cd := got.Header().Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || strings.ContainsAny(cd, "\r\n") {
		t.Errorf("Content-Disposition is %q", cd)
	}

	if !strings.Contains(cd, "re%20port%3B.html") {
		t.Errorf("Content-Disposition does not carry the escaped name: %q", cd)
	}
}

// TestEventSourceSaysItIsNotThere: the session has to advertise the URL,
// so the URL answers in JMAP terms instead of a bare 404.
func TestEventSourceSaysItIsNotThere(t *testing.T) {
	s := newTestServer(t)

	w := do(t, s, http.MethodGet, PathEvents, "")
	if w.Code != http.StatusNotImplemented {
		t.Errorf("the event source answered %d, want 501", w.Code)
	}
}
