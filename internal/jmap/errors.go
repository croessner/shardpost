package jmap

import (
	"encoding/json"
	"net/http"
)

// Request-level error types (RFC 8620 3.6.1). They are returned as an
// RFC 7807 problem document and end the whole request; nothing in it was
// executed.
const (
	errURNUnknownCapability = "urn:ietf:params:jmap:error:unknownCapability"
	errURNNotJSON           = "urn:ietf:params:jmap:error:notJSON"
	errURNNotRequest        = "urn:ietf:params:jmap:error:notRequest"
	errURNLimit             = "urn:ietf:params:jmap:error:limit"
)

// Method-level error types (RFC 8620 3.6.2). They replace the response
// of one method call and leave the others alone.
const (
	errUnknownMethod           = "unknownMethod"
	errInvalidArguments        = "invalidArguments"
	errInvalidResultReference  = "invalidResultReference"
	errForbidden               = "forbidden"
	errAccountNotFound         = "accountNotFound"
	errAccountNotSupported     = "accountNotSupportedByMethod"
	errServerFail              = "serverFail"
	errServerUnavailable       = "serverUnavailable"
	errServerPartialFail       = "serverPartialFail"
	errRequestTooLarge         = "requestTooLarge"
	errCannotCalculateChanges  = "cannotCalculateChanges"
	errUnsupportedFilter       = "unsupportedFilter"
	errFromAccountNotFound     = "fromAccountNotFound"
	errStateMismatch           = "stateMismatch"
	errCannotCalculateSnapshot = "cannotCalculateSnapshot"
	errUnsupportedSort         = "unsupportedSort"
	errAnchorNotFound          = "anchorNotFound"
	errTooManyChanges          = "tooManyChanges"
	// errUnknownDataType is Blob/lookup's (RFC 9404 4.3): a type name the
	// server does not know, or whose capability the request did not use.
	errUnknownDataType = "unknownDataType"
)

// kwError is the method name of an error response.
const kwError = "error"

// The descriptions of the two ways a /changes cannot compute a delta.
const (
	descUnknownState = "unknown state token"
	descLogTrimmed   = "the change log no longer covers that state"
)

// argProperties is the argument name an invalid property list blames.
const argProperties = "properties"

// problem is the RFC 7807 document a request-level error is reported as.
type problem struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Limit names which limit was exceeded, for errURNLimit.
	Limit string `json:"limit,omitempty"`
}

// writeProblem answers with a request-level error.
func writeProblem(w http.ResponseWriter, status int, urn, detail, limit string) {
	w.Header().Set(fieldContentType, "application/problem+json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(&problem{Type: urn, Status: status, Detail: detail, Limit: limit})
}

// methodError is the arguments object of an "error" method response.
type methodError struct {
	Type        string   `json:"type"`
	Description string   `json:"description,omitempty"`
	Arguments   []string `json:"arguments,omitempty"`
}

// errorResponse builds the "error" response of one method call.
func errorResponse(callID, typ, description string) invocation {
	return invocation{Name: kwError, Args: mustRaw(&methodError{Type: typ, Description: description}), CallID: callID}
}

// mustRaw marshals a value that cannot fail to marshal.
func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		// Only a type this package constructs reaches here; a failure
		// would be a programming error, and an empty object keeps the
		// response well formed rather than truncating it.
		return json.RawMessage(`{}`)
	}

	return b
}
