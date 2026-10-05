package dashboard

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

const (
	externalSummaryMaxRunes = 280
	discoveryFeedReadLimit  = 1 << 20
)

type recheckDiscoveryCollector struct {
	cfg    config.SpektacularConfig
	gh     *gh.Client
	client *http.Client
}

func (s *Server) collectRecheckDiscovery(ctx context.Context, base Campaign, since time.Time) ([]knowledge.CampaignExternalEvidence, []knowledge.CampaignSourceFailure) {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return nil, nil
	}
	cfg := s.deps.Config.Runs.Spektacular
	if !cfg.Recheck.Discovery.Enabled || len(cfg.Recheck.Discovery.Sources) == 0 {
		return nil, nil
	}
	timeout := cfg.DiscoveryTimeout()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var ghc *gh.Client
	if s.deps.GHClient != nil {
		ghc = s.deps.GHClient.GoGitHub()
	}
	collector := recheckDiscoveryCollector{cfg: cfg, gh: ghc, client: http.DefaultClient}
	return collector.collect(ctx, base, since)
}

func (c recheckDiscoveryCollector) collect(ctx context.Context, base Campaign, since time.Time) ([]knowledge.CampaignExternalEvidence, []knowledge.CampaignSourceFailure) {
	maxTotal := c.cfg.DiscoveryMaxTotalItems()
	out := []knowledge.CampaignExternalEvidence{}
	failures := []knowledge.CampaignSourceFailure{}
	for _, src := range c.cfg.Recheck.Discovery.Sources {
		if len(out) >= maxTotal {
			break
		}
		items, err := c.collectSource(ctx, src, base, since)
		if err != nil {
			failures = append(failures, knowledge.CampaignSourceFailure{Name: src.Name, Reason: discoveryFailureReason(err)})
			continue
		}
		remaining := maxTotal - len(out)
		if len(items) > remaining {
			items = items[:remaining]
		}
		out = append(out, items...)
	}
	return out, failures
}

func (c recheckDiscoveryCollector) collectSource(ctx context.Context, src config.SpektacularRecheckDiscoverySource, base Campaign, since time.Time) ([]knowledge.CampaignExternalEvidence, error) {
	switch strings.TrimSpace(src.Kind) {
	case "upstream_release":
		return c.githubReleases(ctx, src, since)
	case "repo_activity":
		return c.githubActivity(ctx, src, since)
	case "standards_feed":
		return c.feedItems(ctx, src, since)
	case "landscape":
		return c.landscape(ctx, src, base, since)
	default:
		return nil, fmt.Errorf("bad_kind")
	}
}

func (c recheckDiscoveryCollector) githubReleases(ctx context.Context, src config.SpektacularRecheckDiscoverySource, since time.Time) ([]knowledge.CampaignExternalEvidence, error) {
	if c.gh == nil {
		return nil, fmt.Errorf("github_unavailable")
	}
	owner, repo, ok := splitRepoSlug(src.URLOrRepo)
	if !ok {
		return nil, fmt.Errorf("bad_repo")
	}
	opt := &gh.ListOptions{PerPage: src.EffectiveMaxItems()}
	releases, _, err := c.gh.Repositories.ListReleases(ctx, owner, repo, opt)
	if err != nil {
		return nil, err
	}
	out := []knowledge.CampaignExternalEvidence{}
	for _, rel := range releases {
		if rel.GetDraft() || (rel.GetPrerelease() && !src.AllowPrerelease) {
			continue
		}
		published := rel.GetPublishedAt().Time
		if !since.IsZero() && !published.After(since) {
			continue
		}
		out = append(out, knowledge.CampaignExternalEvidence{
			Source:      src.Name,
			Kind:        "upstream_release",
			Title:       rel.GetName(),
			URL:         rel.GetHTMLURL(),
			PublishedAt: published,
			Summary:     truncateDiscoveryRunes(rel.GetBody(), externalSummaryMaxRunes),
		})
		if len(out) >= src.EffectiveMaxItems() {
			break
		}
	}
	return out, nil
}

func (c recheckDiscoveryCollector) githubActivity(ctx context.Context, src config.SpektacularRecheckDiscoverySource, since time.Time) ([]knowledge.CampaignExternalEvidence, error) {
	releases, err := c.githubReleases(ctx, src, since)
	if err != nil {
		return nil, err
	}
	if c.gh == nil {
		return nil, fmt.Errorf("github_unavailable")
	}
	owner, repo, ok := splitRepoSlug(src.URLOrRepo)
	if !ok {
		return nil, fmt.Errorf("bad_repo")
	}
	prs, _, err := c.gh.PullRequests.List(ctx, owner, repo, &gh.PullRequestListOptions{
		State: "closed", Sort: "updated", Direction: "desc", ListOptions: gh.ListOptions{PerPage: src.EffectiveMaxItems()},
	})
	if err != nil {
		return nil, err
	}
	out := releases
	for _, pr := range prs {
		if pr.MergedAt == nil {
			continue
		}
		merged := pr.GetMergedAt().Time
		if !since.IsZero() && !merged.After(since) {
			continue
		}
		out = append(out, knowledge.CampaignExternalEvidence{
			Source:      src.Name,
			Kind:        "repo_activity",
			Title:       pr.GetTitle(),
			URL:         pr.GetHTMLURL(),
			PublishedAt: merged,
			Summary:     truncateDiscoveryRunes(pr.GetBody(), externalSummaryMaxRunes),
		})
		if len(out) >= src.EffectiveMaxItems() {
			break
		}
	}
	sortEvidence(out)
	if len(out) > src.EffectiveMaxItems() {
		out = out[:src.EffectiveMaxItems()]
	}
	return out, nil
}

