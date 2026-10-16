package automerge

// Stage labels (hivecommons/hive#12429). Each automerge lane already computes
// a per-PR reason every cycle; this file maps that reason (plus the review
// verdict, CI state and the author's admission tier the lane used) onto the
// hive-owned `stage/NN-*`, `author/*` and `stage/stalled|regressed` label
// families in pkg/stagelabels, and reconciles them against the labels the PR
// list payload already carries — no per-PR GET, writes only on change.
//
// Flow per cycle: every lane records an observation for every PR it sees
// (recordStage). After the last lane, ApplyStageLabels picks the owning
// lane's observation per PR (self-authored > trusted-author > contributor
// lane; a lane that merely hands the PR off has no opinion), computes the
// desired set, plans add/remove against the current labels and writes the
// difference. Stage timing lives in stagelabels.Store (persisted) so stalled
// detection needs no GitHub timeline reads.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// StageLabelPolicy is the live `governor.labels.stage` policy.
type StageLabelPolicy struct {
	Enabled bool
	// Dwell overrides the stage table's expected dwell per stage slug.
	Dwell map[string]time.Duration
	// ContributorNudge turns the stage-072 nudge comment + relay interrupt
	// on (governor.labels.stage.contributor_nudge.enabled, default true).
	ContributorNudge bool
	// RenudgeInterval is the delay before the single re-nudge; 0 uses the
	// stage's dwell (the stall threshold).
	RenudgeInterval time.Duration
	// Adoption turns the stage-074 adoption on
	// (governor.labels.stage.adoption.enabled, default true): a contributor
	// PR still red after the grace is superseded by an App PR that preserves
	// the author's commits.
	Adoption bool
	// GraceOnline / GraceOffline are how long after the first nudge for a
	// head the hive waits before adopting, depending on whether the author's
	// relay was online at the nudge (defaults 2h / 1h).
	GraceOnline  time.Duration
	GraceOffline time.Duration
	// HiveID is this hive's id; hive/<HiveID> on a PR the App did not author
	// is a stray the reconciler removes (hivecommons/hive#12545).
	HiveID string
}

// ownershipRoutingPrefix is the lane routing label family. `agent/<lane>`
// means "this hive agent owns the work" (prOwningLane, the classifier, the
// CI-repair router read it); together with hive/<id> ("authored by this
// hive") it belongs only on PRs the App itself authored. Adoption (stage/074)
// opens a NEW App PR, which carries both legitimately; the adopted
// contributor PR keeps its own authorship and must not carry either.
const ownershipRoutingPrefix = "agent/"

// strayOwnershipLabels returns the agent/* and hive/<hiveID> labels on a PR
// that the App did not author. appLogin and author are compared with the
// [bot] suffix tolerated, as the rest of the engine does; an unknown author
// or App login yields nothing (fail closed: never strip on a guess).
func strayOwnershipLabels(labels []string, author, appLogin, hiveID string) []string {
	norm := func(s string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "[bot]")) }
	a, self := norm(author), norm(appLogin)
	if a == "" || self == "" || a == self {
		return nil
	}
	prov := strings.ToLower(hgithub.HiveProvenanceLabel(hiveID))
	var stray []string
	for _, l := range labels {
		lower := strings.ToLower(strings.TrimSpace(l))
		if strings.HasPrefix(lower, ownershipRoutingPrefix) || (prov != "" && lower == prov) {
			stray = append(stray, strings.TrimSpace(l))
		}
	}
	return stray
}

// stripStrayOwnershipLabels self-heals a contributor PR that picked up the
// hive's routing/provenance labels — a review-fix kick's `gh pr edit --body`
// on a human's PR used to stamp agent/<lane> + hive/<id> onto it
// (hivecommons/hive#12545), re-routing a PR the hive never owned. Returns the
// number of labels removed.
func (c *Engine) stripStrayOwnershipLabels(ctx context.Context, obs stageObservation, policy StageLabelPolicy) (int, error) {
	stray := strayOwnershipLabels(obs.Labels, obs.Author, c.transport.AppBotLogin(), policy.HiveID)
	if len(stray) == 0 {
		return 0, nil
	}
	labeler := engineStageLabeler{c.gh}
	removed := 0
	for _, l := range stray {
		if err := labeler.RemoveLabel(ctx, obs.Owner, obs.Repo, obs.Number, l); err != nil {
			c.warn("stage labels: stray ownership label removal failed", "repo", obs.Display, "pr", obs.Number, "label", l, "error", err)
			return removed, err
		}
		removed++
	}
	c.info("stage labels: stray ownership labels removed from non-hive PR", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "removed", stray, "lane", obs.Lane)
	return removed, nil
}

