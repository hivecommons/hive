package automerge

// Stage labels (hivecommons/hive#12429). Each automerge lane already computes
// a per-PR reason every cycle; this file maps that reason (plus the review
// verdict, CI state and the author's admission tier the lane used) onto the
// hive-owned `stage/NN-*`, `role/*`, `author/*` and `stage/stalled|regressed` label
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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// StageLabelPolicy is the live `governor.labels.stage` policy.
type StageLabelPolicy struct {
	Enabled bool
	// Dwell overrides the stage table's expected dwell per stage slug.
	Dwell map[string]time.Duration
	// StalledRequireQuiet (governor.labels.stage.stalled.require_quiet,
	// default true) measures the dwell from the last outside activity
	// (head push, comment, review, CI change, label change) rather than
	// from entering the stage, so stage/stalled means "nobody is working
	// this" (hivecommons/hive#12534). Either way it is never applied while
	// the item waits on the hive (checks running, verdict pending).
	StalledRequireQuiet bool
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
	// AdoptAfterOffline is how long the author's relay must have been
	// offline after the nudge before a clanker PR is adopted (default 30m);
	// AdoptAfterUnregistered is the wait for a clanker author with no
	// contributor profile on this hive (default 2h). Both need a presence
	// source (ContributorPresenceSource); without one the graces above apply.
	AdoptAfterOffline      time.Duration
	AdoptAfterUnregistered time.Duration
	// AdoptInPlace allows adoption by pushing to the PR's own branch when
	// GitHub permits (same-repo head or maintainer_can_modify) instead of a
	// superseding App PR (governor.labels.stage.adoption.in_place, default on).
	AdoptInPlace bool
	// AdoptionStrictAttribution makes in-place adoption yield on a changed head
	// unless GitHub links the commit to the App bot (the stage store only
	// caches heads that already passed that check). It defaults on after the
	// App-linked git identity rollout, with a first-deploy safety valve in
	// settleInPlaceAdoption.
	AdoptionStrictAttribution bool
	// DCORequired resolves the per-repo `governor.labels.stage.dco.required`
	// override for "owner/repo"; ok false means "detect" (a DCO context in
	// the branch's required checks or on the head). nil = always detect.
	DCORequired func(display string) (required, ok bool)
	// HiveID is this hive's id; hive/<HiveID> on a PR the App did not author
	// is a stray the reconciler removes (hivecommons/hive#12545), and the
	// `<!-- hive:origin hive=<id> -->` marker naming it marks a PR as
	// author/hive (hivecommons/hive#12581).
	HiveID string
	// AuthorKindLabels are the configured `author/<kind>` names
	// (governor.labels.author); zero = defaults.
	AuthorKindLabels authorkind.Names
	// RelayKnown reports whether login has a contributor profile on this
	// hive (runs a relay that targets it) — authorkind's weakest signal for
	// author/clanker. nil = unknown.
	RelayKnown func(login string) bool
	// SharedCIIncident returns the open [shared-ci] incident the PR
	// (repo "owner/name", number, head SHA) is deferred to, or 0. A red PR
	// deferred to one is kept out of 070/072 and never nudged
	// (hivecommons/hive#12606). nil = unknown (no deferral).
	SharedCIIncident func(repo string, number int, head string) int
	// BaseBranchRed returns the failing checks on the PR head (repo
	// "owner/name", number, head SHA) that are also red on the base branch
	// tip, when EVERY failing check is — hgithub.PullRequest.BaseRedChecks
	// with AllFailuresBaseRed. Such a PR is kept out of 070/071/072 with
	// reason `base-branch-red:<checks>` and never nudged: the red is the
	// base branch's, not the author's. nil = unknown (no deferral).
	BaseBranchRed func(repo string, number int, head string) []string
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
	removed := 0
	for _, l := range stray {
		if err := c.removeNonStageLabel(ctx, obs.Owner, obs.Repo, obs.Number, l); err != nil {
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
	// HumanApprover is a trusted human whose APPROVED review exactly matches
	// HumanApprovalHead. It resolves a requires_human reviewer verdict without
	// overriding changes_requested/reject.
	HumanApprover     string
	HumanApprovalHead string
	Role              stagelabels.Role
	// RedChecks names the completed-red, non-required checks the CI gate
	// walked past on RedChecksHead (sorted). The lane ignores them by
	// design; the stage engine must not call the PR merge-queued or
	// awaiting admission while the rollup on its head shows a red check
	// (hivecommons/hive#12538, seen on #12457).
	RedChecks     []string
	RedChecksHead string
	// Comments is the PR's issue + review comment count from the deep
	// fetch (the list payload has none); CommentsKnown says it was seen.
	// OwnComments counts the comments this engine posted after that fetch
	// (creditOwnComment); a later fetch resets it.
	Comments      int
	CommentsKnown bool
	OwnComments   int
	// Reviews is the number of review submissions by people (the hive
	// app's excluded) from a lane's ListReviews walk; ReviewsKnown says it
	// was seen this cycle.
	Reviews      int
	ReviewsKnown bool
	// DCO, DCOTotal and DCOUnsigned are the stage pass's own commit check
	// for the head (hivecommons/hive#12542); filled by ApplyStageLabels,
	// not by the lanes.
	DCO                 stagelabels.DCOState
	DCOTotal            int
	DCOUnsigned         int
	DCOUnsignedHive     int
	DCOUnsignedOther    int
	DCOSquashUnsafeHive int
	// DCOGate / DCOGateHead record whether the CI gate saw a DCO context
	// on the head (required-check set or reported status/check run): the
	// detected "this repo enforces DCO" signal the 072 override needs when
	// no per-repo override is configured.
	DCOGate     bool
	DCOGateHead string
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
	// LaneReason is the lane's own skip reason before the reviewer verdict
	// or the human route replaced Reason; it is what still says whether CI
	// is red, pending or green on the head.
	LaneReason string
	Merged     bool
	Err        bool
	Detail     stageDetail
	// Author, Title, URL and OpenedAt feed the stage-072 contributor nudge.
	Author string
	// AuthorKind is who wrote the PR (authorkind.Detect), set on the
	// resolved observation by ApplyStageLabels before the nudge/adoption
	// hooks run: only an author/clanker PR has a hive to interrupt.
	AuthorKind authorkind.Kind
	Title      string
	URL        string
	OpenedAt   time.Time
	// BaseRef and Body feed the stage-074 adoption (superseding PR target
	// and the Closes lines to carry over); both come from the list payload.
	BaseRef string
	Body    string
	// HeadRepo is the head's owner/repo (differs from Display on a fork).
	HeadRepo string
	// Route is set by ApplyStageLabels when the lane handed the PR to a
	// human and the evidence routed it (feeds the entry comments and the
	// 072 nudge diagnosis).
	Route *stagelabels.Route
	// SharedCIIncident is the open shared-CI incident the head is deferred
	// to (StageLabelPolicy.SharedCIIncident), 0 when none.
	SharedCIIncident int
	// BaseRedChecks are the head's failing checks that are also red on the
	// base branch (StageLabelPolicy.BaseBranchRed); non-empty only when
	// every failing check is.
	BaseRedChecks []string
}

// StageTracker accumulates observations across the lanes of one cycle. One
// tracker is shared process-wide (sharedStageTracker) because the governor
// sweep builds a fresh engine per lane.
type StageTracker struct {
	mu      sync.Mutex
	obs     map[string]map[string]stageObservation // key → lane → observation
	details map[string]stageDetail                 // key → detail noted by trySweep*
	ensured map[string]bool                        // repo → labels provisioned this process
	checks  map[string]humanChecksEntry            // key → head check digest (human routing)
}

// NewStageTracker returns an empty tracker.
func NewStageTracker() *StageTracker {
	return &StageTracker{obs: map[string]map[string]stageObservation{}, details: map[string]stageDetail{}, ensured: map[string]bool{}, checks: map[string]humanChecksEntry{}}
}

var sharedStageTracker = NewStageTracker()

func stageKey(owner, repo string, number int) string {
	return stagelabels.Key(owner+"/"+repo, number)
}

func (t *StageTracker) note(key string, fn func(*stageDetail)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := normalizeStageDetail(t.details[key])
	fn(&d)
	t.details[key] = normalizeStageDetail(d)
}

// creditOwnComment records one hive comment posted after the comment count
// a lane fetched for key this cycle, and reports whether there was such a
// fetch. The fetch preceded the comment, so without the credit
// ApplyStageLabels would store the pre-comment count and the next cycle's
// fetch would read the hive's own comment as a person acting.
func (t *StageTracker) creditOwnComment(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	d, ok := t.details[key]
	if !ok || !d.CommentsKnown {
		return false
	}
	d.OwnComments++
	t.details[key] = d
	return true
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
		detail := normalizeStageDetail(t.details[k])
		list := make([]stageObservation, 0, len(lanes))
		for _, o := range lanes {
			o.Detail = normalizeStageDetail(o.Detail)
			// Details noted by any lane's deep evaluation (verdict, author
			// tier) apply to every lane's view of the PR.
			if detail.VerdictKnown {
				o.Detail.Verdict, o.Detail.VerdictKnown = detail.Verdict, true
			}
			if detail.HumanApprover != "" && detail.HumanApprovalHead != "" && detail.HumanApprovalHead == o.HeadSHA {
				o.Detail.HumanApprover, o.Detail.HumanApprovalHead = detail.HumanApprover, detail.HumanApprovalHead
			}
			if detail.Role != stagelabels.RoleNone {
				o.Detail.Role = detail.Role
			}
			if detail.RedChecksHead == "" || o.HeadSHA == "" || detail.RedChecksHead == o.HeadSHA {
				o.Detail.RedChecks, o.Detail.RedChecksHead = detail.RedChecks, detail.RedChecksHead
			}
			if detail.CommentsKnown {
				o.Detail.Comments, o.Detail.CommentsKnown, o.Detail.OwnComments = detail.Comments, true, detail.OwnComments
			}
			if detail.ReviewsKnown {
				o.Detail.Reviews, o.Detail.ReviewsKnown = detail.Reviews, true
			}
			if detail.DCOGateHead != "" && (o.HeadSHA == "" || detail.DCOGateHead == o.HeadSHA) {
				o.Detail.DCOGate, o.Detail.DCOGateHead = detail.DCOGate, detail.DCOGateHead
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

// stageReconcilerTransport is the transport that owns the process's
// stageengine.StageWriter (hgithub.Client). The reconciler is built over
// it so every stage write the engine makes goes through that one writer.
type stageReconcilerTransport interface {
	StageReconciler(store *stagelabels.Store, opts *stageengine.Options, logger *slog.Logger) (stageengine.Reconciler, *stageengine.LegacyTable)
}

// ErrNoStageWriter is returned when the transport cannot supply the stage
// writer the reconciler needs; the sweep writes nothing rather than
// guess.
var ErrNoStageWriter = errors.New("automerge: transport provides no stage writer")

// stageReconciler returns the reconciler for this pass and the table the
// pass fills with its derivation (see Options.StageEngine).
func (c *Engine) stageReconciler(store *stagelabels.Store) (stageengine.Reconciler, *stageengine.LegacyTable, error) {
	t, ok := c.transport.(stageReconcilerTransport)
	if !ok {
		return nil, nil, ErrNoStageWriter
	}
	r, legacy := t.StageReconciler(store, c.stageEngine, c.logger)
	if r == nil || legacy == nil {
		return nil, nil, ErrNoStageWriter
	}
	return r, legacy, nil
}

// prStagePending is one PR the pass derived a stage for, waiting on the
// reconciler's write before its post-write work (stall note, hand-off
// entry, 07x nudge/adoption, regression note) runs.
type prStagePending struct {
	obs   stageObservation
	key   string
	role  stagelabels.Role
	want  stagelabels.Desired
	tr    stagelabels.Transition
	facts stageengine.Facts
}

// removeNonStageLabel takes one non-hive label off a PR (the stray
// agent/* and hive/<id> ownership labels). Stage labels never come
// through here; those are the StageWriter's alone.
func (c *Engine) removeNonStageLabel(ctx context.Context, owner, repo string, number int, label string) error {
	_, err := c.gh.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(label))
	if isGitHubStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// recordStage is called by every lane for every PR it listed, with the
// reason it settled on (""+merged for a merge). It is a no-op when the
// policy is off so lanes never pay for it.
func (c *Engine) recordStage(lane, displayRepo, owner, repo string, pr *gh.PullRequest, reason string, merged bool, err error) {
	policy := c.stagePolicy()
	if !policy.Enabled || pr == nil || pr.GetNumber() <= 0 {
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
	obs.SharedCIIncident = stageSharedCIIncident(policy, owner, repo, obs.Number, obs.HeadSHA)
	obs.BaseRedChecks = stageBaseBranchRed(policy, owner, repo, obs.Number, obs.HeadSHA)
	c.stageTrackerOrShared().record(obs)
}

// noteStageVerdict records the reviewer verdict a lane resolved for the PR
// head, so the stage can tell review-pending from reviewed even when the
// lane's own reason stopped at CI.
func (c *Engine) noteStageVerdict(owner, repo string, number int, v ContributorLaneVerdict) {
	v = normalizeContributorLaneVerdict(v)
	if !c.stagePolicy().Enabled {
		return
	}
	c.stageTrackerOrShared().note(stageKey(owner, repo, number), func(d *stageDetail) {
		d.Verdict, d.VerdictKnown = v, true
	})
}

// noteStageTrustedHumanApproval records that a trusted human approved the
// exact head. Stage derivation uses it only to clear requires_human; rejection
// verdicts still win.
func (c *Engine) noteStageTrustedHumanApproval(owner, repo string, number int, head, login string) {
	if !c.stagePolicy().Enabled || strings.TrimSpace(login) == "" || strings.TrimSpace(head) == "" {
		return
	}
	c.stageTrackerOrShared().note(stageKey(owner, repo, number), func(d *stageDetail) {
		d.HumanApprover, d.HumanApprovalHead = strings.TrimSpace(login), strings.TrimSpace(head)
	})
}

// noteStageRole records the author's admission tier as the lane computed
// it. Lanes disagree by design (the contributor lane reports "untrusted"
// for a maintainer with no registry profile), so tiers merge by precedence
// rather than last-writer-wins (#12490).
func (c *Engine) noteStageRole(owner, repo string, number int, r stagelabels.Role) {
	if !c.stagePolicy().Enabled || r == stagelabels.RoleNone {
		return
	}
	c.stageTrackerOrShared().note(stageKey(owner, repo, number), func(d *stageDetail) { d.Role = d.Role.Merge(r) })
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

// noteStagePR records what a lane's deep PullRequests.Get learned that the
// list payload lacks: the comment count, which feeds the quiet clock behind
// stage/stalled. Neither payload carries a commit or comment timestamp, so
// activity is dated by the cycle that first observes it (Activity.At zero).
func (c *Engine) noteStagePR(owner, repo string, number int, pr *gh.PullRequest) {
	if !c.stagePolicy().Enabled || pr == nil {
		return
	}
	comments := pr.GetComments() + pr.GetReviewComments()
	c.stageTrackerOrShared().note(stageKey(owner, repo, number), func(d *stageDetail) {
		// A fresh fetch already includes any comment credited before it.
		d.Comments, d.CommentsKnown, d.OwnComments = comments, true, 0
	})
}

// noteStageReviews records the review submissions a lane's ListReviews walk
// saw on the PR, minus the hive app's own (queue approvals, reviewer
// verdicts). A body-only Approve or Request-changes from a maintainer adds
// nothing to the comment counts, so this is how it reaches the quiet clock.
func (c *Engine) noteStageReviews(owner, repo string, number int, reviews []*gh.PullRequestReview) {
	if !c.stagePolicy().Enabled {
		return
	}
	n := 0
	for _, r := range reviews {
		if r != nil && !c.isHiveAppReviewAuthor(r) {
			n++
		}
	}
	c.stageTrackerOrShared().note(stageKey(owner, repo, number), func(d *stageDetail) {
		d.Reviews, d.ReviewsKnown = n, true
	})
}

// noteOwnStageComment records that this engine posted a comment on the PR
// outside ApplyStageLabels (takeover notice, rebase request, stale-approval
// or hold-release note), so the next observed comment count does not read
// as outside activity and restart the quiet clock. When a lane already
// fetched the PR this cycle the credit goes on that observation (the fetch
// preceded the comment, and ApplyStageLabels stores the observed count as
// is); otherwise it goes on the store's last count. Comments from other
// hive processes (the reviewer swarm's review comments) are not visible
// here and still count as activity: the stall is then later, never earlier.
func (c *Engine) noteOwnStageComment(owner, repo string, number int) {
	if !c.stagePolicy().Enabled {
		return
	}
	key := stageKey(owner, repo, number)
	if c.stageTrackerOrShared().creditOwnComment(key) {
		return
	}
	c.stageStoreOrShared().CountOwnComments(key, 1)
}

// stageActivity derives this cycle's outside-activity view of a PR.
func stageActivity(obs stageObservation, stage stagelabels.Stage) stagelabels.Activity {
	a := stagelabels.Activity{Head: obs.HeadSHA, Comments: -1, Reviews: -1}
	if obs.Detail.CommentsKnown {
		a.Comments, a.OwnComments = obs.Detail.Comments, obs.Detail.OwnComments
	}
	if obs.Detail.ReviewsKnown {
		a.Reviews = obs.Detail.Reviews
	}
	verdict := "verdict:?"
	if obs.Detail.VerdictKnown {
		verdict = "verdict:" + obs.Detail.Verdict.Verdict
	}
	ci := classifyCIReason(obs.Reason)
	labels := make([]string, 0, len(obs.Labels))
	for _, l := range obs.Labels {
		if stagelabels.Owned(l) || strings.HasPrefix(strings.ToLower(l), "hive/") || strings.HasPrefix(strings.ToLower(l), hgithub.ReviewPriorityLabelPrefix) {
			continue
		}
		labels = append(labels, strings.ToLower(l))
	}
	sort.Strings(labels)
	a.Digest = verdict + "|ci:" + ci.String() + "|" + strings.Join(labels, ",")
	// Checks still running, or a review-pending head with no reviewer
	// verdict yet, is waiting on the hive, not on a person.
	a.WaitingOnHive = ci == ciReasonPending || stageReasonWaitsOnHive(obs.Reason) ||
		(stage == stagelabels.ReviewPending && !(obs.Detail.VerdictKnown && obs.Detail.Verdict.Verdict != ""))
	return a
}

// noteStageDCOGate records whether the CI gate saw a DCO context for head
// (hivecommons/hive#12542): in the required-check set or reported on the
// head by the DCO App / Prow's plugin. It is the detected "this repo
// enforces DCO" signal; a head without one is not routed to 072 for a
// missing sign-off unless config says the repo requires it.
func (c *Engine) noteStageDCOGate(owner, repo string, number int, head string, required map[string]bool, observed map[string]bool) {
	if !c.stagePolicy().Enabled || number <= 0 {
		return
	}
	gate := false
	for _, set := range []map[string]bool{required, observed} {
		for name := range set {
			if hgithub.IsDCOContext(name) {
				gate = true
			}
		}
	}
	c.stageTrackerOrShared().note(stageKey(owner, repo, number), func(d *stageDetail) {
		d.DCOGate, d.DCOGateHead = gate, head
	})
}

// dcoRequired reports whether a missing sign-off may route obs to 072: the
// per-repo override when configured, else the detected gate for this head.
func dcoRequired(policy StageLabelPolicy, obs stageObservation) bool {
	if policy.DCORequired != nil {
		if required, ok := policy.DCORequired(obs.Display); ok {
			return required
		}
	}
	return obs.Detail.DCOGate && (obs.Detail.DCOGateHead == "" || obs.HeadSHA == "" || obs.Detail.DCOGateHead == obs.HeadSHA)
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
	// Human marks a hand-off to a person (085/090 candidate) that
	// ApplyStageLabels refines against the head's evidence
	// (stagelabels.RouteHuman) before it is written.
	Human bool
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

// String is the stable name persisted in Record.ActivityDigest
// (/data/stage-labels.json); it must not follow the iota, or an upgrade
// that renumbers the enum would read every PR as active and reset its
// quiet clock.
func (k ciReasonKind) String() string {
	switch k {
	case ciReasonPending:
		return "pending"
	case ciReasonRed:
		return "red"
	}
	return "none"
}

func classifyCIReason(reason string) ciReasonKind {
	switch {
	case reason == "":
		return ciReasonNone
	case hgithub.CommitCIReasonIsPending(reason), reason == "ci-unverified", reason == "check-pending",
		strings.HasPrefix(reason, "required-check-pending:"), ciReasonInconclusive(reason):
		return ciReasonPending
	case strings.HasPrefix(reason, "required-check-failing:"), strings.HasPrefix(reason, "check-failing:"),
		strings.HasPrefix(reason, "status-"), strings.HasPrefix(reason, "check-"), reason == "check-runs",
		strings.HasPrefix(reason, stageReasonBaseBranchRed):
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

// stageReasonVerdictRequiresHuman is stageVerdictReason for requires_human.
const stageReasonVerdictRequiresHuman = "reviewer-verdict:requires_human"

const stageReasonDecisionRequested = "contributor-lane:decision-requested"

func normalizeStageDetail(d stageDetail) stageDetail {
	if d.VerdictKnown {
		d.Verdict = normalizeContributorLaneVerdict(d.Verdict)
	}
	return d
}

// stageVerdictReason is the recorded reason when the reviewer verdict, not
// the lane's skip reason, decided the stage.
func stageVerdictReason(d stageDetail) string {
	d = normalizeStageDetail(d)
	if d.Verdict.DecisionRequested {
		return stageReasonDecisionRequested
	}
	return "reviewer-verdict:" + strings.ToLower(strings.TrimSpace(d.Verdict.Verdict))
}

// verdictBlocks reports whether a known reviewer verdict asks for changes.
func verdictBlocks(d stageDetail) bool {
	d = normalizeStageDetail(d)
	if !d.VerdictKnown || d.Verdict.Approved {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(d.Verdict.Verdict))
	return v == "changes_requested" || v == "reject" || v == "rejected" || v == "request_changes"
}

// verdictNeedsHuman reports whether a known reviewer verdict defers the PR to
// a human rather than approving or asking for changes. Only requires_human
// qualifies: it is the one such verdict hive-review can emit
// (approve / changes_requested / requires_human / reject), and the 085 reason
// documented for operators is reviewer-verdict:requires_human.
func verdictNeedsHuman(d stageDetail) bool {
	d = normalizeStageDetail(d)
	if !d.VerdictKnown || d.Verdict.Approved {
		return false
	}
	if d.Verdict.DecisionRequested {
		return true
	}
	return strings.ToLower(strings.TrimSpace(d.Verdict.Verdict)) == "requires_human"
}

func trustedHumanApprovedRequiresHuman(d stageDetail, head string) bool {
	d = normalizeStageDetail(d)
	return verdictNeedsHuman(d) && strings.TrimSpace(d.HumanApprover) != "" && strings.TrimSpace(d.HumanApprovalHead) != "" && strings.TrimSpace(d.HumanApprovalHead) == strings.TrimSpace(head)
}

func withHumanApprovalReason(v stageVerdict, reason string) stageVerdict {
	if reason != "" && v.Rank != stageRankNone && !v.Terminal && v.Reason == "" {
		v.Reason = reason
	}
	return v
}

func stageHumanApprovalReason(d stageDetail, head string) string {
	if !trustedHumanApprovedRequiresHuman(d, head) {
		return ""
	}
	return stageReasonVerdictRequiresHuman + ":human-approved@" + strings.TrimSpace(d.HumanApprover)
}

// stageHandoffReasons are reasons that mean "not this lane's PR".
var stageHandoffReasons = map[string]bool{
	"not-app-authored": true, trustedBotLaneSkipReason: true, "repo-auto-merge-disabled": true,
	contributorLaneReasonAppAuthored: true, contributorLaneReasonNoAuthor: true, "missing-head-sha": true,
	"fetch-pr": true, "fetch-pr-recheck": true,
}

// stageAdmissionReasons are reasons that mean "this lane cannot vouch for
// the author": the trusted-author lane's role check and the contributor
// lane's registry/claim admission. They yield a WEAK opinion
// (stageRankRole / stageRankAdmission): another lane that admits the author
// owns the stage. The opinion is normally a 090 candidate, which is dropped
// altogether when the resolved tier is trusted (hivecommons/hive#12403 was
// flipped to 090 by the trusted-author lane's role check whenever the
// contributor lane — which admitted the author from the registry — had no
// classified opinion). When the reviewer verdict for the head is already
// requires_human the weak opinion is 085 instead, and that one is kept
// whatever the tier: a person owes a decision, not a merge click
// (hivecommons/hive#13115).
var stageAdmissionReasons = map[string]int{
	"untrusted-author-role":      stageRankRole,
	contributorLaneReasonNoClaim: stageRankAdmission, contributorLaneReasonClaimAuthor: stageRankAdmission,
	contributorLaneReasonClaimOtherHive: stageRankAdmission, contributorLaneReasonNoLinkedIssue: stageRankAdmission,
	contributorLaneReasonClaimTier: stageRankAdmission, contributorLaneReasonClaimUnverified: stageRankAdmission,
	contributorLaneReasonAuthorAssociation: stageRankAdmission,
}

// stageNeedsHumanReasons are reasons that mean the hive will not merge this
// PR unattended whoever the author is: a person owes a DECISION (stage
// 085), not merely a merge click.
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

// stageMergeQueueReasons are reasons reached only after every gate passed,
// soak included: the merge is imminent or waiting on GitHub. The soaking
// reason itself is 095 pre-merge-soak (classifyStage).
var stageMergeQueueReasons = map[string]bool{
	"head-changed-since-eval": true, "merge-failed": true, "merge-not-applied": true,
	"required-checks-expected": true, "behind": true, "updated-branch": true, "update-branch": true,
	autoMergeReasonBehindBudgetExhausted: true,
}

// stageFixingReasons are reasons an agent (or the author) must fix the head.
var stageFixingReasons = map[string]bool{
	"changes-requested": true, autoMergeReasonHumanChangesRequested: true, contributorLaneReasonChangesRequested: true, "conflicting": true, "not-mergeable": true,
	"label:needs-rebase": true,
}

// fixingStage is the rework stage for the lane: a hive agent reworks its
// own (self-authored lane) PR at 070, while a contributor's PR (trusted-
// author or contributor lane) is handed back to its author with the
// diagnosis — 072 for a clanker, 071 for a human. Before the author kind
// is known (resolveStage runs on raw observations) a non-self PR reads as
// 071; the apply pass re-derives once stageAuthorKind has run.
func fixingStage(o stageObservation) stagelabels.Stage {
	return stagelabels.FixingStageFor(o.Lane == stageLaneSelf, o.AuthorKind)
}

// classifyStage maps one lane observation to a stage opinion. Reason
// strings are the lane constants; see the maps above for the groups.
func classifyStage(o stageObservation) stageVerdict {
	o.Detail = normalizeStageDetail(o.Detail)
	reason := o.Reason
	fixing := fixingStage(o)
	humanApprovalReason := stageHumanApprovalReason(o.Detail, o.HeadSHA)
	if o.Merged && reason == "" {
		return stageVerdict{Terminal: true, Rank: stageRankOwned}
	}
	if o.Err {
		// The lane tripped before deciding. If every author-side gate is
		// already satisfied the PR is waiting on the hive, not the author.
		if v, ok := sharedCIDeferredStage(o); ok {
			return v
		}
		if v, ok := baseBranchRedStage(o); ok {
			return v
		}
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
		if v, ok := requiresHumanStage(o, rank); ok {
			// The reviewer already asked for a person: a 090 candidate would
			// be dropped for the non-approve verdict and fall to 050, where
			// the quiet dwell stalls it (hivecommons/hive#13115). The opinion
			// stays as weak as the admission reason that carries it, so a
			// lane that evaluated the head (conflict, red check) still owns.
			return v
		}
		// Trust is the only thing this lane holds against the PR: a 090
		// candidate, refined by ApplyStageLabels against the head's evidence.
		return withHumanApprovalReason(stageVerdict{Stage: stagelabels.ReadyForHumanMerge, Rank: rank, Human: true}, humanApprovalReason)
	}
	if strings.HasPrefix(reason, workflowApprovalRequiredPrefix) {
		return stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Human: true}
	}
	if strings.HasPrefix(reason, "excluded-label:") || stageNeedsHumanReasons[reason] {
		if hasLabelFold(o.Labels, "needs-rebase") {
			return stageVerdict{Stage: fixing, Rank: stageRankOwned}
		}
		return stageVerdict{Stage: stagelabels.NeedsDecision, Rank: stageRankOwned, Human: true}
	}
	if stageFixingReasons[reason] || strings.HasPrefix(reason, "label:") && strings.Contains(reason, "rebase") {
		return stageVerdict{Stage: fixing, Rank: stageRankOwned}
	}
	if verdictBlocks(o.Detail) {
		// The swarm asked for changes at this head; the lane's own reason
		// (CI pending, no-verdict, …) is not what the author has to act on.
		return stageVerdict{Stage: fixing, Rank: stageRankOwned, Reason: stageVerdictReason(o.Detail)}
	}
	if v, ok := requiresHumanStage(o, stageRankOwned); ok {
		// The reviewer DID answer — requires_human / needs-decision — but
		// the lane folds every non-approve verdict into non-mergeable reasons
		// such as no-verdict or pending CI. Hand it to a person instead of
		// leaving it in review-pending while the reviewer skips it as already
		// reviewed (hive #12417/#12438).
		return v
	}
	if v, ok := sharedCIDeferredStage(o); ok {
		// The red is the shared incident's, not the head's.
		return v
	}
	if v, ok := baseBranchRedStage(o); ok {
		// The red is the base branch's, not the head's.
		return v
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
	if strings.HasPrefix(reason, contributorLaneReasonSoaking) {
		// The lane admitted the head and is sitting out the soak window:
		// 095, not 100 — the merge has not been attempted yet.
		return withHumanApprovalReason(stageVerdict{Stage: stagelabels.PreMergeSoak, Rank: stageRankOwned}, humanApprovalReason)
	}
	if stageMergeQueueReasons[reason] || strings.HasPrefix(reason, "release-hold") {
		return withHumanApprovalReason(stageVerdict{Stage: stagelabels.MergeQueue, Rank: stageRankOwned}, humanApprovalReason)
	}
	if reason == contributorLaneReasonNoVerdict {
		return stageVerdict{Stage: stagelabels.ReviewPending, Rank: stageRankOwned}
	}
	if v, ok := reviewGateStage(reason, fixing); ok {
		return v
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
		return withHumanApprovalReason(stageVerdict{Stage: stagelabels.AwaitingMergeAdmission, Rank: stageRankRole}, humanApprovalReason)
	}
	return stageVerdict{}
}

// reviewGateStage is the opinion for a trusted-author head the enforcing
// review gate holds ("review-gate:<outcome>"): no bundle yet or awaiting
// the swarm's approval is 060 review-pending; a swarm changes_requested is
// the author's rework stage. Without it the PR carried no stage at all
// until the verdict was noted — the OWNERS PRs of 2026-10-20 sat unlabelled
// through the whole review wait.
func reviewGateStage(reason string, fixing stagelabels.Stage) (stageVerdict, bool) {
	if !strings.HasPrefix(reason, autoMergeReasonReviewGatePrefix) {
		return stageVerdict{}, false
	}
	outcome := strings.TrimPrefix(reason, autoMergeReasonReviewGatePrefix)
	switch {
	case outcome == ReviewGateOutcomeNoBundle, outcome == ReviewGateOutcomeAwaiting:
		return stageVerdict{Stage: stagelabels.ReviewPending, Rank: stageRankOwned}, true
	case strings.HasPrefix(outcome, ReviewGateOutcomeChangesRequested):
		return stageVerdict{Stage: fixing, Rank: stageRankOwned, Reason: stagelabels.LaneReasonChangesRequested}, true
	}
	return stageVerdict{}, false
}

// requiresHumanStage is the 085 opinion for a head whose reviewer verdict
// is requires_human / needs-decision and that no trusted human has approved
// since; rank is the strength the caller's reason carries.
func requiresHumanStage(o stageObservation, rank int) (stageVerdict, bool) {
	if !verdictNeedsHuman(o.Detail) || trustedHumanApprovedRequiresHuman(o.Detail, o.HeadSHA) {
		return stageVerdict{}, false
	}
	return stageVerdict{Stage: stagelabels.NeedsDecision, Rank: rank, Human: true, Reason: stageVerdictReason(o.Detail)}, true
}

func preserveNormalizedFixingStage(prev stagelabels.Record, hadPrev bool, obs stageObservation, verdict stageVerdict) (stageVerdict, bool) {
	if !hadPrev || prev.Stage != stagelabels.Fixing || prev.Reason != stagelabels.LaneReasonChangesRequested {
		return stageVerdict{}, false
	}
	if prev.ActivityHead == "" || obs.HeadSHA == "" || prev.ActivityHead != obs.HeadSHA {
		return stageVerdict{}, false
	}
	if verdict.Stage != stagelabels.NeedsDecision || verdict.Reason != stageReasonVerdictRequiresHuman {
		return stageVerdict{}, false
	}
	if !obs.Detail.VerdictKnown {
		return stageVerdict{}, false
	}
	raw := strings.ToLower(strings.TrimSpace(obs.Detail.Verdict.Verdict))
	normalized := strings.ToLower(strings.TrimSpace(normalizeContributorLaneVerdict(obs.Detail.Verdict).Verdict))
	if raw != "requires_human" || normalized != "changes_requested" {
		return stageVerdict{}, false
	}
	return stageVerdict{Stage: stagelabels.Fixing, Rank: stageRankOwned, Reason: stagelabels.LaneReasonChangesRequested}, true
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
// (already sorted by lane precedence) and the role tier to show.
func resolveStage(list []stageObservation) (stageVerdict, stagelabels.Role, stageObservation) {
	var best stageVerdict
	var bestObs stageObservation
	role := stagelabels.RoleNone
	for _, o := range list {
		v := classifyStage(o)
		if v.Rank > best.Rank {
			best, bestObs = v, o
		}
		if o.AuthorKind == authorkind.Hive || hasLabelFold(o.Labels, "author/hive") {
			role = role.Merge(stagelabels.RoleTrusted)
		}
		if v.Rank < stageRankOwned {
			role = role.Merge(stagelabels.RoleOf(o.Labels))
		}
		// The most specific tier wins: the contributor lane knows
		// registry/claim tiers the trusted-author lane cannot see, so a
		// trusted-author "trusted" is kept but its absence is refined.
		role = role.Merge(o.Detail.Role)
	}
	// No lane reported a tier this pass (the contributor lane's deep
	// evaluation does not run every cycle): keep the tier the PR already
	// carries instead of dropping it, which flapped role/trusted (#13637).
	if role == stagelabels.RoleNone {
		for _, o := range list {
			role = role.Merge(stagelabels.RoleOf(o.Labels))
		}
	}
	if best.Rank < stageRankOwned {
		switch {
		case role == stagelabels.RoleNone:
			role = stagelabels.RoleUntrusted
		case best.Stage == stagelabels.ReadyForHumanMerge && role == stagelabels.RoleTrusted:
			// Some lane admits this author; a lane that cannot vouch for
			// them has nothing to say about the stage. Leave it as is. A
			// weak 085 (requires_human verdict, #13115) is deliberately
			// kept: the decision is owed whoever the author is.
			best = stageVerdict{}
		}
	}
	if best.Rank == stageRankNone {
		bestObs = list[0]
	}
	return best, role, bestObs
}

// stageAuthorKind decides who wrote the PR from the observation and the
// engine's own identity. Labels the same pass is about to strip as strays
// (agent/<lane> on a PR the App did not author, #12545) are excluded first,
// so a mis-stamped human PR is never read as author/hive for a cycle.
//
// relayKnown is extra evidence the caller already holds (a live presence
// source saying the author runs a registered relay here); it ORs with the
// policy's RelayKnown hook.
func (c *Engine) stageAuthorKind(obs stageObservation, policy StageLabelPolicy, relayKnown bool) authorkind.Kind {
	appLogin := c.transport.AppBotLogin()
	stray := strayOwnershipLabels(obs.Labels, obs.Author, appLogin, policy.HiveID)
	labels := obs.Labels
	if len(stray) > 0 {
		isStray := map[string]bool{}
		for _, l := range stray {
			isStray[l] = true
		}
		labels = make([]string, 0, len(obs.Labels))
		for _, l := range obs.Labels {
			if !isStray[strings.TrimSpace(l)] {
				labels = append(labels, l)
			}
		}
	}
	in := authorkind.Input{Author: obs.Author, AppLogin: appLogin, HiveID: policy.HiveID, Labels: labels, Body: obs.Body, RelayKnown: relayKnown}
	if !in.RelayKnown && policy.RelayKnown != nil && obs.Author != "" {
		in.RelayKnown = policy.RelayKnown(obs.Author)
	}
	kind, rule := authorkind.Explain(in)
	c.debug("stage labels: author kind", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "kind", kind.String(), "rule", rule)
	return kind
}

// missingStageLanes names the lanes that evaluate every PR of displayRepo
// under the live policies but have no observation in list. A weak opinion
// (an admission reason: "this lane cannot vouch for the author") is only
// actionable once every such lane has spoken — the lane that can vouch may
// simply not be in this drain.
//
// Two drivers run the lanes against one process-wide tracker: the engine's
// own ticker (StartSelfAuthoredAutoMergeSweep) and the main loop's queued
// sweep (cmd/hive runAutoMergeSweepIfDue). When one driver's
// ApplyStageLabels lands while the other is mid-sweep, a PR's observations
// are split across two drains, and the second drain holds only the
// contributor lane's `no-claim` for a maintainer the trusted-author lane
// admitted a minute earlier: the PR was written to 090 and flipped back
// on the next pass. Seen on the hive spoke for
// every maintainer PR; the fix is to treat an incomplete drain as no
// opinion rather than to trust the weak one.
func (c *Engine) missingStageLanes(displayRepo string, list []stageObservation) []string {
	if c == nil || !c.repoAutoMergeAllowed(displayRepo) {
		return nil
	}
	seen := make(map[string]bool, len(list))
	for _, o := range list {
		seen[o.Lane] = true
	}
	var missing []string
	if c.currentTrustedAuthorPolicy().repoAllowed(displayRepo) && !seen[stageLaneTrusted] {
		missing = append(missing, stageLaneTrusted)
	}
	if c.currentContributorLanePolicy().repoAllowed(displayRepo) && !seen[stageLaneContributor] {
		missing = append(missing, stageLaneContributor)
	}
	return missing
}

// StageLabelResult summarises one ApplyStageLabels pass.
type StageLabelResult struct {
	Items    int
	Writes   int
	Comments int
	Skipped  int
	// Healed counts PRs found carrying more than one stage label.
	Healed int
	// HiveHeld counts PRs past their stage's dwell while waiting on the
	// hive (checks running, reviewer verdict pending). They never get
	// stage/stalled — the author is not the holder — so the signal is this
	// count and a warn log per PR, for the reviewer lane to act on
	// (hivecommons/hive#12534, specimen #12417).
	HiveHeld int
}

// ApplyStageLabels reconciles the stage/role/author/health labels for every PR
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
	reconciler, legacy, err := c.stageReconciler(store)
	if err != nil {
		tracker.drain()
		return result, err
	}
	ctx = hgithub.WithRESTCaller(ctx, stageRESTCaller)
	now := c.now()
	var firstErr error
	var pending []prStagePending
	for _, list := range tracker.drain() {
		if len(list) == 0 {
			continue
		}
		result.Items++
		verdict, role, obs := resolveStage(list)
		key := stageKey(obs.Owner, obs.Repo, obs.Number)
		if verdict.Rank == stageRankNone {
			result.Skipped++
			continue
		}
		if verdict.Rank < stageRankOwned && !verdict.Terminal {
			if missing := c.missingStageLanes(obs.Display, list); len(missing) > 0 {
				c.debug("stage labels: weak opinion deferred until every lane reports", "repo", obs.Display, "pr", obs.Number, "stage", verdict.Stage.String(), "lane", obs.Lane, "missing", missing)
				result.Skipped++
				continue
			}
		}
		obs.AuthorKind = c.stageAuthorKind(obs, policy, false)
		if verdict.Stage == stagelabels.NeedsAuthor || verdict.Stage == stagelabels.ReturnedToContributor {
			// classifyStage ran before the author kind was known: split
			// the author-blocked outcome by kind now (human 071 / clanker 072).
			verdict.Stage = fixingStage(obs)
		}
		obs.LaneReason = obs.Reason
		if verdict.Reason != "" {
			obs.Reason = verdict.Reason
		}
		prevRec, hadPrev := store.Get(key)
		// pass is what this pass learns past resolveStage; it rides on the
		// Facts so the engine's DerivePR reproduces the stage written here.
		pass := &stageengine.PRPassFacts{AuthorIsApp: normalizeBotLogin(obs.Author) == normalizeBotLogin(c.transport.AppBotLogin())}
		if hadPrev {
			pass.Prior = &stageengine.PriorStage{Stage: prevRec.Stage, Reason: prevRec.Reason, ActivityHead: prevRec.ActivityHead}
		}
		if replacement, ok := preserveNormalizedFixingStage(prevRec, hadPrev, obs, verdict); ok {
			verdict = replacement
			obs.Reason = replacement.Reason
		}
		var route stagelabels.Route
		if verdict.Human && !verdict.Terminal {
			var evidence stagelabels.Evidence
			route, evidence = c.routeHuman(ctx, tracker, key, obs, verdict.Stage, now)
			pass.Route = &evidence
			verdict.Stage = route.Stage
			if route.LaneReason != "" {
				obs.Reason = route.LaneReason
			}
			obs.Route = &route
		}
		var want stagelabels.Desired
		var tr stagelabels.Transition
		// dcoRead is true once the commit check for this head was read (or
		// served from the cache) without error; it is what gates caching, not
		// the label state, which is reset below for informational no-dco and
		// is unknown for heads with nothing to sign.
		dcoRead := false
		if !verdict.Terminal && verdict.Stage != stagelabels.None {
			// The commit check is the one author-side gate decided here,
			// before CI or a verdict: an unsigned head goes straight back
			// to its author (hivecommons/hive#12542).
			stats, err := c.stageDCO(ctx, store, key, obs)
			if err != nil {
				c.warn("stage labels: dco commit check failed", "repo", obs.Display, "pr", obs.Number, "error", err)
				if firstErr == nil {
					firstErr = err
				}
			}
			dcoRead = err == nil
			obs.Detail.DCO = dcoState(stats.total, stats.unsigned)
			obs.Detail.DCOTotal = stats.total
			obs.Detail.DCOUnsigned = stats.unsigned
			obs.Detail.DCOUnsignedHive = stats.unsignedHive
			obs.Detail.DCOUnsignedOther = stats.unsignedOther
			obs.Detail.DCOSquashUnsafeHive = stats.squashUnsafeHive
			want.DCO = obs.Detail.DCO
			pass.DCO, pass.DCOKnown = want.DCO, true
			pass.DCORequired = dcoRequired(policy, obs)
			pass.SquashUnsafeHive = stats.squashUnsafeHive
			// The label is written everywhere; the 072 override only where
			// the repo actually enforces DCO (config, else detected gate).
			if want.DCO == stagelabels.DCONo && dcoRequired(policy, obs) {
				verdict.Stage = fixingStage(obs)
				verdict.Rank = stageRankOwned
				obs.Reason = stagelabels.LaneReasonDCOMissing
			} else if stats.squashUnsafeHive > 0 && normalizeBotLogin(obs.Author) != normalizeBotLogin(c.transport.AppBotLogin()) {
				// A sign-off naming the App on a commit the squash would
				// land as the contributor: only the branch owner can rewrite
				// it (no hive push lands on a non-App branch), so the PR
				// goes back to its author by kind (071/072), not to 070
				// where nothing can act (hivecommons/hive OWNERS PRs of
				// 2026-10-20).
				verdict.Stage = fixingStage(obs)
				verdict.Rank = stageRankOwned
				obs.Reason = stagelabels.LaneReasonHiveSignoffAttribution
			} else if want.DCO == stagelabels.DCONo {
				// Not enforced here: keep the lane's stage and do not ask
				// the author to rebase in the nudge.
				obs.Detail.DCO = stagelabels.DCOUnknown
			}
		}
		if verdict.Terminal {
			store.Forget(key)
			c.resolveNudge(ctx, prevRec, hadPrev, obs)
		} else {
			if hadPrev && prevRec.AdoptedInPlace() {
				// In-place adoption: a head the hive pushed keeps the
				// adoption; a head the contributor pushed yields it.
				// Settled on every head change whatever the lane sees —
				// the hive's fix going green must not hide the yield.
				n, err := c.settleInPlaceAdoption(ctx, store, key, obs, prevRec, policy)
				result.Comments += n
				if err != nil && firstErr == nil {
					firstErr = err
				}
				prevRec, _ = store.Get(key)
			}
			if hadPrev && prevRec.AdoptionLive(obs.HeadSHA) && verdict.Stage.YieldsToAdoption() {
				// The hive owns this PR: whether the lane sees it red,
				// pending or awaiting a verdict, the stage is 074 until the
				// adoption is released or the merge gate takes it
				// (hivecommons/hive#12736) — unless nothing can finish it
				// there: a dead-end in-place adoption re-homes, and an
				// adoption nobody is working past the dwell is a person's
				// (085) with the lane reason named (hivecommons/hive#12858).
				verdict.Stage = stagelabels.AdoptedByHive
				pass.AdoptionLive = true
				esc, n, err := c.escapeStalledAdoption(ctx, store, key, obs, policy, now)
				result.Comments += n
				if err != nil && firstErr == nil {
					firstErr = err
				}
				if esc.Escalated {
					verdict.Stage = stagelabels.NeedsDecision
					verdict.Rank = stageRankOwned
					obs.Reason = esc.Reason
					pass.EscalationReason = esc.Reason
				}
			}
			tr = store.Observe(key, verdict.Stage, obs.Reason)
			// hivecommons/hive#12502: remember the author so the dashboard can
			// list "PRs returned to this contributor" from the store alone.
			store.NoteAuthor(key, obs.Author)
			if dcoRead {
				store.NoteDCO(key, obs.HeadSHA, obs.Detail.DCOTotal, obs.Detail.DCOUnsigned, obs.Detail.DCOUnsignedHive, obs.Detail.DCOSquashUnsafeHive)
			}
			if verdict.Stage != stagelabels.ReturnedToContributor && verdict.Stage != stagelabels.NeedsAuthor {
				c.resolveNudge(ctx, prevRec, hadPrev, obs)
			}
			store.NoteActivity(key, stageActivity(obs, verdict.Stage))
			rec, _ := store.Get(key)
			want = stagelabels.Desired{
				Stage:      verdict.Stage,
				Role:       role,
				Kind:       obs.AuthorKind,
				KindLabels: policy.AuthorKindLabels,
				Stalled:    stagelabels.StalledWith(rec, policy.Dwell, now, policy.StalledRequireQuiet),
				DCO:        want.DCO,
			}
			if stagelabels.HiveHeld(rec, policy.Dwell, now) {
				result.HiveHeld++
				if !rec.HiveHeldWarned {
					c.warn("stage labels: PR waits on the hive past the stage dwell — reviewer lane needs attention", "repo", obs.Display, "pr", obs.Number, "stage", verdict.Stage.String(), "quiet_for", rec.QuietFor(now).Truncate(time.Minute).String(), "reason", obs.Reason, "lane", obs.Lane)
					store.MarkHiveHeldWarned(key)
				}
			}
		}
		if !verdict.Terminal {
			n, err := c.stripStrayOwnershipLabels(ctx, obs, policy)
			result.Writes += n
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if obs.Detail.VerdictKnown {
			rawVerdict := obs.Detail.Verdict
			normalizedVerdict := normalizeContributorLaneVerdict(rawVerdict)
			c.info("stage labels: reviewer verdict route", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "lane", obs.Lane, "raw_verdict", rawVerdict.Verdict, "normalized_verdict", normalizedVerdict.Verdict, "decision_requested", normalizedVerdict.DecisionRequested, "stage", verdict.Stage.String(), "reason", obs.Reason)
		}
		add, remove := stagelabels.Plan(obs.Labels, want)
		if len(add)+len(remove) > 0 {
			if err := c.ensureStageLabels(ctx, obs.Owner, obs.Repo, tracker, policy.AuthorKindLabels); err != nil {
				c.warn("stage labels: provisioning failed", "repo", obs.Display, "error", err)
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
		}
		facts := factsFrom(list)
		facts.ObservedAt = now
		pass.AuthorKind = obs.AuthorKind
		facts.PR.Pass = pass
		legacy.Set(facts.Ref, stageengine.Decision{
			Stage: want.Stage, Role: want.Role, Kind: want.Kind, KindLabels: want.KindLabels,
			Stalled: want.Stalled, DCO: want.DCO, Terminal: verdict.Terminal, Reason: obs.Reason,
		})
		pending = append(pending, prStagePending{obs: obs, key: key, role: role, want: want, tr: tr, facts: facts})
	}
	// The write: one ReconcileBatch for the pass. The reconciler re-reads
	// each PR before writing so a stage another writer placed since the
	// lane's snapshot is replaced, never joined (hivecommons/hive#12512).
	facts := make([]stageengine.Facts, 0, len(pending))
	for _, p := range pending {
		facts = append(facts, p.facts)
	}
	var transitions []stageengine.Transition
	if len(facts) > 0 {
		transitions, _ = reconciler.ReconcileBatch(ctx, facts)
	}
	for i, p := range pending {
		legacy.Delete(p.facts.Ref)
		obs, key, role, want, tr := p.obs, p.key, p.role, p.want, p.tr
		commentsBefore := result.Comments
		var out stagelabels.Outcome
		err := ctx.Err()
		if i < len(transitions) {
			out, err = transitions[i].Outcome, transitions[i].Err
		}
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
		if out.Changed() {
			c.info("stage labels reconciled", "repo", obs.Display, "pr", obs.Number, "stage", out.Stage.String(), "role", role.String(), "author_kind", obs.AuthorKind.String(), "added", out.Added, "removed", out.Removed, "healed", out.Healed, "clamped", out.Clamped, "wanted", out.Wanted.String(), "reason", obs.Reason, "lane", obs.Lane)
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
		if obs.Route != nil && (want.Stage == stagelabels.ReadyForHumanMerge || want.Stage == stagelabels.NeedsDecision) {
			n, err := c.postHumanEntry(ctx, store, key, obs, role, *obs.Route)
			result.Comments += n
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if (want.Stage == stagelabels.ReturnedToContributor || want.Stage == stagelabels.NeedsAuthor) && (policy.ContributorNudge || policy.Adoption) {
			// The 072 interrupt machinery (nudge + adoption) is for
			// author/clanker only: there is a hive on the other end to
			// interrupt or stand in for. A human-authored PR simply waits
			// at 071 (docs: "Stage 07x by author kind").
			presence, hasPresence := c.contributorPresence(ctx, obs.Author)
			kind := obs.AuthorKind
			if kind != authorkind.Clanker && hasPresence && presence.Registered {
				// Live presence is registry evidence the label pass did
				// not have: a registered relay author is a clanker.
				kind = c.stageAuthorKind(obs, policy, true)
			}
			if kind == authorkind.Clanker {
				if policy.ContributorNudge {
					n, err := c.nudgeContributor(ctx, store, key, obs, policy, now)
					result.Comments += n
					if err != nil && firstErr == nil {
						firstErr = err
					}
				}
				if policy.Adoption {
					n, err := c.adoptContributorPR(ctx, store, key, obs, policy, presence, hasPresence, now)
					result.Comments += n
					if err != nil && firstErr == nil {
						firstErr = err
					}
				}
			} else if obs.Reason == stagelabels.LaneReasonHiveSignoffAttribution {
				// A person owns the fix and nothing else will tell them:
				// one comment per head with the exact commands.
				n, err := c.nudgeSignoffAttribution(ctx, store, key, obs, now)
				result.Comments += n
				if err != nil && firstErr == nil {
					firstErr = err
				}
			} else if rec, ok := store.Get(key); ok && rec.NudgeHead != obs.HeadSHA {
				c.info("stage labels: 07x without interrupt (author kind)", "stage", want.Stage.String(), "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "kind", string(kind))
			}
		}
		if want.Stage == stagelabels.AdoptedByHive {
			n, err := c.settleAdoption(ctx, store, key, obs, policy, now)
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
		// The hive's own comments are not outside activity.
		store.CountOwnComments(key, result.Comments-commentsBefore)
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
// missing or whose colour drifted from the table. A PATCH keeps the label on
// every PR wearing it, so a pre-existing `dco` label is recoloured in place
// rather than re-added.
func (c *Engine) ensureStageLabels(ctx context.Context, owner, repo string, tracker *StageTracker, kinds authorkind.Names) error {
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
	for old, stage := range stagelabels.Renames {
		// Rename in place so every item carrying the old spelling keeps its
		// stage; a repo that already has the new name just drops the old.
		cur, hadOld := existing[strings.ToLower(old)]
		if _, hasNew := existing[strings.ToLower(stage.Label())]; !hadOld || hasNew {
			continue
		}
		renamed, _, err := c.gh.Issues.EditLabel(ctx, owner, repo, cur.GetName(), &gh.Label{Name: gh.Ptr(stage.Label()), Color: gh.Ptr(stagelabels.StageColor), Description: gh.Ptr(stagelabels.ClampLabelDescription(stage.Description()))})
		if err != nil {
			return fmt.Errorf("renaming label %s to %s in %s: %w", old, stage.Label(), full, err)
		}
		delete(existing, strings.ToLower(old))
		if renamed == nil {
			renamed = &gh.Label{Name: gh.Ptr(stage.Label()), Color: gh.Ptr(stagelabels.StageColor)}
		}
		existing[strings.ToLower(stage.Label())] = renamed
	}
	for _, def := range stagelabels.LabelDefsFor(kinds) {
		cur, ok := existing[strings.ToLower(def.Name)]
		switch {
		case !ok:
			_, _, err := c.gh.Issues.CreateLabel(ctx, owner, repo, &gh.Label{Name: gh.Ptr(def.Name), Color: gh.Ptr(def.Color), Description: gh.Ptr(stagelabels.ClampLabelDescription(def.Description))})
			if err != nil && !isGitHubStatus(err, http.StatusUnprocessableEntity) {
				return fmt.Errorf("creating label %s in %s: %w", def.Name, full, err)
			}
		case !strings.EqualFold(cur.GetColor(), def.Color):
			_, _, err := c.gh.Issues.EditLabel(ctx, owner, repo, cur.GetName(), &gh.Label{Name: gh.Ptr(def.Name), Color: gh.Ptr(def.Color), Description: gh.Ptr(stagelabels.ClampLabelDescription(def.Description))})
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
