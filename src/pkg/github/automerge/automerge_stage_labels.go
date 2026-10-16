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
}

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
	obs.Author = hgithub.SafeGetLogin(pr.GetUser())
	obs.Title = pr.GetTitle()
	obs.URL = pr.GetHTMLURL()
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

// stageVerdict is a lane's opinion about a PR's stage.
type stageVerdict struct {
	Stage stagelabels.Stage
	// Rank orders competing opinions: 0 none (hand-off, transient error),
	// 1 weak (the trusted-author lane's "not a trusted author", which the
	// contributor lane refines), 2 owned.
	Rank int
	// Terminal means the PR left the pipeline (merged/closed/gone): remove
	// every hive-owned label and forget its timing.
	Terminal bool
}

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
		strings.HasPrefix(reason, "required-check-pending:"):
		return ciReasonPending
	case strings.HasPrefix(reason, "required-check-failing:"), strings.HasPrefix(reason, "check-failing:"),
		strings.HasPrefix(reason, "status-"), strings.HasPrefix(reason, "check-"), reason == "check-runs":
		return ciReasonRed
	}
	return ciReasonNone
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

// stageNeedsHumanReasons are reasons that mean the hive will not merge this
// PR unattended and a person must act.
var stageNeedsHumanReasons = map[string]bool{
	contributorLaneReasonNoClaim: true, contributorLaneReasonClaimAuthor: true, contributorLaneReasonClaimOtherHive: true,
	contributorLaneReasonNoLinkedIssue: true, contributorLaneReasonClaimTier: true, contributorLaneReasonClaimUnverified: true,
	contributorLaneReasonAuthorAssociation: true, contributorLaneReasonWorkflowPath: true, contributorLaneReasonTestsRemoved: true,
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
		return stageVerdict{Terminal: true, Rank: 2}
	}
	if o.Err {
		// The lane tripped before deciding. If every author-side gate is
		// already satisfied the PR is waiting on the hive, not the author.
		if authorSideDone(o) {
			return stageVerdict{Stage: stagelabels.AwaitingMergeAdmission, Rank: 1}
		}
		return stageVerdict{}
	}
	switch reason {
	case "closed", "gone", "label-removed":
		return stageVerdict{Terminal: true, Rank: 2}
	case "draft":
		// Not in the pipeline yet: no stage, but clear a stale one.
		return stageVerdict{Stage: stagelabels.None, Rank: 2}
	case "untrusted-author-role":
		return stageVerdict{Stage: stagelabels.NeedsHuman, Rank: 1}
	}
	if stageHandoffReasons[reason] {
		return stageVerdict{}
	}
	if strings.HasPrefix(reason, "excluded-label:") || stageNeedsHumanReasons[reason] {
		if hasLabelFold(o.Labels, "needs-rebase") {
			return stageVerdict{Stage: fixing, Rank: 2}
		}
		return stageVerdict{Stage: stagelabels.NeedsHuman, Rank: 2}
	}
	if stageFixingReasons[reason] || strings.HasPrefix(reason, "label:") && strings.Contains(reason, "rebase") || verdictBlocks(o.Detail) {
		return stageVerdict{Stage: fixing, Rank: 2}
	}
	if strings.HasPrefix(reason, contributorLaneReasonSoaking) || stageMergeQueueReasons[reason] || strings.HasPrefix(reason, "release-hold") {
		return stageVerdict{Stage: stagelabels.MergeQueue, Rank: 2}
	}
	if reason == contributorLaneReasonNoVerdict {
		return stageVerdict{Stage: stagelabels.ReviewPending, Rank: 2}
	}
	switch classifyCIReason(reason) {
	case ciReasonRed:
		return stageVerdict{Stage: fixing, Rank: 2}
	case ciReasonPending:
		if o.Detail.VerdictKnown && o.Detail.Verdict.Approved {
			return stageVerdict{Stage: stagelabels.Reviewed, Rank: 2}
		}
		return stageVerdict{Stage: stagelabels.ReviewPending, Rank: 2}
	}
	if authorSideDone(o) {
		// Green CI and an approve verdict, yet the lane stopped for a reason
		// no stage claims: admission is pending on the hive's side.
		return stageVerdict{Stage: stagelabels.AwaitingMergeAdmission, Rank: 1}
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
	if best.Rank == 1 && author == stagelabels.AuthorNone {
		author = stagelabels.AuthorUntrusted
	}
	if best.Rank == 0 {
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
		if verdict.Rank == 0 {
			result.Skipped++
			continue
		}
		var want stagelabels.Desired
		var tr stagelabels.Transition
		prevRec, hadPrev := store.Get(key)
		if verdict.Terminal {
			store.Forget(key)
			c.resolveNudge(ctx, prevRec, hadPrev, obs)
		} else {
			tr = store.Observe(key, verdict.Stage, obs.Reason)
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
		for _, l := range remove {
			if _, err := c.gh.Issues.RemoveLabelForIssue(ctx, obs.Owner, obs.Repo, obs.Number, url.PathEscape(l)); err != nil && !isGitHubStatus(err, http.StatusNotFound) {
				c.warn("stage labels: remove failed", "repo", obs.Display, "pr", obs.Number, "label", l, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			result.Writes++
		}
		if len(add) > 0 {
			if _, _, err := c.gh.Issues.AddLabelsToIssue(ctx, obs.Owner, obs.Repo, obs.Number, add); err != nil {
				c.warn("stage labels: add failed", "repo", obs.Display, "pr", obs.Number, "labels", add, "error", err)
				if firstErr == nil {
					firstErr = err
				}
			} else {
				result.Writes++
			}
		}
		if len(add)+len(remove) > 0 {
			c.info("stage labels reconciled", "repo", obs.Display, "pr", obs.Number, "stage", want.Stage.String(), "author", author.String(), "added", add, "removed", remove, "reason", obs.Reason, "lane", obs.Lane)
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
		if tr.Regressed {
			if rec, ok := store.Get(key); ok && !rec.RegressionCommented {
				body := stagelabels.RegressionComment(tr.Prev, tr.Next, obs.Reason)
				if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
					c.warn("stage labels: regression comment failed", "repo", obs.Display, "pr", obs.Number, "error", err)
					if firstErr == nil {
						firstErr = err
					}
				} else {
					store.MarkRegressionCommented(key)
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
