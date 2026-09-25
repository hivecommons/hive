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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

const (
	DefaultSwarmName          = "Swarm"
	DefaultSwarmDuration      = 24 * time.Hour
	DefaultSwarmPrepTimeout   = 2 * time.Minute
	DefaultSwarmUnlockIdlePct = 50
	SwarmStateFileName        = "swarm-state.json"
)

const (
	swarmEnvName          = "HIVE_SWARM_NAME"
	swarmEnvDuration      = "HIVE_SWARM_DURATION"
	swarmEnvPrepTimeout   = "HIVE_SWARM_PREP_TIMEOUT"
	swarmEnvUnlockIdlePct = "HIVE_SWARM_UNLOCK_IDLE_PCT"
	swarmEnvEventName     = "HIVE_SWARM_EVENT_NAME"
	swarmEnvCallToArms    = "HIVE_SWARM_CALL_TO_ARMS"
	swarmEnvThemesJSON    = "HIVE_SWARM_THEMES_JSON"
)

type SwarmScore struct {
	IssuesClosed        int      `json:"issues_closed"`
	PRsMerged           int      `json:"prs_merged"`
	SpeksCompleted      int      `json:"speks_completed,omitempty"`
	LocalModelPRs       int      `json:"local_model_prs,omitempty"`
	ObjectivesCompleted int      `json:"objectives_completed,omitempty"`
	Participants        []string `json:"participants,omitempty"`
}

func (s SwarmScore) Total() int {
	return s.IssuesClosed + s.PRsMerged + s.SpeksCompleted*3 + s.LocalModelPRs*2 + s.ObjectivesCompleted*2
}

type SwarmRecord struct {
	Repo         string           `json:"repo"`
	DisplayName  string           `json:"display_name"`
	Start        time.Time        `json:"start"`
	End          time.Time        `json:"end"`
	EndedAt      *time.Time       `json:"ended_at,omitempty"`
	Score        SwarmScore       `json:"score"`
	Participants []string         `json:"participants,omitempty"`
	Theme        SwarmTheme       `json:"theme,omitempty"`
	Objectives   []SwarmObjective `json:"objectives,omitempty"`
	Prep         *SwarmPrep       `json:"prep,omitempty"`
	PrepAgents   []string         `json:"prep_agents,omitempty"`
	PrepErrors   []string         `json:"prep_errors,omitempty"`
	EndReason    string           `json:"end_reason,omitempty"`
	ScoringError string           `json:"scoring_error,omitempty"`
}

type SwarmTheme struct {
	EventName        string `json:"event_name"`
	CallToArms       string `json:"call_to_arms"`
	LeaderboardTitle string `json:"leaderboard_title"`
}

type SwarmObjective struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Completed   bool   `json:"completed"`
}

type SwarmPrep struct {
	Agents              []string  `json:"agents,omitempty"`
	Errors              []string  `json:"errors,omitempty"`
	ActionableIssues    int       `json:"actionable_issues"`
	UnlabeledIssues     int       `json:"unlabeled_issues"`
	DuplicateCandidates int       `json:"duplicate_candidates"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at"`
}

type SwarmStatus struct {
	Name          string       `json:"name"`
	Theme         SwarmTheme   `json:"theme"`
	Duration      string       `json:"duration"`
	Active        *SwarmRecord `json:"active,omitempty"`
	ExpiresIn     string       `json:"expires_in,omitempty"`
	IdlePct       int          `json:"idle_pct"`
	UnlockIdlePct int          `json:"unlock_idle_pct"`
	Unlocked      bool         `json:"unlocked"`
}

type SwarmHistoryResponse struct {
	Name        string        `json:"name"`
	Theme       SwarmTheme    `json:"theme"`
	History     []SwarmRecord `json:"history"`
	Leaderboard []SwarmLeader `json:"leaderboard"`
	TopPlayers  []SwarmPlayer `json:"top_players,omitempty"`
}

type SwarmLeader struct {
	Rank   int    `json:"rank"`
	Repo   string `json:"repo"`
	Score  int    `json:"score"`
	Swarms int    `json:"swarms"`
}

