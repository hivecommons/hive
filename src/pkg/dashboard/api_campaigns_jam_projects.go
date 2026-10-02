package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	inboundProjectItemTitle   = "Inbound project status"
	jamProjectSyncEndpointEnv = "HIVE_JAM_PROJECT_SYNC_URL"
	jamProjectSyncTokenEnv    = "HIVE_JAM_PROJECT_SYNC_TOKEN"
	jamProjectSyncDefaultURL  = "https://api.github.com/graphql"
	jamProjectSyncGitHubHost  = "api.github.com"
	jamProjectSyncTimeout     = 10 * time.Second
)

// jamProjectSyncAuth resolves the sync endpoint and the bearer token it may
// receive. GITHUB_TOKEN is only ever sent to the canonical
// https://api.github.com/graphql endpoint; any other endpoint — including a
// look-alike that merely shares the api.github.com host on a different port,
// path or with userinfo set — gets HIVE_JAM_PROJECT_SYNC_TOKEN instead, and
// plain http is allowed only for loopback hosts so no credential crosses the
// network in cleartext (#8811).
func jamProjectSyncAuth(endpoint string) (string, string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return "", "", fmt.Errorf("%s is not a valid URL", jamProjectSyncEndpointEnv)
	}
	host := u.Hostname()
	switch u.Scheme {
	case "https":
	case "http":
		if !jamProjectSyncLoopback(host) {
			return "", "", fmt.Errorf("%s must use https (http is allowed only for loopback hosts)", jamProjectSyncEndpointEnv)
		}
	default:
		return "", "", fmt.Errorf("%s must use https", jamProjectSyncEndpointEnv)
	}
	if jamProjectSyncIsCanonicalGitHub(u) {
		token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
		if token == "" {
			return "", "", errors.New("GITHUB_TOKEN required for GitHub Projects sync")
		}
		return u.String(), token, nil
	}
	return u.String(), strings.TrimSpace(os.Getenv(jamProjectSyncTokenEnv)), nil
}

// jamProjectSyncIsCanonicalGitHub reports whether u is exactly the
// documented default endpoint (https://api.github.com/graphql, no userinfo,
// no non-default port): the only shape GITHUB_TOKEN may be sent to. A URL
// that merely resolves to the same host — a different port, a different
// path, or one carrying userinfo — is treated as a distinct, non-GitHub
// endpoint (hivecommons/hive#10113).
func jamProjectSyncIsCanonicalGitHub(u *url.URL) bool {
	return u.Scheme == "https" && u.User == nil && u.Port() == "" &&
		strings.EqualFold(u.Hostname(), jamProjectSyncGitHubHost) && u.Path == "/graphql"
}

func jamProjectSyncLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type campaignProjectSyncRequest struct {
	Action     string `json:"action"`
	ProjectURL string `json:"project_url"`
	ProjectID  string `json:"project_id"`
	ItemType   string `json:"item_type"`
	ExternalID string `json:"external_id"`
	Status     string `json:"status"`
}

type campaignProjectSyncPayload struct {
	CampaignID string                `json:"campaign_id"`
	ProjectURL string                `json:"project_url,omitempty"`
	ProjectID  string                `json:"project_id,omitempty"`
	Spec       string                `json:"spec,omitempty"`
	Decisions  []CampaignDecision    `json:"decisions,omitempty"`
	Items      []CampaignProjectItem `json:"items,omitempty"`
}

func (s *Server) handleCampaignJamProjectSyncGet(w http.ResponseWriter, r *http.Request) {
	state, err := s.loadCampaignJam(campaignIDFromRequest(r))
	if err != nil {
		jsonError(w, err.Error(), campaignJamStatus(err))
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "project_sync": state.ProjectSync})
}

