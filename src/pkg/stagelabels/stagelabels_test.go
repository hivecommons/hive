package stagelabels

import (
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/config"
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
		Fixing:                 "stage/070-fixing", NeedsAuthor: "stage/071-needs-author", ReturnedToContributor: "stage/072-returned-to-contributor", AdoptedByHive: "stage/074-adopted-by-hive",
		MergeQueue: "stage/100-merge-queue", ReadyForHumanMerge: "stage/090-ready-for-human-merge", NeedsDecision: "stage/085-needs-decision",
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
	if !Reviewed.Before(AwaitingMergeAdmission) || !AwaitingMergeAdmission.Before(NeedsDecision) || !NeedsDecision.Before(ReadyForHumanMerge) || !AwaitingMergeAdmission.StallNote() || MergeQueue.StallNote() {
		t.Fatal("awaiting-merge-admission must sit between reviewed and needs-decision/ready-for-human-merge and be the only stall-note stage")
	}
	if ReadyForHumanMerge.Dwell() != 0 || NeedsDecision.Dwell() != 0 {
		t.Fatal("human stages are open-ended")
	}
	if got := StallComment(AwaitingMergeAdmission, 30*time.Minute, "review-state-check"); got != StallCommentMarker+"\nstage 080-awaiting-merge-admission: no activity for 30m0s: review-state-check" {
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
			t.Errorf("%s is retired and must not be provisioned (hivecommons/hive#12534)", d.Name)
		case d.Name == RoleTrusted.Label():
			if d.Color != RoleTrustedColor {
				t.Errorf("role/trusted colour %s", d.Color)
			}
		case strings.HasPrefix(d.Name, RolePrefix):
			if d.Color != RoleColor {
				t.Errorf("%s colour %s", d.Name, d.Color)
			}
		case d.Name == authorkind.Human.Label():
			if d.Color != AuthorHumanColor {
				t.Errorf("author/human colour %s", d.Color)
			}
		case d.Name == authorkind.Clanker.Label():
			if d.Color != AuthorClankerColor {
				t.Errorf("author/clanker colour %s", d.Color)
			}
		case d.Name == authorkind.Hive.Label():
			if d.Color != AuthorHiveColor {
				t.Errorf("author/hive colour %s", d.Color)
			}
		case d.Name == DCOLabel:
			// hivecommons/hive#12542: the same green as role/trusted.
			if d.Color != RoleTrustedColor {
				t.Errorf("dco colour %s want role/trusted's %s", d.Color, RoleTrustedColor)
			}
		case d.Name == NoDCOLabel:
			if d.Color != StalledColor {
				t.Errorf("no-dco colour %s want the engine's red %s", d.Color, StalledColor)
			}
		default:
			t.Errorf("unexpected def %s", d.Name)
		}
	}
}

// hivecommons/hive#12542: exactly one of `dco` / `no-dco`, Prow's legacy pair
// stripped once the commit check has spoken, nothing touched while unknown.
func TestPlanDCO(t *testing.T) {
	base := Desired{Stage: ReviewPending, Role: RoleContributor}
	cur := []string{"stage/050-review-pending", "role/contributor", "dco-signoff: yes", "bug"}

	yes := base
	yes.DCO = DCOYes
	add, remove := Plan(cur, yes)
	if !reflect.DeepEqual(add, []string{DCOLabel}) || !reflect.DeepEqual(remove, []string{LegacyDCOYes}) {
		t.Fatalf("yes: add=%v remove=%v", add, remove)
	}
	no := base
	no.DCO = DCONo
	add, remove = Plan([]string{"stage/050-review-pending", "role/contributor", DCOLabel, LegacyDCONo}, no)
	if !reflect.DeepEqual(add, []string{NoDCOLabel}) || !reflect.DeepEqual(remove, []string{DCOLabel, LegacyDCONo}) {
		t.Fatalf("no: add=%v remove=%v", add, remove)
	}
	// Unknown keeps whatever is there, own or legacy.
	add, remove = Plan([]string{"stage/050-review-pending", "role/contributor", NoDCOLabel, LegacyDCOYes}, base)
	if len(add)+len(remove) != 0 {
		t.Fatalf("unknown: add=%v remove=%v", add, remove)
	}
	// Idempotent.
	add, remove = Plan([]string{"stage/050-review-pending", "role/contributor", DCOLabel}, yes)
	if len(add)+len(remove) != 0 {
		t.Fatalf("idempotent: add=%v remove=%v", add, remove)
	}
}

func TestDCOFromLabels(t *testing.T) {
	cases := []struct {
		labels []string
		want   DCOState
	}{
		{nil, DCOUnknown},
		{[]string{"bug"}, DCOUnknown},
		{[]string{DCOLabel}, DCOYes},
		{[]string{NoDCOLabel}, DCONo},
		{[]string{DCOLabel, NoDCOLabel}, DCONo},
		{[]string{LegacyDCOYes}, DCOYes},
		{[]string{LegacyDCONo}, DCONo},
		{[]string{LegacyDCOYes, NoDCOLabel}, DCONo},
		{[]string{LegacyDCONo, DCOLabel}, DCOYes},
	}
	for _, c := range cases {
		if got := DCOFromLabels(c.labels); got != c.want {
			t.Errorf("%v → %v want %v", c.labels, got, c.want)
		}
	}
	if DCOYes.String() != "yes" || DCONo.String() != "no" || DCOUnknown.String() != "unknown" || DCOUnknown.Label() != "" {
		t.Fatal("DCOState renderings")
	}
}

