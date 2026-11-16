package dashboard

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Owner-approved prompt and definition source repos (hivecommons/hive#18490).
//
// An agent's prompt_source and definition_source read text from a GitHub repo
// and run it as the agent's instructions, so whoever can merge to that repo can
// steer the agent. The seed allowlist (allow_github_prompt plus
// github_prompt_allowlist) is still edited on the config volume only. These
// endpoints add the owner approval step for the hive's OWN managed repos:
//
//	GET /api/config/prompt-sources
//	PUT /api/config/prompt-sources/owner-repos  {"repo": "org/name", "allowed": true, "acknowledge_trust": true}
//
// Both are owner-only (403 otherwise). A repo outside project.repos is refused;
// it still needs the seed edit. Approving requires acknowledge_trust: true,
// the API form of the dashboard's confirm dialog. Each change is audit-logged
// and persisted through Save into the running config on the PVC, as
// variables.security.owner_github_prompt_repos[].repo with
// variables.security.owner_github_prompt_repos[].approved_by and
// variables.security.owner_github_prompt_repos[].approved_at recording the
// approval.

// promptSourceTrustNotice is the trust implication the confirm names, returned
// by GET so the dashboard and API clients show the same words.
const promptSourceTrustNotice = "Anyone who can merge to this repo can change what this hive's agents are told to do and how they are defined. Approve only a repo whose merge rights you trust as much as owner access to this hive."

// promptSourceAuditAction is the audit action for owner approvals and revokes.
const promptSourceAuditAction = "config_prompt_source_repo"

type promptSourceRepoRow struct {
	Repo       string `json:"repo"`
	Managed    bool   `json:"managed"`
	Owner      bool   `json:"owner_approved"`
	Seed       bool   `json:"seed_allowed"`
	Allowed    bool   `json:"allowed"`
	ApprovedBy string `json:"approved_by,omitempty"`
	ApprovedAt string `json:"approved_at,omitempty"`
}

type promptSourcesResponse struct {
	Repos         []promptSourceRepoRow `json:"repos"`
	SeedEnabled   bool                  `json:"seed_enabled"`
	SeedAllowlist []string              `json:"seed_allowlist"`
	TrustNotice   string                `json:"trust_notice"`
	SeedOnlyNote  string                `json:"seed_only_note"`
}

// promptSourcesSeedOnlyNote explains how a repo outside the managed set is
// allowed: the seed edit, unchanged by #18490.
const promptSourcesSeedOnlyNote = "Repos outside this hive's managed repos can only be allowed by editing the config volume (allow_github_prompt and github_prompt_allowlist)."

func (s *Server) handlePromptSourcesGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, promptSourcesSection(s.deps.Config))
}

func (s *Server) handlePromptSourcesOwnerRepoPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s == nil || s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Repo             string `json:"repo"`
		Allowed          *bool  `json:"allowed"`
		AcknowledgeTrust bool   `json:"acknowledge_trust"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Repo) == "" || body.Allowed == nil {
		jsonError(w, `repo and allowed are required, e.g. {"repo": "org/name", "allowed": true, "acknowledge_trust": true}`, http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	user := requestUser(r)

	if !*body.Allowed {
		if s.deps.SkipReloadFunc != nil {
			s.deps.SkipReloadFunc()
		}
		changed, err := cfg.RevokeOwnerPromptRepoAndSave(body.Repo)
		if err != nil && s.logger != nil {
			s.logger.Error("failed to persist config after prompt source revoke", "repo", body.Repo, "error", err)
		}
		if changed {
			s.auditFromRequest(r, promptSourceAuditAction, auditDetail("repo", strings.TrimSpace(body.Repo), "allowed", "false"), "")
		}
		jsonResponse(w, promptSourcesSection(cfg))
		return
	}

	// --- validate before mutating anything ---
	repo, err := cfg.NormalizeOwnerPromptRepo(body.Repo)
	if err != nil {
		msg := err.Error()
		if errors.Is(err, config.ErrNotManagedRepo) {
			msg += ". " + promptSourcesSeedOnlyNote
		}
		jsonError(w, msg, http.StatusBadRequest)
		return
	}
	if !body.AcknowledgeTrust {
		jsonError(w, "acknowledge_trust must be true to approve "+repo+": "+promptSourceTrustNotice, http.StatusBadRequest)
		return
	}

	// --- apply ---
	if s.deps.SkipReloadFunc != nil {
		s.deps.SkipReloadFunc()
	}
	_, changed, err := cfg.AllowOwnerPromptRepoAndSave(repo, user, time.Now())
	if err != nil && s.logger != nil {
		s.logger.Error("failed to persist config after prompt source approval", "repo", repo, "error", err)
	}
	if changed {
		s.auditFromRequest(r, promptSourceAuditAction, auditDetail("repo", repo, "allowed", "true"), "")
		if s.logger != nil {
			s.logger.Info("owner approved repo as prompt/definition source", "repo", repo, "user", user)
		}
	}
	jsonResponse(w, promptSourcesSection(cfg))
}

// promptSourcesSection lists every managed repo with its gate state, then any
// owner approval whose repo is no longer managed (inert, but revocable).
func promptSourcesSection(cfg *config.Config) promptSourcesResponse {
	owner := cfg.OwnerPromptRepos()
	byRepo := make(map[string]config.OwnerPromptRepo, len(owner))
	for _, e := range owner {
		byRepo[strings.ToLower(e.Repo)] = e
	}
	rows := []promptSourceRepoRow{}
	seen := map[string]bool{}
	for _, slug := range cfg.ManagedRepoSlugs() {
		key := strings.ToLower(slug)
		if seen[key] {
			continue
		}
		seen[key] = true
		e, ok := byRepo[key]
		rows = append(rows, promptSourceRepoRow{
			Repo:       slug,
			Managed:    true,
			Owner:      ok,
			Seed:       cfg.SeedGitHubPromptAllowed(slug),
			Allowed:    cfg.GitHubPromptAllowed(slug),
			ApprovedBy: e.ApprovedBy,
			ApprovedAt: e.ApprovedAt,
		})
	}
	for _, e := range owner {
		if seen[strings.ToLower(e.Repo)] {
			continue
		}
		rows = append(rows, promptSourceRepoRow{
			Repo:       e.Repo,
			Owner:      true,
			Seed:       cfg.SeedGitHubPromptAllowed(e.Repo),
			Allowed:    cfg.GitHubPromptAllowed(e.Repo),
			ApprovedBy: e.ApprovedBy,
			ApprovedAt: e.ApprovedAt,
		})
	}
	seedList := append([]string{}, cfg.Variables.Security.GitHubPromptAllowlist...)
	return promptSourcesResponse{
		Repos:         rows,
		SeedEnabled:   cfg.Variables.Security.AllowGitHubPrompt,
		SeedAllowlist: seedList,
		TrustNotice:   promptSourceTrustNotice,
		SeedOnlyNote:  promptSourcesSeedOnlyNote,
	}
}
