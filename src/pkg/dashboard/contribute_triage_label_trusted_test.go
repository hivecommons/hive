package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// hivecommons/hive#13298 follow-up: a triage bot (Prow) puts needs-triage on
// every new issue, the hive's and a maintainer's included. The contribute
// skip list withholds an issue for that label only when its author is not
// trusted (github.Issue.AuthorTrusted, carried on the enumerated envelope as
// author_trusted). Every other skip label still withholds a trusted
// author's issue, and an envelope without the field (unknown author) is
// withheld exactly as before. Before this change the skip list was
// author-blind, so the trusted rows below were withheld as label_skipped.
func TestContributeSkipLabelPassesTrustedAuthorsNeedsTriage(t *testing.T) {
	hub, server := covK2Hub(t)
	server.deps.Config.Hub.ContributeLabelsMode = config.FilterModeAllow
	server.deps.Config.Hub.ContributeDenyLabels = []string{"3-clanker-queue"}
	server.deps.Config.Hub.ContributeSkipLabels = config.DefaultContributeSkipLabels()

	trusted := intgIssue(1, "maintainer filed, Prow labelled", "maintainer", nil)
	trusted["labels"] = []any{"3-clanker-queue", "needs-triage"}
	trusted["author_trusted"] = true
	untrusted := intgIssue(2, "stranger filed, Prow labelled", "stranger", nil)
	untrusted["labels"] = []any{"3-clanker-queue", "needs-triage"}
	untrusted["author_trusted"] = false
	unknown := intgIssue(3, "author unknown, Prow labelled", "", nil)
	unknown["labels"] = []any{"3-clanker-queue", "needs-triage"}
	question := intgIssue(4, "maintainer filed, a question", "maintainer", nil)
	question["labels"] = []any{"3-clanker-queue", "needs-triage", "question"}
	question["author_trusted"] = true
	setStatusIssues(server, trusted, untrusted, unknown, question)

	queue := hub.ReadyQueue(readyQueueDefaultLimit)
	if len(queue) != 1 || queue[0].Number != 1 {
		t.Fatalf("ReadyQueue = %+v, want only the trusted author's needs-triage issue #1", queue)
	}

	snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
	for _, want := range []struct {
		key   string
		label string
	}{
		{"myorg/repo1#2", "needs-triage"},
		{"myorg/repo1#3", "needs-triage"},
		{"myorg/repo1#4", "question"},
	} {
		item, ok := findWithheld(snap.withheld, want.key)
		if !ok {
			t.Fatalf("%s missing from withheld: %+v", want.key, snap.withheld)
		}
		if item.Reason != contributorAdmissionReasonLabelSkipped || item.SkippedLabel != want.label {
			t.Fatalf("%s withheld = %+v, want label_skipped %q", want.key, item, want.label)
		}
	}
	if _, ok := findWithheld(snap.withheld, "myorg/repo1#1"); ok {
		t.Fatalf("trusted author's needs-triage issue withheld: %+v", snap.withheld)
	}

	task := hub.selectTask(&ContributorConnection{
		profile: &ContributorProfile{
			GitHubUsername: "alice",
			ContributorID:  "c-alice",
			TrustTier:      "contributor",
		},
		lastPong: time.Now(),
	})
	if task == nil || task.Type != "task_assign" || task.Number != 1 {
		t.Fatalf("selectTask = %+v, want the trusted author's issue #1", task)
	}
}

// The admission gate itself: only the awaiting label is passed over for a
// trusted author; blocked, epic, question and the needs-decision label still
// withhold, and an untrusted or unknown author is withheld for needs-triage.
func TestContributorAdmissionNeedsTriageByAuthorTrust(t *testing.T) {
	hub, server := covK2Hub(t)
	server.deps.Config.Hub.ContributeSkipLabels = config.DefaultContributeSkipLabels()
	cases := []struct {
		name     string
		labels   []string
		trusted  bool
		admitted bool
		reason   string
		label    string
	}{
		{"trusted, needs-triage only", []string{"needs-triage", "kind/bug"}, true, true, "", ""},
		{"untrusted, needs-triage", []string{"needs-triage", "kind/bug"}, false, false, contributorAdmissionReasonLabelSkipped, "needs-triage"},
		{"trusted, blocked", []string{"needs-triage", "blocked"}, true, false, contributorAdmissionReasonWorkflowBlocked, "blocked"},
		{"trusted, epic", []string{"needs-triage", "epic"}, true, false, contributorAdmissionReasonLabelSkipped, "epic"},
		{"trusted, question", []string{"needs-triage", "question"}, true, false, contributorAdmissionReasonLabelSkipped, "question"},
		{"trusted, needs-decision label", []string{"needs-triage", config.DefaultNeedsDecisionLabel}, true, false, contributorAdmissionReasonLabelSkipped, config.DefaultNeedsDecisionLabel},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := hub.evaluateContributorNeutralAdmission(hub.newAdmissionSweep(), contributorAdmissionCandidate{
				repoFull:      "myorg/repo1",
				repoName:      "repo1",
				number:        100 + i,
				labels:        tc.labels,
				authorTrusted: tc.trusted,
			})
			if decision.admitted != tc.admitted {
				t.Fatalf("admitted = %v, want %v: %+v", decision.admitted, tc.admitted, decision)
			}
			if !tc.admitted && (decision.reason != tc.reason || decision.skippedLabel != tc.label) {
				t.Fatalf("decision = %+v, want %s %q", decision, tc.reason, tc.label)
			}
		})
	}
}

// Internal agent kicks read the same skip list through the enumerated
// github.Issue, whose AuthorTrusted the enumeration set.
func TestConvergenceKickProjectionNeedsTriageByAuthorTrust(t *testing.T) {
	_, server := covK2Hub(t)
	server.deps.Config.Hub.ContributeSkipLabels = config.DefaultContributeSkipLabels()

	admitted, withheld := server.ConvergenceKickProjection([]ghpkg.Issue{
		{Repo: "myorg/repo1", Number: 1, Labels: []string{"needs-triage"}, AuthorTrusted: true},
		{Repo: "myorg/repo1", Number: 2, Labels: []string{"needs-triage"}},
		{Repo: "myorg/repo1", Number: 3, Labels: []string{"needs-triage", "question"}, AuthorTrusted: true},
	})
	if len(admitted) != 1 || admitted[0].Number != 1 {
		t.Fatalf("admitted = %+v, want only the trusted author's #1", admitted)
	}
	if len(withheld) != 2 {
		t.Fatalf("withheld = %+v, want #2 and #3", withheld)
	}
	for _, w := range withheld {
		if w.Decision.Reason != contributorAdmissionReasonLabelSkipped {
			t.Fatalf("withheld #%d reason = %q, want label_skipped", w.Issue.Number, w.Decision.Reason)
		}
	}
}
