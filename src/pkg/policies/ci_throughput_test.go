package policies

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"testing"
)

// The quality and ci-maintainer lanes are the ones that look at CI health.
// Every variant must carry the throughput/merge-order guidance so a slow or
// fleet-wide red is triaged as capacity or ordering before any PR is touched.
func TestCIPoliciesRequireThroughputAndMergeOrder(t *testing.T) {
	t.Parallel()

	policyNames := []string{
		"quality.md",
		"quality-full.md",
		"quality-holdgated.md",
		"quality-advisory.md",
		"quality-measured.md",
		"ci-maintainer.md",
		"ci-maintainer-full.md",
		"ci-maintainer-holdgated.md",
		"ci-maintainer-advisory.md",
	}
	required := [][]byte{
		[]byte("## CI Throughput and Merge Order"),
		[]byte("Fleet incident, not PR fault."),
		[]byte("runner_name"),
		[]byte("Saturated runners."),
		[]byte("actions/runners"),
		[]byte("git merge-tree --write-tree --name-only"),
		[]byte("<!-- hive-pr-overlap -->"),
		[]byte("update-branch"),
		[]byte("cancel-in-progress"),
		[]byte("cache: false"),
		[]byte("Never add path"),
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
						t.Errorf("%s policy is missing CI throughput guardrail %q", variant, marker)
					}
				}
			}
		})
	}
}
