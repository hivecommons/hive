package hub

import (
	"net/url"
	"os"
	"strings"
)

var saasUsersDir = "/data/saas/users"

// defaultHubAdminUsername is the compile-time fallback fleet-superuser
// GitHub login, used when HIVE_HUB_ADMIN_USERNAME is not set in the
// environment. Keeping the historical value as the default preserves
// backward compatibility for existing deployments.
const defaultHubAdminUsername = "clubanderson"

// hubAdminUsername is the GitHub login treated as the fleet superuser. It is
// resolved once at package init from HIVE_HUB_ADMIN_USERNAME (falling back to
// defaultHubAdminUsername), rather than being a hardcoded constant (audit F12,
// CWE-798). This lets a deployment override the admin login via config and
// removes the fragility where a GitHub username rename or release would silently
// transfer — or forfeit — fleet-superuser privilege. It is set once at startup
// and treated as read-only thereafter (never mutated at runtime).
var hubAdminUsername = resolveHubAdminUsername()

// resolveHubAdminUsername reads the admin login from the environment, trimming
// surrounding whitespace, and falls back to defaultHubAdminUsername when the
// env var is unset or blank.
func resolveHubAdminUsername() string {
	if v := strings.TrimSpace(os.Getenv("HIVE_HUB_ADMIN_USERNAME")); v != "" {
		return v
	}
	return defaultHubAdminUsername
}

// hubAdminsEnv is the env var (comma-separated canonical ids, e.g.
// "github:clubanderson,google:1078...") that overrides the admin SET for
// multi-provider login. A bare login in the list is accepted and treated as
// github: via the identity shim. When unset, the admin set is the single
// hubAdminUsername above (itself overridable via HIVE_HUB_ADMIN_USERNAME), so
// both existing env contracts keep working. Prefer isHubAdmin()/
// primaryHubAdmin() over comparing against hubAdminUsername directly, so
// multi-provider admins work and so a same-subject identity on a DIFFERENT
// provider can never inherit admin.
const hubAdminsEnv = "HIVE_HUB_ADMINS"

// hubAdminSet returns the canonicalized set of admin identities. Sourced from
// HIVE_HUB_ADMINS when set, else the single hubAdminUsername. Every entry is
// run through canonicalizeLegacy so a bare login becomes github:<login>; this
// is what stops a Google/IBMid user whose subject happens to be the admin's
// login from matching the GitHub admin.
func hubAdminSet() map[string]bool {
	raw := strings.TrimSpace(os.Getenv(hubAdminsEnv))
	entries := []string{hubAdminUsername}
	if raw != "" {
		entries = splitCSV(raw)
	}
	set := make(map[string]bool, len(entries))
	for _, e := range entries {
		if c := canonicalizeLegacy(e); c != "" {
			set[strings.ToLower(c)] = true
		}
	}
	return set
}

// isHubAdmin reports whether the given identity (bare-legacy or canonical) is a
// hub admin. Both the input and the configured admin ids are canonicalized, so
// "clubanderson", "github:clubanderson", and "GitHub:ClubAnderson" all match the
// default admin, while "google:clubanderson" does NOT.
func isHubAdmin(id string) bool {
	if id == "" {
		return false
	}
	return hubAdminSet()[strings.ToLower(canonicalizeLegacy(id))]
}

// primaryHubAdmin returns the canonical identity of the primary hub admin — the
// first entry of HIVE_HUB_ADMINS, else the resolved hubAdminUsername. Used where
// the code needs a concrete admin identity to WRITE (e.g. audit attribution).
func primaryHubAdmin() string {
	raw := strings.TrimSpace(os.Getenv(hubAdminsEnv))
	if raw != "" {
		if list := splitCSV(raw); len(list) > 0 {
			return canonicalizeLegacy(list[0])
		}
	}
	return canonicalizeLegacy(hubAdminUsername)
}

// userCanonicalID returns a user's canonical wire-form identity: the explicit
// CanonicalID when present, else the legacy-shimmed GitHubUsername (a bare login
// becomes github:<login>). This is the single source of truth for "who is this
// record" across the dual-read storage path and the provider badge.
func userCanonicalID(u *SaaSUser) string {
	if u == nil {
		return ""
	}
	if u.CanonicalID != "" {
		return canonicalizeLegacy(u.CanonicalID)
	}
	return canonicalizeLegacy(u.GitHubUsername)
}

