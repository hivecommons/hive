package agent

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// Fake Copilot configs. Every token value carries this sentinel so a test can
// prove no log line ever leaks token material. No real token appears here.
const copilotBridgeTestSentinel = "FAKE-COPILOT-SENTINEL"

const (
	copilotBridgeTestLoggedIn  = `{"copilotTokens":{"https://github.com:fake-user":"gho_` + copilotBridgeTestSentinel + `"}}`
	copilotBridgeTestOlder     = `{"copilotTokens":{"https://github.com:fake-user":"gho_older_` + copilotBridgeTestSentinel + `"}}`
	copilotBridgeTestNoLogin   = `{"copilotTokens":{}}`
	copilotBridgeTestAgentName = "scanner"
	copilotBridgeTestUID       = 1001
)

// copilotBridgeFixture provisions a per-agent home for a copilot agent with the
// shared login in place and config.json bridged. It returns the manager, the
// log buffer, the agent, the link and the shared target.
func copilotBridgeFixture(t *testing.T) (*Manager, *bytes.Buffer, *AgentProcess, string, string) {
	t.Helper()
	shared := withSharedAgentHome(t)
	withCopilotBridgeClock(t, time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC))
	copilotBridgeWarned.Delete(filepath.Join(interactiveHomePath(copilotBridgeTestAgentName), copilotHomeDirName, copilotConfigFileName))
	var buf bytes.Buffer
	m := NewManager(map[string]config.AgentConfig{}, slog.New(slog.NewTextHandler(&buf, nil)), ProjectContext{})
	ap := &AgentProcess{Name: copilotBridgeTestAgentName, UID: copilotBridgeTestUID, Config: config.AgentConfig{Backend: "copilot"}}
	m.mu.Lock()
	m.agents[ap.Name] = ap
	m.mu.Unlock()
	target := filepath.Join(shared, copilotHomeDirName, copilotConfigFileName)
	writeCopilotBridgeFile(t, target, copilotBridgeTestLoggedIn)
	m.setupInteractiveHome(ap, "copilot")
	link := filepath.Join(interactiveHomePath(ap.Name), copilotHomeDirName, copilotConfigFileName)
	assertCopilotLinked(t, link, target)
	return m, &buf, ap, link, target
}

func withCopilotBridgeClock(t *testing.T, now time.Time) {
	t.Helper()
	old := copilotBridgeNow
	copilotBridgeNow = func() time.Time { return now }
	t.Cleanup(func() { copilotBridgeNow = old })
}

func writeCopilotBridgeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// simulateCopilotSave does what the Copilot CLI's config store does: write a
// temp file beside config.json and rename it over it, replacing the symlink.
func simulateCopilotSave(t *testing.T, link, body string) {
	t.Helper()
	tmp := link + copilotConfigTmpInfix + "abc123"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("simulated save did not replace the link: info=%v err=%v", info, err)
	}
}

func assertCopilotLinked(t *testing.T, link, target string) {
	t.Helper()
	got, err := os.Readlink(link)
	if err != nil || got != target {
		t.Fatalf("copilot config bridge: readlink=%q err=%v, want %q", got, err, target)
	}
}

func copilotBackups(t *testing.T, link string) []string {
	t.Helper()
	matches, err := filepath.Glob(link + copilotConfigBackupInfix + "*")
	if err != nil {
		t.Fatal(err)
	}
	return matches
}

func assertNoSentinelLogged(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	if strings.Contains(buf.String(), copilotBridgeTestSentinel) {
		t.Fatalf("log leaked token material:\n%s", buf.String())
	}
}

