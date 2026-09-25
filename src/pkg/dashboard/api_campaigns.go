package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

// Campaign is the dashboard projection for resumable inception/Spektacular work.
type Campaign struct {
	ID           string             `json:"id"`
	Title        string             `json:"title"`
	Source       string             `json:"source,omitempty"`
	Repos        []string           `json:"repos,omitempty"`
	CurrentStage string             `json:"current_stage"`
	CurrentStep  string             `json:"current_step,omitempty"`
	Artifacts    []CampaignArtifact `json:"artifacts,omitempty"`
	LinkedPRs    []string           `json:"linked_prs,omitempty"`
	LinkedIssues []string           `json:"linked_issues,omitempty"`
	Contributors []string           `json:"contributors,omitempty"`
	LastActivity string             `json:"last_activity,omitempty"`
	Status       string             `json:"status"`
	Engine       string             `json:"engine"`
	Type         string             `json:"type"`
	RunKey       string             `json:"run_key,omitempty"`
	RunURL       string             `json:"run_url,omitempty"`
	LeaseOwner   string             `json:"lease_owner,omitempty"`
	RevisionOf   string             `json:"revision_of,omitempty"`
	Revision     int                `json:"revision,omitempty"`
}

type CampaignArtifact struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

type campaignResumeRequest struct {
	Surface string `json:"surface"`
}

type campaignResumeResponse struct {
	OK            bool        `json:"ok"`
	Campaign      Campaign    `json:"campaign"`
	Run           *Run        `json:"run,omitempty"`
	State         interface{} `json:"state,omitempty"`
	ResumeCommand string      `json:"resume_command,omitempty"`
	Message       string      `json:"message,omitempty"`
}

type campaignReleaseResponse struct {
	OK       bool     `json:"ok"`
	Campaign Campaign `json:"campaign"`
	Message  string   `json:"message,omitempty"`
}

type campaignReviseResponse struct {
	OK       bool     `json:"ok"`
	Campaign Campaign `json:"campaign"`
	Message  string   `json:"message,omitempty"`
}

func (s *Server) handleCampaignsList(w http.ResponseWriter, r *http.Request) {
	campaigns, err := s.campaignsForRequest(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, map[string]interface{}{"ok": true, "campaigns": campaigns})
}

