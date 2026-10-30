package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// #14856: the spoke dashboard derives the same upgrade state vocabulary the
// hub card uses, from the drain latch, the self-upgrade marker, the landed
// outcome and the hub's policy.
func TestBuildUpgradeProgressStates(t *testing.T) {
	now := time.Date(2026, 10, 26, 17, 10, 0, 0, time.UTC)
	next := now.Add(24 * time.Hour).Format(time.RFC3339)
	managed := &spoke.HeartbeatUpgradePolicy{HubManaged: true, Schedule: "daily", NextWindowAt: next}

	cases := []struct {
		name   string
		in     upgradeProgressInput
		state  string
		detail string
		nilOK  bool
	}{
		{
			name: "drain open",
			in: upgradeProgressInput{Policy: managed, AgentsTotal: 5, RunningSHA: "551ffc34f",
				Drain: &spoke.UpgradeDrainReport{Target: "abc1234", WaitedS: 300, RemainingS: 2400,
					Turns: []spoke.UpgradeTurn{{Agent: "reviewer", ElapsedS: 38 * 60}, {Agent: "scanner", ElapsedS: 12 * 60}}}},
			state: spoke.UpgradeStateDraining, detail: "2/5 agents in turn: reviewer 38m, scanner 12m — proceeds in 40m",
		},
		{
			name: "marker exhausted",
			in: upgradeProgressInput{Policy: managed, Marker: map[string]any{"target": "abc1234", "current": "551ffc3",
				"attempts": 5, "maxAttempts": 5, "failed": true, "lastError": "ErrImagePull"}},
			state: spoke.UpgradeStateMissed, detail: "ErrImagePull",
		},
		{
			name: "marker retrying",
			in: upgradeProgressInput{Policy: managed, Marker: map[string]any{"target": "abc1234", "current": "551ffc3",
				"attempts": 2, "maxAttempts": 5, "failed": false}},
			state: spoke.UpgradeStatePulling, detail: "attempt 2/5",
		},
		{
			name: "outcome landed this boot",
			in: upgradeProgressInput{Policy: managed, StartedAt: now.Add(-10 * time.Minute),
				Outcome: &upgradeOutcome{TargetSHA: "abc1234", CurrentSHA: "551ffc3", RequestedAt: now.Add(-time.Hour), CompletedAt: now.Add(-9 * time.Minute)}},
			state: spoke.UpgradeStateDone, detail: "upgraded to abc1234 9m ago",
		},
		{
			name: "old outcome ignored, legacy hub window shown as delayed",
			in: upgradeProgressInput{StartedAt: now.Add(-10 * time.Minute),
				Outcome: &upgradeOutcome{TargetSHA: "abc1234", CompletedAt: now.Add(-48 * time.Hour)},
				Policy: &spoke.HeartbeatUpgradePolicy{HubManaged: true, NextWindowAt: next, UpgradeHistory: []spoke.UpgradeAttempt{
					{Trigger: spoke.UpgradeTriggerWindow, Outcome: spoke.UpgradeOutcomeMissed, Reason: "spoke still draining",
						StartedAt: now.Add(-3 * time.Hour), EndedAt: now.Add(-3 * time.Hour)}}}},
			state: spoke.UpgradeStateDelayed, detail: "delayed: spoke still draining",
		},
		{
			name: "landed outcome clears stale hub delay before next heartbeat",
			in: upgradeProgressInput{StartedAt: now.Add(-10 * time.Minute),
				Outcome: &upgradeOutcome{TargetSHA: "abc1234", CompletedAt: now.Add(-9 * time.Minute)},
				Policy: &spoke.HeartbeatUpgradePolicy{HubManaged: true, UpgradeHistory: []spoke.UpgradeAttempt{
					{Trigger: spoke.UpgradeTriggerWindow, Outcome: spoke.UpgradeOutcomeMissed, Reason: "merge debounce",
						StartedAt: now.Add(-15 * time.Minute), EndedAt: now.Add(-12 * time.Minute)},
				}}},
			state: spoke.UpgradeStateDone, detail: "upgraded to abc1234",
		},
		{
			name: "new delay is not hidden by this boot's earlier success",
			in: upgradeProgressInput{StartedAt: now.Add(-10 * time.Minute),
				Outcome: &upgradeOutcome{TargetSHA: "abc1234", CompletedAt: now.Add(-9 * time.Minute)},
				Policy: &spoke.HeartbeatUpgradePolicy{HubManaged: true, UpgradeHistory: []spoke.UpgradeAttempt{
					{Trigger: spoke.UpgradeTriggerWindow, Outcome: spoke.UpgradeOutcomeDelayed, Reason: "merge debounce",
						StartedAt: now.Add(-2 * time.Minute), EndedAt: now.Add(-time.Minute)},
				}}},
			state: spoke.UpgradeStateDelayed, detail: "merge debounce",
		},
		{
			name: "genuine hub failure remains missed",
			in: upgradeProgressInput{
				StartedAt: now.Add(-10 * time.Minute),
				Outcome:   &upgradeOutcome{TargetSHA: "abc1234", CompletedAt: now.Add(-9 * time.Minute)},
				Policy: &spoke.HeartbeatUpgradePolicy{HubManaged: true, UpgradeHistory: []spoke.UpgradeAttempt{
					{Trigger: spoke.UpgradeTriggerScheduled, Outcome: spoke.UpgradeOutcomeMissed, Reason: "ErrImagePull", EndedAt: now.Add(-time.Minute)},
				}}},
			state: spoke.UpgradeStateMissed, detail: "ErrImagePull",
		},
		{
			name:  "scheduled with window",
			in:    upgradeProgressInput{Policy: managed},
			state: spoke.UpgradeStateScheduled, detail: "next window " + next,
		},
		{
			name:  "paused",
			in:    upgradeProgressInput{Policy: &spoke.HeartbeatUpgradePolicy{HubManaged: true, Paused: true}},
			state: spoke.UpgradeStateScheduled, detail: "paused",
		},
		{
			name:  "spoke managed instant",
			in:    upgradeProgressInput{Policy: &spoke.HeartbeatUpgradePolicy{SpokeManaged: true}},
			state: spoke.UpgradeStateScheduled, detail: "next poll",
		},
		{name: "unmanaged", in: upgradeProgressInput{}, nilOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Now = now
			p := buildUpgradeProgress(tc.in)
			if tc.nilOK {
				if p != nil {
					t.Fatalf("want nil, got %+v", p)
				}
				return
			}
			if p == nil {
				t.Fatal("nil progress")
			}
			if p.State != tc.state {
				t.Fatalf("state = %q want %q (%+v)", p.State, tc.state, p)
			}
			if !strings.Contains(p.Detail, tc.detail) {
				t.Fatalf("detail %q lacks %q", p.Detail, tc.detail)
			}
			if p.State == spoke.UpgradeStateDraining {
				if p.DrainRemainingS != 2400 || p.DrainDeadline != now.Add(40*time.Minute).Format(time.RFC3339) {
					t.Fatalf("drain countdown wrong: %+v", p)
				}
				if p.NextWindowAt != next {
					t.Fatal("draining must still carry the next window")
				}
			}
		})
	}
}

