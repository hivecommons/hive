package hub

import "testing"

// --- #12844: zero-token token-burn must only fire when spend is expected ---

func TestEvaluateAlerts_ZeroTokenBurnGates(t *testing.T) {
	tests := []struct {
		name    string
		subject func(h *alertHive)
		want    bool
	}{
		{
			name:    "unexplained zero spend still alerts",
			subject: func(h *alertHive) {},
			want:    true,
		},
		{
			name:    "entry already explains the zero spend",
			subject: func(h *alertHive) { h.ZeroTokenSpendExplained = true },
			want:    false,
		},
		{
			name:    "inference auth failure is the stronger alert",
			subject: func(h *alertHive) { h.InferenceAuthError = "401 from gateway" },
			want:    false,
		},
		{
			name:    "GitHub App required is the stronger blocker",
			subject: func(h *alertHive) { h.GitHubAppRequired = true },
			want:    false,
		},
		{
			name: "agents-inactive is the stronger alert",
			subject: func(h *alertHive) {
				h.InactiveAgents = 2
				h.InactiveAgentsReason = "2 agents stuck at login"
			},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := baseHive("subject")
			h.TotalTokens24h = 0
			tc.subject(&h)
			got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
			if hasAlert(got.Alerts, "subject", AlertTypeTokenBurn) != tc.want {
				t.Fatalf("token-burn alert = %v, want %v (alerts: %+v)", !tc.want, tc.want, got.Alerts)
			}
		})
	}
}

