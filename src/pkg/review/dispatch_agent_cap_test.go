package review

import (
	"strings"
	"testing"
	"time"
)

// capRefusals is a stand-in for the proxy's refusal record: the last time
// one of agents was refused, and when that refusal said it would clear.
type capRefusals struct {
	agents map[string]bool
	at     time.Time
	clears time.Time
}

func (r *capRefusals) refusal(agent string, since time.Time) (time.Time, bool) {
	if !r.agents[agent] || r.at.Before(since) {
		return time.Time{}, false
	}
	return r.clears, true
}

func capPendingFor(t *testing.T, state DispatchState) time.Time {
	t.Helper()
	var dispatched time.Time
	for _, p := range state.Pending {
		if p.Number != 7 || p.HeadSHA != "sha1" {
			continue
		}
		if dispatched.IsZero() {
			dispatched = p.Dispatched
		} else if !p.Dispatched.Equal(dispatched) {
			t.Fatalf("pending rows were sent at different times: %+v", state.Pending)
		}
	}
	if dispatched.IsZero() {
		t.Fatalf("no pending rows for the head: %+v", state.Pending)
	}
	return dispatched
}

func holdFor(state DispatchState) *HumanReviewHold {
	for i := range state.Human {
		if state.Human[i].Number == 7 && state.Human[i].HeadSHA == "sha1" {
			return &state.Human[i]
		}
	}
	return nil
}

// projectbluefin/utah-packages#959 at the planner: the reviewer kept hitting
// its 300/h cap, so every session on the head was refused mid-review and the
// relay set its requires_human aside. Each set-aside used to expire into one
// of review.max_redispatch's three tries, so about 2h15m of being capped
// parked the head as review-dispatch-exhausted and asked a person. Expiries
// the cap caused must count on their own, larger budget: still asked again
// after the third, held only after DefaultMaxCapRedispatch.
func TestPlanDispatchCapCausedExpiriesDoNotSpendRedispatchBudget(t *testing.T) {
	start := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	refusals := &capRefusals{agents: map[string]bool{"r1": true, "r2": true}}
	state := DispatchState{Pending: pendingForHead("sha1", start)}
	for cycle := 1; cycle <= DefaultMaxCapRedispatch; cycle++ {
		dispatched := capPendingFor(t, state)
		// Refused ten minutes into the session; the sliding hour frees a
		// slot before the TTL is up, so nothing waits.
		refusals.at = dispatched.Add(10 * time.Minute)
		refusals.clears = dispatched.Add(30 * time.Minute)
		now := dispatched.Add(DefaultPendingReviewTTL + time.Minute)
		opts := pendingTTLOpts(now)
		opts.AgentGitHubRefusal = refusals.refusal
		plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, state, opts)

		if got := plan.State.RedispatchCountFor("acme/hive", 7, "sha1"); got != 0 {
			t.Fatalf("cycle %d: max_redispatch count = %d, want 0 for expiries the cap caused", cycle, got)
		}
		if got := plan.State.CapRedispatchCountFor("acme/hive", 7, "sha1"); got != cycle {
			t.Fatalf("cycle %d: cap count = %d, want %d", cycle, got, cycle)
		}
		if len(plan.Expired) != len(DefaultPerspectives) {
			t.Fatalf("cycle %d: expired = %d, want %d", cycle, len(plan.Expired), len(DefaultPerspectives))
		}
		for _, e := range plan.Expired {
			if !e.CapCaused || e.Count != cycle {
				t.Fatalf("cycle %d: expired entry %+v, want cap-caused with count %d", cycle, e, cycle)
			}
		}
		if cycle < DefaultMaxCapRedispatch {
			if hold := holdFor(plan.State); hold != nil || len(plan.NewHuman) != 0 || len(plan.Exhausted) != 0 {
				t.Fatalf("cycle %d: a head the cap kept from being read asked a person: hold=%+v new=%+v", cycle, hold, plan.NewHuman)
			}
			if len(plan.ReviewKicks) != 1 {
				t.Fatalf("cycle %d: review kicks = %d, want the head asked again", cycle, len(plan.ReviewKicks))
			}
		} else {
			hold := holdFor(plan.State)
			if hold == nil || !strings.HasPrefix(hold.Reason, HoldReasonDispatchExhausted) || !strings.Contains(hold.Reason, "agent GitHub API cap") {
				t.Fatalf("cycle %d: hold = %+v, want a %s hold naming the cap", cycle, hold, HoldReasonDispatchExhausted)
			}
			if len(plan.ReviewKicks) != 0 || len(plan.NewHuman) != 1 || len(plan.Exhausted) != 1 {
				t.Fatalf("cycle %d: kicks=%d new=%d exhausted=%d, want the head held once and not re-kicked", cycle, len(plan.ReviewKicks), len(plan.NewHuman), len(plan.Exhausted))
			}
		}
		state = plan.State
	}
}

