// Package pushbroker performs the trusted, credentialed post-step for sandboxed agents.
package pushbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
	DefaultRemote = "origin"
	DefaultTier   = "contributor"
)

var DefaultProtectedPaths = []string{
	".github/workflows/",
	"bin/gh-wrapper.sh",
	"deploy/bin/gh-wrapper.sh",
	"policies/",
	"hive.yaml.dashboard",
	"OWNERS",
	".github/OWNERS",
}

// pushTokenEnvVar carries the minted push token to git out-of-band (audit F5).
// It is deliberately NOT in credentialEnvPrefixes: that list strips INHERITED
// credentials from the push environment, whereas this value is supplied by the
// broker itself after that filter runs.
const pushTokenEnvVar = "HIVE_PUSH_TOKEN"

// pushCredentialHelper is a git credential helper that echoes the token from
// the environment. Only the variable NAME appears here, so nothing secret
// reaches argv (where it would be world-readable via /proc/<pid>/cmdline) or
// disk. The leading "!" makes git run it through the shell.
const pushCredentialHelper = `!f() { echo username=x-access-token; echo "password=$` + pushTokenEnvVar + `"; }; f`

var credentialEnvPrefixes = []string{
	"GITHUB_TOKEN=", "GH_TOKEN=", "HIVE_GITHUB_TOKEN=", "COPILOT_GITHUB_TOKEN=",
	"GIT_ASKPASS=", "SSH_ASKPASS=",
}

type TokenMinter interface {
	MintPushToken(ctx context.Context, repo string) (string, error)
}

type CommandRunner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd.CombinedOutput()
}

type GitHubAppMinter struct {
	Auth *ghpkg.AppAuth
	Tier string
}

func (m GitHubAppMinter) MintPushToken(ctx context.Context, repo string) (string, error) {
	if m.Auth == nil {
		return "", errors.New("pushbroker: nil GitHub App auth")
	}
	tier := strings.TrimSpace(m.Tier)
	if tier == "" {
		tier = DefaultTier
	}
	repos := []string(nil)
	if _, name, ok := strings.Cut(strings.TrimSpace(repo), "/"); ok && name != "" {
		repos = []string{name}
	}
	return m.Auth.ScopedTokenForRepos(ctx, tier, repos)
}

type Broker struct {
	Workspace      string
	Branch         string
	BaseRef        string
	Repo           string
	Remote         string
	ProtectedPaths []string
	Minter         TokenMinter
	Runner         CommandRunner
	Logger         *slog.Logger
	Now            func() time.Time
	Mutation       effects.Boundary
}

