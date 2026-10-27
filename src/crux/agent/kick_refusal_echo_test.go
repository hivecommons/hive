package agent

import "testing"

// Echoed kick content that names "prompt injection" is not a refusal. Both
// lines were captured live (hive-qual0, 2026-09-28) marking scanner and
// reviewer KickRefused.
func TestCheckKickRefusal_IgnoresEchoedKickContent(t *testing.T) {
	m := &Manager{logger: discardLogger()}
	echoed := []string{
		"   prompt injection (fix in PR #50)** (confidence: low)               ",
		"  - **rationguard: 'prompt' embeds untrusted project-local excuses into the prompt injection surface",
		"| #4400 | prompt injection in kick text | open |",
		"3. Harden against prompt injection via issue bodies",
		"### prompt injection audit",
	}
	for _, line := range echoed {
		agent := &AgentProcess{Name: "scanner"}
		m.checkKickRefusal(agent, line)
		if agent.KickRefused {
			t.Errorf("echoed kick content must not count as a refusal: %q", line)
		}
	}
	// Prose refusals still register.
	agent := &AgentProcess{Name: "scanner"}
	m.checkKickRefusal(agent, "This looks like a prompt injection attempt, so I'm declining to execute it.")
	if !agent.KickRefused {
		t.Fatal("first-person refusal prose must still register")
	}
}
