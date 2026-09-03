package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// withSharedAgentHome points the interactive-home machinery at a temp dir for
// the duration of one test, mirroring the inferenceHomePrefixOverride seam.
func withSharedAgentHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := sharedAgentHome
	sharedAgentHome = dir
	t.Cleanup(func() { sharedAgentHome = old })
	return dir
}

func interactiveHomeTestManager(t *testing.T) *Manager {
	t.Helper()
	return NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})
}

const signedInClaudeSession = `{"oauthAccount":{"accountUuid":"u-1","emailAddress":"op@example.com"},"hasCompletedOnboarding":true}`
const skeletonClaudeSession = `{"hasCompletedOnboarding":true}`

func writeSession(t *testing.T, home, content string) string {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	path := claudeSessionFile(home)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- AgentHome routing -------------------------------------------------------

func TestAgentHome_PerUIDInteractive_IsPerAgent(t *testing.T) {
	shared := withSharedAgentHome(t)
	got := AgentHome("scanner", 1001, "claude")
	want := filepath.Join(shared, "agents", "scanner")
	if got != want {
		t.Errorf("AgentHome = %q, want %q", got, want)
	}
}

func TestAgentHome_EscapeHatch_RestoresSharedHome(t *testing.T) {
	shared := withSharedAgentHome(t)
	t.Setenv("HIVE_SHARED_AGENT_HOME", "1")
	if got := AgentHome("scanner", 1001, "claude"); got != shared {
		t.Errorf("AgentHome with HIVE_SHARED_AGENT_HOME=1 = %q, want %q", got, shared)
	}
}

func TestAgentHome_Inference_KeepsInferenceHome(t *testing.T) {
	withSharedAgentHome(t)
	got := AgentHome("thinker", 1002, "litellm")
	if got != inferenceHomePath("thinker") {
		t.Errorf("inference AgentHome = %q, want %q", got, inferenceHomePath("thinker"))
	}
}

func TestAgentHome_NoUID_UsesProcessHome(t *testing.T) {
	withSharedAgentHome(t)
	t.Setenv("HOME", "/home/hive-test")
	if got := AgentHome("scanner", 0, "claude"); got != "/home/hive-test" {
		t.Errorf("uid 0 AgentHome = %q, want /home/hive-test", got)
	}
}

// --- provisioning ------------------------------------------------------------

func TestSetupInteractiveHome_CreatesHomeAndBridges(t *testing.T) {
	shared := withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}

	m.setupInteractiveHome(ap, "claude")

	home := interactiveHomePath("scanner")
	if info, err := os.Stat(home); err != nil || !info.IsDir() {
		t.Fatalf("home not created: %v", err)
	}
	for _, name := range append(append([]string{}, interactiveHomeBridgeDirs...), interactiveHomeBridgeFiles...) {
		link := filepath.Join(home, name)
		target, err := os.Readlink(link)
		if err != nil {
			t.Errorf("bridge %s: %v", name, err)
			continue
		}
		if want := filepath.Join(shared, name); target != want {
			t.Errorf("bridge %s -> %q, want %q", name, target, want)
		}
	}
	// .claude.json must NOT be bridged — it is the contended per-agent file.
	if _, err := os.Lstat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Errorf(".claude.json should not exist without a signed-in source (err=%v)", err)
	}
	// .bash_history must NOT be bridged.
	if _, err := os.Lstat(filepath.Join(home, ".bash_history")); !os.IsNotExist(err) {
		t.Errorf(".bash_history should not be bridged (err=%v)", err)
	}
}

func TestSetupInteractiveHome_IdempotentAndRepairsStaleLink(t *testing.T) {
	shared := withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}

	m.setupInteractiveHome(ap, "claude")
	home := interactiveHomePath("scanner")

	// Sabotage one bridge with a wrong target, then re-provision.
	stale := filepath.Join(home, ".claude")
	if err := os.Remove(stale); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/somewhere/else", stale); err != nil {
		t.Fatal(err)
	}
	m.setupInteractiveHome(ap, "claude")
	target, err := os.Readlink(stale)
	if err != nil || target != filepath.Join(shared, ".claude") {
		t.Errorf("stale bridge not repaired: target=%q err=%v", target, err)
	}
}

func TestSetupInteractiveHome_NeverClobbersRealDir(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}

	home := interactiveHomePath("scanner")
	realDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(realDir, "keep-me")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	m.setupInteractiveHome(ap, "claude")

	info, err := os.Lstat(realDir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("real .claude dir was clobbered: %v %v", info, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker lost: %v", err)
	}
}

