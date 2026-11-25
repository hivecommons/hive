package agent

import (
	"strings"
	"testing"
)

// A repo whose open-PRs mode refuses a new PR gets a sandboxed kick without
// push or PR client: no branch is pushed and no PR is opened, and the refusal
// is audited.
func TestManagerSandboxRepoPRRefusalSkipsPushAndPR(t *testing.T) {
	pr := &countingPR{}
	m, lines, mu := newSandboxTargetManager(t, pr, &sandboxFakeRunner{commits: 1})
	var asked []string
	m.mu.Lock()
	m.project.RepoPRRefusal = func(agent, repo string) string {
		asked = append(asked, agent+"@"+repo)
		return "pull requests are off for o/r"
	}
	m.mu.Unlock()
	if err := m.SendKick(targetAgent, "harden the tests"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	refused := waitSandboxAudit(t, lines, mu, "sandbox_pr_refused")
	if !strings.Contains(refused.detail, "repo=o/r") || !strings.Contains(refused.detail, "gate=open-prs") {
		t.Fatalf("refusal audit detail = %q", refused.detail)
	}
	waitSandboxAudit(t, lines, mu, "sandbox_complete")
	if n := pr.count(); n != 0 {
		t.Fatalf("CreatePR ran %d times in a repo whose mode refuses PRs", n)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(asked) != 1 || asked[0] != targetAgent+"@o/r" {
		t.Fatalf("gate asked %v, want one call for %s@o/r", asked, targetAgent)
	}
}

// A repo the gate allows still opens its PR.
func TestManagerSandboxRepoPRRefusalAllows(t *testing.T) {
	pr := &countingPR{}
	m, lines, mu := newSandboxTargetManager(t, pr, &sandboxFakeRunner{commits: 1})
	m.mu.Lock()
	m.project.RepoPRRefusal = func(agent, repo string) string { return "" }
	m.mu.Unlock()
	if err := m.SendKick(targetAgent, "fix"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	waitSandboxAudit(t, lines, mu, "sandbox_complete")
	if n := pr.count(); n != 1 {
		t.Fatalf("CreatePR calls = %d, want 1", n)
	}
}
