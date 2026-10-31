package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/sentinel"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Sentinel sweep: evaluates open PRs in the watched repos with pkg/sentinel
// and, on a finding, posts one marker-stamped comment naming the finding(s)
// and reports the event for auditing. Untrusted authors also get the
// configured alert label, which is a hard hold for Hive approvals and
// auto-merge lanes. Trusted authors default to an informational notice without
// the label unless the operator opts into blocking them.
//
// Two entry points share the evaluation (#13379): SweepSentinelPR runs for
// one PR when a `pull_request` opened/synchronize/reopened webhook names it
// and fetches only that PR and its files; SweepSentinel is the reconcile
// cadence (SentinelReconcileInterval) that enumerates every open PR from the
// forgestate cache — or lists them when no cache is registered — and
// catches whatever a missed delivery left behind.
const (
	// SentinelReconcileInterval is the cadence of the full sweep. Webhook
	// deliveries carry the latency for new and updated PRs, so the full pass
	// only has to bound how long a PR whose delivery was lost sits
	// unevaluated.
	SentinelReconcileInterval = 15 * time.Minute

	// SentinelMarker stamps the alert comment so later ticks find it and do
	// not repost for the same head.
	SentinelMarker = "<!-- hive-sentinel -->"
	// SentinelConfigKey is the config block named in the comment.
	SentinelConfigKey = "sentinel"
	// sentinelAlertHeading opens a blocking alert comment; the trusted-author
	// notice uses sentinelNoticeHeading. The sweep reads the heading back to
	// tell "a maintainer cleared this alert" from "this head only ever got a
	// notice" (see sentinelClearedByMaintainer).
	sentinelAlertHeading  = "## ⚠️ Sentinel alert — maintainer review required"
	sentinelNoticeHeading = "## ℹ️ Sentinel notice — informational; author is trusted"
)

// SentinelSweepOptions parameterizes one pass.
type SentinelSweepOptions struct {
	Label            string
	LabelColor       string
	LabelDescription string
	Evaluator        sentinel.Config
	// TrustedAuthor classifies authors with the same predicate used by the
	// auto-merge exception lanes: the Hive App, trusted bots and owner-tier
	// authorized users. nil means no author is trusted.
	TrustedAuthor       func(repo, author string) SentinelAuthorTrust
	TrustedAuthorsBlock bool
	// RepoAllowed restricts the sweep; nil means every watched repo.
	RepoAllowed func(repo string) bool
	MaxActions  int
	// Audit receives one event per PR with a finding this pass.
	Audit func(SentinelSweepEvent)
	// PRCache enumerates open PRs for the full sweep. nil uses the
	// process-wide cache from SetSentinelPRCache; with neither, or for a
	// repo the cache has not reconciled yet, the sweep lists the repo.
	PRCache SentinelPRCache
	// PatchObserver, when non-nil, receives one observation per successful
	// patch fetch, before evaluation. It must return quickly and not block:
	// the sweep waits at most sentinelPatchObserverTimeout for it, recovers a
	// panic and logs it, and never lets it change the sweep's result. nil is a
	// strict no-op. PRs the sentinel skips (unchanged heads, exempt authors)
	// are not observed.
	PatchObserver func(SentinelPatchObservation)
}

// SentinelPatchObservation is what PatchObserver sees: the pushed head's
// patch files exactly as the sentinel fetched them. HiveIdentified is true
// only when the hive can name its own accounts and the author is one of them.
type SentinelPatchObservation struct {
	Repo           string
	Number         int
	HeadSHA        string
	Author         string
	HiveIdentified bool
	Private        bool
	Files          []sentinel.File
	At             time.Time
}

// sentinelPatchObserverTimeout bounds how long the sweep waits for an observer.
var sentinelPatchObserverTimeout = 2 * time.Second

// sentinelRepoPrivate reports whether the PR's repository is private. The PR
// payload's base.repo can be absent, which would read as public, so a missing
// one is resolved from the repository record; if that fails, assume private.
func (c *Client) sentinelRepoPrivate(ctx context.Context, displayRepo string, pr *gh.PullRequest) bool {
	if repo := pr.GetBase().GetRepo(); repo != nil && repo.Private != nil {
		return repo.GetPrivate()
	}
	owner, name, ok := strings.Cut(displayRepo, "/")
	if !ok || c.client == nil {
		return true
	}
	r, _, err := c.client.Repositories.Get(ctx, owner, name)
	if err != nil || r == nil || r.Private == nil {
		c.logger.Warn("sentinel repo privacy lookup failed; treating as private", "repo", displayRepo)
		return true
	}
	return r.GetPrivate()
}

