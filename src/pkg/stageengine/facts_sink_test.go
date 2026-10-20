package stageengine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// captureReconciler records the Facts each FactSink call submits.
type captureReconciler struct {
	got []Facts
	err error
}

func (c *captureReconciler) Reconcile(_ context.Context, f Facts) (Transition, error) {
	c.got = append(c.got, f)
	return Transition{Ref: f.Ref}, c.err
}

func (c *captureReconciler) ReconcileBatch(ctx context.Context, facts []Facts) ([]Transition, error) {
	var out []Transition
	for _, f := range facts {
		tr, err := c.Reconcile(ctx, f)
		if err != nil {
			return out, err
		}
		out = append(out, tr)
	}
	return out, nil
}

func TestFactSinkBuildsMinimalFacts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 19, 13, 0, 0, 0, time.UTC)
	cap := &captureReconciler{}
	sink := NewFactSink(cap, FactSinkOptions{Now: func() time.Time { return now }})
	issue := Ref{Owner: "acme", Repo: "widgets", Number: 1, Kind: stagelabels.IssueItem}
	pr := Ref{Owner: "acme", Repo: "widgets", Number: 9, Kind: stagelabels.PRItem}
	until := now.Add(time.Hour)

	steps := []struct {
		name string
		call func() error
		want func(t *testing.T, f Facts)
	}{
		{"claimed", func() error { return sink.Claimed(ctx, issue, ClaimFact{Holder: "bob", Kind: claims.KindHuman}) }, func(t *testing.T, f Facts) {
			if f.Issue.Bucket != BucketClaimed || f.Issue.Claim == nil || f.Issue.Claim.Holder != "bob" || f.Issue.Claim.Kind != claims.KindHuman {
				t.Fatalf("%+v", f.Issue)
			}
		}},
		{"released", func() error { return sink.ClaimReleased(ctx, issue, "bob") }, func(t *testing.T, f Facts) {
			if f.Issue.Bucket != BucketActionable || f.Issue.Claim != nil {
				t.Fatalf("%+v", f.Issue)
			}
		}},
		{"in-progress", func() error { return sink.InProgress(ctx, issue, "scanner", until) }, func(t *testing.T, f Facts) {
			if f.Issue.Bucket != BucketInProgress || f.Issue.Claim.Kind != claims.KindAgent || f.Issue.Claim.Holder != "scanner" || !f.Issue.Claim.Until.Equal(until) {
				t.Fatalf("%+v", f.Issue.Claim)
			}
		}},
		{"covered", func() error { return sink.CoveredBy(ctx, issue, Ref{Owner: "acme", Repo: "widgets", Number: 9}) }, func(t *testing.T, f Facts) {
			if f.Issue.Bucket != BucketCoveredByPR || f.Issue.CoveringPR == nil || *f.Issue.CoveringPR != pr || f.Issue.Claim.PRNumber != 9 || f.Issue.Claim.PRRepo != "acme/widgets" {
				t.Fatalf("%+v %+v", f.Issue, f.Issue.CoveringPR)
			}
		}},
		{"uncovered", func() error { return sink.Uncovered(ctx, issue, pr) }, func(t *testing.T, f Facts) {
			if f.Issue.Bucket != BucketActionable || f.Issue.CoveringPR != nil {
				t.Fatalf("%+v", f.Issue)
			}
		}},
		{"parked issue", func() error { return sink.Parked(ctx, issue, " needs-human ") }, func(t *testing.T, f Facts) {
			if !f.Park.NeedsHuman || f.Park.DecisionLabel != "needs-human" || f.Issue.Bucket != BucketFiltered || f.Issue.FilteredReason != "needs-human" || len(f.Labels) != 1 {
				t.Fatalf("%+v %+v", f.Park, f.Issue)
			}
		}},
		{"parked pr", func() error { return sink.Parked(ctx, pr, "hold") }, func(t *testing.T, f Facts) {
			if f.Park.NeedsHuman || f.Park.DecisionLabel != "hold" || len(f.Park.Holds) != 1 || f.PR == nil || f.Issue != nil {
				t.Fatalf("%+v pr=%v", f.Park, f.PR)
			}
		}},
		{"unparked issue", func() error { return sink.Unparked(ctx, issue, "maintainer") }, func(t *testing.T, f Facts) {
			if !f.Park.ApprovedDirection || f.Issue == nil || f.Issue.Bucket != BucketActionable {
				t.Fatalf("%+v %+v", f.Park, f.Issue)
			}
		}},
		{"unparked pr", func() error { return sink.Unparked(ctx, pr, "maintainer") }, func(t *testing.T, f Facts) {
			if !f.Park.ApprovedDirection || f.PR == nil || f.Issue != nil {
				t.Fatalf("%+v", f)
			}
		}},
	}
	for i, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			if err := s.call(); err != nil {
				t.Fatal(err)
			}
			if len(cap.got) != i+1 {
				t.Fatalf("%d facts submitted, want %d", len(cap.got), i+1)
			}
			f := cap.got[i]
			if !f.Open || !f.ObservedAt.Equal(now) || f.Ref.Number == 0 {
				t.Fatalf("common facts: %+v", f)
			}
			s.want(t, f)
		})
	}
}

func TestFactSinkEnrichAndErrors(t *testing.T) {
	ctx := context.Background()
	cap := &captureReconciler{}
	ref := Ref{Owner: "acme", Repo: "widgets", Number: 1, Kind: stagelabels.IssueItem}
	sink := NewFactSink(cap, FactSinkOptions{Enrich: func(_ context.Context, f Facts) (Facts, error) {
		f.Labels = append(f.Labels, "bug")
		return f, nil
	}})
	if err := sink.ClaimReleased(ctx, ref, "bob"); err != nil {
		t.Fatal(err)
	}
	if len(cap.got) != 1 || len(cap.got[0].Labels) != 1 || cap.got[0].Labels[0] != "bug" {
		t.Fatalf("enrich not applied: %+v", cap.got)
	}
	boom := errors.New("boom")
	failing := NewFactSink(cap, FactSinkOptions{Enrich: func(context.Context, Facts) (Facts, error) { return Facts{}, boom }})
	if err := failing.ClaimReleased(ctx, ref, "bob"); !errors.Is(err, boom) {
		t.Fatalf("enrich error = %v", err)
	}
	if len(cap.got) != 1 {
		t.Fatal("a failed enrich must not reconcile")
	}
	cap.err = boom
	if err := sink.ClaimReleased(ctx, ref, "bob"); !errors.Is(err, boom) {
		t.Fatalf("reconcile error = %v", err)
	}
}

// DecisionLiftsNeedsHuman: a maintainer's decision answers the hive's own
// `needs-human` (#13075) and never a person's or an unattributed one (#12680).
func TestDecisionLiftsNeedsHuman(t *testing.T) {
	cases := []struct {
		name    string
		applier NeedsHumanApplier
		want    bool
	}{
		{"hive-applied lifts", NeedsHumanApplierHive, true},
		{"person-applied stays", NeedsHumanApplierPerson, false},
		{"unknown stays", NeedsHumanApplierUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecisionLiftsNeedsHuman(tc.applier); got != tc.want {
				t.Fatalf("DecisionLiftsNeedsHuman(%v) = %v, want %v", tc.applier, got, tc.want)
			}
		})
	}
}
