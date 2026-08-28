package config

// Governance tests for the `reviewer` agent role.
//
// The reviewer is unusual in two ways that each need pinning, because both are
// easy to undo with a one-line pack edit that compiles and parses fine:
//
//  1. It is PR-TRIGGERED, not cadence-triggered. Every other agent wakes on a
//     timer and hunts for work; this one is woken per-PR by the review
//     dispatcher and judges the change it is handed. `on_demand: true` is what
//     enforces that — the governor gates cadence kicks, event kicks, and resume
//     kicks on it.
//
//  2. Its authority is capped BELOW the hive's ACMM level. It runs in ADVISORY
//     mode and writes verdicts to the agent-report directory. It has no GitHub
//     write scope, cannot merge, and cannot self-approve.

import "testing"

// reviewerLevels are the packs the role is defined in. L5 and L6 are the levels
// where a PR can reach merge without a mandatory human read — L5 gates on the
// `hold` label and L6 auto-merges on green — so they are where a repo-grounded
// pre-merge judgment has somewhere to land.
var reviewerLevels = []int{5, 6}

// nonReviewerLevels are the packs the role is deliberately absent from. At
// L3/L4 the `hold` label already blocks every agent PR behind a human, so a
// reviewer there is triage rather than a safety gate — and adding it is not
// free, so it is left out rather than shipped unused.
var nonReviewerLevels = []int{1, 2, 3, 4}

func reviewerIn(t *testing.T, level int) (PackAgent, bool) {
	t.Helper()
	p, err := ACMMPackByLevel(level)
	if err != nil {
		t.Fatalf("ACMMPackByLevel(%d): %v", level, err)
	}
	for _, a := range p.Agents {
		if a.Name == "reviewer" {
			return a, true
		}
	}
	return PackAgent{}, false
}

// TestReviewerRoleDefinedAtHighTrustLevels pins where the role ships.
func TestReviewerRoleDefinedAtHighTrustLevels(t *testing.T) {
	for _, lvl := range reviewerLevels {
		a, ok := reviewerIn(t, lvl)
		if !ok {
			t.Fatalf("L%d pack is missing the reviewer agent", lvl)
		}
		if a.Role != "reviewer" {
			t.Errorf("L%d: role = %q, want \"reviewer\"", lvl, a.Role)
		}
		if a.KickTemplate != "reviewer-advisory.md" {
			t.Errorf("L%d: kick_template = %q, want reviewer-advisory.md", lvl, a.KickTemplate)
		}
	}
}

// TestReviewerAbsentBelowL5 pins that the role does NOT silently appear at
// levels whose merge policy already requires a human. This is the positive
// control for the test above: it proves the lookup can return false, so
// "present at L5/L6" is a real assertion rather than a helper that always
// finds something.
func TestReviewerAbsentBelowL5(t *testing.T) {
	for _, lvl := range nonReviewerLevels {
		if _, ok := reviewerIn(t, lvl); ok {
			t.Errorf("L%d must not define the reviewer agent: the hold label already gates every agent PR behind a human there", lvl)
		}
	}
}

// TestReviewerIsPRTriggeredNotCadenceTriggered pins the lifecycle property.
//
// on_demand is the existing mechanism that excludes an agent from every
// governor kick path (agentsDueForKick, the event-kick gate, and
// AllowResumeKick all consult AgentConfig.OnDemand). Dropping it would convert
// the reviewer into yet another agent that wakes on a timer and hunts, which is
// precisely the design this role rejects.
func TestReviewerIsPRTriggeredNotCadenceTriggered(t *testing.T) {
	for _, lvl := range reviewerLevels {
		a, ok := reviewerIn(t, lvl)
		if !ok {
			t.Fatalf("L%d pack is missing the reviewer agent", lvl)
		}
		if !a.OnDemand {
			t.Errorf("L%d: reviewer must be on_demand so the governor never cadence-kicks it; "+
				"it is woken per-PR by the review dispatcher", lvl)
		}
	}
}

// TestReviewerHasNoCadenceEntry pins the same property from the other side: a
// cadence entry for an on-demand agent is dead configuration that misleads an
// operator reading the pack into thinking the reviewer runs on a timer.
func TestReviewerHasNoCadenceEntry(t *testing.T) {
	for _, lvl := range reviewerLevels {
		p, err := ACMMPackByLevel(lvl)
		if err != nil {
			t.Fatalf("ACMMPackByLevel(%d): %v", lvl, err)
		}
		for mode, cadences := range p.Governor.Cadences {
			if v, ok := cadences["reviewer"]; ok {
				t.Errorf("L%d governor cadence %q declares reviewer=%q, but the reviewer is PR-triggered; "+
					"a cadence entry here is dead config", lvl, mode, v)
			}
		}
	}
}

// TestReviewerAuthorityIsCappedBelowHiveLevel pins the governance invariant:
// the reviewer cannot merge, cannot self-approve, and holds no GitHub write
// scope. ADVISORY is the mode that guarantees it — PR reviews, PR edits, and
// pushes all require ModeIssuesAndPRs (see pkg/proxy/rules.go), and issue
// creation requires ModeIssuesOnly, so ADVISORY leaves read-only access.
//
// This is what makes the role safe to enable on an evidence base of six PRs:
// the worst a bad verdict can do is withhold a merge for a human to clear.
func TestReviewerAuthorityIsCappedBelowHiveLevel(t *testing.T) {
	for _, lvl := range reviewerLevels {
		a, ok := reviewerIn(t, lvl)
		if !ok {
			t.Fatalf("L%d pack is missing the reviewer agent", lvl)
		}
		if a.Mode != "ADVISORY" {
			t.Errorf("L%d: reviewer mode = %q, want ADVISORY. The reviewer's product is a structured "+
				"verdict written to the agent-report dir, not a GitHub write. Granting it "+
				"ISSUES_AND_PRS would add a second write-capable agent and let it post PR reviews.", lvl, a.Mode)
		}
	}
}

// TestReviewerCanReadTheRepo pins the measured active ingredient.
//
// An experiment scored a diff-only reviewer at 17% hit rate with 3.6 false
// positives per PR, and the same reviewer with repository access at the PR's
// merge-base at 67% with 1.4 FPs. Repo access is the defining feature of this
// role, not an enhancement — a reviewer without it is not worth running, so
// include_repos is asserted rather than left to a pack author's judgment.
func TestReviewerCanReadTheRepo(t *testing.T) {
	for _, lvl := range reviewerLevels {
		a, ok := reviewerIn(t, lvl)
		if !ok {
			t.Fatalf("L%d pack is missing the reviewer agent", lvl)
		}
		if !a.IncludeRepos {
			t.Errorf("L%d: reviewer must have include_repos: true. Repo access is the measured "+
				"active ingredient (17%%->67%% hit rate, 61%% fewer false positives); "+
				"a diff-only reviewer produces ~9 non-issues per real finding.", lvl)
		}
	}
}

// TestReviewerIsOnDemandFleetWide pins that the shared on-demand registry sees
// the role, so nothing auto-starts it regardless of which level is active.
func TestReviewerIsOnDemandFleetWide(t *testing.T) {
	if !OnDemandAgentsFromPacks()["reviewer"] {
		t.Fatal("reviewer must appear in OnDemandAgentsFromPacks so it is never auto-started on a timer")
	}
}
