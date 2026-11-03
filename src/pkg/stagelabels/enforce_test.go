package stagelabels

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
)

// fakeLabeler is a forge item: truth is what a fresh read returns; writes
// mutate it so a second Enforce sees the result of the first.
type fakeLabeler struct {
	truth   []string
	reads   int
	added   []string
	removed []string
	failRm  string
}

func (f *fakeLabeler) Labels(context.Context, string, string, int) ([]string, error) {
	f.reads++
	return append([]string(nil), f.truth...), nil
}

func (f *fakeLabeler) AddLabels(_ context.Context, _, _ string, _ int, labels []string) error {
	f.added = append(f.added, labels...)
	f.truth = append(f.truth, labels...)
	return nil
}

func (f *fakeLabeler) RemoveLabel(_ context.Context, _, _ string, _ int, label string) error {
	if label == f.failRm {
		return errors.New("boom")
	}
	f.removed = append(f.removed, label)
	var keep []string
	for _, l := range f.truth {
		if l != label {
			keep = append(keep, l)
		}
	}
	f.truth = keep
	return nil
}

func TestIsStageLabel(t *testing.T) {
	for label, want := range map[string]bool{
		"stage/010-actionable": true, "stage/999-unknown": true, Actionable.Label(): true,
		StalledLabel: false, RegressedLabel: false, "stage/": false, "stage/01-x": false,
		"role/trusted": false, "claimed": false, "stage/0a0-x": false,
	} {
		if got := IsStageLabel(label); got != want {
			t.Errorf("IsStageLabel(%q) = %v, want %v", label, got, want)
		}
	}
	got := StageLabels([]string{"bug", Actionable.Label(), StalledLabel, Claimed.Label()})
	if !reflect.DeepEqual(got, []string{Actionable.Label(), Claimed.Label()}) {
		t.Fatalf("StageLabels = %v", got)
	}
}

// hivecommons/hive#12512: the item carries two stage labels. Whatever the
// caller's snapshot said, the write leaves exactly the wanted one.
func TestEnforce_TwoStageLabels(t *testing.T) {
	f := &fakeLabeler{truth: []string{"bug", Actionable.Label(), Claimed.Label(), "hive/claimed-by-ann"}}
	// Snapshot predates the claim hook's add: 010 only, and 020 is wanted.
	out, err := Enforce(context.Background(), f, IssueItem, "acme", "widgets", 12, []string{"bug", Actionable.Label()}, Desired{Stage: Claimed})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Healed || !out.Reread || f.reads != 1 {
		t.Fatalf("outcome %+v reads=%d: the fresh read must be taken and the duplicate noticed", out, f.reads)
	}
	if !reflect.DeepEqual(f.removed, []string{Actionable.Label()}) || len(f.added) != 0 {
		t.Fatalf("removed %v added %v: plan from truth, not the snapshot (020 was already there)", f.removed, f.added)
	}
	if got := StageLabels(f.truth); !reflect.DeepEqual(got, []string{Claimed.Label()}) {
		t.Fatalf("item carries %v, want exactly 020", got)
	}
	// Idempotent: a snapshot that already matches costs no read.
	out, err = Enforce(context.Background(), f, IssueItem, "acme", "widgets", 12, f.truth, Desired{Stage: Claimed})
	if err != nil || out.Changed() || out.Reread || f.reads != 1 {
		t.Fatalf("second pass: out=%+v err=%v reads=%d", out, err, f.reads)
	}
}

func TestEnforce_RemoveFailureContinues(t *testing.T) {
	f := &fakeLabeler{truth: []string{Actionable.Label(), Claimed.Label(), PRInProgress.Label()}, failRm: Claimed.Label()}
	out, err := Enforce(context.Background(), f, IssueItem, "acme", "widgets", 1, f.truth, Desired{Stage: PRInProgress})
	if err == nil {
		t.Fatal("the failed remove must surface")
	}
	if !reflect.DeepEqual(out.Removed, []string{Actionable.Label()}) {
		t.Fatalf("the other remove must still happen: %+v", out)
	}
	if _, err := Enforce(context.Background(), nil, IssueItem, "acme", "widgets", 1, nil, Desired{}); err == nil {
		t.Fatal("nil labeler must error")
	}
}

