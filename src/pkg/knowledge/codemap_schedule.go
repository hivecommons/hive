package knowledge

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// codeMapSourcesDir holds clones of URL-configured code map repositories. It
// is a dot-directory inside the code map vault so FileStore indexing skips it.
const codeMapSourcesDir = ".sources"

// CodeMapRepoConfig names one repository to map. Exactly one of Path (an
// existing local checkout) or URL (cloned with the git source hardening) is
// required.
type CodeMapRepoConfig struct {
	Name   string `yaml:"name"   json:"name"`
	Path   string `yaml:"path"   json:"path,omitempty"`
	URL    string `yaml:"url"    json:"url,omitempty"`
	Branch string `yaml:"branch" json:"branch,omitempty"`
}

// CodeMapConfig configures generated repository code maps (#11106).
type CodeMapConfig struct {
	// Enabled is opt-in: absent or false means no code maps are generated.
	Enabled  *bool               `yaml:"enabled,omitempty" json:"enabled"`
	Schedule string              `yaml:"schedule"          json:"schedule"`
	Layer    LayerType           `yaml:"layer"             json:"layer"`
	Repos    []CodeMapRepoConfig `yaml:"repos"             json:"repos,omitempty"`
	Caps     CodeMapCaps         `yaml:"-"                 json:"-"`
}

// IsEnabled reports whether code map generation is active (default false).
func (c CodeMapConfig) IsEnabled() bool {
	return c.Enabled != nil && *c.Enabled
}

// CodeMapRepoName returns the configured name, or one derived from the URL
// (owner/repo) or the checkout directory name.
func CodeMapRepoName(r CodeMapRepoConfig) string {
	if n := strings.TrimSpace(r.Name); n != "" {
		return n
	}
	if u := strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(r.URL), "/"), ".git"); u != "" {
		segs := strings.Split(u, "/")
		if len(segs) >= 2 {
			return segs[len(segs)-2] + "/" + segs[len(segs)-1]
		}
		return segs[len(segs)-1]
	}
	return filepath.Base(filepath.Clean(strings.TrimSpace(r.Path)))
}

// CodeMapResult reports what one RunOnce pass did for a repository.
type CodeMapResult struct {
	Repo        string
	Path        string
	SourceSHA   string
	Reason      string
	Regenerated bool
	Written     bool
	Err         error
}

// CodeMapScheduler regenerates configured repository code maps on the
// knowledge schedule, but only when a repository's HEAD moved (or the map is
// missing / from an older generator), and rewrites the vault file only when
// the rendered map actually changed.
type CodeMapScheduler struct {
	config   CodeMapConfig
	vaultDir string
	reindex  func()
	logger   *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	cancel  context.CancelFunc
	running bool
}

// NewCodeMapScheduler writes maps into vaultDir and calls reindex (may be
// nil) after any map changed so the serving FileStore picks it up at once.
func NewCodeMapScheduler(config CodeMapConfig, vaultDir string, reindex func(), logger *slog.Logger) *CodeMapScheduler {
	if logger == nil {
		logger = slog.Default()
	}
	if config.Layer == "" {
		config.Layer = LayerProject
	}
	return &CodeMapScheduler{
		config:   config,
		vaultDir: vaultDir,
		reindex:  reindex,
		logger:   logger,
		now:      time.Now,
	}
}

// Interval returns the regeneration-check period (hourly by default).
func (s *CodeMapScheduler) Interval() time.Duration {
	return ParseSynthSchedule(s.config.Schedule)
}

// RunOnce checks every configured repository and regenerates stale maps. It
// honours the enable gate.
func (s *CodeMapScheduler) RunOnce(ctx context.Context) []CodeMapResult {
	if !s.config.IsEnabled() {
		return nil
	}
	results := make([]CodeMapResult, 0, len(s.config.Repos))
	changed := false
	for _, repo := range s.config.Repos {
		if ctx.Err() != nil {
			break
		}
		res := s.runRepo(ctx, repo)
		if res.Err != nil {
			s.logger.Warn("code map generation failed", "repo", res.Repo, "error", res.Err)
		} else if res.Written {
			changed = true
			s.logger.Info("code map regenerated", "repo", res.Repo, "sha", res.SourceSHA, "reason", res.Reason, "path", res.Path)
		}
		results = append(results, res)
	}
	if changed && s.reindex != nil {
		s.reindex()
	}
	return results
}

func (s *CodeMapScheduler) runRepo(ctx context.Context, repo CodeMapRepoConfig) CodeMapResult {
	name := CodeMapRepoName(repo)
	res := CodeMapResult{Repo: name, Path: CodeMapPath(s.vaultDir, name)}
	root, err := s.resolveRoot(ctx, repo, name)
	if err != nil {
		res.Err = err
		return res
	}
	head := CodeMapHeadSHA(ctx, root)
	res.SourceSHA = head
	meta, exists, err := ReadCodeMapMeta(res.Path)
	if err != nil {
		res.Err = err
		return res
	}
	stale, reason := CodeMapNeedsRegeneration(meta, exists, head)
	res.Reason = reason
	if !stale {
		return res
	}
	cm, err := GenerateCodeMap(ctx, CodeMapOptions{
		Repo: name, Root: root, SourceSHA: head, Caps: s.config.Caps, Now: s.now,
	})
	if err != nil {
		res.Err = err
		return res
	}
	res.Regenerated = true
	_, written, err := WriteCodeMap(s.vaultDir, cm, s.config.Layer)
	res.Written = written
	res.Err = err
	return res
}

func (s *CodeMapScheduler) resolveRoot(ctx context.Context, repo CodeMapRepoConfig, name string) (string, error) {
	p := strings.TrimSpace(repo.Path)
	u := strings.TrimSpace(repo.URL)
	switch {
	case p != "" && u != "":
		return "", fmt.Errorf("code map %s: set either path or url, not both", name)
	case p != "":
		return p, nil
	case u != "":
		src := NewGitSource(GitSourceConfig{Name: name, URL: u, Branch: repo.Branch}, filepath.Join(s.vaultDir, codeMapSourcesDir), s.logger)
		if err := ValidateGitSourceURLContext(ctx, src.config.URL); err != nil {
			return "", fmt.Errorf("code map %s: invalid url: %w", name, err)
		}
		if err := ValidateGitSourceBranch(src.config.Branch); err != nil {
			return "", fmt.Errorf("code map %s: invalid branch: %w", name, err)
		}
		if err := src.ensureCloned(ctx); err != nil {
			return "", fmt.Errorf("code map %s: %w", name, err)
		}
		return src.CloneDir(), nil
	}
	return "", fmt.Errorf("code map %s: path or url is required", name)
}

// Start runs RunOnce immediately and then on every interval until ctx is
// cancelled. It returns at once when code maps are disabled.
func (s *CodeMapScheduler) Start(ctx context.Context) {
	if !s.config.IsEnabled() || len(s.config.Repos) == 0 {
		return
	}
	interval := s.Interval()
	s.logger.Info("repository code maps enabled", "repos", len(s.config.Repos), "interval", interval, "vault", s.vaultDir)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.RunOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.RunOnce(ctx)
		}
	}
}

// StartBackground launches Start in a tracked goroutine.
func (s *CodeMapScheduler) StartBackground(parent context.Context) {
	if !s.config.IsEnabled() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.running = true
	go func() {
		s.Start(ctx)
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
}

// Stop cancels the background loop.
func (s *CodeMapScheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}
