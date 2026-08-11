package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"net"
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

	gitAllowedProtocols = "https:http"

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

// NewGitSource creates a git source. Call Init() to clone the repo and start
// the FileStore. The clone lands under baseDir/git-sources/<slug>.
func NewGitSource(config GitSourceConfig, baseDir string, logger *slog.Logger) *GitSource {
	config.URL = strings.TrimSpace(config.URL)
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
	if err := ValidateGitSourceURLContext(ctx, g.config.URL); err != nil {
		return fmt.Errorf("git source %s: invalid url: %w", g.config.Name, err)
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

	if err := gitSourcePull(ctx, g.cloneDir); err != nil {
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
		return gitSourcePull(ctx, g.cloneDir)
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

	args = append(args, "--", g.config.URL, g.cloneDir)

	cmd := exec.CommandContext(cloneCtx, "git", args...)
	cmd.Env = gitSourceEnv()

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
	initCmd.Env = gitSourceEnv()
	if out, err := initCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sparse-checkout init: %s: %w", strings.TrimSpace(string(out)), err)
	}

	setCmd := exec.CommandContext(ctx, "git", "sparse-checkout", "set", g.config.Subpath)
	setCmd.Dir = g.cloneDir
	setCmd.Env = gitSourceEnv()
	if out, err := setCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sparse-checkout set: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return nil
}

// ValidateGitSourceURL enforces the transports permitted for git-backed knowledge sources.
func ValidateGitSourceURL(raw string) error {
	return ValidateGitSourceURLContext(context.Background(), raw)
}

// ValidateGitSourceURLContext is ValidateGitSourceURL with a caller-supplied
// context, so the SSRF DNS lookup inherits the request's deadline and
// cancellation instead of running unbounded on a background context.
func ValidateGitSourceURLContext(ctx context.Context, raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("git URL is required")
	}
	if strings.HasPrefix(trimmed, "-") {
		return fmt.Errorf("git URL must not start with '-'")
	}
	if isSCPStyleGitURL(trimmed) {
		return fmt.Errorf("git URL scp-like syntax is not allowed")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("git URL is invalid")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "https" && scheme != "http" {
		if scheme == "" {
			return fmt.Errorf("git URL scheme is required")
		}
		return fmt.Errorf("git URL scheme %q is not allowed", scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("git URL host is required")
	}
	if strings.Contains(trimmed, "::") {
		return fmt.Errorf("git URL must not contain git transport helper separator")
	}

	// SSRF: reject private/loopback/link-local hosts so a git knowledge source
	// can't be pointed at the pod network, internal services, or the cloud
	// metadata endpoint (169.254.169.254). Operators running a legitimately
	// internal Git server can opt in explicitly; default is fail-closed.
	if !allowPrivateGitSource() && gitSourceHostResolvesPrivate(ctx, parsed.Hostname()) {
		return fmt.Errorf("git URL host resolves to a private/internal address (set HIVE_ALLOW_PRIVATE_GIT_SOURCE=true for an internal Git server)")
	}

	return nil
}

// allowPrivateGitSource reports whether a git knowledge source pointing at a
// private/internal address is permitted. Default false (fail-closed).
func allowPrivateGitSource() bool {
	return os.Getenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE") == "true"
}

// gitSourceResolver is the DNS lookup used by the SSRF check, replaceable in
// tests. Mirrors the dashboard's privateURLResolver seam.
var gitSourceResolver = func(ctx context.Context, host string) ([]string, error) {
	return net.DefaultResolver.LookupHost(ctx, host)
}

// gitSourceLookupTimeout bounds the SSRF DNS lookup so a slow or hostile
// resolver cannot stall the request that triggered validation.
const gitSourceLookupTimeout = 5 * time.Second

// gitSourceHostResolvesPrivate reports whether host is private either as an IP
// literal or by DNS resolution.
//
// SECURITY (audit F8, CWE-918): the literal check alone was the whole defense,
// and its comment argued DNS was skipped "to avoid a rebinding TOCTOU". That
// reasoning is inverted — declining to resolve does not avoid rebinding, it
// removes the check altogether, so `http://internal.attacker.tld/` resolving to
// 169.254.169.254 validated cleanly. Resolving narrows the window to a genuine
// rebind race; not resolving leaves the front door open. The dashboard's
// isPrivateURL already resolves and fails closed; this brings the git path in
// line with it.
//
// Fails CLOSED on lookup error, matching that precedent.
func gitSourceHostResolvesPrivate(ctx context.Context, host string) bool {
	if gitSourceHostIsPrivate(host) {
		return true
	}
	// An IP literal was fully decided above; no DNS to consult.
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return false
	}
	lookupCtx, cancel := context.WithTimeout(ctx, gitSourceLookupTimeout)
	defer cancel()
	addrs, err := gitSourceResolver(lookupCtx, host)
	if err != nil {
		return true
	}
	for _, addr := range addrs {
		if gitSourceHostIsPrivate(addr) {
			return true
		}
	}
	return false
}

// gitSourceHostIsPrivate reports whether host is a loopback/private/link-local
// literal IP or "localhost".
func gitSourceHostIsPrivate(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" || host == "localhost" {
		return host == "localhost"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

func isSCPStyleGitURL(raw string) bool {
	if strings.Contains(raw, "://") {
		return false
	}
	at := strings.Index(raw, "@")
	colon := strings.Index(raw, ":")
	return at > 0 && colon > at+1
}

func gitSourceEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL="+gitAllowedProtocols,
	)
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
