package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Stale-red refresh on a dependency bot's branch. The hive never merges the
// base into a bot's branch: Renovate stops rebasing a branch once another
// identity has committed to it (github.DependencyBotBranch). Renovate with
// `rebaseWhen: "conflicted"` does not rebase a branch that is merely behind
// either, so a bot PR whose check went red against an older base stayed red
// for good: projectbluefin/actions#775 sat at 070 for 33 hours with `pytest`
// red on its head and green on main since two days, and people kept merging
// main into Renovate branches by hand, which stopped Renovate maintaining
// them.
//
// Where the stale-red pass would merge the base in, it asks the bot to
// rebase instead, through the dependency-bot sweep's own request
// (depBotRebaseCheck.askRenovate: Renovate's rebase/retry box, write-back
// kind bot_rebase_request). The bot's rebase is a new head, so every check
// runs again on the current base:
//
//   - every failing check is green on the base tip and the head is behind
//     it: at most once per (PR, head, base tip);
//   - a failing check the base tip never runs (a PR-only check) whose
//     failing run is older than the window
//     (governor.dependency_bot_rebase_after_s, 4 hours), on a head behind a
//     base tip committed after that run: at most once per head.
//
// Each request makes a new head, so neither key bounds a bump that really
// breaks a check. The last request per PR is kept across heads
// (depBotStaleRedAsk): the bot's next head failing any of the same checks is
// not asked about again, nothing holds it, and the sweep's red-CI hand-off
// runs on its usual grace. A green head clears the note. Such a bump costs
// one extra CI run. The pass never asks over a commit that is not the bot's
// (the regeneration drops it), and decides whether the write-back gate would
// allow the request before any API call.
//
// A bot PR the dependency-bot sweep's own red-CI hand-off parked with
// needs-human (red on the same head past depBotRedCIGrace) is still asked
// about, for the first trigger only: once every failing check is green on
// the base tip, the bot's rebase is the fresh run that hand-off asks a
// person for. The sweep's ledger record proves the label is the hand-off's
// (staleRedOwnRedCIHandoff): reason red-ci, recorded for the current head,
// and the label marked as added by the sweep. Right before the request the
// pass also reads the PR's label events once: the newest needs-human event
// must be the App's own `labeled`, so a person who took the label off and
// put it back is not overruled (a failed read counts as a person's). A
// needs-human a person added, one from any other hand-off, or one recorded
// for an older head still blocks, as do hold and do-not-merge. Such a PR is
// asked about at most once per head, whatever the base does next: the
// sweep skips a PR labelled needs-human, so its stale-red-bot-did-not-rebase
// hand-off never comes, and nothing else would end the requests. The PR
// keeps the hand-off's label and comment, and no new hand-off is posted.
// The label comes off when CI goes green, as before; when the bot's new
// head fails again it stays, and the record names the old head, so nothing
// is asked again.
//
// Requests count against the pass's caps (staleRedRefreshMaxPerRepo,
// staleRedRefreshMaxPerPass). Dependabot is never asked, as in the
// dependency-bot sweep: it refuses commands from apps, and its branch is
// skipped as before. Each request is recorded in the sweep's stranded-branch
// record (depBotRebaseState with a Trigger), so a head that has neither moved
// nor gone green one window later is handed to a person through the sweep's
// ordinary hand-off (depBotHandoffStaleRed).

// Triggers recorded in depBotRebaseState.Trigger.
const (
	// staleRedBotTriggerBaseClears: every failing check is green on the
	// base tip.
	staleRedBotTriggerBaseClears = "stale-red"
	// staleRedBotTriggerPROnly: a failing check the base tip never runs,
	// with the base moved since that check failed.
	staleRedBotTriggerPROnly = "pr-only-red"
)

// staleRedBotRebaser is what the pass needs beyond staleRedRefresher to ask
// a bot to rebase: the dependency-bot sweep's reader, commit list and
// request, a read-only compare, and a commit's date. *github.Client
// implements it.
type staleRedBotRebaser interface {
	depBotPRReader
	depBotCommitLister
	depBotRebaseRequester
	StaleRedBehindBy(ctx context.Context, repo, baseSHA, headSHA string) (int, error)
	CommitCommittedAt(ctx context.Context, repo, sha string) (time.Time, error)
}