func (c *Client) observeSentinelPatch(ctx context.Context, displayRepo string, pr *gh.PullRequest, files []sentinel.File, observe func(SentinelPatchObservation)) {
	obs := SentinelPatchObservation{
		Repo:           displayRepo,
		Number:         pr.GetNumber(),
		HeadSHA:        pr.GetHead().GetSHA(),
		Author:         pr.GetUser().GetLogin(),
		HiveIdentified: c.knowsOwnAccounts() && c.isHiveAccount(pr.GetUser()),
		Private:        c.sentinelRepoPrivate(ctx, displayRepo, pr),
		Files:          append([]sentinel.File(nil), files...),
		At:             time.Now(),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				c.logger.Warn("sentinel patch observer panicked", "repo", displayRepo, "number", obs.Number, "panic", fmt.Sprint(r))
			}
		}()
		observe(obs)
	}()
	timer := time.NewTimer(sentinelPatchObserverTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		c.logger.Warn("sentinel patch observer exceeded its time bound", "repo", displayRepo, "number", obs.Number)
	}
}

// SentinelPRCache is the forgestate view the full sweep enumerates from.
// *forgestate.Store satisfies it.
type SentinelPRCache interface {
	Snapshot(repo string) (forgestate.Snapshot, bool)
}

var (
	sentinelPRCacheMu sync.RWMutex
	sentinelPRCache   SentinelPRCache
)

// SetSentinelPRCache installs (or, with nil, removes) the process-wide
// forgestate cache the sentinel sweep enumerates open PRs from. It is the
// composition root's one line of wiring once a forgestate.Store runs.
func SetSentinelPRCache(c SentinelPRCache) {
	sentinelPRCacheMu.Lock()
	sentinelPRCache = c
	sentinelPRCacheMu.Unlock()
}

func (opts SentinelSweepOptions) prCache() SentinelPRCache {
	if opts.PRCache != nil {
		return opts.PRCache
	}
	sentinelPRCacheMu.RLock()
	defer sentinelPRCacheMu.RUnlock()
	return sentinelPRCache
}

// SentinelPREventActions are the `pull_request` webhook actions that run
// SweepSentinelPR: the PR is new or its head moved, so its files need
// evaluating. Every other action leaves the head as it was.
var SentinelPREventActions = map[string]bool{"opened": true, "synchronize": true, "reopened": true}

// SentinelPREventWanted reports whether a `pull_request` delivery with
// action should run the per-PR sentinel evaluation.
func SentinelPREventWanted(event, action string) bool {
	return event == "pull_request" && SentinelPREventActions[strings.ToLower(strings.TrimSpace(action))]
}

// SentinelAuthorTrust records whether the PR author is in a trusted lane and
// the human-readable reason included in the informational notice.
type SentinelAuthorTrust struct {
	Trusted bool
	Reason  string
}

// SentinelDecision is the single block-vs-notify decision for a finding.
type SentinelDecision struct {
	Label         string
	Block         bool
	TrustedReason string
}

// DecideSentinelAction resolves whether a finding should add the alert label
// (block) or only notify. Trusted authors default to notify-only; setting
// trustedAuthorsBlock opts them back into the blocking label.
func DecideSentinelAction(label string, trust SentinelAuthorTrust, trustedAuthorsBlock bool) SentinelDecision {
	label = strings.TrimSpace(label)
	if label == "" {
		label = "sentinel-alert"
	}
	reason := strings.TrimSpace(trust.Reason)
	return SentinelDecision{
		Label:         label,
		Block:         !trust.Trusted || trustedAuthorsBlock,
		TrustedReason: reason,
	}
}

// SentinelSweepEvent is one PR with a sentinel finding.
type SentinelSweepEvent struct {
	Repo          string
	Number        int
	Author        string
	Title         string
	HeadSHA       string
	Findings      []sentinel.Finding
	Blocked       bool
	TrustedReason string
	// Suppressed is set when the write-back gate refused the comment and
	// label: the finding is real and is recorded for the dashboard and
	// /api/sentinel, but nothing was written to GitHub.
	Suppressed bool
	// SuppressedReason is the gate's reason when Suppressed.
	SuppressedReason string
}

