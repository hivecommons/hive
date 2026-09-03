package pushbroker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}}).Run(context.Background())
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
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}}).Run(context.Background())
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
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_minted_push_token"}, Runner: r}).Run(context.Background())
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

	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{token}, Runner: r}).Run(context.Background())
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
	preAmendHead := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	r := &recordingRunner{}
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r, Logger: logger}).Run(context.Background())
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
	// The amend path itself: HEAD must actually have moved (the strip amended
	// the commit), and the reported Commit must be the NEW head, not the one
	// the CLI originally committed with the trailing blank line still in it.
	if res.Commit == preAmendHead {
		t.Fatalf("commit did not change: still %s — normalisation amend did not run", preAmendHead)
	}
	postAmendHead := strings.TrimSpace(runGitOutput(t, dir, "rev-parse", "HEAD"))
	if res.Commit != postAmendHead {
		t.Fatalf("Result.Commit = %s, want the post-amend HEAD %s", res.Commit, postAmendHead)
	}
	if !strings.Contains(buf.String(), "pushbroker normalised trailing blank lines") {
		t.Fatalf("normalisation was not logged: %s", buf.String())
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
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
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
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
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
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
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

// looksBinary samples only the leading 8000 bytes (git's own heuristic
// window). A large, entirely clean text file must not be misclassified as
// binary just because it exceeds that sample size, and — the actual
// uncovered branch — the truncation itself (data[:sample]) must execute
// without a NUL anywhere in the full file.
func TestBrokerNormalisesLargeCleanTextFile(t *testing.T) {
	dir := initRepo(t)
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	body := b.String()                        // > 8000 bytes, well-formed text, single trailing newline
	writeCommit(t, dir, "big.txt", body+"\n") // add one extra blank line at EOF
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "big.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != body {
		t.Fatalf("large clean text file not normalised correctly (len got=%d want=%d)", len(got), len(body))
	}
}

// A large binary file whose only NUL byte falls INSIDE the leading 8000-byte
// sample must still be caught — proving the sample window, not the whole
// file, is what looksBinary actually inspects.
func TestBrokerDetectsBinaryWithinLeadingSample(t *testing.T) {
	dir := initRepo(t)
	data := make([]byte, 9000)
	for i := range data {
		data[i] = 'x'
	}
	data[100] = 0x00 // well inside the 8000-byte sample
	data[len(data)-1] = '\n'
	data[len(data)-2] = '\n' // trailing blank line, which a text path WOULD strip
	path := filepath.Join(dir, "blob.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "blob.bin")
	runGit(t, dir, "commit", "-m", "large binary")
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !slices.Equal(got, data) {
		t.Fatal("large binary file with a NUL in its leading sample was mutated")
	}
}

// A file consisting entirely of newlines is the degenerate input
// trimTrailingBlankLines guards explicitly: bytes.TrimRight would strip
// everything, so the function must fall back to a single newline rather than
// emit an empty file.
func TestBrokerCollapsesAllNewlineFileToOneNewline(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "blank.txt", "\n\n\n\n")
	r := &recordingRunner{}
	res, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v (res=%+v)", err, res)
	}
	got, readErr := os.ReadFile(filepath.Join(dir, "blank.txt"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "\n" {
		t.Fatalf("all-newline file = %q, want a single newline (not empty)", got)
	}
}

// failingRunner lets a test fail one specific git subcommand (matched by a
// substring of the joined argv) while every other invocation proceeds through
// the real git binary — used to exercise stripTrailingBlankLines' own error
// paths (git add / git commit --amend failing) without hand-rolling a full
// scripted double for the whole broker.
type failingRunner struct {
	failSubstr string
	failErr    error
}

func (f *failingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	if name == "git" && strings.Contains(strings.Join(args, " "), f.failSubstr) {
		return []byte("fatal: injected failure"), f.failErr
	}
	if name == "git" && slices.Contains(args, "push") {
		return []byte("ok"), nil
	}
	return ExecRunner{}.Run(ctx, dir, env, name, args...)
}

// If `git add` fails while staging a normalised file, Run must surface that
// as a wrapped, attributable error rather than silently pushing the
// unnormalised commit.
func TestBrokerSurfacesGitAddFailureDuringNormalisation(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	r := &failingRunner{failSubstr: "add --", failErr: errors.New("disk full")}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "normalising trailing newlines") {
		t.Fatalf("Run error = %v, want it to mention normalising trailing newlines", err)
	}
}

