package dashboard

import (
	"testing"
	"time"
)

// TestCooldownDisabled_NeverGatesQueue verifies the operator kill-switch: with
// contribute cooldown DISABLED, a freshly-completed issue (with a verified PR,
// which would normally earn the full week-long cooldown) is NOT reported as in
// cooldown, so it is never excluded from the queue. Completion history is still
// recorded — this only stops cooldown from gating.
func TestCooldownDisabled_NeverGatesQueue(t *testing.T) {
	hub, s := covK2Hub(t)

	disabled := false
	s.deps.Config.Hub.ContributeCooldownEnabled = &disabled

	hub.markTaskCompleted("myorg/repo1", 10, "https://github.com/myorg/repo1/pull/1")
	if hub.isTaskInCooldown("myorg/repo1", 10) {
		t.Fatalf("with cooldown disabled, a completed task must NOT be in cooldown")
	}

	// The completion history is still recorded even though gating is off.
	hub.completedMu.Lock()
	_, recorded := hub.completedTasks["myorg/repo1#10"]
	hub.completedMu.Unlock()
	if !recorded {
		t.Fatalf("completion history must still be recorded when cooldown is disabled")
	}
}

// TestCooldownEnabled_HonorsConfiguredHours verifies the configured period is
// applied: with a 24h cooldown, a task completed 25h ago is NOT in cooldown while
// one completed 23h ago IS. Uses the per-task recorded cooldown (stamped by
// markTaskCompleted from the configured value).
func TestCooldownEnabled_HonorsConfiguredHours(t *testing.T) {
	hub, s := covK2Hub(t)

	// Enabled + a custom 24h period (well below the 168h default).
	enabled := true
	s.deps.Config.Hub.ContributeCooldownEnabled = &enabled
	const customHours = 24
	s.deps.Config.Hub.ContributeCooldownHours = customHours

	// Sanity: the resolver yields the custom period.
	if got := s.deps.Config.Hub.ContributeCooldownHoursOrDefault(); got != customHours {
		t.Fatalf("resolver = %dh, want %dh", got, customHours)
	}

	// Task A completed 25h ago (> 24h window) → NOT in cooldown.
	hub.markTaskCompleted("myorg/repo1", 20, "https://github.com/myorg/repo1/pull/2")
	hub.completedMu.Lock()
	hub.completedTasks["myorg/repo1#20"] = time.Now().Add(-(customHours + 1) * time.Hour)
	hub.completedMu.Unlock()
	if hub.isTaskInCooldown("myorg/repo1", 20) {
		t.Fatalf("a task completed %dh ago must be past a %dh cooldown", customHours+1, customHours)
	}

	// Task B completed 23h ago (< 24h window) → still in cooldown.
	hub.markTaskCompleted("myorg/repo1", 21, "https://github.com/myorg/repo1/pull/3")
	hub.completedMu.Lock()
	hub.completedTasks["myorg/repo1#21"] = time.Now().Add(-(customHours - 1) * time.Hour)
	hub.completedMu.Unlock()
	if !hub.isTaskInCooldown("myorg/repo1", 21) {
		t.Fatalf("a task completed %dh ago must still be within a %dh cooldown", customHours-1, customHours)
	}
}

// TestCooldownDefault_WhenUnset verifies that with no config override the with-PR
// cooldown falls back to the historical default (completedTaskCooldownHours): a
// task completed just now is in cooldown, and a task aged past the default is not.
func TestCooldownDefault_WhenUnset(t *testing.T) {
	hub, _ := covK2Hub(t)

	// No ContributeCooldownEnabled / Hours set → enabled, default 168h.
	hub.markTaskCompleted("myorg/repo1", 30, "https://github.com/myorg/repo1/pull/4")
	if !hub.isTaskInCooldown("myorg/repo1", 30) {
		t.Fatalf("a just-completed task must be in the default cooldown")
	}

	hub.completedMu.Lock()
	hub.completedTasks["myorg/repo1#30"] = time.Now().Add(-(completedTaskCooldownHours + 1) * time.Hour)
	hub.completedMu.Unlock()
	if hub.isTaskInCooldown("myorg/repo1", 30) {
		t.Fatalf("a task aged past the default %dh cooldown must be free", completedTaskCooldownHours)
	}
}
