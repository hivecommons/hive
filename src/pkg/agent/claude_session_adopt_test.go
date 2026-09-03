package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// fakeAgentUserRunner stands in for su-exec. Unit tests cannot create files
// owned by a second UID, so "unreadable to the hive process" is simulated with
// mode 0000 (the test process is not root under `go test`) and this runner
// plays the part of the privileged helper that CAN reach the bytes.
type fakeAgentUserRunner struct {
	mu    sync.Mutex
	specs []string
	argvs [][]string
	fail  error
}

// install swaps the runner in for the duration of one test.
func (f *fakeAgentUserRunner) install(t *testing.T) {
	t.Helper()
	old := runAsAgentUser
	runAsAgentUser = func(spec string, stdin []byte, argv ...string) ([]byte, error) {
		f.mu.Lock()
		f.specs = append(f.specs, spec)
		f.argvs = append(f.argvs, append([]string(nil), argv...))
		failure := f.fail
		f.mu.Unlock()
		if failure != nil {
			return nil, failure
		}
		return f.serve(stdin, argv...)
	}
	t.Cleanup(func() { runAsAgentUser = old })
}

// serve emulates the two argv shapes production uses, bypassing file modes the
// way a real su-exec-to-the-owner would.
func (f *fakeAgentUserRunner) serve(stdin []byte, argv ...string) ([]byte, error) {
	switch {
	case len(argv) == 2 && argv[0] == "cat":
		data, err := readIgnoringMode(argv[1])
		if err != nil {
			return nil, err
		}
		return data, nil
	case len(argv) == 4 && argv[0] == "sh" && argv[1] == "-c":
		target := argv[3]
		if err := os.Chmod(target, 0o600); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		return nil, os.WriteFile(target, stdin, claudeSessionSeedFileMode)
	}
	return nil, fmt.Errorf("unexpected argv %v", argv)
}

func (f *fakeAgentUserRunner) calls() ([]string, [][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.specs...), append([][]string(nil), f.argvs...)
}

// readIgnoringMode reads a file the test itself owns but has locked to 0000,
// by widening the mode just long enough to read it back.
func readIgnoringMode(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	defer func() { _ = os.Chmod(path, info.Mode().Perm()) }()
	return os.ReadFile(path)
}

// writeUnreadableSession writes a session file this process cannot open,
// standing in for the agent-owned 0600 file a fresh install's first login
// produces.
func writeUnreadableSession(t *testing.T, home, content string) string {
	t.Helper()
	path := writeSession(t, home, content)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); !errors.Is(err, os.ErrPermission) {
		t.Skipf("cannot simulate an unreadable file here (running as root?): %v", err)
	}
	return path
}

// --- the fresh-install case (#4637) -------------------------------------------

func TestSeedClaudeSession_AdoptsAgentOwnedSibling(t *testing.T) {
	withSharedAgentHome(t)
	runner := &fakeAgentUserRunner{}
	runner.install(t)
	m := interactiveHomeTestManager(t)

	// Fresh install: no legacy shared file at all, and the one signed-in
	// session belongs to the agent the operator logged in on.
	writeUnreadableSession(t, interactiveHomePath("guide"), signedInClaudeSession)

	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	target := claudeSessionFile(interactiveHomePath("scanner"))
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("fresh-install login did not propagate: %v", err)
	}
	if got := classifyClaudeSession(target, data).State; got != claudeSessionSignedIn {
		t.Errorf("seeded state = %v, want signed-in", got)
	}

	_, argvs := runner.calls()
	if len(argvs) == 0 {
		t.Fatal("expected the su-exec seam to be used for the agent-owned sibling")
	}
	if argvs[0][0] != "cat" {
		t.Errorf("first helper argv = %v, want a cat read", argvs[0])
	}
}

