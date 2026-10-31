package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/holdguard"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/writeback"
)

var (
	holdGuardStoreOnce sync.Once
	holdGuardStore     *holdguard.Store
)

// holdGuardLedgerPath sits beside the fix-loop ledger on the PVC: same
// lifetime, same operator expectations, same backup story.
const holdGuardLedgerPath = "/data/metrics/hold-guard.json"

// getHoldGuardStore lazily loads the hold-gate snapshot ledger (#5589) — a
// sidecar to (never a tenant of) the escalation store, so the formally
// verified escalation Entry lifecycle is untouched by this concern.
func getHoldGuardStore() *holdguard.Store {
	holdGuardStoreOnce.Do(func() {
		holdGuardStore = holdguard.Load(holdGuardLedgerPath)
	})
	return holdGuardStore
}

// holdGuardHumanChangeRequests resolves the decision ledger the hold guard
// reads operator approvals from; tests swap in a memory-only store.
var holdGuardHumanChangeRequests = func() review.HumanChangeRequestStore {
	return review.DefaultHumanChangeRequests()
}

// holdGuardOperatorApproval reports the trusted merger whose recorded
// approval (dashboard Approve or `/hive decision approve`,
// pr_decision_operator.go) stands against exactly headSHA on repo#number.
// The record is head-scoped — a newer head supersedes it — so it is proof
// the person reviewed the tree on the branch now, which the gate-lift
// timeline cannot show when the hive removed the label for them. Fails
// closed: no record, a change request, a carried approval, an unknown or
// untrusted actor, or an empty head never cover the head. The one head change that does not
// supersede the record is the hive's own base refresh — a merge of the base
// branch by the App that leaves the PR's diff as the person saw it — which
// carries the approval onto the new head first (hivecommons/hive#15649).
func holdGuardOperatorApproval(ctx context.Context, cfg *config.Config, ghClient *github.Client, repo string, number int, headSHA string) (string, bool) {
	headSHA = strings.TrimSpace(headSHA)
	if cfg == nil || headSHA == "" {
		return "", false
	}
	store := holdGuardHumanChangeRequests()
	if store == nil {
		return "", false
	}
	req, ok := ghClient.RecordedDecisionFor(ctx, store, repo, number, headSHA)
	if !ok || !req.IsApproval() || !strings.EqualFold(strings.TrimSpace(req.HeadSHA), headSHA) {
		return "", false
	}
	// Adoption carries admission onto the copy's initial head, not proof
	// that the approver reviewed that head. It cannot lift a drifted hold.
	if strings.HasPrefix(req.Body, "approve (carried") {
		return "", false
	}
	login := strings.TrimSpace(req.Actor)
	if !trustedMergerFunc(cfg)(login) {
		return "", false
	}
	return login, true
}

