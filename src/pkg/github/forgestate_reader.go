package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/forgestate"
)

// ForgeState as the governor's work source (hivecommons/hive#13375, Phase
// 2a of #13373).
//
// With a forgestate.Store attached (SetForgeState), EnumerateActionable no
// longer lists every repository: the per-repo open issue and PR listings
// come from the cache, which the store's reconcile (the only full list),
// the webhook re-reads (Observe, one GET per delivery) and the hive's own
// writes (write-through) keep current. The classification that turns a
// listing into the actionable set — hold labels, issue filters, reporter
// trust, SLA, clusters — is unchanged and still runs on every pass; only
// the list calls are gone. Readers that need the full payload take it from
// Item.Raw, so the cache carries what the governor needs without a second
// read.
//
// Accuracy rules the reader enforces:
//
//   - a repo whose cache is not Ready (no successful reconcile yet) is a
//     transient failure for the pass, exactly like a refused list: the
//     repo's last-good enumeration is served if there is one, and when
//     every repo is unready the pass errors so the caller keeps its prior
//     state. cmd/hive additionally waits for the store before the first
//     pass (the startup gate);
//   - a Stale cache (the last reconcile failed) is used but the repo is
//     listed in ActionableResult.StaleRepos with StaleSince = the last good
//     reconcile, so irreversible actions refuse it (SnapshotFreshnessGuard);
//   - every item records its cache fetch time in ItemPatches, so the
//     freshness bound before a close, merge or kick measures from when the
//     item was really read, not from when the pass ran.

// forgestateReconcileRESTCaller tags the store's list calls in the REST
// accounting.
const forgestateReconcileRESTCaller = "hive:forgestate_reconcile"

// ErrForgeStateNotReady is the per-repo enumeration error while the repo's
// cache has not completed its first successful reconcile. It is transient:
// the last-good path applies.
var ErrForgeStateNotReady = errors.New("forgestate: repo cache not ready (no successful reconcile yet)")

// SetForgeState attaches (or with nil detaches) the per-repo state cache
// EnumerateActionable reads listings from. Safe to call at any time; the
// next pass uses it.
func (c *Client) SetForgeState(s *forgestate.Store) {
	if c == nil {
		return
	}
	c.forgeMu.Lock()
	c.forge = s
	c.forgeMu.Unlock()
}

// ForgeState returns the attached cache, nil when the client lists GitHub
// directly.
func (c *Client) ForgeState() *forgestate.Store {
	if c == nil {
		return nil
	}
	c.forgeMu.RLock()
	defer c.forgeMu.RUnlock()
	return c.forge
}

// forgeIssuePayload is what the GitHub fetcher stores in Item.Raw for an
// issue: the go-github issue plus the dependency and sub-issue summaries
// the list payload carries and go-github drops (issue_dependencies.go).
type forgeIssuePayload struct {
	Issue     *gh.Issue
	BlockedBy int
	Children  *IssueChildren
}

// forgeIssueItem converts one listed or fetched issue into a cache item.
func forgeIssueItem(issue *gh.Issue, blockedBy int, children *IssueChildren) forgestate.Item {
	return forgestate.Item{
		Kind:      forgestate.KindIssue,
		Number:    issue.GetNumber(),
		State:     issue.GetState(),
		Labels:    extractLabels(issue.Labels),
		Assignees: extractAssignees(issue.Assignees),
		UpdatedAt: issue.GetUpdatedAt().Time,
		Closed:    strings.EqualFold(issue.GetState(), "closed"),
		Raw:       &forgeIssuePayload{Issue: issue, BlockedBy: blockedBy, Children: children},
	}
}

