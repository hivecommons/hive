package policies

import (
	"strings"
	"testing"
)

// scannerAutomergePolicy is the embedded default policy that drives the
// merge-sweep-capable scanner. It is the runtime source for hosted hives (read
// via DefaultPolicies at kick time in the scheduler), so the invariants below
// are what actually shape scanner behavior in production.
const scannerAutomergePolicyPath = "defaults/scanner-automerge.md"

func readScannerAutomergePolicy(t *testing.T) string {
	t.Helper()
	data, err := DefaultPolicies.ReadFile(scannerAutomergePolicyPath)
	if err != nil {
		t.Fatalf("read embedded %s: %v", scannerAutomergePolicyPath, err)
	}
	return string(data)
}

// TestScannerAutomergeMergeSweepBeforeCIRepair guards against the starvation
// bug in #2638: a single hard CONFLICTING/DIRTY fix-target monopolized the
// scanner session (CI-repair ran before the merge sweep), so PRs that were
// already mergeable=yes/dco=yes never got swept. The fix reorders the Workflow
// so the merge sweep of already-eligible PRs runs FIRST, before any CI-repair
// or fix-work. This test fails if a future edit reintroduces "CI repair first".
func TestScannerAutomergeMergeSweepBeforeCIRepair(t *testing.T) {
	policy := readScannerAutomergePolicy(t)

	// Anchor on the two Workflow step headers. The merge sweep must appear
	// before the CI-repair step; otherwise a hard fix-target can starve the
	// sweep of ready PRs again.
	mergeStep := "**Merge sweep FIRST**"
	ciRepairStep := "**CI repair (time-boxed, skip hard targets)**"

	mergeIdx := strings.Index(policy, mergeStep)
	if mergeIdx < 0 {
		t.Fatalf("Workflow no longer contains the merge-sweep-first step (%q); "+
			"merge sweep MUST run before CI-repair to avoid #2638 starvation", mergeStep)
	}
	ciIdx := strings.Index(policy, ciRepairStep)
	if ciIdx < 0 {
		t.Fatalf("Workflow no longer contains the time-boxed CI-repair step (%q)", ciRepairStep)
	}
	if mergeIdx >= ciIdx {
		t.Fatalf("Workflow orders CI-repair before the merge sweep (mergeIdx=%d, ciIdx=%d); "+
			"a hard fix-target can then starve the merge of already-eligible PRs (#2638). "+
			"The merge sweep MUST come first.", mergeIdx, ciIdx)
	}
}

// TestScannerAutomergeHardTargetSkip guards the second half of the #2638 fix:
// a CONFLICTING/DIRTY or non-progressing fix-target must be DEFERRED/skipped
// (time-boxed), not retried kick after kick, so it cannot consume the whole
// session. Without this guard, even with merge-sweep-first ordering the fix
// phases could still dead-end on one unfixable PR.
func TestScannerAutomergeHardTargetSkip(t *testing.T) {
	policy := readScannerAutomergePolicy(t)

	requiredMarkers := []string{
		"Hard-target skip",          // the dedicated skip section exists
		"CONFLICTING",               // the classification the guard keys on
		"DIRTY",                     // ditto
		"#2638",                     // the guard is explicitly tied to the bug it fixes
	}
	for _, marker := range requiredMarkers {
		if !strings.Contains(policy, marker) {
			t.Errorf("scanner-automerge policy is missing hard-target starvation guard marker %q; "+
				"a non-progressing DIRTY fix-target could again monopolize the session (#2638)", marker)
		}
	}

	// The guard must instruct DEFER/skip rather than indefinite retry.
	if !strings.Contains(policy, "DEFER") {
		t.Errorf("hard-target guard does not instruct DEFER/skip; a conflicted PR must be " +
			"deferred, not retried indefinitely (#2638)")
	}
}