// enforceHoldGuard closes #5589: a hold-gated PR that accumulates commits —
// from ANY author, but especially from other agents via contaminated worktree
// bases — must not sail into the merge lanes when the hold lifts, because the
// diff the human approved under the hold is no longer the diff that merges.
//
// Per eval tick it (1) snapshots the head SHA + commit/author sets of every
// newly-held PR (first-held snapshot wins), (2) on lift compares the current
// head against the snapshot — an unchanged head pins the entire history, so
// the entry simply clears — and (3) on drift posts a one-time evidence
// comment naming the unreviewed commits and authors (plain text, no
// @-mentions), re-applies the hold label so every merge lane re-gates, and
// re-arms the snapshot at the drifted head so the NEXT human lift is the
// fresh approval. Side-effect ordering fails safe: the comment must land
// before the label (a bare re-hold with no explanation is exactly the silent
// state this guard exists to prevent), and the snapshot only re-arms after
// the label sticks (re-arming first would make the next tick read the drifted
// head as clean and merge it).
//
// Returns the drifted PR keys ("repo/number", matching writeMergeEligible's
// hold-set keying) so the SAME tick's merge-eligible artifact already
// excludes them — no one-tick window between detection and the re-applied
// label reaching enumeration.
func enforceHoldGuard(
	ctx context.Context,
	cfg *config.Config,
	ghClient *github.Client,
	writer forge.IssueWriter,
	actionable *github.ActionableResult,
	logger *slog.Logger,
	selfAuthHoldEnabledOverride ...func(string) bool,
) map[string]bool {
	reReview := map[string]bool{}
	if actionable == nil {
		return reReview
	}
	store := getHoldGuardStore()
	org := ""
	selfAuthHoldEnabledForRepo := func(string) bool { return true }
	if cfg != nil {
		selfAuthHoldEnabledForRepo = cfg.SelfAuthorizationHoldEnabledForRepo
		org = cfg.Project.Org
	}
	if len(selfAuthHoldEnabledOverride) > 0 && selfAuthHoldEnabledOverride[0] != nil {
		selfAuthHoldEnabledForRepo = selfAuthHoldEnabledOverride[0]
	}

	fetchCommits := func(repo string, number int) []holdguard.Commit {
		if ghClient == nil {
			return nil
		}
		got, err := ghClient.ListPRCommits(ctx, repo, number)
		if err != nil {
			// The head SHA alone still pins the tree; a missing commit list
			// only degrades the drift comment's evidence, never the gate.
			logger.Warn("hold guard: commit list unavailable", "repo", repo, "pr", number, "error", err)
			return nil
		}
		commits := make([]holdguard.Commit, 0, len(got))
		for _, c := range got {
			commits = append(commits, holdguard.Commit{SHA: c.SHA, Author: c.Author, Title: c.Title, Parents: c.Parents})
		}
		return commits
	}

	// (1) Snapshot newly-held PRs; keep long-standing holds out of retention's
	// reach. The FIRST held observation is the baseline — later pushes while
	// still held must show up as drift at lift time, not become the baseline.
	for _, h := range actionable.Hold.Items {
		if h.Type != "pr" {
			continue
		}
		repo := fullRepoName(h.Repo, org)
		if _, ok := store.Recorded(repo, h.Number); ok {
			// Still held: remember the head seen under the gate, the
			// hive's own proof that it predates any later lift (#13603).
			store.Touch(repo, h.Number, h.HeadSHA)
			continue
		}
		if h.HeadSHA == "" {
			// Enumeration carried no head for this hold (sparse response);
			// nothing to pin yet — retry next tick.
			continue
		}
		if store.Snapshot(repo, h.Number, h.HeadSHA, fetchCommits(h.Repo, h.Number)) {
			logger.Info("hold guard: snapshot recorded for hold-gated PR",
				"repo", repo, "pr", h.Number, "head_sha", h.HeadSHA)
		}
	}

	// (1.5) Keep already-tracked held PRs mergeable (#10437): "held" must gate
	// the MERGE, not the branch. Runs after the snapshot step above so a PR
	// held for the first time this tick is pinned before this ever touches
	// its branch.
	maintainHeldPRMergeability(ctx, ghClient, writer, store, org, actionable.PRs.Held, fetchCommits, logger)

	// (2) Check every open PR that WAS hold-gated (has a snapshot) and no
	// longer is (it enumerated as actionable): the hold lifted this window.
	for _, pr := range actionable.PRs.Items {
		repo := fullRepoName(pr.Repo, org)
		rec, ok := store.Recorded(repo, pr.Number)
		if !ok {
			continue
		}
		if pr.HeadSHA != "" && pr.HeadSHA == rec.HeadSHA {
			// Head unchanged ⇒ identical history ⇒ the tree the human
			// approved under the hold is the tree that merges. Clear and go.
			store.Clear(repo, pr.Number)
			logger.Info("hold guard: hold lifted with head unchanged; merge lanes reopened",
				"repo", repo, "pr", pr.Number, "head_sha", pr.HeadSHA)
			continue
		}
		if !selfAuthHoldEnabledForRepo(pr.Repo) && ghClient != nil {
			selfAuthHold, err := ghClient.LiftedHoldWasSelfAuthorization(ctx, pr.Repo, pr.Number)
			if err != nil {
				logger.Warn("hold guard: could not check #5117 self-authorization hold provenance before config-disabled skip",
					"repo", repo, "pr", pr.Number, "error", err)
			} else if selfAuthHold {
				store.Clear(repo, pr.Number)
				logger.Info("self-authorization hold disabled by config",
					"repo", repo, "pr", pr.Number)
				continue
			}
		}
		// A dashboard Approve (or `/hive decision approve`) is recorded
		// against the exact head the person saw; the hive then removes the
		// gate label on their behalf, so the timeline below shows a hive
		// lift that can never cover the head. The head-keyed approval by a
		// trusted merger is the review the guard asks for: accept it.
		if login, ok := holdGuardOperatorApproval(ctx, cfg, ghClient, repo, pr.Number, pr.HeadSHA); ok {
			store.Clear(repo, pr.Number)
			logger.Info("hold guard: current head approved by a trusted merger through the hive's decision record; head accepted",
				"repo", repo, "pr", pr.Number,
				"recorded_head", rec.HeadSHA, "current_head", pr.HeadSHA, "approver", login)
			continue
		}
		if ghClient != nil {
			// A trusted human who removed the gate label AFTER the current
			// head arrived reviewed the head that is on the branch now; an
			// earlier rebase during the hold is not drift to them (#13603).
			// The head predates the lift only by unforgeable evidence: the
			// server-stamped force-push event that produced it, or the hive
			// having seen this exact head while the gate was still on —
			// never commit dates or SHA-keyed check suites / statuses.
			// Lookup failures and missing evidence fail closed into the
			// drift path below.
			lift, found, err := ghClient.LatestHoldGateLift(ctx, pr.Repo, pr.Number, pr.HeadSHA)
			if err != nil {
				logger.Warn("hold guard: could not read gate-lift provenance; treating moved head as drift",
					"repo", repo, "pr", pr.Number, "error", err)
			} else if found && holdguard.LiftCoversCurrentHead(lift) {
				store.Clear(repo, pr.Number)
				logger.Info("hold guard: gate lifted by a human after the current head arrived; head accepted",
					"repo", repo, "pr", pr.Number,
					"recorded_head", rec.HeadSHA, "current_head", pr.HeadSHA,
					"lifted_at", lift.LiftedAt, "head_arrived_at", lift.LatestPushAt, "push_evidence", lift.PushEvidence)
				continue
			} else if found && holdguard.LiftCoversObservedHead(lift, rec, pr.HeadSHA) {
				store.Clear(repo, pr.Number)
				logger.Info("hold guard: gate lifted by a human on the head the hive observed while held; head accepted",
					"repo", repo, "pr", pr.Number,
					"recorded_head", rec.HeadSHA, "current_head", pr.HeadSHA,
					"lifted_at", lift.LiftedAt)
				continue
			}
		}

		// Drift: keep it out of this tick's merge-eligible artifact
		// unconditionally, keyed the way writeMergeEligible keys its hold set.
		key := fmt.Sprintf("%s/%d", pr.Repo, pr.Number)
		reReview[key] = true

		current := fetchCommits(pr.Repo, pr.Number)
		drift := holdguard.Diff(rec, pr.HeadSHA, current)
		logger.Warn("hold guard: branch moved while hold-gated — blocking auto-merge, requiring fresh review",
			"repo", repo, "pr", pr.Number,
			"recorded_head", rec.HeadSHA, "current_head", pr.HeadSHA,
			"new_commits", len(drift.NewCommits), "new_authors", strings.Join(drift.NewAuthors, ","))

		if writer == nil {
			// No forge writer (booted without credentials): the reReview
			// exclusion above still holds every tick; side effects wait.
			continue
		}
		// Hold-gate upkeep is own-only (writeback.KindHoldGateNotice): the
		// reReview exclusion above already blocks the merge internally; a
		// drift comment and re-hold label on a human's PR the hive never
		// opened is the torch-spyre spam. The gate records the suppression
		// for the dashboard.
		if d := ghClient.WritebackAllowed(repo, ghClient.WritebackItemForPR(pr), writeback.KindHoldGateNotice, holdguard.CommentBody(drift)); !d.Allowed {
			logger.Info("hold guard: drift notice kept off GitHub by write-back gate", "repo", repo, "pr", pr.Number, "reason", d.Reason)
			continue
		}
		if !rec.Commented {
			if err := writer.CreateIssueComment(ctx, pr.Repo, pr.Number, holdguard.CommentBody(drift)); err != nil { // writeback:gated
				// Retry the whole episode next tick rather than re-holding
				// with no explanation — the evidence reaching a human is the
				// point, and the exclusion above already blocks the merge.
				logger.Warn("hold guard: drift comment failed; will retry next pass",
					"repo", repo, "pr", pr.Number, "error", err)
				continue
			}
			store.MarkCommented(repo, pr.Number)
		}
		if err := writer.AddLabels(ctx, pr.Repo, pr.Number, []string{holdguard.ReHoldLabel}); err != nil { // writeback:gated
			// Commented but not re-held: the snapshot stays on the OLD head,
			// so next tick re-detects the drift (comment deduped) and retries
			// the label. Never re-arm before the label sticks.
			logger.Warn("hold guard: re-applying hold label failed; will retry next pass",
				"repo", repo, "pr", pr.Number, "error", err)
			continue
		}
		store.ReArm(repo, pr.Number, pr.HeadSHA, current)
	}

	// (3) Age out entries with no clearing event (PR merged or closed while
	// held). See holdguard.Retention for why absence-pruning would be unsafe.
	store.Prune(holdguard.Retention)
	return reReview
}

