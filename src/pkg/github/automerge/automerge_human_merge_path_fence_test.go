package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// A contributor PR whose workflow edit is also listed in
// auto_merge.human_merge_paths: with owner_approval_releases_workflow_fence
// on, the stage used to say an owner's Approve review "releases the fence and
// the hive merges it". The approval only led the lane to the human-merge-path
// gate, which holds the PR and never merges it. The stage now says from the
// start that a person merges it, and parks it at 090 once it is reviewed,
// green and soaked.

var humanMergePathFenceApproved = ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}

// humanMergePathFenceSetup turns the owner-approval release on under scope,
// with soak, and lists the release workflow in human_merge_paths.
func humanMergePathFenceSetup(scope string, soak time.Duration) func(*Engine) {
	return func(e *Engine) {
		e.contributorLanePolicy = func() ContributorLanePolicy {
			return ContributorLanePolicy{Enabled: true, Soak: soak, RequireTiers: map[string]bool{"trusted": true}, OwnerApprovalReleasesWorkflowFence: true, WorkflowFenceScope: scope}
		}
		e.SetHumanMergePaths(humanPathsFor("acme/widget", testReleaseWorkflow))
	}
}

var humanMergePathFenceScopes = []string{config.ContributorLaneWorkflowFenceScopeAll, config.ContributorLaneWorkflowFenceScopeHumanMergePaths}

