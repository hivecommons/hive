package hub

import (
	"encoding/json"
	"fmt"
	"testing"

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
	}}
	in.Evidence.Calibration[5] = spoke.HeartbeatEvidenceBucket{N: 9, Bad: 1}
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
