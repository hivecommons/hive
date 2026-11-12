package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/api"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// useTestTracer routes the serve tracer at an in-memory exporter for the
// test's lifetime. instrument resolves otel.Tracer per request, so swapping
// the global provider is enough.
func useTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	return exp
}

func useTestMetrics(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m := newRequestMetrics(mp.Meter(instrumentationName))
	prev := globalRequestMetrics
	globalRequestMetrics = func() requestMetrics { return m }
	t.Cleanup(func() {
		globalRequestMetrics = prev
		_ = mp.Shutdown(context.Background())
	})
	return reader
}

func TestRequestMetricsKeyedByRoutePattern(t *testing.T) {
	reader := useTestMetrics(t)
	h := instrument(func() http.Handler {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/agents/{name}", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		return mux
	}())

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/agents/secret-agent-xyz", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nowhere/secret-agent-xyz", nil))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	got := map[string]string{}
	var durations int
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				if m.Name != requestCountMetric {
					t.Errorf("unexpected counter %q", m.Name)
				}
				for _, dp := range data.DataPoints {
					if dp.Attributes.Len() != 3 {
						t.Errorf("attributes = %v, want exactly method, route, status_class", dp.Attributes)
					}
					route, _ := dp.Attributes.Value(attrRoute)
					class, _ := dp.Attributes.Value(attrStatusClass)
					got[route.AsString()] = class.AsString()
					for _, kv := range dp.Attributes.ToSlice() {
						if strings.Contains(kv.Value.Emit(), "secret-agent-xyz") {
							t.Errorf("attribute %s=%q leaks the request path", kv.Key, kv.Value.Emit())
						}
					}
				}
			case metricdata.Histogram[float64]:
				if m.Name != requestDurationMetric || m.Unit != "s" {
					t.Errorf("unexpected histogram %q unit %q", m.Name, m.Unit)
				}
				durations += len(data.DataPoints)
			}
		}
	}
	if got["GET /v1/agents/{name}"] != statusClass4xx || got[routeUnmatched] != statusClass4xx || len(got) != 2 {
		t.Errorf("points = %v, want pattern and unmatched routes, both 4xx", got)
	}
	if durations != 2 {
		t.Errorf("duration points = %d, want 2", durations)
	}
}

func TestRequestMetricsBoundTheMethod(t *testing.T) {
	for _, tc := range []struct{ method, want string }{
		{http.MethodGet, http.MethodGet},
		{http.MethodPatch, http.MethodPatch},
		{"BREW", methodOther},
		{"get", methodOther},
	} {
		t.Run(tc.method, func(t *testing.T) {
			reader := useTestMetrics(t)
			h := instrument(http.NotFoundHandler())
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(tc.method, "/", nil))

			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			var methods []string
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if data, ok := m.Data.(metricdata.Sum[int64]); ok {
						for _, dp := range data.DataPoints {
							v, _ := dp.Attributes.Value(attrMethod)
							methods = append(methods, v.AsString())
						}
					}
				}
			}
			if len(methods) != 1 || methods[0] != tc.want {
				t.Errorf("methods = %v, want [%s]", methods, tc.want)
			}
		})
	}
}

func TestStatusClassBoundaries(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusContinue:            statusClass1xx,
		199:                            statusClass1xx,
		http.StatusOK:                  statusClass2xx,
		299:                            statusClass2xx,
		http.StatusFound:               statusClass3xx,
		399:                            statusClass3xx,
		http.StatusBadRequest:          statusClass4xx,
		499:                            statusClass4xx,
		http.StatusInternalServerError: statusClass5xx,
	} {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
}

// Stream handlers refuse a writer without a Flusher, so instrument must not
// invent one.
func TestInstrumentPreservesFlusher(t *testing.T) {
	var sawFlusher bool
	h := instrument(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, sawFlusher = w.(http.Flusher)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !sawFlusher {
		t.Error("flushable writer lost its Flusher")
	}
	h.ServeHTTP(noFlush{httptest.NewRecorder()}, httptest.NewRequest(http.MethodGet, "/", nil))
	if sawFlusher {
		t.Error("non-flushable writer gained a Flusher")
	}
}

// The span carries the same bounded attributes as the metric point and is
// named after the route pattern: a concrete path (which may embed an agent
// name) must appear in neither the span name nor its attributes.
func TestRequestSpansNamedByRoutePatternWithoutThePath(t *testing.T) {
	exp := useTestTracer(t)
	h := instrument(func() http.Handler {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/agents/{name}", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		})
		return mux
	}())

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/agents/secret-agent-xyz", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nowhere/secret-agent-xyz", nil))

	spans := exp.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want one per request", len(spans))
	}
	got := map[string]map[string]string{}
	for _, sp := range spans {
		if sp.SpanKind != trace.SpanKindServer {
			t.Errorf("span %q kind = %v, want server", sp.Name, sp.SpanKind)
		}
		if strings.Contains(sp.Name, "secret-agent-xyz") {
			t.Errorf("span name %q leaks the request path", sp.Name)
		}
		if len(sp.Attributes) != 3 {
			t.Errorf("span %q attributes = %v, want exactly method, route, status_class", sp.Name, sp.Attributes)
		}
		attrs := map[string]string{}
		for _, kv := range sp.Attributes {
			if strings.Contains(kv.Value.Emit(), "secret-agent-xyz") {
				t.Errorf("span attribute %s=%q leaks the request path", kv.Key, kv.Value.Emit())
			}
			attrs[string(kv.Key)] = kv.Value.Emit()
		}
		got[sp.Name] = attrs
	}
	want := map[string]map[string]string{
		"GET /v1/agents/{name}": {"method": "GET", "route": "GET /v1/agents/{name}", "status_class": statusClass4xx},
		routeUnmatched:          {"method": "GET", "route": routeUnmatched, "status_class": statusClass4xx},
	}
	for name, attrs := range want {
		for k, v := range attrs {
			if got[name][k] != v {
				t.Errorf("span %q %s = %q, want %q (spans: %v)", name, k, got[name][k], v, got)
			}
		}
	}
}

