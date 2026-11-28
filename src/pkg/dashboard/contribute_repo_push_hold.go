package dashboard

// Per-contributor, per-repository push hold.
//
// A contributor works in their own fork. When that fork is far behind
// upstream and their token lacks the `workflow` scope, any branch based on
// current upstream carries workflow-file changes the fork does not have, and
// GitHub rejects the push: no pull request can be opened in that repository,
// whatever the issue. One contributor on a production hive lost six of eleven
// tasks in two days this way, across three repositories, about ten minutes of
// agent time each, while their tasks in a fourth repository (a fresh fork)
// shipped.
//
// Nothing remembered it. The failure streak (contribute_failure_streak.go)
// counts consecutive fast or stalled failures, and these alternate with
// successes elsewhere. The tooling-failure reclassification
// (contribute_blocked_tooling.go) keeps such a report from parking the issue
// for everyone, which is the issue's side; this file is the contributor's
// side. When a task ends, failed or complete without a pull request, with a
// reason that says the push to the contributor's fork was rejected, the hub
// books a hold for that contributor and that repository: selectTask skips the
// repository for them until the hold ends, other contributors are still
// offered its issues, and the held contributor is still offered every other
// repository.
//
// Deliberate boundaries:
//   - The signal is the relay's own reason text. A relay can only hold
//     itself out of a repository with it, never anyone else, and gains
//     nothing by doing so, so the self-report is safe to act on. The
//     client-declared failure_kind is not consulted (#2547).
//   - A failed push together with workflow-scope evidence (the `workflow`
//     scope, or GitHub's own "create or update workflow" refusal) books the
//     hold at once. One clause saying a push to the fork was refused, with
//     no such evidence, is only a strike: text alone cannot tell GitHub's
//     refusal from "I ran out of time before pushing", so the hold is booked
//     when a second such report arrives within 24 hours. The scope merely
//     being mentioned, or a failed push and the word fork in different parts
//     of the text, are nothing, and a reason that names a fork of another
//     repository books nothing.
//   - The key is the contributor account without the session label
//     (contributorFlapIdentity, as for the flap cool-off): a stale fork
//     belongs to the GitHub account, so every session of one contributor
//     shares the hold and a relay cannot dodge it by renaming its session.
//   - The hold ends on its own. The first lasts 6 hours, the next 24, every
//     later one 72. When it ends the contributor is offered the repository
//     once more; a pull request the hub verifies there clears the record,
//     another push failure books the next, longer hold.
//   - A failure reported while a hold stands (a task already in flight on
//     another connection of the same identity) neither extends nor escalates
//     it, and sends no second notice.
//   - The issue is untouched: no failure, cooldown or quarantine is booked
//     on it beyond what the completion or failure already books today.
//   - No operator action clears a hold. Expiry and the verified pull request
//     are the two ways out.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// contributorRepoPushHoldFirst, Second and Max are the backoff: how long
	// the first, the second and every later hold of one contributor from one
	// repository lasts.
	contributorRepoPushHoldFirst  = 6 * time.Hour
	contributorRepoPushHoldSecond = 24 * time.Hour
	contributorRepoPushHoldMax    = 72 * time.Hour
	// contributorRepoPushHoldMemory is how long a record outlives its hold.
	// While it is kept, the next push failure escalates; once it is dropped,
	// the next one starts again at contributorRepoPushHoldFirst.
	contributorRepoPushHoldMemory = 14 * 24 * time.Hour
	// contributorRepoPushStrikeWindow is how long one report of a failed
	// push with no workflow-scope evidence counts as a strike: a second such
	// report for the same contributor and repository inside it books the
	// hold, a later one is a first strike again.
	contributorRepoPushStrikeWindow = 24 * time.Hour
	// contributorRepoPushHoldLimit bounds the ledger. Past it the records
	// whose holds ended longest ago are dropped first.
	contributorRepoPushHoldLimit = 1024
	// contributorRepoPushHoldsFileName is the on-disk ledger, beside the
	// no-PR streak ledger.
	contributorRepoPushHoldsFileName = "contributor-repo-push-holds.json"
	// contributorRepoPushHoldReason names the refusal in the hub log and the
	// decision ring.
	contributorRepoPushHoldReason = "contributor-repo-push-hold"
	// contributorRepoPushHoldFix is how the contributor repairs it, the same
	// words on every surface.
	contributorRepoPushHoldFix = "bring your fork up to date (the Sync fork button on the fork's GitHub page), or give your GitHub token the workflow scope (gh auth refresh -h github.com -s workflow)"
)