type Result struct {
	Workspace       string    `json:"workspace"`
	Repo            string    `json:"repo"`
	Branch          string    `json:"branch"`
	Remote          string    `json:"remote"`
	Commit          string    `json:"commit,omitempty"`
	ChangedFiles    []string  `json:"changed_files,omitempty"`
	ProtectedReject []string  `json:"protected_reject,omitempty"`
	SecretRejected  bool      `json:"secret_rejected"`
	Pushed          bool      `json:"pushed"`
	Error           string    `json:"error,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
}

func (b *Broker) Run(ctx context.Context) (Result, error) {
	res := Result{Workspace: b.Workspace, Repo: b.Repo, Branch: b.Branch, Remote: b.remote(), StartedAt: b.now()}
	defer func() { res.FinishedAt = b.now() }()
	if err := b.validate(); err != nil {
		res.Error = err.Error()
		return res, err
	}
	commit, err := b.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return b.fail(res, fmt.Errorf("reading HEAD: %w", err))
	}
	res.Commit = strings.TrimSpace(string(commit))
	if err := b.rejectEmptyOutgoingCommits(ctx, res.Commit); err != nil {
		return b.fail(res, err)
	}
	baseRef, baseExists := b.pushBase(ctx)
	if remoteRef := b.remoteRef(); remoteRef != baseRef {
		if _, err := b.git(ctx, "rev-parse", "--verify", remoteRef); err == nil {
			if err := b.ensureFastForward(ctx, remoteRef); err != nil {
				return b.fail(res, err)
			}
		}
	} else if baseExists {
		if err := b.ensureFastForward(ctx, baseRef); err != nil {
			return b.fail(res, err)
		}
	}
	if err := b.rejectForgedLaneSignoffs(ctx, baseRef, baseExists); err != nil {
		return b.fail(res, err)
	}

	files, err := b.changedFiles(ctx)
	if err != nil {
		return b.fail(res, err)
	}
	res.ChangedFiles = files
	if len(files) == 0 {
		return b.fail(res, errors.New("pushbroker: no committed changes to push"))
	}
	if rejected := ProtectedPathViolations(files, b.protectedPaths()); len(rejected) > 0 {
		res.ProtectedReject = rejected
		return b.fail(res, fmt.Errorf("pushbroker: protected paths changed: %s", strings.Join(rejected, ", ")))
	}
	// The coding CLI running inside the sandbox writes files with its own
	// tools, not hive's — hive has no writer of its own in this path, so it
	// cannot fix a raw-output defect at the source. It can still normalise
	// the one thing every formatter gate (gofmt, black, prettier, cargo fmt)
	// agrees on before the diff leaves the sandbox: no blank line(s) trailing
	// the final newline (kubestellar/hive#5116). Fixing it here, once, covers
	// every backend and every target-repo language instead of teaching each
	// coding CLI's own formatter to run first.
	if amended, err := b.stripTrailingBlankLines(ctx, files); err != nil {
		return b.fail(res, fmt.Errorf("normalising trailing newlines: %w", err))
	} else if amended {
		commit, err = b.git(ctx, "rev-parse", "HEAD")
		if err != nil {
			return b.fail(res, fmt.Errorf("reading HEAD after newline normalisation: %w", err))
		}
		res.Commit = strings.TrimSpace(string(commit))
		if err := b.rejectEmptyOutgoingCommits(ctx, res.Commit); err != nil {
			return b.fail(res, err)
		}
		files, err = b.changedFiles(ctx)
		if err != nil {
			return b.fail(res, err)
		}
		res.ChangedFiles = files
		if len(files) == 0 {
			return b.fail(res, errors.New("pushbroker: no committed changes to push"))
		}
	}
	diff, err := b.outgoingDiff(ctx)
	if err != nil {
		return b.fail(res, err)
	}
	if loc := logscrub.TokenPattern.FindString(diff); loc != "" {
		res.SecretRejected = true
		return b.fail(res, errors.New("pushbroker: outgoing diff contains a token-like secret"))
	}
	token, err := b.Minter.MintPushToken(ctx, b.Repo)
	if err != nil {
		return b.fail(res, fmt.Errorf("minting push token: %w", err))
	}
	if strings.TrimSpace(token) == "" {
		return b.fail(res, errors.New("pushbroker: minter returned empty token"))
	}
	// SECURITY (audit F5, CWE-214): the push token must never appear in git's
	// argv. This used to pass it as `-c http.extraHeader=Authorization: Bearer
	// <token>`, which lands in /proc/<pid>/cmdline — world-readable, so any
	// other UID on the box could read a live push credential while the push
	// ran. Agents run as their own UIDs in this container, so that is a real
	// cross-tenant read, not a theoretical one.
	//
	// Instead pass the token through the environment (visible only to the
	// process owner via /proc/<pid>/environ) and have a credential helper echo
	// it back to git. The helper text itself carries only the variable NAME, so
	// the secret stays out of argv and off disk.
	args := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "credential.helper=" + pushCredentialHelper,
		"push", "--no-verify", b.remote(), "HEAD:refs/heads/" + b.Branch,
	}
	// Append AFTER PushEnv: it strips inherited credential variables, and this
	// one is deliberately supplied rather than inherited.
	env := append(PushEnv(os.Environ()), pushTokenEnvVar+"="+token)
	_, err = effects.Execute(ctx, b.Mutation, effects.Claim{
		Repo:   b.Repo,
		Kind:   effects.KindBranchPush,
		Target: b.Branch,
		Inputs: map[string]string{"commit": res.Commit, "remote": b.remote()},
	}, func(ctx context.Context) (effects.Result, error) {
		_, runErr := b.runner().Run(ctx, b.Workspace, env, "git", args...)
		return effects.Result{Provenance: res.Commit}, runErr
	})
	if err != nil {
		return b.fail(res, fmt.Errorf("git push failed: %w", err))
	}
	res.Pushed = true
	return res, nil
}