func TestCopilotConfigBridge_RestartRelinksEmptyPrivateCopy(t *testing.T) {
	m, buf, ap, link, target := copilotBridgeFixture(t)
	simulateCopilotSave(t, link, copilotBridgeTestNoLogin)

	m.setupInteractiveHome(ap, "copilot")

	assertCopilotLinked(t, link, target)
	backups := copilotBackups(t, link)
	if len(backups) != 1 {
		t.Fatalf("want exactly one backup, got %v", backups)
	}
	if want := link + copilotConfigBackupInfix + "20261116T100000Z"; backups[0] != want {
		t.Fatalf("backup = %q, want %q", backups[0], want)
	}
	data, err := os.ReadFile(backups[0])
	if err != nil || string(data) != copilotBridgeTestNoLogin {
		t.Fatalf("backup must hold the private copy unchanged: data=%q err=%v", data, err)
	}
	if shared, err := os.ReadFile(target); err != nil || string(shared) != copilotBridgeTestLoggedIn {
		t.Fatalf("shared login must not change: err=%v", err)
	}
	if !strings.Contains(buf.String(), "re-linked") {
		t.Fatalf("repair not logged:\n%s", buf.String())
	}
	assertNoSentinelLogged(t, buf)
}

func TestCopilotConfigBridge_ValidSymlinkLeftAlone(t *testing.T) {
	m, buf, ap, link, target := copilotBridgeFixture(t)
	before, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	m.setupInteractiveHome(ap, "copilot")
	after, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	assertCopilotLinked(t, link, target)
	if !os.SameFile(before, after) {
		t.Fatal("a valid symlink must not be replaced")
	}
	if b := copilotBackups(t, link); len(b) != 0 {
		t.Fatalf("no backup expected for a valid link, got %v", b)
	}
	if strings.Contains(buf.String(), "re-linked") {
		t.Fatalf("a valid link must not be reported as repaired:\n%s", buf.String())
	}
}

func TestCopilotConfigBridge_RepeatedRestartsConvergeAndKeepNewestBackup(t *testing.T) {
	m, buf, ap, link, target := copilotBridgeFixture(t)
	base := time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC)
	var last string
	for i := 0; i < 3; i++ {
		now := base.Add(time.Duration(i) * time.Minute)
		withCopilotBridgeClock(t, now)
		simulateCopilotSave(t, link, copilotBridgeTestNoLogin)
		m.setupInteractiveHome(ap, "copilot")
		assertCopilotLinked(t, link, target)
		last = link + copilotConfigBackupInfix + now.UTC().Format("20060102T150405Z")
	}
	// Further restarts with nothing to repair change nothing.
	m.setupInteractiveHome(ap, "copilot")
	m.setupInteractiveHome(ap, "copilot")
	assertCopilotLinked(t, link, target)
	backups := copilotBackups(t, link)
	if len(backups) != 1 || backups[0] != last {
		t.Fatalf("want only the newest backup %q, got %v", last, backups)
	}
	assertNoSentinelLogged(t, buf)
}

func TestCopilotConfigBridge_PrivateLoginKeptWhenSharedEmpty(t *testing.T) {
	m, buf, ap, link, target := copilotBridgeFixture(t)
	simulateCopilotSave(t, link, copilotBridgeTestLoggedIn)
	writeCopilotBridgeFile(t, target, copilotBridgeTestNoLogin)

	m.setupInteractiveHome(ap, "copilot")
	m.setupInteractiveHome(ap, "copilot")

	info, err := os.Lstat(link)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("private copy holding the only login must stay in place: info=%v err=%v", info, err)
	}
	if b := copilotBackups(t, link); len(b) != 0 {
		t.Fatalf("no backup expected, got %v", b)
	}
	if got := strings.Count(buf.String(), "repair not applied"); got != 1 {
		t.Fatalf("refusal must be logged once, got %d:\n%s", got, buf.String())
	}
	assertNoSentinelLogged(t, buf)
}

