package github

// Issue stage labels (hivecommons/hive#12436, Part 2 of #12429). PR1 gave
// every pull request a `stage/NNN-*` label computed by the automerge lanes;
// this file does the same for issues from what the enumeration cycle already
// decided about each one — no extra GitHub reads:
//
//   - still in the actionable set            → stage/010-actionable, or
//     stage/015-triage-needed when the #9665 reporter gate is holding it:
//     it carries the reporter-trust awaiting label (needs-triage) or sits
//     in the gate's reporter_triage bucket (issueUntriaged). Issues the
//     hive or an agent files, and issues from trusted humans, are triaged
//     by construction and stay 010 — 015 is never derived from who filed
//     it. 015 is a tracking state, not a gate: the gate withholds the
//     issue; the scanner claims from 015 exactly as from 010 once it lets
//     go.
//   - withheld by a live issue claim or by the in-progress sweep (a live
//     kind=agent start mark)                 → stage/020-claimed, or
//     stage/030-pr-in-progress only when the claim carries PR evidence
//     (pr_started= on the marker: the holder's `hive-open-pr --issues`
//     request named the issue, or its start marker named a branch / PR).
//     The label promises a PR; a bare agent claim — a `hive-open-issue
//     claim` with nothing pushed — never wears it (hivecommons/hive#13069)
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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/config"
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
	// workBucketReporterTriage is a human-filed issue the #9665 reporter-
	// trust gate holds until a maintainer adds the triage label
	// (stageengine.BucketReporterTriage).
	workBucketReporterTriage = string(stageengine.BucketReporterTriage)
	// reporterTriageAwaitingReason is the work-detail note on an issue in
	// that bucket, from the full pass and the webhook re-read alike.
	reporterTriageAwaitingReason = "Awaiting maintainer triage label"
)

// Triage evidence for stage/015-triage-needed (stagelabels.TriageNeeded):
// the reporter-trust gate's awaiting label (`reporter_trust.awaiting_label`).
var triageAwaitingLabel = config.DefaultReporterTrustAwaitingLabel

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
	// PRStarted is the live claim's PR evidence (Issue.ClaimPRStarted /
	// stageengine.ClaimFact.PRStarted): the only thing that stages a
	// claimed issue 030-pr-in-progress rather than 020-claimed.
	PRStarted bool
	Claim     *IssueClaimContext
	// Children is set on a decomposed parent (hivecommons/hive#12857).
	Children *IssueChildren
	// DelegatesWork mirrors the watcher's unchecked issue-task predicate.
	// Unlike Children, it includes body-only delegation without ledger entries.
	DelegatesWork bool
	// Held is set for an issue the enumeration put in the Hold list
	// (Client.isHeld: the hive's hold-label matcher, including the
	// configured hive-pause/<id> label). IssueStageFor also reads the
	// labels directly so a caller without the enumeration context agrees
	// (hivecommons/hive#13012).
	Held bool
	// AuthorKind is who filed the issue, as the enumeration judged it
	// (issueAuthorKind: Human, Hive, or Unknown when no login was seen).
	// Carried through to the stage engine's AuthorFacts; the 010/015
	// derivation does not read it.
	AuthorKind authorkind.Kind
}

// issueUntriaged reports whether an actionable issue derives
// 015-triage-needed instead of 010: it carries the reporter-trust awaiting
// label or sits in the gate's reporter_triage bucket. Who filed the issue
// is deliberately not consulted — hive-, agent- and trusted-human-filed
// issues are 010 (owner decision, hivecommons/hive#13287 follow-up).
func issueUntriaged(in IssueStageInput) (untriaged bool, why string) {
	if issueHasLabel(in.Labels, triageAwaitingLabel) {
		return true, "carries " + triageAwaitingLabel
	}
	if in.Bucket == workBucketReporterTriage {
		return true, "reporter-trust gate: awaiting the triage label"
	}
	return false, ""
}

// IssueStageFor classifies one enumerated issue: it resolves the hive's
// label families into stageengine.Facts (issueFactsFrom) and asks the
// engine (stageengine.DeriveIssue) for the stage. covered reports whether
// an open PR the lanes have staged covers it (health then belongs to the
// PR, whose stage the reason names). prStage resolves the covering PR's
// current stage from the shared store. A decomposed parent's reason
// carries the "waiting on N open child issue(s)" suffix.
func IssueStageFor(in IssueStageInput, prStage func(repo string, number int) stagelabels.Stage) (stage stagelabels.Stage, covered bool, reason string) {
	d := issueStageDecision(in, prStage)
	return d.Stage, d.Covered, d.Reason
}

