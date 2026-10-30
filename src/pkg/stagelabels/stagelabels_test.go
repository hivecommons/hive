package stagelabels

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/authorkind"
)

func TestTableIsOrderedSparseAndUnique(t *testing.T) {
	seenNum := map[int]bool{}
	seenSlug := map[string]bool{}
	prev := 0
	for _, d := range Table {
		if d.Number <= prev {
			t.Fatalf("table not strictly increasing at %s (%d after %d)", d.Slug, d.Number, prev)
		}
		if seenNum[d.Number] {
			t.Fatalf("duplicate number %d/%s", d.Number, d.Slug)
		}
		// A slug may repeat only across kinds (045/085 needs-decision: the
		// issue and PR parking bays); within a kind it must be unique.
		kindSlug := d.Slug + "/" + strconv.FormatBool(d.Issue)
		if seenSlug[kindSlug] {
			t.Fatalf("duplicate slug %d/%s", d.Number, d.Slug)
		}
		seenNum[d.Number], seenSlug[kindSlug] = true, true
		prev = d.Number
		if !d.Issue && !d.PR {
			t.Fatalf("%s is emitted for neither issues nor PRs", d.Slug)
		}
		if !strings.HasPrefix(d.Stage.Label(), Prefix) {
			t.Fatalf("label %q lacks prefix", d.Stage.Label())
		}
	}
	want := map[Stage]string{
		Actionable: "stage/010-actionable", TriageNeeded: "stage/015-triage-needed", Claimed: "stage/020-claimed", PRInProgress: "stage/030-pr-in-progress",
		CoveredByPR: "stage/040-covered-by-pr", IssueNeedsDecision: "stage/045-needs-decision", ReviewPending: "stage/050-review-pending", Reviewed: "stage/060-reviewed",
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
	if ReadyForHumanMerge.Dwell() != 0 || NeedsDecision.Dwell() != 0 || IssueNeedsDecision.Dwell() != 0 {
		t.Fatal("human stages are open-ended")
	}
	if !CoveredByPR.Before(IssueNeedsDecision) || !IssueNeedsDecision.Before(ReviewPending) {
		t.Fatal("045 must sit between covered-by-pr and review-pending")
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
		case d.Name == MergeCommitLabel:
			// Lane state, not a verdict: the stage orange.
			if d.Color != StageColor {
				t.Errorf("merge-method/merge colour %s want the stage colour %s", d.Color, StageColor)
			}
		default:
			t.Errorf("unexpected def %s", d.Name)
		}
	}
}

// hivecommons/hive#12542: exactly one of `dco` / `no-dco` once the commit
// check has spoken, nothing touched while unknown. Prow's `dco-signoff: *`
// pair is foreign (hivecommons/hive#13125) and is left alone in every case.
func TestPlanDCO(t *testing.T) {
	base := Desired{Stage: ReviewPending, Role: RoleContributor}
	cur := []string{"stage/050-review-pending", "role/contributor", "dco-signoff: yes", "bug"}

	yes := base
	yes.DCO = DCOYes
	add, remove := Plan(cur, yes)
	if !reflect.DeepEqual(add, []string{DCOLabel}) || len(remove) != 0 {
		t.Fatalf("yes: add=%v remove=%v", add, remove)
	}
	no := base
	no.DCO = DCONo
	add, remove = Plan([]string{"stage/050-review-pending", "role/contributor", DCOLabel, LegacyDCONo}, no)
	if !reflect.DeepEqual(add, []string{NoDCOLabel}) || !reflect.DeepEqual(remove, []string{DCOLabel}) {
		t.Fatalf("no: add=%v remove=%v", add, remove)
	}
	// Unknown keeps whatever is there, own or legacy.
	add, remove = Plan([]string{"stage/050-review-pending", "role/contributor", NoDCOLabel, LegacyDCOYes}, base)
	if len(add)+len(remove) != 0 {
		t.Fatalf("unknown: add=%v remove=%v", add, remove)
	}
	// Idempotent — and still idempotent with Prow's pill alongside ours,
	// which is the steady state on a repo where the Prow dco plugin runs.
	for _, cur := range [][]string{
		{"stage/050-review-pending", "role/contributor", DCOLabel},
		{"stage/050-review-pending", "role/contributor", DCOLabel, LegacyDCOYes},
		{"stage/050-review-pending", "role/contributor", DCOLabel, LegacyDCONo},
	} {
		add, remove = Plan(cur, yes)
		if len(add)+len(remove) != 0 {
			t.Fatalf("idempotent %v: add=%v remove=%v", cur, add, remove)
		}
	}
}