func TestSetupInteractiveHome_SkipsInferenceUIDZeroAndEscapeHatch(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)

	m.setupInteractiveHome(&AgentProcess{Name: "a", UID: 0}, "claude")
	m.setupInteractiveHome(&AgentProcess{Name: "b", UID: 1001}, "litellm")
	if _, err := os.Stat(interactiveHomeRoot()); !os.IsNotExist(err) {
		t.Fatalf("root should not exist after skipped provisioning (err=%v)", err)
	}

	t.Setenv("HIVE_SHARED_AGENT_HOME", "1")
	m.setupInteractiveHome(&AgentProcess{Name: "c", UID: 1001}, "claude")
	if _, err := os.Stat(interactiveHomeRoot()); !os.IsNotExist(err) {
		t.Fatalf("escape hatch should skip provisioning (err=%v)", err)
	}
}

// --- claude session seeding ---------------------------------------------------

func TestSeedClaudeSession_AdoptsSignedInLegacyShared(t *testing.T) {
	shared := withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	writeSession(t, shared, signedInClaudeSession)

	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	target := claudeSessionFile(interactiveHomePath("scanner"))
	if got := inspectClaudeSession(target).State; got != claudeSessionSignedIn {
		t.Errorf("seeded session state = %v, want signed-in", got)
	}
}

func TestSeedClaudeSession_AdoptsSignedInSibling(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	// Legacy shared file is a skeleton (post-#4596 damage); a sibling holds
	// the good session.
	writeSession(t, sharedAgentHome, skeletonClaudeSession)
	writeSession(t, interactiveHomePath("guide"), signedInClaudeSession)

	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	target := claudeSessionFile(interactiveHomePath("scanner"))
	if got := inspectClaudeSession(target).State; got != claudeSessionSignedIn {
		t.Errorf("seeded-from-sibling state = %v, want signed-in", got)
	}
}

func TestSeedClaudeSession_NeverOverwritesSignedInTarget(t *testing.T) {
	shared := withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	own := `{"oauthAccount":{"accountUuid":"mine","emailAddress":"me@example.com"}}`
	writeSession(t, interactiveHomePath("scanner"), own)
	writeSession(t, shared, signedInClaudeSession)

	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	data, err := os.ReadFile(claudeSessionFile(interactiveHomePath("scanner")))
	if err != nil || string(data) != own {
		t.Errorf("signed-in per-agent session was overwritten: %q err=%v", data, err)
	}
}

func TestSeedClaudeSession_OverwritesSkeletonTarget(t *testing.T) {
	shared := withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	writeSession(t, interactiveHomePath("scanner"), skeletonClaudeSession)
	writeSession(t, shared, signedInClaudeSession)

	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	target := claudeSessionFile(interactiveHomePath("scanner"))
	if got := inspectClaudeSession(target).State; got != claudeSessionSignedIn {
		t.Errorf("skeleton target not re-seeded: state = %v", got)
	}
}

func TestSeedClaudeSession_NoSource_NoSeed(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)

	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	target := claudeSessionFile(interactiveHomePath("scanner"))
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("no signed-in source anywhere: no session must be fabricated (err=%v)", err)
	}
}

func TestSeedClaudeSession_SkipsUnparseableLegacyUsesSibling(t *testing.T) {
	shared := withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	writeSession(t, shared, "{not json")
	writeSession(t, interactiveHomePath("guide"), signedInClaudeSession)

	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	target := claudeSessionFile(interactiveHomePath("scanner"))
	if got := inspectClaudeSession(target).State; got != claudeSessionSignedIn {
		t.Errorf("unparseable legacy should fall through to sibling: state = %v", got)
	}
}

// --- orphaned tmp sweep --------------------------------------------------------

func TestSweepOrphanedClaudeTmp(t *testing.T) {
	shared := withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)

	for i := 0; i < 5; i++ {
		name := fmt.Sprintf(".claude.json.tmp.%d.abc", 1000+i)
		if err := os.WriteFile(filepath.Join(shared, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keep := filepath.Join(shared, ".claude.json")
	if err := os.WriteFile(keep, []byte(signedInClaudeSession), 0o600); err != nil {
		t.Fatal(err)
	}

	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	entries, err := os.ReadDir(shared)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) > len(".claude.json.tmp.") && e.Name()[:len(".claude.json.tmp.")] == ".claude.json.tmp." {
			t.Errorf("orphaned tmp file survived sweep: %s", e.Name())
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("real .claude.json must survive the sweep: %v", err)
	}
}
