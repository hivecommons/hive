package automerge

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
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
	case closingLinkAsk(r) != "":
		asks = append(asks, closingLinkAsk(r))
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
	b.WriteString(nudgeHoldSentence(n.Reason))
	b.WriteString("\n\n")
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

// closingLinkAsk is the review ask for a closing-link refusal
// (hivecommons/hive#15025), or "" for any other reason.
func closingLinkAsk(reason string) string {
	prefix, n, ok := prstage.ParseClosingLinkReason(reason)
	if !ok {
		return ""
	}
	if prefix == prstage.ClosingContradictionReasonPrefix {
		return fmt.Sprintf("The body says #%d stays open (`Refs … partial`) and also `Closes #%d`, so GitHub would close #%d on merge. Keep one: drop the closing keyword, or the partial note. No new commit is needed.", n, n, n)
	}
	return fmt.Sprintf("`fix #%d` (or another closing keyword) in the PR body or a commit message closes pull request #%d on merge; write `same change as #%d` instead.", n, n, n)
}

// nudgeHoldSentence says what releases the PR. A contradiction lives only in
// the PR body, so editing the body is enough; anything else needs a new head.
func nudgeHoldSentence(reason string) string {
	if prefix, _, ok := prstage.ParseClosingLinkReason(reason); ok && prefix == prstage.ClosingContradictionReasonPrefix {
		return "The hive will not merge it until the PR body is fixed."
	}
	return "The hive will not merge it until the head changes."
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
	checks, asks, human := nudgeDiagnosisRouted(obs.Reason, obs.Detail, obs.Route)
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
	// "queued" only says the author has a relay registered here; the work
	// sits in its queue whether or not that relay is connected. Counting it
	// as online gave an offline clanker the 2h online grace (#15566).
	store.MarkNudged(key, obs.HeadSHA, ordinal, out.Online || out.Mode == "spawn" || out.Mode == "preempt", now)
	c.info("stage labels: contributor nudged", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "nudge", ordinal, "mode", nonEmpty(out.Mode, "comment"), "online", out.Online, "queue_depth", out.QueueDepth, "relay_failed", out.RelayFailed, "reason", obs.Reason)
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

// SignoffAttributionMarkerPrefix opens the one comment a human author gets
// when their head is held for hive-signoff-attribution; the head keeps it
// idempotent per push.
const SignoffAttributionMarkerPrefix = "<!-- hive:signoff-attribution"

// SignoffAttributionComment renders the author-facing comment for a human
// PR parked at 071 on LaneReasonHiveSignoffAttribution: who owes the fix,
// why no agent can do it, and the exact commands.
func SignoffAttributionComment(n ContributorNudge, stage stagelabels.Stage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s -->\n", SignoffAttributionMarkerPrefix, n.HeadSHA)
	fmt.Fprintf(&b, "@%s this PR is back in your court (`%s`). ", n.Author, stage.Label())
	b.WriteString("Its commits are signed off as the hive App rather than as you, so a squash merge would misattribute them and the sign-off attribution check stays red. No hive agent can rewrite a contributor's commits — the fix is yours:\n\n")
	for _, a := range n.ReviewAsks {
		fmt.Fprintf(&b, "- %s\n", a)
	}
	b.WriteString("\nOnce the new head lands, CI and the review swarm re-run automatically and the PR returns to the normal path.\n")
	return b.String()
}

// nudgeSignoffAttribution posts SignoffAttributionComment once per head for
// a non-clanker author (a clanker gets the regular 072 nudge, which carries
// the same ask). It returns the number of comments posted.
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
	body := SignoffAttributionComment(n, fixingStage(obs))
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: sign-off attribution comment failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 0, err
	}
	store.MarkNudged(key, obs.HeadSHA, 1, false, now)
	c.info("stage labels: author told to re-sign the head", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "squash_unsafe_hive", obs.Detail.DCOSquashUnsafeHive)
	return 1, nil
}

// TrustedHuman071MarkerPrefix opens the one comment a trusted human author
// gets when their PR enters stage 071 (hivecommons/hive#13636); the head keeps
// it idempotent per push.
const TrustedHuman071MarkerPrefix = "<!-- hive:human-071-note"

// TrustedHuman071Comment renders the entry note: why the PR is held at 071
// (conflict / sign-off / failing checks / requested changes) and what the
// hive does next.
func TrustedHuman071Comment(n ContributorNudge, stage stagelabels.Stage) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s -->\n", TrustedHuman071MarkerPrefix, n.HeadSHA)
	fmt.Fprintf(&b, "@%s this PR is held at `%s` and the hive will not merge it yet.\n\n", n.Author, stage.Label())
	if len(n.FailingChecks) > 0 {
		b.WriteString("**Failing checks:**\n")
		for _, c := range n.FailingChecks {
			fmt.Fprintf(&b, "- `%s`\n", c)
		}
	}
	if len(n.ReviewAsks) > 0 {
		b.WriteString("**Why:**\n")
		for _, a := range n.ReviewAsks {
			fmt.Fprintf(&b, "- %s\n", a)
		}
	}
	b.WriteString("\nNext: the hive does not push to your branch. Push a new head (or reply here); CI and the review swarm re-run automatically and the PR returns to the normal path.\n")
	return b.String()
}

// noteTrustedHuman071 posts TrustedHuman071Comment once per head for a
// trusted human PR in 071. It returns the number of comments posted.
func (c *Engine) noteTrustedHuman071(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, now time.Time) (int, error) {
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
	body := TrustedHuman071Comment(n, stagelabels.NeedsAuthor)
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: trusted human 071 note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 0, err
	}
	store.MarkNudged(key, obs.HeadSHA, 1, false, now)
	c.info("stage labels: trusted human told why the PR is at 071", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "reason", obs.Reason)
	return 1, nil
}
