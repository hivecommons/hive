package automerge

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// An owner's Approve on an untrusted-author PR is a trust override for that
// head (hivecommons/hive#15136): the dashboard writes admitted_by on the
// stage record, and a due adoption stands down instead of superseding the
// PR the person just approved. The row shows why (operator-admission); an
// explicit Adopt request from the same dashboard is the person choosing
// re-authoring and goes ahead.
func TestOperatorAdmissionRefusesAdoption(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	key := stageKey("acme", "widget", 7)
	store := h.e.stageStoreOrShared()
	if !store.NoteAdmission(key, "aaa", stagelabels.OperatorApprovalAdmission("octo"), h.clock.now()) {
		t.Fatalf("NoteAdmission refused a known key")
	}
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.api.reset()
	h.clock.advance(5 * time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	rec, _ := store.Get(key)
	if len(h.adopt.pulls) != 0 || len(adoptionComments(h.api)) != 0 || rec.AdoptedHead != "" {
		t.Fatalf("adopted an operator-admitted head: pulls=%d comments=%v rec=%+v", len(h.adopt.pulls), h.api.comments, rec)
	}
	if got := rec.AdoptionBlocked("aaa"); got != stagelabels.AdoptBlockedOperatorAdmission {
		t.Fatalf("blocked reason = %q, want %q (%+v)", got, stagelabels.AdoptBlockedOperatorAdmission, rec)
	}
	if got := rec.Admission("aaa"); got != "operator-approval:octo" {
		t.Fatalf("admission = %q", got)
	}
	if rec.Admission("bbb") != "" {
		t.Fatalf("admission leaked to another head: %+v", rec)
	}
	// The person explicitly asks for re-authoring: not refused.
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", append(labels, delegatedecision.LabelAdoptSupersede)...), "check-failing:unit")
	if rec, _ = store.Get(key); rec.AdoptedHead != "aaa" {
		t.Fatalf("explicit Adopt request refused by admission: %+v", rec)
	}
}

// Without the dashboard's record, a `/hive decision approve` by a trusted
// merger is the same admission: the engine derives it from the recorded
// human approval and writes it back so the row shows it. An approval by
// someone the merger authorizer does not trust admits nothing.
func TestOperatorAdmissionDerivedFromHumanApproval(t *testing.T) {
	for _, tc := range []struct {
		name    string
		actor   string
		trusted bool
		want    string
	}{
		{name: "trusted merger approves", actor: "Octo", trusted: true, want: "octo"},
		{name: "untrusted approver", actor: "mallory", trusted: false, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
			defer done()
			h.e.humanChangeRequests = review.NewHumanChangeRequests("")
			h.e.SetMergerAuthorizer(func(login string) bool { return tc.trusted })
			if err := h.e.humanChangeRequests.Record(review.HumanChangeRequest{Repo: "acme/widget", Number: 7, HeadSHA: "aaa", Actor: tc.actor, Kind: review.HumanChangeRequestKindApprove, SourceID: 1}); err != nil {
				t.Fatal(err)
			}
			h.observe(adoptPR(7, "aaa"), "check-failing:unit")
			if got := h.e.operatorAdmission("acme", "widget", 7, "aaa"); got != tc.want {
				t.Fatalf("operatorAdmission = %q, want %q", got, tc.want)
			}
			rec, _ := h.e.stageStoreOrShared().Get(stageKey("acme", "widget", 7))
			wantRec := ""
			if tc.want != "" {
				wantRec = stagelabels.OperatorApprovalAdmission(tc.want)
			}
			if rec.Admission("aaa") != wantRec {
				t.Fatalf("record admission = %q, want %q", rec.Admission("aaa"), wantRec)
			}
		})
	}
}

// Nothing admitted: a nil engine and an unknown head both answer "" and
// the adoption refusal is a no-op.
func TestOperatorAdmissionAbsent(t *testing.T) {
	var nilEngine *Engine
	if got := nilEngine.operatorAdmission("acme", "widget", 7, "aaa"); got != "" {
		t.Fatalf("nil engine admitted %q", got)
	}
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	if got := h.e.operatorAdmission("acme", "widget", 7, "aaa"); got != "" {
		t.Fatalf("admitted without approval: %q", got)
	}
	if got := h.e.operatorAdmission("acme", "widget", 7, ""); got != "" {
		t.Fatalf("empty head admitted: %q", got)
	}
}

