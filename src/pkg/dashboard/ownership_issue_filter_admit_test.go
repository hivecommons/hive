package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The issue filter (project.issue_filter.require_labels) keeps an issue off
// every agent queue until a maintainer adds a required label. On
// projectbluefin (L6) the un-park replies of 11-05 20:19-20:37Z said "still
// outside the agent queue … A maintainer adding it admits this one" on
// testsuite#1091, server#622, dakota-iso#294 and others, and the agent
// findings testsuite#1085/#1090/#1094 and documentation#1630 had no TRIAGE
// row at L6: all sat in band "outside" with no verb and no inbox row.
func filterRefused(labels ...string) issueOwnershipInput {
	return nonActionable11565(repoWorkBucketFiltered, github.IssueFilterReason, labels...)
}

func personRefused(labels ...string) issueOwnershipInput {
	in := filterRefused(labels...)
	in.authorIsHuman = true
	return in
}

func TestIssueFilterRefusedDecidedAndAgentFindingsAreAdmit(t *testing.T) {
	l6 := ownershipEnv11565(6)
	l6.selfAuthorizationHold = func(string) bool { return false }
	l6.admitLabels = []string{"triage/accepted"}
	l5 := ownershipEnv11565(5)
	l5.selfAuthorizationHold = func(string) bool { return true }
	l5.admitLabels = []string{"triage/accepted"}

	decided := want11565{OwnershipVerbAdmit, OwnershipReasonDecidedNotAdmitted}
	finding := want11565{OwnershipVerbAdmit, OwnershipReasonAgentFindingNotAdmitted}
	person := want11565{OwnershipVerbAdmit, OwnershipReasonPersonIssueNotAdmitted}
	for _, tc := range []struct {
		name string
		env  ownershipEnv
		in   issueOwnershipInput
		want want11565
	}{
		{"testsuite#1091: agent finding, /hive decision applied approved-direction", l6, filterRefused("security", "agent/sec-check", "approved-direction", "hive/workflows-lane"), decided},
		{"server#622: needs-triage too", l6, filterRefused("agent/ci-maintainer", "needs-kind", "needs-triage", "approved-direction", "ci", "hive/workflows-lane"), decided},
		{"utah-packages#882: a lapsed claim label does not hide it", l6, filterRefused("security", "agent/sec-check", "hive/claimed-by-sec-check", "approved-direction"), decided},
		{"a person's decided issue", l6, filterRefused("kind/feature", "approved-direction"), decided},
		{"decided at L5 too", l5, filterRefused("agent/quality", "approved-direction"), decided},
		{"testsuite#1085: reviewer finding at L6", l6, filterRefused("agent/reviewer"), finding},
		{"testsuite#1100: quality finding at L6", l6, filterRefused("testing", "agent/quality"), finding},
		{"agent finding at L5 stays TRIAGE", l5, filterRefused("agent/reviewer"), want11565{OwnershipVerbTriage, OwnershipReasonAgentFiledUnacknowledged}},
		{"a person's undecided issue", l6, personRefused("kind/bug"), person},
		{"a person's undecided issue at L5", l5, personRefused(), person},
		{"a person's decided issue is decided", l6, personRefused("approved-direction"), decided},
		{"a bot's issue (not a dashboard) stays outside", l6, filterRefused("dependencies"), notYours11565},
		{"a person's issue held by a park keeps the park", l6, func() issueOwnershipInput {
			in := nonActionable11565(repoWorkBucketFiltered, config.DefaultNeedsSpecLabel, "needs-spec")
			in.authorIsHuman = true
			return in
		}(), want11565{OwnershipVerbSpec, OwnershipReasonParkedNeedsSpec}},
		{"a person's exempt issue stays outside", l6, func() issueOwnershipInput {
			in := nonActionable11565(repoWorkBucketFiltered, "Exempt label", "wontfix")
			in.authorIsHuman = true
			return in
		}(), notYours11565},
		{"a person's issue under reporter triage stays untrusted_reporter", l6, func() issueOwnershipInput {
			in := nonActionable11565(repoWorkBucketReporterTriage, "", "kind/bug")
			in.authorIsHuman = true
			return in
		}(), want11565{OwnershipVerbAdmit, OwnershipReasonUntrustedReporter}},
		{"a decision park keeps DECIDE", l6, nonActionable11565(repoWorkBucketFiltered, config.DefaultNeedsDecisionLabel, "approved-direction", "stage/045-needs-decision"), want11565{OwnershipVerbDecide, OwnershipReasonParkedNeedsDecision}},
		{"filtered for another reason is not the filter's", l6, nonActionable11565(repoWorkBucketFiltered, "epic", "approved-direction", "epic"), notYours11565},
		{"an admitted decided issue is not ADMIT", l6, freshIssue11565("approved-direction", "triage/accepted"), notYours11565},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkOwnership11565(t, decideIssueOwnership(tc.in, tc.env), tc.want)
		})
	}

	got := decideIssueOwnership(filterRefused("agent/sec-check", "approved-direction"), l6)
	if !strings.Contains(got.Reason, "`triage/accepted`") {
		t.Fatalf("reason %q must name the label that admits it", got.Reason)
	}
	l6.admitLabels = []string{"triage/accepted", "hive-ok"}
	if got := decideIssueOwnership(filterRefused("agent/reviewer"), l6); !strings.Contains(got.Reason, "add `triage/accepted` or `hive-ok` to admit it, or close it") {
		t.Fatalf("reason %q must name every admitting label and offer closing", got.Reason)
	}
	if got := decideIssueOwnership(personRefused(), l6); !strings.Contains(got.Reason, "A person filed this") || !strings.Contains(got.Reason, "so the hive works it, or close it") {
		t.Fatalf("person reason %q", got.Reason)
	}
	l6.admitLabels = nil
	if got := decideIssueOwnership(filterRefused("approved-direction"), l6); !strings.Contains(got.Reason, "add an admission label") {
		t.Fatalf("reason %q without configured labels", got.Reason)
	}
}

