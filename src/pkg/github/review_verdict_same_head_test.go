package github

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/review"
)

const sameHeadSHA = "48b9e51d43185ed8a038e2cec2b379240e3c7e14"

func sameHeadPending(head string, dispatched time.Time) review.PendingReview {
	return review.PendingReview{Repo: "o/r", Number: 376, HeadSHA: head, Perspective: review.PerspectiveSecurity, Agent: "reviewer", Dispatched: dispatched}
}

// confirmedRecent is the recent-dispatch row ConfirmDelivered writes when
// p's kick reaches its agent.
func confirmedRecent(p review.PendingReview) review.RecentReview {
	return review.RecentReview{Repo: p.Repo, Number: p.Number, HeadSHA: p.HeadSHA, BodyDigest: p.BodyDigest, Perspective: p.Perspective,
		Agent: p.Agent, Head: p.Head, Decision: p.Decision, Dispatched: p.Dispatched, Confirmed: p.Dispatched.Add(10 * time.Second)}
}

func recordSameHeadSecurity(t *testing.T, c *Client, dir, verdict, head string) []review.PerspectiveReport {
	t.Helper()
	return recordSameHeadSecurityAs(t, c, dir, "reviewer", verdict, head)
}

func recordSameHeadSecurityAs(t *testing.T, c *Client, dir, agent, verdict, head string) []review.PerspectiveReport {
	t.Helper()
	raw := validVerdictJSONWithHead(t, "o/r", 376, "security", verdict, head)
	return c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 376, Agent: agent, Report: raw}, dir)
}

// storeRequiresHumanAt records an advisory security requires_human for the
// head and backdates its arrival (received_at and the file time) to at.
func storeRequiresHumanAt(t *testing.T, c *Client, dir string, at time.Time) {
	t.Helper()
	storeRequiresHuman(t, c, dir, at, at)
}

// storeRequiresHuman records an advisory security requires_human for the head,
// then sets its received_at stamp to receivedAt (zero: a legacy file without
// one) and its modification time to modTime, the way a copied or restored
// report dir can leave them apart.
func storeRequiresHuman(t *testing.T, c *Client, dir string, receivedAt, modTime time.Time) {
	t.Helper()
	withVerdictDispatchState(t, review.DispatchState{})
	recordSameHeadSecurity(t, c, dir, "requires_human", sameHeadSHA)
	_, path := storedSecurityReport(t, dir)
	setReportArrival(t, path, receivedAt, modTime)
}

func setReportArrival(t *testing.T, path string, receivedAt, modTime time.Time) {
	t.Helper()
	r, _, ok := review.ReadReportFile(path)
	if !ok {
		t.Fatalf("stored report %s unreadable", path)
	}
	r.ReceivedAt = receivedAt.UTC()
	if receivedAt.IsZero() {
		r.ReceivedAt = time.Time{}
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func storedSecurityReport(t *testing.T, dir string) (review.PerspectiveReport, string) {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(dir, review.ReviewReportFilePrefix+"*security*"+review.ReviewReportFileSuffix))
	if len(matches) != 1 {
		t.Fatalf("want one security report file, got %v", matches)
	}
	r, _, ok := review.ReadReportFile(matches[0])
	if !ok {
		t.Fatalf("stored report %s unreadable", matches[0])
	}
	return r, matches[0]
}

func collectedSecurity(t *testing.T, dir string) review.Verdict {
	t.Helper()
	artifact, err := review.Collect(dir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(artifact.Items) != 1 {
		t.Fatalf("want one aggregate, got %+v", artifact.Items)
	}
	return artifact.Items[0].Perspectives[review.PerspectiveSecurity]
}

// projectbluefin/dakota-iso#376: requires_human first, then a second
// automatic review of the same head, planned before that verdict existed,
// records approve. The approve must not replace the requires_human.
func TestRecordReviewVerdictSecondAutomaticReviewKeepsRequiresHuman(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		sameHeadPending(sameHeadSHA, time.Now().UTC().Add(-time.Minute)),
	}})

	if got := recordSameHeadSecurity(t, c, dir, "requires_human", sameHeadSHA); len(got) != 1 {
		t.Fatalf("first verdict not recorded: %+v", got)
	}
	if got := recordSameHeadSecurity(t, c, dir, "approve", sameHeadSHA); len(got) != 0 {
		t.Fatalf("second automatic review recorded %+v, want nothing recorded", got)
	}
	if r, _ := storedSecurityReport(t, dir); r.Verdict != review.VerdictRequiresHuman {
		t.Fatalf("stored verdict = %s, want requires_human kept", r.Verdict)
	}
	if got := collectedSecurity(t, dir); got != review.VerdictRequiresHuman {
		t.Fatalf("aggregate security = %s, want requires_human", got)
	}
}

