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
//   - the PR author is admitted by one of two paths: EITHER the author's
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
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const contributorLaneTier = "contributor-lane"

// contributorLaneClaimKind is the hive:claim kind the contribute hub stamps
// on an issue it dispatched to a relay contributor (claims.KindContributor;
// duplicated so this package keeps no dependency on pkg/claims).
const contributorLaneClaimKind = "contributor"

// Skip reasons. Every contributor-lane skip is prefixed so tick stats and the
// per-PR skip map can tell this tier's holds from the others'.
const (
	contributorLaneReasonPrefix            = contributorLaneTier + ":"
	contributorLaneReasonNoClaim           = contributorLaneReasonPrefix + "no-claim"
	contributorLaneReasonClaimAuthor       = contributorLaneReasonPrefix + "claim-author-mismatch"
	contributorLaneReasonClaimOtherHive    = contributorLaneReasonPrefix + "claim-other-hive"
	contributorLaneReasonNoLinkedIssue     = contributorLaneReasonPrefix + "no-linked-issue"
	contributorLaneReasonNoVerdict         = contributorLaneReasonPrefix + "no-verdict"
	contributorLaneReasonSoaking           = contributorLaneReasonPrefix + "soaking"
	contributorLaneReasonSentinelBlocked   = contributorLaneReasonPrefix + "sentinel-blocked"
	contributorLaneReasonReporterTrustHold = contributorLaneReasonPrefix + "reporter-trust-hold"
	contributorLaneReasonChangesRequested  = contributorLaneReasonPrefix + "changes-requested"
	contributorLaneReasonNoAppBotLogin     = contributorLaneReasonPrefix + "no-app-bot-login"
	contributorLaneReasonAppAuthored       = contributorLaneReasonPrefix + "app-authored"
	contributorLaneReasonNoAuthor          = contributorLaneReasonPrefix + "no-author"
	contributorLaneReasonClaimTier         = contributorLaneReasonPrefix + "claim-tier-untrusted"
	contributorLaneReasonClaimUnverified   = contributorLaneReasonPrefix + "claim-unverified"
	contributorLaneReasonAuthorAssociation = contributorLaneReasonPrefix + "author-association"
	contributorLaneReasonWorkflowPath      = contributorLaneReasonPrefix + "workflow-path"
	contributorLaneReasonTestsRemoved      = contributorLaneReasonPrefix + "tests-removed"
	contributorLaneReasonNoReviewer        = contributorLaneReasonPrefix + "no-reviewer"
	contributorLaneReasonFilesEvidence     = contributorLaneReasonPrefix + "files-evidence"
)

// contributorLaneReasonRegistryTrusted is the admit reason logged when the
// PR author's operator-set registry tier satisfied the tier requirement
// without a claim marker. It is not a skip reason: it names the path that
// admitted the PR so operators can tell it from a marker-admitted merge.
const contributorLaneReasonRegistryTrusted = contributorLaneReasonPrefix + "registry-trusted"

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
}

// ContributorRegistryTierFunc looks up the operator-set trust tier of a
// GitHub login in this hive's contributor registry. ok is false when the
// login has no profile; tier is returned as stored (any case).
type ContributorRegistryTierFunc func(login string) (tier string, ok bool)

