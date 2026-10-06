// Package mergelane is the serialized merge lane for repositories whose
// merge_strategy is hive-serialized (hivecommons/hive#10884). Per (repo,
// target branch) at most one pull request is at the front; only that pull
// request is brought up to date, checked on its exact head, re-checked live
// and merged with its head pinned. The front record is durable under /data
// and blocks regardless of the convergence ledger's mode.
//
// The lane is a gate the merge paths consult; it never decides whether Hive
// merges at all and nothing here runs for repositories left on direct.
package mergelane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghub "github.com/hivecommons/hive/pkg/github"
)

// DefaultFrontTimeout is the hive-wide front timeout (the relay's existing
// wait for CI); it restarts on every branch update.
const DefaultFrontTimeout = 60 * time.Minute

// DefaultStaleAfter drops a waiter whose merge path stopped asking for it.
const DefaultStaleAfter = 15 * time.Minute

// ReasonNotAtFront is the deferral reason every non-front caller gets.
const ReasonNotAtFront = "deferred: not at the front of the lane"

// Audit actions.
const (
	ActionFrontEnter = "front-enter"
	ActionFrontExit  = "front-exit"
	ActionDeferred   = "deferred"
	ActionUpdate     = "branch-update"
	ActionMerge      = "merge"
	ActionRefusal    = "refusal"
	ActionAlert      = "alert"
	ActionRestart    = "restart-reset"
)

// ErrHeadMoved is what a GitHub adapter returns when a pinned call (merge or
// branch update) is refused because the head is no longer the pinned SHA
// (HTTP 409 / 422 expected_head_sha mismatch).
var ErrHeadMoved = errors.New("mergelane: pull request head moved past the pinned SHA")

// PullRequest is the live state the lane reads.
type PullRequest struct {
	Number         int
	Head           string
	Base           string
	Open           bool
	Merged         bool
	Draft          bool
	MergeableState string
	Mergeable      *bool
	Labels         []string
}

// CheckRun is one check result on a head, keyed by its required-check name.
type CheckRun struct {
	Status     string
	Conclusion string
}

// GitHub is everything the lane asks GitHub. Every read must be live (no
// cache), so the final re-check sees the state at that moment.
type GitHub interface {
	PullRequest(ctx context.Context, repo string, number int) (PullRequest, error)
	BranchTip(ctx context.Context, repo, branch string) (string, error)
	// HeadContains compares tip...head with the compare API and reports
	// whether head contains tip (behind_by == 0). GitHub's mergeable_state
	// "behind" is never used: it is not reported without the up-to-date rule.
	HeadContains(ctx context.Context, repo, tip, head string) (bool, error)
	// RequiredChecks is the lane-only branch-rules reader (ReadBranchRules).
	RequiredChecks(ctx context.Context, repo, branch string) ghub.BranchRulesResult
	CheckRuns(ctx context.Context, repo, head string) (map[string]CheckRun, error)
	// UpdateBranch merges the target branch into the PR branch, pinned to
	// expectedHead; it never rebases or force-pushes.
	UpdateBranch(ctx context.Context, repo string, number int, expectedHead string) error
	// Merge is the REST merge with head pinned, no admin bypass; it returns
	// the merge commit SHA.
	Merge(ctx context.Context, repo string, number int, head string) (string, error)
	CommitParents(ctx context.Context, repo, sha string) ([]string, error)
	PullRequestsForCommit(ctx context.Context, repo, sha string) ([]int, error)
}

// Authorizer re-checks the calling merge path's own authorization for head
// (for example: the lgtm approval still matches it). nil fails closed.
type Authorizer func(ctx context.Context, head string) error

// Event is one audited lane decision (R31).
type Event struct {
	At     time.Time
	Repo   string
	Branch string
	PR     int
	Head   string
	Tip    string
	Action string
	Reason string
}

// Alert reports a merge that did not land directly on the validated tip.
type Alert struct {
	Repo             string
	Branch           string
	PR               int
	MergeSHA         string
	ValidatedTip     string
	UnexpectedCommit string
	UnexpectedPRs    []int
	Reason           string
}

