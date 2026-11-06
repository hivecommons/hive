package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// An agent finding the issue filter refuses whose cited lines are gone from
// the default branch (RepoWorkIssue.CitedLinesGone, read by the enumeration)
// is CLOSE, not ADMIT or TRIAGE: projectbluefin/testsuite#1085 cited
// SKILL.md:163 for a fallback main no longer documented.
func TestCitedLinesGoneOffersClose(t *testing.T) {
	l6 := ownershipEnv11565(6)
	l6.selfAuthorizationHold = func(string) bool { return false }
	l5 := ownershipEnv11565(5)
	l5.selfAuthorizationHold = func(string) bool { return true }
	gone := func(cite string, bucket, reason string, labels ...string) issueOwnershipInput {
		in := nonActionable11565(bucket, reason, labels...)
		in.citedLinesGone = cite
		return in
	}
	closeIt := want11565{OwnershipVerbClose, OwnershipReasonCitedLinesGone}
	for _, tc := range []struct {
		name string
		env  ownershipEnv
		in   issueOwnershipInput
		want want11565
	}{
		{"testsuite#1085 at L6", l6, gone("docs/skills/test-authoring/gnome/SKILL.md:163", repoWorkBucketFiltered, github.IssueFilterReason, "agent/reviewer"), closeIt},
		{"at L5 it is CLOSE rather than TRIAGE", l5, gone("scripts/vm_validation_plan.py:108", repoWorkBucketFiltered, github.IssueFilterReason, "agent/reviewer"), closeIt},
		{"a park keeps its ask", l6, gone("a.md:5", repoWorkBucketFiltered, config.DefaultNeedsDecisionLabel, "agent/reviewer", "stage/045-needs-decision"), want11565{OwnershipVerbDecide, OwnershipReasonParkedNeedsDecision}},
		{"verified done stays verified done", l6, gone("a.md:5", repoWorkBucketFiltered, github.IssueFilterReason, "agent/reviewer", "hive/already-done"), want11565{OwnershipVerbClose, OwnershipReasonVerifiedDone}},
		{"lines still on main: unchanged", l5, gone("", repoWorkBucketFiltered, github.IssueFilterReason, "agent/reviewer"), want11565{OwnershipVerbTriage, OwnershipReasonAgentFiledUnacknowledged}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkOwnership11565(t, decideIssueOwnership(tc.in, tc.env), tc.want)
		})
	}
	got := decideIssueOwnership(gone("docs/x.md:163", repoWorkBucketFiltered, github.IssueFilterReason, "agent/reviewer"), l6)
	if !strings.Contains(got.Reason, "no longer match main (docs/x.md:163)") {
		t.Fatalf("reason %q must name the citation", got.Reason)
	}
}

// End to end from the enumeration row the dashboard stores.
func TestRepoOwnershipClosesStaleFindingRow(t *testing.T) {
	now := ownershipNow11565
	env := ownershipEnv11565(6)
	env.selfAuthorizationHold = func(string) bool { return false }
	repos := []FrontendRepo{{Name: "testsuite", Full: "projectbluefin/testsuite", NonActionableIssues: []any{
		github.RepoWorkIssue{Repo: "projectbluefin/testsuite", Number: 1085, Labels: []string{"agent/reviewer"},
			Bucket: repoWorkBucketFiltered, Reason: github.IssueFilterReason, CitedLinesGone: "docs/skills/test-authoring/gnome/SKILL.md:163",
			CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now.Add(-time.Hour)},
	}}}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env)
	d := ownershipFor(repos[0].Ownership, ownershipKindIssue, 1085)
	if d == nil || !d.YourTurn || d.Verb != OwnershipVerbClose || d.ReasonCode != OwnershipReasonCitedLinesGone {
		t.Fatalf("#1085 = %+v, want CLOSE cited_lines_gone", d)
	}
}