// The counter-case: a head whose sessions end with no verdict for any reason
// but the cap still reaches the review-dispatch-exhausted hold after
// max_redispatch, as before. That covers no proxy record at all (none wired,
// or a restart forgot it), a refusal that belongs to an earlier session, a
// refusal of another agent, and a combined expiry where only one row's agent
// was refused.
func TestPlanDispatchOrdinaryExpiriesStillExhaust(t *testing.T) {
	start := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		refusals func(dispatched time.Time) AgentGitHubRefusal
		// mixed sends one of the head's rows to r2 each cycle, the rest to r1.
		mixed bool
	}{
		{
			name:     "no proxy record",
			refusals: func(time.Time) AgentGitHubRefusal { return nil },
		},
		{
			name: "refused before the row was sent",
			refusals: func(dispatched time.Time) AgentGitHubRefusal {
				r := &capRefusals{agents: map[string]bool{"r1": true, "r2": true}, at: dispatched.Add(-time.Minute), clears: dispatched}
				return r.refusal
			},
		},
		{
			name: "another agent refused",
			refusals: func(dispatched time.Time) AgentGitHubRefusal {
				r := &capRefusals{agents: map[string]bool{"scanner": true}, at: dispatched.Add(10 * time.Minute), clears: dispatched.Add(20 * time.Minute)}
				return r.refusal
			},
		},
		{
			name: "only one expired row's agent refused",
			refusals: func(dispatched time.Time) AgentGitHubRefusal {
				r := &capRefusals{agents: map[string]bool{"r1": true}, at: dispatched.Add(10 * time.Minute), clears: dispatched.Add(20 * time.Minute)}
				return r.refusal
			},
			mixed: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := DispatchState{Pending: pendingForHead("sha1", start)}
			for cycle := 1; cycle <= DefaultMaxRedispatch; cycle++ {
				if tc.mixed {
					first := true
					for i := range state.Pending {
						if state.Pending[i].Number == 7 && state.Pending[i].HeadSHA == "sha1" {
							state.Pending[i].Agent = "r1"
							if first {
								state.Pending[i].Agent = "r2"
								first = false
							}
						}
					}
				}
				dispatched := capPendingFor(t, state)
				now := dispatched.Add(DefaultPendingReviewTTL + time.Minute)
				opts := pendingTTLOpts(now)
				opts.AgentGitHubRefusal = tc.refusals(dispatched)
				plan := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, state, opts)
				if got := plan.State.CapRedispatchCountFor("acme/hive", 7, "sha1"); got != 0 {
					t.Fatalf("cycle %d: cap count = %d for expiries the cap did not cause", cycle, got)
				}
				if got := plan.State.RedispatchCountFor("acme/hive", 7, "sha1"); got != cycle {
					t.Fatalf("cycle %d: max_redispatch count = %d, want %d", cycle, got, cycle)
				}
				hold := holdFor(plan.State)
				if cycle < DefaultMaxRedispatch {
					if hold != nil {
						t.Fatalf("cycle %d: held early: %+v", cycle, hold)
					}
				} else if hold == nil || !strings.HasPrefix(hold.Reason, HoldReasonDispatchExhausted) || strings.Contains(hold.Reason, "API cap") {
					t.Fatalf("cycle %d: hold = %+v, want the ordinary %s hold", cycle, hold, HoldReasonDispatchExhausted)
				}
				state = plan.State
			}
		})
	}
}

