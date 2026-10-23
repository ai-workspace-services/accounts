package observability

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func TestConfigureRegistersW3CTraceContextEvenWhenDisabled(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_SDK_DISABLED", "true")

	shutdown, err := Configure(context.Background(), "accounts-test")
	if err != nil {
		t.Fatalf("configure failed: %v", err)
	}
	t.Cleanup(func() {
		_ = shutdown(context.Background())
	})

	headers := propagation.HeaderCarrier(http.Header{
		"Traceparent": []string{"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		"Baggage":     []string{"user_id=secret"},
	})
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), headers)
	fields := otel.GetTextMapPropagator().Fields()
	hasTraceparent := false
	for _, f := range fields {
		if f == "traceparent" {
			hasTraceparent = true
		}
		if f == "baggage" {
			t.Fatalf("propagator must not enable baggage")
		}
	}
	if !hasTraceparent {
		t.Fatalf("expected traceparent in propagator fields, got %v", fields)
	}

	out := propagation.HeaderCarrier(http.Header{})
	otel.GetTextMapPropagator().Inject(ctx, out)
	if got := out.Get("traceparent"); got != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Fatalf("expected propagated traceparent, got %q", got)
	}
}

func TestFirstEndpointFallback(t *testing.T) {
	got := firstEndpoint("", " http://collector:4318 ")
	if got != "http://collector:4318" {
		t.Fatalf("expected fallback endpoint, got %q", got)
	}
	gotPrimary := firstEndpoint("http://traces-collector:4318/v1/traces", "http://collector:4318")
	if gotPrimary != "http://traces-collector:4318/v1/traces" {
		t.Fatalf("expected primary endpoint, got %q", gotPrimary)
	}
}

func TestSQLOptionsDisableQueryAndPrepare(t *testing.T) {
	opts := SQLOptions()
	if len(opts) < 2 {
		t.Fatalf("expected at least 2 otelsql options, got %d", len(opts))
	}
}
