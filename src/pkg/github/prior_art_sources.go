package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/priorart"
)

// Prior-art gate forge sources (hivecommons/hive#13575). Each is one
// bounded GitHub search per issue; the gate's Searcher caches the report
// per issue content hash, so an unchanged issue costs nothing on later
// cycles. Every source is soft: an API error drops that source's
// candidates and the rest still answer.
const (
	// priorArtMergedPRWindow is how far back merged PRs count as prior art.
	priorArtMergedPRWindow = 365 * 24 * time.Hour
	// priorArtSearchTerms is how many top terms form one forge query.
	priorArtSearchTerms = 4
	// priorArtSearchPerPage bounds each search's page.
	priorArtSearchPerPage = 10
	// priorArtCodeSearchPerPage bounds the code search page: code search is
	// the dearest GitHub search (its own low rate limit).
	priorArtCodeSearchPerPage = 5
	// priorArtDocPaths are the paths whose code-search hits read as docs.
	priorArtDocGlob = "docs"
)

// PriorArtSources returns the forge-backed sources for the gate: merged
// PRs (last year) and closed issues, docs (README, CHANGELOG, docs/) and
// code identifiers. Code search needs an authenticated client; the source
// is still returned and fails softly without one.
func (c *Client) PriorArtSources() []priorart.Source {
	if c == nil {
		return nil
	}
	return []priorart.Source{
		priorart.SourceFunc{SourceName: "merged_prs", Fn: c.priorArtMergedPRs},
		priorart.SourceFunc{SourceName: "closed_issues", Fn: c.priorArtClosedIssues},
		priorart.SourceFunc{SourceName: "docs", Fn: c.priorArtDocs},
		priorart.SourceFunc{SourceName: "code", Fn: c.priorArtCode},
	}
}

func (c *Client) priorArtMergedPRs(ctx context.Context, q priorart.Query) ([]priorart.Candidate, error) {
	terms := priorArtQueryTerms(q)
	if terms == "" {
		return nil, nil
	}
	since := time.Now().Add(-priorArtMergedPRWindow).UTC().Format("2006-01-02")
	query := fmt.Sprintf("repo:%s is:pr is:merged merged:>=%s %s", q.Repo, since, terms)
	return c.priorArtIssueSearch(ctx, query, priorart.KindMergedPR)
}

func (c *Client) priorArtClosedIssues(ctx context.Context, q priorart.Query) ([]priorart.Candidate, error) {
	terms := priorArtQueryTerms(q)
	if terms == "" {
		return nil, nil
	}
	query := fmt.Sprintf("repo:%s is:issue is:closed %s", q.Repo, terms)
	return c.priorArtIssueSearch(ctx, query, priorart.KindClosedIssue)
}

func (c *Client) priorArtIssueSearch(ctx context.Context, query string, kind priorart.Kind) ([]priorart.Candidate, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	result, _, err := c.client.Search.Issues(ctx, query, &gh.SearchOptions{
		Sort: "updated", Order: "desc",
		ListOptions: gh.ListOptions{PerPage: priorArtSearchPerPage},
	})
	if err != nil {
		return nil, err
	}
	out := make([]priorart.Candidate, 0, len(result.Issues))
	for _, is := range result.Issues {
		if is == nil {
			continue
		}
		k := kind
		if kind == priorart.KindClosedIssue && is.IsPullRequest() {
			continue
		}
		if kind == priorart.KindClosedIssue && strings.EqualFold(is.GetStateReason(), "not_planned") && !labelPresent(issueLabelNames(is.Labels), "duplicate") {
			// A declined issue is not prior art for a feature; a
			// duplicate-closed one still points at where it lives.
			continue
		}
		out = append(out, priorart.Candidate{
			Kind:  k,
			Title: is.GetTitle(),
			Text:  is.GetBody(),
			URL:   is.GetHTMLURL(),
			Ref:   fmt.Sprintf("#%d", is.GetNumber()),
		})
	}
	return out, nil
}

