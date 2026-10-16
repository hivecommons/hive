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
		"author/trusted": false, "claimed": false, "stage/0a0-x": false,
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
	out, err := Enforce(context.Background(), f, "acme", "widgets", 12, []string{"bug", Actionable.Label()}, Desired{Stage: Claimed})
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
	out, err = Enforce(context.Background(), f, "acme", "widgets", 12, f.truth, Desired{Stage: Claimed})
	if err != nil || out.Changed() || out.Reread || f.reads != 1 {
		t.Fatalf("second pass: out=%+v err=%v reads=%d", out, err, f.reads)
	}
}

func TestEnforce_RemoveFailureContinues(t *testing.T) {
	f := &fakeLabeler{truth: []string{Actionable.Label(), Claimed.Label(), PRInProgress.Label()}, failRm: Claimed.Label()}
	out, err := Enforce(context.Background(), f, "acme", "widgets", 1, f.truth, Desired{Stage: PRInProgress})
	if err == nil {
		t.Fatal("the failed remove must surface")
	}
	if !reflect.DeepEqual(out.Removed, []string{Actionable.Label()}) {
		t.Fatalf("the other remove must still happen: %+v", out)
	}
	if _, err := Enforce(context.Background(), nil, "acme", "widgets", 1, nil, Desired{}); err == nil {
		t.Fatal("nil labeler must error")
	}
}

// The claim hooks know only the stage: author tier, health and the human's
// labels survive; every other stage and the retired labels come off.
func TestEnforceStage_KeepsAuthorAndHealth(t *testing.T) {
	f := &fakeLabeler{truth: []string{"bug", "claimed", Actionable.Label(), StalledLabel, AuthorContributor.Label(), "hive/claimed-by-ann"}}
	out, err := EnforceStage(context.Background(), f, "acme", "widgets", 7, Claimed)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(f.removed)
	if !reflect.DeepEqual(f.removed, []string{"claimed", Actionable.Label()}) || !reflect.DeepEqual(f.added, []string{Claimed.Label()}) {
		t.Fatalf("removed %v added %v (out %+v)", f.removed, f.added, out)
	}
	sort.Strings(f.truth)
	if !reflect.DeepEqual(f.truth, []string{AuthorContributor.Label(), "bug", "hive/claimed-by-ann", Claimed.Label(), StalledLabel}) {
		t.Fatalf("item carries %v", f.truth)
	}
	// Already right: no writes.
	if out, err := EnforceStage(context.Background(), f, "acme", "widgets", 7, Claimed); err != nil || out.Changed() {
		t.Fatalf("idempotent pass wrote %+v (%v)", out, err)
	}
	// None clears the stage but nothing else hive-owned.
	if _, err := EnforceStage(context.Background(), f, "acme", "widgets", 7, None); err != nil {
		t.Fatal(err)
	}
	if got := StageLabels(f.truth); len(got) != 0 || AuthorOf(f.truth) != AuthorContributor {
		t.Fatalf("after None: %v", f.truth)
	}
}
