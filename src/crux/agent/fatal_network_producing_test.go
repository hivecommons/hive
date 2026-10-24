package agent

import (
	"testing"
	"time"
)

// The fatal-network detector restarts an agent on a pattern found in
// SCROLLBACK. agentIsProducing is the veto that stops it from killing an agent
// that matched an error it has already worked past. These tests pin the two
// halves of that contract: a live agent is protected, a dead one is not.
func TestAgentIsProducing(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	grace := time.Duration(fatalNetworkProducingGraceSec) * time.Second

	tests := []struct {
		name           string
		lastPaneChange time.Time
		want           bool
		why            string
	}{
		{
			name:           "streaming right now",
			lastPaneChange: now.Add(-32 * time.Millisecond),
			want:           true,
			why:            "the production case: the reviewer was 32ms from its last render, mid-turn, when it was killed",
		},
		{
			name:           "paused between rendered chunks",
			lastPaneChange: now.Add(-grace + time.Second),
			want:           true,
			why:            "a CLI mid-answer can stall longer than one 3s poll without being dead",
		},
		{
			name:           "silent for the whole grace window",
			lastPaneChange: now.Add(-grace),
			want:           false,
			why:            "the boundary is exclusive, so exactly-grace is not producing",
		},
		{
			name:           "long dead but visually ready",
			lastPaneChange: now.Add(-10 * time.Minute),
			want:           false,
			why:            "this is the case the detector exists for and must keep catching",
		},
		{
			name:           "pane never observed to change",
			lastPaneChange: time.Time{},
			want:           false,
			why:            "an agent that died at startup never advances the clock; the restart must still fire",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentIsProducing(tt.lastPaneChange, now); got != tt.want {
				t.Errorf("agentIsProducing() = %v, want %v — %s", got, tt.want, tt.why)
			}
		})
	}
}

// The veto must not swallow the detector: an agent that is dead AND shows a
// fatal pattern still restarts. This pins the two predicates together, because
// the bug being fixed was a restart firing while the first was false.
func TestFatalNetworkRestartStillFiresForADeadAgent(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	scrollback := []string{"ERROR: fetch failed"}

	if !paneShowsFatalNetworkError(scrollback) {
		t.Fatal("precondition: scrollback must match the fatal pattern")
	}

	dead := now.Add(-10 * time.Minute)
	if agentIsProducing(dead, now) {
		t.Fatal("a pane static for 10 minutes must not count as producing, or the detector is dead code")
	}

	live := now.Add(-time.Second)
	if !agentIsProducing(live, now) {
		t.Error("a pane that changed a second ago must veto the restart")
	}
}

// fatalNetworkProducingGraceSec must stay wider than the poll interval. If it
// ever drops to one interval or less, an agent that renders slightly slower
// than the poller would be declared dead on an ordinary stall — reintroducing
// the bug this guard fixes.
func TestProducingGraceExceedsPollInterval(t *testing.T) {
	const observedPollIntervalSec = 3
	if fatalNetworkProducingGraceSec <= observedPollIntervalSec {
		t.Errorf("fatalNetworkProducingGraceSec = %d, must exceed the ~%ds poll interval by a margin",
			fatalNetworkProducingGraceSec, observedPollIntervalSec)
	}
}