// userProvider returns a user's login provider ("github"/"google"/"ibmid"/
// "redhat"/"microsoft"/"custom"), from the stored Provider field when set, else
// parsed from the canonical identity. Legacy records with neither resolve to
// "github" via the shim. Drives the admin Users-table auth-method badge.
func userProvider(u *SaaSUser) string {
	if u == nil {
		return ""
	}
	if u.Provider != "" {
		return strings.ToLower(u.Provider)
	}
	if p, _, ok := parseCanonical(userCanonicalID(u)); ok {
		return p
	}
	return legacyProvider
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func repoTargetForgeHost(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return "github.com"
	}
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return strings.ToLower(strings.Trim(strings.TrimPrefix(strings.TrimPrefix(baseURL, "https://"), "http://"), "/"))
}

type SaaSUser struct {
	GitHubUsername string            `json:"github_username"`
	CreatedAt      string            `json:"created_at"`
	LastLogin      string            `json:"last_login"`
	Hives          map[string]string `json:"hives"`
	// HiveExpiry optionally bounds a grant in Hives: hive ID → RFC3339 UTC
	// instant after which that grant is revoked (#4150). Grants without an
	// entry are permanent — omitempty keeps every pre-expiry record
	// byte-identical on disk. Enforced at read time by loadSaaSUser's prune
	// and persisted/audited by sweepExpiredAccess (access_expiry.go).
	HiveExpiry     map[string]string `json:"hive_expiry,omitempty"`
	SaaSQuota      int               `json:"saas_quota"`
	Blocked        bool              `json:"blocked"`
	EncryptedToken string            `json:"encrypted_token,omitempty"`

	// Multi-provider identity (phase 1d). All omitempty so the thousands of
	// existing GitHub-only records on the PVC round-trip byte-identical until a
	// user first logs in / links after this ships.
	//
	//   CanonicalID  the wire-form primary identity ("google:1078", "github:foo").
	//                Empty on a legacy record → the shim treats GitHubUsername as
	//                the (github:) primary. saveSaaSUser/loadSaaSUser already dual-
	//                read on GitHubUsername; CanonicalID is the explicit form used
	//                by the badge and by Phase 2's OIDC callback when it creates a
	//                non-GitHub user.
	//   Provider     "github" | "google" | "ibmid" | "redhat" | "microsoft" |
	//                "custom" — drives the admin Users-table auth-method badge.
	//                Derivable from CanonicalID but stored so the badge needs no
	//                parse per render.
	//   AvatarURL    stored avatar (Google/IBMid give a picture claim); replaces
	//                the derived github.com/<login>.png where present.
	//   Email        the provider email claim (display only; NEVER the key — subs
	//                are stable, emails are reassignable).
	//   LinkedGitHubLogin  an OPTIONAL attached GitHub identity for a non-GitHub
	//                primary who needs user-scoped GitHub calls (contributor
	//                reissue). Never required to own a hive — the App does the
	//                GitHub work.
	CanonicalID       string `json:"canonical_id,omitempty"`
	Provider          string `json:"provider,omitempty"`
	AvatarURL         string `json:"avatar_url,omitempty"`
	Email             string `json:"email,omitempty"`
	LinkedGitHubLogin string `json:"linked_github_login,omitempty"`

	// DisplayName is the PROVIDER-ASSERTED human name from the OIDC name claim
	// (or userinfo), refreshed on every completed login. Distinct from FullName,
	// which is ADMIN-entered CRM text and must never be clobbered by a login.
	// Display only — the identity key stays provider:sub. omitempty so existing
	// records round-trip byte-identical until the user's next login enriches
	// them (backfill-by-login, no migration).
	DisplayName string `json:"display_name,omitempty"`

	// Contact/CRM fields. Admin-maintained free text used to reach a hub user
	// outside GitHub (and to remember what was said last time). All three are
	// omitempty so the thousands of existing user records already on the PVC
	// stay byte-identical until an admin actually fills one in — a record
	// without them round-trips through load/save unchanged.
	//
	// These are operator-entered free text rendered into the dashboard, so
	// every render path must escape them and every write path must cap them
	// (see maxContactNameLen / maxContactSlackIDLen / maxContactNotesLen).
	FullName string `json:"full_name,omitempty"`
	SlackID  string `json:"slack_id,omitempty"`
	Notes    string `json:"notes,omitempty"`
	// Company is ADMIN-entered CRM free text — the user's company/organization.
	// Like FullName/SlackID/Notes it is operator-maintained (never asserted by a
	// login) and is deliberately NOT collected in the hive request/provision
	// form; the operator fills it in manually from the admin Users table. Same
	// escaping + length-cap discipline as the other contact fields
	// (maxContactCompanyLen); omitempty so existing records round-trip
	// byte-identical until an admin sets it.
	Company string `json:"company,omitempty"`

	// Country is an OPTIONAL ISO 3166-1 alpha-2 code (uppercase, e.g. "GB"),
	// rendered as a small flag beside the user's avatar. Two sources, in
	// priority order: the explicit dropdown in the get-started wizard (copied
	// here on approval, like FullName/SlackID above), else a best-effort
	// inference from the Accept-Language region subtag at login, which only
	// ever fills an EMPTY value. See user_country.go for the full rationale and
	// the privacy posture.
	//
	// Stored as the code, never as the glyph: the flag is derived at render
	// time from regional-indicator code points, so no external image host is
	// involved and an unknown country renders nothing at all.
	//
	// omitempty so the thousands of existing records on the PVC round-trip
	// byte-identical until a user actually picks a country or logs in from a
	// browser that states a region.
	Country string `json:"country,omitempty"`

	// CountrySetByUser records that the country above was chosen DELIBERATELY
	// by the user rather than inferred, and it is what makes an explicit CLEAR
	// stick.
	//
	// Without it, "explicit" is inferred from `Country != ""` (see
	// applyInferredCountry), which is fine for a pick but wrong for a clear: a
	// user who removes their country via the self-service endpoint leaves an
	// empty field, and the very next login's Accept-Language inference would
	// silently put a flag back. "Prefer not to say" would become impossible to
	// express — and impossible to notice failing, since the flag reappears a
	// login later, far from the action that was supposed to remove it.
	//
	// Set only by the user's own writes: the self-service endpoint
	// (handleMyCountry) and the wizard pick copied on approval
	// (applyRequestContactToUser). NEVER set by the login-path inference, which
	// is precisely the distinction this field exists to draw.
	//
	// omitempty bool so every record that has not been through a deliberate
	// pick — which today is all of them — round-trips byte-identical.
	//
	// STILL WRITTEN, not deprecated: CountrySource below is the finer-grained
	// successor, but this boolean is what other readers and every record
	// already on the PVC speak, so every user-chosen write keeps setting it.
	CountrySetByUser bool `json:"country_set_by_user,omitempty"`

	// CountrySource is the PROVENANCE of the country above — who put it there.
	// One of countrySourceInferred / countrySourceAdmin / countrySourceUser, or
	// "" for a record nothing has ever touched.
	//
	// A boolean stopped being enough the moment an ADMIN could assign a country
	// on someone else's behalf, because that is a third kind of claim and it
	// sits BETWEEN the two the boolean can express:
	//
	//   - It is not user-chosen. Stamping CountrySetByUser for an admin edit
	//     would assert the user made a statement about themselves that they
	//     never made, and — since that marker is also what suppresses ever
	//     asking again — would permanently silence the question for them.
	//   - But it must still outrank Accept-Language inference. An admin's
	//     best-effort attribution is a human looking at evidence; the header is
	//     a language preference. Letting the next login overwrite it would
	//     re-introduce, in a new form, exactly the silent-clobber bug #4374 was
	//     opened to fix.
	//
	// Precedence, strongest first: user > admin > inferred > unset. See
	// countryProvenanceRank and mayOverwriteCountry in user_country.go, which
	// are the single arbiters — no caller compares these strings by hand.
	//
	// BACKWARD COMPATIBILITY. Records written before this field exists carry
	// only CountrySetByUser, so an ABSENT source is read through that boolean:
	// CountrySetByUser=true with no source means user-chosen (see
	// effectiveCountrySource). That is why this is omitempty and why nothing
	// backfills it — an untouched record must still serialize byte-identically.
	CountrySource string `json:"country_source,omitempty"`

	// Engagement stats, admin-only (they ride /api/saas/admin/users, which is
	// requireAdmin). Both omitempty ints so existing records round-trip
	// byte-identical until the user first logs in / opens a hive after this ships.
	//
	// LoginCount is the number of completed hub OAuth logins. Incremented in
	// exactly one place — handleOAuthCallback — never in ensureSaaSUser, whose
	// other callers (my-hives poll, admin provisioning) would inflate it.
	LoginCount int `json:"login_count,omitempty"`
	// SessionSeconds is the cumulative time this user has had at least one live
	// session on a hive dashboard, accumulated by the hub from the spoke's
	// per-heartbeat active-session report (see handleHeartbeat). It is a sampled
	// sum of inter-beat intervals, so it is accurate to roughly the beat interval,
	// not to the second.
	SessionSeconds int64 `json:"session_seconds,omitempty"`
	// EngagedSeconds is the honest subset of SessionSeconds: time accumulated
	// only on beats where the user's browser reported ENGAGED presence — tab
	// visible AND input within the idle window (heartbeat EngagedSessionUsers).
	// An idle open tab grows SessionSeconds but never this. omitempty, and
	// absent on records that predate the field or whose spokes don't report
	// presence yet — absence means NO DATA, never "provably unengaged".
	EngagedSeconds int64 `json:"engaged_seconds,omitempty"`
	// LastEngagedAt is the RFC3339 time of the most recent beat that credited
	// EngagedSeconds — when a human was last actually behind this user's
	// session. Feeds the `active` status tier. Same absence semantics as
	// EngagedSeconds.
	LastEngagedAt string `json:"last_engaged_at,omitempty"`
	// LastActionAt is the RFC3339 time of the user's most recent REAL audited
	// action on any of their hives (config save, agent restart, ACMM change,
	// login, …), folded hub-ward from the spoke audit logs (heartbeat
	// UserLastActions) keeping the per-user maximum. Same absence semantics.
	LastActionAt string `json:"last_action_at,omitempty"`
}

