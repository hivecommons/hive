package automerge

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// projectbluefin/utah-packages#812 and projectbluefin/common#1611: a fork PR
// the hive could not update because main had changed .github/workflows/
// parked at 085 with "bring it into the repository yourself ... otherwise
// close it". GitHub refuses any App push to a fork that carries workflow
// files (the App is not installed on the fork), so no grant fixes it; the
// one action is a person's branch update, and the note must say so.

const forkUpdateWorkflowFile = ".github/workflows/bump-upstream-enqueue.yml"

func forkUpdateObs(reason string, labels ...string) stageObservation {
	return stageObservation{
		Owner: "acme", Repo: "widget", Number: 7, HeadSHA: "aaa", HeadRepo: "mallory/widget",
		BaseRef: "main", Author: "mallory", Reason: reason, LaneReason: reason, Labels: labels,
	}
}

func TestForkRehomeBlockerTextNamesTheUpdate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		obs     stageObservation
		block   forkUpdateBlock
		want    []string
		notWant []string
	}{
		{
			name:  "base workflow change, maintainer edits on",
			obs:   forkUpdateObs("check-failing:unit"),
			block: forkUpdateBlock{WorkflowFile: forkUpdateWorkflowFile},
			want: []string{
				"comes from a fork (`mallory/widget`), and `main` has changed `" + forkUpdateWorkflowFile + "` since this branch was cut",
				"not installed on the fork",
				"inherited from `main`, not part of this PR's own diff",
				"trusted CI",
				"**Ask (maintainer):** bring the branch up to date with `main` yourself (**Update branch** on this PR, or `gh pr update-branch 7 -R acme/widget`), or ask @mallory to.",
				"If GitHub reports a conflict, merge `main` into the branch locally",
				"contributor's commits and sign-off stay",
			},
			notWant: []string{"follow the repository's process", "otherwise close it", "Workflows: write", "lacks"},
		},
		{
			name:  "base workflow change on a conflicting fork without maintainer edits",
			obs:   forkUpdateObs("conflicting"),
			block: forkUpdateBlock{WorkflowFile: forkUpdateWorkflowFile, NoMaintainerEdits: true},
			want: []string{
				"the fork does not allow edits by maintainers, so ask @mallory to bring the branch up to date with `main`",
				"turn on \"Allow edits by maintainers\" so you can",
				"also conflicts with `main`, so **Update branch** cannot do it alone",
			},
			notWant: []string{"yourself (", "If GitHub reports a conflict", "otherwise close it"},
		},
		{
			name:    "no maintainer edits, no workflow change",
			obs:     forkUpdateObs("check-failing:unit"),
			block:   forkUpdateBlock{NoMaintainerEdits: true},
			want:    []string{"comes from a fork (`mallory/widget`) that does not allow edits by maintainers", "trusted CI", "**Ask (maintainer):** ask @mallory to turn on \"Allow edits by maintainers\""},
			notWant: []string{"gh pr update-branch", ".github/workflows"},
		},
		{
			name:  "nothing read keeps the general ask",
			obs:   forkUpdateObs("check-failing:unit"),
			block: forkUpdateBlock{},
			want:  []string{"the hive cannot finish it on the fork's own branch", "**Ask (maintainer):** review the change", "otherwise close it"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := forkRehomeBlockerTextFor(tc.obs, tc.block)
			for _, s := range tc.want {
				if !strings.Contains(got, s) {
					t.Errorf("missing %q:\n%s", s, got)
				}
			}
			for _, s := range tc.notWant {
				if strings.Contains(got, s) {
					t.Errorf("unexpected %q:\n%s", s, got)
				}
			}
		})
	}
	if got, want := forkRehomeBlockerText(forkUpdateObs("x")), forkRehomeBlockerTextFor(forkUpdateObs("x"), forkUpdateBlock{}); got != want {
		t.Fatalf("forkRehomeBlockerText changed its general ask:\n%s\nwant\n%s", got, want)
	}
}