// The evidence a reason can carry (classifyContributorForkPushFailure).
var (
	// forkPushScopeRefusalRE is a refused push with the workflow scope named
	// right behind it: "push to fork owner/repo rejected because the token
	// lacks workflow scope", "could not push to my fork: token lacks workflow
	// scope", or GitHub's own "refusing to allow ... to create or update
	// workflow". The refusal follows the push inside one clause and the scope
	// follows the refusal, so the scope merely being mentioned, or a push
	// that worked beside something else that failed, is not this.
	forkPushScopeRefusalRE = regexp.MustCompile(`(?i)(?:\bpush(?:ed|ing)?\b[^;:,!?]{0,60}?\b(?:rejected|denied|refused|failed|forbidden|not permitted)\b|\b(?:cannot|can't|can not|could not|couldn't|unable to|not able to|failed to)\s+push\b).{0,160}?(?:\bworkflow\b.{0,20}\bscope\b|\bcreate or update workflow\b)`)
	// forkPushRefusedRE is one clause saying a push to the fork was refused:
	// "push to fork owner/repo rejected", "the push to my fork is rejected".
	// The refusal must follow the fork (and its name) directly, so a fork
	// that was pushed to and something else that failed do not add up.
	forkPushRefusedRE = regexp.MustCompile(`(?i)\bpush(?:ed|ing)?\b[^;:,!?]{0,40}?\bto\s+(?:(?:my|the|their|our|your)\s+)?fork(?:\s+(?:of\s+)?[\w.-]+/[\w.-]+)?\s+(?:(?:is|was|were|got|gets|has been|had been|being)\s+)?(?:rejected|denied|refused|failed|forbidden|not permitted)\b`)
	// forkPushCouldNotRE is the same clause the other way round: "could not
	// push to my fork", with at most four words between the push and the
	// fork, so it does not reach a later push that worked.
	forkPushCouldNotRE = regexp.MustCompile(`(?i)\b(?:cannot|can't|can not|could not|couldn't|unable to|not able to|failed to)\s+push\b(?:\s+[^\s;:,!?]+){0,4}?\s+to\s+(?:(?:my|the|their|our|your)\s+)?fork\b`)
	// forkPushForkRefusedRE is "my fork rejected the push".
	forkPushForkRefusedRE = regexp.MustCompile(`(?i)\bfork(?:\s+[\w.-]+/[\w.-]+)?\s+(?:rejected|refused|denied)\s+(?:the|my|this|that)\s+push\b`)
	// forkPushNamedForkRE finds a fork the reason names as owner/repo.
	forkPushNamedForkRE = regexp.MustCompile(`(?i)\bfork\s+(?:of\s+)?([\w.-]+)/([\w.-]+)`)
	// forkPushUpstreamGateRE is a refusal by the upstream repository's branch
	// protection instead.
	forkPushUpstreamGateRE = regexp.MustCompile(`(?i)\bbranch protection\b|\bprotected branch\b`)
)

// forkPushEvidence is how strongly a reason says the contributor could not
// push to their fork.
type forkPushEvidence int

const (
	// forkPushNoEvidence: the reason does not say it.
	forkPushNoEvidence forkPushEvidence = iota
	// forkPushClauseOnly: one clause says a push to the fork was refused or
	// could not be made, and nothing says why. A regular expression cannot
	// tell "GitHub rejected it" from "I ran out of time before pushing", so
	// one such report is a strike and only a second books a hold.
	forkPushClauseOnly
	// forkPushScopeBacked: a failed push together with the missing workflow
	// scope, the fault this hold exists for. It books the hold at once.
	forkPushScopeBacked
)

