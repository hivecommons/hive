package github

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
	"sync/atomic"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/logscrub"
)

// ErrNoGitHubClient is returned by *Client methods invoked on a nil receiver.
//
// A nil *Client is a legitimate, expected runtime state: a hive whose GitHub
// App key is missing or whose app_id is still the placeholder boots in
// dashboard-only mode so its owner can see and fix the problem, and it runs
// with no GitHub client at all. Callers must treat this as "GitHub is
// unavailable right now", not as a bug — it is never a reason to exit.
var ErrNoGitHubClient = errors.New("no github client configured (hive is running without GitHub credentials)")

type Client struct {
	client *gh.Client
	// rateLimits clamps rate-limit readings to be monotone within a window
	// (kubestellar/hive#5733). Per-client because the artifact it corrects is a
	// property of THIS client's token minting. Zero value is ready to use.
	rateLimits   rateLimitTracker
	org          string
	reposMu      sync.RWMutex
	repos        []string
	exemptLabels []string
	// issueFilter is the operator's project.issue_filter (require_labels
	// allow-list) gating which issues become actionable at all. The exclude
	// polarity is NOT here — it is exemptLabels above (governor.labels.exempt,
	// the dashboard Labels tab), the pre-existing single exclusion mechanism,
	// which fetchIssues applies first so it wins on conflict. Enforced in
	// fetchIssues — the choice point where issues enter the actionable set —
	// so everything downstream (governor counts, scheduler kicks, plan-from-
	// label, the contribute queue) inherits it and no agent-side re-listing
	// can bypass it. Guarded because config reload / hub heartbeat delivery
	// re-applies it while the enumeration goroutine reads it. Zero value =
	// admit everything (pre-existing behavior).
	issueFilterMu sync.RWMutex
	issueFilter   IssueAdmitter
	// autoMergeLabel is the configured merger-queue label. Guarded because
	// config reload re-applies it while request handlers read it.
	autoMergeLabelMu sync.RWMutex
	autoMergeLabel   string
	logger           *slog.Logger
	appAuth          *AppAuth // nil for token-authenticated clients
	canariesEnabled  bool
	canaryFailClosed bool
	canaryRegistry   *ioscan.CanaryRegistry
	canaryLeakFunc   func(ioscan.CanaryLeak)
	appBotLogin      string // "<app-slug>[bot]" when the client authenticates as a GitHub App
	// approvalDesk is the RFC #4000 approval-desk consultation performed per PR
	// by the self-authored auto-merge sweep. nil (the default) means the sweep
	// behaves exactly as it did before the desk existed. Set by SetApprovalDesk
	// during startup wiring; see automerge_desk.go.
	approvalDesk ApprovalDeskHook
	// prAuthz gates PR-open requests from the request-file watcher against the
	// per-agent ACMM write-policy + forge-resistance. nil fails closed. Set by
	// StartPRRequestWatcher.
	prAuthz PRRequestAuthorizer
	// prHoldLabel decides, at PR-creation time, whether a freshly-opened PR must
	// carry the "hold" label (F6). It receives the authoring agent so role-specific
	// safety rules (for example, mandatory human review of outreach content) can
	// be enforced alongside the authoritative ACMM level. It is consulted
	// server-side, NOT trusted from a client flag — the
	// gh-wrapper.sh tail that used to add the label is dead code (it sits after
	// `exec hive-open-pr`). nil means "never hold" (backward-compatible no-op).
	// Set by StartPRRequestWatcher.
	prHoldLabel func(agent string) bool
	// prOpenedHook, when set, is told about every NEW PR the request watcher
	// opens (agent, repo, number, url) — the seam progress surfaces such as
	// the Linear session emitter hook. atomic so SetPROpenedHook is safe
	// while the watcher goroutine runs.
	prOpenedHook atomic.Pointer[PROpenedHook]
	// mergeAuthz gates merge requests from the merge-request watcher against the
	// per-agent ACMM merge-policy (CanMerge) + forge-resistance AND the merge
	// TARGET (pinned SHA + governor merge-eligible membership; see
	// MergeRequestAuthorizer / F4). nil fails closed. Set by
	// StartMergeRequestWatcher.
	mergeAuthz MergeRequestAuthorizer
	// issueAuthz gates issue-create/comment/claim requests from the issue-request
	// watcher against the per-agent mode policy (CanCreateIssues) +
	// forge-resistance. nil fails closed. Set by StartIssueRequestWatcher.
	issueAuthz IssueRequestAuthorizer
	// reviewAuthz gates PR-review requests from the review-request watcher against
	// the per-agent push-capability policy + forge-resistance. nil fails closed.
	// Set by StartReviewRequestWatcher.
	reviewAuthz ReviewRequestAuthorizer
	// issueRetries tracks per-request-file retry backoff for the issue-request
	// watcher (in-memory; reset on restart). Guarded by issueRetryMu.
	issueRetryMu sync.Mutex
	issueRetries map[string]*issueRetryState
	// prRetries / reviewRetries apply the same per-request-file backoff +
	// give-up policy to the PR-open and PR-review watchers (request_retry.go).
	// Zero values are ready to use.
	prRetries     retryTracker
	reviewRetries retryTracker
	// defaultBranchMu guards defaultBranches, which memoizes the resolved
	// default branch per "owner/repo" (see defaultbranch.go). A repo's default
	// branch changes about as often as the repo is renamed, so caching it keeps
	// the "what is this repo's base?" lookup off the hot path of every PR open
	// without risking a stale answer that matters. Only successful lookups are
	// cached — a transient API error must not pin a repo to the fallback for
	// the life of the process.
	defaultBranchMu sync.RWMutex
	defaultBranches map[string]string
	// mergeReEngage is Fix #2's re-engagement hook. When a merge attempt fails
	// terminally BECAUSE a required check failed (not a true conflict or a
	// permission error), the watcher calls this instead of silently abandoning
	// the PR, so the fix loop re-dispatches an agent to fix the red check. It
	// returns true if the re-engagement was accepted (dispatch recorded) or
	// false when the loop-safety cap for this PR's red head SHA is exhausted (so
	// a permanently-red PR is not nudged forever). nil is a no-op — the watcher
	// falls back to the original quarantine behavior. Set by
	// SetMergeReEngageHook.
	mergeReEngage MergeReEngageFunc
	// attribution holds the invocation-attribution hooks (trailer gate,
	// per-agent metadata resolver, audit sink) — see attribution.go. Guarded by
	// attribMu because the hooks are installed in stages during startup while
	// the PR-request watcher goroutine may already be reading them.
	attribMu    sync.RWMutex
	attribution AttributionHooks
	// advisoryDigestPosts counts how many times the advisory digest has been
	// (re)posted per "owner/repo#issue" since this process started. The digest
	// is refreshed ~once a minute, so PostAdvisoryDigest audits only the 1st
	// post and every advisoryDigestAuditInterval-th one — a periodic pulse that
	// proves the loop is alive without flooding the audit log. Guarded by
	// advisoryMu.
	advisoryMu          sync.Mutex
	advisoryDigestPosts map[string]int
	// advisoryDigestHashes holds, per "owner/repo#issue", the content hash of
	// the last digest body SUCCESSFULLY written to the forge (#4818). When the
	// next cycle renders a byte-identical body, PostAdvisoryDigest skips the
	// EditComment call entirely — at steady state that saves ~1,440 no-op
	// GitHub writes/day. Only successful writes record a hash, so a failed
	// write keeps being retried. Guarded by advisoryMu.
	advisoryDigestHashes map[string]string
	// advisoryDigestSkips counts consecutive skipped-because-unchanged cycles
	// per key since the last real write. Once it reaches
	// advisoryDigestWriteThroughInterval-1 the next cycle writes through even
	// if unchanged, so permission regressions (e.g. a 403 after the App loses
	// issues:write) still surface within ~an hour instead of never. Guarded by
	// advisoryMu.
	advisoryDigestSkips map[string]int
	// hiveIdentity records which accounts count as "this hive" for the
	// self-authorization gate (pr_self_authorization.go, #5117). It is optional
	// — the gate recognises the App bot without it — and exists so an issue
	// filed under project.ai_author's plain user account is also recognised as
	// ours rather than mistaken for a human's. Guarded because config reload
	// re-installs it while the PR-request watcher goroutine may be reading it.
	hiveIdentityMu sync.RWMutex
	hiveIdentity   HiveIdentity
	// commitTrees memoizes "owner/repo@commitSHA" -> tree SHA for the
	// duplicate-payload guard (pr_duplicate_tree.go, #5111). A commit's tree
	// can never change, so unlike defaultBranches this cache has no staleness
	// window at all; it exists purely so re-inspecting the same open PRs on
	// every PR creation does not re-pay a lookup per candidate. Guarded by
	// commitTreeMu.
	commitTreeMu sync.RWMutex
	commitTrees  map[string]string
}

