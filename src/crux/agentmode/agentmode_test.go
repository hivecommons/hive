package agentmode

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
		{AgentMode(99), "-advisory"},
	}
	for _, tt := range tests {
		if got := tt.mode.Suffix(); got != tt.want {
			t.Errorf("AgentMode(%d).Suffix() = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestAgentModeTokenTier(t *testing.T) {
	tests := []struct {
		mode AgentMode
		want string
	}{
		{ModeAdvisory, "advisor"},
		{ModeIssuesOnly, "newcomer"},
		{ModeIssuesAndPRs, "contributor"},
		{ModeIssuesPRsMerge, "trusted"},
		{AgentMode(99), "advisor"},
	}
	for _, tt := range tests {
		if got := tt.mode.TokenTier(); got != tt.want {
			t.Errorf("AgentMode(%d).TokenTier() = %q, want %q", tt.mode, got, tt.want)
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
