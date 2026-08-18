package agent

import "testing"

func TestAgentModeString(t *testing.T) {
	tests := []struct {
		mode AgentMode
		want string
	}{
		{ModeAdvisory, "ADVISORY"},
		{ModeIssuesOnly, "ISSUES_ONLY"},
		{ModeIssuesAndPRs, "ISSUES_AND_PRS"},
		{ModeIssuesPRsMerge, "ISSUES_PRS_MERGE"},
		{AgentMode(99), "UNKNOWN"},
	}
	for _, tt := range tests {
		if got := tt.mode.String(); got != tt.want {
			t.Errorf("AgentMode(%d).String() = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestAgentModeEmoji(t *testing.T) {
	tests := []struct {
		mode AgentMode
		want string
	}{
		{ModeAdvisory, "\U0001F4DD"},
		{ModeIssuesOnly, "\U0001F3AB"},
		{ModeIssuesAndPRs, "\U0001F527"},
		{ModeIssuesPRsMerge, "\U0001F680"},
		{AgentMode(99), ""},
	}
	for _, tt := range tests {
		if got := tt.mode.Emoji(); got != tt.want {
			t.Errorf("AgentMode(%d).Emoji() = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestAgentModeSuffix(t *testing.T) {
	tests := []struct {
		mode AgentMode
		want string
	}{
		{ModeAdvisory, "-advisory"},
		{ModeIssuesOnly, "-issues"},
		{ModeIssuesAndPRs, "-holdgated"},
		{ModeIssuesPRsMerge, "-automerge"},
	}
	for _, tt := range tests {
		if got := tt.mode.Suffix(); got != tt.want {
			t.Errorf("AgentMode(%d).Suffix() = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestSuffixForLevel(t *testing.T) {
	tests := []struct {
		mode  AgentMode
		level int
		want  string
	}{
		{ModeIssuesAndPRs, 3, "-full"},
		{ModeIssuesAndPRs, 4, "-full"},
		{ModeIssuesAndPRs, 5, "-holdgated"},
		{ModeIssuesAndPRs, 6, "-full"},
		{ModeAdvisory, 3, "-advisory"},
		{ModeIssuesOnly, 4, "-issues"},
		{ModeIssuesPRsMerge, 6, "-automerge"},
	}
	for _, tt := range tests {
		if got := tt.mode.SuffixForLevel(tt.level); got != tt.want {
			t.Errorf("AgentMode(%d).SuffixForLevel(%d) = %q, want %q", tt.mode, tt.level, got, tt.want)
		}
	}
}

func TestAgentModeCapabilities(t *testing.T) {
	tests := []struct {
		mode      AgentMode
		canIssues bool
		canPRs    bool
		canMerge  bool
		canPush   bool
		needsMCP  bool
	}{
		{ModeAdvisory, false, false, false, false, false},
		{ModeIssuesOnly, true, false, false, false, true},
		{ModeIssuesAndPRs, true, true, false, true, true},
		{ModeIssuesPRsMerge, true, true, true, true, true},
	}
	for _, tt := range tests {
		if got := tt.mode.CanCreateIssues(); got != tt.canIssues {
			t.Errorf("%s.CanCreateIssues() = %v, want %v", tt.mode, got, tt.canIssues)
		}
		if got := tt.mode.CanCreatePRs(); got != tt.canPRs {
			t.Errorf("%s.CanCreatePRs() = %v, want %v", tt.mode, got, tt.canPRs)
		}
		if got := tt.mode.CanMerge(); got != tt.canMerge {
			t.Errorf("%s.CanMerge() = %v, want %v", tt.mode, got, tt.canMerge)
		}
		if got := tt.mode.CanPush(); got != tt.canPush {
			t.Errorf("%s.CanPush() = %v, want %v", tt.mode, got, tt.canPush)
		}
		if got := tt.mode.NeedsMCPWrite(); got != tt.needsMCP {
			t.Errorf("%s.NeedsMCPWrite() = %v, want %v", tt.mode, got, tt.needsMCP)
		}
	}
}

func TestParseAgentMode(t *testing.T) {
	tests := []struct {
		input string
		want  AgentMode
		ok    bool
	}{
		{"ADVISORY", ModeAdvisory, true},
		{"ISSUES_ONLY", ModeIssuesOnly, true},
		{"ISSUES_AND_PRS", ModeIssuesAndPRs, true},
		{"ISSUES_PRS_MERGE", ModeIssuesPRsMerge, true},
		{"NO_GITHUB", ModeAdvisory, true}, // legacy alias
		{"invalid", ModeAdvisory, false},
		{"", ModeAdvisory, false},
	}
	for _, tt := range tests {
		got, ok := ParseAgentMode(tt.input)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ParseAgentMode(%q) = (%v, %v), want (%v, %v)", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

func TestDefaultAgentMode(t *testing.T) {
	type testCase struct {
		agent string
		level int
		want  AgentMode
	}

	tests := []testCase{
		// L1: guide only, advisory
		{"guide", 1, ModeAdvisory},

		// L2: all advisory
		{"supervisor", 2, ModeAdvisory},
		{"scanner", 2, ModeAdvisory},
		{"quality", 2, ModeAdvisory},
		{"guide", 2, ModeAdvisory},

		// L3: quality ISSUES_AND_PRS, all others (including supervisor) advisory
		{"supervisor", 3, ModeAdvisory},
		{"scanner", 3, ModeAdvisory},
		{"quality", 3, ModeIssuesAndPRs},
		{"guide", 3, ModeAdvisory},
		{"ci-maintainer", 3, ModeAdvisory},

		// L4: quality/sec-check/ci-maintainer ISSUES_AND_PRS, scanner/guide ISSUES_ONLY
		{"supervisor", 4, ModeAdvisory},
		{"scanner", 4, ModeIssuesOnly},
		{"quality", 4, ModeIssuesAndPRs},
		{"guide", 4, ModeIssuesOnly},
		{"ci-maintainer", 4, ModeIssuesAndPRs},
		{"sec-check", 4, ModeIssuesAndPRs},

		// L5: all ISSUES_AND_PRS, supervisor advisory
		{"supervisor", 5, ModeAdvisory},
		{"scanner", 5, ModeIssuesAndPRs},
		{"quality", 5, ModeIssuesAndPRs},
		{"guide", 5, ModeIssuesAndPRs},
		{"ci-maintainer", 5, ModeIssuesAndPRs},
		{"sec-check", 5, ModeIssuesAndPRs},
		{"architect", 5, ModeIssuesAndPRs},
		{"strategist", 5, ModeIssuesAndPRs},

		// L6: scanner ISSUES_PRS_MERGE, others ISSUES_AND_PRS, supervisor advisory
		{"supervisor", 6, ModeAdvisory},
		{"scanner", 6, ModeIssuesPRsMerge},
		{"quality", 6, ModeIssuesAndPRs},
		{"guide", 6, ModeIssuesAndPRs},
		{"ci-maintainer", 6, ModeIssuesAndPRs},
		{"sec-check", 6, ModeIssuesAndPRs},
		{"architect", 6, ModeIssuesAndPRs},
		{"strategist", 6, ModeIssuesAndPRs},
		{"outreach", 6, ModeIssuesAndPRs},

		// Supervisor: ADVISORY at all levels
		{"supervisor", 1, ModeAdvisory},
		{"supervisor", 6, ModeAdvisory},

		// Unknown level defaults to advisory
		{"scanner", 0, ModeAdvisory},
		{"scanner", 99, ModeAdvisory},

		// Unknown agent at L4 defaults to advisory
		{"unknown-agent", 4, ModeAdvisory},
	}

	for _, tt := range tests {
		got := DefaultAgentMode(tt.agent, tt.level)
		if got != tt.want {
			t.Errorf("DefaultAgentMode(%q, %d) = %s, want %s", tt.agent, tt.level, got, tt.want)
		}
	}
}
