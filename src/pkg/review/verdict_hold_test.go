package review

import (
	"testing"
	"time"
)

func TestLatestVerdictHold(t *testing.T) {
	t0 := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	reviewed := map[Perspective]Verdict{PerspectiveSecurity: VerdictRequiresHuman, PerspectiveCorrectness: VerdictApprove}
	agg := func(head string, v Verdict, at time.Duration) Aggregate {
		a := Aggregate{Repo: "acme/widget", Number: 91, HeadSHA: head, Verdict: v, Perspectives: reviewed, RecordedAt: t0.Add(at)}
		switch v {
		case VerdictRequiresHuman:
			a.RequiresHuman = true
		case VerdictApprove:
			a.MergeEligible = true
		}
		return a
	}
	notEligible := agg("b", VerdictApprove, 2*time.Hour)
	notEligible.MergeEligible = false
	unreviewed := agg("a", VerdictRequiresHuman, 0)
	unreviewed.Perspectives = nil

	cases := []struct {
		name     string
		items    []Aggregate
		want     bool
		wantHead string
		wantV    Verdict
	}{
		{name: "no verdicts", want: false},
		{name: "approve only", items: []Aggregate{agg("a", VerdictApprove, 0)}, want: false},
		{name: "requires_human on the head", items: []Aggregate{agg("a", VerdictRequiresHuman, 0)}, want: true, wantHead: "a", wantV: VerdictRequiresHuman},
		{name: "reject on the head", items: []Aggregate{agg("a", VerdictReject, 0)}, want: true, wantHead: "a", wantV: VerdictReject},
		{name: "changes_requested on the new head does not release", items: []Aggregate{agg("a", VerdictRequiresHuman, 0), agg("b", VerdictChangesRequested, time.Hour)}, want: true, wantHead: "a", wantV: VerdictRequiresHuman},
		{name: "non-eligible approve does not release", items: []Aggregate{agg("a", VerdictRequiresHuman, 0), notEligible}, want: true, wantHead: "a", wantV: VerdictRequiresHuman},
		{name: "fresh approve on the new head releases", items: []Aggregate{agg("a", VerdictRequiresHuman, 0), agg("b", VerdictApprove, time.Hour)}, want: false},
		{name: "approve before the hold does not release", items: []Aggregate{agg("a", VerdictApprove, 0), agg("b", VerdictRequiresHuman, time.Hour)}, want: true, wantHead: "b", wantV: VerdictRequiresHuman},
		{name: "unreviewed aggregate never holds", items: []Aggregate{unreviewed}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := LatestVerdictHold(tc.items)
			if ok != tc.want {
				t.Fatalf("held = %v, want %v (%+v)", ok, tc.want, got)
			}
			if ok && (got.HeadSHA != tc.wantHead || got.Verdict != tc.wantV) {
				t.Fatalf("hold = %+v, want head %q verdict %q", got, tc.wantHead, tc.wantV)
			}
		})
	}
}

func TestArtifactVerdictHoldForMatchesPR(t *testing.T) {
	hold := Aggregate{Repo: "Acme/Widget", Number: 91, HeadSHA: "a", Verdict: VerdictRequiresHuman, RequiresHuman: true,
		Perspectives: map[Perspective]Verdict{PerspectiveSecurity: VerdictRequiresHuman}, RecordedAt: time.Unix(100, 0)}
	other := hold
	other.Number = 92
	artifact := Artifact{Items: []Aggregate{hold, other}}
	if _, ok := artifact.VerdictHoldFor("acme/widget", 91); !ok {
		t.Fatal("hold on acme/widget#91 not found (repo match must ignore case)")
	}
	if _, ok := artifact.VerdictHoldFor("acme/widget", 93); ok {
		t.Fatal("a PR with no verdicts must not be held")
	}
	if _, ok := artifact.VerdictHoldFor("acme/other", 91); ok {
		t.Fatal("another repo's verdict must not hold this PR")
	}
}
