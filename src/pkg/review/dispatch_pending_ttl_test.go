package review

import (
	"strings"
	"testing"
	"time"
)

// pendingTTLOpts is the fan-out configuration the pending-TTL cases share:
// one combined kick per PR, so each cycle dispatches every missing
// perspective at once and "re-dispatched" means exactly one kick.
func pendingTTLOpts(now time.Time) DispatchOptions {
	return DispatchOptions{
		RequireApproval:      true,
		FanOut:               true,
		CombinedPerspectives: true,
		MaxParallelReviews:   5,
		ProjectOrg:           "acme",
		Agents:               []AgentCapability{reviewer("r1"), reviewer("r2")},
		Now:                  now,
	}
}

func pendingForHead(sha string, dispatched time.Time) []PendingReview {
	var out []PendingReview
	for _, p := range DefaultPerspectives {
		out = append(out, PendingReview{Repo: "acme/hive", Number: 7, HeadSHA: sha, Perspective: p, Agent: "r1", Dispatched: dispatched})
	}
	return out
}

func TestPlanDispatchPendingTTL(t *testing.T) {
	now := time.Date(2026, 10, 15, 21, 45, 0, 0, time.UTC)
	tests := []struct {
		name          string
		state         DispatchState
		ttl           time.Duration
		maxRedispatch int
		wantKicks     int
		wantExpired   int
		wantCount     int
		wantHold      bool
	}{
		{
			name:        "fresh pending entry is not re-kicked",
			state:       DispatchState{Pending: pendingForHead("sha1", now.Add(-10*time.Minute))},
			wantKicks:   0,
			wantExpired: 0,
		},
		{
			name:        "entry older than the default TTL expires and is re-dispatched",
			state:       DispatchState{Pending: pendingForHead("sha1", now.Add(-DefaultPendingReviewTTL-time.Minute))},
			wantKicks:   1,
			wantExpired: len(DefaultPerspectives),
			wantCount:   1,
		},
		{
			name:        "configured TTL overrides the default",
			state:       DispatchState{Pending: pendingForHead("sha1", now.Add(-20*time.Minute))},
			ttl:         15 * time.Minute,
			wantKicks:   1,
			wantExpired: len(DefaultPerspectives),
			wantCount:   1,
		},
		{
			name:        "negative TTL disables expiry",
			state:       DispatchState{Pending: pendingForHead("sha1", now.Add(-24*time.Hour))},
			ttl:         -1,
			wantKicks:   0,
			wantExpired: 0,
		},
		{
			name:        "entry without dispatched_at has no age and is kept",
			state:       DispatchState{Pending: pendingForHead("sha1", time.Time{})},
			wantKicks:   0,
			wantExpired: 0,
		},
		{
			name: "second expiry re-dispatches and counts",
			state: DispatchState{
				Pending:      pendingForHead("sha1", now.Add(-time.Hour)),
				Redispatches: []RedispatchCount{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Count: 1}},
			},
			wantKicks:   1,
			wantExpired: len(DefaultPerspectives),
			wantCount:   2,
		},
		{
			name: "expiry at max_redispatch holds the PR for a human instead of re-kicking",
			state: DispatchState{
				Pending:      pendingForHead("sha1", now.Add(-time.Hour)),
				Redispatches: []RedispatchCount{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Count: DefaultMaxRedispatch - 1}},
			},
			wantKicks:   0,
			wantExpired: len(DefaultPerspectives),
			wantCount:   DefaultMaxRedispatch,
			wantHold:    true,
		},
		{
			name: "configured max_redispatch overrides the default",
			state: DispatchState{
				Pending:      pendingForHead("sha1", now.Add(-time.Hour)),
				Redispatches: []RedispatchCount{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Count: 0}},
			},
			maxRedispatch: 1,
			wantKicks:     0,
			wantExpired:   len(DefaultPerspectives),
			wantCount:     1,
			wantHold:      true,
		},
		{
			name: "an exhausted head is not counted again on the next cycle",
			state: DispatchState{
				Pending:      pendingForHead("sha1", now.Add(-2*time.Hour)),
				Redispatches: []RedispatchCount{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Count: DefaultMaxRedispatch}},
				Human:        []HumanReviewHold{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Reason: HoldReasonDispatchExhausted + ": 3 review dispatches for this head ended without a verdict"}},
			},
			wantKicks:   0,
			wantExpired: 0,
			wantCount:   DefaultMaxRedispatch,
			wantHold:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := pendingTTLOpts(now)
			opts.PendingTTL = tt.ttl
			opts.MaxRedispatch = tt.maxRedispatch
			plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, tt.state, opts)
			if len(plan.ReviewKicks) != tt.wantKicks {
				t.Fatalf("review kicks = %d, want %d (pending=%+v)", len(plan.ReviewKicks), tt.wantKicks, plan.State.Pending)
			}
			if len(plan.Expired) != tt.wantExpired {
				t.Fatalf("expired = %d, want %d: %+v", len(plan.Expired), tt.wantExpired, plan.Expired)
			}
			if got := plan.State.RedispatchCountFor("acme/hive", 7, "sha1"); got != tt.wantCount {
				t.Fatalf("redispatch count = %d, want %d", got, tt.wantCount)
			}
			// Whatever happened, the head must still have one pending entry per
			// perspective: either the fresh/kept ones or the re-dispatched set.
			if len(plan.State.Pending) != len(DefaultPerspectives) {
				t.Fatalf("pending = %d, want %d: %+v", len(plan.State.Pending), len(DefaultPerspectives), plan.State.Pending)
			}
			for _, e := range plan.Expired {
				if e.Age < DefaultPendingReviewTTL && tt.ttl == 0 {
					t.Fatalf("expired entry younger than TTL: %+v", e)
				}
				if e.Count != tt.wantCount {
					t.Fatalf("expired entry count = %d, want %d", e.Count, tt.wantCount)
				}
			}
			if tt.wantKicks == 1 {
				for _, p := range plan.State.Pending {
					if !p.Dispatched.Equal(now) {
						t.Fatalf("re-dispatched pending entry kept its old dispatched_at: %+v", p)
					}
				}
			}
			var hold *HumanReviewHold
			for i := range plan.State.Human {
				if plan.State.Human[i].Number == 7 && plan.State.Human[i].HeadSHA == "sha1" {
					hold = &plan.State.Human[i]
				}
			}
			if tt.wantHold != (hold != nil) {
				t.Fatalf("hold present = %v, want %v: %+v", hold != nil, tt.wantHold, plan.State.Human)
			}
			if hold != nil && !strings.HasPrefix(hold.Reason, HoldReasonDispatchExhausted) {
				t.Fatalf("hold reason %q does not carry %q", hold.Reason, HoldReasonDispatchExhausted)
			}
			// Exhausted is reported only on the cycle that spends the last
			// retry, never on later cycles that merely see the hold.
			wantExhausted := 0
			if tt.wantHold && tt.wantExpired > 0 {
				wantExhausted = 1
			}
			if len(plan.Exhausted) != wantExhausted {
				t.Fatalf("exhausted this cycle = %d, want %d", len(plan.Exhausted), wantExhausted)
			}
			// The exhausted hold reaches NewHuman exactly once per head, so the
			// escalation fan-out sends one decision event and none on the next
			// cycle (#12377).
			if len(plan.NewHuman) != wantExhausted {
				t.Fatalf("new human holds this cycle = %d, want %d: %+v", len(plan.NewHuman), wantExhausted, plan.NewHuman)
			}
			if wantExhausted == 1 && !strings.HasPrefix(plan.NewHuman[0].Reason, HoldReasonDispatchExhausted) {
				t.Fatalf("new human hold reason %q does not carry %q", plan.NewHuman[0].Reason, HoldReasonDispatchExhausted)
			}
		})
	}
}

