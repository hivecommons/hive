package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// gitConfigHomeManager builds a manager whose logger records into the returned
// buffer, so the launch-time identity WARN is observable.
func gitConfigHomeManager(t *testing.T) (*Manager, *syncBuffer) {
	t.Helper()
	logBuf := &syncBuffer{}
	m := NewManager(map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Model: "sonnet"},
	}, slog.New(slog.NewTextHandler(logBuf, nil)), ProjectContext{})
	return m, logBuf
}

// TestSetupInteractiveHome_DoesNotBridgeGitConfig is the regression test for
// hivecommons/hive#9478: bridging ~/.gitconfig gave the whole fleet ONE global
// git config, so a `git config --global user.name` by any agent re-attributed
// every other lane's commits until the next write.
func TestSetupInteractiveHome_DoesNotBridgeGitConfig(t *testing.T) {
	withSharedAgentHome(t)
	m, _ := gitConfigHomeManager(t)
	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}

	m.setupInteractiveHome(ap, "claude")

	if _, err := os.Lstat(filepath.Join(interactiveHomePath("scanner"), gitConfigFileName)); !os.IsNotExist(err) {
		t.Errorf(".gitconfig should not be bridged into a per-agent home (err=%v)", err)
	}
	for _, name := range interactiveHomeBridgeFiles {
		if name == gitConfigFileName {
			t.Errorf("%s is still listed in interactiveHomeBridgeFiles", name)
		}
	}
}

// TestSetupInteractiveHome_RetiresLegacyGitConfigBridge: a pre-fix home has
// .gitconfig as a symlink to the shared file. Re-provisioning removes the
// link, leaves the shared file itself untouched (other agents still reach it
// until they are re-provisioned too), and stays idempotent.
func TestSetupInteractiveHome_RetiresLegacyGitConfigBridge(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, logBuf := gitConfigHomeManager(t)
	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	home := interactiveHomePath("scanner")

	sharedConfig := filepath.Join(shared, gitConfigFileName)
	if err := os.WriteFile(sharedConfig, []byte("[user]\n\tname = sec-check\n\temail = sec-check@hive.kubestellar.io\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sharedConfig, filepath.Join(home, gitConfigFileName)); err != nil {
		t.Fatal(err)
	}

	m.setupInteractiveHome(ap, "claude")

	if _, err := os.Lstat(filepath.Join(home, gitConfigFileName)); !os.IsNotExist(err) {
		t.Errorf("legacy .gitconfig bridge not retired (err=%v)", err)
	}
	if data, err := os.ReadFile(sharedConfig); err != nil || !strings.Contains(string(data), "sec-check") {
		t.Errorf("shared .gitconfig was disturbed: data=%q err=%v", string(data), err)
	}
	if !strings.Contains(logBuf.String(), "retired shared .gitconfig bridge") {
		t.Errorf("retirement not logged: %s", logBuf.String())
	}

	// Idempotent: a second provisioning does not re-create the bridge.
	m.setupInteractiveHome(ap, "claude")
	if _, err := os.Lstat(filepath.Join(home, gitConfigFileName)); !os.IsNotExist(err) {
		t.Errorf(".gitconfig bridge came back on re-provision (err=%v)", err)
	}
}

// TestRetireSharedGitConfigBridge_KeepsRealPerAgentFile: a REAL .gitconfig is
// the agent's own state and must never be deleted — only the shared bridge is.
func TestRetireSharedGitConfigBridge_KeepsRealPerAgentFile(t *testing.T) {
	withSharedAgentHome(t)
	m, _ := gitConfigHomeManager(t)
	home := interactiveHomePath("scanner")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, gitConfigFileName)
	if err := os.WriteFile(path, []byte("[alias]\n\tst = status\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.retireSharedGitConfigBridge("scanner", home)

	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "st = status") {
		t.Errorf("real per-agent .gitconfig was clobbered: data=%q err=%v", string(data), err)
	}
}

// TestWarnOnStrayGitIdentity covers proposed-fix item 4: a user identity that
// survives in a global config layer no longer decides who commits (the pinned
// GIT_AUTHOR_*/GIT_COMMITTER_* env vars do), but it must be named at launch
// because `git config user.email` still reports it.
func TestWarnOnStrayGitIdentity(t *testing.T) {
	withSharedAgentHome(t)
	home := interactiveHomePath("scanner")
	xdgConfig := filepath.Join(home, ".config", "git")
	if err := os.MkdirAll(xdgConfig, 0o755); err != nil {
		t.Fatal(err)
	}

	m, logBuf := gitConfigHomeManager(t)
	m.warnOnStrayGitIdentity("scanner", home)
	if strings.Contains(logBuf.String(), "declares a user identity") {
		t.Errorf("warned with no stray identity present: %s", logBuf.String())
	}

	if err := os.WriteFile(filepath.Join(xdgConfig, "config"), []byte("[user]\n\temail = sec-check@hive.kubestellar.io\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, logBuf = gitConfigHomeManager(t)
	m.warnOnStrayGitIdentity("scanner", home)
	out := logBuf.String()
	if !strings.Contains(out, "declares a user identity") || !strings.Contains(out, filepath.Join(".config", "git", "config")) {
		t.Errorf("stray identity in the XDG global layer not warned: %s", out)
	}
}

func TestGitConfigDeclaresUserIdentity(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"user name", "[user]\n\tname = scanner\n", true},
		{"user email", "[user]\nemail=scanner@example.com\n", true},
		{"inline section", "[user] email = scanner@example.com\n", true},
		{"other section only", "[core]\n\teditor = vi\n[credential]\n\thelper = x\n", false},
		{"commented out", "[user]\n#\tname = scanner\n;\temail = a@b\n", false},
		{"lookalike key in other section", "[committer]\n\tname = scanner\n", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-"))
			if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := gitConfigDeclaresUserIdentity(path); got != tc.want {
				t.Errorf("gitConfigDeclaresUserIdentity(%q) = %v, want %v", tc.content, got, tc.want)
			}
		})
	}
	if gitConfigDeclaresUserIdentity(filepath.Join(dir, "missing")) {
		t.Error("a missing config must not count as a finding")
	}
}
