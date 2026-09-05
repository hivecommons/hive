package automerge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// Transport is the small surface the sweep needs from the GitHub transport
// client. The policy state lives in this package; the client only supplies API
// access and transport-scoped metadata.
type Transport interface {
	GoGitHub() *gh.Client
	Repositories() []string
	SplitRepo(repo string) (owner, repoName string)
	AutoMergeLabel() string
	AppBotLogin() string
	IsExemptLabels(labels []string) bool
	RecordPRMergedAudit(repo string, number int, method, sha string)
}

// Options carries policy dependencies owned by the caller.
type Options struct {
	Logger           *slog.Logger
	MergerAuthorizer MergerAuthorizer
	RequiredChecks   map[string]bool
	ApprovalDesk     hgithub.ApprovalDeskHook
	MutationBoundary effects.Boundary
}

// Engine owns the automerge sweep policy state.
type Engine struct {
	transport Transport
	gh        *gh.Client
	logger    *slog.Logger

	mergerAuthzMu sync.RWMutex
	mergerAuthz   MergerAuthorizer

	requiredChecksMu sync.RWMutex
	requiredChecks   map[string]bool

	approvalDesk hgithub.ApprovalDeskHook
	mutation     effects.Boundary
}

// New returns an automerge sweep engine over a GitHub transport client.
func New(transport Transport, opts Options) *Engine {
	var ghClient *gh.Client
	if transport != nil {
		ghClient = transport.GoGitHub()
	}
	e := &Engine{
		transport:      transport,
		gh:             ghClient,
		logger:         opts.Logger,
		mergerAuthz:    opts.MergerAuthorizer,
		requiredChecks: opts.RequiredChecks,
		approvalDesk:   opts.ApprovalDesk,
		mutation:       opts.MutationBoundary,
	}
	if e.mutation == nil {
		if provider, ok := transport.(interface{ MutationBoundary() effects.Boundary }); ok {
			e.mutation = provider.MutationBoundary()
		}
	}
	return e
}

func (c *Engine) ready() bool {
	return c != nil && c.transport != nil && c.gh != nil
}

// SweepQueuedAutoMerges consumes queued automerge requests using a one-shot engine.
func SweepQueuedAutoMerges(ctx context.Context, transport Transport, opts Options, sweepOpts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	return New(transport, opts).SweepQueuedAutoMerges(ctx, sweepOpts)
}

// StartSelfAuthoredAutoMergeSweep starts the self-authored sweep using caller-owned options.
func StartSelfAuthoredAutoMergeSweep(ctx context.Context, transport Transport, maxMerges int, acmmAllowed bool, acmmLevel *int, opts Options) {
	New(transport, opts).StartSelfAuthoredAutoMergeSweep(ctx, maxMerges, acmmAllowed, acmmLevel)
}

// selfMergeMinACMMLevel mirrors config.SelfMergeMinACMMLevel. It is duplicated
// rather than imported so this package keeps no dependency on pkg/config
// (hivecommons/hive#5953 phase 1); config_parity_test.go pins the two equal so
// they cannot drift silently.
const selfMergeMinACMMLevel = 6

// SelfMergeMinACMMLevel is the minimum ACMM level for self-authored automerge.
const SelfMergeMinACMMLevel = selfMergeMinACMMLevel

const DefaultAutoMergeSweepMaxMerges = 3

// selfAuthoredAutoMergeSweepInterval is how often
// StartSelfAuthoredAutoMergeSweep re-scans the App's own open PRs. Matches
// the human-queue merge-request watcher's cadence (mergeRequestPollInterval):
// merges are latency-sensitive (an eligible PR should land quickly) but a
// tight loop risks GitHub secondary rate limits across every managed repo.
const selfAuthoredAutoMergeSweepInterval = 10 * time.Second

// selfAuthoredSweepBudgetShare caps the fraction of the App's hourly REST
// budget this sweep alone may consume. The sweep is a background nicety; the
// agents' own gh calls, the governor's eval cycle, the merge-eligible writer
// and the fix loops all draw on the same allowance and must not be starved.
const selfAuthoredSweepBudgetShare = 0.25

// githubAppHourlyRateLimit is the GitHub App installation REST allowance the
// interval is sized against.
const githubAppHourlyRateLimit = 6900

// selfAuthoredSweepCandidateAllowance is the per-tick request allowance for
// per-CANDIDATE calls, on top of the one list call per repo. Listing is not the
// whole cost of a tick: every open App-authored non-draft PR costs a
// PullRequests.Get in trySweepSelfAuthoredPR, plus a second re-verify Get on
// the ones that reach the merge step. Sizing the interval on repo count alone
// therefore understates a tick — on the hive this was measured on, candidate
// Gets outnumbered list calls whenever the App had a backlog of open PRs.
//
// This is an ALLOWANCE, not a measurement: candidates vary tick to tick, and a
// static budget that covers the common case beats a dynamic ticker for
// reviewability. A hive holding more open App PRs than this simply runs
// slightly hotter within its share; the share itself still bounds the damage.
const selfAuthoredSweepCandidateAllowance = 32