// A queue-mode review has no dispatch row (advisory). It can neither clear a
// stricter verdict nor be cleared by another advisory review.
func TestRecordReviewVerdictAdvisoryReviewDoesNotClearRequiresHuman(t *testing.T) {
	dir := t.TempDir()
	oldPath, oldLegacy := review.ReviewDispatchStatePath, review.LegacyReviewDispatchStatePath
	review.ReviewDispatchStatePath = filepath.Join(dir, "missing", review.ReviewDispatchStateFile)
	review.LegacyReviewDispatchStatePath = filepath.Join(dir, "missing-legacy", review.ReviewDispatchStateFile)
	t.Cleanup(func() {
		review.ReviewDispatchStatePath, review.LegacyReviewDispatchStatePath = oldPath, oldLegacy
	})
	reportDir := filepath.Join(dir, "reports")
	c := &Client{logger: testLogger()}

	recordSameHeadSecurity(t, c, reportDir, "requires_human", sameHeadSHA)
	_, path := storedSecurityReport(t, reportDir)
	old := time.Now().Add(-2 * time.Hour)
	setReportArrival(t, path, old, old)
	recordSameHeadSecurity(t, c, reportDir, "approve", sameHeadSHA)
	if r, _ := storedSecurityReport(t, reportDir); r.Verdict != review.VerdictRequiresHuman {
		t.Fatalf("advisory approve replaced requires_human: %s", r.Verdict)
	}
}

// projectbluefin/bonedigger#116: approve first, requires_human second. The
// stricter verdict replaces the approve, as before.
func TestRecordReviewVerdictLaterRequiresHumanReplacesApprove(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		sameHeadPending(sameHeadSHA, time.Now().UTC().Add(-time.Minute)),
	}})

	recordSameHeadSecurity(t, c, dir, "approve", sameHeadSHA)
	if got := recordSameHeadSecurity(t, c, dir, "requires_human", sameHeadSHA); len(got) != 1 {
		t.Fatalf("later requires_human not recorded: %+v", got)
	}
	if got := collectedSecurity(t, dir); got != review.VerdictRequiresHuman {
		t.Fatalf("aggregate security = %s, want requires_human", got)
	}
}

// A new head clears it: the restrictive verdict judged other code.
func TestRecordReviewVerdictNewHeadReplacesRequiresHuman(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	newHead := "1111111111111111111111111111111111111111"
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		sameHeadPending(sameHeadSHA, time.Now().UTC().Add(-time.Minute)),
		sameHeadPending(newHead, time.Now().UTC().Add(-time.Minute)),
	}})

	recordSameHeadSecurity(t, c, dir, "requires_human", sameHeadSHA)
	if got := recordSameHeadSecurity(t, c, dir, "approve", newHead); len(got) != 1 {
		t.Fatalf("approve on the new head not recorded: %+v", got)
	}
	if r, _ := storedSecurityReport(t, dir); r.Verdict != review.VerdictApprove || r.HeadSHA != newHead {
		t.Fatalf("stored = %s@%s, want approve@%s", r.Verdict, r.HeadSHA, newHead)
	}
}

