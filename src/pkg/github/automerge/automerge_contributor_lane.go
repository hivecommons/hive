package automerge

// Contributor-lane auto-merge tier.
//
// The App tier merges only the App's own PRs and the trusted-author tier only
// merges PRs whose human author already holds merge authority. A PR that
// hive's CONTRIBUTOR LANE produced — the hub dispatched an issue to a relay
// contributor, the contributor opened the PR under their own login — matched
// neither: live on hive's own spoke, #11756 and #11840 sat CI-green for hours
// logging `untrusted-author-role` and `not-app-authored`, waiting for a
// manual `/lgtm` the maintainer never wanted to give ("if I have them trusted
// I don't want to be bothered").
//
// The claim marker proves only that the relay leased the issue to a
// connection, and relay registration is self-asserted (#12041). So the tier
// also requires the marker to say the contributor holds an operator-granted
// tier and proved the GitHub login, and adds author/diff fences of its own.
//
// This tier merges such a PR when ALL of these hold:
//   - the PR author is admitted by one of three paths: the hive ADOPTED the
//     PR in place (stage/074-adopted-by-hive) and the head is exactly the
//     one the stage store records as adopted/pushed by the hive (admit
//     reason contributor-lane:adopted-by-hive; the hive owns that head, so
//     the original author's trust tier is not consulted — if the
//     contributor pushes over it the adoption yields and the tier gates
//     below apply again); OR the author's
//     profile in this hive's contributor registry carries an operator-set
//     trust tier in require_tiers (registry_tiers_sufficient, default on;
//     admit reason contributor-lane:registry-trusted), OR the PR closes an
//     issue (closing keyword in the body, same repo) whose App-authored
//     `hive:claim` marker says kind=contributor, who=<PR author>,
//     hive=<this hive>, tier=<one of require_tiers> and verified=true. Only
//     comments by the App bot count, so a contributor cannot stamp their own
//     marker. The registry path exists because the marker path alone left
//     an operator-trusted contributor's PR waiting whenever the body merely
//     referenced the issue (`Part of #N`) or the issue was claimed outside
//     the relay (#12045, #11756);
//   - when neither admission path above applies, GitHub's author_association
//     for the PR author must not be NONE, FIRST_TIMER or
//     FIRST_TIME_CONTRIBUTOR (an unmarked, unregistered first-timer is never
//     admitted). A verified marker or registry tier is this hive's own
//     statement of trust and outranks GitHub's per-repo association, which
//     reports FIRST_TIME_CONTRIBUTOR for every relay contributor's first PR
//     to a repo (#12344: #12195, #12290 sat green and approved for 15h+ on it);
//   - the diff does not touch CI workflow/action definitions and does not
//     delete or gut test files;
//   - no excluded/hold/sentinel label, not draft, mergeable, base not under a
//     release hold, no human-merge path touched;
//   - CI is green on the head through the same commitGreenForPR gate the
//     other tiers use;
//   - a reviewer agent is configured, and its verdict for the head approves
//     (unless require_reviewer_verdict is off). With no reviewer agent the
//     tier skips rather than merging unreviewed;
//   - the head has been unchanged and green for the configured soak;
//   - the head SHA is re-verified immediately before the squash merge.

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/issueclaim"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const contributorLaneTier = prstage.StageLaneContributor

// contributorLaneClaimKind is the hive:claim kind the contribute hub stamps
// on an issue it dispatched to a relay contributor (claims.KindContributor;
// duplicated so this package keeps no dependency on pkg/claims).
const contributorLaneClaimKind = "contributor"

// Skip reasons. Every contributor-lane skip is prefixed so tick stats and the
// per-PR skip map can tell this tier's holds from the others'.
const (
	contributorLaneReasonPrefix            = contributorLaneTier + ":"
	contributorLaneReasonNoClaim           = prstage.ContributorLaneReasonNoClaim
	contributorLaneReasonClaimAuthor       = prstage.ContributorLaneReasonClaimAuthor
	contributorLaneReasonClaimOtherHive    = prstage.ContributorLaneReasonClaimOtherHive
	contributorLaneReasonNoLinkedIssue     = prstage.ContributorLaneReasonNoLinkedIssue
	contributorLaneReasonNoVerdict         = prstage.ContributorLaneReasonNoVerdict
	contributorLaneReasonSoaking           = prstage.ContributorLaneReasonSoaking
	contributorLaneReasonSentinelBlocked   = prstage.ContributorLaneReasonSentinelBlocked
	contributorLaneReasonReporterTrustHold = prstage.ContributorLaneReasonReporterTrustHold
	contributorLaneReasonChangesRequested  = prstage.ContributorLaneReasonChangesRequested
	contributorLaneReasonNoAppBotLogin     = prstage.ContributorLaneReasonNoAppBotLogin
	contributorLaneReasonAppAuthored       = prstage.ContributorLaneReasonAppAuthored
	contributorLaneReasonNoAuthor          = prstage.ContributorLaneReasonNoAuthor
	contributorLaneReasonClaimTier         = prstage.ContributorLaneReasonClaimTier
	contributorLaneReasonClaimUnverified   = prstage.ContributorLaneReasonClaimUnverified
	contributorLaneReasonAuthorAssociation = prstage.ContributorLaneReasonAuthorAssociation
	contributorLaneReasonWorkflowPath      = prstage.ContributorLaneReasonWorkflowPath
	contributorLaneReasonTestsRemoved      = prstage.ContributorLaneReasonTestsRemoved
	contributorLaneReasonNoReviewer        = prstage.ContributorLaneReasonNoReviewer
	contributorLaneReasonFilesEvidence     = prstage.ContributorLaneReasonFilesEvidence
)

// contributorLaneReasonRegistryTrusted is the admit reason logged when the
// PR author's operator-set registry tier satisfied the tier requirement
// without a claim marker. It is not a skip reason: it names the path that
// admitted the PR so operators can tell it from a marker-admitted merge.
const contributorLaneReasonRegistryTrusted = contributorLaneReasonPrefix + "registry-trusted"

// contributorLaneReasonAdoptedByHive is the admit reason logged when the
// hive's own in-place adoption of the head admitted the PR: the hive owns
// the head (stage/074-adopted-by-hive), so the original author's registry
// tier, claim marker and GitHub association are not consulted. Like the
// registry reason it names the admission path, not a skip.
const contributorLaneReasonAdoptedByHive = prstage.ContributorLaneReasonAdoptedByHive

// contributorLaneTrustedAssociations are the GitHub author_association
// values the tier accepts for an author that neither a verified claim
// marker nor a registry tier admitted. NONE, FIRST_TIMER,
// FIRST_TIME_CONTRIBUTOR, MANNEQUIN and a missing value all skip.
var contributorLaneTrustedAssociations = map[string]bool{
	"OWNER":        true,
	"MEMBER":       true,
	"COLLABORATOR": true,
	"CONTRIBUTOR":  true,
}

// contributorLaneAssociationTrusted reports whether GitHub's own
// author_association for the PR is one the tier accepts on its own.
func contributorLaneAssociationTrusted(pr *gh.PullRequest) bool {
	return contributorLaneTrustedAssociations[strings.ToUpper(strings.TrimSpace(pr.GetAuthorAssociation()))]
}

// contributorLaneWorkflowPaths are CI definitions a contributor PR must not
// touch for unattended merge: changing them changes what "CI green" means.
var contributorLaneWorkflowPaths = []string{
	".github/workflows/**", "**/.github/workflows/**", ".github/actions/**",
}

// contributorLaneTestPaths, contributorLaneTestDeletionFloor and
// contributorLaneTestRemovalRatio mirror Sentinel's test_removal rule: a PR
// that deletes a test file, or removes at least the floor of test lines while
// deleting ratio× more than it adds, skips the tier.
var contributorLaneTestPaths = []string{
	"**/*_test.go", "*_test.go", "**/test/**", "**/tests/**",
	"test/**", "tests/**", "**/__tests__/**", "**/testdata/**",
}

// contributorLaneIsTestPath reports whether name is a test file: one of
// contributorLaneTestPaths, or a `*.test.*` / `*.spec.*` file at any depth.
func contributorLaneIsTestPath(name string) bool {
	if intent.PathMatchesAny(name, contributorLaneTestPaths) {
		return true
	}
	base := path.Base(name)
	return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
}

const (
	contributorLaneTestDeletionFloor = 25
	contributorLaneTestRemovalRatio  = 3
)

// contributorSoakPruneAfter is how long a soak record outlives its last
// observation before the tracker drops it; a merged or closed PR's head stops
// being observed and must not pin memory forever.
const contributorSoakPruneAfter = 7 * 24 * time.Hour

// contributorAutoPromotionMarkerPrefix opens the one author-wide
// trust-promotion announcement: `<!-- hive:trust-promoted login=X merged=N
// scope=S repos=R -->`. contributorAutoPromotionLegacyMarkerPrefix is the
// per-PR marker posted before hivecommons/hive#15443; both count as "already
// announced on this PR".
const (
	contributorAutoPromotionMarkerPrefix       = "<!-- hive:trust-promoted"
	contributorAutoPromotionLegacyMarkerPrefix = "<!-- hive:contributor-auto-promoted"
)

