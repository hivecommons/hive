package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kubestellar/hive/pkg/config"
	hivegithub "github.com/kubestellar/hive/pkg/github"
)

const (
	liteDefaultACMMLevel      = 2
	liteMaxACMMLevel          = 2
	liteRepoAccessHTTPTimeout = 10 * time.Second
)

type LiteEnrollmentConfig struct {
	Mode        string   `json:"mode"`
	ACMMLevel   int      `json:"acmmLevel"`
	Lanes       []string `json:"lanes"`
	Agents      []string `json:"agents"`
	Advisory    bool     `json:"advisory"`
	ZeroSecrets bool     `json:"zeroSecrets"`
}

type LiteEnrollmentRequest struct {
	Owner          string `json:"owner"`
	Repo           string `json:"repo"`
	InstallationID int64  `json:"installation_id,omitempty"`
	ACMMLevel      int    `json:"acmm_level,omitempty"`
	GitHubHost     string `json:"github_host,omitempty"`
}

type LiteEnrollmentResponse struct {
	ID             string               `json:"id"`
	Mode           string               `json:"mode"`
	Owner          string               `json:"owner"`
	Repo           string               `json:"repo"`
	InstallationID int64                `json:"installation_id,omitempty"`
	ACMMLevel      int                  `json:"acmm_level"`
	DashboardURL   string               `json:"dashboard_url"`
	Config         LiteEnrollmentConfig `json:"config"`
	Existing       bool                 `json:"existing"`
	Action         string               `json:"action"`
	SpokeID        string               `json:"spoke_id"`
	SpokeStatus    string               `json:"spoke_status,omitempty"`
	NextSteps      []string             `json:"next_steps"`
}

var verifyLiteRepoAccess = verifyGitHubRepoAccess

