package github

// Issue stage labels (hivecommons/hive#12436, Part 2 of #12429). PR1 gave
// every pull request a `stage/NNN-*` label computed by the automerge lanes;
// this file does the same for issues from what the enumeration cycle already
// decided about each one — no extra GitHub reads:
//
//   - still in the actionable set            → stage/010-actionable
//   - withheld by a live issue claim         → stage/020-claimed, or
//     stage/030-pr-in-progress when the claim marker is kind=agent
//   - withheld by the in-progress sweep      → stage/030-pr-in-progress
//   - withheld because an open PR covers it  → stage/040-covered-by-pr; the
//     covering PR's own stage (read from the shared stage store the
//     automerge sweep fills) is narrated in the reason, never written as
//     a second label — 050+ are PR-only stages (hivecommons/hive#12451)
//   - parked on a person: carrying stage/045-needs-decision itself (the
//     single decision park signal, hivecommons/hive#12683 — an agent, the
//     relay or a person put it there and it stays until a maintainer un-parks),
//     needs-human, needs-spec, or a retired spelling (the pre-045
//     stage/085-needs-decision, needs-decision, needs-direction)
//     → stage/045-needs-decision, the ISSUE parking stage; the label that
//     parked it is the reason. Owner decision: issues park at 045 and
//     stage/085-needs-decision is PR-only — the issue reconciler can no
//     longer emit 085, and an issue found wearing it is re-staged to 045
//     (Plan removes the stale stage label in the same write).
//   - carrying hive/likely-done (merged PR, awaiting confirmation), or
//     filtered out of the pipeline by any other exempt label → no stage.
//
// The reconciler plans against the labels the list payload already carries
// and writes only the difference, so an unchanged issue costs nothing; when
// a write is due it goes through stagelabels.Enforce, which re-reads the
// issue so exactly one stage label survives (hivecommons/hive#12512). It
// also strips the retired `claimed`, `hive/in-progress` and `🐝 covered by
// PR` labels: one cycle after upgrade every open issue carries its stage
// label instead. The retired `needs-decision` / `needs-direction` park
// labels come off the same way once 045 is on (legacy_decision_labels.go),
// and their definitions are deleted once nothing open carries them. Health (`stage/stalled`) is tracked for the issue's own
// stages (010–030); a covered issue (040) leaves health to the PR, where the
// reason is legible. For those issue stages the dwell is still measured
// from stage entry: the issue list payload carries no comment count or
// activity timestamp the hive did not bump itself (hivecommons/hive#12491),
// so the PR engine's no-activity clock (Store.NoteActivity, require_quiet)
// does not apply here.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// IssueStagePolicy is the live `governor.labels.stage` policy as the issue
// reconciler needs it.
type IssueStagePolicy struct {
	Enabled bool
	// Dwell overrides the stage table's expected dwell per stage slug.
	Dwell map[string]time.Duration
}

// SetIssueStageLabels installs the live policy resolver. Nil disables.
func (c *Client) SetIssueStageLabels(fn func() IssueStagePolicy) {
	if c == nil {
		return
	}
	c.issueStage = fn
}

func (c *Client) issueStagePolicy() IssueStagePolicy {
	if c == nil || c.issueStage == nil {
		return IssueStagePolicy{}
	}
	return c.issueStage()
}

// IssueStageResult counts what one reconcile pass did.
type IssueStageResult struct {
	Items    int
	Writes   int
	Comments int
	// PRBodiesEdited counts Hive-authored PR bodies amended with a closing
	// trailer so GitHub can show the issue↔PR link for default-branch work.
	PRBodiesEdited int
	// MergedPRIssuesClosed counts open issues closed because their claiming
	// Hive PR already merged and the claim was a full-fix claim.
	MergedPRIssuesClosed int
	// Healed counts issues found carrying more than one stage label.
	Healed int
	// ClaimedByHealed counts backend-named and retired per-login
	// `hive/claimed-by-*` labels rewritten to the holder's label (see
	// healBackendClaimedByLabels).
	ClaimedByHealed int
	// LegacyDecisionMigrated counts issues whose retired needs-decision /
	// needs-direction label came off after stage/045 was confirmed on them;
	// NeedsDecisionRestaged counts issues moved from the pre-045
	// stage/085-needs-decision to stage/045-needs-decision;
	// LegacyDecisionDefinitionsDeleted counts repo label definitions removed
	// (hivecommons/hive#12683).
	LegacyDecisionMigrated           int
	NeedsDecisionRestaged            int
	LegacyDecisionDefinitionsDeleted int
	// ParentsClosed counts decomposed parents closed because every tracked
	// child was closed; ParentsAwaitingChildren counts parents with an open
	// child that were therefore exempt from stage/stalled
	// (hivecommons/hive#12857).
	ParentsClosed           int
	ParentsAwaitingChildren int
}

