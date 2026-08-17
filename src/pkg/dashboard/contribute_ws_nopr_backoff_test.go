package dashboard

import (
	"fmt"
	"testing"
	"time"
)

// #3980 backstop suite: a completion that ships nothing ("the agent correctly
// determined there is nothing to ship") used to earn the FASTEST re-offer
// (completedNoPRCooldownHours = 4h) every single time, so an issue whose
// remaining work is maintainer-gated (#2547: the shippable parts merged, no
// open PR left to claim it) was re-dispatched every 4h forever. The escalating
// no-PR cooldown bounds that loop regardless of any future claim-parsing gap.

// noPRKey mirrors markTaskCompleted's key derivation.
func noPRKey(repo string, number int) string {
	return fmt.Sprintf("%s#%d", repo, number)
}

func recordedCooldown(t *testing.T, hub *ContributeWSHub, key string) time.Duration {
	t.Helper()
	hub.completedMu.Lock()
	defer hub.completedMu.Unlock()
	d, ok := hub.completedTaskCooldown[key]
	if !ok {
		t.Fatalf("no cooldown recorded for %s", key)
	}
	return d
}

// TestNoPRCooldownEscalatesGeometrically: consecutive no-PR completions of the
// same issue double the cooldown from the 4h base up to the with-PR cap, so
// the waste converges to at most one task cycle per cap period.
func TestNoPRCooldownEscalatesGeometrically(t *testing.T) {
	hub, _ := covK2Hub(t)
	const repo = "nopr-backoff/escalation"
	key := noPRKey(repo, 1)

	base := completedNoPRCooldownHours * time.Hour
	capD := hub.configuredWithPRCooldown()
	want := base
	for i := 0; i < 8; i++ {
		hub.markTaskCompleted(repo, 1, "")
		if got := recordedCooldown(t, hub, key); got != want {
			t.Fatalf("completion %d: cooldown = %v, want %v", i+1, got, want)
		}
		want *= 2
		if want > capD {
			want = capD
		}
	}
	// After 8 rounds (4h→…→168h cap on round 7) the cooldown must be pinned
	// at the cap, not still doubling.
	if got := recordedCooldown(t, hub, key); got != capD {
		t.Fatalf("escalation must cap at the with-PR cooldown: got %v, want %v", got, capD)
	}
}

// TestNoPRCooldownStreakSurvivesCooldownSweep pins the loop's exact shape: the
// completion entry EXPIRES (and isTaskInCooldown sweeps it) before the next
// dispatch, so the streak must live outside the swept maps or every re-offer
// would restart the ladder at 4h and the loop would never slow down.
func TestNoPRCooldownStreakSurvivesCooldownSweep(t *testing.T) {
	hub, _ := covK2Hub(t)
	const repo = "nopr-backoff/sweep"
	key := noPRKey(repo, 2)

	hub.markTaskCompleted(repo, 2, "")

	// Age the completion past its 4h cooldown and let the sweep collect it,
	// exactly as happens between re-offers on a live hub.
	hub.completedMu.Lock()
	hub.completedTasks[key] = time.Now().Add(-5 * time.Hour)
	hub.completedMu.Unlock()
	if hub.isTaskInCooldown(repo, 2) {
		t.Fatal("aged completion should have left cooldown")
	}
	hub.completedMu.Lock()
	_, stillPresent := hub.completedTasks[key]
	hub.completedMu.Unlock()
	if stillPresent {
		t.Fatal("expected the sweep to remove the expired completion entry")
	}

	// The NEXT no-PR completion must continue the ladder (8h), not restart it.
	hub.markTaskCompleted(repo, 2, "")
	if got := recordedCooldown(t, hub, key); got != 8*time.Hour {
		t.Fatalf("streak must survive the cooldown sweep: got %v, want 8h", got)
	}
}

// TestNoPRCooldownResetByShippedPR: a completion that ships a (verified) PR
// books the full with-PR cooldown and ends the streak, so a later no-PR
// completion starts back at the 4h base — the original #2393 behavior for a
// first idle completion is preserved.
func TestNoPRCooldownResetByShippedPR(t *testing.T) {
	hub, _ := covK2Hub(t)
	const repo = "nopr-backoff/reset"
	key := noPRKey(repo, 3)

	hub.markTaskCompleted(repo, 3, "")
	hub.markTaskCompleted(repo, 3, "")
	if got := recordedCooldown(t, hub, key); got != 8*time.Hour {
		t.Fatalf("setup: expected 8h after two no-PR completions, got %v", got)
	}

	hub.markTaskCompleted(repo, 3, "https://github.com/x/y/pull/9")
	if got := recordedCooldown(t, hub, key); got != hub.configuredWithPRCooldown() {
		t.Fatalf("with-PR completion must book the full cooldown, got %v", got)
	}

	hub.markTaskCompleted(repo, 3, "")
	if got := recordedCooldown(t, hub, key); got != completedNoPRCooldownHours*time.Hour {
		t.Fatalf("streak must reset after a shipped PR: got %v, want %v", got, completedNoPRCooldownHours*time.Hour)
	}
}

// TestNoPRCooldownStreakForgetsAfterQuiet: a streak older than
// noPRStreakResetAfter is discarded, so ancient history cannot slow down an
// issue that has been out of circulation for weeks.
func TestNoPRCooldownStreakForgetsAfterQuiet(t *testing.T) {
	hub, _ := covK2Hub(t)
	const repo = "nopr-backoff/quiet"
	key := noPRKey(repo, 4)

	hub.completedMu.Lock()
	hub.noPRStreaks[key] = noPRStreakRecord{Count: 5, LastAt: time.Now().Add(-noPRStreakResetAfter - time.Hour)}
	hub.completedMu.Unlock()

	hub.markTaskCompleted(repo, 4, "")
	if got := recordedCooldown(t, hub, key); got != completedNoPRCooldownHours*time.Hour {
		t.Fatalf("stale streak must reset to the base cooldown: got %v", got)
	}
}