// maintainHeldPRMergeability closes the second half of #10437: a hold-gated
// PR must stay mergeable while it waits on a human, not merely frozen. GitHub
// still computes mergeable_state for a held PR, so a held PR can silently go
// "behind" (a sibling PR merged first) or "dirty" (that sibling's merge now
// conflicts) and sit that way for the entire review wait.
//
// For each PR the guard is already tracking, a "behind" or "dirty" state gets
// one merge-from-base attempt via UpdateBranchExpectedHead, pinned to the
// enumerated head and using the hive's own identity. On success the result
// is hygiene, not reviewable content, so the guard's baseline is advanced to
// the new head (holdguard.AdvanceHygiene) — this is what keeps the next lift
// check from reading the hive's own upkeep push as drift. The advance
// requires proof that the new head IS that merge (holdguard.IsHygieneMerge:
// first parent is the requested head, second is a
// commit on the base branch — the enumerated BaseSHA, or one the live base
// contains when the base moved before the update ran); a head that moved any
// other way stays unadopted and reads as drift. On
// failure (a real conflict the hive cannot resolve), a one-time comment tells
// the human before they open the review, deduplicated
// through the ledger's ConflictNoted bit exactly like the drift comment
// dedupes through Commented.
//
// PRs the guard has not snapshotted yet are skipped — the snapshot step (1)
// runs first every tick, so this only ever sees a PR after its baseline is
// pinned, and advancing a baseline that was never pinned would plant a wrong
// one.
func maintainHeldPRMergeability(
	ctx context.Context,
	ghClient *github.Client,
	writer forge.IssueWriter,
	store *holdguard.Store,
	org string,
	held []github.PullRequest,
	fetchCommits func(repo string, number int) []holdguard.Commit,
	logger *slog.Logger,
) {
	if ghClient == nil || store == nil {
		return
	}
	for _, pr := range held {
		repo := fullRepoName(pr.Repo, org)
		rec, ok := store.Recorded(repo, pr.Number)
		if !ok {
			continue
		}
		state := strings.ToLower(pr.MergeableState)
		if state != "behind" && state != "dirty" {
			continue
		}
		// Own-only (writeback.KindHoldGateNotice): the merge-from-base push
		// and the conflict notice are upkeep on the hive's OWN held PRs. A
		// human's prow-held PR is not the hive's branch to push to or
		// comment on (torch-spyre, 2026-10-28: 13 "Hold-gate mergeability"
		// comments on maintainers' PRs).
		if d := ghClient.WritebackAllowed(repo, ghClient.WritebackItemForPR(pr), writeback.KindHoldGateNotice, holdguard.ConflictCommentBody()); !d.Allowed {
			logger.Debug("hold guard: held-PR mergeability upkeep kept off GitHub by write-back gate", "repo", repo, "pr", pr.Number, "reason", d.Reason)
			continue
		}
		// Pin the update to the enumerated head: GitHub then refuses it if a
		// contributor push landed after enumeration, so the hive never stacks
		// its merge on commits the guard has not evaluated. The post-hoc
		// IsHygieneMerge check below still covers a push landing after the 202
		// (hivecommons/hive#14537).
		if err := ghClient.UpdateBranchExpectedHead(ctx, pr.Repo, pr.Number, pr.HeadSHA, pr.BaseSHA); err != nil { // writeback:gated
			if github.IsUpdateBranchStaleHead(err) {
				// The head moved since enumeration: not a conflict, and not
				// ours to merge onto. The next tick re-reads the head and the
				// lift check sees the move as drift.
				logger.Info("hold guard: held PR head moved since enumeration; skipped keeping it mergeable",
					"repo", repo, "pr", pr.Number, "expected_head", pr.HeadSHA, "error", err)
				continue
			}
			if state != "dirty" {
				// "behind" is expected to be a trivial fast-forward; a
				// failure here is an anomaly (race, API hiccup), not the
				// known-unresolvable conflict the comment below is for.
				logger.Warn("hold guard: keeping held PR mergeable failed",
					"repo", repo, "pr", pr.Number, "mergeable_state", state, "error", err)
				continue
			}
			if rec.ConflictNoted || writer == nil {
				continue
			}
			if cerr := writer.CreateIssueComment(ctx, pr.Repo, pr.Number, holdguard.ConflictCommentBody()); cerr != nil { // writeback:gated
				logger.Warn("hold guard: posting held-PR conflict notice failed; will retry next pass",
					"repo", repo, "pr", pr.Number, "error", cerr)
				continue
			}
			store.MarkConflictNoted(repo, pr.Number)
			continue
		}
		commits := fetchCommits(pr.Repo, pr.Number)
		newHead := ""
		if len(commits) > 0 {
			newHead = commits[len(commits)-1].SHA
		}
		if newHead == "" || newHead == pr.HeadSHA {
			// UpdateBranch always moves the head; failing to confirm the new
			// SHA means the commit list refresh failed — leave the baseline
			// alone rather than advance it on stale information.
			logger.Warn("hold guard: could not confirm new head after keeping held PR mergeable",
				"repo", repo, "pr", pr.Number)
			continue
		}
		onBase := func(sha string) bool {
			if sha == pr.BaseSHA {
				return true
			}
			// update-branch merges the base tip at call time, which may be
			// newer than the enumerated BaseSHA (or BaseSHA may be unknown):
			// ask GitHub whether the base branch contains the merged parent.
			if pr.BaseRef == "" {
				return false
			}
			on, err := ghClient.CommitOnBranch(ctx, pr.Repo, pr.BaseRef, sha)
			if err != nil {
				logger.Warn("hold guard: could not confirm merged parent is on the base branch",
					"repo", repo, "pr", pr.Number, "base_ref", pr.BaseRef, "parent", sha, "error", err)
				return false
			}
			return on
		}
		if !holdguard.IsHygieneMerge(commits[len(commits)-1], pr.HeadSHA, onBase) {
			// The head moved, but not provably to the hive's own merge of the
			// base into the head it asked to update: a contributor push may
			// have landed between the 202 and the refetch. Advancing would
			// adopt unreviewed commits as the baseline, so leave it pinned and
			// let the lift check read the move as drift (hivecommons/hive#14534).
			logger.Warn("hold guard: new head after keeping held PR mergeable is not a merge of the base into the held head; baseline left pinned",
				"repo", repo, "pr", pr.Number, "requested_head", pr.HeadSHA, "base_ref", pr.BaseRef, "base", pr.BaseSHA, "new_head", newHead)
			continue
		}
		store.AdvanceHygiene(repo, pr.Number, newHead, commits)
		logger.Info("hold guard: merged base into held PR to keep it mergeable",
			"repo", repo, "pr", pr.Number, "mergeable_state", state, "new_head", newHead)
	}
}

