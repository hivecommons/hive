package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

const (
	DefaultSwarmName     = "Swarm"
	DefaultSwarmDuration = 24 * time.Hour
	SwarmStateFileName   = "swarm-state.json"
)

const (
	swarmEnvName     = "HIVE_SWARM_NAME"
	swarmEnvDuration = "HIVE_SWARM_DURATION"
)

type SwarmScore struct {
	IssuesClosed int      `json:"issues_closed"`
	PRsMerged    int      `json:"prs_merged"`
	Participants []string `json:"participants,omitempty"`
}

func (s SwarmScore) Total() int { return s.IssuesClosed + s.PRsMerged }

type SwarmRecord struct {
	Repo         string     `json:"repo"`
	DisplayName  string     `json:"display_name"`
	Start        time.Time  `json:"start"`
	End          time.Time  `json:"end"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	Score        SwarmScore `json:"score"`
	Participants []string   `json:"participants,omitempty"`
	PrepAgents   []string   `json:"prep_agents,omitempty"`
	PrepErrors   []string   `json:"prep_errors,omitempty"`
	EndReason    string     `json:"end_reason,omitempty"`
	ScoringError string     `json:"scoring_error,omitempty"`
}

type SwarmStatus struct {
	Name      string       `json:"name"`
	Duration  string       `json:"duration"`
	Active    *SwarmRecord `json:"active,omitempty"`
	ExpiresIn string       `json:"expires_in,omitempty"`
}

type SwarmHistoryResponse struct {
	Name        string        `json:"name"`
	History     []SwarmRecord `json:"history"`
	Leaderboard []SwarmLeader `json:"leaderboard"`
}

type SwarmLeader struct {
	Rank   int    `json:"rank"`
	Repo   string `json:"repo"`
	Score  int    `json:"score"`
	Swarms int    `json:"swarms"`
}

type swarmState struct {
	Active  *SwarmRecord  `json:"active,omitempty"`
	History []SwarmRecord `json:"history,omitempty"`
}

type swarmScorer interface {
	ScoreSwarm(ctx context.Context, repo string, start, end time.Time) (ghpkg.SwarmScore, error)
}

type swarmStore struct {
	mu       sync.Mutex
	path     string
	now      func() time.Time
	duration time.Duration
	name     string
	state    swarmState
	loaded   bool
	scorer   swarmScorer
}

func newSwarmStore(path string, scorer swarmScorer) *swarmStore {
	return &swarmStore{path: path, now: time.Now, duration: configuredSwarmDuration(), name: configuredSwarmName(), scorer: scorer}
}

func configuredSwarmName() string {
	if v := strings.TrimSpace(os.Getenv(swarmEnvName)); v != "" {
		return v
	}
	return DefaultSwarmName
}

func configuredSwarmDuration() time.Duration {
	if v := strings.TrimSpace(os.Getenv(swarmEnvDuration)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultSwarmDuration
}

func (s *Server) registerSwarmRoutes() {
	s.mux.HandleFunc("GET /api/swarm", s.handleSwarmGet)
	s.mux.HandleFunc("POST /api/swarm", s.handleSwarmPost)
	s.mux.HandleFunc("DELETE /api/swarm", s.handleSwarmDelete)
	s.mux.HandleFunc("GET /api/swarm/history", s.handleSwarmHistory)
}

func (s *Server) swarmStore() *swarmStore {
	if s == nil {
		return nil
	}
	s.swarmMu.Lock()
	defer s.swarmMu.Unlock()
	if s.swarm == nil {
		s.swarm = newSwarmStore(filepath.Join(s.dataRootOrDefault(), SwarmStateFileName), s.depsGHClient())
	}
	return s.swarm
}

func (s *Server) depsGHClient() *ghpkg.Client {
	if s != nil && s.deps != nil {
		return s.deps.GHClient
	}
	return nil
}

func (s *Server) dataRootOrDefault() string {
	if s != nil && s.contributorsDir != "" {
		return dataRootFromDir(s.contributorsDir)
	}
	if s != nil && s.deps != nil {
		return dataRootFromDir(contributorsDirFromConfig(s.deps.Config))
	}
	return dataRootFromDir(defaultContributorsDir)
}

func (s *Server) SwarmSnapshot() SwarmStatus {
	store := s.swarmStore()
	if store == nil {
		return SwarmStatus{Name: DefaultSwarmName, Duration: DefaultSwarmDuration.String()}
	}
	status, _ := store.status(context.Background())
	return status
}

func (s *Server) ActiveSwarmRepo() string {
	store := s.swarmStore()
	if store == nil {
		return ""
	}
	status, _ := store.status(context.Background())
	if status.Active == nil {
		return ""
	}
	return status.Active.Repo
}

func (st *swarmStore) status(ctx context.Context) (SwarmStatus, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return SwarmStatus{Name: st.name, Duration: st.duration.String()}, err
	}
	_ = st.expireLocked(ctx)
	var active *SwarmRecord
	var expiresIn string
	if st.state.Active != nil {
		copy := *st.state.Active
		active = &copy
		if remaining := copy.End.Sub(st.now()); remaining > 0 {
			expiresIn = remaining.Round(time.Second).String()
		}
	}
	return SwarmStatus{Name: st.name, Duration: st.duration.String(), Active: active, ExpiresIn: expiresIn}, nil
}

func (st *swarmStore) history(ctx context.Context) (SwarmHistoryResponse, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return SwarmHistoryResponse{Name: st.name}, err
	}
	_ = st.expireLocked(ctx)
	history := append([]SwarmRecord(nil), st.state.History...)
	sort.Slice(history, func(i, j int) bool { return history[i].Start.After(history[j].Start) })
	return SwarmHistoryResponse{Name: st.name, History: history, Leaderboard: swarmLeaderboard(history)}, nil
}

func swarmLeaderboard(history []SwarmRecord) []SwarmLeader {
	byRepo := map[string]*SwarmLeader{}
	for _, h := range history {
		entry := byRepo[h.Repo]
		if entry == nil {
			entry = &SwarmLeader{Repo: h.Repo}
			byRepo[h.Repo] = entry
		}
		entry.Score += h.Score.Total()
		entry.Swarms++
	}
	leaders := make([]SwarmLeader, 0, len(byRepo))
	for _, v := range byRepo {
		leaders = append(leaders, *v)
	}
	sort.Slice(leaders, func(i, j int) bool {
		if leaders[i].Score != leaders[j].Score {
			return leaders[i].Score > leaders[j].Score
		}
		return leaders[i].Repo < leaders[j].Repo
	})
	for i := range leaders {
		leaders[i].Rank = i + 1
	}
	return leaders
}

func (st *swarmStore) start(repo string, prep func(string) ([]string, []string)) (SwarmRecord, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return SwarmRecord{}, err
	}
	if err := st.expireLocked(context.Background()); err != nil {
		return SwarmRecord{}, err
	}
	if st.state.Active != nil {
		return SwarmRecord{}, errSwarmActive
	}
	now := st.now().UTC()
	rec := SwarmRecord{Repo: repo, DisplayName: st.name, Start: now, End: now.Add(st.duration)}
	if prep != nil {
		rec.PrepAgents, rec.PrepErrors = prep(repo)
	}
	st.state.Active = &rec
	return rec, st.saveLocked()
}

func (st *swarmStore) end(ctx context.Context, reason string) (SwarmRecord, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return SwarmRecord{}, err
	}
	if st.state.Active == nil {
		return SwarmRecord{}, os.ErrNotExist
	}
	if !st.now().Before(st.state.Active.End) {
		reason = "expired"
	}
	return st.finishActiveLocked(ctx, reason)
}

func (st *swarmStore) expireLocked(ctx context.Context) error {
	if st.state.Active == nil || st.now().Before(st.state.Active.End) {
		return nil
	}
	_, err := st.finishActiveLocked(ctx, "expired")
	return err
}

func (st *swarmStore) finishActiveLocked(ctx context.Context, reason string) (SwarmRecord, error) {
	rec := *st.state.Active
	now := st.now().UTC()
	ended := now
	if rec.End.Before(now) {
		ended = rec.End
	}
	rec.EndedAt = &ended
	rec.EndReason = reason
	if st.scorer != nil {
		score, err := st.scorer.ScoreSwarm(ctx, rec.Repo, rec.Start, ended)
		if err != nil {
			rec.ScoringError = err.Error()
		} else {
			rec.Score = SwarmScore{IssuesClosed: score.IssuesClosed, PRsMerged: score.PRsMerged, Participants: score.Participants}
			rec.Participants = append([]string(nil), score.Participants...)
		}
	}
	st.state.History = append(st.state.History, rec)
	st.state.Active = nil
	return rec, st.saveLocked()
}

var errSwarmActive = errors.New("another swarm is already active")

func (st *swarmStore) loadLocked() error {
	if st.loaded {
		return nil
	}
	st.loaded = true
	data, err := os.ReadFile(st.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &st.state)
}

func (st *swarmStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(st.path, append(data, '\n'), 0o644)
}

func (s *Server) handleSwarmGet(w http.ResponseWriter, r *http.Request) {
	status, err := s.swarmStore().status(r.Context())
	if err != nil {
		jsonError(w, "swarm state unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, status)
}

func (s *Server) handleSwarmHistory(w http.ResponseWriter, r *http.Request) {
	history, err := s.swarmStore().history(r.Context())
	if err != nil {
		jsonError(w, "swarm history unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, history)
}

func (s *Server) handleSwarmPost(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		Repo string `json:"repo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	repo, err := s.normalizeSwarmRepo(body.Repo)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	rec, err := s.swarmStore().start(repo, s.kickSwarmPrep)
	if errors.Is(err, errSwarmActive) {
		jsonError(w, "another swarm is already active", http.StatusConflict)
		return
	}
	if err != nil {
		jsonError(w, "starting swarm failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "swarm_start", auditDetail("repo", rec.Repo, "ends_at", rec.End.Format(time.RFC3339)), "")
	jsonResponse(w, rec)
}

func (s *Server) handleSwarmDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	rec, err := s.swarmStore().end(r.Context(), "ended")
	if errors.Is(err, os.ErrNotExist) {
		jsonError(w, "no active swarm", http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, "ending swarm failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "swarm_end", auditDetail("repo", rec.Repo, "score", fmt.Sprint(rec.Score.Total())), "")
	jsonResponse(w, rec)
}

func (s *Server) normalizeSwarmRepo(repo string) (string, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "", fmt.Errorf("repo is required")
	}
	cfg := (*config.Config)(nil)
	if s != nil && s.deps != nil {
		cfg = s.deps.Config
	}
	if cfg == nil {
		return repo, nil
	}
	want := strings.ToLower(repo)
	for _, candidate := range cfg.Project.Repos {
		full := candidate
		if !strings.Contains(full, "/") && cfg.Project.Org != "" {
			full = cfg.Project.Org + "/" + full
		}
		if strings.EqualFold(candidate, repo) || strings.EqualFold(full, repo) || strings.EqualFold(strings.TrimPrefix(full, cfg.Project.Org+"/"), repo) {
			return full, nil
		}
	}
	if cfg.Project.PrimaryRepo != "" {
		full := cfg.Project.PrimaryRepo
		if !strings.Contains(full, "/") && cfg.Project.Org != "" {
			full = cfg.Project.Org + "/" + full
		}
		if strings.EqualFold(full, repo) || strings.EqualFold(strings.TrimPrefix(full, cfg.Project.Org+"/"), repo) || strings.EqualFold(want, strings.ToLower(cfg.Project.PrimaryRepo)) {
			return full, nil
		}
	}
	return "", fmt.Errorf("repo %q is not configured for this hive", repo)
}

func (s *Server) kickSwarmPrep(repo string) ([]string, []string) {
	if s == nil || s.deps == nil || s.deps.AgentMgr == nil || s.deps.Config == nil {
		return nil, nil
	}
	message := "SWARM PREP: " + repo + " is in swarm mode for the next 24h. Prepare it for multi-agent work: refresh the issue breakdown, identify blockers, and break suitable issues into parallelizable tasks. Do not invent work; use the repo's existing issues and plans."
	var kicked []string
	var errs []string
	for _, name := range []string{"architect", "scanner"} {
		if _, ok := s.deps.Config.Agents[name]; !ok {
			continue
		}
		if err := s.deps.AgentMgr.SendKick(name, message); err != nil {
			errs = append(errs, name+": "+err.Error())
			continue
		}
		kicked = append(kicked, name)
	}
	return kicked, errs
}
