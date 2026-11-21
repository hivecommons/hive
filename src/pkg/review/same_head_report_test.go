package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const sameHead = "48b9e51d43185ed8a038e2cec2b379240e3c7e14"

func sameHeadReport(verdict Verdict, head string) PerspectiveReport {
	return PerspectiveReport{Perspective: PerspectiveSecurity, Verdict: verdict, Repo: "o/r", Number: 376, HeadSHA: head, ReporterAgent: "reviewer"}
}

func dispatchedAt(r PerspectiveReport, at time.Time) PerspectiveReport {
	r.DispatchedAt = at
	return r
}

// A later, less strict report for the same head does not replace a stricter
// one unless its review was dispatched after the stricter one arrived
// (projectbluefin/dakota-iso#376).
func TestKeepsPriorReport(t *testing.T) {
	received := time.Date(2026, 11, 21, 18, 43, 7, 0, time.UTC)
	before := received.Add(-29 * time.Second) // the kick planned before the verdict existed
	after := received.Add(5 * time.Minute)    // a revisit planned in view of the verdict
	other := "0000000000000000000000000000000000000001"
	cases := []struct {
		name       string
		prior      PerspectiveReport
		next       PerspectiveReport
		dispatched time.Time
		want       bool
	}{
		{"approve after requires_human, kick planned before it: kept", sameHeadReport(VerdictRequiresHuman, sameHead), sameHeadReport(VerdictApprove, sameHead), before, true},
		{"approve after changes_requested, kick planned before it: kept", sameHeadReport(VerdictChangesRequested, sameHead), sameHeadReport(VerdictApprove, sameHead), before, true},
		{"changes_requested after reject: kept", sameHeadReport(VerdictReject, sameHead), sameHeadReport(VerdictChangesRequested, sameHead), before, true},
		{"advisory approve (no dispatch): kept", sameHeadReport(VerdictRequiresHuman, sameHead), sameHeadReport(VerdictApprove, sameHead), time.Time{}, true},
		{"abbreviated same head: kept", sameHeadReport(VerdictRequiresHuman, sameHead), sameHeadReport(VerdictApprove, sameHead[:7]), before, true},
		{"deliberate re-review dispatched after the verdict: replaced", sameHeadReport(VerdictRequiresHuman, sameHead), sameHeadReport(VerdictApprove, sameHead), after, false},
		{"new head: replaced", sameHeadReport(VerdictRequiresHuman, sameHead), sameHeadReport(VerdictApprove, other), before, false},
		{"prior approve: replaced", sameHeadReport(VerdictApprove, sameHead), sameHeadReport(VerdictRequiresHuman, sameHead), before, false},
		{"stricter next: replaced", sameHeadReport(VerdictChangesRequested, sameHead), sameHeadReport(VerdictRequiresHuman, sameHead), before, false},
		{"same verdict again: replaced", sameHeadReport(VerdictRequiresHuman, sameHead), sameHeadReport(VerdictRequiresHuman, sameHead), before, false},
		{"prior names no head: replaced", sameHeadReport(VerdictRequiresHuman, ""), sameHeadReport(VerdictApprove, ""), before, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := KeepsPriorReport(tc.prior, received, dispatchedAt(tc.next, tc.dispatched)); got != tc.want {
				t.Fatalf("KeepsPriorReport = %v, want %v", got, tc.want)
			}
		})
	}
}

// A requires_human the reviewer wrote because a rate limit kept it from
// reading the PR is no verdict, and never holds a later review out.
func TestKeepsPriorReportIgnoresUnreadablePrior(t *testing.T) {
	prior := sameHeadReport(VerdictRequiresHuman, sameHead)
	prior.Summary = "requires human: could not read the delta: every GitHub read returned HTTP 429 (rate limit). Not reviewed."
	if _, unread := UnreadableReviewReason(prior); !unread {
		t.Fatalf("fixture is not an unreadable report")
	}
	if KeepsPriorReport(prior, time.Now(), sameHeadReport(VerdictApprove, sameHead)) {
		t.Fatal("an unreadable requires_human held a later review out")
	}
}

