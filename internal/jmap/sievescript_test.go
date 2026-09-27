package jmap

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/sieve"
)

// The scripts of the tests: one that compiles, another one, one that
// does not, and one that compiles for IMAP events but not for delivery.
const (
	scriptArchive  = "require \"fileinto\";\r\nfileinto \"Archive\";\r\n"
	scriptKeep     = "keep;\r\n"
	scriptBroken   = "keep;\r\nfileinto \"Archive\";\r\n"
	scriptIMAPOnly = "require \"imapsieve\";\r\nkeep;\r\n"
)

// sieveUsing is the "using" of the Sieve tests.
const sieveUsing = `"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:sieve","urn:ietf:params:jmap:blob"]`

// sieveServer is a front-end that offers the SieveScript data type, over
// a store the test keeps a handle on.
func sieveServer(t *testing.T, customize ...func(*Options)) (*Server, model.User) {
	t.Helper()

	store := memstore.New()

	u, err := store.User("test")
	if err != nil {
		t.Fatal(err)
	}

	return newTestServer(t, append([]func(*Options){func(o *Options) {
		o.Store = store
		o.Sieve = &SieveOptions{Config: map[string]string{"imapsieve_enabled": "yes"}}
	}}, customize...)...), u
}

// sieveCalls sends a batch of method calls and returns the responses,
// error responses included, as [name, arguments] pairs.
func sieveCalls(t *testing.T, s *Server, calls string) [][2]any {
	t.Helper()

	out := call(t, s, `{`+sieveUsing+`,"methodCalls":[`+calls+`]}`)

	var list [][2]any

	for _, r := range responses(t, out) {
		inv, _ := r.([]any)
		list = append(list, [2]any{inv[0], inv[1]})
	}

	return list
}

// sieveCall sends one method call and returns its arguments, failing the
// test on an error response.
func sieveCall(t *testing.T, s *Server, name, args string) map[string]any {
	t.Helper()

	return sieveOne(t, s, name, args, name)
}

// sieveError sends one method call and returns the error it gave.
func sieveError(t *testing.T, s *Server, name, args string) map[string]any {
	t.Helper()

	return sieveOne(t, s, name, args, kwError)
}

// sieveOne sends one method call and returns the arguments of its
// response, failing the test when the response is not want.
func sieveOne(t *testing.T, s *Server, name, args, want string) map[string]any {
	t.Helper()

	got := sieveCalls(t, s, `["`+name+`",`+args+`,"c1"]`)[0]
	if got[0] != want {
		t.Fatalf("%s answered %v, want %s: %#v", name, got[0], want, got[1])
	}

	res, _ := got[1].(map[string]any)

	return res
}

// scripts is SieveScript/get of every script, keyed by name, and the
// state.
func scripts(t *testing.T, s *Server) (map[string]map[string]any, string) {
	t.Helper()

	res := sieveCall(t, s, "SieveScript/get", `{"accountId":"test"}`)

	return objects(t, res), res["state"].(string)
}

// createScript uploads a script and creates it under a name, and returns
// its id.
func createScript(t *testing.T, s *Server, name, text string) string {
	t.Helper()

	blob := upload(t, s, text, "application/sieve")
	res := sieveCall(t, s, "SieveScript/set", `{"create":{"a":{"name":"`+name+`","blobId":"`+blob+`"}}}`)

	return createdID(t, res, "a")
}

