package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kubestellar/hive/v2/pkg/beads"
	"github.com/kubestellar/hive/v2/pkg/config"
	"github.com/kubestellar/hive/v2/pkg/github"
	"github.com/kubestellar/hive/v2/pkg/knowledge"
	"github.com/kubestellar/hive/v2/pkg/policies"
)

func (s *Server) RegisterAPI(deps *Dependencies) {
	s.deps = deps
	s.loadSidebarFromDisk()
	s.restoreGHUserSession()
	s.registerContributeRoutes()

	s.mux.HandleFunc("GET /api/version", s.handleVersion)
	s.mux.HandleFunc("GET /api/config", s.handleConfig)
	s.mux.HandleFunc("GET /api/config/download", s.handleConfigDownload)
	s.mux.HandleFunc("GET /api/audit", s.handleAuditLog)
	s.mux.HandleFunc("POST /api/self-upgrade", s.handleSelfUpgrade)
	s.mux.HandleFunc("GET /api/snapshot", s.handleSnapshotAPI)
	s.mux.HandleFunc("GET /snapshot", s.handleSnapshotPage)
	s.mux.HandleFunc("GET /api/history", s.handleHistory)
	s.mux.HandleFunc("GET /api/trends", s.handleTrends)
	s.mux.HandleFunc("GET /api/timeline", s.handleTimeline)
	s.mux.HandleFunc("GET /api/widget", s.handleWidget)
	s.mux.HandleFunc("GET /api/pane/{agent}", s.handlePane)

	s.mux.HandleFunc("GET /api/role", s.handleRole)

	s.mux.HandleFunc("POST /api/kick/{agent}", s.handleKick)
	s.mux.HandleFunc("POST /api/switch/{agent}/{backend}", s.handleSwitch)
	s.mux.HandleFunc("POST /api/model/{agent}/{model}", s.handleModelSet)
	s.mux.HandleFunc("POST /api/pause/{agent}", s.handlePause)
	s.mux.HandleFunc("POST /api/resume/{agent}", s.handleResume)
	s.mux.HandleFunc("POST /api/pin/{agent}/{dimension}", s.handlePin)
	s.mux.HandleFunc("POST /api/unpin/{agent}/{dimension}", s.handleUnpin)
	s.mux.HandleFunc("POST /api/restart/{agent}", s.handleRestart)
	s.mux.HandleFunc("POST /api/reset-restarts/{agent}", s.handleResetRestarts)

	s.mux.HandleFunc("GET /api/tokens", s.handleTokens)
	s.mux.HandleFunc("GET /api/issue-costs", s.handleIssueCosts)
	s.mux.HandleFunc("GET /api/model-advisor", s.handleModelAdvisor)
	s.mux.HandleFunc("GET /api/budget-ignore", s.handleBudgetIgnoreGet)
	s.mux.HandleFunc("POST /api/budget-ignore", s.handleBudgetIgnoreSet)

	s.mux.HandleFunc("GET /api/gh-auth", s.handleGHAuth)
	s.mux.HandleFunc("GET /api/gh-rate-limits", s.handleGHRateLimits)
	s.mux.HandleFunc("GET /api/gh-user-auth/status", s.handleGHUserAuthStatus)
	s.mux.HandleFunc("POST /api/gh-user-auth/start", s.handleGHUserAuthStart)
	s.mux.HandleFunc("POST /api/gh-user-auth/poll", s.handleGHUserAuthPoll)
	s.mux.HandleFunc("POST /api/gh-user-auth/logout", s.handleGHUserAuthLogout)
	s.mux.HandleFunc("GET /api/summaries", s.handleSummaries)

	s.mux.HandleFunc("GET /api/config/agent/{name}", s.handleAgentConfigGet)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/general", s.handleAgentConfigGeneral)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/cadences", s.handleAgentConfigCadences)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/models", s.handleAgentConfigModels)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/pipeline", s.handleAgentConfigPipeline)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/hooks", s.handleAgentConfigHooks)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/restrictions", s.handleAgentConfigRestrictions)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/stats", s.handleAgentConfigStats)
	s.mux.HandleFunc("GET /api/config/agent/{name}/prompt", s.handleAgentPrompt)
	s.mux.HandleFunc("PUT /api/config/agent/{name}/prompt", s.handleAgentPromptSave)
	s.mux.HandleFunc("GET /api/config/agent/{name}/export", s.handleAgentExport)
	s.mux.HandleFunc("GET /api/config/stat-sources", s.handleStatSources)

	s.mux.HandleFunc("GET /api/config/governor", s.handleGovernorConfigGet)
	s.mux.HandleFunc("PUT /api/config/governor/sensing", s.handleGovernorSensing)
	s.mux.HandleFunc("PUT /api/config/governor/thresholds", s.handleGovernorThresholds)
	s.mux.HandleFunc("PUT /api/config/governor/labels", s.handleGovernorLabels)
	s.mux.HandleFunc("PUT /api/config/governor/budget", s.handleGovernorBudget)
	s.mux.HandleFunc("PUT /api/config/governor/notifications", s.handleGovernorNotifications)
	s.mux.HandleFunc("PUT /api/config/governor/health", s.handleGovernorHealth)
	s.mux.HandleFunc("PUT /api/config/governor/logging", s.handleGovernorLogging)
	s.mux.HandleFunc("PUT /api/config/governor/hub", s.handleGovernorHub)
	s.mux.HandleFunc("POST /api/config/governor/agents", s.handleGovernorAddAgent)
	s.mux.HandleFunc("DELETE /api/config/governor/agents/{name}", s.handleGovernorRemoveAgent)
	s.mux.HandleFunc("PUT /api/config/governor/repos", s.handleGovernorRepos)

	s.mux.HandleFunc("GET /api/agents", s.handleAgentsList)
	s.mux.HandleFunc("POST /api/agents", s.handleAgentCreate)
	s.mux.HandleFunc("POST /api/agents/import", s.handleAgentImport)
	s.mux.HandleFunc("DELETE /api/agents/{name}", s.handleAgentDelete)

	s.mux.HandleFunc("GET /api/packs", s.handlePacksList)
	s.mux.HandleFunc("POST /api/packs/{level}/apply", s.handlePackApply)
	s.mux.HandleFunc("PUT /api/packs/level", s.handlePackSetLevel)

	s.mux.HandleFunc("GET /api/config/sidebar", s.handleSidebarGet)
	s.mux.HandleFunc("PUT /api/config/sidebar", s.handleSidebarSet)
	s.mux.HandleFunc("GET /api/config/backends", s.handleBackends)

	s.mux.HandleFunc("GET /api/knowledge", s.handleKnowledgeList)
	s.mux.HandleFunc("GET /api/knowledge/export", s.handleKnowledgeExport)
	s.mux.HandleFunc("GET /api/knowledge/search", s.handleKnowledgeSearch)
	s.mux.HandleFunc("GET /api/knowledge/health", s.handleKnowledgeHealth)
	s.mux.HandleFunc("GET /api/knowledge/stats", s.handleKnowledgeStats)
	s.mux.HandleFunc("POST /api/knowledge/create", s.handleKnowledgeCreate)
	s.mux.HandleFunc("POST /api/knowledge/import", s.handleKnowledgeImport)
	s.mux.HandleFunc("POST /api/knowledge/promote", s.handleKnowledgePromote)
	s.mux.HandleFunc("GET /api/knowledge/subscriptions", s.handleKnowledgeSubsList)
	s.mux.HandleFunc("POST /api/knowledge/subscriptions", s.handleKnowledgeSubsAdd)
	s.mux.HandleFunc("DELETE /api/knowledge/subscriptions", s.handleKnowledgeSubsRemove)
	s.mux.HandleFunc("PUT /api/knowledge/{layer}/{slug}", s.handleKnowledgeUpdate)
	s.mux.HandleFunc("DELETE /api/knowledge/{layer}/{slug}", s.handleKnowledgeDelete)
	s.mux.HandleFunc("GET /api/knowledge/{layer}", s.handleKnowledgeLayer)
	s.mux.HandleFunc("GET /api/knowledge/{layer}/{slug}", s.handleKnowledgeFact)
	s.mux.HandleFunc("PUT /api/knowledge/enabled", s.handleKnowledgeToggle)
	s.mux.HandleFunc("GET /api/knowledge/vaults", s.handleVaultsList)
	s.mux.HandleFunc("POST /api/knowledge/vaults", s.handleVaultsConnect)
	s.mux.HandleFunc("DELETE /api/knowledge/vaults", s.handleVaultsDisconnect)
	s.mux.HandleFunc("POST /api/knowledge/vaults/reindex", s.handleVaultsReindex)
	s.mux.HandleFunc("GET /api/knowledge/vaults/{name}/facts", s.handleVaultFacts)
	s.mux.HandleFunc("GET /api/knowledge/git-sources", s.handleGitSourcesList)
	s.mux.HandleFunc("POST /api/knowledge/git-sources", s.handleGitSourcesConnect)
	s.mux.HandleFunc("DELETE /api/knowledge/git-sources", s.handleGitSourcesDisconnect)
	s.mux.HandleFunc("POST /api/knowledge/obsidian/sync", s.handleObsidianSync)

	s.mux.HandleFunc("GET /api/hive-id", s.handleHiveIDGet)
	s.mux.HandleFunc("PUT /api/hive-id", s.handleHiveIDSet)

	s.mux.HandleFunc("POST /api/inception/start", s.handleInceptionStart)
	s.mux.HandleFunc("POST /api/inception/scan", s.handleInceptionScan)
	s.mux.HandleFunc("GET /api/inception/state", s.handleInceptionState)
	s.mux.HandleFunc("POST /api/inception/questions", s.handleInceptionSetQuestions)
	s.mux.HandleFunc("POST /api/inception/answer", s.handleInceptionAnswer)
	s.mux.HandleFunc("POST /api/inception/facts", s.handleInceptionRecordFacts)
	s.mux.HandleFunc("GET /api/inception/scaffold", s.handleInceptionScaffold)
	s.mux.HandleFunc("POST /api/inception/approve", s.handleInceptionApprove)
	s.mux.HandleFunc("POST /api/inception/reset", s.handleInceptionReset)
	s.mux.HandleFunc("GET /api/inception/ideation-facts", s.handleInceptionIdeationFacts)
	s.mux.HandleFunc("GET /api/inception/download", s.handleInceptionDownload)
	s.mux.HandleFunc("GET /api/inception/has-files", s.handleInceptionHasFiles)
	s.mux.HandleFunc("PUT /api/inception/wiki-name", s.handleInceptionRenameWiki)
	s.mux.HandleFunc("POST /api/inception/import", s.handleInceptionImport)

	s.mux.HandleFunc("POST /api/chat", s.handleChat)

	s.mux.HandleFunc("GET /api/nous/status", s.handleNousStatus)
	s.mux.HandleFunc("GET /api/nous/ledger", s.handleNousLedger)
	s.mux.HandleFunc("GET /api/nous/principles", s.handleNousPrinciples)
	s.mux.HandleFunc("POST /api/nous/approve", s.handleNousApprove)
	s.mux.HandleFunc("POST /api/nous/abort", s.handleNousAbort)
	s.mux.HandleFunc("PUT /api/nous/mode", s.handleNousMode)
	s.mux.HandleFunc("PUT /api/nous/scope", s.handleNousScope)
	s.mux.HandleFunc("GET /api/nous/phase", s.handleNousPhase)
	s.mux.HandleFunc("PUT /api/nous/gate-decision", s.handleNousGateDecision)
	s.mux.HandleFunc("GET /api/nous/gate-pending", s.handleNousGatePending)
	s.mux.HandleFunc("POST /api/nous/gate-respond", s.handleNousGateRespond)
	s.mux.HandleFunc("GET /api/nous/gate-response", s.handleNousGateResponse)
	s.mux.HandleFunc("GET /api/nous/config", s.handleNousConfigGet)
	s.mux.HandleFunc("PUT /api/nous/config/goals", s.handleNousConfigGoals)
	s.mux.HandleFunc("PUT /api/nous/config/repos", s.handleNousConfigRepos)
	s.mux.HandleFunc("PUT /api/nous/config/output", s.handleNousConfigOutput)
	s.mux.HandleFunc("PUT /api/nous/config/fast-fail", s.handleNousConfigFastFail)
	s.mux.HandleFunc("PUT /api/nous/config/schedule", s.handleNousConfigSchedule)
	s.mux.HandleFunc("PUT /api/nous/config/controllables", s.handleNousConfigControllables)
	s.mux.HandleFunc("PUT /api/nous/config/principles", s.handleNousConfigPrinciples)
	s.mux.HandleFunc("DELETE /api/nous/principles/{id}", s.handleNousDeletePrinciple)

	s.mux.HandleFunc("GET /api/beads", s.handleBeadsList)
	s.mux.HandleFunc("GET /api/beads/{agent}", s.handleBeadsList)
	s.mux.HandleFunc("POST /api/beads/{agent}", s.handleBeadsCreate)
	s.mux.HandleFunc("POST /api/beads/reset", s.handleBeadsReset)
	s.mux.HandleFunc("POST /api/beads/reset/{agent}", s.handleBeadsResetAgent)

	s.mux.HandleFunc("GET /api/auth/token", s.handleAuthToken)
}

var (
	versionHash  = "unknown"
	versionShort = "unknown"
)

func SetGitVersion(hash, short string) {
	versionHash = hash
	versionShort = short
}

func jsonResponse(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Warn("jsonResponse encode failed", "error", err)
	}
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "error": msg}); err != nil {
		slog.Warn("jsonError encode failed", "error", err)
	}
}

func okResponse(w http.ResponseWriter, extra map[string]string) {
	result := map[string]interface{}{"ok": true}
	for k, v := range extra {
		result[k] = v
	}
	jsonResponse(w, result)
}

// resolveAgentParam resolves an agent path parameter (name or ID) to the
// canonical YAML key (name). Returns the resolved name.
func (s *Server) resolveAgentParam(nameOrID string) string {
	if s.deps != nil && s.deps.AgentMgr != nil {
		return s.deps.AgentMgr.ResolveAgent(nameOrID)
	}
	return nameOrID
}

func (s *Server) refreshAfterMutation() {
	if s.deps != nil && s.deps.RefreshFunc != nil {
		go s.deps.RefreshFunc()
	}
}

func (s *Server) persistAfterMutation() {
	if s.deps != nil && s.deps.PersistFunc != nil {
		go s.deps.PersistFunc()
	}
}

func (s *Server) refreshAndPersist() {
	s.refreshAfterMutation()
	s.persistAfterMutation()
}

func (s *Server) refreshAndPersistSync() {
	if s.deps != nil && s.deps.RefreshFunc != nil {
		s.deps.RefreshFunc()
	}
	if s.deps != nil && s.deps.PersistFunc != nil {
		s.deps.PersistFunc()
	}
}

// saveConfig persists the in-memory config to disk, skipping the next
// watcher reload to prevent the watcher from overwriting concurrent
// in-memory mutations with a stale file read.
func (s *Server) saveConfig() error {
	if s.deps == nil || s.deps.Config == nil || s.deps.Config.SourcePath == "" {
		return nil
	}
	if s.deps.SkipReloadFunc != nil {
		s.deps.SkipReloadFunc()
	}
	if err := s.deps.Config.Save(); err != nil {
		return err
	}
	if s.deps.Governor != nil {
		s.deps.Governor.UpdateConfig(s.deps.Config.Governor)
	}
	return nil
}

func (s *Server) persistOnly() {
	if s.deps != nil && s.deps.PersistFunc != nil {
		s.deps.PersistFunc()
	}
}