// The evidence the stage routes on: no approval releases the fence, the
// cause is the human-merge path, and it is a rule no approval overrides. On
// v6 tip ApprovalReleasesFence is true and the cause is the awaiting-approval
// prose.
func TestHumanMergePathFenceEvidenceNeverAsksForApproval(t *testing.T) {
	for _, scope := range humanMergePathFenceScopes {
		t.Run(scope, func(t *testing.T) {
			api, closeAPI := seededStageAPI(t)
			defer closeAPI()
			api.prFiles = map[int][]string{9: {testReleaseWorkflow, "src/widget.go"}}
			clock := &contributorLaneClock{at: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
			verdict := humanMergePathFenceApproved
			e, _ := humanStageEngine(t, api.url, clock, &verdict, 0)
			humanMergePathFenceSetup(scope, 0)(e)

			obs := stageObservation{Lane: stageLaneContributor, Display: "acme/widget", Owner: "acme", Repo: "widget", Number: 9, HeadSHA: "sha", Reason: contributorLaneReasonWorkflowPath}
			ev := e.humanEvidence(context.Background(), e.stageTrackerOrShared(), stageKey("acme", "widget", 9), obs, stagelabels.CauseDecision, clock.now())
			if ev.ApprovalReleasesFence {
				t.Fatal("ApprovalReleasesFence = true for a head that touches a human-merge path")
			}
			if ev.CauseReason != prstage.CauseHumanMergePath || !ev.ManualMerge || !ev.ManualMergeRule {
				t.Fatalf("cause = %q manual=%v rule=%v, want the human-merge-path rule", ev.CauseReason, ev.ManualMerge, ev.ManualMergeRule)
			}
			if ev.Cause != stagelabels.CauseTrust || ev.LaneReason != autoMergeReasonHumanMergePath {
				t.Fatalf("cause kind = %v lane reason = %q, want no decision owed under %q", ev.Cause, ev.LaneReason, autoMergeReasonHumanMergePath)
			}
		})
	}
}

// From review to merge click: no verdict yet is 050, the soak running is
// 060, and once the head is reviewed, green and soaked it is 090 under the
// human-merge-path reason. No comment on the way asks for an approval. On v6
// tip the approved head sits at 085 "awaiting owner approval".
func TestHumanMergePathFenceRoutesTo090AfterVerdictChecksAndSoak(t *testing.T) {
	for _, scope := range humanMergePathFenceScopes {
		t.Run(scope, func(t *testing.T) {
			api, closeAPI := seededStageAPI(t)
			defer closeAPI()
			api.prFiles = map[int][]string{9: {testReleaseWorkflow}}
			clock := &contributorLaneClock{at: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
			verdict := ContributorLaneVerdict{Configured: true}
			e, store := humanStageEngine(t, api.url, clock, &verdict, 0)
			humanMergePathFenceSetup(scope, 30*time.Minute)(e)

			var all []string
			sweep := func(want stagelabels.Stage) {
				t.Helper()
				api.reset()
				e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(9, "role/trusted", "author/clanker", "stage/050-review-pending"), contributorLaneReasonWorkflowPath, false, nil)
				e.noteStageVerdict("acme", "widget", 9, verdict)
				e.noteStageRole("acme", "widget", 9, stagelabels.RoleTrusted)
				if _, err := e.ApplyStageLabels(context.Background()); err != nil {
					t.Fatal(err)
				}
				if got := stageOf(t, store, 9); got != want {
					t.Fatalf("stage = %v, want %v (comments %q)", got, want, api.comments)
				}
				all = append(all, api.comments...)
			}

			sweep(stagelabels.ReviewPending)
			verdict = humanMergePathFenceApproved
			clock.at = clock.at.Add(10 * time.Minute)
			sweep(stagelabels.Reviewed)
			clock.at = clock.at.Add(25 * time.Minute)
			sweep(stagelabels.ReadyForHumanMerge)

			rec, ok := store.Get(stageKey("acme", "widget", 9))
			if !ok || prstage.HumanMergeReason(rec.Reason) != prstage.HumanMergeReasonManualMergePolicy {
				t.Fatalf("090 reason = %q (ok=%v), want the human-merge-path reason", rec.Reason, ok)
			}
			var ready string
			for _, c := range all {
				for _, unwanted := range []string{"awaiting owner approval", "approves this head", "releases the fence", "workflow-file fence"} {
					if strings.Contains(c, unwanted) {
						t.Fatalf("comment says %q:\n%s", unwanted, c)
					}
				}
				if strings.Contains(c, stagelabels.ReadyCommentMarker) {
					ready = c
				}
			}
			for _, want := range []string{"Ready for a human merge", "The PR touches a human-merge path (auto_merge.human_merge_paths).", "click **Merge**"} {
				if !strings.Contains(ready, want) {
					t.Fatalf("090 comment missing %q:\n%s", want, ready)
				}
			}
		})
	}
}

// An unanswered requires_human verdict still owes its decision: 085, a
// manual merge, the human-merge path named, and no approval ask.
func TestHumanMergePathFenceRequiresHumanStaysAManualMergeDecision(t *testing.T) {
	_, closeAPI, sweep := manualMergeSweepWith(t, humanMergePathFenceSetup(config.ContributorLaneWorkflowFenceScopeAll, 0), []string{testReleaseWorkflow}, requiresHumanVerdict,
		contributorLaneReasonWorkflowPath, "role/trusted", "author/clanker", "stage/050-review-pending")
	defer closeAPI()
	comments := sweep()
	if len(comments) != 1 {
		t.Fatalf("comments = %q", comments)
	}
	c := comments[0]
	for _, want := range []string{"Needs a manual merge decision", "the reviewer swarm returned `requires_human`", prstage.CauseHumanMergePath, "a maintainer must review and merge this PR manually"} {
		if !strings.Contains(c, want) {
			t.Fatalf("085 comment missing %q:\n%s", want, c)
		}
	}
	if strings.Contains(c, "awaiting owner approval") || strings.Contains(c, "approves this head") {
		t.Fatalf("085 comment asks for an approval:\n%s", c)
	}
}

// A workflow edit outside human_merge_paths keeps today's route: the
// owner's Approve review releases the fence and the hive merges.
func TestHumanMergePathFenceLeavesUnlistedWorkflowAwaitingApproval(t *testing.T) {
	_, closeAPI, sweep := manualMergeSweepWith(t, humanMergePathFenceSetup(config.ContributorLaneWorkflowFenceScopeAll, 0), []string{".github/workflows/lint.yml"}, humanMergePathFenceApproved,
		contributorLaneReasonWorkflowPath, "role/trusted", "author/clanker", "stage/050-review-pending")
	defer closeAPI()
	comments := sweep()
	if len(comments) != 1 || !strings.Contains(comments[0], "awaiting owner approval") ||
		!strings.Contains(comments[0], "**What to do:** a trusted merger approves this head (`sha`) with a GitHub **Approve** review") {
		t.Fatalf("085 comment = %q", comments)
	}
}

// humanMergePathFenceHoldSweep runs one pass over PR 21 held with `hold`,
// its head approved and green, and returns the stage record and comments.
func humanMergePathFenceHoldSweep(t *testing.T, patterns, files []string, comments, events []map[string]any) (stagelabels.Record, []string) {
	t.Helper()
	api := newHumanMergePathLiftAPI(t, 21, files, comments, events)
	clock := &contributorLaneClock{at: time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)}
	verdict := humanMergePathFenceApproved
	e, store := humanStageEngine(t, api.url, clock, &verdict, 0)
	e.SetHumanMergePaths(func(string) []string { return patterns })
	e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(21, "hold", "role/trusted", "author/clanker", "stage/050-review-pending"), "label:hold", false, nil)
	e.noteStageVerdict("acme", "widget", 21, verdict)
	e.noteStageRole("acme", "widget", 21, stagelabels.RoleTrusted)
	if _, err := e.ApplyStageLabels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stageOf(t, store, 21); got != stagelabels.ReadyForHumanMerge {
		t.Fatalf("stage = %v, want 090 (comments %q)", got, api.comments)
	}
	rec, _ := store.Get(stageKey("acme", "widget", 21))
	api.mu.Lock()
	defer api.mu.Unlock()
	return rec, append([]string(nil), api.comments...)
}