// issueStageRESTCaller tags the writes for /api/gh-rate-limits.
const issueStageRESTCaller = "hive:issue_stage_labels"

// Work-detail buckets the reconciler maps (see appendRepoWorkIssueDetail).
const (
	workBucketClaimed     = "claimed"
	workBucketClaimedByPR = "claimed_by_pr"
	workBucketFiltered    = "filtered"
)

// IssueStageInput is the one shape both actionable items and work-detail
// rows reduce to before classification.
type IssueStageInput struct {
	Repo      string
	Number    int
	Labels    []string
	Bucket    string // "" for an issue still in the actionable set
	Reason    string // the bucket's reason; for "filtered", the label that parked it
	ClaimKind string
	ClaimedBy string
	Claim     *IssueClaimContext
	// Children is set on a decomposed parent (hivecommons/hive#12857).
	Children *IssueChildren
	// Held is set for an issue the enumeration put in the Hold list
	// (Client.isHeld: the hive's hold-label matcher, including the
	// configured hive-pause/<id> label). IssueStageFor also reads the
	// labels directly so a caller without the enumeration context agrees
	// (hivecommons/hive#13012).
	Held bool
}

// IssueStageFor classifies one enumerated issue. covered reports whether an
// open PR the lanes have staged covers it (health then belongs to the PR,
// whose stage the reason names). prStage resolves the covering PR's current
// stage from the shared store. The issue itself never goes past
// 040-covered-by-pr: 050+ are PR stages (stagelabels.Table, PR: true) and
// mirroring them onto the issue is what left hivecommons/hive#12451 wearing
// stage/050-review-pending next to its own stages (hivecommons/hive#12451).
func IssueStageFor(in IssueStageInput, prStage func(repo string, number int) stagelabels.Stage) (stage stagelabels.Stage, covered bool, reason string) {
	if issueHasLabel(in.Labels, LikelyDoneLabel) {
		return stagelabels.None, false, "merged PR awaiting confirmation"
	}
	// A held issue is paused, not parked on a person: it shows as
	// "Open · on hold", and hold outranks needs-human / needs-decision /
	// needs-spec (hivecommons/hive#13012). PR holds keep 085 (RouteHuman).
	if label := issueHoldLabel(in); label != "" {
		return stagelabels.None, false, "held: " + label
	}
	if issueHeldByNeedsHuman(in) {
		return stagelabels.IssueNeedsDecision, false, "held for a person: " + issueNeedsHumanLabel
	}
	if label := parkedOnPersonLabel(in); label != "" {
		return stagelabels.IssueNeedsDecision, false, "parked on a person: " + label
	}
	switch in.Bucket {
	case "":
		return stagelabels.Actionable, false, "actionable"
	case workBucketClaimed:
		if strings.EqualFold(in.ClaimKind, string(claims.KindAgent)) {
			return stagelabels.PRInProgress, false, "live agent claim"
		}
		return stagelabels.Claimed, false, "live issue claim"
	case inProgressWorkBucket:
		return stagelabels.PRInProgress, false, "in-progress start mark"
	case workBucketClaimedByPR:
		if in.Claim != nil && in.Claim.PRNumber > 0 && prStage != nil {
			prRepo := in.Claim.PRRepo
			if prRepo == "" {
				prRepo = in.Repo
			}
			if s := prStage(prRepo, in.Claim.PRNumber); s != stagelabels.None && !s.Before(stagelabels.CoveredByPR) {
				return stagelabels.CoveredByPR, true, "open PR #" + strconv.Itoa(in.Claim.PRNumber) + " covers it (PR at " + s.String() + ")"
			}
		}
		return stagelabels.CoveredByPR, false, "open PR covers it"
	}
	return stagelabels.None, false, "outside the pipeline: " + in.Bucket
}