// selfAuthoredSweepInterval sizes the sweep tick so a hive with many repos
// cannot exhaust its GitHub rate limit just by looking for merge candidates.
//
// Cost per tick is roughly repos + candidates: one list call per configured
// repo, plus one Get per open App-authored PR (and a re-verify Get per merge).
// The fixed 10s tick scaled none of this. A 45-repo hive issued 45 x 360 =
// 16,200 list requests/hour against a 6,900/hour limit — 2.3x over on the list
// calls alone, before candidate Gets and before a single agent made a call.
// Observed live: the sweep 403'd continuously, go-github then short-circuited
// every request until the recorded reset ("not making remote request"), and
// the App's own PRs stopped merging entirely.
//
// Small hives are unaffected: the fixed interval remains the floor, so a hive
// with a handful of repos still sweeps every 10s exactly as before.
// selfAuthoredSweepSmallHiveRepos: at or below this many repos the fixed 10s
// tick is kept as-is. The budget-share math above would slow even a 1-repo hive
// (1 list + the candidate allowance per tick lands it over a 25% share at 10s),
// but a small hive's ABSOLUTE spend is comfortably inside the 6,900/hour limit
// — the share exists to stop many-repo hives starving everything else, a
// failure mode small hives cannot produce. Keeping their tick unchanged also
// keeps this change a no-op for the common quick-start deployment.
const selfAuthoredSweepSmallHiveRepos = 4

func selfAuthoredSweepInterval(repos int) time.Duration {
	if repos <= selfAuthoredSweepSmallHiveRepos {
		return selfAuthoredAutoMergeSweepInterval
	}
	budget := float64(githubAppHourlyRateLimit) * selfAuthoredSweepBudgetShare
	perTick := float64(repos + selfAuthoredSweepCandidateAllowance)
	seconds := perTick * 3600.0 / budget
	interval := time.Duration(seconds * float64(time.Second))
	if interval < selfAuthoredAutoMergeSweepInterval {
		return selfAuthoredAutoMergeSweepInterval
	}
	return interval.Round(time.Second)
}

const (
	autoMergeReasonNoHiveQueueApproval        = "no-hive-queue-approval"
	autoMergeReasonNoAppBotLogin              = "no-app-bot-login"
	autoMergeReasonUntrustedQueueApproval     = "untrusted-hive-queue-approval"
	autoMergeReasonUntrustedMerger            = "untrusted-merger"
	autoMergeReasonNoMergerAuthz              = "no-merger-authorizer"
	autoMergeWarnNoAppBotLogin                = "automerge sweep disabled: no GitHub App bot login configured"
	autoMergeWarnUntrustedQueueApproval       = "rejected untrusted Hive auto-merge queue approval"
	autoMergeWarnUntrustedMerger              = "rejected Hive auto-merge queued by an untrusted actor"
	autoMergeWarnNoMergerAuthz                = "automerge sweep disabled: no trusted-merger authorizer configured"
	autoMergeNoAppBotLoginOperatorRemediation = "Hive has no usable GitHub App, so App-authorship cannot be verified and auto-merge is disabled"
	autoMergeNoMergerAuthzRemediation         = "Hive cannot classify who queued the merge, so auto-merge is disabled (fail-closed)"
)

var hiveQueueReviewRE = regexp.MustCompile(`(?i)^Approved by @([A-Za-z0-9-]+) for Hive auto-merge on green CI\.`)

type AutoMergeSweepOptions struct {
	MaxMerges int
	Audit     func(AutoMergeSweepEvent)
}

// MergerAuthorizer reports whether login is trusted to QUEUE a merge — i.e.
// holds at least config.RoleMerger in the hive's authorized-users allowlist.
//
// SECURITY (audit F3). The queue-time role check lives in the dashboard handler
// (requireMergerOrOwnerRole), but the sweep is a SEPARATE authority that runs a
// minute later off nothing but the label and the App-authored approval body. It
// re-derives the queuer's login from that body and used to merge on it
// unconditionally, so anything that could get the merger-queue label applied
// got its PR merged regardless of who asked. Re-verifying the role HERE is what
// makes the merger tier real at the point the merge actually happens.
//
// Returns false for an unknown/unclassifiable login: a nil authorizer or an
// unresolvable actor must never merge (fail CLOSED).
type MergerAuthorizer func(login string) bool

// SetMergerAuthorizer installs the trusted-merger gate consulted by
// SweepQueuedAutoMerges. nil fails closed — the sweep merges nothing.
func (c *Engine) SetMergerAuthorizer(fn MergerAuthorizer) {
	if c == nil {
		return
	}
	c.mergerAuthzMu.Lock()
	defer c.mergerAuthzMu.Unlock()
	c.mergerAuthz = fn
}

// SetAttributionHooks forwards test audit hooks to transports that support them.
func (c *Engine) SetAttributionHooks(hooks hgithub.AttributionHooks) {
	if c == nil {
		return
	}
	if setter, ok := c.transport.(interface {
		SetAttributionHooks(hgithub.AttributionHooks)
	}); ok {
		setter.SetAttributionHooks(hooks)
	}
}

// SetRequiredChecks installs the config-declared required-status-check set
// (config.AutoMergeConfig.RequiredCheckSet) consulted by commitGreen before
// it ever calls GitHub's branch-protection API. nil/empty clears it, meaning
// "not config-declared" — commitGreen then falls back to the API and, if that
// also fails, to the isMetaCheck/isIgnorableCICheck allowlist. Safe to call
// repeatedly (e.g. on every config reload); the sweep goroutine reads the
// installed value through requiredChecksMu.
func (c *Engine) SetRequiredChecks(set map[string]bool) {
	if c == nil {
		return
	}
	c.requiredChecksMu.Lock()
	defer c.requiredChecksMu.Unlock()
	c.requiredChecks = set
}

// SetAutoMergeLabel updates the underlying transport label when it supports the setter.
func (c *Engine) SetAutoMergeLabel(label string) {
	if c == nil {
		return
	}
	if setter, ok := c.transport.(interface{ SetAutoMergeLabel(string) }); ok {
		setter.SetAutoMergeLabel(label)
	}
}

// configRequiredChecks returns the currently installed config-declared
// required-check set and whether one is installed. Mirrors isTrustedMerger's
// nil-safe read pattern for c.mergerAuthz.
func (c *Engine) configRequiredChecks() (map[string]bool, bool) {
	if c == nil {
		return nil, false
	}
	c.requiredChecksMu.RLock()
	defer c.requiredChecksMu.RUnlock()
	if len(c.requiredChecks) == 0 {
		return nil, false
	}
	return c.requiredChecks, true
}