// classifyContributorForkPushFailure reads a task's failure or completion
// reason for the contributor being unable to push to their fork of taskRepo.
// The reason is first a tooling failure by the hub's own classifier
// (isBlockedVerdictToolingFailure); this narrows that verdict to the fork:
//
//   - forkPushScopeBacked: a refused push with workflow-scope evidence
//     right behind it (forkPushScopeRefusalRE);
//   - forkPushClauseOnly: one clause saying a push to the fork was refused,
//     with no scope evidence;
//   - when the reason names a fork as owner/repo, the repository name must
//     be taskRepo's. A fork of some other repository the agent tried to
//     patch says nothing about this one. A git ref ("origin/main") is not a
//     fork name.
//
// The scope merely being mentioned, a stale fork or a refused sync alone is
// not enough, and neither is a push the upstream repository's branch
// protection refused: that is the pull request path working, not a fork to
// repair.
func classifyContributorForkPushFailure(reason, taskRepo string) forkPushEvidence {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" || !isBlockedVerdictToolingFailure(reason) || forkPushUpstreamGateRE.MatchString(reason) {
		return forkPushNoEvidence
	}
	var evidence forkPushEvidence
	switch {
	case forkPushScopeRefusalRE.MatchString(reason):
		evidence = forkPushScopeBacked
	case forkPushRefusedRE.MatchString(reason), forkPushCouldNotRE.MatchString(reason), forkPushForkRefusedRE.MatchString(reason):
		evidence = forkPushClauseOnly
	default:
		return forkPushNoEvidence
	}
	if strings.TrimSpace(taskRepo) == "" {
		return evidence
	}
	want, named := repoNameOnly(taskRepo), false
	for _, m := range forkPushNamedForkRE.FindAllStringSubmatch(reason, -1) {
		owner, name := strings.ToLower(m[1]), strings.TrimSuffix(strings.TrimRight(m[2], "."), ".git")
		// "fork origin/main" and "the fork and/or ..." name no fork.
		if owner == "origin" || owner == "upstream" || owner == "refs" || (owner == "and" && strings.EqualFold(name, "or")) {
			continue
		}
		if strings.EqualFold(name, want) {
			return evidence
		}
		named = true
	}
	if named {
		return forkPushNoEvidence
	}
	return evidence
}

// isContributorForkPushFailure reports whether the reason carries any
// evidence that the contributor could not push to their fork of taskRepo.
func isContributorForkPushFailure(reason, taskRepo string) bool {
	return classifyContributorForkPushFailure(reason, taskRepo) != forkPushNoEvidence
}

// contributorRepoPushHold is the in-memory and on-disk shape of one
// contributor's hold from one repository.
type contributorRepoPushHold struct {
	// Identity is the contributorFlapIdentity key the hold is booked and
	// enforced under: the contributor account, never a session label.
	Identity string `json:"identity"`
	// ContributorID and Username name the registered contributor, for the
	// contributors list and the log.
	ContributorID string `json:"contributor_id,omitempty"`
	Username      string `json:"username,omitempty"`
	Repo          string `json:"repo"`
	// Count is how many holds this contributor has had from this repository
	// without a verified pull request in between; it picks the duration.
	Count  int       `json:"count"`
	HeldAt time.Time `json:"held_at"`
	Until  time.Time `json:"until"`
	// Reason is the relay's reason for the failure that booked the hold:
	// one line, token redacted, capped at noWorkReasonMaxLen.
	Reason string `json:"reason,omitempty"`
	// StrikeAt is when a failed push with no workflow-scope evidence was
	// last reported and booked no hold; zero when there is no such strike.
	// It is cleared when a hold is booked.
	StrikeAt time.Time `json:"strike_at,omitzero"`
	// skipLogged is the Until whose selectTask skip has been logged, so a
	// hold is logged once and not on every scan.
	skipLogged time.Time
}

// holdForgotten reports whether the record's hold ended so long ago that its
// count no longer applies (or it never had one).
func (r contributorRepoPushHold) holdForgotten(now time.Time) bool {
	return r.Count <= 0 || r.Until.IsZero() || !now.Before(r.Until.Add(contributorRepoPushHoldMemory))
}

// strikeLive reports whether the record carries a strike still inside
// contributorRepoPushStrikeWindow.
func (r contributorRepoPushHold) strikeLive(now time.Time) bool {
	return !r.StrikeAt.IsZero() && now.Sub(r.StrikeAt) < contributorRepoPushStrikeWindow
}

