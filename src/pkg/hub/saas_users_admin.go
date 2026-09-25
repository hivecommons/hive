package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// impersonateExitPath is the one mutating endpoint that stays callable while
// impersonation is active — it is how the admin gets OUT. Every other write is
// refused 403 by the write-block below.
const impersonateExitPath = "/api/saas/admin/impersonate/exit"

// blockIfImpersonatingWrite enforces the read-only property of impersonation.
// While a valid admin grant is active, ANY non-GET/HEAD request (except the
// exit endpoint) is refused 403 before it reaches its handler. This is the
// central write gate: it lives in both requireAuth and requireAdmin, through
// which every user-facing mutation is routed, so no write path can slip past.
// It returns true when it has already written the 403 response and the caller
// must stop.
func (s *HubServer) blockIfImpersonatingWrite(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return false
	}
	if r.URL.Path == impersonateExitPath {
		return false
	}
	_, _, impersonating := s.resolveIdentity(r)
	if !impersonating {
		return false
	}
	target := "user"
	if grant, ok := s.activeImpersonationGrant(r); ok {
		target = grant.Target
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	// target is a GitHub login (no quotes/backslashes possible), safe to inline.
	_, _ = w.Write([]byte(`{"error":"read-only while viewing as ` + target + ` — exit impersonation to make changes"}`))
	return true
}

// activeImpersonationGrant returns the verified grant behind the current
// request when (and only when) resolveIdentity would honor it. It is a thin
// read used for messaging/audit/status; the security decisions live in
// resolveIdentity.
func (s *HubServer) activeImpersonationGrant(r *http.Request) (impersonationGrant, bool) {
	if !isHubAdmin(s.getRealAuthUser(r)) {
		return impersonationGrant{}, false
	}
	cookie, err := r.Cookie(impersonateCookieName)
	if err != nil || cookie.Value == "" {
		return impersonationGrant{}, false
	}
	grant, _, ok := verifyImpersonateCookieValueWithGenerations(s.currentGenerations(), cookie.Value, time.Now())
	if !ok || !isHubAdmin(grant.Admin) {
		return impersonationGrant{}, false
	}
	if loadSaaSUser(grant.Target) == nil {
		return impersonationGrant{}, false
	}
	return grant, true
}

// saaSUserFilePaths returns the candidate on-disk paths for an identity, in
// read/try order: the canonical filename first, then the legacy "<login>.json"
// fallback for a bare or github: identity. The caller has already rejected
// path-traversal characters in the raw username.
func saaSUserFilePaths(username string) []string {
	var paths []string
	if stem, err := encodeUserFilename(username); err == nil {
		paths = append(paths, filepath.Join(saasUsersDir, stem+".json"))
	}
	provider, subject, ok := parseCanonical(username)
	if ok && provider == legacyProvider {
		legacy := filepath.Join(saasUsersDir, subject+".json")
		if len(paths) == 0 || paths[0] != legacy {
			paths = append(paths, legacy)
		}
	}
	return paths
}

