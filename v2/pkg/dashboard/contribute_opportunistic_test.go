package dashboard

import (
	"testing"
)

// ── opportunisticHeat ─────────────────────────────────────────────────────────

func TestOpportunisticHeatBrandNew(t *testing.T) {
	heat, reason := opportunisticHeat(0, true, nil)
	if heat != recencyHeatMax {
		t.Errorf("age=0 heat = %f, want %f", heat, recencyHeatMax)
	}
	if reason != "opened just now" {
		t.Errorf("age=0 reason = %q, want %q", reason, "opened just now")
	}
}

func TestOpportunisticHeatAtFloorIsZero(t *testing.T) {
	heat, _ := opportunisticHeat(recencyHeatFloorMinutes, true, nil)
	if heat != 0 {
		t.Errorf("age=floor heat = %f, want 0", heat)
	}
}

func TestOpportunisticHeatOlderThanFloorIsZero(t *testing.T) {
	heat, reason := opportunisticHeat(recencyHeatFloorMinutes+1000, true, nil)
	if heat != 0 {
		t.Errorf("age>floor heat = %f, want 0", heat)
	}
	if reason != "actionable" {
		t.Errorf("old issue reason = %q, want %q", reason, "actionable")
	}
}

func TestOpportunisticHeatNoAge(t *testing.T) {
	heat, reason := opportunisticHeat(0, false, nil)
	if heat != 0 {
		t.Errorf("no-age heat = %f, want 0", heat)
	}
	if reason != "actionable" {
		t.Errorf("no-age reason = %q, want %q", reason, "actionable")
	}
}

func TestOpportunisticHeatLabelBump(t *testing.T) {
	heat, reason := opportunisticHeat(0, false, []string{"Good First Issue"})
	expectedBump := opportunisticLabelHeat["good first issue"]
	if heat != expectedBump {
		t.Errorf("label-only heat = %f, want %f", heat, expectedBump)
	}
	if reason != "good first issue" {
		t.Errorf("label reason = %q, want %q", reason, "good first issue")
	}
}

func TestOpportunisticHeatRecencyPlusLabel(t *testing.T) {
	age := recencyHeatFloorMinutes / 2 // half the floor -> half recency
	heat, _ := opportunisticHeat(age, true, []string{"bug"})
	expectedRecency := 0.5 * recencyHeatMax
	expectedBump := opportunisticLabelHeat["bug"]
	expected := expectedRecency + expectedBump
	if heat < expected-0.01 || heat > expected+0.01 {
		t.Errorf("recency+label heat = %f, want ~%f", heat, expected)
	}
}

func TestOpportunisticHeatNegativeAge(t *testing.T) {
	heat, _ := opportunisticHeat(-5, true, nil)
	if heat != 0 {
		t.Errorf("negative age heat = %f, want 0", heat)
	}
}

func TestOpportunisticHeatMultipleLabels(t *testing.T) {
	heat, _ := opportunisticHeat(0, false, []string{"good first issue", "help wanted", "bug"})
	expected := opportunisticLabelHeat["good first issue"] +
		opportunisticLabelHeat["help wanted"] +
		opportunisticLabelHeat["bug"]
	if heat < expected-0.01 || heat > expected+0.01 {
		t.Errorf("multi-label heat = %f, want %f", heat, expected)
	}
}

func TestOpportunisticHeatUnknownLabelIgnored(t *testing.T) {
	heat, reason := opportunisticHeat(0, false, []string{"wontfix", "duplicate"})
	if heat != 0 {
		t.Errorf("unknown labels heat = %f, want 0", heat)
	}
	if reason != "actionable" {
		t.Errorf("unknown labels reason = %q, want %q", reason, "actionable")
	}
}

// ── humanizeAge ───────────────────────────────────────────────────────────────

func TestHumanizeAgeOpportunistic(t *testing.T) {
	tests := []struct {
		minutes int
		want    string
	}{
		{0, "just now"},
		{1, "just now"},
		{2, "2m ago"},
		{59, "59m ago"},
		{60, "1h ago"},
		{119, "1h ago"},
		{120, "2h ago"},
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

// ── constants sanity ──────────────────────────────────────────────────────────

func TestOpportunisticConstants(t *testing.T) {
	if opportunisticDefaultLimit <= 0 {
		t.Error("default limit must be positive")
	}
	if opportunisticFrontWindow < 0 {
		t.Error("front window must be non-negative")
	}
	if recencyHeatFloorMinutes <= 0 {
		t.Error("recency floor must be positive")
	}
	if recencyHeatMax <= 0 {
		t.Error("recency max must be positive")
	}
}
