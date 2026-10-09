package github

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultWebhookHealthWindow is the "healthy" window used until the governor
// loop publishes 2 × governor.eval_interval_s via SetWebhookHealthWindow.
const DefaultWebhookHealthWindow = 10 * time.Minute

const webhookStatsWindow = time.Hour

// WebhookHealth is the `webhooks` block served by /api/status and
// /api/gh-rate-limits (hivecommons/hive#11177).
type WebhookHealth struct {
	Healthy         bool       `json:"healthy"`
	LastEventAt     *time.Time `json:"last_event_at"`
	Events1h        int        `json:"events_1h"`
	Invalidations1h int        `json:"invalidations_1h"`
}

// WebhookEventResult reports what one delivered GitHub webhook did.
type WebhookEventResult struct {
	Event       string `json:"event"`
	Repo        string `json:"repo"`
	KnownRepo   bool   `json:"known_repo"`
	Invalidated []int  `json:"invalidated"`
}

// webhookTracker records webhook liveness per repository and the set of PRs a
// webhook marked dirty. Clean PRs in a repo whose webhooks are healthy are
// served from the PR detail cache past its TTL; dirty PRs are re-enriched on
// the next cycle.
type webhookTracker struct {
	mu            sync.Mutex
	now           func() time.Time
	window        time.Duration
	lastEvent     map[string]time.Time
	aliases       map[string][]string
	dirty         map[string]map[int]time.Time
	events        []time.Time
	invalidations []time.Time
	wasHealthy    bool
}

func newWebhookTracker() *webhookTracker {
	return &webhookTracker{
		now:       time.Now,
		window:    DefaultWebhookHealthWindow,
		lastEvent: map[string]time.Time{},
		aliases:   map[string][]string{},
		dirty:     map[string]map[int]time.Time{},
	}
}

var sharedWebhookTracker = newWebhookTracker()

// SetWebhookHealthWindow sets how recent the last webhook event must be for
// webhooks to count as healthy. The governor publishes 2 × eval_interval_s.
func SetWebhookHealthWindow(d time.Duration) {
	if d <= 0 {
		d = DefaultWebhookHealthWindow
	}
	t := sharedWebhookTracker
	t.mu.Lock()
	t.window = d
	t.mu.Unlock()
}

// WebhookHealthSnapshot returns the current webhook health block.
func WebhookHealthSnapshot() WebhookHealth {
	return sharedWebhookTracker.snapshot()
}

// ObserveWebhookHealth reports whether webhooks are healthy and whether this
// observation is the first one since they went healthy → stale, so callers can
// WARN exactly once per transition.
func ObserveWebhookHealth() (healthy, becameStale bool) {
	return sharedWebhookTracker.observe()
}

func (t *webhookTracker) recordEvent(aliases []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, a := range aliases {
		t.lastEvent[a] = now
		t.aliases[a] = aliases
	}
	t.events = append(pruneWebhookTimes(t.events, now), now)
}

func (t *webhookTracker) recordInvalidations(n int) {
	if n <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.invalidations = pruneWebhookTimes(t.invalidations, now)
	for i := 0; i < n; i++ {
		t.invalidations = append(t.invalidations, now)
	}
}

func (t *webhookTracker) markDirty(aliases []string, number int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for _, a := range aliases {
		byNumber := t.dirty[a]
		if byNumber == nil {
			byNumber = map[int]time.Time{}
			t.dirty[a] = byNumber
		}
		byNumber[number] = now
	}
}

// clearDirtyBefore clears a dirty mark set before since. A mark set after the
// re-enrichment started belongs to an event the fetch may not reflect, so it
// survives for the next cycle.
func (t *webhookTracker) clearDirtyBefore(repo string, number int, since time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, a := range t.aliasesLocked(repo) {
		if at, ok := t.dirty[a][number]; ok && at.Before(since) {
			delete(t.dirty[a], number)
		}
	}
}

