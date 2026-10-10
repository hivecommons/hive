package automerge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
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
	// IsHeldLabels reports whether labels carry a hold the transport's owner
	// configured: the generic hold substrings plus the exact hive-scoped
	// dashboard pause label (`hive-pause/<hive-id>`). The sweep must use it
	// instead of the generic package predicate so a dashboard ⏸ Hold blocks
	// merges the same way it blocks enumeration (#8927).
	IsHeldLabels(labels []string) bool
	UpdateBranch(ctx context.Context, repo string, number int) error
	RecordPRMergedAudit(repo string, number int, method, sha, path string)
}

type levelHoldTransport interface {
	ReleaseLevelHoldIfEligible(ctx context.Context, owner, repo string, pr *gh.PullRequest) (bool, string, error)
}

// Options carries policy dependencies owned by the caller.
type Options struct {
	Logger           *slog.Logger
	MergerAuthorizer MergerAuthorizer
	RequiredChecks   map[string]bool
	// RequiredChecksForRepo returns the config-declared fallback required-check
	// set for owner/repo, including per-repo overrides.
	RequiredChecksForRepo func(repo string) (map[string]bool, bool)
	ApprovalDesk          hgithub.ApprovalDeskHook
	MutationBoundary      effects.Boundary
	// IntentGate is the intent-tier policy trySweepSelfAuthoredPR enforces
	// (#6258). nil installs no policy; see IntentGate for the semantics.
	IntentGate *IntentGate
	// HumanMergePaths returns the live auto_merge.human_merge_paths patterns
	// for an owner/repo. Every sweep lane refuses a PR touching one (#11038).
	// nil means no repo has any.
	HumanMergePaths func(repo string) []string
	// SelfAuthorizationHoldEnabled returns the live per-repo #5117 hold switch.
	// nil preserves the default-on behavior.
	SelfAuthorizationHoldEnabled      func(repo string) bool
	SelfAuthorizationHoldReleaseLimit int
	// RepoAutoMergeEnabled returns the live per-repo auto-merge switch. nil
	// preserves default-on behavior.
	RepoAutoMergeEnabled func(repo string) bool
	// TrustedBotAuthors returns the live lower-cased set of bot logins whose
	// PRs the self-authored sweep merges alongside the App's own (see
	// config.AutoMergeConfig.TrustedBotAuthors). nil means App-only.
	TrustedBotAuthors func() map[string]bool
	// TrustedAuthorPolicy returns the live, operator opt-in policy for the
	// human-authored tier. nil or disabled means the tier is off.
	TrustedAuthorPolicy func() TrustedAuthorPolicy
	// TrustedAuthorizer reports whether a PR author holds the policy's required
	// authorized-users role. nil fails closed.
	TrustedAuthorizer TrustedAuthorizer
	// SentinelLabel returns the configured sentinel alert label. The label also
	// travels through the transport's hold-label set, but the sweep names this
	// skip distinctly so operators can tell a sentinel block from an ordinary
	// hold in tick stats.
	SentinelLabel func() string
	// MinHeadAge is the youngest PR head commit age the sweep may merge when
	// the required-check set is not config-declared and fully green.
	MinHeadAge time.Duration
	Now        func() time.Time
}

// Engine owns the automerge sweep policy state.
type Engine struct {
	transport Transport
	gh        *gh.Client
	logger    *slog.Logger

	mergerAuthzMu sync.RWMutex
	mergerAuthz   MergerAuthorizer

	requiredChecksMu      sync.RWMutex
	requiredChecks        map[string]bool
	requiredChecksForRepo func(repo string) (map[string]bool, bool)

	approvalDesk hgithub.ApprovalDeskHook
	mutation     effects.Boundary

	intentGateMu sync.RWMutex
	intentGate   *IntentGate

	humanMergePathsMu sync.RWMutex
	humanMergePaths   func(repo string) []string

	selfAuthorizationHoldEnabled      func(repo string) bool
	selfAuthorizationHoldReleaseLimit int
	repoAutoMergeEnabled              func(repo string) bool
	trustedBotAuthors                 func() map[string]bool
	trustedAuthorPolicy               func() TrustedAuthorPolicy
	trustedAuthorizer                 TrustedAuthorizer
	sentinelLabel                     func() string
	minHeadAge                        time.Duration
	now                               func() time.Time
	evaluatedHeadsMu                  sync.Mutex
	evaluatedHeads                    map[string]string
	requiredChecksFallbackWarnedMu    sync.Mutex
	requiredChecksFallbackWarned      map[string]bool
}

