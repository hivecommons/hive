// Per-hive access management: authorized user lists, grantable
// users, access add/remove, and the access request
// (request/approve/deny) flow.
package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// authorizedUsersForHiveID returns the hub's access list for a hive as
// "username:role" entries, for delivery to the spoke in its heartbeat response.
// Returns nil (not an empty slice) when the hive has no SaaS record, so a
// non-hosted spoke's own allowlist is left untouched. The owner is always
// included as owner even if no explicit access record names them.
func authorizedUsersForHiveID(hiveID string) []string {
	users, _ := authorizedUsersAndNamesForHiveID(hiveID)
	return users
}

// authorizedUsersAndNamesForHiveID does the shared work behind
// authorizedUsersForHiveID and the heartbeat handler's AuthorizedUserNames
// delivery: one roster scan producing both the authoritative "username:role"
// allowlist and its cosmetic display-name companion, so the two can never
// drift out of sync with each other (different key sets, different order) by
// construction.
//
// The name map only gets an entry when provisionRequestUserIdentity resolves
// to something FRIENDLIER than the raw key itself (source != "native") — an
// entry with no known human name is simply absent, and the spoke's own
// rendering falls back to the raw key exactly as it does today for a key with
// no map entry at all.
func authorizedUsersAndNamesForHiveID(hiveID string) ([]string, map[string]string) {
	h := loadSaaSHive(hiveID)
	if h == nil {
		return nil, nil
	}
	out := make([]string, 0, 4)
	names := make(map[string]string, 4)
	seen := map[string]bool{}
	addName := func(key string, u *SaaSUser) {
		if id, source := provisionRequestUserIdentity(key, u, ""); source != "native" && id != key {
			names[key] = id
		}
	}
	if h.Owner != "" {
		out = append(out, h.Owner+":owner")
		seen[strings.ToLower(h.Owner)] = true
		addName(h.Owner, loadSaaSUser(h.Owner))
	}
	for _, u := range listAllSaaSUsers() {
		role, ok := u.Hives[hiveID]
		if !ok || u.GitHubUsername == "" {
			continue
		}
		if seen[strings.ToLower(u.GitHubUsername)] {
			continue // owner already added
		}
		out = append(out, u.GitHubUsername+":"+role)
		seen[strings.ToLower(u.GitHubUsername)] = true
		uu := u
		addName(u.GitHubUsername, &uu)
	}
	if len(names) == 0 {
		return out, nil
	}
	return out, names
}

