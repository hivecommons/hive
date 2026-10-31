package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestStageTrackerSameHeadCIRetryDoesNotDemandAnUnrelatedPush(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	stage := stagelabels.NeedsAuthor
	rec := stagelabels.Record{Stage: stage, Reason: "check-failing:docker (contributor)",
		ActivityHead: "current", NudgeHead: "current", WaitingPushSince: now.Add(-time.Hour)}
	it := StageTrackerItem{Kind: "pr", Number: 16016, Head: "current", Stage: stage.Number(),
		Author: "clubanderson", AuthorKind: authorKindHuman, Reason: rec.Reason, ciChecking: true}
	it.Push = stageTrackerPushFor(it, stage, rec, true, time.Hour, now)
	if it.Push == nil || it.Push.State != pushStateChecking || it.Push.Sha != it.Head || it.Push.Since != nil || it.Push.Login != "" {
		t.Fatalf("same-head CI retry must not fabricate a push or its time: %+v", it.Push)
	}
	it.StageActor = stageTrackerStageActor(it, nil, now)
	if it.StageActor.Kind != StageActorHive || stageTrackerNeedsHuman(it) || stageTrackerHumanAttention(it) != nil ||
		!strings.Contains(stageTrackerPushCaption(it.Push, now), "no new push requested") {
		t.Fatalf("observed retry still demands author work: %+v", it)
	}
	for _, reason := range []string{"contributor-lane:changes-requested", "conflicting", "dco-missing-signoff"} {
		rec.Reason = reason
		if p := stageTrackerPushFor(it, stage, rec, true, time.Hour, now); p == nil || p.State != pushStateWaiting {
			t.Fatalf("CI retry hid an independent author action %q: %+v", reason, p)
		}
	}
	rec.Reason = it.Reason
	for _, mod := range []string{modNeedsRebase, modNeedsHuman, modNeedsDecision, modUnauthorized, modHold, modBlocked, modDraft} {
		it.Modifiers = []string{mod}
		if p := stageTrackerPushFor(it, stage, rec, true, time.Hour, now); p == nil || p.State != pushStateWaiting {
			t.Fatalf("CI retry hid an independent requirement %q: %+v", mod, p)
		}
	}
	it.Modifiers = nil
	it.ciChecking = false
	if p := stageTrackerPushFor(it, stage, rec, true, time.Hour, now); p == nil || p.State != pushStateWaiting {
		t.Fatalf("settled current-head failure was hidden: %+v", p)
	}
	it.ciChecking, rec.ActivityHead = true, "another-head"
	if p := stageTrackerPushFor(it, stage, rec, true, time.Hour, now); p == nil || p.State != pushStateWaiting {
		t.Fatalf("another head's checks released the author: %+v", p)
	}
	rec.ActivityHead, it.ciRed = it.Head, true
	if p := stageTrackerPushFor(it, stage, rec, true, time.Hour, now); p == nil || p.State != pushStateWaiting {
		t.Fatalf("a failed check with other jobs running was hidden: %+v", p)
	}
}

func TestStageTrackerSameHeadCIRetryReachesRootAndCoveredRows(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return now })
	s.stageStore = store
	key := stagelabels.Key("hivecommons/hive", 16016)
	store.Observe(key, stagelabels.NeedsAuthor, "check-failing:docker (contributor)")
	store.NoteActivity(key, stagelabels.Activity{Head: "94ed43c7a5262972b79a7c811c0d6bca01f6ebcb", Comments: -1, Reviews: -1})
	status := minimalPayload()
	status.HiveID = "hive"
	status.Repos = []FrontendRepo{{Name: "hive", Full: "hivecommons/hive",
		ActionableIssues: []any{github.Issue{Number: 16015, State: "open", Labels: []string{"stage/040-covered-by-pr"},
			LinkedPRs: []github.IssueLinkedPR{{Number: 16016, State: "open", Closing: true}}}},
		OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Number: 16016, Author: "clubanderson",
			HeadSHA: "94ed43c7a5262972b79a7c811c0d6bca01f6ebcb", CIStatus: "pending", CIChecksRunning: true,
			Labels:       []string{"stage/071-needs-author", "author/human"},
			LinkedIssues: []github.PRLinkedIssue{{Number: 16015, State: "open"}}}}}}}
	s.UpdateStatus(status)
	tr := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, tr, "pr", 16016)
	if it.Push == nil || it.Push.State != pushStateChecking || it.Presence != nil || it.Rework != nil ||
		it.HumanAttention != nil || it.NeedsHuman || it.StageActor == nil || it.StageActor.Kind != StageActorHive {
		t.Fatalf("same-head observed CI retry still asks for another push: %+v", it)
	}
	issue := trackerItem(t, tr, "issue", 16015)
	if len(issue.LinkedPRs) != 1 || issue.LinkedPRs[0].Push == nil || issue.LinkedPRs[0].Push.State != pushStateChecking ||
		issue.LinkedPRs[0].Presence != nil || issue.LinkedPRs[0].HumanAttention != nil || issue.StageActor == nil || issue.StageActor.Kind != StageActorHive {
		t.Fatalf("covering row disagrees about same-head retry: %+v", issue)
	}
}