// stageRESTCaller tags the label/comment writes for /api/gh-rate-limits.
const stageRESTCaller = "hive:stage_labels"

// stageStorePruneAge drops stage records for PRs not listed for this long.
const stageStorePruneAge = 14 * 24 * time.Hour

// Lane names as recorded on observations.
const (
	stageLaneSelf        = "self-authored"
	stageLaneTrusted     = "trusted-author"
	stageLaneContributor = contributorLaneTier
)

var stageLanePrecedence = map[string]int{stageLaneSelf: 3, stageLaneTrusted: 2, stageLaneContributor: 1}

// stageDetail is what a lane's deep evaluation learned beyond the reason:
// the reviewer verdict for the head and the author's admission tier.
type stageDetail struct {
	Verdict      ContributorLaneVerdict
	VerdictKnown bool
	Author       stagelabels.Author
	// RedChecks names the completed-red, non-required checks the CI gate
	// walked past on RedChecksHead (sorted). The lane ignores them by
	// design; the stage engine must not call the PR merge-queued or
	// awaiting admission while the rollup on its head shows a red check
	// (hivecommons/hive#12538, seen on #12457).
	RedChecks     []string
	RedChecksHead string
}

// redCheck returns the first red non-required check on head, or "".
func (d stageDetail) redCheck(head string) string {
	if len(d.RedChecks) == 0 || (head != "" && d.RedChecksHead != "" && d.RedChecksHead != head) {
		return ""
	}
	return d.RedChecks[0]
}

// stageReasonOptionalCheckFailing prefixes the recorded reason when a red
// non-required check put the PR in its repair stage.
const stageReasonOptionalCheckFailing = "optional-check-failing:"

// stageObservation is one lane's view of one PR in one cycle.
type stageObservation struct {
	Lane    string
	Display string
	Owner   string
	Repo    string
	Number  int
	HeadSHA string
	Labels  []string
	Reason  string
	Merged  bool
	Err     bool
	Detail  stageDetail
	// Author, Title, URL and OpenedAt feed the stage-072 contributor nudge.
	Author   string
	Title    string
	URL      string
	OpenedAt time.Time
	// BaseRef and Body feed the stage-074 adoption (superseding PR target
	// and the Closes lines to carry over); both come from the list payload.
	BaseRef string
	Body    string
	// HeadRepo is the head's owner/repo (differs from Display on a fork).
	HeadRepo string
}

// StageTracker accumulates observations across the lanes of one cycle. One
// tracker is shared process-wide (sharedStageTracker) because the governor
// sweep builds a fresh engine per lane.
type StageTracker struct {
	mu      sync.Mutex
	obs     map[string]map[string]stageObservation // key → lane → observation
	details map[string]stageDetail                 // key → detail noted by trySweep*
	ensured map[string]bool                        // repo → labels provisioned this process
}

// NewStageTracker returns an empty tracker.
func NewStageTracker() *StageTracker {
	return &StageTracker{obs: map[string]map[string]stageObservation{}, details: map[string]stageDetail{}, ensured: map[string]bool{}}
}

var sharedStageTracker = NewStageTracker()

func stageKey(owner, repo string, number int) string {
	return stagelabels.Key(owner+"/"+repo, number)
}

func (t *StageTracker) note(key string, fn func(*stageDetail)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.details[key]
	fn(&d)
	t.details[key] = d
}

func (t *StageTracker) record(obs stageObservation) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := stageKey(obs.Owner, obs.Repo, obs.Number)
	lanes := t.obs[key]
	if lanes == nil {
		lanes = map[string]stageObservation{}
		t.obs[key] = lanes
	}
	lanes[obs.Lane] = obs
}

