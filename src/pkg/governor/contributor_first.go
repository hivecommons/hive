package governor

import (
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Contributor-first lanes (`agents.<name>.contributor_first`).
//
// A contributor relay acting as an agent role — "Acting as" on the contributor
// card, or HIVE_AGENT_ROLE on the relay — does that role's work on the
// contributor's own machine, with the contributor's own model. Until now it
// did so IN PARALLEL with the hive's resident agent: the governor had no idea a
// relay was connected, kept kicking the resident agent on cadence, and so the
// hive's own inference budget was spent on work a contributor was already
// covering. Standby (RFC #7629) is the other way round on purpose: it offers
// paused BUDGET work to volunteers and never fires before the budget is gone.
//
// contributor_first inverts the order for one lane. While at least one
// connected, non-stale, non-failure-paused relay is acting as the role (and
// the operator has not suspended contributions), the
// governor WITHHOLDS the resident agent's kicks — at exactly the gates where it
// withholds them for an exhausted budget. The relay is the primary worker; the
// resident agent is the fallback, and it resumes on its normal cadence once no
// relay has covered the role for contributor_first_grace. The grace exists
// because a relay reconnect is routine (#4260 keeps a dropped relay's lease for
// thirty minutes); without it the resident agent would flap on and off with
// every socket drop.
//
// What this deliberately does NOT touch: the budget gate and SuppressedLanes
// (a covered lane is never reported as paused for budget, so standby cannot
// dispatch on it), the contributor path's own admission rules (trust tier,
// grants, allow-lists), and which issues a relay may take.

// ContributorCoverage is the contribute hub's per-tick report that connected
// contributor relays are acting as an agent role right now. The hub owns the
// definition of "acting as": the connection's EFFECTIVE role, after an owner
// assignment has overridden whatever the relay requested.
type ContributorCoverage struct {
	// Contributors are the GitHub logins of the covering relays.
	Contributors []string
}

// LaneCoverage is what the governor publishes for a contributor-first lane it
// is currently withholding kicks for.
type LaneCoverage struct {
	// Contributors are the logins covering the lane right now. Empty while
	// the grace period runs after the last relay disappeared.
	Contributors []string `json:"contributors,omitempty"`
	// Since is when coverage began.
	Since time.Time `json:"since"`
	// LastSeen is the last report in which a relay covered the lane.
	LastSeen time.Time `json:"last_seen"`
	// GraceUntil is when the resident agent resumes if no relay returns. Zero
	// while a relay is covering the lane.
	GraceUntil time.Time `json:"grace_until,omitempty"`
}

// contributorCoveringPauseReason is the machine-readable reason a
// contributor-first lane is not being kicked. It is published through
// State.CoveredLanes and used as the continuous-mode blocker string. It is
// deliberately NOT written into SuppressedLanes or LanePauseReasons, which
// stay budget-only so that standby dispatch keeps its "paused for budget"
// meaning.
const contributorCoveringPauseReason = "contributor_covering"

// ResumeRefusalContributorCovering: a contributor relay is covering the agent's
// role and contributor_first is set, so a crash-recovery resume kick is
// withheld exactly as a scheduled kick is.
const ResumeRefusalContributorCovering ResumeRefusal = "contributor_covering"

// coverageRecord is the governor's memory of one role's coverage.
type coverageRecord struct {
	contributors []string
	since        time.Time
	lastSeen     time.Time
}

// SetContributorCoverage replaces the governor's view of which roles connected
// contributor relays are covering. The eval cycle calls it once per tick,
// before Evaluate, with the hub's current report; nil or empty means nobody is
// covering anything. A role missing from the report keeps its record until its
// grace has elapsed — that is what lets a relay reconnect without the resident
// agent being kicked in between.
//
// Coverage is remembered for every role the hub reports, whether or not the
// agent has contributor_first set, so switching the flag on takes effect on
// the next evaluation rather than after the next relay heartbeat. The gate
// itself (contributorCoveredLocked) is the only place the flag is read.
func (g *Governor) SetContributorCoverage(coverage map[string]ContributorCoverage) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.coverage == nil {
		g.coverage = make(map[string]coverageRecord)
	}

	reported := make(map[string]struct{}, len(coverage))
	for role, cov := range coverage {
		role = strings.ToLower(strings.TrimSpace(role))
		logins := normalizeContributorLogins(cov.Contributors)
		if role == "" || len(logins) == 0 {
			continue
		}
		reported[role] = struct{}{}
		rec, known := g.coverage[role]
		if !known {
			rec = coverageRecord{since: now}
			if g.contributorFirstLocked(role) && g.logger != nil {
				g.logger.Info("contributor relay covering a contributor-first lane — withholding the resident agent's kicks",
					"agent", role, "contributors", strings.Join(logins, ","))
			}
		} else if len(rec.contributors) == 0 && g.contributorFirstLocked(role) && g.logger != nil {
			// Back within the grace window: coverage never really ended.
			g.logger.Info("contributor relay returned within grace — resident agent stays withheld",
				"agent", role, "contributors", strings.Join(logins, ","))
		}
		rec.contributors = logins
		rec.lastSeen = now
		g.coverage[role] = rec
	}

	for role, rec := range g.coverage {
		if _, ok := reported[role]; ok {
			continue
		}
		flagged := g.contributorFirstLocked(role)
		if len(rec.contributors) > 0 {
			// The last relay just disappeared: start the grace.
			rec.contributors = nil
			g.coverage[role] = rec
			if flagged && g.logger != nil {
				g.logger.Info("contributor cover ended — resident agent resumes after grace unless a relay returns",
					"agent", role, "grace", g.contributorFirstGraceLocked(role).String())
			}
			continue
		}
		if !now.Before(rec.lastSeen.Add(g.contributorFirstGraceLocked(role))) {
			delete(g.coverage, role)
			if flagged && g.logger != nil {
				g.logger.Info("contributor-first grace elapsed — resident agent kicks resume", "agent", role)
			}
		}
	}
}