var _ staleRedBotRebaser = (*github.Client)(nil)

// staleRedLabelActorReader reads whether the newest needs-human label event
// on a PR is the App's own `labeled`. *github.Client implements it; a
// rebaser without it never asks over the sweep's red-CI label.
type staleRedLabelActorReader interface {
	StaleRedNeedsHumanLabeledByApp(ctx context.Context, repo string, number int) (bool, error)
}

var _ staleRedLabelActorReader = (*github.Client)(nil)

// staleRedDepBotStore is the dependency-bot sweep's ledger; tests replace it.
var staleRedDepBotStore = getDepBotConflictStore

// staleRedBotOutcome is what the pass did for one bot PR.
type staleRedBotOutcome struct {
	// Asked: the rebase/retry box was ticked (counts against the caps).
	Asked bool
	// Settled: nothing more for this (head, base tip).
	Settled  bool
	Trigger  string
	Via      string
	Checks   []string
	BehindBy int
}

// staleRedBotPass carries one pass's bot requests.
type staleRedBotPass struct {
	rebaser staleRedBotRebaser
	check   depBotRebaseCheck
	logger  *slog.Logger
	tipAt   map[string]time.Time
}

func newStaleRedBotPass(cfg *config.Config, refresher staleRedRefresher, logger *slog.Logger, now time.Time) *staleRedBotPass {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	p := &staleRedBotPass{logger: logger, tipAt: map[string]time.Time{}}
	rebaser, ok := refresher.(staleRedBotRebaser)
	if !ok {
		return p
	}
	p.rebaser = rebaser
	p.check = depBotRebaseCheck{store: staleRedDepBotStore(), reader: rebaser, window: cfg.DependencyBotRebaseAfter(), now: now, logger: logger}
	return p
}

// staleRedBotRebaseCandidate reports whether the pass may ask pr's bot to
// rebase: a dependency-bot branch the dependency-bot sweep watches (open,
// not a draft, not parked with needs-human, hold or do-not-merge), not
// Dependabot's (it refuses commands from apps) and not adopted by the hive.
func staleRedBotRebaseCandidate(pr github.PullRequest) bool {
	return dependencyBotBranchCandidate(pr) &&
		!strings.EqualFold(strings.TrimSpace(pr.Author), depBotDependabotAuthor) &&
		!hasLabel(pr.Labels, stagelabels.AdoptedByHive.Label())
}

// staleRedOwnRedCIHandoff reports whether pr's needs-human is the one the
// dependency-bot sweep's red-CI hand-off added for pr's current head, as
// the sweep's own ledger record for key says: reason depBotHandoffRedCI,
// recorded for this exact head, and LabelApplied (the sweep's own label
// write succeeded). The sweep only hands off a PR that has no needs-human
// yet (dependencyBotHandoffCandidate), so such a record is not a label a
// person added before it. Anything else, including no record, a failed
// label write or a record for an older head, is false.
func staleRedOwnRedCIHandoff(store *depBotConflictStore, key string, pr github.PullRequest) bool {
	if store == nil || strings.TrimSpace(pr.HeadSHA) == "" || !escalation.HasNeedsHumanLabel(pr.Labels) {
		return false
	}
	rec, ok := store.get(key)
	return ok && rec.Reason == depBotHandoffRedCI && rec.LabelApplied && strings.EqualFold(rec.HeadSHA, pr.HeadSHA)
}

// staleRedWithoutNeedsHuman is pr with needs-human dropped from a copy of
// its labels.
func staleRedWithoutNeedsHuman(pr github.PullRequest) github.PullRequest {
	labels := make([]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		if !strings.EqualFold(l, escalation.NeedsHumanLabel) {
			labels = append(labels, l)
		}
	}
	pr.Labels = labels
	return pr
}

