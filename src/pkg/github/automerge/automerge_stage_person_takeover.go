package automerge

// The opt-in timer for a person's sent-back PR
// (governor.labels.stage.adoption.person_after, hivecommons/hive#19395).
//
// A person's PR (personAuthored: author/human, or one of this hive's owners)
// is never adopted on the hive's own initiative (hivecommons/hive#15202);
// only an operator's Adopt now takes it over. In a repository an owner
// listed (person_repos for owners' agent-written PRs, person_human_repos for
// author/human ones, ACMM L6 only), this timer is the one way out without a
// click. It runs in the stage pass after the stage write landed, for a PR
// at 071 / 072 whose stage the write left there:
//
//  1. The PR has been idle for person_after on the abandon_after idle
//     anchor (stagelabels.Record.PersonIdleSince): no head push the hive did
//     not make, no new comment or review by anyone but the hive.
//  2. Nothing says stop: no hold, do-not-merge, needs-human or
//     hive/keep-open label, and the head is not on a fork (Adopt now makes a
//     hive copy, which a fork PR never gets).
//  3. It is work the hive can do (personTakeoverWork): the PR was sent back
//     for a merge conflict, a failed check, or a change request from the
//     review swarm or a review bot. A change request by a person (a
//     non-bot reviewer) means a person is engaged, so the hive keeps
//     waiting for the author.
//
// Then the hive posts one notice and waits person_grace (default 24h). Any
// activity in the grace, or a stop label, cancels the countdown; a fresh
// read of comments and reviews comes before the takeover. The takeover
// itself is the operator's Adopt now: the timer records the same forced
// mark (stagelabels.Store.MarkPersonTakeover, actor
// stagelabels.AdoptForcedByPersonTimer) and the adoption/supersede block of
// the same pass runs adoptContributorPR, whose adoption is recorded with
// reason AdoptReasonPersonTimer. The delegate's refusal to adopt a person's
// PR (delegatedecision.RefusalAdoptPersonAuthored) is untouched: this is a
// separate, timed path.

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// PersonTakeoverNoticeMarker opens the one notice the hive posts before it
// takes a person's PR over on the timer.
const PersonTakeoverNoticeMarker = "<!-- hive:person-takeover-notice"

// What a person's PR was sent back for, when it is work the hive can do
// (personTakeoverWork).
const (
	personWorkConflict = "a merge conflict with the base branch"
	personWorkCheck    = "a failed check"
	personWorkChanges  = "changes the review swarm or a review bot asked for"
)

// personTakeoverApplies reports whether the timer is on for obs: a person's
// PR (personAuthored) in a repository listed for its kind, with adoption on.
// human is true for an author/human PR (person_human_repos), false for an
// owner's agent-written one (person_repos).
func personTakeoverApplies(obs stageObservation, policy StageLabelPolicy) (human, ok bool) {
	if policy.PersonAfter <= 0 || policy.PersonTakeover == nil || !policy.Adoption || !personAuthored(obs, policy) {
		return false, false
	}
	human = obs.AuthorKind == authorkind.Human
	if !policy.PersonTakeover(obs.Owner+"/"+obs.Repo, human) {
		return false, false
	}
	return human, true
}

// personGrace is policy.PersonGrace with its default.
func (p StageLabelPolicy) personGrace() time.Duration {
	if p.PersonGrace > 0 {
		return p.PersonGrace
	}
	return config.DefaultPersonTakeoverGrace
}

// personTakeoverWork names what a person's PR at 071 / 072 was sent back
// for when it is work the hive can do, or "" when it is not. The lane
// reason decides; a lane that stopped at a gate before CI or the verdict
// is read from the human route's evidence, and then every route reason
// must be one of the three. A maintainer's change request
// (human-changes-requested), a `/hive decision` answer, a reviewer's
// question for the author, a missing sign-off and anything else a person
// must do is not.
func personTakeoverWork(obs stageObservation) string {
	r := strings.TrimSpace(obs.Reason)
	switch {
	case r == autoMergeReasonHumanChangesRequested, r == stageReasonDecisionAnswered:
		return ""
	case r == "conflicting" || r == "not-mergeable" || r == "label:needs-rebase" || strings.HasPrefix(r, "merge conflict"):
		return personWorkConflict
	case strings.HasPrefix(r, stageReasonOptionalCheckFailing), classifyCIReason(r) == ciReasonRed:
		return personWorkCheck
	case r == "changes-requested", r == contributorLaneReasonChangesRequested, r == stagelabels.LaneReasonChangesRequested,
		strings.HasPrefix(r, autoMergeReasonReviewGatePrefix+ReviewGateOutcomeChangesRequested):
		return personWorkChanges
	}
	if obs.Route == nil || len(obs.Route.Reasons) == 0 {
		return ""
	}
	work := ""
	for _, rr := range obs.Route.Reasons {
		switch {
		case strings.HasPrefix(rr, "merge conflict"):
			work = nonEmpty(work, personWorkConflict)
		case strings.HasPrefix(rr, "red checks: "):
			work = nonEmpty(work, personWorkCheck)
		case strings.HasPrefix(rr, "verdict changes_requested"):
			work = nonEmpty(work, personWorkChanges)
		default:
			return ""
		}
	}
	return work
}

