package agent

import "testing"

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
