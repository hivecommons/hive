package callback

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// useTestMetrics points the handler at a manual reader for one test.
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

func serve(h http.Handler, method, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	if auth != "" {
		req.Header.Set("Authorization", bearerPrefix+auth)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// counterPoints returns the request counter's data points by attribute set.
func counterPoints(t *testing.T, reader *sdkmetric.ManualReader) map[attribute.Distinct]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	points := map[attribute.Distinct]int64{}
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
						t.Errorf("counter attributes = %v, want exactly capability, status_class, code", dp.Attributes)
					}
					points[dp.Attributes.Equivalent()] += dp.Value
				}
			case metricdata.Histogram[float64]:
				if m.Name != requestDurationMetric || m.Unit != "s" {
					t.Errorf("unexpected histogram %q unit %q", m.Name, m.Unit)
				}
				durations += len(data.DataPoints)
			}
		}
	}
	if durations != len(points) {
		t.Errorf("duration points = %d, counter points = %d, want equal", durations, len(points))
	}
	return points
}

func attrSet(capability, class, code string) attribute.Distinct {
	set := attribute.NewSet(attrCapability.String(capability), attrStatusClass.String(class), attrCode.String(code))
	return set.Equivalent()
}

func TestRequestMetricsBoundedAttributes(t *testing.T) {
	reader := useTestMetrics(t)
	h := NewServer(&fakeCaps{}, testBearer).Handler()

	serve(h, http.MethodGet, PathLinearCredential, testBearer)
	serve(h, http.MethodPut, PathInferenceRoutes+"/secret-agent-xyz", testBearer)

	points := counterPoints(t, reader)
	if len(points) != 2 {
		t.Fatalf("points = %d, want 2: %v", len(points), points)
	}
	if points[attrSet(CapabilityLinear, statusClass2xx, codeNone)] != 1 {
		t.Errorf("missing linear 2xx point: %v", points)
	}
	for _, c := range []string{CapabilityInferenceRoutes} {
		found := false
		for _, class := range []string{statusClass2xx, statusClass4xx, statusClass5xx} {
			for _, code := range []string{codeNone, CodeBadRequest, CodeUpstream, CodeNotConfigured} {
				found = found || points[attrSet(c, class, code)] == 1
			}
		}
		if !found {
			t.Errorf("no point for capability %s: %v", c, points)
		}
	}
}

func TestRequestMetricsNoPerIDLabel(t *testing.T) {
	reader := useTestMetrics(t)
	h := NewServer(&fakeCaps{}, testBearer).Handler()

	serve(h, http.MethodPut, PathInferenceRoutes+"/secret-agent-xyz", testBearer)
	serve(h, http.MethodPost, PathEffects+"/effect-id-123/end", testBearer)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			var sets []attribute.Set
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					sets = append(sets, dp.Attributes)
				}
			case metricdata.Histogram[float64]:
				for _, dp := range data.DataPoints {
					sets = append(sets, dp.Attributes)
				}
			}
			for _, set := range sets {
				for _, kv := range set.ToSlice() {
					v := kv.Value.Emit()
					if strings.Contains(v, "secret-agent-xyz") || strings.Contains(v, "effect-id-123") || strings.Contains(v, "/") {
						t.Errorf("attribute %s=%q leaks request path data", kv.Key, v)
					}
				}
			}
		}
	}
}

func TestRequestMetricsCountUnauthorizedAsUnknown(t *testing.T) {
	reader := useTestMetrics(t)
	h := NewServer(&fakeCaps{}, testBearer).Handler()

	if rr := serve(h, http.MethodGet, PathLinearCredential, "wrong-token"); rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}

	points := counterPoints(t, reader)
	if len(points) != 1 || points[attrSet(capabilityUnknown, statusClass4xx, CodeUnauthorized)] != 1 {
		t.Errorf("points = %v, want one unknown/4xx/unauthorized", points)
	}
}