// personTakeoverStop reports why the timer must not count down on obs this
// pass, from what the pass already holds (no GitHub call), or "". written
// is the stage the pass wrote.
func (c *Engine) personTakeoverStop(obs stageObservation, rec stagelabels.Record, written stagelabels.Stage) string {
	switch {
	case !stagelabels.PersonTakeoverStage(written) || written != rec.Stage:
		return "not waiting on its author at 071 / 072"
	case adoptionBlocked(obs.Labels) != "":
		return "held: " + adoptionBlocked(obs.Labels)
	case c.abandonCloseHeld(obs) != "":
		return c.abandonCloseHeld(obs)
	case hasLabelFold(obs.Labels, config.DefaultNeedsHumanLabel):
		return "needs-human: " + config.DefaultNeedsHumanLabel
	case strings.TrimSpace(obs.HeadRepo) != "" && adoptionFromFork(obs):
		// Adopt now never pushes in place, and a fork's code is never moved
		// into a hive copy (supersedeAdoptedPR, hivecommons/hive#14306): the
		// takeover could not happen, so no notice promises one.
		return "a fork PR, which Adopt now cannot take over"
	case rec.PersonActivitySinceNotice():
		return "activity since the notice"
	case strings.TrimSpace(obs.Detail.HumanChangeRequester) != "":
		return "@" + strings.TrimSpace(obs.Detail.HumanChangeRequester) + " requested changes"
	case personTakeoverWork(obs) == "":
		return "sent back for " + nonEmpty(strings.TrimSpace(obs.Reason), "an unknown reason") + ", which only a person can answer"
	}
	if by, ok := rec.AdoptionForced(); ok && by != stagelabels.AdoptForcedByPersonTimer {
		return "an operator's Adopt now is pending"
	}
	if to := rec.Rerouted(obs.HeadSHA); to != "" {
		// An operator gave the follow-up work to another clanker; taking
		// the PR over would drop that choice (NoteAdoptionForced clears it).
		return "an operator rerouted its work to " + to
	}
	return ""
}

