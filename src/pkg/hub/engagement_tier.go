package hub

import "time"

// User engagement status tiers for the Hub Admin Users table. The old Status
// column said "active" for every non-blocked user — an open idle tab, a user
// who never logged in, and a daily driver all rendered identically. The tier
// replaces that with a classification driven by two honest signals:
//
//   - last REAL action: the newest audit-logged user action on any of the
//     user's hives (SaaSUser.LastActionAt, folded from spoke audit logs), and
//   - focus-aware engaged presence: time credited only while the user's
//     browser was visible with recent input (LastEngagedAt / EngagedSeconds,
//     from the spoke's /api/presence pings).
//
// Tiers, most to least engaged:
//
//	blocked — admin-blocked; overrides everything (the Block button's state).
//	live    — connected right now AND engaged (focused tab, recent input).
//	active  — a real action or engaged presence within engagementActiveWindow.
//	idle    — seen (login / open session / stale action) within
//	          engagementDormantWindow, but nothing REAL within the active
//	          window. This is the idle-open-tab crowd.
//	dormant — no signal of any kind within engagementDormantWindow.
//	never   — zero completed logins, ever.
//
// Records that predate the new fields have no LastActionAt/LastEngagedAt;
// absence is treated as NO DATA (the timestamp simply doesn't contribute),
// never as "engaged as of zero-time" or "engaged as of now". For those users
// the LastLogin fallback still separates idle from dormant, so old spokes and
// old records degrade to a coarser-but-honest answer.
const (
	// engagementActiveWindow is how recently a REAL signal (audited action or
	// engaged presence) must have occurred for a user to rank `active`.
	engagementActiveWindow = 7 * 24 * time.Hour
	// engagementDormantWindow is how long with NO signal at all (not even a
	// login or an open tab) before a user ranks `dormant` rather than `idle`.
	engagementDormantWindow = 30 * 24 * time.Hour
)

// Tier names — the API surface of the status_tier field; the dashboard JS
// matches on these strings.
const (
	tierBlocked = "blocked"
	tierLive    = "live"
	tierActive  = "active"
	tierIdle    = "idle"
	tierDormant = "dormant"
	tierNever   = "never"
)

// parseEngagementTime parses an RFC3339 field, returning ok=false for the
// absent/invalid case so callers treat missing data as missing — a record
// without the field must never behave as if the event happened at zero-time
// or right now.
func parseEngagementTime(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// userStatusTier classifies one user. Pure function of the record plus the
// hub's presence view, so it is directly table-testable:
//
//	liveNow    — the user is in the fresh live set (an open session somewhere).
//	engagedNow — the user is in the fresh ENGAGED set (focused + recent input).
func userStatusTier(u *SaaSUser, liveNow, engagedNow bool, now time.Time) string {
	if u.Blocked {
		return tierBlocked
	}
	// Zero logins ever: LoginCount is backfilled to ≥1 on load for anyone with
	// a LastLogin, so both being empty means the user has never completed a
	// hub login (e.g. admin-provisioned and never showed up).
	if u.LoginCount == 0 && u.LastLogin == "" {
		return tierNever
	}
	if liveNow && engagedNow {
		return tierLive
	}
	// lastReal: newest HONEST signal — audited action or engaged presence.
	var lastReal time.Time
	if t, ok := parseEngagementTime(u.LastActionAt); ok && t.After(lastReal) {
		lastReal = t
	}
	if t, ok := parseEngagementTime(u.LastEngagedAt); ok && t.After(lastReal) {
		lastReal = t
	}
	if !lastReal.IsZero() && now.Sub(lastReal) <= engagementActiveWindow {
		return tierActive
	}
	// lastSeen: newest signal of ANY kind, including the weak ones — a hub
	// login, a merely-open session right now, or the stale real signal above.
	// It only separates idle (recently seen, doing nothing real) from dormant
	// (gone entirely).
	lastSeen := lastReal
	if t, ok := parseEngagementTime(u.LastLogin); ok && t.After(lastSeen) {
		lastSeen = t
	}
	if liveNow {
		lastSeen = now
	}
	if !lastSeen.IsZero() && now.Sub(lastSeen) <= engagementDormantWindow {
		return tierIdle
	}
	return tierDormant
}
