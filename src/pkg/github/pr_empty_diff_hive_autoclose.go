package github

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

// Hive-authored empty-diff PR auto-close (hivecommons/hive#15499).
//
// #15499 was the hive's adoption of a contributor PR whose fix had already
// landed on v5 through #15427/#15449: every merge-from-base left its tree
// identical to the base (changed_files 0). The PR claimed no issue, so the
// superseded rule (pr_superseded_hive_autoclose.go) never fired, and the
// reviewer swarm asked the owner "close as a no-op?" on every new head —
// fifteen times. A PR with nothing in its diff is the one case where the
// person's answer is mechanical: there is nothing to merge. For the hive's
// own PR the hive closes it; a person's PR keeps the decision prompt, as
// the superseded rule does.
//
// The diff is measured, never inferred from the reviewer's text: the GET
// payload's changed_files is the cheap first signal (list payloads omit
// it and read 0, so it is only ever a reason to look closer), and the
// compare API base...head is the proof. A head younger than
// EmptyDiffMinHeadAge is left alone: a branch the author is still
// pushing to may be empty for a moment.

const (
	// SupersessionActionClosedEmptyDiff: a hive-authored PR was closed
	// because its tree is identical to its base (the compare lists no
	// files); the change it carried is already on the base.
	SupersessionActionClosedEmptyDiff = "closed-empty-diff"

	// EmptyDiffMinHeadAge is how old a PR must be before an empty diff
	// closes it: a just-opened PR whose commits are still arriving is not
	// yet empty by intent.
	EmptyDiffMinHeadAge = 15 * time.Minute

	hiveEmptyDiffReasonHasDiff    = "has-diff"
	hiveEmptyDiffReasonUnknown    = "diff-unknown"
	hiveEmptyDiffReasonTooFresh   = "head-too-fresh"
	hiveEmptyDiffReasonGated      = "writeback-gated"
	hiveEmptyDiffReasonDraft      = "draft"
	hiveEmptyDiffReasonNotHive    = hiveSupersededReasonNotHive
	hiveEmptyDiffReasonHeld       = hiveSupersededReasonHeld
	hiveEmptyDiffReasonKeepOpen   = hiveSupersededReasonKeepOpen
	hiveEmptyDiffReasonPerson     = hiveSupersededReasonPerson
	hiveEmptyDiffReasonLevel      = hiveSupersededReasonLevel
	hiveEmptyDiffReasonDisabled   = hiveSupersededReasonDisabled
	hiveEmptyDiffReasonMerged     = hiveSupersededReasonMerged
	hiveEmptyDiffVerdictClose     = hiveSupersededVerdictClose
	hiveEmptyDiffVerdictKeep      = hiveSupersededVerdictKeep
	hiveEmptyDiffVerdictHumanNote = hiveSupersededVerdictHumanNote
)

// HiveEmptyDiffAutoCloseFacts is everything DecideHiveEmptyDiffAutoClose
// needs about one PR.
type HiveEmptyDiffAutoCloseFacts struct {
	// Enabled is governor.auto_close_superseded, the same switch as the
	// superseded rule: both close the hive's own PR because a merged PR
	// already carries its change.
	Enabled bool
	// HiveAuthored: this hive's App (or a login HiveIdentity matches)
	// opened the PR. A person's PR is never closed here.
	HiveAuthored bool
	// Merged / Draft / Held / KeptOpen / ParkedByPerson are the guards every
	// sweep honours.
	Merged, Draft, Held, KeptOpen, ParkedByPerson bool
	// DiffKnown is true when the compare base...head succeeded with a
	// complete file list; DiffFiles is its length.
	DiffKnown bool
	DiffFiles int
	// HeadAge is now minus the PR's last update (or creation when GitHub
	// reports no update).
	HeadAge time.Duration
	// Level is the repo's ACMM level; own-PR closes need L6 like the
	// superseded rule.
	Level int
}

// DecideHiveEmptyDiffAutoClose is the pure rule. It returns "close" when the
// hive's own PR measurably adds nothing to its base, "human" when the PR is
// a person's and empty (the caller leaves the decision prompt in place), or
// "keep" with the guard that held.
func DecideHiveEmptyDiffAutoClose(f HiveEmptyDiffAutoCloseFacts) (verdict, reason string) {
	switch {
	case !f.Enabled:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonDisabled
	case f.Merged:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonMerged
	case f.Draft:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonDraft
	case !f.DiffKnown:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonUnknown
	case f.DiffFiles > 0:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonHasDiff
	case !f.HiveAuthored:
		return hiveEmptyDiffVerdictHumanNote, hiveEmptyDiffReasonNotHive
	case f.Held:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonHeld
	case f.KeptOpen:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonKeepOpen
	case f.ParkedByPerson:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonPerson
	case f.Level < acmmLevelAutonomous:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonLevel
	case f.HeadAge < EmptyDiffMinHeadAge:
		return hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonTooFresh
	}
	return hiveEmptyDiffVerdictClose, SupersessionActionClosedEmptyDiff
}