// The claim hooks know only the stage: author tier, health and the human's
// labels survive; every other stage and the retired labels come off.
func TestEnforceStage_KeepsAuthorAndHealth(t *testing.T) {
	f := &fakeLabeler{truth: []string{"bug", "claimed", Actionable.Label(), StalledLabel, RoleContributor.Label(), "hive/claimed-by-ann"}}
	out, err := EnforceStage(context.Background(), f, IssueItem, "acme", "widgets", 7, Claimed)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(f.removed)
	if !reflect.DeepEqual(f.removed, []string{"claimed", Actionable.Label()}) || !reflect.DeepEqual(f.added, []string{Claimed.Label()}) {
		t.Fatalf("removed %v added %v (out %+v)", f.removed, f.added, out)
	}
	sort.Strings(f.truth)
	if !reflect.DeepEqual(f.truth, []string{"bug", "hive/claimed-by-ann", RoleContributor.Label(), Claimed.Label(), StalledLabel}) {
		t.Fatalf("item carries %v", f.truth)
	}
	// Already right: no writes.
	if out, err := EnforceStage(context.Background(), f, IssueItem, "acme", "widgets", 7, Claimed); err != nil || out.Changed() {
		t.Fatalf("idempotent pass wrote %+v (%v)", out, err)
	}
	// None clears the stage but nothing else hive-owned.
	if _, err := EnforceStage(context.Background(), f, IssueItem, "acme", "widgets", 7, None); err != nil {
		t.Fatal(err)
	}
	if got := StageLabels(f.truth); len(got) != 0 || RoleOf(f.truth) != RoleContributor {
		t.Fatalf("after None: %v", f.truth)
	}
}

// hivecommons/hive#12451: the stage table says which kind of item a stage
// may be written to. 050+ are PR stages; an issue asked to carry one gets
// 040-covered-by-pr (the PR exists — that is all the issue says), and an
// issue stage asked of a PR means nothing and clears the stage. No stage is
// shared: an issue parked on a person wears 045-needs-decision, a PR parked
// on a decision wears 085-needs-decision, and 085 asked of an issue clamps
// to 045 so the park survives on the issue's own stage.
func TestStageForItem(t *testing.T) {
	for _, d := range Table {
		if !d.Issue && !d.PR {
			t.Fatalf("%s: a stage is an issue stage, a PR stage or both, never neither", d.Stage)
		}
		if d.Issue && d.PR {
			t.Fatalf("%s: issue and PR stages are disjoint", d.Stage)
		}
		if d.Issue && d.Number > IssueNeedsDecision.Number() {
			t.Fatalf("%s: an issue's own pipeline ends at 045-needs-decision", d.Stage)
		}
		if d.PR && d.Number <= IssueNeedsDecision.Number() {
			t.Fatalf("%s: PR stages start after 045", d.Stage)
		}
		wantIssue := CoveredByPR
		if d.Stage == NeedsDecision {
			wantIssue = IssueNeedsDecision
		}
		if got := d.Stage.For(IssueItem); d.Issue && got != d.Stage || !d.Issue && got != wantIssue {
			t.Errorf("%s.For(issue) = %s", d.Stage, got)
		}
		if got := d.Stage.For(PRItem); d.PR && got != d.Stage || !d.PR && got != None {
			t.Errorf("%s.For(pr) = %s", d.Stage, got)
		}
	}
	if NeedsDecision.For(IssueItem) != IssueNeedsDecision || ReadyForHumanMerge.For(IssueItem) != CoveredByPR {
		t.Fatal("085 asked of an issue is 045; 090 asked of an issue is still just a covering PR")
	}
	if IssueNeedsDecision.For(PRItem) != None || IssueNeedsDecision.Applies(PRItem) || NeedsDecision.Applies(IssueItem) {
		t.Fatal("045 is issue-only and 085 is PR-only")
	}
	if !None.Applies(IssueItem) || !None.Applies(PRItem) || None.For(PRItem) != None {
		t.Fatal("None applies everywhere")
	}
	if Stage(999).Applies(IssueItem) || Stage(999).For(IssueItem) != None {
		t.Fatal("an unknown stage applies nowhere")
	}
	if IssueItem.String() != "issue" || PRItem.String() != "pr" {
		t.Fatal("Item.String")
	}
}

// The three specimens on hivecommons/hive#12451: an issue carrying 010 + 040
// + 050 (050 mirrored from its PR) is written exactly 040 whatever the
// caller asked for; a PR can never be given an issue stage.
func TestEnforce_ClampsStageToItemKind(t *testing.T) {
	f := &fakeLabeler{truth: []string{"help wanted", Actionable.Label(), CoveredByPR.Label(), ReviewPending.Label()}}
	out, err := Enforce(context.Background(), f, IssueItem, "acme", "widgets", 12451, f.truth, Desired{Stage: ReviewPending})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Clamped || out.Wanted != ReviewPending || out.Stage != CoveredByPR || !out.Healed {
		t.Fatalf("outcome %+v: 050 must clamp to 040 on an issue", out)
	}
	if got := StageLabels(f.truth); !reflect.DeepEqual(got, []string{CoveredByPR.Label()}) {
		t.Fatalf("issue carries %v, want exactly 040", got)
	}
	sort.Strings(f.removed)
	if !reflect.DeepEqual(f.removed, []string{Actionable.Label(), ReviewPending.Label()}) || len(f.added) != 0 {
		t.Fatalf("removed %v added %v", f.removed, f.added)
	}

	// A PR carrying 100 asked for an issue stage: nothing issue-side lands,
	// the PR stage is cleared, the author tier survives.
	pr := &fakeLabeler{truth: []string{MergeQueue.Label(), RoleTrusted.Label()}}
	out, err = EnforceStage(context.Background(), pr, PRItem, "acme", "widgets", 7, Claimed)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Clamped || out.Stage != None || len(StageLabels(pr.truth)) != 0 || RoleOf(pr.truth) != RoleTrusted {
		t.Fatalf("outcome %+v truth %v", out, pr.truth)
	}
	if len(pr.added) != 0 {
		t.Fatalf("a PR must never be given %v", pr.added)
	}

	// An applicable stage is not clamped and the outcome says so.
	ok := &fakeLabeler{truth: []string{ReviewPending.Label()}}
	out, err = Enforce(context.Background(), ok, PRItem, "acme", "widgets", 8, ok.truth, Desired{Stage: Reviewed})
	if err != nil || out.Clamped || out.Stage != Reviewed || out.Wanted != Reviewed {
		t.Fatalf("outcome %+v err %v", out, err)
	}
}

