package dashboard

import (
	"testing"
)

func TestOpportunisticHeat_BrandNewIssue(t *testing.T) {
	heat, reason := opportunisticHeat(0, true, nil)
	// Age 0 with known age should get max recency heat (10.0).
	if heat != recencyHeatMax {
		t.Errorf("heat=%f, want %f", heat, recencyHeatMax)
	}
	if reason != "opened just now" {
		t.Errorf("reason=%q, want 'opened just now'", reason)
	}
}

func TestOpportunisticHeat_MidAge(t *testing.T) {
	// 7 days = half the 14-day floor → heat = 5.0
	age := 7 * 24 * 60
	heat, reason := opportunisticHeat(age, true, nil)
	expected := recencyHeatMax * (1 - float64(age)/float64(recencyHeatFloorMinutes))
	if heat != expected {
		t.Errorf("heat=%f, want %f", heat, expected)
	}
	if reason == "" {
		t.Error("expected non-empty reason")
	}
}

func TestOpportunisticHeat_AtFloor(t *testing.T) {
	// At or above floor → zero recency heat.
	heat, reason := opportunisticHeat(recencyHeatFloorMinutes, true, nil)
	if heat != 0 {
		t.Errorf("heat=%f, want 0 (at floor)", heat)
	}
	if reason != "actionable" {
		t.Errorf("reason=%q, want 'actionable'", reason)
	}
}

func TestOpportunisticHeat_UnknownAge(t *testing.T) {
	// When haveAge is false, no recency heat.
	heat, reason := opportunisticHeat(5, false, nil)
	if heat != 0 {
		t.Errorf("heat=%f, want 0 (unknown age)", heat)
	}
	if reason != "actionable" {
		t.Errorf("reason=%q, want 'actionable'", reason)
	}
}

func TestOpportunisticHeat_LabelBump(t *testing.T) {
	// Unknown age + "good first issue" label → label bump only.
	heat, reason := opportunisticHeat(0, false, []string{"Good First Issue"})
	if heat != 3.0 {
		t.Errorf("heat=%f, want 3.0 (good first issue bump)", heat)
	}
	if reason != "good first issue" {
		t.Errorf("reason=%q, want 'good first issue'", reason)
	}
}

func TestOpportunisticHeat_RecencyPlusLabel(t *testing.T) {
	// Brand new issue + bug label → max recency + bug bump.
	heat, _ := opportunisticHeat(0, true, []string{"bug"})
	expected := recencyHeatMax + 1.5
	if heat != expected {
		t.Errorf("heat=%f, want %f", heat, expected)
	}
}

func TestOpportunisticHeat_MultipleLabels(t *testing.T) {
	// Multiple matching labels stack.
	heat, _ := opportunisticHeat(recencyHeatFloorMinutes, true, []string{"help wanted", "enhancement"})
	expected := 2.0 + 1.0 // help wanted + enhancement
	if heat != expected {
		t.Errorf("heat=%f, want %f (stacked label bumps)", heat, expected)
	}
}

func TestHumanizeAge(t *testing.T) {
	tests := []struct {
		minutes int
		want    string
	}{
		{0, "just now"},
		{1, "just now"},
		{2, "2m ago"},
		{59, "59m ago"},
		{60, "1h ago"},
		{90, "1h ago"},
		{120, "2h ago"},
		{23*60 + 59, "23h ago"},
		{24 * 60, "1d ago"},
		{48 * 60, "2d ago"},
		{7 * 24 * 60, "7d ago"},
	}
	for _, tt := range tests {
		got := humanizeAge(tt.minutes)
		if got != tt.want {
			t.Errorf("humanizeAge(%d) = %q, want %q", tt.minutes, got, tt.want)
		}
	}
}
