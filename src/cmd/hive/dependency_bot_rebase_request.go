package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Stranded dependency-bot branches. The hive never base-syncs a dependency
// bot's branch (github.DependencyBotBranch): the bot rebases it itself. But
// a bot stops maintaining a branch once another identity has committed to it
// (a person clicking "Update branch", a stray merge-forward), and Renovate
// says so once ("will not automatically rebase this PR, because it does not
// recognize the last commit author"). On a base that requires up-to-date
// branches such a PR then sits behind forever: the hive will not sync it, the
// bot will not, and nobody is told.
//
// Once a dependency-bot PR has been behind or conflicting for the window
// (governor.dependency_bot_rebase_after_s, default 4h) and the bot has
// stopped rebasing it — its head commit is not the bot's, or its Renovate
// config rebases never — the sweep asks the bot to rebase through the bot's
// own mechanism, at most once per head (write-back kind bot_rebase_request):
// it ticks Renovate's rebase/retry box. If the head has not moved one window
// later, a person gets the PR through the ordinary hand-off (one comment
// plus needs-human, reason depBotHandoffStranded). It never asks when the
// branch carries a commit someone else wrote other than a merge of the base,
// because the bot's regeneration would drop it; that PR goes to a person
// instead.
//
// A Dependabot PR is never asked: Dependabot answers every comment command
// a GitHub App posts with "Sorry, only users with push access can use that
// command." (dependabot/dependabot-core#9147), so the `@dependabot recreate`
// the sweep used to post could not work. Where the sweep would have asked,
// it hands the PR to a person at once, and the comment says that someone
// with push access has to comment the command.
//
// The stale-red pass (stale_red_bot_rebase.go) asks through the same request
// for a bot branch that is red and behind when the base already passes its
// failing checks, and records it here with a Trigger. Such a record holds the
// other hand-offs for one window; if the head has not moved by then and is
// still red, a person gets the PR with reason depBotHandoffStaleRed.

// depBotHandoffStranded is the hand-off reason for a PR whose bot will not
// rebase it.
const depBotHandoffStranded = "bot-will-not-rebase"

// depBotStrandedReasonText is the plain-words reason shown with the hand-off.
const depBotStrandedReasonText = "dependency bot will not rebase this branch"

// Rebase request mechanisms, recorded in depBotRebaseState.Via.
const (
	depBotViaRenovateCheckbox = "renovate-checkbox"
	depBotViaAlreadyTicked    = "renovate-checkbox-already-ticked"
	// depBotViaDependabotLegacy is only read: older builds recorded it
	// after commenting `@dependabot recreate`, which Dependabot refuses
	// from an App. A record carrying it is handed to a person at once.
	depBotViaDependabotLegacy = "dependabot-recreate"
)

// depBotDependabotAuthor is the login Dependabot opens its PRs as.
const depBotDependabotAuthor = "dependabot[bot]"

// depBotDependabotNoAppText is the plain-words reason a stranded Dependabot
// PR goes to a person instead of to Dependabot.
const depBotDependabotNoAppText = "Hive cannot ask Dependabot to rebase it, because Dependabot does not accept commands from apps " +
	"(it answers \"only users with push access can use that command\")."

// depBotRebaseState is the persisted stranded-branch record for one PR head.
// StuckSince is when the head was first seen behind or conflicting (zero
// while it is not); RequestedAt is when the bot was asked to rebase this
// head (zero until then). A new head replaces the record.
//
// Trigger is set when the stale-red pass made the request
// (staleRedBotTrigger*), with BaseSHA the base tip it asked against and
// Checks the failing checks it asked about; it is empty for a request this
// sweep made for a stranded branch.
type depBotRebaseState struct {
	HeadSHA     string    `json:"head_sha"`
	StuckSince  time.Time `json:"stuck_since,omitzero"`
	RequestedAt time.Time `json:"requested_at,omitzero"`
	Via         string    `json:"via,omitempty"`
	Trigger     string    `json:"trigger,omitempty"`
	BaseSHA     string    `json:"base_sha,omitempty"`
	Checks      []string  `json:"checks,omitempty"`
}

