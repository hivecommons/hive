package hub

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

func TestUpgradeProgressForStates(t *testing.T) {
	loc, err := autoUpgradeLocation()
	if err != nil {
		t.Fatal(err)
	}
	// 14:00 ET on a Monday: the daily window is open.
	now := time.Date(2026, 10, 26, 14, 0, 0, 0, loc)
	started := now.Add(-20 * time.Minute)
	agents := []AgentSummary{{Name: "reviewer"}, {Name: "scanner"}, {Name: "triage"}}

	cases := []struct {
		name    string
		in      upgradeProgressInput
		state   string
		detail  string // substring
		remain  int
		inTurn  int
		nextWin bool
	}{
		{
			name:  "spoke-reported failure is missed",
			in:    upgradeProgressInput{Entry: &RegistryEntry{UpgradeFailed: true, UpgradeError: "image pull backoff", UpgradeTarget: "abc1234"}},
			state: spoke.UpgradeStateMissed, detail: "image pull backoff",
		},
		{
			name: "drain report is draining with countdown and agents",
			in: upgradeProgressInput{Entry: &RegistryEntry{
				Upgrading: true, UpgradeTarget: "abc1234", GitHash: "551ffc3", UpgradeStartedAt: started,
				LastHeartbeat: now.Add(-30 * time.Second).UTC().Format(time.RFC3339), Agents: agents,
				UpgradeDrain: &UpgradeDrainReport{Target: "abc1234", RemainingS: 600, WaitedS: 300,
					Turns: []spoke.UpgradeTurn{{Agent: "reviewer", ElapsedS: 38 * 60}, {Agent: "scanner", ElapsedS: 12 * 60}}},
			}},
			state: spoke.UpgradeStateDraining, detail: "2/3 agents in turn: reviewer 38m, scanner 12m", remain: 570, inTurn: 2,
		},
		{
			name: "legacy drain report without turns still names agents",
			in: upgradeProgressInput{Entry: &RegistryEntry{
				Upgrading: true, UpgradeTarget: "abc1234", UpgradeStartedAt: started, Agents: agents,
				UpgradeDrain: &UpgradeDrainReport{Target: "abc1234", RemainingS: 60, InFlight: []string{"reviewer"}},
			}},
			state: spoke.UpgradeStateDraining, detail: "1/3 agent in turn: reviewer", inTurn: 1,
		},
		{
			name: "beat after instruction on target is verifying",
			in: upgradeProgressInput{Entry: &RegistryEntry{
				Upgrading: true, UpgradeTarget: "abc1234", GitHash: "abc1234", UpgradeStartedAt: started,
				LastHeartbeat: now.Add(-time.Minute).UTC().Format(time.RFC3339),
			}},
			state: spoke.UpgradeStateVerifying, detail: "verifying",
		},
		{
			name: "spoke acknowledged is pulling",
			in: upgradeProgressInput{Entry: &RegistryEntry{
				Upgrading: true, UpgradeTarget: "abc1234", GitHash: "551ffc3", UpgradeStartedAt: started, SpokeReportedUpgrading: true,
				LastHeartbeat: now.Add(-time.Minute).UTC().Format(time.RFC3339),
			}},
			state: spoke.UpgradeStatePulling, detail: "acknowledged",
		},
		{
			name: "silent since instruction is rolling",
			in: upgradeProgressInput{Entry: &RegistryEntry{
				Upgrading: true, UpgradeTarget: "abc1234", GitHash: "551ffc3", UpgradeStartedAt: started,
				LastHeartbeat: started.Add(-time.Minute).UTC().Format(time.RFC3339),
			}},
			state: spoke.UpgradeStateRolling, detail: "restarting",
		},
		{
			name: "beating on old hash without ack is pulling with a qualifier",
			in: upgradeProgressInput{Entry: &RegistryEntry{
				Upgrading: true, UpgradeTarget: "abc1234", GitHash: "551ffc3", UpgradeStartedAt: started,
				LastHeartbeat: now.Add(-time.Minute).UTC().Format(time.RFC3339),
			}},
			state: spoke.UpgradeStatePulling, detail: "draining silently",
		},
		{
			name: "open window held by the gate is delayed",
			in: upgradeProgressInput{Entry: &RegistryEntry{GitHash: "551ffc3"},
				AutoUpgrade: true, Mode: AutoUpgradeModeDaily, Channel: ReleaseChannelStable,
				HoldReason: "hub restarted; stuck-upgrade latch recovering", HoldAt: now.Add(-10 * time.Minute)},
			state: spoke.UpgradeStateDelayed, detail: "delayed", nextWin: true,
		},
		{
			name: "legacy missed window is delayed",
			in: upgradeProgressInput{Entry: &RegistryEntry{GitHash: "551ffc3"},
				AutoUpgrade: true, Mode: AutoUpgradeModeDaily, LastFired: now.Format(autoUpgradeDateFormat),
				History: []spoke.UpgradeAttempt{{Trigger: spoke.UpgradeTriggerWindow, Outcome: spoke.UpgradeOutcomeMissed,
					Reason: "drain lapsed", StartedAt: now.Add(-2 * time.Hour), EndedAt: now.Add(-2 * time.Hour)}}},
			state: spoke.UpgradeStateDelayed, detail: "delayed: drain lapsed", nextWin: true,
		},
		{
			name: "successful upgrade clears a stale live hold and legacy window",
			in: upgradeProgressInput{Entry: &RegistryEntry{GitHash: "abc1234"},
				AutoUpgrade: true, Mode: AutoUpgradeModeDaily, HoldReason: "merge debounce", HoldAt: now.Add(-10 * time.Minute),
				History: []spoke.UpgradeAttempt{
					{Trigger: spoke.UpgradeTriggerWindow, Outcome: spoke.UpgradeOutcomeMissed, Reason: "merge debounce", EndedAt: now.Add(-10 * time.Minute)},
					{Target: "abc1234", Outcome: spoke.UpgradeOutcomeDone, EndedAt: now.Add(-5 * time.Minute)},
				}},
			state: spoke.UpgradeStateDone, detail: "upgraded to abc1234", nextWin: true,
		},
		{
			name: "new hold after a successful upgrade remains visible",
			in: upgradeProgressInput{Entry: &RegistryEntry{GitHash: "abc1234"},
				AutoUpgrade: true, Mode: AutoUpgradeModeDaily, HoldReason: "merge debounce", HoldAt: now.Add(-time.Minute),
				History: []spoke.UpgradeAttempt{
					{Target: "abc1234", Outcome: spoke.UpgradeOutcomeDone, EndedAt: now.Add(-5 * time.Minute)},
				}},
			state: spoke.UpgradeStateDelayed, detail: "merge debounce", nextWin: true,
		},
		{
			name: "genuine failed attempt is not relabeled",
			in: upgradeProgressInput{Entry: &RegistryEntry{GitHash: "551ffc3"},
				History: []spoke.UpgradeAttempt{{Trigger: spoke.UpgradeTriggerScheduled, Outcome: spoke.UpgradeOutcomeMissed,
					Reason: "ErrImagePull", EndedAt: now.Add(-time.Minute)}}},
			state: spoke.UpgradeStateMissed, detail: "ErrImagePull",
		},
		{
			name: "stale hold does not mask a later failed attempt",
			in: upgradeProgressInput{Entry: &RegistryEntry{GitHash: "551ffc3"},
				AutoUpgrade: true, Mode: AutoUpgradeModeDaily, HoldReason: "merge debounce", HoldAt: now.Add(-10 * time.Minute),
				History: []spoke.UpgradeAttempt{{Trigger: spoke.UpgradeTriggerScheduled, Outcome: spoke.UpgradeOutcomeMissed,
					Reason: "ErrImagePull", EndedAt: now.Add(-time.Minute)}}},
			state: spoke.UpgradeStateMissed, detail: "ErrImagePull", nextWin: true,
		},
		{
			name: "recent done from history",
			in: upgradeProgressInput{Entry: &RegistryEntry{GitHash: "abc1234"},
				History: []spoke.UpgradeAttempt{{Target: "abc1234", Outcome: spoke.UpgradeOutcomeDone,
					StartedAt: now.Add(-3 * time.Hour), EndedAt: now.Add(-3 * time.Hour)}}},
			state: spoke.UpgradeStateDone, detail: "upgraded to abc1234 3h0m ago",
		},
		{
			name: "old history falls back to scheduled",
			in: upgradeProgressInput{Entry: &RegistryEntry{GitHash: "abc1234"},
				AutoUpgrade: true, Mode: AutoUpgradeModeDaily, LastFired: now.Format(autoUpgradeDateFormat),
				History: []spoke.UpgradeAttempt{{Outcome: spoke.UpgradeOutcomeDone,
					StartedAt: now.Add(-48 * time.Hour), EndedAt: now.Add(-48 * time.Hour)}}},
			state: spoke.UpgradeStateScheduled, detail: "next window", nextWin: true,
		},
		{
			name:  "instant mode is scheduled on next poll",
			in:    upgradeProgressInput{Entry: &RegistryEntry{}, AutoUpgrade: true, Mode: AutoUpgradeModeInstant},
			state: spoke.UpgradeStateScheduled, detail: "next poll",
		},
		{
			name:  "manual hive",
			in:    upgradeProgressInput{Entry: &RegistryEntry{}},
			state: spoke.UpgradeStateScheduled, detail: "manual",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.in.Now = now
			p := upgradeProgressFor(tc.in)
			if p == nil {
				t.Fatal("nil progress")
			}
			if p.State != tc.state {
				t.Fatalf("state = %q want %q (%+v)", p.State, tc.state, p)
			}
			if !strings.Contains(p.Detail, tc.detail) {
				t.Fatalf("detail %q lacks %q", p.Detail, tc.detail)
			}
			if tc.remain != 0 && p.DrainRemainingS != tc.remain {
				t.Fatalf("remaining = %d want %d", p.DrainRemainingS, tc.remain)
			}
			if len(p.AgentsInTurn) != tc.inTurn {
				t.Fatalf("agents in turn = %d want %d", len(p.AgentsInTurn), tc.inTurn)
			}
			if tc.nextWin != (p.NextWindowAt != "") {
				t.Fatalf("nextWindowAt = %q, want present=%v", p.NextWindowAt, tc.nextWin)
			}
			if p.State == spoke.UpgradeStateDraining && p.DrainDeadline == "" {
				t.Fatal("draining must carry a deadline")
			}
		})
	}
	if upgradeProgressFor(upgradeProgressInput{}) != nil {
		t.Fatal("nil entry must yield nil")
	}
}

