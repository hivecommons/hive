package dashboard

import (
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// ── #7995: an issue that has already eaten several PRs goes to a human ────────
//
// projectbluefin/documentation#1232 was NEXT UP with three merged and four
// closed pull requests behind it. Every gate on the admission ladder said yes:
// nobody held it, no PR claimed it, no dependency blocked it. The question the
// issue actually posed — "is there anything left here?" — is a maintainer's to
// answer, and nothing routed it to one.

// churnedIssue builds an IssueChurn hook that reports #1232's shape, scaled to
// the thresholds, for one issue number and nothing else.
func churnedIssue(number int) func(string, int) (ghpkg.IssueChurn, bool) {
	return func(repo string, n int) (ghpkg.IssueChurn, bool) {
		if n != number {
			return ghpkg.IssueChurn{}, false
		}
		return ghpkg.IssueChurn{
			Repo:  repo,
			Issue: n,
			Merged: []ghpkg.IssuePRRecord{
				{PRNumber: 1236, State: ghpkg.PRStateMerged},
				{PRNumber: 1295, State: ghpkg.PRStateMerged},
			},
			ClosedUnmerged: []ghpkg.IssuePRRecord{
				{PRNumber: 1286, State: ghpkg.PRStateClosed},
				{PRNumber: 1294, State: ghpkg.PRStateClosed},
			},
		}, true
	}
}

// TestChurnGuardFailsOpen pins every direction in which the guard must NOT
// withhold. It is a stop on re-offering work, so each way of being unsure has
// to mean "offer it": no hook (a hive with no GitHub credentials, or a test),
// no history for the issue, or churn below the thresholds.
func TestChurnGuardFailsOpen(t *testing.T) {
	tests := []struct {
		name  string
		churn func(string, int) (ghpkg.IssueChurn, bool)
	}{
		{name: "no hook wired", churn: nil},
		{
			name: "no history for this issue",
			churn: func(string, int) (ghpkg.IssueChurn, bool) {
				return ghpkg.IssueChurn{}, false
			},
		},
		{
			name: "churn below both thresholds",
			churn: func(repo string, n int) (ghpkg.IssueChurn, bool) {
				return ghpkg.IssueChurn{
					Repo:           repo,
					Issue:          n,
					Merged:         []ghpkg.IssuePRRecord{{PRNumber: 1236}},
					ClosedUnmerged: []ghpkg.IssuePRRecord{{PRNumber: 1286}},
				}, true
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub, s := withheldHub(t, nil)
			s.deps.IssueChurn = tt.churn

			decision := hub.evaluateContributorNeutralAdmission(nil, contributorAdmissionCandidate{
				repoFull: "projectbluefin/dakota",
				repoName: "dakota",
				number:   601,
				ref:      refFromIssueMap("projectbluefin/dakota", map[string]any{"number": float64(601)}),
			})
			if !decision.admitted {
				t.Fatalf("admitted = false (reason %q); an uncertain churn guard must offer the work", decision.reason)
			}
		})
	}
}

// TestChurnGuardClearedByMaintainerLabel pins the escape hatch. The guard
// exists to ask a maintainer a question; the maintainer's answer has to be
// able to end it, or a triaged issue is parked for a fortnight for no reason.
func TestChurnGuardClearedByMaintainerLabel(t *testing.T) {
	hub, s := withheldHub(t, func(issue map[string]any) {
		issue["labels"] = []any{"bug", "Hive: Churn-Triaged"}
	})
	s.deps.IssueChurn = churnedIssue(601)

	// The label is on the issue, so the ladder must offer it — and the
	// assignment path must agree, since the two read the same gate.
	assertQueue(t, hub, 601, 700)
	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
	if item, present := findWithheld(snap.withheld, withheldKey601); present {
		t.Fatalf("a triaged issue is still withheld: %+v", item)
	}
}

// TestChurnGuardIsScopedToTheChurningIssue pins that one withheld candidate is
// never a stalled queue: #700 has no churn and stays offerable throughout.
// (The shared ladder test asserts the same thing for the refusal itself; this
// pins it against the repo-name fallback path specifically, where a sloppy
// hook lookup could match every candidate in the repository.)
func TestChurnGuardIsScopedToTheChurningIssue(t *testing.T) {
	hub, s := withheldHub(t, nil)
	s.deps.IssueChurn = churnedIssue(601)

	assertQueue(t, hub, 700)
	assertAssigns(t, hub, 700)
}

// TestChurnGuardTriesBothRepoSpellings pins the lookup contract it shares with
// the open-PR claim gate: the ledger keys on the config repo spelling, which
// is FrontendRepo.Name, while the candidate leads with the org-qualified form.
func TestChurnGuardTriesBothRepoSpellings(t *testing.T) {
	hub, s := withheldHub(t, nil)
	var asked []string
	s.deps.IssueChurn = func(repo string, n int) (ghpkg.IssueChurn, bool) {
		asked = append(asked, repo)
		if repo != "dakota" {
			return ghpkg.IssueChurn{}, false
		}
		return churnedIssue(601)(repo, n)
	}

	decision := hub.evaluateContributorNeutralAdmission(nil, contributorAdmissionCandidate{
		repoFull: "projectbluefin/dakota",
		repoName: "dakota",
		number:   601,
		ref:      refFromIssueMap("projectbluefin/dakota", map[string]any{"number": float64(601)}),
	})
	if decision.admitted || decision.reason != contributorAdmissionReasonIssueChurn {
		t.Fatalf("decision = %+v, want an issue_churn refusal found under the config repo spelling (asked %v)", decision, asked)
	}
}

// TestHasChurnTriagedLabel pins the matching rule for a label a human types.
func TestHasChurnTriagedLabel(t *testing.T) {
	tests := []struct {
		labels []string
		want   bool
	}{
		{[]string{"hive: churn-triaged"}, true},
		{[]string{"Hive: Churn-Triaged"}, true},
		{[]string{"  hive: churn-triaged  "}, true},
		{[]string{"bug", "hive: churn-triaged"}, true},
		{[]string{"bug"}, false},
		{[]string{"churn-triaged"}, false},
		{nil, false},
	}
	for _, tt := range tests {
		if got := hasChurnTriagedLabel(tt.labels); got != tt.want {
			t.Errorf("hasChurnTriagedLabel(%v) = %v, want %v", tt.labels, got, tt.want)
		}
	}
}