// parkedByOwnRedCIHandoff reports whether pr would be a candidate but for
// the needs-human the dependency-bot sweep's own red-CI hand-off added on
// this head (staleRedOwnRedCIHandoff). Such a PR is asked about only when
// the base clears every failing check (consider).
// A head whose label events already showed a person's needs-human (or
// could not be read) is not one (staleRedLabelNotOwn), so it costs no
// further API call.
func (p *staleRedBotPass) parkedByOwnRedCIHandoff(repo string, pr github.PullRequest) bool {
	if p.rebaser == nil {
		return false
	}
	key := escalation.Key(repo, pr.Number)
	return staleRedOwnRedCIHandoff(p.check.store, key, pr) &&
		!p.check.store.staleRedLabelNotOwn(key, pr.HeadSHA) &&
		staleRedBotRebaseCandidate(staleRedWithoutNeedsHuman(pr))
}

// labelIsOwn reads pr's label events once, right before a request on a PR
// parked by the sweep's red-CI hand-off, and reports whether the newest
// needs-human event is the App's own `labeled`. A failed read, a rebaser
// that cannot read them, or another actor fails closed and is remembered
// for this head (in memory), so the head is not read or asked about again.
func (p *staleRedBotPass) labelIsOwn(ctx context.Context, repo, key string, pr github.PullRequest) bool {
	reader, ok := p.rebaser.(staleRedLabelActorReader)
	if !ok {
		p.check.store.rememberStaleRedLabelNotOwn(key, pr.HeadSHA)
		return false
	}
	own, err := reader.StaleRedNeedsHumanLabeledByApp(ctx, repo, pr.Number)
	if err != nil || !own {
		p.logger.Info("stale-red refresh: needs-human on this dependency-bot PR was not last added by Hive, or its label events could not be read; not asking the bot to rebase",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "error", err)
		p.check.store.rememberStaleRedLabelNotOwn(key, pr.HeadSHA)
		return false
	}
	return true
}

// possible reports, without an API call, whether the pass could ask pr's
// bot to rebase: a candidate (or one parked only by the sweep's own red-CI
// hand-off on this head), and bot_rebase_request admits it
// (depBotRebaseCheck.requestPossible: the gate's decision, nothing
// recorded). The loop asks it before reading the base tip, so a hive where
// the gate would refuse spends nothing on bot PRs.
func (p *staleRedBotPass) possible(repo string, pr github.PullRequest) bool {
	if p.rebaser == nil || (!staleRedBotRebaseCandidate(pr) && !p.parkedByOwnRedCIHandoff(repo, pr)) {
		return false
	}
	if p.check.requestPossible(repo, pr) {
		return true
	}
	p.logger.Debug("stale-red refresh: bot_rebase_request not allowed on this dependency-bot PR; not asking the bot to rebase",
		"repo", repo, "pr", pr.Number, "head", pr.HeadSHA)
	return false
}

// noteGreen forgets a bot PR's last stale-red request once its CI is green,
// so a later red is judged as a new failure.
func (p *staleRedBotPass) noteGreen(repo string, pr github.PullRequest) {
	if p.rebaser == nil {
		return
	}
	p.check.store.clearStaleRedAsk(escalation.Key(repo, pr.Number))
}

