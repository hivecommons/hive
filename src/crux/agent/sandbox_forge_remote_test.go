package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/pushbroker"
)

// forgeCloneRunner records the clone URL and the credential-store line the
// clone was given, then behaves like sandboxFakeRunner.
type forgeCloneRunner struct {
	*sandboxFakeRunner
	mu       sync.Mutex
	cloneURL string
	credLine string
}

func (r *forgeCloneRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	effective := stripGitConfigArgs(args)
	if name == "git" && len(effective) >= 4 && effective[0] == "clone" {
		r.mu.Lock()
		r.cloneURL = effective[len(effective)-2]
		for _, a := range args {
			if path, ok := strings.CutPrefix(a, "credential.helper=store --file="); ok {
				b, _ := os.ReadFile(path)
				r.credLine = string(b)
			}
		}
		r.mu.Unlock()
	}
	return r.sandboxFakeRunner.Run(ctx, dir, env, name, args...)
}

type recordingTierMinter struct {
	mu    *sync.Mutex
	tiers *[]string
	tier  string
}

func (m recordingTierMinter) MintPushToken(context.Context, string) (string, error) {
	m.mu.Lock()
	*m.tiers = append(*m.tiers, m.tier)
	m.mu.Unlock()
	return "fj_clone_token", nil
}

func (m recordingTierMinter) WithTier(tier agentmode.TokenTier) pushbroker.TokenMinter {
	m.tier = string(tier)
	return m
}

func sandboxForgeManager(t *testing.T, runner sandboxCommandRunner) *Manager {
	t.Helper()
	yes := true
	cfg := map[string]config.AgentConfig{
		"scanner": {Backend: "claude", Sandbox: &config.AgentSandboxOverride{Enabled: &yes}},
	}
	m := NewManager(cfg, quietTestLogger(), ProjectContext{Org: "team", Repos: []string{"app"}, ACMMLevel: 1, PRsAllowed: true})
	m.SetSandboxConfig(config.AgentSandboxConfig{Enabled: true, Image: "agent-image", WorkspaceDir: t.TempDir()})
	m.SetSandboxLauncher(sandboxFakeLauncher{})
	m.setSandboxRunnerForTest(runner)
	m.SetSandboxPushMinter(sandboxFakeMinter{})
	m.SetSandboxPRClient(&sandboxFakePR{})
	return m
}

