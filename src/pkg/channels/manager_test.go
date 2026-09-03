package channels

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// ---- NewManager ----

// A nil logger must be replaced rather than panicking on first use.
func TestNewManagerToleratesNilLogger(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, nil)
	if m.logger == nil {
		t.Fatal("a nil logger must be replaced with a default")
	}
	// Exercising a path that logs must not panic.
	m.triggerKick("nobody", TriggerContext{ChannelType: "test"})
}

// All three receivers must be wired, or a channel type silently never fires.
func TestNewManagerWiresAllReceivers(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	if m.webhook == nil {
		t.Error("webhook receiver not wired")
	}
	if m.cron == nil {
		t.Error("cron scheduler not wired")
	}
	if m.bead == nil {
		t.Error("bead watcher not wired")
	}
	if m.WebhookHandler() == nil {
		t.Error("WebhookHandler returned nil")
	}
}

// ---- getAgents ----

// getAgents must hand out a COPY: a caller mutating the result must not corrupt
// the manager's config, and a concurrent UpdateConfig must not race it.
func TestGetAgentsReturnsCopy(t *testing.T) {
	rec := &kickRecorder{}
	agents := map[string]config.AgentConfig{"scanner": {BeadsDir: "/original"}}
	m := NewManager(agents, rec.fn, nil, quietLogger())

	got := m.getAgents()
	got["scanner"] = config.AgentConfig{BeadsDir: "/mutated"}
	delete(got, "scanner")

	again := m.getAgents()
	if len(again) != 1 {
		t.Fatalf("mutating the returned map changed the manager's config: %v", again)
	}
	if again["scanner"].BeadsDir != "/original" {
		t.Fatalf("agent config was corrupted: %q", again["scanner"].BeadsDir)
	}
}

// ---- triggerKick ----

// A failing kick must be logged and swallowed, never propagated as a panic —
// one wedged agent must not take the channel receiver down.
func TestTriggerKickSurvivesKickFailure(t *testing.T) {
	rec := &kickRecorder{err: errors.New("agent is busy")}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())

	m.triggerKick("scanner", TriggerContext{ChannelType: "webhook", Event: "issues.opened"})

	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("the kick must still have been attempted, got %v", got)
	}
}

// With no buildMsg the kick carries an empty message rather than panicking on a
// nil func.
func TestTriggerKickWithoutBuildMsg(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())

	m.triggerKick("scanner", TriggerContext{ChannelType: "schedule"})

	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("want 1 kick, got %v", got)
	}
	if got[0].message != "" {
		t.Fatalf("want an empty message with no buildMsg, got %q", got[0].message)
	}
}

// ---- UpdateConfig ----

// A hot reload must be visible to subsequent triggers, which is the whole point
// of reloading without a restart.
func TestUpdateConfigIsVisibleToReceivers(t *testing.T) {
	t.Setenv(webhookSecretEnvVar, "s3cr3t")
	rec := &kickRecorder{}
	m := NewManager(webhookAgents([]string{"issues"}, nil, nil), rec.fn, nil, quietLogger())

	// Before: scanner subscribes to issues.
	if w := postWebhook(t, m.WebhookHandler(), "issues", map[string]any{"action": "opened"}, "s3cr3t"); w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if got := rec.waitForKicks(t, 1); len(got) != 1 {
		t.Fatalf("want 1 kick before reload, got %v", got)
	}

	// After: the same agent now subscribes to push instead.
	m.UpdateConfig(map[string]config.AgentConfig{
		"scanner": {Channels: []config.ChannelConfig{{Type: "webhook", Events: []string{"push"}}}},
	})

	before := len(rec.snapshot())
	if w := postWebhook(t, m.WebhookHandler(), "issues", map[string]any{"action": "opened"}, "s3cr3t"); w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	time.Sleep(50 * time.Millisecond)
	if after := len(rec.snapshot()); after != before {
		t.Fatalf("the old subscription still fired after reload: %d -> %d", before, after)
	}

	if w := postWebhook(t, m.WebhookHandler(), "push", map[string]any{}, "s3cr3t"); w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	if got := rec.waitForKicks(t, before+1); len(got) != before+1 {
		t.Fatalf("the new subscription did not fire, got %v", got)
	}
}

// ---- Start / Stop lifecycle ----

// Start then Stop must be clean, and Stop must be safe to call twice — the
// manager is stopped both on context cancel and on explicit shutdown.
func TestManagerStartStopIsIdempotent(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Channels: []config.ChannelConfig{{Type: "schedule", Schedule: "0 0 * * *"}}},
	}, rec.fn, nil, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	m.Stop()
	m.Stop() // must not panic on a second Stop
	cancel()
}