// A reviewer cannot soften its own verdict on a head, not even under the
// same dispatch time: the relay cannot tell two reports from one kick apart
// from the same agent's queue-mode session and its swarm kick on one head.
func TestKeepsPriorReportSameAgentSameDispatchStillKept(t *testing.T) {
	received := time.Date(2026, 11, 21, 18, 43, 7, 0, time.UTC)
	dispatch := received.Add(-time.Minute)
	prior := dispatchedAt(sameHeadReport(VerdictRequiresHuman, sameHead), dispatch)
	if !KeepsPriorReport(prior, received, dispatchedAt(sameHeadReport(VerdictApprove, sameHead), dispatch)) {
		t.Fatal("the same agent's approve under the same dispatch time cleared its requires_human")
	}
	changes := dispatchedAt(sameHeadReport(VerdictChangesRequested, sameHead), dispatch)
	if !KeepsPriorReport(changes, received, dispatchedAt(sameHeadReport(VerdictApprove, sameHead), dispatch)) {
		t.Fatal("the same agent's approve under the same dispatch time cleared its changes_requested")
	}
}

// ConfirmDelivered carries the pending row's planning time onto the recent
// row, so a verdict that binds the recent row after the pending one is gone
// is still known as a review asked for at that time.
func TestConfirmDeliveredCarriesDispatchTimeToRecentRows(t *testing.T) {
	planned := time.Date(2026, 11, 21, 18, 42, 38, 0, time.UTC)
	k := DispatchKick{Agent: "reviewer-2", Kind: "review", Repo: "o/r", Number: 376, HeadSHA: sameHead, Perspective: PerspectiveCorrectness, Perspectives: []Perspective{PerspectiveCorrectness, PerspectiveSecurity}}
	state := DispatchState{Pending: []PendingReview{
		{Repo: "o/r", Number: 376, HeadSHA: sameHead, Perspective: PerspectiveCorrectness, Agent: "reviewer-2", Dispatched: planned},
		{Repo: "o/r", Number: 376, HeadSHA: sameHead, Perspective: PerspectiveSecurity, Agent: "reviewer-2", Dispatched: planned},
	}}
	got := ConfirmDelivered(state, []DispatchKick{k}, []DispatchKick{k})
	if len(got.Recent) != 2 {
		t.Fatalf("recent = %+v, want one row per perspective", got.Recent)
	}
	for _, r := range got.Recent {
		if !r.Dispatched.Equal(planned) || r.Confirmed.IsZero() {
			t.Fatalf("recent row %+v, want dispatched_at %v and a confirmation", r, planned)
		}
		if !RecentDispatchedAt(r).Equal(planned) {
			t.Fatalf("RecentDispatchedAt = %v, want %v", RecentDispatchedAt(r), planned)
		}
		if !PendingDelivered(got, state.Pending[0]) {
			t.Fatal("delivered pending row not recognised as delivered")
		}
	}
	for _, p := range got.Pending {
		if p.Delivered.IsZero() {
			t.Fatalf("pending row %+v kept no delivery time", p)
		}
		// The row alone proves delivery, after the recent rows age out.
		if !PendingDelivered(DispatchState{}, p) {
			t.Fatalf("pending row %+v with delivered_at not recognised as delivered", p)
		}
	}
	// A row written before dispatched_at existed fails closed: zero, never
	// its delivery time, which may postdate a verdict the kick was planned
	// before.
	if legacy := (RecentReview{Confirmed: planned.Add(time.Minute)}); !RecentDispatchedAt(legacy).IsZero() {
		t.Fatal("a recent row without dispatched_at must read as no dispatch time")
	}
	if PendingDelivered(DispatchState{}, state.Pending[0]) {
		t.Fatal("a row with no confirmation read as delivered")
	}
}

// An undelivered kick's rows are released, and an undelivered row never
// reads as delivered.
func TestConfirmDeliveredMarksOnlyDeliveredRows(t *testing.T) {
	planned := time.Date(2026, 11, 21, 18, 42, 38, 0, time.UTC)
	sent := DispatchKick{Agent: "r1", Kind: "review", Repo: "o/r", Number: 1, HeadSHA: sameHead, Perspective: PerspectiveSecurity}
	deferred := DispatchKick{Agent: "r2", Kind: "review", Repo: "o/r", Number: 2, HeadSHA: sameHead, Perspective: PerspectiveSecurity}
	state := DispatchState{Pending: []PendingReview{
		{Repo: "o/r", Number: 1, HeadSHA: sameHead, Perspective: PerspectiveSecurity, Agent: "r1", Dispatched: planned},
		{Repo: "o/r", Number: 2, HeadSHA: sameHead, Perspective: PerspectiveSecurity, Agent: "r2", Dispatched: planned},
	}}
	got := ConfirmDeliveredInProgress(state, []DispatchKick{sent})
	for _, p := range got.Pending {
		if (p.Number == 1) == p.Delivered.IsZero() {
			t.Fatalf("pending row %+v: delivered_at wrong for its kick", p)
		}
	}
	got = ConfirmDelivered(got, []DispatchKick{sent, deferred}, []DispatchKick{sent})
	if len(got.Pending) != 1 || got.Pending[0].Number != 1 {
		t.Fatalf("pending = %+v, want only the delivered kick's row", got.Pending)
	}
}

