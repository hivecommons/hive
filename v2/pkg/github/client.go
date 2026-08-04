package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// ErrNoGitHubClient is returned by *Client methods invoked on a nil receiver.
//
// A nil *Client is a legitimate, expected runtime state: a hive whose GitHub
// App key is missing or whose app_id is still the placeholder boots in
// dashboard-only mode so its owner can see and fix the problem, and it runs
// with no GitHub client at all. Callers must treat this as "GitHub is
// unavailable right now", not as a bug — it is never a reason to exit.
var ErrNoGitHubClient = errors.New("no github client configured (hive is running without GitHub credentials)")

type Client struct {
	client       *gh.Client
	org          string
	reposMu      sync.RWMutex
	repos        []string
	exemptLabels []string
	logger       *slog.Logger
	appAuth      *AppAuth // nil for token-authenticated clients
	// prAuthz gates PR-open requests from the request-file watcher against the
	// per-agent ACMM write-policy + forge-resistance. nil fails closed. Set by
	// StartPRRequestWatcher.
	prAuthz PRRequestAuthorizer
	// attribution holds the invocation-attribution hooks (trailer gate,
	// per-agent metadata resolver, audit sink) — see attribution.go. Guarded by
	// attribMu because the hooks are installed in stages during startup while
	// the PR-request watcher goroutine may already be reading them.
	attribMu    sync.RWMutex
	attribution AttributionHooks
}

// GoGitHub returns the underlying go-github client for direct API access.
func (c *Client) GoGitHub() *gh.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// AppAuth returns the GitHub App auth backing this client, or nil when the
// client authenticates with a static token instead of an app installation.
func (c *Client) AppAuth() *AppAuth {
	if c == nil {
		return nil
	}
	return c.appAuth
}

