package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Hive-authored superseded PR auto-close (hivecommons/hive#14965 → #14957,
// issue #14952). A PR the hive opened for an issue that a *different*
// merged PR already closed has nothing left to deliver when it is
// conflicting (GitHub refuses to update the head) or its diff is contained
// in the merged PR. The reviewer swarm used to answer `requires_human`
// ("should this be closed in favour of #14957?") and the PR sat at
// stage/085-needs-decision until the owner clicked close-superseded.
// The pick is mechanical for a hive PR: the hive closes it itself —
// `hive/superseded` label, one comment naming the superseding PR, the
// stage engine told it closed — the same path the dashboard's
// close-superseded decision takes. A person's PR is never closed here
// (it keeps the notice + 085 park of the contributor flow).
//
// The rule runs in three places so no reviewer cycle is wasted: the
// automerge sweep before the review swarm is dispatched (the lane already
// knows the PR is conflicting), close-on-merge of the superseding PR, and
// the supersession sweep's own-PR resolution when GitHub refuses the
// rebase. governor.auto_close_superseded (default on) switches it off.

const (
	// SupersessionActionClosedConflicting: a hive-authored superseded PR was
	// closed because its head conflicts with the base (the update GitHub
	// refused would have measured nothing it still adds).
	SupersessionActionClosedConflicting = "closed-conflicting"
	// SupersessionActionClosedContained: a hive-authored superseded PR was
	// closed because every file it touches is in the merged closer.
	SupersessionActionClosedContained = "closed-contained"
	// SupersessionActionClosedDeclared: a hive-authored PR was closed
	// because a merged PR of the same repo declares `Supersedes #N` for it
	// (hivecommons/hive#15208: #15206 said so and closed the parent issue
	// while #15208's own child issue was still open; the maintainer's
	// statement beats the conflict / file-subset measurement).
	SupersessionActionClosedDeclared = "closed-declared"
	// SupersessionActionClosedIssueClosed: a hive-authored PR was closed
	// because every issue it closes was closed as not planned (wontfix):
	// there is nothing left to deliver and nothing to rebase onto.
	SupersessionActionClosedIssueClosed = "closed-issue-closed"

	// hiveSupersededReasonDisabled .. hiveSupersededReasonOpenValue are the
	// keep reasons DecideHiveSupersededAutoClose returns.
	hiveSupersededReasonDisabled   = "disabled"
	hiveSupersededReasonNotHive    = "not-hive-authored"
	hiveSupersededReasonNotSupers  = "not-superseded"
	hiveSupersededReasonMerged     = "merged"
	hiveSupersededReasonHeld       = "held"
	hiveSupersededReasonKeepOpen   = "keep-open"
	hiveSupersededReasonPerson     = "parked-by-person"
	hiveSupersededReasonLevel      = "acmm-below-l6"
	hiveSupersededReasonStillAdds  = "still-adds-files"
	hiveSupersededVerdictClose     = "close"
	hiveSupersededVerdictKeep      = "keep"
	hiveSupersededVerdictHumanNote = "human"
)

// HiveSupersededAutoCloseFacts is everything DecideHiveSupersededAutoClose
// needs about one PR.
type HiveSupersededAutoCloseFacts struct {
	// Enabled is governor.auto_close_superseded.
	Enabled bool
	// HiveAuthored: this hive's App (or a login HiveIdentity matches) opened
	// the PR.
	HiveAuthored bool
	// Superseded: every issue the PR closes is closed and at least one was
	// closed by a different merged PR (detectSupersession), a merged PR
	// declares it supersedes this one (Declared), or every issue it closes
	// is closed with no merged closer (IssuesClosedNoCloser).
	Superseded bool
	// Declared: a merged PR of the same repo says `Supersedes #<this PR>`.
	Declared bool
	// IssuesClosedNoCloser: every issue the PR closes was closed as not
	// planned, so no merged PR closed any of them.
	IssuesClosedNoCloser bool
	// Merged / Held / KeptOpen / ParkedByPerson are the guards every sweep
	// honours: a merged PR, a hold or exemption label, hive/keep-open, or a
	// needs-human a person placed.
	Merged, Held, KeptOpen, ParkedByPerson bool
	// Conflicting: GitHub reports mergeable_state dirty for the head.
	Conflicting bool
	// FilesSubset: every file the PR touches is in the merged closer.
	FilesSubset bool
	// Level is the repo's ACMM level; own-PR closes need L6 like the rest
	// of the supersession sweep's own-PR resolution.
	Level int
}

// DecideHiveSupersededAutoClose is the pure rule. It returns "close" with
// the action to record, "human" when the PR is a person's and superseded
// (the caller leaves the contributor flow's note + park in place), or
// "keep" with the guard that held.
func DecideHiveSupersededAutoClose(f HiveSupersededAutoCloseFacts) (verdict, reason string) {
	switch {
	case !f.Enabled:
		return hiveSupersededVerdictKeep, hiveSupersededReasonDisabled
	case f.Merged:
		return hiveSupersededVerdictKeep, hiveSupersededReasonMerged
	case !f.Superseded:
		return hiveSupersededVerdictKeep, hiveSupersededReasonNotSupers
	case !f.HiveAuthored:
		return hiveSupersededVerdictHumanNote, hiveSupersededReasonNotHive
	case f.Held:
		return hiveSupersededVerdictKeep, hiveSupersededReasonHeld
	case f.KeptOpen:
		return hiveSupersededVerdictKeep, hiveSupersededReasonKeepOpen
	case f.ParkedByPerson:
		return hiveSupersededVerdictKeep, hiveSupersededReasonPerson
	case f.Level < acmmLevelAutonomous:
		return hiveSupersededVerdictKeep, hiveSupersededReasonLevel
	case f.Declared:
		return hiveSupersededVerdictClose, SupersessionActionClosedDeclared
	case f.Conflicting:
		return hiveSupersededVerdictClose, SupersessionActionClosedConflicting
	case f.FilesSubset:
		return hiveSupersededVerdictClose, SupersessionActionClosedContained
	case f.IssuesClosedNoCloser:
		return hiveSupersededVerdictClose, SupersessionActionClosedIssueClosed
	}
	return hiveSupersededVerdictKeep, hiveSupersededReasonStillAdds
}

// SupersededAutoClosePolicy answers, per repo, whether hive-authored
// superseded PRs may be closed automatically and at what ACMM level the
// repo runs.
type SupersededAutoClosePolicy func(repo string) (enabled bool, level int)

// SetSupersededAutoClosePolicy installs the policy the out-of-sweep closers
// (automerge pre-review hook, close-on-merge) consult. nil restores the
// default: enabled, at the autonomous level.
func (c *Client) SetSupersededAutoClosePolicy(fn SupersededAutoClosePolicy) {
	if c == nil {
		return
	}
	c.supersededAutoCloseMu.Lock()
	defer c.supersededAutoCloseMu.Unlock()
	c.supersededAutoClosePolicy = fn
}

func (c *Client) supersededAutoClosePolicyFor(repo string) (bool, int) {
	c.supersededAutoCloseMu.Lock()
	fn := c.supersededAutoClosePolicy
	c.supersededAutoCloseMu.Unlock()
	if fn == nil {
		return true, acmmLevelAutonomous
	}
	return fn(repo)
}

// HiveSupersededClose is what CloseSupersededHivePRIfEligible did.
type HiveSupersededClose struct {
	// Closed is true when the PR was closed by this call.
	Closed bool
	// Verdict / Reason are DecideHiveSupersededAutoClose's answer.
	Verdict, Reason string
	// Superseded is true when detectSupersession found a closer; CloserPR
	// / Issue name it (0 otherwise).
	Superseded bool
	CloserPR   int
	Issue      int
}

// CloseSupersededHivePRIfEligible closes pr as superseded when the hive
// opened it, every issue it closes is closed, one of them by a different
// merged PR, and the PR is conflicting or adds nothing the merged PR did
// not. It is idempotent: a closed PR, or one not superseded, is a no-op.
// repo is "owner/name".
func (c *Client) CloseSupersededHivePRIfEligible(ctx context.Context, repo string, pr *gh.PullRequest) (HiveSupersededClose, error) {
	out := HiveSupersededClose{}
	if c == nil || c.client == nil {
		return out, ErrNoGitHubClient
	}
	if pr == nil {
		return out, nil
	}
	owner, name := c.splitRepo(repo)
	repo = owner + "/" + name
	enabled, level := c.supersededAutoClosePolicyFor(repo)
	facts := HiveSupersededAutoCloseFacts{
		Enabled: enabled,
		Level:   level,
		Merged:  pr.GetMerged() || !pr.GetMergedAt().IsZero() || strings.EqualFold(pr.GetState(), "closed"),
	}
	labels := labelNames(pr.Labels)
	facts.Held = c.isHeld(labels) || c.isExempt(labels)
	facts.KeptOpen = supersessionHasLabel(pr, SupersessionKeepOpenLabel)
	identity := c.getHiveIdentity()
	if identity.AppLogin == "" {
		identity.AppLogin = c.appBotLogin
	}
	facts.HiveAuthored = identity.Matches(safeGetLogin(pr.GetUser()))
	if !enabled || facts.Merged || facts.Held || facts.KeptOpen || !facts.HiveAuthored {
		// Cheap guards first: no GitHub reads for a PR the rule cannot
		// touch. The human case is reported as such for the caller's log.
		if !facts.HiveAuthored && enabled && !facts.Merged {
			out.Verdict, out.Reason = hiveSupersededVerdictHumanNote, hiveSupersededReasonNotHive
			return out, nil
		}
		out.Verdict, out.Reason = DecideHiveSupersededAutoClose(facts)
		return out, nil
	}
	now := time.Now()
	det, err := c.detectSupersessionDetail(ctx, repo, pr, identity, now)
	if err != nil {
		return out, err
	}
	candidate, closer := det.candidate, det.closer
	facts.Superseded = det.detected
	if !det.detected {
		// A merged PR that declares `Supersedes #N` for this one, or every
		// closing claim closed with no merged closer, supersedes it too.
		declared, declaredBy, err := c.detectDeclaredSupersession(ctx, owner, name, pr)
		if err != nil {
			return out, err
		}
		switch {
		case declared:
			facts.Superseded, facts.Declared = true, true
			closer = declaredBy
			candidate = det.closedClaim
			if candidate.Issue == 0 {
				// Name the issue the merged PR closed (#15208: `Part of
				// #15188`, closed by the declaring PR) in the comment.
				candidate = c.firstClosedClaim(ctx, claimsFromPR(pr, repo, identity, now))
			}
		case det.allClosingClosed && det.closingClaims && det.allNotPlanned:
			facts.Superseded, facts.IssuesClosedNoCloser = true, true
			candidate = det.closedClaim
		default:
			out.Verdict, out.Reason = DecideHiveSupersededAutoClose(facts)
			return out, nil
		}
	}
	out.Superseded, out.CloserPR, out.Issue = true, closer.Number, candidate.Issue
	facts.ParkedByPerson = c.supersessionNeedsHumanByPerson(ctx, owner, name, pr)
	if level >= acmmLevelAutonomous && !facts.ParkedByPerson && !facts.Declared && closer.Number > 0 {
		facts.Conflicting = prMergeableStateDirty(pr)
		if !facts.Conflicting && strings.TrimSpace(pr.GetMergeableState()) == "" {
			// List payloads carry no mergeable_state; one GET reads it.
			facts.Conflicting = c.prIsDirty(ctx, owner, name, pr.GetNumber())
		}
		if !facts.Conflicting {
			openFiles, closerFiles, err := c.prFileSets(ctx, owner, name, pr.GetNumber(), closer)
			if err != nil {
				return out, err
			}
			facts.FilesSubset = len(supersessionExtraFiles(openFiles, closerFiles)) == 0
		}
	}
	out.Verdict, out.Reason = DecideHiveSupersededAutoClose(facts)
	if out.Verdict != hiveSupersededVerdictClose {
		return out, nil
	}
	event := SupersessionSweepEvent{
		Repo: repo, Number: pr.GetNumber(), Author: safeGetLogin(pr.GetUser()),
		IssueRepo: candidate.Repo, Issue: candidate.Issue, CloserPR: closer.Number, Action: out.Reason,
	}
	if err := c.closeHiveSupersededPR(ctx, owner, name, pr, event, closer, facts.Conflicting); err != nil {
		return out, err
	}
	out.Closed = true
	return out, nil
}

// closeHiveSupersededPR is the one write path: notice (edited in place
// when the sweep already left one), label + close, stage engine told.
func (c *Client) closeHiveSupersededPR(ctx context.Context, owner, repo string, pr *gh.PullRequest, event SupersessionSweepEvent, closer supersessionCloser, conflicting bool) error {
	number := pr.GetNumber()
	head := stagelabels.ShortSHA(strings.TrimSpace(pr.GetHead().GetSHA()))
	why := hiveSupersededCloseReason(event, closer, head, strings.TrimSpace(pr.GetBase().GetRef()), conflicting)
	if _, err := c.ensureSupersessionComment(ctx, owner, repo, number, renderSupersessionComment(event, closer, why)); err != nil {
		return err
	}
	if err := c.labelAndCloseSupersededPR(ctx, owner, repo, number); err != nil {
		return err
	}
	if sink := c.StageFacts(); sink != nil {
		if err := sink.Closed(ctx, c.PRStageRef(owner+"/"+repo, number)); err != nil && c.logger != nil {
			c.logger.Warn("superseded auto-close: stage close deferred to next sweep", slog.String("repo", owner+"/"+repo), slog.Int("pr", number), slog.String("error", err.Error()))
		}
	}
	if c.logger != nil {
		c.logger.Info("superseded auto-close: closed hive-authored PR",
			slog.String("repo", owner+"/"+repo), slog.Int("pr", number), slog.Int("closer", closer.Number), slog.Int("issue", event.Issue), slog.String("action", event.Action))
	}
	return nil
}

// firstClosedClaim returns the first claim (any tier) whose issue is
// closed; a read failure or an open issue is skipped. Zero when none.
func (c *Client) firstClosedClaim(ctx context.Context, claims []IssueClaim) IssueClaim {
	for _, claim := range claims {
		owner, name := c.splitRepo(claim.Repo)
		issue, _, err := c.client.Issues.Get(ctx, owner, name, claim.Issue)
		if err == nil && strings.EqualFold(issue.GetState(), "closed") {
			return claim
		}
	}
	return IssueClaim{}
}

// hiveSupersededCloseReason is the one-line why of the close comment, by
// the action that closed it.
func hiveSupersededCloseReason(event SupersessionSweepEvent, closer supersessionCloser, head, base string, conflicting bool) string {
	switch event.Action {
	case SupersessionActionClosedDeclared:
		if event.Issue > 0 {
			return fmt.Sprintf("Superseded by #%d which closed #%d and declares it supersedes this PR. Closing this hive-authored PR; reopen if that's wrong.", closer.Number, event.Issue)
		}
		return fmt.Sprintf("Superseded by #%d, which declares it supersedes this PR. Closing this hive-authored PR; reopen if that's wrong.", closer.Number)
	case SupersessionActionClosedIssueClosed:
		return fmt.Sprintf("Closing this hive-authored PR: every issue it closes (#%d) was closed as not planned, so there is nothing left to deliver. Reopen if that's wrong.", event.Issue)
	}
	if conflicting {
		return fmt.Sprintf("Closing this hive-authored PR as superseded by #%d: every issue it closes is already closed and that merged PR closed #%d. Head %s conflicts with `%s`, so there is nothing of its own left to rebase and measure. Reopen if that's wrong.",
			closer.Number, event.Issue, head, base)
	}
	return fmt.Sprintf("Closing this hive-authored PR as superseded by #%d: every issue it closes is already closed, that merged PR closed #%d, and every file head %s touches is in it. Reopen if that's wrong.",
		closer.Number, event.Issue, head)
}

// prMergeableStateDirty reads the conflict flag off a PR payload that
// carries mergeable_state (the single-PR GET; list payloads do not).
func prMergeableStateDirty(pr *gh.PullRequest) bool {
	state := strings.ToLower(strings.TrimSpace(pr.GetMergeableState()))
	return state == "dirty" || state == "conflicting"
}

// CloseSupersededHivePRsForMergedPR runs the rule for every open
// hive-authored PR of repo that closes one of issues, right after
// mergedPR merged and closed them: the superseded PR is closed in the
// same breath as the merge instead of waiting for a sweep. It returns the
// PR numbers closed.
func (c *Client) CloseSupersededHivePRsForMergedPR(ctx context.Context, repo string, mergedPR int, issues []int) ([]int, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	if len(issues) == 0 || mergedPR <= 0 {
		return nil, nil
	}
	owner, name := c.splitRepo(repo)
	repo = owner + "/" + name
	if enabled, _ := c.supersededAutoClosePolicyFor(repo); !enabled {
		return nil, nil
	}
	identity := c.getHiveIdentity()
	if identity.AppLogin == "" {
		identity.AppLogin = c.appBotLogin
	}
	want := map[int]bool{}
	for _, n := range issues {
		want[n] = true
	}
	prs, err := c.listOpenPRsForSupersessionSweep(ctx, owner, name)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var closed []int
	var firstErr error
	for _, pr := range prs {
		if pr.GetNumber() == mergedPR || !identity.Matches(safeGetLogin(pr.GetUser())) {
			continue
		}
		covers := false
		for _, claim := range claimsFromPR(pr, repo, identity, now) {
			if claim.Closing && want[claim.Issue] && supersessionSameRepo(c, claim.Repo, repo) {
				covers = true
				break
			}
		}
		if !covers {
			continue
		}
		res, err := c.CloseSupersededHivePRIfEligible(ctx, repo, pr)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			if c.logger != nil {
				c.logger.Warn("superseded auto-close: close-on-merge check failed", slog.String("repo", repo), slog.Int("pr", pr.GetNumber()), slog.Int("merged", mergedPR), slog.String("error", err.Error()))
			}
			continue
		}
		if res.Closed {
			closed = append(closed, pr.GetNumber())
		} else if c.logger != nil && res.Superseded {
			c.logger.Info("superseded auto-close: kept open", slog.String("repo", repo), slog.Int("pr", pr.GetNumber()), slog.Int("merged", mergedPR), slog.String("verdict", res.Verdict), slog.String("reason", res.Reason))
		}
	}
	return closed, firstErr
}

// supersessionSameRepo reports whether a claim's repo (bare name or
// owner/name, "" for the PR's own) names repo ("owner/name").
func supersessionSameRepo(c *Client, claimRepo, repo string) bool {
	claimRepo = strings.TrimSpace(claimRepo)
	if claimRepo == "" {
		return true
	}
	o, n := c.splitRepo(claimRepo)
	return strings.EqualFold(o+"/"+n, repo)
}
