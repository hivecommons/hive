package dashboard

import (
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/worksource"
)

// Campaign is the dashboard projection for resumable inception/Spektacular work.
type Campaign struct {
	ID             string             `json:"id"`
	Title          string             `json:"title"`
	Source         string             `json:"source,omitempty"`
	Repos          []string           `json:"repos,omitempty"`
	CurrentStage   string             `json:"current_stage"`
	CurrentStep    string             `json:"current_step,omitempty"`
	Artifacts      []CampaignArtifact `json:"artifacts,omitempty"`
	LinkedPRs      []string           `json:"linked_prs,omitempty"`
	LinkedIssues   []string           `json:"linked_issues,omitempty"`
	Contributors   []string           `json:"contributors,omitempty"`
	LastActivity   string             `json:"last_activity,omitempty"`
	ActivityLine   string             `json:"activity_line,omitempty"`
	Status         string             `json:"status"`
	Engine         string             `json:"engine"`
	Type           string             `json:"type"`
	RunKey         string             `json:"run_key,omitempty"`
	RunURL         string             `json:"run_url,omitempty"`
	RunGen         uint64             `json:"run_gen,omitempty"`
	ArtifactID     string             `json:"artifact_id,omitempty"`
	DocumentStatus string             `json:"document_status,omitempty"`
	Interview      *RunInterviewState `json:"interview,omitempty"`
	LeaseOwner     string             `json:"lease_owner,omitempty"`
	RevisionOf     string             `json:"revision_of,omitempty"`
	Revision       int                `json:"revision,omitempty"`
	History        []CampaignGenEntry `json:"history,omitempty"`
	Recheck        *CampaignRecheck   `json:"recheck,omitempty"`
	Drift          *CampaignDrift     `json:"drift,omitempty"`

	reviseLeaseHeld bool
	closedGen       uint64
}