// Length caps for the admin-editable contact fields. These are free text
// written straight to the PVC, so each is bounded independently rather than
// relying on the request-body cap alone: name and Slack ID are identifiers and
// stay short, while notes is the running CRM log for a user and gets the most
// room. Values over the cap are truncated (not rejected) so a long paste still
// saves something useful instead of silently failing.
const (
	// maxContactNameLen bounds a person's full name. Generous versus real
	// names so non-Latin scripts and long multi-part names still fit.
	maxContactNameLen = 128
	// maxContactSlackIDLen bounds a Slack member ID or handle. Real Slack IDs
	// are ~11 chars (U01ABCDEF23); the headroom allows an @handle or a
	// workspace-qualified form.
	maxContactSlackIDLen = 64
	// maxContactCompanyLen bounds the company/organization name — an identifier
	// like the name/Slack fields, sized generously for long legal entity names.
	maxContactCompanyLen = 128
	// maxContactNotesLen bounds the free-text notes field — the longest of the
	// three, sized for a few paragraphs of admin scratch notes per user.
	maxContactNotesLen = 8192
	// maxUpdateUserBodyBytes caps the PUT body for the admin user-update
	// endpoint. Comfortably above the sum of the field caps plus JSON
	// overhead/escaping, and small enough that the endpoint can never be used
	// to push a large blob at the PVC.
	maxUpdateUserBodyBytes = 64 * 1024
)

