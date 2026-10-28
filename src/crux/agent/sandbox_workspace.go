package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/crux/sandbox"
)

// prepareWorkspace clones the target repo and checks out the work branch off
// the base ref. When spec.BaseRef is empty it is RESOLVED from the clone's own
// refs/remotes/origin/HEAD and written back into spec, so the caller opens its
// PR against the same ref it branched from (kubestellar/hive#4928). If that
// resolution fails, prepareWorkspace fails the kick rather than silently
// falling back to "main" — a wrong base is worse than a failed kick, and the
// error surfaces through res.Error into the kick result the operator sees.
func (e *SandboxExecutor) prepareWorkspace(ctx context.Context, spec *SandboxKickSpec) (string, string, error) {
	if strings.TrimSpace(spec.Org) == "" || strings.TrimSpace(spec.Repo) == "" {
		return "", "", errors.New("sandbox workspace prep requires org and repo")
	}
	if strings.TrimSpace(spec.WorkspaceDir) == "" {
		return "", "", errors.New("sandbox workspace root is required")
	}
	if err := os.MkdirAll(spec.WorkspaceDir, 0o770); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(spec.Stage) != "" {
		return e.prepareRunStageWorkspace(ctx, spec)
	}
	workspace := sandbox.WorkspaceDir(spec.WorkspaceDir, spec.Agent, e.now().UnixNano())
	repoURL := fmt.Sprintf("https://github.com/%s/%s.git", spec.Org, spec.Repo)
	authArgs, cleanupAuth, err := e.cloneAuthArgs(ctx, spec.Org+"/"+spec.Repo, spec.WorkspaceDir)
	if err != nil {
		return "", "", err
	}
	defer cleanupAuth()
	cloneArgs := append(append([]string{}, authArgs...), "clone", "--no-checkout", repoURL, workspace)
	if out, err := e.runner().Run(ctx, "", pushbroker.PushEnv(os.Environ()), "git", cloneArgs...); err != nil {
		return "", "", fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(spec.BaseRef) == "" {
		ref, err := e.resolveBaseRef(ctx, workspace)
		if err != nil {
			return "", "", fmt.Errorf("sandbox workspace prep: could not resolve %s/%s's default branch and no explicit base ref was given (refusing to guess %q): %w", spec.Org, spec.Repo, FallbackSandboxBaseRef, err)
		}
		spec.BaseRef = ref
	}
	fetchArgs := append(append([]string{}, authArgs...), "fetch", "origin", spec.BaseRef)
	if out, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", fetchArgs...); err != nil {
		return "", "", fmt.Errorf("git fetch %s: %w: %s", spec.BaseRef, err, strings.TrimSpace(string(out)))
	}
	if out, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "checkout", "-B", spec.Branch, "FETCH_HEAD"); err != nil {
		return "", "", fmt.Errorf("git checkout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	base, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return workspace, strings.TrimSpace(string(base)), nil
}

func (e *SandboxExecutor) prepareRunStageWorkspace(ctx context.Context, spec *SandboxKickSpec) (string, string, error) {
	if strings.TrimSpace(spec.RunKey) == "" || spec.Generation == 0 {
		return "", "", errors.New("sandbox run-stage workspace prep requires run key and generation")
	}
	repoURL := fmt.Sprintf("https://github.com/%s/%s.git", spec.Org, spec.Repo)
	source := runStageSourceRepo(*spec)
	authArgs, cleanupAuth, err := e.cloneAuthArgs(ctx, spec.Org+"/"+spec.Repo, spec.WorkspaceDir)
	if err != nil {
		return "", "", err
	}

	defer cleanupAuth()
	if _, err := os.Stat(filepath.Join(source, ".git")); err != nil {
		if !os.IsNotExist(err) {
			return "", "", err
		}
		if err := os.MkdirAll(filepath.Dir(source), 0o770); err != nil {
			return "", "", err
		}
		cloneArgs := append(append([]string{}, authArgs...), "clone", "--no-checkout", repoURL, source)
		if out, err := e.runner().Run(ctx, "", pushbroker.PushEnv(os.Environ()), "git", cloneArgs...); err != nil {
			return "", "", fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	if strings.TrimSpace(spec.BaseRef) == "" {
		ref, err := e.resolveBaseRef(ctx, source)
		if err != nil {
			return "", "", fmt.Errorf("sandbox workspace prep: could not resolve %s/%s's default branch and no explicit base ref was given (refusing to guess %q): %w", spec.Org, spec.Repo, FallbackSandboxBaseRef, err)
		}
		spec.BaseRef = ref
	}
	fetchArgs := append(append([]string{}, authArgs...), "fetch", "origin", spec.BaseRef)
	if out, err := e.runner().Run(ctx, source, pushbroker.PushEnv(os.Environ()), "git", fetchArgs...); err != nil {
		return "", "", fmt.Errorf("git fetch %s: %w: %s", spec.BaseRef, err, strings.TrimSpace(string(out)))
	}
	mgr := sandbox.WorktreeManager{Runner: func(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
		return e.runner().Run(ctx, dir, pushbroker.PushEnv(os.Environ()), name, args...)
	}}
	wt, err := mgr.Add(ctx, sandbox.WorktreeSpec{
		SourceRepo:   source,
		WorkspaceDir: spec.WorkspaceDir,
		RunKey:       spec.RunKey,
		Stage:        spec.Stage,
		Generation:   spec.Generation,
		TargetRef:    "FETCH_HEAD",
		MaxWorktrees: spec.MaxWorktrees,
	})
	if err != nil {
		return "", "", err
	}
	return wt.Path, wt.SHA, nil
}

func runStageSourceRepo(spec SandboxKickSpec) string {
	return filepath.Join(spec.WorkspaceDir, "_repos", sanitizeBranchPart(spec.Org), sanitizeBranchPart(spec.Repo))
}

// FallbackSandboxBaseRef names the branch resolveBaseRef would have used had
// it not been made an error instead (kubestellar/hive#4928) — kept as a named
// constant purely so the "refusing to guess" error message above never has a
// bare string literal to drift out of sync with intent.
const FallbackSandboxBaseRef = "main"

// resolveBaseRef reports the default branch of the freshly cloned repo. `git
// clone` records the remote's HEAD in refs/remotes/origin/HEAD, so the answer
// is already on disk — no API call, no extra token scope. An unreadable or
// empty answer is returned as an error so the caller can fail the kick rather
// than silently branching off an assumed "main" (kubestellar/hive#4928).
func (e *SandboxExecutor) resolveBaseRef(ctx context.Context, workspace string) (string, error) {
	out, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()),
		"git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("reading refs/remotes/origin/HEAD: %w", err)
	}
	ref := strings.TrimSpace(string(out))
	// symbolic-ref --short renders it as "origin/<branch>".
	if _, branch, found := strings.Cut(ref, "/"); found {
		ref = branch
	}
	if ref == "" {
		return "", errors.New("refs/remotes/origin/HEAD resolved to an empty branch name")
	}
	return ref, nil
}

func (e *SandboxExecutor) cloneAuthArgs(ctx context.Context, repo, dir string) ([]string, func(), error) {
	minter := e.CloneMinter
	if minter == nil {
		minter = e.Minter
	}
	if minter == nil {
		return nil, func() {}, nil
	}
	token, err := minter.MintPushToken(ctx, repo)
	if err != nil {
		return nil, func() {}, fmt.Errorf("minting clone token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return nil, func() {}, errors.New("sandbox workspace prep: minter returned empty clone token")
	}
	path := filepath.Join(dir, ".hive-git-credentials-"+sanitizeBranchPart(repo)+"-"+fmt.Sprint(e.now().UnixNano()))
	line := "https://x-access-token:" + token + "@github.com\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		return nil, func() {}, fmt.Errorf("writing sandbox clone credential helper: %w", err)
	}
	cleanup := func() { _ = os.Remove(path) }
	return []string{"-c", "credential.helper=store --file=" + path}, cleanup, nil
}

func (e *SandboxExecutor) commitsSince(ctx context.Context, workspace, baseSHA string) (string, int, error) {
	head, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "rev-parse", "HEAD")
	if err != nil {
		return "", 0, err
	}
	headSHA := strings.TrimSpace(string(head))
	if headSHA == baseSHA {
		return headSHA, 0, nil
	}
	countOut, err := e.runner().Run(ctx, workspace, pushbroker.PushEnv(os.Environ()), "git", "rev-list", "--count", baseSHA+"..HEAD")
	if err != nil {
		return headSHA, 0, err
	}
	var count int
	_, _ = fmt.Sscanf(strings.TrimSpace(string(countOut)), "%d", &count)
	return headSHA, count, nil
}
