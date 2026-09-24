package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	githubActivityDefaultAPIURL       = "https://api.github.com"
	githubActivityDefaultPollInterval = 5 * time.Minute
	githubActivityStateFile           = "github-activity-feed.json"
	githubActivityHTTPTimeout         = 20 * time.Second
)

type GitHubActivityOptions struct {
	Org              string
	APIURL           string
	Token            string
	WebhookURL       string
	DataDir          string
	PollInterval     time.Duration
	Repos            []string
	Events           []string
	AllowAuthors     []string
	DenyAuthors      []string
	FilterBots       bool
	FilterDependabot bool
}

var githubActivityPollMu sync.Mutex

type GitHubActivityFeed struct {
	opts      GitHubActivityOptions
	client    *http.Client
	logger    *slog.Logger
	statePath string
}

func (s *HubServer) SetGitHubActivityFeed(feed *GitHubActivityFeed) {
	if s == nil {
		return
	}
	s.githubActivityMu.Lock()
	defer s.githubActivityMu.Unlock()
	if s.githubActivityCancel != nil {
		s.githubActivityCancel()
		s.githubActivityCancel = nil
	}
	s.githubActivityFeed = feed
	if feed != nil && s.githubActivityCtx != nil {
		ctx, cancel := context.WithCancel(s.githubActivityCtx)
		s.githubActivityCancel = cancel
		go feed.Run(ctx)
	}
}

func NewGitHubActivityFeed(opts GitHubActivityOptions, logger *slog.Logger) *GitHubActivityFeed {
	if opts.APIURL == "" {
		opts.APIURL = githubActivityDefaultAPIURL
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = githubActivityDefaultPollInterval
	}
	if opts.DataDir == "" {
		opts.DataDir = "/data"
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &GitHubActivityFeed{
		opts: opts,
		client: &http.Client{
			Timeout: githubActivityHTTPTimeout,
		},
		logger:    logger,
		statePath: filepath.Join(opts.DataDir, githubActivityStateFile),
	}
}

func (f *GitHubActivityFeed) Run(ctx context.Context) {
	if f == nil {
		return
	}
	f.pollAndLog(ctx)
	ticker := time.NewTicker(f.opts.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			f.pollAndLog(ctx)
		}
	}
}

func (f *GitHubActivityFeed) PollOnce(ctx context.Context) ([]GitHubActivityEvent, error) {
	if f == nil {
		return nil, nil
	}
	githubActivityPollMu.Lock()
	defer githubActivityPollMu.Unlock()
	state, err := f.loadState()
	if err != nil {
		return nil, err
	}
	current, err := f.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	events := f.diff(state, current)
	for _, event := range events {
		if err := f.postDiscord(ctx, event.Content); err != nil {
			return events, err
		}
		state.Sent[event.Key] = true
		if err := f.saveState(state); err != nil {
			return events, err
		}
	}
	state.Repos = current.Repos
	if err := f.saveState(state); err != nil {
		return events, err
	}
	return events, nil
}

func (f *GitHubActivityFeed) pollAndLog(ctx context.Context) {
	events, err := f.PollOnce(ctx)
	if err != nil {
		f.logger.Warn("github activity poll failed", "error", err)
		return
	}
	if len(events) > 0 {
		f.logger.Info("github activity notifications sent", "count", len(events))
	}
}

type GitHubActivityEvent struct {
	Key     string
	Content string
}

type githubActivityState struct {
	Repos map[string]githubRepoSnapshot `json:"repos"`
	Sent  map[string]bool               `json:"sent"`
}

type githubRepoSnapshot struct {
	Issues map[int]githubIssueSnapshot `json:"issues"`
	PRs    map[int]githubPRSnapshot    `json:"prs"`
}

type githubIssueSnapshot struct {
	Number      int      `json:"number"`
	Title       string   `json:"title"`
	Author      string   `json:"author"`
	State       string   `json:"state"`
	StateReason string   `json:"state_reason,omitempty"`
	URL         string   `json:"url"`
	UpdatedAt   string   `json:"updated_at"`
	Labels      []string `json:"labels,omitempty"`
	Assignees   []string `json:"assignees,omitempty"`
	ClaimMarker bool     `json:"claim_marker,omitempty"`
}