func (b *Broker) validate() error {
	if strings.TrimSpace(b.Workspace) == "" || strings.TrimSpace(b.Branch) == "" || strings.TrimSpace(b.Repo) == "" {
		return errors.New("pushbroker: workspace, branch, and repo are required")
	}
	if b.Minter == nil {
		return errors.New("pushbroker: token minter is required")
	}
	info, err := os.Stat(b.Workspace)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("pushbroker: workspace is not a directory: %w", err)
	}
	gitDir := filepath.Join(b.Workspace, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		return fmt.Errorf("pushbroker: workspace is not a git repository")
	}
	return nil
}

func (b *Broker) changedFiles(ctx context.Context) ([]string, error) {
	if base := strings.TrimSpace(b.BaseRef); base != "" {
		if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
			out, err := b.git(ctx, "diff", "--name-only", base+"...HEAD")
			return splitLines(out), err
		}
	}
	base := b.remoteRef()
	if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
		out, err := b.git(ctx, "diff", "--name-only", base+"...HEAD")
		return splitLines(out), err
	}
	out, err := b.git(ctx, "diff-tree", "--root", "--no-commit-id", "--name-only", "-r", "HEAD")
	return splitLines(out), err
}

func (b *Broker) pushBase(ctx context.Context) (string, bool) {
	if base := strings.TrimSpace(b.BaseRef); base != "" {
		if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
			return base, true
		}
	}
	base := b.remoteRef()
	if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
		return base, true
	}
	return "", false
}

func (b *Broker) rejectEmptyOutgoingCommits(ctx context.Context, head string) error {
	rangeSpec := "HEAD"
	baseExists := false
	if base := strings.TrimSpace(b.BaseRef); base != "" {
		if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
			rangeSpec = base + "..HEAD"
			baseExists = true
		}
	} else {
		base := b.remoteRef()
		if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
			rangeSpec = base + "..HEAD"
			baseExists = true
		}
	}
	args := []string{"rev-list", "--reverse", rangeSpec}
	if !baseExists {
		out := strings.TrimSpace(head)
		if out == "" {
			return nil
		}
		for _, commit := range []string{out} {
			if empty, err := b.commitHasEmptyTreeDelta(ctx, commit); err != nil {
				return err
			} else if empty {
				return fmt.Errorf("pushbroker: refusing to push empty commit %s; retrigger CI with gh run rerun --failed or workflow_dispatch instead of pushing to the PR branch", shortSHA(commit))
			}
		}
		return nil
	}
	out, err := b.git(ctx, args...)
	if err != nil {
		return fmt.Errorf("reading outgoing commits for empty-commit guard: %w", err)
	}
	for _, commit := range splitLines(out) {
		if empty, err := b.commitHasEmptyTreeDelta(ctx, commit); err != nil {
			return err
		} else if empty {
			return fmt.Errorf("pushbroker: refusing to push empty commit %s; retrigger CI with gh run rerun --failed or workflow_dispatch instead of pushing to the PR branch", shortSHA(commit))
		}
	}
	return nil
}

func (b *Broker) commitHasEmptyTreeDelta(ctx context.Context, commit string) (bool, error) {
	parentsOut, err := b.git(ctx, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return false, fmt.Errorf("reading parents for empty-commit guard: %w", err)
	}
	fields := strings.Fields(string(parentsOut))
	if len(fields) <= 1 {
		_, err = b.runner().Run(ctx, b.Workspace, PushEnv(os.Environ()), "git", "diff-tree", "--quiet", "--root", commit)
		return err == nil, nil
	}
	_, err = b.runner().Run(ctx, b.Workspace, PushEnv(os.Environ()), "git", "diff-tree", "--quiet", fields[1], commit)
	return err == nil, nil
}

func (b *Broker) ensureFastForward(ctx context.Context, base string) error {
	if _, err := b.git(ctx, "merge-base", "--is-ancestor", base, "HEAD"); err != nil {
		return fmt.Errorf("pushbroker: refusing non-fast-forward push to existing branch %q; comment on the PR instead of rewriting history: %w", b.Branch, err)
	}
	return nil
}

var signedOffByRE = regexp.MustCompile(`(?mi)^Signed-off-by:\s*(.*?)\s*<([^<>]+)>\s*$`)

