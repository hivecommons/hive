package policies

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"testing"
)

// Copilot CLI resolves the bare sonnet/opus aliases one generation back
// (#10461), so every policy that routes sub-agents by complexity tier must
// name the concrete Copilot model ids alongside the Claude Code aliases.
func TestTierPoliciesNameConcreteCopilotModelIDs(t *testing.T) {
	t.Parallel()

	policyNames := []string{
		"scanner.md",
		"scanner-full.md",
		"scanner-automerge.md",
	}
	required := [][]byte{
		[]byte("`claude-haiku-4.5`"),
		[]byte("`claude-sonnet-5.5`"),
		[]byte("`claude-opus-5.5`"),
		[]byte("Copilot CLI"),
	}

	for _, name := range policyNames {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			embedded, err := DefaultPolicies.ReadFile(path.Join("defaults", name))
			if err != nil {
				t.Fatalf("read embedded policy: %v", err)
			}
			source, err := os.ReadFile(filepath.Join("..", "..", "policies", name))
			if err != nil {
				t.Fatalf("read source policy: %v", err)
			}
			for variant, policy := range map[string][]byte{
				"embedded": embedded,
				"source":   source,
			} {
				for _, marker := range required {
					if !bytes.Contains(policy, marker) {
						t.Errorf("%s policy is missing Copilot sub-agent model id %q", variant, marker)
					}
				}
			}
		})
	}
}