// ContributorLaneVerdict is the reviewer relay's answer for one PR head.
type ContributorLaneVerdict struct {
	// Configured is false when no reviewer agent is enabled for the repo; the
	// tier then skips with contributor-lane:no-reviewer.
	Configured bool
	// Approved is true when the aggregate verdict for the head is approve and
	// merge-eligible.
	Approved bool
	// Verdict is the raw aggregate verdict for the audit comment, if any.
	Verdict           string
	DecisionRequested bool
	DecisionQuestion  string
	HumanSummaries    []string
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
		if !policy.repoAllowed(displayRepo) {
			continue
		}
		if !c.repoAutoMergeAllowed(displayRepo) {
			c.info("contributor-lane automerge sweep skipped repo", "repo", displayRepo, "reason", "repo-auto-merge-disabled")
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
	Reason       string
	// ObservedTier is the raw tier the lane saw for the author (registry
	// profile or this hive's claim marker), admitted or not. It feeds the
	// author/* stage label; admission itself still goes through Reason.
	ObservedTier string
}

// admittedByRegistry reports whether the registry path admitted the author.
func (m contributorClaimMatch) admittedByRegistry() bool {
	return m.RegistryTier != ""
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

// admitContributor resolves the author through the registry path first and
// the claim-marker path second. The registry path is the operator's own
// statement of trust for the login, so a hit needs no linked issue; a miss
// (no profile, or a tier outside RequireTiers) leaves the marker path and
// its skip reasons exactly as before.
func (c *Engine) admitContributor(ctx context.Context, owner, repo string, pr *gh.PullRequest, policy ContributorLanePolicy) (contributorClaimMatch, error) {
	author := hgithub.SafeGetLogin(pr.GetUser())
	if tier := c.admittedRegistryTier(author, policy); tier != "" {
		return contributorClaimMatch{RegistryTier: tier, ObservedTier: tier}, nil
	}
	match, err := c.verifyContributorClaim(ctx, owner, repo, pr, policy)
	if match.ObservedTier == "" {
		match.ObservedTier = c.observedRegistryTier(author)
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
	return c.contributorLaneVerdict(fullRepo, number, headSHA)
}

func (c *Engine) trySweepContributorLanePR(ctx context.Context, displayRepo, owner, repo string, number int, policy ContributorLanePolicy) (AutoMergeSweepEvent, string, error) {
	caller := hgithub.WithRESTCaller(ctx, "hive:contributor_lane_automerge_sweep")
	pr, _, err := c.gh.PullRequests.Get(caller, owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr", err
	}
	c.noteStagePR(owner, repo, number, pr)
	if reason := c.prefilterContributorLanePR(pr, policy); reason != "" {
		if reason == contributorLaneReasonAppAuthored {
			c.noteStageRole(owner, repo, number, stagelabels.RoleTrusted)
		}
		return AutoMergeSweepEvent{}, reason, nil
	}
	author := hgithub.SafeGetLogin(pr.GetUser())
	headSHA := pr.GetHead().GetSHA()
	claim, err := c.admitContributor(ctx, owner, repo, pr, policy)
	if err != nil {
		return AutoMergeSweepEvent{}, claim.Reason, err
	}
	c.noteStageRole(owner, repo, number, stagelabels.RoleFromTier(claim.ObservedTier, policy.RequireTiers))
	c.noteStageVerdict(owner, repo, number, c.contributorLaneVerdictFor(owner+"/"+repo, number, headSHA))
	if claim.Reason != "" {
		// Neither this hive's registry nor an App-authored marker vouches
		// for the author, so GitHub's association is the only evidence left;
		// an untrusted one outranks the marker-path reason (#12195, #12290).
		if !contributorLaneAssociationTrusted(pr) {
			return AutoMergeSweepEvent{}, contributorLaneReasonAuthorAssociation, nil
		}
		return AutoMergeSweepEvent{}, claim.Reason, nil
	}
	if claim.admittedByRegistry() {
		c.info("contributor-lane admitted", "repo", displayRepo, "pr", number, "author", author, "reason", contributorLaneReasonRegistryTrusted, "registry_tier", claim.RegistryTier)
	}
	files, err := ListChangedFiles(caller, c.gh, owner, repo, pr)
	if err != nil {
		return AutoMergeSweepEvent{}, contributorLaneReasonFilesEvidence, err
	}
	if reason := contributorLaneDiffReason(files); reason != "" {
		return AutoMergeSweepEvent{}, reason, nil
	}
	if held, err := c.hasReporterTrustHold(ctx, owner, repo, number); err != nil {
		return AutoMergeSweepEvent{}, "reporter-trust-check", err
	} else if held {
		return AutoMergeSweepEvent{}, contributorLaneReasonReporterTrustHold, nil
	}
	if blocked, err := c.hasOutstandingChangesRequested(ctx, owner, repo, number); err != nil {
		return AutoMergeSweepEvent{}, "review-state-check", err
	} else if blocked {
		return AutoMergeSweepEvent{}, contributorLaneReasonChangesRequested, nil
	}
	mergeableState := strings.ToLower(strings.TrimSpace(pr.GetMergeableState()))
	if mergeableState == "behind" {
		return c.updateBranchAndSkip(ctx, displayRepo, owner, repo, number, headSHA, "behind")
	}
	if mergeableState == "dirty" || mergeableState == "conflicting" {
		return AutoMergeSweepEvent{}, "conflicting", nil
	}
	method := mergeMethodFor(pr)
	fullRepo := owner + "/" + repo
	mergeClaim := effects.Claim{
		Repo:   fullRepo,
		Kind:   effects.KindPullRequestMerge,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"method": method, "expect_sha": headSHA, "lane": contributorLaneTier},
	}
	c.reconcileOpenPRMergeEffect(ctx, mergeClaim, owner, repo, number)
	baseBranch := pr.GetBase().GetRef()
	if held, reason, err := c.releaseHoldActive(ctx, owner, repo, baseBranch); err != nil {
		return AutoMergeSweepEvent{}, reason, err
	} else if held {
		return AutoMergeSweepEvent{}, reason, nil
	}
	headPushedAt := c.headPushedAt(ctx, owner, repo, number, headSHA, pr)
	green, reason, err := c.commitGreenForPR(ctx, owner, repo, baseBranch, headSHA, number, headPushedAt, expectedCheckCacheFromContext(ctx))
	if err != nil {
		return AutoMergeSweepEvent{}, reason, err
	}
	if !green {
		return AutoMergeSweepEvent{}, reason, nil
	}
	verdict := c.contributorLaneVerdictFor(fullRepo, number, headSHA)
	if !verdict.Configured {
		return AutoMergeSweepEvent{}, contributorLaneReasonNoReviewer, nil
	}
	if policy.RequireReviewerVerdict && !verdict.Approved {
		return AutoMergeSweepEvent{}, contributorLaneReasonNoVerdict, nil
	}
	now := c.now()
	soakKey := contributorSoakKey(owner, repo, number, headSHA)
	var greenSince time.Time
	if _, seen := c.contributorSoak.FirstGreen(owner, repo, number, headSHA); !seen {
		greenSince = c.headGreenSince(ctx, owner, repo, headSHA)
	}
	firstGreen := c.contributorSoak.observeGreenSince(soakKey, now, greenSince)
	if soaked := now.Sub(firstGreen); soaked < policy.Soak {
		return AutoMergeSweepEvent{}, contributorLaneSoakReason(policy.Soak - soaked), nil
	}
	if humanReason, err := c.humanMergePathGate(ctx, displayRepo, owner, repo, pr); err != nil || humanReason != "" {
		return AutoMergeSweepEvent{}, humanReason, err
	}
	current, _, err := c.gh.PullRequests.Get(caller, owner, repo, number)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) {
			return AutoMergeSweepEvent{}, "gone", nil
		}
		return AutoMergeSweepEvent{}, "fetch-pr-recheck", err
	}
	if currentHeadSHA := current.GetHead().GetSHA(); currentHeadSHA == "" || currentHeadSHA != headSHA {
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

// contributorLaneAdmitPath names the path that admitted the author for the
// merge log: the registry admit reason, or "claim" for the marker path.
func contributorLaneAdmitPath(claim contributorClaimMatch) string {
	if claim.admittedByRegistry() {
		return contributorLaneReasonRegistryTrusted
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
	if claim.admittedByRegistry() {
		return fmt.Sprintf("Merged by hive contributor-lane auto-merge: CI green for ≥%dm, %s, @%s holds the %s tier in this hive's contributor registry.",
			int(soak.Minutes()), verdictText, login, claim.RegistryTier)
	}
	return fmt.Sprintf("Merged by hive contributor-lane auto-merge: CI green for ≥%dm, %s, claim by @%s on #%d verified.",
		int(soak.Minutes()), verdictText, login, claim.Issue)
}

func (c *Engine) commentContributorLaneMerge(ctx context.Context, owner, repo string, number int, author string, claim contributorClaimMatch, soak time.Duration, verdict ContributorLaneVerdict) error {
	body := contributorLaneMergeComment(author, claim, soak, verdict)
	_, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
	return err
}
