package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestAddAgent(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	m.AddAgent("new-agent", config.AgentConfig{Backend: "claude", Model: "sonnet"})

	status, err := m.GetStatus("new-agent")
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if status.State != StateStopped {
		t.Errorf("state = %q", status.State)
	}
	if status.Config.Backend != "claude" {
		t.Errorf("backend = %q", status.Config.Backend)
	}
}

func TestAddAgent_Duplicate(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	// Should not overwrite
	m.AddAgent("scanner", config.AgentConfig{Backend: "gemini"})
	status, _ := m.GetStatus("scanner")
	if status.Config.Backend != "claude" {
		t.Errorf("backend = %q, want claude (should not overwrite)", status.Config.Backend)
	}
}

func TestRemoveAgent(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.RemoveAgent("scanner")
	_, err := m.GetStatus("scanner")
	if err == nil {
		t.Error("expected error for removed agent")
	}
}

func TestRemoveAgent_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	// Should not panic
	m.RemoveAgent("nonexistent")
}

func TestResetRestartCount(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.mu.Lock()
	m.agents["scanner"].RestartCount = 5
	m.mu.Unlock()

	err := m.ResetRestartCount("scanner")
	if err != nil {
		t.Fatalf("ResetRestartCount: %v", err)
	}
	status, _ := m.GetStatus("scanner")
	if status.RestartCount != 0 {
		t.Errorf("restart count = %d", status.RestartCount)
	}
}

func TestResetRestartCount_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.ResetRestartCount("nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

func TestUnpinCLI(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.PinCLI("scanner", "claude-v2")
	err := m.UnpinCLI("scanner")
	if err != nil {
		t.Fatalf("UnpinCLI: %v", err)
	}
	status, _ := m.GetStatus("scanner")
	if status.PinnedCLI != "" {
		t.Errorf("pinned CLI = %q", status.PinnedCLI)
	}
}

func TestUnpinCLI_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.UnpinCLI("nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

func TestUnpinModel(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	m.PinModel("scanner", "opus")
	err := m.UnpinModel("scanner")
	if err != nil {
		t.Fatalf("UnpinModel: %v", err)
	}
	status, _ := m.GetStatus("scanner")
	if status.PinnedModel != "" {
		t.Errorf("pinned model = %q", status.PinnedModel)
	}
}

func TestUnpinModel_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	err := m.UnpinModel("nonexistent")
	if err == nil {
		t.Error("expected error")
	}
}

func TestGetOutput_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	_, err := m.GetOutput("nonexistent", 10)
	if err == nil {
		t.Error("expected error")
	}
}

func TestGetOutput_WithBuffer(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	// Write some output to the buffer
	m.mu.Lock()
	m.agents["scanner"].OutputBuffer.Write("line 1")
	m.agents["scanner"].OutputBuffer.Write("line 2")
	m.agents["scanner"].OutputBuffer.Write("line 3")
	m.mu.Unlock()

	output, err := m.GetOutput("scanner", 2)
	if err != nil {
		t.Fatalf("GetOutput: %v", err)
	}
	if len(output) != 2 {
		t.Errorf("output lines = %d, want 2", len(output))
	}
}

func TestIsPaused(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	if m.IsPaused("scanner") {
		t.Error("scanner should not be paused initially")
	}

	m.Pause("scanner", "test", "test pause")
	if !m.IsPaused("scanner") {
		t.Error("scanner should be paused")
	}
}

func TestIsPaused_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	if m.IsPaused("nonexistent") {
		t.Error("nonexistent agent should return false")
	}
}

func TestTmuxSession(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})

	session := m.TmuxSession("scanner")
	if session != "hive-scanner" {
		t.Errorf("session = %q, want hive-scanner", session)
	}
}

func TestTmuxSession_NotFound(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	session := m.TmuxSession("nonexistent")
	if session != "" {
		t.Errorf("session = %q, want empty", session)
	}
}

func TestBuildEnvPrefix(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	m.mu.RLock()
	agent := m.agents["scanner"]
	m.mu.RUnlock()

	prefix := m.buildEnvPrefix(agent)
	if prefix == "" {
		t.Error("expected non-empty env prefix")
	}
}

func TestBuildEnvPrefix_EmptyVars(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})

	agent := &AgentProcess{Name: "test"}
	prefix := m.buildEnvPrefix(agent)
	// agentEnvPairs returns at least HIVE_AGENT, so prefix should be non-empty
	if prefix == "" {
		t.Error("expected env prefix to at least include HIVE_AGENT")
	}
}

