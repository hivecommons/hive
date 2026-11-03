package automerge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// manualMergeSweep parks contributor PR 9 at 085 for reason with the given
// labels and verdict, and returns the entry comments posted on that pass.
func manualMergeSweep(t *testing.T, files []string, verdict ContributorLaneVerdict, reason string, labels ...string) (*stageAPI, func(), func() []string) {
	t.Helper()
	return manualMergeSweepWith(t, nil, files, verdict, reason, labels...)
}

// manualMergeSweepWith is manualMergeSweep with setup applied to the engine
// first (lane policy, unauthorized steps).
func manualMergeSweepWith(t *testing.T, setup func(*Engine), files []string, verdict ContributorLaneVerdict, reason string, labels ...string) (*stageAPI, func(), func() []string) {
	t.Helper()
	api, closeAPI := seededStageAPI(t)
	if files != nil {
		api.prFiles = map[int][]string{9: files}
	}
	clock := &contributorLaneClock{at: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	e, store := humanStageEngine(t, api.url, clock, &verdict, 0)
	if setup != nil {
		setup(e)
	}
	sweep := func() []string {
		t.Helper()
		api.reset()
		e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", stagePR(9, labels...), reason, false, nil)
		e.noteStageVerdict("acme", "widget", 9, verdict)
		e.noteStageRole("acme", "widget", 9, stagelabels.RoleTrusted)
		if _, err := e.ApplyStageLabels(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := stageOf(t, store, 9); got != stagelabels.NeedsDecision {
			t.Fatalf("stage=%v want 085", got)
		}
		return append([]string(nil), api.comments...)
	}
	return api, closeAPI, sweep
}

var requiresHumanVerdict = ContributorLaneVerdict{
	Configured:     true,
	Verdict:        "requires_human",
	HumanSummaries: []string{"security requires human: adds a workflow job"},
}

// A contributor PR that edits workflow files and is also parked for a
// sentinel alert and a requires_human verdict (Danathar/zfs-kinoite-complex
// #498): the lane stopped at the sentinel label before its diff fence, so the
// entry comment used to ask a trusted merger to approve the head and "the
// hive re-evaluates", although no approval can merge a workflow edit. It
// must say from the first comment that a person merges it by hand, why, and
// still list the alert and the verdict.
func TestManualMergeNamedWhileSentinelBlocks(t *testing.T) {
	api, closeAPI, sweep := manualMergeSweep(t, []string{".github/workflows/test.yml", "tests/mutation_check.py"}, requiresHumanVerdict,
		contributorLaneReasonSentinelBlocked, "role/trusted", "author/clanker", "stage/050-review-pending", "sentinel-alert")
	defer closeAPI()

	comments := sweep()
	if len(comments) != 1 {
		t.Fatalf("comments = %q", comments)
	}
	c := comments[0]
	for _, want := range []string{
		"Needs a manual merge decision",
		"the reviewer swarm returned `requires_human`",
		"a sentinel alert is open on this PR",
		"edits CI workflow / action files",
		"a maintainer must review and merge this PR manually",
	} {
		if !strings.Contains(c, want) {
			t.Fatalf("085 comment missing %q:\n%s", want, c)
		}
	}
	for _, unwanted := range []string{"approves this head", "the automerge lane will not merge this unattended"} {
		if strings.Contains(c, unwanted) {
			t.Fatalf("085 comment must not say %q:\n%s", unwanted, c)
		}
	}

	// The diff is read once per head, not on every pass.
	if api.fileReads != 1 {
		t.Fatalf("file list read %d times on the first pass", api.fileReads)
	}
	sweep()
	if api.fileReads != 1 {
		t.Fatalf("file list re-read for an unchanged head: %d reads", api.fileReads)
	}
}

// The same for a hold label: the hold is named, and so is the manual merge.
func TestManualMergeNamedWhileHoldLabelBlocks(t *testing.T) {
	_, closeAPI, sweep := manualMergeSweep(t, []string{".github/workflows/ci.yml"}, ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"},
		"label:hold", "role/trusted", "author/clanker", "stage/050-review-pending", "hold")
	defer closeAPI()

	comments := sweep()
	if len(comments) != 1 || !strings.Contains(comments[0], "the `hold` label is set") ||
		!strings.Contains(comments[0], "a maintainer must review and merge this PR manually") {
		t.Fatalf("085 comment = %q", comments)
	}
}

// A clean diff, or one that cannot be read, leaves the comment as the
// lane's reason alone states it: approve the head and the hive re-evaluates.
func TestManualMergeNotClaimedWithoutWorkflowFence(t *testing.T) {
	for name, files := range map[string][]string{
		"clean diff":      {"README.md", "src/x.go"},
		"diff unreadable": nil,
	} {
		t.Run(name, func(t *testing.T) {
			_, closeAPI, sweep := manualMergeSweep(t, files, requiresHumanVerdict,
				contributorLaneReasonSentinelBlocked, "role/trusted", "author/clanker", "stage/050-review-pending", "sentinel-alert")
			defer closeAPI()

			comments := sweep()
			if len(comments) != 1 {
				t.Fatalf("comments = %q", comments)
			}
			c := comments[0]
			if !strings.Contains(c, "Needs a decision, not a merge") || !strings.Contains(c, "approves this head") ||
				strings.Contains(c, "merge this PR manually") {
				t.Fatalf("085 comment = %q", c)
			}
		})
	}
}

// releaseWorkflowFence turns on auto_merge.contributor_lane.
// owner_approval_releases_workflow_fence (hivecommons/hive#14664).
func releaseWorkflowFence(e *Engine) {
	e.contributorLanePolicy = func() ContributorLanePolicy {
		return ContributorLanePolicy{Enabled: true, RequireTiers: map[string]bool{"trusted": true}, OwnerApprovalReleasesWorkflowFence: true}
	}
}

// projectbluefin/testsuite#1037: with the owner-approval release on, the
// workflow-path fence named behind a sentinel alert is one a trusted
// merger's Approve review releases. The entry comment used to say no
// approval overrides it while the hive's other note asked for exactly that
// approval; it now asks for the review and still lists the alert.
func TestWorkflowFenceApprovalReleaseNamedWhileSentinelBlocks(t *testing.T) {
	_, closeAPI, sweep := manualMergeSweepWith(t, releaseWorkflowFence, []string{".github/workflows/e2e.yml"}, requiresHumanVerdict,
		contributorLaneReasonSentinelBlocked, "role/trusted", "author/clanker", "stage/050-review-pending", "sentinel-alert")
	defer closeAPI()

	comments := sweep()
	if len(comments) != 1 {
		t.Fatalf("comments = %q", comments)
	}
	c := comments[0]
	for _, want := range []string{
		"Needs a decision, not a merge",
		"a sentinel alert is open on this PR",
		"awaiting owner approval",
		"with a GitHub **Approve** review",
		"releases the CI workflow-file fence and settles the reviewer's `requires_human` verdict",
		"- resolve the sentinel alert and have a maintainer clear its label",
	} {
		if !strings.Contains(c, want) {
			t.Fatalf("085 comment missing %q:\n%s", want, c)
		}
	}
	for _, unwanted := range []string{"merge this PR manually", "does not override", "never merges unattended"} {
		if strings.Contains(c, unwanted) {
			t.Fatalf("085 comment must not say %q:\n%s", unwanted, c)
		}
	}
}

// The release is on but the lane learnt the App cannot merge this head's
// workflow files (unauthorized merge-workflow-path): a person merges it.
func TestWorkflowFenceUnauthorizedStaysManualWhileSentinelBlocks(t *testing.T) {
	setup := func(e *Engine) {
		releaseWorkflowFence(e)
		e.unauthorized = map[string]unauthorizedState{e.unauthorizedKey("acme", "widget", 9): {Head: "sha", Step: unauthorizedStepMergeWorkflowPath}}
	}
	_, closeAPI, sweep := manualMergeSweepWith(t, setup, []string{".github/workflows/e2e.yml"}, requiresHumanVerdict,
		contributorLaneReasonSentinelBlocked, "role/trusted", "author/clanker", "stage/050-review-pending", "sentinel-alert")
	defer closeAPI()

	comments := sweep()
	if len(comments) != 1 || !strings.Contains(comments[0], "a maintainer must review and merge this PR manually") ||
		!strings.Contains(comments[0], "never merges unattended") || strings.Contains(comments[0], "workflow-file fence") {
		t.Fatalf("085 comment = %q", comments)
	}
}

// The lane itself stopped at the workflow-path fence (nothing else blocks
// the head): with the release on, the 085 comment asks for the approval
// its cause line names, not for a manual merge.
func TestWorkflowFenceLaneReasonAsksForApprovalWhenReleased(t *testing.T) {
	approved := ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
	for name, release := range map[string]bool{"release on": true, "release off": false} {
		t.Run(name, func(t *testing.T) {
			var setup func(*Engine)
			if release {
				setup = releaseWorkflowFence
			}
			_, closeAPI, sweep := manualMergeSweepWith(t, setup, nil, approved, contributorLaneReasonWorkflowPath, "role/trusted", "author/clanker", "stage/050-review-pending")
			defer closeAPI()
			comments := sweep()
			if len(comments) != 1 {
				t.Fatalf("comments = %q", comments)
			}
			asks := strings.Contains(comments[0], "**What to do:** a trusted merger approves this head (`sha`) with a GitHub **Approve** review")
			manual := strings.Contains(comments[0], "a maintainer must review and merge this PR manually")
			if asks != release || manual == release || strings.Contains(comments[0], "awaiting owner approval") != release {
				t.Fatalf("085 comment (release=%v):\n%s", release, comments[0])
			}
		})
	}
}
