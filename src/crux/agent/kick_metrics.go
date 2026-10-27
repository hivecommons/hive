package agent

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Kick instruments (hivecommons/crux#19). They are created on the global
// MeterProvider, which is OTel's delegating no-op until a host registers an
// SDK provider (crux/telemetry for the crux binary, pkg/tracing for hive), so
// recording costs nothing by default and starts exporting once one is set.
//
// Attributes are bounded enums only: the agent role (a configured agent name,
// or kickRoleUnknown for a name the manager does not know, so caller input
// never becomes a label) and the outcome. Never issue/PR numbers, run ids or
// free text.
const (
	kickDurationMetric = "crux.agent.kick.duration"
	kickOutcomeMetric  = "crux.agent.kick.outcomes"

	kickRoleUnknown = "unknown"

	kickOutcomeOK        = "ok"
	kickOutcomeCancelled = "cancelled"
	kickOutcomeError     = "error"
)

var (
	attrKickRole    = attribute.Key("agent.role")
	attrKickOutcome = attribute.Key("outcome")
)

// kickMetrics holds the two kick instruments.
type kickMetrics struct {
	duration metric.Float64Histogram
	outcomes metric.Int64Counter
}

func newKickMetrics(meter metric.Meter) kickMetrics {
	// Instrument constructors only fail on an invalid name or unit; both are
	// constants here, and the returned instrument is a usable no-op either way.
	duration, _ := meter.Float64Histogram(kickDurationMetric,
		metric.WithUnit("s"),
		metric.WithDescription("Time SendKick took to deliver, defer or reject a kick."))
	outcomes, _ := meter.Int64Counter(kickOutcomeMetric,
		metric.WithDescription("Kicks handled by SendKick, by agent role and outcome."))
	return kickMetrics{duration: duration, outcomes: outcomes}
}

var globalKickMetrics = sync.OnceValue(func() kickMetrics {
	return newKickMetrics(otel.Meter(spanInstrumentationName))
})

// kickOutcome maps SendKick's result onto the fixed outcome enum.
func kickOutcome(err error) string {
	switch {
	case err == nil:
		return kickOutcomeOK
	case errors.Is(err, errKickCancelledByRestart):
		return kickOutcomeCancelled
	default:
		return kickOutcomeError
	}
}

// recordKick records one SendKick call's duration and outcome.
func recordKick(role string, start time.Time, err error) {
	globalKickMetrics().record(role, time.Since(start), err)
}

func (k kickMetrics) record(role string, d time.Duration, err error) {
	attrs := metric.WithAttributes(attrKickRole.String(role), attrKickOutcome.String(kickOutcome(err)))
	ctx := context.Background()
	k.duration.Record(ctx, d.Seconds(), attrs)
	k.outcomes.Add(ctx, 1, attrs)
}
