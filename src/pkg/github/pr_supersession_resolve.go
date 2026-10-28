package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Self-resolution of a superseded hive-authored PR (hivecommons/hive#13799).
//
// The supersession sweep used to hand every "claimed issue closed by another
// merged PR, but this PR touches files that PR did not" case to a person by
// adding a bare `needs-human` label next to whatever lane stage the PR was
// in (#13782 sat at stage/050-review-pending wearing needs-human). For a PR
// the hive wrote that is the hive's own call to make: it updates the head
// onto the base so the merged PR's part of the diff drops out, measures what
// is left, and either rewrites `Closes #N` to `Refs #N (closed by #M; …)` and
// carries on in the lane, or — when nothing meaningful remains — closes the
// PR itself. Only a contributor's PR still escalates, and then through the
// stage engine (stage/085-needs-decision via a Parked fact) so the chevron
// and the label agree.
const (
	// SupersessionActionRebased: the sweep asked GitHub to update the hive
	// PR's head onto its base; the remaining diff is measured next pass.
	SupersessionActionRebased = "rebased"
	// SupersessionActionRebaseFailed: GitHub refused the branch update (a
	// conflict, usually); the fix lane owns the rebase, the sweep waits.
	SupersessionActionRebaseFailed = "rebase-failed"
	// SupersessionActionResolved: the hive PR kept its extra files; its body
	// now references the closed issue instead of closing it.
	SupersessionActionResolved = "resolved"
	// SupersessionActionEscalated: a person owes the decision; the PR moved
	// to stage/085-needs-decision through the stage engine.
	SupersessionActionEscalated = "commented-needs-human"

	// supersessionResolvedMarkerPrefix is stamped into a resolved PR's body
	// so later sweeps recognise the reference as their own rewrite and do
	// not treat the (still closed) issue as a fresh supersession.
	supersessionResolvedMarkerPrefix = "<!-- hive:supersession-resolved "
	// supersessionRebaseRefusedPhrase opens the refusal wording so a later
	// pass can recognise its own notice for an unchanged head.
	supersessionRebaseRefusedPhrase = "GitHub refused to update"
	// supersessionResolvedFilesShown caps the files named inline in the
	// rewritten reference and the comment.
	supersessionResolvedFilesShown = 5
)

// supersessionResolvedMarker renders the body marker for one issue/closer pair.
func supersessionResolvedMarker(issue, closer int) string {
	return fmt.Sprintf("%sissue=%d closer=%d -->", supersessionResolvedMarkerPrefix, issue, closer)
}

// supersessionResolvedInBody reports whether the sweep already rewrote this
// PR's claim on issue after closer merged. The marker, not the `Refs` text,
// is the signal: a reference the author wrote is still a candidate.
func supersessionResolvedInBody(body string, issue, closer int) bool {
	return strings.Contains(body, supersessionResolvedMarker(issue, closer))
}

// rewriteSupersededClaim turns every closing claim on issue in body into a
// reference that records who closed the issue and what this PR still adds,
// and stamps the resolved marker. A body with no closing claim (the issue
// was found through a reference or the branch name) gets the reference and
// the marker appended. ok is false only when body already carries the marker.
func rewriteSupersededClaim(body string, issue, closer int, extraFiles []string) (string, bool) {
	if supersessionResolvedInBody(body, issue, closer) {
		return body, false
	}
	reference := fmt.Sprintf("(closed by #%d; this PR adds %s)", closer, supersessionFilesSummary(extraFiles))
	replaced := false
	out := claimRefPattern.ReplaceAllStringFunc(body, func(m string) string {
		sub := claimRefPattern.FindStringSubmatch(m)
		if len(sub) < 4 || sub[3] != fmt.Sprint(issue) {
			return m
		}
		replaced = true
		ref := "#" + sub[3]
		if sub[2] != "" {
			ref = sub[2] + ref
		}
		return "Refs " + ref + " " + reference
	})
	out = strings.TrimRight(out, " \t\r\n")
	if !replaced {
		if out != "" {
			out += "\n\n"
		}
		out += fmt.Sprintf("Refs #%d %s", issue, reference)
	}
	return out + "\n\n" + supersessionResolvedMarker(issue, closer) + "\n", true
}

// supersessionFilesSummary names up to supersessionResolvedFilesShown files
// in a stable order, with a count for the rest.
func supersessionFilesSummary(files []string) string {
	if len(files) == 0 {
		return "no files"
	}
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	shown := sorted
	rest := 0
	if len(shown) > supersessionResolvedFilesShown {
		shown = shown[:supersessionResolvedFilesShown]
		rest = len(sorted) - supersessionResolvedFilesShown
	}
	quoted := make([]string, 0, len(shown))
	for _, f := range shown {
		quoted = append(quoted, "`"+f+"`")
	}
	out := strings.Join(quoted, ", ")
	if rest > 0 {
		out += fmt.Sprintf(" and %d more", rest)
	}
	return out
}

// supersessionExtraFiles is the sorted set of files the open PR touches that
// the closer did not.
func supersessionExtraFiles(open, closer map[string]bool) []string {
	var out []string
	for f := range open {
		if !closer[f] {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// parkSupersededPR is the one way the sweep hands a PR to a person: the
// `needs-human` label (what the lanes read, so the park survives the next
// automerge pass) and a Parked fact, so the stage engine writes
// stage/085-needs-decision now rather than leaving the lane stage standing
// beside the label until the automerge sweep next looks (#13782 wore
// stage/050-review-pending + needs-human for fifteen minutes).
func (c *Client) parkSupersededPR(ctx context.Context, prRepo string, number int) error {
	if err := c.AddLabels(ctx, prRepo, number, []string{issueNeedsHumanLabel, prstage.SupersessionPendingLabel}); err != nil {
		return fmt.Errorf("labeling superseded PR %s#%d: %w", prRepo, number, err)
	}
	sink := c.StageFacts()
	if sink == nil {
		return nil
	}
	if err := sink.Parked(ctx, c.PRStageRef(prRepo, number), issueNeedsHumanLabel); err != nil {
		return fmt.Errorf("parking superseded PR %s#%d for a decision: %w", prRepo, number, err)
	}
	return nil
}

// supersessionNeedsHumanByPerson reports whether a `needs-human` on the PR
// was put there by someone other than the hive. A person's park is an
// instruction the self-resolution must not undo; the hive's own (an earlier
// sweep's escalation) is the state this code replaces.
func (c *Client) supersessionNeedsHumanByPerson(ctx context.Context, owner, repo string, pr *gh.PullRequest) bool {
	if !supersessionHasLabel(pr, issueNeedsHumanLabel) {
		return false
	}
	return c.needsHumanOutcome(ctx, owner, repo, pr.GetNumber(), nil).applier != stageengine.NeedsHumanApplierHive
}

// releaseSupersessionPark removes the sweep's own `needs-human` once the
// hive has taken the resolution over. The stage engine re-derives the lane
// stage from the next automerge pass (leaving 085 is never a regression).
func (c *Client) releaseSupersessionPark(ctx context.Context, prRepo string, pr *gh.PullRequest) error {
	if supersessionHasLabel(pr, prstage.SupersessionPendingLabel) {
		if err := c.RemoveLabel(ctx, prRepo, pr.GetNumber(), prstage.SupersessionPendingLabel); err != nil {
			return err
		}
	}
	if !supersessionHasLabel(pr, issueNeedsHumanLabel) {
		return nil
	}
	if err := c.RemoveLabel(ctx, prRepo, pr.GetNumber(), issueNeedsHumanLabel); err != nil {
		return fmt.Errorf("releasing needs-human on superseded PR %s#%d: %w", prRepo, pr.GetNumber(), err)
	}
	return nil
}

// SupersessionActionReleased: the sweep took its own `needs-human` off a
// PR it had escalated because the cause is gone — the claimed issue was
// reopened, the claim rewritten, or the link turned out to be a reference
// rather than a closing claim (hivecommons/hive#14155).
const SupersessionActionReleased = "released-needs-human"

// supersessionEscalationMark is the phrase every escalation notice carries;
// a sweep comment without it (a resolved / kept-open notice) parked nothing.
const supersessionEscalationMark = "**Decision needed"

// releaseStaleSupersessionPark lifts the sweep's own escalation when the
// supersession it announced no longer holds. The park's cause is the sweep
// notice: a PR carrying `needs-human` that the hive applied while the
// sweep's "Decision needed" notice stands is the sweep's park, and it clears
// with the cause instead of waiting for a maintainer to strip the label
// (#13959 and #14090 sat at stage/085-needs-decision, verdict approved and
// CI green, for a label whose cause had gone). A person's `needs-human`,
// one the hive cannot attribute, or one next to no sweep notice is left
// alone. The lane re-derives the stage from the labels on its next pass.
func (c *Client) releaseStaleSupersessionPark(ctx context.Context, prRepo, owner, repo string, pr *gh.PullRequest) (SupersessionSweepEvent, string, error) {
	changed := false
	if supersessionHasLabel(pr, prstage.SupersessionPendingLabel) {
		if err := c.RemoveLabel(ctx, prRepo, pr.GetNumber(), prstage.SupersessionPendingLabel); err != nil {
			return SupersessionSweepEvent{}, "", err
		}
		changed = true
	}
	if supersessionHasLabel(pr, issueNeedsHumanLabel) {
		notice, err := c.findSupersessionComment(ctx, owner, repo, pr.GetNumber())
		if err != nil {
			return SupersessionSweepEvent{}, "", err
		}
		if notice != nil && strings.Contains(notice.GetBody(), supersessionEscalationMark) && !c.supersessionNeedsHumanByPerson(ctx, owner, repo, pr) {
			if err := c.RemoveLabel(ctx, prRepo, pr.GetNumber(), issueNeedsHumanLabel); err != nil {
				return SupersessionSweepEvent{}, "", err
			}
			changed = true
		}
	}
	if !changed {
		return SupersessionSweepEvent{}, "", nil
	}
	event := SupersessionSweepEvent{Repo: prRepo, Number: pr.GetNumber(), Author: safeGetLogin(pr.GetUser()), Action: SupersessionActionReleased}
	return event, "resolved", nil
}

// prHeadBehindBase is how many base commits the PR head lacks; 0 means the
// head already contains the base tip, so GitHub's file list for the PR is
// the diff against the base as it stands.
func (c *Client) prHeadBehindBase(ctx context.Context, owner, repo string, pr *gh.PullRequest) (int, error) {
	base := strings.TrimSpace(pr.GetBase().GetRef())
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if base == "" || head == "" {
		return 0, fmt.Errorf("PR %s/%s#%d: base ref or head sha missing from the listing", owner, repo, pr.GetNumber())
	}
	cmp, _, err := c.client.Repositories.CompareCommits(ctx, owner, repo, base, head, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return 0, fmt.Errorf("comparing %s...%s for PR %s/%s#%d: %w", base, stagelabels.ShortSHA(head), owner, repo, pr.GetNumber(), err)
	}
	return cmp.GetBehindBy(), nil
}

// resolveOwnSupersededPR decides a hive-authored PR whose claimed issue a
// different merged PR closed. closerFiles are the merged PR's files; subset
// says every file this PR touches is among them.
func (c *Client) resolveOwnSupersededPR(ctx context.Context, prRepo, owner, repo string, pr *gh.PullRequest, event SupersessionSweepEvent, closer supersessionCloser, closerFiles map[string]bool, subset bool, level int, autoClose bool) (SupersessionSweepEvent, string, error) {
	number := pr.GetNumber()
	if c.supersessionNeedsHumanByPerson(ctx, owner, repo, pr) {
		// A person parked it; the hive does not resolve over them.
		return event, "", nil
	}
	if level < acmmLevelAutonomous {
		return c.escalateSupersededPR(ctx, prRepo, owner, repo, pr, event, closer,
			"this hive is below ACMM L6, so the sweep will not close or update PRs automatically. A human should decide whether to close this PR.")
	}
	if subset {
		event.Action = "closed"
		body := renderSupersessionComment(event, closer, "Closing this hive-authored PR because the claimed issue was already closed by the merged PR below.")
		if _, err := c.ensureSupersessionComment(ctx, owner, repo, number, body); err != nil {
			return event, "", err
		}
		if err := c.labelAndCloseSupersededPR(ctx, owner, repo, number); err != nil {
			return event, "", err
		}
		return event, "closed", nil
	}

	head := strings.TrimSpace(pr.GetHead().GetSHA())
	baseRef := strings.TrimSpace(pr.GetBase().GetRef())
	behind, err := c.prHeadBehindBase(ctx, owner, repo, pr)
	if err != nil {
		return event, "", err
	}
	if autoClose && prMergeableStateDirty(pr) {
		// The payload already says the head conflicts: nothing it still
		// adds can be measured, and the issues it closes are closed.
		event.Action = SupersessionActionClosedConflicting
		if err := c.closeHiveSupersededPR(ctx, owner, repo, pr, event, closer, true); err != nil {
			return event, "", err
		}
		return event, "closed", nil
	}
	if behind > 0 {
		return c.rebaseOwnSupersededPR(ctx, prRepo, owner, repo, pr, event, closer, head, baseRef, autoClose)
	}

	remaining, err := c.prFileSet(ctx, owner, repo, number)
	if err != nil {
		return event, "", err
	}
	extra := supersessionExtraFiles(remaining, closerFiles)
	if len(remaining) == 0 || len(extra) == 0 {
		event.Action = "closed"
		why := fmt.Sprintf("Closing this hive-authored PR: with head %s current with `%s`, the diff left after #%d merged is empty.", stagelabels.ShortSHA(head), baseRef, closer.Number)
		if len(remaining) > 0 {
			why = fmt.Sprintf("Closing this hive-authored PR: with head %s current with `%s`, the diff left after #%d merged only re-touches files that PR already changed (%s), so there is nothing of its own to keep. Reopen if that's wrong.", stagelabels.ShortSHA(head), baseRef, closer.Number, supersessionFilesSummary(supersessionSortedFiles(remaining)))
		}
		if _, err := c.ensureSupersessionComment(ctx, owner, repo, number, renderSupersessionComment(event, closer, why)); err != nil {
			return event, "", err
		}
		if err := c.labelAndCloseSupersededPR(ctx, owner, repo, number); err != nil {
			return event, "", err
		}
		return event, "closed", nil
	}

	body, ok := rewriteSupersededClaim(pr.GetBody(), event.Issue, closer.Number, extra)
	if !ok {
		return event, "", nil
	}
	event.Action = SupersessionActionResolved
	note := fmt.Sprintf("Keeping this hive-authored PR open: with head %s current with `%s`, it still adds %d file(s) #%d did not touch: %s. Its body now reads `Refs #%d (closed by #%d; …)` instead of `Closes #%d`, and it continues in the lane.",
		stagelabels.ShortSHA(head), baseRef, len(extra), closer.Number, supersessionFilesSummary(extra), event.Issue, closer.Number, event.Issue)
	if _, _, err := c.client.PullRequests.Edit(WithRESTCaller(ctx, "hive:supersession_resolve"), owner, repo, number, &gh.PullRequest{Body: gh.Ptr(body)}); err != nil {
		return event, "", fmt.Errorf("rewriting body of superseded PR %s#%d: %w", prRepo, number, err)
	}
	if _, err := c.ensureSupersessionComment(ctx, owner, repo, number, renderSupersessionComment(event, closer, note)); err != nil {
		return event, "", err
	}
	if err := c.releaseSupersessionPark(ctx, prRepo, pr); err != nil {
		return event, "", err
	}
	return event, "resolved", nil
}

// rebaseOwnSupersededPR asks GitHub to update the PR head onto its base so
// the next pass can measure the remaining diff. The notice names the head,
// so an unchanged head (the update is still queued, or GitHub refused it)
// is recognised and not re-requested every tick.
func (c *Client) rebaseOwnSupersededPR(ctx context.Context, prRepo, owner, repo string, pr *gh.PullRequest, event SupersessionSweepEvent, closer supersessionCloser, head, baseRef string, autoClose bool) (SupersessionSweepEvent, string, error) {
	number := pr.GetNumber()
	short := stagelabels.ShortSHA(head)
	updating := renderSupersessionComment(event, closer, fmt.Sprintf("This PR touches files the merged PR did not, so the hive is resolving it itself: updating head %s onto `%s` to measure what #%d left for it to add. If nothing meaningful remains it will close; otherwise its claim becomes `Refs` and it continues in the lane.", short, baseRef, closer.Number))
	failed := func(reason string) string {
		return renderSupersessionComment(event, closer, fmt.Sprintf("This PR touches files the merged PR did not, so the hive is resolving it itself, but %s head %s onto `%s` (%s). The fix lane owns the rebase; the sweep re-measures the diff once a new head lands.", supersessionRebaseRefusedPhrase, short, baseRef, reason))
	}
	existing, err := c.findSupersessionComment(ctx, owner, repo, number)
	if err != nil {
		return event, "", err
	}
	if existing != nil {
		// Same head, same notice: the update is still queued or was
		// refused; nothing new to do until the head moves.
		body := existing.GetBody()
		if body == updating || (strings.Contains(body, supersessionRebaseRefusedPhrase) && strings.Contains(body, " head "+short+" ")) {
			return event, "", nil
		}
	}
	event.Action = SupersessionActionRebased
	notice := updating
	if err := c.UpdateBranchExpectedHead(ctx, prRepo, number, head, strings.TrimSpace(pr.GetBase().GetSHA())); err != nil {
		var ghErr *gh.ErrorResponse
		if !errors.As(err, &ghErr) || ghErr.Response == nil {
			return event, "", err
		}
		event.Action = SupersessionActionRebaseFailed
		reason := strings.TrimSpace(ghErr.Message)
		if reason == "" {
			reason = fmt.Sprintf("HTTP %d", ghErr.Response.StatusCode)
		}
		if autoClose && supersessionUpdateRefusedForConflict(ghErr) {
			// GitHub refused the update because the head conflicts with
			// the base. The fix lane's rebase would serve an issue another
			// PR already delivered; the hive closes its own PR instead of
			// parking it on a person (hivecommons/hive#14965 sat at 085
			// for over an hour this way).
			event.Action = SupersessionActionClosedConflicting
			if err := c.closeHiveSupersededPR(ctx, owner, repo, pr, event, closer, true); err != nil {
				return event, "", err
			}
			return event, "closed", nil
		}
		notice = failed(reason)
	}
	// One comment write per outcome: the notice is posted after the update
	// call so a refusal does not leave an "updating" notice behind.
	if _, err := c.ensureSupersessionComment(ctx, owner, repo, number, notice); err != nil {
		return event, "", err
	}
	if event.Action == SupersessionActionRebaseFailed {
		return event, "resolved", nil
	}
	if err := c.releaseSupersessionPark(ctx, prRepo, pr); err != nil {
		return event, "", err
	}
	return event, "resolved", nil
}

// supersessionUpdateRefusedForConflict reports whether GitHub's refusal of
// an update-branch call is the merge-conflict one (HTTP 422 "merge
// conflict between base and head") rather than a transient or permission
// failure.
func supersessionUpdateRefusedForConflict(err *gh.ErrorResponse) bool {
	if err == nil || err.Response == nil {
		return false
	}
	if err.Response.StatusCode != http.StatusUnprocessableEntity {
		return false
	}
	msg := strings.ToLower(err.Message)
	return strings.Contains(msg, "conflict")
}

// escalateSupersededPR posts (or edits) the notice and parks the PR at
// stage/085-needs-decision through the stage engine. An unchanged notice
// means the escalation was delivered on an earlier pass: a human or hive
// that cleared the park for this reason must not see it re-applied every
// tick (hivecommons/hive#12349).
func (c *Client) escalateSupersededPR(ctx context.Context, prRepo, owner, repo string, pr *gh.PullRequest, event SupersessionSweepEvent, closer supersessionCloser, reason string) (SupersessionSweepEvent, string, error) {
	event.Action = SupersessionActionEscalated
	changed, err := c.ensureSupersessionComment(ctx, owner, repo, pr.GetNumber(), renderSupersessionComment(event, closer, reason))
	if err != nil {
		return event, "", err
	}
	if !changed {
		if supersessionHasLabel(pr, issueNeedsHumanLabel) && !supersessionHasLabel(pr, prstage.SupersessionPendingLabel) {
			if err := c.parkSupersededPR(ctx, prRepo, pr.GetNumber()); err != nil {
				return event, "", err
			}
			return event, "commented", nil
		}
		return event, "", nil
	}
	if err := c.parkSupersededPR(ctx, prRepo, pr.GetNumber()); err != nil {
		return event, "", err
	}
	return event, "commented", nil
}

func supersessionSortedFiles(files map[string]bool) []string {
	out := make([]string, 0, len(files))
	for f := range files {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// Ignore ancillary additions only when at least one implementation file also
// overlaps. A docs-only PR or unrelated implementation is never a duplicate
// merely because it mentions the same closed issue.
func supersessionBlockingExtraFiles(open, closer map[string]bool) []string {
	ancillary := func(f string) bool {
		return strings.HasSuffix(f, "_test.go") || strings.HasSuffix(f, ".md") || strings.HasPrefix(f, "docs/") || strings.Contains(f, "/docs/") || strings.HasPrefix(f, "changelog.d/")
	}
	overlap := false
	for f := range open {
		if closer[f] && !ancillary(f) {
			overlap = true
		}
	}
	extra := supersessionExtraFiles(open, closer)
	if !overlap {
		return extra
	}
	var blocking []string
	for _, f := range extra {
		if !ancillary(f) {
			blocking = append(blocking, f)
		}
	}
	return blocking
}