// Rules lists the triggered rule names.
func (e SentinelSweepEvent) Rules() []string { return sentinel.Rules(e.Findings) }

// SentinelSweepResult summarizes one pass.
type SentinelSweepResult struct {
	Seen       int
	Flagged    []SentinelSweepEvent
	Notified   []SentinelSweepEvent
	Remediated []SentinelSweepEvent
	// Suppressed lists findings the write-back gate kept off GitHub
	// (dashboard/API only).
	Suppressed []SentinelSweepEvent
	Skipped    int
	Errors     []string
	ReposHit   map[string]int
	// CachedRepos counts repos enumerated from the forgestate cache rather
	// than listed; ListedRepos counts the fallback.
	CachedRepos int
	ListedRepos int
}

func (r *SentinelSweepResult) actions() int {
	return len(r.Flagged) + len(r.Notified) + len(r.Remediated)
}

// sentinelHeadsSeen remembers the head SHA each PR was last evaluated at so
// an unchanged PR costs nothing on the cadence pass (no PR read, no files
// fetch). It is in-memory only: a restart or a second replica starts empty
// and re-evaluates every open PR once, so it is a cost optimisation, not the
// record of a maintainer's clearance — that lives on GitHub
// (sentinelClearedByMaintainer).
type sentinelHeadsSeen struct {
	mu   sync.Mutex
	seen map[string]string
}

// isSeen reports whether key was last evaluated at head without recording
// anything; unchanged records.
func (s *sentinelHeadsSeen) isSeen(key, head string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.seen[key]
	return ok && prev == head && head != ""
}

func (s *sentinelHeadsSeen) unchanged(key, head string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]string{}
	}
	if prev, ok := s.seen[key]; ok && prev == head && head != "" {
		return true
	}
	s.seen[key] = head
	return false
}

func sentinelKey(owner, repo string, number int) string {
	return strings.ToLower(owner+"/"+repo) + "#" + fmt.Sprint(number)
}

// sentinelSweepPrepared validates opts and returns the label, action cap and
// an empty result.
func (c *Client) sentinelSweepPrepared(opts SentinelSweepOptions) (string, int, *SentinelSweepResult, error) {
	if c == nil {
		return "", 0, nil, ErrNoGitHubClient
	}
	label := strings.TrimSpace(opts.Label)
	if label == "" {
		return "", 0, nil, fmt.Errorf("sentinel sweep: label is required")
	}
	maxActions := opts.MaxActions
	if maxActions <= 0 {
		maxActions = 20
	}
	return label, maxActions, &SentinelSweepResult{ReposHit: map[string]int{}}, nil
}

// SweepSentinel runs one full sentinel pass over the watched repos: the
// reconcile cadence. Open PRs come from the forgestate cache when one is
// registered and has reconciled the repo; a PR whose head the sweep already
// evaluated and that carries no alert label is skipped without any GitHub
// call, and every other candidate is read once and evaluated. Without a
// cache the repo is listed, as before.
func (c *Client) SweepSentinel(ctx context.Context, opts SentinelSweepOptions) (*SentinelSweepResult, error) {
	label, maxActions, result, err := c.sentinelSweepPrepared(opts)
	if err != nil {
		return nil, err
	}
	cache := opts.prCache()
	for _, repo := range c.getRepos() {
		if opts.RepoAllowed != nil && !opts.RepoAllowed(repo) {
			continue
		}
		if result.actions() >= maxActions {
			break
		}
		owner, repoName := c.splitRepo(repo)
		if cache != nil {
			if snap, ok := cache.Snapshot(repo); ok && snap.Ready {
				result.CachedRepos++
				c.sweepSentinelCached(ctx, result, repo, owner, repoName, snap, label, maxActions, opts)
				c.writebackSweepSummary(repo)
				continue
			}
		}
		result.ListedRepos++
		prs, err := c.listOpenPRsForSupersessionSweep(ctx, owner, repoName)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: list PRs: %v", repo, err))
			continue
		}
		for _, pr := range prs {
			if result.actions() >= maxActions {
				break
			}
			if pr == nil {
				continue
			}
			result.Seen++
			c.sentinelEvaluatePR(ctx, result, repo, owner, repoName, pr, label, opts)
		}
		c.writebackSweepSummary(repo)
	}
	return result, nil
}

