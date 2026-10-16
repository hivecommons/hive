package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/hivestate"
	"github.com/hivecommons/hive/pkg/nous"
)

// nousNow is swapped in tests.
var nousNow = time.Now

// nousCampaignView is the campaign block of GET /api/nous/status
// (hivecommons/hive#12501). Snapshots/Percent are scoped to the campaign
// window; the whole observation history is reported separately.
type nousCampaignView struct {
	ID         int               `json:"id"`
	Question   string            `json:"question"`
	Hypothesis string            `json:"hypothesis,omitempty"`
	Scope      nousCampaignScope `json:"scope"`
	Status     string            `json:"status"`
	StartedAt  string            `json:"startedAt"`
	EndedAt    string            `json:"endedAt,omitempty"`
	Snapshots  int               `json:"snapshots"`
	Target     int               `json:"target"`
	// TargetExplainer says what Target means in words (#12532).
	TargetExplainer string                   `json:"targetExplainer"`
	Percent         float64                  `json:"percent"`
	Complete        bool                     `json:"complete"`
	Experiments     int                      `json:"experiments"`
	Principles      int                      `json:"principles"`
	Results         []map[string]interface{} `json:"results,omitempty"`
	// Stage / StageLabel / StageDetail / NextAction describe where the
	// campaign is in its lifecycle and what, if anything, the operator must
	// do next (#12532). NextAction is empty when nothing is required.
	Stage       string `json:"stage"`
	StageLabel  string `json:"stageLabel"`
	StageDetail string `json:"stageDetail,omitempty"`
	NextAction  string `json:"nextAction,omitempty"`
	// ExperimentLoopIdle marks a ready card in suggest/evolve mode on a hive
	// where the external experiment loop has written nothing for
	// NousLoopIdleCycles evaluation cycles (#12541), so the UI can point at
	// the docs instead of promising proposals that will never come.
	ExperimentLoopIdle bool `json:"experimentLoopIdle,omitempty"`
}

// nousCampaignStage fills the lifecycle fields of v from the lab mode and the
// experiment rows attributed to the campaign. Precedence: archived/paused →
// running → proposed → concluded (results and nothing in flight) → baseline
// → ready. The ready stage distinguishes "lab is in observe mode, press
// Generate proposals" from "lab is in suggest/evolve mode, waiting for the
// experiment loop", because the hive process itself designs no proposals.
// loopIdle reports that the experiment loop is known not to be running
// (#12541); the waiting ready stage then says so instead of waiting forever.
func nousCampaignStage(v *nousCampaignView, mode string, rows []nousExperimentProgress, loopIdle bool) {
	if v == nil {
		return
	}
	v.NextAction = ""
	v.ExperimentLoopIdle = false
	switch v.Status {
	case nous.StatusArchived:
		v.Stage, v.StageLabel, v.StageDetail = NousStageConcluded, "concluded", "Archived; baseline, hypotheses and results are frozen read-only."
		return
	case nous.StatusPaused:
		v.Stage, v.StageLabel, v.StageDetail = NousStagePaused, "paused", "Paused; its scope is released and the baseline count stops until resumed."
		return
	}
	var running, pending, judged int
	for _, r := range rows {
		if r.CampaignID != v.ID {
			continue
		}
		switch {
		case r.Status == "pending":
			pending++
		case r.Phase == "judged":
			judged++
		case r.Status == "active" || r.Phase == "experiment" || r.Phase == "cooldown" || r.Phase == "judging":
			running++
		}
	}
	switch {
	case running > 0:
		v.Stage, v.StageLabel = NousStageRunning, "running"
		v.StageDetail = fmt.Sprintf("%d experiment(s) in flight; results land under Results when judged.", running)
	case pending > 0:
		v.Stage, v.StageLabel = NousStageProposed, "proposals awaiting your review"
		v.StageDetail = fmt.Sprintf("%d proposal(s) are pending — approve or reject them under Pending Proposal.", pending)
	case judged > 0:
		v.Stage, v.StageLabel = NousStageConcluded, "concluded"
		v.StageDetail = fmt.Sprintf("%d experiment(s) judged; archive this experiment or let the lab propose more.", judged)
	case !v.Complete:
		v.Stage, v.StageLabel = NousStageBaseline, fmt.Sprintf("collecting baseline %d / %d", v.Snapshots, v.Target)
		v.StageDetail = "Proposals are designed only once the baseline is complete (" + NousBaselineTargetExplainer + ")."
	case strings.EqualFold(mode, "observe") || mode == "":
		v.Stage, v.StageLabel = NousStageReady, "ready — generate proposals"
		v.StageDetail = "Baseline complete. The lab is in Observe mode (data collection only); Generate proposals switches it to Suggest so proposals can be designed for your review."
		v.NextAction = NousActionGenerateProposals
	case loopIdle:
		v.Stage, v.StageLabel = NousStageReady, "ready — the experiment loop is not running on this hive"
		v.StageDetail = fmt.Sprintf("Baseline complete and the lab is in %s mode, but the experiment loop has written nothing to /var/run/nous/governor in the last %d evaluation cycles. The hive designs no proposals itself; they come only from the external experiment loop — see \"The experiment loop\" in the Experiment Lab operator guide.", strings.Title(strings.ToLower(mode)), NousLoopIdleCycles)
		v.ExperimentLoopIdle = true
	default:
		v.Stage, v.StageLabel = NousStageReady, "ready — generating proposals"
		v.StageDetail = "Baseline complete and the lab is in " + strings.Title(strings.ToLower(mode)) + " mode. Proposals appear here as soon as the experiment loop designs them; nothing more is needed from you."
	}
}

