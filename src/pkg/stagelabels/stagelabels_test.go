package stagelabels

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
)

func TestTableIsOrderedSparseAndUnique(t *testing.T) {
	seenNum := map[int]bool{}
	seenSlug := map[string]bool{}
	prev := 0
	for _, d := range Table {
		if d.Number <= prev {
			t.Fatalf("table not strictly increasing at %s (%d after %d)", d.Slug, d.Number, prev)
		}
		if seenNum[d.Number] || seenSlug[d.Slug] {
			t.Fatalf("duplicate number/slug %d/%s", d.Number, d.Slug)
		}
		seenNum[d.Number], seenSlug[d.Slug] = true, true
		prev = d.Number
		if !d.Issue && !d.PR {
			t.Fatalf("%s is emitted for neither issues nor PRs", d.Slug)
		}
		if !strings.HasPrefix(d.Stage.Label(), Prefix) {
			t.Fatalf("label %q lacks prefix", d.Stage.Label())
		}
	}
	want := map[Stage]string{
		Actionable: "stage/010-actionable", Claimed: "stage/020-claimed", PRInProgress: "stage/030-pr-in-progress",
		CoveredByPR: "stage/040-covered-by-pr", ReviewPending: "stage/050-review-pending", Reviewed: "stage/060-reviewed",
		AwaitingMergeAdmission: "stage/080-awaiting-merge-admission",
		Fixing:                 "stage/070-fixing", ReturnedToContributor: "stage/072-returned-to-contributor", AdoptedByHive: "stage/074-adopted-by-hive",
		MergeQueue: "stage/100-merge-queue", NeedsHuman: "stage/090-needs-human",
	}
	for s, l := range want {
		if s.Label() != l {
			t.Errorf("%v label = %q want %q", s, s.Label(), l)
		}
	}
	if None.Label() != "" || None.String() != "none" {
		t.Fatalf("None renders %q/%q", None.Label(), None.String())
	}
	if !Fixing.Before(MergeQueue) || MergeQueue.Before(Fixing) {
		t.Fatal("Before() disagrees with table order")
	}
	if !Reviewed.Before(AwaitingMergeAdmission) || !AwaitingMergeAdmission.Before(NeedsHuman) || !AwaitingMergeAdmission.StallNote() || MergeQueue.StallNote() {
		t.Fatal("awaiting-merge-admission must sit between reviewed and needs-human and be the only stall-note stage")
	}
	if got := StallComment(AwaitingMergeAdmission, 30*time.Minute, "review-state-check"); got != StallCommentMarker+"\nstage 080-awaiting-merge-admission stalled after 30m0s: review-state-check" {
		t.Fatalf("StallComment = %q", got)
	}
}

func TestAllLabelDefsColours(t *testing.T) {
	for _, d := range AllLabelDefs() {
		switch {
		case strings.HasPrefix(d.Name, Prefix) && d.Name != StalledLabel && d.Name != RegressedLabel:
			if d.Color != StageColor {
				t.Errorf("%s colour %s want %s", d.Name, d.Color, StageColor)
			}
		case d.Name == StalledLabel:
			if d.Color != StalledColor {
				t.Errorf("stalled colour %s", d.Color)
			}
		case d.Name == RegressedLabel:
			if d.Color != RegressedColor {
				t.Errorf("regressed colour %s", d.Color)
			}
		case d.Name == AuthorTrusted.Label():
			if d.Color != AuthorTrustedColor {
				t.Errorf("author/trusted colour %s", d.Color)
			}
		case strings.HasPrefix(d.Name, AuthorPrefix):
			if d.Color != AuthorColor {
				t.Errorf("%s colour %s", d.Name, d.Color)
			}
		default:
			t.Errorf("unexpected def %s", d.Name)
		}
	}
}