func (c *Client) SetCanaryScanner(enabled, failClosed bool, reg *ioscan.CanaryRegistry, onLeak func(ioscan.CanaryLeak)) {
	if c == nil {
		return
	}
	c.canariesEnabled = enabled
	c.canaryFailClosed = failClosed
	c.canaryRegistry = reg
	c.canaryLeakFunc = onLeak
}

// GoGitHub returns the underlying go-github client for direct API access.
func (c *Client) GoGitHub() *gh.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// Repositories returns the configured repository list.
func (c *Client) Repositories() []string {
	return c.getRepos()
}

// SplitRepo resolves a repository name into owner and repo components.
func (c *Client) SplitRepo(repo string) (owner, repoName string) {
	if c == nil {
		return "", repo
	}
	return c.splitRepo(repo)
}

// AppBotLogin returns the configured GitHub App bot login.
func (c *Client) AppBotLogin() string {
	if c == nil {
		return ""
	}
	return c.appBotLogin
}

// IsExemptLabels reports whether labels include a configured merge-exempt label.
func (c *Client) IsExemptLabels(labels []string) bool {
	return c.isExempt(labels)
}

// RecordPRMergedAudit records the standard PR-merged audit event.
func (c *Client) RecordPRMergedAudit(repo string, number int, method, sha string) {
	if c == nil {
		return
	}
	c.recordCreationAudit(AuditActionPRMerged, InvocationMeta{Agent: AttributionAgentGovernor},
		"repo", repo,
		"number", strconv.Itoa(number),
		"method", method,
		"sha", sha)
}

// AppAuth returns the GitHub App auth backing this client, or nil when the
// client authenticates with a static token instead of an app installation.
func (c *Client) AppAuth() *AppAuth {
	if c == nil {
		return nil
	}
	return c.appAuth
}

// SetAppBotLogin records the GitHub App bot account that authors App-created
// artifacts. Empty values fail closed for checks that require App authorship.
func (c *Client) SetAppBotLogin(login string) {
	if c == nil {
		return
	}
	c.appBotLogin = strings.TrimSpace(login)
}

type Issue struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Labels    []string  `json:"labels"`
	Assignees []string  `json:"assignees"`
	Priority  string    `json:"priority,omitempty"`
	State     string    `json:"state,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is GitHub's last-activity timestamp for the issue (new commits
	// referencing it, comments, label/assignee changes, …). It is the
	// invalidation signal for the contribute queue's no_work_needed verdict
	// (kubestellar/hive#3987): a verdict recorded BEFORE this timestamp is void,
	// so fresh activity on a previously "nothing to ship" issue re-opens it for
	// offers. Zero when produced by an older enumerator; consumers must fail
	// OPEN (treat activity as unknown → do not suppress) in that case.
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
	AgeMinutes int       `json:"age_minutes"`
	URL        string    `json:"url"`
	IsTracker  bool      `json:"is_tracker"`
	// SourceType and ExternalID carry the work source's own identity through
	// this GitHub-shaped compatibility envelope (kubestellar/hive#4245).
	//
	// Non-default work sources (Linear, Jira) are projected into this struct by
	// worksource.ToGitHubIssues so the scheduler, governor and dashboard keep
	// ONE actionable list. Those items have no GitHub issue number, so Number is
	// 0 — and before these fields existed that was ALL that survived the
	// projection. Every downstream identity site then formatted "repo#0", so two
	// unrelated Linear items shared one hold, one cooldown, one active-task slot.
	//
	// Both are additive and omitempty: an envelope written by GitHub enumeration
	// leaves them empty, and an empty SourceType reads as "github". Consumers
	// must build identity through worksource.Ref rather than reading these
	// directly, so there is a single key implementation.
	SourceType string `json:"source_type,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	// DependsOn carries source-native dependency edges through the legacy
	// GitHub-shaped scheduler envelope. Key is always worksource.Ref.Key().
	DependsOn      []IssueDependency `json:"depends_on,omitempty"`
	ComplexityTier string            `json:"complexity_tier,omitempty"`
	ModelRec       string            `json:"model_recommendation,omitempty"`
	Lane           string            `json:"lane,omitempty"`
}

// IssueDependency is the transport form of a source-aware work dependency.
// It deliberately carries the canonical key rather than source fields so the
// key format has one implementation in pkg/worksource.
type IssueDependency struct {
	Key      string `json:"key"`
	Resolved bool   `json:"resolved"`
}

type PullRequest struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Labels    []string  `json:"labels"`
	Draft     bool      `json:"draft"`
	CreatedAt time.Time `json:"created_at"`
	URL       string    `json:"url"`
	// Mergeable is a tri-state: MergeableYes, MergeableNo, or MergeableUnknown.
	// It is intentionally NOT a bool: a bool zero-values to false, which is
	// indistinguishable from "GitHub says this PR cannot be merged" and would
	// silently gate every PR shut whenever the value was never populated.
	// The zero value is MergeableUnknown ("") so an unfilled field reads as
	// "we do not know" rather than a false negative.
	Mergeable Mergeable `json:"mergeable"`
	CIStatus  string    `json:"ci_status"`
	HeadSHA   string    `json:"head_sha,omitempty"`
	// FailingChecks names the completed check runs whose conclusion was
	// failure/action_required. CIFailureExcerpt carries the raw error lines
	// pulled from those runs' annotations — the evidence a fix agent (or an
	// escalation comment) needs; "CI failed" alone left agents guessing for
	// four days in the 2026-08 seedMission incident.
	FailingChecks    []string `json:"failing_checks,omitempty"`
	CIFailureExcerpt string   `json:"ci_failure_excerpt,omitempty"`
	// BaseSHA is the commit on the base branch the PR targets — the tree a
	// reviewer should read to judge the change in context. It is carried so a
	// repo-grounded review can be pinned to a specific commit rather than to
	// whatever the base branch happens to be when the reviewer runs.
	//
	// This is the base branch tip as reported at enumeration time, not the
	// true merge-base of an ancestry walk. For an up-to-date PR the two are
	// the same commit, and for a stale one the base tip is the more useful
	// tree to read anyway (it is what the change will actually land on). It is
	// an ADVISORY grounding anchor, never a merge-safety input — nothing gates
	// a merge on this field.
	BaseSHA string `json:"base_sha,omitempty"`
}

// HasFailingRequiredCheck reports whether this PR has a completed, non-meta
// (required) check whose conclusion was failure/action_required. It is the
// GENERIC "this PR is red because a required check failed" predicate shared by
// the merge-watcher re-engagement (#2), claim-suppression release (#3), and the
// governor stuck-PR reaper (#4). It keys ONLY off the check state that
// EnrichCIStatus already computed (CIStatus + FailingChecks) — it never inspects
// a specific linter, language, or check name, so nothing here is
// project-specific. CIStatus=="failure" is only ever set when at least one
// non-meta check failed (isMetaCheck already excludes tide/guardrails/netlify/
// copilot), so the FailingChecks guard is belt-and-suspenders.
func (p PullRequest) HasFailingRequiredCheck() bool {
	return p.CIStatus == "failure" && len(p.FailingChecks) > 0
}