func TestSetModelOverride_Pinned(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	// A pin blocks governor auto-selection, not a user's explicit switch:
	// the switch succeeds and the pin retargets to the new model.
	m.PinModel("scanner", "opus")
	if err := m.SetModelOverride("scanner", "haiku"); err != nil {
		t.Errorf("user switch on pinned model should succeed, got %v", err)
	}
	st := m.AllStatuses()["scanner"]
	if st.ModelOverride != "haiku" {
		t.Errorf("ModelOverride = %q, want haiku", st.ModelOverride)
	}
	if st.PinnedModel != "haiku" {
		t.Errorf("PinnedModel = %q, want haiku (pin retargeted, still pinned)", st.PinnedModel)
	}
}

func TestNormalizeLine(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "hello world", "hello world", true},
		{"spinner change", "◐ Reading files", "◎ Reading files", true},
		{"credits change", "AI Credits: 42.3", "AI Credits: 99.1", true},
		{"trailing space", "hello   ", "hello", true},
		{"different content", "line A", "line B", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeLine(tt.a) == normalizeLine(tt.b)
			if got != tt.want {
				t.Errorf("normalizeLine(%q) == normalizeLine(%q) = %v, want %v",
					tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestFindOverlap_SpinnerTolerant(t *testing.T) {
	prev := []string{
		"● Read CLAUDE.md",
		"● Read base.md",
		"/data/agents/scanner                                          AI Credits: 4.02",
		"◐ Analyzing coverage                                        Claude Sonnet 4.6",
	}
	curr := []string{
		"● Read CLAUDE.md",
		"● Read base.md",
		"/data/agents/scanner                                          AI Credits: 4.05",
		"◎ Analyzing coverage                                        Claude Sonnet 4.6",
	}
	overlap := findOverlap(prev, curr)
	if overlap != 4 {
		t.Errorf("findOverlap with spinner/credits change = %d, want 4", overlap)
	}
}

func TestFindOverlap_ExactMatch(t *testing.T) {
	prev := []string{"a", "b", "c"}
	curr := []string{"b", "c", "d"}
	overlap := findOverlap(prev, curr)
	if overlap != 2 {
		t.Errorf("findOverlap exact = %d, want 2", overlap)
	}
}

func TestDiffNewLines_SpinnerChange(t *testing.T) {
	prev := []string{
		"static line 1",
		"static line 2",
		"◐ Working                                                      AI Credits: 10",
	}
	curr := []string{
		"static line 1",
		"static line 2",
		"◎ Working                                                      AI Credits: 11",
	}
	newLines := diffNewLines(prev, curr)
	if len(newLines) != 0 {
		t.Errorf("diffNewLines should return 0 new lines when only spinner/credits changed, got %d: %v",
			len(newLines), newLines)
	}
}

func TestDeduplicateBlocks_NearDuplicates(t *testing.T) {
	lines := []string{
		"● Read base.md",
		"/data/agents/scanner                                          AI Credits: 4.02",
		"◐ Analyzing",
		"● Read base.md",
		"/data/agents/scanner                                          AI Credits: 4.05",
		"◎ Analyzing",
	}
	result := DeduplicateBlocks(lines)
	if len(result) > 3 {
		t.Errorf("DeduplicateBlocks should remove near-duplicate block, got %d lines: %v",
			len(result), result)
	}
}

// Audit F12 (CWE-59/CWE-61). ensureWorldWritable repairs an agent's HOME, a
// tree that is world-writable BY DESIGN — so the agent itself can plant entries
// in it. os.Chmod follows symlinks, so a link planted there aimed the hive's
// own chmod at any file the hive user could reach and made it 0666. The agent
// supplies the link; the hive supplies the privilege.
//
// Config, key files and state all live within reach of that user, so this is a
// privilege-escalation primitive, not a tidiness issue.
func TestF12_EnsureWorldWritableDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}

	victim := filepath.Join(root, "victim.txt")
	if err := os.WriteFile(victim, []byte("hive-owned secret"), 0o600); err != nil {
		t.Fatalf("write victim: %v", err)
	}
	// The agent plants a link inside its own HOME pointing outside it.
	if err := os.Symlink(victim, filepath.Join(home, "innocent.json")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// A real file in the tree, to prove the walk still does its job.
	realFile := filepath.Join(home, "settings.json")
	if err := os.WriteFile(realFile, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write real file: %v", err)
	}

	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	m.ensureWorldWritable(home)

	info, err := os.Lstat(victim)
	if err != nil {
		t.Fatalf("lstat victim: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("F12: a symlink planted in the agent HOME made the hive chmod a file OUTSIDE that tree to %o (want 0600 untouched)", info.Mode().Perm())
	}

	// Positive control: if the walk did nothing at all, the assertion above
	// would hold for the wrong reason.
	rinfo, err := os.Stat(realFile)
	if err != nil {
		t.Fatalf("stat real file: %v", err)
	}
	if rinfo.Mode().Perm() != 0o666 {
		t.Fatalf("test is vacuous: the walk did not repair a genuine file (mode %o, want 0666)", rinfo.Mode().Perm())
	}
}

// Audit F12 (CWE-59/61), the WRITE half.
//
// PR #3432 closed only one of the three sinks the audit named: the chmod walk
// in ensureWorldWritable, which WIDENED a symlinked file's mode. The two
// writers — seedJSONFile and mergeApprovedAPIKeys — kept using a plain
// os.WriteFile, which follows symlinks, so an attacker who could plant a link
// at the predictable inference-home path had the hive overwrite the CONTENTS of
// any file it could reach.
//
// The inference HOME is /tmp/.claude-inference-home-<agent>: a guessable path
// inside world-writable /tmp, in a directory deliberately made world-writable
// so the agent UID can use it. Planting the link needs no privilege at all.
func TestF12_SeedJSONFileDoesNotFollowSymlink(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	const victimContent = "hive-owned secret, must survive"
	victim := filepath.Join(root, "victim.json")
	if err := os.WriteFile(victim, []byte(victimContent), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	// The attacker plants a link where the hive is about to write.
	target := filepath.Join(home, ".claude.json")
	if err := os.Symlink(victim, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	m.seedJSONFile("victim-agent", target, map[string]any{"hasCompletedOnboarding": true})

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != victimContent {
		t.Fatalf("F12: the hive wrote THROUGH a planted symlink and clobbered a file outside the agent home.\n victim now: %s", got)
	}
}

// mergeApprovedAPIKeys is the second writer named by the finding, reached via
// seedClaudeUserConfig. It reads the file first, so it is also a read-and-echo
// primitive: without the guard it slurps a hive-readable file and writes the
// merged result back.
func TestF12_MergeApprovedAPIKeysDoesNotFollowSymlink(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Valid JSON with the shape mergeApprovedAPIKeys mutates, so the function
	// reaches its write path rather than bailing out early.
	const victimContent = `{"customApiKeyResponses":{"approved":[]}}`
	victim := filepath.Join(root, "victim.json")
	if err := os.WriteFile(victim, []byte(victimContent), 0o600); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	target := filepath.Join(home, ".claude.json")
	if err := os.Symlink(victim, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	m.mergeApprovedAPIKeys("victim-agent", target)

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read victim: %v", err)
	}
	if string(got) != victimContent {
		t.Fatalf("F12: mergeApprovedAPIKeys wrote through a planted symlink.\n victim now: %s", got)
	}
}

// POSITIVE CONTROL. The guards must not break the feature: a real (non-symlink)
// config file still has to be seeded and merged, or both tests above would pass
// against a build that simply never writes anything.
func TestF12_RealInferenceConfigStillSeeded(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")

	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	m.seedJSONFile("real-agent", path, map[string]any{"hasCompletedOnboarding": true})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("test is vacuous: the seed never wrote a real file: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("seeded file is not valid JSON: %v", err)
	}
	if parsed["hasCompletedOnboarding"] != true {
		t.Fatalf("seeded key missing — the guard broke normal seeding: %v", parsed)
	}

	// And a second pass must merge into the existing file, not fail on it.
	m.seedJSONFile("real-agent", path, map[string]any{"anotherKey": "v"})
	data2, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	var parsed2 map[string]any
	if err := json.Unmarshal(data2, &parsed2); err != nil {
		t.Fatalf("merged file is not valid JSON: %v", err)
	}
	if parsed2["anotherKey"] != "v" || parsed2["hasCompletedOnboarding"] != true {
		t.Fatalf("merge lost keys: %v", parsed2)
	}
}
