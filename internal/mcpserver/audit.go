package mcpserver

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Request results of the transport audit and the request metric.
const (
	resultOK           = "ok"
	resultToolError    = "tool_error"
	resultUnauthorized = "unauthorized"
	resultForbidden    = "forbidden"
	resultRejected     = "rejected"
	resultRateLimited  = "rate_limited"
	resultBusy         = "busy"
	resultError        = "error"
)

// Results are every value of the result label, for the series that
// exist from the start.
var Results = []string{resultOK, resultToolError, resultUnauthorized, resultForbidden, resultRejected, resultRateLimited,
	resultBusy, resultError}

// labelResult is the label of the request metric.
const labelResult = "result"

// requestsTotal counts endpoint outcomes by a fixed, low-cardinality result label.
var requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{Name: "shardpost_mcp_requests_total",
	Help: "MCP endpoint refused requests and tool calls by result."}, []string{labelResult})

// InitMetrics creates every series of the endpoint with zero.
func InitMetrics() {
	for _, result := range Results {
		requestsTotal.WithLabelValues(result)
	}
}

// accessRecord is one transport audit record. It never carries tokens, arguments, or results.
type accessRecord struct {
	result    string
	reason    string
	tool      string
	principal string
	code      string
	status    int
}

// record counts the outcome and writes the audit record when audit is enabled.
func (s *Server) record(ctx context.Context, entry accessRecord) {
	requestsTotal.WithLabelValues(entry.result).Inc()

	if !s.audit {
		return
	}

	attributes := []slog.Attr{slog.String("event", "mcp_access"), slog.String("result", entry.result),
		slog.Int("status", entry.status)}

	if client := clientIPOf(ctx); client.IsValid() {
		attributes = append(attributes, slog.String("client_ip", client.String()))
	}

	for _, field := range [][2]string{{"reason", entry.reason}, {"tool", entry.tool}, {"principal", entry.principal},
		{"code", entry.code}} {
		if field[1] != "" {
			attributes = append(attributes, slog.String(field[0], field[1]))
		}
	}

	s.logger().LogAttrs(context.WithoutCancel(ctx), slog.LevelInfo, "mcp access", attributes...)
}