// sweepSentinelCached evaluates the cached open PRs of one repo. Cached items
// carry number, head SHA and labels but not author, title or file count, so
// a candidate the heads map does not rule out is read once (GET pull) before
// trySentinelPR runs on it exactly as on a listed PR. A snapshot marked stale
// is still the right enumeration: it is the last good list, and the per-PR
// read confirms each candidate is still open.
func (c *Client) sweepSentinelCached(ctx context.Context, result *SentinelSweepResult, repo, owner, repoName string, snap forgestate.Snapshot, label string, maxActions int, opts SentinelSweepOptions) {
	numbers := make([]int, 0, len(snap.PRs))
	for n := range snap.PRs {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	for _, n := range numbers {
		if result.actions() >= maxActions {
			return
		}
		item := snap.PRs[n]
		result.Seen++
		if !item.HasLabel(label) && c.sentinelHeads.isSeen(sentinelKey(owner, repoName, n), item.HeadSHA) {
			result.Skipped++
			continue
		}
		pr, open, err := c.sentinelReadPR(ctx, owner, repoName, n)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s#%d: %v", repo, n, err))
			continue
		}
		if !open {
			result.Skipped++
			continue
		}
		c.sentinelEvaluatePR(ctx, result, repo, owner, repoName, pr, label, opts)
	}
}

// SweepSentinelPR evaluates one PR: the `pull_request` opened/synchronize/
// reopened webhook path. It reads that PR and, if its head is new, its
// files; nothing is listed. repo may be the configured slug or the
// delivery's full_name. A PR that is not open, or a repo the sweep does not
// cover, is skipped and reported in the result rather than as an error.
func (c *Client) SweepSentinelPR(ctx context.Context, repo string, number int, opts SentinelSweepOptions) (*SentinelSweepResult, error) {
	label, _, result, err := c.sentinelSweepPrepared(opts)
	if err != nil {
		return nil, err
	}
	if number <= 0 {
		return nil, fmt.Errorf("sentinel sweep: PR number is required")
	}
	configured, ok := c.webhookConfiguredRepo(repo)
	if !ok {
		result.Skipped++
		return result, nil
	}
	if opts.RepoAllowed != nil && !opts.RepoAllowed(configured) {
		result.Skipped++
		return result, nil
	}
	owner, repoName := c.splitRepo(configured)
	pr, open, err := c.sentinelReadPR(ctx, owner, repoName, number)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("%s#%d: %v", configured, number, err))
		return result, nil
	}
	result.Seen++
	if !open {
		result.Skipped++
		return result, nil
	}
	c.sentinelEvaluatePR(ctx, result, configured, owner, repoName, pr, label, opts)
	return result, nil
}

// sentinelReadPR is the one targeted read both cached paths make. open is
// false for a closed or merged PR.
func (c *Client) sentinelReadPR(ctx context.Context, owner, repo string, number int) (*gh.PullRequest, bool, error) {
	pr, _, err := c.client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, false, fmt.Errorf("reading PR: %w", err)
	}
	if pr == nil {
		return nil, false, fmt.Errorf("reading PR: empty response")
	}
	if pr.GetMerged() || (pr.State != nil && !strings.EqualFold(pr.GetState(), "open")) {
		return pr, false, nil
	}
	return pr, true, nil
}

// sentinelEvaluatePR runs trySentinelPR on one open PR and files the outcome
// into result; the caller has already counted the PR as seen. The detection
// and labelling logic is trySentinelPR's alone; every entry point ends here.
func (c *Client) sentinelEvaluatePR(ctx context.Context, result *SentinelSweepResult, repo, owner, repoName string, pr *gh.PullRequest, label string, opts SentinelSweepOptions) {
	event, acted, err := c.trySentinelPR(ctx, repo, owner, repoName, pr, label, opts)
	if err != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("%s#%d: %v", repo, pr.GetNumber(), err))
		return
	}
	if !acted {
		result.Skipped++
		return
	}
	event.Repo = repo
	c.recordReviewEvidenceSentinel(repo, pr, event.Findings, event.Blocked, time.Now())
	if event.Suppressed {
		result.Suppressed = append(result.Suppressed, event)
	} else if event.Blocked {
		result.Flagged = append(result.Flagged, event)
	} else if prHasLabel(pr, label) {
		result.Remediated = append(result.Remediated, event)
	} else {
		result.Notified = append(result.Notified, event)
	}
	result.ReposHit[repo]++
	if opts.Audit != nil {
		opts.Audit(event)
	}
}

