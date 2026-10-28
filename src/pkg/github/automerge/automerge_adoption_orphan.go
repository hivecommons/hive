package automerge

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Orphaned adoptions (hivecommons/hive#15052).
//
// A supersede-mode adoption hands the contributor's head to a hive-authored
// copy and parks the original at 074 until that copy merges. When the copy
// is closed without merging — the duplicate-fix sweep closed #14977 under
// #14947 six minutes after the adoption — the original used to forget the
// adoption and fall back to whatever stage its lane derived. On #14947 that
// was 085 (reviewer verdict requires_human), where the 072 adoption
// machinery never runs: the PR sat with "Needs human decision" while the
// hive believed the question was answered on a PR that no longer existed.
//
// The reconciler now treats a closed-unmerged copy as an orphan and
// re-adopts from the same head: a fresh copy, a fresh marker, one log line
// naming the old and new numbers. When the head moved or the fresh copy
// cannot be opened, the adoption is reverted with a plain note so the PR
// rejoins its normal flow instead of waiting on nothing. A copy closed again
// within stagelabels.AdoptionOrphanLoopWindow is a loop: the PR parks at 085
// with LaneReasonAdoptionChildClosedTwice and no third copy is opened.
//
// Detection has two sources. The store path runs where the superseder is
// polled (settleSuperseder) and covers every adoption on record. The marker
// path covers a record that already forgot its adoption (every store written
// before this change, or one rebuilt from scratch): once per head, a PR at
// 085 whose author is a clanker has its comments read for the last
// `hive:adopted … mode=supersede superseded_by=N` marker, and the copy's
// state settles it the same way.

// AdoptionRevertedMarker opens the note posted when an orphaned adoption
// cannot be re-run and the PR returns to its pre-adoption flow.
const AdoptionRevertedMarker = "<!-- hive:adoption-reverted"

// AdoptionChildClosedTwiceMarker opens the note posted when the loop guard
// parks an adoption whose copies keep being closed.
const AdoptionChildClosedTwiceMarker = "<!-- hive:adoption-child-closed-twice"

// adoptionOrphanRevert names the reasons a revert note carries.
const (
	adoptionRevertHeadMoved   = "head-moved"
	adoptionRevertReadoptFail = "readopt-failed"
)

var (
	adoptionMarkerSupersededByRE = regexp.MustCompile(`\bsuperseded_by=(\d+)\b`)
	adoptionMarkerHeadRE         = regexp.MustCompile(`\bhead=([0-9a-fA-F]{7,64})\b`)
	adoptionMarkerModeRE         = regexp.MustCompile(`\bmode=([a-z-]+)\b`)
)

// AdoptionMarker is one `hive:adopted … mode=supersede` marker read back
// from a PR comment.
type AdoptionMarker struct {
	SupersededBy int
	Head         string
	At           time.Time
}

// ParseAdoptionMarker reads a supersede-mode adoption marker from one
// comment body; false for anything else (in-place markers included).
func ParseAdoptionMarker(body string) (AdoptionMarker, bool) {
	body = strings.TrimSpace(body)
	if !strings.HasPrefix(body, AdoptionMarkerPrefix) {
		return AdoptionMarker{}, false
	}
	end := strings.Index(body, "-->")
	if end < 0 {
		return AdoptionMarker{}, false
	}
	head := body[:end]
	if m := adoptionMarkerModeRE.FindStringSubmatch(head); m == nil || m[1] != stagelabels.AdoptModeSupersede {
		return AdoptionMarker{}, false
	}
	m := adoptionMarkerSupersededByRE.FindStringSubmatch(head)
	if m == nil {
		return AdoptionMarker{}, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return AdoptionMarker{}, false
	}
	out := AdoptionMarker{SupersededBy: n}
	if h := adoptionMarkerHeadRE.FindStringSubmatch(head); h != nil {
		out.Head = h[1]
	}
	return out, true
}

// AdoptionSettledMarker reports a comment that ends the adoption the
// marker before it opened: the copy merged and the original was told, the
// adoption was reverted or loop-parked, yielded, or released.
func AdoptionSettledMarker(body string) bool {
	body = strings.TrimSpace(body)
	for _, prefix := range []string{adoptionCloseMarker, AdoptionRevertedMarker, AdoptionChildClosedTwiceMarker, AdoptionYieldMarker, AdoptionSupersederMergedDivergedMarker, AdoptionMaintainerSupersederMarker} {
		if strings.HasPrefix(body, prefix) {
			return true
		}
	}
	return false
}

// AdoptionMarkerScan is what ScanAdoptionMarkers found: the last live
// supersede marker (Live false when none, or a later note settled it) and
// how many supersede markers the PR carries in all.
type AdoptionMarkerScan struct {
	Live   bool
	Marker AdoptionMarker
	Count  int
}

// ScanAdoptionMarkers reads the comments in posting order.
func ScanAdoptionMarkers(comments []*gh.IssueComment) AdoptionMarkerScan {
	var out AdoptionMarkerScan
	for _, c := range comments {
		if c == nil {
			continue
		}
		body := c.GetBody()
		if m, ok := ParseAdoptionMarker(body); ok {
			m.At = c.GetCreatedAt().Time
			out.Marker, out.Live = m, true
			out.Count++
			continue
		}
		if out.Live && AdoptionSettledMarker(body) {
			out.Live = false
		}
	}
	return out
}

// AdoptionLoopFromMarkers reports whether the marker history alone shows
// the loop guard's condition: more than one supersede marker, the latest
// within stagelabels.AdoptionOrphanLoopWindow of now.
func AdoptionLoopFromMarkers(scan AdoptionMarkerScan, now time.Time) bool {
	return scan.Live && scan.Count > 1 && now.Sub(scan.Marker.At) < stagelabels.AdoptionOrphanLoopWindow
}

// AdoptionRevertedComment is the note left when an orphaned adoption is
// handed back to the PR's normal flow.
func AdoptionRevertedComment(obs stageObservation, dead int, why string) string {
	var cause string
	switch why {
	case adoptionRevertHeadMoved:
		cause = fmt.Sprintf("this PR has moved to a newer commit (`%s`) since, so the hive is not copying the old one again", shortSHA(obs.HeadSHA))
	default:
		cause = "the hive could not open a fresh copy just now"
	}
	return fmt.Sprintf("%s superseded_by=%d head=%s why=%s -->\n@%s the hive's copy of this PR, #%d, was closed without merging, and %s. "+
		"The adoption is undone: this PR is back in its normal flow (`%s`), and the hive nudges or adopts again from here if CI stays red.",
		AdoptionRevertedMarker, dead, obs.HeadSHA, why, obs.Author, dead, cause, stagelabels.ReturnedToContributor.Label())
}

// AdoptionChildClosedTwiceComment is the loop-guard note.
func AdoptionChildClosedTwiceComment(obs stageObservation, first, second int) string {
	return fmt.Sprintf("%s first=%d second=%d head=%s -->\nThe hive's copies of this PR were closed without merging twice within an hour (#%d, then #%d). "+
		"Something keeps closing them, so the hive is not opening a third. This PR needs a person (`%s`): reopen one of the copies, close this PR, or say what should happen.",
		AdoptionChildClosedTwiceMarker, first, second, obs.HeadSHA, first, second, stagelabels.NeedsDecision.Label())
}

// recoverOrphanedAdoption runs where the superseder poll found the copy
// closed unmerged. rec carries the adoption (AdoptedBy is the dead copy).
// It returns the comments posted.
func (c *Engine) recoverOrphanedAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, rec stagelabels.Record, policy StageLabelPolicy, now time.Time) (int, error) {
	dead := rec.AdoptedBy
	if dead <= 0 {
		return 0, nil
	}
	logArgs := func(extra ...any) []any {
		return append([]any{"repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "adopted_head", rec.AdoptedHead, "superseded_by", dead}, extra...)
	}
	if rec.AdoptionOrphanedWithin(now, stagelabels.AdoptionOrphanLoopWindow) && rec.AdoptOrphanedBy != dead {
		// The fresh copy from the last recovery is closed too: stop here.
		first := rec.AdoptOrphanedBy
		store.NoteAdoptionOrphaned(key, dead, now)
		store.MarkAdoptionEscalated(key, rec.AdoptedHead, stagelabels.LaneReasonAdoptionChildClosedTwice)
		c.warn("stage labels: adoption copy closed twice within the hour; parking for a person", logArgs("first", first, "reason", stagelabels.LaneReasonAdoptionChildClosedTwice)...)
		if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(AdoptionChildClosedTwiceComment(obs, first, dead))}); err != nil {
			c.warn("stage labels: adoption loop-guard note failed", logArgs("error", err)...)
			return 0, err
		}
		return 1, nil
	}
	if obs.HeadSHA == "" || obs.HeadSHA != rec.AdoptedHead || obs.Merged {
		c.warn("stage labels: adoption orphaned but the head moved; reverting adoption", logArgs("why", adoptionRevertHeadMoved)...)
		return c.revertOrphanedAdoption(ctx, store, key, obs, dead, adoptionRevertHeadMoved)
	}
	store.NoteAdoptionOrphaned(key, dead, now)
	c.info("stage labels: adoption orphaned, re-adopting", logArgs("old", dead, "attempt", rec.AdoptOrphanCount+1)...)
	// supersedeAdoptedPR records the fresh copy on the store; the dead
	// number stays only when it could not open one.
	reason := nonEmpty(rec.AdoptReason, AdoptReasonOffline)
	n, err := c.supersedeAdoptedPR(ctx, store, key, obs, rec, policy, reason, adoptionGrace(rec, policy), rec.NudgeOnline, false, now)
	after, _ := store.Get(key)
	if after.AdoptedBy > 0 && after.AdoptedBy != dead {
		c.info("stage labels: adoption re-adopted", logArgs("old", dead, "new", after.AdoptedBy)...)
		return n, err
	}
	// The failed attempt is logged, not returned: the revert is its
	// resolution, and the sweep must not report an error for a PR that is
	// now back in its normal flow.
	c.warn("stage labels: re-adoption did not open a fresh copy; reverting adoption", logArgs("why", adoptionRevertReadoptFail, "error", err)...)
	m, rerr := c.revertOrphanedAdoption(ctx, store, key, obs, dead, adoptionRevertReadoptFail)
	return n + m, rerr
}

