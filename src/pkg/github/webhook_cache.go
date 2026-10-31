package github

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	itemblocker "github.com/hivecommons/hive/pkg/blocker"
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
	// TargetedRefreshes1h counts the single-item GitHub reads the dashboard
	// made because a webhook named that PR or issue (stage tracker refresh);
	// TrackerInvalidations1h counts the /api/stage-tracker cache drops those
	// deliveries and refreshes caused.
	TargetedRefreshes1h    int `json:"targeted_refreshes_1h"`
	TrackerInvalidations1h int `json:"tracker_invalidations_1h"`
	// WebhookModeStatus is the reconcile cadence the health drives:
	// `mode` webhook|polling and `since` (hivecommons/hive#13381).
	WebhookModeStatus
}

// WebhookEventResult reports what one delivered GitHub webhook did.
type WebhookEventResult struct {
	Event       string `json:"event"`
	Repo        string `json:"repo"`
	KnownRepo   bool   `json:"known_repo"`
	Invalidated []int  `json:"invalidated"`
	// DirtyIssues lists issues opened or relabeled by an `issues` event.
	// They wake evaluation and feed the next un-park sweep (#11313).
	DirtyIssues []int `json:"dirty_issues,omitempty"`
	// ReopenedIssue is the issue an `issues` reopened event named, if any.
	ReopenedIssue int `json:"reopened_issue,omitempty"`
	// BlockerKind is the structured blocker an issue_comment delivery
	// recorded on the issue, if any (hivecommons/hive#15099).
	BlockerKind string `json:"blocker_kind,omitempty"`
	// TouchedIssues lists the non-PR issues an `issues` or `issue_comment`
	// event named, whatever the action: the stage tracker re-reads them.
	TouchedIssues []int `json:"touched_issues,omitempty"`
	// ConfiguredRepo is the repo as this hive configured it (the name the
	// status snapshot uses), which may differ in case from Repo.
	ConfiguredRepo string `json:"configured_repo,omitempty"`
	// WokeReviewWatcher is set when the delivery woke the review-request
	// watcher (hivecommons/hive#13378).
	WokeReviewWatcher bool `json:"woke_review_watcher,omitempty"`
	// HeadSHA is the commit a check_suite, check_run or status event
	// reported on; a reader with its own PR set (forgestate) resolves it to
	// a PR the detail cache may not hold (hivecommons/hive#13377).
	HeadSHA string `json:"head_sha,omitempty"`
	// ClaimedIssues lists the issues a `pull_request` delivery's PR claims
	// (closing keyword, branch name or reference) in its own repository,
	// as recorded into the claim ledger on the event (hivecommons/hive#14814).
	ClaimedIssues []int `json:"claimed_issues,omitempty"`
	// Item is the payload's own view of the issue or pull request the
	// delivery named (every `issues`, `issue_comment` and `pull_request*`
	// payload embeds the full object with its state), nil for deliveries
	// that name none. Readers trust it in the conservative direction only:
	// a closed or merged state drops the row at once, whatever the action,
	// so a stage tracker whose confirming GET is refused or lost cannot keep
	// showing a closed item as open (hivecommons/hive#14159, #14064).
	Item *WebhookItemHint `json:"item,omitempty"`
	// CIFailure is set when a check_run, check_suite or status delivery
	// reported a failed check (failure / timed_out / error) on a head SHA,
	// with the open PRs that head resolved to.
	CIFailure *WebhookCIFailure `json:"ci_failure,omitempty"`
}