func (c *Client) trySentinelPR(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, label string, opts SentinelSweepOptions) (SentinelSweepEvent, bool, error) {
	number := pr.GetNumber()
	head := pr.GetHead().GetSHA()
	author := pr.GetUser().GetLogin()
	trust := SentinelAuthorTrust{}
	if opts.TrustedAuthor != nil {
		trust = opts.TrustedAuthor(displayRepo, author)
	}
	decision := DecideSentinelAction(label, trust, opts.TrustedAuthorsBlock)
	hasAlertLabel := prHasLabel(pr, label)
	key := sentinelKey(owner, repo, number)
	if !hasAlertLabel && c.sentinelHeads.unchanged(key, head) {
		return SentinelSweepEvent{}, false, nil
	}
	if opts.Evaluator.Exempt(author) {
		return SentinelSweepEvent{}, false, nil
	}
	if hasAlertLabel && decision.Block {
		// Already alerted on; a maintainer has not cleared it yet.
		return SentinelSweepEvent{}, false, nil
	}
	files, err := listPRFilesWithPatch(ctx, c.client, owner, repo, pr)
	if err != nil {
		return SentinelSweepEvent{}, false, err
	}
	if opts.PatchObserver != nil {
		c.observeSentinelPatch(ctx, displayRepo, pr, files, opts.PatchObserver)
	}
	findings := sentinel.Evaluate(sentinel.PR{Author: author, Title: pr.GetTitle(), Files: files}, opts.Evaluator)
	if len(findings) == 0 {
		return SentinelSweepEvent{}, false, nil
	}
	event := SentinelSweepEvent{Number: number, Author: author, Title: pr.GetTitle(), HeadSHA: head, Findings: findings, Blocked: decision.Block, TrustedReason: decision.TrustedReason}
	// The write-back gate: a finding on a PR nobody opted in, below the
	// level the sentinel kind needs, is recorded (dashboard, /api/sentinel,
	// review evidence) but never commented or labelled (torch-spyre,
	// 2026-10-28). The marker-comment read is skipped too — no GitHub call.
	if d := c.WritebackAllowed(displayRepo, c.sentinelWritebackItem(pr), writeback.KindSentinel, SentinelComment(decision, head, author, findings)); !d.Allowed {
		event.Suppressed = true
		event.SuppressedReason = d.Reason
		return event, true, nil
	}
	marker := SentinelMarker + " " + head
	comment, posted, err := c.findOwnMarkerComment(ctx, owner, repo, number, marker)
	if err != nil {
		return SentinelSweepEvent{}, false, err
	}
	if decision.Block && !hasAlertLabel && sentinelClearedByMaintainer(comment, posted) {
		// The alert for this exact head is on the PR but the label is not:
		// only a maintainer removes it, and the comment promised "Hive will
		// not re-apply it unless new commits are pushed". The in-memory
		// heads map honoured that until a hub restart emptied it, after
		// which the first pass re-evaluated the unchanged head and
		// re-labelled the cleared PR (#11686, seen on #11614). The clearance is remembered
		// where it is durable: the marker comment itself.
		return SentinelSweepEvent{}, false, nil
	}
	if decision.Block && !hasAlertLabel && !posted {
		// No marker for this head: either it is new and owes an alert, or
		// the head moved (a Renovate rebase, say) between the alert and the
		// maintainer's review, and the maintainer removed the label with
		// this head already in place. The second case is a clearance of
		// this head, not of the one the marker names (#13602); re-alerting
		// would make the maintainer remove the same label twice. A
		// dashboard Approve bound to this head is the same clearance made
		// through the hive, which took the label off as itself.
		cleared, clearedBy, err := c.sentinelClearedAfterLatestPush(ctx, owner, repo, number, head, label)
		if err != nil {
			return SentinelSweepEvent{}, false, err
		}
		note := fmt.Sprintf(sentinelClearedNote, clearedBy, label)
		if !cleared {
			releasedBy, err := c.sentinelReleasedByDashboardApproval(ctx, owner, repo, number, head)
			if err != nil {
				return SentinelSweepEvent{}, false, err
			}
			cleared, note = releasedBy != "", fmt.Sprintf(sentinelReleasedNote, releasedBy, label)
		}
		if cleared {
			if err := c.recordSentinelClearance(ctx, owner, repo, number, note, SentinelComment(decision, head, author, findings)); err != nil {
				return SentinelSweepEvent{}, false, err
			}
			return SentinelSweepEvent{}, false, nil
		}
	}
	if !decision.Block {
		body := SentinelComment(decision, head, author, findings)
		if hasAlertLabel {
			if !posted {
				return SentinelSweepEvent{}, false, nil
			}
			resp, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, label) // writeback:gated
			c.noteWritebackOutcome(displayRepo, resp, err)
			if err != nil {
				return SentinelSweepEvent{}, false, fmt.Errorf("removing %s label from trusted author PR: %w", label, err)
			}
			if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, comment.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
				return SentinelSweepEvent{}, false, fmt.Errorf("updating sentinel notice: %w", err)
			}
			return event, true, nil
		}
		if !posted {
			_, resp, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}) // writeback:gated
			c.noteWritebackOutcome(displayRepo, resp, err)
			if err != nil {
				return SentinelSweepEvent{}, false, fmt.Errorf("posting sentinel notice: %w", err)
			}
		}
		return event, true, nil
	}
	if err := c.EnsureIssueLabel(ctx, owner+"/"+repo, label, opts.LabelColor, opts.LabelDescription); err != nil {
		return SentinelSweepEvent{}, false, fmt.Errorf("ensuring %s label: %w", label, err)
	}
	_, resp, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{label}) // writeback:gated
	c.noteWritebackOutcome(displayRepo, resp, err)
	if err != nil {
		return SentinelSweepEvent{}, false, fmt.Errorf("adding %s label: %w", label, err)
	}
	body := SentinelComment(decision, head, author, findings)
	if posted {
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, comment.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
			return SentinelSweepEvent{}, false, fmt.Errorf("updating sentinel alert: %w", err)
		}
	} else {
		if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
			return SentinelSweepEvent{}, false, fmt.Errorf("posting sentinel comment: %w", err)
		}
	}
	return event, true, nil
}