// Mergeable is a tri-state mergeability verdict for a pull request.
type Mergeable string

const (
	// MergeableUnknown is the zero value: mergeability has not been determined.
	// Consumers gating a merge MUST treat this as "do not know", never as "no".
	MergeableUnknown Mergeable = ""
	// MergeableYes means GitHub reports the PR can be merged.
	MergeableYes Mergeable = "yes"
	// MergeableNo means GitHub reports the PR cannot be merged (conflicts, etc).
	MergeableNo Mergeable = "no"
)

// mergeableFromState maps a GitHub mergeable_state to a tri-state verdict.
//
// mergeable_state is used in preference to the raw "mergeable" bool because
// this project's standing policy ignores non-required checks (Playwright,
// tide). GitHub reports "unstable" when the PR is cleanly mergeable but some
// non-required check is pending or failing — that still counts as mergeable.
//
// Reference: "clean" and "has_hooks" are fully green; "unstable" is mergeable
// with non-required checks outstanding; "blocked" means a required review or
// status is missing; "dirty" means conflicts; "unknown" means GitHub is still
// computing the merge commit in the background.
func mergeableFromState(state string, mergeable *bool) Mergeable {
	switch state {
	case "clean", "has_hooks", "unstable":
		return MergeableYes
	case "dirty", "blocked", "behind", "draft":
		return MergeableNo
	}

	// "unknown" (or an unrecognised state): GitHub is still computing.
	// Fall back to the raw bool only when it was actually present.
	if mergeable != nil {
		if *mergeable {
			return MergeableYes
		}
		return MergeableNo
	}
	return MergeableUnknown
}

type ActionableResult struct {
	GeneratedAt time.Time             `json:"generated_at"`
	Issues      IssueResult           `json:"issues"`
	PRs         PRResult              `json:"prs"`
	Hold        HoldResult            `json:"hold"`
	Clusters    []IssueCluster        `json:"clusters,omitempty"`
	TotalByRepo map[string]RepoCounts `json:"total_by_repo,omitempty"`
}

type RepoCounts struct {
	Issues int `json:"issues"`
	PRs    int `json:"prs"`
}

type IssueResult struct {
	Count         int     `json:"count"`
	Items         []Issue `json:"items"`
	SLAViolations int     `json:"sla_violations"`
}

// IssueResultFromItems builds the queue summary consumed by the governor.
func IssueResultFromItems(items []Issue) IssueResult {
	result := IssueResult{Count: len(items), Items: items}
	for _, issue := range items {
		if issue.AgeMinutes > slaThresholdMinutes {
			result.SLAViolations++
		}
	}
	return result
}

type PRResult struct {
	Count int           `json:"count"`
	Items []PullRequest `json:"items"`
	// StaleDrafts are the App's OWN draft PRs older than staleDraftAfter.
	// Ordinary drafts (someone's in-progress work, human or agent) are
	// excluded from Items entirely and stay that way here — nobody should be
	// nagged about work they are actively doing. This list exists because the
	// scanner kick prompt already instructs: "Close stale drafts (>48h,
	// needs-rebase + dco-no, or fix already merged)" — but fetchPRs drops
	// EVERY draft before the agent ever sees it, so that instruction had
	// nothing to act on. See kubestellar/hive#3963.
	StaleDrafts []PullRequest `json:"stale_drafts,omitempty"`
}

type HoldResult struct {
	Issues int        `json:"issues"`
	PRs    int        `json:"prs"`
	Total  int        `json:"total"`
	Items  []HoldItem `json:"items"`
}

type HoldItem struct {
	Number int    `json:"number"`
	Repo   string `json:"repo"`
	Title  string `json:"title"`
	Type   string `json:"type"`
	// HeadSHA and Author are populated for Type=="pr" holds (both come free
	// with the PR list response — no extra API call). The hold guard (#5589)
	// snapshots HeadSHA while a PR sits hold-gated so a branch that moves
	// before the hold lifts can be forced back through review. Always empty
	// for Type=="issue".
	HeadSHA string `json:"head_sha,omitempty"`
	Author  string `json:"author,omitempty"`
}

type IssueCluster struct {
	Key    string  `json:"key"`
	Count  int     `json:"count"`
	Issues []Issue `json:"issues"`
}

var HoldLabels = []string{"hold", "on-hold", "hold/review"}
var PermanentExemptLabels = []string{"do-not-merge"}

// AutoMergeQueuedLabel is the fallback label a merger/owner queue action
// applies when a hive has not configured `governor.labels.automerge`. Prefer
// Client.AutoMergeLabel(), which honours that configuration; this constant
// exists so a nil or unconfigured client still has a sane value.
const AutoMergeQueuedLabel = "lgtm"

const slaThresholdMinutes = 30

// NewClient creates a GitHub API client. If apiURL is non-empty and differs
// from the default (https://api.github.com), the client's BaseURL and
// UploadURL are overridden for GitHub Enterprise compatibility.
func NewClient(token string, org string, repos []string, logger *slog.Logger, apiURL string) *Client {
	// Proxy-trusting transport: ordinary API traffic is also MITM'd by the
	// in-process proxy under forced egress, so it must trust the proxy CA
	// (see proxytrust.go).
	client := newTokenClient(token, apiURL)
	return &Client{
		client: client,
		org:    org,
		repos:  repos,
		logger: logger,
	}
}

// setBaseURL configures a go-github client for a custom API URL (GHE).
// If apiURL is empty or the default, no change is made.
func setBaseURL(client *gh.Client, apiURL string) {
	if apiURL == "" || apiURL == "https://api.github.com" {
		return
	}
	// Ensure trailing slash for url.Parse compatibility.
	normalized := apiURL
	if !strings.HasSuffix(normalized, "/") {
		normalized += "/"
	}
	base, err := url.Parse(normalized)
	if err != nil {
		return
	}
	client.BaseURL = base
	// Upload URL follows the same base for GHE instances.
	client.UploadURL = base
}

// NewClientForTest creates a client pointing at a test server. Exported for
// cross-package testing (e.g. dashboard tests that need a mock GH client).
func NewClientForTest(serverURL string, org string, repos []string, logger *slog.Logger) *Client {
	c := NewClient("fake", org, repos, logger, "")
	base, err := url.Parse(serverURL + "/")
	if err != nil {
		panic("bad test server URL: " + err.Error())
	}
	c.client.BaseURL = base
	return c
}

// SetOrg is nil-receiver safe for the same reason as SetRepos: dashboard saves
// and hub heartbeat delivery can retarget a running hive without rebuilding the
// GitHub client, so the owner's namespace must stay in sync with config.
func (c *Client) SetOrg(org string) {
	if c == nil {
		return
	}
	c.org = org
}

// SetRepos is nil-receiver safe. A hive that booted without usable GitHub
// credentials runs with a nil *Client for the life of the process, and the
// config-reload / project-claim / override-migration paths all re-sync the repo
// list unconditionally. Making the setter a no-op on nil is safer than adding a
// guard at each of those call sites, which a future one would forget.
func (c *Client) SetRepos(repos []string) {
	if c == nil {
		return
	}
	c.reposMu.Lock()
	defer c.reposMu.Unlock()
	c.repos = repos
}

// MergeableFromState converts GitHub's mergeable_state/raw mergeable fields to a tri-state.
func MergeableFromState(state string, mergeable *bool) Mergeable {
	return mergeableFromState(state, mergeable)
}

func (c *Client) getRepos() []string {
	c.reposMu.RLock()
	defer c.reposMu.RUnlock()
	result := make([]string, len(c.repos))
	copy(result, c.repos)
	return result
}