// drain returns and clears the cycle's observations, sorted by key.
func (t *StageTracker) drain() [][]stageObservation {
	t.mu.Lock()
	defer t.mu.Unlock()
	keys := make([]string, 0, len(t.obs))
	for k := range t.obs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][]stageObservation, 0, len(keys))
	for _, k := range keys {
		lanes := t.obs[k]
		detail := t.details[k]
		list := make([]stageObservation, 0, len(lanes))
		for _, o := range lanes {
			// Details noted by any lane's deep evaluation (verdict, author
			// tier) apply to every lane's view of the PR.
			if detail.VerdictKnown {
				o.Detail.Verdict, o.Detail.VerdictKnown = detail.Verdict, true
			}
			if detail.Author != stagelabels.AuthorNone {
				o.Detail.Author = detail.Author
			}
			if detail.RedChecksHead == "" || o.HeadSHA == "" || detail.RedChecksHead == o.HeadSHA {
				o.Detail.RedChecks, o.Detail.RedChecksHead = detail.RedChecks, detail.RedChecksHead
			}
			list = append(list, o)
		}
		sort.Slice(list, func(i, j int) bool { return stageLanePrecedence[list[i].Lane] > stageLanePrecedence[list[j].Lane] })
		out = append(out, list)
	}
	t.obs = map[string]map[string]stageObservation{}
	t.details = map[string]stageDetail{}
	return out
}

func (c *Engine) stagePolicy() StageLabelPolicy {
	if c == nil || c.stageLabelPolicy == nil {
		return StageLabelPolicy{}
	}
	return c.stageLabelPolicy()
}

func (c *Engine) stageTrackerOrShared() *StageTracker {
	if c != nil && c.stageTracker != nil {
		return c.stageTracker
	}
	return sharedStageTracker
}

func (c *Engine) stageStoreOrShared() *stagelabels.Store {
	if c != nil && c.stageStore != nil {
		return c.stageStore
	}
	return stagelabels.Shared()
}

// recordStage is called by every lane for every PR it listed, with the
// reason it settled on (""+merged for a merge). It is a no-op when the
// policy is off so lanes never pay for it.
func (c *Engine) recordStage(lane, displayRepo, owner, repo string, pr *gh.PullRequest, reason string, merged bool, err error) {
	if !c.stagePolicy().Enabled || pr == nil || pr.GetNumber() <= 0 {
		return
	}
	obs := stageObservation{
		Lane:    lane,
		Display: displayRepo,
		Owner:   owner,
		Repo:    repo,
		Number:  pr.GetNumber(),
		Labels:  labelNames(pr.Labels),
		Reason:  reason,
		Merged:  merged,
		Err:     err != nil,
	}
	if pr.GetHead() != nil {
		obs.HeadSHA = pr.GetHead().GetSHA()
	}
	if pr.GetHead() != nil && pr.GetHead().GetRepo() != nil {
		obs.HeadRepo = pr.GetHead().GetRepo().GetFullName()
	}
	if pr.GetBase() != nil {
		obs.BaseRef = pr.GetBase().GetRef()
	}
	obs.Author = hgithub.SafeGetLogin(pr.GetUser())
	obs.Title = pr.GetTitle()
	obs.URL = pr.GetHTMLURL()
	obs.Body = pr.GetBody()
	obs.OpenedAt = pr.GetCreatedAt().Time
	c.stageTrackerOrShared().record(obs)
}

// noteStageVerdict records the reviewer verdict a lane resolved for the PR
// head, so the stage can tell review-pending from reviewed even when the
// lane's own reason stopped at CI.
func (c *Engine) noteStageVerdict(owner, repo string, number int, v ContributorLaneVerdict) {
	if !c.stagePolicy().Enabled {
		return
	}
	c.stageTrackerOrShared().note(stageKey(owner, repo, number), func(d *stageDetail) {
		d.Verdict, d.VerdictKnown = v, true
	})
}

// noteStageAuthor records the author's admission tier as the lane computed
// it. Lanes disagree by design (the contributor lane reports "untrusted"
// for a maintainer with no registry profile), so tiers merge by precedence
// rather than last-writer-wins (#12490).
func (c *Engine) noteStageAuthor(owner, repo string, number int, a stagelabels.Author) {
	if !c.stagePolicy().Enabled || a == stagelabels.AuthorNone {
		return
	}
	c.stageTrackerOrShared().note(stageKey(owner, repo, number), func(d *stageDetail) { d.Author = d.Author.Merge(a) })
}

