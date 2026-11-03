package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestStageTrackerHeadReconciliationReachesCoveredRows(t *testing.T) {
	const oldHead = "dfaadfae26fd0fabef7cb704d31feb6975bcfcc5"
	const newHead = "78c30d99fc5567ba809871db8c77dac5cb6dd052"
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	clock := now.Add(-time.Hour)
	s := newTestServer()
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	key := stagelabels.Key("hivecommons/hive", 16012)
	store.Observe(key, stagelabels.NeedsAuthor, "contributor-lane:changes-requested")
	store.NoteActivity(key, stagelabels.Activity{Head: oldHead, Comments: -1, Reviews: -1})
	store.MarkNudged(key, oldHead, 1, false, clock)
	status := minimalPayload()
	status.HiveID = "hive"
	status.Repos = []FrontendRepo{{Name: "hive", Full: "hivecommons/hive",
		ActionableIssues: []any{github.Issue{Number: 16010, Title: "Covered work", State: "open", Labels: []string{"stage/040-covered-by-pr"},
			LinkedPRs: []github.IssueLinkedPR{{Number: 16012, State: "open", Closing: true}}}},
		OpenPrs: []any{FrontendPR{PullRequest: github.PullRequest{Number: 16012, Title: "Fix", Author: "clubanderson", HeadSHA: newHead,
			URL: "https://github.com/hivecommons/hive/pull/16012", Labels: []string{"stage/071-needs-author", "author/human"},
			LinkedIssues: []github.PRLinkedIssue{{Number: 16010, State: "open"}}}}}}}
	s.UpdateStatus(status)
	clock = now
	tr := s.buildStageTracker(s.status, store, now)
	it := trackerItem(t, tr, "pr", 16012)
	if it.Stage != stagelabels.NeedsAuthor.Number() || it.Push == nil || it.Push.State != pushStatePushed ||
		it.Presence != nil || it.Rework != nil || it.Adoption != nil || it.HumanAttention != nil || it.NeedsHuman {
		t.Fatalf("new-head payload mixes satisfied author wait with pending review: %+v", it)
	}

	if it.Caption != "new head observed → re-review pending" || it.NoteChangedAt != nil || it.StageActor == nil || it.StageActor.Kind != StageActorHive {
		t.Fatalf("caption/actor disagree with push: %+v", it)
	}
	issue := trackerItem(t, tr, "issue", 16010)
	if len(issue.LinkedPRs) != 1 || issue.LinkedPRs[0].Push == nil || issue.LinkedPRs[0].Push.State != pushStatePushed ||
		issue.LinkedPRs[0].Presence != nil || issue.LinkedPRs[0].HumanAttention != nil || issue.StageActor == nil || issue.StageActor.Kind != StageActorHive {
		t.Fatalf("covered-row projection retained old author work: %+v", issue)
	}
	// No webhook is needed. A real fresh evaluation that still needs a
	// push must restore the author state on this head, not an old event.
	store.NoteActivity(key, stagelabels.Activity{Head: newHead, Comments: -1, Reviews: -1, WaitingOnHive: false})
	tr = s.buildStageTracker(s.status, store, now)
	it = trackerItem(t, tr, "pr", 16012)
	if it.Push == nil || it.Push.State != pushStateWaiting || it.Rework == nil || it.Rework.State == reworkStatePushed ||
		it.StageActor == nil || it.StageActor.Kind != StageActorHuman || it.HumanAttention == nil || !it.NeedsHuman {
		t.Fatalf("fresh current-head author action was hidden: %+v", it)
	}
	if strings.Contains(it.Caption, "new head observed") {
		t.Fatalf("fresh author action still claims only re-review is pending: %q", it.Caption)
	}
}
