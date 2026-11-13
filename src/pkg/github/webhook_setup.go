package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Owner-facing webhook setup (hivecommons/hive#18017): the events a hive
// consumes on /api/webhook/github and the App-side hook creation the
// dashboard offers when the installation may manage hooks.

// WebhookEvents is the list of GitHub webhook events the spoke receiver acts
// on, in the order the GitHub webhook form lists them. It is what the
// Settings card tells an owner to tick, and what CreateWebhook subscribes.
// projects_v2_item is organization-scoped and only useful with the
// github_projects work source, so it is listed separately
// (WebhookEventsOptional).
var WebhookEvents = []string{
	"check_run",
	"check_suite",
	"issue_comment",
	"issues",
	"pull_request",
	"pull_request_review",
	"pull_request_review_comment",
	"push",
	"status",
	"workflow_run",
}

// WebhookEventsOptional are events the receiver understands but that only
// matter with a specific feature enabled.
var WebhookEventsOptional = map[string]string{
	"workflow_job":     "CI job counts from the job payload (needs Actions: read on the App)",
	"projects_v2_item": "GitHub Projects work source (governor.work_source.type: github_projects); organization webhook only",
}

// Installation permissions that allow the App to create hooks.
const (
	PermissionOrganizationHooks = "organization_hooks"
	PermissionRepositoryHooks   = "repository_hooks"
)

// WebhookHookPermissions reports which hook-management permissions the
// installation holds at write level: org-level hooks and repo-level hooks.
func WebhookHookPermissions(info *InstallationInfo) (orgHooks, repoHooks bool) {
	if info == nil {
		return false, false
	}
	return info.Granted(PermissionOrganizationHooks, "write"), info.Granted(PermissionRepositoryHooks, "write")
}

// ErrWebhookExists is returned when GitHub already has a hook for the same
// payload URL at that scope (GitHub answers 422 "Hook already exists").
var ErrWebhookExists = errors.New("a webhook for this payload URL already exists")

// WebhookCreateRequest describes the hook CreateWebhook should create.
type WebhookCreateRequest struct {
	// Org is the owner login; Repo is empty for an organization hook.
	Org, Repo string
	// PayloadURL is the spoke's public receiver URL; Secret the HMAC secret.
	PayloadURL, Secret string
	Events             []string
}

// WebhookCreateResult names the hook GitHub created.
type WebhookCreateResult struct {
	HookID int64  `json:"hook_id"`
	Scope  string `json:"scope"` // "organization" or "repository"
	Target string `json:"target"`
	URL    string `json:"url"` // GitHub settings page for the hook
}

// CreateWebhook creates an organization hook (req.Repo == "") or a repository
// hook through the App installation token. The token carries every granted
// permission, so the caller checks WebhookHookPermissions first and only
// offers the action when the matching write permission is held.
func (a *AppAuth) CreateWebhook(ctx context.Context, req WebhookCreateRequest) (*WebhookCreateResult, error) {
	if a == nil {
		return nil, errors.New("GitHub App auth not configured")
	}
	if strings.TrimSpace(req.Org) == "" || strings.TrimSpace(req.PayloadURL) == "" || strings.TrimSpace(req.Secret) == "" {
		return nil, errors.New("org, payload URL and secret are required")
	}
	token, err := a.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("minting installation token: %w", err)
	}
	client := newTokenClient(token, a.apiURL)
	events := req.Events
	if len(events) == 0 {
		events = WebhookEvents
	}
	hook := &gh.Hook{
		Active: gh.Ptr(true),
		Events: events,
		Config: &gh.HookConfig{
			URL:         gh.Ptr(req.PayloadURL),
			ContentType: gh.Ptr("json"),
			Secret:      gh.Ptr(req.Secret),
			InsecureSSL: gh.Ptr("0"),
		},
	}
	var (
		created *gh.Hook
		resp    *gh.Response
	)
	res := &WebhookCreateResult{}
	if req.Repo == "" {
		created, resp, err = client.Organizations.CreateHook(ctx, req.Org, hook)
		res.Scope, res.Target = "organization", req.Org
	} else {
		created, resp, err = client.Repositories.CreateHook(ctx, req.Org, req.Repo, hook)
		res.Scope, res.Target = "repository", req.Org+"/"+req.Repo
	}
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(err.Error()), "already exists") {
			return nil, ErrWebhookExists
		}
		return nil, err
	}
	res.HookID = created.GetID()
	base := strings.TrimSuffix(webBaseURLForAPI(a.apiURL), "/")
	if req.Repo == "" {
		res.URL = fmt.Sprintf("%s/organizations/%s/settings/hooks/%d", base, req.Org, res.HookID)
	} else {
		res.URL = fmt.Sprintf("%s/%s/%s/settings/hooks/%d", base, req.Org, req.Repo, res.HookID)
	}
	return res, nil
}

// webBaseURLForAPI maps an API URL to the web origin a hook's settings page
// lives on: api.github.com → github.com, https://ghe.example/api/v3 →
// https://ghe.example.
func webBaseURLForAPI(apiURL string) string {
	u := strings.TrimSpace(apiURL)
	if u == "" || strings.Contains(u, "api.github.com") {
		return "https://github.com"
	}
	return strings.TrimSuffix(strings.TrimSuffix(u, "/"), "/api/v3")
}