func TestSeedClaudeSession_UnreadableSignedInTargetIsNotClobbered(t *testing.T) {
	shared := withSharedAgentHome(t)
	runner := &fakeAgentUserRunner{}
	runner.install(t)
	m := interactiveHomeTestManager(t)

	own := `{"oauthAccount":{"accountUuid":"mine","emailAddress":"me@example.com"}}`
	target := writeUnreadableSession(t, interactiveHomePath("scanner"), own)
	writeSession(t, shared, signedInClaudeSession)

	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	data, err := readIgnoringMode(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != own {
		t.Errorf("adopt-only violated: agent's own signed-in session replaced by %q", data)
	}
}

func TestSeedClaudeSession_SeedIsReadableByTheHiveProcess(t *testing.T) {
	withSharedAgentHome(t)
	runner := &fakeAgentUserRunner{}
	runner.install(t)
	m := interactiveHomeTestManager(t)
	writeUnreadableSession(t, interactiveHomePath("guide"), signedInClaudeSession)

	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	// #4636: a CLI the manager spawns against a session file the hive uid
	// cannot read treats the home as a fresh install and rewrites the identity
	// away. A seeded file must not land in that shape.
	info, err := os.Stat(claudeSessionFile(interactiveHomePath("scanner")))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o044 == 0 {
		t.Errorf("seeded session mode %v is not readable outside the owner", info.Mode().Perm())
	}
}

func TestSeedClaudeSession_HelperFailureLeavesLoginMenuHonest(t *testing.T) {
	withSharedAgentHome(t)
	runner := &fakeAgentUserRunner{fail: errors.New("su-exec: not found")}
	runner.install(t)
	m := interactiveHomeTestManager(t)
	writeUnreadableSession(t, interactiveHomePath("guide"), signedInClaudeSession)

	ap := &AgentProcess{Name: "scanner", UID: 1001, Config: config.AgentConfig{Backend: "claude"}}
	m.setupInteractiveHome(ap, "claude")

	target := claudeSessionFile(interactiveHomePath("scanner"))
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Errorf("no readable source: nothing may be fabricated (err=%v)", err)
	}
}

// --- ownership guards ----------------------------------------------------------

func TestClaudeSessionOwnerSpec_RefusesNonRegularAndOversize(t *testing.T) {
	dir := t.TempDir()

	link := filepath.Join(dir, "link.json")
	if err := os.Symlink("/etc/shadow", link); err != nil {
		t.Fatal(err)
	}
	if _, err := claudeSessionOwnerSpec(link); err == nil {
		t.Error("a symlink must never resolve to a helper user spec")
	}

	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, make([]byte, claudeSessionAdoptMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := claudeSessionOwnerSpec(big); err == nil || !strings.Contains(err.Error(), "cap") {
		t.Errorf("oversize file should be refused by the cap, got %v", err)
	}

	if _, err := claudeSessionOwnerSpec(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("a missing file must not produce a user spec")
	}
}

func TestClaudeSessionOwnerSpec_AcceptsOrdinarySessionFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := os.WriteFile(path, []byte(signedInClaudeSession), 0o600); err != nil {
		t.Fatal(err)
	}
	spec, err := claudeSessionOwnerSpec(path)
	if err != nil {
		if os.Getuid() == 0 {
			t.Skip("root-owned file is refused by design")
		}
		t.Fatalf("ordinary session file refused: %v", err)
	}
	if want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()); spec != want {
		t.Errorf("owner spec = %q, want %q", spec, want)
	}
}

// --- the seam itself -----------------------------------------------------------

func TestInspectClaudeSessionForAdoption_FallsBackToOwnerRead(t *testing.T) {
	withSharedAgentHome(t)
	runner := &fakeAgentUserRunner{}
	runner.install(t)
	m := interactiveHomeTestManager(t)

	path := writeUnreadableSession(t, interactiveHomePath("guide"), signedInClaudeSession)

	if got := inspectClaudeSession(path).State; got != claudeSessionUnreadable {
		t.Fatalf("precondition: direct inspect = %v, want unreadable", got)
	}
	if got := m.inspectClaudeSessionForAdoption(path).State; got != claudeSessionSignedIn {
		t.Errorf("owner-aware inspect = %v, want signed-in", got)
	}
	specs, _ := runner.calls()
	if len(specs) == 0 || !strings.Contains(specs[0], ":") {
		t.Errorf("helper spec = %v, want a uid:gid spec", specs)
	}
}

func TestRunAsAgentUser_RejectsEmptyInput(t *testing.T) {
	if _, err := runAsAgentUser("", nil, "cat", "/dev/null"); err == nil {
		t.Error("empty user spec must be rejected before exec")
	}
	if _, err := runAsAgentUser("1001:1001", nil); err == nil {
		t.Error("empty argv must be rejected before exec")
	}
}
