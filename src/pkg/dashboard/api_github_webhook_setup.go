package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// Owner-facing GitHub webhook setup (hivecommons/hive#18017): the Settings →
// Forge App → "GitHub webhooks" section. Everything an owner needs to point
// a GitHub org/repo webhook at this hive with a secret of their own, the
// live receiver status, and — when the App installation may manage hooks —
// a one-click hook creation. The secret value leaves the hive exactly once,
// in the response to the generate call; every other response reports only
// whether and from where one is configured.

const (
	auditWebhookSecretGenerated = "github_webhook_secret_generated"
	auditWebhookCreated         = "github_webhook_created"

	// webhookPermissionProbeTimeout bounds the installation lookup the GET
	// makes to decide whether to offer "Create webhook for me".
	webhookPermissionProbeTimeout = 8 * time.Second
	webhookCreateTimeout          = 20 * time.Second
)

// webhookSetupResponse is GET /api/config/github/webhooks.
type webhookSetupResponse struct {
	// PayloadURL is the public receiver URL to paste into GitHub, or "" when
	// this hive has no known public origin (PayloadURLSource says why).
	PayloadURL       string `json:"payload_url"`
	PayloadURLSource string `json:"payload_url_source"`
	Path             string `json:"path"`
	ContentType      string `json:"content_type"`
	// Events to tick; Optional names events that matter only with a feature.
	Events         []string          `json:"events"`
	OptionalEvents map[string]string `json:"optional_events"`

	// Verification is /api/status.webhooks.verification: github-signed,
	// relay (hub-signed) or none. Hosted reports whether a hub relay key is
	// held, i.e. deliveries arrive through the hub without owner action.
	Verification string `json:"verification"`
	Hosted       bool   `json:"hosted"`
	// SecretConfigured / SecretSource ("env", "file", "") describe the
	// GitHub-signed path; CanGenerateSecret is false while the env var owns
	// the secret.
	SecretConfigured  bool   `json:"secret_configured"`
	SecretSource      string `json:"secret_source"`
	SecretPath        string `json:"secret_path,omitempty"`
	CanGenerateSecret bool   `json:"can_generate_secret"`

	Health ghpkg.WebhookHealth `json:"health"`

	// Org / Repos are the targets a hook would be created for.
	Org   string   `json:"org"`
	Repos []string `json:"repos"`
	// App hook-management permissions, probed live; CreateAvailable is true
	// when at least one is granted and a secret is configured.
	AppOrgHooks     bool   `json:"app_org_hooks"`
	AppRepoHooks    bool   `json:"app_repo_hooks"`
	AppProbeError   string `json:"app_probe_error,omitempty"`
	CreateAvailable bool   `json:"create_available"`

	DocsURL string `json:"docs_url"`
}

const (
	webhookDocsURL = "https://github.com/hivecommons/hive/blob/v5/src/docs/webhooks.md#setting-up-webhooks-owner-guide"
	// Where PayloadURL came from, for the card's explanation.
	payloadURLSourceConfig  = "dashboard.public_url"
	payloadURLSourceHub     = "hub dashboard URL"
	payloadURLSourceRequest = "this request's host"
	payloadURLSourceNone    = "unknown — set dashboard.public_url"
)

// webhookPermissionProbe is the installation lookup; tests swap it.
var webhookPermissionProbe = installationPermissionProbe

func (s *Server) registerGitHubWebhookSetupRoutes() {
	s.mux.HandleFunc("GET /api/config/github/webhooks", s.handleConfigGitHubWebhooks)
	s.mux.HandleFunc("POST /api/config/github/webhooks/secret", s.handleConfigGitHubWebhooksGenerateSecret)
	s.mux.HandleFunc("POST /api/config/github/webhooks/create", s.handleConfigGitHubWebhooksCreate)
}