func (s *Server) refreshAsync() {
	if s.deps != nil && s.deps.RefreshFunc != nil {
		s.deps.RefreshFunc()
	}
}

const maxDecodeBodyBytes = 1 << 20 // 1 MiB

func decodeBody(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(nil, r.Body, maxDecodeBodyBytes)
	return json.NewDecoder(r.Body).Decode(v)
}

// htmlTagPattern matches HTML/XML tags for sanitization.
var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

// sanitizeString strips HTML tags and env var placeholders from user input
// to prevent stored XSS and environment variable injection on config reload.
func sanitizeString(s string) string {
	s = strings.TrimSpace(htmlTagPattern.ReplaceAllString(s, ""))
	s = envVarEscapePattern.ReplaceAllString(s, "")
	return s
}

var envVarEscapePattern = regexp.MustCompile(`\$\{[^}]*\}`)

var tokenRedactor = regexp.MustCompile(`(ghp_|gho_|ghs_|github_pat_)[A-Za-z0-9_]{10,}`)

func redactTokensInLine(s string) string {
	return tokenRedactor.ReplaceAllStringFunc(s, func(m string) string {
		if len(m) > 7 {
			return m[:7] + "***REDACTED***"
		}
		return "***REDACTED***"
	})
}

func sanitizeFilenameComponent(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, s)
}

// --- Core status endpoints ---

func (s *Server) handleRole(w http.ResponseWriter, r *http.Request) {
	role := r.Header.Get("X-Hive-Role")
	user := r.Header.Get("X-Hive-User")
	if user == "" {
		if cookie, err := r.Cookie("hive_hub_user"); err == nil && cookie.Value != "" {
			user = cookie.Value
		}
	}
	if role == "" {
		role = "owner"
	}
	jsonResponse(w, map[string]string{"role": role, "user": user})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"version": "2.0.0",
		"go":      "1.25",
		"hash":    versionHash,
		"short":   versionShort,
	}

	s.versionMu.RLock()
	cached := s.cachedLatestHash
	cacheAge := time.Since(s.cachedLatestAt)
	s.versionMu.RUnlock()

	const versionCacheTTL = 5 * time.Minute
	if cacheAge > versionCacheTTL || cached == "" {
		if latest, err := s.fetchLatestRemoteHash(); err == nil && latest != "" {
			s.versionMu.Lock()
			s.cachedLatestHash = latest
			s.cachedLatestAt = time.Now()
			s.versionMu.Unlock()
			cached = latest
		}
	}

	if cached != "" {
		latestShort := cached
		const shortHashLen = 7
		if len(latestShort) > shortHashLen {
			latestShort = latestShort[:shortHashLen]
		}
		// Only report "behind" if the container image exists on GHCR
		containerReady := ghcrTagExistsCached(latestShort)
		resp["latestHash"] = cached
		resp["latestShort"] = latestShort
		resp["behind"] = containerReady && cached != versionHash
	}

	jsonResponse(w, resp)
}

func (s *Server) fetchLatestRemoteHash() (string, error) {
	if s.deps == nil || s.deps.GHClient == nil {
		return "", fmt.Errorf("no github client")
	}
	ctx := s.deps.Ctx
	if ctx == nil {
		return "", fmt.Errorf("no context")
	}
	return s.deps.GHClient.LatestCommitHash(ctx, "kubestellar", "hive", "v2")
}

// ghcrTagExistsCached checks whether a container tag exists on ghcr.io/kubestellar/hive,
// caching the result to avoid repeated network calls on each version poll.
var (
	ghcrCacheMu     sync.RWMutex
	ghcrCacheResult = map[string]bool{}
	ghcrCacheExpiry = map[string]time.Time{}
)

const ghcrCacheTTL = 2 * time.Minute
const ghcrCheckTimeout = 5 * time.Second

func ghcrTagExistsCached(tag string) bool {
	ghcrCacheMu.RLock()
	if exp, ok := ghcrCacheExpiry[tag]; ok && time.Now().Before(exp) {
		result := ghcrCacheResult[tag]
		ghcrCacheMu.RUnlock()
		return result
	}
	ghcrCacheMu.RUnlock()

	result := ghcrTagExists(tag)
	ghcrCacheMu.Lock()
	ghcrCacheResult[tag] = result
	ghcrCacheExpiry[tag] = time.Now().Add(ghcrCacheTTL)
	ghcrCacheMu.Unlock()
	return result
}

func ghcrTagExists(tag string) bool {
	client := &http.Client{Timeout: ghcrCheckTimeout}
	tokenResp, err := client.Get("https://ghcr.io/token?scope=repository:kubestellar/hive:pull")
	if err != nil {
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

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config
	primaryRepo := cfg.Project.PrimaryRepo
	if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
		primaryRepo = cfg.Project.Repos[0]
	}
	if primaryRepo != "" && cfg.Project.Org != "" && !strings.Contains(primaryRepo, "/") {
		primaryRepo = cfg.Project.Org + "/" + primaryRepo
	}
	jsonResponse(w, map[string]interface{}{
		"org":              cfg.Project.Org,
		"repos":            cfg.Project.Repos,
		"ai_author":        cfg.Project.AIAuthor,
		"agents":           len(cfg.EnabledAgents()),
		"eval_interval_s":  cfg.Governor.EvalIntervalS,
		"primaryRepo":      primaryRepo,
		"hub_url":          cfg.Hub.URL,
		"hive_id":          cfg.HiveID,
	})
}

func (s *Server) handleConfigDownload(w http.ResponseWriter, r *http.Request) {
	role := r.Header.Get("X-Hive-Role")
	if role == "" {
		role = "owner"
	}
	if role != "owner" {
		http.Error(w, "owner access required", http.StatusForbidden)
		return
	}
	configPath := "/etc/hive/hive.yaml"
	if envCfg := os.Getenv("HIVE_CONFIG"); envCfg != "" {
		configPath = envCfg
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		http.Error(w, "config file not found", http.StatusNotFound)
		return
	}
	org := ""
	repo := ""
	if s.deps != nil && s.deps.Config != nil {
		org = s.deps.Config.Project.Org
		if len(s.deps.Config.Project.Repos) > 0 {
			repo = s.deps.Config.Project.Repos[0]
		}
	}
	timestamp := time.Now().Format("2006-01-02_150405")
	safeOrg := sanitizeFilenameComponent(org)
	safeRepo := sanitizeFilenameComponent(repo)
	filename := fmt.Sprintf("hive-%s-%s-%s.yaml", safeOrg, safeRepo, timestamp)
	w.Header().Set("Content-Type", "application/x-yaml")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Write(data)
}

func (s *Server) handleSelfUpgrade(w http.ResponseWriter, r *http.Request) {
	role := r.Header.Get("X-Hive-Role")
	if role == "" {
		role = "owner"
	}
	if role != "owner" {
		jsonError(w, "owner access required", http.StatusForbidden)
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config not loaded", http.StatusInternalServerError)
		return
	}
	hubURL := s.deps.Config.Hub.URL
	hiveID := s.deps.Config.HiveID
	if hubURL == "" || hiveID == "" {
		jsonError(w, "hub URL or hive ID not configured", http.StatusBadRequest)
		return
	}
	upgradeURL := hubURL + "/api/saas/hives/" + url.PathEscape(hiveID) + "/upgrade"

	cookie, _ := r.Cookie("hive_hub_user")
	const upgradeTimeout = 30 * time.Second
	client := &http.Client{Timeout: upgradeTimeout}
	req, err := http.NewRequest("POST", upgradeURL, nil)
	if err != nil {
		jsonError(w, "failed to create upgrade request", http.StatusInternalServerError)
		return
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://hive.kubestellar.io")

	resp, err := client.Do(req)
	if err != nil {
		s.logger.Warn("self-upgrade: hub request failed", "error", err)
		jsonError(w, "hub unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	const maxUpgradeResponseBytes = 1 << 16
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxUpgradeResponseBytes))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
}

func (s *Server) handleSnapshotAPI(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil || !s.deps.Config.Hub.AutoSnapshot {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"snapshots not enabled"}`))
		return
	}
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status == nil {
		http.Error(w, "no data yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	json.NewEncoder(w).Encode(status)
}

func (s *Server) handleSnapshotPage(w http.ResponseWriter, r *http.Request) {
	hubURL := "https://hive.kubestellar.io"
	if s.deps != nil && s.deps.Config != nil && s.deps.Config.Hub.URL != "" {
		hubURL = s.deps.Config.Hub.URL
	}

	cfg := s.deps.Config
	if s.deps == nil || cfg == nil || !cfg.Hub.AutoSnapshot {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="3;url=%s"><title>Hive</title>
<style>body{font-family:system-ui,sans-serif;background:#0a0a0a;color:#e0e0e0;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0}
.card{text-align:center;max-width:480px;padding:40px}.bee{font-size:3rem;margin-bottom:16px}h1{color:#f59e0b;margin:0 0 8px}p{color:#8b949e;line-height:1.6}a{color:#58a6ff}</style>
</head><body><div class="card"><div class="bee">🐝</div><h1>Hive</h1><p>AI Agent Orchestration for Open Source</p><p>Snapshot is not currently published for this hive.</p><p>Redirecting to <a href="%s">%s</a>...</p></div></body></html>`,
			hubURL, hubURL, hubURL)
		return
	}

	mode := r.URL.Query().Get("mode")
	if mode == "classic" {
		mode = "dark"
	}
	if mode != "dark" {
		mode = "light"
	}
	snapshotFile := fmt.Sprintf("/data/snapshots/snapshot-%s.html", mode)
	info, err := os.Stat(snapshotFile)
	intervalMin := cfg.Hub.SnapshotIntervalMin
	if intervalMin < 5 {
		intervalMin = 15
	}
	staleThreshold := time.Duration(intervalMin) * time.Minute
	needsRebuild := err != nil || time.Since(info.ModTime()) > staleThreshold

	if needsRebuild {
		s.buildSnapshot("/data/snapshots/snapshot-dark.html", "dark")
		s.buildSnapshot("/data/snapshots/snapshot-light.html", "light")
	}

	data, err := os.ReadFile(snapshotFile)
	if err != nil {
		http.Error(w, "snapshot not yet generated — try again in a moment", http.StatusServiceUnavailable)
		return
	}

	data = []byte(strings.Replace(string(data),
		`href="/live/hive/light"`,
		`href="/snapshot?mode=light"`, -1))
	data = []byte(strings.Replace(string(data),
		`href="/live/hive/dark"`,
		`href="/snapshot?mode=dark"`, -1))
	data = []byte(strings.Replace(string(data),
		`href="/live/hive"`,
		`href="/snapshot"`, -1))

	html := string(data)
	dashURL := ""
	if s.deps != nil && s.deps.Config != nil {
		dashURL = s.deps.Config.Hub.DashboardURL
	}
	if dashURL != "" {
		html = strings.ReplaceAll(html, `href="`+dashURL, `href="/snapshot`)
		html = strings.ReplaceAll(html, `action="`+dashURL, `action="/snapshot`)
		html = strings.ReplaceAll(html, dashURL, "/snapshot")
	}
	html = regexp.MustCompile(`href="https?://[^"]*\.hive\.kubestellar\.io[^"]*"`).ReplaceAllString(html, `href="/snapshot"`)
	html = regexp.MustCompile(`href="http://localhost:\d+[^"]*"`).ReplaceAllString(html, `href="/snapshot"`)
	html = regexp.MustCompile(`href="http://192\.168\.[^"]*"`).ReplaceAllString(html, `href="/snapshot"`)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Write([]byte(html))
}

func (s *Server) buildSnapshot(outputFile, mode string) {
	os.MkdirAll("/data/snapshots", 0o755)
	dashURL := fmt.Sprintf("http://localhost:%d", s.port)
	htmlSource := "/opt/hive/proxy/public/index.html"
	builderScript := "/opt/hive/dashboard/build-snapshot.mjs"
	cmd := exec.Command("node", builderScript,
		"--mode", mode,
		"--base-path", "/snapshot",
		"--html", htmlSource,
		dashURL, outputFile)
	cmd.Env = append(os.Environ(), "NODE_TLS_REJECT_UNAUTHORIZED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.logger.Warn("snapshot build failed", "error", err, "output", string(out))
	} else {
		s.logger.Info("snapshot built", "file", outputFile)
	}
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	history := s.deps.Governor.EvalHistory()

	seedData, err := os.ReadFile("/data/sparkline-history.json")
	if err == nil {
		var seed []json.RawMessage
		if json.Unmarshal(seedData, &seed) == nil && len(seed) > 0 {
			liveData, err := json.Marshal(history)
			if err != nil {
				jsonResponse(w, history)
				return
			}
			var liveEntries []json.RawMessage
			if json.Unmarshal(liveData, &liveEntries) != nil {
				jsonResponse(w, history)
				return
			}
			combined := append(seed, liveEntries...)
			jsonResponse(w, combined)
			return
		}
	}

	jsonResponse(w, history)
}

func (s *Server) handleTrends(w http.ResponseWriter, r *http.Request) {
	const hoursPerDay = 24
	const hoursPerWeek = 168

	rangeParam := r.URL.Query().Get("range")
	hours, _ := strconv.Atoi(r.URL.Query().Get("hours"))

	const maxTrendHours = 720 // 30 days
	switch rangeParam {
	case "week":
		hours = hoursPerWeek
	case "day":
		hours = hoursPerDay
	default:
		if hours <= 0 {
			hours = hoursPerDay
		} else if hours > maxTrendHours {
			hours = maxTrendHours
		}
	}

	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour)

	evals := s.deps.Governor.EvalHistory()
	filtered := make([]interface{}, 0)
	for _, e := range evals {
		if e.Timestamp > cutoff.UnixMilli() {
			filtered = append(filtered, e)
		}
	}

	// Include token sparkline history within the requested time range
	allTokenHistory := s.TokenSparklineHistory()
	tokenFiltered := make([]TokenSparklineEntry, 0)
	cutoffMs := cutoff.UnixMilli()
	for _, entry := range allTokenHistory {
		if entry.Timestamp > cutoffMs {
			tokenFiltered = append(tokenFiltered, entry)
		}
	}

	jsonResponse(w, map[string]interface{}{
		"evals":        filtered,
		"tokenHistory": tokenFiltered,
	})
}

func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	kicks := s.deps.Governor.KickHistory()

	evals := s.deps.Governor.EvalHistory()
	type timelineMode struct {
		T    int64  `json:"t"`
		Mode string `json:"mode"`
	}
	modes := make([]timelineMode, 0, len(evals))
	for _, e := range evals {
		modes = append(modes, timelineMode{
			T:    e.Timestamp,
			Mode: strings.ToLower(string(e.Mode)),
		})
	}

	seedData, err := os.ReadFile("/data/sparkline-history.json")
	if err == nil {
		var seed []json.RawMessage
		if json.Unmarshal(seedData, &seed) == nil && len(seed) > 0 {
			var seedModes []timelineMode
			for _, raw := range seed {
				var entry struct {
					T       int64  `json:"t"`
					GovMode string `json:"govMode"`
				}
				if json.Unmarshal(raw, &entry) == nil && entry.T > 0 {
					m := strings.ToLower(entry.GovMode)
					if m == "" {
						m = "idle"
					}
					seedModes = append(seedModes, timelineMode{T: entry.T, Mode: m})
				}
			}
			modes = append(seedModes, modes...)
		}
	}

	// If eval-based modes are empty, fall back to explicit mode history
	// so the timeline always shows at least the startup mode.
	if len(modes) == 0 {
		modeChanges := s.deps.Governor.ModeHistory()
		for _, mc := range modeChanges {
			modes = append(modes, timelineMode{
				T:    mc.Timestamp.UnixMilli(),
				Mode: strings.ToLower(string(mc.To)),
			})
		}
	}

	jsonResponse(w, map[string]interface{}{
		"kicks": kicks,
		"modes": modes,
	})
}