// isTrustedMerger reports whether login may queue a merge. Fails CLOSED.
func (c *Engine) isTrustedMerger(login string) (allowed, configured bool) {
	if c == nil {
		return false, false
	}
	c.mergerAuthzMu.RLock()
	fn := c.mergerAuthz
	c.mergerAuthzMu.RUnlock()
	if fn == nil {
		return false, false
	}
	if strings.TrimSpace(login) == "" {
		return false, true
	}
	return fn(login), true
}

func (c *Engine) consultApprovalDesk(ctx context.Context, req hgithub.ApprovalDeskRequest) (bool, string) {
	if c == nil || c.approvalDesk == nil {
		return true, ""
	}
	allow, reason := c.approvalDesk(ctx, req)
	if !allow && reason == "" {
		reason = "approval-desk-withheld"
	}
	return allow, reason
}

type AutoMergeSweepEvent struct {
	Repo     string
	Number   int
	Author   string
	QueuedBy string
	HeadSHA  string
	MergeSHA string
	Label    string
}

type AutoMergeSweepResult struct {
	Merged  []AutoMergeSweepEvent
	Seen    int
	Skipped int
}

type hiveQueueApproval struct {
	QueuedBy string
	HeadSHA  string
}

// SweepQueuedAutoMerges consumes the configured Hive merger-queue label. It
// only squashes open, labelled, non-draft PRs in managed repos after GitHub
// reports them mergeable, commit statuses/check-runs are green, the latest
// Hive App-authored queue approval proves the queuer is not the PR author, and
// the queuer is a TRUSTED merger (audit F3 — see MergerAuthorizer). Without an
// authorizer installed the sweep fails closed and merges nothing.
func (c *Engine) SweepQueuedAutoMerges(ctx context.Context, opts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	if !c.ready() {
		return nil, hgithub.ErrNoGitHubClient
	}
	maxMerges := opts.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultAutoMergeSweepMaxMerges
	}
	label := c.transport.AutoMergeLabel()
	result := &AutoMergeSweepResult{}
	noAppBotLoginWarned := false
	noMergerAuthzWarned := false

	for _, repo := range c.transport.Repositories() {
		if len(result.Merged) >= maxMerges {
			break
		}
		owner, repoName := c.transport.SplitRepo(repo)
		issues, err := c.listQueuedPullRequestIssues(ctx, owner, repoName, label)
		if err != nil {
			return result, err
		}
		for _, issue := range issues {
			if len(result.Merged) >= maxMerges {
				break
			}
			if issue == nil || !issue.IsPullRequest() {
				continue
			}
			result.Seen++
			event, reason, err := c.trySweepQueuedPR(ctx, repo, owner, repoName, issue.GetNumber(), label)
			if err != nil {
				c.warn("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason, "error", err)
				result.Skipped++
				continue
			}
			if reason == autoMergeReasonNoAppBotLogin {
				if !noAppBotLoginWarned {
					c.warn(autoMergeWarnNoAppBotLogin, "repo", repo, "pr", issue.GetNumber(), "reason", reason, "cause", autoMergeNoAppBotLoginOperatorRemediation)
					noAppBotLoginWarned = true
				}
				result.Skipped++
				continue
			}
			if reason == autoMergeReasonNoMergerAuthz {
				if !noMergerAuthzWarned {
					c.warn(autoMergeWarnNoMergerAuthz, "repo", repo, "pr", issue.GetNumber(), "reason", reason, "cause", autoMergeNoMergerAuthzRemediation)
					noMergerAuthzWarned = true
				}
				result.Skipped++
				continue
			}
			if reason != "" {
				c.info("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason)
				result.Skipped++
				continue
			}
			result.Merged = append(result.Merged, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
		}
	}
	return result, nil
}

// SweepSelfAuthoredAutoMerges merges the App's OWN open PRs directly on green
// CI, without a human "Approved ... for Hive auto-merge" queue review and
// without waiting on tide.
//
// Why this must exist as a SEPARATE path from SweepQueuedAutoMerges: Prow
// structurally forbids self-approval — tide requires lgtm+approved labels
// from a reviewer distinct from the PR author, and the author here is always
// the App itself. A human queuer can supply that for someone else's PR (the
// existing sweep above), but nobody can supply it for the App's own PR: the
// App cannot review its own work, and asking a human to rubber-stamp every
// App PR defeats the point of automation. So the App must self-merge
// directly over the GitHub REST API (squash), the same bypass-tide mechanism
// SweepQueuedAutoMerges already uses for tide-pending/unstable states (see
// mergeableFromState) — this path just skips the human-queue-approval lookup
// entirely rather than needing one.
//
// Every OTHER safety property is identical to the human queue: mergeability
// (mergeableFromState), green required checks (commitGreen), and a head-SHA
// re-check immediately before the merge call so a push landing between
// enumeration and merge can never be squashed unreviewed — mirrored below via
// re-fetching the PR right before calling Merge and comparing SHAs, the same
// pattern trySweepQueuedPR uses via the queue-approval's recorded HeadSHA.
// There is no queuedBy in this path, so the author==queuedBy self-merge-ban
// in trySweepQueuedPR simply does not apply — there is no queuer to compare
// against.
func (c *Engine) SweepSelfAuthoredAutoMerges(ctx context.Context, opts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	if !c.ready() {
		return nil, hgithub.ErrNoGitHubClient
	}
	maxMerges := opts.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultAutoMergeSweepMaxMerges
	}
	result := &AutoMergeSweepResult{}
	if strings.TrimSpace(c.transport.AppBotLogin()) == "" {
		// No usable App identity: there is no "self" to authenticate PRs as
		// App-authored, so this sweep has nothing safe to do. Warn once per
		// call (matching the human-queue sweep's per-call warn cadence) rather
		// than per-repo, since the cause is hive-wide, not per-repo.
		c.warn(autoMergeWarnNoAppBotLogin, "reason", autoMergeReasonNoAppBotLogin, "cause", autoMergeNoAppBotLoginOperatorRemediation)
		return result, nil
	}

	for _, repo := range c.transport.Repositories() {
		if len(result.Merged) >= maxMerges {
			break
		}
		owner, repoName := c.transport.SplitRepo(repo)
		prs, err := c.listOpenAppAuthoredPullRequests(ctx, owner, repoName)
		if err != nil {
			return result, err
		}
		for _, pr := range prs {
			if len(result.Merged) >= maxMerges {
				break
			}
			result.Seen++
			event, reason, err := c.trySweepSelfAuthoredPR(ctx, repo, owner, repoName, pr.GetNumber())
			if err != nil {
				c.warn("self-authored automerge sweep skipped PR", "repo", repo, "pr", pr.GetNumber(), "reason", reason, "error", err)
				result.Skipped++
				continue
			}
			if reason != "" {
				c.info("self-authored automerge sweep skipped PR", "repo", repo, "pr", pr.GetNumber(), "reason", reason)
				result.Skipped++
				continue
			}
			result.Merged = append(result.Merged, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
		}
	}
	return result, nil
}

// StartSelfAuthoredAutoMergeSweep runs a loop that periodically calls
// SweepSelfAuthoredAutoMerges. It returns immediately; the loop runs until ctx
// is cancelled. A nil client is a no-op. maxMerges is passed straight through
// to AutoMergeSweepOptions.MaxMerges (<=0 falls back to
// DefaultAutoMergeSweepMaxMerges there). Mirrors
// StartMergeRequestWatcher/StartPRRequestWatcher's own-ticker-goroutine
// pattern so all three App-identity-dependent watchers share one shape.
//
// acmmAllowed is the caller-computed
// config.AutoMergeConfig.SelfAuthoredAutoMergeAllowed(acmmLevel) result (both
// the auto_merge.self_authored flag AND the hive's ACMM level gate self-merge
// authority — see config.SelfMergeMinACMMLevel). When false the loop is never
// started at all: an ACMM L4/L5 hive (l4.md/l5.md both forbid the App
// merging its own PRs) must not self-merge, matching console's L6 hive which
// is unaffected and keeps self-merging as before.
// refreshRateLimitCache re-reads GitHub's real rate limits and writes them back
// into the go-github client's cache.
//
// WHY THIS IS NEEDED. go-github refuses requests PRE-EMPTIVELY: once it has seen
// Remaining==0 it returns a synthetic 403 ("not making remote request") for
// every call until the cached Reset time passes, without contacting GitHub.
// That cache lives on the Client and is only updated by responses the Client
// itself receives.
//
// Hive's App client is created ONCE (NewClientFromApp) while appTransport
// injects a freshly minted INSTALLATION TOKEN per request, and installation
// tokens rotate roughly hourly. A new token gets a new allowance — but the
// Client's cache still says Remaining==0 with the old token's reset, so
// go-github keeps refusing requests the new token could happily serve.
//
// Observed live: the dashboard reported core remaining=6613 of 6900 while every
// sweep tick failed with "API rate limit of 6900 still exceeded", and the fleet
// consumed ZERO requests over six minutes — not rate-limited, just refusing.
// Merges stalled for the remainder of the window each time.
//
// GET /rate_limit does not count against any limit, and RateLimitService.Get
// writes the result back into the client's cache, so this is a cheap, exact
// correction rather than a guess.
func (c *Engine) refreshRateLimitCache(ctx context.Context) {
	if c == nil || c.gh == nil {
		return
	}
	if _, _, err := c.gh.RateLimit.Get(ctx); err != nil {
		c.warn("could not refresh rate-limit cache", "error", err)
		return
	}
	c.info("refreshed rate-limit cache after a pre-emptive rate-limit refusal")
}

// isRateLimited reports whether err is a primary/secondary rate-limit error,
// including go-github's pre-emptive synthetic one.
func isRateLimited(err error) bool {
	var rl *gh.RateLimitError
	if errors.As(err, &rl) {
		return true
	}
	var ab *gh.AbuseRateLimitError
	return errors.As(err, &ab)
}

func (c *Engine) StartSelfAuthoredAutoMergeSweep(ctx context.Context, maxMerges int, acmmAllowed bool, acmmLevel *int) {
	if !c.ready() {
		return
	}
	if !acmmAllowed {
		level := "unset"
		if acmmLevel != nil {
			level = fmt.Sprintf("%d", *acmmLevel)
		}
		c.info("self-authored auto-merge sweep disabled: acmm_level below minimum (or auto_merge.self_authored is off)",
			"acmm_level", level, "min_acmm_level", selfMergeMinACMMLevel)
		return
	}
	interval := selfAuthoredSweepInterval(len(c.transport.Repositories()))
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := c.SweepSelfAuthoredAutoMerges(ctx, AutoMergeSweepOptions{MaxMerges: maxMerges}); err != nil {
					c.warn("self-authored automerge sweep failed", "error", err)
					// A rate-limit refusal may be go-github's cached verdict
					// from a token that has since rotated. Re-read the real
					// limits (free, and it updates that cache) so the next tick
					// is decided on current truth instead of repeating a stale
					// refusal for the rest of the window.
					if isRateLimited(err) {
						c.refreshRateLimitCache(ctx)
					}
				}
			}
		}
	}()
	c.info("self-authored automerge sweep started", "interval", interval, "repos", len(c.transport.Repositories()))
}

