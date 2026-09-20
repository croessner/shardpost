package model

import "context"

// RequestScoped is implemented by user views whose store operations can
// join the trace of the command that runs them. Protocol layers set the
// context of the command they are handling; the store then records its
// operations (index reads, CAS writes, blob access, full-text writes) as
// children of that command's span.
//
// The interface is optional: a store that does not implement it simply
// produces no store-level spans.
type RequestScoped interface {
	// UseContext sets the context of the command currently running on
	// this view. It is called from the view's own goroutine.
	UseContext(ctx context.Context)
}

// UseRequest attaches the context of the running command to u when the
// store supports it. A nil user or a store without tracing support is
// ignored, so callers do not have to check.
func UseRequest(ctx context.Context, u User) {
	if rs, ok := u.(RequestScoped); ok {
		rs.UseContext(ctx)
	}
}