// Only live rows of the dispatch reach the queue-mode mark.
func TestLiveReviewsDropsExpiredAndUndatedRows(t *testing.T) {
	now := time.Date(2026, 11, 21, 19, 0, 0, 0, time.UTC)
	plan := &DispatchPlan{State: DispatchState{Pending: []PendingReview{
		{Repo: "o/r", Number: 1, HeadSHA: "a1", Agent: "r1", Dispatched: now.Add(-time.Minute)},
		{Repo: "o/r", Number: 2, HeadSHA: "b2", Agent: "r1", Dispatched: now.Add(-2 * time.Hour)},
		{Repo: "o/r", Number: 3, HeadSHA: "c3", Agent: "r1"},
	}}}
	got := plan.LiveReviews(0, now)
	if len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("LiveReviews = %+v, want only #1", got)
	}
}

func writeRecordedReport(t *testing.T, dir, name string, r PerspectiveReport) string {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecordedReportsForReadsPlainReportsOfThePlannedPRs(t *testing.T) {
	dir := t.TempDir()
	plain := PerspectiveReport{Perspective: PerspectiveSecurity, Verdict: VerdictRequiresHuman, Repo: "acme/hive", Number: 7, HeadSHA: "sha1"}
	writeRecordedReport(t, dir, ReportFilePrefix("acme/hive", 7)+"security"+ReviewReportFileSuffix, plain)
	headReport := plain
	headReport.Perspective = PerspectiveStyle
	headReport.Head = "cr-sol"
	writeRecordedReport(t, dir, ReportFilePrefix("acme/hive", 7)+"style-cr-sol"+ReviewReportFileSuffix, headReport)
	otherPR := plain
	otherPR.Number = 8
	writeRecordedReport(t, dir, ReportFilePrefix("acme/hive", 8)+"security"+ReviewReportFileSuffix, otherPR)

	got := RecordedReportsFor(dir, []PullRequest{{Repo: "acme/hive", Number: 7}})
	if len(got) != 1 || got[0].Perspective != PerspectiveSecurity || got[0].Verdict != VerdictRequiresHuman || got[0].HeadSHA != "sha1" || got[0].ReceivedAt.IsZero() {
		t.Fatalf("RecordedReportsFor = %+v, want only the plain security report of #7", got)
	}
	if got := RecordedReportsFor(filepath.Join(dir, "missing"), []PullRequest{{Repo: "acme/hive", Number: 7}}); got != nil {
		t.Fatalf("missing dir = %+v, want nil", got)
	}
}

func recordedForAll(at time.Time, head string) []RecordedReport {
	var out []RecordedReport
	for _, p := range DefaultPerspectives {
		v := VerdictApprove
		if p == PerspectiveSecurity {
			v = VerdictRequiresHuman
		}
		out = append(out, RecordedReport{Repo: "acme/hive", Number: 7, HeadSHA: head, Perspective: p, Verdict: v, ReceivedAt: at})
	}
	return out
}

func sameHeadDispatchOpts(now time.Time) DispatchOptions {
	return DispatchOptions{
		RequireApproval: true, FanOut: true, MaxParallelReviews: 5, CombinedPerspectives: true,
		Agents: []AgentCapability{reviewer("r1"), reviewer("r2")}, ProjectOrg: "acme", Now: now,
	}
}

// A head whose verdict the relay recorded after this cycle's collect is not
// asked again: the reports count as delivered until the next collect folds
// them into the artifact.
func TestPlanDispatchDoesNotReplanRecordedButUnaggregatedHead(t *testing.T) {
	now := time.Date(2026, 11, 21, 18, 44, 0, 0, time.UTC)
	opts := sameHeadDispatchOpts(now)
	opts.RecordedReports = recordedForAll(now.Add(-time.Minute), "sha1")
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 0 || len(plan.State.Pending) != 0 {
		t.Fatalf("recorded-but-unaggregated head was planned again: kicks=%+v pending=%+v", plan.ReviewKicks, plan.State.Pending)
	}
}

func TestPlanDispatchAsksOnlyThePerspectivesNotRecorded(t *testing.T) {
	now := time.Date(2026, 11, 21, 18, 44, 0, 0, time.UTC)
	opts := sameHeadDispatchOpts(now)
	opts.RecordedReports = []RecordedReport{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Verdict: VerdictRequiresHuman, ReceivedAt: now.Add(-time.Minute)}}
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("kicks = %d, want 1", len(plan.ReviewKicks))
	}
	for _, p := range plan.ReviewKicks[0].Perspectives {
		if p == PerspectiveSecurity {
			t.Fatalf("recorded perspective asked again: %v", plan.ReviewKicks[0].Perspectives)
		}
	}
	if got := len(plan.ReviewKicks[0].Perspectives); got != len(DefaultPerspectives)-1 {
		t.Fatalf("kick covers %d perspectives, want %d", got, len(DefaultPerspectives)-1)
	}
}