// personChangeRequester returns a person (not the App, another bot or one
// of the hive's own identities) whose latest opinionated review on the PR
// (APPROVED / CHANGES_REQUESTED / DISMISSED) is CHANGES_REQUESTED, or "".
// The review swarm's and review bots' change requests are work the hive
// may pick up; a person's means a person is engaged with the PR.
func (c *Engine) personChangeRequester(ctx context.Context, obs stageObservation, policy StageLabelPolicy) (string, error) {
	latest := map[string]*gh.PullRequestReview{}
	opts := &gh.ListOptions{PerPage: 100}
	for page := 0; page < abandonReadPages; page++ {
		reviews, resp, err := c.gh.PullRequests.ListReviews(ctx, obs.Owner, obs.Repo, obs.Number, opts)
		if err != nil {
			return "", fmt.Errorf("listing reviews for %s#%d: %w", obs.Display, obs.Number, err)
		}
		for _, rv := range reviews {
			login := strings.ToLower(strings.TrimSpace(rv.GetUser().GetLogin()))
			if login == "" {
				continue
			}
			switch strings.ToUpper(strings.TrimSpace(rv.GetState())) {
			case "APPROVED", "CHANGES_REQUESTED", "DISMISSED":
				latest[login] = rv
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	for _, rv := range latest {
		if !strings.EqualFold(strings.TrimSpace(rv.GetState()), "CHANGES_REQUESTED") || !c.outsideActor(rv.GetUser()) {
			continue
		}
		login := strings.TrimSpace(rv.GetUser().GetLogin())
		if policy.HiveAuthor != nil && policy.HiveAuthor(login) {
			continue
		}
		return login, nil
	}
	return "", nil
}

// takeOverPersonPRIfDue runs the timer for one PR after the pass's stage
// write landed: post the notice once, then after the grace and a fresh read
// record the Adopt now mark. It returns the number of comments posted and
// whether the caller should run the Adopt now path for the PR this pass.
// The caller counts the comments as the hive's own.
func (c *Engine) takeOverPersonPRIfDue(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, written stagelabels.Stage, now time.Time) (int, bool, error) {
	rec, ok := store.Get(key)
	if !ok {
		return 0, false, nil
	}
	human, applies := personTakeoverApplies(obs, policy)
	logArgs := []any{"repo", obs.Display, "pr", obs.Number, "author", obs.Author, "author_kind", obs.AuthorKind.String(), "stage", written.String(), "reason", obs.Reason, "human_list", human}
	if !applies {
		// Off, unlisted, below L6 or not a person's PR: a countdown left
		// from before is withdrawn.
		if store.ResetPersonNotice(key, now) {
			c.info("stage labels: person PR takeover countdown cancelled: the timer no longer applies", logArgs...)
		}
		return 0, false, nil
	}
	if why := c.personTakeoverStop(obs, rec, written); why != "" {
		// A stop label, activity or a change of park cancels the
		// countdown (and a takeover handed over but not yet attempted):
		// once it clears, the hive warns again and waits a full grace.
		if store.ResetPersonNotice(key, now) {
			c.info("stage labels: person PR takeover countdown cancelled", append(logArgs, "why", why)...)
		}
		return 0, false, nil
	}
	if rec.PersonTakeoverPending() {
		// Handed to Adopt now on an earlier pass and not attempted yet (the
		// adoption deferred): hand it over again.
		return 0, true, nil
	}
	after, grace := policy.PersonAfter, policy.personGrace()
	if rec.PersonNoticeAt.IsZero() {
		if rec.PersonIdleFor(now) < after {
			return 0, false, nil
		}
		return c.postPersonTakeoverNotice(ctx, store, key, obs, policy, rec, grace, now, logArgs)
	}
	if !rec.PersonTakeoverDue(grace, now) {
		return 0, false, nil
	}
	// A fresh read before the takeover: a reply or review the lane's counts
	// never saw keeps the PR with its author and restarts the clock.
	at, err := c.lastOutsideActivitySince(ctx, obs, rec.PersonNoticeAt)
	if err != nil {
		c.warn("stage labels: person PR takeover check failed; not taken over", append(logArgs, "error", err)...)
		return 0, false, err
	}
	if !at.IsZero() {
		store.NotePersonActivity(key, at, now)
		store.ResetPersonNotice(key, now)
		c.info("stage labels: person PR not taken over: activity since the notice", append(logArgs, "activity_at", at)...)
		return 0, false, nil
	}
	who, err := c.personChangeRequester(ctx, obs, policy)
	if err != nil {
		c.warn("stage labels: person PR takeover review read failed; not taken over", append(logArgs, "error", err)...)
		return 0, false, err
	}
	if who != "" {
		store.ResetPersonNotice(key, now)
		c.info("stage labels: person PR not taken over: a person requested changes", append(logArgs, "reviewer", who)...)
		return 0, false, nil
	}
	store.MarkPersonTakeover(key, now)
	c.info("stage labels: person PR handed to Adopt now by the takeover timer", append(logArgs, "trigger", AdoptTriggerPersonTimer, "by", stagelabels.AdoptForcedByPersonTimer, "noticed_at", rec.PersonNoticeAt, "person_after", after.String(), "grace", grace.String(), "work", personTakeoverWork(obs))...)
	return 0, true, nil
}

// postPersonTakeoverNotice posts the one notice for a PR that just crossed
// person_after, after a confirming read: no outside comment or review since
// the idle clock started, and no person's change request standing. A read
// that finds a person's change request, or fails, backs off an hour
// (Record.PersonCheckBackoff). The store saves after the pass, so a notice
// of the hive's own already posted in this countdown (a restart between the
// post and the save) is taken as the notice instead of being posted again.
func (c *Engine) postPersonTakeoverNotice(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, rec stagelabels.Record, grace time.Duration, now time.Time, logArgs []any) (int, bool, error) {
	if rec.PersonCheckBackoff(now) {
		// A person's change request (or a failed read) an hour ago: not
		// read again every sweep.
		return 0, false, nil
	}
	// The write-back gate is asked before any read: a notice it refuses
	// (supersession_notice needs the opt-in label below full mode) would
	// otherwise cost the reads every sweep. A refusal backs off an hour.
	body := PersonTakeoverNoticeComment(obs, rec, personTakeoverWork(obs), grace, now)
	if d := c.writebackAllowed(obs.Display, writebackItemForObservation(obs), writeback.KindSupersessionNotice, body); !d.Allowed {
		store.NotePersonCheck(key, now)
		c.info("stage labels: person PR takeover notice suppressed by the write-back gate", append(logArgs, "writeback_reason", d.Reason, "writeback_detail", d.Detail)...)
		return 0, false, nil
	}
	since := rec.PersonIdleSince()
	at, err := c.lastOutsideActivitySince(ctx, obs, since)
	if err != nil {
		store.NotePersonCheck(key, now)
		c.warn("stage labels: person PR takeover confirming read failed; not warned yet", append(logArgs, "error", err)...)
		return 0, false, err
	}
	if !at.IsZero() {
		store.NotePersonActivity(key, at, now)
		c.debug("stage labels: person PR takeover confirming read found activity; idle clock restarted", append(logArgs, "activity_at", at)...)
		return 0, false, nil
	}
	who, err := c.personChangeRequester(ctx, obs, policy)
	if err != nil {
		store.NotePersonCheck(key, now)
		c.warn("stage labels: person PR takeover review read failed; not warned yet", append(logArgs, "error", err)...)
		return 0, false, err
	}
	if who != "" {
		store.NotePersonCheck(key, now)
		c.debug("stage labels: person PR not warned: a person requested changes", append(logArgs, "reviewer", who)...)
		return 0, false, nil
	}
	posted, err := c.ownPersonNoticeSince(ctx, obs, rec.PersonCountdownStart())
	if err != nil {
		store.NotePersonCheck(key, now)
		c.warn("stage labels: person PR takeover notice check failed; not warned yet", append(logArgs, "error", err)...)
		return 0, false, err
	}
	if !posted.IsZero() {
		store.MarkPersonNoticed(key, posted)
		c.info("stage labels: person PR takeover notice already posted; countdown resumed", append(logArgs, "noticed_at", posted)...)
		return 0, false, nil
	}
	// The notice opens with PersonTakeoverNoticeMarker, which
	// ownPersonNoticeSince finds after a restart; the gate allowed it above.
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		c.warn("stage labels: person PR takeover notice failed", append(logArgs, "error", err)...)
		return 0, false, err
	}
	store.MarkPersonNoticed(key, now)
	c.info("stage labels: person PR warned before the takeover", append(logArgs, "person_after", policy.PersonAfter.String(), "grace", grace.String())...)
	return 1, false, nil
}

// ownPersonNoticeSince returns when the hive's own takeover notice on the
// PR was posted, if one was posted at or after since, or zero. Only the
// hive's App login counts: anyone can type PersonTakeoverNoticeMarker.
func (c *Engine) ownPersonNoticeSince(ctx context.Context, obs stageObservation, since time.Time) (time.Time, error) {
	if c.transport == nil || normalizeBotLogin(c.transport.AppBotLogin()) == "" {
		return time.Time{}, nil
	}
	cm, err := c.findOwnMarkerComment(ctx, obs.Owner, obs.Repo, obs.Number, func(body string) bool {
		return strings.HasPrefix(body, PersonTakeoverNoticeMarker)
	})
	if err != nil || cm == nil {
		return time.Time{}, err
	}
	at := cm.GetCreatedAt().Time
	if at.Before(since) {
		return time.Time{}, nil
	}
	return at, nil
}

// PersonTakeoverNoticeComment is the one notice before the hive takes a
// person's idle PR over on the timer. work is personTakeoverWork's answer.
func PersonTakeoverNoticeComment(obs stageObservation, rec stagelabels.Record, work string, grace time.Duration, now time.Time) string {
	waits := nonEmpty(work, "a change the hive can make")
	if r := strings.TrimSpace(obs.Reason); r != "" {
		waits += " (`" + strings.ReplaceAll(r, "`", "'") + "`)"
	}
	return fmt.Sprintf("%s head=%s -->\n"+
		"This pull request has had no push, comment or review for %s while it waits on %s. "+
		"That is work the hive can do, and this repository lets the hive finish a person's pull request that stays idle like this. "+
		"Unless someone pushes, comments or reviews here, or adds a `hold` label, the hive will take it over once %s have passed since this notice.\n\n"+
		"Taking it over keeps your commits and sign-off. To keep it yourself, push, comment, or add `hold`.",
		PersonTakeoverNoticeMarker, obs.HeadSHA, durationWords(rec.PersonIdleFor(now)), waits, graceText(grace))
}