// readSaaSUserFile reads a user's JSON, trying the canonical filename then the
// legacy fallback (see saaSUserFilePaths). Returns the first file that reads.
func readSaaSUserFile(username string) ([]byte, error) {
	var firstErr error
	for _, p := range saaSUserFilePaths(username) {
		data, err := os.ReadFile(p)
		if err == nil {
			return data, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = os.ErrNotExist
	}
	return nil, firstErr
}

// saveSaaSUserPath returns the file path a user record is written to. A GitHub
// (or bare-legacy) primary keeps its legacy "<login>.json" so existing records
// are updated in place with no rename; a non-GitHub primary writes the canonical
// "<provider>.<subject>.json".
func saveSaaSUserPath(u *SaaSUser) (string, error) {
	// The record's primary identity: the explicit CanonicalID when set (a
	// non-GitHub or newly-created user), else GitHubUsername (a bare login for
	// legacy GitHub users). Either resolves through parseCanonical + the shim.
	id := userCanonicalID(u)
	provider, subject, ok := parseCanonical(id)
	if !ok {
		return "", fmt.Errorf("invalid identity for save: %q", id)
	}
	if provider == legacyProvider {
		return filepath.Join(saasUsersDir, subject+".json"), nil
	}
	stem, err := encodeUserFilename(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(saasUsersDir, stem+".json"), nil
}

func loadSaaSUser(username string) *SaaSUser {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return nil
	}
	// Dual-read: try the canonical filename ("google.1078.json") then the legacy
	// "<login>.json". No file is rewritten — existing users resolve via legacy.
	data, err := readSaaSUserFile(username)
	if err != nil {
		return nil
	}
	var u SaaSUser
	if json.Unmarshal(data, &u) != nil {
		return nil
	}
	if u.Hives == nil {
		u.Hives = make(map[string]string)
	}
	// Backfill LoginCount for records that predate the login counter (added with
	// the admin engagement card). Those users have a real LastLogin but a zero
	// LoginCount, which renders as the contradictory "0 logins (last <date>)" on
	// the stats card. A user who has logged in at least once is, at minimum, one
	// login — so a present LastLogin with a zero count normalizes to 1. This is a
	// read-time floor only; the real counter keeps incrementing from here on the
	// next OAuth login (handleOAuthCallback), and it never lowers a genuine count.
	if u.LoginCount == 0 && strings.TrimSpace(u.LastLogin) != "" {
		u.LoginCount = 1
	}
	// On-access expiry enforcement (#4150): drop expired grants at READ time so
	// every consumer of a role — auth gates, accessForHive, the heartbeat's
	// authorized-users push — sees the revocation the instant it is due, on the
	// wall clock. Read-time only, never written here; sweepExpiredAccess
	// (access_expiry.go) persists the prune and stamps the timeline event.
	pruneExpiredHiveGrants(&u, time.Now())
	return &u
}

func saveSaaSUser(u *SaaSUser) error {
	if strings.Contains(u.GitHubUsername, "..") || strings.Contains(u.GitHubUsername, "/") || strings.Contains(u.GitHubUsername, "\\") {
		return fmt.Errorf("invalid username for save: %q", u.GitHubUsername)
	}
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(saasUsersDir, 0o755)
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	path, err := saveSaaSUserPath(u)
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func ensureSaaSUser(username string) *SaaSUser {
	now := time.Now().UTC().Format(time.RFC3339)
	u := loadSaaSUser(username)
	if u != nil {
		u.LastLogin = now
		if err := saveSaaSUser(u); err != nil {
			slog.Warn("ensureSaaSUser: save failed", "user", username, "error", err)
		}
		return u
	}
	quota := 0
	if isHubAdmin(username) {
		quota = -1
	}
	u = &SaaSUser{
		GitHubUsername: username,
		CreatedAt:      now,
		LastLogin:      now,
		Hives:          map[string]string{},
		SaaSQuota:      quota,
	}
	if err := saveSaaSUser(u); err != nil {
		slog.Warn("ensureSaaSUser: create failed", "user", username, "error", err)
	}
	return u
}

func listAllSaaSUsers() []SaaSUser {
	// Best-effort: a failed mkdir surfaces via the ReadDir error below.
	_ = os.MkdirAll(saasUsersDir, 0o755)
	entries, err := os.ReadDir(saasUsersDir)
	if err != nil {
		return nil
	}
	var users []SaaSUser
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".json")
		// A canonical filename ("google.1078", "github.foo") decodes to its wire
		// id; a legacy filename ("foo") does not — load it as the bare login.
		key := stem
		if id, ok := decodeUserFilename(stem); ok {
			key = id
		}
		u := loadSaaSUser(key)
		if u != nil {
			users = append(users, *u)
		}
	}
	return users
}

