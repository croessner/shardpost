package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// invocation is one entry of methodCalls or methodResponses: the triple
// [name, arguments, callId] (RFC 8620 3.2).
type invocation struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

// UnmarshalJSON reads the triple.
func (i *invocation) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}

	if len(raw) != 3 {
		return fmt.Errorf("a method call has three elements, not %d", len(raw))
	}

	if err := json.Unmarshal(raw[0], &i.Name); err != nil || i.Name == "" {
		return errors.New("a method call needs a name")
	}

	if err := json.Unmarshal(raw[2], &i.CallID); err != nil {
		return fmt.Errorf("method call id: %w", err)
	}

	if len(raw[1]) == 0 || raw[1][0] != '{' {
		return errors.New("method arguments must be an object")
	}

	i.Args = raw[1]

	return nil
}

// MarshalJSON writes the triple.
func (i invocation) MarshalJSON() ([]byte, error) {
	args := i.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}

	return json.Marshal([]any{i.Name, args, i.CallID})
}

// apiRequest is the body of a call to the API endpoint.
type apiRequest struct {
	CreatedIDs  map[string]string `json:"createdIds,omitempty"`
	Using       []string          `json:"using"`
	MethodCalls []invocation      `json:"methodCalls"`
}

// apiResponse is what the API endpoint answers. CreatedIDs is a pointer
// so that a client that sent an empty map gets one back: RFC 8620 3.3
// asks for the map whenever it was given, and an empty one is given.
type apiResponse struct {
	CreatedIDs      *map[string]string `json:"createdIds,omitempty"`
	SessionState    string             `json:"sessionState"`
	MethodResponses []invocation       `json:"methodResponses"`
}

// method is one JMAP method: it gets the decoded arguments and returns
// the response arguments, or a method-level error.
type method struct {
	// capability is the URN a request has to list in "using" before this
	// method may be called.
	capability string
	run        func(ctx context.Context, s *session, args json.RawMessage) (any, *methodError)
	// shared marks the methods that serve the accounts other namespaces
	// share with the user (see account.go). They run with the session of
	// the account their accountId names. Every other method serves the
	// personal account alone and answers accountNotFound for any other.
	shared bool
}

// methods are the methods this server implements. RFC 8621 adds the mail
// data types on top; until they are here, a client that asks for them is
// told unknownMethod rather than being served an empty result.
var methods = map[string]method{
	"Core/echo":            {capability: CapCore, run: coreEcho},
	"PushSubscription/get": {capability: CapCore, run: pushSubscriptionGet},
	"PushSubscription/set": {capability: CapCore, run: pushSubscriptionSet},
	"Identity/get":         {capability: CapSubmission, run: identityGet},
	"Identity/changes":     {capability: CapSubmission, run: identityChanges},
	"Identity/set":         {capability: CapSubmission, run: identitySet},

	"EmailSubmission/get":          {capability: CapSubmission, run: emailSubmissionGet},
	"EmailSubmission/changes":      {capability: CapSubmission, run: emailSubmissionChanges},
	"EmailSubmission/query":        {capability: CapSubmission, run: emailSubmissionQuery},
	"EmailSubmission/queryChanges": {capability: CapSubmission, run: emailSubmissionQueryChanges},
	"EmailSubmission/set":          {capability: CapSubmission, run: emailSubmissionSet},
	"Mailbox/get":                  {capability: CapMail, run: mailboxGet, shared: true},
	"Mailbox/changes":              {capability: CapMail, run: mailboxChanges, shared: true},
	"Mailbox/query":                {capability: CapMail, run: mailboxQuery, shared: true},
	"Mailbox/queryChanges":         {capability: CapMail, run: mailboxQueryChanges, shared: true},
	"Mailbox/set":                  {capability: CapMail, run: mailboxSet, shared: true},
	"Email/get":                    {capability: CapMail, run: emailGet, shared: true},
	"Email/changes":                {capability: CapMail, run: emailChanges, shared: true},
	"Email/query":                  {capability: CapMail, run: emailQueryRun, shared: true},
	"Email/queryChanges":           {capability: CapMail, run: emailQueryChanges, shared: true},
	methodEmailSet:                 {capability: CapMail, run: emailSet, shared: true},
	"Email/copy":                   {capability: CapMail, run: emailCopy},
	"Email/import":                 {capability: CapMail, run: emailImport, shared: true},
	"Email/parse":                  {capability: CapMail, run: emailParse, shared: true},
	"Thread/get":                   {capability: CapMail, run: threadGet, shared: true},
	"Thread/changes":               {capability: CapMail, run: threadChanges, shared: true},
	"Quota/get":                    {capability: CapQuota, run: quotaGet},
	"Quota/changes":                {capability: CapQuota, run: quotaChanges},
	"Quota/query":                  {capability: CapQuota, run: quotaQuery},
	"Quota/queryChanges":           {capability: CapQuota, run: quotaQueryChanges},
	"VacationResponse/get":         {capability: CapVacationResponse, run: vacationGet},
	"VacationResponse/set":         {capability: CapVacationResponse, run: vacationSet},
	"Blob/upload":                  {capability: CapBlob, run: blobUpload},
	"Blob/get":                     {capability: CapBlob, run: blobGet},
	"Blob/lookup":                  {capability: CapBlob, run: blobLookup},
	"SieveScript/get":              {capability: CapSieve, run: sieveScriptGet},
	"SieveScript/set":              {capability: CapSieve, run: sieveScriptSet},
	"SieveScript/query":            {capability: CapSieve, run: sieveScriptQuery},
	"SieveScript/queryChanges":     {capability: CapSieve, run: sieveScriptQueryChanges},
	"SieveScript/validate":         {capability: CapSieve, run: sieveScriptValidate},
}

