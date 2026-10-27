// Package telemetry installs the OpenTelemetry SDK for the crux binary
// (hivecommons/crux#19). The runtime records spans and kick metrics through
// the OTel API alone; without a registered provider those are no-ops. Init
// registers an OTLP/HTTP TracerProvider and MeterProvider, but only when the
// standard OTEL_EXPORTER_OTLP_* environment names an endpoint. With none set
// it changes nothing: the global providers stay the zero-cost no-ops.
//
// Everything about the export — endpoint, headers, TLS, timeouts, service
// name, resource attributes — comes from the standard OTel environment
// variables, read by the SDK and exporters themselves; no collector address
// lives in code. Hive's own process does not use this package: it wires
// pkg/tracing from hive.yaml.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Standard OpenTelemetry environment variables consulted to decide whether to
// export at all. The exporters read the full OTEL_EXPORTER_OTLP_* family
// (headers, TLS, timeouts, compression) on their own.
const (
	envEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	envTracesEndpoint  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	envMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	envProtocol        = "OTEL_EXPORTER_OTLP_PROTOCOL"
	envTracesProtocol  = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	envMetricsProtocol = "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL"
	envTracesExporter  = "OTEL_TRACES_EXPORTER"
	envMetricsExporter = "OTEL_METRICS_EXPORTER"
	envSDKDisabled     = "OTEL_SDK_DISABLED"
)

// defaultServiceName is the resource service.name when OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES do not name one.
const defaultServiceName = "crux"

// shutdownTimeout bounds the final flush so a dead collector cannot hold the
// process open on exit.
const shutdownTimeout = 5 * time.Second

// Shutdown flushes and stops whatever Init installed. It is safe to call more
// than once and is a no-op when Init installed nothing.
type Shutdown func()

func noopShutdown() {}

// Init registers the OTLP trace and metric pipelines named by the environment
// and returns the function that flushes them. version becomes service.version
// on the resource. On error nothing is registered, the returned Shutdown is a
// no-op, and the caller keeps running with no-op providers.
func Init(ctx context.Context, version string) (Shutdown, error) {
	return initFromEnv(ctx, version, os.Getenv)
}

func initFromEnv(ctx context.Context, version string, getenv func(string) string) (Shutdown, error) {
	traces, metrics := signalsEnabled(getenv)
	if !traces && !metrics {
		return noopShutdown, nil
	}
	for _, key := range []string{envProtocol, envTracesProtocol, envMetricsProtocol} {
		// Only OTLP over HTTP is linked into crux, keeping gRPC out of the
		// module's dependency set; refuse rather than send protobuf-over-HTTP
		// to a gRPC port.
		if p := strings.TrimSpace(getenv(key)); p != "" && p != "http/protobuf" {
			return noopShutdown, fmt.Errorf("%s=%q is not supported (crux exports OTLP over http/protobuf only)", key, p)
		}
	}

	res, err := newResource(ctx, version)
	if err != nil {
		return noopShutdown, err
	}

	// Build both pipelines before registering either global, so a failure
	// on the second leaves no provider (and no shut-down provider) behind.
	var (
		tp    *sdktrace.TracerProvider
		mp    *sdkmetric.MeterProvider
		stops []func(context.Context) error
	)
	if traces {
		exp, err := otlptracehttp.New(ctx)
		if err != nil {
			return noopShutdown, fmt.Errorf("trace exporter: %w", err)
		}
		tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
		stops = append(stops, tp.Shutdown)
	}
	if metrics {
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			for _, stop := range stops {
				_ = stop(ctx)
			}
			return noopShutdown, fmt.Errorf("metric exporter: %w", err)
		}
		mp = sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)), sdkmetric.WithResource(res))
		stops = append(stops, mp.Shutdown)
	}
	if tp != nil {
		otel.SetTracerProvider(tp)
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	}
	if mp != nil {
		otel.SetMeterProvider(mp)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			var errs []error
			for _, stop := range stops {
				errs = append(errs, stop(sctx))
			}
			if err := errors.Join(errs...); err != nil {
				otel.Handle(err)
			}
		})
	}, nil
}

// signalsEnabled reports which signals the environment asks crux to export:
// a signal is on when the shared or its signal-specific endpoint is set, the
// SDK is not disabled, and its OTEL_*_EXPORTER is not "none".
func signalsEnabled(getenv func(string) string) (traces, metrics bool) {
	if strings.EqualFold(strings.TrimSpace(getenv(envSDKDisabled)), "true") {
		return false, false
	}
	shared := strings.TrimSpace(getenv(envEndpoint)) != ""
	traces = (shared || strings.TrimSpace(getenv(envTracesEndpoint)) != "") && !exporterNone(getenv(envTracesExporter))
	metrics = (shared || strings.TrimSpace(getenv(envMetricsEndpoint)) != "") && !exporterNone(getenv(envMetricsExporter))
	return traces, metrics
}

func exporterNone(v string) bool {
	return strings.EqualFold(strings.TrimSpace(v), "none")
}

// newResource identifies this process. The crux defaults come first so
// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES, applied after, override them.
func newResource(ctx context.Context, version string) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{attribute.String("service.name", defaultServiceName)}
	if v := strings.TrimSpace(version); v != "" {
		attrs = append(attrs, attribute.String("service.version", v))
	}
	return resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
}
