package automerge

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Contributor nudge for stage/072-returned-to-contributor
// (hivecommons/hive#12436).
//
// When a contributor's PR lands in stage 072 the hive posts ONE idempotent
// comment @-mentioning the author with the concrete failing checks / review
// asks and a "reply here or push a new head" line, and — through the
// ContributorNudgeSink the dashboard hub implements — pushes the same
// diagnosis to the author's relay session as a fix-pr task. It re-nudges once
// more at the stall threshold and then stops: 2 nudges per head, re-armed
// only when the author pushes a new head.

// ContributorNudgeMarkerPrefix opens the nudge comment's machine marker.
const ContributorNudgeMarkerPrefix = "<!-- hive:contributor-nudge"

// contributorNudgeCap is the documented maximum number of nudges per head.
const contributorNudgeCap = 2

// ContributorNudge is the diagnosis handed to the sink for one PR in stage 072.
type ContributorNudge struct {
	Owner, Repo string
	// Display is the owner/repo spelling used in logs and comments.
	Display string
	Number  int
	HeadSHA string
	Title   string
	URL     string
	// Author is the PR author's GitHub login (the contributor to interrupt).
	Author   string
	OpenedAt time.Time
	// Reason is the lane's skip reason ("check-failing:unit", "conflicting",
	// "changes-requested", ...).
	Reason string
	// FailingChecks lists the check names derived from Reason.
	FailingChecks []string
	// ReviewAsks lists the review-side asks (swarm/human changes requested,
	// conflicts, needs-rebase).
	ReviewAsks []string
	// HumanChangesRequested is true when a human reviewer asked for changes
	// (outranks CI-red in the contributor's fix queue).
	HumanChangesRequested bool
	// Nudge is 1 for the first nudge at this head, 2 for the stall re-nudge.
	Nudge int
	// Stalled is true when this nudge is the stall re-nudge.
	Stalled bool
}

// ContributorNudgeOutcome is what the sink did with a nudge.
type ContributorNudgeOutcome struct {
	// Mode is how the contributor was reached: "spawn" (parallel fix
	// sub-session), "preempt" (primary task parked), "queued" (relay
	// contributor at capacity; waits in their fix queue), or "comment"
	// (no relay session — external author or offline).
	Mode string
	// Online is true when the author's relay heartbeat was fresh.
	Online bool
	// QueueDepth is the author's fix-queue depth after this nudge.
	QueueDepth int
}

// ContributorNudgeSink receives stage-072 nudges. The dashboard hub implements
// it; nil means "GitHub comment only".
type ContributorNudgeSink interface {
	// Nudge records the red PR against its author and, when the author is a
	// relay contributor who is online, dispatches the fix. It must not block
	// on network I/O to the contributor.
	Nudge(ctx context.Context, n ContributorNudge) ContributorNudgeOutcome
	// Resolve tells the sink the PR left stage 072 (new head went green,
	// closed, merged, adopted) so the author's queue drops it.
	Resolve(ctx context.Context, owner, repo string, number int)
}

// nudgeDiagnosis splits a lane reason into failing checks and review asks.
func nudgeDiagnosis(reason string, d stageDetail) (checks, asks []string, human bool) {
	r := strings.TrimSpace(reason)
	switch {
	case strings.HasPrefix(r, "required-check-failing:"):
		checks = append(checks, strings.TrimPrefix(r, "required-check-failing:"))
	case strings.HasPrefix(r, "check-failing:"):
		checks = append(checks, strings.TrimPrefix(r, "check-failing:"))
	case classifyCIReason(r) == ciReasonRed:
		checks = append(checks, r)
	case r == "changes-requested":
		asks = append(asks, "a reviewer requested changes")
		human = true
	case r == contributorLaneReasonChangesRequested:
		asks = append(asks, "the review swarm requested changes")
	case r == "conflicting" || r == "not-mergeable" || r == "label:needs-rebase":
		asks = append(asks, "the branch conflicts with the base and needs a rebase")
	case r != "":
		asks = append(asks, r)
	}
	if verdictBlocks(d) && r != contributorLaneReasonChangesRequested {
		asks = append(asks, "the review swarm requested changes")
	}
	return checks, asks, human
}

