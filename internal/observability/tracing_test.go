package observability

import "testing"

func TestResolveTracingProvider(t *testing.T) {
	t.Run("prefers OTel when both are set", func(t *testing.T) {
		t.Setenv("TRACING_PROVIDER", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://otel:4317")
		t.Setenv("OTEL_SDK_DISABLED", "")
		t.Setenv("DD_AGENT_HOST", "datakit-service.observability-sit.svc")
		if got := ResolveTracingProvider(); got != ProviderOTel {
			t.Fatalf("got %q, want %q", got, ProviderOTel)
		}
	})

	t.Run("falls back to ddtrace when OTel is absent or disabled", func(t *testing.T) {
		t.Setenv("TRACING_PROVIDER", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
		t.Setenv("DD_AGENT_HOST", "datakit-service.observability-sit.svc")
		t.Setenv("DD_TRACE_ENABLED", "")
		if got := ResolveTracingProvider(); got != ProviderDDTrace {
			t.Fatalf("got %q, want %q", got, ProviderDDTrace)
		}

		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://otel:4317")
		t.Setenv("OTEL_SDK_DISABLED", "true")
		if got := ResolveTracingProvider(); got != ProviderDDTrace {
			t.Fatalf("got %q, want %q", got, ProviderDDTrace)
		}
	})

	t.Run("honors explicit override and defaults to none", func(t *testing.T) {
		t.Setenv("TRACING_PROVIDER", "ddtrace")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://otel:4317")
		t.Setenv("DD_AGENT_HOST", "datakit-service.observability-sit.svc")
		if got := ResolveTracingProvider(); got != ProviderDDTrace {
			t.Fatalf("got %q, want %q", got, ProviderDDTrace)
		}

		t.Setenv("TRACING_PROVIDER", "")
		t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
		t.Setenv("DD_AGENT_HOST", "")
		if got := ResolveTracingProvider(); got != ProviderNone {
			t.Fatalf("got %q, want %q", got, ProviderNone)
		}
	})
}

func TestDDTraceAgentAddr(t *testing.T) {
	t.Setenv("DD_AGENT_HOST", "datakit-service.observability-prod.svc")
	t.Setenv("DD_TRACE_AGENT_PORT", "")
	if got := DDTraceAgentAddr(); got != "datakit-service.observability-prod.svc:9529" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("DD_TRACE_AGENT_PORT", "19529")
	if got := DDTraceAgentAddr(); got != "datakit-service.observability-prod.svc:19529" {
		t.Fatalf("got %q", got)
	}
}