func TestPlanDispatchPendingTTLPerPerspective(t *testing.T) {
	now := time.Date(2026, 10, 15, 21, 45, 0, 0, time.UTC)
	// Two perspectives dispatched an hour ago, three ten minutes ago: only the
	// old two expire and are re-asked; the fresh three stay in flight.
	state := DispatchState{Pending: []PendingReview{
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Agent: "r1", Dispatched: now.Add(-time.Hour)},
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Agent: "r1", Dispatched: now.Add(-time.Hour)},
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveIntentAlignment, Agent: "r1", Dispatched: now.Add(-10 * time.Minute)},
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveStyle, Agent: "r1", Dispatched: now.Add(-10 * time.Minute)},
		{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveDocsCurrency, Agent: "r1", Dispatched: now.Add(-10 * time.Minute)},
	}}
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, state, pendingTTLOpts(now))
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("kicks = %d, want 1", len(plan.ReviewKicks))
	}
	if got := plan.ReviewKicks[0].Perspectives; len(got) != 2 || got[0] != PerspectiveCorrectness || got[1] != PerspectiveSecurity {
		t.Fatalf("re-dispatched perspectives = %v, want the two expired ones", got)
	}
	if len(plan.Expired) != 2 {
		t.Fatalf("expired = %d, want 2", len(plan.Expired))
	}
	fresh := 0
	for _, p := range plan.State.Pending {
		if p.Dispatched.Equal(now.Add(-10 * time.Minute)) {
			fresh++
		}
	}
	if fresh != 3 {
		t.Fatalf("fresh pending entries kept = %d, want 3: %+v", fresh, plan.State.Pending)
	}
}