// depBotBranchFacts is what the PR's commit list says about one head, read
// once per head (in memory only).
type depBotBranchFacts struct {
	HeadSHA string
	// ForeignHead: the head commit was not written by the bot.
	ForeignHead bool
	// ForeignWork: some commit on the branch is neither the bot's nor a
	// merge (a person's or a workflow's real change); regenerating the
	// branch would drop it.
	ForeignWork bool
}

// depBotStrandedStep is what the stranded check tells the hand-off loop.
type depBotStrandedStep int

const (
	// depBotStrandedNone: nothing to add; the existing hand-off reasons
	// decide as before.
	depBotStrandedNone depBotStrandedStep = iota
	// depBotStrandedHold: the bot has been (or will be) asked to rebase
	// this head; hold a conflict or red-CI hand-off so the PR does not get
	// a request and a person's comment at once.
	depBotStrandedHold
	// depBotStrandedHandoff: hand the PR to a person now with
	// depBotHandoffStranded.
	depBotStrandedHandoff
	// depBotStrandedStaleRedHandoff: the stale-red pass asked the bot to
	// rebase this head one window ago, the head has not moved and it is
	// still red; hand the PR to a person with depBotHandoffStaleRed.
	depBotStrandedStaleRedHandoff
)

// depBotCommitLister reads a PR's commits. *github.Client satisfies it.
type depBotCommitLister interface {
	ListPRCommits(ctx context.Context, repo string, number int) ([]github.PRCommit, error)
}

// depBotRebaseRequester is the write side of a rebase request.
// *github.Client satisfies it.
type depBotRebaseRequester interface {
	RequestRenovateRebase(ctx context.Context, repo string, number int, headSHA string) (github.RenovateRebaseCheckbox, error)
}

// depBotRebasePolicy is a reader that answers the bot_rebase_request policy
// itself instead of through the GitHub write-back gate (a fake in tests).
type depBotRebasePolicy interface {
	BotRebaseRequestAllowed(repo string, number int) bool
}

// dependencyBotBranchCandidate is dependencyBotHandoffCandidate widened to
// any github.DependencyBotBranch PR (a `renovate/` head ref under any
// account); only the stranded check looks at the wider set.
func dependencyBotBranchCandidate(pr github.PullRequest) bool {
	if pr.Draft || (pr.State != "" && !strings.EqualFold(pr.State, "open")) {
		return false
	}
	if !github.DependencyBotBranch(pr.HeadRef, pr.Author) {
		return false
	}
	return !escalation.HasNeedsHumanLabel(pr.Labels) && !github.HasHoldLabel(pr.Labels) && !hasLabel(pr.Labels, "do-not-merge")
}

func depBotBehind(pr github.PullRequest) bool {
	return strings.EqualFold(strings.TrimSpace(pr.MergeableState), "behind")
}

func depBotStuck(pr github.PullRequest) bool {
	return depBotBehind(pr) || conflictMergeState(pr)
}

// depBotMergeStateKnown reports that GitHub computed a merge state, so "not
// behind and not conflicting" is evidence rather than a gap in the data.
func depBotMergeStateKnown(pr github.PullRequest) bool {
	s := strings.ToLower(strings.TrimSpace(pr.MergeableState))
	return s != "" && s != "unknown"
}

// depBotStuckResolved reports that a stranded hand-off is over without a new
// head: GitHub reports the PR neither behind nor conflicting.
func depBotStuckResolved(pr github.PullRequest) bool {
	return depBotMergeStateKnown(pr) && !depBotStuck(pr)
}

// depBotCommitIsBots reports whether a commit author is the PR's bot: any
// dependency-bot login (a Renovate deployment may commit as renovate[bot]
// while its PRs are authored by another App), or the PR author itself.
func depBotCommitIsBots(pr github.PullRequest, author string) bool {
	a := strings.TrimSpace(author)
	return github.IsDependencyBotAuthor(a) || (a != "" && strings.EqualFold(a, strings.TrimSpace(pr.Author)))
}

