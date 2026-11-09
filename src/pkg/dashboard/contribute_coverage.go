package dashboard

import (
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/governor"
)

// RoleCoverage reports which connected contributor relays are acting as which
// agent role right now, keyed by role, for the governor's contributor-first
// gate (`agents.<name>.contributor_first`; see pkg/governor/contributor_first.go).
//
// A relay counts as covering a role when ALL of these hold:
//
//   - its EFFECTIVE role is that role — the owner's "Acting as" assignment
//     after it has overridden whatever the relay requested, which is what
//     c.role already holds once the hub admitted the claim;
//   - its heartbeat is fresh, by the same wsHeartbeatTimeout rule
//     FleetSnapshot uses to mark a relay stale, so a relay whose socket died
//     silently cannot keep the resident agent idle;
//   - it is signed in as a contributor with a GitHub login;
//   - the operator has not suspended contributions (hub.contribute_suspended).
//     The suspend switch refuses work to every relay while their sockets stay
//     up, so a connected relay is then covering nothing; reporting it as
//     coverage would turn "pause contributors" into "pause the lane";
//   - it has not declared standby (standby_declare, accepted or not). A
//     standby relay stays connected but never asks for ordinary work; it only
//     takes what an owner dispatches to it by hand, so it drains nothing;
//   - it has not reported a quota hold (quota_hold, capability
//     quota_hold_v1). A relay whose quota guard is holding withholds `ready`
//     and declines pushed work, so it is connected and covering nothing.
//     Observed on 2026-11-08: a relay held at 15% of its weekly window kept
//     the scanner lane "covered" while nobody worked it. A relay too old to
//     send quota_hold is still counted, as before;
//   - it has not reported that it is busy on another hive (busy_elsewhere,
//     capability busy_elsewhere_v1) while holding no task from this one. A
//     relay subscribed to several hives works one task at a time; while that
//     task belongs to another hive it heartbeats and keeps its role here but
//     asks this hub for nothing. Observed on 2026-11-09: a relay set "Acting
//     as: scanner" on one hive spent the next seven minutes on another hive's
//     tasks and sent the first hive no ready. Its next ready here clears the
//     report (see busyElsewhereLocked). A relay too old to send busy_elsewhere
//     is still counted, as before;
//   - it is not in a failure-streak pause: a relay whose runtime keeps dying
//     is connected but is not doing the lane's work;
//   - its identity has not used up its hourly or daily task allowance
//     (hub.tier_limits max_per_hour / max_per_day for its trust tier). Task
//     selection refuses such a relay with hourly_limit / daily_limit until the
//     window rolls, so it takes nothing in the meantime. The check is
//     tierRateLimitReached, the same function selection refuses with, so the
//     two can never disagree about which limits apply or how they are counted;
//   - its profile is not revoked and its trust tier is not in
//     hub.disabled_tiers. The check is tierAccessGateReason, the rule a resume
//     is refused with and whose disabled-tier half (tierDisabled) refuses a
//     fresh assignment, so a relay the hub will not hand work to is never
//     reported as working the lane;
//   - it has not been answered no_matching_work (or repo_unmintable, its
//     re-scan form) continuously for coverageNoWorkWindow while holding no
//     task. Such a relay is connected and idle: the queue has nothing it may
//     take. Observed on 2026-11-08: a relay covered the scanner lane while
//     every ready it sent was answered no_matching_work. One empty answer
//     between two tasks does not drop it (see coverageNoWorkWindow).
//
// A relay that is busy on a task still counts — it is draining the lane,
// which is the point. That includes a relay working only a fix-pr task, and a
// relay whose last answer was no_matching_work but which has been handed work
// since. Standby, quota hold, a used-up allowance and a closed tier do not
// wait for the task to end: each means the relay will not take the lane's NEXT
// item, which is what the governor is deciding whether to withhold. (Holding
// max_concurrent tasks is different: that relay is busy, and counts. So is a
// relay busy on another hive that this hub has also handed a task or a fix-pr
// task: it is working this lane too.) Whether
// the relay is ALLOWED the role (trust tier, grants, the delegatable list) was
// settled when the hub set c.role; nothing is re-derived here.
func (h *ContributeWSHub) RoleCoverage(now time.Time) map[string]governor.ContributorCoverage {
	if h == nil || h.contributionsSuspended() {
		return nil
	}
	type candidate struct {
		role, login, identity, tier string
	}
	var candidates []candidate
	h.mu.RLock()
	for _, c := range h.connections {
		if c == nil {
			continue
		}
		c.mu.Lock()
		role := normalizeAgentRole(c.role)
		fresh := now.Sub(c.lastPong) <= wsHeartbeatTimeout
		login, tier := "", ""
		if c.profile != nil {
			login = strings.TrimSpace(c.profile.GitHubUsername)
			tier = c.profile.TrustTier
		}
		notTakingWork := c.standbyDeclared || len(c.standby) > 0 || c.quotaHeld || c.busyElsewhereLocked()
		queueEmpty := c.noWorkForWindowLocked(now)
		c.mu.Unlock()
		if role == "" || !fresh || login == "" || notTakingWork || queueEmpty {
			continue
		}
		candidates = append(candidates, candidate{role: role, login: login, identity: identityOf(c), tier: tier})
	}
	h.mu.RUnlock()

	// The failure-streak ledger and the assignment-rate windows each have
	// their own lock (failureStreakMu, rateMu); consult them with h.mu
	// released so neither is ever nested inside it. The rate check is a scan
	// of at most a day of assignment stamps for one identity, once per
	// candidate per governor evaluation, and never takes selectMu. The tier
	// gate reads only the config, from the tier copied out under c.mu above.
	byRole := make(map[string]map[string]struct{})
	for _, cand := range candidates {
		if h.tierAccessGateReason(cand.tier) != "" {
			continue
		}
		if paused, _, _ := h.contributorFailureStreakActive(cand.identity, now); paused {
			continue
		}
		if h.tierRateLimitReached(cand.identity, cand.tier, now).Reason != "" {
			continue
		}
		if byRole[cand.role] == nil {
			byRole[cand.role] = make(map[string]struct{})
		}
		byRole[cand.role][cand.login] = struct{}{}
	}
	if len(byRole) == 0 {
		return nil
	}
	out := make(map[string]governor.ContributorCoverage, len(byRole))
	for role, logins := range byRole {
		list := make([]string, 0, len(logins))
		for login := range logins {
			list = append(list, login)
		}
		sort.Strings(list)
		out[role] = governor.ContributorCoverage{Contributors: list}
	}
	return out
}

