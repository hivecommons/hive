package github

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

func seedOutcomeBundle(t *testing.T, root string, number int, head string, created time.Time) *evidence.Bundle {
	t.Helper()
	b := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion,
		ID:            evidence.BundleID("o/r", number, head),
		Repo:          "o/r",
		Number:        number,
		Author:        evidence.Author{Login: "alice", Kind: evidence.AuthorHuman},
		BaseSHA:       "base000",
		HeadSHA:       head,
		CreatedAt:     created,
		UpdatedAt:     created,
	}
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeReviewEvidence(ReviewEvidencePath(root, "o/r", number, head), b); err != nil {
		t.Fatal(err)
	}
	return b
}

// The review-outcome ledger's resolved state is stamped on the PR's newest
// bundle without any GitHub call: a merge by someone else becomes an Outcome
// plus an "unrecorded" merge event naming the merger, a close becomes a
// closed Outcome, and the rewritten bundle validates and re-seals.
func TestRecordReviewEvidenceOutcomeStampsNewestBundle(t *testing.T) {
	root := withEvidenceRoot(t)
	fx := &evidencePRFixture{head: "h2"}
	c := evidenceTestClient(t, fx, &ReviewEvidenceSettings{Enabled: true})
	created := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	seedOutcomeBundle(t, root, 7, "h1", created)
	seedOutcomeBundle(t, root, 7, "h2", created.Add(time.Hour))
	seedOutcomeBundle(t, root, 8, "h3", created)

	mergedAt := created.Add(5 * time.Hour)
	now := mergedAt.Add(10 * time.Minute)
	c.RecordReviewEvidenceOutcome("o/r", 7, PRState{State: "closed", MergedAt: mergedAt, ClosedAt: mergedAt, MergedBy: "bob"}, now)
	c.RecordReviewEvidenceOutcome("o/r", 8, PRState{State: "closed", ClosedAt: mergedAt}, now)

	if fx.fetches() != 0 {
		t.Errorf("stamping an outcome made %d GitHub fetches, want 0", fx.fetches())
	}

	newest, err := LoadReviewEvidence(ReviewEvidencePath(root, "o/r", 7, "h2"))
	if err != nil {
		t.Fatal(err)
	}
	if newest.Outcome == nil || newest.Outcome.State != evidence.BackfillStateMerged || newest.Outcome.MergedBy != "bob" || !newest.Outcome.MergedAt.Equal(mergedAt) || newest.Outcome.Source != evidence.OutcomeSourceLedger {
		t.Fatalf("outcome = %+v", newest.Outcome)
	}
	want := evidence.MergeEvent{Actor: "bob", Method: evidence.MergeUnrecorded, At: mergedAt, SHA: "h2"}
	if newest.Merge == nil || *newest.Merge != want {
		t.Errorf("merge = %+v, want %+v", newest.Merge, want)
	}
	if !newest.UpdatedAt.Equal(now) || newest.Hash == "" {
		t.Errorf("bundle not resealed: updated_at=%s hash=%q", newest.UpdatedAt, newest.Hash)
	}
	if err := newest.Validate(); err != nil {
		t.Errorf("stamped bundle does not validate: %v", err)
	}
	older, err := LoadReviewEvidence(ReviewEvidencePath(root, "o/r", 7, "h1"))
	if err != nil {
		t.Fatal(err)
	}
	if older.Outcome != nil || older.Merge != nil {
		t.Errorf("older head must be left alone, got outcome %+v merge %+v", older.Outcome, older.Merge)
	}

	closed, err := LoadReviewEvidence(ReviewEvidencePath(root, "o/r", 8, "h3"))
	if err != nil {
		t.Fatal(err)
	}
	if closed.Outcome == nil || closed.Outcome.State != evidence.BackfillStateClosed || !closed.Outcome.ClosedAt.Equal(mergedAt) || closed.Merge != nil {
		t.Errorf("closed outcome = %+v merge = %+v", closed.Outcome, closed.Merge)
	}

	prs := evidence.FoldPRs([]*evidence.Bundle{older, newest, closed}, "", "")
	if p := prs[evidence.PRKey("o/r", 7)]; !p.OutcomeKnown() || !p.MergedElsewhere {
		t.Errorf("fold: known=%v elsewhere=%v", p.OutcomeKnown(), p.MergedElsewhere)
	}
	if p := prs[evidence.PRKey("o/r", 8)]; !p.OutcomeKnown() || !p.Closed {
		t.Errorf("fold: known=%v closed=%v", p.OutcomeKnown(), p.Closed)
	}
}

