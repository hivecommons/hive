package hub

import (
	"strings"
	"testing"
	"time"
)

// The #5577 signature→action table, one case per row, plus the precedence
// rules the RFC fixes: budget-exhausted beats the generic no-output red,
// App-broken beats everything, and a green verdict never carries a hint.

// remEntry builds an online entry at the given level with one on-duty,
// fully-granted agent so the freshness bands are exercised.
func remEntry(level int) RegistryEntry {
	return RegistryEntry{Online: true, ACMMLevel: level, DashboardURL: "https://spoke.example.com/", Agents: []AgentSummary{
		{Name: "quality", State: agentStateRunning, Enabled: true, ExpectedActive: true,
			CanOpenIssue: true, CanOpenPR: true, CanMerge: true},
	}}
}

func TestRemediationPerSignature(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	rfc := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }
	recent := rfc(now.Add(-1 * time.Hour))
	oldTs := rfc(now.Add(-30 * time.Hour))
	stale := func(e RegistryEntry) RegistryEntry { return withActivity(e, ractivity("o/r", oldTs, oldTs, "", "")) }
	fresh := func(e RegistryEntry) RegistryEntry { return withActivity(e, ractivity("o/r", recent, recent, "", "")) }

	tests := []struct {
		name       string
		entry      RegistryEntry
		rollup     agentFleetRollup
		app        GitHubAppHealth
		queued     int
		wantState  string
		wantReason string // substring, "" = don't check
		wantAction string // substring of Remediation.Action; "NONE" = no hint
		wantLink   string // exact, "" = don't check
	}{
		{
			name:       "App broken → install/repair hint",
			entry:      stale(remEntry(4)),
			rollup:     okRollup(),
			app:        GitHubAppHealth{Bucket: ghAppBucketBroken},
			queued:     5,
			wantState:  HealthStateRed,
			wantReason: "GitHub App",
			wantAction: "Install or repair the GitHub App",
		},
		{
			name:  "agents stuck at login → device-flow login hint with /login link",
			entry: stale(remEntry(4)),
			rollup: agentFleetRollup{Expected: 2, Running: 2, Known: 2,
				Problems: 2, LoginStuck: 2},
			app:        okApp(),
			queued:     5,
			wantState:  HealthStateRed,
			wantReason: "stuck at login",
			wantAction: "device-flow login",
			wantLink:   "https://spoke.example.com/login",
		},
		{
			name: "budget exhausted → own reason with spend X of Y + budget hint",
			entry: func() RegistryEntry {
				e := stale(remEntry(4))
				e.BudgetExhausted = boolptr(true)
				e.BudgetCurrentSpend = int64ptr(300_000_000)
				e.BudgetLimit = int64ptr(100_000_000)
				return e
			}(),
			rollup:     okRollup(),
			app:        okApp(),
			queued:     9,
			wantState:  HealthStateRed,
			wantReason: "budget exhausted — spend 300000000 of 100000000, kicks suppressed",
			wantAction: "Raise or reset the budget limit",
		},
		{
			name: "budget limit below floor → misconfigured reason, same budget hint",
			entry: func() RegistryEntry {
				e := stale(remEntry(4))
				e.BudgetExhausted = boolptr(true)
				e.BudgetLimit = int64ptr(50)
				return e
			}(),
			rollup:     okRollup(),
			app:        okApp(),
			queued:     9,
			wantState:  HealthStateRed,
			wantReason: "misconfigured",
			wantAction: "Raise or reset the budget limit",
		},
		{
			name: "error streak → red names the agent and count, pin-a-model hint",
			entry: func() RegistryEntry {
				e := stale(remEntry(4))
				e.AgentErrorStreaks = map[string]int{"quality": 17, "scanner": 1}
				return e
			}(),
			rollup:     okRollup(),
			app:        okApp(),
			queued:     28,
			wantState:  HealthStateRed,
			wantReason: "agent quality model calls failing (17 consecutive) — pin a working model",
			wantAction: "Pin a working model",
		},
		{
			name: "error streak below threshold → generic red, no hint",
			entry: func() RegistryEntry {
				e := stale(remEntry(4))
				e.AgentErrorStreaks = map[string]int{"quality": 2}
				return e
			}(),
			rollup:     okRollup(),
			app:        okApp(),
			queued:     28,
			wantState:  HealthStateRed,
			wantReason: "no write",
			wantAction: "NONE",
		},
		{
			name: "consent wedge → amber demotes green, consent hint",
			entry: func() RegistryEntry {
				e := fresh(remEntry(4))
				e.ConsentWedged = []string{"copilot-fixer"}
				return e
			}(),
			rollup:     okRollup(),
			app:        okApp(),
			queued:     4,
			wantState:  HealthStateAmber,
			wantReason: "copilot-fixer stuck at Copilot consent",
			wantAction: "Complete the Copilot consent flow",
		},
		{
			name: "no cadences → amber names the agents, set-cadences hint",
			entry: func() RegistryEntry {
				e := fresh(remEntry(4))
				e.NoCadenceAgents = []string{"telemetry", "docs"}
				return e
			}(),
			rollup:     okRollup(),
			app:        okApp(),
			queued:     4,
			wantState:  HealthStateAmber,
			wantReason: "agent(s) telemetry, docs enabled but never kicked — set cadences",
			wantAction: "Set cadences on the agent card",
		},
		{
			name: "consent wedge outranks no-cadence when both fire",
			entry: func() RegistryEntry {
				e := fresh(remEntry(4))
				e.ConsentWedged = []string{"copilot-fixer"}
				e.NoCadenceAgents = []string{"telemetry"}
				return e
			}(),
			rollup:     okRollup(),
			app:        okApp(),
			queued:     4,
			wantState:  HealthStateAmber,
			wantReason: "Copilot consent",
			wantAction: "Complete the Copilot consent flow",
		},
		{
			name: "hold-stale amber keeps #5350's reason and gains the queue link",
			entry: func() RegistryEntry {
				e := stale(remEntry(4))
				e.Org = "flashsystems"
				e.Repos = []string{"ess"}
				held := 14
				e.HoldTotal = &held
				return e
			}(),
			rollup:     okRollup(),
			app:        okApp(),
			queued:     28,
			wantState:  HealthStateAmber,
			wantReason: "awaiting human review — 14 held for approval",
			wantAction: "Review the needs-human queue",
			wantLink:   "https://github.com/flashsystems/ess/pulls",
		},
		{
			// PRECEDENCE: budget-exhausted beats the generic no-output red AND
			// the error-streak re-explanation — agents are halted upstream of
			// any model call, so the streak is a symptom, not the cause.
			name: "precedence: budget exhausted beats error streak and no-output",
			entry: func() RegistryEntry {
				e := stale(remEntry(4))
				e.BudgetExhausted = boolptr(true)
				e.BudgetCurrentSpend = int64ptr(300)
				e.BudgetLimit = int64ptr(100_000_000)
				e.AgentErrorStreaks = map[string]int{"quality": 50}
				return e
			}(),
			rollup:     okRollup(),
			app:        okApp(),
			queued:     9,
			wantState:  HealthStateRed,
			wantReason: "budget exhausted",
			wantAction: "Raise or reset the budget limit",
		},
		{
			// PRECEDENCE: App-broken beats everything.
			name: "precedence: App broken beats budget, streaks and wedges",
			entry: func() RegistryEntry {
				e := stale(remEntry(4))
				e.BudgetExhausted = boolptr(true)
				e.BudgetLimit = int64ptr(100_000_000)
				e.AgentErrorStreaks = map[string]int{"quality": 50}
				e.ConsentWedged = []string{"copilot-fixer"}
				return e
			}(),
			rollup:     okRollup(),
			app:        GitHubAppHealth{Bucket: ghAppBucketBroken},
			queued:     9,
			wantState:  HealthStateRed,
			wantReason: "GitHub App",
			wantAction: "Install or repair the GitHub App",
		},
		{
			name:       "green never carries a hint",
			entry:      fresh(remEntry(4)),
			rollup:     okRollup(),
			app:        okApp(),
			queued:     4,
			wantState:  HealthStateGreen,
			wantAction: "NONE",
		},
		{
			// L1 exemption: the detector ambers must not fault a hive whose
			// level produces no output at all.
			name: "L1 inception stays green even with detector signals",
			entry: func() RegistryEntry {
				e := remEntry(1)
				e.NoCadenceAgents = []string{"telemetry"}
				e.ConsentWedged = []string{"copilot-fixer"}
				return e
			}(),
			rollup:     okRollup(),
			app:        okApp(),
			queued:     0,
			wantState:  HealthStateGreen,
			wantAction: "NONE",
		},
		// --- #5699: the three families the 2026-09-02 fleet sweep found
		// rendering a WHY chip with nothing under it. Each verdict already
		// named its condition; none named a fix, so eleven non-green spokes
		// told an operator what was wrong and not what to do. ---
		{
			name:  "agents down → restart-from-the-card hint",
			entry: stale(remEntry(4)),
			rollup: agentFleetRollup{Expected: 3, Running: 1, Known: 3,
				Problems: 2, DeadOrGone: 2},
			app:        okApp(),
			queued:     5,
			wantState:  HealthStateRed,
			wantReason: "down — restart needed",
			wantAction: "Restart the agent from its card",
			wantLink:   "https://spoke.example.com",
		},
		{
			// Amber, and the hint must offer BOTH resolutions: every agent is
			// paused ON PURPOSE, so "resume them" alone reads as an instruction
			// to undo a deliberate choice.
			name:  "all agents paused → resume-or-mark-idle hint",
			entry: stale(remEntry(4)),
			rollup: agentFleetRollup{Expected: 3, Running: 0, Known: 3,
				Paused: 3},
			app:        okApp(),
			queued:     5,
			wantState:  HealthStateAmber,
			wantReason: "all agents paused",
			wantAction: "Resume agents, or mark the hive idle",
			wantLink:   "https://spoke.example.com",
		},
		{
			name:  "agents out of provider quota → rotate-or-raise hint",
			entry: stale(remEntry(4)),
			rollup: agentFleetRollup{Expected: 2, Running: 2, Known: 2,
				Problems: 2, QuotaExhausted: 2},
			app:        okApp(),
			queued:     5,
			wantState:  HealthStateRed,
			wantReason: "out of provider quota",
			wantAction: "Rotate the agent to a provider with headroom",
		},
		{
			// The SPOKE-level provider limit is a different code path from the
			// agent rollup above — a precondition red that returns before the
			// rollup is consulted — and it was equally hint-less. Same remedy,
			// so it carries the same hint rather than an arbitrarily different
			// one.
			name: "spoke-level provider limit → same rotate-or-raise hint",
			entry: func() RegistryEntry {
				e := stale(remEntry(4))
				e.ProviderLimitReason = "credit balance too low"
				e.ProviderLimitHiveWide = true
				return e
			}(),
			rollup:     okRollup(),
			app:        okApp(),
			queued:     5,
			wantState:  HealthStateRed,
			wantAction: "Rotate the agent to a provider with headroom",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := hiveHealthFor(tc.entry, tc.rollup, tc.app, tc.queued, now)
			if v.State != tc.wantState {
				t.Fatalf("state = %q (reason %q), want %q", v.State, v.Reason, tc.wantState)
			}
			if tc.wantReason != "" && !strings.Contains(v.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want substring %q", v.Reason, tc.wantReason)
			}
			if tc.wantAction == "NONE" {
				if v.Remediation != nil {
					t.Fatalf("unexpected remediation %+v on %q verdict", v.Remediation, v.State)
				}
				return
			}
			if v.Remediation == nil {
				t.Fatalf("no remediation attached (state %q reason %q)", v.State, v.Reason)
			}
			if !strings.Contains(v.Remediation.Action, tc.wantAction) {
				t.Errorf("action = %q, want substring %q", v.Remediation.Action, tc.wantAction)
			}
			if tc.wantLink != "" && v.Remediation.Link != tc.wantLink {
				t.Errorf("link = %q, want %q", v.Remediation.Link, tc.wantLink)
			}
		})
	}
}