// Same shape for the amend itself: a failed `git commit --amend` must not be
// swallowed.
func TestBrokerSurfacesGitAmendFailureDuringNormalisation(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	r := &failingRunner{failSubstr: "commit --amend", failErr: errors.New("hook rejected")}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "normalising trailing newlines") {
		t.Fatalf("Run error = %v, want it to mention normalising trailing newlines", err)
	}
}

// nthCallFailingRunner fails a matched git subcommand only on its Nth
// occurrence (1-indexed), letting an earlier identical invocation (e.g. the
// FIRST "rev-parse HEAD", before any normalisation) succeed while a LATER one
// (the post-amend re-read) fails. slices/strings-substring matched, same as
// failingRunner.
type nthCallFailingRunner struct {
	failSubstr string
	failOnCall int
	failErr    error
	seen       int
}

func (r *nthCallFailingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	if name == "git" && strings.Contains(strings.Join(args, " "), r.failSubstr) {
		r.seen++
		if r.seen == r.failOnCall {
			return []byte("fatal: injected failure"), r.failErr
		}
	}
	if name == "git" && slices.Contains(args, "push") {
		return []byte("ok"), nil
	}
	return ExecRunner{}.Run(ctx, dir, env, name, args...)
}

// If HEAD cannot be re-read immediately after a successful amend, Run must
// surface that rather than report success with a stale (pre-amend) commit.
func TestBrokerSurfacesReadHeadFailureAfterAmend(t *testing.T) {
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	r := &nthCallFailingRunner{failSubstr: "rev-parse HEAD", failOnCall: 2, failErr: errors.New("index corrupt")}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "reading HEAD after newline normalisation") {
		t.Fatalf("Run error = %v, want it to mention reading HEAD after newline normalisation", err)
	}
}

// A changed file that becomes unreadable between being listed by `git diff
// --name-only` and stripTrailingBlankLines opening it (permission revoked
// mid-run, in practice a filesystem race) must surface as an attributable
// error rather than silently skip normalisation.
func TestBrokerSurfacesReadFailureDuringNormalisation(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores file permissions, so an unreadable-file test cannot fail as designed")
	}
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	path := filepath.Join(dir, "main.go")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o644)
	r := &recordingRunner{}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "normalising trailing newlines") {
		t.Fatalf("Run error = %v, want it to mention normalising trailing newlines", err)
	}
}

// A changed file that cannot be WRITTEN back must surface as an attributable
// error, not a silently unnormalised push. os.WriteFile on an existing path
// opens O_WRONLY|O_TRUNC on the file itself (pushbroker.go's own doc comment
// notes this), so it is the FILE's write permission that has to be revoked —
// a read-only directory alone still permits truncating an existing file on at
// least one common platform, which is what made an earlier version of this
// test pass for the wrong reason.
func TestBrokerSurfacesWriteFailureDuringNormalisation(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores file permissions, so a read-only-file test cannot fail as designed")
	}
	dir := initRepo(t)
	writeCommit(t, dir, "main.go", "package main\n\nfunc main() {}\n\n")
	path := filepath.Join(dir, "main.go")
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o644)
	r := &recordingRunner{}
	_, err := (&Broker{Workspace: dir, Branch: "work", Repo: "hivecommons/hive", Minter: fakeMinter{"ghs_pushbroker"}, Runner: r}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "normalising trailing newlines") {
		t.Fatalf("Run error = %v, want it to mention normalising trailing newlines", err)
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
