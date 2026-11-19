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
		[]byte("defer to the preparation owner"),
		[]byte("auto_merge.require_current_base"),
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

// Agents must not refresh a PR that is merely behind its base: on
// current-base repositories the merge lane is the one preparation owner, and
// every extra refresh reruns all of CI and cancels the run in flight. The
// old "update immediately" and "simply behind main" instructions must not
// come back in any variant.
func TestPoliciesDoNotRefreshMerelyBehindPRs(t *testing.T) {
	t.Parallel()

	banned := [][]byte{
		[]byte("should be updated immediately"),
		[]byte("conflicts when the PR branch is simply behind main"),
	}
	entries, err := DefaultPolicies.ReadDir("defaults")
	if err != nil {
		t.Fatalf("read embedded policies: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		embedded, err := DefaultPolicies.ReadFile(path.Join("defaults", name))
		if err != nil {
			t.Fatalf("read embedded policy %s: %v", name, err)
		}
		for _, phrase := range banned {
			if bytes.Contains(embedded, phrase) {
				t.Errorf("%s tells agents to refresh a merely-behind PR again (%q)", name, phrase)
			}
		}
	}

	scanner, err := DefaultPolicies.ReadFile(path.Join("defaults", "scanner-full.md"))
	if err != nil {
		t.Fatalf("read scanner-full.md: %v", err)
	}
	if !bytes.Contains(scanner, []byte("Do NOT call MCP `update_pull_request_branch` on it")) {
		t.Error("scanner-full.md no longer tells the scanner to leave merely-behind PRs to the preparation owner")
	}
}

// ci-maintainer-full carries the standing merge-churn goal: four weekly
// measurements, named thresholds, and one issue per repository.
func TestCIMaintainerFullTracksMergeChurn(t *testing.T) {
	t.Parallel()

	policy, err := DefaultPolicies.ReadFile(path.Join("defaults", "ci-maintainer-full.md"))
	if err != nil {
		t.Fatalf("read ci-maintainer-full.md: %v", err)
	}
	for _, marker := range []string{
		"## Merge Churn (standing goal)",
		"`refreshes_per_merged_pr`",
		"`refresh_cancelled_run_share`",
		"`release_merges_per_day`",
		"`approval_to_merge_median`",
		"merge churn: week of",
		"[ci-maintainer] Merge churn over threshold in",
	} {
		if !bytes.Contains(policy, []byte(marker)) {
			t.Errorf("ci-maintainer-full.md is missing merge-churn marker %q", marker)
		}
	}
}