// A deliberate re-review of the same head — the planner asked for it after
// the verdict was on disk (review.revise_verdicts_before, a body-edit
// revisit, the whole set re-asked after an incomplete re-review expired) —
// still replaces it.
func TestRecordReviewVerdictDeliberateReReviewReplacesRequiresHuman(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		sameHeadPending(sameHeadSHA, time.Now().UTC().Add(-3*time.Hour)),
	}})
	recordSameHeadSecurity(t, c, dir, "requires_human", sameHeadSHA)
	_, path := storedSecurityReport(t, dir)
	recorded := time.Now().Add(-2 * time.Hour)
	setReportArrival(t, path, recorded, recorded)

	// The revisit: planned an hour after the verdict arrived, and delivered.
	revisit := sameHeadPending(sameHeadSHA, time.Now().UTC().Add(-time.Hour))
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{revisit}, Recent: []review.RecentReview{confirmedRecent(revisit)}})
	if got := recordSameHeadSecurity(t, c, dir, "approve", sameHeadSHA); len(got) != 1 {
		t.Fatalf("deliberate re-review not recorded: %+v", got)
	}
	if got := collectedSecurity(t, dir); got != review.VerdictApprove {
		t.Fatalf("aggregate security = %s, want the re-review's approve", got)
	}
}

// A deliberate re-review whose pending row is gone (it outlived pending_ttl,
// or the planner dropped it on a settled head) binds the recent-dispatch row
// ConfirmDelivered wrote. That row carries the planning time, so the
// re-review still replaces the verdict. A legacy row without it fails closed
// (its delivery time may postdate a verdict the kick was planned before),
// and a row planned before the verdict arrived does not replace it.
func TestRecordReviewVerdictRecentRowCarriesTheDispatchTime(t *testing.T) {
	stored := time.Now().Add(-2 * time.Hour)
	for name, tc := range map[string]struct {
		recent review.RecentReview
		want   review.Verdict
	}{
		"revisit planned after the verdict": {review.RecentReview{Dispatched: stored.Add(time.Hour), Confirmed: stored.Add(time.Hour + time.Minute)}, review.VerdictApprove},
		"legacy row, delivered after":       {review.RecentReview{Confirmed: stored.Add(time.Hour)}, review.VerdictRequiresHuman},
		"kick planned before the verdict":   {review.RecentReview{Dispatched: stored.Add(-time.Minute), Confirmed: stored.Add(time.Minute)}, review.VerdictRequiresHuman},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c := &Client{logger: testLogger()}
			storeRequiresHumanAt(t, c, dir, stored)
			r := tc.recent
			r.Repo, r.Number, r.HeadSHA, r.Perspective, r.Agent = "o/r", 376, sameHeadSHA, review.PerspectiveSecurity, "reviewer"
			withVerdictDispatchState(t, review.DispatchState{Recent: []review.RecentReview{r}})
			recordSameHeadSecurity(t, c, dir, "approve", sameHeadSHA)
			if got := collectedSecurity(t, dir); got != tc.want {
				t.Fatalf("aggregate security = %s, want %s", got, tc.want)
			}
		})
	}
}