type githubPRSnapshot struct {
	Number             int      `json:"number"`
	Title              string   `json:"title"`
	Author             string   `json:"author"`
	State              string   `json:"state"`
	URL                string   `json:"url"`
	UpdatedAt          string   `json:"updated_at"`
	HeadSHA            string   `json:"head_sha"`
	MergeCommitSHA     string   `json:"merge_commit_sha,omitempty"`
	BaseRef            string   `json:"base_ref"`
	Draft              bool     `json:"draft,omitempty"`
	Merged             bool     `json:"merged,omitempty"`
	ReviewDecision     string   `json:"review_decision,omitempty"`
	RequestedReviewers []string `json:"requested_reviewers,omitempty"`
	CheckState         string   `json:"check_state,omitempty"`
}

func (f *GitHubActivityFeed) loadState() (githubActivityState, error) {
	state := githubActivityState{Repos: map[string]githubRepoSnapshot{}, Sent: map[string]bool{}}
	data, err := os.ReadFile(f.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return state, nil
		}
		return state, fmt.Errorf("read github activity state: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("parse github activity state: %w", err)
	}
	if state.Repos == nil {
		state.Repos = map[string]githubRepoSnapshot{}
	}
	if state.Sent == nil {
		state.Sent = map[string]bool{}
	}
	return state, nil
}

func (f *GitHubActivityFeed) saveState(state githubActivityState) error {
	if err := os.MkdirAll(filepath.Dir(f.statePath), 0o755); err != nil {
		return fmt.Errorf("create github activity state dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal github activity state: %w", err)
	}
	if err := os.WriteFile(f.statePath, data, 0o600); err != nil {
		return fmt.Errorf("write github activity state: %w", err)
	}
	return nil
}

func (f *GitHubActivityFeed) snapshot(ctx context.Context) (githubActivityState, error) {
	repos, err := f.listRepos(ctx)
	if err != nil {
		return githubActivityState{}, err
	}
	state := githubActivityState{Repos: map[string]githubRepoSnapshot{}, Sent: map[string]bool{}}
	for _, repo := range repos {
		issues, err := f.listIssues(ctx, repo)
		if err != nil {
			return state, err
		}
		prs, err := f.listPRs(ctx, repo)
		if err != nil {
			return state, err
		}
		state.Repos[repo] = githubRepoSnapshot{Issues: issues, PRs: prs}
	}
	return state, nil
}

type apiRepo struct {
	Name string `json:"name"`
}

func (f *GitHubActivityFeed) listRepos(ctx context.Context) ([]string, error) {
	if len(f.opts.Repos) > 0 {
		out := append([]string(nil), f.opts.Repos...)
		sort.Strings(out)
		return out, nil
	}
	var out []string
	for page := 1; ; page++ {
		var repos []apiRepo
		if err := f.getJSON(ctx, fmt.Sprintf("/orgs/%s/repos?per_page=100&page=%d", url.PathEscape(f.opts.Org), page), &repos); err != nil {
			return nil, err
		}
		for _, repo := range repos {
			if strings.TrimSpace(repo.Name) != "" {
				out = append(out, repo.Name)
			}
		}
		if len(repos) < 100 {
			break
		}
	}
	sort.Strings(out)
	return out, nil
}

