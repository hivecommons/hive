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

// WebhookCreateResult names the hook CreateWebhook created or found.
type WebhookCreateResult struct {
	// ID is the GitHub hook id; HookID repeats it for the Settings card.
	ID     int64    `json:"id"`
	HookID int64    `json:"hook_id"`
	Scope  string   `json:"scope"` // "organization" or "repository"
	Target string   `json:"target"`
	URL    string   `json:"url"` // GitHub settings page for the hook
	Events []string `json:"events"`
	// PayloadURL is the hook's config.url (this hive's receiver).
	PayloadURL string `json:"payload_url"`
	// Created is false when a hook with the same payload URL already existed
	// on the target and was returned instead of creating a duplicate.
	Created bool `json:"created"`
}

// webhookListPages bounds the hook listing; GitHub caps hooks per target at
// 20 for repositories and organizations, so one page is normally enough.
const webhookListPages = 5

// CreateWebhook creates an organization hook (req.Repo == "") or a repository
// hook through the App installation token. It is idempotent: the target's
// hooks are listed first and a hook whose config.url equals req.PayloadURL is
// returned (Created=false) instead of creating a duplicate. The token carries
// every granted permission, so the caller checks WebhookHookPermissions first
// and only offers the action when the matching write permission is held.
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

	existing, err := findWebhookByURL(ctx, client, req.Org, req.Repo, req.PayloadURL)
	if err != nil {
		return nil, fmt.Errorf("listing existing hooks: %w", err)
	}
	if existing != nil {
		return a.webhookResult(req, existing, false), nil
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
	if req.Repo == "" {
		created, resp, err = client.Organizations.CreateHook(ctx, req.Org, hook)
	} else {
		created, resp, err = client.Repositories.CreateHook(ctx, req.Org, req.Repo, hook)
	}
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(err.Error()), "already exists") {
			// Lost a race with a concurrent create: return the winner.
			if found, ferr := findWebhookByURL(ctx, client, req.Org, req.Repo, req.PayloadURL); ferr == nil && found != nil {
				return a.webhookResult(req, found, false), nil
			}
			return nil, ErrWebhookExists
		}
		return nil, err
	}
	if len(created.Events) == 0 {
		created.Events = events
	}
	return a.webhookResult(req, created, true), nil
}

// findWebhookByURL returns the hook on the org (repo == "") or repository
// whose config.url equals payloadURL, or nil when there is none.
func findWebhookByURL(ctx context.Context, client *gh.Client, org, repo, payloadURL string) (*gh.Hook, error) {
	want := strings.TrimRight(strings.TrimSpace(payloadURL), "/")
	opts := &gh.ListOptions{PerPage: 100}
	for page := 0; page < webhookListPages; page++ {
		var (
			hooks []*gh.Hook
			resp  *gh.Response
			err   error
		)
		if repo == "" {
			hooks, resp, err = client.Organizations.ListHooks(ctx, org, opts)
		} else {
			hooks, resp, err = client.Repositories.ListHooks(ctx, org, repo, opts)
		}
		if err != nil {
			return nil, err
		}
		for _, h := range hooks {
			if h.GetConfig() == nil {
				continue
			}
			if strings.TrimRight(strings.TrimSpace(h.GetConfig().GetURL()), "/") == want {
				return h, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return nil, nil
		}
		opts.Page = resp.NextPage
	}
	return nil, nil
}

func (a *AppAuth) webhookResult(req WebhookCreateRequest, hook *gh.Hook, created bool) *WebhookCreateResult {
	res := &WebhookCreateResult{
		ID:         hook.GetID(),
		HookID:     hook.GetID(),
		Events:     append([]string(nil), hook.Events...),
		PayloadURL: req.PayloadURL,
		Created:    created,
	}
	if hook.GetConfig() != nil && hook.GetConfig().GetURL() != "" {
		res.PayloadURL = hook.GetConfig().GetURL()
	}
	base := strings.TrimSuffix(webBaseURLForAPI(a.apiURL), "/")
	if req.Repo == "" {
		res.Scope, res.Target = "organization", req.Org
		res.URL = fmt.Sprintf("%s/organizations/%s/settings/hooks/%d", base, req.Org, res.ID)
	} else {
		res.Scope, res.Target = "repository", req.Org+"/"+req.Repo
		res.URL = fmt.Sprintf("%s/%s/%s/settings/hooks/%d", base, req.Org, req.Repo, res.ID)
	}
	return res
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
