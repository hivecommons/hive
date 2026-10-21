package hivectl

import (
	"strings"
	"testing"
)

func TestContributionDiffLinesCoverEveryChangeKind(t *testing.T) {
	d := ContributionDiff{
		Selected:              []StageTarget{{Subscription: "alpha", Repository: "github/github.com/org/a"}},
		Deselected:            []StageTarget{{Subscription: "alpha", Repository: "github/github.com/org/b"}},
		SelectedCount:         1,
		DeselectedCount:       1,
		ExcludedCount:         1,
		ClearedExclusionCount: 1,
		Membership: []StageSelectionChange{
			{Subscription: "alpha"},
			{Subscription: "delta"},
		},
		Exclusions: []StageExclusionEffect{
			{StageTarget: StageTarget{Subscription: "alpha", Repository: "github/github.com/org/c"}, Excluded: true, ScopedSubscriptions: []string{"beta"}, Sources: []string{"gamma"}, LegacyUnrestricted: []string{"old"}},
			{StageTarget: StageTarget{Subscription: "beta", Repository: "github/github.com/org/d"}},
		},
		Follow:            []StageBoolChange{{Target: "alpha", After: true}, {Target: "beta", Before: true}},
		Enabled:           []StageBoolChange{{Target: "gamma", After: true}, {Target: "https://two.example", Before: true}},
		HubFollow:         []StageHubFollowChange{{Hub: "https://one.example", Before: "off", After: "current"}},
		LegacyConversions: []string{"old"},
		SharedBranches:    []StageSharedBranch{{Subscription: "alpha", Hubs: []string{"https://one.example", "https://two.example"}}},
	}
	got := strings.Join(d.Lines(), "\n")
	for _, want := range []string{
		"+1 repositories selected",
		"−1 deselected",
		"1 excluded",
		"1 exclusions cleared",
		"follow on for 1 Hive(s)",
		"stop following for 1 Hive(s)",
		"enable gamma",
		"disable https://two.example (credentials and selections retained)",
		"hub https://one.example future Hives off → current",
		"legacy conversion: old",
		"shared branch alpha changes in all of: https://one.example, https://two.example",
		"exclude github/github.com/org/c at alpha; also affects saved membership at beta; exclusion still kept by gamma; legacy unrestricted old can still receive it",
		"clear exclusion of github/github.com/org/d at beta",
		"membership rules changed at delta",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lines lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "membership rules changed at alpha") {
		t.Errorf("membership line repeated a subscription already explained:\n%s", got)
	}
	if lines := (ContributionDiff{}).Lines(); len(lines) != 0 {
		t.Errorf("empty diff produced lines: %v", lines)
	}

	subs, repos := d.Changed()
	for _, s := range []string{"alpha", "beta", "gamma", "delta", "https://two.example"} {
		if !subs[s] {
			t.Errorf("Changed() missed subscription %q: %v", s, subs)
		}
	}
	for _, r := range []string{"alpha|github/github.com/org/a", "alpha|github/github.com/org/b", "alpha|github/github.com/org/c", "beta|github/github.com/org/c", "beta|github/github.com/org/d"} {
		if !repos[r] {
			t.Errorf("Changed() missed repository %q: %v", r, repos)
		}
	}
	if subs["old"] || len(repos) != 5 {
		t.Errorf("Changed() over-marked: subs=%v repos=%v", subs, repos)
	}
}

func TestSelectionLabelNeverClaimsAllVacuously(t *testing.T) {
	cases := []struct {
		name      string
		sel       TreeSelection
		discovery TreeDiscovery
		want      string
	}{
		{"legacy", TreeSelection{Legacy: true, TreeCounts: TreeCounts{Repositories: 3, Selected: 3}}, TreeComplete, "Unrestricted — legacy"},
		{"empty complete", TreeSelection{}, TreeComplete, "No repositories"},
		{"empty unknown", TreeSelection{}, TreeUnknown, "Off — no saved repositories"},
		{"empty unavailable", TreeSelection{}, TreeUnavailable, "Off — no saved repositories"},
		{"all", TreeSelection{State: "all-current", TreeCounts: TreeCounts{Repositories: 2, Selected: 2}}, TreeComplete, "All current (2/2 selected)"},
		{"partial with details", TreeSelection{State: "partial", TreeCounts: TreeCounts{Repositories: 4, Selected: 1, Excluded: 1, Unreviewed: 2}}, TreeComplete, "Partial (1/4 selected, 1 excluded, 2 unreviewed)"},
		{"off", TreeSelection{State: "off", TreeCounts: TreeCounts{Repositories: 2}}, TreeComplete, "Off (0/2 selected)"},
		{"incomplete", TreeSelection{State: "partial", MembershipIncomplete: true, TreeCounts: TreeCounts{Repositories: 2, Selected: 1}}, TreeUnavailable, "Partial (1/2 selected) — Membership incomplete"},
	}
	for _, c := range cases {
		if got := SelectionLabel(c.sel, c.discovery); got != c.want {
			t.Errorf("%s: SelectionLabel = %q, want %q", c.name, got, c.want)
		}
	}
}
