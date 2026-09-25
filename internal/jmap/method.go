package jmap

import (
	"encoding/json"
	"strconv"
	"strings"
)

// The argument and response shapes RFC 8620 5 gives every data type. The
// mail types fill in what they return; everything about accounts, state
// tokens and paging is the same for all of them and lives here.

// getRequest is the argument shape of a /get (RFC 8620 5.1). A nil IDs
// means "all of them", which is not the same as an empty list.
type getRequest struct {
	IDs        *[]string `json:"ids"`
	Properties *[]string `json:"properties"`
	AccountID  string    `json:"accountId"`
}

// getResponse is what a /get answers.
type getResponse struct {
	AccountID string           `json:"accountId"`
	State     string           `json:"state"`
	List      []map[string]any `json:"list"`
	NotFound  []string         `json:"notFound"`
}

// changesRequest is the argument shape of a /changes (RFC 8620 5.2).
type changesRequest struct {
	MaxChanges *uint64 `json:"maxChanges"`
	AccountID  string  `json:"accountId"`
	SinceState string  `json:"sinceState"`
}

// changesResponse is what a /changes answers. UpdatedProperties says
// that the updated objects changed only in the properties it names; the
// data types that have no such shortcut leave it nil, which RFC 8620 5.2
// reads as "the server cannot tell".
type changesResponse struct {
	UpdatedProperties []string `json:"updatedProperties"`
	AccountID         string   `json:"accountId"`
	OldState          string   `json:"oldState"`
	NewState          string   `json:"newState"`
	Created           []string `json:"created"`
	Updated           []string `json:"updated"`
	Destroyed         []string `json:"destroyed"`
	HasMoreChanges    bool     `json:"hasMoreChanges"`
}

// setRequest is the argument shape of a /set (RFC 8620 5.3).
type setRequest struct {
	Create    map[string]json.RawMessage            `json:"create"`
	Update    map[string]map[string]json.RawMessage `json:"update"`
	IfInState *string                               `json:"ifInState"`
	AccountID string                                `json:"accountId"`
	Destroy   []string                              `json:"destroy"`
}

// setResponse is what a /set answers.
type setResponse struct {
	Created      map[string]map[string]any `json:"created"`
	Updated      map[string]any            `json:"updated"`
	NotCreated   map[string]*setError      `json:"notCreated"`
	NotUpdated   map[string]*setError      `json:"notUpdated"`
	NotDestroyed map[string]*setError      `json:"notDestroyed"`
	AccountID    string                    `json:"accountId"`
	OldState     string                    `json:"oldState"`
	NewState     string                    `json:"newState"`
	Destroyed    []string                  `json:"destroyed"`
}

// newSetResponse returns a response with the maps a client may index
// into; RFC 8620 5.3 lets them be null, but a client that has to test
// for it first is a client that will forget to.
func newSetResponse(account, state string) *setResponse {
	return &setResponse{
		AccountID: account, OldState: state, NewState: state,
		Created:      map[string]map[string]any{},
		Updated:      map[string]any{},
		Destroyed:    []string{},
		NotCreated:   map[string]*setError{},
		NotUpdated:   map[string]*setError{},
		NotDestroyed: map[string]*setError{},
	}
}

// setError is why one object of a /set was not created, updated or
// destroyed (RFC 8620 5.3).
type setError struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Properties  []string `json:"properties,omitempty"`
}

// The set error types this server uses (RFC 8620 5.3, RFC 8621 2.5).
const (
	setNotFound          = "notFound"
	setInvalidProperties = "invalidProperties"
	setForbidden         = "forbidden"
	setServerFail        = "serverFail"
	setMailboxHasEmail   = "mailboxHasEmail"
	setMailboxHasChild   = "mailboxHasChild"
)

// invalidProperty is the error of one property a client got wrong.
func invalidProperty(name, why string) *setError {
	return &setError{Type: setInvalidProperties, Properties: []string{name}, Description: why}
}

// stateString is how an account sequence is written in a state token.
// The token is opaque to a client, and this one is short, ordered and
// readable in a log, which is worth more than hiding it.
func stateString(seq uint64) string { return strconv.FormatUint(seq, 10) }

// parseState reads a state token back. A token this server did not
// write is not an error of its own: the caller answers
// cannotCalculateChanges, which is what a client has to handle anyway.
func parseState(s string) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, 64)

	return n, err == nil
}

// forAccount decodes the arguments and checks that the account the
// client named is the one it authenticated as. Another account is
// accountNotFound rather than forbidden: whether it exists is not this
// account's business.
func forAccount(sess *session, args json.RawMessage, into any) *methodError {
	if err := json.Unmarshal(args, into); err != nil {
		return &methodError{Type: errInvalidArguments, Description: err.Error()}
	}

	if id := accountOf(into); id != "" && id != accountID(sess.account) {
		return &methodError{Type: errAccountNotFound}
	}

	return nil
}

// accountOf reads the accountId out of a decoded argument struct.
func accountOf(v any) string {
	switch a := v.(type) {
	case *getRequest:
		return a.AccountID
	case *changesRequest:
		return a.AccountID
	case *setRequest:
		return a.AccountID
	case *queryRequest:
		return a.AccountID
	}

	return ""
}

// selected reports whether a property is one the client asked for. A
// request without a property list asks for all of them.
func selected(properties *[]string, name string) bool {
	if properties == nil {
		return true
	}

	for _, p := range *properties {
		if p == name {
			return true
		}
	}

	return false
}

// checkProperties refuses a property list naming something the data type
// does not have. RFC 8620 5.1 asks for invalidArguments rather than a
// quietly shorter object, because a client that misspells a property
// would otherwise never find out.
func checkRequested(properties *[]string, known map[string]bool) *methodError {
	if properties == nil {
		return nil
	}

	for _, p := range *properties {
		if !known[p] {
			return &methodError{Type: errInvalidArguments, Arguments: []string{"properties"},
				Description: "no such property: " + p}
		}
	}

	return nil
}

// resolveCreationID turns a "#creationId" reference into the id that
// creation got, and reports whether the reference is still waiting for
// it. A plain id passes through.
func resolveCreationID(sess *session, ref string) (id string, pending bool) {
	if !strings.HasPrefix(ref, "#") {
		return ref, false
	}

	if got, ok := sess.created[ref[1:]]; ok {
		return got, false
	}

	return "", true
}