func (s *Server) handleCampaignGet(w http.ResponseWriter, r *http.Request) {
	id := campaignIDFromRequest(r)
	if id == "" {
		jsonError(w, "campaign id required", http.StatusBadRequest)
		return
	}
	campaigns, err := s.campaignsForRequest(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	for _, campaign := range campaigns {
		if campaign.ID == id || campaign.RunKey == id {
			jsonResponse(w, map[string]interface{}{"ok": true, "campaign": campaign})
			return
		}
	}
	jsonError(w, "campaign not found", http.StatusNotFound)
}

func (s *Server) handleCampaignResume(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	id := campaignIDFromRequest(r)
	if id == "" {
		jsonError(w, "campaign id required", http.StatusBadRequest)
		return
	}
	var req campaignResumeRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeBody(r, &req); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
	}
	campaigns, err := s.campaignsForRequest(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	for _, campaign := range campaigns {
		if campaign.ID != id && campaign.RunKey != id {
			continue
		}
		if campaign.Type == "inception" && s.deps != nil && s.deps.Inception != nil {
			if _, err := s.deps.Inception.ArchiveCurrentCampaign(); err != nil {
				jsonError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			leased, err := s.deps.Inception.LeaseCampaignArchive(campaign.ID, requestUser(r), req.Surface, time.Now())
			if err != nil {
				jsonError(w, err.Error(), campaignArchiveErrorStatus(err))
				return
			}
			state, err := s.deps.Inception.RestoreCampaignArchive(campaign.ID)
			if err != nil {
				jsonError(w, err.Error(), http.StatusNotFound)
				return
			}
			campaign = campaignFromInceptionArchive(*leased)
			s.auditFromRequest(r, "campaign_resume", auditDetail("campaign", campaign.ID, "type", campaign.Type, "surface", strings.TrimSpace(req.Surface)), "")
			jsonResponse(w, campaignResumeResponse{OK: true, Campaign: campaign, State: state, Message: "Inception campaign restored"})
			return
		}
		if campaign.LeaseOwner != "" && campaign.LeaseOwner != requestUser(r) {
			jsonError(w, "campaign lease held by "+campaign.LeaseOwner, http.StatusConflict)
			return
		}
		run, _ := s.runByCampaignKey(campaign.RunKey)
		s.auditFromRequest(r, "campaign_resume", auditDetail("campaign", campaign.ID, "type", campaign.Type, "surface", strings.TrimSpace(req.Surface)), "")
		jsonResponse(w, campaignResumeResponse{OK: true, Campaign: campaign, Run: run, ResumeCommand: spektacularResumeCommand(campaign), Message: "Spektacular state is loaded from its artifact files; continue with the command or run link."})
		return
	}
	jsonError(w, "campaign not found", http.StatusNotFound)
}

func (s *Server) handleCampaignRelease(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	id := campaignIDFromRequest(r)
	if id == "" {
		jsonError(w, "campaign id required", http.StatusBadRequest)
		return
	}
	if s.deps == nil || s.deps.Inception == nil {
		jsonError(w, "campaign store unavailable", http.StatusServiceUnavailable)
		return
	}
	campaigns, err := s.campaignsForRequest(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	for _, campaign := range campaigns {
		if campaign.ID != id && campaign.RunKey != id {
			continue
		}
		if campaign.RunKey != "" && campaign.Type == "spektacular" {
			if s.contributeHub == nil {
				jsonError(w, "run lease registry unavailable", http.StatusServiceUnavailable)
				return
			}
			held, ok := s.contributeHub.runLeaseHolder(campaign.RunKey, time.Now())
			if !ok {
				jsonError(w, "campaign lease not found", http.StatusNotFound)
				return
			}
			if held.identity != requestUser(r) {
				jsonError(w, "campaign lease held by "+held.identity, http.StatusConflict)
				return
			}
			s.contributeHub.revokeLease(held.identity, held.taskID)
			campaign.LeaseOwner = ""
			s.auditFromRequest(r, "campaign_release", auditDetail("campaign", campaign.ID, "type", campaign.Type), "")
			jsonResponse(w, campaignReleaseResponse{OK: true, Campaign: campaign, Message: "Campaign lease released"})
			return
		}
		archive, err := s.deps.Inception.ReleaseCampaignArchive(campaign.ID, requestUser(r), time.Now())
		if err != nil {
			jsonError(w, err.Error(), campaignArchiveErrorStatus(err))
			return
		}
		released := campaignFromInceptionArchive(*archive)
		s.auditFromRequest(r, "campaign_release", auditDetail("campaign", released.ID, "type", released.Type), "")
		jsonResponse(w, campaignReleaseResponse{OK: true, Campaign: released, Message: "Campaign lease released"})
		return
	}
	jsonError(w, "campaign not found", http.StatusNotFound)
}

func (s *Server) handleCampaignRevise(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	id := campaignIDFromRequest(r)
	if id == "" {
		jsonError(w, "campaign id required", http.StatusBadRequest)
		return
	}
	if s.deps == nil || s.deps.Inception == nil {
		jsonError(w, "campaign store unavailable", http.StatusServiceUnavailable)
		return
	}
	campaigns, err := s.campaignsForRequest(r)
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	for _, campaign := range campaigns {
		if campaign.ID != id && campaign.RunKey != id {
			continue
		}
		var archive *knowledge.InceptionCampaignArchive
		if campaign.Type == "inception" {
			archive, err = s.deps.Inception.ReviseCampaignArchive(campaign.ID, requestUser(r), time.Now())
		} else {
			archive, err = s.deps.Inception.ReviseExternalCampaign(campaign.ID, campaign.Title, campaign.Source, campaign.Engine, campaign.Type, requestUser(r), campaign.Repos, time.Now())
		}
		if err != nil {
			jsonError(w, err.Error(), campaignArchiveErrorStatus(err))
			return
		}
		revision := campaignFromInceptionArchive(*archive)
		s.auditFromRequest(r, "campaign_revise", auditDetail("campaign", campaign.ID, "revision", revision.ID, "type", campaign.Type), "")
		jsonResponse(w, campaignReviseResponse{OK: true, Campaign: revision, Message: "Campaign revision created"})
		return
	}
	jsonError(w, "campaign not found", http.StatusNotFound)
}

func campaignIDFromRequest(r *http.Request) string {
	id := strings.TrimSpace(r.PathValue("id"))
	if unescaped, err := url.PathUnescape(id); err == nil {
		id = strings.TrimSpace(unescaped)
	}
	return id
}

func (s *Server) campaignsForRequest(r *http.Request) ([]Campaign, error) {
	campaigns, err := s.allCampaigns(r)
	if err != nil {
		return nil, err
	}
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		q = strings.ToLower(strings.TrimSpace(r.URL.Query().Get("search")))
	}
	repo := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("repo")))
	stage := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("stage")))
	status := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("status")))
	owner := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("owner")))
	out := campaigns[:0]
	for _, campaign := range campaigns {
		if q != "" && !campaignMatchesSearch(campaign, q) {
			continue
		}
		if repo != "" && !campaignHasValue(campaign.Repos, repo) {
			continue
		}
		if stage != "" && !strings.EqualFold(campaign.CurrentStage, stage) {
			continue
		}
		if status != "" && !strings.EqualFold(campaign.Status, status) {
			continue
		}
		if owner != "" && !campaignHasValue(campaign.Contributors, owner) && !strings.EqualFold(campaign.LeaseOwner, owner) {
			continue
		}
		out = append(out, campaign)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActivity > out[j].LastActivity })
	return out, nil
}

