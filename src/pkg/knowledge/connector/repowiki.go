package connector

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// TypeRepoWiki is the repository-carried knowledge connector type name.
const TypeRepoWiki = "repo-wiki"

// DefaultRepoWikiDir is the directory a repository carries its knowledge in.
const DefaultRepoWikiDir = ".hive/wiki"

// Lifecycle states recorded in the `attr_state` front-matter key. Content
// merged to the tracked branch went through normal PR review, so it is
// approved unless the file itself says otherwise.
const (
	repoWikiApproved = "approved"
	repoWikiDraft    = "draft"
)

// repoWikiFile is one markdown file under the repository's knowledge dir.
// Name is the path relative to that dir without the .md extension.
type repoWikiFile struct {
	Name string
	Raw  string
	// UpdatedAt is the last commit time touching the file, zero when unknown.
	UpdatedAt time.Time
}

type repoWikiBackend interface {
	gitBackend
	Files(ctx context.Context, dir string) ([]repoWikiFile, error)
}

type realRepoWikiBackend struct{ realGitBackend }

func (b realRepoWikiBackend) Files(ctx context.Context, dir string) ([]repoWikiFile, error) {
	clone := b.src.CloneDir()
	root := filepath.Join(clone, filepath.FromSlash(dir))
	var files []repoWikiFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && p == root {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() || !strings.HasSuffix(strings.ToLower(d.Name()), ".md") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		files = append(files, repoWikiFile{
			Name:      rel[:len(rel)-len(".md")],
			Raw:       string(data),
			UpdatedAt: fileCommitTime(ctx, clone, path.Join(dir, rel)),
		})
		return nil
	})
	return files, err
}

var newRepoWikiBackend = func(cfg knowledge.GitSourceConfig, baseDir string, logger *slog.Logger) repoWikiBackend {
	return realRepoWikiBackend{realGitBackend{src: knowledge.NewGitSource(cfg, baseDir, logger)}}
}

// repoWikiConnector ingests the knowledge a repository carries in its own
// history (`.hive/wiki/` by default) through the shared git clone/SSRF path.
// Scope keys: repos (comma-separated owner/repo list, required), branch
// (default main) and dir (default .hive/wiki).
type repoWikiConnector struct {
	cfg      ConnectorConfig
	baseDir  string
	logger   *slog.Logger
	backends map[string]repoWikiBackend
}

func newRepoWikiConnector(cfg ConnectorConfig, deps Deps) (Connector, error) {
	baseDir := deps.StateDir
	if baseDir == "" {
		baseDir = deps.KnowledgeDir
	}
	if baseDir == "" {
		return nil, fmt.Errorf("repo-wiki connector needs a state or knowledge directory")
	}
	return &repoWikiConnector{cfg: cfg, baseDir: baseDir, logger: deps.logger(), backends: map[string]repoWikiBackend{}}, nil
}

func (r *repoWikiConnector) Type() string { return TypeRepoWiki }

// FullListing: every sync re-reads each whole checkout, so files removed from
// a repository are tombstoned.
func (r *repoWikiConnector) FullListing() bool { return true }

func repoWikiBranch(cfg ConnectorConfig) string {
	if b := cfg.ScopeValue("branch"); b != "" {
		return b
	}
	return "main"
}

func repoWikiDir(cfg ConnectorConfig) string {
	if d := strings.Trim(cfg.ScopeValue("dir"), "/"); d != "" {
		return path.Clean(d)
	}
	return DefaultRepoWikiDir
}

func (r *repoWikiConnector) Validate(cfg ConnectorConfig) error {
	repos := wikiRepos(cfg)
	if len(repos) == 0 {
		return fmt.Errorf("scope.repos is required (comma-separated owner/repo list)")
	}
	seen := map[string]bool{}
	for _, repo := range repos {
		if !wikiRepoRe.MatchString(repo) || strings.Contains(repo, "..") {
			return fmt.Errorf("scope.repos: %q must look like owner/repo", repo)
		}
		if seen[strings.ToLower(repo)] {
			return fmt.Errorf("scope.repos: %q listed twice", repo)
		}
		seen[strings.ToLower(repo)] = true
		if err := validateGitURL(context.Background(), repoURL(repo)); err != nil {
			return fmt.Errorf("scope.repos: %s: %w", repo, err)
		}
	}
	if err := knowledge.ValidateGitSourceBranch(repoWikiBranch(cfg)); err != nil {
		return fmt.Errorf("scope.branch: %w", err)
	}
	if d := cfg.ScopeValue("dir"); d != "" {
		if strings.HasPrefix(d, "/") || strings.Contains(d, "..") || strings.ContainsAny(d, "\\\n") {
			return fmt.Errorf("scope.dir %q must be relative and must not contain ..", d)
		}
	}
	return nil
}