// noteStageRedChecks records the red non-required checks the CI gate
// skipped on head, replacing any earlier note (a later evaluation of the
// same or a newer head is the fresher rollup).
func (c *Engine) noteStageRedChecks(owner, repo string, number int, head string, names []string) {
	if !c.stagePolicy().Enabled {
		return
	}
	c.stageTrackerOrShared().note(stageKey(owner, repo, number), func(d *stageDetail) {
		d.RedChecks, d.RedChecksHead = append([]string(nil), names...), head
	})
}

// stageVerdict is a lane's opinion about a PR's stage.
type stageVerdict struct {
	Stage stagelabels.Stage
	// Rank orders competing opinions; see the stageRank* constants.
	Rank int
	// Terminal means the PR left the pipeline (merged/closed/gone): remove
	// every hive-owned label and forget its timing.
	Terminal bool
	// Reason, when set, replaces the lane's skip reason as the explanation
	// recorded with the stage (store, regression/stall comments, log): the
	// lane's reason may be a CI state while the stage was really decided
	// by the reviewer verdict.
	Reason string
}

// Opinion ranks. A lane that cannot vouch for the author has only a weak
// opinion ("needs a human, as far as this lane can tell"): another lane may
// admit the same author, and its owned opinion wins. The contributor lane's
// admission reasons name the registry/claim tier, so they outrank the
// trusted-author lane's bare role check when both are weak.
const (
	stageRankNone      = 0
	stageRankRole      = 1
	stageRankAdmission = 2
	stageRankOwned     = 3
)

// ciReasonKind classifies a commitGreenForPR reason.
type ciReasonKind int

const (
	ciReasonNone ciReasonKind = iota
	ciReasonPending
	ciReasonRed
)

func classifyCIReason(reason string) ciReasonKind {
	switch {
	case reason == "":
		return ciReasonNone
	case hgithub.CommitCIReasonIsPending(reason), reason == "ci-unverified", reason == "check-pending",
		strings.HasPrefix(reason, "required-check-pending:"), ciReasonInconclusive(reason):
		return ciReasonPending
	case strings.HasPrefix(reason, "required-check-failing:"), strings.HasPrefix(reason, "check-failing:"),
		strings.HasPrefix(reason, "status-"), strings.HasPrefix(reason, "check-"), reason == "check-runs":
		return ciReasonRed
	}
	return ciReasonNone
}

// ciReasonInconclusive matches a check that finished without a verdict
// (neutral, skipped). commitGreenForPR never blocks on these, but should a
// reason ever carry one it is "not red": the PR is waiting, not failing.
func ciReasonInconclusive(reason string) bool {
	for _, c := range []string{"neutral", "skipped"} {
		if strings.HasSuffix(reason, "-"+c) {
			return true
		}
	}
	return false
}

// stageVerdictReason is the recorded reason when the reviewer verdict, not
// the lane's skip reason, decided the stage.
func stageVerdictReason(d stageDetail) string {
	return "reviewer-verdict:" + strings.ToLower(strings.TrimSpace(d.Verdict.Verdict))
}

// verdictBlocks reports whether a known reviewer verdict asks for changes.
func verdictBlocks(d stageDetail) bool {
	if !d.VerdictKnown || d.Verdict.Approved {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(d.Verdict.Verdict))
	return v == "changes_requested" || v == "reject" || v == "rejected" || v == "request_changes"
}

// stageHandoffReasons are reasons that mean "not this lane's PR".
var stageHandoffReasons = map[string]bool{
	"not-app-authored": true, trustedBotLaneSkipReason: true, "repo-auto-merge-disabled": true,
	contributorLaneReasonAppAuthored: true, contributorLaneReasonNoAuthor: true, "missing-head-sha": true,
	"fetch-pr": true, "fetch-pr-recheck": true,
}

// stageAdmissionReasons are reasons that mean "this lane cannot vouch for
// the author": the trusted-author lane's role check and the contributor
// lane's registry/claim admission. They yield a WEAK needs-human opinion
// (stageRankRole / stageRankAdmission): another lane that admits the author
// owns the stage, and when the resolved tier is trusted the opinion is
// dropped altogether (hivecommons/hive#12403 was flipped to 090 by the
// trusted-author lane's role check whenever the contributor lane — which
// admitted the author from the registry — had no classified opinion).
var stageAdmissionReasons = map[string]int{
	"untrusted-author-role":      stageRankRole,
	contributorLaneReasonNoClaim: stageRankAdmission, contributorLaneReasonClaimAuthor: stageRankAdmission,
	contributorLaneReasonClaimOtherHive: stageRankAdmission, contributorLaneReasonNoLinkedIssue: stageRankAdmission,
	contributorLaneReasonClaimTier: stageRankAdmission, contributorLaneReasonClaimUnverified: stageRankAdmission,
	contributorLaneReasonAuthorAssociation: stageRankAdmission,
}