func (c *Client) EnumerateActionable(ctx context.Context) (*ActionableResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	now := time.Now()
	result := &ActionableResult{
		GeneratedAt: now,
	}

	var allIssues []Issue
	var allPRs []PullRequest
	var holdItems []HoldItem
	var allStaleDrafts []PullRequest
	totalByRepo := make(map[string]RepoCounts)

	repos := c.getRepos()
	failedRepos := 0
	var lastFetchErr error
	for _, repo := range repos {
		issues, held, issueTotal, err := c.fetchIssues(ctx, repo, now)
		if err != nil {
			c.logger.Warn("failed to fetch issues", "repo", repo, "error", err)
			failedRepos++
			lastFetchErr = err
			continue
		}
		allIssues = append(allIssues, issues...)
		holdItems = append(holdItems, held...)

		prs, heldPRs, staleDrafts, prTotal, err := c.fetchPRs(ctx, repo)
		if err != nil {
			// Issues for this repo were already collected; a PR-only failure
			// is partial and must not count toward the all-repos-failed guard,
			// which would discard real issue data and report an outage.
			c.logger.Warn("failed to fetch PRs", "repo", repo, "error", err)
			continue
		}
		allPRs = append(allPRs, prs...)
		holdItems = append(holdItems, heldPRs...)
		allStaleDrafts = append(allStaleDrafts, staleDrafts...)

		totalByRepo[repo] = RepoCounts{Issues: issueTotal, PRs: prTotal}
	}

	// If every repo failed (e.g. API rate limit exhausted), returning a
	// zero-count result would tell the governor the queue is empty and
	// idle the agents. Surface the failure so callers keep prior state.
	if len(repos) > 0 && failedRepos >= len(repos) {
		return nil, fmt.Errorf("all %d repos failed to enumerate (last error: %w)", len(repos), lastFetchErr)
	}

	sort.Slice(allIssues, func(i, j int) bool {
		return allIssues[i].AgeMinutes > allIssues[j].AgeMinutes
	})

	holdIssueCount := 0
	holdPRCount := 0
	for _, h := range holdItems {
		if h.Type == "issue" {
			holdIssueCount++
		} else {
			holdPRCount++
		}
	}

	result.Issues = IssueResultFromItems(allIssues)
	result.PRs = PRResult{
		Count:       len(allPRs),
		Items:       allPRs,
		StaleDrafts: allStaleDrafts,
	}
	result.Hold = HoldResult{
		Issues: holdIssueCount,
		PRs:    holdPRCount,
		Total:  len(holdItems),
		Items:  holdItems,
	}
	result.TotalByRepo = totalByRepo

	return result, nil
}

// splitRepo handles repo names that may include org prefix (e.g. "org/repo").
// When repo contains "/", the prefix is used as owner; otherwise c.org is used.
func (c *Client) splitRepo(repo string) (owner, repoName string) {
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		return parts[0], parts[1]
	}
	return c.org, repo
}

func (c *Client) fetchIssues(ctx context.Context, repo string, now time.Time) (actionable []Issue, held []HoldItem, totalIssues int, err error) {
	issueFilter := c.getIssueFilter()
	owner, repoName := c.splitRepo(repo)
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: 100},
	}

	var allIssues []*gh.Issue
	for {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repoName, opts)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("listing issues for %s/%s: %w", owner, repoName, err)
		}
		allIssues = append(allIssues, issues...)
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}

	for _, issue := range allIssues {
		if issue.IsPullRequest() {
			continue
		}

		totalIssues++
		labels := extractLabels(issue.Labels)

		if isHeld(labels) {
			held = append(held, HoldItem{
				Number: issue.GetNumber(),
				Repo:   repo,
				Title:  issue.GetTitle(),
				Type:   "issue",
			})
			continue
		}

		if c.isExempt(labels) {
			continue
		}

		// project.issue_filter (require_labels) — THE choice point for
		// label-gated agent work. An issue the filter refuses never becomes
		// actionable: it is invisible to the governor's queue counts, every
		// kick prompt, plan-from-label, and the contribute queue, so no
		// downstream consumer (or prompt-injected agent re-listing the repo)
		// can select it. Zero-value filter admits everything — pre-existing
		// behavior, verified by regression tests. Deliberately AFTER the
		// hold/exempt checks: held issues keep appearing in the Hold list
		// exactly as before, and the EXCLUDE polarity stays the sole property
		// of governor.labels.exempt (the dashboard Labels tab) — which
		// therefore wins over the require gate by construction.
		if !issueFilter.Admits(labels) {
			continue
		}

		ageMinutes := int(now.Sub(issue.GetCreatedAt().Time).Minutes())

		actionable = append(actionable, Issue{
			Repo:       repo,
			Number:     issue.GetNumber(),
			Title:      issue.GetTitle(),
			Author:     safeGetLogin(issue.GetUser()),
			Labels:     labels,
			Assignees:  extractAssignees(issue.Assignees),
			CreatedAt:  issue.GetCreatedAt().Time,
			UpdatedAt:  issue.GetUpdatedAt().Time,
			AgeMinutes: ageMinutes,
			URL:        issue.GetHTMLURL(),
			IsTracker:  isTracker(issue.GetTitle(), labels, issue.GetBody()),
		})
	}

	return actionable, held, totalIssues, nil
}

// staleDraftAfter matches the age threshold the scanner kick prompt already
// names ("Close stale drafts (>48h, ...)"). A draft newer than this may still
// be someone's active work-in-progress; older than this, it is more likely an
// agent that opened a WIP PR and moved on. Only the App's OWN drafts are aged
// this way — a human's stale draft is their call, not ours to nag about.
const staleDraftAfter = 48 * time.Hour