// listOpenAppAuthoredPullRequests returns every open, non-draft PR in owner/repo
// authored by the App bot login. Uses the PR list endpoint (not issue search)
// because the caller needs PullRequest objects (head SHA, mergeable state)
// for every candidate, not just issue metadata.
func (c *Engine) listOpenAppAuthoredPullRequests(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error) {
	opts := &gh.PullRequestListOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var out []*gh.PullRequest
	for {
		prs, resp, err := c.gh.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing open PRs for %s/%s: %w", owner, repo, err)
		}
		for _, pr := range prs {
			if pr.GetDraft() {
				continue
			}
			if !strings.EqualFold(hgithub.SafeGetLogin(pr.GetUser()), c.transport.AppBotLogin()) {
				continue
			}
			out = append(out, pr)
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// trySweepSelfAuthoredPR evaluates and, if eligible, merges one App-authored
// PR. Re-fetches the PR immediately before merging to re-verify the head SHA
// against the one that was evaluated as green — the same
// evaluated-then-re-verified-at-merge-time safety property trySweepQueuedPR
// gets from the queue approval's recorded HeadSHA, just without a stored
// approval record to compare against (there is no queue step in this path).
func (c *Engine) trySweepSelfAuthoredPR(ctx context.Context, displayRepo, owner, repo string, number int) (AutoMergeSweepEvent, string, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr", err
	}
	if !strings.EqualFold(pr.GetState(), "open") {
		return AutoMergeSweepEvent{}, "closed", nil
	}
	if pr.GetDraft() {
		return AutoMergeSweepEvent{}, "draft", nil
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	if !strings.EqualFold(author, c.transport.AppBotLogin()) {
		// Not the App's own PR: this path never touches non-App-authored PRs,
		// matching the human-queue sweep's untouched behavior for PRs it does
		// not own. Defense in depth — listOpenAppAuthoredPullRequests already
		// filtered on author, but a PR can change hands (rare, but GitHub
		// permits transferring PR authorship attribution in some flows) between
		// listing and evaluating it here.
		return AutoMergeSweepEvent{}, "not-app-authored", nil
	}
	// Hold and do-not-merge labels must gate this path too (#5589): the
	// enumeration hold gate lives upstream in fetchPRs, but this sweep lists
	// PRs independently, so without this check a hold label — including one
	// the hold guard re-applied because the branch moved while hold-gated —
	// would not stop the App from squashing its own PR on green.
	selfLabels := labelNames(pr.Labels)
	if hgithub.HasHoldLabel(selfLabels) {
		return AutoMergeSweepEvent{}, "held", nil
	}
	if c.transport.IsExemptLabels(selfLabels) {
		return AutoMergeSweepEvent{}, "exempt-label", nil
	}

	evaluatedHeadSHA := ""
	if pr.GetHead() != nil {
		evaluatedHeadSHA = pr.GetHead().GetSHA()
	}
	if evaluatedHeadSHA == "" {
		return AutoMergeSweepEvent{}, "missing-head-sha", nil
	}

	mergeable := hgithub.MergeableFromState(pr.GetMergeableState(), pr.Mergeable)
	if mergeable != hgithub.MergeableYes {
		return AutoMergeSweepEvent{}, "not-mergeable", nil
	}
	baseBranch := ""
	if pr.GetBase() != nil {
		baseBranch = pr.GetBase().GetRef()
	}
	green, reason, err := c.commitGreen(ctx, owner, repo, baseBranch, evaluatedHeadSHA)
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		return AutoMergeSweepEvent{}, reason, nil
	}

	// Approval desk (RFC #4000). Consulted AFTER the sweep's own eligibility
	// checks so the desk only ever sees requests the legacy gate already
	// permits — it can withhold a merge, never widen authority beyond what
	// SelfAuthoredAutoMergeAllowed already granted upstream. No-op when no hook
	// is installed, which is the default; see automerge_desk.go.
	if allow, deskReason := c.consultApprovalDesk(ctx, hgithub.ApprovalDeskRequest{
		Kind:        hgithub.ApprovalDeskKindSelfMerge,
		Repo:        displayRepo,
		Number:      number,
		Author:      author,
		Title:       pr.GetTitle(),
		Labels:      labelNames(pr.Labels),
		ChecksGreen: true, // commitGreen returned green immediately above
		HeadSHA:     evaluatedHeadSHA,
	}); !allow {
		return AutoMergeSweepEvent{}, deskReason, nil
	}

	// Re-verify the head SHA immediately before merging: a push landing
	// between the green-check above and the merge call below must never be
	// squashed without having gone through commitGreen itself.
	current, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr-recheck", err
	}
	currentHeadSHA := ""
	if current.GetHead() != nil {
		currentHeadSHA = current.GetHead().GetSHA()
	}
	if currentHeadSHA == "" || currentHeadSHA != evaluatedHeadSHA {
		return AutoMergeSweepEvent{}, "head-changed-since-eval", nil
	}

	var mergeResult *gh.PullRequestMergeResult
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestMerge,
		Target: fmt.Sprintf("%d", number),
		Actor:  "automerge",
		Inputs: map[string]string{"method": "squash", "expect_sha": evaluatedHeadSHA, "lane": "self-authored"},
	}, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		mergeResult, _, apiErr = c.gh.PullRequests.Merge(ctx, owner, repo, number, "", &gh.PullRequestOptions{
			SHA:         evaluatedHeadSHA,
			MergeMethod: "squash",
		})
		if apiErr != nil {
			return effects.Result{}, apiErr
		}
		return effects.Result{Provenance: mergeResult.GetSHA()}, nil
	})
	if err != nil {
		return AutoMergeSweepEvent{}, "merge-failed", err
	}
	if !mergeResult.GetMerged() {
		return AutoMergeSweepEvent{}, "merge-not-applied", nil
	}
	// Audit the merge like MergePR does (pullrequest.go): the activity
	// collector counts pr_merged entries from this trail, and the /fleet L6
	// health verdict is judged on them. When the fleet's merges moved to this
	// sweep, the unaudited path made merging hives read as "no merge in Nd"
	// red on /fleet (observed live on kubestellar/console, 2026-08-26).
	c.transport.RecordPRMergedAudit(owner+"/"+repo, number, "squash", mergeResult.GetSHA())
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		QueuedBy: "", // no queuer in the self-authored path — the App merges its own PR
		HeadSHA:  evaluatedHeadSHA,
		MergeSHA: mergeResult.GetSHA(),
	}
	c.info("self-authored automerge sweep merged PR", "repo", displayRepo, "pr", number, "author", author, "merge_sha", event.MergeSHA)
	return event, "", nil
}