func (s *Server) handleWidget(w http.ResponseWriter, r *http.Request) {
	state := s.deps.Governor.GetState()
	statuses := s.deps.AgentMgr.AllStatuses()

	running := 0
	paused := 0
	for _, a := range statuses {
		switch a.State {
		case "running":
			running++
		case "paused":
			paused++
		}
	}

	jsonResponse(w, map[string]interface{}{
		"mode":     state.Mode,
		"issues":   state.QueueIssues,
		"prs":      state.QueuePRs,
		"running":  running,
		"paused":   paused,
		"last_eval": state.LastEval,
	})
}

func (s *Server) handlePane(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	const maxPaneLines = 1000
	if lines <= 0 {
		lines = 100
	} else if lines > maxPaneLines {
		lines = maxPaneLines
	}
	source := r.URL.Query().Get("source")

	var output []string
	var err error

	if source == "buffer" {
		output, err = s.deps.AgentMgr.GetBufferOutput(name, lines)
	} else {
		output, err = s.deps.AgentMgr.GetOutput(name, lines)
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}

	for i, line := range output {
		output[i] = redactTokensInLine(line)
	}

	jsonResponse(w, map[string]interface{}{
		"agent":  name,
		"lines":  output,
		"count":  len(output),
	})
}

// --- Agent control endpoints ---

func (s *Server) handleKick(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	var body struct {
		Prompt  string `json:"prompt"`
		Message string `json:"message"`
	}
	if err := decodeBody(r, &body); err != nil {
		s.deps.Logger.Debug("kick body decode failed, using auto-generated message", "agent", name, "error", err)
	}

	msg := body.Prompt
	if msg == "" {
		msg = body.Message
	}

	const maxKickPromptLen = 10000
	if len(msg) > maxKickPromptLen {
		jsonError(w, fmt.Sprintf("prompt too long (%d chars, max %d)", len(msg), maxKickPromptLen), http.StatusBadRequest)
		return
	}

	if msg == "" && s.deps.Scheduler != nil {
		msg = s.deps.Scheduler.BuildAgentMessage(name, nil, s.deps.Scheduler.GetLastActionable())
	}

	if err := s.deps.AgentMgr.SendKick(name, msg); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Governor.RecordKick(name)
	s.deps.Logger.Info("audit: agent kicked", "agent", name, "trigger", "dashboard-api")
	s.auditFromRequest(r, "kick", "", name)
	s.refreshAfterMutation()
	okResponse(w, map[string]string{"status": "kicked", "agent": name})
}

func (s *Server) handleSwitch(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	backend := sanitizeString(r.PathValue("backend"))

	if err := s.deps.AgentMgr.SetBackendOverride(name, backend); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Logger.Info("audit: backend switched", "agent", name, "backend", backend, "trigger", "dashboard-api")
	s.auditFromRequest(r, "switch_backend", auditDetail("backend", backend), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "switched", "agent": name, "backend": backend})
}

func (s *Server) handleModelSet(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	model := sanitizeString(r.PathValue("model"))

	if err := s.deps.AgentMgr.SetModelOverride(name, model); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Logger.Info("audit: model set", "agent", name, "model", model, "trigger", "dashboard-api")
	s.auditFromRequest(r, "set_model", auditDetail("model", model), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "model_set", "agent": name, "model": model})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))

	if err := s.deps.AgentMgr.Pause(name, "dashboard-api", "manual pause"); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.auditFromRequest(r, "pause", "", name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "paused", "agent": name})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))

	if err := s.deps.AgentMgr.Resume(s.deps.Ctx, name, "dashboard-api", "manual resume"); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.auditFromRequest(r, "resume", "", name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "resumed", "agent": name})
}

func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	dimension := r.PathValue("dimension")

	var body struct {
		Value string `json:"value"`
	}
	if err := decodeBody(r, &body); err != nil {
		s.deps.Logger.Debug("pin body decode failed, using current value", "agent", name, "error", err)
	}

	if body.Value == "" {
		proc, getErr := s.deps.AgentMgr.GetStatus(name)
		if getErr != nil || proc == nil {
			jsonError(w, "agent not found", http.StatusBadRequest)
			return
		}
		switch dimension {
		case "cli":
			body.Value = proc.Config.Backend
			if proc.BackendOverride != "" {
				body.Value = proc.BackendOverride
			}
		case "model":
			body.Value = proc.Config.Model
			if proc.ModelOverride != "" {
				body.Value = proc.ModelOverride
			}
		}
	}

	var err error
	switch dimension {
	case "cli":
		err = s.deps.AgentMgr.PinCLI(name, body.Value)
	case "model":
		err = s.deps.AgentMgr.PinModel(name, body.Value)
	default:
		jsonError(w, "dimension must be 'cli' or 'model'", http.StatusBadRequest)
		return
	}

	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Logger.Info("audit: agent pinned", "agent", name, "dimension", dimension, "value", body.Value, "trigger", "dashboard-api")
	s.auditFromRequest(r, "pin", auditDetail("dimension", dimension, "value", body.Value), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "pinned", "agent": name, "dimension": dimension, "value": body.Value})
}

func (s *Server) handleUnpin(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))
	dimension := r.PathValue("dimension")

	var err error
	switch dimension {
	case "cli":
		err = s.deps.AgentMgr.UnpinCLI(name)
	case "model":
		err = s.deps.AgentMgr.UnpinModel(name)
	default:
		jsonError(w, "dimension must be 'cli' or 'model'", http.StatusBadRequest)
		return
	}

	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Logger.Info("audit: agent unpinned", "agent", name, "dimension", dimension, "trigger", "dashboard-api")
	s.auditFromRequest(r, "unpin", auditDetail("dimension", dimension), name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "unpinned", "agent": name, "dimension": dimension})
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))

	// Serialize restart operations to prevent concurrent pause/resume cycles
	// from interfering through shared state (tmux server, config writes).
	s.restartMu.Lock()
	defer s.restartMu.Unlock()

	if err := s.deps.AgentMgr.Restart(s.deps.Ctx, name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Logger.Info("audit: agent restarted", "agent", name, "trigger", "dashboard-api")
	s.auditFromRequest(r, "restart", "", name)
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "restarted", "agent": name})
}

func (s *Server) handleResetRestarts(w http.ResponseWriter, r *http.Request) {
	name := s.resolveAgentParam(r.PathValue("agent"))

	if err := s.deps.AgentMgr.ResetRestartCount(name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.deps.Logger.Info("audit: restart count reset", "agent", name, "trigger", "dashboard-api")
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "reset", "agent": name})
}

// --- Token endpoints ---

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	if s.deps.Tokens == nil {
		jsonResponse(w, map[string]string{"status": "no_collector"})
		return
	}
	summary := s.deps.Tokens.Summary()
	if summary == nil {
		jsonResponse(w, map[string]interface{}{"total_tokens": 0, "sessions": []interface{}{}})
		return
	}
	jsonResponse(w, summary)
}

func (s *Server) handleIssueCosts(w http.ResponseWriter, r *http.Request) {
	if s.deps.Tokens == nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	jsonResponse(w, s.deps.Tokens.IssueCosts())
}

func (s *Server) handleModelAdvisor(w http.ResponseWriter, r *http.Request) {
	budget := s.deps.Governor.GetBudget()
	jsonResponse(w, map[string]interface{}{
		"budget":        budget,
		"recommendation": "Use haiku for simple tasks, sonnet for default, opus for complex refactors",
	})
}

func (s *Server) handleBudgetIgnoreGet(w http.ResponseWriter, r *http.Request) {
	budget := s.deps.Governor.GetBudget()
	jsonResponse(w, map[string]interface{}{
		"ignored": budget.IgnoredAgents,
	})
}

func (s *Server) handleBudgetIgnoreSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agents []string `json:"ignored"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.deps.Governor.SetBudgetIgnored(body.Agents)
	okResponse(w, map[string]string{"status": "updated"})
}

// --- GitHub endpoints ---

func (s *Server) handleGHAuth(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config.GitHub
	authType := "token"
	if cfg.AppID != 0 {
		authType = "app"
	}
	jsonResponse(w, map[string]interface{}{
		"ok":              true,
		"type":            authType,
		"app_id":          cfg.AppID,
		"installation_id": cfg.InstallationID,
	})
}

const userTokenPath = "/data/gh-user-token"

func (s *Server) handleGHUserAuthStatus(w http.ResponseWriter, r *http.Request) {
	tokenData, err := os.ReadFile(userTokenPath)
	if err != nil || len(strings.TrimSpace(string(tokenData))) == 0 {
		jsonResponse(w, map[string]interface{}{"logged_in": false})
		return
	}
	token := strings.TrimSpace(string(tokenData))
	user, err := github.ValidateToken(token)
	if err != nil {
		jsonResponse(w, map[string]interface{}{"logged_in": false, "error": "token expired or revoked"})
		return
	}
	jsonResponse(w, map[string]interface{}{"logged_in": true, "username": user.Login, "avatar_url": user.AvatarURL})
}

func (s *Server) handleGHUserAuthStart(w http.ResponseWriter, r *http.Request) {
	clientID := s.deps.Config.GitHub.OAuthClientID
	if clientID == "" {
		jsonError(w, "oauth_client_id not configured. Add 'oauth_client_id: Ov23ligE2p0gjXg6xAUf' to the github section of hive.yaml and restart the container. This is the public Hive GitHub App client ID used for the Device Flow login — no secret required.", http.StatusBadRequest)
		return
	}

	s.deviceFlowMu.Lock()
	defer s.deviceFlowMu.Unlock()

	state, err := github.StartDeviceFlow(clientID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.deviceFlowState = state
	jsonResponse(w, map[string]interface{}{
		"user_code":        state.UserCode,
		"verification_uri": state.VerificationURI,
		"expires_in":       state.ExpiresIn,
		"interval":         state.Interval,
	})
}

func (s *Server) handleGHUserAuthPoll(w http.ResponseWriter, r *http.Request) {
	s.deviceFlowMu.Lock()
	defer s.deviceFlowMu.Unlock()

	if s.deviceFlowState == nil {
		jsonError(w, "no device flow in progress — call /api/gh-user-auth/start first", http.StatusBadRequest)
		return
	}

	clientID := s.deps.Config.GitHub.OAuthClientID
	token, status, err := github.PollDeviceFlow(clientID, s.deviceFlowState.DeviceCode)
	if err != nil {
		s.deviceFlowState = nil
		jsonResponse(w, map[string]interface{}{"status": "error", "error": err.Error()})
		return
	}
	if status == "authorization_pending" {
		jsonResponse(w, map[string]interface{}{"status": "pending"})
		return
	}
	if status == "slow_down" {
		jsonResponse(w, map[string]interface{}{"status": "slow_down"})
		return
	}

	tmpTokenPath := userTokenPath + ".tmp"
	if err := os.WriteFile(tmpTokenPath, []byte(token), 0o600); err != nil {
		jsonError(w, "failed to save token: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := os.Rename(tmpTokenPath, userTokenPath); err != nil {
		jsonError(w, "failed to persist token: "+err.Error(), http.StatusInternalServerError)
		return
	}

	user, _ := github.ValidateToken(token)
	s.deviceFlowState = nil
	username := ""
	avatarURL := ""
	if user != nil {
		username = user.Login
		avatarURL = user.AvatarURL
	}
	s.deps.Logger.Info("GitHub user authenticated via device flow", "username", username)

	if s.deps.SetUserClient != nil {
		s.deps.SetUserClient(token)
	}

	jsonResponse(w, map[string]interface{}{"status": "complete", "username": username, "avatar_url": avatarURL})
}

func (s *Server) handleGHUserAuthLogout(w http.ResponseWriter, r *http.Request) {
	os.Remove(userTokenPath)
	s.deps.Logger.Info("GitHub user logged out")
	jsonResponse(w, map[string]interface{}{"status": "logged_out"})
}

// restoreGHUserSession loads a previously-saved GitHub user OAuth token from
// disk and calls SetUserClient so that advisory posting works immediately
// after a container restart, without requiring re-login via Device Flow.
func (s *Server) restoreGHUserSession() {
	if s.deps == nil || s.deps.SetUserClient == nil {
		return
	}

	tokenData, err := os.ReadFile(userTokenPath)
	if err != nil {
		return // no saved token — nothing to restore
	}

	token := strings.TrimSpace(string(tokenData))
	if token == "" {
		return
	}

	user, err := github.ValidateToken(token)
	if err != nil {
		s.deps.Logger.Warn("saved GitHub user token is invalid, removing", "error", err)
		os.Remove(userTokenPath)
		return
	}

	s.deps.SetUserClient(token)
	s.deps.Logger.Info("restored GitHub user session from disk", "username", user.Login)
}

func (s *Server) handleGHRateLimits(w http.ResponseWriter, r *http.Request) {
	if s.deps.GHClient == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	limits, err := s.deps.GHClient.RateLimits(s.deps.Ctx)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, limits)
}

func (s *Server) handleSummaries(w http.ResponseWriter, r *http.Request) {
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()

	if status == nil {
		jsonResponse(w, map[string]interface{}{"issues": []interface{}{}, "prs": []interface{}{}})
		return
	}

	allIssues := make([]any, 0)
	allPRs := make([]any, 0)
	for _, repo := range status.Repos {
		allIssues = append(allIssues, repo.ActionableIssues...)
		allPRs = append(allPRs, repo.OpenPrs...)
	}

	jsonResponse(w, map[string]interface{}{
		"issues": allIssues,
		"prs":    allPRs,
		"hold":   status.Hold.Items,
	})
}

// --- Agent config endpoints ---

func (s *Server) handleAgentConfigGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	proc, err := s.deps.AgentMgr.GetStatus(name)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}

	cli := agentCfg.Backend
	if proc.BackendOverride != "" {
		cli = proc.BackendOverride
	}
	model := agentCfg.Model
	if proc.ModelOverride != "" {
		model = proc.ModelOverride
	}

	// Use configured launch command if available; otherwise construct one
	launchCmd := agentCfg.LaunchCmd
	if launchCmd == "" {
		launchCmd = fmt.Sprintf("%s --model %s", cli, model)
		if cli == "claude" {
			launchCmd = fmt.Sprintf("claude --model %s --dangerously-skip-permissions", model)
		} else if cli == "copilot" {
			launchCmd = fmt.Sprintf("/usr/bin/copilot --allow-all --model %s", model)
		}
	}

	// Use configured display name if available
	displayName := agentCfg.DisplayName
	if displayName == "" {
		displayName = ""
	}

	// Stale timeout from config, default to 28800 (8 hours).
	// Must exceed the longest cadence interval to avoid false stale flags.
	const defaultStaleTimeoutS = 28800
	staleTimeout := agentCfg.StaleTimeout
	if staleTimeout == 0 {
		staleTimeout = defaultStaleTimeoutS
	}

	// Restart strategy from config, default to "immediate"
	restartStrategy := agentCfg.RestartStrategy
	if restartStrategy == "" {
		restartStrategy = "immediate"
	}

	// Cadences as seconds (int) — frontend expects numbers, not duration strings
	cadences := map[string]int64{}
	for modeName, modeCfg := range s.deps.Config.Governor.Modes {
		if c, ok := modeCfg.Cadences[name]; ok {
			if c == "pause" || c == "off" || c == "0" {
				cadences[modeName] = 0
			} else {
				d := parseCadenceDuration(c)
				cadences[modeName] = int64(d.Seconds())
			}
		}
	}

	// Per-mode models (empty strings = inherit from general)
	models := map[string]string{}
	for modeName := range s.deps.Config.Governor.Modes {
		models[modeName] = ""
	}

	lastPrompt := proc.LastKickMessage

	// Read restrictions from agent work dir files
	restrictions := s.loadAgentRestrictions(name)

	// Read prompt template from agent policy file
	promptTemplate := s.loadPromptTemplate(name)

	// Read stat sources from config
	stats := s.loadAgentStats(name)

	pipeline := s.getAgentPipeline(name)
	hooks := s.getAgentHooks(name)

	includeRepos := true
	if agentCfg.IncludeRepos != nil {
		includeRepos = *agentCfg.IncludeRepos
	}

	jsonResponse(w, map[string]interface{}{
		"general": map[string]interface{}{
			"launchCmd":       launchCmd,
			"displayName":     displayName,
			"description":     agentCfg.Description,
			"cliPinned":       agentCfg.CLIPinned || proc.PinnedCLI != "",
			"cliPinValue":     cli,
			"staleTimeout":    staleTimeout,
			"restartStrategy": restartStrategy,
			"model":           model,
			"clearOnKick":     agentCfg.ClearOnKick,
			"emoji":           agentCfg.Emoji,
			"color":           agentCfg.Color,
			"sortOrder":       agentCfg.SortOrder,
			"beadRole":        agentCfg.BeadRole,
			"role":            agentCfg.Role,
			"kickTemplate":    agentCfg.KickTemplate,
			"mode":            agentCfg.Mode,
			"includeRepos":    includeRepos,
			"laneKeywords":    agentCfg.LaneKeywords,
			"detectKeywords":  agentCfg.DetectKeywords,
			"aliases":         agentCfg.Aliases,
		},
		"cadences": cadences,
		"models":   models,
		"pipeline": pipeline,
		"hooks":    hooks,
		"restrictions":   restrictions,
		"stats":          stats,
		"prompt":         lastPrompt,
		"promptTemplate": promptTemplate,
	})
}

type restriction struct {
	Pattern string `json:"pattern"`
	Reason  string `json:"reason"`
	Source  string `json:"source"`
}

func (s *Server) loadAgentRestrictions(name string) map[string]interface{} {
	result := map[string]interface{}{
		"agent":  []any{},
		"global": []any{},
		"policy": []any{},
	}

	// Read global restrictions from /data/restrictions.conf (one pattern per line)
	globalRestrictions := []any{}
	if data, err := os.ReadFile("/data/restrictions.conf"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "|", 2)
			r := restriction{Pattern: parts[0], Source: "global"}
			if len(parts) > 1 {
				r.Reason = parts[1]
			}
			globalRestrictions = append(globalRestrictions, r)
		}
	}
	result["global"] = globalRestrictions

	// Read agent-specific restrictions
	agentRestrictions := []any{}
	agentRestFile := fmt.Sprintf("/data/agents/%s/restrictions.conf", name)
	if data, err := os.ReadFile(agentRestFile); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "|", 2)
			r := restriction{Pattern: parts[0], Source: "agent"}
			if len(parts) > 1 {
				r.Reason = parts[1]
			}
			agentRestrictions = append(agentRestrictions, r)
		}
	}
	result["agent"] = agentRestrictions

	// Read policy restrictions from agent policy file
	// Old hive extracts lines containing policy-relevant keywords, including
	// markdown-formatted lines with ** bold markers and numbered list items.
	policyRestrictions := []any{}
	policyContent := s.loadPromptTemplate(name)
	if policyContent != "" {
		content := policyContent
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			// Strip leading markdown list markers (-, *, numbered)
			stripped := line
			if len(stripped) > 2 && (stripped[0] == '-' || stripped[0] == '*') && stripped[1] == ' ' {
				stripped = strings.TrimSpace(stripped[2:])
			}
			// Strip bold markers for pattern matching
			plain := strings.ReplaceAll(stripped, "**", "")

			if strings.HasPrefix(plain, "NEVER ") ||
				strings.HasPrefix(plain, "Do not ") ||
				strings.HasPrefix(plain, "Do NOT ") ||
				strings.HasPrefix(plain, "ALWAYS ") ||
				strings.HasPrefix(plain, "Never ") ||
				strings.Contains(plain, "HARD RULE") ||
				strings.Contains(plain, "LANE BOUNDARY") ||
				strings.Contains(plain, "DO NOT") {
				// Truncate very long lines to keep the response manageable
				const maxPolicyLen = 200
				entry := stripped
				if runes := []rune(entry); len(runes) > maxPolicyLen {
					entry = string(runes[:maxPolicyLen]) + "..."
				}
				policyRestrictions = append(policyRestrictions, restriction{
					Pattern: entry,
					Source:  "policy",
				})
			}
		}
	}
	result["policy"] = policyRestrictions

	return result
}

func (s *Server) loadPromptTemplate(name string) string {
	templateName := ""
	if s.deps != nil && s.deps.Config != nil {
		if ac, ok := s.deps.Config.Agents[name]; ok && ac.KickTemplate != "" {
			templateName = ac.KickTemplate
		}
	}
	if templateName == "" {
		templateName = name + ".md"
	}

	paths := []string{
		fmt.Sprintf("/data/policies/examples/kubestellar/agents/%s", templateName),
	}
	if s.deps != nil && s.deps.Config != nil {
		policyDir := s.deps.Config.Policies.LocalDir
		if policyDir != "" {
			paths = append(paths,
				fmt.Sprintf("%s/examples/kubestellar/agents/%s", policyDir, templateName),
				fmt.Sprintf("%s/%s%s", policyDir, s.deps.Config.Policies.Path, templateName),
			)
		}
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			return s.substituteTemplateVars(string(data), name)
		}
	}
	if data, err := policies.DefaultPolicies.ReadFile("defaults/" + templateName); err == nil {
		return s.substituteTemplateVars(string(data), name)
	}
	return ""
}

// substituteTemplateVars replaces ${VAR} placeholders in a prompt template
// with values from the running config, so the dashboard shows resolved content
// instead of raw variable names.
func (s *Server) substituteTemplateVars(template, agentName string) string {
	if s.deps == nil || s.deps.Config == nil {
		return template
	}
	cfg := s.deps.Config
	org := cfg.Project.Org
	primaryRepo := cfg.Project.PrimaryRepo

	// Build full primary repo path (org/repo) if not already qualified
	fullPrimaryRepo := primaryRepo
	if org != "" && !strings.Contains(primaryRepo, "/") {
		fullPrimaryRepo = fmt.Sprintf("%s/%s", org, primaryRepo)
	}

	reposList := strings.Join(cfg.Project.Repos, ", ")

	displayName := agentName
	if ac, ok := cfg.Agents[agentName]; ok && ac.DisplayName != "" {
		displayName = ac.DisplayName
	}

	replacer := strings.NewReplacer(
		"${AGENT_NAME}", agentName,
		"${AGENT_DISPLAY_NAME}", displayName,
		"${PROJECT_NAME}", cfg.Project.Name,
		"${PROJECT_ORG}", org,
		"${PROJECT_PRIMARY_REPO}", fullPrimaryRepo,
		"${PROJECT_AI_AUTHOR}", cfg.Project.AIAuthor,
		"${PROJECT_REPOS_LIST}", reposList,
		"${HIVE_REPO}", fmt.Sprintf("%s/hive", org),
		"${HIVE_ID}", cfg.HiveID,
	)
	return replacer.Replace(template)
}

func (s *Server) loadAgentStats(name string) []any {
	statsFile := fmt.Sprintf("/data/agents/%s/stats.json", name)
	data, err := os.ReadFile(statsFile)
	if err == nil {
		var wrapper struct {
			Stats []any `json:"stats"`
		}
		if json.Unmarshal(data, &wrapper) == nil && len(wrapper.Stats) > 0 {
			return wrapper.Stats
		}
		var stats []any
		if json.Unmarshal(data, &stats) == nil && len(stats) > 0 {
			return stats
		}
	}
	return defaultStatsConfig(name)
}

func (s *Server) handleAgentConfigGeneral(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body map[string]interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if err := validateAgentGeneralInput(body); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	agentCfg := s.deps.Config.Agents[name]
	if v, ok := body["enabled"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.Enabled = b
		}
	}
	if v, ok := body["clearOnKick"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.ClearOnKick = b
		}
	}
	if v, ok := body["displayName"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.DisplayName = sanitizeString(s)
		}
	}
	if v, ok := body["description"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.Description = sanitizeString(s)
		}
	}
	if v, ok := body["launchCmd"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.LaunchCmd = sanitizeString(s)
		}
	}
	if v, ok := body["staleTimeout"]; ok {
		if f, ok := v.(float64); ok {
			agentCfg.StaleTimeout = int(f)
		}
	}
	if v, ok := body["restartStrategy"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.RestartStrategy = sanitizeString(s)
		}
	}
	if v, ok := body["cliPinned"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.CLIPinned = b
		}
	}
	if v, ok := body["emoji"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.Emoji = sanitizeString(s)
		}
	}
	if v, ok := body["color"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.Color = sanitizeString(s)
		}
	}
	if v, ok := body["sortOrder"]; ok {
		if f, ok := v.(float64); ok {
			agentCfg.SortOrder = int(f)
		}
	}
	if v, ok := body["beadRole"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.BeadRole = sanitizeString(s)
		}
	}
	if v, ok := body["role"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.Role = sanitizeString(s)
		}
	}
	if v, ok := body["kickTemplate"]; ok {
		if s, ok := v.(string); ok {
			agentCfg.KickTemplate = sanitizeString(s)
		}
	}
	if v, ok := body["mode"]; ok {
		if s, ok := v.(string); ok {
			s = sanitizeString(s)
			if s != "" {
				validModes := map[string]bool{"ADVISORY": true, "ISSUES_ONLY": true, "ISSUES_AND_PRS": true, "ISSUES_PRS_MERGE": true, "NO_GITHUB": true}
				if !validModes[s] {
					jsonError(w, "mode must be one of: ADVISORY, ISSUES_ONLY, ISSUES_AND_PRS, ISSUES_PRS_MERGE, NO_GITHUB", http.StatusBadRequest)
					return
				}
			}
			agentCfg.Mode = s
		}
	}
	if v, ok := body["includeRepos"]; ok {
		if b, ok := v.(bool); ok {
			agentCfg.IncludeRepos = &b
		}
	}
	if v, ok := body["laneKeywords"]; ok {
		if arr, ok := v.([]interface{}); ok {
			kw := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					kw = append(kw, sanitizeString(s))
				}
			}
			agentCfg.LaneKeywords = kw
		}
	}
	if v, ok := body["detectKeywords"]; ok {
		if arr, ok := v.([]interface{}); ok {
			kw := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					kw = append(kw, sanitizeString(s))
				}
			}
			agentCfg.DetectKeywords = kw
		}
	}
	if v, ok := body["aliases"]; ok {
		if arr, ok := v.([]interface{}); ok {
			a := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					a = append(a, s)
				}
			}
			agentCfg.Aliases = a
		}
	}
	s.deps.Config.Agents[name] = agentCfg

	// Sync the updated config into the agent process so that status builders
	// (which read from AgentProcess.Config, not the global config map) reflect
	// changes like display_name immediately.
	if err := s.deps.AgentMgr.UpdateConfig(name, agentCfg); err != nil {
		s.logger.Warn("failed to sync agent config to process", "agent", name, "error", err)
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after agent update", "agent", name, "error", err)
	}

	s.deps.AgentMgr.SyncModeFiles(s.deps.AgentMgr.GetACMMLevel())
	if agentsDir := s.deps.Config.Data.AgentsDir; agentsDir != "" {
		if err := config.SaveAgentFile(agentsDir, name, agentCfg); err != nil {
			s.logger.Error("failed to persist agent overlay after update", "agent", name, "error", err)
		}
	}
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigCadences(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body map[string]int64
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	for modeName, seconds := range body {
		mode, ok := s.deps.Config.Governor.Modes[modeName]
		if !ok {
			continue
		}
		if mode.Cadences == nil {
			mode.Cadences = make(map[string]string)
		}
		if seconds <= 0 {
			mode.Cadences[name] = "pause"
		} else {
			mode.Cadences[name] = formatCadenceDuration(seconds)
		}
		s.deps.Config.Governor.Modes[modeName] = mode
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after cadence update", "agent", name, "error", err)
	}
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigModels(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var body struct {
		Backend string `json:"backend"`
		Model   string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	if body.Backend != "" {
		agentCfg.Backend = sanitizeString(body.Backend)
	}
	if body.Model != "" {
		agentCfg.Model = sanitizeString(body.Model)
	}
	s.deps.Config.Agents[name] = agentCfg

	// Sync updated backend/model into the agent process so status builders
	// reflect the change without requiring a restart.
	if err := s.deps.AgentMgr.UpdateConfig(name, agentCfg); err != nil {
		s.logger.Warn("failed to sync agent config to process", "agent", name, "error", err)
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after model update", "agent", name, "error", err)
	}
	if agentsDir := s.deps.Config.Data.AgentsDir; agentsDir != "" {
		if err := config.SaveAgentFile(agentsDir, name, agentCfg); err != nil {
			s.logger.Error("failed to persist agent overlay after model update", "agent", name, "error", err)
		}
	}
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigPipeline(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body map[string]bool
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.pipelineMu.Lock()
	s.agentPipelines[name] = body
	s.pipelineMu.Unlock()

	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigHooks(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body map[string][]any
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.hooksMu.Lock()
	s.agentHooks[name] = body
	s.hooksMu.Unlock()

	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigRestrictions(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Agent []restriction `json:"agent"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	restFile := fmt.Sprintf("/data/agents/%s/restrictions.conf", name)
	_ = os.MkdirAll(fmt.Sprintf("/data/agents/%s", name), 0o755)

	var lines []string
	for _, r := range body.Agent {
		pattern := strings.ReplaceAll(r.Pattern, "\n", "")
		pattern = strings.ReplaceAll(pattern, "\r", "")
		reason := strings.ReplaceAll(r.Reason, "\n", "")
		reason = strings.ReplaceAll(reason, "\r", "")
		line := pattern
		if reason != "" {
			line += "|" + reason
		}
		lines = append(lines, line)
	}
	tmpRest := restFile + ".tmp"
	if err := os.WriteFile(tmpRest, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		s.logger.Warn("rest file write failed", "error", err)
	} else if err := os.Rename(tmpRest, restFile); err != nil {
		s.logger.Warn("rest file rename failed", "error", err)
	}

	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentConfigStats(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Stats []any `json:"stats"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	statsFile := fmt.Sprintf("/data/agents/%s/stats.json", name)
	_ = os.MkdirAll(fmt.Sprintf("/data/agents/%s", name), 0o755)

	data, err := json.Marshal(body)
	if err == nil {
		tmpStats := statsFile + ".tmp"
		if err := os.WriteFile(tmpStats, data, 0o644); err != nil {
			s.logger.Warn("stats file write failed", "error", err)
		} else if err := os.Rename(tmpStats, statsFile); err != nil {
			s.logger.Warn("stats file rename failed", "error", err)
		}
	}

	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "agent": name})
}

func (s *Server) handleAgentPrompt(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	rawParam := r.URL.Query().Get("raw")
	var template string
	if rawParam == "1" {
		template = s.loadPromptTemplateRaw(name)
	} else {
		template = s.loadPromptTemplate(name)
	}

	const repoBaseURL = "https://github.com/kubestellar/hive/blob/v2/"
	sourceFiles := []map[string]string{}

	templateName := ""
	if ac, ok := s.deps.Config.Agents[name]; ok && ac.KickTemplate != "" {
		templateName = ac.KickTemplate
	}
	if templateName == "" {
		templateName = name + ".md"
	}

	sourceFiles = append(sourceFiles, map[string]string{
		"label": "Kick template",
		"path":  "v2/pkg/policies/defaults/" + templateName,
		"url":   repoBaseURL + "v2/pkg/policies/defaults/" + templateName,
		"note":  "kick_template: " + templateName,
	})

	jsonResponse(w, map[string]interface{}{
		"agent":       name,
		"prompt":      template,
		"sourceFiles": sourceFiles,
	})
}

// loadPromptTemplateRaw returns the raw template content without variable substitution.
func (s *Server) loadPromptTemplateRaw(name string) string {
	templateName := ""
	if s.deps != nil && s.deps.Config != nil {
		if ac, ok := s.deps.Config.Agents[name]; ok && ac.KickTemplate != "" {
			templateName = ac.KickTemplate
		}
	}
	if templateName == "" {
		templateName = name + ".md"
	}

	paths := []string{
		fmt.Sprintf("/data/policies/examples/kubestellar/agents/%s", templateName),
	}
	if s.deps != nil && s.deps.Config != nil {
		policyDir := s.deps.Config.Policies.LocalDir
		if policyDir != "" {
			paths = append(paths,
				fmt.Sprintf("%s/examples/kubestellar/agents/%s", policyDir, templateName),
				fmt.Sprintf("%s/%s%s", policyDir, s.deps.Config.Policies.Path, templateName),
			)
		}
	}
	// Check /data/policies first (user-saved templates)
	userPath := fmt.Sprintf("/data/policies/%s", templateName)
	if data, err := os.ReadFile(userPath); err == nil {
		return string(data)
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			return string(data)
		}
	}
	if data, err := policies.DefaultPolicies.ReadFile("defaults/" + templateName); err == nil {
		return string(data)
	}
	return ""
}

const promptTemplateSaveDir = "/data/policies"

func (s *Server) handleAgentPromptSave(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	var body struct {
		Template string `json:"template"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}

	templateFileName := name + ".md"
	if ac, ok := s.deps.Config.Agents[name]; ok && ac.KickTemplate != "" {
		templateFileName = ac.KickTemplate
	}

	if err := os.MkdirAll(promptTemplateSaveDir, 0o755); err != nil {
		jsonError(w, "failed to create policies directory: "+err.Error(), http.StatusInternalServerError)
		return
	}
	savePath := filepath.Join(promptTemplateSaveDir, templateFileName)
	if err := os.WriteFile(savePath, []byte(body.Template), 0o644); err != nil {
		jsonError(w, "failed to save template: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Point the agent's KickTemplate to the saved file
	agentCfg := s.deps.Config.Agents[name]
	agentCfg.KickTemplate = templateFileName
	s.deps.Config.Agents[name] = agentCfg

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after template save", "agent", name, "error", err)
	}
	s.refreshAndPersist()

	s.logger.Info("prompt template saved", "agent", name, "path", savePath)
	okResponse(w, map[string]string{"status": "saved", "path": savePath})
}

