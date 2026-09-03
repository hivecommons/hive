package channels

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// ---- BeadWatcher ----

func TestBeadWatcher_NewReturnsNonNil(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	bw := NewBeadWatcher(m)
	if bw == nil {
		t.Fatal("NewBeadWatcher returned nil")
	}
	if bw.seen == nil {
		t.Fatal("seen map not initialized")
	}
}

func TestBeadWatcher_StartAndStop(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	bw := NewBeadWatcher(m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bw.Start(ctx)
	// Stop should be idempotent and not panic.
	bw.Stop()
	bw.Stop()
}

func TestBeadWatcher_StopBeforeStart(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	bw := NewBeadWatcher(m)
	// Stop before Start must not panic.
	bw.Stop()
}

func TestBeadWatcher_MatchesBead(t *testing.T) {
	dir := t.TempDir()

	// Write a bead file with metadata.
	bead := map[string]interface{}{
		"type":     "advisory",
		"priority": "1",
		"actor":    "quality",
	}
	data, _ := json.Marshal(bead)
	beadPath := filepath.Join(dir, "bead-001.json")
	if err := os.WriteFile(beadPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	bw := NewBeadWatcher(m)

	// Matching criteria.
	if !bw.matchesBead(beadPath, map[string]string{"type": "advisory"}) {
		t.Error("expected match on type=advisory")
	}
	if !bw.matchesBead(beadPath, map[string]string{"type": "advisory", "actor": "quality"}) {
		t.Error("expected match on type+actor")
	}
	// Non-matching.
	if bw.matchesBead(beadPath, map[string]string{"type": "alert"}) {
		t.Error("expected no match on type=alert")
	}
	// Missing key.
	if bw.matchesBead(beadPath, map[string]string{"missing_key": "x"}) {
		t.Error("expected no match on missing key")
	}
	// Empty match matches everything.
	if !bw.matchesBead(beadPath, map[string]string{}) {
		t.Error("empty match should match all beads")
	}
}

func TestBeadWatcher_MatchesBead_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	badPath := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badPath, []byte("not json"), 0644); err != nil {
		t.Fatal(err)
	}

	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	bw := NewBeadWatcher(m)

	if bw.matchesBead(badPath, map[string]string{"type": "x"}) {
		t.Error("invalid JSON should not match")
	}
}

func TestBeadWatcher_MatchesBead_NonexistentFile(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	bw := NewBeadWatcher(m)

	if bw.matchesBead("/nonexistent/bead.json", map[string]string{"type": "x"}) {
		t.Error("nonexistent file should not match")
	}
}

func TestBeadWatcher_MatchesBead_NonStringValue(t *testing.T) {
	dir := t.TempDir()
	bead := map[string]interface{}{
		"type":  "advisory",
		"count": 42, // numeric, not string
	}
	data, _ := json.Marshal(bead)
	beadPath := filepath.Join(dir, "bead.json")
	if err := os.WriteFile(beadPath, data, 0644); err != nil {
		t.Fatal(err)
	}

	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	bw := NewBeadWatcher(m)

	// Matching string field works.
	if !bw.matchesBead(beadPath, map[string]string{"type": "advisory"}) {
		t.Error("expected match on string field")
	}
	// Non-string field should not match.
	if bw.matchesBead(beadPath, map[string]string{"count": "42"}) {
		t.Error("non-string value should not match string comparison")
	}
}

