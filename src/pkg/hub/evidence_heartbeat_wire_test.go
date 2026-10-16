package hub

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"

	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// TestSpokeEvidenceWireCompat pins that the spoke's evidence block decodes
// into the hub's HeartbeatEvidence field-for-field and survives sanitizing.
func TestSpokeEvidenceWireCompat(t *testing.T) {
	if spoke.HeartbeatEvidenceCalibrationBuckets != EvidenceCalibrationBuckets {
		t.Fatalf("calibration buckets: spoke %d, hub %d", spoke.HeartbeatEvidenceCalibrationBuckets, EvidenceCalibrationBuckets)
	}
	yes := true
	in := spoke.HeartbeatPayload{Evidence: &spoke.HeartbeatEvidence{
		Verdicts90d: 30, FalseApprove90d: 2, FalseBlock90d: 3, Merged90d: 12, Reverted14d90d: 1,
		AutomergeSelfAuthored: &yes,
		ReviewerFamilies:      []string{"claude", "gpt"},
		AuthorFamilies:        []string{"gpt"},
		HiveState: &hivestate.Trends{
			Last24h: hivestate.Window{Samples: 720, CoverageHours: 23.9, QueueIssues: hivestate.Quantiles{Median: 4, P90: 9}, SLAViolationRate: 0.1, AgentIdleRatio: 0.4, KicksPerDay: 30, TokensPerDay: 1e6},
			Last7d:  hivestate.Window{Samples: 1000, QueuePRs: hivestate.Quantiles{Median: 2, P90: 3}},
		},
	}}
	in.Evidence.Calibration[5] = spoke.HeartbeatEvidenceBucket{N: 9, Bad: 1}
	in.Evidence.OutcomesByAuthorKind = map[string]spoke.HeartbeatEvidenceOutcome{"agent": {N: 8, Merged: 6, Reverted: 1}}
	in.Evidence.WriteCounters = map[string]spoke.HeartbeatEvidenceWriteCount{"guarded": {Attempted: 20, Blocked: 3}}
	in.Evidence.ShadowGate = &spoke.HeartbeatEvidenceShadow{Intent: spoke.HeartbeatEvidenceShadowGate{Held: spoke.HeartbeatEvidenceOutcome{N: 2, Merged: 1}}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out HeartbeatPayload
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	got := sanitizeEvidence(out.Evidence)
	if got == nil || got.Verdicts90d != 30 || got.FalseApprove90d != 2 || got.FalseBlock90d != 3 ||
		got.Merged90d != 12 || got.Reverted14d90d != 1 || got.Calibration[5] != (EvidenceBucket{N: 9, Bad: 1}) {
		t.Fatalf("decoded = %+v", got)
	}
	if got.AutomergeSelfAuthored == nil || !*got.AutomergeSelfAuthored {
		t.Errorf("automerge_self_authored = %v", got.AutomergeSelfAuthored)
	}
	if len(got.ReviewerFamilies) != 2 || len(got.AuthorFamilies) != 1 {
		t.Errorf("families = %v / %v", got.ReviewerFamilies, got.AuthorFamilies)
	}
	// Fleet-panel blocks (#12370) cross the wire field-for-field.
	if got.OutcomesByAuthorKind["agent"] != (EvidenceOutcome{N: 8, Merged: 6, Reverted: 1}) {
		t.Errorf("outcomes_by_author_kind = %+v", got.OutcomesByAuthorKind)
	}
	if got.WriteCounters["guarded"] != (EvidenceWriteCount{Attempted: 20, Blocked: 3}) {
		t.Errorf("write_counters = %+v", got.WriteCounters)
	}
	if got.ShadowGate == nil || got.ShadowGate.Intent.Held != (EvidenceOutcome{N: 2, Merged: 1}) || got.ShadowGate.ReviewGate != (EvidenceShadowGate{}) {
		t.Errorf("shadow_gate = %+v", got.ShadowGate)
	}
	if got.HiveState == nil || *got.HiveState != *in.Evidence.HiveState {
		t.Errorf("hive_state = %+v, want %+v", got.HiveState, in.Evidence.HiveState)
	}

	// A spoke predating #12370 sends the block without the panel keys.
	raw, err = json.Marshal(spoke.HeartbeatPayload{Evidence: &spoke.HeartbeatEvidence{Verdicts90d: 1}})
	if err != nil {
		t.Fatal(err)
	}
	out = HeartbeatPayload{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if got := sanitizeEvidence(out.Evidence); got.OutcomesByAuthorKind != nil || got.WriteCounters != nil || got.ShadowGate != nil {
		t.Fatalf("pre-#12370 spoke must leave the panel blocks nil: %+v", got)
	}

	// An old spoke sends no evidence key: the hub sees nil, not a zero block.
	raw, err = json.Marshal(spoke.HeartbeatPayload{})
	if err != nil {
		t.Fatal(err)
	}
	out = HeartbeatPayload{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Evidence != nil || sanitizeEvidence(out.Evidence) != nil {
		t.Fatalf("old spoke evidence = %+v, want nil", out.Evidence)
	}
}

func TestSanitizeEvidenceDropsMalformedFamilies(t *testing.T) {
	var many []string
	for i := 0; i < maxEvidenceFamilies+5; i++ {
		many = append(many, fmt.Sprintf("fam%d", i))
	}
	got := sanitizeEvidence(&HeartbeatEvidence{
		Verdicts90d:      maxEvidenceCount + 1,
		ReviewerFamilies: append([]string{"", "  ", "claude", "claude"}, many...),
	})
	if got.Verdicts90d != maxEvidenceCount {
		t.Errorf("verdicts = %d, want clamp to %d", got.Verdicts90d, maxEvidenceCount)
	}
	if len(got.ReviewerFamilies) != maxEvidenceFamilies || got.ReviewerFamilies[0] != "claude" {
		t.Errorf("families = %v, want %d entries starting with claude", got.ReviewerFamilies, maxEvidenceFamilies)
	}
}

func TestSanitizeHiveStateBounds(t *testing.T) {
	if sanitizeHiveState(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	if got := sanitizeEvidence(&HeartbeatEvidence{}); got.HiveState != nil {
		t.Fatalf("absent hive_state = %+v, want nil", got.HiveState)
	}
	bad := hivestate.Window{
		Samples: -1, CoverageHours: 1000,
		QueueIssues:      hivestate.Quantiles{Median: 5, P90: 2},
		QueuePRs:         hivestate.Quantiles{Median: -3, P90: maxEvidenceCount + 1},
		SLAViolationRate: 2, AgentIdleRatio: math.NaN(),
		KicksPerDay: -4, TokensPerDay: math.Inf(1),
	}
	got := sanitizeEvidence(&HeartbeatEvidence{HiveState: &hivestate.Trends{Last24h: bad, Last7d: bad}}).HiveState
	if got == nil {
		t.Fatal("hive_state dropped")
	}
	for name, w := range map[string]struct {
		w     hivestate.Window
		hours float64
	}{"24h": {got.Last24h, 24}, "7d": {got.Last7d, 168}} {
		want := hivestate.Window{
			CoverageHours:    w.hours,
			QueueIssues:      hivestate.Quantiles{Median: 5, P90: 5},
			QueuePRs:         hivestate.Quantiles{Median: 0, P90: maxEvidenceCount},
			SLAViolationRate: 1,
			TokensPerDay:     maxHiveStatePerDay,
		}
		if w.w != want {
			t.Errorf("%s = %+v, want %+v", name, w.w, want)
		}
	}
}
