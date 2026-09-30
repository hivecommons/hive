package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.deps.Config
	primaryRepo := cfg.Project.PrimaryRepo
	if primaryRepo == "" && len(cfg.Project.Repos) > 0 {
		primaryRepo = cfg.Project.Repos[0]
	}
	if primaryRepo != "" && cfg.Project.Org != "" && !strings.Contains(primaryRepo, "/") {
		primaryRepo = cfg.Project.Org + "/" + primaryRepo
	}
	// ResolvedBaseURL (not the raw BaseURL) so pooled/placeholder GHE hives —
	// which legitimately carry base_url: "" but api_url: https://github.ibm.com/api/v3
	// — report github.ibm.com, not github.com. Reading BaseURL alone made the
	// Repos config show bare org/repo with no GHE hint, so operators mistook a
	// github.ibm.com hive for github.com. ResolvedBaseURL falls back to the api_url
	// host in exactly that case (mirrors HostLabel).
	githubBaseURL := cfg.GitHub.ResolvedBaseURL()
	resp := map[string]interface{}{
		"org":       cfg.Project.Org,
		"repos":     cfg.Project.Repos,
		"ai_author": cfg.Project.AIAuthor,
		// ai_author_effective is who agents actually author PRs/commits as: the
		// configured ai_author, or the GitHub App bot login ("<slug>[bot]") when
		// ai_author is empty and the hive authenticates as an App installation.
		"ai_author_effective":   cfg.EffectiveAIAuthor(),
		"agents":                len(cfg.EnabledAgents()),
		"eval_interval_s":       cfg.Governor.EvalIntervalS,
		"primaryRepo":           primaryRepo,
		"hub_url":               cfg.Hub.URL,
		"hive_id":               cfg.HiveID,
		"github_base_url":       githubBaseURL,
		"dashboard_issue_bands": cfg.Dashboard.IssueBands,
		"writing_guide":         cfg.Project.WritingGuide,
		"my_hives_url":          myHivesURL(cfg), // hub My Hives page for the user menu (#9696); "" when standalone
	}
	// The active project.issue_filter, read-only: which issues agents may
	// initiate work on, by label. Omitted entirely when no filter is
	// configured so the payload (and the UI note keyed off it) stays quiet
	// for the ordinary unfiltered hive.
	if !cfg.Project.IssueFilter.IsZero() {
		resp["issue_filter"] = cfg.Project.IssueFilter
	}
	jsonResponse(w, resp)
}

