package crux_test

import (
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/agentmode"
)

func TestParseAgentMode_MatchesLeaf(t *testing.T) {
	for _, in := range []string{"ADVISORY", "issues-only", "ISSUES_AND_PRS", "issues+prs+merge", "", "bogus"} {
		got, gotOK := crux.ParseAgentMode(in)
		want, wantOK := agentmode.ParseAgentMode(in)
		if got != want || gotOK != wantOK {
			t.Errorf("crux.ParseAgentMode(%q) = (%v, %v), want leaf result (%v, %v)", in, got, gotOK, want, wantOK)
		}
	}
	if m, ok := crux.ParseAgentMode("ADVISORY"); !ok || m != crux.ModeAdvisory {
		t.Fatalf("crux.ParseAgentMode(ADVISORY) = (%v, %v), want (crux.ModeAdvisory, true)", m, ok)
	}
}

func TestModeConstants_AliasLeaf(t *testing.T) {
	cases := map[crux.AgentMode]agentmode.AgentMode{
		crux.ModeAdvisory:       agentmode.ModeAdvisory,
		crux.ModeIssuesOnly:     agentmode.ModeIssuesOnly,
		crux.ModeIssuesAndPRs:   agentmode.ModeIssuesAndPRs,
		crux.ModeIssuesPRsMerge: agentmode.ModeIssuesPRsMerge,
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("crux mode %v != leaf mode %v", got, want)
		}
	}
}

func TestDefaultAgentMode_MatchesAgentPackage(t *testing.T) {
	for _, name := range []string{"scanner", "quality", "supervisor", "guide", "governor"} {
		for level := 0; level <= 7; level++ {
			if got, want := crux.DefaultAgentMode(name, level), agent.DefaultAgentMode(name, level); got != want {
				t.Errorf("crux.DefaultAgentMode(%q, %d) = %v, want %v", name, level, got, want)
			}
		}
	}
	if got := crux.DefaultAgentMode("scanner", 6); got != crux.ModeIssuesPRsMerge {
		t.Fatalf("crux.DefaultAgentMode(scanner, 6) = %v, want crux.ModeIssuesPRsMerge", got)
	}
	if got := crux.DefaultAgentMode("supervisor", 6); got != crux.ModeAdvisory {
		t.Fatalf("crux.DefaultAgentMode(supervisor, 6) = %v, want crux.ModeAdvisory", got)
	}
}

func TestAgentAvailableAtACMMLevel_MatchesAgentPackage(t *testing.T) {
	for _, name := range []string{"telemetry", "operations", "scanner", "quality"} {
		for level := 0; level <= 7; level++ {
			if got, want := crux.AgentAvailableAtACMMLevel(name, level), agent.AgentAvailableAtACMMLevel(name, level); got != want {
				t.Errorf("crux.AgentAvailableAtACMMLevel(%q, %d) = %v, want %v", name, level, got, want)
			}
		}
	}
	if crux.AgentAvailableAtACMMLevel("telemetry", 4) {
		t.Fatal("telemetry should be gated below ACMM level 5")
	}
	if !crux.AgentAvailableAtACMMLevel("telemetry", 5) || !crux.AgentAvailableAtACMMLevel("scanner", 1) {
		t.Fatal("telemetry at level 5 and scanner at any level should be available")
	}
}
