package dashboard

import (
	"encoding/json"
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
	ID          int                      `json:"id"`
	Question    string                   `json:"question"`
	Status      string                   `json:"status"`
	StartedAt   string                   `json:"startedAt"`
	EndedAt     string                   `json:"endedAt,omitempty"`
	Snapshots   int                      `json:"snapshots"`
	Target      int                      `json:"target"`
	Percent     float64                  `json:"percent"`
	Complete    bool                     `json:"complete"`
	Experiments int                      `json:"experiments"`
	Principles  int                      `json:"principles"`
	Results     []map[string]interface{} `json:"results,omitempty"`
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
		ID:        c.ID,
		Question:  c.Question,
		Status:    c.Status(),
		StartedAt: c.StartedAt.UTC().Format(time.RFC3339),
		Target:    NousBaselineTarget,
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