func TestAuthorFromTier(t *testing.T) {
	req := map[string]bool{"trusted": true, "merger": true}
	cases := map[string]Author{"trusted": AuthorTrusted, "Merger": AuthorTrusted, "contributor": AuthorContributor, "newcomer": AuthorNewcomer, "revoked": AuthorUntrusted, "": AuthorUntrusted}
	for tier, want := range cases {
		if got := AuthorFromTier(tier, req); got != want {
			t.Errorf("tier %q → %v want %v", tier, got, want)
		}
	}
}

func TestPlanIdempotentExclusiveAndStaleRemoval(t *testing.T) {
	want := Desired{Stage: ReviewPending, Author: AuthorContributor}
	add, remove := Plan([]string{"bug", "stage/050-review-pending", "author/contributor"}, want)
	if len(add)+len(remove) != 0 {
		t.Fatalf("unchanged state planned writes add=%v remove=%v", add, remove)
	}
	add, remove = Plan([]string{"bug", "stage/070-fixing", "stage/99-old", "author/untrusted", "🐝 covered by PR", "claimed", "hive/in-progress", "hive/claimed-by-bob"}, want)
	if !reflect.DeepEqual(add, []string{"author/contributor", "stage/050-review-pending"}) {
		t.Fatalf("add = %v", add)
	}
	if !reflect.DeepEqual(remove, []string{"author/untrusted", "claimed", "hive/in-progress", "stage/070-fixing", "stage/99-old", "🐝 covered by PR"}) {
		t.Fatalf("remove = %v (human labels and hive/claimed-by-* must survive)", remove)
	}
	add, remove = Plan([]string{"stage/100-merge-queue", "stage/stalled", "author/trusted"}, Desired{})
	if len(add) != 0 || !reflect.DeepEqual(remove, []string{"author/trusted", "stage/100-merge-queue", "stage/stalled"}) {
		t.Fatalf("terminal plan add=%v remove=%v", add, remove)
	}
	add, _ = Plan(nil, Desired{Stage: Fixing, Stalled: true, Regressed: true})
	if !reflect.DeepEqual(add, []string{"stage/070-fixing", "stage/regressed"}) {
		t.Fatalf("regressed must beat stalled: %v", add)
	}
}

