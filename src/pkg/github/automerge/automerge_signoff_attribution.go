package automerge

// The sign-off attribution note, one per PR (hivecommons/hive#17066).
//
// A human whose head is held for hive-signoff-attribution gets told once
// with the exact commands. The note used to carry the head in its marker
// so every push re-posted it; now the PR carries one note that a new head
// rewrites in place, with a short capped history of what changed.

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// SignoffAttributionMarkerPrefix opens the one comment a human author gets
// when their head is held for hive-signoff-attribution. Comments from
// before hivecommons/hive#17066 carry ` head=<sha>` after it; the upsert
// adopts the latest of those and rewrites it under SignoffAttributionMarker.
const SignoffAttributionMarkerPrefix = "<!-- hive:signoff-attribution"

// SignoffAttributionMarker is the full first line of the comment.
const SignoffAttributionMarker = SignoffAttributionMarkerPrefix + " -->"

// isSignoffAttributionComment reports whether body is a sign-off
// attribution note, in either the current or the per-head legacy form.
func isSignoffAttributionComment(body string) bool {
	return strings.HasPrefix(body, SignoffAttributionMarkerPrefix)
}

// SignoffAttributionComment renders the author-facing comment for a human
// PR parked at 071 on LaneReasonHiveSignoffAttribution: who owes the fix,
// why no agent can do it, and the exact commands. history is the capped
// list of earlier rewrites (nil on first post).
func SignoffAttributionComment(n ContributorNudge, stage stagelabels.Stage, history []string) string {
	return renderMarkerHistory(signoffAttributionCore(n, stage), history)
}

// signoffAttributionCore is the comment without its history block: the
// part whose change triggers a rewrite.
func signoffAttributionCore(n ContributorNudge, stage stagelabels.Stage) string {
	var b strings.Builder
	b.WriteString(SignoffAttributionMarker)
	b.WriteString("\n")
	fmt.Fprintf(&b, "@%s this PR is back in your court (`%s`). ", n.Author, stage.Label())
	b.WriteString("Its commits are signed off as the hive App rather than as you, so a squash merge would misattribute them and the sign-off attribution check stays red. No hive agent can rewrite a contributor's commits — the fix is yours:\n\n")
	for _, a := range n.ReviewAsks {
		fmt.Fprintf(&b, "- %s\n", a)
	}
	fmt.Fprintf(&b, "\nOnce the new head lands, CI and the review swarm re-run automatically and the PR returns to the normal path. Current head: `%s`.\n", shortSHA(n.HeadSHA))
	return b.String()
}

// signoffAttributionChange is the history line for a rewrite on a new head.
func signoffAttributionChange(headSHA string, hiveSigned int) string {
	if hiveSigned <= 0 {
		hiveSigned = 1
	}
	plural := "s"
	if hiveSigned == 1 {
		plural = ""
	}
	return fmt.Sprintf("head `%s`, %d commit%s signed off as the hive App", shortSHA(headSHA), hiveSigned, plural)
}

// nudgeSignoffAttribution tells a non-clanker author once per PR that the
// head needs re-signing (a clanker gets the regular 072 nudge, which
// carries the same ask). The first head posts the comment; a later head
// rewrites it in place and appends one history line. It returns the number
// of comments posted (an edit counts zero).
func (c *Engine) nudgeSignoffAttribution(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, now time.Time) (int, error) {
	rec, ok := store.Get(key)
	if !ok || obs.Author == "" || obs.HeadSHA == "" || rec.NudgeHead == obs.HeadSHA {
		return 0, nil
	}
	_, asks, _ := nudgeDiagnosis(obs.Reason, obs.Detail)
	n := ContributorNudge{
		Owner: obs.Owner, Repo: obs.Repo, Display: obs.Display, Number: obs.Number,
		HeadSHA: obs.HeadSHA, Title: obs.Title, URL: obs.URL, Author: obs.Author, OpenedAt: obs.OpenedAt,
		Reason: obs.Reason, ReviewAsks: asks, Nudge: 1,
	}
	stage := fixingStage(obs)
	existing, err := c.findOwnMarkerComment(ctx, obs.Owner, obs.Repo, obs.Number, isSignoffAttributionComment)
	if err != nil {
		// A list failure reads as "none": the note is posted rather than lost.
		c.warn("stage labels: could not list comments for the sign-off attribution upsert", "repo", obs.Display, "pr", obs.Number, "error", err)
	}
	if existing != nil {
		body, changed := signoffAttributionRewrite(existing.GetBody(), n, stage, obs.Detail.DCOSquashUnsafeHive, now)
		if changed {
			if err := c.editMarkerComment(ctx, obs.Owner, obs.Repo, existing.GetID(), body); err != nil {
				c.warn("stage labels: sign-off attribution comment edit failed", "repo", obs.Display, "pr", obs.Number, "error", err)
				return 0, err
			}
			c.info("stage labels: sign-off attribution note updated in place", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "squash_unsafe_hive", obs.Detail.DCOSquashUnsafeHive)
		}
		store.MarkNudged(key, obs.HeadSHA, 1, false, now)
		return 0, nil
	}
	body := SignoffAttributionComment(n, stage, nil)
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: sign-off attribution comment failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 0, err
	}
	store.MarkNudged(key, obs.HeadSHA, 1, false, now)
	c.info("stage labels: author told to re-sign the head", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "squash_unsafe_hive", obs.Detail.DCOSquashUnsafeHive)
	return 1, nil
}

// signoffAttributionRewrite is the body that replaces previous for the
// observation n (hiveSigned commits carry the App's sign-off), and whether
// it differs. The history gains one line only when the core text (asks,
// head) changed; an identical ask on the same head leaves the comment alone.
func signoffAttributionRewrite(previous string, n ContributorNudge, stage stagelabels.Stage, hiveSigned int, now time.Time) (string, bool) {
	prevCore, history := splitMarkerHistory(previous)
	core := signoffAttributionCore(n, stage)
	if strings.TrimRight(core, "\n") == prevCore {
		return previous, false
	}
	history = appendMarkerHistory(history, markerHistoryLine(now, signoffAttributionChange(n.HeadSHA, hiveSigned)))
	return renderMarkerHistory(core, history), true
}
