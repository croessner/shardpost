// Package telemetry sets up OpenTelemetry tracing. Without an OTLP
// endpoint (OTEL_EXPORTER_OTLP_ENDPOINT) tracing stays a no-op so the
// instrumentation costs nothing.
package telemetry

import (
	"context"
	"log/slog"
	"net/url"
	"os"
	"shardpost/internal/config"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracer is the process-wide tracer.
var Tracer = otel.Tracer("shardpost")

// Setup configures the global tracer provider from the standard OTEL_*
// environment variables. Returns a shutdown function.
func Setup(ctx context.Context, service string, log *slog.Logger, cfg config.Telemetry) func(context.Context) error {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		return func(context.Context) error { return nil }
	}

	var client otlptrace.Client

	if strings.EqualFold(cfg.Protocol, "http/protobuf") {
		// This is the generic OTLP base URL, matching OTEL_EXPORTER_OTLP_ENDPOINT.
		// WithEndpointURL itself expects the complete signal-specific URL.
		traceURL, err := url.JoinPath(endpoint, "v1/traces")
		if err != nil {
			log.Error("invalid otlp endpoint")
			return func(context.Context) error { return nil }
		}

		client = otlptracehttp.NewClient(otlptracehttp.WithEndpointURL(traceURL))
	} else {
		client = otlptracegrpc.NewClient(otlptracegrpc.WithEndpointURL(endpoint))
	}

	exp, err := otlptrace.New(ctx, client)
	if err != nil {
		log.Error("otel exporter", "err", err)
		return func(context.Context) error { return nil }
	}

	res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(service),
		attribute.String("service.instance.id", hostname()),
	))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(2*time.Second)),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	Tracer = tp.Tracer("shardpost")

	log.Info("opentelemetry tracing enabled", "endpoint", endpoint)

	return tp.Shutdown
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// Span starts a span with common attributes.
func Span(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer.Start(ctx, name, trace.WithAttributes(attrs...))
}