// stageNeedsHumanReasons are reasons that mean the hive will not merge this
// PR unattended and a person must act, whoever the author is.
var stageNeedsHumanReasons = map[string]bool{
	contributorLaneReasonWorkflowPath: true, contributorLaneReasonTestsRemoved: true,
	contributorLaneReasonNoReviewer: true, contributorLaneReasonReporterTrustHold: true, contributorLaneReasonSentinelBlocked: true,
	contributorLaneReasonNoAppBotLogin: true, contributorLaneReasonFilesEvidence: true,
	autoMergeReasonHumanMergePath: true, autoMergeReasonHumanMergePathEvidence: true,
	autoMergeReasonIntentUnauthorized: true, autoMergeReasonIntentEvidence: true,
	"approval-desk-withheld": true, "author-permission-missing": true, "fork-non-member": true,
	"held": true, "label:hold": true, "label:needs-human": true, "exempt-label": true, "sentinel": true,
	autoMergeReasonNoAppBotLogin: true, "self-authorization-hold-released": true,
}

// stageMergeQueueReasons are reasons reached only after every gate passed:
// the merge is imminent or waiting on GitHub.
var stageMergeQueueReasons = map[string]bool{
	"head-changed-since-eval": true, "merge-failed": true, "merge-not-applied": true,
	"required-checks-expected": true, "behind": true, "updated-branch": true, "update-branch": true,
}

// stageFixingReasons are reasons an agent (or the author) must fix the head.
var stageFixingReasons = map[string]bool{
	"changes-requested": true, contributorLaneReasonChangesRequested: true, "conflicting": true, "not-mergeable": true,
	"label:needs-rebase": true,
}

// fixingStage is the rework stage for the lane: a hive agent reworks its
// own (self-authored lane) PR, while a contributor's PR (trusted-author or
// contributor lane) is handed back to its author with the diagnosis.
func fixingStage(o stageObservation) stagelabels.Stage {
	if o.Lane == stageLaneSelf {
		return stagelabels.Fixing
	}
	return stagelabels.ReturnedToContributor
}

