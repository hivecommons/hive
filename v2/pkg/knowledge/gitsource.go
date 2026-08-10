package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// gitSourceCloneTimeout caps how long the initial clone can take.
	gitSourceCloneTimeout = 120 * time.Second

	// gitSourceSyncInterval is how often we pull updates from the remote.
	gitSourceSyncInterval = 5 * time.Minute

	// gitSourceCacheDir is where cloned repos live under the knowledge data dir.
	gitSourceCacheDir = "git-sources"
)

// GitSourceConfig describes a remote git repository (or subdirectory within one)
// that should be indexed as a knowledge source. Any layer can have git sources.
type GitSourceConfig struct {
	Name    string    `yaml:"name"    json:"name"`
	URL     string    `yaml:"url"     json:"url"`
	Branch  string    `yaml:"branch"  json:"branch,omitempty"`
	Subpath string    `yaml:"subpath" json:"subpath,omitempty"`
	Layer   LayerType `yaml:"layer"   json:"layer"`
}

// GitSource manages a single cloned git repository used as a knowledge source.
// It handles cloning, periodic pulling, and exposes the indexed markdown via
// an embedded FileStore.
type GitSource struct {
	config   GitSourceConfig
	cloneDir string
	indexDir string
	store    *FileStore
	logger   *slog.Logger
	mu       sync.RWMutex
	ready    bool
}

// gitAllowedSchemes are the URL schemes accepted by validateGitSourceURL.
// Only https and http are permitted in production. Tests may extend this set
// (via a package-level init in a _test.go file) to use local file:// repos.
var gitAllowedSchemes = map[string]bool{"https": true, "http": true}

// validateGitSourceURL rejects schemes that git accepts but are dangerous in a
// server-side fetch context: file:// can read local filesystem paths (LFI),
// ssh:// opens outbound connections that may exfiltrate host keys, and the
// ext:: helper can execute arbitrary commands. Only https and http are allowed.
func validateGitSourceURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid git source URL: %w", err)
	}
	if gitAllowedSchemes[u.Scheme] {
		return nil
	}
	return fmt.Errorf("git source URL scheme %q is not allowed (only https and http are permitted)", u.Scheme)
}

// NewGitSource creates a git source. Call Init() to clone the repo and start
// the FileStore. The clone lands under baseDir/git-sources/<slug>.
func NewGitSource(config GitSourceConfig, baseDir string, logger *slog.Logger) *GitSource {
	slug := gitSourceSlug(config.URL, config.Subpath)
	cloneDir := filepath.Join(baseDir, gitSourceCacheDir, slug)

	indexDir := cloneDir
	if config.Subpath != "" {
		indexDir = filepath.Join(cloneDir, config.Subpath)
	}

	if config.Branch == "" {
		config.Branch = "main"
	}

	return &GitSource{
		config:   config,
		cloneDir: cloneDir,
		indexDir: indexDir,
		logger:   logger,
	}
}

// Init clones the repo (or reuses an existing clone) and creates the FileStore.
func (g *GitSource) Init(ctx context.Context) error {
	if err := validateGitSourceURL(g.config.URL); err != nil {
		return fmt.Errorf("git source %s: %w", g.config.Name, err)
	}
	if err := g.ensureCloned(ctx); err != nil {
		return fmt.Errorf("git source %s: clone failed: %w", g.config.Name, err)
	}

	if _, err := os.Stat(g.indexDir); err != nil {
		return fmt.Errorf("git source %s: subpath %q not found after clone: %w",
			g.config.Name, g.config.Subpath, err)
	}

	store, err := NewFileStore(g.indexDir, g.config.Name, g.logger)
	if err != nil {
		return fmt.Errorf("git source %s: index failed: %w", g.config.Name, err)
	}

	g.mu.Lock()
	g.store = store
	g.ready = true
	g.mu.Unlock()

	g.logger.Info("git source ready",
		"name", g.config.Name,
		"url", g.config.URL,
		"subpath", g.config.Subpath,
		"layer", g.config.Layer,
		"pages", store.Stats().TotalPages,
	)
	return nil
}

// Store returns the underlying FileStore, or nil if not yet initialized.
func (g *GitSource) Store() *FileStore {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.store
}

// Ready returns true if the git source has been cloned and indexed.
func (g *GitSource) Ready() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.ready
}

// Config returns the source configuration.
func (g *GitSource) Config() GitSourceConfig {
	return g.config
}

