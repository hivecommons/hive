package policies

import (
	"strings"
	"testing"
)

// TestDefaultPoliciesNameForgeCloneRelay (#20231): on a Forgejo-only hive the
// kick text (governorWorkSourceRuleForgejo in pkg/scheduler) tells the agent
// to get a checkout with hive-clone and push with hive-push-branch, because
// `gh` and `hive-gh` cannot read a Forgejo repo. The default policies that
// follow that kick told the agent to use `gh` and never named either relay, so
// the policy contradicted the kick and the agent read a `gh` failure as a token
// fault. Each of these templates must carry the forge note, in both the
// embedded copy and the src/policies mirror.
func TestDefaultPoliciesNameForgeCloneRelay(t *testing.T) {
	for _, name := range []string{
		"reviewer-queue.md",
		"quality-advisory.md",
		"scanner-advisory.md",
		"guide-advisory.md",
	} {
		source, embedded, ok := readPolicyPair(t, name)
		if !ok {
			continue
		}
		for variant, text := range map[string]string{"source": source, "embedded": embedded} {
			for _, want := range []string{
				"Forgejo",
				"hive-clone --repo <owner/repo> [--pr <N>]",
				"hive-clone --repo <owner/repo> --fetch",
				"hive-push-branch --repo <owner/repo>",
				"not a token fault",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("%s %s has no non-GitHub forge note naming %q; on a Forgejo-only hive it contradicts the kick text", variant, name, want)
				}
			}
		}
	}
}
