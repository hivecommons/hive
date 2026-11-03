package policies

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPRCapablePoliciesProtectPublishableContent(t *testing.T) {
	policyNames := []string{
		"architect-full.md",
		"architect-holdgated.md",
		"ci-maintainer-full.md",
		"ci-maintainer-holdgated.md",
		"guide-full.md",
		"guide-holdgated.md",
		"operations-full.md",
		"operations-holdgated.md",
		"outreach-full.md",
		"quality-full.md",
		"quality-holdgated.md",
		"scanner-automerge.md",
		"scanner-full.md",
		"scanner-holdgated.md",
		"sec-check-full.md",
		"sec-check-holdgated.md",
		"strategist-full.md",
		"strategist-holdgated.md",
		"telemetry-full.md",
		"telemetry-holdgated.md",
	}
	markers := []string{
		"Attribution belongs ONLY in the issue or PR body and the DCO commit trailer.",
		"NEVER write `Filed by`, ACMM levels, agent names, or hive run metadata inside any committed file.",
		"When a change mirrors one made in another repository, link that repository's issue or PR only in the issue or PR body. Committed files cite this repository's own issues and PRs, or state the reasoning in place.",
	}

	for _, name := range policyNames {
		t.Run(name, func(t *testing.T) {
			embedded, err := DefaultPolicies.ReadFile("defaults/" + name)
			if err != nil {
				t.Fatalf("read embedded policy: %v", err)
			}
			source, err := os.ReadFile(filepath.Join("..", "..", "policies", name))
			if err != nil {
				t.Fatalf("read source policy: %v", err)
			}
			for _, marker := range markers {
				if !strings.Contains(string(embedded), marker) {
					t.Errorf("embedded policy is missing publishable-content guard %q", marker)
				}
				if !strings.Contains(string(source), marker) {
					t.Errorf("source policy is missing publishable-content guard %q", marker)
				}
			}
		})
	}
}