// depBotFactsFromCommits derives the branch facts for pr's head. ok is false
// when the list does not end at pr.HeadSHA (the snapshot and the branch
// disagree; read again next pass).
func depBotFactsFromCommits(pr github.PullRequest, commits []github.PRCommit) (depBotBranchFacts, bool) {
	if len(commits) == 0 || !strings.EqualFold(commits[len(commits)-1].SHA, pr.HeadSHA) {
		return depBotBranchFacts{}, false
	}
	f := depBotBranchFacts{HeadSHA: pr.HeadSHA}
	for _, c := range commits {
		if depBotCommitIsBots(pr, c.Author) {
			continue
		}
		if c.ParentCount <= 1 {
			f.ForeignWork = true
		}
	}
	f.ForeignHead = !depBotCommitIsBots(pr, commits[len(commits)-1].Author)
	return f, true
}

// depBotRenovateBranch reports whether pr is a Renovate PR (any account).
func depBotRenovateBranch(pr github.PullRequest) bool {
	return pr.Author == renovateBotAuthor || strings.HasPrefix(strings.TrimSpace(pr.HeadRef), "renovate/")
}

// depBotRebaseCheck carries what the stranded check needs for one pass.
type depBotRebaseCheck struct {
	store  *depBotConflictStore
	reader depBotPRReader
	window time.Duration
	now    time.Time
	logger *slog.Logger
}

// requestPossible reports, without writing or recording a refusal, whether
// the sweep could ask pr's bot to rebase: the reader can request and the
// bot_rebase_request policy admits the PR.
func (c depBotRebaseCheck) requestPossible(repo string, pr github.PullRequest) bool {
	if _, ok := c.reader.(depBotRebaseRequester); !ok {
		return false
	}
	if _, ok := c.reader.(depBotCommitLister); !ok {
		return false
	}
	if p, ok := c.reader.(depBotRebasePolicy); ok {
		return p.BotRebaseRequestAllowed(repo, pr.Number)
	}
	if client, ok := c.reader.(*github.Client); ok {
		return client.WritebackGate().Decide(repo, client.WritebackItemForPR(pr), writeback.KindBotRebaseRequest).Allowed
	}
	return false
}

// requestAllowed is the gate check right before the write: it records a
// refusal like every other write-back.
func (c depBotRebaseCheck) requestAllowed(ctx context.Context, repo string, pr github.PullRequest, preview string) bool {
	if p, ok := c.reader.(depBotRebasePolicy); ok {
		return p.BotRebaseRequestAllowed(repo, pr.Number)
	}
	return depBotWritebackAllowed(ctx, c.reader, repo, pr.Number, writeback.KindBotRebaseRequest, preview, c.logger)
}

// facts returns the branch facts for pr's head, reading the commit list once
// per head.
func (c depBotRebaseCheck) facts(ctx context.Context, repo, key string, pr github.PullRequest) (depBotBranchFacts, bool) {
	if f, ok := c.store.branchFactsFor(key, pr.HeadSHA); ok {
		return f, true
	}
	lister, ok := c.reader.(depBotCommitLister)
	if !ok {
		return depBotBranchFacts{}, false
	}
	commits, err := lister.ListPRCommits(ctx, repo, pr.Number)
	if err != nil {
		c.logger.Warn("dependency-bot sweep: PR commit list failed; will retry next pass", "repo", repo, "pr", pr.Number, "error", err)
		return depBotBranchFacts{}, false
	}
	f, ok := depBotFactsFromCommits(pr, commits)
	if !ok {
		return depBotBranchFacts{}, false
	}
	c.store.rememberBranchFacts(key, f)
	return f, true
}

