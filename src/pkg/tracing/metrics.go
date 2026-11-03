package tracing

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Meter returns a named meter from the global MeterProvider. Until Init has
// installed the OTLP metrics pipeline this is OTel's global delegating
// no-op provider: instruments created from it cost nothing and never export,
// and they transparently start recording once a real provider is set. Passing
// an empty name uses the package default instrumentation scope.
func Meter(name string) metric.Meter {
	if name == "" {
		name = instrumentationName
	}
	return otel.Meter(name)
}

// initMetrics installs the OTLP/HTTP metrics pipeline alongside the trace
// pipeline. It shares Init's gating: nothing is installed unless cfg.Enabled
// and an endpoint is named by config or OTEL_EXPORTER_OTLP_ENDPOINT.
func initMetrics(ctx context.Context, cfg Config, res *resource.Resource) (func(context.Context) error, error) {
	opts := []otlpmetrichttp.Option{}
	if ep := strings.TrimSpace(cfg.Endpoint); ep != "" {
		opts = append(opts, otlpmetrichttp.WithEndpointURL(ep))
	} else if strings.TrimSpace(os.Getenv(otelEndpointEnv)) == "" {
		return noopShutdown, nil
	}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlpmetrichttp.WithHeaders(cfg.Headers))
	}
	if cfg.Insecure {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	exporter, err := otlpmetrichttp.New(ctx, opts...)
	if err != nil {
		return noopShutdown, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)
	return mp.Shutdown, nil
}

// joinShutdown runs every shutdown and returns their combined error. The
// trace provider's Shutdown is idempotent and the returned function keeps
// that contract: a second call is a no-op (the metrics reader would
// otherwise report "reader is shutdown").
func joinShutdown(fns ...func(context.Context) error) func(context.Context) error {
	var once sync.Once
	return func(ctx context.Context) error {
		var joined error
		once.Do(func() {
			var errs []error
			for _, fn := range fns {
				if fn == nil {
					continue
				}
				if err := fn(ctx); err != nil {
					errs = append(errs, err)
				}
			}
			joined = errors.Join(errs...)
		})
		return joined
	}
}
