package stageengine

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestDeriveIssueProgress pins Decision.Progress: set only when an issue
// lands on an actionable stage wearing hive/partially-addressed (the PR
// that covered it merged and left a remainder), never from the label at
// another stage nor from the stage alone.
func TestDeriveIssueProgress(t *testing.T) {
	partial := stagelabels.PartiallyAddressedLabel
	cases := []struct {
		name  string
		mut   func(f *Facts)
		stage stagelabels.Stage
		want  bool
	}{
		{"actionable, no label", nil, stagelabels.Actionable, false},
		{"actionable, partially addressed", func(f *Facts) { f.Labels = []string{"help wanted", partial} }, stagelabels.Actionable, true},
		{"label case-insensitive", func(f *Facts) { f.Labels = []string{strings.ToUpper(partial)} }, stagelabels.Actionable, true},
		{"triage needed, partially addressed", func(f *Facts) {
			f.Labels = []string{partial}
			f.Issue.TriageAwaiting = "carries needs-triage"
		}, stagelabels.TriageNeeded, true},
		{"covered again, label still on", func(f *Facts) {
			f.Labels = []string{partial}
			f.Issue.Bucket = BucketCoveredByPR
		}, stagelabels.CoveredByPR, false},
		{"claimed, label still on", func(f *Facts) {
			f.Labels = []string{partial}
			f.Issue.Bucket = BucketClaimed
		}, stagelabels.Claimed, false},
		{"closed, label on", func(f *Facts) {
			f.Labels = []string{partial}
			f.Open = false
		}, stagelabels.None, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := DeriveIssue(derivedIssueFacts(tc.mut))
			if d.Stage != tc.stage || d.Progress != tc.want {
				t.Fatalf("stage=%v progress=%v, want stage=%v progress=%v (%+v)", d.Stage, d.Progress, tc.stage, tc.want, d)
			}
		})
	}
}

// TestReconcileProgressFallBooksNoRegression replays hivecommons/hive#14557
// through the real issue deriver: filed → claimed → covered by a PR → the
// PR merges partially and the issue returns to 010 wearing
// hive/partially-addressed. The record must not read as regressed. The
// control case — the covering PR closes unmerged, no label — still does.
func TestReconcileProgressFallBooksNoRegression(t *testing.T) {
	cases := []struct {
		name          string
		labels        []string
		wantRegressed bool
	}{
		{"covering PR merged partially", []string{stagelabels.CoveredByPR.Label(), stagelabels.PartiallyAddressedLabel}, false},
		{"covering PR closed unmerged", []string{stagelabels.CoveredByPR.Label()}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
			store := stagelabels.NewStore("", func() time.Time { return now })
			r := New(Options{
				Mode:    ModeEnforce,
				Store:   store,
				Writer:  newFakeWriter(),
				Now:     func() time.Time { return now },
				Logger:  slog.New(slog.NewTextHandler(&strings.Builder{}, nil)),
				Metrics: NewCounters(),
			})
			pr := &Ref{Owner: "hivecommons", Repo: "hive", Number: 14562, Kind: stagelabels.PRItem}
			steps := []func(f *Facts){
				func(f *Facts) { f.Issue.Bucket = BucketActionable },
				func(f *Facts) {
					f.Issue.Bucket = BucketClaimed
					f.Issue.Claim = &ClaimFact{Holder: "scanner", Kind: "agent"}
				},
				func(f *Facts) { f.Issue.Bucket = BucketCoveredByPR; f.Issue.CoveringPR = pr },
				func(f *Facts) { f.Issue.Bucket = BucketActionable; f.Labels = tc.labels },
			}
			var last Transition
			for i, mut := range steps {
				now = now.Add(time.Duration(i) * 10 * time.Minute)
				f := Facts{
					Ref:   Ref{Owner: "hivecommons", Repo: "hive", Number: 14557, Kind: stagelabels.IssueItem},
					Open:  true,
					Issue: &IssueFacts{},
				}
				mut(&f)
				tr, err := r.Reconcile(ctx, f)
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				last = tr
			}
			if last.From != stagelabels.CoveredByPR || last.To != stagelabels.Actionable {
				t.Fatalf("final hop %v → %v, want 040 → 010", last.From, last.To)
			}
			rec, ok := store.Get(stagelabels.Key("hivecommons/hive", 14557))
			if !ok {
				t.Fatal("record missing")
			}
			if rec.Regressed() != tc.wantRegressed {
				t.Fatalf("Regressed()=%v, want %v: %+v", rec.Regressed(), tc.wantRegressed, rec)
			}
			if hop := rec.History[len(rec.History)-1]; hop.Regressed != tc.wantRegressed || hop.From != stagelabels.CoveredByPR {
				t.Fatalf("last hop %+v, want regressed=%v from 040", hop, tc.wantRegressed)
			}
		})
	}
}