// HiveAccessEntry is one user's access to a hive.
type HiveAccessEntry struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	// ExpiresAt is the grant's optional expiry (RFC3339 UTC, #4150) copied from
	// the user's HiveExpiry so Manage Access can show and edit it. omitempty —
	// a permanent grant renders exactly as before.
	ExpiresAt string `json:"expires_at,omitempty"`
	// Contact metadata copied from the user's record so the My Hives avatar hover
	// can show WHO someone is, not just their GitHub handle. FullName and SlackID
	// ride for every owner/admin-visible access row. Notes is admin-maintained CRM
	// scratch text (see the SaaSUser doc) and is therefore delivered ONLY to a hub
	// admin — accessForHive's includeNotes gate — so an owner never sees the
	// admin's private commentary about a co-member. All omitempty: a user with no
	// contact fields set renders exactly as before (handle — role).
	FullName string `json:"full_name,omitempty"`
	SlackID  string `json:"slack_id,omitempty"`
	Notes    string `json:"notes,omitempty"`
	// DisplayLabel is the human-facing name for this row, resolved with the
	// SAME precedence provisionRequestUserIdentity uses everywhere else
	// (linked GitHub login → recognizable GitHub login → email → DisplayName
	// → FullName → raw key) — never a second, competing resolver. Username
	// above stays the raw identity key throughout (the auth key / allowlist
	// match, completely unchanged); DisplayLabel is presentation only. Always
	// non-empty: it falls all the way back to Username, so the UI never has
	// to special-case "no name known" beyond comparing the two strings.
	DisplayLabel string `json:"display_label,omitempty"`
	// Provider is the identity provider ("github"/"google"/"ibmid"/"microsoft"/…)
	// so the row can show the right provider mark without re-deriving it from
	// Username client-side. See grantableUserProvider.
	Provider string `json:"provider,omitempty"`
	// AvatarURL is the provider-stored avatar (Google/Microsoft picture claim)
	// for a non-GitHub user; empty for a GitHub user, who keeps the derived
	// github.com/<login>.png the UI already builds from Username.
	AvatarURL string `json:"avatar_url,omitempty"`
	// Engagement stats copied from the user's record so a co-member's My-Hives
	// avatar hover can show the same logins / time-in-hive the admin Users card
	// shows. Like Notes these are stats ABOUT a person, so they ride ONLY for a
	// hub admin (accessForHive's includeAdminOnly gate) — a non-admin owner sees
	// name/Slack but not another member's engagement numbers. omitempty so a user
	// with no stats round-trips as today's handle — role tooltip.
	LoginCount     int   `json:"login_count,omitempty"`
	SessionSeconds int64 `json:"session_seconds,omitempty"`
	// Honest engagement signals (see SaaSUser for semantics) — admin-only like
	// the stats above, and omitempty/absent for records without data yet.
	EngagedSeconds int64  `json:"engaged_seconds,omitempty"`
	LastActionAt   string `json:"last_action_at,omitempty"`
	// LastActive is the RFC3339 time of this user's most recent hub activity —
	// the latest of their last login, last engaged beat, and last audited
	// action (see latestUserActivity). Unlike the engagement stats above it
	// rides for EVERY owner-visible row, not just for admins: "is this account
	// dormant?" is exactly the owner-level question the Manage Access list
	// answers when deciding on access changes (#4146). omitempty — absent means
	// the user has never been active, which the UI renders as "—". It also
	// feeds the Manage Access CSV export's last-active column (#4152).
	LastActive string `json:"last_active,omitempty"`
}

// latestUserActivity returns the RFC3339 timestamp of u's most recent hub
// activity — the latest of LastLogin, LastEngagedAt, and LastActionAt — or ""
// when the user has never been active. Timestamps are parsed rather than
// compared lexically so legacy records with mixed UTC offsets still order
// correctly; an unparseable value is skipped, never surfaced.
func latestUserActivity(u *SaaSUser) string {
	var best time.Time
	var bestStr string
	for _, ts := range []string{u.LastLogin, u.LastEngagedAt, u.LastActionAt} {
		if strings.TrimSpace(ts) == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		if t.After(best) {
			best = t
			bestStr = ts
		}
	}
	return bestStr
}

// accessForHive returns who can sign in to a hive, newest-role-agnostic and
// sorted for a stable render. Access is derived by scanning user records rather
// than reading h.Owner: a user's Hives map is the authoritative grant (see
// handleApproveProvision / handleAssignHive, which write it), and an owner who
// is missing from it genuinely cannot sign in.
// includeAdminOnly controls whether the admin-only fields — the CRM Notes and
// the engagement stats (LoginCount/SessionSeconds) — are copied onto each entry:
// pass true ONLY for a hub admin. FullName/SlackID always ride (they identify the
// person to a co-owner); Notes and the stats are private to admins.
func accessForHive(hiveID string, users []SaaSUser, includeAdminOnly bool) []HiveAccessEntry {
	access := make([]HiveAccessEntry, 0)
	for _, u := range users {
		if role, ok := u.Hives[hiveID]; ok {
			uu := u
			label, _ := provisionRequestUserIdentity(u.GitHubUsername, &uu, "")
			entry := HiveAccessEntry{
				Username:     u.GitHubUsername,
				Role:         role,
				ExpiresAt:    u.HiveExpiry[hiveID],
				FullName:     u.FullName,
				SlackID:      u.SlackID,
				DisplayLabel: label,
				Provider:     grantableUserProvider(&uu),
				AvatarURL:    u.AvatarURL,
				// Coarse last-active rides for every viewer of the row (see the
				// field doc) — only the granular stats below stay admin-only.
				LastActive: latestUserActivity(&u),
			}
			if includeAdminOnly {
				entry.Notes = u.Notes
				entry.LoginCount = u.LoginCount
				entry.SessionSeconds = u.SessionSeconds
				entry.EngagedSeconds = u.EngagedSeconds
				entry.LastActionAt = u.LastActionAt
			}
			access = append(access, entry)
		}
	}
	sort.Slice(access, func(i, j int) bool {
		// Owners first, then alphabetical — the owner is the useful line to read
		// first when scanning a hover with several users on it.
		if (access[i].Role == "owner") != (access[j].Role == "owner") {
			return access[i].Role == "owner"
		}
		return access[i].Username < access[j].Username
	})
	return access
}