func TestUpgradeProgressInFlight(t *testing.T) {
	for _, st := range []string{spoke.UpgradeStateDraining, spoke.UpgradeStatePulling, spoke.UpgradeStateRolling, spoke.UpgradeStateVerifying} {
		if !(&spoke.UpgradeProgress{State: st}).InFlight() {
			t.Fatalf("%s should be in flight", st)
		}
	}
	for _, st := range []string{spoke.UpgradeStateScheduled, spoke.UpgradeStateDelayed, spoke.UpgradeStateDone, spoke.UpgradeStateMissed} {
		if (&spoke.UpgradeProgress{State: st}).InFlight() {
			t.Fatalf("%s should not be in flight", st)
		}
	}
	var nilP *spoke.UpgradeProgress
	if nilP.InFlight() {
		t.Fatal("nil must not be in flight")
	}
}

func TestHeartbeatUpgradePolicyCarriesWindowAndHistory(t *testing.T) {
	useTempUpgradeHistoryDir(t)
	stubBranchHead(t, "v5", "5193426")
	stubChannelRevisions(t, map[string]string{"stable": "5193426"})
	s := &HubServer{logger: targetingLogger(), upgradeHistory: newUpgradeHistoryStore()}
	s.upgradeHistory.windowMissed("fma", "551ffc3", "hub restarted", time.Now().Add(-time.Hour))
	sh := &SaaSHive{ID: "fma", AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeDaily, TrackedChannel: ReleaseChannelStable}
	payload := &HeartbeatPayload{HiveID: "fma"}
	policy := s.heartbeatUpgradePolicy(payload, sh, false, false, "v5", "")
	if policy == nil {
		t.Fatal("nil policy")
	}
	if policy.NextWindowAt == "" {
		t.Fatal("stable daily hive must still report its own next window (#14856)")
	}
	if _, err := time.Parse(time.RFC3339, policy.NextWindowAt); err != nil {
		t.Fatalf("NextWindowAt not RFC3339: %q", policy.NextWindowAt)
	}
	if len(policy.UpgradeHistory) != 1 || policy.UpgradeHistory[0].Outcome != spoke.UpgradeOutcomeDelayed || policy.UpgradeHistory[0].Reason != "hub restarted" {
		t.Fatalf("history not delivered on the policy: %+v", policy.UpgradeHistory)
	}
	s.upgradeHistory.end("fma", spoke.UpgradeOutcomeDone, "", "551ffc3", "5193426", time.Now())
	policy = s.heartbeatUpgradePolicy(payload, sh, false, false, "v5", "")
	if policy == nil || len(policy.UpgradeHistory) != 1 || policy.UpgradeHistory[0].Outcome != spoke.UpgradeOutcomeDone {
		t.Fatalf("successful upgrade must remove the delay from the policy: %+v", policy)
	}
}

