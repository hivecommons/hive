package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// These tests redirect the package-level path vars (a minimal testability seam:
// const->var, no logic/mutex/launch-path change) to temp files so the config,
// token, metrics, and permissions helpers can run their happy/error paths
// without a real /data volume.

func withSharedPaths(t *testing.T, copilotCfg, claudeCred string) {
	t.Helper()
	origC, origCl := sharedCopilotConfigPath, sharedClaudeCredentialPath
	sharedCopilotConfigPath = copilotCfg
	sharedClaudeCredentialPath = claudeCred
	t.Cleanup(func() {
		sharedCopilotConfigPath = origC
		sharedClaudeCredentialPath = origCl
	})
}

// ---------------------------------------------------------------------------
// copilotConfigHasTokens — parse paths (comments stripped, token presence).
// ---------------------------------------------------------------------------

func TestCopilotConfigHasTokens_WithTokens(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	// Includes a // comment line (Copilot CLI writes these) + a token entry.
	content := "// managed file\n{\n  \"copilotTokens\": {\"user\": \"gho_abc\"}\n}\n"
	if err := os.WriteFile(cfg, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	withSharedPaths(t, cfg, filepath.Join(dir, "nope-cred.json"))
	if !copilotConfigHasTokens() {
		t.Error("expected tokens present")
	}
}

func TestCopilotConfigHasTokens_EmptyTokens(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte(`{"copilotTokens":{}}`), 0o644)
	withSharedPaths(t, cfg, filepath.Join(dir, "x"))
	if copilotConfigHasTokens() {
		t.Error("empty tokens map should be false")
	}
}

func TestCopilotConfigHasTokens_BadShapes(t *testing.T) {
	dir := t.TempDir()

	// Invalid JSON.
	p1 := filepath.Join(dir, "bad.json")
	os.WriteFile(p1, []byte("{not json"), 0o644)
	withSharedPaths(t, p1, filepath.Join(dir, "x1"))
	if copilotConfigHasTokens() {
		t.Error("invalid json -> false")
	}

	// Missing copilotTokens key.
	p2 := filepath.Join(dir, "nokey.json")
	os.WriteFile(p2, []byte(`{"other":1}`), 0o644)
	sharedCopilotConfigPath = p2
	if copilotConfigHasTokens() {
		t.Error("missing copilotTokens -> false")
	}

	// copilotTokens not a map.
	p3 := filepath.Join(dir, "wrongtype.json")
	os.WriteFile(p3, []byte(`{"copilotTokens":"str"}`), 0o644)
	sharedCopilotConfigPath = p3
	if copilotConfigHasTokens() {
		t.Error("non-map copilotTokens -> false")
	}
}

// ---------------------------------------------------------------------------
// clearExpiredTokens — rewrites config with emptied token fields.
// ---------------------------------------------------------------------------

func TestClearExpiredTokens_Rewrites(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	content := "// comment\n{\n  \"copilotTokens\": {\"u\": \"gho_x\"},\n  \"loggedInUsers\": [\"u\"],\n  \"lastLoggedInUser\": \"u\"\n}\n"
	if err := os.WriteFile(cfg, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	withSharedPaths(t, cfg, filepath.Join(dir, "x"))

	if err := clearExpiredTokens(); err != nil {
		t.Fatalf("clearExpiredTokens: %v", err)
	}
	data, _ := os.ReadFile(cfg)
	var parsed map[string]any
	// Strip the leading comment lines before parsing.
	if err := json.Unmarshal(stripComments(data), &parsed); err != nil {
		t.Fatalf("rewritten config unparseable: %v", err)
	}
	toks, _ := parsed["copilotTokens"].(map[string]any)
	if len(toks) != 0 {
		t.Errorf("copilotTokens should be emptied, got %v", toks)
	}
	// Login identity SURVIVES a token clear: an expired token does not change
	// who was logged in, and the interactive CLI refuses a re-seeded token
	// without it (hivecommons/hive, 2026-08-22 — agents at "Please use /login"
	// over a valid restored token).
	if parsed["lastLoggedInUser"] != "u" {
		t.Errorf("lastLoggedInUser must be preserved, got %v", parsed["lastLoggedInUser"])
	}
}

func TestClearExpiredTokens_ReadError(t *testing.T) {
	dir := t.TempDir()
	withSharedPaths(t, filepath.Join(dir, "missing.json"), filepath.Join(dir, "x"))
	if err := clearExpiredTokens(); err == nil {
		t.Error("expected read error for missing config")
	}
}

func TestClearExpiredTokens_ParseError(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte("{broken"), 0o644)
	withSharedPaths(t, cfg, filepath.Join(dir, "x"))
	if err := clearExpiredTokens(); err == nil {
		t.Error("expected parse error")
	}
}

// stripComments removes leading // comment lines like copilotConfigHasTokens does.
func stripComments(data []byte) []byte {
	var out []byte
	for _, line := range splitLines(string(data)) {
		if len(line) >= 2 && line[0] == '/' && line[1] == '/' {
			continue
		}
		out = append(out, []byte(line+"\n")...)
	}
	return out
}