// userIsHiveOwner reports whether username may administer hive h: the registry
// creator, any user holding the granted owner role on that hive, or the hub admin.
func userIsHiveOwner(username string, h *SaaSHive) bool {
	if username == "" || h == nil {
		return false
	}
	if isHubAdmin(username) {
		return true
	}
	if canonicalEqual(h.Owner, username) {
		return true
	}
	u := loadSaaSUser(username)
	return u != nil && u.Hives != nil && u.Hives[h.ID] == "owner"
}

// userOwnsHive reports whether username is the TRUE (canonical) owner of the
// hive identified by hiveID, resolving through the live registry first and
// the SaaS meta record second. Unlike userIsHiveOwner it never consults
// stored per-user roles or hub-admin status, so it is safe to use for
// owner-only role elevation without widening admin behavior (#4081).
func (s *HubServer) userOwnsHive(username, hiveID string) bool {
	if username == "" || hiveID == "" {
		return false
	}
	var regOwner string
	s.mu.Lock()
	for _, h := range s.registry.Hives {
		if h.ID == hiveID {
			regOwner = h.Owner
			break
		}
	}
	s.mu.Unlock()
	if regOwner != "" && canonicalEqual(regOwner, username) {
		return true
	}
	if sh := loadSaaSHive(hiveID); sh != nil && canonicalEqual(sh.Owner, username) {
		return true
	}
	return false
}

func (s *HubServer) handleAccessList(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can view access"}`, http.StatusForbidden)
		return
	}
	// Notes is admin-only; a non-admin owner viewing their hive's access gets
	// name+Slack but not the admin's private CRM notes.
	access := accessForHive(hiveID, listAllSaaSUsers(), isHubAdmin(username))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access": access})
}

// handleGrantableUsers lists the usernames a hive owner may grant access to.
// The Manage Access dropdown used to read /api/saas/admin/users, which is
// requireAdmin — so for every owner who is not the hub admin it 403'd and the
// dropdown silently rendered empty, making it look as though known users simply
// "weren't there". Owners legitimately need the roster to grant access, so this
// exposes exactly that and nothing else: usernames only, no emails, quotas, or
// hive assignments (which would leak the shape of other people's fleets).
func (s *HubServer) handleGrantableUsers(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	// Any user who owns at least one hive may see the roster; that is the same
	// bar as being able to open Manage Access at all. Admin always qualifies.
	owns := isHubAdmin(username)
	if !owns {
		for _, h := range listSaaSHives() {
			h := h
			if userIsHiveOwner(username, &h) {
				owns = true
				break
			}
		}
	}
	if !owns {
		http.Error(w, `{"error":"only hive owners can list users"}`, http.StatusForbidden)
		return
	}
	names := make([]string, 0)
	entries := make([]grantableUserEntry, 0)
	for _, u := range listAllSaaSUsers() {
		if u.GitHubUsername == "" {
			continue
		}
		u := u
		names = append(names, u.GitHubUsername)
		entries = append(entries, grantableUserEntry{
			ID:       u.GitHubUsername,
			Label:    grantableUserLabel(&u),
			Provider: grantableUserProvider(&u),
		})
	}
	sort.Strings(names)
	// Sort by the label an owner actually reads in the picker, falling back to
	// the stable ID so two identical display names still order deterministically.
	sort.Slice(entries, func(i, j int) bool {
		li, lj := strings.ToLower(entries[i].Label), strings.ToLower(entries[j].Label)
		if li != lj {
			return li < lj
		}
		return entries[i].ID < entries[j].ID
	})
	w.Header().Set("Content-Type", "application/json")
	// "users" (bare stable IDs) is kept for back-compat with older dashboards;
	// "entries" adds the normalized display label alongside the same stable ID
	// so the picker can show a human name while still granting by identity key.
	_ = json.NewEncoder(w).Encode(map[string]any{"users": names, "entries": entries})
}

