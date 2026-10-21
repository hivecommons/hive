package forgestate

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hivecommons/hive/pkg/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// OpenTelemetry instruments for cache freshness (hivecommons/hive#13380).
// They record through the global MeterProvider pkg/tracing installs; with
// tracing off (the default) every call is a no-op.

const (
	forgestateInstrumentationScope = "github.com/hivecommons/hive/pkg/forgestate"

	// MetricStalenessSeconds is a gauge {repo}: seconds since the repo's last
	// good reconcile while the snapshot is stale, 0 while it is fresh.
	MetricStalenessSeconds = "forgestate.staleness_s"
	// MetricDriftTotal is a counter {repo,kind}: items a reconcile found
	// missing, extra or changed in the cache.
	MetricDriftTotal = "forgestate.drift"
	// MetricEventToPatchMS is a histogram {repo,writer}: milliseconds from a
	// webhook delivery's receipt to the cache patch it caused being applied.
	MetricEventToPatchMS = "webhook.event_to_patch_ms"

	// WriterForgeState labels event_to_patch samples recorded by ApplyWebhook.
	WriterForgeState = "forgestate"
	// WriterCacheInvalidate labels samples recorded by the legacy PR-cache
	// invalidation path in the dashboard webhook handler.
	WriterCacheInvalidate = "cache_invalidate"
)

// Adaptive reconcile thresholds (hivecommons/hive#13380): when the fraction
// of the relevant rate-limit bucket still available drops below a threshold
// the reconcile interval is stretched by its factor, so a draining budget
// buys fewer full lists rather than a mid-window lockout. Ordered from the
// tightest budget to the loosest; the first match wins.
var adaptiveIntervalLadder = []struct {
	below  float64
	factor time.Duration
}{
	{below: 0.05, factor: 8},
	{below: 0.10, factor: 4},
	{below: 0.20, factor: 2},
}

// BudgetFunc reports the rate-limit budget left in bucket: the most recent
// remaining/limit pair, ok false when none has been observed. The hive
// client's github.RateLimitBudget satisfies it.
type BudgetFunc func(bucket string) (remaining, limit int, ok bool)

var defaultBudget atomic.Pointer[BudgetFunc]

// SetDefaultBudgetSource installs the BudgetFunc a Store uses when its
// Options.Budget is nil. cmd/hive wires github.RateLimitBudget here at boot
// so the cache reads the same quota headers the client's transport saw.
func SetDefaultBudgetSource(fn BudgetFunc) {
	if fn == nil {
		defaultBudget.Store(nil)
		return
	}
	defaultBudget.Store(&fn)
}

// AdaptiveInterval stretches base according to how much of the bucket's
// budget is left: 2× below 20 %, 4× below 10 %, 8× below 5 %. It returns
// base unchanged when the budget is unknown (limit ≤ 0) or healthy.
func AdaptiveInterval(base time.Duration, remaining, limit int) time.Duration {
	if base <= 0 || limit <= 0 {
		return base
	}
	if remaining < 0 {
		remaining = 0
	}
	frac := float64(remaining) / float64(limit)
	for _, rung := range adaptiveIntervalLadder {
		if frac < rung.below {
			return base * rung.factor
		}
	}
	return base
}

type forgestateInstruments struct {
	drift        metric.Int64Counter
	eventToPatch metric.Float64Histogram
}

var (
	forgestateInstrumentsOnce sync.Once
	forgestateInstrumentsVal  forgestateInstruments
)

func instruments() forgestateInstruments {
	forgestateInstrumentsOnce.Do(func() {
		m := tracing.Meter(forgestateInstrumentationScope)
		forgestateInstrumentsVal.drift, _ = m.Int64Counter(MetricDriftTotal,
			metric.WithDescription("Items a reconcile found missing, extra or changed in the cache."))
		forgestateInstrumentsVal.eventToPatch, _ = m.Float64Histogram(MetricEventToPatchMS,
			metric.WithDescription("Milliseconds from webhook receipt to the cache patch being applied."),
			metric.WithUnit("ms"))
	})
	return forgestateInstrumentsVal
}

// recordDrift adds n to forgestate.drift{repo,kind}.
func recordDrift(ctx context.Context, repo string, kind Kind, n int) {
	if n <= 0 {
		return
	}
	instruments().drift.Add(ctx, int64(n), metric.WithAttributes(
		attribute.String("repo", repo),
		attribute.String("kind", string(kind)),
	))
}

// RecordEventToPatch records one webhook.event_to_patch_ms sample for repo:
// the time from receivedAt (stamped when the delivery arrived) to applied.
// writer names the cache the patch landed in. A zero receivedAt records
// nothing — the delivery was not timed at receipt.
func RecordEventToPatch(ctx context.Context, repo, writer string, receivedAt, applied time.Time) {
	if receivedAt.IsZero() {
		return
	}
	ms := float64(applied.Sub(receivedAt)) / float64(time.Millisecond)
	if ms < 0 {
		ms = 0
	}
	instruments().eventToPatch.Record(ctx, ms, metric.WithAttributes(
		attribute.String("repo", repo),
		attribute.String("writer", writer),
	))
}

// registerStaleness registers the forgestate.staleness_s observable gauge
// over s's repos. The returned registration is released by Store.Close.
//
// The gauge is created on the same meter handle the callback is registered
// with, never cached across Stores: an observable created through the global
// delegate before pkg/tracing.Init installs the SDK is rejected by the SDK
// meter afterwards ("from different implementation"). Creating it per
// registration keeps instrument and callback on one implementation.
func (s *Store) registerStaleness() metric.Registration {
	m := tracing.Meter(forgestateInstrumentationScope)
	staleness, err := m.Int64ObservableGauge(MetricStalenessSeconds,
		metric.WithDescription("Seconds since the last good reconcile while the snapshot is stale; 0 when fresh."),
		metric.WithUnit("s"))
	if err != nil {
		return nil
	}
	reg, err := m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		now := s.opts.Now()
		s.mu.RLock()
		defer s.mu.RUnlock()
		for _, key := range s.order {
			rs := s.repos[key]
			var secs int64
			if rs.stale && !rs.lastGoodAt.IsZero() {
				secs = int64(now.Sub(rs.lastGoodAt) / time.Second)
			} else if rs.stale {
				secs = int64(now.Sub(rs.staleSince) / time.Second)
			}
			o.ObserveInt64(staleness, secs, metric.WithAttributes(attribute.String("repo", rs.name)))
		}
		return nil
	}, staleness)
	if err != nil {
		return nil
	}
	return reg
}

// budget returns the configured budget source or the process default.
func (s *Store) budget() BudgetFunc {
	if s.opts.Budget != nil {
		return s.opts.Budget
	}
	if p := defaultBudget.Load(); p != nil {
		return *p
	}
	return nil
}
