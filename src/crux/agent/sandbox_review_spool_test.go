package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/sandbox"
)

// spoolProbeLauncher records what the sandbox would see of its review spool
// (hivecommons/hive#14986): the env hive-review reads, and the manifests
// registered while the sandbox is running.
type spoolProbeLauncher struct {
	manifestDir string
	mu          *sync.Mutex
	env         *map[string]string
	manifests   *[]crux.SandboxReviewSpool
	// request, when set, is written into the spool as the sandbox's last act.
	request string
	// written, when set, receives the request's path once it is in the spool.
	written chan string
}

func (l spoolProbeLauncher) Run(_ context.Context, spec sandbox.LaunchSpec) (sandbox.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*l.env = spec.Env
	*l.manifests = readSpoolManifests(l.manifestDir)
	if l.request != "" {
		spool := filepath.Join(spec.Workspace, crux.SandboxReviewSpoolRelPath)
		path := filepath.Join(spool, "r.json")
		if err := os.WriteFile(path, []byte(l.request), 0o660); err != nil {
			return sandbox.Result{}, err
		}
		if l.written != nil {
			l.written <- path
		}
	}
	return sandbox.Result{Stdout: "ok"}, nil
}

func readSpoolManifests(dir string) []crux.SandboxReviewSpool {
	entries, _ := os.ReadDir(dir)
	var out []crux.SandboxReviewSpool
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m crux.SandboxReviewSpool
		if json.Unmarshal(b, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func spoolProbe(manifestDir string) (spoolProbeLauncher, *map[string]string, *[]crux.SandboxReviewSpool) {
	env := map[string]string{}
	var manifests []crux.SandboxReviewSpool
	return spoolProbeLauncher{manifestDir: manifestDir, mu: &sync.Mutex{}, env: &env, manifests: &manifests}, &env, &manifests
}

// A kick registers its spool before the sandbox starts, tells the sandbox
// where it is, and marks it ended after.
func TestSandboxExecutorRegistersReviewSpool(t *testing.T) {
	root := t.TempDir()
	manifestDir := filepath.Join(t.TempDir(), "sandbox-review-spools")
	launcher, env, during := spoolProbe(manifestDir)
	exec := &SandboxExecutor{Runner: &sandboxFakeRunner{}, Launcher: launcher}
	res, err := exec.Run(context.Background(), SandboxKickSpec{
		Agent: "reviewer-spyre", AgentConfig: configSnapshot{Backend: "claude"}, Message: "review #7",
		Org: "torch-spyre", Repo: "spyre-inference", WorkspaceDir: root, Image: "card-image",
		ReviewSpoolDir: manifestDir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got, want := (*env)[crux.SandboxReviewSpoolEnv], "/workspace/.hive/review-requests"; got != want {
		t.Errorf("sandbox %s = %q, want %q", crux.SandboxReviewSpoolEnv, got, want)
	}
	spool := filepath.Join(res.Workspace, crux.SandboxReviewSpoolRelPath)
	if fi, err := os.Stat(spool); err != nil || !fi.IsDir() {
		t.Fatalf("spool not created in the workspace: %v", err)
	}
	if len(*during) != 1 {
		t.Fatalf("manifests registered while the sandbox ran = %d, want 1", len(*during))
	}
	if m := (*during)[0]; m.Agent != "reviewer-spyre" || m.Spool != spool || m.EndedAt != nil {
		t.Errorf("manifest during the run = %+v, want agent reviewer-spyre, spool %s, not ended", m, spool)
	}
	if fi, err := os.Stat(manifestDir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("manifest dir must be owner-only: %v %v", fi.Mode(), err)
	}
	after := readSpoolManifests(manifestDir)
	if len(after) != 1 || after[0].EndedAt == nil {
		t.Errorf("manifest after the run = %+v, want one marked ended", after)
	}
	// The run's spool key reaches the sandbox through its env only: it is
	// in the manifest the relay reads, never in the workspace.
	key := (*during)[0].Key
	if len(key) != 64 {
		t.Fatalf("manifest spool key = %q, want 256 bits hex-encoded", key)
	}
	if got := (*env)[crux.SandboxReviewSpoolKeyEnv]; got != key {
		t.Errorf("sandbox %s = %q, want the manifest's key", crux.SandboxReviewSpoolKeyEnv, got)
	}
	if sandbox.IsCredentialName(crux.SandboxReviewSpoolKeyEnv) {
		t.Errorf("%s would be stripped by the launchers' credential filter", crux.SandboxReviewSpoolKeyEnv)
	}
	_ = filepath.WalkDir(res.Workspace, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, rerr := os.ReadFile(p); rerr == nil && strings.Contains(string(b), key) {
				t.Errorf("spool key written into the workspace at %s", p)
			}
		}
		return nil
	})
}

// Each kick mints its own key, so a request signed in one run is refused in
// any other.
func TestSandboxExecutorMintsAKeyPerKick(t *testing.T) {
	manifestDir := filepath.Join(t.TempDir(), "sandbox-review-spools")
	keys := map[string]bool{}
	for i := 0; i < 2; i++ {
		launcher, env, _ := spoolProbe(manifestDir)
		exec := &SandboxExecutor{Runner: &sandboxFakeRunner{}, Launcher: launcher}
		if _, err := exec.Run(context.Background(), SandboxKickSpec{
			Agent: "reviewer-spyre", AgentConfig: configSnapshot{Backend: "claude"}, Message: "review #7",
			Org: "o", Repo: "r", WorkspaceDir: t.TempDir(), Image: "img", ReviewSpoolDir: manifestDir,
		}); err != nil {
			t.Fatalf("Run: %v", err)
		}
		keys[(*env)[crux.SandboxReviewSpoolKeyEnv]] = true
	}
	if len(keys) != 2 || keys[""] {
		t.Errorf("spool keys across two kicks = %v, want two distinct keys", keys)
	}
}

// POSITIVE CONTROL: with no manifest dir the sandbox gets no spool and no
// env, so the assertions above test the registration and not a default.
func TestSandboxExecutorWithoutReviewSpoolDirRegistersNothing(t *testing.T) {
	root := t.TempDir()
	launcher, env, _ := spoolProbe(filepath.Join(t.TempDir(), "unused"))
	exec := &SandboxExecutor{Runner: &sandboxFakeRunner{}, Launcher: launcher}
	res, err := exec.Run(context.Background(), SandboxKickSpec{
		Agent: "scanner", AgentConfig: configSnapshot{Backend: "claude"}, Message: "fix",
		Org: "o", Repo: "r", WorkspaceDir: root, Image: "img",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := (*env)[crux.SandboxReviewSpoolEnv]; ok {
		t.Errorf("sandbox got %s without a manifest dir", crux.SandboxReviewSpoolEnv)
	}
	if _, err := os.Stat(filepath.Join(res.Workspace, crux.SandboxReviewSpoolRelPath)); !os.IsNotExist(err) {
		t.Errorf("spool created without a manifest dir: %v", err)
	}
}

// A request the sandbox writes as it exits is waited for: Run does not
// return (and a run-stage worktree is not removed) until the relay has taken
// it, bounded by the drain timeout.
func TestSandboxExecutorWaitsForTheRelayToDrainTheSpool(t *testing.T) {
	oldTimeout, oldPoll := reviewSpoolDrainTimeout, reviewSpoolDrainPoll
	reviewSpoolDrainTimeout, reviewSpoolDrainPoll = 10*time.Second, 5*time.Millisecond
	t.Cleanup(func() { reviewSpoolDrainTimeout, reviewSpoolDrainPoll = oldTimeout, oldPoll })

	root := t.TempDir()
	manifestDir := filepath.Join(t.TempDir(), "sandbox-review-spools")
	launcher, _, _ := spoolProbe(manifestDir)
	launcher.request = `{"repo":"o/r","number":7,"event":"comment","body":"x"}`
	launcher.written = make(chan string, 1)
	var consumed atomic.Bool
	go func() {
		// The relay: take the request once the sandbox has written it. The
		// flag is set BEFORE the remove, so a Run that returned only after
		// the spool drained always sees it.
		var path string
		select {
		case path = <-launcher.written:
		case <-time.After(5 * time.Second):
			return
		}
		// Deliberate hold, not a wait on a condition: it gives Run time to
		// return if it did NOT wait for the drain, so that regression fails
		// this test instead of racing the remove below.
		time.Sleep(50 * time.Millisecond)
		consumed.Store(true)
		_ = os.Remove(path)
	}()
	exec := &SandboxExecutor{Runner: &sandboxFakeRunner{}, Launcher: launcher}
	res, err := exec.Run(context.Background(), SandboxKickSpec{
		Agent: "reviewer-spyre", AgentConfig: configSnapshot{Backend: "claude"}, Message: "review",
		Org: "o", Repo: "r", WorkspaceDir: root, Image: "img", ReviewSpoolDir: manifestDir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !consumed.Load() {
		t.Fatal("Run returned before the relay consumed the spooled request")
	}
	if reviewSpoolPending(filepath.Join(res.Workspace, crux.SandboxReviewSpoolRelPath)) {
		t.Error("spool still pending after Run")
	}
}

func TestReviewSpoolAwaitDrainGivesUpAtTheTimeout(t *testing.T) {
	oldTimeout, oldPoll := reviewSpoolDrainTimeout, reviewSpoolDrainPoll
	reviewSpoolDrainTimeout, reviewSpoolDrainPoll = 30*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { reviewSpoolDrainTimeout, reviewSpoolDrainPoll = oldTimeout, oldPoll })

	spool := t.TempDir()
	if err := os.WriteFile(filepath.Join(spool, "r.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Results and quarantined requests are not pending.
	for _, name := range []string{"done.result.json", "x.json.bad", "y.json.tmp"} {
		if err := os.WriteFile(filepath.Join(spool, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := &reviewSpoolHandle{entry: crux.SandboxReviewSpool{Spool: spool}}
	if h.awaitDrain(context.Background()) {
		t.Fatal("awaitDrain reported drained with a request still in the spool")
	}
	if err := os.Remove(filepath.Join(spool, "r.json")); err != nil {
		t.Fatal(err)
	}
	if !h.awaitDrain(context.Background()) {
		t.Fatal("awaitDrain reported pending with only results and quarantined files left")
	}
}

// The manager gives every sandbox kick — Job runtime included — a spool
// registered under its own work dir, where the relay looks.
func TestJobRuntimeKickRegistersReviewSpoolUnderWorkDir(t *testing.T) {
	workDir := t.TempDir()
	t.Setenv("HIVE_WORK_DIR", workDir)
	manifestDir := crux.SandboxReviewSpoolsDir(workDir)
	launcher, env, during := spoolProbe(manifestDir)

	yes := true
	cfg := map[string]config.AgentConfig{"reviewer": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{
		Enabled: &yes, Runtime: config.SandboxRuntimeJob}}}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "torch-spyre", Repos: []string{"spyre-inference"}})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "card-image", WorkspaceDir: t.TempDir(),
		Job: &config.SandboxJobConfig{WorkspaceClaim: "hive-data"}})
	m.setSandboxJobLauncherFactoryForTest(func(config.SandboxJobConfig) sandbox.Launcher { return launcher })
	m.setSandboxRunnerForTest(&sandboxFakeRunner{})
	if err := m.Start(context.Background(), "reviewer"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.SendKick("reviewer", "review #7 on the card"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	waitIdle(t, m, "reviewer")

	launcher.mu.Lock()
	defer launcher.mu.Unlock()
	if (*env)[crux.SandboxReviewSpoolEnv] == "" {
		t.Errorf("job sandbox got no %s", crux.SandboxReviewSpoolEnv)
	}
	if len(*during) != 1 || (*during)[0].Agent != "reviewer" {
		t.Errorf("manifests under %s during the job = %+v, want one for reviewer", manifestDir, *during)
	}
}