// sentinelWritebackItem is the gate's view of a PR read from GitHub.
func (c *Client) sentinelWritebackItem(pr *gh.PullRequest) writeback.Item {
	author := pr.GetUser().GetLogin()
	labels := make([]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		labels = append(labels, l.GetName())
	}
	hive := c.IsHiveAuthorLogin(author)
	return writeback.Item{Number: pr.GetNumber(), IsPR: true, Author: author, HiveAuthored: hive, Labels: labels, CreatedAt: pr.GetCreatedAt().Time}
}

// sentinelClearedByMaintainer reports whether the marker comment found for
// the PR's current head is a blocking alert. The sweep adds the label before
// it posts that comment and never removes the label itself on the blocking
// path, so "alert comment for this head present, label absent" can only mean a
// human removed it. A trusted-author notice for the same head does not count:
// if the operator later turns on trusted_authors_block the head still owes a
// label. The caller must have found the comment with findOwnMarkerComment:
// only a comment the hive itself wrote carries that meaning, since anyone
// who can comment on the PR can type the marker and the heading.
func sentinelClearedByMaintainer(comment *gh.IssueComment, posted bool) bool {
	if !posted || comment == nil {
		return false
	}
	return strings.Contains(comment.GetBody(), sentinelAlertHeading)
}

func prHasLabel(pr *gh.PullRequest, label string) bool {
	for _, l := range pr.Labels {
		if strings.EqualFold(l.GetName(), label) {
			return true
		}
	}
	return false
}

// listPRFilesWithPatch is ListPRChangedFiles plus the unified-diff patch the
// content rules inspect. It fails closed on an incomplete list for the same
// reason: a rule evaluated on a partial file set could miss the one file
// that matters.
func listPRFilesWithPatch(ctx context.Context, client *gh.Client, owner, repo string, pr *gh.PullRequest) ([]sentinel.File, error) {
	if client == nil || pr == nil {
		return nil, fmt.Errorf("listing PR files: no client or PR")
	}
	var files []sentinel.File
	fileOpts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := client.PullRequests.ListFiles(ctx, owner, repo, pr.GetNumber(), fileOpts)
		if err != nil {
			return nil, fmt.Errorf("listing PR files: %w", err)
		}
		for _, f := range page {
			files = append(files, sentinel.File{
				Path:      f.GetFilename(),
				Status:    f.GetStatus(),
				Additions: f.GetAdditions(),
				Deletions: f.GetDeletions(),
				Patch:     f.GetPatch(),
			})
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		fileOpts.Page = resp.NextPage
	}
	if reported := pr.GetChangedFiles(); reported > len(files) {
		return nil, fmt.Errorf("incomplete PR file list: GitHub reported %d changed files but API returned %d", reported, len(files))
	}
	return files, nil
}