// contributorCoveredLocked reports whether the named agent's kicks are
// withheld because a contributor relay is covering its role: the agent has
// contributor_first set, and either a relay covers the role right now or the
// last one disappeared less than contributor_first_grace ago. Callers must
// hold g.mu.
func (g *Governor) contributorCoveredLocked(agentName string) (LaneCoverage, bool) {
	ac, ok := g.agents[agentName]
	if !ok || !ac.ContributorFirst {
		return LaneCoverage{}, false
	}
	rec, ok := g.coverage[strings.ToLower(strings.TrimSpace(agentName))]
	if !ok {
		return LaneCoverage{}, false
	}
	if len(rec.contributors) > 0 {
		return LaneCoverage{
			Contributors: append([]string(nil), rec.contributors...),
			Since:        rec.since,
			LastSeen:     rec.lastSeen,
		}, true
	}
	until := rec.lastSeen.Add(ac.ContributorFirstGraceOrDefault())
	if g.now().Before(until) {
		return LaneCoverage{Since: rec.since, LastSeen: rec.lastSeen, GraceUntil: until}, true
	}
	return LaneCoverage{}, false
}

// coveredLanesLocked is the per-evaluation publication: every contributor-first
// agent whose kicks are currently withheld, with who is covering it. Callers
// must hold g.mu.
func (g *Governor) coveredLanesLocked() map[string]LaneCoverage {
	var out map[string]LaneCoverage
	for name := range g.agents {
		cov, ok := g.contributorCoveredLocked(name)
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]LaneCoverage)
		}
		out[name] = cov
	}
	return out
}

func (g *Governor) contributorFirstLocked(role string) bool {
	ac, ok := g.agents[role]
	return ok && ac.ContributorFirst
}

func (g *Governor) contributorFirstGraceLocked(role string) time.Duration {
	if ac, ok := g.agents[role]; ok {
		return ac.ContributorFirstGraceOrDefault()
	}
	return config.DefaultContributorFirstGrace
}

// normalizeContributorLogins trims, drops empties, de-duplicates and sorts, so
// the published list is stable across ticks and map iteration order.
func normalizeContributorLogins(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, login := range in {
		login = strings.TrimSpace(login)
		if login == "" {
			continue
		}
		if _, dup := seen[login]; dup {
			continue
		}
		seen[login] = struct{}{}
		out = append(out, login)
	}
	sort.Strings(out)
	return out
}

func cloneLaneCoverage(in map[string]LaneCoverage) map[string]LaneCoverage {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]LaneCoverage, len(in))
	for lane, cov := range in {
		cov.Contributors = append([]string(nil), cov.Contributors...)
		out[lane] = cov
	}
	return out
}
