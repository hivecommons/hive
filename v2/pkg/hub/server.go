package hub

import (
	"context"
	cryptoRand "crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

//go:embed static/*
var staticFS embed.FS

const (
	registryPath        = "/data/hub-registry.json"
	maxHeartbeatAge     = 5 * time.Minute
	staleRemoveAge      = 24 * time.Hour
	registrySaveDelay   = 5 * time.Second
	maxPayloadBytes     = 1 << 20 // 1MB
	staleUpgradeTimeout = 10 * time.Minute
)

type Registry struct {
	Hives     []RegistryEntry `json:"hives"`
	UpdatedAt string          `json:"updatedAt"`
}

type RegistryEntry struct {
	ID                        string             `json:"id"`
	Name                      string             `json:"name"`
	Org                       string             `json:"org"`
	Repos                     []string           `json:"repos"`
	PrimaryRepo               string             `json:"primaryRepo"`
	DashboardURL              string             `json:"dashboardUrl"`
	SnapshotURL               string             `json:"snapshotUrl,omitempty"`
	ACMMLevel                 int                `json:"acmmLevel"`
	AgentCount                int                `json:"agentCount"`
	GovernorMode              string             `json:"governorMode"`
	TotalTokens24h            int64              `json:"totalTokens24h"`
	ActionableIssues          int                `json:"actionableIssues"`
	ActionablePRs             int                `json:"actionablePRs"`
	ContributorCount          int                `json:"contributorCount"`
	ActiveContributors        int                `json:"activeContributors"`
	Owner                     string             `json:"owner,omitempty"`
	ClusterID                 string             `json:"clusterId,omitempty"`
	ClusterName               string             `json:"clusterName,omitempty"`
	HiveType                  string             `json:"hiveType,omitempty"`
	IsPublic                  bool               `json:"isPublic"`
	RegisteredAt              string             `json:"registeredAt"`
	LastHeartbeat             string             `json:"lastHeartbeat"`
	Health                    map[string]any     `json:"health"`
	Version                   string             `json:"version"`
	GitHash                   string             `json:"gitHash,omitempty"`
	GitBranch                 string             `json:"gitBranch,omitempty"`
	Agents                    []AgentSummary     `json:"agents,omitempty"`
	Leaderboard               []LeaderboardEntry `json:"leaderboard,omitempty"`
	Online                    bool               `json:"online"`
	Upgrading                 bool               `json:"upgrading,omitempty"`
	UpgradeTarget             string             `json:"upgradeTarget,omitempty"`
	UpgradeStartedAt          time.Time          `json:"upgradeStartedAt,omitempty"`
	IssueHistory              []SparkPoint       `json:"issueHistory,omitempty"`
	PRHistory                 []SparkPoint       `json:"prHistory,omitempty"`
	GitHubAppRequired         bool               `json:"githubAppRequired,omitempty"`
	GitHubAppPermIssue        string             `json:"githubAppPermIssue,omitempty"`
	PendingGitHubAppInstall   bool               `json:"pendingGitHubAppInstall,omitempty"`
	PendingGitHubAppInstallAt time.Time          `json:"pendingGitHubAppInstallAt,omitempty"`
}

type SparkPoint struct {
	T int64 `json:"t"`
	V int   `json:"v"`
}

var safeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func sanitizeField(s string) string {
	s = html.EscapeString(strings.TrimSpace(s))
	const maxSanitizedRunes = 200
	if runes := []rune(s); len(runes) > maxSanitizedRunes {
		s = string(runes[:maxSanitizedRunes])
	}
	return s
}

func isValidName(s string) bool {
	return safeNamePattern.MatchString(s) && len(s) <= 100
}

// isValidRepoRef validates a repo entry, which may be a bare name ("repo")
// or a cross-org "owner/repo" reference. Both segments must be safe names
// (no path traversal); at most one slash is allowed. Cross-org repos are a
// supported hive config (e.g. a primary repo in one org plus a secondary in
// another), so the heartbeat must not 400-reject them — doing so silently
// took a whole hive offline (no heartbeats → no upgrades, shows offline).
func isValidRepoRef(s string) bool {
	if len(s) > 201 {
		return false
	}
	parts := strings.Split(s, "/")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !isValidName(p) {
			return false
		}
	}
	return true
}

