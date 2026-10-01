package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"shardpost/internal/config"
	"slices"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestHTTPBaseEndpointAppendsSignalPath(t *testing.T) {
	paths := make(chan string, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path

		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	stop := Setup(context.Background(), "test", slog.New(slog.NewTextHandler(io.Discard, nil)), config.Telemetry{Endpoint: server.URL + "/collector", Protocol: "http/protobuf", SampleRatio: 1})
	_, span := Span(context.Background(), "endpoint-check")
	span.End()

	if err := stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	select {
	case path := <-paths:
		if path != "/collector/v1/traces" {
			t.Fatalf("path=%s", path)
		}
	default:
		t.Fatal("no trace exported")
	}
}

// TestPersonal: personal span attributes are dropped unless switched on.
func TestPersonal(t *testing.T) {
	kv := attribute.String("imap.user", "alice")
	if got := Personal(kv); got != nil {
		t.Fatalf("personal attributes leak by default: %v", got)
	}

	SetPersonal(true)
	t.Cleanup(func() { SetPersonal(false) })

	if got := Personal(kv); len(got) != 1 || got[0] != kv {
		t.Fatalf("personal attributes missing when switched on: %v", got)
	}
}

// TestFail: a failed span carries the error text only with personal
// attributes switched on, else just the error's type.
func TestFail(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tr := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)).Tracer("t")
	err := errors.New("no such mailbox Entwürfe")

	_, span := tr.Start(context.Background(), "off")
	Fail(span, err)
	span.End()

	SetPersonal(true)
	t.Cleanup(func() { SetPersonal(false) })

	_, span = tr.Start(context.Background(), "on")
	Fail(span, err)
	span.End()

	off, on := rec.Ended()[0], rec.Ended()[1]
	if len(off.Events()) != 0 || off.Status().Code != codes.Error || off.Status().Description != "" {
		t.Errorf("error text leaks without personal attributes: %v %v", off.Events(), off.Status())
	}

	if !slices.Contains(off.Attributes(), attribute.String("error.type", "*errors.errorString")) {
		t.Errorf("no error type: %v", off.Attributes())
	}

	if len(on.Events()) != 1 || on.Status().Description != err.Error() {
		t.Errorf("error text missing with personal attributes: %v %v", on.Events(), on.Status())
	}
}
