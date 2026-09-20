//go:build tentacle

package cephstore

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"shardpost/internal/telemetry"
)

// recordSpans makes the process tracer record into memory for one test.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	rec := tracetest.NewSpanRecorder()

	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	old := otel.GetTracerProvider()

	otel.SetTracerProvider(tp)
	telemetry.Tracer = tp.Tracer("shardpost")

	t.Cleanup(func() {
		otel.SetTracerProvider(old)
		telemetry.Tracer = old.Tracer("shardpost")
	})

	return rec
}

// TestStoreSpansJoinTheCommand: a store operation becomes a child of the
// span the protocol layer set, and a failure is recorded on it.
func TestStoreSpansJoinTheCommand(t *testing.T) {
	rec := recordSpans(t)
	u := &User{}

	ctx, command := telemetry.Span(context.Background(), "imap.FETCH")
	u.UseContext(ctx)

	u.trace(spanBlobRead, attrGUID("abc"))(nil)
	u.trace(spanFTSIndex)(errors.New("index unavailable"))
	command.End()

	// Back to no command: the store still works, its span is a root.
	u.UseContext(context.Background())
	u.trace(spanEntries)(nil)

	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range rec.Ended() {
		byName[s.Name()] = s
	}

	parent := byName["imap.FETCH"].SpanContext().SpanID()

	for _, name := range []string{spanBlobRead, spanFTSIndex} {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("no span %q", name)
		}

		if s.Parent().SpanID() != parent {
			t.Errorf("%s is not a child of the command", name)
		}
	}

	if s := byName[spanFTSIndex]; len(s.Events()) == 0 || s.Status().Code.String() != "Error" {
		t.Errorf("failed operation not recorded: %+v", s.Status())
	}

	if byName[spanEntries].Parent().IsValid() {
		t.Error("span outside a command should be a root span")
	}
}