func TestStoreObserveRegressRecoverAndStalled(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), clock)
	key := Key("o/r", 7)

	tr := st.Observe(key, ReviewPending, "contributor-lane:no-verdict")
	if !tr.Changed || tr.Regressed {
		t.Fatalf("first observe = %+v", tr)
	}
	rec, _ := st.Get(key)
	if Stalled(rec, nil, now.Add(59*time.Minute)) || !Stalled(rec, nil, now.Add(time.Hour)) {
		t.Fatal("stalled threshold at 1h not honoured")
	}
	if Stalled(rec, map[string]time.Duration{"review-pending": 0}, now.Add(48*time.Hour)) {
		t.Fatal("dwell override 0 must mean never")
	}
	now = now.Add(2 * time.Hour)
	if tr = st.Observe(key, ReviewPending, "contributor-lane:no-verdict"); tr.Changed {
		t.Fatal("same stage must not re-enter")
	}
	tr = st.Observe(key, MergeQueue, "contributor-lane:soaking")
	rec, _ = st.Get(key)
	if !tr.Changed || !rec.EnteredAt.Equal(now) || Stalled(rec, nil, now) {
		t.Fatalf("advance: tr=%+v rec=%+v", tr, rec)
	}
	tr = st.Observe(key, Reviewed, "head-changed")
	if !tr.Regressed || tr.Prev != MergeQueue || tr.Next != Reviewed {
		t.Fatalf("regression not detected: %+v", tr)
	}
	rec, _ = st.Get(key)
	if rec.RegressedFrom != MergeQueue || rec.RegressionCommented {
		t.Fatalf("record after regression %+v", rec)
	}
	st.MarkRegressionCommented(key)
	if tr = st.Observe(key, Reviewed, "head-changed"); tr.Regressed || tr.Changed {
		t.Fatalf("repeat observation re-flagged: %+v", tr)
	}
	if rec, _ = st.Get(key); !rec.RegressionCommented {
		t.Fatal("commented flag lost on same-stage observe")
	}
	// A deeper fall keeps the original high-water mark.
	tr = st.Observe(key, ReviewPending, "x")
	rec, _ = st.Get(key)
	if !tr.Regressed || rec.RegressedFrom != MergeQueue || rec.RegressionCommented {
		t.Fatalf("second fall: tr=%+v rec=%+v", tr, rec)
	}
	// Red CI then a fixed head back to review is the repair route: the
	// move to fixing regresses, leaving fixing for review does not.
	if tr = st.Observe(key, Fixing, "check-failing:unit"); tr.Regressed || tr.Recovered {
		t.Fatalf("50→70 is forward: %+v", tr)
	}
	if tr = st.Observe(key, ReviewPending, "check-pending"); tr.Regressed || tr.Recovered {
		t.Fatalf("fixing→review-pending must not regress: %+v", tr)
	}
	if !IsRegression(MergeQueue, Fixing) || IsRegression(Fixing, Reviewed) || !IsRegression(Fixing, Claimed) {
		t.Fatal("IsRegression repair-route exception wrong")
	}
	if tr = st.Observe(key, Reviewed, "y"); tr.Recovered {
		t.Fatal("not yet back to the regressed-from stage")
	}
	tr = st.Observe(key, MergeQueue, "z")
	rec, _ = st.Get(key)
	if !tr.Recovered || rec.Regressed() {
		t.Fatalf("recovery: tr=%+v rec=%+v", tr, rec)
	}
	tr = st.Observe(key, None, "")
	if _, ok := st.Get(key); ok || tr.Prev != MergeQueue {
		t.Fatalf("terminal must drop record: %+v", tr)
	}
	if got := RegressionComment(MergeQueue, Fixing, "check-failing:unit"); got != RegressionCommentMarker+"\nstage 100-merge-queue → 070-fixing: check-failing:unit" {
		t.Fatalf("comment = %q", got)
	}
}

func TestStorePersistsAndPrunes(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "s.json")
	st := NewStore(path, func() time.Time { return now })
	st.Observe(Key("o/r", 1), Fixing, "r")
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	later := now.Add(30 * 24 * time.Hour)
	st2 := NewStore(path, func() time.Time { return later })
	if err := st2.Load(); err != nil {
		t.Fatal(err)
	}
	rec, ok := st2.Get(Key("o/r", 1))
	if !ok || rec.Stage != Fixing || !rec.EnteredAt.Equal(now) {
		t.Fatalf("reload = %+v %v", rec, ok)
	}
	if n := st2.Prune(14 * 24 * time.Hour); n != 1 || len(st2.Keys()) != 0 {
		t.Fatalf("prune = %d keys=%v", n, st2.Keys())
	}
}

func TestLookupsAndAuthorStrings(t *testing.T) {
	if s, ok := StageBySlug(" Merge-Queue "); !ok || s != MergeQueue {
		t.Fatalf("StageBySlug = %v %v", s, ok)
	}
	if _, ok := StageBySlug("nope"); ok {
		t.Fatal("unknown slug resolved")
	}
	if !MergeQueue.Known() || None.Known() || Stage(99).Known() {
		t.Fatal("Known() wrong")
	}
	if Current([]string{"bug", "stage/stalled", "stage/999-old", MergeQueue.Label()}) != MergeQueue || Current([]string{"stage/999-old"}) != None {
		t.Fatal("Current() wrong")
	}
	if AuthorTrusted.String() != "trusted" || AuthorTrusted.Label() != "author/trusted" || AuthorNone.String() != "" || AuthorNone.Label() != "" {
		t.Fatalf("author strings: %q %q %q %q", AuthorTrusted.String(), AuthorTrusted.Label(), AuthorNone.String(), AuthorNone.Label())
	}
	if got := RegressionComment(MergeQueue, Fixing, "  "); !strings.HasSuffix(got, ": state changed") {
		t.Fatalf("empty reason default: %q", got)
	}
	if got := StallComment(AwaitingMergeAdmission, time.Minute, ""); !strings.HasSuffix(got, ": lane has not evaluated this head") {
		t.Fatalf("empty stall reason default: %q", got)
	}
	if Stalled(Record{}, nil, time.Now()) || Stalled(Record{Stage: NeedsHuman, EnteredAt: time.Unix(0, 0)}, nil, time.Now()) {
		t.Fatal("None / open-ended stages never stall")
	}
}

