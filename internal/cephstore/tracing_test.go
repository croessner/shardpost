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

	// Outside a command the store stays silent (see the test below).
	u.UseContext(context.Background())
	u.trace(spanListEntry)(nil)

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

	// Without personal attributes a failure carries its type, not its text.
	if s := byName[spanFTSIndex]; len(s.Events()) != 0 || s.Status().Code.String() != "Error" || !hasAttr(s, "error.type") {
		t.Errorf("failed operation not recorded: %+v %v", s.Status(), s.Attributes())
	}

	if _, ok := byName[spanListEntry]; ok {
		t.Error("store work outside a command must not produce a span")
	}
}

// TestNoSpansWithoutACommand: a job walking millions of objects must not
// turn every store operation into its own trace.
func TestNoSpansWithoutACommand(t *testing.T) {
	rec := recordSpans(t)
	u := &User{}

	u.trace(spanOmapCAS, attrOID("mbox/1"))(nil)
	u.UseContext(context.Background())
	u.trace(spanBlobRead, attrGUID("abc"))(errors.New("gone"))

	if n := len(rec.Ended()); n != 0 {
		t.Fatalf("%d spans without a command", n)
	}
}

// TestSpanBudget: a job that walks a namespace must not put millions of
// children into one trace; the root span says that it was truncated.
func TestSpanBudget(t *testing.T) {
	rec := recordSpans(t)
	u := &User{}

	ctx, job := telemetry.Span(context.Background(), "job.purge")
	u.UseContext(ctx)

	for range maxSpansPerRequest + 50 {
		u.trace(spanOmapCAS, attrOID("mbox/1"))(nil)
	}

	job.End()

	children := 0
	truncated := false

	for _, s := range rec.Ended() {
		if s.Name() == spanOmapCAS {
			children++
		}

		for _, a := range s.Attributes() {
			if string(a.Key) == "store.spans_truncated" {
				truncated = a.Value.AsBool()
			}
		}
	}

	if children != maxSpansPerRequest || !truncated {
		t.Fatalf("%d children, truncated %v", children, truncated)
	}

	// The next command starts with a fresh budget.
	next, cmd := telemetry.Span(context.Background(), "imap.FETCH")
	u.UseContext(next)
	u.trace(spanBlobRead, attrGUID("abc"))(nil)
	cmd.End()

	for _, s := range rec.Ended() {
		if s.Name() == spanBlobRead {
			return
		}
	}

	t.Fatal("the budget was not reset for the next command")
}

// hasAttr reports whether a span carries an attribute named key.
func hasAttr(s sdktrace.ReadOnlySpan, key string) bool {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return true
		}
	}

	return false
}
