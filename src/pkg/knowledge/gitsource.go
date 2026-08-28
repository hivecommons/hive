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
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// gitSourceCloneTimeout caps how long the initial clone can take.
	gitSourceCloneTimeout = 120 * time.Second

	gitAllowedProtocols = "https"

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
	if err := ValidateGitSourceBranch(g.config.Branch); err != nil {
		return fmt.Errorf("git source %s: invalid branch: %w", g.config.Name, err)
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

	cmd := exec.CommandContext(cloneCtx, "git", gitCmdArgs(args...)...)
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
// It also blocks private/loopback destinations to prevent SSRF via git clone.
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
	if scheme != "https" && (scheme != "http" || !allowPrivateGitSource()) {
		if scheme == "" {
			return fmt.Errorf("git URL scheme is required")
		}
		return fmt.Errorf("git URL scheme %q is not allowed", scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("git URL host is required")
	}
	if gitURLContainsTransportHelperSeparator(trimmed) {
		return fmt.Errorf("git URL must not contain git transport helper separator")
	}

	// SSRF: reject private/loopback/link-local hosts so a git knowledge source
	// can't be pointed at the pod network, internal services, or the cloud
	// metadata endpoint (169.254.169.254). Mirrors the document-import guard.
	// Operators running a legitimately-internal Git server (e.g. an in-cluster
	// GitLab) set HIVE_ALLOW_PRIVATE_GIT_SOURCE=true to opt in; default is
	// fail-closed.
	if !allowPrivateGitSource() && gitSourceHostResolvesPrivate(ctx, parsed.Hostname()) {
		return fmt.Errorf("git URL host resolves to a private/internal address (set HIVE_ALLOW_PRIVATE_GIT_SOURCE=true for an internal Git server)")
	}

	return nil
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
// rebind race; not resolving leaves the front door open.
//
// Fails CLOSED on lookup error.
func gitSourceHostResolvesPrivate(ctx context.Context, host string) bool {
	if gitSourceHostIsPrivate(host) {
		return true
	}
	// An IP literal was fully decided above; no DNS to consult. This must also
	// recognise the legacy IPv4 encodings (single-integer, octal, hex,
	// shortened dotted) that gitSourceHostIsPrivate accepts but net.ParseIP
	// rejects — otherwise a public literal like "010.010.010.010" falls through
	// to a DNS lookup that cannot resolve, and the fail-closed branch below
	// misreports it as private.
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return false
	}
	if _, ok := parseGitSourceIPv4Literal(strings.Trim(host, "[]")); ok {
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

// ValidateGitSourceBranch rejects branch values that git could parse as
// command-line options when passed as the argument to `git clone --branch`.
func ValidateGitSourceBranch(branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return fmt.Errorf("git branch is required")
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("git branch must not start with '-'")
	}
	return nil
}

// allowPrivateGitSource reports whether a git knowledge source pointing at a
// private/internal address is permitted. Default false (fail-closed).
func allowPrivateGitSource() bool {
	return os.Getenv("HIVE_ALLOW_PRIVATE_GIT_SOURCE") == "true"
}

// gitSourceHostIsPrivate reports whether host is a loopback/private/link-local
// literal IP or "localhost". It also recognizes legacy IPv4 encodings accepted
// by libcurl/git (single-integer, octal, hex, and shortened dotted forms). A DNS
// name is treated as public here; DNS resolution at validation time cannot
// prevent a later rebinding before git clone resolves the host.
func gitSourceHostIsPrivate(host string) bool {
	host = strings.ToLower(strings.Trim(host, "[]"))
	if zoneStart := strings.LastIndex(host, "%"); zoneStart > -1 && strings.Contains(host, ":") {
		host = host[:zoneStart]
	}
	if host == "" || host == "localhost" {
		return host == "localhost"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		var ok bool
		ip, ok = parseGitSourceIPv4Literal(host)
		if !ok {
			return false
		}
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
		if v4[0] == 0 {
			return true
		}
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

func parseGitSourceIPv4Literal(host string) (net.IP, bool) {
	if strings.Contains(host, ":") {
		return nil, false
	}
	parts := strings.Split(host, ".")
	if len(parts) > 4 {
		return nil, false
	}

	nums := make([]uint64, len(parts))
	for i, part := range parts {
		if part == "" {
			return nil, false
		}
		n, err := strconv.ParseUint(part, 0, 32)
		if err != nil {
			return nil, false
		}
		nums[i] = n
	}

	var value uint64
	switch len(nums) {
	case 1:
		value = nums[0]
	case 2:
		if nums[0] > 0xff || nums[1] > 0xffffff {
			return nil, false
		}
		value = nums[0]<<24 | nums[1]
	case 3:
		if nums[0] > 0xff || nums[1] > 0xff || nums[2] > 0xffff {
			return nil, false
		}
		value = nums[0]<<24 | nums[1]<<16 | nums[2]
	case 4:
		for _, n := range nums {
			if n > 0xff {
				return nil, false
			}
		}
		value = nums[0]<<24 | nums[1]<<16 | nums[2]<<8 | nums[3]
	default:
		return nil, false
	}
	if value > 0xffffffff {
		return nil, false
	}

	return net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value)), true
}

func gitURLContainsTransportHelperSeparator(raw string) bool {
	inIPv6Literal := false
	for i := 0; i < len(raw)-1; i++ {
		switch raw[i] {
		case '[':
			inIPv6Literal = true
		case ']':
			inIPv6Literal = false
		}
		if !inIPv6Literal && raw[i] == ':' && raw[i+1] == ':' {
			return true
		}
	}
	return false
}

func isSCPStyleGitURL(raw string) bool {
	if strings.Contains(raw, "://") {
		return false
	}
	at := strings.Index(raw, "@")
	colon := strings.Index(raw, ":")
	return at > 0 && colon > at+1
}

// gitNoRedirectArgs are the leading `git -c …` overrides that forbid a remote
// from bouncing a fetch to a different host AFTER the URL has passed validation.
//
// AUDIT F8 (residual). The DNS-pinning fix closed the "resolve the validated
// hostname to 127.0.0.1" hole, but validation still only inspects the URL the
// operator configured. git follows HTTP 3xx by default, so a public, allow-listed
// remote can answer the very first request with `302 Location:
// http://169.254.169.254/…` (or any in-cluster service) and git will happily
// re-issue the request there — with credentials, and with every URL/DNS check
// already behind it. `http.followRedirects=false` makes git treat a redirect as
// an error instead of a destination, so a validated URL is the ONLY URL fetched.
//
// This is passed as `-c` args rather than an env var because there is no
// GIT_HTTP_FOLLOW_REDIRECTS; http.followRedirects is config-only, and repo-local
// config is attacker-influenced on a re-used clone dir whereas `-c` on the
// command line wins over every config file.
var gitNoRedirectArgs = []string{"-c", "http.followRedirects=false"}

// gitCmdArgs prefixes a git argument list with the redirect suppression above.
// Every git invocation that talks to a REMOTE must go through this.
func gitCmdArgs(args ...string) []string {
	return append(append([]string{}, gitNoRedirectArgs...), args...)
}

func gitSourceEnv() []string {
	allowedProtocols := gitAllowedProtocols
	if allowPrivateGitSource() {
		allowedProtocols += ":http"
	}
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL="+allowedProtocols,
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
