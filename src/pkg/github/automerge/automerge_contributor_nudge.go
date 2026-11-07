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
	// Name the checks first, so a fork guard among them is dropped below.
	checks = c.nameBareCIChecks(ctx, obs, checks)
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

// bareCIToken reports a failing-check entry that is the CI gate's own token
// ("check-failure", "status-error") rather than a check's name. The gate
// names no check when the base branch requires none (EvaluateCommitCI with
// a known-empty required set), so the nudge listed `check-failure` instead
// of the check that failed.
func bareCIToken(entry string) bool {
	rest, ok := strings.CutPrefix(entry, "check-")
	if !ok {
		rest, ok = strings.CutPrefix(entry, "status-")
	}
	if !ok {
		return false
	}
	switch rest {
	case "failure", "error", "timed_out", "cancelled", "action_required", "startup_failure", "stale":
		return true
	}
	return false
}

// nameBareCIChecks replaces the CI gate's bare tokens in checks with the
// names of the checks that are red on the head. It reads the head's check
// digest only when a bare token is present, and keeps checks as they are
// when the digest cannot be read or names nothing.
func (c *Engine) nameBareCIChecks(ctx context.Context, obs stageObservation, checks []string) []string {
	if !slices.ContainsFunc(checks, bareCIToken) || obs.HeadSHA == "" {
		return checks
	}
	sum, err := c.summarizeChecks(ctx, obs.Owner, obs.Repo, obs.HeadSHA, func() map[string]bool { return nil })
	if err != nil || len(sum.Red) == 0 {
		return checks
	}
	out := make([]string, 0, len(checks)+len(sum.Red))
	for _, name := range checks {
		if !bareCIToken(name) {
			out = append(out, name)
		}
	}
	for _, name := range sum.Red {
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// nudgeWithdrawnMarker follows the nudge marker on a nudge that was
// withdrawn, so the withdrawal edits each comment once.
const nudgeWithdrawnMarker = "<!-- hive:contributor-nudge-withdrawn -->"

// nudgeWithdrawReason reports a recorded reason that says the head's red is
// not the head's: an open shared-CI incident, or checks that are red on the
// base branch too. Both keep the PR out of 072 (SharedCIDeferredStage,
// BaseBranchRedStage), so a nudge already posted for the head is wrong.
func nudgeWithdrawReason(reason string) bool {
	return strings.HasPrefix(reason, stageReasonSharedCIIncident) || strings.HasPrefix(reason, stageReasonBaseBranchRed)
}

// WithdrawnNudgeComment renders the edit that replaces a nudge once its red
// is recognised as a shared incident's or the base branch's. It keeps the
// original marker line and drops the @-mention: an edit notifies nobody,
// and the correction is for whoever reads the thread.
func WithdrawnNudgeComment(previous, reason string) string {
	marker, _, _ := strings.Cut(previous, "\n")
	var b strings.Builder
	b.WriteString(marker + "\n" + nudgeWithdrawnMarker + "\n")
	b.WriteString("**Withdrawn:** this PR is not back in your court after all. ")
	if n, ok := strings.CutPrefix(reason, stageReasonSharedCIIncident); ok {
		fmt.Fprintf(&b, "The failing check is part of shared CI incident #%s, which affects other PRs too; this change did not break it. ", n)
		fmt.Fprintf(&b, "There is nothing to fix here: the PR is back on the normal path, and the hive re-runs the failed checks once #%s closes.\n", n)
		return b.String()
	}
	checks := strings.Split(strings.TrimPrefix(reason, stageReasonBaseBranchRed), ",")
	for i, name := range checks {
		checks[i] = "`" + strings.TrimSpace(name) + "`"
	}
	fmt.Fprintf(&b, "The failing checks (%s) are red on the base branch too; this change did not break them. ", strings.Join(checks, ", "))
	b.WriteString("There is nothing to fix here: the PR is back on the normal path, and the hive checks it again once the base branch is green.\n")
	return b.String()
}

// withdrawNudge edits this head's 072 nudge comments in place once the
// stage pass reads the head's red as a shared CI incident or the base
// branch's (nudgeWithdrawReason), and forgets the nudge so a genuine return
// to 072 on the same head is nudged afresh. The nudge goes out the moment a
// PR enters 072, and the scanner's incident marker can land minutes later:
// without this the PR left 072 but the thread kept saying "the hive will not
// merge it until the head changes" (hivecommons/hive#17003). The record is
// only cleared once every comment is edited, so a failed read or edit is
// retried on the next pass.
func (c *Engine) withdrawNudge(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation) {
	rec, ok := store.Get(key)
	if !ok || obs.HeadSHA == "" || rec.NudgeCount == 0 || rec.NudgeHead != obs.HeadSHA || !nudgeWithdrawReason(obs.Reason) {
		return
	}
	f := &engineStatusForge{e: c, owner: obs.Owner, repo: obs.Repo, number: obs.Number}
	comments, err := f.ListComments(ctx)
	if err != nil {
		c.warn("stage labels: could not list comments to withdraw the contributor nudge", "repo", obs.Display, "pr", obs.Number, "error", err)
		return
	}
	prefix := ContributorNudgeMarkerPrefix + " head=" + obs.HeadSHA + " "
	edited := 0
	for _, cm := range comments {
		if !cm.Own || !strings.HasPrefix(cm.Body, prefix) || strings.Contains(cm.Body, nudgeWithdrawnMarker) {
			continue
		}
		// Rewrites the hive's own nudge; an edit notifies nobody.
		if err := f.EditComment(ctx, cm.ID, WithdrawnNudgeComment(cm.Body, obs.Reason)); err != nil {
			c.warn("stage labels: contributor nudge withdrawal failed", "repo", obs.Display, "pr", obs.Number, "error", err)
			return
		}
		edited++
	}
	store.WithdrawNudge(key, obs.HeadSHA)
	c.info("stage labels: contributor nudge withdrawn", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "reason", obs.Reason, "comments_edited", edited)
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