func TestZeroTokenSpendExplained(t *testing.T) {
	// expecting is a claimed hive with queued work and unpaused expected-active
	// agents: the one shape where zero spend is genuinely unexplained.
	expecting := func() MyHiveEntry {
		e := MyHiveEntry{}
		e.ID = "h1"
		e.Online = true
		e.ActionableIssues = 3
		e.FleetRollup = &agentFleetRollup{Expected: 2}
		e.AdvisoryIssueActivity = AdvisoryIssueActivity{Bucket: advisoryIssueBucketStale}
		return e
	}
	tests := []struct {
		name   string
		mutate func(e *MyHiveEntry)
		want   bool
	}{
		{name: "queued work and expected-active agents", mutate: func(e *MyHiveEntry) {}, want: false},
		{
			name: "expected-active agents with no queue still expects spend",
			mutate: func(e *MyHiveEntry) {
				e.ActionableIssues = 0
			},
			want: false,
		},
		{
			name: "all expected agents paused with work queued",
			mutate: func(e *MyHiveEntry) {
				e.FleetRollup = &agentFleetRollup{Expected: 6, Paused: 6}
			},
			want: true,
		},
		{
			name:   "every agent quiet by design",
			mutate: func(e *MyHiveEntry) { e.AllAgentsQuiet = true },
			want:   true,
		},
		{
			name: "no queued work and no expected-active agents",
			mutate: func(e *MyHiveEntry) {
				e.ActionableIssues = 0
				e.FleetRollup = &agentFleetRollup{}
			},
			want: true,
		},
		{
			name: "no queued work and no rollup reported",
			mutate: func(e *MyHiveEntry) {
				e.ActionableIssues = 0
				e.FleetRollup = nil
			},
			want: true,
		},
		{
			name: "fresh advisory output",
			mutate: func(e *MyHiveEntry) {
				e.AdvisoryIssueActivity.Bucket = advisoryIssueBucketFresh
			},
			want: true,
		},
		{
			name:   "stale or minimal heartbeat stats",
			mutate: func(e *MyHiveEntry) { e.StatsStale = true },
			want:   true,
		},
		{
			name:   "GitHub App install pending",
			mutate: func(e *MyHiveEntry) { e.PendingGitHubAppInstall = true },
			want:   true,
		},
		{
			name: "all-paused health verdict",
			mutate: func(e *MyHiveEntry) {
				e.HealthVerdict = &HealthVerdict{State: HealthStateAmber, cause: causeAllPaused}
			},
			want: true,
		},
		{
			name: "precondition red verdict (login stuck)",
			mutate: func(e *MyHiveEntry) {
				e.HealthVerdict = &HealthVerdict{State: HealthStateRed, cause: causeLoginStuck}
			},
			want: true,
		},
		{
			name: "generic no-output red does not explain zero spend",
			mutate: func(e *MyHiveEntry) {
				e.HealthVerdict = &HealthVerdict{State: HealthStateRed, staleOutput: true}
			},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := expecting()
			tc.mutate(&e)
			if got := zeroTokenSpendExplained(e); got != tc.want {
				t.Fatalf("zeroTokenSpendExplained = %v, want %v", got, tc.want)
			}
			if got := alertHiveFromEntry(e).ZeroTokenSpendExplained; got != tc.want {
				t.Fatalf("projected ZeroTokenSpendExplained = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- #12843: available pool slots must not alert on project_org ---

func TestEvaluateAlerts_PlaceholderProjectOrgCheck(t *testing.T) {
	tests := []struct {
		name        string
		placeholder bool
		provStatus  string
		checks      []any
		want        bool
		wantReason  string
	}{
		{
			name:        "available slot with only project_org failing",
			placeholder: true,
			provStatus:  statusAvailable,
			checks:      []any{map[string]any{"name": "project_org", "status": "fail"}},
			want:        false,
		},
		{
			name:        "available slot still alerts on a non-exempt check",
			placeholder: true,
			provStatus:  statusAvailable,
			checks: []any{
				map[string]any{"name": "project_org", "status": "fail"},
				map[string]any{"name": "queue", "status": "fail"},
			},
			want:       true,
			wantReason: "1 health check failing: queue",
		},
		{
			name:        "org-prefix placeholder not marked available keeps project_org",
			placeholder: true,
			provStatus:  "",
			checks:      []any{map[string]any{"name": "project_org", "status": "fail"}},
			want:        true,
			wantReason:  "1 health check failing: project_org",
		},
		{
			name:       "claimed hive keeps project_org",
			provStatus: statusAssigned,
			checks:     []any{map[string]any{"name": "project_org", "status": "fail"}},
			want:       true,
			wantReason: "1 health check failing: project_org",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := baseHive("h1")
			h.IsPlaceholder = tc.placeholder
			h.ProvStatus = tc.provStatus
			h.Health = map[string]any{"checks": tc.checks}
			got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
			a, ok := findAlert(got.Alerts, "h1", AlertTypeHealthCheckFailing)
			if ok != tc.want {
				t.Fatalf("health-check-failing alert = %v, want %v (alerts: %+v)", ok, tc.want, got.Alerts)
			}
			if ok && a.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", a.Reason, tc.wantReason)
			}
		})
	}
}

// --- #12842: advisory-stale must not judge cached/minimal heartbeat stats ---

func TestEvaluateAlerts_AdvisoryStaleSkipsStaleHeartbeatStats(t *testing.T) {
	tests := []struct {
		name       string
		statsStale bool
		want       bool
	}{
		{name: "fresh stats keep the alert", statsStale: false, want: true},
		{name: "cached or minimal stats suppress it", statsStale: true, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := baseHive("h1")
			h.AdvisoryStale = true
			h.AdvisoryStaleReason = "advisory digest has not updated since 2026-10-16T20:11:19Z"
			h.StatsStale = tc.statsStale
			got := evaluateAlerts(newAlertState(), []alertHive{h}, nil, fixedNow)
			if hasAlert(got.Alerts, "h1", AlertTypeAdvisoryStale) != tc.want {
				t.Fatalf("advisory-stale alert = %v, want %v (alerts: %+v)", !tc.want, tc.want, got.Alerts)
			}
		})
	}
}

func TestAlertHiveFromEntry_CarriesStatsStale(t *testing.T) {
	e := MyHiveEntry{}
	e.ID = "h1"
	e.StatsStale = true
	if !alertHiveFromEntry(e).StatsStale {
		t.Fatal("StatsStale must be projected so the advisory-stale rule can see it")
	}
}