func humanMergePathFenceReadyComment(comments []string) string {
	for _, c := range comments {
		if strings.Contains(c, stagelabels.ReadyCommentMarker) {
			return c
		}
	}
	return ""
}

// The hive's own human-merge-path hold on a reviewed, green head is 090 for a
// person's merge under the human-merge-path reason, whether the path is a
// workflow or not. On v6 tip it is a generic held 090 ("label:hold") that
// tells the person to lift the hold and approve.
func TestHumanMergePathFenceHiveHoldRoutesTo090WithTheHumanMergePathReason(t *testing.T) {
	for _, tc := range []struct {
		name     string
		patterns []string
		files    []string
	}{
		{name: "workflow file", patterns: []string{".github/workflows/**"}, files: []string{".github/workflows/release.yml", "src/widget.go"}},
		{name: "other file", patterns: []string{"deploy/**"}, files: []string{"deploy/prod.yaml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, comments := humanMergePathFenceHoldSweep(t, tc.patterns, tc.files, appHumanMergePathNotice(), appHumanMergePathHold())
			if !rec.Hold.SelfImposed() || !rec.Hold.HumanMergePath() {
				t.Fatalf("hold = %+v, want the hive's human-merge-path hold", rec.Hold)
			}
			if prstage.HumanMergeReason(rec.Reason) != prstage.HumanMergeReasonManualMergePolicy {
				t.Fatalf("090 reason = %q, want the human-merge-path reason", rec.Reason)
			}
			ready := humanMergePathFenceReadyComment(comments)
			for _, want := range []string{"The PR touches a human-merge path (auto_merge.human_merge_paths).", "Held: the `hold` label was set by the hive itself", "click **Merge**"} {
				if !strings.Contains(ready, want) {
					t.Fatalf("090 comment missing %q:\n%s", want, ready)
				}
			}
			if strings.Contains(ready, "lift the hold") || strings.Contains(ready, "approves this head") {
				t.Fatalf("090 comment hands the PR back to the lane:\n%s", ready)
			}
		})
	}
}

// A hold a person set is theirs to lift: even on a head that touches a
// human-merge path the route is today's held 090, under the hold's reason.
func TestHumanMergePathFencePersonHoldRoutesAsBefore(t *testing.T) {
	events := []map[string]any{holdEvent("labeled", "maintainer", humanMergePathLiftHoldAt)}
	rec, comments := humanMergePathFenceHoldSweep(t, []string{".github/workflows/**"}, []string{".github/workflows/release.yml"}, nil, events)
	if rec.Hold == nil || rec.Hold.By != "maintainer" {
		t.Fatalf("hold = %+v, want the maintainer's", rec.Hold)
	}
	if rec.Reason != "label:hold" {
		t.Fatalf("090 reason = %q, want label:hold", rec.Reason)
	}
	ready := humanMergePathFenceReadyComment(comments)
	if !strings.Contains(ready, "Held: the `hold` label was set by @maintainer") || !strings.Contains(ready, "lift the hold") || strings.Contains(ready, prstage.CauseHumanMergePath) {
		t.Fatalf("090 comment = %q", ready)
	}
}

