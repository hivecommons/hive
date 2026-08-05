package config

import "testing"

// TestIsContributeCooldownEnabled_DefaultsOnWhenUnset pins the backward-compatible
// default: a nil pointer (config that predates the toggle) resolves to ENABLED,
// while an explicit false disables it and an explicit true enables it.
func TestIsContributeCooldownEnabled_DefaultsOnWhenUnset(t *testing.T) {
	trueVal := true
	falseVal := false
	cases := []struct {
		name string
		ptr  *bool
		want bool
	}{
		{"nil (unset) defaults on", nil, true},
		{"explicit true", &trueVal, true},
		{"explicit false", &falseVal, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := HubConfig{ContributeCooldownEnabled: tc.ptr}
			if got := h.IsContributeCooldownEnabled(); got != tc.want {
				t.Errorf("IsContributeCooldownEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestContributeCooldownHoursOrDefault covers the resolver: 0/unset -> default
// (168), a positive in-range value passes through, and out-of-range values are
// clamped to [min,max].
func TestContributeCooldownHoursOrDefault(t *testing.T) {
	cases := []struct {
		name  string
		hours int
		want  int
	}{
		{"unset uses default", 0, contributeCooldownDefaultHours},
		{"negative uses default", -5, contributeCooldownDefaultHours},
		{"in-range passes through", 24, 24},
		{"exactly default", 168, 168},
		{"at min", contributeCooldownMinHours, contributeCooldownMinHours},
		{"above max clamps down", contributeCooldownMaxHours + 1000, contributeCooldownMaxHours},
		{"at max", contributeCooldownMaxHours, contributeCooldownMaxHours},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := HubConfig{ContributeCooldownHours: tc.hours}
			if got := h.ContributeCooldownHoursOrDefault(); got != tc.want {
				t.Errorf("ContributeCooldownHoursOrDefault(hours=%d) = %d, want %d", tc.hours, got, tc.want)
			}
		})
	}
}

// TestContributeCooldownHoursNormalizeClamp verifies applyDefaults clamps a
// persisted out-of-range value into the valid range while leaving 0 ("use
// default") untouched.
func TestContributeCooldownHoursNormalizeClamp(t *testing.T) {
	// 0 is preserved (means "use default"), so the resolver still yields the default.
	c := &Config{}
	c.Hub.ContributeCooldownHours = 0
	c.applyDefaults()
	if c.Hub.ContributeCooldownHours != 0 {
		t.Errorf("zero must be preserved as 'use default', got %d", c.Hub.ContributeCooldownHours)
	}

	// Above max clamps down.
	c = &Config{}
	c.Hub.ContributeCooldownHours = contributeCooldownMaxHours + 500
	c.applyDefaults()
	if c.Hub.ContributeCooldownHours != contributeCooldownMaxHours {
		t.Errorf("over-max should clamp to %d, got %d", contributeCooldownMaxHours, c.Hub.ContributeCooldownHours)
	}

	// Below min (but non-zero) clamps up.
	c = &Config{}
	c.Hub.ContributeCooldownHours = -3
	c.applyDefaults()
	if c.Hub.ContributeCooldownHours != contributeCooldownMinHours {
		t.Errorf("sub-min non-zero should clamp to %d, got %d", contributeCooldownMinHours, c.Hub.ContributeCooldownHours)
	}
}
