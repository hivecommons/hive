package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// hivecommons/hive#13298 follow-up: a triage bot (Prow) puts needs-triage on
// every new issue. On a trusted author's issue (github.Issue.AuthorTrusted:
// one of the hive's own accounts, or an author the reporter-trust predicate
// trusts) the activity monitor must not show the "triage needed" chip or ask
// "Accept triage": the stage engine keeps that issue at 010 and the relay
// offers it. An untrusted author's issue is unchanged: the chip at 010, the
// triage question at 010 and 015. The trust verdict rides on the sidelined
// work-detail rows too (github.RepoWorkIssue.AuthorTrusted).
func TestStageTracker_NeedsTriageByAuthorTrust(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return now })
	s.stageStore = store
	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Repos = []FrontendRepo{{
		Name: "console", Full: "acme/console",
		ActionableIssues: []any{
			github.Issue{Number: 31, Title: "maintainer filed", Author: "maintainer", AuthorIsHuman: true, AuthorTrusted: true, Labels: []string{stagelabels.Actionable.Label(), "needs-triage"}},
			github.Issue{Number: 32, Title: "stranger filed", Author: "stranger", AuthorIsHuman: true, Labels: []string{stagelabels.Actionable.Label(), "needs-triage"}},
			github.Issue{Number: 33, Title: "stranger at 015", Author: "stranger", AuthorIsHuman: true, Labels: []string{stagelabels.TriageNeeded.Label(), "needs-triage"}},
			// A 015 the engine has not re-derived yet on a trusted author's
			// issue: never asked about triage.
			github.Issue{Number: 34, Title: "hive filed at 015", Author: "hive-app[bot]", AuthorTrusted: true, Labels: []string{stagelabels.TriageNeeded.Label(), "needs-triage"}},
		},
		NonActionableIssues: []any{
			github.RepoWorkIssue{Number: 35, Title: "trusted, filtered", Author: "maintainer", AuthorIsHuman: true, AuthorTrusted: true, Labels: []string{"needs-triage"}, Bucket: "filtered", Reason: github.IssueFilterReason},
			github.RepoWorkIssue{Number: 36, Title: "untrusted, filtered", Author: "stranger", AuthorIsHuman: true, Labels: []string{"needs-triage"}, Bucket: "filtered", Reason: github.IssueFilterReason},
		},
	}}
	s.UpdateStatus(status)
	resp := s.buildStageTracker(s.status, store, now)

	asksTriage := func(it StageTrackerItem) bool {
		return it.DecisionNeeded != nil && it.DecisionNeeded.AskedBy == decisionAskedByTriage
	}

	trusted := trackerItem(t, resp, "issue", 31)
	if trusted.Stage != stagelabels.Actionable.Number() || hasMod(trusted, modNeedsTriage) || asksTriage(trusted) {
		t.Fatalf("trusted 010 issue still asks for triage: stage=%d mods=%v decision=%+v", trusted.Stage, trusted.Modifiers, trusted.DecisionNeeded)
	}
	for _, m := range trusted.DeferredModifiers {
		if m == modNeedsTriage {
			t.Fatalf("trusted issue keeps a deferred needs-triage: %v", trusted.DeferredModifiers)
		}
	}

	untrusted := trackerItem(t, resp, "issue", 32)
	if !hasMod(untrusted, modNeedsTriage) || !asksTriage(untrusted) {
		t.Fatalf("untrusted 010 issue with needs-triage must keep the chip and the question: mods=%v decision=%+v", untrusted.Modifiers, untrusted.DecisionNeeded)
	}

	untrusted015 := trackerItem(t, resp, "issue", 33)
	if untrusted015.Stage != trackerNumberTriageNeeded || !asksTriage(untrusted015) {
		t.Fatalf("untrusted 015 issue must keep the triage question: stage=%d decision=%+v", untrusted015.Stage, untrusted015.DecisionNeeded)
	}

	trusted015 := trackerItem(t, resp, "issue", 34)
	if hasMod(trusted015, modNeedsTriage) || asksTriage(trusted015) {
		t.Fatalf("trusted issue on a stale 015 must not ask for triage: mods=%v decision=%+v", trusted015.Modifiers, trusted015.DecisionNeeded)
	}

	sidelinedTrusted := trackerItem(t, resp, "issue", 35)
	if hasMod(sidelinedTrusted, modNeedsTriage) || asksTriage(sidelinedTrusted) {
		t.Fatalf("trusted sidelined row lost AuthorTrusted: mods=%v decision=%+v", sidelinedTrusted.Modifiers, sidelinedTrusted.DecisionNeeded)
	}
	sidelinedUntrusted := trackerItem(t, resp, "issue", 36)
	if !hasMod(sidelinedUntrusted, modNeedsTriage) {
		t.Fatalf("untrusted sidelined row must keep the chip: mods=%v", sidelinedUntrusted.Modifiers)
	}
}