// HiveEmptyDiffClose is what CloseEmptyDiffHivePRIfEligible did.
type HiveEmptyDiffClose struct {
	// Closed is true when the PR was closed by this call.
	Closed bool
	// Verdict / Reason are DecideHiveEmptyDiffAutoClose's answer (or
	// "writeback-gated" when the gate refused the close).
	Verdict, Reason string
	// Empty is true when the compare proved the diff empty.
	Empty bool
	// RedundantOf is the PR the close comment names as carrying the
	// change, when the caller knew one.
	RedundantOf int
}

// PRChangedFilesClaimsEmpty reports whether a single-PR GET payload says the
// PR changes no files. List payloads omit changed_files and read false
// here, so this is only ever the cheap reason to run the compare.
func PRChangedFilesClaimsEmpty(pr *gh.PullRequest) bool {
	return pr != nil && pr.ChangedFiles != nil && *pr.ChangedFiles == 0 && pr.Additions != nil && *pr.Additions == 0 && pr.Deletions != nil && *pr.Deletions == 0
}

// prDiffAgainstBase measures the PR's diff as GitHub shows it: the compare
// base...head. known is false when the compare failed or the file list
// was cut short.
func (c *Client) prDiffAgainstBase(ctx context.Context, owner, name string, pr *gh.PullRequest) (files int, known bool) {
	base := strings.TrimSpace(pr.GetBase().GetRef())
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if base == "" || head == "" {
		return 0, false
	}
	comparison, resp, err := c.client.Repositories.CompareCommits(WithRESTCaller(ctx, "hive:empty_diff_autoclose"), owner, name, base, head, &gh.ListOptions{PerPage: githubCompareFileLimit})
	if err != nil || comparison == nil {
		return 0, false
	}
	if (resp != nil && resp.NextPage != 0) || len(comparison.Files) >= githubCompareFileLimit {
		return len(comparison.Files), false
	}
	return len(comparison.Files), true
}

