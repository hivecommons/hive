package hub

import (
	"strings"
	"testing"
	"time"
)

// Direct unit coverage for explainOutputFreshness: the disposition switch
// that downgrades a stale-output red to an explained amber. The table-driven
// TestHiveHealthForReasons reaches this only through full verdict derivation,
// which left every default-reason fallback, the budget-suppressed case, the
// agent-decided-not-writable count fallbacks, and the kick-capable rewrite
// unexercised.
func TestExplainOutputFreshness(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	redStale := HealthVerdict{State: HealthStateRed, Reason: "no writes in 24h", staleOutput: true}

	tests := []struct {
		name        string
		entry       RegistryEntry
		in          HealthVerdict
		queued      int
		wantState   string
		wantStale   bool
		wantReason  string // exact match when set
		wantContain string // substring match when set
	}{
		{
			name:      "non-red verdict passes through untouched",
			entry:     RegistryEntry{LastKickDisposition: "advisory-only"},
			in:        HealthVerdict{State: HealthStateGreen, Reason: "fine"},
			wantState: HealthStateGreen, wantReason: "fine",
		},
		{
			name:      "red but not stale-output passes through untouched",
			entry:     RegistryEntry{LastKickDisposition: "advisory-only"},
			in:        HealthVerdict{State: HealthStateRed, Reason: "app broken"},
			wantState: HealthStateRed, wantReason: "app broken",
		},
		{
			name:      "advisory-only with skip reason",
			entry:     RegistryEntry{LastKickDisposition: "advisory-only", LastKickSkipReason: "band caps writes"},
			in:        redStale,
			wantState: HealthStateAmber, wantReason: "advisory-only — band caps writes",
		},
		{
			name:       "advisory-only default reason",
			entry:      RegistryEntry{LastKickDisposition: "advisory-only"},
			in:         redStale,
			wantState:  HealthStateAmber,
			wantReason: "advisory-only — ACMM advisory band produces advisory output, not writes",
		},
		{
			name:      "idle default reason with idle-since timestamp",
			entry:     RegistryEntry{LastKickDisposition: "idle", LastWriteCapableKickAt: now.Add(-3 * time.Hour)},
			in:        redStale,
			wantState: HealthStateAmber,
			wantReason: "nothing to write — governor idle since " +
				now.Add(-3*time.Hour).UTC().Format(time.RFC3339) +
				" (3h ago) because no write-capable agents due",
		},
		{
			name:       "no-due-agents with explicit reason and no timestamp",
			entry:      RegistryEntry{LastKickDisposition: "no-due-agents", LastKickSkipReason: "all agents off schedule"},
			in:         redStale,
			wantState:  HealthStateAmber,
			wantReason: "nothing to write — governor idle because all agents off schedule",
		},
		{
			name:      "budget-suppressed default reason",
			entry:     RegistryEntry{LastKickDisposition: "budget-suppressed"},
			in:        redStale,
			wantState: HealthStateAmber, wantReason: "nothing written — budget suppressed kicks",
		},
		{
			name:      "budget-suppressed with explicit reason",
			entry:     RegistryEntry{LastKickDisposition: "budget-suppressed", LastKickSkipReason: "daily cap hit"},
			in:        redStale,
			wantState: HealthStateAmber, wantReason: "nothing written — daily cap hit",
		},
		{
			name:      "agent-decided-not-writable uses entry count",
			entry:     RegistryEntry{LastKickDisposition: "agent-decided-not-writable", NotWritableQueued: 4},
			in:        redStale,
			wantState: HealthStateAmber, wantReason: "nothing writable — 4 queued deemed not writable",
		},
		{
			name:      "agent-decided-not-writable falls back to queuedWork when count absent",
			entry:     RegistryEntry{LastKickDisposition: "agent-decided-not-writable"},
			in:        redStale,
			queued:    7,
			wantState: HealthStateAmber, wantReason: "nothing writable — 7 queued deemed not writable",
		},
		{
			name: "agent-decided-not-writable no counts but a skip reason",
			entry: RegistryEntry{
				LastKickDisposition: "agent-decided-not-writable",
				LastKickSkipReason:  "queued items are all epics",
			},
			in:        redStale,
			wantState: HealthStateAmber, wantReason: "nothing writable — queued items are all epics",
		},
		{
			name:      "agent-decided-not-writable no counts no reason",
			entry:     RegistryEntry{LastKickDisposition: "agent-decided-not-writable"},
			in:        redStale,
			wantState: HealthStateAmber, wantReason: "nothing writable — agents declined write",
		},
		{
			name: "kick-capable with recent kick stays red but explains the broken pipeline",
			entry: RegistryEntry{
				LastKickDisposition:    "kick-capable",
				LastWriteCapableKickAt: now.Add(-2 * time.Hour),
			},
			in:          redStale,
			queued:      5,
			wantState:   HealthStateRed,
			wantStale:   true,
			wantContain: "pipeline broken — write-capable kick 2h ago but no writes (5 queued)",
		},
		{
			name: "kick-capable outside the recency window keeps the original reason",
			entry: RegistryEntry{
				LastKickDisposition:    "kick-capable",
				LastWriteCapableKickAt: now.Add(-healthRecencyWindow - time.Hour),
			},
			in:        redStale,
			wantState: HealthStateRed, wantStale: true, wantReason: "no writes in 24h",
		},
		{
			name:      "unknown disposition keeps the original red verdict",
			entry:     RegistryEntry{LastKickDisposition: "something-new"},
			in:        redStale,
			wantState: HealthStateRed, wantStale: true, wantReason: "no writes in 24h",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := explainOutputFreshness(tc.entry, tc.in, tc.queued, now)
			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q", got.State, tc.wantState)
			}
			if got.staleOutput != tc.wantStale {
				t.Errorf("staleOutput = %v, want %v", got.staleOutput, tc.wantStale)
			}
			if tc.wantReason != "" && got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if tc.wantContain != "" && !strings.Contains(got.Reason, tc.wantContain) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, tc.wantContain)
			}
		})
	}
}

func TestOutputIdleSince(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	if got := outputIdleSince(time.Time{}, now); got != "" {
		t.Errorf("zero time: got %q, want empty", got)
	}
	at := now.Add(-90 * time.Minute)
	want := " since " + at.UTC().Format(time.RFC3339) + " (1h ago)"
	if got := outputIdleSince(at, now); got != want {
		t.Errorf("outputIdleSince = %q, want %q", got, want)
	}
}

func TestNameList(t *testing.T) {
	cases := []struct {
		names []string
		want  string
	}{
		{nil, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b", "c"}, "a, b, c"},
		{[]string{"a", "b", "c", "d"}, "a, b, c +1 more"},
		{[]string{"a", "b", "c", "d", "e", "f"}, "a, b, c +3 more"},
	}
	for _, tc := range cases {
		if got := nameList(tc.names); got != tc.want {
			t.Errorf("nameList(%v) = %q, want %q", tc.names, got, tc.want)
		}
	}
}
