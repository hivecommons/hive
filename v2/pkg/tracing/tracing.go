// Package tracing provides an opt-in OpenTelemetry distributed-tracing
// foundation for hive. It is OFF by default: with no configuration (or
// tracing.enabled=false) Init installs a no-op TracerProvider, so StartSpan and
// friends cost effectively nothing and never touch the network. When enabled it
// wires an OTLP/HTTP exporter and a batching SDK TracerProvider, tagged with a
// "hive" service name plus hive-id and branch resource attributes.
//
// Nothing in this package panics on missing configuration. Callers can always
// call Tracer/StartSpan unconditionally; when disabled they get the global
// no-op tracer.
package tracing

import (
	"context"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	// instrumentationName is the default instrumentation scope used by the
	// package-level Tracer() helper.
	instrumentationName = "github.com/kubestellar/hive/v2/pkg/tracing"

	// otelEndpointEnv is the standard OTel environment variable used as the
	// exporter endpoint when Config.Endpoint is empty. Reading it here keeps
	// the collector address out of source (no hardcoded endpoints).
	otelEndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

	// fullSampleRatio is the sampling ratio applied when the configured ratio
	// is unset/zero: sample everything so "enabled:true" alone yields traces.
	fullSampleRatio = 1.0
)

// Resource attribute keys for hive-specific identity. These are package-level
// vars (not consts) because attribute.Key values are not compile-time
// constants.
var (
	attrHiveID  = attribute.Key("hive.id")
	attrBranchK = attribute.Key("hive.branch")
)

// Config is the minimal set of tracing settings this package needs. It mirrors
// config.TracingConfig plus the identity attributes, decoupling the package
// from the config package so it stays independently testable.
type Config struct {
	// Enabled turns tracing on. When false, Init returns a no-op shutdown.
	Enabled bool
	// Endpoint is the OTLP/HTTP collector endpoint. When empty, the standard
	// OTEL_EXPORTER_OTLP_ENDPOINT env var is used instead.
	Endpoint string
	// SampleRatio is the head-based sampling ratio. Zero (the default) is
	// treated as 1.0 (sample everything).
	SampleRatio float64
	// Headers are optional OTLP/HTTP headers, commonly used for collector auth.
	Headers map[string]string
	// ServiceName is the OTLP resource service.name for every hive span.
	ServiceName string
	// Insecure disables TLS for OTLP/HTTP, matching the exporter option.
	Insecure bool
	// HiveID and Branch are recorded as resource attributes for correlation.
	HiveID string
	Branch string
}

// noopShutdown is returned whenever tracing is disabled or fails to initialize;
// it satisfies the shutdown contract without doing anything.
func noopShutdown(context.Context) error { return nil }

// Init sets up the global TracerProvider from cfg and returns a shutdown
// function that flushes and stops the exporter. When cfg.Enabled is false it
// leaves the global provider as OTel's built-in no-op and returns a no-op
// shutdown with a nil error — the zero-overhead default path.
//
// Init never panics: any exporter/provider construction error is returned to
// the caller alongside a safe no-op shutdown, and the global provider is left
// untouched so StartSpan keeps working (as a no-op).
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	if !cfg.Enabled {
		return noopShutdown, nil
	}

	opts := []otlptracehttp.Option{}
	// Only override the endpoint when explicitly configured. An empty endpoint
	// lets the exporter honor OTEL_EXPORTER_OTLP_ENDPOINT itself, so we never
	// bake a collector address into the binary.
	if ep := strings.TrimSpace(cfg.Endpoint); ep != "" {
		opts = append(opts, otlptracehttp.WithEndpointURL(ep))
	} else if strings.TrimSpace(os.Getenv(otelEndpointEnv)) == "" {
		// Neither config nor env named an endpoint. The exporter would default
		// to localhost:4318, which is almost never right and would spew
		// connection errors. Fail closed to a no-op instead of guessing.
		return noopShutdown, nil
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
	}
	if cfg.Insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return noopShutdown, err
	}

	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = "hive"
	}
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(serviceName),
			attrHiveID.String(cfg.HiveID),
			attrBranchK.String(cfg.Branch),
		),
	)
	if err != nil {
		// Fall back to a bare resource rather than aborting tracing entirely.
		res = resource.NewSchemaless(
			semconv.ServiceName(serviceName),
			attrHiveID.String(cfg.HiveID),
			attrBranchK.String(cfg.Branch),
		)
	}

	ratio := cfg.SampleRatio
	if ratio <= 0 {
		ratio = fullSampleRatio
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}

// Tracer returns a named tracer from the global provider. When tracing is
// disabled this is the global no-op tracer. Passing an empty name uses the
// package default instrumentation scope.
func Tracer(name string) trace.Tracer {
	if name == "" {
		name = instrumentationName
	}
	return otel.Tracer(name)
}

// StartSpan starts a span on the default instrumentation tracer and returns the
// derived context and the span. When tracing is disabled the returned span is a
// no-op and the context is unchanged in any observable way. The caller MUST end
// the span (typically `defer span.End()`).
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer(instrumentationName).Start(ctx, name, trace.WithAttributes(attrs...))
}
