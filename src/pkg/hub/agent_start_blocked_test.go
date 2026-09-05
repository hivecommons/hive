package hub

import (
	"testing"
	"time"
)

// #5958, fleet half. The incident hive (#5921) showed "● on · should be
// working" for agents whose CLI had never started, and counted them toward the
// "K able" header. The spoke now reports WHY it stopped relaunching; these tests
// pin that the hub spends that report instead of dropping it.

// startBlocked builds the wire shape a spoke sends for an agent it has given up
// relaunching. State is what the incident actually reported: the manager still
// believed the session was up, which is why nothing downstream flagged it.
func startBlocked(now time.Time, reason string) AgentSummary {
	a := modernWorking(now)
	a.Name = "supervisor"
	a.Backend = "copilot"
	a.StartBlockedReason = reason
	return a
}

// TestVerdict_StartBlockedIsNotAble is the core of the fleet half: an agent the
// spoke has stopped relaunching cannot be able, whatever its reported state.
func TestVerdict_StartBlockedIsNotAble(t *testing.T) {
	now := time.Now()
	v := deriveAgentVerdict(startBlocked(now, "copilot: not logged in"), hiveBlockers{}, 5, now)

	if v.Able {
		t.Error("a start-blocked agent counted as able — this is the over-count that let a hive show green agents that had never started")
	}
	if v.CapabilityTier != tierRed {
		t.Errorf("tier = %s, want red: an agent whose CLI never came up does no part of its job", v.CapabilityTier)
	}
	if !v.Problem {
		t.Error("start-blocked agent is not a Problem — it is the one signal the operator scans for")
	}
	if !v.Stuck {
		t.Error("start-blocked agent is not Stuck")
	}
}

// The spoke's concrete reason must survive to the row. "restart needed" is what
// the incident showed and what sent operators clicking a button that could not
// help; the reason is the entire deliverable.
func TestVerdict_StartBlockedReasonReachesTheRow(t *testing.T) {
	now := time.Now()
	for _, reason := range []string{
		"copilot: not logged in",
		"bob: API key rejected (401)",
		"backend/launch_cmd mismatch (configured copilot, launches bob)",
	} {
		v := deriveAgentVerdict(startBlocked(now, reason), hiveBlockers{}, 5, now)
		if v.BlockedReason != reason {
			t.Errorf("BlockedReason = %q, want the spoke's own reason %q", v.BlockedReason, reason)
		}
	}
}

// The spoke's diagnosis outranks the hub's generic phrasing for the same fault.
// "copilot: not logged in" and "sitting at login prompt" describe one condition;
// only the first says relaunching has been given up on.
func TestVerdict_PreThresholdStartFailureNamesReason(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 0, 0, 0, time.UTC)
	a := modernWorking(now)
	a.Name = "supervisor"
	a.State = "failed"
	a.StartFailureReason = "copilot: no CLI prompt after launch"
	a.StartFailureCount = 2
	a.StartFailureLastAt = now.Add(-5 * time.Minute).Format(time.RFC3339)

	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	want := "starting failed ×2: copilot: no CLI prompt after launch (last 5m ago)"
	if !v.Problem || !v.Stuck || v.BlockedReason != want {
		t.Fatalf("verdict = %+v, want blocker %q", v, want)
	}
}

func TestVerdict_AbsentStartFailureKeepsGenericDown(t *testing.T) {
	now := time.Now()
	a := modernWorking(now)
	a.State = "failed"
	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)
	if !v.Problem || v.BlockedReason != "" || v.RunState != runDead {
		t.Fatalf("verdict = %+v, want legacy generic dead verdict", v)
	}
}

func TestVerdict_StartBlockedReasonOutranksGenericLoginWording(t *testing.T) {
	now := time.Now()
	a := startBlocked(now, "copilot: not logged in")
	a.NeedsLogin = true

	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)

	if v.BlockedReason != "copilot: not logged in" {
		t.Errorf("BlockedReason = %q, want the spoke's specific reason to win", v.BlockedReason)
	}
}

// A start-blocked agent must not read as AMBER ("half its job works"). It does
// no half of its job — the same reason a dead gateway is excluded from amber.
func TestVerdict_StartBlockedIsNeverAmber(t *testing.T) {
	now := time.Now()
	a := startBlocked(now, "bob: API key rejected (401)")
	a.CanOpenIssue, a.CanOpenPR, a.CanMerge = true, true, true

	if v := deriveAgentVerdict(a, hiveBlockers{}, 5, now); v.CapabilityTier != tierRed {
		t.Errorf("tier = %s, want red — amber would claim partial capability for a CLI that never ran", v.CapabilityTier)
	}
}