const exportAPIVersion = "hive.kubestellar.io/v1"
const exportKind = "AgentDefinition"

func (s *Server) handleAgentExport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	agentCfg, ok := s.deps.Config.Agents[name]
	if !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	rawTemplate := s.loadPromptTemplateRaw(name)

	includeRepos := true
	if agentCfg.IncludeRepos != nil {
		includeRepos = *agentCfg.IncludeRepos
	}

	// Collect cadences from governor modes
	cadences := map[string]string{}
	for modeName, modeCfg := range s.deps.Config.Governor.Modes {
		if c, ok := modeCfg.Cadences[name]; ok {
			cadences[modeName] = c
		}
	}

	yamlContent := s.buildExportYAML(name, agentCfg, cadences, includeRepos, rawTemplate)

	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "text/yaml") || strings.Contains(accept, "application/yaml") {
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.yaml", name))
		w.Write([]byte(yamlContent))
		return
	}

	jsonResponse(w, map[string]interface{}{
		"name": name,
		"yaml": yamlContent,
	})
}

func (s *Server) buildExportYAML(name string, cfg config.AgentConfig, cadences map[string]string, includeRepos bool, promptTemplate string) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("apiVersion: %s\n", exportAPIVersion))
	b.WriteString(fmt.Sprintf("kind: %s\n", exportKind))
	b.WriteString("metadata:\n")
	b.WriteString(fmt.Sprintf("  name: %s\n", name))
	if cfg.DisplayName != "" {
		b.WriteString(fmt.Sprintf("  displayName: %q\n", cfg.DisplayName))
	}
	if cfg.Description != "" {
		b.WriteString(fmt.Sprintf("  description: %q\n", cfg.Description))
	}
	if cfg.Emoji != "" {
		b.WriteString(fmt.Sprintf("  emoji: %q\n", cfg.Emoji))
	}
	if cfg.Color != "" {
		b.WriteString(fmt.Sprintf("  color: %q\n", cfg.Color))
	}
	b.WriteString("spec:\n")
	b.WriteString(fmt.Sprintf("  backend: %s\n", valueOrDefault(cfg.Backend, "copilot")))
	if cfg.Model != "" {
		b.WriteString(fmt.Sprintf("  model: %s\n", cfg.Model))
	}
	if cfg.Role != "" {
		b.WriteString(fmt.Sprintf("  role: %s\n", cfg.Role))
	}
	if cfg.Mode != "" {
		b.WriteString(fmt.Sprintf("  mode: %s\n", cfg.Mode))
	}
	b.WriteString(fmt.Sprintf("  sortOrder: %d\n", cfg.SortOrder))
	b.WriteString(fmt.Sprintf("  beadRole: %s\n", cfg.GetBeadRole()))
	b.WriteString(fmt.Sprintf("  staleTimeout: %d\n", cfg.StaleTimeout))
	b.WriteString(fmt.Sprintf("  restartStrategy: %s\n", valueOrDefault(cfg.RestartStrategy, "immediate")))
	b.WriteString(fmt.Sprintf("  clearOnKick: %t\n", cfg.ClearOnKick))
	b.WriteString(fmt.Sprintf("  includeRepos: %t\n", includeRepos))

	if len(cfg.LaneKeywords) > 0 {
		b.WriteString("  laneKeywords:\n")
		for _, kw := range cfg.LaneKeywords {
			b.WriteString(fmt.Sprintf("    - %s\n", kw))
		}
	}
	if len(cfg.DetectKeywords) > 0 {
		b.WriteString("  detectKeywords:\n")
		for _, kw := range cfg.DetectKeywords {
			b.WriteString(fmt.Sprintf("    - %s\n", kw))
		}
	}
	if len(cfg.Aliases) > 0 {
		b.WriteString("  aliases:\n")
		for _, a := range cfg.Aliases {
			b.WriteString(fmt.Sprintf("    - %s\n", a))
		}
	}

	if len(cadences) > 0 {
		b.WriteString("  cadences:\n")
		modeOrder := []string{"idle", "quiet", "busy", "surge"}
		for _, mode := range modeOrder {
			if c, ok := cadences[mode]; ok {
				b.WriteString(fmt.Sprintf("    %s: %s\n", mode, c))
			}
		}
	}

	if promptTemplate != "" {
		b.WriteString("  promptTemplate: |\n")
		for _, line := range strings.Split(promptTemplate, "\n") {
			b.WriteString("    " + line + "\n")
		}
	}

	return b.String()
}

func valueOrDefault(v, dflt string) string {
	if v != "" {
		return v
	}
	return dflt
}

func (s *Server) handleStatSources(w http.ResponseWriter, r *http.Request) {
	sources := map[string]any{
		"status": map[string]any{
			"label":  "Repo Status",
			"fields": []string{"actionableCount", "openPrCount", "mergeableCount"},
		},
		"health": map[string]any{
			"label": "Health Checks",
			"fields": []string{
				"brew", "helm", "ci", "weekly", "nightly",
				"nightlyCompliance", "nightlyDashboard", "nightlyGhaw",
				"nightlyPlaywright", "nightlyRel", "weeklyRel",
				"deploy_vllm_d", "deploy_pok_prod",
			},
		},
		"agentMetrics": map[string]any{
			"label": "Agent Metrics",
			"fields": []string{
				"stars", "forks", "contributors", "adopters", "acmm",
				"outreachOpen", "outreachMerged", "coverage", "prs", "closed",
			},
		},
		"tokens": map[string]any{
			"label":  "Token Usage",
			"fields": []string{"input", "output", "cacheRead", "cacheCreate", "sessions", "messages"},
		},
	}
	styles := []string{"number", "dot", "pct", "pct-bar", "spark"}
	jsonResponse(w, map[string]any{"sources": sources, "styles": styles})
}

// --- Governor config endpoints ---

func (s *Server) handleGovernorConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config

	// Build agents list
	agents := make([]string, 0, len(cfg.Agents))
	for name := range cfg.Agents {
		agents = append(agents, name)
	}

	// Extract thresholds from modes (exclude idle which is always 0)
	thresholds := map[string]int{}
	for modeName, mode := range cfg.Governor.Modes {
		if modeName != "idle" {
			thresholds[modeName] = mode.Threshold
		}
	}

	// Build full org/repo paths
	org := cfg.Project.Org
	repos := make([]string, 0, len(cfg.Project.Repos))
	for _, repo := range cfg.Project.Repos {
		if strings.Contains(repo, "/") {
			repos = append(repos, repo)
		} else {
			repos = append(repos, org+"/"+repo)
		}
	}

	// Build notifications — mask sensitive values like the old hive does
	notifications := map[string]interface{}{
		"ntfyServer":     "",
		"ntfyTopic":      "",
		"discordWebhook": "",
		"hasNtfy":        false,
		"hasDiscord":     false,
	}
	if cfg.Notifications.Ntfy != nil {
		notifications["ntfyServer"] = cfg.Notifications.Ntfy.Server
		notifications["ntfyTopic"] = cfg.Notifications.Ntfy.Topic
		notifications["hasNtfy"] = cfg.Notifications.Ntfy.Server != ""
	}
	if cfg.Notifications.Discord != nil {
		notifications["discordWebhook"] = maskSecret(cfg.Notifications.Discord.Webhook)
		notifications["hasDiscord"] = cfg.Notifications.Discord.Webhook != ""
	}

	primaryRepo := cfg.Project.PrimaryRepo
	if primaryRepo != "" && org != "" && !strings.Contains(primaryRepo, "/") {
		primaryRepo = org + "/" + primaryRepo
	}

	jsonResponse(w, map[string]interface{}{
		"agents":      agents,
		"thresholds":  thresholds,
		"labels":      cfg.Governor.Labels.Exempt,
		"holdLabels":  github.HoldLabels,
		"repos":       repos,
		"primaryRepo": primaryRepo,
		"budget": map[string]interface{}{
			"totalTokens": cfg.Governor.Budget.TotalTokens,
			"periodDays":  cfg.Governor.Budget.PeriodDays,
			"criticalPct": cfg.Governor.Budget.CriticalPct,
		},
		"notifications": notifications,
		"health": map[string]interface{}{
			"healthcheckInterval": cfg.Governor.Health.HealthcheckInterval,
			"restartCooldown":     cfg.Governor.Health.RestartCooldown,
			"modelLock":           cfg.Governor.Health.ModelLock,
		},
		"sensing": map[string]interface{}{
			"ghRatePatterns":     cfg.Governor.Sensing.GHRatePatterns,
			"cliExcludePatterns": cfg.Governor.Sensing.CLIExcludePatterns,
			"loginPatterns":      cfg.Governor.Sensing.LoginPatterns,
			"ttlSeconds":         cfg.Governor.Sensing.TTLSeconds,
			"pullbackSeconds":    cfg.Governor.Sensing.PullbackSeconds,
		},
		"logging": map[string]interface{}{
			"dir":        cfg.Governor.Logging.Dir,
			"maxSizeMB":  cfg.Governor.Logging.MaxSizeMB,
			"maxAgeDays": cfg.Governor.Logging.MaxAgeDays,
			"maxBackups": cfg.Governor.Logging.MaxBackups,
			"compress":   cfg.Governor.Logging.Compress,
			"level":      cfg.Governor.Logging.Level,
		},
		"hub": map[string]interface{}{
			"enabled":                  cfg.Hub.Enabled,
			"url":                      cfg.Hub.URL,
			"dashboard_url":            cfg.Hub.DashboardURL,
			"snapshot_url":             cfg.Hub.SnapshotURL,
			"is_public":               cfg.Hub.IsPublic,
			"auto_snapshot":           cfg.Hub.AutoSnapshot,
			"snapshot_interval_min":   cfg.Hub.SnapshotIntervalMin,
			"contribute_suspended":             cfg.Hub.ContributeSuspended,
			"contribute_allow_labels":          cfg.Hub.ContributeAllowLabels,
			"contribute_deny_labels":           cfg.Hub.ContributeDenyLabels,
			"contribute_deny_titles":           cfg.Hub.ContributeDenyTitles,
			"contribute_deny_authors":          cfg.Hub.ContributeDenyAuthors,
			"contribute_allow_models":          cfg.Hub.ContributeAllowModels,
			"contribute_reject_unknown_models": cfg.Hub.ContributeRejectUnknownModels,
			"disabled_repos":                   cfg.Hub.DisabledRepos,
			"disabled_tiers":                   cfg.Hub.DisabledTiers,
			"tier_limits":                      cfg.Hub.TierLimits,
		},
	})
}

