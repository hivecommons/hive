package upstreamwatch

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	// githubPerPage is the page size for every list call (GitHub's maximum).
	githubPerPage = 100
	// maxPRPages bounds the merged-PR scan. PRs come back sorted by
	// updated_at desc, so once a whole page's updated_at falls at or below
	// the watermark we stop early; this ceiling is the fallback for runs
	// that have never caught up and must not scan the upstream forever.
	maxPRPages = 10
	// maxReleasePages bounds the release scan. Releases are listed newest
	// first, so even a repo with hundreds of releases is well inside the
	// window the first catch-up run needs.
	maxReleasePages = 5
	// maxFilePages bounds the per-PR touched-files scan. One PR with more
	// than this many files is almost always a bulk vendor sync a port
	// would ignore anyway; a hard cap keeps a pathological PR from pulling
	// an entire upstream list run into a long request chain.
	maxFilePages = 10
	// maxFilesPerItem caps the number of filenames stored on one Item.
	// The additions/deletions totals still reflect every file reported by
	// the API — this bound is purely about the slice we keep for the
	// classifier downstream.
	maxFilesPerItem = 300

	// prStateClosed, prSortUpdated and prDirectionDesc list recently
	// closed PRs newest first so the allOlder early-exit (which compares
	// updated_at against the watermark) is sound.
	prStateClosed   = "closed"
	prSortUpdated   = "updated"
	prDirectionDesc = "desc"
)

// GitHubSource implements Source over the GitHub REST API. It only reads.
type GitHubSource struct {
	client *gh.Client
	// fork is the configured repo (owner/name of this project's fork). Its
	// parent is resolved on first use when cfg.Upstream is empty.
	fork repoRef
	cfg  config.UpstreamWatchRepo
	// upstream is the resolved upstream repo. Set either from cfg.Upstream
	// in the constructor or from the fork's parent on first List call.
	upstream *repoRef
}

type repoRef struct {
	owner string
	repo  string
}

// NewGitHubSource returns a Source for the fork at forkOwner/forkRepo. The
// upstream is taken from cfg.Upstream when present, else resolved from the
// fork's parent on the first call.
func NewGitHubSource(client *gh.Client, forkOwner, forkRepo string, cfg config.UpstreamWatchRepo) *GitHubSource {
	s := &GitHubSource{
		client: client,
		fork:   repoRef{owner: forkOwner, repo: forkRepo},
		cfg:    cfg,
	}
	if o, r, ok := SplitRepo(cfg.Upstream); ok {
		s.upstream = &repoRef{owner: o, repo: r}
	}
	return s
}

// SplitRepo splits "owner/name" into its halves.
func SplitRepo(full string) (owner, repo string, ok bool) {
	owner, repo, ok = strings.Cut(strings.TrimSpace(full), "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", false
	}
	return owner, repo, true
}

// List implements Source.
func (s *GitHubSource) List(ctx context.Context, since time.Time) ([]Item, error) {
	up, err := s.resolveUpstream(ctx)
	if err != nil {
		return nil, err
	}
	var items []Item
	if s.wantsSource(config.UpstreamSourcePRs) {
		prs, err := s.mergedPRs(ctx, up, since)
		if err != nil {
			return nil, err
		}
		items = append(items, prs...)
	}
	if s.wantsSource(config.UpstreamSourceReleases) {
		rels, err := s.releases(ctx, up, since)
		if err != nil {
			return nil, err
		}
		items = append(items, rels...)
	}
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].Timestamp.Before(items[j].Timestamp)
	})
	return items, nil
}

// resolveUpstream returns the upstream owner/name, falling back to the fork
// parent when cfg.Upstream was empty. An explicit cfg.Upstream always wins.
func (s *GitHubSource) resolveUpstream(ctx context.Context) (repoRef, error) {
	if s.upstream != nil {
		return *s.upstream, nil
	}
	repo, _, err := s.client.Repositories.Get(ctx, s.fork.owner, s.fork.repo)
	if err != nil {
		return repoRef{}, fmt.Errorf("resolve upstream for %s/%s: %w", s.fork.owner, s.fork.repo, err)
	}
	parent := repo.GetParent()
	if parent == nil {
		return repoRef{}, fmt.Errorf("resolve upstream for %s/%s: repo has no parent (not a fork)", s.fork.owner, s.fork.repo)
	}
	o, r, ok := SplitRepo(parent.GetFullName())
	if !ok {
		return repoRef{}, fmt.Errorf("resolve upstream for %s/%s: parent full_name %q is not owner/repo", s.fork.owner, s.fork.repo, parent.GetFullName())
	}
	ref := repoRef{owner: o, repo: r}
	s.upstream = &ref
	return ref, nil
}

