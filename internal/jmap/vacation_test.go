package jmap

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/sieve"
)

// vacationServer is a front-end that offers the VacationResponse, over a
// store the test keeps a handle on.
func vacationServer(t *testing.T) (*Server, model.User) {
	t.Helper()

	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	return newTestServer(t, func(o *Options) {
		o.Store = store
		o.VacationResponse = true
	}), u
}

// vacationCall is callWith with the vacation response capability.
func vacationCall(t *testing.T, s *Server, name, args string) map[string]any {
	t.Helper()

	return callWith(t, s, CapVacationResponse, name, args)
}

// vacationObject is the singleton as VacationResponse/get returns it.
func vacationObject(t *testing.T, s *Server) (map[string]any, string) {
	t.Helper()

	res := vacationCall(t, s, "VacationResponse/get", `{"accountId":"test"}`)

	list, _ := res["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("get %#v", res)
	}

	return list[0].(map[string]any), res["state"].(string)
}

// jmapScript is the account's vacation script, "" for none.
func jmapScript(t *testing.T, u model.User) string {
	t.Helper()

	text, err := u.GetScript(sieve.JMAPVacationScript)
	if errors.Is(err, model.ErrNoScript) {
		return ""
	}

	if err != nil {
		t.Fatal(err)
	}

	return text
}

// TestVacationResponseRoundTrip: the default is off; a change is kept,
// read back, and written out as the vacation script, which is never
// the active one; turning it off removes the script.
func TestVacationResponseRoundTrip(t *testing.T) {
	s, u := vacationServer(t)

	obj, state := vacationObject(t, s)
	if obj[propID] != vacationID || obj[propIsEnabled] != false || obj[propSubject] != nil || obj[propFromDate] != nil {
		t.Fatalf("default %#v", obj)
	}

	set := vacationCall(t, s, "VacationResponse/set", `{"accountId":"test","ifInState":"`+state+`","update":{"singleton":{
		"isEnabled":true,"fromDate":"2026-10-01T00:00:00Z","toDate":"2026-10-15T00:00:00+02:00",
		"subject":"Away \"really\"","textBody":"Back on the 15th.","htmlBody":null}}}`)
	if _, ok := set["updated"].(map[string]any)[vacationID]; !ok || set["newState"] == state {
		t.Fatalf("set %#v", set)
	}

	obj, _ = vacationObject(t, s)
	if obj[propIsEnabled] != true || obj[propToDate] != "2026-10-14T22:00:00Z" || obj[propSubject] != `Away "really"` {
		t.Errorf("after the set %#v", obj)
	}

	checkVacationScript(t, u)

	vacationCall(t, s, "VacationResponse/set", `{"accountId":"test","update":{"singleton":{"isEnabled":false}}}`)

	if script := jmapScript(t, u); script != "" {
		t.Errorf("the script stays when off: %s", script)
	}

	if obj, _ = vacationObject(t, s); obj[propSubject] != `Away "really"` {
		t.Errorf("turning it off lost the text %#v", obj)
	}
}

// checkVacationScript checks the script the round trip wrote: the
// setting, quoted; compiling; not active.
func checkVacationScript(t *testing.T, u model.User) {
	t.Helper()

	script := jmapScript(t, u)
	if !strings.Contains(script, `:subject "Away \"really\""`) || !strings.Contains(script, "2026-10-14T22:00:00+00:00") {
		t.Errorf("script %s", script)
	}

	if err := sieve.Compile(script); err != nil {
		t.Errorf("the script does not compile: %v", err)
	}

	if _, active, _ := u.ListScripts(); active != "" {
		t.Errorf("the vacation became the active script %q", active)
	}
}

// TestVacationResponseDefaultBody: a reply without a body gets the
// server's text rather than an empty one.
func TestVacationResponseDefaultBody(t *testing.T) {
	s, u := vacationServer(t)

	vacationCall(t, s, "VacationResponse/set", `{"accountId":"test","update":{"singleton":{"isEnabled":true}}}`)

	if script := jmapScript(t, u); !strings.Contains(script, defaultVacationText) {
		t.Errorf("script %s", script)
	}
}