// TestSieveScriptCreateAndActivate: a script is created from an upload
// and activated in the same call; it is ManageSieve's script, and its
// blob is its content, through the download endpoint and Blob/get.
func TestSieveScriptCreateAndActivate(t *testing.T) {
	s, u := sieveServer(t)
	blob := upload(t, s, scriptArchive, "application/sieve")

	res := sieveCall(t, s, "SieveScript/set", `{"create":{"a":{"name":"filter","blobId":"`+blob+`"}},
		"onSuccessActivateScript":"#a"}`)

	created := res["created"].(map[string]any)["a"].(map[string]any)
	if created[propName] != "filter" || created[propIsActive] != true || created[propBlobID] == blob {
		t.Fatalf("created %#v", created)
	}

	if text, _ := u.GetScript("filter"); text != scriptArchive {
		t.Errorf("the store holds %q", text)
	}

	if _, active, _ := u.ListScripts(); active != "filter" {
		t.Errorf("the active script is %q", active)
	}

	got, _ := scripts(t, s)
	if got["filter"][propID] != created[propID] || got["filter"][propBlobID] != created[propBlobID] || got["filter"][propIsActive] != true {
		t.Errorf("get %#v", got["filter"])
	}

	blobID := created[propBlobID].(string)

	w := do(t, s, http.MethodGet, PathDownload+"test/"+blobID+"/filter.siv?accept=application/sieve", "")
	if w.Code != http.StatusOK || w.Body.String() != scriptArchive {
		t.Errorf("the download answered %d: %q", w.Code, w.Body.String())
	}

	data := sieveCall(t, s, "Blob/get", `{"ids":["`+blobID+`"]}`)
	if blobList(t, data)[blobID][propDataAsText] != scriptArchive {
		t.Errorf("Blob/get %#v", data)
	}
}

// TestSieveScriptRefusesContent: what does not compile is invalidSieve,
// with the compiler's message; a missing blob, an empty one and one
// beyond maxSizeScript each have their own error.
func TestSieveScriptRefusesContent(t *testing.T) {
	s, _ := sieveServer(t, func(o *Options) { o.Sieve.MaxScript = 64 })

	broken := upload(t, s, scriptBroken, "application/sieve")
	empty := upload(t, s, "", "application/sieve")
	big := upload(t, s, "keep;"+strings.Repeat(" ", 100), "application/sieve")
	binary := upload(t, s, "keep;\xff", "application/sieve")

	res := sieveCall(t, s, "SieveScript/set", `{"create":{
		"broken":{"name":"a","blobId":"`+broken+`"},
		"empty":{"name":"b","blobId":"`+empty+`"},
		"big":{"name":"c","blobId":"`+big+`"},
		"binary":{"name":"d","blobId":"`+binary+`"},
		"missing":{"name":"e","blobId":"0123456789abcdef0123456789abcdef"},
		"noblob":{"name":"f"},
		"active":{"name":"g","blobId":"`+broken+`","isActive":true},
		"control":{"name":"a\u0007b","blobId":"`+broken+`"}}}`)

	for cid, want := range map[string]string{
		"broken": setInvalidSieve, "empty": setInvalidSieve, "big": setTooLarge, "binary": setInvalidSieve,
		"missing": setBlobNotFound, "noblob": setInvalidProperties, "active": setInvalidProperties, "control": setInvalidProperties,
	} {
		if got := setErrorType(res, "notCreated", cid); got != want {
			t.Errorf("%s gave %q, want %s", cid, got, want)
		}
	}

	e := res["notCreated"].(map[string]any)["broken"].(map[string]any)
	if d, _ := e["description"].(string); !strings.Contains(d, "2:1") {
		t.Errorf("the compile error does not name the line: %q", d)
	}
}