func (s *HubServer) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// SECURITY (N6, CWE-352): admin routes carry the SAME CSRF exposure as
		// requireAuth ones — the hub session cookie is ambient, so a cross-site
		// form POST to e.g. /api/saas/hub/upgrade or the cluster-app-key writer
		// executes with the admin's identity. requireAuth has always checked this
		// (see :499); requireAdmin never did, leaving every admin mutation
		// reachable from ANY origin — strictly worse than the sibling-tenant lane,
		// which at least requires a *.hive.hivecommons.dev foothold. Checked first,
		// before any identity resolution, so a forged request never reaches the
		// impersonation logic below.
		if !isCSRFSafe(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"CSRF check failed"}`))
			return
		}
		// Gate on the REAL logged-in user, not the effective (possibly
		// impersonated) identity. While the admin is viewing as a normal user,
		// getAuthUser resolves to that user on GETs — but admin routes (and in
		// particular the impersonation exit) must still be reachable by the real
		// admin, and no admin-only surface may leak to the impersonated target.
		username := s.getRealAuthUser(r)
		if !isHubAdmin(username) {
			http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
			return
		}
		// While impersonating, NO admin-only surface may leak to the view the
		// admin is "viewing as" — the whole point of read-only impersonation is to
		// see exactly what the target user sees, and a normal user is never an
		// admin. requireAdmin gates on the REAL admin (so exit and admin routes
		// stay reachable), which means an admin-DATA GET (e.g. /api/saas/admin/users)
		// would otherwise still answer 200 under impersonation and the client would
		// render the admin Users section. So: while a grant is active, refuse every
		// admin route (GET included) EXCEPT the impersonation exit — the client's
		// 403 handling then hides the admin section, matching what the target sees.
		if r.URL.Path != impersonateExitPath {
			if _, _, impersonating := s.resolveIdentity(r); impersonating {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"error":"admin surfaces are hidden while viewing as a user — exit impersonation for admin access"}`))
				return
			}
		}
		// Admin writes are also read-only under impersonation (exit excepted),
		// so an impersonating admin cannot mutate through an admin endpoint
		// either. (Redundant with the block above now, but kept as defense in
		// depth / a clear write-specific message if the above is ever relaxed.)
		if s.blockIfImpersonatingWrite(w, r) {
			return
		}
		next(w, r)
	}
}

func (s *HubServer) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	users := listAllSaaSUsers()
	// status_tier is computed HUB-side (it needs the hub's live/engaged
	// presence view and the tier windows) so the dashboard renders and sorts a
	// server-decided classification instead of re-deriving policy in JS.
	// See userStatusTier for the tier rules.
	live := make(map[string]bool)
	for _, name := range s.liveHiveUsernames() {
		live[name] = true
	}
	engaged := make(map[string]bool)
	for _, name := range s.engagedHiveUsernames() {
		engaged[name] = true
	}
	s.mu.RLock()
	hives := append([]RegistryEntry(nil), s.registry.Hives...)
	s.mu.RUnlock()
	now := time.Now()
	type adminUserView struct {
		SaaSUser
		StatusTier string `json:"status_tier"`
		TopRepo    string `json:"top_repo"`
		TopRepoURL string `json:"top_repo_url,omitempty"`
		// Provider is always populated (derived via userProvider) so the Users
		// table's auth-method badge never has to parse — a legacy github-only
		// record resolves to "github". This shadows SaaSUser.Provider's
		// omitempty, so the field is present on every row.
		Provider string `json:"provider"`
	}
	views := make([]adminUserView, 0, len(users))
	for i := range users {
		users[i].EncryptedToken = ""
		name := users[i].GitHubUsername
		topRepo := userTopRepoAssociation(&users[i], hives)
		views = append(views, adminUserView{
			SaaSUser:   users[i],
			StatusTier: userStatusTier(&users[i], live[name], engaged[name], now),
			TopRepo:    topRepo.Label,
			TopRepoURL: topRepo.URL,
			Provider:   userProvider(&users[i]),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"users": views})
}

