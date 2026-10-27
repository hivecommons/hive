package main

import "testing"

func TestShouldHoldAgentPR(t *testing.T) {
	// ownsRepo models config.AgentOwnsRepo: the hive manages "hive" and
	// "console"; anything else is another project's repository.
	ownsRepo := func(_ string, repo string) bool {
		return repo == "hive" || repo == "console" || repo == "hivecommons/hive"
	}
	tests := []struct {
		name     string
		agent    string
		repo     string
		level    int
		ownsRepo func(agent, repo string) bool
		want     bool
	}{
		{name: "cross-repo outreach is held at L6", agent: "outreach", repo: "someone-else/awesome-list", level: 6, ownsRepo: ownsRepo, want: true},
		{name: "same-repo outreach is not held at L6", agent: "outreach", repo: "hive", level: 6, ownsRepo: ownsRepo, want: false},
		{name: "same-repo outreach accepts an org-qualified target", agent: "outreach", repo: "hivecommons/hive", level: 6, ownsRepo: ownsRepo, want: false},
		{name: "same-repo outreach still follows the level gate", agent: "outreach", repo: "console", level: 4, ownsRepo: ownsRepo, want: true},
		{name: "outreach identity is normalized", agent: " Outreach ", repo: "other/repo", level: 6, ownsRepo: ownsRepo, want: true},
		{name: "nil ownership predicate fails closed to hold", agent: "outreach", repo: "hive", level: 6, ownsRepo: nil, want: true},
		{name: "other agents remain autonomous at level 6", agent: "scanner", repo: "hive", level: 6, ownsRepo: ownsRepo, want: false},
		{name: "other agents are not held for cross-repo at level 6", agent: "scanner", repo: "other/repo", level: 6, ownsRepo: ownsRepo, want: false},
		{name: "all agents are held at level 3", agent: "scanner", repo: "hive", level: 3, ownsRepo: ownsRepo, want: true},
		{name: "all agents are held at level 5", agent: "quality", repo: "hive", level: 5, ownsRepo: ownsRepo, want: true},
		{name: "manual level does not add a hold", agent: "scanner", repo: "hive", level: 2, ownsRepo: ownsRepo, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldHoldAgentPR(tt.agent, tt.repo, tt.level, tt.ownsRepo); got != tt.want {
				t.Fatalf("shouldHoldAgentPR(%q, %q, %d) = %v, want %v", tt.agent, tt.repo, tt.level, got, tt.want)
			}
		})
	}
}