// Options configures a Lane. Policy callbacks left nil fail closed.
type Options struct {
	Store  *Store
	GitHub GitHub
	Now    func() time.Time
	// DefaultTimeout is the hive-wide front timeout; TimeoutFor returns a
	// per-repo override (<= 0 means use the default).
	DefaultTimeout time.Duration
	TimeoutFor     func(repo string) time.Duration
	StaleAfter     time.Duration
	// Strategy returns the repo's live merge strategy.
	Strategy         func(repo string) string
	AutoMergeAllowed func(repo string) bool
	Paused           func(repo string) bool
	// Blocked returns a reason when hold, do-not-merge, exempt or pause
	// labels are present; nil uses the shared hold/exempt label helpers.
	Blocked func(repo string, labels []string) string
	Audit   func(Event)
	Alert   func(Alert)
	Logger  *slog.Logger
}

// Outcome is what a gate call did.
type Outcome string

const (
	OutcomeFront    Outcome = "front"
	OutcomeDeferred Outcome = "deferred"
	OutcomeWaiting  Outcome = "waiting"
	OutcomeUpdated  Outcome = "updated"
	OutcomeLeft     Outcome = "left"
	OutcomeMerged   Outcome = "merged"
)

// Decision is a gate call's result. Reason is set for every no-merge.
type Decision struct {
	Outcome  Outcome
	Reason   string
	Position int
	MergeSHA string
}