// rebasesNever reports whether a Renovate PR's description carries the
// `rebaseWhen: never` line, reusing the conflict sweep's per-head no-marker
// memo. ok is false when the description could not be read.
func (c depBotRebaseCheck) rebasesNever(ctx context.Context, repo, key string, pr github.PullRequest) (bool, bool) {
	if !depBotRenovateBranch(pr) || c.store.lacksMarker(key, pr.HeadSHA) {
		return false, true
	}
	body, err := c.reader.GetPRBody(ctx, repo, pr.Number)
	if err != nil {
		c.logger.Warn("dependency-bot sweep: PR body fetch failed; will retry next pass", "repo", repo, "pr", pr.Number, "error", err)
		return false, false
	}
	if renovateRebasesNever(body) {
		return true, true
	}
	c.store.rememberNoMarker(key, pr.HeadSHA)
	return false, true
}

// step is the stranded check for one hand-off candidate whose head has no
// terminal check hold.
func (c depBotRebaseCheck) step(ctx context.Context, repo, key string, pr github.PullRequest) depBotStrandedStep {
	if next, ok := c.staleRedStep(key, pr); ok {
		return next
	}
	if !depBotStuck(pr) {
		if depBotMergeStateKnown(pr) {
			c.store.clearStuck(key, pr.HeadSHA)
		}
		return depBotStrandedNone
	}
	st := c.store.noteStuck(key, pr.HeadSHA, c.now)
	if !st.RequestedAt.IsZero() {
		// Asked once on this head; never again. A new head resets it.
		if st.Via == depBotViaDependabotLegacy {
			// An older build's `@dependabot recreate`, which Dependabot
			// refused: nothing is coming, so a person gets it now.
			return depBotStrandedHandoff
		}
		if c.now.Sub(st.RequestedAt) < c.window {
			return depBotStrandedHold
		}
		return depBotStrandedHandoff
	}
	conflict := conflictMergeState(pr)
	possible := c.requestPossible(repo, pr)
	if !possible && conflict {
		// Report-only: a conflicting PR keeps the existing conflict path.
		return depBotStrandedNone
	}
	if c.now.Sub(st.StuckSince) < c.window {
		if possible && conflict {
			return depBotStrandedHold
		}
		return depBotStrandedNone
	}
	facts, ok := c.facts(ctx, repo, key, pr)
	if !ok {
		if possible {
			return depBotStrandedHold
		}
		return depBotStrandedNone
	}
	stranded := facts.ForeignHead
	if !stranded {
		never, ok := c.rebasesNever(ctx, repo, key, pr)
		if !ok {
			return depBotStrandedHold
		}
		stranded = never
	}
	if !stranded {
		// The bot's own head: it rebases on its own schedule.
		return depBotStrandedNone
	}
	if !possible {
		c.logger.Info("dependency-bot PR stranded behind its base; bot_rebase_request not allowed, handing to a person",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "reason", depBotStrandedReasonText)
		return depBotStrandedHandoff
	}
	if facts.ForeignWork {
		c.logger.Info("dependency-bot PR stranded with someone else's commits; not asking the bot to regenerate it",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA)
		return depBotStrandedHandoff
	}
	return c.request(ctx, repo, key, pr, conflict)
}

// staleRedStep is the stranded check for a head the stale-red pass asked the
// bot to rebase (a record with a Trigger for this head). GitHub may not call
// such a PR behind at all (it says "behind" only where the base requires
// up-to-date branches), so it does not wait for depBotStuck: the request
// holds the other hand-offs for one window, and then a head that is still red
// goes to a person. ok is false when there is no such record, or when the
// head is no longer red: the regular stranded check then decides.
func (c depBotRebaseCheck) staleRedStep(key string, pr github.PullRequest) (depBotStrandedStep, bool) {
	st, found := c.store.rebaseState(key)
	if !found || st.Trigger == "" || st.RequestedAt.IsZero() || !strings.EqualFold(st.HeadSHA, pr.HeadSHA) {
		return depBotStrandedNone, false
	}
	if c.now.Sub(st.RequestedAt) < c.window {
		return depBotStrandedHold, true
	}
	if !strings.EqualFold(strings.TrimSpace(pr.CIStatus), "failure") {
		// The red cleared on this head (a re-run passed): no stale-red
		// hand-off; the regular stranded check decides for a head that is
		// still behind or conflicting.
		return depBotStrandedNone, false
	}
	return depBotStrandedStaleRedHandoff, true
}

