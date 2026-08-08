package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	hivegithub "github.com/kubestellar/hive/v2/pkg/github"
)

const (
	liteDefaultACMMLevel = 2
	liteMaxACMMLevel     = 2
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
	NextSteps      []string             `json:"next_steps"`
	Deferred       []string             `json:"deferred"`
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

	installationID := req.InstallationID
	if installationID == 0 {
		discovered, err := s.discoverLiteInstallation(r.Context(), req.Owner)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		installationID = discovered
	}

	id := liteHiveID(host, req.Owner, req.Repo)
	now := time.Now().UTC().Format(time.RFC3339)
	cfg := defaultLiteEnrollmentConfig(acmm)
	entry := RegistryEntry{
		ID:                   id,
		Name:                 req.Owner + "/" + req.Repo,
		Org:                  req.Owner,
		Repos:                []string{req.Repo},
		PrimaryRepo:          req.Repo,
		ACMMLevel:            acmm,
		GovernorMode:         "ADVISORY",
		Owner:                username,
		HiveType:             "lite",
		Lite:                 true,
		LiteConfig:           &cfg,
		IsPublic:             false,
		RegisteredAt:         now,
		GitHubHost:           host,
		GitHubInstallationID: installationID,
		Online:               false,
	}

	existing := false
	s.mu.Lock()
	for i := range s.registry.Hives {
		h := &s.registry.Hives[i]
		if h.ID != id {
			continue
		}
		if !h.Lite && h.HiveType != "lite" {
			s.mu.Unlock()
			writeJSONError(w, http.StatusConflict, "a non-lite hive already uses this enrollment id")
			return
		}
		if h.Owner != "" && h.Owner != username && username != hubAdminUsername {
			s.mu.Unlock()
			writeJSONError(w, http.StatusForbidden, "lite enrollment already belongs to another user")
			return
		}
		entry.RegisteredAt = h.RegisteredAt
		if entry.RegisteredAt == "" {
			entry.RegisteredAt = now
		}
		s.registry.Hives[i] = entry
		existing = true
		break
	}
	if !existing {
		s.registry.Hives = append(s.registry.Hives, entry)
	}
	s.registry.UpdatedAt = now
	s.mu.Unlock()

	user.Hives[id] = "owner"
	_ = saveSaaSUser(user)
	s.requestSave()

	resp := LiteEnrollmentResponse{
		ID:             id,
		Mode:           "lite",
		Owner:          req.Owner,
		Repo:           req.Repo,
		InstallationID: installationID,
		ACMMLevel:      acmm,
		DashboardURL:   s.dashboardURLForRequest(r),
		Config:         cfg,
		Existing:       existing,
		NextSteps: []string{
			"Open the dashboard and watch advisory findings for this repo.",
			"Raise ACMM from L1 to L2 only after advisory noise is acceptable.",
			"Graduate to a full spoke when you need execution, private runtime, or higher ACMM levels.",
		},
		Deferred: []string{"hub-hosted execution", "optional workflow shim"},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
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

func liteHiveID(host, owner, repo string) string {
	host = strings.ToLower(host)
	owner = strings.ToLower(owner)
	repo = strings.ToLower(repo)
	prefix := "lite-"
	if host != "" && !strings.EqualFold(host, publicGitHubHost) {
		prefix += host + "-"
	}
	base := prefix + owner + "-" + repo
	base = strings.NewReplacer("/", "-", "_", "-", ".", "-").Replace(base)
	sum := sha256.Sum256([]byte(host + "/" + owner + "/" + repo))
	suffix := hex.EncodeToString(sum[:])[:8]
	if len(base) > 81 {
		base = base[:81]
	}
	return base + "-" + suffix
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

func verifyGitHubRepoAccess(ctx context.Context, token, host, owner, repo string) (bool, error) {
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
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("GitHub repo access check failed: %w", err)
	}
	defer resp.Body.Close()
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
