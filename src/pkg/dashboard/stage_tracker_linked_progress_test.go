package dashboard

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestStageTrackerLinkedPRProgressPayload(t *testing.T) {
	now := time.Date(2026, 10, 29, 1, 45, 0, 0, time.UTC)
	for _, reverse := range []bool{false, true} {
		for _, stage := range []stagelabels.Stage{stagelabels.None, stagelabels.Actionable, stagelabels.Claimed, stagelabels.PRInProgress} {
			t.Run(stage.Slug()+"/"+map[bool]string{false: "issue-link", true: "pr-link"}[reverse], func(t *testing.T) {
				s, store := stageTrackerFixture(t, now)
				issue := github.Issue{Number: 15218, Title: "Soak countdown", Author: "pat", CreatedAt: now.Add(-time.Hour)}
				if stage != stagelabels.None {
					store.Observe(stagelabels.Key("acme/console", issue.Number), stage, "actionable")
				}
				pr := github.PullRequest{Number: 15223, Author: "pat", Labels: []string{stagelabels.ReviewPending.Label()}}
				if reverse {
					pr.LinkedIssues = []github.PRLinkedIssue{{Number: issue.Number}}
				} else {
					issue.LinkedPRs = []github.IssueLinkedPR{{Number: pr.Number, State: "open"}}
				}
				repo := &s.status.Repos[0]
				repo.ActionableIssues = append(repo.ActionableIssues, issue,
					github.Issue{Number: 15219, Title: "Still new", CreatedAt: now})
				repo.OpenPrs = append(repo.OpenPrs, FrontendPR{PullRequest: pr})
				for _, prStage := range []stagelabels.Stage{stagelabels.ReviewPending, stagelabels.NeedsAuthor} {
					pr.Labels = []string{prStage.Label()}
					repo.OpenPrs[len(repo.OpenPrs)-1] = FrontendPR{PullRequest: pr}
					resp := s.buildStageTracker(s.status, store, now)
					got := trackerItem(t, resp, "issue", issue.Number)
					cover := trackerItem(t, resp, "pr", pr.Number)
					if got.New != nil || got.StageSlug == StageTrackerNewSlug || got.Caption != "Covered by PR #15223 — "+cover.Caption {
						t.Fatalf("issue retained stale intake narration: %+v", got)
					}
					if got.Stage != stage.Number() {
						t.Fatalf("narration must not mutate the issue's recorded stage: %d", got.Stage)
					}
					// The riding issue names the PR in its human_reason ("#15223 …");
					// who moves and why are the PR's.
					if got.StageActor == nil || cover.StageActor == nil || got.StageActor.Kind != cover.StageActor.Kind || got.StageActor.Login != cover.StageActor.Login || got.StageActor.Why != cover.StageActor.Why {
						t.Fatalf("issue actor %+v, want covering PR actor %+v", got.StageActor, cover.StageActor)
					}
					if fresh := trackerItem(t, resp, "issue", 15219); fresh.New == nil || !strings.Contains(fresh.Caption, "then the scanner") {
						t.Fatalf("no-PR intake behavior changed: %+v", fresh)
					}
				}
			})
		}
	}
}

func TestStageTrackerLinkedPRProgressBoundaries(t *testing.T) {
	prs := []StageTrackerLinkedPR{{Number: 15223, Caption: "Needs @pat to fix CI"}, {Number: 15224}}
	for name, it := range map[string]StageTrackerItem{
		"no PR":         {Kind: "issue"},
		"PR row":        {Kind: "pr", LinkedPRs: prs},
		"held":          {Kind: "issue", LinkedPRs: prs, Hold: &StageTrackerHold{}},
		"blocked":       {Kind: "issue", LinkedPRs: prs, BlockedOn: []string{"#1"}},
		"blocked on PR": {Kind: "issue", LinkedPRs: prs, BlockedOnPR: &StageTrackerBlockedOnPR{}},
		"decision":      {Kind: "issue", Stage: trackerNumberIssueDecision, LinkedPRs: prs},
	} {
		t.Run(name, func(t *testing.T) {
			it.Caption = "Keep the issue's own state"
			stageTrackerLinkedPRProgress(&it)
			if it.Caption != "Keep the issue's own state" {
				t.Fatalf("overrode an independent state: %+v", it)
			}
		})
	}
	for _, mirrored := range []bool{false, true} {
		it := StageTrackerItem{Kind: "issue", Stage: trackerNumberCoveredByPR, Mirrored: mirrored,
			LinkedPRs: prs, Reason: "open PR addresses part of it (references it without a closing keyword)"}
		if mirrored {
			it.Stage = trackerNumberReviewed
		}
		stageTrackerLinkedPRProgress(&it)
		if it.Caption != "Partially covered by PR #15223 — Needs @pat to fix CI; PR #15224" {
			t.Fatalf("partial/multiple/metadata-free coverage: %+v", it)
		}
	}
}

func TestStageTrackerLinkedPRProgressRowsAndEvents(t *testing.T) {
	items := []StageTrackerItem{
		{Repo: "acme/console", Number: 15218, Kind: "issue", StageSlug: StageTrackerNewSlug, New: &StageTrackerNew{},
			Caption: "New · waiting for the stage-label pass, then the scanner"},
		{Repo: "acme/console", Number: 15223, Kind: "pr", Stage: trackerNumberNeedsAuthor, Caption: "Needs @pat to fix CI",
			LinkedIssues: []StageTrackerLinkedIssue{{Number: 15218}}},
	}
	stageTrackerResolveLinkedPRs(items, nil)
	before, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	items[1].Caption = "Needs @pat to rebase"
	stageTrackerResolveLinkedPRs(items, nil)
	after, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	html := indexHTML(t)
	runStageTrackerRowActionsScript(t, stageTrackerRowsPreludeJS(t, html)+stageTrackerRowsFixtureJS+
		jsFunc(t, html, "stageTrackerNewState")+"\n"+jsFunc(t, html, "stageTrackerNewLines")+"\n"+
		"const before = "+string(before)+";\nconst after = "+string(after)+`;
const issue = before[0], key = stageTrackerItemKey(issue);
assert.strictEqual(stageTrackerNewState(issue), null);
assert.deepStrictEqual(stageTrackerNewLines(issue, now), []);
assert.ok(stageTrackerTooltip(issue, now).includes('Covered by PR #15223 — Needs @pat to fix CI'));
for (const row of [
  issue,
  Object.assign({}, issue, {linked_prs: issue.linked_prs.concat([{number: 15224, stage: 50, caption: 'Review pending'}])}),
  Object.assign({}, issue, {linked_prs: issue.linked_prs.map(lp => Object.assign({}, lp, {drawn_under: 1}))})
]) {
  const rendered = stageTrackerRowHtml(row, stages, {nowMs: now});
  assert.ok(!rendered.includes('then the scanner'), rendered);
  assert.ok(!rendered.includes('ham-open-new'), rendered);
}
const prev = stageTrackerStateMap(before), next = stageTrackerStateMap(after);
assert.ok(prev[key].caption.includes('Needs @pat to fix CI'));
assert.deepStrictEqual(stageTrackerEventsNext({}, null, prev, stages, now), {});
const events = stageTrackerEventsNext({}, prev, next, stages, now + 1000);
const line = stageTrackerEventHtml(key, {events, nowMs: now + 1000});
assert.ok(line.includes('Hive') && line.includes('Covered by PR #15223 — Needs @pat to rebase'), line);
assert.ok(!line.includes('scanner') && !line.includes('stage-label pass'), line);
`)
}