type Issue struct {
	Repo           string    `json:"repo"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Author         string    `json:"author"`
	Labels         []string  `json:"labels"`
	Assignees      []string  `json:"assignees"`
	CreatedAt      time.Time `json:"created_at"`
	AgeMinutes     int       `json:"age_minutes"`
	URL            string    `json:"url"`
	IsTracker      bool      `json:"is_tracker"`
	ComplexityTier string    `json:"complexity_tier,omitempty"`
	ModelRec       string    `json:"model_recommendation,omitempty"`
	Lane           string    `json:"lane,omitempty"`
}

type PullRequest struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Labels    []string  `json:"labels"`
	Draft     bool      `json:"draft"`
	CreatedAt time.Time `json:"created_at"`
	URL       string    `json:"url"`
	// Mergeable is a tri-state: MergeableYes, MergeableNo, or MergeableUnknown.
	// It is intentionally NOT a bool: a bool zero-values to false, which is
	// indistinguishable from "GitHub says this PR cannot be merged" and would
	// silently gate every PR shut whenever the value was never populated.
	// The zero value is MergeableUnknown ("") so an unfilled field reads as
	// "we do not know" rather than a false negative.
	Mergeable Mergeable `json:"mergeable"`
	CIStatus  string    `json:"ci_status"`
	HeadSHA   string    `json:"head_sha,omitempty"`
}

// Mergeable is a tri-state mergeability verdict for a pull request.
type Mergeable string

const (
	// MergeableUnknown is the zero value: mergeability has not been determined.
	// Consumers gating a merge MUST treat this as "do not know", never as "no".
	MergeableUnknown Mergeable = ""
	// MergeableYes means GitHub reports the PR can be merged.
	MergeableYes Mergeable = "yes"
	// MergeableNo means GitHub reports the PR cannot be merged (conflicts, etc).
	MergeableNo Mergeable = "no"
)

// mergeableFromState maps a GitHub mergeable_state to a tri-state verdict.
//
// mergeable_state is used in preference to the raw "mergeable" bool because
// this project's standing policy ignores non-required checks (Playwright,
// tide). GitHub reports "unstable" when the PR is cleanly mergeable but some
// non-required check is pending or failing — that still counts as mergeable.
//
// Reference: "clean" and "has_hooks" are fully green; "unstable" is mergeable
// with non-required checks outstanding; "blocked" means a required review or
// status is missing; "dirty" means conflicts; "unknown" means GitHub is still
// computing the merge commit in the background.
func mergeableFromState(state string, mergeable *bool) Mergeable {
	switch state {
	case "clean", "has_hooks", "unstable":
		return MergeableYes
	case "dirty", "blocked", "behind", "draft":
		return MergeableNo
	}
	// "unknown" (or an unrecognised state): GitHub is still computing.
	// Fall back to the raw bool only when it was actually present.
	if mergeable != nil {
		if *mergeable {
			return MergeableYes
		}
		return MergeableNo
	}
	return MergeableUnknown
}

type ActionableResult struct {
	GeneratedAt time.Time             `json:"generated_at"`
	Issues      IssueResult           `json:"issues"`
	PRs         PRResult              `json:"prs"`
	Hold        HoldResult            `json:"hold"`
	Clusters    []IssueCluster        `json:"clusters,omitempty"`
	TotalByRepo map[string]RepoCounts `json:"total_by_repo,omitempty"`
}

type RepoCounts struct {
	Issues int `json:"issues"`
	PRs    int `json:"prs"`
}

type IssueResult struct {
	Count         int     `json:"count"`
	Items         []Issue `json:"items"`
	SLAViolations int     `json:"sla_violations"`
}

type PRResult struct {
	Count int           `json:"count"`
	Items []PullRequest `json:"items"`
}

type HoldResult struct {
	Issues int        `json:"issues"`
	PRs    int        `json:"prs"`
	Total  int        `json:"total"`
	Items  []HoldItem `json:"items"`
}

type HoldItem struct {
	Number int    `json:"number"`
	Repo   string `json:"repo"`
	Title  string `json:"title"`
	Type   string `json:"type"`
}

type IssueCluster struct {
	Key    string  `json:"key"`
	Count  int     `json:"count"`
	Issues []Issue `json:"issues"`
}

var HoldLabels = []string{"hold", "on-hold", "hold/review"}
var PermanentExemptLabels = []string{"do-not-merge"}
var exemptFiles = []string{"ADOPTERS.md", "ADOPTERS.MD"}

const slaThresholdMinutes = 30

// NewClient creates a GitHub API client. If apiURL is non-empty and differs
// from the default (https://api.github.com), the client's BaseURL and
// UploadURL are overridden for GitHub Enterprise compatibility.
func NewClient(token string, org string, repos []string, logger *slog.Logger, apiURL string) *Client {
	client := gh.NewClient(nil).WithAuthToken(token)
	setBaseURL(client, apiURL)
	return &Client{
		client: client,
		org:    org,
		repos:  repos,
		logger: logger,
	}
}

// setBaseURL configures a go-github client for a custom API URL (GHE).
// If apiURL is empty or the default, no change is made.
func setBaseURL(client *gh.Client, apiURL string) {
	if apiURL == "" || apiURL == "https://api.github.com" {
		return
	}
	// Ensure trailing slash for url.Parse compatibility.
	normalized := apiURL
	if !strings.HasSuffix(normalized, "/") {
		normalized += "/"
	}
	base, err := url.Parse(normalized)
	if err != nil {
		return
	}
	client.BaseURL = base
	// Upload URL follows the same base for GHE instances.
	client.UploadURL = base
}

// NewClientForTest creates a client pointing at a test server. Exported for
// cross-package testing (e.g. dashboard tests that need a mock GH client).
func NewClientForTest(serverURL string, org string, repos []string, logger *slog.Logger) *Client {
	c := NewClient("fake", org, repos, logger, "")
	base, err := url.Parse(serverURL + "/")
	if err != nil {
		panic("bad test server URL: " + err.Error())
	}
	c.client.BaseURL = base
	return c
}

// SetRepos is nil-receiver safe. A hive that booted without usable GitHub
// credentials runs with a nil *Client for the life of the process, and the
// config-reload / project-claim / override-migration paths all re-sync the repo
// list unconditionally. Making the setter a no-op on nil is safer than adding a
// guard at each of those call sites, which a future one would forget.
func (c *Client) SetRepos(repos []string) {
	if c == nil {
		return
	}
	c.reposMu.Lock()
	defer c.reposMu.Unlock()
	c.repos = repos
}

func (c *Client) getRepos() []string {
	c.reposMu.RLock()
	defer c.reposMu.RUnlock()
	result := make([]string, len(c.repos))
	copy(result, c.repos)
	return result
}

func (c *Client) EnumerateActionable(ctx context.Context) (*ActionableResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	now := time.Now()
	result := &ActionableResult{
		GeneratedAt: now,
	}

	var allIssues []Issue
	var allPRs []PullRequest
	var holdItems []HoldItem
	totalByRepo := make(map[string]RepoCounts)

	repos := c.getRepos()
	failedRepos := 0
	var lastFetchErr error
	for _, repo := range repos {
		issues, held, issueTotal, err := c.fetchIssues(ctx, repo, now)
		if err != nil {
			c.logger.Warn("failed to fetch issues", "repo", repo, "error", err)
			failedRepos++
			lastFetchErr = err
			continue
		}
		allIssues = append(allIssues, issues...)
		holdItems = append(holdItems, held...)

		prs, heldPRs, prTotal, err := c.fetchPRs(ctx, repo)
		if err != nil {
			// Issues for this repo were already collected; a PR-only failure
			// is partial and must not count toward the all-repos-failed guard,
			// which would discard real issue data and report an outage.
			c.logger.Warn("failed to fetch PRs", "repo", repo, "error", err)
			continue
		}
		allPRs = append(allPRs, prs...)
		holdItems = append(holdItems, heldPRs...)

		totalByRepo[repo] = RepoCounts{Issues: issueTotal, PRs: prTotal}
	}

	// If every repo failed (e.g. API rate limit exhausted), returning a
	// zero-count result would tell the governor the queue is empty and
	// idle the agents. Surface the failure so callers keep prior state.
	if len(repos) > 0 && failedRepos >= len(repos) {
		return nil, fmt.Errorf("all %d repos failed to enumerate (last error: %w)", len(repos), lastFetchErr)
	}

	sort.Slice(allIssues, func(i, j int) bool {
		return allIssues[i].AgeMinutes > allIssues[j].AgeMinutes
	})

	slaViolations := 0
	for _, issue := range allIssues {
		if issue.AgeMinutes > slaThresholdMinutes {
			slaViolations++
		}
	}

	holdIssueCount := 0
	holdPRCount := 0
	for _, h := range holdItems {
		if h.Type == "issue" {
			holdIssueCount++
		} else {
			holdPRCount++
		}
	}

	result.Issues = IssueResult{
		Count:         len(allIssues),
		Items:         allIssues,
		SLAViolations: slaViolations,
	}
	result.PRs = PRResult{
		Count: len(allPRs),
		Items: allPRs,
	}
	result.Hold = HoldResult{
		Issues: holdIssueCount,
		PRs:    holdPRCount,
		Total:  len(holdItems),
		Items:  holdItems,
	}
	result.TotalByRepo = totalByRepo

	return result, nil
}

// splitRepo handles repo names that may include org prefix (e.g. "org/repo").
// When repo contains "/", the prefix is used as owner; otherwise c.org is used.
func (c *Client) splitRepo(repo string) (owner, repoName string) {
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		return parts[0], parts[1]
	}
	return c.org, repo
}

func (c *Client) fetchIssues(ctx context.Context, repo string, now time.Time) (actionable []Issue, held []HoldItem, totalIssues int, err error) {
	owner, repoName := c.splitRepo(repo)
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: 100},
	}

	var allIssues []*gh.Issue
	for {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repoName, opts)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("listing issues for %s/%s: %w", owner, repoName, err)
		}
		allIssues = append(allIssues, issues...)
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}

	for _, issue := range allIssues {
		if issue.IsPullRequest() {
			continue
		}

		totalIssues++
		labels := extractLabels(issue.Labels)

		if isHeld(labels) {
			held = append(held, HoldItem{
				Number: issue.GetNumber(),
				Repo:   repo,
				Title:  issue.GetTitle(),
				Type:   "issue",
			})
			continue
		}

		if c.isExempt(labels) {
			continue
		}

		ageMinutes := int(now.Sub(issue.GetCreatedAt().Time).Minutes())

		actionable = append(actionable, Issue{
			Repo:       repo,
			Number:     issue.GetNumber(),
			Title:      issue.GetTitle(),
			Author:     safeGetLogin(issue.GetUser()),
			Labels:     labels,
			Assignees:  extractAssignees(issue.Assignees),
			CreatedAt:  issue.GetCreatedAt().Time,
			AgeMinutes: ageMinutes,
			URL:        issue.GetHTMLURL(),
			IsTracker:  isTracker(issue.GetTitle(), labels),
		})
	}

	return actionable, held, totalIssues, nil
}

func (c *Client) fetchPRs(ctx context.Context, repo string) (actionable []PullRequest, held []HoldItem, totalPRs int, err error) {
	owner, repoName := c.splitRepo(repo)
	opts := &gh.PullRequestListOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: 100},
	}

	var allPRs []*gh.PullRequest
	for {
		prs, resp, err := c.client.PullRequests.List(ctx, owner, repoName, opts)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("listing PRs for %s/%s: %w", owner, repoName, err)
		}
		allPRs = append(allPRs, prs...)
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}

	for _, pr := range allPRs {
		totalPRs++
		labels := extractPRLabels(pr.Labels)

		if isHeld(labels) {
			held = append(held, HoldItem{
				Number: pr.GetNumber(),
				Repo:   repo,
				Title:  pr.GetTitle(),
				Type:   "pr",
			})
			continue
		}

		if c.isExempt(labels) {
			continue
		}

		if pr.GetDraft() {
			continue
		}

		headSHA := ""
		if pr.GetHead() != nil {
			headSHA = pr.GetHead().GetSHA()
		}

		actionable = append(actionable, PullRequest{
			Repo:      repo,
			Number:    pr.GetNumber(),
			Title:     pr.GetTitle(),
			Author:    safeGetLogin(pr.GetUser()),
			Labels:    labels,
			Draft:     pr.GetDraft(),
			CreatedAt: pr.GetCreatedAt().Time,
			URL:       pr.GetHTMLURL(),
			// Mergeable is deliberately NOT set here. The PullRequests.List
			// endpoint never populates "mergeable" — GitHub computes it
			// per-PR and returns it only from the single-PR GET. Reading it
			// here would yield false for every PR. EnrichCIStatus fills it in
			// from a per-PR fetch; until then it stays MergeableUnknown.
			HeadSHA: headSHA,
		})
	}

	return actionable, held, totalPRs, nil
}

// EnrichCIStatus fetches check-run results for each PR's HEAD commit
// and sets the CIStatus field to "success", "failure", or "pending".
// The "tide" check is skipped — it reports Prow merge-bot state, not CI.
func (c *Client) EnrichCIStatus(ctx context.Context, prs []PullRequest) {
	if c == nil {
		return
	}
	const ciStatusSuccess = "success"
	const ciStatusFailure = "failure"
	const ciStatusPending = "pending"

	for i := range prs {
		if prs[i].HeadSHA == "" {
			prs[i].CIStatus = ciStatusPending
			continue
		}
		owner, repoName := c.splitRepo(prs[i].Repo)

		// Fetch the PR individually to learn its mergeability. The list
		// endpoint that produced these PullRequests never populates
		// "mergeable"/"mergeable_state" — GitHub computes them per-PR and
		// returns them only from this single-PR GET. On error we leave the
		// field as MergeableUnknown rather than guessing.
		if full, _, err := c.client.PullRequests.Get(ctx, owner, repoName, prs[i].Number); err != nil {
			c.logger.Warn("failed to fetch PR mergeability", "repo", prs[i].Repo, "pr", prs[i].Number, "error", err)
		} else {
			prs[i].Mergeable = mergeableFromState(full.GetMergeableState(), full.Mergeable)
		}

		checkRuns, _, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, repoName, prs[i].HeadSHA, &gh.ListCheckRunsOptions{
			ListOptions: gh.ListOptions{PerPage: 100},
		})
		if err != nil {
			c.logger.Warn("failed to fetch check runs", "repo", prs[i].Repo, "pr", prs[i].Number, "error", err)
			prs[i].CIStatus = ciStatusPending
			continue
		}
		if checkRuns.GetTotal() == 0 {
			prs[i].CIStatus = ciStatusPending
			continue
		}
		hasFail := false
		allDone := true
		ciChecksFound := 0
		for _, cr := range checkRuns.CheckRuns {
			if cr.GetName() == "tide" {
				continue
			}
			ciChecksFound++
			if cr.GetStatus() != "completed" {
				allDone = false
				continue
			}
			conclusion := cr.GetConclusion()
			if conclusion == "failure" || conclusion == "action_required" {
				hasFail = true
			}
		}
		if ciChecksFound == 0 {
			prs[i].CIStatus = ciStatusPending
			continue
		}
		switch {
		case hasFail:
			prs[i].CIStatus = ciStatusFailure
		case allDone:
			prs[i].CIStatus = ciStatusSuccess
		default:
			prs[i].CIStatus = ciStatusPending
		}
	}
}

func extractLabels(labels []*gh.Label) []string {
	var result []string
	for _, l := range labels {
		result = append(result, l.GetName())
	}
	return result
}

func extractPRLabels(labels []*gh.Label) []string {
	return extractLabels(labels)
}

func extractAssignees(users []*gh.User) []string {
	var result []string
	for _, u := range users {
		if login := safeGetLogin(u); login != "" {
			result = append(result, login)
		}
	}
	return result
}

func safeGetLogin(u *gh.User) string {
	if u == nil {
		return ""
	}
	return u.GetLogin()
}

func isHeld(labels []string) bool {
	for _, label := range labels {
		lower := strings.ToLower(label)
		for _, sub := range HoldLabels {
			if strings.Contains(lower, sub) {
				return true
			}
		}
	}
	return false
}

// SetExemptLabels is nil-receiver safe for the same reason as SetRepos.
func (c *Client) SetExemptLabels(labels []string) {
	if c == nil {
		return
	}
	c.exemptLabels = labels
}

func (c *Client) isExempt(labels []string) bool {
	for _, label := range labels {
		for _, perm := range PermanentExemptLabels {
			if strings.EqualFold(label, perm) || strings.HasPrefix(label, perm) {
				return true
			}
		}
		for _, exempt := range c.exemptLabels {
			if strings.EqualFold(label, exempt) || strings.HasPrefix(label, exempt) {
				return true
			}
		}
	}
	return false
}

func isTracker(title string, labels []string) bool {
	if strings.HasPrefix(title, "[Tracker]") {
		return true
	}
	for _, l := range labels {
		if l == "meta-tracker" {
			return true
		}
	}
	return false
}

type RateLimitInfo struct {
	Core    RateLimitEntry `json:"core"`
	Search  RateLimitEntry `json:"search"`
	GraphQL RateLimitEntry `json:"graphql"`
}

type RateLimitEntry struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset"`
}