// Nothing is written for a PR that is still open, one with no bundle, one
// whose bundle already states an outcome (a merge this hive performed or an
// earlier stamp), or when evidence is disabled for the repo.
func TestRecordReviewEvidenceOutcomeLeavesSettledBundlesAlone(t *testing.T) {
	root := withEvidenceRoot(t)
	c := evidenceTestClient(t, &evidencePRFixture{head: "h1"}, &ReviewEvidenceSettings{Enabled: true})
	created := time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)
	now := created.Add(6 * time.Hour)

	open := seedOutcomeBundle(t, root, 7, "h1", created)
	c.RecordReviewEvidenceOutcome("o/r", 7, PRState{State: "open"}, now)
	got, _ := LoadReviewEvidence(ReviewEvidencePath(root, "o/r", 7, "h1"))
	if got.Outcome != nil || got.Hash != open.Hash {
		t.Errorf("open PR: bundle changed: outcome=%+v", got.Outcome)
	}

	hive := seedOutcomeBundle(t, root, 8, "h2", created)
	hive.Merge = &evidence.MergeEvent{Actor: "hive[bot]", Method: "squash", At: created.Add(time.Hour), SHA: "m8"}
	if err := evidence.Seal(hive, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeReviewEvidence(ReviewEvidencePath(root, "o/r", 8, "h2"), hive); err != nil {
		t.Fatal(err)
	}
	c.RecordReviewEvidenceOutcome("o/r", 8, PRState{State: "closed", MergedAt: created.Add(time.Hour), MergedBy: "hive[bot]"}, now)
	got, _ = LoadReviewEvidence(ReviewEvidencePath(root, "o/r", 8, "h2"))
	if got.Outcome != nil || got.Hash != hive.Hash {
		t.Errorf("hive-merged PR: bundle changed: outcome=%+v", got.Outcome)
	}

	seedOutcomeBundle(t, root, 9, "h3", created)
	c.RecordReviewEvidenceOutcome("o/r", 9, PRState{State: "closed", ClosedAt: now}, now)
	first, _ := LoadReviewEvidence(ReviewEvidencePath(root, "o/r", 9, "h3"))
	c.RecordReviewEvidenceOutcome("o/r", 9, PRState{State: "closed", MergedAt: now.Add(time.Hour), MergedBy: "bob"}, now.Add(2*time.Hour))
	again, _ := LoadReviewEvidence(ReviewEvidencePath(root, "o/r", 9, "h3"))
	if again.Hash != first.Hash || again.Merge != nil {
		t.Errorf("second stamp must not rewrite: hash %q -> %q merge %+v", first.Hash, again.Hash, again.Merge)
	}

	c.RecordReviewEvidenceOutcome("o/r", 42, PRState{State: "closed", ClosedAt: now}, now)
	if _, err := ListReviewEvidence(root, "o/r", 42); err == nil {
		if entries, _ := ListReviewEvidence(root, "o/r", 42); len(entries) != 0 {
			t.Errorf("PR without a bundle gained %d", len(entries))
		}
	}

	off := evidenceTestClient(t, &evidencePRFixture{head: "h1"}, nil)
	off.RecordReviewEvidenceOutcome("o/r", 7, PRState{State: "closed", ClosedAt: now}, now)
	got, _ = LoadReviewEvidence(ReviewEvidencePath(root, "o/r", 7, "h1"))
	if got.Outcome != nil {
		t.Errorf("evidence disabled: bundle stamped %+v", got.Outcome)
	}
	var nilClient *Client
	nilClient.RecordReviewEvidenceOutcome("o/r", 7, PRState{State: "closed"}, now)
}

func TestReviewEvidenceOutcomeFromState(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if _, ok := reviewEvidenceOutcomeFromState(PRState{State: "open"}, now); ok {
		t.Error("open PR must yield no outcome")
	}
	if _, ok := reviewEvidenceOutcomeFromState(PRState{}, now); ok {
		t.Error("empty state must yield no outcome")
	}
	o, ok := reviewEvidenceOutcomeFromState(PRState{State: "CLOSED"}, now)
	if !ok || o.State != evidence.BackfillStateClosed || !o.ClosedAt.IsZero() || !o.At.Equal(now) {
		t.Errorf("closed without closed_at: ok=%v %+v", ok, o)
	}
	o, ok = reviewEvidenceOutcomeFromState(PRState{State: "closed", MergedAt: now.Add(-time.Hour), MergedBy: " carol "}, now)
	if !ok || o.State != evidence.BackfillStateMerged || o.MergedBy != "carol" {
		t.Errorf("merged: ok=%v %+v", ok, o)
	}
}
