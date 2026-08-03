package hub

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
// Health payload before it ships to the browser: failing auth-class checks are
// reclassified as "skip" with placeholderAuthNote as their detail, and the
// overall status / fails / warns are recomputed from the surviving checks
// using the spoke's own thresholds. Genuine placeholder-applicable failures —
// not ready, agents crash-looping, queue wedged — keep their status untouched
// and still turn the row red, exactly as drift.go's rule 2 intends ("never
// flag a placeholder for a claimed-hive concern", and never hide a real one).
//
// Which rows count as placeholders is decided by isPlaceholderEntry
// (drift.go), the SAME predicate computeDrift and the alert evaluator use, so
// the row dot, the drift badge and the Attention Needed panel can never
// disagree about what an unassigned hive is.

// placeholderAuthNote is the status-hover text that replaces an auth-class
// failure's error detail on an unassigned row, so the hover says WHY the check
// is not evaluated instead of presenting a designed state as a fault.
const placeholderAuthNote = "Unassigned — auth not configured by design"

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
// placeholder row in the My Hives result so auth-class check failures — the
// pool's designed state — no longer read as degradation, while every other
// failure keeps its colour. Claimed hives pass through untouched. Called after
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
		if isAuthClassHealthCheck(name) && isFailingCheckStatus(st) {
			neutralized++
			replaced := make(map[string]any, len(ck)+1)
			for k, val := range ck {
				replaced[k] = val
			}
			replaced["status"] = healthCheckStatusSkip
			replaced["detail"] = placeholderAuthNote
			outChecks = append(outChecks, replaced)
			continue
		}
		// Non-auth checks keep their reported status and are what the row's
		// recomputed overall status is derived from.
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
