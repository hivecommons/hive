package agent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

func TestSpecialistRoleAllowlistIsExact(t *testing.T) {
	want := []SpecialistRole{
		SpecialistQuality,
		SpecialistCIMaintainer,
		SpecialistSecurity,
		SpecialistArchitect,
		SpecialistScanner,
	}
	if len(specialistRoleAllowlist) != len(want) {
		t.Fatalf("specialist role count = %d, want %d", len(specialistRoleAllowlist), len(want))
	}
	for _, role := range want {
		if !isAllowedSpecialistRole(role) {
			t.Errorf("required role %q is not allowed", role)
		}
	}
	for _, role := range []SpecialistRole{"", "supervisor", "guide", "quality-extra", "QUALITY"} {
		if isAllowedSpecialistRole(role) {
			t.Errorf("unexpected role %q is allowed", role)
		}
	}
}

func TestNewManagerWithWorkDirIsExplicitStableAndNamespaced(t *testing.T) {
	configs := testSpecialistConfigs()
	root := filepath.Join(t.TempDir(), "specialists", "agents")
	t.Setenv("HIVE_WORK_DIR", filepath.Join(t.TempDir(), "ambient-must-not-win"))

	provider := testSpecialistProvider(t)
	codexHome := testSpecialistCodexHome(t)
	first, err := NewSpecialistManager(configs, testSpecialistLogger(), ProjectContext{ACMMLevel: 6}, SpecialistManagerOptions{WorkDir: root, CodexCommand: provider, CodexHome: codexHome})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewSpecialistManager(configs, testSpecialistLogger(), ProjectContext{ACMMLevel: 6}, SpecialistManagerOptions{WorkDir: root, CodexCommand: provider, CodexHome: codexHome})
	if err != nil {
		t.Fatal(err)
	}
	if first.workDir != filepath.Clean(root) {
		t.Fatalf("workDir = %q, want %q", first.workDir, filepath.Clean(root))
	}
	for role := range specialistRoleAllowlist {
		name := string(role)
		if first.agents[name].tmuxSession != second.agents[name].tmuxSession {
			t.Fatalf("session for %s is not stable", role)
		}
		if !strings.HasPrefix(first.agents[name].tmuxSession, "hive-"+name+"-") {
			t.Fatalf("session for %s = %q", role, first.agents[name].tmuxSession)
		}
		if first.agents[name].UID != 0 || first.agents[name].tmuxSocket != "" {
			t.Fatalf("explicit private-workdir manager must share controller ownership: %+v", first.agents[name])
		}
		if len(first.agents[name].specialistProviderSHA256) != 64 || first.agents[name].specialistProviderSHA256 != first.specialistProvider.SHA256 {
			t.Fatalf("session for %s is not bound to the sealed provider digest", role)
		}
		if info, statErr := os.Stat(filepath.Join(root, name)); statErr != nil || !info.IsDir() {
			t.Fatalf("isolated work directory for %s: info=%v err=%v", role, info, statErr)
		} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Fatalf("isolated work directory for %s widened to %o", role, info.Mode().Perm())
		}
	}
	if info, statErr := os.Stat(root); statErr != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Fatalf("specialist root is not private: info=%v err=%v", info, statErr)
	}

	other, err := NewSpecialistManager(configs, testSpecialistLogger(), ProjectContext{ACMMLevel: 6}, SpecialistManagerOptions{WorkDir: filepath.Join(t.TempDir(), "specialists", "agents"), CodexCommand: provider, CodexHome: codexHome})
	if err != nil {
		t.Fatal(err)
	}
	if first.agents["quality"].tmuxSession == other.agents["quality"].tmuxSession {
		t.Fatal("different manager roots collided on a tmux session")
	}
	if _, err := NewSpecialistManager(configs, testSpecialistLogger(), ProjectContext{}, SpecialistManagerOptions{WorkDir: "relative", CodexCommand: provider, CodexHome: codexHome}); err == nil {
		t.Fatal("relative work directory was accepted")
	}
	if _, err := NewSpecialistManager(configs, testSpecialistLogger(), ProjectContext{}, SpecialistManagerOptions{WorkDir: filepath.Join(t.TempDir(), "agents"), CodexCommand: "codex --wrapper", CodexHome: codexHome}); err == nil {
		t.Fatal("non-absolute provider command was accepted")
	}
	bad := map[string]config.AgentConfig{"../quality": {Enabled: true, Backend: "codex", Mode: "ADVISORY"}}
	if _, err := NewSpecialistManager(bad, testSpecialistLogger(), ProjectContext{}, SpecialistManagerOptions{WorkDir: filepath.Join(t.TempDir(), "agents"), CodexCommand: provider, CodexHome: codexHome}); err == nil {
		t.Fatal("unsafe agent name was accepted")
	}
}

