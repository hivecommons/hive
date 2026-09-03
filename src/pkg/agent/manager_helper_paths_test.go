package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// Path/branch coverage for small deterministic manager helpers whose error
// arms were previously untested: ProjectContext.PrimaryRepo,
// copilotCredentialFileHasTokens's apps.json/hosts.json shape,
// inferenceHomeIsOwnedBy, mkdirAllNoFollow's refusal arms, writeAgentStateFile
// symlink refusal, ensureBobAuthSettings, and CaptureFullLog.

// ---------------------------------------------------------------------------
// ProjectContext.PrimaryRepo
// ---------------------------------------------------------------------------

func TestPrimaryRepo(t *testing.T) {
	cases := []struct {
		name string
		p    ProjectContext
		want string
	}{
		{"explicit name wins and org prefix is stripped",
			ProjectContext{Org: "hivecommons", PrimaryRepoName: "hivecommons/hive", Repos: []string{"hivecommons/other"}}, "hive"},
		{"explicit bare name kept",
			ProjectContext{Org: "hivecommons", PrimaryRepoName: "hive"}, "hive"},
		{"falls back to first repo",
			ProjectContext{Org: "hivecommons", Repos: []string{"hivecommons/console", "hivecommons/docs"}}, "console"},
		{"empty context yields empty",
			ProjectContext{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.PrimaryRepo(); got != tc.want {
				t.Errorf("PrimaryRepo() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// copilotCredentialFileHasTokens — apps.json / hosts.json shape
// ---------------------------------------------------------------------------

func TestCopilotCredentialFileHasTokens_AppsJSONShape(t *testing.T) {
	dir := t.TempDir()

	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("apps.json with oauth_token", func(t *testing.T) {
		p := write("apps.json", `{"github.com:Iv1.abc":{"oauth_token":"gho_xyz","user":"someone"}}`)
		if !copilotCredentialFileHasTokens(p) {
			t.Error("oauth_token entry must count as credentials")
		}
	})
	t.Run("hosts.json with empty oauth_token", func(t *testing.T) {
		p := write("hosts.json", `{"github.com":{"oauth_token":""}}`)
		if copilotCredentialFileHasTokens(p) {
			t.Error("empty oauth_token must not count as credentials")
		}
	})
	t.Run("hosts.json with non-object entry skipped", func(t *testing.T) {
		p := write("hosts.json", `{"github.com":"not-an-object"}`)
		if copilotCredentialFileHasTokens(p) {
			t.Error("non-object entry must not count as credentials")
		}
	})
	t.Run("host-shape content under a non-hosts filename is ignored", func(t *testing.T) {
		// The host->oauth_token scan is scoped to apps.json/hosts.json; a
		// config.json without copilotTokens must not match it.
		p := write("config.json", `{"github.com":{"oauth_token":"gho_xyz"}}`)
		if copilotCredentialFileHasTokens(p) {
			t.Error("oauth_token scan must be limited to apps.json/hosts.json")
		}
	})
}

// ---------------------------------------------------------------------------
// inferenceHomeIsOwnedBy
// ---------------------------------------------------------------------------

func TestInferenceHomeIsOwnedBy(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
	home := t.TempDir() // owned by the test process uid

	if m.inferenceHomeIsOwnedBy(home, 0) {
		t.Error("uid 0 sentinel (no allocated uid) must never claim ownership")
	}
	if m.inferenceHomeIsOwnedBy(home, -1) {
		t.Error("negative uid must never claim ownership")
	}
	if m.inferenceHomeIsOwnedBy(filepath.Join(home, "missing"), os.Getuid()) {
		t.Error("missing path must not claim ownership")
	}
	if os.Getuid() > 0 {
		if !m.inferenceHomeIsOwnedBy(home, os.Getuid()) {
			t.Error("dir owned by the given uid must be recognized")
		}
	}
	if m.inferenceHomeIsOwnedBy(home, os.Getuid()+12345) {
		t.Error("dir owned by a different uid must be rejected")
	}
}

// ---------------------------------------------------------------------------
// mkdirAllNoFollow — refusal arms
// ---------------------------------------------------------------------------

func TestMkdirAllNoFollow_RefusalArms(t *testing.T) {
	t.Run("nonexistent root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "gone")
		if err := mkdirAllNoFollow(root, filepath.Join(root, "x"), 0o700); err == nil {
			t.Error("expected error for nonexistent root")
		}
	})
	t.Run("dir escaping root refused", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(filepath.Dir(root), "escapee")
		err := mkdirAllNoFollow(root, outside, 0o700)
		if err == nil || !strings.Contains(err.Error(), "escapes root") {
			t.Errorf("error = %v, want escapes root refusal", err)
		}
		if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
			t.Error("escaping dir must not be created")
		}
	})
	t.Run("dir equal to root is a no-op", func(t *testing.T) {
		root := t.TempDir()
		if err := mkdirAllNoFollow(root, root, 0o700); err != nil {
			t.Errorf("dir == root: %v", err)
		}
	})
	t.Run("symlink component refused", func(t *testing.T) {
		root := t.TempDir()
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		err := mkdirAllNoFollow(root, filepath.Join(root, "link", "sub"), 0o700)
		if err == nil {
			t.Fatal("expected symlink refusal (audit F12)")
		}
		if _, statErr := os.Stat(filepath.Join(target, "sub")); !os.IsNotExist(statErr) {
			t.Error("subtree must not be created through the planted link")
		}
	})
	t.Run("creates nested dirs below root", func(t *testing.T) {
		root := t.TempDir()
		want := filepath.Join(root, "a", "b")
		if err := mkdirAllNoFollow(root, want, 0o700); err != nil {
			t.Fatalf("mkdirAllNoFollow: %v", err)
		}
		info, err := os.Stat(want)
		if err != nil || !info.IsDir() {
			t.Errorf("nested dir not created: %v", err)
		}
	})
	if os.Getuid() > 0 {
		t.Run("mkdir failure surfaces", func(t *testing.T) {
			root := t.TempDir()
			locked := filepath.Join(root, "locked")
			if err := os.Mkdir(locked, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
			if err := mkdirAllNoFollow(root, filepath.Join(locked, "sub"), 0o700); err == nil {
				t.Error("expected mkdir error under read-only parent")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// writeAgentStateFile — symlink refusal (TOCTOU guard entry point)
// ---------------------------------------------------------------------------

func TestWriteAgentStateFile_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "state-file")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}

	if err := writeAgentStateFile(link, []byte("clobber")); err == nil {
		t.Fatal("expected O_NOFOLLOW refusal for a planted symlink")
	}
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original" {
		t.Errorf("victim file clobbered through symlink: %q", data)
	}
}

// ---------------------------------------------------------------------------
// ensureBobAuthSettings
// ---------------------------------------------------------------------------

// helperPathsManager builds a bare manager for the helper-path tests.
func helperPathsManager(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	return NewManager(map[string]config.AgentConfig{}, discardLogger(), ProjectContext{})
}

func TestEnsureBobAuthSettings_FailuresAreSwallowed(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: permissions do not bind")
	}
	m := helperPathsManager(t)

	t.Run("unreadable file", func(t *testing.T) {
		home := t.TempDir()
		path := bobSettingsPath(home)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
		// Read fails with EACCES (not IsNotExist) → log and return, no panic.
		m.ensureBobAuthSettings("architect", home)
		if data, err := os.ReadFile(path); err == nil && len(data) != 0 && string(data) != "{}" {
			t.Errorf("unreadable file must not be rewritten, got %q", data)
		}
	})
	t.Run("mkdir failure", func(t *testing.T) {
		home := t.TempDir()
		if err := os.Chmod(home, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(home, 0o755) })
		// MkdirAll of <home>/.bob fails → log and return, no panic.
		m.ensureBobAuthSettings("architect", home)
	})
	t.Run("write failure", func(t *testing.T) {
		home := t.TempDir()
		path := bobSettingsPath(home)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		// Readable but not writable, and carrying the wrong auth type so a
		// write is attempted and fails.
		if err := os.WriteFile(path, []byte(`{"security":{"auth":{"selectedType":"sso"}}}`), 0o444); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
		m.ensureBobAuthSettings("architect", home)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), config.BobAuthTypeAPIKey) {
			t.Error("read-only settings file was somehow rewritten")
		}
	})
}

// ---------------------------------------------------------------------------
// CaptureFullLog
// ---------------------------------------------------------------------------

func TestCaptureFullLog_ErrorPaths(t *testing.T) {
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"scanner": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})

	t.Run("unknown agent", func(t *testing.T) {
		if _, err := m.CaptureFullLog("nope"); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("error = %v, want not found", err)
		}
	})
	t.Run("no active session", func(t *testing.T) {
		// NewManager seeds a default session name; a stopped/never-launched
		// agent has it cleared.
		m.mu.Lock()
		m.agents["scanner"].tmuxSession = ""
		m.mu.Unlock()
		if _, err := m.CaptureFullLog("scanner"); err == nil || !strings.Contains(err.Error(), "no active session") {
			t.Errorf("error = %v, want no active session", err)
		}
	})
	t.Run("capture fails for vanished session", func(t *testing.T) {
		m.mu.Lock()
		m.agents["scanner"].tmuxSession = "hive-capture-gone"
		m.mu.Unlock()
		if _, err := m.CaptureFullLog("scanner"); err == nil || !strings.Contains(err.Error(), "capturing pane") {
			t.Errorf("error = %v, want capturing pane wrap", err)
		}
	})
}

func TestCaptureFullLog_CapturesPaneContent(t *testing.T) {
	if !tmuxAvailable() {
		t.Skip("tmux not available")
	}
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"scanner": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})
	forceSharedUID(t, m, "scanner")

	session := "hive-capture-full-log"
	newRawTmuxSession(t, session)
	paneInject(t, session, "full-log-marker-xyz")

	m.mu.Lock()
	m.agents["scanner"].tmuxSession = session
	m.mu.Unlock()

	out, err := m.CaptureFullLog("scanner")
	if err != nil {
		t.Fatalf("CaptureFullLog: %v", err)
	}
	if !strings.Contains(out, "full-log-marker-xyz") {
		t.Errorf("captured log does not contain injected marker; got %d bytes", len(out))
	}
}
