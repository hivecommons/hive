package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestKickOutcome(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, kickOutcomeOK},
		{fmt.Errorf("%w: agent a restarted mid-send", errKickCancelledByRestart), kickOutcomeCancelled},
		{errors.New("agent a not found"), kickOutcomeError},
	}
	for _, tc := range cases {
		if got := kickOutcome(tc.err); got != tc.want {
			t.Errorf("kickOutcome(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// The instruments export one duration point and one outcome count per kick,
// labelled by exactly agent.role and outcome — no other attribute may leak in.
func TestKickMetricsRecordBoundedAttributes(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	k := newKickMetrics(mp.Meter(spanInstrumentationName))

	k.record("scanner", 2*time.Second, nil)
	k.record(kickRoleUnknown, time.Millisecond, errors.New("agent x not found"))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	okSet := attribute.NewSet(attrKickRole.String("scanner"), attrKickOutcome.String(kickOutcomeOK))
	errSet := attribute.NewSet(attrKickRole.String(kickRoleUnknown), attrKickOutcome.String(kickOutcomeError))
	want := map[attribute.Distinct]bool{
		okSet.Equivalent():  true,
		errSet.Equivalent(): true,
	}
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen[m.Name] = true
			switch data := m.Data.(type) {
			case metricdata.Histogram[float64]:
				if m.Unit != "s" {
					t.Errorf("%s unit = %q, want s", m.Name, m.Unit)
				}
				for _, dp := range data.DataPoints {
					if !want[dp.Attributes.Equivalent()] || dp.Count != 1 {
						t.Errorf("%s point %v count=%d unexpected", m.Name, dp.Attributes.ToSlice(), dp.Count)
					}
				}
			case metricdata.Sum[int64]:
				for _, dp := range data.DataPoints {
					if !want[dp.Attributes.Equivalent()] || dp.Value != 1 {
						t.Errorf("%s point %v value=%d unexpected", m.Name, dp.Attributes.ToSlice(), dp.Value)
					}
				}
			default:
				t.Errorf("%s: unexpected data type %T", m.Name, m.Data)
			}
		}
	}
	for _, name := range []string{kickDurationMetric, kickOutcomeMetric} {
		if !seen[name] {
			t.Errorf("metric %s not exported", name)
		}
	}
}