// EnforceStageAdding puts the caller's own labels on in the same add as the
// stage, skips ones already present and never takes a stage label from
// extra.
func TestEnforceStageAdding_ExtraLabelsInOneAdd(t *testing.T) {
	l := &fakeLabeler{truth: []string{"bug", "stage/010-actionable", "hive/claimed-by-reviewer"}}
	out, err := EnforceStageAdding(context.Background(), l, IssueItem, "o", "r", 1, PRInProgress, "hive/claimed-by-scanner", "hive/claimed-by-reviewer", "stage/100-merge-queue", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"stage/030-pr-in-progress", "hive/claimed-by-scanner"}; !reflect.DeepEqual(out.Added, want) {
		t.Fatalf("added = %v, want %v", out.Added, want)
	}
	if want := []string{"stage/010-actionable"}; !reflect.DeepEqual(out.Removed, want) {
		t.Fatalf("removed = %v, want %v", out.Removed, want)
	}
	if want := []string{"bug", "hive/claimed-by-reviewer", "stage/030-pr-in-progress", "hive/claimed-by-scanner"}; !reflect.DeepEqual(l.truth, want) {
		t.Fatalf("truth = %v, want %v", l.truth, want)
	}
}

// A claim hook that re-writes an item still carrying the pre-#12581
// author/<tier> spelling carries the tier over as role/<tier> and leaves the
// author/<kind> label it has no opinion about.
func TestEnforceStage_MigratesLegacyRoleKeepsKind(t *testing.T) {
	f := &fakeLabeler{truth: []string{"author/trusted", "author/clanker", Actionable.Label()}}
	if _, err := EnforceStage(context.Background(), f, IssueItem, "acme", "widgets", 7, Claimed); err != nil {
		t.Fatal(err)
	}
	sort.Strings(f.truth)
	if !reflect.DeepEqual(f.truth, []string{"author/clanker", "role/trusted", Claimed.Label()}) {
		t.Fatalf("item carries %v", f.truth)
	}
	if RoleOf([]string{"author/contributor"}) != RoleContributor || RoleOf([]string{"author/contributor", "role/trusted"}) != RoleTrusted {
		t.Fatal("RoleOf must read the legacy spelling only when no role/* is present")
	}
}

// Defence in depth for hivecommons/hive#12994: a pull request that somehow
// wears an issue-family stage (an out-of-cycle issue writer named the PR's
// number) loses it on the next PR-side write — every stage/* label that is
// not the decision comes off, whatever family it belongs to — and the
// double pill counts as a heal. The mirror holds for an issue wearing a PR
// stage.
func TestEnforce_StripsForeignFamilyStage(t *testing.T) {
	f := &fakeLabeler{truth: []string{"size/L", PRInProgress.Label(), "hive/claimed-by-scanner", ReviewPending.Label()}}
	out, err := Enforce(context.Background(), f, PRItem, "acme", "widgets", 12994, f.truth, Desired{Stage: ReviewPending})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.Removed, []string{PRInProgress.Label()}) || len(out.Added) != 0 || !out.Healed {
		t.Fatalf("PR wearing an issue stage: out=%+v", out)
	}
	if !reflect.DeepEqual(f.truth, []string{"size/L", "hive/claimed-by-scanner", ReviewPending.Label()}) {
		t.Fatalf("labels after = %v", f.truth)
	}

	f = &fakeLabeler{truth: []string{"bug", ReviewPending.Label()}}
	out, err = Enforce(context.Background(), f, IssueItem, "acme", "widgets", 7, f.truth, Desired{Stage: Actionable})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out.Removed, []string{ReviewPending.Label()}) || !reflect.DeepEqual(out.Added, []string{Actionable.Label()}) {
		t.Fatalf("issue wearing a PR stage: out=%+v", out)
	}
}
