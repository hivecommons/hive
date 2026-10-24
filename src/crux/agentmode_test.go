package crux

import (
	"testing"

	"github.com/hivecommons/hive/crux/agent"
	"github.com/hivecommons/hive/crux/agentmode"
)

func TestParseAgentMode_MatchesLeaf(t *testing.T) {
	for _, in := range []string{"ADVISORY", "issues-only", "ISSUES_AND_PRS", "issues+prs+merge", "", "bogus"} {
		got, gotOK := ParseAgentMode(in)
		want, wantOK := agentmode.ParseAgentMode(in)
		if got != want || gotOK != wantOK {
			t.Errorf("ParseAgentMode(%q) = (%v, %v), want leaf result (%v, %v)", in, got, gotOK, want, wantOK)
		}
	}
	if m, ok := ParseAgentMode("ADVISORY"); !ok || m != ModeAdvisory {
		t.Fatalf("ParseAgentMode(ADVISORY) = (%v, %v), want (ModeAdvisory, true)", m, ok)
	}
}

func TestModeConstants_AliasLeaf(t *testing.T) {
	cases := map[AgentMode]agentmode.AgentMode{
		ModeAdvisory:       agentmode.ModeAdvisory,
		ModeIssuesOnly:     agentmode.ModeIssuesOnly,
		ModeIssuesAndPRs:   agentmode.ModeIssuesAndPRs,
		ModeIssuesPRsMerge: agentmode.ModeIssuesPRsMerge,
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
			if got, want := DefaultAgentMode(name, level), agent.DefaultAgentMode(name, level); got != want {
				t.Errorf("DefaultAgentMode(%q, %d) = %v, want %v", name, level, got, want)
			}
		}
	}
	if got := DefaultAgentMode("scanner", 6); got != ModeIssuesPRsMerge {
		t.Fatalf("DefaultAgentMode(scanner, 6) = %v, want ModeIssuesPRsMerge", got)
	}
	if got := DefaultAgentMode("supervisor", 6); got != ModeAdvisory {
		t.Fatalf("DefaultAgentMode(supervisor, 6) = %v, want ModeAdvisory", got)
	}
}

func TestAgentAvailableAtACMMLevel_MatchesAgentPackage(t *testing.T) {
	for _, name := range []string{"telemetry", "operations", "scanner", "quality"} {
		for level := 0; level <= 7; level++ {
			if got, want := AgentAvailableAtACMMLevel(name, level), agent.AgentAvailableAtACMMLevel(name, level); got != want {
				t.Errorf("AgentAvailableAtACMMLevel(%q, %d) = %v, want %v", name, level, got, want)
			}
		}
	}
	if AgentAvailableAtACMMLevel("telemetry", 4) {
		t.Fatal("telemetry should be gated below ACMM level 5")
	}
	if !AgentAvailableAtACMMLevel("telemetry", 5) || !AgentAvailableAtACMMLevel("scanner", 1) {
		t.Fatal("telemetry at level 5 and scanner at any level should be available")
	}
}
