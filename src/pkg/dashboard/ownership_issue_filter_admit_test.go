package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
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

// projectbluefin/actions#733 (sec-check) sat in the inbox as ADMIT,
// "add triage/accepted", while sec-check's own fix PR #734 closed it on
// merge. A filter-refused issue an open or held PR of any author closes
// waits on that PR, as an actionable issue with an open linked PR does. A
// soft reference, or a closing reference to another repository's issue,
// leaves the ADMIT ask.
func TestRepoOwnershipFilterRefusedIssueClosedByOpenPRWaitsOnPR(t *testing.T) {
	now := ownershipNow11565
	const repoName = "projectbluefin/actions"
	row := func(n int, labels ...string) github.RepoWorkIssue {
		return github.RepoWorkIssue{Repo: repoName, Number: n, Labels: labels,
			Bucket: repoWorkBucketFiltered, Reason: github.IssueFilterReason,
			CreatedAt: now.Add(-24 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	}
	human := func(r github.RepoWorkIssue) github.RepoWorkIssue { r.AuthorIsHuman = true; return r }
	pr := func(n int, app bool, closes ...github.PRLinkedIssue) github.PullRequest {
		return github.PullRequest{Repo: repoName, Number: n, AppAuthored: app, LinkedIssues: closes,
			CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	}
	cfg := &config.Config{}
	cfg.Project.IssueFilter.RequireLabels = []string{"triage/accepted"}
	env := newOwnershipEnv(cfg, nil, "", now, nil)
	env.selfAuthorizationHold = func(string) bool { return false }
	repos := []FrontendRepo{{Name: "actions", Full: repoName,
		NonActionableIssues: []any{
			row(733, "agent/sec-check", "security"),         // hive PR #734 closes it
			row(740, "agent/quality"),                       // a held outside PR #741 closes it
			human(row(742, "kind/bug")),                     // a person's issue an outside PR #743 closes
			row(744, "agent/quality", "approved-direction"), // decided, hive PR #734 closes it too
			row(750, "agent/reviewer"),                      // PR #751 only refers to it
			row(752, "agent/reviewer"),                      // #753 closes another repo's #752
		},
		OpenPrs: []any{
			pr(734, true, github.PRLinkedIssue{Number: 733}, github.PRLinkedIssue{Number: 744}),
			pr(743, false, github.PRLinkedIssue{Number: 742}),
			pr(751, true, github.PRLinkedIssue{Number: 750, Via: github.PRLinkViaRefs}),
			pr(753, true, github.PRLinkedIssue{Number: 752, Repo: "projectbluefin/other"}),
		},
		HeldPrs: []any{pr(741, false, github.PRLinkedIssue{Number: 740})},
	}}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, env)
	for n, closer := range map[int]string{733: "#734", 740: "#741", 742: "#743", 744: "#734"} {
		d := ownershipFor(repos[0].Ownership, ownershipKindIssue, n)
		if d == nil || d.YourTurn || !d.WaitingElsewhere || !strings.Contains(d.Reason, "Waits on PR "+closer) {
			t.Errorf("#%d = %+v, want waiting on PR %s", n, d, closer)
		}
	}
	for _, n := range []int{750, 752} {
		d := ownershipFor(repos[0].Ownership, ownershipKindIssue, n)
		if d == nil || !d.YourTurn || d.Verb != OwnershipVerbAdmit || d.ReasonCode != OwnershipReasonAgentFindingNotAdmitted {
			t.Errorf("#%d = %+v, want ADMIT %s", n, d, OwnershipReasonAgentFindingNotAdmitted)
		}
	}
}

// An untrusted reporter's issue (the reporter_triage bucket) is Admit until a
// maintainer acts, but a maintainer who already replied and is still the
// newest word from a person is waiting on someone else, and a design
// proposal (stage/018) waits on a maintainer's call. projectbluefin/server#655
// (a MEMBER asked jeefy to pick a fix) and #679 (a MEMBER asked the server
// maintainers for a design decision, thanking the reporter) sat in Admit;
// testsuite#1302 and #1303 carry 018. Display only: the bucket, and so the
// agent queues, are untouched.
func reporterTriageIssue(labels ...string) issueOwnershipInput {
	in := nonActionable11565(repoWorkBucketReporterTriage, "Awaiting maintainer triage label", labels...)
	in.authorIsHuman = true
	in.author = "zacharybutler"
	return in
}

// personComment is a comment two days before ownershipNow11565 (Oct 10).
func personComment(login, association string, mentions ...string) *github.IssuePersonComment {
	return personCommentAt(ownershipNow11565.Add(-2*24*time.Hour), login, association, mentions...)
}

func personCommentAt(at time.Time, login, association string, mentions ...string) *github.IssuePersonComment {
	return &github.IssuePersonComment{Login: login, AuthorAssociation: association, CreatedAt: at, Mentions: mentions}
}

func TestReporterTriageOwnershipReadsTheNewestPersonComment(t *testing.T) {
	env := ownershipEnv11565(6)
	admit := want11565{OwnershipVerbAdmit, OwnershipReasonUntrustedReporter}
	decide := want11565{OwnershipVerbDecide, OwnershipReasonOpenForDiscussion}
	discussion := stagelabels.OpenForDiscussion.Label()
	lastYear := time.Date(2025, 12, 30, 9, 0, 0, 0, time.UTC)
	newYear := ownershipEnv11565(6)
	newYear.now = time.Date(2026, 1, 2, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		env     ownershipEnv
		labels  []string
		comment *github.IssuePersonComment
		want    want11565
		wait    string
	}{
		{"server#655: a maintainer asked someone else", env, nil, personComment("castrojo", "MEMBER", "jeefy"), notYours11565,
			"Waits on jeefy: maintainer castrojo asked on Oct 10"},
		// The pick skips bots, slash commands and hive markers
		// (pkg/github newestPersonComment), so the hive's wait notice after
		// the maintainer's comment leaves this input as is.
		{"server#679: a maintainer thanked the reporter and asked for a decision", env, nil, personComment("castrojo", "MEMBER", "zacharybutler"), notYours11565,
			"Maintainer castrojo replied on Oct 10; waiting on a reply"},
		{"the maintainer's own mention is not who it waits on", env, nil, personComment("castrojo", "OWNER", "castrojo", "jeefy", "zacharybutler"), notYours11565,
			"Waits on jeefy: maintainer castrojo asked on Oct 10"},
		{"two people asked: waiting on a reply", env, nil, personComment("castrojo", "COLLABORATOR", "jeefy", "hanthor"), notYours11565,
			"Maintainer castrojo replied on Oct 10; waiting on a reply"},
		{"a comment from last year names the year", newYear, nil, personCommentAt(lastYear, "castrojo", "MEMBER", "jeefy"), notYours11565,
			"Waits on jeefy: maintainer castrojo asked on Dec 30, 2025"},
		{"the reporter replied after the maintainer", env, nil, personComment("zacharybutler", "NONE", "castrojo"), admit, ""},
		{"someone else replied after the maintainer", env, nil, personComment("passerby", "CONTRIBUTOR", "jeefy"), admit, ""},
		{"the issue's author is a collaborator", env, nil, personComment("ZacharyButler", "COLLABORATOR", "jeefy"), admit, ""},
		{"no person comment", env, nil, nil, admit, ""},
		{"a maintainer's ask older than the stale period goes back to Admit", env, nil,
			personCommentAt(ownershipNow11565.Add(-time.Duration(defaultStaleDays+1)*24*time.Hour), "castrojo", "MEMBER", "jeefy"), admit, ""},
		{"a decided issue is left to admission", env, []string{"approved-direction"}, personComment("castrojo", "MEMBER", "jeefy"), admit, ""},
		{"testsuite#1302: a design proposal", env, []string{discussion, "kind/design"}, nil, decide, ""},
		{"a design proposal a maintainer replied on is still the maintainer's call", env, []string{discussion}, personComment("castrojo", "MEMBER", "jeefy"), decide, ""},
		{"a decided design proposal goes back to admission", env, []string{discussion, "approved-direction"}, nil, admit, ""},
		{"a decided design proposal with a maintainer's ask is admission too", env, []string{discussion, "approved-direction"}, personComment("castrojo", "MEMBER", "jeefy"), admit, ""},
	} {
		in := reporterTriageIssue(tc.labels...)
		in.latestPersonComment = tc.comment
		got := decideIssueOwnershipGated(in, tc.env)
		if tc.wait != "" {
			if got.YourTurn || !got.WaitingElsewhere || got.Reason != tc.wait {
				t.Errorf("%s: got %+v, want waiting elsewhere with %q", tc.name, got, tc.wait)
			}
			if strings.ContainsAny(got.Reason, "\u2014\u2013@") {
				t.Errorf("%s: reason %q has a dash or an at sign", tc.name, got.Reason)
			}
			continue
		}
		if got.YourTurn != (tc.want != notYours11565) || got.Verb != tc.want.verb || got.ReasonCode != tc.want.code || got.WaitingElsewhere {
			t.Errorf("%s: got %+v, want %s/%s", tc.name, got, tc.want.verb, tc.want.code)
		}
	}
}

// The comment is passed only for reporter_triage rows, and the Overview card
// files the wait under Waiting on another item with the decision's reason.
// The card's cause calls the decision's helpers, so a stale ask is Admit on
// both.
func TestReporterTriageMaintainerReplyReachesTheCard(t *testing.T) {
	comment := personComment("castrojo", "MEMBER", "jeefy")
	row := func(n int, bucket, reason string) github.RepoWorkIssue {
		return github.RepoWorkIssue{Repo: "o/r", Number: n, Author: "zacharybutler", AuthorIsHuman: true, Bucket: bucket, Reason: reason,
			CreatedAt: ownershipNow11565.Add(-time.Hour), UpdatedAt: ownershipNow11565.Add(-time.Hour), LatestPersonComment: comment}
	}
	repos := []FrontendRepo{{Name: "r", Full: "o/r", NonActionableIssues: []any{
		row(655, repoWorkBucketReporterTriage, "Awaiting maintainer triage label"),
		row(656, repoWorkBucketFiltered, github.IssueFilterReason),
	}}}
	decideRepoOwnership(repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(6))

	waiting := ownershipFor(repos[0].Ownership, ownershipKindIssue, 655)
	if waiting == nil || waiting.YourTurn || !waiting.WaitingElsewhere || waiting.Reason != "Waits on jeefy: maintainer castrojo asked on Oct 10" {
		t.Fatalf("#655 = %+v, want waiting on jeefy", waiting)
	}
	filtered := ownershipFor(repos[0].Ownership, ownershipKindIssue, 656)
	if filtered == nil || !filtered.YourTurn || filtered.ReasonCode != OwnershipReasonPersonIssueNotAdmitted {
		t.Fatalf("#656 = %+v, want the issue filter's ADMIT: the comment is read for reporter_triage rows only", filtered)
	}
	in := cardTestInputs(nil)
	in.now = ownershipNow11565
	cardRow := cardIssueRow{repoFull: "o/r", work: row(655, repoWorkBucketReporterTriage, "Awaiting maintainer triage label"), nonActionable: true, ownership: waiting}
	band := decideIssueCard(cardRow, in)
	if band.Key != cardBandWaitingOnItem || band.Group != cardGroupWaiting || band.ReasonCode != "wait:"+cardWaitMaintainerReply || band.Reason != waiting.Reason {
		t.Fatalf("card band = %+v, want waiting-on-item wait:%s with the decision's reason", band, cardWaitMaintainerReply)
	}
	for _, tc := range []struct {
		name   string
		labels []string
		now    time.Time
	}{
		{"stale ask", nil, ownershipNow11565.Add(time.Duration(defaultStaleDays+3) * 24 * time.Hour)},
		{"decided", []string{"approved-direction"}, ownershipNow11565},
		{"design proposal", []string{stagelabels.OpenForDiscussion.Label()}, ownershipNow11565},
	} {
		r := cardRow
		r.work.Labels = tc.labels
		at := in
		at.now = tc.now
		if got := cardIssueWaitingCause(r, cardIssueParking(r, nil), at); got != "" {
			t.Errorf("%s: card cause %q, want none, as the decision", tc.name, got)
		}
	}
	part := overviewPartitionOf(&StatusPayload{Repos: repos}, "")
	if part.issues.waitingByCause[waitingCauseMaintainerReply] != 1 {
		t.Fatalf("waitingByCause = %+v, want one maintainer_reply", part.issues.waitingByCause)
	}
}