// setImpersonateCookie writes (value != "") or clears (value == "") the signed
// impersonation cookie with the same hardened attributes as the session cookie:
// HttpOnly (no JS access), Secure (HTTPS only), SameSite=Lax. The short MaxAge
// mirrors impersonateTTL so the browser drops the cookie about when the server
// would stop honoring it.
//
// SECURITY (audit F4): this cookie is HOST-ONLY. It previously carried
// Domain=.hive.hivecommons.dev, copied from the session cookie, which meant the
// admin's live impersonation grant was transmitted to every hosted tenant's
// dashboard — i.e. handed to ~62 untrusted third parties on every request they
// received. Unlike hive_hub_user, NOTHING outside the hub ever reads it:
// grep confirms hive_hub_impersonate appears only in this package
// (activeImpersonationGrant / resolveIdentity), never in the spoke proxy
// (src/proxy/server.js) or any manifest. Dropping Domain therefore costs
// nothing and removes the cookie from the sibling attack surface entirely.
//
// Omitting Domain (rather than setting it) is what makes a cookie host-only per
// RFC 6265 §4.1.2.3 — there is no "Domain=host" spelling that achieves this.
//
// No flag day: the mint and the read both happen on hive.hivecommons.dev, so a
// browser holding the OLD domain-scoped cookie still presents it to the hub and
// still verifies. The next impersonate/exit re-mints it host-only. Worst case
// for an in-flight grant is that it expires on its own 30-minute TTL.
func setImpersonateCookie(w http.ResponseWriter, value string) {
	c := &http.Cookie{
		Name:     impersonateCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
	if value == "" {
		c.MaxAge = -1 // delete
	} else {
		c.MaxAge = int(impersonateTTL / time.Second)
	}
	http.SetCookie(w, c)
	if value == "" {
		// Also expire the legacy DOMAIN-scoped cookie. A host-only Set-Cookie
		// cannot delete a domain-scoped one — they are distinct entries in the
		// jar, and the browser would keep sending the old one on every hub
		// request until its own TTL lapsed, leaving "Exit impersonation"
		// silently ineffective for admins mid-migration. Emitting both
		// deletions is unconditional and idempotent: if no legacy cookie
		// exists, this is a no-op the browser discards.
		//
		// Removable once no admin can still be holding a pre-fix grant, which
		// the 30-minute impersonateTTL bounds.
		http.SetCookie(w, &http.Cookie{
			Name:     impersonateCookieName,
			Value:    "",
			Path:     "/",
			Domain:   legacyImpersonateCookieDomain,
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
	}
}

// legacyImpersonateCookieDomain is the sibling-wide scope the impersonation
// cookie used to carry before audit F4 made it host-only. Retained ONLY so the
// exit path can expire cookies minted by the previous build.
const legacyImpersonateCookieDomain = ".kubestellar.io"

// handleImpersonateStart begins an admin read-only "View as user" session.
// Registered behind requireAdmin, so only the real hub admin reaches it (and
// the impersonation write-block cannot fire here because starting requires no
// active grant). It validates the target is a registered user, then sets the
// short-lived signed hive_hub_impersonate cookie. The admin gains NO privilege:
// subsequent GETs render as the target, and every write is refused 403.
func (s *HubServer) handleImpersonateStart(w http.ResponseWriter, r *http.Request) {
	admin := s.getRealAuthUser(r) // == hubAdminUsername (requireAdmin gated)
	target := r.PathValue("username")
	if target == "" || target == admin {
		writeJSONError(w, http.StatusBadRequest, "invalid target user")
		return
	}
	if loadSaaSUser(target) == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	value := mintImpersonateCookieValueForGeneration(s.currentGenerations(), admin, target, time.Now())
	if value == "" {
		writeJSONError(w, http.StatusInternalServerError, "cannot start impersonation")
		return
	}
	setImpersonateCookie(w, value)
	s.logger.Info("audit: admin impersonation started", "admin", admin, "target", target,
		"at", time.Now().UTC().Format(time.RFC3339))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "viewing_as": target})
}

// handleImpersonateExit ends an active "View as user" session by clearing the
// impersonation cookie. It is registered behind requireAdmin (the real admin is
// always the actor) and its path is exempt from the write-block, so it stays
// callable WHILE impersonating — that is the whole point. It is a no-op if no
// grant is active.
func (s *HubServer) handleImpersonateExit(w http.ResponseWriter, r *http.Request) {
	admin := s.getRealAuthUser(r)
	if grant, ok := s.activeImpersonationGrant(r); ok {
		s.logger.Info("audit: admin impersonation ended", "admin", admin, "target", grant.Target,
			"at", time.Now().UTC().Format(time.RFC3339))
	}
	setImpersonateCookie(w, "")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleImpersonationStatus reports whether the current request is inside an
// impersonation session and, if so, who is being viewed. The banner reads this
// (or the equivalent fields folded into /api/auth/user). Because getAuthUser
// resolves to the target on this GET, the status is derived from the real admin
// grant via activeImpersonationGrant rather than from getAuthUser.
func (s *HubServer) handleImpersonationStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if grant, ok := s.activeImpersonationGrant(r); ok {
		_ = json.NewEncoder(w).Encode(map[string]any{"impersonating": true, "viewing_as": grant.Target})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"impersonating": false})
}