// depBotAsk is what one rebase request through the bot's own mechanism did.
type depBotAsk int

const (
	// depBotAskRefused: the write-back gate (bot_rebase_request) refused,
	// or the reader cannot make the request; nothing was written.
	depBotAskRefused depBotAsk = iota
	// depBotAskRetry: the head moved under the request, or the request
	// failed; try again next pass.
	depBotAskRetry
	// depBotAskNoCheckbox: the PR description has no rebase/retry box; the
	// bot offers no way to ask.
	depBotAskNoCheckbox
	// depBotAskTicked: the box was ticked (depBotViaRenovateCheckbox).
	depBotAskTicked
	// depBotAskAlreadyTicked: someone already ticked the box
	// (depBotViaAlreadyTicked); nothing was written.
	depBotAskAlreadyTicked
)

// askRenovate is the one rebase request both callers share: the write-back
// gate (which records a refusal), then Renovate's rebase/retry box, pinned
// to pr's head. preview is what the gate records.
func (c depBotRebaseCheck) askRenovate(ctx context.Context, repo string, pr github.PullRequest, preview string) depBotAsk {
	requester, ok := c.reader.(depBotRebaseRequester)
	if !ok {
		return depBotAskRefused
	}
	if !c.requestAllowed(ctx, repo, pr, preview) {
		return depBotAskRefused
	}
	state, err := requester.RequestRenovateRebase(ctx, repo, pr.Number, pr.HeadSHA)
	switch {
	case errors.Is(err, github.ErrDependencyBotHeadMoved):
		return depBotAskRetry
	case err != nil:
		c.logger.Warn("dependency-bot rebase request failed; will retry next pass", "repo", repo, "pr", pr.Number, "error", err)
		return depBotAskRetry
	}
	switch state {
	case github.RenovateRebaseCheckboxUnticked:
		return depBotAskTicked
	case github.RenovateRebaseCheckboxTicked:
		return depBotAskAlreadyTicked
	}
	return depBotAskNoCheckbox
}

// request asks the bot to rebase pr's head once.
func (c depBotRebaseCheck) request(ctx context.Context, repo, key string, pr github.PullRequest, conflict bool) depBotStrandedStep {
	refused := func() depBotStrandedStep {
		if conflict {
			return depBotStrandedNone
		}
		return depBotStrandedHandoff
	}
	if strings.EqualFold(pr.Author, depBotDependabotAuthor) {
		// Dependabot refuses comment commands from a GitHub App
		// (dependabot/dependabot-core#9147): hand the PR to a person, who
		// can comment the command, instead of posting one that cannot work.
		c.logger.Info("stranded Dependabot PR handed to a person; Dependabot does not accept commands from apps",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA)
		return depBotStrandedHandoff
	}
	switch c.askRenovate(ctx, repo, pr, "tick the rebase/retry checkbox") {
	case depBotAskRefused:
		return refused()
	case depBotAskRetry:
		return depBotStrandedHold
	case depBotAskTicked:
		c.store.markRebaseRequested(key, pr.HeadSHA, depBotViaRenovateCheckbox, c.now)
		c.logger.Info("ticked Renovate's rebase/retry box on a stranded PR", "repo", repo, "pr", pr.Number, "head", pr.HeadSHA)
		return depBotStrandedHold
	case depBotAskAlreadyTicked:
		// Someone already asked; give the bot one window from now.
		c.store.markRebaseRequested(key, pr.HeadSHA, depBotViaAlreadyTicked, c.now)
		return depBotStrandedHold
	}
	// No checkbox: the bot offers no way to ask.
	return depBotStrandedHandoff
}

