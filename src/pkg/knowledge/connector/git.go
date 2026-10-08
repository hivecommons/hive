package connector

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// Built-in connector type names.
const (
	TypeGit      = "git"
	TypeDocument = "document"
)

// gitBackend is the slice of *knowledge.GitSource the git connector uses,
// behind an interface so tests need neither network nor a git binary.
type gitBackend interface {
	Init(ctx context.Context) error
	Sync(ctx context.Context) error
	Ready() bool
	Pages() []knowledge.Fact
	Head(ctx context.Context) string
}

// realGitBackend adapts the existing knowledge.GitSource: clone/pull, SSRF
// validation, redirect suppression and sparse checkout all stay in
// gitsource.go, unchanged.
type realGitBackend struct{ src *knowledge.GitSource }

func (b realGitBackend) Init(ctx context.Context) error { return b.src.Init(ctx) }
func (b realGitBackend) Sync(ctx context.Context) error { return b.src.Sync(ctx) }
func (b realGitBackend) Ready() bool                    { return b.src.Ready() }

func (b realGitBackend) Pages() []knowledge.Fact {
	store := b.src.Store()
	if store == nil {
		return nil
	}
	list := store.ListPages("")
	out := make([]knowledge.Fact, 0, len(list))
	for _, f := range list {
		if full, err := store.ReadPage(f.Slug); err == nil {
			out = append(out, *full)
		}
	}
	return out
}

func (b realGitBackend) Head(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "git", "-C", b.src.CloneDir(), "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var newGitBackend = func(cfg knowledge.GitSourceConfig, baseDir string, logger *slog.Logger) gitBackend {
	return realGitBackend{src: knowledge.NewGitSource(cfg, baseDir, logger)}
}

// validateGitURL is the existing git-source SSRF validator; a var so tests
// can avoid DNS.
var validateGitURL = knowledge.ValidateGitSourceURLContext

// gitConnector exposes a knowledge.git_sources-style repository as a
// connector. Scope keys: url (required), branch (default main), subpath.
type gitConnector struct {
	cfg     ConnectorConfig
	src     knowledge.GitSourceConfig
	baseDir string
	logger  *slog.Logger
	backend gitBackend
}

func newGitConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	baseDir := deps.StateDir
	if baseDir == "" {
		baseDir = deps.KnowledgeDir
	}
	if baseDir == "" {
		return nil, fmt.Errorf("git connector needs a state or knowledge directory")
	}
	return &gitConnector{
		cfg:     cfg,
		src:     gitSourceConfig(cfg),
		baseDir: baseDir,
		logger:  deps.logger(),
	}, nil
}

func gitSourceConfig(cfg ConnectorConfig) knowledge.GitSourceConfig {
	branch := cfg.ScopeValue("branch")
	if branch == "" {
		branch = "main"
	}
	return knowledge.GitSourceConfig{
		Name:    cfg.Name,
		URL:     cfg.ScopeValue("url"),
		Branch:  branch,
		Subpath: cfg.ScopeValue("subpath"),
		Layer:   knowledge.LayerType(cfg.Layer),
	}
}

func (g *gitConnector) Type() string { return TypeGit }

// FullListing: every sync re-reads the whole checkout.
func (g *gitConnector) FullListing() bool { return true }

func (g *gitConnector) Validate(cfg ConnectorConfig) error {
	src := gitSourceConfig(cfg)
	if src.URL == "" {
		return fmt.Errorf("scope.url is required")
	}
	if err := validateGitURL(context.Background(), src.URL); err != nil {
		return fmt.Errorf("scope.url: %w", err)
	}
	if err := knowledge.ValidateGitSourceBranch(src.Branch); err != nil {
		return fmt.Errorf("scope.branch: %w", err)
	}
	if strings.HasPrefix(src.Subpath, "/") || strings.Contains(src.Subpath, "..") {
		return fmt.Errorf("scope.subpath %q must be relative and must not contain ..", src.Subpath)
	}
	return nil
}

// Sync clones (first run) or pulls, then emits every indexed markdown page.
// The cursor is the checked-out commit SHA.
func (g *gitConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	if g.backend == nil {
		g.backend = newGitBackend(g.src, g.baseDir, g.logger)
	}
	var err error
	if g.backend.Ready() {
		err = g.backend.Sync(ctx)
	} else {
		err = g.backend.Init(ctx)
	}
	if err != nil {
		return cur, err
	}
	pages := g.backend.Pages()
	sort.Slice(pages, func(i, j int) bool { return pages[i].Slug < pages[j].Slug })
	for _, f := range pages {
		p := Page{
			ID:       f.Slug,
			Title:    f.Title,
			URL:      g.src.URL,
			Markdown: f.Body,
			Archived: strings.EqualFold(f.Status, StatusDeprecated),
			Attrs:    map[string]string{"branch": g.src.Branch},
		}
		if g.src.Subpath != "" {
			p.Path = []string{g.src.Subpath}
		}
		if err := emit(p); err != nil {
			return cur, err
		}
	}
	return Cursor(g.backend.Head(ctx)), nil
}
