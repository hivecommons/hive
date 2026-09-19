package review

import (
	"strings"
	"testing"
	"time"
)

// A verdict normally settles a PR until its head SHA moves, and that is correct
// while the reviewer is sound. It becomes a trap the moment a reviewer-side
// defect is found: on the bluefin spoke, 93 of 95 PRs sat frozen at "no
// findings" — including a 30-file change removing 1655 lines — because the kick
// had told the reviewer to judge the diff without reading the surrounding tree.
// None of them would ever be looked at again unless someone happened to push a
// commit.
func TestStaleVerdictRevisit(t *testing.T) {
	cutoff := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	before := cutoff.Add(-2 * time.Hour)
	after := cutoff.Add(2 * time.Hour)

	base := DispatchOptions{
		ReviseRepos:          []string{"projectbluefin/server"},
		ReviseVerdictsBefore: cutoff,
	}

	cases := []struct {
		name string
		repo string
		agg  Aggregate
		opts DispatchOptions
		want bool
	}{
		{
			name: "stale verdict in an allowlisted repo is revisitable",
			repo: "projectbluefin/server",
			agg:  Aggregate{Verdict: VerdictApprove, RecordedAt: before},
			opts: base,
			want: true,
		},
		{
			name: "a verdict recorded after the cutoff is left alone",
			repo: "projectbluefin/server",
			agg:  Aggregate{Verdict: VerdictApprove, RecordedAt: after},
			opts: base,
			want: false,
		},
		{
			// The pilot is one repo. A correction that quietly spread to the
			// rest of the org would be exactly the blast radius the allowlist
			// exists to prevent.
			name: "a repo outside the allowlist is never revisited",
			repo: "projectbluefin/utah",
			agg:  Aggregate{Verdict: VerdictApprove, RecordedAt: before},
			opts: base,
			want: false,
		},
		{
			name: "no cutoff configured disables revisiting entirely",
			repo: "projectbluefin/server",
			agg:  Aggregate{Verdict: VerdictApprove, RecordedAt: before},
			opts: DispatchOptions{ReviseRepos: []string{"projectbluefin/server"}},
			want: false,
		},
		{
			name: "no allowlist disables revisiting even with a cutoff",
			repo: "projectbluefin/server",
			agg:  Aggregate{Verdict: VerdictApprove, RecordedAt: before},
			opts: DispatchOptions{ReviseVerdictsBefore: cutoff},
			want: false,
		},
		{
			// Re-reviewing on a guess is how a narrow correction becomes a
			// sweep.
			name: "an undated verdict is not assumed stale",
			repo: "projectbluefin/server",
			agg:  Aggregate{Verdict: VerdictApprove},
			opts: base,
			want: false,
		},
		{
			name: "repo matching is case-insensitive",
			repo: "ProjectBluefin/Server",
			agg:  Aggregate{Verdict: VerdictApprove, RecordedAt: before},
			opts: base,
			want: true,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := staleVerdictRevisitable(tt.repo, tt.agg, tt.opts); got != tt.want {
				t.Errorf("staleVerdictRevisitable = %v, want %v", got, tt.want)
			}
		})
	}
}

// The cutoff must be self-limiting. A re-review records a fresh timestamp that
// is necessarily after it, so the PR falls out of scope on the next sweep. If
// that failed, the pilot would re-review the same PRs every cycle forever.
func TestRevisitIsSelfLimiting(t *testing.T) {
	cutoff := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	opts := DispatchOptions{
		ReviseRepos:          []string{"projectbluefin/server"},
		ReviseVerdictsBefore: cutoff,
	}

	stale := Aggregate{Verdict: VerdictApprove, RecordedAt: cutoff.Add(-time.Hour)}
	if !staleVerdictRevisitable("projectbluefin/server", stale, opts) {
		t.Fatal("a verdict older than the cutoff must be revisitable once")
	}

	// Simulate the re-review landing.
	refreshed := Aggregate{Verdict: VerdictApprove, RecordedAt: cutoff.Add(time.Minute)}
	if staleVerdictRevisitable("projectbluefin/server", refreshed, opts) {
		t.Fatal("a re-reviewed verdict must not be revisited again: this is an infinite review loop")
	}
}

