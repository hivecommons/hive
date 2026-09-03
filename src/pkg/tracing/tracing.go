// Package tracing provides an opt-in OpenTelemetry distributed-tracing
// foundation for hive. It is OFF by default: with no configuration (or
// tracing.enabled=false) Init installs a no-op TracerProvider, so StartSpan and
// friends cost effectively nothing and never touch the network. When enabled it
// wires an OTLP/HTTP exporter and a batching SDK TracerProvider, tagged with a
// "hive" service name plus hive-id, branch, and running-commit resource
// attributes (service.version / hive.commit / hive.image — the PR-reach
// anchors of #3973).
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
	instrumentationName = "github.com/hivecommons/hive/pkg/tracing"

	// otelEndpointEnv is the standard OTel environment variable used as the
	// exporter endpoint when Config.Endpoint is empty. Reading it here keeps
	// the collector address out of source (no hardcoded endpoints).
	otelEndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

	// fullSampleRatio is the sampling ratio applied when the configured ratio
	// is unset/zero: sample everything so "enabled:true" alone yields traces.
	fullSampleRatio = 1.0

	// DefaultServiceName is the fallback OTLP service.name when the caller
	// passes none; it mirrors config.DefaultOTelServiceName without importing
	// the config package (this package stays independently testable).
	DefaultServiceName = "hive"

	// unknownVersion is the ldflags default cmd/hive ships when no commit was
	// injected at build time (see `var gitShort` in cmd/hive). A version equal
	// to it is NOT stamped onto the resource: reach attribution (#3973) must
	// never group spans under a placeholder that isn't a real commit.
	unknownVersion = "unknown"
)

// Resource attribute keys for hive-specific identity. These are package-level
// vars (not consts) because attribute.Key values are not compile-time
// constants.
var (
	attrHiveID  = attribute.Key("hive.id")
	attrBranchK = attribute.Key("hive.branch")
	// attrCommitK stamps every span with the commit SHA baked into the RUNNING
	// binary at build time (ldflags), the reach anchor for #3973: a published
	// digest can advance while the binary stays old (#3816), so reach must
	// attribute to what actually executes, never to the merge/publish event.
	attrCommitK = attribute.Key("hive.commit")
	// attrImageK records the Deployment-declared image ref (digest or tag) when
	// known at runtime. Informational only: the declared ref can be newer than
	// the running binary (#3816) — hive.commit is the authoritative anchor.
	attrImageK = attribute.Key("hive.image")
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
	// Commit is the git commit SHA baked into the running binary (the same
	// ldflags value `hive --version` prints). Recorded as both service.version
	// and hive.commit so span data is attributable to the code that actually
	// ran — the PR-reach anchor for #3973.
	Commit string
	// Image is the Deployment-declared container image ref (ideally a digest)
	// when known at runtime, recorded as hive.image. Empty outside a cluster;
	// an empty value omits the attribute rather than recording "".
	Image string
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

	res := newResource(cfg)

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

// newResource builds the OTLP resource identifying this hive process. Factored
// out of Init so the identity attributes — especially the #3973 reach anchors
// service.version / hive.commit / hive.image — are testable without an
// exporter or network.
func newResource(cfg Config) *resource.Resource {
	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = DefaultServiceName
	}
	attrs := []attribute.KeyValue{
		semconv.ServiceName(serviceName),
		attrHiveID.String(cfg.HiveID),
		attrBranchK.String(cfg.Branch),
	}
	// Version stamping (#3973): the build-time commit becomes service.version
	// (standard semconv, so any OTel backend groups by it out of the box) and
	// hive.commit (explicit, greppable alongside the other hive.* identity
	// attrs). Only stamped when the build actually injected a SHA — an ldflags
	// default like "unknown" would poison reach queries with a fake version.
	if commit := strings.TrimSpace(cfg.Commit); commit != "" && commit != unknownVersion {
		attrs = append(attrs,
			semconv.ServiceVersion(commit),
			attrCommitK.String(commit),
		)
	}
	// The Deployment-declared image ref, when the process could read it. Kept
	// separate from hive.commit because the two CAN disagree (#3816) and that
	// disagreement is itself signal: declared-but-not-running code.
	if img := strings.TrimSpace(cfg.Image); img != "" {
		attrs = append(attrs, attrImageK.String(img))
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(semconv.SchemaURL, attrs...),
	)
	if err != nil {
		// Fall back to a bare resource rather than aborting tracing entirely.
		res = resource.NewSchemaless(attrs...)
	}
	return res
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
//
// Every span automatically carries hive.component derived from its name (see
// SpanComponent), so package-level reach (#3973) is queryable without any
// call-site changes.
//
// Every span ALSO increments the in-process reach counters (#3993) — even when
// no exporter is configured and the underlying span is a no-op. That is design
// D2 of #3973: reach must be reported by every spoke, and the `otel:` exporter
// is opt-in, so counting cannot depend on it. The returned span is a thin
// wrapper that observes End/SetStatus for the error counter; the context
// carries the underlying span, so child spans and any span read back via
// trace.SpanFromContext behave exactly as before (a status set through the
// context rather than the returned handle is invisible to reach, which is
// acceptable at component granularity — every call site in-tree uses the
// returned handle).
func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	component := SpanComponent(name)
	attrs = append(attrs, attribute.String(AttrHiveComponent, component))
	ctx, span := Tracer(instrumentationName).Start(ctx, name, trace.WithAttributes(attrs...))
	recordReachStart(component)
	return ctx, &reachSpan{Span: span, component: component}
}

// SpanComponent extracts the coarse component from a span name: the prefix
// before the first dot under the existing "<component>.<operation>" naming
// convention ("governor.eval_cycle" → "governor"). A name with no dot is its
// own component. This is the deliberate phase-1 granularity for #3973 —
// component-level, not per-PR.
func SpanComponent(name string) string {
	if i := strings.IndexByte(name, '.'); i > 0 {
		return name[:i]
	}
	return name
}