func TestControllerOwnedSpecialistManagerDoesNotChangeOrdinaryUIDIsolation(t *testing.T) {
	configs := testSpecialistConfigs()
	uidMap := NewUIDMap()
	uidMap.AllocateUIDs([]string{"architect", "ci-maintainer", "quality", "scanner", "sec-check"})
	ordinary := newManagerWithUIDMap(configs, testSpecialistLogger(), ProjectContext{}, "/data/agents", "", "", uidMap)
	qualityUID := ordinary.agents["quality"].UID
	if qualityUID == 0 || ordinary.agents["quality"].tmuxSocket == "" {
		t.Fatalf("loaded UID map was not applied to ordinary manager: %+v", ordinary.agents["quality"])
	}

	controllerOwned := newManagerWithUIDMap(configs, testSpecialistLogger(), ProjectContext{}, filepath.Join(t.TempDir(), "specialists", "agents"), "", "", uidMap)
	controllerOwned.useControllerOwnershipForSpecialists()
	if controllerOwned.uidMap != nil || controllerOwned.agents["quality"].UID != 0 || controllerOwned.agents["quality"].tmuxSocket != "" {
		t.Fatalf("specialist manager retained entrypoint UID isolation: %+v", controllerOwned.agents["quality"])
	}
	if ordinary.uidMap != uidMap || ordinary.agents["quality"].UID != qualityUID || ordinary.agents["quality"].tmuxSocket == "" {
		t.Fatalf("specialist ownership override mutated ordinary manager: %+v", ordinary.agents["quality"])
	}
}