type apiIssue struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	State       string `json:"state"`
	StateReason string `json:"state_reason"`
	HTMLURL     string `json:"html_url"`
	UpdatedAt   string `json:"updated_at"`
	Body        string `json:"body"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Assignees []struct {
		Login string `json:"login"`
	} `json:"assignees"`
	PullRequest *struct{} `json:"pull_request"`
}

func (f *GitHubActivityFeed) listIssues(ctx context.Context, repo string) (map[int]githubIssueSnapshot, error) {
	out := map[int]githubIssueSnapshot{}
	for page := 1; ; page++ {
		var issues []apiIssue
		if err := f.getJSON(ctx, fmt.Sprintf("/repos/%s/%s/issues?state=all&per_page=100&page=%d", url.PathEscape(f.opts.Org), url.PathEscape(repo), page), &issues); err != nil {
			return nil, err
		}
		for _, issue := range issues {
			if issue.PullRequest != nil {
				continue
			}
			labels := make([]string, 0, len(issue.Labels))
			for _, label := range issue.Labels {
				labels = append(labels, label.Name)
			}
			assignees := make([]string, 0, len(issue.Assignees))
			for _, assignee := range issue.Assignees {
				assignees = append(assignees, assignee.Login)
			}
			sort.Strings(labels)
			sort.Strings(assignees)
			out[issue.Number] = githubIssueSnapshot{
				Number:      issue.Number,
				Title:       issue.Title,
				Author:      issue.User.Login,
				State:       issue.State,
				StateReason: issue.StateReason,
				URL:         issue.HTMLURL,
				UpdatedAt:   issue.UpdatedAt,
				Labels:      labels,
				Assignees:   assignees,
				ClaimMarker: strings.Contains(issue.Body, "<!-- hive-claim -->"),
			}
		}
		if len(issues) < 100 {
			break
		}
	}
	return out, nil
}

type apiPullRequest struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	State          string `json:"state"`
	HTMLURL        string `json:"html_url"`
	UpdatedAt      string `json:"updated_at"`
	Draft          bool   `json:"draft"`
	MergedAt       string `json:"merged_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	User           struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	RequestedReviewers []struct {
		Login string `json:"login"`
	} `json:"requested_reviewers"`
}

func (f *GitHubActivityFeed) listPRs(ctx context.Context, repo string) (map[int]githubPRSnapshot, error) {
	out := map[int]githubPRSnapshot{}
	for page := 1; ; page++ {
		var prs []apiPullRequest
		if err := f.getJSON(ctx, fmt.Sprintf("/repos/%s/%s/pulls?state=all&per_page=100&page=%d", url.PathEscape(f.opts.Org), url.PathEscape(repo), page), &prs); err != nil {
			return nil, err
		}
		for _, pr := range prs {
			reviewers := make([]string, 0, len(pr.RequestedReviewers))
			for _, reviewer := range pr.RequestedReviewers {
				reviewers = append(reviewers, reviewer.Login)
			}
			sort.Strings(reviewers)
			checkState := ""
			if pr.Head.SHA != "" && pr.State == "open" {
				checkState = f.checkState(ctx, repo, pr.Head.SHA)
			}
			reviewDecision := ""
			if pr.State == "open" {
				reviewDecision = f.reviewDecision(ctx, repo, pr.Number)
			}
			out[pr.Number] = githubPRSnapshot{
				Number:             pr.Number,
				Title:              pr.Title,
				Author:             pr.User.Login,
				State:              pr.State,
				URL:                pr.HTMLURL,
				UpdatedAt:          pr.UpdatedAt,
				HeadSHA:            pr.Head.SHA,
				MergeCommitSHA:     pr.MergeCommitSHA,
				BaseRef:            pr.Base.Ref,
				Draft:              pr.Draft,
				Merged:             pr.MergedAt != "",
				RequestedReviewers: reviewers,
				ReviewDecision:     reviewDecision,
				CheckState:         checkState,
			}
		}
		if len(prs) < 100 {
			break
		}
	}
	return out, nil
}

func (f *GitHubActivityFeed) checkState(ctx context.Context, repo, sha string) string {
	var status struct {
		State string `json:"state"`
	}
	err := f.getJSON(ctx, fmt.Sprintf("/repos/%s/%s/commits/%s/status", url.PathEscape(f.opts.Org), url.PathEscape(repo), url.PathEscape(sha)), &status)
	if err != nil {
		f.logger.Debug("github activity check state unavailable", "repo", repo, "sha", sha, "error", err)
		return ""
	}
	return strings.ToLower(status.State)
}