// TestVacationResponseRefuses: the singleton is not created or
// destroyed, other ids are not found, and a bad value changes nothing.
func TestVacationResponseRefuses(t *testing.T) {
	s, u := vacationServer(t)

	set := vacationCall(t, s, "VacationResponse/set", `{"accountId":"test",
		"create":{"a":{"isEnabled":true}},"destroy":["singleton"],"update":{"other":{"isEnabled":true}}}`)

	if setErrorType(set, "notCreated", "a") != setSingleton || setErrorType(set, "notDestroyed", vacationID) != setSingleton ||
		setErrorType(set, "notUpdated", "other") != setNotFound {
		t.Errorf("set %#v", set)
	}

	for _, patch := range []string{
		`{"isEnabled":true,"fromDate":"tomorrow"}`,
		`{"isEnabled":true,"fromDate":"2026-10-15T00:00:00Z","toDate":"2026-10-01T00:00:00Z"}`,
		`{"isEnabled":true,"subject":"two\r\nlines"}`,
		`{"isEnabled":true,"textBody":"` + strings.Repeat("x", maxVacationBody+1) + `"}`,
		`{"isEnabled":true,"textBody":"nul\u0000"}`,
		`{"isEnabled":"yes"}`,
		`{"isEnabled":true,"id":"other"}`,
		`{"isEnabled":true,"body":"x"}`,
	} {
		set = vacationCall(t, s, "VacationResponse/set", `{"accountId":"test","update":{"singleton":`+patch+`}}`)
		if got := setErrorType(set, "notUpdated", vacationID); got != setInvalidProperties {
			t.Errorf("%s gave %q", patch, got)
		}
	}

	if script := jmapScript(t, u); script != "" {
		t.Errorf("a refused change wrote a script: %s", script)
	}

	res := vacationCall(t, s, "VacationResponse/get", `{"accountId":"test","ids":["singleton","other"],"properties":["isEnabled"]}`)
	if nf, _ := res["notFound"].([]any); len(nf) != 1 || nf[0] != "other" || len(res["list"].([]any)[0].(map[string]any)) != 2 {
		t.Errorf("get %#v", res)
	}

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:vacationresponse"],
		"methodCalls":[["VacationResponse/set",{"accountId":"test","ifInState":"old","update":{"singleton":{"isEnabled":true}}},"c1"]]}`)
	if got := responses(t, out)[0].([]any)[1].(map[string]any)["type"]; got != errStateMismatch {
		t.Errorf("a stale ifInState gave %v", got)
	}
}

// TestVacationResponseNeedsARelay: without a relay the capability is not
// in the session and the methods are unknown; with one it is, and a
// method is only reachable through it.
func TestVacationResponseNeedsARelay(t *testing.T) {
	session := func(s *Server) Session {
		var got Session
		if err := json.Unmarshal(do(t, s, http.MethodGet, PathSession, "").Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}

		return got
	}

	off := newTestServer(t)
	if _, ok := session(off).Capabilities[CapVacationResponse]; ok {
		t.Error("offered without a relay")
	}

	out := call(t, off, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:vacationresponse"],
		"methodCalls":[["VacationResponse/get",{"accountId":"test"},"c1"]]}`)
	if got := responses(t, out)[0].([]any)[1].(map[string]any)["type"]; got != errUnknownMethod {
		t.Errorf("without a relay VacationResponse/get gave %v", got)
	}

	on, _ := vacationServer(t)

	got := session(on)
	if string(got.Capabilities[CapVacationResponse]) != `{}` || got.PrimaryAccounts[CapVacationResponse] != "test" ||
		string(got.Accounts["test"].AccountCapabilities[CapVacationResponse]) != `{}` {
		t.Errorf("session %#v", got)
	}

	if got := mailError(t, on, "VacationResponse/get", `{"accountId":"test"}`); got != errUnknownMethod {
		t.Errorf("without the capability in using: %s", got)
	}
}