// grantableUserEntry is one row of the Manage Access "Add User" picker: the
// stable identity key used for permission grants plus the friendly label an
// owner should see instead of a raw provider ID.
type grantableUserEntry struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider,omitempty"`
}

// identityProviderPrefixRE matches wire-form provider-prefixed identity keys
// like "google:107812...", "ibmid:6500...", "microsoft:AAAA...". A plain
// GitHub login can never match: GitHub logins cannot contain ":".
var identityProviderPrefixRE = regexp.MustCompile(`^([a-z][a-z0-9_-]*):(.+)$`)

// splitIdentityKey breaks a provider-prefixed identity key ("google:1078")
// into its provider and raw subject. A plain login returns ("", key).
func splitIdentityKey(key string) (provider, sub string) {
	if m := identityProviderPrefixRE.FindStringSubmatch(key); m != nil {
		return m[1], m[2]
	}
	return "", key
}

// normalizeIdentityProvider maps provider aliases onto their canonical name
// so the picker classifies "ms:…" identities the same as "microsoft:…" ones.
func normalizeIdentityProvider(p string) string {
	if p == "ms" {
		return "microsoft"
	}
	return p
}

// grantableUserProvider reports the identity provider for a roster entry,
// preferring the stored Provider field and falling back to the prefix of the
// identity key so legacy records still classify correctly.
func grantableUserProvider(u *SaaSUser) string {
	if u.Provider != "" {
		return normalizeIdentityProvider(u.Provider)
	}
	if p, _ := splitIdentityKey(u.GitHubUsername); p != "" {
		return normalizeIdentityProvider(p)
	}
	return "github"
}

// maxOpaqueIDLabelLen bounds how much of a raw opaque subject the fallback
// label shows before truncating — enough to disambiguate, short enough that a
// token-like Microsoft sub doesn't blow out the dropdown.
const maxOpaqueIDLabelLen = 12

// grantableUserLabel derives the friendly display label for a user in the
// Manage Access picker. It NEVER changes the identity key used for grants —
// display only. Preference order:
//  1. provider-asserted DisplayName from the OIDC name claim
//  2. a plain GitHub login (already human-recognizable)
//  3. the provider email claim (display only, never the key)
//  4. an optional linked GitHub login
//  5. a truncated "provider: subject…" rendering of the raw key, so even a
//     record with no human-readable claims stays scannable instead of a wall
//     of token characters.
func grantableUserLabel(u *SaaSUser) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	provider, sub := splitIdentityKey(u.GitHubUsername)
	if provider == "" || provider == "github" {
		return sub
	}
	if u.Email != "" {
		return u.Email
	}
	if u.LinkedGitHubLogin != "" {
		return u.LinkedGitHubLogin
	}
	if len(sub) > maxOpaqueIDLabelLen {
		sub = sub[:maxOpaqueIDLabelLen] + "…"
	}
	return provider + ": " + sub
}