func (s *Server) allCampaigns(r *http.Request) ([]Campaign, error) {
	byID := map[string]Campaign{}
	if s != nil && s.deps != nil && s.deps.Inception != nil {
		archives, err := s.deps.Inception.ListCampaignArchives()
		if err != nil {
			return nil, err
		}
		for _, archive := range archives {
			campaign := campaignFromInceptionArchive(archive)
			byID[campaign.ID] = campaign
		}
		if state := s.deps.Inception.GetState(); state != nil && strings.TrimSpace(state.IdeaSlug) != "" {
			campaign := campaignFromInceptionArchive(knowledge.InceptionCampaignArchive{
				ID:     state.IdeaSlug,
				Engine: "Spec Kit",
				Type:   "inception",
				State:  state,
			})
			byID[campaign.ID] = campaign
		}
	}
	if s != nil && s.contributeHub != nil {
		runs, err := s.activeRuns(true)
		if err != nil {
			return nil, err
		}
		for _, run := range runs {
			campaign := campaignFromRun(run)
			byID[campaign.ID] = campaign
		}
	}
	out := make([]Campaign, 0, len(byID))
	for _, campaign := range byID {
		out = append(out, campaign)
	}
	return out, nil
}

func campaignFromInceptionArchive(archive knowledge.InceptionCampaignArchive) Campaign {
	state := archive.State
	title := firstRunNonEmpty(archive.Title, archive.ID)
	stage := "inception"
	step := "archived"
	status := "parked"
	last := archive.ArchivedAt
	repos := append([]string{}, archive.Repos...)
	linkedIssues := []string{}
	leaseOwner := ""
	runKey := ""
	runURL := ""
	contributors := []string{"brainstorm"}
	if state != nil {
		if strings.TrimSpace(state.IdeaText) != "" {
			title = strings.TrimSpace(state.IdeaText)
		}
		step = string(state.Phase)
		status = inceptionCampaignStatus(state.Phase)
		if !state.StartedAt.IsZero() && last.IsZero() {
			last = state.StartedAt
		}
		if state.PhaseChangedAt != nil {
			last = *state.PhaseChangedAt
		}
		if strings.TrimSpace(state.RepoURL) != "" {
			repos = append(repos, strings.TrimSpace(state.RepoURL))
		}
	} else if firstRunNonEmpty(archive.Type, "inception") == "spektacular" {
		stage = StagePlan
		step = "revision"
		runKey = strings.TrimSpace(archive.Source)
		if runKey != "" {
			runURL = "/api/runs/" + url.PathEscape(runKey)
		}
		contributors = nil
	}
	if archive.Lease != nil && archive.Lease.Owner != "" && time.Now().Before(archive.Lease.ExpiresAt) {
		leaseOwner = archive.Lease.Owner
	}
	artifacts := []CampaignArtifact{{Kind: "state", Label: "Inception state"}}
	for _, file := range archive.WikiFiles {
		artifacts = append(artifacts, CampaignArtifact{Kind: "fact", Label: file})
	}
	return Campaign{
		ID: archive.ID, Title: title, Source: firstRunNonEmpty(archive.Source, "inception"), Repos: repos, CurrentStage: stage, CurrentStep: step,
		Artifacts: artifacts, LinkedIssues: linkedIssues, Contributors: contributors, LastActivity: formatRunTime(last),
		Status: status, Engine: firstRunNonEmpty(archive.Engine, "Spec Kit"), Type: firstRunNonEmpty(archive.Type, "inception"),
		RunKey: runKey, RunURL: runURL, LeaseOwner: leaseOwner, RevisionOf: archive.RevisionOf, Revision: archive.Revision,
	}
}