// A fork PR the hive declined to adopt because the base changed a workflow
// file parks after the 072 dwell with the update as the one action, not the
// general "bring it in or close it" ask.
func TestForkNotAdoptedForBaseWorkflowChangeNamesUpdateBranch(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	h.sink.presence = &ContributorPresence{Registered: true, Online: false}
	h.adopt.allowMaintainerEdits(7, true)
	h.adopt.compareFiles = []string{"README.md", forkUpdateWorkflowFile}
	key := stageKey("acme", "widget", 7)
	returned := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}

	h.observe(forkPR(7, "aaa"), "check-failing:unit")
	h.clock.advance(30 * time.Minute)
	h.observe(forkPR(7, "aaa", returned...), "check-failing:unit")
	if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.ForkRehomeRefused("aaa") || rec.AdoptedHead != "" {
		t.Fatalf("a fork whose update carries a workflow change must not be adopted: %+v", rec)
	}

	h.clock.advance(24 * time.Hour)
	h.observe(forkPR(7, "aaa", returned...), "check-failing:unit")
	want := stagelabels.LaneReasonAdoptionStalledPrefix + adoptStallForkRehomeRefused
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision || rec.Reason != want {
		t.Fatalf("record %+v, want 085 on %s", rec, want)
	}
	notes := stalledComments(h.api)
	if len(notes) != 1 {
		t.Fatalf("escalation note: %v", h.api.comments)
	}
	for _, s := range []string{
		"reason=" + want,
		"`v5` has changed `" + forkUpdateWorkflowFile + "` since this branch was cut",
		"not installed on the fork",
		"bring the branch up to date with `v5` yourself (**Update branch** on this PR, or `gh pr update-branch 7 -R acme/widget`), or ask @mallory to.",
	} {
		if !strings.Contains(notes[0], s) {
			t.Fatalf("note missing %q:\n%s", s, notes[0])
		}
	}
	if strings.Contains(notes[0], "follow the repository's process") {
		t.Fatalf("note kept the general ask:\n%s", notes[0])
	}
	if len(adoptionComments(h.api)) != 0 || len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
		t.Fatalf("fork PR adopted or re-homed: comments=%v pulls=%v refs=%v", h.api.comments, h.adopt.pulls, h.adopt.refs)
	}
}

// utah-packages#812's own path: a fork adopted in place while it could be
// pushed, then main changed a workflow file and the branch needed a rebase.
// The re-home is refused (fork code never moves into the base repository)
// and the 085 note names the conflict and the person's update.
func TestAdoptedForkConflictAfterBaseWorkflowChangeNamesUpdate(t *testing.T) {
	p := adoptPolicy()
	p.AdoptInPlace = true
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, p)
	defer done()
	key := adoptInPlaceAt(h, "aaa", "label:needs-rebase")
	h.adopt.compareFiles = []string{forkUpdateWorkflowFile}
	labels := append(append([]string{}, adoptedLabels...), "needs-rebase")
	h.clock.advance(time.Minute)
	h.observe(forkPR(7, "aaa", labels...), "label:needs-rebase")
	h.clock.advance(adoptedDwell)
	h.observe(forkPR(7, "aaa", labels...), "label:needs-rebase")
	if rec, _ := h.e.stageStoreOrShared().Get(key); !rec.ForkRehomeRefused("aaa") || len(h.adopt.pulls) != 0 || len(h.adopt.refs) != 0 {
		t.Fatalf("fork re-home must be refused: rec=%+v pulls=%v refs=%v", rec, h.adopt.pulls, h.adopt.refs)
	}
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(forkPR(7, "aaa", labels...), "label:needs-rebase")
	want := stagelabels.LaneReasonAdoptionStalledPrefix + adoptStallForkRehomeRefused
	if rec, _ := h.e.stageStoreOrShared().Get(key); rec.Stage != stagelabels.NeedsDecision || rec.Reason != want {
		t.Fatalf("record %+v, want 085 on %s", rec, want)
	}
	notes := stalledComments(h.api)
	if len(notes) != 1 {
		t.Fatalf("stall note: %v", h.api.comments)
	}
	for _, s := range []string{
		"`v5` has changed `" + forkUpdateWorkflowFile + "`",
		"also conflicts with `v5`, so **Update branch** cannot do it alone: merge `v5` into the branch locally, resolve the conflicts and push.",
		"`gh pr update-branch 7 -R acme/widget`",
		"trusted CI",
	} {
		if !strings.Contains(notes[0], s) {
			t.Fatalf("note missing %q:\n%s", s, notes[0])
		}
	}
}
