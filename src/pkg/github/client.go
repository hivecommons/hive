package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/issueclaim"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
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
	client      *gh.Client
	linearTeams map[string][]string
	// foldedTrackers records issues that finding folds landed on (#11587).
	foldedTrackers foldedTrackerSet
	// commentBudget caps automated comments per issue per day (#12733).
	commentBudget issueCommentBudget
	// rateLimits clamps rate-limit readings to be monotone within a window
	// (kubestellar/hive#5733). Per-client because the artifact it corrects is a
	// property of THIS client's token minting. Zero value is ready to use.
	rateLimits rateLimitTracker
	// lastGoodEnum keeps each repo's newest successful enumeration so a
	// transiently refused fetch does not publish the repo as empty
	// (enumerate_lastgood.go). Zero value is ready to use.
	lastGoodEnum lastGoodEnumeration
	// forge is the per-repo state cache EnumerateActionable reads listings
	// from when attached (forgestate_reader.go, hivecommons/hive#13375).
	// Nil lists GitHub directly.
	forgeMu                sync.RWMutex
	forge                  *forgestate.Store
	prDetailTTL            func() time.Duration
	graphQLPRBatchEnabled  func() bool
	graphQLPRBatchPageSize func() int
	prBatchMu              sync.Mutex
	prBatchCheckRuns       map[prBatchCheckRunKey][]*gh.CheckRun
	prBatchReviews         map[string]map[int]prReviewState
	prBatchStats           GraphQLPRBatchStats
	prBatchGraphQLRate     RateLimitEntry
	org                    string
	reposMu                sync.RWMutex
	repos                  []string
	// repoPaused reports whether a repo is under an operator pause (#6203).
	// Guarded by reposMu because the dashboard can pause a repo while the
	// enumeration goroutine is deciding what work exists. Nil (the zero value,
	// and what every test constructs) means nothing is paused, so the client
	// behaves exactly as it did before.
	repoPaused           func(repo string) bool
	scheduledBaseRefresh func(repo string) bool
	// writeback is the unsolicited write-back gate (writeback_gate.go);
	// nil allows everything.
	writeback *writeback.Gate
	// agentServesRepo reports whether an agent is scoped to a repo (#6204).
	// Guarded by reposMu because the dashboard can re-scope an agent while a
	// relay goroutine is deciding whether to fulfil a request. Nil (the zero
	// value, and what every test constructs) means no scoping, so the client
	// behaves exactly as it did before.
	agentServesRepo func(agent, repo string) bool
	// neutralizeMentions reports whether an agent's lane has opted in to
	// write_surface.neutralize_mentions (hivecommons/hive#9587). Guarded by
	// reposMu like agentMayWrite. Nil means no lane is covered, so bodies are
	// posted as written.
	neutralizeMentions func(agent string) bool
	// agentMayWrite reports whether an agent's lane may perform a write
	// operation through the request relays (hivecommons/hive#9587). Guarded
	// by reposMu for the same reason as agentServesRepo. Nil means no
	// allowlist, so every operation allowed today stays allowed.
	agentMayWrite func(agent, op string) bool
	exemptLabels  []string
	holdLabelsMu  sync.RWMutex
	holdLabels    []string
	// neverClaimLabels is governor.claims.never_claim_labels as resolved by
	// the config (nil = package default; see SetNeverClaimLabels).
	neverClaimMu     sync.RWMutex
	neverClaimLabels []string
	// openPRLinkState backs the pre-dispatch open-PR guard and the
	// duplicate-fix resolver (open_pr_links.go); built lazily.
	openPRLinkOnce  sync.Once
	openPRLinkState *openPRLinkCache
	sentinelMu      sync.RWMutex
	sentinelLabel   string
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
	issueFilterMu              sync.RWMutex
	issueFilter                IssueAdmitter
	agentIssueDecisionPolicyMu sync.RWMutex
	agentIssueDecisionPolicy   string
	// reporterConfirmationEnabled restores the legacy close gate for all
	// human-filed bug-family issues when configured. Nil/false means only
	// per-issue opt-in markers activate the gate.
	reporterConfirmationMu      sync.RWMutex
	reporterConfirmationEnabled func() bool
	// supersededAutoClosePolicy gates the hive-authored superseded PR
	// auto-close (pr_superseded_hive_autoclose.go); nil means on.
	supersededAutoCloseMu     sync.Mutex
	supersededAutoClosePolicy SupersededAutoClosePolicy
	// autoMergeLabel is the configured merger-queue label. Guarded because
	// config reload re-applies it while request handlers read it.
	autoMergeLabelMu sync.RWMutex
	autoMergeLabel   string
	// requiredChecks is the config-declared auto_merge.required_checks default the
	// merge-request watcher's CI gate consults (#6173); see SetRequiredChecks.
	requiredChecksMu             sync.RWMutex
	requiredChecks               map[string]bool
	requiredChecksForRepo        func(repo string) (map[string]bool, bool)
	requiredChecksMismatchWarned map[string]bool
	// mergePolicyMu guards the merge-request watcher policy knobs below.
	// Config reloads may replace them while a watcher tick is evaluating a
	// request, so reads must be synchronized.
	mergePolicyMu             sync.RWMutex
	allowUnprotectedBaseRepos map[string]bool
	noCIAllowedRepos          map[string]bool
	humanMergePaths           map[string][]string // auto_merge.human_merge_paths; see SetHumanMergePaths
	sentinelHeads             sentinelHeadsSeen   // head SHAs the sentinel sweep already evaluated
	autoMergeMinHeadAge       time.Duration
	mergeAlertMu              sync.Mutex
	mergeAlertSink            MergeFailureAlertSink
	mergeAlertIDsByRepo       map[string]map[string]mergeAlertEntry
	mergeAlertLastRevalidate  time.Time
	logger                    *slog.Logger
	stageFacts                stageFactsState // out-of-cycle stage writes; see StageFacts
	apiBudget                 apiBudgetState
	appAuth                   *AppAuth // nil for token-authenticated clients
	authToken                 string   // token-authenticated clients only; never log
	canariesEnabled           bool
	canaryFailClosed          bool
	canaryRegistry            *ioscan.CanaryRegistry
	canaryLeakFunc            func(ioscan.CanaryLeak)
	appBotLogin               string // "<app-slug>[bot]" when the client authenticates as a GitHub App
	appBotUserMu              sync.Mutex
	appBotUserID              int64 // cached GitHub user id of appBotLogin; see AppBotUserID
	selfLoginMu               sync.Mutex
	selfLogin                 string        // cached login of the authenticated account; see resolveSelfLogin
	selfLoginFailedAt         time.Time     // last failed GET /user; retried after selfLoginRetryAfter
	selfLoginPending          chan struct{} // closed when the in-flight GET /user completes
	additionalBotLogins       []string
	noBotIdentityWarned       sync.Once
	// approvalDesk is the RFC #4000 approval-desk consultation performed per PR
	// by the self-authored auto-merge sweep. nil (the default) means the sweep
	// behaves exactly as it did before the desk existed. Set by SetApprovalDesk
	// during startup wiring; see automerge_desk.go.
	approvalDesk ApprovalDeskHook
	reviseRepos  []string
	perspectives review.PerspectiveSet
	// reviewerAgent reports whether a lane is a reviewer (review.ReviewCapable).
	// Only reviewers may record a verdict that no dispatch row binds; nil falls
	// back to the dispatcher's name-token rule.
	reviewerAgent func(agent string) bool
	// agentBase maps a lane to its base agent (config.AgentConfig.BaseName),
	// so the self-approval guard treats a base and its replicas as one
	// author. nil compares exact lane names.
	agentBase func(agent string) string
	// relayKnown reports that a login has a contributor profile on this
	// hive (runs a relay that targets it), so a PR under that login is an
	// agent's, not a person's — see SetRelayKnownFunc.
	relayKnown func(login string) bool
	// priorArtFiling* enforce the prior-art filing verdict on relayed issue
	// creates (issue_request_prior_art.go, hivecommons/hive#13879).
	priorArtFilingGate   func() config.PriorArtGateConfig
	priorArtFilingFind   PriorArtFinder
	priorArtFilingRecord PriorArtRefusalRecorder
	// confidenceScore gates the Confidence line on review comments.
	confidenceScore      func() bool
	reviewBacklog        func() (enabled bool, cap int) // out-of-scope finding filing on/off and per-PR cap
	reviewBacklogRouting func() ReviewBacklogRouting    // review.severity / review.backlog; see review_backlog_routing.go
	// reviewEvidence resolves the evidence-bundle settings for a repo. Nil
	// means no bundles are written (see review_evidence.go).
	reviewEvidence func(repo string) ReviewEvidenceSettings
	// mergeCredit resolves issue-author credit for the hive's merges
	// (merge_credit.go, hivecommons/hive#14581); creditIssueAuthors is the
	// live auto_merge.credit_issue_authors switch and mergeCredits holds the
	// credit decided for a PR until its pr_merged audit consumes it.
	mergeCreditMu      sync.Mutex
	mergeCredit        *MergeCreditor
	creditIssueAuthors func(repo string) bool
	mergeCredits       map[string]MergeCredit
	// issueClaims reports whether issue claims are recognised at enumeration
	// time and the TTL an assignee-inferred claim runs for (#8380). Nil or
	// false → no comment is fetched, no Issue carries claim fields. Read live
	// so a Features-panel toggle applies without a client rebuild.
	issueClaims func() (enabled bool, ttl time.Duration)
	// issueClaimLedger resolves the hub's own live ledger claim for an issue
	// (issue_claims.go, SetIssueClaimLedger). Consulted only when the forge
	// carries no live marker: the 🔒 comment is budgeted and can be
	// suppressed, and a claim the ledger holds must still read as a claim.
	issueClaimLedger func(repo string, number int) (issueclaim.Claim, bool)
	// prClaimLookup resolves the duplicate-PR claim ledger's record for an
	// issue (SetPRClaimLookup). An out-of-cycle fact on an issue the ledger
	// knows an open PR covers re-derives under that PR (stage_facts_sink.go,
	// enrichCoveringPR) instead of falling to 010/020 until the next sweep.
	prClaimLookup func(repo string, number int) (IssueClaim, bool)
	// issueClaimLedgerHive is the hive= this hub writes on its own markers;
	// a live forge marker carrying it while the ledger holds nothing is a
	// marker the hub already released (hivecommons/hive#13579).
	// Resolved per call: the ledger is built after the client, and until it
	// exists nothing is "released in the ledger".
	issueClaimLedgerHive func() string
	// inProgress resolves the hive/in-progress start-signal setting (see
	// issue_inprogress.go). Nil → the label is not applied or swept.
	inProgress func() InProgressSetting
	// mttrIssueCache remembers issue creation times used by the MTTR dashboard
	// card so the hourly metrics pass does not refetch the same referenced issue
	// every cycle. Guarded by mttrIssueMu.
	mttrIssueCache map[string]mttrIssueCacheEntry
	mttrIssueMu    sync.Mutex
	// attributedClosedPRCache keeps the last successful bounded closed-PR
	// attribution scan per repo. Closed attributions feed outcome/rework
	// dashboards; they must not be re-fetched from all history each tick, and a
	// transient closed-PR listing failure must not erase the repo's open PRs
	// from the governor.
	attributedClosedPRMu     sync.Mutex
	attributedClosedPRCache  map[string]attributedClosedPRCacheEntry
	attributedClosedPRErrors map[string]string
	// issueClaimCache remembers the claim read for an issue at a given
	// updated_at, so the per-issue comment fetch happens once per activity
	// change rather than once per enumeration. Guarded by issueClaimMu.
	issueClaimCache map[string]issueClaimCacheEntry
	issueClaimMu    sync.Mutex
	// sharedCIRerunSeen memoizes PRs the shared-CI rerun sweep found nothing
	// to do for, keyed "owner/repo#n". Guarded by sharedCIRerunMu.
	sharedCIRerunSeen map[string]sharedCIRerunSeen
	sharedCIRerunMu   sync.Mutex
	// sharedCIRerunForbidden records when the App was last refused a rerun in
	// a repo (403 "Resource not accessible by integration"), keyed
	// "owner/repo". Guarded by sharedCIRerunMu.
	sharedCIRerunForbidden map[string]time.Time
	// combinedPerspectivesFn / maxReviewsPerHeadFn feed the per-head review
	// backstop (perHeadReviewRefusal); read live so a dashboard change applies
	// without a client rebuild. maxReviewsPerHeadFn returning 0 means derive;
	// a negative value disables the backstop.
	combinedPerspectivesFn func() bool
	maxReviewsPerHeadFn    func() int
	// reviewRequireLabelsFn returns review.require_labels, read live so a
	// Features-tab edit gates the next relay request without a rebuild. nil
	// or empty means the gate is off.
	reviewRequireLabelsFn func() []string
	// prAuthz gates PR-open requests from the request-file watcher against the
	// per-agent ACMM write-policy + forge-resistance. nil fails closed. Set by
	// StartPRRequestWatcher.
	prAuthz PRRequestAuthorizer
	// prHoldLabel decides, at PR-creation time, whether a freshly-opened PR must
	// carry the "hold" label (F6). It receives the authoring agent and the PR's
	// target repo so role-specific safety rules (for example, mandatory human
	// review of outreach content aimed at a repo the hive does not manage) can
	// be enforced alongside the authoritative ACMM level. It is consulted
	// server-side, NOT trusted from a client flag — the
	// gh-wrapper.sh tail that used to add the label is dead code (it sits after
	// `exec hive-open-pr`). nil means "never hold" (backward-compatible no-op).
	// Set by StartPRRequestWatcher.
	prHoldLabel func(agent, repo string) bool
	// selfAuthorizationHoldEnabled returns the live per-repo config switch for
	// the #5117 self-authorization hold. It is a callback so dashboard/config
	// changes are honoured at evaluation time rather than frozen at watcher
	// startup. nil preserves the pre-existing default-on gate.
	selfAuthorizationHoldEnabled func(repo string) bool
	selfAuthDisabledLoggedMu     sync.Mutex
	selfAuthDisabledLogged       map[string]bool
	// labelApprovalPolicy resolves label-driven approval per repo
	// (hivecommons/hive#11313, label_approval.go). nil means no repo opted in.
	labelApprovalMu     sync.RWMutex
	labelApprovalPolicy func(repo string) (LabelApprovalPolicy, bool)
	// reporterTrustHoldEnabled and reporterTrusted are the live config seams
	// for the #9665 reporter-trust hold (pr_reporter_trust.go). Both nil
	// until the boot wiring installs them; nil means "off" and "nobody",
	// respectively, so an unwired client never holds.
	reporterTrustEscalation  func(string, int, string) bool
	reporterTrustMu          sync.RWMutex
	reporterTrustHoldEnabled func(repo string) bool
	reporterTrusted          func(login, association string) bool
	otherNeedsHumanReason    func(repo string, number int) bool
	// relayContributor reports whether a login holds a live contributor
	// (relay) claim, the provenance that exempts its PRs from the
	// clanker-requested parking (pr_clanker_requested.go). nil knows no relay
	// contributors.
	relayContributor func(login string) bool
	// prSignedCommits, when set and returning true, makes the PR-request watcher
	// re-author each head branch through createCommitOnBranch before opening the
	// PR, so the commit is GitHub-signed (Verified) and authored by the App bot.
	// A func rather than a bool so a config reload is honoured on the next
	// request without rebuilding the client. nil means off. See
	// reauthorBranchSigned.
	prSignedCommits func() bool
	// signedReconcile is the state of the follow-up signing pass (#9364):
	// last-seen head per open PR, PRs already told why they can't be signed,
	// and when the pass last ran. See pr_signed_reconcile.go.
	signedReconcile signedReconcileState
	// prOpenedHook, when set, is told about every NEW PR the request watcher
	// opens (agent, repo, number, url) — the seam progress surfaces such as
	// the Linear session emitter hook. atomic so SetPROpenedHook is safe
	// while the watcher goroutine runs.
	prOpenedHook atomic.Pointer[PROpenedHook]
	// prOpenedDetailHook is the richer sibling of prOpenedHook (body and
	// handoff summary included), used by PR follow-up resume (#9583).
	prOpenedDetailHook     atomic.Pointer[PROpenedDetailHook]
	prTerminalObservedHook atomic.Pointer[PRTerminalObservedHook]
	prRepoPolicyGate       atomic.Pointer[PRRepoPolicyGate]
	// agentStartHook is told when an agent's own relay request shows it
	// working an issue (#10527). See agent_start_hook.go.
	agentStartHook atomic.Pointer[AgentStartHook]
	// reviewVerdictHook is told when the relay records a verdict bound to a
	// review dispatch (review_verdict_hook.go).
	reviewVerdictHook atomic.Pointer[ReviewVerdictHook]
	// mergeAuthz gates merge requests from the merge-request watcher against the
	// per-agent ACMM merge-policy (CanMerge) + forge-resistance AND the merge
	// TARGET (pinned SHA + governor merge-eligible membership; see
	// MergeRequestAuthorizer / F4). nil fails closed. Set by
	// StartMergeRequestWatcher.
	mergeAuthz MergeRequestAuthorizer
	// pushBranchAuthz gates push-branch requests from the push-branch-request
	// watcher against the per-agent ACMM write-policy (CanPush) +
	// forge-resistance. nil fails closed. Set by
	// StartPushBranchRequestWatcher.
	pushBranchAuthz PushBranchRequestAuthorizer
	// issueAuthz gates issue-create/comment/claim requests from the issue-request
	// watcher against the per-agent mode policy (CanCreateIssues) +
	// forge-resistance. nil fails closed. Set by StartIssueRequestWatcher.
	issueAuthz IssueRequestAuthorizer
	// reviewAuthz gates PR-review requests from the review-request watcher against
	// the per-agent push-capability policy + forge-resistance. nil fails closed.
	// Set by StartReviewRequestWatcher.
	reviewAuthz ReviewRequestAuthorizer
	// reviewBots is classification.review_bots (hivecommons/hive#7360): the
	// external review-bot logins whose threads the hive may reply in and
	// resolve. Zero value = feature off: the monitor reports nothing and the
	// watcher denies every thread request. Guarded because config reload
	// re-installs it while the monitor and watcher goroutines read it. Set by
	// SetReviewBots.
	reviewBotsMu sync.RWMutex
	reviewBots   config.ReviewBotsConfig
	// fixHumanPRs mirrors review.fix_human_prs (hivecommons/hive#8421) into
	// the review-thread reconciler (#9361): when on, CollectReviewThreads also
	// follows up on PRs the hive did not open, since the fixer is now allowed
	// to push to them too. Guarded like reviewBots for the same reload race.
	// Set by SetFixHumanPRs.
	fixHumanPRsMu sync.RWMutex
	fixHumanPRs   bool
	// issueRetries tracks per-request-file retry backoff for the issue-request
	// watcher (in-memory; reset on restart). Guarded by issueRetryMu.
	issueRetryMu sync.Mutex
	issueRetries map[string]*issueRetryState
	// prRetries / reviewRetries apply the same per-request-file backoff +
	// give-up policy to the PR-open and PR-review watchers (request_retry.go).
	// Zero values are ready to use.
	prRetries     retryTracker
	reviewRetries retryTracker
	// forgeState memoizes the advisory issue the attached ForgeState cache
	// resolved (advisory_forgestate.go, hivecommons/hive#13378).
	forgeState forgeStateBinding
	// linkedIssueTitles remembers the title and state of issues PRs close
	// that no open-items listing carries any more (linked_issue_titles.go).
	// Zero value is ready to use.
	linkedIssueTitles linkedIssueTitleCache
	// reviewWake wakes the review-request watcher between ticks
	// (review_request_wake.go). Zero value is ready to use.
	reviewWake reviewRequestWaker
	// defaultBranchMu guards defaultBranches, which memoizes the resolved
	// default branch per "owner/repo" (see defaultbranch.go). A repo's default
	// branch changes about as often as the repo is renamed, so caching it keeps
	// the "what is this repo's base?" lookup off the hot path of every PR open
	// without risking a stale answer that matters. Only successful lookups are
	// cached — a transient API error must not pin a repo to the fallback for
	// the life of the process.
	defaultBranchMu sync.RWMutex
	defaultBranches map[string]string
	// closingLinks memoises issue↔PR pairs already pushed to GitHub's
	// Development sidebar for PRs GitHub did not link itself (hivecommons/hive#11888).
	// Zero value is ready to use.
	closingLinks closingLinkCache
	// coveredByPRLabel memoises repos whose covered-by-PR label is already
	// under its current spelling (hivecommons/hive#11888). Zero value is
	// ready to use.
	coveredByPRLabel coveredByPRLabelState
	// stageFamily memoises repos whose stage/author label family has been
	// provisioned this process (hivecommons/hive#12436). Zero value is ready.
	stageFamily coveredByPRLabelState
	// stageArchived memoises repos GitHub refused label writes on because
	// they are archived (hivecommons/hive#14026); the sweep skips them.
	stageArchived coveredByPRLabelState
	// legacyDecisionLabels memoises repos whose retired needs-decision /
	// needs-direction definitions were deleted (hivecommons/hive#12683), or
	// whose definition cleanup was denied integration permission (#14372).
	legacyDecisionLabels coveredByPRLabelState
	// issueStage resolves the live `governor.labels.stage` policy for the
	// issue stage reconciler; nil disables it.
	issueStage func() IssueStagePolicy
	// authorKindLabels resolves the live governor.labels.author names for
	// dashboard decisions; nil uses the default names.
	authorKindLabels func() authorkind.Names
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
	// mergePolicyGate is the merge-request relay's lane-policy gate (see
	// MergeRequestPolicyGate). nil installs no gate: the relay then applies
	// only its own authz, label and CI checks, as before the gate existed.
	mergePolicyGate MergeRequestPolicyGate
	// forgeMergeSelector routes a relayed merge for a repo on a non-GitHub
	// forge to that forge's merger (see ForgeMergeSelector). nil keeps every
	// repo on the GitHub path.
	forgeMergeSelector ForgeMergeSelector
	// mergeStateMu guards the merge-request watcher's per-PR+SHA wait state
	// (merge_request_state.go). mergeStatePath is where it is persisted;
	// empty keeps it in memory only (tests). Set by SetMergeRequestStatePath.
	mergeStateMu     sync.Mutex
	mergeStatePath   string
	mergeState       map[string]mergeRequestPRState
	mergeStateLoaded bool
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
	mutationMu   sync.RWMutex
	mutation     effects.Boundary
	// stageWriter is the one stageengine.StageWriter for this client and
	// stageEngines the reconcilers built over it, one per Store
	// (stage_writer.go). Guarded by stageEngineMu.
	stageEngineMu sync.Mutex
	stageWriter   *stageWriter
	stageEngines  map[*stagelabels.Store]stageengine.Reconciler
	// baseRefresh is the decision carry's memo (pr_decision_base_refresh.go).
	baseRefreshOnce sync.Once
	baseRefresh     *BaseRefreshChecker
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

