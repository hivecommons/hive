package dashboard

// Live session-role resolution (#4299 — 3rd report of the "owner access
// required" class, after #4081/#4082 and #4134).
//
// # THE RECURRING BUG CLASS THIS KILLS
//
// A per-user session (device-flow login or hub SSO handoff) used to FREEZE the
// role resolved at login into the persisted userSession for the whole
// sessionTTL (30 days, surviving pod restarts). Meanwhile the hive's
// authorized-users allowlist — the authority the login itself consulted — keeps
// changing underneath it: the hub heartbeat re-delivers Manage Access grants
// every beat (hub.AuthorizedUsersCallback updates
// cfg.Dashboard.AuthorizedUsers in place, exactly so grants "take effect
// ... without any kubectl push").
//
// The result was that a Manage Access grant NEVER took effect for an
// already-signed-in user on a direct-route spoke: the hub said "owner", the
// heartbeat delivered "owner", the Manage Access panel showed "owner" — and the
// spoke kept answering every owner-gated mutation with 403 "owner access
// required" from the role frozen days earlier. Refreshing the tab changes
// nothing (the hive_session cookie and its stored role persist); only a full
// sign-out/sign-in re-resolved the role. Downgrades and revocations were
// equally frozen, which is the same defect pointing the other way.
//
// handleSSO's allowlist resolution already re-resolves the role live for
// exactly this reason. This file makes that the ONE shared rule for every
// consumer of a session's role, so no future auth path can reintroduce the
// frozen-role variant of the bug.

// liveSessionRole resolves the CURRENT role for a session-authenticated user,
// consulting this hive's authorized-users allowlist on EVERY request rather
// than trusting the role frozen into the session at login.
//
//   - If the allowlist has an entry for the user (canonical or legacy identity
//     form — see config.AuthorizedRole), that entry's role is authoritative:
//     upgrades AND downgrades granted hub-side take effect immediately.
//   - If the allowlist is enforced (direct-route spoke) and the user is absent,
//     their access was revoked: returns ok=false and the caller must treat the
//     session as unauthenticated. A revoked user must not coast on a stale
//     session for up to 30 days.
//   - If no allowlist governs this spoke (hub-proxied or open/dev), the
//     session's own role stands — the hub nginx or the deployment's open trust
//     model is the authority there, not a list this spoke doesn't have.
func (s *Server) liveSessionRole(sess *userSession) (role string, ok bool) {
	if sess == nil {
		return "", false
	}
	return s.liveAllowlistRole(sess.Username, sess.Role)
}

// liveAllowlistRole is the single shared rule for resolving a user's CURRENT
// role on this spoke: the allowlist entry when one exists, the caller's
// fallback when no allowlist governs, and ok=false (revoked — treat as
// unauthenticated/denied) when the allowlist is enforced and the user is
// absent. Session injection (authenticate) and the SSO handoff mint both
// resolve through here so they can never disagree.
func (s *Server) liveAllowlistRole(username, fallback string) (role string, ok bool) {
	role = fallback
	if s.deps != nil && s.deps.Config != nil {
		if allowRole, found := s.deps.Config.Dashboard.AuthorizedRole(username); found {
			role = allowRole
		} else if s.deps.Config.Dashboard.IsDirectRouteAuthzEnabled() {
			return "", false
		}
	}
	return role, true
}
