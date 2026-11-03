package tracing

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

func restoreGlobalMeterProvider(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { otel.SetMeterProvider(metricnoop.NewMeterProvider()) })
}

// TestInit_DisabledLeavesMeterProviderAlone: with tracing off nothing
// installs a metrics pipeline, so the github.* and forgestate.* instruments
// stay no-ops.
func TestInit_DisabledLeavesMeterProviderAlone(t *testing.T) {
	restoreGlobalProvider(t)
	restoreGlobalMeterProvider(t)
	otel.SetMeterProvider(metricnoop.NewMeterProvider())
	before := otel.GetMeterProvider()
	if _, err := Init(context.Background(), Config{Enabled: false}); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if otel.GetMeterProvider() != before {
		t.Fatal("disabled Init replaced the global MeterProvider")
	}
	if _, err := Init(context.Background(), Config{Enabled: true}); err != nil {
		t.Fatalf("Init(enabled, no endpoint): %v", err)
	}
	if otel.GetMeterProvider() != before {
		t.Fatal("enabled Init without an endpoint replaced the global MeterProvider")
	}
}

// TestInit_EnabledInstallsMeterProvider: the enabled path installs an SDK
// MeterProvider next to the TracerProvider and the joined shutdown flushes
// both.
func TestInit_EnabledInstallsMeterProvider(t *testing.T) {
	restoreGlobalProvider(t)
	restoreGlobalMeterProvider(t)
	srv := newDropServer(t)

	shutdown, err := Init(context.Background(), Config{Enabled: true, Endpoint: srv.URL, Insecure: true})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); !ok {
		t.Fatalf("global MeterProvider = %T, want *sdkmetric.MeterProvider", otel.GetMeterProvider())
	}
	counter, err := Meter("test").Int64Counter("test.counter")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(context.Background(), 1)

	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := shutdown(sctx); err != nil {
		t.Errorf("shutdown returned error: %v", err)
	}
}