// AppAuth returns the GitHub App auth backing this client, or nil when the
// client authenticates with a static token instead of an app installation.
func (c *Client) AppAuth() *AppAuth {
	if c == nil {
		return nil
	}
	return c.appAuth
}

// SetAdditionalBotLogins records further App-bot accounts (e.g. a second hive
// using a different GitHub App on the same repos) whose open issues count as
// hive-filed for twin, fold and rejected-twin dedupe.
func (c *Client) SetAdditionalBotLogins(logins []string) {
	if c == nil {
		return
	}
	var out []string
	for _, l := range logins {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	c.additionalBotLogins = out
}

// hiveBotLogins returns every configured hive-filed-issue author login: the
// client's own App bot plus any additional ones. Empty for PAT-auth clients
// with nothing configured.
func (c *Client) hiveBotLogins() []string {
	var out []string
	if l := strings.TrimSpace(c.appBotLogin); l != "" {
		out = append(out, l)
	}
	return append(out, c.additionalBotLogins...)
}

func isHiveBotLogin(logins []string, login string) bool {
	for _, l := range logins {
		if strings.EqualFold(l, login) {
			return true
		}
	}
	return false
}

// SetAppBotLogin records the GitHub App bot account that authors App-created
// artifacts. Empty values fail closed for checks that require App authorship.
func (c *Client) SetAppBotLogin(login string) {
	if c == nil {
		return
	}
	c.appBotLogin = strings.TrimSpace(login)
}

// SetReviseRepos allowlists the repos where the reviewer may revise its own
// previous review in place rather than posting a second one. Empty disables
// revision everywhere, which is the default: rewriting text a maintainer has
// already read is granted per repo, never assumed.
func (c *Client) SetReviseRepos(repos []string) {
	if c == nil {
		return
	}
	cleaned := make([]string, 0, len(repos))
	for _, r := range repos {
		if trimmed := strings.TrimSpace(r); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	c.reviseRepos = cleaned
}

// SetPerspectives tells the relay which review perspectives this hive defines,
// so a verdict claiming anything else is refused.
//
// The relay is where an agent-supplied verdict crosses into hive state, so it
// must judge names against what this hive actually reviews with rather than a
// compiled-in list — otherwise a hive that adds its own perspective would have
// every verdict for it rejected as unknown, and a hive that removed one would
// still accept verdicts nothing asked for.
func (c *Client) SetPerspectives(set review.PerspectiveSet) {
	if c == nil {
		return
	}
	c.perspectives = set
}

// SetReviewerAgentFunc tells the relay which lanes are reviewers. A verdict
// with no matching dispatch row is recorded only when the submitting agent
// passes this check, so a non-reviewer lane cannot satisfy or veto the
// review-approval merge gate for a PR nothing asked it to review.
func (c *Client) SetReviewerAgentFunc(fn func(agent string) bool) {
	if c == nil {
		return
	}
	c.reviewerAgent = fn
}

func (c *Client) isReviewerAgent(agent string) bool {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return false
	}
	if c != nil && c.reviewerAgent != nil {
		return c.reviewerAgent(agent)
	}
	return review.ReviewCapable(review.AgentCapability{Name: agent}, nil)
}

// SetAgentBaseFunc tells the relay how to resolve a lane to its base agent.
// A replica is review-capable when its base is a reviewer, so the
// self-approval guard must refuse a verdict from any lane of the authoring
// family, not just the authoring lane itself.
func (c *Client) SetAgentBaseFunc(fn func(agent string) string) {
	if c == nil {
		return
	}
	c.agentBase = fn
}

// SetRelayKnownFunc tells the client which logins are relay contributors
// registered on this hive. The duplicate-fix sweep uses it to tell the
// hive's own contributor agent (closable, writes allowed as a hive item)
// from a person who happens to have no author/* label yet
// (hivecommons/hive#15469: a relay's duplicate PR read as a person's, so
// the gate refused the notice and the pair rode the merge train together).
func (c *Client) SetRelayKnownFunc(fn func(login string) bool) {
	if c == nil {
		return
	}
	c.relayKnown = fn
}

// IsRelayContributorLogin reports whether login runs a registered relay on
// this hive; false when no registry is installed.
func (c *Client) IsRelayContributorLogin(login string) bool {
	login = strings.TrimSpace(login)
	if c == nil || c.relayKnown == nil || login == "" {
		return false
	}
	return c.relayKnown(login)
}

func (c *Client) agentBaseName(agent string) string {
	agent = strings.TrimSpace(agent)
	if agent == "" || c == nil || c.agentBase == nil {
		return agent
	}
	if base := strings.TrimSpace(c.agentBase(agent)); base != "" {
		return base
	}
	return agent
}

// SetConfidenceScore turns on the one-line mergeability score the relay
// appends to each review comment (review.confidence_score,
// hivecommons/hive#8182). Read through a func so a live config edit takes
// effect on the next review without rebuilding the client.
// SetReviewCadenceLimits wires the per-head review backstop's inputs.
func (c *Client) SetReviewCadenceLimits(combined func() bool, maxPerHead func() int) {
	c.combinedPerspectivesFn = combined
	c.maxReviewsPerHeadFn = maxPerHead
}

func (c *Client) SetConfidenceScore(enabled func() bool) {
	if c == nil {
		return
	}
	c.confidenceScore = enabled
}

func (c *Client) confidenceScoreOn() bool {
	return c != nil && c.confidenceScore != nil && c.confidenceScore()
}

// SetReviewBacklog configures filing cited out-of-scope review findings as
// follow-up issues. Read through a func so live config edits apply without
// rebuilding the client.
func (c *Client) SetReviewBacklog(cfg func() (enabled bool, cap int)) {
	if c == nil {
		return
	}
	c.reviewBacklog = cfg
}

func (c *Client) reviewBacklogConfig() (bool, int) {
	if c == nil || c.reviewBacklog == nil {
		return true, DefaultReviewBacklogIssueCap
	}
	enabled, cap := c.reviewBacklog()
	if cap <= 0 {
		cap = DefaultReviewBacklogIssueCap
	}
	return enabled, cap
}

type IssueLinkedPR struct {
	Number  int    `json:"number"`
	Repo    string `json:"repo,omitempty"`
	State   string `json:"state"`
	Merged  bool   `json:"merged"`
	URL     string `json:"url,omitempty"`
	Closing bool   `json:"closing,omitempty"`
	// Reference marks a PR that references the issue without closing it
	// (`Refs #N`, `Part of #N`): the dashboard glyph reads "covered by PR
	// #N (partial)" rather than "closes via PR #N" (hivecommons/hive#12144).
	Reference bool `json:"reference,omitempty"`
}

type Issue struct {
	Repo              string `json:"repo"`
	Number            int    `json:"number"`
	Title             string `json:"title"`
	Body              string `json:"body,omitempty"`
	Author            string `json:"author"`
	AuthorIsHuman     bool   `json:"author_is_human,omitempty"`
	HumanAcknowledged bool   `json:"human_acknowledged,omitempty"`
	// AckSource names where a hive-filed issue's acknowledgement came from
	// when it was not given on the issue itself: "parent #N" for a child the
	// relay split out of a human-filed or human-acknowledged parent
	// (hivecommons/hive#9840). Empty when HumanAcknowledged was earned on the
	// issue (label, assignee) or is false.
	AckSource string   `json:"ack_source,omitempty"`
	Labels    []string `json:"labels"`
	Assignees []string `json:"assignees"`
	Priority  string   `json:"priority,omitempty"`
	State     string   `json:"state,omitempty"`
	// Reopened is GitHub's state_reason == "reopened": the issue was closed
	// at least once and a person opened it again. The duplicate-PR guard uses
	// it to decide which merged claims to re-examine (ClaimLedger.
	// ReconcileReopenedIssues) without a timeline read per issue per pass.
	Reopened  bool      `json:"reopened,omitempty"`
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
	// FoldedTracker marks an issue that received a folded finding
	// (<!-- hive-finding-folded -->, #11239) since this process started.
	FoldedTracker bool `json:"folded_tracker,omitempty"`
	// Stage marks staged run work (spec, plan, implement). Empty means a plain
	// task.
	Stage string `json:"stage,omitempty"`
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
	// ClaimContext carries non-suppressing PR-claim context alongside an
	// actionable issue. It is set when the duplicate-PR guard decides that an
	// issue should be worked or triaged despite a related PR claim, but the
	// downstream agent must know about that PR before deciding what remains.
	ClaimContext *IssueClaimContext `json:"claim_context,omitempty"`
	// LinkedPRs carries API-verified PR relationships that are signals for operators
	// but do not by themselves remove the issue from the actionable set.
	LinkedPRs []IssueLinkedPR `json:"linked_prs,omitempty"`
	// Children is set on a decomposed parent: how many tracked child issues
	// it has and how many are closed (hivecommons/hive#12857). Nil when the
	// issue has no children.
	Children *IssueChildren `json:"children,omitempty"`
	// ClaimedBy / ClaimExpiresAt / ClaimSource carry a LIVE issue claim
	// (hivecommons/hive#8380) read at enumeration time: a `hive:claim` marker
	// comment, or an assignee. All three are set together and only while
	// governor.claims.enabled is on, so a hive with claims off emits an
	// envelope byte-for-byte identical to before. ClaimSource is
	// issueclaim.SourceMarker or issueclaim.SourceAssignee.
	ClaimedBy      string     `json:"claimed_by,omitempty"`
	ClaimExpiresAt *time.Time `json:"claim_expires_at,omitempty"`
	ClaimSource    string     `json:"claim_source,omitempty"`
	// ClaimKind is the marker's kind= (agent, human, contributor, external)
	// when the live claim came from a `hive:claim` marker; empty otherwise.
	ClaimKind string `json:"claim_kind,omitempty"`
	// ClaimPRStarted is the marker's pr_started= — the claim carries PR
	// evidence (the holder's PR request named the issue or its start marker
	// named a branch / PR). The issue stage reconciler reads it to tell
	// stage/030-pr-in-progress from stage/020-claimed; the claim's kind
	// alone never does (hivecommons/hive#13069).
	ClaimPRStarted bool `json:"claim_pr_started,omitempty"`
	// ClaimHive is the marker's hive= — the hive that recorded the live
	// claim — when the claim came from a `hive:claim` marker; empty otherwise.
	ClaimHive string `json:"claim_hive,omitempty"`
	// ClaimAdvisory is set when the issue carries a live contributor claim
	// the live-claim filter chose NOT to honour — the issue is in contributor
	// cool-off, or contributor claims have kept hive agents off it past the
	// starvation guard (hivecommons/hive#13579). It names the reason; the
	// issue stays actionable and hive agents may claim it.
	ClaimAdvisory string `json:"claim_advisory,omitempty"`
}

// IssueDependency is the transport form of a source-aware work dependency.
// It deliberately carries the canonical key rather than source fields so the
// key format has one implementation in pkg/worksource.
type IssueDependency struct {
	Key      string `json:"key"`
	Resolved bool   `json:"resolved"`
}

// IssueClaimContext is prompt/API evidence that an actionable issue is related
// to a PR claim that no longer suppresses the issue.
type IssueClaimContext struct {
	PRNumber       int    `json:"pr_number"`
	PRRepo         string `json:"pr_repo"`
	PRURL          string `json:"pr_url,omitempty"`
	PRAuthor       string `json:"pr_author,omitempty"`
	ExternalAuthor bool   `json:"external_author,omitempty"`
	Closing        bool   `json:"closing,omitempty"`
	Reference      bool   `json:"reference,omitempty"`
	// KeepReference prevents the stage-link safeguard from upgrading a
	// delegating tracker's Refs link to a closing keyword. Process-local.
	KeepReference bool      `json:"-"`
	MergedPR      bool      `json:"merged_pr,omitempty"`
	MergedAt      time.Time `json:"merged_at,omitempty"`
	// ReopenedAt is set with Decision ClaimDecisionReopenedAfterMerge: when a
	// person reopened the issue after the PR named here merged.
	ReopenedAt time.Time `json:"reopened_at,omitempty"`
	Decision   string    `json:"decision"`
	// CoveredLabelSynthesized records that annotateIssueWithClaim added
	// stage/040-covered-by-pr to the in-memory Issue.Labels because the
	// forge payload did not carry it. The issue stage sweep strips it again
	// before planning so the label is written when GitHub actually lacks
	// it (hivecommons/hive#13316). Process-local; never serialised.
	CoveredLabelSynthesized bool `json:"-"`
}

type PullRequest struct {
	ReporterTrustReason string `json:"reporter_trust_reason,omitempty"`
	Repo                string `json:"repo"`
	Number              int    `json:"number"`
	Title               string `json:"title"`
	Author              string `json:"author"`
	AppAuthored         bool   `json:"app_authored,omitempty"`
	// AuthorAssociation is GitHub's author_association for the PR author
	// (OWNER, MEMBER, COLLABORATOR, CONTRIBUTOR, FIRST_TIME_CONTRIBUTOR,
	// FIRST_TIMER, NONE), read from the list payload so the first-timer
	// welcome (pkg/firsttimer) costs no per-PR call.
	AuthorAssociation string `json:"author_association,omitempty"`
	// AuthorNonHuman is true when the author is a Bot account, the App's own
	// login or one of this hive's identities (the negation of isHumanAuthor).
	AuthorNonHuman bool      `json:"author_non_human,omitempty"`
	Labels         []string  `json:"labels"`
	Draft          bool      `json:"draft"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	ClosedAt       time.Time `json:"closed_at,omitempty"`
	MergedAt       time.Time `json:"merged_at,omitempty"`
	State          string    `json:"state,omitempty"`
	URL            string    `json:"url"`
	// HiveAttributed is true when the PR body carries the `— hive:`
	// attribution trailer (HasAttributionTrailer). It is how a PR a hive agent
	// opened on a PERSON's credentials — a contributor relay, or an operator
	// running agents under their own GitHub auth — is recognised as
	// hive-mediated even though GitHub shows the person as its author. The
	// review-thread reconciler keys on it alongside the author login
	// (hivecommons/hive#7638). Derived from the list payload at enumeration
	// time; the body itself is not kept, so the queue snapshot stays small.
	HiveAttributed bool   `json:"hive_attributed,omitempty"`
	HiveAgent      string `json:"hive_agent,omitempty"`
	HiveBackend    string `json:"hive_backend,omitempty"`
	HiveModel      string `json:"hive_model,omitempty"`
	// HiveRun and HivePlan are the `Hive-Run:` / `Hive-Plan:` trailer values
	// (ParseRunTrailers, hivecommons/hive#8310), read from the same list
	// payload as the attribution trailer so the plan_match review perspective
	// (#8317) can name the run and plan a PR implements without keeping the
	// body. Empty when the PR carries no run trailer.
	HiveRun  string `json:"hive_run,omitempty"`
	HivePlan string `json:"hive_plan,omitempty"`
	// ScopeContract is the linked issue or lease task the review swarm should
	// judge this PR against, derived from closing keywords before the body is
	// discarded from the actionable snapshot.
	ScopeContract string        `json:"scope_contract,omitempty"`
	BodyDigest    string        `json:"body_digest,omitempty"`
	Rework        PRReworkStats `json:"rework,omitempty"`
	// Mergeable is a tri-state: MergeableYes, MergeableNo, or MergeableUnknown.
	// It is intentionally NOT a bool: a bool zero-values to false, which is
	// indistinguishable from "GitHub says this PR cannot be merged" and would
	// silently gate every PR shut whenever the value was never populated.
	// The zero value is MergeableUnknown ("") so an unfilled field reads as
	// "we do not know" rather than a false negative.
	Mergeable Mergeable `json:"mergeable"`
	// MergeableState is GitHub's raw mergeable_state ("clean", "unstable",
	// "blocked", "dirty", ...) behind the Mergeable verdict, for display.
	// The verdict alone cannot tell an operator WHY a PR is or is not
	// eligible: "unstable" counts as MergeableYes under the standing
	// non-required-checks policy, so a ✓ on the dashboard can sit next to red
	// CI (#7471). Never consulted by any gate — mergeableFromState owns the
	// policy; this is the evidence the pill tooltip shows for it.
	MergeableState string `json:"mergeable_state,omitempty"`
	// Supersedes is the PR this one declares it supersedes ("Supersedes
	// #N" in the body: the hive's adoption / takeover / re-author copy of
	// a contributor PR), read from the list payload; 0 when none.
	// IsHiveSupersedingPR pairs it with AppAuthored.
	Supersedes int `json:"supersedes,omitempty"`
	// BaseDiffEmpty is true when the per-PR detail fetch reported zero
	// changed files, additions and deletions against the base: the PR
	// carries nothing the base does not already have (an adoption whose
	// original landed another way, hivecommons/hive#15499). Only the
	// single-PR GET carries these counts; false until EnrichCIStatus ran.
	BaseDiffEmpty bool   `json:"base_diff_empty,omitempty"`
	CIStatus      string `json:"ci_status"`
	// CIRealCheckRuns is the count of non-meta check runs observed on the head.
	// CIRealCheckRunsKnown distinguishes an observed metadata-only head from
	// older/test payloads that did not collect this evidence.
	CIRealCheckRuns      int    `json:"ci_real_check_runs,omitempty"`
	CIRealCheckRunsKnown bool   `json:"ci_real_check_runs_known,omitempty"`
	HeadSHA              string `json:"head_sha,omitempty"`
	// HeadRef is the PR's head branch name; HeadRepo is the "owner/name" the
	// head branch lives in. FromFork is true when HeadRepo differs from the
	// PR's base repository (GitHub's isCrossRepository) — or when the head
	// repository no longer exists, which is equally unpushable. The hive's
	// App token can push only to the base repository, so a fork PR can be
	// reviewed and commented on but never repaired by pushing: listing it in
	// a push-repair queue without saying so cost a scanner a whole session
	// and produced a stray branch on the base repo, pushed under the fork's
	// head-ref name (hivecommons/hive#7386).
	HeadRef  string `json:"head_ref,omitempty"`
	HeadRepo string `json:"head_repo,omitempty"`
	FromFork bool   `json:"from_fork,omitempty"`
	// MaintainerCanModify mirrors GitHub's "allow edits by maintainers"
	// switch. It is what lets the hive's App token update a fork PR branch
	// through the same branch-push surface used by in-place adoption and
	// check-report repair decisions.
	MaintainerCanModify bool `json:"maintainer_can_modify,omitempty"`
	// BaseRef is the branch the PR targets. Display only: the merge-verdict
	// reason names it ("has merge conflicts with v4 — needs a rebase") so a
	// blocked pill says what to do rather than GitHub's enum
	// (hivecommons/hive#7515). It comes from the list payload — no extra
	// fetch — and stays empty on an abbreviated payload, where the wording
	// falls back to "the base branch".
	BaseRef string `json:"base_ref,omitempty"`
	// FailingChecks names the completed check runs whose conclusion was
	// failure/action_required. CIFailureExcerpt carries the raw error lines
	// pulled from those runs' annotations — the evidence a fix agent (or an
	// escalation comment) needs; "CI failed" alone left agents guessing for
	// four days in the 2026-08 seedMission incident.
	FailingChecks    []string `json:"failing_checks,omitempty"`
	CIFailureExcerpt string   `json:"ci_failure_excerpt,omitempty"`
	// CIChecksRunning reports that CI has NOT settled: at least one non-meta
	// check on the head is still running. It is set alongside a "failure"
	// CIStatus when a shard has already failed while others report — the
	// failure is real evidence (the fix lane uses it at once), but the PR is
	// not yet stuck, so the escalation staleness clock must not start on it
	// (hivecommons/hive#9472: a PR could read as stale ten minutes after the
	// first failure while its last shard was still twelve minutes from
	// finishing).
	CIChecksRunning bool `json:"ci_checks_running,omitempty"`
	// HeadTree is the tree OID of the head commit, fetched only for red PRs.
	// It is what distinguishes a real fix attempt from a `ci: retrigger`
	// commit, which is a new SHA over an identical tree (hivecommons/hive#9473).
	HeadTree string `json:"head_tree,omitempty"`
	// SharedCIIncident is the open [shared-ci] incident issue (same repo)
	// that the newest `<!-- hive-shared-ci-<n> -->` marker on this red PR
	// defers it to; 0 when the PR carries no marker, the incident is closed,
	// or the lookup failed. Kick builders keep such a PR out of their repair
	// lists until the incident closes (hivecommons/hive#10528).
	SharedCIIncident int `json:"shared_ci_incident,omitempty"`
	// BaseRedChecks names the failing checks that are also red on the base
	// branch's tip while the PR touches no CI-owning path (base_branch_red.go).
	// When it covers every failing check (AllFailuresBaseRed) the red is the
	// base branch's, not this PR's: no 070/071/072, no fix kick, no
	// escalation attempt counted.
	BaseRedChecks []string `json:"base_red_checks,omitempty"`
	// BaseSHA is the commit on the base branch the PR targets — the tree a
	// reviewer should read to judge the change in context. It is carried so a
	// repo-grounded review can be pinned to a specific commit rather than to
	// whatever the base branch happens to be when the reviewer runs.
	//
	// This is the base branch tip as reported at enumeration time, not the
	// true merge-base of an ancestry walk. For an up-to-date PR the two are
	// the same commit, and for a stale one the base tip is the more useful
	// tree to read anyway (it is what the change will actually land on). It is
	// an advisory grounding anchor for reviews; nothing gates a merge on it.
	// The hold guard uses it only as a fast path when proving a post-update
	// head is its own merge-from-base (holdguard.IsHygieneMerge, #14534): a
	// stale or empty value falls back to a live base-branch membership check
	// and, failing that, keeps the held baseline pinned, so it fails closed.
	BaseSHA string `json:"base_sha,omitempty"`
	// ReviewClass is the presentational triage class (fix / refactor-docs /
	// tests) the queue snapshot is ordered by — see review_priority.go and
	// src/docs/review-queue-triage.md (#6183). Derived from Title + Labels
	// at enumeration time; never read by the governor or any agent.
	ReviewClass ReviewClass `json:"review_class,omitempty"`
	// ReviewRank, ReviewPriority and ReviewRankReasons are this PR's place in
	// the PR review queue (StampReviewQueue, hivecommons/hive#9590): its
	// 1-based position across every open PR (actionable and held), the
	// label-sized priority, and why it ranked there. Stamped once per eval
	// cycle before the snapshot is written; zero/empty on snapshots written by
	// other paths. The list order itself is unchanged. The optional
	// review.priority_labels toggle is the only writer that acts on them.
	ReviewRank        int            `json:"review_rank,omitempty"`
	ReviewPriority    ReviewPriority `json:"review_priority,omitempty"`
	ReviewRankReasons []string       `json:"review_rank_reasons,omitempty"`
	// Protection carries the branch-protection facts behind GitHub's
	// one-word "blocked" state — which required checks are red or absent,
	// and GitHub's own review decision. It is nil when none of it could be
	// determined, and BranchProtectionBlockReason then declines to guess
	// (hivecommons/hive#7515). Display only: no merge gate reads it.
	Protection *ProtectionFacts `json:"protection,omitempty"`
	// ReviewAddressingCommits and ReviewAddressingReplies are transient
	// follow-up routing inputs. They are populated only by callers that need
	// to decide whether the latest human CHANGES_REQUESTED review was already
	// answered; they are deliberately omitted from snapshots.
	ReviewAddressingCommits []PRCommit  `json:"-"`
	ReviewAddressingReplies []PRComment `json:"-"`
	// ReviewURL links the most recent review the HIVE posted on this PR, and
	// ReviewCount is how many it has posted in total. Both come from the
	// review-links ledger (review_links.go) rather than from GitHub, so they
	// cost no API call on a queue of hundreds of PRs and say nothing about
	// reviews by anyone else.
	//
	// Display only. Merge eligibility reads review VERDICTS, never this: a
	// posted review is evidence the hive looked, not evidence it approved.
	ReviewURL   string     `json:"review_url,omitempty"`
	ReviewState string     `json:"review_state,omitempty"`
	ReviewCount int        `json:"review_count,omitempty"`
	ReviewedAt  *time.Time `json:"reviewed_at,omitempty"`
	// RequestedReviewers and RequestedTeams are the logins and team slugs a
	// review is currently requested from, read from the list payload at
	// enumeration time — no extra call (hivecommons/hive#8968). Display
	// only: the repo-card pill names who a PR is waiting on; no gate reads
	// them.
	RequestedReviewers []string `json:"requested_reviewers,omitempty"`
	RequestedTeams     []string `json:"requested_teams,omitempty"`
	// CommentCount and ReviewThreadCount are GitHub's conversation-comment
	// and review-thread totals, and LinkedIssues are the issues GitHub
	// reports this PR will close (closingIssuesReferences). All three ride
	// the one-query-per-repository GraphQL pass that already carries the
	// review decision (protection_facts.go): no additional request, only a
	// larger response. EnrichReviewSignalsAndWorkPRs stamps them on stale
	// drafts.
	// Display only: the 🔗 #N badge on a PR pill mirrors the issue column's
	// linked-PR badge (#8876); nothing gates on any of them.
	CommentCount      int             `json:"comment_count,omitempty"`
	ReviewThreadCount int             `json:"review_thread_count,omitempty"`
	LinkedIssues      []PRLinkedIssue `json:"linked_issues,omitempty"`
	// ReferencedIssues are the same-repo issues the PR's own title and body
	// refer to (PRReferencedIssues: closing keywords GitHub did not derive,
	// `Refs #N`, `(#N, part 1)` titles, first-paragraph mentions), each
	// tagged with its Via. Read from the list payload, no extra call. The
	// activity monitor falls back to them when LinkedIssues is empty so a
	// PR that works towards an issue still sits on its row, drawn as a
	// soft link.
	ReferencedIssues []PRLinkedIssue `json:"referenced_issues,omitempty"`
}

// PRLinkedIssue is an issue GitHub reports a PR will close on merge
// (closingIssuesReferences) — the PR-side mirror of IssueLinkedPR. State is
// GitHub's issue state lower-cased ("open" / "closed"), as on Issue.State;
// StateReason is its stateReason lower-cased ("completed" / "not_planned" /
// "duplicate" / "reopened"), "" when unknown.
type PRLinkedIssue struct {
	Number      int    `json:"number"`
	Title       string `json:"title,omitempty"`
	Repo        string `json:"repo,omitempty"`
	State       string `json:"state,omitempty"`
	StateReason string `json:"state_reason,omitempty"`
	URL         string `json:"url,omitempty"`
	// Via is how the link was found: "" for GitHub's closing reference,
	// else one of the PRLinkVia* values (pr_reference_links.go).
	Via string `json:"via,omitempty"`
}
type PRReworkStats struct {
	ReviewRounds        int       `json:"review_rounds,omitempty"`
	FixAttempts         int       `json:"fix_attempts,omitempty"`
	HumanChangeRequests int       `json:"human_change_requests,omitempty"`
	FollowUpCommits     int       `json:"follow_up_commits,omitempty"`
	TimeToMergeMinutes  int       `json:"time_to_merge_minutes,omitempty"`
	FirstReviewAt       time.Time `json:"first_review_at,omitempty"`
	FirstPass           bool      `json:"first_pass,omitempty"`
	FixerModels         []string  `json:"fixer_models,omitempty"`
	FixerBackends       []string  `json:"fixer_backends,omitempty"`
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
	GeneratedAt         time.Time                    `json:"generated_at"`
	Issues              IssueResult                  `json:"issues"`
	PRs                 PRResult                     `json:"prs"`
	Hold                HoldResult                   `json:"hold"`
	Clusters            []IssueCluster               `json:"clusters,omitempty"`
	TotalByRepo         map[string]RepoCounts        `json:"total_by_repo,omitempty"`
	WorkBreakdownByRepo map[string]RepoWorkBreakdown `json:"work_breakdown_by_repo,omitempty"`
	WorkDetailsByRepo   map[string]RepoWorkDetails   `json:"work_details_by_repo,omitempty"`
	// StaleRepos lists repos whose fetch GitHub refused transiently this pass
	// (rate limit, 429, 5xx) and whose share of the result is the previous
	// successful enumeration instead (enumerate_lastgood.go). Consumers must
	// treat the snapshot as stale, not as a drop in work.
	StaleRepos []string `json:"stale_repos,omitempty"`
	// StaleSince is the time the stale share of the result was last fetched
	// for real: the data is current as of then. Zero when nothing is stale.
	StaleSince time.Time `json:"stale_since,omitzero"`
	// RestoredAt is when this process read the snapshot back from disk at
	// startup (cmd/hive bootCollectors). Set only on that restored copy, so
	// the dashboard can say "refreshing after restart" with the process's
	// own clock instead of "stale since <previous pod's time>"; the first
	// enumeration replaces the snapshot and the marker with it.
	RestoredAt time.Time `json:"restored_at,omitzero"`
	// ItemPatches records, per item, when its copy was last read from GitHub
	// and (for a webhook-driven upsert) from which delivery, so an
	// irreversible action can bound its staleness and an operator can audit
	// which delivery moved a row. It is filled by the webhook path
	// (webhook_item_patch.go) and, when the governor reads from the
	// forgestate cache (forgestate_reader.go), by every item served from
	// it, since GeneratedAt then says nothing about an item's age. Keyed by
	// ItemPatchKey.
	ItemPatches map[string]ItemPatchMeta `json:"item_patches,omitempty"`
	// WebhookPatches remembers the re-reads ApplyWebhookItem applied, keyed
	// like ItemPatches, so a reconcile pass that started before them can
	// replay them onto its result before publishing
	// (ReplayWebhookPatches). Without it a self-initiated unblock, re-read
	// into the cached snapshot while an enumeration was already listing, was
	// overwritten by that enumeration's pre-unblock copy and the row showed
	// `blocked` until the next pass. Process-local: never serialised.
	WebhookPatches map[string]WebhookItemSnapshot `json:"-"`
}

// ItemPatchMeta is the audit record of one webhook-driven upsert.
type ItemPatchMeta struct {
	FetchedAt  time.Time `json:"fetched_at"`
	UpdatedAt  time.Time `json:"updated_at,omitzero"`
	DeliveryID string    `json:"delivery_id,omitempty"`
	HeadSHA    string    `json:"head_sha,omitempty"`
}

// RepoStale reports whether repo's share of the result is last-good data.
func (r *ActionableResult) RepoStale(repo string) bool {
	if r == nil {
		return false
	}
	for _, s := range r.StaleRepos {
		if repoRefsMatch(s, repo) {
			return true
		}
	}
	return false
}

// repoRefsMatch reports whether two repo references name the same repo.
// The result keys repos the way they were configured (a bare name under
// the org, or owner/name) while action paths ask with owner/name; a bare
// name matches an owner-qualified one on the name.
func repoRefsMatch(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	_, an, aq := strings.Cut(a, "/")
	_, bn, bq := strings.Cut(b, "/")
	if aq && bq {
		return false
	}
	if !aq {
		an = a
	}
	if !bq {
		bn = b
	}
	return strings.EqualFold(an, bn)
}

// ItemFetchedAt is when the result's copy of an item was last read from
// GitHub: the webhook re-read that patched it, else the enumeration pass.
func (r *ActionableResult) ItemFetchedAt(repo string, number int, isPR bool) time.Time {
	if r == nil {
		return time.Time{}
	}
	if meta, ok := r.ItemPatches[ItemPatchKey(repo, number, isPR)]; ok && !meta.FetchedAt.IsZero() {
		return meta.FetchedAt
	}
	if _, name, qualified := strings.Cut(repo, "/"); qualified {
		// The result may key the repo by its bare configured name.
		if meta, ok := r.ItemPatches[ItemPatchKey(name, number, isPR)]; ok && !meta.FetchedAt.IsZero() {
			return meta.FetchedAt
		}
	}
	return r.GeneratedAt
}

// ItemPatchKey is the ItemPatches key for one issue or PR.
func ItemPatchKey(repo string, number int, isPR bool) string {
	kind := "issue"
	if isPR {
		kind = "pr"
	}
	return strings.ToLower(repo) + "#" + strconv.Itoa(number) + ":" + kind
}

// Stale reports whether any repo's share of the result was served from the
// last-good enumeration rather than fetched this pass.
func (r *ActionableResult) Stale() bool {
	return r != nil && len(r.StaleRepos) > 0
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
	// Attributed carries every PR in the governed repos that has the trailing
	// `— hive:` attribution footer, including merged and closed-unmerged PRs.
	// It is populated during repository enumeration so dashboard aggregate
	// endpoints and Prometheus scrapes never call GitHub at request time.
	Attributed []PullRequest `json:"attributed,omitempty"`
	// StaleDrafts are the App's OWN draft PRs older than staleDraftAfter.
	// Ordinary drafts (someone's in-progress work, human or agent) are
	// excluded from Items entirely and stay that way here — nobody should be
	// nagged about work they are actively doing. This list exists because the
	// scanner kick prompt already instructs: "Close stale drafts (>48h,
	// needs-rebase + dco-no, or fix already merged)" — but fetchPRs drops
	// EVERY draft before the agent ever sees it, so that instruction had
	// nothing to act on. See kubestellar/hive#3963.
	StaleDrafts []PullRequest `json:"stale_drafts,omitempty"`
	// Held carries the FULL PullRequest for every non-draft PR that fetchPRs
	// moved into HoldResult.Items. HoldItem has no CIStatus, head SHA or
	// failing-check names, so a held PR used to be invisible to every red-PR
	// consumer: the hold label removed it from Items before CI was ever
	// enriched, and a red held PR could therefore never be repaired — it
	// stayed red, so it stayed held (hivecommons/hive#7438).
	//
	// This is a SEPARATE list on purpose. Held PRs must not re-enter Items:
	// the merge sweep, escalation, duplicate-PR guard and the queue counts all
	// read Items, and the hold is exactly the gate that must keep them out.
	// Only the CI-repair path reads Held.
	Held []PullRequest `json:"held,omitempty"`
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
	// CreatedAt and ReviewClass exist so the hold list — the human review
	// queue at ACMM L5 hold-gated — can be ordered class-then-age
	// (SortHoldItemsForReview, #6183). Both are omitted from the JSON when
	// unset so older snapshots and held issues (which have no class) still
	// round-trip unchanged.
	CreatedAt   time.Time   `json:"created_at,omitzero"`
	ReviewClass ReviewClass `json:"review_class,omitempty"`
	// URL and Labels let the repo card show a held item as a pill of its own
	// — a link to the thing, and the label(s) that hold it, so the tooltip
	// can say WHY it is parked (`hold` alone, or alongside `needs-human`)
	// instead of only that it is (hivecommons/hive#7896). Held issues have
	// no other record than this one; held PRs also ride in PRResult.Held in
	// full. Both are omitted when unset so older snapshots round-trip.
	URL    string   `json:"url,omitempty"`
	Labels []string `json:"labels,omitempty"`
	// Assignees and HumanAcknowledged are populated for Type=="issue" holds
	// so the repo card bands a held issue the same way it bands an
	// actionable one: assigned ⇒ claimed, and an agent-filed issue a human
	// has acknowledged (#5117: approved-direction or a human assignee) is
	// not a triage item (#9019). Both come from the list response — no
	// extra API call — and are omitted when unset.
	Assignees         []string `json:"assignees,omitempty"`
	HumanAcknowledged bool     `json:"human_acknowledged,omitempty"`
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

const (
	slaThresholdMinutes = 30

	attributedClosedPRLookbackEnv = "HIVE_ATTRIBUTED_CLOSED_PR_LOOKBACK"
	// attributedClosedPRDefaultLookback covers dashboard outcome/rework
	// attribution for recent agent PRs without re-walking a repository's entire
	// closed-PR history on every enumeration tick.
	attributedClosedPRDefaultLookback = 14 * 24 * time.Hour
	attributedClosedPRMaxPages        = 5
)

type attributedClosedPRCacheEntry struct {
	prs     []PullRequest
	fetched time.Time
}

// NewClient creates a GitHub API client. If apiURL is non-empty and differs
// from the default (https://api.github.com), the client's BaseURL and
// UploadURL are overridden for GitHub Enterprise compatibility.
func NewClient(token string, org string, repos []string, logger *slog.Logger, apiURL string) *Client {
	// Proxy-trusting transport: ordinary API traffic is also MITM'd by the
	// in-process proxy under forced egress, so it must trust the proxy CA
	// (see proxytrust.go).
	client := newTokenClient(token, apiURL)
	return &Client{
		client:      client,
		org:         org,
		repos:       repos,
		logger:      logger,
		authToken:   token,
		prDetailTTL: func() time.Duration { return (config.GitHubConfig{}).PRDetailTTL() },
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

func (c *Client) SetPRDetailTTLFunc(fn func() time.Duration) {
	if c == nil {
		return
	}
	c.prDetailTTL = fn
}

func (c *Client) SetGraphQLPRBatchConfig(enabled func() bool, pageSize func() int) {
	if c == nil {
		return
	}
	c.graphQLPRBatchEnabled = enabled
	c.graphQLPRBatchPageSize = pageSize
}

func (c *Client) effectivePRDetailTTL() time.Duration {
	if c != nil && c.prDetailTTL != nil {
		if ttl := c.prDetailTTL(); ttl >= 0 {
			return ttl
		}
	}
	return (config.GitHubConfig{}).PRDetailTTL()
}

func (c *Client) cachedPRDetailFromList(repo string, number int, headSHA string, updatedAt time.Time) (*gh.PullRequest, bool) {
	return sharedPRDetailCache.get(repo, number, headSHA, updatedAt, c.effectivePRDetailTTL())
}

func (c *Client) cachedPRDetailAny(repo string, number int) (*gh.PullRequest, bool) {
	return sharedPRDetailCache.getAny(repo, number, c.effectivePRDetailTTL())
}

func (c *Client) cachedPRDetailAnyAllowUnknown(repo string, number int) (*gh.PullRequest, bool) {
	return sharedPRDetailCache.getAnyAllowUnknown(repo, number, c.effectivePRDetailTTL())
}

func (c *Client) cachedPRDetailMergedBy(repo string, number int) (*gh.PullRequest, bool) {
	return sharedPRDetailCache.getMergedBy(repo, number)
}

func (c *Client) storePRDetail(repo string, number int, pr *gh.PullRequest) {
	sharedPRDetailCache.put(repo, number, pr)
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
	// The reconcile pass is what every decision and dashboard row depends
	// on; under caution it takes precedence over periodic housekeeping.
	ctx = WithPacingClass(ctx, PacingHigh)
	now := time.Now()
	result := &ActionableResult{
		GeneratedAt: now,
	}

	var allIssues []Issue
	var allPRs []PullRequest
	var allAttributedPRs []PullRequest
	var holdItems []HoldItem
	var allHeldPRs []PullRequest
	var allStaleDrafts []PullRequest
	totalByRepo := make(map[string]RepoCounts)
	workBreakdownByRepo := make(map[string]RepoWorkBreakdown)
	workDetailsByRepo := make(map[string]RepoWorkDetails)

	// activeRepos, not getRepos: a paused repo must produce no actionable
	// issues or PRs, so no kick, claim or advisory built from this result can
	// hand an agent work on it (#6203). Enumeration is the choke point every
	// one of those paths runs through.
	repos := c.activeRepos()
	reporterTrustWaitBudget := newReporterTrustWaitBudget()
	clankerRequestedBudget := newClankerRequestedBudget()
	failedRepos := 0
	var lastFetchErr error
	var staleRepos []string
	var staleSince time.Time
	// appendRepo folds one repo's enumeration into the aggregate; shared by
	// the fresh path and the last-good substitution path so both produce the
	// same shape.
	appendRepo := func(repo string, enum repoEnumeration) {
		allIssues = append(allIssues, enum.issues...)
		holdItems = append(holdItems, enum.held...)
		allPRs = append(allPRs, enum.prs...)
		allAttributedPRs = append(allAttributedPRs, enum.attributed...)
		allHeldPRs = append(allHeldPRs, enum.heldPRs...)
		allStaleDrafts = append(allStaleDrafts, enum.staleDrafts...)
		totalByRepo[repo] = enum.counts
		workBreakdownByRepo[repo] = enum.breakdown
		workDetailsByRepo[repo] = enum.details
	}
	// serveLastGood substitutes the repo's previous successful enumeration
	// after a transient refusal. Reports whether one was available.
	serveLastGood := func(repo, side string, err error) bool {
		if !isTransientEnumerationError(err) {
			return false
		}
		enum, ok := c.lastGoodEnum.get(repo)
		if !ok {
			return false
		}
		appendRepo(repo, enum)
		staleRepos = append(staleRepos, repo)
		if staleSince.IsZero() || enum.fetchedAt.Before(staleSince) {
			staleSince = enum.fetchedAt
		}
		c.logger.Warn("GitHub refused repo enumeration transiently; serving last-good snapshot for this repo (marked stale)",
			"repo", repo, "side", side, "last_good_at", enum.fetchedAt.UTC().Format(time.RFC3339), "error", err)
		return true
	}
	itemPatches := map[string]ItemPatchMeta{}
	for _, repo := range repos {
		// With a state cache attached the listing comes from it; the
		// classification below is unchanged (forgestate_reader.go).
		listing, err := c.forgeListingForRepo(repo)
		if err != nil {
			failedRepos++
			lastFetchErr = err
			if !serveLastGood(repo, "forgestate", err) {
				c.logger.Warn("forgestate: repo cache not ready; skipping repo this pass", "repo", repo, "error", err)
			}
			continue
		}
		repoCtx := withForgeListing(ctx, listing)
		issues, held, issueTotal, issueBreakdown, issueDetails, err := c.fetchIssues(repoCtx, repo, now, reporterTrustWaitBudget)
		if err != nil {
			failedRepos++
			lastFetchErr = err
			if !serveLastGood(repo, "issues", err) {
				c.logger.Warn("failed to fetch issues", "repo", repo, "error", err)
			}
			continue
		}

		prs, heldItems, heldPRs, staleDrafts, attributedPRs, prTotal, prBreakdown, sidelinedPRs, err := c.fetchPRsWithDetails(repoCtx, repo, clankerRequestedBudget)
		if err != nil {
			// A PR-only failure is partial and must not count toward the
			// all-repos-failed guard, which would discard real issue data and
			// report an outage. With a last-good enumeration the whole repo
			// is served from it (its PR side is the newest known); without
			// one the fresh issues are kept, as before.
			if serveLastGood(repo, "prs", err) {
				continue
			}
			c.logger.Warn("failed to fetch PRs", "repo", repo, "error", err)
			allIssues = append(allIssues, issues...)
			holdItems = append(holdItems, held...)
			continue
		}
		enum := repoEnumeration{
			issues:      issues,
			held:        append(append([]HoldItem(nil), held...), heldItems...),
			prs:         prs,
			heldPRs:     heldPRs,
			staleDrafts: staleDrafts,
			attributed:  attributedPRs,
			counts:      RepoCounts{Issues: issueTotal, PRs: prTotal},
			breakdown:   RepoWorkBreakdown{Issues: issueBreakdown, PRs: prBreakdown},
			details:     RepoWorkDetails{Issues: issueDetails, PRs: sidelinedPRs},
		}
		// An issue list that came back empty beside a populated PR list is
		// verified against the repo record before it is published as zero
		// (hosted hive, 2026-10-22: every repo "0 issues · N PRs"). A list
		// that under-reports is the issue side failing, not an empty
		// backlog: the previous enumeration is served when there is one,
		// else the PRs are kept and the missing issues are reported as an
		// unlisted, stale remainder so the dashboard never prints 0.
		if issueTotal == 0 && prTotal > 0 {
			if missing, verr := c.verifyEmptyIssueList(repoCtx, repo, prTotal); verr != nil || missing > 0 {
				if verr == nil {
					verr = fmt.Errorf("%w: %s lists %d open pull requests and no issues while GitHub reports %d more open items", errIssueListIncomplete, repo, prTotal, missing)
				}
				if serveLastGood(repo, "issues", verr) {
					continue
				}
				enum.counts.Issues = missing
				appendRepo(repo, enum)
				staleRepos = append(staleRepos, repo)
				if staleSince.IsZero() || now.Before(staleSince) {
					staleSince = now
				}
				c.logger.Warn("issue list came back empty beside open pull requests and could not be confirmed; publishing the repo's issue count as unverified (marked stale)",
					"repo", repo, "prs", prTotal, "unlisted_issues", missing, "error", verr)
				continue
			}
		}
		appendRepo(repo, enum)
		if listing != nil {
			listing.itemPatches(repo, itemPatches)
			if listing.snap.Stale {
				// The cache's last reconcile failed: these are last-good
				// items, and nothing irreversible may act on them.
				staleRepos = append(staleRepos, repo)
				if since := forgeStaleSince(listing.snap); staleSince.IsZero() || since.Before(staleSince) {
					staleSince = since
				}
				c.logger.Warn("forgestate: repo cache is stale; serving last-good items (marked stale)",
					"repo", repo, "last_good_at", listing.snap.LastGoodAt.UTC().Format(time.RFC3339), "reason", listing.snap.StaleReason)
				continue
			}
		}
		c.lastGoodEnum.store(repo, enum)
		RecordGitHubActivity(newestEnumeratedUpdate(issues, prs, heldPRs))
	}

	// If every repo failed (e.g. API rate limit exhausted), returning a
	// zero-count result would tell the governor the queue is empty and
	// idle the agents. Surface the failure so callers keep prior state.
	if len(repos) > 0 && failedRepos >= len(repos) {
		return nil, fmt.Errorf("all %d repos failed to enumerate (last error: %w)", len(repos), lastFetchErr)
	}

	RankActionableIssues(allIssues)

	// Review-priority order for the PR side of the snapshot: fixes >
	// refactors/docs > tests, oldest first within a class (#6183). Purely
	// presentational — this is the order last-actionable.json and the
	// dashboard show, not a signal any agent or the governor acts on.
	SortPullRequestsForReview(allPRs)
	SortPullRequestsForReview(allHeldPRs)
	SortHoldItemsForReview(holdItems)

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
		Attributed:  allAttributedPRs,
		StaleDrafts: allStaleDrafts,
		Held:        allHeldPRs,
	}
	result.Hold = HoldResult{
		Issues: holdIssueCount,
		PRs:    holdPRCount,
		Total:  len(holdItems),
		Items:  holdItems,
	}
	result.TotalByRepo = totalByRepo
	result.WorkBreakdownByRepo = workBreakdownByRepo
	result.WorkDetailsByRepo = workDetailsByRepo
	result.StaleRepos = staleRepos
	result.StaleSince = staleSince
	if len(itemPatches) > 0 {
		result.ItemPatches = itemPatches
	}

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

func (c *Client) fetchIssues(ctx context.Context, repo string, now time.Time, reporterTrustWaitBudget *reporterTrustWaitBudget) (actionable []Issue, held []HoldItem, totalIssues int, breakdown RepoIssueBreakdown, details []RepoWorkIssue, err error) {
	issueFilter := c.getIssueFilter()
	owner, repoName := c.splitRepo(repo)

	// #9839: the list call keeps GitHub's issue_dependencies_summary so only
	// issues that report an open "blocked by" count pay for a per-issue
	// dependency fetch below.
	allIssues, blockedCounts, subIssueSummaries, err := c.listOpenIssuesForEnumeration(ctx, owner, repoName)
	if err != nil {
		return nil, nil, 0, RepoIssueBreakdown{}, nil, fmt.Errorf("listing issues for %s/%s: %w", owner, repoName, err)
	}

	// #9840: a hive-filed child the relay split out of a human-filed (or
	// human-acknowledged) parent inherits that acknowledgement. The parent is
	// resolved from this same open-issue snapshot, so no extra call per child,
	// and a closed parent (absent from the snapshot) confers nothing.
	splitParents, splitErr := LoadSplitParents("")
	if splitErr != nil {
		c.logger.Warn("issue enumeration: split-parent ledger unreadable, no inherited acknowledgement this cycle",
			slog.String("repo", repo), slog.String("error", splitErr.Error()))
		splitParents = nil
	}
	openByNumber := make(map[int]*gh.Issue, len(allIssues))
	for _, issue := range allIssues {
		if !issue.IsPullRequest() {
			openByNumber[issue.GetNumber()] = issue
		}
	}

	for _, issue := range allIssues {
		if issue.IsPullRequest() {
			continue
		}

		totalIssues++
		labels := extractLabels(issue.Labels)
		switch standingMetaIssueKindFor(issue.GetTitle(), safeGetLogin(issue.GetUser()), labels) {
		case standingMetaHiveAdvisory:
			breakdown.HiveAdvisory++
			details = append(details, c.workIssue(repo, issue, labels, "hive_advisory", "Hive advisory report"))
			continue
		case standingMetaDependencyDashboard:
			breakdown.DependencyDashboard++
			details = append(details, c.workIssue(repo, issue, labels, "dependency_dashboard", "Dependency dashboard control panel"))
			continue
		}

		// Standing meta issues — the hive's own advisory report and bot
		// dependency dashboards — are structurally not work and never enter
		// the actionable OR held sets (standing_issues.go). Before hold/exempt
		// on purpose: a hold label on the advisory report must not surface it
		// in the Hold list either.
		if reason := standingMetaIssueReason(issue.GetTitle(), safeGetLogin(issue.GetUser()), labels); reason != "" {
			continue
		}

		if c.isHeld(labels) {
			breakdown.Hold++
			held = append(held, HoldItem{
				Number:            issue.GetNumber(),
				Repo:              repo,
				Title:             issue.GetTitle(),
				Type:              "issue",
				CreatedAt:         issue.GetCreatedAt().Time,
				URL:               issue.GetHTMLURL(),
				Labels:            labels,
				Assignees:         extractAssignees(issue.Assignees),
				HumanAcknowledged: c.issueHasCheapHumanAcknowledgement(issue),
			})
			continue
		}

		if c.isExempt(labels) {
			breakdown.Filtered++
			breakdown.Exempt++
			details = append(details, c.workIssue(repo, issue, labels, "filtered", "Exempt label"))
			continue
		}
		// A tracking/archive issue (hive/tracking label, hive:tracking
		// marker or "Keep this issue open" in the body) is not work: it
		// never becomes actionable, is never offered or advertised, and
		// CloseIssue refuses it (hivecommons/hive#14613).
		if _, tracking := isTrackingIssue(issue); tracking {
			breakdown.Filtered++
			breakdown.Tracking++
			details = append(details, c.workIssue(repo, issue, labels, "filtered", trackingFilterReason))
			continue
		}
		// Blocked on an unmerged PR (hivecommons/hive#13093): out of the
		// actionable set until ReconcileBlockedIssues lifts the label. The
		// reason names the PR when the body carries the blocked-on marker,
		// else the recheck-after deadline when it carries one
		// (hivecommons/hive#13310).
		if hasBlockedLabel(labels) {
			breakdown.Filtered++
			breakdown.Blocked++
			row := c.workIssue(repo, issue, labels, "filtered", blockedFilterReason(issue.GetBody()))
			row.RecheckAfter, row.OpsTracking = blockedRecheck(issue.GetBody())
			row.RecheckDone = blockedRecheckDone(issue.GetBody())
			details = append(details, row)
			continue
		}
		if labelPresent(labels, issueNeedsReporterConfirmationLabel) {
			breakdown.Filtered++
			breakdown.ReporterConfirmation++
			details = append(details, c.workIssue(repo, issue, labels, "filtered", issueNeedsReporterConfirmationLabel))
			continue
		}
		if suppress := hardSuppressIssueBucket(issueFilter, labels); suppress != "" {
			breakdown.Filtered++
			breakdown.addHardSuppress(suppress)
			details = append(details, c.workIssue(repo, issue, labels, "filtered", suppress))
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
		// #9665 reporter trust runs FIRST: a stranger's issue needs a triage
		// label before anything else is asked of it. Counted apart from the
		// generic filter bucket so the dashboard can name the reason. Hive-
		// and bot-filed issues are not "reporters" in this sense; they go
		// through #5117 on the PR side instead, so only human authors are
		// judged here.
		if ra, ok := issueFilter.(ReporterAdmitter); ok && ra.ReporterTrustEnabled() && c.isHumanAuthor(issue.GetUser()) {
			if !ra.AdmitsReporter(labels, safeGetLogin(issue.GetUser()), issue.GetAuthorAssociation()) {
				c.markReporterTrustAwaiting(ctx, repo, issue, labels, ra, reporterTrustWaitBudget)
				breakdown.ReporterTriage++
				details = append(details, c.workIssue(repo, issue, labels, workBucketReporterTriage, reporterTriageAwaitingReason))
				continue
			}
			labels = c.clearReporterTrustAwaiting(ctx, repo, issue, labels, ra)
		}

		if !issueFilter.Admits(labels) {
			breakdown.Filtered++
			details = append(details, c.workIssue(repo, issue, labels, "filtered", "Project issue filter"))
			continue
		}

		ageMinutes := int(now.Sub(issue.GetCreatedAt().Time).Minutes())

		authorIsHuman := c.isHumanAuthor(issue.GetUser())
		acknowledged := c.issueHasCheapHumanAcknowledgement(issue)
		ackSource := ""
		if !authorIsHuman && !acknowledged {
			if parentNum, ok := c.inheritedAcknowledgement(splitParents, owner+"/"+repoName, issue, openByNumber); ok {
				acknowledged = true
				ackSource = ackSourceForParent(parentNum)
			}
		}

		// #9839: GitHub "blocked by" links become DependsOn edges. An
		// unreadable blocker list fails OPEN with a warning: hiding work
		// behind an error nobody can see is worse than offering it.
		var dependsOn []IssueDependency
		if blockedCounts[issue.GetNumber()] > 0 {
			deps, depErr := c.githubBlockedByDependencies(ctx, owner, repoName, repo, issue.GetNumber())
			if depErr != nil {
				c.logger.Warn("issue enumeration: could not read \"blocked by\" dependencies; treating the issue as unblocked this cycle",
					slog.String("repo", repo), slog.Int("number", issue.GetNumber()), slog.String("error", depErr.Error()))
			} else {
				dependsOn = deps
			}
		}

		breakdown.Actionable++
		actionable = append(actionable, Issue{
			Repo:              repo,
			Number:            issue.GetNumber(),
			Title:             issue.GetTitle(),
			Body:              issue.GetBody(),
			Author:            safeGetLogin(issue.GetUser()),
			AuthorIsHuman:     authorIsHuman,
			HumanAcknowledged: acknowledged,
			AckSource:         ackSource,
			Labels:            labels,
			Assignees:         extractAssignees(issue.Assignees),
			Reopened:          strings.EqualFold(issue.GetStateReason(), issueStateReasonReopened),
			CreatedAt:         issue.GetCreatedAt().Time,
			UpdatedAt:         issue.GetUpdatedAt().Time,
			AgeMinutes:        ageMinutes,
			URL:               issue.GetHTMLURL(),
			IsTracker:         isTracker(issue.GetTitle(), labels, issue.GetBody()),
			DependsOn:         dependsOn,
		})
	}
	dropMutualBlocks(actionable, c.logger)
	annotateIssueChildren(repo, subIssueSummaries, splitParents, openByNumber, actionable, details)

	// Keep an honest fallback if a future exclusion path is added without a
	// dedicated category. This is the same enumeration snapshot, not a second
	// scan or a browser-side subtraction.
	if unclassified := totalIssues - breakdown.Total(); unclassified > 0 {
		breakdown.Other += unclassified
	}
	// #8380: decorate the actionable set with any live issue claim. A no-op
	// (no fetch, no fields) unless governor.claims.enabled is on.
	c.annotateIssueClaims(ctx, owner, repoName, actionable, now)
	c.annotateFoldedTrackers(actionable)
	return actionable, held, totalIssues, breakdown, details, nil
}

// staleDraftAfter matches the age threshold the scanner kick prompt already
// names ("Close stale drafts (>48h, ...)"). A draft newer than this may still
// be someone's active work-in-progress; older than this, it is more likely an
// agent that opened a WIP PR and moved on. Only the App's OWN drafts are aged
// this way — a human's stale draft is their call, not ours to nag about.
const staleDraftAfter = 48 * time.Hour

// prHeadSHA reads a PR's head commit, tolerating a nil head.
func prHeadSHA(pr *gh.PullRequest) string {
	if pr.GetHead() == nil {
		return ""
	}
	return pr.GetHead().GetSHA()
}

// prBaseRef is the branch a PR targets, or "" on an abbreviated payload.
func prBaseRef(pr *gh.PullRequest) string {
	if pr.GetBase() == nil {
		return ""
	}
	return pr.GetBase().GetRef()
}

// prRequestedReviews reads the logins and team slugs a review is requested
// from, as carried by the list payload. nil slices when none, so the JSON
// field is omitted rather than emitted as [].
func prRequestedReviews(pr *gh.PullRequest) (logins, teams []string) {
	for _, u := range pr.RequestedReviewers {
		if login := safeGetLogin(u); login != "" {
			logins = append(logins, login)
		}
	}
	for _, t := range pr.RequestedTeams {
		if t == nil {
			continue
		}
		if slug := t.GetSlug(); slug != "" {
			teams = append(teams, slug)
		} else if name := t.GetName(); name != "" {
			teams = append(teams, name)
		}
	}
	return logins, teams
}

func reviewScopeContract(title, body, defaultRepo, runKey, planRef, headRef string, teamKeys []string) string {
	refs := ParseClaimedIssues(title+"\n"+body, defaultRepo)
	if len(refs) > 0 {
		ref := refs[0]
		if strings.EqualFold(ref.Repo, defaultRepo) {
			return fmt.Sprintf("linked issue %s#%d from the PR closing keyword", ref.Repo, ref.Issue)
		}
		return fmt.Sprintf("linked issue %s#%d from the PR closing keyword", ref.Repo, ref.Issue)
	}
	if strings.TrimSpace(runKey) != "" {
		if strings.TrimSpace(planRef) != "" {
			return fmt.Sprintf("lease task Hive-Run %s / Hive-Plan %s", strings.TrimSpace(runKey), strings.TrimSpace(planRef))
		}
		return fmt.Sprintf("lease task Hive-Run %s", strings.TrimSpace(runKey))
	}
	return linearScopeContract(title, body, headRef, teamKeys)
}

// SetLinearScopeTeams maps configured Linear teams to their repositories.
func (c *Client) SetLinearScopeTeams(teams []config.LinearTeamSourceConfig) {
	mapped := make(map[string][]string)
	for _, team := range teams {
		for _, repo := range append([]string{team.Repo}, linearProjectRepos(team.Projects)...) {
			if repo != "" && team.Key != "" {
				mapped[strings.ToLower(repo)] = append(mapped[strings.ToLower(repo)], strings.ToUpper(team.Key))
			}
		}
	}
	c.reposMu.Lock()
	c.linearTeams = mapped
	c.reposMu.Unlock()
}

func linearProjectRepos(projects []config.LinearProjectSourceConfig) []string {
	var repos []string
	for _, project := range projects {
		repos = append(repos, project.Repo)
	}
	return repos
}

func (c *Client) linearScopeTeams(repo string) []string {
	c.reposMu.RLock()
	defer c.reposMu.RUnlock()
	return append([]string(nil), c.linearTeams[strings.ToLower(repo)]...)
}

var linearBodyScope = regexp.MustCompile(`(?i)\b(?:fixes|closes|part of)\s+([a-z][a-z0-9]*-[0-9]+)\b`)
var linearTitleScope = regexp.MustCompile(`(?i)^\s*\[([a-z][a-z0-9]*-[0-9]+)\]`)
var linearBranchScope = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])([a-z][a-z0-9]*-[0-9]+)(?:$|[^a-z0-9])`)

func linearScopeContract(title, body, headRef string, keys []string) string {
	for _, source := range []struct {
		pattern *regexp.Regexp
		text    string
	}{
		{linearBodyScope, body}, {linearTitleScope, title}, {linearBranchScope, headRef},
	} {
		for _, match := range source.pattern.FindAllStringSubmatch(source.text, -1) {
			id := strings.ToUpper(match[1])
			key := id[:strings.LastIndexByte(id, '-')]
			for _, allowed := range keys {
				if strings.EqualFold(key, allowed) {
					return "linked Linear issue " + id
				}
			}
		}
	}
	return ""
}

// fetchPRs retains the legacy return shape for callers that only need queue
// partitions. The enumeration path calls fetchPRsWithDetails so the same
// open-PR list also yields display-only records for the activity monitor.
func (c *Client) fetchPRs(ctx context.Context, repo string, clankerBudget *reporterTrustWaitBudget) (actionable []PullRequest, held []HoldItem, heldPRs []PullRequest, staleDrafts []PullRequest, attributed []PullRequest, totalPRs int, breakdown RepoPRBreakdown, err error) {
	actionable, held, heldPRs, staleDrafts, attributed, totalPRs, breakdown, _, err = c.fetchPRsWithDetails(ctx, repo, clankerBudget)
	return actionable, held, heldPRs, staleDrafts, attributed, totalPRs, breakdown, err
}

// fetchPRsWithDetails is fetchPRs plus the display-only records of the open
// PRs it sidelined (drafts, exempt labels, held drafts) so the snapshot can
// account for every open PR of the repo without another list call.
func (c *Client) fetchPRsWithDetails(ctx context.Context, repo string, clankerBudget *reporterTrustWaitBudget) (actionable []PullRequest, held []HoldItem, heldPRs []PullRequest, staleDrafts []PullRequest, attributed []PullRequest, totalPRs int, breakdown RepoPRBreakdown, sidelined []RepoWorkPR, err error) {
	now := time.Now()
	owner, repoName := c.splitRepo(repo)
	allPRs, err := c.listOpenPRsForEnumeration(ctx, owner, repoName)
	if err != nil {
		return nil, nil, nil, nil, nil, 0, RepoPRBreakdown{}, nil, err
	}
	c.prefetchOpenPRDetailsGraphQL(ctx, owner, repoName, repo)

	for _, pr := range allPRs {
		totalPRs++
		labels := extractPRLabels(pr.Labels)
		// Parked before the hold check so a freshly labelled PR lands in the
		// held partition on this same poll (hivecommons/hive#10781).
		labels = c.parkClankerRequestedPR(ctx, repo, pr, labels, clankerBudget)
		labels, reporterTrustReason := c.reconcileReporterTrustSignal(ctx, repo, pr, labels)
		labels = c.clearReleasedReporterTrustNeedsHuman(ctx, repo, pr.GetNumber(), labels)
		attrMeta, hasAttr := ParseAttributionTrailer(pr.GetBody())
		runKey, planRef := ParseRunTrailers(pr.GetBody())
		headRef, _, _ := prHeadOrigin(pr)
		scopeContract := reviewScopeContract(pr.GetTitle(), pr.GetBody(), owner+"/"+repoName, runKey, planRef, headRef, c.linearScopeTeams(owner+"/"+repoName))

		if c.isHeld(labels) {
			breakdown.Hold++
			heldHeadSHA := ""
			if pr.GetHead() != nil {
				heldHeadSHA = pr.GetHead().GetSHA()
			}
			held = append(held, HoldItem{
				Number:      pr.GetNumber(),
				Repo:        repo,
				Title:       pr.GetTitle(),
				Type:        "pr",
				CreatedAt:   pr.GetCreatedAt().Time,
				HeadSHA:     heldHeadSHA,
				Author:      safeGetLogin(pr.GetUser()),
				ReviewClass: ClassifyReviewClass(pr.GetTitle(), labels),
				URL:         pr.GetHTMLURL(),
				Labels:      labels,
			})
			// Also keep the full PR so the CI-repair path can see whether a
			// held PR is red (hivecommons/hive#7438). Drafts stay out, exactly
			// as they do for the actionable list. This slice never feeds the
			// merge sweep — the hold gate is untouched.
			if !pr.GetDraft() {
				headRef, headRepo, fromFork := prHeadOrigin(pr)
				reqLogins, reqTeams := prRequestedReviews(pr)
				if reporterTrustReason == "" {
					reporterTrustReason = c.reporterTrustHeldPRReason(ctx, owner, repoName, pr.GetNumber(), labels)
				}
				heldPRs = append(heldPRs, PullRequest{
					Repo:                repo,
					Number:              pr.GetNumber(),
					Title:               pr.GetTitle(),
					Author:              safeGetLogin(pr.GetUser()),
					Labels:              labels,
					CreatedAt:           pr.GetCreatedAt().Time,
					UpdatedAt:           pr.GetUpdatedAt().Time,
					State:               pr.GetState(),
					URL:                 pr.GetHTMLURL(),
					ReviewClass:         ClassifyReviewClass(pr.GetTitle(), labels),
					HiveAttributed:      hasAttr,
					HiveAgent:           attrMeta.Agent,
					HiveBackend:         attrMeta.Backend,
					HiveModel:           attrMeta.Model,
					HiveRun:             runKey,
					HivePlan:            planRef,
					ScopeContract:       scopeContract,
					ReporterTrustReason: reporterTrustReason,
					HeadSHA:             prHeadSHA(pr),
					BaseSHA:             pr.GetBase().GetSHA(),
					HeadRef:             headRef,
					HeadRepo:            headRepo,
					FromFork:            fromFork,
					MaintainerCanModify: pr.GetMaintainerCanModify(),
					BaseRef:             prBaseRef(pr),
					RequestedReviewers:  reqLogins,
					RequestedTeams:      reqTeams,
					ReferencedIssues:    PRReferencedIssues(pr.GetTitle(), pr.GetBody(), repo, pr.GetNumber()),
					// The review swarm reviews held PRs too; without the digest
					// their kicks carry no "Review body digest:" line and their
					// verdicts split across aggregates (hivecommons/hive#14594).
					BodyDigest: review.PullRequestBodyDigest(pr.GetTitle(), pr.GetBody()),
				})
			} else {
				sidelined = append(sidelined, c.repoWorkPR(repo, pr, labels, hasAttr, workBucketPRDraft, "Held draft"))
			}
			continue
		}

		if c.isExempt(labels) {
			breakdown.Filtered++
			sidelined = append(sidelined, c.repoWorkPR(repo, pr, labels, hasAttr, workBucketPRFiltered, "Exempt label"))
			continue
		}

		if pr.GetDraft() {
			breakdown.Draft++
			sidelined = append(sidelined, c.repoWorkPR(repo, pr, labels, hasAttr, workBucketPRDraft, "Draft"))
			author := safeGetLogin(pr.GetUser())
			if strings.EqualFold(author, c.appBotLogin) && now.Sub(pr.GetCreatedAt().Time) > staleDraftAfter {
				reqLogins, reqTeams := prRequestedReviews(pr)
				staleDrafts = append(staleDrafts, PullRequest{
					Repo:               repo,
					Number:             pr.GetNumber(),
					Title:              pr.GetTitle(),
					Author:             author,
					Labels:             labels,
					Draft:              true,
					CreatedAt:          pr.GetCreatedAt().Time,
					UpdatedAt:          pr.GetUpdatedAt().Time,
					URL:                pr.GetHTMLURL(),
					ReviewClass:        ClassifyReviewClass(pr.GetTitle(), labels),
					RequestedReviewers: reqLogins,
					RequestedTeams:     reqTeams,
					ReferencedIssues:   PRReferencedIssues(pr.GetTitle(), pr.GetBody(), repo, pr.GetNumber()),
				})
			}
			continue
		}

		headSHA := ""
		if pr.GetHead() != nil {
			headSHA = pr.GetHead().GetSHA()
		}
		headRef, headRepo, fromFork := prHeadOrigin(pr)
		baseSHA := ""
		if pr.GetBase() != nil {
			baseSHA = pr.GetBase().GetSHA()
		}
		breakdown.Actionable++
		author := safeGetLogin(pr.GetUser())
		reqLogins, reqTeams := prRequestedReviews(pr)

		actionable = append(actionable, PullRequest{
			Repo:              repo,
			Number:            pr.GetNumber(),
			Title:             pr.GetTitle(),
			Author:            author,
			AppAuthored:       strings.EqualFold(author, c.appBotLogin) && c.appBotLogin != "",
			AuthorAssociation: pr.GetAuthorAssociation(),
			AuthorNonHuman:    !c.isHumanAuthor(pr.GetUser()),
			Labels:            labels,
			Draft:             pr.GetDraft(),
			CreatedAt:         pr.GetCreatedAt().Time,
			UpdatedAt:         pr.GetUpdatedAt().Time,
			State:             pr.GetState(),
			URL:               pr.GetHTMLURL(),
			ReviewClass:       ClassifyReviewClass(pr.GetTitle(), labels),
			// The list payload carries the body, so the hive-mediated test
			// costs no extra call (hivecommons/hive#7638).
			HiveAttributed: hasAttr,
			HiveAgent:      attrMeta.Agent,
			HiveBackend:    attrMeta.Backend,
			HiveModel:      attrMeta.Model,
			HiveRun:        runKey,
			HivePlan:       planRef,
			ScopeContract:  scopeContract,
			BodyDigest:     review.PullRequestBodyDigest(pr.GetTitle(), pr.GetBody()),
			// Mergeable is deliberately NOT set here. The PullRequests.List
			// endpoint never populates "mergeable" — GitHub computes it
			// per-PR and returns it only from the single-PR GET. Reading it
			// here would yield false for every PR. EnrichCIStatus fills it in
			// from a per-PR fetch; until then it stays MergeableUnknown.
			MergeableState:     pr.GetMergeableState(),
			HeadSHA:            headSHA,
			BaseSHA:            baseSHA,
			HeadRef:            headRef,
			HeadRepo:           headRepo,
			FromFork:           fromFork,
			BaseRef:            prBaseRef(pr),
			RequestedReviewers: reqLogins,
			RequestedTeams:     reqTeams,
			ReferencedIssues:   PRReferencedIssues(pr.GetTitle(), pr.GetBody(), repo, pr.GetNumber()),
			Supersedes:         prSupersedesNumberOrZero(pr.GetBody()),
		})
	}

	attributed = append(attributed, collectAttributedPRsFromList(repo, allPRs)...)
	closed, err := c.fetchAttributedClosedPRsForEnumeration(ctx, owner, repoName, repo)
	if err != nil {
		c.warnAttributedClosedPRScan(repo, owner, repoName, err)
		closed = c.cachedAttributedClosedPRs(repo)
	}
	attributed = append(attributed, closed...)
	c.enrichAttributedPRRework(ctx, attributed)

	if unclassified := totalPRs - breakdown.Total(); unclassified > 0 {
		breakdown.Other += unclassified
	}

	return actionable, held, heldPRs, staleDrafts, attributed, totalPRs, breakdown, sidelined, nil
}

func collectAttributedPRsFromList(repo string, prs []*gh.PullRequest) []PullRequest {
	var out []PullRequest
	for _, pr := range prs {
		meta, ok := ParseAttributionTrailer(pr.GetBody())
		if !ok {
			continue
		}
		out = append(out, pullRequestAttributionRecord(repo, pr, meta))
	}
	return out
}

func (c *Client) fetchAttributedClosedPRs(ctx context.Context, owner, repoName, repo string) ([]PullRequest, error) {
	lookback := attributedClosedPRLookback()
	cutoff := time.Now().Add(-lookback)
	opts := &gh.PullRequestListOptions{
		State:       "closed",
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var out []PullRequest
	for page := 0; page < attributedClosedPRMaxPages; page++ {
		prs, resp, err := c.client.PullRequests.List(ctx, owner, repoName, opts)
		if err != nil {
			return nil, fmt.Errorf("listing closed PRs for %s/%s: %w", owner, repoName, err)
		}
		pastWindow := false
		for _, pr := range prs {
			if pr == nil {
				continue
			}
			if pr.GetUpdatedAt().Time.Before(cutoff) {
				pastWindow = true
				break
			}
			if meta, ok := ParseAttributionTrailer(pr.GetBody()); ok {
				out = append(out, pullRequestAttributionRecord(repo, pr, meta))
			}
		}
		if pastWindow || resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	c.storeAttributedClosedPRs(repo, out)
	return out, nil
}

// AttributedClosedPRLookback is the updated-time window the closed attributed
// PR scan covers, so consumers can report the history they actually hold.
func AttributedClosedPRLookback() time.Duration {
	return attributedClosedPRLookback()
}

func attributedClosedPRLookback() time.Duration {
	raw := strings.TrimSpace(os.Getenv(attributedClosedPRLookbackEnv))
	if raw == "" {
		return attributedClosedPRDefaultLookback
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if strings.HasSuffix(raw, "d") {
		if days, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(raw, "d"))); err == nil && days > 0 {
			return time.Duration(days) * 24 * time.Hour
		}
	}
	if days, err := strconv.Atoi(raw); err == nil && days > 0 {
		return time.Duration(days) * 24 * time.Hour
	}
	return attributedClosedPRDefaultLookback
}

func (c *Client) storeAttributedClosedPRs(repo string, prs []PullRequest) {
	if c == nil {
		return
	}
	c.attributedClosedPRMu.Lock()
	defer c.attributedClosedPRMu.Unlock()
	if c.attributedClosedPRCache == nil {
		c.attributedClosedPRCache = map[string]attributedClosedPRCacheEntry{}
	}
	c.attributedClosedPRCache[repo] = attributedClosedPRCacheEntry{
		prs:     append([]PullRequest(nil), prs...),
		fetched: time.Now(),
	}
	if c.attributedClosedPRErrors != nil {
		delete(c.attributedClosedPRErrors, repo)
	}
}

func (c *Client) cachedAttributedClosedPRs(repo string) []PullRequest {
	prs, _, _ := c.cachedAttributedClosedPRsWithAge(repo)
	return prs
}

// cachedAttributedClosedPRsWithAge is cachedAttributedClosedPRs plus when
// the scan ran; ok is false when there is no scan for repo.
func (c *Client) cachedAttributedClosedPRsWithAge(repo string) ([]PullRequest, time.Time, bool) {
	if c == nil {
		return nil, time.Time{}, false
	}
	c.attributedClosedPRMu.Lock()
	defer c.attributedClosedPRMu.Unlock()
	entry, ok := c.attributedClosedPRCache[repo]
	if !ok {
		return nil, time.Time{}, false
	}
	return append([]PullRequest(nil), entry.prs...), entry.fetched, true
}

func (c *Client) warnAttributedClosedPRScan(repo, owner, repoName string, err error) {
	if c == nil {
		return
	}
	msg := err.Error()
	shouldLog := true
	c.attributedClosedPRMu.Lock()
	if c.attributedClosedPRErrors == nil {
		c.attributedClosedPRErrors = map[string]string{}
	}
	if c.attributedClosedPRErrors[repo] == msg {
		shouldLog = false
	} else {
		c.attributedClosedPRErrors[repo] = msg
	}
	c.attributedClosedPRMu.Unlock()
	if shouldLog && c.logger != nil {
		c.logger.Warn("closed PR attribution scan failed; keeping open PR enumeration and last-good closed attributions",
			"repo", repo, "github_repo", owner+"/"+repoName, "error", err)
	}
}

func pullRequestAttributionRecord(repo string, pr *gh.PullRequest, meta InvocationMeta) PullRequest {
	return PullRequest{
		Repo:           repo,
		Number:         pr.GetNumber(),
		Title:          pr.GetTitle(),
		Author:         safeGetLogin(pr.GetUser()),
		Labels:         extractPRLabels(pr.Labels),
		Draft:          pr.GetDraft(),
		CreatedAt:      pr.GetCreatedAt().Time,
		UpdatedAt:      pr.GetUpdatedAt().Time,
		ClosedAt:       pr.GetClosedAt().Time,
		MergedAt:       pr.GetMergedAt().Time,
		State:          pr.GetState(),
		URL:            pr.GetHTMLURL(),
		HiveAttributed: true,
		HiveAgent:      meta.Agent,
		HiveBackend:    meta.Backend,
		HiveModel:      meta.Model,
	}
}

// EnrichCIStatus fetches check-run results for each PR's HEAD commit
// and sets the CIStatus field to "success", "failure", or "pending".
// The "tide" check is skipped — it reports Prow merge-bot state, not CI.
//
// It also collects the branch-protection facts behind a "blocked" PR
// (hivecommons/hive#7515, step 2) — which required checks are red or have
// never reported, and GitHub's own review decision — so that the dashboard
// pill can name the unsatisfied rule instead of repeating the word
// "blocked". Those facts are gathered from data this pass already walks
// plus at most one GraphQL query per repository; nothing here is per-hover
// and nothing here is per-PR beyond the calls that were already made.
//
// For each red PR it also resolves SharedCIIncident (hivecommons/hive#10528):
// one comment listing per red PR, plus one issue GET per distinct incident
// number named by a `<!-- hive-shared-ci-<n> -->` marker in this pass — and
// BaseRedChecks (base_branch_red.go): one check-run listing per distinct
// base branch, plus one file listing per red PR whose failures overlap it.
func (c *Client) EnrichCIStatus(ctx context.Context, prs []PullRequest) {
	if c == nil {
		return
	}
	facts := newProtectionCollector(c)
	incidents := map[string]bool{}
	for i := range prs {
		reported := c.enrichPRCI(ctx, &prs[i])
		if prs[i].CIStatus == "failure" {
			prs[i].SharedCIIncident = c.openSharedCIIncident(ctx, &prs[i], incidents)
		}
		facts.attach(ctx, &prs[i], reported)
	}
	c.enrichBaseBranchRed(ctx, prs)
}

// EnrichReviewSignalsAndWorkPRs stamps the review-decision and repo-card
// triage signals (protection.review_decision, comment/thread counts, linked
// issues — hivecommons/hive#8968) onto prs, and GitHub's closing-issue
// references onto the sidelined PRs (drafts, exempt-labelled) in details so
// the activity monitor can pair them with the issue they close just as it
// does for actionable PRs (hivecommons/hive#13421). Both run over one shared
// per-cycle collector: at most one GraphQL query per repository even when a
// repo has both stale drafts and sidelined PRs, none when the PR batch
// already answered it, and no per-PR REST calls. prs is the stale-draft
// list, which the dashboard shows beside the actionable PRs but which
// EnrichCIStatus deliberately never touches: a draft is not a merge
// candidate, so its mergeability and check runs are never fetched. Either
// argument may be nil; the slices are updated in place.
func (c *Client) EnrichReviewSignalsAndWorkPRs(ctx context.Context, prs []PullRequest, details map[string]RepoWorkDetails) {
	if c == nil {
		return
	}
	facts := newProtectionCollector(c)
	for i := range prs {
		facts.attach(ctx, &prs[i], nil)
	}
	for repo, d := range details {
		for i := range d.PRs {
			pr := &d.PRs[i]
			if rs, ok := facts.reviewState(ctx, repo, pr.Number); ok {
				pr.LinkedIssues = rs.linkedIssues
			}
		}
	}
}

// enrichPRCI is EnrichCIStatus's per-PR body. It returns the names of every
// check run observed on the PR's head SHA — meta checks included, because a
// required status-check context may well be one of them — or nil when no
// check-run listing was obtained at all. The caller distinguishes the two:
// "no listing" must never be read as "the required check never reported".
func (c *Client) enrichPRCI(ctx context.Context, pr *PullRequest) map[string]bool {
	const ciStatusSuccess = "success"
	const ciStatusFailure = "failure"
	const ciStatusPending = "pending"

	if pr.HeadSHA == "" {
		pr.CIStatus = ciStatusPending
		return nil
	}
	owner, repoName := c.splitRepo(pr.Repo)

	// The list endpoint that produced this PullRequest carries a cheap,
	// ETag-friendly head SHA and updated_at but not mergeability. Reuse a
	// detail response only while those list fields still match; merge decisions
	// below still re-fetch fresh details before mutating.
	full, ok := c.cachedPRDetailFromList(pr.Repo, pr.Number, pr.HeadSHA, pr.UpdatedAt)
	if !ok {
		var err error
		started := sharedWebhookTracker.now()
		full, _, err = c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:enrich_pr_ci"), owner, repoName, pr.Number)
		if err != nil {
			c.logger.Warn("failed to fetch PR mergeability", "repo", pr.Repo, "pr", pr.Number, "error", err)
		} else {
			c.storePRDetail(pr.Repo, pr.Number, full)
			sharedWebhookTracker.clearDirtyBefore(pr.Repo, pr.Number, started)
		}
	}
	if full != nil {
		pr.Mergeable = mergeableFromState(full.GetMergeableState(), full.Mergeable)
		pr.MergeableState = full.GetMergeableState()
		pr.MaintainerCanModify = full.GetMaintainerCanModify()
		pr.BaseDiffEmpty = PRChangedFilesClaimsEmpty(full)
		if base := full.GetBase().GetSHA(); base != "" {
			pr.BaseSHA = base
		}
	}

	var allCheckRuns []*gh.CheckRun
	if cached, ok := c.graphQLBatchCheckRuns(pr.Repo, pr.Number, pr.HeadSHA); ok {
		allCheckRuns = cached
	} else {
		checkRuns, _, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, repoName, pr.HeadSHA, &gh.ListCheckRunsOptions{
			ListOptions: gh.ListOptions{PerPage: 100},
		})
		if err != nil {
			c.logger.Warn("failed to fetch check runs", "repo", pr.Repo, "pr", pr.Number, "error", err)
			pr.CIStatus = ciStatusPending
			return nil
		}
		allCheckRuns = checkRuns.CheckRuns
	}
	latestCheckRuns := latestCheckRunsByNameAndApp(allCheckRuns)
	// Snapshot the CI verdict for the evidence bundle once it is known. Never
	// affects the verdict or any merge decision.
	// Keyed by owner/name: pr.Repo may be the bare repo name, and a bundle
	// filed under it validates as "repo must be owner/name" and is dropped,
	// leaving the lane's bundle at <owner>/<name>/<n>/<head>.json without
	// its CI snapshot.
	defer func() {
		c.recordReviewEvidenceCI(owner+"/"+repoName, pr.Number, pr.HeadSHA, pr.CIStatus, full, latestCheckRuns)
	}()
	reported := make(map[string]bool, len(latestCheckRuns))
	for _, cr := range latestCheckRuns {
		if name := cr.GetName(); name != "" {
			reported[name] = true
		}
	}
	if len(latestCheckRuns) == 0 {
		pr.CIStatus = ciStatusPending
		return reported
	}
	hasFail := false
	allDone := true
	ciChecksFound := 0
	pr.CIRealCheckRunsKnown = true
	dcoSatisfied := dcoContextSucceeded(nil, latestCheckRuns)
	var failingNames []string
	var failingIDs []int64
	for _, cr := range latestCheckRuns {
		if isMetaCheck(cr.GetName()) || (isDCOContext(cr.GetName()) && dcoSatisfied) {
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
	pr.CIRealCheckRuns = ciChecksFound
	if ciChecksFound == 0 {
		pr.CIStatus = ciStatusPending
		return reported
	}
	switch {
	case hasFail:
		pr.CIStatus = ciStatusFailure
		pr.FailingChecks = failingNames
		// "Failed, others still running" is a materially different state from
		// "failed, all done": only the latter means the PR is stuck rather
		// than still reporting (hivecommons/hive#9472).
		pr.CIChecksRunning = !allDone
		pr.CIFailureExcerpt = c.fetchFailureExcerpt(ctx, owner, repoName, failingIDs, failingNames)
		pr.HeadTree = c.headTreeOID(ctx, owner, repoName, pr)
	case allDone:
		pr.CIStatus = ciStatusSuccess
	default:
		pr.CIStatus = ciStatusPending
	}
	return reported
}

// headTreeOID returns the tree OID of a red PR's head commit so the escalation
// ledger can count fix ATTEMPTS rather than commits: an empty `ci: retrigger`
// commit is a fresh SHA over an identical tree and is not an attempt at a fix
// (hivecommons/hive#9473). It reuses the duplicate-guard's cached commit→tree
// lookup, so a head whose tree has already been resolved costs nothing, and a
// failure is not an error: the ledger falls back to per-SHA counting when the
// tree is unknown.
func (c *Client) headTreeOID(ctx context.Context, owner, repoName string, pr *PullRequest) string {
	tree, err := c.commitTreeSHA(WithRESTCaller(ctx, "hive:head_tree_oid"), owner, repoName, pr.HeadSHA)
	if err != nil {
		c.logger.Debug("failed to fetch head commit tree", "repo", pr.Repo, "pr", pr.Number, "error", err)
		return ""
	}
	return tree
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

// Bounds for fetchFailureExcerpt: annotations are fetched for at most
// excerptMaxRuns failing check runs, keeping at most excerptMaxLines lines and
// excerptMaxChars characters total. Enough to carry a stack of real errors
// into a kick prompt without flooding it.
const (
	excerptMaxRuns  = 8
	excerptMaxLines = 8
	excerptMaxChars = 1800
)

// exitCodeOnlyAnnotation matches the GitHub Actions "step failed" annotation
// GitHub attaches to every failing job regardless of the real cause (for
// example "Process completed with exit code 1."). It carries no diagnostic
// value, so it is only kept when a run has nothing else to offer.
var exitCodeOnlyAnnotation = regexp.MustCompile(`(?i)^Process completed with exit code \d+\.?$`)

// fetchFailureExcerpt pulls the raw error lines from the annotations of the
// given failing check runs. Annotations capture workflow ##[error] lines (test
// failures, compile errors), which is exactly the evidence fix agents never
// see from a bare "CI failed" status. Works on GitHub and GHE through the
// client's configured API base; on errors it degrades to "" — the excerpt is
// an enrichment, never a gate.
//
// Runs are read up to excerptMaxRuns (runIDs is expected in a deterministic
// order — see latestCheckRunsByNameAndApp — so which runs get covered does
// not vary from call to call), and the resulting lines are filled
// round-robin: one line per run before any run contributes a second, so a
// single noisy shard cannot crowd every other failing check out of the
// excerpt.
func (c *Client) fetchFailureExcerpt(ctx context.Context, owner, repo string, runIDs []int64, runNames []string) string {
	seen := map[string]bool{}
	perRun := make([][]string, 0, len(runIDs))
	for idx, id := range runIDs {
		if idx >= excerptMaxRuns {
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
		var msgs []string
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
			if nl := strings.IndexByte(msg, '\n'); nl > 0 {
				msg = msg[:nl]
			}
			if len(msg) > 300 {
				msg = msg[:300]
			}
			msgs = append(msgs, msg)
		}
		// A bare "exit code N" annotation only earns a slot when the run
		// has no more informative message to report.
		if len(msgs) > 1 {
			kept := msgs[:0]
			for _, m := range msgs {
				if !exitCodeOnlyAnnotation.MatchString(m) {
					kept = append(kept, m)
				}
			}
			if len(kept) > 0 {
				msgs = kept
			}
		}
		if name != "" {
			for i, m := range msgs {
				msgs[i] = name + ": " + m
			}
		}
		if len(msgs) > 0 {
			perRun = append(perRun, msgs)
		}
	}

	var lines []string
	total := 0
	for round := 0; len(lines) < excerptMaxLines; round++ {
		added := false
		for _, msgs := range perRun {
			if round >= len(msgs) {
				continue
			}
			line := msgs[round]
			if total+len(line) > excerptMaxChars {
				return strings.Join(lines, "\n")
			}
			lines = append(lines, line)
			total += len(line)
			added = true
			if len(lines) >= excerptMaxLines {
				break
			}
		}
		if !added {
			break
		}
	}
	return strings.Join(lines, "\n")
}

// IssueCommentsContain reports whether any issue comment body contains needle.
func (c *Client) IssueCommentsContain(ctx context.Context, repo string, number int, needle string) (bool, error) {
	if c == nil {
		return false, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repoName, number, opts)
		if err != nil {
			return false, err
		}
		for _, comment := range comments {
			if strings.Contains(comment.GetBody(), needle) {
				return true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return false, nil
		}
		opts.Page = resp.NextPage
	}
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
	// ExecuteReconciling, not Execute: an ambiguous POST failure journals the
	// op Unknown, and without an observation every later post of the same
	// comment was refused forever (hivecommons/hive#14011).
	_, err := effects.ExecuteReconciling(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + repoName,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Inputs: map[string]string{"body": effects.StableDigest(body)},
	}, func(ctx context.Context) (effects.ExternalState, error) {
		return c.observeIssueComment(ctx, owner, repoName, number, body)
	}, effects.DefaultUnresolvedTTL, func(ctx context.Context) (effects.Result, error) {
		comment, _, apiErr := c.client.Issues.CreateComment(ctx, owner, repoName, number, &gh.IssueComment{Body: gh.Ptr(body)})
		if apiErr != nil && isDefiniteIssueRefusal(apiErr) {
			// A 403/404/410/422 means the comment was not posted; recording
			// Unknown would fence the operation behind reconciliation.
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: comment.GetHTMLURL()}, apiErr
	})
	return err
}

// observeIssueComment is the authoritative observation of an issue comment
// the mutation journal holds as unresolved: the earlier attempt applied when
// the issue already carries a comment with the same body. Bodies are compared
// like review bodies (line endings and surrounding whitespace normalised).
func (c *Client) observeIssueComment(ctx context.Context, owner, repoName string, number int, body string) (effects.ExternalState, error) {
	if c == nil || c.client == nil {
		return effects.ExternalState{}, ErrNoGitHubClient
	}
	want := normalizeReviewBody(body)
	if want == "" {
		return effects.ExternalState{}, errors.New("comment has no body to match against")
	}
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		comments, resp, err := c.client.Issues.ListComments(WithRESTCaller(ctx, "hive:reconcile_issue_comment"), owner, repoName, number, opts)
		if err != nil {
			return effects.ExternalState{}, err
		}
		for _, comment := range comments {
			if normalizeReviewBody(comment.GetBody()) == want {
				return effects.ExternalState{Applied: true, Provenance: comment.GetHTMLURL()}, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return effects.ExternalState{}, nil
		}
		opts.Page = resp.NextPage
	}
}

// GetPRAuthor returns the current author login for an open PR.
func (c *Client) GetPRAuthor(ctx context.Context, repo string, number int) (string, error) {
	if c == nil {
		return "", ErrNoGitHubClient
	}
	// Dashboard decoration tolerates the bounded PR-detail TTL; a stale author
	// display is harmless and avoids another charged GET when enrichment just ran.
	if pr, ok := c.cachedPRDetailAny(repo, number); ok {
		return safeGetLogin(pr.GetUser()), nil
	}
	owner, repoName := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:get_pr_author"), owner, repoName, number)
	if err != nil {
		return "", err
	}
	c.storePRDetail(repo, number, pr)
	return safeGetLogin(pr.GetUser()), nil
}

// PRState is the answer to "what became of this PR": GitHub's open/closed
// state plus the merge and close timestamps that tell the two apart.
type PRState struct {
	State    string
	MergedAt time.Time
	ClosedAt time.Time
	// BaseRef is the branch the PR targets ("" on an abbreviated payload).
	BaseRef string
	// MergedBy is the login that merged the PR ("" when not merged or unknown).
	MergedBy string
	// Title, Body and HeadSHA are carried so a merged PR's text can be
	// scanned for a revert without another fetch (hivecommons/hive#13241).
	Title   string
	Body    string
	HeadSHA string
}

// GetPRState fetches a single PR's state. The review-outcome ledger calls it
// for PRs that left the governor's open list, one GET each, so the ledger can
// tell a merge from a close from a transient enumeration miss.
func (c *Client) GetPRState(ctx context.Context, repo string, number int) (PRState, error) {
	if c == nil {
		return PRState{}, ErrNoGitHubClient
	}
	// Outcome/follow-up ledgers reconcile display history and retry later; they
	// can accept one PR-detail TTL of staleness rather than recharging a GET.
	pr, ok := c.cachedPRDetailAny(repo, number)
	if !ok {
		owner, repoName := c.splitRepo(repo)
		var err error
		pr, _, err = c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:get_pr_state"), owner, repoName, number)
		if err != nil {
			return PRState{}, err
		}
		c.storePRDetail(repo, number, pr)
	}
	st := PRState{State: pr.GetState(), BaseRef: prBaseRef(pr), MergedBy: pr.GetMergedBy().GetLogin(),
		Title: pr.GetTitle(), Body: pr.GetBody(), HeadSHA: pr.GetHead().GetSHA()}
	if !pr.GetMergedAt().IsZero() {
		st.MergedAt = pr.GetMergedAt().Time
	}
	if !pr.GetClosedAt().IsZero() {
		st.ClosedAt = pr.GetClosedAt().Time
	}
	return st, nil
}

// QueuePRAutoMerge approves a PR as the hive App and marks it for Hive's
// auto-merge-on-green sweep. The approval body records who queued the PR so
// the sweep can re-check the self-merge ban before it squashes anything.
func (c *Client) QueuePRAutoMerge(ctx context.Context, repo string, number int, queuedBy string) error {
	return c.QueuePRAutoMergeWithNote(ctx, repo, number, queuedBy, "")
}

// QueuePRAutoMergeWithNote is QueuePRAutoMerge with an extra note appended
// to the approval body after the queue line the sweep parses (the line is
// anchored at the start, so the note never changes who the sweep sees as the
// queuer). The dashboard's delegated decision uses it to carry the
// owner-delegate audit marker on the review itself.
func (c *Client) QueuePRAutoMergeWithNote(ctx context.Context, repo string, number int, queuedBy, note string) error {
	_, err := c.QueuePRAutoMergeWithNoteResult(ctx, repo, number, queuedBy, note)
	return err
}

// PRAutoMergeQueueResult reports how QueuePRAutoMergeWithNoteResult
// recorded the queue: HeadSHA is the head it applies to; ReviewSubmitted is
// false when the hive could not review the PR because it is the hive's own
// (GitHub refuses "Can not approve your own pull request"), in which case
// the queue line was left as a comment instead and the caller records the
// operator's approval in the hive's own store so the self-authored lane
// merges on it (hivecommons/hive#15666).
type PRAutoMergeQueueResult struct {
	HeadSHA         string
	ReviewSubmitted bool
	SelfAuthored    bool
}

// ownPullRequestReviewRefusal is the phrase in GitHub's 422 for an APPROVE
// (or REQUEST_CHANGES) review on the caller's own pull request.
const ownPullRequestReviewRefusal = "own pull request"

// IsOwnPullRequestReviewRefusal reports whether err is GitHub's 422 "Review
// Can not approve your own pull request" (or the request-changes twin): the
// reviewer is the PR author, which no retry changes. Callers treat it as a
// policy answer, not a fault, and fall back to a comment.
func IsOwnPullRequestReviewRefusal(err error) bool {
	if err == nil || !githubStatusError(err, http.StatusUnprocessableEntity) {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), ownPullRequestReviewRefusal)
}

// pullRequestSelfAuthored reports whether pr is the hive's own: opened by
// one of its posting identities or stamped author/hive by the classifier.
func (c *Client) pullRequestSelfAuthored(pr *gh.PullRequest) bool {
	if c == nil || pr == nil {
		return false
	}
	if c.isHiveAccount(pr.GetUser()) || c.IsHiveAuthorLogin(safeGetLogin(pr.GetUser())) {
		return true
	}
	names := authorkind.Names{}
	if c.authorKindLabels != nil {
		names = c.authorKindLabels()
	}
	return authorkind.FromLabels(labelNames(pr.Labels), names) == authorkind.Hive
}

// QueuePRAutoMergeWithNoteResult is QueuePRAutoMergeWithNote reporting how
// the queue was recorded. On the hive's own PR the App cannot approve
// (GitHub 422 "Can not approve your own pull request"), so no review is
// attempted: the queue line is posted as an issue comment and the label
// applied. The same fallback runs when GitHub answers that 422 for an
// author the hive did not recognise as itself.
func (c *Client) QueuePRAutoMergeWithNoteResult(ctx context.Context, repo string, number int, queuedBy, note string) (PRAutoMergeQueueResult, error) {
	if c == nil {
		return PRAutoMergeQueueResult{}, ErrNoGitHubClient
	}
	queuedBy = strings.TrimSpace(queuedBy)
	if queuedBy == "" {
		return PRAutoMergeQueueResult{}, errors.New("queuedBy is required for auto-merge audit and self-merge checks")
	}
	owner, repoName := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:queue_pr_automerge"), owner, repoName, number)
	if err != nil {
		return PRAutoMergeQueueResult{}, fmt.Errorf("fetching PR head for auto-merge approval: %w", err)
	}
	headSHA := ""
	if pr.GetHead() != nil {
		headSHA = pr.GetHead().GetSHA()
	}
	if headSHA == "" {
		return PRAutoMergeQueueResult{}, errors.New("PR head SHA is required for auto-merge approval")
	}
	full := owner + "/" + repoName
	res := PRAutoMergeQueueResult{HeadSHA: headSHA, SelfAuthored: c.pullRequestSelfAuthored(pr)}
	if blocked := c.sentinelBlockedLabel(labelNames(pr.Labels)); blocked != "" {
		c.recordWriteAudit(AuditActionSentinelBlockedApproval, InvocationMeta{Agent: AttributionAgentGovernor},
			WriteTarget{Repo: full, Number: number},
			"agent", queuedBy, "label", blocked)
		return res, fmt.Errorf("approving PR: %s label blocks Hive approval and auto-merge", blocked)
	}
	label := c.AutoMergeLabel()
	if err := c.ensureLabel(ctx, owner, repoName, label); err != nil {
		return res, fmt.Errorf("ensuring %s label: %w", label, err)
	}
	body := fmt.Sprintf("Approved by @%s for Hive auto-merge on green CI.", queuedBy)
	if note = strings.TrimSpace(note); note != "" {
		body += "\n\n" + note
	}
	refusedAsOwn := false
	if !res.SelfAuthored {
		provenance := full + "#" + strconv.Itoa(number)
		_, err = effects.ExecuteReconciling(ctx, c.mutationBoundary(), effects.Claim{
			Repo:   full,
			Kind:   effects.KindReviewSubmit,
			Target: strconv.Itoa(number),
			Inputs: map[string]string{"event": "APPROVE", "head_sha": headSHA, "body": effects.StableDigest(body)},
		}, func(ctx context.Context) (effects.ExternalState, error) {
			return c.observeReviewSubmit(ctx, owner, repoName, number, body, headSHA, provenance)
		}, effects.DefaultUnresolvedTTL, func(ctx context.Context) (effects.Result, error) {
			_, _, apiErr := c.client.PullRequests.CreateReview(ctx, owner, repoName, number, &gh.PullRequestReviewRequest{
				CommitID: gh.Ptr(headSHA),
				Body:     gh.Ptr(body),
				Event:    gh.Ptr("APPROVE"),
			})
			if IsOwnPullRequestReviewRefusal(apiErr) {
				// Policy, not a fault: nothing to reconcile later.
				refusedAsOwn = true
				return effects.Result{Provenance: provenance}, nil
			}
			return effects.Result{Provenance: provenance}, apiErr
		})
		if err != nil {
			return res, fmt.Errorf("approving PR: %w", err)
		}
		res.ReviewSubmitted = !refusedAsOwn
		res.SelfAuthored = res.SelfAuthored || refusedAsOwn
	}
	if res.ReviewSubmitted {
		// Audit the review so it counts as activity on the trail. This is the hive's
		// own auto-merge self-approval (a governor action); agent-authored reviews
		// come through the review-request watcher, which audits separately.
		c.recordWriteAudit(AuditActionPRReviewed, InvocationMeta{Agent: AttributionAgentGovernor},
			WriteTarget{Repo: full, Number: number},
			"agent", queuedBy, "state", "approved")
	} else {
		// The App cannot review its own pull request: leave the queue line
		// as a comment so the decision stays visible and attributed; the
		// caller records the operator's approval in the hive's store.
		if err := c.CreateIssueComment(ctx, full, number, body); err != nil {
			return res, fmt.Errorf("posting auto-merge queue note: %w", err)
		}
		c.info("auto-merge queue: the PR is the hive's own, approval left as a comment instead of a review", "repo", full, "pr", number, "queued_by", queuedBy, "head", headSHA)
	}
	if err := c.AddLabels(ctx, full, number, []string{label}); err != nil {
		return res, fmt.Errorf("adding %s label: %w", label, err)
	}
	if strings.EqualFold(label, prowLgtmLabel) {
		c.bindProwLgtm(ctx, owner, repoName, number, headSHA, queuedBy)
	}
	return res, nil
}

// observeReviewSubmit is the authoritative observation of a review submission
// the mutation journal holds as unresolved: the earlier attempt applied when
// the PR already carries a review with the same body (and, when commitID is
// set, on the same commit). Bodies are compared after normalising line endings
// and surrounding whitespace, which GitHub does not preserve byte-for-byte.
func (c *Client) observeReviewSubmit(ctx context.Context, owner, repoName string, number int, body, commitID, provenance string) (effects.ExternalState, error) {
	if c == nil || c.client == nil {
		return effects.ExternalState{}, ErrNoGitHubClient
	}
	want := normalizeReviewBody(body)
	if want == "" {
		// Nothing distinguishes an empty-bodied review from anyone else's;
		// leave the operation to the TTL expiry rather than guess.
		return effects.ExternalState{}, errors.New("review has no body to match against")
	}
	opts := &gh.ListOptions{PerPage: 100}
	for {
		reviews, resp, err := c.client.PullRequests.ListReviews(WithRESTCaller(ctx, "hive:reconcile_review_submit"), owner, repoName, number, opts)
		if err != nil {
			return effects.ExternalState{}, err
		}
		for _, r := range reviews {
			if commitID != "" && !strings.EqualFold(r.GetCommitID(), commitID) {
				continue
			}
			if normalizeReviewBody(r.GetBody()) == want {
				return effects.ExternalState{Applied: true, Provenance: provenance}, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return effects.ExternalState{}, nil
		}
		opts.Page = resp.NextPage
	}
}

func normalizeReviewBody(body string) string {
	return strings.TrimSpace(strings.ReplaceAll(body, "\r\n", "\n"))
}

// prowLgtmLabel is Prow's approval label; prowLgtmStatusContext is the
// commit status Prow's commit-bound `lgtm` (cncf/prow-github-actions v3,
// bind_to_commit) requires beside the label; without it Prow strips the
// label and the PR drops out of the queue (hivecommons/hive#13616).
const (
	prowLgtmLabel         = "lgtm"
	prowLgtmStatusContext = "prow/lgtm"
	prowStatusDescMaxLen  = 140
)

// bindProwLgtm records the `prow/lgtm` success status on headSHA, the commit
// the queue review approved. The label and review are already in place, so a
// failure is logged and surfaced on the PR but never returned.
func (c *Client) bindProwLgtm(ctx context.Context, owner, repoName string, number int, headSHA, queuedBy string) {
	sha7 := headSHA
	if len(sha7) > 7 {
		sha7 = sha7[:7]
	}
	desc := fmt.Sprintf("lgtm by %s at %s", queuedBy, sha7)
	if len(desc) > prowStatusDescMaxLen {
		desc = desc[:prowStatusDescMaxLen]
	}
	_, err := effects.Execute(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + repoName,
		Kind:   effects.KindStatusCreate,
		Target: strconv.Itoa(number),
		Inputs: map[string]string{"context": prowLgtmStatusContext, "state": "success", "head_sha": headSHA, "description": effects.StableDigest(desc)},
	}, func(ctx context.Context) (effects.Result, error) {
		_, _, apiErr := c.client.Repositories.CreateStatus(WithRESTCaller(ctx, "hive:queue_pr_automerge_status"), owner, repoName, headSHA, &gh.RepoStatus{
			Context:     gh.Ptr(prowLgtmStatusContext),
			State:       gh.Ptr("success"),
			Description: gh.Ptr(desc),
		})
		return effects.Result{Provenance: owner + "/" + repoName + "@" + headSHA}, apiErr
	})
	if err == nil {
		return
	}
	if c.logger != nil {
		c.logger.Warn("could not set the prow/lgtm commit status; the GitHub App likely lacks the statuses: write permission",
			"repo", owner+"/"+repoName, "number", number, "error", err)
	}
	note := "Hive queued this PR but could not set the `prow/lgtm` commit status, so Prow may strip the `lgtm` label and the auto-merge queue may not hold."
	if cerr := c.CreateIssueComment(ctx, owner+"/"+repoName, number, note); cerr != nil && c.logger != nil {
		c.logger.Warn("could not post the prow/lgtm status warning comment",
			"repo", owner+"/"+repoName, "number", number, "error", cerr)
	}
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

// EnsureIssueLabel creates an issue label if it does not already exist.
// origin/* labels are additionally recoloured to the shared family colour.
func (c *Client) EnsureIssueLabel(ctx context.Context, repo, name, color, description string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	owner, repoName := c.splitRepo(repo)
	if IsOriginLabel(name) {
		return c.ensureOriginLabel(ctx, owner, repoName, name)
	}
	if _, _, err := c.client.Issues.GetLabel(ctx, owner, repoName, name); err == nil {
		return nil
	} else if ghErr, ok := err.(*gh.ErrorResponse); !ok || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusNotFound {
		return err
	}
	color = strings.TrimSpace(color)
	if color == "" {
		color = "8250df"
	}
	if def, ok := escalationIssueLabelDefinition(name); ok {
		if color == "8250df" {
			color = def.color
		}
		if strings.TrimSpace(description) == "" {
			description = def.description
		}
	}
	_, _, err := c.client.Issues.CreateLabel(ctx, owner, repoName, &gh.Label{
		Name:        gh.Ptr(name),
		Color:       gh.Ptr(color),
		Description: gh.Ptr(clampLabelDescription(description)),
	})
	if ghErr, ok := err.(*gh.ErrorResponse); ok && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusUnprocessableEntity {
		return nil
	}
	return err
}

// AddLabels adds labels to an issue or PR (no-op for an empty list), mirroring
// forge.Forge.AddLabels for the same swap-in reason as CreateIssueComment.
func (c *Client) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	return c.addLabelsObserved(ctx, repo, number, labels, "")
}

// addLabelsObserved is AddLabels keyed to the item state the caller just
// read. The mutation journal treats an effect with the same inputs as
// applied forever, which is right for a merge but not for a label another
// writer can put back: a stage label removed once could never be removed
// again (hivecommons/hive#12512). Carrying the observed updated_at in the
// claim makes "remove X as of state T" a new effect each time the item
// changed, while a crash-retry within the same state still dedupes.
func (c *Client) addLabelsObserved(ctx context.Context, repo string, number int, labels []string, observed string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	if len(labels) == 0 {
		return nil
	}
	owner, repoName := c.splitRepo(repo)
	claim := effects.Claim{
		Repo:   owner + "/" + repoName,
		Kind:   effects.KindLabelMutation,
		Target: strconv.Itoa(number),
		Inputs: labelMutationInputs(map[string]string{"labels": strings.Join(labels, ",")}, observed),
	}
	effect := func(ctx context.Context) (effects.Result, error) {
		_, _, apiErr := c.client.Issues.AddLabelsToIssue(ctx, owner, repoName, number, labels)
		return effects.Result{Provenance: owner + "/" + repoName + "#" + strconv.Itoa(number)}, apiErr
	}
	_, err := effects.Execute(ctx, c.mutationBoundary(), claim, effect)
	if errors.Is(err, effects.ErrNeedsReconciliation) {
		_, err = c.reconcileLabelMutation(ctx, owner, repoName, number, claim, effect, err, func(current []string) bool {
			return labelsAllPresent(current, labels)
		})
	}
	if err == nil {
		c.forgeWriteThrough(repo, number, forgestate.Mutation{Op: forgestate.OpLabelAdd, Labels: labels})
	}
	return err
}

// RemoveLabel removes one label from an issue or PR. A label that is not
// present (404) is not an error — the desired end state already holds.
func (c *Client) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	return c.removeLabelObserved(ctx, repo, number, label, "")
}

// removeLabelObserved is RemoveLabel keyed to observed item state; see
// addLabelsObserved.
func (c *Client) removeLabelObserved(ctx context.Context, repo string, number int, label string, observed string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	if label == "" {
		return nil
	}
	owner, repoName := c.splitRepo(repo)
	claim := effects.Claim{
		Repo:   owner + "/" + repoName,
		Kind:   effects.KindLabelMutation,
		Target: strconv.Itoa(number),
		Inputs: labelMutationInputs(map[string]string{"remove_label": label}, observed),
	}
	effect := func(ctx context.Context) (effects.Result, error) {
		_, apiErr := c.client.Issues.RemoveLabelForIssue(ctx, owner, repoName, number, url.PathEscape(label))
		if githubStatusError(apiErr, http.StatusNotFound) {
			apiErr = nil
		}
		return effects.Result{Provenance: owner + "/" + repoName + "#" + strconv.Itoa(number)}, apiErr
	}
	_, err := effects.Execute(ctx, c.mutationBoundary(), claim, effect)
	if errors.Is(err, effects.ErrNeedsReconciliation) {
		_, err = c.reconcileLabelMutation(ctx, owner, repoName, number, claim, effect, err, func(current []string) bool {
			return !labelsAllPresent(current, []string{label})
		})
	}
	if err == nil {
		c.forgeWriteThrough(repo, number, forgestate.Mutation{Op: forgestate.OpLabelRemove, Labels: []string{label}})
	}
	return err
}

// reconcileLabelMutation resolves a label mutation the mutation journal holds
// as unresolved — an earlier attempt at the SAME logical operation is still
// Planned (a concurrent writer mid-call, or a process that died between
// intent and acknowledgment) or ended Unknown (ambiguous API failure) — from
// authoritative external state, then replays the mutation once. A label is
// the easy case: it is either on the item or it is not, and one Issues.Get
// says which. Before this the claimed-by label write simply failed with
// "op:… is Planned" and the claim went un-labelled until the next renewal
// (hivecommons/hive#13579). Nothing is expired blind: when the read itself
// fails the needs-reconciliation refusal stands, and the boundary's OnStuck
// reports the operation only once it has been unresolved past StuckAfter.
func (c *Client) reconcileLabelMutation(ctx context.Context, owner, repoName string, number int, claim effects.Claim,
	effect func(context.Context) (effects.Result, error), journalErr error, applied func(current []string) bool) (effects.Result, error) {
	if c == nil || c.client == nil {
		return effects.Result{}, journalErr
	}
	issue, _, lookupErr := c.client.Issues.Get(WithRESTCaller(ctx, "hive:reconcile_label_mutation"), owner, repoName, number)
	if lookupErr != nil {
		return effects.Result{}, fmt.Errorf("%w (label read for reconciliation failed: %v)", journalErr, lookupErr)
	}
	state := effects.ExternalState{Applied: applied(extractLabels(issue.Labels)), Provenance: owner + "/" + repoName + "#" + strconv.Itoa(number)}
	if recErr := effects.Reconcile(ctx, c.mutationBoundary(), claim, state); recErr != nil {
		return effects.Result{}, fmt.Errorf("%w (reconciliation failed: %v)", journalErr, recErr)
	}
	if c.logger != nil {
		c.logger.Info("label mutation: reconciled unresolved operation against the item's current labels",
			slog.String("repo", owner+"/"+repoName), slog.Int("number", number), slog.Bool("applied", state.Applied))
	}
	if state.Applied {
		return effects.Result{Provenance: state.Provenance}, nil
	}
	return effects.Execute(ctx, c.mutationBoundary(), claim, effect)
}

// labelsAllPresent reports whether every wanted label is in current
// (case-insensitively, as GitHub treats label names).
func labelsAllPresent(current, wanted []string) bool {
	for _, w := range wanted {
		found := false
		for _, l := range current {
			if strings.EqualFold(l, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// labelMutationInputs adds the observed item state to a label mutation's
// journal inputs when the caller has one.
func labelMutationInputs(inputs map[string]string, observed string) map[string]string {
	if observed != "" {
		inputs["observed_at"] = observed
	}
	return inputs
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

func extractAssignees(users []*gh.User) []string {
	var result []string
	for _, u := range users {
		if login := safeGetLogin(u); login != "" {
			result = append(result, login)
		}
	}
	return result
}

// Reachable actions for a PR in a kick work list (hivecommons/hive#7386):
// what an agent holding the hive's App token can actually do to it.
const (
	// ReachableActionPush: the head branch is in the base repository, or it
	// is a fork branch that allows maintainer edits, so the agent can check
	// it out, commit, and push a repair when policy says the hive owns that
	// repair.
	ReachableActionPush = "push"
	// ReachableActionCommentOnly: the head branch lives in a fork (or a
	// deleted repository) without maintainer edits. The agent can review and
	// comment; it cannot push, and must never push to a branch of the base
	// repo named after the fork's head ref.
	ReachableActionCommentOnly = "comment-only"
)

// ReachableAction classifies a PR for the repair queue: push-repairable, or
// comment-only because its head is not in the base repository and the fork
// did not grant maintainer edits.
func ReachableAction(pr PullRequest) string {
	if pr.FromFork && !pr.MaintainerCanModify {
		return ReachableActionCommentOnly
	}
	return ReachableActionPush
}

// prHeadOrigin reports where a PR's head branch lives: its ref name, the
// "owner/name" repository holding it, and whether that is a different
// repository from the PR's base (a fork PR, GitHub's isCrossRepository). A
// head whose repository is gone — the fork was deleted after the PR was
// opened — reports fromFork=true with an empty HeadRepo: there is nothing to
// push to either way (hivecommons/hive#7386). Comparison is case-insensitive
// because GitHub repository names are.
func prHeadOrigin(pr *gh.PullRequest) (headRef, headRepo string, fromFork bool) {
	if pr == nil || pr.GetHead() == nil {
		return "", "", false
	}
	head := pr.GetHead()
	headRef = head.GetRef()
	if head.GetRepo() == nil {
		return headRef, "", true
	}
	headRepo = head.GetRepo().GetFullName()
	baseRepo := ""
	if pr.GetBase() != nil && pr.GetBase().GetRepo() != nil {
		baseRepo = pr.GetBase().GetRepo().GetFullName()
	}
	if baseRepo == "" || headRepo == "" {
		// Cannot compare: an abbreviated payload. Do not guess "fork" — that
		// would hide every PR from the repair queue — but do not claim
		// same-repo either when the head is unknown.
		return headRef, headRepo, headRepo == ""
	}
	return headRef, headRepo, !strings.EqualFold(headRepo, baseRepo)
}

func safeGetLogin(u *gh.User) string {
	if u == nil {
		return ""
	}

	return u.GetLogin()
}

func HasHoldLabel(labels []string) bool {
	return HasHoldLabelWith(labels, nil)
}

func HasHoldLabelWith(labels, extraHoldLabels []string) bool {
	holdLabels := append([]string{}, HoldLabels...)
	exactHoldLabels := make([]string, 0, len(extraHoldLabels))
	for _, label := range extraHoldLabels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label != "" {
			if strings.HasPrefix(label, "hive-pause/") {
				exactHoldLabels = append(exactHoldLabels, label)
			} else {
				holdLabels = append(holdLabels, label)
			}
		}
	}
	for _, label := range labels {
		lower := strings.ToLower(label)
		// Provenance labels (hive/<id>) are never hold labels, whatever the
		// hive ID happens to contain. The substring rule below otherwise
		// reads hive/hosted-...-placeHOLDer-r05x as "hold" and parks every
		// item that hive has ever claimed: excluded from the actionable set,
		// skipped by the automerge sweep, counted as on-hold (69 items on
		// one spoke, 2026-09-28). CanonicalHiveHoldLabel deliberately uses
		// the hive-pause/ prefix so hive/<id> can stay provenance-only.
		if !isProvenanceLabel(lower) {
			for _, sub := range holdLabels {
				sub = strings.ToLower(strings.TrimSpace(sub))
				if sub != "" && strings.Contains(lower, sub) {
					return true
				}
			}
		}
		for _, exact := range exactHoldLabels {
			if strings.HasPrefix(exact, "hive-pause/") {
				if strings.EqualFold(strings.TrimSpace(label), exact) {
					return true
				}
				continue
			}
			if strings.Contains(lower, exact) {
				return true
			}
		}
	}
	return false
}

// isProvenanceLabel reports whether a (lower-cased) label is a hive/<id>
// provenance marker as produced by HiveProvenanceLabel.
func isProvenanceLabel(lower string) bool {
	return strings.HasPrefix(lower, "hive/")
}

func isHeld(labels []string) bool { return HasHoldLabel(labels) }

func CanonicalHiveHoldLabel(hiveID string) string {
	hiveID = strings.TrimSpace(hiveID)
	if hiveID == "" {
		return "hold"
	}
	// Avoid "hold" in this hive-scoped spelling: generic hold labels are
	// substring-matched for backwards compatibility, while this label must be
	// matched exactly so hive/<id> can stay provenance-only.
	return "hive-pause/" + hiveID
}

// HiveProvenanceLabel is the retired hive/<hive-id> label
// (hivecommons/hive#12539). Nothing writes it any more; it is kept so the
// hold migration can recognise it and RetireHiveIDLabels can strip it.
// OriginHive reads the body marker that replaced it.
func HiveProvenanceLabel(hiveID string) string {
	hiveID = strings.TrimSpace(hiveID)
	if hiveID == "" {
		return ""
	}
	return "hive/" + hiveID
}

func (c *Client) SetHoldLabels(labels []string) {
	if c == nil {
		return
	}
	c.holdLabelsMu.Lock()
	defer c.holdLabelsMu.Unlock()
	c.holdLabels = append([]string{}, labels...)
}

func (c *Client) SetSentinelAlertLabel(label string) {
	if c == nil {
		return
	}
	c.sentinelMu.Lock()
	defer c.sentinelMu.Unlock()
	c.sentinelLabel = strings.TrimSpace(label)
}

func (c *Client) sentinelAlertLabel() string {
	if c == nil {
		return ""
	}
	c.sentinelMu.RLock()
	defer c.sentinelMu.RUnlock()
	return strings.TrimSpace(c.sentinelLabel)
}

func (c *Client) sentinelBlockedLabel(labels []string) string {
	sentinelLabel := strings.ToLower(c.sentinelAlertLabel())
	if sentinelLabel == "" {
		return ""
	}
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), sentinelLabel) {
			return sentinelLabel
		}
	}
	return ""
}

func (c *Client) isHeld(labels []string) bool {
	if c == nil {
		return HasHoldLabel(labels)
	}
	c.holdLabelsMu.RLock()
	extra := append([]string{}, c.holdLabels...)
	c.holdLabelsMu.RUnlock()
	return HasHoldLabelWith(labels, extra)
}

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

func (c *Client) SetAgentIssueDecisionPolicy(policy string) {
	if c == nil {
		return
	}
	c.agentIssueDecisionPolicyMu.Lock()
	defer c.agentIssueDecisionPolicyMu.Unlock()
	c.agentIssueDecisionPolicy = policy
}

func (c *Client) getAgentIssueDecisionPolicy() string {
	if c == nil {
		return ""
	}
	c.agentIssueDecisionPolicyMu.RLock()
	defer c.agentIssueDecisionPolicyMu.RUnlock()
	return c.agentIssueDecisionPolicy
}

func (c *Client) SetReporterConfirmationEnabledFunc(fn func() bool) {
	if c == nil {
		return
	}
	c.reporterConfirmationMu.Lock()
	defer c.reporterConfirmationMu.Unlock()
	c.reporterConfirmationEnabled = fn
}

func (c *Client) reporterConfirmationGateDefaultEnabled() bool {
	if c == nil {
		return false
	}
	c.reporterConfirmationMu.RLock()
	fn := c.reporterConfirmationEnabled
	c.reporterConfirmationMu.RUnlock()
	return fn != nil && fn()
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

// needsDecisionLabel is the label the issue relay applies to park an
// agent-filed issue on a maintainer decision: the first configured
// hard_suppress_labels.needs_decision label, else stage/045-needs-decision
// (hivecommons/hive#12683). A configured legacy spelling — including the
// pre-045 stage/085, which is PR-only now — is upgraded to the issue stage
// label so no hive keeps writing a retired label on an issue.
func (c *Client) needsDecisionLabel() string {
	if labeler, ok := c.getIssueFilter().(NeedsDecisionLabeler); ok {
		if label := strings.TrimSpace(labeler.NeedsDecisionLabel()); label != "" && !isLegacyDecisionLabel(label) && !strings.EqualFold(label, legacyIssueNeedsDecisionStageLabel) {
			return label
		}
	}
	return NeedsDecisionStageLabel
}

// appendLabelIfMissing returns labels with label added unless it is already
// present (GitHub compares label names case-insensitively).
func appendLabelIfMissing(labels []string, label string) []string {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), label) {
			return labels
		}
	}
	return append(append([]string(nil), labels...), label)
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
	// The title and label markers are matched case- and namespace-insensitively
	// (hivecommons/hive#7995). They used to be one exact-case prefix and one
	// exact label, which meant the two conventions every other tracker check in
	// this package already knew about — the `epic`/`tracker` labels and the
	// `[epic]` title prefix, see incompleteIssueReason — were invisible HERE,
	// where Issue.IsTracker is computed and the contribute queue reads it. A
	// tracking issue is the incremental shape by construction: its children
	// carry the work, and it re-offers after every one of them merges until a
	// human closes it. utah#24 was handed to a contributor that way.
	if trackerTitlePrefix(title) {
		return true
	}
	for _, l := range labels {
		if trackerLabel(l) {
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

// trackerLabelNames are the labels that mark an issue as coordination-only.
// Kept deliberately short: each one names the tracking role outright, so none
// of them can appear on ordinary work by accident.
var trackerLabelNames = map[string]bool{
	"tracker":      true,
	"meta-tracker": true,
	"tracking":     true,
	"epic":         true,
}

// trackerLabel reports whether one label marks a tracker, tolerating the
// prefixed spellings repositories use (`kind/epic`, `type: tracking`) by
// reading only the final segment.
func trackerLabel(label string) bool {
	l := strings.ToLower(strings.TrimSpace(label))
	if i := strings.LastIndexAny(l, "/:"); i >= 0 {
		l = strings.TrimSpace(l[i+1:])
	}
	return trackerLabelNames[l]
}

// trackerTitlePrefix reports whether a title opens with a bracketed tracker
// marker. Emoji-prefixed titles are covered because the marker is looked for
// after any leading non-'[' run, which is what the repository's own
// "🌱 chore: ..." convention produces once a tracker marker is added.
func trackerTitlePrefix(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	if i := strings.Index(t, "["); i >= 0 && i <= 8 {
		t = t[i:]
	}
	return strings.HasPrefix(t, "[tracker]") || strings.HasPrefix(t, "[epic]")
}

type RateLimitInfo struct {
	APIBudget      APIBudgetSnapshot   `json:"api_budget"`
	Core           RateLimitEntry      `json:"core"`
	Search         RateLimitEntry      `json:"search"`
	GraphQL        RateLimitEntry      `json:"graphql"`
	TopConsumers   []RESTConsumer      `json:"top_consumers,omitempty"`
	ETagCache      ETagCacheInfo       `json:"etag_cache"`
	PRDetailCache  ETagCacheInfo       `json:"pr_detail_cache"`
	GraphQLPRBatch GraphQLPRBatchStats `json:"graphql_pr_batch"`
	Webhooks       WebhookHealth       `json:"webhooks"`
}

type GraphQLPRBatchStats struct {
	Repos     int `json:"repos"`
	PRs       int `json:"prs"`
	Pages     int `json:"pages"`
	Fallbacks int `json:"fallbacks"`
	Errors    int `json:"errors"`
	LastCost  int `json:"last_cost"`
	// WebhookSkips counts repo scans that reused the previous batch because
	// webhooks were healthy and no PR was marked dirty (#11177).
	WebhookSkips int `json:"webhook_skips"`
}

type ETagCacheInfo struct {
	Hits    int64 `json:"hits"`
	Misses  int64 `json:"misses"`
	Entries int64 `json:"entries"`
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
	if c != nil {
		c.prBatchMu.Lock()
		if c.prBatchGraphQLRate != (RateLimitEntry{}) {
			info.GraphQL = c.rateLimits.observe("graphql", c.prBatchGraphQLRate)
		}
		info.GraphQLPRBatch = c.prBatchStats
		c.prBatchMu.Unlock()
	}
	hits, misses, entries := ETagCacheStats()
	info.ETagCache = ETagCacheInfo{Hits: hits, Misses: misses, Entries: entries}
	hits, misses, entries = PRDetailCacheStats()
	info.PRDetailCache = ETagCacheInfo{Hits: hits, Misses: misses, Entries: entries}
	info.Webhooks = WebhookHealthSnapshot()
	info.TopConsumers = RESTTopConsumers(10)
	_, snapshot := c.APIBudgetMode()
	info.APIBudget = snapshot

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

// CommitOnBranch reports whether sha is reachable from the tip of branch, i.e.
// GitHub's compare of branch...sha is "identical" or "behind". The hold guard
// uses it to confirm the second parent of an update-branch merge is a base
// commit even when the base moved after enumeration (#14534).
func (c *Client) CommitOnBranch(ctx context.Context, repo, branch, sha string) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	cmp, _, err := c.client.Repositories.CompareCommits(WithRESTCaller(ctx, "hive:hold_guard_base_parent"), owner, repoName, branch, sha, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return false, fmt.Errorf("comparing %s %s...%s: %w", repo, branch, sha, err)
	}
	st := cmp.GetStatus()
	return st == "identical" || st == "behind", nil
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

// FullCommitMessage returns the complete commit message for the given SHA,
// including body and trailers. Artifact linkage uses this rather than
// CommitMessage's one-line status summary.
func (c *Client) FullCommitMessage(ctx context.Context, owner, repo, sha string) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	commit, _, err := c.client.Repositories.GetCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		return "", fmt.Errorf("fetching commit %s/%s@%s: %w", owner, repo, sha, err)
	}
	if commit == nil || commit.GetCommit() == nil {
		return "", fmt.Errorf("github returned no commit message for %s/%s@%s", owner, repo, sha)
	}
	return commit.GetCommit().GetMessage(), nil
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

// IssueClosedAt reports whether a GitHub issue or pull request is closed, and
// when. A pull request IS an issue to this endpoint, so one call covers both and
// a merged PR reports closed -- which is what the advisory digest needs to know.
//
// It exists for #6080: the digest was re-emitting findings computed at an older
// commit as counted, severity-ranked open HIGH items, including one listed open
// at the exact commit that fixed it. When such a finding names its own
// remediation ("Filed issue #208, hold-gated PR #209"), this is the lookup that
// settles whether that remediation landed.
//
// A 404 or 410 (deleted) reports (zero, false, nil): not found is not closed. The caller retires
// a finding only when EVERY reference it names is closed, so an unreadable
// reference keeps the finding open, which is the safe direction. Any other error
// is returned so the caller can distinguish "open" from "could not tell".
func (c *Client) IssueClosedAt(ctx context.Context, owner, repo string, number int) (time.Time, bool, error) {
	if c == nil {
		return time.Time{}, false, ErrNoGitHubClient
	}
	issue, resp, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("reading %s/%s#%d: %w", owner, repo, number, err)
	}
	if issue == nil || issue.GetState() != "closed" {
		return time.Time{}, false, nil
	}
	return issue.GetClosedAt().Time, true, nil
}

// IssueSummary is the display triple of one issue or pull request:
// what the activity tracker's issue slot needs for a closing reference
// whose issue is no longer open (closed by an earlier PR, so absent from
// every open-items listing, with only the number surviving from the PR's
// closingIssuesReferences). State is lower-cased as on Issue.State.
type IssueSummary struct {
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	State  string `json:"state,omitempty"`
	URL    string `json:"url,omitempty"`
}

// IssueSummary reads one issue (or PR: the endpoint serves both) by number
// and returns its title, state and URL. A 404 reports (zero, false, nil):
// the reference names nothing readable, which the caller remembers for its
// TTL rather than asking again. Any other error is returned.
func (c *Client) IssueSummary(ctx context.Context, repo string, number int) (IssueSummary, bool, error) {
	if c == nil {
		return IssueSummary{}, false, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	issue, resp, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return IssueSummary{}, false, nil
		}
		return IssueSummary{}, false, fmt.Errorf("reading %s/%s#%d: %w", owner, name, number, err)
	}
	if issue == nil {
		return IssueSummary{}, false, nil
	}
	return IssueSummary{
		Number: issue.GetNumber(),
		Title:  issue.GetTitle(),
		State:  strings.ToLower(strings.TrimSpace(issue.GetState())),
		URL:    issue.GetHTMLURL(),
	}, true, nil
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

// shaHoldMarker is the hidden ownership marker on the SHA-hold notice. The
// sweep only lifts a `hold` whose newest label event is paired with an
// App-authored notice carrying this marker (or the legacy notice sentence,
// for holds applied before the marker existed).
const shaHoldMarker = "<!-- hive:sha-hold -->"

const shaHoldLegacyNoticeSentence = "_This issue will be on hold until a SHA is provided."

const shaHoldComment = shaHoldMarker + "\n" +
	"Thanks for filing this issue! To help us reproduce and investigate, " +
	"could you please include the **commit SHA** of the build you're running?\n\n" +
	"You can find it by running:\n```\ngit rev-parse --short HEAD\n```\n\n" +
	"Or check the bottom of the console UI for the version string.\n\n" +
	shaHoldLegacyNoticeSentence + " " +
	"Simply add a comment with the SHA and the hold will be automatically removed._"

type SHAHoldConfig struct {
	PrimaryRepo     string
	AIAuthor        string
	InternalAuthors []string
	// TrustedAssociations are the GitHub author associations (OWNER, MEMBER,
	// COLLABORATOR, ...) whose missing SHA gets TrustedLabel instead of the
	// hard `hold` (hivecommons/hive#12002): the SHA request is a
	// reproduction aid, not a maintainer park.
	TrustedAssociations []string
	// TrustedLabel is the soft label for trusted reporters. Empty (or "hold")
	// keeps the legacy hard hold for every reporter.
	TrustedLabel string
}

type SHAHoldResult struct {
	Held    int `json:"held"`
	Unheld  int `json:"unheld"`
	Skipped int `json:"skipped"`
	// Softened counts SHA-sweep holds on trusted reporters' issues that were
	// swapped for the soft TrustedLabel.
	Softened int `json:"softened"`
}

// shaHoldSoftLabel returns the soft label to apply to this reporter's issue,
// or "" when the hard `hold` applies.
func (cfg SHAHoldConfig) shaHoldSoftLabel(association string) string {
	label := strings.TrimSpace(cfg.TrustedLabel)
	if label == "" || strings.EqualFold(label, "hold") {
		return ""
	}
	association = strings.TrimSpace(association)
	if association == "" {
		return ""
	}
	for _, trusted := range cfg.TrustedAssociations {
		if strings.EqualFold(strings.TrimSpace(trusted), association) {
			return label
		}
	}
	return ""
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
		held := c.isHeld(labels)
		hasSHA := shaPattern.MatchString(issue.GetBody())

		if !hasSHA {
			hasSHA = c.checkCommentsForSHA(ctx, owner, repo, issue.GetNumber(), author)
		}

		if soft := cfg.shaHoldSoftLabel(issue.GetAuthorAssociation()); soft != "" {
			c.enforceSoftSHAHold(ctx, owner, repo, issue.GetNumber(), author, soft, labels, held, hasSHA, result)
			continue
		}

		switch {
		case hasSHA && held:
			// Only release a hold this sweep applied. A human's `hold`, a hold
			// applied by another Hive subsystem through the same App (the
			// dashboard hold on a hive with no id), or any non-literal hold
			// spelling (`on-hold`, `hive-pause/<id>`) is left for its owner
			// to lift (#8927).
			ok, err := c.shaHoldIsOwn(ctx, owner, repo, issue.GetNumber())
			if err != nil {
				c.logger.Warn("SHA-UNHOLD skipped: could not verify hold provenance", "repo", repo, "issue", issue.GetNumber(), "error", err)
				result.Skipped++
				continue
			}
			if !ok {
				c.logger.Info("SHA-UNHOLD skipped: current hold was not applied by the SHA-hold sweep", "repo", repo, "issue", issue.GetNumber(), "author", author)
				result.Skipped++
				continue
			}
			c.unhold(ctx, owner, repo, issue.GetNumber())
			result.Unheld++
			c.logger.Info("SHA-UNHOLD", "repo", repo, "issue", issue.GetNumber(), "author", author)
		case !hasSHA && !held:
			c.hold(ctx, owner, repo, issue.GetNumber())
			result.Held++
			c.logger.Info("SHA-HOLD", "repo", repo, "issue", issue.GetNumber(), "author", author)
		default:
			result.Skipped++
		}
	}

	return result, nil
}

// enforceSoftSHAHold is the trusted-reporter branch of EnforceSHAHold
// (hivecommons/hive#12002). It never applies the hard `hold`: a missing SHA
// gets the soft label plus the usual notice, a provided SHA lifts the soft
// label, and a hard `hold` this sweep applied before the soft path existed is
// swapped for the soft label. A `hold` from anyone else is left alone.
func (c *Client) enforceSoftSHAHold(ctx context.Context, owner, repo string, number int, author, soft string, labels []string, held, hasSHA bool, result *SHAHoldResult) {
	softHeld := labelPresent(labels, soft)
	if held {
		ok, err := c.shaHoldIsOwn(ctx, owner, repo, number)
		if err != nil {
			c.logger.Warn("SHA-SOFTEN skipped: could not verify hold provenance", "repo", repo, "issue", number, "error", err)
			result.Skipped++
			return
		}
		if !ok {
			result.Skipped++
			return
		}
		if !hasSHA && !softHeld {
			if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{soft}); err != nil {
				c.logger.Warn("failed to add soft SHA label", "repo", repo, "issue", number, "label", soft, "error", err)
				result.Skipped++
				return
			}
		}
		c.unhold(ctx, owner, repo, number)
		if hasSHA {
			result.Unheld++
			c.logger.Info("SHA-UNHOLD", "repo", repo, "issue", number, "author", author)
			return
		}
		result.Softened++
		c.logger.Info("SHA-SOFTEN", "repo", repo, "issue", number, "author", author, "label", soft)
		return
	}
	switch {
	case hasSHA && softHeld:
		if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, soft); err != nil {
			c.logger.Warn("failed to remove soft SHA label", "repo", repo, "issue", number, "label", soft, "error", err)
			result.Skipped++
			return
		}
		result.Unheld++
		c.logger.Info("SHA-UNHOLD", "repo", repo, "issue", number, "author", author, "label", soft)
	case !hasSHA && !softHeld:
		if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{soft}); err != nil {
			c.logger.Warn("failed to add soft SHA label", "repo", repo, "issue", number, "label", soft, "error", err)
			result.Skipped++
			return
		}
		comment := &gh.IssueComment{Body: gh.Ptr(shaHoldComment)}
		if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, comment); err != nil {
			c.logger.Warn("failed to post SHA hold comment", "repo", repo, "issue", number, "error", err)
		}
		result.Held++
		c.logger.Info("SHA-HOLD", "repo", repo, "issue", number, "author", author, "label", soft)
	default:
		result.Skipped++
	}
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

// shaHoldIsOwn reports whether the issue's current `hold` was applied by this
// sweep. Two pieces of evidence are required, mirroring the level-hold and
// #5117 releases and then narrowing to this subsystem: the newest `hold`
// label event must be a `labeled` by the App bot, and an App-authored
// SHA-hold notice must have been posted at or after that event — hold() adds
// the label and immediately posts the notice, so a later re-hold by a human,
// or by another Hive subsystem through the same App, has no companion notice
// and is left alone. A blank bot login fails closed.
func (c *Client) shaHoldIsOwn(ctx context.Context, owner, repo string, number int) (bool, error) {
	byApp, labeledAt, err := c.latestHoldLabelEventByApp(ctx, owner, repo, number)
	if err != nil || !byApp {
		return false, err
	}
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	for _, comment := range comments {
		if comment == nil || !c.isTrustedAppBotCommentAuthor(comment) || !isSHAHoldNotice(comment.GetBody()) {
			continue
		}
		if !comment.GetCreatedAt().Before(labeledAt.Time) {
			return true, nil
		}
	}
	return false, nil
}

func isSHAHoldNotice(body string) bool {
	return strings.Contains(body, shaHoldMarker) || strings.Contains(body, shaHoldLegacyNoticeSentence)
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

// SetMutationBoundary installs the convergence mutation boundary around
// external GitHub write effects. nil restores the legacy passthrough path.
func (c *Client) SetMutationBoundary(boundary effects.Boundary) {
	if c == nil {
		return
	}
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	c.mutation = boundary
}

func (c *Client) mutationBoundary() effects.Boundary {
	if c == nil {
		return nil
	}

	c.mutationMu.RLock()
	defer c.mutationMu.RUnlock()
	return c.mutation
}

// MutationBoundary returns the installed external-effect boundary for packages
// such as automerge that operate through a narrow transport interface.
func (c *Client) MutationBoundary() effects.Boundary { return c.mutationBoundary() }

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

// IsHeldLabels reports whether labels carry a hold: the generic hold
// substrings (HoldLabels) plus the extra hold labels configured through
// SetHoldLabels, which is where the exact hive-scoped `hive-pause/<hive-id>`
// dashboard hold lives. It is the same predicate enumeration uses, exported so
// the auto-merge sweeps gate on the identical hold set (#8927).
func (c *Client) IsHeldLabels(labels []string) bool {
	return c.isHeld(labels)
}

// RecordPRMergedAudit records the standard PR-merged audit event. path names
// the code path that performed the merge (PRAuditPathSweep, PRAuditPathQueue,
// PRAuditPathRelay, PRAuditPathForge).
func (c *Client) RecordPRMergedAudit(repo string, number int, method, sha, path string) {
	if c == nil {
		return
	}
	credit := c.takeMergeCredit(repo, number)
	extra := []string{"method", method, "sha", sha, "path", path}
	if logins := mergeCreditLogins(credit); logins != "" {
		extra = append(extra, "issue_authors", logins)
	}
	c.recordWriteAudit(AuditActionPRMerged, InvocationMeta{Agent: AttributionAgentGovernor},
		WriteTarget{Repo: repo, Number: number}, extra...)
	// Every merge path ends here, so this is where the merged head's evidence
	// bundle gains its merge event and the PR gets its evidence pointer.
	c.recordReviewEvidenceMerge(repo, number, method, sha, path, credit.Logins())
	// A successful App merge on any path proves the repo is mergeable, so
	// retire its "merge blocked" alerts (#11924).
	c.clearMergeFailureAlertsForRepo(repo)
}

// RepoWorkBreakdown explains the raw open issue and PR totals for one
// repository. Enumeration assigns every raw item to exactly one primary
// bucket at the same choice point that determines whether it is actionable.
type RepoWorkBreakdown struct {
	Issues RepoIssueBreakdown `json:"issues"`
	PRs    RepoPRBreakdown    `json:"prs"`
}

// RepoWorkDetails carries display-only records for counted repository work that
// is not present in the actionable or held lists. It lets the dashboard explain
// every non-actionable bucket without making those items selectable by agents.
type RepoWorkDetails struct {
	Issues []RepoWorkIssue `json:"issues,omitempty"`
	// PRs are the open pull requests fetchPRs sidelined from the actionable
	// and held sets — drafts and exempt-labelled PRs — kept so the Hive
	// activity monitor can list every open PR without a second fetch
	// (hivecommons/hive#12682). Display-only: nothing may act on them.
	PRs []RepoWorkPR `json:"prs,omitempty"`
}

// RepoWorkPR is a display-only record of an open PR outside the actionable
// and held sets; Bucket is the RepoPRBreakdown bucket it was counted in
// ("draft" or "filtered") and Reason says why.
type RepoWorkPR struct {
	Repo           string    `json:"repo"`
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Author         string    `json:"author,omitempty"`
	Labels         []string  `json:"labels,omitempty"`
	Draft          bool      `json:"draft,omitempty"`
	AppAuthored    bool      `json:"app_authored,omitempty"`
	HiveAttributed bool      `json:"hive_attributed,omitempty"`
	CreatedAt      time.Time `json:"created_at,omitzero"`
	UpdatedAt      time.Time `json:"updated_at,omitzero"`
	URL            string    `json:"url,omitempty"`
	Bucket         string    `json:"bucket"`
	Reason         string    `json:"reason,omitempty"`
	// BaseRef is the target branch, retained for the covering-PR chip.
	BaseRef string `json:"base_ref,omitempty"`
	// LinkedIssues mirrors PullRequest.LinkedIssues so a sidelined PR can
	// still stand under the issue its `Fixes #N` names in the activity
	// monitor. Stamped by EnrichReviewSignalsAndWorkPRs, not by the
	// enumeration.
	LinkedIssues []PRLinkedIssue `json:"linked_issues,omitempty"`
	// ReferencedIssues mirrors PullRequest.ReferencedIssues; stamped by the
	// enumeration, which has the body.
	ReferencedIssues []PRLinkedIssue `json:"referenced_issues,omitempty"`
}

// Work-detail buckets for sidelined PRs (RepoPRBreakdown field names).
const (
	workBucketPRDraft    = "draft"
	workBucketPRFiltered = "filtered"
)

func (c *Client) repoWorkPR(repo string, pr *gh.PullRequest, labels []string, hiveAttributed bool, bucket, reason string) RepoWorkPR {
	author := safeGetLogin(pr.GetUser())
	return RepoWorkPR{
		Repo:           repo,
		Number:         pr.GetNumber(),
		Title:          pr.GetTitle(),
		Author:         author,
		Labels:         append([]string(nil), labels...),
		Draft:          pr.GetDraft(),
		AppAuthored:    c.appBotLogin != "" && strings.EqualFold(author, c.appBotLogin),
		HiveAttributed: hiveAttributed,
		CreatedAt:      pr.GetCreatedAt().Time,
		UpdatedAt:      pr.GetUpdatedAt().Time,
		URL:            pr.GetHTMLURL(),
		BaseRef:        prBaseRef(pr),
		Bucket:         bucket,
		Reason:         reason,
		// The list payload carries the body, so the soft issue links cost
		// no extra call.
		ReferencedIssues: PRReferencedIssues(pr.GetTitle(), pr.GetBody(), repo, pr.GetNumber()),
	}
}

type RepoWorkIssue struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Author string `json:"author,omitempty"`
	// AuthorIsHuman is Issue.AuthorIsHuman for a sidelined issue, so the
	// tracker can still classify the reporter of a row parked in a work
	// bucket (reporter triage, filtered, …) instead of rendering it with an
	// unknown author kind.
	AuthorIsHuman bool      `json:"author_is_human,omitempty"`
	Labels        []string  `json:"labels,omitempty"`
	Assignees     []string  `json:"assignees,omitempty"`
	CreatedAt     time.Time `json:"created_at,omitzero"`
	UpdatedAt     time.Time `json:"updated_at,omitzero"`
	URL           string    `json:"url,omitempty"`
	Bucket        string    `json:"bucket"`
	Reason        string    `json:"reason,omitempty"`
	// ClaimKind, ClaimedBy and ClaimPRStarted are Issue.ClaimKind /
	// Issue.ClaimedBy / Issue.ClaimPRStarted, copied for the issue stage
	// reconciler.
	ClaimKind      string `json:"claim_kind,omitempty"`
	ClaimedBy      string `json:"claimed_by,omitempty"`
	ClaimPRStarted bool   `json:"claim_pr_started,omitempty"`
	ClaimHive      string `json:"claim_hive,omitempty"`
	// LinkedPRs and ClaimContext carry the PR-claim evidence of an issue the
	// duplicate-PR guard suppressed into the claimed_by_pr bucket
	// (hivecommons/hive#12144), so the repo card can render the same
	// covering-PR glyph it shows on actionable issues. Both are the
	// github.Issue fields of the same name, copied as-is.
	LinkedPRs    []IssueLinkedPR    `json:"linked_prs,omitempty"`
	ClaimContext *IssueClaimContext `json:"claim_context,omitempty"`
	// Tracker metadata is retained for rows outside the actionable set.
	IsTracker bool `json:"is_tracker,omitempty"`
	// TrackerProgress is display-only checklist progress, never a parent-close signal.
	TrackerProgress *IssueChildren `json:"tracker_progress,omitempty"`
	// Children is Issue.Children, copied for the issue stage reconciler.
	Children *IssueChildren `json:"children,omitempty"`
	// RecheckAfter and OpsTracking are read from a blocked issue's body
	// (`<!-- hive:recheck-after <RFC3339> -->`, `<!-- hive:ops-tracking -->`)
	// for ReconcileBlockedIssues (hivecommons/hive#13310).
	RecheckAfter time.Time `json:"recheck_after,omitzero"`
	OpsTracking  bool      `json:"ops_tracking,omitempty"`
	// RecheckDone is set once the soak triage has stamped
	// `<!-- hive:recheck-done -->` into the body (hivecommons/hive#13311).
	RecheckDone bool `json:"recheck_done,omitempty"`
	// Advisory is the body-derived advisory classification (dedupe
	// checklist marker, "Decision required: no"), read here because the
	// body itself is not carried (hivecommons/hive#15381).
	Advisory *IssueAdvisory `json:"advisory,omitempty"`
}

func repoWorkIssueFromIssue(issue Issue, bucket, reason string) RepoWorkIssue {
	progress := TrackerTaskProgress(issue.Body)
	return RepoWorkIssue{
		Repo:            issue.Repo,
		Number:          issue.Number,
		Title:           issue.Title,
		Author:          issue.Author,
		AuthorIsHuman:   issue.AuthorIsHuman,
		Labels:          append([]string(nil), issue.Labels...),
		Assignees:       append([]string(nil), issue.Assignees...),
		CreatedAt:       issue.CreatedAt,
		UpdatedAt:       issue.UpdatedAt,
		URL:             issue.URL,
		Bucket:          bucket,
		Reason:          reason,
		ClaimKind:       issue.ClaimKind,
		ClaimedBy:       issue.ClaimedBy,
		ClaimPRStarted:  issue.ClaimPRStarted,
		ClaimHive:       issue.ClaimHive,
		LinkedPRs:       append([]IssueLinkedPR(nil), issue.LinkedPRs...),
		ClaimContext:    issue.ClaimContext,
		Children:        issue.Children,
		TrackerProgress: progress,
		IsTracker:       RendersAsTracker(issue.IsTracker, issue.Labels, progress),
		Advisory:        IssueAdvisoryFromBody(issue.Body),
	}
}

func appendRepoWorkIssueDetail(result *ActionableResult, issue Issue, bucket, reason string) {
	if result == nil {
		return
	}
	if result.WorkDetailsByRepo == nil {
		result.WorkDetailsByRepo = make(map[string]RepoWorkDetails)
	}
	details := result.WorkDetailsByRepo[issue.Repo]
	details.Issues = append(details.Issues, repoWorkIssueFromIssue(issue, bucket, reason))
	result.WorkDetailsByRepo[issue.Repo] = details
}

// workIssue is repoWorkIssue with the client's author classification, so a
// sidelined issue carries AuthorIsHuman the way an actionable one does.
func (c *Client) workIssue(repo string, issue *gh.Issue, labels []string, bucket, reason string) RepoWorkIssue {
	row := repoWorkIssue(repo, issue, labels, bucket, reason)
	if issue != nil {
		row.AuthorIsHuman = c.isHumanAuthor(issue.GetUser())
	}
	return row
}

func repoWorkIssue(repo string, issue *gh.Issue, labels []string, bucket, reason string) RepoWorkIssue {
	if issue == nil {
		return RepoWorkIssue{Repo: repo, Labels: append([]string(nil), labels...), Bucket: bucket, Reason: reason}
	}
	progress := TrackerTaskProgress(issue.GetBody())
	return RepoWorkIssue{
		Repo:            repo,
		Number:          issue.GetNumber(),
		Title:           issue.GetTitle(),
		Author:          safeGetLogin(issue.GetUser()),
		Labels:          append([]string(nil), labels...),
		Assignees:       extractAssignees(issue.Assignees),
		CreatedAt:       issue.GetCreatedAt().Time,
		UpdatedAt:       issue.GetUpdatedAt().Time,
		URL:             issue.GetHTMLURL(),
		IsTracker:       RendersAsTracker(isTracker(issue.GetTitle(), labels, issue.GetBody()), labels, progress),
		TrackerProgress: progress,
		Bucket:          bucket,
		Reason:          reason,
		Advisory:        IssueAdvisoryFromBody(issue.GetBody()),
	}
}

type RepoIssueBreakdown struct {
	Actionable          int `json:"actionable"`
	Hold                int `json:"hold"`
	HiveAdvisory        int `json:"hive_advisory"`
	DependencyDashboard int `json:"dependency_dashboard"`
	Filtered            int `json:"filtered"`
	NeedsHuman          int `json:"needs_human,omitempty"`
	// NeedsDecision counts issues parked on stage/045-needs-decision (and,
	// until migrated, the pre-045 stage/085 and the retired needs-decision /
	// needs-direction labels).
	NeedsDecision int `json:"needs_decision,omitempty"`
	NeedsSpec     int `json:"needs_spec,omitempty"`
	// PossiblyExists counts issues parked at stage/017-possible-existing
	// by the prior-art gate, waiting on their author (hivecommons/hive#13575).
	PossiblyExists       int `json:"possibly_exists,omitempty"`
	ReporterConfirmation int `json:"reporter_confirmation,omitempty"`
	Exempt               int `json:"exempt,omitempty"`
	// Blocked counts open issues parked on an unmerged PR or other outside
	// blocker (`blocked` label, hivecommons/hive#13093). Counted inside
	// Filtered; named so the repo card can say why.
	Blocked int `json:"blocked,omitempty"`
	// Tracking counts open tracking/archive issues (`hive/tracking` label,
	// `<!-- hive:tracking -->` marker or "Keep this issue open" in the
	// body, hivecommons/hive#14613). Counted inside Filtered.
	Tracking int `json:"tracking,omitempty"`
	// ReporterTriage counts open issues from reporters the hive does not
	// trust that are waiting for a maintainer's triage label (#9665). Kept
	// apart from Filtered so the repo card can say "N awaiting reporter
	// triage" rather than folding them into the generic filter bucket.
	ReporterTriage int `json:"reporter_triage,omitempty"`
	Other          int `json:"other"`
}

func (b RepoIssueBreakdown) Total() int {
	return b.Actionable + b.Hold + b.HiveAdvisory + b.DependencyDashboard + b.Filtered + b.ReporterTriage + b.Other
}

func (b *RepoIssueBreakdown) addHardSuppress(label string) {
	switch label {
	case issueNeedsHumanLabel:
		b.NeedsHuman++
	case NeedsDecisionStageLabel, legacyIssueNeedsDecisionStageLabel, issueNeedsDecisionLabel, issueNeedsDirectionLabel:
		b.NeedsDecision++
	case issueNeedsSpecLabel:
		b.NeedsSpec++
	case PossiblyExistsStageLabel:
		b.PossiblyExists++
	}
}

type RepoPRBreakdown struct {
	Actionable int `json:"actionable"`
	Hold       int `json:"hold"`
	Draft      int `json:"draft"`
	Filtered   int `json:"filtered"`
	Other      int `json:"other"`
}

func (b RepoPRBreakdown) Total() int {
	return b.Actionable + b.Hold + b.Draft + b.Filtered + b.Other
}

const actionablePriorityLabelsEnv = "HIVE_ACTIONABLE_PRIORITY_LABELS"

var defaultActionablePriorityLabels = []string{
	"triage/accepted",
	"ai-fix-requested",
	HumanAckLabel,
	"kind/bug",
	"bug",
	"priority/critical-urgent",
	"priority/important-soon",
	"help wanted",
	"good first issue",
}

// ActionablePriorityLabels returns the labels that boost human-filed work to
// the very front of kick issue lists. Operators can replace the defaults with a
// comma-separated HIVE_ACTIONABLE_PRIORITY_LABELS value.
func ActionablePriorityLabels() []string {
	raw := strings.TrimSpace(os.Getenv(actionablePriorityLabelsEnv))
	if raw == "" {
		return append([]string(nil), defaultActionablePriorityLabels...)
	}
	parts := strings.Split(raw, ",")
	labels := make([]string, 0, len(parts))
	for _, part := range parts {
		if label := strings.TrimSpace(part); label != "" {
			labels = append(labels, label)
		}
	}
	if len(labels) == 0 {
		return append([]string(nil), defaultActionablePriorityLabels...)
	}
	return labels
}

// RankActionableIssues orders the actionable issue snapshot for kick
// presentation only: human-filed work first, hive-filed work with cheap human
// acknowledgement next, and the remaining hive-filed backlog last. Counts are
// unchanged; oldest-first order is preserved within each tier.
func RankActionableIssues(issues []Issue) {
	priorityLabels := makeLabelSet(ActionablePriorityLabels())
	sort.SliceStable(issues, func(i, j int) bool {
		leftTier := actionableIssueRankTier(issues[i], priorityLabels)
		rightTier := actionableIssueRankTier(issues[j], priorityLabels)
		if leftTier != rightTier {
			return leftTier < rightTier
		}
		return issues[i].AgeMinutes > issues[j].AgeMinutes
	})
}

func makeLabelSet(labels []string) map[string]struct{} {
	set := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		if normalized := strings.ToLower(strings.TrimSpace(label)); normalized != "" {
			set[normalized] = struct{}{}
		}
	}
	return set
}

