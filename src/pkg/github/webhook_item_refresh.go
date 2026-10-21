package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// webhookRefreshRESTCaller tags the single-item reads the stage tracker makes
// after a webhook delivery so /api/gh-rate-limits lists them on their own.
const webhookRefreshRESTCaller = "dashboard:webhook-tracker-refresh"

// ErrAPIBudgetCritical is returned when a discretionary read is refused
// because the core REST budget is in its critical band; callers that only
// refresh a display fall back to the next eval cycle.
var ErrAPIBudgetCritical = errors.New("github api budget critical: discretionary read skipped")

// WebhookItemSnapshot is the display-only state of one PR or issue re-read
// after a webhook named it. It carries exactly the fields the Hive activity
// monitor renders from the status snapshot (title, labels, draft flag,
// last-activity time, open/closed) so the dashboard can patch its row
// without re-enumerating the repository.
type WebhookItemSnapshot struct {
	Repo           string    `json:"repo"`
	Number         int       `json:"number"`
	IsPR           bool      `json:"is_pr"`
	Title          string    `json:"title"`
	Author         string    `json:"author,omitempty"`
	Labels         []string  `json:"labels"`
	Draft          bool      `json:"draft,omitempty"`
	State          string    `json:"state"`
	Merged         bool      `json:"merged,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitzero"`
	URL            string    `json:"url,omitempty"`
	AppAuthored    bool      `json:"app_authored,omitempty"`
	HiveAttributed bool      `json:"hive_attributed,omitempty"`
}

// Open reports whether the item is still open on GitHub.
func (w WebhookItemSnapshot) Open() bool {
	return strings.EqualFold(w.State, "open")
}

// FetchWebhookItemSnapshot re-reads one PR (isPR) or issue with a single
// GET. It goes through the paced REST transport like every other read and
// refuses to spend when the core budget is critical. A fetched PR also
// refreshes the PR detail cache so the next eval cycle's enrichment starts
// from it.
func (c *Client) FetchWebhookItemSnapshot(ctx context.Context, repo string, number int, isPR bool) (WebhookItemSnapshot, error) {
	if c == nil {
		return WebhookItemSnapshot{}, ErrNoGitHubClient
	}
	if mode, _ := c.APIBudgetMode(); mode == APIBudgetCritical {
		return WebhookItemSnapshot{}, ErrAPIBudgetCritical
	}
	owner, repoName := c.splitRepo(repo)
	ctx = WithRESTCaller(ctx, webhookRefreshRESTCaller)
	snap := WebhookItemSnapshot{Repo: repo, Number: number, IsPR: isPR}
	if isPR {
		pr, _, err := c.client.PullRequests.Get(ctx, owner, repoName, number)
		if err != nil {
			return WebhookItemSnapshot{}, fmt.Errorf("fetching %s#%d for webhook refresh: %w", repo, number, err)
		}
		c.storePRDetail(repo, number, pr)
		author := safeGetLogin(pr.GetUser())
		_, hasAttr := ParseAttributionTrailer(pr.GetBody())
		snap.Title = pr.GetTitle()
		snap.Author = author
		snap.Labels = append([]string{}, extractPRLabels(pr.Labels)...)
		snap.Draft = pr.GetDraft()
		snap.State = pr.GetState()
		snap.Merged = pr.GetMerged() || !pr.GetMergedAt().IsZero()
		snap.UpdatedAt = pr.GetUpdatedAt().Time
		snap.URL = pr.GetHTMLURL()
		snap.AppAuthored = c.appBotLogin != "" && strings.EqualFold(author, c.appBotLogin)
		snap.HiveAttributed = hasAttr
		return snap, nil
	}
	issue, _, err := c.client.Issues.Get(ctx, owner, repoName, number)
	if err != nil {
		return WebhookItemSnapshot{}, fmt.Errorf("fetching %s#%d for webhook refresh: %w", repo, number, err)
	}
	snap.IsPR = issue.IsPullRequest()
	snap.Title = issue.GetTitle()
	snap.Author = safeGetLogin(issue.GetUser())
	snap.Labels = append([]string{}, extractLabels(issue.Labels)...)
	snap.State = issue.GetState()
	snap.UpdatedAt = issue.GetUpdatedAt().Time
	snap.URL = issue.GetHTMLURL()
	return snap, nil
}