// forgePRItem converts one listed or fetched pull request into a cache item.
func forgePRItem(pr *gh.PullRequest) forgestate.Item {
	return forgestate.Item{
		Kind:           forgestate.KindPR,
		Number:         pr.GetNumber(),
		State:          pr.GetState(),
		Labels:         extractPRLabels(pr.Labels),
		Assignees:      extractAssignees(pr.Assignees),
		UpdatedAt:      pr.GetUpdatedAt().Time,
		Draft:          pr.GetDraft(),
		Closed:         strings.EqualFold(pr.GetState(), "closed"),
		HeadSHA:        prHeadSHA(pr),
		Merged:         pr.GetMerged() || !pr.GetMergedAt().Time.IsZero(),
		MergeableState: pr.GetMergeableState(),
		Raw:            pr,
	}
}

// issueFromForgeItem rebuilds the go-github issue the classifier expects
// from a cache item. The item's typed Labels, Assignees and State overlay
// the raw payload: a write-through (stage label added, claim) updates those
// fields and must be what the next pass sees. ok is false when the item
// carries no usable payload (it did not come from this fetcher).
func issueFromForgeItem(it forgestate.Item) (issue *gh.Issue, blockedBy int, children *IssueChildren, ok bool) {
	payload, isPayload := it.Raw.(*forgeIssuePayload)
	if !isPayload || payload == nil || payload.Issue == nil {
		return nil, 0, nil, false
	}
	copied := *payload.Issue
	copied.Labels = labelsFromNames(it.Labels)
	copied.Assignees = usersFromLogins(it.Assignees)
	if it.State != "" {
		copied.State = gh.Ptr(it.State)
	}
	return &copied, payload.BlockedBy, payload.Children, true
}

// prFromForgeItem is issueFromForgeItem for pull requests.
func prFromForgeItem(it forgestate.Item) (*gh.PullRequest, bool) {
	pr, isPR := it.Raw.(*gh.PullRequest)
	if !isPR || pr == nil {
		return nil, false
	}
	copied := *pr
	copied.Labels = labelsFromNames(it.Labels)
	copied.Assignees = usersFromLogins(it.Assignees)
	if it.State != "" {
		copied.State = gh.Ptr(it.State)
	}
	return &copied, true
}

func labelsFromNames(names []string) []*gh.Label {
	out := make([]*gh.Label, 0, len(names))
	for _, n := range names {
		out = append(out, &gh.Label{Name: gh.Ptr(n)})
	}
	return out
}

func usersFromLogins(logins []string) []*gh.User {
	out := make([]*gh.User, 0, len(logins))
	for _, l := range logins {
		out = append(out, &gh.User{Login: gh.Ptr(l)})
	}
	return out
}

// ForgeStateFetcher is the forgestate.Fetcher over the hive's GitHub
// client. Current returns the client to use on each call, so a client
// rebuilt on new App credentials (cmd/hive adoptGitHubClient) is picked up
// without rebuilding the store. The listings are the same calls
// EnumerateActionable made directly before the cache existed — the issue
// list keeps GitHub's dependency and sub-issue summaries — and they run at
// the reconcile pass's pacing priority through the paced, ETag-caching
// transport.
type ForgeStateFetcher struct {
	Current func() *Client
}

func (f ForgeStateFetcher) client() (*Client, error) {
	if f.Current == nil {
		return nil, ErrNoGitHubClient
	}
	c := f.Current()
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	return c, nil
}

