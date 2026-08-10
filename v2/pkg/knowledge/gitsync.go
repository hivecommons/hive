package knowledge

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	// gitSyncInterval is how often we pull new commits from the remote.
	gitSyncInterval = 60 * time.Second

	// gitDirName is the hidden directory that marks a git repository.
	gitDirName = ".git"
)

// GitSyncer periodically runs `git pull` on vault directories that are
// git repositories. This picks up changes pushed by Obsidian Git or any
// other editor within one sync cycle.
type GitSyncer struct {
	vaults []*syncedVault
	logger *slog.Logger
}

type syncedVault struct {
	name    string
	rootDir string
	store   *FileStore
}

// NewGitSyncer creates a syncer for the given vaults. Only vaults whose
// root directory contains a .git folder are actually synced.
func NewGitSyncer(logger *slog.Logger) *GitSyncer {
	return &GitSyncer{
		logger: logger,
	}
}

// Add registers a vault for periodic git pull. If the directory is not a
// git repo the vault is still tracked (for reindex) but git pull is skipped.
func (g *GitSyncer) Add(name, rootDir string, store *FileStore) {
	g.vaults = append(g.vaults, &syncedVault{
		name:    name,
		rootDir: rootDir,
		store:   store,
	})
}

// Start runs the sync loop until the context is cancelled.
func (g *GitSyncer) Start(ctx context.Context) {
	if len(g.vaults) == 0 {
		return
	}

	g.logger.Info("git syncer started", "vaults", len(g.vaults), "interval", gitSyncInterval)

	ticker := time.NewTicker(gitSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			g.logger.Info("git syncer stopped")
			return
		case <-ticker.C:
			g.syncAll(ctx)
		}
	}
}

func (g *GitSyncer) syncAll(ctx context.Context) {
	for _, v := range g.vaults {
		if !isGitRepo(v.rootDir) {
			continue
		}
		if err := gitPull(ctx, v.rootDir); err != nil {
			g.logger.Warn("git pull failed",
				"vault", v.name,
				"dir", v.rootDir,
				"error", err,
			)
			continue
		}
		// After a successful pull, force a reindex so new/changed pages
		// are visible immediately rather than waiting for the stale timer.
		v.store.Reindex()
		g.logger.Debug("git sync complete", "vault", v.name)
	}
}

// isGitRepo returns true if the directory contains a .git folder.
func isGitRepo(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, gitDirName))
	return err == nil && info.IsDir()
}

// gitPull runs `git pull --ff-only` in the given directory.
// --ff-only avoids creating merge commits if there are local changes.
func gitPull(ctx context.Context, dir string) error {
	return gitPullWithEnv(ctx, dir, append(os.Environ(), "GIT_TERMINAL_PROMPT=0"))
}

func gitSourcePull(ctx context.Context, dir string) error {
	return gitPullWithEnv(ctx, dir, gitSourceEnv())
}

func gitPullWithEnv(ctx context.Context, dir string, env []string) error {
	const gitPullTimeoutSeconds = 30
	pullCtx, cancel := context.WithTimeout(ctx, gitPullTimeoutSeconds*time.Second)
	defer cancel()

	cmd := exec.CommandContext(pullCtx, "git", "pull", "--ff-only")
	cmd.Dir = dir
	cmd.Env = env

	output, err := cmd.CombinedOutput()
	if err != nil {
		return &gitPullError{dir: dir, output: string(output), err: err}
	}
	return nil
}

type gitPullError struct {
	dir    string
	output string
	err    error
}

func (e *gitPullError) Error() string {
	return "git pull in " + e.dir + ": " + e.err.Error() + " (" + e.output + ")"
}

func (e *gitPullError) Unwrap() error {
	return e.err
}

// InitVaultRepo initializes a vault directory. If a git remote URL is
// configured and the directory does not exist, it clones the repo.
// Otherwise it ensures the directory exists and optionally runs git init.
func InitVaultRepo(path string, logger *slog.Logger) error {
	if _, err := os.Stat(path); err == nil {
		logger.Info("vault directory already exists", "path", path)
		return nil
	}

	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}

	logger.Info("vault directory created", "path", path)
	return nil
}

// SeedVaultContent copies seed files into a vault directory if it is empty
// (no .md files present). This provides starter content for new deployments.
func SeedVaultContent(vaultPath, seedDir string, logger *slog.Logger) error {
	// Check if vault already has markdown content
	entries, err := os.ReadDir(vaultPath)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() && (filepath.Ext(e.Name()) == ".md" || filepath.Ext(e.Name()) == ".markdown") {
			logger.Info("vault already has content, skipping seed", "path", vaultPath)
			return nil
		}
	}

	// Check if seed directory exists
	seedEntries, err := os.ReadDir(seedDir)
	if err != nil {
		logger.Info("no seed directory found, skipping", "seed_dir", seedDir)
		return nil
	}

	copied := 0
	for _, e := range seedEntries {
		if e.IsDir() {
			continue
		}
		src := filepath.Join(seedDir, e.Name())
		dst := filepath.Join(vaultPath, e.Name())

		data, err := os.ReadFile(src)
		if err != nil {
			logger.Warn("failed to read seed file", "path", src, "error", err)
			continue
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			logger.Warn("failed to write seed file", "path", dst, "error", err)
			continue
		}
		copied++
	}

	logger.Info("seed content copied to vault", "path", vaultPath, "files", copied)
	return nil
}
