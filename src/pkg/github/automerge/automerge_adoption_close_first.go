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
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/config"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
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
//
// Closing, labelling and reopening a contributor's original are unsolicited
// writes on a PR the hive did not author, so every one asks the write-back
// gate (writeback.KindContributorAdopt) first. A refused close-first leaves
// the original open on the keep-open path; a refused reopen leaves the
// record due its next poll, so the reopen lands once the gate allows it
// (e.g. when an operator lifts a repo pause).
//
// An agent-authored original (a contributor's clanker or another hive,
// hivecommons/hive#15526) is the exception: under
// governor.labels.stage.adoption.close_superseded: on_adopt (default) the
// gate item names the copy that carries its commits and the gate allows the
// close by policy (writeback.ReasonSupersededAgentPR) — no repo opt-in
// label is asked for, because the pair is one logical PR of the hive's own.
// on_merge skips close-first for those and lets settleSuperseder close them
// when the copy merges. A human's original is never closed on adopt without
// the opt-in label.

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

// closeFirstGateItem is the write-back gate's view of an original about to
// be closed in favour of its copy `superseding`. An agent-authored
// original (author/clanker, author/hive) under close_superseded on_adopt
// (the default) names the copy so the gate allows the close by policy
// (writeback.ReasonSupersededAgentPR); under on_merge it reports onMerge
// and the caller leaves the original to settleSuperseder. A person's
// original is the plain gated item whatever the policy says.
func closeFirstGateItem(obs stageObservation, policy StageLabelPolicy, superseding int) (item writeback.Item, onMerge bool) {
	item = writebackItemForObservation(obs)
	if obs.AuthorKind != authorkind.Clanker && obs.AuthorKind != authorkind.Hive {
		return item, false
	}
	if policy.AdoptCloseSuperseded == config.AdoptionCloseSupersededOnMerge {
		return item, true
	}
	item.AgentAuthored, item.SupersededBy = true, superseding
	return item, false
}