func (s *Server) handleGovernorSensing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EvalIntervalS      int      `json:"eval_interval_s"`
		GHRatePatterns     []string `json:"ghRatePatterns"`
		CLIExcludePatterns []string `json:"cliExcludePatterns"`
		LoginPatterns      []string `json:"loginPatterns"`
		TTLSeconds         int      `json:"ttlSeconds"`
		PullbackSeconds    int      `json:"pullbackSeconds"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	const minEvalIntervalS = 10    // 10 seconds minimum
	const maxEvalIntervalS = 86400 // 24 hours
	if body.EvalIntervalS != 0 {
		if body.EvalIntervalS < minEvalIntervalS || body.EvalIntervalS > maxEvalIntervalS {
			jsonError(w, fmt.Sprintf("eval_interval_s must be between %d and %d", minEvalIntervalS, maxEvalIntervalS), http.StatusBadRequest)
			return
		}
		s.deps.Config.Governor.EvalIntervalS = body.EvalIntervalS
	}
	if body.GHRatePatterns != nil {
		for _, p := range body.GHRatePatterns {
			if _, err := regexp.Compile(p); err != nil {
				jsonError(w, fmt.Sprintf("invalid ghRatePattern regex %q: %v", p, err), http.StatusBadRequest)
				return
			}
		}
		s.deps.Config.Governor.Sensing.GHRatePatterns = body.GHRatePatterns
	}
	if body.CLIExcludePatterns != nil {
		for _, p := range body.CLIExcludePatterns {
			if _, err := regexp.Compile(p); err != nil {
				jsonError(w, fmt.Sprintf("invalid cliExcludePattern regex %q: %v", p, err), http.StatusBadRequest)
				return
			}
		}
		s.deps.Config.Governor.Sensing.CLIExcludePatterns = body.CLIExcludePatterns
	}
	if body.LoginPatterns != nil {
		var filtered []string
		for _, p := range body.LoginPatterns {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if _, err := regexp.Compile(p); err != nil {
				jsonError(w, fmt.Sprintf("invalid login pattern regex %q: %v", p, err), http.StatusBadRequest)
				return
			}
			filtered = append(filtered, p)
		}
		s.deps.Config.Governor.Sensing.LoginPatterns = filtered
	}
	const maxTTLSeconds = 86400 // 24 hours
	if body.TTLSeconds != 0 {
		if body.TTLSeconds < 1 || body.TTLSeconds > maxTTLSeconds {
			jsonError(w, fmt.Sprintf("ttlSeconds must be between 1 and %d", maxTTLSeconds), http.StatusBadRequest)
			return
		}
		s.deps.Config.Governor.Sensing.TTLSeconds = body.TTLSeconds
	}
	const maxPullbackSeconds = 86400 // 24 hours
	if body.PullbackSeconds != 0 {
		if body.PullbackSeconds < 1 || body.PullbackSeconds > maxPullbackSeconds {
			jsonError(w, fmt.Sprintf("pullbackSeconds must be between 1 and %d", maxPullbackSeconds), http.StatusBadRequest)
			return
		}
		s.deps.Config.Governor.Sensing.PullbackSeconds = body.PullbackSeconds
	}

	if err := s.saveConfig(); err != nil { s.logger.Error("failed to persist config", "error", err) }
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorThresholds(w http.ResponseWriter, r *http.Request) {
	var body map[string]int
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if err := validateGovernorThresholds(body); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	for modeName, threshold := range body {
		if mode, ok := s.deps.Config.Governor.Modes[modeName]; ok {
			mode.Threshold = threshold
			s.deps.Config.Governor.Modes[modeName] = mode
		}
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after threshold update", "error", err)
	}

	// Trigger immediate governor re-evaluation so mode change is visible
	// without waiting for the next eval ticker (up to 5 minutes).
	if s.deps.EnumerateFunc != nil {
		go s.deps.EnumerateFunc()
	}

	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorLabels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Labels []string `json:"labels"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateGovernorLabels(body.Labels); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	filtered := make([]string, 0, len(body.Labels))
	for _, l := range body.Labels {
		isPermanent := false
		for _, h := range github.HoldLabels {
			if l == h {
				isPermanent = true
				break
			}
		}
		for _, p := range github.PermanentExemptLabels {
			if l == p {
				isPermanent = true
				break
			}
		}
		if !isPermanent {
			filtered = append(filtered, l)
		}
	}
	s.deps.Config.Governor.Labels.Exempt = filtered
	if s.deps.GHClient != nil {
		s.deps.GHClient.SetExemptLabels(filtered)
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after label update", "error", err)
	}
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorBudget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TotalTokens int64 `json:"totalTokens"`
		PeriodDays  int   `json:"periodDays"`
		CriticalPct int   `json:"criticalPct"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if err := validateGovernorBudget(body.TotalTokens, body.PeriodDays, body.CriticalPct); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if body.TotalTokens > 0 {
		s.deps.Config.Governor.Budget.TotalTokens = body.TotalTokens
		s.deps.Governor.SetBudgetLimit(body.TotalTokens)
	}
	if body.PeriodDays > 0 {
		s.deps.Config.Governor.Budget.PeriodDays = body.PeriodDays
	}
	if body.CriticalPct > 0 {
		s.deps.Config.Governor.Budget.CriticalPct = body.CriticalPct
	}

	if err := s.saveConfig(); err != nil { s.logger.Error("failed to persist config after budget update", "error", err) }
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorNotifications(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NtfyServer     string `json:"ntfyServer"`
		NtfyTopic      string `json:"ntfyTopic"`
		DiscordWebhook string `json:"discordWebhook"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateNotificationURL(body.NtfyServer, "ntfyServer"); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateNotificationURL(body.DiscordWebhook, "discordWebhook"); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	isMasked := func(v string) bool { return strings.HasPrefix(v, "•") }
	if (body.NtfyServer != "" && !isMasked(body.NtfyServer)) || (body.NtfyTopic != "" && !isMasked(body.NtfyTopic)) {
		if s.deps.Config.Notifications.Ntfy == nil {
			s.deps.Config.Notifications.Ntfy = &config.NtfyConfig{}
		}
		if body.NtfyServer != "" && !isMasked(body.NtfyServer) {
			s.deps.Config.Notifications.Ntfy.Server = body.NtfyServer
		}
		if body.NtfyTopic != "" && !isMasked(body.NtfyTopic) {
			s.deps.Config.Notifications.Ntfy.Topic = body.NtfyTopic
		}
	}
	if body.DiscordWebhook != "" && !isMasked(body.DiscordWebhook) {
		if s.deps.Config.Notifications.Discord == nil {
			s.deps.Config.Notifications.Discord = &config.DiscordConfig{}
		}
		s.deps.Config.Notifications.Discord.Webhook = body.DiscordWebhook
	}
	if err := s.saveConfig(); err != nil { s.logger.Error("failed to persist config after notification update", "error", err) }
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorHealth(w http.ResponseWriter, r *http.Request) {
	var body struct {
		HealthcheckInterval int   `json:"healthcheckInterval"`
		RestartCooldown     int   `json:"restartCooldown"`
		ModelLock           *bool `json:"modelLock"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateGovernorHealth(body.HealthcheckInterval, body.RestartCooldown); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if body.HealthcheckInterval > 0 {
		s.deps.Config.Governor.Health.HealthcheckInterval = body.HealthcheckInterval
	}
	if body.RestartCooldown > 0 {
		s.deps.Config.Governor.Health.RestartCooldown = body.RestartCooldown
	}
	if body.ModelLock != nil {
		s.deps.Config.Governor.Health.ModelLock = *body.ModelLock
	}
	if err := s.saveConfig(); err != nil { s.logger.Error("failed to persist config after health update", "error", err) }
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorLogging(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MaxSizeMB  int    `json:"maxSizeMB"`
		MaxAgeDays int    `json:"maxAgeDays"`
		MaxBackups int    `json:"maxBackups"`
		Compress   *bool  `json:"compress"`
		Level      string `json:"level"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateGovernorLogging(s.deps.Config.Governor.Logging.Dir, body.MaxSizeMB, body.MaxAgeDays); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	if body.MaxSizeMB > 0 {
		s.deps.Config.Governor.Logging.MaxSizeMB = body.MaxSizeMB
	}
	if body.MaxAgeDays > 0 {
		s.deps.Config.Governor.Logging.MaxAgeDays = body.MaxAgeDays
	}
	if body.MaxBackups > 0 {
		s.deps.Config.Governor.Logging.MaxBackups = body.MaxBackups
	}
	if body.Compress != nil {
		s.deps.Config.Governor.Logging.Compress = *body.Compress
	}
	if body.Level != "" {
		switch body.Level {
		case "debug", "info", "warn", "error":
			s.deps.Config.Governor.Logging.Level = body.Level
		default:
			jsonError(w, "level must be one of: debug, info, warn, error", http.StatusBadRequest)
			return
		}
	}
	if err := s.saveConfig(); err != nil { s.logger.Error("failed to persist config after logging update", "error", err) }
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorHub(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled                       *bool               `json:"enabled"`
		URL                           string              `json:"url"`
		DashboardURL                  string              `json:"dashboard_url"`
		SnapshotURL                   string              `json:"snapshot_url"`
		IsPublic                      *bool               `json:"is_public"`
		AutoSnapshot                  *bool               `json:"auto_snapshot"`
		ContributeSuspended           *bool               `json:"contribute_suspended"`
		ContributeAllowLabels         []string            `json:"contribute_allow_labels"`
		ContributeDenyLabels          []string            `json:"contribute_deny_labels"`
		ContributeDenyTitles          []string            `json:"contribute_deny_titles"`
		ContributeDenyAuthors         []string            `json:"contribute_deny_authors"`
		ContributeAllowModels         []string            `json:"contribute_allow_models"`
		ContributeRejectUnknownModels *bool               `json:"contribute_reject_unknown_models"`
		DisabledRepos                 []string            `json:"disabled_repos"`
		DisabledTiers                 []string            `json:"disabled_tiers"`
		TierLimits                    map[string]config.TierRate `json:"tier_limits"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	if body.Enabled != nil {
		cfg.Hub.Enabled = *body.Enabled
	}
	if body.URL != "" {
		cfg.Hub.URL = body.URL
	}
	if body.DashboardURL != "" {
		cfg.Hub.DashboardURL = body.DashboardURL
	}
	cfg.Hub.SnapshotURL = body.SnapshotURL
	if body.IsPublic != nil {
		cfg.Hub.IsPublic = *body.IsPublic
	}
	if body.AutoSnapshot != nil {
		cfg.Hub.AutoSnapshot = *body.AutoSnapshot
	}
	if body.ContributeSuspended != nil {
		cfg.Hub.ContributeSuspended = *body.ContributeSuspended
	}
	if body.ContributeAllowLabels != nil {
		cfg.Hub.ContributeAllowLabels = body.ContributeAllowLabels
	}
	if body.ContributeDenyLabels != nil {
		cfg.Hub.ContributeDenyLabels = body.ContributeDenyLabels
	}
	if body.ContributeDenyTitles != nil {
		cfg.Hub.ContributeDenyTitles = body.ContributeDenyTitles
	}
	if body.ContributeDenyAuthors != nil {
		cfg.Hub.ContributeDenyAuthors = body.ContributeDenyAuthors
	}
	if body.ContributeAllowModels != nil {
		cfg.Hub.ContributeAllowModels = body.ContributeAllowModels
	}
	if body.ContributeRejectUnknownModels != nil {
		cfg.Hub.ContributeRejectUnknownModels = *body.ContributeRejectUnknownModels
	}
	if body.DisabledRepos != nil {
		cfg.Hub.DisabledRepos = body.DisabledRepos
	}
	if body.DisabledTiers != nil {
		cfg.Hub.DisabledTiers = body.DisabledTiers
	}
	if body.TierLimits != nil {
		cfg.Hub.TierLimits = body.TierLimits
	}
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

func (s *Server) handleGovernorAddAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name    string `json:"name"`
		Backend string `json:"backend"`
		Model   string `json:"model"`
	}
	if err := decodeBody(r, &body); err != nil || body.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}

	body.Name = sanitizeString(body.Name)
	body.Backend = sanitizeString(body.Backend)
	body.Model = sanitizeString(body.Model)

	if body.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	if strings.ContainsAny(body.Name, " ./\\") || !kickTemplatePattern.MatchString(body.Name+".md") {
		jsonError(w, "name must contain only alphanumeric characters, hyphens, and underscores", http.StatusBadRequest)
		return
	}
	const maxAgentNameLen = 64
	if len(body.Name) > maxAgentNameLen {
		jsonError(w, fmt.Sprintf("name must be at most %d characters", maxAgentNameLen), http.StatusBadRequest)
		return
	}

	if _, exists := s.deps.Config.Agents[body.Name]; exists {
		jsonError(w, "agent already exists", http.StatusConflict)
		return
	}

	if body.Backend == "" {
		body.Backend = "claude"
	}

	agentCfg := config.AgentConfig{
		Backend: body.Backend,
		Model:   body.Model,
		Enabled: true,
	}
	s.deps.Config.Agents[body.Name] = agentCfg
	s.deps.AgentMgr.AddAgent(body.Name, agentCfg)
	if err := s.saveConfig(); err != nil { s.logger.Error("failed to persist config", "error", err) }

	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "added", "agent": body.Name})
}

func (s *Server) handleGovernorRemoveAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.deps.Config.Agents[name]; !ok {
		jsonError(w, "agent not found", http.StatusNotFound)
		return
	}

	delete(s.deps.Config.Agents, name)
	s.deps.AgentMgr.RemoveAgent(name)
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after agent removal", "error", err)
	}
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "removed", "agent": name})
}

func (s *Server) handleGovernorRepos(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repos       []string `json:"repos"`
		PrimaryRepo *string  `json:"primaryRepo,omitempty"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if len(body.Repos) == 0 && body.PrimaryRepo == nil {
		jsonError(w, "at least one repo is required", http.StatusBadRequest)
		return
	}
	org := s.deps.Config.Project.Org

	if len(body.Repos) > 0 {
		stripped := make([]string, 0, len(body.Repos))
		for _, repo := range body.Repos {
			repo = sanitizeString(repo)
			if repo == "" || strings.Contains(repo, "..") || strings.ContainsAny(repo, "<>\"';&|") {
				jsonError(w, fmt.Sprintf("invalid repo name: %s", repo), http.StatusBadRequest)
				return
			}
			if org != "" && strings.HasPrefix(repo, org+"/") {
				stripped = append(stripped, strings.TrimPrefix(repo, org+"/"))
			} else {
				stripped = append(stripped, repo)
			}
		}
		s.deps.Config.Project.Repos = stripped
		if s.deps.GHClient != nil {
			s.deps.GHClient.SetRepos(stripped)
		}
	}

	if body.PrimaryRepo != nil {
		newPrimary := sanitizeString(*body.PrimaryRepo)
		if org != "" && strings.HasPrefix(newPrimary, org+"/") {
			newPrimary = strings.TrimPrefix(newPrimary, org+"/")
		}
		oldPrimary := s.deps.Config.Project.PrimaryRepo
		s.deps.Config.Project.PrimaryRepo = newPrimary
		if newPrimary != oldPrimary {
			s.logger.Info("primary repo changed", "from", oldPrimary, "to", newPrimary)
			if s.deps.AdvisoryResetFunc != nil {
				go s.deps.AdvisoryResetFunc(newPrimary)
			}
		}
	}

	if err := s.saveConfig(); err != nil { s.logger.Error("failed to persist config", "error", err) }
	if s.deps.EnumerateFunc != nil {
		go s.deps.EnumerateFunc()
	}
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated"})
}

// --- Sidebar endpoints ---

func (s *Server) handleSidebarGet(w http.ResponseWriter, r *http.Request) {
	s.sidebarMu.RLock()
	sb := s.sidebar
	s.sidebarMu.RUnlock()
	if sb == nil {
		jsonResponse(w, map[string]interface{}{"sidebar": nil})
		return
	}
	jsonResponse(w, map[string]interface{}{"sidebar": sb})
}

func (s *Server) handleSidebarSet(w http.ResponseWriter, r *http.Request) {
	var body interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.sidebarMu.Lock()
	s.sidebar = body
	s.sidebarMu.Unlock()

	s.saveSidebarToDisk(body)
	okResponse(w, map[string]string{"status": "updated"})
}

const sidebarFile = "/data/sidebar.json"

func (s *Server) loadSidebarFromDisk() {
	data, err := os.ReadFile(sidebarFile)
	if err != nil {
		return
	}
	var sb interface{}
	if json.Unmarshal(data, &sb) == nil {
		s.sidebarMu.Lock()
		s.sidebar = sb
		s.sidebarMu.Unlock()
	}
}

func (s *Server) saveSidebarToDisk(sb interface{}) {
	data, err := json.Marshal(sb)
	if err != nil {
		return
	}
	tmpSidebar := sidebarFile + ".tmp"
	if os.WriteFile(tmpSidebar, data, 0o644) == nil {
		_ = os.Rename(tmpSidebar, sidebarFile)
	}
}

func (s *Server) handleBackends(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, []map[string]interface{}{
		{"id": "claude", "name": "Claude Code", "models": []string{"opus", "sonnet", "haiku"}},
		{"id": "copilot", "name": "GitHub Copilot", "models": []string{"gpt-4o", "gpt-4o-mini"}},
		{"id": "gemini", "name": "Gemini", "models": []string{"gemini-2.5-pro", "gemini-2.5-flash"}},
		{"id": "goose", "name": "Goose", "models": []string{"default"}},
	})
}

// --- Knowledge endpoints ---

func (s *Server) handleKnowledgeToggle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.deps.Config.Knowledge.Enabled = body.Enabled

	if body.Enabled && s.deps.Knowledge == nil {
		layers := make([]knowledge.LayerConfig, len(s.deps.Config.Knowledge.Layers))
		for i, l := range s.deps.Config.Knowledge.Layers {
			layers[i] = knowledge.LayerConfig{Type: knowledge.LayerType(l.Type), Path: l.Path, URL: l.URL, Shared: l.Shared}
		}
		kcfg := knowledge.KnowledgeConfig{
			Enabled: true,
			Layers:  layers,
			Primer: knowledge.PrimerConfig{
				MaxFacts:      s.deps.Config.Knowledge.Primer.MaxFacts,
				MergeStrategy: s.deps.Config.Knowledge.Primer.MergeStrategy,
			},
		}
		api := knowledge.NewKnowledgeAPI(layers, kcfg, s.deps.Logger)
		s.deps.Knowledge = api
	} else if !body.Enabled {
		s.deps.Knowledge = nil
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after knowledge toggle", "error", err)
	}
	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "enabled": fmt.Sprintf("%v", body.Enabled)})
}

