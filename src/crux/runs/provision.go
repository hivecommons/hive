package runs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/sandbox"
	"github.com/hivecommons/hive/crux/upstreamproxy"
)

// gitEnvAllowlist is what the clone may see of the service's environment.
var gitEnvAllowlist = []string{"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR"}

// WorkspaceDirName is the workspace subdirectory of a run's directory when
// the ad-hoc worktree form provisions one.
const WorkspaceDirName = "workspace"

// Provisioner materialises the ad-hoc worktree form under dir and returns
// the workspace path. It is the stand-in for the checkout service (S1):
// when that lands, the service delegates to it and worktrees get an owner,
// a size cap and a TTL.
type Provisioner interface {
	Provision(ctx context.Context, dir string, wt api.ContainedRunWorktree) (string, error)
}

// Runner executes one command in dir and returns its combined output; the
// default runs it, tests substitute a script.
type Runner func(ctx context.Context, dir, name string, args ...string) ([]byte, error)

// GitProvisioner clones the repository with git and pins the commit.
type GitProvisioner struct {
	Run Runner
}

// Provision clones wt.Repo into dir/workspace with every ref, detaches at
// wt.Commit and verifies HEAD is that commit. No credential reaches git:
// the environment is the service's and the repository must be public.
func (p GitProvisioner) Provision(ctx context.Context, dir string, wt api.ContainedRunWorktree) (string, error) {
	path := filepath.Join(dir, WorkspaceDirName)
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return "", err
	}
	run := p.Run
	if run == nil {
		run = execRunner
	}
	if out, err := run(ctx, dir, "git", "clone", "--no-checkout", "--", wt.Repo, path); err != nil {
		return "", fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := run(ctx, path, "git", "checkout", "--detach", wt.Commit); err != nil {
		_ = os.RemoveAll(path)
		return "", fmt.Errorf("git checkout %s: %w: %s", wt.Commit, err, strings.TrimSpace(string(out)))
	}
	head, err := run(ctx, path, "git", "rev-parse", "HEAD")
	if err != nil {
		_ = os.RemoveAll(path)
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	if got := strings.TrimSpace(string(head)); got != wt.Commit {
		_ = os.RemoveAll(path)
		return "", fmt.Errorf("checkout pinned %s but HEAD is %s", wt.Commit, got)
	}
	return path, nil
}

func execRunner(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	// Never prompt and never carry a credential: the ad-hoc form is for
	// public repositories on the allowlist, so git gets the sandbox's
	// credential-free environment plus a no-prompt switch.
	env := append(sandbox.SanitizedEnv(os.Environ(), gitEnvAllowlist, nil), "GIT_TERMINAL_PROMPT=0")
	// The sanitized environment drops HTTPS_PROXY; re-express the upstream
	// forward-proxy rules as scoped git config so clones of hosts behind a
	// corporate proxy still work.
	cmd.Env = append(env, upstreamproxy.Default().GitEnv()...)
	return cmd.CombinedOutput()
}