// Only the dispatched agent's own answer counts as the dispatch's: a sibling
// reviewer bound to the row, and the same agent before its kick was
// delivered (a queue-mode review it ran on its own), bind as before but do
// not clear a stricter verdict, even when the row was planned after it.
func TestRecordReviewVerdictSiblingOrUndeliveredBindingIsNotDeliberate(t *testing.T) {
	stored := time.Now().Add(-2 * time.Hour)
	row := sameHeadPending(sameHeadSHA, stored.Add(time.Hour))
	cases := map[string]struct {
		state review.DispatchState
		agent string
		want  review.Verdict
	}{
		"same agent, kick not delivered": {review.DispatchState{Pending: []review.PendingReview{row}}, "reviewer", review.VerdictRequiresHuman},
		"sibling reviewer":               {review.DispatchState{Pending: []review.PendingReview{row}, Recent: []review.RecentReview{confirmedRecent(row)}}, "reviewer-2", review.VerdictRequiresHuman},
		"same agent, kick delivered":     {review.DispatchState{Pending: []review.PendingReview{row}, Recent: []review.RecentReview{confirmedRecent(row)}}, "reviewer", review.VerdictApprove},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c := &Client{logger: testLogger()}
			storeRequiresHumanAt(t, c, dir, stored)
			withVerdictDispatchState(t, tc.state)
			if got := recordSameHeadSecurityAs(t, c, dir, tc.agent, "approve", sameHeadSHA); tc.want == review.VerdictRequiresHuman && len(got) != 0 {
				t.Fatalf("approve recorded: %+v", got)
			}
			if got := collectedSecurity(t, dir); got != tc.want {
				t.Fatalf("aggregate security = %s, want %s", got, tc.want)
			}
		})
	}
}

// The production layout on knuckle: one agent runs both a queue-mode session
// and a swarm kick on the same head. The kick, planned at T0, is typed into
// the busy session and reads as delivered; the queue session then records
// requires_human, binds the delivered row and is stamped T0; the kick's own
// approve carries the same T0 and reporter. It must not soften the verdict:
// a reviewer cannot soften its own verdict on a head.
func TestRecordReviewVerdictSameAgentQueueThenSwarmKeepsRequiresHuman(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	row := sameHeadPending(sameHeadSHA, time.Now().UTC().Add(-time.Minute))
	row.Delivered = row.Dispatched.Add(5 * time.Second)
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{row}, Recent: []review.RecentReview{confirmedRecent(row)}})

	recordSameHeadSecurity(t, c, dir, "requires_human", sameHeadSHA)
	if r, _ := storedSecurityReport(t, dir); !r.DispatchedAt.Equal(row.Dispatched) {
		t.Fatalf("stored dispatched_at = %v, want the row's %v", r.DispatchedAt, row.Dispatched)
	}
	if got := recordSameHeadSecurity(t, c, dir, "approve", sameHeadSHA); len(got) != 0 {
		t.Fatalf("the same agent's later approve was recorded: %+v", got)
	}
	if got := collectedSecurity(t, dir); got != review.VerdictRequiresHuman {
		t.Fatalf("aggregate security = %s, want requires_human", got)
	}
}

// pending_ttl can outlive the recent rows' 24 hours: the pending row's own
// delivered_at proves the kick was delivered, so a deliberate re-review
// answered late still replaces the verdict.
func TestRecordReviewVerdictPendingRowDeliveryOutlivesRecentRows(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	stored := time.Now().Add(-30 * time.Hour)
	storeRequiresHumanAt(t, c, dir, stored)
	row := sameHeadPending(sameHeadSHA, stored.Add(time.Hour))
	row.Delivered = row.Dispatched.Add(time.Minute)
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{row}})
	if got := recordSameHeadSecurity(t, c, dir, "approve", sameHeadSHA); len(got) != 1 {
		t.Fatalf("late deliberate re-review not recorded: %+v", got)
	}
	if got := collectedSecurity(t, dir); got != review.VerdictApprove {
		t.Fatalf("aggregate security = %s, want approve", got)
	}
}

// A dispatched_at the reviewer wrote into its JSON is never trusted.
func TestRecordReviewVerdictIgnoresReviewerDispatchedAt(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	storeRequiresHumanAt(t, c, dir, time.Now().Add(-2*time.Hour))
	withVerdictDispatchState(t, review.DispatchState{})
	var report map[string]any
	if err := json.Unmarshal([]byte(validVerdictJSONWithHead(t, "o/r", 376, "security", "approve", sameHeadSHA)), &report); err != nil {
		t.Fatal(err)
	}
	report["dispatched_at"] = time.Now().UTC().Format(time.RFC3339)
	raw, _ := json.Marshal(report)
	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 376, Agent: "reviewer", Report: string(raw)}, dir)
	if got := collectedSecurity(t, dir); got != review.VerdictRequiresHuman {
		t.Fatalf("a reviewer-supplied dispatched_at cleared the verdict: %s", got)
	}
}

