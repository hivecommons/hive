package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const (
	curatorExtractLimit   = 50
	minCommentLength      = 20
	ingestRequestTimeout  = 30 * time.Second
)

// Curator extracts knowledge from merged PRs and ingests it into a wiki layer.
type Curator struct {
	ghClient  *gh.Client
	wikiURL   string
	org       string
	repos     []string
	config    CuratorConfig
	logger    *slog.Logger
}

// NewCurator creates a curator that watches the given repos for merged PRs.
func NewCurator(ghClient *gh.Client, wikiURL string, org string, repos []string, config CuratorConfig, logger *slog.Logger) *Curator {
	return &Curator{
		ghClient: ghClient,
		wikiURL:  wikiURL,
		org:      org,
		repos:    repos,
		config:   config,
		logger:   logger,
	}
}

// ExtractedFact is a fact candidate extracted from a PR before ingestion.
type ExtractedFact struct {
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Type       FactType  `json:"type"`
	Confidence float64   `json:"confidence"`
	Tags       []string  `json:"tags"`
	Related    []string  `json:"related,omitempty"`
	SourcePR   string    `json:"source_pr"`
	SourceDate time.Time `json:"source_date"`
}

// RunExtraction fetches merged PRs since the given time and extracts knowledge
// candidates from their diffs and review comments.
func (c *Curator) RunExtraction(ctx context.Context, since time.Time) ([]ExtractedFact, error) {
	var allFacts []ExtractedFact

	for _, repo := range c.repos {
		parts := strings.SplitN(repo, "/", 2)
		owner, repoName := c.org, repo
		if len(parts) == 2 {
			owner, repoName = parts[0], parts[1]
		}

		prs, err := c.fetchMergedPRs(ctx, owner, repoName, since)
		if err != nil {
			c.logger.Warn("failed to fetch PRs",
				"repo", repo,
				"error", err,
			)
			continue
		}

		for _, pr := range prs {
			facts := c.extractFromPR(ctx, owner, repoName, pr)
			allFacts = append(allFacts, facts...)
		}
	}

	c.logger.Info("extraction complete",
		"facts_extracted", len(allFacts),
		"repos_scanned", len(c.repos),
		"since", since.Format(time.RFC3339),
	)

	return allFacts, nil
}

