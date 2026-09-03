package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// ---------------------------------------------------------------------------
// routableBackend + SetGatewayBackendChecker (atomic, lock-free)
// ---------------------------------------------------------------------------

func TestRoutableBackend_InferenceBuiltin(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	// A built-in inference backend is routable without any gateway checker.
	if !m.routableBackend("litellm") {
		t.Error("litellm should be routable")
	}
	// A non-inference backend with no checker is not routable.
	if m.routableBackend("claude") {
		t.Error("claude should not be routable without gateway checker")
	}
}

func TestRoutableBackend_GatewayChecker(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	m.SetGatewayBackendChecker(func(backend string) bool {
		return backend == "my-gateway"
	})
	if !m.routableBackend("my-gateway") {
		t.Error("my-gateway should be routable via checker")
	}
	if m.routableBackend("other") {
		t.Error("other should not be routable")
	}
}

func TestRoutableBackend_NilCheckerStored(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	// Store a nil function pointer — routableBackend must treat it as no checker.
	var fn func(string) bool
	m.SetGatewayBackendChecker(fn)
	if m.routableBackend("claude") {
		t.Error("nil checker should not make claude routable")
	}
}

// ---------------------------------------------------------------------------
// SetModelOverride / SetBackendOverride with inference callbacks
// ---------------------------------------------------------------------------

func TestSetModelOverride_FiresInferenceCallback(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "litellm", Model: "deepseek"},
	}, discardLogger(), ProjectContext{})

	var gotName, gotBackend, gotModel string
	m.SetInferenceCallbacks(func(name, backend, model string) {
		gotName, gotBackend, gotModel = name, backend, model
	}, func(string) {})

	if err := m.SetModelOverride("a", "qwen"); err != nil {
		t.Fatalf("SetModelOverride: %v", err)
	}
	if gotName != "a" || gotBackend != "litellm" || gotModel != "qwen" {
		t.Errorf("callback got (%q,%q,%q)", gotName, gotBackend, gotModel)
	}
}

func TestSetModelOverride_RetargetsPin(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude"},
	}, discardLogger(), ProjectContext{})
	if err := m.PinModel("a", "opus"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetModelOverride("a", "sonnet"); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.agents["a"].PinnedModel != "sonnet" {
		t.Errorf("pin should retarget to sonnet, got %q", m.agents["a"].PinnedModel)
	}
}

func TestSetBackendOverride_RoutesInference(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Model: "opus"},
	}, discardLogger(), ProjectContext{})
	var routed bool
	m.SetInferenceCallbacks(func(name, backend, model string) { routed = true }, func(string) {})
	if err := m.SetBackendOverride("a", "vllm"); err != nil {
		t.Fatal(err)
	}
	if !routed {
		t.Error("switching to vllm should fire inference route callback")
	}
}

func TestSetBackendOverride_ClearsRouteOnNonInference(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "vllm", Model: "x"},
	}, discardLogger(), ProjectContext{})
	var cleared string
	m.SetInferenceCallbacks(func(string, string, string) {}, func(name string) { cleared = name })
	if err := m.SetBackendOverride("a", "copilot"); err != nil {
		t.Fatal(err)
	}
	if cleared != "a" {
		t.Errorf("switching to copilot should clear inference route, got %q", cleared)
	}
}

func TestSetModelOverride_UsesModelOverrideForBackendOverride(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Model: "cfg-model"},
	}, discardLogger(), ProjectContext{})
	var gotModel string
	m.SetInferenceCallbacks(func(_, _, model string) { gotModel = model }, func(string) {})
	// Set backend override to inference with no model override -> uses config model.
	if err := m.SetBackendOverride("a", "litellm"); err != nil {
		t.Fatal(err)
	}
	if gotModel != "cfg-model" {
		t.Errorf("expected cfg-model, got %q", gotModel)
	}
}

// ---------------------------------------------------------------------------
// RefreshInferenceRoutes
// ---------------------------------------------------------------------------

