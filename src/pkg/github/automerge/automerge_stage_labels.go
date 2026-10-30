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
	"github.com/hivecommons/hive/pkg/delegatedecision"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
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
	// AdoptOfflineGrace is the stage-stalled escape for a recorded adoption
	// attempt that opened nothing (a supersede whose replay failed) while
	// the author's clanker is still offline or unregistered: after it the
	// head is admitted to adoption again instead of sitting at 072/085
	// (governor.labels.stage.adoption.offline_grace, default 30m;
	// hivecommons/hive#15307).
	AdoptOfflineGrace time.Duration
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
	// AdoptionAutoMerge lets the contributor lane admit an in-place-adopted
	// head under the hive's own authority, without the original author's
	// trust tier (governor.labels.stage.adoption.auto_merge, default on).
	// Every other contributor-lane gate still applies; the head must be
	// exactly the one the stage store records as adopted/pushed by the hive.
	AdoptionAutoMerge bool
	// AdoptionMaintainerPolicy is governor.labels.stage.adoption.maintainer_policy
	// (config.AdoptionMaintainerPolicy*): what adoption does when the PR
	// author holds admin or maintain on the repository. "" reads as the
	// default, adopt (hivecommons/hive#14524).
	AdoptionMaintainerPolicy string
	// AdoptSupersedeCloseFirst closes the original as soon as the hive's
	// superseding copy is open and reopens it if the copy fails
	// (governor.labels.stage.adoption.supersede.close_first, default on;
	// hivecommons/hive#15135). Off, the original stays open at 074 until
	// the copy merges.
	AdoptSupersedeCloseFirst bool
	// AdoptCloseSuperseded is governor.labels.stage.adoption.close_superseded
	// (config.AdoptionCloseSuperseded*): when an agent-authored original
	// (author/clanker, author/hive) closes once the hive's copy exists.
	// "" or on_adopt closes it in the adopting pass by policy, without the
	// write-back opt-in label the contributor_adopt kind otherwise needs;
	// on_merge leaves it open until the copy merges. Human-authored
	// originals always take the gated path.
	AdoptCloseSuperseded string
	// DCORequired resolves the per-repo `governor.labels.stage.dco.required`
	// override for "owner/repo"; ok false means "detect" (a DCO context in
	// the branch's required checks or on the head). nil = always detect.
	DCORequired func(display string) (required, ok bool)
	// SquashOnly reports whether "owner/repo" forbids merge commits
	// (auto_merge.squash_only). Where it does not, a role/trusted head
	// mixing contributor and self-signed hive commits is merged with a
	// merge commit (`merge-method/merge`) rather than parked on
	// hive-signoff-attribution. nil = merge commits allowed everywhere.
	SquashOnly func(display string) bool
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
	// OwnerAuthor reports whether login is one of this hive's owners
	// (dashboard.authorized_users). Their PRs, like author/human ones, are
	// never adopted (hivecommons/hive#15202). nil = unknown.
	OwnerAuthor func(login string) bool
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
	// ForkGuard returns the failing checks on a fork PR head (repo
	// "owner/name", number, head SHA) that refuse it only because it is from
	// a fork, when EVERY failing check does —
	// hgithub.PullRequest.ForkGuardChecks. Such a PR is parked for a
	// maintainer at 085 with reason `fork-guard:<checks>`: neither its author
	// nor an adoption can turn the check green (hivecommons/hive#14273).
	// nil = unknown (no park).
	ForkGuard func(repo string, number int, head string) []string
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
	stageLaneSelf        = prstage.StageLaneSelf
	stageLaneTrusted     = prstage.StageLaneTrusted
	stageLaneContributor = contributorLaneTier
)

var stageLanePrecedence = map[string]int{stageLaneSelf: 3, stageLaneTrusted: 2, stageLaneContributor: 1}

type stageDetail = prstage.Detail
type stageObservation = prstage.Observation

const stageReasonOptionalCheckFailing = prstage.ReasonOptionalCheckFailing