// issueHoldLabel returns the hold-class label that pauses the issue, or "".
// The enumeration's verdict (in.Held, Client.isHeld with the configured
// hive-pause/<id> label) is authoritative; the label scan covers the
// static hold family — HoldLabels (hold, on-hold, hold/review) as
// HasHoldLabel reads them and prow's do-not-merge(/*) — so a direct
// caller agrees with the dashboard's hold modifier. hive-pause/<id> is
// only a hold for the hive it names, so it is read from in.Held alone.
func issueHoldLabel(in IssueStageInput) string {
	for _, l := range in.Labels {
		lower := strings.ToLower(strings.TrimSpace(l))
		if lower == "do-not-merge" || strings.HasPrefix(lower, "do-not-merge/") || HasHoldLabel([]string{l}) {
			return lower
		}
	}
	if in.Held {
		for _, l := range in.Labels {
			if lower := strings.ToLower(strings.TrimSpace(l)); strings.HasPrefix(lower, "hive-pause/") {
				return lower
			}
		}
		return "hold"
	}
	return ""
}

func issueHeldByNeedsHuman(in IssueStageInput) bool {
	return hasIssueNeedsHumanLabel(in.Labels) ||
		in.Bucket == workBucketFiltered && strings.EqualFold(strings.TrimSpace(in.Reason), issueNeedsHumanLabel)
}

// parkedOnPersonLabel returns the escalation label that parks the issue on
// a person, or "". A parked issue is hard-suppressed at enumeration (bucket
// "filtered", reason = the configured label's canonical name), so the label
// is read from the labels the issue carries and, for an operator-renamed
// label, from the bucket reason. hivecommons/hive#12481 carried
// needs-direction and stage/030-pr-in-progress together for five hours: the
// reconciler cleared the stage to none, which read as "actionable" to a
// person while the hive had in fact handed it to one.
func parkedOnPersonLabel(in IssueStageInput) string {
	if label := hardSuppressIssueLabel(in.Labels); label != "" {
		return label
	}
	if in.Bucket == workBucketFiltered && hardSuppressIssueLabel([]string{in.Reason}) != "" {
		return in.Reason
	}
	return ""
}

// IssueStageInputs flattens an enumeration result into one row per open
// issue the cycle saw, actionable items first, then every work-detail row.
func IssueStageInputs(result *ActionableResult) []IssueStageInput {
	if result == nil {
		return nil
	}
	var out []IssueStageInput
	for _, issue := range result.Issues.Items {
		out = append(out, IssueStageInput{Repo: issue.Repo, Number: issue.Number, Labels: issue.Labels, ClaimKind: issue.ClaimKind, ClaimedBy: issue.ClaimedBy, Claim: issue.ClaimContext, Children: issue.Children})
	}
	// Held issues leave the pipeline at enumeration (Hold list, no
	// work-detail row). They still have to be walked so a stage label
	// written before the hold — #9846 wore stage/045-needs-decision beside
	// `hold` — comes off and the store record is forgotten
	// (hivecommons/hive#13012).
	for _, held := range result.Hold.Items {
		if held.Number <= 0 || !strings.EqualFold(held.Type, "issue") {
			continue
		}
		out = append(out, IssueStageInput{Repo: held.Repo, Number: held.Number, Labels: held.Labels, Held: true})
	}
	repos := make([]string, 0, len(result.WorkDetailsByRepo))
	for repo := range result.WorkDetailsByRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		for _, row := range result.WorkDetailsByRepo[repo].Issues {
			if row.Number <= 0 {
				continue
			}
			out = append(out, IssueStageInput{Repo: row.Repo, Number: row.Number, Labels: row.Labels, Bucket: row.Bucket, Reason: row.Reason, ClaimKind: row.ClaimKind, ClaimedBy: row.ClaimedBy, Claim: row.ClaimContext, Children: row.Children})
		}
	}
	return out
}

