package hub

import (
	"strings"
	"testing"
	"time"
)

// The reason chip is operator-facing: each band must say WHY in the exact
// phrasing the dashboard renders, and ages must humanize without duplicated
// "ago" suffixes (regression: advisoryAge used to append " ago" on top of
// humanizeAge's own).
func TestHiveHealthForReasons(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }

	base := func(level int) RegistryEntry {
		return RegistryEntry{Online: true, ACMMLevel: level}
	}
	// writer gives the entry an on-duty agent holding every write grant, so
	// the #4561 no-writers-on-duty gate doesn't short-circuit banding. #4564
	// extended that gate to L2 (any on-duty agent feeds the advisory stream)
	// and renamed the L3–L5 verb create→write; the L2 fixtures and the two
	// verb expectations below track that (semantic conflict between the
	// admin-merged #4562 test and #4564, which broke v4 HEAD).
	writer := func(e RegistryEntry) RegistryEntry {
		e.Agents = []AgentSummary{{ExpectedActive: true, CanOpenIssue: true, CanOpenPR: true, CanMerge: true}}
		return e
	}

	tests := []struct {
		name       string
		entry      RegistryEntry
		rollup     agentFleetRollup
		app        GitHubAppHealth
		queued     int
		wantState  string
		wantReason string
	}{
		{
			name: "L2 advisory fresh — reason carries a single humanized age",
			entry: func() RegistryEntry {
				e := writer(base(2))
				e.AdvisoryLastPostedAt = rfc(now.Add(-2 * time.Hour))
				return e
			}(),
			rollup: okRollup(), app: okApp(), queued: 3,
			wantState:  HealthStateGreen,
			wantReason: "advisory 2h ago",
		},
		{
			name: "L2 advisory stale — red",
			entry: func() RegistryEntry {
				e := writer(base(2))
				e.AdvisoryLastPostedAt = rfc(now.Add(-100 * time.Hour))
				return e
			}(),
			rollup: okRollup(), app: okApp(), queued: 3,
			wantState:  HealthStateRed,
			wantReason: "advisory stale",
		},
		{
			name:   "L2 no advisory yet, queue empty — idle green",
			entry:  writer(base(2)),
			rollup: okRollup(), app: okApp(), queued: 0,
			wantState:  HealthStateGreen,
			wantReason: "queue empty — idle",
		},
		{
			name:   "L2 no advisory yet, work queued — unknown",
			entry:  writer(base(2)),
			rollup: okRollup(), app: okApp(), queued: 4,
			wantState:  HealthStateUnknown,
			wantReason: "no advisory yet",
		},
		{
			name:   "L4 agents blocked — red precondition names the count",
			entry:  base(4),
			rollup: agentFleetRollup{Expected: 2, Running: 2, Known: 2, Problems: 2},
			app:    okApp(), queued: 3,
			wantState:  HealthStateRed,
			wantReason: "2 agent(s) blocked",
		},
		{
			name: "L3 provider spending limit outranks idle agents",
			entry: func() RegistryEntry {
				e := base(3)
				e.ProviderLimitReason = "provider spending limit reached — 682 refused calls: litellm refused the request on a spending limit (429)"
				e.ProviderLimitRebuffs = 682
				return e
			}(),
			rollup: agentFleetRollup{Expected: 3, Running: 0, Known: 3, Problems: 3, IdleWithWork: 3},
			app:    okApp(), queued: 43,
			wantState:  HealthStateRed,
			wantReason: "provider spending limit reached — 682 refused calls: litellm refused the request on a spending limit (429)",
		},
		{
			name: "L3 provider limit count is named when reason lacks refused-call count",
			entry: func() RegistryEntry {
				e := base(3)
				e.ProviderLimitReason = "litellm refused the request on a spending limit (429)"
				e.ProviderLimitRebuffs = 682
				return e
			}(),
			rollup: agentFleetRollup{Expected: 3, Running: 0, Known: 3, Problems: 3, IdleWithWork: 3},
			app:    okApp(), queued: 43,
			wantState:  HealthStateRed,
			wantReason: "provider spending limit reached — 682 refused calls",
		},
		{
			name:   "L3 all problems quota-exhausted — red names provider quota",
			entry:  base(3),
			rollup: agentFleetRollup{Expected: 1, Running: 0, Known: 1, Problems: 1, QuotaExhausted: 1, IdleWithWork: 1},
			app:    okApp(), queued: 8,
			wantState:  HealthStateRed,
			wantReason: "1 agent(s) out of provider quota",
		},
		{
			// EPM/alchemy live case: every problem is a wedged interactive
			// login, so the reason names the one actionable cause.
			name:   "L3 all problems login-stuck — red names the re-login",
			entry:  base(3),
			rollup: agentFleetRollup{Expected: 0, Running: 1, Known: 4, Problems: 3, LoginStuck: 3},
			app:    okApp(), queued: 43,
			wantState:  HealthStateRed,
			wantReason: "3 agent(s) stuck at login — re-login needed",
		},
		{
			// Mixed causes keep the generic count: naming only the login half
			// would hide the rest.
			name:   "L3 mixed blocked causes — generic blocked count",
			entry:  base(3),
			rollup: agentFleetRollup{Expected: 2, Running: 1, Known: 4, Problems: 3, LoginStuck: 1},
			app:    okApp(), queued: 5,
			wantState:  HealthStateRed,
			wantReason: "3 agent(s) blocked",
		},
		{
			// Live sessions sitting past the idle threshold with a backlog:
			// "no agents running" was factually wrong for this shape and sent
			// operators down the restart path instead of the kick path.
			name:   "L3 all problems idle-with-work — red names the idleness",
			entry:  base(3),
			rollup: agentFleetRollup{Expected: 3, Running: 0, Known: 5, Problems: 3, IdleWithWork: 3},
			app:    okApp(), queued: 8,
			wantState:  HealthStateRed,
			wantReason: "3 agent(s) idle with work queued",
		},
		{
			// Every problem is a dead/vanished session and nothing is alive:
			// still name the restart remedy instead of falling through to the
			// generic full-outage wording.
			name:   "L3 all problems dead — names restart cause",
			entry:  base(3),
			rollup: agentFleetRollup{Expected: 4, Running: 0, Known: 4, Problems: 4, DeadOrGone: 4},
			app:    okApp(), queued: 3,
			wantState:  HealthStateRed,
			wantReason: "4 agent(s) down — restart needed",
		},
		{
			// Katamari/ibm-aiops-orchestrator live shape: guide+supervisor are
			// down, but quality+scanner still run. The hive is not fully out, so
			// preserve the real cause instead of falling through to the generic
			// "2 agent(s) blocked".
			name:   "L3 all problems dead while other agents run — names restart cause",
			entry:  base(3),
			rollup: agentFleetRollup{Expected: 4, Running: 2, Known: 4, Problems: 2, DeadOrGone: 2},
			app:    okApp(), queued: 3,
			wantState:  HealthStateRed,
			wantReason: "2 agent(s) down — restart needed",
		},
		{
			// Live all-paused hives (aslom/hive-agent, castrojo/endusers,
			// hashicorp/dev-portal, inference-sim/sim2real, singhar/go-ci,
			// torch-spyre/spyre-inference, TradingAsBuddies/falcon-core,
			// zacburns/mlz-manager, llm-d/llm-d-workload-variant-autoscaler):
			// ExpectedActive remains true while the operator deliberately pauses
			// every agent. That is not "no agents running" red; queued work will
			// wait until a resume, so show an amber action chip.
			name:   "L3 all expected agents paused — amber resume reason",
			entry:  base(3),
			rollup: agentFleetRollup{Expected: 3, Running: 0, Known: 3, Paused: 3},
			app:    okApp(), queued: 11,
			wantState:  HealthStateAmber,
			wantReason: "all agents paused — resume to produce output",
		},
		{
			name:      "L4 recent create — reason humanizes the age",
			entry:     withActivity(writer(base(4)), ractivity("o/r", rfc(now.Add(-3*time.Hour)), "", "", "")),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    3,
			wantState: HealthStateGreen, wantReason: "last write 3h ago",
		},
		{
			name:      "L4 stale create with queue — red names age and backlog",
			entry:     withActivity(writer(base(4)), ractivity("o/r", rfc(now.Add(-49*time.Hour)), "", "", "")),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    7,
			wantState: HealthStateRed, wantReason: "no write in 2d (7 queued)",
		},
		{
			name:      "L4 stale create but queue empty — idle with last output kept",
			entry:     withActivity(writer(base(4)), ractivity("o/r", rfc(now.Add(-49*time.Hour)), "", "", "")),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    0,
			wantState: HealthStateGreen, wantReason: "queue empty — idle",
		},
		{
			name:      "L6 no output ever with queue — red no-output phrasing",
			entry:     withActivity(writer(base(6)), ractivity("o/r", "", "", "", "")),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    5,
			wantState: HealthStateRed, wantReason: "no merge output (5 queued)",
		},
		{
			name:      "L6 no output ever, queue empty — green no-output-yet phrasing",
			entry:     withActivity(writer(base(6)), ractivity("o/r", "", "", "", "")),
			rollup:    okRollup(),
			app:       okApp(),
			queued:    0,
			wantState: HealthStateGreen, wantReason: "queue empty — no output yet",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := hiveHealthFor(tc.entry, tc.rollup, tc.app, tc.queued, now)
			if v.State != tc.wantState {
				t.Errorf("state = %q, want %q (reason %q)", v.State, tc.wantState, v.Reason)
			}
			if v.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", v.Reason, tc.wantReason)
			}
			if strings.Contains(v.Reason, "ago ago") {
				t.Errorf("reason %q has a duplicated ago suffix", v.Reason)
			}
		})
	}
}