// classifyStage maps one lane observation to a stage opinion. Reason
// strings are the lane constants; see the maps above for the groups.
func classifyStage(o stageObservation) stageVerdict {
	reason := o.Reason
	fixing := fixingStage(o)
	if o.Merged && reason == "" {
		return stageVerdict{Terminal: true, Rank: stageRankOwned}
	}
	if o.Err {
		// The lane tripped before deciding. If every author-side gate is
		// already satisfied the PR is waiting on the hive, not the author.
		if name := o.Detail.redCheck(o.HeadSHA); name != "" {
			return stageVerdict{Stage: fixing, Rank: stageRankOwned, Reason: stageReasonOptionalCheckFailing + name}
		}
		if authorSideDone(o) {
			return stageVerdict{Stage: stagelabels.AwaitingMergeAdmission, Rank: stageRankRole}
		}
		return stageVerdict{}
	}
	switch reason {
	case "closed", "gone", "label-removed":
		return stageVerdict{Terminal: true, Rank: stageRankOwned}
	case "draft":
		// Not in the pipeline yet: no stage, but clear a stale one.
		return stageVerdict{Stage: stagelabels.None, Rank: stageRankOwned}
	}
	if stageHandoffReasons[reason] {
		return stageVerdict{}
	}
	if rank := stageAdmissionReasons[reason]; rank != stageRankNone {
		if hasLabelFold(o.Labels, "needs-rebase") {
			return stageVerdict{Stage: fixing, Rank: stageRankOwned}
		}
		return stageVerdict{Stage: stagelabels.NeedsHuman, Rank: rank}
	}
	if strings.HasPrefix(reason, "excluded-label:") || stageNeedsHumanReasons[reason] {
		if hasLabelFold(o.Labels, "needs-rebase") {
			return stageVerdict{Stage: fixing, Rank: stageRankOwned}
		}
		return stageVerdict{Stage: stagelabels.NeedsHuman, Rank: stageRankOwned}
	}
	if stageFixingReasons[reason] || strings.HasPrefix(reason, "label:") && strings.Contains(reason, "rebase") {
		return stageVerdict{Stage: fixing, Rank: stageRankOwned}
	}
	if verdictBlocks(o.Detail) {
		// The swarm asked for changes at this head; the lane's own reason
		// (CI pending, no-verdict, …) is not what the author has to act on.
		return stageVerdict{Stage: fixing, Rank: stageRankOwned, Reason: stageVerdictReason(o.Detail)}
	}
	if classifyCIReason(reason) == ciReasonRed {
		// A red required check is the lane's own diagnosis; it outranks a
		// red optional one in the recorded reason.
		return stageVerdict{Stage: fixing, Rank: stageRankOwned}
	}
	if name := o.Detail.redCheck(o.HeadSHA); name != "" {
		// The lane gates on required checks only, so it soaks (and merges)
		// past a red optional check and its reason reads as queued or
		// pending. A red check on the head is red CI to its author: the
		// repair stage, with the check named, never 080 or 100.
		return stageVerdict{Stage: fixing, Rank: stageRankOwned, Reason: stageReasonOptionalCheckFailing + name}
	}
	if strings.HasPrefix(reason, contributorLaneReasonSoaking) || stageMergeQueueReasons[reason] || strings.HasPrefix(reason, "release-hold") {
		return stageVerdict{Stage: stagelabels.MergeQueue, Rank: stageRankOwned}
	}
	if reason == contributorLaneReasonNoVerdict {
		return stageVerdict{Stage: stagelabels.ReviewPending, Rank: stageRankOwned}
	}
	if classifyCIReason(reason) == ciReasonPending {
		if o.Detail.VerdictKnown && o.Detail.Verdict.Approved {
			return stageVerdict{Stage: stagelabels.Reviewed, Rank: stageRankOwned}
		}
		return stageVerdict{Stage: stagelabels.ReviewPending, Rank: stageRankOwned}
	}
	if authorSideDone(o) {
		// Green CI and an approve verdict, yet the lane stopped for a reason
		// no stage claims: admission is pending on the hive's side.
		return stageVerdict{Stage: stagelabels.AwaitingMergeAdmission, Rank: stageRankRole}
	}
	return stageVerdict{}
}

// authorSideDone reports whether the PR has nothing left for its author to
// do: the reviewer verdict for this head is approve and the lane's reason
// is not a CI state.
func authorSideDone(o stageObservation) bool {
	return o.Detail.VerdictKnown && o.Detail.Verdict.Approved && !verdictBlocks(o.Detail) && classifyCIReason(o.Reason) == ciReasonNone
}