// The recorded status is the one the client saw: net/http commits the first
// WriteHeader (or an implicit 200 on the first Write) and ignores the rest,
// so the recorder must keep the first status too, not the last.
func TestRecordedStatusIsTheOneTheClientSaw(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"implicit 200 on write, late 500 ignored", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("ok"))
			w.WriteHeader(http.StatusInternalServerError)
		}, statusClass2xx},
		{"first WriteHeader wins", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			w.WriteHeader(http.StatusInternalServerError)
		}, statusClass4xx},
		{"nothing written is 200", func(http.ResponseWriter, *http.Request) {}, statusClass2xx},
		{"explicit 5xx", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}, statusClass5xx},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := useTestMetrics(t)
			rec := httptest.NewRecorder()
			instrument(tc.handler).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

			var rm metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &rm); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			var classes []string
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
						for _, dp := range sum.DataPoints {
							class, _ := dp.Attributes.Value(attrStatusClass)
							classes = append(classes, class.AsString())
						}
					}
				}
			}
			if len(classes) != 1 || classes[0] != tc.want {
				t.Fatalf("status_class points = %v, want [%s]", classes, tc.want)
			}
			if statusClass(rec.Code) != tc.want {
				t.Fatalf("recorder answered %d but %s was recorded: the metric disagrees with the client", rec.Code, tc.want)
			}
		})
	}
}

func TestRedirectIsCountedAs3xx(t *testing.T) {
	reader := useTestMetrics(t)
	instrument(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if got := collectStatusClasses(t, reader); len(got) != 1 || got[0] != statusClass3xx {
		t.Errorf("status classes = %v, want [%s]", got, statusClass3xx)
	}
}

// net/http recovers a handler panic above instrument, so the span and the
// sample must be recorded while the panic unwinds, and the panic must still
// propagate.
func TestPanickingHandlerIsRecordedAs5xx(t *testing.T) {
	exp := useTestTracer(t)
	reader := useTestMetrics(t)
	h := instrument(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	if recovered != "boom" {
		t.Fatalf("recovered = %v, want the handler's panic re-raised", recovered)
	}
	if got := collectStatusClasses(t, reader); len(got) != 1 || got[0] != statusClass5xx {
		t.Errorf("status classes = %v, want [%s]", got, statusClass5xx)
	}
	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want the panicking request's span ended", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Errorf("span status = %v, want error", spans[0].Status.Code)
	}
}

func collectStatusClasses(t *testing.T, reader *sdkmetric.ManualReader) []string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var classes []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok {
				for _, dp := range sum.DataPoints {
					class, _ := dp.Attributes.Value(attrStatusClass)
					classes = append(classes, class.AsString())
				}
			}
		}
	}
	return classes
}

func TestAuthRejectionsCountedByReason(t *testing.T) {
	reader := useTestMetrics(t)
	_, h := principalFixture(t)

	callWith(h, api.PathHealth, "wrong-secret-token")
	callWith(h, api.PathHealth, "")
	callWith(h, api.PathHealth, testProjectToken)
	callWith(h, api.PathHealth, "another-wrong-token")

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != authRejectionMetric {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				if dp.Attributes.Len() != 1 {
					t.Errorf("attributes = %v, want only reason", dp.Attributes)
				}
				reason, _ := dp.Attributes.Value(attrReason)
				got[reason.AsString()] = dp.Value
			}
		}
	}
	want := map[string]int64{rejectInvalidToken: 3, rejectProjectForbidden: 1}
	if len(got) != len(want) {
		t.Fatalf("rejections = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("rejections[%s] = %d, want %d", k, got[k], v)
		}
	}
}