// dependencyBotStrandedComment is the hand-off comment for
// depBotHandoffStranded.
func dependencyBotStrandedComment(pr github.PullRequest, st depBotRebaseState, facts depBotBranchFacts, factsKnown bool, window time.Duration) string {
	name := dependencyBotDisplayName(pr.Author)
	state := "has been behind " + depBotBaseTarget(pr.BaseRef)
	if conflictMergeState(pr) {
		state = "has conflicted with " + depBotBaseTarget(pr.BaseRef)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The %s: this %s PR %s for over %s", depBotStrandedReasonText, name, state, depBotHours(window))
	if factsKnown && facts.ForeignHead {
		b.WriteString(", and its last commit is not the bot's, so the bot stopped updating the branch")
	}
	b.WriteString(". ")
	fix := "rebase the branch by hand, or close the PR so the bot can open a fresh one."
	dependabot := strings.EqualFold(pr.Author, depBotDependabotAuthor)
	if dependabot {
		fix = "comment `" + github.DependabotRecreateCommand + "` on this PR (it discards any commits on the branch that are not Dependabot's), or rebase the branch by hand."
	} else if depBotRenovateBranch(pr) || st.Via == depBotViaRenovateCheckbox || st.Via == depBotViaAlreadyTicked {
		fix = "tick 'If you want to rebase/retry this PR, check this box' in the description if it has one (Renovate regenerates the branch and discards commits that are not its own), or rebase the branch by hand."
	}
	switch {
	case st.Via == depBotViaRenovateCheckbox:
		fmt.Fprintf(&b, "Hive ticked the rebase/retry box %s ago and the branch has not moved since. ", depBotHours(window))
		fix = "check the bot's logs or dependency dashboard for why it did not regenerate the branch, rebase it by hand, or close the PR so the bot can open a fresh one."
	case st.Via == depBotViaAlreadyTicked:
		fmt.Fprintf(&b, "The rebase/retry box has been ticked for over %s and the branch has not moved. ", depBotHours(window))
		fix = "check the bot's logs or dependency dashboard for why it did not regenerate the branch, rebase it by hand, or close the PR so the bot can open a fresh one."
	case factsKnown && facts.ForeignWork:
		b.WriteString("Hive did not ask the bot to regenerate it, because that would drop commits someone else pushed to the branch. ")
		fix = "rebase the branch by hand keeping those commits, or, if they may go, " + fix
	case dependabot:
		// Also covers a record an older build wrote after commenting the
		// command itself (depBotViaDependabotLegacy): Dependabot refused it.
		b.WriteString(depBotDependabotNoAppText + " ")
		fix = "someone with push access comments `" + github.DependabotRecreateCommand + "` on this PR (it discards any commits on the branch that are not Dependabot's), or rebases the branch by hand."
	}
	b.WriteString("To fix it, " + fix)
	return b.String()
}

// ---- store ----------------------------------------------------------------

func (s *depBotConflictStore) noteStuck(key, headSHA string, now time.Time) depBotRebaseState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rebase == nil {
		s.rebase = map[string]*depBotRebaseState{}
	}
	st := s.rebase[key]
	switch {
	case st == nil || !strings.EqualFold(st.HeadSHA, headSHA):
		st = &depBotRebaseState{HeadSHA: headSHA, StuckSince: now}
		s.rebase[key] = st
		s.saveRebaseLocked()
	case st.StuckSince.IsZero():
		st.StuckSince = now
		s.saveRebaseLocked()
	}
	return *st
}

// clearStuck records that the PR is neither behind nor conflicting: a new
// head forgets the record, the same head only stops its stuck clock (a
// request already made on it still stands).
func (s *depBotConflictStore) clearStuck(key, headSHA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.rebase[key]
	switch {
	case st == nil:
		return
	case !strings.EqualFold(st.HeadSHA, headSHA):
		delete(s.rebase, key)
	case !st.StuckSince.IsZero():
		st.StuckSince = time.Time{}
	default:
		return
	}
	s.saveRebaseLocked()
}

