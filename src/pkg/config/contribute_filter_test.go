package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

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

func TestContributeRepoFiltersLayerOnHiveFilters(t *testing.T) {
	hub := HubConfig{
		ContributeLabelsMode: FilterModeDeny,
		ContributeDenyLabels: []string{"hold"},
		ContributeRepoFilters: map[string]ContributeRepoFilter{
			"org/a": {LabelsMode: FilterModeDeny, DenyLabels: []string{"2-discussing"}},
			"org/c": {LabelsMode: FilterModeAllow, DenyLabels: []string{"3-clanker-queue"}},
		},
	}
	cases := []struct {
		name       string
		repo       string
		labels     []string
		wantPass   bool
		wantScope  string
		wantFilter string
	}{
		{"hive deny blocks every repo", "org/a", []string{"hold", "3-clanker-queue"}, false, "hive", "label"},
		{"repo deny blocks matching repo", "org/a", []string{"2-discussing"}, false, "repo", "label"},
		{"repo deny does not affect siblings", "org/b", []string{"2-discussing"}, true, "", ""},
		{"repo allow admits matching label", "org/c", []string{"3-clanker-queue"}, true, "", ""},
		{"repo allow narrows matching repo", "org/c", []string{"bug"}, false, "repo", "label"},
		{"repo allow does not affect siblings", "org/b", []string{"bug"}, true, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hub.EvaluateContributeFilters(tc.repo, "fix", "human", tc.labels)
			if got.Admitted() != tc.wantPass {
				t.Fatalf("admitted = %v, want %v (decision %+v)", got.Admitted(), tc.wantPass, got)
			}
			if got.Scope != tc.wantScope || got.Filter != tc.wantFilter {
				t.Fatalf("scope/filter = %q/%q, want %q/%q", got.Scope, got.Filter, tc.wantScope, tc.wantFilter)
			}
		})
	}
}

func TestContributeRepoFiltersYAMLRoundTripAndNormalize(t *testing.T) {
	var cfg Config
	raw := []byte(`
hub:
  contribute_repo_filters:
    org/docs:
      labels_mode: allow
      deny_labels: [good-first-issue]
      titles_mode: bogus
      deny_titles: [WIP]
`)
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	cfg.applyDefaults()
	filter, ok := cfg.Hub.ContributeRepoFilterFor("ORG/DOCS")
	if !ok {
		t.Fatal("repo filter not found after round trip")
	}
	if filter.LabelsMode != FilterModeAllow || filter.TitlesMode != FilterModeDeny {
		t.Fatalf("normalized modes = labels %q titles %q", filter.LabelsMode, filter.TitlesMode)
	}
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var reloaded Config
	if err := yaml.Unmarshal(out, &reloaded); err != nil {
		t.Fatalf("yaml.Unmarshal(reloaded): %v", err)
	}
	if _, ok := reloaded.Hub.ContributeRepoFilterFor("org/docs"); !ok {
		t.Fatalf("reloaded config missing contribute_repo_filters: %s", string(out))
	}
}
