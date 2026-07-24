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
)

var saasUsersDir = "/data/saas/users"

const hubAdminUsername = "clubanderson"

// hubUpgradeDebounce is the minimum gap between hub self-upgrade rollout
// restarts. The behind-latest check runs every SHA-poll cycle, so without this
// the hub could re-trigger a restart before the previous rollout's new pod
// reports the new hash. One cycle plus rollout headroom.
const hubUpgradeDebounce = 4 * time.Minute

type SaaSUser struct {
	GitHubUsername string            `json:"github_username"`
	CreatedAt      string            `json:"created_at"`
	LastLogin      string            `json:"last_login"`
	Hives          map[string]string `json:"hives"`
	SaaSQuota      int               `json:"saas_quota"`
	Blocked        bool              `json:"blocked"`
	EncryptedToken string            `json:"encrypted_token,omitempty"`
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
	s.mux.HandleFunc("GET /api/saas/hive-config/{hiveID}", s.requireAuth(s.handleProxyHiveConfig))
	s.mux.HandleFunc("GET /api/saas/latest-sha", s.handleLatestSHA)
	s.mux.HandleFunc("POST /api/saas/hub/upgrade", s.requireAdmin(s.handleHubSelfUpgrade))
	s.mux.HandleFunc("PUT /api/saas/hub/auto-upgrade", s.requireAdmin(s.handleHubAutoUpgrade))
	s.mux.HandleFunc("GET /api/saas/auth-check", s.handleSaaSAuthCheck)
	s.mux.HandleFunc("POST /api/saas/user-token", s.requireAuth(s.handleUserToken))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/access", s.requireAuth(s.handleAccessList))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/access", s.requireAuth(s.handleAccessAdd))
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}/access/{username}", s.requireAuth(s.handleAccessRemove))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/request-access", s.requireAuth(s.handleRequestAccess))
	s.mux.HandleFunc("GET /api/saas/hives/{id}/requests", s.requireAuth(s.handleGetRequests))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/approve", s.requireAuth(s.handleApproveRequest))
	s.mux.HandleFunc("POST /api/saas/hives/{id}/requests/{username}/deny", s.requireAuth(s.handleDenyRequest))
	s.mux.HandleFunc("PUT /api/saas/hives/{id}/approve-access/{username}", s.requireAuth(s.handleApproveAccess))
	s.mux.HandleFunc("DELETE /api/saas/hives/{id}/deny-access/{username}", s.requireAuth(s.handleDenyAccess))
	s.mux.HandleFunc("GET /api/saas/access-status", s.handleAccessStatus)
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
	s.mux.HandleFunc("GET /api/hub/clusters", s.requireAuth(s.handleListClusters))
	s.mux.HandleFunc("POST /api/saas/admin/hub-banner", s.requireAdmin(s.handleSendHubBanner))
	s.mux.HandleFunc("DELETE /api/saas/admin/hub-banner", s.requireAdmin(s.handleClearHubBanner))
	s.mux.HandleFunc("GET /api/saas/admin/hub-banner", s.requireAdmin(s.handleGetHubBanner))

	// Under `go test` these long-lived pollers leak across test cases: they
	// immediately hit the GitHub API and read the package-level saas path
	// variables that the filesystem test helper swaps per-test, which the
	// race detector rightly flags. Production behavior is unchanged; tests
	// that need poller logic call the functions directly.
	if !testing.Testing() {
		go s.startProvisionWatcher()
		go s.StartLatestSHAPoller()
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
		if loadSaaSUser(cookie.Value) != nil {
			return cookie.Value
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

func (s *HubServer) handleAdminUpdateUser(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	u := loadSaaSUser(username)
	if u == nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	var body struct {
		SaaSQuota *int  `json:"saas_quota"`
		Blocked   *bool `json:"blocked"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if body.SaaSQuota != nil {
		u.SaaSQuota = *body.SaaSQuota
	}
	if body.Blocked != nil {
		u.Blocked = *body.Blocked
	}
	saveSaaSUser(u)
	s.logger.Info("audit: admin updated user", "target", username, "quota", u.SaaSQuota, "blocked", u.Blocked)
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
		http.Error(w, `{"error":"cannot delete the hub admin"}`, http.StatusForbidden)
		return
	}
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		http.Error(w, `{"error":"invalid username"}`, http.StatusBadRequest)
		return
	}
	u := loadSaaSUser(username)
	if u == nil {
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
		return
	}
	var ownedHives []string
	for hiveID, role := range u.Hives {
		if role == "owner" {
			ownedHives = append(ownedHives, hiveID)
		}
	}
	if len(ownedHives) > 0 {
		msg, _ := json.Marshal(fmt.Sprintf("user still owns %d hive(s); delete or reassign them first: %s", len(ownedHives), strings.Join(ownedHives, ", ")))
		http.Error(w, `{"error":`+string(msg)+`}`, http.StatusConflict)
		return
	}
	path := filepath.Join(saasUsersDir, username+".json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		s.logger.Warn("admin delete user: remove failed", "target", username, "error", err)
		http.Error(w, `{"error":"failed to delete user record"}`, http.StatusInternalServerError)
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
}

func (s *HubServer) handleListClusters(w http.ResponseWriter, r *http.Request) {
	var entries []ClusterListEntry
	for _, c := range s.clusters {
		entries = append(entries, ClusterListEntry{
			ID:     c.ID,
			Name:   c.Name,
			HasGPU: c.HasGPU,
			Arch:   c.Arch,
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
	Pods          int    `json:"pods"`
	PodCapacity   int    `json:"pod_capacity"`
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
	HiveCount     int `json:"hive_count"`
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

	return &ClusterHealthResponse{
		Nodes: allNodes,
		Summary: ClusterHealthSummary{
			TotalNodes:    len(allNodes),
			TotalCPUCores: aggCPUCores,
			TotalCPUPct:   aggCPUPct,
			TotalMemGB:    aggMemGB,
			TotalMemPct:   aggMemPct,
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

	result := PerClusterHealth{
		Nodes: nodes,
		Summary: ClusterHealthSummary{
			TotalNodes:            len(nodes),
			TotalCPUCores:         totalCPUCores,
			TotalCPUPct:           totalCPUPct,
			TotalMemGB:            totalMemGB,
			TotalMemPct:           totalMemPct,
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
			HiveCount:     hiveCount,
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
	Role                string                 `json:"role"`
	ProvError           string                 `json:"provError,omitempty"`
	ProvStatus          string                 `json:"provStatus,omitempty"`
	AutoUpgrade         bool                   `json:"autoUpgrade"`
	PendingRequestCount int                    `json:"pendingRequestCount,omitempty"`
	PendingRequests     []PendingAccessRequest `json:"pending_requests,omitempty"`

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
}

func (s *HubServer) handleMyHives(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	user := ensureSaaSUser(username)

	s.mu.Lock()
	s.markStaleHives()
	allHives := make([]RegistryEntry, len(s.registry.Hives))
	copy(allHives, s.registry.Hives)
	s.mu.Unlock()

	var result []MyHiveEntry

	autoUpgradeMap := make(map[string]bool)
	saasByID := make(map[string]*SaaSHive)
	for _, sh := range listSaaSHives() {
		shCopy := sh
		autoUpgradeMap[sh.ID] = sh.AutoUpgrade
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
			projectConfigForHiveID(entry.ID, entry.Org, entry.Repos, entry.PrimaryRepo, entry.ACMMLevel) != nil {
			entry.Assigning = true
			entry.AssigningTo = sh.Org
		}
	}

	isAdmin := username == hubAdminUsername
	for _, h := range allHives {
		if role, ok := user.Hives[h.ID]; ok {
			if isAdmin && role != "owner" {
				role = "owner"
			}
			entry := MyHiveEntry{RegistryEntry: h, Role: role, AutoUpgrade: autoUpgradeMap[h.ID]}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
			continue
		}
		if strings.EqualFold(h.Owner, username) {
			entry := MyHiveEntry{RegistryEntry: h, Role: "owner", AutoUpgrade: autoUpgradeMap[h.ID]}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
			user.Hives[h.ID] = "owner"
			continue
		}
		if isAdmin {
			entry := MyHiveEntry{RegistryEntry: h, Role: "owner", AutoUpgrade: autoUpgradeMap[h.ID]}
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

	saasCount := 0
	for i, h := range result {
		if strings.HasPrefix(h.ID, "hosted-") || strings.HasPrefix(h.ID, "saas-") {
			saasCount++
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
		"show_my_hives":           true,
	}

	myReq := loadProvisionRequest(username)
	if myReq != nil {
		resp["my_provision_request"] = myReq
	}

	if isAdmin {
		resp["provision_requests"] = listProvisionRequests()
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
	s.markStaleHives()
	allHives := make([]RegistryEntry, len(s.registry.Hives))
	copy(allHives, s.registry.Hives)
	s.mu.Unlock()

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
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
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
		if err := provisionHive(h, &req, cluster, s.logger); err != nil {
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
		w.Write([]byte(`{"status":"deleted"}`))
		return
	}
	if h.Owner != username && username != hubAdminUsername {
		http.Error(w, `{"error":"only the owner can delete this hive"}`, http.StatusForbidden)
		return
	}

	// Full de-provisioning: namespace, PV, OCI export, OCI file system, disk record, user cleanup.
	cluster := s.clusterForHive(h)
	if cluster == nil {
		s.logger.Error("no cluster config for deprovision", "hive_id", id, "cluster_id", h.ClusterID)
		http.Error(w, `{"error":"no cluster config — cannot deprovision"}`, http.StatusInternalServerError)
		return
	}
	deprovisionHive(h, cluster, s.logger)
	s.removeRegistryEntry(id, username)

	s.logger.Info("audit: hosted hive deleted", "hive_id", id, "by", username)
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"deleted"}`))
}

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
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
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

	go s.migrateHive(h, &fromCluster, &toCluster)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": "migrating",
		"from":   currentClusterID,
		"to":     req.TargetClusterID,
	})
}