// forgotten reports whether nothing in the record applies any more: no hold
// count worth keeping and no live strike.
func (r contributorRepoPushHold) forgotten(now time.Time) bool {
	return r.holdForgotten(now) && !r.strikeLive(now)
}

// repoPushFailureOutcome is what one reported fork push failure did.
type repoPushFailureOutcome int

const (
	// repoPushNothing: a hold already stands; nothing changed.
	repoPushNothing repoPushFailureOutcome = iota
	// repoPushStrike: a first report with no scope evidence; no hold yet.
	repoPushStrike
	// repoPushHoldBooked: a hold was booked on scope-backed evidence.
	repoPushHoldBooked
	// repoPushHoldBookedOnSecond: a hold was booked on the second report
	// with no scope evidence inside the strike window.
	repoPushHoldBookedOnSecond
)

// contributorRepoPushHoldState is the ledger. pushHoldMu guards
// repoPushHolds; repoPushHoldsFile overrides the ledger's path;
// pushHoldSaveMu is held from a save's snapshot through its rename, so saves
// land in snapshot order.
type contributorRepoPushHoldState struct {
	pushHoldMu        sync.Mutex
	repoPushHolds     map[string]contributorRepoPushHold
	repoPushHoldsFile string
	pushHoldSaveMu    sync.Mutex
}

// contributorRepoPushHoldDuration is how long hold number count lasts.
func contributorRepoPushHoldDuration(count int) time.Duration {
	switch {
	case count <= 1:
		return contributorRepoPushHoldFirst
	case count == 2:
		return contributorRepoPushHoldSecond
	}
	return contributorRepoPushHoldMax
}

// contributorRepoPushHoldKey is the ledger key for one identity and one
// repository. Repository names compare case-insensitively on GitHub.
func contributorRepoPushHoldKey(identity, repo string) string {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if identity == "" || repo == "" {
		return ""
	}
	return identity + "\x00" + repo
}

// normalizePushHoldReason prepares the relay's reason for the ledger, as the
// idle-completion ledger does: token redacted, one line, capped.
func normalizePushHoldReason(reason string) string {
	reason = strings.Join(strings.Fields(redactTokens(reason)), " ")
	return strings.TrimSpace(truncateReasonBytes(reason))
}

// bookContributorRepoPushHold books a hold of c's contributor from repo at
// now, as scope-backed evidence does. It returns the hold that now stands and
// whether this call booked it: a failure inside a standing hold returns that
// hold and false.
func (h *ContributeWSHub) bookContributorRepoPushHold(c *ContributorConnection, repo, reason string, now time.Time) (contributorRepoPushHold, bool) {
	rec, outcome := h.recordContributorRepoPushFailure(c, repo, reason, true, now)
	return rec, outcome == repoPushHoldBooked
}

// recordContributorRepoPushFailure applies one reported fork push failure of
// c's contributor in repo at now. With scope-backed evidence it books the
// hold at once. Without, the first report is a strike that books nothing and
// the second inside contributorRepoPushStrikeWindow books the hold. A report
// inside a standing hold changes nothing. It returns the record as it now
// stands and what happened.
func (h *ContributeWSHub) recordContributorRepoPushFailure(c *ContributorConnection, repo, reason string, scopeBacked bool, now time.Time) (contributorRepoPushHold, repoPushFailureOutcome) {
	identity := contributorFlapIdentity(c)
	key := contributorRepoPushHoldKey(identity, repo)
	if h == nil || key == "" {
		return contributorRepoPushHold{}, repoPushNothing
	}
	h.pushHoldMu.Lock()
	if h.repoPushHolds == nil {
		h.repoPushHolds = make(map[string]contributorRepoPushHold)
	}
	rec := h.repoPushHolds[key]
	if rec.Count > 0 && now.Before(rec.Until) {
		h.pushHoldMu.Unlock()
		return rec, repoPushNothing
	}
	secondStrike := rec.strikeLive(now)
	if rec.holdForgotten(now) {
		rec = contributorRepoPushHold{}
	}
	rec.Identity = identity
	rec.ContributorID = c.profile.ContributorID
	rec.Username = c.profile.GitHubUsername
	rec.Repo = strings.TrimSpace(repo)
	outcome := repoPushStrike
	switch {
	case scopeBacked:
		outcome = repoPushHoldBooked
	case secondStrike:
		outcome = repoPushHoldBookedOnSecond
	}
	if outcome == repoPushStrike {
		rec.StrikeAt = now
	} else {
		rec.Count++
		rec.HeldAt = now
		rec.Until = now.Add(contributorRepoPushHoldDuration(rec.Count))
		rec.Reason = normalizePushHoldReason(reason)
		rec.StrikeAt = time.Time{}
		rec.skipLogged = time.Time{}
	}
	h.repoPushHolds[key] = rec
	h.pruneContributorRepoPushHoldsLocked(now)
	h.pushHoldMu.Unlock()
	h.saveContributorRepoPushHolds()
	return rec, outcome
}