// TestSieveScriptActivation: activating one script deactivates the other,
// deactivating leaves none, nothing is activated when a change of the
// same call failed, and a script that only compiles for IMAP events is
// not made the active one.
func TestSieveScriptActivation(t *testing.T) {
	s, u := sieveServer(t)
	a := createScript(t, s, "a", scriptArchive)
	b := createScript(t, s, "b", scriptKeep)
	imapOnly := createScript(t, s, "imap", scriptIMAPOnly)

	sieveCall(t, s, "SieveScript/set", `{"onSuccessActivateScript":"`+a+`"}`)

	res := sieveCall(t, s, "SieveScript/set", `{"onSuccessActivateScript":"`+b+`"}`)
	hasValues(t, "deactivated", res["updated"].(map[string]any)[a].(map[string]any), map[string]any{propIsActive: false})
	hasValues(t, "activated", res["updated"].(map[string]any)[b].(map[string]any), map[string]any{propIsActive: true})

	res = sieveCall(t, s, "SieveScript/set", `{"destroy":["nothing"],"onSuccessActivateScript":"`+a+`"}`)
	if _, active, _ := u.ListScripts(); active != "b" || len(res["updated"].(map[string]any)) != 0 {
		t.Errorf("a failed /set activated %q: %#v", active, res)
	}

	res = sieveCall(t, s, "SieveScript/set", `{"onSuccessActivateScript":"`+imapOnly+`"}`)
	if _, active, _ := u.ListScripts(); active != "b" || len(res["updated"].(map[string]any)) != 0 {
		t.Errorf("a script that does not compile for delivery became active (%q): %#v", active, res)
	}

	res = sieveCall(t, s, "SieveScript/set", `{"onSuccessDeactivateScript":true}`)
	hasValues(t, "deactivated", res["updated"].(map[string]any)[b].(map[string]any), map[string]any{propIsActive: false})

	if _, active, _ := u.ListScripts(); active != "" {
		t.Errorf("after deactivation %q is active", active)
	}

	// Deactivation comes first when both are asked for.
	sieveCall(t, s, "SieveScript/set", `{"onSuccessDeactivateScript":true,"onSuccessActivateScript":"`+a+`"}`)

	if _, active, _ := u.ListScripts(); active != "a" {
		t.Errorf("deactivate and activate left %q active", active)
	}
}

// TestSieveScriptDestroy: the active script cannot be destroyed until it
// was deactivated in a call of its own; another can.
func TestSieveScriptDestroy(t *testing.T) {
	s, u := sieveServer(t)
	a := createScript(t, s, "a", scriptArchive)
	b := createScript(t, s, "b", scriptKeep)

	sieveCall(t, s, "SieveScript/set", `{"onSuccessActivateScript":"`+a+`"}`)

	res := sieveCall(t, s, "SieveScript/set", `{"destroy":["`+a+`","`+b+`"],"onSuccessDeactivateScript":true}`)
	if got := setErrorType(res, "notDestroyed", a); got != setSieveIsActive {
		t.Errorf("destroying the active script gave %q", got)
	}

	if d, _ := res["destroyed"].([]any); len(d) != 1 || d[0] != b {
		t.Errorf("destroyed %#v", res["destroyed"])
	}

	if _, active, _ := u.ListScripts(); active != "a" {
		t.Errorf("a failed destruction still deactivated: %q active", active)
	}

	sieveCall(t, s, "SieveScript/set", `{"onSuccessDeactivateScript":true}`)

	res = sieveCall(t, s, "SieveScript/set", `{"destroy":["`+a+`"]}`)
	if d, _ := res["destroyed"].([]any); len(d) != 1 {
		t.Errorf("destroy after deactivation %#v", res)
	}

	if names, _, _ := u.ListScripts(); len(names) != 0 {
		t.Errorf("scripts left: %v", names)
	}
}

// TestSieveScriptUpdate: a rename keeps the id, a new content gives a new
// blobId under the same id, and a name another script has is
// alreadyExists with that script's id.
func TestSieveScriptUpdate(t *testing.T) {
	s, u := sieveServer(t)
	a := createScript(t, s, "a", scriptArchive)
	b := createScript(t, s, "b", scriptKeep)
	before, _ := scripts(t, s)

	res := sieveCall(t, s, "SieveScript/set", `{"update":{"`+a+`":{"name":"renamed"}}}`)
	if v, ok := res["updated"].(map[string]any)[a]; !ok || v != nil {
		t.Fatalf("rename %#v", res)
	}

	blob := upload(t, s, scriptKeep+"# changed\r\n", "application/sieve")
	res = sieveCall(t, s, "SieveScript/set", `{"update":{"`+a+`":{"blobId":"`+blob+`","name":"again"}},
		"create":{"x":{"name":"b","blobId":"`+blob+`"}}}`)

	changed, _ := res["updated"].(map[string]any)[a].(map[string]any)
	after, _ := scripts(t, s)

	if after["again"][propID] != a || after["again"][propBlobID] == before["a"][propBlobID] ||
		changed[propBlobID] != after["again"][propBlobID] {
		t.Errorf("after the update %#v, updated %#v", after, res["updated"])
	}

	if text, _ := u.GetScript("again"); text != scriptKeep+"# changed\r\n" {
		t.Errorf("the store holds %q", text)
	}

	e := res["notCreated"].(map[string]any)["x"].(map[string]any)
	if e["type"] != setAlreadyExists || e["existingId"] != b {
		t.Errorf("a second b gave %#v", e)
	}
}

