package forgestate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

var (
	otelTestOnce   sync.Once
	otelTestReader *sdkmetric.ManualReader
)

// otelTestReaderFor installs one in-memory metrics pipeline per test binary
// (the global provider delegates to the first one set).
func otelTestReaderFor(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	otelTestOnce.Do(func() {
		otelTestReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(otelTestReader)))
	})
	return otelTestReader
}

func collect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func hasAttrs(set attribute.Set, want ...attribute.KeyValue) bool {
	for _, kv := range want {
		if v, ok := set.Value(kv.Key); !ok || v != kv.Value {
			return false
		}
	}
	return true
}

func TestAdaptiveInterval(t *testing.T) {
	const base = 15 * time.Minute
	cases := []struct {
		name             string
		remaining, limit int
		want             time.Duration
	}{
		{"unknown budget", 0, 0, base},
		{"full", 5000, 5000, base},
		{"half", 2500, 5000, base},
		{"at 20 percent", 1000, 5000, base},
		{"just under 20 percent", 999, 5000, 2 * base},
		{"at 10 percent", 500, 5000, 2 * base},
		{"just under 10 percent", 499, 5000, 4 * base},
		{"at 5 percent", 250, 5000, 4 * base},
		{"just under 5 percent", 249, 5000, 8 * base},
		{"exhausted", 0, 5000, 8 * base},
		{"negative remaining clamps", -3, 5000, 8 * base},
		{"zero base", 0, 5000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := base
			if tc.name == "zero base" {
				b = 0
			}
			if got := AdaptiveInterval(b, tc.remaining, tc.limit); got != tc.want {
				t.Fatalf("AdaptiveInterval(%v, %d, %d) = %v, want %v", b, tc.remaining, tc.limit, got, tc.want)
			}
		})
	}
}

// The reconcile cadence stretches with the core budget, for both the normal
// and the short (stale/drift) interval, and ignores an unknown budget.
func TestStoreIntervalStretchesWithBudget(t *testing.T) {
	budget := struct {
		sync.Mutex
		remaining, limit int
		ok               bool
	}{remaining: 5000, limit: 5000, ok: true}
	h := newHarness(t)
	h.store.opts.Budget = func(bucket string) (int, int, bool) {
		if bucket != string(BucketCore) {
			t.Errorf("budget bucket = %q, want core", bucket)
		}
		budget.Lock()
		defer budget.Unlock()
		return budget.remaining, budget.limit, budget.ok
	}
	rs := h.store.lookup("o/r")
	base := h.store.opts.ReconcileInterval
	if got := h.store.interval(rs); got != base {
		t.Fatalf("healthy interval = %v, want %v", got, base)
	}
	budget.Lock()
	budget.remaining = 400
	budget.Unlock()
	if got := h.store.interval(rs); got != 4*base {
		t.Fatalf("interval at 8%% budget = %v, want %v", got, 4*base)
	}

	h.fetcher.failList(errors.New("403"))
	_ = h.store.Reconcile(context.Background(), "o/r")
	short := h.store.opts.MinReconcileInterval
	if got := h.store.interval(rs); got != 4*short {
		t.Fatalf("stale interval at 8%% budget = %v, want %v", got, 4*short)
	}
	budget.Lock()
	budget.ok = false
	budget.Unlock()
	if got := h.store.interval(rs); got != short {
		t.Fatalf("stale interval with unknown budget = %v, want %v", got, short)
	}
}

func TestDefaultBudgetSource(t *testing.T) {
	t.Cleanup(func() { SetDefaultBudgetSource(nil) })
	h := newHarness(t)
	rs := h.store.lookup("o/r")
	base := h.store.opts.ReconcileInterval
	if got := h.store.interval(rs); got != base {
		t.Fatalf("interval without a budget source = %v, want %v", got, base)
	}
	SetDefaultBudgetSource(func(string) (int, int, bool) { return 100, 5000, true })
	if got := h.store.interval(rs); got != 8*base {
		t.Fatalf("interval via default budget source = %v, want %v", got, 8*base)
	}
	SetDefaultBudgetSource(nil)
	if got := h.store.interval(rs); got != base {
		t.Fatalf("interval after clearing the source = %v, want %v", got, base)
	}
}

