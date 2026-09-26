package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/jev"
)

func jevEnv(t *testing.T, mode string) map[string]string {
	t.Helper()
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude", Model: "opus", JevMode: mode}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	out := map[string]string{}
	for _, p := range m.agentEnvPairs(agent) {
		out[p.Key] = p.Value
	}
	return out
}

// TestAgentEnvPairs_JevOnlyWhenAssist: HIVE_JEV_MODE / HIVE_JEV_ENDPOINT are
// exported only for jev_mode: assist — "" and "off" export nothing, so a
// default agent's environment is byte-identical to before the feature.
func TestAgentEnvPairs_JevOnlyWhenAssist(t *testing.T) {
	for _, mode := range []string{"", config.JevModeOff} {
		env := jevEnv(t, mode)
		if _, has := env[jev.ModeEnvVar]; has {
			t.Errorf("jev_mode=%q must not export %s", mode, jev.ModeEnvVar)
		}
		if _, has := env[jev.EndpointEnvVar]; has {
			t.Errorf("jev_mode=%q must not export %s", mode, jev.EndpointEnvVar)
		}
	}
	env := jevEnv(t, config.JevModeAssist)
	if env[jev.ModeEnvVar] != "assist" || env[jev.EndpointEnvVar] != jev.DefaultEndpoint {
		t.Errorf("assist env = %q / %q", env[jev.ModeEnvVar], env[jev.EndpointEnvVar])
	}
	for k, v := range env {
		if strings.Contains(strings.ToUpper(k), "JEV") && strings.Contains(k, "KEY") {
			t.Errorf("no Jev key may be exported to an agent: %s=%q", k, v)
		}
	}
}

func TestJevSkillDir(t *testing.T) {
	cases := map[string]string{
		"claude":  "/home/a/.claude/skills/jev-decide",
		"codex":   "/data/home/.codex-a/skills/jev-decide",
		"copilot": "/home/a/.copilot/skills/jev-decide",
		"gemini":  "/home/a/.gemini/skills/jev-decide",
		"goose":   "/home/a/.config/goose/skills/jev-decide",
		"aider":   "",
	}
	for backend, want := range cases {
		if got := jevSkillDir("/home/a", "/data/home/.codex-a", backend); got != want {
			t.Errorf("%s: %q, want %q", backend, got, want)
		}
	}
}

// TestInstallJevSkill_WritesEmbeddedSkill: the direct (shared-UID) path lays
// down SKILL.md verbatim and is idempotent.
func TestInstallJevSkill_WritesEmbeddedSkill(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".claude", "skills", "jev-decide")
	for i := range 2 {
		if err := installJevSkill(dir, ""); err != nil {
			t.Fatalf("install #%d: %v", i+1, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, jev.SkillFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(jev.SkillMarkdown()) {
		t.Fatal("installed SKILL.md differs from the embedded skill")
	}
}

// TestInstallJevForAgent_OffInstallsNothing: the default agent gets no skill
// file — the caller does not even resolve a home.
func TestInstallJevForAgent_OffInstallsNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	m.installJevForAgent(agent, "claude")
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "jev-decide", jev.SkillFile)); !os.IsNotExist(err) {
		t.Fatalf("jev_mode off must install nothing (stat err=%v)", err)
	}
}

// TestInstallJevForAgent_AssistSharedUIDWritesToHome: with UID 0 (shared dev
// UID) the skill lands under $HOME for the backend's skills dir.
func TestInstallJevForAgent_AssistSharedUIDWritesToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "gemini", JevMode: config.JevModeAssist}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	agent.UID = 0
	m.installJevForAgent(agent, "gemini")
	if _, err := os.Stat(filepath.Join(home, ".gemini", "skills", "jev-decide", jev.SkillFile)); err != nil {
		t.Fatalf("expected skill under HOME: %v", err)
	}
	// Unsupported backend: logged and skipped, nothing written anywhere under HOME.
	m.installJevForAgent(agent, "aider")
	entries, _ := os.ReadDir(home)
	for _, e := range entries {
		if e.Name() != ".gemini" {
			t.Errorf("unsupported backend wrote %s", e.Name())
		}
	}
}

// TestInstallJevForAgent_OffRemovesEarlierSkill: an agent that ran with
// assist and is restarted with jev_mode off loses SKILL.md, so it stops
// advertising a tool the hive now refuses. Only the file the hive wrote is
// removed: a user's extra file inside jev-decide survives (and keeps the
// dir); once the dir is empty it is pruned too.
func TestInstallJevForAgent_OffRemovesEarlierSkill(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude", JevMode: config.JevModeAssist}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	agent.UID = 0
	m.installJevForAgent(agent, "claude")
	skillDir := filepath.Join(home, ".claude", "skills", "jev-decide")
	skill := filepath.Join(skillDir, jev.SkillFile)
	if _, err := os.Stat(skill); err != nil {
		t.Fatalf("assist must install the skill: %v", err)
	}
	extra := filepath.Join(skillDir, "notes.md")
	if err := os.WriteFile(extra, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	agent.Config.JevMode = config.JevModeOff
	m.installJevForAgent(agent, "claude")
	if _, err := os.Lstat(skill); !os.IsNotExist(err) {
		t.Fatalf("jev_mode off must remove SKILL.md (lstat err=%v)", err)
	}
	if data, err := os.ReadFile(extra); err != nil || string(data) != "mine" {
		t.Fatalf("a user's file inside jev-decide must survive: %v %q", err, data)
	}

	// With the user's file gone, the next off launch prunes the empty dir.
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	m.installJevForAgent(agent, "claude")
	if _, err := os.Lstat(skillDir); !os.IsNotExist(err) {
		t.Fatalf("empty jev-decide dir must be pruned (lstat err=%v)", err)
	}
	// And with nothing installed at all, off is a no-op: the parent skills
	// dir (left by the assist install) is neither removed nor repopulated.
	m.installJevForAgent(agent, "claude")
	entries, err := os.ReadDir(filepath.Join(home, ".claude", "skills"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("off with nothing installed must change nothing: err=%v entries=%d", err, len(entries))
	}
}

// TestRemoveJevSkill_AsUser drives the su-exec branch through a stand-in
// su-exec that drops the user spec and execs the rest, so the shell script is
// what runs: only SKILL.md goes, a sibling survives, an empty dir is pruned,
// and a missing file reports nothing removed without an error.
func TestRemoveJevSkill_AsUser(t *testing.T) {
	bin := t.TempDir()
	stub := filepath.Join(bin, "su-exec")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := filepath.Join(t.TempDir(), "jev-decide")
	if err := installJevSkill(dir, ""); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(dir, "notes.md")
	if err := os.WriteFile(extra, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := removeJevSkill(dir, "1001:1001")
	if err != nil || !removed {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, jev.SkillFile)); !os.IsNotExist(err) {
		t.Fatalf("SKILL.md must be gone (lstat err=%v)", err)
	}
	if _, err := os.Stat(extra); err != nil {
		t.Fatalf("sibling must survive: %v", err)
	}
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	removed, err = removeJevSkill(dir, "1001:1001")
	if err != nil || removed {
		t.Fatalf("nothing to remove: removed=%v err=%v", removed, err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("empty dir must be pruned (lstat err=%v)", err)
	}
}
