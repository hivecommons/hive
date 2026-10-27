package knowledge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/crux/logscrub"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// BeadSynthPublishBranch is the fixed head branch of the single bead-synth
// publish PR. Every run commits to it, so one open PR accumulates updates.
const BeadSynthPublishBranch = "hive/bead-synth-knowledge"

// beadSynthPublishProposeVia names where edits belong in the attribution
// footer; the vault, not the published copy, is the source of truth.
const beadSynthPublishProposeVia = "the Hive knowledge vault"

// beadSynthPersonalLayer is never published: personal facts stay local.
const beadSynthPersonalLayer = "personal"

// BeadSynthRepoClient is the GitHub surface the bead-synth publisher needs.
// *github.Client satisfies it.
type BeadSynthRepoClient interface {
	DefaultBranch(ctx context.Context, owner, repo string) (string, error)
	OpenPRForHead(ctx context.Context, repo, branch string) (number int, url string, ok bool, err error)
	GetFileContentRef(ctx context.Context, owner, repo, path, ref string) (string, error)
	LatestCommitHash(ctx context.Context, owner, repo, branch string) (string, error)
	ResetBranch(ctx context.Context, repo, branch, base string) (string, error)
	CommitFilesOnBranch(ctx context.Context, repo, branch, expectedHeadSHA, headline, body string, files map[string]string) (string, error)
	CreatePR(ctx context.Context, repo, head, base, title, body string) (hgithub.CreatePRResult, error)
}

// BeadSynthPublisherConfig is the runtime form of
// knowledge.bead_synthesizer.publish. Repo is "owner/repo" or a bare name
// under Org; an empty BaseBranch selects the repository's default branch.
type BeadSynthPublisherConfig struct {
	Repo       string
	Org        string
	Path       string
	BaseBranch string
	Layer      string
}

// BeadSynthPublisher publishes synthesized facts to <Path>/<slug>.md in a
// repository through one pull request on BeadSynthPublishBranch. It only
// opens PRs; it never commits to the base branch.
type BeadSynthPublisher struct {
	client BeadSynthRepoClient
	cfg    BeadSynthPublisherConfig
	owner  string
	repo   string
	logger *slog.Logger
}

// NewBeadSynthPublisher validates cfg and returns a publisher.
func NewBeadSynthPublisher(client BeadSynthRepoClient, cfg BeadSynthPublisherConfig, logger *slog.Logger) (*BeadSynthPublisher, error) {
	if client == nil {
		return nil, errors.New("bead-synth publisher: GitHub client is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	owner, repo, ok := strings.Cut(strings.TrimSpace(cfg.Repo), "/")
	if !ok {
		owner, repo = strings.TrimSpace(cfg.Org), owner
	}
	if owner == "" || repo == "" || strings.Contains(repo, "/") {
		return nil, fmt.Errorf("bead-synth publisher: repository %q must be owner/repo or a bare name with an org", cfg.Repo)
	}
	dir, err := cleanBeadSynthPublishPath(cfg.Path)
	if err != nil {
		return nil, err
	}
	cfg.Path = dir
	cfg.BaseBranch = strings.TrimSpace(cfg.BaseBranch)
	if cfg.BaseBranch == BeadSynthPublishBranch {
		return nil, fmt.Errorf("bead-synth publisher: base branch must differ from %s", BeadSynthPublishBranch)
	}
	return &BeadSynthPublisher{client: client, cfg: cfg, owner: owner, repo: repo, logger: logger}, nil
}

func cleanBeadSynthPublishPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\:\x00") {
		return "", fmt.Errorf("bead-synth publisher: path %q must be relative with no .. components", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", fmt.Errorf("bead-synth publisher: path %q must be relative with no .. components", p)
		}
	}
	p = path.Clean("/" + p)[1:]
	return p, nil
}

// FilePath returns the repository path a fact publishes to, or "" when the
// title yields no slug. It uses the same slug as the local vault file, so a
// re-synthesized fact updates the same file.
func (p *BeadSynthPublisher) FilePath(f ExtractedFact) string {
	slug := strings.ReplaceAll(vaultFileSlug(f.Title), "/", "_")
	if slug == "" {
		return ""
	}
	return path.Join(p.cfg.Path, slug+".md")
}