func TestBeadWatcher_CheckTriggersKick(t *testing.T) {
	dir := t.TempDir()

	// Write a matching bead.
	bead := map[string]interface{}{"type": "advisory", "actor": "quality"}
	data, _ := json.Marshal(bead)
	if err := os.WriteFile(filepath.Join(dir, "bead-trigger.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	agents := map[string]config.AgentConfig{
		"strategist": {
			BeadsDir: dir,
			Channels: []config.ChannelConfig{{
				Type:  "bead",
				Match: map[string]string{"type": "advisory"},
			}},
		},
	}

	rec := &kickRecorder{}
	m := NewManager(agents, rec.fn, nil, quietLogger())
	bw := m.bead
	bw.agents = agents

	// Directly call check() to trigger bead processing.
	bw.check()

	// Wait for async kick goroutine.
	kicks := rec.waitForKicks(t, 1)
	if len(kicks) == 0 {
		t.Fatal("expected a kick to be triggered")
	}
	if kicks[0].agent != "strategist" {
		t.Errorf("expected kick to strategist, got %q", kicks[0].agent)
	}
}

func TestBeadWatcher_DeduplicatesBeads(t *testing.T) {
	dir := t.TempDir()

	bead := map[string]interface{}{"type": "advisory"}
	data, _ := json.Marshal(bead)
	if err := os.WriteFile(filepath.Join(dir, "bead-dup.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	agents := map[string]config.AgentConfig{
		"scanner": {
			BeadsDir: dir,
			Channels: []config.ChannelConfig{{
				Type:  "bead",
				Match: map[string]string{"type": "advisory"},
			}},
		},
	}

	rec := &kickRecorder{}
	m := NewManager(agents, rec.fn, nil, quietLogger())
	bw := m.bead
	bw.agents = agents

	// Check twice — second should NOT re-trigger.
	bw.check()
	bw.check()

	kicks := rec.waitForKicks(t, 2)
	if len(kicks) != 1 {
		t.Errorf("expected exactly 1 kick (dedup), got %d", len(kicks))
	}
}

func TestBeadWatcher_SkipsDisabledChannel(t *testing.T) {
	dir := t.TempDir()

	bead := map[string]interface{}{"type": "advisory"}
	data, _ := json.Marshal(bead)
	if err := os.WriteFile(filepath.Join(dir, "bead.json"), data, 0644); err != nil {
		t.Fatal(err)
	}

	disabled := false
	agents := map[string]config.AgentConfig{
		"scanner": {
			BeadsDir: dir,
			Channels: []config.ChannelConfig{{
				Type:    "bead",
				Enabled: &disabled,
				Match:   map[string]string{"type": "advisory"},
			}},
		},
	}

	rec := &kickRecorder{}
	m := NewManager(agents, rec.fn, nil, quietLogger())
	bw := m.bead
	bw.agents = agents

	bw.check()

	if len(rec.snapshot()) != 0 {
		t.Error("disabled channel should not trigger kicks")
	}
}

func TestBeadWatcher_SkipsEmptyBeadsDir(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"scanner": {
			BeadsDir: "", // empty
			Channels: []config.ChannelConfig{{
				Type:  "bead",
				Match: map[string]string{"type": "advisory"},
			}},
		},
	}

	rec := &kickRecorder{}
	m := NewManager(agents, rec.fn, nil, quietLogger())
	bw := m.bead
	bw.agents = agents

	// Should not panic.
	bw.check()

	if len(rec.snapshot()) != 0 {
		t.Error("empty beads_dir should not trigger kicks")
	}
}

func TestBeadWatcher_SkipsDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "subdir"), 0755); err != nil {
		t.Fatal(err)
	}

	agents := map[string]config.AgentConfig{
		"scanner": {
			BeadsDir: dir,
			Channels: []config.ChannelConfig{{
				Type:  "bead",
				Match: map[string]string{},
			}},
		},
	}

	rec := &kickRecorder{}
	m := NewManager(agents, rec.fn, nil, quietLogger())
	bw := m.bead
	bw.agents = agents

	bw.check()

	if len(rec.snapshot()) != 0 {
		t.Error("directories should be skipped")
	}
}

func TestBeadWatcher_Rebuild(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	bw := m.bead

	newAgents := map[string]config.AgentConfig{
		"new-agent": {
			BeadsDir: t.TempDir(),
			Channels: []config.ChannelConfig{{Type: "bead"}},
		},
	}
	bw.Rebuild(newAgents)

	bw.mu.Lock()
	if _, ok := bw.agents["new-agent"]; !ok {
		t.Error("Rebuild did not update agents")
	}
	bw.mu.Unlock()
}

// ---- CronScheduler ----

func TestCronScheduler_NewReturnsNonNil(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	cs := NewCronScheduler(m)
	if cs == nil {
		t.Fatal("NewCronScheduler returned nil")
	}
}