// pruneContributorRepoPushHoldsLocked drops forgotten records and then, past
// contributorRepoPushHoldLimit, the records whose holds ended longest ago.
// Callers must hold pushHoldMu.
func (h *ContributeWSHub) pruneContributorRepoPushHoldsLocked(now time.Time) {
	for key, rec := range h.repoPushHolds {
		if rec.forgotten(now) {
			delete(h.repoPushHolds, key)
		}
	}
	over := len(h.repoPushHolds) - contributorRepoPushHoldLimit
	if over <= 0 {
		return
	}
	keys := make([]string, 0, len(h.repoPushHolds))
	for key := range h.repoPushHolds {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := h.repoPushHolds[keys[i]], h.repoPushHolds[keys[j]]
		if !a.Until.Equal(b.Until) {
			return a.Until.Before(b.Until)
		}
		return keys[i] < keys[j]
	})
	for _, key := range keys[:over] {
		delete(h.repoPushHolds, key)
	}
}

// clearContributorRepoPushHold forgets identity's hold from repo and its
// count, and reports whether there was a record. Called when the hub has
// verified a pull request by that contributor in that repository: the push
// works.
func (h *ContributeWSHub) clearContributorRepoPushHold(identity, repo string) bool {
	key := contributorRepoPushHoldKey(identity, repo)
	if h == nil || key == "" {
		return false
	}
	h.pushHoldMu.Lock()
	_, had := h.repoPushHolds[key]
	delete(h.repoPushHolds, key)
	h.pushHoldMu.Unlock()
	if had {
		h.saveContributorRepoPushHolds()
	}
	return had
}

// contributorRepoPushHoldActive reports whether identity is held from repo
// at now, with the hold. A lapsed record is left in place so the next push
// failure escalates.
func (h *ContributeWSHub) contributorRepoPushHoldActive(identity, repo string, now time.Time) (contributorRepoPushHold, bool) {
	key := contributorRepoPushHoldKey(identity, repo)
	if h == nil || key == "" {
		return contributorRepoPushHold{}, false
	}
	h.pushHoldMu.Lock()
	defer h.pushHoldMu.Unlock()
	rec, ok := h.repoPushHolds[key]
	if !ok || rec.Count <= 0 || !now.Before(rec.Until) {
		return contributorRepoPushHold{}, false
	}
	return rec, true
}

// contributorRepoPushHoldSkip is contributorRepoPushHoldActive for selectTask:
// the third return is true the first time a hold's skip is seen, so the
// caller logs each hold once and not on every scan.
func (h *ContributeWSHub) contributorRepoPushHoldSkip(identity, repo string, now time.Time) (contributorRepoPushHold, bool, bool) {
	key := contributorRepoPushHoldKey(identity, repo)
	if h == nil || key == "" {
		return contributorRepoPushHold{}, false, false
	}
	h.pushHoldMu.Lock()
	defer h.pushHoldMu.Unlock()
	rec, ok := h.repoPushHolds[key]
	if !ok || rec.Count <= 0 || !now.Before(rec.Until) {
		return contributorRepoPushHold{}, false, false
	}
	first := !rec.skipLogged.Equal(rec.Until)
	if first {
		rec.skipLogged = rec.Until
		h.repoPushHolds[key] = rec
	}
	return rec, true, first
}