// CloneDir returns the local filesystem path where the repo is cloned.
func (g *GitSource) CloneDir() string {
	return g.cloneDir
}

// Sync pulls the latest changes and reindexes.
func (g *GitSource) Sync(ctx context.Context) error {
	if !isGitRepo(g.cloneDir) {
		return fmt.Errorf("clone dir %s is not a git repo", g.cloneDir)
	}

	if err := gitPull(ctx, g.cloneDir); err != nil {
		return fmt.Errorf("git source %s: pull failed: %w", g.config.Name, err)
	}

	g.mu.RLock()
	store := g.store
	g.mu.RUnlock()

	if store != nil {
		store.Reindex()
	}
	return nil
}

// StartSyncLoop runs periodic git pull + reindex until ctx is cancelled.
func (g *GitSource) StartSyncLoop(ctx context.Context) {
	ticker := time.NewTicker(gitSourceSyncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := g.Sync(ctx); err != nil {
				g.logger.Warn("git source sync failed",
					"name", g.config.Name,
					"error", err,
				)
			} else {
				g.logger.Debug("git source synced", "name", g.config.Name)
			}
		}
	}
}

// ensureCloned does a fresh clone or reuses an existing one.
func (g *GitSource) ensureCloned(ctx context.Context) error {
	if isGitRepo(g.cloneDir) {
		g.logger.Info("git source already cloned, pulling latest",
			"name", g.config.Name,
			"dir", g.cloneDir,
		)
		return gitPull(ctx, g.cloneDir)
	}

	if err := os.MkdirAll(filepath.Dir(g.cloneDir), 0o755); err != nil {
		return fmt.Errorf("creating parent dir: %w", err)
	}

	cloneCtx, cancel := context.WithTimeout(ctx, gitSourceCloneTimeout)
	defer cancel()

	args := []string{"clone", "--depth", "1", "--branch", g.config.Branch}

	if g.config.Subpath != "" {
		args = append(args, "--filter=blob:none", "--sparse")
	}

	args = append(args, g.config.URL, g.cloneDir)

	cmd := exec.CommandContext(cloneCtx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone: %s: %w", strings.TrimSpace(string(output)), err)
	}

	if g.config.Subpath != "" {
		if err := g.setupSparseCheckout(cloneCtx); err != nil {
			return err
		}
	}

	g.logger.Info("git source cloned",
		"name", g.config.Name,
		"url", g.config.URL,
		"branch", g.config.Branch,
		"dir", g.cloneDir,
	)
	return nil
}

// setupSparseCheckout configures sparse-checkout so only the subpath is
// materialized on disk. This avoids downloading the full repo contents.
func (g *GitSource) setupSparseCheckout(ctx context.Context) error {
	initCmd := exec.CommandContext(ctx, "git", "sparse-checkout", "init", "--cone")
	initCmd.Dir = g.cloneDir
	initCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := initCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sparse-checkout init: %s: %w", strings.TrimSpace(string(out)), err)
	}

	setCmd := exec.CommandContext(ctx, "git", "sparse-checkout", "set", g.config.Subpath)
	setCmd.Dir = g.cloneDir
	setCmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := setCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sparse-checkout set: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

// gitSourceSlug creates a filesystem-safe directory name from a git URL + subpath.
func gitSourceSlug(url, subpath string) string {
	s := url
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimSuffix(s, ".git")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, ":", "_")

	if subpath != "" {
		sub := strings.ReplaceAll(subpath, "/", "_")
		s = s + "__" + sub
	}
	return s
}

// GitSourceInfo describes a connected git source for the dashboard.
type GitSourceInfo struct {
	Name     string    `json:"name"`
	URL      string    `json:"url"`
	Branch   string    `json:"branch"`
	Subpath  string    `json:"subpath"`
	Layer    LayerType `json:"layer"`
	CloneDir string    `json:"clone_dir"`
	Ready    bool      `json:"ready"`
	Pages    int       `json:"pages"`
}

// Info returns dashboard-friendly info about this git source.
func (g *GitSource) Info() GitSourceInfo {
	info := GitSourceInfo{
		Name:     g.config.Name,
		URL:      g.config.URL,
		Branch:   g.config.Branch,
		Subpath:  g.config.Subpath,
		Layer:    g.config.Layer,
		CloneDir: g.cloneDir,
		Ready:    g.Ready(),
	}
	if g.Ready() {
		info.Pages = g.Store().Stats().TotalPages
	}
	return info
}
