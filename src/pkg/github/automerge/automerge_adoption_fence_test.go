package automerge

import (
	"context"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestCarriedFenceMarkerRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "tests removed", body: CarriedFenceMarker(contributorLaneReasonTestsRemoved, 13212) + "\nSupersedes #13212.", want: contributorLaneReasonTestsRemoved},
		{name: "workflow path, no origin", body: "intro\n" + CarriedFenceMarker(contributorLaneReasonWorkflowPath, 0), want: contributorLaneReasonWorkflowPath},
		{name: "no marker", body: "Supersedes #7 (adopted).", want: ""},
		{name: "unknown reason ignored", body: CarriedFenceMarker("contributor-lane:no-claim", 7), want: ""},
		{name: "deferral verdict is not a fence", body: CarriedFenceMarker(adoptionReasonHeadChanged, 7), want: ""},
		{name: "empty body", body: "", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := carriedFenceReason(tc.body); got != tc.want {
				t.Fatalf("carriedFenceReason(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
	if got := CarriedFenceMarker(contributorLaneReasonTestsRemoved, 13212); got != "<!-- hive:carried-fence reason=contributor-lane:tests-removed from=#13212 -->" {
		t.Fatalf("marker = %q", got)
	}
}

func TestAdoptionFenceDefersOnlyUnreadableVerdicts(t *testing.T) {
	for reason, want := range map[string]bool{
		adoptionReasonHeadChanged:          true,
		contributorLaneReasonFilesEvidence: true,
		contributorLaneReasonTestsRemoved:  false,
		contributorLaneReasonWorkflowPath:  false,
	} {
		if got := adoptionFenceDefers(reason); got != want {
			t.Fatalf("adoptionFenceDefers(%q) = %v, want %v", reason, got, want)
		}
	}
}

// A green App PR whose body carries a diff fence from the contributor PR it
// superseded is never merged by the self-authored lane while its current
// diff still trips that fence: the reason is the carried contributor-lane
// fence (a stage-085 reason) and no PUT .../merge happens. The fence is
// re-checked against the adopting PR's own diff on every head
// (hivecommons/hive#19537): a carried fence the diff no longer trips is
// dropped and the PR merges, and a file list that cannot be read or is
// incomplete keeps the fence. A body without the marker merges as before.
func TestSelfAuthoredSweepCarriedFenceHoldsMerge(t *testing.T) {
	workflowBody := CarriedFenceMarker(contributorLaneReasonWorkflowPath, 7) + "\nSupersedes #7."
	for _, tc := range []struct {
		name         string
		body         string
		files        []string
		removed      []string
		renamedFrom  map[string]string
		changedFiles int
		filesStatus  int
		wantReason   string
		wantMerges   int
	}{
		{name: "tests-removed carried, tests still removed", body: adoptionPRBodyWithFence(contributorLaneReasonTestsRemoved), removed: []string{"pkg/widget/widget_test.go"}, wantReason: contributorLaneReasonTestsRemoved},
		{name: "tests-removed carried, tests restored", body: adoptionPRBodyWithFence(contributorLaneReasonTestsRemoved), files: []string{"pkg/widget/widget.go", "pkg/widget/widget_test.go"}, wantMerges: 1},
		{name: "workflow-path carried, workflow file in diff", body: workflowBody, files: []string{"src/main.go", ".github/workflows/ci.yml"}, wantReason: contributorLaneReasonWorkflowPath},
		{name: "workflow-path carried, action file in diff", body: workflowBody, files: []string{".github/actions/setup/action.yml"}, wantReason: contributorLaneReasonWorkflowPath},
		{name: "workflow-path carried, workflow renamed out of the directory", body: workflowBody, files: []string{"scripts/ci.yml"}, renamedFrom: map[string]string{"scripts/ci.yml": ".github/workflows/ci.yml"}, wantReason: contributorLaneReasonWorkflowPath},
		{name: "workflow-path carried, no workflow or action file in diff", body: workflowBody, files: []string{"src/main.go", "docs/github-workflows.md"}, wantMerges: 1},
		{name: "workflow-path carried, file list unreadable", body: workflowBody, files: []string{"src/main.go"}, filesStatus: http.StatusInternalServerError, wantReason: contributorLaneReasonWorkflowPath},
		{name: "workflow-path carried, file list incomplete", body: workflowBody, files: []string{"src/main.go"}, changedFiles: 3, wantReason: contributorLaneReasonWorkflowPath},
		{name: "no marker merges", body: "Supersedes #7 (adopted after CI went red).", wantMerges: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merges := 0
			api := newSelfSweepGuardAPI(t, selfSweepFixture{
				body: tc.body, files: tc.files, removedFiles: tc.removed, renamedFrom: tc.renamedFrom,
				changedFiles: tc.changedFiles, filesHTTPCode: tc.filesStatus, mergeApplied: true, mergeCalls: &merges,
			})
			defer api.Close()
			c := newAutoMergeSweepClient(api.URL)
			for tick := 0; tick < 2; tick++ {
				event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
				if err != nil {
					t.Fatalf("tick %d: err = %v", tick, err)
				}
				if tc.wantReason != "" && reason != tc.wantReason {
					t.Fatalf("tick %d: reason = %q, want %q", tick, reason, tc.wantReason)
				}
				if tc.wantReason != "" && event.MergeSHA != "" {
					t.Fatalf("tick %d: fenced PR merged: %+v", tick, event)
				}
				if tc.wantMerges > 0 && carriedFenceReasons[reason] {
					t.Fatalf("tick %d: dropped fence still held the PR: %q", tick, reason)
				}
			}
			if tc.wantMerges == 0 && merges != 0 {
				t.Fatalf("merges = %d, want 0", merges)
			}
			if tc.wantMerges > 0 && merges == 0 {
				t.Fatal("PR whose carried fence no longer applies did not merge")
			}
		})
	}
}

func TestCarriedFenceStillApplies(t *testing.T) {
	file := func(name, status string, adds, dels int) intent.ChangedFile {
		return intent.ChangedFile{Filename: name, Status: status, Additions: adds, Deletions: dels}
	}
	for _, tc := range []struct {
		name   string
		reason string
		files  []intent.ChangedFile
		want   bool
	}{
		{name: "workflow edit", reason: contributorLaneReasonWorkflowPath, files: []intent.ChangedFile{file(".github/workflows/ci.yml", "modified", 1, 1)}, want: true},
		{name: "nested workflow edit", reason: contributorLaneReasonWorkflowPath, files: []intent.ChangedFile{file("sub/.github/workflows/ci.yml", "modified", 1, 1)}, want: true},
		{name: "action edit", reason: contributorLaneReasonWorkflowPath, files: []intent.ChangedFile{file(".github/actions/x/action.yml", "added", 3, 0)}, want: true},
		{name: "renamed out of workflows", reason: contributorLaneReasonWorkflowPath, files: []intent.ChangedFile{{Filename: "ci.yml", Status: "renamed", PreviousFilename: ".github/workflows/ci.yml"}}, want: true},
		{name: "no workflow file", reason: contributorLaneReasonWorkflowPath, files: []intent.ChangedFile{file("src/main.go", "modified", 1, 1), file(".github/CODEOWNERS", "modified", 1, 0)}},
		{name: "empty diff", reason: contributorLaneReasonWorkflowPath},
		{name: "test file removed", reason: contributorLaneReasonTestsRemoved, files: []intent.ChangedFile{file("pkg/a_test.go", "removed", 0, 10)}, want: true},
		{name: "test lines deleted past floor", reason: contributorLaneReasonTestsRemoved, files: []intent.ChangedFile{file("pkg/a_test.go", "modified", 1, contributorLaneTestDeletionFloor)}, want: true},
		{name: "tests kept", reason: contributorLaneReasonTestsRemoved, files: []intent.ChangedFile{file("pkg/a_test.go", "modified", 10, 2)}},
		{name: "tests-removed ignores a workflow edit", reason: contributorLaneReasonTestsRemoved, files: []intent.ChangedFile{file(".github/workflows/ci.yml", "modified", 1, 1)}},
		{name: "unknown reason applies", reason: "contributor-lane:something-new", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := carriedFenceStillApplies(tc.reason, tc.files); got != tc.want {
				t.Fatalf("carriedFenceStillApplies(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

// The drop is logged once per head: a second sweep over the same head is
// quiet, a new head logs again.
func TestNoteCarriedFenceDropOncePerHead(t *testing.T) {
	var c Engine
	if !c.noteCarriedFenceDrop("acme", "widget", 7, "aaa") {
		t.Fatal("first drop on a head must log")
	}
	if c.noteCarriedFenceDrop("acme", "widget", 7, "AAA") {
		t.Fatal("second drop on the same head must not log")
	}
	if !c.noteCarriedFenceDrop("acme", "widget", 7, "bbb") {
		t.Fatal("a new head must log again")
	}
}

// adoptionPRBodyWithFence renders a superseding PR body carrying fence, as
// supersedeAdoptedPR would for the adopted contributor PR #7.
func adoptionPRBodyWithFence(fence string) string {
	obs := stageObservation{Owner: "acme", Repo: "widget", Display: "acme/widget", Number: 7, HeadSHA: "aaa", Author: "mallory", BaseRef: "v5", Title: "Fix the widget"}
	return AdoptionPRBody(obs, stagelabels.Record{AdoptReason: AdoptReasonOffline, AdoptDiffFence: fence}, false, "")
}
