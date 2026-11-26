package observability

import (
	"context"
	"net"
	"os"
	"strings"

	"github.com/DataDog/dd-trace-go/v2/ddtrace/tracer"
)

const (
	ProviderNone    = "none"
	ProviderOTel    = "otel"
	ProviderDDTrace = "ddtrace"
)

// ResolveTracingProvider selects the active tracing backend with OTel-first
// mutual exclusion so HTTP and SQL clients are not double-instrumented.
func ResolveTracingProvider() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TRACING_PROVIDER"))) {
	case ProviderOTel:
		return ProviderOTel
	case ProviderDDTrace:
		return ProviderDDTrace
	case ProviderNone:
		return ProviderNone
	}

	if strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) != "" &&
		!strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return ProviderOTel
	}

	if strings.TrimSpace(os.Getenv("DD_AGENT_HOST")) != "" &&
		!strings.EqualFold(strings.TrimSpace(os.Getenv("DD_TRACE_ENABLED")), "false") {
		return ProviderDDTrace
	}

	return ProviderNone
}

// DDTraceServiceName resolves the Datadog service name.
func DDTraceServiceName(defaultService string) string {
	return firstNonEmpty(os.Getenv("DD_SERVICE"), os.Getenv("OTEL_SERVICE_NAME"), defaultService)
}

// DDTraceAgentAddr builds host:port for DataKit DDTrace ingestion (default port 9529).
func DDTraceAgentAddr() string {
	host := firstNonEmpty(os.Getenv("DD_AGENT_HOST"), "datakit-service.datakit.svc")
	port := strings.TrimSpace(os.Getenv("DD_TRACE_AGENT_PORT"))
	if port == "" {
		port = "9529"
	}
	return net.JoinHostPort(host, port)
}

// ConfigureTracing initializes OTel or DDTrace according to ResolveTracingProvider.
func ConfigureTracing(ctx context.Context, defaultService string) (func(context.Context) error, string, error) {
	provider := ResolveTracingProvider()
	switch provider {
	case ProviderOTel:
		shutdown, err := Configure(ctx, defaultService)
		if err != nil {
			return nil, ProviderNone, err
		}
		return shutdown, ProviderOTel, nil
	case ProviderDDTrace:
		opts := []tracer.StartOption{
			tracer.WithService(DDTraceServiceName(defaultService)),
			tracer.WithEnv(firstNonEmpty(
				os.Getenv("DD_ENV"),
				os.Getenv("OTEL_ENVIRONMENT"),
				os.Getenv("DEPLOY_ENV"),
				os.Getenv("RUNTIME_ENV"),
				os.Getenv("ENVIRONMENT"),
				"unknown",
			)),
			tracer.WithAgentAddr(DDTraceAgentAddr()),
		}
		if version := strings.TrimSpace(os.Getenv("DD_VERSION")); version != "" {
			opts = append(opts, tracer.WithServiceVersion(version))
		}
		if err := tracer.Start(opts...); err != nil {
			return nil, ProviderNone, err
		}
		return func(context.Context) error {
			tracer.Stop()
			return nil
		}, ProviderDDTrace, nil
	default:
		return func(context.Context) error { return nil }, ProviderNone, nil
	}
}

// DDTraceSpanAttrs returns trace/span IDs from an active DDTrace span when present.
func DDTraceSpanAttrs(ctx context.Context, defaultService string) []any {
	span, ok := tracer.SpanFromContext(ctx)
	if !ok {
		return nil
	}
	sc := span.Context()
	if sc == nil || sc.TraceID() == "" {
		return nil
	}
	attrs := RuntimeLogAttrs(defaultService)
	return append(attrs,
		"trace_id", sc.TraceID(),
		"span_id", sc.SpanID(),
		"dd.trace_id", sc.TraceID(),
		"dd.span_id", sc.SpanID(),
	)
}
