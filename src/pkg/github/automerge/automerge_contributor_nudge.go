package automerge

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Contributor nudge for stage/072-returned-to-contributor
// (hivecommons/hive#12436).
//
// When a contributor's PR lands in stage 072 the hive posts ONE comment
// @-mentioning the author with the concrete failing checks / review asks and
// a "reply here or push a new head" line, and — through the
// ContributorNudgeSink the dashboard hub implements — pushes the same
// diagnosis to the author's relay session as a fix-pr task.
//
// The comment is one per PR (hivecommons/hive#17067): the marker names the
// reason class it explains, and a new head red for the same class, or a
// changed class, rewrites the hive's existing nudge comment in place rather
// than posting another copy (the relay dispatch and the adoption-grace
// ledger still see every head). While the same class persists the comment
// is rewritten once more after policy.RenudgeInterval
// (auto_merge.nudge_repeat_after, default 72h) as the last reminder, then
// the hive stops: 2 nudges per PR and reason class.

// ContributorNudgeMarkerPrefix opens the nudge comment's machine marker.
const ContributorNudgeMarkerPrefix = "<!-- hive:contributor-nudge"

// contributorNudgeCap is the documented maximum number of nudges per PR
// and reason class.
const contributorNudgeCap = 2

// ContributorNudgeMarker renders the marker that keys the one nudge comment
// on a PR: `<!-- hive:contributor-nudge reason=<slug>` plus head, ordinal
// and delivery mode. The upsert searches for ContributorNudgeMarkerPrefix
// alone, so a changed reason slug still finds the comment.
func ContributorNudgeMarker(reason, head string, nudge int, mode string) string {
	return fmt.Sprintf("%s reason=%s head=%s n=%d mode=%s -->", ContributorNudgeMarkerPrefix, stagelabels.NudgeReasonSlug(reason), head, nudge, nonEmpty(mode, "comment"))
}

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
	// Nudge is 1 for the first nudge for this reason class, 2 for the
	// repeat re-nudge, and 0 for a head-only rewrite of the existing
	// comment (the class is unchanged and already counted).
	Nudge int
	// Stalled is true when this nudge is the repeat re-nudge.
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
	// RelayFailed is true when the relay reported the fix-pr task failed
	// right after dispatch: the nudge was not delivered as a task and the
	// fix waits in the queue for a retry; RelayError is the relay's reason.
	RelayFailed bool
	RelayError  string
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
	case strings.HasPrefix(r, stageReasonOptionalCheckFailing):
		// Every red optional check on the head, not just the first one
		// the stage reason names (hivecommons/hive#12538).
		if len(d.RedChecks) > 0 {
			checks = append(checks, d.RedChecks...)
		} else {
			checks = append(checks, strings.TrimPrefix(r, stageReasonOptionalCheckFailing))
		}
	case classifyCIReason(r) == ciReasonRed:
		checks = append(checks, r)
	case r == "changes-requested":
		asks = append(asks, "a reviewer requested changes")
		human = true
	case r == autoMergeReasonHumanChangesRequested:
		asks = append(asks, "a maintainer requested changes; the hive is pushing the fix")
		human = true
	case r == stageReasonDecisionAnswered:
		asks = append(asks, "a maintainer answered the decision question with an instruction; the hive is pushing the fix")
		human = true
	case r == contributorLaneReasonChangesRequested, r == stagelabels.LaneReasonChangesRequested:
		asks = append(asks, "the review swarm requested changes")
	case r == stagelabels.LaneReasonReviewerQuestion:
		// A person's PR routed to them on requires_human
		// (hivecommons/hive#15202); the route carries the question itself.
		asks = append(asks, "the review swarm returned `requires_human`: it has a question for you, not a fix for the hive")
	case r == "conflicting" || r == "not-mergeable" || r == "label:needs-rebase":
		asks = append(asks, "the branch conflicts with the base and needs a rebase")
	case r == autoMergeReasonForkNotEditable:
		asks = append(asks, "the branch is behind the base and Hive cannot update it; enable \"allow edits by maintainers\" or rebase")
	case r == stagelabels.LaneReasonDCOMissing:
		asks = append(asks, dcoAsk(d))
	case r == stagelabels.LaneReasonHiveSignoffAttribution:
		asks = append(asks, signoffAttributionAsk(d))
	case r != "":
		asks = append(asks, r)
	}
	if verdictBlocks(d) && r != contributorLaneReasonChangesRequested && r != stagelabels.LaneReasonChangesRequested {
		asks = append(asks, "the review swarm requested changes")
	}
	if d.DCO == stagelabels.DCONo && r != stagelabels.LaneReasonDCOMissing {
		asks = append(asks, dcoAsk(d))
	}
	return checks, asks, human
}