// List implements forgestate.Fetcher. Conditional requests happen in the
// client's transport, so etag is not re-sent and NotModified is never
// reported; a failed page is an error, never a short list.
func (f ForgeStateFetcher) List(ctx context.Context, repo, _ string) (forgestate.ListResult, error) {
	c, err := f.client()
	if err != nil {
		return forgestate.ListResult{}, err
	}
	ctx = WithRESTCaller(WithPacingClass(ctx, PacingHigh), forgestateReconcileRESTCaller)
	owner, repoName := c.splitRepo(repo)
	issues, blocked, children, err := c.listOpenIssuesWithBlockedCounts(ctx, owner, repoName)
	if err != nil {
		return forgestate.ListResult{}, fmt.Errorf("listing issues for %s/%s: %w", owner, repoName, err)
	}
	var out forgestate.ListResult
	for _, issue := range issues {
		if issue == nil || issue.IsPullRequest() {
			continue
		}
		var kids *IssueChildren
		if ch, ok := children[issue.GetNumber()]; ok {
			k := ch
			kids = &k
		}
		out.Issues = append(out.Issues, forgeIssueItem(issue, blocked[issue.GetNumber()], kids))
	}
	prs, err := c.listOpenPRs(ctx, owner, repoName)
	if err != nil {
		return forgestate.ListResult{}, err
	}
	for _, pr := range prs {
		if pr == nil {
			continue
		}
		out.PRs = append(out.PRs, forgePRItem(pr))
	}
	return out, nil
}

// Issue implements forgestate.Fetcher.
func (f ForgeStateFetcher) Issue(ctx context.Context, repo string, number int) (forgestate.Item, error) {
	c, err := f.client()
	if err != nil {
		return forgestate.Item{}, err
	}
	owner, repoName := c.splitRepo(repo)
	issue, blocked, children, err := c.getIssueWithBlockedCount(ctx, owner, repoName, number)
	if err != nil {
		return forgestate.Item{}, forgeNotFound(err)
	}
	return forgeIssueItem(issue, blocked, children), nil
}

// PR implements forgestate.Fetcher.
func (f ForgeStateFetcher) PR(ctx context.Context, repo string, number int) (forgestate.Item, error) {
	c, err := f.client()
	if err != nil {
		return forgestate.Item{}, err
	}
	owner, repoName := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(ctx, owner, repoName, number)
	if err != nil {
		return forgestate.Item{}, forgeNotFound(err)
	}
	return forgePRItem(pr), nil
}

// forgeNotFound wraps forgestate.ErrNotFound around a 404/410 so the store
// drops the item.
func forgeNotFound(err error) error {
	if githubStatusError(err, http.StatusNotFound) || githubStatusError(err, http.StatusGone) {
		return fmt.Errorf("%w: %w", forgestate.ErrNotFound, err)
	}
	return err
}