// wantsSource reports whether name is enabled. An empty cfg.Sources enables
// both, matching config.applyUpstreamWatchDefaults.
func (s *GitHubSource) wantsSource(name string) bool {
	if len(s.cfg.Sources) == 0 {
		return true
	}
	for _, src := range s.cfg.Sources {
		if src == name {
			return true
		}
	}
	return false
}

// mergedPRs lists upstream PRs merged after since, filtered by the configured
// pr_labels. It walks pages of closed PRs sorted by updated_at desc and stops
// when a whole page is older than the watermark.
func (s *GitHubSource) mergedPRs(ctx context.Context, up repoRef, since time.Time) ([]Item, error) {
	labelFilter := labelSet(s.cfg.PRLabels)
	opts := &gh.PullRequestListOptions{
		State:       prStateClosed,
		Sort:        prSortUpdated,
		Direction:   prDirectionDesc,
		ListOptions: gh.ListOptions{PerPage: githubPerPage},
	}
	var out []Item
	for page := 0; page < maxPRPages; page++ {
		prs, resp, err := s.client.PullRequests.List(ctx, up.owner, up.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list merged PRs for %s/%s: %w", up.owner, up.repo, err)
		}
		allOlder := len(prs) > 0
		for _, pr := range prs {
			if pr == nil {
				continue
			}
			if pr.GetUpdatedAt().Time.After(since) {
				allOlder = false
			}
			if pr.MergedAt == nil {
				continue
			}
			merged := pr.GetMergedAt().Time
			if !merged.After(since) {
				continue
			}
			labels := labelNames(pr.Labels)
			if labelFilter != nil && !anyLabel(labels, labelFilter) {
				continue
			}
			files, adds, dels, err := s.prFiles(ctx, up, pr.GetNumber())
			if err != nil {
				return nil, err
			}
			out = append(out, Item{
				Kind:      KindPR,
				Ref:       formatPRRef(pr.GetNumber()),
				Title:     pr.GetTitle(),
				Body:      pr.GetBody(),
				HTMLURL:   pr.GetHTMLURL(),
				Timestamp: merged,
				Labels:    labels,
				Files:     files,
				Additions: adds,
				Deletions: dels,
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		if allOlder {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// prFiles lists one PR's touched files and sums its line changes. The
// filename slice is bounded by maxFilesPerItem; the additions/deletions
// counts cover every file the API reported.
func (s *GitHubSource) prFiles(ctx context.Context, up repoRef, number int) ([]string, int, int, error) {
	opts := &gh.ListOptions{PerPage: githubPerPage}
	var files []string
	adds, dels := 0, 0
	for page := 0; page < maxFilePages; page++ {
		batch, resp, err := s.client.PullRequests.ListFiles(ctx, up.owner, up.repo, number, opts)
		if err != nil {
			return nil, 0, 0, fmt.Errorf("list files for %s/%s#%d: %w", up.owner, up.repo, number, err)
		}
		for _, f := range batch {
			if f == nil {
				continue
			}
			if name := f.GetFilename(); name != "" && len(files) < maxFilesPerItem {
				files = append(files, name)
			}
			adds += f.GetAdditions()
			dels += f.GetDeletions()
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return files, adds, dels, nil
}

// releases lists upstream releases published after since. Drafts are
// skipped: a draft is not something a fork can port.
func (s *GitHubSource) releases(ctx context.Context, up repoRef, since time.Time) ([]Item, error) {
	opts := &gh.ListOptions{PerPage: githubPerPage}
	var out []Item
	for page := 0; page < maxReleasePages; page++ {
		rels, resp, err := s.client.Repositories.ListReleases(ctx, up.owner, up.repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list releases for %s/%s: %w", up.owner, up.repo, err)
		}
		for _, rel := range rels {
			if rel == nil || rel.GetDraft() || rel.PublishedAt == nil {
				continue
			}
			published := rel.GetPublishedAt().Time
			if !published.After(since) {
				continue
			}
			title := rel.GetName()
			if title == "" {
				title = rel.GetTagName()
			}
			out = append(out, Item{
				Kind:      KindRelease,
				Ref:       "release:" + rel.GetTagName(),
				Title:     title,
				Body:      rel.GetBody(),
				HTMLURL:   rel.GetHTMLURL(),
				Timestamp: published,
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

func formatPRRef(number int) string { return fmt.Sprintf("upstream#%d", number) }

func labelSet(labels []string) map[string]bool {
	if len(labels) == 0 {
		return nil
	}
	out := make(map[string]bool, len(labels))
	for _, l := range labels {
		if l = strings.TrimSpace(l); l != "" {
			out[strings.ToLower(l)] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func labelNames(labels []*gh.Label) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == nil {
			continue
		}
		if n := l.GetName(); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func anyLabel(have []string, want map[string]bool) bool {
	for _, l := range have {
		if want[strings.ToLower(l)] {
			return true
		}
	}
	return false
}