// hivecommons/hive#13125: the writer re-added nothing and removed
// `dco-signoff: yes` every cycle while Prow / the DCO workflow put it back —
// a label war on every open PR. Labels other tools own must never appear in
// a plan, whatever the desired state, on PRs or issues.
func TestPlanNeverTouchesThirdPartyLabels(t *testing.T) {
	foreign := []string{
		LegacyDCOYes, LegacyDCONo, "DCO-Signoff: Yes", "dco-signoff: pending",
		"lgtm", "approved", "needs-rebase", "ok-to-test",
		"tide/merge-method-squash",
		"do-not-merge/hold", "do-not-merge/work-in-progress",
		"size/XS", "size/XXL",
		"kind/bug", "kind/feature",
		"dco", // exact own name, wanted below — listed to prove exactness
	}
	wants := []Desired{
		{},
		{Stage: ReviewPending, Role: RoleContributor, DCO: DCOYes},
		{Stage: ReviewPending, Role: RoleContributor, DCO: DCONo},
		{Stage: ReturnedToContributor, Role: RoleContributor, Kind: authorkind.Human, Stalled: true, DCO: DCONo},
		{Stage: MergeQueue, Role: RoleTrusted, DCO: DCOYes},
	}
	for _, want := range wants {
		cur := append([]string{"stage/050-review-pending", "role/contributor", "author/human"}, foreign...)
		add, remove := Plan(cur, want)
		for _, l := range append(add, remove...) {
			if l == DCOLabel || l == NoDCOLabel {
				continue
			}
			for _, f := range foreign {
				if l == f {
					t.Errorf("want=%+v: plan touched third-party label %q (add=%v remove=%v)", want, l, add, remove)
				}
			}
		}
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

func TestPlanCaseInsensitiveMatching(t *testing.T) {
	// Test that Plan recognizes mis-cased stage labels.
	want := Desired{Stage: ReviewPending, Role: RoleContributor}
	add, remove := Plan([]string{"bug", "STAGE/050-REVIEW-PENDING", "role/contributor"}, want)
	if len(add)+len(remove) != 0 {
		t.Fatalf("mis-cased stage label should be recognized: add=%v remove=%v", add, remove)
	}

	// Test that Plan removes a mis-cased label and adds the canonical form.
	add, remove = Plan([]string{"bug", "STAGE/070-FIXING"}, Desired{Stage: ReviewPending})
	if !reflect.DeepEqual(remove, []string{"STAGE/070-FIXING"}) {
		t.Fatalf("mis-cased stage/070-fixing should be removed: remove=%v", remove)
	}
	if !reflect.DeepEqual(add, []string{"stage/050-review-pending"}) {
		t.Fatalf("canonical stage should be added: add=%v", add)
	}

	// A mixed-case configured author/<kind> name already on the item must be
	// matched (no churn), and when missing must be added with its configured
	// spelling.
	names := authorkind.Names{Human: "author/Human"}
	add, remove = Plan([]string{"bug", "author/Human", "stage/050-review-pending"},
		Desired{Stage: ReviewPending, Kind: authorkind.Human, KindLabels: names})
	if len(add)+len(remove) != 0 {
		t.Fatalf("configured mixed-case author label should be idempotent: add=%v remove=%v", add, remove)
	}
	add, remove = Plan([]string{"bug", "stage/050-review-pending"},
		Desired{Stage: ReviewPending, Kind: authorkind.Human, KindLabels: names})
	if !reflect.DeepEqual(add, []string{"author/Human"}) || len(remove) != 0 {
		t.Fatalf("configured author label should be added verbatim: add=%v remove=%v", add, remove)
	}

	// Test that Owned recognizes mis-cased labels.
	if !Owned("STAGE/010-ACTIONABLE") || !Owned("ROLE/TRUSTED") || !Owned("AUTHOR/HUMAN") {
		t.Fatal("Owned should recognize mis-cased labels")
	}

	// Test that Current recognizes mis-cased labels.
	cur := Current([]string{"STAGE/030-PR-IN-PROGRESS"})
	if cur != PRInProgress {
		t.Fatalf("Current should recognize mis-cased labels: got %v want %v", cur, PRInProgress)
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
	// move to fixing is forward (and, being a forward step, resolves the
	// open regression — the badge comes off as soon as the item moves on),
	// leaving fixing for review does not regress.
	if tr = st.Observe(key, Fixing, "check-failing:unit"); tr.Regressed || !tr.Recovered {
		t.Fatalf("50→70 is forward and resolves the regression: %+v", tr)
	}
	if rec, _ = st.Get(key); rec.Regressed() {
		t.Fatalf("forward step must clear the regression: %+v", rec)
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
	// 070/071/072 re-attribute the fix among hive, human and clanker; none
	// of those moves is a knock-back (hive-signoff-attribution 072 → 070).
	for _, from := range []Stage{Fixing, NeedsAuthor, ReturnedToContributor} {
		for _, to := range []Stage{Fixing, NeedsAuthor, ReturnedToContributor} {
			if IsRegression(from, to) {
				t.Errorf("%v → %v flagged as regression", from, to)
			}
		}
	}
	// Fall again from 100 so a plain forward step can be watched clearing
	// it: 050 → 060 is not back at the high-water mark but it is moving
	// ahead, and that is enough.
	st.Observe(key, MergeQueue, "soaking")
	if tr = st.Observe(key, ReviewPending, "head-changed"); !tr.Regressed {
		t.Fatalf("fall again: %+v", tr)
	}
	tr = st.Observe(key, Reviewed, "y")
	rec, _ = st.Get(key)
	if !tr.Recovered || rec.Regressed() {
		t.Fatalf("first forward step must resolve the regression: tr=%+v rec=%+v", tr, rec)
	}
	if tr = st.Observe(key, MergeQueue, "z"); tr.Recovered || tr.Regressed {
		t.Fatalf("nothing left to recover: %+v", tr)
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

// hivecommons/hive#15443: the per-author trust-promotion record is claimed
// once, released by a failed post, cleared by a demotion, survives a
// reload and Prune only drops it after trustPromotionRetention.
func TestStoreTrustPromotionAnnouncement(t *testing.T) {
	now := time.Date(2026, 10, 29, 17, 20, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "s.json")
	st := NewStore(path, func() time.Time { return now })
	if !st.ClaimTrustPromotionAnnouncement("Norway-02") {
		t.Fatal("first claim should win")
	}
	if st.ClaimTrustPromotionAnnouncement("norway-02") {
		t.Fatal("second claim (case-insensitive) should lose")
	}
	if !st.TrustPromotionAnnounced("NORWAY-02") {
		t.Fatal("announced should be true after a claim")
	}
	st.ReleaseTrustPromotionAnnouncement("Norway-02")
	if st.TrustPromotionAnnounced("Norway-02") || !st.ClaimTrustPromotionAnnouncement("Norway-02") {
		t.Fatal("release should let the next claim win")
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	later := now.Add(30 * 24 * time.Hour)
	st2 := NewStore(path, func() time.Time { return later })
	if err := st2.Load(); err != nil {
		t.Fatal(err)
	}
	if n := st2.Prune(14 * 24 * time.Hour); n != 0 || !st2.TrustPromotionAnnounced("Norway-02") {
		t.Fatalf("prune dropped the trust record: n=%d keys=%v", n, st2.Keys())
	}
	if st2.ClaimTrustPromotionAnnouncement("Norway-02") {
		t.Fatal("claim after reload should lose")
	}
	veryLate := now.Add(trustPromotionRetention + 24*time.Hour)
	st3 := NewStore(path, func() time.Time { return veryLate })
	if err := st3.Load(); err != nil {
		t.Fatal(err)
	}
	if n := st3.Prune(14 * 24 * time.Hour); n != 1 || st3.TrustPromotionAnnounced("Norway-02") {
		t.Fatalf("prune kept a trust record past retention: n=%d keys=%v", n, st3.Keys())
	}
	st2.ClearTrustPromotion("Norway-02")
	if st2.TrustPromotionAnnounced("Norway-02") || !st2.ClaimTrustPromotionAnnouncement("Norway-02") {
		t.Fatal("clear should let a re-promotion announce again")
	}
	if st.ClaimTrustPromotionAnnouncement("") || (*Store)(nil).ClaimTrustPromotionAnnouncement("x") {
		t.Fatal("empty login or nil store must never claim")
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
	// 045 is the issue parking bay: un-parking to 010 is not a step back.
	for _, to := range []Stage{Actionable, Claimed, PRInProgress, CoveredByPR} {
		if IsRegression(IssueNeedsDecision, to) {
			t.Errorf("045 → %v flagged as regression", to)
		}
	}
	if IsRegression(CoveredByPR, IssueNeedsDecision) {
		t.Fatal("040 → 045 is forward")
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
func TestStoreNoteDCOSplitAndAdoptReason(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	s := NewStore("", func() time.Time { return now })
	key := Key("acme/widget", 7)
	s.NoteDCO(key, "a", 3, 2, 1, 0)
	s.NoteAdoptReason(key, "dco-rehome")
	if _, ok := s.Get(key); ok {
		t.Fatal("NoteDCO/NoteAdoptReason created a record")
	}
	s.Observe(key, ReturnedToContributor, "check-failing:dco")
	s.NoteDCO(key, "a", 3, 2, 1, 0)
	r, _ := s.Get(key)
	if t0, u, ok := r.DCOFor("a"); !ok || t0 != 3 || u != 2 || r.DCOUnsignedHive != 1 || r.DCOUnsignedOther != 1 || !r.DCOSplitKnown("a") || r.DCOSplitKnown("b") {
		t.Fatalf("cached split: %+v", r)
	}
	s.NoteDCO(key, "a", 3, 2, 2, 0)
	if r, _ = s.Get(key); r.DCOUnsignedHive != 2 || r.DCOUnsignedOther != 0 || !r.DCOSplitKnown("a") {
		t.Fatalf("hive share not updated: %+v", r)
	}
	if (Record{DCOHead: "a", DCOTotal: 3, DCOUnsigned: 2}).DCOSplitKnown("a") {
		t.Fatal("legacy record without split must not claim one")
	}
	if (Record{DCOHead: "a", DCOTotal: 3}).DCOSplitKnown("a") {
		t.Fatal("signed head has no split")
	}
	s.MarkAdoptedInPlace(key, "a", "contributor-offline", now)
	s.MarkAdopted(key, "a", 0, now)
	s.NoteAdoptReason(key, "dco-rehome")
	if r, _ = s.Get(key); r.AdoptReason != "dco-rehome" || r.AdoptMode != AdoptModeSupersede || r.AdoptedBy != 0 {
		t.Fatalf("adopt reason: %+v", r)
	}
}

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
		{TriageNeeded, true},
		{ReviewPending, true},
		{Reviewed, true},
		{Fixing, true},
		{NeedsAuthor, true},
		{ReturnedToContributor, true},
		{AdoptedByHive, true},
		{AwaitingMergeAdmission, false},
		{IssueNeedsDecision, true},
		{NeedsDecision, false},
		{ReadyForHumanMerge, false},
		{MergeQueue, false},
	} {
		if got := tc.stage.YieldsToAdoption(); got != tc.want {
			t.Errorf("%s.YieldsToAdoption() = %v, want %v", tc.stage, got, tc.want)
		}
	}
}

// hivecommons/hive#14975: a PR parked at 085 by the review swarm's
// requires_human yields to a supersede-mode adoption (the hive now owns the
// change through the superseding PR), unless a person's hold or sentinel
// keeps it parked. Every stage YieldsToAdoption yields here too.
func TestYieldsToAdoptionFor(t *testing.T) {
	hold := &Evidence{Holds: []string{"hive/hold"}}
	sentinel := &Evidence{Sentinel: true}
	clean := &Evidence{HeadFresh: true}
	for _, tc := range []struct {
		stage Stage
		ev    *Evidence
		want  bool
	}{
		{ReviewPending, nil, true},
		{ReviewPending, hold, true},
		{NeedsDecision, nil, true},
		{NeedsDecision, clean, true},
		{NeedsDecision, hold, false},
		{NeedsDecision, sentinel, false},
		{NeedsDecision, &Evidence{LaneReason: "label:needs-human"}, false},
		{NeedsDecision, &Evidence{LaneReason: "excluded-label:wip"}, false},
		{NeedsDecision, &Evidence{LaneReason: "held"}, false},
		{NeedsDecision, &Evidence{LaneReason: "contributor-lane:tests-removed", CauseReason: "the PR removes tests", Cause: CauseDecision}, false},
		{NeedsDecision, &Evidence{LaneReason: "contributor-lane:workflow-path", ManualMergeRule: true}, false},
		{NeedsDecision, &Evidence{ForkGuard: []string{"lint"}}, false},
		{NeedsDecision, &Evidence{LaneReason: "reviewer-verdict:requires_human", Verdict: VerdictRequiresHuman}, true},
		{NeedsDecision, &Evidence{LaneReason: "contributor-lane:decision-requested", Verdict: VerdictRequiresHuman}, true},
		{AwaitingMergeAdmission, nil, false},
		{ReadyForHumanMerge, nil, false},
		{MergeQueue, clean, false},
	} {
		if got := tc.stage.YieldsToAdoptionFor(tc.ev); got != tc.want {
			t.Errorf("%s.YieldsToAdoptionFor(%+v) = %v, want %v", tc.stage, tc.ev, got, tc.want)
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

// Stage-074 escape bookkeeping (hivecommons/hive#12858): the adopted-head
// clock restarts on a hive push, a failed supersession reads as nobody
// working the head, and an escalation is remembered once per head.
func TestStoreAdoptionEscapeBookkeeping(t *testing.T) {
	now := time.Date(2026, 10, 18, 14, 26, 0, 0, time.UTC)
	clock := now
	s := NewStore("", func() time.Time { return clock })
	key := Key("hivecommons/hive", 12858)
	s.Observe(key, Fixing, LaneReasonChangesRequested)
	s.MarkAdoptedInPlace(key, "5665cda", "grace-elapsed", now)
	r, _ := s.Get(key)
	if !r.AdoptedHeadSince().Equal(now) || !r.AdoptedHeadAt.Equal(now) || r.AdoptedHeadClockUnset() {
		t.Fatalf("fresh in-place adoption clock: %+v", r)
	}
	if r.AdoptionEscalated("5665cda") || r.AdoptionAttemptedNothingOpened("5665cda") {
		t.Fatalf("in-place record misread: %+v", r)
	}

	clock = now.Add(4 * time.Hour)
	s.MarkAdoptedHead(key, "5665cda")
	if r, _ = s.Get(key); !r.AdoptedHeadSince().Equal(now) {
		t.Fatalf("same head restarted the clock: %+v", r)
	}
	s.MarkAdoptedHead(key, "f72fa44")
	if r, _ = s.Get(key); !r.AdoptedHeadSince().Equal(clock) || r.AdoptedHead != "f72fa44" {
		t.Fatalf("hive push did not restart the clock: %+v", r)
	}

	s.MarkAdoptionEscalated("missing", "f72fa44", "adoption-stalled:x")
	if _, ok := s.Get("missing"); ok {
		t.Fatal("MarkAdoptionEscalated created a record")
	}
	s.MarkAdoptionEscalated(key, "", "adoption-stalled:x")
	if r, _ = s.Get(key); r.AdoptionEscalated("") || r.AdoptEscalatedHead != "" {
		t.Fatalf("empty head escalated: %+v", r)
	}
	s.MarkAdoptionEscalated(key, "f72fa44", "adoption-stalled:check-failing:unit")
	r, _ = s.Get(key)
	if !r.AdoptionEscalated("f72fa44") || r.AdoptionEscalated("5665cda") || r.AdoptEscalatedReason != "adoption-stalled:check-failing:unit" || !r.AdoptionEscalationNoted("f72fa44") {
		t.Fatalf("escalation not recorded: %+v", r)
	}
	s.dirty = false
	s.MarkAdoptionEscalated(key, "f72fa44", "adoption-stalled:check-failing:unit")
	if s.dirty {
		t.Fatal("re-marking the same escalation dirtied the store")
	}
	// hivecommons/hive#13040: the escalation releases when the reason
	// clears, but the head stays noted so a flapping blocker is noted once.
	s.ClearAdoptionEscalation(key)
	r, _ = s.Get(key)
	if r.AdoptionEscalated("f72fa44") || r.AdoptEscalatedReason != "" || !r.AdoptionEscalationNoted("f72fa44") || r.AdoptionEscalationNoted("5665cda") {
		t.Fatalf("escalation not released: %+v", r)
	}
	s.dirty = false
	s.ClearAdoptionEscalation(key)
	s.ClearAdoptionEscalation("missing")
	if s.dirty {
		t.Fatal("clearing a released escalation dirtied the store")
	}
	s.MarkAdoptionEscalated(key, "f72fa44", "adoption-stalled:check-failing:unit")
	if r, _ = s.Get(key); !r.AdoptionEscalated("f72fa44") {
		t.Fatalf("re-escalation not recorded: %+v", r)
	}

	// A supersession that opened nothing is an adoption nobody works.
	s.MarkAdopted(key, "f72fa44", 0, clock)
	if r, _ = s.Get(key); !r.AdoptionAttemptedNothingOpened("f72fa44") || r.AdoptionAttemptedNothingOpened("5665cda") {
		t.Fatalf("failed supersession misread: %+v", r)
	}
	s.MarkAdopted(key, "f72fa44", 90, clock)
	if r, _ = s.Get(key); r.AdoptionAttemptedNothingOpened("f72fa44") {
		t.Fatalf("live superseder misread: %+v", r)
	}

	// Yield and clear forget the escalation and the head clock.
	s.MarkYielded(key, "abc")
	if r, _ = s.Get(key); r.AdoptEscalatedHead != "" || r.AdoptEscalatedReason != "" || r.AdoptEscalationNotedHead != "" || !r.AdoptedHeadAt.IsZero() {
		t.Fatalf("yield kept escape state: %+v", r)
	}
	s.MarkAdoptedInPlace(key, "abc", "grace-elapsed", clock)
	s.MarkAdoptedHead(key, "abd")
	s.MarkAdoptionEscalated(key, "abd", "adoption-stalled:x")
	s.ClearAdoption(key)
	if r, _ = s.Get(key); r.AdoptEscalatedHead != "" || r.AdoptEscalationNotedHead != "" || !r.AdoptedHeadAt.IsZero() {
		t.Fatalf("clear kept escape state: %+v", r)
	}
}

// hivecommons/hive#13040: a record written before AdoptedHeadAt existed
// carries a head the hive pushed but no clock for it. The reconciler stamps
// the clock on first sight instead of reading the dwell from AdoptedAt.
func TestStoreStampAdoptedHeadAtMigratesLegacyRecord(t *testing.T) {
	adoptedAt := time.Date(2026, 10, 18, 14, 26, 0, 0, time.UTC)
	clock := adoptedAt.Add(28 * time.Hour)
	s := NewStore("", func() time.Time { return clock })
	key := Key("hivecommons/hive", 12858)
	s.Observe(key, AdoptedByHive, "optional-check-failing:overlap")
	legacy, _ := s.Get(key)
	legacy.AdoptedHead, legacy.AdoptMode, legacy.AdoptReason = "92fba9a", AdoptModeInPlace, "grace-elapsed"
	legacy.AdoptedAt, legacy.AdoptCheckedAt = adoptedAt, adoptedAt
	legacy.HivePushedHeads = []string{"92fba9a"}
	data, err := json.Marshal(map[string]Record{key: legacy})
	if err != nil {
		t.Fatal(err)
	}
	var items map[string]Record
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatal(err)
	}
	s.items = items
	r, _ := s.Get(key)
	if !r.AdoptedHeadClockUnset() || !r.AdoptedHeadSince().Equal(adoptedAt) {
		t.Fatalf("legacy record misread: %+v", r)
	}
	if s.StampAdoptedHeadAt(key, "other") || s.StampAdoptedHeadAt(key, "") || s.StampAdoptedHeadAt("missing", "92fba9a") {
		t.Fatal("stamped the wrong record or head")
	}
	if !s.StampAdoptedHeadAt(key, "92fba9a") {
		t.Fatal("legacy record not stamped")
	}
	r, _ = s.Get(key)
	if r.AdoptedHeadClockUnset() || !r.AdoptedHeadSince().Equal(clock) || !r.AdoptedAt.Equal(adoptedAt) {
		t.Fatalf("stamp: %+v", r)
	}
	s.dirty = false
	if s.StampAdoptedHeadAt(key, "92fba9a") || s.dirty {
		t.Fatal("re-stamped a live clock")
	}
	// A supersession has no head clock to stamp.
	s.MarkAdopted(key, "92fba9a", 0, clock)
	if r, _ = s.Get(key); r.AdoptedHeadClockUnset() || s.StampAdoptedHeadAt(key, "92fba9a") {
		t.Fatalf("supersession stamped: %+v", r)
	}
}

// 095 sits between 090 and 100, is PR-only with a stall note, and a 100 → 095
// re-evaluation (the lane re-reports a still-soaking head) is a
// reclassification, not a regression — while leaving 095 for a fix is.
func TestPreMergeSoakStage(t *testing.T) {
	if got := PreMergeSoak.Label(); got != "stage/095-pre-merge-soak" {
		t.Fatalf("label = %q", got)
	}
	if !ReadyForHumanMerge.Before(PreMergeSoak) || !PreMergeSoak.Before(MergeQueue) || !AwaitingMergeAdmission.Before(PreMergeSoak) {
		t.Fatal("095 must order between 080/090 and 100")
	}
	if !PreMergeSoak.StallNote() || !PreMergeSoak.Applies(PRItem) || PreMergeSoak.Applies(IssueItem) {
		t.Fatalf("095 flags: stall=%v pr=%v issue=%v", PreMergeSoak.StallNote(), PreMergeSoak.Applies(PRItem), PreMergeSoak.Applies(IssueItem))
	}
	if PreMergeSoak.Dwell() != PreMergeSoakDwell || PreMergeSoakDwell != 45*time.Minute {
		t.Fatalf("dwell = %s", PreMergeSoak.Dwell())
	}
	if IsRegression(MergeQueue, PreMergeSoak) {
		t.Fatal("100 → 095 must not be a regression")
	}
	for _, to := range []Stage{Fixing, Reviewed, ReviewPending, AwaitingMergeAdmission} {
		if !IsRegression(PreMergeSoak, to) {
			t.Fatalf("095 → %s must be a regression", to)
		}
	}
	if !IsRegression(MergeQueue, AwaitingMergeAdmission) {
		t.Fatal("100 → 080 stays a regression")
	}

	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	st := NewStore(filepath.Join(t.TempDir(), "s.json"), func() time.Time { return now })
	key := Key("o/r", 13020)
	st.Observe(key, AwaitingMergeAdmission, "contributor-lane:no-decision")
	if tr := st.Observe(key, PreMergeSoak, "contributor-lane:soaking(remaining=30m0s)"); !tr.Changed || tr.Regressed {
		t.Fatalf("080 → 095: %+v", tr)
	}
	if tr := st.Observe(key, MergeQueue, "contributor-lane:merge-eligible"); !tr.Changed || tr.Regressed {
		t.Fatalf("095 → 100: %+v", tr)
	}
	if tr := st.Observe(key, PreMergeSoak, "contributor-lane:soaking(remaining=1m0s)"); !tr.Changed || tr.Regressed {
		t.Fatalf("100 → 095 must be a plain move, got %+v", tr)
	}
	rec, _ := st.Get(key)
	if rec.RegressedFrom != None {
		t.Fatalf("100 → 095 must not record a regression: %+v", rec)
	}

	c := StallComment(PreMergeSoak, PreMergeSoakDwell, "contributor-lane:soaking(remaining=0s)")
	if !strings.Contains(c, "stage 095-pre-merge-soak: no activity for 45m0s") || !strings.Contains(c, PreMergeSoakStallNote) {
		t.Fatalf("stall comment = %q", c)
	}
	if c := StallComment(MergeQueue, time.Hour, "x"); strings.Contains(c, PreMergeSoakStallNote) {
		t.Fatalf("100 stall must not carry the soak note: %q", c)
	}

	for _, tc := range []struct {
		soak, elapsed time.Duration
		want          string
	}{
		{30 * time.Minute, 12 * time.Minute, "soaking: admitted, 12m0s of 30m0s elapsed"},
		{30 * time.Minute, 45 * time.Minute, "soaking: admitted, 30m0s of 30m0s elapsed"},
		{30 * time.Minute, -time.Minute, "soaking: admitted, 0s of 30m0s elapsed"},
		{0, time.Minute, "soaking: admitted, no soak window"},
	} {
		if got := PreMergeSoakSummary(tc.soak, tc.elapsed); got != tc.want {
			t.Fatalf("PreMergeSoakSummary(%s,%s) = %q, want %q", tc.soak, tc.elapsed, got, tc.want)
		}
	}
}

// TestTriageNeededStage: 015 is an issue-only tracking stage between 010
// and 020 (hivecommons/hive#13268). A maintainer triaging the issue moves
// it 015 → 010, which is lateral, never a regression; 010 → 015 is forward
// (the awaiting label landed); falling back from a claim / PR to 015 is a
// regression exactly like falling back to 010.
func TestTriageNeededStage(t *testing.T) {
	if TriageNeeded.Number() != 15 || TriageNeeded.Slug() != "triage-needed" || TriageNeeded.String() != "015-triage-needed" {
		t.Fatalf("015 identity: %d %q %q", TriageNeeded.Number(), TriageNeeded.Slug(), TriageNeeded.String())
	}
	if !TriageNeeded.Applies(IssueItem) || TriageNeeded.Applies(PRItem) {
		t.Fatal("015 must be issue-only")
	}
	if !Actionable.Before(TriageNeeded) || !TriageNeeded.Before(Claimed) {
		t.Fatal("015 sits between 010 and 020")
	}
	if TriageNeeded.Dwell() != TriageNeededDwell || TriageNeededDwell != Actionable.Dwell() {
		t.Fatalf("015 dwell %v, want the 010 dwell %v", TriageNeeded.Dwell(), Actionable.Dwell())
	}
	if _, ok := StageBySlug("triage-needed"); !ok {
		t.Fatal("StageBySlug(triage-needed)")
	}
	found := false
	for _, d := range AllLabelDefs() {
		if d.Name == TriageNeeded.Label() {
			found = true
			if d.Color != StageColor || d.Description == "" || len(d.Description) > 100 {
				t.Fatalf("015 label def %+v", d)
			}
		}
	}
	if !found {
		t.Fatal("AllLabelDefs lacks stage/015-triage-needed")
	}
	for _, to := range []Stage{Actionable, Claimed, PRInProgress, CoveredByPR, IssueNeedsDecision} {
		if IsRegression(TriageNeeded, to) {
			t.Errorf("015 → %v flagged as regression", to)
		}
	}
	if IsRegression(Actionable, TriageNeeded) {
		t.Fatal("010 → 015 is forward")
	}
	for _, from := range []Stage{PRInProgress, CoveredByPR} {
		if !IsRegression(from, TriageNeeded) {
			t.Errorf("%v → 015 must be a regression, like → 010", from)
		}
	}
	if IsRegression(Claimed, TriageNeeded) {
		t.Fatal("020 → 015 is a lapsed reservation, not a regression (like → 010)")
	}
	if IsRegression(IssueNeedsDecision, TriageNeeded) {
		t.Fatal("un-parking 045 → 015 is not a step back")
	}
}

// hivecommons/hive#14471: the supersession mark and the poll gate it closes.
func TestStoreMarkSupersededAndPollDue(t *testing.T) {
	now := time.Date(2026, 10, 26, 22, 0, 0, 0, time.UTC)
	s := NewStore("", func() time.Time { return now })
	key := Key("acme/widget", 7)
	s.Observe(key, ReturnedToContributor, "check-failing:unit")
	r, _ := s.Get(key)
	if r.SupersederPollDue(now, 10*time.Minute) {
		t.Fatal("poll due with no superseder on record")
	}
	s.MarkAdopted(key, "a", 90, now)
	r, _ = s.Get(key)
	if !r.SupersederPollDue(now.Add(10*time.Minute), 10*time.Minute) || r.SupersederPollDue(now.Add(time.Minute), 10*time.Minute) {
		t.Fatalf("poll gate: %+v", r)
	}
	s.MarkSuperseded(key, 90, true, now)
	r, _ = s.Get(key)
	if !r.Superseded() || r.SupersededBy != 90 || !r.SupersededOpen || !r.SupersededAt.Equal(now) {
		t.Fatalf("superseded open: %+v", r)
	}
	if r.SupersederPollDue(now.Add(time.Hour), 10*time.Minute) {
		t.Fatal("poll due after supersession")
	}
	// The close succeeding later flips open off without moving the clock.
	s.MarkSuperseded(key, 90, false, now.Add(time.Minute))
	r, _ = s.Get(key)
	if r.SupersededOpen || !r.SupersededAt.Equal(now) {
		t.Fatalf("superseded closed: %+v", r)
	}
	s.MarkSupersederMergedNoted(key, "b")
	r, _ = s.Get(key)
	if r.SupersederMergedNotedHead != "b" {
		t.Fatalf("noted head: %+v", r)
	}
	s.ClearAdoption(key)
	r, _ = s.Get(key)
	if r.Superseded() || r.SupersederMergedNotedHead != "" || r.SupersededOpen {
		t.Fatalf("ClearAdoption kept supersession: %+v", r)
	}
	s.MarkSuperseded(key, 0, false, now)
	if r, _ = s.Get(key); r.Superseded() {
		t.Fatal("MarkSuperseded accepted PR 0")
	}
}