func TestHiveHealth_L2FreshAdvisoryIgnoresWriteIncapableIdleAgents(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	agent := func(name, state string, paused bool) AgentSummary {
		return AgentSummary{
			Name: name, State: state, Backend: "bob",
			Enabled: true, ExpectedActive: !paused, Paused: paused,
			StartedAt: settled(now), LastActivityAt: activeAt(now, 3*time.Hour),
		}
	}
	e := RegistryEntry{
		Online:               true,
		ACMMLevel:            2,
		AdvisoryLastPostedAt: rfc(now.Add(-4 * time.Minute)),
		Agents: []AgentSummary{
			agent("quality", agentStateRunning, false),
			agent("guide", agentStateRunning, false),
			agent("scanner", agentStatePaused, true),
			agent("supervisor", agentStatePaused, true),
			agent("brainstorm", agentStatePaused, true),
		},
	}

	rollup := rollupAgents(e.Agents, hiveBlockers{}, 9, now)
	if rollup.Problems != 0 || rollup.IdleWithWork != 0 {
		t.Fatalf("write-incapable L2 agents must not create idle-with-work problems, got %+v", rollup)
	}

	v := hiveHealthFor(e, rollup, okApp(), 9, now)
	if v.State != HealthStateGreen || !strings.Contains(v.Reason, "advisory 4m ago") {
		t.Fatalf("fresh L2 advisory should stay healthy, got state=%s reason=%q", v.State, v.Reason)
	}
}

