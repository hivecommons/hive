package hub

import "strings"

// Placeholder (unassigned pool) rows on the My Hives dashboard.
//
// An UNCLAIMED pool slot has no GitHub auth BY DESIGN — credentials only
// arrive when a project is claimed — so the spoke's auth-class health checks
// (github_auth, and any github_app* variant) legitimately fail on every
// placeholder. Left alone, that painted the whole "Unassigned Hives" section
// red: every row carried a degraded dot, a warning icon and a "2 checks
// failing" pill, burying the one placeholder that is genuinely broken. The
// maintainer's rule: if unassigned, that is noted in the status, and the
// hive's overall status is green.
//
// sanitizePlaceholderRows therefore rewrites each placeholder's spoke-reported
// Health payload before it ships to the browser: checks whose non-passing
// state IS the designed state of an unassigned slot — the auth-class family;
// the tokens check's zero-consumed warning (no project means no workload to
// spend on); and the agents check when it fails ONLY because agents await the
// owner's CLI login (see placeholderDesignedCheckNote and
// agentsFailIsLoginPending) — are reclassified as "skip" with a note saying why
// as their detail, and the overall status / fails / warns are recomputed from
// the surviving checks using the spoke's own thresholds. Genuine
// placeholder-applicable failures — not ready, agents CRASH-LOOPING (a "down:"
// agent, distinct from a login-pending one), queue wedged — keep their status
// untouched and still turn the row red, exactly as drift.go's rule 2 intends
// ("never flag a placeholder for a claimed-hive concern", and never hide a real
// one).
//
// The agents distinction is why the sanitizer inspects a check's detail, not
// just its name: on an unclaimed slot, agents "0 running / paused / need login"
// is the by-design unclaimed state (no owner has claimed the slot or run the
// CLI login yet) — the exact analogue of github_auth being unconfigured by
// design. But an agent that is genuinely "down:" (crash-looping / exiting) is a
// real fault even on a placeholder, so only the login-pending shape is exempted;
// anything ambiguous stays degrading.
//
// Which rows count as placeholders is decided by isPlaceholderEntry
// (drift.go), the SAME predicate computeDrift and the alert evaluator use, so
// the row dot, the drift badge and the Attention Needed panel can never
// disagree about what an unassigned hive is.

// placeholderAuthNote is the status-hover text that replaces an auth-class
// failure's error detail on an unassigned row, so the hover says WHY the check
// is not evaluated instead of presenting a designed state as a fault.
const placeholderAuthNote = "Unassigned — auth not configured by design"

// healthCheckTokens is the spoke's token-consumption check (HealthSummary,
// pkg/dashboard/server.go). It warns with "zero consumed" whenever the hive
// has spent nothing — which on an unassigned pool slot is not a warning at
// all: a slot with no project has no workload to spend tokens on.
const healthCheckTokens = "tokens"

// placeholderTokensNote replaces the tokens check's "zero consumed" warning
// detail on an unassigned row, for the same reason as placeholderAuthNote.
const placeholderTokensNote = "Unassigned — no workload by design"

// healthCheckAgents is the spoke's agent-liveness check (HealthSummary,
// pkg/dashboard/server.go). It fails with a detail line like
// "0 running, 1 paused, 4 need login: guide, quality, scanner, supervisor"
// when agents are not doing work.
const healthCheckAgents = "agents"

// placeholderAgentsNote replaces the agents check's login-pending failure
// detail on an unassigned row. On an UNCLAIMED slot, agents that are "0
// running / paused / need login" are not broken — no owner has claimed the
// slot or run the CLI login yet, so their idleness is the designed state,
// exactly like github_auth being unconfigured by design.
const placeholderAgentsNote = "Unassigned — agents await the owner's CLI login by design"

// Agents-check detail markers. The spoke's HealthSummary composes the agents
// detail line from named buckets (pkg/dashboard/server.go): "need login" for
// an agent whose CLI is simply unauthenticated (owner-actionable), and "down:"
// for an agent that is genuinely not running for any other reason
// (crash-looping / exiting). We discriminate on these substrings rather than
// re-deriving agent state the hub does not have.
const (
	// agentsDetailNeedLogin marks the login-pending bucket, whose detail reads
	// "N need login: ..." (plural) or "1 needs login: ..." (singular). Both end
	// in "login:", so matching that colon-terminated suffix catches both spellings
	// without also matching the "down:" bucket.
	agentsDetailNeedLogin = "login:"
	// agentsDetailDown marks the crash/exit bucket ("N down: ..."). Its presence
	// means at least one agent is genuinely broken, so the fail is NOT by-design
	// even on a placeholder.
	agentsDetailDown = "down:"
)

// agentsFailIsLoginPending reports whether a failing agents check is failing
// ONLY because agents await the owner's CLI login — the by-design unclaimed
// state of a pool slot — and not because any agent is genuinely down
// (crash-looping / exiting). It requires positive evidence of the login-pending
// bucket AND the absence of the "down:" crash bucket: a "down:" agent is a real
// fault even on a placeholder, and a detail with neither marker (an unrecognised
// shape) is treated as ambiguous and left degrading.
func agentsFailIsLoginPending(detail string) bool {
	return strings.Contains(detail, agentsDetailNeedLogin) &&
		!strings.Contains(detail, agentsDetailDown)
}

