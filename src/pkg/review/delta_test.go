package review

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/outputschema"
)

var deltaNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func deltaOpts(agents ...AgentCapability) DispatchOptions {
	o := swarmOpts(agents...)
	o.DeltaRereview = true
	o.Now = deltaNow
	return o
}

func priorAggregate(sha string, verdict Verdict, recorded time.Time, findings ...PerspectiveFinding) Aggregate {
	return Aggregate{
		Repo: "acme/hive", Number: 7, HeadSHA: sha, Verdict: verdict, RecordedAt: recorded,
		MergeEligible: verdict == VerdictApprove,
		Perspectives:  map[Perspective]Verdict{PerspectiveCorrectness: verdict, PerspectiveSecurity: VerdictApprove},
		Findings:      findings,
	}
}

func flagged(file string, line int, title string) PerspectiveFinding {
	return PerspectiveFinding{Perspective: PerspectiveCorrectness, Finding: outputschema.Finding{Title: title, Severity: outputschema.SeverityMedium, File: file, Line: line}}
}

func knownDelta(prev string, files ...DeltaFile) *DeltaDiff {
	return &DeltaDiff{PreviousHead: prev, Known: true, Files: files}
}

func TestPriorAggregateForPicksLatestOtherHead(t *testing.T) {
	art := Artifact{Items: []Aggregate{
		priorAggregate("sha1", VerdictChangesRequested, deltaNow.Add(-48*time.Hour)),
		priorAggregate("sha2", VerdictApprove, deltaNow.Add(-2*time.Hour)),
		priorAggregate("sha3", VerdictApprove, deltaNow),
		{Repo: "acme/other", Number: 7, HeadSHA: "sha9", Verdict: VerdictApprove, RecordedAt: deltaNow},
	}}
	got, ok := art.PriorAggregateFor("acme/hive", 7, "sha3")
	if !ok || got.HeadSHA != "sha2" {
		t.Fatalf("prior = %+v ok=%v, want sha2", got, ok)
	}
	if _, ok := art.PriorAggregateFor("acme/hive", 8, "sha3"); ok {
		t.Fatal("unreviewed PR reported a prior aggregate")
	}
	one := Artifact{Items: []Aggregate{priorAggregate("sha1", VerdictApprove, deltaNow)}}
	if _, ok := one.PriorAggregateFor("acme/hive", 7, "sha1"); ok {
		t.Fatal("the current head counted as its own prior")
	}
}