// PublishFacts implements BeadFactPublisher. Sensitive facts and facts of a
// personal layer are skipped. Files whose rendered content already matches
// the publish branch (or the base branch when no PR is open) are left alone;
// when nothing changed no commit or PR is made.
func (p *BeadSynthPublisher) PublishFacts(ctx context.Context, facts []ExtractedFact) error {
	if strings.EqualFold(strings.TrimSpace(p.cfg.Layer), beadSynthPersonalLayer) {
		p.logger.Info("bead-synth publish skipped: personal-layer facts are never published")
		return nil
	}
	rendered := make(map[string]string)
	for _, f := range facts {
		if IsSensitiveFact(f) {
			p.logger.Info("bead-synth publish skipped a sensitive fact")
			continue
		}
		fp := p.FilePath(f)
		if fp == "" {
			continue
		}
		rendered[fp] = p.render(f)
	}
	if len(rendered) == 0 {
		return nil
	}

	fullRepo := p.owner + "/" + p.repo
	base := p.cfg.BaseBranch
	if base == "" {
		resolved, err := p.client.DefaultBranch(ctx, p.owner, p.repo)
		if err != nil {
			return fmt.Errorf("resolving default branch of %s: %w", fullRepo, err)
		}
		base = resolved
	}
	if base == BeadSynthPublishBranch {
		return fmt.Errorf("base branch of %s must differ from %s", fullRepo, BeadSynthPublishBranch)
	}
	prNumber, _, open, err := p.client.OpenPRForHead(ctx, fullRepo, BeadSynthPublishBranch)
	if err != nil {
		return err
	}
	ref := base
	if open {
		ref = BeadSynthPublishBranch
	}

	changed := make(map[string]string)
	for fp, content := range rendered {
		current, err := p.client.GetFileContentRef(ctx, p.owner, p.repo, fp, ref)
		switch {
		case err == nil && current == content:
			continue
		case err != nil && !isGitHubNotFound(err):
			return fmt.Errorf("reading %s at %s: %w", fp, ref, err)
		}
		changed[fp] = content
	}
	if len(changed) == 0 {
		return nil
	}

	var head string
	if open {
		head, err = p.client.LatestCommitHash(ctx, p.owner, p.repo, BeadSynthPublishBranch)
	} else {
		head, err = p.client.ResetBranch(ctx, fullRepo, BeadSynthPublishBranch, base)
	}
	if err != nil {
		return err
	}
	paths := sortedBeadSynthPaths(changed)
	headline := fmt.Sprintf("docs(knowledge): update %d synthesized fact(s)", len(paths))
	if _, err := p.client.CommitFilesOnBranch(ctx, fullRepo, BeadSynthPublishBranch, head, headline, beadSynthFileList(paths), changed); err != nil {
		return err
	}
	if open {
		p.logger.Info("bead-synth facts published to open PR", "repo", fullRepo, "pr", prNumber, "files", len(paths))
		return nil
	}
	res, err := p.client.CreatePR(ctx, fullRepo, BeadSynthPublishBranch, base, "📚 knowledge: synthesized facts", beadSynthPRBody(paths))
	if err != nil {
		return err
	}
	p.logger.Info("bead-synth facts published", "repo", fullRepo, "pr", res.Number, "reused", res.AlreadyExisted, "files", len(paths))
	return nil
}

// render returns the published markdown: frontmatter, the fact, and the same
// attribution footer as connector publishing. It carries nothing that changes
// between identical syntheses, so an unchanged fact renders identically.
func (p *BeadSynthPublisher) render(f ExtractedFact) string {
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %s\n", sanitizeFrontmatterValue(f.Title))
	fmt.Fprintf(&b, "type: %s\n", sanitizeFrontmatterValue(string(f.Type)))
	if layer := strings.TrimSpace(p.cfg.Layer); layer != "" {
		fmt.Fprintf(&b, "layer: %s\n", sanitizeFrontmatterValue(layer))
	}
	fmt.Fprintf(&b, "confidence: %.2f\n", f.Confidence)
	if len(f.Tags) > 0 {
		fmt.Fprintf(&b, "tags: [%s]\n", sanitizeFrontmatterList(f.Tags))
	}
	if len(f.Related) > 0 {
		fmt.Fprintf(&b, "related: [%s]\n", sanitizeFrontmatterList(f.Related))
	}
	if !f.SourceDate.IsZero() {
		fmt.Fprintf(&b, "date: %s\n", f.SourceDate.UTC().Format("2006-01-02"))
	}
	b.WriteString("---\n\n")
	body := strings.TrimSpace(f.Body)
	if !strings.HasPrefix(body, "# ") {
		fmt.Fprintf(&b, "# %s\n\n", sanitizeFrontmatterValue(f.Title))
	}
	if body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "---\n\n_Maintained by Hive — edits here are overwritten; propose changes via %s._\n", beadSynthPublishProposeVia)
	return b.String()
}

// IsSensitiveFact reports whether a fact must never leave the hive: it is
// flagged Sensitive, carries a security tag, or contains credential-shaped or
// already-redacted text.
func IsSensitiveFact(f ExtractedFact) bool {
	if f.Sensitive {
		return true
	}
	for _, t := range f.Tags {
		if strings.EqualFold(strings.TrimSpace(t), "security") {
			return true
		}
	}
	parts := make([]string, 0, 2+len(f.Tags)+len(f.Related))
	parts = append(parts, f.Title, f.Body)
	parts = append(parts, f.Tags...)
	parts = append(parts, f.Related...)
	text := strings.Join(parts, "\n")
	return logscrub.ScrubString(text) != text || logscrub.ContainsRedactionMarker(text)
}

func isGitHubNotFound(err error) bool {
	var ghErr *gh.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusNotFound
}

func sortedBeadSynthPaths(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func beadSynthFileList(paths []string) string {
	var b strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	return b.String()
}

func beadSynthPRBody(paths []string) string {
	return "Synthesized knowledge facts from completed work, published from the knowledge vault. " +
		"Later synthesis runs update this PR in place.\n\n" + beadSynthFileList(paths)
}
