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
