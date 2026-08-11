package observability

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Configure enables OTLP tracing only when an endpoint is configured. The
// deployment selects OTLP/gRPC for the private collector; HTTP remains
// supported for the public ingestion endpoint during migration.
func Configure(ctx context.Context, defaultService string) (func(context.Context) error, error) {
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))
	if endpoint == "" || strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return func(context.Context) error { return nil }, nil
	}

	ratio := 0.05
	if raw := strings.TrimSpace(os.Getenv("OTEL_TRACES_SAMPLER_ARG")); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed < 0 || parsed > 1 {
			return nil, fmt.Errorf("invalid OTEL_TRACES_SAMPLER_ARG %q", raw)
		}
		ratio = parsed
	}
	serviceName := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME"))
	if serviceName == "" {
		serviceName = defaultService
	}
	environment := firstNonEmpty(
		os.Getenv("OTEL_ENVIRONMENT"),
		os.Getenv("DEPLOY_ENV"),
		os.Getenv("RUNTIME_ENV"),
		os.Getenv("ENVIRONMENT"),
		"unknown",
	)
	instance := firstNonEmpty(
		os.Getenv("OTEL_SERVICE_INSTANCE_ID"),
		os.Getenv("INSTANCE"),
		os.Getenv("HOSTNAME"),
		serviceName,
	)

	var exporter sdktrace.SpanExporter
	var err error
	if strings.EqualFold(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"), "grpc") {
		exporter, err = otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(endpoint))
	} else {
		exporter, err = otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	}
	if err != nil {
		return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithResource(resource.NewWithAttributes("",
			attribute.String("service.name", serviceName),
			attribute.String("service.instance.id", instance),
			attribute.String("deployment.environment.name", environment),
			attribute.String("deployment.environment", environment),
			attribute.String("environment", environment),
			attribute.String("instance", instance),
		)),
	)
	otel.SetTracerProvider(provider)
	return provider.Shutdown, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return "unknown"
}

// TraceLogAttrs returns stable correlation fields for structured request logs.
// Keep both the OTel names and the short names used by VictoriaLogs/Grafana.
func TraceLogAttrs(ctx context.Context) []any {
	spanContext := trace.SpanContextFromContext(ctx)
	if !spanContext.IsValid() {
		return nil
	}
	attrs := RuntimeLogAttrs("web-saas-accounts")
	return append(attrs,
		"trace_id", spanContext.TraceID().String(),
		"span_id", spanContext.SpanID().String(),
	)
}

func RuntimeLogAttrs(defaultService string) []any {
	serviceName := firstNonEmpty(os.Getenv("OTEL_SERVICE_NAME"), defaultService)
	environment := firstNonEmpty(os.Getenv("OTEL_ENVIRONMENT"), os.Getenv("DEPLOY_ENV"), os.Getenv("RUNTIME_ENV"), os.Getenv("ENVIRONMENT"))
	instance := firstNonEmpty(os.Getenv("OTEL_SERVICE_INSTANCE_ID"), os.Getenv("INSTANCE"), os.Getenv("HOSTNAME"), serviceName)
	return []any{"service.name", serviceName, "service_name", serviceName, "environment", environment, "instance", instance}
}
