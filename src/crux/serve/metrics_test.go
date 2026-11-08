package serve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

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

func TestStatusClassBoundaries(t *testing.T) {
	for status, want := range map[int]string{
		http.StatusOK:                  statusClass2xx,
		399:                            statusClass2xx,
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
