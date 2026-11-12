package proxy

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Inference translator instruments (hivecommons/crux#153). Like serve's they
// sit on the global providers, which are OTel's delegating no-ops until a host
// registers an SDK provider, so recording costs nothing by default.
//
// Attributes are bounded to the status class and a fixed outcome enum. Never an
// agent name, path, model name, token or body.
const (
	instrumentationName = "github.com/hivecommons/hive/crux/proxy"

	translatorDurationMetric = "crux.proxy.inference.duration"
	translatorCountMetric    = "crux.proxy.inference.requests"
)

// Outcome of one translator request: a fixed enum, so the attribute stays bounded.
const (
	outcomeOK                 = "ok"
	outcomeUnidentifiedCaller = "unidentified_caller"
	outcomeNoRoute            = "no_route"
	outcomeUpstreamError      = "upstream_error"
)

var (
	attrStatusClass = attribute.Key("status_class")
	attrOutcome     = attribute.Key("outcome")
)

type translatorMetrics struct {
	duration metric.Float64Histogram
	requests metric.Int64Counter
}

func newTranslatorMetrics(meter metric.Meter) translatorMetrics {
	// Instrument constructors only fail on an invalid name or unit; both are
	// constants here, and the returned instrument is a usable no-op either way.
	duration, _ := meter.Float64Histogram(translatorDurationMetric,
		metric.WithUnit("s"),
		metric.WithDescription("Time the inference translator took to answer a request."))
	requests, _ := meter.Int64Counter(translatorCountMetric,
		metric.WithDescription("Inference translator requests, by status class and outcome."))
	return translatorMetrics{duration: duration, requests: requests}
}

var globalTranslatorMetrics = sync.OnceValue(func() translatorMetrics {
	return newTranslatorMetrics(otel.Meter(instrumentationName))
})

func (m translatorMetrics) record(ctx context.Context, d time.Duration, attrs []attribute.KeyValue) {
	opt := metric.WithAttributes(attrs...)
	m.duration.Record(ctx, d.Seconds(), opt)
	m.requests.Add(ctx, 1, opt)
}

type outcomeKey struct{}

// outcomeHolder lets the handler name an outcome the status alone cannot tell.
type outcomeHolder struct{ outcome string }

func setTranslatorOutcome(ctx context.Context, outcome string) {
	if h, ok := ctx.Value(outcomeKey{}).(*outcomeHolder); ok {
		h.outcome = outcome
	}
}

type translatorStatusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *translatorStatusRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *translatorStatusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *translatorStatusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// translatorFlushRecorder offers Flush only when the wrapped writer does; the
// SSE path type-asserts http.Flusher.
type translatorFlushRecorder struct {
	*translatorStatusRecorder
	http.Flusher
}

func translatorStatusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}

// instrumentTranslator records one request's server span, duration and count
// around next.
func instrumentTranslator(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		holder := &outcomeHolder{}
		ctx, span := otel.Tracer(instrumentationName).Start(
			context.WithValue(r.Context(), outcomeKey{}, holder),
			"inference_translator", trace.WithSpanKind(trace.SpanKindServer))
		rec := &translatorStatusRecorder{ResponseWriter: w}
		var out http.ResponseWriter = rec
		if f, ok := w.(http.Flusher); ok {
			out = translatorFlushRecorder{rec, f}
		}
		returned := false
		defer func() {
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			if !returned {
				status = http.StatusInternalServerError
				span.SetStatus(codes.Error, "handler panic")
			}
			outcome := holder.outcome
			if outcome == "" {
				outcome = outcomeOK
				if status >= 400 {
					outcome = outcomeUpstreamError
				}
			}
			attrs := []attribute.KeyValue{
				attrStatusClass.String(translatorStatusClass(status)),
				attrOutcome.String(outcome),
			}
			span.SetAttributes(attrs...)
			span.End()
			globalTranslatorMetrics().record(ctx, time.Since(start), attrs)
		}()
		next.ServeHTTP(out, r.WithContext(ctx))
		returned = true
	})
}
