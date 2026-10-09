package connector

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// TypeGitHubWiki is the GitHub Wiki connector type name.
const TypeGitHubWiki = "github-wiki"

const (
	wikiHome    = "Home"
	wikiSidebar = "_Sidebar"
	wikiFooter  = "_Footer"
)

// wikiFile is one markdown file of a wiki checkout. Name is the page name
// relative to the wiki root without the .md extension.
type wikiFile struct {
	Name      string
	Body      string
	UpdatedAt time.Time
}

// wikiBackend is a git checkout of one wiki plus per-file history. It embeds
// gitBackend so clone/pull share the git connector's code path.
type wikiBackend interface {
	gitBackend
	Files(ctx context.Context) ([]wikiFile, error)
}

type realWikiBackend struct {
	realGitBackend
}

func (b realWikiBackend) Files(ctx context.Context) ([]wikiFile, error) {
	dir := b.src.CloneDir()
	// The shared clone is shallow; per-page history needs the full (small) log.
	unshallow := exec.CommandContext(ctx, "git", "-C", dir, "fetch", "--quiet", "--unshallow")
	unshallow.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	_ = unshallow.Run()
	var files []wikiFile
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		files = append(files, wikiFile{
			Name:      rel[:len(rel)-len(".md")],
			Body:      string(data),
			UpdatedAt: fileCommitTime(ctx, dir, rel),
		})
		return nil
	})
	return files, err
}

// fileCommitTime is the commit time of the last commit touching rel, zero when
// unknown.
func fileCommitTime(ctx context.Context, dir, rel string) time.Time {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "log", "-1", "--format=%cI", "--", rel).Output()
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

var newWikiBackend = func(cfg knowledge.GitSourceConfig, baseDir string, logger *slog.Logger) wikiBackend {
	b := realGitBackend{src: knowledge.NewGitSource(cfg, baseDir, logger)}
	return realWikiBackend{realGitBackend: b}
}

var wikiRepoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9._-]+$`)

// githubWikiConnector syncs GitHub wikis (<owner>/<repo>.wiki.git) through the
// git clone/SSRF path. Scope keys: repos (comma-separated owner/repo list,
// required) and branch (default master, the wiki default).
type githubWikiConnector struct {
	cfg      ConnectorConfig
	baseDir  string
	logger   *slog.Logger
	backends map[string]wikiBackend
}

func newGitHubWikiConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	baseDir := deps.StateDir
	if baseDir == "" {
		baseDir = deps.KnowledgeDir
	}
	if baseDir == "" {
		return nil, fmt.Errorf("github-wiki connector needs a state or knowledge directory")
	}
	return &githubWikiConnector{cfg: cfg, baseDir: baseDir, logger: deps.logger(), backends: map[string]wikiBackend{}}, nil
}

func (g *githubWikiConnector) Type() string { return TypeGitHubWiki }

// FullListing: every sync re-reads each whole checkout.
func (g *githubWikiConnector) FullListing() bool { return true }

func wikiRepos(cfg ConnectorConfig) []string {
	var repos []string
	for _, r := range strings.Split(cfg.ScopeValue("repos"), ",") {
		if r = strings.TrimSpace(r); r != "" {
			repos = append(repos, r)
		}
	}
	return repos
}

func wikiBranch(cfg ConnectorConfig) string {
	if b := cfg.ScopeValue("branch"); b != "" {
		return b
	}
	return "master"
}

func wikiURL(repo string) string { return "https://github.com/" + repo + ".wiki.git" }

func (g *githubWikiConnector) Validate(cfg ConnectorConfig) error {
	repos := wikiRepos(cfg)
	if len(repos) == 0 {
		return fmt.Errorf("scope.repos is required (comma-separated owner/repo list)")
	}
	seen := map[string]bool{}
	for _, r := range repos {
		if !wikiRepoRe.MatchString(r) || strings.Contains(r, "..") {
			return fmt.Errorf("scope.repos: %q must look like owner/repo", r)
		}
		if seen[strings.ToLower(r)] {
			return fmt.Errorf("scope.repos: %q listed twice", r)
		}
		seen[strings.ToLower(r)] = true
		if err := validateGitURL(context.Background(), wikiURL(r)); err != nil {
			return fmt.Errorf("scope.repos: %s: %w", r, err)
		}
	}
	if err := knowledge.ValidateGitSourceBranch(wikiBranch(cfg)); err != nil {
		return fmt.Errorf("scope.branch: %w", err)
	}
	return nil
}

// Sync pulls every configured wiki and emits Home first, then pages in
// _Sidebar.md order, then the remaining pages alphabetically. The cursor is
// the per-repo checked-out commit SHAs.
func (g *githubWikiConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	var heads []string
	for _, repo := range wikiRepos(g.cfg) {
		head, err := g.syncRepo(ctx, repo, emit)
		if err != nil {
			return cur, err
		}
		heads = append(heads, repo+"@"+head)
	}
	return Cursor(strings.Join(heads, ",")), nil
}

func (g *githubWikiConnector) syncRepo(ctx context.Context, repo string, emit func(Page) error) (string, error) {
	b := g.backends[repo]
	if b == nil {
		src := knowledge.GitSourceConfig{
			Name:   g.cfg.Name + "-" + slugPart(repo),
			URL:    wikiURL(repo),
			Branch: wikiBranch(g.cfg),
			Layer:  knowledge.LayerType(g.cfg.Layer),
		}
		b = newWikiBackend(src, g.baseDir, g.logger)
		g.backends[repo] = b
	}
	var err error
	if b.Ready() {
		err = b.Sync(ctx)
	} else {
		err = b.Init(ctx)
	}
	if err != nil {
		return "", wikiError(repo, err)
	}
	files, err := b.Files(ctx)
	if err != nil {
		return "", fmt.Errorf("github wiki %s: reading checkout: %w", repo, err)
	}
	for _, p := range wikiPages(repo, wikiBranch(g.cfg), files) {
		if err := emit(p); err != nil {
			return "", err
		}
	}
	return b.Head(ctx), nil
}

// wikiError turns git's missing-repository output into a clear status error.
func wikiError(repo string, err error) error {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "not found") || strings.Contains(msg, "404") ||
		strings.Contains(msg, "could not read username") {
		return fmt.Errorf("github wiki %s not found: the repository has no wiki (404) or it is private; enable the wiki or check access: %w", repo, err)
	}
	return fmt.Errorf("github wiki %s: %w", repo, err)
}

var (
	wikiMDLink   = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)[^)]*\)`)
	wikiWikiLink = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
)