// contributorHistoryReasonDemoted is the contributorMergedHistory.Reason
// for revert or sentinel evidence in the author's merges — the one outcome
// that withdraws an earlier promotion.
const contributorHistoryReasonDemoted = "revert-or-sentinel"

// ContributorLanePolicy is the live auto_merge.contributor_lane.* policy.
type ContributorLanePolicy struct {
	Enabled bool
	// HiveID is this hive's id; a claim marker must name it in hive=.
	HiveID string
	Soak   time.Duration
	// RequireReviewerVerdict gates on ContributorLaneVerdict when a reviewer
	// is configured.
	RequireReviewerVerdict bool
	Repos                  map[string]bool
	ExcludeLabels          map[string]bool
	// RequireTiers is the set of contributor tiers (lower-case) whose
	// verified claim makes a PR eligible. Empty admits nobody.
	RequireTiers map[string]bool
	// RegistryTiersSufficient admits a PR whose author holds a RequireTiers
	// tier in the contributor registry without a linked issue or claim
	// marker (auto_merge.contributor_lane.registry_tiers_sufficient).
	RegistryTiersSufficient bool
	// AutoPromoteAfterMergedPRs admits the author after this many recent
	// merged PRs have no revert or sentinel-alert evidence.
	AutoPromoteAfterMergedPR int
	// AutoPromotionScope is where those merges are counted
	// (auto_merge.contributor_lane.auto_promotion_scope): "repo" counts the
	// PR's own repository only; anything else (the default, "hive") counts
	// every repository this hive manages (#14517).
	AutoPromotionScope string
	// ApproveHeldRunsForRegistryTrusted lets the lane approve `action_required`
	// workflow runs once per head for an author the registry path admitted
	// (auto_merge.contributor_lane.approve_held_runs_for_registry_trusted).
	// See automerge_contributor_held_runs.go.
	ApproveHeldRunsForRegistryTrusted bool
	// OwnerApprovalReleasesWorkflowFence lets a trusted merger's explicit
	// approval on the current head (a GitHub APPROVED review, or a
	// dashboard / `/hive decision approve` comment) release the
	// workflow-path fence so the lane merges the PR itself
	// (auto_merge.contributor_lane.owner_approval_releases_workflow_fence,
	// default on). See automerge_contributor_workflow_approval.go.
	OwnerApprovalReleasesWorkflowFence bool
}

// Contributor-lane auto-promotion scopes; mirrors
// config.ContributorLaneAutoPromotionScope*.
const (
	contributorAutoPromotionScopeHive = "hive"
	contributorAutoPromotionScopeRepo = "repo"
)

// hiveWideAutoPromotion reports whether the policy counts clean merges across
// every repository the hive manages rather than the PR's own repository.
func (p ContributorLanePolicy) hiveWideAutoPromotion() bool {
	return !strings.EqualFold(strings.TrimSpace(p.AutoPromotionScope), contributorAutoPromotionScopeRepo)
}

// ContributorRegistryTierFunc looks up the operator-set trust tier of a
// GitHub login in this hive's contributor registry. ok is false when the
// login has no profile; tier is returned as stored (any case).
type ContributorRegistryTierFunc func(login string) (tier string, ok bool)

// ContributorLaneVerdict is the reviewer relay's answer for one PR head.
type ContributorLaneVerdict = prstage.ReviewVerdict

func normalizeContributorLaneVerdict(v ContributorLaneVerdict) ContributorLaneVerdict {
	return prstage.NormalizeReviewVerdict(v)
}

// ContributorLaneVerdictFunc resolves the reviewer verdict for repo#number at
// headSHA. repo is owner/repo.
type ContributorLaneVerdictFunc func(repo string, number int, headSHA string) ContributorLaneVerdict

// ContributorSoakTracker remembers when each (repo, pr, head) was first seen
// green so the tier can hold a head for the configured soak. One tracker is
// shared by every engine in the process (sharedContributorSoak) because the
// governor sweep builds a fresh engine per tick.
type ContributorSoakTracker struct {
	mu         sync.Mutex
	firstGreen map[string]time.Time
	lastSeen   map[string]time.Time
}

var sharedContributorSoak = NewContributorSoakTracker()

// NewContributorSoakTracker returns an empty tracker.
func NewContributorSoakTracker() *ContributorSoakTracker {
	return &ContributorSoakTracker{firstGreen: make(map[string]time.Time), lastSeen: make(map[string]time.Time)}
}

func contributorSoakKey(owner, repo string, number int, sha string) string {
	return strings.ToLower(strings.TrimSpace(owner)) + "/" + strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number) + "@" + strings.TrimSpace(sha)
}

// observeGreen records that key was green at now and returns when it was
// first seen green.
func (t *ContributorSoakTracker) observeGreen(key string, now time.Time) time.Time {
	return t.observeGreenSince(key, now, time.Time{})
}