// webhookPayloadURL resolves this hive's public receiver URL: the configured
// dashboard.public_url, else the hub-assigned dashboard URL, else the origin
// the request arrived on (behind a proxy, X-Forwarded-Host/Proto).
func (s *Server) webhookPayloadURL(r *http.Request) (url, source string) {
	var cfg *config.Config
	if s.deps != nil {
		cfg = s.deps.Config
	}
	if cfg != nil {
		if u := strings.TrimRight(strings.TrimSpace(cfg.Dashboard.PublicURL), "/"); u != "" {
			return u + githubWebhookPath, payloadURLSourceConfig
		}
	}
	s.versionMu.RLock()
	pushed := s.hubPushedDashboardURL
	s.versionMu.RUnlock()
	if cfg != nil && pushed == "" {
		pushed = cfg.Hub.DashboardURL
	}
	if u := strings.TrimRight(strings.TrimSpace(pushed), "/"); u != "" {
		return u + githubWebhookPath, payloadURLSourceHub
	}
	if r != nil {
		host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
		if host == "" {
			host = r.Host
		}
		if host != "" && !isLoopbackHost(host) {
			scheme := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
			if scheme == "" {
				scheme = "https"
			}
			return scheme + "://" + host + githubWebhookPath, payloadURLSourceRequest
		}
	}
	return "", payloadURLSourceNone
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(host)
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		h = h[:i]
	}
	return h == "localhost" || h == "127.0.0.1" || h == "[::1]" || h == "::1"
}

func (s *Server) webhookSetupTargets() (org string, repos []string) {
	if s.deps == nil || s.deps.Config == nil {
		return "", nil
	}
	p := s.deps.Config.Project
	for _, r := range p.Repos {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if _, name, ok := strings.Cut(r, "/"); ok {
			r = name
		}
		repos = append(repos, r)
	}
	return strings.TrimSpace(p.Org), repos
}

func (s *Server) webhookProbeContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx := context.Background()
	if s.deps != nil && s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	return context.WithTimeout(ctx, timeout)
}

// handleConfigGitHubWebhooks serves GET /api/config/github/webhooks
// (owner-only).
func (s *Server) handleConfigGitHubWebhooks(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	payloadURL, source := s.webhookPayloadURL(r)
	org, repos := s.webhookSetupTargets()
	secretSource := ghpkg.WebhookSecretSource()
	resp := webhookSetupResponse{
		PayloadURL:        payloadURL,
		PayloadURLSource:  source,
		Path:              githubWebhookPath,
		ContentType:       "application/json",
		Events:            append([]string(nil), ghpkg.WebhookEvents...),
		OptionalEvents:    ghpkg.WebhookEventsOptional,
		Verification:      ghpkg.WebhookVerification(),
		Hosted:            ghpkg.WebhookRelayKey() != "",
		SecretConfigured:  secretSource != ghpkg.WebhookSecretSourceNone,
		SecretSource:      secretSource,
		CanGenerateSecret: secretSource != ghpkg.WebhookSecretSourceEnv,
		Health:            ghpkg.WebhookHealthSnapshot(),
		Org:               org,
		Repos:             repos,
		DocsURL:           webhookDocsURL,
	}
	if secretSource == ghpkg.WebhookSecretSourceFile {
		resp.SecretPath = ghpkg.WebhookSecretPath()
	}
	resp.Health.Verification = resp.Verification
	var auth *ghpkg.AppAuth
	if s.deps != nil {
		auth = s.deps.GHAppAuth
	}
	if auth != nil && org != "" {
		ctx, cancel := s.webhookProbeContext(webhookPermissionProbeTimeout)
		info, err := webhookPermissionProbe(ctx, auth)
		cancel()
		if err != nil {
			resp.AppProbeError = err.Error()
		} else {
			resp.AppOrgHooks, resp.AppRepoHooks = ghpkg.WebhookHookPermissions(info)
		}
	}
	resp.CreateAvailable = resp.SecretConfigured && resp.PayloadURL != "" && (resp.AppOrgHooks || (resp.AppRepoHooks && len(repos) > 0))
	jsonResponse(w, resp)
}

// handleConfigGitHubWebhooksGenerateSecret serves
// POST /api/config/github/webhooks/secret (owner-only). It mints a secret,
// stores it on the data volume and returns it ONCE; the receiver verifies
// GitHub-signed deliveries with it from the next request on.
func (s *Server) handleConfigGitHubWebhooksGenerateSecret(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	secret, err := ghpkg.GenerateWebhookSecret()
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ghpkg.ErrWebhookSecretFromEnv) {
			status = http.StatusConflict
		}
		jsonError(w, err.Error(), status)
		return
	}
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	s.audit.LogRecord(user, auditWebhookSecretGenerated, auditDetail("path", ghpkg.WebhookSecretPath()), "", "", 0)
	s.webhookLogger().Info("github webhook secret generated from the dashboard", "user", user, "path", ghpkg.WebhookSecretPath())
	jsonResponse(w, map[string]any{
		"secret":       secret,
		"secret_path":  ghpkg.WebhookSecretPath(),
		"verification": ghpkg.WebhookVerification(),
		"note":         "Shown once. Paste it into the GitHub webhook's Secret field; the hive keeps only the file.",
	})
}