// New returns an automerge sweep engine over a GitHub transport client.
func New(transport Transport, opts Options) *Engine {
	var ghClient *gh.Client
	if transport != nil {
		ghClient = transport.GoGitHub()
	}
	e := &Engine{
		transport:                         transport,
		gh:                                ghClient,
		logger:                            opts.Logger,
		mergerAuthz:                       opts.MergerAuthorizer,
		requiredChecks:                    opts.RequiredChecks,
		requiredChecksForRepo:             opts.RequiredChecksForRepo,
		approvalDesk:                      opts.ApprovalDesk,
		mutation:                          opts.MutationBoundary,
		intentGate:                        opts.IntentGate,
		humanMergePaths:                   opts.HumanMergePaths,
		selfAuthorizationHoldEnabled:      opts.SelfAuthorizationHoldEnabled,
		selfAuthorizationHoldReleaseLimit: opts.SelfAuthorizationHoldReleaseLimit,
		repoAutoMergeEnabled:              opts.RepoAutoMergeEnabled,
		trustedBotAuthors:                 opts.TrustedBotAuthors,
		trustedAuthorPolicy:               opts.TrustedAuthorPolicy,
		trustedAuthorizer:                 opts.TrustedAuthorizer,
		sentinelLabel:                     opts.SentinelLabel,
		minHeadAge:                        opts.MinHeadAge,
		now:                               opts.Now,
		evaluatedHeads:                    make(map[string]string),
		requiredChecksFallbackWarned:      make(map[string]bool),
	}
	if e.now == nil {
		e.now = time.Now
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

func (c *Engine) repoAutoMergeAllowed(repo string) bool {
	if c == nil || c.repoAutoMergeEnabled == nil {
		return true
	}
	return c.repoAutoMergeEnabled(repo)
}

// activeRepos is Repositories() minus the repos under an operator pause
// (#6203). The narrowing lives behind an optional transport capability, the
// same shape New uses for MutationBoundary: a transport that does not know
// about pauses (the sweep's own fakes) keeps its full repository list, so
// pause support is additive rather than a Transport-interface break.
func (c *Engine) activeRepos() []string {
	if c == nil || c.transport == nil {
		return nil
	}
	if provider, ok := c.transport.(interface{ ActiveRepositories() []string }); ok {
		return provider.ActiveRepositories()
	}
	return c.transport.Repositories()
}

// SweepQueuedAutoMerges consumes queued automerge requests using a one-shot engine.
func SweepQueuedAutoMerges(ctx context.Context, transport Transport, opts Options, sweepOpts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	return New(transport, opts).SweepQueuedAutoMerges(ctx, sweepOpts)
}

// StartSelfAuthoredAutoMergeSweep starts the self-authored sweep using caller-owned options.
// The returned channel closes once the sweep loop has exited (see the Engine
// method).
func StartSelfAuthoredAutoMergeSweep(ctx context.Context, transport Transport, maxMerges int, acmmAllowed bool, acmmLevel *int, opts Options) <-chan struct{} {
	return New(transport, opts).StartSelfAuthoredAutoMergeSweep(ctx, maxMerges, acmmAllowed, acmmLevel)
}

func SweepTrustedAuthorAutoMerges(ctx context.Context, transport Transport, opts Options, sweepOpts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	return New(transport, opts).SweepTrustedAuthorAutoMerges(ctx, sweepOpts)
}

// selfMergeMinACMMLevel mirrors config.SelfMergeMinACMMLevel. It is duplicated
// rather than imported so this package keeps no dependency on pkg/config
// (hivecommons/hive#5953 phase 1); config_parity_test.go pins the two equal so
// they cannot drift silently.
const selfMergeMinACMMLevel = 6

// SelfMergeMinACMMLevel is the minimum ACMM level for self-authored automerge.
const SelfMergeMinACMMLevel = selfMergeMinACMMLevel

const DefaultAutoMergeSweepMaxMerges = 3
const selfAuthoredSweepMaxBranchUpdates = 5

const releaseInProgressStatusContext = "release-in-progress"
const releaseInProgressMaxAge = 10 * time.Minute

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

// selfAuthoredSweepCandidateAllowance is the default per-tick request allowance
// for per-CANDIDATE calls, on top of the one list call per repo. Listing is not
// the whole cost of a tick: every open App-authored non-draft PR that survives
// cheap list-response gates costs a PullRequests.Get in trySweepSelfAuthoredPR,
// plus a second re-verify Get on the ones that reach the merge step. Sizing the
// interval on repo count alone therefore understates a tick.
//
// This is only the starting allowance. StartSelfAuthoredAutoMergeSweep adapts
// the next tick to the observed post-filter candidate count when a backlog is
// larger than this default, so a held/exempt backlog does not burn the budget
// and a genuinely large mergeable backlog slows proportionally.
const selfAuthoredSweepCandidateAllowance = 32

// selfAuthoredSweepMaxInterval bounds adaptive backoff so the self-authored
// sweep still makes progress even during unusually large candidate backlogs.
const selfAuthoredSweepMaxInterval = 15 * time.Minute

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
	return selfAuthoredSweepIntervalForCandidates(repos, selfAuthoredSweepCandidateAllowance)
}

func selfAuthoredSweepIntervalForCandidates(repos, candidates int) time.Duration {
	if repos <= selfAuthoredSweepSmallHiveRepos {
		return selfAuthoredAutoMergeSweepInterval
	}
	if candidates < selfAuthoredSweepCandidateAllowance {
		candidates = selfAuthoredSweepCandidateAllowance
	}
	budget := float64(githubAppHourlyRateLimit) * selfAuthoredSweepBudgetShare
	perTick := float64(repos + candidates)
	seconds := perTick * 3600.0 / budget
	interval := time.Duration(seconds * float64(time.Second))
	if interval < selfAuthoredAutoMergeSweepInterval {
		return selfAuthoredAutoMergeSweepInterval
	}
	if interval > selfAuthoredSweepMaxInterval {
		return selfAuthoredSweepMaxInterval
	}
	return interval.Round(time.Second)
}

func nextSelfAuthoredSweepInterval(repos int, current time.Duration, result *AutoMergeSweepResult) (time.Duration, bool) {
	if result == nil {
		return current, false
	}
	next := selfAuthoredSweepIntervalForCandidates(repos, result.Candidates)
	return next, next != current
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
var requiredChecksExpectedRE = regexp.MustCompile(`(?i)(required status check ["'][^"']+["'] is expected|\d+\s+of\s+\d+\s+required status checks are expected)`)

// mergeMethodFor returns the GitHub merge method the sweep should use for pr.
// Forward-merge PRs between release lines must land as true merge commits so
// the target line keeps the source line's ancestry; everything else keeps the
// repository's squash convention. The classification is shared with the
// hive-merge relay via hgithub.IsForwardMergePR.
func mergeMethodFor(pr *gh.PullRequest) string {
	if hgithub.IsForwardMergePR(pr) {
		return "merge"
	}
	return "squash"
}

type AutoMergeSweepOptions struct {
	MaxMerges int
	Audit     func(AutoMergeSweepEvent)
}

type TrustedAuthorPolicy struct {
	Enabled                 bool
	Repos                   map[string]bool
	RequireRole             string
	RequireGitHubPermission bool
	ExcludeLabels           map[string]bool
}

type TrustedAuthorDecision struct {
	Allowed bool
	Role    string
}

// TrustedAuthorizer reports whether a PR author holds the configured
// authorized-users role for the trusted-author tier. The concrete role is
// returned for audit comments. Unknown actors fail closed.
type TrustedAuthorizer func(login, requireRole string) TrustedAuthorDecision

type trustedAuthorPermission struct {
	Allowed bool
	Level   string
}

type expectedCheckCache struct {
	entries map[string]map[string]bool
}

type expectedCheckCacheContextKey struct{}

func newExpectedCheckCache() *expectedCheckCache {
	return &expectedCheckCache{entries: make(map[string]map[string]bool)}
}

func contextWithExpectedCheckCache(ctx context.Context, cache *expectedCheckCache) context.Context {
	if ctx == nil || cache == nil {
		return ctx
	}
	return context.WithValue(ctx, expectedCheckCacheContextKey{}, cache)
}

func expectedCheckCacheFromContext(ctx context.Context) *expectedCheckCache {
	if ctx == nil {
		return nil
	}
	cache, _ := ctx.Value(expectedCheckCacheContextKey{}).(*expectedCheckCache)
	return cache
}

func (c *Engine) evaluatedHeadKey(owner, repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(owner)) + "/" + strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

func (c *Engine) rememberEvaluatedHead(owner, repo string, number int, sha string) {
	if c == nil || strings.TrimSpace(sha) == "" {
		return
	}
	c.evaluatedHeadsMu.Lock()
	defer c.evaluatedHeadsMu.Unlock()
	if c.evaluatedHeads == nil {
		c.evaluatedHeads = make(map[string]string)
	}
	c.evaluatedHeads[c.evaluatedHeadKey(owner, repo, number)] = sha
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

// SetRequiredChecks installs the config-declared fallback required-status-check
// set (config.AutoMergeConfig.RequiredCheckSet). Branch protection is the
// authoritative source; this set is used only when protection cannot be read.
func (c *Engine) SetRequiredChecks(set map[string]bool) {
	if c == nil {
		return
	}
	c.requiredChecksMu.Lock()
	defer c.requiredChecksMu.Unlock()
	c.requiredChecks = set
	c.requiredChecksForRepo = nil
}

func (c *Engine) SetRequiredChecksForRepo(fn func(repo string) (map[string]bool, bool)) {
	if c == nil {
		return
	}
	c.requiredChecksMu.Lock()
	defer c.requiredChecksMu.Unlock()
	c.requiredChecksForRepo = fn
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
	return c.configRequiredChecksForRepo("")
}

func (c *Engine) configRequiredChecksForRepo(repo string) (map[string]bool, bool) {
	if c == nil {
		return nil, false
	}
	c.requiredChecksMu.RLock()
	fn := c.requiredChecksForRepo
	set := c.requiredChecks
	c.requiredChecksMu.RUnlock()
	if fn != nil {
		return fn(repo)
	}
	if len(set) == 0 {
		return nil, false
	}
	return set, true
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

func (c *Engine) currentTrustedAuthorPolicy() TrustedAuthorPolicy {
	if c == nil || c.trustedAuthorPolicy == nil {
		return TrustedAuthorPolicy{}
	}
	p := c.trustedAuthorPolicy()
	if p.RequireRole == "" {
		p.RequireRole = "merger"
	}
	if p.ExcludeLabels == nil {
		p.ExcludeLabels = map[string]bool{"hold": true, "do-not-merge": true, "needs-human": true}
	}
	return p
}

func (c *Engine) trustedAuthorDecision(login, requireRole string) TrustedAuthorDecision {
	if c == nil || c.trustedAuthorizer == nil || strings.TrimSpace(login) == "" {
		return TrustedAuthorDecision{}
	}
	return c.trustedAuthorizer(login, requireRole)
}

func (p TrustedAuthorPolicy) repoAllowed(repo string) bool {
	if !p.Enabled {
		return false
	}
	if len(p.Repos) == 0 {
		return true
	}
	return p.Repos[strings.ToLower(strings.TrimSpace(repo))]
}

func (p TrustedAuthorPolicy) excludedLabel(labels []string) string {
	for _, label := range labels {
		key := strings.ToLower(strings.TrimSpace(label))
		if p.ExcludeLabels[key] {
			return key
		}
	}
	return ""
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
	Tier     string
	// BranchUpdated is set on skipped events when the sweep successfully
	// queued a PR branch sync instead of merging this tick.
	BranchUpdated bool
}

type AutoMergeSweepResult struct {
	Merged          []AutoMergeSweepEvent
	Seen            int
	Skipped         int
	Candidates      int
	UpdatedBranches int
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
	expectedChecks := newExpectedCheckCache()
	ctx = contextWithExpectedCheckCache(ctx, expectedChecks)

	// activeRepos: an operator-paused repo receives no automerges (#6203). This
	// sweep is hive-driven, not kick-driven, so leaving it on Repositories()
	// would have kept merging into a repo during its release freeze.
	for _, repo := range c.activeRepos() {
		if len(result.Merged) >= maxMerges {
			break
		}
		if !c.repoAutoMergeAllowed(repo) {
			c.info("automerge sweep skipped repo", "repo", repo, "reason", "repo-auto-merge-disabled")
			continue
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
			if reason := c.prefilterQueuedIssue(issue, label); reason != "" {
				if reason == "held" || reason == "exempt-label" {
					c.debug("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason)
				} else {
					c.info("automerge sweep skipped PR", "repo", repo, "pr", issue.GetNumber(), "reason", reason)
				}
				result.Skipped++
				continue
			}
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
// (mergeableFromState), green required checks (commitGreen), the intent tier
// gate (selfMergeIntentGate, #6258 — the same refusal predicate
// writeMergeEligible applies before a PR can be queued), and a head-SHA
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

	// See the queued sweep above: paused repos are out of scope for automerge.
	selfAuthReleaseBudget := c.selfAuthorizationReleaseBudget()
	branchUpdateAttempts := 0
	expectedChecks := newExpectedCheckCache()
	ctx = contextWithExpectedCheckCache(ctx, expectedChecks)
	for _, repo := range c.activeRepos() {
		if len(result.Merged) >= maxMerges {
			break
		}
		if !c.repoAutoMergeAllowed(repo) {
			c.info("self-authored automerge sweep skipped repo", "repo", repo, "reason", "repo-auto-merge-disabled")
			continue
		}
		owner, repoName := c.transport.SplitRepo(repo)
		prs, err := c.listOpenPullRequests(ctx, owner, repoName)
		if err != nil {
			return result, err
		}
		repoSeen := 0
		repoSkipped := 0
		repoCandidates := 0
		repoMergedBefore := len(result.Merged)
		repoSkipReasons := make(map[string]int)
		for _, pr := range prs {
			if len(result.Merged) >= maxMerges {
				break
			}
			number := 0
			if pr != nil {
				number = pr.GetNumber()
			}
			result.Seen++
			repoSeen++
			// #5117 hold release must run before the prefilter: the
			// prefilter skips held PRs outright, and an eligible
			// self-authorization hold has to be released, not skipped.
			if selfAuthReleaseBudget > 0 && pr != nil && !c.selfAuthorizationHoldActive(repo) && c.isHeld(labelNames(pr.Labels)) {
				released, err := c.releaseSelfAuthorizationHoldIfEligible(ctx, repo, owner, repoName, number)
				if err != nil {
					c.warn("self-authored automerge sweep could not evaluate #5117 hold release", "repo", repo, "pr", number, "error", err)
					result.Skipped++
					repoSkipped++
					repoSkipReasons["self-authorization-release-error"]++
					continue
				}
				if released {
					selfAuthReleaseBudget--
					result.Skipped++
					repoSkipped++
					repoSkipReasons["self-authorization-hold-released"]++
					continue
				}
			}
			// Level-applied holds (#7060) must also be evaluated before the
			// generic prefilter skips held PRs outright. If a hold is still
			// required (or is not a level-applied hold), count it as the returned
			// skip reason; if it was released, skip this tick and let the next
			// sweep evaluate the now-unheld PR.
			if pr != nil && c.isHeld(labelNames(pr.Labels)) {
				if _, reason, err := c.releaseLevelHoldIfEligible(ctx, owner, repoName, pr); err != nil {
					c.warn("self-authored automerge sweep could not evaluate level hold release", "repo", repo, "pr", number, "reason", reason, "error", err)
					result.Skipped++
					repoSkipped++
					if reason == "" {
						reason = "level-hold-release-error"
					}
					repoSkipReasons[reason]++
					continue
				} else if reason != "" {
					if reason == "hold" {
						reason = "held"
					}
					result.Skipped++
					repoSkipped++
					repoSkipReasons[reason]++
					continue
				}
			}
			if reason := c.prefilterSelfAuthoredPR(pr); reason != "" {
				c.info("automerge skip", "repo", repo, "pr", number, "reason", reason)
				result.Skipped++
				repoSkipped++
				repoSkipReasons[reason]++
				continue
			}
			result.Candidates++
			repoCandidates++
			event, reason, err := c.trySweepSelfAuthoredPR(ctx, repo, owner, repoName, number, branchUpdateAttempts < selfAuthoredSweepMaxBranchUpdates)
			if event.BranchUpdated || reason == "updated-branch" || reason == "update-branch" {
				branchUpdateAttempts++
			}
			if err != nil {
				c.warn("self-authored automerge sweep skipped PR", "repo", repo, "pr", number, "reason", reason, "error", err)
				result.Skipped++
				repoSkipped++
				repoSkipReasons[reason]++
				continue
			}
			if event.BranchUpdated || reason == "updated-branch" {
				result.UpdatedBranches++
			}
			if reason != "" {
				c.info("automerge skip", "repo", repo, "pr", number, "reason", reason)
				result.Skipped++
				repoSkipped++
				repoSkipReasons[reason]++
				continue
			}
			result.Merged = append(result.Merged, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
		}
		if repoSeen > 0 || repoCandidates > 0 || len(result.Merged) > repoMergedBefore {
			args := []any{
				"repo", repo,
				"seen", repoSeen,
				"candidates", repoCandidates,
				"merged", len(result.Merged) - repoMergedBefore,
				"updated_branches", result.UpdatedBranches,
				"skipped", repoSkipped,
			}
			skipReasonOrder := []string{"label:hold", "label:needs-human", "label:needs-rebase", "held", "exempt-label", "draft", "closed", "not-app-authored", "missing-head-sha", "updated-branch", "conflicting", "not-mergeable"}
			if sentinelLabel := c.sentinelLabelName(); sentinelLabel != "" {
				skipReasonOrder = append([]string{sentinelLabel}, skipReasonOrder...)
			}
			for _, reason := range skipReasonOrder {
				if count := repoSkipReasons[reason]; count > 0 {
					args = append(args, reason, count)
				}
			}
			c.info("self-authored automerge sweep tick", args...)
		}
	}
	return result, nil
}

// SweepTrustedAuthorAutoMerges merges green PRs whose human author already
// holds the configured authorized-users role and, by default, GitHub
// repository write/maintain/admin permission. This is an operator opt-in tier:
// unlike App self-merge, a human PR has no structural self-approval problem, so
// the sweep must prove Hive is only executing a merge the author could perform
// themselves.
func (c *Engine) SweepTrustedAuthorAutoMerges(ctx context.Context, opts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	if !c.ready() {
		return nil, hgithub.ErrNoGitHubClient
	}
	policy := c.currentTrustedAuthorPolicy()
	result := &AutoMergeSweepResult{}
	if !policy.Enabled {
		return result, nil
	}
	maxMerges := opts.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultAutoMergeSweepMaxMerges
	}
	expectedChecks := newExpectedCheckCache()
	ctx = contextWithExpectedCheckCache(ctx, expectedChecks)
	for _, displayRepo := range c.activeRepos() {
		if len(result.Merged) >= maxMerges {
			break
		}
		if !policy.repoAllowed(displayRepo) {
			continue
		}
		if !c.repoAutoMergeAllowed(displayRepo) {
			c.info("trusted-author automerge sweep skipped repo", "repo", displayRepo, "reason", "repo-auto-merge-disabled")
			continue
		}
		owner, repoName := c.transport.SplitRepo(displayRepo)
		prs, err := c.listOpenPullRequests(ctx, owner, repoName)
		if err != nil {
			return result, err
		}
		for _, pr := range prs {
			if len(result.Merged) >= maxMerges {
				break
			}
			number := 0
			if pr != nil {
				number = pr.GetNumber()
			}
			result.Seen++
			if reason := c.prefilterTrustedAuthorPR(pr, policy); reason != "" {
				c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", reason, "tier", "trusted-author")
				result.Skipped++
				continue
			}
			result.Candidates++
			event, reason, err := c.trySweepTrustedAuthorPR(ctx, displayRepo, owner, repoName, number, policy)
			if err != nil {
				c.warn("trusted-author automerge sweep skipped PR", "repo", displayRepo, "pr", number, "reason", reason, "error", err)
				result.Skipped++
				continue
			}
			if reason != "" {
				c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", reason, "tier", "trusted-author")
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

// StartSelfAuthoredAutoMergeSweep starts the sweep loop in its own goroutine.
// The returned channel closes once that loop has exited after ctx is
// cancelled, including any sweep tick that was in flight, or immediately when
// the sweep does not start at all. A caller that restarts the sweep on a
// rebuilt client (#9621) waits on it so two sweeps never overlap.
func (c *Engine) StartSelfAuthoredAutoMergeSweep(ctx context.Context, maxMerges int, acmmAllowed bool, acmmLevel *int) <-chan struct{} {
	done := make(chan struct{})
	if !c.ready() {
		close(done)
		return done
	}
	if !acmmAllowed && !c.currentTrustedAuthorPolicy().Enabled {
		level := "unset"
		if acmmLevel != nil {
			level = fmt.Sprintf("%d", *acmmLevel)
		}
		c.info("self-authored auto-merge sweep disabled: acmm_level below minimum (or auto_merge.self_authored is off)",
			"acmm_level", level, "min_acmm_level", selfMergeMinACMMLevel)
		close(done)
		return done
	}
	repos := len(c.transport.Repositories())
	interval := selfAuthoredSweepInterval(repos)
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				result := &AutoMergeSweepResult{}
				if acmmAllowed {
					var err error
					result, err = c.SweepSelfAuthoredAutoMerges(ctx, AutoMergeSweepOptions{MaxMerges: maxMerges})
					if err != nil {
						c.warn("self-authored automerge sweep failed", "error", err)
						// A rate-limit refusal may be go-github's cached verdict
						// from a token that has since rotated. Re-read the real
						// limits (free, and it updates that cache) so the next tick
						// is decided on current truth instead of repeating a stale
						// refusal for the rest of the window.
						if isRateLimited(err) {
							c.refreshRateLimitCache(ctx)
						}
						continue
					}
				}
				remaining := maxMerges
				if remaining <= 0 {
					remaining = DefaultAutoMergeSweepMaxMerges
				}
				remaining -= len(result.Merged)
				if remaining > 0 && c.currentTrustedAuthorPolicy().Enabled {
					trustedResult, trustedErr := c.SweepTrustedAuthorAutoMerges(ctx, AutoMergeSweepOptions{MaxMerges: remaining})
					if trustedErr != nil {
						c.warn("trusted-author automerge sweep failed", "error", trustedErr)
						if isRateLimited(trustedErr) {
							c.refreshRateLimitCache(ctx)
						}
					} else {
						result.Seen += trustedResult.Seen
						result.Skipped += trustedResult.Skipped
						result.Candidates += trustedResult.Candidates
						result.Merged = append(result.Merged, trustedResult.Merged...)
					}
				}
				if next, changed := nextSelfAuthoredSweepInterval(repos, interval, result); changed {
					t.Reset(next)
					c.info("self-authored automerge sweep interval adjusted",
						"previous_interval", interval,
						"next_interval", next,
						"repos", repos,
						"candidates", result.Candidates,
						"default_candidate_allowance", selfAuthoredSweepCandidateAllowance)
					interval = next
				}
			}
		}
	}()
	c.info("self-authored automerge sweep started", "interval", interval, "repos", len(c.transport.Repositories()))
	return done
}

// listOpenAppAuthoredPullRequests returns every open PR in owner/repo. Uses the
// PR list endpoint (not issue search) because the caller needs PullRequest
// objects: PullRequests.List populates Labels and Head.SHA, so cheap gates can
// run before paying for PullRequests.Get. It does not populate MergeableState;
// PRs that survive the cheap gates are fetched once for that safety-critical
// evaluation data and fetched again immediately before merge to re-verify SHA.
func (c *Engine) listOpenPullRequests(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error) {
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
		out = append(out, prs...)
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

func (c *Engine) releaseHoldActive(ctx context.Context, owner, repo, branch string) (bool, string, error) {
	branch = strings.TrimSpace(branch)
	if c == nil || c.gh == nil || branch == "" {
		return false, "", nil
	}
	br, _, err := c.gh.Repositories.GetBranch(ctx, owner, repo, branch, 3)
	if err != nil {
		return false, "release-hold-check", err
	}
	sha := br.GetCommit().GetSHA()
	if sha == "" {
		return false, "", nil
	}
	statuses, _, err := c.gh.Repositories.GetCombinedStatus(ctx, owner, repo, sha, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return false, "release-hold-check", err
	}
	for _, st := range statuses.Statuses {
		if st == nil || !strings.EqualFold(st.GetContext(), releaseInProgressStatusContext) {
			continue
		}
		if !strings.EqualFold(st.GetState(), "pending") {
			return false, "", nil
		}
		updated := st.GetUpdatedAt().Time
		if updated.IsZero() || c.now().Sub(updated) <= releaseInProgressMaxAge {
			return true, releaseInProgressStatusContext, nil
		}
		return false, "", nil
	}
	return false, "", nil
}

func (c *Engine) trySweepSelfAuthoredPR(ctx context.Context, displayRepo, owner, repo string, number int, branchUpdateAllowed bool) (AutoMergeSweepEvent, string, error) {
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number)
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
	lane, laneOK := c.sweepLaneForAuthor(author)
	if !laneOK {
		// Neither the App's own PR nor a trusted bot's: this path never touches
		// other PRs, matching the human-queue sweep's untouched behavior for
		// PRs it does not own. Defense in depth — prefilterSelfAuthoredPR
		// already filtered on author, but a PR can change hands (rare, but
		// GitHub permits transferring PR authorship attribution in some flows)
		// between listing and evaluating it here.
		return AutoMergeSweepEvent{}, "not-app-authored", nil
	}
	selfLabels := labelNames(pr.Labels)
	if blocked := c.labelBlockReason(selfLabels, true); blocked != "" {
		return AutoMergeSweepEvent{}, blocked, nil
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
	method := mergeMethodFor(pr)
	mergeClaim := effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestMerge,
		Target: fmt.Sprintf("%d", number),
		Actor:  "automerge",
		Inputs: map[string]string{"method": method, "expect_sha": evaluatedHeadSHA, "lane": lane},
	}
	c.reconcileOpenPRMergeEffect(ctx, mergeClaim, owner, repo, number)

	mergeableState := strings.ToLower(strings.TrimSpace(pr.GetMergeableState()))
	if strings.EqualFold(pr.GetMergeableState(), "behind") {
		if !branchUpdateAllowed {
			return AutoMergeSweepEvent{}, "check-pending", nil
		}
		return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, evaluatedHeadSHA, "behind")
	}
	if mergeableState == "dirty" || mergeableState == "conflicting" {
		return AutoMergeSweepEvent{}, "conflicting", nil
	}
	baseBranch := ""
	if pr.GetBase() != nil {
		baseBranch = pr.GetBase().GetRef()
	}
	if held, reason, err := c.releaseHoldActive(ctx, owner, repo, baseBranch); err != nil {
		return AutoMergeSweepEvent{}, reason, err
	} else if held {
		return AutoMergeSweepEvent{}, reason, nil
	}
	var headPushedAt time.Time
	if updatedAt := pr.GetUpdatedAt(); !updatedAt.IsZero() {
		headPushedAt = updatedAt.Time
	}
	green, reason, err := c.commitGreenForPR(ctx, owner, repo, baseBranch, evaluatedHeadSHA, number, headPushedAt, expectedCheckCacheFromContext(ctx))
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		return AutoMergeSweepEvent{}, reason, nil
	}

	// Intent tier gate (#6258). The human merge lane never lets a PR reach
	// trySweepQueuedPR at a tier intent enforcement refuses (writeMergeEligible
	// drops it first); this path lists the App's PRs independently, so it
	// must ask the same question itself — via intent.EvaluateForAppSelfMerge,
	// the tier gate written for this path — or every green App PR merges
	// regardless of tier. Consulted after commitGreen so the extra files
	// fetch is only spent on PRs that are otherwise mergeable, and before the
	// approval desk so the desk still only sees requests policy permits.
	//
	// Human-merge paths (#11038) come first and do not depend on intent: the
	// App self-merge contract authorizes Tier 2/3 without a person, so a path
	// the operator reserved for a person must be refused before
	// EvaluateForAppSelfMerge is ever asked.
	if humanReason, err := c.humanMergePathGate(ctx, displayRepo, owner, repo, pr); err != nil || humanReason != "" {
		return AutoMergeSweepEvent{}, humanReason, err
	}
	if intentReason, err := c.selfMergeIntentGate(ctx, displayRepo, owner, repo, pr, author, selfLabels); err != nil {
		return AutoMergeSweepEvent{}, intentReason, err
	} else if intentReason != "" {
		return AutoMergeSweepEvent{}, intentReason, nil
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
	current, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number)
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
	mergeOut, err := c.executeMergeEffect(ctx, mergeClaim, owner, repo, number, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		mergeResult, _, apiErr = c.gh.PullRequests.Merge(ctx, owner, repo, number, "", &gh.PullRequestOptions{
			SHA:         evaluatedHeadSHA,
			MergeMethod: method,
		})
		if apiErr != nil {
			return effects.Result{}, mergeAPIErrorNotApplied(apiErr)
		}
		return effects.Result{Provenance: mergeResult.GetSHA()}, nil
	})
	if err != nil {
		if requiredChecksExpectedMergeError(err) {
			if !branchUpdateAllowed {
				return AutoMergeSweepEvent{}, "check-pending", nil
			}
			return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, evaluatedHeadSHA, "required-checks-expected")
		}
		return AutoMergeSweepEvent{}, "merge-failed", err
	}
	if mergeResult == nil {
		mergeResult = &gh.PullRequestMergeResult{Merged: gh.Ptr(true), SHA: gh.Ptr(mergeOut.Provenance)}
	}
	if !mergeResult.GetMerged() {
		return AutoMergeSweepEvent{}, "merge-not-applied", nil
	}
	// Audit the merge like MergePR does (pullrequest.go): the activity
	// collector counts pr_merged entries from this trail, and the /fleet L6
	// health verdict is judged on them. When the fleet's merges moved to this
	// sweep, the unaudited path made merging hives read as "no merge in Nd"
	// red on /fleet (observed live on kubestellar/console, 2026-08-26).
	c.transport.RecordPRMergedAudit(owner+"/"+repo, number, method, mergeResult.GetSHA(), hgithub.PRAuditPathSweep)
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		QueuedBy: "", // no queuer in the self-authored path — the App merges its own PR
		HeadSHA:  evaluatedHeadSHA,
		MergeSHA: mergeResult.GetSHA(),
		Tier:     lane,
	}
	c.info("self-authored automerge sweep merged PR", "repo", displayRepo, "pr", number, "author", author, "lane", lane, "merge_sha", event.MergeSHA)
	return event, "", nil
}

func (c *Engine) releaseLevelHoldIfEligible(ctx context.Context, owner, repo string, pr *gh.PullRequest) (bool, string, error) {
	if c == nil || c.transport == nil {
		return false, "", nil
	}
	transport, ok := c.transport.(levelHoldTransport)
	if !ok {
		return false, "", nil
	}
	return transport.ReleaseLevelHoldIfEligible(ctx, owner, repo, pr)
}

// isHeld is the sweep's single hold predicate. It defers to the transport so
// the configured hold set (generic substrings plus the exact hive-scoped
// `hive-pause/<hive-id>` dashboard hold) gates merges exactly as it gates
// enumeration. A nil engine or transport fails closed to the generic set.
// sweepLaneForAuthor reports which self-authored-sweep lane a PR author falls
// in: "self-authored" for the App's own login, "trusted-bot" for a login in
// the operator's trusted_bot_authors set, or ok=false for anyone else. The
// lane is recorded on the mutation claim and the merge log so audits can tell
// the two apart; both lanes pass through exactly the same eligibility gates.
func (c *Engine) sweepLaneForAuthor(author string) (string, bool) {
	author = strings.TrimSpace(author)
	if author == "" {
		return "", false
	}
	if c != nil && c.transport != nil && strings.EqualFold(author, c.transport.AppBotLogin()) {
		return "self-authored", true
	}
	if c == nil || c.trustedBotAuthors == nil {
		return "", false
	}
	if c.trustedBotAuthors()[strings.ToLower(author)] {
		return "trusted-bot", true
	}
	return "", false
}

func (c *Engine) isHeld(labels []string) bool {
	if c == nil || c.transport == nil {
		return hgithub.HasHoldLabel(labels)
	}
	return c.transport.IsHeldLabels(labels)
}

func (c *Engine) sentinelLabelName() string {
	if c == nil || c.sentinelLabel == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(c.sentinelLabel()))
}

func (c *Engine) labelBlockReason(labels []string, trusted bool) string {
	sentinelLabel := c.sentinelLabelName()
	holdLabels := labels
	if trusted && sentinelLabel != "" {
		holdLabels = make([]string, 0, len(labels))
		for _, label := range labels {
			if !strings.EqualFold(strings.TrimSpace(label), sentinelLabel) {
				holdLabels = append(holdLabels, label)
			}
		}
	}
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		lower := strings.ToLower(trimmed)
		switch {
		case sentinelLabel != "" && strings.EqualFold(trimmed, sentinelLabel):
			if !trusted {
				return "sentinel"
			}
		case lower == "needs-rebase":
			return "label:needs-rebase"
		case lower == "needs-human":
			return "label:needs-human"
		case lower == "do-not-merge" || strings.HasPrefix(lower, "do-not-merge/"):
			return "label:" + lower
		}
	}
	if c.isHeld(holdLabels) {
		return "label:hold"
	}
	return ""
}

func (c *Engine) prefilterSelfAuthoredPR(pr *gh.PullRequest) string {
	if pr == nil {
		return "missing-head-sha"
	}
	if state := pr.GetState(); state != "" && !strings.EqualFold(state, "open") {
		return "closed"
	}
	if pr.GetDraft() {
		return "draft"
	}
	if _, ok := c.sweepLaneForAuthor(hgithub.SafeGetLogin(pr.GetUser())); !ok {
		return "not-app-authored"
	}
	labels := labelNames(pr.Labels)
	if blocked := c.labelBlockReason(labels, true); blocked != "" {
		return blocked
	}
	if c.transport.IsExemptLabels(labels) {
		return "exempt-label"
	}
	if pr.GetHead() == nil || pr.GetHead().GetSHA() == "" {
		return "missing-head-sha"
	}
	return ""
}

func (c *Engine) prefilterQueuedIssue(issue *gh.Issue, label string) string {
	if issue == nil || !issue.IsPullRequest() {
		return "not-pull-request"
	}
	labels := labelNames(issue.Labels)
	if !hasLabel(labels, label) {
		return "label-removed"
	}
	if blocked := c.labelBlockReason(labels, false); blocked != "" {
		return blocked
	}
	if c.transport.IsExemptLabels(labels) {
		return "exempt-label"
	}
	return ""
}

func (c *Engine) prefilterTrustedAuthorPR(pr *gh.PullRequest, policy TrustedAuthorPolicy) string {
	if pr == nil {
		return "missing-head-sha"
	}
	if state := pr.GetState(); state != "" && !strings.EqualFold(state, "open") {
		return "closed"
	}
	if pr.GetDraft() {
		return "draft"
	}
	labels := labelNames(pr.Labels)
	if blocked := c.labelBlockReason(labels, true); blocked != "" {
		return blocked
	}
	if excluded := policy.excludedLabel(labels); excluded != "" {
		if sentinelLabel := c.sentinelLabelName(); sentinelLabel != "" && strings.EqualFold(excluded, sentinelLabel) {
			excluded = ""
		}
		if excluded != "" {
			return "excluded-label:" + excluded
		}
	}
	if c.transport.IsExemptLabels(labels) {
		return "exempt-label"
	}
	if pr.GetHead() == nil || pr.GetHead().GetSHA() == "" {
		return "missing-head-sha"
	}
	if decision := c.trustedAuthorDecision(hgithub.SafeGetLogin(pr.GetUser()), policy.RequireRole); !decision.Allowed {
		return "untrusted-author-role"
	}
	return ""
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

func (c *Engine) trySweepTrustedAuthorPR(ctx context.Context, displayRepo, owner, repo string, number int, policy TrustedAuthorPolicy) (AutoMergeSweepEvent, string, error) {
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:trusted_author_automerge_sweep"), owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr", err
	}
	if reason := c.prefilterTrustedAuthorPR(pr, policy); reason != "" {
		return AutoMergeSweepEvent{}, reason, nil
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	decision := c.trustedAuthorDecision(author, policy.RequireRole)
	if !decision.Allowed {
		return AutoMergeSweepEvent{}, "untrusted-author-role", nil
	}
	permission := trustedAuthorPermission{Allowed: true, Level: "not-required"}
	if policy.RequireGitHubPermission {
		var err error
		permission, err = c.authorRepoPermission(ctx, owner, repo, author)
		if err != nil {
			return AutoMergeSweepEvent{}, "author-permission-check", err
		}
		if !permission.Allowed {
			return AutoMergeSweepEvent{}, "author-permission-missing", nil
		}
	}
	if c.isForkPR(pr) && !policy.RequireGitHubPermission {
		var err error
		permission, err = c.authorRepoPermission(ctx, owner, repo, author)
		if err != nil {
			return AutoMergeSweepEvent{}, "author-permission-check", err
		}
		if !permission.Allowed {
			return AutoMergeSweepEvent{}, "fork-non-member", nil
		}
	}
	if blocked, err := c.hasOutstandingChangesRequested(ctx, owner, repo, number); err != nil {
		return AutoMergeSweepEvent{}, "review-state-check", err
	} else if blocked {
		return AutoMergeSweepEvent{}, "changes-requested", nil
	}
	mergeableState := strings.ToLower(strings.TrimSpace(pr.GetMergeableState()))
	if mergeableState == "behind" {
		headSHA := ""
		if pr.GetHead() != nil {
			headSHA = pr.GetHead().GetSHA()
		}
		if headSHA == "" {
			return AutoMergeSweepEvent{}, "missing-head-sha", nil
		}
		return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, headSHA, "behind")
	}
	if mergeableState == "dirty" || mergeableState == "conflicting" {
		return AutoMergeSweepEvent{}, "conflicting", nil
	}
	headSHA := ""
	if pr.GetHead() != nil {
		headSHA = pr.GetHead().GetSHA()
	}
	if headSHA == "" {
		return AutoMergeSweepEvent{}, "missing-head-sha", nil
	}
	method := mergeMethodFor(pr)
	mergeClaim := effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestMerge,
		Target: fmt.Sprintf("%d", number),
		Actor:  "automerge",
		Inputs: map[string]string{"method": method, "expect_sha": headSHA, "lane": "trusted-author"},
	}
	c.reconcileOpenPRMergeEffect(ctx, mergeClaim, owner, repo, number)
	baseBranch := ""
	if pr.GetBase() != nil {
		baseBranch = pr.GetBase().GetRef()
	}
	if held, reason, err := c.releaseHoldActive(ctx, owner, repo, baseBranch); err != nil {
		return AutoMergeSweepEvent{}, reason, err
	} else if held {
		return AutoMergeSweepEvent{}, reason, nil
	}
	var headPushedAt time.Time
	if updatedAt := pr.GetUpdatedAt(); !updatedAt.IsZero() {
		headPushedAt = updatedAt.Time
	}
	green, reason, err := c.commitGreenForPR(ctx, owner, repo, baseBranch, headSHA, number, headPushedAt, expectedCheckCacheFromContext(ctx))
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		return AutoMergeSweepEvent{}, reason, nil
	}
	if humanReason, err := c.humanMergePathGate(ctx, displayRepo, owner, repo, pr); err != nil || humanReason != "" {
		return AutoMergeSweepEvent{}, humanReason, err
	}
	current, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:trusted_author_automerge_sweep"), owner, repo, number)
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
	if currentHeadSHA == "" || currentHeadSHA != headSHA {
		return AutoMergeSweepEvent{}, "head-changed-since-eval", nil
	}

	var mergeResult *gh.PullRequestMergeResult
	mergeOut, err := c.executeMergeEffect(ctx, mergeClaim, owner, repo, number, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		mergeResult, _, apiErr = c.gh.PullRequests.Merge(ctx, owner, repo, number, "", &gh.PullRequestOptions{
			SHA:         headSHA,
			MergeMethod: method,
		})
		if apiErr != nil {
			return effects.Result{}, mergeAPIErrorNotApplied(apiErr)
		}
		return effects.Result{Provenance: mergeResult.GetSHA()}, nil
	})
	if err != nil {
		if requiredChecksExpectedMergeError(err) {
			return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, headSHA, "required-checks-expected")
		}
		return AutoMergeSweepEvent{}, "merge-failed", err
	}
	if mergeResult == nil {
		mergeResult = &gh.PullRequestMergeResult{Merged: gh.Ptr(true), SHA: gh.Ptr(mergeOut.Provenance)}
	}
	if !mergeResult.GetMerged() {
		return AutoMergeSweepEvent{}, "merge-not-applied", nil
	}
	c.transport.RecordPRMergedAudit(owner+"/"+repo, number, method, mergeResult.GetSHA(), hgithub.PRAuditPathSweep)
	if err := c.commentTrustedAuthorMerge(ctx, owner, repo, number, author, decision.Role, permission.Level, headSHA); err != nil {
		c.warn("trusted-author automerge audit comment failed", "repo", displayRepo, "pr", number, "error", err)
	}
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		HeadSHA:  headSHA,
		MergeSHA: mergeResult.GetSHA(),
		Tier:     "trusted-author",
	}
	c.info("trusted-author automerge sweep merged PR", "repo", displayRepo, "pr", number, "author", author, "role", decision.Role, "permission", permission.Level, "merge_sha", event.MergeSHA)
	return event, "", nil
}

