package dashboard

import (
	"strings"
	"testing"
)

// TestNeedsHumanEscalationBadgePinned pins the dashboard's needs-human
// escalation surfacing in the Repositories section. The escalation ledger
// (pkg/escalation) labels a PR `needs-human` once automated fix attempts are
// exhausted; before this UI existed, nine escalated PRs sat invisible to the
// operator for a week (kubestellar/console, 2026-09-01). The invariants:
//
//  1. A PR row with the needs-human label renders a "needs human" state chip
//     in the action-chip slot, taking precedence over Queue auto-merge (an
//     escalated PR is out of the automated lane by definition).
//  2. The PR pill itself takes the needs-human (alert) tint over mergeable
//     green.
//  3. The section header shows a "N need human review" counter, hidden at
//     zero.
func TestNeedsHumanEscalationBadgePinned(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		// Label check is the single source of truth (same label the
		// escalation ledger writes — pkg/escalation NeedsHumanLabel).
		"labels.includes('needs-human')",
		// State chip in the action-chip slot.
		"pill-needs-human-badge",
		"⚠ needs human",
		// Pill tint: needs-human wins over mergeable.
		".repo-pr-pill.needs-human",
		"needsHuman ? ' needs-human' : mergeClass",
		// Section-header counter element + text.
		"repos-needs-human",
		"' human review'",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("dashboard needs-human escalation UI missing snippet %q", snippet)
		}
	}
}
