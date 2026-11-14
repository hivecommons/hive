package hub

// Self-service (byo-) hive registration API (#18060, step 3 of the RFC; the
// store it writes to is selfservice_registry.go, the bearer check is in
// hub_keys.go, and the credential fence is heartbeat_selfservice_fence.go).
//
// A signed-in hub user registers a hive they run on their own infrastructure,
// proves they administer every repo it will work on with THEIR OWN stored
// GitHub token, and receives a random heartbeat key exactly once. They can list
// their own entries, rotate a key, and revoke a hive; a hub admin can revoke
// anyone's. The whole family sits behind HIVE_HUB_SELFSERVICE_REGISTRATION
// (default off) and answers 404 when the flag is off so a hub that does not
// offer self-service does not advertise the routes. GET /status is the one
// exception: it always answers so the dashboard can decide whether the
// Register button opens the form or the self-hosting guide.
//
// Invariants:
//   - Every ID this file produces starts with SelfServiceHiveIDPrefix and is
//     built as byo-<github login>--<name>; nothing a caller sends can escape it,
//     and no two (login, name) pairs produce the same ID (#18166).
//   - The raw key is in the 201 and the rotate body and nowhere else: not in
//     the list route, not in a log line, not on disk (only its hash is).
//   - HIVE_HUB_SECRET is never consulted here and never appears in a response.
//   - A caller learns nothing about another user's hive: wrong owner is 404,
//     the same answer as a hive that does not exist.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// selfServiceRegistrationEnv turns the registration API on. Off by default:
	// a hub that does not want to list owner-run hives never exposes it.
	selfServiceRegistrationEnv = "HIVE_HUB_SELFSERVICE_REGISTRATION"
	// selfServiceMaxPerUserEnv caps how many byo- hives one user may register.
	selfServiceMaxPerUserEnv     = "HIVE_HUB_SELFSERVICE_MAX_PER_USER"
	selfServiceMaxPerUserDefault = 5
	// selfServiceExpiryEnv is how long a registered hive may go without an
	// accepted heartbeat before the expiry sweep (selfservice_expiry.go)
	// removes it. Read here so the status route can tell the form the window.
	selfServiceExpiryEnv     = "HIVE_HUB_SELFSERVICE_EXPIRY"
	selfServiceExpiryDefault = 24 * time.Hour

	// selfServiceMaxRequestBody bounds a registration body; the largest legal
	// body is selfServiceMaxRepos repo refs plus a URL, far under this.
	selfServiceMaxRequestBody = 64 << 10
	// selfServiceMaxRepos bounds how many repos one registration may prove
	// ownership of, and so how many GitHub calls one request can trigger.
	selfServiceMaxRepos = 20
	// selfServiceGitHubAPIBase is the production API the ownership proof
	// calls; tests point selfServiceOwnershipChecker.apiBase at a stub.
	selfServiceGitHubAPIBase = "https://api.github.com"
	// selfServiceOwnershipErrorBodyLimit bounds how much of a GitHub error body
	// is read before it is discarded. The body is never logged or returned.
	selfServiceOwnershipErrorBodyLimit = 512

	// selfServiceDashboardURLScheme is the only scheme a dashboard_url may use.
	selfServiceDashboardURLScheme = "https"

	// selfServiceIDSeparator joins the GitHub login and the hive name in a
	// byo- ID. A single '-' was ambiguous because logins contain hyphens
	// (login a-b + name c and login a + name b-c both gave byo-a-b-c, #18166).
	// GitHub logins never contain "--" and never end in '-', so the first "--"
	// after the prefix is always the boundary; selfServiceBuildID refuses a
	// login that breaks either rule rather than mint an ambiguous ID.
	selfServiceIDSeparator = "--"
)

// selfServiceReservedNamePrefixes are hive ID namespaces a self-service name
// may not borrow. The final ID is always byo-<login>--<name>, so none of these
// could become the ID's prefix; refusing them in the NAME keeps a registration
// from reading like a hosted hive in the dashboard (byo-alice--hosted-prod).
var selfServiceReservedNamePrefixes = []string{"hosted-", "saas-", SelfServiceHiveIDPrefix}

