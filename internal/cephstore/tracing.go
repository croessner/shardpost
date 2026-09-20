package cephstore

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"shardpost/internal/model"
	"shardpost/internal/telemetry"
)

// Store operations report themselves as spans below the span of the
// command that runs them, so a trace shows where the time of an IMAP
// command or a delivery actually goes: index reads, CAS writes, blob
// access and the full-text index. Without an OTLP endpoint the tracer is
// a no-op and this costs nothing.

var _ model.RequestScoped = (*User)(nil)

// UseContext implements model.RequestScoped.
func (u *User) UseContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	u.traceCtx.Store(&ctx)
	u.spans.Store(0)
}

// reqContext is the context of the command currently running on this
// view, or the background context.
func (u *User) reqContext() context.Context {
	if p := u.traceCtx.Load(); p != nil {
		return *p
	}

	return context.Background()
}

// maxSpansPerRequest bounds the store spans of one command or job. A
// job that walks a whole namespace would otherwise put millions of
// children into a single trace, which the batch processor drops and the
// backend refuses; the root span is marked when that happens.
const maxSpansPerRequest = 1000

// noSpan is the result of trace without a running command.
func noSpan(error) {}

// trace starts a span for one store operation, but only below the span
// of a running command: store work has no trace of its own. A job that
// walks millions of objects would otherwise produce millions of root
// traces, each sampled on its own. The returned function ends the span
// and records a failure; pass nil for success.
func (u *User) trace(name string, attrs ...attribute.KeyValue) func(error) {
	ctx := u.reqContext()

	// Nothing to hang the operation on, or the command is not sampled:
	// then the span would cost allocations and be dropped anyway.
	parent := trace.SpanFromContext(ctx)
	if sc := parent.SpanContext(); !sc.IsValid() || !sc.IsSampled() {
		return noSpan
	}

	switch n := u.spans.Add(1); {
	case n == maxSpansPerRequest+1:
		parent.SetAttributes(attribute.Bool("store.spans_truncated", true))

		return noSpan
	case n > maxSpansPerRequest:
		return noSpan
	}

	_, span := telemetry.Span(ctx, name, attrs...)

	return func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}

		span.End()
	}
}

// Span names of the store.
const (
	spanDeliver    = "store.deliver"
	spanBlobWrite  = "store.blob_write"
	spanBlobRead   = "store.blob_read"
	spanFTSIndex   = "store.fts_index"
	spanFTSSearch  = "store.fts_search"
	spanFlags      = "store.flags"
	spanCopy       = "store.copy"
	spanMove       = "store.move"
	spanExpunge    = "store.expunge"
	spanHeaderRead = "store.header_read"
	spanHeaderCAS  = "store.header_cas"
	spanListEntry  = "store.list_entries"
	spanGetEntry   = "store.get_entries"
	spanOmapCAS    = "rados.omap_cas"
)

// Attribute keys, in one place. Mailbox names stay out of traces: they
// are user content, and the collector filter of the operating profile
// only knows the protocol attributes. Object ids identify a mailbox
// where it matters.
func attrGUID(guid string) attribute.KeyValue { return attribute.String("store.guid", guid) }
func attrCount(n int) attribute.KeyValue      { return attribute.Int("store.count", n) }
func attrBytes(n int) attribute.KeyValue      { return attribute.Int("store.bytes", n) }
func attrOID(oid string) attribute.KeyValue   { return attribute.String("rados.oid", oid) }
