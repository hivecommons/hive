package policies

import (
	"strings"
	"testing"
)

// supervisorPolicyTemplates are the supervisor kick templates. Every built-in
// ACMM pack (level-2 through level-6) uses supervisor-nogithub.md;
// supervisor-advisory.md carries the same rules for operators who select it.
var supervisorPolicyTemplates = []string{
	"supervisor-nogithub.md",
	"supervisor-advisory.md",
}

// TestSupervisorPolicyDoesNotInstructKicks pins hivecommons/hive#19891. Agents
// have no kick route: POST /api/kick/{agent} is owner-only because a kick types
// text into the target agent's CLI (#6557), and the governor already kicks every
// agent on its cadence. A supervisor policy that tells the agent to kick sends it
// hunting for a route on every turn (dashboard API, the hive binary, pluk-send,
// tmux) and reporting "no kick endpoint" all day. The policy must say the
// governor kicks, forbid the search by name, and route stale agents to an
// advisory bead instead.
func TestSupervisorPolicyDoesNotInstructKicks(t *testing.T) {
	forbidden := []string{
		"kick agents, monitor health",
		"Kick agents that need to run",
		"via internal kick mechanism",
	}
	required := []string{
		"Do not kick agents",
		"The governor kicks every agent on its cadence",
		"You have no kick route, and you must not look for one",
		"`/api/kick/*` is owner-only",
		"the `hive` binary",
		"`pluk-send`",
		"tmux",
		"record it in one advisory bead with the agent name and the evidence, and do nothing further for that agent",
	}

	for _, name := range supervisorPolicyTemplates {
		t.Run(name, func(t *testing.T) {
			source, embedded, ok := readPolicyPair(t, name)
			if !ok {
				return
			}
			for tree, body := range map[string]string{"source": source, "embedded": embedded} {
				lower := strings.ToLower(body)
				for _, phrase := range forbidden {
					if strings.Contains(lower, strings.ToLower(phrase)) {
						t.Errorf("%s %s still instructs the supervisor to kick: %q", tree, name, phrase)
					}
				}
				for _, phrase := range required {
					if !strings.Contains(body, phrase) {
						t.Errorf("%s %s is missing no-kick guidance %q", tree, name, phrase)
					}
				}
				// The supervisor stays advisory: no GitHub credentials reach it.
				if strings.Contains(body, "${GH_AUTH}") {
					t.Errorf("%s %s must not carry ${GH_AUTH}", tree, name)
				}
			}
		})
	}
}