func (c *Engine) authorRepoPermission(ctx context.Context, owner, repo, author string) (trustedAuthorPermission, error) {
	level, _, err := c.gh.Repositories.GetPermissionLevel(ctx, owner, repo, author)
	if err != nil {
		return trustedAuthorPermission{}, err
	}
	permission := strings.ToLower(strings.TrimSpace(level.GetPermission()))
	roleName := strings.ToLower(strings.TrimSpace(level.GetRoleName()))
	display := roleName
	if display == "" {
		display = permission
	}
	allowed := permission == "admin" || permission == "write" || roleName == "admin" || roleName == "maintain" || roleName == "write"
	return trustedAuthorPermission{Allowed: allowed, Level: display}, nil
}

func (c *Engine) isForkPR(pr *gh.PullRequest) bool {
	if pr == nil || pr.GetHead() == nil || pr.GetBase() == nil {
		return false
	}
	headRepo := ""
	if pr.GetHead().GetRepo() != nil {
		headRepo = pr.GetHead().GetRepo().GetFullName()
	}
	baseRepo := ""
	if pr.GetBase().GetRepo() != nil {
		baseRepo = pr.GetBase().GetRepo().GetFullName()
	}
	return headRepo != "" && baseRepo != "" && !strings.EqualFold(headRepo, baseRepo)
}

