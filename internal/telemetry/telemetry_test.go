package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"shardpost/internal/config"
	"testing"

	"go.opentelemetry.io/otel/attribute"
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