// contributorRepoPushHoldNotice is what the relay prints when a hold is
// booked: what happened, how to fix it, and when the hub tries again.
func contributorRepoPushHoldNotice(hold contributorRepoPushHold, onSecondReport bool) string {
	why := ": your agent could not push its branch to your fork of that repository, so no pull request was opened."
	if onSecondReport {
		why = ": twice within a day your agent reported that it could not push its branch to your fork of that repository, so no pull request was opened."
	}
	return "Tasks from " + hold.Repo + " are paused for you until " + hold.Until.UTC().Format(time.RFC3339) + why +
		" To fix it, " + contributorRepoPushHoldFix + "." +
		" Tasks from other repositories keep arriving. This hive offers you " + hold.Repo +
		" again when the pause ends; a pull request from you there ends the pauses, another failed push starts a longer one."
}

// contributorRepoPushHoldIdleMessage rides on the no_matching_work answer when
// the scan skipped one or more held repositories and nothing else was
// admissible, so a relay that missed the notice still learns why it is idle.
func contributorRepoPushHoldIdleMessage(holds []contributorRepoPushHold) string {
	if len(holds) == 0 {
		return ""
	}
	parts := make([]string, 0, len(holds))
	for _, hold := range holds {
		parts = append(parts, hold.Repo+" until "+hold.Until.UTC().Format(time.RFC3339))
	}
	sort.Strings(parts)
	return "Tasks from these repositories are paused for you because your agent could not push to your fork: " +
		strings.Join(parts, ", ") + ". To fix it, " + contributorRepoPushHoldFix + "."
}

// holdRepoIfForkPushFailed is the one entry point for both terminal reports:
// a task_failed, and a task_complete booked idle with no pull request. It
// classifies reason and applies it for c and the task's repository: a failed
// push with workflow-scope evidence books the hold at once; a refused push
// with no such evidence is a strike the first time and books the hold the
// second time within contributorRepoPushStrikeWindow. For a hold this call
// booked it logs it, records the decision and tells the relay in a `notice`
// frame, a type every relay already prints. It reports whether a hold was
// booked.
func (h *ContributeWSHub) holdRepoIfForkPushFailed(c *ContributorConnection, task *WSTaskAssign, reason string) bool {
	if h == nil || c == nil || c.profile == nil || task == nil || task.Repo == "" {
		return false
	}
	evidence := classifyContributorForkPushFailure(reason, task.Repo)
	if evidence == forkPushNoEvidence {
		return false
	}
	hold, outcome := h.recordContributorRepoPushFailure(c, task.Repo, reason, evidence == forkPushScopeBacked, time.Now())
	switch outcome {
	case repoPushNothing:
		return false
	case repoPushStrike:
		h.logger.Info("[contribute-ws] "+contributorRepoPushHoldReason+": first report of a refused push to the fork, with no workflow-scope evidence; no hold booked yet, a second report within "+contributorRepoPushStrikeWindow.String()+" books one",
			"username", hold.Username, "identity", hold.Identity,
			"repo", hold.Repo, "task", task.TaskID,
			"reason", normalizePushHoldReason(reason))
		return false
	}
	until := hold.Until.UTC().Format(time.RFC3339)
	// Info, not Warn: this is the hold working as designed, and it repeats
	// across contributors and repositories.
	h.logger.Info("[contribute-ws] "+contributorRepoPushHoldReason+": contributor could not push to their fork; repository withheld from this contributor",
		"username", hold.Username, "identity", hold.Identity,
		"repo", hold.Repo, "task", task.TaskID,
		"hold_count", hold.Count, "held_until", until,
		"second_report", outcome == repoPushHoldBookedOnSecond,
		"reason", hold.Reason)
	h.recordDecision(decisionUsername(c), decisionRefused, task.TaskID, task.Repo, task.Number,
		contributorRepoPushHoldReason+": the relay reported it could not push to its fork; hold "+
			strconv.Itoa(hold.Count)+" on "+hold.Repo+" until "+until)
	if c.ws != nil {
		notice := contributorRepoPushHoldNotice(hold, outcome == repoPushHoldBookedOnSecond)
		if err := c.send(WSMessage{Type: "notice", Seq: h.nextSeq(), Message: notice}); err != nil {
			h.logger.Warn("[contribute-ws] failed to send the push hold notice", "username", hold.Username, "error", err)
		}
	}
	return true
}