// placeholderDesignedCheckNote reports whether a check's non-passing state is
// the DESIGNED state of an unassigned pool slot, and if so returns the hover
// note that replaces its error detail. It takes the check's detail because the
// agents check is designed-state ONLY when its failure is login-pending
// (agentsFailIsLoginPending); a crash-looping ("down:") agents check is a
// genuine fault even on a placeholder. Everything else returns "" and keeps its
// reported status: ready, stall_detection, template_vars, kick_refusal and
// queue failures are genuine faults even on a placeholder (it runs a real spoke
// process), and governor/contribute only ever report pass.
func placeholderDesignedCheckNote(name, detail string) string {
	switch {
	case isAuthClassHealthCheck(name):
		return placeholderAuthNote
	case name == healthCheckTokens:
		return placeholderTokensNote
	case name == healthCheckAgents && agentsFailIsLoginPending(detail):
		return placeholderAgentsNote
	}
	return ""
}

const (
	// healthCheckStatusSkip is the spoke's own not-applicable check status
	// (rendered as a muted dash by the dashboard), which is exactly what an
	// auth check on a slot with no auth configured by design is.
	healthCheckStatusSkip = "skip"
	// healthCheckStatusWarn / Critical / Error are the remaining non-passing
	// statuses the dashboard's FAILING_CHECK_STATUSES treats as failures.
	// Critical and error never come from today's spoke HealthSummary (it emits
	// pass/fail/warn/skip) but the field is a free-form passthrough, so they
	// are matched defensively.
	healthCheckStatusWarn     = "warn"
	healthCheckStatusCritical = "critical"
	healthCheckStatusError    = "error"
)

const (
	// healthStatusOK / Warning / Degraded / Critical mirror the overall values
	// the spoke's HealthSummary (pkg/dashboard/server.go) emits.
	healthStatusOK       = "ok"
	healthStatusWarning  = "warning"
	healthStatusDegraded = "degraded"
	healthStatusCritical = "critical"
	// healthCriticalFailThreshold mirrors HealthSummary's rule: MORE than this
	// many failing checks is "critical"; any failing check is "degraded".
	healthCriticalFailThreshold = 2
)

// isFailingCheckStatus mirrors the dashboard's FAILING_CHECK_STATUSES: the
// check statuses that count as a failure for the row pill and the hover.
func isFailingCheckStatus(st string) bool {
	switch st {
	case healthCheckStatusFail, healthCheckStatusWarn, healthCheckStatusCritical, healthCheckStatusError:
		return true
	}
	return false
}

// sanitizePlaceholderRows rewrites the Health payload of every UNASSIGNED
// placeholder row in the My Hives result so designed-state check failures
// (placeholderDesignedCheckNote: the auth-class family, the zero-consumed
// tokens warning, and a login-pending agents check) no longer read as
// degradation, while every other failure keeps its colour. Claimed hives pass through untouched. Called after
// enrichFromSaaSMeta has run on every row (provStatus must be present, or a
// placeholder that has not yet reported it would be misread as claimed by
// everything downstream of the org-prefix fallback).
func sanitizePlaceholderRows(result []MyHiveEntry) {
	for i := range result {
		if !isPlaceholderEntry(result[i]) {
			continue
		}
		result[i].Health = placeholderSanitizedHealth(result[i].Health)
	}
}

// placeholderSanitizedHealth returns a COPY of the health map in which every
// failing auth-class check is reclassified as "skip" with placeholderAuthNote
// as its detail, and status / fails / warns are recomputed from the surviving
// checks with the spoke's own thresholds. When nothing needs neutralising the
// input is returned as-is.
//
// The input is never mutated: RegistryEntry.Health is a map shared by
// reference with the hub's live registry snapshot (handleMyHives copies the
// slice, not the maps), so an in-place rewrite here would corrupt the hub's
// own record of what the spoke reported.
//
// Only the ARRAY shape of "checks" — the shape the heartbeat actually carries
// (spoke HealthSummary) — is rewritten. The defensive map shape that
// failingHealthChecks also accepts is left untouched: turning a row green
// demands positive evidence that only designed-state checks were failing, and
// an unrecognised shape is not that.
func placeholderSanitizedHealth(health map[string]any) map[string]any {
	if health == nil {
		return nil
	}
	rawChecks, ok := health["checks"].([]any)
	if !ok {
		return health
	}

	neutralized := 0
	fails, warns := 0, 0
	outChecks := make([]any, 0, len(rawChecks))
	for _, v := range rawChecks {
		ck, isObj := v.(map[string]any)
		if !isObj {
			outChecks = append(outChecks, v)
			continue
		}
		name, _ := ck["name"].(string)
		st, _ := ck["status"].(string)
		detail, _ := ck["detail"].(string)
		if note := placeholderDesignedCheckNote(name, detail); note != "" && isFailingCheckStatus(st) {
			neutralized++
			replaced := make(map[string]any, len(ck)+1)
			for k, val := range ck {
				replaced[k] = val
			}
			replaced["status"] = healthCheckStatusSkip
			replaced["detail"] = note
			outChecks = append(outChecks, replaced)
			continue
		}
		// Checks whose failure is genuine even on a placeholder keep their
		// reported status and are what the recomputed overall status is
		// derived from.
		switch st {
		case healthCheckStatusFail, healthCheckStatusCritical, healthCheckStatusError:
			fails++
		case healthCheckStatusWarn:
			warns++
		}
		outChecks = append(outChecks, v)
	}
	if neutralized == 0 {
		return health
	}

	out := make(map[string]any, len(health))
	for k, v := range health {
		out[k] = v
	}
	out["checks"] = outChecks
	out["fails"] = fails
	out["warns"] = warns
	out["status"] = overallHealthStatus(fails, warns)
	return out
}

// overallHealthStatus mirrors the spoke's HealthSummary thresholds exactly, so
// a recomputed status is always one the dashboard already knows how to colour.
func overallHealthStatus(fails, warns int) string {
	switch {
	case fails > healthCriticalFailThreshold:
		return healthStatusCritical
	case fails > 0:
		return healthStatusDegraded
	case warns > 0:
		return healthStatusWarning
	default:
		return healthStatusOK
	}
}