// TestSieveScriptUpdateRefuses: a name another script has is
// alreadyExists with that script's id, and isActive is the server's; a
// creation without a name is named by the server.
func TestSieveScriptUpdateRefuses(t *testing.T) {
	s, _ := sieveServer(t)
	a := createScript(t, s, "a", scriptArchive)
	b := createScript(t, s, "b", scriptKeep)
	blob := upload(t, s, scriptKeep, "application/sieve")

	e := sieveCall(t, s, "SieveScript/set", `{"update":{"`+a+`":{"name":"b"}}}`)["notUpdated"].(map[string]any)[a].(map[string]any)
	if e["type"] != setAlreadyExists || e["existingId"] != b {
		t.Errorf("renaming onto b gave %#v", e)
	}

	res := sieveCall(t, s, "SieveScript/set", `{"update":{"`+a+`":{"isActive":true}},"create":{"n":{"name":null,"blobId":"`+blob+`"}}}`)
	if got := setErrorType(res, "notUpdated", a); got != setInvalidProperties {
		t.Errorf("setting isActive gave %q", got)
	}

	if name, _ := res["created"].(map[string]any)["n"].(map[string]any)[propName].(string); !strings.HasPrefix(name, "script-") {
		t.Errorf("a script without a name was named %q", name)
	}
}

// TestSieveScriptFromManageSieve: a script stored over ManageSieve gets
// an id the first time JMAP lists it, and keeps it; the ids are kept in
// the server-managed metadata entry.
func TestSieveScriptFromManageSieve(t *testing.T) {
	s, u := sieveServer(t)

	if err := u.PutScript("roundcube", scriptKeep); err != nil {
		t.Fatal(err)
	}

	first, state := scripts(t, s)
	second, again := scripts(t, s)

	id, _ := first["roundcube"][propID].(string)
	if id == "" || second["roundcube"][propID] != id || state != again {
		t.Fatalf("the ids are %#v then %#v", first, second)
	}

	md, err := u.GetMetadata("", "test")
	if err != nil {
		t.Fatal(err)
	}

	var ids map[string]string
	if err := json.Unmarshal([]byte(md[sieveScriptsEntry]), &ids); err != nil || ids["roundcube"] != id {
		t.Errorf("the entry holds %q", md[sieveScriptsEntry])
	}

	// A change over ManageSieve moves the state.
	if err := u.PutScript("roundcube", scriptArchive); err != nil {
		t.Fatal(err)
	}

	if _, changed := scripts(t, s); changed == state {
		t.Error("a new content left the state as it was")
	}
}