func secureCompareHub(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// heartbeatHealthStaleness is the maximum age of heartbeat-reported health
// data before it is considered stale and displayed with a warning.
const heartbeatHealthStaleness = 5 * time.Minute

// HeartbeatHealthEntry stores heartbeat-reported cluster health along with
// the timestamp it was received, so the hub can detect staleness.
type HeartbeatHealthEntry struct {
	Report     *HeartbeatClusterHealthReport
	ReceivedAt time.Time
}

type HubServer struct {
	mux               *http.ServeMux
	registry          Registry
	mu                sync.RWMutex
	logger            *slog.Logger
	saveCh            chan struct{}
	hubGitHash        string
	hubGitBranch      string
	hubSecret         string
	httpServer        *http.Server
	httpMu            sync.Mutex // guards httpServer (Start runs in a goroutine; Shutdown races it)
	clusters          map[string]ClusterConfig
	heartbeatHealth   map[string]*HeartbeatHealthEntry // cluster ID → latest health from spoke heartbeat
	heartbeatHealthMu sync.RWMutex

	// heartbeatUpgrade tracks hives that should be upgraded via heartbeat
	// UpgradeTo because kubectl rollout restart failed (cluster unreachable).
	// Key: hive ID, value: target SHA.  Cleared when the spoke reports the
	// target SHA, proving the upgrade completed.
	heartbeatUpgrade map[string]string
	// heartbeatSwitchTag tracks hives that should switch their deployment
	// image to a specific tag (branch switch) via heartbeat, for clusters
	// the hub can't reach over kubectl. Cleared once the spoke reports it.
	heartbeatSwitchTag map[string]string

	// pendingWebhooks stores GitHub App installation webhooks that arrived
	// before the matching spoke heartbeat.  Key: lowercase org name.
	pendingWebhooks   map[string]*pendingWebhookEntry
	pendingWebhooksMu sync.Mutex

	// pendingGitHubAppConfigs stores GitHub App config to deliver to spokes
	// via heartbeat response (bypasses OAuth proxies). Key: hive ID.
	pendingGitHubAppConfigs   map[string]*HeartbeatGitHubAppConfig
	pendingGitHubAppConfigsMu sync.Mutex

	// hubBanners stores admin-sent banners to deliver to spokes via
	// heartbeat response. Key: hive ID → banner entry.
	hubBanners   map[string]*HubBannerEntry
	hubBannersMu sync.RWMutex
}

// HubBannerEntry stores an admin banner targeted at a specific hive.
type HubBannerEntry struct {
	ID      string `json:"id"`
	Message string `json:"message"`
	Color   string `json:"color"`
	SentAt  string `json:"sent_at"`
}

func NewHubServer(port int, logger *slog.Logger, gitHash, gitBranch string) *HubServer {
	if gitBranch == "" || gitBranch == "unknown" {
		gitBranch = "v2"
	}
	secret := os.Getenv("HIVE_HUB_SECRET")
	if secret == "" {
		if data, err := os.ReadFile("/data/saas/hub-secret.key"); err == nil {
			secret = strings.TrimSpace(string(data))
		}
	}
	if secret == "" {
		const secretLen = 32
		b := make([]byte, secretLen)
		cryptoRand.Read(b)
		secret = fmt.Sprintf("%x", b)
		os.MkdirAll("/data/saas", 0o755)
		if err := os.WriteFile("/data/saas/hub-secret.key", []byte(secret), 0o600); err != nil {
			logger.Error("failed to write hub secret", "error", err)
		}
		logger.Info("generated hub secret", "path", "/data/saas/hub-secret.key")
	}
	s := &HubServer{
		mux:                     http.NewServeMux(),
		logger:                  logger,
		saveCh:                  make(chan struct{}, 1),
		hubGitHash:              gitHash,
		hubGitBranch:            gitBranch,
		hubSecret:               secret,
		clusters:                loadClusters(logger),
		heartbeatHealth:         make(map[string]*HeartbeatHealthEntry),
		heartbeatUpgrade:        make(map[string]string),
		heartbeatSwitchTag:      make(map[string]string),
		pendingWebhooks:         make(map[string]*pendingWebhookEntry),
		pendingGitHubAppConfigs: make(map[string]*HeartbeatGitHubAppConfig),
		hubBanners:              make(map[string]*HubBannerEntry),
	}

	s.loadRegistry()

	s.mux.HandleFunc("POST /api/heartbeat", s.handleHeartbeat)
	s.mux.HandleFunc("POST /api/task-status", s.handleTaskStatus)
	s.mux.HandleFunc("GET /api/registry", s.handleRegistry)
	s.mux.HandleFunc("GET /api/hub/leaderboard", s.handleLeaderboard)
	s.mux.HandleFunc("GET /api/hub/stats", s.handleStats)
	s.mux.HandleFunc("GET /api/hub/version", s.handleHubVersion)
	s.mux.HandleFunc("DELETE /api/hub/registry/{id}", s.handleRegistryDelete)
	s.mux.HandleFunc("POST /api/contribute/register", s.handleContributeProxy)
	s.mux.HandleFunc("GET /api/contribute/status", s.handleContributeStatus)
	s.mux.HandleFunc("GET /api/contribute/ws", s.handleContributeWSProxy)
	s.mux.HandleFunc("POST /api/github/webhook", s.handleGitHubWebhook)
	s.mux.HandleFunc("GET /learn", s.serveStatic("static/learn.html"))
	s.mux.HandleFunc("GET /get-started", s.serveStatic("static/get-started.html"))
	s.mux.HandleFunc("GET /api/docs", s.serveStatic("static/api-docs.html"))
	s.mux.HandleFunc("GET /{$}", s.serveStatic("static/index.html"))
	s.mux.Handle("GET /", http.FileServerFS(staticFS))

	s.registerOAuth()
	s.registerSaaSRoutes()
	go s.saveLoop()

	return s
}

const (
	hubReadTimeout  = 30 * time.Second
	hubWriteTimeout = 60 * time.Second
	hubIdleTimeout  = 120 * time.Second
)

func (s *HubServer) Start(port int) error {
	addr := fmt.Sprintf(":%d", port)
	s.logger.Info("hub server starting", "addr", addr)
	srv := &http.Server{
		Addr:         addr,
		Handler:      s.mux,
		ReadTimeout:  hubReadTimeout,
		WriteTimeout: hubWriteTimeout,
		IdleTimeout:  hubIdleTimeout,
	}
	// Start typically runs in its own goroutine while Shutdown is called
	// from another; guard the handoff of the server handle.
	s.httpMu.Lock()
	s.httpServer = srv
	s.httpMu.Unlock()
	return srv.ListenAndServe()
}

func (s *HubServer) Shutdown(timeout time.Duration) error {
	s.httpMu.Lock()
	srv := s.httpServer
	s.httpMu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return srv.Shutdown(ctx)
}

func (s *HubServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if s.hubSecret != "" {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || !secureCompareHub(strings.TrimPrefix(auth, "Bearer "), s.hubSecret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var payload HeartbeatPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	payload.HiveID = sanitizeHeartbeatField(payload.HiveID)
	if payload.HiveID == "" {
		http.Error(w, "hive_id required", http.StatusBadRequest)
		return
	}
	// Normalize the reported SHA to the canonical short length up front so every
	// downstream comparison is same-length against the hub's 7-char stored SHAs.
	// (Spokes now build gitShort with `--short=7`; this covers any that predate
	// that or report a longer value.)
	payload.GitHash = shortSHA(payload.GitHash)
	if !isValidName(payload.HiveID) {
		http.Error(w, "invalid hive_id", http.StatusBadRequest)
		return
	}
	if payload.Org != "" && !isValidName(payload.Org) {
		http.Error(w, "invalid org name", http.StatusBadRequest)
		return
	}
	if payload.PrimaryRepo != "" && !isValidRepoRef(payload.PrimaryRepo) {
		http.Error(w, "invalid repo name", http.StatusBadRequest)
		return
	}
	for _, repo := range payload.Repos {
		if !isValidRepoRef(repo) {
			http.Error(w, "invalid repo name in list", http.StatusBadRequest)
			return
		}
	}
	if payload.DashboardURL != "" && !strings.HasPrefix(payload.DashboardURL, "http://") && !strings.HasPrefix(payload.DashboardURL, "https://") {
		http.Error(w, "dashboard_url must start with http:// or https://", http.StatusBadRequest)
		return
	}
	if payload.SnapshotURL != "" && !strings.HasPrefix(payload.SnapshotURL, "http://") && !strings.HasPrefix(payload.SnapshotURL, "https://") {
		http.Error(w, "snapshot_url must start with http:// or https://", http.StatusBadRequest)
		return
	}

	payload.Org = sanitizeField(payload.Org)
	payload.PrimaryRepo = sanitizeField(payload.PrimaryRepo)
	payload.Owner = sanitizeField(payload.Owner)
	for i, a := range payload.Agents {
		if !isValidName(a.Name) {
			http.Error(w, "invalid agent name", http.StatusBadRequest)
			return
		}
		payload.Agents[i].Name = sanitizeField(a.Name)
	}
	for i, lb := range payload.Leaderboard {
		if !isValidName(lb.GitHubUsername) {
			http.Error(w, "invalid leaderboard username", http.StatusBadRequest)
			return
		}
		payload.Leaderboard[i].GitHubUsername = sanitizeField(lb.GitHubUsername)
		payload.Leaderboard[i].HiveName = sanitizeField(lb.HiveName)
	}
	safeOrg := payload.Org
	safePrimary := payload.PrimaryRepo

	entry := RegistryEntry{
		ID:   payload.HiveID,
		Name: safeOrg + "/" + safePrimary,
		Org:  safeOrg,
		Repos: func() []string {
			safe := make([]string, len(payload.Repos))
			for i, r := range payload.Repos {
				safe[i] = sanitizeHeartbeatField(r)
			}
			return safe
		}(),
		PrimaryRepo:  safePrimary,
		DashboardURL: payload.DashboardURL,
		SnapshotURL:  payload.SnapshotURL,
		ACMMLevel:    clampInt(payload.ACMMLevel, 0, 6),
		AgentCount: func() int {
			count := 0
			for _, a := range payload.Agents {
				if a.State == "running" {
					count++
				}
			}
			return count
		}(),
		GovernorMode:       sanitizeHeartbeatField(payload.Governor.Mode),
		TotalTokens24h:     clampInt64(payload.Tokens24h, 0, 100_000_000),
		ActionableIssues:   clampInt(payload.Governor.Issues, 0, 10_000),
		ActionablePRs:      clampInt(payload.Governor.PRs, 0, 10_000),
		ContributorCount:   clampInt(payload.Contributors.Registered, 0, 10_000),
		ActiveContributors: clampInt(payload.Contributors.Active, 0, 10_000),
		Owner:              sanitizeHeartbeatField(payload.Owner),
		HiveType: func() string {
			if payload.HiveType != "" {
				return sanitizeHeartbeatField(payload.HiveType)
			}
			if strings.HasPrefix(payload.HiveID, "hosted-") || strings.HasPrefix(payload.HiveID, "saas-") {
				return "hosted"
			}
			return "local"
		}(),
		IsPublic:      payload.IsPublic,
		LastHeartbeat: time.Now().UTC().Format(time.RFC3339),
		Health:        payload.Health,
		Version:       sanitizeHeartbeatField(payload.Version),
		GitHash:       shortSHA(sanitizeHeartbeatField(payload.GitHash)),
		GitBranch:     sanitizeHeartbeatField(payload.GitBranch),
		Agents: func() []AgentSummary {
			for i := range payload.Agents {
				payload.Agents[i].Name = sanitizeHeartbeatField(payload.Agents[i].Name)
				payload.Agents[i].State = sanitizeHeartbeatField(payload.Agents[i].State)
				payload.Agents[i].Mode = sanitizeHeartbeatField(payload.Agents[i].Mode)
			}
			const maxAgents = 50
			if len(payload.Agents) > maxAgents {
				payload.Agents = payload.Agents[:maxAgents]
			}
			return payload.Agents
		}(),
		Leaderboard: func() []LeaderboardEntry {
			hiveName := safeOrg + "/" + safePrimary
			for i := range payload.Leaderboard {
				payload.Leaderboard[i].HiveName = hiveName
				payload.Leaderboard[i].GitHubUsername = sanitizeHeartbeatField(payload.Leaderboard[i].GitHubUsername)
				payload.Leaderboard[i].CurrentTask = sanitizeHeartbeatField(payload.Leaderboard[i].CurrentTask)
			}
			return payload.Leaderboard
		}(),
		Online:                  true,
		GitHubAppRequired:       payload.GitHubAppRequired,
		GitHubAppPermIssue:      sanitizeHeartbeatField(payload.GitHubAppPermIssue),
		PendingGitHubAppInstall: payload.PendingGitHubAppInstall,
		PendingGitHubAppInstallAt: func() time.Time {
			if payload.PendingGitHubAppInstall {
				return time.Now()
			}
			return time.Time{}
		}(),
	}

	// Populate ClusterID from SaaS hive record, heartbeat payload, or default.
	clusterID := ""
	if sh := loadSaaSHive(payload.HiveID); sh != nil {
		clusterID = sh.ClusterID
	}
	if clusterID == "" && payload.ClusterID != "" {
		clusterID = sanitizeHeartbeatField(payload.ClusterID)
	}
	if clusterID == "" {
		clusterID = defaultClusterID
	}
	entry.ClusterID = clusterID
	if c, ok := s.clusters[clusterID]; ok {
		entry.ClusterName = c.Name
	}

	if payload.Upgrading {
		entry.Upgrading = true
	}

	s.mu.Lock()
	found := false
	for i, h := range s.registry.Hives {
		if h.ID == payload.HiveID {
			entry.RegisteredAt = h.RegisteredAt
			if entry.SnapshotURL == "" {
				entry.SnapshotURL = h.SnapshotURL
			}
			// Preserve hub-side visibility: if the hub set this hive
			// to public, don't let the spoke's heartbeat revert it.
			if h.IsPublic && !payload.IsPublic {
				entry.IsPublic = h.IsPublic
			}
			branchForLatest := payload.GitBranch
			if branchForLatest == "" {
				branchForLatest = "v2"
			}
			registryLatestSHA := getLatestSHAForBranch(branchForLatest)
			if h.Upgrading && !payload.Upgrading && (sameCommit(payload.GitHash, h.UpgradeTarget) || (registryLatestSHA != "" && sameCommit(payload.GitHash, registryLatestSHA))) {
				// Non-upgrading heartbeat at the target SHA or at latest —
				// upgrade completed (image may have advanced past the
				// original target before the spoke pulled it). sameCommit
				// tolerates short/full SHA length mismatch — a raw == left the
				// Upgrading badge spinning forever when the spoke reported a
				// full SHA and the target/latest was the 7-char short form.
				entry.Upgrading = false
				entry.UpgradeTarget = ""
			} else if h.Upgrading && !payload.Upgrading && h.UpgradeTarget == "" {
				// Upgrading with no target (stale flag from config-only restart).
				entry.Upgrading = false
				entry.UpgradeTarget = ""
			} else if h.Upgrading && h.UpgradeTarget != "" {
				if entry.GitHash != h.GitHash && entry.GitHash != "" {
					entry.Upgrading = false
					entry.UpgradeTarget = ""
				} else {
					entry.Upgrading = true
					entry.UpgradeTarget = h.UpgradeTarget
					// Preserve the trigger timestamp — losing it here made
					// the stale-upgrade recovery in triggerAutoUpgrades()
					// unreachable (it saw a zero UpgradeStartedAt forever).
					entry.UpgradeStartedAt = h.UpgradeStartedAt
				}
			}
			// Sparkline history: sample every 15 min, keep 7 days (672 points)
			const sparkSampleInterval = 15 * 60 // seconds
			const sparkMaxPoints = 672
			now := time.Now().Unix()
			entry.IssueHistory = h.IssueHistory
			entry.PRHistory = h.PRHistory
			shouldSample := len(h.IssueHistory) == 0 || (now-h.IssueHistory[len(h.IssueHistory)-1].T) >= sparkSampleInterval
			if shouldSample {
				entry.IssueHistory = append(entry.IssueHistory, SparkPoint{T: now, V: entry.ActionableIssues})
				entry.PRHistory = append(entry.PRHistory, SparkPoint{T: now, V: entry.ActionablePRs})
				if len(entry.IssueHistory) > sparkMaxPoints {
					entry.IssueHistory = entry.IssueHistory[len(entry.IssueHistory)-sparkMaxPoints:]
				}
				if len(entry.PRHistory) > sparkMaxPoints {
					entry.PRHistory = entry.PRHistory[len(entry.PRHistory)-sparkMaxPoints:]
				}
			}
			s.registry.Hives[i] = entry
			found = true
			// Clear stale "error" status on SaaS hives that are actively heartbeating.
			if strings.HasPrefix(payload.HiveID, "hosted-") {
				if sh := loadSaaSHive(payload.HiveID); sh != nil && sh.Status == "error" {
					sh.Status = "running"
					sh.Error = ""
					_ = saveSaaSHive(sh)
				}
			}
			break
		}
	}
	const maxRegistryEntries = 200
	if !found {
		if strings.HasPrefix(payload.HiveID, "hosted-") || strings.HasPrefix(payload.HiveID, "saas-") {
			if loadSaaSHive(payload.HiveID) == nil {
				s.mu.Unlock()
				http.Error(w, "hosted/saas hive IDs can only be created via provisioning", http.StatusForbidden)
				return
			}
		}
		if len(s.registry.Hives) >= maxRegistryEntries {
			s.mu.Unlock()
			http.Error(w, "registry full", http.StatusServiceUnavailable)
			return
		}
		entry.RegisteredAt = time.Now().UTC().Format(time.RFC3339)
		now := time.Now().Unix()
		entry.IssueHistory = []SparkPoint{{T: now, V: entry.ActionableIssues}}
		entry.PRHistory = []SparkPoint{{T: now, V: entry.ActionablePRs}}
		s.registry.Hives = append(s.registry.Hives, entry)
	}
	s.registry.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	s.mu.Unlock()

	s.requestSave()

	// Store heartbeat-reported cluster health so the hub can use it as a
	// fallback when it cannot reach the cluster directly via kubectl.
	if payload.ClusterHealth != nil && entry.ClusterID != "" {
		s.heartbeatHealthMu.Lock()
		s.heartbeatHealth[entry.ClusterID] = &HeartbeatHealthEntry{
			Report:     payload.ClusterHealth,
			ReceivedAt: time.Now(),
		}
		s.heartbeatHealthMu.Unlock()
		s.logger.Debug("stored heartbeat cluster health",
			"hive_id", payload.HiveID,
			"cluster_id", entry.ClusterID,
			"nodes", len(payload.ClusterHealth.Nodes),
		)
	}

	// Check if a pending webhook matches this heartbeat's pending install flag.
	if payload.PendingGitHubAppInstall {
		s.checkPendingWebhookMatch(payload.Org, payload.HiveID)
	}

	s.logger.Info("audit: hub heartbeat received",
		"hive_id", payload.HiveID,
		"org", payload.Org,
		"acmm_level", payload.ACMMLevel,
		"agents", len(payload.Agents),
	)

	// Build the heartbeat response with version info for all spokes.
	branch := payload.GitBranch
	if branch == "" {
		branch = "v2"
	}
	latestSHA := getLatestSHAForBranch(branch)
	resp := HeartbeatResponse{
		OK:         true,
		HubGitHash: s.hubGitHash,
		LatestSHA:  latestSHA,
	}

	if entry.IsPublic != payload.IsPublic {
		resp.IsPublic = &entry.IsPublic
	}

	// Deliver the hub's authoritative access list so heartbeat-only spokes
	// (which authorize their own logins) stay in sync with Manage Access grants.
	// Only for hives the hub actually has a SaaS record for; a bare non-SaaS
	// spoke gets nothing and keeps its own configured allowlist.
	if authUsers := authorizedUsersForHiveID(payload.HiveID); authUsers != nil {
		resp.AuthorizedUsers = authUsers
	}

	// Check if the hive should self-upgrade via heartbeat response.
	//
	// Three upgrade paths, all controlled by a single toggle:
	//   1. Spoke-managed: ConfigMap auto_upgrade=true, hub has no SaaS record.
	//      Hub sends UpgradeTo in every heartbeat when SHA differs.
	//   2. Hub-managed (kubectl reachable): SaaS AutoUpgrade=true.
	//      triggerAutoUpgrades() does "kubectl rollout restart". No UpgradeTo
	//      sent — avoids pod dying before replacement is Ready (503 window).
	//   3. Hub-managed (kubectl unreachable): SaaS AutoUpgrade=true but
	//      kubectl failed. triggerAutoUpgrades() adds hive to
	//      s.heartbeatUpgrade; the next heartbeat sends UpgradeTo as fallback.
	spokeManaged := payload.AutoUpgrade
	if sh := loadSaaSHive(payload.HiveID); sh != nil && sh.AutoUpgrade {
		spokeManaged = false
	}

	// Pending branch switch (image-tag change) delivered via heartbeat for
	// clusters the hub can't kubectl-reach. Takes precedence over a plain
	// SHA upgrade. Cleared when the spoke reports running that tag's build.
	s.mu.RLock()
	switchTag := s.heartbeatSwitchTag[payload.HiveID]
	hbTarget := s.heartbeatUpgrade[payload.HiveID]
	s.mu.RUnlock()
	if switchTag != "" {
		// Clear once the spoke reports it's on the target branch — its tag is
		// branchToTag(GitBranch)+"-latest". Otherwise keep instructing.
		if payload.GitBranch != "" && branchToTag(payload.GitBranch)+"-latest" == switchTag {
			s.mu.Lock()
			delete(s.heartbeatSwitchTag, payload.HiveID)
			for i := range s.registry.Hives {
				if s.registry.Hives[i].ID == payload.HiveID {
					s.registry.Hives[i].Upgrading = false
					s.registry.Hives[i].UpgradeTarget = ""
					break
				}
			}
			s.mu.Unlock()
			s.logger.Info("heartbeat: spoke branch switch complete",
				"hive_id", payload.HiveID, "branch", payload.GitBranch)
			switchTag = ""
		} else {
			resp.SwitchToTag = switchTag
			s.logger.Info("heartbeat: instructing spoke to switch branch image",
				"hive_id", payload.HiveID, "tag", switchTag)
		}
	}
	if hbTarget != "" && (sameCommit(payload.GitHash, hbTarget) || (latestSHA != "" && sameCommit(payload.GitHash, latestSHA))) {
		// Spoke already at the target or at latest — clear the fallback entry.
		// sameCommit tolerates short/full SHA length mismatch (hub stores a
		// 7-char short SHA; a spoke may report a longer one) so a hive already
		// at HEAD is not told to upgrade forever.
		s.mu.Lock()
		delete(s.heartbeatUpgrade, payload.HiveID)
		s.mu.Unlock()
		hbTarget = ""
	}

	if spokeManaged && latestSHA != "" && payload.GitHash != "" && !sameCommit(payload.GitHash, latestSHA) {
		resp.UpgradeTo = latestSHA
		s.logger.Info("heartbeat: instructing spoke-managed hive to upgrade",
			"hive_id", payload.HiveID,
			"from", payload.GitHash,
			"to", latestSHA,
		)
	} else if hbTarget != "" && !sameCommit(payload.GitHash, hbTarget) {
		resp.UpgradeTo = hbTarget
		s.logger.Info("heartbeat: instructing hub-managed hive to upgrade (kubectl fallback)",
			"hive_id", payload.HiveID,
			"from", payload.GitHash,
			"to", hbTarget,
		)
	}

	if ghCfg := s.consumePendingGitHubAppConfig(payload.HiveID); ghCfg != nil {
		resp.GitHubAppConfig = ghCfg
		s.logger.Info("heartbeat: delivering github app config to spoke",
			"hive_id", payload.HiveID,
			"app_id", ghCfg.AppID,
			"installation_id", ghCfg.InstallationID,
		)
	}

	s.hubBannersMu.RLock()
	if banner, ok := s.hubBanners[payload.HiveID]; ok {
		resp.HubBanner = &HubBanner{
			ID:      banner.ID,
			Message: banner.Message,
			Color:   banner.Color,
		}
	}
	s.hubBannersMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(resp)
	w.Write(data)
}

func (s *HubServer) storePendingGitHubAppConfig(hiveID string, cfg *HeartbeatGitHubAppConfig) {
	s.pendingGitHubAppConfigsMu.Lock()
	defer s.pendingGitHubAppConfigsMu.Unlock()
	s.pendingGitHubAppConfigs[hiveID] = cfg
}

func (s *HubServer) consumePendingGitHubAppConfig(hiveID string) *HeartbeatGitHubAppConfig {
	s.pendingGitHubAppConfigsMu.Lock()
	defer s.pendingGitHubAppConfigsMu.Unlock()
	cfg, ok := s.pendingGitHubAppConfigs[hiveID]
	if !ok {
		return nil
	}
	delete(s.pendingGitHubAppConfigs, hiveID)
	return cfg
}

func (s *HubServer) handleRegistry(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.markStaleHives()
	s.mu.Unlock()
	s.mu.RLock()
	hostedNames := make(map[string]bool)
	for _, h := range s.registry.Hives {
		if h.IsPublic && h.HiveType == "hosted" {
			hostedNames[h.Name] = true
		}
	}
	filtered := Registry{UpdatedAt: s.registry.UpdatedAt}
	for _, h := range s.registry.Hives {
		if !h.IsPublic {
			continue
		}
		if h.HiveType != "hosted" && !h.Online && hostedNames[h.Name] {
			continue
		}
		filtered.Hives = append(filtered.Hives, h)
	}
	data, _ := json.Marshal(filtered)
	s.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *HubServer) handleLeaderboard(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	merged := s.mergeLeaderboards()
	s.mu.RUnlock()

	data, _ := json.Marshal(map[string]any{"leaderboard": merged})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *HubServer) handleTaskStatus(w http.ResponseWriter, r *http.Request) {
	if s.hubSecret != "" {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || !secureCompareHub(strings.TrimPrefix(auth, "Bearer "), s.hubSecret) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadBytes))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var payload struct {
		HiveID       string             `json:"hive_id"`
		Leaderboard  []LeaderboardEntry `json:"leaderboard"`
		Contributors ContributorSummary `json:"contributors"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	payload.HiveID = sanitizeHeartbeatField(payload.HiveID)
	if payload.HiveID == "" {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}
	for i, lb := range payload.Leaderboard {
		payload.Leaderboard[i].GitHubUsername = sanitizeField(lb.GitHubUsername)
	}

	hiveName := ""
	s.mu.Lock()
	for i, h := range s.registry.Hives {
		if h.ID == payload.HiveID {
			if !h.Online {
				s.mu.Unlock()
				http.Error(w, "hive is offline — heartbeat first", http.StatusForbidden)
				return
			}
			hiveName = h.Name
			s.registry.Hives[i].ContributorCount = payload.Contributors.Registered
			s.registry.Hives[i].ActiveContributors = payload.Contributors.Active
			for j := range payload.Leaderboard {
				payload.Leaderboard[j].HiveName = hiveName
			}
			s.registry.Hives[i].Leaderboard = payload.Leaderboard
			break
		}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func (s *HubServer) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	totalAgents := 0
	totalContributors := 0
	totalIssues := 0
	totalPRs := 0
	onlineCount := 0
	publicCount := 0
	for _, h := range s.registry.Hives {
		if !h.IsPublic {
			continue
		}
		publicCount++
		totalAgents += h.AgentCount
		totalContributors += h.ActiveContributors
		totalIssues += h.ActionableIssues
		totalPRs += h.ActionablePRs
		if h.Online {
			onlineCount++
		}
	}
	s.mu.RUnlock()

	data, _ := json.Marshal(map[string]any{
		"hives":        publicCount,
		"online":       onlineCount,
		"agents":       totalAgents,
		"contributors": totalContributors,
		"issues":       totalIssues,
		"prs":          totalPRs,
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

// removeRegistryEntry removes a hive from the in-memory registry by ID.
func (s *HubServer) removeRegistryEntry(id, by string) {
	s.mu.Lock()
	for i, h := range s.registry.Hives {
		if h.ID == id {
			s.registry.Hives = append(s.registry.Hives[:i], s.registry.Hives[i+1:]...)
			s.mu.Unlock()
			s.requestSave()
			s.logger.Info("audit: registry entry removed", "id", id, "by", by)
			return
		}
	}
	s.mu.Unlock()
}

func (s *HubServer) handleRegistryDelete(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("hive_hub_user")
	if err != nil || cookie.Value != hubAdminUsername {
		http.Error(w, "admin access required", http.StatusForbidden)
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	removed := false
	for i, h := range s.registry.Hives {
		if h.ID == id {
			s.registry.Hives = append(s.registry.Hives[:i], s.registry.Hives[i+1:]...)
			removed = true
			break
		}
	}
	s.mu.Unlock()
	if removed {
		s.requestSave()
		s.logger.Info("audit: admin removed registry entry", "id", id, "admin", cookie.Value)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"removed": removed, "id": id})
}

func (s *HubServer) handleHubVersion(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"git_hash":    s.hubGitHash,
		"git_branch":  s.hubGitBranch,
		"latest_sha":  getLatestSHA(),
		"latest_shas": getLatestSHAs(),
	}
	cookie, _ := r.Cookie("hive_hub_user")
	if cookie != nil && cookie.Value == hubAdminUsername {
		resp["hub_secret"] = s.hubSecret
	}
	data, _ := json.Marshal(resp)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *HubServer) markStaleHives() {
	now := time.Now()
	kept := s.registry.Hives[:0]
	for i := range s.registry.Hives {
		if s.registry.Hives[i].LastHeartbeat == "" {
			s.registry.Hives[i].Online = false
			kept = append(kept, s.registry.Hives[i])
			continue
		}
		t, err := time.Parse(time.RFC3339, s.registry.Hives[i].LastHeartbeat)
		if err != nil {
			s.registry.Hives[i].Online = false
			kept = append(kept, s.registry.Hives[i])
			continue
		}
		age := now.Sub(t)
		if age > staleRemoveAge {
			s.logger.Info("removing stale hive", "id", s.registry.Hives[i].ID, "last_heartbeat", s.registry.Hives[i].LastHeartbeat)
			continue
		}
		s.registry.Hives[i].Online = age <= maxHeartbeatAge
		kept = append(kept, s.registry.Hives[i])
	}
	s.registry.Hives = kept
}

func (s *HubServer) mergeLeaderboards() []LeaderboardEntry {
	merged := map[string]*LeaderboardEntry{}
	for _, h := range s.registry.Hives {
		if !h.IsPublic {
			continue
		}
		for _, lb := range h.Leaderboard {
			if lb.GitHubUsername == "" || lb.TrustTier == "agent" {
				continue
			}
			if existing, ok := merged[lb.GitHubUsername]; ok {
				existing.TasksCompleted += lb.TasksCompleted
				existing.TasksFailed += lb.TasksFailed
				if lb.Active {
					existing.Active = true
					existing.CurrentTask = lb.CurrentTask
					existing.HiveName = lb.HiveName
				}
			} else {
				entry := lb
				merged[lb.GitHubUsername] = &entry
			}
		}
	}
	result := make([]LeaderboardEntry, 0, len(merged))
	for _, v := range merged {
		if v.TasksCompleted == 0 && v.TasksFailed == 0 && !v.Active {
			continue
		}
		result = append(result, *v)
	}
	return result
}

func (s *HubServer) serveStatic(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := staticFS.ReadFile(path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(data)
	}
}

func (s *HubServer) loadRegistry() {
	data, err := os.ReadFile(registryPath)
	if err != nil {
		s.logger.Info("no existing hub registry, starting fresh")
		return
	}
	if err := json.Unmarshal(data, &s.registry); err != nil {
		s.logger.Warn("failed to parse hub registry", "error", err)
	} else {
		s.logger.Info("hub registry loaded", "hives", len(s.registry.Hives))
	}
}

func (s *HubServer) requestSave() {
	select {
	case s.saveCh <- struct{}{}:
	default:
	}
}

func (s *HubServer) saveLoop() {
	for range s.saveCh {
		time.Sleep(registrySaveDelay)
		s.mu.RLock()
		data, err := json.MarshalIndent(s.registry, "", "  ")
		s.mu.RUnlock()
		if err != nil {
			s.logger.Warn("hub registry marshal failed", "error", err)
			continue
		}
		tmpPath := registryPath + ".tmp"
		if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
			s.logger.Warn("hub registry save failed", "path", tmpPath, "error", err)
			continue
		}
		if err := os.Rename(tmpPath, registryPath); err != nil {
			s.logger.Warn("hub registry rename failed", "error", err)
		}
	}
}

func (s *HubServer) findContributeHive() *RegistryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.Online && h.IsPublic && h.DashboardURL != "" && !isPrivateURL(context.Background(), h.DashboardURL) && h.Owner != "" {
			cp := h
			return &cp
		}
	}
	return nil
}

func (s *HubServer) handleContributeProxy(w http.ResponseWriter, r *http.Request) {
	hive := s.findContributeHive()
	if hive == nil {
		w.Header().Set("Content-Type", "application/json")
		http.Error(w, `{"error":"no hives available for contribution"}`, http.StatusServiceUnavailable)
		return
	}
	target, err := url.Parse(hive.DashboardURL)
	if err != nil {
		http.Error(w, `{"error":"invalid hive dashboard URL"}`, http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	r.URL.Path = "/api/contribute/register"
	r.Host = target.Host
	s.logger.Info("proxying contribute registration", "hive", hive.ID, "target", target.String())
	proxy.ServeHTTP(w, r)
}

func (s *HubServer) handleContributeStatus(w http.ResponseWriter, r *http.Request) {
	hive := s.findContributeHive()
	if hive == nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"available": false,
			"message":   "no hives currently available for contribution",
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"available": true,
		"hive_id":   hive.ID,
		"hive_name": hive.Name,
		"org":       hive.Org,
	})
}

func (s *HubServer) handleContributeWSProxy(w http.ResponseWriter, r *http.Request) {
	hive := s.findContributeHive()
	if hive == nil {
		http.Error(w, "no hives available", http.StatusServiceUnavailable)
		return
	}
	target, err := url.Parse(hive.DashboardURL)
	if err != nil {
		http.Error(w, "invalid hive URL", http.StatusInternalServerError)
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	r.URL.Path = "/api/contribute/ws"
	r.Host = target.Host
	s.logger.Info("proxying contribute WS", "hive", hive.ID, "target", target.String())
	proxy.ServeHTTP(w, r)
}

// privateURLDNSTimeout bounds DNS resolution inside the SSRF guard so a
// slow or malicious DNS server cannot block the caller indefinitely.
const privateURLDNSTimeout = 5 * time.Second

func isPrivateURL(ctx context.Context, rawURL string) bool {
	for _, scheme := range []string{"https://", "http://", "wss://", "ws://"} {
		if strings.HasPrefix(rawURL, scheme) {
			rawURL = strings.TrimPrefix(rawURL, scheme)
			break
		}
	}
	host := rawURL
	if idx := strings.IndexAny(host, ":/"); idx >= 0 {
		host = host[:idx]
	}
	host = strings.ToLower(host)
	blocked := []string{"localhost", "127.", "10.", "172.16.", "172.17.", "172.18.", "172.19.",
		"172.20.", "172.21.", "172.22.", "172.23.", "172.24.", "172.25.", "172.26.", "172.27.",
		"172.28.", "172.29.", "172.30.", "172.31.", "192.168.", "169.254.", "[::1]", "[::ffff:", "0.0.0.0", "0."}
	for _, p := range blocked {
		if strings.HasPrefix(host, p) {
			return true
		}
	}

	// Resolve hostname to catch DNS names that map to private IPs (DNS rebinding).
	resolveCtx, cancel := context.WithTimeout(ctx, privateURLDNSTimeout)
	defer cancel()
	resolver := &net.Resolver{}
	addrs, err := resolver.LookupHost(resolveCtx, host)
	if err != nil {
		// If DNS fails, treat as private (fail-closed) to prevent bypass.
		return true
	}
	for _, addr := range addrs {
		for _, p := range blocked {
			if strings.HasPrefix(addr, p) {
				return true
			}
		}
	}

	return false
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func clampInt64(v, min, max int64) int64 {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func sanitizeHeartbeatField(s string) string {
	var b strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '/' {
			b.WriteRune(c)
		}
	}
	return b.String()
}
