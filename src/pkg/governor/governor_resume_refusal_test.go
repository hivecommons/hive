package governor

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestAllowResumeKickReason_Table pins the refusal reason for every gate
// (#9612) and the side effect the throttle depends on: a grant is recorded in
// resumeKicks ONLY when the kick is allowed, so a refusal never starts (or
// restarts) the one-per-interval window.
func TestAllowResumeKickReason_Table(t *testing.T) {
	const (
		weeklyLimit = 1000
		overspend   = 2000
	)
	cases := []struct {
		name  string
		agent string
		setup func(t *testing.T) *Governor
		want  ResumeRefusal
	}{
		{
			name: "interval cadence allowed", agent: "scanner",
			setup: func(t *testing.T) *Governor { return resumeTestGovernor(t, "6h") },
			want:  ResumeAllowed,
		},
		{
			name: "no cadence entry is unscheduled", agent: "no-such-agent",
			setup: func(t *testing.T) *Governor { return resumeTestGovernor(t, "6h") },
			want:  ResumeRefusalUnscheduled,
		},
		{
			name: "mode cadence pause", agent: "scanner",
			setup: func(t *testing.T) *Governor { return resumeTestGovernor(t, "pause") },
			want:  ResumeRefusalPausedInMode,
		},
		{
			name: "zero interval", agent: "scanner",
			setup: func(t *testing.T) *Governor {
				g := resumeTestGovernor(t, "6h")
				g.mu.Lock()
				g.state.Cadences["scanner"] = AgentCadence{Agent: "scanner", Schedule: config.NewIntervalCadence("")}
				g.mu.Unlock()
				return g
			},
			want: ResumeRefusalNotInterval,
		},
		{
			name: "time-of-day schedule", agent: "scanner",
			setup: func(t *testing.T) *Governor {
				c, err := configFromYAML(`{times: ["09:00"], tz: UTC}`)
				if err != nil {
					t.Fatal(err)
				}
				g := timeCadenceGovernor(c, time.Date(2026, 8, 7, 8, 0, 0, 0, time.UTC))
				g.Evaluate(0, 0, 0, 0)
				return g
			},
			want: ResumeRefusalNotInterval,
		},
		{
			name: "on-demand agent", agent: "scanner",
			setup: func(t *testing.T) *Governor {
				cfg := config.GovernorConfig{Modes: map[string]config.ModeConfig{
					"idle": {Threshold: 0, Cadences: map[string]config.Cadence{"scanner": "1h"}},
				}}
				g := New(cfg, map[string]config.AgentConfig{"scanner": {Backend: "bob", Enabled: true, OnDemand: true}}, testLogger())
				g.Evaluate(queueDepthIdle, 0, 0, 0)
				return g
			},
			want: ResumeRefusalOnDemand,
		},
		{
			name: "budget exhausted", agent: "scanner",
			setup: func(t *testing.T) *Governor {
				g := resumeTestGovernor(t, "1h")
				g.SetBudgetLimit(weeklyLimit)
				g.UpdateBudgetFromTotals(0, nil, nil)
				g.UpdateBudgetFromTotals(overspend, nil, nil)
				return g
			},
			want: ResumeRefusalBudgetExhausted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := tc.setup(t)
			allowed, reason := g.AllowResumeKickReason(tc.agent)
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q", reason, tc.want)
			}
			if allowed != (tc.want == ResumeAllowed) {
				t.Fatalf("allowed = %v with reason %q", allowed, reason)
			}
			g.mu.RLock()
			recorded := len(g.resumeKicks)
			g.mu.RUnlock()
			if wantRecorded := map[bool]int{true: 1, false: 0}[allowed]; recorded != wantRecorded {
				t.Fatalf("resumeKicks has %d entries, want %d (record only on allow)", recorded, wantRecorded)
			}
		})
	}
}

// TestAllowResumeKickReason_ThrottleDoesNotMoveGrant: the second restart in
// one interval reports interval_throttle and leaves the original grant time in
// place, so the window still ends one interval after the FIRST resume kick.
func TestAllowResumeKickReason_ThrottleDoesNotMoveGrant(t *testing.T) {
	base := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	g := resumeTestGovernor(t, "6h")
	g.now = func() time.Time { return base }
	if ok, reason := g.AllowResumeKickReason("scanner"); !ok || reason != ResumeAllowed {
		t.Fatalf("first resume kick = (%v, %q), want allowed", ok, reason)
	}
	g.now = func() time.Time { return base.Add(time.Hour) }
	if ok, reason := g.AllowResumeKickReason("scanner"); ok || reason != ResumeRefusalIntervalThrottle {
		t.Fatalf("second resume kick = (%v, %q), want interval_throttle", ok, reason)
	}
	if got := g.resumeKicks["scanner"]; !got.Equal(base) {
		t.Fatalf("grant moved to %v on refusal, want %v", got, base)
	}
	if g.AllowResumeKick("scanner") {
		t.Fatal("bool wrapper must agree with the reason verdict")
	}
}

// TestResumeKickVerdict_ReadOnly: the reconcile view reports the verdict and
// interval but never records a grant, so polling it every cycle cannot spend
// an agent's resume allowance.
func TestResumeKickVerdict_ReadOnly(t *testing.T) {
	g := resumeTestGovernor(t, "6h")
	reason, interval := g.ResumeKickVerdict("scanner")
	if reason != ResumeAllowed || interval != 6*time.Hour {
		t.Fatalf("verdict = (%q, %v), want (allowed, 6h)", reason, interval)
	}
	if len(g.resumeKicks) != 0 {
		t.Fatalf("ResumeKickVerdict recorded a grant: %v", g.resumeKicks)
	}
	if !g.AllowResumeKick("scanner") {
		t.Fatal("allowance must still be available after a verdict query")
	}
	if reason, interval = g.ResumeKickVerdict("scanner"); reason != ResumeRefusalIntervalThrottle || interval != 6*time.Hour {
		t.Fatalf("verdict after grant = (%q, %v), want (interval_throttle, 6h)", reason, interval)
	}
	if reason, interval = g.ResumeKickVerdict("no-such-agent"); reason != ResumeRefusalUnscheduled || interval != 0 {
		t.Fatalf("unknown agent verdict = (%q, %v), want (unscheduled, 0)", reason, interval)
	}
}