// revertOrphanedAdoption forgets the adoption and tells the PR why.
func (c *Engine) revertOrphanedAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, dead int, why string) (int, error) {
	store.ClearAdoption(key)
	if obs.Merged {
		return 0, nil
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(AdoptionRevertedComment(obs, dead, why))}); err != nil {
		c.warn("stage labels: adoption revert note failed", "repo", obs.Display, "pr", obs.Number, "superseded_by", dead, "error", err)
		return 0, err
	}
	return 1, nil
}

// adoptionMarkerScanDue reports a PR whose record carries no adoption but
// may still wear a supersede marker on GitHub: a clanker's open PR the lane
// parks at 085, not yet read at this head.
func adoptionMarkerScanDue(obs stageObservation, rec stagelabels.Record, hasRec bool, stage stagelabels.Stage, policy StageLabelPolicy) bool {
	if !policy.Adoption || !hasRec || obs.Merged || obs.HeadSHA == "" || obs.Lane == stageLaneSelf {
		return false
	}
	if stage != stagelabels.NeedsDecision || obs.AuthorKind != authorkind.Clanker {
		return false
	}
	if rec.AdoptedBy > 0 || rec.AdoptionLive(obs.HeadSHA) || rec.Superseded() {
		return false
	}
	return rec.AdoptMarkerScanHead != obs.HeadSHA
}

