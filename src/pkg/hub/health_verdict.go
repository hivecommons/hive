package hub

import (
	"fmt"
	"strings"
	"time"
)

// Hive-health verdict: does this spoke have RECENT OUTPUT back to its work
// source, banded by ACMM level? Computed on read in handleMyHives from signals
// already on the registry entry — no new spoke work. The spine (operator's
// definition): a hive is HEALTHY when it has recent output for the level it is
// AT. Absence of output a level does not produce is NEVER a fault:
//
//	L1 Inception : no output at all      → preconditions only, never red for "no output"
//	L2 Advisory  : advisory digest       → healthy when advisory is fresh
//	L3–L5        : issues/PRs CREATED     → healthy when a create is recent; "not merged" is NOT a problem
//	L6           : creates AND merges     → healthy when a merge is recent (created-but-unmerged + queued = red)
//
// Recent output is only POSSIBLE when three preconditions hold — GitHub App
// green, ≥1 agent logged in, ≥1 agent running at cadence — so when output is
// absent those three explain WHY. A hive with an empty work queue can't produce
// and must not be faulted for it (the queue-gate). A hive we cannot see (old
// spoke / silent) is UNKNOWN, never green.

const (
	HealthStateGreen   = "green"
	HealthStateRed     = "red"
	HealthStateUnknown = "unknown"
)

// healthRecencyWindow is how recent output must be to count as "recent". The
// operator set 12h; the spoke advertises the same via RepoActivityWindowHours,
// but the hub owns the decision so a spoke can't widen it.
const healthRecencyWindow = 12 * time.Hour

// ACMM level bands. The registry stores a 0..6 integer; these name the
// operator's output tiers so the verdict reads in those terms.
const (
	acmmInceptionMax = 1 // ≤1 → Inception: no output expected
	acmmAdvisoryMax  = 2 // ==2 → Advisory: fresh advisory is the output
	acmmMergeMin     = 6 // ≥6 → also merges; below 6, unmerged PRs are fine
	// L3..L5 (create-but-don't-merge) is everything between advisory and merge.
)

// HealthVerdict is the at-a-glance answer for one hive, surfaced on MyHiveEntry.
type HealthVerdict struct {
	State string `json:"state"` // green | red | unknown
	// Reason is a short human phrase for the WHY chip ("last output 3h ago",
	// "GitHub App broken", "no agents running", "queue empty — idle").
	Reason string `json:"reason"`
	// OutputKind names what output this level is judged on: "advisory",
	// "creates", "merges", or "none" (Inception).
	OutputKind string `json:"outputKind"`
	// LastOutputAt is the newest relevant output timestamp (RFC3339), "" if none.
	LastOutputAt string `json:"lastOutputAt,omitempty"`
	// QueuedWork is the actionable backlog (issues+PRs) that output would drain;
	// zero means "nothing to do", which makes "no output" healthy.
	QueuedWork int `json:"queuedWork"`
}