func (s *depBotConflictStore) markRebaseRequested(key, headSHA, via string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rebase == nil {
		s.rebase = map[string]*depBotRebaseState{}
	}
	st := s.rebase[key]
	if st == nil || !strings.EqualFold(st.HeadSHA, headSHA) {
		st = &depBotRebaseState{HeadSHA: headSHA, StuckSince: at}
		s.rebase[key] = st
	}
	st.RequestedAt, st.Via = at, via
	s.saveRebaseLocked()
}

// markStaleRedRebaseRequested records a stale-red request on headSHA
// against baseSHA. A repeat on the same head (a later base tip) keeps the
// first request's time and mechanism, so the hand-off still comes one window
// after the bot was first asked about this head. It also notes the request
// per PR (staleRedLast), which outlives the head, so the bot's next head red
// on the same checks is not asked about again.
func (s *depBotConflictStore) markStaleRedRebaseRequested(key, headSHA, baseSHA, trigger, via string, checks []string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rebase == nil {
		s.rebase = map[string]*depBotRebaseState{}
	}
	st := s.rebase[key]
	if st == nil || !strings.EqualFold(st.HeadSHA, headSHA) {
		st = &depBotRebaseState{HeadSHA: headSHA}
		s.rebase[key] = st
	}
	if st.RequestedAt.IsZero() {
		st.RequestedAt, st.Via = at, via
	}
	st.Trigger, st.BaseSHA = trigger, baseSHA
	st.Checks = append([]string(nil), checks...)
	s.saveRebaseLocked()
	if s.staleRedLast == nil {
		s.staleRedLast = map[string]*depBotStaleRedAsk{}
	}
	s.staleRedLast[key] = &depBotStaleRedAsk{HeadSHA: headSHA, Trigger: trigger, Checks: append([]string(nil), checks...), At: at}
	s.saveStaleRedLocked()
}

func (s *depBotConflictStore) rebaseState(key string) (depBotRebaseState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.rebase[key]
	if st == nil {
		return depBotRebaseState{}, false
	}
	return *st, true
}

// pruneRebase forgets PRs that left the open snapshot.
func (s *depBotConflictStore) pruneRebase(open map[string]github.PullRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k := range s.rebase {
		if _, ok := open[k]; !ok {
			delete(s.rebase, k)
			changed = true
		}
	}
	for k := range s.branchFacts {
		if _, ok := open[k]; !ok {
			delete(s.branchFacts, k)
		}
	}
	for k := range s.staleRedNotOwnLabel {
		if _, ok := open[k]; !ok {
			delete(s.staleRedNotOwnLabel, k)
		}
	}
	if changed {
		s.saveRebaseLocked()
	}
	staleRedChanged := false
	for k := range s.staleRedLast {
		if _, ok := open[k]; !ok {
			delete(s.staleRedLast, k)
			staleRedChanged = true
		}
	}
	if staleRedChanged {
		s.saveStaleRedLocked()
	}
}

func (s *depBotConflictStore) branchFactsFor(key, headSHA string) (depBotBranchFacts, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.branchFacts[key]
	return f, ok && strings.EqualFold(f.HeadSHA, headSHA)
}

func (s *depBotConflictStore) rememberBranchFacts(key string, f depBotBranchFacts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.branchFacts == nil {
		s.branchFacts = map[string]depBotBranchFacts{}
	}
	s.branchFacts[key] = f
}

func depBotRebasePath(path string) string {
	if path == "" {
		return ""
	}
	return strings.TrimSuffix(path, ".json") + "-rebase.json"
}

func loadDepBotRebaseStates(path string) map[string]*depBotRebaseState {
	out := map[string]*depBotRebaseState{}
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var m map[string]*depBotRebaseState
	if json.Unmarshal(data, &m) == nil && m != nil {
		out = m
	}
	return out
}

func (s *depBotConflictStore) saveRebaseLocked() {
	path := depBotRebasePath(s.path)
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(s.rebase, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}