func TestStoreMarksForgetsAndSharedLoad(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "s.json")
	st := NewStore(path, func() time.Time { return now })
	key := Key("O/R", 3)
	if key != "o/r#3" {
		t.Fatalf("Key = %q", key)
	}
	st.MarkStallCommented(key) // unknown key: no-op
	st.MarkRegressionCommented(key)
	st.Observe(key, AwaitingMergeAdmission, "x")
	st.MarkStallCommented(key)
	st.MarkStallCommented(key)
	if rec, _ := st.Get(key); !rec.StallCommented {
		t.Fatal("stall comment not recorded")
	}
	st.Observe(key, MergeQueue, "y")
	if rec, _ := st.Get(key); rec.StallCommented {
		t.Fatal("stage change must reset the stall note")
	}
	st.Forget(key)
	st.Forget(key)
	if _, ok := st.Get(key); ok {
		t.Fatal("Forget left the record")
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(); err != nil { // clean: no-op
		t.Fatal(err)
	}
	if err := NewStore("", nil).Load(); err != nil {
		t.Fatal(err)
	}
	if err := NewStore("", nil).Save(); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(filepath.Join(t.TempDir(), "missing.json"), nil).Load(); err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewStore(bad, nil).Load(); err == nil {
		t.Fatal("corrupt file must error")
	}
	if err := os.WriteFile(bad, []byte("null"), 0o644); err != nil {
		t.Fatal(err)
	}
	nullStore := NewStore(bad, nil)
	if err := nullStore.Load(); err != nil || len(nullStore.Keys()) != 0 {
		t.Fatalf("null file: %v %v", err, nullStore.Keys())
	}

	saved := StatePath
	t.Cleanup(func() { StatePath = saved })
	StatePath = filepath.Join(t.TempDir(), "shared.json")
	sharedOnce = sync.Once{}
	first := Shared()
	if first == nil || Shared() != first {
		t.Fatal("Shared() must return one store")
	}
}

// TestClaimLabelConstantsMatchStages pins pkg/claims' label strings (which
// cannot import this package) to the stage table so the claim hooks and the
// reconciler never disagree on the spelling.
func TestClaimLabelConstantsMatchStages(t *testing.T) {
	if got := Claimed.Label(); got != claims.LabelClaimed {
		t.Fatalf("claims.LabelClaimed = %q, stage table says %q", claims.LabelClaimed, got)
	}
	if got := PRInProgress.Label(); got != claims.LabelPRInProgress {
		t.Fatalf("claims.LabelPRInProgress = %q, stage table says %q", claims.LabelPRInProgress, got)
	}
	for _, l := range []string{claims.LegacyLabelClaimed, claims.LegacyLabelInProgress, LegacyCoveredByPR, LegacyCoveredByPRASCII, "CLAIMED"} {
		if !IsLegacy(l) || !Owned(l) {
			t.Errorf("%q should be a retired, owned label", l)
		}
	}
	if IsLegacy(claims.LabelClaimed) || IsLegacy("hive/claimed-by-bob") {
		t.Fatal("live stage label / who-modifier must not be legacy")
	}
	if !CarriesStageAtLeast([]string{"bug", MergeQueue.Label()}, CoveredByPR) {
		t.Fatal("mirrored PR stage should count as covered")
	}
	if CarriesStageAtLeast([]string{Claimed.Label()}, CoveredByPR) || CarriesStageAtLeast(nil, Actionable) {
		t.Fatal("claimed / no stage must not count as covered")
	}
}
