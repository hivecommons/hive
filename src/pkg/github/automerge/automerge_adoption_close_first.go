package automerge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Close-first supersession (hivecommons/hive#15135).
//
// A supersede-mode adoption used to open the hive's copy and leave the
// original open at 074 until the copy merged, so the operator saw two
// active PRs for one issue (#15029 and its copy #15123). Under
// governor.labels.stage.adoption.supersede.close_first (default on) the
// original is closed in the same pass the copy is opened — push branch →
// open copy → close original, so the one close note can name the copy —
// with the `hive/superseded` label, and the record reads superseded from
// the first render after the copy exists.
//
// The copy is then polled from the store (settleCloseFirstSupersessions:
// a closed original is not observed by any lane). Merged settles the
// record; closed unmerged, required checks red at adoptionCopyRedHeadsMax
// consecutive heads, or a branch the re-adoption could not push reopens the
// original with one note, drops the label and parks it at 085 with
// stagelabels.LaneReasonAdoptionCopyFailed. One reopen per original per
// stagelabels.AdoptionReopenWindow: a copy that keeps failing after that
// leaves the original closed with a note rather than flipping it.

// AdoptionCopyFailedMarker opens the note posted when the hive reopens (or,
// past the reopen guard, declines to reopen) a close-first original.
const AdoptionCopyFailedMarker = "<!-- hive:adoption-copy-failed"

// adoptionCopyRedHeadsMax is how many distinct copy heads must show red
// required checks before the original is reopened: the fixer gets one
// repair push before the hive gives the PR back to a person.
const adoptionCopyRedHeadsMax = 2

// Reasons a close-first copy failed (the `why=` field of the reopen note).
const (
	adoptionCopyFailedClosed = "copy-closed"
	adoptionCopyFailedRed    = "copy-red-twice"
)

// adoptionCloseFirstCause is the plain-words reason the close note gives
// for the supersession.
func adoptionCloseFirstCause(reason string) string {
	switch reason {
	case AdoptReasonOffline:
		return "the author's clanker is offline"
	case AdoptReasonUnregistered:
		return "the author has no registered clanker here and is not a trusted author"
	case AdoptReasonOperator:
		return "an operator asked for it"
	case AdoptReasonDCORehome, AdoptReasonSignoffRehome:
		return "its commits needed a sign-off only a hive-owned branch can carry"
	case AdoptReasonInPlaceStalled, AdoptReasonRebaseRehome, AdoptReasonPushRehome:
		return "the hive could not finish it on the contributor's branch"
	}
	return "CI stayed red and the author was unavailable"
}

// AdoptionCloseFirstComment is the one note left on an original closed in
// favour of its copy. It opens with the supersede marker so the marker
// scan still finds the adoption when the store forgot it.
func AdoptionCloseFirstComment(obs stageObservation, hiveID string, superseding int, reason string) string {
	return fmt.Sprintf("%s by=%s reason=%s mode=%s superseded_by=%d head=%s close_first=true -->\n"+
		"Superseded by hive: re-authored as #%d with the same commits and sign-off because %s. "+
		"This PR is closed; if #%d fails, hive reopens it.",
		AdoptionMarkerPrefix, nonEmpty(hiveID, "hive"), reason, stagelabels.AdoptModeSupersede, superseding, obs.HeadSHA,
		superseding, adoptionCloseFirstCause(reason), superseding)
}

// adoptionCopyFailedWhy is the plain-words clause of the reopen note.
func adoptionCopyFailedWhy(why string, redHeads int) string {
	switch why {
	case adoptionCopyFailedClosed:
		return "closed without merging"
	case adoptionCopyFailedRed:
		return fmt.Sprintf("required checks red on %d heads", redHeads)
	}
	return why
}

// AdoptionCopyFailedComment is the note posted on a reopened original.
func AdoptionCopyFailedComment(number, copy int, head, why string, redHeads int) string {
	return fmt.Sprintf("%s superseded_by=%d head=%s why=%s -->\nHive's copy #%d failed (%s); reopening this PR; needs a person (`%s`).",
		AdoptionCopyFailedMarker, copy, head, why, copy, adoptionCopyFailedWhy(why, redHeads), stagelabels.NeedsDecision.Label())
}

// AdoptionCopyFailedHeldComment is the note left when the reopen guard
// keeps a failed original closed: it was reopened for an earlier copy less
// than stagelabels.AdoptionReopenWindow ago.
func AdoptionCopyFailedHeldComment(copy int, head, why string, redHeads int, reopenedAt, now time.Time) string {
	ago := now.Sub(reopenedAt).Round(time.Minute)
	return fmt.Sprintf("%s superseded_by=%d head=%s why=%s held=true -->\nHive's copy #%d failed (%s). This PR was already reopened for an earlier copy %s ago, so the hive is not reopening it again; a person should decide what happens to it.",
		AdoptionCopyFailedMarker, copy, head, why, copy, adoptionCopyFailedWhy(why, redHeads), ago)
}

// closeSupersededOriginalFirst closes the original right after its copy
// opened. The comment goes first so GitHub shows why before the close;
// the record is marked as soon as the note is out so nothing re-posts it.
// A close GitHub refuses leaves the original open with the note (as the
// merge-time close does); any other close failure leaves the record on
// the keep-open path, where settleSuperseder closes it when the copy merges.
func (c *Engine) closeSupersededOriginalFirst(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, superseding int, reason, fence string, now time.Time) (int, error) {
	owner, repo, number := obs.Owner, obs.Repo, obs.Number
	logArgs := []any{"repo", obs.Display, "pr", number, "head", obs.HeadSHA, "superseded_by", superseding, "reason", reason}
	body := AdoptionCloseFirstComment(obs, policy.HiveID, superseding, reason) + adoptionFenceNote(fence)
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: close-first adoption note failed; original left open on the keep-open path", append(logArgs, "error", err)...)
		return 0, err
	}
	if _, _, err := c.gh.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{hgithub.SupersededLabel}); err != nil {
		c.warn("stage labels: close-first adoption label failed", append(logArgs, "label", hgithub.SupersededLabel, "error", err)...)
	}
	if _, _, err := c.gh.PullRequests.Edit(ctx, owner, repo, number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil {
		if isGitHubStatus(err, http.StatusForbidden) || isGitHubStatus(err, http.StatusNotFound) {
			store.MarkSupersededClosedFirst(key, superseding, true, now)
			c.warn("stage labels: cannot close superseded original (permission); left open with the note", append(logArgs, "error", err)...)
			return 1, nil
		}
		c.warn("stage labels: closing superseded original failed; it closes when the copy merges", append(logArgs, "error", err)...)
		return 1, err
	}
	store.MarkSupersededClosedFirst(key, superseding, false, now)
	c.info("stage labels: contributor PR closed in favour of the hive's copy", logArgs...)
	return 1, nil
}