// An approval on a head the lane cannot merge as-is (the fork is not
// editable and sits behind the base) must not lock the PR into "merge
// as-is": that deadlocked hive#15312 and friends between "a maintainer
// approved this head; the hive merges it as-is" and "fork-not-editable".
// Approval means approved to proceed — the hive adopts, and the approval
// travels to its copy.
func TestOperatorAdmissionDoesNotBlockUnmergeableHead(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.e.humanChangeRequests = review.NewHumanChangeRequests("")
	h.observe(adoptPR(7, "aaa"), autoMergeReasonForkNotEditable)
	key := stageKey("acme", "widget", 7)
	store := h.e.stageStoreOrShared()
	if !store.NoteAdmission(key, "aaa", stagelabels.OperatorApprovalAdmission("octo"), h.clock.now()) {
		t.Fatalf("NoteAdmission refused a known key")
	}
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.api.reset()
	h.clock.advance(5 * time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), autoMergeReasonForkNotEditable)
	rec, _ := store.Get(key)
	if rec.AdoptedHead != "aaa" || rec.AdoptedBy == 0 || len(h.adopt.pulls) != 1 {
		t.Fatalf("approved fork-not-editable head not adopted: pulls=%d rec=%+v", len(h.adopt.pulls), rec)
	}
	if got := rec.AdoptionBlocked("aaa"); got != "" {
		t.Fatalf("adoption blocked %q on a head the lane cannot merge as-is", got)
	}
	if body, _ := h.adopt.pulls[0]["body"].(string); !strings.Contains(body, "**Approval carried over:** @octo approved #7") {
		t.Fatalf("copy body lacks the carried approval:\n%s", body)
	}
	req, ok := h.e.humanChangeRequests.For("acme/widget", rec.AdoptedBy, "aaa")
	if !ok || !req.IsApproval() || req.Actor != "octo" {
		t.Fatalf("approval not carried to copy #%d: %+v ok=%v", rec.AdoptedBy, req, ok)
	}
}

// A mergeable admitted head still stands down (the person merges it as-is);
// the lane reasons that make a head unmergeable as-is are named.
func TestHeadNotMergeableAsIs(t *testing.T) {
	for _, tc := range []struct {
		name string
		obs  stageObservation
		want string
	}{
		{name: "check failing only", obs: stageObservation{Reason: "check-failing:unit"}, want: ""},
		{name: "fork not editable", obs: stageObservation{Reason: autoMergeReasonForkNotEditable}, want: autoMergeReasonForkNotEditable},
		{name: "lane reason conflicting", obs: stageObservation{Reason: "decision-requested", LaneReason: "conflicting"}, want: "conflicting"},
		{name: "needs-rebase label", obs: stageObservation{Labels: []string{"needs-rebase"}}, want: "label:needs-rebase"},
		{name: "dco missing", obs: stageObservation{Detail: stageDetail{DCO: stagelabels.DCONo}}, want: stagelabels.LaneReasonDCOMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := headNotMergeableAsIs(tc.obs); got != tc.want {
				t.Fatalf("headNotMergeableAsIs = %q, want %q", got, tc.want)
			}
		})
	}
}

// Approve, then Adopt now: the forced adoption wins. The recorded
// "operator-admission" blocker clears the moment Adopt now is recorded (the
// row stops saying "not adopted") and the next pass supersedes the PR.
func TestForcedAdoptionOverridesAdmission(t *testing.T) {
	h, done := newAdoptHarness(t, ContributorNudgeOutcome{Mode: "comment"}, adoptPolicy())
	defer done()
	h.observe(adoptPR(7, "aaa"), "check-failing:unit")
	key := stageKey("acme", "widget", 7)
	store := h.e.stageStoreOrShared()
	store.NoteAdmission(key, "aaa", stagelabels.OperatorApprovalAdmission("octo"), h.clock.now())
	labels := []string{"stage/072-returned-to-contributor", "role/contributor", "author/clanker"}
	h.api.reset()
	h.clock.advance(5 * time.Hour)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	if rec, _ := store.Get(key); rec.AdoptionBlocked("aaa") != stagelabels.AdoptBlockedOperatorAdmission || rec.AdoptedHead != "" {
		t.Fatalf("precondition: mergeable admitted head should be blocked, got %+v", rec)
	}
	store.NoteAdoptionForced(key, "octo", h.clock.now())
	if rec, _ := store.Get(key); rec.AdoptionBlocked("aaa") != "" {
		t.Fatalf("Adopt now left the admission blocker standing: %+v", rec)
	}
	h.api.reset()
	h.clock.advance(time.Minute)
	h.observe(adoptPR(7, "aaa", labels...), "check-failing:unit")
	rec, _ := store.Get(key)
	if rec.AdoptedHead != "aaa" || rec.AdoptedBy == 0 || rec.AdoptReason != AdoptReasonOperatorForced {
		t.Fatalf("forced adoption did not run over the admission: %+v", rec)
	}
	if _, pending := rec.AdoptionForced(); pending {
		t.Fatalf("forced adoption still pending after it ran: %+v", rec)
	}
}
