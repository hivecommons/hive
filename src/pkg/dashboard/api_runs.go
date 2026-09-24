package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	convergenceaudit "github.com/hivecommons/hive/pkg/convergence/audit"
	"github.com/hivecommons/hive/pkg/convergence/outcome"
	hubspoke "github.com/hivecommons/hive/pkg/hub/spoke"
	"github.com/hivecommons/hive/pkg/planning"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/worksource"
)

type RunWaitingOn string

const (
	RunWaitingOnAgent  RunWaitingOn = "agent"
	RunWaitingOnRemote RunWaitingOn = "remote"
	RunWaitingOnHuman  RunWaitingOn = "human"
	RunWaitingOnCI     RunWaitingOn = "ci"
	RunWaitingOnNone   RunWaitingOn = "none"
)

type RunStage struct {
	Name    string            `json:"name"`
	Status  string            `json:"status"`
	Gen     uint64            `json:"gen"`
	Receipt string            `json:"receipt,omitempty"`
	Actor   string            `json:"actor,omitempty"`
	At      string            `json:"at,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
	// Reason is the owner's explanation on a stage transition that carried one,
	// today only an owner reset (#8350); it is read back from the timeline
	// event so the run's history says why it stepped back.
	Reason string `json:"reason,omitempty"`
}

type RunWavePR struct {
	Repo  string `json:"repo"`
	Role  string `json:"role,omitempty"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
}

type RunReviewWave struct {
	Wave          int         `json:"wave"`
	PRs           []RunWavePR `json:"prs"`
	ApproveAction string      `json:"approve_action,omitempty"`
}

type RunBurndown struct {
	Source    string `json:"source"`
	Satisfied int    `json:"satisfied"`
	Remaining int    `json:"remaining"`
	Unknown   int    `json:"unknown"`
	Scope     int    `json:"scope"`
}

// runResetRequest is the body of POST /api/runs/{key}/reset (#8350).
type runResetRequest struct {
	To     string `json:"to"`
	Reason string `json:"reason"`
}

// runResetResponse is what POST /api/runs/{key}/reset returns on success.
type runResetResponse struct {
	OK        bool   `json:"ok"`
	Key       string `json:"key"`
	StageFrom string `json:"stage_from"`
	Stage     string `json:"stage"`
	Gen       uint64 `json:"gen"`
	Reason    string `json:"reason"`
}

// auditActionRunStageReset is the dashboard audit action handleRunReset books
// against the requesting owner (the agent audit sink separately records the
// system-side lease_stage_reset with the same stage/gen fields).
const auditActionRunStageReset = "run_stage_reset"

const auditActionRunAuditCampaign = "run_audit_campaign"

const runResetReasonTriageFix = "triage_fix"

const defaultAuditScopeDir = "/data/convergence/audit/scope"

type Run struct {
	Key            string       `json:"key"`
	LeaseKey       string       `json:"lease_key,omitempty"`
	Title          string       `json:"title"`
	Repo           string       `json:"repo"`
	State          string       `json:"state,omitempty"`
	Stage          string       `json:"stage"`
	Gen            uint64       `json:"gen"`
	StageStartedAt string       `json:"stage_started_at,omitempty"`
	CompletedAt    string       `json:"completed_at,omitempty"`
	WaitingOn      RunWaitingOn `json:"waiting_on"`
	WaitingReason  string       `json:"waiting_reason,omitempty"`
	WaitingSince   string       `json:"waiting_since,omitempty"`
	Assignee       string       `json:"assignee,omitempty"`
	// ClaimedBy / ClaimExpiresAt expose the issue claim recorded on the run's
	// lease (hivecommons/hive#8380). ClaimPosted says whether the claim
	// comment reached the forge or lives on the lease only. All omitempty:
	// absent while claims are off.
	ClaimedBy       string          `json:"claimed_by,omitempty"`
	ClaimExpiresAt  string          `json:"claim_expires_at,omitempty"`
	ClaimPosted     bool            `json:"claim_posted,omitempty"`
	LastReceipt     string          `json:"last_receipt,omitempty"`
	PlanEpicID      string          `json:"plan_epic_id,omitempty"`
	Stages          []RunStage      `json:"stages"`
	ReviewWaves     []RunReviewWave `json:"review_waves,omitempty"`
	WaveIDs         []string        `json:"wave_ids,omitempty"`
	Burndown        *RunBurndown    `json:"burndown,omitempty"`
	TriageVerdict   string          `json:"triage_verdict,omitempty"`
	TriageRationale string          `json:"triage_rationale,omitempty"`
	ArtifactName    string          `json:"artifact_name,omitempty"`
	DocumentStatus  string          `json:"document_status,omitempty"`
	CurrentStep     string          `json:"current_step,omitempty"`
}

type RunSummary = Run

type RunWaitSnapshot struct {
	Key          string
	Title        string
	Repo         string
	Stage        string
	Gen          uint64
	WaitingOn    string
	WaitingSince time.Time
	Link         string
}

type runAuditRequest struct {
	CampaignKey string `json:"campaign_key"`
	ScopeDir    string `json:"scope_dir"`
	Store       string `json:"store"`
	Generation  uint64 `json:"generation"`
	RunKey      string `json:"run_key"`
	RunURL      string `json:"run_url"`
}

type runAuditResponse struct {
	OK                 bool                      `json:"ok"`
	CampaignKey        string                    `json:"campaign_key"`
	ScopeDir           string                    `json:"scope_dir"`
	Store              string                    `json:"store"`
	Mode               string                    `json:"mode"`
	Generation         uint64                    `json:"generation"`
	Findings           int                       `json:"findings"`
	Receipts           int                       `json:"receipts"`
	Burndown           convergenceaudit.Burndown `json:"burndown"`
	PublicationEnabled bool                      `json:"publication_enabled"`
	PublicationSkipped bool                      `json:"publication_skipped"`
	PublicationReason  string                    `json:"publication_reason,omitempty"`
	Publication        *auditPublicationSummary  `json:"publication,omitempty"`
}