func TestCheckSpecialistRoleStartsPromptFreeSessionWithoutTask(t *testing.T) {
	manager := testSpecialistManager(t, map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "codex", Mode: "ADVISORY"},
	})
	runtime, counters := fakeSpecialistRuntime(manager)

	identity, err := manager.checkSpecialistRole(context.Background(), SpecialistQuality, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if counters.starts != 1 || counters.restarts != 0 || counters.sends != 0 || counters.configures != 1 {
		t.Fatalf("readiness side effects = %+v", *counters)
	}
	if identity.Specialist != SpecialistQuality || identity.SessionID == "" || identity.WorkDir != filepath.Join(manager.workDir, "quality") {
		t.Fatalf("identity = %+v", identity)
	}
	if lease := manager.specialistLeases["quality"]; lease != "" {
		t.Fatalf("readiness lease was not released: %q", lease)
	}
	if manager.agents["quality"].LastKick != nil || manager.agents["quality"].LastKickMessage != "" {
		t.Fatal("readiness sent a model task")
	}
	dispatched, err := manager.dispatchSpecialistTask(context.Background(), SpecialistDispatchRequest{
		TaskID: "swo-" + strings.Repeat("9", 64), Specialist: SpecialistQuality, Message: "bounded task",
	}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched.SessionID != identity.SessionID || dispatched.WorkDir != identity.WorkDir {
		t.Fatalf("Check/Dispatch identity drift: check=%+v dispatch=%+v", identity, dispatched.SpecialistSessionIdentity)
	}
}

func TestDispatchSpecialistTaskLeasesStartsKicksAndReuses(t *testing.T) {
	manager := testSpecialistManager(t, map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "codex", Mode: "ADVISORY", Role: "quality"},
	})
	runtime, counters := fakeSpecialistRuntime(manager)
	taskA := "swo-" + strings.Repeat("a", 64)
	taskB := "swo-" + strings.Repeat("b", 64)
	request := SpecialistDispatchRequest{TaskID: taskA, Specialist: SpecialistQuality, Message: "Read the bounded work order and propose its patch."}

	first, err := manager.dispatchSpecialistTask(context.Background(), request, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Started || first.Reused || counters.starts != 1 || counters.sends != 1 || counters.lastTask != taskA {
		t.Fatalf("first dispatch = %+v counters=%+v", first, *counters)
	}
	if manager.specialistLeases["quality"] != taskA {
		t.Fatal("task lease was not retained")
	}
	if err := manager.SendKick("quality", "ordinary governor kick"); err == nil || !strings.Contains(err.Error(), "leased") {
		t.Fatalf("ordinary SendKick raced specialist lease: %v", err)
	}

	repeated, err := manager.dispatchSpecialistTask(context.Background(), request, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !repeated.Reused || repeated.Started || counters.starts != 1 || counters.sends != 1 {
		t.Fatalf("repeated dispatch duplicated work: result=%+v counters=%+v", repeated, *counters)
	}
	if _, err := manager.dispatchSpecialistTask(context.Background(), SpecialistDispatchRequest{TaskID: taskB, Specialist: SpecialistQuality, Message: "different"}, runtime); err == nil || !strings.Contains(err.Error(), taskA) {
		t.Fatalf("different task acquired leased role: %v", err)
	}
	if err := manager.ReleaseSpecialistTask(SpecialistQuality, taskB); err == nil {
		t.Fatal("wrong task released the role")
	}
	if err := manager.ReleaseSpecialistTask(SpecialistQuality, taskA); err != nil {
		t.Fatal(err)
	}
	if err := manager.ReleaseSpecialistTask(SpecialistQuality, taskA); err == nil {
		t.Fatal("missing lease release succeeded")
	}

	second, err := manager.dispatchSpecialistTask(context.Background(), SpecialistDispatchRequest{TaskID: taskB, Specialist: SpecialistQuality, Message: "next bounded task"}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if second.Started || second.Reused || counters.starts != 1 || counters.sends != 2 || counters.lastTask != taskB {
		t.Fatalf("second task did not reuse persistent session: result=%+v counters=%+v", second, *counters)
	}
}

func TestDispatchSpecialistTaskEnforcesEnabledAdvisoryConfigurationAndLaunch(t *testing.T) {
	taskID := "swo-" + strings.Repeat("c", 64)
	request := SpecialistDispatchRequest{TaskID: taskID, Specialist: SpecialistQuality, Message: "bounded"}

	tests := []struct {
		name   string
		config config.AgentConfig
		mutate func(*AgentProcess)
		want   string
	}{
		{name: "disabled", config: config.AgentConfig{Backend: "codex", Mode: "ADVISORY"}, want: "disabled"},
		{name: "effective mode", config: config.AgentConfig{Enabled: true, Backend: "codex", Mode: "ADVISORY", Tools: &config.ToolsConfig{Preset: "full"}}, want: "ADVISORY is required"},
		{name: "wrong configured role", config: config.AgentConfig{Enabled: true, Backend: "codex", Mode: "ADVISORY", Role: "scanner"}, want: "configured for role"},
		{name: "runtime skill expansion", config: config.AgentConfig{Enabled: true, Backend: "codex", Mode: "ADVISORY", CavemanMode: "lite"}, want: "runtime skills"},
		{name: "MCP expansion", config: config.AgentConfig{Enabled: true, Backend: "codex", Mode: "ADVISORY", Connections: []config.ConnectionConfig{{Name: "tools", Type: "mcp", URI: "local"}}}, want: "MCP connection"},
		{name: "authenticated connection", config: config.AgentConfig{Enabled: true, Backend: "codex", Mode: "ADVISORY", Connections: []config.ConnectionConfig{{Name: "api", Type: "api", Auth: &config.ConnectionAuth{Type: "env", EnvVar: "API_TOKEN"}}}}, want: "authenticated connection"},
		{name: "running other mode", config: config.AgentConfig{Enabled: true, Backend: "codex", Mode: "ADVISORY"}, mutate: func(agent *AgentProcess) {
			agent.State = StateRunning
			agent.HasLaunched = true
			agent.LaunchedMode = ModeIssuesAndPRs
		}, want: "already running"},
		{name: "paused", config: config.AgentConfig{Enabled: true, Backend: "codex", Mode: "ADVISORY"}, mutate: func(agent *AgentProcess) {
			agent.State = StatePaused
			agent.Paused = true
		}, want: "paused"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := testSpecialistManager(t, map[string]config.AgentConfig{"quality": test.config})
			if test.mutate != nil {
				test.mutate(manager.agents["quality"])
			}
			runtime, _ := fakeSpecialistRuntime(manager)
			if _, err := manager.dispatchSpecialistTask(context.Background(), request, runtime); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
			if lease := manager.specialistLeases["quality"]; lease != "" {
				t.Fatalf("failed dispatch retained lease %q", lease)
			}
		})
	}

	manager := testSpecialistManager(t, map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "codex", Mode: "ISSUES_PRS_MERGE", Tools: &config.ToolsConfig{Preset: "advisory"}},
	})
	runtime, counters := fakeSpecialistRuntime(manager)
	if _, err := manager.dispatchSpecialistTask(context.Background(), request, runtime); err != nil {
		t.Fatalf("advisory Tools preset did not take precedence: %v", err)
	}
	if counters.sends != 1 || manager.agents["quality"].LaunchedMode != ModeAdvisory {
		t.Fatalf("effective advisory launch was not used: counters=%+v mode=%s", *counters, manager.agents["quality"].LaunchedMode)
	}
}

