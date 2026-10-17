package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func evidenceCriterionByName(t *testing.T, axes []EvidenceAxisCriteria, axis, name string) EvidenceCriterion {
	t.Helper()
	for _, a := range axes {
		if a.Axis != axis {
			continue
		}
		for _, c := range a.Criteria {
			if c.Name == name {
				return c
			}
		}
	}
	t.Fatalf("criterion %s/%s not found in %+v", axis, name, axes)
	return EvidenceCriterion{}
}

func TestEvidenceCriteriaForMetUnmetNotReported(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	axes := evidenceCriteriaFor(RegistryEntry{ID: "a", Online: true, ACMMLevel: 1,
		Evidence: &HeartbeatEvidence{Merged90d: 20, Reverted14d90d: 0}}, now)

	if len(axes) != len(QuadrantAxisOrder) {
		t.Fatalf("axes = %d, want %d", len(axes), len(QuadrantAxisOrder))
	}
	for i, a := range axes {
		if a.Axis != QuadrantAxisOrder[i] || a.Label != QuadrantAxisLabels[a.Axis] {
			t.Errorf("axis %d = %q/%q, want %q", i, a.Axis, a.Label, QuadrantAxisOrder[i])
		}
	}

	revert := evidenceCriterionByName(t, axes, AxisTrust, critRevert)
	if revert.Status != EvidenceCriterionMet || revert.Score == nil || *revert.Score != 100 || revert.Display == "" {
		t.Errorf("zero reverts over 20 merges must be met: %+v", revert)
	}
	autonomy := evidenceCriterionByName(t, axes, AxisTrust, critAutonomy)
	if autonomy.Status != EvidenceCriterionUnmet || autonomy.Score == nil || *autonomy.Score != 0 {
		t.Errorf("ACMM L1 must be unmet: %+v", autonomy)
	}
	approve := evidenceCriterionByName(t, axes, AxisTrust, critFalseApprove)
	if approve.Status != EvidenceCriterionNotReported || approve.Score != nil || approve.Reason != reasonFewVerdicts {
		t.Errorf("a criterion without enough input must be not reported, never unmet: %+v", approve)
	}
	if !axes[0].Scored {
		t.Error("trust has measured criteria and must be scored")
	}
}

func TestEvidenceCriteriaMetThreshold(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		level int
		want  string
	}{{3, EvidenceCriterionUnmet}, {4, EvidenceCriterionMet}, {6, EvidenceCriterionMet}} {
		axes := evidenceCriteriaFor(RegistryEntry{ID: "a", Online: true, ACMMLevel: tc.level, Evidence: &HeartbeatEvidence{}}, now)
		if got := evidenceCriterionByName(t, axes, AxisTrust, critAutonomy); got.Status != tc.want {
			t.Errorf("L%d autonomy = %s (score %v), want %s", tc.level, got.Status, got.Score, tc.want)
		}
	}
}

// Offline and evidence-less hives are "not reported" on every criterion,
// never "unmet" — including work source, which the quadrant always measures.
func TestEvidenceCriteriaOfflineAndUnreportedNeverUnmet(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		entry  RegistryEntry
		reason string
	}{
		"offline": {RegistryEntry{ID: "off", ACMMLevel: 1,
			Evidence: &HeartbeatEvidence{Merged90d: 20, Reverted14d90d: 10}}, evidenceCriteriaOffline},
		"no evidence": {RegistryEntry{ID: "bare", Online: true, ACMMLevel: 1}, evidenceCriteriaNoEvidence},
	} {
		axes := evidenceCriteriaFor(tc.entry, now)
		if len(axes) != len(QuadrantAxisOrder) {
			t.Fatalf("%s: axes = %d", name, len(axes))
		}
		for _, a := range axes {
			if a.Scored {
				t.Errorf("%s: axis %s must not be scored", name, a.Axis)
			}
			if len(a.Criteria) == 0 {
				t.Errorf("%s: axis %s lists no criteria", name, a.Axis)
			}
			for _, c := range a.Criteria {
				if c.Status != EvidenceCriterionNotReported || c.Score != nil || c.Display != "" || c.Reason != tc.reason {
					t.Errorf("%s: %s/%s = %+v, want not reported (%s)", name, a.Axis, c.Name, c, tc.reason)
				}
			}
		}
	}
}

func TestBuildEvidenceViewCarriesCriteria(t *testing.T) {
	v := buildEvidenceView([]RegistryEntry{
		{ID: "a", Online: true, ACMMLevel: 6, Evidence: &HeartbeatEvidence{Merged90d: 20}},
		{ID: "b"},
	})
	if len(v.Hives) != 2 {
		t.Fatalf("hives = %d", len(v.Hives))
	}
	for _, h := range v.Hives {
		if len(h.Criteria) != len(QuadrantAxisOrder) {
			t.Errorf("hive %s criteria = %d axes", h.ID, len(h.Criteria))
		}
	}
	if c := evidenceCriterionByName(t, v.Hives[0].Criteria, AxisTrust, critAutonomy); c.Status != EvidenceCriterionMet {
		t.Errorf("L6 autonomy = %+v", c)
	}
	if c := evidenceCriterionByName(t, v.Hives[1].Criteria, AxisProductivity, critWorkSource); c.Status != EvidenceCriterionNotReported {
		t.Errorf("an offline hive's work source must be not reported: %+v", c)
	}

	raw, err := json.Marshal(v.Hives[0])
	if err != nil {
		t.Fatal(err)
	}
	var shape struct {
		Criteria []struct {
			Axis     string `json:"axis"`
			Label    string `json:"label"`
			Scored   *bool  `json:"scored"`
			Criteria []struct {
				Name   string `json:"name"`
				Label  string `json:"label"`
				Status string `json:"status"`
				Score  *int   `json:"score"`
				Reason string `json:"reason"`
			} `json:"criteria"`
		} `json:"criteria"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	if len(shape.Criteria) != len(QuadrantAxisOrder) || shape.Criteria[0].Axis != AxisTrust || shape.Criteria[0].Scored == nil {
		t.Fatalf("criteria JSON shape: %s", raw)
	}
	statuses := map[string]bool{}
	for _, a := range shape.Criteria {
		for _, c := range a.Criteria {
			statuses[c.Status] = true
			if c.Name == "" || c.Label == "" {
				t.Errorf("criterion without name/label: %+v", c)
			}
			if (c.Status == EvidenceCriterionNotReported) != (c.Score == nil) {
				t.Errorf("score must be present exactly when measured: %+v", c)
			}
		}
	}
	for s := range statuses {
		if s != EvidenceCriterionMet && s != EvidenceCriterionUnmet && s != EvidenceCriterionNotReported {
			t.Errorf("unexpected status %q", s)
		}
	}
	if !strings.Contains(string(raw), `"status":"not_reported"`) {
		t.Errorf("expected a not_reported criterion in %s", raw)
	}
}