// End-to-end through PlanDispatch: a frozen PR must actually be dispatched
// again, and the kick it receives must tell the reviewer to correct its
// existing review rather than post a second one.
func TestPlanDispatchRevisitsFrozenPRWithReviseKick(t *testing.T) {
	cutoff := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	now := cutoff.Add(time.Hour)

	pr := PullRequest{
		Repo:    "projectbluefin/server",
		Number:  180,
		Title:   "refactor the thing",
		Author:  "castrojo",
		HeadSHA: "abc123",
	}
	frozen := Artifact{Items: []Aggregate{{
		Repo:       pr.Repo,
		Number:     pr.Number,
		HeadSHA:    pr.HeadSHA,
		Verdict:    VerdictApprove,
		RecordedAt: cutoff.Add(-24 * time.Hour),
	}}}

	opts := DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		MaxParallelReviews:   4,
		MaxPerspectivesPerPR: 1,
		ReviewerAgents:       []string{"reviewer"},
		PostComments:         true,
		AllAuthors:           true,
		ReviseRepos:          []string{"projectbluefin/server"},
		ReviseVerdictsBefore: cutoff,
		Agents: []AgentCapability{
			{Name: "reviewer", Enabled: true, UsesKick: true},
		},
	}

	// Baseline: without the revise settings the PR stays frozen.
	quiet := opts
	quiet.ReviseRepos = nil
	quiet.ReviseVerdictsBefore = time.Time{}
	if plan := PlanDispatch([]PullRequest{pr}, frozen, DispatchState{}, quiet); len(plan.ReviewKicks) != 0 {
		t.Fatalf("a settled verdict must not be re-reviewed by default, got %d kicks", len(plan.ReviewKicks))
	}

	plan := PlanDispatch([]PullRequest{pr}, frozen, DispatchState{}, opts)
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("frozen PR should be revisited exactly once, got %d kicks", len(plan.ReviewKicks))
	}

	kick := plan.ReviewKicks[0]
	if kick.Repo != pr.Repo || kick.Number != pr.Number {
		t.Fatalf("kick targeted %s#%d, want %s#%d", kick.Repo, kick.Number, pr.Repo, pr.Number)
	}

	// The kick must carry the in-place correction path, not a second review.
	for _, want := range []string{
		"--revise",
		"YOU HAVE REVIEWED THIS PR BEFORE",
		"Do NOT add a second one",
		"notifies nobody",
	} {
		if !strings.Contains(kick.Message, want) {
			t.Errorf("revisit kick missing %q:\n%s", want, kick.Message)
		}
	}

	// It must not invite a manufactured finding to justify the second look.
	if !strings.Contains(kick.Message, "Do not manufacture a finding") {
		t.Error("revisit kick must permit reaching the same conclusion again")
	}

	// The production shape: a PR with a settled verdict ALWAYS has pending
	// entries for its head — they are the lifetime record of what was asked.
	// The revisit must clear them and ask again, or it never dispatches.
	settled := DispatchState{}
	for _, p := range DefaultPerspectives {
		settled.Pending = append(settled.Pending, PendingReview{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: p, Agent: "reviewer", Dispatched: cutoff.Add(-23 * time.Hour)})
	}
	plan = PlanDispatch([]PullRequest{pr}, frozen, settled, opts)
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("revisit with lifetime pending entries should still dispatch once, got %d kicks", len(plan.ReviewKicks))
	}
	if !strings.Contains(plan.ReviewKicks[0].Message, "--revise") {
		t.Fatal("revisit kick lost --revise once pending entries were present")
	}

	// And only once: the next cycle sees the revisit in flight (pending
	// dispatched after the cutoff, verdict still the stale one) and waits.
	inFlight := plan.State
	for i := range inFlight.Pending {
		inFlight.Pending[i].Dispatched = now
	}
	if again := PlanDispatch([]PullRequest{pr}, frozen, inFlight, opts); len(again.ReviewKicks) != 0 {
		t.Fatalf("revisit re-kicked while its verdict was still outstanding: %d kicks", len(again.ReviewKicks))
	}
}

// A first review is not a revision: an ordinary dispatch must never carry
// --revise, or the relay would look for a prior review that does not exist.
func TestOrdinaryKickDoesNotRevise(t *testing.T) {
	got := BuildPerspectivePromptWith(PerspectiveCorrectness, PullRequest{
		Repo: "o/r", Number: 1, HeadSHA: "deadbeef",
	}, PromptOptions{PostComments: true})

	if strings.Contains(got, "--revise") {
		t.Fatalf("a first review must not be told to revise:\n%s", got)
	}
	if strings.Contains(got, "YOU HAVE REVIEWED THIS PR BEFORE") {
		t.Fatal("ordinary kick claims a prior review that may not exist")
	}
}
