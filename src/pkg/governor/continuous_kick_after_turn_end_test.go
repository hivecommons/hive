package governor

import (
	"testing"
	"time"
)

// TestRecordKickForRepoAtKeepsScheduleWhenTurnAlreadyEnded pins the hosted
// scanner stall: the kick went out at 04:10:42, the turn ended at 04:13:48
// (NextKick armed for 04:14:48), and only then — at 04:14:45, after another
// lane's prompt timeouts — did the dispatch pass record the kick. Zeroing
// NextKick at that point left the scanner idle with no re-arm path until the
// 45-minute turn-ceiling presumption. The recorded kick must keep the
// schedule the turn end already produced.
func TestRecordKickForRepoAtKeepsScheduleWhenTurnAlreadyEnded(t *testing.T) {
	now := time.Date(2026, 10, 28, 4, 10, 0, 0, time.UTC)
	g := continuousWakeGovernor(&now)
	g.Evaluate(0, 0, 0, 0)

	kickedAt := now.Add(42 * time.Second)
	endedAt := kickedAt.Add(3 * time.Minute)
	now = endedAt
	g.RecordKickOutcome("reviewer", KickOutcomeEnded, "done", kickedAt, endedAt)
	wantNext, who := g.NextContinuousKickAt()
	if who != "reviewer" || wantNext.IsZero() {
		t.Fatalf("turn end must arm the re-kick, got %v %q", wantNext, who)
	}

	now = endedAt.Add(57 * time.Second)
	g.RecordKickForRepoAt("reviewer", "", kickedAt)

	st := g.GetState().Continuous["reviewer"]
	if !st.NextKick.Equal(wantNext) {
		t.Fatalf("NextKick = %v after late kick record, want %v kept", st.NextKick, wantNext)
	}
	if lk := g.GetState().LastKick["reviewer"]; !lk.Equal(kickedAt) {
		t.Fatalf("LastKick = %v, want the delivery time %v", lk, kickedAt)
	}
	if at, _ := g.NextContinuousKickAt(); !at.Equal(wantNext) {
		t.Fatalf("NextContinuousKickAt = %v, want %v", at, wantNext)
	}
	// The next eval still finds the agent due once the cooldown elapses.
	now = wantNext
	found := false
	for _, key := range g.agentsDueForKick() {
		found = found || key == "reviewer"
	}
	if !found {
		t.Fatalf("agentsDueForKick must include reviewer once the kept re-kick elapses")
	}
}

// TestRecordKickForRepoAtZeroesScheduleWhileTurnRunning keeps the original
// contract: a kick recorded while its turn is still running (no newer
// outcome) clears NextKick so the agent is not re-kicked mid-session.
func TestRecordKickForRepoAtZeroesScheduleWhileTurnRunning(t *testing.T) {
	now := time.Date(2026, 10, 28, 4, 10, 0, 0, time.UTC)
	g := continuousWakeGovernor(&now)
	g.Evaluate(0, 0, 0, 0)

	kickedAt := now.Add(42 * time.Second)
	now = kickedAt.Add(4 * time.Minute)
	g.RecordKickForRepoAt("reviewer", "", kickedAt)

	st := g.GetState().Continuous["reviewer"]
	if !st.NextKick.IsZero() {
		t.Fatalf("NextKick = %v while the kicked turn is running, want zero", st.NextKick)
	}
	if lk := g.GetState().LastKick["reviewer"]; !lk.Equal(kickedAt) {
		t.Fatalf("LastKick = %v, want %v", lk, kickedAt)
	}
}