func TestDispatchSpecialistTaskRestartsExistingAdvisorySessionForIsolation(t *testing.T) {
	manager := testSpecialistManager(t, map[string]config.AgentConfig{
		"scanner": {Enabled: true, Backend: "codex", Mode: "ADVISORY"},
	})
	agent := manager.agents["scanner"]
	agent.State = StateRunning
	agent.HasLaunched = true
	agent.LaunchedMode = ModeAdvisory
	runtime, counters := fakeSpecialistRuntime(manager)

	result, err := manager.dispatchSpecialistTask(context.Background(), SpecialistDispatchRequest{
		TaskID: "swo-" + strings.Repeat("d", 64), Specialist: SpecialistScanner, Message: "triage",
	}, runtime)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Started || counters.restarts != 1 || counters.starts != 0 || counters.sends != 1 {
		t.Fatalf("running unisolated advisory session was not restarted: result=%+v counters=%+v", result, *counters)
	}
}

func TestDispatchFailureReleasesLease(t *testing.T) {
	manager := testSpecialistManager(t, map[string]config.AgentConfig{
		"architect": {Enabled: true, Backend: "codex", Mode: "ADVISORY"},
	})
	runtime, _ := fakeSpecialistRuntime(manager)
	runtime.send = func(string, string, string) error { return errors.New("kick failed") }
	taskID := "swo-" + strings.Repeat("e", 64)
	if _, err := manager.dispatchSpecialistTask(context.Background(), SpecialistDispatchRequest{TaskID: taskID, Specialist: SpecialistArchitect, Message: "review"}, runtime); err == nil {
		t.Fatal("dispatch failure was accepted")
	}
	if lease := manager.specialistLeases["architect"]; lease != "" {
		t.Fatalf("dispatch failure retained lease %q", lease)
	}
}

func TestPublicDispatchClassifiesOnlyManagerErrorsAsNotDelivered(t *testing.T) {
	manager := testSpecialistManager(t, map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "codex", Mode: "ADVISORY"},
	})
	_, err := manager.DispatchSpecialistTask(context.Background(), SpecialistDispatchRequest{
		TaskID: "swo-" + strings.Repeat("f", 64), Specialist: SpecialistQuality, Message: "review",
	})
	if err == nil || !errors.Is(err, ErrSpecialistTaskNotDelivered) {
		t.Fatalf("public Manager dispatch did not classify its pre-delivery failure: %v", err)
	}
	if errors.Is(errors.New("third-party dispatcher failed"), ErrSpecialistTaskNotDelivered) {
		t.Fatal("arbitrary dispatcher errors were classified as definitely undelivered")
	}
}

func TestSpecialistDispatchReadyMarker(t *testing.T) {
	taskID := "swo-" + strings.Repeat("a", 64)
	marker := SpecialistDispatchReadyMarker(taskID)
	if marker != "HIVE_SPECIALIST_DISPATCH_READY_"+strings.Repeat("a", 16) {
		t.Fatalf("marker = %q", marker)
	}
	if SpecialistDispatchReadyMarker("invalid") != "" {
		t.Fatal("invalid work-order ID received a dispatch marker")
	}
}

func TestSpecialistTmuxPasteBufferArgsEnableBracketedPaste(t *testing.T) {
	got := specialistTmuxPasteBufferArgs("swo-buffer", "hive-quality-session")
	want := []string{"paste-buffer", "-p", "-r", "-b", "swo-buffer", "-d", "-t", "hive-quality-session"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("specialist tmux paste args = %q, want %q", got, want)
	}
}