func (c *Client) fetchPRs(ctx context.Context, repo string) (actionable []PullRequest, held []HoldItem, staleDrafts []PullRequest, totalPRs int, err error) {
	now := time.Now()
	owner, repoName := c.splitRepo(repo)
	opts := &gh.PullRequestListOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: 100},
	}

	var allPRs []*gh.PullRequest
	for {
		prs, resp, err := c.client.PullRequests.List(ctx, owner, repoName, opts)
		if err != nil {
			return nil, nil, nil, 0, fmt.Errorf("listing PRs for %s/%s: %w", owner, repoName, err)
		}
		allPRs = append(allPRs, prs...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	for _, pr := range allPRs {
		totalPRs++
		labels := extractPRLabels(pr.Labels)

		if isHeld(labels) {
			heldHeadSHA := ""
			if pr.GetHead() != nil {
				heldHeadSHA = pr.GetHead().GetSHA()
			}
			held = append(held, HoldItem{
				Number:  pr.GetNumber(),
				Repo:    repo,
				Title:   pr.GetTitle(),
				Type:    "pr",
				HeadSHA: heldHeadSHA,
				Author:  safeGetLogin(pr.GetUser()),
			})
			continue
		}

		if c.isExempt(labels) {
			continue
		}

		if pr.GetDraft() {
			author := safeGetLogin(pr.GetUser())
			if strings.EqualFold(author, c.appBotLogin) && now.Sub(pr.GetCreatedAt().Time) > staleDraftAfter {
				staleDrafts = append(staleDrafts, PullRequest{
					Repo:      repo,
					Number:    pr.GetNumber(),
					Title:     pr.GetTitle(),
					Author:    author,
					Labels:    labels,
					Draft:     true,
					CreatedAt: pr.GetCreatedAt().Time,
					URL:       pr.GetHTMLURL(),
				})
			}
			continue
		}

		headSHA := ""
		if pr.GetHead() != nil {
			headSHA = pr.GetHead().GetSHA()
		}
		baseSHA := ""
		if pr.GetBase() != nil {
			baseSHA = pr.GetBase().GetSHA()
		}

		actionable = append(actionable, PullRequest{
			Repo:      repo,
			Number:    pr.GetNumber(),
			Title:     pr.GetTitle(),
			Author:    safeGetLogin(pr.GetUser()),
			Labels:    labels,
			Draft:     pr.GetDraft(),
			CreatedAt: pr.GetCreatedAt().Time,
			URL:       pr.GetHTMLURL(),
			// Mergeable is deliberately NOT set here. The PullRequests.List
			// endpoint never populates "mergeable" — GitHub computes it
			// per-PR and returns it only from the single-PR GET. Reading it
			// here would yield false for every PR. EnrichCIStatus fills it in
			// from a per-PR fetch; until then it stays MergeableUnknown.
			HeadSHA: headSHA,
			BaseSHA: baseSHA,
		})
	}

	return actionable, held, staleDrafts, totalPRs, nil
}

// EnrichCIStatus fetches check-run results for each PR's HEAD commit
// and sets the CIStatus field to "success", "failure", or "pending".
// The "tide" check is skipped — it reports Prow merge-bot state, not CI.
func (c *Client) EnrichCIStatus(ctx context.Context, prs []PullRequest) {
	if c == nil {
		return
	}
	const ciStatusSuccess = "success"
	const ciStatusFailure = "failure"
	const ciStatusPending = "pending"

	for i := range prs {
		if prs[i].HeadSHA == "" {
			prs[i].CIStatus = ciStatusPending
			continue
		}
		owner, repoName := c.splitRepo(prs[i].Repo)

		// Fetch the PR individually to learn its mergeability. The list
		// endpoint that produced these PullRequests never populates
		// "mergeable"/"mergeable_state" — GitHub computes them per-PR and
		// returns them only from this single-PR GET. On error we leave the
		// field as MergeableUnknown rather than guessing.
		if full, _, err := c.client.PullRequests.Get(ctx, owner, repoName, prs[i].Number); err != nil {
			c.logger.Warn("failed to fetch PR mergeability", "repo", prs[i].Repo, "pr", prs[i].Number, "error", err)
		} else {
			prs[i].Mergeable = mergeableFromState(full.GetMergeableState(), full.Mergeable)
		}

		checkRuns, _, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, repoName, prs[i].HeadSHA, &gh.ListCheckRunsOptions{
			ListOptions: gh.ListOptions{PerPage: 100},
		})
		if err != nil {
			c.logger.Warn("failed to fetch check runs", "repo", prs[i].Repo, "pr", prs[i].Number, "error", err)
			prs[i].CIStatus = ciStatusPending
			continue
		}
		if checkRuns.GetTotal() == 0 {
			prs[i].CIStatus = ciStatusPending
			continue
		}
		hasFail := false
		allDone := true
		ciChecksFound := 0
		var failingNames []string
		var failingIDs []int64
		for _, cr := range checkRuns.CheckRuns {
			if isMetaCheck(cr.GetName()) {
				continue
			}
			ciChecksFound++
			if cr.GetStatus() != "completed" {
				allDone = false
				continue
			}
			conclusion := cr.GetConclusion()
			if conclusion == "failure" || conclusion == "action_required" {
				hasFail = true
				failingNames = append(failingNames, cr.GetName())
				failingIDs = append(failingIDs, cr.GetID())
			}
		}
		if ciChecksFound == 0 {
			prs[i].CIStatus = ciStatusPending
			continue
		}
		switch {
		case hasFail:
			prs[i].CIStatus = ciStatusFailure
			prs[i].FailingChecks = failingNames
			prs[i].CIFailureExcerpt = c.fetchFailureExcerpt(ctx, owner, repoName, failingIDs, failingNames)
		case allDone:
			prs[i].CIStatus = ciStatusSuccess
		default:
			prs[i].CIStatus = ciStatusPending
		}
	}
}