type swarmState struct {
	Active  *SwarmRecord            `json:"active,omitempty"`
	History []SwarmRecord           `json:"history,omitempty"`
	Players map[string]*SwarmPlayer `json:"players,omitempty"`
	Themes  map[string]SwarmTheme   `json:"themes,omitempty"`
}

type swarmScorer interface {
	ScoreSwarm(ctx context.Context, repo string, start, end time.Time) (ghpkg.SwarmScore, error)
}

type SwarmAnnouncer func(msg string) error

type swarmStore struct {
	mu                   sync.Mutex
	path                 string
	now                  func() time.Time
	duration             time.Duration
	name                 string
	defaultTheme         SwarmTheme
	configuredThemes     map[string]SwarmTheme
	state                swarmState
	loaded               bool
	scorer               swarmScorer
	expiredAnnouncements []SwarmRecord
}

func newSwarmStore(path string, scorer swarmScorer) *swarmStore {
	return newSwarmStoreWithConfig(path, scorer, nil)
}

func newSwarmStoreWithConfig(path string, scorer swarmScorer, cfg *config.Config) *swarmStore {
	return &swarmStore{
		path:             path,
		now:              time.Now,
		duration:         configuredSwarmDuration(),
		name:             configuredSwarmName(),
		defaultTheme:     defaultSwarmTheme(cfg),
		configuredThemes: configuredSwarmThemes(cfg),
		scorer:           scorer,
	}
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

func configuredSwarmPrepTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv(swarmEnvPrepTimeout)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return DefaultSwarmPrepTimeout
}

func defaultSwarmTheme(cfg *config.Config) SwarmTheme {
	theme := SwarmTheme{EventName: configuredSwarmName(), CallToArms: "All hands on deck!", LeaderboardTitle: "Swarm leaderboard"}
	if cfg != nil {
		mergeSwarmTheme(&theme, SwarmTheme{
			EventName:        cfg.Swarm.EventName,
			CallToArms:       cfg.Swarm.CallToArms,
			LeaderboardTitle: cfg.Swarm.LeaderboardTitle,
		})
	}
	if v := strings.TrimSpace(os.Getenv(swarmEnvEventName)); v != "" {
		theme.EventName = v
	}
	if v := strings.TrimSpace(os.Getenv(swarmEnvCallToArms)); v != "" {
		theme.CallToArms = v
	}
	return theme
}

func mergeSwarmTheme(dst *SwarmTheme, src SwarmTheme) {
	if strings.TrimSpace(src.EventName) != "" {
		dst.EventName = src.EventName
	}
	if strings.TrimSpace(src.CallToArms) != "" {
		dst.CallToArms = src.CallToArms
	}
	if strings.TrimSpace(src.LeaderboardTitle) != "" {
		dst.LeaderboardTitle = src.LeaderboardTitle
	}
}

func defaultSwarmObjectives() []SwarmObjective {
	return []SwarmObjective{
		{ID: "spec-written", Title: "Spec written", Description: "Capture the problem and acceptance criteria before implementation."},
		{ID: "plan-approved", Title: "Plan approved", Description: "Agree on a modest implementation plan."},
		{ID: "review-done", Title: "Review done", Description: "Complete a human or hive review pass."},
		{ID: "docs-updated", Title: "Docs updated", Description: "Update user-facing or operator documentation."},
	}
}

func completedSwarmObjectives(objectives []SwarmObjective) int {
	n := 0
	for _, obj := range objectives {
		if obj.Completed {
			n++
		}
	}
	return n
}

func (st *swarmStore) themeForRepoLocked(repo string) SwarmTheme {
	theme := st.defaultTheme
	if strings.TrimSpace(theme.EventName) == "" {
		theme = defaultSwarmTheme(nil)
	}
	if st.state.Themes == nil {
		st.state.Themes = copySwarmThemes(st.configuredThemes)
	}
	if custom, ok := st.state.Themes[strings.ToLower(strings.TrimSpace(repo))]; ok {
		mergeSwarmTheme(&theme, custom)
	}
	return theme
}