// ContributorRepoPushHoldView is one active hold as GET /api/contributors
// shows it, on the contributor it belongs to.
type ContributorRepoPushHoldView struct {
	Repo   string `json:"repo"`
	Reason string `json:"reason,omitempty"`
	Count  int    `json:"count"`
	HeldAt string `json:"held_at"`
	Until  string `json:"until"`
}

// RepoPushHolds returns the holds active at now, keyed by ContributorID and
// sorted by repository.
func (h *ContributeWSHub) RepoPushHolds(now time.Time) map[string][]ContributorRepoPushHoldView {
	if h == nil {
		return nil
	}
	out := make(map[string][]ContributorRepoPushHoldView)
	h.pushHoldMu.Lock()
	for _, rec := range h.repoPushHolds {
		if rec.Count <= 0 || rec.ContributorID == "" || !now.Before(rec.Until) {
			continue
		}
		out[rec.ContributorID] = append(out[rec.ContributorID], ContributorRepoPushHoldView{
			Repo:   rec.Repo,
			Reason: rec.Reason,
			Count:  rec.Count,
			HeldAt: rec.HeldAt.UTC().Format(time.RFC3339),
			Until:  rec.Until.UTC().Format(time.RFC3339),
		})
	}
	h.pushHoldMu.Unlock()
	for _, views := range out {
		sort.Slice(views, func(i, j int) bool { return views[i].Repo < views[j].Repo })
	}
	return out
}

// contributorRepoPushHoldsPath is where the ledger lives: the per-hub
// override, else the directory the no-PR streak ledger is written to.
func (h *ContributeWSHub) contributorRepoPushHoldsPath() string {
	if h != nil && h.repoPushHoldsFile != "" {
		return h.repoPushHoldsFile
	}
	return filepath.Join(filepath.Dir(h.noPRStreaksPath()), contributorRepoPushHoldsFileName)
}

func (h *ContributeWSHub) loadContributorRepoPushHolds() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.pushHoldMu.Lock()
	defer h.pushHoldMu.Unlock()
	data, err := os.ReadFile(h.contributorRepoPushHoldsPath())
	if err != nil {
		return
	}
	var records []contributorRepoPushHold
	if json.Unmarshal(data, &records) != nil {
		return
	}
	now := time.Now()
	for _, rec := range records {
		key := contributorRepoPushHoldKey(rec.Identity, rec.Repo)
		if key == "" || rec.forgotten(now) {
			continue
		}
		if h.repoPushHolds == nil {
			h.repoPushHolds = make(map[string]contributorRepoPushHold)
		}
		h.repoPushHolds[key] = rec
	}
	h.pruneContributorRepoPushHoldsLocked(now)
	h.logger.Info("[contribute-ws] loaded contributor repo push holds", "count", len(h.repoPushHolds))
}

func (h *ContributeWSHub) saveContributorRepoPushHolds() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.pushHoldSaveMu.Lock()
	defer h.pushHoldSaveMu.Unlock()
	now := time.Now()
	h.pushHoldMu.Lock()
	saved := make([]contributorRepoPushHold, 0, len(h.repoPushHolds))
	for _, rec := range h.repoPushHolds {
		if rec.forgotten(now) {
			continue
		}
		saved = append(saved, rec)
	}
	h.pushHoldMu.Unlock()
	sort.Slice(saved, func(i, j int) bool {
		if saved[i].Identity != saved[j].Identity {
			return saved[i].Identity < saved[j].Identity
		}
		return saved[i].Repo < saved[j].Repo
	})
	h.writeContributorHoldLedger(h.contributorRepoPushHoldsPath(), saved, "contributor repo push holds")
}

// writeContributorHoldLedger writes one of the contributor hold ledgers (this
// one and the quota holds) with the crash-safe idiom of the standby outcomes
// ledger (writeStandbyOutcomesFile: a unique temp file, synced and renamed
// into place). The caller holds that ledger's save mutex. A failure is logged
// and not fatal.
func (h *ContributeWSHub) writeContributorHoldLedger(path string, records any, what string) {
	data, err := json.Marshal(records)
	if err != nil {
		h.logger.Warn("[contribute-ws] "+what+" marshal failed", "error", err)
		return
	}
	if err := writeStandbyOutcomesFile(path, data); err != nil {
		h.logger.Warn("[contribute-ws] "+what+" persist failed", "error", err)
	}
}
