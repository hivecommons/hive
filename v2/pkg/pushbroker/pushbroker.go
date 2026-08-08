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
	"strings"
	"time"

	ghpkg "github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/logscrub"
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
	"OWNERS",
	".github/OWNERS",
}

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
	args := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "http.extraHeader=Authorization: Bearer " + token,
		"push", "--no-verify", b.remote(), "HEAD:refs/heads/" + b.Branch,
	}
	if _, err := b.runner().Run(ctx, b.Workspace, PushEnv(os.Environ()), "git", args...); err != nil {
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