func (c *Engine) listQueuedPullRequestIssues(ctx context.Context, owner, repo, label string) ([]*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{label},
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var all []*gh.Issue
	for {
		issues, resp, err := c.gh.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing queued PRs for %s/%s: %w", owner, repo, err)
		}
		all = append(all, issues...)
		if resp.NextPage == 0 {
			return all, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

func (c *Engine) trySweepQueuedPR(ctx context.Context, displayRepo, owner, repo string, number int, label string) (AutoMergeSweepEvent, string, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr", err
	}
	if !strings.EqualFold(pr.GetState(), "open") {
		return AutoMergeSweepEvent{}, "closed", nil
	}
	if pr.GetDraft() {
		return AutoMergeSweepEvent{}, "draft", nil
	}
	labels := hgithub.ExtractPRLabels(pr.Labels)
	if !hasLabel(labels, label) {
		return AutoMergeSweepEvent{}, "label-removed", nil
	}
	// Hold labels outrank the merger queue (#5589): a hold applied AFTER a
	// merger queued the PR — including the hold guard re-applying one because
	// the branch moved while hold-gated — must stop the sweep, not race it.
	if hgithub.HasHoldLabel(labels) {
		return AutoMergeSweepEvent{}, "held", nil
	}
	if c.transport.IsExemptLabels(labels) {
		return AutoMergeSweepEvent{}, "exempt-label", nil
	}

	author := hgithub.SafeGetLogin(pr.GetUser())
	headSHA := ""
	if pr.GetHead() != nil {
		headSHA = pr.GetHead().GetSHA()
	}
	if headSHA == "" {
		return AutoMergeSweepEvent{}, "missing-head-sha", nil
	}
	if strings.TrimSpace(c.transport.AppBotLogin()) == "" {
		return AutoMergeSweepEvent{}, autoMergeReasonNoAppBotLogin, nil
	}
	approval, ok, reason, err := c.latestHiveQueueApproval(ctx, owner, repo, number)
	if err != nil {
		return AutoMergeSweepEvent{}, "queue-approval-check", err
	}
	if !ok {
		if reason != "" {
			return AutoMergeSweepEvent{}, reason, nil
		}
		return AutoMergeSweepEvent{}, autoMergeReasonNoHiveQueueApproval, nil
	}
	if approval.HeadSHA == "" {
		if err := c.invalidateQueuedAutoMerge(ctx, owner, repo, number, label, "Hive auto-merge approval is missing a reviewed head SHA — re-queue required."); err != nil {
			return AutoMergeSweepEvent{}, "queue-approval-missing-head", err
		}
		return AutoMergeSweepEvent{}, "queue-approval-missing-head", nil
	}
	if approval.HeadSHA != headSHA {
		if err := c.invalidateQueuedAutoMerge(ctx, owner, repo, number, label, "Hive auto-merge approval head changed since approval — re-queue required."); err != nil {
			return AutoMergeSweepEvent{}, "queue-approval-head-changed", err
		}
		return AutoMergeSweepEvent{}, "queue-approval-head-changed", nil
	}
	queuedBy := approval.QueuedBy
	if strings.EqualFold(author, queuedBy) {
		return AutoMergeSweepEvent{}, "self-merge-ban", nil
	}
	// SECURITY (audit F3): the self-merge ban above only proves queuer !=
	// author. It is defeated by a sockpuppet — a second account queues and
	// approves the first account's work — and on its own it lets ANY actor who
	// can get the merger-queue label applied merge anything. Re-verify the
	// merger tier here, at the point the merge actually happens, rather than
	// trusting the queue-time check in the dashboard handler.
	trusted, configured := c.isTrustedMerger(queuedBy)
	if !configured {
		return AutoMergeSweepEvent{}, autoMergeReasonNoMergerAuthz, nil
	}
	if !trusted {
		c.warn(autoMergeWarnUntrustedMerger, "owner", owner, "repo", repo, "pr", number,
			"queued_by", queuedBy, "author", author)
		return AutoMergeSweepEvent{}, autoMergeReasonUntrustedMerger, nil
	}

	mergeable := hgithub.MergeableFromState(pr.GetMergeableState(), pr.Mergeable)
	if mergeable != hgithub.MergeableYes {
		return AutoMergeSweepEvent{}, "not-mergeable", nil
	}
	baseBranch := ""
	if pr.GetBase() != nil {
		baseBranch = pr.GetBase().GetRef()
	}
	green, reason, err := c.commitGreen(ctx, owner, repo, baseBranch, headSHA)
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		return AutoMergeSweepEvent{}, reason, nil
	}

	// Approval desk (RFC #4000) for the trusted human merge-queue lane.
	// Consulted only after the legacy queue approval, trusted-merger,
	// mergeability, and green-check gates pass, so enabling the desk can record
	// or withhold this operation but cannot widen merge authority.
	if allow, deskReason := c.consultApprovalDesk(ctx, hgithub.ApprovalDeskRequest{
		Kind:        hgithub.ApprovalDeskKindQueuedMerge,
		Repo:        displayRepo,
		Number:      number,
		Author:      author,
		Title:       pr.GetTitle(),
		Labels:      labels,
		ChecksGreen: true,
		HeadSHA:     headSHA,
	}); !allow {
		return AutoMergeSweepEvent{}, deskReason, nil
	}

	var mergeResult *gh.PullRequestMergeResult
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestMerge,
		Target: fmt.Sprintf("%d", number),
		Actor:  "automerge",
		Inputs: map[string]string{"method": "squash", "expect_sha": headSHA, "lane": "queued"},
	}, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		mergeResult, _, apiErr = c.gh.PullRequests.Merge(ctx, owner, repo, number, "", &gh.PullRequestOptions{
			SHA:         headSHA,
			MergeMethod: "squash",
		})
		if apiErr != nil {
			return effects.Result{}, apiErr
		}
		return effects.Result{Provenance: mergeResult.GetSHA()}, nil
	})
	if err != nil {
		return AutoMergeSweepEvent{}, "merge-failed", err
	}
	if !mergeResult.GetMerged() {
		return AutoMergeSweepEvent{}, "merge-not-applied", nil
	}
	// Same audit obligation as the self-authored path above: pr_merged on
	// the trail is what makes this merge count as hive output.
	c.transport.RecordPRMergedAudit(owner+"/"+repo, number, "squash", mergeResult.GetSHA())
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		QueuedBy: queuedBy,
		HeadSHA:  headSHA,
		MergeSHA: mergeResult.GetSHA(),
		Label:    label,
	}
	c.info("automerge sweep merged PR", "repo", displayRepo, "pr", number, "queued_by", queuedBy, "merge_sha", event.MergeSHA)
	return event, "", nil
}