func (f *GitHubActivityFeed) reviewDecision(ctx context.Context, repo string, number int) string {
	var reviews []struct {
		State       string `json:"state"`
		SubmittedAt string `json:"submitted_at"`
	}
	err := f.getJSON(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", url.PathEscape(f.opts.Org), url.PathEscape(repo), number), &reviews)
	if err != nil {
		f.logger.Debug("github activity review state unavailable", "repo", repo, "number", number, "error", err)
		return ""
	}
	for i := len(reviews) - 1; i >= 0; i-- {
		switch strings.ToLower(reviews[i].State) {
		case "approved":
			return "approved"
		case "changes_requested":
			return "changes_requested"
		}
	}
	return ""
}

func (f *GitHubActivityFeed) getJSON(ctx context.Context, apiPath string, out any) error {
	base := strings.TrimRight(f.opts.APIURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+apiPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if f.opts.Token != "" {
		req.Header.Set("Authorization", "Bearer "+f.opts.Token)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github activity GET %s: status %d: %s", apiPath, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("github activity decode %s: %w", apiPath, err)
	}
	return nil
}

func (f *GitHubActivityFeed) diff(previous, current githubActivityState) []GitHubActivityEvent {
	events := []GitHubActivityEvent{}
	for repo, currentRepo := range current.Repos {
		previousRepo, repoSeen := previous.Repos[repo]
		events = append(events, f.diffIssues(previous.Sent, repo, repoSeen, previousRepo.Issues, currentRepo.Issues)...)
		events = append(events, f.diffPRs(previous.Sent, repo, repoSeen, previousRepo.PRs, currentRepo.PRs)...)
	}
	return events
}

func (f *GitHubActivityFeed) diffIssues(sent map[string]bool, repo string, repoSeen bool, previous, current map[int]githubIssueSnapshot) []GitHubActivityEvent {
	var events []GitHubActivityEvent
	for number, issue := range current {
		prev, seen := previous[number]
		if !seen {
			if repoSeen && !f.authorSuppressed(issue.Author, false, "issue_opened") {
				if issue.State == "open" && f.eventAllowed("issue_opened") {
					events = append(events, f.issueEvent(sent, repo, issue, "issue_opened", issue.UpdatedAt, "Issue", "opened")...)
				} else if issue.State == "closed" && f.eventAllowed("issue_closed") {
					reason := issueClosedReason(issue.StateReason)
					events = append(events, f.issueEvent(sent, repo, issue, "issue_closed", issue.UpdatedAt, "Issue", reason)...)
				}
			}
			continue
		}
		if f.authorSuppressed(issue.Author, false, "") {
			continue
		}
		if prev.State != issue.State {
			switch issue.State {
			case "open":
				if f.eventAllowed("issue_reopened") {
					events = append(events, f.issueEvent(sent, repo, issue, "issue_reopened", issue.UpdatedAt, "Issue", "reopened")...)
				}
			case "closed":
				if f.eventAllowed("issue_closed") {
					events = append(events, f.issueEvent(sent, repo, issue, "issue_closed", issue.UpdatedAt, "Issue", issueClosedReason(issue.StateReason))...)
				}
			}
		}
		for _, label := range addedStrings(prev.Labels, issue.Labels) {
			if isHiveActivityLabel(label) && f.eventAllowed("issue_claimed") {
				events = append(events, f.issueEvent(sent, repo, issue, "issue_labeled_"+label, issue.UpdatedAt, "Issue", "labeled "+label)...)
			}
		}
		for _, assignee := range addedStrings(prev.Assignees, issue.Assignees) {
			if f.eventAllowed("issue_claimed") {
				events = append(events, f.issueEvent(sent, repo, issue, "issue_assigned_"+assignee, issue.UpdatedAt, "Issue", "claimed by @"+assignee)...)
			}
		}
		if prev.ClaimMarker != issue.ClaimMarker {
			action := "claim marker released"
			eventName := "issue_released"
			if issue.ClaimMarker {
				action = "claim marker taken"
				eventName = "issue_claimed"
			}
			if f.eventAllowed(eventName) {
				events = append(events, f.issueEvent(sent, repo, issue, "issue_claim_marker", issue.UpdatedAt, "Issue", action)...)
			}
		}
	}
	return events
}

func (f *GitHubActivityFeed) diffPRs(sent map[string]bool, repo string, repoSeen bool, previous, current map[int]githubPRSnapshot) []GitHubActivityEvent {
	var events []GitHubActivityEvent
	for number, pr := range current {
		prev, seen := previous[number]
		if !seen {
			if repoSeen {
				forward := isForwardMergePR(pr.Title)
				if !f.authorSuppressed(pr.Author, forward, "pr_merged") && f.eventAllowed("pr_merged") && pr.Merged {
					events = append(events, f.prEvent(sent, repo, pr, "pr_merged", mergeEventSHA(pr), "merged into "+pr.BaseRef)...)
				} else if !f.authorSuppressed(pr.Author, forward, "pr_closed_unmerged") && f.eventAllowed("pr_closed_unmerged") && pr.State == "closed" {
					events = append(events, f.prEvent(sent, repo, pr, "pr_closed_unmerged", pr.HeadSHA, "closed unmerged")...)
				} else if !f.authorSuppressed(pr.Author, forward, "pr_opened") && f.eventAllowed("pr_opened") && pr.State == "open" && !pr.Draft {
					events = append(events, f.prEvent(sent, repo, pr, "pr_opened", pr.HeadSHA, "opened")...)
				}
			}
			continue
		}
		forward := isForwardMergePR(pr.Title)
		if f.authorSuppressed(pr.Author, forward, "pr_"+prTerminalEvent(pr)) {
			continue
		}
		if forward && !(pr.Merged && !prev.Merged) {
			continue
		}
		if !prev.Merged && pr.Merged && f.eventAllowed("pr_merged") {
			events = append(events, f.prEvent(sent, repo, pr, "pr_merged", mergeEventSHA(pr), "merged into "+pr.BaseRef)...)
			continue
		}
		if prev.State == "open" && pr.State == "closed" && !pr.Merged && f.eventAllowed("pr_closed_unmerged") {
			events = append(events, f.prEvent(sent, repo, pr, "pr_closed_unmerged", pr.HeadSHA, "closed unmerged")...)
		}
		if prev.Draft && !pr.Draft && pr.State == "open" && f.eventAllowed("pr_ready_for_review") {
			events = append(events, f.prEvent(sent, repo, pr, "pr_ready_for_review", pr.HeadSHA, "ready for review")...)
		}
		for _, reviewer := range addedStrings(prev.RequestedReviewers, pr.RequestedReviewers) {
			if f.eventAllowed("pr_review") {
				events = append(events, f.prEvent(sent, repo, pr, "pr_review_requested_"+reviewer, pr.HeadSHA, "review requested from @"+reviewer)...)
			}
		}
		if prev.ReviewDecision != pr.ReviewDecision {
			switch pr.ReviewDecision {
			case "approved":
				if f.eventAllowed("pr_review") {
					events = append(events, f.prEvent(sent, repo, pr, "pr_approved", pr.HeadSHA, "approved")...)
				}
			case "changes_requested":
				if f.eventAllowed("pr_review") {
					events = append(events, f.prEvent(sent, repo, pr, "pr_changes_requested", pr.HeadSHA, "changes requested")...)
				}
			}
		}
		if prev.CheckState != pr.CheckState && isTerminalCheckState(prev.CheckState) && isTerminalCheckState(pr.CheckState) && f.eventAllowed("pr_ci_status") {
			action := "CI went " + pr.CheckState
			events = append(events, f.prEvent(sent, repo, pr, "pr_ci_"+pr.CheckState, pr.HeadSHA, action)...)
		}
	}
	return events
}

func (f *GitHubActivityFeed) eventAllowed(event string) bool {
	if len(f.opts.Events) == 0 {
		return true
	}
	for _, allowed := range f.opts.Events {
		allowed = strings.TrimSpace(allowed)
		if allowed == event {
			return true
		}
		switch allowed {
		case "issue_claimed":
			if strings.HasPrefix(event, "issue_labeled_") || strings.HasPrefix(event, "issue_assigned_") {
				return true
			}
		case "pr_review":
			if strings.HasPrefix(event, "pr_review_requested_") || event == "pr_approved" || event == "pr_changes_requested" {
				return true
			}
		case "pr_ci_status":
			if strings.HasPrefix(event, "pr_ci_") {
				return true
			}
		case "pr_closed":
			if event == "pr_closed_unmerged" {
				return true
			}
		}
	}
	return false
}

func (f *GitHubActivityFeed) issueEvent(sent map[string]bool, repo string, issue githubIssueSnapshot, event, sha, kind, action string) []GitHubActivityEvent {
	if f.authorSuppressed(issue.Author, false, event) {
		return nil
	}
	key := activityKey(repo, issue.Number, event, sha)
	if sent[key] {
		return nil
	}
	repoName := path.Base(repo)
	content := fmt.Sprintf("🐝 [%s] %s #%d %s — %s · @%s", repoName, kind, issue.Number, action, issue.Title, issue.Author)
	if issue.URL != "" {
		content += " <" + issue.URL + ">"
	}
	return []GitHubActivityEvent{{Key: key, Content: content}}
}

func (f *GitHubActivityFeed) prEvent(sent map[string]bool, repo string, pr githubPRSnapshot, event, sha, action string) []GitHubActivityEvent {
	key := activityKey(repo, pr.Number, event, sha)
	if sent[key] {
		return nil
	}
	repoName := path.Base(repo)
	content := fmt.Sprintf("🐝 [%s] PR #%d %s — %s · @%s", repoName, pr.Number, action, pr.Title, pr.Author)
	if pr.URL != "" {
		content += " <" + pr.URL + ">"
	}
	return []GitHubActivityEvent{{Key: key, Content: content}}
}

func (f *GitHubActivityFeed) authorSuppressed(author string, forwardMerge bool, event string) bool {
	login := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(author), "@"))
	if login == "" {
		return false
	}
	if containsLogin(f.opts.DenyAuthors, login) {
		return true
	}
	if containsLogin(f.opts.AllowAuthors, login) {
		return false
	}
	if forwardMerge && event == "pr_merged" {
		return false
	}
	if f.opts.FilterDependabot && (login == "dependabot" || login == "dependabot[bot]") {
		return true
	}
	return f.opts.FilterBots && strings.HasSuffix(login, "[bot]")
}