func TestRecordDCO(t *testing.T) {
	r := Record{DCOHead: "abc", DCOTotal: 3, DCOUnsigned: 1}
	if r.DCO("abc") != DCONo || r.DCO("def") != DCOUnknown {
		t.Fatal("record DCO by head")
	}
	r.DCOUnsigned = 0
	if r.DCO("abc") != DCOYes {
		t.Fatal("signed head")
	}
	if (Record{DCOHead: "abc"}).DCO("abc") != DCOUnknown {
		t.Fatal("no commits is unknown, not green")
	}
}

func TestAuthorFromTier(t *testing.T) {
	req := map[string]bool{"trusted": true, "merger": true}
	cases := map[string]Role{"trusted": RoleTrusted, "Merger": RoleTrusted, "contributor": RoleContributor, "newcomer": RoleNewcomer, "revoked": RoleUntrusted, "": RoleUntrusted}
	for tier, want := range cases {
		if got := RoleFromTier(tier, req); got != want {
			t.Errorf("tier %q → %v want %v", tier, got, want)
		}
	}
}

func TestAuthorMergePrecedence(t *testing.T) {
	cases := []struct{ cur, next, want Role }{
		{RoleNone, RoleUntrusted, RoleUntrusted},
		{RoleNone, RoleTrusted, RoleTrusted},
		// #12490: the trusted-author lane's verdict survives the contributor
		// lane's "no registry profile" in either order.
		{RoleTrusted, RoleUntrusted, RoleTrusted},
		{RoleUntrusted, RoleTrusted, RoleTrusted},
		{RoleUntrusted, RoleContributor, RoleContributor},
		{RoleContributor, RoleUntrusted, RoleContributor},
		{RoleContributor, RoleTrusted, RoleTrusted},
		{RoleContributor, RoleNewcomer, RoleContributor},
		{RoleNewcomer, RoleNone, RoleNewcomer},
		{RoleNone, RoleNone, RoleNone},
	}
	for _, tc := range cases {
		if got := tc.cur.Merge(tc.next); got != tc.want {
			t.Errorf("%v.Merge(%v) = %v want %v", tc.cur, tc.next, got, tc.want)
		}
	}
}