func (t *webhookTracker) clearDirtyRepoBefore(repo string, since time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, a := range t.aliasesLocked(repo) {
		for n, at := range t.dirty[a] {
			if at.Before(since) {
				delete(t.dirty[a], n)
			}
		}
	}
}

// cleanAndHealthy reports whether a cached PR may outlive its TTL: the repo's
// webhooks are healthy and no webhook has marked the PR dirty since.
func (t *webhookTracker) cleanAndHealthy(repo string, number int) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := canonicalPRDetailRepo(repo)
	if !t.repoHealthyLocked(key, t.now()) {
		return false
	}
	_, dirty := t.dirty[key][number]
	return !dirty
}

// repoCleanAndHealthy reports whether the whole repo can skip its per-cycle
// re-enrichment: healthy webhooks and no dirty PR.
func (t *webhookTracker) repoCleanAndHealthy(repo string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := canonicalPRDetailRepo(repo)
	return t.repoHealthyLocked(key, t.now()) && len(t.dirty[key]) == 0
}

func (t *webhookTracker) repoHealthyLocked(key string, now time.Time) bool {
	last, ok := t.lastEvent[key]
	return ok && now.Sub(last) < t.window
}

func (t *webhookTracker) aliasesLocked(repo string) []string {
	key := canonicalPRDetailRepo(repo)
	if group, ok := t.aliases[key]; ok {
		return group
	}
	return []string{key}
}

func (t *webhookTracker) healthyLocked(now time.Time) (bool, time.Time) {
	var latest time.Time
	for _, at := range t.lastEvent {
		if at.After(latest) {
			latest = at
		}
	}
	return !latest.IsZero() && now.Sub(latest) < t.window, latest
}

func (t *webhookTracker) snapshot() WebhookHealth {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.events = pruneWebhookTimes(t.events, now)
	t.invalidations = pruneWebhookTimes(t.invalidations, now)
	healthy, latest := t.healthyLocked(now)
	out := WebhookHealth{Healthy: healthy, Events1h: len(t.events), Invalidations1h: len(t.invalidations)}
	if !latest.IsZero() {
		at := latest.UTC()
		out.LastEventAt = &at
	}
	return out
}

func (t *webhookTracker) observe() (healthy, becameStale bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	healthy, _ = t.healthyLocked(t.now())
	becameStale = t.wasHealthy && !healthy
	t.wasHealthy = healthy
	return healthy, becameStale
}

func pruneWebhookTimes(in []time.Time, now time.Time) []time.Time {
	cut := 0
	for cut < len(in) && now.Sub(in[cut]) >= webhookStatsWindow {
		cut++
	}
	if cut == 0 {
		return in
	}
	return append(in[:0], in[cut:]...)
}

func resetWebhookTrackerForTest(now func() time.Time) func() {
	old := sharedWebhookTracker
	t := newWebhookTracker()
	if now != nil {
		t.now = now
	}
	sharedWebhookTracker = t
	return func() { sharedWebhookTracker = old }
}

type webhookPRRef struct {
	Number int `json:"number"`
}