// knownCapabilities are the URNs a request may list in "using".
var knownCapabilities = map[string]bool{CapCore: true, CapMail: true, CapWebPushVAPID: true, CapSubmission: true,
	CapQuota: true, CapVacationResponse: true, CapBlob: true, CapSieve: true}

// coreEcho returns its arguments unchanged (RFC 8620 4).
func coreEcho(_ context.Context, _ *session, args json.RawMessage) (any, *methodError) {
	return json.RawMessage(args), nil
}

// handleAPI runs one API request: it checks the limits, validates the
// capabilities, then executes the method calls in order, resolving the
// back-references of each against the responses of the ones before it.
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	sess := sessionOf(r)

	if !s.enter(s.inFlong, sess.account, s.o.Limits.MaxConcurrentRequests) {
		writeProblem(w, http.StatusTooManyRequests, errURNLimit,
			"too many requests in flight for this account", "maxConcurrentRequests")

		return
	}
	defer s.leave(s.inFlong, sess.account)

	req, ok := s.readRequest(w, r)
	if !ok {
		return
	}

	// The creation ids a client sends can be referenced by the calls of
	// this request, and the ones those calls hand out go back with the
	// response (RFC 8620 3.3).
	sess.created = req.CreatedIDs
	if sess.created == nil {
		sess.created = map[string]string{}
	}

	out := apiResponse{
		MethodResponses: s.runCalls(r.Context(), sess, req),
		SessionState:    s.session(sess, s.baseURL(r)).State,
	}

	if req.CreatedIDs != nil || len(sess.created) > 0 {
		out.CreatedIDs = &sess.created
	}

	w.Header().Set(fieldContentType, "application/json; charset=utf-8")

	if err := json.NewEncoder(w).Encode(&out); err != nil {
		s.o.Log.Warn("jmap: api response not written", "err", err)
	}
}

// readRequest reads and validates the request body. It answers the
// request-level error itself and reports false when it did.
func (s *Server) readRequest(w http.ResponseWriter, r *http.Request) (*apiRequest, bool) {
	body, err := io.ReadAll(io.LimitReader(r.Body, s.o.Limits.MaxSizeRequest+1))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, errURNNotRequest, "the request could not be read", "")

		return nil, false
	}

	if int64(len(body)) > s.o.Limits.MaxSizeRequest {
		writeProblem(w, http.StatusRequestEntityTooLarge, errURNLimit,
			"the request is larger than maxSizeRequest", "maxSizeRequest")

		return nil, false
	}

	if !json.Valid(body) {
		writeProblem(w, http.StatusBadRequest, errURNNotJSON, "the body is not JSON", "")

		return nil, false
	}

	// From here the body is JSON but may not be a request: a wrong shape
	// is notRequest, which RFC 8620 3.6.1 keeps apart from notJSON.
	var req apiRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, errURNNotRequest, err.Error(), "")

		return nil, false
	}

	return &req, s.validate(w, &req)
}

// validate checks what RFC 8620 3.3 and 3.6.1 require of a request.
func (s *Server) validate(w http.ResponseWriter, req *apiRequest) bool {
	if req.Using == nil || req.MethodCalls == nil {
		writeProblem(w, http.StatusBadRequest, errURNNotRequest,
			"a request needs the properties using and methodCalls", "")

		return false
	}

	for _, urn := range req.Using {
		if !knownCapabilities[urn] {
			writeProblem(w, http.StatusBadRequest, errURNUnknownCapability,
				"this server does not support "+urn, "")

			return false
		}
	}

	if len(req.MethodCalls) > s.o.Limits.MaxCallsInRequest {
		writeProblem(w, http.StatusBadRequest, errURNLimit,
			"the request has more method calls than maxCallsInRequest", "maxCallsInRequest")

		return false
	}

	return true
}