func TestCronScheduler_StartAndStop(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"nightly": {
			Channels: []config.ChannelConfig{{
				Type:     "schedule",
				Schedule: "0 0 * * *", // daily at midnight
			}},
		},
	}

	rec := &kickRecorder{}
	m := NewManager(agents, rec.fn, nil, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	m.cron.Start(ctx)
	// Stop via context cancellation.
	cancel()
	// Double stop should not panic.
	m.cron.Stop()
}

func TestCronScheduler_StopBeforeStart(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())
	// Stop before Start must not panic.
	m.cron.Stop()
}

func TestCronScheduler_FiresKick(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"fast-agent": {
			Channels: []config.ChannelConfig{{
				Type:     "schedule",
				Schedule: "@every 1s",
			}},
		},
	}

	rec := &kickRecorder{}
	m := NewManager(agents, rec.fn, nil, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cron.Start(ctx)

	// Wait for at least one fire.
	kicks := rec.waitForKicks(t, 1)
	if len(kicks) == 0 {
		t.Fatal("cron job did not fire within deadline")
	}
	if kicks[0].agent != "fast-agent" {
		t.Errorf("expected kick to fast-agent, got %q", kicks[0].agent)
	}
}

func TestCronScheduler_SkipsDisabledChannel(t *testing.T) {
	disabled := false
	agents := map[string]config.AgentConfig{
		"disabled-agent": {
			Channels: []config.ChannelConfig{{
				Type:     "schedule",
				Enabled:  &disabled,
				Schedule: "@every 1s",
			}},
		},
	}

	rec := &kickRecorder{}
	m := NewManager(agents, rec.fn, nil, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cron.Start(ctx)

	if entries := m.cron.runner.Entries(); len(entries) != 0 {
		t.Fatalf("disabled schedule channel registered %d jobs, want 0", len(entries))
	}
	if len(rec.snapshot()) != 0 {
		t.Error("disabled schedule channel should not fire")
	}
}

func TestCronScheduler_InvalidScheduleDoesNotPanic(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"bad-schedule": {
			Channels: []config.ChannelConfig{{
				Type:     "schedule",
				Schedule: "not a valid cron expression",
			}},
		},
	}

	rec := &kickRecorder{}
	m := NewManager(agents, rec.fn, nil, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Should not panic, just log a warning.
	m.cron.Start(ctx)
	if entries := m.cron.runner.Entries(); len(entries) != 0 {
		t.Fatalf("invalid schedule registered %d jobs, want 0", len(entries))
	}
	m.cron.Stop()
}

func TestCronScheduler_Rebuild(t *testing.T) {
	rec := &kickRecorder{}
	m := NewManager(map[string]config.AgentConfig{}, rec.fn, nil, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cron.Start(ctx)

	// Rebuild with a fast schedule.
	newAgents := map[string]config.AgentConfig{
		"rebuilt-agent": {
			Channels: []config.ChannelConfig{{
				Type:     "schedule",
				Schedule: "@every 1s",
			}},
		},
	}
	m.cron.Rebuild(newAgents)

	kicks := rec.waitForKicks(t, 1)
	if len(kicks) == 0 {
		t.Fatal("rebuilt cron job did not fire")
	}
	if kicks[0].agent != "rebuilt-agent" {
		t.Errorf("expected kick to rebuilt-agent, got %q", kicks[0].agent)
	}
}

func TestCronScheduler_BuildMsgUsed(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"msg-agent": {
			Channels: []config.ChannelConfig{{
				Type:     "schedule",
				Schedule: "@every 1s",
			}},
		},
	}

	buildMsg := func(agent string, ctx TriggerContext) string {
		return "custom kick message for " + agent
	}

	rec := &kickRecorder{}
	m := NewManager(agents, rec.fn, buildMsg, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.cron.Start(ctx)

	kicks := rec.waitForKicks(t, 1)
	if len(kicks) == 0 {
		t.Fatal("cron job did not fire")
	}
	expected := "custom kick message for msg-agent"
	if kicks[0].message != expected {
		t.Errorf("message = %q, want %q", kicks[0].message, expected)
	}
}