type auditPublicationSummary struct {
	Accepted     bool           `json:"accepted"`
	Publications map[string]int `json:"publications"`
}

type RunsSummary struct {
	Active         int `json:"active"`
	WaitingOnHuman int `json:"waiting_on_human"`
}

type runLeaseSnapshot struct {
	identity        string
	taskID          string
	repo            string
	number          int
	key             string
	leaseKey        string
	stage           string
	gen             uint64
	expiresAt       time.Time
	title           string
	stageStarted    time.Time
	claimedBy       string
	claimExpiresAt  time.Time
	claimPosted     bool
	triageVerdict   string
	triageRationale string
}

type currentTaskRunInfo struct {
	title     string
	startedAt time.Time
}

type runPlanSnapshot struct {
	epicID       string
	state        string
	reason       string
	waitingSince time.Time
	waveIDs      []string
}

type runHumanReviewHold struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	UpdatedAt time.Time `json:"updated_at"`
}

// runCheckpointPolicy is the resolved answer to "does this stage boundary
// wait for a human?", carried with the provenance an audit entry needs: which
// config said so, and why.
type runCheckpointPolicy struct {
	stage  string
	blocks bool
	reason string
	source string
}

var runReviewDispatchStatePath = "/data/review-dispatch-state.json"

func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	runs, err := s.activeRuns(false)
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, runs)
}