// nudgeDiagnosisRouted extends nudgeDiagnosis with the evidence the human
// routing gathered (hivecommons/hive#12528): every red check, a missing DCO
// sign-off, the conflict — so the 072 entry comment lists the concrete
// reasons even when the lane itself stopped at admission.
func nudgeDiagnosisRouted(reason string, d stageDetail, route *stagelabels.Route) (checks, asks []string, human bool) {
	checks, asks, human = nudgeDiagnosis(reason, d)
	if route == nil {
		return checks, asks, human
	}
	seenCheck := map[string]bool{}
	for _, c := range checks {
		seenCheck[c] = true
	}
	seenAsk := map[string]bool{}
	for _, a := range asks {
		seenAsk[a] = true
	}
	for _, r := range route.Reasons {
		switch {
		case strings.HasPrefix(r, "red checks: "):
			for _, name := range strings.Split(strings.TrimPrefix(r, "red checks: "), ", ") {
				if name = strings.TrimSpace(name); name != "" && !seenCheck[name] {
					seenCheck[name] = true
					checks = append(checks, name)
				}
			}
		case strings.HasPrefix(r, "verdict changes_requested"):
			if a := "the review swarm requested changes"; !seenAsk[a] {
				seenAsk[a] = true
				asks = append(asks, a)
			}
		case r == "DCO sign-off missing":
			if a := dcoAsk(d); !seenAsk[a] {
				seenAsk[a] = true
				asks = append(asks, a)
			}
		case strings.HasPrefix(r, "merge conflict"):
			if a := "the branch conflicts with the base and needs a rebase"; !seenAsk[a] {
				seenAsk[a] = true
				asks = append(asks, a)
			}
		case strings.HasPrefix(r, "reviewer question: "), strings.HasPrefix(r, "reviewer summary: "):
			// A person's PR at 071 on a requires_human verdict: the
			// reviewer's question is theirs to answer (hivecommons/hive#15202).
			if a := "the reviewer asked you: " + strings.TrimSpace(r[strings.Index(r, ": ")+2:]); !seenAsk[a] {
				seenAsk[a] = true
				asks = append(asks, a)
			}
		}
	}
	return checks, asks, human
}