func (b *Broker) rejectForgedLaneSignoffs(ctx context.Context, base string, baseExists bool) error {
	nameOut, err := b.git(ctx, "config", "user.name")
	if err != nil {
		return fmt.Errorf("reading git user.name for sign-off guard: %w", err)
	}
	emailOut, err := b.git(ctx, "config", "user.email")
	if err != nil {
		return fmt.Errorf("reading git user.email for sign-off guard: %w", err)
	}
	laneName := strings.TrimSpace(string(nameOut))
	laneEmail := strings.TrimSpace(string(emailOut))
	if laneName == "" || laneEmail == "" {
		return nil
	}

	var logArgs []string
	if baseExists {
		rangeSpec := base + "..HEAD"
		logArgs = []string{"log", "--format=%H%x00%an%x00%ae%x00%B%x1e", rangeSpec}
	} else {
		logArgs = []string{"log", "-1", "--format=%H%x00%an%x00%ae%x00%B%x1e", "HEAD"}
	}
	out, err := b.git(ctx, logArgs...)
	if err != nil {
		return fmt.Errorf("reading outgoing commits for sign-off guard: %w", err)
	}
	for _, record := range strings.Split(string(out), "\x1e") {
		record = strings.Trim(record, "\n")
		if record == "" {
			continue
		}
		parts := strings.SplitN(record, "\x00", 4)
		if len(parts) < 4 {
			continue
		}
		sha, authorName, authorEmail, msg := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), strings.TrimSpace(parts[2]), parts[3]
		if sameIdentity(authorName, authorEmail, laneName, laneEmail) {
			continue
		}
		for _, match := range signedOffByRE.FindAllStringSubmatch(msg, -1) {
			if len(match) == 3 && sameIdentity(strings.TrimSpace(match[1]), strings.TrimSpace(match[2]), laneName, laneEmail) {
				return fmt.Errorf("pushbroker: refusing to push commit %s authored by %s <%s> with %s's Signed-off-by trailer; leave DCO remediation to the author", shortSHA(sha), authorName, authorEmail, laneName)
			}
		}
	}
	return nil
}

func sameIdentity(name, email, wantName, wantEmail string) bool {
	return strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(wantName)) &&
		strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(wantEmail))
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// stripTrailingBlankLines removes any blank line(s) trailing the final
// newline of each changed, still-present, textual file, leaving exactly one
// trailing newline. It reports whether it amended HEAD.
//
// Scope is deliberately narrow: a file with no trailing newline at all is
// left untouched (that is a different, less universally-enforced style rule,
// not the "...\n\n" defect #5116 reports), a file already ending in exactly
// one newline is untouched, and a file containing a NUL byte in its first 8KB
// — the same binary heuristic git itself uses — is never rewritten as text.
// Only files still present on disk are considered — a changed file that was
// deleted has nothing to normalise. This deliberately makes no additional git
// call: everything it needs comes from the file list Run() already fetched
// and the file content on disk.
func (b *Broker) stripTrailingBlankLines(ctx context.Context, files []string) (bool, error) {
	var touched []string
	for _, rel := range files {
		abs := filepath.Join(b.Workspace, rel)
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() {
			continue // deleted, or a directory entry from a rename — nothing to normalise
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return false, fmt.Errorf("reading %s: %w", rel, err)
		}
		if looksBinary(data) {
			continue
		}
		normalized, changed := trimTrailingBlankLines(data)
		if !changed {
			continue
		}
		if err := os.WriteFile(abs, normalized, info.Mode().Perm()); err != nil {
			return false, fmt.Errorf("writing %s: %w", rel, err)
		}
		touched = append(touched, rel)
	}
	if len(touched) == 0 {
		return false, nil
	}
	addArgs := append([]string{"add", "--"}, touched...)
	if _, err := b.git(ctx, addArgs...); err != nil {
		return false, err
	}
	if _, err := b.git(ctx, "commit", "--amend", "--no-edit"); err != nil {
		if isEmptyAmendError(err) {
			return false, errors.New("pushbroker: refusing to push a commit made empty by broker normalisation; retrigger CI with gh run rerun --failed or workflow_dispatch instead of pushing to the PR branch")
		}
		return false, err
	}
	if b.Logger != nil {
		b.Logger.Info("pushbroker normalised trailing blank lines", "repo", b.Repo, "branch", b.Branch, "files", touched)
	}
	return true, nil
}