func (s *Server) handleKnowledgeList(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"enabled": false, "facts": []interface{}{}})
		return
	}

	typeFilter := r.URL.Query().Get("type")
	facts := s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, "", typeFilter, 0)
	jsonResponse(w, map[string]interface{}{
		"enabled": true,
		"count":   len(facts),
		"facts":   facts,
	})
}

func (s *Server) handleKnowledgeExport(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write([]byte("# Agent Knowledge\n\nKnowledge base not available.\n"))
		return
	}

	facts := s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, "", "", 0)

	grouped := make(map[string][]knowledge.Fact)
	for _, f := range facts {
		t := string(f.Type)
		if t == "" {
			t = "general"
		}
		grouped[t] = append(grouped[t], f)
	}

	typeLabels := map[string]string{
		"pattern":        "Patterns",
		"gotcha":         "Gotchas",
		"decision":       "Decisions",
		"regression":     "Regressions",
		"test_scaffold":  "Test Scaffolds",
		"integration":    "Integration",
		"coverage_rule":  "Coverage Rules",
		"idea":           "Ideas",
		"vision":         "Vision",
		"constitution":   "Constitution",
		"requirement":    "Requirements",
		"constraint":     "Constraints",
		"stakeholder":    "Stakeholders",
		"general":        "General",
	}

	var sb strings.Builder
	sb.WriteString("# Agent Knowledge\n\n")
	sb.WriteString("This file is auto-generated from the hive knowledge base.\n")
	sb.WriteString("It refreshes periodically — do not edit manually.\n\n")

	order := []string{"constitution", "constraint", "requirement", "decision",
		"pattern", "gotcha", "regression", "coverage_rule", "test_scaffold",
		"integration", "idea", "vision", "stakeholder", "general"}

	for _, t := range order {
		ff, ok := grouped[t]
		if !ok || len(ff) == 0 {
			continue
		}
		label := typeLabels[t]
		if label == "" {
			label = strings.Title(t)
		}
		sb.WriteString("## " + label + "\n\n")
		for _, f := range ff {
			sb.WriteString("### " + f.Title + "\n\n")
			if f.Body != "" {
				sb.WriteString(f.Body + "\n\n")
			}
			if len(f.Tags) > 0 {
				sb.WriteString("Tags: " + strings.Join(f.Tags, ", ") + "\n\n")
			}
		}
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("ETag", fmt.Sprintf(`"%x"`, len(facts)))
	w.Write([]byte(sb.String()))
}

func (s *Server) handleKnowledgeSearch(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"results": []interface{}{}})
		return
	}

	query := r.URL.Query().Get("q")
	if query == "" {
		jsonError(w, "q parameter is required", http.StatusBadRequest)
		return
	}

	typeFilter := r.URL.Query().Get("type")
	limitStr := r.URL.Query().Get("limit")
	limit := 0
	if limitStr != "" {
		limit, _ = strconv.Atoi(limitStr)
	}

	results := s.deps.Knowledge.SearchAllWithVaults(s.deps.Ctx, query, typeFilter, limit)
	jsonResponse(w, map[string]interface{}{
		"query":   query,
		"count":   len(results),
		"results": results,
	})
}

func (s *Server) handleKnowledgeHealth(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, map[string]interface{}{"enabled": false})
		return
	}
	jsonResponse(w, s.deps.Knowledge.Health(s.deps.Ctx))
}

func (s *Server) handleKnowledgeStats(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"enabled": false})
		return
	}
	stats := s.deps.Knowledge.Stats(s.deps.Ctx)
	stats["vaults"] = s.deps.Knowledge.Vaults()
	stats["git_sources"] = s.deps.Knowledge.GitSources()
	jsonResponse(w, stats)
}

func (s *Server) handleKnowledgeLayer(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonResponse(w, map[string]interface{}{"enabled": false, "facts": []interface{}{}})
		return
	}

	layer := r.PathValue("layer")
	typeFilter := r.URL.Query().Get("type")

	const gitSourcePrefix = "git_source:"
	if strings.HasPrefix(layer, gitSourcePrefix) {
		name := strings.TrimPrefix(layer, gitSourcePrefix)
		store := s.deps.Knowledge.GetGitSourceStore(name)
		if store == nil {
			jsonResponse(w, map[string]interface{}{"layer": layer, "count": 0, "facts": []interface{}{}})
			return
		}
		facts := store.ListPages(typeFilter)
		for i := range facts {
			facts[i].Layer = knowledge.LayerType(layer)
		}
		jsonResponse(w, map[string]interface{}{
			"layer": layer,
			"count": len(facts),
			"facts": facts,
		})
		return
	}

	knowledgeLayer := knowledge.LayerType(layer)
	facts := s.deps.Knowledge.LayerFacts(s.deps.Ctx, knowledgeLayer, typeFilter)
	if facts == nil {
		facts = []knowledge.Fact{}
	}
	jsonResponse(w, map[string]interface{}{
		"layer": layer,
		"count": len(facts),
		"facts": facts,
	})
}

func (s *Server) handleKnowledgeFact(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not enabled", http.StatusNotFound)
		return
	}

	slug := r.PathValue("slug")
	fact, err := s.deps.Knowledge.ReadFact(s.deps.Ctx, slug)
	if err != nil || fact == nil {
		jsonError(w, "fact not found", http.StatusNotFound)
		return
	}
	jsonResponse(w, fact)
}

func (s *Server) handleKnowledgeCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req knowledge.CreateFactRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Title == "" || req.Body == "" {
		jsonError(w, "title and body are required", http.StatusBadRequest)
		return
	}
	if req.Layer == "" {
		req.Layer = "project"
	}
	if req.Type == "" {
		req.Type = "pattern"
	}
	const defaultConfidence = 0.7
	if req.Confidence <= 0 {
		req.Confidence = defaultConfidence
	}

	if err := s.deps.Knowledge.CreateFact(s.deps.Ctx, req); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "title": req.Title, "layer": req.Layer})
}

func (s *Server) handleKnowledgeUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	layer := r.PathValue("layer")
	slug := r.PathValue("slug")

	var req knowledge.UpdateFactRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.UpdateFact(s.deps.Ctx, knowledge.LayerType(layer), slug, req); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "slug": slug, "layer": layer})
}

func (s *Server) handleKnowledgeDelete(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	layer := r.PathValue("layer")
	slug := r.PathValue("slug")

	if err := s.deps.Knowledge.DeleteFact(s.deps.Ctx, knowledge.LayerType(layer), slug); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "deleted": slug})
}

func (s *Server) handleKnowledgePromote(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req knowledge.PromoteRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Slug == "" || req.FromLayer == "" || req.ToLayer == "" {
		jsonError(w, "slug, from_layer, and to_layer are required", http.StatusBadRequest)
		return
	}
	if req.Promoter == "" {
		req.Promoter = "dashboard"
	}

	result := s.deps.Knowledge.PromoteFact(s.deps.Ctx, req)
	if !result.Success {
		fact, err := s.deps.Knowledge.VaultFact(req.Slug)
		if err != nil || fact == nil {
			jsonError(w, result.Error, http.StatusBadRequest)
			return
		}
		syncReq := knowledge.ObsidianSyncRequest{
			Filename: req.Slug + ".md",
			Content:  fact.Body,
		}
		syncReq.Frontmatter = map[string]interface{}{
			"title":      fact.Title,
			"type":       string(fact.Type),
			"layer":      string(req.ToLayer),
			"confidence": fact.Confidence,
			"tags":       fact.Tags,
		}
		syncResult, syncErr := s.deps.Knowledge.ObsidianSync(s.deps.Ctx, syncReq)
		if syncErr != nil {
			jsonError(w, syncErr.Error(), http.StatusInternalServerError)
			return
		}
		jsonResponse(w, knowledge.PromoteResult{
			Slug:      syncResult.Slug,
			FromLayer: req.FromLayer,
			ToLayer:   req.ToLayer,
			Success:   true,
		})
		return
	}
	jsonResponse(w, result)
}

func (s *Server) handleKnowledgeImport(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Content string `json:"content"`
		Format  string `json:"format"`
		Layer   string `json:"layer"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.Content == "" {
		jsonError(w, "content is required", http.StatusBadRequest)
		return
	}
	if req.Layer == "" {
		req.Layer = "project"
	}
	if req.Format == "" {
		req.Format = "markdown"
	}

	count, err := s.deps.Knowledge.ImportFacts(s.deps.Ctx, knowledge.LayerType(req.Layer), req.Content, req.Format)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "imported": count, "layer": req.Layer, "format": req.Format})
}

func (s *Server) handleKnowledgeSubsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, s.deps.Knowledge.Subscriptions())
}

func (s *Server) handleKnowledgeSubsAdd(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var sub knowledge.Subscription
	if err := decodeBody(r, &sub); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if sub.URL == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(sub.URL, "https://") && !strings.HasPrefix(sub.URL, "http://") {
		jsonError(w, "url must use http or https scheme", http.StatusBadRequest)
		return
	}
	if isPrivateURL(r.Context(), sub.URL) {
		jsonError(w, "subscription url must not point to private/internal addresses", http.StatusBadRequest)
		return
	}
	if sub.Layer == "" {
		sub.Layer = knowledge.LayerOrg
	}

	if err := s.deps.Knowledge.AddSubscription(sub); err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "subscription": sub})
}

func (s *Server) handleKnowledgeSubsRemove(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		URL string `json:"url"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if req.URL == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.RemoveSubscription(req.URL); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "removed": req.URL})
}

// --- Vault endpoints ---

func (s *Server) handleVaultsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, s.deps.Knowledge.Vaults())
}

func (s *Server) handleVaultsConnect(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		jsonError(w, "path is required", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Path, "..") {
		jsonError(w, "vault path must not contain '..'", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(req.Path) {
		jsonError(w, "vault path must be absolute", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		req.Name = filepath.Base(req.Path)
	}

	if err := s.deps.Knowledge.ConnectVault(req.Path, req.Name); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}

	jsonResponse(w, map[string]interface{}{"ok": true, "name": req.Name, "path": req.Path})
}

func (s *Server) handleVaultsDisconnect(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		jsonError(w, "path is required", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Path, "..") {
		jsonError(w, "vault path must not contain '..'", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(req.Path) {
		jsonError(w, "vault path must be absolute", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.DisconnectVault(req.Path); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "removed": req.Path})
}

func (s *Server) handleVaultsReindex(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Path string `json:"path"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Path == "" {
		jsonError(w, "path is required", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Path, "..") {
		jsonError(w, "path must not contain '..'", http.StatusBadRequest)
		return
	}
	if !filepath.IsAbs(req.Path) {
		jsonError(w, "path must be absolute", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.ReindexVault(req.Path); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "reindexed": req.Path})
}

func (s *Server) handleVaultFacts(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}

	name := r.PathValue("name")
	facts := s.deps.Knowledge.VaultFacts(name)
	if facts == nil {
		facts = []knowledge.Fact{}
	}
	jsonResponse(w, facts)
}

// --- Git source endpoints ---

func (s *Server) handleGitSourcesList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, s.deps.Knowledge.GitSources())
}

func (s *Server) handleGitSourcesConnect(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "knowledge not available", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Branch  string `json:"branch"`
		Subpath string `json:"subpath"`
		Layer   string `json:"layer"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.URL == "" || req.Name == "" {
		jsonError(w, "name and url are required", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(req.URL, "https://") && !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "git@") {
		jsonError(w, "url must start with https://, http://, or git@", http.StatusBadRequest)
		return
	}
	if strings.Contains(req.Name, "..") || strings.ContainsAny(req.Name, "/\\") {
		jsonError(w, "invalid name", http.StatusBadRequest)
		return
	}
	if req.Branch != "" && strings.HasPrefix(req.Branch, "-") {
		jsonError(w, "branch must not start with '-'", http.StatusBadRequest)
		return
	}
	if req.Subpath != "" && (strings.HasPrefix(req.Subpath, "-") || strings.Contains(req.Subpath, "..")) {
		jsonError(w, "subpath must not start with '-' or contain '..'", http.StatusBadRequest)
		return
	}
	if req.Layer == "" {
		req.Layer = "project"
	}

	gsConfig := knowledge.GitSourceConfig{
		Name:    req.Name,
		URL:     req.URL,
		Branch:  req.Branch,
		Subpath: req.Subpath,
		Layer:   knowledge.LayerType(req.Layer),
	}
	if err := s.deps.Knowledge.ConnectGitSource(s.deps.Ctx, gsConfig); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	alreadyInConfig := false
	for _, gs := range s.deps.Config.Knowledge.GitSources {
		if gs.URL == req.URL && gs.Subpath == req.Subpath {
			alreadyInConfig = true
			break
		}
	}
	if !alreadyInConfig {
		s.deps.Config.Knowledge.GitSources = append(s.deps.Config.Knowledge.GitSources, config.GitSourceConfigYAML{
			Name:    req.Name,
			URL:     req.URL,
			Branch:  req.Branch,
			Subpath: req.Subpath,
			Layer:   req.Layer,
		})
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after git source connect", "error", err)
	}

	jsonResponse(w, map[string]interface{}{"ok": true, "name": req.Name, "url": req.URL})
}

func (s *Server) handleGitSourcesDisconnect(w http.ResponseWriter, r *http.Request) {
	if s.deps.Knowledge == nil {
		jsonError(w, "knowledge not enabled", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		URL     string `json:"url"`
		Subpath string `json:"subpath"`
	}
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		jsonError(w, "url is required", http.StatusBadRequest)
		return
	}

	if err := s.deps.Knowledge.DisconnectGitSource(req.URL, req.Subpath); err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}

	filtered := make([]config.GitSourceConfigYAML, 0, len(s.deps.Config.Knowledge.GitSources))
	for _, gs := range s.deps.Config.Knowledge.GitSources {
		if gs.URL == req.URL && gs.Subpath == req.Subpath {
			continue
		}
		filtered = append(filtered, gs)
	}
	s.deps.Config.Knowledge.GitSources = filtered
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after git source disconnect", "error", err)
	}

	jsonResponse(w, map[string]interface{}{"ok": true, "removed": req.URL})
}

// --- Obsidian sync endpoint ---

func (s *Server) ensureKnowledge() bool {
	if s.deps == nil {
		return false
	}
	s.knowledgeMu.Lock()
	defer s.knowledgeMu.Unlock()
	if s.deps.Knowledge == nil {
		s.deps.Knowledge = knowledge.NewKnowledgeAPI(nil, knowledge.KnowledgeConfig{Enabled: true, Engine: "file"}, s.logger)
		s.logger.Info("created file-based knowledge API for vault/obsidian access")
		entries, err := os.ReadDir("/data/knowledge")
		if err == nil {
			for _, e := range entries {
				if e.IsDir() {
					dir := filepath.Join("/data/knowledge", e.Name())
					if connErr := s.deps.Knowledge.ConnectVault(dir, e.Name()); connErr == nil {
						s.logger.Info("auto-connected knowledge vault", "name", e.Name(), "dir", dir)
					}
				}
			}
		}
	}
	return true
}

func (s *Server) handleObsidianSync(w http.ResponseWriter, r *http.Request) {
	if !s.ensureKnowledge() {
		jsonError(w, "server not initialized", http.StatusServiceUnavailable)
		return
	}

	var req knowledge.ObsidianSyncRequest
	if err := decodeBody(r, &req); err != nil {
		s.logger.Warn("obsidian sync: json decode failed", "error", err)
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}

	if req.Filename == "" {
		jsonError(w, "filename is required", http.StatusBadRequest)
		return
	}
	if req.Content == "" {
		if title, ok := req.Frontmatter["title"]; ok {
			if s, ok := title.(string); ok && s != "" {
				req.Content = s
			}
		}
		if req.Content == "" {
			req.Content = "(no body)"
		}
	}

	result, err := s.deps.Knowledge.ObsidianSync(s.deps.Ctx, req)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"ok":     true,
		"slug":   result.Slug,
		"action": result.Action,
		"fact":   result.Fact,
	})
}

// --- Hive ID endpoints ---

// hiveIDFilePath is the persistent file where the Hive ID is stored.
const hiveIDFilePath = "/data/hive-id"

func (s *Server) handleHiveIDGet(w http.ResponseWriter, r *http.Request) {
	id := ""
	if s.deps != nil && s.deps.Config != nil {
		id = s.deps.Config.HiveID
	}
	jsonResponse(w, map[string]string{"id": id})
}

func (s *Server) handleHiveIDSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &body); err != nil || body.ID == "" {
		jsonError(w, "id is required", http.StatusBadRequest)
		return
	}

	const maxHiveIDLen = 64
	if len(body.ID) > maxHiveIDLen {
		jsonError(w, fmt.Sprintf("id must be at most %d characters", maxHiveIDLen), http.StatusBadRequest)
		return
	}
	if !displayNamePattern.MatchString(body.ID) {
		jsonError(w, "id must contain only alphanumeric characters, spaces, hyphens, and underscores", http.StatusBadRequest)
		return
	}
	body.ID = sanitizeString(body.ID)

	if s.deps != nil && s.deps.Config != nil {
		s.deps.Config.HiveID = body.ID
	}

	// Persist the new ID to disk so it survives restarts
	tmpHiveID := hiveIDFilePath + ".tmp"
	if err := os.WriteFile(tmpHiveID, []byte(body.ID+"\n"), 0o644); err != nil {
		s.logger.Warn("failed to persist hive ID", "error", err)
	} else if err := os.Rename(tmpHiveID, hiveIDFilePath); err != nil {
		s.logger.Warn("failed to rename hive ID file", "error", err)
	}

	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "id": body.ID})
}