// ContributorNudgeComment renders the nudge comment. The marker carries the
// head, the nudge ordinal and the delivery mode so a later pass can prove the
// comment exists without a per-PR GET.
func ContributorNudgeComment(n ContributorNudge, out ContributorNudgeOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s n=%d mode=%s -->\n", ContributorNudgeMarkerPrefix, n.HeadSHA, n.Nudge, nonEmpty(out.Mode, "comment"))
	if n.Stalled {
		fmt.Fprintf(&b, "@%s this PR has been waiting on you for a while (`%s`). ", n.Author, stagelabels.ReturnedToContributor.Label())
	} else {
		fmt.Fprintf(&b, "@%s this PR is back in your court (`%s`). ", n.Author, stagelabels.ReturnedToContributor.Label())
	}
	b.WriteString("The hive will not merge it until the head changes.\n\n")
	if len(n.FailingChecks) > 0 {
		b.WriteString("**Failing checks:**\n")
		for _, c := range n.FailingChecks {
			fmt.Fprintf(&b, "- `%s`\n", c)
		}
	}
	if len(n.ReviewAsks) > 0 {
		b.WriteString("**Review asks:**\n")
		for _, a := range n.ReviewAsks {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	b.WriteString("\nReply here or push a new head to this branch (same sign-off); CI and the review swarm re-run automatically and the PR returns to the normal path.\n")
	switch out.Mode {
	case "spawn":
		b.WriteString("\n_Your relay session received this as a `fix-pr` task and launched a parallel fix session; your current work continues._\n")
	case "preempt":
		b.WriteString("\n_Your relay session received this as a `fix-pr` task; your current task is parked (claim kept alive) until this PR is green._\n")
	case "queued":
		fmt.Fprintf(&b, "\n_Queued in your relay fix queue (%d waiting): fixes are handed out before new work as soon as you have capacity._\n", out.QueueDepth)
	}
	if n.Stalled {
		b.WriteString("\n_This is the last automatic reminder for this head. If you are unavailable the hive may adopt the PR to finish it (your commits are preserved)._\n")
	}
	return b.String()
}

func nonEmpty(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// nudgeDue decides whether a nudge is owed for the record at this head: the
// first on entering 072 with a new head, the second once renudge elapsed
// (the stall threshold), never a third.
func nudgeDue(rec stagelabels.Record, head string, renudge time.Duration, now time.Time) (ordinal int, ok bool) {
	if head == "" {
		return 0, false
	}
	if rec.NudgeHead != head {
		return 1, true
	}
	if rec.NudgeCount >= contributorNudgeCap {
		return 0, false
	}
	if renudge <= 0 || rec.LastNudgeAt.IsZero() || now.Sub(rec.LastNudgeAt) < renudge {
		return 0, false
	}
	return rec.NudgeCount + 1, true
}

// nudgeContributor runs the stage-072 nudge for one observation. It returns
// the number of GitHub comments posted.
func (c *Engine) nudgeContributor(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, now time.Time) (int, error) {
	rec, ok := store.Get(key)
	if !ok || obs.Author == "" {
		return 0, nil
	}
	renudge := policy.RenudgeInterval
	if renudge <= 0 {
		renudge = stagelabels.Dwell(stagelabels.ReturnedToContributor, policy.Dwell)
	}
	ordinal, due := nudgeDue(rec, obs.HeadSHA, renudge, now)
	if !due {
		return 0, nil
	}
	checks, asks, human := nudgeDiagnosis(obs.Reason, obs.Detail)
	n := ContributorNudge{
		Owner: obs.Owner, Repo: obs.Repo, Display: obs.Display, Number: obs.Number,
		HeadSHA: obs.HeadSHA, Title: obs.Title, URL: obs.URL, Author: obs.Author, OpenedAt: obs.OpenedAt,
		Reason: obs.Reason, FailingChecks: checks, ReviewAsks: asks, HumanChangesRequested: human,
		Nudge: ordinal, Stalled: ordinal > 1,
	}
	var out ContributorNudgeOutcome
	if c.contributorNudge != nil {
		out = c.contributorNudge.Nudge(ctx, n)
	}
	body := ContributorNudgeComment(n, out)
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: contributor nudge failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 0, err
	}
	store.MarkNudged(key, obs.HeadSHA, ordinal, now)
	c.info("stage labels: contributor nudged", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "nudge", ordinal, "mode", nonEmpty(out.Mode, "comment"), "online", out.Online, "queue_depth", out.QueueDepth, "reason", obs.Reason)
	return 1, nil
}

// resolveNudge tells the sink a PR that was in stage 072 last cycle has
// left it (new head went green, closed, merged, adopted).
func (c *Engine) resolveNudge(ctx context.Context, rec stagelabels.Record, had bool, obs stageObservation) {
	if c.contributorNudge == nil || !had || rec.Stage != stagelabels.ReturnedToContributor {
		return
	}
	c.contributorNudge.Resolve(ctx, obs.Owner, obs.Repo, obs.Number)
}
