package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// .local must be a REAL per-agent directory (#6238), not a bridge.
	if info, err := os.Lstat(filepath.Join(home, ".local")); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Errorf(".local should be a real per-agent dir: info=%v err=%v", info, err)
	}
}

// --- per-agent XDG data/state (#6238) ------------------------------------------

func xdgPairs(t *testing.T, m *Manager, ap *AgentProcess) (data, state string) {
	t.Helper()
	for _, p := range m.agentEnvPairs(ap) {
		switch p.Key {
		case "XDG_DATA_HOME":
			data = p.Value
		case "XDG_STATE_HOME":
			state = p.Value
		}
	}
	return data, state
}

func TestAgentEnvPairs_XDGStateHome_IsPerAgent(t *testing.T) {
	shared := withSharedAgentHome(t)
	m := NewManager(map[string]config.AgentConfig{
		"scanner":  {Backend: "claude", Model: "sonnet"},
		"reviewer": {Backend: "goose", Model: "sonnet"},
	}, discardLogger(), ProjectContext{})

	a := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude", Model: "sonnet"}}
	b := &AgentProcess{Name: "reviewer", UID: 1002, Config: config.AgentConfig{Backend: "goose", Model: "sonnet"}}

	dataA, stateA := xdgPairs(t, m, a)
	dataB, stateB := xdgPairs(t, m, b)
	if stateA == "" || stateB == "" || dataA == "" || dataB == "" {
		t.Fatalf("XDG vars missing: a=(%q,%q) b=(%q,%q)", dataA, stateA, dataB, stateB)
	}
	if stateA == stateB {
		t.Fatalf("two launched agents share XDG_STATE_HOME %q", stateA)
	}
	if dataA == dataB {
		t.Fatalf("two launched agents share XDG_DATA_HOME %q", dataA)
	}
	// Each resolves under ITS OWN per-agent home, never under the shared tree.
	if want := filepath.Join(interactiveHomePath("scanner"), ".local", "state"); stateA != want {
		t.Errorf("scanner XDG_STATE_HOME = %q, want %q", stateA, want)
	}
	if want := filepath.Join(interactiveHomePath("reviewer"), ".local", "share"); dataB != want {
		t.Errorf("reviewer XDG_DATA_HOME = %q, want %q", dataB, want)
	}
	sharedLocal := filepath.Join(shared, ".local") + string(filepath.Separator)
	for _, v := range []string{dataA, stateA, dataB, stateB} {
		if strings.HasPrefix(v, sharedLocal) {
			t.Errorf("%q resolves under the shared .local tree", v)
		}
	}
}

func TestAgentEnvPairs_XDG_NotExportedUnderSharedHome(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)

	// No UID: the process HOME is shared, so nothing is exported.
	if data, state := xdgPairs(t, m, &AgentProcess{Name: "scanner", Config: config.AgentConfig{Backend: "claude"}}); data != "" || state != "" {
		t.Errorf("uid-less agent exported XDG vars: %q %q", data, state)
	}
	// Escape hatch: legacy shared layout must stay whole.
	t.Setenv("HIVE_SHARED_AGENT_HOME", "1")
	if data, state := xdgPairs(t, m, &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}); data != "" || state != "" {
		t.Errorf("escape hatch exported XDG vars: %q %q", data, state)
	}
}

func TestSetupAgentXDGDirs_RetiresLegacyBridgeAndSharesOnlyNamedEntries(t *testing.T) {
	shared := withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	home := interactiveHomePath("scanner")

	// A pre-#6238 home: .local is a symlink bridge into the shared tree, and
	// the shared tree already holds state that must NOT be touched.
	if err := os.MkdirAll(filepath.Join(shared, ".local", "share", "goose", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(shared, ".local"), filepath.Join(home, ".local")); err != nil {
		t.Fatal(err)
	}

	m.setupInteractiveHome(ap, "claude")

	local := filepath.Join(home, ".local")
	if info, err := os.Lstat(local); err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		t.Fatalf("legacy .local bridge not retired: info=%v err=%v", info, err)
	}
	for _, dir := range []string{
		agentXDGDataHome(home),
		agentXDGStateHome(home),
		filepath.Join(agentXDGStateHome(home), gooseLogsStateRel),
	} {
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			t.Errorf("per-agent XDG dir %s missing: info=%v err=%v", dir, info, err)
		}
	}
	// Per-agent data is per-agent: goose sessions under the shared tree are
	// NOT visible through the agent's XDG_DATA_HOME, and untouched in place.
	if _, err := os.Stat(filepath.Join(agentXDGDataHome(home), "goose", "sessions")); !os.IsNotExist(err) {
		t.Errorf("shared goose sessions leaked into per-agent data home (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(shared, ".local", "share", "goose", "sessions")); err != nil {
		t.Errorf("shared goose sessions were disturbed: %v", err)
	}
	// The one named credential entry is bridged back to the shared tree.
	link := filepath.Join(agentXDGDataHome(home), "opencode")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("opencode credential bridge missing: %v", err)
	}
	if want := filepath.Join(shared, ".local", "share", "opencode"); target != want {
		t.Errorf("opencode bridge -> %q, want %q", target, want)
	}

	// Idempotent: a second provisioning keeps the real dir and the bridge.
	m.setupInteractiveHome(ap, "claude")
	if info, err := os.Lstat(local); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Errorf(".local regressed to a bridge on re-provision: info=%v err=%v", info, err)
	}
	if target, err := os.Readlink(link); err != nil || target != filepath.Join(shared, ".local", "share", "opencode") {
		t.Errorf("opencode bridge not stable: target=%q err=%v", target, err)
	}
}

func TestSetupAgentXDGDirs_KeepsExistingRealLocal(t *testing.T) {
	withSharedAgentHome(t)
	m := interactiveHomeTestManager(t)
	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	home := interactiveHomePath("scanner")

	marker := filepath.Join(agentXDGStateHome(home), "muse", "keep-me")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	m.setupInteractiveHome(ap, "claude")

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("live per-agent state lost: %v", err)
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