func actionableIssueRankTier(issue Issue, priorityLabels map[string]struct{}) int {
	if issue.AuthorIsHuman {
		if issueHasAnyLabel(issue.Labels, priorityLabels) {
			return 0
		}
		return 1
	}
	if issueHasCheapHumanAcknowledgement(issue) {
		return 2
	}
	return 3
}

func issueHasAnyLabel(labels []string, want map[string]struct{}) bool {
	for _, label := range labels {
		if _, ok := want[strings.ToLower(strings.TrimSpace(label))]; ok {
			return true
		}
	}
	return false
}

func issueHasCheapHumanAcknowledgement(issue Issue) bool {
	if issue.HumanAcknowledged {
		return true
	}
	if issueHasAnyLabel(issue.Labels, map[string]struct{}{HumanAckLabel: {}}) {
		return true
	}
	for _, assignee := range issue.Assignees {
		login := strings.TrimSpace(assignee)
		if login == "" || strings.HasSuffix(login, "[bot]") || strings.EqualFold(login, issue.Author) {
			continue
		}
		return true
	}
	return false
}

func (c *Client) issueHasCheapHumanAcknowledgement(issue *gh.Issue) bool {
	for _, label := range issue.Labels {
		if strings.EqualFold(label.GetName(), HumanAckLabel) {
			return true
		}
	}
	for _, assignee := range issue.Assignees {
		if c.isHumanAuthor(assignee) {
			return true
		}
	}
	return false
}