func (s *Server) handleConfigDownload(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	// F9 (CWE-862): the raw hive.yaml carries secrets, so a MISSING X-Hive-Role
	// must NOT default to owner on a spoke that has an auth boundary. Least
	// privilege: only a genuinely open/dev spoke treats an empty role as owner.
	if !s.requestRoleAllowsOwner(r) {
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
	_, _ = w.Write(data)
}

func (s *Server) handleGitHubAppInstallClicked(w http.ResponseWriter, r *http.Request) {
	s.SetPendingGitHubAppInstall()
	s.deps.Logger.Info("github app install link clicked, flagging heartbeat")
	okResponse(w, map[string]string{"status": "pending"})
}

type githubConfigUpdate struct {
	AppID                 *int64
	InstallationID        *int64
	KeyFile               string
	PrivateKey            string
	SelfAuthorizationHold *bool
}

type githubConfigUpdateError struct {
	message string
	code    int
}

func (e *githubConfigUpdateError) Error() string { return e.message }

func newGitHubConfigUpdateError(message string, code int) *githubConfigUpdateError {
	return &githubConfigUpdateError{message: message, code: code}
}

func (s *Server) handleConfigGitHub(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var body struct {
		AppID                 *int64 `json:"app_id"`
		InstallationID        *int64 `json:"installation_id"`
		KeyFile               string `json:"key_file"`
		PrivateKey            string `json:"private_key"`
		SelfAuthorizationHold *bool  `json:"self_authorization_hold"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	if body.AppID == nil && body.InstallationID == nil && body.KeyFile == "" && body.PrivateKey == "" && body.SelfAuthorizationHold == nil {
		jsonError(w, "at least one field required: app_id, installation_id, key_file, private_key, self_authorization_hold", http.StatusBadRequest)
		return
	}

	result, err := s.applyGitHubConfigUpdate(r, githubConfigUpdate{
		AppID:                 body.AppID,
		InstallationID:        body.InstallationID,
		KeyFile:               body.KeyFile,
		PrivateKey:            body.PrivateKey,
		SelfAuthorizationHold: body.SelfAuthorizationHold,
	})
	if err != nil {
		code := http.StatusInternalServerError
		if updateErr, ok := err.(*githubConfigUpdateError); ok {
			code = updateErr.code
		}
		jsonError(w, err.Error(), code)
		return
	}
	jsonResponse(w, result)
}

func (s *Server) applyGitHubConfigUpdate(r *http.Request, body githubConfigUpdate) (map[string]interface{}, error) {
	if s.deps == nil || s.deps.Config == nil {
		return nil, newGitHubConfigUpdateError("config not available", http.StatusInternalServerError)
	}

	s.githubConfigMu.Lock()
	defer s.githubConfigMu.Unlock()

	cfg := s.deps.Config

	// saveConfig() silently no-ops when the config has no source path, so
	// without this guard the handler would report "status":"updated" for a
	// value that lives only in memory and is lost on the next restart (#2459).
	// Refuse up front, before mutating anything, and name the cause.
	if cfg.SourcePath == "" {
		s.logger.Error("github config update rejected: config has no source path, save would be an in-memory no-op")
		return nil, newGitHubConfigUpdateError("config not persisted: config has no source path, so the change would be lost on restart", http.StatusInternalServerError)
	}

	if body.PrivateKey != "" {
		keyPath := body.KeyFile
		if keyPath == "" {
			keyPath = "/data/gh-app-key.pem"
		}
		if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
			s.logger.Warn("could not create key directory", "path", keyPath, "error", err)
			return nil, newGitHubConfigUpdateError(fmt.Sprintf("failed to create key directory: %v", err), http.StatusInternalServerError)
		}
		const keyFileMode = 0o600
		if err := os.WriteFile(keyPath, []byte(body.PrivateKey), keyFileMode); err != nil {
			return nil, newGitHubConfigUpdateError(fmt.Sprintf("failed to write key file: %v", err), http.StatusInternalServerError)
		}
		cfg.GitHub.KeyFile = keyPath
		s.logger.Info("github app private key written", "path", keyPath)
	} else if body.KeyFile != "" {
		cfg.GitHub.KeyFile = body.KeyFile
	}

	if body.AppID != nil {
		cfg.GitHub.AppID = *body.AppID
	}
	if body.InstallationID != nil {
		cfg.GitHub.InstallationID = *body.InstallationID
	}
	if body.SelfAuthorizationHold != nil {
		v := *body.SelfAuthorizationHold
		cfg.GitHub.SelfAuthorizationHold = &v
	}

	result, err := s.finishGitHubConfigUpdateLocked(requestAuditUser(r), "")
	if err != nil {
		if strings.Contains(err.Error(), "source path") {
			s.logger.Error("github config update rejected: config has no source path, save would be an in-memory no-op")
			return nil, newGitHubConfigUpdateError("config not persisted: config has no source path, so the change would be lost on restart", http.StatusInternalServerError)
		}
		s.logger.Error("failed to persist github config", "error", err)
		return nil, newGitHubConfigUpdateError("failed to save config", http.StatusInternalServerError)
	}
	return result, nil
}

func requestAuditUser(r *http.Request) string {
	user := r.Header.Get("X-Hive-User")
	if user == "" {
		return "local"
	}
	return user
}

// finishGitHubConfigUpdateLocked is the shared tail of the manual
// PUT /api/config/github flow and automatic installation-ID discovery. The
// caller must hold githubConfigMu and must have already mutated s.deps.Config.
func (s *Server) finishGitHubConfigUpdateLocked(auditUser, detail string) (map[string]interface{}, error) {
	cfg := s.deps.Config
	if cfg.SourcePath == "" {
		return nil, fmt.Errorf("config has no source path")
	}
	if err := s.saveConfig(); err != nil {
		return nil, err
	}

	result := map[string]interface{}{
		"status":                  "updated",
		"app_id":                  cfg.GitHub.AppID,
		"installation_id":         cfg.GitHub.InstallationID,
		"key_file":                cfg.GitHub.KeyFile,
		"self_authorization_hold": cfg.GitHub.SelfAuthorizationHoldEnabledAtLevel(cfg.ACMMLevelOrZero()),
	}

	// Resolve the signing key the same way the boot and heartbeat-apply paths
	// do, NOT from the raw config value: on hosted spokes the key is
	// hub-delivered to the per-app-id path with key_file deliberately left
	// empty, and gating reinit on cfg.GitHub.KeyFile alone made Set ID save the
	// installation_id but never rebuild the client — banner never cleared,
	// Re-check dead-ended on a nil client (#2459).
	keyFile := cfg.GitHub.KeyFile
	if s.deps.ResolveAppKeyFileFunc != nil {
		keyFile = s.deps.ResolveAppKeyFileFunc(cfg.GitHub.KeyFile, cfg.GitHub.AppID)
	}

	// HasUsableApp() rejects the placeholder sentinel: reinitializing App auth
	// against it would fail on every save and, before this guard, could not
	// succeed no matter what installation_id the operator supplied.
	if cfg.GitHub.HasUsableApp() && keyFile != "" {
		if s.deps.ReinitGitHubFunc != nil {
			if err := s.deps.ReinitGitHubFunc(cfg.GitHub.AppID, cfg.GitHub.InstallationID, keyFile); err != nil {
				s.logger.Error("github client reinit failed", "error", err)
				result["reinit"] = "failed"
				result["reinit_error"] = err.Error()
			} else {
				result["reinit"] = "ok"
				s.SetGitHubAppRequired(false)
				s.ClearPendingGitHubAppInstall()
			}
		}
	}

	s.AuditLog(auditUser, "config_github", detail, "")
	s.refreshAndPersist()
	return result, nil
}

// AutoDiscoverGitHubInstallationID tries to fill an empty github.installation_id
// from the App-level GitHub API, then persists and reinitializes through the
// same path used by PUT /api/config/github. All failures are soft; callers keep
// showing the manual installation-ID flow.
func (s *Server) AutoDiscoverGitHubInstallationID(ctx context.Context, force bool) (int64, error) {
	if s == nil || s.deps == nil || s.deps.Config == nil || s.deps.GHAppAuth == nil || !s.deps.GHAppAuth.HasKey() {
		return 0, nil
	}
	cfg := s.deps.Config
	if cfg.GitHub.InstallationID != 0 {
		return 0, nil
	}
	org := configuredProjectOrgForGitHubApp(cfg, s.logger)
	if org == "" {
		return 0, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if force {
		s.deps.GHAppAuth.ForgetInstallationDiscovery(org)
	}

	id, err := s.deps.GHAppAuth.DiscoverInstallationID(ctx, org)
	if err != nil {
		s.logger.Warn("github app installation auto-discovery failed; manual installation ID entry remains available",
			"org", org, "error", err)
		return 0, err
	}
	if id == 0 {
		return 0, nil
	}

	s.githubConfigMu.Lock()
	defer s.githubConfigMu.Unlock()
	if cfg.GitHub.InstallationID != 0 {
		return 0, nil
	}
	cfg.GitHub.InstallationID = id
	result, err := s.finishGitHubConfigUpdateLocked("system",
		auditDetail("source", "auto_discovery", "org", org, "installation_id", strconv.FormatInt(id, 10)))
	if err != nil {
		cfg.GitHub.InstallationID = 0
		s.logger.Warn("github app installation auto-discovered but could not be persisted; manual installation ID entry remains available",
			"org", org, "installation_id", id, "error", err)
		return 0, err
	}
	if result["reinit"] == "failed" {
		cfg.GitHub.InstallationID = 0
		if saveErr := s.saveConfig(); saveErr != nil {
			s.logger.Warn("github app installation auto-discovery reinit failed and rollback could not be persisted",
				"org", org, "installation_id", id, "reinit_error", result["reinit_error"], "rollback_error", saveErr)
		}
		return 0, fmt.Errorf("reinitializing github client after auto-discovery: %v", result["reinit_error"])
	}
	s.logger.Info("github app installation auto-discovered", "org", org, "installation_id", id)
	return id, nil
}

func (s *Server) handleGitHubAppSetupCallback(w http.ResponseWriter, r *http.Request) {
	redirect := func(flag string) {
		http.Redirect(w, r, "/?ghSetup="+flag, http.StatusSeeOther)
	}

	action := strings.TrimSpace(r.URL.Query().Get("setup_action"))
	switch action {
	case "request":
		s.logger.Info("github app setup callback recorded pending approval request")
		redirect("requested")
		return
	case "install", "update":
	default:
		s.logger.Warn("github app setup callback rejected: unsupported setup_action", "setup_action", action)
		redirect("error")
		return
	}

	rawID := strings.TrimSpace(r.URL.Query().Get("installation_id"))
	installationID, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || installationID <= 0 {
		s.logger.Warn("github app setup callback rejected: invalid installation_id", "installation_id", rawID)
		redirect("error")
		return
	}

	if err := s.verifyGitHubSetupInstallation(r, installationID); err != nil {
		s.logger.Warn("github app setup callback rejected", "installation_id", installationID, "error", err)
		redirect("error")
		return
	}

	if err := s.persistGitHubSetupInstallation(r, installationID); err != nil {
		s.logger.Warn("github app setup callback failed to persist installation", "installation_id", installationID, "error", err)
		redirect("error")
		return
	}

	redirect("ok")
}

func (s *Server) verifyGitHubSetupInstallation(r *http.Request, installationID int64) error {
	if s.deps == nil || s.deps.Config == nil {
		return fmt.Errorf("config not available")
	}
	cfg := s.deps.Config
	if cfg.GitHub.InstallationID != 0 && cfg.GitHub.InstallationID != installationID && !s.requestHasGitHubSetupAdmin(r) {
		return fmt.Errorf("installation_id is already configured; authenticated admin required to replace it")
	}
	if !cfg.GitHub.HasApp() {
		return fmt.Errorf("github app_id is not configured")
	}
	keyFile := cfg.GitHub.KeyFile
	if s.deps.ResolveAppKeyFileFunc != nil {
		keyFile = s.deps.ResolveAppKeyFileFunc(cfg.GitHub.KeyFile, cfg.GitHub.AppID)
	}
	if strings.TrimSpace(keyFile) == "" {
		return fmt.Errorf("github app key file is not configured")
	}
	auth, err := github.NewAppAuth(cfg.GitHub.AppID, installationID, keyFile, s.logger, cfg.GitHub.ResolvedAPIURL())
	if err != nil {
		return err
	}
	ctx := r.Context()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	// /gh-setup is public because GitHub redirects a fresh browser here without
	// a hive session. We still do not trust the query string: the App JWT call
	// below proves the ID exists for this App and that its account is exactly
	// this hive's configured org, preventing installation hijacking.
	return auth.VerifyInstallationForOrg(ctx, installationID, configuredProjectOrgForGitHubApp(cfg, s.logger))
}

func (s *Server) persistGitHubSetupInstallation(r *http.Request, installationID int64) error {
	s.githubConfigMu.Lock()
	defer s.githubConfigMu.Unlock()

	cfg := s.deps.Config
	if cfg.GitHub.InstallationID != 0 && cfg.GitHub.InstallationID != installationID && !s.requestHasGitHubSetupAdmin(r) {
		return fmt.Errorf("installation_id is already configured; authenticated admin required to replace it")
	}
	cfg.GitHub.InstallationID = installationID
	result, err := s.finishGitHubConfigUpdateLocked(requestAuditUser(r),
		auditDetail("source", "setup_url", "installation_id", strconv.FormatInt(installationID, 10)))
	if err != nil {
		return err
	}
	if result["reinit"] == "failed" {
		return fmt.Errorf("reinitializing github client after setup callback: %v", result["reinit_error"])
	}
	return nil
}

func (s *Server) requestHasGitHubSetupAdmin(r *http.Request) bool {
	if sess := s.sessionFromRequest(r); sess != nil {
		// Authz decision: use the LIVE allowlist role (session_live_role.go),
		// never the role frozen into the session at login — a downgrade or
		// revocation must strip GitHub-setup admin immediately, and a granted
		// owner must gain it without re-login (same class as #4299).
		live, ok := s.liveSessionRole(sess)
		return ok && config.RoleAtLeast(live, config.RoleReadWrite)
	}
	role := r.Header.Get("X-Hive-Role")
	if !config.RoleAtLeast(role, config.RoleReadWrite) {
		return false
	}
	if s.directRouteAuthzEnabled() {
		return false
	}
	proof := r.Header.Get(proxyAuthHeader)
	return s.authToken != "" && proof != "" && secureCompare(proof, s.authToken)
}
