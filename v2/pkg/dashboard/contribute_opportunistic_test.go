package dashboard

import "testing"

func TestOpportunisticHeat_FreshIssue(t *testing.T) {
	// A brand-new issue (age 0) should get maximum recency heat.
	heat, reason := opportunisticHeat(0, true, nil)
	if heat != recencyHeatMax {
		t.Errorf("age 0 heat = %f, want %f", heat, recencyHeatMax)
	}
	if reason != "opened just now" {
		t.Errorf("reason = %q, want %q", reason, "opened just now")
	}
}

func TestOpportunisticHeat_HalfLifeIssue(t *testing.T) {
	// At half the recency floor the heat should be ~half the max.
	halfAge := recencyHeatFloorMinutes / 2
	heat, _ := opportunisticHeat(halfAge, true, nil)
	expected := recencyHeatMax * 0.5
	if heat < expected-0.1 || heat > expected+0.1 {
		t.Errorf("half-age heat = %f, want ~%f", heat, expected)
	}
}

func TestOpportunisticHeat_OldIssue(t *testing.T) {
	// An issue older than the recency floor gets zero recency heat.
	heat, reason := opportunisticHeat(recencyHeatFloorMinutes+1, true, nil)
	if heat != 0 {
		t.Errorf("old issue heat = %f, want 0", heat)
	}
	if reason != "actionable" {
		t.Errorf("reason = %q, want %q", reason, "actionable")
	}
}

func TestOpportunisticHeat_NoAge(t *testing.T) {
	// When haveAge is false, recency heat is 0.
	heat, reason := opportunisticHeat(0, false, nil)
	if heat != 0 {
		t.Errorf("no-age heat = %f, want 0", heat)
	}
	if reason != "actionable" {
		t.Errorf("reason = %q, want %q", reason, "actionable")
	}
}

func TestOpportunisticHeat_LabelBump(t *testing.T) {
	// A label bump adds to heat; matching label provides reason when no recency.
	heat, reason := opportunisticHeat(recencyHeatFloorMinutes+1, true, []string{"Good First Issue"})
	if heat != 3 {
		t.Errorf("label-only heat = %f, want 3", heat)
	}
	if reason != "good first issue" {
		t.Errorf("reason = %q, want %q", reason, "good first issue")
	}
}

func TestOpportunisticHeat_RecencyPlusLabel(t *testing.T) {
	// Recency + label should combine additively; reason comes from recency.
	heat, reason := opportunisticHeat(0, true, []string{"bug"})
	expected := recencyHeatMax + 1.5 // bug bump
	if heat != expected {
		t.Errorf("heat = %f, want %f", heat, expected)
	}
	if reason != "opened just now" {
		t.Errorf("reason = %q, want %q", reason, "opened just now")
	}
}

func TestOpportunisticHeat_NegativeAge(t *testing.T) {
	// Negative age should produce zero recency (guard against clock skew).
	heat, _ := opportunisticHeat(-5, true, nil)
	if heat != 0 {
		t.Errorf("negative age heat = %f, want 0", heat)
	}
}

func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		m    int
		want string
	}{
		{0, "just now"},
		{1, "just now"},
		{2, "2m ago"},
		{59, "59m ago"},
		{60, "1h ago"},
		{119, "1h ago"},
		{120, "2h ago"},
		{1440, "1d ago"},
		{2880, "2d ago"},
	}
	for _, c := range cases {
		got := humanizeAge(c.m)
		if got != c.want {
			t.Errorf("humanizeAge(%d) = %q, want %q", c.m, got, c.want)
		}
	}
}