func TestHiveHealth_LiveFleetCauseShapes(t *testing.T) {
	now := time.Date(2026, 8, 26, 15, 28, 0, 0, time.UTC)
	base := RegistryEntry{Online: true, ACMMLevel: 3}
	working := func(name string) AgentSummary {
		a := modernWorking(now)
		a.Name = name
		return a
	}
	paused := func(name string) AgentSummary {
		a := working(name)
		a.State = agentStatePaused
		a.Paused = true
		return a
	}
	dead := func(name string) AgentSummary {
		a := working(name)
		a.State = "failed"
		return a
	}
	loginInGrace := func(name string) AgentSummary {
		a := working(name)
		a.Backend = "copilot"
		a.NeedsLogin = true
		a.StartedAt = now.Add(-(inactiveAgentStartupGrace + 2*time.Minute)).UTC().Format(time.RFC3339)
		return a
	}

	tests := []struct {
		name       string
		agents     []AgentSummary
		wantState  string
		wantReason string
	}{
		{
			// The all-paused hives seen live (for example aslom/hive-agent and
			// castrojo/endusers) have queued work and ExpectedActive agents, but
			// every expected agent is deliberately paused by the operator.
			name:       "all agents paused",
			agents:     []AgentSummary{paused("quality"), paused("scanner"), paused("supervisor")},
			wantState:  HealthStateAmber,
			wantReason: "all agents paused — resume to produce output",
		},
		{
			// The placeholder/available-akswec2 pool shape is inside the 20m
			// login grace, so RunState is still working; the ABLE leg already
			// knows the cause is a login prompt and the hive chip must say that.
			name:       "login prompt inside grace",
			agents:     []AgentSummary{loginInGrace("guide"), loginInGrace("quality"), loginInGrace("scanner"), loginInGrace("supervisor")},
			wantState:  HealthStateRed,
			wantReason: "4 agent(s) stuck at login — re-login needed",
		},
		{
			// Katamari/ibm-aiops-orchestrator had guide+supervisor down while
			// quality+scanner still ran; this is a partial outage, not a generic
			// blocked count.
			name:       "dead agents while others run",
			agents:     []AgentSummary{dead("guide"), dead("supervisor"), working("quality"), working("scanner")},
			wantState:  HealthStateRed,
			wantReason: "2 agent(s) down — restart needed",
		},
		{
			// Katamari/ibm-aiops-orchestrator can also have all four expected bob
			// agents failed. That should keep the actionable restart cause instead
			// of saying only that no agents are running.
			name:       "all expected agents dead",
			agents:     []AgentSummary{dead("guide"), dead("quality"), dead("scanner"), dead("supervisor")},
			wantState:  HealthStateRed,
			wantReason: "4 agent(s) down — restart needed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			e.Agents = tc.agents
			rollup := rollupAgents(tc.agents, hiveBlockers{}, 9, now)
			v := hiveHealthFor(e, rollup, okApp(), 9, now)
			if v.State != tc.wantState || v.Reason != tc.wantReason {
				t.Fatalf("verdict = (%s, %q), want (%s, %q); rollup=%+v",
					v.State, v.Reason, tc.wantState, tc.wantReason, rollup)
			}
		})
	}
}

