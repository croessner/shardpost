package imapproto

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"shardpost/internal/memstore"
	"shardpost/internal/model"
	"shardpost/internal/telemetry"
)

// tracingStore hands out users that remember the context the protocol
// layer set on them (what a real store uses for its own spans).
type tracingStore struct {
	inner *memstore.Store
	seen  *seenContexts
}

type seenContexts struct {
	mu  sync.Mutex
	ids []trace.SpanContext
}

func (s *seenContexts) add(sc trace.SpanContext) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sc.IsValid() {
		s.ids = append(s.ids, sc)
	}
}

func (s *seenContexts) all() []trace.SpanContext {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]trace.SpanContext{}, s.ids...)
}

func (s tracingStore) User(name string) (model.User, error) {
	u, err := s.inner.User(name)
	if err != nil {
		return nil, err
	}

	return &tracingUser{User: u, seen: s.seen}, nil
}

type tracingUser struct {
	model.User
	seen *seenContexts
}

func (u *tracingUser) UseContext(ctx context.Context) {
	u.seen.add(trace.SpanContextFromContext(ctx))
}

// TestStoreSeesTheCommandContext: the IMAP layer hands the span of the
// running command to the store, so store operations can be recorded
// below it.
func TestStoreSeesTheCommandContext(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	old := otel.GetTracerProvider()
	telemetry.Tracer = tp.Tracer("shardpost")

	t.Cleanup(func() { telemetry.Tracer = old.Tracer("shardpost") })

	seen := &seenContexts{}

	c, _ := newTestServer(t, func(o *Options, store *memstore.Store) {
		o.Store = tracingStore{inner: store, seen: seen}
	})
	defer func() { _ = c.conn.Close() }()

	c.ok("LOGIN test test")
	c.ok("SELECT INBOX")

	commands := map[trace.SpanID]string{}
	for _, s := range rec.Ended() {
		commands[s.SpanContext().SpanID()] = s.Name()
	}

	var names []string

	for _, sc := range seen.all() {
		if name, ok := commands[sc.SpanID()]; ok {
			names = append(names, name)
		}
	}

	if len(names) == 0 {
		t.Fatalf("the store never saw a command span (seen %d contexts, ended %d spans)", len(seen.all()), len(rec.Ended()))
	}

	for _, want := range []string{"imap.SELECT"} {
		if !contains(names, want) {
			t.Errorf("no store context for %s, got %v", want, names)
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}

	return false
}