// isMetaCheck reports check runs that are merge-gates or deploy-status
// mirrors, NOT code CI. They must not drive CIStatus: a stale
// enforce-guardrails (red-main era) or Netlify status left three
// actually-green PRs classified ci_failing and invisible to the merge sweep
// for hours (2026-08-04). The merge step itself re-enforces these gates —
// counting them here only poisons the eligibility signal.
func isMetaCheck(name string) bool {
	switch name {
	case "tide", "enforce-guardrails":
		return true
	}

	for _, prefix := range []string{"Header rules", "Pages changed", "Redirect rules", "netlify/", "copilot"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// chromiumShardCheckRE matches the Playwright matrix's per-shard check-run
// names, e.g. "Test (chromium, shard 1)", "Test (chromium, shard 2/4)".
var chromiumShardCheckRE = regexp.MustCompile(`(?i)^test \(chromium,\s*shard`)

// isIgnorableCICheck reports check runs this project's standing merge policy
// treats as non-required: it is the superset used to decide whether a
// check-run's COMPLETED CONCLUSION (not just its presence) is allowed to
// block a merge. It always includes the meta/deploy-gate checks from
// isMetaCheck, plus the browser/E2E suites (Playwright, Mobile Browser Tests,
// the chromium shard matrix) and coverage reporting that GitHub's own
// mergeable_state="unstable" already treats as non-required (see
// mergeableFromState) but that isMetaCheck deliberately does NOT cover —
// isMetaCheck's own test locks in that a chromium shard IS real code CI for
// CIStatus classification purposes, so that name-blocking logic must stay
// separate from this one.
//
// This predicate exists because commitGreen (automerge_sweep.go) must ignore
// a CANCELLED conclusion on these checks: Playwright/Mobile/chromium-shard
// runs routinely get cancelled mid-flight (matrix cancellation, concurrency
// groups, flake reruns) even though nothing about them is required for
// branch protection, and a cancelled non-required check must never wedge an
// otherwise-green, MERGEABLE PR out of the self-merge sweep (#22438,
// #22440 — build-gate success, PR mergeable, only blocker was a cancelled
// Playwright/Mobile shard).
func isIgnorableCICheck(name string) bool {
	if isMetaCheck(name) {
		return true
	}

	switch name {
	case "Playwright", "Mobile Browser Tests", "coverage-report":
		return true
	}
	if strings.HasPrefix(name, "Playwright") || strings.HasPrefix(name, "Mobile Browser Tests") {
		return true
	}
	return chromiumShardCheckRE.MatchString(name)
}

// IsIgnorableCICheck reports whether a non-required CI context is ignored by fallback gating.
func IsIgnorableCICheck(name string) bool {
	return isIgnorableCICheck(name)
}

// Bounds for fetchFailureExcerpt: annotations are fetched for at most
// excerptMaxRuns failing check runs, keeping at most excerptMaxLines lines and
// excerptMaxChars characters total. Enough to carry a stack of real errors
// into a kick prompt without flooding it.
const (
	excerptMaxRuns  = 2
	excerptMaxLines = 8
	excerptMaxChars = 900
)

// fetchFailureExcerpt pulls the raw error lines from the annotations of the
// given failing check runs. Annotations capture workflow ##[error] lines (test
// failures, compile errors), which is exactly the evidence fix agents never
// see from a bare "CI failed" status. Works on GitHub and GHE through the
// client's configured API base; on errors it degrades to "" — the excerpt is
// an enrichment, never a gate.

// IsMetaCheck reports whether a CI context is metadata-only for merge gating.
func IsMetaCheck(name string) bool {
	return isMetaCheck(name)
}

func (c *Client) fetchFailureExcerpt(ctx context.Context, owner, repo string, runIDs []int64, runNames []string) string {
	var lines []string
	total := 0
	seen := map[string]bool{}
	for idx, id := range runIDs {
		if idx >= excerptMaxRuns || len(lines) >= excerptMaxLines {
			break
		}
		anns, _, err := c.client.Checks.ListCheckRunAnnotations(ctx, owner, repo, id, &gh.ListOptions{PerPage: 20})
		if err != nil {
			continue
		}
		name := ""
		if idx < len(runNames) {
			name = runNames[idx]
		}
		for _, a := range anns {
			level := a.GetAnnotationLevel()
			if level != "failure" && level != "warning" {
				continue
			}
			msg := strings.TrimSpace(a.GetMessage())
			if msg == "" || seen[msg] {
				continue
			}
			seen[msg] = true
			line := msg
			if name != "" {
				line = name + ": " + msg
			}
			if nl := strings.IndexByte(line, '\n'); nl > 0 {
				line = line[:nl]
			}
			if len(line) > 300 {
				line = line[:300]
			}
			if total+len(line) > excerptMaxChars {
				return strings.Join(lines, "\n")
			}
			lines = append(lines, line)
			total += len(line)
			if len(lines) >= excerptMaxLines {
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

// CreateIssueComment posts a comment on an issue or PR. The signature mirrors
// forge.Forge.CreateIssueComment so escalation callers can swap in a neutral
// forge adapter on non-GitHub hives without changing call sites.
//
// Canary-gated exactly like CreateIssue/CreatePR (kubestellar/hive#4960): the
// issue-request watcher dispatches the same agent-supplied body to either
// CreateIssue (kind "issue") or here (kind "comment"), so a comment left
// ungated would let a prompt-injected agent exfiltrate a canary that a new
// issue would have refused fail-closed.
func (c *Client) CreateIssueComment(ctx context.Context, repo string, number int, body string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	if leak, ok := c.scanCanaryText(body, "hive-comment:"+repo); ok {
		if c.canaryFailClosed {
			return fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
		}
	}
	body = logscrub.ScrubString(body)
	owner, repoName := c.splitRepo(repo)
	_, _, err := c.client.Issues.CreateComment(ctx, owner, repoName, number, &gh.IssueComment{Body: gh.Ptr(body)})
	return err
}

// GetPRAuthor returns the current author login for an open PR.
func (c *Client) GetPRAuthor(ctx context.Context, repo string, number int) (string, error) {
	if c == nil {
		return "", ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(ctx, owner, repoName, number)
	if err != nil {
		return "", err
	}
	return safeGetLogin(pr.GetUser()), nil
}

// QueuePRAutoMerge approves a PR as the hive App and marks it for Hive's
// auto-merge-on-green sweep. The approval body records who queued the PR so
// the sweep can re-check the self-merge ban before it squashes anything.
func (c *Client) QueuePRAutoMerge(ctx context.Context, repo string, number int, queuedBy string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	queuedBy = strings.TrimSpace(queuedBy)
	if queuedBy == "" {
		return errors.New("queuedBy is required for auto-merge audit and self-merge checks")
	}
	owner, repoName := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(ctx, owner, repoName, number)
	if err != nil {
		return fmt.Errorf("fetching PR head for auto-merge approval: %w", err)
	}
	headSHA := ""
	if pr.GetHead() != nil {
		headSHA = pr.GetHead().GetSHA()
	}
	if headSHA == "" {
		return errors.New("PR head SHA is required for auto-merge approval")
	}
	label := c.AutoMergeLabel()
	if err := c.ensureLabel(ctx, owner, repoName, label); err != nil {
		return fmt.Errorf("ensuring %s label: %w", label, err)
	}
	body := fmt.Sprintf("Approved by @%s for Hive auto-merge on green CI.", queuedBy)
	_, _, err = c.client.PullRequests.CreateReview(ctx, owner, repoName, number, &gh.PullRequestReviewRequest{
		CommitID: gh.Ptr(headSHA),
		Body:     gh.Ptr(body),
		Event:    gh.Ptr("APPROVE"),
	})
	if err != nil {
		return fmt.Errorf("approving PR: %w", err)
	}
	// Audit the review so it counts as activity on the trail. This is the hive's
	// own auto-merge self-approval (a governor action); agent-authored reviews
	// come through the review-request watcher, which audits separately.
	c.recordCreationAudit(AuditActionPRReviewed, InvocationMeta{Agent: AttributionAgentGovernor},
		"repo", owner+"/"+repoName, "number", strconv.Itoa(number),
		"agent", queuedBy, "state", "approved")
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repoName, number, []string{label}); err != nil {
		return fmt.Errorf("adding %s label: %w", label, err)
	}
	return nil
}

func (c *Client) ensureLabel(ctx context.Context, owner, repo, name string) error {
	if _, _, err := c.client.Issues.GetLabel(ctx, owner, repo, name); err == nil {
		return nil
	} else if ghErr, ok := err.(*gh.ErrorResponse); !ok || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusNotFound {
		return err
	}
	_, _, err := c.client.Issues.CreateLabel(ctx, owner, repo, &gh.Label{
		Name:        gh.Ptr(name),
		Color:       gh.Ptr("8250df"),
		Description: gh.Ptr("Approved by a Hive merger/owner for auto-merge on green CI"),
	})
	if ghErr, ok := err.(*gh.ErrorResponse); ok && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusUnprocessableEntity {
		return nil
	}
	return err
}

// AddLabels adds labels to an issue or PR (no-op for an empty list), mirroring
// forge.Forge.AddLabels for the same swap-in reason as CreateIssueComment.
func (c *Client) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	if len(labels) == 0 {
		return nil
	}
	owner, repoName := c.splitRepo(repo)
	_, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repoName, number, labels)
	return err
}

func extractLabels(labels []*gh.Label) []string {
	var result []string
	for _, l := range labels {
		result = append(result, l.GetName())
	}
	return result
}

func extractPRLabels(labels []*gh.Label) []string {
	return extractLabels(labels)
}

// ExtractPRLabels returns the names from a pull request label list.
func ExtractPRLabels(labels []*gh.Label) []string {
	return extractPRLabels(labels)
}

func extractAssignees(users []*gh.User) []string {
	var result []string
	for _, u := range users {
		if login := safeGetLogin(u); login != "" {
			result = append(result, login)
		}
	}
	return result
}

func safeGetLogin(u *gh.User) string {
	if u == nil {
		return ""
	}

	return u.GetLogin()
}

// SafeGetLogin returns a GitHub user's login, or an empty string for nil.
func SafeGetLogin(u *gh.User) string {
	return safeGetLogin(u)
}

func HasHoldLabel(labels []string) bool {
	return HasHoldLabelWith(labels, nil)
}

func HasHoldLabelWith(labels, extraHoldLabels []string) bool {
	holdLabels := append([]string{}, HoldLabels...)
	holdLabels = append(holdLabels, extraHoldLabels...)
	for _, label := range labels {
		lower := strings.ToLower(label)
		for _, sub := range holdLabels {
			sub = strings.ToLower(strings.TrimSpace(sub))
			if sub != "" && strings.Contains(lower, sub) {
				return true
			}
		}
	}
	return false
}

func isHeld(labels []string) bool { return HasHoldLabel(labels) }

// SetExemptLabels is nil-receiver safe for the same reason as SetRepos.
func (c *Client) SetExemptLabels(labels []string) {
	if c == nil {
		return
	}
	c.exemptLabels = labels
}

// SetIssueFilter installs the operator's project.issue_filter. Nil-receiver
// safe for the same reason as SetRepos: the config-reload / heartbeat-delivery
// paths re-apply it unconditionally, and a hive that booted without GitHub
// credentials runs with a nil *Client.
func (c *Client) SetIssueFilter(f IssueAdmitter) {
	if c == nil {
		return
	}
	c.issueFilterMu.Lock()
	defer c.issueFilterMu.Unlock()
	c.issueFilter = f
}

// getIssueFilter never returns nil: an unset filter admits everything, which
// is the zero-value contract the config struct had before this became an
// interface. Returning nil here would turn "no filter configured" into a
// panic at the one call site that consults it.
func (c *Client) getIssueFilter() IssueAdmitter {
	c.issueFilterMu.RLock()
	defer c.issueFilterMu.RUnlock()
	if c.issueFilter == nil {
		return admitAllIssues{}
	}
	return c.issueFilter
}

// SetAutoMergeLabel is nil-receiver safe for the same reason as SetRepos. An
// empty or whitespace-only value keeps the default rather than clearing it,
// so a partially-populated config cannot produce an unnamed label.
func (c *Client) SetAutoMergeLabel(label string) {
	if c == nil {
		return
	}
	if strings.TrimSpace(label) == "" {
		return
	}
	c.autoMergeLabelMu.Lock()
	defer c.autoMergeLabelMu.Unlock()
	c.autoMergeLabel = strings.TrimSpace(label)
}

// AutoMergeLabel returns the configured queue label, falling back to the
// default when unset. Nil-receiver safe so callers can report the label even
// when the hive booted without GitHub credentials.
func (c *Client) AutoMergeLabel() string {
	if c == nil {
		return AutoMergeQueuedLabel
	}
	c.autoMergeLabelMu.RLock()
	defer c.autoMergeLabelMu.RUnlock()
	if c.autoMergeLabel == "" {
		return AutoMergeQueuedLabel
	}
	return c.autoMergeLabel
}

func (c *Client) isExempt(labels []string) bool {
	return HasExemptLabel(labels, c.exemptLabels)
}

func HasExemptLabel(labels, exemptLabels []string) bool {
	for _, label := range labels {
		for _, perm := range PermanentExemptLabels {
			if strings.EqualFold(label, perm) || strings.HasPrefix(label, perm) {
				return true
			}
		}
		for _, exempt := range exemptLabels {
			if strings.EqualFold(label, exempt) || strings.HasPrefix(label, exempt) {
				return true
			}
		}
	}
	return false
}

func FilterExemptIssues(items []Issue, exemptLabels []string) []Issue {
	if len(items) == 0 {
		return []Issue{}
	}
	out := make([]Issue, 0, len(items))
	for _, issue := range items {
		if HasExemptLabel(issue.Labels, exemptLabels) {
			continue
		}
		out = append(out, issue)
	}
	return out
}

// trackerTaskListRe matches a Markdown task-list item whose subject is an issue
// reference — "- [ ] #4199", "* [x] owner/repo#12". This is deliberately
// STRUCTURAL rather than a prose match: GitHub renders these lists specially
// (the "N of M" tracked-task-list widget), so the pattern is a first-class
// convention an author opts into, not a phrase they happened to write. Prose
// gets reworded; structure does not.
var trackerTaskListRe = regexp.MustCompile(`(?m)^\s*[-*]\s*\[[ xX]\]\s*(?:[\w.-]+/[\w.-]+)?#\d+`)

// trackerTaskListMin is how many task-list issue references a body needs before
// the issue counts as a tracker. One or two can legitimately appear on ordinary
// work (a blocked-on note, an acceptance criterion); three or more is a
// tracking list by construction.
const trackerTaskListMin = 3

// isTracker reports whether an issue is coordination-only — an umbrella whose
// children carry the actual work — and so must never be handed out as a single
// task.
//
// body is consulted because the title/label markers below miss the common case.
// kubestellar/hive#4188 carried no labels and an ordinary "✨ feature:" title
// while its body said, in a callout, "This is a coordination-only umbrella. Do
// not assign #4188 as one Hive task", above a 13-item task list of children. It
// was offered to a contributor anyway, which burned the full 30-minute task
// budget and booked a failure on work that cannot be completed by one agent
// (only final integration closes such a parent).
//
// The callout text itself is NOT matched — free-text matching is brittle in the
// direction that hurts. The task list is enough, and it is what GitHub itself
// treats as the tracking signal.
func IsTrackerIssue(title string, labels []string, body string) bool {
	if strings.HasPrefix(title, "[Tracker]") {
		return true
	}
	for _, l := range labels {
		if l == "meta-tracker" {
			return true
		}
	}
	// FindAllString with a cap: we only care whether the threshold is reached,
	// not how many refs a long umbrella body carries.
	if len(trackerTaskListRe.FindAllString(body, trackerTaskListMin)) >= trackerTaskListMin {
		return true
	}
	return false
}

func isTracker(title string, labels []string, body string) bool {
	return IsTrackerIssue(title, labels, body)
}

type RateLimitInfo struct {
	Core    RateLimitEntry `json:"core"`
	Search  RateLimitEntry `json:"search"`
	GraphQL RateLimitEntry `json:"graphql"`
}

type RateLimitEntry struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset"`
	// ObservedAt is when this reading was actually taken. Reset alone cannot
	// answer "how old is this number" — it moves independently of the sample,
	// and on the deployment in kubestellar/hive#5733 it was 8.5 minutes adrift
	// of reality while the card sat pinned at the full limit. A value that
	// stops updating is now visibly stale rather than silently confident.
	ObservedAt time.Time `json:"observed_at"`
}

func (c *Client) RateLimits(ctx context.Context) (*RateLimitInfo, error) {
	limits, _, err := c.client.RateLimit.Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching rate limits: %w", err)
	}

	info := &RateLimitInfo{}
	if limits.Core != nil {
		info.Core = RateLimitEntry{
			Limit:     limits.Core.Limit,
			Remaining: limits.Core.Remaining,
			Reset:     limits.Core.Reset.Time,
		}
	}
	if limits.Search != nil {
		info.Search = RateLimitEntry{
			Limit:     limits.Search.Limit,
			Remaining: limits.Search.Remaining,
			Reset:     limits.Search.Reset.Time,
		}
	}
	if limits.GraphQL != nil {
		info.GraphQL = RateLimitEntry{
			Limit:     limits.GraphQL.Limit,
			Remaining: limits.GraphQL.Remaining,
			Reset:     limits.GraphQL.Reset.Time,
		}
	}

	// Clamp to monotone-within-window before anything sees these numbers
	// (kubestellar/hive#5733). A just-minted installation token reports a fresh,
	// EMPTY bucket for the installation's shared budget, and latching that made
	// the dashboard claim 100% headroom at ~89% real usage. Applied here rather
	// than in the dashboard so every consumer of RateLimits() inherits it — the
	// status card and /api/gh-rate-limits are both display paths, and a future
	// caller that gates work on headroom must not be handed the raw artifact.
	// See ratelimit_window.go for the mechanism and why the obvious
	// reset-based clamp does not work.
	info.Core = c.rateLimits.observe("core", info.Core)
	info.Search = c.rateLimits.observe("search", info.Search)
	info.GraphQL = c.rateLimits.observe("graphql", info.GraphQL)

	return info, nil
}

func (c *Client) LatestCommitHash(ctx context.Context, owner, repo, branch string) (string, error) {
	ref, _, err := c.client.Git.GetRef(ctx, owner, repo, "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("fetching ref %s/%s@%s: %w", owner, repo, branch, err)
	}
	return ref.GetObject().GetSHA(), nil
}

// CompareAheadBy returns how many commits head is ahead of base according to
// GitHub's compare endpoint. When head is the release tip and base is a running
// build SHA, this is the "commits behind" count for that build.
func (c *Client) CompareAheadBy(ctx context.Context, owner, repo, base, head string) (int, error) {
	if c == nil || c.client == nil {
		return 0, ErrNoGitHubClient
	}
	cmp, _, err := c.client.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if err != nil {
		return 0, fmt.Errorf("comparing commits %s/%s %s...%s: %w", owner, repo, base, head, err)
	}
	return cmp.GetAheadBy(), nil
}

// CommitMessage returns the first line of the commit message for the given SHA.
func (c *Client) CommitMessage(ctx context.Context, owner, repo, sha string) (string, error) {
	commit, _, err := c.client.Repositories.GetCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		return "", fmt.Errorf("fetching commit %s/%s@%s: %w", owner, repo, sha, err)
	}
	msg := commit.GetCommit().GetMessage()
	if idx := strings.Index(msg, "\n"); idx >= 0 {
		msg = msg[:idx]
	}
	return msg, nil
}

func (c *Client) GetRepo(ctx context.Context, owner, repo string) (*gh.Repository, *gh.Response, error) {
	return c.client.Repositories.Get(ctx, owner, repo)
}

func (c *Client) GetContributorCount(ctx context.Context, owner, repo string) (int, error) {
	const perPage = 100
	opts := &gh.ListContributorsOptions{
		ListOptions: gh.ListOptions{PerPage: perPage},
	}
	var total int
	for {
		contribs, resp, err := c.client.Repositories.ListContributors(ctx, owner, repo, opts)
		if err != nil {
			return 0, fmt.Errorf("listing contributors for %s/%s: %w", owner, repo, err)
		}
		total += len(contribs)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return total, nil
}

func (c *Client) GetFileContent(ctx context.Context, owner, repo, path string) (string, error) {
	return c.GetFileContentRef(ctx, owner, repo, path, "")
}

// GetFileContentRef reads a single file's decoded contents from a repo at an
// optional git ref (branch, tag, or SHA). An empty ref uses the repo's default
// branch. Errors if the path resolves to a directory rather than a file.
func (c *Client) GetFileContentRef(ctx context.Context, owner, repo, path, ref string) (string, error) {
	var opts *gh.RepositoryContentGetOptions
	if ref != "" {
		opts = &gh.RepositoryContentGetOptions{Ref: ref}
	}
	fc, _, _, err := c.client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return "", fmt.Errorf("getting file %s/%s/%s: %w", owner, repo, path, err)
	}
	if fc == nil {
		return "", fmt.Errorf("file %s/%s/%s is a directory", owner, repo, path)
	}
	content, err := fc.GetContent()
	if err != nil {
		return "", fmt.Errorf("decoding %s/%s/%s: %w", owner, repo, path, err)
	}
	return content, nil
}

// PathExistsAtRef reports whether path exists in owner/repo at the given git ref
// (branch, tag, or commit SHA). It is used by the advisory digest to verify that
// a finding's file reference still exists at the pinned analysis commit (#3704),
// so a stale path (e.g. a since-removed "docs/install.md") is not cited as if it
// were live. A 404 means "does not exist"; any other error is returned so the
// caller can decide how to treat an inconclusive check.
func (c *Client) PathExistsAtRef(ctx context.Context, owner, repo, path, ref string) (bool, error) {
	if c == nil {
		return false, ErrNoGitHubClient
	}
	var opts *gh.RepositoryContentGetOptions
	if ref != "" {
		opts = &gh.RepositoryContentGetOptions{Ref: ref}
	}
	fileContent, dirContent, resp, err := c.client.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("checking %s/%s/%s@%s: %w", owner, repo, path, ref, err)
	}
	return fileContent != nil || len(dirContent) > 0, nil
}