func TestHiveHealth_LiveFleetDetectorCoverageFromRawSignals(t *testing.T) {
	now := time.Date(2026, 9, 3, 3, 30, 0, 0, time.UTC)
	base := RegistryEntry{Online: true, ACMMLevel: 3}
	agent := func(name string) AgentSummary {
		a := modernWorking(now)
		a.Name = name
		a.CanMerge = false
		return a
	}

	tests := []struct {
		name         string
		entry        RegistryEntry
		agents       []AgentSummary
		blockers     hiveBlockers
		wantProblems int
		wantReason   string
	}{
		{
			name: "provider-side spend limit reported by spoke",
			entry: func() RegistryEntry {
				e := base
				e.ProviderLimitReason = "litellm refused the request on a spending limit (429)"
				e.ProviderLimitRebuffs = 682
				return e
			}(),
			agents: []AgentSummary{agent("quality"), agent("scanner")},
			blockers: hiveBlockers{
				ProviderLimitReason: "litellm refused the request on a spending limit (429)",
			},
			wantProblems: 2,
			wantReason:   "provider spending limit reached — 682 refused calls",
		},
		{
			name:  "agent-level provider quota from heartbeat",
			entry: base,
			agents: func() []AgentSummary {
				a := agent("guide")
				a.QuotaExhausted = true
				return []AgentSummary{a}
			}(),
			wantProblems: 1,
			wantReason:   "1 agent(s) out of provider quota",
		},
		{
			name:  "missed cadence with queued work",
			entry: base,
			agents: func() []AgentSummary {
				a := agent("quality")
				a.KickIntervalSec = int64(30 * time.Minute / time.Second)
				a.LastActivityAt = activeAt(now, 2*time.Hour)
				return []AgentSummary{a}
			}(),
			wantProblems: 1,
			wantReason:   "1 agent(s) idle with work queued",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.entry
			e.Agents = tc.agents
			rollup := rollupAgents(tc.agents, tc.blockers, 9, now)
			if rollup.Problems != tc.wantProblems {
				t.Fatalf("problems = %d, want %d; rollup=%+v", rollup.Problems, tc.wantProblems, rollup)
			}
			v := hiveHealthFor(e, rollup, okApp(), 9, now)
			if v.State != HealthStateRed || v.Reason != tc.wantReason {
				t.Fatalf("verdict = (%s, %q), want (red, %q); rollup=%+v",
					v.State, v.Reason, tc.wantReason, rollup)
			}
		})
	}
}

func TestMaxRFC3339(t *testing.T) {
	early, late := "2026-08-22T01:00:00Z", "2026-08-22T09:00:00Z"
	cases := []struct{ a, b, want string }{
		{"", "", ""},
		{"", late, late},
		{early, "", early},
		{early, late, late},
		{late, early, late},
		{late, late, late},
	}
	for _, c := range cases {
		if got := maxRFC3339(c.a, c.b); got != c.want {
			t.Errorf("maxRFC3339(%q, %q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestAdvisoryAge(t *testing.T) {
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	adv := AdvisoryIssueActivity{LastActivityAt: "2026-08-22T09:00:00Z"}
	if got := advisoryAge(adv, now); got != "3h ago" {
		t.Errorf("advisoryAge parseable = %q, want \"3h ago\"", got)
	}
	if got := advisoryAge(AdvisoryIssueActivity{LastActivityAt: "garbage"}, now); got != "fresh" {
		t.Errorf("advisoryAge unparseable = %q, want \"fresh\"", got)
	}
}

func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "just now"}, // clock skew reads as just-now
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5m ago"},
		{3 * time.Hour, "3h ago"},
		{49 * time.Hour, "2d ago"},
	}
	for _, c := range cases {
		if got := humanizeAge(c.d); got != c.want {
			t.Errorf("humanizeAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