func (c *Engine) latestHiveQueueApproval(ctx context.Context, owner, repo string, number int) (hiveQueueApproval, bool, string, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := hiveQueueApproval{}
	untrusted := false
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return hiveQueueApproval{}, false, "", err
		}
		for _, review := range reviews {
			if !strings.EqualFold(review.GetState(), "APPROVED") {
				continue
			}
			queuedBy := parseHiveQueueReview(review.GetBody())
			if queuedBy == "" {
				continue
			}
			if !c.isHiveAppReviewAuthor(review) {
				untrusted = true
				c.warn(autoMergeWarnUntrustedQueueApproval, "owner", owner, "repo", repo, "pr", number, "review_author", hgithub.SafeGetLogin(review.GetUser()), "claimed_queued_by", queuedBy, "expected_app_bot", c.transport.AppBotLogin())
				continue
			}
			latest = hiveQueueApproval{QueuedBy: queuedBy, HeadSHA: review.GetCommitID()}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if latest.QueuedBy != "" {
		return latest, true, "", nil
	}
	if untrusted {
		return latest, false, autoMergeReasonUntrustedQueueApproval, nil
	}
	return latest, false, "", nil
}

func (c *Engine) isHiveAppReviewAuthor(review *gh.PullRequestReview) bool {
	if c == nil || review == nil || strings.TrimSpace(c.transport.AppBotLogin()) == "" {
		return false
	}
	return strings.EqualFold(hgithub.SafeGetLogin(review.GetUser()), c.transport.AppBotLogin())
}