func TestUpgradeProgressForRowReadsSaaSHiveAndHistory(t *testing.T) {
	useTempUpgradeHistoryDir(t)
	s := &HubServer{logger: targetingLogger(), upgradeHistory: newUpgradeHistoryStore()}
	now := time.Now()
	s.upgradeHistory.start("h", "551ffc3", "abc1234", spoke.UpgradeTriggerScheduled, now.Add(-time.Hour))
	s.upgradeHistory.end("h", spoke.UpgradeOutcomeDone, "", "", "", now.Add(-50*time.Minute))
	entry := &RegistryEntry{ID: "h", GitHash: "abc1234"}
	sh := &SaaSHive{ID: "h", AutoUpgrade: true, AutoUpgradeMode: AutoUpgradeModeDaily}
	p, hist := s.upgradeProgressForRow(entry, sh, "", now)
	if p == nil || p.State != spoke.UpgradeStateDone {
		t.Fatalf("progress = %+v", p)
	}
	if len(hist) != 1 {
		t.Fatalf("history = %+v", hist)
	}
	if p2, _ := s.upgradeProgressForRow(&RegistryEntry{ID: "other"}, nil, "", now); p2 == nil || p2.State != spoke.UpgradeStateScheduled {
		t.Fatalf("unmanaged spoke progress = %+v", p2)
	}
}