func TestPlanDispatchPendingTTLHeadMoveResetsBudget(t *testing.T) {
	now := time.Date(2026, 10, 15, 21, 45, 0, 0, time.UTC)
	state := DispatchState{
		Pending:      pendingForHead("old", now.Add(-2*time.Hour)),
		Redispatches: []RedispatchCount{{Repo: "acme/hive", Number: 7, HeadSHA: "old", Count: DefaultMaxRedispatch}},
		Human:        []HumanReviewHold{{Repo: "acme/hive", Number: 7, HeadSHA: "old", Reason: HoldReasonDispatchExhausted}},
	}
	plan := PlanDispatch([]PullRequest{dispatchPR("new")}, Artifact{}, state, pendingTTLOpts(now))
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("new head was not dispatched: kicks=%d", len(plan.ReviewKicks))
	}
	if len(plan.State.Redispatches) != 0 || len(plan.State.Human) != 0 {
		t.Fatalf("old head's exhaustion survived the push: redispatches=%+v human=%+v", plan.State.Redispatches, plan.State.Human)
	}
}

func TestPlanDispatchPendingTTLVerdictWins(t *testing.T) {
	now := time.Date(2026, 10, 15, 21, 45, 0, 0, time.UTC)
	// A verdict for the head settles it however old the pending entries are.
	state := DispatchState{Pending: pendingForHead("sha1", now.Add(-3*time.Hour))}
	artifact := Artifact{Items: []Aggregate{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Verdict: VerdictApprove}}}
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, artifact, state, pendingTTLOpts(now))
	if len(plan.ReviewKicks) != 0 || len(plan.Expired) != 0 {
		t.Fatalf("approved head was re-dispatched: kicks=%d expired=%d", len(plan.ReviewKicks), len(plan.Expired))
	}
}

func TestPlanDispatchPendingTTLRevisitInFlight(t *testing.T) {
	now := time.Date(2026, 10, 15, 21, 45, 0, 0, time.UTC)
	cutoff := now.Add(-6 * time.Hour)
	stale := Artifact{Items: []Aggregate{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Verdict: VerdictApprove, RecordedAt: cutoff.Add(-time.Hour)}}}
	base := pendingTTLOpts(now)
	base.ReviseRepos = []string{"acme/hive"}
	base.ReviseVerdictsBefore = cutoff

	// Revisit kicked ten minutes ago: still in flight, not re-kicked.
	fresh := DispatchState{Pending: pendingForHead("sha1", now.Add(-10*time.Minute))}
	if plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, stale, fresh, base); len(plan.ReviewKicks) != 0 {
		t.Fatalf("fresh revisit was re-kicked: %d", len(plan.ReviewKicks))
	}
	// Revisit kicked an hour ago with no new verdict: expired, asked again.
	old := DispatchState{Pending: pendingForHead("sha1", now.Add(-time.Hour))}
	plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, stale, old, base)
	if len(plan.ReviewKicks) != 1 || len(plan.Expired) != len(DefaultPerspectives) {
		t.Fatalf("expired revisit was not re-kicked: kicks=%d expired=%d", len(plan.ReviewKicks), len(plan.Expired))
	}
}

func TestDispatchStatePendingSinceAndRedispatchCount(t *testing.T) {
	now := time.Date(2026, 10, 15, 21, 45, 0, 0, time.UTC)
	state := DispatchState{
		Pending: []PendingReview{
			{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveSecurity, Dispatched: now.Add(-5 * time.Minute)},
			{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Dispatched: now.Add(-30 * time.Minute)},
			{Repo: "acme/hive", Number: 8, HeadSHA: "sha1", Perspective: PerspectiveCorrectness, Dispatched: now.Add(-90 * time.Minute)},
		},
		Redispatches: []RedispatchCount{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Count: 2}},
	}
	if got := state.PendingSince("acme/hive", 7, "sha1"); !got.Equal(now.Add(-30 * time.Minute)) {
		t.Fatalf("PendingSince = %v, want the earliest dispatch", got)
	}
	if got := state.PendingSince("acme/hive", 7, "other"); !got.IsZero() {
		t.Fatalf("PendingSince for an unknown head = %v, want zero", got)
	}
	if got := state.RedispatchCountFor("acme/hive", 7, "sha1"); got != 2 {
		t.Fatalf("RedispatchCountFor = %d, want 2", got)
	}
	if got := state.RedispatchCountFor("acme/hive", 8, "sha1"); got != 0 {
		t.Fatalf("RedispatchCountFor on a never-expired head = %d, want 0", got)
	}
}