func configuredSwarmThemes(cfg *config.Config) map[string]SwarmTheme {
	normalized := map[string]SwarmTheme{}
	if cfg != nil {
		for repo, theme := range cfg.Swarm.Themes {
			if key := strings.ToLower(strings.TrimSpace(repo)); key != "" {
				normalized[key] = SwarmTheme{
					EventName:        theme.EventName,
					CallToArms:       theme.CallToArms,
					LeaderboardTitle: theme.LeaderboardTitle,
				}
			}
		}
	}
	raw := strings.TrimSpace(os.Getenv(swarmEnvThemesJSON))
	if raw == "" {
		return normalizedOrNil(normalized)
	}
	var themes map[string]SwarmTheme
	if err := json.Unmarshal([]byte(raw), &themes); err != nil {
		return normalizedOrNil(normalized)
	}
	for repo, theme := range themes {
		if key := strings.ToLower(strings.TrimSpace(repo)); key != "" {
			normalized[key] = theme
		}
	}
	return normalizedOrNil(normalized)
}

func normalizedOrNil(themes map[string]SwarmTheme) map[string]SwarmTheme {
	if len(themes) == 0 {
		return nil
	}
	return themes
}

func copySwarmThemes(themes map[string]SwarmTheme) map[string]SwarmTheme {
	if len(themes) == 0 {
		return nil
	}
	out := make(map[string]SwarmTheme, len(themes))
	for repo, theme := range themes {
		out[repo] = theme
	}
	return out
}

func configuredSwarmUnlockIdlePct() int {
	if v := strings.TrimSpace(os.Getenv(swarmEnvUnlockIdlePct)); v != "" {
		pct, err := strconv.Atoi(v)
		if err == nil && pct >= 0 {
			if pct > 100 {
				return 100
			}
			return pct
		}
	}
	return DefaultSwarmUnlockIdlePct
}

func (s *Server) registerSwarmRoutes() {
	s.mux.HandleFunc("GET /api/swarm", s.handleSwarmGet)
	s.mux.HandleFunc("POST /api/swarm", s.handleSwarmPost)
	s.mux.HandleFunc("DELETE /api/swarm", s.handleSwarmDelete)
	s.mux.HandleFunc("GET /api/swarm/history", s.handleSwarmHistory)
	s.mux.HandleFunc("GET /api/swarm/players", s.handleSwarmPlayers)
	s.mux.HandleFunc("GET /api/swarm/themes", s.handleSwarmThemesGet)
	s.mux.HandleFunc("PUT /api/swarm/themes", s.handleSwarmThemesPut)
	s.mux.HandleFunc("PATCH /api/swarm/objectives", s.handleSwarmObjectivesPatch)
	s.mux.HandleFunc("GET /api/leaderboard/swarm", s.handleSwarmPublicLeaderboard)
}

func (s *Server) swarmStore() *swarmStore {
	if s == nil {
		return nil
	}
	s.swarmMu.Lock()
	defer s.swarmMu.Unlock()
	if s.swarm == nil {
		var cfg *config.Config
		if s.deps != nil {
			cfg = s.deps.Config
		}
		s.swarm = newSwarmStoreWithConfig(filepath.Join(s.dataRootOrDefault(), SwarmStateFileName), s.depsGHClient(), cfg)
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
		status := SwarmStatus{Name: DefaultSwarmName, Theme: defaultSwarmTheme(nil), Duration: DefaultSwarmDuration.String()}
		return s.withSwarmUnlockStatus(status, false)
	}
	status, _ := store.status(context.Background())
	hasHistory, _ := store.hasHistory(context.Background())
	s.announceExpiredSwarms(store.drainExpiredAnnouncements())
	return s.withSwarmUnlockStatus(status, hasHistory)
}

func (s *Server) ActiveSwarmRepo() string {
	store := s.swarmStore()
	if store == nil {
		return ""
	}
	status, _ := store.status(context.Background())
	s.announceExpiredSwarms(store.drainExpiredAnnouncements())
	if status.Active == nil {
		return ""
	}
	return status.Active.Repo
}