func (s *Server) handleCampaignJamProjectSyncPost(w http.ResponseWriter, r *http.Request) {
	if !requireJamMaintainerRole(w, r) {
		return
	}
	id := campaignIDFromRequest(r)
	var req campaignProjectSyncRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	action := firstRunNonEmpty(strings.TrimSpace(req.Action), "sync")
	switch action {
	case "enable", "disable", "inbound_status":
		state, err := s.mutateCampaignJam(id, func(state *CampaignJamState) error {
			return updateCampaignProjectSync(state, req, action)
		})
		if err != nil {
			jsonError(w, err.Error(), campaignJamStatus(err))
			return
		}
		jsonResponse(w, map[string]any{"ok": true, "jam": state, "project_sync": state.ProjectSync})
	case "sync":
		s.syncCampaignJamProject(w, r, id)
	default:
		jsonError(w, "unsupported project sync action", http.StatusBadRequest)
	}
}

func updateCampaignProjectSync(state *CampaignJamState, req campaignProjectSyncRequest, action string) error {
	if state.ProjectSync == nil {
		state.ProjectSync = &CampaignProjectSync{}
	}
	switch action {
	case "enable":
		projectURL := strings.TrimSpace(req.ProjectURL)
		projectID := strings.TrimSpace(req.ProjectID)
		if projectURL == "" && projectID == "" {
			return errors.New("project_url or project_id required")
		}
		state.ProjectSync.Enabled = true
		state.ProjectSync.ProjectURL = projectURL
		state.ProjectSync.ProjectID = projectID
		state.ProjectSync.LastStatus = "enabled"
		state.ProjectSync.LastError = ""
		state.ProjectSync.RetryAdvice = ""
	case "disable":
		state.ProjectSync.Enabled = false
		state.ProjectSync.LastStatus = "disabled"
	case "inbound_status":
		status := strings.TrimSpace(req.Status)
		if status == "" {
			return errors.New("status required")
		}
		itemType := firstRunNonEmpty(strings.TrimSpace(req.ItemType), "status")
		externalID := strings.TrimSpace(req.ExternalID)
		now := jamNow()
		state.ProjectSync.LastStatus = status
		state.ProjectSync.LastError = ""
		state.ProjectSync.RetryAdvice = ""
		state.ProjectSync.PublishedItems = append(state.ProjectSync.PublishedItems, CampaignProjectItem{
			Type: itemType, Title: inboundProjectItemTitle, Status: status, ExternalID: externalID, UpdatedAt: now,
		})
	}
	return nil
}