// nousCampaignScope is the wire form of nous.Scope. Label is "hive-wide" or
// the repo list joined for display.
type nousCampaignScope struct {
	Repos        []string `json:"repos"`
	Intervention string   `json:"intervention,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	HiveWide     bool     `json:"hiveWide"`
}

func nousScopeToView(sc nous.Scope) nousCampaignScope {
	v := nousCampaignScope{Repos: append([]string{}, sc.Repos...), Intervention: sc.Intervention, Tags: sc.Tags, HiveWide: sc.HiveWide()}
	return v
}

func nousScopeFromView(v nousCampaignScope) nous.Scope {
	return nous.Scope{Repos: v.Repos, Intervention: v.Intervention, Tags: v.Tags}
}

// nousObservationView is the question-independent hive-state history.
type nousObservationView struct {
	Snapshots int    `json:"snapshots"`
	Since     string `json:"since,omitempty"`
}

// Campaigns returns the campaign store, creating campaign 1 on first use so
// a hive that predates per-campaign baselines keeps its history as campaign
// 1. It returns nil when no snapshot directory or campaign file is set.
func (ns *NousState) Campaigns() *nous.Store {
	if ns == nil || (ns.SnapshotDir == "" && ns.CampaignFile == "") {
		return nil
	}
	ns.campaignOnce.Do(func() {
		path := ns.CampaignFile
		if path == "" {
			path = filepath.Join(filepath.Dir(ns.SnapshotDir), nous.DefaultFileName)
		}
		store := nous.NewStore(path)
		startedAt := nousNow().UTC()
		if _, first := hivestate.Count(ns.SnapshotDir, time.Time{}); !first.IsZero() {
			startedAt = first
		}
		if _, _, err := store.Ensure("", startedAt); err != nil {
			return
		}
		ns.campaignStore = store
	})
	return ns.campaignStore
}

// ActiveCampaign returns the active campaign, if campaigns are enabled.
func (ns *NousState) ActiveCampaign() (nous.Campaign, bool) {
	store := ns.Campaigns()
	if store == nil {
		return nous.Campaign{}, false
	}
	return store.Active()
}

// CampaignSnapshots counts the snapshot files recorded since c started.
func (ns *NousState) CampaignSnapshots(c nous.Campaign) int {
	if ns == nil || ns.SnapshotDir == "" {
		return 0
	}
	n, _ := hivestate.Count(ns.SnapshotDir, c.StartedAt)
	return n
}

// Observation reports the whole hive-state history: file count and the time
// of the earliest file.
func (ns *NousState) Observation() nousObservationView {
	if ns == nil || ns.SnapshotDir == "" {
		return nousObservationView{}
	}
	n, first := hivestate.Count(ns.SnapshotDir, time.Time{})
	v := nousObservationView{Snapshots: n}
	if !first.IsZero() {
		v.Since = first.Format(time.RFC3339)
	}
	return v
}

// ApplyQuestion records a research-question change. A materially different
// question archives the active campaign (with outcome) and starts a new one;
// a first question on a campaign with no observations yet, or a whitespace-
// only edit, is written in place. It reports whether a new campaign started.
func (ns *NousState) ApplyQuestion(question string, outcome func(nous.Campaign) nous.Outcome) (bool, error) {
	store := ns.Campaigns()
	if store == nil {
		return false, nil
	}
	question = strings.TrimSpace(question)
	active, ok := store.Active()
	if !ok {
		_, _, err := store.Ensure(question, nousNow())
		return false, err
	}
	if !nous.QuestionChanged(active.Question, question) {
		if question != "" && question != active.Question {
			return false, store.SetQuestion(question)
		}
		return false, nil
	}
	if active.Question == "" && ns.CampaignSnapshots(active) == 0 {
		return false, store.SetQuestion(question)
	}
	var out nous.Outcome
	if outcome != nil {
		out = outcome(active)
	}
	_, _, err := store.Start(question, nousNow(), out)
	return err == nil, err
}

// StartCampaign explicitly archives the active campaign and starts a new one
// for question (the active question when empty).
func (ns *NousState) StartCampaign(question string, outcome func(nous.Campaign) nous.Outcome) (nous.Campaign, error) {
	store := ns.Campaigns()
	if store == nil {
		return nous.Campaign{}, nous.ErrNoActive
	}
	question = strings.TrimSpace(question)
	var out nous.Outcome
	if active, ok := store.Active(); ok {
		if question == "" {
			question = active.Question
		}
		if outcome != nil {
			out = outcome(active)
		}
	}
	_, next, err := store.Start(question, nousNow(), out)
	return next, err
}

// nousCampaignOutcome freezes what the campaign produced: its snapshot
// count, the experiments and judged results that started inside its window,
// and the principle count at archive time.
func (s *Server) nousCampaignOutcome(c nous.Campaign) nous.Outcome {
	ns := s.deps.Nous
	ns.Mu.Lock()
	status := make(map[string]interface{}, len(ns.Status))
	for k, v := range ns.Status {
		status[k] = v
	}
	config := make(map[string]interface{}, len(ns.Config))
	for k, v := range ns.Config {
		config[k] = v
	}
	ledger := append([]map[string]interface{}(nil), ns.Ledger...)
	principles := append([]NousPrinciple(nil), ns.Principles...)
	ns.Mu.Unlock()

	rows, results := s.nousExperimentProgress(status, config, ledger, principles, c.StartedAt)
	out := nous.Outcome{
		Snapshots:   ns.CampaignSnapshots(c),
		Experiments: len(rows),
		Principles:  len(principles),
	}
	for _, r := range results {
		var m map[string]interface{}
		if data, err := json.Marshal(r); err == nil && json.Unmarshal(data, &m) == nil {
			out.Results = append(out.Results, m)
		}
	}
	return out
}

func nousCampaignToView(ns *NousState, c nous.Campaign) nousCampaignView {
	v := nousCampaignView{
		ID:              c.ID,
		Question:        c.Question,
		Hypothesis:      c.Hypothesis,
		Scope:           nousScopeToView(c.Scope),
		Status:          c.Status(),
		StartedAt:       c.StartedAt.UTC().Format(time.RFC3339),
		Target:          NousBaselineTarget,
		TargetExplainer: NousBaselineTargetExplainer,
	}
	if c.Archived() {
		v.EndedAt = c.EndedAt.UTC().Format(time.RFC3339)
		v.Snapshots = c.Outcome.Snapshots
		v.Experiments = c.Outcome.Experiments
		v.Principles = c.Outcome.Principles
		v.Results = c.Outcome.Results
	} else {
		v.Snapshots = ns.CampaignSnapshots(c)
	}
	v.Percent = NousBaselinePct(v.Snapshots)
	v.Complete = v.Snapshots >= NousBaselineTarget
	return v
}

// nousCampaignStartRequest is the body of POST /api/nous/campaign/start.
type nousCampaignStartRequest struct {
	Question string `json:"question"`
}

// handleNousCampaignStart archives the active campaign and starts a new
// experiment: baseline and hypotheses restart at zero, the previous campaign
// is kept read-only with its results. Owner role required.
func (s *Server) handleNousCampaignStart(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}
	var req nousCampaignStartRequest
	if r.ContentLength != 0 {
		if err := decodeBody(r, &req); err != nil {
			jsonError(w, "invalid body", http.StatusBadRequest)
			return
		}
	}
	ns := s.deps.Nous
	if ns.Campaigns() == nil {
		jsonError(w, "campaigns not enabled: no snapshot directory", http.StatusConflict)
		return
	}
	next, err := ns.StartCampaign(req.Question, s.nousCampaignOutcome)
	if err != nil {
		jsonError(w, "start campaign: "+err.Error(), http.StatusInternalServerError)
		return
	}
	ns.Mu.Lock()
	if ns.Config == nil {
		ns.Config = make(map[string]interface{})
	}
	ns.Config["goals"] = map[string]interface{}{"repoGoal": next.Question}
	ns.Mu.Unlock()
	s.auditFromRequest(r, "nous_campaign_start", auditDetail("campaign", strconv.Itoa(next.ID)), "")
	s.refreshAndPersist()
	jsonResponse(w, map[string]interface{}{"ok": true, "status": "started", "campaign": nousCampaignToView(ns, next)})
}

// handleNousConfigGoals stores the goals section and, when the research
// question materially changed, rotates the campaign (#12501).
func (s *Server) handleNousConfigGoals(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return
	}
	var body interface{}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	ns := s.deps.Nous
	ns.Mu.Lock()
	if ns.Config == nil {
		ns.Config = make(map[string]interface{})
	}
	ns.Config["goals"] = body
	ns.Mu.Unlock()
	s.auditFromRequest(r, "nous_config_update", auditDetail("section", "goals"), "")

	started, err := ns.ApplyQuestion(nousResearchQuestion(body), s.nousCampaignOutcome)
	if err != nil {
		jsonError(w, "campaign: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resp := map[string]interface{}{"ok": true, "status": "updated", "section": "goals", "newCampaign": started}
	if c, ok := ns.ActiveCampaign(); ok {
		resp["campaign"] = nousCampaignToView(ns, c)
		if started {
			s.auditFromRequest(r, "nous_campaign_start", auditDetail("campaign", strconv.Itoa(c.ID)), "")
		}
	}
	s.refreshAndPersist()
	jsonResponse(w, resp)
}

// OpenCampaigns returns every non-archived campaign (active and paused).
func (ns *NousState) OpenCampaigns() []nous.Campaign {
	store := ns.Campaigns()
	if store == nil {
		return nil
	}
	return store.Open()
}

// ArchiveCampaign ends campaign id with the outcome computed by outcome.
func (ns *NousState) ArchiveCampaign(id int, outcome func(nous.Campaign) nous.Outcome) (nous.Campaign, error) {
	store := ns.Campaigns()
	if store == nil {
		return nous.Campaign{}, nous.ErrNoActive
	}
	c, ok := store.Get(id)
	if !ok {
		return nous.Campaign{}, nous.ErrNotFound
	}
	var out nous.Outcome
	if outcome != nil && !c.Archived() {
		out = outcome(c)
	}
	return store.Archive(id, nousNow(), out)
}

// nousCampaignRequest is the body of POST /api/nous/campaigns and the edit
// fields of PATCH /api/nous/campaigns/{id}.
type nousCampaignRequest struct {
	Question   *string            `json:"question,omitempty"`
	Hypothesis *string            `json:"hypothesis,omitempty"`
	Scope      *nousCampaignScope `json:"scope,omitempty"`
	// Action is pause, resume or archive (PATCH only). Edits in the same
	// body are applied first.
	Action string `json:"action,omitempty"`
}

// nousCampaignConflict writes the 409 for a scope collision: the conflicting
// campaign is included so the UI can offer to pause it (#12532).
func nousCampaignConflict(w http.ResponseWriter, ns *NousState, conf *nous.Conflict) {
	shared := nous.ScopeHiveWide
	if len(conf.Shared) > 0 {
		shared = strings.Join(conf.Shared, ", ")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":       false,
		"error":    "scope conflicts with running experiment #" + strconv.Itoa(conf.With.ID) + " (" + shared + "): concurrent interventions on the same scope confound each other's outcomes. Pause that experiment or narrow the scope.",
		"code":     "scope_conflict",
		"shared":   conf.Shared,
		"conflict": nousCampaignToView(ns, conf.With),
	})
}

// nousCampaignError maps store errors to HTTP statuses.
func (s *Server) nousCampaignError(w http.ResponseWriter, ns *NousState, err error) {
	var conf *nous.Conflict
	switch {
	case errors.As(err, &conf):
		nousCampaignConflict(w, ns, conf)
	case errors.Is(err, nous.ErrNotFound):
		jsonError(w, "campaign not found", http.StatusNotFound)
	case errors.Is(err, nous.ErrArchived):
		jsonError(w, "campaign is archived and read-only", http.StatusConflict)
	default:
		jsonError(w, "campaign: "+err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) nousCampaignsStore(w http.ResponseWriter) (*NousState, *nous.Store, bool) {
	if s.deps.Nous == nil {
		jsonError(w, "nous not configured", http.StatusNotFound)
		return nil, nil, false
	}
	ns := s.deps.Nous
	store := ns.Campaigns()
	if store == nil {
		jsonError(w, "campaigns not enabled: no snapshot directory", http.StatusConflict)
		return nil, nil, false
	}
	return ns, store, true
}

// nousCampaignViews renders every campaign, live-counting open ones.
func (s *Server) nousCampaignViews(ns *NousState) []nousCampaignView {
	out := []nousCampaignView{}
	store := ns.Campaigns()
	if store == nil {
		return out
	}
	for _, c := range store.All() {
		out = append(out, nousCampaignToView(ns, c))
	}
	return out
}

// handleNousCampaignsList is GET /api/nous/campaigns: every campaign in ID
// order with per-campaign baseline counts.
func (s *Server) handleNousCampaignsList(w http.ResponseWriter, r *http.Request) {
	ns, _, ok := s.nousCampaignsStore(w)
	if !ok {
		return
	}
	jsonResponse(w, map[string]interface{}{
		"ok":              true,
		"campaigns":       s.nousCampaignViews(ns),
		"target":          NousBaselineTarget,
		"targetExplainer": NousBaselineTargetExplainer,
	})
}

// handleNousCampaignsCreate is POST /api/nous/campaigns: start a new
// experiment next to the running ones. 409 scope_conflict when a running
// campaign already covers part of the scope. Owner role required.
func (s *Server) handleNousCampaignsCreate(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	ns, store, ok := s.nousCampaignsStore(w)
	if !ok {
		return
	}
	var req nousCampaignRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	question := ""
	if req.Question != nil {
		question = strings.TrimSpace(*req.Question)
	}
	if question == "" {
		jsonError(w, "question is required", http.StatusBadRequest)
		return
	}
	hypothesis := ""
	if req.Hypothesis != nil {
		hypothesis = *req.Hypothesis
	}
	var scope nous.Scope
	if req.Scope != nil {
		scope = nousScopeFromView(*req.Scope)
	}
	c, err := store.Create(question, hypothesis, scope, nousNow())
	if err != nil {
		s.nousCampaignError(w, ns, err)
		return
	}
	s.auditFromRequest(r, "nous_campaign_create", auditDetail("campaign", strconv.Itoa(c.ID)), "")
	s.nousSyncGoalsFromCampaigns(ns)
	s.refreshAndPersist()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "status": "created", "campaign": nousCampaignToView(ns, c)})
}

// handleNousCampaignPatch is PATCH /api/nous/campaigns/{id}: edit question /
// hypothesis / scope, then optionally pause, resume or archive. Owner role
// required.
func (s *Server) handleNousCampaignPatch(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	ns, store, ok := s.nousCampaignsStore(w)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		jsonError(w, "invalid campaign id", http.StatusBadRequest)
		return
	}
	var req nousCampaignRequest
	if err := decodeBody(r, &req); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	switch action {
	case "", "pause", "resume", "archive":
	default:
		jsonError(w, "unknown action: "+action, http.StatusBadRequest)
		return
	}
	var c nous.Campaign
	if req.Question != nil || req.Hypothesis != nil || req.Scope != nil {
		patch := nous.Patch{Question: req.Question, Hypothesis: req.Hypothesis}
		if req.Scope != nil {
			sc := nousScopeFromView(*req.Scope)
			patch.Scope = &sc
		}
		if c, err = store.Update(id, patch); err != nil {
			s.nousCampaignError(w, ns, err)
			return
		}
	} else if c, ok = store.Get(id); !ok {
		jsonError(w, "campaign not found", http.StatusNotFound)
		return
	}
	switch action {
	case "pause":
		c, err = store.Pause(id)
	case "resume":
		c, err = store.Resume(id)
	case "archive":
		c, err = ns.ArchiveCampaign(id, s.nousCampaignOutcome)
	}
	if err != nil {
		s.nousCampaignError(w, ns, err)
		return
	}
	s.auditFromRequest(r, "nous_campaign_update", auditDetail("campaign", strconv.Itoa(id)), action)
	s.nousSyncGoalsFromCampaigns(ns)
	s.refreshAndPersist()
	jsonResponse(w, map[string]interface{}{"ok": true, "status": "updated", "action": action, "campaign": nousCampaignToView(ns, c)})
}

// nousSyncGoalsFromCampaigns keeps the legacy single-question goals section
// pointing at the oldest running campaign so the config dialog's Research
// Question field and the experiment loop keep working.
func (s *Server) nousSyncGoalsFromCampaigns(ns *NousState) {
	active, ok := ns.ActiveCampaign()
	ns.Mu.Lock()
	defer ns.Mu.Unlock()
	if ns.Config == nil {
		ns.Config = make(map[string]interface{})
	}
	if ok {
		ns.Config["goals"] = map[string]interface{}{"repoGoal": active.Question}
	} else {
		delete(ns.Config, "goals")
	}
}

// nousAssignCampaigns tags each experiment row with the open campaign it
// belongs to: the row must start inside the campaign window and, when the
// row names a repo, the campaign's scope must cover it (hive-wide covers
// all). The oldest matching campaign wins.
func nousAssignCampaigns(rows []nousExperimentProgress, open []nous.Campaign) {
	for i := range rows {
		rows[i].CampaignID = nousCampaignFor(rows[i], open)
	}
}

func nousCampaignFor(row nousExperimentProgress, open []nous.Campaign) int {
	start := parseTime(row.StartedAt)
	for _, c := range open {
		if !start.IsZero() && start.Before(c.StartedAt) {
			continue
		}
		if row.Repo != "" && !c.Scope.HiveWide() {
			if _, ok := c.Scope.Overlaps(nous.Scope{Repos: []string{row.Repo}}); !ok {
				continue
			}
		}
		return c.ID
	}
	return 0
}