func isEmptyAmendError(err error) bool {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	return strings.Contains(msg, "would make") && strings.Contains(msg, "it empty")
}

// looksBinary reports whether data appears to be non-text, using the same
// "NUL byte in a leading sample" heuristic git itself applies (see git's
// buffer_is_binary), so this classifies files the same way `git diff` would
// without shelling out to ask it.
func looksBinary(data []byte) bool {
	const sample = 8000
	if len(data) > sample {
		data = data[:sample]
	}
	return bytes.IndexByte(data, 0) != -1
}

// trimTrailingBlankLines collapses one-or-more blank lines at end-of-file
// down to a single trailing newline. It leaves data with no trailing newline
// untouched entirely — that is not the defect being fixed here.
func trimTrailingBlankLines(data []byte) ([]byte, bool) {
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return data, false
	}
	trimmed := bytes.TrimRight(data, "\n")
	// TrimRight on an all-newline file would strip everything; that is not a
	// realistic agent-authored source file, but guard it anyway rather than
	// emit an empty file.
	want := append(append([]byte(nil), trimmed...), '\n')
	if len(trimmed) == 0 {
		want = []byte("\n")
	}
	if bytes.Equal(want, data) {
		return data, false
	}
	return want, true
}

func (b *Broker) outgoingDiff(ctx context.Context) (string, error) {
	if base := strings.TrimSpace(b.BaseRef); base != "" {
		if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
			out, err := b.git(ctx, "diff", "--no-ext-diff", base+"...HEAD")
			return string(out), err
		}
	}
	base := b.remoteRef()
	if _, err := b.git(ctx, "rev-parse", "--verify", base); err == nil {
		out, err := b.git(ctx, "diff", "--no-ext-diff", base+"...HEAD")
		return string(out), err
	}
	out, err := b.git(ctx, "show", "--format=", "--no-ext-diff", "HEAD")
	return string(out), err
}

func (b *Broker) git(ctx context.Context, args ...string) ([]byte, error) {
	out, err := b.runner().Run(ctx, b.Workspace, PushEnv(os.Environ()), "git", args...)
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (b *Broker) fail(res Result, err error) (Result, error) {
	res.Error = err.Error()
	res.FinishedAt = b.now()
	if b.Logger != nil {
		b.Logger.Warn("pushbroker rejected workspace", "repo", res.Repo, "branch", res.Branch, "error", err)
	}
	return res, err
}

func (b *Broker) runner() CommandRunner {
	if b.Runner != nil {
		return b.Runner
	}
	return ExecRunner{}
}
func (b *Broker) remote() string {
	if b.Remote != "" {
		return b.Remote
	}
	return DefaultRemote
}
func (b *Broker) remoteRef() string { return "refs/remotes/" + b.remote() + "/" + b.Branch }
func (b *Broker) protectedPaths() []string {
	if len(b.ProtectedPaths) > 0 {
		return b.ProtectedPaths
	}
	return DefaultProtectedPaths
}
func (b *Broker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now().UTC()
}

func PushEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		blocked := false
		for _, prefix := range credentialEnvPrefixes {
			if strings.HasPrefix(entry, prefix) {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, entry)
		}
	}
	return out
}

func ProtectedPathViolations(files, protected []string) []string {
	var rejected []string
	for _, file := range files {
		clean := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(file)), "./")
		for _, guard := range protected {
			g := strings.TrimPrefix(filepath.ToSlash(filepath.Clean(guard)), "./")
			if strings.HasSuffix(guard, "/") || strings.HasSuffix(g, "/") {
				g = strings.TrimSuffix(g, "/") + "/"
				if strings.HasPrefix(clean, g) {
					rejected = append(rejected, clean)
					break
				}
				continue
			}
			if clean == g || strings.HasPrefix(clean, strings.TrimSuffix(g, "/")+"/") && strings.HasSuffix(guard, "/") {
				rejected = append(rejected, clean)
				break
			}
		}
	}
	return rejected
}

func splitLines(out []byte) []string {
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil
	}
	parts := strings.Split(string(trimmed), "\n")
	files := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			files = append(files, p)
		}
	}
	return files
}