// CampaignGenEntry is one closed generation of a campaign's run, as recorded
// in the archive's generation log when a recheck rewound it. Campaign.Revision
// and Campaign.History are how clients follow a campaign across rechecks;
// Campaign.RevisionOf is deprecated, never written on v6, and is removed in
// the next release line (ADR 0020 v6 addendum, R4).
type CampaignGenEntry struct {
	Revision   int    `json:"revision"`
	ArchivedAt string `json:"archived_at,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Actor      string `json:"actor,omitempty"`
	RewoundAt  string `json:"rewound_at,omitempty"`
	LastGen    uint64 `json:"last_gen,omitempty"`
}

type CampaignRecheck struct {
	Enabled        bool                    `json:"enabled"`
	Interval       string                  `json:"interval,omitempty"`
	LastAt         string                  `json:"last_at,omitempty"`
	NextAt         string                  `json:"next_at,omitempty"`
	InFlight       bool                    `json:"in_flight"`
	LastDeltaCount int                     `json:"last_delta_count,omitempty"`
	ExternalCount  int                     `json:"external_count,omitempty"`
	SourcesFailed  []CampaignSourceFailure `json:"sources_failed,omitempty"`
}

type CampaignDrift struct {
	CodebaseChanged bool                       `json:"codebase_changed"`
	PriorHeadSHA    string                     `json:"prior_head_sha,omitempty"`
	CurrentHeadSHA  string                     `json:"current_head_sha,omitempty"`
	PriorRevision   string                     `json:"prior_revision,omitempty"`
	DeltaCount      int                        `json:"delta_count"`
	RecheckReason   string                     `json:"recheck_reason,omitempty"`
	External        []CampaignExternalEvidence `json:"external,omitempty"`
	ExternalCount   int                        `json:"external_count,omitempty"`
	SourcesFailed   []CampaignSourceFailure    `json:"sources_failed,omitempty"`
}

type CampaignExternalEvidence struct {
	Source      string `json:"source"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	URL         string `json:"url,omitempty"`
	PublishedAt string `json:"published_at,omitempty"`
	Summary     string `json:"summary,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
}

type CampaignSourceFailure struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
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
		// A Spektacular run's lease owner is its stage-lease identity
		// (hive-triage once admitted, then a contributor or executor), never a
		// dashboard user, and resume only reads the run back; gating it on
		// that identity refused every operator (hivecommons/hive#10059).
		if campaign.Type != "spektacular" && campaign.LeaseOwner != "" && campaign.LeaseOwner != requestUser(r) {
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
		// A revised run has a separate archive lease, not a stage lease.
		if campaign.RunKey != "" && campaign.Type == "spektacular" && campaign.Revision == 0 {
			// Run-backed campaigns expose the runner's stage lease, not a
			// separate campaign pickup lease. Revoking it here stops the
			// contributor and can remove the run from both API projections.
			jsonError(w, "run-backed campaigns cannot be released here; the contributor's stage lease is managed by the run lifecycle", http.StatusConflict)
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
		s.auditFromRequest(r, "campaign_revise", auditDetail("campaign", revision.ID, "revision", strconv.Itoa(revision.Revision), "type", campaign.Type), "")
		jsonResponse(w, campaignReviseResponse{OK: true, Campaign: revision, Message: "Campaign revision updated"})
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
			if revision, ok := byID[campaign.ID]; ok && revision.Type == campaign.Type {
				// Keep the live run's stage and artifacts while exposing its durable
				// revision and the independently releasable revise lease.
				campaign.Revision = revision.Revision
				campaign.RevisionOf = revision.RevisionOf
				campaign.History = revision.History
				campaign.Recheck = revision.Recheck
				campaign.Drift = revision.Drift
				campaign.reviseLeaseHeld = revision.reviseLeaseHeld
				campaign.closedGen = revision.closedGen
				if revision.Revision > 0 {
					campaign.LeaseOwner = revision.LeaseOwner
				}
				s.settleRewoundGeneration(&campaign, run)
			}
			byID[campaign.ID] = campaign
		}
	}
	s.decorateCampaignRechecks(byID)
	out := make([]Campaign, 0, len(byID))
	for _, campaign := range byID {
		out = append(out, campaign)
	}
	return out, nil
}

// settleRewoundGeneration applies a recheck's Drift to the run-backed card. A
// completion from a generation the rewind already closed never completes the
// rewound one; the current generation's completion clears Drift (R1, R2).
func (s *Server) settleRewoundGeneration(campaign *Campaign, run Run) {
	if campaign.Drift == nil || campaign.CurrentStage != "completed" {
		return
	}
	if run.Gen <= campaign.closedGen {
		campaign.CurrentStage = StageSpec
		campaign.Status = runCampaignStatus(Run{Stage: StageSpec})
		return
	}
	campaign.Drift = nil
	if s != nil && s.deps != nil && s.deps.Inception != nil {
		if _, err := s.deps.Inception.SetCampaignDrift(campaign.ID, nil); err != nil && s.logger != nil {
			s.logger.Warn("[spektacular] clearing converged campaign drift failed", "campaign", campaign.ID, "error", err)
		}
	}
}

func campaignGenerationLogFromArchive(history []knowledge.CampaignRevisionHistory) []CampaignGenEntry {
	if len(history) == 0 {
		return nil
	}
	out := make([]CampaignGenEntry, 0, len(history))
	for _, entry := range history {
		out = append(out, CampaignGenEntry{
			Revision: entry.Revision, ArchivedAt: formatRunTime(entry.ArchivedAt), Reason: entry.Reason,
			Actor: entry.Actor, RewoundAt: formatRunTime(entry.RewoundAt), LastGen: entry.LastGen,
		})
	}
	return out
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
		if archive.Drift != nil {
			stage = StageSpec
		}
		step = "revision"
		runKey = strings.TrimSpace(archive.Source)
		if runKey != "" {
			runURL = "/api/runs/" + url.PathEscape(runKey)
		}
		contributors = nil
	}
	reviseLeaseHeld := false
	if archive.Lease != nil && archive.Lease.Owner != "" && time.Now().Before(archive.Lease.ExpiresAt) {
		leaseOwner = archive.Lease.Owner
		reviseLeaseHeld = archive.Lease.Surface == "revise"
	}
	var closedGen uint64
	if n := len(archive.History); n > 0 {
		closedGen = archive.History[n-1].LastGen
	}
	artifacts := []CampaignArtifact{{Kind: "state", Label: "Inception state"}}
	for _, file := range archive.WikiFiles {
		artifacts = append(artifacts, CampaignArtifact{Kind: "fact", Label: file})
	}
	campaign := Campaign{
		ID: archive.ID, Title: title, Source: firstRunNonEmpty(archive.Source, "inception"), Repos: repos, CurrentStage: stage, CurrentStep: step,
		Artifacts: artifacts, LinkedIssues: linkedIssues, Contributors: contributors, LastActivity: formatRunTime(last),
		Status: status, Engine: firstRunNonEmpty(archive.Engine, "Spec Kit"), Type: firstRunNonEmpty(archive.Type, "inception"),
		RunKey: runKey, RunURL: runURL, LeaseOwner: leaseOwner, RevisionOf: archive.RevisionOf, Revision: archive.Revision,
		History: campaignGenerationLogFromArchive(archive.History),
		Recheck: campaignRecheckFromArchive(archive, false), Drift: campaignDriftFromArchive(archive),
		reviseLeaseHeld: reviseLeaseHeld, closedGen: closedGen,
	}
	if campaign.Recheck != nil && campaign.Drift != nil {
		campaign.Recheck.ExternalCount = campaign.Drift.ExternalCount
		campaign.Recheck.SourcesFailed = campaign.Drift.SourcesFailed
	}
	return campaign
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
	if run.WorkItem != nil && run.WorkItem.URL != "" {
		artifacts = append(artifacts, CampaignArtifact{Kind: "work_item", Label: "Source work item (" + firstRunNonEmpty(run.WorkItem.SourceType, "github") + ")", URL: run.WorkItem.URL})
	}
	if run.LastReceipt != "" {
		artifacts = append(artifacts, CampaignArtifact{Kind: "receipt", Label: run.LastReceipt})
	}
	return Campaign{
		ID: id, Title: run.Title, Source: run.Key, Repos: nonEmptyStrings(run.Repo), CurrentStage: run.Stage,
		CurrentStep: firstRunNonEmpty(run.CurrentStep, run.WaitingReason, run.TriageVerdict), Artifacts: artifacts, LinkedPRs: prs, LinkedIssues: issues,
		Contributors: nonEmptyStrings(firstRunNonEmpty(run.Assignee, run.ClaimedBy)), LastActivity: firstRunNonEmpty(run.LastActivity, run.CompletedAt, run.StageStartedAt), ActivityLine: run.ActivitySummary,
		Status: runCampaignStatus(run), Engine: "Spektacular", Type: "spektacular", RunKey: run.Key,
		RunURL: "/api/runs/" + url.PathEscape(run.Key), RunGen: run.Gen, LeaseOwner: run.Assignee, ArtifactID: run.ArtifactID, DocumentStatus: run.DocumentStatus, Interview: run.Interview,
	}
}

// campaignArtifactID translates campaign identity only at the CLI boundary.
// Issue-numbered worksource keys use Spektacular's artifact spelling for resume,
// while campaign IDs remain lossless for archive lookup and revision deduplication.
// Embedded external slugs have already been resolved by runKeyOfLease.
func campaignArtifactID(id string) string {
	if ref, ok := worksource.ParseKey(id); ok && ref.Number > 0 {
		return ref.ArtifactSlug()
	}
	return id
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
	return "spektacular " + stage + " status " + campaignArtifactID(c.ID)
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
