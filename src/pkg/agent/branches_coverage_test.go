package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// ---------------------------------------------------------------------------
// deliverStartupKick — drop branches (stopped / generation mismatch)
// ---------------------------------------------------------------------------

func TestDeliverStartupKick_DroppedStopped(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covsk1"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covsk1": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covsk1"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	agent.State = StateStopped // not running -> dropped after readiness gates
	agent.launchGen = 0
	// Render a ready prompt so the wait gates pass quickly.
	paneInject(t, session, "goose is ready")
	m.deliverStartupKick(agent, "prompt", 0)
	if agent.LastKick != nil {
		t.Error("startup kick to a stopped agent should be dropped")
	}
}

func TestDeliverStartupKick_DroppedGenMismatch(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covsk2"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covsk2": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covsk2"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	agent.State = StateRunning
	agent.launchGen = 5
	paneInject(t, session, "goose is ready")
	// Pass a stale generation (0 != 5) -> dropped.
	m.deliverStartupKick(agent, "prompt", 0)
	if agent.LastKick != nil {
		t.Error("startup kick with stale generation should be dropped")
	}
}

func TestDeliverStartupKick_Delivered(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covsk3"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covsk3": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covsk3"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	agent.State = StateRunning
	agent.launchGen = 3
	paneInject(t, session, "goose is ready")
	// Don't start deliverStartupKick's readiness clock until tmux has actually
	// painted the marker: on a loaded runner the render can outlast
	// paneInject's fixed sleep AND the TestMain-shrunk cliReadyTimeout,
	// dropping the kick and flaking this test for reasons unrelated to the
	// delivery path under test. Same pattern as
	// TestDeliverStartupKick_BobReceivesKick (#4871).
	requirePaneShows(t, session, "goose is ready")
	m.deliverStartupKick(agent, "bootstrap prompt", 3)
	if agent.LastKick == nil {
		t.Error("startup kick should be delivered on matching generation")
	}
}

// ---------------------------------------------------------------------------
// runCopilotDiagnostic — binary-not-found early return.
// ---------------------------------------------------------------------------

func TestRunCopilotDiagnostic_NoBinary(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	// Temporarily remove the global copilot stub so backendBinary("copilot")
	// fails — without mutating PATH (which would race launch goroutines).
	stub := filepath.Join(stubBinDir, "copilot")
	if orig, err := os.ReadFile(stub); err == nil {
		os.Remove(stub)
		t.Cleanup(func() { _ = os.WriteFile(stub, orig, 0o755) })
	}
	session := "hive-covdiag"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covdiag": {Backend: "copilot"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covdiag"]
	m.mu.RUnlock()
	agent.tmuxSession = session
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// ensureTmuxSession succeeds (session exists), then copilot binary lookup
	// fails -> early return.
	m.runCopilotDiagnostic(ctx, agent)
}

// ---------------------------------------------------------------------------
// NewManager — with a UID map file present on disk.
// ---------------------------------------------------------------------------

