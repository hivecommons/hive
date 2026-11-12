package serve

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

// Serve request instruments (hivecommons/crux#126). Like the callback API's
// they sit on the global providers, which are OTel's delegating no-ops until a
// host registers an SDK provider, so recording costs nothing by default.
//
// Attributes are bounded: the HTTP method (standard set, else _OTHER), the mux
// route pattern (for example "GET /v1/agents/{name}", never the concrete path or
// an agent name; routeUnmatched when no route matched) and the status class.
// Never a path, token or body.
const (
	instrumentationName = "github.com/hivecommons/hive/crux/serve"

	requestDurationMetric = "crux.serve.request.duration"
	requestCountMetric    = "crux.serve.requests"
	authRejectionMetric   = "crux.serve.auth.rejections"

	routeUnmatched = "unmatched"

	statusClass1xx = "1xx"
	statusClass2xx = "2xx"
	statusClass3xx = "3xx"
	statusClass4xx = "4xx"
	statusClass5xx = "5xx"

	methodOther = "_OTHER"
)

// Why authenticate refused a request: a fixed enum, so the rejection counter's
// reason attribute stays bounded.
const (
	rejectProjectForbidden  = "project_forbidden"
	rejectNoTokenConfigured = "no_token_configured"
	rejectInvalidToken      = "missing_or_invalid_token"
)

var (
	attrReason      = attribute.Key("reason")
	attrMethod      = attribute.Key("method")
	attrRoute       = attribute.Key("route")
	attrStatusClass = attribute.Key("status_class")
)

// requestMetrics holds the serve request and auth rejection instruments.
type requestMetrics struct {
	duration metric.Float64Histogram
	requests metric.Int64Counter
	// rejections counts requests authenticate refused with a 401. They never
	// reach instrument, which sits inside authenticate.
	rejections metric.Int64Counter
}

func newRequestMetrics(meter metric.Meter) requestMetrics {
	// Instrument constructors only fail on an invalid name or unit; both are
	// constants here, and the returned instrument is a usable no-op either way.
	duration, _ := meter.Float64Histogram(requestDurationMetric,
		metric.WithUnit("s"),
		metric.WithDescription("Time crux serve took to answer a request."))
	requests, _ := meter.Int64Counter(requestCountMetric,
		metric.WithDescription("crux serve requests, by method, route pattern and status class."))
	rejections, _ := meter.Int64Counter(authRejectionMetric,
		metric.WithDescription("crux serve requests refused by authentication, by reason."))
	return requestMetrics{duration: duration, requests: requests, rejections: rejections}
}

var globalRequestMetrics = sync.OnceValue(func() requestMetrics {
	return newRequestMetrics(otel.Meter(instrumentationName))
})

// statusRecorder captures the status a handler answered.
type statusRecorder struct {
	http.ResponseWriter
	status int
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

// flushRecorder is a statusRecorder over a writer that can stream. The stream
// handlers type-assert http.Flusher and refuse a writer without one, so the
// wrapper must offer Flush only when the wrapped writer does.
type flushRecorder struct {
	*statusRecorder
	http.Flusher
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return statusClass5xx
	case status >= 400:
		return statusClass4xx
	case status >= 300:
		return statusClass3xx
	case status >= 200:
		return statusClass2xx
	default:
		return statusClass1xx
	}
}

// boundedMethod maps a client-supplied method onto the standard set, and
// everything else onto _OTHER, as OTel HTTP semconv does for
// http.request.method.
func boundedMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions, http.MethodConnect, http.MethodTrace:
		return method
	default:
		return methodOther
	}
}

// instrument records one request's span, duration and count around next. It
// wraps the mux itself so the matched pattern is known once next returns.
func instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ctx, span := otel.Tracer(instrumentationName).Start(r.Context(), "serve", trace.WithSpanKind(trace.SpanKindServer))
		rec := &statusRecorder{ResponseWriter: w}
		var out http.ResponseWriter = rec
		if f, ok := w.(http.Flusher); ok {
			out = flushRecorder{rec, f}
		}
		// The mux records the matched pattern on the request it dispatches.
		req := r.WithContext(ctx)
		// A handler panic unwinds through here (net/http recovers it further
		// up), so record in a defer and count it as a 5xx; the panic then
		// continues untouched.
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
			route := req.Pattern
			if route == "" {
				route = routeUnmatched
			}
			attrs := []attribute.KeyValue{
				attrMethod.String(boundedMethod(r.Method)),
				attrRoute.String(route),
				attrStatusClass.String(statusClass(status)),
			}
			span.SetName(route)
			span.SetAttributes(attrs...)
			span.End()
			globalRequestMetrics().record(ctx, time.Since(start), attrs)
		}()
		next.ServeHTTP(out, req)
		returned = true
	})
}

// recordAuthRejection counts one 401 with only the bounded reason attribute.
func recordAuthRejection(ctx context.Context, reason string) {
	globalRequestMetrics().rejections.Add(ctx, 1, metric.WithAttributes(attrReason.String(reason)))
}

func (m requestMetrics) record(ctx context.Context, d time.Duration, attrs []attribute.KeyValue) {
	opt := metric.WithAttributes(attrs...)
	m.duration.Record(ctx, d.Seconds(), opt)
	m.requests.Add(ctx, 1, opt)
}