// listOpenPRs pages through a repo's open pull requests.
func (c *Client) listOpenPRs(ctx context.Context, owner, repoName string) ([]*gh.PullRequest, error) {
	opts := &gh.PullRequestListOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var all []*gh.PullRequest
	for {
		prs, resp, err := c.client.PullRequests.List(ctx, owner, repoName, opts)
		if err != nil {
			return nil, fmt.Errorf("listing PRs for %s/%s: %w", owner, repoName, err)
		}
		all = append(all, prs...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// forgeListing is one repo's listing as EnumerateActionable took it from
// the cache, carried on the context so fetchIssues and fetchPRsWithDetails
// read the same snapshot instead of listing.
type forgeListing struct {
	snap forgestate.Snapshot
	// closedScanReuse is how long the attributed closed-PR scan from a
	// previous pass stays good for a cache-served pass: the store's
	// reconcile interval, so that history refreshes on the same cadence
	// as the open listing instead of on every wake.
	closedScanReuse time.Duration
}

type forgeListingKey struct{}

func withForgeListing(ctx context.Context, l *forgeListing) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, forgeListingKey{}, l)
}

func forgeListingFrom(ctx context.Context) *forgeListing {
	if ctx == nil {
		return nil
	}
	l, _ := ctx.Value(forgeListingKey{}).(*forgeListing)
	return l
}

// forgeListingForRepo takes repo's listing from the attached cache. nil,
// nil means list GitHub directly: no cache attached, or the repo is not one
// the cache was built with (a repo added since boot lists live until the
// store is rebuilt). ErrForgeStateNotReady is returned before the repo's
// first successful reconcile.
func (c *Client) forgeListingForRepo(repo string) (*forgeListing, error) {
	store := c.ForgeState()
	if store == nil {
		return nil, nil
	}
	snap, ok := store.Snapshot(repo)
	if !ok {
		c.logger.Debug("forgestate: repo not in cache; listing GitHub directly", "repo", repo)
		return nil, nil
	}
	if !snap.Ready {
		return nil, fmt.Errorf("%w: %s", ErrForgeStateNotReady, repo)
	}
	return &forgeListing{snap: snap, closedScanReuse: store.ReconcileInterval()}, nil
}

// fetchAttributedClosedPRsForEnumeration is fetchPRsWithDetails's closed
// attributed-PR scan. A cache-served pass reuses the previous scan while it
// is younger than the reconcile interval; everything else lists.
func (c *Client) fetchAttributedClosedPRsForEnumeration(ctx context.Context, owner, repoName, repo string) ([]PullRequest, error) {
	if l := forgeListingFrom(ctx); l != nil && l.closedScanReuse > 0 {
		if prs, fetched, ok := c.cachedAttributedClosedPRsWithAge(repo); ok && time.Since(fetched) < l.closedScanReuse {
			return prs, nil
		}
	}
	return c.fetchAttributedClosedPRs(ctx, owner, repoName, repo)
}

// issues returns the cached open issues in number order, with the
// dependency and sub-issue summaries the list carried.
func (l *forgeListing) issues() ([]*gh.Issue, map[int]int, map[int]IssueChildren) {
	blocked := map[int]int{}
	children := map[int]IssueChildren{}
	out := make([]*gh.Issue, 0, len(l.snap.Issues))
	for _, it := range l.snap.Issues {
		issue, b, kids, ok := issueFromForgeItem(it)
		if !ok {
			continue
		}
		out = append(out, issue)
		if b > 0 {
			blocked[issue.GetNumber()] = b
		}
		if kids != nil && kids.Total > 0 {
			children[issue.GetNumber()] = *kids
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetNumber() < out[j].GetNumber() })
	return out, blocked, children
}

// prs returns the cached open pull requests in number order.
func (l *forgeListing) prs() []*gh.PullRequest {
	out := make([]*gh.PullRequest, 0, len(l.snap.PRs))
	for _, it := range l.snap.PRs {
		if pr, ok := prFromForgeItem(it); ok {
			out = append(out, pr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetNumber() < out[j].GetNumber() })
	return out
}

// itemPatches records every cached item's fetch time and delivery on the
// result, so ItemFetchedAt measures from the cache read (webhook or
// reconcile) rather than from the pass.
func (l *forgeListing) itemPatches(repo string, into map[string]ItemPatchMeta) {
	for _, it := range l.snap.Issues {
		into[ItemPatchKey(repo, it.Number, false)] = forgeItemMeta(it, l.snap)
	}
	for _, it := range l.snap.PRs {
		into[ItemPatchKey(repo, it.Number, true)] = forgeItemMeta(it, l.snap)
	}
}

func forgeItemMeta(it forgestate.Item, snap forgestate.Snapshot) ItemPatchMeta {
	meta := ItemPatchMeta{FetchedAt: it.FetchedAt, UpdatedAt: it.UpdatedAt, HeadSHA: it.HeadSHA}
	if it.Source == forgestate.SourceWebhook {
		meta.DeliveryID = snap.LastDeliveryID
	}
	return meta
}

// listOpenIssuesForEnumeration is fetchIssues's list: the cached listing
// when the pass attached one, the live list otherwise.
func (c *Client) listOpenIssuesForEnumeration(ctx context.Context, owner, repoName string) ([]*gh.Issue, map[int]int, map[int]IssueChildren, error) {
	if l := forgeListingFrom(ctx); l != nil {
		issues, blocked, children := l.issues()
		return issues, blocked, children, nil
	}
	return c.listOpenIssuesWithBlockedCounts(ctx, owner, repoName)
}

// listOpenPRsForEnumeration is fetchPRsWithDetails's list; see
// listOpenIssuesForEnumeration.
func (c *Client) listOpenPRsForEnumeration(ctx context.Context, owner, repoName string) ([]*gh.PullRequest, error) {
	if l := forgeListingFrom(ctx); l != nil {
		return l.prs(), nil
	}
	return c.listOpenPRs(ctx, owner, repoName)
}

// forgeRepoKey maps a repo reference in any spelling the client accepts
// ("name" under the configured org, or "owner/name") onto the string the
// store was built with, or "" when the store does not hold it.
func forgeRepoKey(store *forgestate.Store, c *Client, repo string) string {
	owner, name := c.splitRepo(repo)
	for _, cfg := range store.Repos() {
		if o, n := c.splitRepo(cfg); strings.EqualFold(o, owner) && strings.EqualFold(n, name) {
			return cfg
		}
	}
	return ""
}

// forgeObserve hands an item another reader just fetched to the attached
// cache (forgestate.Store.Observe). No cache, or a repo the cache does not
// hold, is a no-op.
func (c *Client) forgeObserve(repo string, it forgestate.Item, src forgestate.Source, deliveryID string) {
	store := c.ForgeState()
	if store == nil {
		return
	}
	repo = forgeRepoKey(store, c, repo)
	if repo == "" {
		return
	}
	if _, err := store.Observe(repo, it, src, deliveryID); err != nil && !errors.Is(err, forgestate.ErrUnknownRepo) {
		c.logger.Debug("forgestate: observe failed", "repo", repo, "number", it.Number, "error", err)
	}
}

// forgeWriteThrough applies one of the hive's own accepted writes to the
// attached cache. The kind is resolved from the cache itself (issues and
// PRs share a number space); an item the cache does not hold is left to
// its webhook.
func (c *Client) forgeWriteThrough(repo string, number int, m forgestate.Mutation) {
	store := c.ForgeState()
	if store == nil || number <= 0 {
		return
	}
	repo = forgeRepoKey(store, c, repo)
	if repo == "" {
		return
	}
	m.Number = number
	if m.Kind == "" {
		switch {
		case hasForgeItem(store, repo, forgestate.KindIssue, number):
			m.Kind = forgestate.KindIssue
		case hasForgeItem(store, repo, forgestate.KindPR, number):
			m.Kind = forgestate.KindPR
		default:
			return
		}
	}
	if err := store.WriteThrough(repo, m); err != nil && !errors.Is(err, forgestate.ErrUnknownRepo) {
		c.logger.Debug("forgestate: write-through failed", "repo", repo, "number", number, "op", string(m.Op), "error", err)
	}
}

func hasForgeItem(store *forgestate.Store, repo string, k forgestate.Kind, n int) bool {
	if k == forgestate.KindPR {
		_, ok := store.PR(repo, n)
		return ok
	}
	_, ok := store.Issue(repo, n)
	return ok
}

// webhookDeliveryKey carries the X-GitHub-Delivery id of the delivery that
// asked for a re-read, so FetchWebhookItemSnapshot can tag the snapshot and
// the cache with it.
type webhookDeliveryKey struct{}

// WithWebhookDelivery tags ctx with the delivery id behind a re-read.
func WithWebhookDelivery(ctx context.Context, deliveryID string) context.Context {
	if deliveryID == "" {
		return ctx
	}
	return context.WithValue(ctx, webhookDeliveryKey{}, deliveryID)
}

// WebhookDeliveryFrom returns the delivery id WithWebhookDelivery set, if any.
func WebhookDeliveryFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(webhookDeliveryKey{}).(string)
	return id
}

// forgeStaleSince is the StaleSince an enumeration pass reports for a
// stale cache: the last successful reconcile, when there was one.
func forgeStaleSince(snap forgestate.Snapshot) time.Time {
	if !snap.LastGoodAt.IsZero() {
		return snap.LastGoodAt
	}
	return snap.StaleSince
}