// MergeableFromState converts GitHub's mergeable_state/raw mergeable fields to a tri-state.
func MergeableFromState(state string, mergeable *bool) Mergeable {
	return mergeableFromState(state, mergeable)
}

// SetRepoPausedFunc installs the operator's per-repo pause predicate (#6203).
// The hive passes config's IsRepoPaused, so a pause taken in the dashboard is
// in force on the very next sweep with nothing to re-wire; passing nil clears
// it.
func (c *Client) SetRepoPausedFunc(fn func(repo string) bool) {
	if c == nil {
		return
	}
	c.reposMu.Lock()
	defer c.reposMu.Unlock()
	c.repoPaused = fn
}

// RepoIsPaused reports whether repo is under an operator pause (#6203). repo
// may be bare or "owner/repo" — the configured predicate normalizes both.
//
// The hive-mediated PR, merge, issue and review relays MUST check this
// themselves: they fulfil agent requests with the hive's credentials, without
// traversing the agent proxy. Proxy-only enforcement would leave agents able
// to open and merge PRs, file issues, comment, claim issues and submit reviews
// on a paused repo.
func (c *Client) RepoIsPaused(repo string) bool {
	if c == nil {
		return false
	}
	c.reposMu.RLock()
	paused := c.repoPaused
	c.reposMu.RUnlock()
	return paused != nil && paused(repo)
}

