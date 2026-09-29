package jmap

import (
	"context"
	"net"
	"testing"

	"shardpost/internal/auth"
	"shardpost/internal/memstore"
)

// identityAuth authenticates like auth.Static and answers the identity
// lookup with fixed attributes.
type identityAuth struct {
	auth.Static
	attrs map[string][]string
	// client records the client address of the last lookup.
	client *string
}

func (a identityAuth) Lookup(_ context.Context, address, _ string, client net.Addr) (*auth.Result, error) {
	if a.client != nil {
		*a.client = ""
		if client != nil {
			*a.client = client.String()
		}
	}

	return &auth.Result{Account: address, Attributes: a.attrs, DisplayName: "Test User"}, nil
}

// identityServer is a front-end with the submission capability.
func identityServer(t *testing.T) *Server {
	t.Helper()

	return identityServerRecording(t, nil)
}

// identityServerRecording is identityServer whose lookups record the client address in client.
func identityServerRecording(t *testing.T, client *string) *Server {
	t.Helper()

	store := memstore.New()

	return newTestServer(t, func(o *Options) {
		o.Store = store
		o.Auth = identityAuth{Static: auth.Static{"test": "secret"}, client: client,
			attrs: map[string][]string{"mail": {"test@example.org"}, "alias": {"t@example.org", "no-address", "TEST@example.org", "@example.org", "a@b@example.org"}}}
		o.Submission = &SubmissionOptions{AddressAttributes: []string{"mail", "alias"}}
	})
}

// submissionCall is mailCall with the submission capability.
func submissionCall(t *testing.T, s *Server, name, args string) map[string]any {
	t.Helper()

	return callWith(t, s, CapSubmission, name, args)
}

// TestIdentities: one identity per address from the lookup, editable,
// with added ones only for those addresses.
func TestIdentities(t *testing.T) {
	s := identityServer(t)

	res := submissionCall(t, s, "Identity/get", `{"accountId":"test"}`)
	list, _ := res["list"].([]any)

	if len(list) != 2 {
		t.Fatalf("identities %#v", res)
	}

	first := list[0].(map[string]any)
	if first[propEmail] != "test@example.org" || first[propName] != "Test User" || first[propMayDelete] != false {
		t.Errorf("first identity %#v", first)
	}

	id, _ := first[propID].(string)
	state, _ := res["state"].(string)

	set := submissionCall(t, s, "Identity/set", `{"accountId":"test","ifInState":"`+state+`",
		"update":{"`+id+`":{"textSignature":"-- \nTest"}},
		"create":{"a":{"email":"t@example.org","name":"Other"},"b":{"email":"boss@example.org"}},
		"destroy":["`+id+`"]}`)

	if got := setErrorType(set, "notCreated", "b"); got != setForbiddenFrom {
		t.Errorf("a foreign address gave %q", got)
	}

	if got := setErrorType(set, "notDestroyed", id); got != setForbidden {
		t.Errorf("destroying a base identity gave %q", got)
	}

	created, _ := set["created"].(map[string]any)["a"].(map[string]any)
	if created == nil || set["newState"] == state {
		t.Fatalf("set %#v", set)
	}

	res = submissionCall(t, s, "Identity/get", `{"accountId":"test","ids":["`+id+`"]}`)
	if got := res["list"].([]any)[0].(map[string]any)[propTextSignature]; got != "-- \nTest" {
		t.Errorf("signature %#v", got)
	}

	if got := mailError(t, s, "Identity/changes", `{"accountId":"test","sinceState":"`+state+`"}`); got != errUnknownMethod {
		// mailError sends the mail capability only; Identity needs submission.
		t.Errorf("Identity/changes without the submission capability gave %s", got)
	}

	res = submissionCall(t, s, "Identity/set", `{"accountId":"test","destroy":["`+created[propID].(string)+`"]}`)
	if d, _ := res["destroyed"].([]any); len(d) != 1 {
		t.Errorf("destroying an added identity gave %#v", res)
	}
}

// TestIdentityLookupCarriesClientAddress: the identity lookup names the
// JMAP client, like every other lookup of a session, so the backend's
// policy sees the client's address instead of none.
func TestIdentityLookupCarriesClientAddress(t *testing.T) {
	var client string

	s := identityServerRecording(t, &client)

	res := submissionCall(t, s, "Identity/get", `{"accountId":"test"}`)
	if list, _ := res["list"].([]any); len(list) == 0 {
		t.Fatalf("identities %#v", res)
	}

	if client == "" {
		t.Fatal("the identity lookup carried no client address")
	}

	if ip := net.ParseIP(client); ip == nil {
		t.Errorf("the identity lookup carried %q, want the client's IP address", client)
	}
}

// TestNoSubmissionWithoutConfiguration: the capability and its methods
// exist only when configured.
func TestNoSubmissionWithoutConfiguration(t *testing.T) {
	s := newTestServer(t)

	out := call(t, s, `{"using":["urn:ietf:params:jmap:core","urn:ietf:params:jmap:submission"],
		"methodCalls":[["Identity/get",{"accountId":"test"},"c1"]]}`)

	got, _ := responses(t, out)[0].([]any)
	if got[0] != kwError {
		t.Errorf("Identity/get without submission configured answered %v", got)
	}
}
