package cephstore

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

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
}

// reqContext is the context of the command currently running on this
// view, or the background context.
func (u *User) reqContext() context.Context {
	if p := u.traceCtx.Load(); p != nil {
		return *p
	}

	return context.Background()
}

// trace starts a span for one store operation. The returned function
// ends it and records a failure; pass nil for success.
func (u *User) trace(name string, attrs ...attribute.KeyValue) func(error) {
	_, span := telemetry.Span(u.reqContext(), name, attrs...)

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
	spanDeliver   = "store.deliver"
	spanRead      = "store.read"
	spanBlobWrite = "store.blob_write"
	spanBlobRead  = "store.blob_read"
	spanFTSIndex  = "store.fts_index"
	spanFTSSearch = "store.fts_search"
	spanEntries   = "store.entries"
	spanFlags     = "store.flags"
	spanCopy      = "store.copy"
	spanMove      = "store.move"
	spanExpunge   = "store.expunge"
	spanOmapCAS   = "rados.omap_cas"
)

// attrMailbox and friends keep the attribute keys in one place.
func attrMailbox(name string) attribute.KeyValue { return attribute.String("store.mailbox", name) }
func attrGUID(guid string) attribute.KeyValue    { return attribute.String("store.guid", guid) }
func attrCount(n int) attribute.KeyValue         { return attribute.Int("store.count", n) }
func attrBytes(n int) attribute.KeyValue         { return attribute.Int("store.bytes", n) }
func attrOID(oid string) attribute.KeyValue      { return attribute.String("rados.oid", oid) }