func TestPlanDeltaFirstReviewIsFull(t *testing.T) {
	pr := dispatchPR("sha1")
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, deltaOpts(reviewer("r1")))
	if len(plan.ReviewKicks) != 1 || len(plan.DeltaRestamps) != 0 {
		t.Fatalf("kicks=%d restamps=%d, want 1/0", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
	k := plan.ReviewKicks[0]
	if k.DeltaPreviousHead != "" || strings.Contains(k.Message, "DELTA RE-REVIEW") {
		t.Fatalf("first review carried delta context: %+v", k)
	}
	if plan.State.Pending[0].DeltaPreviousHead != "" {
		t.Fatalf("pending stamped a previous head on a first review: %+v", plan.State.Pending[0])
	}
}

func TestPlanDeltaReReviewPromptScopesToDiff(t *testing.T) {
	pr := dispatchPR("sha2")
	pr.DeltaDiff = knownDelta("sha1", DeltaFile{Path: "pkg/a/a.go", Additions: 30, Deletions: 4}, DeltaFile{Path: "pkg/b/b.go", Additions: 10})
	art := Artifact{Items: []Aggregate{priorAggregate("sha1", VerdictChangesRequested, deltaNow.Add(-time.Hour), flagged("pkg/a/a.go", 12, "nil deref on empty input"))}}
	plan := PlanDispatch([]PullRequest{pr}, art, DispatchState{}, deltaOpts(reviewer("r1")))
	if len(plan.ReviewKicks) != 1 || len(plan.DeltaRestamps) != 0 {
		t.Fatalf("kicks=%d restamps=%d, want 1/0", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
	k := plan.ReviewKicks[0]
	if k.DeltaPreviousHead != "sha1" {
		t.Fatalf("kick previous head = %q, want sha1", k.DeltaPreviousHead)
	}
	for _, want := range []string{
		"DELTA RE-REVIEW",
		"already reviewed this PR at head sha1",
		"verdict was changes_requested",
		"correctness=changes_requested",
		"compare/sha1...sha2",
		"44 changed line(s) across 2 file(s): pkg/a/a.go, pkg/b/b.go",
		"[correctness] pkg/a/a.go:12 nil deref on empty input",
		"Review ONLY what changed since that head",
	} {
		if !strings.Contains(k.Message, want) {
			t.Fatalf("delta prompt lacks %q:\n%s", want, k.Message)
		}
	}
	if plan.State.Pending[0].DeltaPreviousHead != "sha1" {
		t.Fatalf("pending did not record the previous head: %+v", plan.State.Pending[0])
	}
}

func TestPlanDeltaUnknownDiffStillDeltaButNoRestamp(t *testing.T) {
	pr := dispatchPR("sha2")
	art := Artifact{Items: []Aggregate{priorAggregate("sha1", VerdictApprove, deltaNow.Add(-time.Hour))}}
	for name, diff := range map[string]*DeltaDiff{
		"nil":            nil,
		"unknown":        {PreviousHead: "sha1"},
		"wrong previous": knownDelta("sha0"),
	} {
		pr.DeltaDiff = diff
		plan := PlanDispatch([]PullRequest{pr}, art, DispatchState{}, deltaOpts(reviewer("r1")))
		if len(plan.ReviewKicks) != 1 || len(plan.DeltaRestamps) != 0 {
			t.Fatalf("%s: kicks=%d restamps=%d, want 1/0", name, len(plan.ReviewKicks), len(plan.DeltaRestamps))
		}
		p := plan.ReviewKicks[0].Message
		if !strings.Contains(p, "DELTA RE-REVIEW") || strings.Contains(p, "changed line(s) across") {
			t.Fatalf("%s: unknown diff prompt wrong:\n%s", name, p)
		}
		if !strings.Contains(p, "recorded no findings") {
			t.Fatalf("%s: prompt lacks the no-findings note:\n%s", name, p)
		}
	}
}

func TestPlanDeltaTrivialSkipRestampsWithoutKick(t *testing.T) {
	prior := priorAggregate("sha1", VerdictApprove, deltaNow.Add(-time.Hour))
	prior.Reasons = []string{"all perspectives approved"}
	art := Artifact{Items: []Aggregate{prior}}
	cases := []struct {
		name     string
		diff     *DeltaDiff
		wantNote string
	}{
		{"identical tree", knownDelta("sha1"), "identical tree"},
		{"under threshold", knownDelta("sha1", DeltaFile{Path: "README.md", Additions: 3, Deletions: 1}), "delta touches 1 file(s), 4 line(s)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := dispatchPR("sha2")
			pr.BodyDigest = "body2"
			pr.DeltaDiff = tc.diff
			plan := PlanDispatch([]PullRequest{pr}, art, DispatchState{}, deltaOpts(reviewer("r1")))
			if len(plan.ReviewKicks) != 0 || len(plan.State.Pending) != 0 {
				t.Fatalf("trivial delta still dispatched: kicks=%d pending=%d", len(plan.ReviewKicks), len(plan.State.Pending))
			}
			if len(plan.DeltaRestamps) != 1 {
				t.Fatalf("restamps = %+v, want one", plan.DeltaRestamps)
			}
			r := plan.DeltaRestamps[0]
			if r.Repo != "acme/hive" || r.Number != 7 || r.HeadSHA != "sha2" || r.PreviousHead != "sha1" {
				t.Fatalf("restamp keyed wrong: %+v", r)
			}
			agg := r.Aggregate
			if agg.HeadSHA != "sha2" || agg.BodyDigest != "body2" || agg.Verdict != VerdictApprove || !agg.MergeEligible || !agg.RecordedAt.Equal(deltaNow) {
				t.Fatalf("restamped aggregate = %+v", agg)
			}
			if agg.Delta == nil || !agg.Delta.Trivial || agg.Delta.PreviousHead != "sha1" || !strings.Contains(agg.Delta.Note, tc.wantNote) {
				t.Fatalf("delta info = %+v, want trivial from sha1 with %q", agg.Delta, tc.wantNote)
			}
			if len(agg.Reasons) != 2 || agg.Reasons[0] != "all perspectives approved" || !strings.HasPrefix(agg.Reasons[1], "delta re-review: ") {
				t.Fatalf("reasons = %v", agg.Reasons)
			}
		})
	}
}

func TestPlanDeltaTrivialNotTakenWhenFlaggedFileOrOverThreshold(t *testing.T) {
	art := Artifact{Items: []Aggregate{priorAggregate("sha1", VerdictChangesRequested, deltaNow.Add(-time.Hour), flagged("pkg/a/a.go", 12, "bug"))}}
	cases := map[string]*DeltaDiff{
		"flagged file touched": knownDelta("sha1", DeltaFile{Path: "pkg/a/a.go", Additions: 1}),
		"at threshold":         knownDelta("sha1", DeltaFile{Path: "docs/x.md", Additions: 20}),
	}
	for name, diff := range cases {
		pr := dispatchPR("sha2")
		pr.DeltaDiff = diff
		plan := PlanDispatch([]PullRequest{pr}, art, DispatchState{}, deltaOpts(reviewer("r1")))
		if len(plan.ReviewKicks) != 1 || len(plan.DeltaRestamps) != 0 {
			t.Fatalf("%s: kicks=%d restamps=%d, want 1/0", name, len(plan.ReviewKicks), len(plan.DeltaRestamps))
		}
		if plan.ReviewKicks[0].DeltaPreviousHead != "sha1" {
			t.Fatalf("%s: not a delta kick: %+v", name, plan.ReviewKicks[0])
		}
	}
	// A negative threshold disables the trivial skip even for a one-line delta.
	pr := dispatchPR("sha2")
	pr.DeltaDiff = knownDelta("sha1", DeltaFile{Path: "docs/x.md", Additions: 1})
	opts := deltaOpts(reviewer("r1"))
	opts.DeltaTrivialLines = -1
	plan := PlanDispatch([]PullRequest{pr}, art, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 1 || len(plan.DeltaRestamps) != 0 {
		t.Fatalf("disabled threshold: kicks=%d restamps=%d, want 1/0", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
	// Identical trees always re-stamp, threshold or not.
	pr.DeltaDiff = knownDelta("sha1")
	plan = PlanDispatch([]PullRequest{pr}, art, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 0 || len(plan.DeltaRestamps) != 1 {
		t.Fatalf("identical with disabled threshold: kicks=%d restamps=%d, want 0/1", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
}

func TestPlanDeltaTrivialSkipNotWhilePending(t *testing.T) {
	art := Artifact{Items: []Aggregate{priorAggregate("sha1", VerdictApprove, deltaNow.Add(-time.Hour))}}
	pr := dispatchPR("sha2")
	pr.DeltaDiff = knownDelta("sha1")
	state := DispatchState{Pending: []PendingReview{{Repo: "acme/hive", Number: 7, HeadSHA: "sha2", Perspective: PerspectiveCorrectness, Agent: "r1", Dispatched: deltaNow.Add(-time.Minute)}}}
	plan := PlanDispatch([]PullRequest{pr}, art, state, deltaOpts(reviewer("r1")))
	if len(plan.DeltaRestamps) != 0 {
		t.Fatalf("restamped a head with a review in flight: %+v", plan.DeltaRestamps)
	}
}

func TestPlanDeltaFallsBackToFullReview(t *testing.T) {
	fresh := deltaNow.Add(-time.Hour)
	stale := deltaNow.Add(-DefaultDeltaMaxAge - time.Hour)
	cases := []struct {
		name  string
		prior Aggregate
		opts  func(DispatchOptions) DispatchOptions
	}{
		{"option off", priorAggregate("sha1", VerdictApprove, fresh), func(o DispatchOptions) DispatchOptions { o.DeltaRereview = false; return o }},
		{"prior reject", priorAggregate("sha1", VerdictReject, fresh), nil},
		{"prior incomplete", priorAggregate("sha1", VerdictIncomplete, fresh), nil},
		{"stale changes_requested with findings", priorAggregate("sha1", VerdictChangesRequested, stale, flagged("pkg/a/a.go", 1, "bug")), nil},
		{"custom max age exceeded", priorAggregate("sha1", VerdictChangesRequested, deltaNow.Add(-3*time.Hour), flagged("pkg/a/a.go", 1, "bug")), func(o DispatchOptions) DispatchOptions { o.DeltaMaxAge = 2 * time.Hour; return o }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := dispatchPR("sha2")
			pr.DeltaDiff = knownDelta("sha1")
			opts := deltaOpts(reviewer("r1"))
			if tc.opts != nil {
				opts = tc.opts(opts)
			}
			plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{tc.prior}}, DispatchState{}, opts)
			if len(plan.ReviewKicks) != 1 || len(plan.DeltaRestamps) != 0 {
				t.Fatalf("kicks=%d restamps=%d, want a full review", len(plan.ReviewKicks), len(plan.DeltaRestamps))
			}
			k := plan.ReviewKicks[0]
			if k.DeltaPreviousHead != "" || strings.Contains(k.Message, "DELTA RE-REVIEW") {
				t.Fatalf("expected full review, got delta: %+v", k)
			}
		})
	}
	// A stale changes_requested WITHOUT findings has nothing to go stale, so
	// it still delta-reviews; a stale approve likewise.
	for _, prior := range []Aggregate{priorAggregate("sha1", VerdictChangesRequested, stale), priorAggregate("sha1", VerdictApprove, stale)} {
		pr := dispatchPR("sha2")
		pr.DeltaDiff = &DeltaDiff{PreviousHead: "sha1"}
		plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{prior}}, DispatchState{}, deltaOpts(reviewer("r1")))
		if len(plan.ReviewKicks) != 1 || plan.ReviewKicks[0].DeltaPreviousHead != "sha1" {
			t.Fatalf("stale %s without findings did not delta-review: %+v", prior.Verdict, plan.ReviewKicks)
		}
	}
}

func TestPlanDeltaSameHeadRevisitIsNotDelta(t *testing.T) {
	// A verdict at this very head means the PR is judged; the delta machinery
	// must not treat an older head's verdict as a prior to diff against.
	art := Artifact{Items: []Aggregate{
		priorAggregate("sha1", VerdictApprove, deltaNow.Add(-2*time.Hour)),
		priorAggregate("sha2", VerdictApprove, deltaNow.Add(-time.Hour)),
	}}
	pr := dispatchPR("sha2")
	pr.DeltaDiff = knownDelta("sha1")
	plan := PlanDispatch([]PullRequest{pr}, art, DispatchState{}, deltaOpts(reviewer("r1")))
	if len(plan.ReviewKicks) != 0 || len(plan.DeltaRestamps) != 0 {
		t.Fatalf("judged head re-planned: kicks=%d restamps=%d", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
}

func TestDeltaSectionInBothPromptBuilders(t *testing.T) {
	pr := dispatchPR("sha2")
	pr.Repo = "acme/hive"
	ctx := &DeltaContext{PreviousHead: "sha1", PreviousVerdict: VerdictApprove, PreviousRecordedAt: deltaNow.Add(-time.Hour), DiffKnown: true, Files: []string{"a.go"}, ChangedLines: 25}
	single := BuildPerspectivePromptWith(PerspectiveCorrectness, pr, PromptOptions{Delta: ctx})
	combined := BuildCombinedPrompt(pr, DefaultPerspectives, PromptOptions{Delta: ctx})
	for name, p := range map[string]string{"perspective": single, "combined": combined} {
		if !strings.Contains(p, "DELTA RE-REVIEW") || !strings.Contains(p, "repos/acme/hive/compare/sha1...sha2") || !strings.Contains(p, "2026-10-28 11:00 UTC") {
			t.Fatalf("%s prompt lacks delta section:\n%s", name, p)
		}
	}
	if s := deltaSection(pr, nil); s != "" {
		t.Fatalf("nil context produced a section: %q", s)
	}
	if s := deltaSection(pr, &DeltaContext{}); s != "" {
		t.Fatalf("empty previous head produced a section: %q", s)
	}
}

func TestDeltaTrivialRules(t *testing.T) {
	prior := Aggregate{Findings: []PerspectiveFinding{flagged("pkg/a/a.go", 1, "bug")}}
	if !deltaTrivial(DeltaDiff{Known: true}, prior, 20) {
		t.Fatal("identical known delta not trivial")
	}
	if deltaTrivial(DeltaDiff{}, prior, 20) {
		t.Fatal("unknown delta counted as trivial")
	}
	if !deltaTrivial(DeltaDiff{Known: true, Files: []DeltaFile{{Path: "x.go", Additions: 19}}}, prior, 20) {
		t.Fatal("19 lines in an unflagged file not trivial")
	}
	if deltaTrivial(DeltaDiff{Known: true, Files: []DeltaFile{{Path: "x.go", Additions: 20}}}, prior, 20) {
		t.Fatal("20 lines at threshold counted as trivial")
	}
	if deltaTrivial(DeltaDiff{Known: true, Files: []DeltaFile{{Path: " pkg/a/a.go ", Additions: 1}}}, prior, 20) {
		t.Fatal("flagged file (with whitespace) counted as trivial")
	}
	if deltaTrivial(DeltaDiff{Known: true, Files: []DeltaFile{{Path: "x.go", Additions: 1}}}, prior, 0) {
		t.Fatal("zero threshold allowed a non-empty trivial delta")
	}
	d := DeltaDiff{Known: true, Files: []DeltaFile{{Path: "a", Additions: 2, Deletions: 3}, {Path: "b", Deletions: 4}}}
	if d.ChangedLines() != 9 || d.Identical() {
		t.Fatalf("ChangedLines=%d Identical=%v", d.ChangedLines(), d.Identical())
	}
}

func TestModelStampsCarryDeltaPreviousHead(t *testing.T) {
	stamps := ModelStampsFromState(DispatchState{Pending: []PendingReview{{Repo: "acme/hive", Number: 7, HeadSHA: "sha2", Perspective: PerspectiveCorrectness, Agent: "r1", DeltaPreviousHead: "sha1"}}})
	r := PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha2", Perspective: PerspectiveCorrectness, Verdict: VerdictApprove}
	stamps.apply(&r)
	if r.DeltaPreviousHead != "sha1" {
		t.Fatalf("report delta previous head = %q, want sha1", r.DeltaPreviousHead)
	}
	kept := PerspectiveReport{Repo: "acme/hive", Number: 7, HeadSHA: "sha2", Perspective: PerspectiveCorrectness, DeltaPreviousHead: "sha0"}
	stamps.apply(&kept)
	if kept.DeltaPreviousHead != "sha0" {
		t.Fatalf("stamp overwrote the report's own value: %q", kept.DeltaPreviousHead)
	}
	agg := aggregateReports([]PerspectiveReport{
		r,
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha2", Perspective: PerspectiveSecurity, Verdict: VerdictApprove},
	}, AggregateOptions{})
	if agg.Delta == nil || agg.Delta.PreviousHead != "sha1" || agg.Delta.Trivial {
		t.Fatalf("aggregate delta = %+v, want non-trivial from sha1", agg.Delta)
	}
	plain := aggregateReports([]PerspectiveReport{{Repo: "acme/hive", Number: 7, HeadSHA: "sha2", Perspective: PerspectiveSecurity, Verdict: VerdictApprove}}, AggregateOptions{})
	if plain.Delta != nil {
		t.Fatalf("full review aggregate carries delta info: %+v", plain.Delta)
	}
}

func TestRecordDeltaRestampsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review-verdicts.json")
	existing := Artifact{Items: []Aggregate{priorAggregate("sha1", VerdictApprove, deltaNow.Add(-time.Hour))}}
	if err := WriteArtifact(path, existing); err != nil {
		t.Fatal(err)
	}
	if n, err := RecordDeltaRestamps(path, nil, deltaNow); err != nil || n != 0 {
		t.Fatalf("empty restamps: n=%d err=%v", n, err)
	}
	stamped := restampAggregate(existing.Items[0], PullRequest{Repo: "acme/hive", Number: 7, HeadSHA: "sha2", BodyDigest: "b"}, DeltaContext{PreviousHead: "sha1", DiffKnown: true}, deltaNow)
	restamp := DeltaRestamp{Repo: "acme/hive", Number: 7, HeadSHA: "sha2", PreviousHead: "sha1", Aggregate: stamped}
	if n, err := RecordDeltaRestamps(path, []DeltaRestamp{restamp}, deltaNow); err != nil || n != 1 {
		t.Fatalf("record: n=%d err=%v", n, err)
	}
	// Recording the same head again replaces rather than duplicates.
	if _, err := RecordDeltaRestamps(path, []DeltaRestamp{restamp}, deltaNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	art, err := LoadArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(art.Items) != 2 {
		t.Fatalf("items = %d, want prior + restamp", len(art.Items))
	}
	got, ok := art.AggregateForBody("acme/hive", 7, "sha2", "b")
	if !ok || got.Delta == nil || !got.Delta.Trivial || got.Delta.PreviousHead != "sha1" || got.Verdict != VerdictApprove {
		t.Fatalf("restamped verdict did not round-trip: %+v ok=%v", got, ok)
	}
	if !strings.Contains(got.Delta.Note, "identical tree") {
		t.Fatalf("note = %q", got.Delta.Note)
	}
}

// hivecommons/hive#15499: a PR with no diff against its base is re-stamped
// on every new head, even when the compare from the previous head is not
// empty (a merge-from-base shows the base's commits there).
func TestPlanDeltaEmptyBaseDiffRestampsDespiteNonTrivialCompare(t *testing.T) {
	prior := priorAggregate("sha1", VerdictRequiresHuman, deltaNow.Add(-time.Hour))
	prior.DecisionRequested = true
	prior.RedundantOf = 15427
	art := Artifact{Items: []Aggregate{prior}}
	pr := dispatchPR("sha2")
	pr.BaseDiffEmpty = true
	pr.DeltaDiff = knownDelta("sha1", DeltaFile{Path: "pkg/a/a.go", Additions: 300, Deletions: 40}, DeltaFile{Path: "pkg/b/b.go", Additions: 90})
	plan := PlanDispatch([]PullRequest{pr}, art, DispatchState{}, deltaOpts(reviewer("r1")))
	if len(plan.ReviewKicks) != 0 || len(plan.DeltaRestamps) != 1 {
		t.Fatalf("kicks=%d restamps=%d, want 0/1", len(plan.ReviewKicks), len(plan.DeltaRestamps))
	}
	agg := plan.DeltaRestamps[0].Aggregate
	if agg.Verdict != VerdictRequiresHuman || agg.RedundantOf != 15427 || agg.Delta == nil || !agg.Delta.Trivial || !strings.Contains(agg.Delta.Note, "no diff against its base") {
		t.Fatalf("restamped aggregate = %+v delta=%+v", agg, agg.Delta)
	}
	pr.BaseDiffEmpty = false
	plan = PlanDispatch([]PullRequest{pr}, art, DispatchState{}, deltaOpts(reviewer("r1")))
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("with a real diff the delta must still be reviewed: %+v", plan)
	}
}
