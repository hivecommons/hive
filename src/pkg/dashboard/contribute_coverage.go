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
// A relay counts as covering a role when:
//
//   - its EFFECTIVE role is that role — the owner's "Acting as" assignment
//     after it has overridden whatever the relay requested, which is what
//     c.role already holds once the hub admitted the claim;
//   - its heartbeat is fresh, by the same wsHeartbeatTimeout rule
//     FleetSnapshot uses to mark a relay stale, so a relay whose socket died
//     silently cannot keep the resident agent idle;
//   - it is not in a failure-streak pause: a relay whose runtime keeps dying
//     is connected but is not doing the lane's work;
//   - it has not declared standby (standby_declare, accepted or not). A
//     standby relay stays connected but never asks for ordinary work; it only
//     takes what an owner dispatches to it by hand, so it drains nothing;
//   - it has not reported a quota hold (quota_hold, capability
//     quota_hold_v1). A relay whose quota guard is holding withholds `ready`
//     and declines pushed work, so it is connected and covering nothing.
//     Observed on 2026-11-08: a relay held at 15% of its weekly window kept
//     the scanner lane "covered" while nobody worked it. A relay too old to
//     send quota_hold is still counted, as before;
//   - its identity has not used up its hourly or daily task allowance
//     (hub.tier_limits max_per_hour / max_per_day for its trust tier). Task
//     selection refuses such a relay with hourly_limit / daily_limit until the
//     window rolls, so it takes nothing in the meantime. The check is
//     tierRateLimitReached, the same function selection refuses with, so the
//     two can never disagree about which limits apply or how they are counted;
//   - the operator has not suspended contributions (hub.contribute_suspended).
//     The suspend switch refuses work to every relay while their sockets stay
//     up, so a connected relay is then covering nothing; reporting it as
//     coverage would turn "pause contributors" into "pause the lane".
//
// A relay that is busy on a task still counts — it is draining the lane,
// which is the point. Standby, quota hold and a used-up allowance do not wait
// for the task to end: each means the relay will not take the lane's NEXT
// item, which is what the governor is deciding whether to withhold. (Holding
// max_concurrent tasks is different: that relay is busy, and counts.) Whether the relay is ALLOWED the role (trust tier,
// grants, the delegatable list) was settled when the hub set c.role; nothing
// is re-derived here.
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
		notTakingWork := c.standbyDeclared || len(c.standby) > 0 || c.quotaHeld
		c.mu.Unlock()
		if role == "" || !fresh || login == "" || notTakingWork {
			continue
		}
		candidates = append(candidates, candidate{role: role, login: login, identity: identityOf(c), tier: tier})
	}
	h.mu.RUnlock()

	// The failure-streak ledger and the assignment-rate windows each have
	// their own lock (failureStreakMu, rateMu); consult them with h.mu
	// released so neither is ever nested inside it. The rate check is a scan
	// of at most a day of assignment stamps for one identity, once per
	// candidate per governor evaluation, and never takes selectMu.
	byRole := make(map[string]map[string]struct{})
	for _, cand := range candidates {
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