type webhookCreateBody struct {
	// Scope is "organization" or "repository"; Repo names the repository
	// (bare name) for the repository scope.
	Scope string `json:"scope"`
	Repo  string `json:"repo"`
}

// handleConfigGitHubWebhooksCreate serves POST /api/config/github/webhooks/create
// (owner-only): creates the org or repo hook through the App installation.
// 409 without a webhook secret, 412 when the installation lacks the hooks
// write permission for the scope; when a hook for this hive's payload URL
// already exists it is returned with created=false instead of duplicated.
func (s *Server) handleConfigGitHubWebhooksCreate(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body webhookCreateBody
	if r.Body != nil {
		// An empty body means "organization scope".
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			jsonError(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}
	secret := ghpkg.WebhookSecret()
	if secret == "" {
		jsonError(w, "no webhook secret configured — generate one first", http.StatusConflict)
		return
	}
	payloadURL, _ := s.webhookPayloadURL(r)
	if payloadURL == "" {
		jsonError(w, "this hive has no public dashboard URL; set dashboard.public_url first", http.StatusConflict)
		return
	}
	org, repos := s.webhookSetupTargets()
	if org == "" {
		jsonError(w, "project.org is not configured", http.StatusConflict)
		return
	}
	var auth *ghpkg.AppAuth
	if s.deps != nil {
		auth = s.deps.GHAppAuth
	}
	if auth == nil {
		jsonError(w, "GitHub App auth not configured", http.StatusConflict)
		return
	}
	ctx, cancel := s.webhookProbeContext(webhookCreateTimeout)
	defer cancel()
	info, err := webhookPermissionProbe(ctx, auth)
	if err != nil {
		jsonError(w, "could not read the App installation's permissions: "+err.Error(), http.StatusBadGateway)
		return
	}
	orgHooks, repoHooks := ghpkg.WebhookHookPermissions(info)
	req := ghpkg.WebhookCreateRequest{Org: org, PayloadURL: payloadURL, Secret: secret, Events: ghpkg.WebhookEvents}
	switch strings.ToLower(strings.TrimSpace(body.Scope)) {
	case "", "organization", "org":
		if !orgHooks {
			jsonError(w, "the App installation lacks organization_hooks: write permission — grant it on the App and accept the installation update", http.StatusPreconditionFailed)
			return
		}
	case "repository", "repo":
		if !repoHooks {
			jsonError(w, "the App installation lacks repository_hooks: write permission — grant it on the App and accept the installation update", http.StatusPreconditionFailed)
			return
		}
		repo := strings.TrimSpace(body.Repo)
		if _, name, ok := strings.Cut(repo, "/"); ok {
			repo = name
		}
		if repo == "" || !containsFold(repos, repo) {
			jsonError(w, "repo must be one of this hive's project.repos", http.StatusBadRequest)
			return
		}
		req.Repo = repo
	default:
		jsonError(w, "scope must be organization or repository", http.StatusBadRequest)
		return
	}
	res, err := auth.CreateWebhook(ctx, req)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, ghpkg.ErrWebhookExists) {
			status = http.StatusConflict
		}
		jsonError(w, err.Error(), status)
		return
	}
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if !res.Created {
		// Idempotent: a hook for this payload URL already exists on the
		// target; nothing was mutated, so there is nothing to audit.
		s.webhookLogger().Info("github webhook already present; returning it", "user", user, "scope", res.Scope, "target", res.Target, "hook_id", res.ID)
		jsonResponse(w, res)
		return
	}
	s.audit.LogRecord(user, auditWebhookCreated, auditDetail("scope", res.Scope, "target", res.Target, "payload_url", payloadURL, "hook_id", strconv.FormatInt(res.ID, 10)), "", res.Target, 0)
	s.webhookLogger().Info("github webhook created through the App", "user", user, "scope", res.Scope, "target", res.Target, "hook_id", res.ID)
	jsonResponse(w, res)
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}
