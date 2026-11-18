package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Error codes the endpoint itself answers a tool call with; they use the admin API's error shape.
const (
	codeBadRequest       = "bad_request"
	codeResponseTooLarge = "response_too_large"
	codeInternal         = "internal"
	codeTimeout          = "timeout"

	// wrappedMember holds a list or scalar answer in the structured content object.
	wrappedMember = "result"
)

// errUnauthenticated is a protocol error for a tool call without verified facts; the bearer gate prevents it.
var errUnauthenticated = errors.New("unauthenticated tool call")

// toolError is the admin API's error shape.
type toolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// response is the outcome of one tool call: the admin API's status and body.
type response struct {
	body   []byte
	code   string
	status int
}

// isError reports whether the response is an error.
func (r response) isError() bool { return r.status != http.StatusOK }

// newSDKServer registers every tool as read-only. All tools are listed to every caller; the admin API decides
// authorization per call.
func (s *Server) newSDKServer() *mcp.Server {
	sdkServer := mcp.NewServer(&mcp.Implementation{Name: implementationName, Title: implementationTitle, Version: s.deps.Version},
		&mcp.ServerOptions{Instructions: serverInstructions, SupportedProtocolVersions: supportedProtocolVersions,
			Logger: s.logger()})

	closedWorld, nonDestructive := false, false

	for _, op := range s.tools {
		sdkServer.AddTool(&mcp.Tool{Name: op.name, Title: op.title, Description: op.description,
			InputSchema: json.RawMessage(op.schema), Annotations: &mcp.ToolAnnotations{Title: op.title, ReadOnlyHint: true,
				IdempotentHint: true, DestructiveHint: &nonDestructive, OpenWorldHint: &closedWorld}},
			s.toolHandler(op))
	}

	return sdkServer
}

// toolHandler executes one operation with the verified credential under the call budget. Legacy-era calls run on
// the stateless session context, so the HTTP request context is merged in to stop work for a gone client.
func (s *Server) toolHandler(op operation) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		cred, requestCtx, pending, ok := verifiedFacts(request)
		if !ok {
			return nil, errUnauthenticated
		}

		ctx, stop := mergeCancellation(ctx, requestCtx)
		defer stop()

		ctx, cancel := context.WithTimeout(ctx, s.timeout)
		defer cancel()

		arguments := []byte(request.Params.Arguments)
		if len(arguments) == 0 {
			arguments = []byte("{}")
		}

		result := s.execute(ctx, op, cred, arguments, pending)
		s.recordTool(requestCtx, op.name, cred, result)

		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(result.body)}},
			StructuredContent: json.RawMessage(result.body), IsError: result.isError()}, nil
	}
}

// execute serves one call through the admin API as the credential's caller and bounds the answer. The structured
// content and its text copy together stay below the response bound.
func (s *Server) execute(ctx context.Context, op operation, cred credential, arguments []byte, pending *sync.WaitGroup) response {
	target, err := op.request(arguments)
	if err != nil {
		return errorResponse(http.StatusBadRequest, codeBadRequest, "The arguments do not fit the tool's input schema.")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return errorResponse(http.StatusBadRequest, codeBadRequest, "The arguments do not form a valid request.")
	}

	// The admin API call runs on its own, so that the call budget holds even where the store does not watch the
	// context; the gate slot stays taken until it returned (pending). After the deadline the writer belongs to the
	// abandoned call alone.
	writer := newCapture(int(s.maxResponse / 2))
	done := make(chan struct{})

	pending.Add(1)

	go func() {
		defer pending.Done()
		defer close(done)

		s.deps.Admin.ServeAs(writer, request, cred.caller)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		return errorResponse(http.StatusGatewayTimeout, codeTimeout,
			"The call did not finish within the endpoint's time budget; narrow the request or retry later.")
	}

	return s.render(writer)
}

// render turns the captured admin API answer into the tool result.
func (s *Server) render(writer *capture) response {
	if writer.status >= http.StatusMultipleChoices && writer.status < http.StatusBadRequest {
		return errorResponse(http.StatusBadRequest, codeBadRequest, "The arguments do not name an operation of the tool.")
	}

	if writer.overflow {
		return errorResponse(http.StatusUnprocessableEntity, codeResponseTooLarge,
			"The result exceeds the transport size limit; narrow the request (account, filters, limit).")
	}

	body := bytes.TrimSpace(writer.body.Bytes())
	if !json.Valid(body) {
		return errorResponse(http.StatusInternalServerError, codeInternal, "The admin API answered without JSON.")
	}

	result := response{status: writer.status, body: body}

	if result.isError() {
		var envelope toolError
		if json.Unmarshal(body, &envelope) == nil {
			result.code = envelope.Code
		}
	}

	// Structured content is an object; a list or a scalar answer is wrapped.
	if body[0] != '{' {
		wrapped, _ := json.Marshal(map[string]json.RawMessage{wrappedMember: body})
		result.body = wrapped
	}

	if int64(2*len(result.body)) > s.maxResponse {
		return errorResponse(http.StatusUnprocessableEntity, codeResponseTooLarge,
			"The result exceeds the transport size limit; narrow the request (account, filters, limit).")
	}

	return result
}

// errorResponse renders an error of the endpoint itself in the admin API's error shape.
func errorResponse(status int, code, message string) response {
	body, _ := json.Marshal(toolError{Code: code, Message: message})

	return response{status: status, body: body, code: code}
}

// verifiedFacts returns the credential and the HTTP request context the bearer gate attached to the call.
func verifiedFacts(request *mcp.CallToolRequest) (credential, context.Context, *sync.WaitGroup, bool) {
	if request == nil || request.Extra == nil || request.Extra.TokenInfo == nil {
		return credential{}, nil, nil, false
	}

	extra := request.Extra.TokenInfo.Extra

	cred, ok := extra[extraCredential].(credential)
	pending, held := extra[extraPending].(*sync.WaitGroup)

	if !ok || !held || cred.identity() == "" || cred.caller.Name == "" {
		return credential{}, nil, nil, false
	}

	requestCtx, _ := extra[extraRequestContext].(context.Context)

	return cred, requestCtx, pending, true
}

// mergeCancellation cancels ctx when other ends as well; stop releases the link.
func mergeCancellation(ctx context.Context, other context.Context) (context.Context, func()) {
	if other == nil {
		return ctx, func() {}
	}

	merged, cancel := context.WithCancelCause(ctx)
	unlink := context.AfterFunc(other, func() { cancel(context.Cause(other)) })

	return merged, func() {
		unlink()
		cancel(nil)
	}
}

// recordTool writes the transport record of one tool call.
func (s *Server) recordTool(ctx context.Context, name string, cred credential, result response) {
	if ctx == nil {
		ctx = context.Background()
	}

	outcome := resultOK
	if result.isError() {
		outcome = resultToolError
	}

	s.record(ctx, accessRecord{result: outcome, tool: name, principal: cred.identity(), code: result.code,
		status: result.status})

	if result.status >= http.StatusInternalServerError {
		s.logger().LogAttrs(context.WithoutCancel(ctx), slog.LevelWarn, "MCP tool call failed", slog.String("tool", name),
			slog.String("code", result.code), slog.Int("status", result.status))
	}
}