func campaignFromRun(run Run) Campaign {
	id := runKeyOfLease(run.Key, run.Repo)
	if leaseKey := strings.TrimSpace(run.LeaseKey); leaseKey != "" {
		id = runKeyOfLease(leaseKey, run.Repo)
	}
	prs := []string{}
	for _, wave := range run.ReviewWaves {
		for _, pr := range wave.PRs {
			if pr.URL != "" {
				prs = append(prs, pr.URL)
			}
		}
	}
	issues := []string{}
	if run.Repo != "" && run.Key != "" {
		issues = append(issues, run.Key)
	}
	artifacts := []CampaignArtifact{{Kind: "run", Label: "Run detail", URL: "/api/runs/" + url.PathEscape(run.Key)}}
	if run.LastReceipt != "" {
		artifacts = append(artifacts, CampaignArtifact{Kind: "receipt", Label: run.LastReceipt})
	}
	return Campaign{
		ID: id, Title: run.Title, Source: run.Key, Repos: nonEmptyStrings(run.Repo), CurrentStage: run.Stage,
		CurrentStep: firstRunNonEmpty(run.WaitingReason, run.TriageVerdict), Artifacts: artifacts, LinkedPRs: prs, LinkedIssues: issues,
		Contributors: nonEmptyStrings(firstRunNonEmpty(run.Assignee, run.ClaimedBy)), LastActivity: firstRunNonEmpty(run.CompletedAt, run.StageStartedAt),
		Status: runCampaignStatus(run), Engine: "Spektacular", Type: "spektacular", RunKey: run.Key,
		RunURL: "/api/runs/" + url.PathEscape(run.Key), LeaseOwner: run.Assignee,
	}
}

func campaignArchiveErrorStatus(err error) int {
	switch {
	case errors.Is(err, knowledge.ErrCampaignLeaseHeld):
		return http.StatusConflict
	case errors.Is(err, knowledge.ErrCampaignNoLease):
		return http.StatusConflict
	case strings.Contains(err.Error(), "not found"):
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

func inceptionCampaignStatus(phase knowledge.InceptionPhase) string {
	switch phase {
	case knowledge.PhaseComplete:
		return "shipped"
	case knowledge.PhaseScaffold:
		return "in review"
	case knowledge.PhaseStructure:
		return "planned"
	case knowledge.PhaseClarify, knowledge.PhaseCapture:
		return "drafting"
	default:
		return "parked"
	}
}

func runCampaignStatus(run Run) string {
	if run.State == "completed" || run.Stage == "completed" {
		return "shipped"
	}
	if run.WaitingOn == RunWaitingOnHuman {
		return "in review"
	}
	switch run.Stage {
	case StageSpec:
		return "drafting"
	case StagePlan:
		return "planned"
	case StageImplement:
		return "implementing"
	default:
		if run.State == "queued" {
			return "parked"
		}
		return firstRunNonEmpty(run.State, "parked")
	}
}

func (s *Server) runByCampaignKey(key string) (*Run, bool) {
	if s == nil || s.contributeHub == nil || key == "" {
		return nil, false
	}
	runs, err := s.activeRuns(true)
	if err != nil {
		return nil, false
	}
	for _, run := range runs {
		if run.Key == key || run.LeaseKey == key {
			cp := run
			return &cp, true
		}
	}
	return nil, false
}

func spektacularResumeCommand(c Campaign) string {
	stage := strings.TrimSpace(c.CurrentStage)
	if stage == "" || stage == StageImplement || stage == "completed" {
		stage = StagePlan
	}
	return "spektacular " + stage + " status " + c.ID
}

func campaignMatchesSearch(c Campaign, q string) bool {
	fields := []string{c.ID, c.Title, c.Source, c.CurrentStage, c.CurrentStep, c.Status, c.Engine, c.Type, c.RunKey, c.LeaseOwner}
	fields = append(fields, c.Repos...)
	fields = append(fields, c.LinkedIssues...)
	fields = append(fields, c.LinkedPRs...)
	fields = append(fields, c.Contributors...)
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), q) {
			return true
		}
	}
	return false
}

func campaignHasValue(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(strings.ToLower(value), want) {
			return true
		}
	}
	return false
}

func nonEmptyStrings(values ...string) []string {
	out := []string{}
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}
