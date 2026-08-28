package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ghpkg "github.com/kubestellar/hive/pkg/github"
)

// Regression coverage for kubestellar/hive#4928 on the sandbox path: the
// executor branched from, and opened its PR against, a hardcoded "main"
// regardless of what the target repository's default branch actually is. `git
// clone` already records the remote's HEAD, so the right answer was on disk the
// whole time.

// baseRefRunner is a git stub that reports a configurable default branch for
// refs/remotes/origin/HEAD and records the ref the executor fetched.
type baseRefRunner struct {
	mu sync.Mutex

	symbolicRef    string // what `git symbolic-ref --short refs/remotes/origin/HEAD` prints
	symbolicRefErr error  // when set, the lookup fails instead

	fetchedRef      string
	symbolicRefRuns int
	revParses       int
}

func (r *baseRefRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	if name != "git" {
		return nil, errors.New("unexpected command")
	}
	effective := stripGitConfigArgs(args)
	joined := strings.Join(effective, " ")
	switch {
	case len(effective) >= 4 && effective[0] == "clone":
		workspace := effective[len(effective)-1]
		return nil, os.MkdirAll(filepath.Join(workspace, ".git"), 0o770)
	case joined == "symbolic-ref --short refs/remotes/origin/HEAD":
		r.mu.Lock()
		r.symbolicRefRuns++
		r.mu.Unlock()
		if r.symbolicRefErr != nil {
			return nil, r.symbolicRefErr
		}
		return []byte(r.symbolicRef + "\n"), nil
	case strings.HasPrefix(joined, "fetch origin "):
		r.mu.Lock()
		r.fetchedRef = strings.TrimPrefix(joined, "fetch origin ")
		r.mu.Unlock()
		return []byte("ok\n"), nil
	case joined == "rev-parse HEAD":
		// First answer is the base the workspace was branched from; later ones
		// are the agent's commit, so commitsSince sees real work to push.
		r.mu.Lock()
		r.revParses++
		n := r.revParses
		r.mu.Unlock()
		if n > 1 {
			return []byte("headsha\n"), nil
		}
		return []byte("basesha\n"), nil
	case joined == "rev-list --count basesha..HEAD":
		return []byte("1\n"), nil
	case joined == "rev-parse --verify basesha":
		return []byte("basesha\n"), nil
	case joined == "diff --name-only basesha...HEAD":
		return []byte("file.txt\n"), nil
	case joined == "diff --no-ext-diff basesha...HEAD":
		return []byte("diff --git a/file.txt b/file.txt\n"), nil
	case strings.Contains(joined, "push ") && strings.Contains(joined, " origin HEAD:refs/heads/"):
		return []byte("pushed\n"), nil
	default:
		return []byte(""), nil
	}
}

// baseRefPRClient records the base the executor asked the hive to open against.
type baseRefPRClient struct{ base string }

func (p *baseRefPRClient) CreatePR(_ context.Context, _, _, base, _, _ string) (ghpkg.CreatePRResult, error) {
	p.base = base
	return ghpkg.CreatePRResult{Number: 1, URL: "https://example.test/pr/1"}, nil
}

func runSandboxForBaseRef(t *testing.T, runner *baseRefRunner, specBase string) (*baseRefPRClient, SandboxKickResult, error) {
	t.Helper()
	pr := &baseRefPRClient{}
	exec := &SandboxExecutor{
		Runner:      runner,
		Launcher:    sandboxFakeLauncher{},
		PRClient:    pr,
		PushEnabled: true,
		Minter:      sandboxFakeMinter{},
		Logger:      discardLogger(),
	}
	res, err := exec.Run(context.Background(), SandboxKickSpec{
		Agent: "scanner", AgentConfig: configSnapshot{Backend: "claude"}, Message: "fix",
		Org: "kubestellar", Repo: "hive", BaseRef: specBase,
		WorkspaceDir: t.TempDir(), Image: "agent-image",
	})
	return pr, res, err
}

// TestSandboxExecutorBranchesFromRepoDefaultBranch asserts the invariant this
// bug broke: a repository whose default branch is NOT "main" gets its PR based
// on that branch. A test that only exercised a "main" default would pass even
// with the old hardcoded behavior — this one fails if resolveBaseRef (or its
// call site) is deleted.
func TestSandboxExecutorBranchesFromRepoDefaultBranch(t *testing.T) {
	runner := &baseRefRunner{symbolicRef: "origin/testing"}
	pr, _, err := runSandboxForBaseRef(t, runner, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if runner.fetchedRef != "testing" {
		t.Fatalf("fetched %q, want testing — the clone's own default branch", runner.fetchedRef)
	}
	if pr.base != "testing" {
		t.Fatalf("PR opened against %q, want testing", pr.base)
	}
}

// TestSandboxExecutorFailsKickWhenDefaultBranchUnreadable asserts requirement
// 2: an unresolvable default branch must fail the kick rather than silently
// substitute "main" — a wrong base is worse than a failed kick.
func TestSandboxExecutorFailsKickWhenDefaultBranchUnreadable(t *testing.T) {
	runner := &baseRefRunner{symbolicRefErr: errors.New("no origin/HEAD")}
	_, res, err := runSandboxForBaseRef(t, runner, "")

	if err == nil {
		t.Fatal("expected the kick to fail when the default branch cannot be resolved")
	}
	if !strings.Contains(err.Error(), "could not resolve") {
		t.Fatalf("error = %v, want it to explain the unresolved default branch", err)
	}
	if res.Error == "" {
		t.Error("res.Error must carry the failure (the dashboard renders this field)")
	}
	if runner.fetchedRef != "" {
		t.Fatalf("fetched %q, want no fetch at all — the kick must fail before guessing a base", runner.fetchedRef)
	}
}

func TestSandboxExecutorHonorsPinnedBaseRef(t *testing.T) {
	runner := &baseRefRunner{symbolicRef: "origin/testing"}
	pr, _, err := runSandboxForBaseRef(t, runner, "release-1.2")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if runner.fetchedRef != "release-1.2" {
		t.Fatalf("fetched %q, want the pinned release-1.2", runner.fetchedRef)
	}
	if pr.base != "release-1.2" {
		t.Fatalf("PR opened against %q, want release-1.2", pr.base)
	}
	if runner.symbolicRefRuns != 0 {
		t.Fatalf("pinned base still ran %d default-branch lookups, want 0", runner.symbolicRefRuns)
	}
}