// Cancelling the context must stop the receivers without a panic.
func TestManagerStopsOnContextCancel(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	cancel()
	time.Sleep(50 * time.Millisecond) // let the watcher goroutines observe it
	m.Stop()
}

// ---- CronScheduler ----

// A valid cron expression registers; an invalid one is logged and skipped
// rather than taking down the whole scheduler with it.
func TestCronSchedulerSkipsInvalidExpression(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{
		"good": {Channels: []config.ChannelConfig{{Type: "schedule", Schedule: "*/5 * * * *"}}},
		"bad":  {Channels: []config.ChannelConfig{{Type: "schedule", Schedule: "not a cron expression"}}},
	}, rec.fn, nil, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cron.Start(ctx) // must not panic on the invalid entry
	defer m.cron.Stop()

	m.cron.mu.Lock()
	entries := len(m.cron.runner.Entries())
	m.cron.mu.Unlock()
	if entries != 1 {
		t.Fatalf("want only the valid schedule registered, got %d entries", entries)
	}
}

// A disabled schedule channel must not be registered at all.
func TestCronSchedulerSkipsDisabledChannel(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Channels: []config.ChannelConfig{{
			Type: "schedule", Schedule: "*/5 * * * *", Enabled: boolPtr(false),
		}}},
	}, rec.fn, nil, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cron.Start(ctx)
	defer m.cron.Stop()

	m.cron.mu.Lock()
	entries := len(m.cron.runner.Entries())
	m.cron.mu.Unlock()
	if entries != 0 {
		t.Fatalf("a disabled schedule must not register, got %d entries", entries)
	}
}

// Rebuild must replace the job set, not append to it — otherwise a hot reload
// leaves the old schedule firing alongside the new one.
func TestCronSchedulerRebuildReplacesJobs(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{
		"a": {Channels: []config.ChannelConfig{{Type: "schedule", Schedule: "*/5 * * * *"}}},
		"b": {Channels: []config.ChannelConfig{{Type: "schedule", Schedule: "*/10 * * * *"}}},
	}, rec.fn, nil, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cron.Start(ctx)
	defer m.cron.Stop()

	m.cron.mu.Lock()
	before := len(m.cron.runner.Entries())
	m.cron.mu.Unlock()
	if before != 2 {
		t.Fatalf("want 2 jobs initially, got %d", before)
	}

	m.cron.Rebuild(map[string]config.AgentConfig{
		"a": {Channels: []config.ChannelConfig{{Type: "schedule", Schedule: "*/5 * * * *"}}},
	})

	m.cron.mu.Lock()
	after := len(m.cron.runner.Entries())
	m.cron.mu.Unlock()
	if after != 1 {
		t.Fatalf("Rebuild must REPLACE the job set, got %d jobs", after)
	}
}

// Stop before Start must not panic.
func TestCronSchedulerStopBeforeStart(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	m.cron.Stop()
}

// ---- BeadWatcher ----

func writeBead(t *testing.T, dir, name string, metadata map[string]any) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A bead whose metadata satisfies every match key triggers its agent.
func TestBeadWatcherTriggersOnMatchingBead(t *testing.T) {
	dir := t.TempDir()
	writeBead(t, dir, "bead-1.json", map[string]any{"status": "ready", "kind": "bug"})

	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {
			BeadsDir: dir,
			Channels: []config.ChannelConfig{{
				Type:  "bead",
				Match: map[string]string{"status": "ready"},
			}},
		},
	}, rec.fn, nil, quietLogger())

	m.bead.Rebuild(m.getAgents())
	m.bead.check()

	got := rec.waitForKicks(t, 1)
	if len(got) != 1 || got[0].agent != "scanner" {
		t.Fatalf("want scanner kicked for the matching bead, got %v", got)
	}
}

// A bead is triggered ONCE: re-firing on every 30s poll would kick the agent
// forever for the same work item.
func TestBeadWatcherDeduplicatesSeenBeads(t *testing.T) {
	dir := t.TempDir()
	writeBead(t, dir, "bead-1.json", map[string]any{"status": "ready"})

	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {
			BeadsDir: dir,
			Channels: []config.ChannelConfig{{Type: "bead", Match: map[string]string{"status": "ready"}}},
		},
	}, rec.fn, nil, quietLogger())

	m.bead.Rebuild(m.getAgents())
	m.bead.check()
	if got := rec.waitForKicks(t, 1); len(got) != 1 {
		t.Fatalf("want 1 kick on the first poll, got %v", got)
	}

	m.bead.check()
	m.bead.check()
	time.Sleep(50 * time.Millisecond)
	if got := rec.snapshot(); len(got) != 1 {
		t.Fatalf("a bead must trigger only once, got %d kicks", len(got))
	}
}

