package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/holdguard"
	"github.com/hivecommons/hive/pkg/intent"
)

// noCadenceAlertMessage renders the banner line: symptom, cause AND fix — the
// exact gap the RFC calls out in the dashboard's not-producing warnings,
// which name only the symptom.
func noCadenceAlertMessage(agents []string) string {
	return fmt.Sprintf("agent(s) %s enabled but never kicked — no cadence configured; set cadences on the agent card",
		strings.Join(agents, ", "))
}

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
			commits = append(commits, holdguard.Commit{SHA: c.SHA, Author: c.Author, Title: c.Title})
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
			store.Touch(repo, h.Number)
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
		if !rec.Commented {
			if err := writer.CreateIssueComment(ctx, pr.Repo, pr.Number, holdguard.CommentBody(drift)); err != nil {
				// Retry the whole episode next tick rather than re-holding
				// with no explanation — the evidence reaching a human is the
				// point, and the exclusion above already blocks the merge.
				logger.Warn("hold guard: drift comment failed; will retry next pass",
					"repo", repo, "pr", pr.Number, "error", err)
				continue
			}
			store.MarkCommented(repo, pr.Number)
		}
		if err := writer.AddLabels(ctx, pr.Repo, pr.Number, []string{holdguard.ReHoldLabel}); err != nil {
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

// selfMergeIntentGate builds the intent-tier gate the App self-merge sweep
// enforces (#6258) from the same config and bead evidence the human merge
// lane uses. Enforce is read through cfg on every call so a reload applies.
func selfMergeIntentGate(cfg *config.Config, beadStores map[string]*beads.Store) *automerge.IntentGate {
	return &automerge.IntentGate{
		Config:     intentConfigFromCfg(cfg),
		Enforce:    func() bool { return cfg != nil && cfg.Intent.Enforce },
		BeadStores: beadStores,
	}
}