// closeSupersededOriginalFirst closes the original right after its copy
// opened. The comment goes first so GitHub shows why before the close;
// the record is marked as soon as the note is out so nothing re-posts it.
// A close GitHub refuses leaves the original open with the note (as the
// merge-time close does); any other close failure leaves the record on
// the keep-open path, where settleSuperseder closes it when the copy merges.
// A write-back gate refusal writes nothing and leaves it on that path too.
func (c *Engine) closeSupersededOriginalFirst(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, superseding int, reason, fence string, now time.Time) (int, error) {
	owner, repo, number := obs.Owner, obs.Repo, obs.Number
	logArgs := []any{"repo", obs.Display, "pr", number, "head", obs.HeadSHA, "superseded_by", superseding, "reason", reason}
	body := AdoptionCloseFirstComment(obs, policy.HiveID, superseding, reason) + adoptionFenceNote(fence)
	item, onMerge := closeFirstGateItem(obs, policy, superseding)
	if onMerge {
		c.info("stage labels: agent-authored original left open until the copy merges (close_superseded: on_merge)", logArgs...)
		return 0, nil
	}
	if d := c.writebackAllowed(obs.Display, item, writeback.KindContributorAdopt, body); !d.Allowed {
		c.info("stage labels: close-first adoption suppressed by the write-back gate; original left open on the keep-open path", append(logArgs, "writeback_reason", d.Reason, "writeback_detail", d.Detail)...)
		return 0, nil
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		c.warn("stage labels: close-first adoption note failed; original left open on the keep-open path", append(logArgs, "error", err)...)
		return 0, err
	}
	if _, _, err := c.gh.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{hgithub.SupersededLabel}); err != nil { // writeback:gated
		c.warn("stage labels: close-first adoption label failed", append(logArgs, "label", hgithub.SupersededLabel, "error", err)...)
	}
	if _, _, err := c.gh.PullRequests.Edit(ctx, owner, repo, number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil { // writeback:gated
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
// the last reopen the original stays closed and the note says so. A
// write-back gate refusal writes nothing and settles nothing: the record
// stays due its next poll. A pause is logged at info (it lifts); any other
// refusal means the policy moved under a PR the hive closed, and is warned.
func (c *Engine) reopenSupersededOriginal(ctx context.Context, store *stagelabels.Store, cand closeFirstRecord, why string, redHeads int, now time.Time) (int, error) {
	rec, copy := cand.rec, cand.rec.AdoptedBy
	owner, repo, number := cand.owner, cand.repo, cand.number
	logArgs := []any{"repo", owner + "/" + repo, "pr", number, "head", rec.AdoptedHead, "superseded_by", copy, "why", why, "red_heads", redHeads}
	held := rec.AdoptionReopenedWithin(now, stagelabels.AdoptionReopenWindow)
	preview := AdoptionCopyFailedComment(number, copy, rec.AdoptedHead, why, redHeads)
	if held {
		preview = AdoptionCopyFailedHeldComment(copy, rec.AdoptedHead, why, redHeads, rec.AdoptReopenedAt, now)
	}
	if d := c.writebackAllowed(owner+"/"+repo, c.closeFirstOriginalItem(ctx, cand), writeback.KindContributorAdopt, preview); !d.Allowed {
		gateArgs := append(logArgs, "writeback_reason", d.Reason, "writeback_detail", d.Detail)
		if d.Reason == writeback.ReasonRepoPaused || writeback.IsContentReason(d.Reason) {
			c.info("stage labels: close-first original reopen suppressed by the write-back gate; retried at the next poll", gateArgs...)
			return 0, nil
		}
		// Not a pause: the contributor_adopt policy changed after the hive
		// closed this PR, so the reopen that would undo that close no
		// longer clears the gate. The PR stays closed until the policy
		// allows it again or a person reopens it; say so loudly.
		c.warn("stage labels: close-first original reopen refused by the write-back gate; hive-closed PR stays closed until the contributor_adopt policy allows the reopen or a person reopens it", gateArgs...)
		return 0, nil
	}
	if held {
		store.MarkCloseFirstSettled(cand.key)
		c.warn("stage labels: close-first copy failed again within the reopen window; original stays closed", append(logArgs, "reopened_at", rec.AdoptReopenedAt)...)
		if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(preview)}); err != nil { // writeback:gated
			c.warn("stage labels: close-first held note failed", append(logArgs, "error", err)...)
			return 0, err
		}
		return 1, nil
	}
	if _, _, err := c.gh.PullRequests.Edit(ctx, owner, repo, number, &gh.PullRequest{State: gh.Ptr("open")}); err != nil { // writeback:gated
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
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(preview)}); err != nil { // writeback:gated
		c.warn("stage labels: reopen note failed", append(logArgs, "error", err)...)
		firstErr = err
	} else {
		comments++
	}
	if _, err := c.gh.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape(hgithub.SupersededLabel)); err != nil && !isGitHubStatus(err, http.StatusNotFound) { // writeback:gated
		c.warn("stage labels: removing superseded label failed", append(logArgs, "label", hgithub.SupersededLabel, "error", err)...)
		if firstErr == nil {
			firstErr = err
		}
	}
	return comments, firstErr
}

// closeFirstOriginalItem is the gate's view of a closed original. No lane
// lists a closed PR, so with a gate installed it is read once here (the
// labels decide the opt-in); without one, or when the read fails, the bare
// number stands in, which the gate treats as carrying no opt-in label.
func (c *Engine) closeFirstOriginalItem(ctx context.Context, cand closeFirstRecord) writeback.Item {
	item := writeback.Item{Number: cand.number, IsPR: true}
	if c.writebackGate() == nil {
		return item
	}
	pr, _, err := c.gh.PullRequests.Get(ctx, cand.owner, cand.repo, cand.number)
	if err != nil || pr == nil {
		c.warn("stage labels: close-first original lookup for the write-back gate failed", "repo", cand.owner+"/"+cand.repo, "pr", cand.number, "error", err)
		return item
	}
	item.Author = pr.GetUser().GetLogin()
	for _, l := range pr.Labels {
		item.Labels = append(item.Labels, l.GetName())
	}
	item.CreatedAt = pr.GetCreatedAt().Time
	return item
}