// TestSieveScriptReserved: the display copy of the VacationResponse is
// shown, never active, and cannot be changed, destroyed, activated or
// created over JMAP.
func TestSieveScriptReserved(t *testing.T) {
	s, u := sieveServer(t)

	if err := u.PutScript(sieve.JMAPVacationScript, scriptKeep); err != nil {
		t.Fatal(err)
	}

	got, _ := scripts(t, s)

	vac := got[sieve.JMAPVacationScript]
	if vac == nil || vac[propIsActive] != false {
		t.Fatalf("the vacation copy is %#v", got)
	}

	id := vac[propID].(string)
	blob := upload(t, s, scriptKeep, "application/sieve")

	res := sieveCall(t, s, "SieveScript/set", `{"update":{"`+id+`":{"name":"mine"}},"destroy":["`+id+`"],
		"create":{"x":{"name":"`+sieve.JMAPVacationScript+`","blobId":"`+blob+`"}}}`)
	if setErrorType(res, "notUpdated", id) != setForbidden || setErrorType(res, "notDestroyed", id) != setForbidden ||
		setErrorType(res, "notCreated", "x") != setForbidden {
		t.Errorf("changes to the vacation copy gave %#v", res)
	}

	mine := createScript(t, s, "mine", scriptKeep)

	res = sieveCall(t, s, "SieveScript/set", `{"update":{"`+mine+`":{"name":"`+sieve.JMAPVacationScript+`"}}}`)
	if got := setErrorType(res, "notUpdated", mine); got != setForbidden {
		t.Errorf("renaming onto the vacation copy gave %q", got)
	}

	e := sieveError(t, s, "SieveScript/set", `{"onSuccessActivateScript":"`+id+`"}`)
	if e["type"] != errForbidden {
		t.Errorf("activating the vacation copy gave %#v", e)
	}

	if _, active, _ := u.ListScripts(); active != "" {
		t.Errorf("%q is active", active)
	}
}

// TestSieveScriptQuery: the name and isActive conditions, with operators,
// the two sort keys, and what is refused.
func TestSieveScriptQuery(t *testing.T) {
	s, _ := sieveServer(t)
	alpha := createScript(t, s, "Alpha", scriptKeep)
	beta := createScript(t, s, "beta", scriptKeep)
	gamma := createScript(t, s, "gamma-filter", scriptKeep)

	sieveCall(t, s, "SieveScript/set", `{"onSuccessActivateScript":"`+beta+`"}`)

	for args, want := range map[string][]string{
		`{}`:                           {alpha, beta, gamma},
		`{"filter":{"name":"A"}}`:      {alpha, beta, gamma},
		`{"filter":{"name":"filter"}}`: {gamma},
		`{"filter":{"isActive":true}}`: {beta},
		`{"filter":{"operator":"NOT","conditions":[{"isActive":true}]},"sort":[{"property":"name","isAscending":false}]}`: {gamma, alpha},
		`{"sort":[{"property":"isActive","isAscending":false},{"property":"name"}]}`:                                      {beta, alpha, gamma},
		`{"sort":[{"property":"name","collation":"i;octet"}]}`:                                                            {alpha, beta, gamma},
	} {
		res := sieveCall(t, s, "SieveScript/query", args)
		if !idsAre(res["ids"], want...) || res["canCalculateChanges"] != false {
			t.Errorf("%s gave %#v, want %v", args, res["ids"], want)
		}
	}

	for args, want := range map[string]string{
		`{"filter":{"blobId":"x"}}`:                     errUnsupportedFilter,
		`{"sort":[{"property":"blobId"}]}`:              errUnsupportedSort,
		`{"filter":{"operator":"XOR","conditions":[]}}`: errUnsupportedFilter,
	} {
		if got := sieveError(t, s, "SieveScript/query", args); got["type"] != want {
			t.Errorf("%s gave %#v, want %s", args, got, want)
		}
	}

	if got := sieveError(t, s, "SieveScript/queryChanges", `{"sinceQueryState":"x"}`); got["type"] != errCannotCalculateChanges {
		t.Errorf("queryChanges gave %#v", got)
	}
}

