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

const contributorAutoPromotionMarkerPrefix = "<!-- hive:contributor-auto-promoted"

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
	// merged PRs in the repo have no revert or sentinel-alert evidence.
	AutoPromoteAfterMergedPR int
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

// contributorLaneSoakReason renders the soaking skip with the time left so a
// reader of the log or the dashboard knows when the merge is due.
func contributorLaneSoakReason(remaining time.Duration) string {
	return fmt.Sprintf("%s(remaining=%s)", contributorLaneReasonSoaking, remaining.Round(time.Minute))
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
				c.recordStage(stageLaneContributor, displayRepo, owner, repoName, pr, reason, merged, err)
			}
			if reason := c.prefilterContributorLanePR(pr, policy); reason != "" {
				c.recordContributorLaneSkip(displayRepo, number, reason)
				result.Skipped++
				stage(reason, false, nil)
				continue
			}
			result.Candidates++
			event, reason, err := c.trySweepContributorLanePR(ctx, displayRepo, owner, repoName, number, policy)
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
	Reason       string
	// AdoptedHead is set when the hive's own in-place adoption admitted the
	// PR: the head SHA the stage store records as adopted/pushed by the
	// hive, which the lane verified equals the PR head it is evaluating.
	AdoptedHead string
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
	if tier := c.admittedRegistryTier(author, policy); tier != "" {
		return contributorClaimMatch{RegistryTier: tier, ObservedTier: tier}, nil
	}
	match, err := c.verifyContributorClaim(ctx, owner, repo, pr, policy)
	if match.ObservedTier == "" {
		match.ObservedTier = c.observedRegistryTier(author)
	}
	if err == nil && match.Reason != "" && policy.AutoPromoteAfterMergedPR > 0 && !contributorRegistryTierRevoked(c.observedRegistryTier(author)) {
		clean, checked, reason, promoteErr := c.contributorCleanMergedHistory(ctx, owner, repo, author, policy.AutoPromoteAfterMergedPR)
		if promoteErr != nil {
			return contributorClaimMatch{Reason: "contributor-lane:auto-promote-history"}, promoteErr
		}
		if clean {
			if commentErr := c.commentContributorAutoPromotion(ctx, owner, repo, pr.GetNumber(), author, checked); commentErr != nil {
				c.warn("contributor-lane auto-promotion audit comment failed", "repo", owner+"/"+repo, "pr", pr.GetNumber(), "author", author, "error", commentErr)
			}
			return contributorClaimMatch{AutoPromoted: checked, ObservedTier: "trusted"}, nil
		}
		if reason != "" {
			c.debug("contributor-lane auto-promotion skipped", "repo", owner+"/"+repo, "pr", pr.GetNumber(), "author", author, "reason", reason, "clean_merged", checked)
		}
	}
	return match, err
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