// Lane is the gate. One Lane per process; it resets every in-progress front on
// construction so a validation interrupted by a restart is repeated in full.
type Lane struct {
	opts  Options
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New builds the lane and fences every front left by a previous process.
func New(opts Options) (*Lane, error) {
	if opts.Store == nil || opts.GitHub == nil {
		return nil, errors.New("mergelane: a store and a GitHub client are required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.DefaultTimeout <= 0 {
		opts.DefaultTimeout = DefaultFrontTimeout
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = DefaultStaleAfter
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	l := &Lane{opts: opts, locks: map[string]*sync.Mutex{}}
	var ev []Event
	err := opts.Store.update(func(lanes map[string]*Record) error {
		for _, rec := range lanes {
			if rec.Front == nil {
				continue
			}
			rec.Epoch++
			rec.Front.Epoch = rec.Epoch
			rec.Front.Stage = StageValidating
			rec.Front.EvaluatedHead = ""
			rec.Front.LastReason = ""
			ev = append(ev, l.event(rec, rec.Front.PR, "", "", ActionRestart,
				"restart: the in-progress validation is repeated in full before any merge"))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	l.emit(ev)
	return l, nil
}

// Snapshot returns the lane's durable record.
func (l *Lane) Snapshot(repo, branch string) (Record, bool, error) {
	return l.opts.Store.Snapshot(repo, branch)
}

// Acquire records that pr is eligible on its merge path and asks for the
// front. The oldest eligible pull request reaches the front first; every
// other caller gets ReasonNotAtFront with its position behind the front.
func (l *Lane) Acquire(repo, branch string, pr int, path string) (Decision, error) {
	if err := validArgs(repo, branch, pr); err != nil {
		return Decision{}, err
	}
	now := l.opts.Now()
	var dec Decision
	var ev []Event
	err := l.opts.Store.update(func(lanes map[string]*Record) error {
		rec := lane(lanes, repo, branch)
		l.expireLocked(rec, now, &ev)
		l.pruneLocked(rec, now, pr)
		if rec.Front == nil || rec.Front.PR != pr {
			if i := rec.waiterIndex(pr); i >= 0 {
				rec.Waiting[i].LastSeen = now
			} else {
				rec.Waiting = append(rec.Waiting, Waiter{PR: pr, Path: path, EligibleAt: now, LastSeen: now})
				if rec.Front != nil {
					ev = append(ev, l.event(rec, pr, "", "", ActionDeferred, ReasonNotAtFront))
				}
			}
		}
		l.promoteLocked(rec, now, &ev)
		dec = position(rec, pr)
		return nil
	})
	if err != nil {
		return Decision{}, err
	}
	l.emit(ev)
	return dec, nil
}

// Release takes pr out of the lane (its path no longer finds it eligible). A
// released front PR goes to the back when it becomes eligible again.
func (l *Lane) Release(repo, branch string, pr int, reason string) error {
	if err := validArgs(repo, branch, pr); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		reason = "released by its merge path"
	}
	now := l.opts.Now()
	var ev []Event
	err := l.opts.Store.update(func(lanes map[string]*Record) error {
		rec := lane(lanes, repo, branch)
		if rec.Front != nil && rec.Front.PR == pr {
			l.exitLocked(rec, now, rec.Front.EvaluatedHead, "", reason, &ev)
			l.promoteLocked(rec, now, &ev)
			return nil
		}
		rec.removeWaiter(pr)
		return nil
	})
	if err != nil {
		return err
	}
	l.emit(ev)
	return nil
}

// Advance runs one round of the front PR's validation: validate → update →
// wait → final live re-check → pinned merge → post-merge verify. A caller
// that is not at the front gets ReasonNotAtFront and nothing is read or
// written on GitHub for it. Any change, error or ambiguity means no merge.
func (l *Lane) Advance(ctx context.Context, repo, branch string, pr int, authorize Authorizer) (Decision, error) {
	if err := validArgs(repo, branch, pr); err != nil {
		return Decision{}, err
	}
	unlock := l.lockLane(repo, branch)
	defer unlock()
	now := l.opts.Now()
	var front *Front
	var dec Decision
	var ev []Event
	err := l.opts.Store.update(func(lanes map[string]*Record) error {
		rec := lane(lanes, repo, branch)
		l.expireLocked(rec, now, &ev)
		l.promoteLocked(rec, now, &ev)
		if rec.Front == nil || rec.Front.PR != pr {
			dec = position(rec, pr)
			return nil
		}
		f := *rec.Front
		front = &f
		return nil
	})
	if err != nil {
		return Decision{}, err
	}
	l.emit(ev)
	if front == nil {
		return dec, nil
	}
	r := &round{l: l, ctx: ctx, repo: repo, branch: branch, front: front, authorize: authorize}
	return r.run()
}

func validArgs(repo, branch string, pr int) error {
	if strings.TrimSpace(repo) == "" || strings.TrimSpace(branch) == "" || pr <= 0 {
		return fmt.Errorf("mergelane: repo, branch and a positive PR number are required (got %q, %q, %d)", repo, branch, pr)
	}
	return nil
}

func position(rec *Record, pr int) Decision {
	if rec.Front != nil && rec.Front.PR == pr {
		return Decision{Outcome: OutcomeFront}
	}
	return Decision{Outcome: OutcomeDeferred, Reason: ReasonNotAtFront, Position: rec.waiterIndex(pr) + 1}
}

func (l *Lane) lockLane(repo, branch string) func() {
	key := laneKey(repo, branch)
	l.mu.Lock()
	m, ok := l.locks[key]
	if !ok {
		m = &sync.Mutex{}
		l.locks[key] = m
	}
	l.mu.Unlock()
	m.Lock()
	return m.Unlock
}

func (l *Lane) timeoutFor(repo string) time.Duration {
	if l.opts.TimeoutFor != nil {
		if d := l.opts.TimeoutFor(repo); d > 0 {
			return d
		}
	}
	return l.opts.DefaultTimeout
}

func (l *Lane) expireLocked(rec *Record, now time.Time, ev *[]Event) {
	if rec.Front != nil && now.After(rec.Front.DeadlineAt) {
		l.exitLocked(rec, now, rec.Front.EvaluatedHead, "",
			fmt.Sprintf("front timeout: required checks did not finish within %s", l.timeoutFor(rec.Repo)), ev)
	}
}

func (l *Lane) exitLocked(rec *Record, now time.Time, head, tip, reason string, ev *[]Event) {
	pr := rec.Front.PR
	rec.Front = nil
	rec.removeWaiter(pr)
	rec.LastExit = &Exit{PR: pr, Reason: reason, At: now}
	*ev = append(*ev, l.event(rec, pr, head, tip, ActionFrontExit, reason))
}

func (l *Lane) pruneLocked(rec *Record, now time.Time, keep int) {
	cutoff := now.Add(-l.opts.StaleAfter)
	kept := rec.Waiting[:0]
	for _, w := range rec.Waiting {
		if w.PR == keep || !w.LastSeen.Before(cutoff) {
			kept = append(kept, w)
		}
	}
	rec.Waiting = kept
}

func (l *Lane) promoteLocked(rec *Record, now time.Time, ev *[]Event) {
	if rec.Front != nil || len(rec.Waiting) == 0 {
		return
	}
	w := rec.Waiting[0]
	rec.Waiting = append([]Waiter(nil), rec.Waiting[1:]...)
	rec.Epoch++
	rec.Front = &Front{
		PR: w.PR, Path: w.Path, Epoch: rec.Epoch, Stage: StageValidating,
		EnteredAt: now, DeadlineAt: now.Add(l.timeoutFor(rec.Repo)),
	}
	*ev = append(*ev, l.event(rec, w.PR, "", "", ActionFrontEnter, "oldest eligible pull request reached the front"))
}

func (l *Lane) event(rec *Record, pr int, head, tip, action, reason string) Event {
	return Event{At: l.opts.Now(), Repo: rec.Repo, Branch: rec.Branch, PR: pr, Head: head, Tip: tip, Action: action, Reason: reason}
}

func (l *Lane) emit(events []Event) {
	for _, e := range events {
		l.opts.Logger.Info("merge lane decision",
			"repo", e.Repo, "branch", e.Branch, "pr", e.PR, "head", e.Head, "tip", e.Tip,
			"action", e.Action, "reason", e.Reason)
		if l.opts.Audit != nil {
			l.opts.Audit(e)
		}
	}
}

// strategyProblem reports a repo no longer on hive-serialized (R4): its
// validation ends without a merge. Deciding this needs no GitHub call.
func (l *Lane) strategyProblem(repo string) string {
	strategy := ""
	if l.opts.Strategy != nil {
		strategy = l.opts.Strategy(repo)
	}
	if strategy != config.MergeStrategyHiveSerialized {
		return fmt.Sprintf("merge strategy is %q, not %q: validation ended without a merge", strategy, config.MergeStrategyHiveSerialized)
	}
	return ""
}

// policyProblem re-reads the live repo policy and labels.
func (l *Lane) policyProblem(repo string, labels []string) string {
	if reason := l.strategyProblem(repo); reason != "" {
		return reason
	}
	if l.opts.Paused != nil && l.opts.Paused(repo) {
		return "repository is paused"
	}
	if l.opts.AutoMergeAllowed == nil || !l.opts.AutoMergeAllowed(repo) {
		return "auto-merge is not allowed for the repository"
	}
	if l.opts.Blocked != nil {
		return l.opts.Blocked(repo, labels)
	}
	return defaultBlocked(labels)
}

func defaultBlocked(labels []string) string {
	for _, label := range labels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(label)), "hive-pause/") {
			return fmt.Sprintf("pause label %q is present", label)
		}
	}
	if ghub.HasHoldLabel(labels) {
		return "a hold label is present"
	}
	if ghub.HasExemptLabel(labels, nil) {
		return "a do-not-merge or exempt label is present"
	}
	return ""
}

// prShape reports a PR that can no longer merge into branch; such a PR is no
// longer eligible and leaves the front.
func prShape(p PullRequest, branch string) string {
	switch {
	case p.Merged:
		return "pull request is already merged"
	case !p.Open:
		return "pull request is closed"
	case p.Draft:
		return "pull request is a draft"
	case p.Base != branch:
		return fmt.Sprintf("pull request now targets %q, not %q", p.Base, branch)
	}
	return ""
}

// mergeability maps GitHub's answer: conflicts leave the front, unknown and
// any other no wait for the next round.
func mergeability(p PullRequest) (string, bool) {
	switch ghub.MergeableFromState(p.MergeableState, p.Mergeable) {
	case ghub.MergeableYes:
		return "", false
	case ghub.MergeableUnknown:
		return "mergeability is not yet known", false
	}
	if p.MergeableState == "dirty" {
		return "pull request has merge conflicts", true
	}
	return fmt.Sprintf("pull request is not mergeable (state %q)", p.MergeableState), false
}

type checkVerdict struct {
	outcome ghub.CheckOutcome
	reason  string
}

// checks re-reads the required set and its results on head. An unknown or
// empty set never passes (Decision #5); missing, queued or running waits.
func (l *Lane) checks(ctx context.Context, repo, branch, head string) checkVerdict {
	rules := l.opts.GitHub.RequiredChecks(ctx, repo, branch)
	if !rules.Known || len(rules.Required) == 0 {
		reason := rules.Reason
		if reason == "" {
			reason = "required check set is empty"
		}
		return checkVerdict{ghub.CheckWait, "required checks unknown, no merge: " + reason}
	}
	runs, err := l.opts.GitHub.CheckRuns(ctx, repo, head)
	if err != nil {
		return checkVerdict{ghub.CheckWait, "reading check results failed: " + err.Error()}
	}
	var waiting []string
	for _, name := range rules.SortedRequired() {
		run, ok := runs[name]
		if !ok {
			waiting = append(waiting, name+" (missing)")
			continue
		}
		switch ghub.ClassifyRequiredCheck(run.Status, run.Conclusion) {
		case ghub.CheckFailed:
			return checkVerdict{ghub.CheckFailed, fmt.Sprintf("required check %q finished as %s on head %s", name, run.Conclusion, short(head))}
		case ghub.CheckWait:
			waiting = append(waiting, fmt.Sprintf("%s (%s)", name, run.Status))
		}
	}
	if len(waiting) > 0 {
		return checkVerdict{ghub.CheckWait, fmt.Sprintf("waiting for required checks on head %s: %s", short(head), strings.Join(waiting, ", "))}
	}
	return checkVerdict{outcome: ghub.CheckPassed}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// round is one Advance of the front PR. front is a copy of the durable front;
// every write back is fenced on its epoch.
type round struct {
	l         *Lane
	ctx       context.Context
	repo      string
	branch    string
	front     *Front
	authorize Authorizer
}

func (r *round) run() (Decision, error) {
	gh := r.l.opts.GitHub
	if reason := r.l.strategyProblem(r.repo); reason != "" {
		return r.leave("", "", reason)
	}
	p, err := gh.PullRequest(r.ctx, r.repo, r.front.PR)
	if err != nil {
		return r.hold("", "", "reading the pull request failed: "+err.Error())
	}
	if reason := prShape(p, r.branch); reason != "" {
		return r.leave(p.Head, "", reason)
	}
	if reason := r.l.policyProblem(r.repo, p.Labels); reason != "" {
		return r.leave(p.Head, "", reason)
	}
	if r.front.PinnedHead != "" {
		if p.Head == r.front.PinnedHead {
			return r.hold(p.Head, "", "waiting for the branch update to produce a new head")
		}
		r.front.PinnedHead = ""
		r.front.EvaluatedHead = ""
	}
	if r.front.EvaluatedHead != "" && r.front.EvaluatedHead != p.Head {
		r.l.emit([]Event{r.event(p.Head, "", ActionRefusal,
			fmt.Sprintf("head changed during validation from %s: the new head is evaluated from the start", short(r.front.EvaluatedHead)))})
	}
	r.front.EvaluatedHead = p.Head
	r.front.Stage = StageValidating

	tip, err := gh.BranchTip(r.ctx, r.repo, r.branch)
	if err != nil {
		return r.hold(p.Head, "", "reading the target branch tip failed: "+err.Error())
	}
	contains, err := gh.HeadContains(r.ctx, r.repo, tip, p.Head)
	if err != nil {
		return r.hold(p.Head, tip, "comparing the head with the target branch failed: "+err.Error())
	}
	if !contains {
		return r.update(p.Head, tip)
	}
	if v := r.l.checks(r.ctx, r.repo, r.branch, p.Head); v.outcome != ghub.CheckPassed {
		if v.outcome == ghub.CheckFailed {
			return r.leave(p.Head, tip, v.reason)
		}
		r.front.Stage = StageWaitingChecks
		return r.hold(p.Head, tip, v.reason)
	}
	if reason, leave := mergeability(p); reason != "" {
		if leave {
			return r.leave(p.Head, tip, reason)
		}
		return r.hold(p.Head, tip, reason)
	}
	if reason, leave := r.finalCheck(p.Head, tip); reason != "" {
		reason = "final re-check: " + reason
		if leave {
			return r.leave(p.Head, tip, reason)
		}
		return r.hold(p.Head, tip, reason)
	}
	return r.merge(p.Head, tip)
}

// finalCheck re-reads everything live immediately before the merge (R10).
// The head and tip are compared with the validated ones; containment is a
// property of those two SHAs, so it holds when both are unchanged.
func (r *round) finalCheck(head, tip string) (string, bool) {
	gh := r.l.opts.GitHub
	q, err := gh.PullRequest(r.ctx, r.repo, r.front.PR)
	if err != nil {
		return "reading the pull request failed: " + err.Error(), false
	}
	if q.Head != head {
		return fmt.Sprintf("head changed from %s to %s", short(head), short(q.Head)), false
	}
	if reason := prShape(q, r.branch); reason != "" {
		return reason, true
	}
	if reason, leave := mergeability(q); reason != "" {
		return reason, leave
	}
	if reason := r.l.policyProblem(r.repo, q.Labels); reason != "" {
		return reason, true
	}
	now, err := gh.BranchTip(r.ctx, r.repo, r.branch)
	if err != nil {
		return "reading the target branch tip failed: " + err.Error(), false
	}
	if now != tip {
		return fmt.Sprintf("target branch moved from %s to %s; the pull request is brought up to date again", short(tip), short(now)), false
	}
	if v := r.l.checks(r.ctx, r.repo, r.branch, head); v.outcome != ghub.CheckPassed {
		return v.reason, v.outcome == ghub.CheckFailed
	}
	if r.authorize == nil {
		return "no merge-path authorization was supplied", false
	}
	if err := r.authorize(r.ctx, head); err != nil {
		return "the merge path's authorization no longer holds: " + err.Error(), false
	}
	return "", false
}

func (r *round) update(head, tip string) (Decision, error) {
	err := r.l.opts.GitHub.UpdateBranch(r.ctx, r.repo, r.front.PR, head)
	if errors.Is(err, ErrHeadMoved) {
		r.front.EvaluatedHead = ""
		return r.hold(head, tip, "head changed before the branch update; the new head is evaluated from the start")
	}
	if err != nil {
		return r.hold(head, tip, "branch update failed: "+err.Error())
	}
	r.front.PinnedHead = head
	r.front.Stage = StageUpdating
	r.front.LastReason = ""
	r.front.DeadlineAt = r.l.opts.Now().Add(r.l.timeoutFor(r.repo))
	reason := fmt.Sprintf("head does not contain the target branch tip %s: merged the tip in, pinned to head %s", short(tip), short(head))
	if err := r.save(); err != nil {
		return Decision{Outcome: OutcomeWaiting, Reason: err.Error()}, err
	}
	r.l.emit([]Event{r.event(head, tip, ActionUpdate, reason)})
	return Decision{Outcome: OutcomeUpdated, Reason: reason}, nil
}

func (r *round) merge(head, tip string) (Decision, error) {
	r.front.Stage = StageMerging
	if err := r.save(); err != nil {
		return Decision{Outcome: OutcomeWaiting, Reason: err.Error()}, err
	}
	sha, err := r.l.opts.GitHub.Merge(r.ctx, r.repo, r.front.PR, head)
	if errors.Is(err, ErrHeadMoved) {
		r.front.EvaluatedHead = ""
		r.front.Stage = StageValidating
		return r.hold(head, tip, "merge refused: head moved past the pinned SHA; validation restarts")
	}
	if err != nil {
		r.front.Stage = StageValidating
		return r.hold(head, tip, "merge call failed; no merge assumed this round: "+err.Error())
	}
	reason := fmt.Sprintf("merged as %s after the final re-check", short(sha))
	r.l.emit([]Event{r.event(head, tip, ActionMerge, reason)})
	dec, lerr := r.leave(head, tip, reason)
	dec.Outcome, dec.MergeSHA = OutcomeMerged, sha
	r.verify(sha, tip)
	return dec, lerr
}

// verify checks the merge landed directly on the validated tip (R13).
func (r *round) verify(sha, tip string) {
	gh := r.l.opts.GitHub
	a := Alert{Repo: r.repo, Branch: r.branch, PR: r.front.PR, MergeSHA: sha, ValidatedTip: tip}
	parents, err := gh.CommitParents(r.ctx, r.repo, sha)
	switch {
	case err != nil:
		a.Reason = fmt.Sprintf("could not verify that PR #%d's merge %s landed on the validated tip %s: %v", r.front.PR, short(sha), short(tip), err)
	case len(parents) == 0:
		a.Reason = fmt.Sprintf("PR #%d's merge %s has no parent commit", r.front.PR, short(sha))
	case parents[0] != tip:
		a.UnexpectedCommit = parents[0]
		a.UnexpectedPRs, _ = gh.PullRequestsForCommit(r.ctx, r.repo, parents[0])
		a.Reason = fmt.Sprintf("PR #%d merged as %s on unexpected commit %s, not on the validated tip %s", r.front.PR, short(sha), short(parents[0]), short(tip))
		if len(a.UnexpectedPRs) > 0 {
			a.Reason += fmt.Sprintf(" (commit from PR #%d)", a.UnexpectedPRs[0])
		}
	default:
		return
	}
	r.l.opts.Logger.Error("merge lane: merge did not land on the validated tip",
		"repo", r.repo, "branch", r.branch, "pr", r.front.PR, "merge", sha, "reason", a.Reason)
	r.l.emit([]Event{r.event(sha, tip, ActionAlert, a.Reason)})
	if r.l.opts.Alert != nil {
		r.l.opts.Alert(a)
	}
}

func (r *round) event(head, tip, action, reason string) Event {
	return Event{At: r.l.opts.Now(), Repo: r.repo, Branch: r.branch, PR: r.front.PR, Head: head, Tip: tip, Action: action, Reason: reason}
}

// hold keeps the PR at the front without a merge this round; a reason is
// audited once per change.
func (r *round) hold(head, tip, reason string) (Decision, error) {
	audit := reason != r.front.LastReason
	r.front.LastReason = reason
	if err := r.save(); err != nil {
		return Decision{Outcome: OutcomeWaiting, Reason: err.Error()}, err
	}
	if audit {
		r.l.emit([]Event{r.event(head, tip, ActionRefusal, reason)})
	}
	return Decision{Outcome: OutcomeWaiting, Reason: reason}, nil
}

// leave ends the front with reason and starts the next pull request.
func (r *round) leave(head, tip, reason string) (Decision, error) {
	now := r.l.opts.Now()
	var ev []Event
	err := r.l.opts.Store.update(func(lanes map[string]*Record) error {
		rec := lane(lanes, r.repo, r.branch)
		if !r.fenceOK(rec) {
			return ErrFenced
		}
		r.l.exitLocked(rec, now, head, tip, reason, &ev)
		r.l.promoteLocked(rec, now, &ev)
		return nil
	})
	r.l.emit(ev)
	return Decision{Outcome: OutcomeLeft, Reason: reason}, err
}

// save writes the round's front back only if it is still the same front.
func (r *round) save() error {
	return r.l.opts.Store.update(func(lanes map[string]*Record) error {
		rec := lane(lanes, r.repo, r.branch)
		if !r.fenceOK(rec) {
			return ErrFenced
		}
		*rec.Front = *r.front
		return nil
	})
}

func (r *round) fenceOK(rec *Record) bool {
	return rec.Front != nil && rec.Front.PR == r.front.PR && rec.Front.Epoch == r.front.Epoch
}