// hiveHealthFor computes the verdict for one registry entry. rollup is the
// already-computed agent fleet rollup (Known/Running/Expected/Problems);
// queuedWork is ActionableIssues+ActionablePRs; app is the GitHub App health.
func hiveHealthFor(e RegistryEntry, rollup agentFleetRollup, app GitHubAppHealth, queuedWork int, now time.Time) HealthVerdict {
	v := HealthVerdict{QueuedWork: queuedWork}

	// --- Unknown gates first: never claim health for a hive we can't see. ---
	if !e.Online {
		v.State = HealthStateUnknown
		v.Reason = "not reporting (offline)"
		v.OutputKind = outputKindForLevel(e.ACMMLevel)
		return v
	}
	if rollup.Known == 0 {
		// Old spoke that predates the divergence signals — we cannot tell able
		// from stuck, so we cannot assert health.
		v.State = HealthStateUnknown
		v.Reason = "spoke too old to report health"
		v.OutputKind = outputKindForLevel(e.ACMMLevel)
		return v
	}

	v.OutputKind = outputKindForLevel(e.ACMMLevel)

	// --- Precondition reds: output is IMPOSSIBLE, so say why. These apply at
	// every level that is expected to produce anything (L2+). At L1 there is no
	// output to enable, so a precondition gap is not a health fault. ---
	if e.ACMMLevel > acmmInceptionMax {
		if app.Bucket == ghAppBucketBroken {
			v.State = HealthStateRed
			v.Reason = "GitHub App broken"
			return v
		}
		if rollup.Expected > 0 && rollup.Running == 0 {
			v.State = HealthStateRed
			v.Reason = "no agents running"
			return v
		}
		if rollup.Problems > 0 {
			v.State = HealthStateRed
			v.Reason = fmt.Sprintf("%d agent(s) blocked", rollup.Problems)
			return v
		}
	}

	// --- ACMM-banded output freshness. ---
	switch {
	case e.ACMMLevel <= acmmInceptionMax:
		// L1 Inception: no output is produced by design. Preconditions were the
		// only thing to check and (being L1) we don't even fault those. Green so
		// long as it is reporting — it is doing exactly what its level allows.
		v.State = HealthStateGreen
		v.Reason = "inception — no output expected"
		return v

	case e.ACMMLevel == acmmAdvisoryMax:
		// L2 Advisory: freshness of the advisory stream is the health signal.
		// Every agent contributes to the advisory stream, so if none are on
		// duty (all paused/off-schedule), staleness is caused by that — say so
		// instead of a bare "advisory stale" red. Only judged when the spoke
		// reports per-agent detail; older spokes send none, and absence of
		// detail must not read as absence of agents.
		if len(e.Agents) > 0 {
			if roster := grantRosterFor(e.Agents, func(AgentSummary) bool { return true }); len(roster.onDuty) == 0 {
				return noWritersOnDuty(v, "advisory", roster)
			}
		}
		// Reuse the existing advisory/issue-activity bucketing rather than the
		// per-repo activity collector.
		adv := advisoryIssueActivityFor(e, now)
		v.LastOutputAt = adv.LastActivityAt
		switch adv.Bucket {
		case advisoryIssueBucketFresh, advisoryIssueBucketAging:
			v.State = HealthStateGreen
			v.Reason = "advisory " + advisoryAge(adv, now)
		case advisoryIssueBucketStale:
			v.State = HealthStateRed
			v.Reason = "advisory stale"
		default: // unknown
			if queuedWork == 0 {
				v.State = HealthStateGreen
				v.Reason = "queue empty — idle"
			} else {
				v.State = HealthStateUnknown
				v.Reason = "no advisory yet"
			}
		}
		return v

	case e.ACMMLevel >= acmmMergeMin:
		// L6: judged on MERGES. A create that never merges, with work still
		// queued, is the failure this level exists to catch.
		if roster := grantRosterFor(e.Agents, func(a AgentSummary) bool { return a.CanMerge }); len(roster.onDuty) == 0 {
			return noWritersOnDuty(v, "merge", roster)
		}
		last, ok := newestOutput(e.RepoActivity, func(r RepoActivityWire) string { return r.Merges.NewestAt })
		return bandFreshness(v, last, ok, queuedWork, now, "merge")

	default:
		// L3–L5: judged on authored WRITES to the work source — issue/PR
		// creates, comments, and reviews. Merges are the human's job here, so
		// an unmerged PR is not a fault — only a fully stalled write stream
		// (with work queued) is. Comments/reviews count because an agent
		// triaging a backlog (dismissing false positives, reviewing held PRs)
		// is producing real output even on a kick that creates nothing; the
		// operator: "if it's writing comments then that is output".
		roster := grantRosterFor(e.Agents, func(a AgentSummary) bool { return a.CanOpenIssue || a.CanOpenPR })
		if len(roster.onDuty) == 0 {
			return noWritersOnDuty(v, "create", roster)
		}
		last, ok := newestOutput(e.RepoActivity, func(r RepoActivityWire) string {
			return maxRFC3339(maxRFC3339(r.Issues.NewestAt, r.PRs.NewestAt),
				maxRFC3339(r.Comments.NewestAt, r.Reviews.NewestAt))
		})
		return bandFreshness(v, last, ok, queuedWork, now, "write")
	}
}

// grantRoster splits a hive's agents holding a write grant into the ones the
// governor currently expects to work and the ones that are off (paused or
// off-schedule). The off list carries names so a verdict caused by "the
// responsible agent is off" can SAY so — the operator's ask: when the agent
// responsible for creating/commenting/merging is off and that is the reason,
// indicate the correlation.
type grantRoster struct {
	onDuty []string
	off    []string
}

func grantRosterFor(agents []AgentSummary, grant func(AgentSummary) bool) grantRoster {
	var r grantRoster
	for _, a := range agents {
		if !grant(a) {
			continue
		}
		// Off duty = operator-paused (Paused flag or paused state — paused
		// agents still report ExpectedActive true, the governor keeps them on
		// the schedule) or off-schedule (!ExpectedActive).
		if a.ExpectedActive && !a.Paused && !strings.EqualFold(a.State, agentStatePaused) {
			r.onDuty = append(r.onDuty, a.Name)
		} else {
			r.off = append(r.off, a.Name)
		}
	}
	return r
}