// discordSuppressEmbeds is Discord's SUPPRESS_EMBEDS message flag (1<<2):
// factory lines already carry the title, so the link preview card is noise.
// URLs are additionally wrapped in <> so clients that ignore flags still
// render them bare. Mirrors notify.DiscordSuppressEmbeds.
const discordSuppressEmbeds = 1 << 2

func (f *GitHubActivityFeed) postDiscord(ctx context.Context, content string) error {
	payload, err := json.Marshal(map[string]any{"content": content, "flags": discordSuppressEmbeds})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.opts.WebhookURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("discord factory webhook returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func activityKey(repo string, number int, event, sha string) string {
	return fmt.Sprintf("%s|%d|%s|%s", repo, number, event, sha)
}

func mergeEventSHA(pr githubPRSnapshot) string {
	if pr.MergeCommitSHA != "" {
		return pr.MergeCommitSHA
	}
	if pr.HeadSHA != "" {
		return pr.HeadSHA
	}
	return pr.UpdatedAt
}

func issueClosedReason(reason string) string {
	switch reason {
	case "not_planned":
		return "closed as not planned"
	case "completed", "":
		return "closed as completed"
	default:
		return "closed"
	}
}

func prTerminalEvent(pr githubPRSnapshot) string {
	if pr.Merged {
		return "merged"
	}
	if pr.State == "closed" {
		return "closed_unmerged"
	}
	return "updated"
}

func isHiveActivityLabel(label string) bool {
	lower := strings.ToLower(strings.TrimSpace(label))
	return lower == "claimed" || strings.HasPrefix(lower, "hive/") || strings.HasPrefix(lower, "priority/")
}

func addedStrings(previous, current []string) []string {
	seen := map[string]bool{}
	for _, item := range previous {
		seen[strings.ToLower(item)] = true
	}
	var added []string
	for _, item := range current {
		if !seen[strings.ToLower(item)] {
			added = append(added, item)
		}
	}
	sort.Strings(added)
	return added
}

func containsLogin(list []string, login string) bool {
	for _, item := range list {
		if strings.ToLower(strings.TrimPrefix(strings.TrimSpace(item), "@")) == login {
			return true
		}
	}
	return false
}

func isForwardMergePR(title string) bool {
	lower := strings.ToLower(title)
	return strings.Contains(lower, "forward-merge") || strings.Contains(lower, "forward merge")
}

func isTerminalCheckState(state string) bool {
	switch strings.ToLower(state) {
	case "success", "failure", "error":
		return true
	default:
		return false
	}
}