func TestPlanIdempotentExclusiveAndStaleRemoval(t *testing.T) {
	want := Desired{Stage: ReviewPending, Role: RoleContributor}
	add, remove := Plan([]string{"bug", "stage/050-review-pending", "role/contributor"}, want)
	if len(add)+len(remove) != 0 {
		t.Fatalf("unchanged state planned writes add=%v remove=%v", add, remove)
	}
	add, remove = Plan([]string{"bug", "stage/070-fixing", "stage/99-old", "role/untrusted", "🐝 covered by PR", "claimed", "hive/in-progress", "hive/claimed-by-bob"}, want)
	if !reflect.DeepEqual(add, []string{"role/contributor", "stage/050-review-pending"}) {
		t.Fatalf("add = %v", add)
	}
	if !reflect.DeepEqual(remove, []string{"claimed", "hive/in-progress", "role/untrusted", "stage/070-fixing", "stage/99-old", "🐝 covered by PR"}) {
		t.Fatalf("remove = %v (human labels and hive/claimed-by-* must survive)", remove)
	}
	add, remove = Plan([]string{"stage/100-merge-queue", "stage/stalled", "role/trusted", "author/clanker"}, Desired{Kind: authorkind.Human})
	if len(add) != 1 || add[0] != "author/human" || !reflect.DeepEqual(remove, []string{"author/clanker", "role/trusted", "stage/100-merge-queue", "stage/stalled"}) {
		t.Fatalf("terminal plan add=%v remove=%v", add, remove)
	}
	add, remove = Plan([]string{"stage/070-fixing", "stage/regressed"}, Desired{Stage: Fixing, Stalled: true})
	if !reflect.DeepEqual(add, []string{"stage/stalled"}) || !reflect.DeepEqual(remove, []string{"stage/regressed"}) {
		t.Fatalf("retired stage/regressed must be stripped by the sweep: add=%v remove=%v", add, remove)
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
	if IsRegression(AdoptedByHive, ReturnedToContributor) || IsRegression(AdoptedByHive, ReviewPending) || !IsRegression(AdoptedByHive, Claimed) {
		t.Fatal("IsRegression adoption hand-back exception wrong")
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
	if RoleTrusted.String() != "trusted" || RoleTrusted.Label() != "role/trusted" || RoleNone.String() != "" || RoleNone.Label() != "" {
		t.Fatalf("author strings: %q %q %q %q", RoleTrusted.String(), RoleTrusted.Label(), RoleNone.String(), RoleNone.Label())
	}
	if got := RegressionComment(MergeQueue, Fixing, "  "); !strings.HasSuffix(got, ": state changed") {
		t.Fatalf("empty reason default: %q", got)
	}
	if got := StallComment(AwaitingMergeAdmission, time.Minute, ""); !strings.HasSuffix(got, ": lane has not evaluated this head") {
		t.Fatalf("empty stall reason default: %q", got)
	}
	if Stalled(Record{}, nil, time.Now()) || Stalled(Record{Stage: ReadyForHumanMerge, EnteredAt: time.Unix(0, 0)}, nil, time.Now()) {
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

func TestStoreMarkNudged(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s := NewStore("", func() time.Time { return now })
	s.MarkNudged("missing", "a", 1, false, now) // unknown key: no-op
	if _, ok := s.Get("missing"); ok {
		t.Fatal("MarkNudged created a record")
	}
	key := Key("acme/widget", 7)
	s.Observe(key, ReturnedToContributor, "check-failing:unit")
	s.MarkNudged(key, "a", 1, true, now)
	r, _ := s.Get(key)
	if r.NudgeHead != "a" || r.NudgeCount != 1 || !r.LastNudgeAt.Equal(now) || !r.FirstNudgeAt.Equal(now) || !r.NudgeOnline {
		t.Fatalf("after first: %+v", r)
	}
	s.MarkNudged(key, "a", 0, false, now.Add(time.Hour)) // n <= count increments; first-nudge facts kept
	if r, _ = s.Get(key); r.NudgeCount != 2 || !r.FirstNudgeAt.Equal(now) || !r.NudgeOnline {
		t.Fatalf("increment: %+v", r)
	}
	s.MarkNudged(key, "b", 1, false, now.Add(2*time.Hour)) // new head restarts
	if r, _ = s.Get(key); r.NudgeHead != "b" || r.NudgeCount != 1 || !r.FirstNudgeAt.Equal(now.Add(2*time.Hour)) || r.NudgeOnline {
		t.Fatalf("new head: %+v", r)
	}
	// Adoption memory: one head, superseding PR, cleared on request.
	s.MarkAdopted(key, "b", 42, now.Add(3*time.Hour))
	if r, _ = s.Get(key); !r.Adopted("b") || r.Adopted("a") || r.AdoptedBy != 42 || !r.AdoptCheckedAt.Equal(now.Add(3*time.Hour)) {
		t.Fatalf("adopted: %+v", r)
	}
	s.MarkAdoptChecked(key, now.Add(4*time.Hour))
	if r, _ = s.Get(key); !r.AdoptCheckedAt.Equal(now.Add(4 * time.Hour)) {
		t.Fatalf("adopt checked: %+v", r)
	}
	s.ClearAdoption(key)
	if r, _ = s.Get(key); r.Adopted("b") || r.AdoptedBy != 0 {
		t.Fatalf("clear adoption: %+v", r)
	}
	// A stage change keeps the nudge memory (same head, no re-nudge).
	s.Observe(key, ReviewPending, "")
	if r, _ = s.Get(key); r.NudgeHead != "b" {
		t.Fatalf("stage change dropped nudge memory: %+v", r)
	}
}

// TestHumanStagesAreNotHighWaterMarks: 085 and 090 are parking stages, so
// leaving them is never a regression, and a record that regressed "from
// 090" under the old rule heals on its next observation
// (hivecommons/hive#12403, #12528).
func TestHumanStagesAreNotHighWaterMarks(t *testing.T) {
	for _, from := range []Stage{ReadyForHumanMerge, NeedsDecision} {
		for _, to := range []Stage{ReviewPending, Reviewed, Fixing, ReturnedToContributor, AwaitingMergeAdmission} {
			if IsRegression(from, to) {
				t.Errorf("%v → %v flagged as regression", from, to)
			}
		}
	}
	if IsRegression(ReadyForHumanMerge, NeedsDecision) || IsRegression(NeedsDecision, ReadyForHumanMerge) {
		t.Fatal("085 ↔ 090 is a flip, not a regression")
	}
	if !IsRegression(MergeQueue, ReadyForHumanMerge) || !IsRegression(AwaitingMergeAdmission, ReviewPending) {
		t.Fatal("ordinary falls must still be regressions")
	}
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	key := Key("o/r", 12403)
	st.Observe(key, ReadyForHumanMerge, "untrusted-author-role")
	if tr := st.Observe(key, ReviewPending, "contributor-lane:no-verdict"); tr.Regressed {
		t.Fatalf("090 → 050 regressed: %+v", tr)
	}
	if rec, _ := st.Get(key); rec.Regressed() {
		t.Fatalf("record regressed: %+v", rec)
	}
	// A record persisted by the old rule.
	st.items[key] = Record{Stage: ReviewPending, EnteredAt: now, RegressedFrom: ReadyForHumanMerge, RegressedAt: now, RegressionCommented: true}
	tr := st.Observe(key, ReturnedToContributor, "reviewer-verdict:changes_requested")
	if !tr.Recovered || tr.Regressed {
		t.Fatalf("stale 090 mark did not heal: %+v", tr)
	}
	if rec, _ := st.Get(key); rec.Regressed() || rec.RegressionCommented {
		t.Fatalf("record after heal: %+v", rec)
	}
}

// hivecommons/hive#12502: the store remembers the author so the dashboard
// can list a contributor's returned PRs without a GitHub call.
func TestStoreNoteAuthorAndByAuthor(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "s.json")
	st := NewStore(path, func() time.Time { return now })

	st.NoteAuthor("acme/w#1", "Alice") // unknown key: no-op
	if _, ok := st.Get("acme/w#1"); ok {
		t.Fatal("NoteAuthor must not create a record")
	}
	st.Observe("acme/w#1", ReturnedToContributor, "check-failing:unit")
	st.Observe("acme/w#2", ReturnedToContributor, "conflicting")
	st.Observe("acme/w#3", ReviewPending, "")
	st.Observe("acme/w#4", ReturnedToContributor, "changes-requested")
	st.NoteAuthor("acme/w#1", "Alice")
	st.NoteAuthor("acme/w#2", "alice")
	st.NoteAuthor("acme/w#3", "ALICE")
	st.NoteAuthor("acme/w#4", "bob")
	st.NoteAuthor("acme/w#4", "") // empty login: no-op

	if r, _ := st.Get("acme/w#1"); r.Author != "alice" {
		t.Fatalf("author should be lower-cased, got %q", r.Author)
	}
	if r, _ := st.Get("acme/w#4"); r.Author != "bob" {
		t.Fatalf("empty login must not clear the author, got %q", r.Author)
	}
	got := st.ByAuthor("aLiCe", ReturnedToContributor)
	if len(got) != 2 || got[0].Key != "acme/w#1" || got[1].Key != "acme/w#2" {
		t.Fatalf("ByAuthor(072) = %+v", got)
	}
	if all := st.ByAuthor("alice", None); len(all) != 3 {
		t.Fatalf("ByAuthor(any) = %+v", all)
	}
	if st.ByAuthor("", None) != nil || len(st.ByAuthor("carol", None)) != 0 {
		t.Fatal("unknown / empty author must return nothing")
	}
	// Author survives a stage change and a persistence round trip.
	st.Observe("acme/w#1", ReviewPending, "")
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st2 := NewStore(path, func() time.Time { return now })
	if err := st2.Load(); err != nil {
		t.Fatal(err)
	}
	if r, _ := st2.Get("acme/w#1"); r.Author != "alice" || r.Stage != ReviewPending {
		t.Fatalf("round trip: %+v", r)
	}
	if got := st2.ByAuthor("alice", ReturnedToContributor); len(got) != 1 || got[0].Key != "acme/w#2" {
		t.Fatalf("after recovery ByAuthor(072) = %+v", got)
	}
}

// hivecommons/hive#12534: stage/stalled means "nobody is working this".
// hivecommons/hive#12417 is the specimen: review-pending with no reviewer
// verdict for the head is waiting on the hive and must never stall.
func TestStoreStalledRequiresQuiet(t *testing.T) {
	now := time.Date(2026, 10, 16, 13, 33, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), clock)
	key := Key("hivecommons/hive", 12417)
	st.Observe(key, ReviewPending, "contributor-lane:no-verdict")
	st.NoteActivity(key, Activity{Head: "d2f1572", Comments: 0, Digest: "verdict:?|ci:0|", WaitingOnHive: true})
	rec, _ := st.Get(key)
	if rec.LastActivityAt.IsZero() || !rec.WaitingOnHive {
		t.Fatalf("activity not recorded: %+v", rec)
	}
	for _, quiet := range []bool{true, false} {
		if StalledWith(rec, nil, now.Add(6*time.Hour), quiet) {
			t.Fatalf("050 with no verdict stalled after 6h (require_quiet=%v): the wait is on the reviewer, not a person", quiet)
		}
	}
	// Checks running is likewise waiting on the hive.
	now = now.Add(2 * time.Hour)
	st.NoteActivity(key, Activity{Head: "d2f1572", Comments: 0, Digest: "verdict:approve|ci:2|", WaitingOnHive: true})
	rec, _ = st.Get(key)
	if StalledWith(rec, nil, now.Add(3*time.Hour), true) {
		t.Fatal("stalled while checks pending")
	}
	// Verdict landed, checks green, and then nothing happens for the dwell.
	now = now.Add(time.Minute)
	if !st.NoteActivity(key, Activity{Head: "d2f1572", Comments: 0, Digest: "verdict:approve|ci:1|"}) {
		t.Fatal("digest change must count as activity")
	}
	rec, _ = st.Get(key)
	if !rec.QuietSince().Equal(now) {
		t.Fatalf("quiet since %v want %v", rec.QuietSince(), now)
	}
	if StalledWith(rec, nil, now.Add(59*time.Minute), true) {
		t.Fatal("stalled before the dwell elapsed quietly")
	}
	if !StalledWith(rec, nil, now.Add(time.Hour), true) {
		t.Fatal("not stalled after a quiet dwell with the verdict in hand")
	}
	if rec.QuietFor(now.Add(90*time.Minute)) != 90*time.Minute {
		t.Fatalf("QuietFor = %v", rec.QuietFor(now.Add(90*time.Minute)))
	}
	// Any outside activity clears it: a comment …
	now = now.Add(2 * time.Hour)
	if !st.NoteActivity(key, Activity{Head: "d2f1572", Comments: 1, Digest: "verdict:approve|ci:1|"}) {
		t.Fatal("new comment must count as activity")
	}
	rec, _ = st.Get(key)
	if StalledWith(rec, nil, now.Add(30*time.Minute), true) || !StalledWith(rec, nil, now.Add(time.Hour), true) {
		t.Fatal("comment did not restart the quiet clock")
	}
	// … but not the hive's own comment …
	st.CountOwnComments(key, 1)
	if st.NoteActivity(key, Activity{Head: "d2f1572", Comments: 2, Digest: "verdict:approve|ci:1|"}) {
		t.Fatal("hive's own comment counted as outside activity")
	}
	// … a body-only review submission counts (Reviews -1 = not listed) …
	if st.NoteActivity(key, Activity{Head: "d2f1572", Comments: 2, Reviews: -1, Digest: "verdict:approve|ci:1|"}) {
		t.Fatal("unknown review count must not read as activity")
	}
	if !st.NoteActivity(key, Activity{Head: "d2f1572", Comments: 2, Reviews: 1, Digest: "verdict:approve|ci:1|"}) {
		t.Fatal("new review submission must count as activity")
	}
	if st.NoteActivity(key, Activity{Head: "d2f1572", Comments: 2, Reviews: 1, Digest: "verdict:approve|ci:1|"}) {
		t.Fatal("re-observed review counted again")
	}
	// … and a head push, dated by the commit, not by observation.
	pushed := now.Add(45 * time.Minute)
	now = now.Add(time.Hour)
	if !st.NoteActivity(key, Activity{Head: "e000001", Comments: 2, Digest: "verdict:?|ci:0|", At: pushed, WaitingOnHive: true}) {
		t.Fatal("head push must count as activity")
	}
	rec, _ = st.Get(key)
	if !rec.LastActivityAt.Equal(pushed) {
		t.Fatalf("activity at %v want commit date %v", rec.LastActivityAt, pushed)
	}
	// Without require_quiet the dwell is measured from entering the stage.
	rec.WaitingOnHive = false
	if !StalledWith(rec, nil, now, false) || StalledWith(rec, nil, now, true) {
		t.Fatal("require_quiet=false must fall back to the entered-at clock")
	}
}

func TestStoreHistoryRecordsTransitions(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	key := Key("o/r", 1)
	for _, s := range []Stage{ReviewPending, Reviewed, MergeQueue, Reviewed} {
		now = now.Add(time.Hour)
		st.Observe(key, s, "r")
	}
	rec, _ := st.Get(key)
	if len(rec.History) != 4 || rec.History[3].From != MergeQueue || rec.History[3].Stage != Reviewed || !rec.History[3].Regressed {
		t.Fatalf("history = %+v", rec.History)
	}
	for i := 0; i < HistoryLimit+3; i++ {
		now = now.Add(time.Hour)
		st.Observe(key, []Stage{ReviewPending, Reviewed}[i%2], "r")
	}
	rec, _ = st.Get(key)
	if len(rec.History) != HistoryLimit {
		t.Fatalf("history not capped: %d", len(rec.History))
	}
}

// hivecommons/hive#12557: a gate flapping 100 → 060 → 100 → 060 on one head
// used to re-arm the regression comment on every recover. The comment is
// due once per head SHA; a new head re-arms it.
func TestRegressionCommentOncePerHeadAcrossFlaps(t *testing.T) {
	now := time.Date(2026, 10, 16, 20, 45, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	key := Key("o/r", 12423)

	st.Observe(key, MergeQueue, "contributor-lane:soaking")
	if tr := st.Observe(key, Reviewed, "pending: head pushed 54s ago (< min_head_age)"); !tr.Regressed {
		t.Fatalf("first fall not a regression: %+v", tr)
	}
	rec, _ := st.Get(key)
	if !rec.RegressionCommentDue("73708b4") {
		t.Fatal("first regression on a head must be due")
	}
	st.MarkRegressionCommentedForHead(key, "73708b4")

	for i := 0; i < 3; i++ {
		if tr := st.Observe(key, MergeQueue, "contributor-lane:soaking"); !tr.Recovered {
			t.Fatalf("flap %d: recover not reported: %+v", i, tr)
		}
		if tr := st.Observe(key, Reviewed, "pending: head pushed 1m15s ago (< min_head_age)"); !tr.Regressed {
			t.Fatalf("flap %d: fall not a regression: %+v", i, tr)
		}
		rec, _ = st.Get(key)
		if rec.RegressionCommentDue("73708b4") {
			t.Fatalf("flap %d: regression comment due again on the same head: %+v", i, rec)
		}
	}
	if !rec.RegressionCommentDue("690a729") {
		t.Fatal("a new head must re-arm the regression comment")
	}
	// Items without a head keep the per-regression behaviour.
	if !rec.RegressionCommentDue("") {
		t.Fatal("empty head must fall back to the per-regression flag")
	}
}

// TestStoreHistoryTrail checks that Observe records one Hop per stage
// change (not per same-stage observation), caps the trail at HistoryLimit
// oldest-first, persists it, and that a same-stage reason change still
// marks the store dirty.
func TestStoreHistoryTrail(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	path := filepath.Join(t.TempDir(), "s.json")
	st := NewStore(path, clock)
	key := Key("o/r", 9)

	st.Observe(key, ReviewPending, "contributor-lane:no-verdict")
	now = now.Add(time.Minute)
	st.Observe(key, ReviewPending, "required-check-pending:build")
	rec, _ := st.Get(key)
	if len(rec.History) != 1 || rec.History[0].Stage != ReviewPending || rec.History[0].Reason != "contributor-lane:no-verdict" {
		t.Fatalf("same-stage observation must not add a hop: %+v", rec.History)
	}
	if rec.Reason != "required-check-pending:build" || !st.dirty {
		t.Fatalf("reason update must be kept and persisted: %+v dirty=%v", rec, st.dirty)
	}
	stages := []Stage{Reviewed, Fixing, ReviewPending, Reviewed, AwaitingMergeAdmission, MergeQueue, Reviewed, MergeQueue, ReadyForHumanMerge}
	for _, s := range stages {
		now = now.Add(time.Minute)
		st.Observe(key, s, "r-"+s.Slug())
	}
	rec, _ = st.Get(key)
	if len(rec.History) != HistoryLimit {
		t.Fatalf("history len = %d, want cap %d", len(rec.History), HistoryLimit)
	}
	if last := rec.History[HistoryLimit-1]; last.Stage != ReadyForHumanMerge || !last.At.Equal(now) || last.Reason != "r-ready-for-human-merge" {
		t.Fatalf("newest hop = %+v", last)
	}
	if first := rec.History[0]; first.Stage != Fixing {
		t.Fatalf("oldest kept hop = %+v, want the drop of the two oldest", first)
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st2 := NewStore(path, clock)
	if err := st2.Load(); err != nil {
		t.Fatal(err)
	}
	rec2, _ := st2.Get(key)
	if len(rec2.History) != HistoryLimit || rec2.History[0].Stage != Fixing {
		t.Fatalf("history not persisted: %+v", rec2.History)
	}
}

// hivecommons/hive#12581: the trust axis moved from author/<tier> to
// role/<tier>. Plan migrates the old spelling in the same write, and with
// no author-kind opinion leaves an existing author/<kind> label alone.
func TestPlanMigratesLegacyRoleAndKeepsKindWhenUnknown(t *testing.T) {
	add, remove := Plan([]string{"stage/050-review-pending", "author/trusted", "author/clanker"}, Desired{Stage: ReviewPending, Role: RoleTrusted})
	if !reflect.DeepEqual(add, []string{"role/trusted"}) || !reflect.DeepEqual(remove, []string{"author/trusted"}) {
		t.Fatalf("migration add=%v remove=%v", add, remove)
	}
	// With a kind opinion the stale kind is swapped and the legacy tier still goes.
	add, remove = Plan([]string{"stage/050-review-pending", "author/contributor", "author/clanker"}, Desired{Stage: ReviewPending, Role: RoleContributor, Kind: authorkind.Hive})
	if !reflect.DeepEqual(add, []string{"author/hive", "role/contributor"}) || !reflect.DeepEqual(remove, []string{"author/clanker", "author/contributor"}) {
		t.Fatalf("kind swap add=%v remove=%v", add, remove)
	}
	// Configured names are honoured, and the defaults they replace are stale.
	names := authorkind.Names{Clanker: "author/relay"}
	add, remove = Plan([]string{"author/clanker"}, Desired{Stage: ReviewPending, Kind: authorkind.Clanker, KindLabels: names})
	if !reflect.DeepEqual(add, []string{"author/relay", "stage/050-review-pending"}) || !reflect.DeepEqual(remove, []string{"author/clanker"}) {
		t.Fatalf("configured names add=%v remove=%v", add, remove)
	}
	for _, l := range LegacyRoleLabels() {
		if !IsLegacyRole(l) || !Owned(l) {
			t.Errorf("%s not recognised as a retired role spelling", l)
		}
	}
	if IsLegacyRole("author/human") || IsLegacyRole("role/trusted") {
		t.Error("kind/current labels misread as legacy role")
	}
	if RoleTrusted.LegacyLabel() != "author/trusted" || RoleNone.LegacyLabel() != "" {
		t.Errorf("LegacyLabel: %q %q", RoleTrusted.LegacyLabel(), RoleNone.LegacyLabel())
	}
}

func TestLabelDefsForUsesConfiguredKindNames(t *testing.T) {
	defs := LabelDefsFor(authorkind.Names{Hive: "author/bot-hive"})
	seen := map[string]string{}
	for _, d := range defs {
		seen[d.Name] = d.Color
	}
	if seen["author/bot-hive"] != AuthorHiveColor || seen["author/human"] != AuthorHumanColor || seen["author/clanker"] != AuthorClankerColor {
		t.Fatalf("defs = %v", seen)
	}
	if _, ok := seen["author/hive"]; ok {
		t.Fatal("default hive name provisioned despite override")
	}
	if _, ok := seen["author/trusted"]; ok {
		t.Fatal("legacy role spelling provisioned")
	}
}

// Stage-074 in-place adoption bookkeeping: MarkAdoptedInPlace / MarkAdoptedHead
// keep the hive's ownership across its own pushes; MarkHivePushedHead records
// local push evidence; MarkYielded releases it and remembers the contributor's
// head so the yield note is posted once.
func TestStoreAdoptInPlaceAndYield(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s := NewStore("", func() time.Time { return now })
	key := Key("acme/widget", 7)
	s.MarkAdoptedInPlace("missing", "a", "contributor-offline", now)
	if _, ok := s.Get("missing"); ok {
		t.Fatal("MarkAdoptedInPlace created a record")
	}
	s.Observe(key, ReturnedToContributor, "check-failing:unit")
	r, _ := s.Get(key)
	if r.AdoptedInPlace() || r.Adopted("a") {
		t.Fatalf("fresh record adopted: %+v", r)
	}
	s.MarkAdoptedInPlace(key, "a", "contributor-offline", now)
	r, _ = s.Get(key)
	if !r.AdoptedInPlace() || r.AdoptMode != AdoptModeInPlace || r.AdoptReason != "contributor-offline" || r.AdoptedHead != "a" || !r.AdoptedAt.Equal(now) || r.AdoptedBy != 0 {
		t.Fatalf("in place: %+v", r)
	}
	s.MarkAdoptedHead(key, "b")
	r, _ = s.Get(key)
	if !r.AdoptedInPlace() || r.AdoptedHead != "b" {
		t.Fatalf("head moved: %+v", r)
	}
	s.MarkHivePushedHead(key, "c")
	r, _ = s.Get(key)
	if !r.HivePushedHead("c") || r.HivePushedHead("d") || !s.HasHivePushEvidence("acme/widget") || s.HasHivePushEvidence("other/repo") {
		t.Fatalf("hive pushed head not recorded: %+v", r)
	}
	s.MarkYielded(key, "c")
	r, _ = s.Get(key)
	if r.AdoptedInPlace() || r.AdoptedHead != "" || r.AdoptMode != "" || r.AdoptReason != "" || r.YieldedHead != "c" {
		t.Fatalf("yielded: %+v", r)
	}
	// Supersede path tags its mode too; ClearAdoption wipes everything.
	s.MarkAdopted(key, "c", 90, now)
	r, _ = s.Get(key)
	if r.AdoptMode != AdoptModeSupersede || !r.Adopted("c") || r.AdoptedInPlace() {
		t.Fatalf("supersede: %+v", r)
	}
	s.ClearAdoption(key)
	r, _ = s.Get(key)
	if r.AdoptMode != "" || r.AdoptReason != "" || r.AdoptedHead != "" {
		t.Fatalf("cleared: %+v", r)
	}
}

// GitHub rejects label descriptions over 100 characters with 422; one such
// label aborted ensureStageLabels for every repo, so nothing downstream of
// it (adoption included) ran. Every provisioned description must fit.
func TestLabelDescriptionsFitGitHubLimit(t *testing.T) {
	for _, def := range AllLabelDefs() {
		if n := len([]rune(def.Description)); n > MaxLabelDescription {
			t.Errorf("%s: description is %d runes, max %d: %q", def.Name, n, MaxLabelDescription, def.Description)
		}
	}
	for old, stage := range Renames {
		if n := len([]rune(stage.Description())); n > MaxLabelDescription {
			t.Errorf("rename %s → %s: description is %d runes, max %d", old, stage.Label(), n, MaxLabelDescription)
		}
	}
}

func TestClampLabelDescription(t *testing.T) {
	long := strings.Repeat("x", MaxLabelDescription) + " tail"
	if got := ClampLabelDescription(long); len([]rune(got)) != MaxLabelDescription {
		t.Fatalf("clamp: got %d runes", len([]rune(got)))
	}
	if got := ClampLabelDescription("short"); got != "short" {
		t.Fatalf("clamp changed a short description: %q", got)
	}
}

// Stage 07x by author kind: the "author must act" outcome is 070 fixing for
// the hive's own PR, 071 needs-author for a human's, 072
// returned-to-contributor for a clanker's. Same evidence, same transitions;
// only who is on the hook differs.
func TestFixingStageForByAuthorKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		self bool
		kind authorkind.Kind
		want Stage
	}{
		{"human", false, authorkind.Human, NeedsAuthor},
		{"clanker", false, authorkind.Clanker, ReturnedToContributor},
		{"hive kind", false, authorkind.Hive, Fixing},
		{"self-authored lane", true, authorkind.Unknown, Fixing},
		{"self-authored lane beats clanker kind", true, authorkind.Clanker, Fixing},
		{"unknown kind reads as human", false, authorkind.Unknown, NeedsAuthor},
	} {
		if got := FixingStageFor(tc.self, tc.kind); got != tc.want {
			t.Errorf("%s: FixingStageFor(%v, %q) = %v, want %v", tc.name, tc.self, tc.kind, got, tc.want)
		}
	}
	for _, s := range []Stage{Fixing, NeedsAuthor, ReturnedToContributor} {
		if !AuthorBlocked(s) {
			t.Errorf("%v must be author-blocked", s)
		}
	}
	for _, s := range []Stage{None, ReviewPending, AdoptedByHive, NeedsDecision, MergeQueue} {
		if AuthorBlocked(s) {
			t.Errorf("%v must not be author-blocked", s)
		}
	}
	// 071 sits between 070 and 072, is a PR-only stage, dwells like 072, and
	// reads from labels like any other stage.
	d, ok := definition(NeedsAuthor)
	if !ok || d.Number != 71 || d.Slug != "needs-author" || !d.PR || d.Issue || d.Dwell != ReturnedToContributor.Dwell() {
		t.Fatalf("071 definition = %+v ok=%v", d, ok)
	}
	if !Fixing.Before(NeedsAuthor) || !NeedsAuthor.Before(ReturnedToContributor) {
		t.Fatal("071 must order between 070 and 072")
	}
	if got := Current([]string{"kind/bug", "stage/071-needs-author"}); got != NeedsAuthor {
		t.Fatalf("Current = %v", got)
	}
	// Leaving 071 for review of a new head is the repair route, not a
	// regression; 074 → 071 is a declined adoption handed back, not a knock-back.
	if IsRegression(NeedsAuthor, ReviewPending) || IsRegression(NeedsAuthor, Reviewed) || IsRegression(AdoptedByHive, NeedsAuthor) {
		t.Fatal("071 repair transitions must not regress")
	}
	if !IsRegression(MergeQueue, NeedsAuthor) {
		t.Fatal("100 → 071 is a regression")
	}
}

func TestRenamesWithdrawnNeedsAuthorSpellingReads072(t *testing.T) {
	if got := Current([]string{"stage/072-needs-author"}); got != ReturnedToContributor {
		t.Fatalf("withdrawn 072 spelling = %v, want ReturnedToContributor", got)
	}
	if got := Current([]string{NeedsAuthor.Label()}); got != NeedsAuthor {
		t.Fatalf("071 label = %v, want NeedsAuthor", got)
	}
	if NeedsAuthor.Label() != "stage/071-needs-author" || ReturnedToContributor.Label() != "stage/072-returned-to-contributor" {
		t.Fatalf("labels: %q %q", NeedsAuthor.Label(), ReturnedToContributor.Label())
	}
}

// hivecommons/hive#12683: config spells the decision park label out because
// it cannot import this package; the two must never drift.
func TestNeedsDecisionLabelMatchesConfigDefault(t *testing.T) {
	if NeedsDecision.Label() != config.DefaultNeedsDecisionLabel {
		t.Fatalf("stage label %q != config.DefaultNeedsDecisionLabel %q", NeedsDecision.Label(), config.DefaultNeedsDecisionLabel)
	}
}

// Adoption is an ownership state, not a CI state: a live adoption replaces
// every review-side stage with 074 and gives way only at the merge gate
// (hivecommons/hive#12736).
func TestYieldsToAdoption(t *testing.T) {
	for _, tc := range []struct {
		stage Stage
		want  bool
	}{
		{None, false},
		{Actionable, true},
		{ReviewPending, true},
		{Reviewed, true},
		{Fixing, true},
		{NeedsAuthor, true},
		{ReturnedToContributor, true},
		{AdoptedByHive, true},
		{AwaitingMergeAdmission, false},
		{NeedsDecision, false},
		{ReadyForHumanMerge, false},
		{MergeQueue, false},
	} {
		if got := tc.stage.YieldsToAdoption(); got != tc.want {
			t.Errorf("%s.YieldsToAdoption() = %v, want %v", tc.stage, got, tc.want)
		}
	}
}

func TestRecordAdoptionLive(t *testing.T) {
	clock := func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), clock)
	key := "acme/widget#7"
	st.Observe(key, ReturnedToContributor, "check-failing:unit")
	if rec, _ := st.Get(key); rec.AdoptionLive("aaa") {
		t.Fatal("live before adoption")
	}
	// Supersede: bound to the adopted head only.
	st.MarkAdopted(key, "aaa", 99, clock())
	rec, _ := st.Get(key)
	if !rec.AdoptionLive("aaa") || rec.AdoptionLive("bbb") || rec.AdoptionLive("") {
		t.Fatalf("supersede: %+v", rec)
	}
	st.ClearAdoption(key)
	if rec, _ = st.Get(key); rec.AdoptionLive("aaa") {
		t.Fatal("live after ClearAdoption")
	}
	// In place: the hive's pushes move the head, the adoption follows.
	st.MarkAdoptedInPlace(key, "aaa", "contributor-offline", clock())
	st.MarkAdoptedHead(key, "bbb")
	if rec, _ = st.Get(key); !rec.AdoptionLive("bbb") || !rec.AdoptionLive("ccc") {
		t.Fatalf("in-place: %+v", rec)
	}
	st.MarkYielded(key, "ccc")
	if rec, _ = st.Get(key); rec.AdoptionLive("ccc") || rec.YieldedHead != "ccc" {
		t.Fatalf("yielded: %+v", rec)
	}
}