func TestNewManager_ResolveByID(t *testing.T) {
	// Write a uid-map to a temp path and point the loader at it by pre-seeding
	// the well-known path is not possible (const). Instead exercise LoadUIDMap
	// + NewManager wiring by constructing agents whose IDs differ from names.
	dir := t.TempDir()
	t.Setenv("HIVE_WORK_DIR", dir)
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", ID: "scanner-id-1"},
	}, discardLogger(), ProjectContext{})
	if got := m.ResolveAgent("scanner-id-1"); got != "scanner" {
		t.Errorf("ResolveAgent by id = %q, want scanner", got)
	}
	if got := m.ResolveAgent("unknown"); got != "unknown" {
		t.Errorf("ResolveAgent unknown should echo input, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// tmuxCmd — per-agent socket branch (agent.tmuxSocket set).
// ---------------------------------------------------------------------------

func TestTmuxCmd_SocketBranch(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	agent.tmuxSocket = "hive-a"
	cmd := m.tmuxCmd(agent, "has-session", "-t", "x")
	// The built command should route through the -L socket flag.
	joined := ""
	for _, a := range cmd.Args {
		joined += a + " "
	}
	if !containsBoot(joined, "-L") || !containsBoot(joined, "hive-a") {
		t.Errorf("socket tmux cmd should include -L hive-a: %q", joined)
	}
}

func TestTmuxCmd_SuExecBranch(t *testing.T) {
	agentName := "definitely-missing-agent-user"
	m := NewManager(map[string]config.AgentConfig{agentName: {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents[agentName]
	m.mu.RUnlock()
	agent.UID = 2001
	agent.tmuxSocket = "hive-" + agentName
	cmd := m.tmuxCmd(agent, "has-session")
	if cmd.Args[0] != "su-exec" {
		t.Errorf("UID>0 tmux cmd should run via su-exec, got %q", cmd.Args[0])
	}
	wantUserSpec := "2001:" + strconv.Itoa(os.Getgid())
	if cmd.Args[1] != wantUserSpec {
		t.Errorf("UID>0 tmux cmd should fall back to numeric uid:gid %q, got %q", wantUserSpec, cmd.Args[1])
	}
}

func TestEnsureTmuxSession_IncludesStderr(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "su-exec")
	script := `#!/bin/sh
case "$*" in
  *has-session*) exit 1 ;;
  *new-session*) echo "su-exec: getpwnam(hive-architect): Success" >&2; exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HIVE_WORK_DIR", filepath.Join(dir, "agents"))

	m := NewManager(map[string]config.AgentConfig{"architect": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["architect"]
	agent.UID = 2006
	agent.tmuxSocket = "hive-architect"
	agent.tmuxSession = "hive-architect"
	err := m.ensureTmuxSession(agent)
	m.mu.Unlock()
	if err == nil {
		t.Fatal("ensureTmuxSession returned nil, want tmux creation error")
	}
	if got := err.Error(); !strings.Contains(got, "getpwnam") {
		t.Fatalf("ensureTmuxSession error %q does not include tmux stderr", got)
	}
}

// ---------------------------------------------------------------------------
// ensureTmuxSession — mkdir failure branch (work dir path blocked by a file).
// ---------------------------------------------------------------------------

func TestEnsureTmuxSession_MkdirError(t *testing.T) {
	dir := t.TempDir()
	// Make the work dir a regular file so MkdirAll(workDir/agent) fails.
	blocker := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_WORK_DIR", blocker)
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	agent := m.agents["a"]
	agent.tmuxSession = "hive-a-mkdirfail-xyz"
	err := m.ensureTmuxSession(agent)
	m.mu.Unlock()
	if err == nil {
		t.Error("expected mkdir error when work dir is a file")
	}
}

// ---------------------------------------------------------------------------
// confirmMenuOption — navigation path (wanted option not initially selected).
// ---------------------------------------------------------------------------

func TestConfirmMenuOption_NavigatesToOption(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	session := "hive-covnav"
	newRawTmuxSession(t, session)
	m := NewManager(map[string]config.AgentConfig{"covnav": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["covnav"]
	m.mu.RUnlock()
	agent.tmuxSession = session

	// Title present, but the selected ❯ line does NOT contain the wanted text,
	// so confirmMenuOption presses navKey and re-reads (up to max steps).
	testTmuxCommand("send-keys", "-t", session, "-l", ": Detected a custom API key").Run()
	testTmuxCommand("send-keys", "-t", session, "Enter").Run()
	testTmuxCommand("send-keys", "-t", session, "-l", "❯ 1. No (recommended)").Run()
	testTmuxCommand("send-keys", "-t", session, "Enter").Run()
	time.Sleep(300 * time.Millisecond)

	// Wanted "Yes" is never on the selected line -> exhausts nav steps -> false.
	// (Exercises the navigation loop + the "not reached" warning branch.)
	_ = m.confirmMenuOption(agent, "Detected a custom API key", "Yes", "Up")
}

// ---------------------------------------------------------------------------
// seedJSONFile — marshal path already covered; add write-target unwritable
// (directory as target) to hit the write-error warn branch.
// ---------------------------------------------------------------------------

func TestSeedJSONFile_WriteError(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	dir := t.TempDir()
	// Target path is a directory -> WriteFile fails -> warn branch.
	target := filepath.Join(dir, "adir")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	m.seedJSONFile("agent", target, map[string]any{"k": "v"})
}

// ---------------------------------------------------------------------------
// mergeApprovedAPIKeys — already-present seed (no change branch) + write path.
// ---------------------------------------------------------------------------

func TestMergeApprovedAPIKeys_AlreadyPresent(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")

	// Seed the file already containing every approved key form for "scanner".
	seed := inferenceUserConfigSeed("scanner")
	data, _ := json.Marshal(map[string]any{
		"customApiKeyResponses": seed["customApiKeyResponses"],
	})
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// No missing keys -> changed=false -> early return (no rewrite).
	m.mergeApprovedAPIKeys("scanner", path)
}