// closeFirstRecord is one store record whose copy is due a poll, with
// the repository coordinates parsed from its key.
type closeFirstRecord struct {
	key    string
	owner  string
	repo   string
	number int
	rec    stagelabels.Record
}

// closeFirstPollCandidates lists the records whose close-first copy is due
// a poll, in key order.
func closeFirstPollCandidates(store *stagelabels.Store, now time.Time, interval time.Duration) []closeFirstRecord {
	var out []closeFirstRecord
	for _, key := range store.Keys() {
		rec, ok := store.Get(key)
		if !ok || !rec.CloseFirstPollDue(now, interval) {
			continue
		}
		owner, repo, number, ok := splitStageKey(key)
		if !ok {
			continue
		}
		out = append(out, closeFirstRecord{key: key, owner: owner, repo: repo, number: number, rec: rec})
	}
	return out
}

// splitStageKey parses "owner/repo#n".
func splitStageKey(key string) (owner, repo string, number int, ok bool) {
	full, n, found := strings.Cut(key, "#")
	if !found {
		return "", "", 0, false
	}
	owner, repo, found = strings.Cut(full, "/")
	if !found || owner == "" || repo == "" {
		return "", "", 0, false
	}
	number, err := strconv.Atoi(n)
	if err != nil || number <= 0 {
		return "", "", 0, false
	}
	return owner, repo, number, true
}