func (c *Client) RateLimits(ctx context.Context) (*RateLimitInfo, error) {
	limits, _, err := c.client.RateLimit.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching rate limits: %w", err)
	}

	info := &RateLimitInfo{}
	if limits.Core != nil {
		info.Core = RateLimitEntry{
			Limit:     limits.Core.Limit,
			Remaining: limits.Core.Remaining,
			Reset:     limits.Core.Reset.Time,
		}
	}
	if limits.Search != nil {
		info.Search = RateLimitEntry{
			Limit:     limits.Search.Limit,
			Remaining: limits.Search.Remaining,
			Reset:     limits.Search.Reset.Time,
		}
	}
	if limits.GraphQL != nil {
		info.GraphQL = RateLimitEntry{
			Limit:     limits.GraphQL.Limit,
			Remaining: limits.GraphQL.Remaining,
			Reset:     limits.GraphQL.Reset.Time,
		}
	}

	return info, nil
}

func (c *Client) LatestCommitHash(ctx context.Context, owner, repo, branch string) (string, error) {
	ref, _, err := c.client.Git.GetRef(ctx, owner, repo, "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("fetching ref %s/%s@%s: %w", owner, repo, branch, err)
	}
	return ref.GetObject().GetSHA(), nil
}

// CommitMessage returns the first line of the commit message for the given SHA.
func (c *Client) CommitMessage(ctx context.Context, owner, repo, sha string) (string, error) {
	commit, _, err := c.client.Repositories.GetCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		return "", fmt.Errorf("fetching commit %s/%s@%s: %w", owner, repo, sha, err)
	}
	msg := commit.GetCommit().GetMessage()
	if idx := strings.Index(msg, "\n"); idx >= 0 {
		msg = msg[:idx]
	}
	return msg, nil
}