// End to end from the enumeration rows the dashboard stores, with the labels
// read from configuration: the rows are in the inbox as ADMIT.
func TestRepoOwnershipAdmitsFilterRefusedRows(t *testing.T) {
	now := ownershipNow11565
	row := func(n int, labels ...string) github.RepoWorkIssue {
		return github.RepoWorkIssue{Repo: "projectbluefin/testsuite", Number: n, Labels: labels,
			Bucket: repoWorkBucketFiltered, Reason: github.IssueFilterReason,
			CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	}
	human := func(r github.RepoWorkIssue) github.RepoWorkIssue { r.AuthorIsHuman = true; return r }
	cfg := &config.Config{}
	cfg.Project.IssueFilter.RequireLabels = []string{"triage/accepted"}
	env := newOwnershipEnv(cfg, nil, "", now, nil)
	env.selfAuthorizationHold = func(string) bool { return false }
	repos := []FrontendRepo{{Name: "testsuite", Full: "projectbluefin/testsuite", NonActionableIssues: []any{
		row(1091, "security", "agent/sec-check", "approved-direction"),
		row(1085, "agent/reviewer"),
		human(row(1087)),
		row(1200, "dependencies"),
	}}}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env)
	for n, code := range map[int]string{1091: OwnershipReasonDecidedNotAdmitted, 1085: OwnershipReasonAgentFindingNotAdmitted, 1087: OwnershipReasonPersonIssueNotAdmitted} {
		d := ownershipFor(repos[0].Ownership, ownershipKindIssue, n)
		if d == nil || !d.YourTurn || d.Verb != OwnershipVerbAdmit || d.ReasonCode != code || !strings.Contains(d.Reason, "`triage/accepted`") {
			t.Errorf("#%d = %+v, want ADMIT %s naming triage/accepted", n, d, code)
		}
	}
	if d := ownershipFor(repos[0].Ownership, ownershipKindIssue, 1200); d != nil && d.YourTurn {
		t.Errorf("#1200 (a bot's, undecided) = %+v, want not your turn", d)
	}
}
