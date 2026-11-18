package mcpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const (
	methodToolsCall = "tools/call"
	headerMCPMethod = "Mcp-Method"
	headerMCPName   = "Mcp-Name"
)

// rpcEnvelope is the part of a JSON-RPC message the endpoint reads before the SDK.
type rpcEnvelope struct {
	method string
	name   string
}

// envelopeMembers are the member names the endpoint and the SDK both read; the SDK matches them case-sensitively.
var envelopeMembers = []string{"jsonrpc", "id", "method", "params"}

// paramsMembers are the parameter names the endpoint reads before the SDK.
var paramsMembers = []string{"name", "arguments", "_meta"}

// parseEnvelope reads method and tool name with exact, case-sensitive member names like the SDK, and refuses any
// case variant of a member both parsers read, so the inspection can never see another message than the SDK.
func parseEnvelope(body []byte) (rpcEnvelope, bool) {
	var members map[string]json.RawMessage
	if json.Unmarshal(body, &members) != nil || hasCaseVariant(members, envelopeMembers) {
		return rpcEnvelope{}, false
	}

	var envelope rpcEnvelope
	if raw, ok := members["method"]; ok && json.Unmarshal(raw, &envelope.method) != nil {
		return rpcEnvelope{}, false
	}

	raw, ok := members["params"]
	if !ok || envelope.method != methodToolsCall {
		return envelope, true
	}

	var params map[string]json.RawMessage
	if json.Unmarshal(raw, &params) != nil || hasCaseVariant(params, paramsMembers) {
		return rpcEnvelope{}, false
	}

	if rawName, named := params["name"]; named && json.Unmarshal(rawName, &envelope.name) != nil {
		return rpcEnvelope{}, false
	}

	return envelope, true
}

// hasCaseVariant reports whether members contain a name that equals one of names only case-insensitively.
func hasCaseVariant(members map[string]json.RawMessage, names []string) bool {
	for member := range members {
		for _, name := range names {
			if member != name && strings.EqualFold(member, name) {
				return true
			}
		}
	}

	return false
}

// inspectBody reads the bounded body once and refuses what the SDK must never see: oversized bodies, duplicate
// member names, batches, and headers that disagree with the body (2026-07-28 Mcp-Method/Mcp-Name). Every tool needs
// the same scopes, which verify already required, so there is no per-tool scope step-up here.
func (s *Server) inspectBody(writer http.ResponseWriter, request *http.Request) bool {
	if request.Method != http.MethodPost {
		return true
	}

	body, err := io.ReadAll(io.LimitReader(request.Body, s.maxRequest+1))
	if err != nil {
		s.reject(writer, request, http.StatusBadRequest, resultRejected, "unreadable_body")

		return false
	}

	if int64(len(body)) > s.maxRequest {
		s.reject(writer, request, http.StatusRequestEntityTooLarge, resultRejected, "body_too_large")

		return false
	}

	if reason := envelopeDefect(request, body); reason != "" {
		s.reject(writer, request, http.StatusBadRequest, resultRejected, reason)

		return false
	}

	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))

	return true
}

// envelopeDefect returns why a body is refused before the SDK, or "". A body that is not a valid JSON object is left
// to the SDK, which answers it with a JSON-RPC error.
func envelopeDefect(request *http.Request, body []byte) string {
	trimmed := bytes.TrimSpace(body)

	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		return "batch_not_supported"
	case hasDuplicateNames(trimmed):
		return "duplicate_json_names"
	case len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed):
		return ""
	}

	envelope, ok := parseEnvelope(trimmed)
	if !ok {
		return "ambiguous_json_rpc_envelope"
	}

	return headerDefect(request, envelope)
}

// headerDefect returns why the 2026-07-28 Mcp-Method / Mcp-Name headers disagree with the body, or "".
func headerDefect(request *http.Request, envelope rpcEnvelope) string {
	if method := request.Header.Get(headerMCPMethod); method != "" && method != envelope.method {
		return "mcp_method_mismatch"
	}

	if name := request.Header.Get(headerMCPName); envelope.method == methodToolsCall && name != "" && name != envelope.name {
		return "mcp_name_mismatch"
	}

	return ""
}