func findMarkerComment(ctx context.Context, client *gh.Client, owner, repo string, number int, marker string) (*gh.IssueComment, bool, error) {
	return findMarkerCommentWhere(ctx, client, owner, repo, number, marker, nil)
}

// findOwnMarkerComment is findMarkerComment restricted to comments one of
// the hive's own accounts wrote. The sentinel reads state back from its
// marker comment (which head was alerted on, whether a maintainer cleared
// it), and any PR author can post a comment containing the marker text, so
// a comment of unverified authorship must not be read as the hive's. A hive
// that cannot name its own accounts keeps the legacy any-author rule, the
// same fallback markerAuthorTrusted applies to the label-approval markers.
func (c *Client) findOwnMarkerComment(ctx context.Context, owner, repo string, number int, marker string) (*gh.IssueComment, bool, error) {
	return findMarkerCommentWhere(ctx, c.client, owner, repo, number, marker, func(comment *gh.IssueComment) bool {
		return c.markerAuthorTrusted(comment.GetUser())
	})
}

func findMarkerCommentWhere(ctx context.Context, client *gh.Client, owner, repo string, number int, marker string, accept func(*gh.IssueComment) bool) (*gh.IssueComment, bool, error) {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, false, fmt.Errorf("listing comments for sentinel marker: %w", err)
		}
		for _, comment := range comments {
			if !strings.Contains(comment.GetBody(), marker) {
				continue
			}
			if accept != nil && !accept(comment) {
				continue
			}
			return comment, true, nil
		}
		if resp == nil || resp.NextPage == 0 {
			return nil, false, nil
		}
		opts.Page = resp.NextPage
	}
}

func hasMarkerComment(ctx context.Context, client *gh.Client, owner, repo string, number int, marker string) (bool, error) {
	_, found, err := findMarkerComment(ctx, client, owner, repo, number, marker)
	return found, err
}

// SentinelComment is the body of the alert/notice comment. The marker carries
// the head SHA so a new push gets a fresh comment while an unchanged head never
// gets a duplicate.
func SentinelComment(decision SentinelDecision, head, author string, findings []sentinel.Finding) string {
	var b strings.Builder
	b.WriteString(SentinelMarker + " " + head + "\n")
	if decision.Block {
		b.WriteString(sentinelAlertHeading + "\n\n")
	} else {
		reason := decision.TrustedReason
		if strings.TrimSpace(reason) == "" {
			reason = "trusted author"
		}
		fmt.Fprintf(&b, "%s (%s), merge is not blocked\n\n", sentinelNoticeHeading, reason)
	}
	fmt.Fprintf(&b, "Hive flagged this PR (author @%s, head `%s`) because it matches behaviors that can override security controls, escalate privileges or damage the codebase. This is a heuristic, not an accusation — a maintainer should confirm the change is intended before it merges.\n\n", author, shortSHA(head))
	for _, f := range findings {
		fmt.Fprintf(&b, "- **%s** — %s", f.Rule, f.Summary)
		if desc := sentinel.RuleDescriptions[f.Rule]; desc != "" {
			fmt.Fprintf(&b, " _(%s)_", desc)
		}
		b.WriteString("\n")
		paths := append([]string(nil), f.Paths...)
		sort.Strings(paths)
		for _, p := range paths {
			fmt.Fprintf(&b, "  - `%s`\n", p)
		}
	}
	if decision.Block {
		fmt.Fprintf(&b, "\nHive added the `%s` label. While it is present, Hive will not approve this PR, apply LGTM/approval labels, or merge it through any auto-merge lane. Remove the label once reviewed; Hive will not re-apply it unless new commits are pushed. Tune paths and behaviors under `%s` in hive.yaml or the dashboard Security tab.\n", decision.Label, SentinelConfigKey)
	} else {
		fmt.Fprintf(&b, "\nHive did not add the `%s` label because this author is trusted. The finding remains recorded for audit and the dashboard Security tab; set `%s.trusted_authors_block: true` to require the blocking label for trusted authors too.\n", decision.Label, SentinelConfigKey)
	}
	return b.String()
}