// projectbluefin/dakota-iso#376 exactly: the queue-mode run's advisory
// requires_human is on file; the swarm kick planned before it then reaches
// the same agent and records approve. Planned before, so kept.
func TestRecordReviewVerdictDeliveredKickPlannedBeforeTheVerdictKeepsIt(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	stored := time.Now().Add(-30 * time.Second)
	storeRequiresHumanAt(t, c, dir, stored)
	k2 := sameHeadPending(sameHeadSHA, stored.Add(-29*time.Second))
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{k2}, Recent: []review.RecentReview{confirmedRecent(k2)}})
	if got := recordSameHeadSecurity(t, c, dir, "approve", sameHeadSHA); len(got) != 0 {
		t.Fatalf("approve recorded: %+v", got)
	}
	if got := collectedSecurity(t, dir); got != review.VerdictRequiresHuman {
		t.Fatalf("aggregate security = %s, want requires_human", got)
	}
}

// The relay stamps received_at as it writes the report, and drops any value
// the reviewer put in its JSON.
func TestRecordReviewVerdictStampsReceivedAt(t *testing.T) {
	dir := t.TempDir()
	c := &Client{logger: testLogger()}
	withVerdictDispatchState(t, review.DispatchState{})
	var report map[string]any
	if err := json.Unmarshal([]byte(validVerdictJSONWithHead(t, "o/r", 376, "security", "requires_human", sameHeadSHA)), &report); err != nil {
		t.Fatal(err)
	}
	report["received_at"] = "2020-01-01T00:00:00Z"
	raw, _ := json.Marshal(report)
	before := time.Now().UTC().Add(-time.Second)
	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 376, Agent: "reviewer", Report: string(raw)}, dir)
	r, _ := storedSecurityReport(t, dir)
	if r.ReceivedAt.Before(before) || r.ReceivedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Fatalf("received_at = %v, want the relay's write time", r.ReceivedAt)
	}
}

// The arrival that orders a stored verdict against a later dispatch is the
// received_at stamp, not the file time: a copied, rsynced or restored report
// dir rewrites modification times. A legacy file without the stamp still
// falls back to its modification time.
func TestRecordReviewVerdictOrdersByReceivedAtNotFileTime(t *testing.T) {
	now := time.Now().UTC()
	revisit := sameHeadPending(sameHeadSHA, now.Add(-time.Hour))
	revisit.Delivered = revisit.Dispatched.Add(time.Minute)
	for name, tc := range map[string]struct {
		receivedAt, modTime time.Time
		want                review.Verdict
	}{
		// Copied dir: the old verdict's file looks new, its stamp does not.
		"stamp before the revisit, file time after (copied dir)": {now.Add(-2 * time.Hour), now, review.VerdictApprove},
		// Restored backup: the file looks old, the stamp shows the verdict
		// arrived after the revisit was planned.
		"stamp after the revisit, file time before (restore)": {now.Add(-10 * time.Minute), now.Add(-3 * time.Hour), review.VerdictRequiresHuman},
		"legacy file, file time before the revisit":           {time.Time{}, now.Add(-2 * time.Hour), review.VerdictApprove},
		"legacy file, file time after the revisit":            {time.Time{}, now.Add(-10 * time.Minute), review.VerdictRequiresHuman},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			c := &Client{logger: testLogger()}
			storeRequiresHuman(t, c, dir, tc.receivedAt, tc.modTime)
			withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{revisit}})
			recordSameHeadSecurity(t, c, dir, "approve", sameHeadSHA)
			if got := collectedSecurity(t, dir); got != tc.want {
				t.Fatalf("aggregate security = %s, want %s", got, tc.want)
			}
		})
	}
}