// intentConfigFromCfg maps config.IntentConfig onto the intent package's
// classification config. Shared by writeIntentVerdicts (human merge lane) and
// the App self-merge sweep's IntentGate (#6258) so both lanes classify with
// identical patterns.
func intentConfigFromCfg(cfg *config.Config) intent.Config {
	if cfg == nil {
		return intent.Config{}
	}
	return intent.Config{
		TestPathPatterns:      cfg.Intent.TestPathPatterns,
		DocsPathPatterns:      cfg.Intent.DocsPathPatterns,
		GuardrailPathPatterns: cfg.Intent.GuardrailPathPatterns,
		FeatureSignals:        cfg.Intent.FeatureSignals,
	}
}

// humanMergePathsFor returns auto_merge.human_merge_paths for repo
// (owner/repo). Every automerge sweep lane consults it on each evaluation
// (#11038); nil cfg means no paths.
func humanMergePathsFor(cfg *config.Config, repo string) []string {
	if cfg == nil {
		return nil
	}
	return cfg.AutoMerge.HumanMergePathsFor(repo)
}

// humanMergeRuleFor reads auto_merge.human_merge_title_prefixes /
// human_merge_labels through cfg on every call so a reload applies
// (hivecommons/hive#15442). A nil config means the defaults.
func humanMergeRuleFor(cfg *config.Config) prstage.HumanMergeRule {
	if cfg == nil {
		return prstage.HumanMergeRule{}
	}
	return cfg.AutoMerge.HumanMergeRule()
}

// selfMergeIntentGate builds the intent-tier gate the App self-merge sweep
// enforces (#6258) from the same config and bead evidence the human merge
// lane uses. Enforce is read through cfg on every call so a reload applies.
func selfMergeIntentGate(cfg *config.Config, beadStores map[string]*beads.Store) *automerge.IntentGate {
	return &automerge.IntentGate{
		Config:     intentConfigFromCfg(cfg),
		Enforce:    func() bool { return cfg != nil && cfg.Intent.Enforce },
		BeadStores: beadStores,
		Shadow:     func() bool { return intentShadowEnabled(cfg) },
		RecordShadow: func(fullRepo string, number int, head string, gate evidence.ShadowGate) {
			writeIntentShadow(cfg, slog.Default(), "self-merge", fullRepo, number, head, &gate)
		},
	}
}
