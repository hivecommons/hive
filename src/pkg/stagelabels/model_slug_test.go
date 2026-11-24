package stagelabels

import (
	"context"
	"reflect"
	"testing"
)

// hivecommons/hive#19611: a workflow model may spell a stage's label in its
// own slug, in the stage's number band.

func TestValidateModelLabel(t *testing.T) {
	for _, tc := range []struct {
		n     int
		label string
		ok    bool
	}{
		{20, "stage/020-accepted", true},
		{50, "stage/050-reviewer-check", true},
		{100, "stage/100-ship-it", true},
		{20, "stage/020-claimed", true}, // the built-in spelling is allowed, if pointless
		{20, "stage/030-accepted", false},
		{20, "stage/20-accepted", false},
		{20, "stage/020-", false},
		{20, "stage/020-Accepted", false},
		{20, "stage/020-accepted-", false},
		{20, "stage/020--accepted", false},
		{20, "stage/020-acc_pted", false},
		{20, "triage/accepted", false},
		{21, "stage/021-accepted", false},    // not a table number
		{0, "stage/000-nothing", false},      // unnumbered
		{90, "stage/090-needs-human", false}, // retired spelling
	} {
		err := ValidateModelLabel(tc.n, tc.label)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateModelLabel(%d, %q) = %v, want ok=%v", tc.n, tc.label, err, tc.ok)
		}
	}
}

func TestLabelAs(t *testing.T) {
	for _, tc := range []struct {
		stage    Stage
		override string
		want     string
	}{
		{Claimed, "", Claimed.Label()},
		{Claimed, "stage/020-accepted", "stage/020-accepted"},
		{Claimed, " Stage/020-Accepted ", "stage/020-accepted"},
		// A mismatched band (a stage clamped to another kind) falls back.
		{Actionable, "stage/020-accepted", Actionable.Label()},
		{ReadyForHumanMerge, "stage/090-needs-human", ReadyForHumanMerge.Label()},
		{None, "stage/020-accepted", ""},
	} {
		if got := tc.stage.LabelAs(tc.override); got != tc.want {
			t.Errorf("%v.LabelAs(%q) = %q, want %q", tc.stage, tc.override, got, tc.want)
		}
	}
}

// A model slug reads as the built-in stage at its number, so the store,
// the tracker and every number-keyed reader see the same stage whichever
// spelling the item wears.
func TestCurrentReadsModelSlugByNumber(t *testing.T) {
	for labels, want := range map[string]Stage{
		"stage/020-accepted":           Claimed,
		"stage/070-drafting":           Fixing,
		"stage/050-reviewer-check":     ReviewPending,
		"stage/060-stakeholder-review": Reviewed,
		"Stage/020-Accepted":           Claimed,
		"stage/021-accepted":           None,
		"stage/999-unknown":            None,
		StalledLabel:                   None,
		"stage/090-needs-human":        ReadyForHumanMerge,
	} {
		if got := Current([]string{"bug", labels}); got != want {
			t.Errorf("Current(%q) = %v, want %v", labels, got, want)
		}
	}
	if got := Current([]string{"stage/020-accepted", CoveredByPR.Label()}); got != CoveredByPR {
		t.Fatalf("furthest along must still win: %v", got)
	}
}

// The writer swaps the built-in spelling for the model's (and back when the
// model stops declaring one), never leaving two stage labels.
func TestPlanModelStageLabel(t *testing.T) {
	want := Desired{Stage: Claimed, StageLabel: "stage/020-accepted"}
	add, remove := Plan([]string{"bug", Claimed.Label()}, want)
	if !reflect.DeepEqual(add, []string{"stage/020-accepted"}) || !reflect.DeepEqual(remove, []string{Claimed.Label()}) {
		t.Fatalf("swap to model slug: add=%v remove=%v", add, remove)
	}
	if add, remove := Plan([]string{"bug", "stage/020-accepted"}, want); len(add)+len(remove) != 0 {
		t.Fatalf("model slug already worn: add=%v remove=%v", add, remove)
	}
	add, remove = Plan([]string{"bug", "stage/020-accepted"}, Desired{Stage: Claimed})
	if !reflect.DeepEqual(add, []string{Claimed.Label()}) || !reflect.DeepEqual(remove, []string{"stage/020-accepted"}) {
		t.Fatalf("swap back to built-in: add=%v remove=%v", add, remove)
	}
	add, remove = Plan([]string{"stage/020-accepted"}, Desired{Stage: PRInProgress, StageLabel: "stage/020-accepted"})
	if !reflect.DeepEqual(add, []string{PRInProgress.Label()}) || !reflect.DeepEqual(remove, []string{"stage/020-accepted"}) {
		t.Fatalf("a slug outside the stage's band is ignored: add=%v remove=%v", add, remove)
	}
}

// A claim hook only knows the stage: it keeps the model's spelling of that
// stage instead of swapping in the built-in one the model's writer would
// swap straight back, but still replaces it when the stage changes.
func TestEnforceStageKeepsModelSlug(t *testing.T) {
	f := &fakeLabeler{truth: []string{"bug", "stage/020-accepted"}}
	out, err := EnforceStage(context.Background(), f, IssueItem, "acme", "widgets", 7, Claimed)
	if err != nil || out.Changed() {
		t.Fatalf("same stage must not churn the model slug: out=%+v err=%v", out, err)
	}
	if _, err := EnforceStage(context.Background(), f, IssueItem, "acme", "widgets", 7, PRInProgress); err != nil {
		t.Fatal(err)
	}
	if got := StageLabels(f.truth); !reflect.DeepEqual(got, []string{PRInProgress.Label()}) {
		t.Fatalf("stage change must replace the slug: %v", got)
	}
	// A retired spelling of the same stage is still migrated.
	f = &fakeLabeler{truth: []string{"stage/090-needs-human"}}
	if _, err := EnforceStage(context.Background(), f, PRItem, "acme", "widgets", 8, ReadyForHumanMerge); err != nil {
		t.Fatal(err)
	}
	if got := StageLabels(f.truth); !reflect.DeepEqual(got, []string{ReadyForHumanMerge.Label()}) {
		t.Fatalf("retired spelling kept: %v", got)
	}
}