// SyncIssueStageLabels reconciles the stage labels on every open issue the
// enumeration cycle saw. It runs after the claim guards and the in-progress
// sweep so the buckets are final. store may be nil (the shared store is
// used). Best-effort: a failed write is logged and retried next cycle.
func SyncIssueStageLabels(ctx context.Context, client *Client, result *ActionableResult, store *stagelabels.Store, now time.Time, logger *slog.Logger) IssueStageResult {
	return SyncIssueStageLabelsWith(ctx, client, result, store, now, logger, nil)
}

// issueStagePending is one issue the sweep derived a stage for, waiting on
// the reconciler's write before its post-write work (legacy park-label
// migration, reconcile log, regression note) runs.
type issueStagePending struct {
	in      IssueStageInput
	key     string
	want    stagelabels.Desired
	tr      stagelabels.Transition
	covered bool
	reason  string
	facts   stageengine.Facts
}

// SyncIssueStageLabelsWith is SyncIssueStageLabels with the stage engine's
// options chosen by the caller (ADR-0025). Nil engine is the default:
// shadow mode with this sweep's derivation as the legacy comparator, so
// the labels written are exactly what the sweep derived. Every stage write
// goes through client.StageReconciler → StageWriter; nothing here names a
// label to the forge.
func SyncIssueStageLabelsWith(ctx context.Context, client *Client, result *ActionableResult, store *stagelabels.Store, now time.Time, logger *slog.Logger, engine *stageengine.Options) IssueStageResult {
	var res IssueStageResult
	if client == nil || result == nil {
		return res
	}
	policy := client.issueStagePolicy()
	if !policy.Enabled {
		return res
	}
	if store == nil {
		store = stagelabels.Shared()
	}
	reconciler, legacy := client.StageReconciler(store, engine, logger)
	ctx = WithRESTCaller(ctx, issueStageRESTCaller)
	prStage := func(repo string, number int) stagelabels.Stage {
		owner, name := client.splitRepo(repo)
		rec, ok := store.Get(stagelabels.Key(owner+"/"+name, number))
		if !ok {
			return stagelabels.None
		}
		return rec.Stage
	}
	seen := map[string]bool{}
	legacyCarriers := map[string]int{}
	reposSeen := map[string]bool{}
	var pending []issueStagePending
	for _, in := range IssueStageInputs(result) {
		owner, name := client.splitRepo(in.Repo)
		key := stagelabels.Key(owner+"/"+name, in.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		res.Items++
		reposSeen[in.Repo] = true
		if hasLegacyDecisionLabel(in.Labels) {
			legacyCarriers[in.Repo]++
		}
		res.ClaimedByHealed += healBackendClaimedByLabels(ctx, client, in, logger)
		// A decomposed parent is waiting on its children, not on itself
		// (hivecommons/hive#12857): all closed → close it; any open → it
		// keeps its stage but is never marked stalled.
		awaitingChildren := false
		if in.Children != nil && in.Children.Total > 0 {
			if in.Children.AllClosed() && client.closeDecomposedParent(ctx, in, store, key, logger) {
				res.ParentsClosed++
				res.Writes++
				res.Comments++
				continue
			}
			awaitingChildren = true
			res.ParentsAwaitingChildren++
		}
		if closed, commented := handleMergedPRIssueStage(ctx, client, in, logger); closed {
			res.MergedPRIssuesClosed++
			res.Writes++
			if commented {
				res.Comments++
			}
			store.Forget(key)
			continue
		} else if commented {
			res.Comments++
		}
		if in.Claim != nil && in.Claim.MergedPR {
			in.Bucket = ""
			in.ClaimKind = ""
			in.ClaimedBy = ""
		}
		stage, covered, reason := IssueStageFor(in, prStage)
		if awaitingChildren && stage != stagelabels.None {
			reason += "; waiting on " + strconv.Itoa(in.Children.Open()) + " open child issue(s)"
		}
		var want stagelabels.Desired
		var tr stagelabels.Transition
		if stage == stagelabels.None {
			store.Forget(key)
		} else {
			tr = store.Observe(key, stage, reason)
			rec, _ := store.Get(key)
			want = stagelabels.Desired{Stage: stage}
			if !covered && !awaitingChildren {
				want.Stalled = stagelabels.Stalled(rec, policy.Dwell, now)
			}
		}
		if stage == stagelabels.PRInProgress || stage == stagelabels.CoveredByPR {
			edited, commented := ensureIssueStagePRLink(ctx, client, in, store, key, logger)
			if edited {
				res.PRBodiesEdited++
				res.Writes++
			}
			if commented {
				res.Comments++
			}
		}
		add, remove := stagelabels.Plan(in.Labels, want)
		if len(add)+len(remove) == 0 {
			// Stage already right; a retired park label left beside 045
			// still has to come off (hivecommons/hive#12683).
			if n := removeLegacyDecisionLabels(ctx, client, in, in.Labels, logger); n > 0 {
				res.LegacyDecisionMigrated += n
				legacyCarriers[in.Repo]--
			}
			continue
		}
		if err := client.ensureStageLabelFamily(ctx, in.Repo); err != nil {
			if logger != nil {
				logger.Warn("issue stage labels: provisioning failed", "repo", in.Repo, "error", err)
			}
			continue
		}
		facts := issueFactsFrom(in, prStage)
		facts.Ref.Owner, facts.Ref.Repo = owner, name
		facts.ObservedAt = now
		facts.Issue.Stalled = want.Stalled
		legacy.Set(facts.Ref, stageengine.Decision{Stage: want.Stage, Stalled: want.Stalled, Terminal: stage == stagelabels.None, Reason: reason})
		pending = append(pending, issueStagePending{in: in, key: key, want: want, tr: tr, covered: covered, reason: reason, facts: facts})
	}
	// The write: one ReconcileBatch for the pass. The reconciler re-reads
	// each issue before writing so a stage a claim hook placed since the
	// list payload was fetched is replaced, not joined
	// (hivecommons/hive#12512), heals duplicates, and IssueItem clamps the
	// stage to the issue stages (hivecommons/hive#12451).
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
		in, key, want, tr, covered, reason := p.in, p.key, p.want, p.tr, p.covered, p.reason
		var out stagelabels.Outcome
		err := ctx.Err()
		if i < len(transitions) {
			out, err = transitions[i].Outcome, transitions[i].Err
		}
		if err != nil && logger != nil {
			logger.Warn("issue stage labels: write failed", "repo", in.Repo, "issue", in.Number, "stage", want.Stage.String(), "error", err)
		}
		res.Writes += len(out.Removed)
		if len(out.Added) > 0 {
			res.Writes++
		}
		if out.Healed {
			res.Healed++
		}
		if out.Stage == stagelabels.IssueNeedsDecision && slices.Contains(out.Removed, legacyIssueNeedsDecisionStageLabel) {
			res.NeedsDecisionRestaged++
		}
		if err == nil && out.Stage == stagelabels.IssueNeedsDecision {
			// 045 is on the issue (written or confirmed); the retired park
			// label that used to carry the meaning can go.
			if n := removeLegacyDecisionLabels(ctx, client, in, append(append([]string(nil), in.Labels...), out.Added...), logger); n > 0 {
				res.LegacyDecisionMigrated += n
				legacyCarriers[in.Repo]--
			}
		}
		if logger != nil && (out.Changed() || err != nil) {
			logger.Info("issue stage labels reconciled", "repo", in.Repo, "issue", in.Number, "stage", out.Stage.String(), "added", out.Added, "removed", out.Removed, "healed", out.Healed, "clamped", out.Clamped, "wanted", out.Wanted.String(), "reason", reason, "bucket", in.Bucket)
		}
		// A regression between the issue's OWN stages (a claim or kick that
		// lapsed without a PR) is the hive's failure and gets the one-line
		// note; a covering PR's own falls are narrated on the PR.
		if tr.Regressed && !covered && tr.Prev.Before(stagelabels.CoveredByPR) {
			if rec, ok := store.Get(key); ok && !rec.RegressionCommented {
				if err := client.CreateIssueComment(ctx, in.Repo, in.Number, stagelabels.RegressionComment(tr.Prev, tr.Next, reason)); err != nil {
					if logger != nil {
						logger.Warn("issue stage labels: regression comment failed", "repo", in.Repo, "issue", in.Number, "error", err)
					}
				} else {
					store.MarkRegressionCommented(key)
					res.Comments++
				}
			}
		}
	}
	for repo := range reposSeen {
		if legacyCarriers[repo] <= 0 {
			res.LegacyDecisionDefinitionsDeleted += client.retireLegacyDecisionLabelDefinitions(ctx, repo, logger)
		}
	}
	if err := store.Save(); err != nil && logger != nil {
		logger.Warn("issue stage labels: persisting state failed", "error", err)
	}
	return res
}