func humanMergePathFencePinPR(author string) (*gh.PullRequest, []intent.ChangedFile) {
	pr := &gh.PullRequest{Number: gh.Ptr(9), User: &gh.User{Login: gh.Ptr(author)}, ChangedFiles: gh.Ptr(1), Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha")}}
	files := []intent.ChangedFile{{Filename: ".github/workflows/ci.yml", Status: "modified", Additions: 1, Deletions: 1, Patch: "@@ -3 +3 @@\n-" + testPinOldLine + "\n+" + testPinNewLine}}
	return pr, files
}

// What a head's file list records: a match is a person's merge unless the
// pin-only exception might admit the PR; no paths is known and never a
// person's merge; a configuration change re-judges the head.
func TestHumanMergePathFenceHeadRecord(t *testing.T) {
	obs := stageObservation{Owner: "acme", Repo: "widget", Number: 9, HeadSHA: "sha"}
	key := stageKey("acme", "widget", 9)
	for _, tc := range []struct {
		name    string
		pinOnly func(string) (config.PinOnlyException, bool)
		author  string
		want    bool
	}{
		{name: "no exception: person-merged", author: "renovate-bot", want: true},
		{name: "pin-only candidate: route unchanged", pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "renovate-bot", want: false},
		{name: "untrusted author: person-merged", pinOnly: pinOnlyFor("acme/widget", testPinOnlyException()), author: "mallory", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New(nil, Options{})
			tracker := NewStageTracker()
			e.SetHumanMergePaths(humanPathsFor("acme/widget", testHumanMergePattern))
			e.SetHumanMergePinOnly(tc.pinOnly)
			pr, files := humanMergePathFencePinPR(tc.author)
			if _, known := e.humanMergePathFenceHead(tracker, key, obs); known {
				t.Fatal("an unread head is known")
			}
			e.noteHumanMergePathFenceHead(tracker, key, "acme", "widget", "sha", pr, files)
			if got, known := e.humanMergePathFenceHead(tracker, key, obs); !known || got != tc.want {
				t.Fatalf("person-merged = %v known = %v, want %v", got, known, tc.want)
			}
			moved := obs
			moved.HeadSHA = "sha2"
			if _, known := e.humanMergePathFenceHead(tracker, key, moved); known {
				t.Fatal("another head is known from this one's files")
			}
			e.SetHumanMergePaths(humanPathsFor("acme/widget", "deploy/**"))
			if _, known := e.humanMergePathFenceHead(tracker, key, obs); known {
				t.Fatal("a human_merge_paths change did not re-judge the head")
			}
			e.SetHumanMergePaths(nil)
			if got, known := e.humanMergePathFenceHead(tracker, key, obs); !known || got {
				t.Fatalf("no paths: person-merged = %v known = %v, want false/true", got, known)
			}
		})
	}
}

// A PR the pin-only exception might admit keeps today's route: the owner's
// approval is asked for, and the gate decides once it is given.
func TestHumanMergePathFencePinOnlyCandidateRoutesAsBefore(t *testing.T) {
	setup := func(e *Engine) {
		releaseWorkflowFence(e)
		e.SetHumanMergePaths(humanPathsFor("acme/widget", testHumanMergePattern))
		e.SetHumanMergePinOnly(pinOnlyFor("acme/widget", testPinOnlyException()))
		pr, files := humanMergePathFencePinPR("renovate-bot")
		e.noteHumanMergePathFenceHead(e.stageTrackerOrShared(), stageKey("acme", "widget", 9), "acme", "widget", "sha", pr, files)
	}
	_, closeAPI, sweep := manualMergeSweepWith(t, setup, []string{".github/workflows/ci.yml"}, humanMergePathFenceApproved,
		contributorLaneReasonWorkflowPath, "role/trusted", "author/clanker", "stage/050-review-pending")
	defer closeAPI()
	comments := sweep()
	if len(comments) != 1 || !strings.Contains(comments[0], "awaiting owner approval") {
		t.Fatalf("085 comment = %q", comments)
	}
}

// The contributor lane records the match from the file list it already read
// for its diff fence, so the stage pass needs no read of its own.
func TestHumanMergePathFenceLaneRecordsTheHeadsFiles(t *testing.T) {
	fx := validContributorFixture()
	fx.files = []map[string]any{{"filename": testReleaseWorkflow, "status": "modified", "additions": 2, "deletions": 1}}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: mustParseTime(t, "2026-11-10T13:00:00Z")}
	tracker := NewStageTracker()
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock), HumanMergePaths: humanPathsFor("acme/widget", testReleaseWorkflow), StageTracker: tracker})
	_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil || reason != contributorLaneReasonWorkflowPath {
		t.Fatalf("reason = %q err = %v, want %q", reason, err, contributorLaneReasonWorkflowPath)
	}
	obs := stageObservation{Owner: "acme", Repo: "widget", Number: 7, HeadSHA: "sha7"}
	if got, known := engine.humanMergePathFenceHead(tracker, stageKey("acme", "widget", 7), obs); !known || !got {
		t.Fatalf("lane record: person-merged = %v known = %v, want true/true", got, known)
	}
}