// observeGreenSince is observeGreen with a seed for a key this process has
// not seen yet: when the forge already knows when the head went green
// (greenSince, non-zero and before now), the soak is measured from there
// rather than from this first sighting. The tracker lives in memory only, so
// without the seed every pod restart or upgrade would restart every open
// soak from zero (hivecommons/hive#12413 never finished a 2h soak across a
// day of candidate rolls).
func (t *ContributorSoakTracker) observeGreenSince(key string, now, greenSince time.Time) time.Time {
	if t == nil {
		return now
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	first, ok := t.firstGreen[key]
	if !ok {
		first = now
		if !greenSince.IsZero() && greenSince.Before(now) {
			first = greenSince
		}
		t.firstGreen[key] = first
	}
	t.lastSeen[key] = now
	for k, seen := range t.lastSeen {
		if now.Sub(seen) > contributorSoakPruneAfter {
			delete(t.lastSeen, k)
			delete(t.firstGreen, k)
		}
	}
	return first
}

// FirstGreen reports when the head was first observed green, if ever.
func (t *ContributorSoakTracker) FirstGreen(owner, repo string, number int, sha string) (time.Time, bool) {
	if t == nil {
		return time.Time{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	first, ok := t.firstGreen[contributorSoakKey(owner, repo, number, sha)]
	return first, ok
}

// headGreenSince is when the forge says sha turned green: the latest
// completed_at over its completed check runs (and the latest status
// update). Zero when nothing carries a timestamp or the lookup fails, in
// which case the caller measures from its own first sighting as before.
func (c *Engine) headGreenSince(ctx context.Context, owner, repo, sha string) time.Time {
	if c == nil || c.gh == nil || strings.TrimSpace(sha) == "" {
		return time.Time{}
	}
	caller := hgithub.WithRESTCaller(ctx, "hive:automerge_sweep")
	var latest time.Time
	runs, _, err := c.gh.Checks.ListCheckRunsForRef(caller, owner, repo, sha, &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}})
	if err != nil {
		return time.Time{}
	}
	for _, run := range runs.CheckRuns {
		if at := run.GetCompletedAt(); !at.IsZero() && at.Time.After(latest) {
			latest = at.Time
		}
	}
	if statuses, _, err := c.gh.Repositories.GetCombinedStatus(caller, owner, repo, sha, &gh.ListOptions{PerPage: 100}); err == nil {
		for _, st := range statuses.Statuses {
			if at := st.GetUpdatedAt(); !at.IsZero() && at.Time.After(latest) {
				latest = at.Time
			}
		}
	}
	return latest
}

func (c *Engine) currentContributorLanePolicy() ContributorLanePolicy {
	if c == nil || c.contributorLanePolicy == nil {
		return ContributorLanePolicy{}
	}
	p := c.contributorLanePolicy()
	if p.ExcludeLabels == nil {
		p.ExcludeLabels = map[string]bool{"hold": true, "do-not-merge": true, "needs-human": true, "blocked": true, "needs-rebase": true}
	}
	return p
}

func (p ContributorLanePolicy) repoAllowed(repo string) bool {
	if !p.Enabled {
		return false
	}
	if len(p.Repos) == 0 {
		return true
	}
	return p.Repos[strings.ToLower(strings.TrimSpace(repo))]
}

func (p ContributorLanePolicy) excludedLabel(labels []string) string {
	for _, label := range labels {
		key := strings.ToLower(strings.TrimSpace(label))
		if p.ExcludeLabels[key] || strings.HasPrefix(key, "do-not-merge/") {
			return key
		}
	}
	return ""
}

// SweepContributorLaneAutoMerges merges green, soaked, reviewer-approved PRs
// that this hive's contributor lane produced. See the file comment for the
// full gate list.
func (c *Engine) SweepContributorLaneAutoMerges(ctx context.Context, opts AutoMergeSweepOptions) (*AutoMergeSweepResult, error) {
	if !c.ready() {
		return nil, hgithub.ErrNoGitHubClient
	}
	ctx = lowPriority(ctx)
	policy := c.currentContributorLanePolicy()
	result := &AutoMergeSweepResult{}
	if !policy.Enabled {
		return result, nil
	}
	maxMerges := opts.MaxMerges
	if maxMerges <= 0 {
		maxMerges = DefaultAutoMergeSweepMaxMerges
	}
	ctx = contextWithExpectedCheckCache(ctx, newExpectedCheckCache())
	for _, displayRepo := range c.activeRepos() {
		if len(result.Merged) >= maxMerges {
			break
		}
		owner, repoName := c.transport.SplitRepo(displayRepo)
		if !policy.repoAllowed(displayRepo) || !opts.Targets.selectsRepo(owner+"/"+repoName) {
			continue
		}
		if !c.repoAutoMergeAllowed(displayRepo) {
			c.info("contributor-lane automerge sweep skipped repo", "repo", displayRepo, "reason", "repo-auto-merge-disabled")
			continue
		}
		prs, skipReason, err := c.openPullRequests(ctx, owner, repoName, opts.Targets)
		if err != nil {
			return result, err
		}
		if skipReason != "" {
			c.info("contributor-lane automerge sweep skipped repo", "repo", displayRepo, "reason", skipReason)
			continue
		}
		train := c.planMergeTrains(ctx, stageLaneContributor, displayRepo, owner, repoName, prs, func(pr *gh.PullRequest) string {
			return c.prefilterContributorLanePR(pr, policy)
		})
		for _, pr := range prs {
			if len(result.Merged) >= maxMerges {
				break
			}
			number := 0
			if pr != nil {
				number = pr.GetNumber()
			}
			result.Seen++
			stage := func(reason string, merged bool, err error) {
				c.reconcileNativeQueueGate(ctx, stageLaneContributor, owner, repoName, pr, reason)
				c.recordStage(stageLaneContributor, displayRepo, owner, repoName, pr, reason, merged, err)
			}
			if _, held := train.duplicateOf(number); held {
				result.Skipped++
				stage(c.mergeTrainDuplicateSkip(train, number), false, nil)
				continue
			}
			if _, held := train.claimConflictOf(number); held {
				result.Skipped++
				stage(c.mergeTrainClaimConflictSkip(train, number), false, nil)
				continue
			}
			if train.isSibling(number) {
				result.Skipped++
				stage(c.mergeTrainSkip(train, number), false, nil)
				continue
			}
			if reason, handled := c.mergeTrainFront(ctx, train, pr); handled {
				c.recordContributorLaneSkip(displayRepo, number, reason)
				result.Skipped++
				stage(reason, false, nil)
				continue
			}
			if reason := c.sentinelBlockedUnlessHumanChangesRequested(ctx, owner, repoName, pr, c.prefilterContributorLanePR(pr, policy)); reason != "" {
				c.recordContributorLaneSkip(displayRepo, number, reason)
				result.Skipped++
				stage(reason, false, nil)
				continue
			}
			result.Candidates++
			event, reason, err := c.trySweepContributorLanePR(ctx, displayRepo, owner, repoName, number, policy)
			if err == nil {
				c.noteMergeTrainOutcome(stageLaneContributor, owner, repoName, number, pr.GetHead().GetSHA(), reason)
			}
			if err != nil {
				c.warn("contributor-lane automerge sweep skipped PR", "repo", displayRepo, "pr", number, "reason", reason, "error", err)
				result.Skipped++
				stage(reason, false, err)
				continue
			}
			if reason != "" {
				c.recordContributorLaneSkip(displayRepo, number, reason)
				result.Skipped++
				stage(reason, false, nil)
				continue
			}
			stage("", true, nil)
			result.Merged = append(result.Merged, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
			if train.isFront(number) {
				c.mergeTrainAfterFrontMerge(ctx, train, number)
			}
		}
	}
	return result, nil
}

func (c *Engine) recordContributorLaneSkip(displayRepo string, number int, reason string) {
	c.info("automerge skip", "repo", displayRepo, "pr", number, "reason", reason, "tier", contributorLaneTier)
}

// prefilterContributorLanePR runs the gates that need nothing beyond the
// list payload. The trusted-author prefilter is not reused directly because
// its final gate (authorized-users role) is exactly what this tier replaces.
func (c *Engine) prefilterContributorLanePR(pr *gh.PullRequest, policy ContributorLanePolicy) string {
	if pr == nil {
		return "missing-head-sha"
	}
	if state := pr.GetState(); state != "" && !strings.EqualFold(state, "open") {
		return "closed"
	}
	if pr.GetDraft() {
		return "draft"
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	if strings.TrimSpace(author) == "" {
		return contributorLaneReasonNoAuthor
	}
	// The App's own PRs and allow-listed bots belong to the self-authored
	// lane; a claim marker can never make them contributor work.
	if _, ok := c.sweepLaneForAuthor(author); ok {
		return contributorLaneReasonAppAuthored
	}
	// author_association is deliberately not checked here: a verified claim
	// marker or registry tier admits a FIRST_TIME_CONTRIBUTOR, and only
	// admitContributor can tell. See trySweepContributorLanePR.
	labels := labelNames(pr.Labels)
	if reason := c.humanMergeRequiredReason(pr.GetTitle(), labels); reason != "" {
		return reason
	}
	// trusted=false so the sentinel label blocks outright: a contributor PR
	// gets the label, not the trusted-author courtesy comment.
	if blocked := c.labelBlockReason(labels, false); blocked != "" {
		if blocked == "sentinel" {
			return contributorLaneReasonSentinelBlocked
		}
		return blocked
	}
	if hasLabelFold(labels, "needs-rebase") {
		return "label:needs-rebase"
	}
	if excluded := policy.excludedLabel(labels); excluded != "" {
		return "excluded-label:" + excluded
	}
	if c.transport.IsExemptLabels(labels) {
		return "exempt-label"
	}
	if pr.GetHead() == nil || pr.GetHead().GetSHA() == "" {
		return "missing-head-sha"
	}
	return ""
}

// contributorClaimMatch is the outcome of admitting the PR author: the
// linked issue whose hive:claim marker matched, or the registry tier that
// made a marker unnecessary. Reason is set when neither path admitted.
type contributorClaimMatch struct {
	Issue        int
	RegistryTier string
	AutoPromoted int
	// AutoPromotedHiveWide is set when AutoPromoted counted clean merges
	// across the hive's repositories rather than the PR's own repository;
	// AutoPromotedRepos is the per-repo breakdown ("hive:8,docs:1").
	AutoPromotedHiveWide bool
	AutoPromotedRepos    string
	Reason               string
	// AdoptedHead is set when the hive's own in-place adoption admitted the
	// PR: the head SHA the stage store records as adopted/pushed by the
	// hive, which the lane verified equals the PR head it is evaluating.
	AdoptedHead string
	// OperatorLogin is set when a person's Approve admitted the head
	// (hivecommons/hive#15136, operatorAdmission).
	OperatorLogin string
	// ObservedTier is the raw tier the lane saw for the author (registry
	// profile or this hive's claim marker), admitted or not. It feeds the
	// author/* stage label; admission itself still goes through Reason.
	ObservedTier string
}

// admittedByRegistry reports whether the registry path admitted the author.
func (m contributorClaimMatch) admittedByRegistry() bool {
	return m.RegistryTier != ""
}

func (m contributorClaimMatch) admittedByAutoPromotion() bool {
	return m.AutoPromoted > 0
}

// admittedByAdoption reports whether the hive's in-place adoption of the
// head admitted the PR under the hive's own authority.
func (m contributorClaimMatch) admittedByAdoption() bool {
	return m.AdoptedHead != ""
}

// adoptionAutoMergeEnabled reports whether an in-place-adopted head may be
// admitted under the hive's own authority
// (governor.labels.stage.adoption.auto_merge). An engine without a stage
// policy keeps the default: on.
func (c *Engine) adoptionAutoMergeEnabled() bool {
	if c == nil || c.stageLabelPolicy == nil {
		return true
	}
	return c.stageLabelPolicy().AdoptionAutoMerge
}

// adoptedHeadOwnedByHive reports whether the stage store says the hive
// adopted this PR IN PLACE and headSHA is exactly the adopted head. A
// superseding adoption (hive/adopt-<n>) is not admitted here: the App PR it
// opened merges through the self-authored lane and closes the original. A
// contributor push over the adopted head clears AdoptedHead (MarkYielded,
// 074 → 072), so a stale record can never vouch for a head the hive did not
// own; the record alone decides, not the 074 label, which can lag.
func (c *Engine) adoptedHeadOwnedByHive(owner, repo string, number int, headSHA string) bool {
	headSHA = strings.TrimSpace(headSHA)
	if c == nil || headSHA == "" || !c.adoptionAutoMergeEnabled() {
		return false
	}
	rec, ok := c.stageStoreOrShared().Get(stageKey(owner, repo, number))
	if !ok {
		return false
	}
	return rec.AdoptedInPlace() && rec.Adopted(headSHA)
}

// admittedRegistryTier returns the PR author's registry tier when the
// policy lets it stand in for a claim marker and it is one of RequireTiers.
// A missing lookup, a missing profile or a tier outside RequireTiers all
// return "" so the caller falls through to the marker path unchanged.
func (c *Engine) admittedRegistryTier(author string, policy ContributorLanePolicy) string {
	if c == nil || c.contributorRegistryTier == nil || !policy.RegistryTiersSufficient {
		return ""
	}
	author = strings.TrimSpace(strings.TrimPrefix(author, "@"))
	if author == "" {
		return ""
	}
	tier, ok := c.contributorRegistryTier(author)
	if !ok {
		return ""
	}
	tier = strings.ToLower(strings.TrimSpace(tier))
	if tier == "" || !policy.RequireTiers[tier] {
		return ""
	}
	return tier
}

// admitContributor resolves the author through the adoption path first,
// the registry path second and the claim-marker path third. An in-place
// adoption of exactly this head is the hive's own ownership statement (the
// hive repaired and pushed it), so it needs neither tier nor linked issue.
// The registry path is the operator's own
// statement of trust for the login, so a hit needs no linked issue; a miss
// (no profile, or a tier outside RequireTiers) leaves the marker path and
// its skip reasons exactly as before.
func (c *Engine) admitContributor(ctx context.Context, owner, repo string, pr *gh.PullRequest, policy ContributorLanePolicy) (contributorClaimMatch, error) {
	author := hgithub.SafeGetLogin(pr.GetUser())
	if headSHA := pr.GetHead().GetSHA(); c.adoptedHeadOwnedByHive(owner, repo, pr.GetNumber(), headSHA) {
		return contributorClaimMatch{AdoptedHead: headSHA, ObservedTier: c.observedRegistryTier(author)}, nil
	}
	if login := c.operatorAdmission(ctx, owner, repo, pr.GetNumber(), pr.GetHead().GetSHA()); login != "" {
		return contributorClaimMatch{OperatorLogin: login, ObservedTier: c.observedRegistryTier(author)}, nil
	}
	if tier := c.admittedRegistryTier(author, policy); tier != "" {
		return contributorClaimMatch{RegistryTier: tier, ObservedTier: tier}, nil
	}
	match, err := c.verifyContributorClaim(ctx, owner, repo, pr, policy)
	if match.ObservedTier == "" {
		match.ObservedTier = c.observedRegistryTier(author)
	}
	if err == nil && match.Reason != "" && policy.AutoPromoteAfterMergedPR > 0 && !contributorRegistryTierRevoked(c.observedRegistryTier(author)) {
		history, promoteErr := c.contributorCleanMergedHistory(ctx, owner, repo, author, policy.AutoPromoteAfterMergedPR, policy.hiveWideAutoPromotion())
		if promoteErr != nil {
			return contributorClaimMatch{Reason: "contributor-lane:auto-promote-history"}, promoteErr
		}
		if history.Clean {
			c.announceContributorAutoPromotion(ctx, owner, repo, pr.GetNumber(), author, history)
			return contributorClaimMatch{AutoPromoted: history.Checked, AutoPromotedHiveWide: history.HiveWide, AutoPromotedRepos: history.repoBreakdown(), ObservedTier: "trusted"}, nil
		}
		if history.Reason == contributorHistoryReasonDemoted {
			// Revert or sentinel evidence withdraws the promotion; regaining
			// it later is announced once more.
			c.stageStoreOrShared().ClearTrustPromotion(author)
		}
		if history.Reason != "" {
			c.debug("contributor-lane auto-promotion skipped", "repo", owner+"/"+repo, "pr", pr.GetNumber(), "author", author, "reason", history.Reason, "clean_merged", history.Checked, "scope", history.scopeName(), "repos", history.repoBreakdown())
		}
	}
	return match, err
}

// noteRegistryRole records the author's CURRENT registry tier as the PR's
// role for this cycle without the deep admission pass. The lane short-cuts
// a parked PR (needs-rebase, hold, excluded label, draft) before
// admitContributor runs, and without this the stage pass fell back to the
// role/<tier> label the PR already carried — a tier the operator changed in
// the registry never reached a parked PR, so two PRs by one author showed
// different tiers (hivecommons/hive: #14822 vs #14947). The lookup is a
// local registry read, so it is cheap enough for every sweep; an author
// with no profile notes nothing and keeps the marker/label evidence.
func (c *Engine) noteRegistryRole(owner, repo string, number int, author string, policy ContributorLanePolicy) {
	if c == nil || c.contributorRegistryTier == nil {
		return
	}
	author = strings.TrimSpace(strings.TrimPrefix(author, "@"))
	if author == "" {
		return
	}
	tier, ok := c.contributorRegistryTier(author)
	if !ok {
		return
	}
	c.noteStageRole(owner, repo, number, stagelabels.RoleFromTier(tier, policy.RequireTiers))
}

// observedRegistryTier returns the author's raw registry tier for labelling,
// regardless of whether the policy lets the registry admit them.
func (c *Engine) observedRegistryTier(author string) string {
	if c == nil || c.contributorRegistryTier == nil {
		return ""
	}
	author = strings.TrimSpace(strings.TrimPrefix(author, "@"))
	if author == "" {
		return ""
	}
	tier, ok := c.contributorRegistryTier(author)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(tier))
}

// contributorRegistryTierRevoked reports whether the registry tier is the
// operator's explicit `revoked` decision. Revocation is a statement that the
// contributor must not be trusted again without a human reinstating them, so
// the merged-history ratchet never runs for such an author: their history is
// typically long and clean precisely because they used to be trusted.
func contributorRegistryTierRevoked(tier string) bool {
	return strings.EqualFold(strings.TrimSpace(tier), contributorRegistryTierRevokedName)
}

const contributorRegistryTierRevokedName = "revoked"

// contributorMergedHistory is the outcome of the clean-merged-PR ratchet
// for one author: whether the threshold was met, how many clean merges were
// counted, where they were found, and why it stopped when it did.
type contributorMergedHistory struct {
	Clean    bool
	Checked  int
	HiveWide bool
	Reason   string
	// Repos is the per-repo clean-merge count in evaluation order (the PR's
	// own repository first).
	Repos []contributorRepoMerges
}

type contributorRepoMerges struct {
	Repo  string
	Clean int
}

// repoBreakdown renders Repos as "hive:8,docs:1" for log lines and markers.
func (h contributorMergedHistory) repoBreakdown() string {
	parts := make([]string, 0, len(h.Repos))
	for _, r := range h.Repos {
		parts = append(parts, r.Repo+":"+strconv.Itoa(r.Clean))
	}
	return strings.Join(parts, ",")
}

func (h contributorMergedHistory) scopeName() string {
	return contributorAutoPromotionScopeName(h.HiveWide)
}

func contributorAutoPromotionScopeName(hiveWide bool) string {
	if hiveWide {
		return contributorAutoPromotionScopeHive
	}
	return contributorAutoPromotionScopeRepo
}

// contributorCleanMergedHistory reports whether the author's last `need`
// merges are clean: none is itself a revert, none carries the sentinel
// label, and none was undone by a later merged revert PR (matched by
// merge-commit SHA in a "This reverts commit" body or by a `Revert "<title>"`
// title) — the revert evidence the audit comment promises to have checked.
//
// With hiveWide the merges are counted across every repository the hive
// manages (#14517: trust is a property of the person, and the satellite
// repos of a multi-repo hive never reach a per-repo threshold on their own).
// The PR's own repository is walked first and settles the common case
// alone; only when it is short are the other repositories discovered, with
// one search/issues query for the author's merged PRs rather than a
// closed-PR listing per repository, and only the ones that hold a merge by
// the author are walked for revert evidence. A single revert or sentinel
// hit in any repository blocks promotion.
func (c *Engine) contributorCleanMergedHistory(ctx context.Context, owner, repo, author string, need int, hiveWide bool) (contributorMergedHistory, error) {
	history := contributorMergedHistory{HiveWide: hiveWide}
	if c == nil || c.gh == nil || need <= 0 || strings.TrimSpace(author) == "" {
		history.Reason = "disabled"
		return history, nil
	}
	walk := func(ref contributorRepoRef) (done bool, err error) {
		clean, blocked, err := c.contributorCleanMergesInRepo(ctx, ref.Owner, ref.Repo, author, need-history.Checked)
		if err != nil {
			history.Reason = "list-merged-prs"
			return true, err
		}
		history.Checked += clean
		history.Repos = append(history.Repos, contributorRepoMerges{Repo: ref.Repo, Clean: clean})
		if blocked {
			history.Reason = contributorHistoryReasonDemoted
			return true, nil
		}
		if history.Checked >= need {
			history.Clean = true
			return true, nil
		}
		return false, nil
	}
	if done, err := walk(contributorRepoRef{Owner: owner, Repo: repo}); done {
		return history, err
	}
	if hiveWide {
		for _, ref := range c.contributorMergedRepos(ctx, owner, repo, author) {
			if done, err := walk(ref); done {
				return history, err
			}
		}
	}
	history.Reason = "insufficient-clean-merges"
	return history, nil
}

// contributorCleanMergesInRepo walks one repository's recently merged PRs
// and counts the author's clean merges, up to need. blocked is true when a
// merge by the author is itself a revert, carries the sentinel label, or
// was undone by a later merged revert PR.
func (c *Engine) contributorCleanMergesInRepo(ctx context.Context, owner, repo, author string, need int) (clean int, blocked bool, err error) {
	opts := &gh.PullRequestListOptions{State: "closed", Sort: "updated", Direction: "desc", ListOptions: gh.ListOptions{PerPage: 100}}
	prs, _, err := c.gh.PullRequests.List(ctx, owner, repo, opts)
	if err != nil {
		return 0, false, err
	}
	reverted := mergedRevertTargets(prs)
	for _, old := range prs {
		if old == nil || !strings.EqualFold(hgithub.SafeGetLogin(old.GetUser()), author) || old.GetMergedAt().IsZero() {
			continue
		}
		if contributorPromotionBlockedByHistory(old) || reverted.contains(old) {
			return clean, true, nil
		}
		clean++
		if clean >= need {
			return clean, false, nil
		}
	}
	return clean, false, nil
}

type contributorRepoRef struct {
	Owner string
	Repo  string
}

// contributorSearchQueryMaxLen is GitHub's limit on a search `q` parameter.
const contributorSearchQueryMaxLen = 256

// contributorMergedRepos returns the hive's other active repositories that
// hold at least one merged PR by author, in the order the transport lists
// them, excluding the PR's own (owner, repo). It asks search/issues
// `is:pr is:merged author:<login> repo:… repo:…`, chunked to GitHub's
// 256-character query limit, so a ten-repo hive costs one or two search
// calls instead of ten closed-PR listings. A search failure degrades to the
// PR's own repository with a warning rather than blocking the sweep: the
// result is then exactly the pre-#14517 per-repo count.
func (c *Engine) contributorMergedRepos(ctx context.Context, owner, repo, author string) []contributorRepoRef {
	if c == nil || c.gh == nil || c.transport == nil {
		return nil
	}
	own := strings.ToLower(owner + "/" + repo)
	var candidates []contributorRepoRef
	seen := map[string]bool{}
	for _, display := range c.activeRepos() {
		o, r := c.transport.SplitRepo(display)
		if o == "" || r == "" {
			continue
		}
		full := strings.ToLower(o + "/" + r)
		if full == own || seen[full] {
			continue
		}
		seen[full] = true
		candidates = append(candidates, contributorRepoRef{Owner: o, Repo: r})
	}
	if len(candidates) == 0 {
		return nil
	}
	base := "is:pr is:merged author:" + strings.TrimPrefix(strings.TrimSpace(author), "@")
	found := map[string]bool{}
	query := base
	flush := func() error {
		if query == base {
			return nil
		}
		result, _, err := c.gh.Search.Issues(ctx, query, &gh.SearchOptions{Sort: "updated", Order: "desc", ListOptions: gh.ListOptions{PerPage: 100}})
		query = base
		if err != nil {
			return err
		}
		for _, issue := range result.Issues {
			if issue == nil {
				continue
			}
			if full := repoFromRepositoryURL(issue.GetRepositoryURL()); full != "" {
				found[full] = true
			}
		}
		return nil
	}
	for _, ref := range candidates {
		qualifier := " repo:" + ref.Owner + "/" + ref.Repo
		if len(query)+len(qualifier) > contributorSearchQueryMaxLen {
			if err := flush(); err != nil {
				c.warn("contributor-lane auto-promotion repo search failed; counting the PR's repository only", "repo", owner+"/"+repo, "author", author, "error", err)
				return nil
			}
		}
		query += qualifier
	}
	if err := flush(); err != nil {
		c.warn("contributor-lane auto-promotion repo search failed; counting the PR's repository only", "repo", owner+"/"+repo, "author", author, "error", err)
		return nil
	}
	out := make([]contributorRepoRef, 0, len(found))
	for _, ref := range candidates {
		if found[strings.ToLower(ref.Owner+"/"+ref.Repo)] {
			out = append(out, ref)
		}
	}
	return out
}

// repoFromRepositoryURL extracts lower-cased owner/repo from a search
// result's repository_url (https://api.github.com/repos/owner/repo).
func repoFromRepositoryURL(u string) string {
	_, rest, ok := strings.Cut(strings.TrimSpace(u), "/repos/")
	if !ok {
		return ""
	}
	parts := strings.Split(strings.TrimSuffix(rest, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return strings.ToLower(parts[0] + "/" + parts[1])
}

// revertTargets indexes what the merged revert PRs in a window undid.
type revertTargets struct {
	shas   map[string]bool
	titles map[string]bool
}

var revertsCommitRE = regexp.MustCompile(`(?i)this reverts commit ([0-9a-f]{7,40})`)

// mergedRevertTargets collects, from every merged PR in prs, the commit SHAs
// named by "This reverts commit <sha>" lines and the quoted titles of
// `Revert "<title>"` PRs.
func mergedRevertTargets(prs []*gh.PullRequest) revertTargets {
	t := revertTargets{shas: map[string]bool{}, titles: map[string]bool{}}
	for _, pr := range prs {
		if pr == nil || pr.GetMergedAt().IsZero() {
			continue
		}
		for _, m := range revertsCommitRE.FindAllStringSubmatch(pr.GetBody(), -1) {
			t.shas[strings.ToLower(m[1])] = true
		}
		title := strings.TrimSpace(pr.GetTitle())
		if len(title) > len(`Revert ""`) && strings.HasPrefix(strings.ToLower(title), `revert "`) && strings.HasSuffix(title, `"`) {
			t.titles[normalizeRevertTitle(title[len(`Revert "`):len(title)-1])] = true
		}
	}
	return t
}

// contains reports whether pr's merge commit or title was the target of a
// merged revert in the window.
func (t revertTargets) contains(pr *gh.PullRequest) bool {
	if pr == nil {
		return false
	}
	if sha := strings.ToLower(strings.TrimSpace(pr.GetMergeCommitSHA())); sha != "" {
		for target := range t.shas {
			if strings.HasPrefix(sha, target) || strings.HasPrefix(target, sha) {
				return true
			}
		}
	}
	return t.titles[normalizeRevertTitle(pr.GetTitle())]
}

// normalizeRevertTitle lowercases and strips the ` (#N)` suffix GitHub's
// squash merge appends, so `Revert "fix: x (#12)"` matches PR title `fix: x`.
func normalizeRevertTitle(title string) string {
	title = strings.ToLower(strings.TrimSpace(title))
	if i := strings.LastIndex(title, " (#"); i > 0 && strings.HasSuffix(title, ")") {
		title = strings.TrimSpace(title[:i])
	}
	return title
}

func contributorPromotionBlockedByHistory(pr *gh.PullRequest) bool {
	if pr == nil {
		return false
	}
	title := strings.ToLower(strings.TrimSpace(pr.GetTitle()))
	if strings.HasPrefix(title, "revert") || strings.Contains(strings.ToLower(pr.GetBody()), "this reverts commit") {
		return true
	}
	for _, label := range labelNames(pr.Labels) {
		if strings.EqualFold(strings.TrimSpace(label), defaultSentinelLabel) {
			return true
		}
	}
	return false
}

// contributorAutoPromotionWhere names where the clean merges were counted,
// for the audit and merge comments.
func contributorAutoPromotionWhere(hiveWide bool) string {
	if hiveWide {
		return "in this hive's repositories"
	}
	return "in this repository"
}

// announceContributorAutoPromotion posts the one author-wide promotion
// comment. Promotion is a property of the author in this hive, not of the
// PR (hivecommons/hive#15443: two open PRs by one author each got the
// comment six seconds apart), so the stage store's per-author record is
// claimed first — concurrent PRs by the author in the same sweep and PRs
// after a restart post nothing — and released again only when the post
// fails. The store is saved at once so a restart before the stage pass's
// own save does not re-announce.
func (c *Engine) announceContributorAutoPromotion(ctx context.Context, owner, repo string, number int, author string, history contributorMergedHistory) {
	store := c.stageStoreOrShared()
	if !store.ClaimTrustPromotionAnnouncement(author) {
		return
	}
	if err := c.commentContributorAutoPromotion(ctx, owner, repo, number, author, history); err != nil {
		store.ReleaseTrustPromotionAnnouncement(author)
		c.warn("contributor-lane auto-promotion audit comment failed", "repo", owner+"/"+repo, "pr", number, "author", author, "error", err)
		return
	}
	if err := store.Save(); err != nil {
		c.warn("contributor-lane auto-promotion: persisting announcement failed", "author", author, "error", err)
	}
}

// contributorAutoPromotionAnnounced reports whether body is this hive's
// promotion announcement for login, in the current or the pre-#15443 form.
func contributorAutoPromotionAnnounced(body, login string) bool {
	return strings.HasPrefix(body, contributorAutoPromotionMarkerPrefix+" login="+login+" ") ||
		strings.HasPrefix(body, contributorAutoPromotionLegacyMarkerPrefix+" author="+login+" ")
}

// commentContributorAutoPromotion posts the author-wide announcement on
// the PR that earned it. The PR's own comments are read first — one call,
// on the announcing path only — so a store rebuilt from scratch does not
// repeat an announcement already on this PR.
func (c *Engine) commentContributorAutoPromotion(ctx context.Context, owner, repo string, number int, author string, history contributorMergedHistory) error {
	if c == nil || c.gh == nil || number <= 0 {
		return nil
	}
	login := strings.TrimPrefix(author, "@")
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return err
	}
	for _, comment := range comments {
		if comment != nil && contributorAutoPromotionAnnounced(comment.GetBody(), login) {
			return nil
		}
	}
	body := fmt.Sprintf("%s login=%s merged=%d scope=%s repos=%s -->\n@%s is now trusted for contributor-lane merging in this hive (last %d merged PRs: no reverts or sentinel alerts). Applies to their open and future PRs here; CI, review verdicts, diff fences, soak and branch protection still apply.",
		contributorAutoPromotionMarkerPrefix, login, history.Checked, history.scopeName(), history.repoBreakdown(), login, history.Checked)
	_, _, err = c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
	return err
}

// verifyContributorClaim finds the linked issue whose App-authored
// contributor claim names the PR author and this hive, with a tier in
// policy.RequireTiers and verified=true. Reasons are ranked so the most
// specific failure wins: an untrusted tier beats an unverified login, which
// beats a marker for another hive, then a marker for another author, then no
// marker at all.
func (c *Engine) verifyContributorClaim(ctx context.Context, owner, repo string, pr *gh.PullRequest, policy ContributorLanePolicy) (contributorClaimMatch, error) {
	appLogin := strings.TrimSpace(c.transport.AppBotLogin())
	if appLogin == "" {
		return contributorClaimMatch{Reason: contributorLaneReasonNoAppBotLogin}, nil
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	fullRepo := owner + "/" + repo
	issues := contributorLaneLinkedIssues(pr.GetBody(), fullRepo)
	if len(issues) == 0 {
		return contributorClaimMatch{Reason: contributorLaneReasonNoLinkedIssue}, nil
	}
	sawOtherHive, sawOtherAuthor, sawUntrustedTier, sawUnverified := false, false, false, false
	observedTier := ""
	for _, issue := range issues {
		comments, err := c.listIssueComments(ctx, owner, repo, issue)
		if err != nil {
			return contributorClaimMatch{Reason: "claim-comments-fetch"}, err
		}
		for _, comment := range comments {
			if comment == nil || !strings.EqualFold(hgithub.SafeGetLogin(comment.GetUser()), appLogin) {
				continue
			}
			claim, ok := issueclaim.ParseMarker(comment.GetBody())
			if !ok || !strings.EqualFold(claim.Kind, contributorLaneClaimKind) {
				continue
			}
			if !strings.EqualFold(strings.TrimPrefix(claim.Identity, "@"), author) {
				sawOtherAuthor = true
				continue
			}
			if !strings.EqualFold(strings.TrimSpace(claim.Hive), strings.TrimSpace(policy.HiveID)) {
				sawOtherHive = true
				continue
			}
			observedTier = strings.ToLower(strings.TrimSpace(claim.Tier))
			if !policy.RequireTiers[observedTier] {
				sawUntrustedTier = true
				continue
			}
			if !claim.Verified {
				sawUnverified = true
				continue
			}
			return contributorClaimMatch{Issue: issue, ObservedTier: observedTier}, nil
		}
	}
	switch {
	case sawUntrustedTier:
		return contributorClaimMatch{Reason: contributorLaneReasonClaimTier, ObservedTier: observedTier}, nil
	case sawUnverified:
		return contributorClaimMatch{Reason: contributorLaneReasonClaimUnverified, ObservedTier: observedTier}, nil
	case sawOtherHive:
		return contributorClaimMatch{Reason: contributorLaneReasonClaimOtherHive}, nil
	case sawOtherAuthor:
		return contributorClaimMatch{Reason: contributorLaneReasonClaimAuthor}, nil
	}
	return contributorClaimMatch{Reason: contributorLaneReasonNoClaim}, nil
}

// contributorLaneLinkedIssues returns the same-repo issues the PR body closes
// with a GitHub closing keyword, ascending and de-duplicated. intent's linked
// refs supply the candidates; FindClosingKeywordReference confirms each is a
// closing reference (not `refs`/`related to`).
func contributorLaneLinkedIssues(body, fullRepo string) []int {
	seen := map[int]bool{}
	var out []int
	for _, ref := range intent.LinkedIssueRefs(body, fullRepo) {
		if ref.Number <= 0 || seen[ref.Number] || !strings.EqualFold(ref.Repo, fullRepo) {
			continue
		}
		if _, ok := hgithub.FindClosingKeywordReference(body, fullRepo, fullRepo, ref.Number); !ok {
			continue
		}
		seen[ref.Number] = true
		out = append(out, ref.Number)
	}
	sort.Ints(out)
	return out
}

// contributorLaneDiffReason inspects the changed files for the diff fences:
// CI workflow/action definitions and test deletion. It returns "" when the
// diff may be merged unattended.
func contributorLaneDiffReason(files []intent.ChangedFile) string {
	adds, dels := 0, 0
	for _, f := range files {
		if intent.PathMatchesAny(f.Filename, contributorLaneWorkflowPaths) {
			return contributorLaneReasonWorkflowPath
		}
	}
	for _, f := range files {
		if !contributorLaneIsTestPath(f.Filename) {
			continue
		}
		if strings.EqualFold(f.Status, "removed") {
			return contributorLaneReasonTestsRemoved
		}
		adds += f.Additions
		dels += f.Deletions
	}
	if dels >= contributorLaneTestDeletionFloor && dels >= contributorLaneTestRemovalRatio*max(adds, 1) {
		return contributorLaneReasonTestsRemoved
	}
	return ""
}

func (c *Engine) listIssueComments(ctx context.Context, owner, repo string, number int) ([]*gh.IssueComment, error) {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var all []*gh.IssueComment
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing comments for %s/%s#%d: %w", owner, repo, number, err)
		}
		all = append(all, comments...)
		if resp == nil || resp.NextPage == 0 {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

// hasReporterTrustHold reports whether the #9665 reporter-trust gate posted
// its notice on the PR. The gate also applies hold/needs-human, which the
// exclude set already blocks; the marker check holds even if a label was
// cleared by someone other than the maintainer.
func (c *Engine) hasReporterTrustHold(ctx context.Context, owner, repo string, number int) (bool, error) {
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	for _, comment := range comments {
		if comment != nil && strings.Contains(comment.GetBody(), hgithub.ReporterTrustNoticeMarker) {
			return true, nil
		}
	}
	return false, nil
}

func (c *Engine) contributorLaneVerdictFor(fullRepo string, number int, headSHA string) ContributorLaneVerdict {
	if c == nil || c.contributorLaneVerdict == nil {
		return ContributorLaneVerdict{}
	}
	return c.withRecordedDecision(fullRepo, number, headSHA, normalizeContributorLaneVerdict(c.contributorLaneVerdict(fullRepo, number, headSHA)))
}

// withRecordedDecision folds the trusted human's recorded decision on
// headSHA (review-human-change-requests.json) into the reviewer verdict, so
// a requires_human / decision-requested verdict the person already answered
// reads as answered on every tick and after every restart — never back to
// 085 until a new head draws a new verdict (hivecommons/hive#14679).
func (c *Engine) withRecordedDecision(fullRepo string, number int, headSHA string, v ContributorLaneVerdict) ContributorLaneVerdict {
	if c == nil || c.humanChangeRequests == nil {
		return v
	}
	req, ok := review.DecisionAnswerFor(c.humanChangeRequests, fullRepo, number, headSHA)
	if !ok {
		return v
	}
	return v.WithDecisionAnswer(strings.ToLower(req.Actor), headSHA, req.Directive())
}

// contributorLaneEval is what the contributor lane established about one PR
// before merging it: the fetched PR, its admitted author and head, the
// reviewer verdict and the effects claim the merge would be recorded under.
// trySweepContributorLanePR merges from it; the merge-request relay
// (RelayMergeRefusal) only asks whether it exists.
type contributorLaneEval struct {
	pr         *gh.PullRequest
	author     string
	headSHA    string
	claim      contributorClaimMatch
	verdict    ContributorLaneVerdict
	method     string
	mergeClaim effects.Claim
	// workflowApprover is the trusted merger whose approval released the
	// workflow-path fence for headSHA ("" when the fence did not trip).
	workflowApprover string
	// soakStartedAt / soakRemaining are the soak clock the lane holds
	// headSHA on when the skip reason is contributorLaneReasonSoaking:
	// when the head was first seen green and how long is left. The reason
	// itself carries no countdown (hivecommons/hive#15729: a reason that
	// changed every pass re-stamped the stage record and the dashboard
	// read each pass as a fresh hive note); the record keeps the deadline.
	soakStartedAt time.Time
	soakRemaining time.Duration
}

// evaluateContributorLanePR runs every contributor-lane gate except the
// merge itself and returns the lane's skip reason when one fails ("" when
// the lane would merge the PR now). The sweep and the merge-request relay
// share it so the two App merge paths apply ONE policy: tier admission,
// author association, diff fences, labels, review verdict, CI (with
// min_head_age), soak and human-merge paths. relay=true is the relay's
// read-only evaluation: a behind head is reported, not updated, and no
// effects claim is reconciled, because the relay performs the merge through
// its own path.
func (c *Engine) evaluateContributorLanePR(ctx context.Context, displayRepo, owner, repo string, number int, policy ContributorLanePolicy, relay bool) (contributorLaneEval, string, error) {
	caller := hgithub.WithRESTCaller(ctx, "hive:contributor_lane_automerge_sweep")
	pr, _, err := c.gh.PullRequests.Get(caller, owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return contributorLaneEval{}, "gone", nil
		}
		return contributorLaneEval{}, "fetch-pr", err
	}
	c.noteStagePR(owner, repo, number, pr)
	if reason := c.sentinelBlockedUnlessHumanChangesRequested(caller, owner, repo, pr, c.prefilterContributorLanePR(pr, policy)); reason != "" {
		if reason == contributorLaneReasonAppAuthored {
			c.noteStageRole(owner, repo, number, stagelabels.RoleTrusted)
		} else {
			c.noteRegistryRole(owner, repo, number, hgithub.SafeGetLogin(pr.GetUser()), policy)
		}
		return contributorLaneEval{pr: pr}, reason, nil
	}
	if reason := contributorLaneConflictReason(pr); reason != "" {
		if reason == "conflicting" {
			// A conflicted head is the contributor's to fix, and the rebase push
			// brings new held runs: approving this head's runs is wasted, so the
			// unauthorized mark must not stay the row's blocker. The next head
			// re-marks if its runs are held too (hivecommons/hive#14668).
			c.clearUnauthorized(ctx, displayRepo, owner, repo, pr, number, unauthorizedStepApproveWorkflowRuns)
		}
		return contributorLaneEval{pr: pr}, reason, nil
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	headSHA := pr.GetHead().GetSHA()
	eval := contributorLaneEval{pr: pr, author: author, headSHA: headSHA}
	claim, err := c.admitContributor(ctx, owner, repo, pr, policy)
	eval.claim = claim
	if err != nil {
		return eval, claim.Reason, err
	}
	c.noteStageRole(owner, repo, number, stagelabels.RoleFromTier(claim.ObservedTier, policy.RequireTiers))
	fullRepo := owner + "/" + repo
	verdict := c.contributorLaneVerdictFor(fullRepo, number, headSHA)
	eval.verdict = verdict
	approver, humanApproved := c.trustedHumanApprovalForRequiresHuman(ctx, owner, repo, number, headSHA, verdict)
	c.noteStageVerdict(owner, repo, number, verdict)
	// A trusted head-scoped change request answers the swarm's question
	// even when admission or a requires_human verdict would return early.
	if rv, ok := c.latestTrustedHumanChangesRequested(ctx, owner, repo, number, headSHA); ok {
		c.noteStageHumanChangeRequest(owner, repo, number, headSHA, hgithub.SafeGetLogin(rv.GetUser()))
		return eval, contributorLaneReasonChangesRequested, nil
	}
	if claim.Reason != "" {
		// Neither this hive's registry nor an App-authored marker vouches
		// for the author, so GitHub's association is the only evidence left;
		// an untrusted one outranks the marker-path reason (#12195, #12290).
		if !contributorLaneAssociationTrusted(pr) {
			return eval, contributorLaneReasonAuthorAssociation, nil
		}
		return eval, claim.Reason, nil
	}
	if claim.admittedByAdoption() {
		c.info("contributor-lane admitted", "repo", displayRepo, "pr", number, "author", author, "reason", contributorLaneReasonAdoptedByHive, "adopted_head", claim.AdoptedHead)
	}
	if claim.admittedByOperator() {
		c.info("contributor-lane admitted", "repo", displayRepo, "pr", number, "author", author, "reason", contributorLaneReasonOperatorApproval, "admitted_by", claim.OperatorLogin, "head", headSHA)
	}
	if claim.admittedByRegistry() {
		c.info("contributor-lane admitted", "repo", displayRepo, "pr", number, "author", author, "reason", contributorLaneReasonRegistryTrusted, "registry_tier", claim.RegistryTier)
	}
	if claim.admittedByAutoPromotion() {
		c.info("contributor-lane admitted", "repo", displayRepo, "pr", number, "author", author, "reason", "contributor-lane:auto-promoted", "merged_prs", claim.AutoPromoted, "scope", contributorAutoPromotionScopeName(claim.AutoPromotedHiveWide), "repos", claim.AutoPromotedRepos)
	}
	files, err := ListChangedFiles(caller, c.gh, owner, repo, pr)
	if err != nil {
		return eval, contributorLaneReasonFilesEvidence, err
	}
	if reason := contributorLaneDiffReason(files); reason != "" {
		approver, released := c.workflowFenceReleased(ctx, displayRepo, owner, repo, pr, number, headSHA, reason, policy)
		if !released {
			return eval, reason, nil
		}
		eval.workflowApprover = approver
	}
	if held, err := c.hasReporterTrustHold(ctx, owner, repo, number); err != nil {
		return eval, "reporter-trust-check", err
	} else if held {
		return eval, contributorLaneReasonReporterTrustHold, nil
	}
	if blocked, err := c.hasOutstandingChangesRequested(ctx, owner, repo, number); err != nil {
		return eval, "review-state-check", err
	} else if blocked {
		return eval, contributorLaneReasonChangesRequested, nil
	}
	mergeableState := strings.ToLower(strings.TrimSpace(pr.GetMergeableState()))
	// See trySweepSelfAuthoredPR: the queue bypass is an explicit opt-in. A
	// relay evaluation never enqueues, so it keeps the current-base gates.
	queueSkipsBase := sync.OnceValue(func() bool { return !relay && c.nativeQueueSkipsCurrentBase(ctx, owner+"/"+repo, pr) })
	if mergeableState == "behind" && !queueSkipsBase() && !c.requireCurrentBaseFor(owner+"/"+repo) {
		if relay {
			return eval, "behind", nil
		}
		_, reason, err := c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, headSHA, "behind", c.isForkPR(pr))
		return eval, reason, err
	}
	if mergeableState == "dirty" || mergeableState == "conflicting" {
		return eval, "conflicting", nil
	}
	var methodReason string
	eval.method, methodReason = c.mergeMethodWithReason(ctx, owner, repo, pr)
	eval.mergeClaim = effects.Claim{
		Repo:   fullRepo,
		Kind:   effects.KindPullRequestMerge,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: mergeClaimInputs(eval.method, methodReason, headSHA, contributorLaneTier),
	}
	if !relay {
		c.reconcileOpenPRMergeEffect(ctx, eval.mergeClaim, owner, repo, number)
	}
	baseBranch := pr.GetBase().GetRef()
	if held, reason, err := c.releaseHoldActive(ctx, owner, repo, baseBranch); err != nil {
		return eval, reason, err
	} else if held {
		return eval, reason, nil
	}
	headPushedAt := c.headPushedAt(ctx, owner, repo, number, headSHA, pr)
	green, reason, err := c.commitGreenForPR(ctx, owner, repo, baseBranch, headSHA, number, headPushedAt, expectedCheckCacheFromContext(ctx))
	if err != nil {
		return eval, reason, err
	}
	if !green {
		if strings.HasPrefix(reason, requiredCheckMissingPrefix) {
			_, reason = c.approveHeldRunsForRegistryTrusted(ctx, displayRepo, owner, repo, pr, number, headSHA, claim, policy, files, reason)
		}
		if !strings.HasPrefix(reason, unauthorizedReasonPrefix) {
			// The required check exists now (pending or red), or the head
			// moved on: whoever approved the runs did the step, so the
			// unauthorized mark comes off in this same tick (#14589).
			c.clearUnauthorized(ctx, displayRepo, owner, repo, pr, number, unauthorizedStepApproveWorkflowRuns)
		}
		return eval, reason, nil
	}
	c.clearUnauthorized(ctx, displayRepo, owner, repo, pr, number, unauthorizedStepApproveWorkflowRuns)
	if !verdict.Configured {
		return eval, contributorLaneReasonNoReviewer, nil
	}
	if policy.RequireReviewerVerdict && !verdict.Approved {
		if !humanApproved {
			if verdictBlocks(stageDetail{Verdict: verdict, VerdictKnown: true}) {
				return eval, contributorLaneReasonChangesRequested, nil
			}
			return eval, contributorLaneReasonNoVerdict, nil
		}
		c.info("contributor-lane reviewer verdict cleared by trusted human approval", "repo", displayRepo, "pr", number, "head", headSHA, "approver", approver)
	}
	now := c.now()
	soakKey := contributorSoakKey(owner, repo, number, headSHA)
	var greenSince time.Time
	if _, seen := c.contributorSoak.FirstGreen(owner, repo, number, headSHA); !seen {
		greenSince = c.headGreenSince(ctx, owner, repo, headSHA)
	}
	firstGreen := c.contributorSoak.observeGreenSince(soakKey, now, greenSince)
	if soaked := now.Sub(firstGreen); soaked < policy.Soak {
		eval.soakStartedAt, eval.soakRemaining = firstGreen, policy.Soak-soaked
		return eval, contributorLaneReasonSoaking, nil
	}
	if humanReason, err := c.humanMergePathGate(ctx, displayRepo, owner, repo, pr); err != nil || humanReason != "" {
		return eval, humanReason, err
	}
	// Current-base gate (hivecommons/hive#13634): only a green, soaked,
	// approved head is worth an update-branch commit; a relay evaluation
	// never updates the branch.
	if c.requireCurrentBaseFor(owner+"/"+repo) && !queueSkipsBase() {
		if _, reason, stop, err := c.currentBaseGate(ctx, displayRepo, owner, repo, number, baseBranch, headSHA, !relay, "behind", c.isForkPR(pr), stageLaneContributor); stop {
			return eval, reason, err
		}
	}
	if gateReason, err := c.forgestateMergeGate(ctx, owner, repo, number, headSHA); gateReason != "" || err != nil {
		return eval, gateReason, err
	}
	current, _, err := c.gh.PullRequests.Get(caller, owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return eval, "gone", nil
		}
		return eval, "fetch-pr-recheck", err
	}
	if currentHeadSHA := current.GetHead().GetSHA(); currentHeadSHA == "" || currentHeadSHA != headSHA {
		return eval, "head-changed-since-eval", nil
	}
	return eval, "", nil
}

func (c *Engine) trySweepContributorLanePR(ctx context.Context, displayRepo, owner, repo string, number int, policy ContributorLanePolicy) (AutoMergeSweepEvent, string, error) {
	eval, reason, err := c.evaluateContributorLanePR(ctx, displayRepo, owner, repo, number, policy, false)
	if err != nil || reason != "" {
		return AutoMergeSweepEvent{}, reason, err
	}
	author, headSHA, claim, verdict, method, mergeClaim := eval.author, eval.headSHA, eval.claim, eval.verdict, eval.method, eval.mergeClaim
	fullRepo := owner + "/" + repo
	if reason, err := c.tryNativeQueue(ctx, eval.pr, mergeClaim); reason != "" || err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}

	attempt := mergeAttempt{owner: owner, repo: repo, base: eval.pr.GetBase().GetRef(), number: number, head: headSHA, state: eval.pr.GetMergeableState(), method: method}
	if refused := c.knownMergeRefusal(attempt); refused != "" {
		return AutoMergeSweepEvent{}, refused, nil
	}

	mergeMessage, mergeOpts := c.mergeOptions(ctx, owner, repo, eval.pr, headSHA, method)
	var mergeResult *gh.PullRequestMergeResult
	mergeOut, err := c.executeMergeEffect(ctx, mergeClaim, owner, repo, number, headSHA, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		mergeResult, _, apiErr = c.gh.PullRequests.Merge(ctx, owner, repo, number, mergeMessage, mergeOpts)
		if apiErr != nil {
			return effects.Result{}, mergeAPIErrorNotApplied(apiErr)
		}
		return effects.Result{Provenance: mergeResult.GetSHA()}, nil
	})
	if err != nil {
		if c.requiredChecksExpected(attempt, err) {
			return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, headSHA, "required-checks-expected", c.isForkPR(eval.pr))
		}
		if reason := c.workflowFenceMergeUnauthorized(ctx, displayRepo, owner, repo, eval, err); reason != "" {
			return AutoMergeSweepEvent{}, reason, nil
		}
		if refused := c.refusedMerge(attempt, err); refused != "" {
			return AutoMergeSweepEvent{}, refused, nil
		}
		return AutoMergeSweepEvent{}, "merge-failed", err
	}
	if mergeResult == nil {
		mergeResult = &gh.PullRequestMergeResult{Merged: gh.Ptr(true), SHA: gh.Ptr(mergeOut.Provenance)}
	}
	if !mergeResult.GetMerged() {
		return AutoMergeSweepEvent{}, "merge-not-applied", nil
	}
	c.transport.RecordPRMergedAudit(fullRepo, number, method, mergeResult.GetSHA(), hgithub.PRAuditPathSweep)
	if err := c.commentContributorLaneMerge(ctx, owner, repo, number, author, claim, policy.Soak, verdict); err != nil {
		c.warn("contributor-lane automerge audit comment failed", "repo", displayRepo, "pr", number, "error", err)
	}
	event := AutoMergeSweepEvent{
		Repo:     displayRepo,
		Number:   number,
		Author:   author,
		HeadSHA:  headSHA,
		MergeSHA: mergeResult.GetSHA(),
		Tier:     contributorLaneTier,
	}
	c.info("automerge merged", "tier", contributorLaneTier, "repo", displayRepo, "pr", number, "author", author, "issue", claim.Issue, "admitted_by", contributorLaneAdmitPath(claim), "soak", policy.Soak, "merge_sha", event.MergeSHA)
	return event, "", nil
}