// A kick on a Forgejo repo clones from the forge with the forge minter's
// token, narrowed to the agent's tier, and the GitHub App token never appears
// in the credential the clone gets.
func TestManagerSandboxClonesFromTheRepoForge(t *testing.T) {
	runner := &forgeCloneRunner{sandboxFakeRunner: &sandboxFakeRunner{commits: 1}}
	m := sandboxForgeManager(t, runner)
	var mu sync.Mutex
	var tiers []string
	var asked []string
	m.SetSandboxForgeRemotes(func(repo string) (pushbroker.ForgeRemote, bool, error) {
		asked = append(asked, repo)
		return pushbroker.ForgeRemote{BaseURL: "https://forgejo.example.com/", Minter: recordingTierMinter{mu: &mu, tiers: &tiers}}, true, nil
	})
	if err := m.Start(context.Background(), "scanner"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.SendKick("scanner", "fix"); err != nil {
		t.Fatalf("SendKick: %v", err)
	}
	waitFor(t, "sandbox agent to complete", func() bool { return m.AllStatuses()["scanner"].State == StateIdle })
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(asked) == 0 || asked[0] != "team/app" {
		t.Fatalf("resolver asked for %v, want team/app", asked)
	}
	if runner.cloneURL != "https://forgejo.example.com/team/app.git" {
		t.Fatalf("clone URL = %q", runner.cloneURL)
	}
	if runner.credLine != "https://x-access-token:fj_clone_token@forgejo.example.com\n" {
		t.Fatalf("credential line = %q", runner.credLine)
	}
	if strings.Contains(runner.credLine, "ghs_fake") || strings.Contains(runner.credLine, "github.com") {
		t.Fatalf("GitHub credential used for a forge clone: %q", runner.credLine)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tiers) == 0 || tiers[0] == "" {
		t.Fatalf("forge minter was not narrowed to a tier: %v", tiers)
	}
}

// A repo the resolver refuses (a forge hive cannot clone from) is not kicked.
func TestManagerSandboxRefusesAnUnsupportedForge(t *testing.T) {
	runner := &forgeCloneRunner{sandboxFakeRunner: &sandboxFakeRunner{}}
	m := sandboxForgeManager(t, runner)
	m.SetSandboxForgeRemotes(func(string) (pushbroker.ForgeRemote, bool, error) {
		return pushbroker.ForgeRemote{}, false, errors.New("gitea clone not implemented")
	})
	if err := m.Start(context.Background(), "scanner"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.SendKick("scanner", "fix"); err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("SendKick err = %v, want the forge refusal", err)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.cloneURL != "" {
		t.Fatalf("refused repo was cloned from %q", runner.cloneURL)
	}
}

func TestSandboxForgeRemoteLocked(t *testing.T) {
	m := &Manager{}
	if _, ok, err := m.sandboxForgeRemoteLocked("team/app"); ok || err != nil {
		t.Fatalf("no resolver: ok=%v err=%v", ok, err)
	}
	m.sandboxGit.forgeRemotes = func(string) (pushbroker.ForgeRemote, bool, error) { return pushbroker.ForgeRemote{}, false, nil }
	if _, ok, err := m.sandboxForgeRemoteLocked("team/app"); ok || err != nil {
		t.Fatalf("github repo: ok=%v err=%v", ok, err)
	}
	m.sandboxGit.forgeRemotes = func(string) (pushbroker.ForgeRemote, bool, error) {
		return pushbroker.ForgeRemote{BaseURL: "https://u:p@forgejo.example.com", Minter: sandboxFakeMinter{}}, true, nil
	}
	if _, _, err := m.sandboxForgeRemoteLocked("team/app"); err == nil {
		t.Fatal("invalid forge URL accepted")
	}
	m.sandboxGit.forgeRemotes = func(string) (pushbroker.ForgeRemote, bool, error) {
		return pushbroker.ForgeRemote{BaseURL: "https://forgejo.example.com"}, true, nil
	}
	if _, _, err := m.sandboxForgeRemoteLocked("team/app"); err == nil || !strings.Contains(err.Error(), "no token minter") {
		t.Fatalf("missing minter err = %v", err)
	}
}

func TestSandboxRepoURL(t *testing.T) {
	if got, err := sandboxRepoURL(SandboxKickSpec{Org: "team", Repo: "app"}); err != nil || got != "https://github.com/team/app.git" {
		t.Fatalf("github url = %q, %v", got, err)
	}
	if got, err := sandboxRepoURL(SandboxKickSpec{Org: "team", Repo: "app", ForgeBaseURL: "https://forgejo.example.com"}); err != nil || got != "https://forgejo.example.com/team/app.git" {
		t.Fatalf("forgejo url = %q, %v", got, err)
	}
	if _, err := sandboxRepoURL(SandboxKickSpec{Org: "team", Repo: "app", ForgeBaseURL: "ftp://x"}); err == nil {
		t.Fatal("invalid forge URL accepted")
	}
	if _, err := sandboxRepoURL(SandboxKickSpec{Org: "team", Repo: "a/b"}); err == nil {
		t.Fatal("malformed repo accepted")
	}
}

func TestSandboxCloneAuthArgsBadToken(t *testing.T) {
	e := &SandboxExecutor{CloneMinter: fixedMinter("bad@token")}
	if _, _, err := e.cloneAuthArgs(context.Background(), "https://forgejo.example.com", "team/app", t.TempDir()); err == nil {
		t.Fatal("token that cannot sit in a credential URL accepted")
	}
}

type fixedMinter string

func (m fixedMinter) MintPushToken(context.Context, string) (string, error) { return string(m), nil }

func TestUserSessionWorkspaceRemote(t *testing.T) {
	r := &UserSessionRuntime{cloneMinter: sandboxFakeMinter{}}
	base, minter, err := r.workspaceRemote("team/app")
	if err != nil || base != pushbroker.DefaultForgeBaseURL || minter != (sandboxFakeMinter{}) {
		t.Fatalf("no resolver: %q %v %v", base, minter, err)
	}
	r.SetForgeRemotes(func(repo string) (pushbroker.ForgeRemote, bool, error) {
		switch repo {
		case "team/app":
			return pushbroker.ForgeRemote{BaseURL: "https://forgejo.example.com/", Minter: fixedMinter("fj")}, true, nil
		case "team/bad":
			return pushbroker.ForgeRemote{BaseURL: "nope"}, true, nil
		case "team/gitea":
			return pushbroker.ForgeRemote{}, false, errors.New("not implemented")
		}
		return pushbroker.ForgeRemote{}, false, nil
	})
	base, minter, err = r.workspaceRemote("team/app")
	if err != nil || base != "https://forgejo.example.com" || minter != fixedMinter("fj") {
		t.Fatalf("forgejo: %q %v %v", base, minter, err)
	}
	if base, _, err := r.workspaceRemote("team/gh"); err != nil || base != pushbroker.DefaultForgeBaseURL {
		t.Fatalf("github: %q %v", base, err)
	}
	for _, repo := range []string{"team/bad", "team/gitea"} {
		if _, _, err := r.workspaceRemote(repo); err == nil {
			t.Fatalf("%s: want an error", repo)
		}
	}
}