// contributorCleanMergedHistory walks the repository's recently merged PRs and
// reports whether the author's last `need` merges are clean: none is itself a
// revert, none carries the sentinel label, and none was undone by a later
// merged revert PR (matched by merge-commit SHA in a "This reverts commit"
// body or by a `Revert "<title>"` title) — the revert evidence the audit
// comment promises to have checked.
func (c *Engine) contributorCleanMergedHistory(ctx context.Context, owner, repo, author string, need int) (bool, int, string, error) {
	if c == nil || c.gh == nil || need <= 0 || strings.TrimSpace(author) == "" {
		return false, 0, "disabled", nil
	}
	opts := &gh.PullRequestListOptions{State: "closed", Sort: "updated", Direction: "desc", ListOptions: gh.ListOptions{PerPage: 100}}
	prs, _, err := c.gh.PullRequests.List(ctx, owner, repo, opts)
	if err != nil {
		return false, 0, "list-merged-prs", err
	}
	reverted := mergedRevertTargets(prs)
	clean := 0
	for _, old := range prs {
		if old == nil || !strings.EqualFold(hgithub.SafeGetLogin(old.GetUser()), author) || old.GetMergedAt().IsZero() {
			continue
		}
		if contributorPromotionBlockedByHistory(old) || reverted.contains(old) {
			return false, clean, "revert-or-sentinel", nil
		}
		clean++
		if clean >= need {
			return true, clean, "", nil
		}
	}
	return false, clean, "insufficient-clean-merges", nil
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

func (c *Engine) commentContributorAutoPromotion(ctx context.Context, owner, repo string, number int, author string, merged int) error {
	if c == nil || c.gh == nil || number <= 0 {
		return nil
	}
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return err
	}
	for _, comment := range comments {
		if comment != nil && strings.HasPrefix(comment.GetBody(), contributorAutoPromotionMarkerPrefix) {
			return nil
		}
	}
	body := fmt.Sprintf("%s author=%s merged=%d -->\n@%s is auto-promoted for contributor-lane merging on this PR: the last %d merged PRs by this author in this repository have no revert or sentinel-alert evidence. CI, reviewer verdict, diff fences, soak, holds and branch protection still apply.",
		contributorAutoPromotionMarkerPrefix, strings.TrimPrefix(author, "@"), merged, strings.TrimPrefix(author, "@"), merged)
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
	return normalizeContributorLaneVerdict(c.contributorLaneVerdict(fullRepo, number, headSHA))
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
	if reason := c.prefilterContributorLanePR(pr, policy); reason != "" {
		if reason == contributorLaneReasonAppAuthored {
			c.noteStageRole(owner, repo, number, stagelabels.RoleTrusted)
		}
		return contributorLaneEval{pr: pr}, reason, nil
	}
	if reason := contributorLaneConflictReason(pr); reason != "" {
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
	if claim.admittedByRegistry() {
		c.info("contributor-lane admitted", "repo", displayRepo, "pr", number, "author", author, "reason", contributorLaneReasonRegistryTrusted, "registry_tier", claim.RegistryTier)
	}
	if claim.admittedByAutoPromotion() {
		c.info("contributor-lane admitted", "repo", displayRepo, "pr", number, "author", author, "reason", "contributor-lane:auto-promoted", "merged_prs", claim.AutoPromoted)
	}
	files, err := ListChangedFiles(caller, c.gh, owner, repo, pr)
	if err != nil {
		return eval, contributorLaneReasonFilesEvidence, err
	}
	if reason := contributorLaneDiffReason(files); reason != "" {
		return eval, reason, nil
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
	if mergeableState == "behind" {
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
		return eval, reason, nil
	}
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
		return eval, contributorLaneSoakReason(policy.Soak - soaked), nil
	}
	if humanReason, err := c.humanMergePathGate(ctx, displayRepo, owner, repo, pr); err != nil || humanReason != "" {
		return eval, humanReason, err
	}
	// Current-base gate (hivecommons/hive#13634): only a green, soaked,
	// approved head is worth an update-branch commit; a relay evaluation
	// never updates the branch.
	if _, reason, stop, err := c.currentBaseGate(ctx, displayRepo, owner, repo, number, baseBranch, headSHA, !relay, "behind", c.isForkPR(pr)); stop {
		return eval, reason, err
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
	attempt := mergeAttempt{owner: owner, repo: repo, base: eval.pr.GetBase().GetRef(), number: number, head: headSHA, state: eval.pr.GetMergeableState(), method: method}
	if refused := c.knownMergeRefusal(attempt); refused != "" {
		return AutoMergeSweepEvent{}, refused, nil
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
			return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, headSHA, "required-checks-expected", c.isForkPR(eval.pr))
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
	if claim.admittedByRegistry() {
		return fmt.Sprintf("Merged by hive contributor-lane auto-merge: CI green for ≥%dm, %s, @%s holds the %s tier in this hive's contributor registry.",
			int(soak.Minutes()), verdictText, login, claim.RegistryTier)
	}
	if claim.admittedByAutoPromotion() {
		return fmt.Sprintf("Merged by hive contributor-lane auto-merge: CI green for ≥%dm, %s, @%s was auto-promoted after %d clean merged PRs in this repository.",
			int(soak.Minutes()), verdictText, login, claim.AutoPromoted)
	}
	return fmt.Sprintf("Merged by hive contributor-lane auto-merge: CI green for ≥%dm, %s, claim by @%s on #%d verified.",
		int(soak.Minutes()), verdictText, login, claim.Issue)
}

func (c *Engine) commentContributorLaneMerge(ctx context.Context, owner, repo string, number int, author string, claim contributorClaimMatch, soak time.Duration, verdict ContributorLaneVerdict) error {
	body := contributorLaneMergeComment(author, claim, soak, verdict)
	_, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
	return err
}
