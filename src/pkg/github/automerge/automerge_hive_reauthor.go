package automerge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Automatic re-author of the hive's own exhausted PR.
//
// Owner on #16456 (a hive PR parked at 085 with "Re-author ▸" after six
// fixer attempts): "we should not need a re-author pill on a hive agent's
// PR." A hive-authored PR whose fixer gave up is the hive's to redo, not
// a person's to click: the pass closes it as superseded and leaves the
// issue for the next kick to open a fresh PR from the current base. It
// is bounded to ONE automatic re-author per issue — the durable marker is
// stagelabels.LabelReauthored on the issue plus the stage store record —
// so a second exhaustion parks at 085 with the remaining answers (give
// direction, hand to a person, close as not planned); re-authoring is
// never offered as a human choice on a hive PR. Rewrites a person asks
// for on a CONTRIBUTOR's PR stay `decide:adopt`.

// reauthorMarker stamps the close note on the superseded hive PR.
const reauthorMarker = "<!-- hive:reauthor"

// reauthorTarget is what autoReauthorTarget decided for an exhausted PR.
type reauthorTarget struct {
	// Issue is the issue the fresh PR is for; 0 when the PR closes none.
	Issue int
	// PriorPR is the hive PR already re-authored for Issue (the marker is
	// set), 0 when this would be the first; -1 when only the label says
	// so and the store has no record of which PR.
	PriorPR int
}

// reauthorRecord is what a pass that re-authored a PR writes on the store
// once the key is observed.
type reauthorRecord struct {
	issue, attempts int
	noted           bool
}

// eligible reports whether the PR is re-authored automatically now.
func (t reauthorTarget) eligible() bool { return t.Issue > 0 && t.PriorPR == 0 }

// hiveOwnPR reports whether obs is a PR the hive App itself opened — the
// only PR the hive re-authors on its own.
func (c *Engine) hiveOwnPR(obs stageObservation) bool {
	if obs.AuthorKind == authorkind.Hive {
		return true
	}
	app := c.transport.AppBotLogin()
	return app != "" && normalizeBotLogin(obs.Author) == normalizeBotLogin(app)
}

// autoReauthorTarget reads the exhausted PR's closing issue and whether
// that issue has been re-authored before (store record, else the issue's
// marker label). Issue 0 means nothing to re-author for.
func (c *Engine) autoReauthorTarget(ctx context.Context, store *stagelabels.Store, obs stageObservation) reauthorTarget {
	if !c.hiveOwnPR(obs) {
		return reauthorTarget{}
	}
	refs := hgithub.ClosingIssueRefs(obs.Body, obs.Owner+"/"+obs.Repo)
	if len(refs) == 0 {
		return reauthorTarget{}
	}
	t := reauthorTarget{Issue: refs[0]}
	if prior := store.ReauthoredFor(obs.Owner+"/"+obs.Repo, t.Issue); prior > 0 && prior != obs.Number {
		t.PriorPR = prior
		return t
	}
	issue, _, err := c.gh.Issues.Get(ctx, obs.Owner, obs.Repo, t.Issue)
	if err != nil {
		// Unknown marker state: do not re-author on a guess.
		c.warn("stage labels: re-author marker unreadable; parking the exhausted PR for a person", "repo", obs.Display, "pr", obs.Number, "issue", t.Issue, "error", err)
		t.PriorPR = -1
		return t
	}
	for _, l := range issue.Labels {
		if strings.EqualFold(l.GetName(), stagelabels.LabelReauthored) {
			t.PriorPR = -1
			return t
		}
	}
	return t
}

// reauthorExhaustedHivePR closes the hive's exhausted PR as superseded so
// the issue returns to the queue for a fresh PR. Order: marker label on
// the issue first (durable before anything else, so a crash can never
// re-author twice), note, superseded label, close. Returns the number of
// comments posted and whether the note is out; the caller records both
// on the store once the pass has observed the key (MarkReauthored /
// MarkReauthorNoted), so the next pass neither re-labels nor re-notes.
func (c *Engine) reauthorExhaustedHivePR(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, fixer review.FixDispatchStatus, issue int, now time.Time) (comments int, noted bool, err error) {
	logArgs := []any{"repo", obs.Display, "pr", obs.Number, "issue", issue, "head", obs.HeadSHA, "attempts", fixer.Attempts}
	rec, _ := store.Get(key)
	noted = rec.ReauthorNoted
	if !rec.Reauthored() {
		if _, _, err := c.gh.Issues.AddLabelsToIssue(ctx, obs.Owner, obs.Repo, issue, []string{stagelabels.LabelReauthored}); err != nil { // writeback:gated
			c.warn("stage labels: re-author marker label failed; parking the exhausted PR for a person", append(logArgs, "error", err)...)
			return 0, false, err
		}
		c.info("stage labels: hive PR's fixer exhausted; re-authoring it for the issue from the current base", logArgs...)
	}
	body := reauthorCloseComment(obs, issue, fixer.Attempts)
	item := writebackItemForObservation(obs)
	item.HiveAuthored = true
	if d := c.writebackAllowed(obs.Display, item, writeback.KindSupersessionClose, body); !d.Allowed {
		c.info("stage labels: re-author close withheld by the write-back gate", append(logArgs, "writeback_reason", d.Reason)...)
		return 0, noted, nil
	}
	if !noted {
		if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
			c.warn("stage labels: re-author close note failed", append(logArgs, "error", err)...)
			return 0, false, err
		}
		// The note is out: the close below may be retried, the note never.
		noted, comments = true, 1
		if _, _, err := c.gh.Issues.AddLabelsToIssue(ctx, obs.Owner, obs.Repo, obs.Number, []string{hgithub.SupersededLabel}); err != nil { // writeback:gated
			c.warn("stage labels: re-author superseded label failed", append(logArgs, "label", hgithub.SupersededLabel, "error", err)...)
		}
	}
	if _, _, err := c.gh.PullRequests.Edit(ctx, obs.Owner, obs.Repo, obs.Number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil { // writeback:gated
		if isGitHubStatus(err, http.StatusForbidden) || isGitHubStatus(err, http.StatusNotFound) {
			c.warn("stage labels: cannot close re-authored PR (permission); left open with the note", append(logArgs, "error", err)...)
			return comments, noted, nil
		}
		c.warn("stage labels: closing re-authored PR failed; retried next pass", append(logArgs, "error", err)...)
		return comments, noted, err
	}
	return comments, noted, nil
}

// reauthorCloseComment is the one note the superseded hive PR gets.
func reauthorCloseComment(obs stageObservation, issue, attempts int) string {
	base := nonEmpty(obs.BaseRef, "the base branch")
	return fmt.Sprintf("%s issue=%d head=%s -->\n**Re-authoring:** the hive's fixer gave up on this PR after %d attempts on `%s`. Closing it as superseded; the hive opens a fresh PR for #%d from the current `%s` on its next cycle. Nothing from this branch is merged.\n",
		reauthorMarker, issue, obs.HeadSHA, attempts, shortSHA(obs.HeadSHA), issue, base)
}