func TestStatusCarriesUpgradeProgress(t *testing.T) {
	origMarker, origOutcome := upgradeMarkerPath, upgradeOutcomePath
	dir := t.TempDir()
	upgradeMarkerPath = filepath.Join(dir, "upgrade-requested")
	upgradeOutcomePath = filepath.Join(dir, "last-upgrade-outcome")
	t.Cleanup(func() { upgradeMarkerPath, upgradeOutcomePath = origMarker, origOutcome })
	if err := os.WriteFile(upgradeMarkerPath, []byte(`{"target_sha":"abc1234","current_sha":"551ffc3","attempts":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := &Server{status: &StatusPayload{}, startedAt: time.Now()}
	s.SetHubUpgradePolicy(&spoke.HeartbeatUpgradePolicy{HubManaged: true, NextWindowAt: "2026-10-27T17:00:00Z"})
	drainCalls := 0
	s.SetUpgradeDrainSource(func(now time.Time) *spoke.UpgradeDrainReport {
		drainCalls++
		return &spoke.UpgradeDrainReport{Target: "abc1234", RemainingS: 90, InFlight: []string{"reviewer"}}
	})

	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		UpgradeProgress *spoke.UpgradeProgress `json:"upgradeProgress"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if drainCalls != 1 {
		t.Fatalf("drain source called %d times, want 1 (serve-time)", drainCalls)
	}
	// The live drain outranks the persisted marker.
	if body.UpgradeProgress == nil || body.UpgradeProgress.State != spoke.UpgradeStateDraining {
		t.Fatalf("upgradeProgress = %+v", body.UpgradeProgress)
	}
	if body.UpgradeProgress.DrainRemainingS != 90 || len(body.UpgradeProgress.AgentsInTurn) != 1 || body.UpgradeProgress.AgentsInTurn[0].Agent != "reviewer" {
		t.Fatalf("drain fields = %+v", body.UpgradeProgress)
	}
	if body.UpgradeProgress.NextWindowAt != "2026-10-27T17:00:00Z" {
		t.Fatalf("nextWindowAt = %q", body.UpgradeProgress.NextWindowAt)
	}

	// Without the drain the marker speaks.
	s.SetUpgradeDrainSource(nil)
	rec = httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status?fields=upgradeProgress", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.UpgradeProgress == nil || body.UpgradeProgress.State != spoke.UpgradeStatePulling {
		t.Fatalf("marker-only upgradeProgress = %+v", body.UpgradeProgress)
	}
}
