package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// LedgerCoverBackfill is one BackfillCoveredByLedger pass.
type LedgerCoverBackfill struct {
	// Checked counts ledger claims an open PR makes on an issue the store
	// holds below 040.
	Checked int
	// Covered counts issues moved to 040 under their covering PR.
	Covered int
	// SkippedRedStale counts issues the red+stale valve kept released.
	SkippedRedStale int
	// SkippedNotOpen counts claims whose PR the fresh read found closed or
	// merged; the ledger catches up on its own.
	SkippedNotOpen int
	Errors         int
}

// BackfillCoveredByLedger reconciles the stage store with the duplicate-PR
// claim ledger: every issue the ledger says an OPEN pull request covers
// (IssueClaim.CoversOpenIssue — the same claims FilterClaimedIssues buckets
// claimed_by_pr) whose store record still sits below 040 gets a CoveredBy
// fact, after a fresh read confirms the PR is open and unmerged. The sweep
// derives the same answer, but it only runs on the reconcile pass; between
// passes, a claim release on a covered issue used to write 010 over the
// ledger's 040 and the record then stayed wrong for hours (nine issues on
// the hosted spoke, hivecommons/hive#14910 et al.). This pass is cheap when
// the two agree — a ledger walk against the in-memory store, no API call —
// so it runs on every cycle, cached ones included, and at startup. redStale
// (nil = never) is the same release valve the guard applies, so an issue the
// guard released for a red, stale PR is not re-covered here. Issues the store
// has never seen are left to the sweep: the record is the evidence that the
// engine owns the item.
func (c *Client) BackfillCoveredByLedger(ctx context.Context, ledger *ClaimLedger, redStale RedStaleFunc) LedgerCoverBackfill {
	var res LedgerCoverBackfill
	if c == nil || c.client == nil || ledger == nil || ledger.Len() == 0 || !c.issueStagePolicy().Enabled {
		return res
	}
	store := stagelabels.Shared()
	for _, claim := range ledger.Claims() {
		if !claim.CoversOpenIssue() || claim.Issue <= 0 || strings.TrimSpace(claim.Repo) == "" {
			continue
		}
		if _, live := ledger.Lookup(claim.Repo, claim.Issue); !live {
			continue
		}
		issue := c.IssueStageRef(claim.Repo, claim.Issue)
		if !issue.IsDefaultSource() {
			continue
		}
		rec, ok := store.Get(issue.Key())
		if !ok || !rec.Stage.Before(stagelabels.CoveredByPR) {
			continue
		}
		res.Checked++
		if redStale != nil && redStale(claim.PRRepo, claim.PRNumber) {
			res.SkippedRedStale++
			continue
		}
		pr, ok := c.claimPRRef(claim, issue)
		if !ok {
			continue
		}
		if ctx.Err() != nil {
			res.Errors++
			return res
		}
		live, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:stage_backfill"), pr.Owner, pr.Repo, pr.Number)
		if err != nil {
			res.Errors++
			c.warnLedgerBackfill("reading covering PR failed", issue, pr, err)
			continue
		}
		if strings.EqualFold(live.GetState(), "closed") || live.GetMerged() || !live.GetMergedAt().IsZero() {
			res.SkippedNotOpen++
			continue
		}
		if err := c.EnsureCoveredByPRLabel(ctx, issue.FullRepo()); err != nil {
			res.Errors++
			c.warnLedgerBackfill("ensuring covered-by-pr label failed", issue, pr, err)
			continue
		}
		// A non-closing "Refs #N" claim stages 040 as "addresses part of
		// it", never "covers it" (hivecommons/hive#15008).
		if claim.Reference && !claim.Closing {
			err = c.StageFacts().AddressedBy(ctx, issue, pr)
		} else {
			err = c.StageFacts().CoveredBy(ctx, issue, pr)
		}
		if err != nil {
			res.Errors++
			c.warnLedgerBackfill("moving issue to covered-by-pr failed", issue, pr, err)
			continue
		}
		res.Covered++
		if c.logger != nil {
			c.logger.Info("stage backfill: issue record disagreed with the claim ledger; covered by its open PR",
				"issue", issue.String(), "pr", pr.String(), "was", rec.Stage.Slug())
		}
	}
	return res
}

// claimPRRef is the engine Ref of the pull request a ledger claim names.
// PRRepo may be bare (the configured spelling) or owner-qualified, or empty
// for a claim on the PR's own repository.
func (c *Client) claimPRRef(claim IssueClaim, issue stageengine.Ref) (stageengine.Ref, bool) {
	if claim.PRNumber <= 0 {
		return stageengine.Ref{}, false
	}
	prRepo := claim.PRRepo
	if prRepo == "" {
		prRepo = claim.Repo
	}
	owner, name := c.splitRepo(prRepo)
	if owner == "" {
		owner = issue.Owner
	}
	if owner == "" || name == "" {
		return stageengine.Ref{}, false
	}
	return stageengine.Ref{Owner: owner, Repo: name, Number: claim.PRNumber, Kind: stagelabels.PRItem}, true
}

func (c *Client) warnLedgerBackfill(what string, issue, pr stageengine.Ref, err error) {
	if c.logger == nil {
		return
	}
	c.logger.Warn(fmt.Sprintf("stage backfill: %s", what), "issue", issue.String(), "pr", pr.String(), "error", err)
}