// handleAdminUpdateUser applies a partial admin edit to one hub user record.
// Every field is a POINTER in the request body, so a request carries only the
// keys the admin actually changed: the handler loads the current record, sets
// just those fields, and saves. That read-modify-write is what keeps a contact
// edit from clobbering Hives / quota / Blocked (and vice versa) when two admin
// widgets on the dashboard post concurrently.
//
// The contact fields (full_name, slack_id, notes) are admin-entered free text.
// They are length-capped here — the last point before the value reaches the
// PVC — and escaped on every dashboard render path.
//
// `country` rides this same body rather than a route of its own. It is the only
// way the field can be set for the thousands of users who joined before it
// existed: the wizard is a one-time gate already behind them, and the
// self-service endpoint reaches only the acting user, so an admin looking at a
// row with an empty Country column previously had no control at all. It carries
// ADMIN provenance, never user provenance — see the block on the country branch
// below, which is the load-bearing decision in this change.
//
// PRIVACY: the code rides the JSON BODY, never the path or a query string, for
// the same reason the self-service endpoint does — a URL lands in access logs,
// Referer headers and browser history.
//
// Registered behind requireAdmin; this handler does no auth of its own.
func (s *HubServer) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	u := loadSaaSUser(username)
	if u == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	// Free-text fields land on a PVC, so bound the body before decoding it.
	r.Body = http.MaxBytesReader(w, r.Body, maxUpdateUserBodyBytes)
	var body struct {
		SaaSQuota *int    `json:"saas_quota"`
		Blocked   *bool   `json:"blocked"`
		FullName  *string `json:"full_name"`
		SlackID   *string `json:"slack_id"`
		Notes     *string `json:"notes"`
		Company   *string `json:"company"`
		// Pointer like the rest, and for a sharper reason here: `""` is an
		// explicit CLEAR ("remove this country"), while an absent key means the
		// admin edited some other field and this one must not be touched. A
		// plain string would collapse the two and let a quota edit silently
		// wipe a country.
		Country *string `json:"country"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	// Whether the country branch below actually APPLIED, which is not the same
	// as the key being present: a stronger user-chosen value declines the edit.
	// Tracked so the audit line records what changed rather than what was asked.
	countryEdited := false
	if body.SaaSQuota != nil {
		u.SaaSQuota = *body.SaaSQuota
	}
	if body.Blocked != nil {
		u.Blocked = *body.Blocked
	}
	// Trim before capping so trailing whitespace does not eat the budget, and
	// so clearing a field to spaces stores "" (and drops out via omitempty).
	if body.FullName != nil {
		u.FullName = truncateRunes(strings.TrimSpace(*body.FullName), maxContactNameLen)
	}
	if body.SlackID != nil {
		u.SlackID = truncateRunes(strings.TrimSpace(*body.SlackID), maxContactSlackIDLen)
	}
	if body.Notes != nil {
		u.Notes = truncateRunes(strings.TrimSpace(*body.Notes), maxContactNotesLen)
	}
	if body.Company != nil {
		u.Company = truncateRunes(strings.TrimSpace(*body.Company), maxContactCompanyLen)
	}
	if body.Country != nil {
		raw := strings.TrimSpace(*body.Country)
		code := ""
		if raw != "" {
			// The SAME validator every other country path uses, so this
			// endpoint cannot drift into accepting a shape the render sites
			// reject. Not capped like the free-text fields above: a country is
			// two letters or it is rejected outright, so there is nothing to
			// truncate — a bad value must 400 rather than be silently reshaped
			// into a different country.
			code = normalizeCountryCode(raw)
			if code == "" {
				writeJSONError(w, http.StatusBadRequest, "country must be an ISO 3166-1 alpha-2 code (two letters), or \"\" to clear it")
				return
			}
		}
		// PROVENANCE — the whole point of this branch, and the easy thing to get
		// wrong. An admin edit is countrySourceAdmin, NEVER countrySourceUser:
		//
		//   - It must not claim the user chose this. They did not; an admin
		//     inferred it from a conference badge, an email domain, a
		//     conversation. Marking it user-chosen would fabricate a statement
		//     and would permanently suppress ever asking them for a real one.
		//   - It must still outrank the login-path Accept-Language inference,
		//     or the assignment is silently reverted the next time the user
		//     signs in from a differently-configured browser — the #4374 bug in
		//     a new form, and invisible in exactly the same way.
		//
		// mayOverwriteCountry is what keeps the admin from stepping on a value
		// the USER stated about themselves. It is not an error to try: the edit
		// is simply not applied to the country, the rest of the request still
		// lands, and the response is still a 200 — the admin has changed
		// nothing they were entitled to change. A 409 here would fail an
		// otherwise-valid multi-field save over a field the admin may not even
		// have meant to touch.
		if mayOverwriteCountry(u, countrySourceAdmin) {
			setUserCountry(u, code, countrySourceAdmin)
			countryEdited = true
		}
	}
	// A failed write is the one outcome the admin MUST hear about: the dashboard
	// closes the editor on a 2xx, so reporting success here after the PVC write
	// failed would silently discard the edit.
	if err := saveSaaSUser(u); err != nil {
		s.logger.Error("admin update user: save failed", "target", username, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to save user record")
		return
	}
	// Do not log the note bodies — they are free text and may hold anything an
	// admin jotted down. Log only that contact fields were touched.
	//
	// The country VALUE is logged, unlike those bodies: it is a two-letter code
	// from a closed shape, an admin assigning one on another person's behalf is
	// exactly the attribution an audit trail exists to record, and "who decided
	// this user is in GB" is unanswerable from a bare "countryEdited: true".
	// Logged only when the write actually applied, so the line never claims a
	// change that mayOverwriteCountry declined.
	attrs := []any{"target", username, "quota", u.SaaSQuota, "blocked", u.Blocked,
		"contactEdited", body.FullName != nil || body.SlackID != nil || body.Notes != nil || body.Company != nil}
	if countryEdited {
		attrs = append(attrs, "countryAssigned", u.Country, "countrySource", u.CountrySource)
	}
	s.logger.Info("audit: admin updated user", attrs...)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// handleAdminDeleteUser removes a hub user record. It refuses to delete the
// hub admin, and refuses to delete a user who still owns hosted hives — those
// must be deleted (or reassigned) first so no namespace is orphaned. Deleting
// a user does not touch GitHub; it only removes the hub's local account
// record (login state, quota, encrypted token).
func (s *HubServer) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if isHubAdmin(username) {
		writeJSONError(w, http.StatusForbidden, "cannot delete the hub admin")
		return
	}
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		writeJSONError(w, http.StatusBadRequest, "invalid username")
		return
	}
	u := loadSaaSUser(username)
	if u == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	var ownedHives []string
	for hiveID, role := range u.Hives {
		if role == "owner" {
			ownedHives = append(ownedHives, hiveID)
		}
	}
	if len(ownedHives) > 0 {
		writeJSONError(w, http.StatusConflict, fmt.Sprintf("user still owns %d hive(s); delete or reassign them first: %s",
			len(ownedHives), strings.Join(ownedHives, ", ")))
		return
	}
	path := filepath.Join(saasUsersDir, username+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("admin delete user: remove failed", "target", username, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to delete user record")
		return
	}
	s.logger.Info("audit: admin deleted user", "target", username, "by", s.getAuthUser(r))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}