func (c *Engine) invalidateQueuedAutoMerge(ctx context.Context, owner, repo string, number int, label, body string) error {
	if _, err := c.gh.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(label)); err != nil && !isGitHubStatus(err, http.StatusNotFound) {
		return fmt.Errorf("removing %s label: %w", label, err)
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		return fmt.Errorf("commenting on stale auto-merge approval: %w", err)
	}
	return nil
}

func parseHiveQueueReview(body string) string {
	matches := hiveQueueReviewRE.FindStringSubmatch(strings.TrimSpace(body))
	if len(matches) != 2 {
		return ""
	}

	return matches[1]
}

// commitGreen reports whether the head SHA is mergeable from a CI
// standpoint.
//
// Gating is REQUIRED-CHECKS-ONLY: commitGreen first asks which status
// contexts/check-run names are actually required for the target branch
// (requiredStatusCheckContexts). Any status or check-run whose context/name
// is NOT in that required set is skipped entirely, regardless of its state or
// conclusion — pending, failing, or cancelled non-required checks can never
// block self-merge. A required check still fully gates: pending blocks
// (return not-green, "pending" — the sweep must never squash a PR before its
// required CI has finished), and a failure/error/cancelled conclusion on a
// required check blocks too.
//
// Why required-only, not a hardcoded ignore-list: the previous approach
// (isMetaCheck/isIgnorableCICheck as an ALLOWLIST of names to ignore) is
// whack-a-mole against an open-ended set of non-required checks — #3611 added
// Playwright/Mobile Browser Tests/coverage-report/the chromium shard matrix,
// but "Detect untested files" (cancelled) still blocked #22471 and "Analyze
// (python)" (CodeQL, failure) still blocks #22450 because neither name was on
// the list. Every managed repo's ACTUAL required-checks set (e.g. console's
// main branch requires only "build-gate") is the one true source of what
// must be green; anything else is by definition non-required and must never
// wedge the queue.
//
// Where that required set comes from (see requiredStatusCheckContexts for
// the full precedence): config (auto_merge.required_checks) FIRST — the Hive
// App token lacks administration:read, so GitHub's branch-protection API
// (Repositories.GetRequiredStatusChecks, #3723) reliably errors in practice;
// the operator-declared config list needs no such scope. The API is tried
// only as a secondary source (in case the App ever does have that scope, or
// the branch is legitimately unprotected).
//
// Fail-closed fallback: if the required-checks set cannot be determined by
// EITHER config or the API (no config list, branch protection absent/erroring,
// no branch known, or the API call fails) this deliberately does NOT fall
// back to "ignore everything" — that would merge over a genuinely broken
// build the moment both sources are unavailable. Instead it falls back to the
// OLD isMetaCheck/isIgnorableCICheck allowlist behavior, so the previously-
// shipped conservative behavior is preserved rather than degrading to
// "always green".
func (c *Engine) commitGreen(ctx context.Context, owner, repo, branch, sha string) (bool, string, error) {
	required, requiredKnown := c.requiredStatusCheckContexts(ctx, owner, repo, branch)

	statusOpts := &gh.ListOptions{PerPage: 100}
	for {
		status, resp, err := c.gh.Repositories.GetCombinedStatus(ctx, owner, repo, sha, statusOpts)
		if err != nil {
			return false, "status-check", err
		}
		for _, s := range status.Statuses {
			ctxName := s.GetContext()
			if requiredKnown {
				// Required-checks-only gating: skip anything not on the
				// branch's actual required list, no matter its state.
				if !required[ctxName] {
					continue
				}
			} else if hgithub.IsMetaCheck(ctxName) {
				// Fail-closed fallback path (required set unavailable).
				continue
			}
			switch s.GetState() {
			case "success":
			case "pending":
				return false, "status-pending", nil
			default: // "failure", "error"
				if !requiredKnown && hgithub.IsIgnorableCICheck(ctxName) {
					continue
				}
				return false, "status-" + s.GetState(), nil
			}
		}
		if resp.NextPage == 0 {
			break
		}
		statusOpts.Page = resp.NextPage
	}

	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		checkRuns, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, opts)
		if err != nil {
			return false, "check-runs", err
		}
		for _, cr := range checkRuns.CheckRuns {
			name := cr.GetName()
			if requiredKnown {
				if !required[name] {
					continue
				}
			} else if hgithub.IsMetaCheck(name) {
				continue
			}
			if cr.GetStatus() != "completed" {
				if !requiredKnown && hgithub.IsIgnorableCICheck(name) {
					continue
				}
				return false, "check-pending", nil
			}
			switch cr.GetConclusion() {
			case "success", "neutral", "skipped":
			default:
				if !requiredKnown && hgithub.IsIgnorableCICheck(name) {
					continue
				}
				return false, "check-" + cr.GetConclusion(), nil
			}
		}
		if resp.NextPage == 0 {
			return true, "", nil
		}
		opts.Page = resp.NextPage
	}
}