func TestRepairCopilotConfigBridge_LegacyDirectoryLinkUntouched(t *testing.T) {
	shared := withSharedAgentHome(t)
	target := filepath.Join(shared, copilotHomeDirName, copilotConfigFileName)
	writeCopilotBridgeFile(t, target, copilotBridgeTestLoggedIn)
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(target), filepath.Join(home, copilotHomeDirName)); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, copilotHomeDirName, copilotConfigFileName)

	backup, err := repairCopilotConfigBridge(link, target)
	if !errors.Is(err, errCopilotBridgeLinkIsTarget) || backup != "" {
		t.Fatalf("want errCopilotBridgeLinkIsTarget and no backup, got %q, %v", backup, err)
	}
	if data, rerr := os.ReadFile(target); rerr != nil || string(data) != copilotBridgeTestLoggedIn {
		t.Fatalf("shared login must be untouched: err=%v", rerr)
	}
	if copilotConfigDiverged(link, target) {
		t.Fatal("a path that IS the shared file is not diverged")
	}
}

func TestRepairCopilotConfigBridge_NothingToRepair(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "shared.json")
	// Absent link.
	if b, err := repairCopilotConfigBridge(filepath.Join(dir, "absent.json"), target); b != "" || err != nil {
		t.Fatalf("absent link: got %q, %v", b, err)
	}
	// A directory at the link path is not this failure.
	d := filepath.Join(dir, "adir")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if b, err := repairCopilotConfigBridge(d, target); b != "" || err != nil {
		t.Fatalf("directory link: got %q, %v", b, err)
	}
	// Unresolvable directories: nothing is moved.
	link := filepath.Join(dir, "config.json")
	writeCopilotBridgeFile(t, link, copilotBridgeTestNoLogin)
	missingTarget := filepath.Join(dir, "missing", "config.json")
	if b, err := repairCopilotConfigBridge(link, missingTarget); b != "" || err != nil {
		t.Fatalf("unresolvable target dir: got %q, %v", b, err)
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("file must stay in place: info=%v err=%v", info, err)
	}
}

func TestMoveAsideCopilotConfig_PicksFreeName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, copilotConfigFileName)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	taken := path + copilotConfigBackupInfix + "20261116T120000Z"
	writeCopilotBridgeFile(t, taken, copilotBridgeTestNoLogin)
	writeCopilotBridgeFile(t, path, copilotBridgeTestNoLogin)

	dst, err := moveAsideCopilotConfig(path, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := taken + "-1"; dst != want {
		t.Fatalf("dst = %q, want %q", dst, want)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("source must be gone after the move: %v", err)
	}
	if _, err := moveAsideCopilotConfig(filepath.Join(dir, "absent"), now); err == nil {
		t.Fatal("moving an absent file must fail")
	}
}

func TestPruneCopilotConfigBackups_KeepsOnlyNamedAndNonRegular(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, copilotConfigFileName)
	keep := link + copilotConfigBackupInfix + "b"
	old := link + copilotConfigBackupInfix + "a"
	sub := link + copilotConfigBackupInfix + "dir"
	writeCopilotBridgeFile(t, keep, "{}")
	writeCopilotBridgeFile(t, old, "{}")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	pruneCopilotConfigBackups(link, keep)
	if _, err := os.Lstat(keep); err != nil {
		t.Fatalf("kept backup removed: %v", err)
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatalf("older backup must be pruned: %v", err)
	}
	if _, err := os.Lstat(sub); err != nil {
		t.Fatalf("non-regular entry must be left alone: %v", err)
	}
}

func TestCopilotConfigDiverged(t *testing.T) {
	dir := t.TempDir()
	sharedDir := filepath.Join(dir, "shared")
	agentDir := filepath.Join(dir, "agent")
	target := filepath.Join(sharedDir, copilotConfigFileName)
	link := filepath.Join(agentDir, copilotConfigFileName)
	writeCopilotBridgeFile(t, target, copilotBridgeTestLoggedIn)
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if copilotConfigDiverged(link, target) {
		t.Fatal("absent file is not diverged")
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if copilotConfigDiverged(link, target) {
		t.Fatal("a symlink is not diverged")
	}
	_ = os.Remove(link)

	cases := []struct {
		name    string
		private string
		want    bool
	}{
		{"empty login", copilotBridgeTestNoLogin, true},
		{"unparseable", "not json", true},
		{"differs from shared", copilotBridgeTestOlder, true},
		{"identical to shared", copilotBridgeTestLoggedIn, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writeCopilotBridgeFile(t, link, tc.private)
			if got := copilotConfigDiverged(link, target); got != tc.want {
				t.Fatalf("diverged = %v, want %v", got, tc.want)
			}
		})
	}

	// No shared file to compare: a private copy with a login is what the
	// agent reads and is not diverged.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	writeCopilotBridgeFile(t, link, copilotBridgeTestOlder)
	if copilotConfigDiverged(link, target) {
		t.Fatal("private login with no shared file is not diverged")
	}
}