func (s *Server) syncCampaignJamProject(w http.ResponseWriter, r *http.Request, id string) {
	state, err := s.loadCampaignJam(id)
	if err != nil {
		jsonError(w, err.Error(), campaignJamStatus(err))
		return
	}
	if state.ProjectSync == nil || !state.ProjectSync.Enabled {
		jsonError(w, "project sync is disabled", http.StatusBadRequest)
		return
	}
	payload := buildCampaignProjectSyncPayload(state)
	if err := postCampaignProjectSync(payload); err != nil {
		_, _ = s.mutateCampaignJam(id, func(state *CampaignJamState) error {
			if state.ProjectSync == nil {
				state.ProjectSync = &CampaignProjectSync{}
			}
			state.ProjectSync.LastStatus = "failed"
			state.ProjectSync.LastError = err.Error()
			state.ProjectSync.RetryAdvice = "Verify the GitHub token, project link, and retry project sync from the Jam tab."
			return nil
		})
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	state, err = s.mutateCampaignJam(id, func(state *CampaignJamState) error {
		state.ProjectSync.LastStatus = "synced"
		state.ProjectSync.LastSyncAt = jamNow()
		state.ProjectSync.LastError = ""
		state.ProjectSync.RetryAdvice = ""
		kept := make([]CampaignProjectItem, 0, len(payload.Items))
		for _, item := range state.ProjectSync.PublishedItems {
			if item.Title == inboundProjectItemTitle {
				kept = append(kept, item)
			}
		}
		state.ProjectSync.PublishedItems = append(kept, payload.Items...)
		return nil
	})
	if err != nil {
		jsonError(w, err.Error(), campaignJamStatus(err))
		return
	}
	s.auditFromRequest(r, "campaign_jam_project_sync", auditDetail("campaign", id), "")
	jsonResponse(w, map[string]any{"ok": true, "jam": state, "project_sync": state.ProjectSync})
}

func buildCampaignProjectSyncPayload(state *CampaignJamState) campaignProjectSyncPayload {
	payload := campaignProjectSyncPayload{
		CampaignID: state.CampaignID,
		ProjectURL: state.ProjectSync.ProjectURL,
		ProjectID:  state.ProjectSync.ProjectID,
		Spec:       state.SpecContent,
		Decisions:  campaignProjectDecisions(state),
		Items:      campaignProjectItems(state),
	}
	return payload
}

func campaignProjectDecisions(state *CampaignJamState) []CampaignDecision {
	var decisions []CampaignDecision
	for _, poll := range state.Polls {
		if poll.Decision != nil {
			decisions = append(decisions, *poll.Decision)
		}
	}
	for _, rev := range state.Revisions {
		decisions = append(decisions, rev.Decisions...)
	}
	return decisions
}

func campaignProjectItems(state *CampaignJamState) []CampaignProjectItem {
	now := jamNow()
	items := []CampaignProjectItem{{
		Type: "spec", Title: "Campaign spec " + state.CampaignID, Body: state.SpecContent, Status: state.ProjectSync.LastStatus, ExternalID: state.SpecRevisionID, UpdatedAt: now,
	}}
	for _, poll := range state.Polls {
		if poll.Decision == nil {
			continue
		}
		items = append(items, CampaignProjectItem{
			Type: "decision", Title: poll.Question, Body: poll.Decision.Rationale, Status: poll.Decision.Outcome, ExternalID: poll.ID, UpdatedAt: now,
		})
	}
	for _, suggestion := range state.Suggestions {
		items = append(items, CampaignProjectItem{
			Type: "derived_issue", Title: suggestion.Section, Body: suggestion.ProposedText, Status: suggestion.Status, ExternalID: suggestion.ID, UpdatedAt: now,
		})
	}
	return items
}

func postCampaignProjectSync(payload campaignProjectSyncPayload) error {
	endpoint := strings.TrimSpace(os.Getenv(jamProjectSyncEndpointEnv))
	if endpoint == "" {
		endpoint = jamProjectSyncDefaultURL
	}
	endpoint, token, err := jamProjectSyncAuth(endpoint)
	if err != nil {
		return err
	}
	if u, perr := url.Parse(endpoint); perr == nil && jamProjectSyncIsCanonicalGitHub(u) {
		return postGitHubProjectDrafts(endpoint, token, payload)
	}
	return jamProjectSyncGraphQL(endpoint, token,
		"mutation HiveJamProjectSync($input: JSON!) { hiveJamProjectSync(input: $input) { ok } }",
		map[string]any{"input": payload})
}

// postGitHubProjectDrafts publishes each item as a draft issue through
// GitHub's real Projects v2 API (addProjectV2DraftIssue). It needs the
// project's GraphQL node id (PVT_…); a project URL alone cannot be resolved
// to one without extra scopes, so it is rejected with an actionable message.
func postGitHubProjectDrafts(endpoint, token string, payload campaignProjectSyncPayload) error {
	projectID := strings.TrimSpace(payload.ProjectID)
	if projectID == "" {
		return errors.New("project_id (GitHub Projects v2 node id, PVT_…) required to sync to GitHub Projects")
	}
	const mutation = "mutation($input: AddProjectV2DraftIssueInput!) { addProjectV2DraftIssue(input: $input) { projectItem { id } } }"
	for _, item := range payload.Items {
		title := strings.TrimSpace(item.Title)
		if title == "" {
			title = item.Type
		}
		input := map[string]any{"projectId": projectID, "title": title}
		if item.Body != "" {
			input["body"] = item.Body
		}
		if err := jamProjectSyncGraphQL(endpoint, token, mutation, map[string]any{"input": input}); err != nil {
			return err
		}
	}
	return nil
}

func jamProjectSyncGraphQL(endpoint, token, query string, variables map[string]any) error {
	raw, err := json.Marshal(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: jamProjectSyncTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var decoded struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("github projects sync failed with status %d", resp.StatusCode)
	}
	if len(decoded.Errors) > 0 {
		return errors.New(decoded.Errors[0].Message)
	}
	return nil
}