// WebhookItemHint is the state a webhook payload reports for the item it
// names, as of the event.
type WebhookItemHint struct {
	Number    int       `json:"number"`
	IsPR      bool      `json:"is_pr"`
	State     string    `json:"state"`
	Merged    bool      `json:"merged,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// Closed reports whether the payload says the item is no longer open.
func (h *WebhookItemHint) Closed() bool {
	return h != nil && (h.Merged || strings.EqualFold(h.State, "closed"))
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
	dirtyIssues   map[string]map[int]time.Time
	events        []time.Time
	invalidations []time.Time
	refreshes     []time.Time
	trackerDrops  []time.Time
	wasHealthy    bool
	// lastActivity is the newest GitHub `updated_at` a poll listed and
	// lastFailure the newest delivery that failed verification; both feed
	// the cadence mode machine (webhook_mode.go).
	lastActivity time.Time
	lastFailure  time.Time
	mode         webhookModeMachine
}

func newWebhookTracker() *webhookTracker {
	return &webhookTracker{
		now:         time.Now,
		window:      DefaultWebhookHealthWindow,
		lastEvent:   map[string]time.Time{},
		aliases:     map[string][]string{},
		dirty:       map[string]map[int]time.Time{},
		dirtyIssues: map[string]map[int]time.Time{},
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
// observation is the first one since they changed state in either direction
// (stale → healthy, healthy → stale), so callers can log exactly once per
// transition.
func ObserveWebhookHealth() (healthy, becameHealthy, becameStale bool) {
	return sharedWebhookTracker.observe()
}

// RecordWebhookEventForTest records a delivery for repo as if a verified
// webhook had arrived, so tests outside this package can drive webhook health
// without a GitHub client.
func RecordWebhookEventForTest(repo string) {
	key := canonicalPRDetailRepo(repo)
	sharedWebhookTracker.recordEvent([]string{key})
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

// RecordWebhookTargetedRefreshes counts n single-item GitHub reads made on
// behalf of webhook deliveries (/api/status webhooks.targeted_refreshes_1h).
func RecordWebhookTargetedRefreshes(n int) {
	t := sharedWebhookTracker
	t.recordStamped(&t.refreshes, n)
}

// RecordWebhookTrackerInvalidations counts n stage-tracker cache drops caused
// by webhook deliveries (/api/status webhooks.tracker_invalidations_1h).
func RecordWebhookTrackerInvalidations(n int) {
	t := sharedWebhookTracker
	t.recordStamped(&t.trackerDrops, n)
}

func (t *webhookTracker) recordStamped(dst *[]time.Time, n int) {
	if n <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	*dst = pruneWebhookTimes(*dst, now)
	for i := 0; i < n; i++ {
		*dst = append(*dst, now)
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

// markIssueDirty records that an issue's labels changed, so the un-park
// sweep can run ahead of its polling interval. Pull requests never land
// here; they have their own dirty set. The mark is stored under the first
// alias only (the configured repo name) because it is counted, not looked up
// by alias.
func (t *webhookTracker) markIssueDirty(aliases []string, number int) {
	if len(aliases) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	byNumber := t.dirtyIssues[aliases[0]]
	if byNumber == nil {
		byNumber = map[int]time.Time{}
		t.dirtyIssues[aliases[0]] = byNumber
	}
	byNumber[number] = t.now()
}

// takeDirtyIssues returns how many issues webhooks marked dirty since the last
// call and clears them.
func (t *webhookTracker) takeDirtyIssues() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, byNumber := range t.dirtyIssues {
		n += len(byNumber)
	}
	t.dirtyIssues = map[string]map[int]time.Time{}
	return n
}

// TakeDirtyIssueCount reports how many issues an `issues` webhook marked dirty
// since the last call, and clears the mark. The un-park sweep scheduler uses
// a non-zero answer to run early instead of waiting out its interval; polling
// stays the fallback when webhooks are not delivered.
func TakeDirtyIssueCount() int {
	return sharedWebhookTracker.takeDirtyIssues()
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
	t.refreshes = pruneWebhookTimes(t.refreshes, now)
	t.trackerDrops = pruneWebhookTimes(t.trackerDrops, now)
	healthy, latest := t.healthyLocked(now)
	out := WebhookHealth{Healthy: healthy, Events1h: len(t.events), Invalidations1h: len(t.invalidations),
		TargetedRefreshes1h: len(t.refreshes), TrackerInvalidations1h: len(t.trackerDrops),
		WebhookModeStatus: t.mode.status(t.window, now)}
	if !latest.IsZero() {
		at := latest.UTC()
		out.LastEventAt = &at
	}
	return out
}

func (t *webhookTracker) observe() (healthy, becameHealthy, becameStale bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	healthy, _ = t.healthyLocked(t.now())
	becameHealthy = !t.wasHealthy && healthy
	becameStale = t.wasHealthy && !healthy
	t.wasHealthy = healthy
	return healthy, becameHealthy, becameStale
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

// ResetWebhookTrackerForTest swaps in a fresh process-wide webhook tracker and
// returns a restore func. Tests outside this package that read webhook health
// (e.g. via the governor eval interval) call it so webhook events recorded by
// earlier tests in the binary cannot make webhooks look healthy.
func ResetWebhookTrackerForTest() func() {
	return resetWebhookTrackerForTest(nil)
}

// ResetWebhookTrackerForTestWithClock is ResetWebhookTrackerForTest with an
// injected clock, so a test can age a recorded delivery past the health
// window without sleeping.
func ResetWebhookTrackerForTestWithClock(now func() time.Time) func() {
	return resetWebhookTrackerForTest(now)
}

type webhookPRRef struct {
	Number int `json:"number"`
}

type webhookEventPayload struct {
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Number      int    `json:"number"`
	Action      string `json:"action"`
	PullRequest *struct {
		Number    int        `json:"number"`
		Title     string     `json:"title"`
		Body      string     `json:"body"`
		State     string     `json:"state"`
		Merged    bool       `json:"merged"`
		MergedAt  *time.Time `json:"merged_at"`
		ClosedAt  *time.Time `json:"closed_at"`
		CreatedAt *time.Time `json:"created_at"`
		UpdatedAt *time.Time `json:"updated_at"`
		HTMLURL   string     `json:"html_url"`
		User      *struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA string `json:"sha"`
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`
	Issue *struct {
		Number      int              `json:"number"`
		State       string           `json:"state"`
		UpdatedAt   *time.Time       `json:"updated_at"`
		PullRequest *json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	Comment *struct {
		Body      string     `json:"body"`
		CreatedAt *time.Time `json:"created_at"`
		User      *struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
	CheckSuite *struct {
		HeadSHA      string         `json:"head_sha"`
		Conclusion   string         `json:"conclusion"`
		URL          string         `json:"url"`
		PullRequests []webhookPRRef `json:"pull_requests"`
		App          struct {
			Name string `json:"name"`
		} `json:"app"`
	} `json:"check_suite"`
	CheckRun *struct {
		HeadSHA      string         `json:"head_sha"`
		Name         string         `json:"name"`
		Conclusion   string         `json:"conclusion"`
		DetailsURL   string         `json:"details_url"`
		HTMLURL      string         `json:"html_url"`
		PullRequests []webhookPRRef `json:"pull_requests"`
	} `json:"check_run"`
	WorkflowRun *struct {
		HeadSHA      string         `json:"head_sha"`
		PullRequests []webhookPRRef `json:"pull_requests"`
	} `json:"workflow_run"`
	SHA       string `json:"sha"`
	State     string `json:"state"`
	Context   string `json:"context"`
	TargetURL string `json:"target_url"`
	Branches  []struct {
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
	res.ConfiguredRepo = configured
	aliases := webhookRepoAliases(configured, p.Repository.FullName)
	sharedWebhookTracker.recordEvent(aliases)

	res.Item = webhookItemHint(event, p)
	if n, ok := webhookTouchedIssue(event, p); ok {
		res.TouchedIssues = []int{n}
		if res.Item.Closed() {
			// Closed issues no longer contribute to blocked_items. Comments
			// delivered after closure must not recreate their blocker.
			l := itemblocker.Shared()
			l.Clear(configured, n)
			// Comment webhooks use the configured name; sweep reports use
			// owner/repo. Clear both when configuration uses a short name.
			owner, repo := c.splitRepo(configured)
			if full := owner + "/" + repo; full != configured {
				l.Clear(full, n)
			}
		} else if body, at, ok := webhookIssueCommentBody(event, p); ok {
			// A needs-human report that names a capability the App lacks
			// becomes a structured blocker the moment it lands, so the
			// tracker and "Let hive decide" read it before the next
			// enumeration (hivecommons/hive#15099).
			if e, changed := itemblocker.Shared().Observe(configured, n, body, at); changed && e.Blocks() {
				res.BlockerKind = string(e.Blocker.Kind)
			}
		}
	}
	if reviewRequestWakeEvent(event, p) {
		c.reviewWake.wake()
		res.WokeReviewWatcher = true
	}

	if n, ok := webhookIssueDirtyTarget(event, p); ok {
		sharedWebhookTracker.markIssueDirty(aliases, n)
		res.DirtyIssues = []int{n}
	}
	if n, ok := webhookIssueReopenedTarget(event, p); ok {
		res.ReopenedIssue = n
		if hook := issueReopenedHook(); hook != nil {
			hook(configured, n, sharedWebhookTracker.now())
		}
	}
	if d, ok := c.webhookPRClaimDelivery(event, configured, p, sharedWebhookTracker.now()); ok {
		// The PR's claims take effect now, not on the next reconcile pass
		// (hivecommons/hive#14814); the live open-PR index for the repo is
		// stale the same instant, so the next guard call lists again.
		res.ClaimedIssues = d.ClaimedIssues()
		c.InvalidateOpenPRLinks(configured)
		if hook := prClaimsHook(); hook != nil {
			hook(d)
		}
		c.coverIssuesForWebhookPR(configured, d)
	}
	if merged, ok := webhookMergedPR(event, p); ok {
		// A merged revert names the PR it undid; the payload already carries
		// the title and body, so this costs no GitHub call (#13241).
		c.RecordReviewEvidenceRevert(merged)
	}
	res.HeadSHA = webhookHeadSHA(event, p)
	if res.HeadSHA != "" {
		sharedCIReportingCache.invalidate(p.Repository.FullName, res.HeadSHA)
	}
	numbers := webhookInvalidationTargets(event, p, aliases)
	// Capture the failed-check identity and the PRs it resolved to BEFORE
	// invalidation drops their cache entries: the receiver reacts to a red
	// check on the event rather than on the next sweep.
	res.CIFailure = c.webhookCIFailureResult(event, p, aliases, numbers)
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

// webhookIssueDirtyTarget returns an opened or relabeled non-PR issue.
// Opened issues must wake evaluation too: a tracker refresh alone does not
// run the CEL rules against the newly patched actionable snapshot.
func webhookIssueDirtyTarget(event string, p webhookEventPayload) (int, bool) {
	if event != "issues" || p.Issue == nil || p.Issue.PullRequest != nil || p.Issue.Number <= 0 {
		return 0, false
	}
	switch p.Action {
	case "opened", "labeled", "unlabeled":
		return p.Issue.Number, true
	}
	return 0, false
}

// webhookItemHint is the payload's state for the issue or pull request an
// `issues`, `issue_comment` or `pull_request*` delivery embeds; nil when the
// event carries neither object.
func webhookItemHint(event string, p webhookEventPayload) *WebhookItemHint {
	switch event {
	case "pull_request", "pull_request_review", "pull_request_review_comment", "pull_request_review_thread":
		if p.PullRequest == nil || p.PullRequest.Number <= 0 {
			return nil
		}
		h := &WebhookItemHint{Number: p.PullRequest.Number, IsPR: true, State: p.PullRequest.State,
			Merged: p.PullRequest.Merged || (p.PullRequest.MergedAt != nil && !p.PullRequest.MergedAt.IsZero())}
		if p.PullRequest.UpdatedAt != nil {
			h.UpdatedAt = *p.PullRequest.UpdatedAt
		}
		return h
	case "issues", "issue_comment":
		if p.Issue == nil || p.Issue.Number <= 0 {
			return nil
		}
		h := &WebhookItemHint{Number: p.Issue.Number, IsPR: p.Issue.PullRequest != nil, State: p.Issue.State}
		if p.Issue.UpdatedAt != nil {
			h.UpdatedAt = *p.Issue.UpdatedAt
		}
		return h
	}
	return nil
}

// webhookTouchedIssue returns the number of the non-PR issue an `issues` or
// `issue_comment` event concerns, whatever the action.
func webhookTouchedIssue(event string, p webhookEventPayload) (int, bool) {
	if event != "issues" && event != "issue_comment" {
		return 0, false
	}
	if p.Issue == nil || p.Issue.PullRequest != nil || p.Issue.Number <= 0 {
		return 0, false
	}
	return p.Issue.Number, true
}

// webhookIssueCommentBody returns the body and time of a created or edited
// issue_comment delivery on a non-PR issue.
func webhookIssueCommentBody(event string, p webhookEventPayload) (string, time.Time, bool) {
	if event != "issue_comment" || p.Comment == nil || (p.Action != "created" && p.Action != "edited") {
		return "", time.Time{}, false
	}
	at := sharedWebhookTracker.now()
	if p.Comment.CreatedAt != nil && !p.Comment.CreatedAt.IsZero() {
		at = *p.Comment.CreatedAt
	}
	return p.Comment.Body, at, true
}

// webhookIssueReopenedTarget returns the issue number of an `issues` event
// whose action reopened a non-PR issue.
func webhookIssueReopenedTarget(event string, p webhookEventPayload) (int, bool) {
	if event != "issues" || p.Issue == nil || p.Issue.PullRequest != nil || p.Issue.Number <= 0 {
		return 0, false
	}
	return p.Issue.Number, p.Action == "reopened"
}

// IssueReopenedHook is told, with the configured repo name, the issue number
// and the delivery time, that an `issues` webhook reopened an issue. The
// duplicate-PR claim ledger registers one so a reopen voids the merged claim
// on the issue without waiting for the next enumeration pass.
type IssueReopenedHook func(repo string, number int, at time.Time)

var (
	issueReopenedHookMu sync.RWMutex
	issueReopenedHookFn IssueReopenedHook
)

// SetIssueReopenedHook installs (or, with nil, removes) the process-wide
// IssueReopenedHook.
func SetIssueReopenedHook(fn IssueReopenedHook) {
	issueReopenedHookMu.Lock()
	issueReopenedHookFn = fn
	issueReopenedHookMu.Unlock()
}

func issueReopenedHook() IssueReopenedHook {
	issueReopenedHookMu.RLock()
	defer issueReopenedHookMu.RUnlock()
	return issueReopenedHookFn
}

// webhookHeadSHA returns the head commit a check_suite, check_run or status
// event concerns; "" for every other event.
func webhookHeadSHA(event string, p webhookEventPayload) string {
	switch event {
	case "check_suite":
		if p.CheckSuite != nil {
			return strings.TrimSpace(p.CheckSuite.HeadSHA)
		}
	case "check_run":
		if p.CheckRun != nil {
			return strings.TrimSpace(p.CheckRun.HeadSHA)
		}
	case "status":
		return strings.TrimSpace(p.SHA)
	case "workflow_run":
		if p.WorkflowRun != nil {
			return strings.TrimSpace(p.WorkflowRun.HeadSHA)
		}
	}
	return ""
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
	case "pull_request", "pull_request_review", "pull_request_review_comment":
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
	case "workflow_run":
		if p.WorkflowRun != nil {
			addRefs(p.WorkflowRun.PullRequests)
			bySHA(p.WorkflowRun.HeadSHA)
		}
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

// webhookCIFailureResult pairs a failed-check delivery with the PRs it
// resolved to, read from the detail cache before the delivery invalidates
// them. A PR the cache does not hold is reported by number only.
func (c *Client) webhookCIFailureResult(event string, p webhookEventPayload, aliases []string, numbers []int) *WebhookCIFailure {
	f := webhookCIFailure(event, p)
	if f == nil || f.HeadSHA == "" {
		return nil
	}
	for _, n := range numbers {
		ref := CIFailurePR{Number: n}
		for _, repo := range aliases {
			pr, headSHA, ok := sharedPRDetailCache.peek(repo, n)
			if !ok {
				continue
			}
			if pr.GetState() != "" && pr.GetState() != "open" {
				ref.Number = 0
				break
			}
			if headSHA != "" && !strings.EqualFold(headSHA, f.HeadSHA) {
				// The cache knows a newer head: this failure is stale.
				ref.Number = 0
				break
			}
			ref.Author = pr.GetUser().GetLogin()
			ref.Title = pr.GetTitle()
			ref.URL = pr.GetHTMLURL()
			ref.BaseRef = pr.GetBase().GetRef()
			ref.HiveAuthored = c.IsHiveAuthorLogin(ref.Author)
			break
		}
		if ref.Number > 0 {
			f.PRs = append(f.PRs, ref)
		}
	}
	return f
}