// SearchPRCount searches GitHub for PRs by author within an org.
// state is "open" or "merged".
// NOTE (org transfer): the org qualifier uses the CONFIGURED org verbatim.
// Unlike the REST repo endpoints, GitHub SEARCH does not follow repository
// transfers or redirects — after a repo moves orgs, a stale configured org
// here quietly searches the old (now empty) org and returns 0.
func (c *Client) SearchPRCount(ctx context.Context, author, org, state string) (int, error) {
	qualifier := fmt.Sprintf("type:pr author:%s org:%s", author, org)
	if state == "merged" {
		qualifier += " is:merged"
	} else {
		qualifier += " is:open"
	}

	result, _, err := c.client.Search.Issues(ctx, qualifier, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil {
		return 0, fmt.Errorf("searching PRs: %w", err)
	}
	return result.GetTotal(), nil
}

// SearchOutreachPRCount searches GitHub for outreach PRs by author on repos
// OUTSIDE the org. These are PRs with the project name in the title, authored
// by the AI author, on external repos (e.g., awesome-lists, adopters, install missions).
// state is "open" or "merged".
func (c *Client) SearchOutreachPRCount(ctx context.Context, author, org, projectName, state string) (int, error) {
	if projectName == "" {
		return 0, fmt.Errorf("projectName is required for outreach PR search (set project.name in config)")
	}
	// Match the old hive query: author:X type:pr is:STATE "ProjectName" in:title -org:ORG
	// The -org: prefix excludes PRs within the org, showing only external outreach PRs.
	// NOTE (org transfer): like SearchPRCount, the -org: qualifier takes the
	// CONFIGURED org verbatim — GitHub search does not follow repo transfers,
	// so a stale org keeps counting PRs against the old org name.
	// Without the projectName filter, this returns ALL external PRs by the author, inflating the count.
	qualifier := fmt.Sprintf("type:pr author:%s -org:%s \"%s\" in:title", author, org, projectName)
	if state == "merged" {
		qualifier += " is:merged"
	} else {
		qualifier += " is:open"
	}

	result, _, err := c.client.Search.Issues(ctx, qualifier, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: 1},
	})
	if err != nil {
		return 0, fmt.Errorf("searching outreach PRs: %w", err)
	}
	return result.GetTotal(), nil
}