// recoverOrphanedAdoptionFromMarker reads the PR's comments once per head
// for a supersede marker the store forgot and settles it from the copy's
// state: merged → the close path, closed → the orphan recovery, open → the
// adoption is put back on record.
func (c *Engine) recoverOrphanedAdoptionFromMarker(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, now time.Time) (int, error) {
	store.MarkAdoptMarkerScanned(key, obs.HeadSHA)
	comments, err := c.listIssueComments(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		c.warn("stage labels: adoption marker scan failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 0, err
	}
	scan := ScanAdoptionMarkers(comments)
	if !scan.Live {
		return 0, nil
	}
	dead := scan.Marker.SupersededBy
	logArgs := []any{"repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "marker_head", scan.Marker.Head, "superseded_by", dead, "markers", scan.Count}
	sup, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, dead)
	if err != nil {
		c.warn("stage labels: adoption marker names a copy the hive cannot read", append(logArgs, "error", err)...)
		return 0, err
	}
	// The marker is the adoption the store lost: restore it so every
	// existing path (close on merge, orphan recovery, 074 derivation)
	// runs as if it had never been forgotten.
	adoptedHead := nonEmpty(scan.Marker.Head, obs.HeadSHA)
	store.MarkAdopted(key, adoptedHead, dead, now)
	rec, _ := store.Get(key)
	switch {
	case sup.GetMerged():
		c.info("stage labels: adoption marker found with a merged copy; closing the original", logArgs...)
		return c.closeSupersededOriginal(ctx, store, key, obs, rec, sup, now)
	case strings.EqualFold(sup.GetState(), "closed"):
		if AdoptionLoopFromMarkers(scan, now) {
			// The marker path has no first-orphan record; the comment
			// history is it: a second copy closed within the hour.
			store.NoteAdoptionOrphaned(key, dead, now)
			store.MarkAdoptionEscalated(key, adoptedHead, stagelabels.LaneReasonAdoptionChildClosedTwice)
			c.warn("stage labels: adoption copies closed twice within the hour (from markers); parking for a person", append(logArgs, "reason", stagelabels.LaneReasonAdoptionChildClosedTwice)...)
			if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(AdoptionChildClosedTwiceComment(obs, 0, dead))}); err != nil {
				return 0, err
			}
			return 1, nil
		}
		c.warn("stage labels: adoption marker found with a closed copy; the store had forgotten it", logArgs...)
		return c.recoverOrphanedAdoption(ctx, store, key, obs, rec, policy, now)
	}
	c.info("stage labels: adoption marker found with an open copy; adoption restored on record", logArgs...)
	return 0, nil
}
