package jmap

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"shardpost/internal/model"
	"shardpost/internal/telemetry"
)

// Tracing: one span per HTTP request (jmap.session, jmap.api,
// jmap.upload, jmap.download) and, below jmap.api, one per method call
// (jmap.<Method>, e.g. jmap.Email/query). The store views the request
// opens hang their operations below the span of the call that runs them
// (model.RequestScoped); the store creates spans only below a sampled
// parent. The event source is not traced: it stays open for hours, and a
// span that long tells nothing. Attributes carry the account and the
// method, never a credential, a filter or anything of a message.

// Span attributes of the front-end.
const (
	attrUser   = "jmap.user"
	attrMethod = "jmap.method"
	attrAcct   = "jmap.account"
	attrError  = "jmap.error"
)

// statusRecorder remembers the status code of a response for the span.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// WriteHeader implements http.ResponseWriter.
func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}

	r.ResponseWriter.WriteHeader(code)
}

// Write implements http.ResponseWriter.
func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}

	return r.ResponseWriter.Write(b)
}

// Unwrap gives http.ResponseController the writer underneath.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// traced runs a handler in a span of its own. The span starts before
// authentication, so a refused request is traced as well; the account is
// added once it is known.
func traced(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := telemetry.Span(r.Context(), name,
			attribute.String("http.method", r.Method), attribute.String("net.peer", clientIP(r)))
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w}
		next(rec, r.WithContext(ctx))

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}

		span.SetAttributes(attribute.Int("http.status_code", status))

		if status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	}
}

// tracedUser names the authenticated account on the request's span.
func tracedUser(ctx context.Context, account string) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String(attrUser, account))
}

// callTrace is the span of one method call, and the store views it
// pointed at itself.
type callTrace struct {
	span  trace.Span
	sess  *session
	outer context.Context
}

// startCall starts the span of one method call. The account is added
// when it is known (attach): naming it can fail, and that failure is
// the call's too.
func startCall(ctx context.Context, name string) (context.Context, *callTrace) {
	cctx, span := telemetry.Span(ctx, "jmap."+name, attribute.String(attrMethod, name))

	return cctx, &callTrace{span: span, outer: ctx}
}

// attach names the account of the call on its span and points the store
// views of the call at it.
func (c *callTrace) attach(ctx context.Context, sess *session) {
	c.sess = sess
	c.span.SetAttributes(attribute.String(attrAcct, sess.accountID()))

	for _, u := range sess.storeViews() {
		model.UseRequest(ctx, u)
	}

	// A namespace the call opens later starts below the call as well.
	if sess.views != nil && sess.views.ctx != nil {
		sess.views.ctx = ctx
	}
}

// end ends the span with the outcome of the call and points the views
// back at the request.
func (c *callTrace) end(merr *methodError) {
	if merr != nil {
		c.span.SetAttributes(attribute.String(attrError, merr.Type))

		if serverError(merr.Type) {
			c.span.SetStatus(codes.Error, merr.Type)
		}
	}

	c.span.End()

	if c.sess == nil {
		return
	}

	for _, u := range c.sess.storeViews() {
		model.UseRequest(c.outer, u)
	}

	if c.sess.views != nil && c.sess.views.ctx != nil {
		c.sess.views.ctx = c.outer
	}
}

// serverError reports whether a method error is the server's failure
// rather than the client's.
func serverError(t string) bool {
	return t == errServerFail || t == errServerUnavailable || t == errServerPartialFail
}

// storeViews are the store views a method call can work on: the
// personal one, the one of the account it was opened for, and every
// other namespace the request opened.
func (s *session) storeViews() []model.User {
	out := []model.User{s.user}

	if s.own != nil && s.own != s.user {
		out = append(out, s.own)
	}

	if s.views != nil {
		for _, u := range s.views.open {
			if u != s.user {
				out = append(out, u)
			}
		}
	}

	return out
}