func repoURL(repo string) string { return "https://github.com/" + repo + ".git" }

// Sync pulls every configured repository and emits one page per markdown file
// under its knowledge directory. Files with invalid front matter are skipped
// and reported together in the returned error (which the syncer records as the
// connector's last error) after every valid page has been emitted. The cursor
// is the per-repo checked-out commit SHAs.
func (r *repoWikiConnector) Sync(ctx context.Context, cur Cursor, emit func(Page) error) (Cursor, error) {
	var heads, problems []string
	for _, repo := range wikiRepos(r.cfg) {
		head, bad, err := r.syncRepo(ctx, repo, emit)
		if err != nil {
			return cur, err
		}
		problems = append(problems, bad...)
		heads = append(heads, repo+"@"+head)
	}
	next := Cursor(strings.Join(heads, ","))
	if len(problems) > 0 {
		return next, fmt.Errorf("repo-wiki: %d invalid file(s): %s", len(problems), strings.Join(problems, "; "))
	}
	return next, nil
}

func (r *repoWikiConnector) syncRepo(ctx context.Context, repo string, emit func(Page) error) (string, []string, error) {
	dir := repoWikiDir(r.cfg)
	b := r.backends[repo]
	if b == nil {
		src := knowledge.GitSourceConfig{
			Name:    r.cfg.Name + "-" + slugPart(repo),
			URL:     repoURL(repo),
			Branch:  repoWikiBranch(r.cfg),
			Subpath: dir,
			Layer:   knowledge.LayerType(r.cfg.Layer),
		}
		b = newRepoWikiBackend(src, r.baseDir, r.logger)
		r.backends[repo] = b
	}
	var err error
	if b.Ready() {
		err = b.Sync(ctx)
	} else {
		err = b.Init(ctx)
	}
	if err != nil {
		return "", nil, fmt.Errorf("repo-wiki %s: %w", repo, err)
	}
	files, err := b.Files(ctx, dir)
	if err != nil {
		return "", nil, fmt.Errorf("repo-wiki %s: reading checkout: %w", repo, err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	head := b.Head(ctx)
	var bad []string
	for _, f := range files {
		p, err := repoWikiPage(repo, repoWikiBranch(r.cfg), dir, head, f)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s/%s/%s.md: %v", repo, dir, f.Name, err))
			continue
		}
		if err := emit(p); err != nil {
			return "", nil, err
		}
	}
	return head, bad, nil
}

// repoWikiPage converts one file into a page, validating its front matter.
func repoWikiPage(repo, branch, dir, commit string, f repoWikiFile) (Page, error) {
	fm, body, err := parseRepoWikiFrontMatter(f.Raw)
	if err != nil {
		return Page{}, err
	}
	state := repoWikiApproved
	archived := false
	switch s := strings.ToLower(fm["status"]); s {
	case "", "approved", "active":
	case repoWikiDraft:
		state = repoWikiDraft
	case "deprecated", "superseded":
		state, archived = s, true
	default:
		return Page{}, fmt.Errorf("status %q must be draft, approved or deprecated", fm["status"])
	}
	title := fm["title"]
	if title == "" {
		title = strings.ReplaceAll(filepathBase(f.Name), "-", " ")
	}
	rel := path.Join(dir, f.Name+".md")
	attrs := map[string]string{"repo": repo, "branch": branch, "path": rel, "state": state}
	if commit != "" {
		attrs["commit"] = commit
	}
	if fm["tags"] != "" {
		attrs["tags"] = fm["tags"]
	}
	return Page{
		ID:        repo + "/" + f.Name,
		Title:     title,
		URL:       "https://github.com/" + repo + "/blob/" + branch + "/" + rel,
		Markdown:  body,
		UpdatedAt: f.UpdatedAt,
		Archived:  archived,
		Path:      []string{repo, dir},
		Attrs:     attrs,
	}, nil
}

// parseRepoWikiFrontMatter splits an optional leading `---` block of flat
// `key: value` lines from the body. A file without front matter is valid.
func parseRepoWikiFrontMatter(raw string) (map[string]string, string, error) {
	raw = strings.TrimPrefix(raw, "\ufeff")
	norm := strings.ReplaceAll(raw, "\r\n", "\n")
	fm := map[string]string{}
	if !strings.HasPrefix(norm, "---\n") {
		return fm, norm, nil
	}
	lines := strings.Split(norm[len("---\n"):], "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "---" {
			return fm, strings.TrimLeft(strings.Join(lines[i+1:], "\n"), "\n"), nil
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		k = strings.ToLower(strings.TrimSpace(k))
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return nil, "", fmt.Errorf("front matter line %d %q is not `key: value`", i+2, line)
		}
		fm[k] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return nil, "", fmt.Errorf("front matter is not closed with ---")
}