func TestRecordUpgradeHistoryTransition(t *testing.T) {
	useTempUpgradeHistoryDir(t)
	s := &HubServer{logger: targetingLogger(), upgradeHistory: newUpgradeHistoryStore()}
	idle := &RegistryEntry{ID: "h", GitHash: "551ffc3"}
	rolling := &RegistryEntry{ID: "h", GitHash: "551ffc3", Upgrading: true, UpgradeTarget: "abc1234"}
	s.recordUpgradeHistoryTransition(idle, rolling)
	if open, ok := s.upgradeHistory.open("h"); !ok || open.Trigger != spoke.UpgradeTriggerObserved || open.Target != "abc1234" {
		t.Fatalf("observed start not recorded: %+v ok=%v", open, ok)
	}
	landed := &RegistryEntry{ID: "h", GitHash: "abc1234"}
	s.recordUpgradeHistoryTransition(rolling, landed)
	got := s.upgradeHistory.recent("h")
	if len(got) != 1 || got[0].Outcome != spoke.UpgradeOutcomeDone || got[0].Target != "abc1234" {
		t.Fatalf("landed row = %+v", got)
	}

	s.recordUpgradeHistoryTransition(idle, rolling)
	failed := &RegistryEntry{ID: "h", GitHash: "551ffc3", UpgradeFailed: true, UpgradeError: "ErrImagePull"}
	s.recordUpgradeHistoryTransition(rolling, failed)
	got = s.upgradeHistory.recent("h")
	if len(got) != 2 || got[1].Outcome != spoke.UpgradeOutcomeMissed || got[1].Reason != "ErrImagePull" {
		t.Fatalf("failed row = %+v", got)
	}
}