func (c *Engine) updateBranchAndSkip(ctx context.Context, displayRepo, owner, repo string, number int, headSHA, reason string) (AutoMergeSweepEvent, string, error) {
	if err := c.executeUpdateBranchEffect(ctx, owner, repo, number, headSHA, reason); err != nil {
		if isGitHubStatus(err, http.StatusUnprocessableEntity) {
			return AutoMergeSweepEvent{}, "conflicting", nil
		}
		return AutoMergeSweepEvent{}, "update-branch", err
	}
	c.info("automerge update-branch", "repo", displayRepo, "pr", number, "reason", reason)
	return AutoMergeSweepEvent{Repo: displayRepo, Number: number, HeadSHA: headSHA, BranchUpdated: true}, "check-pending", nil
}

func (c *Engine) executeUpdateBranchEffect(ctx context.Context, owner, repo string, number int, headSHA, reason string) error {
	inputs := map[string]string{"expected_head_sha": headSHA, "reason": reason}
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindBranchUpdate,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: inputs,
	}, func(ctx context.Context) (effects.Result, error) {
		var opts *gh.PullRequestBranchUpdateOptions
		if strings.TrimSpace(headSHA) != "" {
			opts = &gh.PullRequestBranchUpdateOptions{ExpectedHeadSHA: gh.Ptr(headSHA)}
		}
		_, _, apiErr := c.gh.PullRequests.UpdateBranch(ctx, owner, repo, number, opts)
		var accepted *gh.AcceptedError
		if errors.As(apiErr, &accepted) {
			apiErr = nil
		}
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: owner + "/" + repo + "#" + strconv.Itoa(number)}, nil
	})
	return err
}