// truncateRunes clips s to at most max runes. It counts runes rather than
// bytes so a cap never splits a multi-byte character (which would write
// invalid UTF-8 into the user record and then into the dashboard HTML).
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func (s *HubServer) registerSaaSRoutes() {
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /access-denied", s.handleAccessDenied)
	s.mux.HandleFunc("GET /api/saas/my-hives", s.requireAuth(s.handleMyHives))
	// Per-release image-pulls series, headline (active release line) plus
	// per-line (external adoption gauge). requireAuth,
	// not requireAdmin: any signed-in hub user sees the same public-adoption
	// number, and the underlying data is scraped from the PUBLIC package page.
	s.mux.HandleFunc("GET /api/hub/image-pulls", s.requireAuth(s.handleImagePulls))
	// Token attribution rollups. requireAuth (not requireAdmin) because a
	// non-admin legitimately sees their OWN hives' usage; the handler scopes
	// fleet-wide data to admins itself.
	s.mux.HandleFunc("GET /api/saas/usage", s.requireAuth(s.handleUsage))
	// Self-service country: the ONE field a non-admin may write on their own
	// user record. requireAuth, not requireAdmin — that is the entire point,
	// since every other SaaSUser write is admin-gated and the wizard is a
	// one-time surface. The handler resolves the acting user from the SESSION
	// and the body carries no identity, so this cannot reach anyone else's
	// record. See handleMyCountry in user_country.go.
	//
	// PUT with a JSON body rather than a code in the path: country is personal
	// data and a URL would put it in access logs, Referer headers and history.
	s.mux.HandleFunc("GET /api/saas/me/country", s.requireAuth(s.handleMyCountry))
	s.mux.HandleFunc("PUT /api/saas/me/country", s.requireAuth(s.handleMyCountry))
	s.mux.HandleFunc("POST /api/saas/lite/enroll", s.requireAuth(s.handleLiteEnroll))
	s.mux.HandleFunc("POST /api/saas/hives", s.requireAuth(s.handleCreateHive))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/status", s.requireAuth(s.handleHiveStatus))
	// /open is a browser NAVIGATION endpoint (the SSO handoff), not an API call.
	// It is registered WITHOUT requireAuth so an unauthenticated visit redirects
	// to the hub login (and back) instead of dumping a raw {"error":...} JSON.
	// handleOpenHive does its own auth check + login redirect.
	s.mux.HandleFunc("GET /api/saas/hives/{id}/open", s.handleOpenHive)
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}", s.requireAuth(s.handleDeleteHive))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/upgrade", s.requireAuthOrSpokeUpgrade(s.handleUpgradeHive))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/switch-branch", s.requireAuth(s.handleSwitchBranch))
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/visibility", s.requireAuth(s.handleToggleVisibility))
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/auto-upgrade", s.requireAuth(s.handleToggleAutoUpgrade))
	// Rename a hive's display name (its ProjectName). requireAuth plus an inner
	// owner-or-admin check, exactly like visibility/auto-upgrade above — the
	// gate is the security boundary, not just the hidden UI affordance.
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/name", s.requireAuth(s.handleRenameHive))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/agents/{agent}/restart-counter/reset", s.requireAuth(s.handleResetAgentRestarts))
	// Move a hive between forges (github.com <-> a GitHub Enterprise host).
	// requireAuth plus an inner owner-or-admin check, exactly like
	// switch-branch and auto-upgrade above.
	s.mux.HandleFunc("POST /api/saas/hives/{id}/forge", s.requireAuth(s.handleSwitchForge))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/reset-app", s.requireAuth(s.handleResetApp))
	// Assigns a hive its OPTIONAL second GitHub App (#4815). requireAuth is the
	// same outer gate reset-app uses; the handler itself re-checks isHubAdmin,
	// which is the authoritative check for both.
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/secondary-app", s.requireAuth(s.handleSetHiveSecondaryApp))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/restart-spoke", s.requireAuth(s.handleRestartSpoke))
	s.mux.HandleFunc("GET /api/saas/hive-config/{hiveID}", s.requireAuth(s.handleProxyHiveConfig))
	s.mux.HandleFunc("GET /api/saas/latest-sha", s.handleLatestSHA)
	s.mux.HandleFunc("POST /api/saas/hub/upgrade", s.requireAdmin(s.handleHubSelfUpgrade))
	s.mux.HandleFunc("PUT /api/saas/hub/auto-upgrade", s.requireAdmin(s.handleHubAutoUpgrade))
	// Admin upgrade kill switch (upgrade_pause.go): pause hub self-upgrades
	// and/or ALL automatic spoke image changes, fleet-wide.
	s.mux.HandleFunc("GET /api/saas/upgrade-pause", s.requireAdmin(s.handleGetUpgradePause))
	s.mux.HandleFunc("POST /api/saas/upgrade-pause", s.requireAdmin(s.handleSetUpgradePause))
	s.mux.HandleFunc("GET /api/saas/auth-check", s.handleSaaSAuthCheck)
	// Sibling-product identity bridge (#4171): dibs.kubestellar.io forwards the
	// browser's hive_hub_user cookie here server-to-server to resolve the
	// session. Registered GET-only via the method pattern, and WITHOUT
	// requireAuth so the unauthenticated answer is the exact 401 JSON shape the
	// dibs bridge expects rather than the generic middleware error.
	s.mux.HandleFunc("GET /api/saas/whoami", s.handleSaaSWhoami)
	// Sibling-product repo registry (#4193): dibs polls this server-to-server
	// (no session) every ~5 minutes to learn which repos hives manage. Public
	// by design, so it returns ONLY already-public data — see handleDibsRepos.
	s.mux.HandleFunc("GET /api/saas/dibs/repos", s.handleDibsRepos)
	s.mux.HandleFunc("POST /api/saas/user-token", s.requireAuth(s.handleUserToken))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/access", s.requireAuth(s.handleAccessList))
	s.mux.HandleFunc("GET /api/saas/grantable-users", s.requireAuth(s.handleGrantableUsers))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/access", s.requireAuth(s.handleAccessAdd))
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}/access/{username}", s.requireAuth(s.handleAccessRemove))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/request-access", s.requireAuth(s.handleRequestAccess))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/requests", s.requireAuth(s.handleGetRequests))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/timeline", s.requireAuth(s.handleHiveTimeline))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/access-log", s.requireAuth(s.handleAccessLog))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/approve", s.requireAuth(s.handleApproveRequest))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/deny", s.requireAuth(s.handleDenyRequest))
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/approve-access/{username}", s.requireAuth(s.handleApproveAccess))
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}/deny-access/{username}", s.requireAuth(s.handleDenyAccess))
	s.mux.HandleFunc("GET /api/saas/access-status", s.handleAccessStatus)
	// GET /api/saas/repos is deliberately gone. Repo discovery ran on the
	// requester's github.com OAuth token, so it could only ever see public
	// GitHub — invisible to GitHub Enterprise, and structurally unable to list
	// a GitLab or Gitea repo when those forges arrive. The request flow now
	// takes a typed repository URL, which works for every forge without new
	// code, and that is what let the login drop to an empty OAuth scope.
	// Restoring this endpoint would also require restoring that scope.
	s.mux.HandleFunc("POST /api/saas/request-provision", s.requireAuth(s.handleRequestProvision))
	s.mux.HandleFunc("PUT /api/saas/approve-provision/{username}", s.requireAdmin(s.handleApproveProvision))
	s.mux.HandleFunc("DELETE /api/saas/deny-provision/{username}", s.requireAdmin(s.handleDenyProvision))
	s.mux.HandleFunc("GET /api/saas/admin/available-placeholders", s.requireAdmin(s.handleAvailablePlaceholders))
	s.mux.HandleFunc("GET /api/saas/admin/scale-settings", s.requireAdmin(s.handleGetScaleSettings))
	s.mux.HandleFunc("POST /api/saas/admin/scale-settings", s.requireAdmin(s.handleSetScaleSettings))
	s.mux.HandleFunc("GET /api/saas/admin/users", s.requireAdmin(s.handleAdminUsers))
	// Aggregate geographic rollup of the user base (counts only, no usernames).
	// Admin-gated like the rest of the CRM/Users surface: country is personal
	// data, so even the aggregate stays behind requireAdmin. Takes no query
	// parameters — no country ever appears in a URL. See user_country_rollup.go.
	s.mux.HandleFunc("GET /api/saas/admin/user-countries", s.requireAdmin(s.handleAdminUserCountries))
	// #3234: fleet readiness for removing the N1/N2 legacy compatibility lanes.
	s.mux.HandleFunc("GET /api/saas/admin/auth-rollout", s.requireAdmin(s.handleAuthRollout))
	// Master-secret rotation (src/docs/design/master-key-rotation.md). Both are
	// requireAdmin, which enforces isCSRFSafe BEFORE resolving identity — an
	// ambient hub session cookie would otherwise make a cross-site POST able to
	// rotate the fleet's master key. The rotate route is a POST for that reason
	// too: isCSRFSafe exempts safe methods.
	s.mux.HandleFunc("GET /api/saas/admin/key-generations", s.requireAdmin(s.handleKeyGenerations))
	s.mux.HandleFunc("POST /api/saas/admin/rotate-master-key", s.requireAdmin(s.handleRotateMasterKey))
	s.mux.HandleFunc("PUT /api/saas/admin/users/{username}", s.requireAdmin(s.handleAdminUpdateUser))
	s.mux.HandleFunc("DELETE /api/saas/admin/users/{username}", s.requireAdmin(s.handleAdminDeleteUser))
	// Admin read-only "View as user" impersonation. Enter is admin-only and
	// sets the short-lived signed hive_hub_impersonate cookie; exit clears it
	// and is exempt from the impersonation write-block (see impersonateExitPath)
	// so the admin can always get back out. Status folds into /api/auth/user for
	// the banner, but a dedicated read is offered too.
	s.mux.HandleFunc("POST /api/saas/admin/impersonate/exit", s.requireAdmin(s.handleImpersonateExit))
	s.mux.HandleFunc("POST /api/saas/admin/impersonate/{username}", s.requireAdmin(s.handleImpersonateStart))
	s.mux.HandleFunc("GET /api/saas/impersonation-status", s.requireAuth(s.handleImpersonationStatus))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/assign", s.requireAuth(s.handleAssignHive))
	// Escape hatch: return an assigned-but-unclaimed placeholder to the available
	// pool so it can be re-armed. Admin-only, and guarded to the wedged middle
	// state (assigned && !claim_delivered) inside the handler.
	s.mux.HandleFunc("POST /api/saas/hives/{id}/reset-assignment", s.requireAdmin(s.handleResetAssignment))
	s.mux.HandleFunc("GET /api/saas/cluster-health", s.requireAdmin(s.handleClusterHealth))
	// PR reach telemetry (#3994): the read-only join of merged PRs against
	// the commits/components the fleet reports actually running. The payload
	// names hives fleet-wide — the same exposure class as cluster-health
	// directly above, so the same requireAdmin gate.
	s.mux.HandleFunc("GET /api/reach", s.requireAdmin(s.handleReach))
	// Advisory-staleness diagnostics (#4167): the read-only fleet view of WHICH
	// gate decided each hive's advisory verdict, and how many stale digests no
	// pill is reporting. Names hives fleet-wide with their App state, so the
	// same requireAdmin gate as cluster-health and /api/reach above.
	s.mux.HandleFunc("GET /api/saas/admin/advisory-diagnostics", s.requireAdmin(s.handleAdvisoryDiagnostics))
	// Acknowledging a fleet alert is an operator action on the operator's own
	// view, so it is admin-only (see alerts.go).
	s.mux.HandleFunc("POST /api/saas/admin/alert-ack", s.requireAdmin(s.handleAlertAck))
	s.mux.HandleFunc("GET /api/hub/clusters", s.requireAuth(s.handleListClusters))
	// Per-cluster GitHub App key store. The GET is fingerprints only (never key
	// material); the PUT is the single write-only entry point for a key.
	s.mux.HandleFunc("GET /api/saas/admin/cluster-app-keys", s.requireAdmin(s.handleGetClusterAppKeys))
	s.mux.HandleFunc("PUT /api/saas/admin/cluster-app-keys/{clusterID}", s.requireAdmin(s.handlePutClusterAppKey))
	s.mux.HandleFunc("POST /api/saas/admin/hub-banner", s.requireAdmin(s.handleSendHubBanner))
	s.mux.HandleFunc("DELETE /api/saas/admin/hub-banner", s.requireAdmin(s.handleClearHubBanner))
	s.mux.HandleFunc("GET /api/saas/admin/hub-banner", s.requireAdmin(s.handleGetHubBanner))
	s.registerBulkRoutes()
	// Slack messaging. The single-user and hive-owner routes are admin-or-owner
	// (checked inside each handler, like switch-branch); the BROADCAST is
	// admin-only, because it reaches every user with a slack_id and cannot be
	// recalled. It additionally requires a typed confirmation and offers a dry
	// run — see slack.go.
	s.mux.HandleFunc("POST /api/saas/slack/user/{username}", s.requireAuth(s.handleSlackMessageUser))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/slack", s.requireAuth(s.handleSlackMessageHiveOwner))
	s.mux.HandleFunc("POST /api/saas/admin/slack/broadcast", s.requireAdmin(s.handleSlackBroadcast))
	s.mux.HandleFunc("POST /api/saas/admin/journey-snooze", s.requireAdmin(s.handleJourneySnooze))
	s.mux.HandleFunc("GET /api/saas/admin/journey-status", s.requireAdmin(s.handleJourneyStatus))
}