func splitLines(s string) []string {
	var lines []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			lines = append(lines, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

// ---------------------------------------------------------------------------
// configHasTokens — claude credential present short-circuits to true.
// ---------------------------------------------------------------------------

func TestConfigHasTokens_CopilotFallback(t *testing.T) {
	dir := t.TempDir()
	copilotCfg := filepath.Join(dir, "config.json")
	os.WriteFile(copilotCfg, []byte(`{"copilotTokens":{"u":"gho"}}`), 0o644)
	// Claude cred missing -> falls through to copilotConfigHasTokens (true).
	withSharedPaths(t, copilotCfg, filepath.Join(dir, "no-claude.json"))
	if !configHasTokens() {
		t.Error("expected true via copilot fallback")
	}
}

// ---------------------------------------------------------------------------
// fixSharedConfigPerms — file present with wrong perms -> chmod branch.
// ---------------------------------------------------------------------------

func TestFixSharedConfigPerms_ChmodsWrongPerms(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte("{}"), 0o600) // wrong perms (want 0660)
	withSharedPaths(t, cfg, filepath.Join(dir, "x"))

	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "copilot"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	m.fixSharedConfigPerms(agent)

	info, err := os.Stat(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != sharedConfigDesiredMode {
		t.Errorf("perms = %04o, want %04o", info.Mode().Perm(), sharedConfigDesiredMode)
	}
}

func TestFixSharedConfigPerms_AlreadyCorrectSeam(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	os.WriteFile(cfg, []byte("{}"), sharedConfigDesiredMode)
	withSharedPaths(t, cfg, filepath.Join(dir, "x"))
	m := NewManager(map[string]config.AgentConfig{"a": {Backend: "copilot"}}, discardLogger(), ProjectContext{})
	m.mu.RLock()
	agent := m.agents["a"]
	m.mu.RUnlock()
	m.fixSharedConfigPerms(agent) // correct perms -> early return
}

// ---------------------------------------------------------------------------
// readCoveragePreamble — via redirected metricsCachePath.
// ---------------------------------------------------------------------------

func TestReadCoveragePreamble_Valid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "metrics.json")
	os.WriteFile(p, []byte(`{"ci-maintainer":{"coverage":85,"coverageTarget":92}}`), 0o644)
	orig := metricsCachePath
	metricsCachePath = p
	t.Cleanup(func() { metricsCachePath = orig })

	got := (&Manager{}).readCoveragePreamble()
	if got == "" {
		t.Error("expected a coverage preamble")
	}
	if !containsBoot(got, "85") || !containsBoot(got, "92") {
		t.Errorf("preamble = %q", got)
	}
}

func TestReadCoveragePreamble_MissingTargetDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.json")
	os.WriteFile(p, []byte(`{"ci-maintainer":{"coverage":70}}`), 0o644)
	orig := metricsCachePath
	metricsCachePath = p
	t.Cleanup(func() { metricsCachePath = orig })
	if got := (&Manager{}).readCoveragePreamble(); !containsBoot(got, "91") {
		t.Errorf("expected default target 91, got %q", got)
	}
}

func TestReadCoveragePreamble_BadInputs(t *testing.T) {
	orig := metricsCachePath
	t.Cleanup(func() { metricsCachePath = orig })
	m := &Manager{}

	dir := t.TempDir()
	// Invalid JSON.
	p1 := filepath.Join(dir, "bad.json")
	os.WriteFile(p1, []byte("{bad"), 0o644)
	metricsCachePath = p1
	if m.readCoveragePreamble() != "" {
		t.Error("invalid json -> empty")
	}
	// Missing ci-maintainer key.
	p2 := filepath.Join(dir, "nokey.json")
	os.WriteFile(p2, []byte(`{"other":{"coverage":1}}`), 0o644)
	metricsCachePath = p2
	if m.readCoveragePreamble() != "" {
		t.Error("missing ci-maintainer -> empty")
	}
	// Missing coverage field.
	p3 := filepath.Join(dir, "nocov.json")
	os.WriteFile(p3, []byte(`{"ci-maintainer":{"other":1}}`), 0o644)
	metricsCachePath = p3
	if m.readCoveragePreamble() != "" {
		t.Error("missing coverage -> empty")
	}
}

// ---------------------------------------------------------------------------
// Permissions watcher against a writable temp tree.
// ---------------------------------------------------------------------------

func TestPermissionsWatcher_TempTree(t *testing.T) {
	dir := t.TempDir()
	watched := filepath.Join(dir, "home")
	if err := os.MkdirAll(filepath.Join(watched, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(watched, "sub", "f"), []byte("x"), 0o600)

	origW, origG := WatchedHomeDirs, GooseLogsDir
	WatchedHomeDirs = []string{watched}
	GooseLogsDir = filepath.Join(dir, "goose", "logs")
	t.Cleanup(func() { WatchedHomeDirs = origW; GooseLogsDir = origG })

	ensureWatchedDirs(discardLogger())
	fixPermissions(discardLogger())

	if _, err := os.Stat(GooseLogsDir); err != nil {
		t.Errorf("goose logs dir should be created: %v", err)
	}
}

func TestPermissionsWatcher_MissingRootRecreates(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "not-created-yet")
	origW := WatchedHomeDirs
	WatchedHomeDirs = []string{missing}
	t.Cleanup(func() { WatchedHomeDirs = origW })
	// fixPermissions sees the root doesn't exist -> calls ensureWatchedDirs.
	fixPermissions(discardLogger())
	if _, err := os.Stat(missing); err != nil {
		t.Errorf("missing watched dir should be recreated: %v", err)
	}
}
