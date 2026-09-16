package escalation

import (
	"path/filepath"
	"testing"
	"time"
)

// The re-engagement budget (MaxReEngagements) is meant to bound how many fix
// attempts a stuck red PR gets before a human is paged. It was being spent at
// governor-tick pace instead of agent pace: StaleRed compares now-FirstRedAt,
// and FirstRedAt does not advance while the head SHA is unchanged, so a red PR
// past RedPRStaleAfter reads as stale on EVERY tick. The reaper drained all six
// re-engagements in consecutive ticks and escalated the PR to needs-human
// minutes later, having never given the owning agent a kick it could answer.
//
// Live evidence this reproduces: kubestellar/console#23459 and #23475 both
// reached re_engagements=6 with len(RedSHAs)==1 — parked for a human without a
// single repair attempt.
func TestTryReEngage_BudgetNotBurnedAtTickPace(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	now := time.Now().UTC()
	s.SetClock(func() time.Time { return now })

	if !s.TryReEngage("org/repo", 1, "sha1") {
		t.Fatal("first re-engagement should be granted")
	}

	// Replay the observed incident: the reaper fires every governor tick
	// (~2 min) while the PR stays red on one SHA. #23459 went from its first
	// re-engagement to needs-human in ~13 minutes this way.
	const tick = 2 * time.Minute
	const burnWindow = 13 * time.Minute
	for elapsed := time.Duration(0); elapsed < burnWindow; elapsed += tick {
		now = now.Add(tick)
		s.TryReEngage("org/repo", 1, "sha1")
	}

	if got := s.ReEngagements("org/repo", 1); got >= MaxReEngagements {
		t.Fatalf("budget exhausted inside the %s burn window: re_engagements=%d, want < %d",
			burnWindow, got, MaxReEngagements)
	}
	// Spacing is the whole point: at most one extra grant can fit in 13 min.
	if got := s.ReEngagements("org/repo", 1); got > 2 {
		t.Fatalf("re_engagements=%d in %s, want <= 2 with a %s cooldown",
			got, burnWindow, ReEngageCooldown)
	}
}

// Control: the budget is still finite. Spacing attempts by the cooldown must
// still reach the cap, otherwise a permanently-red PR would be nudged forever.
func TestTryReEngage_BudgetStillExhaustsAtAgentPace(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	now := time.Now().UTC()
	s.SetClock(func() time.Time { return now })

	granted := 0
	for i := 0; i < MaxReEngagements+3; i++ {
		if s.TryReEngage("org/repo", 2, "sha1") {
			granted++
		}
		now = now.Add(ReEngageCooldown + time.Minute)
	}
	if granted != MaxReEngagements {
		t.Fatalf("granted=%d, want exactly %d", granted, MaxReEngagements)
	}
}

// A pushed fix (new head SHA) must clear the cooldown as well as the counter,
// so an agent that just pushed is never made to wait before its next attempt.
func TestTryReEngage_NewSHAClearsCooldown(t *testing.T) {
	s := Load(filepath.Join(t.TempDir(), "ledger.json"))
	now := time.Now().UTC()
	s.SetClock(func() time.Time { return now })

	if !s.TryReEngage("org/repo", 3, "sha1") {
		t.Fatal("first re-engagement should be granted")
	}
	now = now.Add(30 * time.Second) // well inside the cooldown
	if !s.TryReEngage("org/repo", 3, "sha2") {
		t.Fatal("a new red head SHA must reset the cooldown, not inherit it")
	}
	if got := s.ReEngagements("org/repo", 3); got != 1 {
		t.Fatalf("re_engagements=%d after SHA change, want 1", got)
	}
}
