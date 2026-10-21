package dashboard

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#13093 regression: hanthor's relay failed #13093 three
// times in an hour ("pane went idle before omp produced any task output")
// and was handed the same issue back each time the ten-minute global
// failure cooldown lapsed, flapping its claim. Once the global cooldown is
// over, the SAME contributor must still not get the issue back inside the
// reclaim window, while a different contributor may take it at once.
func TestReclaimCooldown_SameContributorNotReofferedAfterFailure(t *testing.T) {
	hub, s := covK2Hub(t)
	twoIssueStatus(s)

	flaky := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "hanthor", ContributorID: "c-f20bd0da618e", TrustTier: "merger"},
		session:  "omp",
		lastPong: time.Now(),
	}
	other := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: "arteeh", ContributorID: "c-other", TrustTier: "contributor"},
		lastPong: time.Now(),
	}

	msg := hub.selectTask(flaky)
	if msg == nil || msg.Number != 10 {
		t.Fatalf("first assignment = %+v, want #10", msg)
	}
	flaky.mu.Lock()
	flaky.currentTask = nil
	flaky.mu.Unlock()
	// The task_failed handler revokes the lease and books both ledgers.
	hub.revokeLease(identityOf(flaky), msg.TaskID)
	hub.recordTaskFailure("myorg/repo1", 10, false)
	hub.bookReclaimCooldown(identityOf(flaky), "myorg/repo1#10")

	// Age the global failure cooldown past its short window: the issue is
	// admissible again to the fleet.
	hub.completedMu.Lock()
	hub.failedTasks["myorg/repo1#10"] = time.Now().Add(-(failedTaskCooldownMinutes + 1) * time.Minute)
	hub.completedMu.Unlock()
	if hub.isTaskInFailureCooldown("myorg/repo1", 10) {
		t.Fatal("precondition: global failure cooldown should have lapsed")
	}

	msg2 := hub.selectTask(flaky)
	if msg2 == nil {
		t.Fatal("flaky contributor should still be offered B")
	}
	if msg2.Number == 10 {
		t.Fatal("flapping: the contributor that just failed #10 was handed it back after the global cooldown")
	}
	if msg2.Number != 20 {
		t.Fatalf("flaky got #%d, want #20", msg2.Number)
	}
	// flaky keeps its lease on #20, so the only item left for another
	// contributor is #10 — which it gets at once: the window is per
	// contributor, not per issue.
	if hub.inReclaimCooldown(identityOf(other), "myorg/repo1#10") {
		t.Fatal("reclaim cooldown leaked to another contributor")
	}
	msg3 := hub.selectTask(other)
	if msg3 == nil || msg3.Number != 10 {
		t.Fatalf("another contributor should be offered #10 at once, got %+v", msg3)
	}
}

// The window is keyed per (identity, item), reads with an injected clock,
// and honours governor.claims.reclaim_cooldown_s: 0 → 6h default, negative
// → off.
func TestReclaimCooldown_WindowAndConfig(t *testing.T) {
	hub, s := covK2Hub(t)
	base := time.Date(2026, 10, 20, 2, 5, 0, 0, time.UTC)
	hub.bookReclaimCooldownAt("c-a#omp", "o/r#1", base)

	if !hub.inReclaimCooldownAt("c-a#omp", "o/r#1", base.Add(5*time.Hour+59*time.Minute)) {
		t.Fatal("inside the default 6h window")
	}
	if hub.inReclaimCooldownAt("c-a#omp", "o/r#2", base.Add(time.Minute)) {
		t.Fatal("another item is not in cooldown")
	}
	if hub.inReclaimCooldownAt("c-b", "o/r#1", base.Add(time.Minute)) {
		t.Fatal("another contributor is not in cooldown")
	}
	if hub.inReclaimCooldownAt("c-a#omp", "o/r#1", base.Add(6*time.Hour+time.Second)) {
		t.Fatal("window should have lapsed")
	}
	// Lapsed entries are dropped on read.
	hub.completedMu.Lock()
	_, still := hub.reclaimCooldowns[reclaimCooldownKey("c-a#omp", "o/r#1")]
	hub.completedMu.Unlock()
	if still {
		t.Fatal("lapsed entry not dropped")
	}

	if s.deps == nil || s.deps.Config == nil {
		t.Skip("hub has no config to tune")
	}
	s.deps.Config.Governor.Claims.ReclaimCooldownS = 60
	hub.bookReclaimCooldownAt("c-a", "o/r#3", base)
	if !hub.inReclaimCooldownAt("c-a", "o/r#3", base.Add(59*time.Second)) || hub.inReclaimCooldownAt("c-a", "o/r#3", base.Add(61*time.Second)) {
		t.Fatal("configured 60s window not honoured")
	}
	s.deps.Config.Governor.Claims.ReclaimCooldownS = -1
	hub.bookReclaimCooldownAt("c-a", "o/r#4", base)
	if hub.inReclaimCooldownAt("c-a", "o/r#4", base.Add(time.Second)) {
		t.Fatal("negative setting should turn the window off")
	}
	if got := (config.ClaimsConfig{}).EffectiveReclaimCooldown(); got != config.DefaultReclaimCooldown {
		t.Fatalf("default = %v", got)
	}
}