var shaPattern = regexp.MustCompile(`[0-9a-f]{7,40}\b`)

const shaHoldComment = "Thanks for filing this issue! To help us reproduce and investigate, " +
	"could you please include the **commit SHA** of the build you're running?\n\n" +
	"You can find it by running:\n```\ngit rev-parse --short HEAD\n```\n\n" +
	"Or check the bottom of the console UI for the version string.\n\n" +
	"_This issue will be on hold until a SHA is provided. " +
	"Simply add a comment with the SHA and the hold will be automatically removed._"

type SHAHoldConfig struct {
	PrimaryRepo     string
	AIAuthor        string
	InternalAuthors []string
}

type SHAHoldResult struct {
	Held    int `json:"held"`
	Unheld  int `json:"unheld"`
	Skipped int `json:"skipped"`
}

func (c *Client) EnforceSHAHold(ctx context.Context, cfg SHAHoldConfig) (*SHAHoldResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	result := &SHAHoldResult{}
	owner, repo := c.splitRepo(cfg.PrimaryRepo)

	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{"kind/bug"},
		ListOptions: gh.ListOptions{PerPage: 100},
	}

	issues, _, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
	if err != nil {
		return nil, fmt.Errorf("listing kind/bug issues for %s/%s: %w", owner, repo, err)
	}

	for _, issue := range issues {
		if issue.IsPullRequest() {
			continue
		}

		author := safeGetLogin(issue.GetUser())
		if author == cfg.AIAuthor || isInternalAuthor(author, cfg.InternalAuthors) {
			result.Skipped++
			continue
		}

		labels := extractLabels(issue.Labels)
		held := isHeld(labels)
		hasSHA := shaPattern.MatchString(issue.GetBody())

		if !hasSHA {
			hasSHA = c.checkCommentsForSHA(ctx, owner, repo, issue.GetNumber(), author)
		}

		if hasSHA && held {
			c.unhold(ctx, owner, repo, issue.GetNumber())
			result.Unheld++
			c.logger.Info("SHA-UNHOLD", "repo", repo, "issue", issue.GetNumber(), "author", author)
		} else if !hasSHA && !held {
			c.hold(ctx, owner, repo, issue.GetNumber())
			result.Held++
			c.logger.Info("SHA-HOLD", "repo", repo, "issue", issue.GetNumber(), "author", author)
		} else {
			result.Skipped++
		}
	}

	return result, nil
}

func (c *Client) checkCommentsForSHA(ctx context.Context, owner, repo string, number int, _ string) bool {
	opts := &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	comments, _, err := c.client.Issues.ListComments(ctx, owner, repo, number, opts)
	if err != nil {
		c.logger.Warn("failed to list comments for SHA check", "repo", repo, "issue", number, "error", err)
		return false
	}
	for _, comment := range comments {
		if shaPattern.MatchString(comment.GetBody()) {
			return true
		}
	}
	return false
}

func (c *Client) hold(ctx context.Context, owner, repo string, number int) {
	_, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{"hold"})
	if err != nil {
		c.logger.Warn("failed to add hold label", "repo", repo, "issue", number, "error", err)
	}

	comment := &gh.IssueComment{Body: gh.Ptr(shaHoldComment)}
	_, _, err = c.client.Issues.CreateComment(ctx, owner, repo, number, comment)
	if err != nil {
		c.logger.Warn("failed to post SHA hold comment", "repo", repo, "issue", number, "error", err)
	}
}

func (c *Client) unhold(ctx context.Context, owner, repo string, number int) {
	_, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, "hold")
	if err != nil {
		c.logger.Warn("failed to remove hold label", "repo", repo, "issue", number, "error", err)
	}
}

func isInternalAuthor(author string, internalAuthors []string) bool {
	if strings.HasSuffix(author, "[bot]") {
		return true
	}
	for _, ia := range internalAuthors {
		if strings.EqualFold(author, ia) {
			return true
		}
	}
	return false
}
