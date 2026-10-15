package hub

import "testing"

func TestSanitizeEvidenceBounds(t *testing.T) {
	if sanitizeEvidence(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	got := sanitizeEvidence(&HeartbeatEvidence{
		Verdicts90d: 20, FalseApprove90d: 99, FalseBlock90d: -3,
		Merged90d: 5, Reverted14d90d: 9,
		Calibration: [EvidenceCalibrationBuckets]EvidenceBucket{{N: 4, Bad: 8}},
	})
	if got.FalseApprove90d != 20 || got.FalseBlock90d != 0 || got.Reverted14d90d != 5 || got.Calibration[0].Bad != 4 {
		t.Fatalf("not clamped: %+v", got)
	}
}

func TestBuildEvidenceViewSmallNIsUnknown(t *testing.T) {
	v := buildEvidenceView([]RegistryEntry{
		{ID: "a", Evidence: &HeartbeatEvidence{Verdicts90d: 20, FalseBlock90d: 5, Merged90d: 3, Reverted14d90d: 1,
			Calibration: [EvidenceCalibrationBuckets]EvidenceBucket{{N: 12, Bad: 6}}}},
		{ID: "b"},
	})
	if v.Spokes != 2 || v.Reporting != 1 || v.Hives[1].Reported {
		t.Fatalf("coverage wrong: %+v", v)
	}
	a := v.Hives[0]
	if a.FalseBlockRate.Rate == nil || *a.FalseBlockRate.Rate != 0.25 {
		t.Fatalf("false block rate: %+v", a.FalseBlockRate)
	}
	if a.RevertRate.Rate != nil {
		t.Fatal("n<10 must be unknown")
	}
	if v.Calibration[0].Rate == nil || *v.Calibration[0].Rate != 0.5 || v.Calibration[1].Rate != nil {
		t.Fatalf("pooled calibration: %+v", v.Calibration)
	}
}

func TestEvidenceCriteria(t *testing.T) {
	yes := true
	in := quadrantInputs{selfMergeAllowed: &yes, evidence: &HeartbeatEvidence{
		Verdicts90d: 20, FalseBlock90d: 4, FalseApprove90d: 1, Merged90d: 20, Reverted14d90d: 4}}
	findCriterion(t, trustCriteria(in), "delegation")
	findCriterion(t, efficiencyCriteria(in), "revert_rate")
	findCriterion(t, satisfactionCriteria(in), "human_override_rate")
	if got := satisfactionCriteria(quadrantInputs{evidence: &HeartbeatEvidence{Verdicts90d: 9}}); len(got) != 0 {
		t.Fatalf("<10 verdicts must be unscored: %v", got)
	}
}