func TestCopilotConfigBridgePaths(t *testing.T) {
	withSharedAgentHome(t)
	if _, _, ok := copilotConfigBridgePaths("a", 0, "copilot"); ok {
		t.Fatal("uid 0 has no per-agent bridge")
	}
	if _, _, ok := copilotConfigBridgePaths("a", copilotBridgeTestUID, "litellm"); ok {
		t.Fatal("inference backends have no per-agent bridge")
	}
	t.Setenv("HIVE_SHARED_AGENT_HOME", "1")
	if _, _, ok := copilotConfigBridgePaths("a", copilotBridgeTestUID, "copilot"); ok {
		t.Fatal("forced shared home has no per-agent bridge")
	}
}

// The health check must fail when the per-agent file differs from the shared
// one, even though the shared file and the in-memory token both hold a login.
func TestCredentialProbe_CopilotEvaluatesFileAgentReads(t *testing.T) {
	m, buf, ap, link, target := copilotBridgeFixture(t)
	m.mu.Lock()
	m.copilotAuthToken = "fake-in-memory-" + copilotBridgeTestSentinel
	m.mu.Unlock()

	if !m.AgentHasValidCredential(ap.Name) {
		t.Fatal("linked config with a shared login must prove the credential")
	}
	simulateCopilotSave(t, link, copilotBridgeTestNoLogin)
	if m.AgentHasValidCredential(ap.Name) {
		t.Fatal("a diverged per-agent copy must not count as a valid credential")
	}
	simulateCopilotSave(t, link, copilotBridgeTestOlder)
	if m.AgentHasValidCredential(ap.Name) {
		t.Fatal("a per-agent copy that differs from the shared login must not count as valid")
	}

	// The poller's login-prompt hook repairs it, and the probe then passes.
	if !m.repairCopilotConfigOnLoginPrompt(ap) {
		t.Fatal("login-prompt hook must repair a diverged copy")
	}
	assertCopilotLinked(t, link, target)
	if !m.AgentHasValidCredential(ap.Name) {
		t.Fatal("after relinking, the shared login must prove the credential")
	}
	if m.repairCopilotConfigOnLoginPrompt(ap) {
		t.Fatal("a second call has nothing to repair")
	}
	assertNoSentinelLogged(t, buf)
}

func TestRepairCopilotConfigOnLoginPrompt_IgnoresOtherBackends(t *testing.T) {
	m, _, _, link, _ := copilotBridgeFixture(t)
	simulateCopilotSave(t, link, copilotBridgeTestNoLogin)
	other := &AgentProcess{Name: copilotBridgeTestAgentName, UID: copilotBridgeTestUID, Config: config.AgentConfig{Backend: "claude"}}
	if m.repairCopilotConfigOnLoginPrompt(other) {
		t.Fatal("non-copilot backends are not repaired by the copilot hook")
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("file must stay in place: info=%v err=%v", info, err)
	}
	noHome := &AgentProcess{Name: "planner", Config: config.AgentConfig{Backend: "copilot"}}
	if m.repairCopilotConfigOnLoginPrompt(noHome) {
		t.Fatal("agent without a per-agent home has nothing to repair")
	}
}