func hasLabelFold(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

// resolveStage picks the owning opinion among a PR's lane observations
// (already sorted by lane precedence) and the author tier to show.
func resolveStage(list []stageObservation) (stageVerdict, stagelabels.Author, stageObservation) {
	var best stageVerdict
	var bestObs stageObservation
	author := stagelabels.AuthorNone
	for _, o := range list {
		v := classifyStage(o)
		if v.Rank > best.Rank {
			best, bestObs = v, o
		}
		// The most specific author tier wins: the contributor lane knows
		// registry/claim tiers the trusted-author lane cannot see, so a
		// trusted-author "trusted" is kept but its absence is refined.
		author = author.Merge(o.Detail.Author)
	}
	if best.Rank < stageRankOwned {
		switch {
		case author == stagelabels.AuthorNone:
			author = stagelabels.AuthorUntrusted
		case best.Stage == stagelabels.NeedsHuman && author == stagelabels.AuthorTrusted:
			// Some lane admits this author; a lane that cannot vouch for
			// them has nothing to say about the stage. Leave it as is.
			best = stageVerdict{}
		}
	}
	if best.Rank == stageRankNone {
		bestObs = list[0]
	}
	return best, author, bestObs
}

// StageLabelResult summarises one ApplyStageLabels pass.
type StageLabelResult struct {
	Items    int
	Writes   int
	Comments int
	Skipped  int
	// Healed counts PRs found carrying more than one stage label.
	Healed int
}

// ApplyStageLabels reconciles the stage/author/health labels for every PR
// observed since the last pass and persists stage timing. It is the single
// write point for the families; lanes only record.
func (c *Engine) ApplyStageLabels(ctx context.Context) (StageLabelResult, error) {
	var result StageLabelResult
	policy := c.stagePolicy()
	tracker := c.stageTrackerOrShared()
	if !policy.Enabled {
		tracker.drain()
		return result, nil
	}
	if !c.ready() {
		return result, hgithub.ErrNoGitHubClient
	}
	store := c.stageStoreOrShared()
	ctx = hgithub.WithRESTCaller(ctx, stageRESTCaller)
	now := c.now()
	var firstErr error
	for _, list := range tracker.drain() {
		if len(list) == 0 {
			continue
		}
		result.Items++
		verdict, author, obs := resolveStage(list)
		key := stageKey(obs.Owner, obs.Repo, obs.Number)
		if verdict.Rank == stageRankNone {
			result.Skipped++
			continue
		}
		if verdict.Reason != "" {
			obs.Reason = verdict.Reason
		}
		var want stagelabels.Desired
		var tr stagelabels.Transition
		prevRec, hadPrev := store.Get(key)
		if verdict.Terminal {
			store.Forget(key)
			c.resolveNudge(ctx, prevRec, hadPrev, obs)
		} else {
			if verdict.Stage == stagelabels.ReturnedToContributor && hadPrev && prevRec.Adopted(obs.HeadSHA) {
				// Adopted at this head: the lane still sees a red
				// contributor PR, but the hive owns it now.
				verdict.Stage = stagelabels.AdoptedByHive
			}
			tr = store.Observe(key, verdict.Stage, obs.Reason)
			// hivecommons/hive#12502: remember the author so the dashboard can
			// list "PRs returned to this contributor" from the store alone.
			store.NoteAuthor(key, obs.Author)
			if verdict.Stage != stagelabels.ReturnedToContributor {
				c.resolveNudge(ctx, prevRec, hadPrev, obs)
			}
			rec, _ := store.Get(key)
			want = stagelabels.Desired{
				Stage:     verdict.Stage,
				Author:    author,
				Stalled:   stagelabels.Stalled(rec, policy.Dwell, now),
				Regressed: rec.Regressed(),
			}
		}
		if !verdict.Terminal {
			n, err := c.stripStrayOwnershipLabels(ctx, obs, policy)
			result.Writes += n
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		add, remove := stagelabels.Plan(obs.Labels, want)
		if len(add)+len(remove) > 0 {
			if err := c.ensureStageLabels(ctx, obs.Owner, obs.Repo, tracker); err != nil {
				c.warn("stage labels: provisioning failed", "repo", obs.Display, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
		}
		var out stagelabels.Outcome
		if len(add)+len(remove) > 0 {
			// Enforce re-reads the PR so a stage another writer placed since
			// the lane's snapshot is replaced, never joined (hivecommons/hive#12512).
			var err error
			out, err = stagelabels.Enforce(ctx, engineStageLabeler{c.gh}, obs.Owner, obs.Repo, obs.Number, obs.Labels, want)
			if err != nil {
				c.warn("stage labels: write failed", "repo", obs.Display, "pr", obs.Number, "stage", want.Stage.String(), "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
			result.Writes += len(out.Removed)
			if len(out.Added) > 0 {
				result.Writes++
			}
			if out.Healed {
				result.Healed++
			}
		}
		if out.Changed() {
			c.info("stage labels reconciled", "repo", obs.Display, "pr", obs.Number, "stage", want.Stage.String(), "author", author.String(), "added", out.Added, "removed", out.Removed, "healed", out.Healed, "reason", obs.Reason, "lane", obs.Lane)
		}
		if want.Stalled && want.Stage.StallNote() {
			if rec, ok := store.Get(key); ok && !rec.StallCommented {
				body := stagelabels.StallComment(want.Stage, stagelabels.Dwell(want.Stage, policy.Dwell), obs.Reason)
				if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
					c.warn("stage labels: stall note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
					if firstErr == nil {
						firstErr = err
					}
				} else {
					store.MarkStallCommented(key)
					result.Comments++
				}
			}
		}
		if want.Stage == stagelabels.ReturnedToContributor && policy.ContributorNudge {
			n, err := c.nudgeContributor(ctx, store, key, obs, policy, now)
			result.Comments += n
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if want.Stage == stagelabels.ReturnedToContributor && policy.Adoption {
			n, err := c.adoptContributorPR(ctx, store, key, obs, policy, now)
			result.Comments += n
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if want.Stage == stagelabels.AdoptedByHive {
			n, err := c.settleAdoption(ctx, store, key, obs, now)
			result.Comments += n
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if tr.Regressed {
			// Keyed by head SHA: a gate flapping between two stages on one
			// head (e.g. min_head_age) must not post every sweep (#12557).
			if rec, ok := store.Get(key); ok && rec.RegressionCommentDue(obs.HeadSHA) {
				body := stagelabels.RegressionComment(tr.Prev, tr.Next, obs.Reason)
				if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
					c.warn("stage labels: regression comment failed", "repo", obs.Display, "pr", obs.Number, "error", err)
					if firstErr == nil {
						firstErr = err
					}
				} else {
					store.MarkRegressionCommentedForHead(key, obs.HeadSHA)
					result.Comments++
				}
			}
		}
	}
	store.Prune(stageStorePruneAge)
	if err := store.Save(); err != nil {
		c.warn("stage labels: persisting state failed", "error", err)
		if firstErr == nil {
			firstErr = err
		}
	}
	return result, firstErr
}

// ensureStageLabels provisions (or recolours) the family's labels in a repo
// once per process: one list call, then a create or patch per label that is
// missing or whose colour drifted from the table.
func (c *Engine) ensureStageLabels(ctx context.Context, owner, repo string, tracker *StageTracker) error {
	full := strings.ToLower(owner + "/" + repo)
	tracker.mu.Lock()
	done := tracker.ensured[full]
	tracker.mu.Unlock()
	if done {
		return nil
	}
	existing := map[string]*gh.Label{}
	opts := &gh.ListOptions{PerPage: 100}
	for {
		labels, resp, err := c.gh.Issues.ListLabels(ctx, owner, repo, opts)
		if err != nil {
			return fmt.Errorf("listing labels for %s: %w", full, err)
		}
		for _, l := range labels {
			if l != nil {
				existing[strings.ToLower(l.GetName())] = l
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	for _, def := range stagelabels.AllLabelDefs() {
		cur, ok := existing[strings.ToLower(def.Name)]
		switch {
		case !ok:
			_, _, err := c.gh.Issues.CreateLabel(ctx, owner, repo, &gh.Label{Name: gh.Ptr(def.Name), Color: gh.Ptr(def.Color), Description: gh.Ptr(def.Description)})
			if err != nil && !isGitHubStatus(err, http.StatusUnprocessableEntity) {
				return fmt.Errorf("creating label %s in %s: %w", def.Name, full, err)
			}
		case !strings.EqualFold(cur.GetColor(), def.Color):
			_, _, err := c.gh.Issues.EditLabel(ctx, owner, repo, cur.GetName(), &gh.Label{Name: gh.Ptr(def.Name), Color: gh.Ptr(def.Color), Description: gh.Ptr(def.Description)})
			if err != nil {
				return fmt.Errorf("recolouring label %s in %s: %w", def.Name, full, err)
			}
		}
	}
	tracker.mu.Lock()
	tracker.ensured[full] = true
	tracker.mu.Unlock()
	return nil
}

// ApplyStageLabels runs the reconcile pass on a one-shot engine. The
// tracker and store are process-shared, so observations recorded by the
// lanes' own one-shot engines earlier in the cycle are what it applies.
func ApplyStageLabels(ctx context.Context, transport Transport, opts Options) (StageLabelResult, error) {
	return New(transport, opts).ApplyStageLabels(ctx)
}

// engineStageLabeler adapts the engine's go-github client to
// stagelabels.Labeler.
type engineStageLabeler struct{ gh *gh.Client }

func (l engineStageLabeler) Labels(ctx context.Context, owner, repo string, number int) ([]string, error) {
	issue, _, err := l.gh.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(issue.Labels))
	for _, lab := range issue.Labels {
		if lab != nil && lab.GetName() != "" {
			out = append(out, lab.GetName())
		}
	}
	return out, nil
}

func (l engineStageLabeler) AddLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	_, _, err := l.gh.Issues.AddLabelsToIssue(ctx, owner, repo, number, labels)
	return err
}

func (l engineStageLabeler) RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error {
	_, err := l.gh.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(label))
	if isGitHubStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}
