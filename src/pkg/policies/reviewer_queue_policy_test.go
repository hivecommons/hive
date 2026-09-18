package policies

import (
	"strings"
	"testing"
)

// TestReviewerQueuePolicyShips guards the failure that motivated shipping this
// template: the reviewer's only policy files lived on a single spoke's PVC
// (/data/policies), nothing in the repo referenced them, and the agent was
// configured to the stale "Advisory Mode" copy which told it that it had no
// GitHub write access and must not comment — while the kick built by
// pkg/review/prompts.go simultaneously ordered it to post a comment. The agent
// received both instructions in one context for hours.
func TestReviewerQueuePolicyShips(t *testing.T) {
	data, err := DefaultPolicies.ReadFile("defaults/reviewer-queue.md")
	if err != nil {
		t.Fatalf("reviewer-queue.md must ship as an embedded default: %v", err)
	}
	body := string(data)

	for _, want := range []string{"${GH_AUTH}", "${KNOWLEDGE}"} {
		if !strings.Contains(body, want) {
			t.Errorf("reviewer-queue.md is missing the %s placeholder", want)
		}
	}

	// The template must agree with the relay it is told to use: pkg/review
	// builds a kick that permits only --comment, and the review request
	// watcher submits it under the App installation.
	if !strings.Contains(body, "hive-review") {
		t.Error("reviewer-queue.md must direct publishing through the hive-review relay")
	}
	if !strings.Contains(body, "COMMENT") {
		t.Error("reviewer-queue.md must name the COMMENT event as its product")
	}

	// It must not re-acquire the contradictions of the advisory copy.
	for _, banned := range []string{
		"no GitHub write access",
		"not get the PR body",
		"do not wake on a schedule",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("reviewer-queue.md contains stale advisory-mode claim %q", banned)
		}
	}
}