// requiredStatusCheckContexts returns the set of status-check contexts /
// check-run names that are actually required for branch, and whether that
// set could be determined at all. It is the single source of truth
// commitGreen gates on: membership in this set is what makes a check
// "required" (must be green) versus ignorable (any state/conclusion, never
// blocks).
//
// Precedence (first available source wins):
//  1. Config: c.configRequiredChecks(), i.e. the operator-declared
//     auto_merge.required_checks list (config.AutoMergeConfig.RequiredCheckSet).
//     This needs NO GitHub API call and NO administration:read scope, so it
//     is checked first and is the primary path in practice — the Hive App's
//     token does not hold that scope, so path 2 below reliably errors.
//  2. GitHub's branch-protection API (Repositories.GetRequiredStatusChecks).
//     Kept as a fallback in case the App ever does have admin-read scope, or
//     the branch is legitimately unprotected (gh.ErrBranchNotProtected — a
//     repo with zero required checks is a valid, common state, NOT an
//     error, so that case returns requiredKnown=true with an empty set).
//  3. Neither available (no config list AND branch empty / API call failed
//     for a reason other than "not protected") → requiredKnown=false. The
//     caller must fall back to the OLD isMetaCheck/isIgnorableCICheck
//     allowlist rather than treating "we don't know the required set" as
//     "nothing is required" — see commitGreen's fail-closed comment.
func (c *Engine) requiredStatusCheckContexts(ctx context.Context, owner, repo, branch string) (map[string]bool, bool) {
	if set, ok := c.configRequiredChecks(); ok {
		return set, true
	}
	if strings.TrimSpace(branch) == "" {
		return nil, false
	}
	rsc, _, err := c.gh.Repositories.GetRequiredStatusChecks(ctx, owner, repo, branch)
	if err != nil {
		// gh.ErrBranchNotProtected means "this branch legitimately requires
		// nothing" — that IS a known, empty required set, not a failure to
		// determine it, so requiredKnown is true with an empty map (every
		// check is then non-required and ignorable).
		if errors.Is(err, gh.ErrBranchNotProtected) {
			return map[string]bool{}, true
		}
		return nil, false
	}
	if rsc == nil {
		return map[string]bool{}, true
	}
	required := make(map[string]bool)
	if rsc.Contexts != nil {
		for _, name := range *rsc.Contexts {
			required[name] = true
		}
	}
	if rsc.Checks != nil {
		for _, check := range *rsc.Checks {
			if check == nil {
				continue
			}
			required[check.Context] = true
		}
	}
	return required, true
}

func labelNames(labels []*gh.Label) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == nil {
			continue
		}
		if name := l.GetName(); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func hasLabel(labels []string, want string) bool {
	for _, label := range labels {
		if strings.EqualFold(label, want) {
			return true
		}
	}
	return false
}

func isGitHubStatus(err error, status int) bool {
	ghErr, ok := err.(*gh.ErrorResponse)
	return ok && ghErr.Response != nil && ghErr.Response.StatusCode == status
}

func (c *Engine) warn(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Warn(msg, args...)
	}
}

func (c *Engine) info(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Info(msg, args...)
	}
}
