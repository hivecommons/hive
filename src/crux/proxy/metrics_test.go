package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/crux/agent"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestTranslatorMetricsBoundedAttributes(t *testing.T) {
	fakeProcNetTCP(t)
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	m := newTranslatorMetrics(mp.Meter(instrumentationName))
	prev := globalTranslatorMetrics
	globalTranslatorMetrics = func() translatorMetrics { return m }
	t.Cleanup(func() {
		globalTranslatorMetrics = prev
		_ = mp.Shutdown(context.Background())
	})

	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.uidMap = &agent.UIDMap{BaseUID: 2001, Agents: map[string]int{"quality": 2002}}
	rec := httptest.NewRecorder()
	p.inferenceTranslatorHandler().ServeHTTP(rec, translatorReq("127.0.0.1:43210", "quality"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var counts, durations int
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			got := map[string]string{}
			collect := func(len int, kv func(string) (string, bool)) {
				if len != 2 {
					t.Errorf("%s: %d attributes, want exactly status_class and outcome", mt.Name, len)
				}
				for _, k := range []string{"status_class", "outcome", "agent", "path", "model"} {
					if v, ok := kv(k); ok {
						got[k] = v
					}
				}
			}
			switch data := mt.Data.(type) {
			case metricdata.Sum[int64]:
				counts++
				for _, dp := range data.DataPoints {
					collect(dp.Attributes.Len(), func(k string) (string, bool) {
						v, ok := dp.Attributes.Value(attrKey(k))
						return v.AsString(), ok
					})
				}
			case metricdata.Histogram[float64]:
				durations++
				for _, dp := range data.DataPoints {
					collect(dp.Attributes.Len(), func(k string) (string, bool) {
						v, ok := dp.Attributes.Value(attrKey(k))
						return v.AsString(), ok
					})
				}
			}
			if got["status_class"] != "4xx" || got["outcome"] != outcomeUnidentifiedCaller {
				t.Errorf("%s attributes = %v, want 4xx/unidentified_caller", mt.Name, got)
			}
			for _, bad := range []string{"agent", "path", "model"} {
				if _, ok := got[bad]; ok {
					t.Errorf("%s carries forbidden attribute %q", mt.Name, bad)
				}
			}
		}
	}
	if counts != 1 || durations != 1 {
		t.Fatalf("counters = %d, histograms = %d, want 1 each", counts, durations)
	}
}

func attrKey(k string) attribute.Key { return attribute.Key(k) }