func TestRefreshInferenceRoutes(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "litellm", Model: "m1"},
		"b": {Backend: "claude", Model: "m2"},
		"c": {Backend: "litellm", Model: "m3"},
	}, discardLogger(), ProjectContext{})
	fired := map[string]bool{}
	m.SetInferenceCallbacks(func(name, _, _ string) { fired[name] = true }, func(string) {})

	m.RefreshInferenceRoutes("litellm")
	if !fired["a"] || !fired["c"] {
		t.Errorf("both litellm agents should refresh, got %v", fired)
	}
	if fired["b"] {
		t.Error("claude agent b should not refresh for litellm")
	}
}

func TestRefreshInferenceRoutes_NoCallback(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "litellm"}}, discardLogger(), ProjectContext{})
	// No callback set — should be a no-op, no panic.
	m.RefreshInferenceRoutes("litellm")
}

func TestRefreshInferenceRoutes_NonRoutable(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	fired := false
	m.SetInferenceCallbacks(func(string, string, string) { fired = true }, func(string) {})
	m.RefreshInferenceRoutes("claude") // not routable -> no-op
	if fired {
		t.Error("non-routable backend should not fire refresh")
	}
}

func TestRefreshInferenceRoutes_BackendOverrideMatch(t *testing.T) {
	m := NewManager(map[string]config.AgentConfig{
		"a": {Backend: "claude", Model: "m1"},
	}, discardLogger(), ProjectContext{})
	var gotModel string
	m.SetInferenceCallbacks(func(_, _, model string) { gotModel = model }, func(string) {})
	if err := m.SetBackendOverride("a", "vllm"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetModelOverride("a", "override-model"); err != nil {
		t.Fatal(err)
	}
	gotModel = ""
	m.RefreshInferenceRoutes("vllm")
	if gotModel != "override-model" {
		t.Errorf("refresh should use model override, got %q", gotModel)
	}
}

// ---------------------------------------------------------------------------
// toolRulesToLaunchCmd
// ---------------------------------------------------------------------------

func denyTools(patterns ...string) *config.ToolsConfig {
	rules := make([]config.ToolRule, len(patterns))
	for i, p := range patterns {
		rules[i] = config.ToolRule{Pattern: p, Action: "deny"}
	}
	return &config.ToolsConfig{Rules: rules}
}

func TestToolRulesToLaunchCmd_Claude(t *testing.T) {
	tools := denyTools("mcp__github__merge_pull_request")
	cmd := toolRulesToLaunchCmd("claude", "opus", "claude", tools, false)
	if !containsBoot(cmd, "--model opus") || !containsBoot(cmd, "--dangerously-skip-permissions") {
		t.Errorf("claude cmd missing flags: %q", cmd)
	}
	if !containsBoot(cmd, "--disallowed-tools") {
		t.Errorf("claude cmd should include deny: %q", cmd)
	}
}

func TestToolRulesToLaunchCmd_ClaudeInference(t *testing.T) {
	tools := &config.ToolsConfig{}
	cmd := toolRulesToLaunchCmd("claude", "m", "claude", tools, true)
	if !containsBoot(cmd, "--bare") || !containsBoot(cmd, claudeInferenceSettingsPath) {
		t.Errorf("inference claude cmd should have --bare --settings: %q", cmd)
	}
}

func TestToolRulesToLaunchCmd_Copilot(t *testing.T) {
	// Authorship fix: copilot must NEVER pass --enable-all-github-mcp-tools —
	// the built-in github-mcp-server is read-only by default, so GitHub MCP
	// WRITE tools are never registered and an agent cannot author issues/PRs as
	// the logged-in user. (This test previously asserted the OPPOSITE — that the
	// flag was present — which was the bug: it let agents author as the user.)
	tools := denyTools("mcp__something__else")
	cmd := toolRulesToLaunchCmd("copilot", "auto", "copilot", tools, false)
	if containsBoot(cmd, "--enable-all-github-mcp-tools") {
		t.Errorf("copilot must NOT enable all github mcp tools (read-only default): %q", cmd)
	}
	if !containsBoot(cmd, "--deny-tool=") {
		t.Errorf("copilot cmd should include deny-tool: %q", cmd)
	}
}

func TestToolRulesToLaunchCmd_CopilotGithubDeny(t *testing.T) {
	tools := denyTools("mcp__github__create_pull_request")
	cmd := toolRulesToLaunchCmd("copilot", "auto", "copilot", tools, false)
	if containsBoot(cmd, "--enable-all-github-mcp-tools") {
		t.Errorf("copilot with github deny should NOT enable all github tools: %q", cmd)
	}
	// Deny must use the built-in server's REAL name github-mcp-server, not the
	// bogus `github` name (which was a silent no-op — the source of the leak).
	if !containsBoot(cmd, "github-mcp-server(create_pull_request)") {
		t.Errorf("copilot deny should translate to github-mcp-server(tool): %q", cmd)
	}
	if containsBoot(cmd, "--deny-tool='github(") {
		t.Errorf("copilot deny must NOT use the wrong `github(` server name: %q", cmd)
	}
}

func TestToolRulesToLaunchCmd_Default(t *testing.T) {
	tools := &config.ToolsConfig{}
	cmd := toolRulesToLaunchCmd("gemini", "flash", "gemini", tools, false)
	if cmd != "gemini --model flash" {
		t.Errorf("default backend cmd: %q", cmd)
	}
	// An UNKNOWN backend with no model gets the bare binary. This used to be
	// asserted with backend "bob", which meant it pinned bob's broken
	// fall-through: a bare `bob` has no --accept-license (hard-errors), no
	// auth flag, and no --approval-mode (stalls on the first tool call). bob
	// now has its own branch, so use a backend that really is unknown.
	cmd = toolRulesToLaunchCmd("mystery", "", "mystery", tools, false)
	if cmd != "mystery" {
		t.Errorf("default backend with empty model: %q", cmd)
	}
}

// ---------------------------------------------------------------------------
// connectionMCPFlags
// ---------------------------------------------------------------------------

func TestConnectionMCPFlags_Claude(t *testing.T) {
	conns := []config.ConnectionConfig{
		{Type: "mcp", URI: "https://mcp.example.com"},
		{Type: "http", URI: "https://ignored"},
		{Type: "mcp", URI: ""},
	}
	flags := connectionMCPFlags(conns, "claude")
	if !containsBoot(flags, "--mcp-server 'https://mcp.example.com'") {
		t.Errorf("expected mcp-server flag, got %q", flags)
	}
}

func TestConnectionMCPFlags_NonClaude(t *testing.T) {
	conns := []config.ConnectionConfig{{Type: "mcp", URI: "https://x"}}
	if flags := connectionMCPFlags(conns, "copilot"); flags != "" {
		t.Errorf("copilot should produce no mcp flags, got %q", flags)
	}
}

// ---------------------------------------------------------------------------
// normalizeModelName
// ---------------------------------------------------------------------------

func TestNormalizeModelName(t *testing.T) {
	cases := []struct{ model, backend, want string }{
		{"opus", "claude", "opus"},                // claude passes through
		{"deepseek-14", "litellm", "deepseek-14"}, // inference passes through
		// copilot no longer takes the blind digit-suffix dot-rewrite (#4262):
		// ids are canonicalized against the CLI-accepted list and UNKNOWN ids
		// pass through verbatim — the rewrite is what corrupted the known
		// claude-fable-5 into the CLI-rejected claude-fable.5.
		{"gpt-4o-2024", "copilot", "gpt-4o-2024"},        // unknown id passthrough
		{"claude-fable-5", "copilot", "claude-fable-5"},  // dashed family kept dashed
		{"claude-fable.5", "copilot", "claude-fable-5"},  // stored bad id self-corrects
		{"claude-opus-4-6", "copilot", "claude-opus-4.6"}, // known dotted family still mapped
		{"gpt-4o-2024", "gemini", "gpt-4o.2024"},        // digit suffix -> dot (non-copilot)
		{"gpt-4o-preview", "copilot", "gpt-4o-preview"}, // non-digit suffix unchanged
		{"nohyphen", "copilot", "nohyphen"},             // no hyphen
		{"trailing-", "copilot", "trailing-"},           // trailing hyphen
	}
	for _, c := range cases {
		if got := normalizeModelName(c.model, c.backend); got != c.want {
			t.Errorf("normalizeModelName(%q,%q)=%q want %q", c.model, c.backend, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Pause / Resume with persist callback
// ---------------------------------------------------------------------------

func TestPauseResume_PersistCallback(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	var events []bool
	m.SetPersistPauseCallback(func(name string, paused bool) {
		events = append(events, paused)
	})

	if err := m.Pause("a", "dashboard-api", "operator"); err != nil {
		t.Fatal(err)
	}
	if !m.IsPaused("a") {
		t.Error("agent should be paused")
	}
	if err := m.Resume(context.Background(), "a", "dashboard-api", "unpause"); err != nil {
		t.Fatal(err)
	}
	defer cleanupAgent(t, m, "a")
	if m.IsPaused("a") {
		t.Error("agent should be resumed")
	}
	if len(events) != 2 || events[0] != true || events[1] != false {
		t.Errorf("persist callback events = %v", events)
	}
}

func TestResume_RelaunchFromPaused(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "claude"}}, discardLogger(), ProjectContext{})
	m.mu.Lock()
	m.agents["a"].State = StatePaused
	m.agents["a"].Paused = true
	m.mu.Unlock()
	// Resume triggers ensureTmuxSession + launchInTmux. Real tmux + stub claude.
	err := m.Resume(context.Background(), "a", "test", "resume")
	if err != nil {
		t.Fatalf("Resume relaunch: %v", err)
	}
	cleanupAgent(t, m, "a")
}

// ---------------------------------------------------------------------------
// seedJSONFile / mergeApprovedAPIKeys (temp-file seam)
// ---------------------------------------------------------------------------

func TestSeedJSONFile_CreatesAndMerges(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	seed := map[string]any{"foo": "bar", "n": float64(1)}
	m.seedJSONFile("agent", path, seed)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if !containsBoot(string(data), "\"foo\"") {
		t.Errorf("seed not written: %s", data)
	}

	// Existing key not overwritten.
	if err := os.WriteFile(path, []byte(`{"foo":"original"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m.seedJSONFile("agent", path, map[string]any{"foo": "new", "added": true})
	data, _ = os.ReadFile(path)
	if !containsBoot(string(data), "original") {
		t.Errorf("existing key should not be overwritten: %s", data)
	}
	if !containsBoot(string(data), "added") {
		t.Errorf("missing key should be added: %s", data)
	}
}

func TestSeedJSONFile_UnparseableRewritten(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.seedJSONFile("agent", path, map[string]any{"k": "v"})
	data, _ := os.ReadFile(path)
	if !containsBoot(string(data), "\"k\"") {
		t.Errorf("unparseable file should be rewritten from seed: %s", data)
	}
}

func TestSeedJSONFile_CompleteNoWrite(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	dir := t.TempDir()
	path := filepath.Join(dir, "complete.json")
	orig := `{"k":"v"}`
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	m.seedJSONFile("agent", path, map[string]any{"k": "other"})
	data, _ := os.ReadFile(path)
	if string(data) != orig {
		t.Errorf("complete file should be untouched, got %s", data)
	}
}

func TestMergeApprovedAPIKeys_NoFile(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	// Nonexistent path -> silent return.
	m.mergeApprovedAPIKeys("agent", filepath.Join(t.TempDir(), "missing.json"))
}

func TestMergeApprovedAPIKeys_AddsSeed(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	dir := t.TempDir()
	path := filepath.Join(dir, "claude.json")
	// Start with an empty approved list.
	if err := os.WriteFile(path, []byte(`{"customApiKeyResponses":{"approved":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m.mergeApprovedAPIKeys("scanner", path)
	data, _ := os.ReadFile(path)
	// The seed for "scanner" is sk-hive-scanner (first 20 chars) — just verify
	// the file gained an approved entry.
	if !containsBoot(string(data), "sk-hive") {
		t.Errorf("expected seeded key merged in: %s", data)
	}
}

func TestMergeApprovedAPIKeys_MalformedShapes(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	dir := t.TempDir()

	// Invalid JSON -> return.
	p1 := filepath.Join(dir, "bad.json")
	os.WriteFile(p1, []byte("{bad"), 0o644)
	m.mergeApprovedAPIKeys("a", p1)

	// Missing customApiKeyResponses -> return.
	p2 := filepath.Join(dir, "nokey.json")
	os.WriteFile(p2, []byte(`{"other":1}`), 0o644)
	m.mergeApprovedAPIKeys("a", p2)
}

// ---------------------------------------------------------------------------
// ensureClaudeSettings — /tmp-based inference home (writable seam)
// ---------------------------------------------------------------------------

func TestEnsureClaudeSettings(t *testing.T) {
	name := "cov-inference-test"
	home := inferenceHomePath(name)
	t.Cleanup(func() {
		os.RemoveAll(home)
		os.Remove(claudeInferenceSettingsPath)
	})
	m := NewManager(nil, discardLogger(), ProjectContext{})
	m.ensureClaudeSettings(name, 0)

	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Errorf("settings.json not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); err != nil {
		t.Errorf(".claude.json not created: %v", err)
	}
	// Second call repairs/no-ops without error.
	m.ensureClaudeSettings(name, 0)
}

// ---------------------------------------------------------------------------
// AgentMode.TokenTier and helpers
// ---------------------------------------------------------------------------

func TestAgentModeTokenTier(t *testing.T) {
	cases := map[AgentMode]string{
		ModeAdvisory:       "advisor",
		ModeIssuesOnly:     "newcomer",
		ModeIssuesAndPRs:   "contributor",
		ModeIssuesPRsMerge: "trusted",
		AgentMode(99):      "advisor",
	}
	for mode, want := range cases {
		if got := mode.TokenTier(); got != want {
			t.Errorf("mode %d TokenTier=%q want %q", mode, got, want)
		}
	}
}

// TestToolRulesToLaunchCmd_BobNeverGetsModel verifies bob's launch command
// carries no --model even when a concrete model is configured.
//
// Agents with Config.Tools set bypass bobLaunchCmd and build their command
// here, so without an explicit bob branch they fell into the default case and
// got `bob --model <id>`. That is the crash: normalizeModelName rewrites a
// trailing -<digits> to .<digits> for every non-claude backend, so a stored
// claude-sonnet-4-6 reaches bob as claude-sonnet-4.6 — an id bob's backend
// does not know — and every prompt dies with "Cannot read properties of
// undefined (reading 'maxTokens')". bob auto-selects its own model.
//
// The assertion is deliberately about the ABSENCE of --model and the INVARIANCE
// of the command across models, not equality against a fixed string. bob's
// launch command legitimately carries required flags (--accept-license,
// --auth-method api-key, --approval-mode yolo, --trust) that this test must not
// re-litigate; bobLaunchCmd's own doc comment and
// TestToolRulesToLaunchCmdBobHasNoModel in bob_test.go own that contract and
// assert those flags are present. An earlier `cmd != "bob"` check here duplicated
// that ownership badly: it pinned the whole command to the bare binary name and
// so contradicted the sibling test the moment a required flag was added.
func TestToolRulesToLaunchCmd_BobNeverGetsModel(t *testing.T) {
	tools := &config.ToolsConfig{}
	// Includes "auto": the dashboard's only bob option is a display/stored
	// placeholder, and `bob --model auto` is untested and must not be emitted.
	models := []string{"", "auto", "claude-sonnet-4.6", "claude-sonnet-4-6", "granite-3"}

	// The command bob gets must not vary with the configured model at all, so
	// every iteration is compared against the first one rather than a literal.
	want := toolRulesToLaunchCmd("bob", models[0], bobBackend, tools, false)
	for _, model := range models {
		cmd := toolRulesToLaunchCmd("bob", model, bobBackend, tools, false)
		if strings.Contains(cmd, "--model") {
			t.Errorf("bob launch cmd with model %q must not contain --model: %q", model, cmd)
		}
		// Catches a model leaking in under any spelling, not just as --model.
		if model != "" && strings.Contains(cmd, model) {
			t.Errorf("bob launch cmd with model %q leaked the model id: %q", model, cmd)
		}
		if cmd != want {
			t.Errorf("bob launch cmd with model %q = %q, want it identical to the no-model command %q", model, cmd, want)
		}
	}
}

// TestToolRulesToLaunchCmd_OtherBackendsStillGetModel guards the bob branch
// against swallowing the default case for everything else.
func TestToolRulesToLaunchCmd_OtherBackendsStillGetModel(t *testing.T) {
	tools := &config.ToolsConfig{}
	for _, backend := range []string{"gemini", "goose", "codex"} {
		cmd := toolRulesToLaunchCmd(backend, "some-model", backend, tools, false)
		if !strings.Contains(cmd, "--model some-model") {
			t.Errorf("%s must still receive --model: %q", backend, cmd)
		}
	}
}