// An OFF-SCHEDULE start-blocked agent is not an alarm, and this is the
// deliberate limit of the fix rather than an oversight.
//
// runStuckAtLogin gets a bypass past the expectedActive gate because a wedged
// interactive credential is hive-wide: every kick to that backend fails. A
// failed start is one agent. If the governor is not scheduling it in this mode,
// nobody asked it to run, and raising a Problem would break the rule the rest of
// the fleet view keeps — quiet by design is never a fault. The count still stays
// honest: it is not ABLE either.
func TestVerdict_OffScheduleStartBlockedIsNotAnAlarm(t *testing.T) {
	now := time.Now()
	a := startBlocked(now, "copilot: not logged in")
	a.ExpectedActive = false

	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)

	if v.Able {
		t.Error("start-blocked agent counted as able while off schedule")
	}
	if v.Problem {
		t.Error("an agent the governor is not scheduling raised a Problem — quiet by design is never a fault")
	}
	if !v.QuietByDesign {
		t.Error("off-schedule agent is not quiet-by-design")
	}
}

// ...and the moment the mode DOES schedule it, the same agent becomes the alarm
// it should be. This is the pair that makes the carve-out above safe: the fault
// is deferred, never dropped.
func TestVerdict_ScheduledStartBlockedIsAnAlarm(t *testing.T) {
	now := time.Now()
	a := startBlocked(now, "copilot: not logged in")
	a.ExpectedActive = true

	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)

	if v.Able {
		t.Error("start-blocked agent counted as able while scheduled")
	}
	if !v.Problem {
		t.Error("a scheduled agent that cannot start is not a Problem — this is the #5921 miss")
	}
}

// A paused agent is the operator's own choice and must stay quiet-by-design even
// carrying a stale reason: the mechanism must not manufacture alarms on agents
// nobody is asking to run.
func TestVerdict_PausedStartBlockedStaysQuiet(t *testing.T) {
	now := time.Now()
	a := startBlocked(now, "copilot: not logged in")
	a.Paused = true

	v := deriveAgentVerdict(a, hiveBlockers{}, 5, now)

	if v.Problem {
		t.Error("a paused agent raised a Problem — pausing is the operator's choice, never a fault")
	}
	if !v.QuietByDesign {
		t.Error("paused agent is not quiet-by-design")
	}
}

// The rollup is where the count the header prints is actually formed.
func TestRollup_StartBlockedExcludedFromAble(t *testing.T) {
	now := time.Now()
	healthy := modernWorking(now)
	healthy.Name = "guide"

	r := rollupAgents([]AgentSummary{startBlocked(now, "copilot: not logged in"), healthy}, hiveBlockers{}, 5, now)

	if r.Able != 1 {
		t.Errorf("rollup able = %d, want 1 — the blocked agent must not inflate the header", r.Able)
	}
	if r.Problems != 1 {
		t.Errorf("rollup problems = %d, want 1", r.Problems)
	}
}

// An empty reason is what every legacy spoke sends, and it must read as "no such
// fault" rather than dragging healthy agents red.
func TestVerdict_NoStartBlockedReasonChangesNothing(t *testing.T) {
	now := time.Now()
	v := deriveAgentVerdict(startBlocked(now, ""), hiveBlockers{}, 5, now)

	if !v.Able || v.CapabilityTier != tierGreen {
		t.Errorf("agent with no start-blocked reason = able %v tier %s, want able/green", v.Able, v.CapabilityTier)
	}
	if v.BlockedReason != "" {
		t.Errorf("BlockedReason = %q, want empty", v.BlockedReason)
	}
}

// Whitespace is not a reason. A spoke that sends " " must not block an agent on
// a value that renders as nothing in the row.
func TestVerdict_BlankStartBlockedReasonIsNotABlock(t *testing.T) {
	now := time.Now()
	if v := deriveAgentVerdict(startBlocked(now, "   "), hiveBlockers{}, 5, now); !v.Able {
		t.Error("a whitespace-only reason blocked the agent, and would render as an empty explanation")
	}
}

// The wire field must survive NewAgentSummary: a signal the builder drops is a
// signal that quietly disappears, which is what AgentActivity exists to prevent.
func TestNewAgentSummaryCarriesStartBlockedReason(t *testing.T) {
	as := NewAgentSummary("supervisor", "failed", "ADVISORY", AgentActivity{
		StartBlockedReason: "copilot: not logged in",
	})
	if as.StartBlockedReason != "copilot: not logged in" {
		t.Errorf("StartBlockedReason = %q, want it carried through", as.StartBlockedReason)
	}
}

func TestNewAgentSummaryCarriesCurrentStartFailure(t *testing.T) {
	now := time.Date(2026, 9, 4, 23, 4, 0, 0, time.FixedZone("local", -4*60*60))
	exitCode := 77
	as := NewAgentSummary("supervisor", "failed", "ADVISORY", AgentActivity{
		StartFailureReason:   "copilot: no CLI prompt after launch",
		StartFailureCount:    2,
		StartFailureLastAt:   now,
		StartFailureExitCode: &exitCode,
		StartFailureSignal:   "TERM",
	})
	if as.StartFailureReason != "copilot: no CLI prompt after launch" ||
		as.StartFailureCount != 2 ||
		as.StartFailureLastAt != now.UTC().Format(time.RFC3339) ||
		as.StartFailureExitCode == nil || *as.StartFailureExitCode != 77 ||
		as.StartFailureSignal != "TERM" {
		t.Fatalf("start failure fields were not carried through: %+v", as)
	}
}