// runCalls executes the method calls in order and returns their
// responses. A call that fails produces an error response and does not
// stop the ones after it: that is what makes batching useful.
func (s *Server) runCalls(ctx context.Context, sess *session, req *apiRequest) []invocation {
	using := map[string]bool{}
	for _, urn := range req.Using {
		using[urn] = true
	}

	sess.using = using

	out := make([]invocation, 0, len(req.MethodCalls))

	for _, call := range req.MethodCalls {
		out = append(out, s.runCall(ctx, sess, using, call, out)...)
	}

	return out
}

// runCall executes one method call against the responses so far.
func (s *Server) runCall(ctx context.Context, sess *session, using map[string]bool,
	call invocation, done []invocation,
) []invocation {
	m, ok := methods[call.Name]
	if !ok || !using[m.capability] {
		return []invocation{errorResponse(call.CallID, errUnknownMethod, "")}
	}

	args, merr := resolveReferences(call.Args, done)
	if merr != nil {
		return []invocation{{Name: kwError, Args: mustRaw(merr), CallID: call.CallID}}
	}

	callSess := sess

	if m.shared {
		if callSess, merr = s.callSession(sess, args); merr != nil {
			return []invocation{{Name: kwError, Args: mustRaw(merr), CallID: call.CallID}}
		}
	}

	res, merr := m.run(ctx, callSess, args)
	if merr != nil {
		return []invocation{{Name: kwError, Args: mustRaw(merr), CallID: call.CallID}}
	}

	out := []invocation{{Name: call.Name, Args: mustRaw(res), CallID: call.CallID}}

	// A method that makes an implicit call of another (EmailSubmission/set
	// and its Email/set, RFC 8621 7.5) returns that call's response after
	// its own, under the same call id.
	if f, ok := res.(followUp); ok {
		for _, inv := range f.followUps() {
			inv.CallID = call.CallID
			out = append(out, inv)
		}
	}

	return out
}

// followUp is implemented by a method result that carries the responses
// of implicit calls.
type followUp interface {
	followUps() []invocation
}

// resultReference is a back-reference argument (RFC 8620 3.7).
type resultReference struct {
	ResultOf string `json:"resultOf"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

// resolveReferences replaces every "#name" argument by the value its
// reference points at in an earlier response. An argument that is given
// both as "#name" and as "name" is a contradiction and is refused.
func resolveReferences(args json.RawMessage, done []invocation) (json.RawMessage, *methodError) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(args, &obj); err != nil {
		return nil, &methodError{Type: errInvalidArguments, Description: err.Error()}
	}

	for key, raw := range obj {
		if !strings.HasPrefix(key, "#") {
			continue
		}

		name := key[1:]
		if _, clash := obj[name]; clash {
			return nil, &methodError{Type: errInvalidArguments,
				Description: "the argument " + name + " is given both directly and as a reference"}
		}

		value, merr := resolveOne(raw, done)
		if merr != nil {
			return nil, merr
		}

		obj[name] = value

		delete(obj, key)
	}

	return mustRaw(obj), nil
}

// resolveOne evaluates one result reference.
func resolveOne(raw json.RawMessage, done []invocation) (json.RawMessage, *methodError) {
	var ref resultReference
	if err := json.Unmarshal(raw, &ref); err != nil {
		return nil, &methodError{Type: errInvalidResultReference, Description: err.Error()}
	}

	source, ok := responseOf(done, ref.ResultOf)
	if !ok || source.Name != ref.Name {
		return nil, &methodError{Type: errInvalidResultReference,
			Description: "no response of " + ref.Name + " with the call id " + ref.ResultOf}
	}

	var decoded any
	if err := json.Unmarshal(source.Args, &decoded); err != nil {
		return nil, &methodError{Type: errInvalidResultReference, Description: err.Error()}
	}

	value, err := resolvePointer(decoded, ref.Path)
	if err != nil {
		return nil, &methodError{Type: errInvalidResultReference, Description: ref.Path}
	}

	return mustRaw(value), nil
}

// responseOf finds the response a reference names: the first one with
// that call id, as RFC 8620 3.7 prescribes. A client that reuses a call
// id gets the first response, and if that one is not of the method the
// reference names, the reference fails rather than silently resolving
// against another call.
func responseOf(done []invocation, callID string) (invocation, bool) {
	for _, r := range done {
		if r.CallID == callID {
			return r, true
		}
	}

	return invocation{}, false
}
