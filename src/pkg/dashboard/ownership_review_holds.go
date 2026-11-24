package dashboard

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/fleetreport"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/pipeline"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Review-swarm hold reason codes (hivecommons/hive#12377). A PR the review
// pipeline places in human_hold for a review-swarm reason is your turn for
// any author; review_requires_human (#12329) covers requires_human verdicts.
const (
	// OwnershipReasonReviewRejected: a review verdict rejects the PR.
	OwnershipReasonReviewRejected = "review_rejected"
	// OwnershipReasonReviewDispatchHold: the review dispatcher parked the
	// current head for a person (reviews exhausted their retries, the fix cap
	// was reached, or no fixer agent is available).
	OwnershipReasonReviewDispatchHold = "review_dispatch_hold"
	// OwnershipReasonReviewLoopCap: the review/fix loop reached its cap.
	OwnershipReasonReviewLoopCap = "review_loop_cap"
)

func init() {
	ownershipReasonVerbs[OwnershipReasonReviewRejected] = OwnershipVerbReview
	ownershipReasonVerbs[OwnershipReasonReviewDispatchHold] = OwnershipVerbReview
	ownershipReasonVerbs[OwnershipReasonReviewLoopCap] = OwnershipVerbReview
}

// reviewSwarmState is what the review pipeline reads besides the verdicts:
// the dispatch state (its human holds and fix attempts) and the review-links
// ledger. Ownership reads the same records so the two surfaces agree.
type reviewSwarmState struct {
	dispatch review.DispatchState
	links    map[string]github.ReviewLink
	// releases are the persons' releases of verdict holds the self-merge
	// sweep recorded (#12962).
	releases map[string]github.ReviewHoldRelease
	// answer returns the trusted human's recorded answer standing against a
	// PR head (review.DecisionAnswerFor over the decision ledger the
	// un-park sweep writes); nil reads as no answer.
	answer func(repo string, number int, head string) (review.HumanChangeRequest, bool)
	// selfLogins are the hive's own App bot logins and ownerLogins the
	// dashboard owners (dashboard.authorized_users), both from
	// configuration. reviewDeciderOwnership reads them to tell who a
	// review's decision question names; display only.
	selfLogins  []string
	ownerLogins []string
}

// ownershipDecisionAnswers is the decision ledger ownership reads; tests swap
// it.
var ownershipDecisionAnswers = func() review.HumanChangeRequestStore { return review.DefaultHumanChangeRequests() }

// decisionAnswerLookup reads the ledger by the PR's full name: snapshot rows
// may carry the bare repository name, the ledger records owner/repo.
func decisionAnswerLookup(org string) func(repo string, number int, head string) (review.HumanChangeRequest, bool) {
	return func(repo string, number int, head string) (review.HumanChangeRequest, bool) {
		full := strings.TrimSpace(repo)
		if !strings.Contains(full, "/") && strings.TrimSpace(org) != "" {
			full = strings.TrimSpace(org) + "/" + full
		}
		return review.DecisionAnswerFor(ownershipDecisionAnswers(), full, number, head)
	}
}

// answeredChangeRequest is the person's instruction that answered the review
// verdict holds on pr's current head: recorded on that head, after every
// verdict hold began, and an instruction rather than a bare approval (an
// approval leaves the merge to the release rules, #12962). A verdict drawn
// after the answer is a new question, so it stands.
func (e ownershipEnv) answeredChangeRequest(pr github.PullRequest, holds []pipeline.ReviewHold) (review.HumanChangeRequest, bool) {
	if e.reviewSwarm.answer == nil || strings.TrimSpace(pr.HeadSHA) == "" {
		return review.HumanChangeRequest{}, false
	}
	req, ok := e.reviewSwarm.answer(pr.Repo, pr.Number, pr.HeadSHA)
	if !ok || req.IsApproval() {
		return review.HumanChangeRequest{}, false
	}
	verdictHeld := false
	for _, h := range holds {
		if !answerableReviewHold(h) {
			continue
		}
		verdictHeld = true
		if !h.Since.IsZero() && !req.RecordedAt.After(h.Since) {
			return review.HumanChangeRequest{}, false
		}
	}
	return req, verdictHeld
}

// answerableReviewHold reports whether a person's answer settles h: a
// requires_human or reject verdict does; the dispatcher's hold and the
// fix-loop cap stay with the owner whatever was answered.
func answerableReviewHold(h pipeline.ReviewHold) bool {
	return h.Kind == pipeline.ReviewHoldRequiresHuman || h.Kind == pipeline.ReviewHoldReject
}

// answeredWaitReason says who has the ball after a person answered the
// review: the author agent is fixing the head.
func answeredWaitReason(req review.HumanChangeRequest) string {
	who := "You"
	if a := strings.TrimSpace(req.Actor); a != "" {
		who = "@" + a
	}
	when := ""
	if !req.RecordedAt.IsZero() {
		when = " at " + req.RecordedAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("%s answered Hive's review on this head%s"+reviewAnsweredWaitTail, who, when)
}

// reviewAnsweredWaitTail ends every answeredWaitReason; reviewAnsweredWaiting
// reads it back for the overview's waiting breakdown.
const reviewAnsweredWaitTail = "; the hive is fixing it, and the review comes back to you if the new head needs you"

// reviewAnsweredWaiting reports a waiting decision reviewHoldOwnership made
// because a person answered the review on the head (answeredWaitReason).
func reviewAnsweredWaiting(o *Ownership) bool {
	return o != nil && o.WaitingElsewhere && !o.YourTurn && strings.HasSuffix(o.Reason, reviewAnsweredWaitTail)
}

// answeredPushRefusedReason says the answer is recorded but cannot be
// carried out: the hive cannot push the branch.
func answeredPushRefusedReason(req review.HumanChangeRequest, why string) string {
	who := "You"
	if a := strings.TrimSpace(req.Actor); a != "" {
		who = "@" + a
	}
	return fmt.Sprintf("%s answered Hive's review on this head, but the hive cannot push this branch: %s. Push the fix yourself, ask the author, or close it", who, why)
}

// loadReviewSwarm loads the verdict artifact, the dispatch state, the
// review-links ledger and the review hold releases /api/review/pipeline
// reads. A missing file reads as
// empty; an unreadable one is logged and treated as empty.
func (e *ownershipEnv) loadReviewSwarm() {
	if artifact, err := review.LoadArtifact(""); err == nil {
		e.reviewVerdicts = artifact.Items
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("ownership: review verdicts unreadable", "error", err)
	}
	if dispatch, err := review.LoadDispatchState(""); err == nil {
		e.reviewSwarm.dispatch = dispatch
	} else if !errors.Is(err, os.ErrNotExist) {
		slog.Warn("ownership: review dispatch state unreadable", "error", err)
	}
	if links, err := github.LoadReviewLinks(""); err == nil {
		e.reviewSwarm.links = links
	} else {
		slog.Warn("ownership: review links unreadable", "error", err)
	}
	if releases, err := github.LoadReviewHoldReleases(""); err == nil {
		e.reviewSwarm.releases = releases
	} else {
		slog.Warn("ownership: review hold releases unreadable", "error", err)
	}
}

// reviewHolds returns the review-swarm reasons /api/review/pipeline would put
// the PR in human_hold for, read through pipeline.ReviewHolds so the two
// surfaces cannot disagree.
func (e ownershipEnv) reviewHolds(pr github.PullRequest) []pipeline.ReviewHold {
	if len(e.reviewVerdicts) == 0 && len(e.reviewSwarm.dispatch.Human) == 0 && len(e.reviewSwarm.dispatch.Fixes) == 0 && len(e.reviewSwarm.dispatch.FixAttempts) == 0 {
		return nil
	}
	in := pipeline.Inputs{
		PR:       github.ReviewQueueEntry{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, HiveAuthored: prAgentRole(pr) != ""},
		Verdicts: e.reviewVerdicts,
		Dispatch: e.reviewSwarm.dispatch,
	}
	if link, ok := lookupReviewLink(e.reviewSwarm.links, pr.Repo, reviewPipelineBareRepo(pr.Repo), pr.Number); ok {
		in.ReviewLink = &link
	}
	if release, ok := github.ReviewHoldReleaseFor(e.reviewSwarm.releases, pr.Repo, pr.Number); ok {
		in.HoldRelease = &release
	}
	return pipeline.ReviewHolds(in)
}

// reviewHoldOwnership is the your-turn decision for a PR the review swarm
// holds for a person, for any author (#12377). A Hive PR still inside the
// adjudicator's needs-human wait waits for the adjudicator first; the wait is
// bounded by ownershipAdjudicatorWait.
func (e ownershipEnv) reviewHoldOwnership(pr github.PullRequest, withinAdjudicatorWait bool) (Ownership, bool) {
	if withinAdjudicatorWait {
		return Ownership{}, false
	}
	holds := e.reviewHolds(pr)
	if len(holds) == 0 {
		return Ownership{}, false
	}
	// A person already answered the verdict on this head with an
	// instruction (`/hive decision <text>`, a changes-requested review):
	// the ball is with the author agent until it pushes a new head or the
	// swarm asks again. Danathar/dns-failover#155 stayed "Hive's review needs
	// a human decision" for over an hour after the hive moved it to
	// 070-fixing on the owner's answer. A dispatcher hold (fix cap, no
	// fixer) or the loop cap is still the owner's.
	if req, ok := e.answeredChangeRequest(pr, holds); ok {
		// An answer only the hive's fixer could carry out, on a branch the
		// hive cannot push, leaves the PR with the person who answered
		// (projectbluefin/utah#919, projectbluefin/testsuite#1037).
		if e.adoptedPushRefused != nil {
			if why := e.adoptedPushRefused(pr); why != "" {
				return yourTurn(OwnershipReasonAdoptedPushRefused, answeredPushRefusedReason(req, why)), true
			}
		}
		var rest []pipeline.ReviewHold
		for _, h := range holds {
			if !answerableReviewHold(h) {
				rest = append(rest, h)
			}
		}
		if len(rest) == 0 {
			return waitingElsewhereFor(answeredWaitReason(req)), true
		}
		holds = rest
	}
	pick := func(kind string) (pipeline.ReviewHold, bool) {
		for _, h := range holds {
			if h.Kind == kind {
				return h, true
			}
		}
		return pipeline.ReviewHold{}, false
	}
	if _, ok := pick(pipeline.ReviewHoldReject); ok {
		return yourTurn(OwnershipReasonReviewRejected, "Hive's review rejected this pull request"), true
	}
	if _, ok := pick(pipeline.ReviewHoldRequiresHuman); ok {
		if o, ok := e.reviewDeciderOwnership(pr, holds); ok {
			return o, true
		}
		return yourTurn(OwnershipReasonReviewRequiresHuman, "Hive's review needs a human decision"), true
	}
	if h, ok := pick(pipeline.ReviewHoldDispatch); ok {
		return yourTurn(OwnershipReasonReviewDispatchHold, "Hive's review is parked for a person: "+strings.TrimPrefix(h.Reason, "review dispatch parked for a human: ")), true
	}
	h := holds[0]
	return yourTurn(OwnershipReasonReviewLoopCap, "Hive's review/fix loop stopped: "+h.Reason), true
}

// Display-only reading of who a requires_human question waits on. The hive's
// review usually names the person who should decide in its
// `<!-- hive-decision-question: ... -->` marker, which the verdict relay
// stores as the verdict's DecisionQuestion. A question addressed to someone
// other than the dashboard owner is theirs to answer for a while, then comes
// back to the owner. Nothing here changes the hold, the review or the merge:
// the PR stays held for a human decision exactly as before.
const (
	// reviewDeciderAnswerWindow is how long a question that names its
	// decider waits on that person before it is the owner's turn again.
	reviewDeciderAnswerWindow = 3 * 24 * time.Hour
	// reviewDeciderAskedPhrase is in every waiting reason
	// reviewDeciderOwnership returns; reviewDeciderWaiting reads it back so
	// every surface places the row by the same rule.
	reviewDeciderAskedPhrase = ": Hive's review asked on "
)

// reviewDeciderNotPeople are logins that are bots or apps even without a
// "[bot]" suffix; github.IsDependencyBotAuthor covers the dependency bots.
var reviewDeciderNotPeople = map[string]bool{"copilot": true, "github-actions": true, "codex": true, "coderabbitai": true}

// reviewDeciderMaintainerWordRE finds a question that asks a role rather than
// the person it mentions: "a maintainer", "the owners", "an approver".
var reviewDeciderMaintainerWordRE = regexp.MustCompile(`(?i)\b(?:(?:a|an|the)\s+)?(?:maintainers?|owners?|approvers?)\b`)

// reviewDeciderCodeRE matches fenced code blocks and inline code spans; a
// login quoted there is not a mention.
var reviewDeciderCodeRE = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`")

// reviewDeciderLoginsFor returns the hive's own logins (the App bot, the
// additional bot logins and the AI author) and the dashboard owners from
// configuration, for reviewSwarmState. The device-flow operator that
// FleetReportOwners also counts is not read: resolving it validates a token
// against GitHub, which the ownership pass does not do.
func reviewDeciderLoginsFor(cfg *config.Config) (self, owners []string) {
	if cfg == nil {
		return nil, nil
	}
	for _, l := range append([]string{cfg.GitHub.BotLogin(), cfg.EffectiveAIAuthor()}, cfg.GitHub.AdditionalBotLogins...) {
		if l = strings.TrimSpace(l); l != "" {
			self = append(self, l)
		}
	}
	return self, fleetreport.NormalizeLogins(cfg.Dashboard.OwnerLogins())
}

// reviewDeciderDate renders the day the question was asked (UTC), with the
// year when it is not now's year.
func reviewDeciderDate(since, now time.Time) string {
	since, now = since.UTC(), now.UTC()
	if since.Year() != now.Year() {
		return since.Format("Jan 2, 2006")
	}
	return since.Format("Jan 2")
}

// reviewDeciderWaitReason is the waiting reason while the named decider has
// the question.
func reviewDeciderWaitReason(decider string, since, now time.Time) string {
	return "Waits on " + decider + reviewDeciderAskedPhrase + reviewDeciderDate(since, now)
}

// reviewDeciderLapsedReason is the your-turn reason once the decider has had
// the question for reviewDeciderAnswerWindow.
func reviewDeciderLapsedReason(decider string, since, now time.Time) string {
	return "Hive's review needs a human decision: " + decider + " has not answered since " + reviewDeciderDate(since, now)
}

// reviewDeciderWaiting reports a waiting decision reviewDeciderOwnership
// made: a requires_human question waiting on the person it names.
func reviewDeciderWaiting(o *Ownership) bool {
	return o != nil && o.WaitingElsewhere && !o.YourTurn &&
		strings.HasPrefix(o.Reason, "Waits on ") && strings.Contains(o.Reason, reviewDeciderAskedPhrase)
}

// ignoredDeciderMention reports a mentioned login that cannot be the
// decider: a bot or app account, the policy's `@maintainer` placeholder
// (stagelabels.DecisionQuestionPlaceholderMention, the rule the 085 comment
// applies), or one of the hive's own logins.
func (s reviewSwarmState) ignoredDeciderMention(login string) bool {
	l := strings.ToLower(strings.TrimSpace(login))
	if l == "" || strings.HasSuffix(l, "[bot]") || reviewDeciderNotPeople[l] || github.IsDependencyBotAuthor(l+"[bot]") ||
		stagelabels.DecisionQuestionPlaceholderMention("@"+l) {
		return true
	}
	for _, self := range s.selfLogins {
		if strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(self), "[bot]"), strings.TrimSuffix(l, "[bot]")) {
			return true
		}
	}
	return false
}

// reviewQuestionDecider names the person a review's decision question asks,
// from its @-mentions outside code (github.MentionLogins) less bots,
// placeholders and the hive's own logins:
//   - exactly one mentioned login other than the PR's author decides;
//   - with no other login, the author decides when named, unless the
//     question asks a role ("a maintainer", "the owners", "an approver");
//   - anything else (two or more other logins, a team, nobody) names no
//     one, and "" is returned.
func reviewQuestionDecider(question, author string, ignored func(string) bool) string {
	text := reviewDeciderCodeRE.ReplaceAllString(question, " ")
	author = strings.TrimSpace(author)
	var others []string
	authorNamed := false
	for _, login := range github.MentionLogins(text) {
		if ignored != nil && ignored(login) {
			continue
		}
		if strings.Contains(login, "/") {
			// A team is not one person.
			return ""
		}
		if author != "" && strings.EqualFold(login, author) {
			authorNamed = true
			continue
		}
		others = append(others, login)
	}
	switch {
	case len(others) == 1:
		return others[0]
	case len(others) == 0 && authorNamed && !reviewDeciderMaintainerWordRE.MatchString(text):
		return author
	}
	return ""
}

// reviewDeciderAsks returns the person a verdict's question names when the
// verdict is a requires_human decision request (not a reject, not the fix
// loop's cap, which keeps its question but is the owner's), or "".
func (e ownershipEnv) reviewDeciderAsks(pr github.PullRequest, a *review.Aggregate) string {
	if a == nil || a.Verdict == review.VerdictReject || !pipeline.RequiresHuman(a) || !a.DecisionRequested || review.FixCycleCapReached(*a) {
		return ""
	}
	q := strings.TrimSpace(a.DecisionQuestion)
	if q == "" {
		return ""
	}
	return reviewQuestionDecider(q, pr.Author, e.reviewSwarm.ignoredDeciderMention)
}

// reviewDeciderOwnership is the display decision for a requires_human hold
// whose question names one person other than the dashboard owner: waiting on
// that person within reviewDeciderAnswerWindow of the question, then your
// turn with the same reason code as any requires_human hold. It applies only
// when the requires_human verdict on the current head is the whole hold (the
// dispatcher's hold that same verdict placed is part of it); a reject, a
// fix-cap verdict or dispatcher hold, a no-fixer hold, the loop cap, a hold
// carried from an earlier head or an unnamed question keep today's decision
// (ok false). A person's recorded answer is read before this
// (answeredChangeRequest) and wins as before.
func (e ownershipEnv) reviewDeciderOwnership(pr github.PullRequest, holds []pipeline.ReviewHold) (Ownership, bool) {
	if e.now.IsZero() {
		return Ownership{}, false
	}
	entry := github.ReviewQueueEntry{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA}
	current, _ := pipeline.HeadVerdict(e.reviewVerdicts, entry)
	decider := e.reviewDeciderAsks(pr, current)
	if decider == "" {
		return Ownership{}, false
	}
	var holdSince time.Time
	for _, h := range holds {
		switch h.Kind {
		case pipeline.ReviewHoldRequiresHuman:
			holdSince = h.Since
		case pipeline.ReviewHoldDispatch:
			if !e.dispatchHoldPlacedBy(pr, *current) {
				return Ownership{}, false
			}
		default:
			return Ownership{}, false
		}
	}
	for _, owner := range e.reviewSwarm.ownerLogins {
		if strings.EqualFold(owner, decider) {
			// The question asks the owner: it is the owner's turn as before.
			return Ownership{}, false
		}
	}
	since := e.reviewDeciderSince(pr, decider, holdSince, current.FirstRecorded())
	if since.IsZero() {
		return Ownership{}, false
	}
	if e.now.Sub(since) < reviewDeciderAnswerWindow {
		return waitingElsewhereFor(reviewDeciderWaitReason(decider, since, e.now)), true
	}
	return yourTurn(OwnershipReasonReviewRequiresHuman, reviewDeciderLapsedReason(decider, since, e.now)), true
}

// reviewDeciderSince is when the decider was first asked: the oldest verdict
// of the unbroken run, at the end of the PR's blocking chain
// (review.LatestVerdictHold, oldest first), whose question names the same
// person. A new head or a re-collected verdict that asks the same person
// again does not restart the window; a verdict that cleared the hold, or one
// that asked someone else, ends the run. Without a chain it is the hold's
// start, then the verdict's.
func (e ownershipEnv) reviewDeciderSince(pr github.PullRequest, decider string, holdSince, verdictSince time.Time) time.Time {
	since := holdSince
	if since.IsZero() {
		since = verdictSince
	}
	var mine []review.Aggregate
	for _, a := range e.reviewVerdicts {
		if a.Number == pr.Number && pipeline.SameRepo(a.Repo, pr.Repo) {
			mine = append(mine, a)
		}
	}
	chain, ok := review.LatestVerdictHold(mine)
	if !ok {
		return since
	}
	for i := len(chain.Verdicts) - 1; i >= 0; i-- {
		held := chain.Verdicts[i]
		var asked *review.Aggregate
		for j := range mine {
			if strings.TrimSpace(mine[j].HeadSHA) == strings.TrimSpace(held.HeadSHA) && mine[j].FirstRecorded().Equal(held.RecordedAt) {
				asked = &mine[j]
				break
			}
		}
		if who := e.reviewDeciderAsks(pr, asked); who == "" || !strings.EqualFold(who, decider) {
			break
		}
		if !held.RecordedAt.IsZero() && (since.IsZero() || held.RecordedAt.Before(since)) {
			since = held.RecordedAt
		}
	}
	return since
}

// dispatchHoldPlacedBy reports whether the dispatcher's hold on pr's head is
// the one it parks for agg's own requires_human verdict, read the way
// pipeline.ReviewHolds finds that hold.
func (e ownershipEnv) dispatchHoldPlacedBy(pr github.PullRequest, agg review.Aggregate) bool {
	head := strings.TrimSpace(pr.HeadSHA)
	for _, h := range e.reviewSwarm.dispatch.Human {
		if h.Number != pr.Number || !pipeline.SameRepo(h.Repo, pr.Repo) {
			continue
		}
		if held := strings.TrimSpace(h.HeadSHA); held != "" && head != "" && held != head {
			continue
		}
		return h.PlacedByVerdict(agg)
	}
	return false
}