// issueStageDecision is IssueStageFor with the whole Decision: the sweep
// also needs Progress, which tells the store that a 040 → 010 fall came
// from the covering PR merging (hive/partially-addressed) rather than
// closing unmerged.
func issueStageDecision(in IssueStageInput, prStage func(repo string, number int) stagelabels.Stage) stageengine.Decision {
	return stageengine.DeriveIssue(issueFactsFrom(in, prStage))
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
// possiblyExistsParked reports the prior-art gate's park: the 017 label on
// the issue, or an enumeration that filtered the issue for it.
func possiblyExistsParked(in IssueStageInput) bool {
	if issueHasLabel(in.Labels, PossiblyExistsStageLabel) {
		return true
	}
	return in.Bucket == workBucketFiltered && strings.EqualFold(strings.TrimSpace(in.Reason), PossiblyExistsStageLabel)
}

func parkedOnPersonLabel(in IssueStageInput) string {
	if label := hardSuppressIssueLabel(in.Labels); label != "" {
		return label
	}
	if in.Bucket == workBucketFiltered && hardSuppressIssueLabel([]string{in.Reason}) != "" {
		return in.Reason
	}
	return ""
}

// issueAuthorKind is the stage input's author kind from the enumeration's
// verdict (Client.isHumanAuthor): Human when it said so, Hive — the App
// bot, a `[bot]` login or a hive identity — when it did not. The verdict
// is a tri-state like isKnownNonHumanAuthor: a row with no author login
// at all ("we could not tell") is Unknown, never "a bot filed it".
func issueAuthorKind(issue Issue) authorkind.Kind {
	switch {
	case issue.AuthorIsHuman:
		return authorkind.Human
	case strings.TrimSpace(issue.Author) == "":
		return authorkind.Unknown
	default:
		return authorkind.Hive
	}
}

// forgeIssueLabels returns the labels GitHub actually returned for the
// issue. annotateIssueWithClaim adds stage/040-covered-by-pr to the
// in-memory copy of an issue an open PR covers; planning against that
// copy made Plan see the stage as already present, so the sweep that owns
// the write never wrote it — hivecommons/hive#13221 lost its 040 to a
// claim release and no cycle restored it (hivecommons/hive#13316).
func forgeIssueLabels(labels []string, claim *IssueClaimContext) []string {
	if claim == nil || !claim.CoveredLabelSynthesized {
		return labels
	}
	return removeIssueLabel(slices.Clone(labels), CoveredByPRLabel)
}

// IssueStageInputs flattens an enumeration result into one row per open
// issue the cycle saw, actionable items first, then every work-detail row.
func IssueStageInputs(result *ActionableResult) []IssueStageInput {
	if result == nil {
		return nil
	}
	var out []IssueStageInput
	for _, issue := range result.Issues.Items {
		out = append(out, IssueStageInput{Repo: issue.Repo, Number: issue.Number, Labels: forgeIssueLabels(issue.Labels, issue.ClaimContext), ClaimKind: issue.ClaimKind, ClaimedBy: issue.ClaimedBy, PRStarted: issue.ClaimPRStarted, Claim: issue.ClaimContext, Children: issue.Children, AuthorKind: issueAuthorKind(issue), DelegatesWork: hasUnfinishedDelegatedWork(issue.Body)})
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
			out = append(out, IssueStageInput{Repo: row.Repo, Number: row.Number, Labels: forgeIssueLabels(row.Labels, row.ClaimContext), Bucket: row.Bucket, Reason: row.Reason, ClaimKind: row.ClaimKind, ClaimedBy: row.ClaimedBy, PRStarted: row.ClaimPRStarted, Claim: row.ClaimContext, Children: row.Children})
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
	// Expiry of the best-effort sweep budget is not an item write failure.
	// Report it once, without feeding a warning per unattempted issue into
	// the repeated-error self-reporter. The next enumeration retries it.
	var batchErr error
	defer func() {
		err := ctx.Err()
		if err == nil && (errors.Is(batchErr, context.DeadlineExceeded) || errors.Is(batchErr, context.Canceled)) {
			err = batchErr
		}
		if err != nil && logger != nil {
			logger.Debug("issue stage labels: sweep interrupted; retrying next cycle", "error", err)
		}
	}()
	policy := client.issueStagePolicy()
	if !policy.Enabled {
		return res
	}
	if store == nil {
		store = stagelabels.Shared()
	}
	// Persist observations even when the sweep expires before reconciliation
	// or legacy cleanup. Saving local state does not need the sweep context.
	defer func() {
		if err := store.Save(); err != nil && logger != nil {
			logger.Warn("issue stage labels: persisting state failed", "error", err)
		}
	}()
	reconciler := client.StageReconciler(store, engine, logger)
	ctx = WithRESTCaller(ctx, issueStageRESTCaller)
	// The closes this pass may perform (decomposed parent, merged full-fix
	// PR) are irreversible; bound them to a fresh, non-stale view of the
	// issue (decision_freshness.go).
	ctx = WithIrreversibleGuard(ctx, client.SnapshotFreshnessGuard(result, func() time.Time { return now }))
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
	linkRateLimited := false
	linkSkipped := 0
	defer func() {
		if linkRateLimited && logger != nil {
			logger.Info("issue stage labels: PR issue-link safeguard paused for this sweep, GitHub rate limit exceeded; retrying next sweep", "skipped_pairs", linkSkipped)
		}
	}()
	for _, in := range IssueStageInputs(result) {
		if ctx.Err() != nil {
			return res
		}
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
			in.PRStarted = false
		}
		d := issueStageDecision(in, prStage)
		stage, covered, reason := d.Stage, d.Covered, d.Reason
		var want stagelabels.Desired
		var tr stagelabels.Transition
		if stage == stagelabels.None {
			store.ForgetStage(key)
		} else {
			tr = store.ObserveProgress(key, stage, reason, d.Progress)
			noteIssueCoveringPR(client, store, key, in, stage)
			rec, _ := store.Get(key)
			want = stagelabels.Desired{Stage: stage}
			if !covered && !awaitingChildren {
				want.Stalled = stagelabels.Stalled(rec, policy.Dwell, now)
			}
			// A claimed / covered issue still wearing the reporter-trust
			// awaiting label is stale (015 is not a gate): the engine's
			// Decision.Clear takes it off in the stage write, and the
			// pre-plan here must see the same write as due so an issue
			// whose stage label is already right is still reconciled
			// (hivecommons/hive#14586).
			if stage.SettlesTriage() && issueHasLabel(in.Labels, triageAwaitingLabel) {
				want.Clear = []string{triageAwaitingLabel}
			}
		}
		if stage == stagelabels.PRInProgress || stage == stagelabels.CoveredByPR {
			var edited, commented bool
			if linkRateLimited {
				linkSkipped++
			} else {
				edited, commented, linkRateLimited = ensureIssueStagePRLink(ctx, client, in, store, key, logger)
			}
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
		if client.stageArchived.has(strings.ToLower(owner + "/" + name)) {
			continue
		}
		if err := client.ensureStageLabelFamily(ctx, in.Repo); err != nil {
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return res
			}
			if logger != nil {
				if client.stageArchived.has(strings.ToLower(owner + "/" + name)) {
					logger.Warn("issue stage labels: repository is archived and read-only, skipping it until restart; remove it from the configured repos", "repo", in.Repo, "error", err)
				} else {
					logger.Warn("issue stage labels: provisioning failed", "repo", in.Repo, "error", err)
				}
			}
			continue
		}
		facts := issueFactsFrom(in, prStage)
		facts.Ref.Owner, facts.Ref.Repo = owner, name
		facts.ObservedAt = now
		facts.Issue.Stalled = want.Stalled
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
		transitions, batchErr = reconciler.ReconcileBatch(ctx, facts)
	}
	for i, p := range pending {
		// ReconcileBatch stops on cancellation and returns only attempted
		// items. Never invent write failures for its unattempted tail.
		if i >= len(transitions) {
			break
		}
		in, key, want, tr, covered, reason := p.in, p.key, p.want, p.tr, p.covered, p.reason
		out, err := transitions[i].Outcome, transitions[i].Err
		interrupted := errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
		// A live-context request timeout still needs one visible diagnostic;
		// stopping the batch below prevents per-item warning fan-out.
		if err != nil && (!interrupted || ctx.Err() == nil) && logger != nil {
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
		// Keep partial write counts, but stop on pacer fail-fast as well as
		// actual sweep expiry; follow-up writes need the same budget.
		if interrupted {
			return res
		}
		if ctx.Err() != nil {
			continue
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
	if ctx.Err() != nil {
		return res
	}
	for repo := range reposSeen {
		if legacyCarriers[repo] <= 0 {
			res.LegacyDecisionDefinitionsDeleted += client.retireLegacyDecisionLabelDefinitions(ctx, repo, logger)
		}
	}
	return res
}

// noteIssueCoveringPR mirrors stageengine's covering-PR note for the
// sweep's own Observe (which runs whether or not a label write follows):
// the ledger's claiming PR while the issue is at 040, nothing otherwise.
func noteIssueCoveringPR(client *Client, store *stagelabels.Store, key string, in IssueStageInput, stage stagelabels.Stage) {
	if stage != stagelabels.CoveredByPR {
		store.SetCoveringPR(key, "")
		return
	}
	if in.Claim == nil || in.Claim.PRNumber <= 0 {
		return
	}
	prRepo := in.Claim.PRRepo
	if prRepo == "" {
		prRepo = in.Repo
	}
	owner, name := client.splitRepo(prRepo)
	store.SetCoveringPR(key, stagelabels.Key(owner+"/"+name, in.Claim.PRNumber))
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

func ensureIssueStagePRLink(ctx context.Context, client *Client, in IssueStageInput, store *stagelabels.Store, key string, logger *slog.Logger) (edited, commented, rateLimited bool) {
	if client == nil || client.client == nil || in.Claim == nil || in.Claim.PRNumber <= 0 {
		return false, false, false
	}
	// Older tests and payloads may carry only a PR number. Real claim rows
	// include URL/repo/grade metadata; require at least one such field before
	// spending a PR lookup.
	if in.Claim.PRURL == "" && in.Claim.PRRepo == "" && in.Claim.PRAuthor == "" && !in.Claim.Closing && !in.Claim.Reference {
		return false, false, false
	}
	claim := in.Claim
	if claim.Reference && (in.DelegatesWork || in.Children != nil && in.Children.Open() > 0 || issueHasLabel(in.Labels, PartiallyAddressedLabel)) {
		// Coverage is display-only for a delegating tracker (#13747).
		// Keep its non-closing scope, without mutating the shared claim.
		copy := *claim
		copy.KeepReference = true
		claim = &copy
	}
	link, err := client.EnsureIssueStagePRLink(ctx, in.Repo, in.Number, claim)
	if err != nil {
		if isSearchRateLimit(err) {
			// The client short-circuits locally while limited; every
			// further pair would fail identically. Stop for this sweep.
			return false, false, true
		}
		if logger != nil {
			logger.Warn("issue stage labels: PR issue-link safeguard failed", "repo", in.Repo, "issue", in.Number, "pr", in.Claim.PRNumber, "error", err)
		}
		return false, false, false
	}
	if link.EditedBody {
		if logger != nil {
			logger.Info("issue stage labels: appended closing keyword to Hive-authored PR body for GitHub issue link", "repo", in.Repo, "issue", in.Number, "pr", in.Claim.PRNumber, "base", link.Base)
		}
		return true, false, false
	}
	if strings.TrimSpace(link.StageComment) == "" {
		return false, false, false
	}
	rec, ok := store.Get(key)
	if !ok || rec.EntryCommented {
		return false, false, false
	}
	if err := client.CreateIssueComment(ctx, in.Repo, in.Number, link.StageComment); err != nil {
		owner, name := client.splitRepo(in.Repo)
		if client.noteArchivedWrite(strings.ToLower(owner+"/"+name), err) {
			if logger != nil {
				logger.Debug("issue stage labels: repository is archived and read-only, skipping PR link stage comment", "repo", in.Repo, "issue", in.Number)
			}
			return false, false, false
		}
		if logger != nil {
			logger.Warn("issue stage labels: PR link stage comment failed", "repo", in.Repo, "issue", in.Number, "pr", in.Claim.PRNumber, "error", err)
		}
		return false, false, false
	}
	store.MarkEntryCommented(key, "")
	return false, true, false
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
			_, _, err := c.client.Issues.CreateLabel(ctx, owner, name, &gh.Label{Name: gh.Ptr(def.Name), Color: gh.Ptr(def.Color), Description: gh.Ptr(clampLabelDescription(def.Description))})
			if err != nil && !githubStatusError(err, http.StatusUnprocessableEntity) {
				c.noteArchivedLabelRefusal(key, err)
				return err
			}
		case !strings.EqualFold(cur.GetColor(), def.Color):
			if _, _, err := c.client.Issues.EditLabel(ctx, owner, name, cur.GetName(), &gh.Label{Name: gh.Ptr(def.Name), Color: gh.Ptr(def.Color), Description: gh.Ptr(clampLabelDescription(def.Description))}); err != nil {
				c.noteArchivedLabelRefusal(key, err)
				return err
			}
		}
	}
	c.stageFamily.mark(key)
	c.coveredByPRLabel.mark(key)
	return nil
}

// noteArchivedLabelRefusal memoises the repo when GitHub refused a label
// create or edit because the repository is archived and read-only
// (hivecommons/hive#14026), so later sweeps skip it instead of retrying.
func (c *Client) noteArchivedLabelRefusal(key string, err error) {
	c.noteArchivedWrite(key, err)
}

// noteArchivedWrite memoises the repo when err is GitHub's 403 refusal of a
// write to an archived, read-only repository and reports whether it was one
// (hivecommons/hive#14177, #14213).
func (c *Client) noteArchivedWrite(key string, err error) bool {
	if githubStatusError(err, http.StatusForbidden) && strings.Contains(strings.ToLower(err.Error()), "archived") {
		c.stageArchived.mark(key)
		return true
	}
	return false
}