// nameList renders up to three agent names, then "+N more".
func nameList(names []string) string {
	if len(names) <= 3 {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:3], ", ") + fmt.Sprintf(" +%d more", len(names)-3)
}

// noWritersOnDuty is the verdict when no on-duty agent holds the write grant
// the level is judged on (live case: a QUIET-mode L3 hive whose only running
// agent had no issue/PR grants read "no create output" red — but a hive that
// CANNOT write by mode/pause configuration is quiet by design, not failing;
// same spine as L1 "no output expected"). Absence of output the configuration
// does not permit is never a fault. When grant-holding agents EXIST but are
// paused/off-schedule, the reason names them — that correlation ("the agent
// responsible for this output is off") is the answer to the operator's next
// question, so put it in the chip instead of making them hunt the agent rows.
func noWritersOnDuty(v HealthVerdict, verb string, roster grantRoster) HealthVerdict {
	v.State = HealthStateGreen
	if len(roster.off) > 0 {
		v.Reason = fmt.Sprintf("no output expected — %s-capable agent(s) off: %s", verb, nameList(roster.off))
	} else {
		v.Reason = fmt.Sprintf("no %s-capable agent configured — no output expected", verb)
	}
	return v
}

// bandFreshness applies the recency window to a create/merge output stream:
// recent → green; nothing/old but queue empty → green (nothing to do); old with
// work queued → red. verb is "create" or "merge" for the reason phrase.
func bandFreshness(v HealthVerdict, last time.Time, ok bool, queuedWork int, now time.Time, verb string) HealthVerdict {
	if ok {
		v.LastOutputAt = last.UTC().Format(time.RFC3339)
	}
	switch {
	case ok && now.Sub(last) <= healthRecencyWindow:
		v.State = HealthStateGreen
		v.Reason = fmt.Sprintf("last %s %s", verb, humanizeAge(now.Sub(last)))
	case queuedWork == 0:
		// No output, but nothing to produce — idle, not broken.
		v.State = HealthStateGreen
		if ok {
			v.Reason = "queue empty — idle"
		} else {
			v.Reason = "queue empty — no output yet"
		}
	case ok:
		v.State = HealthStateRed
		// TrimSuffix: humanizeAge says "18h ago" for badge use; "no create in
		// 18h ago" is not English, so drop the suffix here.
		v.Reason = fmt.Sprintf("no %s in %s (%d queued)", verb, strings.TrimSuffix(humanizeAge(now.Sub(last)), " ago"), queuedWork)
	default:
		v.State = HealthStateRed
		v.Reason = fmt.Sprintf("no %s output (%d queued)", verb, queuedWork)
	}
	return v
}

// outputKindForLevel names the output a level is judged on, for the UI.
func outputKindForLevel(level int) string {
	switch {
	case level <= acmmInceptionMax:
		return "none"
	case level == acmmAdvisoryMax:
		return "advisory"
	case level >= acmmMergeMin:
		return "merges"
	default:
		return "creates"
	}
}

// newestOutput returns the newest parseable timestamp produced by pick across
// all repos, and whether any was found.
func newestOutput(repos []RepoActivityWire, pick func(RepoActivityWire) string) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, r := range repos {
		if t, ok := parseRFC3339(pick(r)); ok {
			if !found || t.After(newest) {
				newest, found = t, true
			}
		}
	}
	return newest, found
}

// maxRFC3339 returns the later of two RFC3339 strings (lexical compare is valid
// for canonical UTC RFC3339); a non-empty value beats an empty one.
func maxRFC3339(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	if a >= b {
		return a
	}
	return b
}

// advisoryAge renders a short age phrase for the advisory reason chip.
// humanizeAge already carries the "ago" suffix, so nothing is appended here
// (appending produced "advisory 3h ago ago").
func advisoryAge(adv AdvisoryIssueActivity, now time.Time) string {
	if t, ok := parseRFC3339(adv.LastActivityAt); ok {
		return humanizeAge(now.Sub(t))
	}
	return "fresh"
}

// humanizeAge renders a duration as a compact "Nm"/"Nh"/"Nd" phrase. Negative
// (clock skew) is treated as just-now.
func humanizeAge(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}