// Ingest sends extracted facts to the wiki for storage. The wiki handles
// deduplication, cross-referencing, and confidence scoring.
func (c *Curator) Ingest(ctx context.Context, facts []ExtractedFact) error {
	if len(facts) == 0 {
		return nil
	}

	payload, err := json.Marshal(facts)
	if err != nil {
		return fmt.Errorf("marshaling facts: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, ingestRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.wikiURL+"/api/ingest", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("creating ingest request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ingest request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("ingest returned HTTP %d", resp.StatusCode)
	}

	c.logger.Info("facts ingested", "count", len(facts))
	return nil
}

func (c *Curator) fetchMergedPRs(ctx context.Context, owner, repo string, since time.Time) ([]*gh.PullRequest, error) {
	opts := &gh.PullRequestListOptions{
		State:     "closed",
		Sort:      "updated",
		Direction: "desc",
		ListOptions: gh.ListOptions{
			PerPage: curatorExtractLimit,
		},
	}

	prs, _, err := c.ghClient.PullRequests.List(ctx, owner, repo, opts)
	if err != nil {
		return nil, fmt.Errorf("listing PRs for %s/%s: %w", owner, repo, err)
	}

	var merged []*gh.PullRequest
	for _, pr := range prs {
		if pr.MergedAt == nil {
			continue
		}
		mergedAt := pr.MergedAt.Time
		if mergedAt.Before(since) {
			break
		}
		merged = append(merged, pr)
	}

	return merged, nil
}

func (c *Curator) extractFromPR(ctx context.Context, owner, repo string, pr *gh.PullRequest) []ExtractedFact {
	var facts []ExtractedFact
	prRef := fmt.Sprintf("%s/%s#%d", owner, repo, pr.GetNumber())

	if c.shouldExtractFrom("review_comments") {
		comments := c.fetchReviewComments(ctx, owner, repo, pr.GetNumber())
		for _, comment := range comments {
			if fact := classifyComment(comment, prRef, pr.GetMergedAt().Time); fact != nil {
				facts = append(facts, *fact)
			}
		}
	}

	if c.shouldExtractFrom("pr_comments") {
		comments := c.fetchIssueComments(ctx, owner, repo, pr.GetNumber())
		for _, comment := range comments {
			if fact := classifyComment(comment, prRef, pr.GetMergedAt().Time); fact != nil {
				facts = append(facts, *fact)
			}
		}
	}

	return facts
}

func (c *Curator) fetchReviewComments(ctx context.Context, owner, repo string, prNumber int) []string {
	comments, _, err := c.ghClient.PullRequests.ListComments(ctx, owner, repo, prNumber, nil)
	if err != nil {
		c.logger.Debug("failed to fetch review comments", "pr", prNumber, "error", err)
		return nil
	}

	var bodies []string
	for _, comment := range comments {
		body := comment.GetBody()
		if len(body) >= minCommentLength {
			bodies = append(bodies, body)
		}
	}
	return bodies
}

func (c *Curator) fetchIssueComments(ctx context.Context, owner, repo string, prNumber int) []string {
	comments, _, err := c.ghClient.Issues.ListComments(ctx, owner, repo, prNumber, nil)
	if err != nil {
		c.logger.Debug("failed to fetch issue comments", "pr", prNumber, "error", err)
		return nil
	}

	var bodies []string
	for _, comment := range comments {
		body := comment.GetBody()
		if len(body) >= minCommentLength {
			bodies = append(bodies, body)
		}
	}
	return bodies
}

func (c *Curator) shouldExtractFrom(source string) bool {
	for _, s := range c.config.ExtractFrom {
		if s == source {
			return true
		}
	}
	return false
}

// classifyComment examines a comment for knowledge signals and returns an
// ExtractedFact if the comment contains actionable knowledge. Returns nil for
// comments that are just conversational or too short.
func classifyComment(comment string, prRef string, mergedAt time.Time) *ExtractedFact {
	lower := strings.ToLower(comment)

	var factType FactType
	var confidence float64

	switch {
	case containsAny(lower, "always", "never", "must", "do not", "don't"):
		factType = FactGotcha
		confidence = 0.7
	case containsAny(lower, "regression", "broke", "broke again", "reverted"):
		factType = FactRegression
		confidence = 0.8
	case containsAny(lower, "pattern", "convention", "prefer", "should use", "best practice"):
		factType = FactPattern
		confidence = 0.6
	case containsAny(lower, "test", "coverage", "mock", "fixture", "assert"):
		factType = FactTestScaff
		confidence = 0.5
	case containsAny(lower, "decided", "agreed", "going forward", "from now on"):
		factType = FactDecision
		confidence = 0.7
	default:
		return nil
	}

	title := extractTitle(comment)
	if title == "" {
		return nil
	}

	return &ExtractedFact{
		Title:      title,
		Body:       truncate(comment, 500),
		Type:       factType,
		Confidence: confidence,
		Tags:       extractTags(comment),
		SourcePR:   prRef,
		SourceDate: mergedAt,
	}
}

func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// extractTitle pulls the first meaningful sentence from a comment.
func extractTitle(comment string) string {
	lines := strings.Split(comment, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) < minCommentLength {
			continue
		}
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "<!--") {
			continue
		}
		const maxTitleLen = 120
		if runes := []rune(trimmed); len(runes) > maxTitleLen {
			trimmed = string(runes[:maxTitleLen]) + "..."
		}
		return trimmed
	}
	return ""
}

func extractTags(comment string) []string {
	lower := strings.ToLower(comment)
	var tags []string

	tagKeywords := map[string]string{
		"typescript": "typescript",
		"react":      "react",
		"go ":        "go",
		"golang":     "go",
		"test":       "testing",
		"ci":         "ci",
		"docker":     "docker",
		"kubernetes": "kubernetes",
		"k8s":        "kubernetes",
		"helm":       "helm",
		"security":   "security",
		"auth":       "auth",
	}

	seen := make(map[string]bool)
	for keyword, tag := range tagKeywords {
		if strings.Contains(lower, keyword) && !seen[tag] {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}

	return tags
}

func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen]) + "..."
}