// CloseEmptyDiffHivePRIfEligible closes pr when the hive opened it and the
// compare against its base lists no files: the change it carried is
// already on the base. redundantOf, when known (the reviewer's
// `hive-redundant-of` marker), names the PR that carries it in the close
// comment. The close goes through the writeback gate as a supersession
// close. Idempotent: a closed PR, a PR with a diff, or a person's PR is a
// no-op. repo is "owner/name".
func (c *Client) CloseEmptyDiffHivePRIfEligible(ctx context.Context, repo string, pr *gh.PullRequest, redundantOf int) (HiveEmptyDiffClose, error) {
	out := HiveEmptyDiffClose{RedundantOf: redundantOf}
	if c == nil || c.client == nil {
		return out, ErrNoGitHubClient
	}
	if pr == nil {
		return out, nil
	}
	owner, name := c.splitRepo(repo)
	repo = owner + "/" + name
	enabled, level := c.supersededAutoClosePolicyFor(repo)
	identity := c.getHiveIdentity()
	if identity.AppLogin == "" {
		identity.AppLogin = c.appBotLogin
	}
	labels := labelNames(pr.Labels)
	facts := HiveEmptyDiffAutoCloseFacts{
		Enabled:      enabled,
		Level:        level,
		Merged:       pr.GetMerged() || !pr.GetMergedAt().IsZero() || strings.EqualFold(pr.GetState(), "closed"),
		Draft:        pr.GetDraft(),
		HiveAuthored: identity.Matches(safeGetLogin(pr.GetUser())),
		Held:         c.isHeld(labels) || c.isExempt(labels),
		KeptOpen:     supersessionHasLabel(pr, SupersessionKeepOpenLabel),
		HeadAge:      emptyDiffHeadAge(pr, time.Now()),
	}
	if pr.ChangedFiles != nil && *pr.ChangedFiles > 0 {
		// The GET payload already shows a diff: no compare needed.
		facts.DiffKnown, facts.DiffFiles = true, *pr.ChangedFiles
	}
	if !enabled || facts.Merged || facts.Draft || facts.Held || facts.KeptOpen || facts.DiffFiles > 0 {
		// Cheap guards first: no compare for a PR the rule cannot touch.
		out.Verdict, out.Reason = DecideHiveEmptyDiffAutoClose(facts)
		return out, nil
	}
	if !facts.DiffKnown {
		facts.DiffFiles, facts.DiffKnown = c.prDiffAgainstBase(ctx, owner, name, pr)
	}
	out.Empty = facts.DiffKnown && facts.DiffFiles == 0
	if out.Empty && facts.HiveAuthored {
		facts.ParkedByPerson = c.supersessionNeedsHumanByPerson(ctx, owner, name, pr)
	}
	out.Verdict, out.Reason = DecideHiveEmptyDiffAutoClose(facts)
	if out.Verdict != hiveEmptyDiffVerdictClose {
		return out, nil
	}
	if d := c.WritebackAllowed(repo, c.sentinelWritebackItem(pr), writeback.KindSupersessionClose, "close empty-diff PR"); !d.Allowed {
		out.Verdict, out.Reason = hiveEmptyDiffVerdictKeep, hiveEmptyDiffReasonGated
		return out, nil
	}
	event := SupersessionSweepEvent{
		Repo: repo, Number: pr.GetNumber(), Author: safeGetLogin(pr.GetUser()),
		IssueRepo: repo, CloserPR: redundantOf, Action: SupersessionActionClosedEmptyDiff,
	}
	closer := supersessionCloser{Repo: repo, Number: redundantOf}
	why := hiveEmptyDiffCloseReason(redundantOf, shortHeadSHA(pr), strings.TrimSpace(pr.GetBase().GetRef()))
	if _, err := c.ensureSupersessionComment(ctx, owner, name, pr.GetNumber(), renderEmptyDiffCloseComment(closer, why)); err != nil {
		return out, err
	}
	if err := c.labelAndCloseSupersededPR(ctx, owner, name, pr.GetNumber()); err != nil {
		return out, err
	}
	if sink := c.StageFacts(); sink != nil {
		if err := sink.Closed(ctx, c.PRStageRef(repo, pr.GetNumber())); err != nil && c.logger != nil {
			c.logger.Warn("empty-diff auto-close: stage close deferred to next sweep", slog.String("repo", repo), slog.Int("pr", pr.GetNumber()), slog.String("error", err.Error()))
		}
	}
	if c.logger != nil {
		c.logger.Info("empty-diff auto-close: closed hive-authored PR with no diff against its base",
			slog.String("repo", repo), slog.Int("pr", pr.GetNumber()), slog.Int("redundant_of", redundantOf), slog.String("action", event.Action))
	}
	out.Closed = true
	return out, nil
}

func emptyDiffHeadAge(pr *gh.PullRequest, now time.Time) time.Duration {
	at := pr.GetUpdatedAt().Time
	if at.IsZero() {
		at = pr.GetCreatedAt().Time
	}
	if at.IsZero() {
		return 0
	}
	return now.Sub(at)
}

func shortHeadSHA(pr *gh.PullRequest) string {
	sha := strings.TrimSpace(pr.GetHead().GetSHA())
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// hiveEmptyDiffCloseReason is the one-line why of the close comment.
func hiveEmptyDiffCloseReason(redundantOf int, head, base string) string {
	if redundantOf > 0 {
		return fmt.Sprintf("Closing this hive-authored PR as superseded by #%d: head %s is identical to `%s` (the compare lists no files), so the change it carried is already on the base. Reopen if that's wrong.", redundantOf, head, base)
	}
	return fmt.Sprintf("Closing this hive-authored PR: head %s is identical to `%s` (the compare lists no files), so there is nothing left to merge. Reopen if that's wrong.", head, base)
}

// renderEmptyDiffCloseComment is the close notice, under the supersession
// marker so ensureSupersessionComment edits it in place and the
// `hive-redundant-of` marker so the dashboard and the dedupe key read the
// superseding PR.
func renderEmptyDiffCloseComment(closer supersessionCloser, reason string) string {
	var b strings.Builder
	fmt.Fprintln(&b, supersessionSweepMarker)
	if closer.Number > 0 {
		fmt.Fprintf(&b, "%s #%d -->\n", reviewRedundantOfMarker, closer.Number)
	}
	fmt.Fprintln(&b, "The hive found that this PR's diff against its base is empty.")
	fmt.Fprintln(&b)
	switch {
	case closer.Number <= 0:
		fmt.Fprintln(&b, "- Superseded by: the base branch itself (no single PR named)")
	case closer.URL != "":
		fmt.Fprintf(&b, "- Superseded by: [#%d](%s)\n", closer.Number, closer.URL)
	default:
		fmt.Fprintf(&b, "- Superseded by: %s#%d\n", closer.Repo, closer.Number)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, reason)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "_This comment is edited in place by the hive; it is not duplicated._")
	return b.String()
}