// selfServiceRefusalReasons are the audit-log reason values for
// selfservice_hive_refused.
const (
	selfServiceRefusedInvalidName    = "invalid_name"
	selfServiceRefusedInvalidLogin   = "invalid_login"
	selfServiceRefusedReservedPrefix = "reserved_prefix"
	selfServiceRefusedDuplicateID    = "duplicate_id"
	selfServiceRefusedInvalidRepos   = "invalid_repos"
	selfServiceRefusedInvalidURL     = "invalid_dashboard_url"
	selfServiceRefusedCap            = "per_user_cap"
	selfServiceRefusedNoToken        = "no_github_token"
	selfServiceRefusedNotAdmin       = "not_repo_admin"
	selfServiceRefusedNoGitHubLogin  = "no_github_login"
	selfServiceRefusedOwnershipError = "ownership_check_error"
)

// selfServiceRegistrationEnabled reads the feature flag on every call so an
// operator can flip it without a restart of their test harness; the hub is
// env-configured and reads it at request time like the other hub toggles.
func selfServiceRegistrationEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(selfServiceRegistrationEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func selfServiceMaxPerUser() int {
	return positiveIntEnv(selfServiceMaxPerUserEnv, selfServiceMaxPerUserDefault)
}

func selfServiceExpiry() time.Duration {
	return durationEnv(selfServiceExpiryEnv, selfServiceExpiryDefault)
}

// selfServiceOwnershipChecker asks GitHub whether a token's user administers
// a repo. It is a struct rather than a function so the production instance can
// own its SSRF-guarded client while a test substitutes an httptest server.
type selfServiceOwnershipChecker struct {
	apiBase string
	client  *http.Client
}

func defaultSelfServiceOwnershipChecker() *selfServiceOwnershipChecker {
	return &selfServiceOwnershipChecker{apiBase: selfServiceGitHubAPIBase, client: liteRepoAccessHTTPClient()}
}

// ownershipChecker returns the injected checker or the production default.
func (s *HubServer) ownershipChecker() *selfServiceOwnershipChecker {
	if s != nil && s.selfServiceOwnership != nil {
		return s.selfServiceOwnership
	}
	return defaultSelfServiceOwnershipChecker()
}

// repoAdmin reports whether token's user has permissions.admin on owner/repo.
// A 401, 403 or 404 means "no" (GitHub answers 404 for a private repo the
// token cannot see); any other non-2xx is an error, reported by status only so
// nothing GitHub echoes back can reach a log or a response. The RFC requires
// admin specifically; maintain is not accepted (unlike the lite-enrollment
// check, which has different stakes).
func (c *selfServiceOwnershipChecker) repoAdmin(ctx context.Context, token, owner, repo string) (bool, error) {
	u, err := url.JoinPath(c.apiBase, "repos", owner, repo)
	if err != nil {
		return false, fmt.Errorf("build GitHub repo URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, fmt.Errorf("build GitHub ownership check: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("GitHub ownership check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return false, nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, selfServiceOwnershipErrorBodyLimit))
		return false, fmt.Errorf("GitHub ownership check returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Permissions struct {
			Admin bool `json:"admin"`
		} `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return false, fmt.Errorf("decode GitHub ownership check: %w", err)
	}
	return payload.Permissions.Admin, nil
}

// selfServiceRegisterRequest is the body of POST /api/hub/selfservice/hives.
type selfServiceRegisterRequest struct {
	Name         string   `json:"name"`
	Org          string   `json:"org"`
	Repos        []string `json:"repos"`
	DashboardURL string   `json:"dashboard_url"`
}

// selfServiceHiveView is what the API shows about a registration. It is built
// from SelfServiceHive by hand so a future field on the stored record (a hash,
// a key) can never leak by default.
type selfServiceHiveView struct {
	ID                      string    `json:"id"`
	Owner                   string    `json:"owner"`
	Org                     string    `json:"org,omitempty"`
	Repos                   []string  `json:"repos,omitempty"`
	DashboardURL            string    `json:"dashboard_url,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
	OwnershipCheckedAt      time.Time `json:"ownership_checked_at,omitempty"`
	LastAcceptedHeartbeatAt time.Time `json:"last_accepted_heartbeat_at,omitempty"`
	// Online mirrors the registry entry the hive's heartbeats create; false
	// until the first accepted heartbeat.
	Online bool `json:"online"`
}

// selfServiceKeySnippets are copy-paste deployment stubs that reference the
// three variables a self-hosted spoke needs. The key is substituted ONCE into
// the response; the templates themselves carry only the variable names.
type selfServiceKeySnippets struct {
	DockerCompose string `json:"docker_compose"`
	Podman        string `json:"podman"`
	Kubernetes    string `json:"kubernetes"`
}

// selfServiceSpokeImage is the image the snippets reference.
const selfServiceSpokeImage = "ghcr.io/hivecommons/hive:stable"

func selfServiceSnippets(hubURL, hiveID, rawKey string) selfServiceKeySnippets {
	compose := fmt.Sprintf(`services:
  hive:
    image: %s
    environment:
      HIVE_HUB_URL: %q
      HIVE_ID: %q
      HIVE_HEARTBEAT_KEY: %q
`, selfServiceSpokeImage, hubURL, hiveID, rawKey)
	podman := fmt.Sprintf("podman run -d --name hive \\\n  -e HIVE_HUB_URL=%q \\\n  -e HIVE_ID=%q \\\n  -e HIVE_HEARTBEAT_KEY=%q \\\n  %s\n", hubURL, hiveID, rawKey, selfServiceSpokeImage)
	k8s := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: hive-hub-credentials
type: Opaque
stringData:
  HIVE_HUB_URL: %q
  HIVE_ID: %q
  HIVE_HEARTBEAT_KEY: %q
`, hubURL, hiveID, rawKey)
	return selfServiceKeySnippets{DockerCompose: compose, Podman: podman, Kubernetes: k8s}
}

func selfServiceView(h SelfServiceHive, online bool) selfServiceHiveView {
	return selfServiceHiveView{
		ID: h.ID, Owner: h.Owner, Org: h.Org, Repos: h.Repos, DashboardURL: h.DashboardURL,
		CreatedAt: h.CreatedAt, OwnershipCheckedAt: h.OwnershipCheckedAt,
		LastAcceptedHeartbeatAt: h.LastAcceptedHeartbeatAt, Online: online,
	}
}

// selfServiceOwns reports whether login is the owner of h. Both sides are
// canonicalized so a bare login and its github: form compare equal.
func selfServiceOwns(h SelfServiceHive, login string) bool {
	return login != "" && canonicalizeLegacy(h.Owner) == canonicalizeLegacy(login)
}

// selfServiceCount is how many hives login has registered.
func (s *HubServer) selfServiceCount(login string) int {
	n := 0
	for _, h := range s.selfService.list() {
		if selfServiceOwns(h, login) {
			n++
		}
	}
	return n
}

// selfServiceRegistryHas reports whether id is in the general registry.
func (s *HubServer) selfServiceRegistryHas(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if strings.EqualFold(h.ID, id) {
			return true
		}
	}
	return false
}

func (s *HubServer) selfServiceOnline(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.registry.Hives {
		if h.ID == id {
			return h.Online
		}
	}
	return false
}

// requireSelfServiceEnabled answers 404 when the flag is off so the route
// family is not discoverable on a hub that does not offer it.
func (s *HubServer) requireSelfServiceEnabled(w http.ResponseWriter) bool {
	if selfServiceRegistrationEnabled() {
		return true
	}
	writeJSONError(w, http.StatusNotFound, "not found")
	return false
}

func (s *HubServer) auditSelfServiceRefused(login, hiveID, reason string, extra ...any) {
	args := append([]any{"user", login, "hive", hiveID, "reason", reason}, extra...)
	s.logger.Info("audit: selfservice_hive_refused", args...)
}

// handleSelfServiceStatus is GET /api/hub/selfservice/status.
func (s *HubServer) handleSelfServiceStatus(w http.ResponseWriter, r *http.Request) {
	login := s.getAuthUser(r)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"enabled":      selfServiceRegistrationEnabled(),
		"max_per_user": selfServiceMaxPerUser(),
		"count":        s.selfServiceCount(login),
		"expiry":       selfServiceExpiry().String(),
	})
}

// handleSelfServiceList is GET /api/hub/selfservice/hives: the caller's own
// registrations, never a hash or a key.
func (s *HubServer) handleSelfServiceList(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	login := s.getAuthUser(r)
	views := make([]selfServiceHiveView, 0)
	for _, h := range s.selfService.list() {
		if selfServiceOwns(h, login) {
			views = append(views, selfServiceView(h, s.selfServiceOnline(h.ID)))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"hives": views})
}

// selfServiceValidateRepos checks every repo ref is owner/name and that there
// is at least one: ownership proof is the whole point, and it needs a repo.
func selfServiceValidateRepos(repos []string) ([]string, error) {
	if len(repos) == 0 {
		return nil, errors.New("at least one owner/name repo is required")
	}
	if len(repos) > selfServiceMaxRepos {
		return nil, fmt.Errorf("at most %d repos per registration", selfServiceMaxRepos)
	}
	out := make([]string, 0, len(repos))
	seen := make(map[string]struct{}, len(repos))
	for _, raw := range repos {
		repo := strings.TrimSpace(raw)
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || !isValidRepoRef(repo) || owner == "" || name == "" {
			return nil, fmt.Errorf("repo %q must be owner/name", repo)
		}
		key := strings.ToLower(repo)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, repo)
	}
	return out, nil
}

// selfServiceValidateDashboardURL accepts an empty URL or an https URL to a
// public address, the same guard the hive config proxy applies.
func selfServiceValidateDashboardURL(ctx context.Context, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return "", errors.New("dashboard_url must be an absolute https URL")
	}
	if !strings.EqualFold(u.Scheme, selfServiceDashboardURLScheme) {
		return "", errors.New("dashboard_url must use https")
	}
	if u.User != nil {
		return "", errors.New("dashboard_url must not carry credentials")
	}
	if isPrivateURL(ctx, raw) {
		return "", errors.New("dashboard_url resolves to a private or internal address")
	}
	return u.String(), nil
}

// selfServiceBuildID derives byo-<login>--<name> from the GitHub login and the
// caller's name, refusing names that cannot become a valid hive ID or that
// borrow a reserved namespace, and logins that would make the ID ambiguous.
func selfServiceBuildID(githubLogin, name string) (string, string, error) {
	login := strings.ToLower(strings.TrimSpace(githubLogin))
	if login == "" || !isValidName(login) || strings.Contains(login, selfServiceIDSeparator) ||
		strings.HasPrefix(login, "-") || strings.HasSuffix(login, "-") {
		return "", selfServiceRefusedInvalidLogin, errors.New("your GitHub login cannot form an unambiguous self-hosted hive ID")
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return "", selfServiceRefusedInvalidName, errors.New("name is required")
	}
	if strings.Contains(name, "/") || !isValidName(name) {
		return "", selfServiceRefusedInvalidName, errors.New("name may contain only letters, digits, '.', '_' and '-'")
	}
	// A leading '-' would merge into the separator (a + -b gives byo-a---b,
	// whose first "--" no longer marks the boundary).
	if strings.HasPrefix(name, "-") {
		return "", selfServiceRefusedInvalidName, errors.New("name may not start with '-'")
	}
	for _, p := range selfServiceReservedNamePrefixes {
		if strings.HasPrefix(name, p) {
			return "", selfServiceRefusedReservedPrefix, fmt.Errorf("name may not start with the reserved prefix %q", p)
		}
	}
	id := SelfServiceHiveIDPrefix + login + selfServiceIDSeparator + name
	if !isValidName(id) || !IsSelfServiceHiveID(id) {
		return "", selfServiceRefusedInvalidName, errors.New("name is too long for a hive ID")
	}
	return id, "", nil
}

// handleSelfServiceRegister is POST /api/hub/selfservice/hives.
func (s *HubServer) handleSelfServiceRegister(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	login := s.getAuthUser(r)
	user := loadSaaSUser(login)
	if user == nil {
		writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}
	var body selfServiceRegisterRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, selfServiceMaxRequestBody)).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	githubLogin := topRepoProfileLogin(user)
	if githubLogin == "" {
		s.auditSelfServiceRefused(login, "", selfServiceRefusedNoGitHubLogin)
		writeJSONError(w, http.StatusBadRequest, "a GitHub identity is required to register a self-hosted hive")
		return
	}
	id, reason, err := selfServiceBuildID(githubLogin, body.Name)
	if err != nil {
		s.auditSelfServiceRefused(login, "", reason)
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.selfServiceRegistryHas(id) {
		s.auditSelfServiceRefused(login, id, selfServiceRefusedDuplicateID)
		writeJSONError(w, http.StatusConflict, "a hive with this ID is already registered")
		return
	}
	if _, exists := s.selfService.get(id); exists {
		s.auditSelfServiceRefused(login, id, selfServiceRefusedDuplicateID)
		writeJSONError(w, http.StatusConflict, "a hive with this ID is already registered")
		return
	}
	org := strings.TrimSpace(body.Org)
	if org != "" && !isValidName(org) {
		s.auditSelfServiceRefused(login, id, selfServiceRefusedInvalidName)
		writeJSONError(w, http.StatusBadRequest, "org may contain only letters, digits, '.', '_' and '-'")
		return
	}
	repos, err := selfServiceValidateRepos(body.Repos)
	if err != nil {
		s.auditSelfServiceRefused(login, id, selfServiceRefusedInvalidRepos)
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	dashboardURL, err := selfServiceValidateDashboardURL(r.Context(), body.DashboardURL)
	if err != nil {
		s.auditSelfServiceRefused(login, id, selfServiceRefusedInvalidURL)
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if maxPerUser := selfServiceMaxPerUser(); s.selfServiceCount(login) >= maxPerUser {
		s.auditSelfServiceRefused(login, id, selfServiceRefusedCap, "max_per_user", maxPerUser)
		writeJSONError(w, http.StatusTooManyRequests, fmt.Sprintf("you already have %d self-hosted hives registered; remove one first", maxPerUser))
		return
	}

	// Ownership proof with the user's OWN token, never the hub's App token: the
	// question is what this person administers, not what the hub can see.
	if strings.TrimSpace(user.EncryptedToken) == "" {
		s.auditSelfServiceRefused(login, id, selfServiceRefusedNoToken)
		writeJSONError(w, http.StatusForbidden, "no GitHub token is stored for your account; sign out and back in with GitHub, then retry")
		return
	}
	token, err := decryptToken(user.EncryptedToken)
	if err != nil {
		s.logger.Warn("self-service registration: stored token could not be decrypted", "user", login, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "stored GitHub token could not be read")
		return
	}
	checker := s.ownershipChecker()
	for _, repo := range repos {
		owner, name, _ := strings.Cut(repo, "/")
		admin, err := checker.repoAdmin(r.Context(), token, owner, name)
		if err != nil {
			s.auditSelfServiceRefused(login, id, selfServiceRefusedOwnershipError, "repo", repo)
			s.logger.Warn("self-service registration: ownership check failed", "user", login, "hive", id, "repo", repo, "error", err)
			writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("could not verify your permission on %s; try again", repo))
			return
		}
		if !admin {
			s.auditSelfServiceRefused(login, id, selfServiceRefusedNotAdmin, "repo", repo)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": fmt.Sprintf("your GitHub account is not an admin of %s", repo),
				"repo":  repo,
			})
			return
		}
	}

	now := time.Now().UTC()
	// The cap is enforced again inside the store, under the lock that inserts
	// the record: the count above ran before up to selfServiceMaxRepos GitHub
	// calls, so concurrent POSTs from one user could all have passed it.
	maxPerUser := selfServiceMaxPerUser()
	rawKey, err := s.selfService.registerCapped(SelfServiceHive{
		ID: id, Owner: login, Org: org, Repos: repos, DashboardURL: dashboardURL, OwnershipCheckedAt: now,
	}, maxPerUser)
	if err != nil {
		if errors.Is(err, errSelfServiceExists) {
			s.auditSelfServiceRefused(login, id, selfServiceRefusedDuplicateID)
			writeJSONError(w, http.StatusConflict, "a hive with this ID is already registered")
			return
		}
		if errors.Is(err, errSelfServiceCapReached) {
			s.auditSelfServiceRefused(login, id, selfServiceRefusedCap, "max_per_user", maxPerUser)
			writeJSONError(w, http.StatusTooManyRequests, fmt.Sprintf("you already have %d self-hosted hives registered; remove one first", maxPerUser))
			return
		}
		s.logger.Error("self-service registration: store write failed", "user", login, "hive", id, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "could not persist the registration")
		return
	}
	s.logger.Info("audit: selfservice_hive_registered", "user", login, "hive", id, "repos", len(repos), "org", org)

	stored, _ := s.selfService.get(id)
	hubURL := hubPublicURL()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"hive":     selfServiceView(stored, false),
		"key":      rawKey,
		"hub_url":  hubURL,
		"snippets": selfServiceSnippets(hubURL, id, rawKey),
	})
}

// selfServiceOwnedHive loads the hive named in the path and checks login owns
// it (or, when allowAdmin, that login is a hub admin). A missing hive and
// someone else's hive both answer 404 so the route reveals nothing.
func (s *HubServer) selfServiceOwnedHive(w http.ResponseWriter, r *http.Request, login string, allowAdmin bool) (SelfServiceHive, bool) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	h, ok := s.selfService.get(id)
	if !ok || !(selfServiceOwns(h, login) || (allowAdmin && isHubAdmin(login))) {
		writeJSONError(w, http.StatusNotFound, "self-hosted hive not found")
		return SelfServiceHive{}, false
	}
	return h, true
}

// handleSelfServiceRotate is POST /api/hub/selfservice/hives/{id}/rotate.
func (s *HubServer) handleSelfServiceRotate(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	login := s.getAuthUser(r)
	h, ok := s.selfServiceOwnedHive(w, r, login, false)
	if !ok {
		return
	}
	rawKey, err := s.selfService.rotate(h.ID)
	if err != nil {
		if errors.Is(err, errSelfServiceNotFound) {
			writeJSONError(w, http.StatusNotFound, "self-hosted hive not found")
			return
		}
		s.logger.Error("self-service rotation: store write failed", "user", login, "hive", h.ID, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "could not persist the new key")
		return
	}
	s.logger.Info("audit: selfservice_hive_rotated", "user", login, "hive", h.ID, "overlap", selfServiceKeyRotationOverlap.String())
	hubURL := hubPublicURL()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":       h.ID,
		"key":      rawKey,
		"hub_url":  hubURL,
		"overlap":  selfServiceKeyRotationOverlap.String(),
		"snippets": selfServiceSnippets(hubURL, h.ID, rawKey),
	})
}

// handleSelfServiceDelete is DELETE /api/hub/selfservice/hives/{id}: the
// owner or a hub admin revokes the key and removes the registry entry.
func (s *HubServer) handleSelfServiceDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireSelfServiceEnabled(w) {
		return
	}
	login := s.getAuthUser(r)
	h, ok := s.selfServiceOwnedHive(w, r, login, true)
	if !ok {
		return
	}
	removed, err := s.selfService.revoke(h.ID)
	if err != nil {
		s.logger.Error("self-service revocation: store write failed", "user", login, "hive", h.ID, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "could not persist the revocation")
		return
	}
	s.removeRegistryEntry(h.ID, login)
	s.logger.Info("audit: selfservice_hive_revoked", "user", login, "hive", h.ID, "owner", h.Owner, "by_admin", !selfServiceOwns(h, login))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"removed": removed, "id": h.ID})
}