// ContributorNudgeComment renders the nudge comment. The marker carries the
// reason class, the head, the nudge ordinal and the delivery mode so a later
// pass can prove the comment exists and what it explains without parsing
// the prose.
func ContributorNudgeComment(n ContributorNudge, out ContributorNudgeOutcome) string {
	var b strings.Builder
	b.WriteString(ContributorNudgeMarker(n.Reason, n.HeadSHA, n.Nudge, out.Mode))
	b.WriteString("\n")
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
		if out.RelayFailed {
			b.WriteString("\n_Your relay could not take this as a `fix-pr` task")
			if e := strings.TrimSpace(out.RelayError); e != "" {
				fmt.Fprintf(&b, " (%s)", e)
			}
			b.WriteString("; the hive will retry it from your relay fix queue._\n")
			break
		}
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

// nudgeDue decides whether the nudge comment is owed a write for the record
// at this head and reason: ordinal 1 when the reason class is new to the PR
// (first nudge, or a changed reason rewriting the comment), 0 when a new
// head is red for the class already explained (rewrite only, uncounted),
// 2 once repeatAfter elapsed since the class was last counted — never a
// third. Same head, same class, inside the window: nothing.
func nudgeDue(rec stagelabels.Record, head, reason string, repeatAfter time.Duration, now time.Time) (ordinal int, ok bool) {
	if head == "" {
		return 0, false
	}
	slug := stagelabels.NudgeReasonSlug(reason)
	if rec.NudgeReason != slug {
		return 1, true
	}
	if rec.NudgeHead != head {
		return 0, true
	}
	if rec.NudgeReasonCount >= contributorNudgeCap {
		return 0, false
	}
	if repeatAfter <= 0 || rec.NudgeReasonAt.IsZero() || now.Sub(rec.NudgeReasonAt) < repeatAfter {
		return 0, false
	}
	return rec.NudgeReasonCount + 1, true
}

// nudgeContributor runs the stage-072 nudge for one observation. It returns
// the number of GitHub comments posted.
func (c *Engine) nudgeContributor(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, now time.Time) (int, error) {
	rec, ok := store.Get(key)
	if !ok || obs.Author == "" {
		return 0, nil
	}
	ordinal, due := nudgeDue(rec, obs.HeadSHA, obs.Reason, policy.RenudgeInterval, now)
	if !due {
		return 0, nil
	}
	checks, asks, human := nudgeDiagnosisRouted(obs.Reason, obs.Detail, obs.Route)
	// A fork guard is a maintainer's to clear (hivecommons/hive#14273): the
	// author is never asked to fix it, and a head red on nothing else is not
	// nudged at all — no nudge, no adoption grace
	// (projectbluefin/utah-packages#869).
	if kept := withoutForkGuard(checks, obs.ForkGuardChecks); len(kept) < len(checks) {
		checks = kept
		if len(checks) == 0 && len(asks) == 0 {
			c.debug("stage labels: contributor nudge skipped, only a fork guard is red", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "checks", obs.ForkGuardChecks)
			return 0, nil
		}
	}
	// A head-only rewrite (ordinal 0) keeps the class's recorded count in
	// the marker; the author pushed, so it is not the stall reminder.
	shown := ordinal
	if shown == 0 {
		shown = max(rec.NudgeReasonCount, 1)
	}
	n := ContributorNudge{
		Owner: obs.Owner, Repo: obs.Repo, Display: obs.Display, Number: obs.Number,
		HeadSHA: obs.HeadSHA, Title: obs.Title, URL: obs.URL, Author: obs.Author, OpenedAt: obs.OpenedAt,
		Reason: obs.Reason, FailingChecks: checks, ReviewAsks: asks, HumanChangesRequested: human,
		Nudge: shown, Stalled: ordinal > 1,
	}
	var out ContributorNudgeOutcome
	if c.contributorNudge != nil {
		out = c.contributorNudge.Nudge(ctx, n)
	}
	body := ContributorNudgeComment(n, out)
	// One nudge comment per PR (hivecommons/hive#17067): the hive's
	// existing comment carrying the marker is rewritten for a new head or
	// a changed reason; an edit notifies nobody, the relay dispatch above
	// is the interrupt.
	posted := 0
	if existing := c.findOwnEntryComment(ctx, obs, ContributorNudgeMarkerPrefix); existing != nil {
		if existing.GetBody() != body {
			if _, _, err := c.gh.Issues.EditComment(ctx, obs.Owner, obs.Repo, existing.GetID(), &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
				c.warn("stage labels: contributor nudge edit failed", "repo", obs.Display, "pr", obs.Number, "error", err)
				return 0, err
			}
		}
	} else {
		if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			c.warn("stage labels: contributor nudge failed", "repo", obs.Display, "pr", obs.Number, "error", err)
			return 0, err
		}
		posted = 1
	}
	// "queued" only says the author has a relay registered here; the work
	// sits in its queue whether or not that relay is connected. Counting it
	// as online gave an offline clanker the 2h online grace (#15566).
	store.MarkNudged(key, obs.HeadSHA, max(ordinal, 1), out.Online || out.Mode == "spawn" || out.Mode == "preempt", now)
	store.MarkNudgedReason(key, obs.Reason, ordinal, now)
	c.info("stage labels: contributor nudged", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "nudge", ordinal, "posted", posted == 1, "mode", nonEmpty(out.Mode, "comment"), "online", out.Online, "queue_depth", out.QueueDepth, "relay_failed", out.RelayFailed, "reason", obs.Reason)
	return posted, nil
}

// withoutForkGuard drops the fork-guard checks from a nudge's failing
// checks; checks is returned unchanged when there is no guard.
func withoutForkGuard(checks, guard []string) []string {
	if len(guard) == 0 {
		return checks
	}
	var out []string
	for _, name := range checks {
		if !slices.Contains(guard, strings.TrimSpace(name)) {
			out = append(out, name)
		}
	}
	return out
}