func (s *Server) SetSwarmAnnouncer(fn SwarmAnnouncer) {
	if s == nil {
		return
	}
	s.swarmMu.Lock()
	defer s.swarmMu.Unlock()
	s.swarmAnnouncer = fn
}

func (st *swarmStore) status(ctx context.Context) (SwarmStatus, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return SwarmStatus{Name: st.name, Theme: st.defaultTheme, Duration: st.duration.String()}, err
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
	return SwarmStatus{Name: st.name, Theme: st.defaultTheme, Duration: st.duration.String(), Active: active, ExpiresIn: expiresIn}, nil
}

func (st *swarmStore) hasHistory(ctx context.Context) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return false, err
	}
	if err := st.expireLocked(ctx); err != nil {
		return false, err
	}
	return len(st.state.History) > 0, nil
}

func (st *swarmStore) drainExpiredAnnouncements() []SwarmRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := append([]SwarmRecord(nil), st.expiredAnnouncements...)
	st.expiredAnnouncements = nil
	return out
}

func (st *swarmStore) history(ctx context.Context) (SwarmHistoryResponse, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return SwarmHistoryResponse{Name: st.name, Theme: st.defaultTheme}, err
	}
	_ = st.expireLocked(ctx)
	history := append([]SwarmRecord(nil), st.state.History...)
	sort.Slice(history, func(i, j int) bool { return history[i].Start.After(history[j].Start) })
	players := sortedSwarmPlayers(st.state.Players)
	if len(players) > 10 {
		players = players[:10]
	}
	return SwarmHistoryResponse{Name: st.name, Theme: st.defaultTheme, History: history, Leaderboard: swarmLeaderboard(history), TopPlayers: players}, nil
}

func (st *swarmStore) players(ctx context.Context) ([]SwarmPlayer, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return nil, err
	}
	if err := st.expireLocked(ctx); err != nil {
		return nil, err
	}
	return sortedSwarmPlayers(st.state.Players), nil
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
	theme := st.themeForRepoLocked(repo)
	rec := SwarmRecord{Repo: repo, DisplayName: theme.EventName, Theme: theme, Objectives: defaultSwarmObjectives(), Start: now, End: now.Add(st.duration)}
	if prep != nil {
		rec.PrepAgents, rec.PrepErrors = prep(repo)
	}
	st.state.Active = &rec
	return rec, st.saveLocked()
}

func (st *swarmStore) setPrep(start time.Time, prep SwarmPrep) (SwarmRecord, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.loadLocked(); err != nil {
		return SwarmRecord{}, err
	}
	if st.state.Active == nil || !st.state.Active.Start.Equal(start) {
		return SwarmRecord{}, os.ErrNotExist
	}
	st.state.Active.Prep = &prep
	st.state.Active.PrepAgents = append([]string(nil), prep.Agents...)
	st.state.Active.PrepErrors = append([]string(nil), prep.Errors...)
	rec := *st.state.Active
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
	rec, err := st.finishActiveLocked(ctx, "expired")
	if err == nil {
		st.expiredAnnouncements = append(st.expiredAnnouncements, rec)
	}
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
	rec.Score.ObjectivesCompleted = completedSwarmObjectives(rec.Objectives)
	if st.scorer != nil {
		score, err := st.scorer.ScoreSwarm(ctx, rec.Repo, rec.Start, ended)
		if err != nil {
			rec.ScoringError = err.Error()
		} else {
			rec.Score = SwarmScore{IssuesClosed: score.IssuesClosed, PRsMerged: score.PRsMerged, SpeksCompleted: score.SpeksCompleted, LocalModelPRs: score.LocalModelPRs, ObjectivesCompleted: rec.Score.ObjectivesCompleted, Participants: score.Participants}
			rec.Participants = append([]string(nil), score.Participants...)
			st.updatePlayersLocked(rec, score)
		}
	}
	st.state.History = append(st.state.History, rec)
	st.state.Active = nil
	return rec, st.saveLocked()
}