func (c recheckDiscoveryCollector) feedItems(ctx context.Context, src config.SpektacularRecheckDiscoverySource, since time.Time) ([]knowledge.CampaignExternalEvidence, error) {
	u, err := url.Parse(strings.TrimSpace(src.URLOrRepo))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("bad_feed_url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	client := c.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("http_%d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, discoveryFeedReadLimit))
	if err != nil {
		return nil, err
	}
	items := parseFeedEvidence(src.Name, body)
	out := []knowledge.CampaignExternalEvidence{}
	for _, item := range items {
		if !since.IsZero() && !item.PublishedAt.IsZero() && !item.PublishedAt.After(since) {
			continue
		}
		out = append(out, item)
		if len(out) >= src.EffectiveMaxItems() {
			break
		}
	}
	return out, nil
}

func (c recheckDiscoveryCollector) landscape(ctx context.Context, src config.SpektacularRecheckDiscoverySource, _ Campaign, since time.Time) ([]knowledge.CampaignExternalEvidence, error) {
	raw, err := readLandscape()
	if err != nil {
		return nil, err
	}
	repos := githubReposFromMarkdown(string(raw))
	out := []knowledge.CampaignExternalEvidence{}
	for _, repo := range repos {
		child := src
		child.URLOrRepo = repo
		child.Name = firstRunNonEmpty(src.Name, "landscape") + ":" + repo
		items, err := c.githubReleases(ctx, child, since)
		if err != nil {
			continue
		}
		for _, item := range items {
			item.Source = src.Name
			item.Kind = "landscape"
			out = append(out, item)
			if len(out) >= src.EffectiveMaxItems() {
				sortEvidence(out)
				return out, nil
			}
		}
	}
	sortEvidence(out)
	return out, nil
}

func parseFeedEvidence(source string, body []byte) []knowledge.CampaignExternalEvidence {
	type atomEntry struct {
		Title   string `xml:"title"`
		Summary string `xml:"summary"`
		Content string `xml:"content"`
		Updated string `xml:"updated"`
		Link    []struct {
			Href string `xml:"href,attr"`
		} `xml:"link"`
	}
	type atomFeed struct {
		Entries []atomEntry `xml:"entry"`
	}
	var atom atomFeed
	if err := xml.Unmarshal(body, &atom); err == nil && len(atom.Entries) > 0 {
		out := make([]knowledge.CampaignExternalEvidence, 0, len(atom.Entries))
		for _, e := range atom.Entries {
			link := ""
			if len(e.Link) > 0 {
				link = e.Link[0].Href
			}
			out = append(out, knowledge.CampaignExternalEvidence{Source: source, Kind: "standards_feed", Title: strings.TrimSpace(e.Title), URL: link, PublishedAt: parseFeedTime(e.Updated), Summary: truncateDiscoveryRunes(firstRunNonEmpty(e.Summary, e.Content), externalSummaryMaxRunes)})
		}
		return out
	}
	type rssItem struct {
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		Description string `xml:"description"`
		PubDate     string `xml:"pubDate"`
	}
	type rss struct {
		Items []rssItem `xml:"channel>item"`
	}
	var r rss
	_ = xml.Unmarshal(body, &r)
	out := make([]knowledge.CampaignExternalEvidence, 0, len(r.Items))
	for _, item := range r.Items {
		out = append(out, knowledge.CampaignExternalEvidence{Source: source, Kind: "standards_feed", Title: strings.TrimSpace(item.Title), URL: strings.TrimSpace(item.Link), PublishedAt: parseFeedTime(item.PubDate), Summary: truncateDiscoveryRunes(item.Description, externalSummaryMaxRunes)})
	}
	return out
}

func parseFeedTime(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, time.RFC1123Z, time.RFC1123} {
		if t, err := time.Parse(layout, value); err == nil {
			return t
		}
	}
	return time.Time{}
}

func splitRepoSlug(value string) (string, string, bool) {
	value = strings.TrimSpace(value)
	if u, err := url.Parse(value); err == nil && u.Hostname() != "" {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 2 {
			return parts[0], strings.TrimSuffix(parts[1], ".git"), true
		}
	}
	owner, repo, ok := strings.Cut(value, "/")
	return strings.TrimSpace(owner), strings.TrimSpace(repo), ok && strings.TrimSpace(owner) != "" && strings.TrimSpace(repo) != ""
}

func truncateDiscoveryRunes(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	if maxRunes <= 0 || utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[:maxRunes])
}

func discoveryFailureReason(err error) string {
	if err == nil {
		return ""
	}
	if err == context.DeadlineExceeded {
		return "timeout"
	}
	return strings.ReplaceAll(strings.TrimSpace(err.Error()), "\n", " ")
}

func sortEvidence(items []knowledge.CampaignExternalEvidence) {
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].PublishedAt.After(items[j].PublishedAt)
	})
}

func readLandscape() ([]byte, error) {
	for _, path := range []string{"docs/landscape.md", "src/docs/landscape.md"} {
		if raw, err := os.ReadFile(path); err == nil {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("landscape_unavailable")
}

func githubReposFromMarkdown(markdown string) []string {
	re := regexp.MustCompile(`https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)`)
	seen := map[string]bool{}
	out := []string{}
	for _, match := range re.FindAllStringSubmatch(markdown, -1) {
		repo := strings.TrimSuffix(match[1], ".git")
		if !seen[repo] {
			seen[repo] = true
			out = append(out, repo)
		}
	}
	return out
}
