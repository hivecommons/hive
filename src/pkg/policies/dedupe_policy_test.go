package policies

import (
	"regexp"
	"strings"
	"testing"
)

// TestDedupePolicyShips pins the contract of the scheduled issue+PR
// duplicate-triage lane (hivecommons/hive#11582): it builds a full inventory
// of open issues AND PRs without the listing commands agents are denied,
// verifies candidates by reading both sides, and writes to exactly one place
// per repository — a consolidation checklist issue found by its marker and
// updated in place — while never closing or editing anyone else's items.
func TestDedupePolicyShips(t *testing.T) {
	data, err := DefaultPolicies.ReadFile("defaults/dedupe-issues.md")
	if err != nil {
		t.Fatalf("dedupe-issues.md must ship as an embedded default: %v", err)
	}
	body := string(data)

	for _, want := range []string{"${GH_AUTH}", "${KNOWLEDGE}", "${WRITING_GUIDE}"} {
		if !strings.Contains(body, want) {
			t.Errorf("dedupe-issues.md is missing the %s placeholder", want)
		}
	}

	// Inventory: the kick's ISSUE_LIST/PR_LIST are the lane-filtered
	// actionable subset, so the template must point at the full snapshot and
	// at search, which restrictions.conf permits where `gh issue list` and
	// `gh pr list` are denied.
	for _, want := range []string{"/data/last-actionable.json", "gh search issues", "gh search prs", "gh pr diff"} {
		if !strings.Contains(body, want) {
			t.Errorf("dedupe-issues.md must build its inventory with %q", want)
		}
	}
	if m := regexp.MustCompile("(?m)^\\s*gh (issue|pr) list").FindString(body); m != "" {
		t.Errorf("dedupe-issues.md instructs a blocked listing command: %q", m)
	}

	// One checklist per repository, found by marker, created only with a
	// finding and parked as a maintainer decision, otherwise edited in place.
	for _, want := range []string{
		"hive:dedupe-checklist",
		"--needs-decision",
		"gh issue edit",
		"--body-file",
		"Checked and not duplicates",
		"| Keep | Merge into it | Reason |",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dedupe-issues.md is missing %q", want)
		}
	}

	// Only an action-required checklist is parked as needs-decision; an
	// advisory-only one carries a top-of-body verdict and the triage/advisory
	// label instead (hivecommons/hive#11864).
	for _, want := range []string{
		"Decision required: <yes|no>",
		"--label triage/advisory",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dedupe-issues.md is missing %q", want)
		}
	}
	if !strings.Contains(body, "<!-- hive:dedupe-checklist repo=<owner/repo> -->\nDecision required: <yes|no>\n") {
		t.Error("dedupe-issues.md template must put the Decision required verdict directly under the marker line")
	}
	// An advisory-only pass never opens a new checklist (hivecommons/hive#15381):
	// merge-order reminders and cross-links go to the kick output.
	for _, want := range []string{
		"every row is advisory** → file nothing",
		"never a fresh filing",
		"The relay refuses the create for a",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dedupe-issues.md is missing %q", want)
		}
	}

	// Recommendations only: the lane must forbid itself the actions whose
	// absence makes the two reference checklists (cncf/endusers#1283,
	// projectbluefin/dakota#1764) safe to post unattended.
	for _, want := range []string{"**close**", "**merge, approve, label, assign, retitle, or edit**", "Never edit"} {
		if !strings.Contains(body, want) {
			t.Errorf("dedupe-issues.md must carry the prohibition %q", want)
		}
	}
	if strings.Contains(body, "gh issue close") || strings.Contains(body, "gh pr close") || strings.Contains(body, "gh pr merge") {
		t.Error("dedupe-issues.md must not show a close or merge command")
	}

	// The lane does not compete with the reviewer's per-PR duplicate flag or
	// the mechanical file-set sweep; it names both so the agent cross-refs
	// rather than repeats them.
	if !strings.Contains(body, "reviewer-queue.md") || !strings.Contains(body, "clusters") {
		t.Error("dedupe-issues.md must position itself against the reviewer's PR duplicate flag and the sweep's clusters")
	}
}