// StageTracker accumulates observations across the lanes of one cycle. One
// tracker is shared process-wide (sharedStageTracker) because the governor
// sweep builds a fresh engine per lane.
type StageTracker struct {
	mu      sync.Mutex
	obs     map[string]map[string]stageObservation // key → lane → observation
	details map[string]stageDetail                 // key → detail noted by trySweep*
	ensured map[string]bool                        // repo → labels provisioned this process
	checks  map[string]humanChecksEntry            // key → head check digest (human routing)
	// superseded remembers when the hive-authored superseded check last
	// ran per key+head, so a PR that legitimately sits at 085 is not
	// re-read every pass.
	superseded map[string]time.Time
}

// NewStageTracker returns an empty tracker.
func NewStageTracker() *StageTracker {
	return &StageTracker{obs: map[string]map[string]stageObservation{}, details: map[string]stageDetail{}, ensured: map[string]bool{}, checks: map[string]humanChecksEntry{}, superseded: map[string]time.Time{}}
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
			if detail.HumanChangeRequester != "" && detail.HumanChangeRequestHead != "" && detail.HumanChangeRequestHead == o.HeadSHA {
				o.Detail.HumanChangeRequester, o.Detail.HumanChangeRequestHead = detail.HumanChangeRequester, detail.HumanChangeRequestHead
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
	StageReconciler(store *stagelabels.Store, opts *stageengine.Options, logger *slog.Logger) stageengine.Reconciler
}

// ErrNoStageWriter is returned when the transport cannot supply the stage
// writer the reconciler needs; the sweep writes nothing rather than
// guess.
var ErrNoStageWriter = errors.New("automerge: transport provides no stage writer")

// stageReconciler returns the reconciler for this pass (see
// Options.StageEngine).
func (c *Engine) stageReconciler(store *stagelabels.Store) (stageengine.Reconciler, error) {
	t, ok := c.transport.(stageReconcilerTransport)
	if !ok {
		return nil, ErrNoStageWriter
	}
	r := t.StageReconciler(store, c.stageEngine, c.logger)
	if r == nil {
		return nil, ErrNoStageWriter
	}
	return r, nil
}

// restageAfterAdoption re-derives and writes the stage of a PR this pass
// just adopted, in the same pass: the batch write above ran before the
// adoption, so the label it placed (085 needs-decision on a reviewer
// `requires_human`, 072 returned-to-contributor) is the stage of a PR
// nobody owned. With the adoption on record the stage is 074
// adopted-by-hive now — the hive owns the PR and answers the question on
// the superseding PR — not on the next sweep, when the Activity Monitor
// would have shown "Needs human decision" on it in the meantime
// (hivecommons/hive#14975 → #14980, #14947 → #14977). A hold label or
// sentinel alert keeps the park (YieldsToAdoptionFor).
func (c *Engine) restageAfterAdoption(ctx context.Context, reconciler stageengine.Reconciler, store *stagelabels.Store, p prStagePending) error {
	rec, ok := store.Get(p.key)
	if !ok || reconciler == nil || p.facts.PR == nil || p.facts.PR.Pass == nil || !rec.AdoptionLive(p.obs.HeadSHA) {
		return nil
	}
	if p.want.Stage == stagelabels.AdoptedByHive || !p.want.Stage.YieldsToAdoptionFor(p.facts.PR.Pass.Route) {
		return nil
	}
	facts := p.facts
	pass := *facts.PR.Pass
	pass.AdoptionLive = true
	pr := *facts.PR
	pr.Pass = &pass
	facts.PR = &pr
	facts.Adoption = stageengine.AdoptionFacts{Adopted: true, Live: true, Head: rec.AdoptedHead, InPlace: rec.AdoptedInPlace(), SupersededBy: rec.AdoptedBy}
	tr, err := reconciler.Reconcile(ctx, facts)
	if err != nil {
		c.warn("stage labels: re-stage after adoption failed", "repo", p.obs.Display, "pr", p.obs.Number, "superseded_by", rec.AdoptedBy, "error", err)
		return err
	}
	if tr.Outcome.Changed() {
		c.info("stage labels: adopted PR re-staged", "repo", p.obs.Display, "pr", p.obs.Number, "from", p.want.Stage.String(), "stage", tr.Outcome.Stage.String(), "superseded_by", rec.AdoptedBy)
		// Leaving 072 in this pass: the nudge queue entry resolves now, as
		// it would have on the next pass's 072 → 074 observation.
		c.resolveNudge(ctx, stagelabels.Record{Stage: p.want.Stage}, true, p.obs)
	}
	return nil
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
		obs.HeadRef = pr.GetHead().GetRef()
	}
	if pr.GetHead() != nil && pr.GetHead().GetRepo() != nil {
		obs.HeadRepo = pr.GetHead().GetRepo().GetFullName()
	}
	if pr.GetBase() != nil {
		obs.BaseRef = pr.GetBase().GetRef()
	}
	obs.ForwardMerge = hgithub.IsForwardMergeHead(pr.GetHead().GetRef())
	obs.Author = hgithub.SafeGetLogin(pr.GetUser())
	obs.Title = pr.GetTitle()
	obs.URL = pr.GetHTMLURL()
	obs.Body = pr.GetBody()
	obs.OpenedAt = pr.GetCreatedAt().Time
	obs.SharedCIIncident = stageSharedCIIncident(policy, owner, repo, obs.Number, obs.HeadSHA)
	obs.BaseRedChecks = stageBaseBranchRed(policy, owner, repo, obs.Number, obs.HeadSHA)
	obs.ForkGuardChecks = stageForkGuard(policy, owner, repo, obs.Number, obs.HeadSHA)
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

// noteStageHumanChangeRequest records that a trusted human's change request
// (review or `/hive decision <text>`) stands against the exact head. Stage
// derivation reads it as "the person answered": the requires_human /
// decision-requested verdict on that head no longer parks the PR at 085
// (hivecommons/hive#14679).
func (c *Engine) noteStageHumanChangeRequest(owner, repo string, number int, head, login string) {
	if !c.stagePolicy().Enabled || strings.TrimSpace(login) == "" || strings.TrimSpace(head) == "" {
		return
	}
	c.stageTrackerOrShared().note(stageKey(owner, repo, number), func(d *stageDetail) {
		d.HumanChangeRequester, d.HumanChangeRequestHead = strings.TrimSpace(login), strings.TrimSpace(head)
	})
}

// foldRecordedDecision stamps the trusted human's recorded decision on each
// observation's head into its verdict before classification. The lanes note
// the same answer when they reach it, but a lane that skips early (the
// contributor lane on an app-authored PR, a lane stopped at CI) never does,
// and the stage pass must not re-derive 085 from the stale decision-requested
// verdict in that case — that was the 070↔085 flap on hive#14628
// (hivecommons/hive#14679). Reads the durable store, so it holds across
// restarts and across the drain that clears the tracker's details.
func (c *Engine) foldRecordedDecision(list []stageObservation) {
	if c == nil || c.humanChangeRequests == nil {
		return
	}
	for i := range list {
		o := &list[i]
		if o.HeadSHA == "" {
			continue
		}
		req, ok := review.DecisionAnswerFor(c.humanChangeRequests, o.Owner+"/"+o.Repo, o.Number, o.HeadSHA)
		if !ok {
			continue
		}
		// A standing change request is the person's answer whether or not
		// a reviewer verdict was read for the head: a lane that skipped
		// before reviews (the contributor lane on a sentinel park,
		// hivecommons/hive#15159) carries no verdict to hang it on.
		if !req.IsApproval() && o.Detail.HumanChangeRequestHead != o.HeadSHA {
			o.Detail.HumanChangeRequester, o.Detail.HumanChangeRequestHead = strings.TrimSpace(req.Actor), o.HeadSHA
		}
		if o.Detail.VerdictKnown && o.Detail.Verdict.DecisionAnswered && o.Detail.Verdict.DecisionAnsweredHead == o.HeadSHA {
			continue
		}
		o.Detail.Verdict = o.Detail.Verdict.WithDecisionAnswer(strings.ToLower(req.Actor), o.HeadSHA, req.Directive())
		o.Detail = normalizeStageDetail(o.Detail)
	}
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

// stageReasonSkipsContributorReview reports admission failures that stop the
// contributor lane before it requests a reviewer verdict. A person must clear
// admission before a missing verdict can mean the hive owes a review.
func stageReasonSkipsContributorReview(reason string) bool {
	switch reason {
	case contributorLaneReasonNoClaim, contributorLaneReasonNoLinkedIssue, contributorLaneReasonClaimUnverified,
		contributorLaneReasonClaimAuthor, contributorLaneReasonClaimOtherHive, contributorLaneReasonClaimTier,
		contributorLaneReasonAuthorAssociation:
		return true
	}
	return false
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
	// verdict yet, is waiting on the hive, not on a person. A contributor PR
	// the lane skipped for an admission failure gets no verdict until the
	// contributor or a maintainer acts, so the missing verdict alone does
	// not make it wait on the hive (ApplyStageLabels still honours the
	// head's running checks from the human route's digest).
	a.WaitingOnHive = ci == ciReasonPending || stageReasonWaitsOnHive(obs.Reason) ||
		(stage == stagelabels.ReviewPending && !stageReasonSkipsContributorReview(obs.Reason) &&
			!(obs.Detail.VerdictKnown && obs.Detail.Verdict.Verdict != ""))
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

type stageVerdict = prstage.Verdict
type ciReasonKind = stageengine.CIReasonKind

const (
	stageRankNone                   = prstage.RankNone
	stageRankRole                   = prstage.RankRole
	stageRankAdmission              = prstage.RankAdmission
	stageRankOwned                  = prstage.RankOwned
	ciReasonNone                    = stageengine.CIReasonNone
	ciReasonPending                 = stageengine.CIReasonPending
	ciReasonRed                     = stageengine.CIReasonRed
	stageReasonVerdictRequiresHuman = stageengine.StageReasonVerdictRequiresHuman
	stageReasonDecisionRequested    = stageengine.StageReasonDecisionRequested
	stageReasonDecisionAnswered     = stageengine.StageReasonDecisionAnswered
)

func classifyCIReason(reason string) ciReasonKind      { return stageengine.ClassifyCIReason(reason) }
func normalizeStageDetail(d stageDetail) stageDetail   { return stageengine.NormalizeStageDetail(d) }
func verdictBlocks(d stageDetail) bool                 { return stageengine.VerdictBlocks(d) }
func verdictNeedsHuman(d stageDetail) bool             { return stageengine.VerdictNeedsHuman(d) }
func fixingStage(o stageObservation) stagelabels.Stage { return stageengine.FixingStage(o) }
func classifyStage(o stageObservation) stageVerdict    { return stageengine.ClassifyStage(o) }
func preserveNormalizedFixingStage(prev stagelabels.Record, hadPrev bool, obs stageObservation, verdict stageVerdict) (stageVerdict, bool) {
	return stageengine.PreserveNormalizedFixingStage(prev, hadPrev, obs, verdict)
}
func hasLabelFold(labels []string, want string) bool { return stageengine.HasLabelFold(labels, want) }
func resolveStage(list []stageObservation) (stageVerdict, stagelabels.Role, stageObservation) {
	return stageengine.ResolveStage(list)
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
	// ClosedSuperseded counts hive-authored PRs the pass closed as
	// superseded instead of parking them on a person (hivecommons/hive#15208).
	ClosedSuperseded int
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
	reconciler, err := c.stageReconciler(store)
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
		c.foldHumanReturnLabel(ctx, store, list)
		c.foldRecordedDecision(list)
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
		if hasLabelFold(obs.Labels, "hold") && !verdict.Terminal {
			// Who held it and why rides the row (hivecommons/hive#15020);
			// a reporter-trust hold the hive put on its own PR is lifted
			// here — the hive is the author, the reporter's trust is moot.
			prov, notice, err := c.holdProvenance(ctx, obs.Owner, obs.Repo, obs.Number)
			if err != nil {
				c.debug("stage labels: hold provenance unreadable", "repo", obs.Display, "pr", obs.Number, "error", err)
			}
			hiveAuthored := pass.AuthorIsApp || obs.AuthorKind == authorkind.Hive
			if hiveAuthored && prov.SelfImposed() && prov.ReporterTrust() {
				released, err := c.releaseSelfImposedReporterTrustHold(ctx, obs, prov, notice)
				if err != nil {
					c.warn("stage labels: could not release self-imposed reporter-trust hold", "repo", obs.Display, "pr", obs.Number, "error", err)
					if firstErr == nil {
						firstErr = err
					}
				}
				if released {
					result.Comments++
					result.Skipped++
					store.ClearHold(key)
					continue
				}
			}
			obs.Hold = prov
		}
		// Issue-closed beats the reviewer's verdict: a hive-authored PR
		// whose work a merged PR already delivered is closed here, not
		// parked at 085 for a person to say so, and not sent to 070 to
		// rebase a conflicting head onto it (hivecommons/hive#15208).
		if !verdict.Terminal && verdict.Reason != prstage.ReasonSupersessionPending &&
			supersededCheckWarranted(verdict, obs) &&
			c.closeSupersededBeforeHumanRoute(ctx, tracker, key, obs, pass, now) {
			store.Forget(key)
			c.resolveNudge(ctx, prevRec, hadPrev, obs)
			result.ClosedSuperseded++
			result.Skipped++
			continue
		}
		var route stagelabels.Route
		if verdict.Human && !verdict.Terminal && verdict.Reason != prstage.ReasonSupersessionPending {
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
		if !verdict.Terminal && verdict.Stage != stagelabels.None && verdict.Reason != prstage.ReasonSupersessionPending {
			// The commit check is the one author-side gate decided here,
			// before CI or a verdict: an unsigned head goes straight back
			// to its author (hivecommons/hive#12542).
			var stats dcoStats
			if !obs.ForwardMerge {
				// A forward-merge lands as a merge commit carrying other
				// people's commits by design: nothing is squashed, and a
				// rebase to re-sign them would rewrite the source line
				// (hivecommons/hive#13635). The skip caches nothing: a
				// zero record for the head would be served back if the
				// same head later stopped matching.
				var err error
				stats, err = c.stageDCO(ctx, store, key, obs)
				if err != nil {
					c.warn("stage labels: dco commit check failed", "repo", obs.Display, "pr", obs.Number, "error", err)
					if firstErr == nil {
						firstErr = err
					}
				}
				dcoRead = err == nil
			}
			obs.Detail.DCO = dcoState(stats.total, stats.unsigned)
			obs.Detail.DCOTotal = stats.total
			obs.Detail.DCOUnsigned = stats.unsigned
			obs.Detail.DCOUnsignedHive = stats.unsignedHive
			obs.Detail.DCOUnsignedOther = stats.unsignedOther
			obs.Detail.DCOSquashUnsafeHive = stats.squashUnsafeHive
			want.DCO = obs.Detail.DCO
			if dcoRead && stats.squashUnsafeHive == 0 {
				// Nothing a squash would misattribute: a head that no
				// longer needs the merge commit drops the label.
				want.MergeCommit = stagelabels.MergeCommitNotWanted
			}
			pass.DCO, pass.DCOKnown = want.DCO, true
			pass.MergeCommit = want.MergeCommit
			pass.DCORequired = dcoRequired(policy, obs)
			pass.SquashUnsafeHive = stats.squashUnsafeHive
			// The label is written everywhere; the 072 override only where
			// the repo actually enforces DCO (config, else detected gate).
			if want.DCO == stagelabels.DCONo && dcoRequired(policy, obs) {
				verdict.Stage = fixingStage(obs)
				verdict.Rank = stageRankOwned
				obs.Reason = stagelabels.LaneReasonDCOMissing
			} else if stats.squashUnsafeHive > 0 && !pass.AuthorIsApp {
				// A sign-off naming the App on a commit the squash would
				// land as the contributor. For a role/trusted PR whose
				// every commit is self-signed the lane selects a merge
				// commit instead (mergeCommitElection): nothing is
				// squashed, so nothing is misattributed, and the PR
				// stays in its lane. Otherwise only the branch owner can
				// rewrite it (no hive push lands on a non-App branch), so
				// the PR goes back to its author by kind (071/072), not
				// to 070 where nothing can act (hivecommons/hive OWNERS
				// PRs of 2026-10-20).
				election := mergeCommitElection(stats, role, policy, obs.Display)
				obs.Detail.MergeCommit, obs.Detail.MergeCommitRefused = election.Elected, election.Refused
				if election.Elected {
					want.MergeCommit = stagelabels.MergeCommitWanted
					pass.MergeCommit = want.MergeCommit
					n, err := c.noteMergeCommit(ctx, store, key, obs, stats)
					result.Comments += n
					if err != nil && firstErr == nil {
						firstErr = err
					}
				} else {
					want.MergeCommit, pass.MergeCommit = stagelabels.MergeCommitNotWanted, stagelabels.MergeCommitNotWanted
					verdict.Stage = fixingStage(obs)
					verdict.Rank = stageRankOwned
					obs.Reason = stagelabels.LaneReasonHiveSignoffAttribution
				}
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
				n, err := c.settleInPlaceAdoption(ctx, store, key, obs, prevRec, policy, now)
				result.Comments += n
				if err != nil && firstErr == nil {
					firstErr = err
				}
				prevRec, _ = store.Get(key)
			}
			if hadPrev && prevRec.AdoptionLive(obs.HeadSHA) && verdict.Stage.YieldsToAdoptionFor(pass.Route) {
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
			if hadPrev && !verdict.Terminal && prevRec.AdoptionBlocked(obs.HeadSHA) == stagelabels.AdoptBlockedSupersedeConflict && verdict.Stage == fixingStage(obs) {
				// A delegated supersede that could not replay onto the base
				// keeps the PR on its fixing stage with the failure named.
				obs.Reason = stagelabels.AdoptBlockedSupersedeConflict
				pass.AdoptionRefusedReason = stagelabels.AdoptBlockedSupersedeConflict
			}
			if !verdict.Terminal && verdict.Stage == fixingStage(obs) && c.adoptionCopyRebaseFailedTwice(obs) {
				// The hive's own adoption copy, still conflicting after the
				// fixer's two rebase-resolve attempts (hivecommons/hive#15307):
				// a person decides (adopt into a fresh branch / close as
				// superseded) instead of the copy idling on its fixing stage.
				verdict.Stage = stagelabels.NeedsDecision
				verdict.Rank = stageRankOwned
				obs.Reason = stagelabels.LaneReasonRebaseFailedTwice
				pass.RebaseFailedTwice = true
			}
			tr = store.Observe(key, verdict.Stage, obs.Reason)
			// hivecommons/hive#12502: remember the author so the dashboard can
			// list "PRs returned to this contributor" from the store alone.
			store.NoteAuthor(key, obs.Author)
			store.NoteHold(key, obs.Hold)
			noteSoak(store, key, obs.Reason, obs.HeadSHA, pass.Route, c.currentContributorLanePolicy().Soak, now)
			if dcoRead {
				store.NoteDCO(key, obs.HeadSHA, obs.Detail.DCOTotal, obs.Detail.DCOUnsigned, obs.Detail.DCOUnsignedHive, obs.Detail.DCOSquashUnsafeHive)
			}
			if verdict.Stage != stagelabels.ReturnedToContributor && verdict.Stage != stagelabels.NeedsAuthor {
				c.resolveNudge(ctx, prevRec, hadPrev, obs)
			}
			act := stageActivity(obs, verdict.Stage)
			if !act.WaitingOnHive && verdict.Stage == stagelabels.ReviewPending && pass.Route != nil && pass.Route.Checks.Known && pass.Route.Checks.Pending > 0 {
				// An admission reason decided before CI was read (no
				// linked issue) says nothing about the checks; the human
				// route's digest of the head does, and running checks
				// wait on the hive.
				act.WaitingOnHive = true
			}
			store.NoteActivity(key, act)
			rec, _ := store.Get(key)
			want = stagelabels.Desired{
				Stage:       verdict.Stage,
				Role:        role,
				Kind:        obs.AuthorKind,
				KindLabels:  policy.AuthorKindLabels,
				Stalled:     stagelabels.StalledWith(rec, policy.Dwell, now, policy.StalledRequireQuiet),
				DCO:         want.DCO,
				MergeCommit: want.MergeCommit,
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
			c.info("stage labels: reviewer verdict route", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "lane", obs.Lane, "raw_verdict", rawVerdict.Verdict, "normalized_verdict", normalizedVerdict.Verdict, "decision_requested", normalizedVerdict.DecisionRequested, "decision_answered", normalizedVerdict.DecisionAnswered && normalizedVerdict.DecisionAnsweredHead == obs.HeadSHA, "decision_by", normalizedVerdict.DecisionAnsweredBy, "decision_directive", normalizedVerdict.DecisionDirective, "stage", verdict.Stage.String(), "reason", obs.Reason)
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
		pass.Stalled, pass.KindLabels = want.Stalled, want.KindLabels
		facts.PR.Pass = pass
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
		if i >= len(transitions) {
			// A context or writer deadline stopped the batch. The tail
			// was never attempted and must not trigger public side effects.
			if firstErr == nil {
				firstErr = ctx.Err()
				if firstErr == nil {
					firstErr = fmt.Errorf("stage labels: batch stopped before %s#%d", p.obs.Display, p.obs.Number)
				}
			}
			break
		}
		obs, key, role, want, tr := p.obs, p.key, p.role, p.want, p.tr
		commentsBefore := result.Comments
		out, err := transitions[i].Outcome, transitions[i].Err
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
		// Explicit delegated adoption also applies to a PR parked at 085,
		// whether or not a nudge was ever sent — except for a PR a person
		// pushed themselves, which adoptContributorPR refuses and records
		// (hivecommons/hive#15202).
		if policy.Adoption && !obs.Merged && hasLabel(obs.Labels, delegatedecision.LabelAdoptSupersede) {
			before, _ := store.Get(key)
			// Presence is read so a forced adoption's note can tell an
			// online author the maintainer took over; the request itself
			// does not depend on it.
			presence, hasPresence := c.contributorPresence(ctx, obs.Author)
			n, err := c.adoptContributorPR(ctx, store, key, obs, policy, presence, hasPresence, now)
			result.Comments += n
			if err != nil && firstErr == nil {
				firstErr = err
			}
			if err := c.restageAfterAdoption(ctx, reconciler, store, p); err != nil && firstErr == nil {
				firstErr = err
			}
			// The request is one attempt, not a standing order: clear the
			// label once an attempt was recorded for this head (success or
			// failure) so a persistent write failure re-parks the PR at 085
			// instead of retrying every sweep. A deferral (hold label, fence
			// unreadable) records nothing and keeps the label for a retry. A
			// terminal refusal that recorded no adoption (the App's own PR,
			// a replay that conflicted) is consumed too (AdoptionRequestConsumed).
			if after, ok := store.Get(key); ok && (after.AdoptedBy > 0 || delegatedAdoptionAttempted(before, after) || after.AdoptionRequestConsumed(obs.HeadSHA)) {
				if err := c.removeNonStageLabel(ctx, obs.Owner, obs.Repo, obs.Number, delegatedecision.LabelAdoptSupersede); err != nil && firstErr == nil {
					firstErr = err
				}
			}
		}
		// hivecommons/hive#15307: an adoption attempt that opened nothing
		// pins the PR at 074/085 where this block never ran, so a clanker
		// that stayed offline was never adopted again. The retry runs the
		// adoption half only (adoptionRetryDue gates it on the offline
		// grace); nobody is re-nudged.
		adoptionRetry := false
		if policy.Adoption && !obs.Merged && (want.Stage == stagelabels.AdoptedByHive || want.Stage == stagelabels.NeedsDecision) {
			if rec, ok := store.Get(key); ok && rec.AdoptionAttemptedNothingOpened(obs.HeadSHA) {
				adoptionRetry = true
			}
		}
		if dup := mergeTrainDuplicateOf(obs.Reason); dup > 0 && (want.Stage == stagelabels.ReturnedToContributor || want.Stage == stagelabels.NeedsAuthor) {
			// A duplicate held out of the merge queue
			// (hivecommons/hive#15469) is parked with its author as
			// superseded: no "fix your CI" nudge, no adoption — there is
			// nothing to fix, and adopting it would land the issue twice.
			// Its one notice is the duplicate-fix sweep's gated comment.
			if rec, ok := store.Get(key); !ok || rec.NudgeHead != obs.HeadSHA {
				c.info("stage labels: 07x duplicate held without interrupt", "stage", want.Stage.String(), "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "duplicate_of", dup)
			}
		} else if !hasLabel(obs.Labels, delegatedecision.LabelAdoptSupersede) && (want.Stage == stagelabels.ReturnedToContributor || want.Stage == stagelabels.NeedsAuthor || adoptionRetry) && (policy.ContributorNudge || policy.Adoption) {
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
				if policy.ContributorNudge && !adoptionRetry {
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
					if err := c.restageAfterAdoption(ctx, reconciler, store, p); err != nil && firstErr == nil {
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
		// A person whose 071 is the reviewer's question used to get the 085
		// entry comment; now that it routes to them (hivecommons/hive#15202)
		// they are told whatever their role.
		reviewerAsked := obs.AuthorKind == authorkind.Human && obs.Reason == stagelabels.LaneReasonReviewerQuestion
		if want.Stage == stagelabels.NeedsAuthor && (role == stagelabels.RoleTrusted || reviewerAsked) && obs.AuthorKind != authorkind.Clanker && obs.Reason != stagelabels.LaneReasonHiveSignoffAttribution {
			// Silence is the worst outcome for a trusted person
			// (hivecommons/hive#13636): always on, not gated on policy.
			n, err := c.noteTrustedHuman071(ctx, store, key, obs, now)
			result.Comments += n
			if err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if want.Stage == stagelabels.AdoptedByHive {
			n, err := c.settleAdoption(ctx, store, key, obs, policy, now)
			result.Comments += n
			if err != nil && firstErr == nil {
				firstErr = err
			}
		} else if rec, ok := store.Get(key); ok && rec.AdoptedBy > 0 && !obs.Merged {
			// hivecommons/hive#14471: the superseder is still on record while
			// the lane derives 085 (needs-human) or 072 (a later contributor
			// head). Its merge must still close this PR.
			n, err := c.settleSuperseder(ctx, store, key, obs, rec, policy, now)
			result.Comments += n
			if err != nil && firstErr == nil {
				firstErr = err
			}
		} else if ok && adoptionMarkerScanDue(obs, rec, ok, want.Stage, policy) {
			// hivecommons/hive#15052: the record forgot its adoption but the
			// PR may still wear the marker; settle it from the copy's state.
			n, err := c.recoverOrphanedAdoptionFromMarker(ctx, store, key, obs, policy, now)
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
	// hivecommons/hive#15135: a close-first original is in no lane's
	// listing; its copy is polled from the store.
	if n, err := c.settleCloseFirstSupersessions(ctx, store, policy, now); true {
		result.Comments += n
		if err != nil && firstErr == nil {
			firstErr = err
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
