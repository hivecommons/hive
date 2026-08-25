package policies

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"testing"
)

func TestQualityPoliciesRequireCombinedCoverageEvidence(t *testing.T) {
	t.Parallel()

	policyNames := []string{
		"quality-advisory.md",
		"quality-measured.md",
		"quality-holdgated.md",
		"quality-full.md",
	}
	required := [][]byte{
		[]byte("## Coverage Evidence and Priority (MANDATORY)"),
		[]byte("gh run list"),
		[]byte("gh run download"),
		[]byte("Combine unit and end-to-end coverage evidence"),
		[]byte("Do not infer missing end-to-end coverage from a unit `coverprofile`"),
		[]byte("including the workflow run ID and SHA used"),
		[]byte("Never assign priority 0 (critical) to a `coverage-gap`"),
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
			if !bytes.Equal(source, embedded) {
				t.Fatal("source policy and embedded default differ")
			}
			for _, marker := range required {
				if !bytes.Contains(embedded, marker) {
					t.Errorf("policy is missing coverage guardrail %q", marker)
				}
			}
		})
	}
}
