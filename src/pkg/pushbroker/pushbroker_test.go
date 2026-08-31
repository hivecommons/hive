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

// kubestellar/hive#5116: five agent-authored PRs across four languages
// failed CI formatter gates (gofmt, cargo fmt, prettier) on a trailing blank
// line the underlying coding CLI left at EOF. Hive has no writer of its own
// in this path — the sandboxed CLI writes the file directly — so the broker
// normalises it at the one point hive already controls: right before the
// diff leaves the sandbox.
func TestBrokerStripsTrailingBlankLineBeforePush(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "kubestellar/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed {
		t.Fatal("Pushed=false")
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "main.go"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if want := "package main\n\nfunc main() {}\n"; string(got) != want {
		t.Fatalf("trailing blank line not stripped: got %q, want %q", got, want)
	}
}

// A file already ending in a single newline — the common, correctly
// formatted case — must be left byte-identical and must not needlessly
// amend the commit.
func TestBrokerLeavesCorrectlyFormattedFileAlone(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	preAmendHead := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "kubestellar/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if res.Commit != preAmendHead {
		t.Fatalf("commit changed for an already-clean file: before=%s after=%s", preAmendHead, res.Commit)
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "main.go"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if want := "package main\n\nfunc main() {}\n"; string(got) != want {
		t.Fatalf("file mutated when it should not have been: got %q", got)
	}
}

// A file with no trailing newline at all is a different style question than
// #5116's "...\n\n" defect and must not be touched by this normalisation.
func TestBrokerDoesNotAddMissingTrailingNewline(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}")
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "kubestellar/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	if !res.Pushed {
		t.Fatal("Pushed=false")
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "main.go"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if want := "package main\n\nfunc main() {}"; string(got) != want {
		t.Fatalf("file with no trailing newline was mutated: got %q", got)
	}
}

// A binary file that happens to end in two 0x0a bytes must not be reinterpreted
// as text and truncated — the NUL-sniff heuristic exists precisely so a binary
// diff never gets treated like source.
func TestBrokerLeavesBinaryFileWithTrailingNewlinesAlone(t *testing.T) {
	dir := initRepo(t)
	binary := []byte{0x00, 0x01, 0x02, '\n', '\n'}
	path := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(path, binary, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "blob.bin")
	runGit(t, dir, "commit", "-m", "binary")
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "kubestellar/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !slices.Equal(got, binary) {
		t.Fatalf("binary file mutated: got %v, want %v", got, binary)
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
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
