package hub

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/config"
)

var saasUsersDir = "/data/saas/users"

const hubAdminUsername = "clubanderson"

// hubUpgradeDebounce is the minimum gap between hub self-upgrade rollout
// restarts. The behind-latest check runs every SHA-poll cycle, so without this
// the hub could re-trigger a restart before the previous rollout's new pod
// reports the new hash. One cycle plus rollout headroom.
const hubUpgradeDebounce = 4 * time.Minute

// upgradeKubectlTimeout bounds a single auto-upgrade kubectl call. kubectl's own
// default retries an unreachable API server for ~2 minutes before giving up;
// paying that per hive serialized the upgrade loop and starved the hub's own
// upgrade check that runs after it. The heartbeat fallback is the real delivery
// path for unreachable clusters, so failing fast costs nothing.
const upgradeKubectlTimeout = 15 * time.Second

// clusterUnreachableTTL is how long the hub skips kubectl for a cluster after a
// dial failure. Long enough that one poll cycle probes a firewalled cluster at
// most once, short enough that a cluster coming back online is picked up soon.
const clusterUnreachableTTL = 10 * time.Minute

type SaaSUser struct {
	GitHubUsername string            `json:"github_username"`
	CreatedAt      string            `json:"created_at"`
	LastLogin      string            `json:"last_login"`
	Hives          map[string]string `json:"hives"`
	SaaSQuota      int               `json:"saas_quota"`
	Blocked        bool              `json:"blocked"`
	EncryptedToken string            `json:"encrypted_token,omitempty"`

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

var hmacKeyPath = "/data/saas/hmac.key"

const hmacKeySize = 32

func loadOrCreateHMACKey() ([]byte, error) {
	os.MkdirAll(filepath.Dir(hmacKeyPath), 0o755)
	if data, err := os.ReadFile(hmacKeyPath); err == nil && len(data) == hmacKeySize {
		return data, nil
	}
	key := make([]byte, hmacKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(hmacKeyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("write HMAC key: %w", err)
	}
	return key, nil
}

func encryptToken(plaintext string) (string, error) {
	key, err := loadOrCreateHMACKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decryptToken(encoded string) (string, error) {
	key, err := loadOrCreateHMACKey()
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (s *HubServer) registerSaaSRoutes() {
	s.mux.HandleFunc("GET /dashboard", s.handleDashboard)
	s.mux.HandleFunc("GET /access-denied", s.handleAccessDenied)
	s.mux.HandleFunc("GET /api/saas/my-hives", s.requireAuth(s.handleMyHives))
	// Token attribution rollups. requireAuth (not requireAdmin) because a
	// non-admin legitimately sees their OWN hives' usage; the handler scopes
	// fleet-wide data to admins itself.
	s.mux.HandleFunc("GET /api/saas/usage", s.requireAuth(s.handleUsage))
	s.mux.HandleFunc("POST /api/saas/hives", s.requireAuth(s.handleCreateHive))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/status", s.requireAuth(s.handleHiveStatus))
	// /open is a browser NAVIGATION endpoint (the SSO handoff), not an API call.
	// It is registered WITHOUT requireAuth so an unauthenticated visit redirects
	// to the hub login (and back) instead of dumping a raw {"error":...} JSON.
	// handleOpenHive does its own auth check + login redirect.
	s.mux.HandleFunc("GET /api/saas/hives/{id}/open", s.handleOpenHive)
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}", s.requireAuth(s.handleDeleteHive))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/upgrade", s.requireAuth(s.handleUpgradeHive))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/switch-branch", s.requireAuth(s.handleSwitchBranch))
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/visibility", s.requireAuth(s.handleToggleVisibility))
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/auto-upgrade", s.requireAuth(s.handleToggleAutoUpgrade))
	// Move a hive between forges (github.com <-> a GitHub Enterprise host).
	// requireAuth plus an inner owner-or-admin check, exactly like
	// switch-branch and auto-upgrade above.
	s.mux.HandleFunc("POST /api/saas/hives/{id}/forge", s.requireAuth(s.handleSwitchForge))
	s.mux.HandleFunc("GET /api/saas/hive-config/{hiveID}", s.requireAuth(s.handleProxyHiveConfig))
	s.mux.HandleFunc("GET /api/saas/latest-sha", s.handleLatestSHA)
	s.mux.HandleFunc("POST /api/saas/hub/upgrade", s.requireAdmin(s.handleHubSelfUpgrade))
	s.mux.HandleFunc("PUT /api/saas/hub/auto-upgrade", s.requireAdmin(s.handleHubAutoUpgrade))
	s.mux.HandleFunc("GET /api/saas/auth-check", s.handleSaaSAuthCheck)
	s.mux.HandleFunc("POST /api/saas/user-token", s.requireAuth(s.handleUserToken))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/access", s.requireAuth(s.handleAccessList))
	s.mux.HandleFunc("GET /api/saas/grantable-users", s.requireAuth(s.handleGrantableUsers))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/access", s.requireAuth(s.handleAccessAdd))
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}/access/{username}", s.requireAuth(s.handleAccessRemove))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/request-access", s.requireAuth(s.handleRequestAccess))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/requests", s.requireAuth(s.handleGetRequests))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/timeline", s.requireAuth(s.handleHiveTimeline))
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
	s.mux.HandleFunc("GET /api/saas/admin/users", s.requireAdmin(s.handleAdminUsers))
	s.mux.HandleFunc("PUT /api/saas/admin/users/{username}", s.requireAdmin(s.handleAdminUpdateUser))
	s.mux.HandleFunc("DELETE /api/saas/admin/users/{username}", s.requireAdmin(s.handleAdminDeleteUser))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/assign", s.requireAuth(s.handleAssignHive))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/migrate", s.requireAuth(s.handleMigrateHive))
	s.mux.HandleFunc("GET /api/saas/cluster-health", s.requireAdmin(s.handleClusterHealth))
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

	// Under `go test` these long-lived pollers leak across test cases: they
	// immediately hit the GitHub API and read the package-level saas path
	// variables that the filesystem test helper swaps per-test, which the
	// race detector rightly flags. Production behavior is unchanged; tests
	// that need poller logic call the functions directly.
	if !testing.Testing() {
		go s.startProvisionWatcher(context.Background())
		go s.StartLatestSHAPoller(context.Background())
		// Periodically probe every spoke's unauthenticated /api/status and alert
		// on any that answer 200 (wide open) — catches auth drift automatically.
		go s.StartAuthAudit(context.Background())
	}
}

func (s *HubServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isCSRFSafe(r) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"CSRF check failed"}`))
			return
		}
		username := s.getAuthUser(r)
		if username == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"not authenticated"}`))
			return
		}
		user := loadSaaSUser(username)
		if user == nil {
			ensureSaaSUser(username)
			user = loadSaaSUser(username)
		}
		if user == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unknown user — please log in again"}`))
			return
		}
		if user.Blocked {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"account blocked"}`))
			return
		}
		next(w, r)
	}
}

func isCSRFSafe(r *http.Request) bool {
	if r.Method == "GET" || r.Method == "HEAD" || r.Method == "OPTIONS" {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin != "" {
		return isTrustedOrigin(origin)
	}
	referer := r.Header.Get("Referer")
	if referer != "" {
		return isTrustedOrigin(referer)
	}
	ct := r.Header.Get("Content-Type")
	return strings.Contains(ct, "application/json")
}

func isTrustedOrigin(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "hive.kubestellar.io" ||
		strings.HasSuffix(host, ".hive.kubestellar.io") ||
		host == "localhost" ||
		host == "127.0.0.1"
}

func (s *HubServer) getAuthUser(r *http.Request) string {
	cookie, err := r.Cookie("hive_hub_user")
	if err == nil && cookie.Value != "" {
		// The cookie value is only trusted when its HMAC signature verifies
		// against the hub secret. A legacy unsigned cookie or a forged value
		// fails here and is treated as logged out, so the user re-authenticates
		// through the normal login flow (which re-mints a signed cookie).
		if username, ok := verifyHubUserCookieValue(s.hubSecret, cookie.Value); ok {
			if loadSaaSUser(username) != nil {
				return username
			}
		}
	}

	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		token := strings.TrimPrefix(auth, "Bearer ")
		if username := s.validateGitHubToken(token); username != "" {
			return username
		}
	}

	return ""
}

var (
	ghTokenCacheMu sync.RWMutex
	ghTokenCache   = map[string]ghTokenCacheEntry{}
)

const ghTokenCacheTTL = 5 * time.Minute

type ghTokenCacheEntry struct {
	username  string
	expiresAt time.Time
}

func (s *HubServer) validateGitHubToken(token string) string {
	if token == "" {
		return ""
	}

	ghTokenCacheMu.RLock()
	if entry, ok := ghTokenCache[token]; ok && time.Now().Before(entry.expiresAt) {
		ghTokenCacheMu.RUnlock()
		return entry.username
	}
	ghTokenCacheMu.RUnlock()

	client := &http.Client{Timeout: 10 * time.Second}
	// Hub always validates tokens against github.com (the hub is a SaaS service).
	req, err := http.NewRequest("GET", defaultGHUserURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()
	var user struct {
		Login string `json:"login"`
	}
	if json.NewDecoder(resp.Body).Decode(&user) != nil {
		return ""
	}

	ghTokenCacheMu.Lock()
	ghTokenCache[token] = ghTokenCacheEntry{username: user.Login, expiresAt: time.Now().Add(ghTokenCacheTTL)}
	ghTokenCacheMu.Unlock()

	return user.Login
}

func loadSaaSUser(username string) *SaaSUser {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return nil
	}
	path := filepath.Join(saasUsersDir, username+".json")
	data, err := os.ReadFile(path)
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
	return &u
}

func saveSaaSUser(u *SaaSUser) error {
	if strings.Contains(u.GitHubUsername, "..") || strings.Contains(u.GitHubUsername, "/") || strings.Contains(u.GitHubUsername, "\\") {
		return fmt.Errorf("invalid username for save: %q", u.GitHubUsername)
	}
	os.MkdirAll(saasUsersDir, 0o755)
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(saasUsersDir, u.GitHubUsername+".json")
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
	if username == hubAdminUsername {
		quota = -1
	}
	u = &SaaSUser{
		GitHubUsername: username,
		CreatedAt:      now,
		LastLogin:      now,
		Hives:          map[string]string{},
		SaaSQuota:      quota,
	}
	saveSaaSUser(u)
	return u
}

func listAllSaaSUsers() []SaaSUser {
	os.MkdirAll(saasUsersDir, 0o755)
	entries, err := os.ReadDir(saasUsersDir)
	if err != nil {
		return nil
	}
	var users []SaaSUser
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		u := loadSaaSUser(strings.TrimSuffix(e.Name(), ".json"))
		if u != nil {
			users = append(users, *u)
		}
	}
	return users
}

func (s *HubServer) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := s.getAuthUser(r)
		if username != hubAdminUsername {
			http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func (s *HubServer) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	users := listAllSaaSUsers()
	for i := range users {
		users[i].EncryptedToken = ""
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"users": users})
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
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
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
	s.logger.Info("audit: admin updated user", "target", username, "quota", u.SaaSQuota, "blocked", u.Blocked,
		"contactEdited", body.FullName != nil || body.SlackID != nil || body.Notes != nil)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// handleAdminDeleteUser removes a hub user record. It refuses to delete the
// hub admin, and refuses to delete a user who still owns hosted hives — those
// must be deleted (or reassigned) first so no namespace is orphaned. Deleting
// a user does not touch GitHub; it only removes the hub's local account
// record (login state, quota, encrypted token).
func (s *HubServer) handleAdminDeleteUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	if username == hubAdminUsername {
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
	json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
}

// --- Cluster Helpers ---

// clusterIDForSaaSHive returns the cluster ID for a SaaS hive, defaulting to
// the default cluster when the field is empty (backward compatibility).
func clusterIDForSaaSHive(sh SaaSHive) string {
	if sh.ClusterID != "" {
		return sh.ClusterID
	}
	return defaultClusterID
}

// ensureClusterIDForClaim stamps a non-blank cluster_id onto a hive that is
// about to be persisted by a claim/assign. This is the data-integrity guard
// for the observed bug where CLAIMED hives lost cluster_id in their meta.json
// and then fell back to defaultClusterID (hive-oke) in clusterForHive — which
// mis-routed App/host resolution for hives that actually run on vllm-d.
//
// Precedence, most-trusted first:
//  1. The hive's OWN non-blank ClusterID (the placeholder already belongs to a
//     cluster — always the most authoritative source; never override it).
//  2. poolFallback — the pool the claim was drawn from, when the caller knows
//     it (e.g. handleApproveProvision picks a pool by auth_method), but only
//     when it names a cluster the hub actually has.
//  3. defaultClusterID — last resort, matching clusterForHive's own fallback.
//
// The result is always non-blank, so with omitempty on the json tag it still
// serializes to a concrete "cluster_id" value rather than vanishing.
func (s *HubServer) ensureClusterIDForClaim(h *SaaSHive, poolFallback string) {
	if h.ClusterID != "" {
		return
	}
	if poolFallback != "" {
		if _, ok := s.clusters[poolFallback]; ok {
			h.ClusterID = poolFallback
			return
		}
	}
	h.ClusterID = defaultClusterID
}

// clusterNameForID returns the human-readable name for a cluster ID.
// Returns empty string when the cluster is not found.
func (s *HubServer) clusterNameForID(clusterID string) string {
	if c, ok := s.clusters[clusterID]; ok {
		return c.Name
	}
	return ""
}

// --- Cluster List API ---

// ClusterListEntry is the JSON response for the clusters list endpoint.
type ClusterListEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	HasGPU bool   `json:"has_gpu"`
	Arch   string `json:"arch"`
	// GitHubHost is the bare hostname of the GitHub instance hives on this
	// cluster default to ("github.com" for public GitHub). Shown in the
	// create-hive modal so an admin can see which GitHub a hive will target.
	GitHubHost string `json:"github_host,omitempty"`
	// AppInstallURL is the GitHub App install link for THIS cluster's GitHub
	// host and app slug. A GitHub Enterprise cluster must never be handed a
	// public github.com link: the install request would land on the wrong
	// GitHub and the GHE org admin would never see it.
	AppInstallURL string `json:"app_install_url,omitempty"`
}

// clusterGitHubConfig projects a cluster's GitHub settings onto the
// config.GitHubConfig that owns URL construction, so the hub and the spoke
// build install links from exactly one implementation.
func clusterGitHubConfig(c *ClusterConfig) config.GitHubConfig {
	if c == nil {
		return config.GitHubConfig{}
	}
	base := c.GitHubBaseURL
	// The cluster stores "" for public GitHub; config.GitHubConfig uses the
	// same convention, so pass it through untouched. Carry the api_url too so the
	// derived config's HostLabel()/IsGHE()/AppInstallURL() resolve the forge
	// base-or-api: a GHE cluster that records only an api_url (blank base_url —
	// the common state) is still recognised as GHE, not mislabelled github.com.
	return config.GitHubConfig{BaseURL: base, APIURL: c.GitHubAPIURL, AppSlug: c.GitHubAppSlug}
}

// githubHostLabel renders a GitHub base URL as a bare hostname for display.
// Empty (public GitHub) becomes "github.com" rather than an empty chip.
func githubHostLabel(baseURL string) string {
	h := strings.TrimSpace(baseURL)
	if h == "" {
		return "github.com"
	}
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	return strings.TrimRight(h, "/")
}

// GitHubHostLabel is the exported form of githubHostLabel, for the spoke to
// normalize its own configured GitHub base URL before reporting it over the
// heartbeat. Sharing one implementation keeps the value the spoke sends and
// the value the hub renders from drifting into two different spellings.
func GitHubHostLabel(baseURL string) string { return githubHostLabel(baseURL) }

func (s *HubServer) handleListClusters(w http.ResponseWriter, r *http.Request) {
	var entries []ClusterListEntry
	for _, c := range s.clusters {
		gh := clusterGitHubConfig(&c)
		entries = append(entries, ClusterListEntry{
			ID:            c.ID,
			Name:          c.Name,
			HasGPU:        c.HasGPU,
			Arch:          c.Arch,
			GitHubHost:    gh.HostLabel(),
			AppInstallURL: gh.AppInstallURL(),
		})
	}
	// Sort for deterministic API output.
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].ID < entries[j].ID
	})
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// --- Cluster Health ---

const clusterHealthCacheTTL = 30 * time.Second

// clusterHealthCPUWarnPct is the CPU usage percentage threshold for warning state.
const clusterHealthCPUWarnPct = 60

// clusterHealthCPUDangerPct is the CPU usage percentage threshold for danger state.
const clusterHealthCPUDangerPct = 80

// clusterHealthMemWarnPct is the memory usage percentage threshold for warning state.
const clusterHealthMemWarnPct = 60

// clusterHealthMemDangerPct is the memory usage percentage threshold for danger state.
const clusterHealthMemDangerPct = 80

// Disk thresholds are anchored to kubelet's own behaviour rather than to
// round numbers, so a coloured bar means something concrete is about to
// happen on the node:
//
//	evictionHard: nodefs.available<10%  -> kubelet starts evicting pods at
//	                                       90% used, so that is the danger line.
//	imageGCHighThresholdPercent: 85     -> kubelet begins garbage-collecting
//	                                       images at 85% used. That is the
//	                                       first automatic reaction to disk
//	                                       filling up, which makes it the
//	                                       right moment to warn an operator
//	                                       while there is still headroom.
//
// clusterHealthDiskWarnPct is the disk usage percentage at which kubelet's
// image garbage collection kicks in (imageGCHighThresholdPercent).
const clusterHealthDiskWarnPct = 85

// clusterHealthDiskDangerPct is the disk usage percentage at which kubelet's
// hard eviction threshold fires (nodefs.available<10%).
const clusterHealthDiskDangerPct = 90

// kubectlTopTimeoutSec is the timeout for kubectl top nodes commands.
const kubectlTopTimeoutSec = 10

// kubectlGetTimeoutSec is the timeout for kubectl get nodes commands.
const kubectlGetTimeoutSec = 10

// millicoresPerCore converts cores to millicores.
const millicoresPerCore = 1000

// mbPerGB converts megabytes to gigabytes.
const mbPerGB = 1024

// kiToBytes converts Ki units to bytes.
const kiToBytes = 1024

// miToBytes converts Mi units to bytes.
const miToBytes = 1024 * 1024

// giToBytes converts Gi units to bytes.
const giToBytes = 1024 * 1024 * 1024

// bytesPerMB converts bytes to megabytes.
const bytesPerMB = 1024 * 1024

// percentMultiplier converts a ratio to a percentage.
const percentMultiplier = 100

type ClusterHealthNode struct {
	Name          string `json:"name"`
	CPUCores      int    `json:"cpu_cores"`
	CPUUsedMillis int64  `json:"cpu_used_millicores"`
	CPUPercent    int    `json:"cpu_percent"`
	MemTotalMB    int64  `json:"mem_total_mb"`
	MemUsedMB     int64  `json:"mem_used_mb"`
	MemPercent    int    `json:"mem_percent"`
	DiskPressure  bool   `json:"disk_pressure"`
	// Disk fields describe the node filesystem (nodefs) that kubelet applies
	// its eviction thresholds to. They are pointers because live disk usage
	// comes from the kubelet stats/summary endpoint, which a hub may not be
	// able to reach; nil means "unknown" and must render as dashes rather
	// than as 0 (which would read as healthy).
	DiskTotalMB *int64 `json:"disk_total_mb,omitempty"`
	DiskUsedMB  *int64 `json:"disk_used_mb,omitempty"`
	DiskPercent *int   `json:"disk_percent,omitempty"`
	Pods        int    `json:"pods"`
	PodCapacity int    `json:"pod_capacity"`
	// HiveCount is the number of distinct hive-hosted-* namespaces with a
	// running pod on this node (namespaces, not pods, so a hive briefly
	// running two pods during a rollout is counted once).
	HiveCount  int      `json:"hive_count"`
	Conditions []string `json:"conditions"`
}

// hiveHostedNamespacePrefix is the namespace prefix used for SaaS-provisioned
// hives; pods in these namespaces identify hives running on a node.
const hiveHostedNamespacePrefix = "hive-hosted-"

type ClusterHealthSummary struct {
	TotalNodes    int `json:"total_nodes"`
	TotalCPUCores int `json:"total_cpu_cores"`
	TotalCPUPct   int `json:"total_cpu_percent"`
	TotalMemGB    int `json:"total_mem_gb"`
	TotalMemPct   int `json:"total_mem_percent"`
	// Disk totals cover only the nodes that reported live disk usage. They
	// are pointers so a cluster with no reachable kubelet stats endpoint
	// omits them entirely instead of reporting a misleading 0%.
	TotalDiskGB  *int `json:"total_disk_gb,omitempty"`
	TotalDiskPct *int `json:"total_disk_percent,omitempty"`
	HiveCount    int  `json:"hive_count"`
	// HiveCapacityRemaining estimates how many MORE hives the cluster can
	// hold: per Ready, schedulable node, the per-hive request footprint
	// (see hive_capacity.go) bin-packed into allocatable-minus-requested
	// capacity, summed across nodes. Pointer so it is omitted entirely when
	// pod request data was unavailable (nil = no data, 0 = cluster full).
	HiveCapacityRemaining *int `json:"hive_capacity_remaining,omitempty"`
}

// GPUSummary reports aggregate GPU counts for a cluster.
type GPUSummary struct {
	TotalGPUs       int `json:"total_gpus"`
	AllocatableGPUs int `json:"allocatable_gpus"`
}

// PerClusterHealth holds health data for a single cluster.
type PerClusterHealth struct {
	ID         string               `json:"id"`
	Name       string               `json:"name"`
	Nodes      []ClusterHealthNode  `json:"nodes"`
	Summary    ClusterHealthSummary `json:"summary"`
	GPUSummary *GPUSummary          `json:"gpu_summary,omitempty"`
	HiveCount  int                  `json:"hive_count"`
	Error      string               `json:"error,omitempty"`
	DataSource string               `json:"data_source,omitempty"` // "heartbeat" when data comes from spoke heartbeat instead of kubectl
	DataStale  bool                 `json:"data_stale,omitempty"`  // true when heartbeat data is older than heartbeatHealthStaleness
	DataAge    string               `json:"data_age,omitempty"`    // human-readable age or collection timestamp
}

type ClusterHealthResponse struct {
	// Flat fields for backward compatibility (aggregate across all clusters).
	Nodes   []ClusterHealthNode  `json:"nodes"`
	Summary ClusterHealthSummary `json:"summary"`
	// Per-cluster breakdown.
	Clusters []PerClusterHealth `json:"clusters,omitempty"`
}

var (
	clusterHealthCache     *ClusterHealthResponse
	clusterHealthCacheTime time.Time
	clusterHealthCacheMu   sync.Mutex
)

func (s *HubServer) handleClusterHealth(w http.ResponseWriter, r *http.Request) {
	clusterHealthCacheMu.Lock()
	if clusterHealthCache != nil && time.Since(clusterHealthCacheTime) < clusterHealthCacheTTL {
		cached := clusterHealthCache
		clusterHealthCacheMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cached)
		return
	}
	clusterHealthCacheMu.Unlock()

	resp, err := buildClusterHealth(s)
	if err != nil {
		s.logger.Error("cluster health failed", "error", err)
		http.Error(w, `{"error":"failed to gather cluster health"}`, http.StatusInternalServerError)
		return
	}

	clusterHealthCacheMu.Lock()
	clusterHealthCache = resp
	clusterHealthCacheTime = time.Now()
	clusterHealthCacheMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

const (
	// defaultClusterHealthQueryTimeout limits how long we wait for in-cluster
	// health queries when the cluster config does not override it.
	defaultClusterHealthQueryTimeout = 30 * time.Second
	// remoteClusterHealthQueryTimeout gives remote clusters more time because
	// they traverse an external network path from the hub pod.
	remoteClusterHealthQueryTimeout = 60 * time.Second
)

// gpuResourceKey is the extended resource name for NVIDIA GPUs on Kubernetes nodes.
const gpuResourceKey = "nvidia.com/gpu"

func buildClusterHealth(s *HubServer) (*ClusterHealthResponse, error) {
	// Count hives per cluster, deduplicated by hive ID. A hosted hive appears
	// both as a SaaS record and as a registry entry with a ClusterID (they
	// share the same ID), so counting both sources naively doubles the count.
	hiveIDsByCluster := make(map[string]map[string]bool)
	allHiveIDs := make(map[string]bool)
	addHive := func(clusterID, hiveID string) {
		if hiveIDsByCluster[clusterID] == nil {
			hiveIDsByCluster[clusterID] = make(map[string]bool)
		}
		hiveIDsByCluster[clusterID][hiveID] = true
		allHiveIDs[hiveID] = true
	}
	for _, sh := range listSaaSHives() {
		addHive(clusterIDForSaaSHive(sh), sh.ID)
	}
	s.mu.RLock()
	for _, h := range s.registry.Hives {
		if h.ClusterID != "" {
			addHive(h.ClusterID, h.ID)
		} else {
			// Self-hosted hives without a cluster still count globally.
			allHiveIDs[h.ID] = true
		}
	}
	s.mu.RUnlock()
	hiveCounts := make(map[string]int, len(hiveIDsByCluster))
	for cid, ids := range hiveIDsByCluster {
		hiveCounts[cid] = len(ids)
	}
	totalHiveCount := len(allHiveIDs)

	// Query all clusters in parallel.
	type clusterResult struct {
		health PerClusterHealth
		err    error
	}
	type clusterQuery struct {
		cluster ClusterConfig
		ch      chan clusterResult
	}
	results := make(map[string]clusterQuery)
	for _, c := range s.clusters {
		ch := make(chan clusterResult, 1)
		results[c.ID] = clusterQuery{cluster: c, ch: ch}
		go func(cluster ClusterConfig) {
			health, err := buildSingleClusterHealth(&cluster, hiveCounts[cluster.ID], s.logger)
			ch <- clusterResult{health: health, err: err}
		}(c)
	}

	// Collect results with timeout.
	var allNodes []ClusterHealthNode
	var perCluster []PerClusterHealth
	var aggCPUCores int
	var aggCPUUsed int64
	var aggCPUAlloc int64
	var aggMemAlloc int64
	var aggMemUsed int64

	for cID, query := range results {
		timeout := clusterHealthQueryTimeoutFor(&query.cluster)
		select {
		case res := <-query.ch:
			if res.err != nil {
				s.logger.Warn("cluster health query failed", "cluster", cID, "error", res.err)
				// Fall back to heartbeat-reported health if available.
				if hbHealth := s.getHeartbeatHealthForCluster(cID); hbHealth != nil {
					pch := convertHeartbeatToPerClusterHealth(cID, s.clusterNameForID(cID), hbHealth, hiveCounts[cID])
					perCluster = append(perCluster, pch)
					allNodes = append(allNodes, pch.Nodes...)
					aggCPUCores += pch.Summary.TotalCPUCores
					aggCPUUsed += int64(pch.Summary.TotalCPUPct) * int64(pch.Summary.TotalCPUCores) * millicoresPerCore / percentMultiplier
					aggCPUAlloc += int64(pch.Summary.TotalCPUCores) * millicoresPerCore
					aggMemAlloc += int64(pch.Summary.TotalMemGB) * giToBytes
					aggMemUsed += int64(pch.Summary.TotalMemPct) * int64(pch.Summary.TotalMemGB) * giToBytes / percentMultiplier
					s.logger.Info("cluster health: using heartbeat fallback", "cluster", cID)
					continue
				}
				perCluster = append(perCluster, PerClusterHealth{
					ID:    cID,
					Name:  s.clusterNameForID(cID),
					Error: res.err.Error(),
				})
				continue
			}
			pch := res.health
			pch.ID = cID
			pch.Name = s.clusterNameForID(cID)
			perCluster = append(perCluster, pch)
			allNodes = append(allNodes, pch.Nodes...)
			aggCPUCores += pch.Summary.TotalCPUCores
			aggCPUUsed += int64(pch.Summary.TotalCPUPct) * int64(pch.Summary.TotalCPUCores) * millicoresPerCore / percentMultiplier
			aggCPUAlloc += int64(pch.Summary.TotalCPUCores) * millicoresPerCore
			aggMemAlloc += int64(pch.Summary.TotalMemGB) * giToBytes
			aggMemUsed += int64(pch.Summary.TotalMemPct) * int64(pch.Summary.TotalMemGB) * giToBytes / percentMultiplier
		case <-time.After(timeout):
			s.logger.Warn("cluster health query timed out", "cluster", cID, "timeout", timeout.String())
			// Fall back to heartbeat-reported health if available.
			if hbHealth := s.getHeartbeatHealthForCluster(cID); hbHealth != nil {
				pch := convertHeartbeatToPerClusterHealth(cID, s.clusterNameForID(cID), hbHealth, hiveCounts[cID])
				perCluster = append(perCluster, pch)
				allNodes = append(allNodes, pch.Nodes...)
				aggCPUCores += pch.Summary.TotalCPUCores
				aggCPUUsed += int64(pch.Summary.TotalCPUPct) * int64(pch.Summary.TotalCPUCores) * millicoresPerCore / percentMultiplier
				aggCPUAlloc += int64(pch.Summary.TotalCPUCores) * millicoresPerCore
				aggMemAlloc += int64(pch.Summary.TotalMemGB) * giToBytes
				aggMemUsed += int64(pch.Summary.TotalMemPct) * int64(pch.Summary.TotalMemGB) * giToBytes / percentMultiplier
				s.logger.Info("cluster health: using heartbeat fallback after timeout", "cluster", cID)
				continue
			}
			perCluster = append(perCluster, PerClusterHealth{
				ID:    cID,
				Name:  s.clusterNameForID(cID),
				Error: "query timed out",
			})
		}
	}

	aggCPUPct := 0
	if aggCPUAlloc > 0 {
		aggCPUPct = int(aggCPUUsed * percentMultiplier / aggCPUAlloc)
	}
	aggMemPct := 0
	if aggMemAlloc > 0 {
		aggMemPct = int(aggMemUsed * percentMultiplier / aggMemAlloc)
	}
	aggMemGB := int(aggMemAlloc / giToBytes)

	// Include heartbeat-only clusters that are NOT in s.clusters but do have
	// heartbeat-reported health data. This handles firewalled spokes whose
	// cluster isn't in the hub's clusters.json.
	clusterSeen := make(map[string]bool, len(results))
	for cID := range results {
		clusterSeen[cID] = true
	}
	s.heartbeatHealthMu.RLock()
	for cID, entry := range s.heartbeatHealth {
		if clusterSeen[cID] || entry == nil || entry.Report == nil {
			continue
		}
		pch := convertHeartbeatToPerClusterHealth(cID, s.clusterNameForID(cID), entry, hiveCounts[cID])
		perCluster = append(perCluster, pch)
		allNodes = append(allNodes, pch.Nodes...)
		aggCPUCores += pch.Summary.TotalCPUCores
		aggCPUUsed += int64(pch.Summary.TotalCPUPct) * int64(pch.Summary.TotalCPUCores) * millicoresPerCore / percentMultiplier
		aggCPUAlloc += int64(pch.Summary.TotalCPUCores) * millicoresPerCore
		aggMemAlloc += int64(pch.Summary.TotalMemGB) * giToBytes
		aggMemUsed += int64(pch.Summary.TotalMemPct) * int64(pch.Summary.TotalMemGB) * giToBytes / percentMultiplier
	}
	s.heartbeatHealthMu.RUnlock()

	// Recompute aggregates after including heartbeat-only clusters.
	aggCPUPct = 0
	if aggCPUAlloc > 0 {
		aggCPUPct = int(aggCPUUsed * percentMultiplier / aggCPUAlloc)
	}
	aggMemPct = 0
	if aggMemAlloc > 0 {
		aggMemPct = int(aggMemUsed * percentMultiplier / aggMemAlloc)
	}
	aggMemGB = int(aggMemAlloc / giToBytes)

	// Sort clusters by ID for deterministic output.
	sort.Slice(perCluster, func(i, j int) bool {
		return perCluster[i].ID < perCluster[j].ID
	})

	// Every collection path above appends its nodes to allNodes, so the fleet
	// disk total is derived from them directly. Nodes with no disk data are
	// skipped, so an unreachable cluster lowers coverage without skewing the
	// percentage; if no node anywhere reported, disk is omitted entirely.
	aggDiskGB, aggDiskPct := summarizeDisk(allNodes)

	return &ClusterHealthResponse{
		Nodes: allNodes,
		Summary: ClusterHealthSummary{
			TotalNodes:    len(allNodes),
			TotalCPUCores: aggCPUCores,
			TotalCPUPct:   aggCPUPct,
			TotalMemGB:    aggMemGB,
			TotalMemPct:   aggMemPct,
			TotalDiskGB:   aggDiskGB,
			TotalDiskPct:  aggDiskPct,
			HiveCount:     totalHiveCount,
		},
		Clusters: perCluster,
	}, nil
}

func clusterHealthQueryTimeoutFor(cluster *ClusterConfig) time.Duration {
	if cluster != nil && cluster.ClusterHealthTimeoutSeconds > 0 {
		return time.Duration(cluster.ClusterHealthTimeoutSeconds) * time.Second
	}
	if cluster != nil && !cluster.InCluster {
		return remoteClusterHealthQueryTimeout
	}
	return defaultClusterHealthQueryTimeout
}

// buildSingleClusterHealth queries a single cluster for node health data.
func buildSingleClusterHealth(cluster *ClusterConfig, hiveCount int, logger *slog.Logger) (PerClusterHealth, error) {
	timeout := clusterHealthQueryTimeoutFor(cluster)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Run kubectl top nodes
	topCmd := kubectlForClusterContext(ctx, cluster, "--request-timeout", timeout.String(), "top", "nodes", "--no-headers")
	topOut, err := topCmd.CombinedOutput()
	if err != nil {
		return PerClusterHealth{}, fmt.Errorf("kubectl top nodes on %s: exit status 1: %s", cluster.ID, string(topOut))
	}

	// Run kubectl get nodes -o json
	getCmd := kubectlForClusterContext(ctx, cluster, "--request-timeout", timeout.String(), "get", "nodes", "-o", "json")
	getOut, err := getCmd.CombinedOutput()
	if err != nil {
		return PerClusterHealth{}, fmt.Errorf("kubectl get nodes on %s: %w: %s", cluster.ID, err, string(getOut))
	}

	// Parse kubectl get nodes output
	var nodesJSON struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Allocatable map[string]string `json:"allocatable"`
				Capacity    map[string]string `json:"capacity"`
				Conditions  []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(getOut, &nodesJSON); err != nil {
		return PerClusterHealth{}, fmt.Errorf("parse nodes JSON on %s: %w", cluster.ID, err)
	}

	// Build a map of node info from kubectl get.
	type nodeInfo struct {
		cpuAllocatable int64 // millicores
		memAllocatable int64 // bytes
		podCapacity    int
		diskPressure   bool
		ready          bool
		unschedulable  bool // cordoned; excluded from hive capacity estimates
		conditions     []string
		gpuCapacity    int
		gpuAllocatable int
	}
	nodeMap := make(map[string]*nodeInfo)
	var totalGPUCapacity, totalGPUAllocatable int
	for _, item := range nodesJSON.Items {
		ni := &nodeInfo{}
		// Allocatable (not raw capacity) is what the scheduler can place
		// pods against, so hive capacity math below uses these values.
		ni.cpuAllocatable = parseK8sCPU(item.Status.Allocatable["cpu"])
		ni.memAllocatable = parseK8sMemory(item.Status.Allocatable["memory"])
		ni.podCapacity = parseInt(item.Status.Capacity["pods"])
		ni.gpuCapacity = parseInt(item.Status.Capacity[gpuResourceKey])
		ni.gpuAllocatable = parseInt(item.Status.Allocatable[gpuResourceKey])
		ni.unschedulable = item.Spec.Unschedulable
		totalGPUCapacity += ni.gpuCapacity
		totalGPUAllocatable += ni.gpuAllocatable
		for _, cond := range item.Status.Conditions {
			if cond.Type == "DiskPressure" && cond.Status == "True" {
				ni.diskPressure = true
			}
			if cond.Type == "Ready" && cond.Status == "True" {
				ni.ready = true
				ni.conditions = append(ni.conditions, "Ready")
			} else if cond.Type == "Ready" && cond.Status != "True" {
				ni.conditions = append(ni.conditions, "NotReady")
			}
		}
		if len(ni.conditions) == 0 {
			ni.conditions = []string{"Unknown"}
		}
		nodeMap[item.Metadata.Name] = ni
	}

	// Parse kubectl top nodes output.
	// Format: NAME  CPU(cores)  CPU%  MEMORY(bytes)  MEMORY%
	var nodes []ClusterHealthNode
	lines := strings.Split(strings.TrimSpace(string(topOut)), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		const topFieldCount = 5
		if len(fields) < topFieldCount {
			continue
		}
		name := fields[0]
		cpuUsed := parseTopCPU(fields[1])
		memUsed := parseTopMemory(fields[3])

		ni, ok := nodeMap[name]
		if !ok {
			continue
		}

		cpuCores := int(ni.cpuAllocatable / millicoresPerCore)
		cpuPct := 0
		if ni.cpuAllocatable > 0 {
			cpuPct = int(cpuUsed * percentMultiplier / ni.cpuAllocatable)
		}

		memTotalMB := ni.memAllocatable / bytesPerMB
		memUsedMB := memUsed / bytesPerMB
		memPct := 0
		if ni.memAllocatable > 0 {
			memPct = int(memUsed * percentMultiplier / ni.memAllocatable)
		}

		nodes = append(nodes, ClusterHealthNode{
			Name:          name,
			CPUCores:      cpuCores,
			CPUUsedMillis: cpuUsed,
			CPUPercent:    cpuPct,
			MemTotalMB:    memTotalMB,
			MemUsedMB:     memUsedMB,
			MemPercent:    memPct,
			DiskPressure:  ni.diskPressure,
			Pods:          0, // populated below
			PodCapacity:   ni.podCapacity,
			Conditions:    ni.conditions,
		})
	}

	// Collect LIVE node filesystem usage from each kubelet's stats/summary
	// endpoint. This is best-effort per node: a node whose kubelet proxy is
	// unreachable simply keeps nil disk fields and renders as unknown, which
	// must never degrade the rest of this cluster's health data.
	for i := range nodes {
		rawStats, statsErr := kubectlForClusterContext(ctx, cluster,
			"--request-timeout", timeout.String(),
			"get", "--raw", nodeStatsSummaryPath(nodes[i].Name)).Output()
		if statsErr != nil {
			if logger != nil {
				logger.Debug("cluster health: node disk stats unavailable",
					"cluster", cluster.ID, "node", nodes[i].Name, "error", statsErr)
			}
			continue
		}
		if usage, ok := parseNodeStatsSummaryDisk(rawStats); ok {
			applyNodeDiskUsage(&nodes[i], usage)
		}
	}

	// Count running pods per node and sum their container resource REQUESTS
	// (requests, not usage — that is what the scheduler bin-packs against).
	// Listing only Running pods slightly undercounts requests (Pending pods
	// already assigned to a node are missed), so the capacity estimate below
	// can be marginally optimistic.
	var cpuRequestedPerNode, memRequestedPerNode map[string]int64
	podOut, _ := kubectlForCluster(cluster, "get", "pods", "--all-namespaces", "--field-selector=status.phase=Running", "-o", "json").Output()
	if len(podOut) > 0 {
		var podsJSON struct {
			Items []struct {
				Metadata struct {
					Namespace string `json:"namespace"`
				} `json:"metadata"`
				Spec struct {
					NodeName   string `json:"nodeName"`
					Containers []struct {
						Resources struct {
							Requests map[string]string `json:"requests"`
						} `json:"resources"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"items"`
		}
		if json.Unmarshal(podOut, &podsJSON) == nil {
			podCounts := make(map[string]int)
			cpuRequestedPerNode = make(map[string]int64)
			memRequestedPerNode = make(map[string]int64)
			// hiveNamespacesPerNode tracks distinct hive-hosted-* namespaces per
			// node so each hive is counted once even with multiple pods.
			hiveNamespacesPerNode := make(map[string]map[string]bool)
			for _, p := range podsJSON.Items {
				podCounts[p.Spec.NodeName]++
				for _, c := range p.Spec.Containers {
					cpuRequestedPerNode[p.Spec.NodeName] += parseK8sCPU(c.Resources.Requests["cpu"])
					memRequestedPerNode[p.Spec.NodeName] += parseK8sMemory(c.Resources.Requests["memory"])
				}
				if strings.HasPrefix(p.Metadata.Namespace, hiveHostedNamespacePrefix) {
					if hiveNamespacesPerNode[p.Spec.NodeName] == nil {
						hiveNamespacesPerNode[p.Spec.NodeName] = make(map[string]bool)
					}
					hiveNamespacesPerNode[p.Spec.NodeName][p.Metadata.Namespace] = true
				}
			}
			for i := range nodes {
				nodes[i].Pods = podCounts[nodes[i].Name]
				nodes[i].HiveCount = len(hiveNamespacesPerNode[nodes[i].Name])
			}
		}
	}

	// Estimate remaining hive capacity: bin-pack the per-hive request
	// footprint into each Ready, schedulable node's free (allocatable minus
	// requested) capacity. Only computed when the pod listing above parsed,
	// since without per-node requests the estimate would be meaningless.
	var hiveCapacityRemaining *int
	if cpuRequestedPerNode != nil {
		var totalSlots int64
		for name, ni := range nodeMap {
			totalSlots += hiveSlotsForNode(ni.cpuAllocatable, ni.memAllocatable,
				cpuRequestedPerNode[name], memRequestedPerNode[name], ni.ready, ni.unschedulable)
		}
		slots := int(totalSlots)
		hiveCapacityRemaining = &slots
	}

	// Build summary.
	var totalCPUCores int
	var totalCPUUsed int64
	var totalCPUAlloc int64
	var totalMemAlloc int64
	var totalMemUsed int64
	for _, n := range nodes {
		totalCPUCores += n.CPUCores
		totalCPUUsed += n.CPUUsedMillis
		if ni, ok := nodeMap[n.Name]; ok {
			totalCPUAlloc += ni.cpuAllocatable
			totalMemAlloc += ni.memAllocatable
		}
		totalMemUsed += n.MemUsedMB * bytesPerMB
	}

	totalCPUPct := 0
	if totalCPUAlloc > 0 {
		totalCPUPct = int(totalCPUUsed * percentMultiplier / totalCPUAlloc)
	}
	totalMemPct := 0
	if totalMemAlloc > 0 {
		totalMemPct = int(totalMemUsed * percentMultiplier / totalMemAlloc)
	}
	totalMemGB := int(totalMemAlloc / giToBytes)
	totalDiskGB, totalDiskPct := summarizeDisk(nodes)

	result := PerClusterHealth{
		Nodes: nodes,
		Summary: ClusterHealthSummary{
			TotalNodes:            len(nodes),
			TotalCPUCores:         totalCPUCores,
			TotalCPUPct:           totalCPUPct,
			TotalMemGB:            totalMemGB,
			TotalMemPct:           totalMemPct,
			TotalDiskGB:           totalDiskGB,
			TotalDiskPct:          totalDiskPct,
			HiveCount:             hiveCount,
			HiveCapacityRemaining: hiveCapacityRemaining,
		},
		HiveCount: hiveCount,
	}

	// Include GPU summary for clusters with GPUs.
	if totalGPUCapacity > 0 {
		result.GPUSummary = &GPUSummary{
			TotalGPUs:       totalGPUCapacity,
			AllocatableGPUs: totalGPUAllocatable,
		}
	}

	return result, nil
}

// getHeartbeatHealthForCluster retrieves the latest heartbeat-reported health
// for a cluster. Returns nil if no data exists or the data is too old.
func (s *HubServer) getHeartbeatHealthForCluster(clusterID string) *HeartbeatHealthEntry {
	s.heartbeatHealthMu.RLock()
	entry, ok := s.heartbeatHealth[clusterID]
	s.heartbeatHealthMu.RUnlock()
	if !ok || entry == nil || entry.Report == nil {
		return nil
	}
	return entry
}

// convertHeartbeatToPerClusterHealth converts heartbeat-reported health data
// into the hub's PerClusterHealth format for display. If the data is older
// than heartbeatHealthStaleness, it is marked with a staleness warning.
func convertHeartbeatToPerClusterHealth(clusterID, clusterName string, entry *HeartbeatHealthEntry, hiveCount int) PerClusterHealth {
	report := entry.Report

	// Convert HeartbeatNodeMetric to ClusterHealthNode.
	nodes := make([]ClusterHealthNode, len(report.Nodes))
	for i, n := range report.Nodes {
		nodes[i] = ClusterHealthNode{
			Name:          n.Name,
			CPUCores:      n.CPUCores,
			CPUUsedMillis: n.CPUUsedMillis,
			CPUPercent:    n.CPUPercent,
			MemTotalMB:    n.MemTotalMB,
			MemUsedMB:     n.MemUsedMB,
			MemPercent:    n.MemPercent,
			DiskPressure:  n.DiskPressure,
			DiskTotalMB:   n.DiskTotalMB,
			DiskUsedMB:    n.DiskUsedMB,
			DiskPercent:   n.DiskPercent,
			Pods:          n.Pods,
			PodCapacity:   n.PodCapacity,
			HiveCount:     n.HiveCount,
			Conditions:    n.Conditions,
		}
	}

	pch := PerClusterHealth{
		ID:    clusterID,
		Name:  clusterName,
		Nodes: nodes,
		Summary: ClusterHealthSummary{
			TotalNodes:    report.Summary.TotalNodes,
			TotalCPUCores: report.Summary.TotalCPUCores,
			TotalCPUPct:   report.Summary.TotalCPUPct,
			TotalMemGB:    report.Summary.TotalMemGB,
			TotalMemPct:   report.Summary.TotalMemPct,
			// nil for spokes that could not read kubelet disk stats.
			TotalDiskGB:  report.Summary.TotalDiskGB,
			TotalDiskPct: report.Summary.TotalDiskPct,
			HiveCount:    hiveCount,
			// nil for spokes running older builds that do not report it.
			HiveCapacityRemaining: report.Summary.HiveCapacityRemaining,
		},
		HiveCount:  hiveCount,
		DataSource: "heartbeat",
	}

	// Mark staleness if heartbeat data is too old.
	age := time.Since(entry.ReceivedAt)
	if age > heartbeatHealthStaleness {
		pch.DataStale = true
		pch.DataAge = fmt.Sprintf("%dm ago", int(age.Minutes()))
	} else if report.CollectedAt != "" {
		pch.DataAge = report.CollectedAt
	}

	// Convert GPU summary.
	if report.GPUSummary != nil {
		pch.GPUSummary = &GPUSummary{
			TotalGPUs:       report.GPUSummary.Total,
			AllocatableGPUs: report.GPUSummary.Total - report.GPUSummary.Allocated,
		}
	}

	return pch
}

// nodeDiskUsage holds live node filesystem usage for one node.
type nodeDiskUsage struct {
	usedBytes     int64
	capacityBytes int64
}

// nodeStatsSummaryPath builds the kubelet stats/summary proxy path for a node.
// This endpoint is the only source of LIVE disk usage: the node object's
// capacity/allocatable["ephemeral-storage"] reports the declared size only and
// says nothing about how full the filesystem actually is.
func nodeStatsSummaryPath(nodeName string) string {
	return "/api/v1/nodes/" + nodeName + "/proxy/stats/summary"
}

// parseNodeStatsSummaryDisk extracts node filesystem usage from a kubelet
// stats/summary response. node.fs is the nodefs that kubelet's
// evictionHard nodefs.available threshold applies to.
func parseNodeStatsSummaryDisk(raw []byte) (nodeDiskUsage, bool) {
	var summary struct {
		Node struct {
			FS struct {
				UsedBytes     *int64 `json:"usedBytes"`
				CapacityBytes *int64 `json:"capacityBytes"`
			} `json:"fs"`
		} `json:"node"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nodeDiskUsage{}, false
	}
	fs := summary.Node.FS
	// Both values are required: without capacity there is no percentage, and
	// a missing usedBytes must not be treated as zero usage.
	if fs.UsedBytes == nil || fs.CapacityBytes == nil || *fs.CapacityBytes <= 0 {
		return nodeDiskUsage{}, false
	}
	return nodeDiskUsage{usedBytes: *fs.UsedBytes, capacityBytes: *fs.CapacityBytes}, true
}

// applyNodeDiskUsage fills the disk fields on a health node from live usage.
// Nodes with no usable stats keep nil disk fields and render as unknown.
func applyNodeDiskUsage(n *ClusterHealthNode, d nodeDiskUsage) {
	totalMB := d.capacityBytes / bytesPerMB
	usedMB := d.usedBytes / bytesPerMB
	pct := int(d.usedBytes * percentMultiplier / d.capacityBytes)
	n.DiskTotalMB = &totalMB
	n.DiskUsedMB = &usedMB
	n.DiskPercent = &pct
}

// summarizeDisk aggregates per-node disk usage into cluster totals, counting
// only nodes that actually reported usage. Returns nil,nil when no node did,
// so the UI omits disk for that cluster rather than showing a false 0%.
func summarizeDisk(nodes []ClusterHealthNode) (*int, *int) {
	var totalBytes, usedBytes int64
	for _, n := range nodes {
		if n.DiskTotalMB == nil || n.DiskUsedMB == nil {
			continue
		}
		totalBytes += *n.DiskTotalMB * bytesPerMB
		usedBytes += *n.DiskUsedMB * bytesPerMB
	}
	if totalBytes <= 0 {
		return nil, nil
	}
	totalGB := int(totalBytes / giToBytes)
	pct := int(usedBytes * percentMultiplier / totalBytes)
	return &totalGB, &pct
}

// parseK8sCPU parses Kubernetes CPU resource strings (e.g. "4", "4000m", "5866711668n").
// Returns millicores.
func parseK8sCPU(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "n") {
		v, _ := strconv.ParseInt(strings.TrimSuffix(s, "n"), 10, 64)
		const nanocoresPerMillicore = 1_000_000
		return v / nanocoresPerMillicore
	}
	if strings.HasSuffix(s, "m") {
		v := parseInt(strings.TrimSuffix(s, "m"))
		return int64(v)
	}
	v := parseInt(s)
	return int64(v) * millicoresPerCore
}

// parseK8sMemory parses Kubernetes memory resource strings (e.g. "16384Ki", "8Gi").
func parseK8sMemory(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "Ki") {
		v := parseInt(strings.TrimSuffix(s, "Ki"))
		return int64(v) * kiToBytes
	}
	if strings.HasSuffix(s, "Mi") {
		v := parseInt(strings.TrimSuffix(s, "Mi"))
		return int64(v) * miToBytes
	}
	if strings.HasSuffix(s, "Gi") {
		v := parseInt(strings.TrimSuffix(s, "Gi"))
		return int64(v) * giToBytes
	}
	return int64(parseInt(s))
}

// parseTopCPU parses kubectl top CPU values (e.g. "1200m", "2").
func parseTopCPU(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "m") {
		v := parseInt(strings.TrimSuffix(s, "m"))
		return int64(v)
	}
	v := parseInt(s)
	return int64(v) * millicoresPerCore
}

// parseTopMemory parses kubectl top memory values (e.g. "4096Mi", "8Gi").
func parseTopMemory(s string) int64 {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "Ki") {
		v := parseInt(strings.TrimSuffix(s, "Ki"))
		return int64(v) * kiToBytes
	}
	if strings.HasSuffix(s, "Mi") {
		v := parseInt(strings.TrimSuffix(s, "Mi"))
		return int64(v) * miToBytes
	}
	if strings.HasSuffix(s, "Gi") {
		v := parseInt(strings.TrimSuffix(s, "Gi"))
		return int64(v) * giToBytes
	}
	return int64(parseInt(s))
}

// parseInt parses an integer from a string, returning 0 on failure.
func parseInt(s string) int {
	var v int
	fmt.Sscanf(s, "%d", &v)
	return v
}

type MyHiveEntry struct {
	RegistryEntry
	Role        string `json:"role"`
	ProvError   string `json:"provError,omitempty"`
	ProvStatus  string `json:"provStatus,omitempty"`
	AutoUpgrade bool   `json:"autoUpgrade"`
	// AutoUpgradeMode is always sent NORMALIZED (never empty when autoUpgrade is
	// on) so the dashboard can render the effective mode without re-deriving the
	// legacy empty-means-instant rule in JavaScript.
	AutoUpgradeMode     string                 `json:"autoUpgradeMode,omitempty"`
	PendingRequestCount int                    `json:"pendingRequestCount,omitempty"`
	PendingRequests     []PendingAccessRequest `json:"pending_requests,omitempty"`

	// Access lists who can sign in to this hive and with what role, so My Hives
	// can show it on hover without a per-row API call. Same data as
	// GET /hives/{id}/access, and populated under the same authorization rule:
	// only for rows the caller owns (or admin). Nil on rows the caller merely
	// has delegated access to — knowing who else shares a hive is the owner's
	// information, not every reader's.
	Access []HiveAccessEntry `json:"access,omitempty"`

	// Assigning is true while a freshly-assigned placeholder's spoke has not yet
	// reported the real project via heartbeat: the meta.json already records the
	// real project (org/repos/ACMM set, status no longer "available") but the live
	// registry entry still shows the placeholder identity. It flips false — and the
	// dashboard spinner clears — once the spoke reconciles and the registry reports
	// the real project. AssigningTo is the target org so the row can say "Assigning
	// to <org>". This is exactly the condition projectConfigForHiveID keeps sending
	// its reconcile under.
	Assigning   bool   `json:"assigning,omitempty"`
	AssigningTo string `json:"assigningTo,omitempty"`

	// Migration tracking (Phase 7).
	MigrationStatus string `json:"migrationStatus,omitempty"`
	MigrationFrom   string `json:"migrationFrom,omitempty"`
	MigrationTo     string `json:"migrationTo,omitempty"`

	// Drift holds config-drift signals computed server-side against the fleet
	// norm (see drift.go). It rides this payload rather than a per-row API call
	// so the My Hives table can render the badge and the fleet-exceptions
	// summary from data it already has.
	Drift DriftReport `json:"drift"`

	// RecentEvents carries the newest few timeline events so the My Hives
	// status hover can show recent activity WITHOUT a per-row fetch.
	//
	// Embedding rather than lazy-fetching is deliberate. The hover is a
	// transient, high-frequency interaction: sweeping the pointer down a
	// 42-row table would fire 42 requests, each of which hits the filesystem
	// via loadSaaSHive in handleHiveTimeline. No debounce or TTL cache removes
	// that — scanning the list IS the normal way the table is read, so the
	// requests are the common case, not the edge case.
	//
	// The cost of embedding was measured rather than guessed: at
	// myHivesRecentEventCount events per row a typical fleet of 42 hives adds
	// ~14 KB, and ~50 KB in the pathological case where every event carries a
	// maxed-out timelineMaxDetailRunes detail. That is small beside what a row
	// already ships (health map, drift report, access list, agents,
	// leaderboard and the issue/PR spark histories), and the events are
	// already in memory hub-side, so serving them costs no extra I/O.
	//
	// Populated under the SAME authorization as Access — owner or admin only —
	// because it is the same per-hive operational detail handleHiveTimeline
	// guards. The full 200-event history stays behind that endpoint and its
	// modal; this is only the hover preview.
	RecentEvents []TimelineEvent `json:"recentEvents,omitempty"`

	// AdvisoryStale is true when this hive SHOULD be posting advisory digests
	// but its digest has quietly gone stale — computed on read by advisoryStale()
	// so the browser never re-derives the threshold or the gating (advisory-mode
	// + app-can-write) and cannot drift from the Go rule. AdvisoryStaleReason is
	// the tooltip cause. Both stay zero/empty for hives that are not in advisory
	// mode, whose App cannot write, or that report an unknown timestamp — those
	// must never show the pill.
	AdvisoryStale       bool   `json:"advisoryStale,omitempty"`
	AdvisoryStaleReason string `json:"advisoryStaleReason,omitempty"`

	// InactiveAgents is how many of this hive's agents are RUNNING but not
	// doing any work — session gone, sitting on a login prompt, or producing
	// nothing while work is queued. Computed on read by
	// evaluateInactiveAgents() so the browser never re-derives the thresholds
	// or the paused/on-demand gating and cannot drift from the Go rule.
	//
	// Agents the operator deliberately PAUSED are excluded by that rule and
	// never counted here: a pause is a choice, not a fault, and a facet that
	// alarms on it would be wrong on every hive with a parked agent.
	// InactiveAgentsReason is the tooltip cause. Both stay zero/empty for
	// hives with nothing wrong, so the pill and the facet self-suppress.
	InactiveAgents       int    `json:"inactiveAgents,omitempty"`
	InactiveAgentsReason string `json:"inactiveAgentsReason,omitempty"`

	// URLUnreachable is true when this hive's PUBLIC dashboard URL failed to
	// serve on the last several probes — the link in this very table is dead.
	// Computed on read from the auth-audit loop's observations, so the browser
	// never re-derives the failure threshold. Stays zero/empty for a hive that
	// is serving, that is too new to have converged, or whose whole cluster is
	// out (an outage is one condition, not N broken hives) — those must never
	// show the pill.
	URLUnreachable       bool   `json:"urlUnreachable,omitempty"`
	URLUnreachableReason string `json:"urlUnreachableReason,omitempty"`
}

// myHivesRecentEventCount is how many timeline events ride the My Hives
// payload for the status hover. The hover panel already carries a status word,
// the per-check health lines, the relay line and the user list; three events
// is enough to answer "what just happened to this hive?" without turning a
// transient tooltip into a scrolling log. "See all" opens the full modal.
const myHivesRecentEventCount = 3

func (s *HubServer) handleMyHives(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	user := ensureSaaSUser(username)

	s.mu.Lock()
	offlineEvents := s.markStaleHives()
	allHives := make([]RegistryEntry, len(s.registry.Hives))
	copy(allHives, s.registry.Hives)
	s.mu.Unlock()
	s.flushOfflineEvents(offlineEvents)

	var result []MyHiveEntry

	autoUpgradeMap := make(map[string]bool)
	// Normalized here (empty → instant) so every consumer sees the effective
	// mode rather than the raw legacy blank.
	autoUpgradeModeMap := make(map[string]string)
	saasByID := make(map[string]*SaaSHive)
	for _, sh := range listSaaSHives() {
		shCopy := sh
		autoUpgradeMap[sh.ID] = sh.AutoUpgrade
		autoUpgradeModeMap[sh.ID] = normalizeAutoUpgradeMode(sh.AutoUpgradeMode)
		saasByID[sh.ID] = &shCopy
	}

	// enrichFromSaaSMeta overlays SaaS meta.json fields onto an entry built from
	// a live registry hive (registry entries come from spoke heartbeats and carry
	// NO provStatus, so provStatus/migration/error must come from meta).
	//
	// ACMM level is the subtle case, and getting it wrong caused a regression:
	//   - For a CLAIMED / running hive, the spoke's LIVE heartbeat level is
	//     authoritative — it's what the hive is actually running. Many pre-claim
	//     meta.json records still carry a stale acmm_level: 0 even though the
	//     spoke runs at a real level, so unconditionally taking the meta level
	//     downgraded live hives to L0 on the dashboard.
	//   - For an unclaimed PLACEHOLDER (status: available), there is no
	//     meaningful live level (the slot reports L0), so the INTENDED level from
	//     meta ("L2 Instructed") is what should show, and it also gates the
	//     "Assign" menu (provStatus === 'available').
	// Rule: take the meta level only for placeholders, or as a fallback when the
	// live registry level is 0 (unknown) but meta has a real one. Otherwise the
	// live registry level wins.

	enrichFromSaaSMeta := func(entry *MyHiveEntry) {
		sh := saasByID[entry.ID]
		if sh == nil {
			return
		}
		entry.ProvStatus = sh.Status
		if sh.Status == statusAvailable || (entry.ACMMLevel == 0 && sh.ACMMLevel > 0) {
			entry.ACMMLevel = sh.ACMMLevel
		}
		// Show the friendly vanity host for a claimed hive the instant it is
		// claimed, rather than waiting for the spoke to adopt+report it back over
		// the heartbeat. Until then entry.DashboardURL (spoke-reported) is still the
		// raw placeholder host, which is the placeholder-URL-persists bug in My
		// Hives / the row's Open link. Only overlays a validated meta vanity_url;
		// an unclaimed placeholder or a hive with no vanity keeps its own URL.
		if v := claimedVanityURL(sh); v != "" {
			entry.DashboardURL = v
		}
		switch sh.Status {
		case "provisioning":
			entry.GovernorMode = "PROVISIONING"
		case "error":
			entry.GovernorMode = "ERROR"
			entry.ProvError = sh.Error
		}
		entry.MigrationStatus = sh.MigrationStatus
		entry.MigrationFrom = sh.MigrationFrom
		entry.MigrationTo = sh.MigrationTo

		// Assigning transient state: after a placeholder is assigned, the meta
		// records the real project but the spoke still reports the old placeholder
		// identity until it reconciles via heartbeat.
		//
		// projectConfigForHiveID returns non-nil whenever meta != what the spoke
		// reports — but that ALSO happens transiently during an UPGRADE (the spoke
		// pod restarts and its heartbeat momentarily reports an empty/stale org),
		// which falsely lit "Assigning to <org>" on already-claimed hives that were
		// merely upgrading. Guard against that:
		//   - never show Assigning while the hive is Upgrading (an upgrade is not
		//     an assignment), and
		//   - only show it when the spoke is genuinely reporting a DIFFERENT,
		//     non-empty project (an empty reported org means the spoke just
		//     restarted / hasn't beaten yet — not a fresh assignment).
		spokeReportsDifferentProject := entry.Org != "" && !strings.EqualFold(entry.Org, sh.Org)
		if !entry.Upgrading && spokeReportsDifferentProject &&
			// Empty curAPIURL deliberately: a GHE API-URL-only push is a config
			// repair, not an assignment, and must never light "Assigning to <org>".
			projectConfigForHiveID(entry.ID, entry.Org, entry.Repos, entry.PrimaryRepo, entry.ACMMLevel, entry.DashboardURL, "") != nil {
			entry.Assigning = true
			entry.AssigningTo = sh.Org
		}
	}

	// resolveGitHubHost fills in the Location-column GitHub host pill when the
	// spoke did not report one over the heartbeat.
	//
	// The spoke-reported value always wins: it is the hive's real runtime
	// GitHub, and it is the only source that is correct when a hive's GitHub
	// differs from its cluster's default (the enricom8 hive is the live
	// example — its host had to be corrected by hand). Heartbeat-only and
	// firewalled spokes upgrade slowly, so for those this falls back to the
	// hive's recorded host, then its cluster's default. Anything still empty
	// is left empty and rendered as "github.com" by the pill, so an old spoke
	// shows a sane default rather than a wrong host.
	resolveGitHubHost := func(entry *MyHiveEntry) {
		if entry.GitHubHost != "" {
			return // spoke-reported — authoritative
		}
		if sh := saasByID[entry.ID]; sh != nil {
			if sh.GitHubHost != "" {
				entry.GitHubHost = githubHostLabel(sh.GitHubHost)
				return
			}
			if c := s.clusterForHive(sh); c != nil && (c.GitHubBaseURL != "" || c.GitHubAPIURL != "") {
				// base-or-api so a GHE cluster recorded with only an api_url
				// (blank base_url — the common state) is recognised as GHE, not
				// mislabelled github.com.
				entry.GitHubHost = clusterGitHubConfig(c).HostLabel()
				return
			}
		}
		// No meta record: fall back to the cluster the registry entry reports.
		// s.clusters is read unlocked here to match every other reader in this
		// package (clusterForHive, clusterNameForID); it is effectively
		// immutable after load, and taking s.mu here would nest inside callers
		// that already hold it.
		if entry.ClusterID != "" {
			if c, ok := s.clusters[entry.ClusterID]; ok && (c.GitHubBaseURL != "" || c.GitHubAPIURL != "") {
				entry.GitHubHost = clusterGitHubConfig(&c).HostLabel()
			}
		}
	}

	isAdmin := username == hubAdminUsername
	for _, h := range allHives {
		if role, ok := user.Hives[h.ID]; ok {
			if isAdmin && role != "owner" {
				role = "owner"
			}
			entry := MyHiveEntry{RegistryEntry: h, Role: role, AutoUpgrade: autoUpgradeMap[h.ID], AutoUpgradeMode: autoUpgradeModeMap[h.ID]}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
			continue
		}
		if strings.EqualFold(h.Owner, username) {
			entry := MyHiveEntry{RegistryEntry: h, Role: "owner", AutoUpgrade: autoUpgradeMap[h.ID], AutoUpgradeMode: autoUpgradeModeMap[h.ID]}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
			user.Hives[h.ID] = "owner"
			continue
		}
		if isAdmin {
			entry := MyHiveEntry{RegistryEntry: h, Role: "owner", AutoUpgrade: autoUpgradeMap[h.ID], AutoUpgradeMode: autoUpgradeModeMap[h.ID]}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
		}
	}

	seen := make(map[string]bool)
	for _, h := range result {
		seen[h.ID] = true
	}
	for hiveID, role := range user.Hives {
		if seen[hiveID] {
			continue
		}
		if strings.HasPrefix(hiveID, "hosted-") || strings.HasPrefix(hiveID, "saas-") {
			sh := loadSaaSHive(hiveID)
			if sh != nil {
				entry := MyHiveEntry{
					RegistryEntry: RegistryEntry{
						ID:          sh.ID,
						Name:        sh.Org + "/" + sh.PrimaryRepo,
						Org:         sh.Org,
						Repos:       sh.Repos,
						PrimaryRepo: sh.PrimaryRepo,
						ACMMLevel:   sh.ACMMLevel,
						HiveType:    "hosted",
						ClusterID:   clusterIDForSaaSHive(*sh),
						ClusterName: s.clusterNameForID(clusterIDForSaaSHive(*sh)),
					},
					Role: role,
				}
				entry.ProvStatus = sh.Status
				if sh.Status == "provisioning" {
					entry.GovernorMode = "PROVISIONING"
				} else if sh.Status == "error" {
					entry.GovernorMode = "ERROR"
					entry.ProvError = sh.Error
				}
				entry.MigrationStatus = sh.MigrationStatus
				entry.MigrationFrom = sh.MigrationFrom
				entry.MigrationTo = sh.MigrationTo
				result = append(result, entry)
				seen[sh.ID] = true
			}
		}
	}

	for _, sh := range listSaaSHives() {
		if (sh.Owner == username || isAdmin) && !seen[sh.ID] {
			user.Hives[sh.ID] = "owner"
			entry := MyHiveEntry{
				RegistryEntry: RegistryEntry{
					ID:          sh.ID,
					Name:        sh.Org + "/" + sh.PrimaryRepo,
					Org:         sh.Org,
					Repos:       sh.Repos,
					PrimaryRepo: sh.PrimaryRepo,
					ACMMLevel:   sh.ACMMLevel,
					HiveType:    "hosted",
					ClusterID:   clusterIDForSaaSHive(sh),
					ClusterName: s.clusterNameForID(clusterIDForSaaSHive(sh)),
				},
				Role: "owner",
			}
			entry.ProvStatus = sh.Status
			if sh.Status == "provisioning" {
				entry.GovernorMode = "PROVISIONING"
			} else if sh.Status == "error" {
				entry.GovernorMode = "ERROR"
				entry.ProvError = sh.Error
			}
			entry.MigrationStatus = sh.MigrationStatus
			entry.MigrationFrom = sh.MigrationFrom
			entry.MigrationTo = sh.MigrationTo
			result = append(result, entry)
			seen[sh.ID] = true
		}
	}

	if len(user.Hives) > 0 {
		saveSaaSUser(user)
	}

	// Read the user roster ONCE for the access hover rather than per row —
	// listAllSaaSUsers hits the filesystem for every user record, and My Hives
	// can carry dozens of rows.
	var allSaaSUsers []SaaSUser
	for _, h := range result {
		if h.Role == "owner" || isAdmin {
			allSaaSUsers = listAllSaaSUsers()
			break
		}
	}

	saasCount := 0
	for i, h := range result {
		if strings.HasPrefix(h.ID, "hosted-") || strings.HasPrefix(h.ID, "saas-") {
			saasCount++
		}
		// Backfill the Location-column GitHub host for rows whose spoke is too
		// old to report one. Done here, over the assembled set, so no entry
		// construction site can be missed.
		resolveGitHubHost(&result[i])
		// Who-has-access is shown only to owners (and admin), matching
		// handleAccessList's rule. A read/read-write member is deliberately not
		// told who else shares the hive.
		if h.Role == "owner" || isAdmin {
			result[i].Access = accessForHive(h.ID, allSaaSUsers)
			// Recent activity for the status hover, same owner/admin rule as
			// the access list and as handleHiveTimeline itself. s.timeline is
			// nil in tests that construct a bare HubServer, and recent() is a
			// read under the store's own leaf mutex — no s.mu is held here.
			if s.timeline != nil {
				result[i].RecentEvents = s.timeline.recent(h.ID, myHivesRecentEventCount)
			}
		}
		if h.Role == "owner" || h.Role == "read-write" || isAdmin {
			reqs := loadAccessRequests(h.ID)
			var pending []PendingAccessRequest
			for _, req := range reqs {
				if req.Status == "pending" {
					pending = append(pending, PendingAccessRequest{
						Username:    req.Username,
						RequestedAt: req.RequestedAt,
						Note:        req.Note,
					})
				}
			}
			result[i].PendingRequestCount = len(pending)
			result[i].PendingRequests = pending
		}
	}

	// Config drift, computed once over the caller's full visible set — the
	// fleet norm is derived from that set, so this must run AFTER every row has
	// been collected and enriched (a row still missing its provStatus would be
	// misread as a claimed hive and flagged for having no App).
	annotateDrift(result, getDisplaySHAs(), time.Now())

	// Dead-link pill, computed once over the full visible set for the same
	// reason as drift: the cluster-outage suppression is a property of the SET
	// (most of a cluster failing is an outage, not N broken hives), so it
	// cannot be decided one row at a time. Derived from the same alert list the
	// panel renders, so pill and panel always agree.
	{
		regs := make([]RegistryEntry, 0, len(result))
		for i := range result {
			regs = append(regs, result[i].RegistryEntry)
		}
		urlAlerts := s.urlUnreachableAlerts(regs, time.Now())
		for i := range result {
			if bad, reason := urlUnreachableFacet(urlAlerts, result[i].ID); bad {
				result[i].URLUnreachable = true
				result[i].URLUnreachableReason = reason
			}
		}
	}

	// Attach the user-journey stage to every row so the table can show who is
	// stalled where. Derived on read; never persisted on the registry entry.
	journeyNow := time.Now()
	for i := range result {
		st := s.journey.get(result[i].ID)
		status := JourneyStatusFor(&result[i].RegistryEntry, st, journeyNow)
		result[i].Journey = &status

		// Advisory-staleness pill, computed on read (same as Journey) so the
		// gating — advisory-mode participation, app-can-write, past-threshold —
		// lives ONLY in Go and the browser just renders the flag.
		if stale, reason := advisoryStale(result[i].RegistryEntry, journeyNow); stale {
			result[i].AdvisoryStale = true
			result[i].AdvisoryStaleReason = reason
		}

		// Running-but-inactive agents, computed on read for the same reason:
		// the thresholds and the paused/on-demand exclusions live ONLY in Go.
		//
		// The queue gate is the governor's own actionable backlog, which is
		// what makes the idle rule safe on a genuinely quiet hive: with no
		// issues and no PRs waiting, idle agents are CORRECT and nothing is
		// reported. The two unambiguous faults (dead session, login prompt)
		// are independent of it.
		queuedWork := result[i].ActionableIssues + result[i].ActionablePRs
		if rep := evaluateInactiveAgents(result[i].Agents, queuedWork, journeyNow); rep.Count > 0 {
			result[i].InactiveAgents = rep.Count
			result[i].InactiveAgentsReason = rep.Reason
		}

		// Sparkline history dominated this payload: at 42 hives the two series
		// were ~755 KB of an 818 KB response (92%), yet they are drawn into a
		// 50 px-wide SVG. Downsample on the WIRE only — the registry keeps the
		// full 7-day series, so nothing is lost server-side and a future
		// full-resolution view can still fetch it per hive.
		result[i].IssueHistory = downsampleSpark(result[i].IssueHistory, sparkWirePoints)
		result[i].PRHistory = downsampleSpark(result[i].PRHistory, sparkWirePoints)
	}

	resp := map[string]any{
		"hives":                   result,
		"saas_quota":              user.SaaSQuota,
		"saas_used":               saasCount,
		"is_admin":                isAdmin,
		"latest_sha":              getLatestSHA(),
		"latest_shas":             getDisplaySHAs(),
		"latest_sha_messages":     getDisplaySHAMessages(),
		"latest_sha_image_status": getImageStatuses(),
		"commit_messages":         getCommitMessages(),
		"hub_git_hash":            s.hubGitHash,
		"hub_git_branch":          s.hubGitBranch,
		"tracked_branches":        s.trackedBranchList(),
		"hub_auto_upgrade":        isHubAutoUpgrade(),
		"hub_upgrade_state":       s.hubUpgradeState(),
		"show_my_hives":           true,
		// Fleet alerts ship WITH the hive list so the "Attention needed" panel
		// renders in the same paint as the rows it summarises — a second
		// round-trip would make the panel pop in after the list and shift it.
		// Scoped to the hives this caller can already see, so it never leaks
		// the existence of a hive they have no access to.
		"alerts": s.fleetAlerts(result),
	}

	myReq := loadProvisionRequest(username)
	if myReq != nil {
		resp["my_provision_request"] = myReq
	}

	if isAdmin {
		// Admin-only: enrichProvisionRequests attaches other users' hive
		// memberships and roles, so it must stay inside this branch. A non-admin
		// caller never receives provision_requests at all.
		resp["provision_requests"] = enrichProvisionRequests(listProvisionRequests())
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *HubServer) handleAccessStatus(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"authenticated": false,
			"show_my_hives": false,
		})
		return
	}

	user := loadSaaSUser(username)
	if user == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"authenticated": true,
			"show_my_hives": true,
			"hives":         map[string]string{},
		})
		return
	}

	s.mu.Lock()
	offlineEvents := s.markStaleHives()
	allHives := make([]RegistryEntry, len(s.registry.Hives))
	copy(allHives, s.registry.Hives)
	s.mu.Unlock()
	s.flushOfflineEvents(offlineEvents)

	type hiveAccessInfo struct {
		Role   string `json:"role"`
		Status string `json:"status"`
	}
	hiveAccess := make(map[string]hiveAccessInfo)

	isAdmin := username == hubAdminUsername
	for _, h := range allHives {
		if role, ok := user.Hives[h.ID]; ok {
			hiveAccess[h.ID] = hiveAccessInfo{Role: role, Status: "accepted"}
			continue
		}
		if strings.EqualFold(h.Owner, username) {
			hiveAccess[h.ID] = hiveAccessInfo{Role: "owner", Status: "accepted"}
			continue
		}
		if isAdmin {
			hiveAccess[h.ID] = hiveAccessInfo{Role: "owner", Status: "accepted"}
			continue
		}
		reqs := loadAccessRequests(h.ID)
		for _, req := range reqs {
			if req.Username == username && req.Status == "pending" {
				hiveAccess[h.ID] = hiveAccessInfo{Status: "pending"}
				break
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"authenticated":           true,
		"show_my_hives":           true,
		"hives":                   hiveAccess,
		"latest_sha":              getLatestSHA(),
		"latest_shas":             getDisplaySHAs(),
		"latest_sha_messages":     getDisplaySHAMessages(),
		"latest_sha_image_status": getImageStatuses(),
		"commit_messages":         getCommitMessages(),
	})
}

func (s *HubServer) handleCreateHive(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	user := loadSaaSUser(username)
	if user == nil || user.Blocked {
		http.Error(w, `{"error":"account blocked or not found"}`, http.StatusForbidden)
		return
	}

	if user.SaaSQuota == 0 {
		http.Error(w, `{"error":"no hosted hive quota — contact the hub admin to request access"}`, http.StatusForbidden)
		return
	}

	const maxCreateHiveBodyBytes = 64 * 1024 // 64 KiB — includes app private key
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateHiveBodyBytes)
	var req CreateHiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if req.Org == "" || req.Repos == "" {
		http.Error(w, `{"error":"org and repos are required"}`, http.StatusBadRequest)
		return
	}
	if !isValidName(req.Org) {
		http.Error(w, `{"error":"invalid org name — alphanumeric, dashes, dots, underscores only"}`, http.StatusBadRequest)
		return
	}
	for _, r := range strings.Split(req.Repos, ",") {
		if !isValidRepoRef(strings.TrimSpace(r)) {
			http.Error(w, `{"error":"invalid repo name"}`, http.StatusBadRequest)
			return
		}
	}
	hasToken := req.GitHubToken != ""
	hasApp := req.AuthMethod == "app" && req.AppID != "" && req.InstallationID != "" && req.AppPrivateKey != ""
	hasAppLater := req.AuthMethod == "app" && req.AppID != "" && req.InstallationID == "" && req.AppPrivateKey == ""
	if !hasToken && !hasApp && !hasAppLater {
		http.Error(w, `{"error":"provide either a GitHub token or GitHub App credentials"}`, http.StatusBadRequest)
		return
	}
	if hasToken && !strings.HasPrefix(req.GitHubToken, "ghp_") && !strings.HasPrefix(req.GitHubToken, "github_pat_") {
		http.Error(w, `{"error":"token must start with ghp_ or github_pat_"}`, http.StatusBadRequest)
		return
	}
	if hasApp && !strings.HasPrefix(strings.TrimSpace(req.AppPrivateKey), "-----BEGIN") {
		http.Error(w, `{"error":"private key must be PEM format"}`, http.StatusBadRequest)
		return
	}

	if user.SaaSQuota > 0 && countUserHives(username) >= user.SaaSQuota {
		http.Error(w, fmt.Sprintf(`{"error":"quota reached — max %d SaaS hives"}`, user.SaaSQuota), http.StatusBadRequest)
		return
	}

	if maxSaaSHivesTotal > 0 && len(listSaaSHives()) >= maxSaaSHivesTotal {
		http.Error(w, `{"error":"hosted capacity reached — try again later"}`, http.StatusServiceUnavailable)
		return
	}

	repos := strings.Split(req.Repos, ",")
	for i := range repos {
		repos[i] = strings.TrimSpace(repos[i])
	}
	primaryRepo := req.PrimaryRepo
	if primaryRepo == "" && len(repos) > 0 {
		primaryRepo = repos[0]
	}
	acmm := req.ACMMLevel
	if acmm < 1 || acmm > 6 {
		acmm = 1
	}

	// Validate and default the target cluster.
	targetCluster := req.ClusterID
	if targetCluster == "" {
		targetCluster = defaultClusterID
	}
	if _, ok := s.clusters[targetCluster]; !ok {
		http.Error(w, `{"error":"unknown cluster_id"}`, http.StatusBadRequest)
		return
	}

	hiveID := generateHiveID(req.Org, primaryRepo)

	// Determine which cluster to provision on. Default to hive-oke if unspecified.
	clusterID := req.ClusterID
	if clusterID == "" {
		clusterID = defaultClusterID
	}
	// Look up the cluster to get its domain for the subdomain.
	cluster, clusterFound := s.clusters[clusterID]
	if !clusterFound {
		http.Error(w, `{"error":"unknown cluster_id"}`, http.StatusBadRequest)
		return
	}
	subdomain := hiveID + "." + cluster.Domain

	h := &SaaSHive{
		ID:          hiveID,
		Owner:       username,
		ProjectName: req.ProjectName,
		Org:         req.Org,
		Repos:       repos,
		PrimaryRepo: primaryRepo,
		ACMMLevel:   acmm,
		ClusterID:   targetCluster,
		Status:      "provisioning",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Subdomain:   subdomain,
		// Default to public when the request omits is_public — matches
		// the pre-#1604 template that hardcoded is_public: true. Owners
		// can toggle visibility later from My Hives.
		IsPublic: req.IsPublic == nil || *req.IsPublic,
		// Per-hive GitHub host override (empty = cluster default). Lets a
		// public-GitHub org provision correctly on a GHE-defaulted cluster.
		GitHubBaseURL: req.GitHubBaseURL,
		GitHubAPIURL:  req.GitHubAPIURL,
	}

	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save hive metadata"}`, http.StatusInternalServerError)
		return
	}

	user.Hives[hiveID] = "owner"
	saveSaaSUser(user)

	provisionWG.Add(1)
	go func() {
		defer provisionWG.Done()
		cluster := s.clusterForHive(h)
		if cluster == nil {
			h.Status = "error"
			h.Error = "no cluster config available"
			saveSaaSHive(h)
			s.logger.Error("no cluster config for provisioning", "hive_id", hiveID, "cluster_id", h.ClusterID)
			return
		}
		if err := provisionHive(h, &req, cluster, s.appKeysByAppID(), s.logger); err != nil {
			h.Status = "error"
			h.Error = err.Error()
			saveSaaSHive(h)
			s.logger.Warn("hosted hive provision failed", "hive_id", hiveID, "error", err)
			return
		}
		h.Status = "provisioning"
		saveSaaSHive(h)
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":        hiveID,
		"status":    "provisioning",
		"subdomain": h.Subdomain,
	})
}

func (s *HubServer) handleHiveStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	user := loadSaaSUser(username)
	if user == nil {
		http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
		return
	}
	if h.Owner != username && username != hubAdminUsername {
		if _, hasAccess := user.Hives[id]; !hasAccess {
			http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(h)
}

// handleOpenHive is the SSO handoff entry point: a hub-authenticated user hits
// this to open a spoke dashboard without a second GitHub login. It confirms the
// user may access the hive, mints a short-lived HMAC token bound to {user, role,
// hiveID} with the shared hub secret, and 302-redirects to the spoke's
// <dashboardURL>/sso?token=… . The spoke verifies the token and its own
// authorized_users allowlist before minting a session (see dashboard.handleSSO).
//
// If SSO can't be used (no hub secret, or the spoke reported no dashboard URL),
// it falls back to redirecting straight to the dashboard URL (or the hive-oke
// host), preserving today's behavior — the spoke will then prompt for login.
func (s *HubServer) handleOpenHive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.Contains(id, "..") || strings.Contains(id, "/") {
		http.Error(w, `{"error":"invalid hive id"}`, http.StatusBadRequest)
		return
	}
	// /open is the link people actually paste into Slack, so serve the Hive
	// preview card to crawlers here rather than letting them follow the 303 to
	// /login. Short-circuiting is the robust option: an unfurler that caps or
	// skips redirects would otherwise never reach the card, and one that follows
	// the chain unauthenticated lands on GitHub's OAuth page and scrapes GitHub's
	// Open Graph tags — which is exactly the bug.
	if isLinkPreviewCrawler(r) {
		writeLinkPreview(w)
		return
	}
	username := s.getAuthUser(r)
	if username == "" {
		// Not logged in — this is a browser navigation, so send the user through
		// the hub login and return them to THIS /open URL afterward, so the SSO
		// handoff completes and they land logged-in on the spoke (instead of the
		// raw {"error":"not authenticated"} JSON dead end).
		self := "/api/saas/hives/" + url.PathEscape(id) + "/open"
		http.Redirect(w, r, "/login?redirect="+url.QueryEscape(self), http.StatusSeeOther)
		return
	}

	// Resolve the spoke's base URL: prefer the heartbeat-reported dashboard URL
	// (correct for firewalled spokes), fall back to the hive-oke host pattern.
	base := ""
	s.mu.RLock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			base = s.registry.Hives[i].DashboardURL
			break
		}
	}
	s.mu.RUnlock()
	// For a claimed hive, hand the SSO handoff off to its vanity host rather than
	// the raw placeholder host the spoke may still be reporting — the vanity URL
	// is the validated, user-facing host (and the one the spoke will settle on).
	// Only a validated meta vanity_url is used; unclaimed placeholders are left on
	// their working placeholder host.
	if v := claimedVanityURL(loadSaaSHive(id)); v != "" {
		base = v
	}
	if base == "" && (strings.HasPrefix(id, "hosted-") || strings.HasPrefix(id, "saas-")) {
		base = "https://" + id + ".hive.kubestellar.io"
	}
	if base == "" {
		http.Error(w, `{"error":"hive has no reachable dashboard URL yet"}`, http.StatusConflict)
		return
	}
	base = strings.TrimRight(base, "/")

	// Access gate: only the owner, an authorized user, or the hub admin may open
	// the spoke. The role we pass is advisory — the spoke re-checks its own
	// allowlist and uses that role authoritatively.
	role := saasRoleRead
	if username == hubAdminUsername {
		role = saasRoleOwner
	} else {
		user := loadSaaSUser(username)
		if user == nil {
			http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
			return
		}
		if h := loadSaaSHive(id); h != nil && h.Owner == username {
			role = saasRoleOwner
		} else if r, ok := user.Hives[id]; ok {
			role = r
		} else {
			http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
			return
		}
	}

	// Mint the handoff token. Without a hub secret we can't sign one, so fall
	// back to a plain dashboard redirect (spoke will prompt for login).
	if s.hubSecret != "" {
		if tok := MintSSOToken(s.hubSecret, username, role, id, time.Now()); tok != "" {
			http.Redirect(w, r, base+"/sso?token="+url.QueryEscape(tok), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, base+"/", http.StatusSeeOther)
}

func (s *HubServer) handleDeleteHive(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	id := r.PathValue("id")
	if strings.Contains(id, "..") || strings.Contains(id, "/") {
		http.Error(w, `{"error":"invalid hive id"}`, http.StatusBadRequest)
		return
	}

	h := loadSaaSHive(id)
	if h == nil {
		// SaaS entry already gone — still clean up the in-memory registry
		// so the hive disappears from the listing immediately.
		s.removeRegistryEntry(id, username)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": deleteStatusDeleted})
		return
	}
	if h.Owner != username && username != hubAdminUsername {
		http.Error(w, `{"error":"only the owner can delete this hive"}`, http.StatusForbidden)
		return
	}

	// Full de-provisioning: namespace, PV, OCI export, OCI file system, disk record, user cleanup.
	//
	// The registry entry is removed unconditionally, even when the cluster
	// teardown cannot run or only partially succeeds. A hive whose namespace is
	// already gone (or whose cluster config has been removed) would otherwise be
	// permanently un-deletable: the handler used to return 500 before reaching
	// removeRegistryEntry, stranding a ghost row in "My Hives" that no amount of
	// re-clicking Delete could clear. Leaving a stranded row is strictly worse
	// than leaving cloud resources behind, because the row is what the user sees
	// and it is the only thing they can act on. Partial failures are reported in
	// the response so the user knows manual cleanup may still be needed.
	cluster := s.clusterForHive(h)
	if cluster != nil {
		deprovisionHive(h, cluster, s.logger)
	} else {
		s.logger.Error("no cluster config for deprovision; removing registry entry anyway",
			"hive_id", id, "cluster_id", h.ClusterID)
	}
	s.removeRegistryEntry(id, username)

	s.logger.Info("audit: hosted hive deleted", "hive_id", id, "by", username,
		"deprovisioned", cluster != nil)
	w.Header().Set("Content-Type", "application/json")
	if cluster == nil {
		// deleteStatusPartial tells the UI the registry row is gone but cloud
		// resources may survive and need manual cleanup.
		json.NewEncoder(w).Encode(map[string]string{
			"status":  deleteStatusPartial,
			"warning": "removed from the hub registry, but no cluster config was available to delete the namespace, PV, or OCI storage — these may need manual cleanup",
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": deleteStatusDeleted})
}

// Delete outcome statuses returned by handleDeleteHive.
const (
	// deleteStatusDeleted means the registry entry was removed and the cluster
	// teardown ran.
	deleteStatusDeleted = "deleted"
	// deleteStatusPartial means the registry entry was removed but the cluster
	// teardown could not run; cloud resources may need manual cleanup.
	deleteStatusPartial = "partially_deleted"
)

// MigrateRequest is the JSON body for POST /api/saas/hives/{id}/migrate.
type MigrateRequest struct {
	TargetClusterID string `json:"target_cluster_id"`
}

// migrateMaxBodyBytes limits the migrate request body size.
const migrateMaxBodyBytes = 1024

func (s *HubServer) handleMigrateHive(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	id := r.PathValue("id")
	if strings.Contains(id, "..") || strings.Contains(id, "/") {
		http.Error(w, `{"error":"invalid hive id"}`, http.StatusBadRequest)
		return
	}

	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if h.Owner != username && username != hubAdminUsername {
		http.Error(w, `{"error":"only the owner can migrate this hive"}`, http.StatusForbidden)
		return
	}

	// Reject if already migrating.
	if h.MigrationStatus == "migrating" {
		http.Error(w, `{"error":"migration already in progress"}`, http.StatusConflict)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, migrateMaxBodyBytes)
	var req MigrateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if req.TargetClusterID == "" {
		http.Error(w, `{"error":"target_cluster_id is required"}`, http.StatusBadRequest)
		return
	}

	// Validate target cluster exists.
	toCluster, ok := s.clusters[req.TargetClusterID]
	if !ok {
		http.Error(w, `{"error":"unknown target cluster"}`, http.StatusBadRequest)
		return
	}

	// Validate target is different from current.
	currentClusterID := clusterIDForSaaSHive(*h)
	if req.TargetClusterID == currentClusterID {
		http.Error(w, `{"error":"target cluster is the same as current cluster"}`, http.StatusBadRequest)
		return
	}

	fromCluster, ok := s.clusters[currentClusterID]
	if !ok {
		http.Error(w, `{"error":"current cluster config not found"}`, http.StatusInternalServerError)
		return
	}

	// Set migration status and launch background goroutine.
	h.MigrationStatus = "migrating"
	h.MigrationFrom = currentClusterID
	h.MigrationTo = req.TargetClusterID
	h.MigrationStartedAt = time.Now().UTC().Format(time.RFC3339)
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save migration state"}`, http.StatusInternalServerError)
		return
	}

	s.logger.Info("audit: hive migration initiated",
		"hive_id", id, "from", currentClusterID, "to", req.TargetClusterID, "by", username)

	// Registered with provisionWG so tests can drain this goroutine before
	// swapping the package-level saas*Dir vars. Without it, migrateHive's
	// saveSaaSHive call races the next test's temp-dir teardown.
	provisionWG.Add(1)
	go func() {
		defer provisionWG.Done()
		s.migrateHive(h, &fromCluster, &toCluster)
	}()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "migrating",
		"from":   currentClusterID,
		"to":     req.TargetClusterID,
	})
}

var hubAutoUpgradePath = "/data/saas/hub-auto-upgrade"

// The hub's own auto-upgrade is deliberately NOT given the daily schedule that
// per-hive auto-upgrade has. The two look symmetric but carry opposite risk:
//
//   - A spoke hive is a workload. Restarting it interrupts running agents, so
//     deferring to an after-hours window is a clear win — that is exactly the
//     "don't disturb a working hive" motivation for the daily mode.
//   - The hub is the control plane. It is what DELIVERS every spoke upgrade,
//     serves the dashboard, and receives every heartbeat. Holding a hub fix for
//     up to 24 hours means holding back fixes to the upgrade machinery itself,
//     including any fix to this scheduler. A hub restart is also cheap: it is a
//     single stateless pod whose state lives on the PVC, and spokes tolerate a
//     missed heartbeat cycle by design.
//
// Deferring hub upgrades would therefore add real risk (a known-bad hub stays
// up all day) to avoid a disruption the hub does not really suffer. It stays a
// plain on/off toggle. Revisit only if hub restarts are ever shown to disrupt
// in-flight spoke work.
func isHubAutoUpgrade() bool {
	data, err := os.ReadFile(hubAutoUpgradePath)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == "true"
}

func (s *HubServer) handleHubAutoUpgrade(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AutoUpgrade bool `json:"auto_upgrade"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	val := "false"
	if body.AutoUpgrade {
		val = "true"
	}
	os.WriteFile(hubAutoUpgradePath, []byte(val), 0644)
	s.logger.Info("audit: hub auto-upgrade toggled", "enabled", body.AutoUpgrade, "by", s.getAuthUser(r))

	// If enabling and hub is behind, trigger immediately
	if body.AutoUpgrade {
		latestSHA := getLatestHubSHAForBranch(s.hubGitBranch)
		if latestSHA != "" && !sameCommit(latestSHA, s.hubGitHash) {
			s.logger.Info("audit: hub auto-upgrade initial trigger", "from", s.hubGitHash, "to", latestSHA)
			// Route through rolloutHubToSHA so this shares the hub-image gate and
			// the SHA-pin (avoids a stale cached v2-latest) with the poller path.
			if err := s.rolloutHubToSHA(latestSHA); err != nil {
				s.logger.Warn("hub auto-upgrade skipped", "to", latestSHA, "reason", err)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t}`, body.AutoUpgrade)
}

// rolloutHubToSHA upgrades the hub deployment to a specific v2 SHA. It first
// verifies the hub's OWN image (ghcrRepoHub) exists for that SHA on GHCR — a
// separate build job from the spoke image, so a failed hive-hub build must not
// trigger a doomed rollout — then pins the deployment to the immutable
// hive-hub:<sha> tag via `kubectl set image`. Pinning (rather than
// `rollout restart` of the mutable v2-latest tag) forces the node to pull that
// exact image and stops a stale cached v2-latest digest from coming back up
// (the "rolled but still on the old hash" failure). On success it records the
// in-flight state so the dashboard shows "Upgrading". Returns an error the
// caller surfaces; safe to call from both the poller and the admin handler.
func (s *HubServer) rolloutHubToSHA(sha string) error {
	if sha == "" {
		return fmt.Errorf("empty target SHA")
	}
	// Shape-check the tag BEFORE it can reach a live Deployment. Writing an
	// unresolvable tag (the `target1` incident — a test fixture SHA that escaped
	// to the real cluster) leaves the new ReplicaSet in ImagePullBackOff while
	// the old one keeps serving: the hub stays "up" but silently runs stale
	// code. Refusing here leaves the last good image running and makes the
	// failure loud instead of invisible.
	if err := validateImageTag(sha); err != nil {
		s.logger.Error("hub self-upgrade REFUSED: invalid image tag",
			"tag", sha,
			"deployment", hubDeploymentName,
			"namespace", hubNamespace,
			"error", err)
		s.setHubUpgradeFault(fmt.Sprintf("refused invalid image tag %q: %v", sha, err))
		return err
	}
	if !hubImageExists(sha, s.logger) {
		return fmt.Errorf("hub image %s:%s not published yet", ghcrRepoHub, sha)
	}
	image := fmt.Sprintf("ghcr.io/%s:%s", ghcrRepoHub, sha)
	cmd := kubectlForCluster(s.hubCluster(), "set", "image",
		"deployment/"+hubDeploymentName, hubContainerName+"="+image, "-n", hubNamespace)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("kubectl set image failed: %s", strings.TrimSpace(string(out)))
	}
	s.hubUpgradeMu.Lock()
	s.lastHubUpgradeTrigger = time.Now()
	s.hubUpgradeTarget = sha
	s.hubUpgradeFault = "" // a fresh, validated rollout clears any prior refusal
	s.hubUpgradeMu.Unlock()
	// Watch the rollout land. Detect-and-report only: an auto-rollback would
	// race the upgrade poller (which re-triggers the same target every cycle),
	// so a rollback could fight the upgrade loop and flap the deployment. See
	// watchHubRollout.
	go s.watchHubRollout(sha, image)
	return nil
}

// hubRolloutWatchTimeout bounds how long we wait for a self-upgrade we just
// triggered to become Ready before flagging it as stuck. The hub's own image is
// a few hundred MB and a cold node has to pull it, so this is generous enough
// to avoid false alarms while still catching an ImagePullBackOff — which
// back-off-retries indefinitely and would otherwise never surface.
const hubRolloutWatchTimeout = 5 * time.Minute

// hubRolloutPollInterval is how often watchHubRollout re-checks rollout status.
const hubRolloutPollInterval = 15 * time.Second

// watchHubRollout confirms a self-upgrade actually became Ready, and records a
// loud, user-visible fault if it did not.
//
// It deliberately does NOT roll back. The auto-upgrade poller re-evaluates the
// same target every cycle, so an automatic rollback would immediately be undone
// and re-applied, flapping the deployment during an already-degraded window.
// Detect and report is the safe half of the loop: the operator sees the stuck
// upgrade in the UI and in the logs, and decides.
func (s *HubServer) watchHubRollout(sha, image string) {
	deadline := time.Now().Add(hubRolloutWatchTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(hubRolloutPollInterval)
		// `rollout status --timeout=0s` returns non-zero while a rollout is
		// still progressing and zero once it has fully succeeded.
		cmd := kubectlForCluster(s.hubCluster(), "rollout", "status",
			"deployment/"+hubDeploymentName, "-n", hubNamespace, "--timeout=0s")
		if err := cmd.Run(); err == nil {
			s.hubUpgradeMu.Lock()
			s.hubUpgradeFault = ""
			s.hubUpgradeMu.Unlock()
			s.logger.Info("hub self-upgrade rollout completed", "sha", sha, "image", image)
			return
		}
	}
	// Still not Ready. Pull the pod-level reason so the log names the actual
	// cause (ImagePullBackOff / ErrImagePull / CrashLoopBackOff) rather than
	// just "timed out".
	reason := s.hubRolloutFailureReason()
	s.logger.Error("hub self-upgrade STUCK: new ReplicaSet not Ready — hub is still serving the OLD image",
		"sha", sha,
		"image", image,
		"deployment", hubDeploymentName,
		"namespace", hubNamespace,
		"waited", hubRolloutWatchTimeout.String(),
		"reason", reason)
	s.setHubUpgradeFault(fmt.Sprintf("upgrade to %s stuck after %s: %s (still serving the previous image)",
		image, hubRolloutWatchTimeout, reason))
}

// hubRolloutFailureReason best-effort extracts why the hub's pods are not
// Ready, so the stuck-rollout log/status names the real cause.
func (s *HubServer) hubRolloutFailureReason() string {
	const reasonJSONPath = `{range .items[*].status.containerStatuses[*]}{.state.waiting.reason}{" "}{.state.waiting.message}{"\n"}{end}`
	cmd := kubectlForCluster(s.hubCluster(), "get", "pods",
		"-n", hubNamespace, "-l", "app="+hubDeploymentName,
		"-o", "jsonpath="+reasonJSONPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "unknown (could not read pod status)"
	}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return "unknown (no waiting container state reported)"
}

// setHubUpgradeFault records a self-upgrade failure for the dashboard so a
// stuck or refused upgrade is visible in the UI, not only in `kubectl describe`.
func (s *HubServer) setHubUpgradeFault(msg string) {
	s.hubUpgradeMu.Lock()
	s.hubUpgradeFault = msg
	s.hubUpgradeMu.Unlock()
}

// HubUpgradeFault returns the current self-upgrade fault message, or "" when
// the last upgrade attempt was healthy.
func (s *HubServer) HubUpgradeFault() string {
	s.hubUpgradeMu.Lock()
	defer s.hubUpgradeMu.Unlock()
	return s.hubUpgradeFault
}

// hubUpgradeState reports the hub's own upgrade status for the dashboard badge:
//   - "current"   — running the latest v2 SHA
//   - "upgrading" — a rollout we triggered is in flight (within the debounce
//     window after the trigger, before the new pod reports the new hash)
//   - "queued"    — behind latest, auto-upgrade ON, no rollout in flight yet
//     (the poller will trigger one shortly)
//   - "behind"    — behind latest, auto-upgrade OFF (admin must click Upgrade)
//   - "failed"    — an upgrade was REFUSED (malformed image tag) or its rollout
//     never became Ready. The hub is behind and cannot self-heal;
//     an operator must look. HubUpgradeFault() carries the reason.
//   - "unknown"   — latest SHA not resolved yet
//
// This is the field the badge needs: previously the frontend could only tell an
// admin-clicked rollout ("upgrading") from everything else ("queued"), so an
// AUTO rollout in progress showed a misleading "queued".
func (s *HubServer) hubUpgradeState() string {
	// Gate the badge on the HUB image, matching what rolloutHubToSHA can
	// actually roll to — otherwise the UI shows "behind"/"queued" against a
	// target the hub is incapable of reaching.
	latest := getLatestHubSHAForBranch(s.hubGitBranch)
	if latest == "" {
		return "unknown"
	}
	if sameCommit(latest, s.hubGitHash) {
		return "current"
	}
	s.hubUpgradeMu.Lock()
	inFlight := s.hubUpgradeTarget != "" &&
		time.Since(s.lastHubUpgradeTrigger) < hubUpgradeDebounce
	fault := s.hubUpgradeFault
	s.hubUpgradeMu.Unlock()
	// A refused or stuck upgrade outranks "upgrading"/"queued": the hub is
	// behind AND cannot get there on its own, which needs an operator. Without
	// this the badge showed a reassuring "queued" while the rollout was wedged.
	if fault != "" {
		return hubUpgradeStateFailed
	}
	if inFlight {
		return "upgrading"
	}
	if isHubAutoUpgrade() {
		return "queued"
	}
	return "behind"
}

func (s *HubServer) handleHubSelfUpgrade(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	target := getLatestHubSHAForBranch(s.hubGitBranch)
	s.logger.Info("audit: hub self-upgrade triggered", "by", username, "to", target)
	if err := s.rolloutHubToSHA(target); err != nil {
		s.logger.Warn("hub self-upgrade failed", "error", err)
		http.Error(w, `{"error":"hub upgrade failed — check logs"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"upgrading"}`))
}

func (s *HubServer) handleUpgradeHive(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isTrustedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if h.Owner != username && username != hubAdminUsername {
		http.Error(w, `{"error":"only the owner can upgrade"}`, http.StatusForbidden)
		return
	}
	cluster := s.clusterForHive(h)
	if cluster == nil {
		http.Error(w, `{"error":"no cluster config for this hive"}`, http.StatusInternalServerError)
		return
	}
	ns := "hive-hosted-" + id
	cmd := kubectlForCluster(cluster, "rollout", "restart", "deployment/hive", "-n", ns)
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.logger.Warn("upgrade failed", "hive", id, "cluster", cluster.ID, "output", string(out))
		http.Error(w, `{"error":"upgrade failed — check hub logs for details"}`, http.StatusInternalServerError)
		return
	}
	s.logger.Info("audit: hosted hive upgraded", "hive_id", id, "by", username, "cluster", cluster.ID)
	s.recordTimeline(id, TimelineUpgradeStarted, "upgrade requested from the hub dashboard", username)
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			branch := s.registry.Hives[i].GitBranch
			if branch == "" {
				branch = "v2"
			}
			latestSHA := getLatestSHAForBranch(branch)
			s.registry.Hives[i].Upgrading = true
			s.registry.Hives[i].UpgradeTarget = latestSHA
			s.registry.Hives[i].UpgradeStartedAt = time.Now()
			break
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"upgrading"}`))
}

// branchToTag converts a git branch name into a valid Docker image tag.
// Branch names may contain '/' (e.g. feat/x) which is illegal in a tag; the
// docker.yml build sanitizes the same way (feat/x -> feat-x-latest), so the
// hub must match to find the image.
func branchToTag(branch string) string {
	return strings.ReplaceAll(branch, "/", "-")
}

func (s *HubServer) handleSwitchBranch(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isTrustedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if h.Owner != username && username != hubAdminUsername {
		http.Error(w, `{"error":"only the owner can switch branches"}`, http.StatusForbidden)
		return
	}
	var body struct {
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Branch == "" {
		http.Error(w, `{"error":"branch is required"}`, http.StatusBadRequest)
		return
	}
	validBranch := false
	for _, b := range s.trackedBranchList() {
		if b == body.Branch {
			validBranch = true
			break
		}
	}
	// Bootstrap case: trackedBranchList only includes branches already
	// assigned to some hive, so the FIRST hive moved to a new dev branch
	// would never validate. Accept any branch that actually exists on the
	// hive repo — a live one-shot check (fetchBranchSHA populates the SHA
	// cache as a side effect, so the branch is tracked from here on).
	if !validBranch {
		fetchBranchSHA(s.logger, body.Branch)
		if getLatestSHAForBranch(body.Branch) != "" {
			validBranch = true
		}
	}
	if !validBranch {
		http.Error(w, `{"error":"unknown branch (does not exist on the hive repo)"}`, http.StatusBadRequest)
		return
	}
	cluster := s.clusterForHive(h)
	if cluster == nil {
		http.Error(w, `{"error":"no cluster config for this hive"}`, http.StatusInternalServerError)
		return
	}
	ns := "hive-hosted-" + id
	imageTag := branchToTag(body.Branch) + "-latest"
	image := "ghcr.io/kubestellar/hive:" + imageTag
	// Refuse a branch name that sanitizes into something that is not a valid
	// channel tag, rather than stranding the spoke on ImagePullBackOff behind a
	// still-serving old ReplicaSet.
	if err := validateImageTag(imageTag); err != nil {
		s.logger.Error("branch switch REFUSED: invalid image tag",
			"hive", id, "branch", body.Branch, "tag", imageTag, "error", err)
		http.Error(w, `{"error":"branch does not map to a valid image tag"}`, http.StatusBadRequest)
		return
	}
	// Shape is not existence. A DEPRECATED branch (v3 was retired while hives
	// were still pointed at it) keeps a perfectly well-formed "<branch>-latest"
	// tag that CI no longer publishes, so validateImageTag passes and the pull
	// then fails with an opaque "manifest unknown". Kubernetes keeps the old
	// ReplicaSet serving, so the hive looks alive while silently running stale
	// code. Verify the tag is actually pullable before writing it.
	if !spokeImageExists(imageTag, s.logger) {
		s.logger.Error("branch switch REFUSED: image tag not published on GHCR",
			"hive", id, "branch", body.Branch, "tag", imageTag,
			"hint", "branch may be deprecated or its CI image build never completed")
		http.Error(w, `{"error":"no published image for that branch (deprecated branch, or its image build has not completed)"}`, http.StatusBadRequest)
		return
	}
	// "*=" updates every container including init containers (copy-config,
	// init-permissions) — pinning only "hive" left inits on the old branch tag.
	cmd := kubectlForCluster(cluster, "set", "image", "deployment/hive", "*="+image, "-n", ns)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// The hub can't reach this hive's cluster over kubectl (e.g. vllm-d
		// from the hive-oke hub). Fall back to the heartbeat path: record the
		// target tag; the spoke — which has in-cluster RBAC (hive-self-upgrade
		// role) to patch its own deployment — applies it on its next
		// heartbeat. This is the ONLY path that works for unreachable
		// clusters, so it's not an error.
		s.logger.Warn("branch switch kubectl failed, using heartbeat fallback",
			"hive", id, "branch", body.Branch, "output", string(out))
		s.mu.Lock()
		s.heartbeatSwitchTag[id] = imageTag
		for i := range s.registry.Hives {
			if s.registry.Hives[i].ID == id {
				s.registry.Hives[i].Upgrading = true
				s.registry.Hives[i].UpgradeTarget = imageTag
				s.registry.Hives[i].UpgradeStartedAt = time.Now()
				break
			}
		}
		s.mu.Unlock()
		s.logger.Info("audit: hive branch switch queued via heartbeat", "hive_id", id, "branch", body.Branch, "image", image, "by", username)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"status": "switching", "branch": body.Branch, "image": image, "via": "heartbeat"})
		return
	}
	// Restart the deployment to pull the new image
	restartCmd := kubectlForCluster(cluster, "rollout", "restart", "deployment/hive", "-n", ns)
	restartOut, restartErr := restartCmd.CombinedOutput()
	if restartErr != nil {
		s.logger.Warn("rollout restart after branch switch failed", "hive", id, "output", string(restartOut))
	}
	s.logger.Info("audit: hive branch switched", "hive_id", id, "branch", body.Branch, "image", image, "by", username)
	s.recordTimeline(id, TimelineBranchChanged,
		fmt.Sprintf("branch switch to %s requested (image %s)", body.Branch, image), username)
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			s.registry.Hives[i].Upgrading = true
			s.registry.Hives[i].UpgradeTarget = branchToTag(body.Branch) + "-latest"
			s.registry.Hives[i].UpgradeStartedAt = time.Now()
			break
		}
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status": "switching",
		"branch": body.Branch,
		"image":  image,
	})
}

func (s *HubServer) handleToggleVisibility(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isTrustedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)

	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found — only hosted hives can be toggled from here"}`, http.StatusNotFound)
		return
	}
	if h.Owner != username && username != hubAdminUsername {
		http.Error(w, `{"error":"only the owner can change visibility"}`, http.StatusForbidden)
		return
	}

	var body struct {
		IsPublic bool `json:"is_public"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// The hub's registry and SaaS store are the source of truth for the
	// dashboard, so persist the change here first — the same pattern used
	// by handleToggleAutoUpgrade. Pushing the new value to the spoke hive
	// is a best-effort, asynchronous notification: the spoke re-reports
	// its is_public value on every heartbeat, so a transient failure to
	// reach it (pod restarting, rollout in progress, etc.) must not block
	// or fail the user-facing toggle.
	h.IsPublic = body.IsPublic
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save"}`, http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	for i, reg := range s.registry.Hives {
		if reg.ID == id {
			s.registry.Hives[i].IsPublic = body.IsPublic
			break
		}
	}
	s.mu.Unlock()

	s.logger.Info("audit: visibility toggled", "hive_id", id, "is_public", body.IsPublic, "by", username)

	go s.pushVisibilityToSpoke(id, body.IsPublic)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"is_public":%t}`, body.IsPublic)
}

// pushVisibilityToSpoke best-effort notifies a hosted hive's own governor
// config of a visibility change made from the hub dashboard. It never
// affects the outcome of the toggle request — the hub's registry/SaaS
// store already reflect the change; this just keeps the spoke's local
// config in sync so it doesn't overwrite the hub's value on its next
// heartbeat. Failures are logged, not surfaced to the user.
func (s *HubServer) pushVisibilityToSpoke(id string, isPublic bool) {
	const goAPIPort = 3002
	const visibilityPushTimeout = 10 * time.Second
	ns := "hive-hosted-" + id
	svcURL := fmt.Sprintf("http://hive.%s.svc.cluster.local:%d/api/config/governor/hub", ns, goAPIPort)
	payload := fmt.Sprintf(`{"is_public":%t}`, isPublic)
	req, err := http.NewRequest("PUT", svcURL, strings.NewReader(payload))
	if err != nil {
		s.logger.Warn("visibility spoke push: failed to create request", "hive", id, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: visibilityPushTimeout}
	spokeResp, err := client.Do(req)
	if err != nil {
		s.logger.Warn("visibility spoke push failed, will resync on next heartbeat", "hive", id, "error", err)
		return
	}
	defer spokeResp.Body.Close()
	if spokeResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(spokeResp.Body)
		s.logger.Warn("visibility spoke push rejected", "hive", id, "status", spokeResp.StatusCode, "body", string(respBody))
	}
}

func (s *HubServer) handleToggleAutoUpgrade(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isTrustedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		// Hive may exist in registry via heartbeat but have no SaaS entry yet.
		// Create a minimal entry so the auto-upgrade preference can be stored
		// and delivered via heartbeat response.
		s.mu.RLock()
		var regEntry *RegistryEntry
		for i := range s.registry.Hives {
			if s.registry.Hives[i].ID == id {
				regEntry = &s.registry.Hives[i]
				break
			}
		}
		s.mu.RUnlock()
		if regEntry == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"hive not found"}`)
			return
		}
		h = &SaaSHive{
			ID:    id,
			Owner: regEntry.Owner,
			Org:   regEntry.Org,
			Repos: regEntry.Repos,
		}
	}
	if h.Owner != username && username != hubAdminUsername {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"only the owner can change auto-upgrade"}`)
		return
	}
	// The mode rides on the EXISTING endpoint rather than a second one: it is
	// the same preference, the same owner-or-admin authorization checked above,
	// and the same persistence. A separate endpoint would let the two settings
	// drift apart across two requests. Older clients that send only
	// auto_upgrade omit the field, which reads as "" = instant, preserving
	// their behaviour exactly.
	var body struct {
		AutoUpgrade bool   `json:"auto_upgrade"`
		Mode        string `json:"auto_upgrade_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid request body"}`)
		return
	}
	// Reject unknown modes instead of defaulting — a typo must not silently
	// change how often a hive restarts.
	if !isValidAutoUpgradeMode(body.Mode) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid auto_upgrade_mode (expected \"instant\" or \"daily\")"}`)
		return
	}
	h.AutoUpgrade = body.AutoUpgrade
	h.AutoUpgradeMode = body.Mode
	// Switching modes clears the day's fire record. Otherwise a hive flipped to
	// daily after an instant upgrade earlier today would inherit a stale date
	// and skip tonight's window.
	h.AutoUpgradeLastFired = ""
	if err := saveSaaSHive(h); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"failed to save"}`)
		return
	}
	s.logger.Info("audit: auto-upgrade toggled", "hive_id", id, "auto_upgrade", body.AutoUpgrade, "mode", normalizeAutoUpgradeMode(body.Mode), "by", username)

	// If enabling auto-upgrade and hive is behind, trigger immediately via kubectl
	// for hosted hives. For heartbeat-connected hives, the upgrade instruction
	// is delivered via the heartbeat response.
	// Daily mode deliberately does NOT kick an upgrade here: the whole point of
	// choosing it is that turning auto-upgrade on should not immediately
	// restart a working hive. It will roll at the next daily ET window
	// (autoUpgradeDailyHour, currently 13:00 / 1pm ET). The
	// operator who wants it now still has the explicit Upgrade button, which
	// goes through handleUpgradeHive and is never gated by mode.
	if body.AutoUpgrade && normalizeAutoUpgradeMode(body.Mode) == AutoUpgradeModeInstant {
		s.mu.RLock()
		var currentSHA, branch string
		for _, reg := range s.registry.Hives {
			if reg.ID == id {
				currentSHA = reg.GitHash
				branch = reg.GitBranch
				break
			}
		}
		s.mu.RUnlock()
		if branch == "" {
			branch = "v2"
		}
		latestSHA := getLatestSHAForBranch(branch)
		if latestSHA != "" && currentSHA != "" && !sameCommit(currentSHA, latestSHA) {
			s.logger.Info("audit: auto-upgrade initial trigger", "hive_id", id, "from", currentSHA, "to", latestSHA)
			s.mu.Lock()
			for i := range s.registry.Hives {
				if s.registry.Hives[i].ID == id {
					s.registry.Hives[i].Upgrading = true
					s.registry.Hives[i].UpgradeTarget = latestSHA
					s.registry.Hives[i].UpgradeStartedAt = time.Now()
					break
				}
			}
			s.mu.Unlock()
			hiveCluster := s.clusterForHive(h)
			if hiveCluster != nil {
				ns := "hive-hosted-" + id
				cmd := kubectlForCluster(hiveCluster, "rollout", "restart", "deployment/hive", "-n", ns)
				if out, err := cmd.CombinedOutput(); err != nil {
					s.logger.Warn("auto-upgrade initial trigger failed (will retry via heartbeat)", "hive", id, "cluster", hiveCluster.ID, "output", string(out))
				}
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t,"auto_upgrade_mode":%q}`, body.AutoUpgrade, normalizeAutoUpgradeMode(body.Mode))
}

// branchSHAInfo holds a short SHA and the first line of its commit message.
type branchSHAInfo struct {
	SHA     string `json:"sha"`
	Message string `json:"message"`
}

// Container-image build status values for a branch head commit, exposed to
// the dashboard as latest_sha_image_status.
const (
	imageStatusReady    = "ready"    // image tag verified on GHCR
	imageStatusBuilding = "building" // docker workflow queued/in progress, or image not yet visible
	imageStatusFailed   = "failed"   // docker workflow completed unsuccessfully
)

// branchHeadInfo tracks the branch HEAD commit, which may be ahead of the
// image-verified SHA in latestSHAByBranch while its container image builds.
type branchHeadInfo struct {
	SHA         string
	Message     string
	ImageStatus string
}

// githubAPIBase and ghcrBase are the GitHub/GHCR origins used by the SHA-poll
// fetch helpers. They are vars (not consts) so tests can point those helpers at
// a local httptest server; production never reassigns them.
var (
	githubAPIBase = "https://api.github.com"
	ghcrBase      = "https://ghcr.io"
)

// GHCR image repositories built by .github/workflows/docker.yml. Each is a
// SEPARATE build job tagged with the short git SHA, so one can succeed while
// another fails for the same commit. The hub runs ghcrRepoHub; spokes run
// ghcrRepoSpoke. The hub self-upgrade MUST verify ghcrRepoHub (not ghcrRepoSpoke)
// before targeting a SHA — otherwise a failed hive-hub build leaves the hub
// chasing a SHA whose hub image was never pushed, and the rollout falls back to
// a stale cached image.
const (
	ghcrRepoSpoke = "kubestellar/hive"
	ghcrRepoHub   = "kubestellar/hive-hub"

	// hubDeploymentName / hubContainerName / hubNamespace identify the hub's own
	// Kubernetes objects for self-upgrade. NOTE the container is named "hub", not
	// "hive-hub" — a `kubectl set image` that names the wrong container silently
	// no-ops the target (or errors), so pin the image against hubContainerName.
	hubDeploymentName = "hive-hub"
	hubContainerName  = "hub"
	hubNamespace      = "hive-hub"

	// hubUpgradeStateFailed is the hubUpgradeState() value meaning the hub is
	// behind latest AND its last upgrade attempt was refused or wedged, so it
	// cannot reach latest without operator action.
	hubUpgradeStateFailed = "failed"
)

// hubImageExists reports whether the hub's own container image is published on
// GHCR for the given SHA. A var (not a plain call) so tests can stub the GHCR
// round-trip; production checks ghcrRepoHub over the network.
var hubImageExists = func(sha string, logger *slog.Logger) bool {
	client := &http.Client{Timeout: 10 * time.Second}
	return ghcrTagExists(client, ghcrRepoHub, sha, logger)
}

// spokeImageExists is the ghcrRepoSpoke counterpart of hubImageExists: it
// reports whether a SPOKE tag (a "<branch>-latest" channel tag or a SHA) is
// actually published. Separate from hubImageExists because the two repos are
// independent build jobs — the hub image for a SHA can exist while the spoke
// image for that same SHA does not. A var so tests can stub the GHCR round-trip.
var spokeImageExists = func(tag string, logger *slog.Logger) bool {
	client := &http.Client{Timeout: 10 * time.Second}
	return ghcrTagExists(client, ghcrRepoSpoke, tag, logger)
}

var (
	latestSHAMu sync.RWMutex
	// latestSHAByBranch only ever advances to SHAs whose container image is
	// verified pullable on GHCR — it drives upgrade targets.
	latestSHAByBranch = map[string]branchSHAInfo{}
	// latestHubSHAByBranch is the same idea for the HUB's own image, which is a
	// separate build from the spoke's (ghcrRepoHub vs ghcrRepoSpoke) and can
	// succeed or fail independently for the same commit. Tracking one shared
	// value meant the hub's upgrade target was gated on the SPOKE image: when a
	// spoke build failed but the hub build succeeded, that SHA never entered
	// latestSHAByBranch, so the hub could never roll to it and reported
	// "current" against a stale target.
	latestHubSHAByBranch = map[string]branchSHAInfo{}
	// headSHAByBranch advances to the branch HEAD immediately so the
	// dashboard can show the newest commit with a build-status indicator.
	headSHAByBranch = map[string]branchHeadInfo{}
	// commitMsgBySHA caches the first line of each commit message, keyed by short SHA.
	commitMsgBySHA = map[string]string{}
)

// trackedBranches lists the always-tracked branches that produce Docker
// images via CI. Personal dev branches (e.g. mk) are tracked dynamically:
// see HubServer.trackedBranchList.
var trackedBranches = []string{"v2", "v3"}

// trackedBranchList returns the static CI branches plus every branch some
// registered hive is assigned to, so a personal dev branch gets SHA polling,
// Latest-images display, branch-switch validation, and auto-upgrade without
// a hub code change per branch. Static branches keep their order and come
// first. Caller must not hold s.mu.
func (s *HubServer) trackedBranchList() []string {
	seen := make(map[string]bool, len(trackedBranches))
	out := make([]string, 0, len(trackedBranches))
	add := func(b string) {
		if b != "" && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	for _, b := range trackedBranches {
		add(b)
	}
	// Branches already assigned to a hive.
	s.mu.RLock()
	for _, h := range s.registry.Hives {
		add(h.GitBranch)
	}
	s.mu.RUnlock()
	// Any branch that has a published <branch>-latest image on GHCR, even
	// with no hive assigned yet — so the UI lists every assignable branch
	// and a user can switch to one before any hive uses it.
	for _, b := range discoveredImageBranches() {
		add(b)
	}
	return out
}

var (
	imageBranchMu       sync.RWMutex
	imageBranchCache    []string
	imageBranchCachedAt time.Time
)

const imageBranchCacheTTL = 5 * time.Minute

// discoveredImageBranches returns branch names inferred from published
// ghcr.io/kubestellar/hive:<branch>-latest tags (cached). A tag with a '-'
// that our sanitizer would have produced can't be reversed unambiguously, so
// we only surface tags that round-trip: the tag minus the "-latest" suffix.
// Slashless branches (v2, v3, mk) round-trip exactly; slashed branches
// (feat/x → feat-x-latest) surface as "feat-x", which is still a valid
// switch target because switch-branch/branchToTag normalize both to the same
// image tag.
func discoveredImageBranches() []string {
	imageBranchMu.RLock()
	if time.Since(imageBranchCachedAt) < imageBranchCacheTTL && imageBranchCache != nil {
		cp := append([]string(nil), imageBranchCache...)
		imageBranchMu.RUnlock()
		return cp
	}
	imageBranchMu.RUnlock()

	const listTimeout = 8 * time.Second
	client := &http.Client{Timeout: listTimeout}
	imageBranches := listLatestImageBranches(client)

	// A merged branch is deleted but its <branch>-latest image lingers on
	// GHCR, so the image list alone would offer dead branches in the
	// switcher. Keep only image branches whose branch still EXISTS on the
	// repo. Branch names are compared in their sanitized (tag) form because
	// the image tag is branchToTag(branch); e.g. real "feat/x" ⇒ image
	// "feat-x-latest" ⇒ we must match it back to the live "feat/x".
	live := map[string]struct{}{}
	for _, b := range listRepoBranches(client) {
		live[branchToTag(b)] = struct{}{}
	}
	branches := imageBranches
	if len(live) > 0 { // only filter when the branch list fetch succeeded
		branches = branches[:0]
		for _, b := range imageBranches {
			if _, ok := live[b]; ok {
				branches = append(branches, b)
			}
		}
	}

	imageBranchMu.Lock()
	imageBranchCache = branches
	imageBranchCachedAt = time.Now()
	imageBranchMu.Unlock()
	return branches
}

// listRepoBranches returns the names of branches on kubestellar/hive via the
// GitHub API (paginated). Best-effort: returns nil on any failure so callers
// treat "unknown" as "don't filter" rather than hiding valid branches.
func listRepoBranches(client *http.Client) []string {
	var names []string
	url := githubAPIBase + "/repos/kubestellar/hive/branches?per_page=100"
	const maxPages = 10
	for page := 0; url != "" && page < maxPages; page++ {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := client.Do(req)
		if err != nil {
			return nil
		}
		var body []struct {
			Name string `json:"name"`
		}
		decErr := json.NewDecoder(resp.Body).Decode(&body)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decErr != nil {
			return nil
		}
		for _, b := range body {
			names = append(names, b.Name)
		}
		url = nextGitHubLink(link)
	}
	return names
}

// nextGitHubLink extracts the rel="next" URL from a GitHub Link header (an
// absolute URL), or "".
func nextGitHubLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		if i, j := strings.Index(part, "<"), strings.Index(part, ">"); i >= 0 && j > i {
			return part[i+1 : j]
		}
	}
	return ""
}

// listLatestImageBranches queries the GHCR tag list for
// ghcr.io/kubestellar/hive and returns the branch name of every "<x>-latest"
// tag (the "<x>" part).
func listLatestImageBranches(client *http.Client) []string {
	tokenResp, err := client.Get(ghcrBase + "/token?scope=repository:kubestellar/hive:pull")
	if err != nil {
		return nil
	}
	defer tokenResp.Body.Close()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		return nil
	}

	// The registry tag list is paginated via RFC5988 Link headers. Untagged
	// build digests aside, the repo can hold thousands of SHA tags, so the
	// "<branch>-latest" tags we want may live on a later page — follow Link
	// until exhausted (bounded) rather than reading only the first page.
	branchSet := map[string]struct{}{}
	next := ghcrBase + "/v2/kubestellar/hive/tags/list?n=1000"
	const maxPages = 20 // bound: up to ~20k tags
	for page := 0; next != "" && page < maxPages; page++ {
		req, _ := http.NewRequest("GET", next, nil)
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		resp, err := client.Do(req)
		if err != nil {
			break
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&body)
		link := resp.Header.Get("Link")
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || decodeErr != nil {
			break
		}
		for _, t := range body.Tags {
			if strings.HasSuffix(t, "-latest") {
				branchSet[strings.TrimSuffix(t, "-latest")] = struct{}{}
			}
		}
		next = nextLinkURL(link)
	}
	branches := make([]string, 0, len(branchSet))
	for b := range branchSet {
		branches = append(branches, b)
	}
	return branches
}

// nextLinkURL extracts the rel="next" URL from a registry Link header, or "".
// GHCR returns a relative path (e.g. </v2/.../tags/list?last=...&n=1000>),
// which we resolve against the registry host.
func nextLinkURL(link string) string {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start := strings.Index(part, "<")
		end := strings.Index(part, ">")
		if start < 0 || end < 0 || end <= start {
			return ""
		}
		u := part[start+1 : end]
		if strings.HasPrefix(u, "/") {
			return ghcrBase + u
		}
		return u
	}
	return ""
}

// provisionWG tracks async hive-provisioning goroutines so tests (which swap
// the package-level saas*Dir path variables) can wait for them to drain
// before mutating shared state.
var provisionWG sync.WaitGroup

const latestSHAPollInterval = 2 * time.Minute

func getLatestSHA() string {
	return getLatestSHAForBranch("v2")
}

func getLatestSHAForBranch(branch string) string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	return latestSHAByBranch[branch].SHA
}

// getLatestHubSHAForBranch returns the newest SHA on this branch whose HUB
// image is verified pullable. Use this — never getLatestSHAForBranch — for the
// hub's own upgrade target, since the hub and spoke images are separate builds
// that can fail independently for the same commit.
func getLatestHubSHAForBranch(branch string) string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	return latestHubSHAByBranch[branch].SHA
}

// getLatestHubSHAs returns a branch→SHA map of the newest commit per branch
// whose HUB image is verified pullable. Exposed alongside getLatestSHAs so an
// operator can see the two independent build results side by side: the hub and
// spoke images are separate builds that can succeed in either order (or fail
// independently) for the same commit, which is why the caches are separate.
func getLatestHubSHAs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestHubSHAByBranch))
	for k, v := range latestHubSHAByBranch {
		cp[k] = v.SHA
	}
	return cp
}

// getHeadSHAs returns a branch→HEAD-SHA map: the newest commit the poller has
// seen on each branch, regardless of whether its images exist yet. Comparing
// this against getLatestSHAs tells an operator WHY the advertised SHA is behind
// HEAD — see getImageStatuses for the distinguishing signal.
func getHeadSHAs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(headSHAByBranch))
	for k, v := range headSHAByBranch {
		if v.SHA != "" {
			cp[k] = v.SHA
		}
	}
	return cp
}

// getLatestSHAs returns a branch→SHA map (backward-compatible string values).
func getLatestSHAs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v.SHA
	}
	return cp
}

// getLatestSHAMessages returns a branch→commit-message map for tooltip display.
func getLatestSHAMessages() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v.Message
	}
	return cp
}

// getCommitMessages returns a short-SHA→commit-message map for tooltip display.
func getCommitMessages() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(commitMsgBySHA))
	for k, v := range commitMsgBySHA {
		cp[k] = v
	}
	return cp
}

// getBranchHead returns the tracked HEAD info for a branch (zero value if
// no head fetch has succeeded since startup).
func getBranchHead(branch string) branchHeadInfo {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	return headSHAByBranch[branch]
}

// setBranchHead records the branch HEAD and its image build status, keeping
// the previous commit message when the new fetch didn't include one.
func setBranchHead(branch, sha, msg, status string) {
	latestSHAMu.Lock()
	defer latestSHAMu.Unlock()
	if msg == "" && headSHAByBranch[branch].SHA == sha {
		msg = headSHAByBranch[branch].Message
	}
	headSHAByBranch[branch] = branchHeadInfo{SHA: sha, Message: msg, ImageStatus: status}
	if msg != "" {
		commitMsgBySHA[sha] = msg
	}
}

// getDisplaySHAs returns the branch→SHA map shown under "Latest images":
// the branch HEAD when known (its image may still be building), falling back
// to the last image-verified SHA (e.g. right after a hub restart, before the
// first head fetch succeeds).
func getDisplaySHAs() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch)+len(headSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v.SHA
	}
	for k, v := range headSHAByBranch {
		if v.SHA != "" {
			cp[k] = v.SHA
		}
	}
	return cp
}

// getDisplaySHAMessages returns branch→commit-message for the SHAs returned
// by getDisplaySHAs.
func getDisplaySHAMessages() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch)+len(headSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v.Message
	}
	for k, v := range headSHAByBranch {
		if v.SHA != "" {
			cp[k] = v.Message
		}
	}
	return cp
}

// getImageStatuses returns branch→image build status for the SHAs returned
// by getDisplaySHAs. Branches known only from the image-verified cache are
// ready by definition.
func getImageStatuses() map[string]string {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]string, len(latestSHAByBranch)+len(headSHAByBranch))
	for k := range latestSHAByBranch {
		cp[k] = imageStatusReady
	}
	for k, v := range headSHAByBranch {
		if v.SHA != "" && v.ImageStatus != "" {
			cp[k] = v.ImageStatus
		}
	}
	return cp
}

// latestSHAsPath persists the last-known-good branch→SHA cache across hub
// restarts (PVC-backed, like the rest of /data/saas). The hub restarts on
// every auto-upgrade; without this file the cache starts empty, and if the
// unauthenticated GitHub branches API is rate-limited for one branch at
// startup, that branch silently disappears from "Latest images" until a
// later poll succeeds (up to an hour under rate limiting).
var latestSHAsPath = "/data/saas/latest-shas.json"

// snapshotBranchSHAs returns a copy of the full branch→info cache.
func snapshotBranchSHAs() map[string]branchSHAInfo {
	latestSHAMu.RLock()
	defer latestSHAMu.RUnlock()
	cp := make(map[string]branchSHAInfo, len(latestSHAByBranch))
	for k, v := range latestSHAByBranch {
		cp[k] = v
	}
	return cp
}

// loadPersistedSHAs restores the last-known-good SHA cache from disk so a
// freshly restarted hub serves the previous values while live fetches are
// failing or rate-limited. Branches no longer in trackedBranches are dropped;
// branches already populated by a live fetch are never overwritten.
func loadPersistedSHAs(logger *slog.Logger, branches []string) {
	data, err := os.ReadFile(latestSHAsPath)
	if err != nil {
		return // first run or no PVC — nothing to restore
	}
	var persisted map[string]branchSHAInfo
	if err := json.Unmarshal(data, &persisted); err != nil {
		logger.Warn("SHA poll: persisted SHA cache unreadable, ignoring", "path", latestSHAsPath, "error", err)
		return
	}
	latestSHAMu.Lock()
	defer latestSHAMu.Unlock()
	for _, branch := range branches {
		info, ok := persisted[branch]
		if !ok || info.SHA == "" {
			continue
		}
		if _, exists := latestSHAByBranch[branch]; exists {
			continue // live fetch already populated this branch
		}
		latestSHAByBranch[branch] = info
		if info.Message != "" {
			commitMsgBySHA[info.SHA] = info.Message
		}
		logger.Info("SHA poll: restored last-known SHA from disk", "branch", branch, "sha", info.SHA)
	}
}

// persistLatestSHAs writes the current SHA cache to disk (atomic tmp+rename,
// same pattern as the other /data/saas state files).
func persistLatestSHAs(logger *slog.Logger) {
	snapshot := snapshotBranchSHAs()
	if len(snapshot) == 0 {
		return // never overwrite a good file with an empty cache
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		logger.Warn("SHA poll: persist marshal failed", "error", err)
		return
	}
	os.MkdirAll(filepath.Dir(latestSHAsPath), 0o755)
	tmpPath := latestSHAsPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		logger.Warn("SHA poll: persist write failed", "path", latestSHAsPath, "error", err)
		return
	}
	if err := os.Rename(tmpPath, latestSHAsPath); err != nil {
		logger.Warn("SHA poll: persist rename failed", "path", latestSHAsPath, "error", err)
	}
}

// StartLatestSHAPoller polls GitHub/GHCR for the latest branch SHAs until ctx
// is cancelled. It takes a context so the loop can be stopped for a clean
// shutdown (and so tests can stop the goroutine rather than leaking it past the
// test, which otherwise races the package-level saas path state that per-test
// temp-dir setup rewrites — same class as startProvisionWatcher).
func (s *HubServer) StartLatestSHAPoller(ctx context.Context) {
	// Serve last-known-good SHAs immediately; live fetches below refresh them.
	loadPersistedSHAs(s.logger, s.trackedBranchList())
	prevInfos := snapshotBranchSHAs()
	fetchAllBranchSHAs(s.logger, s.trackedBranchList())
	if cur := snapshotBranchSHAs(); !maps.Equal(cur, prevInfos) {
		persistLatestSHAs(s.logger)
	}
	// On first poll, check if any auto-upgrade hives are behind
	s.triggerAutoUpgrades()
	// Clear Upgrading flags orphaned by spoke pods that vanished mid-upgrade.
	// Runs alongside — not inside — triggerAutoUpgrades because that function
	// only ever considers hives with AutoUpgrade enabled, while the flag is set
	// by the admin and bulk upgrade paths for any hive.
	s.sweepOrphanedUpgrades()
	ticker := time.NewTicker(latestSHAPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		oldSHAs := getLatestSHAs()
		oldInfos := snapshotBranchSHAs()
		// Re-resolve each tick so branches from newly registered hives are
		// picked up without a hub restart.
		fetchAllBranchSHAs(s.logger, s.trackedBranchList())
		newSHAs := getLatestSHAs()
		if !maps.Equal(snapshotBranchSHAs(), oldInfos) {
			persistLatestSHAs(s.logger)
		}
		// Always check for pending auto-upgrades (retries failed/missed hives).
		s.triggerAutoUpgrades()
		s.sweepOrphanedUpgrades()
		changed := false
		for branch, sha := range newSHAs {
			if sha != "" && sha != oldSHAs[branch] {
				changed = true
				break
			}
		}
		_ = changed
		// Hub auto-upgrade — checked EVERY cycle, not only when the SHA just
		// changed. Previously this lived inside `if changed {}`, so if the hub
		// missed the one poll where v2's SHA flipped (busy, mid-restart, or the
		// SHA moved between polls), it stayed "queued" forever and never retried,
		// while spokes retry every cycle via triggerAutoUpgrades() above. Mirror
		// that: whenever auto-upgrade is on and the hub is behind latest v2, trigger
		// a rollout restart. A debounce prevents re-restarting every 2min while a
		// restart is already rolling out (the new pod reports the new hash, which
		// clears the condition, but the poll can fire before the rollout lands).
		// Use the hub's OWN branch, not a hardcoded "v2": hubUpgradeState() and
		// handleHubSelfUpgrade() both read s.hubGitBranch, so hardcoding here
		// made the badge and the poller disagree the moment a hub ran on v3.
		hubBranchSHA := getLatestHubSHAForBranch(s.hubGitBranch)
		s.hubUpgradeMu.Lock()
		debounced := time.Since(s.lastHubUpgradeTrigger) > hubUpgradeDebounce
		s.hubUpgradeMu.Unlock()
		if isHubAutoUpgrade() && hubBranchSHA != "" && !sameCommit(hubBranchSHA, s.hubGitHash) && debounced {
			s.logger.Info("audit: hub auto-upgrade triggered", "from", s.hubGitHash, "to", hubBranchSHA)
			// rolloutHubToSHA verifies the hub image exists (skips a doomed roll
			// when the hive-hub build for this SHA failed) and pins the SHA so a
			// stale cached v2-latest can't come back up. It records the in-flight
			// state on success so the dashboard shows "Upgrading", not "queued".
			if err := s.rolloutHubToSHA(hubBranchSHA); err != nil {
				s.logger.Warn("hub auto-upgrade skipped", "to", hubBranchSHA, "reason", err)
			}
		}
	}
}

// clusterRecentlyUnreachable reports whether the hub failed to reach this
// cluster recently enough that another kubectl attempt would just burn the
// timeout again. Callers should go straight to the heartbeat fallback instead.
func (s *HubServer) clusterRecentlyUnreachable(clusterID string) bool {
	if clusterID == "" {
		return false
	}
	s.clusterUnreachableMu.Lock()
	defer s.clusterUnreachableMu.Unlock()
	until, ok := s.clusterUnreachableUntil[clusterID]
	return ok && time.Now().Before(until)
}

// markClusterUnreachable starts (or extends) the kubectl suppression window for
// a cluster the hub just failed to dial.
func (s *HubServer) markClusterUnreachable(clusterID string) {
	if clusterID == "" {
		return
	}
	s.clusterUnreachableMu.Lock()
	defer s.clusterUnreachableMu.Unlock()
	s.clusterUnreachableUntil[clusterID] = time.Now().Add(clusterUnreachableTTL)
}

// markClusterReachable clears any suppression after a kubectl call succeeds, so
// a cluster that recovers is used immediately rather than waiting out the TTL.
func (s *HubServer) markClusterReachable(clusterID string) {
	if clusterID == "" {
		return
	}
	s.clusterUnreachableMu.Lock()
	defer s.clusterUnreachableMu.Unlock()
	delete(s.clusterUnreachableUntil, clusterID)
}

// rolloutRestartHive issues the auto-upgrade `kubectl rollout restart` for one
// hive under a bounded timeout, honouring and maintaining the unreachable-cluster
// cache. It returns false when kubectl did not deliver the restart — the caller
// must then arm the heartbeat fallback, which is the ONLY path that works for
// firewalled clusters. Skipping is reported as a plain failure (not an error) so
// callers treat "known unreachable" and "just failed" identically.
func (s *HubServer) rolloutRestartHive(cluster *ClusterConfig, hiveID string) bool {
	if s.clusterRecentlyUnreachable(cluster.ID) {
		s.logger.Debug("auto-upgrade kubectl skipped — cluster known unreachable, using heartbeat",
			"hive", hiveID, "cluster", cluster.ID)
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), upgradeKubectlTimeout)
	defer cancel()
	ns := "hive-hosted-" + hiveID
	cmd := kubectlForClusterContext(ctx, cluster, "rollout", "restart", "deployment/hive", "-n", ns)
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.markClusterUnreachable(cluster.ID)
		s.logger.Warn("auto-upgrade kubectl failed, falling back to heartbeat",
			"hive", hiveID, "cluster", cluster.ID,
			"timeout", upgradeKubectlTimeout, "output", strings.TrimSpace(string(out)))
		return false
	}
	s.markClusterReachable(cluster.ID)
	return true
}

func (s *HubServer) triggerAutoUpgrades() {
	hives := listSaaSHives()
	for _, h := range hives {
		if !h.AutoUpgrade {
			continue
		}
		// Scheduling gate. Instant-mode hives (and every legacy record, whose
		// mode is empty) pass straight through, so this changes nothing for the
		// existing fleet. Daily-mode hives are held until the first cycle at or
		// after autoUpgradeDailyHour ET and released only once per ET day.
		// Evaluated BEFORE any of the work below so a held hive costs nothing.
		decision := shouldAutoUpgradeNow(h.AutoUpgradeMode, h.AutoUpgradeLastFired, time.Now())
		if !decision.Allowed {
			s.logger.Debug("auto-upgrade held by schedule",
				"hive_id", h.ID, "mode", h.AutoUpgradeMode, "reason", decision.Reason)
			continue
		}
		// Skip hives that are actively provisioning or in error state.
		// Empty status means the hive predates the provisioning system — treat as eligible.
		if h.Status == "provisioning" || h.Status == "error" {
			continue
		}
		s.mu.RLock()
		var currentSHA, branch, upgradeTarget string
		var alreadyUpgrading bool
		var upgradeStartedAt time.Time
		for _, reg := range s.registry.Hives {
			if reg.ID == h.ID {
				currentSHA = reg.GitHash
				branch = reg.GitBranch
				alreadyUpgrading = reg.Upgrading
				upgradeTarget = reg.UpgradeTarget
				upgradeStartedAt = reg.UpgradeStartedAt
				break
			}
		}
		s.mu.RUnlock()
		if alreadyUpgrading {
			if branch == "" {
				branch = "v2"
			}
			upgradeAge := time.Since(upgradeStartedAt)
			// Zero UpgradeStartedAt means the timestamp was lost (heartbeats
			// used to wipe it on rebuild) — treat as stale so already-stuck
			// hives self-heal instead of upgrading forever.
			isStale := upgradeStartedAt.IsZero() || upgradeAge > staleUpgradeTimeout

			if isStale {
				// Upgrade has been stuck longer than staleUpgradeTimeout.
				// Recover it. Two things can be wrong: (a) the target SHA
				// contains a crashing bug a newer commit fixes — advance the
				// target to latest; (b) the kubectl rollout never reached the
				// spoke (e.g. the hub can't route to the hive's cluster API),
				// so the upgrade was never actually delivered.
				//
				// The heartbeat fallback (heartbeatUpgrade → the spoke
				// self-restarts on its next heartbeat) is the ONLY path that
				// works when kubectl can't reach the cluster, so re-arm it
				// unconditionally for a stale upgrade — not only when the
				// target advances. Previously, when the target already equalled
				// latest, this branch was skipped entirely and the hive stayed
				// latched-upgrading forever behind an unreachable kubectl.
				latestSHA := getLatestSHAForBranch(branch)
				recoverTarget := upgradeTarget
				if latestSHA != "" && latestSHA != upgradeTarget {
					s.logger.Warn("advancing upgrade target for stale upgrade",
						"hive", h.ID, "stale_minutes", int(upgradeAge.Minutes()),
						"old_target", upgradeTarget, "new_target", latestSHA)
					recoverTarget = latestSHA
				} else {
					s.logger.Warn("re-arming heartbeat fallback for stale upgrade",
						"hive", h.ID, "stale_minutes", int(upgradeAge.Minutes()),
						"target", recoverTarget)
				}
				if recoverTarget != "" {
					s.mu.Lock()
					for i := range s.registry.Hives {
						if s.registry.Hives[i].ID == h.ID {
							s.registry.Hives[i].UpgradeTarget = recoverTarget
							s.registry.Hives[i].UpgradeStartedAt = time.Now()
							break
						}
					}
					s.heartbeatUpgrade[h.ID] = recoverTarget
					s.mu.Unlock()
					hiveCluster := s.clusterForHive(&h)
					if hiveCluster != nil && !hiveCluster.InCluster {
						// Failure here is expected when the hub can't reach the
						// hive's cluster; the heartbeat fallback armed above is
						// what actually delivers the upgrade. rolloutRestartHive
						// bounds the attempt and skips it entirely for clusters
						// already known unreachable, so this recovery path can't
						// re-introduce the per-hive timeout stall either.
						s.rolloutRestartHive(hiveCluster, h.ID)
					}
					continue
				}
			}

			// Not stale — keep the original target so the hive can satisfy it.
			// Re-populate the heartbeatUpgrade map in case the hub restarted.
			hiveCluster := s.clusterForHive(&h)
			if hiveCluster != nil && !hiveCluster.InCluster {
				if upgradeTarget != "" {
					s.mu.Lock()
					s.heartbeatUpgrade[h.ID] = upgradeTarget
					s.mu.Unlock()
				}
			}
			s.logger.Debug("skipping target advance — upgrade still in progress",
				"hive", h.ID, "current", currentSHA)
			continue
		}
		if branch == "" {
			branch = "v2"
		}
		if currentSHA == "" {
			continue
		}
		latestSHA := getLatestSHAForBranch(branch)
		if latestSHA == "" || sameCommit(currentSHA, latestSHA) {
			continue
		}
		hiveCluster := s.clusterForHive(&h)
		if hiveCluster == nil {
			s.logger.Warn("auto-upgrade skipped — no cluster config", "hive_id", h.ID, "cluster_id", h.ClusterID)
			continue
		}
		// Record the day's fire BEFORE kicking the rollout. Persisting first
		// means a hub crash between here and the restart cannot cause a second
		// upgrade for the same ET day; at worst the hive waits for tomorrow's
		// window, which is the conservative direction for a "don't disturb it"
		// mode. Only daily-mode hives carry a fire date (decision.FireDate is
		// empty for instant), and a save failure is logged but not fatal — the
		// upgrade itself still proceeds.
		if decision.FireDate != "" {
			stored := loadSaaSHive(h.ID)
			if stored == nil {
				stored = &h
			}
			stored.AutoUpgradeLastFired = decision.FireDate
			if err := saveSaaSHive(stored); err != nil {
				s.logger.Warn("failed to persist auto-upgrade fire date — a hub restart today could re-fire",
					"hive_id", h.ID, "date", decision.FireDate, "error", err)
			}
		}
		s.logger.Info("audit: auto-upgrade triggered", "hive_id", h.ID, "branch", branch, "from", currentSHA, "to", latestSHA, "cluster", hiveCluster.ID, "mode", normalizeAutoUpgradeMode(h.AutoUpgradeMode))
		s.recordTimeline(h.ID, TimelineUpgradeStarted,
			fmt.Sprintf("auto-upgrade triggered on %s: %s → %s", branch, orDash(currentSHA), latestSHA), "auto-upgrade")
		s.mu.Lock()
		for i := range s.registry.Hives {
			if s.registry.Hives[i].ID == h.ID {
				s.registry.Hives[i].Upgrading = true
				s.registry.Hives[i].UpgradeTarget = latestSHA
				s.registry.Hives[i].UpgradeStartedAt = time.Now()
				break
			}
		}
		s.mu.Unlock()
		if !s.rolloutRestartHive(hiveCluster, h.ID) {
			// kubectl can't reach the cluster — fall back to heartbeat-based
			// upgrade (path 3). The next heartbeat from this hive will include
			// UpgradeTo, causing the spoke to self-restart.
			s.mu.Lock()
			s.heartbeatUpgrade[h.ID] = latestSHA
			// Keep Upgrading=true so the dashboard shows the correct state.
			s.mu.Unlock()
		}
	}
}

func fetchAllBranchSHAs(logger *slog.Logger, branches []string) {
	for _, branch := range branches {
		fetchBranchSHA(logger, branch)
	}
}

func fetchBranchSHA(logger *slog.Logger, branch string) {
	// Step 1: get the latest commit SHA on the branch from the GitHub API
	const shaFetchTimeout = 10 * time.Second
	client := &http.Client{Timeout: shaFetchTimeout}
	branchURL := fmt.Sprintf("%s/repos/kubestellar/hive/branches/%s", githubAPIBase, branch)
	req, _ := http.NewRequest("GET", branchURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("SHA poll: branch API request failed", "branch", branch, "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("SHA poll: branch API non-200", "branch", branch, "status", resp.StatusCode)
		// Backfill missing commit messages for already-cached SHAs
		backfillCommitMessage(client, branch, logger)
		return
	}
	var branchResult struct {
		Commit struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
			} `json:"commit"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&branchResult); err != nil {
		logger.Warn("SHA poll: branch decode failed", "branch", branch, "error", err)
		return
	}
	if len(branchResult.Commit.SHA) < StandardSHALen {
		logger.Warn("SHA poll: branch SHA too short", "branch", branch, "sha", branchResult.Commit.SHA)
		return
	}
	candidateSHA := shortSHA(branchResult.Commit.SHA)
	// Extract only the first line of the commit message for tooltip display.
	commitMsg := branchResult.Commit.Commit.Message
	if idx := strings.Index(commitMsg, "\n"); idx >= 0 {
		commitMsg = commitMsg[:idx]
	}

	// Step 2: verify that a container image with this SHA tag exists on GHCR.
	// The image-verified cache (latestSHAByBranch) only advances once it does;
	// the head cache advances immediately with a build-status indicator so the
	// dashboard can show the new commit while its image builds.
	prevHead := getBranchHead(branch)
	headChanged := prevHead.SHA != candidateSHA

	// The hub image is a SEPARATE build from the spoke image and can land in
	// either order (or fail independently). Probe it on its own so the hub's
	// upgrade target is never gated on the spoke build, and vice versa.
	if candidateSHA != getLatestHubSHAForBranch(branch) &&
		ghcrTagExists(client, ghcrRepoHub, candidateSHA, logger) {
		latestSHAMu.Lock()
		latestHubSHAByBranch[branch] = branchSHAInfo{SHA: candidateSHA, Message: commitMsg}
		latestSHAMu.Unlock()
		logger.Info("SHA poll: hub image verified on GHCR", "branch", branch, "sha", candidateSHA)
	}

	if candidateSHA == getLatestSHAForBranch(branch) {
		// Head unchanged since its image was verified — nothing to re-check.
		setBranchHead(branch, candidateSHA, commitMsg, imageStatusReady)
		return
	}

	if ghcrTagExists(client, ghcrRepoSpoke, candidateSHA, logger) {
		// If commit message is empty (rate-limited or missing), fetch it separately
		// from the commits API using the full SHA (one-shot, only on new SHAs).
		if commitMsg == "" {
			commitMsg = fetchCommitMessage(client, branchResult.Commit.SHA, logger)
		}
		latestSHAMu.Lock()
		latestSHAByBranch[branch] = branchSHAInfo{SHA: candidateSHA, Message: commitMsg}
		commitMsgBySHA[candidateSHA] = commitMsg
		latestSHAMu.Unlock()
		setBranchHead(branch, candidateSHA, commitMsg, imageStatusReady)
		logger.Info("SHA poll: latest image verified on GHCR", "branch", branch, "sha", candidateSHA)
		return
	}

	// Image not on GHCR yet — ask the docker workflow whether the build for
	// this head commit is still running or has failed.
	status := fetchImageBuildStatus(client, branchResult.Commit.SHA, logger)
	if status == "" {
		// Actions API unavailable (rate-limited/network): keep the last-known
		// status for this head; a brand-new head with no image is presumed
		// building. Never invent "failed" from an API error.
		status = prevHead.ImageStatus
		if headChanged || status == "" || status == imageStatusReady {
			status = imageStatusBuilding
		}
	}
	if commitMsg == "" && headChanged {
		commitMsg = fetchCommitMessage(client, branchResult.Commit.SHA, logger)
	}
	setBranchHead(branch, candidateSHA, commitMsg, status)
	logger.Info("SHA poll: container image not yet on GHCR", "branch", branch, "sha", candidateSHA, "image_status", status)
}

// dockerWorkflowFile is the workflow that builds and pushes the container
// images (ghcr.io/kubestellar/hive:<branch>-latest and :<short-sha>) on
// every push to a tracked branch.
const dockerWorkflowFile = "docker.yml"

// fetchImageBuildStatus queries the docker workflow run for a specific head
// commit and maps it to an image build status. Returns "" when the API is
// unavailable so the caller can keep the last-known status instead of
// flapping ready/building on transient errors.
func fetchImageBuildStatus(client *http.Client, fullSHA string, logger *slog.Logger) string {
	runsURL := fmt.Sprintf("%s/repos/kubestellar/hive/actions/workflows/%s/runs?head_sha=%s&per_page=1", githubAPIBase, dockerWorkflowFile, fullSHA)
	req, _ := http.NewRequest("GET", runsURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("SHA poll: workflow runs request failed", "sha", fullSHA, "error", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("SHA poll: workflow runs non-200", "sha", fullSHA, "status", resp.StatusCode)
		return ""
	}
	var result struct {
		WorkflowRuns []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"workflow_runs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		logger.Warn("SHA poll: workflow runs decode failed", "sha", fullSHA, "error", err)
		return ""
	}
	if len(result.WorkflowRuns) == 0 {
		// The push event may not have spawned the workflow run yet.
		return imageStatusBuilding
	}
	run := result.WorkflowRuns[0]
	if run.Status != "completed" {
		return imageStatusBuilding // queued, in_progress, waiting, pending
	}
	if run.Conclusion == "success" {
		// Workflow finished but the manifest isn't visible on GHCR yet —
		// treat as still publishing; the GHCR check flips it to ready.
		return imageStatusBuilding
	}
	return imageStatusFailed // failure, cancelled, timed_out, startup_failure
}

// fetchCommitMessage fetches the first line of a commit message from the GitHub API.
// Uses a separate endpoint that's less likely to be rate-limited since it's called
// only once per new SHA (not every poll cycle).
func fetchCommitMessage(client *http.Client, fullSHA string, logger *slog.Logger) string {
	commitURL := fmt.Sprintf("%s/repos/kubestellar/hive/commits/%s", githubAPIBase, fullSHA)
	req, _ := http.NewRequest("GET", commitURL, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Warn("SHA poll: commit message fetch non-200", "sha", fullSHA[:7], "status", resp.StatusCode)
		return ""
	}
	var result struct {
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	msg := result.Commit.Message
	if idx := strings.Index(msg, "\n"); idx >= 0 {
		msg = msg[:idx]
	}
	return msg
}

// backfillCommitMessage fills in a missing commit message for an already-cached SHA.
// Called when the branches API is rate-limited but we have the SHA from a prior poll.
func backfillCommitMessage(client *http.Client, branch string, logger *slog.Logger) {
	latestSHAMu.RLock()
	info := latestSHAByBranch[branch]
	latestSHAMu.RUnlock()
	if info.SHA == "" || info.Message != "" {
		return // no SHA cached, or message already present
	}
	msg := fetchCommitMessage(client, info.SHA, logger)
	if msg == "" {
		return
	}
	latestSHAMu.Lock()
	info.Message = msg
	latestSHAByBranch[branch] = info
	commitMsgBySHA[info.SHA] = msg
	latestSHAMu.Unlock()
	logger.Info("SHA poll: backfilled commit message", "branch", branch, "sha", info.SHA, "message", msg)
}

// ghcrTagExists checks whether a container tag exists on ghcr.io/<repo> (e.g.
// ghcrRepoSpoke or ghcrRepoHub). Uses an anonymous token (public package) and a
// HEAD on the manifest endpoint. The repo is a parameter because the hub and the
// spoke are DIFFERENT images built by separate jobs — verifying the wrong one
// lets the hub target a SHA whose own image was never published.
func ghcrTagExists(client *http.Client, repo, tag string, logger *slog.Logger) bool {
	// Get anonymous pull token
	tokenResp, err := client.Get(ghcrBase + "/token?scope=repository:" + repo + ":pull")
	if err != nil {
		logger.Warn("SHA poll: GHCR token request failed", "repo", repo, "error", err)
		return false
	}
	defer tokenResp.Body.Close()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		return false
	}

	manifestURL := fmt.Sprintf("%s/v2/%s/manifests/%s", ghcrBase, repo, tag)
	req, _ := http.NewRequest("HEAD", manifestURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *HubServer) handleProxyHiveConfig(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("hiveID")
	s.mu.RLock()
	var dashURL string
	for _, h := range s.registry.Hives {
		if h.ID == hiveID && h.DashboardURL != "" {
			dashURL = h.DashboardURL
			break
		}
	}
	s.mu.RUnlock()
	if dashURL == "" {
		http.Error(w, `{"error":"hive not found or no dashboard URL"}`, http.StatusNotFound)
		return
	}
	const proxyConfigTimeout = 10 * time.Second
	const maxConfigResponseBytes = 1 << 20
	client := &http.Client{Timeout: proxyConfigTimeout}
	resp, err := client.Get(strings.TrimRight(dashURL, "/") + "/api/config/download")
	if err != nil {
		slog.Warn("hive config proxy failed", "hiveID", hiveID, "error", err)
		http.Error(w, `{"error":"could not reach hive"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxConfigResponseBytes))
	w.Header().Set("Content-Type", "application/x-yaml")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func (s *HubServer) handleLatestSHA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"sha": getLatestSHA()})
}

// authorizedUsersForHiveID returns the hub's access list for a hive as
// "username:role" entries, for delivery to the spoke in its heartbeat response.
// Returns nil (not an empty slice) when the hive has no SaaS record, so a
// non-hosted spoke's own allowlist is left untouched. The owner is always
// included as owner even if no explicit access record names them.
func authorizedUsersForHiveID(hiveID string) []string {
	h := loadSaaSHive(hiveID)
	if h == nil {
		return nil
	}
	out := make([]string, 0, 4)
	seen := map[string]bool{}
	if h.Owner != "" {
		out = append(out, h.Owner+":owner")
		seen[strings.ToLower(h.Owner)] = true
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
	}
	return out
}

// HiveAccessEntry is one user's access to a hive.
type HiveAccessEntry struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

// accessForHive returns who can sign in to a hive, newest-role-agnostic and
// sorted for a stable render. Access is derived by scanning user records rather
// than reading h.Owner: a user's Hives map is the authoritative grant (see
// handleApproveProvision / handleAssignHive, which write it), and an owner who
// is missing from it genuinely cannot sign in.
func accessForHive(hiveID string, users []SaaSUser) []HiveAccessEntry {
	access := make([]HiveAccessEntry, 0)
	for _, u := range users {
		if role, ok := u.Hives[hiveID]; ok {
			access = append(access, HiveAccessEntry{Username: u.GitHubUsername, Role: role})
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

func (s *HubServer) handleAccessList(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if h.Owner != username && username != hubAdminUsername {
		http.Error(w, `{"error":"only the owner can view access"}`, http.StatusForbidden)
		return
	}
	access := accessForHive(hiveID, listAllSaaSUsers())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"access": access})
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
	owns := username == hubAdminUsername
	if !owns {
		for _, h := range listSaaSHives() {
			if h.Owner == username {
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
	for _, u := range listAllSaaSUsers() {
		if u.GitHubUsername != "" {
			names = append(names, u.GitHubUsername)
		}
	}
	sort.Strings(names)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"users": names})
}

func (s *HubServer) handleAccessAdd(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if h.Owner != username && username != hubAdminUsername {
		http.Error(w, `{"error":"only the owner can manage access"}`, http.StatusForbidden)
		return
	}
	var body struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Username == "" || body.Role == "" {
		http.Error(w, `{"error":"username and role required"}`, http.StatusBadRequest)
		return
	}
	if body.Role != "read" && body.Role != "read-write" && body.Role != "owner" {
		http.Error(w, `{"error":"role must be read, read-write, or owner"}`, http.StatusBadRequest)
		return
	}
	target := ensureSaaSUser(body.Username)
	target.Hives[hiveID] = body.Role
	saveSaaSUser(target)
	s.logger.Info("audit: access granted", "hive", hiveID, "target", body.Username, "role", body.Role, "by", username)
	s.recordTimeline(hiveID, TimelineAccess,
		fmt.Sprintf("access granted to %s as %s", body.Username, body.Role), username)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "granted"})
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
	if h.Owner != username && username != hubAdminUsername {
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
			http.Error(w, `{"error":"cannot remove the last owner"}`, http.StatusBadRequest)
			return
		}
	}
	delete(target.Hives, hiveID)
	saveSaaSUser(target)
	s.logger.Info("audit: access revoked", "hive", hiveID, "target", targetUsername, "by", username)
	s.recordTimeline(hiveID, TimelineAccess,
		fmt.Sprintf("access revoked from %s", targetUsername), username)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "revoked"})
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
	json.Unmarshal(data, &reqs)
	return reqs
}

func saveAccessRequests(hiveID string, reqs []AccessRequest) {
	if strings.Contains(hiveID, "..") || strings.Contains(hiveID, "/") || strings.Contains(hiveID, "\\") {
		slog.Warn("saveAccessRequests: invalid hiveID", "hiveID", hiveID)
		return
	}
	dir := filepath.Join(saasHivesDir, hiveID)
	os.MkdirAll(dir, 0o755)
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
	json.NewDecoder(r.Body).Decode(&body)
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

	s.logger.Info("audit: access requested", "hive", hiveID, "by", username)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "requested"})
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
	if role != "owner" && role != "read-write" && username != hubAdminUsername {
		http.Error(w, `{"error":"need owner or read-write access"}`, http.StatusForbidden)
		return
	}

	reqs := loadAccessRequests(hiveID)
	pending := make([]AccessRequest, 0)
	for _, req := range reqs {
		if req.Status == "pending" {
			pending = append(pending, req)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"requests": pending})
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
	if approverRole != "owner" && approverRole != "read-write" && approver != hubAdminUsername {
		http.Error(w, `{"error":"need owner or read-write access"}`, http.StatusForbidden)
		return
	}

	var body struct {
		Role string `json:"role"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if body.Role == "" {
		body.Role = "read"
	}

	roleRank := map[string]int{"read": 1, "read-write": 2, "owner": 3}
	if approver != hubAdminUsername && roleRank[body.Role] >= roleRank[approverRole] {
		http.Error(w, `{"error":"cannot grant a role equal to or higher than your own"}`, http.StatusForbidden)
		return
	}

	target := ensureSaaSUser(targetUsername)
	target.Hives[hiveID] = body.Role
	saveSaaSUser(target)

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
	json.NewEncoder(w).Encode(map[string]string{"status": "approved"})
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
	if denierRole != "owner" && denierRole != "read-write" && denier != hubAdminUsername {
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
	json.NewEncoder(w).Encode(map[string]string{"status": "denied"})
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
	if approverRole != "owner" && approver != hubAdminUsername {
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
	target.Hives[hiveID] = defaultApproveRole
	saveSaaSUser(target)

	s.logger.Info("audit: access approved via PUT", "hive", hiveID, "target", targetUsername, "by", approver)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
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
	if denierRole != "owner" && denier != hubAdminUsername {
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
	w.Write([]byte(`{"ok":true}`))
}

var provisionRequestsDir = "/data/saas/provision-requests"

const (
	provisionStatusPending  = "pending"
	provisionStatusApproved = "approved"
	provisionStatusDenied   = "denied"
)

const maxProvisionRequestBodyBytes = 4 * 1024

type ProvisionRequest struct {
	Username string `json:"username"`
	// GitHubHost is the GitHub instance the org lives on — empty means public
	// github.com, otherwise a GitHub Enterprise host (github.ibm.com,
	// github.cisco.com, …). Captured so an admin can see which instance a
	// request targets before deciding where to place the hive.
	GitHubHost  string `json:"github_host,omitempty"`
	Org         string `json:"org"`
	Repos       string `json:"repos"`
	PrimaryRepo string `json:"primary_repo"`
	ACMMLevel   int    `json:"acmm_level"`
	AuthMethod  string `json:"auth_method"`
	RequestedAt string `json:"requested_at"`
	Status      string `json:"status"`

	// Who is asking, as a person rather than as a GitHub login. Both reuse the
	// SaaSUser contact fields of the same name and the same caps
	// (maxContactNameLen / maxContactSlackIDLen) — deliberately NOT new keys.
	// A second Slack key would split one fact across two names, with the
	// request form writing one and the admin users panel reading the other.
	//
	// These are captured here and copied onto the SaaSUser record on approval
	// (handleApproveProvision); asking and then dropping the answer would be
	// worse than not asking. Both are omitempty so requests filed before these
	// fields existed round-trip unchanged.
	FullName string `json:"full_name,omitempty"`
	SlackID  string `json:"slack_id,omitempty"`

	// Decision audit. Previously a request only carried its final Status, so
	// once it left "pending" there was no record of WHO decided, WHEN, or —
	// for an approval — which hive the requester actually got. That made the
	// history unauditable: an approved request and a denied one looked equally
	// anonymous. Empty on records decided before these fields existed.
	DecidedBy    string `json:"decided_by,omitempty"`
	DecidedAt    string `json:"decided_at,omitempty"`
	AssignedHive string `json:"assigned_hive,omitempty"`
	// DenyReason is the optional free-text explanation shown back to the
	// requester when a request is turned down.
	DenyReason string `json:"deny_reason,omitempty"`

	// --- Derived, never persisted ---
	// These are filled in on read (see enrichProvisionRequests) so the admin
	// Past Requests table can show the requester's role on the hive they were
	// given, plus the rest of their fleet, without one API call per row. They
	// are omitempty so records written before they existed stay valid and so a
	// round-trip through saveProvisionRequest never bakes stale roles onto disk.
	//
	// AssignedRole is the requester's role on AssignedHive, read from their
	// SaaSUser.Hives map — the authoritative grant (see accessForHive). Empty
	// when the request was denied, when no hive was assigned, or when the grant
	// was since revoked.
	AssignedRole string `json:"assigned_role,omitempty"`
	// OtherHives is every OTHER hive the requester can sign in to, with their
	// role on each — the person's footprint beyond this one request. Sorted
	// owners-first then by hive ID for a stable render.
	OtherHives []UserHiveRole `json:"other_hives,omitempty"`
}

// roleOwner is the role string stored in SaaSUser.Hives for the hive's owner.
// Named so the owners-first sort below does not repeat a bare literal.
const roleOwner = "owner"

// UserHiveRole is one hive a user can sign in to and the role they hold on it.
// The mirror image of HiveAccessEntry: that answers "who is on this hive", this
// answers "which hives is this user on".
type UserHiveRole struct {
	HiveID string `json:"hive_id"`
	Role   string `json:"role"`
}

// hivesForUser returns every hive the named user can sign in to, with their
// role on each, optionally excluding one hive ID (the one already shown in its
// own column). Access comes from SaaSUser.Hives — the authoritative grant that
// handleApproveProvision / handleAssignHive write — not from hive.Owner, which
// can name someone whose grant was revoked.
//
// users is passed in rather than read here so a caller enriching many rows can
// read the roster ONCE: listAllSaaSUsers hits the filesystem per user record.
func hivesForUser(username string, excludeHiveID string, users []SaaSUser) []UserHiveRole {
	if username == "" {
		return nil
	}
	out := make([]UserHiveRole, 0)
	for _, u := range users {
		if !strings.EqualFold(u.GitHubUsername, username) {
			continue
		}
		for id, role := range u.Hives {
			if id == "" || id == excludeHiveID {
				continue
			}
			out = append(out, UserHiveRole{HiveID: id, Role: role})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		// Owned hives first — the useful line to read when scanning someone's
		// footprint — then alphabetical by hive ID for a stable order.
		if (out[i].Role == roleOwner) != (out[j].Role == roleOwner) {
			return out[i].Role == roleOwner
		}
		return out[i].HiveID < out[j].HiveID
	})
	if len(out) == 0 {
		return nil // omitempty: no cell rather than an empty array
	}
	return out
}

// roleForUserOnHive returns the named user's role on the named hive, or "" if
// they hold no grant on it. Same roster-passed-in contract as hivesForUser.
func roleForUserOnHive(username, hiveID string, users []SaaSUser) string {
	if username == "" || hiveID == "" {
		return ""
	}
	for _, u := range users {
		if !strings.EqualFold(u.GitHubUsername, username) {
			continue
		}
		if role, ok := u.Hives[hiveID]; ok {
			return role
		}
	}
	return ""
}

// enrichProvisionRequests fills in the derived AssignedRole / OtherHives fields
// on every request in place.
//
// ADMIN-ONLY: this exposes other people's hive memberships, so it must only be
// called on a payload already gated behind requireAdmin (or the isAdmin branch
// of the dashboard handler). Do not call it on a per-user response.
//
// The roster is read once here — O(1) filesystem sweeps for the whole table
// rather than O(rows) — because listAllSaaSUsers walks and unmarshals every
// user record on disk.
func enrichProvisionRequests(requests []ProvisionRequest) []ProvisionRequest {
	if len(requests) == 0 {
		return requests
	}
	users := listAllSaaSUsers()
	for i := range requests {
		requests[i].AssignedRole = roleForUserOnHive(requests[i].Username, requests[i].AssignedHive, users)
		requests[i].OtherHives = hivesForUser(requests[i].Username, requests[i].AssignedHive, users)
	}
	return requests
}

// applyRequestContactToUser carries the requester's contact details from an
// approved provision request onto their SaaSUser record.
//
// This is what makes asking for them worth anything. The admin users panel and
// the Slack sender both read SaaSUser, NOT ProvisionRequest — a value that
// stops at the request file is invisible to every consumer of it. Slack
// messaging shipped with no user having a slack_id, settable only by hand one
// user at a time; approval is the natural point of capture.
//
// It only ever FILLS A BLANK. An admin who has already curated these fields (or
// a user who corrected them later) outranks whatever was typed into a request
// form, and a re-approval must not silently revert that.
//
// Nil-safe on both sides: it runs on the approval path beside other work that
// can legitimately leave either side absent, and must not panic there.
func applyRequestContactToUser(user *SaaSUser, pr *ProvisionRequest) {
	if user == nil || pr == nil {
		return
	}
	if user.FullName == "" && pr.FullName != "" {
		user.FullName = truncateRunes(strings.TrimSpace(pr.FullName), maxContactNameLen)
	}
	if user.SlackID == "" && pr.SlackID != "" {
		user.SlackID = truncateRunes(strings.TrimSpace(pr.SlackID), maxContactSlackIDLen)
	}
}

func loadProvisionRequest(username string) *ProvisionRequest {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return nil
	}
	path := filepath.Join(provisionRequestsDir, username+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pr ProvisionRequest
	if json.Unmarshal(data, &pr) != nil {
		return nil
	}
	return &pr
}

func saveProvisionRequest(pr *ProvisionRequest) error {
	os.MkdirAll(provisionRequestsDir, 0o755)
	data, err := json.MarshalIndent(pr, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(provisionRequestsDir, pr.Username+".json")
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func deleteProvisionRequest(username string) {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return
	}
	os.Remove(filepath.Join(provisionRequestsDir, username+".json"))
}

func listProvisionRequests() []ProvisionRequest {
	os.MkdirAll(provisionRequestsDir, 0o755)
	entries, err := os.ReadDir(provisionRequestsDir)
	if err != nil {
		return nil
	}
	var result []ProvisionRequest
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		uname := strings.TrimSuffix(e.Name(), ".json")
		pr := loadProvisionRequest(uname)
		// Return decided requests too, not just pending ones — the admin view
		// splits them into a pending action queue and a decision-history table,
		// and filtering here made the history permanently empty.
		if pr != nil {
			result = append(result, *pr)
		}
	}
	return result
}

func (s *HubServer) handleRequestProvision(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	existing := loadProvisionRequest(username)
	if existing != nil && existing.Status == provisionStatusPending {
		http.Error(w, `{"error":"you already have a pending provision request"}`, http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxProvisionRequestBodyBytes)
	var body struct {
		Org         string `json:"org"`
		GitHubHost  string `json:"github_host"`
		Repos       string `json:"repos"`
		PrimaryRepo string `json:"primary_repo"`
		ACMMLevel   int    `json:"acmm_level"`
		AuthMethod  string `json:"auth_method"`
		FullName    string `json:"full_name"`
		SlackID     string `json:"slack_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Org == "" || body.Repos == "" {
		http.Error(w, `{"error":"org and repos are required"}`, http.StatusBadRequest)
		return
	}
	// Contact details for the person asking.
	//
	// No charset validation on purpose: isValidName() is for identifiers
	// (org/repo/host names) and would reject spaces, apostrophes and every
	// non-ASCII script. These are display strings — trimmed, rune-capped and
	// escaped at every render site, exactly as the admin contact editor
	// (handleAdminUpdateUser) already treats these same two fields. We also do
	// NOT try to detect a "real" name: any pattern for that (two words,
	// capitalised, Latin script) is wrong for a large fraction of the world's
	// names, and a determined user types junk regardless. Human review is the
	// check, not a regex.
	body.FullName = truncateRunes(strings.TrimSpace(body.FullName), maxContactNameLen)
	body.SlackID = truncateRunes(strings.TrimSpace(body.SlackID), maxContactSlackIDLen)
	// Accept a pasted org/repo URL, not just a bare name. Users read
	// "GitHub Organization" and paste the org's URL; the old validator rejected
	// ":" and "/" and returned a bare "invalid org name" that explained nothing.
	// The host is kept so the admin can see whether a request is for github.com
	// or a GitHub Enterprise instance before placing the hive.
	ghHost, orgName := normalizeOrgRef(body.Org)
	body.Org = orgName
	if ghHost != "" {
		body.GitHubHost = ghHost
	}
	// The forge is REQUIRED. A bare org name ("z-innersource") does not say
	// whether the org lives on github.com or on a GitHub Enterprise instance,
	// and the hub cannot guess: the wrong choice provisions the hive against
	// the wrong GitHub and points the App-install link at a forge the org
	// admin never sees. Normalize FIRST — the field accepts
	// "https://github.ibm.com/" and isValidName rejects ":" and "/", so
	// validating the raw value would reject a form the form itself advertises.
	forgeHost, ok := normalizeForgeHost(body.GitHubHost)
	if !ok {
		http.Error(w, `{"error":"a GitHub forge is required — enter the host your org lives on (e.g. github.com or github.ibm.com); without it we cannot tell which GitHub to provision against"}`, http.StatusBadRequest)
		return
	}
	body.GitHubHost = forgeHost
	// Single-host-per-spoke: every repo (and the primary) must be on the same
	// GitHub host as the org. Check BEFORE normalizeRepoRef strips the host off
	// each pasted repo. Reject a mixed request up front with a clear message —
	// a spoke that mixed github.com and a GHE instance would silently fail to
	// authenticate against half its repos, the onboarding footgun this removes.
	if err := validateSingleRepoHost(body.GitHubHost, body.PrimaryRepo, strings.Split(body.Repos, ",")); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
		return
	}
	{
		var cleaned []string
		for _, repo := range strings.Split(body.Repos, ",") {
			if r := normalizeRepoRef(repo); r != "" {
				cleaned = append(cleaned, r)
			}
		}
		body.Repos = strings.Join(cleaned, ",")
		body.PrimaryRepo = normalizeRepoRef(body.PrimaryRepo)
	}
	if !isValidName(body.Org) {
		http.Error(w, fmt.Sprintf(`{"error":"invalid org name %q — use the org name or its URL (e.g. github.ibm.com/my-org)"}`, body.Org), http.StatusBadRequest)
		return
	}
	// Defence in depth: normalizeForgeHost above already guaranteed this, but
	// keep the check so a future edit that reorders the handler cannot let an
	// unvalidated host reach the stored request.
	if !isValidName(body.GitHubHost) {
		http.Error(w, `{"error":"invalid github host"}`, http.StatusBadRequest)
		return
	}
	for _, repo := range strings.Split(body.Repos, ",") {
		if !isValidRepoRef(strings.TrimSpace(repo)) {
			http.Error(w, `{"error":"invalid repo name"}`, http.StatusBadRequest)
			return
		}
	}

	// Name is REQUIRED; Slack ID is optional.
	//
	// The name exists to map a GitHub login to a person an operator can talk
	// to, which only works if it is actually populated — a field that is
	// usually blank cannot be relied on and stops being read. Requesting a hive
	// is a deliberate, one-per-user action already reviewed by a human, so one
	// short field is negligible friction at the moment the requester is most
	// motivated to answer.
	//
	// Checked AFTER the org/forge/repo validation above so the more specific
	// "which GitHub is this org on?" errors keep their precedence — a request
	// missing both should be told about the forge, not sent round the loop one
	// field at a time.
	//
	// This does tighten an existing endpoint: a caller that posted no full_name
	// now gets a 400 where it used to get a 200. That is intended — the whole
	// point is that the field is reliably present.
	//
	// The get-started wizard (static/get-started.html) is now the only in-tree
	// caller, but do not read that as "it always was". When this check landed
	// (#2369) this comment claimed the wizard was the only caller and it was
	// simply wrong: the hub dashboard had its own Request-a-Hive modal posting
	// here, added later than the wizard, reachable by every logged-in user, and
	// it went un-updated — so the button 400'd with no field on screen that
	// could satisfy it. The modal has since been removed deliberately (the
	// wizard is the single supported request path), which is what makes this
	// sentence true today rather than aspirational.
	//
	// The modal was invisible to CI because it was inline JS inside the
	// dashboardHTML raw string with no test naming any of its symbols. Before
	// adding another required field here, re-run the caller audit rather than
	// trusting this comment: TestRequestProvisionInTreeCallersSendRequiredFields
	// greps the embedded JS and the static wizard for callers of this endpoint
	// and fails on one that omits a required field.
	if body.FullName == "" {
		http.Error(w, `{"error":"your name is required — we use it to know who the request is from"}`, http.StatusBadRequest)
		return
	}

	// A hive is never REQUESTED above L3 Measured. L4-L6 are real levels, but
	// they are reached after provisioning, from the hive's own dashboard, once
	// the project has the coverage and CI history to justify them. The
	// get-started wizard only offers L1-L3, but the wizard is one client: clamp
	// here so a crafted request cannot provision straight into auto-merge.
	// Admin paths (assign/provision) keep the full 0..6 range on purpose — an
	// operator setting a level deliberately is not the case being guarded.
	acmm := body.ACMMLevel
	if acmm < minRequestACMMLevel || acmm > maxRequestACMMLevel {
		acmm = minRequestACMMLevel
	}

	primaryRepo := body.PrimaryRepo
	if primaryRepo == "" {
		repos := strings.Split(body.Repos, ",")
		if len(repos) > 0 {
			primaryRepo = strings.TrimSpace(repos[0])
		}
	}

	pr := &ProvisionRequest{
		Username:    username,
		GitHubHost:  body.GitHubHost,
		Org:         body.Org,
		Repos:       body.Repos,
		PrimaryRepo: primaryRepo,
		ACMMLevel:   acmm,
		AuthMethod:  body.AuthMethod,
		FullName:    body.FullName,
		SlackID:     body.SlackID,
		RequestedAt: time.Now().UTC().Format(time.RFC3339),
		Status:      provisionStatusPending,
	}
	if err := saveProvisionRequest(pr); err != nil {
		http.Error(w, `{"error":"failed to save provision request"}`, http.StatusInternalServerError)
		return
	}

	s.logger.Info("audit: provision request created", "user", username, "org", body.Org, "repos", body.Repos)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": provisionStatusPending})
}

// ApproveProvisionRequest is the OPTIONAL body of PUT
// /api/saas/approve-provision/{username}. HiveID lets the admin pick the exact
// available placeholder to assign (from the approve-picker modal); an empty or
// absent HiveID preserves the historical auto-pick behavior.
type ApproveProvisionRequest struct {
	HiveID string `json:"hive_id"`
	// GitHubHost is the admin's explicit choice of GitHub instance for this
	// hive, overriding the one on the provision request. "public" forces public
	// github.com even on a GitHub Enterprise cluster; empty means "use the
	// request's host, else the cluster default".
	GitHubHost string `json:"github_host,omitempty"`
}

func (s *HubServer) handleApproveProvision(w http.ResponseWriter, r *http.Request) {
	targetUsername := r.PathValue("username")
	approver := s.getAuthUser(r)

	pr := loadProvisionRequest(targetUsername)
	if pr == nil || pr.Status != provisionStatusPending {
		http.Error(w, `{"error":"no pending provision request for this user"}`, http.StatusNotFound)
		return
	}

	// Optionally the admin picks the EXACT placeholder to assign (from the
	// approve-picker modal) instead of letting the hub auto-pick. The body is
	// tolerated as absent/empty — an empty hive_id preserves the historical
	// auto-pick behavior. The body is tiny (a single id), so cap it small.
	const maxApproveRequestBodyBytes = 1 * 1024
	var approveBody ApproveProvisionRequest
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxApproveRequestBodyBytes)
		// An empty body is valid (auto-pick); ignore EOF/empty decode errors and
		// fall through to auto-pick. Any non-empty malformed body is rejected.
		if err := json.NewDecoder(r.Body).Decode(&approveBody); err != nil && err != io.EOF {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}

	// Approving now ASSIGNS an available placeholder instead of just bumping
	// quota for a manual provision. Pick the pool from the request's auth_method
	// (private → vllm-d/GPU pool, otherwise → hive-oke/public pool). If no
	// placeholder is available in that pool, tell the admin to provision more.
	pool := poolClusterForAuthMethod(pr.AuthMethod)
	hiveID := strings.TrimSpace(approveBody.HiveID)
	if hiveID != "" {
		// Admin chose a specific placeholder — validate it is an available
		// placeholder (same check the assign path uses) before using it. The
		// full status recheck under loadSaaSHive below still guards the race.
		if sel := loadSaaSHive(hiveID); sel == nil || sel.Status != statusAvailable || sel.Owner != hubAdminUsername {
			http.Error(w, `{"error":"selected placeholder is not available"}`, http.StatusConflict)
			return
		}
	} else {
		hiveID = findAvailablePlaceholder(pool)
	}
	if hiveID == "" {
		http.Error(w, fmt.Sprintf(`{"error":"no available placeholder hive in pool %q — provision more placeholders"}`, pool), http.StatusConflict)
		return
	}

	h := loadSaaSHive(hiveID)
	if h == nil || h.Status != statusAvailable {
		// Raced with another assignment between selection and load.
		http.Error(w, `{"error":"selected placeholder became unavailable — retry"}`, http.StatusConflict)
		return
	}

	// Reuse the request's org/repos/primary_repo/acmm as the assignment inputs.
	var repos []string
	for _, repo := range strings.Split(pr.Repos, ",") {
		if repo = strings.TrimSpace(repo); repo != "" {
			repos = append(repos, repo)
		}
	}
	primaryRepo := pr.PrimaryRepo
	if primaryRepo == "" && len(repos) > 0 {
		primaryRepo = repos[0]
	}
	acmm := pr.ACMMLevel
	if acmm == 0 {
		acmm = defaultAssignACMMLevel
	}
	if acmm < minAssignACMMLevel || acmm > maxAssignACMMLevel {
		acmm = defaultAssignACMMLevel
	}

	// Rewrite the placeholder's meta.json to the requesting user's real project.
	// Clearing status makes it show under the new owner in My Hives; the project
	// config reaches the spoke via the heartbeat channel (projectConfigForHiveID).
	h.Owner = targetUsername
	h.Org = pr.Org
	h.Repos = repos
	h.PrimaryRepo = primaryRepo
	h.ACMMLevel = acmm
	// Record the level as REQUESTED, not merely as current. ACMMLevel is
	// overwritten the moment the spoke reports the level it was minted at, so it
	// cannot be the source of truth for "what did the owner ask for". Keeping the
	// requested value in its own field is what lets the delivery reconcile below
	// remain correct — and idempotent — across any number of heartbeats.
	h.RequestedACMMLevel = acmm
	// Re-arm the level handshake for this claim. A placeholder is reused across
	// assignments, so a flag left true by a previous tenancy would suppress
	// delivery for the new owner.
	h.ACMMDelivered = false
	h.Status = ""
	h.Error = ""
	// Preserve the placeholder's real cluster before ANY cluster-derived
	// resolution below (host backfill uses s.clusterForHive(h), which silently
	// returns hive-oke when ClusterID is blank). The placeholder was picked
	// from `pool`, so a blank cluster_id here can only mean the placeholder was
	// created without one — stamp the pool it came from rather than leaving a
	// blank that later resolves to the wrong (default) cluster's App/host.
	s.ensureClusterIDForClaim(h, pool)
	// The requester already told us which GitHub their org lives on (parsed
	// from the org URL they pasted, or picked explicitly), so honour it. Before
	// this, approve dropped github_host entirely — only the manual assign path
	// ever set it — and a GHE request approved through this path produced a
	// hive with a blank host talking to api.github.com. An override the admin
	// chose in the approve modal arrives on the request body below and wins.
	if host := strings.TrimSpace(approveBody.GitHubHost); host != "" {
		if !isValidName(host) && !strings.EqualFold(host, githubHostPublic) {
			http.Error(w, `{"error":"invalid github host"}`, http.StatusBadRequest)
			return
		}
		// An explicit "public" choice means public github.com. Record it as a
		// blank host so gheAPIURLForHost pushes nothing and the spoke keeps its
		// own api.github.com default — and so the cluster backfill below, which
		// only ever fills a blank, does not silently re-GHE it.
		if strings.EqualFold(host, githubHostPublic) {
			h.GitHubHost = ""
			h.GitHubBaseURL = githubHostPublic
		} else {
			h.GitHubHost = host
		}
	} else if pr.GitHubHost != "" {
		// The request itself named a host. Honour a "public" sentinel here the SAME
		// way the admin override does: the self-service onboarding form now sends
		// "public" for an explicit github.com choice (never a blank), so a
		// github.com request must be pinned public — NOT stored as the literal host
		// "public", and NOT left blank for the cluster backfill below to re-GHE.
		if strings.EqualFold(pr.GitHubHost, githubHostPublic) {
			h.GitHubHost = ""
			h.GitHubBaseURL = githubHostPublic
		} else {
			h.GitHubHost = pr.GitHubHost
		}
	}
	// Same cluster backfill the manual assign path does: when neither the admin
	// nor the request named a host, inherit the cluster's GHE default rather
	// than leaving a blank that pushes an empty API URL forever.
	if host := backfillGitHubHostFromCluster(h, s.clusterForHive(h)); host != "" {
		h.GitHubHost = host
		s.logger.Info("backfilled hive github host from cluster defaults",
			"hive", hiveID, "github_host", host)
	}
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to assign placeholder hive"}`, http.StatusInternalServerError)
		return
	}

	// Ensure the user record exists, grant them owner access, and count this
	// owned hive against a quota.
	//
	// Granting Hives[hiveID] is what actually puts the requester on the hive's
	// permissions: handleAccessList builds the access list by scanning every
	// user record for Hives[hiveID], NOT from h.Owner. Without this the
	// assignment set h.Owner correctly but the new owner never appeared under
	// Manage Access — only the admin who provisioned the placeholder did — and
	// on a heartbeat-only cluster (vllm-d) that stale list is what gets
	// delivered to the spoke.
	user := loadSaaSUser(targetUsername)
	if user == nil {
		user = ensureSaaSUser(targetUsername)
	}
	if user.Hives == nil {
		user.Hives = map[string]string{}
	}
	user.Hives[hiveID] = "owner"
	user.SaaSQuota++
	applyRequestContactToUser(user, pr)
	if err := saveSaaSUser(user); err != nil {
		s.logger.Warn("assigned placeholder but failed to grant owner access", "user", targetUsername, "hive", hiveID, "error", err)
	}

	// Mark the request fulfilled, recording who approved it and which hive the
	// requester actually received — that pairing is the whole point of the
	// history table, and it is unrecoverable after the fact if not stored now.
	pr.Status = provisionStatusApproved
	pr.DecidedBy = approver
	pr.DecidedAt = time.Now().UTC().Format(time.RFC3339)
	pr.AssignedHive = hiveID
	if err := saveProvisionRequest(pr); err != nil {
		s.logger.Warn("assigned placeholder but failed to update provision request", "user", targetUsername, "error", err)
	}

	s.logger.Info("audit: provision request approved via placeholder assignment",
		"target", targetUsername, "by", approver, "org", pr.Org, "repos", pr.Repos,
		"hive_id", hiveID, "cluster", clusterIDForHive(h))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"status":  provisionStatusApproved,
		"hive_id": hiveID,
	})
}

func (s *HubServer) handleDenyProvision(w http.ResponseWriter, r *http.Request) {
	targetUsername := r.PathValue("username")
	denier := s.getAuthUser(r)

	pr := loadProvisionRequest(targetUsername)
	if pr == nil || pr.Status != provisionStatusPending {
		http.Error(w, `{"error":"no pending provision request for this user"}`, http.StatusNotFound)
		return
	}

	// Retain the record instead of deleting it. Deleting made a denial
	// indistinguishable from a request that was never made — the history table
	// could only ever show approvals, and an admin had no way to answer "did we
	// already turn this person down, and why?". The retained record is what
	// makes a denial re-requestable-but-accountable.
	const maxDenyRequestBodyBytes = 1 * 1024
	var denyBody struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, maxDenyRequestBodyBytes)
		_ = json.NewDecoder(r.Body).Decode(&denyBody) // absent body is fine
	}
	pr.Status = provisionStatusDenied
	pr.DecidedBy = denier
	pr.DecidedAt = time.Now().UTC().Format(time.RFC3339)
	pr.DenyReason = strings.TrimSpace(denyBody.Reason)
	if err := saveProvisionRequest(pr); err != nil {
		s.logger.Warn("failed to record provision denial", "target", targetUsername, "error", err)
	}

	s.logger.Info("audit: provision request denied", "target", targetUsername, "by", denier)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// authMethodPrivate is the request auth_method value that routes a provision
// request to the private/GPU placeholder pool (vllm-d). Any other value (the
// default) routes to the public pool (hive-oke).
const authMethodPrivate = "private"

// poolClusterForAuthMethod maps a provision request's auth_method to the
// placeholder pool it should draw from: private methods → the GPU pool
// (vllm-d, heartbeat-only); everything else → the public pool (hive-oke).
func poolClusterForAuthMethod(authMethod string) string {
	if strings.EqualFold(authMethod, authMethodPrivate) || strings.EqualFold(authMethod, gpuClusterID) {
		return gpuClusterID
	}
	return defaultClusterID
}

// clusterIDForHive returns the effective cluster ID of a SaaS hive, treating an
// empty cluster_id as the default (hive-oke) — matching clusterForHive's own
// fallback so pool matching agrees with cluster resolution.
func clusterIDForHive(h *SaaSHive) string {
	if h.ClusterID == "" {
		return defaultClusterID
	}
	return h.ClusterID
}

// findAvailablePlaceholder returns the ID of an available placeholder hive in
// the given pool (cluster), or "" if none exists. A placeholder is a SaaS hive
// owned by the hub admin, sitting at statusAvailable, on the target cluster.
func findAvailablePlaceholder(clusterID string) string {
	for _, h := range listSaaSHives() {
		if h.Status != statusAvailable {
			continue
		}
		if h.Owner != hubAdminUsername {
			continue
		}
		if clusterIDForHive(&h) != clusterID {
			continue
		}
		return h.ID
	}
	return ""
}

// AvailablePlaceholder is one row of the approve-picker dropdown: an available
// placeholder hive the admin can assign a provision request to.
type AvailablePlaceholder struct {
	ID          string `json:"id"`
	ClusterID   string `json:"cluster_id"`
	ProjectName string `json:"project_name"`
}

// listAvailablePlaceholders returns every available placeholder (admin-owned,
// statusAvailable), optionally filtered to a single pool (cluster). An empty
// pool returns placeholders across all pools. It mirrors findAvailablePlaceholder's
// availability predicate so the picker and the assign path agree on what's usable.
func listAvailablePlaceholders(pool string) []AvailablePlaceholder {
	var result []AvailablePlaceholder
	for _, h := range listSaaSHives() {
		if h.Status != statusAvailable {
			continue
		}
		if h.Owner != hubAdminUsername {
			continue
		}
		cluster := clusterIDForHive(&h)
		if pool != "" && cluster != pool {
			continue
		}
		result = append(result, AvailablePlaceholder{
			ID:          h.ID,
			ClusterID:   cluster,
			ProjectName: h.ProjectName,
		})
	}
	return result
}

// handleAvailablePlaceholders (admin-only) returns the available placeholders
// the approve-picker modal populates its dropdown from. An optional ?pool=
// filters to a single cluster; the default is all available placeholders.
func (s *HubServer) handleAvailablePlaceholders(w http.ResponseWriter, r *http.Request) {
	pool := strings.TrimSpace(r.URL.Query().Get("pool"))
	placeholders := listAvailablePlaceholders(pool)
	if placeholders == nil {
		placeholders = []AvailablePlaceholder{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"placeholders": placeholders})
}

// projectConfigForHiveID returns the claimed project's real org/repos/ACMM for
// delivery to the spoke in its heartbeat response, or nil when no reconcile is
// needed. It mirrors authorizedUsersForHiveID: nil when the hive has no SaaS
// record, is still an unclaimed placeholder (statusAvailable), or the spoke
// already reports the recorded project. The spoke's currently-reported values
// (curOrg/curRepos/curPrimary/curACMM) let the hub stop sending once matched.
// adoptSpokeProjectConfig makes the hub's meta.json track what a CLAIMED hive's
// spoke reports for its OPERATOR-CONTROLLED runtime settings: org, repos,
// primary_repo, and ACMM level. Once a placeholder is claimed, the spoke
// dashboard is the source of truth for these — an operator can re-point repos or
// change the level there. Without adopting them, the reconcile in
// projectConfigForHiveID keeps re-pushing meta's old values and silently reverts
// the operator's edit every heartbeat (the spyre / Joe Runde bug — originally
// just ACMM, but org/repos have the identical failure mode).
//
// Guardrails:
//   - No-op for a hive with no SaaS record or an unclaimed placeholder
//     (statusAvailable) — assign owns those until claimed.
//   - Never adopt EMPTY/zero values: a spoke reporting empty org/repos or level 0
//     (e.g. mid-boot, before its config loads) must not wipe/downgrade meta.
//   - Only writes when something actually changed.
func (s *HubServer) adoptSpokeProjectConfig(hiveID, org string, repos []string, primary string, level int) {
	h := loadSaaSHive(hiveID)
	if h == nil || h.Status == statusAvailable {
		return // no record, or an unclaimed placeholder — assign controls it
	}
	changed := false
	prevLevel := h.ACMMLevel

	// ACMM runs its OWN delivery handshake (ACMMDelivered), not the org/repos
	// one (ClaimDelivered).
	//
	// #2061 made ACMM "operator-owned from the start — always adopt", which fixed
	// the spyre revert but left a gap on the ASSIGN path: a freshly-claimed
	// placeholder keeps reporting the level it was MINTED at, and adopting that
	// pre-delivery report overwrote the level the requester asked for. #2333
	// closed that by gating on ClaimDelivered — correct for hives claimed after
	// it shipped, but a no-op for every hive claimed BEFORE, whose
	// ClaimDelivered was already true from the old org/repos-only rule. Those
	// hives adopt the stale report on the very first beat after upgrade and the
	// push stays disabled, which is why the live oke-11 hive is still L2 against
	// an approved L3 request.
	//
	// The dedicated flag defaults to false on those hives, so the level is
	// delivered exactly once, then ownership passes to the spoke as #2061
	// intended.

	// Backfill the requested level for hives assigned before the field existed.
	// Without this the reconcile has no target on exactly the hives that need
	// it. ACMMLevel is the best available record of the assignment at this
	// point, and using it is safe: if the spoke already agrees, the delivery is
	// a no-op that simply marks itself done.
	if h.RequestedACMMLevel == 0 && h.ACMMLevel > 0 {
		h.RequestedACMMLevel = h.ACMMLevel
		changed = true
	}

	// Delivery is complete once the spoke reports the level the hub asked for.
	// A level-0 report means "too old / mid-boot to say", never a mismatch.
	if !h.ACMMDelivered && h.RequestedACMMLevel > 0 && level == h.RequestedACMMLevel {
		h.ACMMDelivered = true
		changed = true
		s.logger.Info("acmm level delivered to spoke",
			"hive_id", hiveID, "acmm_level", level)
	}

	switch {
	case level <= 0:
		// Nothing reported — say nothing, change nothing.
	case !h.ACMMDelivered && h.RequestedACMMLevel > 0:
		// Pre-delivery the REQUESTED level is authoritative. Hold meta at it so
		// the stale spoke report cannot overwrite the target the push below is
		// still working toward — the exact loop that lost the L3.
		if h.ACMMLevel != h.RequestedACMMLevel {
			h.ACMMLevel = h.RequestedACMMLevel
			changed = true
		}
		if level != h.RequestedACMMLevel {
			s.logger.Info("acmm level not yet applied on spoke; holding requested level",
				"hive_id", hiveID,
				"spoke_reports", level,
				"requested", h.RequestedACMMLevel)
		}
	case level != h.ACMMLevel:
		// Post-delivery the spoke's dashboard owns the level: adopt operator
		// edits, and keep the requested level in step so a later re-delivery
		// never reverts the operator's choice.
		h.ACMMLevel = level
		h.RequestedACMMLevel = level
		changed = true
	}

	// Org/repos: while the claim hasn't been delivered yet, the hub is still
	// PUSHING them down and the spoke may report its OLD placeholder project —
	// do NOT adopt that (it would clobber the real claim). Mark the claim
	// delivered once the spoke reports the matching org/repos, and only AFTER
	// delivery treat the spoke as the source of truth for repo edits.
	orgMatches := org != "" && strings.EqualFold(org, h.Org)
	reposMatch := len(repos) > 0 && sameStringSliceFold(repos, h.Repos)
	primaryMatches := primary == "" || strings.EqualFold(primary, h.PrimaryRepo)
	// ACMM is NO LONGER part of this condition. #2333 added it here so the claim
	// could not be declared delivered while the level was outstanding, but that
	// coupled two independent deliveries: a hive whose level lagged would also
	// have its org/repos pushed forever, and — worse — the level had no way to
	// re-arm on a hive whose ClaimDelivered was already true. ACMMDelivered
	// above now tracks the level on its own, so this returns to being purely
	// about the project payload.
	if !h.ClaimDelivered {
		if orgMatches && reposMatch && primaryMatches {
			h.ClaimDelivered = true
			changed = true
		}
	} else {
		if org != "" && !strings.EqualFold(org, h.Org) {
			h.Org = org
			changed = true
		}
		if len(repos) > 0 && !sameStringSliceFold(repos, h.Repos) {
			h.Repos = repos
			changed = true
		}
		if primary != "" && !strings.EqualFold(primary, h.PrimaryRepo) {
			h.PrimaryRepo = primary
			changed = true
		}
	}
	if !changed {
		return
	}
	if err := saveSaaSHive(h); err != nil {
		s.logger.Warn("failed to persist spoke-reported project config to meta",
			"hive_id", hiveID, "error", err)
		return
	}
	// Keep the in-memory registry consistent so the UI and the next
	// projectConfigForHiveID comparison see the adopted values immediately.
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			s.registry.Hives[i].Org = h.Org
			s.registry.Hives[i].Repos = h.Repos
			s.registry.Hives[i].PrimaryRepo = h.PrimaryRepo
			s.registry.Hives[i].ACMMLevel = h.ACMMLevel
			break
		}
	}
	s.mu.Unlock()
	s.logger.Info("adopted dashboard-set project config from spoke heartbeat",
		"hive_id", hiveID, "org", h.Org, "primary_repo", h.PrimaryRepo,
		"acmm_was", prevLevel, "acmm_now", h.ACMMLevel)
}

// claimedVanityURL returns the vanity dashboard URL the hub should SHOW and LINK
// for a hive (My Hives, the SSO /open handoff, config proxy), or "" to fall back
// to the spoke-reported placeholder host.
//
// The rule is deliberately narrow so it never resurrects the 503 bug:
//   - Unclaimed placeholder (statusAvailable): "" — it has no project yet, so its
//     placeholder host is the only correct URL. Leave it.
//   - Claimed hive with a non-empty meta VanityURL: return it. A vanity URL is
//     only non-empty because it was VALIDATED as servable at provision/assign
//     time (addVanityHostToIngress succeeded, or a cluster wildcard/OpenShift
//     route already serves it, e.g. vllm-d's hosted-...apps.fmaas-vllm-d... host).
//     Trusting it here means the hub shows/links the friendly host the instant a
//     hive is claimed, instead of waiting for the spoke to adopt+report it back.
//   - Claimed hive with an empty VanityURL: "" — never mint an unvalidated host
//     here; the placeholder still works.
func claimedVanityURL(h *SaaSHive) string {
	if h == nil || h.Status == statusAvailable {
		return ""
	}
	return h.VanityURL
}

// curAPIURL is the GitHub API base URL the spoke reports it is CURRENTLY using
// (HeartbeatPayload.GitHubAPIURL). Empty means the spoke is too old to report
// it — treated as UNKNOWN, never as a mismatch.
func projectConfigForHiveID(hiveID, curOrg string, curRepos []string, curPrimary string, curACMM int, curURL, curAPIURL string) *HeartbeatProjectConfig {
	h := loadSaaSHive(hiveID)
	if h == nil {
		return nil
	}
	// This reconcile exists ONLY to push a freshly-CLAIMED placeholder's project
	// down to its spoke. It must NEVER touch a pre-existing hive, whose meta.json
	// predates the claim feature and carries stale/empty fields (empty
	// primary_repo, acmm_level: 0) even though the spoke runs a real project at a
	// real ACMM. Reconciling from that stale record silently wiped org/repos and
	// DOWNGRADED live hives to L0. So we only reconcile a record that looks like a
	// genuine claim — a complete project (org + repos + primary_repo) AND a real
	// non-zero ACMM — and even then we never send a value that would blank/lower
	// what the spoke already has.
	if h.Status == statusAvailable { // still an unclaimed placeholder
		return nil
	}
	primary := h.PrimaryRepo
	if primary == "" && len(h.Repos) > 0 {
		primary = h.Repos[0]
	}
	claimComplete := h.Org != "" && len(h.Repos) > 0 && primary != "" && h.ACMMLevel > 0
	if !claimComplete {
		// Incomplete/stale record (a pre-claim hive) — leave the spoke's PROJECT
		// (org/repos/ACMM) alone; reconciling from a stale record wiped/downgraded
		// live hives. BUT the vanity URL is independent of project completeness: a
		// claimed hive can carry a validated meta vanity_url (set at provision,
		// e.g. vllmd-10's hosted-...apps.fmaas-vllm-d... route) while its meta's
		// org/repos/ACMM are still stale/empty. Without pushing it, the spoke never
		// adopts the vanity URL and the hub keeps showing the raw placeholder host
		// forever (the placeholder-URL-persists bug). Push the URL alone — never
		// the stale project — until the spoke reports the vanity back.
		//
		// Safety: only a NON-EMPTY VanityURL is ever pushed. A vanity URL is only
		// non-empty because it was validated/served at provision or assign time
		// (addVanityHostToIngress succeeded, or a cluster wildcard route already
		// serves it), so this never pushes an unserved host and never reintroduces
		// the 503.
		//
		// A pending FORGE SWITCH is likewise independent of project
		// completeness — a hive can be moved between forges whether or not its
		// meta carries a complete claim, and the switch is precisely the
		// operator action that must not be silently dropped. Push the API URL
		// alone (never the stale project) until the spoke reports the requested
		// host back.
		if apiURL := pendingForgeAPIURL(h, curAPIURL); apiURL != "" {
			return &HeartbeatProjectConfig{GitHubAPIURL: apiURL}
		}
		if h.VanityURL != "" && curURL != h.VanityURL {
			return &HeartbeatProjectConfig{DashboardURL: h.VanityURL}
		}
		return nil
	}
	// What the hub still PUSHES to the spoke, and until when:
	//   - org/repos/primary_repo: pushed only until the claim is DELIVERED (the
	//     spoke first reports the assigned project). After delivery the spoke's
	//     dashboard owns them and the caller adopts operator edits instead.
	//   - vanity URL: pushed until the spoke first reports it back.
	//   - ACMM level: pushed on its OWN handshake (ACMMDelivered), until the
	//     spoke reports the requested level back. After that it is never pushed
	//     again — the spoke's dashboard owns it and the caller adopts operator
	//     edits, exactly as #2061 intended.
	//
	//     #2061 removed the ACMM push entirely because pushing it FOREVER
	//     reverted every dashboard level change (the spyre bug). Dropping it
	//     altogether meant a freshly-assigned placeholder was never told the
	//     level its owner requested. #2333 bounded the push by ClaimDelivered,
	//     which is right in principle but dead in practice for every hive
	//     claimed before it shipped: their ClaimDelivered was already true, so
	//     the push never fires. Bounding by the level's own flag is what makes
	//     the delivery reachable for those hives — and it is self-limiting, so
	//     re-running it is harmless.
	needClaimPush := !h.ClaimDelivered &&
		!(strings.EqualFold(curOrg, h.Org) &&
			sameStringSliceFold(curRepos, h.Repos) &&
			strings.EqualFold(curPrimary, primary))
	// Independent of the project claim: deliver the requested level until the
	// spoke confirms it. curACMM == 0 means the spoke did not report a level, so
	// there is nothing to correct yet.
	needACMMPush := !h.ACMMDelivered && h.RequestedACMMLevel > 0 && curACMM != h.RequestedACMMLevel
	needURLPush := h.VanityURL != "" && curURL != h.VanityURL
	// A spoke reporting a repo that fails isValidRepoRef is wedged: the hub 400s
	// its every heartbeat ("invalid repo name"), /api/livez then fails on the
	// stale heartbeat and the kubelet crash-loops the pod. Push a corrected
	// project even when the claim was already delivered — otherwise the guard
	// above ("nothing left to push") leaves the hive broken forever, since a
	// wedged spoke can never report anything the hub will accept.
	needRepoRepair := false
	for _, r := range append(append([]string{}, curRepos...), curPrimary) {
		if r != "" && !isValidRepoRef(r) {
			needRepoRepair = true
			break
		}
	}
	// A hive whose GitHubHost was filled in AFTER its claim was delivered —
	// the retroactive repair, or an admin editing the host later — has
	// ClaimDelivered == true and a matching vanity URL, so every gate above is
	// false and the reconcile returns nil forever. The spoke would then keep
	// talking to api.github.com against a GitHub Enterprise org (the vllm-d /
	// hosted-available-vllmd-01 failure). Push whenever we have a GHE API URL
	// to deliver and the spoke reports a DIFFERENT one.
	//
	// Deliberately conservative: an empty curAPIURL means the spoke is too old
	// to report its API URL, which is UNKNOWN, not a mismatch — pushing on it
	// would re-send on every beat with no read-back to ever stop it.
	wantAPIURL := gheAPIURLForHost(h.GitHubHost)
	needGHEAPIPush := wantAPIURL != "" && curAPIURL != "" && curAPIURL != wantAPIURL
	// A pending FORGE SWITCH pushes on its own handshake. It cannot ride
	// needGHEAPIPush: that gate is deliberately conservative about an empty
	// curAPIURL (unknown, not a mismatch) and — more importantly — it can never
	// deliver a switch TO public github.com, whose wantAPIURL is "" by
	// definition. An operator moving a hive back to github.com must be able to,
	// so the switch carries its own target and its own read-back.
	forgeAPIURL := pendingForgeAPIURL(h, curAPIURL)
	needForgePush := forgeAPIURL != ""
	if !needClaimPush && !needURLPush && !needRepoRepair && !needGHEAPIPush && !needACMMPush && !needForgePush {
		return nil // nothing left to push
	}
	// Sanitize before pushing. A repo pasted as a URL
	// ("github.ibm.com/enricom-ibm/jackrabbit") has two slashes, which
	// isValidRepoRef rejects — so the hub 400s the spoke's every heartbeat
	// ("invalid repo name"), /api/livez then fails on the stale heartbeat, and
	// the kubelet restarts the pod in a loop. Normalizing here repairs an
	// already-broken hive over the heartbeat, which is the only channel that
	// reaches a firewalled cluster (vllm-d).
	pushRepos := make([]string, 0, len(h.Repos))
	for _, r := range h.Repos {
		if rr := sanitizeRepoEntry(r); rr != "" {
			pushRepos = append(pushRepos, rr)
		}
	}
	if len(pushRepos) == 0 {
		pushRepos = h.Repos
	}
	pushPrimary := sanitizeRepoEntry(primary)
	if pushPrimary == "" {
		pushPrimary = primary
	}

	// Pre-delivery the REQUESTED level is what goes down the wire. The adopt path
	// also holds h.ACMMLevel at that value, so the two normally agree — but
	// stating it here means a push cannot deliver a stale level if this function
	// ever runs against a record the adopt path has not touched yet.
	pushACMM := h.ACMMLevel
	if !h.ACMMDelivered && h.RequestedACMMLevel > 0 {
		pushACMM = h.RequestedACMMLevel
	}

	return &HeartbeatProjectConfig{
		Org:          h.Org,
		Repos:        pushRepos,
		PrimaryRepo:  pushPrimary,
		ACMMLevel:    pushACMM,
		DashboardURL: h.VanityURL,
		// Point a GHE hive at its enterprise API. jjs-world
		// (hosted-open-source-osscar) is the working reference: a bare
		// primary_repo plus github.api_url = https://<host>/api/v3. Empty host
		// pushes nothing, so a github.com hive keeps the spoke's own default.
		// A pending forge switch wins: forgeAPIURL is the host the operator
		// asked for and is only non-empty while that delivery is outstanding.
		// Once the spoke reports the requested host, ForgeDelivered latches and
		// this falls back to the ordinary host-derived value — which by then
		// derives from the SAME host, so the two agree and nothing flaps.
		GitHubAPIURL: func() string {
			if forgeAPIURL != "" {
				return forgeAPIURL
			}
			return gheAPIURLForHost(h.GitHubHost)
		}(),
		// AIAuthor is deliberately left empty here. Provisioning state never
		// knows the agents' GitHub account — the spoke owns it — and the spoke
		// treats an empty author as "leave mine alone". Setting it from this
		// struct would reintroduce the blanking bug.
	}
}

// sameStringSliceFold reports whether two string slices contain the same
// entries in the same order, case-insensitively (org/repo names are compared
// case-insensitively throughout the hub).
func sameStringSliceFold(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// AssignHiveRequest is the body of POST /api/saas/hives/{id}/assign. It carries
// the real project the placeholder is being claimed for, plus optional GitHub
// App credentials to deliver to the spoke via the heartbeat channel.
type AssignHiveRequest struct {
	Owner string `json:"owner"`
	Org   string `json:"org"`
	// GitHubHost is the GitHub instance the org lives on ("" = public
	// github.com, otherwise a GHE host). Parsed from a pasted org URL when the
	// caller does not send it explicitly.
	GitHubHost     string `json:"github_host,omitempty"`
	Repos          string `json:"repos"`
	PrimaryRepo    string `json:"primary_repo"`
	ProjectName    string `json:"project_name"`
	ACMMLevel      int    `json:"acmm_level"`
	IsPublic       bool   `json:"is_public"`
	AppID          string `json:"app_id"`
	InstallationID string `json:"installation_id"`
	AppPrivateKey  string `json:"app_private_key"`
}

// handleAssignHive assigns an available placeholder hive to a real owner/project
// (admin-only). It rewrites the hive's meta.json to the real project and clears
// its "available" status, then delivers the new project config — and any GitHub
// App creds — to the spoke via the heartbeat response. This works uniformly for
// both reachable (hive-oke) and heartbeat-only (vllm-d) clusters: NO hub→spoke
// push or kubectl is used, so a vllm-d claim is delivered entirely by heartbeat.
func (s *HubServer) handleAssignHive(w http.ResponseWriter, r *http.Request) {
	if s.getAuthUser(r) != hubAdminUsername {
		http.Error(w, `{"error":"admin access required"}`, http.StatusForbidden)
		return
	}
	hiveID := r.PathValue("id")

	// A GitHub App private key PEM can be a few KB, so allow more headroom than
	// the provision-request body limit.
	const maxAssignRequestBodyBytes = 16 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxAssignRequestBodyBytes)
	var body AssignHiveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	h := loadSaaSHive(hiveID)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if h.Status != statusAvailable {
		http.Error(w, `{"error":"hive is not an available placeholder"}`, http.StatusConflict)
		return
	}

	// Validate the claimed project inputs (reuse the shared validators).
	if body.Owner == "" || !isValidName(body.Owner) {
		http.Error(w, `{"error":"invalid owner"}`, http.StatusBadRequest)
		return
	}
	// Accept a pasted org/repo URL here too — an admin assigning a hive reaches
	// for the same paste the requester did.
	if h, o := normalizeOrgRef(body.Org); o != "" {
		body.Org = o
		if h != "" && body.GitHubHost == "" {
			body.GitHubHost = h
		}
	}
	if body.Org == "" || !isValidName(body.Org) {
		http.Error(w, fmt.Sprintf(`{"error":"invalid org name %q — use the org name or its URL (e.g. github.ibm.com/my-org)"}`, body.Org), http.StatusBadRequest)
		return
	}
	// "public" is the sentinel that forces public github.com even on a GHE
	// cluster; it is not a hostname, so exempt it from the hostname validator.
	if body.GitHubHost != "" && !isValidName(body.GitHubHost) && !strings.EqualFold(body.GitHubHost, githubHostPublic) {
		http.Error(w, `{"error":"invalid github host"}`, http.StatusBadRequest)
		return
	}
	if body.Repos == "" {
		http.Error(w, `{"error":"repos are required"}`, http.StatusBadRequest)
		return
	}
	// Single-host-per-spoke (assign path — mirrors the request path). Every repo
	// and the primary must share the spoke's host, checked on the raw pasted
	// values before normalizeRepoRef strips the host. The "public" sentinel means
	// github.com, so pass "" to the validator for it.
	{
		spokeHost := body.GitHubHost
		if strings.EqualFold(spokeHost, githubHostPublic) {
			spokeHost = ""
		}
		if err := validateSingleRepoHost(spokeHost, body.PrimaryRepo, strings.Split(body.Repos, ",")); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}
	}
	{
		var cleaned []string
		for _, r := range strings.Split(body.Repos, ",") {
			if rr := normalizeRepoRef(r); rr != "" {
				cleaned = append(cleaned, rr)
			}
		}
		body.Repos = strings.Join(cleaned, ",")
		if body.PrimaryRepo != "" {
			body.PrimaryRepo = normalizeRepoRef(body.PrimaryRepo)
		}
	}
	var repos []string
	for _, repo := range strings.Split(body.Repos, ",") {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if !isValidRepoRef(repo) {
			http.Error(w, `{"error":"invalid repo name"}`, http.StatusBadRequest)
			return
		}
		repos = append(repos, repo)
	}
	if len(repos) == 0 {
		http.Error(w, `{"error":"repos are required"}`, http.StatusBadRequest)
		return
	}
	primaryRepo := strings.TrimSpace(body.PrimaryRepo)
	if primaryRepo == "" {
		primaryRepo = repos[0]
	} else if !isValidRepoRef(primaryRepo) {
		http.Error(w, `{"error":"invalid primary repo"}`, http.StatusBadRequest)
		return
	}

	acmm := body.ACMMLevel
	if acmm == 0 {
		acmm = defaultAssignACMMLevel
	}
	if acmm < minAssignACMMLevel || acmm > maxAssignACMMLevel {
		http.Error(w, `{"error":"acmm_level must be between 0 and 6"}`, http.StatusBadRequest)
		return
	}

	// Rewrite the placeholder's meta.json to the real project. Clearing status
	// (and any stale error) alone makes it show under the new owner in My Hives.
	h.Owner = body.Owner
	h.Org = body.Org
	// Preserve the placeholder's real cluster before the host backfill below,
	// which resolves via s.clusterForHive(h) and would fall back to hive-oke on
	// a blank cluster_id. The admin assigns a specific placeholder, so its own
	// ClusterID is authoritative; only a placeholder created without one lands
	// on the default here (never a silent blank that mis-routes to hive-oke).
	s.ensureClusterIDForClaim(h, "")
	// Record the GHE host (if any) so the heartbeat can point this spoke at the
	// right GitHub API. Never blank an existing value with an empty one.
	//
	// "public" is an explicit choice of public github.com on a cluster whose
	// defaults point at GHE. Record it as a blank host (so gheAPIURLForHost
	// pushes nothing and the spoke keeps api.github.com) PLUS the
	// GitHubBaseURL sentinel, which is what makes effectiveGitHubBaseURL
	// resolve to "" and therefore makes the cluster backfill below decline to
	// re-GHE the hive. Without the sentinel a blank host would simply be
	// refilled from the cluster on the very next line.
	if strings.EqualFold(body.GitHubHost, githubHostPublic) {
		h.GitHubHost = ""
		h.GitHubBaseURL = githubHostPublic
	} else if body.GitHubHost != "" {
		h.GitHubHost = body.GitHubHost
	}
	// Backfill the host from the hive's cluster when neither the request nor
	// the placeholder carries one. Placeholders provisioned BEFORE their
	// cluster gained github_base_url/github_api_url have GitHubHost == "", and
	// nothing else ever fills it in: projectConfigForHiveID pushes
	// gheAPIURLForHost(h.GitHubHost), which is empty for those hives, so the
	// spoke keeps api.github.com and the public app_id even though the cluster
	// is a GHE cluster (observed on vllm-d: hosted-available-vllmd-01 has
	// base_url: "" / api_url: "" against a github.ibm.com cluster). The hive's
	// own value always wins; this only fills a blank.
	if host := backfillGitHubHostFromCluster(h, s.clusterForHive(h)); host != "" {
		h.GitHubHost = host
		s.logger.Info("backfilled hive github host from cluster defaults",
			"hive", hiveID, "github_host", host)
	}
	h.Repos = repos
	h.PrimaryRepo = primaryRepo
	if body.ProjectName != "" {
		h.ProjectName = body.ProjectName
	}
	h.ACMMLevel = acmm
	// Same as the approve-provision path: the admin-assigned level is what the
	// hub must deliver, and it needs its own field because the spoke will
	// overwrite ACMMLevel with whatever it is currently running.
	h.RequestedACMMLevel = acmm
	h.ACMMDelivered = false
	h.IsPublic = body.IsPublic
	h.Status = ""
	h.Error = ""
	// A (re)assignment is a new claim payload: reset delivery so the hub pushes
	// this project to the spoke until it reports the new org/repos back, before
	// letting the spoke's dashboard own them.
	h.ClaimDelivered = false
	// Derive a stable, friendly vanity URL from the claimed project so the user
	// sees hosted-<org>-<repo>-*.<domain> instead of the raw placeholder host.
	// Compute once (idempotent re-assign keeps the same URL) and only when the
	// hive's cluster domain is known. The spoke adopts this via the heartbeat
	// (HeartbeatProjectConfig.DashboardURL) and reports it back, so the hub
	// registry's dashboardUrl becomes the vanity URL and stays that way.
	if h.VanityURL == "" {
		if cluster := s.clusterForHive(h); cluster != nil && cluster.Domain != "" {
			vanityHost := generateHiveID(body.Org, primaryRepo) + "." + cluster.Domain
			// Make the vanity host servable, then adopt it. Nothing else creates a
			// route for this host: provisioning templates a single DashboardHost, so
			// a vanity URL minted here resolves to no backend and every hub link to
			// it — including the SSO handoff — returns 503, while the raw placeholder
			// host works fine. Create the route FIRST, then adopt the URL.
			//
			// VanityURL doubles as the "placeholder is claimed" marker
			// (projectConfigForHiveID keeps pushing until the spoke reports it
			// back), so it is always set once we have a domain to derive it from —
			// but a failed create is logged at Warn rather than swallowed, since the
			// symptom is a 503 on every hub link to this hive until the route is
			// reconciled (cert-manager on nginx, or the cluster wildcard on the
			// heartbeat-only OpenShift pool where the hub cannot kubectl to create
			// it, so the create "fails" yet the *.apps.<domain> wildcard still
			// serves the host — the vllm-d case).
			// Goes through the same seam as the retroactive repair so both
			// adoption paths share one definition of "servable" (and so tests,
			// which have no kubectl, can exercise the success path).
			if err := s.makeVanityHostServable(hiveID, vanityHost, cluster); err != nil {
				// Do NOT adopt a host we could not make servable. This used to
				// assume an OpenShift cluster wildcard would serve the host
				// anyway and adopt it regardless; on vllm-d that assumption is
				// false — only explicitly created *-vanity Routes are served, so
				// the hub advertised a hostname that returned 503 while the raw
				// placeholder host worked fine. Leaving VanityURL empty makes
				// every read path fall back to the working placeholder, and the
				// heartbeat repair re-attempts adoption once a route exists.
				s.logger.Warn("vanity host is not served: could not add it to the spoke ingress/route — "+
					"keeping the working placeholder host; the heartbeat repair will retry",
					"hive", hiveID, "host", vanityHost, "cluster", cluster.ID, "error", err)
			} else {
				h.VanityURL = "https://" + vanityHost
			}
		}
	}
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save hive assignment"}`, http.StatusInternalServerError)
		return
	}

	// Grant the assignee owner access. handleAccessList builds a hive's access
	// list by scanning every user record for Hives[hiveID], NOT from h.Owner —
	// so without this the assignment set h.Owner correctly while Manage Access
	// still showed only the admin who provisioned the placeholder. On a
	// heartbeat-only cluster (vllm-d) that stale list is what reaches the spoke.
	assignee := loadSaaSUser(body.Owner)
	if assignee == nil {
		assignee = ensureSaaSUser(body.Owner)
	}
	if assignee.Hives == nil {
		assignee.Hives = map[string]string{}
	}
	if assignee.Hives[hiveID] != "owner" {
		assignee.Hives[hiveID] = "owner"
		assignee.SaaSQuota++
		if err := saveSaaSUser(assignee); err != nil {
			s.logger.Warn("assigned hive but failed to grant owner access", "user", body.Owner, "hive", hiveID, "error", err)
		}
	}

	// Deliver GitHub App creds (if supplied) via the SAME heartbeat channel the
	// webhook path uses — storePendingGitHubAppConfig queues them for the next
	// heartbeat response (consumePendingGitHubAppConfig in handleHeartbeat). We
	// deliberately do NOT call pushGitHubConfigToSpoke here: it requires a
	// reachable dashboardURL and would fail for vllm-d. The heartbeat path
	// covers both clusters uniformly.
	appDelivered := false
	if body.AppID != "" && body.InstallationID != "" && strings.TrimSpace(body.AppPrivateKey) != "" {
		appID, err1 := strconv.ParseInt(strings.TrimSpace(body.AppID), 10, 64)
		installID, err2 := strconv.ParseInt(strings.TrimSpace(body.InstallationID), 10, 64)
		if err1 != nil || err2 != nil {
			http.Error(w, `{"error":"app_id and installation_id must be numeric"}`, http.StatusBadRequest)
			return
		}
		if !strings.HasPrefix(strings.TrimSpace(body.AppPrivateKey), "-----BEGIN") {
			http.Error(w, `{"error":"app_private_key must be a PEM private key"}`, http.StatusBadRequest)
			return
		}
		s.storePendingGitHubAppConfig(hiveID, &HeartbeatGitHubAppConfig{
			AppID:          appID,
			InstallationID: installID,
			PrivateKey:     strings.TrimSpace(body.AppPrivateKey),
		})
		appDelivered = true
	}

	// The project config itself is delivered by handleHeartbeat via
	// projectConfigForHiveID on the next beat — it keeps sending until the spoke
	// reports the matching project. No hub→spoke push or kubectl is needed, so
	// this works for the heartbeat-only vllm-d pool as well as hive-oke.

	s.logger.Info("audit: placeholder hive assigned",
		"hive_id", hiveID,
		"owner", h.Owner,
		"org", h.Org,
		"primary_repo", h.PrimaryRepo,
		"acmm_level", h.ACMMLevel,
		"cluster", clusterIDForHive(h),
		"app_creds_delivered", appDelivered,
	)
	s.recordTimeline(hiveID, TimelineOwnership,
		fmt.Sprintf("hive assigned to %s (%s/%s, ACMM %d)", h.Owner, h.Org, h.PrimaryRepo, h.ACMMLevel),
		s.getAuthUser(r))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":       "assigned",
		"id":           hiveID,
		"owner":        h.Owner,
		"org":          h.Org,
		"primary_repo": h.PrimaryRepo,
		"acmm_level":   h.ACMMLevel,
	})
}

func (s *HubServer) handleUserToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HiveID   string `json:"hive_id"`
		Username string `json:"username"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.HiveID == "" || body.Username == "" {
		http.Error(w, `{"error":"hive_id and username required"}`, http.StatusBadRequest)
		return
	}

	requester := s.getAuthUser(r)
	if requester != body.Username && requester != hubAdminUsername {
		http.Error(w, `{"error":"can only retrieve your own token"}`, http.StatusForbidden)
		return
	}

	user := loadSaaSUser(body.Username)
	if user == nil {
		writeJSONError(w, http.StatusNotFound, "user not found")
		return
	}

	if _, ok := user.Hives[body.HiveID]; !ok {
		http.Error(w, `{"error":"user has no access to this hive"}`, http.StatusForbidden)
		return
	}

	if user.EncryptedToken == "" {
		http.Error(w, `{"error":"no token stored for this user"}`, http.StatusNotFound)
		return
	}

	token, err := decryptToken(user.EncryptedToken)
	if err != nil {
		s.logger.Warn("failed to decrypt user token", "user", body.Username, "error", err)
		http.Error(w, `{"error":"token decryption failed"}`, http.StatusInternalServerError)
		return
	}

	s.logger.Info("audit: user token issued", "user", body.Username, "hive", body.HiveID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": token})
}

var publicPaths = []string{"/snapshot", "/leaderboard", "/contribute", "/api/leaderboard", "/api/contribute", ssoHandoffPath}

// ssoHandoffPath is the spoke's SSO handoff endpoint. It MUST bypass the hub's
// nginx auth_request gate.
//
// Why: the auth-check subrequest authorizes a user against THIS hive's grant
// list (user.Hives[hiveID]). The whole point of the handoff is to admit a user
// who is authenticated on the hub but has no hub-side grant row for the hive —
// the spoke's own authorized_users allowlist is the authority. Gating /sso on
// the hub check therefore 401s exactly the requests the handoff exists to
// serve, and nginx turns that 401 into an auth-signin redirect back to the hub
// login, which (the user already having a valid hub cookie) immediately
// redirects to /sso again — an infinite bounce the browser eventually aborts.
//
// This does NOT weaken authentication: the signed, hive-scoped, short-lived
// HMAC token in the query IS the credential, and dashboard.handleSSO verifies
// it against HIVE_HUB_SECRET plus the spoke's own allowlist before minting any
// session. It is the same reasoning that already makes /sso a public path on
// the spoke half (see dashboard.isPublicPath).
const ssoHandoffPath = "/sso"

// proxyAuthHeader is the response header the hub sets on a successful
// auth-check subrequest to PROVE to the spoke that the request was
// authenticated by the hub's nginx auth-proxy (not spoofed by a client that
// merely supplied X-Hive-User/X-Hive-Role directly). nginx is configured to
// copy this header onto the upstream request via the auth-response-headers
// annotation (see saas_provision.go). Its value is the spoke's own dashboard
// token, so the spoke can constant-time-compare it against its authToken.
//
// CONTRACT (spoke half, separate v3 PR must verify EXACTLY this):
//   - Header name: "X-Hive-Proxy-Auth"
//   - Value: the hive's dashboard token — the raw value stored in the spoke's
//     "hive-secrets" k8s secret under key "dashboard-token", i.e. the same
//     string the spoke reads from DASHBOARD_AUTH_TOKEN into its authToken.
//   - Set ONLY on the authenticated success path; NEVER on public-path,
//     unfurl-bot, unauthenticated (401), or no-access (403) responses.
const proxyAuthHeader = "X-Hive-Proxy-Auth"

// spokeProxyAuthCacheTTL bounds how long a hive's dashboard token is memoized
// so the per-request auth-check subrequest avoids a kubectl exec on every call
// while still picking up a re-provisioned token within a bounded window.
const spokeProxyAuthCacheTTL = 5 * time.Minute

// spokeProxyAuthEntry is a cached dashboard token with its expiry.
type spokeProxyAuthEntry struct {
	token   string
	expires time.Time
}

// spokeProxyAuthToken returns the given hive's dashboard token — the shared
// secret the spoke holds as its authToken (DASHBOARD_AUTH_TOKEN / the
// "dashboard-token" key of the "hive-secrets" k8s secret). It memoizes the
// value for spokeProxyAuthCacheTTL so the hot auth-check path does not exec
// kubectl on every proxied request. Returns "" if the token cannot be
// resolved (e.g. the hive isn't in the registry or its cluster is unreachable);
// callers must then omit the proof header rather than send an empty one.
func (s *HubServer) spokeProxyAuthToken(hiveID string) string {
	now := time.Now()

	s.spokeProxyAuthMu.Lock()
	if entry, ok := s.spokeProxyAuthCache[hiveID]; ok && now.Before(entry.expires) {
		tok := entry.token
		s.spokeProxyAuthMu.Unlock()
		return tok
	}
	s.spokeProxyAuthMu.Unlock()

	// Resolve the registry entry (ID + ClusterID) that loadSpokeAuthToken needs.
	var hive *RegistryEntry
	s.mu.RLock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			h := s.registry.Hives[i]
			hive = &h
			break
		}
	}
	s.mu.RUnlock()
	if hive == nil {
		return ""
	}

	token := s.loadSpokeAuthToken(hive)
	if token == "" {
		return ""
	}

	s.spokeProxyAuthMu.Lock()
	s.spokeProxyAuthCache[hiveID] = spokeProxyAuthEntry{token: token, expires: now.Add(spokeProxyAuthCacheTTL)}
	s.spokeProxyAuthMu.Unlock()

	return token
}

func (s *HubServer) handleSaaSAuthCheck(w http.ResponseWriter, r *http.Request) {
	hiveID := r.URL.Query().Get("hive")
	if hiveID == "" {
		http.Error(w, "missing hive param", http.StatusBadRequest)
		return
	}

	originalURI := r.Header.Get("X-Original-URI")
	if originalURI == "" {
		if origURL := r.Header.Get("X-Original-URL"); origURL != "" {
			if u, err := url.Parse(origURL); err == nil {
				originalURI = u.Path
			}
		}
	}
	if originalURI == "" {
		originalURI = r.URL.Query().Get("uri")
	}
	for _, p := range publicPaths {
		if strings.HasPrefix(originalURI, p) {
			w.WriteHeader(http.StatusOK)
			return
		}
	}

	if isUnfurlBot(r.Header.Get("User-Agent")) {
		w.WriteHeader(http.StatusOK)
		return
	}

	username := s.getAuthUser(r)
	if username == "" {
		http.Error(w, "not authenticated", http.StatusUnauthorized)
		return
	}

	user := loadSaaSUser(username)
	if user == nil {
		http.Error(w, "no access", http.StatusForbidden)
		return
	}

	role, ok := user.Hives[hiveID]
	if !ok {
		http.Error(w, "no access to this hive", http.StatusForbidden)
		return
	}

	w.Header().Set("X-Hive-User", username)
	w.Header().Set("X-Hive-Role", role)
	// Prove to the spoke that this request really passed through the hub's
	// auth-proxy: set X-Hive-Proxy-Auth to the hive's own dashboard token so
	// the spoke can constant-time-compare it against its authToken. Only set on
	// this authenticated success path; a client hitting the spoke directly
	// cannot forge it because it never learns the token. If the token can't be
	// resolved, omit the header (backward-compatible: the spoke half must fail
	// open only until it is deployed — see the v3 spoke PR).
	if proxyAuth := s.spokeProxyAuthToken(hiveID); proxyAuth != "" {
		w.Header().Set(proxyAuthHeader, proxyAuth)
	}
	w.WriteHeader(http.StatusOK)
}

func isUnfurlBot(ua string) bool {
	bots := []string{"Slackbot", "Slack-ImgProxy", "Discordbot", "Twitterbot", "facebookexternalhit", "LinkedInBot", "WhatsApp", "TelegramBot"}
	for _, b := range bots {
		if strings.Contains(ua, b) {
			return true
		}
	}
	return false
}

const ogFallbackHTML = `<!DOCTYPE html><html><head>
<meta charset="utf-8">
<meta property="og:title" content="My Hives — Hive Hub">
<meta property="og:description" content="AI Agent Orchestration for Open Source. Manage your hive instances — monitor agents, governor mode, issues, PRs, and contributor activity.">
<meta property="og:type" content="website">
<meta property="og:site_name" content="Hive Hub">
<meta property="og:url" content="https://hive.kubestellar.io/dashboard">
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><text y='.9em' font-size='90'>🍯</text></svg>">
<title>My Hives — Hive Hub</title>
</head><body></body></html>`

func (s *HubServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if isUnfurlBot(r.UserAgent()) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(ogFallbackHTML))
		return
	}
	cookie, err := r.Cookie("hive_hub_user")
	if err != nil || cookie.Value == "" {
		http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, dashboardHTML)
}

func (s *HubServer) handleAccessDenied(w http.ResponseWriter, r *http.Request) {
	hiveID := sanitize(r.URL.Query().Get("hive"))

	ownerLink := ""
	s.mu.RLock()
	for _, h := range s.registry.Hives {
		if h.ID == hiveID && h.Owner != "" {
			safeOwner := sanitize(h.Owner)
			if safeOwner != "" {
				ownerLink = fmt.Sprintf(`<a href="https://github.com/%s" target="_blank" style="color:#58a6ff;text-decoration:underline">the hive owner</a>`, safeOwner)
			}
			break
		}
	}
	s.mu.RUnlock()
	if ownerLink == "" {
		ownerLink = "the hive owner"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>Access Denied — Hive Hub</title>
<script async src="https://www.googletagmanager.com/gtag/js?id=G-4707R797K3"></script><script>window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments)}gtag("js",new Date());gtag("config","G-4707R797K3");gtag("event","access_denied",{hive_id:"%s"});</script>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0d1117;color:#e6edf3;display:flex;justify-content:center;align-items:center;min-height:100vh}
.card{background:#161b22;border:1px solid #30363d;border-radius:12px;padding:48px;max-width:520px;text-align:center}
h1{font-size:2rem;margin-bottom:8px}
.bee{font-size:3rem;margin-bottom:16px}
.msg{color:#8b949e;margin-bottom:24px;line-height:1.6}
.hive-name{color:#f0883e;font-family:monospace;font-weight:600}
.btn{display:inline-block;padding:10px 24px;border-radius:8px;text-decoration:none;font-weight:600;font-size:0.9rem;margin:6px}
.btn-primary{background:#238636;color:#fff}
.btn-secondary{background:transparent;color:#58a6ff;border:1px solid #30363d}
.help{color:#8b949e;font-size:0.8rem;margin-top:24px}
</style></head><body>
<div class="card">
<div class="bee">🐝</div>
<h1>Access Denied</h1>
<p class="msg">
You don't have access to
<span class="hive-name">%s</span>.<br><br>
Ask %s to grant you access from their
<a href="/dashboard" style="color:#58a6ff">My Hives</a> dashboard.
</p>
<a href="/dashboard" class="btn btn-primary">Go to My Hives</a>
<a href="/" class="btn btn-secondary">Browse Public Hives</a>
<p class="help">If you believe this is an error, <a href="https://github.com/kubestellar/hive/issues" style="color:#58a6ff">file an issue</a>.</p>
</div>
</body></html>`, hiveID, hiveID, ownerLink)
}

const dashboardHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><text y='.9em' font-size='90'>🍯</text></svg>">
  <meta property="og:title" content="My Hives — Hive Hub">
  <meta property="og:description" content="Manage your AI agent hives. View local and hosted hive instances, monitor status, upgrade, and control access.">
  <meta property="og:type" content="website">
  <meta property="og:site_name" content="Hive Hub">
  <!-- GA4 --><script async src="https://www.googletagmanager.com/gtag/js?id=G-4707R797K3"></script><script>window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments)}gtag("js",new Date());gtag("config","G-4707R797K3");</script>
  <title>My Hives — Hive Hub</title>
  <style>
    :root {
      color-scheme: dark;
      --bg: #080b0f;
      --bg-soft: #0d1218;
      --panel: #121922;
      --panel-strong: #17212d;
      --text: #f6f8fb;
      --fg: #f6f8fb;
      --muted: #a8b3c2;
      --line: #263545;
      --amber: #f4c75f;
      --green: #74df9a;
      --blue: #80bfff;
      --red: #ff7e7e;
      --purple: #b482ff;
      --shadow: 0 24px 80px #0006;
      /* Legacy token aliases used by dashboard markup and scripts */
      --surface: var(--panel);
      --border: var(--line);
      --accent: var(--amber);
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
    }
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body {
      background:
        radial-gradient(circle at 18% 8%, #80bfff2e, transparent 24rem),
        radial-gradient(circle at 86% 4%, #f4c75f29, transparent 20rem),
        linear-gradient(180deg, #090d12 0%, var(--bg) 48%, #0a0e13 100%);
      min-width: 320px;
      min-height: 100vh;
      color: var(--text);
    }
    a { color: var(--accent); text-decoration: none; }
    a:hover { text-decoration: underline; }
    input, textarea, select, button { font-family: inherit; }

    /* ── Header ── */
    .site-header {
      z-index: 50;
      backdrop-filter: blur(18px);
      -webkit-backdrop-filter: blur(18px);
      background: #080b0fc7;
      border-bottom: 1px solid #ffffff14;
      display: grid;
      grid-template-columns: 1fr auto 1fr;
      align-items: center;
      gap: 1.5rem;
      padding: 1rem clamp(1rem, 4vw, 4.5rem);
      position: sticky;
      top: 0;
    }
    .brand, .header-link, .site-header nav { align-items: center; display: flex; }
    .brand { letter-spacing: 0; gap: .75rem; font-weight: 800; color: var(--text); }
    .brand:hover { text-decoration: none; }
    .brand-mark {
      width: 2.5rem; height: 2.5rem;
      color: var(--amber);
      background: linear-gradient(145deg, #f4c75f2e, #80bfff1a);
      border: 1px solid #f4c75f6b;
      border-radius: .65rem;
      place-items: center;
      font-size: 1.2rem;
      display: grid;
    }
    .site-header nav { color: var(--muted); gap: 1.35rem; font-size: .94rem; flex-wrap: nowrap; }
    .site-header nav a { color: var(--muted); white-space: nowrap; }
    .site-header nav a:hover, .header-link:hover { color: var(--text); text-decoration: none; }
    .header-link {
      border: 1px solid var(--line);
      width: fit-content;
      color: var(--text);
      border-radius: .55rem;
      justify-self: end;
      padding: .72rem 1rem;
      font-weight: 700;
    }
    .header-right { display: flex; align-items: center; gap: .8rem; justify-self: end; }
    .nav-user { display: inline-flex; align-items: center; gap: 6px; white-space: nowrap; color: var(--text); }
    .nav-avatar { width: 28px; height: 28px; border-radius: 50%; }

    /* ── Layout ── */
    .content { max-width: 1600px; margin: 0 auto; padding: 2.5rem clamp(1rem, 4vw, 4.5rem) 3rem; }
    .section-label {
      color: var(--amber);
      letter-spacing: .12em;
      text-transform: uppercase;
      margin: 0 0 .8rem;
      font-size: .82rem;
      font-weight: 900;
    }
    h1 { letter-spacing: 0; font-size: clamp(2rem, 4vw, 3rem); line-height: .98; margin-bottom: .8rem; }
    .subtitle { color: var(--muted); font-size: 1.02rem; line-height: 1.7; margin-bottom: 32px; }

    /* ── Table ── */
    /* Status filter chips above the My Hives table. --chip-color is set inline
       per chip so one rule serves every state colour. */
    #hive-filter-bar { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin-bottom: 14px; }
    #hive-drift-summary { margin-bottom: 12px; padding: 10px 12px; border: 1px solid var(--border); border-radius: 8px; background: rgba(248,81,73,0.04); }
    .filter-chips { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
    .filter-chip { display: inline-flex; align-items: center; gap: 6px; padding: 5px 12px; background: var(--surface); color: var(--muted); border: 1px solid var(--border); border-radius: 9999px; font-size: 0.72rem; font-weight: 600; cursor: pointer; font-family: inherit; transition: all .15s; }
    .filter-chip:hover { border-color: var(--chip-color, var(--muted)); color: var(--text); }
    .filter-chip.on { color: var(--chip-color); border-color: var(--chip-color); background: rgba(255,255,255,0.06); }
    .filter-chip-dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; flex: none; }
    .filter-chip-count { font-size: 0.65rem; opacity: 0.75; font-variant-numeric: tabular-nums; }
    .filter-chip-clear { color: var(--muted); }
    .filter-summary { font-size: 0.72rem; color: var(--muted); white-space: nowrap; }

    /* ── Attention needed (fleet alerts) ── */
    /* The panel inverts the default reading of My Hives: instead of scanning 40+
       rows for problems, the operator reads this and stops. When the fleet is
       clean it collapses to a single quiet line, so "nothing needs you" is the
       normal state of the screen rather than an absence the eye has to infer.
       --alert-color is set inline per severity so one rule serves all three. */
    #fleet-alerts-panel { margin-bottom: 16px; }
    .alert-panel { border: 1px solid var(--border); border-radius: 10px; background: var(--surface); padding: 12px 16px; }
    .alert-panel.has-critical { border-color: rgba(248,81,73,0.45); background: rgba(248,81,73,0.06); }
    .alert-panel.has-warning { border-color: rgba(245,158,11,0.4); background: rgba(245,158,11,0.05); }
    .alert-panel-clean { display: flex; align-items: center; gap: 8px; font-size: 0.78rem; color: var(--muted); }
    .alert-panel-head { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
    .alert-panel-title { display: flex; align-items: center; gap: 8px; font-size: 0.85rem; font-weight: 700; color: var(--text); }
    .alert-sev-pill { display: inline-flex; align-items: center; gap: 5px; padding: 2px 9px; border-radius: 9999px; font-size: 0.68rem; font-weight: 700; background: rgba(255,255,255,0.05); color: var(--alert-color); border: 1px solid var(--alert-color); font-variant-numeric: tabular-nums; }
    .alert-type-chips { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; margin-top: 10px; }
    .alert-rows { margin-top: 10px; display: flex; flex-direction: column; gap: 6px; }
    .alert-row { display: flex; align-items: baseline; gap: 8px; flex-wrap: wrap; font-size: 0.74rem; padding: 5px 8px; border-radius: 6px; background: rgba(255,255,255,0.03); }
    .alert-row.acked { opacity: 0.55; }
    /* An alert row is a real button: clicking it jumps to that hive's row in the
       table below. Styled to look clickable (pointer, hover lift, focus ring)
       because the previous inert <div> gave no hint that anything would happen.
       width:100%/text-align:left undo the <button> defaults so the row lays out
       exactly as the old div did. */
    /* The jump button and the Acknowledge button are siblings (a button cannot
       nest inside a button), so this wrapper keeps them on one visual line. */
    .alert-row-wrap { display: flex; align-items: center; gap: 6px; }
    .alert-row-wrap .alert-ack-btn { flex: 0 0 auto; }
    button.alert-row { flex: 1 1 auto; min-width: 0; width: 100%; text-align: left; font-family: inherit; color: inherit; border: 1px solid transparent; cursor: pointer; }
    button.alert-row:hover { background: rgba(255,255,255,0.07); border-color: var(--border); }
    button.alert-row:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
    /* Target highlight: a brief ring on the hive row the operator was sent to,
       so it is obvious WHICH row answered the click after the scroll settles. */
    .hive-row-targeted > td { background: rgba(88,166,255,0.16) !important; }
    .hive-row-targeted > td:first-child { box-shadow: inset 3px 0 0 var(--accent); }
    .alert-row-hive { font-weight: 700; color: var(--text); }
    .alert-row-reason { color: var(--muted); }
    .alert-row-age { color: var(--muted); font-size: 0.68rem; margin-left: auto; white-space: nowrap; }
    .alert-ack-btn { background: none; border: 1px solid var(--border); color: var(--muted); border-radius: 5px; padding: 1px 8px; font-size: 0.66rem; font-family: inherit; cursor: pointer; }
    .alert-ack-btn:hover { color: var(--text); border-color: var(--muted); }
    .alert-panel-more { margin-top: 8px; font-size: 0.7rem; color: var(--muted); background: none; border: none; font-family: inherit; cursor: pointer; text-decoration: underline; padding: 0; }

    /* ── Hive search + facets ──
       The facet panel is a CLICK-TOGGLED left tray, collapsed by default so the
       dashboard is uncluttered until the operator asks for filters. Collapsed it
       is a narrow rail (--facet-rail-w) carrying a filter glyph and, whenever
       anything is narrowing the list, an active-filter count badge. Clicking the
       rail expands the tray; clicking again collapses it. The state persists in
       localStorage (LS_FACET_TRAY_OPEN).

       No hover-reveal: a pointer-only affordance is unreachable on touch and
       flickers across the gap between rail and panel. A real <button> with
       aria-expanded works identically with mouse, touch and keyboard.

       Why overlay rather than push: pushing would re-flow the table on every
       toggle, which re-triggers .table-wrap's horizontal-scroll measurement.
       Overlaying leaves the scroll container completely untouched.

       Layering: the tray sits at z-index 40, deliberately BELOW the row hover
       panels (.hive-access-pop / healthBadge's custom panel, z-index 60), so a
       row's panel always draws above the tray rather than behind it. */
    #hive-search-row { display: flex; align-items: center; gap: 10px; margin-bottom: 12px; }
    #hive-search { flex: 1; min-width: 0; padding: 8px 14px; background: var(--surface); border: 1px solid var(--border); border-radius: 6px; color: var(--text); font-size: 0.85rem; font-family: inherit; }
    #hive-search:focus { outline: none; border-color: var(--accent); }
    #hive-search-clear { flex: none; }
    /* Collapsed rail width and expanded tray width. The layout grid reserves
       only the rail; the tray overhangs it. */
    :root { --facet-rail-w: 34px; --facet-tray-w: 240px; }
    .hive-layout { display: grid; grid-template-columns: var(--facet-rail-w) minmax(0, 1fr); gap: 12px; align-items: start; }
    /* The tray's positioning context. position:relative only — no overflow
       clipping, so the table's own hover panels are never cut off by it. */
    .facet-shell { position: relative; min-width: 0; }
    /* Always-visible collapsed affordance. .has-active turns it accent-coloured
       and reveals the badge, so a filtered list always has a visible cause even
       with the tray shut. */
    .facet-rail-tab { display: flex; flex-direction: column; align-items: center; gap: 6px; width: var(--facet-rail-w); padding: 8px 0; background: var(--surface); border: 1px solid var(--border); border-radius: 8px; color: var(--muted); font: inherit; font-size: 0.8rem; cursor: pointer; }
    .facet-rail-tab:hover, .facet-rail-tab:focus-visible { color: var(--text); border-color: var(--muted); }
    .facet-rail-tab.has-active { border-color: var(--accent); color: var(--accent); }
    .facet-rail-word { writing-mode: vertical-rl; letter-spacing: 0.08em; font-size: 0.6rem; text-transform: uppercase; }
    .facet-active-badge { display: inline-flex; align-items: center; justify-content: center; min-width: 18px; height: 18px; padding: 0 4px; border-radius: 9999px; background: var(--accent); color: #000; font-size: 0.62rem; font-weight: 800; font-variant-numeric: tabular-nums; }
    /* Closed by default; .facet-open (driven entirely by JS from the persisted
       flag) is the only thing that reveals it. display:none while closed keeps
       every control inside it out of the tab order. */
    .facet-tray { display: none; position: absolute; top: 0; left: 0; z-index: 40; width: var(--facet-tray-w); max-height: 70vh; overflow-y: auto; padding: 10px; background: var(--bg-soft, var(--surface)); border: 1px solid var(--border); border-radius: 10px; box-shadow: 0 12px 32px rgba(0,0,0,0.5); }
    .facet-shell.facet-open .facet-tray { display: block; }
    .facet-tray-head { display: flex; align-items: center; justify-content: space-between; gap: 6px; margin-bottom: 8px; color: var(--muted); font-size: 0.66rem; font-weight: 700; text-transform: uppercase; letter-spacing: 0.06em; }
    .facet-tray-close { background: none; border: none; color: var(--muted); font: inherit; font-size: 0.9rem; line-height: 1; cursor: pointer; padding: 2px 4px; }
    .facet-tray-close:hover { color: var(--text); }
    .facet-group { margin-bottom: 14px; border: 1px solid var(--border); border-radius: 8px; background: var(--surface); overflow: hidden; }
    .facet-group-head { display: flex; align-items: center; justify-content: space-between; gap: 6px; width: 100%; padding: 8px 10px; background: none; border: none; color: var(--muted); font: inherit; font-size: 0.7rem; font-weight: 700; text-transform: uppercase; letter-spacing: 0.5px; cursor: pointer; text-align: left; }
    .facet-group-head:hover { color: var(--text); }
    .facet-values { padding: 2px 6px 8px; }
    .facet-value { display: flex; align-items: center; justify-content: space-between; gap: 6px; width: 100%; padding: 4px 6px; background: none; border: none; border-radius: 4px; color: var(--muted); font: inherit; font-size: 0.74rem; cursor: pointer; text-align: left; }
    .facet-value:hover { background: rgba(255,255,255,0.05); color: var(--text); }
    .facet-value.on { color: var(--accent); font-weight: 700; background: rgba(244,199,95,0.08); }
    .facet-value-label { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
    .facet-value-count { flex: none; font-size: 0.65rem; opacity: 0.75; font-variant-numeric: tabular-nums; }
    /* Section headers double as expand/collapse buttons for the admin
       assigned/unassigned groups. */
    .hive-section-head td { cursor: pointer; user-select: none; }
    .hive-section-head td:hover { color: var(--text) !important; }
    .hive-section-caret { display: inline-block; width: 12px; margin-right: 4px; }
    /* Grouping + saved-view controls, sitting above the status chips. */
    #hive-view-bar { display: flex; align-items: center; justify-content: space-between; gap: 12px; flex-wrap: wrap; margin-bottom: 10px; }
    .view-ctls { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
    .view-ctl { display: inline-flex; align-items: center; gap: 6px; }
    .view-ctl-label { font-size: 0.68rem; color: var(--muted); text-transform: uppercase; letter-spacing: 0.5px; font-weight: 600; }
    .view-select { background: var(--surface); color: var(--text); border: 1px solid var(--border); border-radius: 6px; padding: 4px 8px; font-size: 0.72rem; font-family: inherit; cursor: pointer; max-width: 220px; }
    .view-btn { background: var(--surface); color: var(--muted); border: 1px solid var(--border); border-radius: 6px; padding: 4px 10px; font-size: 0.7rem; font-weight: 600; font-family: inherit; cursor: pointer; transition: all .15s; }
    .view-btn:hover:not(:disabled) { color: var(--text); border-color: var(--muted); }
    .view-btn:disabled { opacity: 0.4; cursor: not-allowed; }
    /* Group header rows nest one step in from the Assigned/Unassigned headers,
       so the outer section split stays legible as the outer structure. */
    .hive-group-head td { cursor: pointer; }
    .hive-group-head:hover td { color: var(--text) !important; }
    .hive-group-caret { display: inline-block; width: 12px; font-size: 0.7rem; }
    .table-wrap { overflow: visible; margin: 0 auto; position: relative; }
    .hive-menu-cell:hover .hive-menu-dropdown { display: block !important; }
    .hive-menu-dropdown a:hover, .hive-menu-dropdown div[onclick]:hover { background: rgba(244,199,95,0.08); border-radius: 4px; }
    .table-wrap::-webkit-scrollbar { height: 10px; display: block; }
    .table-wrap::-webkit-scrollbar-track { background: var(--bg-soft); border-radius: 4px; }
    .table-wrap::-webkit-scrollbar-thumb { background: var(--line); border-radius: 4px; min-width: 40px; }
    .table-wrap::-webkit-scrollbar-thumb:hover { background: var(--muted); }
    .table-wrap.has-scroll { padding-bottom: 4px; border-bottom: 2px solid var(--line); }
    .hive-table { width: 100%; border-collapse: collapse; font-size: 0.82rem; }
    .hive-table th { text-align: center; padding: 10px 12px; border-bottom: 2px solid var(--line); color: var(--muted); font-weight: 600; font-size: 0.75rem; white-space: nowrap; text-transform: uppercase; letter-spacing: 0.5px; }
    .hive-table td { padding: 12px; border-bottom: 1px solid #ffffff0a; vertical-align: middle; text-align: center; }
    .hive-table td:first-child { text-align: left; }
    .hive-table tr:hover td { background: rgba(244,199,95,0.04); }
    /* Admin contact/CRM editor row. The .hive-table td rule above centres every
       cell, which is right for the dense status columns but wrong for a form:
       it centred the labels and the text inside the inputs. Fixed here on the
       panel cell, which owns the form, rather than with !important per field. */
    .contact-panel-cell { text-align: left; }
    .contact-panel-cell input, .contact-panel-cell textarea { text-align: left; }
    .online-dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 6px; }
    .online-dot.on { background: var(--green); box-shadow: 0 0 6px rgba(116,223,154,0.5); }
    .online-dot.off { background: #6b7280; }
    .hive-name { font-weight: 600; color: var(--text); }
    .hive-org { font-size: 0.75rem; color: var(--muted); }

    /* ── Badges ── */
    .role-badge { display: inline-block; padding: 2px 10px; border-radius: 9999px; font-size: 0.7rem; font-weight: 600; }
    .role-owner { background: rgba(244,199,95,0.15); color: var(--amber); border: 1px solid rgba(244,199,95,0.3); }
    .role-read { background: rgba(128,191,255,0.15); color: var(--blue); border: 1px solid rgba(128,191,255,0.3); }
    .role-read-write { background: rgba(116,223,154,0.15); color: var(--green); border: 1px solid rgba(116,223,154,0.3); }
    /* Editable role pill: a <select> styled to match the role badges. Options
       render in the native menu (dark on most platforms); the closed control
       keeps the role color. */
    /* A down-caret drawn as an SVG background hints that the pill is a dropdown.
       stroke=%236b7280 is a neutral gray that reads on every role color; the
       extra right-padding keeps the role text clear of the caret. */
    .role-select { font-weight: 700; -webkit-appearance: none; appearance: none; outline: none;
      padding-right: 20px !important;
      background-image: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='10' height='10' viewBox='0 0 24 24' fill='none' stroke='%236b7280' stroke-width='3' stroke-linecap='round' stroke-linejoin='round'%3E%3Cpolyline points='6 9 12 15 18 9'/%3E%3C/svg%3E");
      background-repeat: no-repeat; background-position: right 7px center; }
    .role-select option { color: initial; background: #fff; }
    .acmm-badge { display: inline-block; padding: 4px 12px; border-radius: 9999px; font-size: 0.7rem; font-weight: 700; white-space: nowrap; cursor: help; }
    .acmm-1 { background: rgba(128,191,255,0.15); color: #80bfff; border: 1px solid rgba(128,191,255,0.3); }
    .acmm-2 { background: rgba(116,223,154,0.15); color: #74df9a; border: 1px solid rgba(116,223,154,0.3); }
    /* L3 gets its own teal so it is distinct from L2's green (they were both #74df9a). */
    .acmm-3 { background: rgba(45,212,191,0.15); color: #2dd4bf; border: 1px solid rgba(45,212,191,0.3); }
    .acmm-4 { background: rgba(244,199,95,0.15); color: #f4c75f; border: 1px solid rgba(244,199,95,0.3); }
    .acmm-5 { background: rgba(255,126,126,0.15); color: #ff7e7e; border: 1px solid rgba(255,126,126,0.3); }
    .acmm-6 { background: rgba(180,130,255,0.15); color: #b482ff; border: 1px solid rgba(180,130,255,0.3); }

    /* ── User-journey stage badges ──
       Severity drives the color so a stalled hive is scannable at a glance:
       green = nothing outstanding, blue = a gentle nudge, amber = overdue,
       red = a de-provision warning has been sent. */
    .journey-badge { display: inline-block; padding: 3px 9px; border-radius: 9999px; font-size: 0.65rem; font-weight: 700; white-space: nowrap; cursor: help; }
    .journey-none { background: rgba(116,223,154,0.12); color: #74df9a; border: 1px solid rgba(116,223,154,0.28); }
    .journey-gentle { background: rgba(128,191,255,0.15); color: #80bfff; border: 1px solid rgba(128,191,255,0.3); }
    .journey-firm { background: rgba(244,199,95,0.15); color: #f4c75f; border: 1px solid rgba(244,199,95,0.3); }
    .journey-deprovision-warning { background: rgba(255,126,126,0.18); color: #ff7e7e; border: 1px solid rgba(255,126,126,0.4); }
    .journey-snoozed { background: rgba(107,114,128,0.15); color: #9ca3af; border: 1px solid rgba(107,114,128,0.3); }
    /* The relay badge marks a hive satisfying stage 2 through human
       contributors rather than an assigned method/model — deliberately its own
       label so such a hive is never silently reported as "assigned". */
    .journey-relay { display: inline-block; margin-left: 4px; padding: 3px 8px; border-radius: 9999px; font-size: 0.62rem; font-weight: 700; white-space: nowrap; cursor: help; background: rgba(45,212,191,0.15); color: #2dd4bf; border: 1px solid rgba(45,212,191,0.3); }

    /* ── Buttons ── */
    .btn-primary { display: inline-flex; align-items: center; justify-content: center; padding: .72rem 1.2rem; background: var(--amber); color: #17110a; font-weight: 800; border-radius: .55rem; border: none; cursor: pointer; font-size: 0.85rem; transition: all .2s; }
    .btn-primary:hover { background: #f8d87a; text-decoration: none; }
    .btn-primary:disabled { opacity: 0.5; cursor: not-allowed; }

    /* ── Toasts & dialogs ── */
    .hive-toast { position: fixed; top: 70px; right: 24px; z-index: 200; padding: 12px 20px; border-radius: 8px; font-size: 0.85rem; max-width: 400px; animation: toast-in 0.3s ease; color: #fff; }
    .hive-toast.success { background: rgba(116,223,154,0.9); }
    .hive-toast.error { background: rgba(255,126,126,0.9); }
    .hive-toast.info { background: rgba(128,191,255,0.9); }
    @keyframes spin { to { transform: rotate(360deg); } }
    @keyframes toast-in { from { transform: translateX(100%); opacity: 0; } to { transform: translateX(0); opacity: 1; } }
    .hive-confirm-overlay { position: fixed; inset: 0; background: rgba(0,0,0,0.6); z-index: 150; display: flex; align-items: center; justify-content: center; }
    .hive-confirm { box-shadow: var(--shadow); background: linear-gradient(#17212df5, #0c1219f5); border: 1px solid #ffffff1a; border-radius: .85rem; padding: 24px; max-width: 400px; width: 90%; }
    .hive-confirm p { color: var(--text); margin-bottom: 16px; font-size: 0.9rem; }
    .hive-confirm-btns { display: flex; gap: 8px; justify-content: flex-end; }
    .empty-state { text-align: center; padding: 48px; color: var(--muted); }
    .dash-link { color: var(--blue); font-size: 0.8rem; }
    /* Access hover panel on the My Hives status dot. Guarded by hover:hover so
       touch devices don't latch it open with no way to dismiss — the row's own
       Manage Access dialog is the path there. */
    @media (hover: hover) {
      .hive-access-wrap:hover .hive-access-pop,
      .hive-access-wrap:focus-within .hive-access-pop { display: block !important; }
    }
    /* The panel is offset 6px below the dot. That gap is outside both the dot
       and the panel, so travelling to the panel's "View all activity" button
       would drop :hover and close it mid-move. This pseudo-element bridges the
       gap with a transparent strip so the pointer never leaves the wrapper.
       Height matches the 6px offset in the panel's inline top. */
    .hive-access-pop::before { content: ''; position: absolute; left: 0; right: 0; top: -6px; height: 6px; }
    /* The wrapper is cursor:help; the one thing inside that is genuinely
       clickable says so. */
    .hover-view-timeline { cursor: pointer; }
    .hover-view-timeline:hover { color: #79c0ff !important; }
    .repo-link { color: var(--blue); font-size: 0.8rem; }
    .hive-name-link { color: #58a6ff; font-weight: 700; text-decoration: none; }
    .hive-name-link:hover { color: #79c0ff; text-decoration: underline; }
    .hive-sub-link { color: #6b7280; font-weight: 400; text-decoration: none; }
    .hive-sub-link:hover { color: #58a6ff; text-decoration: underline; }
    .loading { text-align: center; padding: 32px; color: var(--muted); }

    /* ── Footer ── */
    .footer { border-top: 1px solid var(--line); padding: 2rem clamp(1rem, 4vw, 4.5rem); text-align: center; font-size: .82rem; color: var(--muted); }
    .footer-links { display: flex; justify-content: center; gap: 1.5rem; margin-bottom: .8rem; }
    .footer-links a { color: var(--muted); }
    .footer-links a:hover { color: var(--text); }

    /* ── Responsive ── */
    @media (max-width: 900px) {
      .site-header { grid-template-columns: 1fr; position: static; }
      .site-header nav { flex-wrap: wrap; gap: .85rem; }
      .header-link { justify-self: start; }
      /* Narrow screens stack the tray above the table instead of squeezing the
         grid. The tray stops being an overlay and becomes an in-flow
         disclosure, driven by the same .facet-open class and the same toggle
         button, so behaviour is identical — only the placement differs. */
      .hive-layout { grid-template-columns: minmax(0, 1fr); }
      .facet-rail-tab { flex-direction: row; width: auto; justify-content: center; gap: 8px; padding: 8px 12px; }
      .facet-rail-word { writing-mode: horizontal-tb; }
      .facet-tray { position: static; width: auto; max-height: none; box-shadow: none; margin-top: 8px; }
      .facet-group { margin-bottom: 10px; }
    }
    @media (max-width: 600px) {
      .content { padding: 1.5rem 12px 32px; }
      .site-header { padding: 10px 12px; }
      .brand { font-size: 0.95rem; }
      h1 { font-size: 1.4rem; }
      .table-wrap { overflow-x: auto; -webkit-overflow-scrolling: touch; }
      .hive-table { font-size: 0.72rem; min-width: 600px; }
      .hive-table td, .hive-table th { padding: 8px 6px; }
      .hive-modal { width: 95vw; max-height: 90vh; padding: 20px; }
      .empty-state { padding: 24px; }
      .hive-confirm-btns { flex-direction: column; }
      .hive-confirm-btns button { width: 100%; }
    }
    @media (max-width: 400px) {
      .content { padding: 1rem 8px 24px; }
      .site-header nav { gap: .5rem; font-size: 0.7rem; }
      .hive-modal { padding: 14px; }
      h1 { font-size: 1.2rem; }
    }
  </style>
</head>
<body>
  <header class="site-header">
    <a href="/" class="brand">
      <span class="brand-mark">🐝</span>
      <span>Hive</span>
      <span onclick="window.open(&#39;https://github.com/kubestellar/hive&#39;,&#39;_blank&#39;)" title="Source Code" style="opacity:0.6;margin-left:2px;cursor:pointer;display:inline-flex"><svg width="16" height="16" viewBox="0 0 16 16" fill="currentColor"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg></span>
    </a>
    <nav>
      <a href="/">Hives</a>
      <a href="/learn">Learn</a>
      <a href="/reading">Reading</a>
      <a href="/get-started">Get Started</a>
      <a href="/dashboard" style="color:var(--amber)">My Hives</a>
      <a href="/api/docs" target="_blank">API</a>
      <a href="https://kubestellar.io/docs/hive/overview/introduction" target="_blank" rel="noopener">Docs</a>
      <span id="nav-user" class="nav-user"></span>
    </nav>
    <div class="header-right">
      <span id="hub-version"></span>
      <a href="#" class="header-link" onclick="fetch('/api/auth/logout',{method:'POST'}).then(function(){location.href='/'});return false;">Logout</a>
    </div>
  </header>

  <div class="content">
    <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:24px">
      <div>
        <p class="section-label">Dashboard</p>
        <h1>My Hives</h1>
        <p class="subtitle">Hive instances you own or have access to</p>
        <p id="latest-image-sha" style="font-size:0.7rem;color:var(--muted);margin-top:4px"></p>
      </div>
      <div style="display:flex;gap:8px;align-items:center">
        <button class="btn-primary" id="btn-send-banner-top" style="display:none;background:#d97706" onclick="_bannerTargetHive=null;document.getElementById('banner-modal').style.display='flex';loadBannerHiveList()">Send Banner</button>
        <button class="btn-primary" id="btn-add-hive" disabled onclick="document.getElementById('create-modal').style.display='flex'">+ Add Hosted Hive</button>
      </div>
    </div>

    <div id="provision-request-banner" style="display:none"></div>
    <div id="admin-provision-requests" style="display:none;margin-bottom:24px">
      <h3 style="font-size:1rem;color:var(--accent);margin-bottom:12px">Pending Provision Requests</h3>
      <div id="admin-provision-list"></div>
      <h3 id="past-requests-header" role="button" tabindex="0" aria-expanded="false" aria-controls="past-requests-body"
          onclick="togglePastRequests()" onkeydown="if(event.key==='Enter'||event.key===' '){event.preventDefault();togglePastRequests();}"
          style="font-size:1rem;color:var(--accent);margin:20px 0 10px;cursor:pointer;user-select:none;display:flex;align-items:center;gap:6px">
        <span id="past-requests-toggle" aria-hidden="true">&#9656;</span><span>Past Requests</span>
        <span id="past-requests-count" style="font-size:0.7rem;color:var(--muted);font-weight:400"></span>
      </h3>
      <div id="past-requests-body" style="display:none;overflow-x:auto"><div id="admin-request-history"></div></div>
    </div>

    <div id="hive-drift-summary" style="display:none"></div>
    <div id="fleet-alerts-panel" style="display:none"></div>
    <div id="hive-view-bar" style="display:none"></div>
    <div id="hive-filter-bar" style="display:none"></div>
    <div id="usage-panel" style="display:none;margin-bottom:24px"></div>
    <div id="bulk-action-bar" style="display:none"></div>
    <div class="hive-layout">
      <div class="facet-shell" id="hive-facet-shell">
        <button type="button" class="facet-rail-tab" id="hive-facet-toggle"
                aria-expanded="false" aria-controls="hive-facet-tray"
                title="Show filters">
          <span aria-hidden="true">&#9776;</span>
          <span class="facet-rail-word">Filters</span>
          <span class="facet-active-badge" id="hive-facet-active-badge" style="display:none"></span>
        </button>
        <div class="facet-tray" id="hive-facet-tray">
          <div class="facet-tray-head">
            <span>Filters</span>
            <button type="button" class="facet-tray-close" id="hive-facet-close"
                    title="Hide filters" aria-label="Hide filters">&#10005;</button>
          </div>
          <div id="hive-facet-rail"></div>
        </div>
      </div>
      <div>
        <div id="hive-search-row" style="display:none">
          <input type="text" id="hive-search" placeholder="Search hives — org, repo, cluster, branch, user… (space = OR)" oninput="onHiveSearchInput()" autocomplete="off">
          <button type="button" id="hive-search-clear" class="filter-chip filter-chip-clear" onclick="clearHiveSearch()">Clear</button>
        </div>
        <div id="hives-container"><div class="loading">Loading your hives...</div></div>
      </div>
    </div>

    <div id="public-hives-section" style="display:none;margin-top:48px">
      <h2 style="font-size:1.3rem;color:var(--accent);margin-bottom:16px">Public Hives</h2>
      <div id="public-hives-container"></div>
    </div>

    <div id="admin-section" style="display:none;margin-top:48px">
      <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:16px">
        <h2 id="admin-users-header" role="button" tabindex="0" aria-expanded="false" aria-controls="admin-users-body"
            onclick="toggleAdminUsers()" onkeydown="if(event.key==='Enter'||event.key===' '){event.preventDefault();toggleAdminUsers();}"
            style="font-size:1.3rem;color:var(--accent);margin:0;cursor:pointer;user-select:none;display:flex;align-items:center;gap:8px">
          <span id="admin-users-toggle" aria-hidden="true" style="font-size:0.7rem">&#9656;</span><span>Hub Admin &mdash; Users</span>
          <span id="admin-users-count" style="font-size:0.75rem;color:var(--muted);font-weight:400"></span>
        </h2>
        <input type="text" id="user-search" placeholder="Search users..." oninput="filterUsers()" style="padding:8px 14px;background:var(--surface);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem;width:250px">
      </div>
      <div id="admin-users-body" style="display:none">
        <div id="users-container"><div class="loading">Loading users...</div></div>
      </div>
    </div>

    <div id="hub-banner-section" style="display:none;margin-top:48px">
      <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:16px">
        <h2 style="font-size:1.3rem;color:var(--accent)">Hub Banner</h2>
        <button class="btn-primary" id="btn-clear-banner" style="background:rgba(239,68,68,0.15);color:#f87171;display:none" onclick="clearHubBanner()">Clear All Banners</button>
      </div>
      <div id="active-banner-display" style="display:none;padding:16px;border-radius:8px;border:1px solid var(--border);background:var(--surface);margin-bottom:16px">
        <div style="font-size:0.8rem;color:var(--muted);margin-bottom:8px">Active Banner</div>
        <div id="active-banner-preview" style="padding:12px 16px;border-radius:6px;font-size:0.85rem;margin-bottom:8px"></div>
        <div id="active-banner-targets" style="font-size:0.75rem;color:var(--muted)"></div>
      </div>
    </div>

    <div id="cluster-health-section" style="display:none;margin-top:48px">
      <div onclick="toggleClusterHealth()" style="display:flex;align-items:center;gap:8px;cursor:pointer;user-select:none;margin-bottom:16px">
        <span id="cluster-health-toggle" style="font-size:0.7rem;color:var(--muted);transition:transform 0.2s">&#9654;</span>
        <h2 style="font-size:1.3rem;color:var(--accent);margin:0">Cluster Health</h2>
        <span id="cluster-health-summary-bar" style="font-size:0.8rem;color:var(--muted);margin-left:8px"></span>
      </div>
      <div id="cluster-health-body" style="display:none">
        <div id="cluster-health-grid" style="display:grid;grid-template-columns:repeat(2,1fr);gap:12px"></div>
      </div>
    </div>
  </div>

  <footer class="footer">
    <div class="footer-links">
      <a href="https://github.com/kubestellar/hive">Source Code</a>
      <a href="https://arxiv.org/abs/2604.09388">ACMM Paper</a>
      <a href="https://kubestellar.io">KubeStellar</a>
    </div>
    <p style="color:#3a4555">Hive is an open source project by KubeStellar</p>
  </footer>

  <script>
    function esc(s) { var d = document.createElement('div'); d.textContent = s || ''; return d.innerHTML; }

    /* jsArg renders a string as a quoted JS string literal safe to embed in an
       inline handler attribute. esc() alone is not enough here: it leaves the
       apostrophe intact, and an org or branch name containing one would close
       the handler's quote early. Backslash first (so later escapes are not
       re-escaped), then the quote, then newlines, and finally HTML-escape the
       whole literal because it lands inside an attribute value. */
    function jsArg(s) {
      var lit = "'" + String(s === null || s === undefined ? '' : s)
        .replace(/\\/g, '\\\\')
        .replace(/'/g, "\\'")
        .replace(/\r/g, '\\r')
        .replace(/\n/g, '\\n') + "'";
      return esc(lit);
    }

    // escAttr escapes for a QUOTED ATTRIBUTE value. esc() goes through
    // textContent -> innerHTML, which per spec escapes & < > but NOT quotes, so
    // esc() alone is unsafe between double quotes: a hive name containing a '"'
    // closes the attribute and everything after it is parsed as markup, which
    // is enough to inject an event handler. Hive names and org/repo strings are
    // spoke-reported, i.e. untrusted. Use escAttr for anything interpolated
    // into an attribute; esc() remains correct for text nodes.
    function escAttr(s) { return esc(s).replace(/"/g, '&quot;').replace(/'/g, '&#39;'); }

    /* ---- Clickable user avatars ---------------------------------------
       Every face in this dashboard is a link to that person's GitHub profile.
       The avatar IS the affordance: a separate ↗ glyph or a separately-linked
       username next to it was two controls for one destination, so those are
       gone and the picture carries the click.

       ALWAYS github.com, never a github_host. The account that signs in to the
       hub is a github.com account even when the ORG it works on lives on a
       GitHub Enterprise host — the same reasoning that kept the Past Requests
       user link on github.com while its REPO link is built from the request's
       github_host (see ghRepoURL). A profile link is about the person; the
       repo link is about the instance.

       The username lands in two hostile contexts and gets a different helper in
       each: encodeURIComponent for the URL path segment, escAttr for quoted
       attribute values (esc() leaves quotes intact and a crafted login could
       close the attribute). */
    function ghProfileURL(username) {
      return 'https://github.com/' + encodeURIComponent(String(username || ''));
    }

    /* Wrap avatar markup in a profile anchor.

       display:inline-block with line-height:0 keeps the anchor from adding a
       text baseline's worth of height to whatever row or cell holds it: a bare
       inline anchor inherits the line-height of its parent and can push a dense
       table row taller than its neighbours. The avatars themselves are 16-28px
       inline images and keep their own vertical-align.

       Returns the avatar unwrapped when there is no username to link to (a
       placeholder or a malformed record) — an anchor to a profile that does not
       exist is worse than no anchor.

       label is the accessible name; it also becomes the native tooltip, so any
       role information the caller already showed there is preserved. */
    function avatarProfileLink(username, label, avatarHTML) {
      var uname = String(username || '');
      if (!uname) return avatarHTML;
      return '<a href="' + escAttr(ghProfileURL(uname)) + '" target="_blank" rel="noopener noreferrer" ' +
        'title="' + escAttr(label || uname) + '" aria-label="' + escAttr(label || uname) + '" ' +
        'style="display:inline-block;line-height:0;text-decoration:none">' + avatarHTML + '</a>';
    }

    /* Round avatar <img> for a github.com login, at the given rendered size in
       CSS pixels. Requests 2x from GitHub so the face stays sharp on HiDPI.
       onerror hides a 404 avatar rather than leaving a broken-image glyph — an
       especially important detail now that the image sits inside a link, where
       a broken icon would read as a dead control. extraStyle appends to the
       inline style (borders, flex sizing) and defaults to nothing. */
    var AVATAR_HIDPI_SCALE = 2;
    function avatarImg(username, px, extraStyle) {
      return '<img src="' + escAttr(ghProfileURL(username)) + '.png?size=' + (px * AVATAR_HIDPI_SCALE) + '" alt="" ' +
        'style="width:' + px + 'px;height:' + px + 'px;border-radius:50%;vertical-align:middle;' +
        (extraStyle || '') + '" ' +
        'onerror="this.style.visibility=\'hidden\'">';
    }

    /* The common case: a linked, round avatar for a github.com login. */
    function linkedAvatar(username, px, label, extraStyle) {
      return avatarProfileLink(username, label, avatarImg(username, px, extraStyle));
    }

    /* Rendered avatar sizes, in CSS pixels, one per surface. They differ because
       the surfaces differ in density, not arbitrarily: the status-dot hover
       panel and the compact request/access lists are tight vertical lists; the
       admin Users table row and the pending provision cards have more room. */
    var PANEL_ACCESS_AVATAR_PX = 18;   /* rows inside the status-dot hover panel */
    var LIST_AVATAR_PX = 20;           /* compact request/access lists */
    var TABLE_AVATAR_PX = 24;          /* admin Users table + provision cards */

    // ghRepoURL builds the URL for an org/repo on the RIGHT GitHub instance.
    // Hives are not all on public github.com: a provision request carries a
    // github_host ('' = public github.com, otherwise a GitHub Enterprise host
    // such as github.ibm.com), so the host is taken from the record and never
    // hardcoded. Returns '' when there is no org or no repo to link to, so
    // callers can fall back to plain text rather than emitting a dead link.
    function ghRepoURL(host, org, repo) {
      if (!org || !repo) return '';
      var h = (host || 'github.com').replace(/^https?:\/\//, '').replace(/\/+$/, '');
      // Path segments are encoded; the host is not (it is a hostname, and
      // encoding would break the dots) but is constrained to a hostname shape.
      if (!/^[A-Za-z0-9.-]+$/.test(h)) return '';
      return 'https://' + h + '/' + encodeURIComponent(org) + '/' + encodeURIComponent(repo);
    }

    // sameShaJS: two git SHAs are the same commit even if one is a short
    // prefix of the other. The hub stores 7-char short SHAs while a spoke may
    // report a longer one; a raw === left the "Upgrading" badge spinning
    // forever on a hive already at the target. Mirrors hub sameCommit().
    function sameShaJS(a, b) {
      if (!a || !b) return false;
      var n = Math.min(a.length, b.length);
      return a.slice(0, n).toLowerCase() === b.slice(0, n).toLowerCase();
    }

    function dismissBanner(key, btn) {
      var dismissed = JSON.parse(localStorage.getItem('hive-dismissed-banners') || '{}');
      dismissed[key] = Date.now();
      localStorage.setItem('hive-dismissed-banners', JSON.stringify(dismissed));
      btn.parentNode.remove();
    }

    function hiveToast(msg, type) {
      var t = document.createElement('div');
      t.className = 'hive-toast ' + (type || 'info');
      t.textContent = msg;
      var toastBaseTop = 70;
      var toastGap = 8;
      var existing = document.querySelectorAll('.hive-toast');
      var offset = 0;
      existing.forEach(function(e) { offset += e.offsetHeight + toastGap; });
      t.style.top = (toastBaseTop + offset) + 'px';
      document.body.appendChild(t);
      setTimeout(function() { t.remove(); }, 4000);
    }

    function hiveConfirm(msg, rawHTML) {
      return new Promise(function(resolve) {
        var overlay = document.createElement('div');
        overlay.className = 'hive-confirm-overlay';
        overlay.innerHTML = '<div class="hive-confirm"><p>' + (rawHTML ? msg : esc(msg)) + '</p><div class="hive-confirm-btns">' +
          '<button style="padding:8px 16px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>' +
          '<button style="padding:8px 16px;background:var(--red);color:#fff;border:none;border-radius:6px;cursor:pointer" id="hive-confirm-ok">Confirm</button></div></div>';
        document.body.appendChild(overlay);
        var done = false;
        function finish(val) { if (done) return; done = true; overlay.remove(); document.removeEventListener('keydown', onKey); resolve(val); }
        function onKey(e) { if (e.key === 'Escape') finish(false); if (e.key === 'Enter') finish(true); }
        document.addEventListener('keydown', onKey);
        overlay.querySelector('#hive-confirm-ok').onclick = function() { finish(true); };
        overlay.querySelector('button:first-child').onclick = function() { finish(false); };
        overlay.querySelector('#hive-confirm-ok').focus();
      });
    }

    document.addEventListener('keydown', function(e) {
      if (e.key !== 'Escape') return;
      var createModal = document.getElementById('create-modal');
      if (createModal && createModal.style.display === 'flex') { createModal.style.display = 'none'; return; }
      var accessOverlay = document.querySelector('.hive-confirm-overlay');
      if (accessOverlay) { accessOverlay.remove(); return; }
      var timelineModal = document.getElementById('timeline-modal');
      if (timelineModal && timelineModal.style.display === 'flex') { timelineModal.style.display = 'none'; return; }
      var accessModal = document.getElementById('access-modal');
      if (accessModal && accessModal.style.display === 'flex') { accessModal.style.display = 'none'; }
    });

    var ACMM_LABELS = {1:'L1 Assisted',2:'L2 Instructed',3:'L3 Measured',4:'L4 Adaptive',5:'L5 Semi-Automated',6:'L6 Autonomous'};
    /* One-line "what this level actually does" per ACMM level, hoisted out of
       acmmBadge so the badge tooltip and the Request-a-Hive level picker read
       from the SAME strings. They used to be a local inside acmmBadge, and the
       request modal carried its own hand-written copy that had drifted into
       plain wrong ("L3 CI/CD", "L4 Auto PR", "L6 Fully Autonomous") — L4 in
       particular is NOT fully autonomous, it opens hold-gated PRs for human
       review. Anything describing a level must use this object. */
    var ACMM_TIPS = {1:'L1 Assisted — Advisory only.',2:'L2 Instructed — Advisory beads, no GitHub writes.',3:'L3 Measured — Hold-gated PRs, CI gates.',4:'L4 Adaptive — Agents open issues, sec-check.',5:'L5 Semi-Automated — PRs with hold label, batch review.',6:'L6 Autonomous — Auto-merge on green CI.'};
    /* The tip strings above lead with the level label; the picker renders the
       label separately, so strip the redundant "Lx Name — " prefix there. */
    function acmmTipDetail(level) {
      var tip = ACMM_TIPS[level] || '';
      var dash = tip.indexOf('—');
      return dash >= 0 ? tip.slice(dash + 1).trim() : tip;
    }
    function sparkline(points, color, w, h) {
      if (!points || points.length < 2) return '';
      var vals = points.map(function(p) { return p.v; });
      var mn = Math.min.apply(null, vals);
      var mx = Math.max.apply(null, vals);
      var range = mx - mn || 1;
      var sw = w || 60;
      var sh = h || 16;
      var step = sw / (vals.length - 1);
      var pts = vals.map(function(v, i) {
        return (i * step).toFixed(1) + ',' + (sh - ((v - mn) / range) * sh).toFixed(1);
      }).join(' ');
      return '<svg width="' + sw + '" height="' + sh + '" style="vertical-align:middle;margin-right:4px"><polyline points="' + pts + '" fill="none" stroke="' + (color || '#6b7280') + '" stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round"/></svg>';
    }

    function acmmBadge(level) {
      var l = level || 0;
      return '<span class="acmm-badge acmm-' + l + '" title="' + esc(ACMM_TIPS[l] || '') + '">' + (ACMM_LABELS[l] || 'L' + l) + '</span>';
    }
    /* Classified GitHub App auth states, matching github.AppAuthState.String()
       on the spoke and operatorSideAppStates in journey.go. The two OPERATOR
       states describe a failure the hive OWNER cannot fix: the App private key
       is distributed by the hub, so a missing or mismatched key is our problem,
       not theirs. The UI must never present these as something the user should
       install or reconfigure. */
    var GH_APP_STATE_KEY_MISSING = 'key-missing';
    var GH_APP_STATE_KEY_INVALID = 'key-invalid';
    var GH_APP_OPERATOR_STATES = {};
    GH_APP_OPERATOR_STATES[GH_APP_STATE_KEY_MISSING] = true;
    GH_APP_OPERATOR_STATES[GH_APP_STATE_KEY_INVALID] = true;
    /* An unreported or unrecognised state is deliberately NOT operator-side:
       a spoke too old to classify keeps the existing behaviour. */
    function ghAppIsOperatorSide(state) {
      return GH_APP_OPERATOR_STATES[String(state || '').trim()] === true;
    }

    /* Labels for each journey stage, matching JourneyStage.String() on the hub. */
    var JOURNEY_STAGE_LABELS = {
      'none': 'On track',
      'github-app': 'GitHub App',
      'method-model': 'Method/model',
      'acmm-level': 'ACMM',
    };
    /* What each stage means, for the hover tooltip. */
    var JOURNEY_STAGE_TIPS = {
      'none': 'No outstanding adoption steps.',
      'github-app': 'Stage 1 — the GitHub App is not installed, so no agent can open issues or PRs.',
      'method-model': 'Stage 2 — no agent has a method/model assigned and ClankeR (the contributor relay) is not in use.',
      'acmm-level': 'Stage 3 — running steadily; due a gentle suggestion to consider the next ACMM level. Never a de-provision risk.',
    };
    /* Rank a journey status for sorting. Snoozed hives rank lowest — an admin
       has already dealt with them — then on-track, then by escalation. */
    var JOURNEY_SEVERITY_RANK = {'none': 0, 'gentle': 1, 'firm': 2, 'deprovision-warning': 3};
    function journeySortValue(j) {
      if (!j) return -1;
      if (j.snoozed) return -1;
      var sev = JOURNEY_SEVERITY_RANK[j.severity || 'none'] || 0;
      /* SEVERITY_SPAN spaces the stage ranks apart so severity orders within a
         stage without ever bleeding into the next stage's band. */
      var SEVERITY_SPAN = 10;
      return (j.stageNum || 0) * SEVERITY_SPAN + sev;
    }
    function journeyBadge(j) {
      if (!j) return '<span style="color:var(--muted)">—</span>';
      var stage = j.stage || 'none';
      var label = JOURNEY_STAGE_LABELS[stage] || stage;
      var tip = JOURNEY_STAGE_TIPS[stage] || '';
      if (j.stalledFor && stage !== 'none') tip += ' Stalled for ' + j.stalledFor + '.';
      /* A snoozed hive shows as snoozed regardless of stage — an admin has
         exempted it, so it must not read as an active problem. */
      var cls, text;
      if (j.snoozed) {
        cls = 'journey-snoozed';
        text = label + ' (snoozed)';
        tip = 'Nudges snoozed by an admin' + (j.snoozedUntil ? ' until ' + j.snoozedUntil : '') + '. ' + tip;
      } else {
        var sev = j.severity || 'none';
        cls = 'journey-' + (sev === 'none' ? 'none' : sev);
        text = label;
        if (sev === 'deprovision-warning') text = label + ' ⚠';
      }
      var html = '<span class="journey-badge ' + cls + '" title="' + esc(tip) + '">' + esc(text) + '</span>';
      /* Stage 2 satisfied via ClankeR gets its own visible badge. */
      if (j.viaRelay) {
        html += '<span class="journey-relay" title="' + esc('Stage 2 is satisfied through ClankeR, the contributor relay (human contributors picking up tasks), not by an assigned method/model.') + '">relay</span>';
      }
      return html;
    }
    function roleBadge(role) {
      var cls = role === 'owner' ? 'role-owner' : role === 'read-write' ? 'role-read-write' : 'role-read';
      return '<span class="role-badge ' + cls + '">' + esc(role) + '</span>';
    }

    /* ---- Inline hive access avatars -----------------------------------
       The status-dot hover panel (healthBadge) is the DETAIL view of who can
       reach a hive: avatar + username + role, one per line. These inline
       avatars are the scannable SUMMARY of the same h.access list, rendered in
       the name cell so "who else is on this hive" is answerable without
       hovering 30 rows one at a time. Both read the same server-built list
       (accessForHive), so they can never disagree about membership.

       Role colours live here and are reused by the hover panel's rows so a
       face in the row and its line in the panel read as the same role. */
    var ACCESS_ROLE_COLORS = {'owner': '#d29922', 'read-write': '#3fb950'};
    var ACCESS_ROLE_COLOR_DEFAULT = '#6b7280';
    function accessRoleColor(role) {
      return ACCESS_ROLE_COLORS[role] || ACCESS_ROLE_COLOR_DEFAULT;
    }

    /* Faces shown inline before collapsing the rest into a "+N" chip. The hive
       table is 16 columns and already dense; four 16px faces plus the chip fit
       beside the role badge without widening the name column, and a hive with
       a dozen members must not push the row wide. The full list stays one
       hover away on the status dot. */
    var INLINE_ACCESS_AVATAR_MAX = 4;
    /* Rendered size of an inline face, in CSS pixels — small enough to sit on
       the name cell's second line beside the role badge. */
    var INLINE_ACCESS_AVATAR_PX = 16;
    /* The pixel size requested from GitHub is INLINE_ACCESS_AVATAR_PX *
       AVATAR_HIDPI_SCALE, applied by the shared avatarImg() helper so every
       face in the dashboard stays sharp on HiDPI by the same rule. */

    /* Everyone with access to h EXCEPT the signed-in viewer. Their own
       membership is implied by the row being visible to them, so showing their
       own face on every row would be noise. GitHub logins are case-insensitive,
       hence the lowercased compare. Entries with no username are dropped: they
       would render as a permanently broken image.

       Returns [] for placeholder (unassigned) rows — a pool slot nobody has
       been granted has no meaningful membership to summarise — and for rows
       where the server withheld the access list (h.access is only populated for
       rows the caller owns, or for the hub admin). */
    function otherHiveMembers(h) {
      if (!h || isPlaceholderHive(h)) return [];
      var out = [];
      var list = (h.access || []);
      for (var i = 0; i < list.length; i++) {
        var a = list[i] || {};
        var uname = String(a.username || '');
        if (!uname) continue;
        if (_currentUser && uname.toLowerCase() === _currentUser) continue;
        out.push(a);
      }
      return out;
    }

    /* One inline face, linked to that user's GitHub profile. Carries a native
       title ONLY — deliberately no custom hover panel on this element. The
       status dot owns the one custom panel in this row (see healthBadge and
       TestSingleHoverPanelInvariant); an element with both draws the browser
       tooltip on top of the panel, which is the overlap bug that was fixed once
       already. The title moves to the anchor, which is still a plain native
       tooltip, not a panel.

       The role stays in the tooltip so the face still answers "who and at what
       permission" on hover, exactly as before it became a link. */
    function inlineAccessAvatar(a) {
      var uname = String(a.username || '');
      var role = String(a.role || '');
      return linkedAvatar(uname, INLINE_ACCESS_AVATAR_PX, uname + (role ? ' — ' + role : ''),
        'border:1px solid ' + accessRoleColor(role) + ';background:var(--surface);flex:0 0 auto');
    }

    /* Inline summary of the OTHER users on this hive, or '' when there are
       none. Returning the empty string (rather than an empty container) matters:
       an empty wrapper would still occupy its gap and margin and make rows with
       no co-members sit a few pixels wider than their neighbours. */
    function hiveAccessAvatars(h) {
      var members = otherHiveMembers(h);
      if (!members.length) return '';
      var shown = members.slice(0, INLINE_ACCESS_AVATAR_MAX);
      var faces = '';
      for (var i = 0; i < shown.length; i++) faces += inlineAccessAvatar(shown[i]);
      var overflow = members.length - shown.length;
      if (overflow > 0) {
        /* The +N chip names the people it hides, so the hidden members are
           still identifiable without opening the hover panel. */
        var hiddenNames = [];
        for (var j = shown.length; j < members.length; j++) {
          hiddenNames.push(String(members[j].username || ''));
        }
        faces += '<span title="' + escAttr(hiddenNames.join(', ')) + '" ' +
          'style="font-size:0.62rem;color:var(--muted);font-weight:600;white-space:nowrap;cursor:help">+' + overflow + '</span>';
      }
      var label = members.length === 1
        ? '1 other user with access'
        : members.length + ' other users with access';
      return '<span class="hive-access-faces" aria-label="' + escAttr(label) + '" ' +
        'style="display:inline-flex;align-items:center;gap:2px;margin-left:6px;vertical-align:middle">' + faces + '</span>';
    }
    function fmtTokens(n) {
      n = Number(n) || 0;
      if (n <= 0) return '<span style="color:var(--muted)">—</span>';
      if (n >= 1e9) return (n / 1e9).toFixed(1).replace(/\.0$/, '') + 'B';
      if (n >= 1e6) return (n / 1e6).toFixed(1).replace(/\.0$/, '') + 'M';
      if (n >= 1e3) return (n / 1e3).toFixed(1).replace(/\.0$/, '') + 'K';
      return String(n);
    }
    function clusterBadge(clusterId, clusterName) {
      var cid = clusterId || 'hive-oke';
      var isGPU = cid === 'vllm-d' || (clusterName || '').toLowerCase().indexOf('gpu') >= 0;
      var bg = isGPU ? 'rgba(34,197,94,0.15)' : 'rgba(59,130,246,0.15)';
      var color = isGPU ? '#3fb950' : '#60a5fa';
      var border = isGPU ? 'rgba(34,197,94,0.3)' : 'rgba(59,130,246,0.3)';
      var title = clusterName || cid;
      return '<span class="cluster-badge" style="display:inline-block;padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:' + bg + ';color:' + color + ';border:1px solid ' + border + '" title="' + esc(title) + '">' + esc(cid) + '</span>';
    }
    function modeBadge(mode) {
      var m = (mode || 'idle').toUpperCase();
      var levels = {IDLE:0, QUIET:1, BUSY:2, SURGE:3};
      var colors = {IDLE:'#6b7280', QUIET:'#3b82f6', BUSY:'#f59e0b', SURGE:'#ef4444'};
      var fill = levels[m] !== undefined ? levels[m] : 0;
      var c = colors[m] || '#6b7280';
      var bars = '';
      for (var i = 0; i < 4; i++) {
        var h = 6 + i * 4;
        var bc = i <= fill ? c : '#1e1e2e';
        bars += '<rect x="' + (i * 6) + '" y="' + (20 - h) + '" width="4" height="' + h + '" rx="1" fill="' + bc + '"/>';
      }
      return '<span title="' + m + '" style="display:inline-flex;align-items:center;gap:4px"><svg width="24" height="20" viewBox="0 0 24 20">' + bars + '</svg><span style="font-size:0.7rem;color:' + c + ';font-weight:600">' + m + '</span></span>';
    }
    /* Status-filter keys for the My Hives list. Kept as named constants so the
       filter chips, the classifier and the persisted selection can never drift
       apart on a typo'd string literal. */
    var HIVE_FILTER_APP_MISSING = 'app-missing';
    var HIVE_FILTER_NO_TOKENS = 'no-tokens';
    var HIVE_FILTER_DEGRADED = 'degraded';
    var HIVE_FILTER_OK = 'ok';

    /* A hive has "used no tokens" when its last heartbeat reported a cumulative
       token total of zero — the same value the Tokens column renders as "—". */
    var NO_TOKENS_THRESHOLD = 0;

    /* hiveStatusFlags derives the four filterable states from data the hub
       ALREADY has per hive (all reported over the spoke heartbeat):
         - githubAppRequired / githubAppPermIssue  (RegistryEntry, server.go)
         - health.status                            (RegistryEntry.Health)
         - totalTokens24h                           (RegistryEntry)
         - online                                   (RegistryEntry)
       The GitHub-App and degraded rules deliberately mirror healthBadge()
       exactly, so a row's dot and the chip it matches can never disagree. */
    function hiveStatusFlags(h) {
      h = h || {};
      var hp = h.health || {};
      var st = hp.status || 'unknown';
      /* githubAppRequired means the spoke wants the App but it is not usable:
         with a perm issue it is installed-but-insufficient, without one it is
         not installed at all. healthBadge() forces "degraded" in both cases. */
      var appMissing = !!h.githubAppRequired && !h.githubAppPermIssue;
      var degraded = !!h.githubAppRequired || st === 'degraded' || st === 'critical';
      /* An offline hive has no live reading at all, so it is not "OK" even if
         its last stored health snapshot said ok. */
      var ok = !degraded && st === 'ok' && !!h.online;
      return {
        appMissing: appMissing,
        noTokens: (Number(h.totalTokens24h) || 0) <= NO_TOKENS_THRESHOLD,
        degraded: degraded,
        ok: ok
      };
    }

    // uptimeCell renders process uptime for the Uptime column.
    //
    // This used to be an inline pill next to the hive name, which wrapped long
    // org names onto a second line. In its own column it can always show a
    // value, so the column reads as real data rather than an occasional
    // warning — but the COLOUR still carries the signal, since a hive can sit
    // at 1/1 Running while restarting every few minutes and otherwise look
    // perfectly healthy.
    function uptimeCell(h) {
      // Restart just happened; red. Matches the old inline pill's threshold.
      var RECENT_RESTART_SECS = 600;   // 10 min
      // Below this, still worth noticing — the pod has not settled yet.
      var SETTLING_SECS = 3600;        // 1 hour
      var SHOW_SECONDS_BELOW = 90;     // finer granularity while very fresh
      var HOUR_SECS = 3600, DAY_SECS = 86400;
      if (!h.startedAt) return '<span style="color:var(--muted)">—</span>';
      var secs = (Date.now() - new Date(h.startedAt).getTime()) / 1000;
      if (!isFinite(secs) || secs < 0) return '<span style="color:var(--muted)">—</span>';
      var label;
      if (secs < SHOW_SECONDS_BELOW) label = Math.round(secs) + 's';
      else if (secs < HOUR_SECS) label = Math.round(secs / 60) + 'm';
      else if (secs < DAY_SECS) label = (secs / HOUR_SECS).toFixed(1).replace(/\.0$/, '') + 'h';
      else label = Math.floor(secs / DAY_SECS) + 'd';
      var c = secs < RECENT_RESTART_SECS ? 'var(--red)'
            : (secs < SETTLING_SECS ? 'var(--yellow, #d29922)' : 'var(--muted)');
      var title = secs < SETTLING_SECS
        ? 'Restarted recently — a short value that keeps resetting means the pod is restarting'
        : 'Process uptime since the last restart';
      return '<span title="' + esc(title) + '" style="font-size:0.72rem;color:' + c + ';cursor:help">' + esc(label) + '</span>';
    }
    /* ---- Per-check health detail -------------------------------------------
       Health.checks[] arrives over the heartbeat as free-form JSON decoded into
       map[string]any on the hub (RegistryEntry.Health, server.go). It may be
       absent, null, an empty array, or hold entries missing name/status/detail.
       Every accessor below therefore guards each field individually: one
       malformed spoke payload must never blank the whole table. */

    /* Check statuses that count as a FAILURE for the inline row summary, the
       hover grouping and the per-check filter. 'warn' is included because an
       operator scanning for "what is wrong here" wants warnings surfaced too;
       'skip' is not a problem and 'pass' obviously is not. */
    var FAILING_CHECK_STATUSES = {fail: true, warn: true, critical: true, error: true};

    /* How many failing check NAMES to spell out inline in the row before
       collapsing to a bare count. The name column is already three lines deep,
       so one name is all that fits without pushing the row taller. */
    var INLINE_FAILING_CHECK_NAMES = 1;

    /* Longest failing-check name rendered inline before it is ellipsised.
       Check names are spoke-supplied and unbounded. */
    var INLINE_CHECK_NAME_MAXLEN = 22;

    /* healthChecks returns the hive's checks as a clean array of
       {name, status, detail} strings — never null, never holding non-objects. */
    function healthChecks(h) {
      var hp = (h && h.health) || {};
      var raw = hp.checks;
      if (!raw || !raw.length) return [];
      var out = [];
      for (var i = 0; i < raw.length; i++) {
        var ck = raw[i];
        if (!ck || typeof ck !== 'object') continue;
        var nm = typeof ck.name === 'string' ? ck.name : '';
        if (!nm) continue;   /* an unnamed check cannot be shown or filtered on */
        out.push({
          name: nm,
          status: typeof ck.status === 'string' ? ck.status : 'unknown',
          detail: typeof ck.detail === 'string' ? ck.detail : ''
        });
      }
      return out;
    }

    /* failingChecks returns only the checks in a failing/warning state. */
    function failingChecks(h) {
      return healthChecks(h).filter(function(ck) { return !!FAILING_CHECK_STATUSES[ck.status]; });
    }

    /* failingCheckSummary renders the compact in-row failure summary: the name
       of the single failing check, or "N checks failing" when there are more.
       Returns '' when nothing is failing, so healthy rows stay exactly as
       dense as they are today. */
    function failingCheckSummary(h) {
      var bad = failingChecks(h);
      if (!bad.length) return '';
      var label;
      if (bad.length <= INLINE_FAILING_CHECK_NAMES) {
        var nm = bad[0].name;
        if (nm.length > INLINE_CHECK_NAME_MAXLEN) nm = nm.substring(0, INLINE_CHECK_NAME_MAXLEN - 1) + '…';
        label = nm;
      } else {
        label = bad.length + ' checks failing';
      }
      /* Any 'fail'-class check is red; a row that is only warning stays amber so
         the pill colour agrees with the row dot. */
      var anyHard = bad.some(function(ck) { return ck.status !== 'warn'; });
      var col = anyHard ? '#f85149' : '#d29922';
      var full = bad.map(function(ck) {
        return ck.name + (ck.detail ? ': ' + ck.detail : '');
      }).join('\n');
      /* One element, one tooltip source: this pill owns a plain title and never
         also carries a custom panel (that is healthBadge()'s job). */
      return '<span title="' + esc(full) + '" style="display:inline-block;margin-left:6px;padding:0 6px;' +
        'border-radius:9999px;font-size:0.62rem;font-weight:600;line-height:1.5;cursor:help;white-space:nowrap;' +
        'color:' + col + ';background:' + (anyHard ? 'rgba(248,81,73,0.12)' : 'rgba(210,153,34,0.12)') + ';' +
        'border:1px solid ' + (anyHard ? 'rgba(248,81,73,0.35)' : 'rgba(210,153,34,0.35)') + '">' +
        esc(label) + '</span>';
    }

    /* advisoryStaleSummary renders a small "stale advisory" pill next to the
       failing-check pill when the hub has flagged this hive's advisory digest as
       stale. The flag (h.advisoryStale) and its reason are computed server-side
       by advisoryStale() — gated on advisory-mode participation AND app-can-write
       AND past-threshold — so this renderer never re-derives the rule and a
       not-installed or PR-mode hive is already filtered out before it gets here.
       Amber, matching the "worth noticing" band of the uptime pill: a stale
       digest is an operator nudge, not a hard row failure. */
    function advisoryStaleSummary(h) {
      if (!h || !h.advisoryStale) return '';
      var col = '#d29922';
      var title = h.advisoryStaleReason || 'Advisory digest has gone stale';
      return '<span title="' + esc(title) + '" style="display:inline-block;margin-left:6px;padding:0 6px;' +
        'border-radius:9999px;font-size:0.62rem;font-weight:600;line-height:1.5;cursor:help;white-space:nowrap;' +
        'color:' + col + ';background:rgba(210,153,34,0.12);border:1px solid rgba(210,153,34,0.35)">' +
        'stale advisory</span>';
    }

    /* deadLinkSummary renders a "dead link" pill when this hive's own public
       URL did not serve on the last several probes. Red, not amber: unlike a
       stale digest this is user-facing breakage — the link in this very row
       returns an error page, and because the spoke itself is healthy and
       heartbeating, NOTHING else on the row indicates a problem.
       h.urlUnreachable and its reason are computed server-side from the same
       alert list the panel renders (see urlUnreachableFacet), gated on
       consecutive failures AND minimum age AND cluster-outage suppression, so
       this renderer never re-derives the rule and a converging or
       whole-cluster-out hive is filtered out before it gets here. */
    function deadLinkSummary(h) {
      if (!h || !h.urlUnreachable) return '';
      var title = h.urlUnreachableReason || 'This hive public URL did not respond';
      return '<span title="' + esc(title) + '" style="display:inline-block;margin-left:6px;padding:0 6px;' +
        'border-radius:9999px;font-size:0.62rem;font-weight:600;line-height:1.5;cursor:help;white-space:nowrap;' +
        'color:#f85149;background:rgba(248,81,73,0.12);border:1px solid rgba(248,81,73,0.35)">' +
        'dead link</span>';
    }

    /* ---- ClankeR, the contributor relay (hover panel) ------------------
       The relay is a first-class SUBSTITUTE for assigning a method/model:
       when humans are picking up this hive's tasks through /contribute, the
       "tokens for an agent" requirement is satisfied. The product rule is
       that this must read as its OWN state — never silently folded into
       "assigned"/"OK" — so an operator can see the requirement is being met
       *via the relay*.

       The signal is h.journey.viaRelay, computed on the hub by relayInUse()
       (pkg/hub/journey.go) and already shipped on every My Hives row for the
       Journey column's "relay" badge. Reading the SAME field here is what
       keeps the two surfaces from ever disagreeing about one hive — there is
       no second, browser-side relay rule to drift out of sync. */

    /* Teal, matching the .journey-relay badge, so "relay" reads as the same
       thing in the Journey column and in this panel. */
    var RELAY_COLOR = '#2dd4bf';

    /* relayLine renders the relay state plus how many contributors are behind
       it. ContributorCount is the durable registered-profile count; active is
       the live-WebSocket count, which legitimately drops to zero when nobody
       is connected right now — so the registered count is what's headlined and
       the active count is only added when it is non-zero. */
    function relayLine(h) {
      var j = h.journey || {};
      if (!j.viaRelay) return '';
      var registered = h.contributorCount || 0;
      var active = h.activeContributors || 0;
      var who;
      if (registered > 0) {
        who = registered === 1 ? '1 contributor' : registered + ' contributors';
        if (active > 0) who += ', ' + active + ' active';
      } else if (active > 0) {
        who = active === 1 ? '1 active contributor' : active + ' active contributors';
      } else {
        /* viaRelay with no counts means the signal came from a leaderboard
           entry with a task in flight; say so rather than printing "0". */
        who = 'task in progress';
      }
      return '<div style="padding:1px 0;color:' + RELAY_COLOR + '">' +
        esc('◆ ClankeR · ' + who) + '</div>' +
        '<div style="padding:0 0 1px 12px;color:var(--muted);font-size:0.65rem">' +
        esc('satisfies the method/model requirement') + '</div>';
    }

    /* ---- Recent activity (hover panel) --------------------------------
       Events ride the My Hives payload as h.recentEvents (owner/admin only,
       see MyHiveEntry.RecentEvents) rather than being fetched on hover, so
       sweeping the pointer down the table costs zero requests. The full
       history stays in the timeline modal. */

    /* How many events the panel renders. The server already trims to
       myHivesRecentEventCount; this is the browser-side guard so an older or
       hand-crafted payload can never stretch the panel. */
    var HOVER_EVENT_COUNT = 3;

    /* An event detail is a full sentence; the panel is 300px wide at most, so
       long details are clipped to keep one event on one line. */
    var HOVER_EVENT_DETAIL_MAXLEN = 48;

    function hoverEventRows(h) {
      var events = (h.recentEvents || []).slice(0, HOVER_EVENT_COUNT);
      if (!events.length) return '';
      var rows = events.map(function(ev) {
        var kind = TIMELINE_KINDS[ev.kind] || { label: ev.kind || 'Event', color: '#8b949e' };
        var detail = ev.detail || '';
        if (detail.length > HOVER_EVENT_DETAIL_MAXLEN) {
          detail = detail.substring(0, HOVER_EVENT_DETAIL_MAXLEN - 1) + '…';
        }
        return '<div style="display:flex;gap:6px;align-items:baseline;padding:1px 0">' +
          '<span style="flex:0 0 auto;color:' + kind.color + ';font-size:0.6rem;font-weight:600">' + esc(kind.label) + '</span>' +
          '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--muted)">' + esc(detail) + '</span>' +
          '<span style="flex:0 0 auto;color:var(--muted);font-size:0.6rem">' + esc(timelineAgo(ev.ts)) + '</span>' +
          '</div>';
      }).join('');
      /* "View all" opens the existing 200-event modal. It is a data-attribute
         button read by a delegated listener, NOT an inline onclick: a hive name
         is spoke-reported and esc() escapes neither quotes nor apostrophes, so
         building a handler string from it would be an injection. The values
         land in quoted attributes, so they go through escAttr(), not esc(). */
      var viewAll = '<div style="padding:3px 0 0"><button type="button" class="hover-view-timeline" ' +
        'data-hive-id="' + escAttr(h.id) + '" data-hive-name="' + escAttr(h.name || h.id) + '" ' +
        'style="background:none;border:none;padding:0;color:var(--blue);font-size:0.62rem;cursor:pointer;text-decoration:underline">' +
        'View all activity</button></div>';
      return '<span style="display:block;border-top:1px solid var(--border);margin:6px 0 4px"></span>' +
        '<span style="display:block;color:var(--muted);font-size:0.62rem;text-transform:uppercase;letter-spacing:0.04em;margin-bottom:4px">Recent activity</span>' +
        rows + viewAll;
    }

    function healthBadge(h) {
      var hp = h.health || {};
      var st = hp.status || 'unknown';
      var colors = {ok:'#3fb950',warning:'#d29922',degraded:'#f85149',critical:'#ff4040',unknown:'#6b7280'};
      var icons = {ok:'✓',warning:'⚠',degraded:'⚠',critical:'✕',unknown:'?'};
      var checkIcons = {pass:'✓',fail:'✕',warn:'⚠',skip:'–'};
      var c = colors[st] || colors.unknown;
      var ic = icons[st] || '?';
      var isUpgrading = _upgradingHives[h.id];
      var statusLabel = isUpgrading ? 'Starting up after upgrade' : st.charAt(0).toUpperCase() + st.slice(1);
      /* Checks, failures first, so the reason for a bad status reads before the
         wall of passing checks. Stable within each group (spoke report order). */
      var checks = healthChecks(h).slice().sort(function(a, b) {
        var fa = FAILING_CHECK_STATUSES[a.status] ? 0 : 1;
        var fb = FAILING_CHECK_STATUSES[b.status] ? 0 : 1;
        return fa - fb;
      });
      var lines = [statusLabel];
      for (var i = 0; i < checks.length; i++) {
        var ck = checks[i];
        var ci = checkIcons[ck.status] || '?';
        var line = ci + ' ' + ck.name;
        if (ck.detail) line += ': ' + ck.detail;
        lines.push(line);
      }
      if (h.githubAppRequired && ghAppIsOperatorSide(h.githubAppState)) {
        /* Operator-side: the key we distribute has not landed, or is for the
           wrong App. Still degraded — the hive genuinely cannot work — but the
           hover must name the real cause so an admin does not chase the user
           about an installation that is already correct. */
        lines.push(h.githubAppState === GH_APP_STATE_KEY_INVALID
          ? '⚠ GitHub App: key does not match the App (operator must push the correct key)'
          : '⚠ GitHub App: credentials not yet delivered by the hub (operator action)');
        st = 'degraded'; c = colors.degraded; ic = icons.degraded; statusLabel = 'Degraded'; lines[0] = statusLabel;
      }
      else if (h.githubAppRequired && h.githubAppPermIssue) { lines.push('✓ GitHub App installed'); lines.push('⚠ GitHub App: permissions insufficient'); st = 'degraded'; c = colors.degraded; ic = icons.degraded; statusLabel = 'Degraded'; lines[0] = statusLabel; }
      else if (h.githubAppRequired) { lines.push('✕ GitHub App not installed'); st = 'degraded'; c = colors.degraded; ic = icons.degraded; statusLabel = 'Degraded'; lines[0] = statusLabel; }
      else if (!h.githubAppRequired) { lines.push('✓ GitHub App installed'); }
      if (!checks.length) lines.push('No check data');
      // Heartbeat freshness — so a reading that is minutes old isn't mistaken
      // for current health (the source of "stuck Degraded" reports: the last
      // heartbeat carried a transient failure and never got refreshed).
      if (h.lastHeartbeat) {
        var ageMs = Date.now() - new Date(h.lastHeartbeat).getTime();
        if (!isNaN(ageMs) && ageMs >= 0) {
          var ageMin = Math.floor(ageMs / 60000);
          var ageStr = ageMin < 1 ? 'just now' : (ageMin === 1 ? '1 min ago' : ageMin + ' min ago');
          lines.push('— as of ' + ageStr);
          // A reading older than 2× the heartbeat interval is stale; don't
          // present it as a live status.
          var staleAfterMs = 5 * 60000;
          if (ageMs > staleAfterMs && st !== 'unknown') {
            statusLabel = statusLabel + ' (stale)';
            lines[0] = statusLabel;
            c = colors.warning; ic = icons.warning;
          }
        }
      }
      var access = h.access || [];
      var dotMarkup = '<span style="display:inline-block;width:8px;height:8px;border-radius:50%;background:' + c + '"></span>' +
        '<span style="font-size:0.7rem;color:' + c + ';font-weight:600">' + ic + '</span>';

      // No access list (not an owner of this row): nothing to render beyond the
      // health lines, so the native title tooltip is enough and cheapest.
      //
      // recentEvents is populated under the SAME owner/admin rule as access, so
      // a row without an access list has no events either — there is nothing to
      // consolidate here and this branch stays a plain title. The relay state is
      // carried as a text line so this branch still reports it, rather than the
      // relay being visible only to owners.
      if (!access.length) {
        var plainLines = lines.slice();
        if ((h.journey || {}).viaRelay) {
          plainLines.push('◆ ClankeR (contributor relay) in use — satisfies the method/model requirement');
        }
        return '<span title="' + esc(plainLines.join('\n')) + '" style="display:inline-flex;align-items:center;gap:4px;cursor:help;white-space:pre-line">' + dotMarkup + '</span>';
      }

      // ONE panel holding health AND access. Setting a title attribute here too
      // would make the browser draw its own tooltip on top of this panel — two
      // overlapping boxes saying different things, which is exactly what the
      // first cut of this did.
      var healthRows = (lines || []).map(function(l, i) {
        // lines[0] is the status word ("Warning", "Degraded", ...); render it as
        // the panel's title in the status colour, the rest as check lines.
        if (i === 0) {
          return '<span style="display:block;color:' + c + ';font-weight:600;margin-bottom:4px">' + esc(l) + '</span>';
        }
        /* Failing check lines (built above with ✕/⚠ icons) are lifted out of the
           muted grey so the reason for the status is the first thing read. The
           lines are already sorted failures-first. */
        var lc = 'var(--muted)';
        if (l.indexOf('✕') === 0) lc = '#f85149';
        else if (l.indexOf('⚠') === 0) lc = '#d29922';
        return '<div style="padding:1px 0;color:' + lc + '">' + esc(l) + '</div>';
      }).join('');

      var accessRows = access.map(function(a) {
        /* Shared with the inline row faces (accessRoleColor) so a face in the
           name cell and its line in this panel read as the same role. */
        var rc = accessRoleColor(a.role);
        /* The face links to the profile. The title lives on the ANCHOR, which is
           a plain native tooltip inside the panel — the invariant that matters
           is that no title sits on the panel's own root element (see
           TestSingleHoverPanelInvariant), not that the panel's contents are
           tooltip-free. */
        return '<div style="display:flex;align-items:center;gap:6px;padding:2px 0">' +
          linkedAvatar(a.username, PANEL_ACCESS_AVATAR_PX,
            String(a.username || '') + (a.role ? ' — ' + a.role : ''), 'flex:0 0 auto') +
          '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap">' + esc(a.username) + '</span>' +
          '<span style="color:' + rc + ';font-size:0.62rem;font-weight:600;white-space:nowrap">' + esc(a.role) + '</span>' +
          '</div>';
      }).join('');
      var heading = access.length === 1 ? '1 user with access' : access.length + ' users with access';

      return '<span class="hive-access-wrap" style="position:relative;display:inline-flex;align-items:center;gap:4px;cursor:help">' + dotMarkup +
        '<span class="hive-access-pop" style="display:none;position:absolute;left:0;top:calc(100% + 6px);z-index:60;' +
        'min-width:210px;max-width:300px;padding:8px 10px;border-radius:8px;border:1px solid var(--border);' +
        'background:var(--surface);box-shadow:0 6px 20px rgba(0,0,0,0.35);font-size:0.72rem;text-align:left;font-weight:400;white-space:normal">' +
        healthRows +
        relayLine(h) +
        '<span style="display:block;border-top:1px solid var(--border);margin:6px 0 4px"></span>' +
        '<span style="display:block;color:var(--muted);font-size:0.62rem;text-transform:uppercase;letter-spacing:0.04em;margin-bottom:4px">' + esc(heading) + '</span>' +
        accessRows +
        hoverEventRows(h) + '</span></span>';
    }
    /* ---- Config drift -------------------------------------------------
       The server computes drift signals per hive (pkg/hub/drift.go) and ships
       them on the My Hives payload as h.drift = {signals, count, worstSeverity}.
       Everything below only RENDERS that; no drift rule lives in the browser,
       so the badge, the summary and any future consumer can never disagree
       about what counts as drift. */

    /* Palette reused from healthBadge() so a critical drift signal reads as the
       same red as a critical health dot. */
    var DRIFT_SEVERITY_COLORS = {info: '#58a6ff', warn: '#d29922', critical: '#f85149'};
    var DRIFT_SEVERITY_LABELS = {info: 'Info', warn: 'Warning', critical: 'Critical'};
    /* Display order, worst first, for the fleet-exceptions breakdown. */
    var DRIFT_SEVERITY_ORDER = ['critical', 'warn', 'info'];

    /* Human labels for the server's stable drift kind identifiers. A kind with
       no entry here falls back to its raw identifier rather than rendering
       blank, so a new server-side signal is still legible before the UI
       catches up. */
    var DRIFT_KIND_LABELS = {
      'version-behind':  'Version differs from fleet',
      'branch-mismatch': 'Branch differs from fleet',
      'pinned-image':    'Pinned to an immutable image',
      'heartbeat-stale': 'Heartbeat stale',
      'app-missing':     'GitHub App not installed',
      'app-perm-issue':  'GitHub App permissions',
      'app-creds-operator': 'GitHub App key (operator)',
      'app-id-placeholder': 'Placeholder App ID (operator)',
      'health-degraded': 'Health degraded',
      'upgrade-stuck':   'Upgrade stuck',
      'acmm-unset':      'ACMM level unset',
      'no-agents':       'No agents running'
    };

    function driftKindLabel(kind) { return DRIFT_KIND_LABELS[kind] || kind || 'Unknown'; }

    /* driftOf normalizes the server's report so every caller can treat signals
       as an array and count as a number, even for a row the server predates
       (h.drift undefined) or a payload that arrived malformed. */
    function driftOf(h) {
      var d = (h && h.drift) || {};
      var signals = Array.isArray(d.signals) ? d.signals : [];
      return {
        signals: signals,
        count: typeof d.count === 'number' ? d.count : signals.length,
        worstSeverity: d.worstSeverity || ''
      };
    }

    /* driftBadge renders the Drift column: the signal count coloured by the
       worst severity, with a hover panel listing every reason.

       It follows healthBadge()'s ONE-panel rule exactly: a custom hover panel
       and NO title attribute on the same element. Setting both makes the
       browser draw its native tooltip on top of the panel — two overlapping
       boxes saying different things. */
    function driftBadge(h) {
      var d = driftOf(h);
      if (!d.count) {
        return '<span title="No configuration drift detected" style="color:var(--muted);font-size:0.72rem;cursor:help">—</span>';
      }
      var c = DRIFT_SEVERITY_COLORS[d.worstSeverity] || DRIFT_SEVERITY_COLORS.info;

      var rows = (d.signals || []).map(function(s) {
        s = s || {};
        var sc = DRIFT_SEVERITY_COLORS[s.severity] || DRIFT_SEVERITY_COLORS.info;
        var sevLabel = DRIFT_SEVERITY_LABELS[s.severity] || s.severity || 'Info';
        return '<div style="padding:4px 0;border-top:1px solid var(--border)">' +
          '<div style="display:flex;align-items:center;gap:6px;margin-bottom:2px">' +
          '<span style="display:inline-block;width:6px;height:6px;border-radius:50%;background:' + sc + ';flex:0 0 auto"></span>' +
          '<span style="color:' + sc + ';font-weight:600">' + esc(driftKindLabel(s.kind)) + '</span>' +
          '<span style="margin-left:auto;color:var(--muted);font-size:0.6rem;text-transform:uppercase;letter-spacing:0.04em">' + esc(sevLabel) + '</span>' +
          '</div>' +
          '<div style="color:var(--muted);line-height:1.4">' + esc(s.reason || '') + '</div>' +
          '</div>';
      }).join('');

      var heading = d.count === 1 ? '1 drift signal' : d.count + ' drift signals';

      return '<span class="hive-access-wrap" style="position:relative;display:inline-flex;align-items:center;gap:4px;cursor:help">' +
        '<span style="display:inline-block;min-width:18px;padding:1px 6px;border-radius:9999px;font-size:0.65rem;font-weight:700;' +
        'color:' + c + ';background:rgba(255,255,255,0.04);border:1px solid ' + c + '">' + d.count + '</span>' +
        '<span class="hive-access-pop" style="display:none;position:absolute;right:0;top:calc(100% + 6px);z-index:60;' +
        'min-width:260px;max-width:380px;padding:8px 10px;border-radius:8px;border:1px solid var(--border);' +
        'background:var(--surface);box-shadow:0 6px 20px rgba(0,0,0,0.35);font-size:0.72rem;text-align:left;font-weight:400;white-space:normal">' +
        '<span style="display:block;color:' + c + ';font-weight:600;margin-bottom:2px">' + esc(heading) + '</span>' +
        rows + '</span></span>';
    }

    function dashboardLink(h) {
      var isHosted = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-')));
      // Open hosted spokes via the hub's SSO handoff endpoint so the user's hub
      // login follows them to the spoke (no second GitHub login), including for
      // direct-route/firewalled spokes that the hub nginx can't front. The
      // endpoint mints a signed, single-hive token and 302s to the spoke's /sso;
      // if SSO can't be used it falls back to the plain dashboard URL. The
      // visible label still shows the spoke host.
      if (isHosted && h.id) {
        var ssoHref = '/api/saas/hives/' + encodeURIComponent(h.id) + '/open';
        var label = (h.dashboardUrl && !h.dashboardUrl.includes('localhost'))
          ? h.dashboardUrl.replace(/^https?:\/\//, '').substring(0, 30) + '...'
          : esc(h.id) + '.hive...';
        return '<a href="' + ssoHref + '" target="_blank" class="dash-link">' + esc(label) + '</a>';
      }
      if (h.dashboardUrl && !h.dashboardUrl.includes('localhost'))
        return '<a href="' + esc(h.dashboardUrl) + '" target="_blank" class="dash-link">' + esc(h.dashboardUrl.replace(/^https?:\/\//,'').substring(0,30)) + '...</a>';
      return '<span style="color:var(--muted);font-size:0.75rem">—</span>';
    }
    function snapshotLink(h) {
      if (h.snapshotUrl) return '<a href="' + esc(h.snapshotUrl) + '" target="_blank" class="dash-link">↗</a>';
      return '';
    }
    function apiLink(h) {
      var isHosted = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-')));
      var base = '';
      if (h.dashboardUrl && !h.dashboardUrl.includes('localhost')) {
        base = esc(h.dashboardUrl);
      } else if (isHosted) {
        base = 'https://' + esc(h.id) + '.hive.kubestellar.io';
      }
      if (!base) return '';
      return '<a href="' + base + '/api/docs" target="_blank" style="padding:3px 10px;background:rgba(88,166,255,0.15);color:#58a6ff;border:1px solid rgba(88,166,255,0.3);border-radius:4px;font-size:0.7rem;text-decoration:none;white-space:nowrap">API ↗</a>';
    }
    function resolvedBase(h) {
      if (h.dashboardUrl && !h.dashboardUrl.includes('localhost')) return h.dashboardUrl;
      var isH = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-')));
      if (isH) return 'https://' + h.id + '.hive.kubestellar.io';
      return '';
    }

    /* SSO_NAV_ATTR marks an anchor whose href is a DISPLAY url (the friendly
       vanity host) but whose click must actually go through the hub's /open
       handoff. The real navigation target is carried here rather than in href
       because browsers show the raw href in the status bar on hover, and the
       placeholder-id /open path is exactly what we do not want a user to read
       there. Named so the delegated click listener below and the builder agree
       on one string. */
    var SSO_NAV_ATTR = 'data-sso-open';

    /* ssoDisplayLink builds the hive-name anchor.

       href      = displayUrl (vanity) when we have one, so the status bar, the
                   context menu's "Copy link address" and middle-click all show
                   and use the friendly host.
       data attr = openUrl, the hub /open endpoint that mints the 90s HMAC SSO
                   handoff token. A plain left-click is intercepted and sent
                   there instead, so the normal path still lands authenticated.

       The cmd-click / middle-click divergence is DELIBERATE and safe: the spoke
       serves a real "Sign in with GitHub" device-flow page at its root, so a
       token-less arrival on the vanity host is an ordinary login, not a dead
       end. Trading a rare extra sign-in for a readable URL on every hover is
       the intended bargain.

       When displayUrl is empty (unclaimed placeholder, or vanity adoption
       skipped) there is nothing friendlier to show, so href stays the /open
       url and behavior is byte-for-byte what it was before. */
    function ssoDisplayLink(openUrl, displayUrl, text, cls) {
      var shown = displayUrl || openUrl;
      var navAttr = displayUrl ? ' ' + SSO_NAV_ATTR + '="' + esc(openUrl) + '"' : '';
      return '<a href="' + esc(shown) + '" target="_blank"' + navAttr +
        ' class="' + cls + '" title="Open dashboard">' + esc(text) + '</a>';
    }

    /* One delegated listener rather than per-row handlers: the table is
       re-rendered wholesale on every refresh, so inline handlers would be
       re-attached constantly. Only a plain primary-button click is taken over;
       cmd/ctrl/shift/alt-click and middle-click fall through to the browser so
       "open in new tab" keeps its native meaning (landing on the vanity login
       page, per the note above). */
    document.addEventListener('click', function(ev) {
      var a = ev.target && ev.target.closest ? ev.target.closest('[' + SSO_NAV_ATTR + ']') : null;
      if (!a) return;
      if (ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
      var target = a.getAttribute(SSO_NAV_ATTR);
      if (!target) return;
      ev.preventDefault();
      window.open(target, '_blank');
    });

    async function loadUser() {
      try {
        var resp = await fetch('/api/auth/user');
        var data = await resp.json();
        if (data.authenticated) {
          _isAdmin = !!data.hub_admin;
          /* The signed-in login is the ONE piece of viewer identity the hive
             rows need: the inline access avatars omit the viewer, whose own
             membership is implied by the row being visible at all. Stored
             lowercased because GitHub usernames are case-insensitive and the
             roster and the auth payload can disagree on casing. */
          _currentUser = String(data.login || '').toLowerCase();
          var roleText = data.hub_admin ? 'Hub Admin' : 'User';
          /* The viewer's own face links to their own profile, like every other
             face in the dashboard. avatar_url comes from the auth payload (it is
             GitHub's CDN URL, not derivable from the login), so this one builds
             its <img> directly rather than via avatarImg — but the anchor and
             the role tooltip are the shared ones. */
          document.getElementById('nav-user').innerHTML =
            avatarProfileLink(data.login, String(data.login || '') + ' — ' + roleText,
              '<img src="' + escAttr(data.avatar_url) + '" class="nav-avatar" alt="" ' +
              'onerror="this.style.visibility=\'hidden\'">') +
            '<span style="font-size:0.85rem">' + esc(data.login) + '</span>' +
            '<span style="font-size:0.65rem;color:var(--muted);margin-left:6px">' + roleText + '</span>';
        }
      } catch(e) {}
    }

    var _userQuota = 0, _userUsed = 0, _isAdmin = false;
    /* Lowercased login of the signed-in viewer, set by loadUser(). Empty until
       that resolves, and empty is safe: hiveAccessAvatars() then simply omits
       nobody, which is a cosmetic over-render for one paint rather than a
       leak — every name it can show was already in h.access. */
    var _currentUser = '';
    var _latestSHA = '';
    var _latestSHAs = {};
    var _latestSHAMessages = {};
    var _latestImageStatus = {};
    var _trackedBranchesList = [];
    var _clusterList = [];
    var _commitMessages = {};
    var _allDashHives = [];
    /* Seeded to the A-Z default rather than '' (registry/arrival order) so the
       very first paint — including paintCachedHives, which runs before any
       network call — is already alphabetical. loadHiveSortPrefs overwrites both
       from localStorage when the operator has a stored choice; see
       HIVE_SORT_DEFAULT_KEY for why a stored choice wins over this default. */
    var _dashSortKey = 'name', _dashSortAsc = true;
    var _hivesLoading = false;
    var _lastHivesJSON = '';
    var _lastUsersJSON = '';

    /* ── Instant-paint cache for the hive list ──
       A returning operator used to stare at "Loading your hives..." for the
       whole round-trip. We persist the last good list and paint it on load,
       then reconcile when the fresh payload lands.

       Bounded and versioned on purpose:
       - HIVES_CACHE_TTL_MS keeps a stale fleet from being presented as current.
       - HIVES_CACHE_VERSION is bumped whenever the row shape changes, so an
         old-shaped cache can never be rendered by new code (that would
         reintroduce exactly the class of render crash this fixes).
       Reads NEVER throw: storage can be disabled, full, or hold junk, and none
       of that may block the network path. */
    var LS_HIVES_CACHE = 'hive-my-hives-cache';
    /* Bump on ANY change to the hive row shape consumed by renderHives(). */
    var HIVES_CACHE_VERSION = 1;
    /* 10 minutes: long enough to cover a reload or a tab restore, short enough
       that a cached fleet is never wildly out of date before the poll lands. */
    var HIVES_CACHE_TTL_MS = 10 * 60 * 1000;
    /* Cap what we persist. Only the fields renderHives() needs for the first
       paint are stored, but a huge fleet could still overflow the ~5 MB quota
       and make every write fail. */
    var HIVES_CACHE_MAX_ROWS = 200;

    function readHivesCache() {
      try {
        var raw = window.localStorage.getItem(LS_HIVES_CACHE);
        if (!raw) return null;
        var c = JSON.parse(raw);
        if (!c || c.version !== HIVES_CACHE_VERSION) return null;
        if (!c.savedAt || (Date.now() - c.savedAt) > HIVES_CACHE_TTL_MS) return null;
        if (!Array.isArray(c.hives) || !c.hives.length) return null;
        return c;
      } catch (e) {
        /* Disabled/!full/corrupt storage must never break the load path. */
        console.warn('[hive] hive cache read failed, using network only:', e);
        return null;
      }
    }

    function writeHivesCache(hives) {
      try {
        var rows = (hives || []).slice(0, HIVES_CACHE_MAX_ROWS);
        window.localStorage.setItem(LS_HIVES_CACHE, JSON.stringify({
          version: HIVES_CACHE_VERSION,
          savedAt: Date.now(),
          hives: rows
        }));
      } catch (e) {
        /* Quota errors are expected on large fleets — caching is best-effort. */
        console.warn('[hive] hive cache write failed:', e);
      }
    }

    /* Paint the cached fleet before the first request returns. Guarded so any
       failure (including a stale-shaped row slipping past the version check)
       falls back to the normal spinner + network path rather than killing
       init(). */
    function paintCachedHives() {
      var c = readHivesCache();
      if (!c) return false;
      try {
        _allDashHives = c.hives;
        _hiveRegistry = c.hives;
        renderHives(sortedDashHives(), true);
        return true;
      } catch (e) {
        console.error('[hive] cached render failed, falling back to network:', e);
        _allDashHives = [];
        _hiveRegistry = [];
        _lastHivesJSON = '';
        var container = document.getElementById('hives-container');
        if (container) container.innerHTML = '<div class="loading">Loading your hives...</div>';
        return false;
      }
    }

    /* Active status filters, keyed by HIVE_FILTER_*. Multi-select: several may
       be on at once and a hive matches if it satisfies ANY active one (OR), so
       turning on more chips widens the list rather than narrowing it to nothing
       — "Degraded" and "OK" are mutually exclusive and an AND would always be
       empty. Empty object = no filtering, show everything. */
    var _dashStatusFilters = {};

    /* Chip definitions, in display order. Colours reuse the health palette from
       healthBadge() so a chip reads as the same state as the row dot. */
    var HIVE_FILTER_CHIPS = [
      {key: HIVE_FILTER_APP_MISSING, label: 'GitHub App not installed', color: '#f85149'},
      {key: HIVE_FILTER_NO_TOKENS, label: 'No tokens used', color: '#6b7280'},
      {key: HIVE_FILTER_DEGRADED, label: 'Degraded', color: '#f85149'},
      {key: HIVE_FILTER_OK, label: 'OK', color: '#3fb950'}
    ];

    /* Active failing-check-name filter: '' = off, otherwise a check name that a
       hive must be FAILING for it to be shown. This is an AND against the status
       chips (chips OR among themselves), because "degraded hives" and
       "github_auth is failing" are two different questions and the useful answer
       is their intersection. */
    var _dashFailingCheckFilter = '';

    /* Max distinct failing check names offered in the picker. Names come from
       spokes, so an unbounded fleet could otherwise produce a huge menu. */
    var MAX_FAILING_CHECK_FILTER_OPTIONS = 12;

    /* If a single check is failing on at least this many hives, it is called out
       as a fleet-wide signal rather than reading as per-hive noise. */
    var FLEET_CHECK_SIGNAL_MIN_HIVES = 2;

    /* failingCheckCounts tallies, over the hives given, how many have each check
       in a failing state — {name: hiveCount}. Callers pass the ASSIGNED set so
       placeholders never contribute. */
    function failingCheckCounts(hives) {
      var counts = {};
      (hives || []).forEach(function(h) {
        var seen = {};
        failingChecks(h).forEach(function(ck) {
          if (seen[ck.name]) return;   /* count each hive once per check name */
          seen[ck.name] = true;
          counts[ck.name] = (counts[ck.name] || 0) + 1;
        });
      });
      return counts;
    }

    /* setFailingCheckFilter selects (or clears, with '') the check-name filter. */
    function setFailingCheckFilter(name) {
      _dashFailingCheckFilter = name || '';
      renderHives(_allDashHives, true);
    }

    /* The drift-kind currently selected in the fleet-exceptions summary, or ''
       for none. Single-select (unlike the multi-select status chips): the
       summary's purpose is "show me the hives with THIS problem", and an
       OR across several problem types is what the status chips already do.
       It composes with the status chips by AND — the chips answer "which
       states", this answers "which specific misconfiguration". */
    var _dashDriftFilter = '';

    /* hiveMatchesDriftFilter answers whether a hive carries the selected drift
       kind. Guarded so a row with no drift report never throws. */
    function hiveMatchesDriftFilter(h) {
      if (!_dashDriftFilter) return true;
      var signals = driftOf(h).signals;
      for (var i = 0; i < signals.length; i++) {
        if (signals[i] && signals[i].kind === _dashDriftFilter) return true;
      }
      return false;
    }

    /* ── Upgrade-state filter ──
       Which upgrade state the list is narrowed to, or '' for none. Single-
       select like the drift filter, not multi-select: "upgrading" and "queued"
       are successive stages of one lifecycle, so a hive is in exactly one of
       them and an OR across both would just mean "any pending upgrade" — which
       is what clearing the filter already shows.

       This exists so an operator can WATCH an upgrade roll across the fleet,
       and — the reason it was asked for — pull up every wedged hive at once
       when a rollout stalls. */
    var UPGRADE_FILTER_UPGRADING = 'upgrading';
    var UPGRADE_FILTER_QUEUED = 'queued';
    var _dashUpgradeFilter = '';

    /* hiveUpgradeState classifies a hive as 'upgrading', 'queued' or '' (in
       neither state).

       The definitions mirror the Version cell's render conditions on purpose,
       so the facet count and the rows can never disagree:
         upgrading — the hub says an upgrade is in flight (h.upgrading).
         queued    — auto-upgrade is on and the hive is BEHIND latest, but the
                     hub has not instructed the upgrade yet. This is the state
                     with no upgradeStartedAt, which is precisely why the
                     elapsed counter must not claim to know one.
       'upgrading' is tested FIRST because an in-flight upgrade outranks a
       queued one; without that ordering a hive would be double-counted. */
    function hiveUpgradeState(h) {
      if (!h) return '';
      if (h.upgrading) return UPGRADE_FILTER_UPGRADING;
      if (h.autoUpgrade && h.upgradeTarget) return UPGRADE_FILTER_QUEUED;
      return '';
    }

    /* hiveMatchesUpgradeFilter answers whether a hive survives the upgrade-
       state filter. Composes by AND with the status chips and facets. */
    function hiveMatchesUpgradeFilter(h) {
      if (!_dashUpgradeFilter) return true;
      return hiveUpgradeState(h) === _dashUpgradeFilter;
    }

    /* ── Stale-advisory filter ──
       When true, the list is narrowed to hives whose advisory digest the hub
       has flagged stale. A single boolean, not a value set: the underlying
       signal is itself boolean, so there is no second value to OR against.
       Composes by AND with the status chips and the facets, like the drift
       filter above. */
    var _dashAdvisoryStaleFilter = false;

    /* hiveMatchesAdvisoryStaleFilter answers whether a hive survives the
       stale-advisory filter. Reads the SERVER-computed h.advisoryStale flag —
       the browser must never re-derive the threshold or the advisory-mode /
       app-can-write gating, or the filter and the row pill would drift apart
       (see advisoryStale() in advisory_staleness.go). */
    function hiveMatchesAdvisoryStaleFilter(h) {
      if (!_dashAdvisoryStaleFilter) return true;
      return !!(h && h.advisoryStale);
    }

    /* ────────────────────────────────────────────────────────────────────────
       Grouping and saved views for My Hives.

       Grouping applies WITHIN the existing Assigned/Unassigned split — that
       split is the outer structure and survives untouched. A group-by turns
       each side into several labelled, collapsible subsections.
       ──────────────────────────────────────────────────────────────────────── */

    /* Group-by dimension keys. Named constants so the <select>, the persisted
       value, the grouper and the collapse-state keys can never drift on a
       typo'd string literal. */
    var HIVE_GROUP_NONE = 'none';
    var HIVE_GROUP_CLUSTER = 'cluster';
    var HIVE_GROUP_ORG = 'org';
    var HIVE_GROUP_OWNER = 'owner';
    var HIVE_GROUP_ACMM = 'acmm';
    var HIVE_GROUP_BRANCH = 'branch';

    /* Label shown for a hive whose grouping field is empty/unreported. Kept as
       one constant so every dimension buckets blanks identically. */
    var HIVE_GROUP_UNKNOWN_LABEL = 'Unspecified';

    /* localStorage keys. Prefix matches the existing convention in this file
       ('hive-dismissed-banners', 'hive-cluster-health-collapsed'). */
    var LS_HIVE_GROUP_BY = 'hive-group-by';
    var LS_HIVE_GROUP_COLLAPSED = 'hive-group-collapsed';
    var LS_HIVE_SAVED_VIEWS = 'hive-saved-views';
    var LS_HIVE_DEFAULT_VIEW = 'hive-default-view';
    var LS_HIVE_SORT = 'hive-sort';
    var LS_HIVE_SCROLL = 'hive-scroll';

    /* HIVE_SORT_DEFAULT_KEY is the sort a FIRST-TIME visitor gets: the Hive
       column, ascending — i.e. plain A-Z over the label the name cell actually
       displays (see hiveNameSortValue / hiveLabel). Before this the table
       rendered in registry order, which is arrival order and looks arbitrary.

       This is a DEFAULT, not an override: loadHiveSortPrefs applies it only
       when there is no usable stored choice, so an operator who sorted by
       Tokens still returns to Tokens. */
    var HIVE_SORT_DEFAULT_KEY = 'name';
    var HIVE_SORT_DEFAULT_ASC = true;

    /* HIVE_SORT_KEYS is every key a header can sort by — the allowlist that a
       persisted value is validated against. A stored key that is no longer a
       column (a renamed field, a hand-edited value, a downgrade) degrades to
       the A-Z default instead of silently sorting on an absent property, which
       reads as "sorting is broken" because every row compares equal. */
    var HIVE_SORT_KEYS = ['name', 'clusterId', 'startedAt', 'acmmLevel', 'aiAuthor',
      'journey', 'agentCount', 'totalTokens24h', 'governorMode', 'actionableIssues',
      'actionablePRs', 'activeContributors', 'registeredAt'];

    function isHiveSortKey(key) {
      for (var i = 0; i < (HIVE_SORT_KEYS || []).length; i++) {
        if (HIVE_SORT_KEYS[i] === key) return true;
      }
      return false;
    }

    /* Cap on stored saved views. localStorage is a small shared budget and an
       unbounded list would let one runaway page fill it for every other
       feature on this origin. */
    var HIVE_SAVED_VIEWS_MAX = 50;

    /* Cap on a saved-view name, so the picker stays readable and a pasted wall
       of text cannot blow the storage budget on its own. */
    var HIVE_VIEW_NAME_MAX_LEN = 60;

    /* Group-by dimension definitions, in <select> order.
       - key:   stable identifier, persisted
       - label: shown in the control
       - of(h): the group label for a hive; '' means "unspecified"
       - sort:  optional comparator over group labels; default is locale sort */
    var HIVE_GROUP_DIMENSIONS = [
      {key: HIVE_GROUP_NONE, label: 'No grouping', of: function() { return ''; }},
      {key: HIVE_GROUP_CLUSTER, label: 'Cluster', of: function(h) { return (h && (h.clusterName || h.clusterId)) || ''; }},
      {key: HIVE_GROUP_ORG, label: 'Org', of: function(h) { return (h && h.org) || ''; }},
      {key: HIVE_GROUP_OWNER, label: 'Owner', of: function(h) { return (h && h.owner) || ''; }},
      {key: HIVE_GROUP_ACMM, label: 'ACMM level', of: function(h) {
        /* acmmLevel is numeric; render as "Level N" so the header reads as a
           label rather than a bare digit. 0/absent falls through to
           HIVE_GROUP_UNKNOWN_LABEL like every other dimension. */
        var lvl = h && h.acmmLevel;
        return lvl ? 'Level ' + lvl : '';
      }, sort: function(a, b) {
        /* Numeric order, so Level 10 sorts after Level 2 rather than before it.
           Non-"Level N" labels (i.e. Unspecified) sort last. */
        var na = _acmmGroupOrder(a), nb = _acmmGroupOrder(b);
        return na - nb;
      }},
      {key: HIVE_GROUP_BRANCH, label: 'Branch', of: function(h) { return (h && h.gitBranch) || ''; }}
    ];

    /* Sort weight for an ACMM group label. "Level N" → N; anything else (the
       Unspecified bucket) sorts after every real level. */
    var ACMM_GROUP_UNKNOWN_ORDER = Number.MAX_SAFE_INTEGER;
    function _acmmGroupOrder(label) {
      var m = /^Level (\d+)$/.exec(label || '');
      return m ? parseInt(m[1], 10) : ACMM_GROUP_UNKNOWN_ORDER;
    }

    /* Active group-by key, and per-group collapse state as {groupId: true}
       where true means COLLAPSED (absent = expanded, so a fresh browser shows
       everything open). */
    var _dashGroupBy = HIVE_GROUP_NONE;
    var _dashGroupCollapsed = {};

    /* Saved views: [{name, groupBy, filters, sortKey, sortAsc}]. Client-side
       only — a view is a personal lens over data the page already has, so it
       needs no server round-trip and no server state to go stale. */
    var _dashSavedViews = [];
    var _dashDefaultView = '';

    /* lsGetJSON reads and parses a JSON value from localStorage. localStorage
       can hold anything a previous version (or another tab, or a user poking
       at devtools) left behind, and can be disabled or full outright — so every
       failure falls back to the caller's default rather than throwing and
       blanking the page. */
    function lsGetJSON(key, fallback) {
      try {
        var raw = localStorage.getItem(key);
        if (!raw) return fallback;
        var v = JSON.parse(raw);
        return (v === null || v === undefined) ? fallback : v;
      } catch (e) { return fallback; }
    }

    /* lsSetJSON persists a JSON value, swallowing quota/disabled errors: losing
       a preference is not worth breaking the render. */
    function lsSetJSON(key, value) {
      try { localStorage.setItem(key, JSON.stringify(value)); } catch (e) { /* storage full or disabled */ }
    }

    /* isGroupByKey validates a persisted/incoming group-by against the known
       dimensions, so a stale or hand-edited value degrades to "no grouping"
       instead of silently grouping by nothing. */
    function isGroupByKey(key) {
      for (var i = 0; i < (HIVE_GROUP_DIMENSIONS || []).length; i++) {
        if (HIVE_GROUP_DIMENSIONS[i].key === key) return true;
      }
      return false;
    }

    /* groupDimension returns the dimension definition for a key, or the
       "none" entry when unknown. */
    function groupDimension(key) {
      for (var i = 0; i < (HIVE_GROUP_DIMENSIONS || []).length; i++) {
        if (HIVE_GROUP_DIMENSIONS[i].key === key) return HIVE_GROUP_DIMENSIONS[i];
      }
      return HIVE_GROUP_DIMENSIONS[0];
    }

    /* sanitizeSavedView coerces one stored entry into a well-formed view, or
       returns null if it is unusable. Everything in localStorage is untrusted
       input. */
    function sanitizeSavedView(v) {
      if (!v || typeof v !== 'object') return null;
      var name = typeof v.name === 'string' ? v.name.trim() : '';
      if (!name) return null;
      var filters = {};
      if (v.filters && typeof v.filters === 'object') {
        for (var k in v.filters) {
          if (Object.prototype.hasOwnProperty.call(v.filters, k) && v.filters[k]) filters[k] = true;
        }
      }
      return {
        name: name.slice(0, HIVE_VIEW_NAME_MAX_LEN),
        groupBy: isGroupByKey(v.groupBy) ? v.groupBy : HIVE_GROUP_NONE,
        filters: filters,
        sortKey: typeof v.sortKey === 'string' ? v.sortKey : '',
        sortAsc: v.sortAsc !== false
      };
    }

    /* loadDashViewPrefs restores group-by, collapse state, saved views and the
       default view from localStorage. Called once before the first render. */
    function loadDashViewPrefs() {
      var gb = null;
      try { gb = localStorage.getItem(LS_HIVE_GROUP_BY); } catch (e) { gb = null; }
      _dashGroupBy = isGroupByKey(gb) ? gb : HIVE_GROUP_NONE;

      var collapsed = lsGetJSON(LS_HIVE_GROUP_COLLAPSED, {});
      _dashGroupCollapsed = {};
      if (collapsed && typeof collapsed === 'object') {
        for (var ck in collapsed) {
          if (Object.prototype.hasOwnProperty.call(collapsed, ck) && collapsed[ck]) _dashGroupCollapsed[ck] = true;
        }
      }

      var views = lsGetJSON(LS_HIVE_SAVED_VIEWS, []);
      _dashSavedViews = [];
      if (Object.prototype.toString.call(views) === '[object Array]') {
        for (var vi = 0; vi < views.length && _dashSavedViews.length < HIVE_SAVED_VIEWS_MAX; vi++) {
          var sv = sanitizeSavedView(views[vi]);
          if (sv) _dashSavedViews.push(sv);
        }
      }

      var def = null;
      try { def = localStorage.getItem(LS_HIVE_DEFAULT_VIEW); } catch (e) { def = null; }
      _dashDefaultView = (typeof def === 'string' && findSavedView(def)) ? def : '';
      if (_dashDefaultView) applySavedView(_dashDefaultView, true);
    }

    /* findSavedView looks a view up by name (names are the identity — the
       picker shows them and rename edits them in place). */
    function findSavedView(name) {
      for (var i = 0; i < (_dashSavedViews || []).length; i++) {
        if (_dashSavedViews[i].name === name) return _dashSavedViews[i];
      }
      return null;
    }

    function persistSavedViews() { lsSetJSON(LS_HIVE_SAVED_VIEWS, _dashSavedViews); }
    function persistGroupCollapsed() { lsSetJSON(LS_HIVE_GROUP_COLLAPSED, _dashGroupCollapsed); }

    /* groupIdFor builds the stable identity used both as the collapse-state
       localStorage key and as the DOM id for a group's rows. Scoped by
       dimension and by section (assigned/unassigned) so the same org name in
       two sections collapses independently, and so switching dimensions does
       not inherit an unrelated group's collapse state. */
    function groupIdFor(section, label) {
      return _dashGroupBy + '|' + section + '|' + label;
    }

    /* setDashGroupBy switches the grouping dimension and re-renders. */
    function setDashGroupBy(key) {
      _dashGroupBy = isGroupByKey(key) ? key : HIVE_GROUP_NONE;
      try { localStorage.setItem(LS_HIVE_GROUP_BY, _dashGroupBy); } catch (e) { /* storage full or disabled */ }
      renderHives(_allDashHives, true);
    }

    /* toggleDashGroup flips one group's collapse state. */
    function toggleDashGroup(groupId) {
      if (_dashGroupCollapsed[groupId]) delete _dashGroupCollapsed[groupId];
      else _dashGroupCollapsed[groupId] = true;
      persistGroupCollapsed();
      renderHives(_allDashHives, true);
    }

    /* groupHives buckets hives by the active dimension, preserving the incoming
       order within each bucket (the caller has already applied sortDashHives).
       Returns [{label, id, hives}] with empty buckets impossible by
       construction — a group only exists because a hive landed in it, so no
       empty headers can render. */
    function groupHives(hives, section) {
      var dim = groupDimension(_dashGroupBy);
      var order = [], byLabel = {};
      for (var i = 0; i < (hives || []).length; i++) {
        var h = hives[i];
        var label = dim.of(h) || HIVE_GROUP_UNKNOWN_LABEL;
        if (!Object.prototype.hasOwnProperty.call(byLabel, label)) { byLabel[label] = []; order.push(label); }
        byLabel[label].push(h);
      }
      order.sort(dim.sort || function(a, b) { return String(a).localeCompare(String(b)); });
      var out = [];
      for (var oi = 0; oi < order.length; oi++) {
        out.push({label: order[oi], id: groupIdFor(section, order[oi]), hives: byLabel[order[oi]]});
      }
      return out;
    }

    /* ── Saved views ── */

    /* currentViewState snapshots the parts of the UI a saved view captures.
       Search terms and facets are not on v2 yet; when the search/facets branch
       lands, extend this one function and sanitizeSavedView to match. */
    function currentViewState() {
      var filters = {};
      for (var k in (_dashStatusFilters || {})) {
        if (Object.prototype.hasOwnProperty.call(_dashStatusFilters, k) && _dashStatusFilters[k]) filters[k] = true;
      }
      return {groupBy: _dashGroupBy, filters: filters, sortKey: _dashSortKey, sortAsc: _dashSortAsc};
    }

    /* saveCurrentView names and stores the current group-by + filters + sort.
       Saving over an existing name overwrites it, which is what "save" means
       when the picker already shows that name. */
    function saveCurrentView() {
      var raw = window.prompt('Name this view:', '');
      if (raw === null) return;
      var name = String(raw).trim().slice(0, HIVE_VIEW_NAME_MAX_LEN);
      if (!name) { hiveToast('View name cannot be empty', 'error'); return; }
      var state = currentViewState();
      state.name = name;
      var existing = findSavedView(name);
      if (existing) {
        if (!window.confirm('A view named "' + name + '" already exists. Overwrite it?')) return;
        existing.groupBy = state.groupBy;
        existing.filters = state.filters;
        existing.sortKey = state.sortKey;
        existing.sortAsc = state.sortAsc;
      } else {
        if (_dashSavedViews.length >= HIVE_SAVED_VIEWS_MAX) {
          hiveToast('Saved-view limit reached (' + HIVE_SAVED_VIEWS_MAX + '). Delete one first.', 'error');
          return;
        }
        _dashSavedViews.push(state);
      }
      persistSavedViews();
      _dashActiveView = name;
      renderHives(_allDashHives, true);
      hiveToast('View "' + name + '" saved', 'success');
    }

    /* Name of the view most recently applied or saved, for the picker's
       selected option. Not persisted: it is a UI cursor, not a preference. */
    var _dashActiveView = '';

    /* applySavedView restores a stored view. quiet=true suppresses the
       re-render, for the load path where the caller renders once afterwards. */
    function applySavedView(name, quiet) {
      var v = findSavedView(name);
      if (!v) return;
      _dashGroupBy = isGroupByKey(v.groupBy) ? v.groupBy : HIVE_GROUP_NONE;
      _dashStatusFilters = {};
      for (var k in (v.filters || {})) {
        if (Object.prototype.hasOwnProperty.call(v.filters, k) && v.filters[k]) _dashStatusFilters[k] = true;
      }
      _dashSortKey = v.sortKey || '';
      _dashSortAsc = v.sortAsc !== false;
      _dashActiveView = name;
      try { localStorage.setItem(LS_HIVE_GROUP_BY, _dashGroupBy); } catch (e) { /* storage full or disabled */ }
      if (!quiet) renderHives(sortedDashHives(), true);
    }

    /* onSavedViewPick handles the picker's change event: '' means "no view". */
    function onSavedViewPick(name) {
      if (!name) { _dashActiveView = ''; renderHives(_allDashHives, true); return; }
      applySavedView(name);
    }

    /* renameSavedView renames a view in place, keeping the default-view pointer
       in sync so a renamed default stays the default. */
    function renameSavedView() {
      if (!_dashActiveView) { hiveToast('Select a view to rename', 'error'); return; }
      var v = findSavedView(_dashActiveView);
      if (!v) return;
      var raw = window.prompt('Rename view:', v.name);
      if (raw === null) return;
      var name = String(raw).trim().slice(0, HIVE_VIEW_NAME_MAX_LEN);
      if (!name) { hiveToast('View name cannot be empty', 'error'); return; }
      if (name !== v.name && findSavedView(name)) { hiveToast('A view named "' + name + '" already exists', 'error'); return; }
      var wasDefault = _dashDefaultView === v.name;
      v.name = name;
      _dashActiveView = name;
      persistSavedViews();
      if (wasDefault) {
        _dashDefaultView = name;
        try { localStorage.setItem(LS_HIVE_DEFAULT_VIEW, name); } catch (e) { /* storage full or disabled */ }
      }
      renderHives(_allDashHives, true);
    }

    /* deleteSavedView removes the selected view, clearing the default pointer
       if it pointed at the deleted view. */
    function deleteSavedView() {
      if (!_dashActiveView) { hiveToast('Select a view to delete', 'error'); return; }
      if (!window.confirm('Delete view "' + _dashActiveView + '"?')) return;
      var kept = [];
      for (var i = 0; i < (_dashSavedViews || []).length; i++) {
        if (_dashSavedViews[i].name !== _dashActiveView) kept.push(_dashSavedViews[i]);
      }
      _dashSavedViews = kept;
      if (_dashDefaultView === _dashActiveView) {
        _dashDefaultView = '';
        try { localStorage.removeItem(LS_HIVE_DEFAULT_VIEW); } catch (e) { /* storage disabled */ }
      }
      _dashActiveView = '';
      persistSavedViews();
      renderHives(_allDashHives, true);
    }

    /* toggleDefaultView marks/unmarks the selected view as the one applied on
       load. */
    function toggleDefaultView() {
      if (!_dashActiveView) { hiveToast('Select a view first', 'error'); return; }
      if (_dashDefaultView === _dashActiveView) {
        _dashDefaultView = '';
        try { localStorage.removeItem(LS_HIVE_DEFAULT_VIEW); } catch (e) { /* storage disabled */ }
        hiveToast('Default view cleared', 'success');
      } else {
        _dashDefaultView = _dashActiveView;
        try { localStorage.setItem(LS_HIVE_DEFAULT_VIEW, _dashDefaultView); } catch (e) { /* storage full or disabled */ }
        hiveToast('"' + _dashDefaultView + '" is now the default view', 'success');
      }
      renderHives(_allDashHives, true);
    }

    /* hiveMatchesFilters answers whether a hive survives the active chips. */
    function hiveMatchesFilters(h) {
      if (_dashFailingCheckFilter) {
        var hit = failingChecks(h).some(function(ck) { return ck.name === _dashFailingCheckFilter; });
        if (!hit) return false;
      }
      if (!hiveMatchesDriftFilter(h)) return false;
      if (!hiveMatchesUpgradeFilter(h)) return false;
      if (!hiveMatchesAdvisoryStaleFilter(h)) return false;
      var active = Object.keys(_dashStatusFilters || {}).filter(function(k) { return _dashStatusFilters[k]; });
      if (!active.length) return true;
      var f = hiveStatusFlags(h);
      var byKey = {};
      byKey[HIVE_FILTER_APP_MISSING] = f.appMissing;
      byKey[HIVE_FILTER_NO_TOKENS] = f.noTokens;
      byKey[HIVE_FILTER_DEGRADED] = f.degraded;
      byKey[HIVE_FILTER_OK] = f.ok;
      for (var i = 0; i < active.length; i++) {
        if (byKey[active[i]]) return true;
      }
      return false;
    }

    /* ── Fleet alerts ("Attention needed") ────────────────────────────────
       Server-evaluated (see alerts.go); the client only renders and filters.
       Deliberately NOT recomputed here: a second, drifting implementation of
       "what is wrong" is exactly how the panel and the rows start disagreeing. */

    /* EMPTY_ALERT_SUMMARY is the shape every consumer can assume. Frozen so a
       caller cannot accidentally mutate the shared fallback. */
    var EMPTY_ALERT_SUMMARY = {alerts: [], countsBySeverity: {}, countsByType: {}, total: 0, acknowledgedTotal: 0};

    var _fleetAlerts = EMPTY_ALERT_SUMMARY;
    /* Active alert-type filter: '' = no alert filtering. Single-select, unlike
       the status chips — "show me the crash-looping hives" is a drill-down, and
       OR-ing several alert types back together just reproduces the full list. */
    var _alertTypeFilter = '';
    /* Whether the acknowledged alerts are expanded into view. */
    var _alertShowAcked = false;

    /* Severity display order + colour, mirroring alerts.go's ranking. */
    var ALERT_SEVERITIES = [
      {key: 'critical', label: 'Critical', color: '#f85149'},
      {key: 'warning', label: 'Warning', color: '#f59e0b'},
      {key: 'info', label: 'Info', color: '#60a5fa'}
    ];

    /* Human labels for the alert type chips. Keys MUST match the AlertType*
       constants in alerts.go; an unknown type falls back to its raw key rather
       than rendering blank. */
    var ALERT_TYPE_LABELS = {
      'crash-loop': 'Crash-looping',
      'offline': 'Offline',
      'stuck-upgrade': 'Stuck upgrade',
      'health-check-failing': 'Health check failing',
      'token-burn': 'Token burn',
      'provision-error': 'Provision error',
      /* Added by the hub in #2308. Without a label here the chip for a
         genuinely failed upgrade would render as the raw key 'failed-upgrade'. */
      'failed-upgrade': 'Failed upgrade',
      'advisory-stale': 'Stale advisory',
      /* Agents that are up but doing nothing — a session that has gone, a
         login prompt, or no output while work is queued. Deliberately paused
         agents are excluded server-side and never counted here. */
      'agents-inactive': 'Idle agents',
      /* Raised by the auth-audit loop since #2306 but never labelled, so its
         chip rendered the raw key. */
      'url-unreachable': 'Dashboard URL unreachable'
    };

    /* How many alert rows are listed before the panel collapses the remainder
       behind the "+N more" toggle. Enough to act on, few enough that the panel
       never pushes the hive list off the screen. */
    var ALERT_ROWS_SHOWN_MIN = 6;

    /* Which halves of the panel are expanded past ALERT_ROWS_SHOWN_MIN. Keyed
       'live'/'acked' so the two lists expand independently. Not persisted: an
       expanded list is a momentary "show me the rest", not a view preference,
       and a panel that reloads 50 rows deep would bury the hive table. */
    var _alertRowsExpanded = {live: false, acked: false};

    /* toggleAlertRowsExpanded flips one half of the panel open or closed. */
    function toggleAlertRowsExpanded(which) {
      var key = (which === 'acked') ? 'acked' : 'live';
      _alertRowsExpanded[key] = !_alertRowsExpanded[key];
      renderHives(_allDashHives, true);
    }

    function alertTypeLabel(t) { return ALERT_TYPE_LABELS[t] || t || 'Unknown'; }

    /* alertsForType returns the UNACKNOWLEDGED alerts of one type. Acknowledged
       ones are excluded so filtering by a type never selects hives whose alert
       the operator has already dealt with. */
    function alertsForType(t) {
      return ((_fleetAlerts && _fleetAlerts.alerts) || []).filter(function(a) {
        return a && !a.acknowledged && a.type === t;
      });
    }

    /* hiveMatchesAlertFilter answers whether a hive survives the active alert
       drill-down. No filter = everything passes. */
    function hiveMatchesAlertFilter(h) {
      if (!_alertTypeFilter) return true;
      if (!h || !h.id) return false;
      var matches = alertsForType(_alertTypeFilter);
      for (var i = 0; i < matches.length; i++) {
        if (matches[i].hiveId === h.id) return true;
      }
      return false;
    }

    /* toggleAlertFilter drills into (or back out of) one alert type. Clicking
       the active chip again clears it. */
    function toggleAlertFilter(t) {
      _alertTypeFilter = (_alertTypeFilter === t) ? '' : t;
      renderHives(_allDashHives, true);
    }

    function clearAlertFilter() {
      _alertTypeFilter = '';
      renderHives(_allDashHives, true);
    }

    /* clearAllHiveFilters resets EVERY narrowing control at once — status chips,
       the failing-check and drift filters, the alert-type filter, the search box
       and the facets. The empty-state escape hatch uses it, because any one of
       them alone can empty the list and the user usually cannot tell which is
       responsible. */
    function clearAllHiveFilters() {
      _dashStatusFilters = {};
      _dashFailingCheckFilter = '';
      _dashDriftFilter = '';
      _dashUpgradeFilter = '';
      _dashAdvisoryStaleFilter = false;
      _alertTypeFilter = '';
      _dashFacets = {};
      _dashSearchQuery = '';
      var searchEl = document.getElementById('hive-search');
      if (searchEl) searchEl.value = '';
      renderHives(_allDashHives, true);
    }

    function toggleAlertAcked() {
      _alertShowAcked = !_alertShowAcked;
      renderHives(_allDashHives, true);
    }

    /* alertAge renders how long a condition has been firing, from the server's
       firstSeen. Rounded coarsely so it does not churn every poll. */
    function alertAge(firstSeen) {
      if (!firstSeen) return '';
      var ms = Date.now() - new Date(firstSeen).getTime();
      if (!isFinite(ms) || ms < 0) return '';
      var MIN = 60000, HOUR = 3600000, DAY = 86400000;
      if (ms < MIN) return 'just now';
      if (ms < HOUR) return Math.round(ms / MIN) + 'm';
      if (ms < DAY) return Math.round(ms / HOUR) + 'h';
      return Math.floor(ms / DAY) + 'd';
    }

    /* ackAlert silences (or un-silences) one alert. Admin-only server-side; the
       button is only rendered for admins, but the server is the real gate. */
    async function ackAlert(hiveId, type, clear) {
      try {
        var resp = await fetch('/api/saas/admin/alert-ack', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({hiveId: hiveId, type: type, clear: !!clear})
        });
        var data = await resp.json().catch(function() { return {}; });
        if (!resp.ok) {
          hiveToast(data.error || 'Failed to update alert', 'error');
          return;
        }
        hiveToast(clear ? 'Alert restored' : 'Alert acknowledged', 'success');
        /* Re-fetch rather than mutating locally: the server owns whether an ack
           actually applies (it refuses one for a condition that is not live). */
        loadHives();
      } catch (e) {
        hiveToast('Error: ' + e.message, 'error');
      }
    }

    /* ── Jumping from an alert row to the hive's row in the table ──

       hiveRowDomId builds the anchor id renderHives puts on each <tr>. Prefixed
       so it can never collide with another element id, and encodeURIComponent'd
       because a hive id is user-influenced and may contain characters that are
       not valid raw in an id/selector. */
    var HIVE_ROW_DOM_ID_PREFIX = 'hive-row-';
    function hiveRowDomId(hiveId) {
      return HIVE_ROW_DOM_ID_PREFIX + encodeURIComponent(String(hiveId || ''));
    }

    /* How long the targeted row stays highlighted. Long enough to find the row
       after the smooth scroll finishes, short enough that a stale highlight does
       not linger and get mistaken for a status. */
    var HIVE_ROW_TARGET_HIGHLIGHT_MS = 2600;

    /* Handle of the pending highlight-clear timer, so two jumps in quick
       succession do not leave the first row highlighted forever. */
    var _hiveRowTargetTimer = null;

    /* focusHiveRow scrolls one hive's table row into view and highlights it.
       Returns false when the row is not currently in the DOM. */
    function focusHiveRow(hiveId) {
      var row = document.getElementById(hiveRowDomId(hiveId));
      if (!row) return false;
      /* Clear any previous target first — only ever one highlighted row. */
      if (_hiveRowTargetTimer) { clearTimeout(_hiveRowTargetTimer); _hiveRowTargetTimer = null; }
      var prev = document.querySelectorAll('.hive-row-targeted');
      (prev || []).forEach(function(el) { el.classList.remove('hive-row-targeted'); });

      row.scrollIntoView({behavior: 'smooth', block: 'center'});
      row.classList.add('hive-row-targeted');
      /* Move keyboard focus to the row as well, so a keyboard user lands there
         rather than being scrolled somewhere their focus ring is not. */
      if (!row.hasAttribute('tabindex')) row.setAttribute('tabindex', '-1');
      try { row.focus({preventScroll: true}); } catch (e) { /* focus is best-effort */ }

      _hiveRowTargetTimer = setTimeout(function() {
        row.classList.remove('hive-row-targeted');
        _hiveRowTargetTimer = null;
      }, HIVE_ROW_TARGET_HIGHLIGHT_MS);
      return true;
    }

    /* jumpToHiveRow is what an "Attention needed" row does when clicked.

       The hard case is a target the current view is HIDING. The hive list now
       carries persisted filters, facets, a search box, an alert drill-down and
       collapsible sections/groups (#2309, #2317), so the alerted hive may well
       not be on screen. Scrolling to a row that is not in the DOM would look
       like the click did nothing.

       The choice made here: try the current view FIRST, and only if the row is
       genuinely absent do we widen the view — clearing every narrowing control
       and expanding the collapsed sections/groups. Filters are the user's state,
       so they are never discarded silently: we clear them only when that is the
       one thing that can satisfy the click, and we say so in a toast that names
       the hive. The alternative (refusing to navigate and just warning) leaves
       the operator to work out which of six controls is hiding the row, which is
       the problem the panel exists to avoid. */
    function jumpToHiveRow(hiveId, hiveName) {
      if (!hiveId) return;
      if (focusHiveRow(hiveId)) return;

      /* Not rendered under the current view. Widen everything that can hide a
         row, re-render synchronously, then try again. */
      clearAllHiveFilters();
      expandAllHiveSections();
      _dashGroupCollapsed = {};
      persistGroupCollapsed();
      renderHives(_allDashHives, true);

      if (focusHiveRow(hiveId)) {
        hiveToast('Filters cleared to show ' + (hiveName || hiveId), 'info');
        return;
      }
      /* Still absent: the hive is genuinely not in the loaded list (for example
         it was deleted between the alert being evaluated and now). Say so rather
         than leaving a click that silently did nothing. */
      hiveToast('Could not find ' + (hiveName || hiveId) + ' in the hive list', 'error');
    }

    /* renderAlertsPanel draws the "Attention needed" panel above the hive list.
       When the fleet is clean it renders one quiet line instead of hiding
       entirely, so the operator can trust that the check actually ran. */
    function renderAlertsPanel() {
      var panel = document.getElementById('fleet-alerts-panel');
      if (!panel) return;
      var summary = _fleetAlerts || EMPTY_ALERT_SUMMARY;
      var all = summary.alerts || [];
      var counts = summary.countsBySeverity || {};
      var byType = summary.countsByType || {};
      var total = Number(summary.total) || 0;
      var ackedTotal = Number(summary.acknowledgedTotal) || 0;

      /* Nothing at all to say — and no hives yet — stay out of the way. */
      if (!total && !ackedTotal && !(_allDashHives || []).length) {
        panel.style.display = 'none';
        return;
      }
      panel.style.display = '';

      if (!total) {
        var cleanNote = ackedTotal
          ? ' <button type="button" class="alert-panel-more" onclick="toggleAlertAcked()">' +
            ackedTotal + ' acknowledged</button>'
          : '';
        var ackedRows = (_alertShowAcked && ackedTotal) ? renderAlertRows(all, true) : '';
        panel.innerHTML = '<div class="alert-panel">' +
          '<div class="alert-panel-clean"><span style="color:#3fb950">&#10003;</span>' +
          '<span>Nothing needs your attention.</span>' + cleanNote + '</div>' +
          ackedRows + '</div>';
        return;
      }

      var cls = 'alert-panel';
      if (counts.critical) cls += ' has-critical';
      else if (counts.warning) cls += ' has-warning';

      var pills = (ALERT_SEVERITIES || []).map(function(s) {
        var n = Number(counts[s.key]) || 0;
        if (!n) return '';
        return '<span class="alert-sev-pill" style="--alert-color:' + s.color + '">' +
          n + ' ' + esc(s.label) + '</span>';
      }).join('');

      /* Type chips, ordered by the severity of the alerts they represent so the
         most urgent drill-down is leftmost. */
      var typeKeys = Object.keys(byType).filter(function(k) { return Number(byType[k]) > 0; });
      typeKeys.sort(function(a, b) {
        var ra = alertTypeWorstSeverityRank(a), rb = alertTypeWorstSeverityRank(b);
        if (ra !== rb) return ra - rb;
        return a < b ? -1 : (a > b ? 1 : 0);
      });
      var chips = typeKeys.map(function(k) {
        var on = _alertTypeFilter === k;
        var color = ALERT_SEVERITY_COLORS[alertTypeWorstSeverity(k)] || '#8b949e';
        return '<button type="button" class="filter-chip' + (on ? ' on' : '') +
          '" aria-pressed="' + (on ? 'true' : 'false') +
          '" onclick="toggleAlertFilter(\'' + esc(k) + '\')" style="--chip-color:' + color + '">' +
          '<span class="filter-chip-dot" style="background:' + color + '"></span>' +
          esc(alertTypeLabel(k)) +
          '<span class="filter-chip-count">' + (Number(byType[k]) || 0) + '</span></button>';
      }).join('');
      var clearChip = _alertTypeFilter
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="clearAlertFilter()">Show all hives</button>'
        : '';

      var ackNote = ackedTotal
        ? '<button type="button" class="alert-panel-more" onclick="toggleAlertAcked()">' +
          (_alertShowAcked ? 'Hide' : 'Show') + ' ' + ackedTotal + ' acknowledged</button>'
        : '';

      panel.innerHTML = '<div class="' + cls + '">' +
        '<div class="alert-panel-head">' +
          '<div class="alert-panel-title"><span>&#9888;</span><span>Attention needed</span></div>' +
          '<div style="display:flex;gap:6px;flex-wrap:wrap">' + pills + '</div>' +
        '</div>' +
        '<div class="alert-type-chips">' + chips + clearChip + '</div>' +
        renderAlertRows(all, false) +
        (_alertShowAcked ? renderAlertRows(all, true) : '') +
        ackNote +
      '</div>';
    }

    /* ALERT_SEVERITY_COLORS maps a severity to its chip colour, derived from
       ALERT_SEVERITIES so the two can never disagree. */
    var ALERT_SEVERITY_COLORS = (function() {
      var m = {};
      (ALERT_SEVERITIES || []).forEach(function(s) { m[s.key] = s.color; });
      return m;
    })();

    /* alertTypeWorstSeverity / ...Rank find the most urgent severity currently
       present for a type, so a type chip is coloured by the worst thing in it. */
    function alertTypeWorstSeverity(t) {
      var worst = '', worstRank = ALERT_SEVERITIES.length;
      alertsForType(t).forEach(function(a) {
        var r = alertSeverityRankJS(a.severity);
        if (r < worstRank) { worstRank = r; worst = a.severity; }
      });
      return worst;
    }
    function alertTypeWorstSeverityRank(t) {
      var r = alertSeverityRankJS(alertTypeWorstSeverity(t));
      return r;
    }
    /* alertSeverityRankJS mirrors alerts.go's ordering; unknown sorts last. */
    function alertSeverityRankJS(sev) {
      for (var i = 0; i < ALERT_SEVERITIES.length; i++) {
        if (ALERT_SEVERITIES[i].key === sev) return i;
      }
      return ALERT_SEVERITIES.length;
    }

    /* renderAlertRows lists individual alerts. acked selects which half of the
       list is drawn: the live alerts, or the acknowledged ones. */
    function renderAlertRows(all, acked) {
      var rows = (all || []).filter(function(a) { return a && !!a.acknowledged === !!acked; });
      /* When a type drill-down is active, the rows follow it too — otherwise the
         panel would still list alerts for hives the table is no longer showing. */
      if (_alertTypeFilter) {
        rows = rows.filter(function(a) { return a.type === _alertTypeFilter; });
      }
      if (!rows.length) return '';
      /* Each half of the panel (live vs acknowledged) expands independently, so
         expanding the acknowledged list does not also dump every live alert. */
      var expanded = !!_alertRowsExpanded[acked ? 'acked' : 'live'];
      var limit = expanded ? rows.length : ALERT_ROWS_SHOWN_MIN;
      var shown = rows.slice(0, limit);
      var html = shown.map(function(a) {
        var color = ALERT_SEVERITY_COLORS[a.severity] || '#8b949e';
        var age = alertAge(a.firstSeen);
        /* Every interpolated value below is spoke- or admin-supplied and is
           escaped: hive names, reasons (which embed check names and provision
           error text), and the acking username. */
        var ackBtn = '';
        if (_isAdmin) {
          /* jsArg for the same reason as the jump handler below: esc() leaves an
             apostrophe intact, which would close the handler's quote early. */
          ackBtn = acked
            ? '<button type="button" class="alert-ack-btn" onclick="ackAlert(' +
              jsArg(a.hiveId) + ',' + jsArg(a.type) + ',true)">Restore</button>'
            : '<button type="button" class="alert-ack-btn" onclick="ackAlert(' +
              jsArg(a.hiveId) + ',' + jsArg(a.type) + ',false)">Acknowledge</button>';
        }
        var ackedBy = (acked && a.ackBy) ? '<span class="alert-row-reason">— ack by ' + esc(a.ackBy) + '</span>' : '';
        /* The row itself is a <button> that jumps to this hive's row in the
           table below. A real button (not a div with role/tabindex) gets Enter,
           Space and the focus ring for free.

           The Acknowledge button is a SIBLING, not a child: nesting a button
           inside a button is invalid HTML and browsers drop the inner one, which
           would silently break acknowledging. Both sit in a flex wrapper so the
           row still reads as one line. */
        var label = a.hiveName || a.hiveId;
        var jump = '<button type="button" class="alert-row' + (acked ? ' acked' : '') +
          '" title="Show ' + escAttr(label) + ' in the hive list below"' +
          /* jsArg, not esc: a hive or org name containing an apostrophe would
             close the handler's quote early and break the click. */
          ' onclick="jumpToHiveRow(' + jsArg(a.hiveId) + ',' + jsArg(label) + ')">' +
          '<span class="filter-chip-dot" style="background:' + color + '"></span>' +
          '<span class="alert-row-hive">' + esc(label) + '</span>' +
          '<span class="alert-row-reason">' + esc(a.reason) + '</span>' + ackedBy +
          '<span class="alert-row-age">' + esc(age) + '</span>' +
        '</button>';
        return '<div class="alert-row-wrap">' + jump + ackBtn + '</div>';
      }).join('');
      /* "+N more" used to be inert text, which was actively misleading once the
         rows above it became clickable: it looked like the same kind of thing
         and did nothing. It is now a real toggle that expands the full list in
         place (and collapses it again), so every alerted hive is reachable —
         which is the point, since the hive the operator needs is as likely to be
         #7 as #1. */
      var more = '';
      if (rows.length > limit) {
        more = '<button type="button" class="alert-panel-more" onclick="toggleAlertRowsExpanded(' +
          jsArg(acked ? 'acked' : 'live') + ')">+' + (rows.length - limit) +
          ' more</button>';
      } else if (expanded && rows.length > ALERT_ROWS_SHOWN_MIN) {
        more = '<button type="button" class="alert-panel-more" onclick="toggleAlertRowsExpanded(' +
          jsArg(acked ? 'acked' : 'live') + ')">Show fewer</button>';
      }
      return '<div class="alert-rows">' + html + more + '</div>';
    }

    /* ── Free-text search over the hive list ──
       OR semantics: whitespace-separated terms, a hive matches if ANY term is a
       case-insensitive substring of its searchable text. Like the status chips,
       search only narrows ASSIGNED hives — the unassigned placeholder pool is
       inventory, not a search result, and making it vanish as an admin types
       would hide the very slots they are about to assign. See _dashSearchQuery
       and hiveMatchesSearch below. */

    /* ── Collapsible admin sections ──
       Keys follow the 'hive-*' localStorage convention already used by
       hive-dismissed-banners / hive-cluster-health-collapsed. */
    var HIVE_SECTION_ASSIGNED = 'assigned';
    var HIVE_SECTION_UNASSIGNED = 'unassigned';
    var LS_SECTION_ASSIGNED_COLLAPSED = 'hive-section-assigned-collapsed';
    var LS_SECTION_UNASSIGNED_COLLAPSED = 'hive-section-unassigned-collapsed';

    /* Defaults: Assigned EXPANDED (the hives you actually operate), Unassigned
       COLLAPSED (a pool that can run to dozens of identical slots). Stored as
       the string 'true'/'false'; anything else falls back to the default, so a
       first visit and a corrupted value behave the same. */
    var _dashSectionCollapsed = {};
    _dashSectionCollapsed[HIVE_SECTION_ASSIGNED] =
      localStorage.getItem(LS_SECTION_ASSIGNED_COLLAPSED) === 'true';
    _dashSectionCollapsed[HIVE_SECTION_UNASSIGNED] =
      localStorage.getItem(LS_SECTION_UNASSIGNED_COLLAPSED) !== 'false';

    /* expandAllHiveSections opens both sections and PERSISTS that, matching
       toggleHiveSection. An in-memory-only reset would silently re-collapse on
       the next reload, so the row the operator was just sent to would vanish
       again — the collapsed Unassigned pool is the default, so this matters for
       any alert on an unassigned placeholder. */
    function expandAllHiveSections() {
      _dashSectionCollapsed[HIVE_SECTION_ASSIGNED] = false;
      _dashSectionCollapsed[HIVE_SECTION_UNASSIGNED] = false;
      try {
        localStorage.setItem(LS_SECTION_ASSIGNED_COLLAPSED, 'false');
        localStorage.setItem(LS_SECTION_UNASSIGNED_COLLAPSED, 'false');
      } catch(e) {} /* private-browsing quota — expansion still works in-session */
    }

    function toggleHiveSection(sectionKey) {
      _dashSectionCollapsed[sectionKey] = !_dashSectionCollapsed[sectionKey];
      var lsKey = sectionKey === HIVE_SECTION_ASSIGNED
        ? LS_SECTION_ASSIGNED_COLLAPSED : LS_SECTION_UNASSIGNED_COLLAPSED;
      try {
        localStorage.setItem(lsKey, _dashSectionCollapsed[sectionKey] ? 'true' : 'false');
      } catch(e) {} /* private-browsing quota — collapse still works in-session */
      renderHives(_allDashHives, true);
    }

    var _dashSearchQuery = '';
    /* Debounce keystrokes so a large hive list is not re-rendered per character.
       120ms is below the ~200ms threshold where typing starts to feel laggy. */
    var HIVE_SEARCH_DEBOUNCE_MS = 120;
    var _hiveSearchTimer = null;

    /* hiveSearchText flattens a hive's user-visible metadata into one lowercase
       haystack. Includes the access list usernames so an admin can find "which
       hive did I grant octocat?" — access is only populated on owned rows. */
    function hiveSearchText(h) {
      if (!h) return '';
      var parts = [
        h.id, h.name, h.org, h.primaryRepo, h.clusterId, h.clusterName,
        h.role, h.gitBranch, h.gitHash, h.dashboardUrl,
        /* The GitHub host is shown as a pill in the Location column, so it has
           to be searchable too — "github.ibm.com" is how an admin narrows to
           the GHE fleet. Absent means public GitHub, which is what the pill
           renders, so search on that same default rather than nothing. */
        h.githubHost || PAST_REQUESTS_DEFAULT_GITHUB_HOST
      ];
      (h.repos || []).forEach(function(r) { parts.push(r); });
      (h.access || []).forEach(function(a) {
        if (!a) return;
        parts.push(a.username);
        parts.push(a.role);
      });
      return parts.filter(function(p) { return p; }).join(' ').toLowerCase();
    }

    /* hiveMatchesSearch answers whether a hive survives the search box. */
    function hiveMatchesSearch(h) {
      var terms = (_dashSearchQuery || '').toLowerCase().split(/\s+/).filter(function(t) { return t; });
      if (!terms.length) return true;
      var hay = hiveSearchText(h);
      for (var i = 0; i < terms.length; i++) {
        if (hay.indexOf(terms[i]) !== -1) return true; /* OR */
      }
      return false;
    }

    /* onHiveSearchInput debounces then re-renders from the unfiltered cache so
       search composes with the chips, the facets and the current sort. */
    function onHiveSearchInput() {
      var el = document.getElementById('hive-search');
      var next = el ? el.value : '';
      if (_hiveSearchTimer) clearTimeout(_hiveSearchTimer);
      _hiveSearchTimer = setTimeout(function() {
        _dashSearchQuery = next;
        renderHives(_allDashHives, true);
      }, HIVE_SEARCH_DEBOUNCE_MS);
    }

    function clearHiveSearch() {
      var el = document.getElementById('hive-search');
      if (el) el.value = '';
      _dashSearchQuery = '';
      renderHives(_allDashHives, true);
    }

    /* ── Faceted search ──
       Facet groups are derived from the assigned hives themselves, so a group
       only appears when the data has something to offer. Within one group the
       selected values are OR'd (pick two clusters => hives in either); across
       groups they are AND'd (that cluster AND that role), which is the standard
       faceted-search contract users expect from e-commerce-style filters. */
    var FACET_CLUSTER = 'cluster';
    var FACET_ACMM = 'acmm';
    var FACET_ROLE = 'role';
    var FACET_STATUS = 'status';
    var FACET_BRANCH = 'branch';
    /* GitHub instance the hive targets. Worth its own facet because the
       GHE-vs-public split is a real operational boundary — a github.ibm.com
       hive can only be reached from a cluster that can route to it. */
    var FACET_GITHUB_HOST = 'github-host';

    /* Value shown when a hive has no value for a facet, so those rows stay
       reachable instead of silently dropping out of every facet count. */
    var FACET_UNKNOWN = '—';

    /* Group keys for the two filters moved into the tray from the old chip bar.
       They are NOT in HIVE_FACET_GROUPS — those are derived from hive fields via
       hiveFacetValues, whereas these keep their own pre-existing state
       (_dashStatusFilters / _dashFailingCheckFilter) and matching semantics.
       They share _dashFacetCollapsed only for the collapse chrome. */
    var FACET_GROUP_HEALTH = 'health';
    var FACET_GROUP_FAILING_CHECK = 'failing-check';
    /* Upgrade-state group. Follows the HEALTH/FAILING_CHECK pattern rather than
       the derived-facet one: the values are a fixed, hand-written lifecycle
       ('upgrading', 'queued') with their own matching rule and their own state
       (_dashUpgradeFilter), not a dimension enumerated from the data like
       cluster or branch, so it stays OUT of HIVE_FACET_GROUPS. */
    var FACET_GROUP_UPGRADE = 'upgrade-state';
    /* Stale-advisory group. Follows the HEALTH/FAILING_CHECK pattern rather
       than the derived-facet one: "is this hive's advisory digest stale" is a
       boolean health signal read off a server-computed flag, not an enumerated
       dimension of the hive like cluster or branch, so it keeps its own state
       (_dashAdvisoryStaleFilter) and its own matching rule and stays OUT of
       HIVE_FACET_GROUPS. */
    var FACET_GROUP_ADVISORY_STALE = 'advisory-stale';

    var HIVE_FACET_GROUPS = [
      {key: FACET_CLUSTER, label: 'Location'},
      {key: FACET_ACMM, label: 'ACMM level'},
      {key: FACET_ROLE, label: 'Your role'},
      {key: FACET_STATUS, label: 'Status'},
      {key: FACET_BRANCH, label: 'Branch'},
      {key: FACET_GITHUB_HOST, label: 'GitHub'}
    ];

    /* Selected facet values: {facetKey: {value: true}}. */
    var _dashFacets = {};
    /* Collapsed facet GROUPS (chrome only, not a filter): {facetKey: true}. */
    var _dashFacetCollapsed = {};

    /* ── Facet tray open/closed ──
       Click-toggled, persisted, and COLLAPSED BY DEFAULT so the dashboard is
       uncluttered until filters are asked for. Key follows the same 'hive-*'
       localStorage convention as hive-section-assigned-collapsed and
       hive-cluster-health-collapsed. Open only when explicitly stored 'true',
       so a first visit and a corrupted value both fall back to closed. */
    var LS_FACET_TRAY_OPEN = 'hive-facet-tray-open';
    var _dashFacetTrayOpen = false;
    try {
      _dashFacetTrayOpen = localStorage.getItem(LS_FACET_TRAY_OPEN) === 'true';
    } catch (e) { _dashFacetTrayOpen = false; } /* storage disabled */

    /* applyFacetTrayState paints the DOM from _dashFacetTrayOpen. Split out so
       the initial paint and every toggle go through exactly one code path and
       can never disagree about aria-expanded. */
    function applyFacetTrayState() {
      var shell = document.getElementById('hive-facet-shell');
      var toggle = document.getElementById('hive-facet-toggle');
      if (shell) shell.classList.toggle('facet-open', _dashFacetTrayOpen);
      if (toggle) {
        toggle.setAttribute('aria-expanded', _dashFacetTrayOpen ? 'true' : 'false');
        toggle.title = _dashFacetTrayOpen ? 'Hide filters' : 'Show filters';
      }
    }

    function setFacetTrayOpen(open) {
      _dashFacetTrayOpen = !!open;
      try {
        localStorage.setItem(LS_FACET_TRAY_OPEN, _dashFacetTrayOpen ? 'true' : 'false');
      } catch (e) {} /* private-browsing quota — the toggle still works in-session */
      applyFacetTrayState();
    }

    function toggleFacetTray() { setFacetTrayOpen(!_dashFacetTrayOpen); }

    /* activeFilterCount is what the collapsed rail's badge shows. EVERY control
       that can narrow the list is counted, because the badge is the only thing
       telling the operator why the table is short while the tray is shut. Each
       selected facet value counts individually — "3" must mean three choices,
       not three groups. */
    function activeFilterCount() {
      var n = 0;
      n += Object.keys(_dashStatusFilters || {}).length;
      if (_dashFailingCheckFilter) n++;
      if (_dashDriftFilter) n++;
      if (_dashUpgradeFilter) n++;
      if (_dashAdvisoryStaleFilter) n++;
      if (_alertTypeFilter) n++;
      if (_dashSearchQuery) n++;
      var groups = Object.keys(_dashFacets || {});
      for (var i = 0; i < groups.length; i++) {
        n += Object.keys(_dashFacets[groups[i]] || {}).length;
      }
      return n;
    }

    /* renderFacetTrayAffordance keeps the collapsed rail honest: accent border
       plus a count badge whenever anything is filtering. */
    function renderFacetTrayAffordance() {
      var toggle = document.getElementById('hive-facet-toggle');
      var badge = document.getElementById('hive-facet-active-badge');
      var n = activeFilterCount();
      if (toggle) toggle.classList.toggle('has-active', n > 0);
      if (badge) {
        badge.style.display = n > 0 ? '' : 'none';
        badge.textContent = n > 0 ? String(n) : '';
        badge.title = n === 1 ? '1 active filter' : n + ' active filters';
      }
      if (toggle) {
        var base = _dashFacetTrayOpen ? 'Hide filters' : 'Show filters';
        toggle.title = n > 0 ? base + ' — ' + (n === 1 ? '1 filter active' : n + ' filters active') : base;
        toggle.setAttribute('aria-label', toggle.title);
      }
    }

    /* Bound once at parse time via delegation on document, so the handlers
       survive every re-render of the tray's contents and no inline onclick has
       to interpolate anything. */
    document.addEventListener('click', function(ev) {
      var t = ev.target;
      if (!t || !t.closest) return;
      if (t.closest('#hive-facet-toggle')) { toggleFacetTray(); return; }
      if (t.closest('#hive-facet-close')) { setFacetTrayOpen(false); return; }
    });
    /* Escape closes the tray when focus is inside it, and returns focus to the
       toggle so the keyboard user is not stranded on a hidden control. */
    document.addEventListener('keydown', function(ev) {
      if (ev.key !== 'Escape' || !_dashFacetTrayOpen) return;
      var tray = document.getElementById('hive-facet-tray');
      if (!tray || !tray.contains(document.activeElement)) return;
      setFacetTrayOpen(false);
      var toggle = document.getElementById('hive-facet-toggle');
      if (toggle) toggle.focus();
    });
    /* Paint the persisted state immediately. This script runs after the tray
       markup, so the elements already exist and there is no open-then-snap-shut
       flash on a reload with the tray remembered open. */
    applyFacetTrayState();

    /* hiveFacetValues returns the facet values a single hive belongs to. A hive
       contributes at most one value per group. */
    function hiveFacetValues(h) {
      h = h || {};
      var f = {};
      f[FACET_CLUSTER] = h.clusterName || h.clusterId || 'local';
      f[FACET_ACMM] = (h.acmmLevel != null && h.acmmLevel !== '') ? ('L' + h.acmmLevel) : FACET_UNKNOWN;
      f[FACET_ROLE] = h.role || FACET_UNKNOWN;
      f[FACET_BRANCH] = h.gitBranch || FACET_UNKNOWN;
      /* Absent host means public GitHub — bucket it under the same label the
         pill renders, not FACET_UNKNOWN, so old spokes group with github.com
         rather than forming a meaningless "—" bucket. */
      f[FACET_GITHUB_HOST] = h.githubHost || PAST_REQUESTS_DEFAULT_GITHUB_HOST;
      var flags = hiveStatusFlags(h);
      f[FACET_STATUS] = flags.degraded ? 'Degraded' : (h.online ? 'Online' : 'Offline');
      return f;
    }

    /* hiveMatchesFacets: OR within a group, AND across groups. */
    function hiveMatchesFacets(h) {
      var vals = hiveFacetValues(h);
      var groups = Object.keys(_dashFacets || {});
      for (var g = 0; g < groups.length; g++) {
        var picked = Object.keys(_dashFacets[groups[g]] || {}).filter(function(v) { return _dashFacets[groups[g]][v]; });
        if (!picked.length) continue;
        if (picked.indexOf(String(vals[groups[g]])) === -1) return false;
      }
      return true;
    }

    function toggleFacetValue(facetKey, value) {
      if (!_dashFacets[facetKey]) _dashFacets[facetKey] = {};
      if (_dashFacets[facetKey][value]) {
        delete _dashFacets[facetKey][value];
        if (!Object.keys(_dashFacets[facetKey]).length) delete _dashFacets[facetKey];
      } else {
        _dashFacets[facetKey][value] = true;
      }
      renderHives(_allDashHives, true);
    }

    function clearHiveFacets() {
      _dashFacets = {};
      renderHives(_allDashHives, true);
    }

    function toggleFacetGroup(facetKey) {
      _dashFacetCollapsed[facetKey] = !_dashFacetCollapsed[facetKey];
      renderHives(_allDashHives, true);
    }

    /* facetGroupShell wraps one collapsible group so the status/failing-check
       groups moved in from the old chip bar are visually and behaviourally
       identical to the derived facet groups. */
    function facetGroupShell(key, label, collapsed, bodyHTML) {
      return '<div class="facet-group">' +
        '<button type="button" class="facet-group-head" aria-expanded="' + (collapsed ? 'false' : 'true') +
        '" onclick="toggleFacetGroup(\'' + esc(key) + '\')">' +
        '<span>' + esc(label) + '</span><span>' + (collapsed ? '▸' : '▾') + '</span></button>' +
        (collapsed ? '' : bodyHTML) + '</div>';
    }

    /* renderStatusFacetGroup renders the four health chips — GitHub App not
       installed / No tokens used / Degraded / OK — as a facet group inside the
       tray, replacing the old standalone chip row. Semantics are UNCHANGED:
       these still OR among themselves via _dashStatusFilters and
       hiveMatchesFilters, exactly like a facet group ORs within itself.
       Counts are over the FULL assigned set (placeholders already excluded by
       the caller), matching what the old bar advertised. */
    function renderStatusFacetGroup(assignedNoPlaceholders) {
      var counts = {};
      counts[HIVE_FILTER_APP_MISSING] = 0;
      counts[HIVE_FILTER_NO_TOKENS] = 0;
      counts[HIVE_FILTER_DEGRADED] = 0;
      counts[HIVE_FILTER_OK] = 0;
      (assignedNoPlaceholders || []).forEach(function(h) {
        var f = hiveStatusFlags(h);
        if (f.appMissing) counts[HIVE_FILTER_APP_MISSING]++;
        if (f.noTokens) counts[HIVE_FILTER_NO_TOKENS]++;
        if (f.degraded) counts[HIVE_FILTER_DEGRADED]++;
        if (f.ok) counts[HIVE_FILTER_OK]++;
      });
      var body = '<div class="facet-values">' + (HIVE_FILTER_CHIPS || []).map(function(c) {
        var on = !!_dashStatusFilters[c.key];
        return '<button type="button" class="facet-value' + (on ? ' on' : '') +
          '" aria-pressed="' + (on ? 'true' : 'false') +
          '" onclick="toggleStatusFilter(\'' + esc(c.key) + '\')" title="' + esc(c.label) + '">' +
          '<span class="facet-value-label">' + esc(c.label) + '</span>' +
          '<span class="facet-value-count">' + counts[c.key] + '</span></button>';
      }).join('') +
      /* Group-scoped reset for the health + failing-check pair, so a user can
         undo just these without also dropping their search term and facets. */
      (Object.keys(_dashStatusFilters || {}).length || _dashFailingCheckFilter || _dashDriftFilter || _dashUpgradeFilter || _dashAdvisoryStaleFilter
        ? '<button type="button" class="facet-value" onclick="clearStatusFilters()" title="Clear the health, failing-check, drift, upgrade-state and stale-advisory filters">' +
          '<span class="facet-value-label">Clear health filters</span></button>'
        : '') + '</div>';
      return facetGroupShell(FACET_GROUP_HEALTH, 'Health',
        !!_dashFacetCollapsed[FACET_GROUP_HEALTH], body);
    }

    /* renderFailingCheckFacetGroup renders the failing-check picker as a facet
       group. Semantics are UNCHANGED: still SINGLE-select and still ANDed
       against the health chips by hiveMatchesFilters. That is deliberately not
       the OR-within-group rule the derived facets use — "degraded hives" and
       "github_auth is failing" are different questions whose useful answer is
       the intersection — so the group head says so, rather than silently
       looking like the others. Returns '' on a healthy fleet. */
    function renderFailingCheckFacetGroup(assignedNoPlaceholders) {
      var fcCounts = failingCheckCounts(assignedNoPlaceholders);
      var fcNames = Object.keys(fcCounts).sort(function(a, b) {
        return fcCounts[b] - fcCounts[a] || a.localeCompare(b);
      }).slice(0, MAX_FAILING_CHECK_FILTER_OPTIONS);
      if (!fcNames.length && !_dashFailingCheckFilter) return '';
      /* Keep a selected check listed even at count 0 so it can be clicked off. */
      if (_dashFailingCheckFilter && fcNames.indexOf(_dashFailingCheckFilter) === -1) {
        fcNames.unshift(_dashFailingCheckFilter);
        if (fcCounts[_dashFailingCheckFilter] == null) fcCounts[_dashFailingCheckFilter] = 0;
      }
      var body = '<div class="facet-values">' + fcNames.map(function(nm) {
        var on = _dashFailingCheckFilter === nm;
        var n = fcCounts[nm] || 0;
        var tip = n >= FLEET_CHECK_SIGNAL_MIN_HIVES
          ? nm + ' failing on ' + n + ' hives — likely fleet-wide'
          : nm + ' failing on ' + n + (n === 1 ? ' hive' : ' hives');
        return '<button type="button" class="facet-value' + (on ? ' on' : '') +
          '" aria-pressed="' + (on ? 'true' : 'false') + '" title="' + esc(tip) + '"' +
          ' onclick="setFailingCheckFilter(' + (on ? "''" : "'" + esc(nm).replace(/'/g, '&#39;') + "'") + ')">' +
          '<span class="facet-value-label">' + esc(nm) + '</span>' +
          '<span class="facet-value-count">' + n + '</span></button>';
      }).join('') + '</div>';
      return facetGroupShell(FACET_GROUP_FAILING_CHECK, 'Failing check (one at a time)',
        !!_dashFacetCollapsed[FACET_GROUP_FAILING_CHECK], body);
    }

    /* How long a hive must have been upgrading before the facet's tooltip
       calls the fleet out as wedged rather than busy. Same limit the counter
       and the hub's stuck-upgrade alert use — one definition of "too long",
       reused, so the rail, the row and the alert panel cannot disagree. */
    var UPGRADE_FACET_STUCK_MS = UPGRADE_ELAPSED_RED_MS;

    /* renderUpgradeFacetGroup renders the upgrade-state picker. Single-select,
       ANDed against the health chips like the failing-check group.

       The 'upgrading' value additionally reports how many of those hives are
       past the stuck limit, because the whole point of this filter is catching
       a rollout that has stopped moving — a bare count of 15 upgrading hives
       looks identical whether they are 10 seconds or 40 minutes in.

       Returns '' when nothing is upgrading or queued AND no filter is on, so a
       settled fleet sees no extra chrome; renders while the filter is on even
       at count 0 so it can always be clicked back off. */
    function renderUpgradeFacetGroup(assignedNoPlaceholders) {
      var hives = assignedNoPlaceholders || [];
      var counts = {};
      counts[UPGRADE_FILTER_UPGRADING] = 0;
      counts[UPGRADE_FILTER_QUEUED] = 0;
      var stuck = 0;
      var now = Date.now();
      for (var i = 0; i < hives.length; i++) {
        var st = hiveUpgradeState(hives[i]);
        if (!st) continue;
        counts[st]++;
        if (st === UPGRADE_FILTER_UPGRADING) {
          var ms = upgradeElapsedMs(hives[i], now);
          /* null (unknown start time) is NOT counted as stuck — an unknown
             elapsed is not evidence of a fault, and inflating this number
             would make the warning untrustworthy. */
          if (ms !== null && ms >= UPGRADE_FACET_STUCK_MS) stuck++;
        }
      }
      var total = counts[UPGRADE_FILTER_UPGRADING] + counts[UPGRADE_FILTER_QUEUED];
      if (!total && !_dashUpgradeFilter) return '';

      var values = [
        {key: UPGRADE_FILTER_UPGRADING, label: 'Upgrading'},
        {key: UPGRADE_FILTER_QUEUED, label: 'Queued'}
      ];
      var body = '<div class="facet-values">' + (values || []).map(function(v) {
        var on = _dashUpgradeFilter === v.key;
        var n = counts[v.key] || 0;
        var tip;
        if (v.key === UPGRADE_FILTER_UPGRADING) {
          tip = n + (n === 1 ? ' hive is' : ' hives are') + ' upgrading now';
          if (stuck) {
            tip += ' — ' + stuck + ' past ' + Math.round(UPGRADE_FACET_STUCK_MS / 60000) +
              ' minutes and likely wedged rather than slow';
          }
        } else {
          tip = n + (n === 1 ? ' hive is' : ' hives are') +
            ' behind latest with auto-upgrade on, waiting to be instructed';
        }
        /* The count turns red when any upgrade is past the stuck limit, so the
           fault is visible in the rail without opening the filter. */
        var countStyle = (v.key === UPGRADE_FILTER_UPGRADING && stuck)
          ? ' style="color:var(--red);font-weight:600"' : '';
        return '<button type="button" class="facet-value' + (on ? ' on' : '') +
          '" aria-pressed="' + (on ? 'true' : 'false') + '" title="' + escAttr(tip) + '"' +
          ' onclick="toggleUpgradeFilter(' + jsArg(v.key) + ')">' +
          '<span class="facet-value-label">' + esc(v.label) + '</span>' +
          '<span class="facet-value-count"' + countStyle + '>' + n + '</span></button>';
      }).join('') + '</div>';
      return facetGroupShell(FACET_GROUP_UPGRADE, 'Upgrade state (one at a time)',
        !!_dashFacetCollapsed[FACET_GROUP_UPGRADE], body);
    }

    /* renderAdvisoryStaleFacetGroup renders the stale-advisory toggle as a
       facet group. One value, because the signal is boolean: picking it means
       "only hives whose advisory digest has gone stale".

       Returns '' when no hive is affected AND the filter is off, so a fleet
       with healthy digests sees no new chrome at all — the same self-
       suppressing contract advisoryStaleSummary() uses for the row pill. When
       the filter IS on the group always renders, even at count 0, so it can
       always be clicked back off (the failing-check group does the same). */
    function renderAdvisoryStaleFacetGroup(assignedNoPlaceholders) {
      var n = (assignedNoPlaceholders || []).filter(function(h) {
        return !!(h && h.advisoryStale);
      }).length;
      if (!n && !_dashAdvisoryStaleFilter) return '';
      var on = _dashAdvisoryStaleFilter;
      var tip = n === 1
        ? '1 hive should be posting advisory digests but its digest has gone stale'
        : n + ' hives should be posting advisory digests but their digests have gone stale';
      var body = '<div class="facet-values">' +
        '<button type="button" class="facet-value' + (on ? ' on' : '') +
        '" aria-pressed="' + (on ? 'true' : 'false') + '" title="' + esc(tip) + '"' +
        ' onclick="toggleAdvisoryStaleFilter()">' +
        '<span class="facet-value-label">Stale advisory</span>' +
        '<span class="facet-value-count">' + n + '</span></button></div>';
      return facetGroupShell(FACET_GROUP_ADVISORY_STALE, 'Advisory digest',
        !!_dashFacetCollapsed[FACET_GROUP_ADVISORY_STALE], body);
    }

    /* renderFacetRail draws the tray's body: the health chips and failing-check
       picker moved in from the old standalone bar, then the derived facet
       groups. Derived-group counts are computed over the assigned hives after
       the chips and the search box, but BEFORE the facets in the same group are
       applied — so a group keeps showing its alternatives once you pick one,
       rather than collapsing to the single value you chose. */
    function renderFacetRail(assignedAll) {
      var rail = document.getElementById('hive-facet-rail');
      /* The rail's affordance reflects filter state even when the rail body
         itself has nothing to draw, so update it before any early return. */
      renderFacetTrayAffordance();
      if (!rail) return;
      /* The health and failing-check groups filter ASSIGNED hives only — the
         unassigned pool is inventory. The caller already scopes to assigned;
         strip any placeholder defensively so the counts match the old bar. */
      var assignedReal = (assignedAll || []).filter(function(h) { return !isPlaceholderHive(h); });
      var base = (assignedAll || []).filter(hiveMatchesFilters).filter(hiveMatchesSearch);
      var head = renderStatusFacetGroup(assignedReal) + renderFailingCheckFacetGroup(assignedReal) +
        renderUpgradeFacetGroup(assignedReal) + renderAdvisoryStaleFacetGroup(assignedReal);
      if (!base.length && !Object.keys(_dashFacets || {}).length) {
        rail.innerHTML = head + clearFacetsButton();
        return;
      }
      var html = head;
      (HIVE_FACET_GROUPS || []).forEach(function(grp) {
        /* Count within this group ignoring this group's own selections. */
        var others = {};
        Object.keys(_dashFacets || {}).forEach(function(k) { if (k !== grp.key) others[k] = _dashFacets[k]; });
        var saved = _dashFacets;
        _dashFacets = others;
        var pool = base.filter(hiveMatchesFacets);
        _dashFacets = saved;

        var counts = {};
        pool.forEach(function(h) {
          var v = String(hiveFacetValues(h)[grp.key]);
          counts[v] = (counts[v] || 0) + 1;
        });
        /* Always surface a currently-selected value, even at count 0, so the
           user can always click it off again. */
        Object.keys((_dashFacets[grp.key] || {})).forEach(function(v) { if (counts[v] == null) counts[v] = 0; });
        var values = Object.keys(counts).sort();
        if (!values.length) return;
        var collapsed = !!_dashFacetCollapsed[grp.key];
        var valuesHTML = '';
        if (!collapsed) {
          valuesHTML = '<div class="facet-values">' + values.map(function(v) {
            var on = !!(_dashFacets[grp.key] && _dashFacets[grp.key][v]);
            return '<button type="button" class="facet-value' + (on ? ' on' : '') + '" aria-pressed="' + (on ? 'true' : 'false') +
              '" onclick="toggleFacetValue(\'' + esc(grp.key) + '\',\'' + esc(v).replace(/'/g, '&#39;') + '\')" title="' + esc(v) + '">' +
              '<span class="facet-value-label">' + esc(v) + '</span>' +
              '<span class="facet-value-count">' + counts[v] + '</span></button>';
          }).join('') + '</div>';
        }
        html += facetGroupShell(grp.key, grp.label, collapsed, valuesHTML);
      });
      rail.innerHTML = html + clearFacetsButton();
    }

    /* One Clear button for the whole tray. It calls clearAllHiveFilters(), not
       clearHiveFacets(), because the health chips and the failing-check picker
       now live in the same tray: a button labelled "Clear" that left two of the
       three groups still filtering would be a lie. */
    function clearFacetsButton() {
      return activeFilterCount() > 0
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="clearAllHiveFilters()">Clear all filters</button>'
        : '';
    }

    /* applyDashFilters filters the hives the caller wants rendered.
       Placeholder (unassigned) rows bypass every filter — see isPlaceholderHive.
       For assigned rows all four narrowing mechanisms compose as an AND: the
       status chips (by state), the alert-type filter (hives carrying that
       alert), the search box and the facets. That is what "click an alert type
       to see those hives" has to mean when a chip or a search term is already
       active. */
    function applyDashFilters(hives) {
      return (hives || []).filter(function(h) {
        if (isPlaceholderHive(h)) return true;
        return hiveMatchesFilters(h) && hiveMatchesAlertFilter(h) &&
          hiveMatchesSearch(h) && hiveMatchesFacets(h);
      });
    }

    /* toggleStatusFilter flips one chip and re-renders from the unfiltered
       cache, so chips compose with whatever sort is currently applied. */
    function toggleStatusFilter(key) {
      _dashStatusFilters[key] = !_dashStatusFilters[key];
      if (!_dashStatusFilters[key]) delete _dashStatusFilters[key];
      renderHives(_allDashHives, true);
    }

    function clearStatusFilters() {
      _dashStatusFilters = {};
      /* "Clear filters" is offered from the empty state, so it must clear
         EVERY filter — leaving one on would keep the list empty and make the
         button look broken. */
      _dashFailingCheckFilter = '';
      _dashDriftFilter = '';
      _dashUpgradeFilter = '';
      _dashAdvisoryStaleFilter = false;
      renderHives(_allDashHives, true);
    }

    /* toggleUpgradeFilter selects an upgrade state, or clears it when the
       already-selected value is clicked again (single-select, like the drift
       filter). */
    function toggleUpgradeFilter(state) {
      _dashUpgradeFilter = (_dashUpgradeFilter === state) ? '' : (state || '');
      renderHives(_allDashHives, true);
    }

    /* toggleAdvisoryStaleFilter flips the stale-advisory narrowing on or off. */
    function toggleAdvisoryStaleFilter() {
      _dashAdvisoryStaleFilter = !_dashAdvisoryStaleFilter;
      renderHives(_allDashHives, true);
    }

    /* toggleDriftFilter selects (or deselects) one drift kind in the fleet
       exceptions summary. */
    function toggleDriftFilter(kind) {
      _dashDriftFilter = (_dashDriftFilter === kind) ? '' : kind;
      renderHives(_allDashHives, true);
    }

    /* renderDriftSummary draws the "N hives need attention" strip above the
       table, broken down by drift kind and clickable to filter.

       Counts are computed over the ASSIGNED set the caller passes, matching
       the status chips: an unassigned placeholder is never flagged for the
       claimed-hive concerns (no App, ACMM 0, no agents) server-side, and
       scoping the summary the same way keeps the headline count equal to the
       number of rows a human can actually act on. */
    function renderDriftSummary(assignedHives) {
      var el = document.getElementById('hive-drift-summary');
      if (!el) return;
      var hives = assignedHives || [];

      var hivesWithDrift = 0;
      var worstOverall = '';
      var kindCounts = {};   // kind -> number of HIVES carrying it
      var kindWorst = {};    // kind -> worst severity seen for that kind
      for (var i = 0; i < hives.length; i++) {
        var d = driftOf(hives[i]);
        if (!d.count) continue;
        hivesWithDrift++;
        if (DRIFT_SEVERITY_ORDER.indexOf(d.worstSeverity) >= 0 &&
            (worstOverall === '' || DRIFT_SEVERITY_ORDER.indexOf(d.worstSeverity) < DRIFT_SEVERITY_ORDER.indexOf(worstOverall))) {
          worstOverall = d.worstSeverity;
        }
        /* A hive can legitimately report the same kind once only, but count
           distinct kinds per hive defensively so a duplicated signal cannot
           inflate the breakdown past the headline. */
        var seenKinds = {};
        for (var j = 0; j < d.signals.length; j++) {
          var s = d.signals[j] || {};
          if (!s.kind || seenKinds[s.kind]) continue;
          seenKinds[s.kind] = true;
          kindCounts[s.kind] = (kindCounts[s.kind] || 0) + 1;
          var prev = kindWorst[s.kind];
          if (!prev || DRIFT_SEVERITY_ORDER.indexOf(s.severity) < DRIFT_SEVERITY_ORDER.indexOf(prev)) {
            kindWorst[s.kind] = s.severity;
          }
        }
      }

      if (!hivesWithDrift) {
        el.style.display = 'none';
        el.innerHTML = '';
        return;
      }
      el.style.display = '';

      /* Sort the breakdown worst-severity-first, then by how many hives are
         affected, then by label — so the most urgent, most widespread problem
         is always the first thing read. */
      var kinds = Object.keys(kindCounts).sort(function(a, b) {
        var sa = DRIFT_SEVERITY_ORDER.indexOf(kindWorst[a]);
        var sb = DRIFT_SEVERITY_ORDER.indexOf(kindWorst[b]);
        if (sa !== sb) return sa - sb;
        if (kindCounts[b] !== kindCounts[a]) return kindCounts[b] - kindCounts[a];
        return driftKindLabel(a).localeCompare(driftKindLabel(b));
      });

      var chips = kinds.map(function(k) {
        var color = DRIFT_SEVERITY_COLORS[kindWorst[k]] || DRIFT_SEVERITY_COLORS.info;
        var on = _dashDriftFilter === k;
        var cls = on ? 'filter-chip on' : 'filter-chip';
        return '<button type="button" class="' + cls + '" aria-pressed="' + (on ? 'true' : 'false') +
          '" onclick="toggleDriftFilter(\'' + esc(k) + '\')" style="--chip-color:' + color + '">' +
          '<span class="filter-chip-dot" style="background:' + color + '"></span>' +
          esc(driftKindLabel(k)) + '<span class="filter-chip-count">' + kindCounts[k] + '</span></button>';
      }).join('');

      var headColor = DRIFT_SEVERITY_COLORS[worstOverall] || DRIFT_SEVERITY_COLORS.info;
      var headline = hivesWithDrift === 1
        ? '1 hive needs attention'
        : hivesWithDrift + ' hives need attention';
      var clearBtn = _dashDriftFilter
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="toggleDriftFilter(\'' + esc(_dashDriftFilter) + '\')">Show all</button>'
        : '';

      el.innerHTML =
        '<div style="display:flex;align-items:center;gap:8px;flex-wrap:wrap">' +
        '<span style="color:' + headColor + ';font-weight:600;font-size:0.8rem">⚠ ' + esc(headline) + '</span>' +
        '<span style="color:var(--muted);font-size:0.7rem">configuration drift detected from the fleet norm</span>' +
        '</div>' +
        '<div class="filter-chips" style="margin-top:8px">' + chips + clearBtn + '</div>';
    }

    /* renderStatusFilterBar draws the "Showing X of Y" summary and the Clear
       button. The health chips and the failing-check picker used to live here as
       standalone chip rows; they now render inside the facet tray
       (renderStatusFacetGroup / renderFailingCheckFacetGroup). What stays is the
       one thing that must NOT be hidden behind a collapsed tray: the statement
       that the list is currently narrowed, and the way back out.

       Unassigned placeholders are excluded from the totals — they are
       inventory, never filtered by these controls. */
    function renderStatusFilterBar(allHivesIn, shownCount) {
      var bar = document.getElementById('hive-filter-bar');
      if (!bar) return;
      var allHives = (allHivesIn || []).filter(function(h) { return !isPlaceholderHive(h); });
      /* activeFilterCount covers EVERY narrowing control — the health chips,
         the failing-check picker, drift, the alert drill-down, the search box
         and the facets. Omitting one would hide the Clear button while the list
         is still filtered, which reads as hives having disappeared. */
      var activeN = activeFilterCount();
      var anyActive = activeN > 0;
      var total = (allHives || []).length;
      var summary = anyActive
        ? 'Showing ' + shownCount + ' of ' + total + ' assigned hives' +
          ' &middot; ' + activeN + (activeN === 1 ? ' filter active' : ' filters active')
        : total + (total === 1 ? ' assigned hive' : ' assigned hives');
      var clearBtn = anyActive
        ? '<button type="button" class="filter-chip filter-chip-clear" onclick="clearAllHiveFilters()">Clear filters</button>'
        : '';
      bar.innerHTML = '<span class="filter-summary">' + summary + '</span>' + clearBtn;
      /* The bar no longer carries chip rows, so on a fleet with no assigned
         hives it would render as an empty flex box still claiming its
         margin-bottom. Collapse it outright in that case rather than leaving a
         gap above the table. */
      bar.style.display = total ? '' : 'none';
    }

    /* renderViewBar draws the group-by <select> and the saved-view controls.
       Every user-controlled string here — saved-view names the operator typed —
       is escaped on render; group-by labels are our own constants but go
       through esc() too so the rule needs no exceptions. */
    function renderViewBar() {
      var bar = document.getElementById('hive-view-bar');
      if (!bar) return;
      var opts = (HIVE_GROUP_DIMENSIONS || []).map(function(d) {
        return '<option value="' + esc(d.key) + '"' + (d.key === _dashGroupBy ? ' selected' : '') + '>' + esc(d.label) + '</option>';
      }).join('');
      var groupCtl = '<label class="view-ctl"><span class="view-ctl-label">Group by</span>' +
        '<select id="hive-group-by" class="view-select" onchange="setDashGroupBy(this.value)">' + opts + '</select></label>';

      var viewOpts = '<option value="">No saved view</option>' + (_dashSavedViews || []).map(function(v) {
        var isDefault = v.name === _dashDefaultView;
        return '<option value="' + esc(v.name) + '"' + (v.name === _dashActiveView ? ' selected' : '') + '>' +
          esc(v.name) + (isDefault ? ' ★' : '') + '</option>';
      }).join('');
      var hasSel = !!_dashActiveView && !!findSavedView(_dashActiveView);
      var dis = hasSel ? '' : ' disabled';
      var defaultLabel = (hasSel && _dashDefaultView === _dashActiveView) ? 'Unset default' : 'Set default';
      var viewCtl = '<label class="view-ctl"><span class="view-ctl-label">View</span>' +
        '<select id="hive-saved-view" class="view-select" onchange="onSavedViewPick(this.value)">' + viewOpts + '</select></label>' +
        '<button type="button" class="view-btn" onclick="saveCurrentView()" title="Save the current grouping, filters and sort as a named view">Save view</button>' +
        '<button type="button" class="view-btn" onclick="renameSavedView()"' + dis + '>Rename</button>' +
        '<button type="button" class="view-btn" onclick="deleteSavedView()"' + dis + '>Delete</button>' +
        '<button type="button" class="view-btn" onclick="toggleDefaultView()"' + dis +
        ' title="Apply this view automatically the next time My Hives loads">' + defaultLabel + '</button>';

      bar.innerHTML = '<div class="view-ctls">' + groupCtl + '</div><div class="view-ctls">' + viewCtl + '</div>';
    }

    /* hiveProvisionTime returns when this hive came into existence, as epoch
       ms, or null when that is genuinely unknown.

       Why registeredAt and NOT the per-hive meta.json created_at: created_at is
       declared on SaaSHive and written on exactly one provisioning path, so
       every hive that predates it — or that was created any other way — carries
       the empty string. On the live fleet at the time of writing, 26 of 50
       meta.json files have "created_at": "", including long-running assigned
       hives, while registeredAt was populated on 50 of 50 registry entries.
       registeredAt is also preserved across heartbeats (server.go copies it
       from the previous entry rather than restamping it), so it stays the
       first-seen time instead of drifting to "now" on every beat.

       Returns null — never NaN and never a rendered 'Invalid Date' — for a
       missing or unparseable value, so the caller can order those rows
       explicitly instead of comparing against NaN, which is false in both
       directions and would make the sort non-deterministic. */
    function hiveProvisionTime(h) {
      var raw = h && h.registeredAt;
      if (typeof raw !== 'string' || !raw) return null;
      var t = Date.parse(raw);
      return isNaN(t) ? null : t;
    }

    /* sortedDashHives applies the CURRENT sort key/direction to the unfiltered
       cache. Split out of sortDashHives so restoring a saved view (which sets
       the key/direction directly rather than by clicking a header) reproduces
       the same ordering without re-implementing the comparator. */
    function sortedDashHives() {
      var key = _dashSortKey;
      if (!key) return _allDashHives;
      return (_allDashHives || []).slice().sort(function(a, b) {
        if (key === 'journey') {
          /* journey is an object, so sort by how urgent it is rather than by
             stringifying it. Higher = needs attention sooner, so the default
             ascending sort puts on-track hives first and the most-escalated
             last; click again to bring the problems to the top. */
          var ja = journeySortValue(a && a.journey);
          var jb = journeySortValue(b && b.journey);
          return _dashSortAsc ? ja - jb : jb - ja;
        }
        if (key === 'registeredAt') {
          /* Provision time. Compared as epoch ms, not as text: registeredAt is
             RFC3339 so it happens to sort lexically today, but a value that
             ever arrives with an offset ('...+02:00') or without the 'Z' would
             collate wrong, and a string compare cannot express "unknown last".

             Hives with no parseable timestamp sort LAST in both directions
             rather than clumping at whichever end '' collates to — they are
             the least informative rows, so they stay out of the way whether
             the operator asked for oldest-first or newest-first. */
          var ra = hiveProvisionTime(a);
          var rb2 = hiveProvisionTime(b);
          if (ra === null && rb2 === null) return 0;
          if (ra === null) return 1;
          if (rb2 === null) return -1;
          return _dashSortAsc ? ra - rb2 : rb2 - ra;
        }
        var va = key === 'name' ? hiveNameSortValue(a) : ((a && a[key]) || '');
        var vb = key === 'name' ? hiveNameSortValue(b) : ((b && b[key]) || '');
        if (typeof va === 'number' && typeof vb === 'number') return _dashSortAsc ? va - vb : vb - va;
        return _dashSortAsc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
      });
    }

    /* hiveLabel derives the two-line name-cell label from the hive's PROJECT
       (org + primary repo) rather than by splitting h.name.

       Why not h.name: the hub synthesises name as org + "/" + primaryRepo on
       every heartbeat (see RegistryEntry construction in server.go), so
       splitting it back apart is a lossy round-trip of the two fields we
       already have on the row — and it breaks whenever either field itself
       contains a slash. Live fleet evidence: a URL-parsing bug put a HOST in
       the org position, so the listing showed 'github.ibm.com / forthehive'.
       Reading org/primaryRepo directly makes the label reflect the project.

       Precedence, most specific first:
         1. org + primaryRepo  — the real project, the whole point
         2. h.name             — a hive that reported a name but no project
         3. h.id               — last resort, never blank

       Returns {line1, line2}. line2 is '' whenever there is no SECOND thing
       worth saying, so the caller never renders an empty link or a stray '/'.
       An unassigned placeholder (org 'available-*' or 'placeholder', no
       primaryRepo) therefore keeps showing its org on line 1 with no empty
       second line — the same headline it shows today.

       primaryRepo is rendered VERBATIM, including any embedded slash
       ('enricom-ibm/jackrabbit' is a real live value produced by the same
       corruption). Taking only the last segment would silently hide half of
       a legitimate value, and the repo path is what the adjacent GitHub link
       resolves to — the label and the link must agree. */
    function hiveLabel(h) {
      if (!h) return { line1: '', line2: '' };
      var org = h.org || '';
      var repo = h.primaryRepo || '';
      if (org && repo) return { line1: org, line2: repo };
      /* Exactly one half of the project is known — show it alone, on the
         bold line, rather than emitting 'org/' or '/repo'. */
      if (org || repo) return { line1: org || repo, line2: '' };
      /* No project at all. Fall back to whatever identity the row carries. */
      return { line1: h.name || h.id || '', line2: '' };
    }

    /* hiveNameSortValue is the value the Hive column SORTS on. It must track
       the label the name cell DISPLAYS, so it delegates to hiveLabel —
       otherwise clicking "Hive ⇅" orders rows by text nobody can see.

       Sorting on the raw h.name is what diverges: the hub synthesises name as
       org + "/" + primaryRepo, so a hive with no org sorts under "/repo" (the
       leading slash collates before every letter) and a hive with neither
       sorts under "" (always first), while both DISPLAY an ordinary word. This
       reproduces hiveLabel's precedence and joins the two lines with a space,
       so the sort reads top line first, then second line — exactly how the
       cell is read. */
    function hiveNameSortValue(h) {
      var label = hiveLabel(h);
      return label.line2 ? label.line1 + ' ' + label.line2 : label.line1;
    }

    function sortDashHives(key) {
      if (_dashSortKey === key) { _dashSortAsc = !_dashSortAsc; } else { _dashSortKey = key; _dashSortAsc = true; }
      persistHiveSort();
      renderHives(sortedDashHives(), true);
    }

    /* persistHiveSort records the operator's EXPLICIT sort choice. Written only
       from sortDashHives (a header click), never from loadHiveSortPrefs, so
       restoring a preference can't rewrite it and a first visit leaves the key
       absent rather than storing the default — which is what lets
       loadHiveSortPrefs tell "never chose" apart from "chose the default". */
    function persistHiveSort() {
      lsSetJSON(LS_HIVE_SORT, {key: _dashSortKey, asc: _dashSortAsc});
    }

    /* ---- Scroll position across refresh --------------------------------
       "Location on page" is the WINDOW's scroll offset, not a container's:
       .table-wrap is overflow:visible at desktop widths (it only becomes an
       overflow-x scroller in the narrow media query, and that axis is
       horizontal), so the hive list scrolls the document itself.

       Writes are throttled through requestAnimationFrame because scroll fires
       at input rate and localStorage.setItem is synchronous — persisting on
       every event would jank the scroll it is trying to record. */
    var HIVE_SCROLL_MAX_AGE_MS = 30 * 60 * 1000; /* 30 min — see readHiveScroll */
    var HIVE_SCROLL_RESTORE_FRAMES = 3;          /* see restoreHiveScroll */
    var _hiveScrollWritePending = false;
    var _hiveScrollRestored = false;

    function persistHiveScroll() {
      if (_hiveScrollWritePending) return;
      _hiveScrollWritePending = true;
      window.requestAnimationFrame(function() {
        _hiveScrollWritePending = false;
        var y = window.pageYOffset || document.documentElement.scrollTop || 0;
        lsSetJSON(LS_HIVE_SCROLL, {y: y, t: Date.now()});
      });
    }

    /* readHiveScroll returns the stored offset, or 0 when there is nothing
       usable. Stale entries are discarded: coming back to the dashboard after
       hours, the fleet has changed underneath and the old offset points at
       unrelated rows, so landing at the top is the honest result. Guards the
       shape too — a hand-edited or truncated value must not yield NaN, which
       window.scrollTo would silently treat as 0 anyway but which would also
       poison the comparison below. */
    function readHiveScroll() {
      var s = lsGetJSON(LS_HIVE_SCROLL, null);
      if (!s || typeof s !== 'object') return 0;
      var y = Number(s.y), t = Number(s.t);
      if (!isFinite(y) || y <= 0) return 0;
      if (!isFinite(t) || (Date.now() - t) > HIVE_SCROLL_MAX_AGE_MS) return 0;
      return y;
    }

    /* restoreHiveScroll re-applies the saved offset ONCE per page load.

       Why not a single scrollTo at init: the rows are painted asynchronously
       (paintCachedHives, then loadHives' await, then renderHives), and the
       document is only as tall as what has been painted so far. Scrolling to
       y before the table exists clamps to the current maximum — silently
       landing at the top, which is exactly the bug this is meant to fix.

       So it retries across a few animation frames and stops as soon as the
       document is actually tall enough for the offset to survive, which is the
       observable definition of "the rows have rendered". The frame budget is
       small and bounded so a genuinely shorter list (hives removed since) costs
       three frames and then gives up rather than fighting the user forever. */
    function restoreHiveScroll() {
      if (_hiveScrollRestored) return;
      var y = readHiveScroll();
      if (!y) { _hiveScrollRestored = true; return; }
      var attempts = 0;
      var tryScroll = function() {
        var maxY = Math.max(0, document.documentElement.scrollHeight - window.innerHeight);
        if (maxY >= y) {
          window.scrollTo(0, y);
          _hiveScrollRestored = true;
          return;
        }
        if (++attempts >= HIVE_SCROLL_RESTORE_FRAMES) {
          /* Never tall enough — go as far as the page allows so the operator
             at least lands near where they were. */
          window.scrollTo(0, maxY);
          _hiveScrollRestored = true;
          return;
        }
        window.requestAnimationFrame(tryScroll);
      };
      window.requestAnimationFrame(tryScroll);
    }

    /* loadHiveSortPrefs resolves the stored sort against the A-Z default.

       Precedence: a PERSISTED choice wins, the default applies only when there
       is nothing usable stored — first visit, cleared storage, or a value that
       no longer validates. Applying A-Z unconditionally would undo the operator's
       choice on every refresh, which is the exact complaint this fixes.

       Every failure mode degrades to the default rather than throwing: lsGetJSON
       already swallows disabled storage and malformed JSON, and isHiveSortKey
       rejects a stale key so a removed column cannot leave the table sorting on
       a property no row has. */
    function loadHiveSortPrefs() {
      var stored = lsGetJSON(LS_HIVE_SORT, null);
      if (stored && typeof stored === 'object' && isHiveSortKey(stored.key)) {
        _dashSortKey = stored.key;
        /* Only an explicit false flips direction — a stored object missing
           'asc' still means ascending. */
        _dashSortAsc = stored.asc !== false;
        return;
      }
      _dashSortKey = HIVE_SORT_DEFAULT_KEY;
      _dashSortAsc = HIVE_SORT_DEFAULT_ASC;
    }

    /* ---- Usage attribution panel ----------------------------------------
       Token rollups by org / owner / cluster. TOKENS ONLY: the hub receives a
       single blended token total per hive with no per-model split, so no
       dollar figure is derivable here (see usage.go). The panel shows the
       server's currencyNote rather than inventing a rate. */

    /* USAGE_TOP_N is how many rollup rows each dimension shows collapsed. At
       42+ hives the full org/owner lists do not fit on screen; 5 keeps the
       three tables side-by-side and readable, with "Show all" to expand. */
    var USAGE_TOP_N = 5;
    /* USAGE_ZERO_TOP_N bounds the zero-consumption list the same way. Slightly
       larger than USAGE_TOP_N because this list is the actionable one — these
       are claimed hives burning nothing, i.e. possibly broken. */
    var USAGE_ZERO_TOP_N = 8;
    /* USAGE_SPARK_MIN_POINTS is the minimum sampled points before a trend line
       is drawn. Two points is the honest minimum for a line — with one sample
       there is no trend, only a dot, and drawing it would imply history the
       hub does not have. */
    var USAGE_SPARK_MIN_POINTS = 2;

    var _usageData = null;
    var _usageExpanded = {};

    function toggleUsageSection(key) {
      _usageExpanded[key] = !_usageExpanded[key];
      renderUsagePanel();
    }

    /* Renders one rollup dimension as a compact table. rows are already sorted
       by consumption descending server-side. */
    function renderUsageSection(title, rows, key) {
      rows = rows || [];
      if (!rows.length) {
        return '<div style="flex:1;min-width:200px">' +
          '<div style="font-size:0.72rem;text-transform:uppercase;letter-spacing:0.04em;color:var(--muted);margin-bottom:6px">' + esc(title) + '</div>' +
          '<div style="color:var(--muted);font-size:0.75rem;padding:4px 0">No data</div></div>';
      }
      var expanded = !!_usageExpanded[key];
      var shown = expanded ? rows.length : Math.min(rows.length, USAGE_TOP_N);
      var html = '';
      for (var i = 0; i < shown; i++) {
        var r = rows[i] || {};
        var pct = Number(r.sharePct) || 0;
        html += '<tr>' +
          '<td style="padding:2px 6px 2px 0;max-width:150px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' + esc(r.key) + '">' + esc(r.key) + '</td>' +
          '<td style="padding:2px 6px;text-align:right;white-space:nowrap">' + fmtTokens(r.tokens) + '</td>' +
          '<td style="padding:2px 6px;text-align:right;color:var(--muted);white-space:nowrap">' + (Number(r.hives) || 0) + '</td>' +
          /* Inline share bar — cheap visual ranking without a chart library. */
          '<td style="padding:2px 0 2px 6px;width:64px">' +
            '<div style="position:relative;height:9px;background:var(--surface);border-radius:2px;overflow:hidden" title="' + pct.toFixed(1) + '% of total">' +
            '<div style="position:absolute;left:0;top:0;bottom:0;width:' + Math.max(0, Math.min(100, pct)).toFixed(1) + '%;background:var(--accent);opacity:0.65"></div>' +
            '</div></td>' +
          '<td style="padding:2px 0 2px 6px;text-align:right;color:var(--muted);font-size:0.68rem;white-space:nowrap">' + pct.toFixed(1) + '%</td>' +
        '</tr>';
      }
      var more = '';
      if (rows.length > USAGE_TOP_N) {
        more = '<div style="margin-top:4px"><a href="#" onclick="toggleUsageSection(\'' + esc(key) + '\');return false" style="font-size:0.7rem;color:var(--accent);text-decoration:none">' +
          (expanded ? 'Show top ' + USAGE_TOP_N : 'Show all ' + rows.length) + '</a></div>';
      }
      return '<div style="flex:1;min-width:200px">' +
        '<div style="font-size:0.72rem;text-transform:uppercase;letter-spacing:0.04em;color:var(--muted);margin-bottom:6px">' + esc(title) + '</div>' +
        '<table style="width:100%;border-collapse:collapse;font-size:0.75rem"><tbody>' + html + '</tbody></table>' + more +
        '</div>';
    }

    function renderUsagePanel() {
      var el = document.getElementById('usage-panel');
      if (!el) return;
      var d = _usageData;
      if (!d) { el.style.display = 'none'; return; }
      el.style.display = 'block';

      var isFleet = d.scope === 'fleet';
      var scopeLabel = isFleet
        ? 'Fleet-wide'
        : 'Your hives only';

      /* Trend: only drawn from genuinely sampled history. With fewer than
         USAGE_SPARK_MIN_POINTS points we say so instead of drawing a line. */
      var hist = d.history || [];
      var trend = '';
      if (hist.length >= USAGE_SPARK_MIN_POINTS) {
        var pts = [];
        for (var i = 0; i < hist.length; i++) {
          /* sparkline() reads .v — map the snapshot shape onto it. */
          pts.push({ t: hist[i].t, v: Number(hist[i].v) || 0 });
        }
        trend = '<span title="Fleet total, sampled every 15 min (cumulative)">' + sparkline(pts, '#3fb950', 90, 16) + '</span>';
      } else if (isFleet) {
        trend = '<span style="font-size:0.68rem;color:var(--muted)" title="Snapshots are sampled every 15 minutes; a trend line needs at least two.">trend building…</span>';
      }

      var zero = d.zeroConsumption || [];
      var zeroHtml = '';
      if (zero.length) {
        var zExpanded = !!_usageExpanded['zero'];
        var zShown = zExpanded ? zero.length : Math.min(zero.length, USAGE_ZERO_TOP_N);
        var chips = '';
        for (var z = 0; z < zShown; z++) {
          var h = zero[z] || {};
          var dot = h.online
            ? '<span style="color:#3fb950" title="Online but consuming nothing — idle or stuck">●</span>'
            : '<span style="color:var(--muted)" title="Offline">○</span>';
          chips += '<span style="display:inline-block;padding:2px 7px;margin:2px 4px 2px 0;background:var(--surface);border:1px solid var(--border);border-radius:10px;font-size:0.7rem" title="' + esc((h.org || '') + (h.owner ? ' · ' + h.owner : '')) + '">' + dot + ' ' + esc(h.name || h.id) + '</span>';
        }
        var zMore = '';
        if (zero.length > USAGE_ZERO_TOP_N) {
          zMore = ' <a href="#" onclick="toggleUsageSection(\'zero\');return false" style="font-size:0.7rem;color:var(--accent);text-decoration:none">' +
            (zExpanded ? 'show fewer' : 'show all ' + zero.length) + '</a>';
        }
        zeroHtml = '<div style="margin-top:12px;padding-top:10px;border-top:1px solid var(--border)">' +
          '<div style="font-size:0.72rem;text-transform:uppercase;letter-spacing:0.04em;color:var(--muted);margin-bottom:6px" title="Claimed hives reporting no tokens. Unassigned pool slots are excluded — those are supposed to consume nothing.">' +
          'Zero consumption (' + zero.length + ')</div>' + chips + zMore + '</div>';
      }

      el.innerHTML =
        '<div style="background:var(--card,var(--surface));border:1px solid var(--border);border-radius:8px;padding:14px 16px">' +
          '<div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin-bottom:12px">' +
            '<h3 style="font-size:0.95rem;color:var(--accent);margin:0">Usage</h3>' +
            '<span style="font-size:0.7rem;color:var(--muted);border:1px solid var(--border);border-radius:10px;padding:1px 7px">' + esc(scopeLabel) + '</span>' +
            '<span style="flex:1"></span>' +
            trend +
            '<span style="font-size:0.8rem;color:var(--text)"><strong>' + fmtTokens(d.totalTokens) + '</strong> tokens' +
            ' <span style="color:var(--muted);font-size:0.72rem">across ' + (Number(d.hiveCount) || 0) + ' hive' + ((Number(d.hiveCount) || 0) === 1 ? '' : 's') + '</span></span>' +
          '</div>' +
          '<div style="display:flex;gap:20px;flex-wrap:wrap">' +
            /* byOrg buckets on org/repo (usageOrgRepoKey in usage.go), so the
               label says so; the JSON field keeps its original name. */
            renderUsageSection('By org / repo', d.byOrg, 'org') +
            renderUsageSection('By owner', d.byOwner, 'owner') +
            renderUsageSection('By cluster', d.byCluster, 'cluster') +
          '</div>' +
          zeroHtml +
          '<div style="margin-top:10px;font-size:0.66rem;color:var(--muted);line-height:1.4">' + esc(d.currencyNote || '') + '</div>' +
        '</div>';
    }

    async function loadUsage() {
      try {
        var resp = await fetch('/api/saas/usage');
        if (!resp.ok) { _usageData = null; renderUsagePanel(); return; }
        _usageData = await resp.json();
      } catch (e) {
        _usageData = null;
      }
      renderUsagePanel();
    }

    async function loadHives() {
      if (_hivesLoading) return;
      _hivesLoading = true;
      try {
        var resp = await fetch('/api/saas/my-hives');
        if (resp.status === 401) {
          window.location.href = '/login';
          return;
        }
        var data = await resp.json();
        _userQuota = data.saas_quota || 0;
        _userUsed = data.saas_used || 0;
        _allDashHives = data.hives || [];
        _hiveRegistry = data.hives || [];
        /* Alerts ride along on the same payload — see handleMyHives. Normalise
           to the empty summary so every consumer can iterate without guarding. */
        _fleetAlerts = data.alerts || EMPTY_ALERT_SUMMARY;
        _latestSHA = data.latest_sha || _latestSHA;
        if (data.latest_shas) _latestSHAs = data.latest_shas;
        if (data.tracked_branches) _trackedBranchesList = data.tracked_branches;
        if (data.latest_sha_messages) _latestSHAMessages = data.latest_sha_messages;
        if (data.latest_sha_image_status) _latestImageStatus = data.latest_sha_image_status;
        if (data.commit_messages) _commitMessages = data.commit_messages;
        if (data.hub_auto_upgrade !== undefined) _hubAutoUpgrade = data.hub_auto_upgrade;
        var shaEl = document.getElementById('latest-image-sha');
        if (shaEl) {
          var lines = '';
          var branches = Object.keys(_latestSHAs).sort();
          if (branches.length) {
            for (var bi = 0; bi < branches.length; bi++) {
              var br = branches[bi];
              var brMsg = _latestSHAMessages[br] || '';
              var brStatus = _latestImageStatus[br] || 'ready';
              var brStatusHTML = '';
              if (brStatus === 'building') {
                brStatusHTML = '<span style="display:inline-block;flex:none;width:10px;height:10px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite" title="Container image for this commit is still building"></span><span style="font-size:0.65rem;color:var(--muted);opacity:0.7;white-space:nowrap">building image…</span>';
              } else if (brStatus === 'failed') {
                brStatusHTML = '<span style="color:var(--red);font-size:0.7rem;cursor:help" title="Image build failed for this commit — upgrades keep using the previous image">✗</span>';
              }
              lines += '<div style="display:flex;align-items:center;gap:6px;margin-bottom:2px"><span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3)">' + esc(br) + '</span><span style="font-family:monospace;color:var(--muted)">' + esc(_latestSHAs[br]) + '</span>' + (brMsg ? '<span style="font-size:0.7rem;color:var(--muted);opacity:0.7">: ' + esc(brMsg) + '</span>' : '') + brStatusHTML + '</div>';
            }
          } else if (_latestSHA) {
            lines = '<span style="font-family:monospace;color:var(--muted)">' + esc(_latestSHA) + '</span>';
          }
          shaEl.innerHTML = lines ? '<div style="font-size:0.7rem;color:var(--muted);margin-bottom:2px">Latest available images:</div>' + lines : '<div style="display:flex;align-items:center;gap:6px;font-size:0.7rem;color:var(--muted)"><span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite"></span>Resolving latest available images…</div>';
        }
        var hubHash = data.hub_git_hash || '';
        var hubBranch = data.hub_git_branch || 'v2';
        if (hubHash) {
          var el = document.getElementById('hub-version');
          if (el) {
            var hubBranchLatest = _latestSHAs[hubBranch] || _latestSHA;
            var hubLatestUnknown = !hubBranchLatest;
            var isCurrent = hubBranchLatest && sameShaJS(hubHash, hubBranchLatest);
            var hubUpgradeBtn = '';
            // Distinguish two states, matching the per-hive badges:
            //   - "queued"    = behind latest with auto-upgrade ON, but the
            //     rollout hasn't started yet (auto-upgrade will apply shortly).
            //   - "Upgrading" = a rollout is ACTUALLY in progress (auto OR admin).
            // The server now tells us which via hub_upgrade_state, so an AUTO
            // rollout in progress shows "Upgrading" too — previously it was
            // mislabeled "queued" because the frontend could only see the
            // admin-click flag (_hubUpgrading) and had no signal for an auto roll.
            var hubState = data.hub_upgrade_state || '';
            // Trust the server state; fall back to the optimistic admin-click flag
            // so the badge flips to "Upgrading" immediately on click, before the
            // next /api/saas/my-hives poll reports "upgrading".
            var hubIsUpgrading = hubState === 'upgrading' || _hubUpgrading;
            var hubQueued = !hubIsUpgrading && (hubState === 'queued' ||
              (hubState === '' && !isCurrent && hubBranchLatest && _hubAutoUpgrade));
            if (!isCurrent && hubBranchLatest && _isAdmin && !hubIsUpgrading && !hubQueued) {
              hubUpgradeBtn = ' <button id="hub-upgrade-btn" onclick="upgradeHub(\'' + esc(hubHash) + '\')" style="padding:2px 8px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem;margin-left:6px;white-space:nowrap">Upgrade</button>';
            } else if (hubIsUpgrading) {
              hubUpgradeBtn = ' <span title="Upgrading to ' + esc(hubBranchLatest || '?') + '" style="display:inline-block;padding:2px 8px;background:var(--surface);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;margin-left:6px;white-space:nowrap;opacity:0.8"><span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:3px"></span>Upgrading</span>';
            } else if (hubQueued) {
              hubUpgradeBtn = ' <span title="Auto-upgrade will apply ' + esc(hubBranchLatest || '?') + ' shortly' + (_isAdmin ? ' — click to upgrade now' : '') + '"' + (_isAdmin ? ' onclick="upgradeHub(\'' + esc(hubHash) + '\')" style="cursor:pointer;' : ' style="') + 'display:inline-block;padding:2px 8px;background:var(--surface);color:var(--muted);border:1px dashed var(--border);border-radius:4px;font-size:0.65rem;margin-left:6px;white-space:nowrap">queued</span>';
            } else if (hubLatestUnknown && _isAdmin) {
              hubUpgradeBtn = ' <button disabled title="Waiting for latest version…" style="padding:2px 8px;background:var(--surface);color:var(--muted);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;margin-left:6px;white-space:nowrap;cursor:not-allowed;opacity:0.5">Upgrade</button>';
            }
            if (isCurrent) { _hubUpgrading = false; }
            var hubAutoCheck = '';
            if (_isAdmin) {
              hubAutoCheck = ' <label style="margin-left:6px;font-size:0.6rem;color:var(--muted);cursor:pointer;white-space:nowrap" title="Auto-upgrade hub when a new image is available"><input type="checkbox" ' + (_hubAutoUpgrade ? 'checked' : '') + ' onchange="toggleHubAutoUpgrade(this.checked)" style="vertical-align:middle;margin-right:2px;cursor:pointer">auto</label>';
            }
            var hubStatusIcon = hubLatestUnknown
              ? ' <span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-left:3px" title="Resolving latest version…"></span>'
              : isCurrent ? '<span style="color:var(--green);margin-left:3px" title="hub is on latest">✓</span>' : '<span style="color:var(--red);margin-left:3px" title="hub is behind latest ' + esc(hubBranchLatest) + '">↑</span>';
            var hubBranchPill = '<span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);margin-right:4px">' + esc(hubBranch) + '</span>';
            var hubMsg = _latestSHAMessages[hubBranch] || '';
            el.innerHTML = hubBranchPill + '<span style="font-family:monospace;font-size:0.7rem;color:var(--muted)" title="' + esc(hubMsg) + '">' + esc(hubHash) + '</span>' +
              hubStatusIcon + hubUpgradeBtn + hubAutoCheck;
          }
        }
        var canCreate = _userQuota < 0 || _userQuota > _userUsed;
        var addBtn = document.getElementById('btn-add-hive');
        if (addBtn) {
          addBtn.disabled = !canCreate;
          addBtn.title = canCreate ? '' : 'No hosted quota — contact hub admin';
        }
        /* Render through sortedDashHives() rather than the raw payload so an
           active sort — whether the operator clicked a column or restored a
           saved view that carries one — survives each poll instead of snapping
           back to server order every REFRESH cycle. With no sort key set this
           returns _allDashHives unchanged, i.e. the previous behaviour. */
        renderHives(sortedDashHives());
        /* Persist AFTER a successful render: a payload we could not render must
           never be cached, or the next page load would replay the same crash
           from localStorage before the network could correct it. */
        writeHivesCache(data.hives || []);
        renderPendingBanner(data.hives || []);
        renderUserAccessBanner();
        renderProvisionRequestBanner(data.my_provision_request || null);
        renderAdminProvisionRequests(data.provision_requests || []);
        loadPublicHives(data.hives || []);
        loadUsage();
      } catch(e) {
        /* Surface EVERY failure in the load→render path, not just fetch errors.
           The old guard only painted a message when _allDashHives was empty —
           but _allDashHives is assigned from the payload BEFORE renderHives()
           runs, so a throw inside rendering left the container showing
           "Loading your hives..." forever with nothing in the console. That is
           how an unbalanced brace in the inline JS (which silently nested a
           third of the dashboard's top-level declarations inside
           renderAlertRows) reached production unnoticed. Never swallow: log the
           real error AND give the operator a way to retry. */
        console.error('[hive] my-hives load/render failed:', e);
        renderHivesError(e);
      } finally {
        _hivesLoading = false;
      }
    }

    /* renderHivesError replaces the eternal spinner with an actionable message.
       Written with DOM APIs + addEventListener rather than innerHTML with an
       inline handler so the error text can never be interpreted as markup. */
    function renderHivesError(err) {
      var container = document.getElementById('hives-container');
      if (!container) return;
      /* Keep a good list on screen if we already have one — a failed refresh
         should not blank out the fleet the operator is looking at. */
      if (_allDashHives.length && !/Loading your hives/.test(container.innerHTML) &&
          !container.querySelector('.hives-load-error')) {
        return;
      }
      var box = document.createElement('div');
      box.className = 'empty-state hives-load-error';
      var title = document.createElement('p');
      title.style.cssText = 'font-size:1.1rem;margin-bottom:8px;color:var(--red)';
      title.textContent = 'Could not load your hives';
      var detail = document.createElement('p');
      detail.style.cssText = 'font-size:0.8rem;color:var(--muted);word-break:break-word';
      detail.textContent = (err && (err.message || String(err))) || 'Unknown error';
      var hint = document.createElement('p');
      hint.style.cssText = 'font-size:0.75rem;color:var(--muted);opacity:0.8;margin-top:6px';
      hint.textContent = 'Details were logged to the browser console.';
      var wrap = document.createElement('p');
      wrap.style.marginTop = '12px';
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'filter-chip filter-chip-clear';
      btn.textContent = 'Retry';
      btn.addEventListener('click', function() {
        container.innerHTML = '<div class="loading">Loading your hives...</div>';
        /* Force the next render past the signature short-circuit — the cached
           signature may still match the payload that failed to render. */
        _lastHivesJSON = '';
        loadHives();
      });
      wrap.appendChild(btn);
      box.appendChild(title);
      box.appendChild(detail);
      box.appendChild(hint);
      box.appendChild(wrap);
      container.innerHTML = '';
      container.appendChild(box);
    }

    async function loadPublicHives(myHives) {
      try {
        var resp = await fetch('/api/registry');
        var data = await resp.json();
        var allPublic = (data.hives || []).filter(function(h) { return h.isPublic !== false && h.hiveType === 'hosted'; });
        var myIds = {};
        (myHives || []).forEach(function(h) { myIds[h.id] = true; });
        var otherHives = allPublic.filter(function(h) { return !myIds[h.id]; });
        var section = document.getElementById('public-hives-section');
        if (!otherHives.length) { section.style.display = 'none'; return; }
        section.style.display = '';
        var statusResp = await fetch('/api/saas/access-status');
        var statusData = await statusResp.json();
        var accessMap = statusData.hives || {};
        var rows = otherHives.map(function(h) {
          var repoPath = h.org && h.primaryRepo ? h.org + '/' + h.primaryRepo : h.primaryRepo || '';
          // Link on the hive's own GitHub instance (github_host) — a GHE repo must
          // point at github.ibm.com, not 404 on github.com. Falls back to plain
          // text when there is no org/repo to build a valid link from.
          var repoHref = ghRepoURL(h.github_host, h.org, h.primaryRepo);
          var repoLink = repoPath ? (repoHref ? '<a href="' + repoHref + '" target="_blank" class="repo-link">' + esc(h.primaryRepo) + '</a>' : '<span class="repo-link">' + esc(h.primaryRepo) + '</span>') : '';
          var actionCell = '';
          var access = accessMap[h.id];
          if (access && access.status === 'accepted') {
            // Use the hive's heartbeat-reported dashboard URL (resolvedBase),
            // NOT a hardcoded <id>.hive.kubestellar.io host. Firewalled spokes
            // (e.g. vllm-d on *.apps.fmaas-vllm-d.fmaas.res.ibm.com) live on a
            // different domain, so the hardcoded host 503'd/failed to resolve.
            var cBase = resolvedBase(h);
            var actionCell2 = cBase
              ? '<a href="' + cBase + '/contribute" target="_blank" style="padding:3px 10px;background:rgba(34,197,94,0.15);color:#4ade80;border:1px solid rgba(34,197,94,0.3);border-radius:4px;font-size:0.7rem;text-decoration:none">Contribute</a>'
              : '<span style="padding:3px 10px;color:var(--muted);font-size:0.7rem" title="hive has not reported its dashboard URL yet">Contribute unavailable</span>';
            actionCell = actionCell2;
          } else if (access && access.status === 'pending') {
            actionCell = '<span style="padding:3px 10px;background:rgba(245,158,11,0.15);color:#fbbf24;border:1px solid rgba(245,158,11,0.3);border-radius:4px;font-size:0.7rem">Pending</span>';
          } else {
            actionCell = '<button onclick="dashRequestAccess(\'' + esc(h.id) + '\',this)" style="padding:3px 10px;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);border-radius:4px;font-size:0.7rem;cursor:pointer;border:1px solid rgba(59,130,246,0.3)">Request Access</button>';
          }
          return '<tr>' +
            '<td style="text-align:left">' + esc(h.name || h.id) + '</td>' +
            '<td>' + repoLink + '</td>' +
            '<td>' + acmmBadge(h.acmmLevel) + '</td>' +
            '<td>' + actionCell + '</td>' +
            '</tr>';
        }).join('');
        document.getElementById('public-hives-container').innerHTML =
          '<table class="hive-table"><thead><tr><th style="text-align:left">Hive</th><th>Repo</th><th>ACMM</th><th></th></tr></thead><tbody>' + rows + '</tbody></table>';
      } catch(e) {}
    }

    var _requestAccessHiveId = null;
    var _requestAccessBtn = null;

    function dashRequestAccess(hiveId, btn) {
      // A justification note is required, so collect it via the modal
      // rather than firing the request immediately.
      _requestAccessHiveId = hiveId;
      _requestAccessBtn = btn || null;
      var label = document.getElementById('request-access-hive-label');
      if (label) label.textContent = hiveId;
      var ta = document.getElementById('request-access-note');
      if (ta) ta.value = '';
      var submit = document.getElementById('request-access-submit');
      if (submit) { submit.disabled = false; submit.textContent = 'Send Request'; }
      document.getElementById('request-access-modal').style.display = 'flex';
      if (ta) ta.focus();
    }

    function closeRequestAccessModal() {
      document.getElementById('request-access-modal').style.display = 'none';
      _requestAccessHiveId = null;
      _requestAccessBtn = null;
    }

    async function submitRequestAccess() {
      var hiveId = _requestAccessHiveId;
      if (!hiveId) { closeRequestAccessModal(); return; }
      var ta = document.getElementById('request-access-note');
      var note = ta ? ta.value.trim() : '';
      if (!note) { hiveToast('Please explain why you need access', 'error'); if (ta) ta.focus(); return; }
      var submit = document.getElementById('request-access-submit');
      if (submit) { submit.disabled = true; submit.textContent = 'Sending...'; }
      var btn = _requestAccessBtn;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/request-access', {
          method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({note: note})
        });
        var data = await resp.json();
        if (!resp.ok) {
          hiveToast(data.error || 'Request failed', 'error');
          if (submit) { submit.disabled = false; submit.textContent = 'Send Request'; }
          return;
        }
        if (btn) btn.outerHTML = '<span style="padding:3px 10px;background:rgba(245,158,11,0.15);color:#fbbf24;border:1px solid rgba(245,158,11,0.3);border-radius:4px;font-size:0.7rem">Pending</span>';
        hiveToast('Access request sent!', 'success');
        closeRequestAccessModal();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
        if (submit) { submit.disabled = false; submit.textContent = 'Send Request'; }
      }
    }

    /* isPlaceholderHive identifies an UNASSIGNED pool slot: provStatus
       'available' is authoritative, with an 'available-*' org prefix as the
       fallback for placeholders that have not reported provStatus yet. Shared by
       the status-filter scoping and the Assigned/Unassigned section split so the
       two can never disagree about what counts as a placeholder. */
    function isPlaceholderHive(h) {
      if (!h) return false;
      return h.provStatus === 'available' || (!!h.org && h.org.indexOf('available-') === 0);
    }

    /* ---------- Bulk multi-select actions ----------
       _bulkSelected is the authoritative selection, keyed by hive id, so it
       survives re-render, re-sort and filter changes: the checkbox state is
       derived from this map on every render rather than read back out of the
       DOM. Ids that are no longer visible are pruned in syncBulkSelection so a
       hidden hive can never be swept into a bulk action the user cannot see. */
    var _bulkSelected = {};

    /* Client-side mirror of the server's maxBulkHivesPerRequest. The server is
       the enforcing authority; this only gives a better message than a 400. */
    var BULK_MAX_HIVES = 100;

    /* Actions that disrupt a running hive and therefore require confirmation
       naming the count. Auto-upgrade toggles only change a stored preference,
       so they are not in this set. */
    var BULK_DISRUPTIVE_ACTIONS = {'restart': 1, 'upgrade': 1, 'switch-branch': 1};

    /* Only hosted hives the user owns (or any hosted hive, for the hub admin)
       can be targeted — the same predicate the single-hive buttons use. Local
       hives have no hub-managed deployment to restart, and unassigned
       placeholders have nothing running yet. */
    function isBulkEligible(h) {
      if (!h || !h.id) return false;
      if (isPlaceholderHive(h)) return false;
      var isHosted = h.hiveType === 'hosted' || (h.id.indexOf('hosted-') === 0 || h.id.indexOf('saas-') === 0);
      if (!isHosted) return false;
      return h.role === 'owner' || _isAdmin;
    }

    function bulkSelectedIds() {
      var out = [];
      for (var k in _bulkSelected) { if (_bulkSelected[k]) out.push(k); }
      return out;
    }

    /* Drop selections for hives that are no longer present/eligible in the
       current view. Called at the top of every render so the count in the bulk
       bar always matches what is actually on screen. */
    function syncBulkSelection(visibleHives) {
      var live = {};
      var list = visibleHives || [];
      for (var i = 0; i < list.length; i++) {
        if (isBulkEligible(list[i])) live[list[i].id] = true;
      }
      for (var k in _bulkSelected) {
        if (!live[k]) delete _bulkSelected[k];
      }
    }

    function toggleBulkHive(id, checked) {
      if (checked) _bulkSelected[id] = true; else delete _bulkSelected[id];
      renderBulkBar();
      syncBulkSectionHeaderBoxes();
    }

    /* Select-all is per section (Assigned / Unassigned) so an admin cannot
       accidentally sweep the whole page from one box. section is the key used
       by the header checkbox id. */
    function toggleBulkSection(section, checked) {
      var boxes = document.querySelectorAll('input[type=checkbox][data-bulk-section="' + section + '"]');
      for (var i = 0; i < boxes.length; i++) {
        var id = boxes[i].getAttribute('data-bulk-id');
        if (!id) continue;
        boxes[i].checked = checked;
        if (checked) _bulkSelected[id] = true; else delete _bulkSelected[id];
      }
      renderBulkBar();
    }

    /* Put each section header box into checked / indeterminate / unchecked to
       match its rows. */
    function syncBulkSectionHeaderBoxes() {
      var heads = document.querySelectorAll('input[type=checkbox][data-bulk-section-head]');
      for (var i = 0; i < heads.length; i++) {
        var section = heads[i].getAttribute('data-bulk-section-head');
        var boxes = document.querySelectorAll('input[type=checkbox][data-bulk-section="' + section + '"]');
        var total = boxes.length, sel = 0;
        for (var j = 0; j < boxes.length; j++) {
          if (_bulkSelected[boxes[j].getAttribute('data-bulk-id')]) sel++;
        }
        heads[i].checked = total > 0 && sel === total;
        heads[i].indeterminate = sel > 0 && sel < total;
      }
    }

    function clearBulkSelection() {
      _bulkSelected = {};
      var boxes = document.querySelectorAll('input[type=checkbox][data-bulk-id]');
      for (var i = 0; i < boxes.length; i++) { boxes[i].checked = false; }
      renderBulkBar();
      syncBulkSectionHeaderBoxes();
    }

    /* Row checkbox markup. section scopes it to a select-all group. */
    function bulkCheckboxCell(h, section) {
      if (!isBulkEligible(h)) {
        return '<td style="width:26px;text-align:center"></td>';
      }
      var checked = _bulkSelected[h.id] ? ' checked' : '';
      return '<td style="width:26px;text-align:center">' +
        '<input type="checkbox" data-bulk-id="' + esc(h.id) + '" data-bulk-section="' + esc(section) + '"' + checked +
        ' onclick="event.stopPropagation()" onchange="toggleBulkHive(\'' + esc(h.id) + '\',this.checked)"' +
        ' title="Select for bulk actions" style="cursor:pointer"></td>';
    }

    function bulkSectionCheckbox(section) {
      /* stopPropagation because the section-header row is itself a click target
         that expands/collapses the section — without this, ticking select-all
         would also collapse the very rows it just selected. */
      return '<input type="checkbox" data-bulk-section-head="' + esc(section) + '"' +
        ' onclick="event.stopPropagation()"' +
        ' onchange="toggleBulkSection(\'' + esc(section) + '\',this.checked)"' +
        ' title="Select all in this section" style="cursor:pointer;margin-right:8px;vertical-align:middle">';
    }

    function renderBulkBar() {
      var bar = document.getElementById('bulk-action-bar');
      if (!bar) return;
      var ids = bulkSelectedIds();
      if (!ids.length) { bar.style.display = 'none'; bar.innerHTML = ''; return; }
      var btn = 'padding:5px 12px;border-radius:4px;cursor:pointer;font-size:0.72rem;white-space:nowrap;border:1px solid var(--border);background:var(--surface);color:var(--text)';
      var over = ids.length > BULK_MAX_HIVES;
      var branchOpts = '';
      var branches = _trackedBranchesList.length > 0 ? _trackedBranchesList : Object.keys(_latestSHAs);
      for (var bi = 0; bi < branches.length; bi++) {
        branchOpts += '<option value="' + esc(branches[bi]) + '">' + esc(branches[bi]) + '</option>';
      }
      var branchPicker = branches.length > 0
        ? '<select id="bulk-branch-select" style="' + btn + ';padding:4px 8px"><option value="">Switch branch…</option>' + branchOpts + '</select>'
        : '';
      bar.innerHTML =
        '<div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;margin-bottom:14px;padding:10px 14px;' +
        'background:rgba(59,130,246,0.08);border:1px solid rgba(59,130,246,0.3);border-radius:8px">' +
        '<strong style="font-size:0.8rem;color:#60a5fa">' + ids.length + (ids.length === 1 ? ' hive' : ' hives') + ' selected</strong>' +
        (over ? '<span style="font-size:0.72rem;color:var(--red)">Over the ' + BULK_MAX_HIVES + '-hive limit — deselect some.</span>' : '') +
        '<span style="flex:1"></span>' +
        '<button type="button" onclick="runBulkAction(\'restart\')" style="' + btn + '">Restart</button>' +
        '<button type="button" onclick="runBulkAction(\'upgrade\')" style="' + btn + '">Upgrade to latest</button>' +
        '<button type="button" onclick="runBulkAction(\'enable-auto-upgrade\')" style="' + btn + '">Auto: instant</button>' +
        '<button type="button" onclick="runBulkAction(\'daily-auto-upgrade\')" style="' + btn + '" title="Upgrade at most once a day, midday — keeps a stable hive from being restarted mid-work, and puts a bad roll in staffed hours">Auto: daily 1pm ET</button>' +
        '<button type="button" onclick="runBulkAction(\'disable-auto-upgrade\')" style="' + btn + '">Auto-upgrade off</button>' +
        branchPicker +
        '<button type="button" onclick="clearBulkSelection()" style="' + btn + ';color:var(--muted)">Clear</button>' +
        '</div>';
      bar.style.display = '';
      var sel = document.getElementById('bulk-branch-select');
      if (sel) {
        sel.onchange = function() {
          var b = sel.value;
          sel.value = '';
          if (b) runBulkAction('switch-branch', b);
        };
      }
    }

    var BULK_ACTION_LABELS = {
      'restart': 'Restart',
      'upgrade': 'Upgrade to latest',
      'enable-auto-upgrade': 'Enable instant auto-upgrade on',
      'daily-auto-upgrade': 'Enable daily 1pm ET auto-upgrade on',
      'disable-auto-upgrade': 'Disable auto-upgrade on',
      'switch-branch': 'Switch branch for'
    };

    async function runBulkAction(action, branch) {
      var ids = bulkSelectedIds();
      if (!ids.length) { hiveToast('No hives selected', 'error'); return; }
      if (ids.length > BULK_MAX_HIVES) {
        hiveToast('Select at most ' + BULK_MAX_HIVES + ' hives per bulk action', 'error');
        return;
      }
      var label = BULK_ACTION_LABELS[action] || action;
      var noun = ids.length === 1 ? 'hive' : 'hives';
      /* Confirmation names BOTH the action and the count — restarting a batch
         of live hives must never be one misclick. Disruptive actions also list
         the hives so the user can see exactly what is in the batch. */
      if (BULK_DISRUPTIVE_ACTIONS[action]) {
        var names = ids.slice(0, 10).map(function(i) { return esc(i); }).join('<br>');
        if (ids.length > 10) names += '<br>… and ' + (ids.length - 10) + ' more';
        var msg = '<strong>' + esc(label) + (branch ? ' ' + esc(branch) : '') + '</strong> on <strong>' +
          ids.length + ' ' + noun + '</strong>?<br><br>' +
          '<div style="font-family:monospace;font-size:0.75rem;color:var(--muted);text-align:left;max-height:180px;overflow:auto">' + names + '</div>';
        if (!await hiveConfirm(msg, true)) return;
      }
      hiveToast(label + ' — ' + ids.length + ' ' + noun + '…', 'info');
      try {
        var resp = await fetch('/api/saas/hives/bulk', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({action: action, hive_ids: ids, branch: branch || ''})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast((data && data.error) || 'Bulk action failed', 'error'); return; }
        showBulkResults(label, data);
        clearBulkSelection();
        loadHives();
        setTimeout(loadHives, 10000);
        setTimeout(loadHives, 30000);
        setTimeout(loadHives, 60000);
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
      }
    }

    /* Partial failure is the normal case, so always show the per-hive
       breakdown rather than a single success toast: the user needs to know
       WHICH hives failed and why. */
    function showBulkResults(label, data) {
      var results = (data && data.results) || [];
      var ok = data.succeeded || 0, failed = data.failed || 0;
      var failedRows = '';
      var heartbeatCount = 0;
      for (var i = 0; i < results.length; i++) {
        var r = results[i] || {};
        if (r.ok) { if (r.via === 'heartbeat') heartbeatCount++; continue; }
        failedRows += '<div style="display:flex;gap:8px;padding:3px 0;font-size:0.72rem">' +
          '<span style="font-family:monospace;color:var(--red);flex:none">' + esc(r.hive_id || '?') + '</span>' +
          '<span style="color:var(--muted)">' + esc(r.error || 'failed') + '</span></div>';
      }
      if (!failed) {
        var extra = heartbeatCount
          ? ' (' + heartbeatCount + ' queued for delivery on next check-in)'
          : '';
        hiveToast(label + ': ' + ok + ' succeeded' + extra, 'success');
        return;
      }
      hiveConfirm('<strong>' + esc(label) + '</strong><br><br>' +
        '<span style="color:var(--green)">' + ok + ' succeeded</span>' +
        (heartbeatCount ? ' <span style="color:var(--muted);font-size:0.75rem">(' + heartbeatCount + ' via heartbeat)</span>' : '') +
        ' &nbsp; <span style="color:var(--red)">' + failed + ' failed</span>' +
        '<div style="margin-top:10px;text-align:left;max-height:220px;overflow:auto">' + failedRows + '</div>', true);
    }

    function renderHives(allHives, force) {
      allHives = allHives || [];
      /* The signature must include EVERY piece of render-affecting view state,
         otherwise changing it while the hive data is unchanged is silently a
         no-op — toggling a chip, drilling into an alert type, expanding the
         acknowledged list, typing in the search box, picking a facet,
         collapsing a section, switching group-by, collapsing a group or
         applying a saved view would all appear to do nothing. */
      var sig = JSON.stringify(allHives) + '|' + JSON.stringify(_dashStatusFilters) +
        '|' + _dashFailingCheckFilter + '|' + _dashDriftFilter + '|' + _dashUpgradeFilter +
        '|' + _dashAdvisoryStaleFilter +
        '|' + _alertTypeFilter + '|' + _alertShowAcked + '|' + JSON.stringify(_fleetAlerts) +
        /* Without this, expanding "+N more" changes no hive data and the
           early-return below would make the click a silent no-op. */
        '|' + JSON.stringify(_alertRowsExpanded) +
        '|' + _dashSearchQuery + '|' + JSON.stringify(_dashFacets) +
        '|' + JSON.stringify(_dashFacetCollapsed) + '|' + JSON.stringify(_dashSectionCollapsed) +
        '|' + _dashGroupBy + '|' + JSON.stringify(_dashGroupCollapsed) +
        '|' + _dashActiveView + '|' + _dashDefaultView + '|' + JSON.stringify(_dashSavedViews) +
        '|' + _dashFacetTrayOpen;
      if (!force && sig === _lastHivesJSON) return;
      _lastHivesJSON = sig;
      /* Status filters describe ASSIGNED hives only. An unassigned placeholder
         has no GitHub App, no tokens and no real health to speak of, so every
         chip would appear to "hide" the whole pool — and filtering to e.g.
         Degraded made the Unassigned section vanish, which reads as the
         placeholders having been deleted. Split first, filter only the assigned
         side, and leave the pool alone. */
      var assignedAll = [], unassignedAll = [];
      for (var _si = 0; _si < allHives.length; _si++) {
        (isPlaceholderHive(allHives[_si]) ? unassignedAll : assignedAll).push(allHives[_si]);
      }
      var hives = applyDashFilters(assignedAll).concat(unassignedAll);
      var filterBar = document.getElementById('hive-filter-bar');
      if (filterBar) filterBar.style.display = allHives.length ? '' : 'none';
      var searchRow = document.getElementById('hive-search-row');
      if (searchRow) searchRow.style.display = allHives.length ? '' : 'none';
      /* Nothing renderable below means no rows to act on — drop the selection
         so the bulk bar can't linger over an empty or fully-filtered table. */
      if (!allHives.length || !hives.length) {
        _bulkSelected = {};
        renderBulkBar();
      }
      /* The view bar shows whenever there are hives at all — including when the
         status filters currently match none of them, so the operator can still
         switch grouping or restore a saved view from the empty state. */
      var viewBar = document.getElementById('hive-view-bar');
      if (viewBar) viewBar.style.display = allHives.length ? '' : 'none';
      renderViewBar();
      /* Counts are over the assigned set only, matching what the chips filter. */
      renderStatusFilterBar(assignedAll, hives.length - unassignedAll.length);
      /* Facets are offered over the assigned set for the same reason the chips
         are: a placeholder carries no cluster, role or branch worth faceting. */
      renderFacetRail(assignedAll);
      /* Drift exceptions are scoped to assigned hives for the same reason: a
         placeholder is never flagged for claimed-hive concerns server-side. */
      renderDriftSummary(assignedAll);
      /* Drawn BEFORE the empty-state early-returns below: a fleet whose every
         hive is filtered out still has alerts worth showing, and the panel is
         how the operator gets back out of a drill-down. */
      renderAlertsPanel();
      if (!allHives.length) {
        var driftEl0 = document.getElementById('hive-drift-summary');
        if (driftEl0) driftEl0.style.display = 'none';
        document.getElementById('hives-container').innerHTML =
          '<div class="empty-state">' +
          '<p style="font-size:1.2rem;margin-bottom:8px">No hives yet</p>' +
          '<p>Log in to a local hive dashboard to see it here, or create a hosted hive.</p>' +
          '</div>';
        return;
      }
      if (!hives.length) {
        /* Hives exist, but every one was filtered out — say so, and offer the
           way back rather than looking like the list failed to load. Only
           assigned hives can be hidden, so report that count, not the total. */
        document.getElementById('hives-container').innerHTML =
          '<div class="empty-state">' +
          '<p style="font-size:1.2rem;margin-bottom:8px">No hives match these filters</p>' +
          '<p>' + assignedAll.length + (assignedAll.length === 1 ? ' hive is' : ' hives are') + ' hidden by the search, facets or status filters.</p>' +
          /* clearAllHiveFilters, not clearStatusFilters: a search term, a facet
             or an alert drill-down can empty the list too, and a button that
             only clears the chips would leave the operator stuck looking at an
             empty table. */
          '<p style="margin-top:12px"><button type="button" class="filter-chip filter-chip-clear" onclick="clearAllHiveFilters()">Clear filters</button></p>' +
          '</div>';
        return;
      }
      /* Prune selections for hives that are no longer visible BEFORE building
         rows, so the checkbox state and the bulk bar's count agree. */
      syncBulkSelection(hives);
      var repoPath = function(h) { return h.org && h.primaryRepo ? h.org + '/' + h.primaryRepo : h.primaryRepo || ''; };
      var buildRow = function(h, i, section) {
        var dot = h.online ? healthBadge(h) : '<span class="online-dot off"></span>';
        var rp = repoPath(h);
        // Link on the hive's own GitHub instance (github_host) so a GHE repo
        // points at github.ibm.com, not 404 on github.com.
        var repoHref = ghRepoURL(h.github_host, h.org, h.primaryRepo);
        var repoLink = rp ? (repoHref ? '<a href="' + repoHref + '" target="_blank" class="repo-link">' + esc(h.primaryRepo) + '</a>' : '<span class="repo-link">' + esc(h.primaryRepo) + '</span>') : '';
        var repoCount = (h.repos || []).length;
        var isHosted = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-')));
        var isLocal = !isHosted;
        var canConvert = isLocal && h.role === 'owner' && (_userQuota < 0 || _userQuota > _userUsed);
        var modeCell = h.provStatus === 'error'
          ? '<span style="color:var(--red);cursor:help;white-space:nowrap" title="' + esc(h.provError || '') + '">⚠ ERROR</span>'
          : h.assigning
          ? '<span style="color:var(--accent);white-space:nowrap" title="Waiting for the spoke to report the new project via heartbeat"><span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>Assigning to ' + esc(h.assigningTo || '?') + '</span>'
          : h.provStatus === 'provisioning'
          ? '<span style="color:var(--accent);white-space:nowrap">⏳ Provisioning</span>'
          : h.migrationStatus === 'migrating'
          ? '<span style="color:var(--accent);white-space:nowrap"><span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>Migrating to ' + esc(h.migrationTo || '?') + '</span>'
          : h.migrationStatus === 'failed'
          ? '<span style="color:var(--red);cursor:help;white-space:nowrap" title="' + esc(h.provError || '') + '">⚠ Migration failed</span>'
          : modeBadge(h.governorMode);
        var rb = resolvedBase(h);
        var contributeUrl = rb ? rb + '/contribute' : '';
        var actions = '';
        if (canConvert) {
          actions = '<button onclick="openConvert(this)" data-hive-id="' + esc(h.id) + '" data-dash-url="' + esc(h.dashboardUrl||'') + '" data-org="' + esc(h.org) + '" data-repos="' + esc((h.repos||[]).join(', ')) + '" data-primary="' + esc(h.primaryRepo) + '" data-level="' + (h.acmmLevel||1) + '" data-name="' + esc(h.name||'') + '" style="padding:3px 10px;background:var(--accent);color:#000;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;white-space:nowrap">Convert to Hosted</button>';
          if (h.role === 'owner') {
            actions += '<br style="margin-bottom:4px"><button onclick="removeLocalHive(\'' + esc(h.id) + '\')" style="margin-top:6px;padding:3px 10px;background:var(--surface);color:var(--muted);border:1px solid var(--border);border-radius:4px;cursor:pointer;font-size:0.65rem;white-space:nowrap" title="Remove from registry (does not delete the hive)">Remove</button>';
          }
        } else if (isHosted && (h.role === 'owner' || h.role === 'read-write')) {
          actions = '<button onclick="openAccessModal(\'' + esc(h.id) + '\',\'' + esc(h.dashboardUrl || '') + '\')" style="padding:3px 10px;background:var(--blue);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;white-space:nowrap;margin-right:4px">Permissions</button>';
          if (h.role === 'owner') {
            actions += '<button onclick="deleteHive(\'' + esc(h.id) + '\')" style="padding:3px 10px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;white-space:nowrap">Delete</button>';
          }
        }
        var menuId = 'hive-menu-' + i;
        var dashUrl = dashboardLink(h);
        var snapUrl = snapshotLink(h);
        var apiUrl = apiLink(h);
        var menuItems = [];
        var mi = 'display:block;padding:7px 14px;color:#c9d1d9;text-decoration:none;font-size:0.78rem;cursor:pointer';
        if (_isAdmin && h.provStatus === 'available' && !h.assigning) menuItems.push('<div onclick="openAssignModal(\'' + esc(h.id) + '\')" style="' + mi + ';color:#3fb950;font-weight:600">Assign / Claim</div><div style="border-top:1px solid #30363d;margin:4px 0"></div>');
        if (contributeUrl) menuItems.push('<a href="' + contributeUrl + '" target="_blank" style="' + mi + '">Contribute</a>');
        if (h.snapshotUrl) menuItems.push('<a href="' + esc(h.snapshotUrl) + '" target="_blank" style="' + mi + '">Preview</a>');
        var apiBase = rb ? esc(rb) : '';
        if (apiBase) menuItems.push('<a href="' + apiBase + '/api/docs" target="_blank" style="' + mi + '">API Docs</a>');
        if (menuItems.length > 0 && (canConvert || isHosted || isLocal)) menuItems.push('<div style="border-top:1px solid #30363d;margin:4px 0"></div>');
        if (canConvert) menuItems.push('<div onclick="openConvert(this)" data-hive-id="' + esc(h.id) + '" data-dash-url="' + esc(h.dashboardUrl||'') + '" data-org="' + esc(h.org) + '" data-repos="' + esc((h.repos||[]).join(', ')) + '" data-primary="' + esc(h.primaryRepo) + '" data-level="' + (h.acmmLevel||1) + '" data-name="' + esc(h.name||'') + '" style="' + mi + '">Convert to Hosted</div>');
        if (isHosted && (h.role === 'owner' || h.role === 'read-write')) menuItems.push('<div onclick="openAccessModal(\'' + esc(h.id) + '\',\'' + esc(h.dashboardUrl || '') + '\')" style="' + mi + '">Permissions</div>');
        /* Timeline is owner-or-admin, matching the API's authorization. */
        if (isHosted && (h.role === 'owner' || _isAdmin)) menuItems.push('<div onclick="openTimelineModal(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">Activity Timeline</div>');
        if (h.role === 'owner' || h.role === 'read-write' || _isAdmin) menuItems.push('<div onclick="openOpenRouterFundModal(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">⚡ Fund with OpenRouter</div>');
        if (_isAdmin && isHosted) menuItems.push('<div onclick="openBannerForHive(\'' + esc(h.id) + '\',\'' + esc(h.name || h.id) + '\')" style="' + mi + '">Send Banner</div>');
        if (isLocal && h.role === 'owner') menuItems.push('<div onclick="removeLocalHive(\'' + esc(h.id) + '\')" style="' + mi + '">Remove</div>');
        if (isHosted && h.role === 'owner' && _clusterList && _clusterList.length > 1 && h.migrationStatus !== 'migrating') menuItems.push('<div onclick="openMigrateModal(\'' + esc(h.id) + '\',\'' + esc(h.clusterId || '') + '\')" style="' + mi + '">Move to cluster</div>');
        if (isHosted && h.role === 'owner') menuItems.push('<div style="border-top:1px solid #30363d;margin:4px 0"></div><div onclick="deleteHive(\'' + esc(h.id) + '\')" style="' + mi + ';color:#f85149">Delete</div>');
        var sha = h.gitHash || '';
        var versionCell = '';
        if (sha) {
          var branchName = h.gitBranch || 'v2';
          var branchLatest = _latestSHAs[branchName] || _latestSHA;
          var _trackedBranches = _trackedBranchesList.length > 0 ? _trackedBranchesList : Object.keys(_latestSHAs);
          if (_trackedBranches.length === 0) _trackedBranches = ['v2'];
          var canSwitchBranch = isHosted && h.role === 'owner' && _trackedBranches.length > 1 && !h.upgrading;
          var branchOptions = '';
          if (canSwitchBranch) {
            for (var bi = 0; bi < _trackedBranches.length; bi++) {
              var tb = _trackedBranches[bi];
              if (tb !== branchName) {
                branchOptions += '<div onclick="event.stopPropagation();switchBranch(\'' + esc(h.id) + '\',\'' + esc(tb) + '\',this)" style="padding:4px 10px;cursor:pointer;font-size:0.65rem;white-space:nowrap;color:#c9d1d9;border-radius:4px" onmouseover="this.style.background=\'rgba(59,130,246,0.2)\'" onmouseout="this.style.background=\'transparent\'">' + esc(tb) + '</div>';
              }
            }
          }
          var branch = canSwitchBranch
            ? '<span id="branch-pill-' + esc(h.id) + '" style="display:inline-block;position:relative;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);cursor:pointer" onclick="toggleBranchMenu(\'' + esc(h.id) + '\')" title="Click to switch branch">' + esc(branchName) + ' ▾<div id="branch-menu-' + esc(h.id) + '" style="display:none;position:absolute;top:100%;left:0;margin-top:4px;background:#1c2128;border:1px solid #30363d;border-radius:6px;padding:4px 0;z-index:1000;min-width:60px;box-shadow:0 4px 12px rgba(0,0,0,0.4)">' + branchOptions + '</div></span>'
            : '<span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3)">' + esc(branchName) + '</span>';
          var latestUnknown = !branchLatest;
          var isCurrent = branchLatest && sameShaJS(sha, branchLatest);
          /* Branch switch in flight: the hive still reports the OLD branch
             (often current on it) until the new pod heartbeats — without
             this, isCurrent suppresses every progress indicator. */
          /* Switch-vs-upgrade is decided in ONE place (hiveUpgradeState) so the
             spinner, label and title always agree. Only a target on a DIFFERENT
             branch is a switch; a plain-SHA (auto-upgrade) target always reads
             "Upgrading", even if a stale switch sentinel lingers. */
          var upgradeState = hiveUpgradeState(h, branchName);
          var isSwitching = upgradeState.isSwitching;
          var targetBranch = upgradeState.targetBranch;
          /* Drop a stale switch sentinel (resolved to a same-branch SHA) so it
             stops forcing the upgrading state on later auto-upgrades. */
          if (upgradeState.switchSentinelStale) delete _upgradingHives[h.id];
          var sentinel = _upgradingHives[h.id];
          var isUpgrading = isSwitching ||
            (sentinel && sha === sentinel) || (h.upgrading && !isCurrent && !latestUnknown);
          if (sentinel && sha !== sentinel && !isSwitching) { delete _upgradingHives[h.id]; delete _switchStartedAt[h.id]; }
          if (isCurrent && h.upgrading && !isSwitching) { h.upgrading = false; }
          var imageBuilding = (_latestImageStatus[branchName] || '') === 'building';
          var buildingHint = imageBuilding ? ' (image still building — upgrading now pulls the previous image)' : '';
          var status = latestUnknown
            ? ' <span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.2);border-top-color:var(--accent);border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-left:3px" title="Resolving latest version…"></span>'
            : isCurrent ? '<span style="color:var(--green);margin-left:3px" title="latest">✓</span>' : '<span style="color:var(--red);margin-left:3px" title="behind latest ' + esc(branchLatest) + '">↑</span>';
          var upgradeIcon = '';
          if (isUpgrading) {
            var switchStale = isSwitching && _switchStartedAt[h.id] && (Date.now() - _switchStartedAt[h.id] > SWITCH_STALE_MS);
            var progressLabel = isSwitching ? (switchStale ? 'Switching to ' + esc(targetBranch) + ' — taking longer than expected' : 'Switching to ' + esc(targetBranch)) : 'Upgrading';
            var progressTitle = isSwitching ? (switchStale ? 'The hive has not reported branch ' + esc(targetBranch) + ' yet — it may be offline or its build predates in-cluster switch support. It will apply on its next successful check-in.' : 'Rolling out ' + esc(h.upgradeTarget || '') + ' — the pill updates when the hive reports the new branch') : 'Upgrading to ' + esc(branchLatest || h.upgradeTarget || '?');
            /* In-flight state is not clickable, so it loses the box entirely:
               a border around a non-target was the padding that made this the
               tall line. The spinner is the affordance-free progress signal. */
            /* The elapsed counter rides INSIDE the pill so a long-running
               upgrade cannot be read as progress. It self-suppresses when the
               start time is unknown or the rollout is still fast (see
               renderUpgradeElapsed), so a healthy fleet looks unchanged. */
            upgradeIcon = '<span title="' + progressTitle + '" style="font-size:0.7rem;white-space:nowrap;opacity:0.8;color:var(--muted)"><span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:currentColor;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>' + progressLabel + '</span>' + renderUpgradeElapsed(h);
          } else if (!isCurrent && !latestUnknown && isHosted && h.role === 'owner' && h.autoUpgrade) {
            /* A daily-mode hive is NOT upgrading "shortly" — saying so would be
               a lie the operator would notice hours later. Name the window and
               keep the click-to-upgrade-now escape hatch, which is the manual
               path and is never gated by the schedule. */
            var queuedDaily = h.autoUpgradeMode === AUTO_UPGRADE_DAILY;
            var queuedLabel = queuedDaily ? 'queued · 1pm ET' : 'queued';
            var queuedTitle = queuedDaily
              ? 'Auto-upgrade will apply ' + esc(branchLatest) + ' at the next 1pm ET window — click to upgrade now' + esc(buildingHint)
              : 'Auto-upgrade will apply ' + esc(branchLatest) + ' shortly — click to upgrade now' + esc(buildingHint);
            /* escAttr, not esc, for the title: esc() leaves quotes intact and a
               branch name or commit subject carrying one would break out of the
               attribute. jsArg supplies its own quotes for the handler args. */
            upgradeIcon = '<span id="upgrade-' + escAttr(h.id) + '" role="button" tabindex="0"' +
              ' onclick="upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')"' +
              ' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')}"' +
              ' title="' + escAttr(queuedTitle) + '" style="' + UPGRADE_LINK_STYLE + ';color:var(--muted);font-size:0.7rem;text-decoration-style:dashed">' + esc(queuedLabel) + '</span>';
          } else if (!isCurrent && !latestUnknown && isHosted && h.role === 'owner') {
            /* Text-with-an-underline rather than a filled button: on its own
               line the padding bought no extra affordance and cost 8px of row
               height. Green still marks it as the go-forward action. */
            upgradeIcon = '<span id="upgrade-' + escAttr(h.id) + '" role="button" tabindex="0"' +
              ' onclick="upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')"' +
              ' onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();upgradeHive(' + jsArg(h.id) + ',' + jsArg(sha) + ',' + jsArg(branchName) + ')}"' +
              ' title="' + escAttr('Current: ' + sha + ' → Latest: ' + branchLatest + buildingHint) + '" style="' + UPGRADE_LINK_STYLE + ';color:var(--green);font-weight:600;font-size:0.7rem">Upgrade</span>';
          } else if (latestUnknown && isHosted && h.role === 'owner') {
            upgradeIcon = '<span title="Waiting for latest version…" style="font-size:0.7rem;color:var(--muted);white-space:nowrap;cursor:not-allowed;opacity:0.5">Upgrade</span>';
          }
          /* Auto-upgrade control: a single select replaces the old checkbox.
             The dense table has no room for a checkbox PLUS a mode dropdown,
             and two controls for one setting invites the illegal-looking
             "off but daily" state. One select shows the effective mode at a
             glance — no dialog, no hover needed — which is the whole point:
             an operator scanning the fleet must be able to see which hives
             restart on sight of a new commit and which wait for the evening.
             The id travels in a data-* attribute and the handler is bound by
             delegation (see bindAutoUpgradeSelects) because esc() does NOT
             escape quotes and is unsafe to interpolate into an attribute. */
          var autoUpgradeCheck = '';
          if (isHosted && h.role === 'owner') {
            var mode = h.autoUpgrade ? (h.autoUpgradeMode === AUTO_UPGRADE_DAILY ? AUTO_UPGRADE_DAILY : AUTO_UPGRADE_INSTANT) : AUTO_UPGRADE_OFF;
            var opts = '';
            for (var oi = 0; oi < AUTO_UPGRADE_OPTIONS.length; oi++) {
              var opt = AUTO_UPGRADE_OPTIONS[oi];
              /* Each OPTION carries its own full-meaning title too, so opening
                 the list explains the three icons without a legend elsewhere. */
              opts += '<option value="' + escAttr(opt.value) + '" title="' + escAttr(opt.ariaLabel) + '"' + (mode === opt.value ? ' selected' : '') + '>' + esc(opt.label) + '</option>';
            }
            var d = document.createElement('select');
            d.className = 'auto-upgrade-select';
            d.setAttribute('data-hive-id', h.id);
            /* The label is now icons only, so the meaning has to live in the
               accessible name and the tooltip or it is simply lost. aria-label
               names the CURRENT mode; title explains what the modes are. */
            d.setAttribute('aria-label', autoUpgradeAriaLabel(mode));
            d.title = autoUpgradeAriaLabel(mode) + ' — ' + AUTO_UPGRADE_TITLE;
            d.setAttribute('style', 'font-size:0.65rem;color:var(--muted);background:var(--surface);border:1px solid var(--border);border-radius:4px;' +
              'height:' + AUTO_SELECT_H_PX + 'px;min-width:' + AUTO_SELECT_MIN_W_PX + 'px;padding:0 ' + AUTO_SELECT_PAD_X_PX + 'px;cursor:pointer;vertical-align:middle;line-height:' + AUTO_SELECT_H_PX + 'px');
            d.innerHTML = opts;
            autoUpgradeCheck = d.outerHTML;
          }
          var shaMsg = _commitMessages[sha] || _latestSHAMessages[branchName] || '';
          /* FOUR stacked lines, ONE element per line. The column is as wide as
             its widest LINE, so as long as no two controls share a line the
             width collapses to the widest single element — which, now that the
             select is icon-only, is the 56px select rather than the ~150px
             "[queued · 1pm ET] [Auto: daily 1pm ET]" pair that used to set it.
               1. the branch pill, which the owner can CHANGE (it is a switcher,
                  so it owns a line and stays a full tap target);
               2. the SHA and its current/behind glyph — two views of the same
                  immutable fact, "what commit is this hive on". These are one
                  element for width purposes: the glyph is 10px and rides
                  alongside the hash rather than setting the line's width.
               3. what happens NEXT: the upgrade affordance alone;
               4. the icon-only auto-upgrade mode select alone.
             Lines 3 and 4 are omitted entirely when empty — a read-only viewer
             gets two lines, not four with two blanks. Every fragment below is
             embedded verbatim, so ids, handlers and tooltips are preserved. */
          var shaLine = '<span style="font-family:monospace;color:var(--muted)" title="' + escAttr(shaMsg) + '">' + esc(sha) + '</span>' + status;
          versionCell = '<div style="' + STACKED_CELL_STYLE + '">' +
            '<div style="' + STACKED_LINE_STYLE + '">' + branch + '</div>' +
            '<div style="' + STACKED_LINE_STYLE + '">' + shaLine + '</div>' +
            (upgradeIcon ? '<div style="' + STACKED_LINE_STYLE + '">' + upgradeIcon + '</div>' : '') +
            (autoUpgradeCheck ? '<div style="' + STACKED_LINE_STYLE + '">' + autoUpgradeCheck + '</div>' : '') +
            '</div>';
        } else { versionCell = '<span style="color:var(--muted)">—</span>'; }
        var pendingBadge = (h.pendingRequestCount > 0 && (h.role === 'owner' || h.role === 'read-write'))
          ? '<span style="position:absolute;top:-2px;right:-2px;background:var(--blue);color:#fff;border-radius:50%;width:16px;height:16px;font-size:0.6rem;display:flex;align-items:center;justify-content:center;font-weight:700">' + h.pendingRequestCount + '</span>'
          : '';
        var pendingPill = '';
        if (h.pendingRequestCount > 0 && (h.role === 'owner' || h.role === 'read-write')) {
          pendingPill = '<a href="#" onclick="togglePendingRow(\'' + esc(h.id) + '\');return false" style="display:inline-flex;align-items:center;gap:4px;padding:3px 10px;background:rgba(59,130,246,0.12);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);border-radius:4px;font-size:0.7rem;text-decoration:none;cursor:pointer;white-space:nowrap">&#x1F514; ' + h.pendingRequestCount + ' pending</a>';
        }
        // 16 = the 13 original columns, plus Uptime, plus Drift, plus the
        // bulk-select column, plus Journey, plus Provisioned, MINUS Public —
        // visibility now stacks under Location instead of owning a column.
        // Counted against the <th> cells in the header and the <td> cells
        // emitted below (bulkCheckboxCell contributes one).
        var TOTAL_COLUMNS = 18;
        /* Visibility moved OUT of its own column and under Location: "where
           does this hive run" and "who can see it" are both facts about the
           hive's placement, so they read as one cell, and folding them saves a
           whole column in a table that had 17.

           All THREE original branches survive unchanged — the owner toggle
           (with its 'vis-<id>' element id and onchange handler), the local
           link into Governor Config, and the read-only ✓/— for everyone else.
           They are deliberately NOT flattened: each answers a different
           question about who may change the setting. */
        var visibilityCell = (function() {
          var pub = !!h.isPublic;
          /* escAttr, not esc: this id lands inside a quoted attribute and a
             hive id carrying a quote would otherwise break out of it. Still
             'vis-' + the hive id, so it stays unique per row exactly as before. */
          var tid = 'vis-' + escAttr(h.id);
          /* Branch 1 — hosted hive the viewer OWNS: a live toggle. The owner is
             the only person who may change listing, so they get the control. */
          if (isHosted && h.role === 'owner') {
            return '<label style="position:relative;display:inline-block;width:' + VIS_SWITCH_W_PX + 'px;height:' + VIS_SWITCH_H_PX + 'px;cursor:pointer">' +
              '<input type="checkbox" id="' + tid + '"' + (pub ? ' checked' : '') +
              /* jsArg (not esc) for the handler argument: esc leaves an
                 apostrophe intact, which would close the attribute quote early.
                 jsArg supplies its own surrounding quotes. */
              ' onchange="toggleVisibility(' + jsArg(h.id) + ',this.checked)" style="opacity:0;width:0;height:0">' +
              '<span style="position:absolute;inset:0;background:' + (pub ? 'var(--green)' : 'var(--border)') + ';border-radius:' + VIS_SWITCH_RADIUS_PX + 'px;transition:background 0.2s"></span>' +
              '<span style="position:absolute;top:' + VIS_KNOB_INSET_PX + 'px;left:' + (pub ? VIS_KNOB_ON_LEFT_PX : VIS_KNOB_INSET_PX) + 'px;width:' + VIS_KNOB_PX + 'px;height:' + VIS_KNOB_PX + 'px;background:#fff;border-radius:50%;transition:left 0.2s"></span>' +
              '</label>';
          }
          /* Branch 2 — LOCAL hive: the hub cannot write its config, so this is
             a signpost to where the setting actually lives (Governor → Hub),
             not a control. Falls back to a plain badge with no dashboard URL. */
          if (isLocal) {
            var dh = h.dashboardUrl && !h.dashboardUrl.includes('localhost') ? h.dashboardUrl : '';
            var badge = pub ? '<span style="color:var(--green)">Public</span>' : '<span style="color:var(--muted)">Private</span>';
            return dh
              ? '<a href="' + escAttr(dh) + '#config/governor/Hub" target="_blank" title="Change in Governor Config → Hub tab" style="text-decoration:none;cursor:pointer">' + badge + ' <span style="font-size:0.6rem;color:var(--muted)">↗</span></a>'
              : badge;
          }
          /* Branch 3 — everyone else: read-only state, no affordance implied. */
          return pub ? '<span style="color:var(--green)">✓</span>' : '<span style="color:var(--muted)">—</span>';
        })();
        var locationBadge = isLocal ? '<span style="display:inline-block;padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:rgba(107,114,128,0.15);color:#9ca3af;border:1px solid rgba(107,114,128,0.3)">local</span>' : clusterBadge(h.clusterId, h.clusterName);
        /* Location on top, visibility beneath, GitHub host last — three stacked
           lines, matching the three-line Version treatment. The owner toggle is
           a 36x20 switch and keeps its own box on its own line, so it stays an
           easy tap target.

           The host line reuses githubHostPill — the same helper the pending
           action cards and the Past Requests table use — so a GHE hive reads
           identically wherever it appears. An absent host renders as
           "github.com" rather than a blank chip, which is what an old
           heartbeat-only spoke that cannot yet report its host should show. */
        var locationCell = '<div style="' + STACKED_CELL_STYLE + '">' +
          '<div style="' + STACKED_LINE_STYLE + '">' + locationBadge + '</div>' +
          '<div style="' + STACKED_LINE_STYLE + ';font-size:0.7rem">' + visibilityCell + '</div>' +
          '</div>';
        var pendingExpandRow = '';
        if (h.pendingRequestCount > 0 && (h.role === 'owner' || h.role === 'read-write') && (h.pending_requests || []).length > 0) {
          var prItems = (h.pending_requests || []).map(function(pr) {
            var avatar = linkedAvatar(pr.username, LIST_AVATAR_PX, pr.username, 'margin-right:6px');
            var note = (pr.note || '').trim();
            var noteHtml = note
              ? '<div style="margin-top:4px;font-size:0.75rem;color:var(--text);white-space:pre-wrap;word-break:break-word;background:rgba(0,0,0,0.15);border-left:2px solid var(--accent);padding:4px 8px;border-radius:2px">' + esc(note) + '</div>'
              : '<div style="margin-top:4px;font-size:0.72rem;color:var(--muted);font-style:italic">(no note)</div>';
            return '<div style="padding:6px 0;border-bottom:1px solid var(--border)">' +
              '<div style="display:flex;align-items:center;justify-content:space-between">' +
              '<div>' + avatar + '<span style="font-size:0.85rem">' + esc(pr.username) + '</span></div>' +
              '<div style="display:flex;gap:4px">' +
              '<button onclick="inlineApproveAccess(\'' + esc(h.id) + '\',\'' + esc(pr.username) + '\',this)" style="padding:2px 8px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Approve</button>' +
              '<button onclick="inlineDenyAccess(\'' + esc(h.id) + '\',\'' + esc(pr.username) + '\',this)" style="padding:2px 8px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Deny</button>' +
              '</div></div>' + noteHtml + '</div>';
          }).join('');
          pendingExpandRow = '<tr id="pending-row-' + esc(h.id) + '" style="display:none"><td colspan="' + TOTAL_COLUMNS + '"><div style="padding:8px 16px;background:rgba(59,130,246,0.05);border-radius:6px;margin:4px 0">' + prItems + '</div></td></tr>';
        }
        /* Stable per-hive anchor so the "Attention needed" panel can scroll a
           specific row into view and highlight it. Built from the hive id, which
           is unique across both the assigned and unassigned sections. */
        return '<tr id="' + escAttr(hiveRowDomId(h.id)) + '">' +
          bulkCheckboxCell(h, section || 'all') +
          '<td class="hive-menu-cell" style="position:relative;width:30px;text-align:center;overflow:visible">' + (h.migrationStatus === 'migrating' ? '<span style="font-size:1.1rem;color:var(--border);user-select:none;cursor:not-allowed" title="Disabled during migration">⋮</span>' : '<span style="cursor:pointer;font-size:1.1rem;color:var(--muted);user-select:none">⋮</span>' + pendingBadge + '<div class="hive-menu-dropdown" style="display:none;position:absolute;left:0;bottom:auto;background:#1c2128;border:1px solid #30363d;border-radius:8px;min-width:160px;padding:4px 0;z-index:1000;box-shadow:0 8px 24px rgba(0,0,0,0.5)">' + menuItems.join('') + '</div>') + '</td>' +
          '<td style="text-align:left;line-height:1.4">' + (function() { var isHostedRow = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-'))); var dh = isHostedRow && h.id ? ('/api/saas/hives/' + encodeURIComponent(h.id) + '/open') : (rb ? esc(rb) : ''); /* Label derived from the PROJECT (org + primary repo), not by splitting h.name — see hiveLabel. */ var label = hiveLabel(h); var orgName = label.line1; var repoName = label.line2; var rp = h.org && h.primaryRepo ? h.org + '/' + h.primaryRepo : ''; var rpHref = ghRepoURL(h.github_host, h.org, h.primaryRepo); var ghIcon = (rp && rpHref) ? '<a href="' + rpHref + '" target="_blank" style="opacity:0.5;vertical-align:middle" title="' + esc(rp) + '"><svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor" style="vertical-align:middle"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg></a>' : (rp ? '<span style="opacity:0.5;vertical-align:middle" title="' + esc(rp) + '"><svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor" style="vertical-align:middle"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg></span>' : ''); /* vanityDisplay is the friendly host shown in the status bar on hover. rb is resolvedBase(h), which the hub has already overlaid with the claimed vanity_url; it is only a DISPLAY url here, never the click target — see ssoDisplayLink. Empty for a row with no vanity host, which falls back to today's /open href. Only meaningful on hosted rows: a non-hosted row's dh IS rb already. */ var vanityDisplay = isHostedRow && rb ? rb : ''; /* vanityDisplay is the friendly host shown in the status bar on hover. rb is resolvedBase(h), which the hub has already overlaid with the claimed vanity_url; it is only a DISPLAY url here, never the click target — see ssoDisplayLink. Empty for a row with no vanity host, which falls back to today's /open href. Only meaningful on hosted rows: a non-hosted row's dh IS rb already. */ var vanityDisplay = isHostedRow && rb ? rb : ''; var link = function(text, bold) { if (dh) { return ssoDisplayLink(dh, vanityDisplay, text, bold ? 'hive-name-link' : 'hive-sub-link'); } var s = bold ? 'font-weight:700;color:inherit' : 'color:#6b7280;font-weight:400'; return '<span style="' + s + '">' + esc(text) + '</span>'; }; var line1 = dot + ' ' + link(orgName, true); var fcPill = h.online ? failingCheckSummary(h) : ''; /* Advisory-staleness pill sits right beside the failing-check pill: both are "something is quietly wrong with this working hive" signals, and advisoryStaleSummary already self-suppresses (empty string) unless the hub flagged the digest stale, so unaffected rows are pixel-identical. */ var advPill = h.online ? advisoryStaleSummary(h) : ''; /* Dead-link pill is deliberately NOT gated on h.online: the entire point is a hive that IS online and heartbeating while its public URL is broken, so gating it the way the other pills are gated would hide exactly the case it exists to surface. */ var dlPill = deadLinkSummary(h); /* Inline access faces sit on the name cell's second line, immediately after this row's own role badge: the badge already answers "what am I on this hive", so the co-members read as the natural continuation of the same thought, in the one cell that is left-aligned and has room to grow. It also keeps them out of the 16 dense metric columns, none of which is about people. Empty string when the viewer is the only member, so those rows are pixel-identical to today. */ var accessFaces = hiveAccessAvatars(h); /* Keyed off repoName, not orgName: line 1 now always carries SOME identity, so the presence of a second line is decided purely by whether there is a repo to put on it. Without a repo the row still shows the GitHub icon, role badge, faces and failing-check pill on the compact variant. */ var line2 = repoName ? '<div style="padding-left:18px;font-size:0.8rem">' + link(repoName, false) + ' ' + ghIcon + ' ' + roleBadge(h.role) + accessFaces + fcPill + advPill + dlPill + '</div>' : '<div style="padding-left:18px">' + ghIcon + ' ' + roleBadge(h.role) + accessFaces + fcPill + advPill + dlPill + '</div>'; var line3 = pendingPill ? '<div style="margin-top:4px;padding-left:18px">' + pendingPill + '</div>' : ''; return line1 + line2 + line3; })() + '</td>' +
          '<td>' + locationCell + '</td>' +
          '<td style="white-space:nowrap">' + uptimeCell(h) + '</td>' +
          /* No white-space:nowrap on the cell itself: the stacked lines each
             carry their own nowrap, so the SHA still never breaks mid-hash,
             but the COLUMN is no longer forced to the width of every part laid
             end to end. That nowrap was the main reason this column was wide. */
          '<td style="font-size:0.7rem">' + versionCell + '</td>' +
          /* Repo count over the GitHub-instance pill: the host qualifies WHICH
             GitHub these repos live on, so the two belong together. Stacked
             rather than inline to keep the numeric column narrow. */
          '<td title="' + esc((h.repos || []).join('\n')) + '" style="cursor:' + (repoCount > 0 ? 'help' : 'default') + '">' +
            '<div style="' + STACKED_CELL_STYLE + '">' +
              '<div style="' + STACKED_LINE_STYLE + '">' + repoCount + '</div>' +
              '<div style="' + STACKED_LINE_STYLE + '" title="GitHub instance these repos live on">' + githubHostPill(h.githubHost) + '</div>' +
            '</div>' +
          '</td>' +
          '<td>' + acmmBadge(h.acmmLevel) + '</td>' +
          /* AI Author: who this hive authors PRs as. aiAuthorEffective is the
             App bot ("<slug>[bot]") in App-authored mode, else the configured
             ai_author (falls back to aiAuthor for spokes too old to report the
             effective value). A "[bot]" value is the informative case. */
          /* aiAuthorEffective already folds in ai_author (it returns ai_author
             when set, else the App bot, else empty). Do NOT fall back to a raw
             ai_author here: a hive with no usable GitHub App has no author and
             must render "—", not a stale personal ai_author it can't act as. */
          '<td style="white-space:nowrap;font-size:0.75rem" title="GitHub identity for this hive\'s PRs/commits (— = no GitHub App installed yet)">' + esc(h.aiAuthorEffective || '—') + '</td>' +
          '<td>' + journeyBadge(h.journey) + '</td>' +
          /* Drift sits immediately right of Journey: both answer "how healthy
             is this hive's configuration", so they read as one pair rather
             than being separated by nine metric columns. Header index and body
             index are both 10 of 16 — keep them in lockstep. */
          '<td style="white-space:nowrap;text-align:right">' + driftBadge(h) + '</td>' +
          '<td title="' + esc((h.agents || []).map(function(a){ var label = a.name + ' (' + a.state + ')'; if (a.mode === 'on_demand') label += ' — on demand'; return label; }).join('\n')) + '" style="cursor:' + ((h.agentCount || 0) > 0 ? 'help' : 'default') + '">' + (h.agentCount || 0) + '</td>' +
          '<td title="Cumulative tokens consumed, as of the last heartbeat" style="white-space:nowrap;cursor:help">' + fmtTokens(h.totalTokens24h || 0) + '</td>' +
          '<td>' + modeCell + '</td>' +
          '<td>' + sparkline(h.issueHistory, '#f59e0b', 50, 14) + (h.actionableIssues || 0) + '</td>' +
          '<td>' + sparkline(h.prHistory, '#3b82f6', 50, 14) + (h.actionablePRs || 0) + '</td>' +
          '<td>' + (h.activeContributors || 0) + '</td>' +
          /* Provisioned. hiveProvisionTime is the single source of truth for
             "is this timestamp usable" — reusing it here keeps the cell and the
             comparator from ever disagreeing (a row showing a date but sorting
             as unknown, or the reverse). An em dash, never 'Invalid Date', for
             a hive whose registeredAt is missing or unparseable. */
          '<td style="font-size:0.75rem;color:var(--muted);white-space:nowrap">' +
            (hiveProvisionTime(h) === null ? '—' : esc(fmtUserTS(h.registeredAt))) + '</td>' +
          '</tr>' + pendingExpandRow;
      };
      /* Section-header row: a labeled separator spanning all columns, styled to
         match the table's muted uppercase heading treatment (see .hive-table th). */
      /* Count of <th> cells in the hive table header below. The section-header
         row spans all of them; a stale value would leave the separator short
         and the table visibly ragged. 15 with the Drift column, plus the
         bulk-select column, plus the Journey column, plus the Provisioned
         column, minus Public (visibility is stacked under Location). Must stay
         equal to TOTAL_COLUMNS. */
      var TOTAL_COLUMNS_HEADER = 18;
      /* The header is a click target that expands/collapses its section. The
         caret mirrors aria-expanded so the affordance and the a11y state can
         never disagree. sectionKey also scopes the select-all checkbox to THIS
         section's rows only (Assigned and Unassigned select independently);
         the checkbox stops propagation so selecting does not also collapse.
         selectable=false suppresses the box for sections whose rows are never
         bulk-eligible (unassigned placeholders), where it would be a control
         that visibly does nothing. */
      var sectionHeader = function(label, count, sectionKey, selectable) {
        var collapsed = !!_dashSectionCollapsed[sectionKey];
        return '<tr class="hive-section-head" role="button" tabindex="0" aria-expanded="' + (collapsed ? 'false' : 'true') + '" ' +
          'onclick="toggleHiveSection(\'' + esc(sectionKey) + '\')" ' +
          'onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();toggleHiveSection(\'' + esc(sectionKey) + '\')}" ' +
          'title="Click to ' + (collapsed ? 'expand' : 'collapse') + '">' +
          '<td colspan="' + TOTAL_COLUMNS_HEADER + '" ' +
          'style="padding:14px 12px 6px;color:var(--muted);font-weight:600;font-size:0.75rem;' +
          'text-transform:uppercase;letter-spacing:0.5px;text-align:left">' +
          '<span class="hive-section-caret" aria-hidden="true">' + (collapsed ? '▸' : '▾') + '</span>' +
          (sectionKey && selectable !== false ? bulkSectionCheckbox(sectionKey) : '') +
          esc(label) + ' (' + count + ')</td></tr>';
      };
      /* Group-header row: nested one indent step inside a section header, with
         a caret showing collapse state and a count. Clicking toggles. Labels are
         user-controlled (org, owner, cluster and branch names all come from
         spoke heartbeats) so they are escaped here — and again in the onclick
         argument, where the id is additionally quote-escaped.

         This deliberately mirrors sectionHeader above — same caret glyphs, same
         role/tabindex/aria-expanded, same keyboard activation — because a group
         header and a section header are the same affordance at two nesting
         levels. Only the persistence key differs: sections store one flag each
         under hive-section-*-collapsed, groups store a set under
         hive-group-collapsed (their ids are dynamic and unbounded). */
      var GROUP_HEADER_INDENT_PX = 26;
      var groupHeader = function(label, count, collapsed, groupId) {
        var toggle = 'toggleDashGroup(' + jsArg(groupId) + ')';
        return '<tr class="hive-group-head" role="button" tabindex="0" aria-expanded="' + (collapsed ? 'false' : 'true') + '" ' +
          'onclick="' + toggle + '" ' +
          'onkeydown="if(event.key===\'Enter\'||event.key===\' \'){event.preventDefault();' + toggle + '}" ' +
          'title="Click to ' + (collapsed ? 'expand' : 'collapse') + '">' +
          '<td colspan="' + TOTAL_COLUMNS_HEADER + '" ' +
          'style="padding:8px 12px 6px ' + GROUP_HEADER_INDENT_PX + 'px;color:var(--muted);font-weight:600;' +
          'font-size:0.72rem;letter-spacing:0.3px;text-align:left">' +
          '<span class="hive-group-caret" aria-hidden="true">' + (collapsed ? '▸' : '▾') + '</span>' +
          esc(label) + ' <span style="opacity:0.7">(' + count + ')</span></td></tr>';
      };

      /* renderSection emits one Assigned/Unassigned section: its header, then
         either a flat run of rows or the grouped subsections. It takes the
         running index by reference through the counter object so the caller's
         GLOBAL index keeps advancing across sections AND groups — menu ids
         (hive-menu-<i>) must stay unique across the whole table or the ⋮
         dropdowns open the wrong row's menu.

         Critically, the index advances for rows inside COLLAPSED groups too:
         the index is a per-hive identity, not a per-visible-row position, so
         expanding a group must not renumber anything below it. */
      var renderSection = function(label, sectionKey, list, counter, selectable) {
        if (!(list || []).length) return '';
        var out = sectionHeader(label, list.length, sectionKey, selectable);
        /* The section is the OUTER collapse: when it is closed nothing inside
           it renders — not its rows and not its group headers. The counter
           still advances over every hive so menu ids stay stable. */
        var sectionOpen = !_dashSectionCollapsed[sectionKey];
        if (_dashGroupBy === HIVE_GROUP_NONE) {
          for (var fi = 0; fi < list.length; fi++) {
            var flatRow = buildRow(list[fi], counter.i++, sectionKey);
            if (sectionOpen) out += flatRow;
          }
          return out;
        }
        /* groupHives only ever emits non-empty buckets, so no empty group
           header can render. */
        var groups = groupHives(list, sectionKey);
        for (var gi = 0; gi < groups.length; gi++) {
          var g = groups[gi];
          var isCollapsed = !!_dashGroupCollapsed[g.id];
          if (sectionOpen) out += groupHeader(g.label, g.hives.length, isCollapsed, g.id);
          for (var ri = 0; ri < g.hives.length; ri++) {
            var rowHTML = buildRow(g.hives[ri], counter.i++, sectionKey);
            if (sectionOpen && !isCollapsed) out += rowHTML;
          }
        }
        return out;
      };

      var rows;
      if (_isAdmin) {
        /* Admin-only organizational aid: split into assigned (real, claimed)
           hives and unassigned placeholders — see isPlaceholderHive. Preserve
           incoming order so each section stays sorted.

           This split is the OUTER structure — grouping subdivides each side,
           it never replaces the split. */
        var assigned = [], unassigned = [];
        for (var _hi = 0; _hi < hives.length; _hi++) {
          var _h = hives[_hi];
          if (isPlaceholderHive(_h)) unassigned.push(_h); else assigned.push(_h);
        }
        /* Global running index across BOTH sections so menu ids (hive-menu-<i>)
           never collide between sections and the ⋮ dropdowns keep working.
           It advances over EVERY row, including rows inside a collapsed section
           or a collapsed group, so collapsing anything never renumbers the
           menus below it. renderSection carries the counter by reference. */
        var _counter = {i: 0};
        rows = renderSection('Assigned hives', HIVE_SECTION_ASSIGNED, assigned, _counter) +
          /* Unassigned placeholders are never bulk-eligible (nothing is running
             yet), so the section collapses but gets no select-all box. */
          renderSection('Unassigned hives', HIVE_SECTION_UNASSIGNED, unassigned, _counter, false);
      } else if (_dashGroupBy !== HIVE_GROUP_NONE) {
        /* Non-admin with grouping on: one flat set of groups, no section
           headers (a non-admin sees no placeholders to split off). The same
           global counter rule applies. */
        var _flatCounter = {i: 0};
        var _flatGroups = groupHives(hives, 'all');
        rows = '';
        for (var _fgi = 0; _fgi < _flatGroups.length; _fgi++) {
          var _fg = _flatGroups[_fgi];
          var _fgCollapsed = !!_dashGroupCollapsed[_fg.id];
          rows += groupHeader(_fg.label, _fg.hives.length, _fgCollapsed, _fg.id);
          for (var _fri = 0; _fri < _fg.hives.length; _fri++) {
            var _fgRow = buildRow(_fg.hives[_fri], _flatCounter.i++, 'all');
            if (!_fgCollapsed) rows += _fgRow;
          }
        }
      } else {
        /* Non-admin, no grouping: single flat list, exactly as before. */
        rows = hives.map(function(h, i) { return buildRow(h, i, 'all'); }).join('');
      }
      document.getElementById('hives-container').innerHTML =
        '<div class="table-wrap"><table class="hive-table"><thead><tr>' +
        /* Non-admin lists have no section headers, so the flat list's
           select-all lives in the table head instead. */
        '<th style="width:26px;text-align:center">' + (_isAdmin ? '' : bulkSectionCheckbox('all')) + '</th>' +
        '<th></th><th onclick="sortDashHives(\'name\')" style="cursor:pointer">Hive ⇅</th><th onclick="sortDashHives(\'clusterId\')" style="cursor:pointer" title="Where this hive runs, and whether it is listed publicly">Location / Public ⇅</th><th onclick="sortDashHives(\'startedAt\')" style="cursor:pointer" title="Process uptime since the last restart — a short value that keeps resetting means the pod is restarting">Uptime ⇅</th><th>Version</th><th>Repos</th><th onclick="sortDashHives(\'acmmLevel\')" style="cursor:pointer">ACMM ⇅</th><th onclick="sortDashHives(\'aiAuthor\')" style="cursor:pointer" title="The GitHub identity this hive\'s agents open PRs as — the configured ai_author, or the App bot (&lt;slug&gt;[bot]) in App-authored mode">AI Author ⇅</th><th onclick="sortDashHives(\'journey\')" style="cursor:pointer" title="Where this hive is on the adoption journey: install the GitHub App, assign a method/model (or run ClankeR, the contributor relay), then raise the ACMM level">Journey ⇅</th><th title="Configuration drift from the fleet norm — hover a value for the specific signals">Drift</th><th onclick="sortDashHives(\'agentCount\')" style="cursor:pointer">Agents ⇅</th><th onclick="sortDashHives(\'totalTokens24h\')" style="cursor:pointer" title="Cumulative tokens consumed, as of the last heartbeat">Tokens ⇅</th><th onclick="sortDashHives(\'governorMode\')" style="cursor:pointer">Mode ⇅</th><th onclick="sortDashHives(\'actionableIssues\')" style="cursor:pointer">Issues ⇅</th><th onclick="sortDashHives(\'actionablePRs\')" style="cursor:pointer">PRs ⇅</th><th onclick="sortDashHives(\'activeContributors\')" style="cursor:pointer">Contributors ⇅</th><th onclick="sortDashHives(\'registeredAt\')" style="cursor:pointer" title="When this hive was first provisioned — the hub&apos;s first-seen time, preserved across restarts and heartbeats">Provisioned ⇅</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table></div>';
      /* Delegated, so binding once is enough no matter how often the table is
         re-rendered. The guard keeps repeated renders from stacking listeners. */
      if (!_autoUpgradeSelectsBound) { bindAutoUpgradeSelects(); _autoUpgradeSelectsBound = true; }
      /* Re-derive the bar and the select-all tri-state from _bulkSelected now
         that the fresh checkboxes exist in the DOM. */
      renderBulkBar();
      syncBulkSectionHeaderBoxes();
      setTimeout(function() {
        var tw = document.querySelector('.table-wrap');
        if (tw && tw.scrollWidth > tw.clientWidth) tw.classList.add('has-scroll');
      }, 0);
    }

    async function toggleVisibility(id, isPublic) {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/visibility', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({is_public: isPublic})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to change visibility', 'error'); loadHives(); return; }
        hiveToast(id + ' is now ' + (isPublic ? 'public' : 'private'), 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadHives(); }
    }

    /* Auto-upgrade select values. AUTO_UPGRADE_OFF is a UI-only sentinel: on
       the wire "off" is auto_upgrade:false, and the two real modes match the
       server's allowlist ("instant" / "daily"). */
    var _autoUpgradeSelectsBound = false;
    var AUTO_UPGRADE_OFF = 'off';
    var AUTO_UPGRADE_INSTANT = 'instant';
    var AUTO_UPGRADE_DAILY = 'daily';
    /* Copy is framed around DISRUPTION, not cron mechanics: the operator is
       choosing when it is acceptable to interrupt a hive that is working. */
    var AUTO_UPGRADE_TITLE = 'When to apply new versions. Instant restarts the hive as soon as a new version lands; Daily restarts it at most once a day, after hours, so a stable hive is not disturbed mid-work.';
    /* Icon-only labels. The "Auto:" prefix repeated on every row was the widest
       single element in the Version column and said nothing a scanning operator
       did not already know from the column it sits in — so it is carried by the
       title/aria-label instead of being spelled out on every line.

       The three glyphs are chosen to be DISTINCT SHAPES, not intensities of one
       shape: 'off' must not read as a greyed-out 'instant'.
         ⦸  off     — a struck-through circle: the universal "disabled" mark,
                      and the only glyph here with a diagonal stroke.
         ⚡ instant  — a bolt: applies the moment a version lands.
         🕔 5p      — a clock face, plus the literal window "5p", because the
                      hour is the whole point of the mode and no icon conveys it.
       All three are plain text glyphs rather than SVG or background images, so
       they inherit the select's currentColor. The hub renders on one dark
       surface today, but nothing here hard-codes a fill or stroke colour, so a
       light surface would not make any of the three vanish. The label text
       after the glyph is the shortest string that still disambiguates — and it
       is there precisely so the control never degrades to "a picture only":
       ⚡ and 🕔 are emoji whose rendering varies by platform, so the word
       carries the meaning if the glyph renders as a box.

       ariaLabel carries the FULL meaning for screen readers and for the
       per-option tooltip, so nothing is lost by dropping the inline words. */
    var AUTO_UPGRADE_OPTIONS = [
      {value: AUTO_UPGRADE_OFF, label: '⦸ off', ariaLabel: 'Auto-upgrade: off'},
      {value: AUTO_UPGRADE_INSTANT, label: '⚡ instant', ariaLabel: 'Auto-upgrade: instantly when a new version lands'},
      {value: AUTO_UPGRADE_DAILY, label: '🕐 1p', ariaLabel: 'Auto-upgrade: daily at 1pm ET'}
    ];
    /* Accessible name for the select itself, resolved from the CURRENT mode so
       the control announces what it is set to rather than only what it does. */
    function autoUpgradeAriaLabel(mode) {
      for (var ai = 0; ai < AUTO_UPGRADE_OPTIONS.length; ai++) {
        if (AUTO_UPGRADE_OPTIONS[ai].value === mode) return AUTO_UPGRADE_OPTIONS[ai].ariaLabel;
      }
      return 'Auto-upgrade';
    }

    /* One delegated listener on the container, bound once at startup, so every
       re-render of the table keeps working without rebinding per row. */
    function bindAutoUpgradeSelects() {
      var container = document.getElementById('hives-container');
      if (!container) return;
      container.addEventListener('change', function(e) {
        var el = e.target;
        if (!el || !el.classList || !el.classList.contains('auto-upgrade-select')) return;
        var id = el.getAttribute('data-hive-id');
        if (!id) return;
        setAutoUpgradeMode(id, el.value);
      });
    }

    /* Maps the select's value onto the existing endpoint's two fields. "off"
       sends auto_upgrade:false; the mode is still sent so the server records a
       concrete preference rather than leaving a blank to be re-interpreted. */
    async function setAutoUpgradeMode(id, value) {
      var enabled = value !== AUTO_UPGRADE_OFF;
      var mode = (value === AUTO_UPGRADE_DAILY) ? AUTO_UPGRADE_DAILY : AUTO_UPGRADE_INSTANT;
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/auto-upgrade', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({auto_upgrade: enabled, auto_upgrade_mode: mode})
        });
        if (!resp.ok) { hiveToast('Failed to update auto-upgrade', 'error'); loadHives(); return; }
        var label = !enabled ? 'off'
          : (mode === AUTO_UPGRADE_DAILY ? 'daily at 1pm ET' : 'instant');
        hiveToast(id + ' auto-upgrade: ' + label, 'success');
        loadHives();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
        loadHives();
      }
    }

    async function toggleAutoUpgrade(id, enabled) {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/auto-upgrade', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({auto_upgrade: enabled})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed', 'error'); loadHives(); return; }
        hiveToast(id + ' auto-upgrade ' + (enabled ? 'enabled' : 'disabled'), 'success');
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadHives(); }
    }

    var _hubUpgrading = false;
    var _hubAutoUpgrade = false;
    async function toggleHubAutoUpgrade(enabled) {
      try {
        var resp = await fetch('/api/saas/hub/auto-upgrade', {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({auto_upgrade: enabled})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed', 'error'); return; }
        _hubAutoUpgrade = enabled;
        hiveToast('Hub auto-upgrade ' + (enabled ? 'enabled' : 'disabled'), 'success');
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }
    async function upgradeHub(currentSHA) {
      var toSHA = _latestSHA ? _latestSHA.substring(0, 7) : 'latest';
      var fromSHA = currentSHA ? currentSHA.substring(0, 7) : '?';
      if (!await hiveConfirm('Upgrade Hive Hub?<br><br><span style="font-family:monospace;font-size:0.85rem;color:var(--muted)">' + fromSHA + '</span> → <span style="font-family:monospace;font-size:0.85rem;color:var(--green)">' + toSHA + '</span>', true)) return;
      var btn = document.getElementById('hub-upgrade-btn');
      if (btn) { btn.disabled = true; btn.innerHTML = '<span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:3px"></span>Upgrading'; btn.style.opacity = '0.6'; }
      try {
        var resp = await fetch('/api/saas/hub/upgrade', {method: 'POST'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Hub upgrade failed', 'error'); return; }
        _hubUpgrading = true;
        hiveToast('Hub upgrade started — page will refresh when ready', 'success');
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    var _upgradingHives = {};
    var _switchStartedAt = {}; // hiveId → ms timestamp the switch was initiated
    var SWITCH_STALE_MS = 8 * 60 * 1000; // warn if a switch hasn't landed in 8 min

    /* ── Upgrade elapsed-time counter ──
       A hive that has been "Upgrading" for a long time is a FAULT SIGNAL, not
       progress: the incident this exists to prevent had 15 hives pinned at
       upgrading:true for 20+ minutes while their spokes exited 0 in a restart
       loop, and a static spinner made a permanently-wedged fleet look
       identical to a merely slow one.

       The thresholds below escalate the counter's colour so that state is
       obvious at a glance instead of requiring the operator to notice a
       number. UPGRADE_ELAPSED_RED_MS deliberately equals staleUpgradeTimeout
       in server.go (10m) — the same instant the hub raises its stuck-upgrade
       alert. Turning red EARLIER would accuse a hive the hub still considers
       healthy; turning red LATER would leave a window where the alert panel
       says stuck and the row still looks fine. The amber step sits at half of
       that, an early warning while the upgrade is still plausibly in flight. */
    var UPGRADE_ELAPSED_RED_MS = 10 * 60 * 1000;  // == staleUpgradeTimeout (server.go): hub calls this stuck
    var UPGRADE_ELAPSED_AMBER_MS = 5 * 60 * 1000; // half the stuck limit: early warning, still plausible

    /* Below this the counter is suppressed entirely. Every upgrade begins at
       0s, and a counter that flickers "1s, 2s…" on every healthy rollout would
       train the operator to ignore it — which is exactly the habit that let the
       incident go unnoticed. It only appears once an upgrade is slow enough to
       be worth a human's attention. */
    var UPGRADE_ELAPSED_MIN_SHOW_MS = 30 * 1000; // don't nag during a normal fast rollout

    /* Clocks are not synchronised. The browser's Date.now() and the hub's
       upgradeStartedAt can disagree by seconds, which yields a small NEGATIVE
       elapsed on a freshly-started upgrade. Tolerate that as "just started"
       rather than rendering a negative duration; anything more negative than
       this is real clock skew and the counter is suppressed instead of shown
       as a lie. */
    var UPGRADE_ELAPSED_SKEW_TOLERANCE_MS = 60 * 1000; // treat small negatives as 0

    /* upgradeElapsedMs returns how long a hive has been upgrading, in ms, or
       null when that cannot be known — a MISSING, EMPTY or UNPARSEABLE
       upgradeStartedAt, or a negative elapsed beyond the skew tolerance.
       Callers must render nothing on null; they must never render NaN, a
       negative duration, or a fabricated "0s" that implies the upgrade just
       started when in truth the timestamp was lost.

       Note a real server behaviour this relies on: upgradeStartedAt is stamped
       ONLY where Upgrading is set true, so a hive that is merely QUEUED has no
       timestamp and correctly gets null here rather than a bogus counter. */
    function upgradeElapsedMs(h, nowMs) {
      if (!h) return null;
      var started = h.upgradeStartedAt;
      if (!started || typeof started !== 'string') return null;
      var t = Date.parse(started);
      if (isNaN(t)) return null; /* unparseable timestamp — show nothing, never NaN */
      var elapsed = (typeof nowMs === 'number' ? nowMs : Date.now()) - t;
      if (elapsed < 0) {
        /* Clock skew. A small negative is the browser being marginally behind
           the hub on an upgrade that genuinely just started; clamp it to zero.
           A large negative means the timestamp is not trustworthy at all. */
        if (elapsed > -UPGRADE_ELAPSED_SKEW_TOLERANCE_MS) return 0;
        return null;
      }
      return elapsed;
    }

    /* formatUpgradeElapsed renders a compact duration: "45s", "3m", "1h12m".
       Minutes are the operator's working unit here, so past an hour it keeps
       the minutes rather than collapsing to a bare "1h" that would hide a
       72-minute wedge behind the same string as a 61-minute one. */
    function formatUpgradeElapsed(ms) {
      if (typeof ms !== 'number' || isNaN(ms) || ms < 0) return '';
      var totalSec = Math.floor(ms / 1000);
      if (totalSec < 60) return totalSec + 's';
      var totalMin = Math.floor(totalSec / 60);
      if (totalMin < 60) return totalMin + 'm';
      var hours = Math.floor(totalMin / 60);
      return hours + 'h' + (totalMin % 60) + 'm';
    }

    /* upgradeElapsedColor escalates the counter past the thresholds above.
       Muted while the rollout is plausibly healthy, amber as an early warning,
       red once the hub itself would call the upgrade stuck. */
    function upgradeElapsedColor(ms) {
      if (typeof ms !== 'number' || isNaN(ms)) return 'var(--muted)';
      if (ms >= UPGRADE_ELAPSED_RED_MS) return 'var(--red)';
      if (ms >= UPGRADE_ELAPSED_AMBER_MS) return 'var(--amber, #f59e0b)';
      return 'var(--muted)';
    }

    /* renderUpgradeElapsed builds the counter fragment appended to the
       "Upgrading" pill. Returns '' whenever the elapsed time is unknown or
       still below the nag threshold, so the common healthy case renders
       exactly what it renders today. */
    function renderUpgradeElapsed(h, nowMs) {
      var ms = upgradeElapsedMs(h, nowMs);
      if (ms === null) return '';
      if (ms < UPGRADE_ELAPSED_MIN_SHOW_MS) return '';
      var label = formatUpgradeElapsed(ms);
      if (!label) return '';
      var stuck = ms >= UPGRADE_ELAPSED_RED_MS;
      /* Past the red line the wording stops describing progress and names the
         fault, because at that point the hub has raised its stuck-upgrade
         alert and "still upgrading" would be the same misleading reassurance
         the incident turned on. */
      var tip = stuck
        ? 'Upgrading for ' + label + ' — past the ' + Math.round(UPGRADE_ELAPSED_RED_MS / 60000) +
          ' minute limit. This upgrade is almost certainly wedged, not slow; check the hive for a failed self-upgrade.'
        : 'Upgrading for ' + label;
      return '<span title="' + escAttr(tip) + '" style="margin-left:4px;font-variant-numeric:tabular-nums;color:' +
        upgradeElapsedColor(ms) + (stuck ? ';font-weight:600' : '') + '">' + esc(label) + '</span>';
    }

    /* Version and Location cells stack their contents vertically so neither
       column is forced as wide as the sum of its parts. The Version cell used
       to lay branch pill + SHA + status + upgrade affordance + auto-upgrade
       select on ONE nowrap line, which made it the widest column in a table
       that already carries 16 of them.

       STACKED_LINE_HEIGHT is deliberately tighter than the table's default so
       the stack stays as short as four lines can be.

       ONE ELEMENT PER LINE. The three-line arrangement did not actually narrow
       the column, because its last line carried TWO controls side by side (the
       upgrade affordance and the auto-upgrade select). A column is as wide as
       its widest line, so that pair — not the short lines above it — set the
       width, and the stacking bought nothing. Splitting them onto their own
       lines is what makes the widest line a single element.

       THE HONEST NUMBERS — do not "simplify" these away and do not restate
       them from arithmetic alone. A previous change to this cell shipped a
       claim of height-neutrality that was false. These were MEASURED in
       Chromium on the rendered cell (getBoundingClientRect), for an owner row
       that is behind latest with auto-upgrade set to daily — the widest and
       tallest variant:
         column width   266.4px -> 110.4px   (-156.0px, -58.6%)
         cell height     66.8px ->  79.8px   (+13.0px)
         stack height    50.8px ->  63.8px   (+13.0px)
       The four lines measure 15.0 / 12.9 / 12.9 / 20.0 px plus 3 gaps of
       STACKED_ROW_GAP_PX. So ROWS DO GROW, by 13px on owner rows. That is the
       price of the width win and it is stated rather than hidden. Two things
       hold it down: the upgrade affordance is now text-with-an-underline rather
       than a padded button (8px of padding and border removed), and the select
       is pinned to AUTO_SELECT_H_PX rather than left to the UA's default
       control height, which is taller.

       What actually sets the width now is the LONGEST upgrade label,
       "queued · 1pm ET" at 90.4px — not the select, which measures 77px. The
       old width was set by that label and the "Auto: daily 1pm ET" select
       sitting on ONE line together.

       Non-owner rows are UNCHANGED: they still get at most two lines (28.9px),
       under the two-line name cell's 40.3px, which continues to set their
       height. Only owner rows pay.

       STACKED_ROW_GAP_PX separates the stacked lines without adding a full line
       box. 1px proved too tight in practice: the branch pill, SHA, upgrade link
       and auto-upgrade select read as one crowded block, and the Location cell's
       cluster badge sat flush against its visibility toggle. 4px is enough to
       group them as distinct rows while still costing far less than a line box. */
    var STACKED_LINE_HEIGHT = 1.15;
    var STACKED_ROW_GAP_PX = 4;
    /* Shared style for a stacked cell: a vertical flex column, centred to match
       the table's default text-align:center for non-first cells. */
    var STACKED_CELL_STYLE = 'display:flex;flex-direction:column;align-items:center;justify-content:center;gap:' + STACKED_ROW_GAP_PX + 'px;line-height:' + STACKED_LINE_HEIGHT;
    /* Horizontal gap between the items that share one stacked line (e.g. the
       SHA and its current/behind glyph). Small enough that a line stays visibly
       one group rather than reading as separate columns. */
    var STACKED_ITEM_GAP_PX = 4;
    /* One line inside a stacked cell. Items within a line stay on one row (the
       SHA must never wrap mid-hash) but the LINES themselves are what narrow
       the column. */
    var STACKED_LINE_STYLE = 'display:flex;align-items:center;justify-content:center;gap:' + STACKED_ITEM_GAP_PX + 'px;white-space:nowrap';

    /* Icon-only auto-upgrade select geometry. The control lost its "Auto:"
       prefix, so its box must be sized deliberately rather than left to shrink
       to a glyph: 20px tall and at least 56px wide keeps it a comfortable
       pointer and touch target (the row is 63.8px tall, so 20px is roughly a
       third of it — visibly a control, not a decoration) while still being far
       narrower than the "Auto: daily 1pm ET" string it replaces. Widening it
       would give back the column width this change exists to reclaim; making it
       smaller would make it fiddly to hit. */
    var AUTO_SELECT_H_PX = 20;
    var AUTO_SELECT_MIN_W_PX = 56;
    /* Horizontal padding inside the select. 4px, not the previous 3px, because
       an icon needs a little breathing room from the border to read as a glyph
       rather than as part of the frame. */
    var AUTO_SELECT_PAD_X_PX = 4;
    /* The upgrade affordance is text-with-an-underline, not a padded button:
       on its own line a button's 3px padding and 1px border added 8px of row
       height for no extra affordance, since the whole line is already the
       click target. The underline plus the pointer cursor is what says
       "clickable"; colour alone would not, and would fail in one theme. */
    var UPGRADE_LINK_STYLE = 'cursor:pointer;text-decoration:underline;text-underline-offset:2px;white-space:nowrap';

    /* Visibility toggle switch geometry, used by the Location cell's owner
       branch. A 36x20 track with a 16px knob inset 2px is the standard iOS-style
       switch proportion: the knob travels W - knob - 2*inset = 18px, which is
       why the "on" position is 18px from the left. Keeping these as constants
       means the track, the knob and the travel distance can never drift apart. */
    var VIS_SWITCH_W_PX = 36;
    var VIS_SWITCH_H_PX = 20;
    var VIS_KNOB_PX = 16;
    var VIS_KNOB_INSET_PX = 2;
    var VIS_SWITCH_RADIUS_PX = VIS_SWITCH_H_PX / 2; // fully rounded track
    var VIS_KNOB_ON_LEFT_PX = VIS_SWITCH_W_PX - VIS_KNOB_PX - VIS_KNOB_INSET_PX;

    /* Prefix marking a client-side branch-switch sentinel in _upgradingHives.
       The intended target branch follows the prefix (e.g. "switch:v3") so the
       label can tell a genuine branch switch apart from a same-branch upgrade
       purely from state — a bare sentinel could not. */
    var SWITCH_SENTINEL_PREFIX = 'switch:';
    /* Suffix the hub appends to a branch-switch upgrade target. */
    var BRANCH_TARGET_SUFFIX = '-latest';

    /* Single source of truth for switch-vs-upgrade, consumed by the spinner,
       the label and the title so they can never disagree. Resolves the target
       branch from the server's "<branch>-latest" upgradeTarget or, before the
       server reflects it, from the optimistic client sentinel ("switch:<branch>").
       A genuine branch switch means a target branch that is present AND differs
       from the branch the hive currently reports. A plain-SHA target — an
       auto-upgrade — yields no target branch and is therefore an upgrade, never
       a switch, regardless of any sticky sentinel. */
    function hiveUpgradeState(h, branchName) {
      var sentinel = _upgradingHives[h.id];
      var hasSwitchSentinel = typeof sentinel === 'string'
        && sentinel.indexOf(SWITCH_SENTINEL_PREFIX) === 0;
      var upgradeTarget = h.upgradeTarget || '';
      var isBranchTarget = upgradeTarget.length > BRANCH_TARGET_SUFFIX.length
        && upgradeTarget.slice(-BRANCH_TARGET_SUFFIX.length) === BRANCH_TARGET_SUFFIX;
      var targetBranch = '';
      if (isBranchTarget) {
        targetBranch = upgradeTarget.slice(0, -BRANCH_TARGET_SUFFIX.length);
      } else if (hasSwitchSentinel) {
        targetBranch = sentinel.slice(SWITCH_SENTINEL_PREFIX.length);
      }
      /* A same-branch target is not a switch — drop it so nothing downstream
         mistakes an auto-upgrade for one. */
      var isSwitching = !!(targetBranch && targetBranch !== branchName);
      if (!isSwitching) targetBranch = '';
      /* A switch sentinel that no longer resolves to a switch (target became a
         same-branch SHA / auto-upgrade) is stale and must not force upgrading. */
      var switchSentinelStale = hasSwitchSentinel && !isSwitching;
      return { isSwitching: isSwitching, targetBranch: targetBranch, switchSentinelStale: switchSentinelStale };
    }

    async function upgradeHive(id, currentSHA, branch) {
      var fromSHA = currentSHA ? currentSHA.substring(0, 7) : '?';
      var branchLatest = (branch && _latestSHAs[branch]) || _latestSHA;
      var toSHA = branchLatest ? branchLatest.substring(0, 7) : 'latest';
      if (!await hiveConfirm('Upgrade ' + id + '?<br><br><span style="font-family:monospace;font-size:0.85rem;color:var(--muted)">' + fromSHA + '</span> → <span style="font-family:monospace;font-size:0.85rem;color:var(--green)">' + toSHA + '</span>', true)) return;
      var btn = document.getElementById('upgrade-' + id);
      if (btn) { btn.disabled = true; btn.innerHTML = '<span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>Upgrading'; btn.style.opacity = '0.6'; }
      try {
        hiveToast('Upgrading ' + id + '...', 'info');
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id) + '/upgrade', {method: 'POST'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Upgrade failed', 'error'); delete _upgradingHives[id]; loadHives(); return; }
        _upgradingHives[id] = currentSHA;
        hiveToast('Upgrade started for ' + id + ' — waiting for rollout', 'success');
        loadHives();
        setTimeout(loadHives, 10000);
        setTimeout(loadHives, 30000);
        setTimeout(loadHives, 60000);
        setTimeout(loadHives, 90000);
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); delete _upgradingHives[id]; loadHives(); }
    }

    function toggleBranchMenu(hiveId) {
      var menu = document.getElementById('branch-menu-' + hiveId);
      if (!menu) return;
      var isOpen = menu.style.display !== 'none';
      document.querySelectorAll('[id^="branch-menu-"]').forEach(function(m) { m.style.display = 'none'; });
      if (!isOpen) {
        menu.style.display = 'block';
        var closeHandler = function(e) {
          if (!e.target.closest('#branch-pill-' + hiveId)) {
            menu.style.display = 'none';
            document.removeEventListener('click', closeHandler);
          }
        };
        setTimeout(function() { document.addEventListener('click', closeHandler); }, 0);
      }
    }

    var _switchTimers = {};
    function switchBranch(hiveId, newBranch, el) {
      if (el) el.closest('[id^="branch-menu-"]').style.display = 'none';
      if (_switchTimers[hiveId]) {
        clearInterval(_switchTimers[hiveId].interval);
        delete _switchTimers[hiveId];
      }
      var SWITCH_DELAY_SEC = 5;
      var remaining = SWITCH_DELAY_SEC;
      var pill = document.getElementById('branch-pill-' + hiveId);
      if (!pill) return;
      var origHTML = pill.innerHTML;
      pill.style.background = 'rgba(234,179,8,0.2)';
      pill.style.borderColor = 'rgba(234,179,8,0.5)';
      pill.style.color = '#eab308';
      pill.onclick = null;
      // Install the cancel handler after the selecting click finishes
      // bubbling — the menu lives inside the pill, so the same click
      // would otherwise cancel the switch it just started.
      setTimeout(function() {
        pill.onclick = function() { cancelBranchSwitch(hiveId, origHTML); };
      }, 0);
      pill.innerHTML = esc(newBranch) + ' in ' + remaining + 's ✕';
      pill.title = 'Click to cancel';
      var interval = setInterval(function() {
        remaining--;
        if (remaining <= 0) {
          clearInterval(interval);
          delete _switchTimers[hiveId];
          pill.innerHTML = '<span style="display:inline-block;width:10px;height:10px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:3px"></span>Switching to ' + esc(newBranch);
          pill.style.cursor = 'default';
          pill.onclick = null;
          // Set the switching sentinel BEFORE the request so any loadHives()
          // re-render during the POST round-trip keeps showing "Switching to
          // <branch>" — previously the sentinel was set only after the fetch
          // resolved, leaving a gap where the pill reverted to plain status.
          _upgradingHives[hiveId] = SWITCH_SENTINEL_PREFIX + newBranch;
          _switchStartedAt[hiveId] = Date.now();
          doSwitchBranch(hiveId, newBranch);
          return;
        }
        pill.innerHTML = esc(newBranch) + ' in ' + remaining + 's ✕';
      }, 1000);
      _switchTimers[hiveId] = { interval: interval, origHTML: origHTML };
    }

    function cancelBranchSwitch(hiveId, origHTML) {
      if (_switchTimers[hiveId]) {
        clearInterval(_switchTimers[hiveId].interval);
        delete _switchTimers[hiveId];
      }
      var pill = document.getElementById('branch-pill-' + hiveId);
      if (!pill) return;
      pill.style.background = 'rgba(59,130,246,0.15)';
      pill.style.borderColor = 'rgba(59,130,246,0.3)';
      pill.style.color = '#60a5fa';
      pill.innerHTML = origHTML;
      pill.onclick = function() { toggleBranchMenu(hiveId); };
      pill.title = 'Click to switch branch';
      hiveToast('Branch switch cancelled', 'info');
    }

    async function doSwitchBranch(hiveId, newBranch) {
      try {
        hiveToast('Switching ' + hiveId + ' to ' + newBranch + '...', 'info');
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/switch-branch', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ branch: newBranch })
        });
        var data = await resp.json();
        if (!resp.ok) {
          delete _upgradingHives[hiveId]; delete _switchStartedAt[hiveId]; // clear switching state on failure
          hiveToast(data.error || 'Branch switch failed', 'error');
          loadHives();
          return;
        }
        // Sentinel already set before the fetch; keep it until the hive
        // reports the new branch (cleared in the render path).
        hiveToast('Switching ' + hiveId + ' to ' + newBranch + ' — applies on next check-in', 'success');
        loadHives();
        setTimeout(loadHives, 10000);
        setTimeout(loadHives, 30000);
        setTimeout(loadHives, 60000);
      } catch(e) {
        delete _upgradingHives[hiveId]; delete _switchStartedAt[hiveId]; // clear switching state on error
        hiveToast('Error: ' + e.message, 'error');
        loadHives();
      }
    }

    function autoRequestAccessFromUrl() {
      var params = new URLSearchParams(window.location.search);
      var hiveId = params.get('request_hive');
      if (!hiveId) return;
      window.history.replaceState({}, '', '/dashboard');
      // A justification note is required, so open the request modal to
      // collect it instead of firing a note-less request.
      dashRequestAccess(hiveId, null);
    }

    function togglePendingRow(hiveId) {
      var row = document.getElementById('pending-row-' + hiveId);
      if (row) row.style.display = row.style.display === 'none' ? '' : 'none';
    }

    async function inlineApproveAccess(hiveId, username, btn) {
      btn.disabled = true;
      btn.textContent = '...';
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/approve-access/' + encodeURIComponent(username), {method: 'PUT'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Approve failed', 'error'); btn.disabled = false; btn.textContent = 'Approve'; return; }
        hiveToast(username + ' approved', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); btn.disabled = false; btn.textContent = 'Approve'; }
    }

    async function inlineDenyAccess(hiveId, username, btn) {
      btn.disabled = true;
      btn.textContent = '...';
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/deny-access/' + encodeURIComponent(username), {method: 'DELETE'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Deny failed', 'error'); btn.disabled = false; btn.textContent = 'Deny'; return; }
        hiveToast(username + ' denied', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); btn.disabled = false; btn.textContent = 'Deny'; }
    }

    function renderPendingBanner(hives) {
      var existing = document.getElementById('pending-banner');
      if (existing) existing.remove();
      var pending = (hives || []).filter(function(h) { return (h.role === 'owner' || h.role === 'read-write') && h.pendingRequestCount > 0; });
      if (!pending.length) return;
      var total = pending.reduce(function(sum, h) { return sum + h.pendingRequestCount; }, 0);
      var banner = document.createElement('div');
      banner.id = 'pending-banner';
      banner.style.cssText = 'background:rgba(59,130,246,0.12);border:1px solid rgba(59,130,246,0.3);border-radius:8px;padding:12px 16px;margin-bottom:16px;display:flex;align-items:center;gap:10px';
      banner.innerHTML = '<span style="font-size:1.1rem">📬</span><span style="font-size:0.85rem;color:var(--text)">' + total + ' pending access request' + (total > 1 ? 's' : '') + ' across ' + pending.length + ' hive' + (pending.length > 1 ? 's' : '') + '. Open <strong>Permissions</strong> on each hive to approve or deny.</span>';
      var container = document.getElementById('hives-container');
      container.parentNode.insertBefore(banner, container);
    }

    async function renderUserAccessBanner() {
      var existing = document.getElementById('user-access-banner');
      if (existing) existing.remove();
      try {
        var resp = await fetch('/api/saas/access-status');
        var data = await resp.json();
        var hives = data.hives || {};
        var pendingIds = [];
        var acceptedIds = [];
        for (var hid in hives) {
          var info = hives[hid];
          if (info.status === 'pending') pendingIds.push(hid);
          if (info.status === 'accepted' && info.role !== 'owner') acceptedIds.push(hid);
        }
        if (!pendingIds.length && !acceptedIds.length) return;
        var container = document.getElementById('hives-container');
        var banner = document.createElement('div');
        banner.id = 'user-access-banner';
        banner.style.cssText = 'margin-bottom:16px';
        var html = '';
        var dismissed = JSON.parse(localStorage.getItem('hive-dismissed-banners') || '{}');
        if (pendingIds.length) {
          var pKey = 'pending:' + pendingIds.sort().join(',');
          if (!dismissed[pKey]) {
            html += '<div style="background:rgba(245,158,11,0.12);border:1px solid rgba(245,158,11,0.3);border-radius:8px;padding:12px 16px;margin-bottom:8px;display:flex;align-items:center;gap:10px">' +
              '<span style="font-size:1.1rem">&#x1F514;</span><span style="flex:1;font-size:0.85rem;color:var(--text)">Access pending: <strong>' + pendingIds.map(esc).join(', ') + '</strong></span>' +
              '<button onclick="dismissBanner(\'' + pKey.replace(/'/g,'') + '\',this)" style="margin-left:auto;background:none;border:none;color:var(--muted);cursor:pointer;font-size:1rem;padding:0 4px" title="Dismiss">&times;</button></div>';
          }
        }
        if (acceptedIds.length) {
          var aKey = 'accepted:' + acceptedIds.sort().join(',');
          if (!dismissed[aKey]) {
            html += '<div style="background:rgba(34,197,94,0.12);border:1px solid rgba(34,197,94,0.3);border-radius:8px;padding:12px 16px;margin-bottom:8px;display:flex;align-items:center;gap:10px">' +
              '<span style="font-size:1.1rem">&#x2705;</span><span style="flex:1;font-size:0.85rem;color:var(--text)">Access granted: <strong>' + acceptedIds.map(esc).join(', ') + '</strong> — Start contributing!</span>' +
              '<button onclick="dismissBanner(\'' + aKey.replace(/'/g,'') + '\',this)" style="margin-left:auto;background:none;border:none;color:var(--muted);cursor:pointer;font-size:1rem;padding:0 4px" title="Dismiss">&times;</button></div>';
          }
        }
        banner.innerHTML = html;
        container.parentNode.insertBefore(banner, container);
      } catch(e) {}
    }

    function renderProvisionRequestBanner(req) {
      var el = document.getElementById('provision-request-banner');
      if (!el) return;
      if (!req) { el.style.display = 'none'; return; }
      el.style.display = '';
      var project = esc(req.org) + '/' + esc(req.primary_repo || req.repos);
      var status = req.status || 'pending';
      var icon, bg, border, msg;
      if (status === 'approved') {
        icon = '&#x2705;';
        bg = 'rgba(34,197,94,0.12)'; border = 'rgba(34,197,94,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> has been approved! An admin will assign your hive shortly.';
      } else if (status === 'denied') {
        icon = '&#x274C;';
        bg = 'rgba(239,68,68,0.12)'; border = 'rgba(239,68,68,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> was denied by an admin.';
      } else {
        icon = '&#x1F3D7;&#xFE0F;';
        bg = 'rgba(245,158,11,0.12)'; border = 'rgba(245,158,11,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> is pending admin approval.';
      }
      /* The banner is purely informational in EVERY state now that the
         Provision button is gone (hives are assigned by an admin), so all
         three states are dismissable. The earlier carve-out that kept
         'approved' non-dismissable existed only because that state carried the
         Provision button — the sole place the action was offered — and
         dismissals never expire (see dismissBanner: the stored timestamp is
         written but no reader ever compares it), so a stray click would have
         stranded the user. With no unique action left in any state, there is
         nothing to strand and the carve-out no longer applies.

         The key embeds BOTH the request identity and its status, so a
         dismissed 'pending' banner re-raises on its own once the request flips
         to approved or denied — a stray click can never permanently hide a
         status change. ProvisionRequest carries no id, so identity is the
         org/primary-repo pair it targets: the same thing the banner text
         names. */
      var dismissKey = ('provision:' + (req.org || '') + '/' +
        (req.primary_repo || req.repos || '') + ':' + status).replace(/'/g, '');
      var dismissedReqs = {};
      try {
        dismissedReqs = JSON.parse(localStorage.getItem('hive-dismissed-banners') || '{}') || {};
      } catch (e) { dismissedReqs = {}; } /* corrupted value — show the banner */
      if (dismissedReqs[dismissKey]) { el.style.display = 'none'; return; }
      el.innerHTML = '<div style="background:' + bg + ';border:1px solid ' + border + ';border-radius:8px;padding:12px 16px;margin-bottom:16px;display:flex;align-items:center;gap:10px">' +
        '<span style="font-size:1.1rem">' + icon + '</span>' +
        '<span style="flex:1;font-size:0.85rem;color:var(--text)">' + msg + '</span>' +
        '<button onclick="dismissBanner(\'' + esc(dismissKey) + '\',this)" style="margin-left:auto;background:none;border:none;color:var(--muted);cursor:pointer;font-size:1rem;padding:0 4px" title="Dismiss">&times;</button>' +
        '</div>';
    }

    // normalizeProvisionStatus trims and lowercases a request status so the
    // pending/decided split cannot be defeated by casing or stray whitespace in
    // a stored record. Returns '' for a missing status, which legacy records
    // written before the field existed have — those are treated as pending.
    function normalizeProvisionStatus(status) {
      return String(status == null ? '' : status).trim().toLowerCase();
    }

    // --- Past Requests (decided provision requests) ---
    // Collapsed by default: this is an audit trail, not a work queue, so it
    // should never push the pending action cards off the fold. The choice is
    // remembered per browser under the same 'hive-*-collapsed' key convention
    // used by the cluster health panel.
    var PAST_REQUESTS_COLLAPSED_KEY = 'hive-past-requests-collapsed';
    // A blank github_host on a request means the public instance; a GitHub
    // Enterprise request carries its host (github.ibm.com, …). Used both as the
    // pill label and as the origin for repo links, so Enterprise requesters get
    // a link that actually resolves instead of a dead github.com URL.
    var PAST_REQUESTS_DEFAULT_GITHUB_HOST = 'github.com';
    // Above this many other hives the cell shows a count instead of the full
    // list, with every hive still readable in the title tooltip. Four keeps a
    // typical footprint inline while stopping a heavy user from turning one
    // table cell into a wall of hive IDs.
    var PAST_REQUESTS_MAX_INLINE_OTHER_HIVES = 4;
    // Absent key means collapsed; only an explicit 'false' expands the section.
    var _pastRequestsCollapsed = localStorage.getItem(PAST_REQUESTS_COLLAPSED_KEY) !== 'false';

    // applyPastRequestsCollapsed pushes _pastRequestsCollapsed onto the DOM.
    // Split out from the toggle so the initial render can restore the persisted
    // state without duplicating the show/hide and aria bookkeeping.
    function applyPastRequestsCollapsed() {
      var body = document.getElementById('past-requests-body');
      var toggle = document.getElementById('past-requests-toggle');
      var header = document.getElementById('past-requests-header');
      if (body) body.style.display = _pastRequestsCollapsed ? 'none' : '';
      // ▸ collapsed / ▾ expanded
      if (toggle) toggle.innerHTML = _pastRequestsCollapsed ? '&#9656;' : '&#9662;';
      if (header) header.setAttribute('aria-expanded', _pastRequestsCollapsed ? 'false' : 'true');
    }

    function togglePastRequests() {
      _pastRequestsCollapsed = !_pastRequestsCollapsed;
      localStorage.setItem(PAST_REQUESTS_COLLAPSED_KEY, _pastRequestsCollapsed ? 'true' : 'false');
      applyPastRequestsCollapsed();
    }

    // githubHostPill renders the GitHub instance a request targets. Blank
    // github_host means public github.com; a GHE host is called out with the
    // accent treatment so the admin can place the hive on a cluster that can
    // reach it. Shared by the pending action cards and the Past Requests table
    // so the same concept does not drift into two different styles.
    function githubHostPill(githubHost) {
      /* Public github.com reads green, GitHub Enterprise reads blue, so the
         two instances are distinguishable at a glance without reading the
         text. An absent host is treated as public, matching the label below. */
      var isGHE = !!githubHost && githubHost !== PAST_REQUESTS_DEFAULT_GITHUB_HOST;
      var border = isGHE ? 'var(--blue);color:var(--blue)' : 'var(--green);color:var(--green)';
      return '<span style="font-size:0.68rem;padding:1px 7px;border-radius:999px;border:1px solid ' + border + '">' +
        esc(githubHost || PAST_REQUESTS_DEFAULT_GITHUB_HOST) + '</span>';
    }

    // otherHivesCell summarises the rest of a requester's fleet: which other
    // hives they can sign in to and their role on each, so an admin reading one
    // row sees the person's whole footprint. The list arrives on the payload
    // (admin-only) so this never costs an API call per row.
    function otherHivesCell(otherHives) {
      var list = (otherHives || []).filter(function(e) { return e && e.hive_id; });
      if (!list.length) return '<span style="color:var(--muted);font-size:0.7rem">—</span>';
      // Plain text for the tooltip — a title attribute is not HTML, and esc()
      // does not escape quotes, so nothing user-controlled may be interpolated
      // into the attribute string. Set it via the DOM instead.
      var full = list.map(function(e) { return e.hive_id + ' · ' + (e.role || '?'); }).join('\n');
      var span = document.createElement('span');
      span.style.fontSize = '0.7rem';
      if (list.length > PAST_REQUESTS_MAX_INLINE_OTHER_HIVES) {
        span.textContent = list.length + ' hives';
        span.style.color = 'var(--muted)';
        span.style.borderBottom = '1px dotted var(--border)';
        span.style.cursor = 'help';
      } else {
        span.textContent = list.map(function(e) {
          return e.hive_id + ' · ' + (e.role || '?');
        }).join(', ');
        span.style.color = 'var(--muted)';
        span.style.fontFamily = 'ui-monospace,monospace';
      }
      span.title = full;
      // outerHTML of a textContent-populated node is escaped by the DOM, which
      // covers quotes and apostrophes that esc() would leave through.
      return span.outerHTML;
    }

    // renderRequestHistory renders every already-decided request as a table:
    // who asked, for what, what was decided, by whom, and — for an approval —
    // which hive they actually got. Pending requests stay as action cards above;
    // this is the audit trail, not a work queue, so it is deliberately a dense
    // table rather than more cards.
    // provisionRepoLabel renders a provision request's org/repo as a link to
    // the repository on the GitHub instance the request actually targets.
    // github_host is authoritative: '' means public github.com, a value like
    // github.ibm.com means GitHub Enterprise — hardcoding github.com would send
    // an admin to a 404 (or worse, an unrelated public repo of the same name).
    // Requests with no repo recorded fall back to plain escaped text.
    /* provisionRequesterLabel renders the requester's real name and Slack ID
       beside their GitHub login on the review card. Mapping a login to a person
       is the reason the wizard asks for a name at all, and this is where the
       operator deciding the request needs it.

       Both are free text a user typed. esc() everywhere, and the whole label is
       built as a text node's escaped HTML rather than interpolated into an
       attribute — nothing here is ever placed inside an inline handler, where
       esc() alone would leave an apostrophe live. Older requests predate these
       fields and simply render nothing. */
    function provisionRequesterLabel(pr) {
      if (!pr) return '';
      var name = (pr.full_name || '').trim();
      var slack = (pr.slack_id || '').trim();
      if (!name && !slack) return '';
      var bits = [];
      if (name) bits.push(esc(name));
      if (slack) bits.push('slack: ' + esc(slack));
      return '<span style="font-size:0.75rem;color:var(--muted);margin-left:8px">' +
        bits.join(' &middot; ') + '</span>';
    }

    function provisionRepoLabel(pr) {
      if (!pr) return '';
      var org = pr.org || '';
      var repo = pr.primary_repo || pr.repos || '';
      var text = esc(org) + '/' + esc(repo);
      // repos may be a comma-separated list; only a single concrete repo can be
      // turned into a URL, so leave a multi-repo value as plain text.
      var url = (repo.indexOf(',') === -1) ? ghRepoURL(pr.github_host, org, repo) : '';
      if (!url) return text;
      return '<a href="' + escAttr(url) + '" target="_blank" rel="noopener noreferrer"' +
        ' style="color:inherit;text-decoration:underline;text-decoration-style:dotted">' + text + '</a>';
    }

    function renderRequestHistory(requests) {
      var host = document.getElementById('admin-request-history');
      if (!host) return;
      var decided = (requests || []).filter(function(pr) {
        return pr && normalizeProvisionStatus(pr.status) !== '' && normalizeProvisionStatus(pr.status) !== 'pending';
      });
      // Restore the persisted collapse state on every render — the section is
      // rebuilt on each dashboard poll, so applying it once at load is not enough.
      applyPastRequestsCollapsed();
      var countEl = document.getElementById('past-requests-count');
      if (countEl) countEl.textContent = decided.length ? '(' + decided.length + ')' : '';
      if (!decided.length) {
        host.innerHTML = '<div style="color:var(--muted);font-size:0.75rem;padding:6px 0">No past requests yet.</div>';
        return;
      }
      // Most recently decided first; fall back to requested_at on legacy records
      // that predate decided_at so they still sort somewhere sensible.
      decided.sort(function(a, b) {
        var av = a.decided_at || a.requested_at || '';
        var bv = b.decided_at || b.requested_at || '';
        return bv.localeCompare(av);
      });
      var rows = decided.map(function(pr) {
        var approved = pr.status === 'approved';
        var color = approved ? 'var(--green)' : 'var(--red)';
        var uname = pr.username || '';
        // Link the requester to their GitHub profile. Always github.com: a
        // profile link is about the person, and the account that signed in to
        // the hub is a github.com account even when the ORG they asked for
        // lives on an Enterprise host.
        //
        // The AVATAR carries that link now. The username used to be a second
        // anchor to the same profile; two controls for one destination is noise,
        // so the name is plain text and the face is the affordance.
        var userCell =
          linkedAvatar(uname, PANEL_ACCESS_AVATAR_PX, uname, 'margin-right:6px') +
          (uname
            ? '<span>' + esc(uname) + '</span>'
            : '<span style="color:var(--muted)">—</span>');
        // Repo link, built by the shared provisionRepoLabel(): github_host is
        // empty for public github.com and otherwise a GitHub Enterprise host
        // (github.ibm.com, …), so the origin comes from the record rather than
        // being hardcoded — hardcoding github.com would produce dead links for
        // every Enterprise requester. The helper also declines to link a
        // comma-separated multi-repo value, which has no single URL, and falls
        // back to plain escaped text when there is nothing to link.
        var org = pr.org || '';
        var repo = pr.primary_repo || pr.repos || '';
        var repoCell = (org || repo)
          ? provisionRepoLabel(pr)
          : '<span style="color:var(--muted)">—</span>';
        // For an approval the outcome is the hive they received — linked to the
        // existing SSO handoff endpoint, the same affordance My Hives uses —
        // plus their role on it. For a denial it is the reason, if one was
        // given. Legacy records have neither.
        var outcome;
        if (approved && pr.assigned_hive) {
          var roleSuffix = pr.assigned_role
            ? ' <span style="color:var(--muted);font-size:0.68rem">&middot; ' + esc(pr.assigned_role) + '</span>'
            : '';
          outcome =
            '<a href="/api/saas/hives/' + encodeURIComponent(pr.assigned_hive) + '/open" target="_blank" rel="noopener noreferrer" style="font-family:ui-monospace,monospace;font-size:0.7rem;color:inherit" title="Open dashboard">' +
            esc(pr.assigned_hive) + '</a>' + roleSuffix;
        } else if (!approved && pr.deny_reason) {
          outcome = '<span style="color:var(--muted)">' + esc(pr.deny_reason) + '</span>';
        } else {
          outcome = '<span style="color:var(--muted)">—</span>';
        }
        return '<tr>' +
          '<td style="white-space:nowrap">' + userCell + '</td>' +
          '<td>' + repoCell + '</td>' +
          '<td style="white-space:nowrap">' + githubHostPill(pr.github_host) + '</td>' +
          '<td style="white-space:nowrap">' + acmmBadge(pr.acmm_level) + '</td>' +
          '<td style="white-space:nowrap;color:var(--muted);font-size:0.7rem">' + esc((pr.requested_at || '').substring(0, 10)) + '</td>' +
          '<td style="white-space:nowrap"><span style="color:' + color + ';font-weight:600;font-size:0.72rem">' + esc(pr.status) + '</span></td>' +
          '<td style="white-space:nowrap">' + esc(pr.decided_by || '—') + '</td>' +
          '<td style="white-space:nowrap;color:var(--muted);font-size:0.7rem">' + esc((pr.decided_at || '').substring(0, 10) || '—') + '</td>' +
          '<td>' + outcome + '</td>' +
          '<td>' + otherHivesCell(pr.other_hives) + '</td>' +
          '</tr>';
      }).join('');
      // Header cells here MUST stay in lockstep with the <td> cells emitted
      // above — 10 columns: User, Requested, Host, ACMM, On, Decision, By,
      // When, Assigned / reason, Other hives.
      host.innerHTML =
        '<table class="hive-table" style="width:100%;font-size:0.78rem">' +
        '<thead><tr><th>User</th><th>Requested</th><th>Host</th><th>ACMM</th><th>On</th>' +
        '<th>Decision</th><th>By</th><th>When</th><th>Assigned / reason</th><th>Other hives</th></tr></thead>' +
        '<tbody>' + rows + '</tbody></table>';
    }

    function renderAdminProvisionRequests(requests) {
      var section = document.getElementById('admin-provision-requests');
      var list = document.getElementById('admin-provision-list');
      if (!section || !list) return;
      // History covers decided requests; the cards below cover pending ones. Do
      // this before the pending early-return, or the history disappears the
      // moment the queue empties — which is exactly when it matters most.
      renderRequestHistory(requests);
      // Only genuinely pending requests get Approve/Deny cards. Anything already
      // approved or denied belongs in Past Requests, never in the action queue.
      var pending = (requests || []).filter(function(pr) {
        if (!pr) return false;
        var st = normalizeProvisionStatus(pr.status);
        return st === '' || st === 'pending';
      });
      if (!pending.length) {
        // Keep the section visible when there is history to show, so the table
        // does not vanish along with the empty queue.
        var anyDecided = (requests || []).some(function(pr) {
          if (!pr) return false;
          var st = normalizeProvisionStatus(pr.status);
          return st !== '' && st !== 'pending';
        });
        list.innerHTML = '<div style="color:var(--muted);font-size:0.75rem;padding:6px 0">No pending requests.</div>';
        section.style.display = anyDecided ? '' : 'none';
        return;
      }
      section.style.display = '';
      // Stash by username so the approve-picker modal can read the full request
      // (org/repos/acmm) without threading every field through an onclick string.
      _provisionRequestsByUser = {};
      pending.forEach(function(pr) { _provisionRequestsByUser[pr.username] = pr; });
      var rows = pending.map(function(pr) {
        var avatar = linkedAvatar(pr.username, TABLE_AVATAR_PX, pr.username, 'margin-right:8px');
        return '<div style="display:flex;align-items:center;justify-content:space-between;padding:10px 14px;background:var(--surface);border:1px solid var(--border);border-radius:8px;margin-bottom:8px">' +
          '<div style="display:flex;align-items:center;gap:8px">' +
          avatar +
          '<div>' +
          '<span style="font-size:0.85rem;font-weight:600">' + esc(pr.username) + '</span>' +
          // Who the requester actually IS. Mapping a login to a person is the
          // reason the wizard asks for a name, and this review card is where an
          // operator needs it. esc() only — free text, never markup.
          provisionRequesterLabel(pr) +
          // org/repo links to the repo on the instance the request targets.
          '<span style="font-size:0.75rem;color:var(--muted);margin-left:8px">' + provisionRepoLabel(pr) + '</span>' +
          // Show which GitHub instance the request targets — same pill the Past
          // Requests table uses, so pending and decided rows read alike.
          ' ' + githubHostPill(pr.github_host) +
          ' ' + acmmBadge(pr.acmm_level) +
          '<div style="font-size:0.7rem;color:var(--muted)">' + esc((pr.requested_at || '').substring(0, 10)) + '</div>' +
          '</div></div>' +
          '<div style="display:flex;gap:6px">' +
          '<button onclick="openApproveModal(\'' + esc(pr.username) + '\')" style="padding:5px 14px;background:var(--green);color:#fff;border:none;border-radius:6px;cursor:pointer;font-size:0.78rem;font-weight:600">Approve</button>' +
          '<button onclick="denyProvision(\'' + esc(pr.username) + '\',this)" style="padding:5px 14px;background:var(--red);color:#fff;border:none;border-radius:6px;cursor:pointer;font-size:0.78rem;font-weight:600">Deny</button>' +
          '</div></div>';
      }).join('');
      list.innerHTML = rows;
    }

    var _provisionRequestsByUser = {};

    /* openAssignForUser is the entry point from a click on a user in the admin
       users table. It routes to whichever existing flow fits, rather than adding
       a third assign path:
         - the user has a PENDING provision request  → openApproveModal, which
           approves the request AND assigns a placeholder in one step;
         - otherwise → openAssignModal on an available placeholder, pre-filled
           with the user as owner. openAssignModal needs a concrete placeholder
           id, so we ask the same endpoint the approve picker uses.
       Authorization is unchanged: both modals post to admin-only endpoints
       (/api/saas/approve-provision, /api/saas/hives/{id}/assign), which
       re-check the caller server-side, so this is purely a UI shortcut. */
    async function openAssignForUser(username) {
      if (!_isAdmin) return;
      if (_provisionRequestsByUser[username]) { openApproveModal(username); return; }
      try {
        var resp = await fetch('/api/saas/admin/available-placeholders');
        if (!resp.ok) { hiveToast('Could not load available placeholders', 'error'); return; }
        var data = await resp.json();
        var placeholders = data.placeholders || [];
        if (!placeholders.length) {
          hiveToast('No available placeholder hives to assign — provision one first.', 'error');
          return;
        }
        openAssignModal(placeholders[0].id, username, placeholders);
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function openApproveModal(username) {
      var pr = _provisionRequestsByUser[username] || {username: username};
      var fld = 'width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box';
      var lbl = 'display:block;font-size:0.75rem;color:var(--muted);margin:10px 0 4px';
      var reposText = pr.primary_repo || pr.repos || '';
      var summary =
        '<div style="padding:10px 12px;background:var(--surface);border:1px solid var(--border);border-radius:6px;font-size:0.8rem;margin-bottom:4px">' +
        '<div><span style="color:var(--muted)">User:</span> <strong>' + esc(username) + '</strong></div>' +
        '<div><span style="color:var(--muted)">Org:</span> ' + esc(pr.org || '') + '</div>' +
        '<div><span style="color:var(--muted)">Repos:</span> ' + esc(pr.repos || '') + '</div>' +
        '<div><span style="color:var(--muted)">Primary:</span> ' + esc(pr.primary_repo || '') + '</div>' +
        '<div><span style="color:var(--muted)">ACMM:</span> ' + (pr.acmm_level != null ? pr.acmm_level : '—') + '</div>' +
        '<div><span style="color:var(--muted)">GitHub:</span> ' + esc(pr.github_host || PUBLIC_GITHUB_HOST) + '</div>' +
        '</div>';
      /* The requester's own github_host pre-fills the picker — they already
         told us which GitHub their org lives on. It is only the cluster
         default when the request carries none. The cluster is unknown until a
         placeholder is picked (auto-pick resolves it server-side), so the
         picker opens on "Cluster default" and the note fills in on selection. */
      var approveRequestHost = pr.github_host || '';
      var content =
        summary +
        '<label style="' + lbl + '">Placeholder to assign</label>' +
        '<select id="approve-hive" style="' + fld + '"><option value="">Loading placeholders…</option></select>' +
        '<div style="font-size:0.72rem;color:var(--muted);margin-top:6px">Auto-pick chooses an available placeholder from the request&#39;s pool.</div>' +
        githubHostChoiceMarkup('approve', '', approveRequestHost, fld, lbl) +
        '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:16px">' +
        '<button onclick="closeApproveModal()" style="padding:6px 16px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;cursor:pointer">Cancel</button>' +
        '<button id="approve-submit" onclick="confirmApprove(\'' + esc(username) + '\')" style="padding:6px 16px;background:#3fb950;color:#000;border:none;border-radius:6px;cursor:pointer;font-weight:600">Approve</button>' +
        '</div>';
      var overlay = document.createElement('div');
      overlay.id = 'approve-overlay';
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:2000;display:flex;align-items:center;justify-content:center';
      overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:24px;max-width:440px;width:90%;max-height:88vh;overflow-y:auto">' +
        '<h3 style="margin:0 0 12px 0;font-size:1rem">Approve &amp; assign placeholder</h3>' + content + '</div>';
      overlay.addEventListener('click', function(e) { if (e.target === overlay) closeApproveModal(); });
      document.body.appendChild(overlay);
      /* No cluster is known yet (auto-pick is the default selection), so the
         "Cluster default" note stays generic until a placeholder is chosen. */
      wireGitHubHostChoice('approve', '');
      // Populate the dropdown with available placeholders (auto-pick default).
      var _approvePlaceholders = [];
      try {
        var resp = await fetch('/api/saas/admin/available-placeholders');
        var data = await resp.json();
        var sel = document.getElementById('approve-hive');
        if (!sel) return;
        _approvePlaceholders = data.placeholders || [];
        var opts = '<option value="">Auto-pick</option>';
        _approvePlaceholders.forEach(function(p) {
          opts += '<option value="' + esc(p.id) + '">' + esc(p.id) + '  (' + esc(p.cluster_id || 'default') + ')</option>';
        });
        sel.innerHTML = opts;
        /* Picking a concrete placeholder reveals its cluster, so the "Cluster
           default" option can finally name the GitHub instance the hive will
           land on. Auto-pick ('') leaves it unnamed — the server resolves the
           cluster and backfills the host itself. */
        sel.addEventListener('change', function() {
          var ph = (_approvePlaceholders || []).reduce(function(m, p) { return (p && p.id === sel.value) ? p : m; }, null);
          var host = ph ? clusterGitHubHost(ph.cluster_id || '') : '';
          var hostSel = document.getElementById('approve-ghhost');
          if (hostSel && hostSel.options && hostSel.options.length) {
            hostSel.options[0].textContent = host ? 'Cluster default — ' + host : 'Cluster default';
          }
          wireGitHubHostChoice('approve', host);
        });
      } catch(e) {
        var sel2 = document.getElementById('approve-hive');
        if (sel2) sel2.innerHTML = '<option value="">Auto-pick</option>';
      }
    }
    function closeApproveModal() {
      var ov = document.getElementById('approve-overlay');
      if (ov) ov.remove();
    }
    async function confirmApprove(username) {
      var sel = document.getElementById('approve-hive');
      var hiveId = sel ? sel.value : '';
      var submit = document.getElementById('approve-submit');
      if (submit) { submit.disabled = true; submit.textContent = 'Assigning...'; }
      try {
        // Approving assigns a placeholder (the chosen one, or auto-pick from the
        // correct pool when hive_id is empty) and marks the request fulfilled.
        var resp = await fetch('/api/saas/approve-provision/' + encodeURIComponent(username), {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          /* github_host: '' = use the request's host, else the cluster
             default (server-side); 'public' = force public github.com. */
          body: JSON.stringify({hive_id: hiveId, github_host: githubHostChoiceValue('approve')})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Assign failed', 'error'); if (submit) { submit.disabled = false; submit.textContent = 'Approve'; } return; }
        closeApproveModal();
        hiveToast('Approved ' + username + ' → ' + (hiveId || 'auto') + ' (' + (data.hive_id || 'a hive') + ')', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); if (submit) { submit.disabled = false; submit.textContent = 'Approve'; } }
    }

    async function denyProvision(username, btn) {
      if (!await hiveConfirm('Deny provision request from ' + username + '?')) return;
      btn.disabled = true;
      btn.textContent = 'Denying...';
      try {
        var resp = await fetch('/api/saas/deny-provision/' + encodeURIComponent(username), {method: 'DELETE'});
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Deny failed', 'error'); btn.disabled = false; btn.textContent = 'Deny'; return; }
        hiveToast('Provision request denied for ' + username, 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); btn.disabled = false; btn.textContent = 'Deny'; }
    }


    // --- Cluster Health Panel ---
    var CLUSTER_HEALTH_POLL_MS = 30000;
    var CLUSTER_CPU_WARN_PCT = 60;
    var CLUSTER_CPU_DANGER_PCT = 80;
    var CLUSTER_MEM_WARN_PCT = 60;
    var CLUSTER_MEM_DANGER_PCT = 80;
    // Disk thresholds mirror kubelet's own behaviour on these nodes rather
    // than round numbers: image garbage collection starts at
    // imageGCHighThresholdPercent (85% used) and hard eviction fires at
    // evictionHard nodefs.available<10% (90% used). Amber therefore means
    // "kubelet is already reclaiming disk", red means "pods are being evicted".
    var CLUSTER_DISK_WARN_PCT = 85;
    var CLUSTER_DISK_DANGER_PCT = 90;
    // MiB per GiB, for rendering disk_*_mb values as GiB.
    var MB_PER_GB = 1024;
    var _clusterHealthCollapsed = localStorage.getItem('hive-cluster-health-collapsed') !== 'false';

    function toggleClusterHealth() {
      _clusterHealthCollapsed = !_clusterHealthCollapsed;
      localStorage.setItem('hive-cluster-health-collapsed', _clusterHealthCollapsed ? 'true' : 'false');
      var body = document.getElementById('cluster-health-body');
      var toggle = document.getElementById('cluster-health-toggle');
      if (body) body.style.display = _clusterHealthCollapsed ? 'none' : '';
      if (toggle) toggle.style.transform = _clusterHealthCollapsed ? '' : 'rotate(90deg)';
    }

    function healthBarColor(pct, warnThreshold, dangerThreshold) {
      if (pct >= dangerThreshold) return 'var(--red)';
      if (pct >= warnThreshold) return 'var(--accent)';
      return 'var(--green)';
    }

    var SPARKLINE_BLOCKS = '▁▂▃▄▅▆▇█';
    var SPARKLINE_HISTORY_LEN = 10;
    var _clusterSparkHistory = {};

    function pushSparkPoint(nodeKey, metric, value) {
      var key = nodeKey + ':' + metric;
      if (!_clusterSparkHistory[key]) _clusterSparkHistory[key] = [];
      _clusterSparkHistory[key].push(value);
      if (_clusterSparkHistory[key].length > SPARKLINE_HISTORY_LEN) _clusterSparkHistory[key].shift();
    }

    function renderUnicodeSparkline(nodeKey, metric, color) {
      var key = nodeKey + ':' + metric;
      var pts = _clusterSparkHistory[key] || [];
      if (pts.length < 2) return '';
      var max = Math.max.apply(null, pts);
      if (max === 0) max = 1;
      var blocks = SPARKLINE_BLOCKS;
      var spark = pts.map(function(v) {
        var idx = Math.round((v / max) * (blocks.length - 1));
        return blocks[Math.min(idx, blocks.length - 1)];
      }).join('');
      return '<span style="font-family:monospace;font-size:0.7rem;color:' + color + ';letter-spacing:-1px">' + spark + '</span>';
    }

    function renderHealthMetric(label, used, total, unit, pct, warnThreshold, dangerThreshold, nodeKey, metric) {
      var color = healthBarColor(pct, warnThreshold, dangerThreshold);
      pushSparkPoint(nodeKey, metric, pct);
      var spark = renderUnicodeSparkline(nodeKey, metric, color);
      return '<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:4px">' +
        '<span style="font-size:0.7rem;color:var(--muted)">' + label + '</span>' +
        '<span style="display:flex;align-items:center;gap:6px">' +
        spark +
        '<span style="font-family:monospace;font-size:0.75rem;color:' + color + '">' + used + ' / ' + total + ' ' + unit + '</span>' +
        '<span style="font-size:0.7rem;min-width:28px;text-align:right;color:' + color + '">' + pct + '%</span>' +
        '</span></div>';
    }

    // Renders a metric row whose value could not be collected. Deliberately
    // dashed rather than 0%, so an unreachable node never reads as healthy.
    function renderHealthMetricUnknown(label, reason) {
      return '<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:4px" title="' + esc(reason || 'No data') + '">' +
        '<span style="font-size:0.7rem;color:var(--muted)">' + label + '</span>' +
        '<span style="display:flex;align-items:center;gap:6px">' +
        '<span style="font-family:monospace;font-size:0.75rem;color:var(--muted)">— / — GiB</span>' +
        '<span style="font-size:0.7rem;min-width:28px;text-align:right;color:var(--muted)">—</span>' +
        '</span></div>';
    }

    // Disk usage is optional: the collector omits it when the kubelet
    // stats endpoint was unreachable, so absent means unknown, not zero.
    function renderNodeDiskMetric(n, nk) {
      if (n.disk_percent == null || n.disk_total_mb == null || n.disk_used_mb == null) {
        return renderHealthMetricUnknown('DISK', 'Disk usage unavailable for this node');
      }
      var diskUsedGB = (n.disk_used_mb / MB_PER_GB).toFixed(1);
      var diskTotalGB = Math.round(n.disk_total_mb / MB_PER_GB);
      return renderHealthMetric('DISK', diskUsedGB, diskTotalGB, 'GiB', n.disk_percent,
        CLUSTER_DISK_WARN_PCT, CLUSTER_DISK_DANGER_PCT, nk, 'disk');
    }

    function renderNodeCard(n) {
      var nk = n.name;
      var readyBadge = (n.conditions || []).indexOf('Ready') >= 0
        ? '<span style="color:var(--green);font-size:0.7rem;font-weight:600">Ready</span>'
        : '<span style="color:var(--red);font-size:0.7rem;font-weight:600">NotReady</span>';
      var diskWarn = n.disk_pressure
        ? '<div style="margin-top:6px;padding:4px 8px;background:rgba(239,68,68,0.1);border:1px solid rgba(239,68,68,0.3);border-radius:4px;font-size:0.7rem;color:var(--red)">⚠ Disk Pressure</div>'
        : '';
      var cpuUsed = (n.cpu_used_millicores / 1000).toFixed(1);
      var memUsedGB = (n.mem_used_mb / 1024).toFixed(1);
      var memTotalGB = Math.round(n.mem_total_mb / 1024);
      var hiveCount = n.hive_count || 0;
      var hivePill = '<span style="padding:2px 8px;background:var(--bg);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;color:var(--muted)">' + hiveCount + (hiveCount === 1 ? ' hive' : ' hives') + '</span>';
      return '<div style="background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:14px">' +
        '<div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:8px">' +
        '<span style="font-family:monospace;font-size:0.8rem;color:var(--text)">' + esc(nk) + '</span>' +
        '<span style="display:flex;align-items:center;gap:6px">' + readyBadge +
        hivePill +
        '<span style="padding:2px 8px;background:var(--bg);border:1px solid var(--border);border-radius:4px;font-size:0.65rem;color:var(--muted)">' + (n.pods || 0) + '/' + (n.pod_capacity || 0) + ' pods</span>' +
        '</span></div>' +
        renderHealthMetric('CPU', cpuUsed, n.cpu_cores, 'cores', n.cpu_percent, CLUSTER_CPU_WARN_PCT, CLUSTER_CPU_DANGER_PCT, nk, 'cpu') +
        renderHealthMetric('MEM', memUsedGB, memTotalGB, 'GB', n.mem_percent, CLUSTER_MEM_WARN_PCT, CLUSTER_MEM_DANGER_PCT, nk, 'mem') +
        renderNodeDiskMetric(n, nk) +
        diskWarn +
        '</div>';
    }

    async function loadClusterHealth() {
      if (!_isAdmin) return;
      try {
        var resp = await fetch('/api/saas/cluster-health');
        if (resp.status === 403) {
          document.getElementById('cluster-health-section').style.display = 'none';
          return;
        }
        if (!resp.ok) return;
        var data = await resp.json();
        document.getElementById('cluster-health-section').style.display = '';

        var s = data.summary || {};
        var summaryBar = document.getElementById('cluster-health-summary-bar');
        if (summaryBar) {
          pushSparkPoint('_cluster', 'cpu', s.total_cpu_percent || 0);
          pushSparkPoint('_cluster', 'mem', s.total_mem_percent || 0);
          var cpuColor = healthBarColor(s.total_cpu_percent || 0, CLUSTER_CPU_WARN_PCT, CLUSTER_CPU_DANGER_PCT);
          var memColor = healthBarColor(s.total_mem_percent || 0, CLUSTER_MEM_WARN_PCT, CLUSTER_MEM_DANGER_PCT);
          var clusterCount = (data.clusters || []).length;
          // Disk is omitted when no node reported usage; showing 0% there
          // would claim the fleet is healthy on no evidence at all.
          var diskSegment = '';
          if (s.total_disk_percent != null) {
            pushSparkPoint('_cluster', 'disk', s.total_disk_percent);
            var diskColor = healthBarColor(s.total_disk_percent, CLUSTER_DISK_WARN_PCT, CLUSTER_DISK_DANGER_PCT);
            diskSegment = renderUnicodeSparkline('_cluster', 'disk', diskColor) +
              ' <span style="color:' + diskColor + '">' + s.total_disk_percent + '% disk</span> · ';
          } else {
            diskSegment = '<span style="color:var(--muted)" title="No node reported live disk usage">— disk</span> · ';
          }
          summaryBar.innerHTML = clusterCount + ' cluster' + (clusterCount !== 1 ? 's' : '') + ' · ' +
            (s.total_nodes || 0) + ' nodes · ' + (s.total_cpu_cores || 0) + ' vCPU · ' +
            renderUnicodeSparkline('_cluster', 'cpu', cpuColor) + ' <span style="color:' + cpuColor + '">' + (s.total_cpu_percent || 0) + '% cpu</span> · ' +
            renderUnicodeSparkline('_cluster', 'mem', memColor) + ' <span style="color:' + memColor + '">' + (s.total_mem_percent || 0) + '% mem</span> · ' +
            diskSegment +
            (s.hive_count || 0) + ' hives';
        }

        var body = document.getElementById('cluster-health-body');
        var toggle = document.getElementById('cluster-health-toggle');
        if (!_clusterHealthCollapsed) {
          if (body) body.style.display = '';
          if (toggle) toggle.style.transform = 'rotate(90deg)';
        }

        var grid = document.getElementById('cluster-health-grid');
        if (!grid) return;

        // Render per-cluster sections if available, otherwise fall back to flat nodes.
        var clusters = data.clusters || [];
        if (clusters.length > 0) {
          grid.style.display = 'block';
          grid.innerHTML = clusters.map(function(c) {
            var cLabel = c.name || c.id;
            var cs = c.summary || {};
            var cCpuColor = healthBarColor(cs.total_cpu_percent || 0, CLUSTER_CPU_WARN_PCT, CLUSTER_CPU_DANGER_PCT);
            var cMemColor = healthBarColor(cs.total_mem_percent || 0, CLUSTER_MEM_WARN_PCT, CLUSTER_MEM_DANGER_PCT);
            // Absent disk data (e.g. an unreachable cluster) renders dashed,
            // never as 0% — a full disk and an unknown disk must not look alike.
            var cDiskSegment;
            if (cs.total_disk_percent != null) {
              var cDiskColor = healthBarColor(cs.total_disk_percent, CLUSTER_DISK_WARN_PCT, CLUSTER_DISK_DANGER_PCT);
              cDiskSegment = '<span style="color:' + cDiskColor + '">' + cs.total_disk_percent + '% disk</span> · ';
            } else {
              cDiskSegment = '<span style="color:var(--muted)" title="No node in this cluster reported live disk usage">— disk</span> · ';
            }
            var gpuLine = '';
            if (c.gpu_summary) {
              gpuLine = ' · <span style="color:var(--green)">' + c.gpu_summary.allocatable_gpus + '/' + c.gpu_summary.total_gpus + ' GPUs</span>';
            }
            // Remaining hive capacity: omitted entirely when the collector had
            // no pod request data (field absent); 0 means the cluster is full.
            var capacityLine = '';
            if (cs.hive_capacity_remaining != null) {
              var capRemaining = cs.hive_capacity_remaining;
              capacityLine = ' · <span title="Estimated headroom: per-hive CPU/memory request footprint bin-packed into free (allocatable minus requested) capacity on Ready, schedulable nodes only">room for ~' + capRemaining + ' more hive' + (capRemaining === 1 ? '' : 's') + '</span>';
            }
            var errorLine = c.error ? '<div style="margin:8px 0;padding:6px 10px;background:rgba(239,68,68,0.1);border:1px solid rgba(239,68,68,0.3);border-radius:6px;font-size:0.75rem;color:var(--red)">' + esc(c.error) + '</div>' : '';
            var headerHtml = '<div style="display:flex;align-items:center;gap:8px;margin:16px 0 8px">' +
              clusterBadge(c.id, c.name) +
              '<span style="font-size:0.85rem;color:var(--text);font-weight:600">' + esc(cLabel) + '</span>' +
              '<span style="font-size:0.75rem;color:var(--muted)">' +
              (cs.total_nodes || 0) + ' nodes · ' + (cs.total_cpu_cores || 0) + ' vCPU · ' +
              '<span style="color:' + cCpuColor + '">' + (cs.total_cpu_percent || 0) + '% cpu</span> · ' +
              '<span style="color:' + cMemColor + '">' + (cs.total_mem_percent || 0) + '% mem</span> · ' +
              cDiskSegment +
              (c.hive_count || 0) + ' hives' + capacityLine + gpuLine +
              '</span></div>';
            var nodesHtml = (c.nodes || []).length > 0
              ? '<div style="display:grid;grid-template-columns:repeat(2,1fr);gap:12px">' + (c.nodes || []).map(renderNodeCard).join('') + '</div>'
              : '';
            return headerHtml + errorLine + nodesHtml;
          }).join('');
        } else {
          // Fallback: flat node list (single cluster / backward compat).
          grid.style.display = 'grid';
          grid.style.gridTemplateColumns = 'repeat(2,1fr)';
          var nodes = data.nodes || [];
          if (!nodes.length) {
            grid.innerHTML = '<div style="color:var(--muted);font-size:0.85rem;grid-column:span 2">No node data available</div>';
            return;
          }
          grid.innerHTML = nodes.map(renderNodeCard).join('');
        }
      } catch(e) {
        console.error('cluster health error:', e);
      }
    }

    async function loadClusters() {
      try {
        var resp = await fetch('/api/hub/clusters');
        if (!resp.ok) return;
        var clusters = await resp.json();
        _clusterList = clusters || [];
        var sel = document.getElementById('f-cluster');
        if (!sel || !clusters || !clusters.length) return;
        sel.innerHTML = clusters.map(function(c) {
          var caps = [];
          if (c.has_gpu) caps.push('GPU');
          if (c.arch) caps.push(c.arch);
          /* Surface the GitHub host: on a GitHub Enterprise cluster the App
             lives on that GHE instance, and picking the wrong cluster is the
             difference between an install request the org admin can see and
             one that silently lands on public github.com. */
          if (c.github_host && c.github_host !== PUBLIC_GITHUB_HOST) caps.push(c.github_host);
          var label = c.name || c.id;
          if (caps.length) label += ' (' + caps.join(', ') + ')';
          return '<option value="' + esc(c.id) + '">' + esc(label) + '</option>';
        }).join('');
        sel.removeEventListener('change', syncCreateModalInstallLinks);
        sel.addEventListener('change', syncCreateModalInstallLinks);
        syncCreateModalInstallLinks();
      } catch(e) { /* cluster dropdown stays at default */ }
    }

    /* PUBLIC_GITHUB_HOST is the bare hostname the hub reports for a cluster
       with no GitHub Enterprise override. Anything else is a GHE instance. */
    var PUBLIC_GITHUB_HOST = 'github.com';

    /* selectedClusterEntry returns the ClusterListEntry for the cluster chosen
       in the create-hive modal, or null when the list has not loaded. */
    function selectedClusterEntry() {
      var sel = document.getElementById('f-cluster');
      if (!sel || !sel.value) return null;
      var list = _clusterList || [];
      for (var i = 0; i < list.length; i++) {
        if (list[i] && list[i].id === sel.value) return list[i];
      }
      return null;
    }

    /* PUBLIC_GITHUB_SENTINEL is the value the assign/approve APIs accept to
       FORCE public github.com on a cluster whose defaults point at a GitHub
       Enterprise instance. It mirrors githubHostPublic on the server, where
       effectiveGitHubBaseURL resolves it to "" (public). It is deliberately
       NOT a hostname — the server exempts it from the hostname validator. */
    var PUBLIC_GITHUB_SENTINEL = 'public';

    /* clusterEntryById returns the ClusterListEntry for a cluster id, or null
       when the cluster list has not loaded (or the id is unknown). An empty id
       is how the hub records "the default cluster", so fall through to a
       lookup rather than guessing a host. */
    function clusterEntryById(clusterId) {
      if (!clusterId) return null;
      var list = _clusterList || [];
      for (var i = 0; i < list.length; i++) {
        if (list[i] && list[i].id === clusterId) return list[i];
      }
      return null;
    }

    /* clusterGitHubHost returns the bare GitHub host a cluster's hives default
       to ('github.com' for public GitHub), or '' when the cluster is unknown /
       the list has not loaded yet. Never guess github.com for an unknown
       cluster: silently claiming "public" is the exact bug this control
       exists to surface. */
    function clusterGitHubHost(clusterId) {
      var c = clusterEntryById(clusterId);
      return (c && c.github_host) || '';
    }

    /* githubHostChoiceMarkup renders the GitHub-instance picker shared by the
       assign and approve modals.

       Options are: the cluster's own host (the default, and the right answer
       in almost every case), explicit public github.com, and a free-text host
       for the rare cross-instance assignment. clusterHost may be '' when the
       cluster list has not loaded — the control then opens on "Use cluster
       default", which submits nothing and lets the SERVER's cluster backfill
       decide, exactly as if the control did not exist. */
    function githubHostChoiceMarkup(idPrefix, clusterHost, requestHost, fld, lbl) {
      var isGHE = clusterHost && clusterHost !== PUBLIC_GITHUB_HOST;
      var defaultLabel = clusterHost
        ? 'Cluster default — ' + esc(clusterHost)
        : 'Cluster default';
      /* A provision request's github_host, when present, is what the REQUESTER
         told us their org lives on. Prefer it over the cluster default: they
         know which GitHub their org is on and we should not silently override
         them. */
      var preselectCustom = !!(requestHost && requestHost !== clusterHost && requestHost !== PUBLIC_GITHUB_HOST);
      var opts =
        '<option value="">' + defaultLabel + '</option>' +
        '<option value="' + esc(PUBLIC_GITHUB_SENTINEL) + '">Public github.com' + (isGHE ? ' (override)' : '') + '</option>' +
        '<option value="__custom__"' + (preselectCustom ? ' selected' : '') + '>Other GitHub Enterprise host…</option>';
      return '<label style="' + lbl + '">GitHub instance</label>' +
        '<select id="' + esc(idPrefix) + '-ghhost" style="' + fld + '">' + opts + '</select>' +
        '<input id="' + esc(idPrefix) + '-ghhost-custom" style="' + fld + ';margin-top:6px;display:' + (preselectCustom ? 'block' : 'none') + '" ' +
          'placeholder="github.example.com" value="' + esc(preselectCustom ? requestHost : '') + '">' +
        '<div id="' + esc(idPrefix) + '-ghhost-note" style="font-size:0.72rem;color:var(--muted);margin-top:6px"></div>';
    }

    /* wireGitHubHostChoice attaches the behaviour for a picker rendered by
       githubHostChoiceMarkup: show the free-text box only for "Other", and
       keep a live note of exactly which GitHub instance the hive will target.
       Uses addEventListener rather than inline handlers because the values
       involved (hosts, org names) are user-controlled. */
    function wireGitHubHostChoice(idPrefix, clusterHost) {
      var sel = document.getElementById(idPrefix + '-ghhost');
      var custom = document.getElementById(idPrefix + '-ghhost-custom');
      var note = document.getElementById(idPrefix + '-ghhost-note');
      if (!sel) return;
      function render() {
        var isCustom = sel.value === '__custom__';
        if (custom) custom.style.display = isCustom ? 'block' : 'none';
        if (!note) return;
        var target;
        if (isCustom) {
          target = (custom && custom.value.trim()) || '';
        } else if (sel.value === PUBLIC_GITHUB_SENTINEL) {
          target = PUBLIC_GITHUB_HOST;
        } else {
          target = clusterHost || '';
        }
        if (!target) {
          note.textContent = 'This hive will use its cluster’s GitHub instance.';
          note.style.color = 'var(--muted)';
          return;
        }
        var ghe = target !== PUBLIC_GITHUB_HOST;
        note.textContent = 'This hive will target ' + target + '.' +
          (ghe ? ' The GitHub App must be installed on the org on ' + target + ' — a public github.com App will not authenticate there.' : '');
        note.style.color = ghe ? 'var(--accent, #58a6ff)' : 'var(--muted)';
      }
      /* Idempotent: this is re-invoked when the assign modal's placeholder
         picker changes the cluster, and binding a second listener each time
         would leave a stale closure (holding the OLD clusterHost) racing the
         new one. Store the current renderer and swap it out. */
      if (sel._ghHostRender) {
        sel.removeEventListener('change', sel._ghHostRender);
        if (custom) custom.removeEventListener('input', sel._ghHostRender);
      }
      sel._ghHostRender = render;
      sel.addEventListener('change', render);
      if (custom) custom.addEventListener('input', render);
      render();
    }

    /* githubHostChoiceValue returns what to send as github_host: '' for
       "cluster default" (the server backfills), the 'public' sentinel, or a
       typed GHE hostname. */
    function githubHostChoiceValue(idPrefix) {
      var sel = document.getElementById(idPrefix + '-ghhost');
      if (!sel) return '';
      if (sel.value !== '__custom__') return sel.value;
      var custom = document.getElementById(idPrefix + '-ghhost-custom');
      return (custom && custom.value.trim()) || '';
    }

    /* syncCreateModalInstallLinks points every "install the app" link in the
       create-hive modal at the SELECTED cluster's GitHub host. The hub serves
       one page to admins provisioning onto both public GitHub and GitHub
       Enterprise clusters, so a static github.com anchor is wrong for half of
       them: a GHE user following it lands on public github.com and their org
       admin never sees the pending install request.

       The URL comes from the server (config.GitHubConfig.AppInstallURL), which
       also honours a per-cluster app slug — a GHE instance hosts its own App
       registration and it is rarely named "kubestellar-hive". */
    function syncCreateModalInstallLinks() {
      var c = selectedClusterEntry();
      var url = (c && c.app_install_url) || '';
      var host = (c && c.github_host) || PUBLIC_GITHUB_HOST;
      var isGHE = host !== PUBLIC_GITHUB_HOST;
      var ids = ['auth-info-app-link', 'auth-info-later-link', 'auth-later-install-link'];
      for (var i = 0; i < ids.length; i++) {
        var el = document.getElementById(ids[i]);
        if (!el) continue;
        if (url) {
          el.href = url;
          el.removeAttribute('aria-disabled');
        } else {
          /* No URL yet (clusters still loading). Do not fall back to a
             github.com guess — a dead link is safer than one that sends a
             GHE admin to the wrong GitHub. */
          el.removeAttribute('href');
          el.setAttribute('aria-disabled', 'true');
        }
      }
      var appLink = document.getElementById('auth-info-later-link');
      if (appLink) appLink.textContent = '→ Install app on ' + host + ' now';
      var btn = document.getElementById('auth-later-install-link');
      if (btn) btn.textContent = 'Install app on your ' + (isGHE ? host + ' ' : '') + 'org';
      var note = document.getElementById('auth-later-ghe-note');
      if (note) {
        if (isGHE) {
          note.textContent = 'This cluster targets ' + host + '. The GitHub App must be registered and'
            + ' installed on that GitHub Enterprise instance — a public github.com install will not'
            + ' reach it, and the ' + host + ' org admin will see no pending request.';
          note.style.display = '';
        } else {
          note.textContent = '';
          note.style.display = 'none';
        }
      }
    }

    // OpenRouter scan-to-fund return: toast the result and clear the query flag
    // so a reload doesn't re-toast.
    function handleOpenRouterReturn() {
      try {
        var p = new URLSearchParams(window.location.search);
        var or = p.get('openrouter');
        if (or === 'connected') hiveToast('OpenRouter funded — the key is being delivered to the hive', 'success');
        else if (or === 'error') hiveToast('OpenRouter funding failed — please try again', 'error');
        if (or) {
          p.delete('openrouter');
          var qs = p.toString();
          history.replaceState(null, '', window.location.pathname + (qs ? '?' + qs : ''));
        }
      } catch (e) { /* non-fatal */ }
    }

    async function init() {
      /* Restore grouping, collapse state and saved views (including applying
         the default view) BEFORE the first loadHives(), so the initial render
         already reflects the operator's view rather than flashing the ungrouped
         list and then rearranging. */
      loadDashViewPrefs();
      /* Sort is resolved AFTER loadDashViewPrefs because applying a default
         saved view sets a key/direction of its own: the operator's explicitly
         stored sort is the more specific signal and must have the last word.
         Both run before the first paint so no render happens in the wrong
         order. */
      loadHiveSortPrefs();
      /* Progressive paint: put the last known fleet on screen immediately so a
         returning operator sees their hives instead of a spinner. The network
         path below still runs unconditionally and reconciles the list; this
         only changes what fills the gap while it is in flight. Must come after
         loadDashViewPrefs() so the cached rows honour the active view. */
      paintCachedHives();
      /* Record the offset from here on. Attached before the awaits below so a
         scroll during a slow load is still captured. */
      window.addEventListener('scroll', persistHiveScroll, {passive: true});
      await loadUser();
      await autoRequestAccessFromUrl();
      await loadHives();
      /* Restore AFTER the real rows are in the DOM — loadHives' renderHives is
         what gives the document its full height, and restoreHiveScroll still
         waits for that height before it commits (see its frame loop). Calling
         it any earlier clamps the offset to a short page and lands at the top. */
      restoreHiveScroll();
      await loadAdminUsers();
      if (!_adminLoaded) setTimeout(loadAdminUsers, 2000);
      loadClusterHealth();
      loadClusters();
      handleOpenRouterReturn();
    }
    init();
    var POLL_INTERVAL_MS = 30000;
    setInterval(loadHives, POLL_INTERVAL_MS);
    setInterval(loadAdminUsers, POLL_INTERVAL_MS);
    setInterval(loadClusterHealth, CLUSTER_HEALTH_POLL_MS);
    var _refreshTimer = null;
    var REFRESH_DEBOUNCE_MS = 500;
    function debouncedRefresh() {
      if (_refreshTimer) return;
      _refreshTimer = setTimeout(function() { _refreshTimer = null; loadHives(); loadAdminUsers(); }, REFRESH_DEBOUNCE_MS);
    }
    document.addEventListener('visibilitychange', function() { if (!document.hidden) debouncedRefresh(); });
    window.addEventListener('focus', debouncedRefresh);

    var _allUsers = [];
    var _adminLoaded = false;
    var _adminExpandedUsers = {};
    var _hiveRegistry = [];
    var _userSortKey = 'created_at', _userSortAsc = false;

    /* --- Collapsible "Hub Admin — Users" section ---
       The roster runs to ~90 records, so an expanded users table pushes the
       Hub Banner and Cluster Health sections far below the fold. An operator
       is normally here for hives, not for the roster, so this follows the same
       convention as the other long admin sections (Past Requests, Cluster
       Health): collapsed by default, remembered per browser under the same
       'hive-*-collapsed' localStorage key convention. Absent key means
       collapsed; only an explicit 'false' expands, so a first visit and a
       corrupted value behave identically. */
    var ADMIN_USERS_COLLAPSED_KEY = 'hive-admin-users-collapsed';
    var _adminUsersCollapsed = localStorage.getItem(ADMIN_USERS_COLLAPSED_KEY) !== 'false';

    /* applyAdminUsersCollapsed pushes _adminUsersCollapsed onto the DOM. Split
       out from the toggle so the initial render and expandAdminUsersSection()
       can restore state without duplicating the show/hide + aria bookkeeping. */
    function applyAdminUsersCollapsed() {
      var body = document.getElementById('admin-users-body');
      var toggle = document.getElementById('admin-users-toggle');
      var header = document.getElementById('admin-users-header');
      if (body) body.style.display = _adminUsersCollapsed ? 'none' : '';
      /* ▸ collapsed / ▾ expanded */
      if (toggle) toggle.innerHTML = _adminUsersCollapsed ? '&#9656;' : '&#9662;';
      if (header) header.setAttribute('aria-expanded', _adminUsersCollapsed ? 'false' : 'true');
    }

    /* persistAdminUsersCollapsed writes the current state. localStorage throws
       under private-browsing quota; collapsing must still work in-session, so
       the write is best-effort. */
    function persistAdminUsersCollapsed() {
      try {
        localStorage.setItem(ADMIN_USERS_COLLAPSED_KEY, _adminUsersCollapsed ? 'true' : 'false');
      } catch(e) {}
    }

    function toggleAdminUsers() {
      _adminUsersCollapsed = !_adminUsersCollapsed;
      persistAdminUsersCollapsed();
      applyAdminUsersCollapsed();
    }

    /* expandAdminUsersSection opens the section and PERSISTS that, mirroring
       expandAllHiveSections (#2348). Anything that reveals a row inside this
       section must go through here rather than flipping display directly: an
       in-memory-only expand silently re-collapses on the next reload, so the
       row the operator was just sent to would vanish again. */
    function expandAdminUsersSection() {
      if (!_adminUsersCollapsed) return;
      _adminUsersCollapsed = false;
      persistAdminUsersCollapsed();
      applyAdminUsersCollapsed();
    }

    /* The count beside the header is the only signal of roster size while the
       section is collapsed, so it is kept current on every render. */
    function updateAdminUsersCount(shown, total) {
      var el = document.getElementById('admin-users-count');
      if (!el) return;
      var n = Number(total) || 0;
      el.textContent = (Number(shown) === n)
        ? '(' + n + ')'
        : '(' + (Number(shown) || 0) + ' of ' + n + ')';
    }

    function fmtUserTS(ts) {
      if (!ts) return '';
      var d = new Date(ts);
      if (isNaN(d.getTime())) return ts.substring(0, 10);
      // 12-hour clock with a compact single-letter meridiem (e.g. "9:21p").
      var parts = d.toLocaleString('en-US', {year:'numeric',month:'2-digit',day:'2-digit',hour:'numeric',minute:'2-digit',hour12:true,timeZone:'America/New_York'}).replace(',','').split(' ');
      var meridiem = (parts.pop() || '').toLowerCase().charAt(0); // "AM"->"a", "PM"->"p"
      return parts.join(' ') + meridiem + ' EDT';
    }

    function sortUsers(key) {
      if (_userSortKey === key) { _userSortAsc = !_userSortAsc; } else { _userSortKey = key; _userSortAsc = true; }
      applySortUsers();
    }

    function applySortUsers() {
      var key = _userSortKey;
      var q = (document.getElementById('user-search') ? document.getElementById('user-search').value : '').toLowerCase();
      // Search matches the GitHub login and the contact fields, so an admin can
      // find someone by real name or by something they wrote in the notes —
      // which is the point of keeping notes here in the first place.
      var filtered = (_allUsers || []).filter(function(u) {
        if (!q) return true;
        if (!u) return false;
        var hay = [u.github_username, u.full_name, u.slack_id, u.notes]
          .filter(function(v) { return !!v; }).join(' ').toLowerCase();
        return hay.includes(q);
      });
      var sorted = filtered.slice().sort(function(a, b) {
        var va, vb;
        if (key === 'hiveCount') {
          var regIds = new Set((_hiveRegistry || []).map(function(h) { return h.id; }));
          va = Object.keys(a.hives || {}).filter(function(h) { return regIds.has(h); }).length;
          vb = Object.keys(b.hives || {}).filter(function(h) { return regIds.has(h); }).length;
        } else if (key === 'status') {
          va = a.blocked ? 1 : 0;
          vb = b.blocked ? 1 : 0;
        } else {
          va = a[key] || ''; vb = b[key] || '';
        }
        if (typeof va === 'number' && typeof vb === 'number') return _userSortAsc ? va - vb : vb - va;
        return _userSortAsc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
      });
      updateAdminUsersCount(sorted.length, (_allUsers || []).length);
      renderUsers(sorted, true);
    }

    function toggleAdminExpand(username) {
      _adminExpandedUsers[username] = !_adminExpandedUsers[username];
      var el = document.getElementById('expand-' + username);
      if (el) el.style.display = _adminExpandedUsers[username] ? '' : 'none';
    }
    var _adminLoading = false;
    async function loadAdminUsers() {
      if (_adminLoading) return;
      _adminLoading = true;
      try {
        var resp = await fetch('/api/saas/admin/users');
        if (resp.status === 403) {
          if (!_adminLoaded) { document.getElementById('admin-section').style.display = 'none'; document.getElementById('hub-banner-section').style.display = 'none'; document.getElementById('btn-send-banner-top').style.display = 'none'; }
          return;
        }
        _adminLoaded = true;
        document.getElementById('admin-section').style.display = '';
        /* The section is display:none until the admin check passes, so this is
           the first point at which the persisted collapse state can be pushed
           onto real DOM. Idempotent, so running it on every poll is fine. */
        applyAdminUsersCollapsed();
        document.getElementById('hub-banner-section').style.display = '';
        document.getElementById('btn-send-banner-top').style.display = '';
        loadActiveBanner();
        var data = await resp.json();
        _allUsers = data.users || [];
        try { applySortUsers(); } catch(re) { console.error('renderUsers error:', re); }
      } catch(e) {
        if (!_adminLoaded) document.getElementById('admin-section').style.display = 'none';
      } finally {
        _adminLoading = false;
      }
    }

    function filterUsers() {
      applySortUsers();
    }

    // --- Admin Users: contact / CRM fields -------------------------------
    // Full name, Slack ID and notes are admin-maintained free text on the user
    // record. They are edited in a per-user panel row rather than inline in the
    // main row: notes is expected to be the longest field and a multi-line box
    // simply does not fit in a table cell next to quota and the action buttons.
    // The main row shows a one-line summary plus an Edit toggle, so the common
    // case (glance at who someone is) costs nothing and the edit case is one
    // click away.

    // Number of columns in the admin users table. Panel/expand rows span the
    // full width, so this must track the <th> count in renderUsers.
    var USERS_TABLE_COLSPAN = 8;
    // Rows of the notes textarea. Big enough for a short paragraph without
    // pushing the next user off screen. It is the only free-form prose field,
    // so it gets the height as well as the width; still user-resizable.
    var CONTACT_NOTES_ROWS = 4;
    // Characters of a note shown in the collapsed summary before ellipsis.
    var CONTACT_NOTES_PREVIEW_CHARS = 40;
    // Client-side maxlength on each field. Mirrors the server caps in saas.go
    // (maxContactNameLen / maxContactSlackIDLen / maxContactNotesLen) — the
    // server still truncates; this is only so the admin sees the limit.
    var CONTACT_MAX_NAME = 128;
    var CONTACT_MAX_SLACK = 64;
    var CONTACT_MAX_NOTES = 8192;

    // Field widths in the contact editor panel. The panel is its own full-width
    // row spanning every column (USERS_TABLE_COLSPAN), so widening these does
    // NOT add horizontal pressure to the already-dense main table — the fields
    // only compete with each other for the row's width.
    //
    // Expressed as flex "grow basis" pairs in percent so the row reflows with
    // the viewport instead of locking to pixel widths. The three bases sum to
    // less than 100% so the gap between fields has room; the grow factors then
    // divide the remainder, giving Notes the lion's share because it is
    // free-form prose while the other two are short identifiers.
    var CONTACT_W_NAME_BASIS = '22%';
    var CONTACT_W_NAME_MIN = '190px';
    var CONTACT_W_SLACK_BASIS = '18%';
    var CONTACT_W_SLACK_MIN = '150px';
    // Notes grows ~3x faster than the single-line fields and starts widest.
    var CONTACT_W_NOTES_BASIS = '48%';
    var CONTACT_W_NOTES_MIN = '320px';
    var CONTACT_NOTES_GROW = 3;

    // Which users currently have their contact panel open, keyed by username.
    // Re-rendering on the admin poll must not slam a panel shut mid-edit.
    var _contactExpandedUsers = {};

    // --- Protecting in-progress contact edits from the admin poll ---------
    // renderUsers() rebuilds the whole users table via innerHTML, which
    // destroys and recreates every contact input. The contact fields save on
    // blur, so a value that has been typed but not yet blurred lives ONLY in
    // the DOM node the re-render is about to throw away — losing it is real
    // data loss, not a cosmetic reflow.
    //
    // Approach: defer the render while an edit is in progress, rather than
    // trying to restore focus/caret/selection afterwards. Caret restoration
    // across an innerHTML swap is fiddly and fails in exactly the cases that
    // matter (IME composition, native undo history, a textarea scrolled
    // mid-note), and it cannot restore the browser's undo stack at all. The
    // users table is admin-only, low-churn data; postponing a refresh for a
    // few seconds while someone types costs nothing.
    //
    // "In progress" deliberately means anywhere in the table, not just the
    // focused node: an admin who typed a name, tabbed to Slack ID and is
    // still typing has an unsaved name too, and yanking the row out from
    // under them loses it just the same.

    // How long after the last contact keystroke/blur the table is still
    // considered "being edited". Long enough to cover thinking-pauses while
    // composing a note, short enough that the table is not stale for long
    // after the admin walks away mid-edit.
    var CONTACT_EDIT_QUIET_MS = 5000;
    // How often a deferred render re-checks whether editing has finished.
    // Also the ceiling on how late a deferred render can be beyond the quiet
    // window above.
    var CONTACT_DEFER_RECHECK_MS = 1000;

    // Timestamp of the most recent contact-field interaction, and the handle
    // of the pending re-check timer (null when no render is deferred).
    var _contactLastEditAt = 0;
    var _contactDeferTimer = null;
    // Set while a deferred render is waiting. The poll keeps updating _allUsers
    // regardless; this only gates the DOM write, so the deferred render always
    // paints the newest data rather than a stale snapshot.
    var _contactRenderPending = false;

    // Per-field dirty values: what the admin has typed but not yet saved,
    // keyed by "username::field". Survives a render so that if one does slip
    // through, the typed text is re-applied rather than lost. Multiple users'
    // fields can be dirty at once (type a note, click into another user's
    // Slack ID without blurring back), so this is a map, not a single slot.
    var _contactDirty = {};

    function contactDirtyKey(username, field) { return username + '::' + field; }

    // markContactEditing records activity so the poll backs off.
    function markContactEditing() { _contactLastEditAt = Date.now(); }

    // contactEditInProgress is true when the table must not be rebuilt:
    // either a contact control currently has focus, or something is typed and
    // unsaved, or a keystroke landed within the quiet window.
    function contactEditInProgress() {
      var container = document.getElementById('users-container');
      if (container) {
        var active = document.activeElement;
        if (active && active.getAttribute && active.getAttribute('data-contact-field') &&
            container.contains(active)) {
          return true;
        }
      }
      for (var k in _contactDirty) {
        if (Object.prototype.hasOwnProperty.call(_contactDirty, k)) return true;
      }
      return (Date.now() - _contactLastEditAt) < CONTACT_EDIT_QUIET_MS;
    }

    // contactPanelId maps a username to a DOM id. Usernames are GitHub logins
    // (alphanumerics and hyphens) but this is defensive: anything outside that
    // set is replaced so a stored username can never inject markup through an
    // id attribute or break querySelector.
    function contactPanelId(username) {
      return 'contact-panel-' + String(username || '').replace(/[^A-Za-z0-9_-]/g, '_');
    }

    // renderContactCell is the collapsed summary shown in the main user row.
    function renderContactCell(u) {
      var bits = [];
      if (u.full_name) bits.push('<span style="font-size:0.78rem">' + esc(u.full_name) + '</span>');
      if (u.slack_id) bits.push('<span style="font-size:0.7rem;color:var(--muted)">slack: ' + esc(u.slack_id) + '</span>');
      if (u.notes) {
        var preview = u.notes.length > CONTACT_NOTES_PREVIEW_CHARS
          ? u.notes.substring(0, CONTACT_NOTES_PREVIEW_CHARS) + '…' : u.notes;
        // title= carries the full note on hover, so it needs attribute escaping.
        bits.push('<span title="' + escAttr(u.notes) + '" style="font-size:0.7rem;color:var(--muted);font-style:italic">' + esc(preview) + '</span>');
      }
      // align-items:flex-start + text-align:left keep the contact lines
      // left-justified. Without them the cell inherits the users table's
      // centred alignment, so name/slack/notes rendered ragged-centre — each
      // line a different width, which is hard to scan down a column.
      var summary = bits.length
        ? '<div style="display:flex;flex-direction:column;align-items:flex-start;text-align:left;gap:1px;max-width:220px;overflow:hidden">' + bits.join('') + '</div>'
        : '<span style="color:var(--muted);font-size:0.72rem">—</span>';
      var open = !!(_contactExpandedUsers && _contactExpandedUsers[u.github_username]);
      var btn = '<button type="button" data-contact-toggle="' + escAttr(u.github_username) + '"' +
        ' id="' + contactToggleId(u.github_username) + '"' +
        ' aria-controls="' + contactPanelId(u.github_username) + '"' +
        ' aria-expanded="' + (open ? 'true' : 'false') + '"' +
        ' aria-label="' + escAttr(contactToggleAriaLabel(u.github_username, open)) + '"' +
        ' style="' + contactToggleStyle(open) + '">' + contactToggleLabel(open) + '</button>';
      return summary + btn;
    }

    /* --- The Edit/Save toggle ---
       The button in the CONTACT column is the operator's primary affordance:
       "Edit" while the panel is shut, "Save" while it is open. Clicking Save
       commits every pending field and closes the panel.

       Save is styled as the primary action (accent border and text) so the eye
       lands on it; the × and the Close button inside the panel remain the
       escape hatches, and Escape still works. Making the row button the commit
       is what gives an explicit save point WITHOUT fighting the per-field blur
       saves: blur keeps storing silently as the admin tabs around, and this
       button is the only thing that closes on success. */

    /* Style is derived from state in one place so the initial markup and the
       live in-place update below can never drift apart. */
    function contactToggleStyle(open) {
      var color = open ? 'var(--accent)' : 'var(--muted)';
      return 'margin-top:2px;padding:1px 7px;background:none;border:1px solid ' + color +
        ';border-radius:4px;color:' + color + ';cursor:pointer;font-size:0.65rem' +
        (open ? ';font-weight:600' : '');
    }

    function contactToggleLabel(open) { return open ? 'Save' : 'Edit'; }

    /* The accessible name carries the username as well as the verb, because a
       screen-reader user tabbing a table of ~90 identical "Save" buttons needs
       to know which row they are on. */
    function contactToggleAriaLabel(username, open) {
      return (open ? 'Save contact details for ' : 'Edit contact details for ') + String(username || '');
    }

    function contactToggleId(username) {
      return 'contact-toggle-' + String(username || '').replace(/[^A-Za-z0-9_-]/g, '_');
    }

    /* refreshContactToggleLabel re-derives the button from _contactExpandedUsers
       and writes label, style, aria-expanded and the accessible name TOGETHER.
       Updating the visible text without the aria state would leave a screen
       reader announcing "Edit, collapsed" over an open editor. */
    function refreshContactToggleLabel(username) {
      if (!username) return;
      var btn = document.getElementById(contactToggleId(username));
      if (!btn) return;
      var open = !!(_contactExpandedUsers && _contactExpandedUsers[username]);
      btn.textContent = contactToggleLabel(open);
      btn.setAttribute('aria-expanded', open ? 'true' : 'false');
      btn.setAttribute('aria-label', contactToggleAriaLabel(username, open));
      btn.setAttribute('style', contactToggleStyle(open));
    }

    // renderContactPanelRow is the expandable editor row that follows each user
    // row. It is always emitted (hidden unless expanded) so toggling is a
    // display flip rather than a re-render, which keeps focus and caret intact.
    function renderContactPanelRow(u) {
      var open = !!(_contactExpandedUsers && _contactExpandedUsers[u.github_username]);
      var fld = 'padding:5px 7px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--text);font-size:0.78rem;box-sizing:border-box;width:100%';
      var lbl = 'display:block;font-size:0.66rem;color:var(--muted);margin-bottom:3px;text-transform:uppercase;letter-spacing:0.04em';
      var user = escAttr(u.github_username);
      return '<tr id="' + contactPanelId(u.github_username) + '" style="display:' + (open ? '' : 'none') + '">' +
        /* position:relative anchors the absolutely-positioned × below. */
        '<td class="contact-panel-cell" colspan="' + USERS_TABLE_COLSPAN + '" style="background:var(--surface);position:relative">' +
        '<div style="padding:10px 14px 12px 40px;display:flex;gap:14px;flex-wrap:wrap;align-items:flex-start">' +
          '<div style="flex:1 1 ' + CONTACT_W_NAME_BASIS + ';min-width:' + CONTACT_W_NAME_MIN + '">' +
            '<label style="' + lbl + '">Full name</label>' +
            '<input type="text" data-contact-user="' + user + '" data-contact-field="full_name"' +
              ' maxlength="' + CONTACT_MAX_NAME + '" value="' + escAttr(u.full_name || '') + '" style="' + fld + '">' +
          '</div>' +
          '<div style="flex:1 1 ' + CONTACT_W_SLACK_BASIS + ';min-width:' + CONTACT_W_SLACK_MIN + '">' +
            '<label style="' + lbl + '">Slack ID</label>' +
            '<input type="text" data-contact-user="' + user + '" data-contact-field="slack_id"' +
              ' maxlength="' + CONTACT_MAX_SLACK + '" value="' + escAttr(u.slack_id || '') + '" style="' + fld + '">' +
          '</div>' +
          '<div style="flex:' + CONTACT_NOTES_GROW + ' 1 ' + CONTACT_W_NOTES_BASIS + ';min-width:' + CONTACT_W_NOTES_MIN + '">' +
            '<label style="' + lbl + '">Notes</label>' +
            '<textarea data-contact-user="' + user + '" data-contact-field="notes" rows="' + CONTACT_NOTES_ROWS + '"' +
              ' maxlength="' + CONTACT_MAX_NOTES + '" style="' + fld + ';resize:vertical;font-family:inherit">' + esc(u.notes || '') + '</textarea>' +
          '</div>' +
          '<div style="align-self:flex-end;display:flex;align-items:center;gap:10px;padding-bottom:4px">' +
            '<span style="font-size:0.65rem;color:var(--muted)">Save closes &middot; Esc cancels</span>' +
            '<button type="button" data-contact-close="' + user + '"' +
              ' style="padding:4px 12px;background:var(--bg);border:1px solid var(--border);border-radius:4px;' +
              'color:var(--muted);cursor:pointer;font-size:0.7rem">Cancel</button>' +
          '</div>' +
        '</div>' +
        /* The × sits top-right of the panel, the conventional place to look for
           a dismiss control, and duplicates the Close button so the affordance
           is visible without reading to the end of a wide row. */
        '<button type="button" data-contact-close="' + user + '" aria-label="Close editor for ' + user + '"' +
          ' style="position:absolute;top:6px;right:10px;background:none;border:none;color:var(--muted);' +
          'cursor:pointer;font-size:1rem;line-height:1;padding:2px 6px">&#10005;</button>' +
        '</td></tr>';
    }

    /* contactPanelHasUnsavedEdits reports whether anything in this user's panel
       has been typed but not yet handed to the save path. The fields save on
       blur, so text that has never been blurred lives ONLY in the DOM node —
       closing the panel without a warning would be real data loss. */
    function contactPanelHasUnsavedEdits(username) {
      if (!username) return false;
      for (var k in _contactDirty) {
        if (Object.prototype.hasOwnProperty.call(_contactDirty, k) &&
            k.indexOf(username + '::') === 0) {
          return true;
        }
      }
      return false;
    }

    /* closeContactPanel is the single exit for the editor: the ×, the Close
       button, Escape, and a successful save all funnel through here.

       Unsaved text is committed rather than discarded when the admin confirms:
       blur normally saves, but Escape and the × can fire while a field still
       has focus and has never blurred, so the value would otherwise be lost. */
    async function closeContactPanel(username, opts) {
      if (!username) return;
      var force = !!(opts && opts.force);
      if (!force && contactPanelHasUnsavedEdits(username)) {
        var keep = await hiveConfirm('You have unsaved changes for ' + username +
          '. Save them and close?');
        if (!keep) return;
        /* Save first, and close only if the save actually landed. A failed
           save keeps the editor open with the typed text still in the fields
           so the admin can read the toast, fix the cause and retry — closing
           here would discard the very edit the server just rejected. */
        var saved = await commitContactPanelEdits(username);
        if (!saved) return;
      }
      setContactPanelOpen(username, false);
    }

    /* commitContactPanelEdits flushes every dirty field for one user through
       the normal save path and resolves true only when every one succeeded.
       saveContactField is a no-op for unchanged values, so this is safe to
       call broadly. Dirty marks are cleared only for fields that saved, so a
       rejected field stays dirty and the poll keeps backing off rather than
       rebuilding the table over text the admin has not managed to store. */
    async function commitContactPanelEdits(username) {
      var prefix = username + '::';
      var keys = Object.keys(_contactDirty || {}).filter(function(k) {
        return k.indexOf(prefix) === 0;
      });
      /* Also flush whatever is currently in the panel's inputs. The commit can
         be triggered by a click on Save while a field still holds focus and has
         never blurred, so its text may not be in _contactDirty yet. */
      var panel = document.getElementById(contactPanelId(username));
      if (panel) {
        var live = panel.querySelectorAll('[data-contact-field]');
        Array.prototype.forEach.call(live || [], function(el) {
          var f = el.getAttribute('data-contact-field');
          if (!f) return;
          var k = prefix + f;
          _contactDirty[k] = el.value;
          if (keys.indexOf(k) < 0) keys.push(k);
        });
      }
      /* Saves are sequential, not Promise.all: the handler is a read-modify-write
         over one JSON file per user, so three concurrent PUTs for the same user
         can interleave and lose a field. */
      var allOK = true;
      for (var i = 0; i < (keys || []).length; i++) {
        var k = keys[i];
        var field = k.substring(prefix.length);
        /* Not silent: this IS the explicit commit, so a failure here is exactly
           what the admin needs to see. */
        var ok = await saveContactField(username, field, _contactDirty[k]);
        if (ok) { delete _contactDirty[k]; } else { allOK = false; }
      }
      /* A field can be clean here yet still have failed earlier during a silent
         blur-save. Surface that recorded reason rather than closing on what
         would look like a no-op success. */
      if (allOK && _contactLastError[username]) {
        hiveToast('Could not save ' + username + ': ' + _contactLastError[username], 'error');
        return false;
      }
      return allOK;
    }

    /* openContactPanelUsername returns the username of the contact editor that
       currently owns focus, or '' when focus is elsewhere. Escape must only
       close the editor the admin is actually working in — a stray Escape with
       focus on the page body should fall through to the global handler that
       closes create-modal and friends (#2321). */
    function openContactPanelUsername() {
      var active = document.activeElement;
      if (!active || !active.closest) return '';
      var cell = active.closest('.contact-panel-cell');
      if (!cell) return '';
      var field = cell.querySelector('[data-contact-user]');
      return field ? (field.getAttribute('data-contact-user') || '') : '';
    }

    /* Escape-to-close for the contact editor. Registered in the CAPTURE phase
       so it can decide before the global Escape handler (#2321) runs, and it
       stops propagation ONLY when it actually closed an editor — otherwise the
       global handler keeps its existing behaviour for create-modal, the
       request modal, the confirm overlay and the timeline/access modals. */
    document.addEventListener('keydown', function(e) {
      if (e.key !== 'Escape') return;
      /* hiveConfirm puts a modal overlay on top and binds its own Escape; while
         one is up it owns the key, so never steal it. */
      if (document.querySelector('.hive-confirm-overlay')) return;
      var username = openContactPanelUsername();
      if (!username) return;
      e.stopPropagation();
      e.preventDefault();
      closeContactPanel(username);
    }, true);

    // bindContactPanels attaches listeners after renderUsers writes the table.
    // Listeners rather than inline on* attributes: esc() leaves apostrophes
    // alone, so a value like O'Brien inside onchange="save('...')" would break
    // the handler (and be an injection vector). Nothing user-controlled is ever
    // interpolated into executable markup here.
    function bindContactPanels() {
      var container = document.getElementById('users-container');
      if (!container) return;
      var toggles = container.querySelectorAll('[data-contact-toggle]');
      Array.prototype.forEach.call(toggles || [], function(btn) {
        btn.addEventListener('click', function() {
          toggleContactPanel(btn.getAttribute('data-contact-toggle'));
        });
      });
      /* The × and the Close button share data-contact-close, so one binding
         covers both. Bound here rather than as inline on* attributes for the
         same reason as the toggles: nothing user-controlled is interpolated
         into executable markup. */
      var closers = container.querySelectorAll('[data-contact-close]');
      Array.prototype.forEach.call(closers || [], function(btn) {
        btn.addEventListener('click', function() {
          closeContactPanel(btn.getAttribute('data-contact-close'));
        });
      });
      var fields = container.querySelectorAll('[data-contact-field]');
      Array.prototype.forEach.call(fields || [], function(el) {
        var user = el.getAttribute('data-contact-user');
        var field = el.getAttribute('data-contact-field');
        var key = contactDirtyKey(user, field);
        // Re-apply anything typed but not yet saved. Belt-and-braces: the
        // render gate should mean we never rebuild mid-edit, but if a render
        // does land (e.g. one already in flight when typing began), the typed
        // text is restored instead of silently reverting to the server value.
        if (Object.prototype.hasOwnProperty.call(_contactDirty, key)) {
          el.value = _contactDirty[key];
        }
        // Any keystroke marks the table as being edited and records the
        // in-progress value, so the poll backs off and nothing typed is only
        // ever held in a DOM node that is about to be replaced.
        el.addEventListener('input', function() {
          markContactEditing();
          _contactDirty[key] = el.value;
        });
        // focus/blur also count as activity: tabbing between fields must not
        // leave a gap the poll can render into.
        el.addEventListener('focus', markContactEditing);
        // Save on blur so an admin can tab between fields and type freely
        // without a request per keystroke.
        //
        // Blur-saves are SILENT: no toast on success, and on failure the value
        // stays dirty so the field keeps the typed text and the Save button
        // keeps reading "Save". Blur fires constantly (tabbing between the
        // three fields, clicking the Save button itself), so a toast per blur
        // would be noise, and a blur-triggered error would fire while the
        // admin is mid-sentence in the next field. The Save button is the
        // explicit commit point where failures are reported — see
        // commitContactPanelEdits.
        el.addEventListener('blur', function() {
          markContactEditing();
          // Clear dirty only once the save has actually landed, so there is no
          // window where the value is neither dirty nor stored. A rejected
          // blur-save therefore leaves the panel dirty and Save still armed.
          var pending = el.value;
          saveContactField(user, field, pending, {silent: true}).then(function(ok) {
            if (ok && _contactDirty[key] === pending) delete _contactDirty[key];
            refreshContactToggleLabel(user);
          });
        });
      });
    }

    /* The row button is stateful: "Edit" when shut, "Save" when open.

       Shut  -> open the panel (a plain display flip, which is what keeps focus
                and caret intact) and relabel to Save.
       Open  -> this IS the Save action: commit every pending field, and close
                only if the commit succeeded. A failure keeps the panel open,
                keeps the label on Save, and lets the error toast stand — with
                a visible Save button, a false success is the worst outcome, so
                nothing here closes on an unverified save.

       Note this deliberately does NOT go through closeContactPanel: that path
       is for the escape hatches (×, Close, Escape), where the right question is
       "you have unsaved changes, save them?". Pressing Save has already
       answered that question. */
    async function toggleContactPanel(username) {
      if (!username) return;
      if (_contactExpandedUsers[username]) {
        var saved = await commitContactPanelEdits(username);
        if (!saved) return;
        setContactPanelOpen(username, false);
        return;
      }
      setContactPanelOpen(username, true);
    }

    /* setContactPanelOpen is the ONLY place panel visibility changes, so the
       row/panel display and the button's label + aria state can never fall out
       of step with _contactExpandedUsers. */
    function setContactPanelOpen(username, open) {
      _contactExpandedUsers[username] = !!open;
      var row = document.getElementById(contactPanelId(username));
      if (row) row.style.display = open ? '' : 'none';
      refreshContactToggleLabel(username);
    }

    // Last value saved per user+field, so a blur that changed nothing (tabbing
    // through, or a re-render restoring focus) does not fire a pointless PUT.
    var _contactLastSaved = {};

    // Always returns a Promise<boolean> so callers can sequence on the outcome;
    // a no-op resolves true because "nothing to do" is not a failure.
    // opts.silent suppresses the failure toast, for blur-saves where the Save
    // button is the reporting point. The boolean result is unaffected.
    function saveContactField(username, field, value, opts) {
      if (!username || !field) return Promise.resolve(true);
      var key = username + '::' + field;
      var current = _allUsers ? (_allUsers.find(function(x) { return x.github_username === username; }) || {}) : {};
      var previous = (_contactLastSaved[key] !== undefined) ? _contactLastSaved[key] : (current[field] || '');
      var next = (value || '').trim();
      if (next === previous) return Promise.resolve(true);
      _contactLastSaved[key] = next;
      // Keep the in-memory copy in step so the next poll's signature check does
      // not treat our own edit as an external change and re-render mid-typing.
      if (current) current[field] = next;
      var payload = {};
      payload[field] = next;
      // On failure, roll the optimistic bookkeeping back to the previous value.
      // Without this the cache claims the value was stored, so the
      // next-equals-previous short-circuit above would skip the retry of the very
      // same value — the edit could never be saved again without a reload.
      return updateUser(username, payload, opts).then(function(ok) {
        if (!ok) {
          _contactLastSaved[key] = previous;
          if (current) current[field] = previous;
        }
        return ok;
      });
    }

    // scheduleDeferredUsersRender keeps re-checking until editing has stopped
    // and then renders. This is what guarantees a deferred refresh actually
    // happens: the timer re-arms itself on every check that still sees an edit
    // in progress, so the only way out of the loop is an actual render. It is
    // never cleared without rendering, and only one timer is ever in flight.
    function scheduleDeferredUsersRender() {
      _contactRenderPending = true;
      if (_contactDeferTimer) return;
      _contactDeferTimer = setTimeout(function() {
        _contactDeferTimer = null;
        if (contactEditInProgress()) { scheduleDeferredUsersRender(); return; }
        _contactRenderPending = false;
        // Re-derive from the latest _allUsers rather than replaying the
        // snapshot that was deferred, so the render that finally lands shows
        // current data and not whatever the poll saw minutes ago.
        try { applySortUsers(); } catch (e) { console.error('deferred renderUsers error:', e); }
      }, CONTACT_DEFER_RECHECK_MS);
    }

    function renderUsers(users, force) {
      var sig = JSON.stringify(users);
      if (!force && sig === _lastUsersJSON) return;
      // Never rebuild the table out from under an in-progress contact edit —
      // the inputs save on blur, so unblurred text exists only in the DOM.
      // The data is already in _allUsers; only the DOM write is postponed.
      if (contactEditInProgress()) { scheduleDeferredUsersRender(); return; }
      _lastUsersJSON = sig;
      if (!users.length) { document.getElementById('users-container').innerHTML = '<div class="loading">No users found</div>'; return; }
      var rows = users.map(function(u) {
        var blocked = u.blocked ? '<span style="color:var(--red);font-weight:600">BLOCKED</span>' : '<span style="color:var(--green)">active</span>';
        var avatar = linkedAvatar(u.github_username, TABLE_AVATAR_PX,
          u.github_username + ' — GitHub profile', 'margin-right:6px');
        var isAdmin = u.github_username === 'clubanderson';
        var hivesObj = u.hives || {};
        var registryIds = new Set((_hiveRegistry || []).map(function(h) { return h.id; }));
        var hiveIds = Object.keys(hivesObj).filter(function(hid) { return registryIds.has(hid); });
        var hiveCount = hiveIds.length;
        var expandId = 'expand-' + esc(u.github_username);
        var isExpanded = _adminExpandedUsers && _adminExpandedUsers[u.github_username];

        var hiveRows = '';
        if (hiveCount > 0) {
          hiveRows = '<tr id="' + expandId + '" style="display:' + (isExpanded ? '' : 'none') + '"><td colspan="' + USERS_TABLE_COLSPAN + '"><div style="padding:8px 12px 8px 40px;font-size:0.75rem">';
          hiveRows += '<table style="width:100%;border-collapse:collapse"><thead><tr style="color:var(--muted);font-size:0.7rem"><th style="text-align:left;padding:4px 8px">Hive</th><th>Role</th><th>Type</th><th>Link</th></tr></thead><tbody>';
          hiveIds.forEach(function(hid) {
            var role = hivesObj[hid];
            var isHosted = hid.startsWith('hosted-') || hid.startsWith('saas-');
            var regEntry = (_hiveRegistry || []).find(function(h) { return h.id === hid; });
            var hiveName = regEntry ? (regEntry.name || hid) : hid;
            // Prefer the hive's heartbeat-reported dashboard URL so firewalled
            // spokes (vllm-d etc.) link to their real route, not a dead
            // <id>.hive.kubestellar.io host. Fall back to the hive-oke pattern.
            var linkBase = (regEntry && regEntry.dashboardUrl && !regEntry.dashboardUrl.includes('localhost'))
              ? regEntry.dashboardUrl : (isHosted ? 'https://' + esc(hid) + '.hive.kubestellar.io' : '');
            var linkLabel = linkBase.replace(/^https?:\/\//, '');
            var link = linkBase ? '<a href="' + esc(linkBase) + '" target="_blank" class="dash-link">' + esc(linkLabel) + '</a>' : '<span style="color:var(--muted)">local</span>';
            var typeBadge = isHosted ? '<span style="color:#60a5fa">hosted</span>' : '<span style="color:#9ca3af">local</span>';
            hiveRows += '<tr><td style="padding:4px 8px">' + esc(hiveName) + '</td><td style="text-align:center">' + esc(role) + '</td><td style="text-align:center">' + typeBadge + '</td><td>' + link + '</td></tr>';
          });
          hiveRows += '</tbody></table></div></td></tr>';
        }

        /* Clicking the name opens the assign flow for this user (admin-only, and
           the underlying endpoints re-check that server-side). The GitHub
           profile is reachable via the avatar, so one click is not overloaded
           with two destinations. The separate ↗ glyph that used to sit between
           them is gone: it went to the same profile the face now links to, and
           a 0.65rem arrow was a far smaller click target than the picture. A
           pending provision request is called out because the click then
           approves it rather than assigning a bare placeholder. */
        var hasPendingReq = !!_provisionRequestsByUser[u.github_username];
        var nameCell = avatar +
          '<a href="#" onclick="openAssignForUser(\'' + esc(u.github_username) + '\');return false" ' +
          'title="' + (hasPendingReq ? 'Approve this user&#39;s pending request and assign a hive' : 'Assign an available hive to this user') + '" ' +
          'style="color:var(--blue);cursor:pointer">' + esc(u.github_username) + '</a>' +
          (hasPendingReq ? ' <span style="color:var(--accent);font-size:0.65rem" title="Has a pending provision request">&#9679; request</span>' : '');
        return '<tr>' +
          '<td>' + nameCell + (isAdmin ? ' <span style="color:var(--accent);font-size:0.7rem">admin</span>' : '') + '</td>' +
          '<td style="font-size:0.75rem;color:var(--muted)">' + esc(fmtUserTS(u.created_at)) + '</td>' +
          '<td style="font-size:0.75rem;color:var(--muted)">' + esc(fmtUserTS(u.last_login)) + '</td>' +
          '<td style="text-align:left">' + renderContactCell(u) + '</td>' +
          '<td>' + blocked + '</td>' +
          '<td><input type="number" min="0" max="10" value="' + (u.saas_quota || 0) + '" style="width:50px;padding:4px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--text);text-align:center" onchange="updateUser(\'' + esc(u.github_username) + '\',{saas_quota:parseInt(this.value)||0})"></td>' +
          '<td>' + (hiveCount > 0 ? '<a href="#" onclick="toggleAdminExpand(\'' + esc(u.github_username) + '\');return false" style="color:var(--blue);font-size:0.8rem">' + hiveCount + ' hive' + (hiveCount > 1 ? 's' : '') + '</a>' : '<span style="color:var(--muted)">0</span>') + '</td>' +
          '<td>' + (isAdmin ? '' : '<button onclick="updateUser(\'' + esc(u.github_username) + '\',{blocked:' + (!u.blocked) + '})" style="padding:3px 10px;background:' + (u.blocked ? 'var(--green)' : 'var(--amber)') + ';color:' + (u.blocked ? '#fff' : '#1a1a1a') + ';border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">' + (u.blocked ? 'Unblock' : 'Block') + '</button> <button onclick="deleteUser(\'' + esc(u.github_username) + '\',' + hiveCount + ')" style="padding:3px 10px;background:#b02a2a;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">Delete</button>') + '</td>' +
          '</tr>' + renderContactPanelRow(u) + hiveRows;
      }).join('');
      document.getElementById('users-container').innerHTML =
        '<table class="hive-table"><thead><tr>' +
        '<th onclick="sortUsers(\'github_username\')" style="cursor:pointer">User ⇅</th><th onclick="sortUsers(\'created_at\')" style="cursor:pointer">Joined ⇅</th><th onclick="sortUsers(\'last_login\')" style="cursor:pointer">Last Login ⇅</th><th onclick="sortUsers(\'full_name\')" style="cursor:pointer">Contact ⇅</th><th onclick="sortUsers(\'status\')" style="cursor:pointer">Status ⇅</th><th onclick="sortUsers(\'saas_quota\')" style="cursor:pointer">Quota ⇅</th><th onclick="sortUsers(\'hiveCount\')" style="cursor:pointer">Hives ⇅</th><th>Actions</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table>';
      // The contact panels are built as raw HTML above; wire their listeners
      // once the rows are actually in the DOM. Inline on* handlers are avoided
      // for these fields because esc() does not escape apostrophes, so a name
      // or note containing one would break out of an inline handler string.
      bindContactPanels();
    }

    /* updateUser previously ignored the response entirely: a 404/400/500 was
       indistinguishable from success, so a rejected edit looked like it had
       been saved until the next poll quietly reverted the field. It now checks
       resp.ok and reports the server's reason, and returns a boolean so
       callers can decide whether to close the editor.

       readErrorMessage is used rather than a bare resp.json(): the handler
       writes errors with writeJSONError so the body IS JSON, but a proxy or
       middleware fault can still return HTML/plain text, and resp.json() on
       that throws — which is exactly how #2348's handleAlertAck turned a real
       cause into a generic "failed". */
    async function readErrorMessage(resp, fallback) {
      try {
        var text = await resp.text();
        if (text) {
          try {
            var parsed = JSON.parse(text);
            if (parsed && parsed.error) return String(parsed.error);
          } catch(je) {
            /* Not JSON — show the raw text, which is still more useful than
               a bare status code. */
            return text.trim().substring(0, ERROR_TEXT_MAX_CHARS);
          }
        }
      } catch(e) {}
      return fallback + ' (HTTP ' + resp.status + ')';
    }

    /* Longest slice of a non-JSON error body shown in a toast. Long enough for
       a real proxy/server message, short enough not to fill the screen. */
    var ERROR_TEXT_MAX_CHARS = 200;

    /* Reason for the most recent failed save, per user. A silent blur-save
       records the cause here instead of toasting it, so the Save button can
       report the REAL reason rather than a generic "failed" when the admin
       finally commits. Cleared on any success for that user. */
    var _contactLastError = {};

    /* opts.silent suppresses the toast (blur-saves); the failure is still
       recorded in _contactLastError and still returns false. */
    async function updateUser(username, updates, opts) {
      var silent = !!(opts && opts.silent);
      function fail(reason) {
        _contactLastError[username] = reason;
        if (!silent) hiveToast('Could not save ' + username + ': ' + reason, 'error');
        return false;
      }
      try {
        var resp = await fetch('/api/saas/admin/users/' + encodeURIComponent(username), {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(updates)
        });
        if (!resp.ok) {
          return fail(await readErrorMessage(resp, 'update failed'));
        }
        delete _contactLastError[username];
        loadAdminUsers();
        return true;
      } catch(e) {
        return fail(e.message);
      }
    }

    async function deleteUser(username, hiveCount) {
      if (hiveCount > 0) {
        hiveToast('Cannot delete ' + username + ' — they still own ' + hiveCount + ' hive(s). Delete or reassign those first.', 'error');
        return;
      }
      if (!await hiveConfirm('Delete hub user ' + username + '? This removes their hub account record (login, quota, saved token). It does not touch GitHub.')) return;
      try {
        var resp = await fetch('/api/saas/admin/users/' + encodeURIComponent(username), { method: 'DELETE' });
        if (!resp.ok) {
          var e = await resp.json().catch(function(){ return {}; });
          hiveToast('Delete failed: ' + (e.error || resp.status), 'error');
          return;
        }
        hiveToast('Deleted ' + username, 'success');
        loadAdminUsers();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function deleteHive(id) {
      if (!await hiveConfirm('Delete ' + id + '? This removes the namespace, PV, OCI storage, and all data.')) return;
      var btns = document.querySelectorAll('button[onclick*="deleteHive"]');
      btns.forEach(function(b) { b.disabled = true; b.textContent = 'Deleting...'; b.style.opacity = '0.6'; });
      try {
        gtag('event','hive_deleted',{hive_id:id});
        hiveToast('Deleting ' + id + '...', 'info');
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(id), {method: 'DELETE'});
        if (!resp.ok) {
          var d = await resp.json().catch(function(){ return {}; });
          hiveToast(d.error || 'Delete failed', 'error');
          // Refresh regardless: the hub removes the registry entry even when
          // teardown fails, so the listing may already be correct.
          loadHives();
          return;
        }
        var ok = await resp.json().catch(function(){ return {}; });
        if (ok.status === 'partially_deleted') {
          hiveToast('Removed ' + id + ' from the hub, but cleanup was incomplete: ' + (ok.warning || 'cloud resources may need manual removal'), 'error');
        } else {
          hiveToast('Deleted ' + id, 'success');
        }
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
      finally { btns.forEach(function(b) { b.disabled = false; b.textContent = 'Delete'; b.style.opacity = '1'; }); }
    }

    function toggleHiveMenu(menuId) {
      var menu = document.getElementById(menuId);
      var wasOpen = menu.style.display !== 'none';
      document.querySelectorAll('[id^="hive-menu-"]').forEach(function(m) { m.style.display = 'none'; });
      if (!wasOpen) menu.style.display = 'block';
    }
    document.addEventListener('click', function(e) {
      if (!e.target.closest('[id^="hive-menu-"]') && !e.target.closest('[onclick*="toggleHiveMenu"]')) {
        document.querySelectorAll('[id^="hive-menu-"]').forEach(function(m) { m.style.display = 'none'; });
      }
    });

    /* "View all activity" in the status hover panel. Delegated rather than an
       inline onclick because the hive name is user-controlled and esc() does
       not escape apostrophes — a name containing one would break out of an
       inline handler string. The values are read back from data attributes,
       where esc() IS sufficient. */
    document.addEventListener('click', function(e) {
      var btn = e.target.closest ? e.target.closest('.hover-view-timeline') : null;
      if (!btn) return;
      openTimelineModal(btn.getAttribute('data-hive-id') || '', btn.getAttribute('data-hive-name') || '');
    });

    function openMigrateModal(hiveId, currentClusterId) {
      var targets = (_clusterList || []).filter(function(c) { return c.id !== currentClusterId; });
      if (!targets.length) { hiveToast('No other clusters available', 'error'); return; }
      var currentName = (_clusterList || []).reduce(function(n, c) { return c.id === currentClusterId ? (c.name || c.id) : n; }, currentClusterId);
      var options = targets.map(function(c) {
        var caps = [];
        if (c.has_gpu) caps.push('GPU');
        if (c.arch) caps.push(c.arch);
        var label = c.name || c.id;
        if (caps.length) label += ' (' + caps.join(', ') + ')';
        return '<option value="' + esc(c.id) + '">' + esc(label) + '</option>';
      }).join('');
      var content = '<div style="margin-bottom:12px">Move <strong>' + esc(hiveId) + '</strong> from <strong>' + esc(currentName) + '</strong> to:</div>' +
        '<select id="migrate-target" style="width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;margin-bottom:12px">' + options + '</select>' +
        '<div style="padding:8px 12px;background:rgba(234,179,8,0.1);border:1px solid rgba(234,179,8,0.3);border-radius:6px;font-size:0.8rem;color:#eab308;margin-bottom:12px">The hive will be reprovisioned on the target cluster. This may take a few minutes. The hive will rebuild its state from GitHub.</div>' +
        '<div style="display:flex;gap:8px;justify-content:flex-end">' +
        '<button onclick="closeMigrateModal()" style="padding:6px 16px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;cursor:pointer">Cancel</button>' +
        '<button onclick="confirmMigrate(\'' + esc(hiveId) + '\')" style="padding:6px 16px;background:var(--accent);color:#000;border:none;border-radius:6px;cursor:pointer;font-weight:600">Move</button>' +
        '</div>';
      var overlay = document.createElement('div');
      overlay.id = 'migrate-overlay';
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:2000;display:flex;align-items:center;justify-content:center';
      overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:24px;max-width:420px;width:90%">' +
        '<h3 style="margin:0 0 16px 0;font-size:1rem">Move to cluster</h3>' + content + '</div>';
      overlay.addEventListener('click', function(e) { if (e.target === overlay) closeMigrateModal(); });
      document.body.appendChild(overlay);
    }
    function closeMigrateModal() {
      var ov = document.getElementById('migrate-overlay');
      if (ov) ov.remove();
    }
    async function confirmMigrate(hiveId) {
      var sel = document.getElementById('migrate-target');
      if (!sel) return;
      var targetId = sel.value;
      closeMigrateModal();
      hiveToast('Migrating ' + hiveId + '...', 'info');
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/migrate', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({target_cluster_id: targetId})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Migration failed', 'error'); return; }
        gtag('event', 'hive_migrate', {hive_id: hiveId, from: data.from, to: data.to});
        hiveToast('Migration started: ' + hiveId + ' moving to ' + targetId, 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    var ASSIGN_DEFAULT_ACMM = 2;
    /* openAssignModal claims one placeholder for a real project.
       prefillOwner (optional) pre-fills the Owner field — used when the flow is
       started by clicking a user in the admin users table.
       placeholders (optional) turns the fixed hive id into a dropdown, so that
       user-first entry point can retarget without backing out to the hive list.
       Both are omitted by the original ⋮ → "Assign / Claim" call site. */
    function openAssignModal(hiveId, prefillOwner, placeholders) {
      var h = (_allDashHives || []).reduce(function(m, x) { return x.id === hiveId ? x : m; }, null) || {};
      var fld = 'width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box';
      var lbl = 'display:block;font-size:0.75rem;color:var(--muted);margin:10px 0 4px';
      var acmmOpts = '';
      for (var lv = 0; lv <= 6; lv++) { acmmOpts += '<option value="' + lv + '"' + (lv === ASSIGN_DEFAULT_ACMM ? ' selected' : '') + '>' + lv + '</option>'; }
      var orgVal = esc(h.org || '');
      var reposVal = esc((h.repos || []).join(', '));
      /* With a placeholder list, let the admin retarget in place; without one,
         keep the original fixed-hive wording. */
      var hasPicker = !!(placeholders && placeholders.length);
      var hiveField;
      if (hasPicker) {
        var phOpts = (placeholders || []).map(function(p) {
          return '<option value="' + esc(p.id) + '"' + (p.id === hiveId ? ' selected' : '') + '>' +
            esc(p.id) + '  (' + esc(p.cluster_id || 'default') + ')</option>';
        }).join('');
        hiveField =
          '<div style="margin-bottom:4px;font-size:0.8rem;color:var(--muted)">Claim an available placeholder for a real project.</div>' +
          '<label style="' + lbl + '">Placeholder to assign *</label>' +
          '<select id="assign-hive-pick" style="' + fld + '">' + phOpts + '</select>';
      } else {
        hiveField = '<div style="margin-bottom:4px;font-size:0.8rem;color:var(--muted)">Claim placeholder <strong style="color:var(--fg)">' + esc(hiveId) + '</strong> for a real project.</div>';
      }
      /* The GitHub instance defaults to the SELECTED placeholder's cluster.
         With a picker the selection can change, so resolve the cluster of the
         initially-selected placeholder and re-resolve on change below. */
      function clusterIdForSelection(id) {
        var fromList = (placeholders || []).reduce(function(m, p) { return (p && p.id === id) ? p : m; }, null);
        if (fromList) return fromList.cluster_id || '';
        var fromDash = (_allDashHives || []).reduce(function(m, x) { return (x && x.id === id) ? x : m; }, null);
        return (fromDash && fromDash.clusterId) || '';
      }
      var assignClusterHost = clusterGitHubHost(clusterIdForSelection(hiveId));
      var content =
        hiveField +
        '<label style="' + lbl + '">Owner (GitHub login) *</label>' +
        '<input id="assign-owner" style="' + fld + '" value="' + esc(prefillOwner || '') + '" placeholder="octocat">' +
        '<label style="' + lbl + '">Org *</label>' +
        '<input id="assign-org" style="' + fld + '" value="' + orgVal + '" placeholder="my-org">' +
        githubHostChoiceMarkup('assign', assignClusterHost, '', fld, lbl) +
        '<label style="' + lbl + '">Repos * (comma-separated)</label>' +
        '<input id="assign-repos" style="' + fld + '" value="' + reposVal + '" placeholder="repo-a, repo-b">' +
        '<label style="' + lbl + '">Primary repo (optional, defaults to first)</label>' +
        '<input id="assign-primary" style="' + fld + '" placeholder="repo-a">' +
        '<label style="' + lbl + '">Project name (optional)</label>' +
        '<input id="assign-name" style="' + fld + '" placeholder="My Project">' +
        '<label style="' + lbl + '">ACMM level</label>' +
        '<select id="assign-acmm" style="' + fld + '">' + acmmOpts + '</select>' +
        '<label style="display:flex;align-items:center;gap:8px;margin:12px 0 4px;font-size:0.8rem;cursor:pointer"><input type="checkbox" id="assign-public"> Public (visible to everyone)</label>' +
        '<div onclick="toggleAssignAdvanced()" id="assign-adv-toggle" style="margin-top:12px;font-size:0.78rem;color:var(--accent);cursor:pointer;user-select:none">▸ GitHub App credentials (optional)</div>' +
        '<div id="assign-adv" style="display:none;margin-top:8px">' +
        '<div style="font-size:0.72rem;color:var(--muted);margin-bottom:8px">Optional — the owner can install the App from their dashboard after assignment.</div>' +
        '<label style="' + lbl + '">App ID</label>' +
        '<input id="assign-app-id" style="' + fld + '" placeholder="123456">' +
        '<label style="' + lbl + '">Installation ID</label>' +
        '<input id="assign-install-id" style="' + fld + '" placeholder="654321">' +
        '<label style="' + lbl + '">App private key (PEM)</label>' +
        '<textarea id="assign-app-key" rows="4" style="' + fld + ';font-family:monospace;font-size:0.7rem" placeholder="-----BEGIN RSA PRIVATE KEY-----"></textarea>' +
        '</div>' +
        '<div style="display:flex;gap:8px;justify-content:flex-end;margin-top:16px">' +
        '<button onclick="closeAssignModal()" style="padding:6px 16px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;cursor:pointer">Cancel</button>' +
        '<button id="assign-submit" onclick="confirmAssign(\'' + esc(hiveId) + '\')" style="padding:6px 16px;background:#3fb950;color:#000;border:none;border-radius:6px;cursor:pointer;font-weight:600">Assign</button>' +
        '</div>';
      var overlay = document.createElement('div');
      overlay.id = 'assign-overlay';
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:2000;display:flex;align-items:center;justify-content:center';
      overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:24px;max-width:440px;width:90%;max-height:88vh;overflow-y:auto">' +
        '<h3 style="margin:0 0 12px 0;font-size:1rem">Assign placeholder hive</h3>' + content + '</div>';
      overlay.addEventListener('click', function(e) { if (e.target === overlay) closeAssignModal(); });
      document.body.appendChild(overlay);
      wireGitHubHostChoice('assign', assignClusterHost);
      /* Retargeting the placeholder can change the cluster, and with it the
         default GitHub instance. Re-resolve so the note never claims the
         previous cluster's host. An explicit override the admin already made
         (public / a typed host) is left alone — only the "cluster default"
         label and note follow the selection. */
      var phPick = document.getElementById('assign-hive-pick');
      if (phPick) {
        phPick.addEventListener('change', function() {
          assignClusterHost = clusterGitHubHost(clusterIdForSelection(phPick.value));
          var sel = document.getElementById('assign-ghhost');
          if (sel && sel.options && sel.options.length) {
            sel.options[0].textContent = assignClusterHost
              ? 'Cluster default — ' + assignClusterHost
              : 'Cluster default';
          }
          wireGitHubHostChoice('assign', assignClusterHost);
        });
      }
    }
    function toggleAssignAdvanced() {
      var adv = document.getElementById('assign-adv');
      var tog = document.getElementById('assign-adv-toggle');
      if (!adv || !tog) return;
      var open = adv.style.display === 'none';
      adv.style.display = open ? 'block' : 'none';
      tog.textContent = (open ? '▾' : '▸') + ' GitHub App credentials (optional)';
    }
    function closeAssignModal() {
      var ov = document.getElementById('assign-overlay');
      if (ov) ov.remove();
    }
    async function confirmAssign(hiveId) {
      /* When the modal was opened with a placeholder picker, the dropdown — not
         the id baked into this onclick — is the authority on the target. */
      var pick = document.getElementById('assign-hive-pick');
      if (pick && pick.value) hiveId = pick.value;
      var owner = document.getElementById('assign-owner').value.trim();
      var org = document.getElementById('assign-org').value.trim();
      var repos = document.getElementById('assign-repos').value.trim();
      var primary = document.getElementById('assign-primary').value.trim();
      var name = document.getElementById('assign-name').value.trim();
      var acmm = parseInt(document.getElementById('assign-acmm').value, 10) || 0;
      var isPublic = document.getElementById('assign-public').checked;
      var appId = document.getElementById('assign-app-id').value.trim();
      var installId = document.getElementById('assign-install-id').value.trim();
      var appKey = document.getElementById('assign-app-key').value.trim();
      if (!owner) { hiveToast('Owner is required', 'error'); return; }
      if (!org) { hiveToast('Org is required', 'error'); return; }
      if (!repos) { hiveToast('Repos are required', 'error'); return; }
      var submit = document.getElementById('assign-submit');
      if (submit) { submit.disabled = true; submit.textContent = 'Assigning...'; }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(hiveId) + '/assign', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          /* github_host: '' = let the server backfill from the cluster,
             'public' = force public github.com, otherwise a GHE hostname. */
          body: JSON.stringify({owner: owner, org: org, github_host: githubHostChoiceValue('assign'), repos: repos, primary_repo: primary, project_name: name, acmm_level: acmm, is_public: isPublic, app_id: appId, installation_id: installId, app_private_key: appKey})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Assignment failed', 'error'); if (submit) { submit.disabled = false; submit.textContent = 'Assign'; } return; }
        closeAssignModal();
        hiveToast('Assigned ' + hiveId + ' to ' + owner, 'success');
        loadHives();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
        if (submit) { submit.disabled = false; submit.textContent = 'Assign'; }
      }
    }

    // ---- OpenRouter scan-to-fund (hub side) ---------------------------------
    // Fund a SPECIFIC hive: pick a default model, scan a QR (or open the link)
    // to authorize on OpenRouter. The hub exchanges the code for a scoped key and
    // delivers it to the hive as the "openrouter" gateway over the next heartbeat.
    var _orFundPoll = null;
    async function openOpenRouterFundModal(hiveId, hiveName) {
      var models = { suggested: [], models: [], default: '' };
      try {
        var r = await fetch('/api/openrouter/models');
        if (r.ok) models = await r.json();
      } catch (e) { /* fall back to manual entry */ }
      var opts = (models.suggested || []).map(function(m) {
        return '<option value="' + esc(m.id) + '">' + esc(m.label) + '</option>';
      }).join('') + '<option value="__manual__">Enter a model id manually…</option>';
      var fld = 'width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box';
      var content =
        '<div style="font-size:0.8rem;color:var(--muted);margin-bottom:10px">Fund <strong style="color:var(--fg)">' + esc(hiveName) + '</strong> with an OpenRouter key. Scan the QR (or open the link) to authorize on OpenRouter — the scoped key is delivered to the hive as its <code>openrouter</code> gateway.</div>' +
        '<label style="display:block;font-size:0.75rem;color:var(--muted);margin-bottom:4px">Default model</label>' +
        '<select id="orf-model" onchange="orfModelChange()" style="' + fld + ';margin-bottom:8px">' + opts + '</select>' +
        '<input id="orf-model-manual" placeholder="e.g. anthropic/claude-opus-4.8" style="display:none;' + fld + ';margin-bottom:8px">' +
        '<div style="display:flex;gap:8px;margin-top:4px">' +
        '<button onclick="orfStart(\'' + esc(hiveId) + '\')" style="padding:7px 16px;background:var(--accent,#58a6ff);color:#000;border:none;border-radius:6px;cursor:pointer;font-weight:600">Generate QR</button>' +
        '<button onclick="closeOrFundModal()" style="padding:7px 16px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;cursor:pointer">Close</button>' +
        '</div>' +
        '<div id="orf-qr" style="margin-top:12px"></div>' +
        '<div id="orf-status" style="margin-top:8px;font-size:0.78rem"></div>';
      var overlay = document.createElement('div');
      overlay.id = 'orf-overlay';
      overlay.style.cssText = 'position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:2000;display:flex;align-items:center;justify-content:center';
      overlay.innerHTML = '<div style="background:var(--bg);border:1px solid var(--border);border-radius:12px;padding:24px;max-width:460px;width:90%;max-height:88vh;overflow-y:auto">' +
        '<h3 style="margin:0 0 12px 0;font-size:1rem">⚡ Fund with OpenRouter</h3>' + content + '</div>';
      overlay.addEventListener('click', function(e) { if (e.target === overlay) closeOrFundModal(); });
      document.body.appendChild(overlay);
    }
    function orfModelChange() {
      var sel = document.getElementById('orf-model');
      var man = document.getElementById('orf-model-manual');
      if (sel && man) man.style.display = sel.value === '__manual__' ? 'block' : 'none';
    }
    function orfChosenModel() {
      var sel = document.getElementById('orf-model');
      if (!sel) return '';
      if (sel.value === '__manual__') {
        var man = document.getElementById('orf-model-manual');
        return (man && man.value.trim()) || '';
      }
      return sel.value;
    }
    async function orfStart(hiveId) {
      var model = orfChosenModel();
      var qr = document.getElementById('orf-qr');
      var status = document.getElementById('orf-status');
      if (qr) qr.innerHTML = '<span style="color:var(--muted);font-size:0.78rem">Preparing…</span>';
      try {
        var res = await fetch('/api/openrouter/connect/start?hive_id=' + encodeURIComponent(hiveId) + '&model=' + encodeURIComponent(model));
        if (!res.ok) { var e = await res.json().catch(function(){return{};}); throw new Error(e.error || ('HTTP ' + res.status)); }
        var data = await res.json();
        var authURL = data.authorize_url;
        var qrSrc = '/api/openrouter/qr?data=' + encodeURIComponent(authURL);
        if (qr) qr.innerHTML =
          '<div style="display:flex;gap:16px;align-items:center;flex-wrap:wrap">' +
          '<img src="' + esc(qrSrc) + '" alt="OpenRouter QR" width="180" height="180" style="border:6px solid #fff;border-radius:8px;background:#fff">' +
          '<div style="font-size:0.78rem"><div style="margin-bottom:6px">Scan with your phone, or on this device:</div>' +
          '<a href="' + esc(authURL) + '" target="_blank" rel="noopener" style="color:var(--accent,#58a6ff);font-weight:600">Open OpenRouter authorization ↗</a></div></div>';
        if (status) status.innerHTML = '<span style="color:var(--muted)">Waiting for authorization…</span>';
        orfStartPolling(hiveId);
      } catch (e) {
        if (qr) qr.innerHTML = '<span style="color:#f85149;font-size:0.78rem">Failed: ' + esc(e.message) + '</span>';
      }
    }
    function orfStartPolling(hiveId) {
      if (_orFundPoll) clearInterval(_orFundPoll);
      var ORF_POLL_MS = 4000;
      _orFundPoll = setInterval(function() { orfCheck(hiveId); }, ORF_POLL_MS);
    }
    async function orfCheck(hiveId) {
      var status = document.getElementById('orf-status');
      try {
        var res = await fetch('/api/openrouter/credit?hive_id=' + encodeURIComponent(hiveId));
        if (!res.ok) return;
        var d = await res.json();
        // pending_delivery flips true the moment the fund completes on the hub;
        // it then flips back to false once the hive drains it on a heartbeat.
        if (d.pending_delivery) {
          if (status) status.innerHTML = '<span style="color:var(--green,#3fb950);font-weight:600">✓ Funded — delivering to the hive on its next heartbeat…</span>';
        }
      } catch (e) { /* keep polling */ }
    }
    function closeOrFundModal() {
      if (_orFundPoll) { clearInterval(_orFundPoll); _orFundPoll = null; }
      var ov = document.getElementById('orf-overlay');
      if (ov) ov.remove();
    }

    async function removeLocalHive(id) {
      if (!await hiveConfirm('Remove ' + id + ' from the registry? The hive itself is not affected — it will reappear if it sends another heartbeat.')) return;
      try {
        var resp = await fetch('/api/hub/registry/' + encodeURIComponent(id), {method: 'DELETE'});
        if (!resp.ok) { var d = await resp.json(); hiveToast(d.error || 'Remove failed', 'error'); return; }
        hiveToast('Removed ' + id + ' from registry', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    function openConvert(btn) {
      document.getElementById('f-org').value = btn.dataset.org || '';
      document.getElementById('f-repos').value = btn.dataset.repos || '';
      document.getElementById('f-primary').value = btn.dataset.primary || '';
      document.getElementById('f-name').value = btn.dataset.name || '';
      document.getElementById('f-level').value = btn.dataset.level || '1';
      document.getElementById('create-modal').style.display = 'flex';
      var dashUrl = (btn.dataset.dashUrl || '').replace(/\/$/, '');
      var dlLink = document.getElementById('yaml-download-link');
      var dlHref = document.getElementById('yaml-download-href');
      if (dashUrl && dlLink && dlHref) {
        dlHref.href = dashUrl + '/api/config/download';
        dlLink.style.display = '';
      } else if (dlLink) {
        dlLink.style.display = 'none';
      }
    }

    var _createInProgress = false;
    async function createHive() {
      if (_createInProgress) return;
      _createInProgress = true;
      document.getElementById('btn-go').disabled = true;
      document.getElementById('btn-go').textContent = 'Provisioning...';
      var org = document.getElementById('f-org').value.trim();
      var repos = document.getElementById('f-repos').value.trim();
      var primary = document.getElementById('f-primary').value.trim();
      var name = document.getElementById('f-name').value.trim();
      var level = parseInt(document.getElementById('f-level').value) || 1;
      var clusterSel = document.getElementById('f-cluster');
      var clusterId = clusterSel ? clusterSel.value : '';
      var method = document.querySelector('input[name="auth-method"]:checked').value;
      var token = document.getElementById('f-token').value.trim();
      var appId = (document.getElementById('f-app-id') || {}).value || '';
      var installId = (document.getElementById('f-install-id') || {}).value || '';
      var appKey = (document.getElementById('f-app-key') || {}).value || '';

      gtag('event','hive_create_started',{org:org,primary_repo:primary,acmm_level:level,cluster_id:clusterId});
      if (!org || !repos) { hiveToast('Org and repos are required', 'error'); _createInProgress = false; document.getElementById('btn-go').disabled = false; document.getElementById('btn-go').textContent = 'Go'; return; }
      if (method === 'pat' && !token) { hiveToast('GitHub token is required', 'error'); _createInProgress = false; document.getElementById('btn-go').disabled = false; document.getElementById('btn-go').textContent = 'Go'; return; }
      if (method === 'app' && (!appId || !installId || !appKey)) { hiveToast('App ID, Installation ID, and Private Key are required', 'error'); _createInProgress = false; document.getElementById('btn-go').disabled = false; document.getElementById('btn-go').textContent = 'Go'; return; }
      if (method === 'later') { method = 'app'; appId = '3568013'; installId = ''; appKey = ''; }

      try {
        var body = {org: org, repos: repos, primary_repo: primary || repos.split(',')[0].trim(), project_name: name, acmm_level: level, cluster_id: clusterId, auth_method: method, is_public: document.getElementById('f-public').checked};
        if (method === 'pat') body.github_token = token;
        else { body.app_id = appId.trim(); body.installation_id = installId.trim(); body.app_private_key = appKey.trim(); }

        var resp = await fetch('/api/saas/hives', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(body)
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to create hive', 'error'); return; }

        document.getElementById('create-modal').style.display = 'none';
        document.getElementById('btn-go').disabled = false;
        document.getElementById('btn-go').textContent = 'Go';

        hiveToast('Hive ' + data.id + ' is provisioning!', 'success');
        loadHives();
      } catch(e) {
        hiveToast('Error: ' + e.message, 'error');
      } finally {
        _createInProgress = false;
        document.getElementById('btn-go').disabled = false;
        document.getElementById('btn-go').textContent = 'Go';
      }
    }

    function parseHiveYaml(text) {
      var cfg = {};
      var lines = text.split('\n');
      var section = '';
      for (var i = 0; i < lines.length; i++) {
        var line = lines[i];
        var trimmed = line.replace(/\s+$/, '');
        if (/^project:/.test(trimmed)) { section = 'project'; continue; }
        if (/^github:/.test(trimmed)) { section = 'github'; continue; }
        if (/^governor:/.test(trimmed)) { section = 'governor'; continue; }
        if (/^\S/.test(trimmed) && /:/.test(trimmed)) { section = ''; continue; }
        if (section === 'project') {
          var m;
          if ((m = trimmed.match(/^\s+org:\s*(.+)/))) cfg.org = m[1].trim().replace(/^["']|["']$/g, '');
          if ((m = trimmed.match(/^\s+repos:\s*$/))) { cfg.repos = []; for (var j = i + 1; j < lines.length && /^\s+-\s/.test(lines[j]); j++) { cfg.repos.push(lines[j].replace(/^\s+-\s*/, '').trim().replace(/^["']|["']$/g, '')); } }
          if ((m = trimmed.match(/^\s+repos:\s*\[(.+)\]/))) cfg.repos = m[1].split(',').map(function(r) { return r.trim().replace(/^["']|["']$/g, ''); });
          if ((m = trimmed.match(/^\s+primary_repo:\s*(.+)/))) cfg.primary = m[1].trim().replace(/^["']|["']$/g, '');
          if ((m = trimmed.match(/^\s+name:\s*(.+)/))) cfg.name = m[1].trim().replace(/^["']|["']$/g, '');
        }
        if (section === 'github') {
          var m;
          if ((m = trimmed.match(/^\s+token:\s*(.+)/))) cfg.token = m[1].trim().replace(/^["']|["']$/g, '');
          if ((m = trimmed.match(/^\s+app_id:\s*(\d+)/))) cfg.appId = m[1];
          if ((m = trimmed.match(/^\s+installation_id:\s*(\d+)/)) && !trimmed.match(/docs_installation_id/)) cfg.installId = m[1];
        }
        if (section === 'governor') {
          var m;
          if ((m = trimmed.match(/^\s+acmm_level:\s*(\d+)/))) cfg.level = parseInt(m[1]);
        }
      }
      return cfg;
    }

    function applyYamlConfig(cfg) {
      if (cfg.org) document.getElementById('f-org').value = cfg.org;
      if (cfg.repos) document.getElementById('f-repos').value = cfg.repos.join(', ');
      if (cfg.primary) document.getElementById('f-primary').value = cfg.primary;
      if (cfg.name) document.getElementById('f-name').value = cfg.name;
      if (cfg.level) document.getElementById('f-level').value = cfg.level;
      if (cfg.appId) {
        document.querySelector('input[name="auth-method"][value="app"]').checked = true;
        document.getElementById('auth-pat').style.display = 'none';
        document.getElementById('auth-app').style.display = '';
        document.getElementById('f-app-id').value = cfg.appId;
        if (cfg.installId) document.getElementById('f-install-id').value = cfg.installId;
      } else if (cfg.token) {
        document.getElementById('f-token').value = cfg.token;
      }
      var drop = document.getElementById('yaml-drop');
      drop.innerHTML = '<div style="font-size:0.82rem;color:var(--green)">✓ Config loaded</div>';
    }

    function readYamlFile(file) {
      var reader = new FileReader();
      reader.onload = function() {
        var cfg = parseHiveYaml(reader.result);
        applyYamlConfig(cfg);
        hiveToast('Config loaded from ' + file.name, 'success');
      };
      reader.readAsText(file);
    }
  </script>

  <div id="banner-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:540px;width:90%;max-height:90vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 16px;margin:0;color:var(--accent);flex-shrink:0">Send Hub Banner</h2>
      <div style="flex:1;overflow-y:auto;padding:0 32px">
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Message *</label>
          <div style="display:flex;gap:6px;margin-bottom:6px">
            <button type="button" onclick="bannerFmtBold()" title="Bold (**text**)" style="padding:3px 10px;background:var(--bg);border:1px solid var(--border);border-radius:5px;color:var(--text);cursor:pointer;font-weight:700;font-size:0.8rem">B</button>
            <button type="button" onclick="bannerFmtLink()" title="Insert link ([text](url))" style="padding:3px 10px;background:var(--bg);border:1px solid var(--border);border-radius:5px;color:var(--text);cursor:pointer;font-size:0.8rem">🔗 Link</button>
          </div>
          <textarea id="banner-message" rows="3" maxlength="500" oninput="updateBannerPreview()" placeholder="Announce a new capability... — use the buttons above for bold and links" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem;resize:vertical;font-family:inherit"></textarea>
          <div style="font-size:0.7rem;color:var(--muted);display:flex;justify-content:space-between;margin-top:2px"><span>Markdown: <code>**bold**</code>, <code>[text](https://url)</code></span><span><span id="banner-char-count">0</span>/500</span></div>
          <div style="font-size:0.7rem;color:var(--muted);margin:8px 0 3px">Preview</div>
          <div id="banner-preview" style="padding:10px 14px;border:1px dashed var(--border);border-radius:6px;font-size:0.85rem;min-height:1.4em;color:var(--text)"></div>
        </div>
        <div style="margin-bottom:16px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:8px">Color</label>
          <div id="banner-color-picker" style="display:flex;gap:8px;flex-wrap:wrap">
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;padding:6px 12px;border-radius:6px;border:1px solid var(--border);background:rgba(22,163,74,0.12)">
              <input type="radio" name="banner-color" value="green" checked style="accent-color:#4ade80"> <span style="color:#4ade80;font-size:0.82rem">Green</span>
            </label>
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;padding:6px 12px;border-radius:6px;border:1px solid var(--border);background:rgba(59,130,246,0.12)">
              <input type="radio" name="banner-color" value="blue" style="accent-color:#93c5fd"> <span style="color:#93c5fd;font-size:0.82rem">Blue</span>
            </label>
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;padding:6px 12px;border-radius:6px;border:1px solid var(--border);background:rgba(245,158,11,0.12)">
              <input type="radio" name="banner-color" value="amber" style="accent-color:#fcd34d"> <span style="color:#fcd34d;font-size:0.82rem">Amber</span>
            </label>
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;padding:6px 12px;border-radius:6px;border:1px solid var(--border);background:rgba(107,114,128,0.12)">
              <input type="radio" name="banner-color" value="gray" style="accent-color:#d1d5db"> <span style="color:#d1d5db;font-size:0.82rem">Gray</span>
            </label>
          </div>
        </div>
        <div style="margin-bottom:16px">
          <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:8px">
            <label style="font-size:0.8rem;color:var(--muted)">Target Hives *</label>
            <div style="display:flex;gap:8px">
              <button onclick="toggleAllBannerHives(true)" style="font-size:0.72rem;color:var(--accent);background:none;border:none;cursor:pointer;text-decoration:underline">Select All</button>
              <button onclick="toggleAllBannerHives(false)" style="font-size:0.72rem;color:var(--muted);background:none;border:none;cursor:pointer;text-decoration:underline">Deselect All</button>
            </div>
          </div>
          <div id="banner-hive-list" style="max-height:200px;overflow-y:auto;border:1px solid var(--border);border-radius:6px;background:var(--bg);padding:4px"></div>
        </div>
      </div>
      <div style="display:flex;justify-content:flex-end;gap:8px;padding:16px 32px 32px;flex-shrink:0">
        <button onclick="document.getElementById('banner-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>
        <button onclick="sendHubBanner()" class="btn-primary" id="btn-send-banner">Send Banner</button>
      </div>
    </div>
  </div>

  <div id="create-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:640px;width:90%;max-height:90vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 16px;margin:0;color:var(--accent);flex-shrink:0">Create Hosted Hive</h2>
      <div style="flex:1;overflow-y:auto;padding:0 32px">
      <div id="yaml-drop" style="margin-bottom:16px;border:2px dashed var(--border);border-radius:8px;padding:16px;text-align:center;cursor:pointer;transition:border-color 0.2s"
        ondragover="event.preventDefault();this.style.borderColor='var(--accent)'"
        ondragleave="this.style.borderColor='var(--border)'"
        ondrop="event.preventDefault();this.style.borderColor='var(--border)';var f=event.dataTransfer.files[0];if(f)readYamlFile(f)"
        onclick="document.getElementById('yaml-upload').click()">
        <div style="font-size:0.82rem;color:var(--muted)">Drop a <code>hive.yaml</code> here or <span style="color:var(--accent);text-decoration:underline">browse</span></div>
        <div style="font-size:0.7rem;color:var(--muted);margin-top:4px">Auto-fills all fields including GitHub App credentials</div>
        <div id="yaml-download-link" style="display:none;font-size:0.7rem;margin-top:6px"><a id="yaml-download-href" href="#" target="_blank" style="color:var(--accent)" onclick="event.stopPropagation()">⬇ Download hive.yaml from your local hive</a></div>
        <input type="file" id="yaml-upload" accept=".yaml,.yml" style="display:none" onchange="if(this.files[0])readYamlFile(this.files[0])">
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">GitHub Organization *</label>
        <input id="f-org" type="text" placeholder="my-org" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Repositories * <span style="font-size:0.7rem">(comma-separated)</span></label>
        <input id="f-repos" type="text" placeholder="repo1, repo2" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Primary Repository</label>
        <input id="f-primary" type="text" placeholder="defaults to first repo" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Project Name</label>
        <input id="f-name" type="text" placeholder="defaults to org/repo" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
      </div>
      <div style="display:flex;gap:12px;margin-bottom:12px">
        <div style="flex:1">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">ACMM Level</label>
          <select id="f-level" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
            <option value="1">L1 — Assisted</option>
            <option value="2">L2 — Instructed</option>
            <option value="3" selected>L3 — CI/CD</option>
            <option value="4">L4 — Auto PR</option>
            <option value="5">L5 — Self-Governing</option>
            <option value="6">L6 — Fully Autonomous</option>
          </select>
        </div>
        <div style="flex:1">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Target Cluster</label>
          <select id="f-cluster" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
            <option value="">Loading clusters...</option>
          </select>
        </div>
      </div>
      <div style="margin-bottom:12px">
        <label style="display:flex;align-items:center;gap:8px;cursor:pointer;font-size:0.8rem;color:var(--muted)"><input type="checkbox" id="f-public" checked> Publicly visible in the hive registry <span style="font-size:0.7rem">(owners can toggle later from My Hives)</span></label>
      </div>
      <div style="margin-bottom:12px">
        <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Authentication Method</label>
        <div style="display:flex;gap:12px;margin-top:4px;flex-wrap:wrap">
          <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-size:0.8rem"><input type="radio" name="auth-method" value="pat" checked onchange="document.getElementById('auth-pat').style.display='';document.getElementById('auth-app').style.display='none';document.getElementById('auth-later').style.display='none';document.getElementById('auth-info-pat').style.display='';document.getElementById('auth-info-app').style.display='none';document.getElementById('auth-info-later').style.display='none'"> Personal Access Token</label>
          <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-size:0.8rem"><input type="radio" name="auth-method" value="app" onchange="document.getElementById('auth-pat').style.display='none';document.getElementById('auth-app').style.display='';document.getElementById('auth-later').style.display='none';document.getElementById('auth-info-pat').style.display='none';document.getElementById('auth-info-app').style.display='';document.getElementById('auth-info-later').style.display='none'"> GitHub App <span style="font-size:0.65rem;color:#3fb950;font-weight:600">Recommended</span></label>
          <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-size:0.8rem"><input type="radio" name="auth-method" value="later" onchange="document.getElementById('auth-pat').style.display='none';document.getElementById('auth-app').style.display='none';document.getElementById('auth-later').style.display='';document.getElementById('auth-info-pat').style.display='none';document.getElementById('auth-info-app').style.display='none';document.getElementById('auth-info-later').style.display=''"> Configure Later</label>
        </div>
        <div id="auth-info-pat" style="font-size:0.7rem;color:var(--muted);margin-top:8px;line-height:1.5;padding:8px 10px;background:rgba(255,255,255,0.03);border-radius:6px;border:1px solid var(--border)">
          The hive uses this token for all GitHub API calls — creating issues, posting advisory comments, reading repos, pushing code, and merging PRs. All actions appear as the token owner. Permissions cannot be scoped per agent trust tier.
        </div>
        <div id="auth-info-app" style="display:none;font-size:0.7rem;color:var(--muted);margin-top:8px;line-height:1.5;padding:8px 10px;background:rgba(255,255,255,0.03);border-radius:6px;border:1px solid var(--border)">
          The hive generates short-lived installation tokens scoped to each agent's trust tier — newcomers get issues-only access, contributors get code + PR access, and trusted agents can merge. Actions appear as the app, not a personal account. Requires a <a id="auth-info-app-link" href="" target="_blank" rel="noopener" style="color:var(--accent)">GitHub App</a> installed on the target org/repo.
        </div>
        <div id="auth-info-later" style="display:none;font-size:0.7rem;color:var(--muted);margin-top:8px;line-height:1.5;padding:8px 10px;background:rgba(255,255,255,0.03);border-radius:6px;border:1px solid var(--border)">
          The hive will be provisioned with the <strong>kubestellar-hive</strong> GitHub App pre-configured (App ID: 3568013). Agents will be unable to access GitHub until the app is installed on the target org and the installation ID is supplied via the hive config.<br><br>
          <a id="auth-info-later-link" href="" target="_blank" rel="noopener" style="color:var(--accent);font-weight:600">→ Install app now</a>
        </div>
      </div>
      <div id="auth-pat">
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">GitHub Token *</label>
          <input id="f-token" type="password" placeholder="ghp_xxxxxxxxxxxx" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
          <div style="font-size:0.7rem;color:var(--muted);margin-top:6px;line-height:1.5">
            Create a <a href="https://github.com/settings/tokens?type=beta" target="_blank">Fine-grained PAT</a>: Contents, Issues, Pull requests (read/write), Metadata (read).<br>
            Classic tokens (<code>ghp_</code>) work with <code>repo</code> scope.
          </div>
        </div>
      </div>
      <div id="auth-app" style="display:none">
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">App ID *</label>
          <input id="f-app-id" type="text" placeholder="123456" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
        </div>
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Installation ID *</label>
          <input id="f-install-id" type="text" placeholder="78901234" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
        </div>
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Private Key (PEM) *</label>
          <textarea id="f-app-key" rows="6" placeholder="-----BEGIN RSA PRIVATE KEY-----&#10;Paste or drag a .pem file here...&#10;-----END RSA PRIVATE KEY-----" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.8rem;font-family:monospace;resize:vertical" ondragover="event.preventDefault();this.style.borderColor='var(--accent)'" ondragleave="this.style.borderColor='var(--border)'" ondrop="event.preventDefault();this.style.borderColor='var(--border)';var f=event.dataTransfer.files[0];if(f){var r=new FileReader();r.onload=function(){document.getElementById('f-app-key').value=r.result};r.readAsText(f)}"></textarea>
          <div style="font-size:0.7rem;color:var(--muted);margin-top:4px">Download from your <a href="https://github.com/settings/apps" target="_blank">GitHub App settings</a> → Private keys.</div>
        </div>
      </div>
      <div id="auth-later" style="display:none">
        <div style="margin-bottom:12px;padding:12px;background:rgba(59,130,246,0.08);border:1px solid rgba(59,130,246,0.2);border-radius:8px">
          <div style="font-size:0.85rem;font-weight:600;color:var(--text);margin-bottom:8px">Hive App: kubestellar-hive</div>
          <div style="font-size:0.75rem;color:var(--muted);line-height:1.5">App ID: <code>3568013</code> (pre-configured)<br>The hive will start without GitHub access. Install the app on the target org, then supply the Installation ID and Private Key via the hive config.</div>
          <div id="auth-later-ghe-note" style="display:none;font-size:0.75rem;color:var(--muted);line-height:1.5;margin-top:8px"></div>
          <a id="auth-later-install-link" href="" target="_blank" rel="noopener" style="display:inline-block;margin-top:8px;padding:6px 14px;background:var(--accent);color:#fff;border-radius:6px;font-size:0.8rem;font-weight:600;text-decoration:none">Install app on your org</a>
        </div>
      </div>
      </div>
      <div style="display:flex;gap:12px;justify-content:flex-end;padding:16px 32px;border-top:1px solid var(--border);flex-shrink:0">
        <button onclick="document.getElementById('create-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>
        <button id="btn-go" onclick="createHive()" class="btn-primary">Go</button>
      </div>
    </div>
  </div>

  <div id="access-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:500px;width:90%;max-height:80vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 16px;margin:0;color:var(--accent);flex-shrink:0">Manage Access</h2>
      <div style="flex:1;overflow-y:auto;padding:0 32px 32px">
      <p style="font-size:0.8rem;color:var(--muted);margin-bottom:16px" id="access-hive-label"></p>
      <div id="access-list"><div class="loading">Loading...</div></div>
      <div style="margin-top:12px;border-top:1px solid var(--border);padding-top:12px">
        <h3 style="font-size:0.9rem;margin-bottom:8px;color:var(--accent)">Pending Requests</h3>
        <div id="pending-requests"><span style="color:var(--muted);font-size:0.8rem">Loading...</span></div>
      </div>
      <div style="margin-top:16px;border-top:1px solid var(--border);padding-top:16px">
        <h3 style="font-size:0.9rem;margin-bottom:8px;color:var(--text)">Add User</h3>
        <div style="display:flex;gap:8px">
          <select id="access-username" style="flex:1;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem"><option value="">Select user...</option></select>
          <select id="access-role" style="padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
            <option value="read">Read</option>
            <option value="read-write">Read-Write</option>
            <option value="owner">Owner</option>
          </select>
          <button onclick="addAccess()" class="btn-primary" style="padding:8px 16px;font-size:0.8rem">Add</button>
        </div>
      </div>
      </div>
      <div style="display:flex;justify-content:flex-end;padding:16px 32px;border-top:1px solid var(--border);flex-shrink:0">
        <button onclick="document.getElementById('access-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Close</button>
      </div>
    </div>
  </div>

  <div id="timeline-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:640px;width:92%;max-height:82vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 8px;margin:0;color:var(--accent);flex-shrink:0">Activity Timeline</h2>
      <p style="font-size:0.8rem;color:var(--muted);margin:0;padding:0 32px 16px;flex-shrink:0" id="timeline-hive-label"></p>
      <div style="flex:1;overflow-y:auto;padding:0 32px 24px">
        <div id="timeline-list"><div class="loading">Loading...</div></div>
      </div>
      <div style="display:flex;justify-content:flex-end;padding:16px 32px;border-top:1px solid var(--border);flex-shrink:0">
        <button onclick="document.getElementById('timeline-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Close</button>
      </div>
    </div>
  </div>

  <div id="request-access-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:480px;width:90%;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 8px;margin:0;color:var(--accent)">Request Access</h2>
      <div style="padding:0 32px 24px">
        <p style="font-size:0.85rem;color:var(--muted);margin-bottom:12px">Requesting access to <strong id="request-access-hive-label" style="color:var(--text)"></strong>. The owner will review your request.</p>
        <label for="request-access-note" style="display:block;font-size:0.8rem;color:var(--text);margin-bottom:6px">Why do you need access? <span style="color:var(--red)">*</span></label>
        <textarea id="request-access-note" rows="4" placeholder="Explain why you should be granted access to this hive..." style="width:100%;padding:10px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem;resize:vertical;box-sizing:border-box"></textarea>
      </div>
      <div style="display:flex;justify-content:flex-end;gap:8px;padding:16px 32px;border-top:1px solid var(--border)">
        <button onclick="closeRequestAccessModal()" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>
        <button id="request-access-submit" onclick="submitRequestAccess()" class="btn-primary" style="padding:8px 20px">Send Request</button>
      </div>
    </div>
  </div>


  <script>
    var _accessHiveId = '';
    var _timelineHiveId = '';

    /* Per-event presentation: a colour and a short label per event kind, so the
       timeline is scannable rather than a wall of text. Kinds come from the
       Timeline* constants in timeline.go; unknown kinds fall through to a
       neutral default so an older dashboard never renders a blank row. */
    var TIMELINE_KINDS = {
      version_changed:   { label: 'Version',    color: '#3fb950' },
      branch_changed:    { label: 'Branch',     color: '#60a5fa' },
      went_offline:      { label: 'Offline',    color: '#f85149' },
      came_online:       { label: 'Online',     color: '#3fb950' },
      health_changed:    { label: 'Health',     color: '#f59e0b' },
      upgrade_started:   { label: 'Upgrade',    color: '#60a5fa' },
      upgrade_completed: { label: 'Upgraded',   color: '#3fb950' },
      upgrade_stale:     { label: 'Stuck',      color: '#f85149' },
      restarted:         { label: 'Restart',    color: '#f59e0b' },
      acmm_changed:      { label: 'ACMM',       color: '#a371f7' },
      agents_changed:    { label: 'Agents',     color: '#a371f7' },
      github_app_changed:{ label: 'GitHub App', color: '#f59e0b' },
      access:            { label: 'Access',     color: '#60a5fa' },
      ownership:         { label: 'Ownership',  color: '#60a5fa' },
      admin:             { label: 'Admin',      color: '#8b949e' }
    };

    /* Relative time, coarse on purpose: "what happened to this hive" is
       answered by ordering and rough age, not by seconds. */
    var TIMELINE_MS_PER_MIN = 60000;
    var TIMELINE_MIN_PER_HOUR = 60;
    var TIMELINE_HOURS_PER_DAY = 24;
    function timelineAgo(ts) {
      var t = Date.parse(ts);
      if (isNaN(t)) return '';
      var mins = Math.floor((Date.now() - t) / TIMELINE_MS_PER_MIN);
      if (mins < 1) return 'just now';
      if (mins < TIMELINE_MIN_PER_HOUR) return mins + 'm ago';
      var hours = Math.floor(mins / TIMELINE_MIN_PER_HOUR);
      if (hours < TIMELINE_HOURS_PER_DAY) return hours + 'h ago';
      return Math.floor(hours / TIMELINE_HOURS_PER_DAY) + 'd ago';
    }

    async function openTimelineModal(hiveId, hiveName) {
      _timelineHiveId = hiveId;
      document.getElementById('timeline-hive-label').textContent = hiveName || hiveId;
      document.getElementById('timeline-list').innerHTML = '<div class="loading">Loading...</div>';
      document.getElementById('timeline-modal').style.display = 'flex';
      await loadTimeline();
    }

    async function loadTimeline() {
      var el = document.getElementById('timeline-list');
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_timelineHiveId) + '/timeline');
        if (!resp.ok) {
          el.innerHTML = '<div style="color:var(--red);font-size:0.85rem">Could not load this hive\'s timeline.</div>';
          return;
        }
        var data = await resp.json();
        var events = (data && data.events) || [];
        if (events.length === 0) {
          el.innerHTML = '<div style="color:var(--muted);font-size:0.85rem">No recorded activity yet. Events appear here when something actually changes — a version lands, health flips, the hive restarts or goes offline.</div>';
          return;
        }
        /* Server returns newest first; render in that order. */
        el.innerHTML = events.map(function(ev) {
          var kind = TIMELINE_KINDS[ev.kind] || { label: ev.kind || 'Event', color: '#8b949e' };
          var actor = ev.actor
            ? '<span style="color:var(--muted);font-size:0.7rem;margin-left:6px">by ' + esc(ev.actor) + '</span>'
            : '';
          return '<div style="display:flex;gap:10px;padding:9px 0;border-bottom:1px solid var(--border)">' +
            '<div style="flex-shrink:0;width:84px"><span style="display:inline-block;padding:2px 7px;border-radius:9999px;font-size:0.62rem;font-weight:600;white-space:nowrap;background:' + kind.color + '22;color:' + kind.color + ';border:1px solid ' + kind.color + '55">' + esc(kind.label) + '</span></div>' +
            '<div style="flex:1;min-width:0">' +
              '<div style="font-size:0.82rem;color:var(--text);word-break:break-word">' + esc(ev.detail || '') + actor + '</div>' +
              '<div style="font-size:0.68rem;color:var(--muted);margin-top:2px" title="' + esc(ev.ts || '') + '">' + esc(timelineAgo(ev.ts)) + '</div>' +
            '</div></div>';
        }).join('');
      } catch(e) {
        el.innerHTML = '<div style="color:var(--red);font-size:0.85rem">Failed to load timeline: ' + esc(e.message) + '</div>';
      }
    }

    async function openAccessModal(hiveId, dashUrl) {
      _accessHiveId = hiveId;
      // Show the hive's URL alongside its id. The raw placeholder id
      // (hosted-available-oke-05-placeholder-6q84) says nothing about where the
      // hive actually lives, and the vanity URL is what an owner recognises.
      var label = document.getElementById('access-hive-label');
      label.textContent = 'Hive: ' + hiveId;
      var urlEl = document.getElementById('access-hive-url');
      if (!urlEl) {
        urlEl = document.createElement('div');
        urlEl.id = 'access-hive-url';
        urlEl.style.cssText = 'padding:0 32px 4px;font-size:0.8rem';
        label.parentNode.insertBefore(urlEl, label.nextSibling);
      }
      if (dashUrl && dashUrl.indexOf('localhost') === -1) {
        urlEl.innerHTML = '<a href="' + esc(dashUrl) + '" target="_blank" rel="noopener" style="color:var(--blue);text-decoration:none">' +
          esc(dashUrl.replace(/^https?:\/\//, '')) + '</a>';
        urlEl.style.display = '';
      } else {
        urlEl.textContent = '';
        urlEl.style.display = 'none';
      }
      document.getElementById('access-modal').style.display = 'flex';
      await loadAccessList();
      await loadAccessUserDropdown();
      await loadPendingRequests();
    }

    async function loadPendingRequests() {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/requests');
        if (!resp.ok) return;
        var data = await resp.json();
        var reqs = data.requests || [];
        var el = document.getElementById('pending-requests');
        if (!el) return;
        if (!reqs.length) { el.innerHTML = '<span style="color:var(--muted);font-size:0.8rem">No pending requests</span>'; return; }
        el.innerHTML = reqs.map(function(r) {
          var avatar = linkedAvatar(r.username, LIST_AVATAR_PX, r.username, 'margin-right:6px');
          var note = (r.note || '').trim();
          var noteHtml = note
            ? '<div style="margin-top:4px;font-size:0.75rem;color:var(--text);white-space:pre-wrap;word-break:break-word;background:var(--bg);border-left:2px solid var(--accent);padding:4px 8px;border-radius:2px">' + esc(note) + '</div>'
            : '<div style="margin-top:4px;font-size:0.72rem;color:var(--muted);font-style:italic">(no note)</div>';
          return '<div style="padding:6px 0;border-bottom:1px solid var(--border)">' +
            '<div style="display:flex;align-items:center;justify-content:space-between">' +
            '<div>' + avatar + '<span style="font-size:0.85rem">' + esc(r.username) + '</span> <span style="font-size:0.7rem;color:var(--muted)">' + esc(r.requested_at.substring(0,10)) + '</span></div>' +
            '<div style="display:flex;gap:4px">' +
            '<select id="req-role-' + esc(r.username) + '" style="padding:2px 6px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--text);font-size:0.7rem"><option value="read">Read</option><option value="read-write">Read-Write</option></select>' +
            '<button onclick="approveRequest(\'' + esc(r.username) + '\')" style="padding:2px 8px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Approve</button>' +
            '<button onclick="denyRequest(\'' + esc(r.username) + '\')" style="padding:2px 8px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Deny</button>' +
            '</div></div>' + noteHtml + '</div>';
        }).join('');
      } catch(e) {}
    }

    async function approveRequest(username) {
      var role = (document.getElementById('req-role-' + username) || {}).value || 'read';
      try {
        await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/requests/' + encodeURIComponent(username) + '/approve', {
          method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({role: role})
        });
        loadPendingRequests();
        loadAccessList();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function denyRequest(username) {
      try {
        await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/requests/' + encodeURIComponent(username) + '/deny', {method: 'POST'});
        loadPendingRequests();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function loadAccessUserDropdown() {
      var sel = document.getElementById('access-username');
      if (!sel) return;
      try {
        // grantable-users, NOT admin/users: the latter is admin-only, so every
        // non-admin owner got a 403 and an empty dropdown with no explanation.
        var resp = await fetch('/api/saas/grantable-users');
        if (!resp.ok) throw new Error('HTTP ' + resp.status);
        var data = await resp.json();
        var users = (data.users || []);
        if (!users.length) {
          sel.innerHTML = '<option value="">No users yet — they must sign in to the hub once</option>';
          return;
        }
        sel.innerHTML = '<option value="">Select user...</option>' + users.map(function(u) {
          return '<option value="' + esc(u) + '">' + esc(u) + '</option>';
        }).join('');
      } catch(e) {
        // Never leave the control looking merely empty — an empty dropdown is
        // indistinguishable from "no such users", which is what made this
        // confusing in the first place.
        sel.innerHTML = '<option value="">Could not load users</option>';
      }
    }

    async function loadAccessList() {
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access');
        var data = await resp.json();
        var users = data.access || [];
        if (!users.length) {
          document.getElementById('access-list').innerHTML = '<div style="color:var(--muted);font-size:0.85rem">No users have access yet</div>';
          return;
        }
        var ownerCount = users.filter(function(u) { return u.role === 'owner'; }).length;
        var rows = users.map(function(u) {
          var avatar = linkedAvatar(u.username, LIST_AVATAR_PX,
            String(u.username || '') + (u.role ? ' — ' + u.role : ''), 'margin-right:6px');
          // The last owner can be neither removed nor demoted — doing so would
          // orphan the hive with no one able to manage access.
          var isLastOwner = (u.role === 'owner' && ownerCount <= 1);
          var canRemove = !isLastOwner;
          var removeBtn = canRemove ?
            '<button onclick="removeAccess(\'' + esc(u.username) + '\')" style="padding:2px 8px;background:var(--red);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.65rem">Remove</button>' :
            '<span style="font-size:0.6rem;color:var(--muted)">last owner</span>';
          // The role pill is an editable dropdown: changing it POSTs the new role
          // (the add endpoint upserts). The last owner's role is locked (shown as
          // a static pill) so the hive can't be left without an owner.
          var ROLES = ['read', 'read-write', 'owner'];
          var roleControl = isLastOwner ?
            '<span class="role-badge role-' + u.role.replace(' ','-') + '" style="font-size:0.7rem" title="The last owner\'s role cannot be changed">' + esc(u.role) + '</span>' :
            '<select class="role-select role-' + u.role.replace(' ','-') + '" style="font-size:0.7rem;padding:2px 6px;border-radius:9999px;cursor:pointer" title="Change this user\'s permission" onchange="changeAccessRole(\'' + esc(u.username) + '\', this.value, \'' + esc(u.role) + '\')">' +
              ROLES.map(function(r) { return '<option value="' + r + '"' + (r === u.role ? ' selected' : '') + '>' + r + '</option>'; }).join('') +
            '</select>';
          return '<div style="display:flex;align-items:center;justify-content:space-between;padding:8px 0;border-bottom:1px solid var(--border)">' +
            '<div>' + avatar + '<span style="font-size:0.85rem">' + esc(u.username) + '</span></div>' +
            '<div style="display:flex;align-items:center;gap:8px">' +
            roleControl +
            removeBtn +
            '</div></div>';
        }).join('');
        document.getElementById('access-list').innerHTML = rows;
      } catch(e) {
        document.getElementById('access-list').innerHTML = '<div style="color:var(--red)">Failed to load</div>';
      }
    }

    async function addAccess() {
      var username = document.getElementById('access-username').value;
      var role = document.getElementById('access-role').value;
      if (!username) { hiveToast('Select a user', 'error'); return; }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({username: username, role: role})
        });
        if (!resp.ok) { var d = await resp.json(); hiveToast(d.error || 'Failed', 'error'); return; }
        document.getElementById('access-username').value = '';
        loadAccessList();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    async function changeAccessRole(username, newRole, oldRole) {
      if (newRole === oldRole) return;
      // Granting owner is significant — confirm it.
      if (newRole === 'owner' && !await hiveConfirm('Make ' + username + ' an owner? Owners can manage access and delete the hive.')) {
        loadAccessList();
        return;
      }
      try {
        var resp = await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({username: username, role: newRole})
        });
        if (!resp.ok) { var d = await resp.json().catch(function(){return {};}); hiveToast(d.error || 'Failed to change role', 'error'); loadAccessList(); return; }
        hiveToast(username + ' is now ' + newRole, 'success');
        loadAccessList();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); loadAccessList(); }
    }

    async function removeAccess(username) {
      if (!await hiveConfirm('Remove access for ' + username + '?')) return;
      try {
        await fetch('/api/saas/hives/' + encodeURIComponent(_accessHiveId) + '/access/' + encodeURIComponent(username), {method: 'DELETE'});
        loadAccessList();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }

    /* Banner text is NEUTRAL (matches the spoke's banner-contrast rule);
       the color choice shows in the tint and border only. */
    var _bannerColorStyles = {
      green: {bg: 'rgba(22,163,74,0.12)', border: '1px solid rgba(22,163,74,0.3)', color: 'var(--text)'},
      blue:  {bg: 'rgba(59,130,246,0.12)', border: '1px solid rgba(59,130,246,0.3)', color: 'var(--text)'},
      amber: {bg: 'rgba(245,158,11,0.12)', border: '1px solid rgba(245,158,11,0.3)', color: 'var(--text)'},
      gray:  {bg: 'rgba(107,114,128,0.12)', border: '1px solid rgba(107,114,128,0.3)', color: 'var(--text)'}
    };
    /* Set by the per-hive "Send Banner" menu item so the banner modal targets a
       single spoke; sendHubBanner() still reads .banner-hive-cb:checked, so this
       is only bookkeeping for the open path — the checked cb is the source of truth. */
    var _bannerTargetHive = null;

    (function() {
      var msgEl = document.getElementById('banner-message');
      if (msgEl) {
        msgEl.addEventListener('input', function() {
          document.getElementById('banner-char-count').textContent = this.value.length;
          updateBannerPreview();
        });
      }
      var radios = document.querySelectorAll('input[name="banner-color"]');
      radios.forEach(function(r) { r.addEventListener('change', updateBannerPreview); });
    })();

    // Same safe minimal-markdown as the spoke's renderHubBanner: escape first,
    // then re-introduce only **bold** and [text](http(s)://url) links. Keep this
    // in sync with bannerMarkdown() in the spoke dashboard.
    function bannerMarkdown(text, linkColor) {
      var out = String(text || '')
        .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
      out = out.replace(/\[([^\]]+)\]\((https?:\/\/[^\s)"']+)\)/g, function(_, label, url) {
        return '<a href="' + url + '" target="_blank" rel="noopener noreferrer" style="color:'
          + (linkColor || 'inherit') + ';text-decoration:underline">' + label + '</a>';
      });
      return out.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    }

    function updateBannerPreview() {
      var msg = (document.getElementById('banner-message').value || '').trim();
      var color = (document.querySelector('input[name="banner-color"]:checked') || {}).value || 'green';
      var s = _bannerColorStyles[color];
      var preview = document.getElementById('banner-preview');
      preview.style.background = s.bg;
      preview.style.border = s.border;
      preview.style.color = s.color;
      preview.innerHTML = msg ? ('📢 ' + bannerMarkdown(msg, s.color)) : '<em style="opacity:0.6">Type a message above to preview...</em>';
    }

    // Toolbar helpers: wrap the current selection (or insert a template) at the
    // cursor, then refresh the char count + preview.
    function bannerInsert(before, after, placeholder) {
      var el = document.getElementById('banner-message');
      var start = el.selectionStart, end = el.selectionEnd;
      var sel = el.value.slice(start, end) || placeholder;
      el.value = el.value.slice(0, start) + before + sel + after + el.value.slice(end);
      // Re-select the inserted content so the user can type over the placeholder.
      el.focus();
      el.selectionStart = start + before.length;
      el.selectionEnd = start + before.length + sel.length;
      document.getElementById('banner-char-count').textContent = el.value.length;
      updateBannerPreview();
    }
    function bannerFmtBold() { bannerInsert('**', '**', 'bold text'); }
    function bannerFmtLink() { bannerInsert('[', '](https://)', 'link text'); }

    function loadBannerHiveList() {
      var container = document.getElementById('banner-hive-list');
      container.innerHTML = '';
      var hives = _hiveRegistry || [];
      if (!hives.length) {
        container.innerHTML = '<div style="padding:12px;color:var(--muted);font-size:0.8rem;text-align:center">No hives found</div>';
        return;
      }
      hives.forEach(function(h) {
        var label = h.name || h.id;
        var div = document.createElement('div');
        div.style.cssText = 'display:flex;align-items:center;gap:8px;padding:6px 10px;border-bottom:1px solid var(--border)';
        div.innerHTML = '<label style="display:flex;align-items:center;gap:8px;cursor:pointer;flex:1;font-size:0.82rem;color:var(--text)">' +
          '<input type="checkbox" class="banner-hive-cb" value="' + esc(h.id) + '" checked style="accent-color:var(--accent)"> ' + esc(label) +
          '</label>';
        container.appendChild(div);
      });
    }

    function toggleAllBannerHives(checked) {
      document.querySelectorAll('.banner-hive-cb').forEach(function(cb) { cb.checked = checked; });
    }

    /* Per-hive entry point: opens the SAME banner modal but pre-scoped to one
       spoke. Instead of loadBannerHiveList()'s multi-hive checklist, we render a
       single non-editable target line plus one hidden checked .banner-hive-cb so
       the unchanged sendHubBanner() (which reads .banner-hive-cb:checked) posts
       exactly this hive_id to POST /api/saas/admin/hub-banner. */
    function openBannerForHive(hiveId, hiveName) {
      _bannerTargetHive = hiveId;
      document.getElementById('banner-modal').style.display = 'flex';
      /* Reset message + color to match the global open path's fresh state. */
      document.getElementById('banner-message').value = '';
      document.getElementById('banner-char-count').textContent = '0';
      document.querySelector('input[name="banner-color"][value="green"]').checked = true;
      updateBannerPreview();
      var container = document.getElementById('banner-hive-list');
      container.innerHTML = '<div style="display:flex;align-items:center;gap:8px;padding:10px;color:var(--text);font-size:0.82rem">' +
        '<span style="color:var(--muted)">Sending to:</span> <strong>' + esc(hiveName) + '</strong>' +
        '<input type="checkbox" class="banner-hive-cb" value="' + esc(hiveId) + '" checked style="display:none">' +
        '</div>';
    }

    async function sendHubBanner() {
      var msg = (document.getElementById('banner-message').value || '').trim();
      if (!msg) { hiveToast('Message is required', 'error'); return; }
      var color = (document.querySelector('input[name="banner-color"]:checked') || {}).value || 'green';
      var hiveIDs = [];
      document.querySelectorAll('.banner-hive-cb:checked').forEach(function(cb) { hiveIDs.push(cb.value); });
      if (!hiveIDs.length) { hiveToast('Select at least one hive', 'error'); return; }
      var btn = document.getElementById('btn-send-banner');
      btn.disabled = true;
      btn.textContent = 'Sending...';
      try {
        var resp = await fetch('/api/saas/admin/hub-banner', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({message: msg, color: color, hive_ids: hiveIDs})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Failed to send', 'error'); return; }
        hiveToast('Banner sent to ' + data.hive_count + ' hive(s)', 'success');
        document.getElementById('banner-modal').style.display = 'none';
        document.getElementById('banner-message').value = '';
        document.getElementById('banner-char-count').textContent = '0';
        document.querySelector('input[name="banner-color"][value="green"]').checked = true;
        updateBannerPreview();
        loadActiveBanner();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
      finally { btn.disabled = false; btn.textContent = 'Send Banner'; }
    }

    async function loadActiveBanner() {
      try {
        var resp = await fetch('/api/saas/admin/hub-banner');
        if (!resp.ok) return;
        var data = await resp.json();
        var banners = data.banners || [];
        var display = document.getElementById('active-banner-display');
        var clearBtn = document.getElementById('btn-clear-banner');
        if (!banners.length) {
          display.style.display = 'none';
          clearBtn.style.display = 'none';
          return;
        }
        var first = banners[0];
        var s = _bannerColorStyles[first.color] || _bannerColorStyles.green;
        var preview = document.getElementById('active-banner-preview');
        preview.style.background = s.bg;
        preview.style.border = s.border;
        preview.style.color = s.color;
        preview.textContent = first.message;
        var targets = document.getElementById('active-banner-targets');
        var hiveNames = banners.map(function(b) { return b.hive_id; });
        targets.textContent = 'Sent to ' + banners.length + ' hive(s): ' + hiveNames.join(', ');
        display.style.display = '';
        clearBtn.style.display = '';
      } catch(e) { /* ignore */ }
    }

    async function clearHubBanner() {
      if (!await hiveConfirm('Clear all active hub banners?')) return;
      try {
        var resp = await fetch('/api/saas/admin/hub-banner', {method: 'DELETE'});
        if (!resp.ok) { hiveToast('Failed to clear', 'error'); return; }
        hiveToast('All banners cleared', 'success');
        loadActiveBanner();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
    }
  </script>
</body>
</html>`

const (
	bannerIDPrefix       = "hub-banner-"
	maxBannerMessageLen  = 500
	maxBannerTargetHives = 100
)

var validBannerColors = map[string]bool{
	"green": true,
	"blue":  true,
	"amber": true,
	"gray":  true,
}

func (s *HubServer) handleSendHubBanner(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string   `json:"message"`
		Color   string   `json:"color"`
		HiveIDs []string `json:"hive_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	body.Message = strings.TrimSpace(body.Message)
	if body.Message == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}
	if len([]rune(body.Message)) > maxBannerMessageLen {
		http.Error(w, fmt.Sprintf(`{"error":"message exceeds %d characters"}`, maxBannerMessageLen), http.StatusBadRequest)
		return
	}
	if body.Color == "" {
		body.Color = "green"
	}
	if !validBannerColors[body.Color] {
		http.Error(w, `{"error":"invalid color; must be green, blue, amber, or gray"}`, http.StatusBadRequest)
		return
	}
	if len(body.HiveIDs) == 0 {
		http.Error(w, `{"error":"at least one hive must be selected"}`, http.StatusBadRequest)
		return
	}
	if len(body.HiveIDs) > maxBannerTargetHives {
		http.Error(w, fmt.Sprintf(`{"error":"too many hives (max %d)"}`, maxBannerTargetHives), http.StatusBadRequest)
		return
	}

	bannerID := fmt.Sprintf("%s%d", bannerIDPrefix, time.Now().UnixMilli())
	now := time.Now().UTC().Format(time.RFC3339)
	entry := &HubBannerEntry{
		ID:      bannerID,
		Message: body.Message,
		Color:   body.Color,
		SentAt:  now,
	}

	s.hubBannersMu.Lock()
	for _, hiveID := range body.HiveIDs {
		s.hubBanners[hiveID] = entry
	}
	s.hubBannersMu.Unlock()
	// Persist so the banner survives a hub restart/upgrade (the pod roll would
	// otherwise wipe the in-memory map and silently drop it).
	s.saveHubBanners()

	username := s.getAuthUser(r)
	s.logger.Info("hub banner sent",
		"banner_id", bannerID,
		"message", body.Message,
		"color", body.Color,
		"hive_count", len(body.HiveIDs),
		"by", username,
	)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"banner_id":%q,"hive_count":%d}`, bannerID, len(body.HiveIDs))
}

func (s *HubServer) handleClearHubBanner(w http.ResponseWriter, r *http.Request) {
	s.hubBannersMu.Lock()
	count := len(s.hubBanners)
	s.hubBanners = make(map[string]*HubBannerEntry)
	s.hubBannersMu.Unlock()
	// Persist the cleared (empty) state so banners stay gone across a restart.
	s.saveHubBanners()

	username := s.getAuthUser(r)
	s.logger.Info("hub banners cleared", "cleared_count", count, "by", username)

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"cleared":%d}`, count)
}

func (s *HubServer) handleGetHubBanner(w http.ResponseWriter, r *http.Request) {
	s.hubBannersMu.RLock()
	defer s.hubBannersMu.RUnlock()

	type bannerStatus struct {
		HiveID  string `json:"hive_id"`
		ID      string `json:"id"`
		Message string `json:"message"`
		Color   string `json:"color"`
		SentAt  string `json:"sent_at"`
	}
	var banners []bannerStatus
	for hiveID, entry := range s.hubBanners {
		banners = append(banners, bannerStatus{
			HiveID:  hiveID,
			ID:      entry.ID,
			Message: entry.Message,
			Color:   entry.Color,
			SentAt:  entry.SentAt,
		})
	}
	if banners == nil {
		banners = []bannerStatus{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"banners": banners})
}
