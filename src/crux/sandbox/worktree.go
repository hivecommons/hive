package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const DefaultMaxRunWorktrees = 8

type WorktreeSpec struct {
	SourceRepo   string
	WorkspaceDir string
	RunKey       string
	Stage        string
	Generation   uint64
	TargetRef    string
	MaxWorktrees int
}

type WorktreeResult struct {
	Path string
	SHA  string
}

type WorktreeManager struct {
	Runner func(ctx context.Context, dir string, name string, args ...string) ([]byte, error)
}

func (m WorktreeManager) Add(ctx context.Context, spec WorktreeSpec) (WorktreeResult, error) {
	if strings.TrimSpace(spec.SourceRepo) == "" {
		return WorktreeResult{}, errors.New("sandbox/worktree: source repo is required")
	}
	if strings.TrimSpace(spec.WorkspaceDir) == "" {
		return WorktreeResult{}, errors.New("sandbox/worktree: workspace dir is required")
	}
	if strings.TrimSpace(spec.RunKey) == "" || strings.TrimSpace(spec.Stage) == "" || spec.Generation == 0 {
		return WorktreeResult{}, errors.New("sandbox/worktree: run key, stage, and generation are required")
	}
	max := spec.MaxWorktrees
	if max <= 0 {
		max = DefaultMaxRunWorktrees
	}
	root := filepath.Join(spec.WorkspaceDir, "runs")
	if live, err := countLiveRunWorktrees(root); err != nil {
		return WorktreeResult{}, err
	} else if live >= max {
		return WorktreeResult{}, fmt.Errorf("sandbox/worktree: runs.max_worktrees cap %d reached; refusing run stage %s/%s generation %d", max, spec.RunKey, spec.Stage, spec.Generation)
	}
	path := filepath.Join(root, safePathPart(spec.RunKey), safePathPart(spec.Stage)+"-"+strconv.FormatUint(spec.Generation, 10))
	if err := os.MkdirAll(filepath.Dir(path), 0o770); err != nil {
		return WorktreeResult{}, err
	}
	ref := strings.TrimSpace(spec.TargetRef)
	if ref == "" {
		ref = "HEAD"
	}
	if out, err := m.run(ctx, spec.SourceRepo, "git", "worktree", "add", "--detach", path, ref); err != nil {
		return WorktreeResult{}, fmt.Errorf("git worktree add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	sha, err := m.run(ctx, path, "git", "rev-parse", "HEAD")
	if err != nil {
		_ = m.Remove(ctx, spec.SourceRepo, path)
		return WorktreeResult{}, fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return WorktreeResult{Path: path, SHA: strings.TrimSpace(string(sha))}, nil
}

func (m WorktreeManager) Remove(ctx context.Context, sourceRepo, path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if strings.TrimSpace(sourceRepo) != "" {
		if _, err := m.run(ctx, sourceRepo, "git", "worktree", "remove", "--force", path); err == nil {
			return nil
		}
	}
	return os.RemoveAll(path)
}

func (m WorktreeManager) run(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
	if m.Runner != nil {
		return m.Runner(ctx, dir, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func countLiveRunWorktrees(root string) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d == nil || d.IsDir() || d.Name() != ".git" {
			return nil
		}
		count++
		return nil
	})
	if os.IsNotExist(err) {
		return 0, nil
	}
	return count, err
}

func safePathPart(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if ok {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "run"
	}
	return out
}
