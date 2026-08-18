package pushbroker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type fakeMinter struct{ token string }

func (f fakeMinter) MintPushToken(context.Context, string) (string, error) { return f.token, nil }

type recordingRunner struct {
	envOnPush  []string
	argsOnPush []string
}

func (r *recordingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	if name == "git" && slices.Contains(args, "push") {
		r.envOnPush = append([]string(nil), env...)
		r.argsOnPush = append([]string(nil), args...)
		return []byte("ok"), nil
	}
	return ExecRunner{}.Run(ctx, dir, env, name, args...)
}

func TestBrokerRejectsTokenLikeSecretInOutgoingDiff(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "secret.txt", "token=ghp_abcdefghijklmnopqrstuvwxyz\n")
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "kubestellar/hive", Minter: fakeMinter{"ghs_pushbroker"}}).Run(context.Background())
	if err == nil {
		t.Fatal("expected secret rejection")
	}
	if !res.SecretRejected {
		t.Fatalf("SecretRejected=false, err=%v", err)
	}
}

func TestBrokerRejectsProtectedPaths(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, ".github/workflows/ci.yaml", "name: ci\n")
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "kubestellar/hive", Minter: fakeMinter{"ghs_pushbroker"}}).Run(context.Background())
	if err == nil {
		t.Fatal("expected protected path rejection")
	}
	if len(res.ProtectedReject) != 1 || res.ProtectedReject[0] != ".github/workflows/ci.yaml" {
		t.Fatalf("ProtectedReject=%v", res.ProtectedReject)
	}
}

func TestBrokerPushSanitizesCredentialEnvironmentAndWorkspace(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "safe.txt", "hello\n")
	r := &recordingRunner{}
	t.Setenv("GITHUB_TOKEN", "ghp_should_not_leave_hive")
	t.Setenv("HIVE_GITHUB_TOKEN", "ghp_full_token")
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "kubestellar/hive", Minter: fakeMinter{"ghs_minted_push_token"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed {
		t.Fatal("Pushed=false")
	}
	if !slices.Contains(r.argsOnPush, "core.hooksPath=/dev/null") || !slices.Contains(r.argsOnPush, "--no-verify") {
		t.Fatalf("push did not disable hooks: %v", r.argsOnPush)
	}
	for _, env := range r.envOnPush {
		if strings.Contains(env, "should_not_leave_hive") || strings.Contains(env, "full_token") || strings.HasPrefix(env, "GITHUB_TOKEN=") || strings.HasPrefix(env, "HIVE_GITHUB_TOKEN=") {
			t.Fatalf("credential leaked into push env: %q", env)
		}
	}
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), "ghs_minted_push_token") {
			t.Fatalf("minted token written to workspace file %s", path)
		}
		return nil
	})
}

// Audit F5 (CWE-214). The minted push token used to be passed as
// `-c http.extraHeader=Authorization: Bearer <token>`, putting a live
// credential in git's argv and therefore in /proc/<pid>/cmdline, which is
// world-readable. Agents run under their own UIDs in this container, so any of
// them could read another tenant's push token for the duration of the push.
//
// The token must now travel in the environment (owner-readable only) and reach
// git through a credential helper. The sibling sanitize test walks the
// workspace and the inherited environment but never inspected argv, which is
// why this was invisible to a green suite.
func TestF5_PushTokenNeverAppearsInGitArgv(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "safe.txt", "hello\n")
	r := &recordingRunner{}
	const token = "ghs_f5_secret_push_token"

	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "kubestellar/hive", Minter: fakeMinter{token}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed {
		t.Fatal("Pushed=false")
	}

	for _, arg := range r.argsOnPush {
		if strings.Contains(arg, token) {
			t.Fatalf("F5: push token leaked into git argv (readable via /proc/<pid>/cmdline): %q", arg)
		}
	}

	// Positive control: if the token is not actually reaching git, the push is
	// silently unauthenticated and this test would pass for the wrong reason.
	var delivered bool
	for _, env := range r.envOnPush {
		if env == pushTokenEnvVar+"="+token {
			delivered = true
		}
	}
	if !delivered {
		t.Fatalf("F5: token was not delivered via %s — the push would be unauthenticated", pushTokenEnvVar)
	}
	var helper bool
	for _, arg := range r.argsOnPush {
		if strings.HasPrefix(arg, "credential.helper=") {
			helper = true
			if !strings.Contains(arg, pushTokenEnvVar) {
				t.Fatalf("F5: credential helper does not read %s: %q", pushTokenEnvVar, arg)
			}
		}
	}
	if !helper {
		t.Fatal("F5: no credential helper configured — git cannot authenticate from the environment")
	}
}

func TestProtectedPathViolationsTable(t *testing.T) {
	files := []string{"policies/guard.yaml", "OWNERS", "hive.yaml.dashboard", "pkg/safe.go"}
	got := ProtectedPathViolations(files, DefaultProtectedPaths)
	if strings.Join(got, ",") != "policies/guard.yaml,OWNERS,hive.yaml.dashboard" {
		t.Fatalf("got %v", got)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "work")
	runGit(t, dir, "config", "user.email", "hive@example.com")
	runGit(t, dir, "config", "user.name", "Hive Test")
	return dir
}

func writeCommit(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", rel)
	runGit(t, dir, "commit", "-m", "test")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
}