func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.PathValue("key"))
	if unescaped, err := url.PathUnescape(key); err == nil {
		key = strings.TrimSpace(unescaped)
	}
	if key == "" {
		jsonError(w, "run key required", http.StatusBadRequest)
		return
	}
	runs, err := s.activeRuns(true)
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	for _, run := range runs {
		if run.Key == key || run.LeaseKey == key {
			if err := s.populateRunBurndown(r, &run); err != nil {
				jsonError(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
			jsonResponse(w, run)
			return
		}
	}
	if run, ok := s.queuedRunByKey(key); ok {
		if err := s.populateRunBurndown(r, &run); err != nil {
			jsonError(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		jsonResponse(w, run)
		return
	}
	jsonError(w, "run not found", http.StatusNotFound)
}

func (s *Server) populateRunBurndown(r *http.Request, run *Run) error {
	if s == nil || s.deps == nil || s.deps.RunBurndown == nil || run == nil {
		return nil
	}
	burndown, err := s.deps.RunBurndown(r.Context(), run.Key)
	if err != nil {
		return err
	}
	run.Burndown = burndown
	return nil
}

func (s *Server) queuedRunByKey(key string) (Run, bool) {
	if s == nil || s.contributeHub == nil {
		return Run{}, false
	}
	snap := s.contributeHub.admissionQueueSnapshot(int(^uint(0)>>1), withheldNone)
	for _, item := range snap.queue {
		if item.Held || item.Key != key || item.SourceType != worksource.SourceTypeRun || item.ExternalID == "" {
			continue
		}
		stage := runStageFromLabels(item.Labels)
		if stage == "" {
			stage = StageImplement
		}
		return Run{
			Key:       item.Key,
			Title:     redactTokens(item.Title),
			Repo:      item.Repo,
			State:     "queued",
			Stage:     stage,
			WaitingOn: RunWaitingOnAgent,
			Stages:    leaseRunStages(stage, 0),
		}, true
	}
	return Run{}, false
}

func runStageFromLabels(labels []string) string {
	for _, label := range labels {
		if stage, ok := strings.CutPrefix(strings.TrimSpace(label), "stage/"); ok {
			switch stage {
			case StageSpec, StagePlan, StageImplement:
				return stage
			}
		}
	}
	return ""
}

// handleRunAudit serves POST /api/runs/audit: owner-triggered activation of
// the convergence audit campaign over an explicit or default scope directory.
func (s *Server) handleRunAudit(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil {
		jsonError(w, "dashboard dependencies unavailable", http.StatusServiceUnavailable)
		return
	}
	var body runAuditRequest
	if r.Body != nil {
		if err := decodeBody(r, &body); err != nil {
			jsonError(w, "invalid request body", http.StatusBadRequest)
			return
		}
	}
	body.CampaignKey = strings.TrimSpace(body.CampaignKey)
	if body.CampaignKey == "" {
		body.CampaignKey = "audit-campaign"
	}
	body.ScopeDir = strings.TrimSpace(body.ScopeDir)
	if body.ScopeDir == "" {
		body.ScopeDir = defaultAuditScopeDir
	}
	storeName, store := s.auditCampaignStore(strings.TrimSpace(body.Store))
	if store == nil {
		jsonError(w, "audit bead store unavailable", http.StatusServiceUnavailable)
		return
	}
	if s.deps.AuditLedger == nil || s.deps.AuditJournal == nil {
		jsonError(w, "audit mutation ledger unavailable", http.StatusServiceUnavailable)
		return
	}
	mode := config.ConvergenceModeShadow
	publicationEnabled := false
	if s.deps.Config != nil {
		mode = s.deps.Config.ConvergenceMode()
		publicationEnabled = s.deps.Config.Publication.Enabled
	}
	publisher := s.auditPublisher()
	publicationSkipped := false
	publicationReason := ""
	if !publicationEnabled {
		publisher = nil
		publicationSkipped = true
		publicationReason = "publication.disabled"
	} else if publisher == nil {
		publicationSkipped = true
		publicationReason = "publisher.unavailable"
	}
	res, err := convergenceaudit.Run(convergenceaudit.Options{
		CampaignKey: body.CampaignKey,
		ScopeDir:    body.ScopeDir,
		Store:       store,
		Ledger:      s.deps.AuditLedger,
		Journal:     s.deps.AuditJournal,
		ProofStore:  s.deps.AuditProofs,
		Mode:        mode,
		Generation:  body.Generation,
		Holder:      "dashboard",
		Publisher:   publisher,
		Outcomes:    s.auditOutcomes(),
		RunKey:      strings.TrimSpace(body.RunKey),
		RunURL:      strings.TrimSpace(body.RunURL),
	})
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.auditFromRequest(r, auditActionRunAuditCampaign, auditDetail(
		"campaign", body.CampaignKey, "scope_dir", body.ScopeDir,
		"store", storeName, "mode", res.Mode,
		"generation", strconv.FormatUint(res.Generation, 10)), "")
	jsonResponse(w, runAuditResponse{
		OK:                 true,
		CampaignKey:        body.CampaignKey,
		ScopeDir:           body.ScopeDir,
		Store:              storeName,
		Mode:               res.Mode,
		Generation:         res.Generation,
		Findings:           len(res.Findings),
		Receipts:           len(res.Receipts),
		Burndown:           res.Burndown,
		PublicationEnabled: publicationEnabled,
		PublicationSkipped: publicationSkipped,
		PublicationReason:  publicationReason,
		Publication:        summarizeAuditPublication(res),
	})
}

func (s *Server) auditPublisher() convergenceaudit.FindingPublisher {
	if s.deps == nil {
		return nil
	}
	if s.deps.AuditPublisherFunc != nil {
		return s.deps.AuditPublisherFunc()
	}
	return s.deps.AuditPublisher
}

func (s *Server) auditOutcomes() *outcome.Ledger {
	if s.deps == nil {
		return nil
	}
	if s.deps.AuditOutcomesFunc != nil {
		return s.deps.AuditOutcomesFunc()
	}
	return s.deps.AuditOutcomes
}

func (s *Server) auditCampaignStore(requested string) (string, *beads.Store) {
	if s.deps == nil || len(s.deps.BeadStores) == 0 {
		return "", nil
	}
	if requested != "" {
		return requested, s.deps.BeadStores[requested]
	}
	for _, name := range []string{"audit", "auditor", "scanner", "supervisor"} {
		if store := s.deps.BeadStores[name]; store != nil {
			return name, store
		}
	}
	names := make([]string, 0, len(s.deps.BeadStores))
	for name := range s.deps.BeadStores {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", nil
	}
	return names[0], s.deps.BeadStores[names[0]]
}

func summarizeAuditPublication(res convergenceaudit.Result) *auditPublicationSummary {
	if res.Publication == nil {
		return nil
	}
	counts := make(map[string]int)
	for _, pub := range res.Publication.Publications {
		counts[pub.State]++
	}
	return &auditPublicationSummary{Accepted: res.Publication.Accepted, Publications: counts}
}

// handleRunReset serves POST /api/runs/{key}/reset (#8350): move a run's lease
// back to an earlier stage, minting a new generation so the relay working the
// old generation cannot resume it, and record why.
//
// OWNER-ONLY. This is the only backwards stage move; a read-write member being
// able to knock a run out of implement would undo an owner's plan approval
// from the other side, so it sits behind the same gate as approve/reject. The
// gate runs before anything else so an unverified caller learns nothing about
// which runs exist. Body: {"to": "<stage>", "reason": "<why>"}; both required.
func (s *Server) handleRunReset(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if unescaped, err := url.PathUnescape(key); err == nil {
		key = strings.TrimSpace(unescaped)
	}
	if key == "" {
		jsonError(w, "run key required", http.StatusBadRequest)
		return
	}
	if s.contributeHub == nil {
		jsonError(w, "run lease registry unavailable", http.StatusServiceUnavailable)
		return
	}
	var body runResetRequest
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}
	body.To = strings.TrimSpace(body.To)
	body.Reason = sanitizeString(body.Reason)
	if body.Reason == "" || (body.Reason != runResetReasonTriageFix && body.To == "") {
		jsonError(w, "to and reason are required", http.StatusBadRequest)
		return
	}
	now := time.Now()
	held, ok := s.contributeHub.runLeaseHolder(key, now)
	if !ok {
		jsonError(w, "run not found", http.StatusNotFound)
		return
	}
	if body.Reason == runResetReasonTriageFix {
		if held.stage != StageSpec {
			jsonError(w, "triage_fix can only retire a spec-stage run", http.StatusBadRequest)
			return
		}
		s.contributeHub.revokeLease(held.identity, held.taskID)
		s.LifecycleTimeline().Record(timeline.Event{
			IssueRef: key,
			Kind:     timeline.KindStageCompleted,
			Agent:    held.identity,
			Attrs: map[string]string{
				"stage_from": held.stage,
				"stage_to":   "fix",
				"gen":        strconv.FormatUint(held.gen, 10),
				"reason":     body.Reason,
				"reset":      "true",
			},
		})
		s.auditFromRequest(r, auditActionRunStageReset, auditDetail(
			"run", key, "stage_from", held.stage, "stage_to", "fix",
			"reason", body.Reason, "gen", strconv.FormatUint(held.gen, 10)), "")
		jsonResponse(w, runResetResponse{
			OK: true, Key: key, StageFrom: held.stage, Stage: "fix", Gen: held.gen, Reason: body.Reason,
		})
		return
	}
	lease, err := s.contributeHub.resetLeaseStage(held.identity, held.taskID, body.To, body.Reason, now)
	if err != nil {
		jsonError(w, err.Error(), runResetErrorStatus(err))
		return
	}
	s.auditFromRequest(r, auditActionRunStageReset, auditDetail(
		"run", key, "stage_from", held.stage, "stage_to", lease.stage,
		"reason", body.Reason, "gen", strconv.FormatUint(lease.gen, 10)), "")
	jsonResponse(w, runResetResponse{
		OK: true, Key: key, StageFrom: held.stage, Stage: lease.stage, Gen: lease.gen, Reason: body.Reason,
	})
}

// runResetErrorStatus maps a refused reset to its HTTP status: a run that has
// gone away is 404, one whose lease lapsed between lookup and reset is 409, a
// bad target stage or missing reason is 400, and a persist failure is 500 so
// the owner knows the registry, not the request, is the problem.
func runResetErrorStatus(err error) int {
	switch {
	case errors.Is(err, errLeaseNotFound):
		return http.StatusNotFound
	case errors.Is(err, errLeaseExpired):
		return http.StatusConflict
	case errors.Is(err, errLeaseStageInvalid), errors.Is(err, errLeaseResetReason):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) activeRuns(includeTimeline bool) ([]Run, error) {
	leases, err := s.activeRunLeaseSnapshots(time.Now())
	if err != nil {
		return nil, err
	}
	plans := s.runPlanSnapshots()
	holds := runHumanReviewHolds(runReviewDispatchStatePath)
	runs := make([]Run, 0, len(leases))
	active := map[string]bool{}
	for _, lease := range leases {
		active[lease.key] = true
		active[lease.leaseKey] = true
		plan := firstRunPlanSnapshot(plans[lease.key], plans[lease.leaseKey])
		run := runFromLease(lease, plan, firstRunHold(holds[lease.key], holds[lease.leaseKey]), s.runsConfigSnapshot())
		if run.PlanEpicID != "" {
			run.ReviewWaves = s.planReviewWaves(run.PlanEpicID)
		}
		events := append(s.LifecycleTimeline().ByIssue(lease.key), s.LifecycleTimeline().ByIssue(lease.leaseKey)...)
		applyRunArtifactStatus(&run, events)
		run.LastReceipt = latestRunReceipt(events)
		if run.StageStartedAt == "" {
			run.StageStartedAt = formatRunTime(timelineStageTime(events, lease.stage))
		}
		if includeTimeline {
			run.Stages = mergeRunTimelineStages(run.Stages, events)
		}
		runs = append(runs, run)
	}
	for _, journey := range s.LifecycleTimeline().Journeys(0) {
		if active[journey.Ref] {
			continue
		}
		run, ok := completedRunFromJourney(journey, includeTimeline, plans[journey.Ref])
		if ok {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].StageStartedAt != runs[j].StageStartedAt {
			return runs[i].StageStartedAt < runs[j].StageStartedAt
		}
		return runs[i].Key < runs[j].Key
	})
	return runs, nil
}

func (s *Server) planReviewWaves(epicID string) []RunReviewWave {
	if s == nil || s.deps == nil || strings.TrimSpace(epicID) == "" {
		return nil
	}
	byWave := map[int][]RunWavePR{}
	for _, store := range s.deps.BeadStores {
		if store == nil {
			continue
		}
		tree, err := planning.GetPlanTree(store, epicID)
		if err != nil || tree == nil {
			continue
		}
		for _, child := range tree.Children {
			wave, err := strconv.Atoi(strings.TrimSpace(child.Wave))
			if err != nil || wave <= 0 || child.PRURL == "" {
				continue
			}
			repo := strings.TrimSpace(child.Repo)
			if repo == "" {
				repo = strings.TrimSpace(child.Title)
			}
			byWave[wave] = append(byWave[wave], RunWavePR{
				Repo: repo, Role: child.RepoRole, URL: child.PRURL, Title: child.Title,
			})
		}
		break
	}
	if len(byWave) == 0 {
		return nil
	}
	waves := make([]int, 0, len(byWave))
	for wave := range byWave {
		waves = append(waves, wave)
	}
	sort.Ints(waves)
	out := make([]RunReviewWave, 0, len(waves))
	for _, wave := range waves {
		out = append(out, RunReviewWave{Wave: wave, PRs: byWave[wave], ApproveAction: "approve_plan_wave"})
	}
	return out
}

func (s *Server) activeRunLeaseSnapshots(now time.Time) ([]runLeaseSnapshot, error) {
	if s == nil || s.contributeHub == nil {
		return nil, errors.New("run lease registry unavailable")
	}
	h := s.contributeHub
	infos := h.currentTaskInfos()
	out := []runLeaseSnapshot{}
	h.leaseMu.Lock()
	defer h.leaseMu.Unlock()
	for _, l := range h.leases {
		if l == nil || l.stage == "" || l.expiresAt.IsZero() || now.After(l.expiresAt) {
			continue
		}
		stageLeaseKey := leaseWorkKey(l)
		key := s.canonicalRunKey(l.repo, l.number, runKeyOfLease(stageLeaseKey, l.repo), stageLeaseKey)
		repo := s.canonicalRunRepo(l.repo, key)
		info := infos[leaseKey(l.identity, l.taskID)]
		title := info.title
		if title == "" {
			title = l.title
		}
		if title == "" {
			title = key
		}
		out = append(out, runLeaseSnapshot{
			identity: l.identity, taskID: l.taskID, repo: repo, number: l.number,
			key: key, leaseKey: stageLeaseKey, stage: l.stage, gen: l.gen, expiresAt: l.expiresAt,
			title: title, stageStarted: info.startedAt,
			claimedBy: l.claimedBy, claimExpiresAt: l.claimExpiresAt, claimPosted: l.claimPosted,
			triageVerdict: l.triageVerdict, triageRationale: l.triageRationale,
		})
	}
	return out, nil
}

func (h *ContributeWSHub) currentTaskInfos() map[string]currentTaskRunInfo {
	out := map[string]currentTaskRunInfo{}
	if h == nil {
		return out
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.connections {
		if c == nil {
			continue
		}
		c.mu.Lock()
		if c.currentTask != nil {
			out[leaseKey(identityOf(c), c.currentTask.TaskID)] = currentTaskRunInfo{
				title:     c.currentTask.Title,
				startedAt: c.taskAssignedAt,
			}
		}
		c.mu.Unlock()
	}
	return out
}

func runFromLease(lease runLeaseSnapshot, plan runPlanSnapshot, hold runHumanReviewHold, cfgs ...*config.Config) Run {
	var cfg *config.Config
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	started := formatRunTime(lease.stageStarted)
	run := Run{
		Key: lease.key, LeaseKey: lease.leaseKey, Title: redactTokens(lease.title), Repo: lease.repo,
		State: "active", Stage: lease.stage, Gen: lease.gen, StageStartedAt: started,
		WaitingOn: RunWaitingOnAgent, Assignee: lease.identity,
		ClaimedBy: lease.claimedBy, ClaimExpiresAt: formatRunTime(lease.claimExpiresAt), ClaimPosted: lease.claimPosted,
		PlanEpicID:    plan.epicID,
		WaveIDs:       append([]string(nil), plan.waveIDs...),
		Stages:        leaseRunStages(lease.stage, lease.gen),
		TriageVerdict: lease.triageVerdict, TriageRationale: lease.triageRationale,
	}
	if plan.epicID != "" && (plan.state == planning.PlanStateReview ||
		plan.state == planning.PlanStateStuck || plan.state == planning.PlanStateDesignReview || plan.state == planning.PlanStateDesignStuck) {
		if decision := runCheckpointPolicyForConfig(cfg, lease.stage); decision.blocks {
			run.WaitingOn = RunWaitingOnHuman
			run.WaitingReason = firstRunNonEmpty(plan.reason, decision.reason)
			run.WaitingSince = formatRunTime(plan.waitingSince)
		}
	}
	if plan.reason == planning.WaitingReasonStalePlan {
		run.WaitingOn = RunWaitingOnHuman
		run.WaitingReason = plan.reason
		run.WaitingSince = formatRunTime(plan.waitingSince)
	}
	if !hold.UpdatedAt.IsZero() && runCheckpointBlocks(cfg, lease.stage) {
		run.WaitingOn = RunWaitingOnHuman
		run.WaitingSince = formatRunTime(hold.UpdatedAt)
	}
	return run
}

// runsConfigSnapshot is the nil-safe accessor for the running config the run
// projection consults.
func (s *Server) runsConfigSnapshot() *config.Config {
	if s == nil || s.deps == nil {
		return nil
	}
	return s.deps.Config
}

// runCheckpointPolicy resolves the checkpoint decision for one stage against
// this server's running config.
func (s *Server) runCheckpointPolicy(stage string) runCheckpointPolicy {
	var cfg *config.Config
	if s != nil && s.deps != nil {
		cfg = s.deps.Config
	}
	return runCheckpointPolicyForConfig(cfg, stage)
}

// runCheckpointBlocks is the boolean shorthand over runCheckpointPolicyForConfig.
func runCheckpointBlocks(cfg *config.Config, stage string) bool {
	return runCheckpointPolicyForConfig(cfg, stage).blocks
}

// runCheckpointPolicyForConfig fails closed: with no config, an unrecognised
// stage, or an ACMM level below RunImplementCheckpointMinACMM for the
// implement boundary, the checkpoint blocks. Only an explicit `false` on a
// recognised stage at a permitted ACMM level opens the gate.
func runCheckpointPolicyForConfig(cfg *config.Config, stage string) runCheckpointPolicy {
	normalized := strings.TrimSpace(strings.ToLower(stage))
	policy := runCheckpointPolicy{stage: normalized, blocks: true, reason: "checkpoint_enabled", source: "runtime config"}
	if cfg == nil {
		policy.reason = "config_unavailable"
		return policy
	}
	if strings.TrimSpace(cfg.SourcePath) != "" {
		policy.source = cfg.SourcePath
	}
	if normalized == StageImplement && cfg.ACMMLevelOrZero() < config.RunImplementCheckpointMinACMM {
		policy.reason = fmt.Sprintf("implement checkpoint relaxation requires ACMM L%d; current ACMM L%d", config.RunImplementCheckpointMinACMM, cfg.ACMMLevelOrZero())
		return policy
	}
	if cfg.Runs.CheckpointBlocks(normalized) {
		return policy
	}
	policy.blocks = false
	policy.reason = runCheckpointDisabledReason
	return policy
}

func completedRunFromJourney(j timeline.Journey, includeTimeline bool, plan runPlanSnapshot) (Run, bool) {
	stage := j.Stages[timeline.KindStageCompleted]
	if stage == nil || stage.Attrs == nil ||
		stage.Attrs["stage_from"] != StageImplement || stage.Attrs["stage_to"] != "completed" {
		return Run{}, false
	}
	gen, _ := strconv.ParseUint(stage.Attrs["gen"], 10, 64)
	repo := ""
	if ref, ok := worksource.ParseKey(j.Ref); ok {
		repo = ref.Repo
	}
	run := Run{
		Key: j.Ref, Title: j.Ref, Repo: repo,
		State: "completed", Stage: "completed", Gen: gen,
		StageStartedAt: formatRunTime(time.UnixMilli(stage.FirstAt)),
		CompletedAt:    formatRunTime(time.UnixMilli(stage.LastAt)),
		WaitingOn:      RunWaitingOnNone,
		Assignee:       stage.Agent,
		PlanEpicID:     plan.epicID,
		WaveIDs:        append([]string(nil), plan.waveIDs...),
		Stages:         completedRunStages(gen),
	}
	if includeTimeline {
		run.Stages = mergeRunTimelineStages(run.Stages, synthesizeRunJourneyEvents(j))
	}
	return run, true
}

func leaseRunStages(current string, gen uint64) []RunStage {
	out := make([]RunStage, 0, len(orderedLeaseStages))
	seenCurrent := false
	for _, name := range orderedLeaseStages {
		status := "pending"
		stageGen := uint64(0)
		switch {
		case name == current:
			status = "current"
			stageGen = gen
			seenCurrent = true
		case !seenCurrent:
			status = "completed"
		}
		out = append(out, RunStage{Name: name, Status: status, Gen: stageGen})
	}
	return out
}

func completedRunStages(gen uint64) []RunStage {
	out := make([]RunStage, 0, len(orderedLeaseStages))
	for _, name := range orderedLeaseStages {
		stageGen := uint64(0)
		if name == StageImplement {
			stageGen = gen
		}
		out = append(out, RunStage{Name: name, Status: "completed", Gen: stageGen})
	}
	return out
}

func synthesizeRunJourneyEvents(j timeline.Journey) []timeline.Event {
	events := make([]timeline.Event, 0, len(j.Stages))
	for kind, stage := range j.Stages {
		if stage == nil {
			continue
		}
		events = append(events, timeline.Event{
			IssueRef: j.Ref,
			Kind:     kind,
			Agent:    stage.Agent,
			At:       stage.LastAt,
			Attrs:    stage.Attrs,
		})
	}
	sort.Slice(events, func(i, k int) bool {
		if events[i].At != events[k].At {
			return events[i].At > events[k].At
		}
		return events[i].ID < events[k].ID
	})
	return events
}

func mergeRunTimelineStages(stages []RunStage, events []timeline.Event) []RunStage {
	if len(events) == 0 {
		return stages
	}
	out := append([]RunStage{}, stages...)
	for _, ev := range events {
		name := string(ev.Kind)
		if name == "" {
			continue
		}
		gen := uint64(0)
		receipt, reason := "", ""
		if ev.Attrs != nil {
			if raw := ev.Attrs["gen"]; raw != "" {
				gen, _ = strconv.ParseUint(raw, 10, 64)
			}
			receipt = firstRunNonEmpty(ev.Attrs["receipt"], ev.Attrs["receipt_digest"], ev.Attrs["path"], ev.Attrs["digest"])
			reason = ev.Attrs["reason"]
		}
		attrs := map[string]string(nil)
		if len(ev.Attrs) > 0 {
			attrs = make(map[string]string, len(ev.Attrs))
			for k, v := range ev.Attrs {
				attrs[k] = v
			}
		}
		out = append(out, RunStage{
			Name: name, Status: "observed", Gen: gen, Receipt: receipt, Reason: reason,
			Actor: ev.Agent, At: formatRunTime(time.UnixMilli(ev.At)), Attrs: attrs,
		})
	}
	return out
}

func applyRunArtifactStatus(run *Run, events []timeline.Event) {
	if run == nil {
		return
	}
	for _, ev := range events {
		if ev.Attrs == nil {
			continue
		}
		if run.ArtifactName == "" {
			run.ArtifactName = firstRunNonEmpty(ev.Attrs[stageAttrArtifact], ev.Attrs[stageAttrPath])
		}
		if run.DocumentStatus == "" {
			run.DocumentStatus = ev.Attrs[stageAttrDocumentStatus]
		}
		if run.CurrentStep == "" {
			run.CurrentStep = firstRunNonEmpty(ev.Attrs["current_step"], ev.Attrs["step"])
		}
		if run.ArtifactName != "" && run.DocumentStatus != "" && run.CurrentStep != "" {
			return
		}
	}
}

func latestRunReceipt(events []timeline.Event) string {
	for _, ev := range events {
		if ev.Attrs == nil {
			continue
		}
		if receipt := firstRunNonEmpty(ev.Attrs["receipt"], ev.Attrs["receipt_digest"], ev.Attrs["path"], ev.Attrs["digest"]); receipt != "" {
			return receipt
		}
	}
	return ""
}

func timelineStageTime(events []timeline.Event, stage string) time.Time {
	for _, ev := range events {
		if string(ev.Kind) == stage && ev.At > 0 {
			return time.UnixMilli(ev.At)
		}
	}
	return time.Time{}
}

func (s *Server) runPlanSnapshots() map[string]runPlanSnapshot {
	out := map[string]runPlanSnapshot{}
	if s == nil || s.deps == nil {
		return out
	}
	for _, p := range planning.ListPlans(s.deps.BeadStores) {
		if p.IssueRepo == "" || p.IssueNumber == "" {
			continue
		}
		key := s.canonicalRunKey(p.IssueRepo, atoiOrZero(p.IssueNumber), "", "")
		out[key] = runPlanSnapshot{epicID: p.EpicID, state: p.State}
	}
	for _, store := range s.deps.BeadStores {
		if store == nil {
			continue
		}
		for _, b := range store.List(beads.ListFilter{}) {
			if b.Type != beads.TypeEpic || b.Meta(planning.MetaPlanStatus) == "" {
				continue
			}
			repo, number, runKey := b.Meta(planning.MetaIssueRepo), b.Meta(planning.MetaIssueNumber), b.Meta(planning.MetaRunKey)
			if repo == "" || (number == "" && runKey == "") {
				continue
			}
			key := s.canonicalRunKey(repo, atoiOrZero(number), runKey, "")
			if number == "" {
				key = s.qualifyRunRepo(repo) + "!" + runKey
			}
			snap := out[key]
			snap.epicID = firstRunNonEmpty(snap.epicID, b.ID)
			switch b.Meta(planning.MetaPlanStatus) {
			case planning.PlanStatusDraft:
				snap.state = firstRunNonEmpty(snap.state, planning.PlanStateReview)
			case planning.PlanStatusApproved:
				snap.state = firstRunNonEmpty(snap.state, planning.PlanStateExecuting)
			}
			snap.waveIDs = splitRunWaveIDs(b.Meta(planning.MetaRunWaveIDs))
			if reason := b.Meta(planning.MetaRunWaitingReason); reason != "" {
				snap.reason = reason
			}
			snap.waitingSince = b.UpdatedAt.Time
			if number != "" {
				out[s.canonicalRunKey(repo, atoiOrZero(number), "", "")] = snap
			}
			if runKey != "" && repo != "" {
				qualifiedRepo := s.qualifyRunRepo(repo)
				for _, stage := range orderedLeaseStages {
					out[qualifiedRepo+"!"+runKey+":"+stage] = snap
					out[repo+"!"+runKey+":"+stage] = snap
				}
				out[qualifiedRepo+"!"+runKey] = snap
				out[repo+"!"+runKey] = snap
				if canonical := s.canonicalRunKey(repo, 0, runKey, ""); canonical != "" {
					out[canonical] = snap
				}
			}
		}
	}
	return out
}

func (s *Server) canonicalRunKey(repo string, number int, runKey, fallback string) string {
	runKey = strings.TrimSpace(runKey)
	if ref, ok := worksource.ParseKey(runKey); ok && ref.Number > 0 {
		return worksource.Ref{Repo: s.qualifyRunRepo(ref.Repo), Number: ref.Number}.Key()
	}
	if number > 0 {
		return worksource.Ref{Repo: s.qualifyRunRepo(repo), Number: number}.Key()
	}
	if strings.TrimSpace(fallback) != "" {
		return strings.TrimSpace(fallback)
	}
	return runKey
}

func (s *Server) canonicalRunRepo(repo, key string) string {
	if ref, ok := worksource.ParseKey(key); ok && ref.Repo != "" {
		return ref.Repo
	}
	return s.qualifyRunRepo(repo)
}

func (s *Server) qualifyRunRepo(repo string) string {
	repo = strings.TrimSpace(repo)
	if repo == "" || strings.Contains(repo, "/") || s == nil || s.deps == nil || s.deps.Config == nil {
		return repo
	}
	return config.QualifyRepo(s.deps.Config.Project.Org, repo)
}

func atoiOrZero(raw string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(raw))
	return n
}

func firstRunPlanSnapshot(a, b runPlanSnapshot) runPlanSnapshot {
	if a.epicID != "" || a.state != "" || a.reason != "" || !a.waitingSince.IsZero() || len(a.waveIDs) > 0 {
		return a
	}
	return b
}

func firstRunHold(a, b runHumanReviewHold) runHumanReviewHold {
	if !a.UpdatedAt.IsZero() {
		return a
	}
	return b
}

func splitRunWaveIDs(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func runHumanReviewHolds(path string) map[string]runHumanReviewHold {
	out := map[string]runHumanReviewHold{}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var state struct {
		Human []runHumanReviewHold `json:"requires_human"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return out
	}
	for _, hold := range state.Human {
		if hold.Repo == "" || hold.Number == 0 {
			continue
		}
		out[worksource.Ref{Repo: hold.Repo, Number: hold.Number}.Key()] = hold
	}
	return out
}

func (s *Server) HeartbeatRunsSummary() *hubspoke.RunsSummary {
	runs, err := s.activeRuns(false)
	if err != nil {
		return nil
	}
	return heartbeatRunsSummary(runs, func(key string) []timeline.Event {
		if s == nil {
			return nil
		}
		return s.LifecycleTimeline().ByIssue(key)
	}, time.Now())
}

func (s *Server) RunWaitSnapshot() []RunWaitSnapshot {
	runs, err := s.activeRuns(false)
	if err != nil {
		return nil
	}
	out := make([]RunWaitSnapshot, 0, len(runs))
	for _, run := range runs {
		var since time.Time
		if run.WaitingSince != "" {
			since, _ = time.Parse(time.RFC3339, run.WaitingSince)
		}
		out = append(out, RunWaitSnapshot{
			Key:          run.Key,
			Title:        run.Title,
			Repo:         run.Repo,
			Stage:        run.Stage,
			Gen:          run.Gen,
			WaitingOn:    string(run.WaitingOn),
			WaitingSince: since,
			Link:         s.runDashboardURL(run.Key),
		})
	}
	return out
}

func heartbeatRunsSummary(runs []Run, eventsFor func(string) []timeline.Event, now time.Time) *hubspoke.RunsSummary {
	active := len(runs)
	waiting := 0
	var oldestWait *int64
	var lastCompleted string
	for _, run := range runs {
		if run.WaitingOn == RunWaitingOnHuman {
			waiting++
			if t, err := time.Parse(time.RFC3339, run.WaitingSince); err == nil {
				secs := int64(now.Sub(t).Seconds())
				if secs < 0 {
					secs = 0
				}
				if oldestWait == nil || secs > *oldestWait {
					v := secs
					oldestWait = &v
				}
			}
		}
		if eventsFor != nil {
			for _, ev := range eventsFor(run.Key) {
				if ev.Kind != timeline.KindStageCompleted || ev.At <= 0 {
					continue
				}
				at := time.UnixMilli(ev.At).UTC().Format(time.RFC3339)
				if at > lastCompleted {
					lastCompleted = at
				}
			}
		}
	}
	summary := &hubspoke.RunsSummary{
		Active:         &active,
		WaitingOnHuman: &waiting,
	}
	if oldestWait != nil {
		summary.OldestWaitSeconds = oldestWait
	}
	if lastCompleted != "" {
		summary.LastStageCompletedAt = &lastCompleted
	}
	return summary
}

func formatRunTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func firstRunNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// SnapshotRunHistoryLimit bounds the recent finished runs carried in the
// public status/snapshot payload (#8349). The payload is public and cached, so
// the history is a fixed-size window, never the whole timeline.
const SnapshotRunHistoryLimit = 20

const (
	// RunOutcomeActive marks a run that still holds a live stage lease.
	RunOutcomeActive = "active"
	// RunOutcomeCompleted marks a run whose last recorded stage completion has
	// no live lease behind it any more.
	RunOutcomeCompleted = "completed"
	// RunOutcomeMerged marks a completed run whose journey reached merge.
	RunOutcomeMerged = "merged"
)

// RunHistoryEntry is one bounded, public-safe row of run history: no lease
// ids, no tokens, and a title that passed the status token redactor (#8349).
type RunHistoryEntry struct {
	Key            string       `json:"key"`
	Title          string       `json:"title"`
	Repo           string       `json:"repo,omitempty"`
	Stage          string       `json:"stage"`
	Gen            uint64       `json:"gen"`
	WaitingOn      RunWaitingOn `json:"waiting_on"`
	Outcome        string       `json:"outcome"`
	StageStartedAt string       `json:"stage_started_at,omitempty"`
	CompletedAt    string       `json:"completed_at,omitempty"`
}

// RunHistory is the snapshot's run block: the runs holding a live lease plus
// the most recent finished ones. A nil RunHistory on the status payload means
// the spoke could not project runs at all and the consumer must render
// "runs: unknown", never zero.
type RunHistory struct {
	Active []RunHistoryEntry `json:"active"`
	Recent []RunHistoryEntry `json:"recent"`
	Limit  int               `json:"limit"`
}

// scrubRunTitle runs a run title through the status token redactor (GitHub
// tokens, sk- API keys, device codes) so no credential-shaped text can reach
// a public payload. It deliberately reuses the redactor pkg/dashboard already
// has rather than importing pkg/logscrub: the package import-count ratchet
// (import_count_test.go) is at its ceiling, and the timeline never stores
// anything the status redactor does not already cover.
func scrubRunTitle(title string) string {
	return redactTokens(title)
}

// runHistoryFromProjection builds the bounded snapshot run history from the
// active-run projection and the lifecycle timeline journeys. Finished runs are
// journeys with a recorded stage completion and no live lease.
func runHistoryFromProjection(active []Run, journeys []timeline.Journey, limit int) *RunHistory {
	if limit <= 0 {
		limit = SnapshotRunHistoryLimit
	}
	history := &RunHistory{
		Active: make([]RunHistoryEntry, 0, len(active)),
		Recent: []RunHistoryEntry{},
		Limit:  limit,
	}
	liveKeys := make(map[string]bool, len(active))
	for _, run := range active {
		liveKeys[run.Key] = true
		history.Active = append(history.Active, RunHistoryEntry{
			Key: run.Key, Title: scrubRunTitle(run.Title), Repo: run.Repo,
			Stage: run.Stage, Gen: run.Gen, WaitingOn: run.WaitingOn,
			Outcome: RunOutcomeActive, StageStartedAt: run.StageStartedAt,
		})
	}
	for _, j := range journeys {
		if j.Ref == "" || liveKeys[j.Ref] {
			continue
		}
		st := j.Stages[timeline.KindStageCompleted]
		if st == nil || st.LastAt <= 0 {
			continue
		}
		entry := RunHistoryEntry{
			Key:            j.Ref,
			Title:          j.Ref,
			Stage:          st.Attrs["stage_to"],
			WaitingOn:      RunWaitingOnNone,
			Outcome:        RunOutcomeCompleted,
			StageStartedAt: formatRunTime(time.UnixMilli(st.FirstAt)),
			CompletedAt:    formatRunTime(time.UnixMilli(st.LastAt)),
		}
		if title := st.Attrs["title"]; title != "" {
			entry.Title = scrubRunTitle(title)
		}
		if raw := st.Attrs["gen"]; raw != "" {
			entry.Gen, _ = strconv.ParseUint(raw, 10, 64)
		}
		if repo, _, ok := strings.Cut(j.Ref, "#"); ok {
			entry.Repo = repo
		}
		if j.Current == timeline.KindMerged {
			entry.Outcome = RunOutcomeMerged
		}
		history.Recent = append(history.Recent, entry)
	}
	sort.Slice(history.Recent, func(i, k int) bool {
		if history.Recent[i].CompletedAt != history.Recent[k].CompletedAt {
			return history.Recent[i].CompletedAt > history.Recent[k].CompletedAt
		}
		return history.Recent[i].Key < history.Recent[k].Key
	})
	if len(history.Recent) > limit {
		history.Recent = history.Recent[:limit]
	}
	return history
}

// StatusRunHistory projects the bounded run history for the status/snapshot
// payload. It returns nil when the run registry is unavailable so the payload
// omits the field and consumers render unknown rather than an empty history.
func (s *Server) StatusRunHistory() *RunHistory {
	active, err := s.activeRuns(false)
	if err != nil {
		return nil
	}
	return runHistoryFromProjection(active, s.LifecycleTimeline().Journeys(0), SnapshotRunHistoryLimit)
}

// stageCompletionsByIdentity counts recorded stage completions per lease
// identity from the lifecycle timeline. The timeline keeps one aggregate per
// (issue, kind) with the latest agent, so repeated completions on one issue
// are credited to the identity that completed the stage most recently.
func (s *Server) stageCompletionsByIdentity() map[string]int {
	out := map[string]int{}
	if s == nil {
		return out
	}
	for _, j := range s.LifecycleTimeline().Journeys(0) {
		st := j.Stages[timeline.KindStageCompleted]
		if st == nil || st.Agent == "" || st.Count <= 0 {
			continue
		}
		out[st.Agent] += st.Count
	}
	return out
}