// The recorded reports bound the wait to one pending TTL, a new head is not
// covered by the old head's reports, and another body digest is a different
// review.
func TestPlanDispatchRecordedReportsDoNotCoverOtherHeadsOrOldReports(t *testing.T) {
	now := time.Date(2026, 11, 21, 18, 44, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		recorded []RecordedReport
		pr       PullRequest
	}{
		"older than the pending TTL": {recordedForAll(now.Add(-DefaultPendingReviewTTL-time.Minute), "sha1"), dispatchPR("sha1")},
		"new head":                   {recordedForAll(now.Add(-time.Minute), "sha1"), dispatchPR("sha2")},
		"other body digest": {recordedForAll(now.Add(-time.Minute), "sha1"), func() PullRequest {
			pr := dispatchPR("sha1")
			pr.BodyDigest = "edited"
			return pr
		}()},
	} {
		t.Run(name, func(t *testing.T) {
			opts := sameHeadDispatchOpts(now)
			opts.RecordedReports = tc.recorded
			plan := PlanDispatch([]PullRequest{tc.pr}, Artifact{}, DispatchState{}, opts)
			if len(plan.ReviewKicks) != 1 || len(plan.ReviewKicks[0].Perspectives) != len(DefaultPerspectives) {
				t.Fatalf("kicks = %+v, want one kick for every perspective", plan.ReviewKicks)
			}
		})
	}
}

// A review in flight for the head (its pending rows) is never planned a
// second time, whichever reviewer is free.
func TestPlanDispatchDoesNotReplanHeadWithReviewInFlight(t *testing.T) {
	now := time.Date(2026, 11, 21, 18, 42, 38, 0, time.UTC)
	var pending []PendingReview
	for _, p := range DefaultPerspectives {
		pending = append(pending, PendingReview{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: p, Agent: "r1", Dispatched: now.Add(-5 * time.Minute)})
	}
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{Pending: pending}, sameHeadDispatchOpts(now))
	if len(plan.ReviewKicks) != 0 {
		t.Fatalf("head with a review in flight was planned again: %+v", plan.ReviewKicks)
	}
}

// Reports recorded after the collect cover only their perspectives: the rest
// still get a delta re-review against the earlier judged head, and the
// trivial restamp, which would race the recorded reports, is not used.
func TestPlanDispatchRecordedReportsKeepDeltaReviewForTheRest(t *testing.T) {
	now := time.Date(2026, 11, 21, 18, 44, 0, 0, time.UTC)
	opts := sameHeadDispatchOpts(now)
	opts.DeltaRereview = true
	opts.RecordedReports = []RecordedReport{{Repo: "acme/hive", Number: 7, HeadSHA: "sha2", Perspective: PerspectiveSecurity, Verdict: VerdictApprove, ReceivedAt: now.Add(-time.Minute)}}
	pr := dispatchPR("sha2")
	pr.DeltaDiff = knownDelta("sha1") // identical tree: a restamp without the recorded report
	artifact := Artifact{Items: []Aggregate{priorAggregate("sha1", VerdictApprove, now.Add(-time.Hour))}}
	plan := PlanDispatch([]PullRequest{pr}, artifact, DispatchState{}, opts)
	if len(plan.DeltaRestamps) != 0 {
		t.Fatalf("restamp planned over recorded reports: %+v", plan.DeltaRestamps)
	}
	if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].DeltaPreviousHead != "sha1" {
		t.Fatalf("kicks = %+v, want one delta kick against sha1", plan.ReviewKicks)
	}
	for _, p := range plan.ReviewKicks[0].Perspectives {
		if p == PerspectiveSecurity {
			t.Fatalf("recorded perspective asked again: %v", plan.ReviewKicks[0].Perspectives)
		}
	}
}