var hubAutoUpgradePath = "/data/saas/hub-auto-upgrade"

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
		latestSHA := getLatestSHA()
		if latestSHA != "" && latestSHA != s.hubGitHash {
			s.logger.Info("audit: hub auto-upgrade initial trigger", "from", s.hubGitHash, "to", latestSHA)
			cmd := kubectlForCluster(s.hubCluster(), "rollout", "restart", "deployment/hive-hub", "-n", "hive-hub")
			if out, err := cmd.CombinedOutput(); err != nil {
				s.logger.Warn("hub auto-upgrade failed", "output", string(out))
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t}`, body.AutoUpgrade)
}

func (s *HubServer) handleHubSelfUpgrade(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	s.logger.Info("audit: hub self-upgrade triggered", "by", username)
	cmd := kubectlForCluster(s.hubCluster(), "rollout", "restart", "deployment/hive-hub", "-n", "hive-hub")
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.logger.Warn("hub self-upgrade failed", "output", string(out))
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
	var body struct {
		AutoUpgrade bool `json:"auto_upgrade"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid request body"}`)
		return
	}
	h.AutoUpgrade = body.AutoUpgrade
	if err := saveSaaSHive(h); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":"failed to save"}`)
		return
	}
	s.logger.Info("audit: auto-upgrade toggled", "hive_id", id, "auto_upgrade", body.AutoUpgrade, "by", username)

	// If enabling auto-upgrade and hive is behind, trigger immediately via kubectl
	// for hosted hives. For heartbeat-connected hives, the upgrade instruction
	// is delivered via the heartbeat response.
	if body.AutoUpgrade {
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
	fmt.Fprintf(w, `{"ok":true,"auto_upgrade":%t}`, body.AutoUpgrade)
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

var (
	latestSHAMu sync.RWMutex
	// latestSHAByBranch only ever advances to SHAs whose container image is
	// verified pullable on GHCR — it drives upgrade targets.
	latestSHAByBranch = map[string]branchSHAInfo{}
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
	url := "https://api.github.com/repos/kubestellar/hive/branches?per_page=100"
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
	tokenResp, err := client.Get("https://ghcr.io/token?scope=repository:kubestellar/hive:pull")
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
	next := "https://ghcr.io/v2/kubestellar/hive/tags/list?n=1000"
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
			return "https://ghcr.io" + u
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

func (s *HubServer) StartLatestSHAPoller() {
	// Serve last-known-good SHAs immediately; live fetches below refresh them.
	loadPersistedSHAs(s.logger, s.trackedBranchList())
	prevInfos := snapshotBranchSHAs()
	fetchAllBranchSHAs(s.logger, s.trackedBranchList())
	if cur := snapshotBranchSHAs(); !maps.Equal(cur, prevInfos) {
		persistLatestSHAs(s.logger)
	}
	// On first poll, check if any auto-upgrade hives are behind
	s.triggerAutoUpgrades()
	ticker := time.NewTicker(latestSHAPollInterval)
	defer ticker.Stop()
	for range ticker.C {
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
		hubBranchSHA := getLatestSHAForBranch("v2")
		if isHubAutoUpgrade() && hubBranchSHA != "" && !sameCommit(hubBranchSHA, s.hubGitHash) &&
			time.Since(s.lastHubUpgradeTrigger) > hubUpgradeDebounce {
			s.lastHubUpgradeTrigger = time.Now()
			s.logger.Info("audit: hub auto-upgrade triggered", "from", s.hubGitHash, "to", hubBranchSHA)
			cmd := kubectlForCluster(s.hubCluster(), "rollout", "restart", "deployment/hive-hub", "-n", "hive-hub")
			if out, err := cmd.CombinedOutput(); err != nil {
				s.logger.Warn("hub auto-upgrade failed", "output", string(out))
			}
		}
	}
}

func (s *HubServer) triggerAutoUpgrades() {
	hives := listSaaSHives()
	for _, h := range hives {
		if !h.AutoUpgrade {
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
						ns := "hive-hosted-" + h.ID
						cmd := kubectlForCluster(hiveCluster, "rollout", "restart", "deployment/hive", "-n", ns)
						if out, err := cmd.CombinedOutput(); err != nil {
							// Expected when the hub can't reach the hive's
							// cluster — the heartbeat fallback above handles it.
							s.logger.Warn("stale upgrade kubectl restart failed (heartbeat fallback armed)",
								"hive", h.ID, "output", string(out))
						}
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
		s.logger.Info("audit: auto-upgrade triggered", "hive_id", h.ID, "branch", branch, "from", currentSHA, "to", latestSHA, "cluster", hiveCluster.ID)
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
		ns := "hive-hosted-" + h.ID
		cmd := kubectlForCluster(hiveCluster, "rollout", "restart", "deployment/hive", "-n", ns)
		if out, err := cmd.CombinedOutput(); err != nil {
			s.logger.Warn("auto-upgrade kubectl failed, falling back to heartbeat",
				"hive", h.ID, "cluster", hiveCluster.ID, "target", latestSHA, "output", string(out))
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
	branchURL := fmt.Sprintf("https://api.github.com/repos/kubestellar/hive/branches/%s", branch)
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

	if candidateSHA == getLatestSHAForBranch(branch) {
		// Head unchanged since its image was verified — nothing to re-check.
		setBranchHead(branch, candidateSHA, commitMsg, imageStatusReady)
		return
	}

	if ghcrTagExists(client, candidateSHA, logger) {
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
	runsURL := fmt.Sprintf("https://api.github.com/repos/kubestellar/hive/actions/workflows/%s/runs?head_sha=%s&per_page=1", dockerWorkflowFile, fullSHA)
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
	commitURL := fmt.Sprintf("https://api.github.com/repos/kubestellar/hive/commits/%s", fullSHA)
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

// ghcrTagExists checks whether a container tag exists on ghcr.io/kubestellar/hive.
// Uses an anonymous token (public package) and a HEAD on the manifest endpoint.
func ghcrTagExists(client *http.Client, tag string, logger *slog.Logger) bool {
	// Get anonymous pull token
	tokenResp, err := client.Get("https://ghcr.io/token?scope=repository:kubestellar/hive:pull")
	if err != nil {
		logger.Warn("SHA poll: GHCR token request failed", "error", err)
		return false
	}
	defer tokenResp.Body.Close()
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(tokenResp.Body).Decode(&tok); err != nil {
		return false
	}

	manifestURL := fmt.Sprintf("https://ghcr.io/v2/kubestellar/hive/manifests/%s", tag)
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
	users := listAllSaaSUsers()
	var access []map[string]string
	for _, u := range users {
		if role, ok := u.Hives[hiveID]; ok {
			access = append(access, map[string]string{"username": u.GitHubUsername, "role": role})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"access": access})
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
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
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
	Username    string `json:"username"`
	Org         string `json:"org"`
	Repos       string `json:"repos"`
	PrimaryRepo string `json:"primary_repo"`
	ACMMLevel   int    `json:"acmm_level"`
	AuthMethod  string `json:"auth_method"`
	RequestedAt string `json:"requested_at"`
	Status      string `json:"status"`
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
		if pr != nil && pr.Status == provisionStatusPending {
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
		Repos       string `json:"repos"`
		PrimaryRepo string `json:"primary_repo"`
		ACMMLevel   int    `json:"acmm_level"`
		AuthMethod  string `json:"auth_method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if body.Org == "" || body.Repos == "" {
		http.Error(w, `{"error":"org and repos are required"}`, http.StatusBadRequest)
		return
	}
	if !isValidName(body.Org) {
		http.Error(w, `{"error":"invalid org name"}`, http.StatusBadRequest)
		return
	}
	for _, repo := range strings.Split(body.Repos, ",") {
		if !isValidRepoRef(strings.TrimSpace(repo)) {
			http.Error(w, `{"error":"invalid repo name"}`, http.StatusBadRequest)
			return
		}
	}

	const minACMMLevel = 1
	const maxACMMLevel = 6
	acmm := body.ACMMLevel
	if acmm < minACMMLevel || acmm > maxACMMLevel {
		acmm = minACMMLevel
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
		Org:         body.Org,
		Repos:       body.Repos,
		PrimaryRepo: primaryRepo,
		ACMMLevel:   acmm,
		AuthMethod:  body.AuthMethod,
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
			http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
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
	h.Status = ""
	h.Error = ""
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to assign placeholder hive"}`, http.StatusInternalServerError)
		return
	}

	// Ensure the user record exists and count this owned hive against a quota.
	user := loadSaaSUser(targetUsername)
	if user == nil {
		user = ensureSaaSUser(targetUsername)
	}
	user.SaaSQuota++
	if err := saveSaaSUser(user); err != nil {
		s.logger.Warn("assigned placeholder but failed to update user quota", "user", targetUsername, "error", err)
	}

	// Mark the request fulfilled.
	pr.Status = provisionStatusApproved
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

	deleteProvisionRequest(targetUsername)

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
func projectConfigForHiveID(hiveID, curOrg string, curRepos []string, curPrimary string, curACMM int) *HeartbeatProjectConfig {
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
		// Incomplete/stale record (a pre-claim hive) — leave the spoke alone.
		return nil
	}
	// Already matching — stop sending (mirrors AuthorizedUsers "leave alone"
	// semantics once the spoke has caught up).
	if strings.EqualFold(curOrg, h.Org) &&
		sameStringSliceFold(curRepos, h.Repos) &&
		strings.EqualFold(curPrimary, primary) &&
		curACMM == h.ACMMLevel {
		return nil
	}
	return &HeartbeatProjectConfig{
		Org:         h.Org,
		Repos:       h.Repos,
		PrimaryRepo: primary,
		ACMMLevel:   h.ACMMLevel,
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
	Owner         string `json:"owner"`
	Org           string `json:"org"`
	Repos         string `json:"repos"`
	PrimaryRepo   string `json:"primary_repo"`
	ProjectName   string `json:"project_name"`
	ACMMLevel     int    `json:"acmm_level"`
	IsPublic      bool   `json:"is_public"`
	AppID         string `json:"app_id"`
	InstallationID string `json:"installation_id"`
	AppPrivateKey string `json:"app_private_key"`
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
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
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
	if body.Org == "" || !isValidName(body.Org) {
		http.Error(w, `{"error":"invalid org name"}`, http.StatusBadRequest)
		return
	}
	if body.Repos == "" {
		http.Error(w, `{"error":"repos are required"}`, http.StatusBadRequest)
		return
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
	h.Repos = repos
	h.PrimaryRepo = primaryRepo
	if body.ProjectName != "" {
		h.ProjectName = body.ProjectName
	}
	h.ACMMLevel = acmm
	h.IsPublic = body.IsPublic
	h.Status = ""
	h.Error = ""
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save hive assignment"}`, http.StatusInternalServerError)
		return
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
		http.Error(w, `{"error":"user not found"}`, http.StatusNotFound)
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

var publicPaths = []string{"/snapshot", "/leaderboard", "/contribute", "/api/leaderboard", "/api/contribute"}

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
        <button class="btn-primary" id="btn-request-hive" style="display:none;background:var(--blue)" onclick="document.getElementById('request-modal').style.display='flex'">Request a Hive</button>
      </div>
    </div>

    <div id="provision-request-banner" style="display:none"></div>
    <div id="admin-provision-requests" style="display:none;margin-bottom:24px">
      <h3 style="font-size:1rem;color:var(--accent);margin-bottom:12px">Pending Provision Requests</h3>
      <div id="admin-provision-list"></div>
    </div>

    <div id="hives-container"><div class="loading">Loading your hives...</div></div>

    <div id="public-hives-section" style="display:none;margin-top:48px">
      <h2 style="font-size:1.3rem;color:var(--accent);margin-bottom:16px">Public Hives</h2>
      <div id="public-hives-container"></div>
    </div>

    <div id="admin-section" style="display:none;margin-top:48px">
      <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:16px">
        <h2 style="font-size:1.3rem;color:var(--accent)">Hub Admin — Users</h2>
        <input type="text" id="user-search" placeholder="Search users..." oninput="filterUsers()" style="padding:8px 14px;background:var(--surface);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem;width:250px">
      </div>
      <div id="users-container"><div class="loading">Loading users...</div></div>
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
      var requestModal = document.getElementById('request-modal');
      if (requestModal && requestModal.style.display === 'flex') { requestModal.style.display = 'none'; return; }
      var accessOverlay = document.querySelector('.hive-confirm-overlay');
      if (accessOverlay) { accessOverlay.remove(); return; }
      var accessModal = document.getElementById('access-modal');
      if (accessModal && accessModal.style.display === 'flex') { accessModal.style.display = 'none'; }
    });

    var ACMM_LABELS = {1:'L1 Assisted',2:'L2 Instructed',3:'L3 Measured',4:'L4 Adaptive',5:'L5 Semi-Automated',6:'L6 Autonomous'};
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
      var tips = {1:'L1 Assisted — Advisory only.',2:'L2 Instructed — Advisory beads, no GitHub writes.',3:'L3 Measured — Hold-gated PRs, CI gates.',4:'L4 Adaptive — Agents open issues, sec-check.',5:'L5 Semi-Automated — PRs with hold label, batch review.',6:'L6 Autonomous — Auto-merge on green CI.'};
      return '<span class="acmm-badge acmm-' + l + '" title="' + esc(tips[l] || '') + '">' + (ACMM_LABELS[l] || 'L' + l) + '</span>';
    }
    function roleBadge(role) {
      var cls = role === 'owner' ? 'role-owner' : role === 'read-write' ? 'role-read-write' : 'role-read';
      return '<span class="role-badge ' + cls + '">' + esc(role) + '</span>';
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
      var checks = hp.checks || [];
      var lines = [statusLabel];
      for (var i = 0; i < checks.length; i++) {
        var ck = checks[i];
        var ci = checkIcons[ck.status] || '?';
        var line = ci + ' ' + ck.name;
        if (ck.detail) line += ': ' + ck.detail;
        lines.push(line);
      }
      if (h.githubAppRequired && h.githubAppPermIssue) { lines.push('✓ GitHub App installed'); lines.push('⚠ GitHub App: permissions insufficient'); st = 'degraded'; c = colors.degraded; ic = icons.degraded; statusLabel = 'Degraded'; lines[0] = statusLabel; }
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
      return '<span title="' + esc(lines.join('\n')) + '" style="display:inline-flex;align-items:center;gap:4px;cursor:help;white-space:pre-line"><span style="display:inline-block;width:8px;height:8px;border-radius:50%;background:' + c + '"></span><span style="font-size:0.7rem;color:' + c + ';font-weight:600">' + ic + '</span></span>';
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

    async function loadUser() {
      try {
        var resp = await fetch('/api/auth/user');
        var data = await resp.json();
        if (data.authenticated) {
          _isAdmin = !!data.hub_admin;
          var roleText = data.hub_admin ? 'Hub Admin' : 'User';
          document.getElementById('nav-user').innerHTML =
            '<img src="' + esc(data.avatar_url) + '" class="nav-avatar" title="' + esc(data.login) + ' — ' + roleText + '">' +
            '<span style="font-size:0.85rem">' + esc(data.login) + '</span>' +
            '<span style="font-size:0.65rem;color:var(--muted);margin-left:6px">' + roleText + '</span>';
        }
      } catch(e) {}
    }

    var _userQuota = 0, _userUsed = 0, _isAdmin = false;
    var _latestSHA = '';
    var _latestSHAs = {};
    var _latestSHAMessages = {};
    var _latestImageStatus = {};
    var _trackedBranchesList = [];
    var _clusterList = [];
    var _commitMessages = {};
    var _allDashHives = [];
    var _dashSortKey = '', _dashSortAsc = true;
    var _hivesLoading = false;
    var _lastHivesJSON = '';
    var _lastUsersJSON = '';

    function sortDashHives(key) {
      if (_dashSortKey === key) { _dashSortAsc = !_dashSortAsc; } else { _dashSortKey = key; _dashSortAsc = true; }
      var sorted = _allDashHives.slice().sort(function(a, b) {
        var va = a[key] || '', vb = b[key] || '';
        if (typeof va === 'number' && typeof vb === 'number') return _dashSortAsc ? va - vb : vb - va;
        return _dashSortAsc ? String(va).localeCompare(String(vb)) : String(vb).localeCompare(String(va));
      });
      renderHives(sorted, true);
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
            //   - "Upgrading" = a rollout is ACTUALLY in progress (admin clicked
            //     Upgrade, so _hubUpgrading is true).
            // Previously the auto-pending case was mislabeled "Upgrading", which
            // was inconsistent with the hive rows that say "queued" for the same
            // pending state and implied the hub was mid-upgrade when it wasn't.
            var hubQueued = !_hubUpgrading && !isCurrent && hubBranchLatest && _hubAutoUpgrade;
            var hubIsUpgrading = _hubUpgrading;
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
        renderHives(data.hives || []);
        renderPendingBanner(data.hives || []);
        renderUserAccessBanner();
        renderProvisionRequestBanner(data.my_provision_request || null);
        renderAdminProvisionRequests(data.provision_requests || []);
        renderRequestHiveButton(data);
        loadPublicHives(data.hives || []);
      } catch(e) {
        if (!_allDashHives.length) {
          document.getElementById('hives-container').innerHTML = '<div class="loading">Failed to load hives</div>';
        }
      } finally {
        _hivesLoading = false;
      }
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
          var repoLink = repoPath ? '<a href="https://github.com/' + esc(repoPath) + '" target="_blank" class="repo-link">' + esc(h.primaryRepo) + '</a>' : '';
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

    function renderHives(hives, force) {
      var sig = JSON.stringify(hives);
      if (!force && sig === _lastHivesJSON) return;
      _lastHivesJSON = sig;
      if (!hives.length) {
        document.getElementById('hives-container').innerHTML =
          '<div class="empty-state">' +
          '<p style="font-size:1.2rem;margin-bottom:8px">No hives yet</p>' +
          '<p>Log in to a local hive dashboard to see it here, or create a hosted hive.</p>' +
          '</div>';
        return;
      }
      var repoPath = function(h) { return h.org && h.primaryRepo ? h.org + '/' + h.primaryRepo : h.primaryRepo || ''; };
      var buildRow = function(h, i) {
        var dot = h.online ? healthBadge(h) : '<span class="online-dot off"></span>';
        var rp = repoPath(h);
        var repoLink = rp ? '<a href="https://github.com/' + esc(rp) + '" target="_blank" class="repo-link">' + esc(h.primaryRepo) + '</a>' : '';
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
          actions = '<button onclick="openAccessModal(\'' + esc(h.id) + '\')" style="padding:3px 10px;background:var(--blue);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;white-space:nowrap;margin-right:4px">Permissions</button>';
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
        if (isHosted && (h.role === 'owner' || h.role === 'read-write')) menuItems.push('<div onclick="openAccessModal(\'' + esc(h.id) + '\')" style="' + mi + '">Permissions</div>');
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
            ? '<span id="branch-pill-' + esc(h.id) + '" style="display:inline-block;position:relative;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);margin-right:4px;cursor:pointer" onclick="toggleBranchMenu(\'' + esc(h.id) + '\')" title="Click to switch branch">' + esc(branchName) + ' ▾<div id="branch-menu-' + esc(h.id) + '" style="display:none;position:absolute;top:100%;left:0;margin-top:4px;background:#1c2128;border:1px solid #30363d;border-radius:6px;padding:4px 0;z-index:1000;min-width:60px;box-shadow:0 4px 12px rgba(0,0,0,0.4)">' + branchOptions + '</div></span>'
            : '<span style="display:inline-block;padding:1px 6px;border-radius:9999px;font-size:0.6rem;background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);margin-right:4px">' + esc(branchName) + '</span>';
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
            upgradeIcon = ' <span title="' + progressTitle + '" style="display:inline-block;padding:3px 10px;background:var(--surface);border:1px solid var(--border);border-radius:4px;font-size:0.7rem;margin-left:6px;white-space:nowrap;opacity:0.8"><span style="display:inline-block;width:12px;height:12px;border:2px solid rgba(255,255,255,0.3);border-top-color:#fff;border-radius:50%;animation:spin 1s linear infinite;vertical-align:middle;margin-right:4px"></span>' + progressLabel + '</span>';
          } else if (!isCurrent && !latestUnknown && isHosted && h.role === 'owner' && h.autoUpgrade) {
            upgradeIcon = ' <span id="upgrade-' + esc(h.id) + '" onclick="upgradeHive(\'' + esc(h.id) + '\',\'' + esc(sha) + '\',\'' + esc(branchName) + '\')" title="Auto-upgrade will apply ' + esc(branchLatest) + ' shortly — click to upgrade now' + esc(buildingHint) + '" style="display:inline-block;padding:3px 10px;background:var(--surface);color:var(--muted);border:1px dashed var(--border);border-radius:4px;cursor:pointer;font-size:0.7rem;margin-left:6px;white-space:nowrap">queued</span>';
          } else if (!isCurrent && !latestUnknown && isHosted && h.role === 'owner') {
            upgradeIcon = ' <button id="upgrade-' + esc(h.id) + '" onclick="upgradeHive(\'' + esc(h.id) + '\',\'' + esc(sha) + '\',\'' + esc(branchName) + '\')" title="Current: ' + esc(sha) + ' → Latest: ' + esc(branchLatest) + esc(buildingHint) + '" style="padding:3px 10px;background:var(--green);color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem;margin-left:6px;white-space:nowrap">Upgrade</button>';
          } else if (latestUnknown && isHosted && h.role === 'owner') {
            upgradeIcon = ' <button disabled title="Waiting for latest version…" style="padding:3px 10px;background:var(--surface);color:var(--muted);border:1px solid var(--border);border-radius:4px;font-size:0.7rem;margin-left:6px;white-space:nowrap;cursor:not-allowed;opacity:0.5">Upgrade</button>';
          }
          var autoUpgradeCheck = '';
          if (isHosted && h.role === 'owner') {
            autoUpgradeCheck = ' <label style="margin-left:8px;font-size:0.65rem;color:var(--muted);cursor:pointer;white-space:nowrap" title="Automatically upgrade when a new version is available"><input type="checkbox" ' + (h.autoUpgrade ? 'checked' : '') + ' onchange="toggleAutoUpgrade(\'' + esc(h.id) + '\',this.checked)" style="vertical-align:middle;margin-right:2px;cursor:pointer">auto</label>';
          }
          var shaMsg = _commitMessages[sha] || _latestSHAMessages[branchName] || '';
          versionCell = branch + '<span style="font-family:monospace;color:var(--muted)" title="' + esc(shaMsg) + '">' + esc(sha) + '</span>' + status + upgradeIcon + autoUpgradeCheck;
        } else { versionCell = '<span style="color:var(--muted)">—</span>'; }
        var pendingBadge = (h.pendingRequestCount > 0 && (h.role === 'owner' || h.role === 'read-write'))
          ? '<span style="position:absolute;top:-2px;right:-2px;background:var(--blue);color:#fff;border-radius:50%;width:16px;height:16px;font-size:0.6rem;display:flex;align-items:center;justify-content:center;font-weight:700">' + h.pendingRequestCount + '</span>'
          : '';
        var pendingPill = '';
        if (h.pendingRequestCount > 0 && (h.role === 'owner' || h.role === 'read-write')) {
          pendingPill = '<a href="#" onclick="togglePendingRow(\'' + esc(h.id) + '\');return false" style="display:inline-flex;align-items:center;gap:4px;padding:3px 10px;background:rgba(59,130,246,0.12);color:#60a5fa;border:1px solid rgba(59,130,246,0.3);border-radius:4px;font-size:0.7rem;text-decoration:none;cursor:pointer;white-space:nowrap">&#x1F514; ' + h.pendingRequestCount + ' pending</a>';
        }
        var TOTAL_COLUMNS = 13;
        var pendingExpandRow = '';
        if (h.pendingRequestCount > 0 && (h.role === 'owner' || h.role === 'read-write') && (h.pending_requests || []).length > 0) {
          var prItems = (h.pending_requests || []).map(function(pr) {
            var avatar = '<img src="https://github.com/' + esc(pr.username) + '.png" style="width:20px;height:20px;border-radius:50%;vertical-align:middle;margin-right:6px">';
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
        return '<tr>' +
          '<td class="hive-menu-cell" style="position:relative;width:30px;text-align:center;overflow:visible">' + (h.migrationStatus === 'migrating' ? '<span style="font-size:1.1rem;color:var(--border);user-select:none;cursor:not-allowed" title="Disabled during migration">⋮</span>' : '<span style="cursor:pointer;font-size:1.1rem;color:var(--muted);user-select:none">⋮</span>' + pendingBadge + '<div class="hive-menu-dropdown" style="display:none;position:absolute;left:0;bottom:auto;background:#1c2128;border:1px solid #30363d;border-radius:8px;min-width:160px;padding:4px 0;z-index:1000;box-shadow:0 8px 24px rgba(0,0,0,0.5)">' + menuItems.join('') + '</div>') + '</td>' +
          '<td style="text-align:left;line-height:1.4">' + (function() { var isHostedRow = h.hiveType === 'hosted' || (h.id && (h.id.startsWith('hosted-') || h.id.startsWith('saas-'))); var dh = isHostedRow && h.id ? ('/api/saas/hives/' + encodeURIComponent(h.id) + '/open') : (rb ? esc(rb) : ''); var displayName = h.name || h.id; var parts = displayName.split('/'); var orgName = parts.length > 1 ? parts[0] : ''; var repoName = parts.length > 1 ? parts.slice(1).join('/') : displayName; var rp = h.org && h.primaryRepo ? h.org + '/' + h.primaryRepo : ''; var ghIcon = rp ? '<a href="https://github.com/' + esc(rp) + '" target="_blank" style="opacity:0.5;vertical-align:middle" title="' + esc(rp) + '"><svg width="13" height="13" viewBox="0 0 16 16" fill="currentColor" style="vertical-align:middle"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg></a>' : ''; var link = function(text, bold) { if (dh) { return '<a href="' + dh + '" target="_blank" class="' + (bold ? 'hive-name-link' : 'hive-sub-link') + '" title="Open dashboard">' + esc(text) + '</a>'; } var s = bold ? 'font-weight:700;color:inherit' : 'color:#6b7280;font-weight:400'; return '<span style="' + s + '">' + esc(text) + '</span>'; }; var line1 = dot + ' ' + link(orgName || repoName, true); var line2 = orgName ? '<div style="padding-left:18px;font-size:0.8rem">' + link(repoName, false) + ' ' + ghIcon + ' ' + roleBadge(h.role) + '</div>' : '<div style="padding-left:18px">' + ghIcon + ' ' + roleBadge(h.role) + '</div>'; var line3 = pendingPill ? '<div style="margin-top:4px;padding-left:18px">' + pendingPill + '</div>' : ''; return line1 + line2 + line3; })() + '</td>' +
          '<td>' + (isLocal ? '<span style="display:inline-block;padding:2px 8px;border-radius:9999px;font-size:0.65rem;font-weight:600;background:rgba(107,114,128,0.15);color:#9ca3af;border:1px solid rgba(107,114,128,0.3)">local</span>' : clusterBadge(h.clusterId, h.clusterName)) + '</td>' +
          '<td>' + (function() { var pub = !!h.isPublic; var tid = 'vis-' + esc(h.id); if (isHosted && h.role === 'owner') { return '<label style="position:relative;display:inline-block;width:36px;height:20px;cursor:pointer"><input type="checkbox" id="' + tid + '" ' + (pub ? 'checked' : '') + ' onchange="toggleVisibility(\'' + esc(h.id) + '\',this.checked)" style="opacity:0;width:0;height:0"><span style="position:absolute;inset:0;background:' + (pub ? 'var(--green)' : 'var(--border)') + ';border-radius:10px;transition:background 0.2s"></span><span style="position:absolute;top:2px;left:' + (pub ? '18px' : '2px') + ';width:16px;height:16px;background:#fff;border-radius:50%;transition:left 0.2s"></span></label>'; } if (isLocal) { var dh = h.dashboardUrl && !h.dashboardUrl.includes('localhost') ? h.dashboardUrl : ''; var badge = pub ? '<span style="color:var(--green)">Public</span>' : '<span style="color:var(--muted)">Private</span>'; return dh ? '<a href="' + esc(dh) + '#config/governor/Hub" target="_blank" title="Change in Governor Config → Hub tab" style="text-decoration:none;cursor:pointer">' + badge + ' <span style="font-size:0.6rem;color:var(--muted)">↗</span></a>' : badge; } return pub ? '<span style="color:var(--green)">✓</span>' : '<span style="color:var(--muted)">—</span>'; })() + '</td>' +
          '<td style="font-size:0.7rem;white-space:nowrap">' + versionCell + '</td>' +
          '<td title="' + esc((h.repos || []).join('\n')) + '" style="cursor:' + (repoCount > 0 ? 'help' : 'default') + '">' + repoCount + '</td>' +
          '<td>' + acmmBadge(h.acmmLevel) + '</td>' +
          '<td title="' + esc((h.agents || []).map(function(a){ var label = a.name + ' (' + a.state + ')'; if (a.mode === 'on_demand') label += ' — on demand'; return label; }).join('\n')) + '" style="cursor:' + ((h.agentCount || 0) > 0 ? 'help' : 'default') + '">' + (h.agentCount || 0) + '</td>' +
          '<td title="Cumulative tokens consumed, as of the last heartbeat" style="white-space:nowrap;cursor:help">' + fmtTokens(h.totalTokens24h || 0) + '</td>' +
          '<td>' + modeCell + '</td>' +
          '<td>' + sparkline(h.issueHistory, '#f59e0b', 50, 14) + (h.actionableIssues || 0) + '</td>' +
          '<td>' + sparkline(h.prHistory, '#3b82f6', 50, 14) + (h.actionablePRs || 0) + '</td>' +
          '<td>' + (h.activeContributors || 0) + '</td>' +
          '</tr>' + pendingExpandRow;
      };
      /* Section-header row: a labeled separator spanning all columns, styled to
         match the table's muted uppercase heading treatment (see .hive-table th). */
      var TOTAL_COLUMNS_HEADER = 13;
      var sectionHeader = function(label, count) {
        return '<tr class="hive-section-head"><td colspan="' + TOTAL_COLUMNS_HEADER + '" ' +
          'style="padding:14px 12px 6px;color:var(--muted);font-weight:600;font-size:0.75rem;' +
          'text-transform:uppercase;letter-spacing:0.5px;text-align:left">' +
          esc(label) + ' (' + count + ')</td></tr>';
      };
      var rows;
      if (_isAdmin) {
        /* Admin-only organizational aid: split into assigned (real, claimed)
           hives and unassigned placeholders. A placeholder is signalled by
           provStatus === 'available' (primary), with an org 'available-*'
           prefix as a fallback for placeholders that have not yet reported
           provStatus. Preserve incoming order so each section stays sorted. */
        var assigned = [], unassigned = [];
        for (var _hi = 0; _hi < hives.length; _hi++) {
          var _h = hives[_hi];
          var _isPlaceholder = _h.provStatus === 'available' ||
            (_h.org && _h.org.indexOf('available-') === 0);
          if (_isPlaceholder) unassigned.push(_h); else assigned.push(_h);
        }
        /* Global running index across BOTH groups so menu ids (hive-menu-<i>)
           never collide between sections and the ⋮ dropdowns keep working. */
        var _idx = 0;
        rows = '';
        if (assigned.length > 0) {
          rows += sectionHeader('Assigned hives', assigned.length);
          for (var _ai = 0; _ai < assigned.length; _ai++) { rows += buildRow(assigned[_ai], _idx++); }
        }
        if (unassigned.length > 0) {
          rows += sectionHeader('Unassigned hives', unassigned.length);
          for (var _ui = 0; _ui < unassigned.length; _ui++) { rows += buildRow(unassigned[_ui], _idx++); }
        }
      } else {
        /* Non-admin: single flat list, exactly as before. */
        rows = hives.map(buildRow).join('');
      }
      document.getElementById('hives-container').innerHTML =
        '<div class="table-wrap"><table class="hive-table"><thead><tr>' +
        '<th></th><th onclick="sortDashHives(\'name\')" style="cursor:pointer">Hive ⇅</th><th onclick="sortDashHives(\'clusterId\')" style="cursor:pointer">Location ⇅</th><th>Public</th><th>Version</th><th>Repos</th><th onclick="sortDashHives(\'acmmLevel\')" style="cursor:pointer">ACMM ⇅</th><th onclick="sortDashHives(\'agentCount\')" style="cursor:pointer">Agents ⇅</th><th onclick="sortDashHives(\'totalTokens24h\')" style="cursor:pointer" title="Cumulative tokens consumed, as of the last heartbeat">Tokens ⇅</th><th onclick="sortDashHives(\'governorMode\')" style="cursor:pointer">Mode ⇅</th><th onclick="sortDashHives(\'actionableIssues\')" style="cursor:pointer">Issues ⇅</th><th onclick="sortDashHives(\'actionablePRs\')" style="cursor:pointer">PRs ⇅</th><th onclick="sortDashHives(\'activeContributors\')" style="cursor:pointer">Contributors ⇅</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table></div>';
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

    function renderRequestHiveButton(data) {
      var btn = document.getElementById('btn-request-hive');
      if (!btn) return;
      btn.style.display = '';
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
        msg = 'Your hive request for <strong>' + project + '</strong> has been approved! Click <strong>Provision</strong> to set up your hive.' +
          ' <button onclick="openProvisionDialog(\'' + esc(req.org) + '\',\'' + esc(req.repos) + '\',\'' + esc(req.primary_repo || '') + '\',' + (req.acmm_level || 1) + ')" style="margin-left:8px;padding:4px 12px;background:#238636;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.8rem;font-weight:600">Provision</button>';
      } else if (status === 'denied') {
        icon = '&#x274C;';
        bg = 'rgba(239,68,68,0.12)'; border = 'rgba(239,68,68,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> was denied by an admin.';
      } else {
        icon = '&#x1F3D7;&#xFE0F;';
        bg = 'rgba(245,158,11,0.12)'; border = 'rgba(245,158,11,0.3)';
        msg = 'Your hive request for <strong>' + project + '</strong> is pending admin approval.';
      }
      el.innerHTML = '<div style="background:' + bg + ';border:1px solid ' + border + ';border-radius:8px;padding:12px 16px;margin-bottom:16px;display:flex;align-items:center;gap:10px">' +
        '<span style="font-size:1.1rem">' + icon + '</span>' +
        '<span style="flex:1;font-size:0.85rem;color:var(--text)">' + msg + '</span>' +
        '</div>';
    }

    function renderAdminProvisionRequests(requests) {
      var section = document.getElementById('admin-provision-requests');
      var list = document.getElementById('admin-provision-list');
      if (!section || !list) return;
      if (!requests || !requests.length) { section.style.display = 'none'; return; }
      section.style.display = '';
      // Stash by username so the approve-picker modal can read the full request
      // (org/repos/acmm) without threading every field through an onclick string.
      _provisionRequestsByUser = {};
      requests.forEach(function(pr) { _provisionRequestsByUser[pr.username] = pr; });
      var rows = requests.map(function(pr) {
        var avatar = '<img src="https://github.com/' + esc(pr.username) + '.png" style="width:24px;height:24px;border-radius:50%;vertical-align:middle;margin-right:8px">';
        return '<div style="display:flex;align-items:center;justify-content:space-between;padding:10px 14px;background:var(--surface);border:1px solid var(--border);border-radius:8px;margin-bottom:8px">' +
          '<div style="display:flex;align-items:center;gap:8px">' +
          avatar +
          '<div>' +
          '<span style="font-size:0.85rem;font-weight:600">' + esc(pr.username) + '</span>' +
          '<span style="font-size:0.75rem;color:var(--muted);margin-left:8px">' + esc(pr.org) + '/' + esc(pr.primary_repo || pr.repos) + '</span>' +
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
        '</div>';
      var content =
        summary +
        '<label style="' + lbl + '">Placeholder to assign</label>' +
        '<select id="approve-hive" style="' + fld + '"><option value="">Loading placeholders…</option></select>' +
        '<div style="font-size:0.72rem;color:var(--muted);margin-top:6px">Auto-pick chooses an available placeholder from the request&#39;s pool.</div>' +
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
      // Populate the dropdown with available placeholders (auto-pick default).
      try {
        var resp = await fetch('/api/saas/admin/available-placeholders');
        var data = await resp.json();
        var sel = document.getElementById('approve-hive');
        if (!sel) return;
        var opts = '<option value="">Auto-pick</option>';
        (data.placeholders || []).forEach(function(p) {
          opts += '<option value="' + esc(p.id) + '">' + esc(p.id) + '  (' + esc(p.cluster_id || 'default') + ')</option>';
        });
        sel.innerHTML = opts;
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
          body: JSON.stringify({hive_id: hiveId})
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

    var _requestInProgress = false;
    async function submitProvisionRequest() {
      if (_requestInProgress) return;
      _requestInProgress = true;
      var btn = document.getElementById('btn-request-go');
      btn.disabled = true;
      btn.textContent = 'Submitting...';
      var org = document.getElementById('rq-org').value.trim();
      var repos = document.getElementById('rq-repos').value.trim();
      var primary = document.getElementById('rq-primary').value.trim();
      var level = parseInt(document.getElementById('rq-level').value) || 1;

      if (!org || !repos) { hiveToast('Org and repos are required', 'error'); _requestInProgress = false; btn.disabled = false; btn.textContent = 'Submit Request'; return; }

      try {
        var resp = await fetch('/api/saas/request-provision', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({org: org, repos: repos, primary_repo: primary || repos.split(',')[0].trim(), acmm_level: level})
        });
        var data = await resp.json();
        if (!resp.ok) { hiveToast(data.error || 'Request failed', 'error'); return; }
        document.getElementById('request-modal').style.display = 'none';
        hiveToast('Provision request submitted — awaiting admin approval', 'success');
        loadHives();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
      finally { _requestInProgress = false; btn.disabled = false; btn.textContent = 'Submit Request'; }
    }

    // --- Cluster Health Panel ---
    var CLUSTER_HEALTH_POLL_MS = 30000;
    var CLUSTER_CPU_WARN_PCT = 60;
    var CLUSTER_CPU_DANGER_PCT = 80;
    var CLUSTER_MEM_WARN_PCT = 60;
    var CLUSTER_MEM_DANGER_PCT = 80;
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
          summaryBar.innerHTML = clusterCount + ' cluster' + (clusterCount !== 1 ? 's' : '') + ' · ' +
            (s.total_nodes || 0) + ' nodes · ' + (s.total_cpu_cores || 0) + ' vCPU · ' +
            renderUnicodeSparkline('_cluster', 'cpu', cpuColor) + ' <span style="color:' + cpuColor + '">' + (s.total_cpu_percent || 0) + '% cpu</span> · ' +
            renderUnicodeSparkline('_cluster', 'mem', memColor) + ' <span style="color:' + memColor + '">' + (s.total_mem_percent || 0) + '% mem</span> · ' +
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
          var label = c.name || c.id;
          if (caps.length) label += ' (' + caps.join(', ') + ')';
          return '<option value="' + esc(c.id) + '">' + esc(label) + '</option>';
        }).join('');
      } catch(e) { /* cluster dropdown stays at default */ }
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
      await loadUser();
      await autoRequestAccessFromUrl();
      await loadHives();
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
      var filtered = _allUsers.filter(function(u) { return !q || u.github_username.toLowerCase().includes(q); });
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

    function renderUsers(users, force) {
      var sig = JSON.stringify(users);
      if (!force && sig === _lastUsersJSON) return;
      _lastUsersJSON = sig;
      if (!users.length) { document.getElementById('users-container').innerHTML = '<div class="loading">No users found</div>'; return; }
      var rows = users.map(function(u) {
        var blocked = u.blocked ? '<span style="color:var(--red);font-weight:600">BLOCKED</span>' : '<span style="color:var(--green)">active</span>';
        var avatar = '<img src="https://github.com/' + esc(u.github_username) + '.png" style="width:24px;height:24px;border-radius:50%;vertical-align:middle;margin-right:6px">';
        var isAdmin = u.github_username === 'clubanderson';
        var hivesObj = u.hives || {};
        var registryIds = new Set((_hiveRegistry || []).map(function(h) { return h.id; }));
        var hiveIds = Object.keys(hivesObj).filter(function(hid) { return registryIds.has(hid); });
        var hiveCount = hiveIds.length;
        var expandId = 'expand-' + esc(u.github_username);
        var isExpanded = _adminExpandedUsers && _adminExpandedUsers[u.github_username];

        var hiveRows = '';
        if (hiveCount > 0) {
          hiveRows = '<tr id="' + expandId + '" style="display:' + (isExpanded ? '' : 'none') + '"><td colspan="7"><div style="padding:8px 12px 8px 40px;font-size:0.75rem">';
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

        return '<tr>' +
          '<td>' + avatar + '<a href="https://github.com/' + esc(u.github_username) + '" target="_blank">' + esc(u.github_username) + '</a>' + (isAdmin ? ' <span style="color:var(--accent);font-size:0.7rem">admin</span>' : '') + '</td>' +
          '<td style="font-size:0.75rem;color:var(--muted)">' + esc(fmtUserTS(u.created_at)) + '</td>' +
          '<td style="font-size:0.75rem;color:var(--muted)">' + esc(fmtUserTS(u.last_login)) + '</td>' +
          '<td>' + blocked + '</td>' +
          '<td><input type="number" min="0" max="10" value="' + (u.saas_quota || 0) + '" style="width:50px;padding:4px;background:var(--bg);border:1px solid var(--border);border-radius:4px;color:var(--text);text-align:center" onchange="updateUser(\'' + esc(u.github_username) + '\',{saas_quota:parseInt(this.value)||0})"></td>' +
          '<td>' + (hiveCount > 0 ? '<a href="#" onclick="toggleAdminExpand(\'' + esc(u.github_username) + '\');return false" style="color:var(--blue);font-size:0.8rem">' + hiveCount + ' hive' + (hiveCount > 1 ? 's' : '') + '</a>' : '<span style="color:var(--muted)">0</span>') + '</td>' +
          '<td>' + (isAdmin ? '' : '<button onclick="updateUser(\'' + esc(u.github_username) + '\',{blocked:' + (!u.blocked) + '})" style="padding:3px 10px;background:' + (u.blocked ? 'var(--green)' : 'var(--amber)') + ';color:' + (u.blocked ? '#fff' : '#1a1a1a') + ';border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">' + (u.blocked ? 'Unblock' : 'Block') + '</button> <button onclick="deleteUser(\'' + esc(u.github_username) + '\',' + hiveCount + ')" style="padding:3px 10px;background:#b02a2a;color:#fff;border:none;border-radius:4px;cursor:pointer;font-size:0.7rem">Delete</button>') + '</td>' +
          '</tr>' + hiveRows;
      }).join('');
      document.getElementById('users-container').innerHTML =
        '<table class="hive-table"><thead><tr>' +
        '<th onclick="sortUsers(\'github_username\')" style="cursor:pointer">User ⇅</th><th onclick="sortUsers(\'created_at\')" style="cursor:pointer">Joined ⇅</th><th onclick="sortUsers(\'last_login\')" style="cursor:pointer">Last Login ⇅</th><th onclick="sortUsers(\'status\')" style="cursor:pointer">Status ⇅</th><th onclick="sortUsers(\'saas_quota\')" style="cursor:pointer">Quota ⇅</th><th onclick="sortUsers(\'hiveCount\')" style="cursor:pointer">Hives ⇅</th><th>Actions</th>' +
        '</tr></thead><tbody>' + rows + '</tbody></table>';
    }

    async function updateUser(username, updates) {
      try {
        await fetch('/api/saas/admin/users/' + encodeURIComponent(username), {
          method: 'PUT',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(updates)
        });
        loadAdminUsers();
      } catch(e) { hiveToast('Error: ' + e.message, 'error'); }
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
        if (!resp.ok) { var d = await resp.json(); hiveToast(d.error || 'Delete failed', 'error'); return; }
        hiveToast('Deleted ' + id, 'success');
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
    function openAssignModal(hiveId) {
      var h = (_allDashHives || []).reduce(function(m, x) { return x.id === hiveId ? x : m; }, null) || {};
      var fld = 'width:100%;padding:8px;background:var(--surface);color:var(--fg);border:1px solid var(--border);border-radius:6px;box-sizing:border-box';
      var lbl = 'display:block;font-size:0.75rem;color:var(--muted);margin:10px 0 4px';
      var acmmOpts = '';
      for (var lv = 0; lv <= 6; lv++) { acmmOpts += '<option value="' + lv + '"' + (lv === ASSIGN_DEFAULT_ACMM ? ' selected' : '') + '>' + lv + '</option>'; }
      var orgVal = esc(h.org || '');
      var reposVal = esc((h.repos || []).join(', '));
      var content =
        '<div style="margin-bottom:4px;font-size:0.8rem;color:var(--muted)">Claim placeholder <strong style="color:var(--fg)">' + esc(hiveId) + '</strong> for a real project.</div>' +
        '<label style="' + lbl + '">Owner (GitHub login) *</label>' +
        '<input id="assign-owner" style="' + fld + '" placeholder="octocat">' +
        '<label style="' + lbl + '">Org *</label>' +
        '<input id="assign-org" style="' + fld + '" value="' + orgVal + '" placeholder="my-org">' +
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
          body: JSON.stringify({owner: owner, org: org, repos: repos, primary_repo: primary, project_name: name, acmm_level: acmm, is_public: isPublic, app_id: appId, installation_id: installId, app_private_key: appKey})
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
    function openProvisionDialog(org, repos, primaryRepo, acmmLevel) {
      document.getElementById('f-org').value = org || '';
      document.getElementById('f-repos').value = repos || '';
      document.getElementById('f-primary').value = primaryRepo || '';
      var levelSelect = document.getElementById('f-level');
      if (levelSelect && acmmLevel) levelSelect.value = String(acmmLevel);
      document.getElementById('create-modal').style.display = 'flex';
    }

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
          <textarea id="banner-message" rows="3" maxlength="500" placeholder="Announce a new capability..." style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem;resize:vertical;font-family:inherit"></textarea>
          <div style="font-size:0.7rem;color:var(--muted);text-align:right;margin-top:2px"><span id="banner-char-count">0</span>/500</div>
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
        <div style="margin-bottom:12px">
          <label style="font-size:0.8rem;color:var(--muted);margin-bottom:4px;display:block">Preview</label>
          <div id="banner-preview" style="padding:12px 16px;border-radius:6px;font-size:0.85rem;background:rgba(22,163,74,0.12);border:1px solid rgba(22,163,74,0.3);color:var(--text)">
            <em style="opacity:0.6">Type a message above to preview...</em>
          </div>
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
          The hive generates short-lived installation tokens scoped to each agent's trust tier — newcomers get issues-only access, contributors get code + PR access, and trusted agents can merge. Actions appear as the app, not a personal account. Requires a <a href="https://github.com/apps/kubestellar-hive" target="_blank" style="color:var(--accent)">GitHub App</a> installed on the target org/repo.
        </div>
        <div id="auth-info-later" style="display:none;font-size:0.7rem;color:var(--muted);margin-top:8px;line-height:1.5;padding:8px 10px;background:rgba(255,255,255,0.03);border-radius:6px;border:1px solid var(--border)">
          The hive will be provisioned with the <strong>kubestellar-hive</strong> GitHub App pre-configured (App ID: 3568013). Agents will be unable to access GitHub until the app is installed on the target org and the installation ID is supplied via the hive config.<br><br>
          <a href="https://github.com/apps/kubestellar-hive/installations/new" target="_blank" style="color:var(--accent);font-weight:600">→ Install kubestellar-hive app now</a>
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
          <a href="https://github.com/apps/kubestellar-hive/installations/new" target="_blank" style="display:inline-block;margin-top:8px;padding:6px 14px;background:var(--accent);color:#fff;border-radius:6px;font-size:0.8rem;font-weight:600;text-decoration:none">Install kubestellar-hive on your org</a>
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

  <div id="request-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,0.6);z-index:100;align-items:center;justify-content:center">
    <div style="background:var(--surface);border:1px solid var(--border);border-radius:12px;max-width:540px;width:90%;max-height:90vh;display:flex;flex-direction:column">
      <h2 style="font-size:1.3rem;padding:32px 32px 16px;margin:0;color:var(--accent);flex-shrink:0">Request a Hive</h2>
      <div style="flex:1;overflow-y:auto;padding:0 32px">
        <p style="font-size:0.8rem;color:var(--muted);margin-bottom:16px">Submit a request for a hosted hive. An admin will review and approve it.</p>
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">GitHub Organization *</label>
          <input id="rq-org" type="text" placeholder="my-org" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
        </div>
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Repositories * <span style="font-size:0.7rem">(comma-separated)</span></label>
          <input id="rq-repos" type="text" placeholder="repo1, repo2" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
        </div>
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">Primary Repository</label>
          <input id="rq-primary" type="text" placeholder="defaults to first repo" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
        </div>
        <div style="margin-bottom:12px">
          <label style="display:block;font-size:0.8rem;color:var(--muted);margin-bottom:4px">ACMM Level</label>
          <select id="rq-level" style="width:100%;padding:8px 12px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--text);font-size:0.85rem">
            <option value="1">L1 &#x2014; Assisted</option>
            <option value="2">L2 &#x2014; Instructed</option>
            <option value="3" selected>L3 &#x2014; CI/CD</option>
            <option value="4">L4 &#x2014; Auto PR</option>
            <option value="5">L5 &#x2014; Self-Governing</option>
            <option value="6">L6 &#x2014; Fully Autonomous</option>
          </select>
        </div>
      </div>
      <div style="display:flex;gap:12px;justify-content:flex-end;padding:16px 32px;border-top:1px solid var(--border);flex-shrink:0">
        <button onclick="document.getElementById('request-modal').style.display='none'" style="padding:8px 20px;background:var(--bg);border:1px solid var(--border);border-radius:6px;color:var(--muted);cursor:pointer">Cancel</button>
        <button id="btn-request-go" onclick="submitProvisionRequest()" class="btn-primary" style="background:var(--blue)">Submit Request</button>
      </div>
    </div>
  </div>

  <script>
    var _accessHiveId = '';

    async function openAccessModal(hiveId) {
      _accessHiveId = hiveId;
      document.getElementById('access-hive-label').textContent = 'Hive: ' + hiveId;
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
          var avatar = '<img src="https://github.com/' + esc(r.username) + '.png" style="width:20px;height:20px;border-radius:50%;vertical-align:middle;margin-right:6px">';
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
      try {
        var resp = await fetch('/api/saas/admin/users');
        if (resp.status === 403) return;
        var data = await resp.json();
        var users = (data.users || []).map(function(u) { return u.github_username; });
        var sel = document.getElementById('access-username');
        sel.innerHTML = '<option value="">Select user...</option>' + users.map(function(u) {
          return '<option value="' + esc(u) + '">' + esc(u) + '</option>';
        }).join('');
      } catch(e) {}
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
          var avatar = '<img src="https://github.com/' + esc(u.username) + '.png" style="width:20px;height:20px;border-radius:50%;vertical-align:middle;margin-right:6px">';
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

    function updateBannerPreview() {
      var msg = (document.getElementById('banner-message').value || '').trim();
      var color = (document.querySelector('input[name="banner-color"]:checked') || {}).value || 'green';
      var s = _bannerColorStyles[color];
      var preview = document.getElementById('banner-preview');
      preview.style.background = s.bg;
      preview.style.border = s.border;
      preview.style.color = s.color;
      preview.innerHTML = msg ? esc(msg) : '<em style="opacity:0.6">Type a message above to preview...</em>';
    }

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
