package callback

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Callback request instruments (hivecommons/crux#46). Like the kick
// instruments in crux/agent they sit on the global providers, which are
// OTel's delegating no-ops until a host registers an SDK provider, so
// recording costs nothing by default and exports once one is set.
//
// Attributes are bounded enums only: the capability (one of allCapabilities,
// or capabilityUnknown for the health route, unmatched routes and requests
// refused before dispatch), the status class and the Code* constant of a
// refusal. Never the URL path (it carries {agent} and {id} segments), a token
// or a body.
const (
	instrumentationName = "github.com/hivecommons/hive/crux/api/callback"

	requestDurationMetric = "crux.callback.request.duration"
	requestCountMetric    = "crux.callback.requests"

	capabilityUnknown = "unknown"
	codeNone          = "none"

	statusClass2xx = "2xx"
	statusClass4xx = "4xx"
	statusClass5xx = "5xx"
)

var (
	attrCapability  = attribute.Key("capability")
	attrStatusClass = attribute.Key("status_class")
	attrCode        = attribute.Key("code")
)

// requestMetrics holds the two callback request instruments.
type requestMetrics struct {
	duration metric.Float64Histogram
	requests metric.Int64Counter
}

func newRequestMetrics(meter metric.Meter) requestMetrics {
	// Instrument constructors only fail on an invalid name or unit; both are
	// constants here, and the returned instrument is a usable no-op either way.
	duration, _ := meter.Float64Histogram(requestDurationMetric,
		metric.WithUnit("s"),
		metric.WithDescription("Time the callback API took to answer a request."))
	requests, _ := meter.Int64Counter(requestCountMetric,
		metric.WithDescription("Callback API requests, by capability, status class and refusal code."))
	return requestMetrics{duration: duration, requests: requests}
}

var globalRequestMetrics = sync.OnceValue(func() requestMetrics {
	return newRequestMetrics(otel.Meter(instrumentationName))
})

// statusRecorder captures what a handler answered. The capability wrapper and
// writeError fill in capability and code through recorderOf.
type statusRecorder struct {
	http.ResponseWriter
	status     int
	capability string
	code       string
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// recorderOf returns w's statusRecorder, or nil when w was not wrapped by
// instrument (a handler called directly).
func recorderOf(w http.ResponseWriter) *statusRecorder {
	rec, _ := w.(*statusRecorder)
	return rec
}

// statusClass maps an HTTP status onto the fixed class enum.
func statusClass(status int) string {
	switch {
	case status >= 500:
		return statusClass5xx
	case status >= 400:
		return statusClass4xx
	default:
		return statusClass2xx
	}
}

// instrument records one request's duration, count and span around next.
func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, span := otel.Tracer(instrumentationName).Start(r.Context(), "callback", trace.WithSpanKind(trace.SpanKindServer))
		rec := &statusRecorder{ResponseWriter: w, capability: capabilityUnknown, code: codeNone}
		next.ServeHTTP(rec, r.WithContext(ctx))

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		attrs := []attribute.KeyValue{
			attrCapability.String(rec.capability),
			attrStatusClass.String(statusClass(status)),
			attrCode.String(rec.code),
		}
		span.SetName(rec.capability)
		span.SetAttributes(attrs...)
		span.End()
		globalRequestMetrics().record(ctx, time.Since(start), attrs)
	})
}

func (m requestMetrics) record(ctx context.Context, d time.Duration, attrs []attribute.KeyValue) {
	opt := metric.WithAttributes(attrs...)
	m.duration.Record(ctx, d.Seconds(), opt)
	m.requests.Add(ctx, 1, opt)
}
