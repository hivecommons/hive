package config

import "testing"

func TestFilterPasses(t *testing.T) {
	list := []string{"epic:*", "*dashboard*"}

	cases := []struct {
		name  string
		value string
		mode  string
		want  bool
	}{
		// deny mode: match -> blocked, non-match -> pass.
		{"deny match", "epic: rollout", FilterModeDeny, false},
		{"deny wildcard match", "renovate dashboard", FilterModeDeny, false},
		{"deny non-match", "fix login bug", FilterModeDeny, true},
		{"empty mode defaults to deny (match)", "epic: x", "", false},
		{"empty mode defaults to deny (non-match)", "hello", "", true},

		// allow mode: only match passes.
		{"allow match", "epic: rollout", FilterModeAllow, true},
		{"allow non-match", "fix login bug", FilterModeAllow, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FilterPasses(tc.value, list, tc.mode); got != tc.want {
				t.Errorf("FilterPasses(%q, list, %q) = %v, want %v", tc.value, tc.mode, got, tc.want)
			}
		})
	}

	// An empty ALLOW list means "filter off" — everything passes (never block all).
	if !FilterPasses("anything", nil, FilterModeAllow) {
		t.Error("empty allow list should pass (filter off), got blocked")
	}
	// An empty DENY list also passes everything.
	if !FilterPasses("anything", nil, FilterModeDeny) {
		t.Error("empty deny list should pass everything")
	}
}

func TestLabelsFilterPasses(t *testing.T) {
	list := []string{"good-first-issue", "help-wanted"}

	cases := []struct {
		name   string
		labels []string
		mode   string
		want   bool
	}{
		// deny: pass unless ANY label matches.
		{"deny no match", []string{"bug", "p1"}, FilterModeDeny, true},
		{"deny one matches", []string{"bug", "help-wanted"}, FilterModeDeny, false},
		{"deny empty labels", nil, FilterModeDeny, true},

		// allow: pass only if AT LEAST ONE label matches.
		{"allow one matches", []string{"bug", "good-first-issue"}, FilterModeAllow, true},
		{"allow none match", []string{"bug", "p1"}, FilterModeAllow, false},
		{"allow empty labels -> no match -> blocked", nil, FilterModeAllow, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LabelsFilterPasses(tc.labels, list, tc.mode); got != tc.want {
				t.Errorf("LabelsFilterPasses(%v, list, %q) = %v, want %v", tc.labels, tc.mode, got, tc.want)
			}
		})
	}

	// Empty allow list -> filter off -> everything passes even with no labels.
	if !LabelsFilterPasses(nil, nil, FilterModeAllow) {
		t.Error("empty allow label list should pass (filter off)")
	}
}

func TestNormalizeFilterMode(t *testing.T) {
	if NormalizeFilterMode("allow") != FilterModeAllow {
		t.Error("allow should stay allow")
	}
	for _, m := range []string{"", "deny", "bogus", "DENY"} {
		if NormalizeFilterMode(m) != FilterModeDeny {
			t.Errorf("%q should normalize to deny", m)
		}
	}
}

// TestLegacyAllowLabelsMigration verifies a hive with the old dual label lists
// (a populated allow list, no deny list, default mode) migrates to a single
// allow-mode label filter on defaults application.
func TestLegacyAllowLabelsMigration(t *testing.T) {
	c := &Config{}
	c.Hub.ContributeAllowLabels = []string{"good-first-issue"}
	c.applyDefaults()

	if c.Hub.ContributeLabelsMode != FilterModeAllow {
		t.Errorf("labels mode = %q, want allow after migration", c.Hub.ContributeLabelsMode)
	}
	if len(c.Hub.ContributeDenyLabels) != 1 || c.Hub.ContributeDenyLabels[0] != "good-first-issue" {
		t.Errorf("migrated list = %v, want [good-first-issue]", c.Hub.ContributeDenyLabels)
	}
	if len(c.Hub.ContributeAllowLabels) != 0 {
		t.Errorf("allow list should be cleared after migration, got %v", c.Hub.ContributeAllowLabels)
	}
}

// TestDefaultFilterModes verifies modes default to deny when unset.
func TestDefaultFilterModes(t *testing.T) {
	c := &Config{}
	c.applyDefaults()
	if c.Hub.ContributeTitlesMode != FilterModeDeny ||
		c.Hub.ContributeAuthorsMode != FilterModeDeny ||
		c.Hub.ContributeLabelsMode != FilterModeDeny {
		t.Errorf("modes should default to deny: titles=%q authors=%q labels=%q",
			c.Hub.ContributeTitlesMode, c.Hub.ContributeAuthorsMode, c.Hub.ContributeLabelsMode)
	}
}