func (s *HubServer) handleLiteEnroll(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	user := ensureSaaSUser(username)
	if user == nil || user.Blocked {
		writeJSONError(w, http.StatusForbidden, "account blocked or not found")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req LiteEnrollmentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Owner = strings.TrimSpace(req.Owner)
	req.Repo = normalizeRepoRef(req.Repo)
	if req.Owner == "" || req.Repo == "" {
		writeJSONError(w, http.StatusBadRequest, "owner and repo are required")
		return
	}
	if !isValidName(req.Owner) || !isValidRepoRef(req.Repo) {
		writeJSONError(w, http.StatusBadRequest, "invalid repo")
		return
	}
	host := githubHostLabel(req.GitHubHost)
	if host == "" {
		host = publicGitHubHost
	}
	if !isValidName(host) {
		writeJSONError(w, http.StatusBadRequest, "invalid github_host")
		return
	}
	if err := validateLiteGitHubHost(r.Context(), host); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	token := bearerTokenFromRequest(r)
	if token == "" {
		writeJSONError(w, http.StatusUnauthorized, "hivectl enroll requires a GitHub bearer token so the hub can verify repo admin access")
		return
	}
	ok, err := verifyLiteRepoAccess(r.Context(), token, host, req.Owner, req.Repo)
	if err != nil {
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	}
	if !ok {
		writeJSONError(w, http.StatusForbidden, "GitHub token does not have admin or maintain access to the repo")
		return
	}
	acmm := req.ACMMLevel
	if acmm <= 0 {
		acmm = liteDefaultACMMLevel
	}
	if acmm > liteMaxACMMLevel {
		acmm = liteMaxACMMLevel
	}

	cfg := defaultLiteEnrollmentConfig(acmm)

	spoke, existing := s.findOwnedLiteTargetSpoke(username, req.Owner, host)
	if existing {
		if err := s.addLiteRepoToSpoke(spoke, req.Owner, req.Repo, acmm, host); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp := LiteEnrollmentResponse{
			ID:           spoke.ID,
			Mode:         "lite",
			Owner:        req.Owner,
			Repo:         req.Repo,
			ACMMLevel:    acmm,
			DashboardURL: s.dashboardURLForRequest(r),
			Config:       cfg,
			Existing:     true,
			Action:       "spoke_config_update",
			SpokeID:      spoke.ID,
			SpokeStatus:  spoke.Status,
			NextSteps: []string{
				"Wait for the spoke heartbeat to apply the updated repo list.",
				"Open the spoke dashboard and review advisory findings.",
				"Raise ACMM from L1 to L2 only after advisory noise is acceptable.",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	if user.SaaSQuota == 0 {
		writeJSONError(w, http.StatusForbidden, "no hosted hive quota — contact the hub admin to request access")
		return
	}
	if user.SaaSQuota > 0 && countUserHives(username) >= user.SaaSQuota {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("quota reached — max %d SaaS hives", user.SaaSQuota))
		return
	}
	if maxSaaSHivesTotal > 0 && len(listSaaSHives()) >= maxSaaSHivesTotal {
		writeJSONError(w, http.StatusServiceUnavailable, "hosted capacity reached — try again later")
		return
	}

	installationID := req.InstallationID
	if installationID == 0 {
		discovered, err := s.discoverLiteInstallation(r.Context(), req.Owner)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		installationID = discovered
	}

	spoke, err = s.requestHostedLiteSpoke(username, req.Owner, req.Repo, host, installationID, acmm)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	user.Hives[spoke.ID] = "owner"
	_ = saveSaaSUser(user)

	resp := LiteEnrollmentResponse{
		ID:             spoke.ID,
		Mode:           "lite",
		Owner:          req.Owner,
		Repo:           req.Repo,
		InstallationID: installationID,
		ACMMLevel:      acmm,
		DashboardURL:   s.dashboardURLForRequest(r),
		Config:         cfg,
		Existing:       false,
		Action:         "hosted_lite_spoke_requested",
		SpokeID:        spoke.ID,
		SpokeStatus:    spoke.Status,
		NextSteps: []string{
			"Wait for the hosted lite spoke to come online.",
			"Open the spoke dashboard and watch advisory findings for this repo.",
			"Raise ACMM from L1 to L2 only after advisory noise is acceptable.",
			"Graduate to a full spoke when you need execution, private runtime, or higher ACMM levels.",
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *HubServer) findOwnedLiteTargetSpoke(username, owner, host string) (*SaaSHive, bool) {
	wantHost := githubHostLabel(host)
	if wantHost == "" {
		wantHost = publicGitHubHost
	}
	hives := listSaaSHives()
	for i := range hives {
		h := &hives[i]
		if h.Owner != username || h.Status == statusAvailable {
			continue
		}
		if !strings.EqualFold(h.Org, owner) {
			continue
		}
		if !s.liteSpokeProfileCompatible(h) {
			continue
		}
		if gotHost := s.spokeGitHubHostForLiteMatch(h); gotHost != wantHost {
			continue
		}
		return h, true
	}
	return nil, false
}

func (s *HubServer) liteSpokeProfileCompatible(h *SaaSHive) bool {
	if h == nil {
		return false
	}
	if s.registrySpokeIsLite(h.ID) {
		return true
	}
	return h.ACMMLevel == 0 || h.ACMMLevel <= liteMaxACMMLevel
}

func (s *HubServer) spokeGitHubHostForLiteMatch(h *SaaSHive) string {
	if h == nil {
		return publicGitHubHost
	}
	host := githubHostLabel(h.GitHubHost)
	if host == "" {
		if c := s.clusterForHive(h); c != nil {
			host = githubHostLabel(effectiveGitHubBaseURL(h, c))
		}
	}
	if host == "" {
		return publicGitHubHost
	}
	return host
}

func (s *HubServer) addLiteRepoToSpoke(h *SaaSHive, owner, repo string, acmm int, host string) error {
	if h == nil {
		return fmt.Errorf("spoke not found")
	}
	isLite := s.registrySpokeIsLite(h.ID)
	if h.Org == "" {
		h.Org = owner
	}
	if h.PrimaryRepo == "" {
		h.PrimaryRepo = repo
	}
	found := false
	for _, existing := range h.Repos {
		if strings.EqualFold(existing, repo) {
			found = true
			break
		}
	}
	if !found {
		h.Repos = append(h.Repos, repo)
	}
	if h.ACMMLevel == 0 || (isLite && h.ACMMLevel > liteMaxACMMLevel) {
		h.ACMMLevel = acmm
	}
	if h.RequestedACMMLevel == 0 || (isLite && h.RequestedACMMLevel > liteMaxACMMLevel) {
		h.RequestedACMMLevel = h.ACMMLevel
	}
	h.ClaimDelivered = false
	h.ACMMDelivered = false
	if host != "" && host != publicGitHubHost && h.GitHubHost == "" {
		h.GitHubHost = host
	}
	if err := saveSaaSHive(h); err != nil {
		return fmt.Errorf("save spoke config request: %w", err)
	}
	return nil
}

func (s *HubServer) registrySpokeIsLite(hiveID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == hiveID {
			return s.registry.Hives[i].Lite || s.registry.Hives[i].HiveType == "lite"
		}
	}
	return false
}

func (s *HubServer) requestHostedLiteSpoke(username, owner, repo, host string, installationID int64, acmm int) (*SaaSHive, error) {
	clusterID := defaultClusterID
	cluster, ok := s.clusters[clusterID]
	if !ok {
		return nil, fmt.Errorf("default hosted-spoke cluster %q is not configured", clusterID)
	}
	hiveID := generateHiveID(owner, repo)
	now := time.Now().UTC().Format(time.RFC3339)
	h := &SaaSHive{
		ID:                 hiveID,
		Owner:              username,
		ProjectName:        "Hive lite: " + owner + "/" + repo,
		Org:                owner,
		Repos:              []string{repo},
		PrimaryRepo:        repo,
		ACMMLevel:          acmm,
		RequestedACMMLevel: acmm,
		ClaimDelivered:     false,
		ACMMDelivered:      false,
		Status:             "provisioning",
		CreatedAt:          now,
		Subdomain:          hiveID + "." + cluster.Domain,
		ClusterID:          clusterID,
		IsPublic:           false,
		AutoUpgrade:        true,
	}
	if host != "" && host != publicGitHubHost {
		h.GitHubHost = host
		h.GitHubBaseURL = "https://" + host
		h.GitHubAPIURL = gheAPIURLForHost(host)
	}
	if err := saveSaaSHive(h); err != nil {
		return nil, fmt.Errorf("save hosted lite spoke metadata: %w", err)
	}
	user := ensureSaaSUser(username)
	user.Hives[hiveID] = saasRoleOwner
	_ = saveSaaSUser(user)

	identity := ResolveHiveIdentity(h, &cluster)
	appID := identity.AppID
	if appID == 0 {
		appID = config.PlaceholderAppID
	}
	req := &CreateHiveRequest{
		Org:            owner,
		Repos:          repo,
		PrimaryRepo:    repo,
		ProjectName:    h.ProjectName,
		ACMMLevel:      acmm,
		ClusterID:      clusterID,
		AuthMethod:     "app",
		AppID:          strconv.FormatInt(appID, 10),
		InstallationID: strconv.FormatInt(installationID, 10),
		AppPrivateKey:  loadClusterAppKey(clusterID),
		GitHubBaseURL:  h.GitHubBaseURL,
		GitHubAPIURL:   h.GitHubAPIURL,
	}
	provisionHiveRecord := *h
	provisionHiveRecord.Repos = append([]string(nil), h.Repos...)
	// Queued, not spawned — bounded hub-wide and per cluster (provision_queue.go).
	enqueueProvision(clusterID, func() {
		provisionedHive := &provisionHiveRecord
		if err := provisionHive(provisionedHive, req, &cluster, s.appKeysByAppID(), s.logger); err != nil {
			provisionedHive.Status = "error"
			provisionedHive.Error = err.Error()
			_ = saveSaaSHive(provisionedHive)
			s.logger.Warn("hosted lite spoke provision failed", "hive_id", hiveID, "error", err)
			return
		}
		provisionedHive.Status = "provisioning"
		_ = saveSaaSHive(provisionedHive)
	})
	s.updateRegistryForLiteSpoke(h)
	return h, nil
}

func (s *HubServer) updateRegistryForLiteSpoke(h *SaaSHive) {
	if h == nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	cfg := defaultLiteEnrollmentConfig(cappedLiteACMM(h.ACMMLevel))
	entry := RegistryEntry{
		ID:           h.ID,
		Name:         h.ProjectName,
		Org:          h.Org,
		Repos:        append([]string(nil), h.Repos...),
		PrimaryRepo:  h.PrimaryRepo,
		ACMMLevel:    cappedLiteACMM(h.ACMMLevel),
		GovernorMode: "ADVISORY",
		Owner:        h.Owner,
		HiveType:     "lite",
		Lite:         true,
		LiteConfig:   &cfg,
		IsPublic:     false,
		RegisteredAt: func() string {
			if h.CreatedAt != "" {
				return h.CreatedAt
			}
			return now
		}(),
		GitHubHost: h.GitHubHost,
		Online:     false,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == h.ID {
			entry.LastHeartbeat = s.registry.Hives[i].LastHeartbeat
			entry.DashboardURL = s.registry.Hives[i].DashboardURL
			entry.Online = s.registry.Hives[i].Online
			s.registry.Hives[i] = entry
			s.registry.UpdatedAt = now
			s.requestSave()
			return
		}
	}
	s.registry.Hives = append(s.registry.Hives, entry)
	s.registry.UpdatedAt = now
	s.requestSave()
}

func defaultLiteEnrollmentConfig(acmm int) LiteEnrollmentConfig {
	return LiteEnrollmentConfig{
		Mode:        "lite",
		ACMMLevel:   acmm,
		Lanes:       []string{"security", "quality", "maintenance"},
		Agents:      []string{"advisor", "triage"},
		Advisory:    true,
		ZeroSecrets: true,
	}
}

func (s *HubServer) discoverLiteInstallation(ctx context.Context, owner string) (int64, error) {
	clusterID := defaultClusterID
	identity := s.appIdentityForCluster(clusterID)
	if identity == nil || identity.AppID == 0 || identity.PrivateKey == "" {
		return 0, fmt.Errorf("installation_id is required because the hub has no GitHub App key for auto-discovery")
	}
	auth, err := hivegithub.NewAppAuthFromPEM(identity.AppID, 0, []byte(identity.PrivateKey), s.logger, identity.APIURL)
	if err != nil {
		return 0, fmt.Errorf("hub GitHub App key is not usable for discovery: %w", err)
	}
	id, err := auth.DiscoverInstallationID(ctx, owner)
	if err != nil {
		return 0, fmt.Errorf("GitHub App installation not found for %s: %w", owner, err)
	}
	return id, nil
}

func cappedLiteACMM(level int) int {
	if level <= 0 {
		return liteDefaultACMMLevel
	}
	if level > liteMaxACMMLevel {
		return liteMaxACMMLevel
	}
	return level
}

func (s *HubServer) dashboardURLForRequest(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		if r.TLS != nil {
			proto = "https"
		} else {
			proto = "http"
		}
	}
	host := r.Header.Get("X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return "/dashboard"
	}
	return proto + "://" + host + "/dashboard"
}

func bearerTokenFromRequest(r *http.Request) string {
	if token := strings.TrimSpace(r.Header.Get("X-Hive-GitHub-Token")); token != "" {
		return token
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}

func validateLiteGitHubHost(ctx context.Context, host string) error {
	if host == "" || host == publicGitHubHost {
		return nil
	}
	if isPrivateURL(ctx, "https://"+host) {
		return fmt.Errorf("github_host resolves to a private/internal address")
	}
	return nil
}

func liteRepoAccessHTTPClient() *http.Client {
	return &http.Client{
		Timeout: liteRepoAccessHTTPTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			if isPrivateURL(req.Context(), req.URL.String()) {
				return fmt.Errorf("redirect to private/internal github_host blocked")
			}
			return nil
		},
	}
}

func verifyGitHubRepoAccess(ctx context.Context, token, host, owner, repo string) (bool, error) {
	if err := validateLiteGitHubHost(ctx, host); err != nil {
		return false, err
	}
	apiBase := "https://api.github.com"
	if host != "" && host != publicGitHubHost {
		apiBase = "https://" + host + "/api/v3"
	}
	u, err := url.JoinPath(apiBase, "repos", owner, repo)
	if err != nil {
		return false, fmt.Errorf("invalid GitHub API URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, fmt.Errorf("build GitHub repo access check: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := liteRepoAccessHTTPClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("GitHub repo access check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized {
		return false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("GitHub repo access check returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var payload struct {
		Permissions struct {
			Admin    bool `json:"admin"`
			Maintain bool `json:"maintain"`
		} `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, fmt.Errorf("decode GitHub repo access check: %w", err)
	}
	return payload.Permissions.Admin || payload.Permissions.Maintain, nil
}
