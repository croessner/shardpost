package telemetry

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"shardpost/internal/config"
	"testing"
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