// --- Chat endpoint ---

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query   string `json:"query"`
		History []any  `json:"history"`
	}
	if err := decodeBody(r, &body); err != nil || body.Query == "" {
		jsonError(w, "message is required", http.StatusBadRequest)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"answer": fmt.Sprintf("Chat is not yet fully implemented. You asked: %s", sanitizeString(body.Query)),
		"status": "stub",
	})
}

// --- Nous endpoints ---

func (s *Server) handleNousStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]string{"status": "not_configured"})
		return
	}
	s.deps.Nous.Mu.Lock()
	status := make(map[string]interface{}, len(s.deps.Nous.Status))
	for k, v := range s.deps.Nous.Status {
		status[k] = v
	}
	s.deps.Nous.Mu.Unlock()
	jsonResponse(w, status)
}

func (s *Server) handleNousLedger(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	s.deps.Nous.Mu.Lock()
	ledger := s.deps.Nous.Ledger
	s.deps.Nous.Mu.Unlock()
	if ledger == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, ledger)
}

func (s *Server) handleNousPrinciples(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	s.deps.Nous.Mu.Lock()
	principles := s.deps.Nous.Principles
	s.deps.Nous.Mu.Unlock()
	if principles == nil {
		jsonResponse(w, []interface{}{})
		return
	}
	jsonResponse(w, principles)
}

func (s *Server) handleNousApprove(w http.ResponseWriter, r *http.Request) {
	okResponse(w, map[string]string{"status": "approved"})
}

func (s *Server) handleNousAbort(w http.ResponseWriter, r *http.Request) {
	okResponse(w, map[string]string{"status": "aborted"})
}

func (s *Server) handleNousMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := decodeBody(r, &body); err != nil || body.Mode == "" {
		jsonError(w, "mode is required", http.StatusBadRequest)
		return
	}

	body.Mode = sanitizeString(body.Mode)
	if body.Mode == "" {
		jsonError(w, "mode is required", http.StatusBadRequest)
		return
	}

	if s.deps.Nous != nil {
		s.deps.Nous.Mode = body.Mode
	}

	okResponse(w, map[string]string{"status": "updated", "mode": body.Mode})
}

func (s *Server) handleNousScope(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope string `json:"scope"`
	}
	if err := decodeBody(r, &body); err != nil || body.Scope == "" {
		jsonError(w, "scope is required", http.StatusBadRequest)
		return
	}

	body.Scope = sanitizeString(body.Scope)
	if body.Scope == "" {
		jsonError(w, "scope is required", http.StatusBadRequest)
		return
	}

	if s.deps.Nous != nil {
		s.deps.Nous.Scope = body.Scope
	}

	okResponse(w, map[string]string{"status": "updated", "scope": body.Scope})
}

func (s *Server) handleNousPhase(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]string{"phase": "inactive"})
		return
	}
	jsonResponse(w, map[string]string{"phase": s.deps.Nous.Phase})
}

func (s *Server) handleNousGateDecision(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}

	var body struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil || body.Decision == "" {
		jsonError(w, "decision is required", http.StatusBadRequest)
		return
	}

	body.Decision = sanitizeString(body.Decision)
	body.Reason = sanitizeString(body.Reason)
	if body.Decision == "" {
		jsonError(w, "decision is required", http.StatusBadRequest)
		return
	}

	if s.deps.Nous.GatePending == nil {
		s.deps.Nous.GatePending = make(map[string]interface{})
	}
	s.deps.Nous.GateResponse = map[string]interface{}{
		"decision": body.Decision,
		"reason":   body.Reason,
	}

	okResponse(w, map[string]string{"status": "decided", "decision": body.Decision})
}

func (s *Server) handleNousGatePending(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	jsonResponse(w, s.deps.Nous.GatePending)
}

func (s *Server) handleNousGateRespond(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if s.deps.Nous != nil {
		s.deps.Nous.GateResponse = body
	}

	okResponse(w, map[string]string{"status": "responded"})
}

func (s *Server) handleNousGateResponse(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	jsonResponse(w, s.deps.Nous.GateResponse)
}

func (s *Server) handleNousConfigGet(w http.ResponseWriter, r *http.Request) {
	if s.deps.Nous == nil {
		jsonResponse(w, map[string]interface{}{})
		return
	}
	s.deps.Nous.Mu.Lock()
	cfg := s.deps.Nous.Config
	s.deps.Nous.Mu.Unlock()
	jsonResponse(w, cfg)
}

func (s *Server) handleNousConfigGoals(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "goals")
}

func (s *Server) handleNousConfigRepos(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "repos")
}

func (s *Server) handleNousConfigOutput(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "output")
}

func (s *Server) handleNousConfigFastFail(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "fast_fail")
}

func (s *Server) handleNousConfigSchedule(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "schedule")
}

func (s *Server) handleNousConfigControllables(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "controllables")
}

func (s *Server) handleNousConfigPrinciples(w http.ResponseWriter, r *http.Request) {
	s.handleNousConfigSection(w, r, "principles")
}

func (s *Server) handleNousDeletePrinciple(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}

	s.deps.Nous.Mu.Lock()
	filtered := make([]NousPrinciple, 0, len(s.deps.Nous.Principles))
	for _, p := range s.deps.Nous.Principles {
		if p.ID != id {
			filtered = append(filtered, p)
		}
	}
	s.deps.Nous.Principles = filtered
	s.deps.Nous.Mu.Unlock()

	okResponse(w, map[string]string{"status": "deleted", "id": id})
}

func (s *Server) handleNousConfigSection(w http.ResponseWriter, r *http.Request, section string) {
	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}

	var body interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	s.deps.Nous.Mu.Lock()
	if s.deps.Nous.Config == nil {
		s.deps.Nous.Config = make(map[string]interface{})
	}
	s.deps.Nous.Config[section] = body
	s.deps.Nous.Mu.Unlock()

	s.refreshAndPersist()
	okResponse(w, map[string]string{"status": "updated", "section": section})
}

var defaultPipelineSteps = map[string]bool{
	"resolve-beads":  true,
	"track-prs":      true,
	"stale-check":    true,
	"repo-scan":      true,
	"coverage-gate":  true,
	"prompt-compose": true,
	"budget-check":   true,
	"api-collect":    true,
	"final-compose":  true,
}

func (s *Server) getAgentPipeline(name string) map[string]bool {
	s.pipelineMu.RLock()
	defer s.pipelineMu.RUnlock()
	if p, ok := s.agentPipelines[name]; ok {
		return p
	}
	result := make(map[string]bool, len(defaultPipelineSteps))
	for k, v := range defaultPipelineSteps {
		result[k] = v
	}
	return result
}

func (s *Server) getAgentHooks(name string) map[string][]any {
	s.hooksMu.RLock()
	defer s.hooksMu.RUnlock()
	if h, ok := s.agentHooks[name]; ok {
		return h
	}
	return map[string][]any{"preKick": {}, "postIdle": {}}
}

// maskSecret replaces the interior of a secret string with bullet characters,
// revealing only the last 4 characters (matching old hive behavior).
func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	const visibleSuffix = 4
	if len(s) <= visibleSuffix {
		return strings.Repeat("•", len(s))
	}
	masked := strings.Repeat("•", len(s)-visibleSuffix)
	return masked + s[len(s)-visibleSuffix:]
}

func (s *Server) handleAuthToken(w http.ResponseWriter, r *http.Request) {
	token := s.authToken
	if token == "" {
		token = os.Getenv("HIVE_DASHBOARD_TOKEN")
	}
	if token == "" {
		okResponse(w, map[string]string{"token": "(not set)", "configured": "false"})
		return
	}
	okResponse(w, map[string]string{"token": token, "configured": "true"})
}

// maskToken replaces all but the last 4 characters with bullet characters.
func maskToken(token string) string {
	const visibleSuffix = 4
	if len(token) <= visibleSuffix {
		return token
	}
	masked := make([]byte, 0, len(token))
	hideLen := len(token) - visibleSuffix
	for i := 0; i < hideLen; i++ {
		masked = append(masked, "•"...)
	}
	masked = append(masked, token[hideLen:]...)
	return string(masked)
}

func (s *Server) handleBeadsReset(w http.ResponseWriter, r *http.Request) {
	if s.deps.BeadStores == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil {
		body.Reason = "manual reset via API"
	}
	body.Reason = sanitizeString(body.Reason)
	if body.Reason == "" {
		body.Reason = "manual reset via API"
	}

	results := make(map[string]int)
	for name, store := range s.deps.BeadStores {
		closed, err := store.CloseAll(body.Reason)
		if err != nil {
			s.deps.Logger.Error("beads reset failed", "agent", name, "error", err)
		}
		results[name] = closed
	}

	s.refreshAndPersist()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "reset", "closed": results, "reason": body.Reason})
}

func (s *Server) handleBeadsResetAgent(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	if s.deps.BeadStores == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	store, ok := s.deps.BeadStores[agentName]
	if !ok {
		jsonError(w, fmt.Sprintf("no bead store for agent %q", agentName), http.StatusNotFound)
		return
	}

	var body struct {
		Reason string `json:"reason"`
	}
	if err := decodeBody(r, &body); err != nil {
		body.Reason = "manual reset via API"
	}
	body.Reason = sanitizeString(body.Reason)
	if body.Reason == "" {
		body.Reason = "manual reset via API"
	}

	closed, err := store.CloseAll(body.Reason)
	if err != nil {
		jsonError(w, fmt.Sprintf("reset failed: %v", err), http.StatusInternalServerError)
		return
	}

	s.refreshAndPersist()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "reset", "agent": agentName, "closed": closed, "reason": body.Reason})
}

func (s *Server) handleBeadsList(w http.ResponseWriter, r *http.Request) {
	if s.deps.BeadStores == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	agentName := r.PathValue("agent")
	result := make(map[string]any)

	if agentName != "" {
		store, ok := s.deps.BeadStores[agentName]
		if !ok {
			jsonError(w, fmt.Sprintf("no bead store for agent %q", agentName), http.StatusNotFound)
			return
		}
		result[agentName] = store.List(beads.ListFilter{})
	} else {
		for name, store := range s.deps.BeadStores {
			result[name] = store.List(beads.ListFilter{})
		}
	}
	jsonResponse(w, result)
}

const maxBeadPriority = 4

func (s *Server) handleBeadsCreate(w http.ResponseWriter, r *http.Request) {
	agentName := r.PathValue("agent")
	if s.deps.BeadStores == nil {
		jsonError(w, "bead stores not initialized", http.StatusServiceUnavailable)
		return
	}

	store, ok := s.deps.BeadStores[agentName]
	if !ok {
		jsonError(w, fmt.Sprintf("no bead store for agent %q", agentName), http.StatusNotFound)
		return
	}

	var body struct {
		Title       string            `json:"title"`
		Type        string            `json:"type"`
		Priority    int               `json:"priority"`
		ExternalRef string            `json:"external_ref"`
		Metadata    map[string]string `json:"metadata"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	body.Title = sanitizeString(body.Title)
	const maxBeadTitleLen = 500
	if body.Title == "" {
		jsonError(w, "title is required", http.StatusBadRequest)
		return
	}
	if len(body.Title) > maxBeadTitleLen {
		jsonError(w, fmt.Sprintf("title too long (%d chars, max %d)", len(body.Title), maxBeadTitleLen), http.StatusBadRequest)
		return
	}
	body.Type = sanitizeString(body.Type)
	if body.Type == "" {
		body.Type = "advisory"
	}
	body.ExternalRef = sanitizeString(body.ExternalRef)
	if body.Priority < 0 || body.Priority > maxBeadPriority {
		jsonError(w, "priority must be 0-4", http.StatusBadRequest)
		return
	}

	b, err := store.Create(body.Title, beads.BeadType(body.Type), beads.Priority(body.Priority), agentName, body.ExternalRef)
	if err != nil {
		jsonError(w, fmt.Sprintf("failed to create bead: %v", err), http.StatusInternalServerError)
		return
	}

	const maxMetadataKeyLen = 100
	const maxMetadataValueLen = 1000
	const maxMetadataEntries = 50
	metaCount := 0
	for k, v := range body.Metadata {
		if metaCount >= maxMetadataEntries || len(k) > maxMetadataKeyLen || len(v) > maxMetadataValueLen {
			continue
		}
		_ = store.SetMetadata(b.ID, k, v)
		metaCount++
	}

	w.WriteHeader(http.StatusCreated)
	jsonResponse(w, b)
}

