package github

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Webhook-driven CI evidence (hivecommons/hive#16521).
//
// The Activity Monitor's CI chevron counts check runs on a PR's current head
// (PullRequest.CIActivity). Until now that evidence had one producer, the
// per-cycle check-run listing in EnrichCIStatus, so a row read "CI ?/?"
// from the moment a head was pushed until the next full cycle, and a
// sidelined PR never had any. A `check_run`, `workflow_job` or `status`
// delivery already names the head, the check and its result, so the payload
// is applied straight into a per-(repo, head) check set here. The tracker
// merges that set over whatever the last listing produced, and a head no
// listing has covered yet is re-read once, coalesced, so the required-check
// set behind the counts becomes known. The listing stays as the backstop
// for spokes without webhooks.

// githubActionsAppID is the App id GitHub Actions reports on its check runs;
// a `workflow_job` payload carries no app object, but its check run is
// always this producer's.
const githubActionsAppID int64 = 15368

// ciEvidenceMaxHeads bounds the per-head store; the oldest-touched heads are
// evicted first. ciEvidenceTTL drops heads no delivery or listing has
// touched for that long (a merged PR's head, for one).
const (
	ciEvidenceMaxHeads = 4096
	ciEvidenceTTL      = 48 * time.Hour
)

// webhookCIEventTypes are the deliveries that carry CI evidence for a head.
var webhookCIEventTypes = map[string]bool{
	"check_run":    true,
	"check_suite":  true,
	"status":       true,
	"workflow_run": true,
	"workflow_job": true,
}

// IsWebhookCIEvent reports whether event is one of the check/status/workflow
// deliveries that drive CI evidence.
func IsWebhookCIEvent(event string) bool { return webhookCIEventTypes[event] }

// WebhookCIEvidence reports what a CI delivery did to the per-head check set.
type WebhookCIEvidence struct {
	HeadSHA string `json:"head_sha"`
	// Applied is the number of check observations taken from the payload
	// itself (check_run, workflow_job, status). A check_suite or
	// workflow_run payload carries none.
	Applied int `json:"applied"`
	// Refetch is set when the head had no check-run listing yet (no
	// required-check set to count against) and one coalesced re-read of
	// the head's check runs was scheduled.
	Refetch bool `json:"refetch,omitempty"`
	// PRs are the open PR numbers the head resolved to.
	PRs []int `json:"prs,omitempty"`
}

type ciHeadKey struct {
	repo string // canonical owner/name, lower-case
	head string // lower-case SHA
}

type ciHeadEvidence struct {
	// prs are the open PR numbers deliveries resolved this head to. A
	// check delivery invalidates the PR detail cache, so a later `status`
	// or `workflow_job` payload (which names no PR) could not resolve the
	// head by SHA without this memory.
	prs map[int]bool
	// listed is the last complete observation of the head (check-run
	// listing plus required set and statuses), nil until one ran.
	listed *PRCIActivity
	// events holds the checks taken from deliveries, newest per identity.
	events   []PRActivityCheck
	eventsAt time.Time
	touched  time.Time
}

type ciEvidenceStore struct {
	mu    sync.Mutex
	now   func() time.Time
	heads map[ciHeadKey]*ciHeadEvidence
}

func newCIEvidenceStore() *ciEvidenceStore {
	return &ciEvidenceStore{now: time.Now, heads: map[ciHeadKey]*ciHeadEvidence{}}
}

var ciEvidencePtr = func() *atomic.Pointer[ciEvidenceStore] {
	p := &atomic.Pointer[ciEvidenceStore]{}
	p.Store(newCIEvidenceStore())
	return p
}()

func sharedCIEvidence() *ciEvidenceStore { return ciEvidencePtr.Load() }

// ResetCIEvidenceForTest swaps in an empty per-head CI evidence store with
// the given clock and returns a restore func.
func ResetCIEvidenceForTest(now func() time.Time) func() {
	old := sharedCIEvidence()
	s := newCIEvidenceStore()
	if now != nil {
		s.now = now
	}
	ciEvidencePtr.Store(s)
	return func() { ciEvidencePtr.Store(old) }
}

func ciEvidenceKey(repo, head string) ciHeadKey {
	return ciHeadKey{repo: canonicalPRDetailRepo(repo), head: strings.ToLower(strings.TrimSpace(head))}
}

// entryLocked returns the head's record, creating it when create is set. A
// bare repo name (no owner) matches the one stored full name ending in it.
func (s *ciEvidenceStore) entryLocked(key ciHeadKey, create bool) *ciHeadEvidence {
	if e, ok := s.heads[key]; ok {
		return e
	}
	if !strings.Contains(key.repo, "/") {
		for k, e := range s.heads {
			if k.head == key.head && strings.HasSuffix(k.repo, "/"+key.repo) {
				return e
			}
		}
	}
	if !create {
		return nil
	}
	e := &ciHeadEvidence{}
	s.heads[key] = e
	return e
}

func (s *ciEvidenceStore) pruneLocked(now time.Time) {
	for k, e := range s.heads {
		if now.Sub(e.touched) >= ciEvidenceTTL {
			delete(s.heads, k)
		}
	}
	for len(s.heads) > ciEvidenceMaxHeads {
		var oldestKey ciHeadKey
		var oldest time.Time
		first := true
		for k, e := range s.heads {
			if first || e.touched.Before(oldest) {
				oldestKey, oldest, first = k, e.touched, false
			}
		}
		delete(s.heads, oldestKey)
	}
}

// recordListed stores a complete observation of the head. Event checks
// older than the listing are dropped: the listing already reflects them.
func (s *ciEvidenceStore) recordListed(repo, head string, a *PRCIActivity) {
	if a == nil || strings.TrimSpace(head) == "" || !strings.EqualFold(a.Head, head) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	e := s.entryLocked(ciEvidenceKey(repo, head), true)
	e.listed = cloneCIActivity(a)
	e.touched = now
	if a.ObservedAt != nil && !e.eventsAt.After(*a.ObservedAt) {
		e.events, e.eventsAt = nil, time.Time{}
	}
	s.pruneLocked(now)
}

// hasListed reports whether a check-run listing has covered the head.
func (s *ciEvidenceStore) hasListed(repo, head string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entryLocked(ciEvidenceKey(repo, head), false)
	return e != nil && e.listed != nil
}

// applyEvent upserts the checks a delivery reported for the head and
// returns how many it kept.
func (s *ciEvidenceStore) applyEvent(repo, head string, checks []PRActivityCheck) int {
	if strings.TrimSpace(head) == "" || len(checks) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	e := s.entryLocked(ciEvidenceKey(repo, head), true)
	applied := 0
	for _, check := range checks {
		if check.Name == "" {
			continue
		}
		check.Head = head
		e.events = upsertActivityCheck(e.events, check)
		applied++
	}
	if applied > 0 {
		e.eventsAt = now
	}
	e.touched = now
	s.pruneLocked(now)
	return applied
}

// resolvePRs remembers the PRs a CI delivery resolved the head to and
// returns them joined with the ones earlier deliveries remembered, sorted.
func (s *ciEvidenceStore) resolvePRs(repo, head string, numbers []int) []int {
	if strings.TrimSpace(head) == "" {
		return numbers
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	e := s.entryLocked(ciEvidenceKey(repo, head), true)
	if e.prs == nil {
		e.prs = map[int]bool{}
	}
	for _, n := range numbers {
		if n > 0 {
			e.prs[n] = true
		}
	}
	e.touched = now
	out := make([]int, 0, len(e.prs))
	for n := range e.prs {
		out = append(out, n)
	}
	sort.Ints(out)
	s.pruneLocked(now)
	return out
}

// touch keeps the head alive without evidence (check_suite, workflow_run).
func (s *ciEvidenceStore) touch(repo, head string) {
	if strings.TrimSpace(head) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	e := s.entryLocked(ciEvidenceKey(repo, head), true)
	e.touched = now
	s.pruneLocked(now)
}

// merged returns the head's evidence: the newest listing (the store's or the
// caller's base, whichever observed later) with the delivered checks laid
// over it. Nil when neither exists.
func (s *ciEvidenceStore) merged(repo, head string, base *PRCIActivity) *PRCIActivity {
	if strings.TrimSpace(head) == "" {
		return base
	}
	if base != nil && !strings.EqualFold(base.Head, head) {
		base = nil
	}
	s.mu.Lock()
	e := s.entryLocked(ciEvidenceKey(repo, head), false)
	var listed *PRCIActivity
	var events []PRActivityCheck
	var eventsAt time.Time
	if e != nil {
		listed = e.listed
		events = append([]PRActivityCheck(nil), e.events...)
		eventsAt = e.eventsAt
	}
	s.mu.Unlock()
	if base != nil && (listed == nil || observedAfter(base.ObservedAt, listed.ObservedAt)) {
		listed = base
	}
	if len(events) == 0 {
		return listed
	}
	var out *PRCIActivity
	if listed != nil {
		out = cloneCIActivity(listed)
		if out.ObservedAt != nil && !eventsAt.After(*out.ObservedAt) {
			return out
		}
	} else {
		out = &PRCIActivity{Head: head}
	}
	applied := 0
	for _, check := range events {
		out.Checks = upsertActivityCheck(out.Checks, check)
		applied++
	}
	if applied > 0 {
		at := eventsAt.UTC()
		out.ObservedAt = &at
		switch {
		case out.Source == "":
			out.Source = "github-webhook"
		case !strings.Contains(out.Source, "github-webhook"):
			out.Source += "+github-webhook"
		}
		sort.SliceStable(out.Checks, func(i, j int) bool { return out.Checks[i].Name < out.Checks[j].Name })
	}
	return out
}

func observedAfter(a, b *time.Time) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	return a.After(*b)
}

// upsertActivityCheck places check into list the way GitHub's own "latest"
// listing would: the same run (node id) is updated in place, a newer run of
// the same check (kind, name, producing App) replaces the older one, an
// older observation than the one held is dropped, and anything else is
// appended.
func upsertActivityCheck(list []PRActivityCheck, check PRActivityCheck) []PRActivityCheck {
	for i := range list {
		if check.NodeID != "" && list[i].NodeID == check.NodeID {
			if checkBefore(check, list[i]) {
				return list
			}
			list[i] = check
			return list
		}
	}
	for i := range list {
		if !sameCheckIdentity(list[i], check) {
			continue
		}
		if checkBefore(check, list[i]) {
			return list
		}
		list[i] = check
		return list
	}
	return append(list, check)
}

// checkBefore reports whether a is a strictly older observation than b;
// an observation without a time is never older.
func checkBefore(a, b PRActivityCheck) bool {
	return a.Since != nil && b.Since != nil && a.Since.Before(*b.Since)
}

// sameCheckIdentity matches two runs of one check by kind, name and
// producing App (an unknown App matches any).
func sameCheckIdentity(a, b PRActivityCheck) bool {
	if a.Kind != b.Kind || a.Name != b.Name {
		return false
	}
	if a.AppID == nil || b.AppID == nil {
		return true
	}
	return *a.AppID == *b.AppID
}

func cloneCIActivity(a *PRCIActivity) *PRCIActivity {
	if a == nil {
		return nil
	}
	out := *a
	out.Required = append([]string(nil), a.Required...)
	out.RequiredChecks = append([]PRRequiredCheck(nil), a.RequiredChecks...)
	out.Checks = append([]PRActivityCheck(nil), a.Checks...)
	if a.WorkflowApproval != nil {
		wa := *a.WorkflowApproval
		wa.RunIDs = append([]int64(nil), a.WorkflowApproval.RunIDs...)
		out.WorkflowApproval = &wa
	}
	return &out
}

// HeadCIActivity is the tracker's read: the CI evidence for repo's head,
// combining the last listing (base, from the status snapshot, or the
// store's own) with every check delivered since. It returns base unchanged
// when no delivery has named the head.
func HeadCIActivity(repo, head string, base *PRCIActivity) *PRCIActivity {
	return sharedCIEvidence().merged(repo, head, base)
}

// webhookCIChecks extracts the check observations a CI delivery carries.
func webhookCIChecks(event string, p webhookEventPayload) []PRActivityCheck {
	switch event {
	case "check_run":
		cr := p.CheckRun
		if cr == nil || strings.TrimSpace(cr.Name) == "" {
			return nil
		}
		check := PRActivityCheck{
			Name: strings.TrimSpace(cr.Name), State: cr.Status, Conclusion: cr.Conclusion,
			URL: firstNonEmpty(cr.DetailsURL, cr.HTMLURL), ID: cr.ID, NodeID: cr.NodeID, Head: strings.TrimSpace(cr.HeadSHA),
		}
		if cr.App != nil && cr.App.ID > 0 {
			id := cr.App.ID
			check.AppID = &id
		}
		check.Since = checkSince(cr.Status, cr.StartedAt, cr.CompletedAt)
		return []PRActivityCheck{check}
	case "workflow_job":
		job := p.WorkflowJob
		if job == nil || strings.TrimSpace(job.Name) == "" {
			return nil
		}
		id := githubActionsAppID
		check := PRActivityCheck{
			Name: strings.TrimSpace(job.Name), State: job.Status, Conclusion: job.Conclusion,
			URL: job.HTMLURL, ID: job.ID, NodeID: job.NodeID, AppID: &id, Head: strings.TrimSpace(job.HeadSHA),
		}
		check.Since = checkSince(job.Status, job.StartedAt, job.CompletedAt)
		return []PRActivityCheck{check}
	case "status":
		if strings.TrimSpace(p.Context) == "" {
			return nil
		}
		check := PRActivityCheck{
			Name: strings.TrimSpace(p.Context), State: "completed", URL: p.TargetURL,
			ID: p.ID, NodeID: p.NodeID, Head: strings.TrimSpace(p.SHA), Kind: "commit_status",
		}
		switch p.State {
		case "success":
			check.Conclusion = "success"
		case "failure", "error":
			check.Conclusion = "failure"
		case "pending":
			check.State = "pending"
		default:
			check.State = "unknown"
		}
		if at := firstTime(p.UpdatedAt, p.CreatedAt); at != nil {
			check.Since = at
		}
		return []PRActivityCheck{check}
	}
	return nil
}

func checkSince(status string, started, completed *time.Time) *time.Time {
	if status == "completed" && completed != nil && !completed.IsZero() {
		t := completed.UTC()
		return &t
	}
	if started != nil && !started.IsZero() {
		t := started.UTC()
		return &t
	}
	return nil
}

func firstTime(ts ...*time.Time) *time.Time {
	for _, t := range ts {
		if t != nil && !t.IsZero() {
			u := t.UTC()
			return &u
		}
	}
	return nil
}

// applyWebhookCIEvidence is HandleWebhookEvent's CI step: it applies the
// payload's checks to the head's set and, when no listing has covered the
// head, schedules one coalesced re-read of its check runs. numbers are the
// open PRs the delivery resolved to; the first one's base branch names the
// required-check policy the re-read resolves.
func (c *Client) applyWebhookCIEvidence(event string, p webhookEventPayload, configured string, numbers []int) *WebhookCIEvidence {
	if !IsWebhookCIEvent(event) {
		return nil
	}
	head := webhookHeadSHA(event, p)
	if head == "" && p.WorkflowJob != nil {
		head = strings.TrimSpace(p.WorkflowJob.HeadSHA)
	}
	if head == "" {
		return nil
	}
	repo := p.Repository.FullName
	sharedWebhookTracker().recordCIEvent()
	out := &WebhookCIEvidence{HeadSHA: head, PRs: append([]int(nil), numbers...)}
	store := sharedCIEvidence()
	checks := webhookCIChecks(event, p)
	if len(checks) > 0 {
		out.Applied = store.applyEvent(repo, head, checks)
		sharedWebhookTracker().recordCIApplied(out.Applied)
	} else {
		store.touch(repo, head)
	}
	if len(numbers) > 0 && !store.hasListed(repo, head) {
		target := ciRefetchTarget{repo: configured, fullName: repo, head: head, number: numbers[0]}
		for _, alias := range webhookRepoAliases(configured, repo) {
			if pr, _, ok := sharedPRDetailCache.peek(alias, numbers[0]); ok {
				target.baseRef = pr.GetBase().GetRef()
				break
			}
		}
		out.Refetch = c.ciRefetcher().enqueue(target)
	}
	if hook := ciEvidenceHook(); hook != nil && (out.Applied > 0) {
		hook(repo, head)
	}
	return out
}

// CIEvidenceHook is told when a head's CI evidence changed (a delivery was
// applied or a re-read landed) so the dashboard can re-derive the row.
type CIEvidenceHook func(repo, head string)

var (
	ciEvidenceHookMu sync.RWMutex
	ciEvidenceHookFn CIEvidenceHook
)

// SetCIEvidenceHook installs (or, with nil, removes) the process-wide hook.
func SetCIEvidenceHook(fn CIEvidenceHook) {
	ciEvidenceHookMu.Lock()
	ciEvidenceHookFn = fn
	ciEvidenceHookMu.Unlock()
}

func ciEvidenceHook() CIEvidenceHook {
	ciEvidenceHookMu.RLock()
	defer ciEvidenceHookMu.RUnlock()
	return ciEvidenceHookFn
}

// ciRefetchTarget is one head whose check runs are to be listed once.
type ciRefetchTarget struct {
	repo     string // as configured (the detail cache's key)
	fullName string // owner/name from the payload
	head     string
	number   int
	baseRef  string
}

// ciRefetchTimer is the one-shot debounce timer; tests hand-fire it.
type ciRefetchTimer interface{ Stop() bool }

// ciRefetcher coalesces head re-reads: a check fan-out of twenty deliveries
// on a fresh head costs one listing, not twenty.
type ciRefetcher struct {
	mu       sync.Mutex
	pending  map[ciHeadKey]ciRefetchTarget
	timer    ciRefetchTimer
	debounce time.Duration
	newTimer func(d time.Duration, f func()) ciRefetchTimer
	fetch    func(ctx context.Context, t ciRefetchTarget) error
	ctx      func() context.Context
	// flushes counts debounce windows that fired (tests).
	flushes int
}

const defaultCIRefetchDebounce = 2 * time.Second

// webhookCIRefetchRESTCaller tags the check-run listings made on behalf of
// CI deliveries so /api/gh-rate-limits lists them on their own.
const webhookCIRefetchRESTCaller = "hive:webhook_ci_refetch"

func (c *Client) ciRefetcher() *ciRefetcher {
	c.ciRefetchOnce.Do(func() {
		if c.ciRefetch == nil {
			c.ciRefetch = &ciRefetcher{}
		}
		r := c.ciRefetch
		if r.pending == nil {
			r.pending = map[ciHeadKey]ciRefetchTarget{}
		}
		if r.debounce <= 0 {
			r.debounce = defaultCIRefetchDebounce
		}
		if r.newTimer == nil {
			r.newTimer = func(d time.Duration, f func()) ciRefetchTimer { return time.AfterFunc(d, f) }
		}
		if r.fetch == nil {
			r.fetch = c.refetchHeadCI
		}
		if r.ctx == nil {
			r.ctx = context.Background
		}
	})
	return c.ciRefetch
}

// enqueue queues one head; a head already waiting keeps its slot (the PR
// number and base ref of the first delivery). It reports whether the head
// is now pending.
func (r *ciRefetcher) enqueue(t ciRefetchTarget) bool {
	if r == nil || t.head == "" || t.number <= 0 {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := ciEvidenceKey(t.fullName, t.head)
	if cur, ok := r.pending[key]; ok {
		if cur.baseRef == "" && t.baseRef != "" {
			cur.baseRef = t.baseRef
			r.pending[key] = cur
		}
		return true
	}
	r.pending[key] = t
	if r.timer == nil {
		r.timer = r.newTimer(r.debounce, r.flush)
	}
	return true
}

func (r *ciRefetcher) pendingCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// flush lists every pending head once. Heads enqueued while the listings
// run start a new window.
func (r *ciRefetcher) flush() {
	r.mu.Lock()
	targets := make([]ciRefetchTarget, 0, len(r.pending))
	for _, t := range r.pending {
		targets = append(targets, t)
	}
	r.pending = map[ciHeadKey]ciRefetchTarget{}
	r.timer = nil
	r.flushes++
	fetch, ctxFn := r.fetch, r.ctx
	r.mu.Unlock()
	if fetch == nil {
		return
	}
	ctx := context.Background()
	if ctxFn != nil {
		if c := ctxFn(); c != nil {
			ctx = c
		}
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].fullName != targets[j].fullName {
			return targets[i].fullName < targets[j].fullName
		}
		return targets[i].number < targets[j].number
	})
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		_ = fetch(ctx, t)
	}
}