const mergedPRPartialStageMarker = "<!-- hive:issue-stage-merged-partial"

// mergedPRClosedStageMarker prefixes the one comment the issue-stage lane
// posts before closing an issue whose merged Hive PR was the full fix. The
// lane checks for it before commenting again, so a close that fails after the
// comment landed is retried without a second comment.
const mergedPRClosedStageMarker = "<!-- hive:closed-by-merged-pr"

func handleMergedPRIssueStage(ctx context.Context, client *Client, in IssueStageInput, logger *slog.Logger) (closed, commented bool) {
	if client == nil || client.client == nil || in.Claim == nil || !in.Claim.MergedPR || in.Claim.PRNumber <= 0 {
		return false, false
	}
	if in.Claim.ExternalAuthor {
		return false, false
	}
	if in.Claim.Reference {
		if !issueHasLabel(in.Labels, stagelabels.PRInProgress.Label()) && !issueHasLabel(in.Labels, stagelabels.CoveredByPR.Label()) {
			return false, false
		}
		marker := fmt.Sprintf("%s pr=%d -->", mergedPRPartialStageMarker, in.Claim.PRNumber)
		if ok, err := client.IssueCommentsContain(ctx, in.Repo, in.Number, marker); err == nil && ok {
			return false, false
		} else if err != nil && logger != nil {
			logger.Warn("issue stage labels: could not check merged partial PR stage comment; posting a fresh note", "repo", in.Repo, "issue", in.Number, "pr", in.Claim.PRNumber, "error", err)
		}
		url := strings.TrimSpace(in.Claim.PRURL)
		if url == "" {
			url = fmt.Sprintf("#%d", in.Claim.PRNumber)
		}
		body := fmt.Sprintf("%s\nPR #%d has merged: %s\n\nHive is moving this issue back to the actionable lane because the PR used a non-closing reference (`Refs` / `Part of`) and therefore landed partial work without closing the issue.", marker, in.Claim.PRNumber, url)
		if err := client.CreateIssueComment(ctx, in.Repo, in.Number, body); err != nil {
			if logger != nil {
				logger.Warn("issue stage labels: merged partial PR comment failed", "repo", in.Repo, "issue", in.Number, "pr", in.Claim.PRNumber, "error", err)
			}
			return false, false
		}
		return false, true
	}
	if !in.Claim.Closing {
		return false, false
	}
	marker := fmt.Sprintf("%s pr=%d -->", mergedPRClosedStageMarker, in.Claim.PRNumber)
	alreadyCommented, err := client.IssueCommentsContain(ctx, in.Repo, in.Number, marker)
	if err != nil && logger != nil {
		logger.Warn("issue stage labels: could not check merged full-fix PR close comment; posting a fresh note", "repo", in.Repo, "issue", in.Number, "pr", in.Claim.PRNumber, "error", err)
	}
	if !alreadyCommented {
		body := fmt.Sprintf("%s\nFixed by #%d (merged). Closing because Hive's PR claim metadata says this PR was the full fix.", marker, in.Claim.PRNumber)
		if strings.TrimSpace(in.Claim.PRURL) != "" {
			body = fmt.Sprintf("%s\nFixed by #%d (%s, merged). Closing because Hive's PR claim metadata says this PR was the full fix.", marker, in.Claim.PRNumber, in.Claim.PRURL)
		}
		if err := client.CreateIssueComment(ctx, in.Repo, in.Number, body); err != nil {
			if logger != nil {
				logger.Warn("issue stage labels: merged full-fix PR close comment failed", "repo", in.Repo, "issue", in.Number, "pr", in.Claim.PRNumber, "error", err)
			}
			return false, false
		}
		commented = true
	}
	if err := client.CloseIssue(ctx, in.Repo, in.Number, IssueCloseOptions{OverrideReason: fmt.Sprintf("merged Hive-authored PR #%d is the full fix", in.Claim.PRNumber), SuppressOverrideComment: true, StateReason: IssueStateReasonCompleted}); err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: merged full-fix PR issue close failed", "repo", in.Repo, "issue", in.Number, "pr", in.Claim.PRNumber, "error", err)
		}
		return false, commented
	}
	return true, commented
}