// Digest-lag (#5577's channel row): only a GREEN verdict is demoted, only for
// a spoke tracking "stable" that is measurably ≥2 commits behind with no
// upgrade in flight — and it is skipped entirely when the divergence info is
// absent.
func TestApplyChannelLag(t *testing.T) {
	behind := func(n int) *int { return &n }
	green := func() *HealthVerdict { return &HealthVerdict{State: HealthStateGreen, Reason: "last write 1h ago"} }

	v := green()
	applyChannelLag(v, "stable", behind(3), false)
	if v.State != HealthStateAmber || !strings.Contains(v.Reason, "3 commits behind stable") {
		t.Fatalf("lagging spoke not demoted: %+v", v)
	}
	if v.Remediation == nil || !strings.Contains(v.Remediation.Action, "check auto-upgrade") {
		t.Fatalf("channel-lag hint missing: %+v", v.Remediation)
	}

	for name, tc := range map[string]struct {
		channel   string
		behind    *int
		upgrading bool
	}{
		"one commit is a rollout in flight": {"stable", behind(1), false},
		"no divergence info reported":       {"stable", nil, false},
		"not tracking a channel":            {"", behind(5), false},
		"upgrade already running":           {"stable", behind(5), true},
	} {
		v := green()
		applyChannelLag(v, tc.channel, tc.behind, tc.upgrading)
		if v.State != HealthStateGreen || v.Remediation != nil {
			t.Errorf("%s: green verdict touched: %+v", name, v)
		}
	}

	// A red verdict is never re-explained by channel lag.
	red := &HealthVerdict{State: HealthStateRed, Reason: "no write in 30h (7 queued)"}
	applyChannelLag(red, "stable", behind(9), false)
	if red.State != HealthStateRed || red.Remediation != nil {
		t.Errorf("red verdict touched by channel lag: %+v", red)
	}
}