func (c *Client) GetRepo(ctx context.Context, owner, repo string) (*gh.Repository, *gh.Response, error) {
	return c.client.Repositories.Get(ctx, owner, repo)
}

func (c *Client) GetContributorCount(ctx context.Context, owner, repo string) (int, error) {
	const perPage = 100
	opts := &gh.ListContributorsOptions{
		ListOptions: gh.ListOptions{PerPage: perPage},
	}
	var total int
	for {
		contribs, resp, err := c.client.Repositories.ListContributors(ctx, owner, repo, opts)
		if err != nil {
			return 0, fmt.Errorf("listing contributors for %s/%s: %w", owner, repo, err)
		}
		total += len(contribs)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return total, nil
}

func (c *Client) GetFileContent(ctx context.Context, owner, repo, path string) (string, error) {
	return c.GetFileContentRef(ctx, owner, repo, path, "")
}

// GetFileContentRef reads a single file's decoded contents from a repo at an
// optional git ref (branch, tag, or SHA). An empty ref uses the repo's default
// branch. Errors if the path resolves to a directory rather than a file.
func (c *Client) GetFileContentRef(ctx context.Context, owner, repo, path, ref string) (string, error) {
	var opts *gh.RepositoryContentGetOptions
	if ref != "" {
		opts = &gh.RepositoryContentGetOptions{Ref: ref}
	}
	fc, _, _, err := c.client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return "", fmt.Errorf("getting file %s/%s/%s: %w", owner, repo, path, err)
	}
	if fc == nil {
		return "", fmt.Errorf("file %s/%s/%s is a directory", owner, repo, path)
	}
	content, err := fc.GetContent()
	if err != nil {
		return "", fmt.Errorf("decoding %s/%s/%s: %w", owner, repo, path, err)
	}
	return content, nil
}