func contributorLaneConflictReason(pr *gh.PullRequest) string {
	if pr == nil {
		return ""
	}
	if hasLabelFold(labelNames(pr.Labels), "needs-rebase") {
		return "label:needs-rebase"
	}
	switch strings.ToLower(strings.TrimSpace(pr.GetMergeableState())) {
	case "dirty", "conflicting":
		return "conflicting"
	case "unknown":
		return "mergeable-unknown"
	default:
		return ""
	}
}

// contributorLaneAdmitPath names the path that admitted the author for the
// merge log: the adoption or registry admit reason, or "claim" for the
// marker path.
func contributorLaneAdmitPath(claim contributorClaimMatch) string {
	if claim.admittedByAdoption() {
		return contributorLaneReasonAdoptedByHive
	}
	if claim.admittedByOperator() {
		return contributorLaneReasonOperatorApproval
	}
	if claim.admittedByRegistry() {
		return contributorLaneReasonRegistryTrusted
	}
	if claim.admittedByAutoPromotion() {
		return "contributor-lane:auto-promoted"
	}
	return "claim"
}

// contributorLaneMergeComment is the one-line audit comment the tier leaves
// on the PR it merged. It names the admission path so a reader can tell a
// registry-tier merge from a claim-marker merge.
func contributorLaneMergeComment(author string, claim contributorClaimMatch, soak time.Duration, verdict ContributorLaneVerdict) string {
	verdictText := "reviewer verdict approve"
	switch {
	case !verdict.Configured:
		verdictText = "no reviewer agent configured"
	case !verdict.Approved:
		verdictText = "reviewer verdict not required"
	}
	login := strings.TrimPrefix(author, "@")
	if claim.admittedByAdoption() {
		return fmt.Sprintf("Merged by hive contributor-lane auto-merge: CI green for ≥%dm, %s, head `%s` adopted by the hive (`%s`) with @%s's commits — merged under the hive's own authority.",
			int(soak.Minutes()), verdictText, shortSHA(claim.AdoptedHead), stagelabels.AdoptedByHive.Label(), login)
	}
	if claim.admittedByOperator() {
		return fmt.Sprintf("Merged by hive contributor-lane auto-merge: CI green for ≥%dm, %s, @%s approved this head as the owner decision (`admitted_by: %s`) — merged as-is on that approval.",
			int(soak.Minutes()), verdictText, claim.OperatorLogin, stagelabels.OperatorApprovalAdmission(claim.OperatorLogin))
	}
	if claim.admittedByRegistry() {
		return fmt.Sprintf("Merged by hive contributor-lane auto-merge: CI green for ≥%dm, %s, @%s holds the %s tier in this hive's contributor registry.",
			int(soak.Minutes()), verdictText, login, claim.RegistryTier)
	}
	if claim.admittedByAutoPromotion() {
		return fmt.Sprintf("Merged by hive contributor-lane auto-merge: CI green for ≥%dm, %s, @%s was auto-promoted after %d clean merged PRs %s.",
			int(soak.Minutes()), verdictText, login, claim.AutoPromoted, contributorAutoPromotionWhere(claim.AutoPromotedHiveWide))
	}
	return fmt.Sprintf("Merged by hive contributor-lane auto-merge: CI green for ≥%dm, %s, claim by @%s on #%d verified.",
		int(soak.Minutes()), verdictText, login, claim.Issue)
}

func (c *Engine) commentContributorLaneMerge(ctx context.Context, owner, repo string, number int, author string, claim contributorClaimMatch, soak time.Duration, verdict ContributorLaneVerdict) error {
	body := contributorLaneMergeComment(author, claim, soak, verdict)
	_, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
	return err
}