func (s *HubServer) handleAccessAdd(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can manage access"}`, http.StatusForbidden)
		return
	}
	var body struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		// ExpiresAt is the grant's optional expiry (#4150). Pointer semantics:
		//   absent (nil)  → preserve the target's existing expiry, so a plain
		//                   role change never silently clears a time limit
		//   ""            → clear the expiry (grant becomes permanent)
		//   "YYYY-MM-DD"  → valid through that day, UTC
		//   RFC3339       → exact instant
		ExpiresAt *string `json:"expires_at"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" || body.Role == "" {
		http.Error(w, `{"error":"username and role required"}`, http.StatusBadRequest)
		return
	}
	if !config.ValidRole(body.Role) {
		http.Error(w, `{"error":"role must be read, read-write, merger, or owner"}`, http.StatusBadRequest)
		return
	}
	var expiresAt string
	if body.ExpiresAt != nil && strings.TrimSpace(*body.ExpiresAt) != "" {
		t, err := parseAccessExpiry(*body.ExpiresAt)
		if err != nil {
			http.Error(w, `{"error":"expiry must be a YYYY-MM-DD date or RFC3339 timestamp"}`, http.StatusBadRequest)
			return
		}
		if !t.After(time.Now()) {
			http.Error(w, `{"error":"expiry must be in the future"}`, http.StatusBadRequest)
			return
		}
		expiresAt = t.Format(time.RFC3339)
	}
	// An expiring owner grant on the hive's ONLY owner would auto-revoke into
	// an ownerless hive — the same orphaning handleAccessRemove refuses. Block
	// it here rather than special-casing the sweep, so the owner learns at set
	// time instead of the hive silently degrading later.
	if expiresAt != "" && body.Role == "owner" {
		ownerCount := 0
		for _, u := range listAllSaaSUsers() {
			if u.Hives[hiveID] == "owner" && !canonicalEqual(u.GitHubUsername, body.Username) {
				ownerCount++
			}
		}
		if ownerCount == 0 {
			http.Error(w, `{"error":"cannot set an expiry on the only owner"}`, http.StatusBadRequest)
			return
		}
	}
	target := ensureSaaSUser(body.Username)
	// Distinguish a fresh grant from a role change so the audit trail answers
	// "who changed X's role, from what, and when" (#4148) — a bare "granted as
	// merger" entry hides the fact the user was previously an owner.
	prevRole := target.Hives[hiveID]
	prevExpiry := target.HiveExpiry[hiveID]
	target.Hives[hiveID] = body.Role
	switch {
	case body.ExpiresAt == nil:
		// Preserve any existing expiry: a role change is not an extension.
	case expiresAt == "":
		delete(target.HiveExpiry, hiveID)
	default:
		if target.HiveExpiry == nil {
			target.HiveExpiry = map[string]string{}
		}
		target.HiveExpiry[hiveID] = expiresAt
	}
	if err := saveSaaSUser(target); err != nil {
		s.logger.Error("audit: access grant save failed", "hive", hiveID, "target", body.Username, "error", err)
		http.Error(w, `{"error":"failed to save access grant"}`, http.StatusInternalServerError)
		return
	}
	// The stored (possibly preserved) expiry after the update, for the audit
	// trail; "" means permanent.
	newExpiry := target.HiveExpiry[hiveID]
	auditExpiry := newExpiry
	if auditExpiry == "" {
		auditExpiry = "never"
	}
	expiryNote := ""
	if newExpiry != "" {
		expiryNote = " (expires " + newExpiry + ")"
	}
	switch {
	case prevRole == "":
		s.logger.Info("audit: access granted", "hive", hiveID, "target", body.Username, "role", body.Role, "expires", auditExpiry, "by", username)
		s.recordTimeline(hiveID, TimelineAccess,
			fmt.Sprintf("access granted to %s as %s%s", body.Username, body.Role, expiryNote), username)
	case prevRole != body.Role:
		s.logger.Info("audit: role changed", "hive", hiveID, "target", body.Username, "from", prevRole, "to", body.Role, "expires", auditExpiry, "by", username)
		s.recordTimeline(hiveID, TimelineAccess,
			fmt.Sprintf("role for %s changed: %s → %s%s", body.Username, prevRole, body.Role, expiryNote), username)
	case newExpiry != prevExpiry:
		// Expiry-only change (extend, shorten, or clear): still a permission
		// change, so it belongs in the append-only log like any other.
		detail := fmt.Sprintf("access expiry for %s cleared (now permanent)", body.Username)
		if newExpiry != "" {
			detail = fmt.Sprintf("access for %s now expires %s", body.Username, newExpiry)
		}
		s.logger.Info("audit: access expiry changed", "hive", hiveID, "target", body.Username, "expires", auditExpiry, "by", username)
		s.recordTimeline(hiveID, TimelineAccess, detail, username)
	default:
		// Same role re-granted: a no-op — do not pollute the append-only log.
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "granted"})
}

func (s *HubServer) handleAccessRemove(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	targetUsername := r.PathValue("username")
	username := s.getAuthUser(r)
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can manage access"}`, http.StatusForbidden)
		return
	}
	target := loadSaaSUser(targetUsername)
	if target == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}
	if target.Hives[hiveID] == "owner" {
		ownerCount := 0
		for _, u := range listAllSaaSUsers() {
			if u.Hives[hiveID] == "owner" {
				ownerCount++
			}
		}
		if ownerCount <= 1 {
			http.Error(w, `{"error":"at least one owner is required — cannot remove the last owner"}`, http.StatusBadRequest)
			return
		}
	}
	delete(target.Hives, hiveID)
	delete(target.HiveExpiry, hiveID)
	if err := saveSaaSUser(target); err != nil {
		s.logger.Error("audit: access revoke save failed", "hive", hiveID, "target", targetUsername, "error", err)
		http.Error(w, `{"error":"failed to save access revocation"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: access revoked", "hive", hiveID, "target", targetUsername, "by", username)
	s.recordTimeline(hiveID, TimelineAccess,
		fmt.Sprintf("access revoked from %s", targetUsername), username)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
}

type AccessRequest struct {
	Username    string `json:"username"`
	RequestedAt string `json:"requested_at"`
	Status      string `json:"status"`
	// Note is the free-text justification the requester must supply
	// explaining why they should be granted access. Shown to the
	// owner/approver when they review the request. May be empty on
	// legacy records created before this field existed.
	Note string `json:"note,omitempty"`
}

func loadAccessRequests(hiveID string) []AccessRequest {
	if strings.Contains(hiveID, "..") || strings.Contains(hiveID, "/") || strings.Contains(hiveID, "\\") {
		return nil
	}
	path := filepath.Join(saasHivesDir, hiveID, "requests.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var reqs []AccessRequest
	if err := json.Unmarshal(data, &reqs); err != nil {
		return nil
	}
	return reqs
}

func (s *HubServer) decoratePendingAccessRequests(reqs []PendingAccessRequest) []PendingAccessRequest {
	for i := range reqs {
		username := strings.TrimSpace(reqs[i].Username)
		if username == "" {
			continue
		}
		label, avatar := s.displayIdentity(username)
		reqs[i].DisplayLabel = label
		reqs[i].AvatarURL = avatar
		if u := loadSaaSUser(username); u != nil {
			reqs[i].Provider = grantableUserProvider(u)
			continue
		}
		if provider, _ := splitIdentityKey(username); provider != "" {
			reqs[i].Provider = normalizeIdentityProvider(provider)
		} else {
			reqs[i].Provider = legacyProvider
		}
	}
	return reqs
}

func saveAccessRequests(hiveID string, reqs []AccessRequest) {
	if strings.Contains(hiveID, "..") || strings.Contains(hiveID, "/") || strings.Contains(hiveID, "\\") {
		slog.Warn("saveAccessRequests: invalid hiveID", "hiveID", hiveID)
		return
	}
	dir := filepath.Join(saasHivesDir, hiveID)
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(dir, 0o755)
	data, err := json.MarshalIndent(reqs, "", "  ")
	if err != nil {
		slog.Warn("saveAccessRequests: marshal failed", "hiveID", hiveID, "error", err)
		return
	}
	path := filepath.Join(dir, "requests.json")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		slog.Warn("saveAccessRequests: write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		slog.Warn("saveAccessRequests: rename failed", "error", err)
	}
}

// maxAccessRequestNoteLen bounds the requester's justification note so a
// single request cannot bloat the stored requests.json.
const maxAccessRequestNoteLen = 2000

func (s *HubServer) handleRequestAccess(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	username := s.getAuthUser(r)

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}

	// The requester must supply a justification note explaining why they
	// need access; it is shown to the owner/approver on review.
	var body struct {
		Note string `json:"note"`
	}
	// Body is optional to decode (missing/invalid JSON leaves Note empty,
	// which the validation below rejects with a clear message).
	_ = json.NewDecoder(r.Body).Decode(&body)
	note := strings.TrimSpace(body.Note)
	if note == "" {
		http.Error(w, `{"error":"a note explaining why you need access is required"}`, http.StatusBadRequest)
		return
	}
	if len(note) > maxAccessRequestNoteLen {
		note = note[:maxAccessRequestNoteLen]
	}

	user := loadSaaSUser(username)
	if user != nil {
		if _, ok := user.Hives[hiveID]; ok {
			http.Error(w, `{"error":"you already have access"}`, http.StatusBadRequest)
			return
		}
	}

	reqs := loadAccessRequests(hiveID)
	for _, req := range reqs {
		if req.Username == username && req.Status == "pending" {
			http.Error(w, `{"error":"request already pending"}`, http.StatusBadRequest)
			return
		}
	}

	reqs = append(reqs, AccessRequest{
		Username:    username,
		RequestedAt: time.Now().UTC().Format(time.RFC3339),
		Status:      "pending",
		Note:        note,
	})
	saveAccessRequests(hiveID, reqs)

	// Push-notify the owner (Slack DM where configured — access_notify.go).
	// Placed after the pending-duplicate rejection above, so a request that was
	// already pending can never fire a second notification.
	s.notifyOwnerAccessRequest(hiveID, h.Owner, username, note)

	s.logger.Info("audit: access requested", "hive", hiveID, "by", username)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "requested"})
}

func (s *HubServer) handleGetRequests(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	username := s.getAuthUser(r)

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}

	user := loadSaaSUser(username)
	if user == nil {
		http.Error(w, `{"error":"not authorized"}`, http.StatusForbidden)
		return
	}
	role := user.Hives[hiveID]
	if !config.RoleAtLeast(role, config.RoleReadWrite) && !isHubAdmin(username) {
		http.Error(w, `{"error":"need owner or read-write access"}`, http.StatusForbidden)
		return
	}

	reqs := loadAccessRequests(hiveID)
	pending := make([]PendingAccessRequest, 0)
	for _, req := range reqs {
		if req.Status == "pending" {
			pending = append(pending, PendingAccessRequest{
				Username:    req.Username,
				RequestedAt: req.RequestedAt,
				Note:        req.Note,
			})
		}
	}
	pending = s.decoratePendingAccessRequests(pending)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"requests": pending})
}

func (s *HubServer) handleApproveRequest(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	targetUsername := r.PathValue("username")
	approver := s.getAuthUser(r)

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}

	approverUser := loadSaaSUser(approver)
	if approverUser == nil {
		http.Error(w, `{"error":"not authorized"}`, http.StatusForbidden)
		return
	}
	approverRole := approverUser.Hives[hiveID]
	if !config.RoleAtLeast(approverRole, config.RoleReadWrite) && !isHubAdmin(approver) {
		http.Error(w, `{"error":"need owner or read-write access"}`, http.StatusForbidden)
		return
	}

	var body struct {
		Role string `json:"role"`
	}
	// Body is optional to decode (missing/invalid JSON leaves Role empty,
	// defaulted to "read" below).
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Role == "" {
		body.Role = "read"
	}

	if !config.ValidRole(body.Role) {
		http.Error(w, `{"error":"role must be read, read-write, merger, or owner"}`, http.StatusBadRequest)
		return
	}
	if !isHubAdmin(approver) && config.RoleAtLeast(body.Role, approverRole) {
		http.Error(w, `{"error":"cannot grant a role equal to or higher than your own"}`, http.StatusForbidden)
		return
	}

	target := ensureSaaSUser(targetUsername)
	target.Hives[hiveID] = body.Role
	if err := saveSaaSUser(target); err != nil {
		s.logger.Error("audit: access request approval save failed", "hive", hiveID, "target", targetUsername, "error", err)
		http.Error(w, `{"error":"failed to save access grant"}`, http.StatusInternalServerError)
		return
	}

	reqs := loadAccessRequests(hiveID)
	for i := range reqs {
		if reqs[i].Username == targetUsername && reqs[i].Status == "pending" {
			reqs[i].Status = "approved"
		}
	}
	saveAccessRequests(hiveID, reqs)

	s.logger.Info("audit: access request approved", "hive", hiveID, "target", targetUsername, "role", body.Role, "by", approver)
	s.recordTimeline(hiveID, TimelineAccess,
		fmt.Sprintf("access request from %s approved as %s", targetUsername, body.Role), approver)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "approved"})
}

func (s *HubServer) handleDenyRequest(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	targetUsername := r.PathValue("username")
	denier := s.getAuthUser(r)

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}

	denierUser := loadSaaSUser(denier)
	if denierUser == nil {
		http.Error(w, `{"error":"not authorized"}`, http.StatusForbidden)
		return
	}
	denierRole := denierUser.Hives[hiveID]
	if !config.RoleAtLeast(denierRole, config.RoleReadWrite) && !isHubAdmin(denier) {
		http.Error(w, `{"error":"need owner or read-write access"}`, http.StatusForbidden)
		return
	}

	reqs := loadAccessRequests(hiveID)
	for i := range reqs {
		if reqs[i].Username == targetUsername && reqs[i].Status == "pending" {
			reqs[i].Status = "denied"
		}
	}
	saveAccessRequests(hiveID, reqs)

	s.logger.Info("audit: access request denied", "hive", hiveID, "target", targetUsername, "by", denier)
	s.recordTimeline(hiveID, TimelineAccess,
		fmt.Sprintf("access request from %s denied", targetUsername), denier)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "denied"})
}

func (s *HubServer) handleApproveAccess(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	targetUsername := r.PathValue("username")
	approver := s.getAuthUser(r)

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}

	approverUser := loadSaaSUser(approver)
	if approverUser == nil {
		http.Error(w, `{"error":"not authorized"}`, http.StatusForbidden)
		return
	}
	approverRole := approverUser.Hives[hiveID]
	if approverRole != "owner" && !isHubAdmin(approver) {
		http.Error(w, `{"error":"only the owner can approve access"}`, http.StatusForbidden)
		return
	}

	reqs := loadAccessRequests(hiveID)
	found := false
	for i := range reqs {
		if reqs[i].Username == targetUsername && reqs[i].Status == "pending" {
			reqs[i].Status = "approved"
			found = true
		}
	}
	if !found {
		http.Error(w, `{"error":"no pending request for this user"}`, http.StatusNotFound)
		return
	}
	saveAccessRequests(hiveID, reqs)

	const defaultApproveRole = "read"
	target := ensureSaaSUser(targetUsername)
	// Never demote the hive's TRUE owner (or an already-granted owner) to the
	// default role — this unconditional overwrite is how owners lost their
	// stored role and, with it, every owner-gated affordance (#4081).
	if target.Hives[hiveID] != "owner" && !canonicalEqual(h.Owner, targetUsername) {
		target.Hives[hiveID] = defaultApproveRole
	}
	if err := saveSaaSUser(target); err != nil {
		s.logger.Error("audit: access approve-via-PUT save failed", "hive", hiveID, "target", targetUsername, "error", err)
		http.Error(w, `{"error":"failed to save access grant"}`, http.StatusInternalServerError)
		return
	}

	s.logger.Info("audit: access approved via PUT", "hive", hiveID, "target", targetUsername, "by", approver)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *HubServer) handleDenyAccess(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	targetUsername := r.PathValue("username")
	denier := s.getAuthUser(r)

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}

	denierUser := loadSaaSUser(denier)
	if denierUser == nil {
		http.Error(w, `{"error":"not authorized"}`, http.StatusForbidden)
		return
	}
	denierRole := denierUser.Hives[hiveID]
	if denierRole != "owner" && !isHubAdmin(denier) {
		http.Error(w, `{"error":"only the owner can deny access"}`, http.StatusForbidden)
		return
	}

	reqs := loadAccessRequests(hiveID)
	for i := range reqs {
		if reqs[i].Username == targetUsername && reqs[i].Status == "pending" {
			reqs[i].Status = "denied"
		}
	}
	saveAccessRequests(hiveID, reqs)

	s.logger.Info("audit: access denied via DELETE", "hive", hiveID, "target", targetUsername, "by", denier)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}
