package config

import "testing"

// TestIoscanFailModeForLevel checks the ioscan_fail_mode default resolves from
// the embedded pack YAML: "closed" for the high-trust levels (L5/L6) where
// agents can merge, empty (fail-open) for the lower levels, and empty for
// unknown levels.
func TestIoscanFailModeForLevel(t *testing.T) {
	cases := []struct {
		level int
		want  string
	}{
		{1, ""},
		{2, ""},
		{3, ""},
		{4, ""},
		{5, "closed"},
		{6, "closed"},
		{99, ""}, // unknown level → safe default (fail-open)
	}
	for _, c := range cases {
		if got := IoscanFailModeForLevel(c.level); got != c.want {
			t.Errorf("IoscanFailModeForLevel(%d) = %q, want %q", c.level, got, c.want)
		}
	}
}

// TestPackGovernorIoscanFailModeField verifies the field is parsed onto
// PackGovernor for the levels that set it.
func TestPackGovernorIoscanFailModeField(t *testing.T) {
	for _, level := range []int{5, 6} {
		p, err := ACMMPackByLevel(level)
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		if p.Governor.IoscanFailMode != "closed" {
			t.Errorf("level %d pack ioscan_fail_mode = %q, want closed", level, p.Governor.IoscanFailMode)
		}
	}
	for _, level := range []int{1, 2, 3, 4} {
		p, err := ACMMPackByLevel(level)
		if err != nil {
			t.Fatalf("level %d: %v", level, err)
		}
		if p.Governor.IoscanFailMode != "" {
			t.Errorf("level %d pack ioscan_fail_mode = %q, want empty (fail-open)", level, p.Governor.IoscanFailMode)
		}
	}
}