// SearchPRCount searches GitHub for PRs by author within an org.
// state is "open" or "merged".
func (c *Client) SearchPRCount(ctx context.Context, author, org, state string) (int, error) {
	qualifier := fmt.Sprintf("type:pr author:%s org:%s", author, org)
	if state == "merged" {
		qualifier += " is:merged"
	} else {
		qualifier += " is:open"
	}

	result, _, err := c.client.Search.Issues(ctx, qualifier, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil {
		return 0, fmt.Errorf("searching PRs: %w", err)
	}
	return result.GetTotal(), nil
}

// SearchOutreachPRCount searches GitHub for outreach PRs by author on repos
// OUTSIDE the org. These are PRs with the project name in the title, authored
// by the AI author, on external repos (e.g., awesome-lists, adopters, install missions).
// state is "open" or "merged".
func (c *Client) SearchOutreachPRCount(ctx context.Context, author, org, projectName, state string) (int, error) {
	if projectName == "" {
		return 0, fmt.Errorf("projectName is required for outreach PR search (set project.name in config)")
	}
	// Match the old hive query: author:X type:pr is:STATE "ProjectName" in:title -org:ORG
	// The -org: prefix excludes PRs within the org, showing only external outreach PRs.
	// Without the projectName filter, this returns ALL external PRs by the author, inflating the count.
	qualifier := fmt.Sprintf("type:pr author:%s -org:%s \"%s\" in:title", author, org, projectName)
	if state == "merged" {
		qualifier += " is:merged"
	} else {
		qualifier += " is:open"
	}

	result, _, err := c.client.Search.Issues(ctx, qualifier, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil {
		return 0, fmt.Errorf("searching outreach PRs: %w", err)
	}
	return result.GetTotal(), nil
}

const perPage = 100

var shaPattern = regexp.MustCompile(`[0-9a-f]{7,40}\b`)

const shaHoldComment = "Thanks for filing this issue! To help us reproduce and investigate, " +
	"could you please include the **commit SHA** of the build you're running?\n\n" +
	"You can find it by running:\n```\ngit rev-parse --short HEAD\n```\n\n" +
	"Or check the bottom of the console UI for the version string.\n\n" +
	"_This issue will be on hold until a SHA is provided. " +
	"Simply add a comment with the SHA and the hold will be automatically removed._"

type SHAHoldConfig struct {
	PrimaryRepo     string
	AIAuthor        string
	InternalAuthors []string
}

type SHAHoldResult struct {
	Held    int `json:"held"`
	Unheld  int `json:"unheld"`
	Skipped int `json:"skipped"`
}

func (c *Client) EnforceSHAHold(ctx context.Context, cfg SHAHoldConfig) (*SHAHoldResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	result := &SHAHoldResult{}
	owner, repo := c.splitRepo(cfg.PrimaryRepo)

	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{"kind/bug"},
		ListOptions: gh.ListOptions{PerPage: 100},
	}

	issues, _, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
	if err != nil {
		return nil, fmt.Errorf("listing kind/bug issues for %s/%s: %w", owner, repo, err)
	}

	for _, issue := range issues {
		if issue.IsPullRequest() {
			continue
		}

		author := safeGetLogin(issue.GetUser())
		if author == cfg.AIAuthor || isInternalAuthor(author, cfg.InternalAuthors) {
			result.Skipped++
			continue
		}

		labels := extractLabels(issue.Labels)
		held := isHeld(labels)
		hasSHA := shaPattern.MatchString(issue.GetBody())

		if !hasSHA {
			hasSHA = c.checkCommentsForSHA(ctx, owner, repo, issue.GetNumber(), author)
		}

		if hasSHA && held {
			c.unhold(ctx, owner, repo, issue.GetNumber())
			result.Unheld++
			c.logger.Info("SHA-UNHOLD", "repo", repo, "issue", issue.GetNumber(), "author", author)
		} else if !hasSHA && !held {
			c.hold(ctx, owner, repo, issue.GetNumber())
			result.Held++
			c.logger.Info("SHA-HOLD", "repo", repo, "issue", issue.GetNumber(), "author", author)
		} else {
			result.Skipped++
		}
	}

	return result, nil
}

func (c *Client) checkCommentsForSHA(ctx context.Context, owner, repo string, number int, _ string) bool {
	opts := &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	comments, _, err := c.client.Issues.ListComments(ctx, owner, repo, number, opts)
	if err != nil {
		c.logger.Warn("failed to list comments for SHA check", "repo", repo, "issue", number, "error", err)
		return false
	}
	for _, comment := range comments {
		if shaPattern.MatchString(comment.GetBody()) {
			return true
		}
	}
	return false
}

func (c *Client) hold(ctx context.Context, owner, repo string, number int) {
	_, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{"hold"})
	if err != nil {
		c.logger.Warn("failed to add hold label", "repo", repo, "issue", number, "error", err)
	}

	comment := &gh.IssueComment{Body: gh.Ptr(shaHoldComment)}
	_, _, err = c.client.Issues.CreateComment(ctx, owner, repo, number, comment)
	if err != nil {
		c.logger.Warn("failed to post SHA hold comment", "repo", repo, "issue", number, "error", err)
	}
}

func (c *Client) unhold(ctx context.Context, owner, repo string, number int) {
	_, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, "hold")
	if err != nil {
		c.logger.Warn("failed to remove hold label", "repo", repo, "issue", number, "error", err)
	}
}

func isInternalAuthor(author string, internalAuthors []string) bool {
	if strings.HasSuffix(author, "[bot]") {
		return true
	}
	for _, ia := range internalAuthors {
		if strings.EqualFold(author, ia) {
			return true
		}
	}
	return false
}