type sidebarEntry struct {
	page string
	path []string
}

// parseSidebar returns pages referenced by _Sidebar.md in order of first
// appearance. Each entry's path holds the enclosing heading and parent list
// items (outermost first).
func parseSidebar(body string) []sidebarEntry {
	var out []sidebarEntry
	seen := map[string]bool{}
	var heading string
	type parent struct {
		indent int
		text   string
	}
	var stack []parent
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		if strings.HasPrefix(trim, "#") {
			heading = strings.TrimSpace(strings.TrimLeft(trim, "#"))
			stack = nil
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}
		text, page := sidebarLink(trim)
		if text == "" {
			text = strings.TrimSpace(strings.Trim(trim, "-*+ \t"))
		}
		if page != "" && !seen[page] {
			seen[page] = true
			var path []string
			if heading != "" {
				path = append(path, heading)
			}
			for _, p := range stack {
				path = append(path, p.text)
			}
			out = append(out, sidebarEntry{page: page, path: path})
		}
		stack = append(stack, parent{indent: indent, text: text})
	}
	return out
}

// sidebarLink extracts the first [text](Page) or [[text|Page]] link of a line.
func sidebarLink(line string) (text, page string) {
	loc := wikiMDLink.FindStringSubmatchIndex(line)
	wloc := wikiWikiLink.FindStringSubmatchIndex(line)
	if wloc != nil && (loc == nil || wloc[0] < loc[0]) {
		inner := line[wloc[2]:wloc[3]]
		text, page = inner, inner
		if i := strings.Index(inner, "|"); i >= 0 {
			text, page = inner[:i], inner[i+1:]
		}
		return strings.TrimSpace(text), normalizeWikiPage(page)
	}
	if loc != nil {
		page = line[loc[4]:loc[5]]
		if strings.Contains(page, "://") {
			return strings.TrimSpace(line[loc[2]:loc[3]]), ""
		}
		return strings.TrimSpace(line[loc[2]:loc[3]]), normalizeWikiPage(page)
	}
	return "", ""
}

func normalizeWikiPage(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexAny(p, "#?"); i >= 0 {
		p = p[:i]
	}
	p = strings.TrimPrefix(p, "/")
	p = strings.TrimSuffix(p, ".md")
	return strings.ReplaceAll(p, " ", "-")
}

// wikiPages orders and converts a wiki checkout into pages.
func wikiPages(repo, branch string, files []wikiFile) []Page {
	byName := map[string]wikiFile{}
	var sidebar string
	var names []string
	for _, f := range files {
		switch f.Name {
		case wikiSidebar:
			sidebar = f.Body
		case wikiFooter:
		default:
			byName[strings.ToLower(f.Name)] = f
			names = append(names, f.Name)
		}
	}
	sort.Strings(names)

	var ordered []string
	paths := map[string][]string{}
	used := map[string]bool{}
	add := func(name string, path []string) {
		f, ok := byName[strings.ToLower(name)]
		if !ok || used[f.Name] {
			return
		}
		used[f.Name] = true
		ordered = append(ordered, f.Name)
		paths[f.Name] = path
	}
	add(wikiHome, nil)
	for _, e := range parseSidebar(sidebar) {
		add(e.page, e.path)
	}
	for _, n := range names {
		add(n, nil)
	}

	pages := make([]Page, 0, len(ordered))
	for _, n := range ordered {
		f := byName[strings.ToLower(n)]
		attrs := map[string]string{"repo": repo, "branch": branch}
		if n == wikiHome {
			attrs["root"] = "true"
		}
		pages = append(pages, Page{
			ID:        repo + "/" + n,
			Title:     strings.ReplaceAll(filepathBase(n), "-", " "),
			URL:       "https://github.com/" + repo + "/wiki/" + n,
			Markdown:  f.Body,
			UpdatedAt: f.UpdatedAt,
			Path:      append([]string{repo}, paths[n]...),
			Attrs:     attrs,
		})
	}
	return pages
}

func filepathBase(n string) string {
	if i := strings.LastIndex(n, "/"); i >= 0 {
		return n[i+1:]
	}
	return n
}