// settleCloseFirstSupersessions polls every close-first copy on record (at
// most every adoptCheckInterval each) and settles its original: merged →
// the record is finished; closed unmerged, or red at
// adoptionCopyRedHeadsMax heads → the original is reopened. It returns the
// comments posted. Runs once per ApplyStageLabels pass, after the observed
// items, because a closed original is in no lane's listing.
func (c *Engine) settleCloseFirstSupersessions(ctx context.Context, store *stagelabels.Store, policy StageLabelPolicy, now time.Time) (int, error) {
	if !policy.Adoption {
		return 0, nil
	}
	comments := 0
	var firstErr error
	for _, cand := range closeFirstPollCandidates(store, now, adoptCheckInterval) {
		store.MarkAdoptChecked(cand.key, now)
		n, err := c.settleCloseFirstCopy(ctx, store, cand, now)
		comments += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return comments, firstErr
}

// settleCloseFirstCopy reads one copy's state and acts on it.
func (c *Engine) settleCloseFirstCopy(ctx context.Context, store *stagelabels.Store, cand closeFirstRecord, now time.Time) (int, error) {
	rec, copy := cand.rec, cand.rec.AdoptedBy
	display := cand.owner + "/" + cand.repo
	logArgs := func(extra ...any) []any {
		return append([]any{"repo", display, "pr", cand.number, "head", rec.AdoptedHead, "superseded_by", copy}, extra...)
	}
	sup, _, err := c.gh.PullRequests.Get(ctx, cand.owner, cand.repo, copy)
	if err != nil {
		c.warn("stage labels: close-first copy lookup failed", logArgs("error", err)...)
		return 0, err
	}
	switch {
	case sup.GetMerged():
		store.MarkCloseFirstSettled(cand.key)
		c.info("stage labels: close-first copy merged; supersession settled", logArgs()...)
		return 0, nil
	case strings.EqualFold(sup.GetState(), "closed"):
		return c.reopenSupersededOriginal(ctx, store, cand, adoptionCopyFailedClosed, 0, now)
	}
	// The copy's own stage record carries the lane's last reason for its
	// head; red required checks at a second head mean the fixer's repair
	// did not take.
	copyRec, ok := store.Get(stagelabels.Key(display, copy))
	head := sup.GetHead().GetSHA()
	if !ok || head == "" || adoptionFailingCheck(copyRec.Reason) == "" || copyRec.ActivityHead != head {
		return 0, nil
	}
	redHeads := store.NoteAdoptCopyRedHead(cand.key, head)
	if redHeads < adoptionCopyRedHeadsMax {
		c.info("stage labels: close-first copy red", logArgs("copy_head", head, "copy_reason", copyRec.Reason, "red_heads", redHeads)...)
		return 0, nil
	}
	return c.reopenSupersededOriginal(ctx, store, cand, adoptionCopyFailedRed, redHeads, now)
}

// reopenSupersededOriginal gives a close-first original back to a person:
// reopen, one note, `hive/superseded` off, parked at 085 with
// LaneReasonAdoptionCopyFailed. Within stagelabels.AdoptionReopenWindow of
// the last reopen the original stays closed and the note says so.
func (c *Engine) reopenSupersededOriginal(ctx context.Context, store *stagelabels.Store, cand closeFirstRecord, why string, redHeads int, now time.Time) (int, error) {
	rec, copy := cand.rec, cand.rec.AdoptedBy
	owner, repo, number := cand.owner, cand.repo, cand.number
	logArgs := []any{"repo", owner + "/" + repo, "pr", number, "head", rec.AdoptedHead, "superseded_by", copy, "why", why, "red_heads", redHeads}
	if rec.AdoptionReopenedWithin(now, stagelabels.AdoptionReopenWindow) {
		store.MarkCloseFirstSettled(cand.key)
		c.warn("stage labels: close-first copy failed again within the reopen window; original stays closed", append(logArgs, "reopened_at", rec.AdoptReopenedAt)...)
		body := AdoptionCopyFailedHeldComment(copy, rec.AdoptedHead, why, redHeads, rec.AdoptReopenedAt, now)
		if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			c.warn("stage labels: close-first held note failed", append(logArgs, "error", err)...)
			return 0, err
		}
		return 1, nil
	}
	if _, _, err := c.gh.PullRequests.Edit(ctx, owner, repo, number, &gh.PullRequest{State: gh.Ptr("open")}); err != nil {
		c.warn("stage labels: reopening superseded original failed", append(logArgs, "error", err)...)
		return 0, err
	}
	// The reopen is out: record it before the note so a failed comment
	// never reopens twice.
	store.MarkAdoptionReopened(cand.key, copy, now)
	store.MarkAdoptionEscalated(cand.key, rec.AdoptedHead, stagelabels.LaneReasonAdoptionCopyFailed)
	c.warn("stage labels: hive's copy failed; original reopened for a person", logArgs...)
	comments := 0
	var firstErr error
	body := AdoptionCopyFailedComment(number, copy, rec.AdoptedHead, why, redHeads)
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: reopen note failed", append(logArgs, "error", err)...)
		firstErr = err
	} else {
		comments++
	}
	if _, err := c.gh.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(hgithub.SupersededLabel)); err != nil && !isGitHubStatus(err, http.StatusNotFound) {
		c.warn("stage labels: removing superseded label failed", append(logArgs, "label", hgithub.SupersededLabel, "error", err)...)
		if firstErr == nil {
			firstErr = err
		}
	}
	return comments, firstErr
}