// An expiry the cap caused is asked again only once the refusal has
// cleared: sooner, the proxy would just refuse the new session too. The wait
// is bounded at agentCapWaitLimit past the TTL.
func TestPlanDispatchCapCausedExpiryWaitsForTheRefusalToClear(t *testing.T) {
	dispatched := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	ttl := DefaultPendingReviewTTL
	run := func(clears, now time.Time) DispatchPlan {
		r := &capRefusals{agents: map[string]bool{"r1": true}, at: dispatched.Add(40 * time.Minute), clears: clears}
		opts := pendingTTLOpts(now)
		opts.AgentGitHubRefusal = r.refusal
		return PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{Pending: pendingForHead("sha1", dispatched)}, opts)
	}

	clears := dispatched.Add(ttl + 30*time.Minute)
	waiting := run(clears, dispatched.Add(ttl+time.Minute))
	if len(waiting.ReviewKicks) != 0 || len(waiting.Expired) != 0 {
		t.Fatalf("asked again before the cap cleared: kicks=%d expired=%d", len(waiting.ReviewKicks), len(waiting.Expired))
	}
	if len(waiting.State.Pending) != len(DefaultPerspectives) || !capPendingFor(t, waiting.State).Equal(dispatched) {
		t.Fatalf("waiting rows were not kept as asked: %+v", waiting.State.Pending)
	}
	if waiting.State.RedispatchCountFor("acme/hive", 7, "sha1") != 0 || waiting.State.CapRedispatchCountFor("acme/hive", 7, "sha1") != 0 {
		t.Fatal("a wait was counted as an expiry")
	}
	if holdFor(waiting.State) != nil {
		t.Fatalf("a wait held the head: %+v", waiting.State.Human)
	}

	cleared := run(clears, clears.Add(time.Minute))
	if len(cleared.ReviewKicks) != 1 || cleared.State.CapRedispatchCountFor("acme/hive", 7, "sha1") != 1 {
		t.Fatalf("after the cap cleared: kicks=%d cap count=%d, want one re-ask counted as cap-caused", len(cleared.ReviewKicks), cleared.State.CapRedispatchCountFor("acme/hive", 7, "sha1"))
	}

	far := dispatched.Add(ttl + 3*time.Hour)
	bounded := run(far, dispatched.Add(ttl+agentCapWaitLimit+time.Minute))
	if len(bounded.ReviewKicks) != 1 {
		t.Fatalf("waited past agentCapWaitLimit for a refusal that clears at %v", far)
	}
}

// A new push starts the head over: the cap count goes with the old head,
// like max_redispatch's.
func TestPlanDispatchCapRedispatchesResetOnNewHead(t *testing.T) {
	now := time.Date(2026, 11, 9, 1, 0, 0, 0, time.UTC)
	state := DispatchState{
		Pending:         pendingForHead("old", now.Add(-time.Hour)),
		CapRedispatches: []RedispatchCount{{Repo: "acme/hive", Number: 7, HeadSHA: "old", Count: DefaultMaxCapRedispatch - 1}},
	}
	plan := PlanDispatch([]PullRequest{dispatchPR("new")}, Artifact{}, state, pendingTTLOpts(now))
	if len(plan.State.CapRedispatches) != 0 {
		t.Fatalf("old head's cap count survived the push: %+v", plan.State.CapRedispatches)
	}
}
