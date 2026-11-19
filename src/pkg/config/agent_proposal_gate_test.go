package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAgentProposalDefaults(t *testing.T) {
	var f IssueFilterConfig
	for _, tc := range []struct {
		labels []string
		title  string
		want   bool
	}{
		{[]string{"AGENT/STRATEGIST"}, "idea", true},
		{[]string{"agent/outreach"}, "idea", true},
		{nil, " [STRATEGIST] idea", true},
		{nil, "[outreach] idea", true},
		{[]string{"agent/architect"}, "[architect] idea", false},
		{[]string{"agent/strategist-extra"}, "ordinary bug", false},
		{nil, "a [strategist] idea", false},
	} {
		if got := f.IsAgentProposal(tc.labels, tc.title); got != tc.want {
			t.Errorf("IsAgentProposal(%v, %q) = %v, want %v", tc.labels, tc.title, got, tc.want)
		}
	}
}

func TestAgentProposalConfigRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, input                string
		wantDefault, wantArchitect bool
	}{
		{"omitted", "{}", true, false},
		{"disabled", "agent_proposal: {}", false, false},
		{"custom", "agent_proposal:\n  labels: [' agent/architect ', ' ']\n  title_prefixes: [' [architect] ', ' ']", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f IssueFilterConfig
			if err := yaml.Unmarshal([]byte(tc.input), &f); err != nil {
				t.Fatal(err)
			}
			check := func(f IssueFilterConfig) {
				t.Helper()
				if got := f.IsAgentProposal([]string{"agent/strategist"}, "ordinary"); got != tc.wantDefault {
					t.Errorf("default marker = %v, want %v", got, tc.wantDefault)
				}
				if got := f.IsAgentProposal([]string{"agent/architect"}, "ordinary"); got != tc.wantArchitect {
					t.Errorf("architect label = %v, want %v", got, tc.wantArchitect)
				}
				if got := f.IsAgentProposal(nil, "[ARCHITECT] idea"); got != tc.wantArchitect {
					t.Errorf("architect title = %v, want %v", got, tc.wantArchitect)
				}
				if f.IsAgentProposal(nil, "ordinary") {
					t.Error("blank markers matched")
				}
			}
			check(f)
			for _, codec := range []struct {
				marshal   func(any) ([]byte, error)
				unmarshal func([]byte, any) error
			}{{yaml.Marshal, yaml.Unmarshal}, {json.Marshal, json.Unmarshal}} {
				data, err := codec.marshal(f)
				if err != nil {
					t.Fatal(err)
				}
				var roundTrip IssueFilterConfig
				if err := codec.unmarshal(data, &roundTrip); err != nil {
					t.Fatal(err)
				}
				if !f.Equal(roundTrip) {
					t.Error("round trip changed configuration")
				}
				check(roundTrip)
			}
		})
	}
	if (IssueFilterConfig{}).Equal(IssueFilterConfig{AgentProposal: &AgentProposalGateConfig{}}) {
		t.Error("omitted and disabled configurations must differ")
	}
	if (IssueFilterConfig{AgentProposal: &AgentProposalGateConfig{}}).IsZero() {
		t.Error("explicit disable must survive config reconciliation")
	}
}