func ensureIssueStagePRLink(ctx context.Context, client *Client, in IssueStageInput, store *stagelabels.Store, key string, logger *slog.Logger) (edited, commented bool) {
	if client == nil || client.client == nil || in.Claim == nil || in.Claim.PRNumber <= 0 {
		return false, false
	}
	// Older tests and payloads may carry only a PR number. Real claim rows
	// include URL/repo/grade metadata; require at least one such field before
	// spending a PR lookup.
	if in.Claim.PRURL == "" && in.Claim.PRRepo == "" && in.Claim.PRAuthor == "" && !in.Claim.Closing && !in.Claim.Reference {
		return false, false
	}
	link, err := client.EnsureIssueStagePRLink(ctx, in.Repo, in.Number, in.Claim)
	if err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: PR issue-link safeguard failed", "repo", in.Repo, "issue", in.Number, "pr", in.Claim.PRNumber, "error", err)
		}
		return false, false
	}
	if link.EditedBody {
		if logger != nil {
			logger.Info("issue stage labels: appended closing keyword to Hive-authored PR body for GitHub issue link", "repo", in.Repo, "issue", in.Number, "pr", in.Claim.PRNumber, "base", link.Base)
		}
		return true, false
	}
	if strings.TrimSpace(link.StageComment) == "" {
		return false, false
	}
	rec, ok := store.Get(key)
	if !ok || rec.EntryCommented {
		return false, false
	}
	if err := client.CreateIssueComment(ctx, in.Repo, in.Number, link.StageComment); err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: PR link stage comment failed", "repo", in.Repo, "issue", in.Number, "pr", in.Claim.PRNumber, "error", err)
		}
		return false, false
	}
	store.MarkEntryCommented(key, "")
	return false, true
}