func TestSpecialistBracketedPasteStatusSupportsTmux33AndFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name      string
		output    string
		wantKnown bool
		wantReady bool
	}{
		{name: "enabled", output: "1\n", wantKnown: true, wantReady: true},
		{name: "disabled", output: "0\n", wantKnown: true, wantReady: false},
		{name: "tmux_33_format_unavailable", output: "", wantKnown: false, wantReady: false},
		{name: "ambiguous", output: "1 0\n", wantKnown: true, wantReady: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			known, ready := specialistBracketedPasteStatus([]byte(test.output))
			if known != test.wantKnown || ready != test.wantReady {
				t.Fatalf("specialistBracketedPasteStatus(%q) = (%t, %t), want (%t, %t)", test.output, known, ready, test.wantKnown, test.wantReady)
			}
		})
	}
}

func TestInspectSpecialistRoleIsNonStartingAndReleaseDistinguishesAbsentLease(t *testing.T) {
	provider := testSpecialistProvider(t)
	manager, err := NewSpecialistManager(map[string]config.AgentConfig{
		"quality": {ID: "quality", Role: "quality", Enabled: true, Backend: "codex", Mode: "ADVISORY"},
	}, testSpecialistLogger(), ProjectContext{}, SpecialistManagerOptions{
		WorkDir: filepath.Join(t.TempDir(), "specialists"), CodexCommand: provider, CodexHome: testSpecialistCodexHome(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	inspections := 0
	identity, err := manager.inspectSpecialistRole(context.Background(), SpecialistQuality, func(_ context.Context, agent *AgentProcess) bool {
		inspections++
		return agent.State == StateStopped && !agent.specialistReady && !agent.HasLaunched
	})
	if err != nil {
		t.Fatal(err)
	}
	if inspections != 1 || identity.AgentName != "quality" || identity.SessionID == "" || identity.ProviderSHA256 != manager.SpecialistProviderSHA256() {
		t.Fatalf("non-starting inspection identity = %+v, calls=%d", identity, inspections)
	}
	if manager.agents["quality"].State != StateStopped || manager.agents["quality"].specialistReady || len(manager.specialistLeases) != 0 {
		t.Fatalf("inspection mutated the specialist runtime: %+v leases=%v", manager.agents["quality"], manager.specialistLeases)
	}
	taskID := "swo-" + strings.Repeat("1", 64)
	if err := manager.ReleaseSpecialistTask(SpecialistQuality, taskID); !errors.Is(err, ErrSpecialistTaskLeaseAbsent) {
		t.Fatalf("absent lease error = %v", err)
	}
	manager.specialistLeases["quality"] = "swo-" + strings.Repeat("2", 64)
	if err := manager.ReleaseSpecialistTask(SpecialistQuality, taskID); err == nil || errors.Is(err, ErrSpecialistTaskLeaseAbsent) {
		t.Fatalf("wrong-task lease was confused with absence: %v", err)
	}
	manager.specialistLeases = map[string]string{}
	if err := manager.ShutdownSpecialists(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestSpecialistEnvironmentRemovesWriterCredentialsAndIsolatesGitHub(t *testing.T) {
	for _, name := range append(append([]string(nil), specialistGitHubCredentialNames...), "GH_APP_FUTURE_SECRET", "GITHUB_APP_FUTURE_KEY", "HIVE_GITHUB_APP_FUTURE_TOKEN") {
		if !isSpecialistGitHubCredential(name) {
			t.Errorf("writer credential %q was not recognized", name)
		}
	}
	for _, name := range []string{"COPILOT_GITHUB_TOKEN", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "PATH"} {
		if isSpecialistGitHubCredential(name) {
			t.Errorf("AI/runtime credential %q would be stripped", name)
		}
	}

	manager := testSpecialistManager(t, map[string]config.AgentConfig{
		"sec-check": {Enabled: true, Backend: "codex", Mode: "ADVISORY"},
	})
	agent := manager.agents["sec-check"]
	agent.specialistReady = true
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "OPENAI_API_KEY", "HTTP_PROXY", "HTTPS_PROXY", "NODE_EXTRA_CA_CERTS"} {
		t.Setenv(name, "ambient-secret-must-not-cross")
	}
	prefix := manager.buildEnvPrefix(agent)
	if !strings.HasPrefix(prefix, "env -i ") {
		t.Fatalf("specialist launch does not start from a clean environment: %s", prefix)
	}
	for _, expected := range []string{"HOME=", "CODEX_HOME=", "HIVE_AGENT='sec-check'", "HIVE_SPECIALIST_NAMESPACE='testnamespace'", "GH_CONFIG_DIR=", "GH_PROMPT_DISABLED='1'", "GIT_TERMINAL_PROMPT='0'", "GIT_CONFIG_GLOBAL=", "credential.interactive", "credential.helper"} {
		if !strings.Contains(prefix, expected) {
			t.Errorf("launch prefix missing %q: %s", expected, prefix)
		}
	}
	for _, forbidden := range []string{"ambient-secret-must-not-cross", "GH_TOKEN=", "GITHUB_TOKEN=", "OPENAI_API_KEY=", "HTTP_PROXY=", "HTTPS_PROXY=", "NODE_EXTRA_CA_CERTS=", "HIVE_AGENT_TOKEN_CACHE="} {
		if strings.Contains(prefix, forbidden) {
			t.Errorf("specialist launch exposed forbidden environment %q: %s", forbidden, prefix)
		}
	}
	modeFile := manager.agentModeFile(agent)
	if info, err := os.Stat(modeFile); err != nil {
		t.Fatalf("private specialist mode file: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o640 {
		t.Fatalf("specialist mode file permissions = %o, want 640", info.Mode().Perm())
	}
}

func TestCodexInteractiveCommandIsWorkspaceBoundAndExpansionFree(t *testing.T) {
	if !paneHasInputPrompt("Codex ready\n›") {
		t.Fatal("Codex interactive prompt is not recognized")
	}
	workDir := "/private/hive/specialists/quality"
	command := codexSpecialistLaunchCmd("/opt/codex", "gpt-specialist", workDir)
	for _, expected := range []string{
		"'/opt/codex' --model 'gpt-specialist'",
		"--cd '/private/hive/specialists/quality'",
		"--ask-for-approval never",
		"web_search=\"disabled\"",
		`default_permissions="hive_specialist_mailbox"`,
		`permissions.hive_specialist_mailbox.filesystem={":minimal"="read"}`,
		"permissions.hive_specialist_mailbox.network.enabled=false",
		"--disable plugins",
		"--disable enable_mcp_apps",
		"--disable tool_search",
		"--strict-config",
	} {
		if !strings.Contains(command, expected) {
			t.Errorf("command missing %q: %s", expected, command)
		}
	}
	for _, forbidden := range []string{"--dangerously", "--add-dir", "--sandbox workspace-write", `="write"`, "--enable ", "--search", "--ignore-user-config", "--ignore-rules", "--ephemeral", "target-checkout", "mcp-server"} {
		if strings.Contains(command, forbidden) {
			t.Errorf("command contains forbidden %q: %s", forbidden, command)
		}
	}
	fromTools := toolRulesToLaunchCmd("/opt/codex", "gpt-specialist", "codex", &config.ToolsConfig{Preset: "advisory"}, false)
	if strings.Contains(fromTools, "hive_specialist_mailbox") || fromTools == command {
		t.Fatalf("ordinary Codex tool rules unexpectedly received specialist containment: %s", fromTools)
	}
	injected := codexSpecialistLaunchCmd("/opt/codex", "model; touch /tmp/escaped", "/private/hive/specialists/quality; touch /tmp/escaped")
	if !strings.Contains(injected, "--model 'model; touch /tmp/escaped'") {
		t.Fatalf("model was not shell-quoted: %s", injected)
	}
	if !strings.Contains(injected, "--cd '/private/hive/specialists/quality; touch /tmp/escaped'") {
		t.Fatalf("specialist work directory was not shell-quoted: %s", injected)
	}
}

func TestBackendBinaryResolvesCodex(t *testing.T) {
	directory := t.TempDir()
	name := "codex"
	body := []byte("#!/bin/sh\nexit 0\n")
	if runtime.GOOS == "windows" {
		name = "codex.cmd"
		body = []byte("@exit /b 0\r\n")
	}
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))
	resolved, err := backendBinary("codex")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(resolved) || !strings.EqualFold(filepath.Base(resolved), name) {
		t.Fatalf("resolved Codex path = %q", resolved)
	}
}

func TestSpecialistBackendBinaryUsesExactInstalledCodexOnlyForSpecialists(t *testing.T) {
	directory := t.TempDir()
	ambientDirectory := t.TempDir()
	name := "codex"
	if runtime.GOOS == "windows" {
		name = "codex.exe"
	}
	exact := filepath.Join(directory, name)
	if err := os.WriteFile(exact, []byte("native-placeholder"), 0o755); err != nil {
		t.Fatal(err)
	}
	ambient := filepath.Join(ambientDirectory, name)
	if err := os.WriteFile(ambient, []byte("ambient-placeholder"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ambientDirectory)
	agent := &AgentProcess{Config: config.AgentConfig{Backend: "codex", LaunchCmd: filepath.Join(directory, "ignored")}}
	identity, err := inspectSpecialistProvider(exact)
	if err != nil {
		t.Fatal(err)
	}
	manager := &Manager{specialistProvider: identity}
	ordinary, err := manager.specialistBackendBinary(agent)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(ordinary) != filepath.Clean(ambient) {
		t.Fatalf("ordinary agent resolved %q, want ambient PATH command %q", ordinary, ambient)
	}
	agent.specialistReady = true
	resolved, err := manager.specialistBackendBinary(agent)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != filepath.Clean(exact) {
		t.Fatalf("resolved path = %q, want %q", resolved, exact)
	}
	manager.specialistProvider = nil
	if _, err := manager.specialistBackendBinary(agent); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("prepared specialist fell back to PATH without an exact binding: %v", err)
	}
	manager.specialistProvider = identity

	agent.Config.Backend = "claude"
	if _, err := manager.specialistBackendBinary(agent); err == nil || !strings.Contains(err.Error(), "only for codex") {
		t.Fatalf("non-Codex exact command was accepted: %v", err)
	}
}

func TestSpecialistProviderSealSurvivesSourceReplacementAndRejectsSealMutation(t *testing.T) {
	provider := testSpecialistProvider(t)
	root := filepath.Join(t.TempDir(), "specialists")
	manager, err := NewSpecialistManager(testSpecialistConfigs(), testSpecialistLogger(), ProjectContext{}, SpecialistManagerOptions{WorkDir: root, CodexCommand: provider, CodexHome: testSpecialistCodexHome(t)})
	if err != nil {
		t.Fatal(err)
	}
	originalDigest := manager.specialistProvider.SHA256
	sealedPath := manager.specialistProvider.Path
	if sealedPath == filepath.Clean(provider) || !strings.HasPrefix(sealedPath, filepath.Join(root, ".provider-seals")) {
		t.Fatalf("provider was not sealed beneath the private manager root: %q", sealedPath)
	}
	if err := os.WriteFile(provider, []byte("replaced-source"), 0o755); err != nil {
		t.Fatal(err)
	}
	agent := manager.agents["quality"]
	agent.specialistReady = true
	resolved, err := manager.specialistBackendBinary(agent)
	if err != nil || resolved != sealedPath || manager.specialistProvider.SHA256 != originalDigest {
		t.Fatalf("sealed provider drifted with its source: resolved=%q err=%v", resolved, err)
	}
	if err := os.Chmod(sealedPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sealedPath, []byte("mutated-seal"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.specialistBackendBinary(agent); err == nil || !strings.Contains(err.Error(), "changed before launch") {
		t.Fatalf("mutated sealed provider was accepted: %v", err)
	}
}

func TestShutdownSpecialistsKillsOnlyExactNamespacedSpecialistSessions(t *testing.T) {
	configs := testSpecialistConfigs()
	configs["supervisor"] = config.AgentConfig{Enabled: true, Backend: "codex", Mode: "ADVISORY"}
	manager := testSpecialistManager(t, configs)
	for _, name := range []string{"quality", "scanner", "supervisor"} {
		agent := manager.agents[name]
		agent.State = StateRunning
		agent.HasLaunched = true
		agent.LaunchedMode = ModeAdvisory
		agent.specialistReady = true
		manager.specialistLeases[name] = "swo-" + strings.Repeat(string(name[0]), 64)
	}
	var killed []string
	err := manager.shutdownSpecialists(context.Background(), func(ctx context.Context, agent *AgentProcess) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("shutdown command has no timeout")
		}
		killed = append(killed, agent.Name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(killed, ",") != "architect,ci-maintainer,quality,scanner,sec-check" {
		t.Fatalf("killed sessions = %v", killed)
	}
	if manager.agents["quality"].State != StateStopped || manager.agents["scanner"].State != StateStopped {
		t.Fatal("prepared specialists were not stopped")
	}
	if manager.agents["supervisor"].State != StateRunning {
		t.Fatal("non-specialist session was stopped")
	}
	if len(manager.specialistLeases) != 1 || manager.specialistLeases["supervisor"] == "" {
		t.Fatalf("shutdown touched non-specialist lease or retained specialist lease: %+v", manager.specialistLeases)
	}
	if err := manager.shutdownSpecialists(context.Background(), func(context.Context, *AgentProcess) error {
		t.Fatal("idempotent shutdown killed twice")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	runtime, _ := fakeSpecialistRuntime(manager)
	if _, err := manager.prepareSpecialist(context.Background(), SpecialistQuality, specialistReadinessLeasePrefix+"quality", runtime); err == nil || !strings.Contains(err.Error(), "shut down") {
		t.Fatalf("shutdown manager accepted new specialist work: %v", err)
	}
}

type specialistRuntimeCounters struct {
	starts     int
	restarts   int
	sends      int
	configures int
	lastAgent  string
	lastTask   string
	lastKick   string
}

func fakeSpecialistRuntime(manager *Manager) (specialistDispatchRuntime, *specialistRuntimeCounters) {
	counters := &specialistRuntimeCounters{}
	markRunning := func(name string) {
		manager.mu.Lock()
		agent := manager.agents[name]
		agent.State = StateRunning
		agent.HasLaunched = true
		agent.LaunchedMode = manager.agentMode(agent)
		manager.mu.Unlock()
	}
	return specialistDispatchRuntime{
		start: func(_ context.Context, name string) error {
			counters.starts++
			markRunning(name)
			return nil
		},
		restart: func(_ context.Context, name string) error {
			counters.restarts++
			markRunning(name)
			return nil
		},
		send: func(name, message, taskID string) error {
			manager.mu.RLock()
			leased := manager.specialistLeases[name]
			manager.mu.RUnlock()
			if leased != taskID {
				return errors.New("fake observed missing task lease")
			}
			counters.sends++
			counters.lastAgent = name
			counters.lastTask = taskID
			counters.lastKick = message
			return nil
		},
		configure: func(agent *AgentProcess) error {
			if !agent.specialistReady {
				return errors.New("fake observed unisolated specialist")
			}
			counters.configures++
			return nil
		},
		resolve: func(agent *AgentProcess) (string, error) {
			if effectiveBackend(agent) != "codex" {
				return "", errors.New("unexpected backend")
			}
			return "/opt/codex", nil
		},
	}, counters
}

func testSpecialistConfigs() map[string]config.AgentConfig {
	result := make(map[string]config.AgentConfig, len(specialistRoleAllowlist))
	for role := range specialistRoleAllowlist {
		result[string(role)] = config.AgentConfig{Enabled: true, Backend: "codex", Mode: "ADVISORY", Role: string(role)}
	}
	return result
}

func testSpecialistManager(t *testing.T, configs map[string]config.AgentConfig) *Manager {
	t.Helper()
	manager := newManager(configs, testSpecialistLogger(), ProjectContext{ACMMLevel: 6}, filepath.Join(t.TempDir(), "agents"))
	manager.specialistNamespace = "testnamespace"
	manager.specialistCodexHome = filepath.Join(manager.workDir, ".codex-homes", "manager-test")
	if err := os.MkdirAll(manager.specialistCodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manager.specialistCodexHome, "auth.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, process := range manager.agents {
		if err := os.MkdirAll(filepath.Join(manager.workDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		process.specialistProviderSHA256 = strings.Repeat("a", 64)
		process.tmuxSession = "hive-" + name + "-" + manager.specialistNamespace
	}
	return manager
}

func testSpecialistProvider(t *testing.T) string {
	t.Helper()
	name := "codex"
	if runtime.GOOS == "windows" {
		name = "codex.exe"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("native-placeholder"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testSpecialistCodexHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func testSpecialistLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestSpecialistShutdownAggregatesFailures(t *testing.T) {
	manager := testSpecialistManager(t, map[string]config.AgentConfig{
		"quality": {Enabled: true, Backend: "codex", Mode: "ADVISORY"},
	})
	agent := manager.agents["quality"]
	agent.specialistReady = true
	agent.State = StateRunning
	err := manager.shutdownSpecialists(context.Background(), func(context.Context, *AgentProcess) error {
		return errors.New("bounded kill failed")
	})
	if err == nil || !strings.Contains(err.Error(), "quality") || !strings.Contains(err.Error(), "bounded kill failed") {
		t.Fatalf("shutdown error = %v", err)
	}
	if agent.State != StateRunning {
		t.Fatal("failed shutdown falsely marked session stopped")
	}
}

func TestSpecialistCommandTimeoutIsBounded(t *testing.T) {
	if specialistCommandTimeout <= 0 || specialistCommandTimeout > time.Minute {
		t.Fatalf("specialist command timeout = %s", specialistCommandTimeout)
	}
}