// resolveNudge tells the sink a PR that was in stage 072 last cycle has
// left it (new head went green, closed, merged, adopted).
func (c *Engine) resolveNudge(ctx context.Context, rec stagelabels.Record, had bool, obs stageObservation) {
	if c.contributorNudge == nil || !had || rec.Stage != stagelabels.ReturnedToContributor {
		return
	}
	c.contributorNudge.Resolve(ctx, obs.Owner, obs.Repo, obs.Number)
}

// TrustedHuman071MarkerPrefix opens the one comment a trusted human author
// gets when their PR enters stage 071 (hivecommons/hive#13636); the head keeps
// it idempotent per push.
const TrustedHuman071MarkerPrefix = "<!-- hive:human-071-note"

// TrustedHuman071Comment renders the entry note: why the PR is held at 071
// (conflict / sign-off / failing checks / requested changes) and what the
// hive does next.
func TrustedHuman071Comment(n ContributorNudge, stage stagelabels.Stage) string {
	return TrustedHuman071Note(n, stage, true)
}

// TrustedHuman071Note is TrustedHuman071Comment with the @-mention optional:
// in quiet notification mode the author is mentioned once per cause, and a
// re-render for a new head with the same cause names them without the @.
//
// The author's mention is the only deliberate one. The diagnosis quotes
// check names and review text the hive did not write, so that part is
// neutralised (advisory.NeutralizeMentions) and never notifies the people
// it happens to name.
func TrustedHuman071Note(n ContributorNudge, stage stagelabels.Stage, mention bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s -->\n", TrustedHuman071MarkerPrefix, n.HeadSHA)
	at := "@"
	if !mention {
		at = ""
	}
	fmt.Fprintf(&b, "%s%s this PR is held at `%s` and the hive will not merge it yet.\n\n", at, n.Author, stage.Label())
	var d strings.Builder
	if len(n.FailingChecks) > 0 {
		d.WriteString("**Failing checks:**\n")
		for _, c := range n.FailingChecks {
			fmt.Fprintf(&d, "- `%s`\n", c)
		}
	}
	if len(n.ReviewAsks) > 0 {
		d.WriteString("**Why:**\n")
		for _, a := range n.ReviewAsks {
			fmt.Fprintf(&d, "- %s\n", a)
		}
	}
	b.WriteString(advisory.NeutralizeMentions(d.String()))
	b.WriteString("\nNext: the hive does not push to your branch. Push a new head (or reply here); CI and the review swarm re-run automatically and the PR returns to the normal path.\n")
	return b.String()
}

// noteTrustedHuman071 posts TrustedHuman071Comment once per head for a
// trusted human PR in 071. It returns the number of comments posted.
func (c *Engine) noteTrustedHuman071(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, now time.Time) (int, error) {
	rec, ok := store.Get(key)
	if !ok || obs.Author == "" || obs.HeadSHA == "" || rec.NudgeHead == obs.HeadSHA {
		return 0, nil
	}
	checks, asks, human := nudgeDiagnosisRouted(obs.Reason, obs.Detail, obs.Route)
	if len(checks) == 0 && len(asks) == 0 && obs.Reason != "" {
		asks = append(asks, obs.Reason)
	}
	n := ContributorNudge{
		Owner: obs.Owner, Repo: obs.Repo, Display: obs.Display, Number: obs.Number,
		HeadSHA: obs.HeadSHA, Title: obs.Title, URL: obs.URL, Author: obs.Author, OpenedAt: obs.OpenedAt,
		Reason: obs.Reason, FailingChecks: checks, ReviewAsks: asks, HumanChangesRequested: human, Nudge: 1,
	}
	// Normal mode: every head is a fresh comment with a mention. Quiet
	// mode: one section of the status comment, rewritten per head, and the
	// author is @-mentioned only when the cause changed since the last
	// mention — a new head failing the same way is the same ask.
	mention := !policy.QuietNotifications || rec.MentionDue(obs.Reason)
	body := TrustedHuman071Note(n, stagelabels.NeedsAuthor, mention)
	posted, err := c.postStageNoteMentioning(ctx, policy, obs, prstatus.KeyHuman071Note, body)
	if err != nil {
		c.warn("stage labels: trusted human 071 note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 0, err
	}
	store.MarkNudged(key, obs.HeadSHA, 1, false, now)
	if mention {
		store.MarkMentioned(key, obs.Reason)
	}
	c.info("stage labels: trusted human told why the PR is at 071", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "reason", obs.Reason, "mention", mention)
	return posted, nil
}