// A bead that fails any match key must not trigger.
func TestBeadWatcherIgnoresNonMatchingBead(t *testing.T) {
	dir := t.TempDir()
	writeBead(t, dir, "bead-1.json", map[string]any{"status": "blocked"})

	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {
			BeadsDir: dir,
			Channels: []config.ChannelConfig{{Type: "bead", Match: map[string]string{"status": "ready"}}},
		},
	}, rec.fn, nil, quietLogger())

	m.bead.Rebuild(m.getAgents())
	m.bead.check()
	time.Sleep(50 * time.Millisecond)
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("a non-matching bead must not trigger, got %v", got)
	}
}

// matchesBead must fail closed on unreadable, malformed, or missing-key input
// rather than treating "cannot tell" as a match.
func TestMatchesBeadFailsClosed(t *testing.T) {
	dir := t.TempDir()
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	b := m.bead

	t.Run("missing file", func(t *testing.T) {
		if b.matchesBead(filepath.Join(dir, "absent.json"), map[string]string{"a": "b"}) {
			t.Fatal("a missing bead must not match")
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		p := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		if b.matchesBead(p, map[string]string{"a": "b"}) {
			t.Fatal("a malformed bead must not match")
		}
	})

	t.Run("missing key", func(t *testing.T) {
		writeBead(t, dir, "nokey.json", map[string]any{"other": "value"})
		if b.matchesBead(filepath.Join(dir, "nokey.json"), map[string]string{"status": "ready"}) {
			t.Fatal("a bead missing the match key must not match")
		}
	})

	t.Run("non-string value", func(t *testing.T) {
		writeBead(t, dir, "numeric.json", map[string]any{"status": 42})
		if b.matchesBead(filepath.Join(dir, "numeric.json"), map[string]string{"status": "42"}) {
			t.Fatal("a non-string metadata value must not match a string criterion")
		}
	})

	t.Run("empty match matches any valid bead", func(t *testing.T) {
		writeBead(t, dir, "any.json", map[string]any{"status": "ready"})
		if !b.matchesBead(filepath.Join(dir, "any.json"), nil) {
			t.Fatal("an empty match set must accept any readable bead")
		}
	})
}

// An agent with no beads dir configured must be skipped silently.
func TestBeadWatcherSkipsAgentWithoutBeadsDir(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Channels: []config.ChannelConfig{{Type: "bead", Match: map[string]string{"a": "b"}}}},
	}, rec.fn, nil, quietLogger())

	m.bead.Rebuild(m.getAgents())
	m.bead.check() // must not panic
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("no kicks expected, got %v", got)
	}
}

// An unreadable beads directory must be tolerated, not fatal.
func TestBeadWatcherToleratesMissingBeadsDir(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {
			BeadsDir: filepath.Join(t.TempDir(), "does-not-exist"),
			Channels: []config.ChannelConfig{{Type: "bead", Match: map[string]string{"a": "b"}}},
		},
	}, rec.fn, nil, quietLogger())

	m.bead.Rebuild(m.getAgents())
	m.bead.check()
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("no kicks expected, got %v", got)
	}
}

// Subdirectories inside the beads dir are not beads.
func TestBeadWatcherIgnoresSubdirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {
			BeadsDir: dir,
			Channels: []config.ChannelConfig{{Type: "bead"}},
		},
	}, rec.fn, nil, quietLogger())

	m.bead.Rebuild(m.getAgents())
	m.bead.check()
	time.Sleep(50 * time.Millisecond)
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("a subdirectory must not be treated as a bead, got %v", got)
	}
}

// A disabled bead channel must not fire.
func TestBeadWatcherSkipsDisabledChannel(t *testing.T) {
	dir := t.TempDir()
	writeBead(t, dir, "bead-1.json", map[string]any{"status": "ready"})

	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {
			BeadsDir: dir,
			Channels: []config.ChannelConfig{{
				Type: "bead", Match: map[string]string{"status": "ready"}, Enabled: boolPtr(false),
			}},
		},
	}, rec.fn, nil, quietLogger())

	m.bead.Rebuild(m.getAgents())
	m.bead.check()
	time.Sleep(50 * time.Millisecond)
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("a disabled bead channel must not trigger, got %v", got)
	}
}

// Stop before Start must not panic.
func TestBeadWatcherStopBeforeStart(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	m.bead.Stop()
}