// consider handles one bot PR the pass reached with the base tip read.
// verdict is StaleRedBaseVerdictFor's.
func (p *staleRedBotPass) consider(ctx context.Context, repo string, pr github.PullRequest, base github.StaleRedBaseState, verdict github.StaleRedBaseVerdict) staleRedBotOutcome {
	if !p.possible(repo, pr) {
		// Today's handling: the branch is the bot's, and nothing is asked.
		return staleRedBotOutcome{Settled: true}
	}
	key := escalation.Key(repo, pr.Number)
	st, found := p.check.store.rebaseState(key)
	askedHead := found && !st.RequestedAt.IsZero() && strings.EqualFold(st.HeadSHA, pr.HeadSHA)

	// possible admitted pr, so a PR that is not a plain candidate is parked
	// by the sweep's own red-CI hand-off on this head.
	parked := !staleRedBotRebaseCandidate(pr) && p.parkedByOwnRedCIHandoff(repo, pr)
	if parked && verdict != github.StaleRedBaseClears {
		// Asked about only once the base tip clears every failing check.
		// Read again next pass.
		return staleRedBotOutcome{}
	}
	if parked && askedHead {
		// At most once per head, whatever the base does next: the sweep
		// skips a labelled PR, so no stale-red-bot-did-not-rebase hand-off
		// would end the requests. The request is in the persisted ledger,
		// so a restart does not ask again.
		return staleRedBotOutcome{Settled: true}
	}

	out := staleRedBotOutcome{}
	switch verdict {
	case github.StaleRedBaseClears:
		// Once per (head, base tip). A request the sweep made for a
		// stranded branch on this head, or a PR-only request (once per
		// head), is never followed by a second one on the same head.
		if askedHead && (st.Trigger != staleRedBotTriggerBaseClears || strings.EqualFold(st.BaseSHA, base.SHA)) {
			return staleRedBotOutcome{Settled: true}
		}
		out.Trigger, out.Checks = staleRedBotTriggerBaseClears, pr.FailingChecks
	case github.StaleRedBaseUnknown:
		prOnly, ok := staleRedPROnlyChecks(pr.FailingChecks, base)
		if !ok {
			// A failing check still running on the base tip: read again
			// next pass.
			return staleRedBotOutcome{}
		}
		if askedHead {
			// At most once per head, whatever the base does next.
			return staleRedBotOutcome{Settled: true}
		}
		ranAt, known := staleRedLatestFailingRun(pr, prOnly)
		if !known || p.check.now.Sub(ranAt) < p.check.window {
			// Unknown run time, or a run younger than the window: wait.
			return staleRedBotOutcome{}
		}
		tipAt, err := p.tipCommittedAt(ctx, repo, base.SHA)
		if err != nil {
			p.logger.Warn("stale-red refresh: could not read the base tip's commit date; retrying next pass", "repo", repo, "pr", pr.Number, "base_sha", base.SHA, "error", err)
			return staleRedBotOutcome{}
		}
		if !tipAt.After(ranAt) {
			// The base has not moved since the check failed: a rebase
			// would run it against the same base.
			return staleRedBotOutcome{Settled: true}
		}
		out.Trigger, out.Checks = staleRedBotTriggerPROnly, prOnly
	default:
		return staleRedBotOutcome{}
	}

	// The bot already rebased this PR once for these checks and its new
	// head fails them again: the bump breaks them, and another rebase only
	// runs CI again. Leave it to the red-CI hand-off, which no request holds
	// on this head.
	if last, ok := p.check.store.staleRedAsk(key); ok && !strings.EqualFold(last.HeadSHA, pr.HeadSHA) && staleRedChecksOverlap(last.Checks, out.Checks) {
		p.logger.Info("stale-red refresh: the bot's new head fails the same checks after a rebase request; not asking again",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "asked_head", last.HeadSHA, "failing_checks", out.Checks)
		return staleRedBotOutcome{Settled: true}
	}

	behind, err := p.rebaser.StaleRedBehindBy(ctx, repo, base.SHA, pr.HeadSHA)
	if err != nil {
		p.logger.Warn("stale-red refresh: compare failed on a dependency-bot branch; retrying next pass", "repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "base_sha", base.SHA, "error", err)
		return staleRedBotOutcome{}
	}
	out.BehindBy = behind
	if behind <= 0 {
		out.Settled = true
		return out
	}

	// Ticking the box regenerates the branch and drops every commit that is
	// not the bot's. Never ask over a person's commit (the dependency-bot
	// sweep's own rule), nor when the branch cannot be read: the stranded
	// or red-CI hand-off takes such a PR to a person.
	facts, known := p.check.facts(ctx, repo, key, pr)
	if !known || facts.ForeignWork {
		p.logger.Info("stale-red refresh: dependency-bot branch carries someone else's commits, or its commits could not be read; not asking the bot to regenerate it",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "facts_known", known)
		out.Settled = true
		return out
	}

	// The last check before the request, so the label events are read only
	// when the bot would otherwise be asked.
	if parked && !p.labelIsOwn(ctx, repo, key, pr) {
		out.Settled = true
		return out
	}

	preview := fmt.Sprintf("tick the rebase/retry checkbox on %s#%d at %s (red on %s, behind %s@%s)",
		repo, pr.Number, stagelabels.ShortSHA(pr.HeadSHA), strings.Join(out.Checks, ","), pr.BaseRef, stagelabels.ShortSHA(base.SHA))
	switch p.check.askRenovate(ctx, repo, pr, preview) {
	case depBotAskRefused:
		p.logger.Info("stale-red refresh: bot_rebase_request refused by the write-back gate; not asking the dependency bot to rebase",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "base_sha", base.SHA, "trigger", out.Trigger)
		out.Settled = true
	case depBotAskRetry:
		// Head moved or the request failed: judged again next pass.
	case depBotAskNoCheckbox:
		p.logger.Info("stale-red refresh: dependency-bot PR has no rebase/retry box; nothing to ask",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "base_sha", base.SHA)
		out.Settled = true
	case depBotAskTicked:
		out.Asked, out.Settled, out.Via = true, true, depBotViaRenovateCheckbox
		p.check.store.markStaleRedRebaseRequested(key, pr.HeadSHA, base.SHA, out.Trigger, out.Via, out.Checks, p.check.now)
	case depBotAskAlreadyTicked:
		// Someone (or an earlier request) already asked; nothing written,
		// but the hand-off clock starts like any request.
		out.Settled, out.Via = true, depBotViaAlreadyTicked
		p.check.store.markStaleRedRebaseRequested(key, pr.HeadSHA, base.SHA, out.Trigger, out.Via, out.Checks, p.check.now)
		p.logger.Info("stale-red refresh: dependency-bot PR's rebase/retry box is already ticked",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "base_sha", base.SHA, "trigger", out.Trigger)
	}
	return out
}

func (p *staleRedBotPass) tipCommittedAt(ctx context.Context, repo, sha string) (time.Time, error) {
	k := strings.ToLower(repo) + "@" + sha
	if at, ok := p.tipAt[k]; ok {
		return at, nil
	}
	at, err := p.rebaser.CommitCommittedAt(ctx, repo, sha)
	if err != nil {
		return time.Time{}, err
	}
	p.tipAt[k] = at
	return at, nil
}

// staleRedPROnlyChecks returns the failing checks the base tip has no run
// for at all (PR-only checks). ok is false when there is none, or when some
// failing check is red on the base tip or has a run there that has not
// completed: then the base says nothing yet. Every other failing check is
// green on the base tip.
func staleRedPROnlyChecks(failing []string, base github.StaleRedBaseState) ([]string, bool) {
	var prOnly []string
	for _, name := range failing {
		switch {
		case base.Red[name]:
			return nil, false
		case base.Completed[name]:
		case base.Reported[name]:
			return nil, false
		default:
			prOnly = append(prOnly, name)
		}
	}
	return prOnly, len(prOnly) > 0
}

// staleRedLatestFailingRun is when the newest failing run of names on pr's
// head completed, from the CI activity the enumeration already read. known
// is false when any of them has no completed failing run with a time.
func staleRedLatestFailingRun(pr github.PullRequest, names []string) (time.Time, bool) {
	a := pr.CIActivity
	if a == nil || (a.Head != "" && !strings.EqualFold(a.Head, pr.HeadSHA)) {
		return time.Time{}, false
	}
	var latest time.Time
	for _, name := range names {
		var at time.Time
		for _, c := range a.Checks {
			if c.Name != name || c.Since == nil || (c.Head != "" && !strings.EqualFold(c.Head, pr.HeadSHA)) {
				continue
			}
			if !strings.EqualFold(c.State, "completed") || (c.Conclusion != "failure" && c.Conclusion != "action_required") {
				continue
			}
			if c.Since.After(at) {
				at = *c.Since
			}
		}
		if at.IsZero() {
			return time.Time{}, false
		}
		if at.After(latest) {
			latest = at
		}
	}
	return latest, !latest.IsZero()
}

// dependencyBotStaleRedComment is the hand-off comment for
// depBotHandoffStaleRed.
func dependencyBotStaleRedComment(pr github.PullRequest, st depBotRebaseState, window time.Duration) string {
	name := dependencyBotDisplayName(pr.Author)
	base := depBotBaseTarget(pr.BaseRef)
	checks := depBotQuotedChecks(st.Checks)
	if checks == "" {
		checks = depBotQuotedChecks(pr.FailingChecks)
	}
	if checks == "" {
		checks = "a check"
	}
	var b strings.Builder
	if st.Trigger == staleRedBotTriggerPROnly {
		fmt.Fprintf(&b, "This %s PR is red on %s, which runs only on pull requests, and %s has moved since it failed. ", name, checks, base)
	} else {
		fmt.Fprintf(&b, "This %s PR is red on %s, which passes on %s now, and its branch is behind %s. ", name, checks, base, base)
	}
	if st.Via == depBotViaAlreadyTicked {
		fmt.Fprintf(&b, "The rebase/retry box has been ticked for over %s and the branch has not moved, so the checks have not run again. ", depBotHours(window))
	} else {
		fmt.Fprintf(&b, "Hive ticked the rebase/retry box %s ago so the bot would rebase the branch and run the checks again, and the branch has not moved since. ", depBotHours(window))
	}
	b.WriteString("To fix it, check the bot's logs or dependency dashboard for why it did not regenerate the branch, " +
		"rebase it by hand (the bot then stops updating the branch), or close the PR so the bot can open a fresh one.")
	return b.String()
}

// depBotQuotedChecks is up to five check names in backticks, "" for none.
func depBotQuotedChecks(names []string) string {
	if len(names) == 0 {
		return ""
	}
	more := ""
	if len(names) > 5 {
		more = fmt.Sprintf(" and %d more", len(names)-5)
		names = names[:5]
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + n + "`"
	}
	return strings.Join(quoted, ", ") + more
}

// staleRedBotAuditReason is the audit line's reason for a trigger.
func staleRedBotAuditReason(trigger string) string {
	if trigger == staleRedBotTriggerPROnly {
		return "pr-only check failed before the base moved"
	}
	return "failing checks green on the base tip"
}

// depBotStaleRedAsk is the last stale-red rebase request on a PR: the head
// it asked about, why, and the checks that were red. Unlike
// depBotRebaseState it is kept when the bot pushes a new head, so a bump
// that really breaks a check is asked about once, not once per base move.
// A green head clears it (clearStaleRedAsk).
type depBotStaleRedAsk struct {
	HeadSHA string    `json:"head_sha"`
	Trigger string    `json:"trigger,omitempty"`
	Checks  []string  `json:"checks,omitempty"`
	At      time.Time `json:"at"`
}

func (s *depBotConflictStore) staleRedAsk(key string) (depBotStaleRedAsk, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.staleRedLast[key]
	if a == nil {
		return depBotStaleRedAsk{}, false
	}
	return *a, true
}

// staleRedLabelNotOwn reports whether headSHA's needs-human was found not
// to be the App's own (or unreadable) on an earlier pass.
func (s *depBotConflictStore) staleRedLabelNotOwn(key, headSHA string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sha, ok := s.staleRedNotOwnLabel[key]
	return ok && strings.EqualFold(sha, headSHA)
}

func (s *depBotConflictStore) rememberStaleRedLabelNotOwn(key, headSHA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.staleRedNotOwnLabel == nil {
		s.staleRedNotOwnLabel = map[string]string{}
	}
	s.staleRedNotOwnLabel[key] = headSHA
}

// clearStaleRedAsk forgets the PR's last stale-red request: its CI went
// green, so a later red is a new failure.
func (s *depBotConflictStore) clearStaleRedAsk(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.staleRedLast[key]; !ok {
		return
	}
	delete(s.staleRedLast, key)
	s.saveStaleRedLocked()
}

func depBotStaleRedPath(path string) string {
	if path == "" {
		return ""
	}
	return strings.TrimSuffix(path, ".json") + "-stale-red.json"
}

func loadDepBotStaleRedAsks(path string) map[string]*depBotStaleRedAsk {
	out := map[string]*depBotStaleRedAsk{}
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var m map[string]*depBotStaleRedAsk
	if json.Unmarshal(data, &m) == nil && m != nil {
		out = m
	}
	return out
}

func (s *depBotConflictStore) saveStaleRedLocked() {
	path := depBotStaleRedPath(s.path)
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(s.staleRedLast, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

// staleRedChecksOverlap reports whether any of now was red at the last ask.
func staleRedChecksOverlap(last, now []string) bool {
	for _, a := range last {
		for _, b := range now {
			if a == b {
				return true
			}
		}
	}
	return false
}
