package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
)

// webhookRefreshRESTCaller tags the single-item reads the stage tracker makes
// after a webhook delivery so /api/gh-rate-limits lists them on their own.
const webhookRefreshRESTCaller = "dashboard:webhook-tracker-refresh"

// ErrAPIBudgetCritical is returned when a discretionary read is refused
// because the core REST budget is in its critical band; callers that only
// refresh a display fall back to the next eval cycle.
var ErrAPIBudgetCritical = errors.New("github api budget critical: discretionary read skipped")

// ErrRateLimitCaution is returned when a discretionary read is refused
// because the slow-start caution window is engaged after a rate-limit
// refusal: the next eval cycle re-enumerates the item anyway, so the
// targeted GET would only compete with that for paced slots.
var ErrRateLimitCaution = errors.New("github rate-limit caution window engaged: discretionary read skipped")

// WebhookItemSnapshot is the state of one PR or issue re-read after a
// webhook named it. It carries the fields the Hive activity monitor renders
// from the status snapshot (title, labels, draft flag, last-activity time,
// open/closed) plus, in Patch, the classified item for the governor's
// actionable snapshot, so both can be patched without re-enumerating the
// repository.
type WebhookItemSnapshot struct {
	Repo   string   `json:"repo"`
	Number int      `json:"number"`
	IsPR   bool     `json:"is_pr"`
	Title  string   `json:"title"`
	Author string   `json:"author,omitempty"`
	Labels []string `json:"labels"`
	Draft  bool     `json:"draft,omitempty"`
	State  string   `json:"state"`
	Merged bool     `json:"merged,omitempty"`
	// MergedAt and MergedBy are GitHub's merge instant and merger login for
	// a merged PR; MergedByHive is true when that login is the hive's own
	// identity or App bot (the dashboard's merged ledger reads all three).
	MergedAt       time.Time `json:"merged_at,omitzero"`
	MergedBy       string    `json:"merged_by,omitempty"`
	MergedByHive   bool      `json:"merged_by_hive,omitempty"`
	UpdatedAt      time.Time `json:"updated_at,omitzero"`
	URL            string    `json:"url,omitempty"`
	AppAuthored    bool      `json:"app_authored,omitempty"`
	HiveAttributed bool      `json:"hive_attributed,omitempty"`
	// HeadSHA is the PR's head commit at the time of the read (PRs only).
	HeadSHA string `json:"head_sha,omitempty"`
	// FetchedAt is when the GET returned; ApplyWebhookItem records it so an
	// irreversible action can bound how old its view of the item is.
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	// DeliveryID is the X-GitHub-Delivery header of the delivery that asked
	// for the re-read, kept on the patched item for audit. The delivery's
	// payload itself is never copied into the snapshot: the event is a hint
	// to re-read, and this GET is the state.
	DeliveryID string `json:"delivery_id,omitempty"`
	// Patch is the governor-side classification of the same read
	// (webhook_item_patch.go) so the actionable snapshot can be upserted
	// without re-listing the repository.
	Patch WebhookItemPatch `json:"patch,omitzero"`
}

// Open reports whether the item is still open on GitHub.
func (w WebhookItemSnapshot) Open() bool {
	return strings.EqualFold(w.State, "open")
}

// FetchWebhookItemSnapshot re-reads one PR (isPR) or issue with a single
// GET. It goes through the paced REST transport like every other read and
// refuses to spend when the core budget is critical or GitHub is pacing the
// hive after a rate-limit refusal. A fetched PR also
// refreshes the PR detail cache so the next eval cycle's enrichment starts
// from it.
func (c *Client) FetchWebhookItemSnapshot(ctx context.Context, repo string, number int, isPR bool) (WebhookItemSnapshot, error) {
	if c == nil {
		return WebhookItemSnapshot{}, ErrNoGitHubClient
	}
	if mode, _ := c.APIBudgetMode(); mode == APIBudgetCritical {
		return WebhookItemSnapshot{}, ErrAPIBudgetCritical
	}
	if RateLimitCautionEngaged() {
		return WebhookItemSnapshot{}, ErrRateLimitCaution
	}
	owner, repoName := c.splitRepo(repo)
	ctx = WithRESTCaller(ctx, webhookRefreshRESTCaller)
	now := time.Now()
	deliveryID := WebhookDeliveryFrom(ctx)
	// The same GET feeds the attached state cache (forgestate_reader.go):
	// a delivery-prompted read is a webhook patch, any other a read-through.
	observeSource := forgestate.SourceReadThrough
	if deliveryID != "" {
		observeSource = forgestate.SourceWebhook
	}
	snap := WebhookItemSnapshot{Repo: repo, Number: number, IsPR: isPR, FetchedAt: now, DeliveryID: deliveryID}
	if isPR {
		pr, _, err := c.client.PullRequests.Get(ctx, owner, repoName, number)
		if err != nil {
			return WebhookItemSnapshot{}, fmt.Errorf("fetching %s#%d for webhook refresh: %w", repo, number, err)
		}
		c.storePRDetail(repo, number, pr)
		c.forgeObserve(repo, forgePRItem(pr), observeSource, deliveryID)
		author := safeGetLogin(pr.GetUser())
		_, hasAttr := ParseAttributionTrailer(pr.GetBody())
		labels := append([]string{}, extractPRLabels(pr.Labels)...)
		snap.Title = pr.GetTitle()
		snap.Author = author
		snap.Labels = labels
		snap.Draft = pr.GetDraft()
		snap.State = pr.GetState()
		snap.Merged = pr.GetMerged() || !pr.GetMergedAt().IsZero()
		if snap.Merged {
			snap.MergedAt = pr.GetMergedAt().Time
			snap.MergedBy = safeGetLogin(pr.GetMergedBy())
			snap.MergedByHive = prTerminalMergeAttribution(snap.MergedBy, c.getHiveIdentity(), c.appBotLogin) == PRMergeAttributionHive
		}
		snap.UpdatedAt = pr.GetUpdatedAt().Time
		snap.URL = pr.GetHTMLURL()
		snap.HeadSHA = prHeadSHA(pr)
		snap.AppAuthored = c.appBotLogin != "" && strings.EqualFold(author, c.appBotLogin)
		snap.HiveAttributed = hasAttr
		if snap.Open() && !snap.Merged {
			snap.Patch = c.classifyWebhookPR(repo, pr, labels, now)
		}
		return snap, nil
	}
	issue, blockedBy, children, err := c.getIssueWithBlockedCount(ctx, owner, repoName, number)
	if err != nil {
		return WebhookItemSnapshot{}, fmt.Errorf("fetching %s#%d for webhook refresh: %w", repo, number, err)
	}
	snap.IsPR = issue.IsPullRequest()
	if !snap.IsPR {
		c.forgeObserve(repo, forgeIssueItem(issue, blockedBy, children), observeSource, deliveryID)
	}
	snap.Title = issue.GetTitle()
	snap.Author = safeGetLogin(issue.GetUser())
	labels := append([]string{}, extractLabels(issue.Labels)...)
	snap.Labels = labels
	snap.State = issue.GetState()
	snap.UpdatedAt = issue.GetUpdatedAt().Time
	snap.URL = issue.GetHTMLURL()
	// An issue number that turned out to be a PR is left unclassified: the
	// issue payload lacks the PR fields, and the PR-side delivery for it
	// carries its own re-read.
	if snap.Open() && !snap.IsPR {
		snap.Patch = c.classifyWebhookIssue(repo, issue, labels, now)
	}
	return snap, nil
}