// coverageNoWorkWindow is how long a relay holding no task must have been
// answered nothing but no_matching_work / repo_unmintable before RoleCoverage
// stops counting it.
//
// Two minutes, measured from the FIRST answer of the run. An idle relay asks
// again 30 s after each task_unavailable (TASK_UNAVAILABLE_RETRY_MS in
// bin/contributor-relay.js), so that is five empty answers in a row: one
// empty answer between two tasks, with work arriving on the next ask, never
// reaches it. It is a time, not an answer count, so a relay that asks less
// often (or stops asking) is judged on how long it has been idle, not on how
// many times it happened to ask. It does not need to be longer to stop the
// resident agent flapping: RoleCoverage is sampled once per governor
// evaluation (governor.eval_interval_s, 300 s by default, sooner on webhook
// triggers), and a role that drops out of the report is still withheld for
// contributor_first_grace (10 min by default) before the resident agent is
// kicked, so a relay that finds work again inside the grace keeps the lane.
const coverageNoWorkWindow = 2 * time.Minute

// idleRunKey folds the task_unavailable reasons that mean "nothing in the queue
// for this relay" into one run: the re-scan wrapper in selectTask renames
// no_matching_work to repo_unmintable when it dropped an unmintable repo on the
// way, so the two can alternate on consecutive readies with nothing changing.
func idleRunKey(reason string) string {
	if reason == taskUnavailableRepoUnmintable {
		return taskUnavailableNoMatchingWork
	}
	return reason
}

