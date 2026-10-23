package stagelabels

import (
	"reflect"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
)

// `merge-method/merge` (mixed authorship, each commit self-signed): the
// writer reconciles it only when the pass has a verdict, and it is not a
// stage/role label a terminal pass strips.
func TestPlanMergeCommit(t *testing.T) {
	base := Desired{Stage: AdoptedByHive, Role: RoleTrusted}
	cur := []string{"stage/074-adopted-by-hive", "role/trusted", "bug"}

	wanted := base
	wanted.MergeCommit = MergeCommitWanted
	add, remove := Plan(cur, wanted)
	if !reflect.DeepEqual(add, []string{MergeCommitLabel}) || len(remove) != 0 {
		t.Fatalf("wanted: add=%v remove=%v", add, remove)
	}
	labelled := append(append([]string{}, cur...), MergeCommitLabel)
	if add, remove = Plan(labelled, wanted); len(add)+len(remove) != 0 {
		t.Fatalf("wanted idempotent: add=%v remove=%v", add, remove)
	}

	notWanted := base
	notWanted.MergeCommit = MergeCommitNotWanted
	if add, remove = Plan(labelled, notWanted); len(add) != 0 || !reflect.DeepEqual(remove, []string{MergeCommitLabel}) {
		t.Fatalf("not wanted: add=%v remove=%v", add, remove)
	}
	if add, remove = Plan(cur, notWanted); len(add)+len(remove) != 0 {
		t.Fatalf("not wanted idempotent: add=%v remove=%v", add, remove)
	}

	// Unknown (commit check not read) leaves it as found either way.
	if add, remove = Plan(labelled, base); len(add)+len(remove) != 0 {
		t.Fatalf("unknown keeps: add=%v remove=%v", add, remove)
	}
	if add, remove = Plan(cur, base); len(add)+len(remove) != 0 {
		t.Fatalf("unknown adds nothing: add=%v remove=%v", add, remove)
	}
	// Not an owned pipeline label: the terminal zero Desired does not strip it.
	if Owned(MergeCommitLabel) {
		t.Fatal("merge-method/merge must not be Owned")
	}
	if add, remove = Plan([]string{MergeCommitLabel, "stage/074-adopted-by-hive"}, Desired{}); len(add) != 0 || !reflect.DeepEqual(remove, []string{"stage/074-adopted-by-hive"}) {
		t.Fatalf("terminal: add=%v remove=%v", add, remove)
	}
}

func TestHasMergeCommitLabelAndDef(t *testing.T) {
	if HasMergeCommitLabel([]string{"bug", "role/trusted"}) {
		t.Fatal("false positive")
	}
	if !HasMergeCommitLabel([]string{"bug", " " + MergeCommitLabel + " "}) {
		t.Fatal("missed trimmed label")
	}
	var found bool
	for _, d := range LabelDefsFor(authorkind.Names{}) {
		if d.Name == MergeCommitLabel {
			found = true
			if d.Color != MergeCommitColor || d.Description == "" {
				t.Fatalf("def: %+v", d)
			}
		}
	}
	if !found {
		t.Fatal("merge-method/merge not provisioned")
	}
}

func TestStoreMergeCommitNoted(t *testing.T) {
	s := NewStore("", func() time.Time { return time.Unix(1_700_000_000, 0) })
	s.Observe("k", AdoptedByHive, "r")
	if rec, _ := s.Get("k"); rec.MergeCommitNoted("a1899cc") {
		t.Fatalf("noted before mark: %+v", rec)
	}
	s.MarkMergeCommitNoted("k", "")
	s.MarkMergeCommitNoted("k", "a1899cc")
	rec, _ := s.Get("k")
	if !rec.MergeCommitNoted("a1899cc") || rec.MergeCommitNoted("bbb") || rec.MergeCommitNoted("") {
		t.Fatalf("after mark: %+v", rec)
	}
	s.MarkMergeCommitNoted("missing", "a1899cc")
	if _, ok := s.Get("missing"); ok {
		t.Fatal("mark created a record")
	}
}