// refetchHeadCI lists the head's check runs once, resolves the base branch's
// required-check policy (one cached GraphQL query per repo@branch) and the
// commit statuses it needs, records the result as the head's listing and
// tells the hook. It refuses to spend when the REST budget is critical or
// GitHub is pacing the hive; the next eval cycle is then the backstop.
func (c *Client) refetchHeadCI(ctx context.Context, t ciRefetchTarget) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	if mode, _ := c.APIBudgetMode(); mode == APIBudgetCritical {
		return ErrAPIBudgetCritical
	}
	if RateLimitCautionEngaged() {
		return ErrRateLimitCaution
	}
	owner, name := c.splitRepo(firstNonEmpty(t.fullName, t.repo))
	ctx = WithRESTCaller(ctx, webhookCIRefetchRESTCaller)
	var all []*gh.CheckRun
	options := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, name, t.head, options)
		if err != nil || runs == nil || runs.CheckRuns == nil {
			c.logger.Warn("webhook CI refetch: failed to list check runs", "repo", owner+"/"+name, "pr", t.number, "head", t.head, "error", err)
			if err == nil {
				err = ErrNoGitHubClient
			}
			return err
		}
		all = append(all, runs.CheckRuns...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		options.Page = resp.NextPage
	}
	at := sharedWebhookTracker().now().UTC()
	activity := prCIActivity(t.head, "github-check-runs", activityCheckRunsByNameAndApp(checkRunsOnHead(all, t.head)), &at)
	if t.baseRef != "" {
		pr := &PullRequest{Repo: owner + "/" + name, Number: t.number, HeadSHA: t.head, BaseRef: t.baseRef, CIActivity: activity}
		newProtectionCollector(c).attachRequiredChecks(ctx, pr)
	}
	sharedCIEvidence().recordListed(owner+"/"+name, t.head, activity)
	sharedWebhookTracker().recordCIRefetches(1)
	if hook := ciEvidenceHook(); hook != nil {
		hook(owner+"/"+name, t.head)
	}
	return nil
}

// FlushWebhookCIRefetch runs any pending head re-reads now, synchronously,
// instead of waiting out the debounce. The governor calls it before a
// webhook-wake cycle; tests use it in place of a timer.
func (c *Client) FlushWebhookCIRefetch() {
	if c == nil || c.ciRefetch == nil {
		return
	}
	r := c.ciRefetcher()
	r.mu.Lock()
	if len(r.pending) == 0 {
		r.mu.Unlock()
		return
	}
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	r.flush()
}

// RecordCIEvidenceForTest applies checks to repo's head as a delivery would,
// so tests outside this package can drive the tracker's merged read.
func RecordCIEvidenceForTest(repo, head string, checks []PRActivityCheck) int {
	return sharedCIEvidence().applyEvent(repo, head, checks)
}

// RecordCIListingForTest records a complete check-run listing for repo's
// head, as the enrich path or a webhook re-read would, so tests can exercise
// the payload-only delivery path that needs no re-read.
func RecordCIListingForTest(repo, head string, activity *PRCIActivity) {
	sharedCIEvidence().recordListed(repo, head, activity)
}