// TestSieveScriptValidate: CHECKSCRIPT over JMAP — null for a script that
// compiles, invalidSieve for one that does not; nothing is stored.
func TestSieveScriptValidate(t *testing.T) {
	s, u := sieveServer(t)
	good := upload(t, s, scriptArchive, "application/sieve")
	bad := upload(t, s, scriptBroken, "application/sieve")

	if res := sieveCall(t, s, "SieveScript/validate", `{"blobId":"`+good+`"}`); res["error"] != nil {
		t.Errorf("a good script gave %#v", res)
	}

	res := sieveCall(t, s, "SieveScript/validate", `{"blobId":"`+bad+`"}`)
	if e, _ := res["error"].(map[string]any); e == nil || e["type"] != setInvalidSieve {
		t.Errorf("a broken script gave %#v", res)
	}

	if names, _, _ := u.ListScripts(); len(names) != 0 {
		t.Errorf("validate stored %v", names)
	}
}

// TestSieveScriptStateAndLookup: ifInState is checked, and Blob/lookup
// finds the scripts a content belongs to.
func TestSieveScriptStateAndLookup(t *testing.T) {
	s, _ := sieveServer(t)
	a := createScript(t, s, "a", scriptKeep)
	got, state := scripts(t, s)

	if e := sieveError(t, s, "SieveScript/set", `{"ifInState":"old","destroy":["`+a+`"]}`); e["type"] != errStateMismatch {
		t.Errorf("a stale state gave %#v", e)
	}

	res := sieveCall(t, s, "Blob/lookup", `{"typeNames":["SieveScript"],"ids":["`+got["a"][propBlobID].(string)+`","nothing"]}`)
	list := res["list"].([]any)

	if m := list[0].(map[string]any)["matchedIds"].(map[string]any); !idsAre(m[typeSieveScript], a) {
		t.Errorf("lookup %#v", list)
	}

	if m := list[1].(map[string]any)["matchedIds"].(map[string]any); !idsAre(m[typeSieveScript]) {
		t.Errorf("lookup of nothing %#v", list)
	}

	res = sieveCall(t, s, "SieveScript/set", `{"ifInState":"`+state+`","destroy":["`+a+`"]}`)
	if res["oldState"] != state || res["newState"] == state {
		t.Errorf("the states of a destruction %#v", res)
	}
}

// TestSieveScriptCapability: the session names the implementation, the
// account the limits ManageSieve has; without the options the type is
// not there.
func TestSieveScriptCapability(t *testing.T) {
	s, _ := sieveServer(t)

	got := s.session("test", "https://mail.example.org")
	if !strings.HasPrefix(string(got.Capabilities[CapSieve]), `{"implementation":"ShardPost`) || got.PrimaryAccounts[CapSieve] != "test" {
		t.Errorf("the session lists %s", got.Capabilities[CapSieve])
	}

	var acc sieveCapability
	if err := json.Unmarshal(got.Accounts["test"].AccountCapabilities[CapSieve], &acc); err != nil {
		t.Fatal(err)
	}

	if acc.MaxSizeScriptName != model.MaxScriptName || acc.MaxSizeScript != model.MaxScriptSize || acc.MaxNumberScripts != nil ||
		acc.MaxNumberRedirects != sieve.MaxRedirects || !containsString(acc.SieveExtensions, "imapsieve") ||
		!containsString(acc.SieveExtensions, "fileinto") || acc.ExternalLists != nil {
		t.Errorf("the account lists %+v", acc)
	}

	if !strings.Contains(string(got.Accounts["test"].AccountCapabilities[CapBlob]), typeSieveScript) {
		t.Errorf("Blob/lookup does not list SieveScript: %s", got.Accounts["test"].AccountCapabilities[CapBlob])
	}
}

// TestSieveScriptNotOffered: without the options the capability is not
// in the session and the methods are unknown.
func TestSieveScriptNotOffered(t *testing.T) {
	off := newTestServer(t)
	if _, ok := off.session("test", "https://mail.example.org").Capabilities[CapSieve]; ok {
		t.Error("the capability is offered without the options")
	}

	if e := sieveError(t, off, "SieveScript/get", `{}`); e["type"] != errUnknownMethod {
		t.Errorf("without the options SieveScript/get gave %#v", e)
	}
}