// RepoPausedReason is the operator-facing explanation written into a relay
// request's result file when the target repo is paused.
func RepoPausedReason(repo string) string {
	return "repository " + repo + " is paused by the operator — the hive is deliberately quiet on it. Resume the repo to allow agent writes again."
}

// activeRepos is getRepos() minus the operator-paused repos: the set this
// client may act on.
//
// It is deliberately NOT folded into getRepos(). getRepos() also answers
// "which repos does this hive have" for things that are not work — primaryRepo()
// reads repos[0], and filtering there would silently re-point the hive's
// primary repo at a different repository the moment an operator paused the
// first one. Work scope and identity are different questions; only the former
// is narrowed by a pause.
func (c *Client) activeRepos() []string {
	if c == nil {
		return nil
	}
	repos := c.getRepos()
	c.reposMu.RLock()
	paused := c.repoPaused
	c.reposMu.RUnlock()
	if paused == nil {
		return repos
	}
	out := make([]string, 0, len(repos))
	for _, repo := range repos {
		if paused(repo) {
			continue
		}
		out = append(out, repo)
	}
	return out
}

// ActiveRepositories is Repositories() minus the repos under an operator pause
// (#6203). It satisfies the automerge sweep's optional pause-aware transport
// capability; Repositories() stays unfiltered because it is the configured
// list, not the actionable one.
func (c *Client) ActiveRepositories() []string {
	return c.activeRepos()
}

// IsIgnorableCICheck reports whether a non-required CI context is ignored by fallback gating.
func IsIgnorableCICheck(name string) bool {
	return isIgnorableCICheck(name)
}

// IsMetaCheck reports whether a CI context is metadata-only for merge gating.
func IsMetaCheck(name string) bool {
	return isMetaCheck(name)
}

// ExtractPRLabels returns the names from a pull request label list.
func ExtractPRLabels(labels []*gh.Label) []string {
	return extractPRLabels(labels)
}

// SafeGetLogin returns a GitHub user's login, or an empty string for nil.
func SafeGetLogin(u *gh.User) string {
	return safeGetLogin(u)
}

func hardSuppressIssueBucket(filter IssueAdmitter, labels []string) string {
	if classifier, ok := filter.(HardSuppressClassifier); ok {
		return classifier.HardSuppressIssueBucket(labels)
	}
	return hardSuppressIssueLabel(labels)
}