// noteIdleReasonLocked records reason as the task_unavailable answer just sent
// to c, starting a new run unless it continues the current one. The caller
// holds c.mu.
func (c *ContributorConnection) noteIdleReasonLocked(reason string, now time.Time) {
	if c.lastIdleReasonSince.IsZero() || idleRunKey(reason) != idleRunKey(c.lastIdleReason) {
		c.lastIdleReasonSince = now
	}
	c.lastIdleReason = reason
}

// clearIdleReasonLocked ends the idle run: c has been handed work. The caller
// holds c.mu.
func (c *ContributorConnection) clearIdleReasonLocked() {
	c.lastIdleReason = ""
	c.lastIdleReasonSince = time.Time{}
}

// noWorkForWindowLocked reports whether c holds no task (primary or fix-pr)
// and has been answered no_matching_work / repo_unmintable continuously for at
// least coverageNoWorkWindow at now. The caller holds c.mu.
func (c *ContributorConnection) noWorkForWindowLocked(now time.Time) bool {
	if c.currentTask != nil || len(c.fixTasks) > 0 {
		return false
	}
	if c.lastIdleReasonSince.IsZero() || idleRunKey(c.lastIdleReason) != taskUnavailableNoMatchingWork {
		return false
	}
	return now.Sub(c.lastIdleReasonSince) >= coverageNoWorkWindow
}

// busyElsewhereLocked reports whether c has said it is working another hive's
// task (busy_elsewhere) and this hub has handed it no task of its own, primary
// or fix-pr. The caller holds c.mu.
//
// There is no wall-clock expiry. The flag is per connection, so a socket close
// or a relay restart drops it with the connection; the relay clears it when the
// other task ends, and the relay's next ready here clears it too (handleReady).
// A timeout would only re-count a relay that is still busy on a long task —
// the bug this check exists to stop — while a flag left set by mistake errs
// the safe way: the resident agent works the lane, as it would without
// contributor_first.
func (c *ContributorConnection) busyElsewhereLocked() bool {
	return c.busyElsewhere && c.currentTask == nil && len(c.fixTasks) == 0
}

// contributionsSuspended mirrors the task-selection gate in
// contribute_select.go: with hub.contribute_suspended set, no relay is handed
// work, whatever role it holds.
func (h *ContributeWSHub) contributionsSuspended() bool {
	return h != nil && h.server != nil && h.server.deps != nil && h.server.deps.Config != nil &&
		h.server.deps.Config.Hub.ContributeSuspended
}

// ContributorRoleCoverage is the eval cycle's entry point to RoleCoverage. It
// is nil-safe on both the server and the hub: a hive without the contribute
// hub reports no coverage, and the governor treats nil as "nobody is covering
// anything".
func (s *Server) ContributorRoleCoverage(now time.Time) map[string]governor.ContributorCoverage {
	if s == nil || s.contributeHub == nil {
		return nil
	}
	return s.contributeHub.RoleCoverage(now)
}

// frontendLaneCoverage renders the governor's covered-lane map for the status
// payload, timestamps as RFC 3339 strings like the other FrontendGovernor
// times.
func frontendLaneCoverage(in map[string]governor.LaneCoverage) map[string]FrontendLaneCoverage {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]FrontendLaneCoverage, len(in))
	for lane, cov := range in {
		fc := FrontendLaneCoverage{Contributors: append([]string(nil), cov.Contributors...)}
		if !cov.Since.IsZero() {
			fc.Since = cov.Since.UTC().Format(time.RFC3339)
		}
		if !cov.LastSeen.IsZero() {
			fc.LastSeen = cov.LastSeen.UTC().Format(time.RFC3339)
		}
		if !cov.GraceUntil.IsZero() {
			fc.GraceUntil = cov.GraceUntil.UTC().Format(time.RFC3339)
		}
		out[lane] = fc
	}
	return out
}