func TestCopilotConfigTmpMaxAge(t *testing.T) {
	t.Setenv(copilotConfigTmpMaxAgeEnv, "")
	if got := copilotConfigTmpMaxAge(); got != defaultCopilotConfigTmpMaxAge {
		t.Fatalf("default = %v", got)
	}
	t.Setenv(copilotConfigTmpMaxAgeEnv, "15m")
	if got := copilotConfigTmpMaxAge(); got != 15*time.Minute {
		t.Fatalf("override = %v", got)
	}
	for _, bad := range []string{"nonsense", "-5m", "0s"} {
		t.Setenv(copilotConfigTmpMaxAgeEnv, bad)
		if got := copilotConfigTmpMaxAge(); got != defaultCopilotConfigTmpMaxAge {
			t.Fatalf("invalid %q must fall back to default, got %v", bad, got)
		}
	}
}

func TestSweepStaleCopilotConfigTmp(t *testing.T) {
	shared := withSharedAgentHome(t)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	withCopilotBridgeClock(t, now)
	t.Setenv(copilotConfigTmpMaxAgeEnv, "")
	var buf bytes.Buffer
	m := &Manager{logger: slog.New(slog.NewTextHandler(&buf, nil))}

	home := filepath.Join(t.TempDir(), "agent")
	sharedDir := filepath.Join(shared, copilotHomeDirName)
	agentDir := filepath.Join(home, copilotHomeDirName)
	staleShared := filepath.Join(sharedDir, copilotConfigFileName+copilotConfigTmpInfix+"old1")
	staleAgent := filepath.Join(agentDir, copilotConfigFileName+copilotConfigTmpInfix+"old2")
	fresh := filepath.Join(sharedDir, copilotConfigFileName+copilotConfigTmpInfix+"new")
	unrelated := filepath.Join(sharedDir, "other.json.tmp.x")
	tmpDir := filepath.Join(sharedDir, copilotConfigFileName+copilotConfigTmpInfix+"dir")
	for _, p := range []string{staleShared, staleAgent, fresh, unrelated} {
		writeCopilotBridgeFile(t, p, "{}")
	}
	if err := os.Mkdir(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * defaultCopilotConfigTmpMaxAge)
	for _, p := range []string{staleShared, staleAgent, unrelated, tmpDir} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	young := now.Add(-time.Minute)
	if err := os.Chtimes(fresh, young, young); err != nil {
		t.Fatal(err)
	}

	m.sweepStaleCopilotConfigTmp("scanner", home)

	for _, gone := range []string{staleShared, staleAgent} {
		if _, err := os.Lstat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s should be swept: %v", gone, err)
		}
	}
	for _, kept := range []string{fresh, unrelated, tmpDir} {
		if _, err := os.Lstat(kept); err != nil {
			t.Fatalf("%s should be kept: %v", kept, err)
		}
	}
	if !strings.Contains(buf.String(), "swept 2 stale copilot config tmp file(s)") {
		t.Fatalf("sweep not logged:\n%s", buf.String())
	}

	// A shorter threshold from the env sweeps the younger file too.
	t.Setenv(copilotConfigTmpMaxAgeEnv, "30s")
	m.sweepStaleCopilotConfigTmp("scanner", home)
	if _, err := os.Lstat(fresh); !os.IsNotExist(err) {
		t.Fatalf("env threshold not honoured: %v", err)
	}
	// Missing directories are not an error.
	m.sweepStaleCopilotConfigTmp("scanner", filepath.Join(t.TempDir(), "nohome"))
}

// A completed relink is routine self-healing and must not log at WARN, or the
// fleet self-reports it as an unclassified repeated error (#18497).
func TestRebridgeCopilotConfig_SuccessLogsInfoNotWarn(t *testing.T) {
	m, buf, ap, link, target := copilotBridgeFixture(t)
	simulateCopilotSave(t, link, copilotBridgeTestNoLogin)
	if !m.rebridgeCopilotConfig(ap.Name, link, target, copilotBridgeTriggerLoginPrompt) {
		t.Fatal("diverged copy must be relinked")
	}
	out := buf.String()
	if !strings.Contains(out, "re-linked") {
		t.Fatalf("relink not logged:\n%s", out)
	}
	if strings.Contains(out, "level=WARN") {
		t.Fatalf("successful relink must not log at WARN:\n%s", out)
	}
}