// priorArtDocs searches README, CHANGELOG and docs/ for the identifiers
// first and the top plain terms otherwise.
func (c *Client) priorArtDocs(ctx context.Context, q priorart.Query) ([]priorart.Candidate, error) {
	terms := priorArtCodeQueryTerms(q)
	if terms == "" {
		return nil, nil
	}
	query := fmt.Sprintf("repo:%s %s (path:%s OR filename:README.md OR filename:CHANGELOG.md)", q.Repo, terms, priorArtDocGlob)
	return c.priorArtCodeSearch(ctx, query, priorart.KindDoc, func(path string) bool {
		low := strings.ToLower(path)
		return strings.HasSuffix(low, ".md") || strings.HasSuffix(low, ".rst") || strings.HasSuffix(low, ".txt")
	})
}

// priorArtCode searches source files for the issue's identifiers; with no
// identifier the source answers nothing, since plain words in code are
// noise.
func (c *Client) priorArtCode(ctx context.Context, q priorart.Query) ([]priorart.Candidate, error) {
	if len(q.Identifiers) == 0 {
		return nil, nil
	}
	ids := q.Identifiers
	if len(ids) > 2 {
		ids = ids[:2]
	}
	query := fmt.Sprintf("repo:%s %s NOT path:%s", q.Repo, priorArtQuote(ids), priorArtDocGlob)
	return c.priorArtCodeSearch(ctx, query, priorart.KindCode, func(path string) bool {
		low := strings.ToLower(path)
		return !strings.HasSuffix(low, ".md") && !strings.Contains(low, "_test.") && !strings.Contains(low, "/testdata/")
	})
}

func (c *Client) priorArtCodeSearch(ctx context.Context, query string, kind priorart.Kind, keep func(path string) bool) ([]priorart.Candidate, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	result, _, err := c.client.Search.Code(ctx, query, &gh.SearchOptions{
		TextMatch:   true,
		ListOptions: gh.ListOptions{PerPage: priorArtCodeSearchPerPage},
	})
	if err != nil {
		return nil, err
	}
	out := make([]priorart.Candidate, 0, len(result.CodeResults))
	for _, r := range result.CodeResults {
		path := r.GetPath()
		if path == "" || (keep != nil && !keep(path)) {
			continue
		}
		var text strings.Builder
		for _, m := range r.TextMatches {
			text.WriteString(m.GetFragment())
			text.WriteString("\n")
		}
		k := kind
		if kind == priorart.KindCode && looksLikeSettingPath(path) {
			k = priorart.KindSetting
		}
		out = append(out, priorart.Candidate{
			Kind:  k,
			Title: path,
			Text:  text.String(),
			URL:   r.GetHTMLURL(),
			Ref:   path,
		})
	}
	return out, nil
}

// looksLikeSettingPath marks config-shaped files so a hit reads "setting"
// rather than "code".
func looksLikeSettingPath(path string) bool {
	low := strings.ToLower(path)
	return strings.Contains(low, "config") || strings.HasSuffix(low, ".yaml") || strings.HasSuffix(low, ".yml") || strings.HasSuffix(low, ".toml")
}

// priorArtQueryTerms is the top plain terms, quoted, for issue/PR search.
func priorArtQueryTerms(q priorart.Query) string {
	return priorArtQuote(q.TopTerms(priorArtSearchTerms))
}

// priorArtCodeQueryTerms prefers identifiers (exact and rare) and falls
// back to the top two plain terms.
func priorArtCodeQueryTerms(q priorart.Query) string {
	if len(q.Identifiers) > 0 {
		ids := q.Identifiers
		if len(ids) > 2 {
			ids = ids[:2]
		}
		return priorArtQuote(ids)
	}
	return priorArtQuote(q.TopTerms(2))
}

func priorArtQuote(terms []string) string {
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		t = strings.Trim(strings.TrimSpace(t), `"`)
		if t == "" {
			continue
		}
		if strings.ContainsAny(t, " -./_:") {
			parts = append(parts, `"`+t+`"`)
		} else {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}
