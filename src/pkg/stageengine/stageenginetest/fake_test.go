package stageenginetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func issueRef(n int) stageengine.Ref {
	return stageengine.Ref{Owner: "o", Repo: "r", Number: n, Kind: stagelabels.IssueItem}
}

// A deriver that stages every issue 020 when the facts carry a claim and 010
// otherwise; enough to drive the Writer through a real reconcile.
func claimDeriver() stageengine.Deriver {
	return stageengine.DeriverFunc(stagelabels.IssueItem, func(f stageengine.Facts) stageengine.Decision {
		if f.Issue == nil {
			return stageengine.Decision{Abstain: true, Reason: "no issue facts"}
		}
		if f.Issue.Claim != nil {
			return stageengine.Decision{Stage: stagelabels.Claimed, Reason: "claimed"}
		}
		return stageengine.Decision{Stage: stagelabels.Actionable, Reason: "actionable"}
	})
}

func TestWriterAppliesAgainstItsOwnLabels(t *testing.T) {
	w := NewWriter()
	ref := issueRef(1)
	w.Seed(ref, "bug", stagelabels.Actionable.Label())

	out, err := w.Apply(context.Background(), ref, nil, stagelabels.Desired{Stage: stagelabels.Claimed})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.LabelsOf(ref); strings.Join(got, ",") != "bug,"+stagelabels.Claimed.Label() {
		t.Fatalf("labels = %v, want bug + 020 (010 swapped out, stale hint ignored)", got)
	}
	applies := w.Applies()
	if len(applies) != 1 || applies[0].Ref != ref || applies[0].Want.Stage != stagelabels.Claimed || strings.Join(applies[0].Out.Added, ",") != strings.Join(out.Added, ",") {
		t.Fatalf("applies = %+v", applies)
	}

	w.Err = errors.New("boom")
	if _, err := w.Apply(context.Background(), ref, nil, stagelabels.Desired{Stage: stagelabels.Actionable}); err == nil {
		t.Fatal("Err must fail Apply")
	}
	if len(w.Applies()) != 1 {
		t.Fatal("a failed Apply is not recorded")
	}
}

func TestLabelerAddIsIdempotentAndRemoveIsExact(t *testing.T) {
	w := NewWriter()
	l := labeler{w}
	ctx := context.Background()
	if err := l.AddLabels(ctx, "o", "r", 2, []string{"a", "b", "a"}); err != nil {
		t.Fatal(err)
	}
	if err := l.RemoveLabel(ctx, "o", "r", 2, "A"); err != nil {
		t.Fatal(err)
	}
	got, err := l.Labels(ctx, "o", "r", 2)
	if err != nil || strings.Join(got, ",") != "a,b" {
		t.Fatalf("labels = %v, %v", got, err)
	}
	if err := l.RemoveLabel(ctx, "o", "r", 2, "a"); err != nil {
		t.Fatal(err)
	}
	if got := w.LabelsOf(issueRef(2)); strings.Join(got, ",") != "b" {
		t.Fatalf("labels = %v", got)
	}
}

func TestNewSinkRunsTheRealReconciler(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	sink, w := NewSink(Options{Issue: claimDeriver(), Now: func() time.Time { return now }})
	ref := issueRef(3)
	w.Seed(ref, "bug", stagelabels.Actionable.Label())
	ctx := context.Background()

	if err := sink.Claimed(ctx, ref, stageengine.ClaimFact{Holder: "hive-a"}); err != nil {
		t.Fatal(err)
	}
	if got := w.LabelsOf(ref); strings.Join(got, ",") != "bug,"+stagelabels.Claimed.Label() {
		t.Fatalf("after Claimed: %v", got)
	}
	if err := sink.ClaimReleased(ctx, ref, "hive-a"); err != nil {
		t.Fatal(err)
	}
	if got := w.LabelsOf(ref); stagelabels.Current(got) != stagelabels.Actionable {
		t.Fatalf("after ClaimReleased: %v", got)
	}
	// Unparked marks the bucket actionable through the default Enrich.
	if err := sink.Unparked(ctx, ref, "owner"); err != nil {
		t.Fatal(err)
	}
	if got := w.LabelsOf(ref); stagelabels.Current(got) != stagelabels.Actionable {
		t.Fatalf("after Unparked: %v", got)
	}

	// The built-in PR deriver handles PR facts without registration.
	pr := stageengine.Ref{Owner: "o", Repo: "r", Number: 4, Kind: stagelabels.PRItem}
	if err := sink.Parked(ctx, pr, "needs-decision"); err != nil {
		t.Fatal(err)
	}
	if got := w.LabelsOf(pr); stagelabels.Current(got) != stagelabels.NeedsDecision {
		t.Fatalf("after PR Parked: %v, want needs-decision", got)
	}
}

func TestNewSinkHonoursEnrich(t *testing.T) {
	enrichErr := errors.New("read failed")
	sink, w := NewSink(Options{
		Issue: claimDeriver(),
		Enrich: func(context.Context, stageengine.Facts) (stageengine.Facts, error) {
			return stageengine.Facts{}, enrichErr
		},
	})
	if err := sink.Claimed(context.Background(), issueRef(5), stageengine.ClaimFact{Holder: "x"}); !errors.Is(err, enrichErr) {
		t.Fatalf("err = %v, want the Enrich error", err)
	}
	if len(w.Applies()) != 0 {
		t.Fatal("a failed Enrich must not write")
	}
}

func TestRecorderRecordsEveryMethod(t *testing.T) {
	r := &Recorder{}
	ctx := context.Background()
	issue, pr := issueRef(6), stageengine.Ref{Owner: "o", Repo: "r", Number: 7, Kind: stagelabels.PRItem}
	until := time.Date(2026, 10, 19, 13, 0, 0, 0, time.UTC)

	_ = r.Claimed(ctx, issue, stageengine.ClaimFact{Holder: "h"})
	_ = r.ClaimReleased(ctx, issue, "h")
	_ = r.InProgress(ctx, issue, "agent", until)
	_ = r.CoveredBy(ctx, issue, pr)
	_ = r.Uncovered(ctx, issue, pr)
	_ = r.Parked(ctx, issue, "needs-decision")
	_ = r.Unparked(ctx, issue, "owner")

	got := r.All()
	var methods []string
	for _, rec := range got {
		methods = append(methods, rec.Method)
	}
	if strings.Join(methods, ",") != "Claimed,ClaimReleased,InProgress,CoveredBy,Uncovered,Parked,Unparked" {
		t.Fatalf("methods = %v", methods)
	}
	if got[0].Claim == nil || got[0].Who != "h" || got[2].Until != until || *got[3].PR != pr || got[5].Label != "needs-decision" || got[6].Who != "owner" {
		t.Fatalf("recorded = %+v", got)
	}

	r.Err = errors.New("refused")
	if err := r.Parked(ctx, issue, "x"); err == nil {
		t.Fatal("Err must be returned")
	}
	if len(r.All()) != 8 {
		t.Fatal("a refused fact is still recorded")
	}
}