func (st *swarmStore) updatePlayersLocked(rec SwarmRecord, score ghpkg.SwarmScore) {
	if st.state.Players == nil {
		st.state.Players = map[string]*SwarmPlayer{}
	}
	maxPRs := 0
	for _, n := range score.PRsByAuthor {
		if n > maxPRs {
			maxPRs = n
		}
	}
	seen := map[string]bool{}
	for _, login := range score.Participants {
		login = strings.TrimSpace(login)
		if login != "" {
			seen[login] = true
		}
	}
	for login := range score.PRsByAuthor {
		if strings.TrimSpace(login) != "" {
			seen[login] = true
		}
	}
	for login := range score.IssuesClosedBy {
		if strings.TrimSpace(login) != "" {
			seen[login] = true
		}
	}
	for login := range seen {
		p := st.state.Players[login]
		if p == nil {
			p = &SwarmPlayer{Login: login, FirstSwarm: rec.Start}
			st.state.Players[login] = p
		}
		if p.FirstSwarm.IsZero() || rec.Start.Before(p.FirstSwarm) {
			p.FirstSwarm = rec.Start
		}
		p.LastSwarm = rec.Start
		p.Swarms++
		p.currentSwarmPRs = score.PRsByAuthor[login]
		p.currentSwarmIssues = score.IssuesClosedBy[login]
		p.currentSwarmSpeks = score.SpeksCompletedBy[login]
		p.currentSwarmLocalPRs = score.LocalModelPRsBy[login]
		p.currentSwarmObjectives = completedSwarmObjectives(rec.Objectives)
		p.PRsMerged += p.currentSwarmPRs
		p.IssuesClosed += p.currentSwarmIssues
		p.SpeksCompleted += p.currentSwarmSpeks
		p.LocalModelPRs += p.currentSwarmLocalPRs
		p.ObjectivesCompleted += p.currentSwarmObjectives
		rank := 0
		if maxPRs > 0 && p.currentSwarmPRs == maxPRs {
			rank = 1
		}
		p.Achievements = append(p.Achievements, awardSwarmAchievements(p, rec, rank)...)
	}
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
	store := s.swarmStore()
	status, err := store.status(r.Context())
	if err != nil {
		jsonError(w, "swarm state unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	hasHistory, err := store.hasHistory(r.Context())
	if err != nil {
		jsonError(w, "swarm state unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.announceExpiredSwarms(store.drainExpiredAnnouncements())
	jsonResponse(w, s.withSwarmUnlockStatus(status, hasHistory))
}

func (s *Server) handleSwarmHistory(w http.ResponseWriter, r *http.Request) {
	store := s.swarmStore()
	history, err := store.history(r.Context())
	if err != nil {
		jsonError(w, "swarm history unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.announceExpiredSwarms(store.drainExpiredAnnouncements())
	jsonResponse(w, history)
}

func (s *Server) handleSwarmPlayers(w http.ResponseWriter, r *http.Request) {
	players, err := s.swarmStore().players(r.Context())
	if err != nil {
		jsonError(w, "swarm players unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, swarmPlayersResponse{Players: players, AchievementsCatalog: swarmAchievementCatalog})
}

func (s *Server) handleSwarmThemesGet(w http.ResponseWriter, r *http.Request) {
	store := s.swarmStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.loadLocked(); err != nil {
		jsonError(w, "swarm themes unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if store.state.Themes == nil {
		store.state.Themes = copySwarmThemes(store.configuredThemes)
	}
	jsonResponse(w, map[string]any{"default": store.defaultTheme, "repos": store.state.Themes})
}

func (s *Server) handleSwarmThemesPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		Repo  string     `json:"repo"`
		Theme SwarmTheme `json:"theme"`
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
	store := s.swarmStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.loadLocked(); err != nil {
		jsonError(w, "swarm themes unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if store.state.Themes == nil {
		store.state.Themes = copySwarmThemes(store.configuredThemes)
	}
	if store.state.Themes == nil {
		store.state.Themes = map[string]SwarmTheme{}
	}
	store.state.Themes[strings.ToLower(repo)] = body.Theme
	if err := store.saveLocked(); err != nil {
		jsonError(w, "saving swarm theme failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]any{"repo": repo, "theme": store.themeForRepoLocked(repo)})
}

func (s *Server) handleSwarmObjectivesPatch(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		Objectives []SwarmObjective `json:"objectives"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	store := s.swarmStore()
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.loadLocked(); err != nil {
		jsonError(w, "swarm state unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if store.state.Active == nil {
		jsonError(w, "no active swarm", http.StatusNotFound)
		return
	}
	byID := map[string]SwarmObjective{}
	for _, obj := range body.Objectives {
		if id := strings.TrimSpace(obj.ID); id != "" {
			obj.ID = id
			byID[id] = obj
		}
	}
	for i, obj := range store.state.Active.Objectives {
		if incoming, ok := byID[obj.ID]; ok {
			store.state.Active.Objectives[i].Completed = incoming.Completed
		}
	}
	if err := store.saveLocked(); err != nil {
		jsonError(w, "saving swarm objectives failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, store.state.Active)
}

func (s *Server) handleSwarmPublicLeaderboard(w http.ResponseWriter, r *http.Request) {
	history, err := s.swarmStore().history(r.Context())
	if err != nil {
		jsonError(w, "swarm leaderboard unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, history)
}

func (s *Server) handleSwarmPost(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	var body struct {
		Repo  string `json:"repo"`
		Force bool   `json:"force"`
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
	store := s.swarmStore()
	if hasHistory, err := store.hasHistory(r.Context()); err != nil {
		jsonError(w, "swarm state unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	} else if locked, pct, threshold := s.swarmStartLocked(hasHistory); locked && !body.Force {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":           fmt.Sprintf("swarm locked: hive is %d%% idle, needs %d%%", pct, threshold),
			"idle_pct":        pct,
			"unlock_idle_pct": threshold,
		})
		return
	}
	rec, err := store.start(repo, nil)
	if errors.Is(err, errSwarmActive) {
		jsonError(w, "another swarm is already active", http.StatusConflict)
		return
	}
	if err != nil {
		jsonError(w, "starting swarm failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	prep := s.prepareSwarmRepo(r.Context(), repo)
	if updated, err := store.setPrep(rec.Start, prep); err == nil {
		rec = updated
	} else if s.logger != nil {
		s.logger.Warn("persisting swarm prep failed", "repo", repo, "error", err)
	}
	action := "swarm_start"
	if body.Force {
		action = "swarm_start_forced"
	}
	s.auditFromRequest(r, action, auditDetail("repo", rec.Repo, "ends_at", rec.End.Format(time.RFC3339)), "")
	s.announceSwarmStart(rec)
	jsonResponse(w, rec)
}

func (s *Server) withSwarmUnlockStatus(status SwarmStatus, hasHistory bool) SwarmStatus {
	pct, _, _ := s.swarmIdlePercent()
	threshold := configuredSwarmUnlockIdlePct()
	status.IdlePct = pct
	status.UnlockIdlePct = threshold
	status.Unlocked = threshold == 0 || !hasHistory || pct >= threshold
	return status
}

func (s *Server) swarmStartLocked(hasHistory bool) (bool, int, int) {
	pct, _, _ := s.swarmIdlePercent()
	threshold := configuredSwarmUnlockIdlePct()
	return threshold > 0 && hasHistory && pct < threshold, pct, threshold
}

func (s *Server) swarmIdlePercent() (pct int, idle int, total int) {
	if s == nil {
		return 100, 0, 0
	}
	var agents []FrontendAgent
	s.statusMu.RLock()
	if s.status != nil {
		agents = append(agents, s.status.Agents...)
	}
	s.statusMu.RUnlock()

	configured := map[string]bool{}
	if s.deps != nil && s.deps.Config != nil {
		for name := range s.deps.Config.Agents {
			configured[name] = true
		}
	}
	if len(configured) == 0 {
		total = len(agents)
		for _, a := range agents {
			if !swarmAgentWorking(a) {
				idle++
			}
		}
	} else {
		total = len(configured)
		byName := map[string]FrontendAgent{}
		for _, a := range agents {
			byName[a.Name] = a
		}
		for name := range configured {
			a, ok := byName[name]
			if !ok || !swarmAgentWorking(a) {
				idle++
			}
		}
	}
	if total == 0 {
		return 100, 0, 0
	}
	return idle * 100 / total, idle, total
}

func swarmAgentWorking(a FrontendAgent) bool {
	return strings.EqualFold(strings.TrimSpace(a.Busy), "working") || strings.EqualFold(strings.TrimSpace(a.State), "working")
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
	s.announceSwarmEnd(rec)
	jsonResponse(w, rec)
}

func (s *Server) announceExpiredSwarms(records []SwarmRecord) {
	for _, rec := range records {
		s.announceSwarmEnd(rec)
	}
}

func (s *Server) announceSwarmStart(rec SwarmRecord) {
	prep := "none"
	if len(rec.PrepAgents) > 0 {
		prep = strings.Join(rec.PrepAgents, ", ")
	}
	call := strings.TrimSpace(rec.Theme.CallToArms)
	if call == "" {
		call = "All hands on deck!"
	}
	s.announceSwarm(fmt.Sprintf("🐝 **%s started** %s for `%s` — ends %s. Prep: %s.", rec.DisplayName, call, rec.Repo, rec.End.Format(time.RFC1123), prep))
}

func (s *Server) announceSwarmEnd(rec SwarmRecord) {
	reason := rec.EndReason
	if reason == "" {
		reason = "ended"
	}
	participants := "none"
	if len(rec.Participants) > 0 {
		participants = strings.Join(rec.Participants, ", ")
	}
	s.announceSwarm(fmt.Sprintf("🏁 **%s ended** for `%s` (%s) — %d issues closed, %d PRs merged. Participants: %s", rec.DisplayName, rec.Repo, reason, rec.Score.IssuesClosed, rec.Score.PRsMerged, participants))
}

func (s *Server) announceSwarm(msg string) {
	if s == nil {
		return
	}
	s.swarmMu.Lock()
	fn := s.swarmAnnouncer
	s.swarmMu.Unlock()
	if fn == nil {
		return
	}
	go func() {
		if err := fn(msg); err != nil && s.logger != nil {
			s.logger.Warn("swarm discord announcement failed", "error", err)
		}
	}()
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

func (s *Server) prepareSwarmRepo(parent context.Context, repo string) SwarmPrep {
	started := time.Now().UTC()
	ctx, cancel := context.WithTimeout(parent, configuredSwarmPrepTimeout())
	defer cancel()
	prep := SwarmPrep{StartedAt: started}
	gh := s.depsGHClient()
	if gh == nil {
		prep.Errors = append(prep.Errors, "github prep skipped: no GitHub client")
	} else {
		if actionable, err := gh.EnumerateActionable(ctx); err != nil {
			prep.Errors = append(prep.Errors, "actionable issues: "+err.Error())
		} else if actionable != nil {
			for _, issue := range actionable.Issues.Items {
				if strings.EqualFold(strings.TrimSpace(issue.Repo), repo) || strings.EqualFold(strings.TrimSpace(issue.Repo), repo[strings.LastIndex(repo, "/")+1:]) {
					prep.ActionableIssues++
				}
			}
		}
		if n, err := gh.CountUnlabeledOpenIssues(ctx, repo); err != nil {
			prep.Errors = append(prep.Errors, "unlabeled issues: "+err.Error())
		} else {
			prep.UnlabeledIssues = n
		}
		if result, err := gh.SweepDuplicatePRs(ctx, ghpkg.DuplicateSweepOptions{PostComments: false}); err != nil {
			prep.Errors = append(prep.Errors, "duplicate sweep: "+err.Error())
		} else if result != nil {
			prep.DuplicateCandidates = len(result.Clusters)
		}
	}
	agents, errs := s.kickSwarmPrep(repo)
	prep.Agents = agents
	prep.Errors = append(prep.Errors, errs...)
	prep.FinishedAt = time.Now().UTC()
	return prep
}