func TestOTelDriftCounterPerKind(t *testing.T) {
	reader := otelTestReaderFor(t)
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0), issue(2, t0)}, []Item{pr(10, t0, "a")}, "")
	mustReconcile(t, h, "o/r")
	h.clock.Advance(time.Minute)
	// Issue #2 closed (missing), #3 opened (extra); PR #10 new head (changed).
	h.fetcher.setList([]Item{issue(1, t0), issue(3, t0)}, []Item{pr(10, t0.Add(time.Second), "b")}, "")
	mustReconcile(t, h, "o/r")

	m, ok := collect(t, reader)[MetricDriftTotal]
	if !ok {
		t.Fatalf("metric %s not recorded", MetricDriftTotal)
	}
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("data = %T, want Sum[int64]", m.Data)
	}
	var issues, prs int64
	for _, dp := range sum.DataPoints {
		switch {
		case hasAttrs(dp.Attributes, attribute.String("repo", "o/r"), attribute.String("kind", string(KindIssue))):
			issues += dp.Value
		case hasAttrs(dp.Attributes, attribute.String("repo", "o/r"), attribute.String("kind", string(KindPR))):
			prs += dp.Value
		}
	}
	if issues != 2 || prs != 1 {
		t.Fatalf("forgestate.drift{issue}=%d {pr}=%d, want 2 and 1", issues, prs)
	}
}

func TestOTelStalenessGauge(t *testing.T) {
	reader := otelTestReaderFor(t)
	h := newHarness(t, "o/stale")
	h.fetcher.setList([]Item{issue(1, t0)}, nil, "")
	mustReconcile(t, h, "o/stale")

	staleness := func() (int64, bool) {
		m, ok := collect(t, reader)[MetricStalenessSeconds]
		if !ok {
			return 0, false
		}
		g, ok := m.Data.(metricdata.Gauge[int64])
		if !ok {
			t.Fatalf("data = %T, want Gauge[int64]", m.Data)
		}
		for _, dp := range g.DataPoints {
			if hasAttrs(dp.Attributes, attribute.String("repo", "o/stale")) {
				return dp.Value, true
			}
		}
		return 0, false
	}
	if v, ok := staleness(); !ok || v != 0 {
		t.Fatalf("fresh staleness = %d ok=%v, want 0", v, ok)
	}
	h.clock.Advance(time.Minute)
	h.fetcher.failList(errors.New("403"))
	_ = h.store.Reconcile(context.Background(), "o/stale")
	h.clock.Advance(90 * time.Second)
	if v, ok := staleness(); !ok || v != 150 {
		t.Fatalf("stale staleness = %d ok=%v, want 150s since last good", v, ok)
	}
}

func TestOTelEventToPatchHistogram(t *testing.T) {
	reader := otelTestReaderFor(t)
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0)}, nil, "")
	mustReconcile(t, h, "o/r")
	h.fetcher.setItem(issue(1, t0.Add(time.Second), "bug"))

	received := h.clock.Now()
	h.clock.Advance(250 * time.Millisecond)
	ev := WebhookEvent{DeliveryID: "d1", Event: "issues", Action: "labeled", Kind: KindIssue, Number: 1, ReceivedAt: received}
	if err := h.store.ApplyWebhook(context.Background(), "o/r", ev); err != nil {
		t.Fatalf("ApplyWebhook: %v", err)
	}
	RecordEventToPatch(context.Background(), "o/r", WriterCacheInvalidate, received, received.Add(5*time.Millisecond))
	RecordEventToPatch(context.Background(), "o/r", WriterCacheInvalidate, time.Time{}, received)

	m, ok := collect(t, reader)[MetricEventToPatchMS]
	if !ok {
		t.Fatalf("metric %s not recorded", MetricEventToPatchMS)
	}
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("data = %T, want Histogram[float64]", m.Data)
	}
	var sawForgestate, sawLegacy bool
	for _, dp := range hist.DataPoints {
		if hasAttrs(dp.Attributes, attribute.String("repo", "o/r"), attribute.String("writer", WriterForgeState)) {
			sawForgestate = true
			if dp.Count != 1 || dp.Sum != 250 {
				t.Errorf("forgestate sample count=%d sum=%v, want 1 and 250ms", dp.Count, dp.Sum)
			}
		}
		if hasAttrs(dp.Attributes, attribute.String("repo", "o/r"), attribute.String("writer", WriterCacheInvalidate)) {
			sawLegacy = true
			if dp.Count != 1 || dp.Sum != 5 {
				t.Errorf("legacy sample count=%d sum=%v, want 1 and 5ms (zero receivedAt records nothing)", dp.Count, dp.Sum)
			}
		}
	}
	if !sawForgestate || !sawLegacy {
		t.Fatalf("writers seen: forgestate=%v legacy=%v", sawForgestate, sawLegacy)
	}
}
