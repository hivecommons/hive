package stageengine

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The reconciler keeps the issue record's memory of its covering PR in step
// with the stage it observes: the named PR at 040, nothing once the issue
// leaves 040, and a 040 that did not name the PR leaves the memory alone.
func TestReconcileNotesCoveringPR(t *testing.T) {
	ctx := context.Background()
	w := newFakeWriter()
	r, _, store := newTestReconciler(t, ModeEnforce, w, nil)
	issue := Ref{Owner: "o", Repo: "r", Number: 7, Kind: stagelabels.IssueItem}
	pr := Ref{Owner: "o", Repo: "r", Number: 8, Kind: stagelabels.PRItem}

	if _, err := r.Reconcile(ctx, Facts{Ref: issue, Open: true, Issue: &IssueFacts{Bucket: BucketCoveredByPR, CoveringPR: &pr}}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.Get(issue.Key()); rec.Stage != stagelabels.CoveredByPR || rec.CoveringPR != pr.Key() {
		t.Fatalf("after CoveredBy: stage=%v covering=%q, want 040 and %q", rec.Stage, rec.CoveringPR, pr.Key())
	}
	// A 040 without the PR named (a sweep bucket with no ledger number) keeps it.
	if _, err := r.Reconcile(ctx, Facts{Ref: issue, Open: true, Issue: &IssueFacts{Bucket: BucketCoveredByPR}}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.Get(issue.Key()); rec.CoveringPR != pr.Key() {
		t.Fatalf("unnamed 040 cleared the covering PR: %q", rec.CoveringPR)
	}
	// Leaving 040 forgets it.
	if _, err := r.Reconcile(ctx, Facts{Ref: issue, Open: true, Issue: &IssueFacts{Bucket: BucketActionable}}); err != nil {
		t.Fatal(err)
	}
	if rec, _ := store.Get(issue.Key()); rec.Stage != stagelabels.Actionable || rec.CoveringPR != "" {
		t.Fatalf("after Uncovered: stage=%v covering=%q, want 010 and none", rec.Stage, rec.CoveringPR)
	}
}

// An Uncovered fact names the PR that closed so enrichment can tell "that
// cover is gone" from "a claim lapsed under a cover that still stands".
func TestFactSinkUncoveredNamesClosedPR(t *testing.T) {
	var got Facts
	r, _, _ := newTestReconciler(t, ModeEnforce, newFakeWriter(), nil)
	sink := NewFactSink(r, FactSinkOptions{Enrich: func(_ context.Context, f Facts) (Facts, error) { got = f; return f, nil }})
	issue := Ref{Owner: "o", Repo: "r", Number: 7, Kind: stagelabels.IssueItem}
	pr := Ref{Owner: "o", Repo: "r", Number: 8}
	if err := sink.Uncovered(context.Background(), issue, pr); err != nil {
		t.Fatal(err)
	}
	if got.Issue == nil || got.Issue.ClosedPR == nil || got.Issue.ClosedPR.Key() != "o/r#8" || got.Issue.ClosedPR.Kind != stagelabels.PRItem {
		t.Fatalf("Uncovered facts = %+v, want ClosedPR o/r#8 as a PR", got.Issue)
	}
	if got.Issue.Bucket != BucketActionable {
		t.Fatalf("Uncovered bucket = %q, want actionable", got.Issue.Bucket)
	}
}