func (c *Engine) hasOutstandingChangesRequested(ctx context.Context, owner, repo string, number int) (bool, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := map[string]string{}
	for {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, owner, repo, number, opts)
		if err != nil {
			return false, err
		}
		for _, review := range reviews {
			login := strings.ToLower(strings.TrimSpace(hgithub.SafeGetLogin(review.GetUser())))
			if login == "" {
				continue
			}
			state := strings.ToUpper(strings.TrimSpace(review.GetState()))
			if state == "APPROVED" || state == "CHANGES_REQUESTED" || state == "DISMISSED" {
				latest[login] = state
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	for _, state := range latest {
		if state == "CHANGES_REQUESTED" {
			return true, nil
		}
	}
	return false, nil
}

func (c *Engine) commentTrustedAuthorMerge(ctx context.Context, owner, repo string, number int, author, role, permission, sha string) error {
	body := fmt.Sprintf("merged by Hive trusted-author auto-merge: author @%s holds role %s and GitHub permission %s on repo; CI green at %s.", author, role, permission, sha)
	_, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
	return err
}

func (c *Engine) trySweepQueuedPR(ctx context.Context, displayRepo, owner, repo string, number int, label string) (AutoMergeSweepEvent, string, error) {
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number)
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
	if c.isHeld(labels) {
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
	method := mergeMethodFor(pr)
	mergeClaim := effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestMerge,
		Target: fmt.Sprintf("%d", number),
		Actor:  "automerge",
		Inputs: map[string]string{"method": method, "expect_sha": headSHA, "lane": "queued"},
	}
	c.reconcileOpenPRMergeEffect(ctx, mergeClaim, owner, repo, number)
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
	if held, reason, err := c.releaseHoldActive(ctx, owner, repo, baseBranch); err != nil {
		return AutoMergeSweepEvent{}, reason, err
	} else if held {
		return AutoMergeSweepEvent{}, reason, nil
	}
	var headPushedAt time.Time
	if updatedAt := pr.GetUpdatedAt(); !updatedAt.IsZero() {
		headPushedAt = updatedAt.Time
	}
	green, reason, err := c.commitGreenForPR(ctx, owner, repo, baseBranch, headSHA, number, headPushedAt, expectedCheckCacheFromContext(ctx))
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		return AutoMergeSweepEvent{}, reason, nil
	}

	// Human-merge paths (#11038): a merger queuing the PR is not the same as
	// a person merging it, so the queued lane honors the list too.
	if humanReason, err := c.humanMergePathGate(ctx, displayRepo, owner, repo, pr); err != nil || humanReason != "" {
		return AutoMergeSweepEvent{}, humanReason, err
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
	mergeOut, err := c.executeMergeEffect(ctx, mergeClaim, owner, repo, number, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		mergeResult, _, apiErr = c.gh.PullRequests.Merge(ctx, owner, repo, number, "", &gh.PullRequestOptions{
			SHA:         headSHA,
			MergeMethod: method,
		})
		if apiErr != nil {
			return effects.Result{}, mergeAPIErrorNotApplied(apiErr)
		}
		return effects.Result{Provenance: mergeResult.GetSHA()}, nil
	})
	if err != nil {
		return AutoMergeSweepEvent{}, "merge-failed", err
	}
	if mergeResult == nil {
		mergeResult = &gh.PullRequestMergeResult{Merged: gh.Ptr(true), SHA: gh.Ptr(mergeOut.Provenance)}
	}
	if !mergeResult.GetMerged() {
		return AutoMergeSweepEvent{}, "merge-not-applied", nil
	}
	// Same audit obligation as the self-authored path above: pr_merged on
	// the trail is what makes this merge count as hive output. path=queue:
	// a person queued this PR; the sweep only carried the merge out.
	c.transport.RecordPRMergedAudit(owner+"/"+repo, number, method, mergeResult.GetSHA(), hgithub.PRAuditPathQueue)
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		QueuedBy: queuedBy,
		HeadSHA:  headSHA,
		MergeSHA: mergeResult.GetSHA(),
		Label:    label,
		Tier:     "queued",
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
// the full precedence): branch protection first, then non-admin branch/rules
// fallbacks, then the operator-declared auto_merge.required_checks fallback.
// GitHub's protection APIs require administration:read on App tokens, but the
// branch payload exposes the same required status-check contexts to pushable
// installations.
//
// Unknown-source fallback: if the required-checks set cannot be determined by
// config, branch protection, the branch payload, or repository rulesets, the
// sweep no longer treats arbitrary red check-runs as required. It waits only
// while a check is still pending, then attempts the merge and lets GitHub's
// merge endpoint enforce the actual required checks server-side.
func (c *Engine) commitGreen(ctx context.Context, owner, repo, branch, sha string) (bool, string, error) {
	return c.commitGreenForPR(ctx, owner, repo, branch, sha, 0, time.Time{}, nil)
}

func (c *Engine) commitGreenForPR(ctx context.Context, owner, repo, branch, sha string, prNumber int, headPushedAt time.Time, expectedChecks *expectedCheckCache) (bool, string, error) {
	// The walk itself lives in hgithub.EvaluateCommitCI so the merge-request
	// watcher's positive-confirmation gate (#6173) evaluates a SHA with the
	// identical rules; only the required-set precedence is engine-specific.
	configRequired, configKnown := c.configRequiredChecksForRepo(owner + "/" + repo)
	required, requiredKnown, fromConfig, fallback, source := hgithub.RequiredStatusCheckContextsDetailedWithSource(ctx, c.gh, owner, repo, branch, configRequired, configKnown)
	actualRequired, actualKnown := required, requiredKnown && !fromConfig
	positiveEvidenceGate := actualKnown && len(required) == 0
	if positiveEvidenceGate {
		required, requiredKnown = nil, false
	}
	if prNumber > 0 {
		c.info("automerge required checks source", "repo", owner+"/"+repo, "pr", prNumber, "branch", branch, "source", source, "known", requiredKnown, "count", len(required))
		if !requiredKnown {
			c.info("automerge required checks unknown; relying on server-side enforcement", "repo", owner+"/"+repo, "pr", prNumber, "branch", branch, "source", source)
		}
	}
	if fallback && fromConfig {
		c.warnRequiredChecksFallback(owner, repo, branch)
	}
	st, err := hgithub.EvaluateCommitCI(ctx, c.gh, owner, repo, sha, hgithub.CommitCIOptions{
		Required:                            required,
		RequiredKnown:                       requiredKnown,
		RequiredKnownFromConfig:             fromConfig,
		UnknownRequiredChecksServerEnforced: !requiredKnown,
		RequireEvidence:                     positiveEvidenceGate,
		MinHeadAge:                          c.minHeadAge,
		HeadPushedAt:                        headPushedAt,
		Now:                                 c.now,
	})
	if err != nil {
		return false, st.Reason, err
	}
	if configKnown && actualKnown {
		c.warnRequiredChecksMismatch(owner, repo, configRequired, actualRequired, st.Observed)
	}
	if !st.Green && isFreshHeadOrMissingExpectedReason(st.Reason) && prNumber > 0 {
		c.info("automerge CI gate blocked PR", "repo", owner+"/"+repo, "pr", prNumber, "sha", sha, "reason", st.Reason)
	}
	if prNumber > 0 && !isFreshHeadOrMissingExpectedReason(st.Reason) {
		c.rememberEvaluatedHead(owner, repo, prNumber, sha)
	}
	return st.Green, st.Reason, nil
}

func isFreshHeadOrMissingExpectedReason(reason string) bool {
	return strings.HasPrefix(reason, "pending: head pushed ") || (strings.HasPrefix(reason, "pending: ") && strings.HasSuffix(reason, " has not started"))
}

func (c *Engine) warnRequiredChecksFallback(owner, repo, branch string) {
	if c == nil {
		return
	}
	key := strings.ToLower(owner + "/" + repo + "#" + branch)
	c.requiredChecksFallbackWarnedMu.Lock()
	if c.requiredChecksFallbackWarned[key] {
		c.requiredChecksFallbackWarnedMu.Unlock()
		return
	}
	c.requiredChecksFallbackWarned[key] = true
	c.requiredChecksFallbackWarnedMu.Unlock()
	c.warn("branch protection required checks unavailable; falling back to configured automerge required checks", "repo", owner+"/"+repo, "branch", branch)
}

func (c *Engine) warnRequiredChecksMismatch(owner, repo string, declared, actual, observed map[string]bool) {
	if c == nil {
		return
	}
	missing := mismatchedDeclaredChecks(declared, actual, observed)
	if len(missing) == 0 {
		return
	}
	key := strings.ToLower("mismatch:" + owner + "/" + repo)
	c.requiredChecksFallbackWarnedMu.Lock()
	if c.requiredChecksFallbackWarned[key] {
		c.requiredChecksFallbackWarnedMu.Unlock()
		return
	}
	c.requiredChecksFallbackWarned[key] = true
	c.requiredChecksFallbackWarnedMu.Unlock()
	c.warn("automerge required_checks override does not match repo protection", "repo", owner+"/"+repo, "declared", sortedSetKeys(declared), "actual", sortedSetKeys(actual), "using", "actual", "mismatch", missing)
}

func mismatchedDeclaredChecks(declared, actual, observed map[string]bool) []string {
	out := make([]string, 0)
	for name := range declared {
		if !actual[name] && !observed[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func sortedSetKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// requiredStatusCheckContexts returns the set of status-check contexts /
// check-run names that are actually required for branch, and whether that
// set could be determined at all. It is the single source of truth
// commitGreen gates on: membership in this set is what makes a check
// "required" (must be green) versus ignorable (any state/conclusion, never
// blocks).
//
// Precedence (first available source wins):
//  1. GitHub's branch-protection API (Repositories.GetRequiredStatusChecks).
//     Kept as a fallback in case the App ever does have admin-read scope, or
//     the branch is legitimately unprotected (gh.ErrBranchNotProtected — a
//     repo with zero required checks is a valid, common state, NOT an
//     error, so that case returns requiredKnown=true with an empty set).
//  2. Config: c.configRequiredChecks(), i.e. the operator-declared
//     auto_merge.required_checks list (config.AutoMergeConfig.RequiredCheckSet).
//  3. Neither available (no config list AND branch empty / API call failed
//     for a reason other than "not protected") → requiredKnown=false. The
//     caller must fall back to the OLD isMetaCheck/isIgnorableCICheck
//     allowlist rather than treating "we don't know the required set" as
//     "nothing is required" — see commitGreen's fail-closed comment.
func (c *Engine) requiredStatusCheckContexts(ctx context.Context, owner, repo, branch string) (map[string]bool, bool) {
	set, ok := c.configRequiredChecksForRepo(owner + "/" + repo)
	return hgithub.RequiredStatusCheckContexts(ctx, c.gh, owner, repo, branch, set, ok)
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
	var ghErr *gh.ErrorResponse
	return errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == status
}

func requiredChecksExpectedMergeError(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusMethodNotAllowed {
		return false
	}
	msg := ghErr.Message
	if msg == "" {
		msg = ghErr.Error()
	}
	return requiredChecksExpectedRE.MatchString(msg)
}

func mergeAPIErrorNotApplied(err error) error {
	if err == nil {
		return nil
	}
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return err
	}
	switch ghErr.Response.StatusCode {
	case http.StatusMethodNotAllowed, http.StatusConflict, http.StatusUnprocessableEntity:
		return effects.NotApplied(err)
	}
	return err
}

func (c *Engine) executeMergeEffect(ctx context.Context, claim effects.Claim, owner, repo string, number int, merge func(context.Context) (effects.Result, error)) (effects.Result, error) {
	out, err := effects.Execute(ctx, c.mutation, claim, merge)
	if !errors.Is(err, effects.ErrNeedsReconciliation) {
		return out, err
	}
	reconciled, recErr := c.reconcileMergeEffect(ctx, claim, owner, repo, number)
	if recErr != nil {
		return out, fmt.Errorf("%w (reconciliation lookup failed: %v)", err, recErr)
	}
	c.info("reconciled unresolved automerge effect", "repo", owner+"/"+repo, "pr", number, "applied", reconciled.Applied)
	return effects.Execute(ctx, c.mutation, claim, merge)
}

func (c *Engine) reconcileOpenPRMergeEffect(ctx context.Context, claim effects.Claim, owner, repo string, number int) {
	if _, ok := c.mutation.(effects.Reconciler); !ok {
		return
	}
	if err := effects.Reconcile(ctx, c.mutation, claim, effects.ExternalState{Applied: false}); err == nil {
		c.info("reconciled open PR automerge effect as not applied", "repo", owner+"/"+repo, "pr", number)
	}
}

func (c *Engine) reconcileMergeEffect(ctx context.Context, claim effects.Claim, owner, repo string, number int) (effects.ExternalState, error) {
	if c == nil || c.gh == nil {
		return effects.ExternalState{}, hgithub.ErrNoGitHubClient
	}
	pr, _, err := c.gh.PullRequests.Get(hgithub.WithRESTCaller(ctx, "hive:automerge_reconcile_merge"), owner, repo, number)
	if err != nil {
		return effects.ExternalState{}, err
	}
	state := effects.ExternalState{Applied: pr.GetMerged(), Provenance: pr.GetMergeCommitSHA()}
	if !pr.GetMerged() {
		state.Provenance = ""
	}
	if err := effects.Reconcile(ctx, c.mutation, claim, state); err != nil {
		return state, err
	}
	return state, nil
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

func (c *Engine) debug(msg string, args ...any) {
	if c != nil && c.logger != nil {
		c.logger.Debug(msg, args...)
	}
}