// ensureStageLabelFamily provisions (or recolours) the stage/author label
// family in a repo once per process: one list call, then one create or
// patch per label that is missing or whose colour drifted.
func (c *Client) ensureStageLabelFamily(ctx context.Context, repo string) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	key := strings.ToLower(owner + "/" + name)
	if c.stageFamily.has(key) {
		return nil
	}
	existing := map[string]*gh.Label{}
	opts := &gh.ListOptions{PerPage: 100}
	for {
		labels, resp, err := c.client.Issues.ListLabels(ctx, owner, name, opts)
		if err != nil {
			return err
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
			_, _, err := c.client.Issues.CreateLabel(ctx, owner, name, &gh.Label{Name: gh.Ptr(def.Name), Color: gh.Ptr(def.Color), Description: gh.Ptr(def.Description)})
			if err != nil && !githubStatusError(err, http.StatusUnprocessableEntity) {
				return err
			}
		case !strings.EqualFold(cur.GetColor(), def.Color):
			if _, _, err := c.client.Issues.EditLabel(ctx, owner, name, cur.GetName(), &gh.Label{Name: gh.Ptr(def.Name), Color: gh.Ptr(def.Color), Description: gh.Ptr(def.Description)}); err != nil {
				return err
			}
		}
	}
	c.stageFamily.mark(key)
	c.coveredByPRLabel.mark(key)
	return nil
}