type webhookEventPayload struct {
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Number      int `json:"number"`
	PullRequest *struct {
		Number int `json:"number"`
	} `json:"pull_request"`
	Issue *struct {
		Number      int              `json:"number"`
		PullRequest *json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	CheckSuite *struct {
		HeadSHA      string         `json:"head_sha"`
		PullRequests []webhookPRRef `json:"pull_requests"`
	} `json:"check_suite"`
	CheckRun *struct {
		HeadSHA      string         `json:"head_sha"`
		PullRequests []webhookPRRef `json:"pull_requests"`
	} `json:"check_run"`
	SHA      string `json:"sha"`
	Branches []struct {
		Name string `json:"name"`
	} `json:"branches"`
	Ref string `json:"ref"`
}

// HandleWebhookEvent applies one signature-verified GitHub webhook delivery to
// the PR caches. Every event for a configured repository refreshes that repo's
// webhook liveness; PR-shaped events also invalidate the matching PR detail
// cache entry and GraphQL batch check-run/review entries and mark the PR dirty
// so the next cycle re-enriches it. Events for repositories this hive does not
// manage are ignored.
func (c *Client) HandleWebhookEvent(event string, payload []byte) (WebhookEventResult, error) {
	res := WebhookEventResult{Event: event}
	if c == nil {
		return res, nil
	}
	var p webhookEventPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return res, fmt.Errorf("decoding %s webhook payload: %w", event, err)
	}
	res.Repo = p.Repository.FullName
	configured, ok := c.webhookConfiguredRepo(p.Repository.FullName)
	if !ok {
		return res, nil
	}
	res.KnownRepo = true
	aliases := webhookRepoAliases(configured, p.Repository.FullName)
	sharedWebhookTracker.recordEvent(aliases)

	numbers := webhookInvalidationTargets(event, p, aliases)
	for _, n := range numbers {
		c.invalidateWebhookPR(aliases, n)
	}
	sharedWebhookTracker.recordInvalidations(len(numbers))
	res.Invalidated = numbers
	return res, nil
}

func (c *Client) webhookConfiguredRepo(fullName string) (string, bool) {
	fullName = strings.TrimSpace(fullName)
	if fullName == "" {
		return "", false
	}
	for _, r := range c.getRepos() {
		owner, name := c.splitRepo(r)
		if strings.EqualFold(owner+"/"+name, fullName) {
			return r, true
		}
	}
	return "", false
}

func webhookRepoAliases(configured, fullName string) []string {
	a, b := canonicalPRDetailRepo(configured), canonicalPRDetailRepo(fullName)
	if a == b {
		return []string{a}
	}
	return []string{a, b}
}

func webhookInvalidationTargets(event string, p webhookEventPayload, aliases []string) []int {
	set := map[int]bool{}
	add := func(n int) {
		if n > 0 {
			set[n] = true
		}
	}
	addRefs := func(refs []webhookPRRef) {
		for _, r := range refs {
			add(r.Number)
		}
	}
	addMatching := func(match func(headSHA, headRef, baseRef string) bool) {
		for _, n := range sharedPRDetailCache.numbersMatching(aliases, match) {
			add(n)
		}
	}
	bySHA := func(sha string) {
		if sha = strings.TrimSpace(sha); sha != "" {
			addMatching(func(headSHA, _, _ string) bool { return headSHA == sha })
		}
	}

	switch event {
	case "pull_request", "pull_request_review":
		if p.PullRequest != nil && p.PullRequest.Number > 0 {
			add(p.PullRequest.Number)
		} else {
			add(p.Number)
		}
	case "issue_comment":
		if p.Issue != nil && p.Issue.PullRequest != nil {
			add(p.Issue.Number)
		}
	case "check_suite":
		if p.CheckSuite != nil {
			addRefs(p.CheckSuite.PullRequests)
			// Fork PRs arrive with an empty pull_requests list; the head SHA
			// still identifies them.
			bySHA(p.CheckSuite.HeadSHA)
		}
	case "check_run":
		if p.CheckRun != nil {
			addRefs(p.CheckRun.PullRequests)
			bySHA(p.CheckRun.HeadSHA)
		}
	case "status":
		bySHA(p.SHA)
	case "push":
		if ref, ok := strings.CutPrefix(p.Ref, "refs/heads/"); ok && ref != "" {
			addMatching(func(_, headRef, baseRef string) bool { return headRef == ref || baseRef == ref })
		}
	}

	out := make([]int, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func (c *Client) invalidateWebhookPR(aliases []string, number int) {
	for _, repo := range aliases {
		sharedPRDetailCache.invalidate(repo, number)
		c.invalidateGraphQLPRBatchPR(repo, number)
	}
	sharedWebhookTracker.markDirty(aliases, number)
}
