package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

type fakeSwarmScorer struct {
	score ghpkg.SwarmScore
	err   error
	calls int
}

func (f *fakeSwarmScorer) ScoreSwarm(context.Context, string, time.Time, time.Time) (ghpkg.SwarmScore, error) {
	f.calls++
	return f.score, f.err
}

func TestSwarmStoreOneAtATimeAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), SwarmStateFileName)
	store := newSwarmStore(path, nil)
	store.now = func() time.Time { return time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC) }
	store.duration = time.Hour

	first, err := store.start("acme/api", nil)
	if err != nil {
		t.Fatalf("start first: %v", err)
	}
	if first.Repo != "acme/api" || first.End.Sub(first.Start) != time.Hour {
		t.Fatalf("first swarm = %+v", first)
	}
	if _, err := store.start("acme/web", nil); !errors.Is(err, errSwarmActive) {
		t.Fatalf("second start err = %v, want active conflict", err)
	}

	reloaded := newSwarmStore(path, nil)
	reloaded.now = func() time.Time { return time.Date(2026, 9, 24, 12, 30, 0, 0, time.UTC) }
	status, err := reloaded.status(context.Background())
	if err != nil {
		t.Fatalf("reload status: %v", err)
	}
	if status.Active == nil || status.Active.Repo != "acme/api" {
		t.Fatalf("reloaded active = %+v", status.Active)
	}
}

func TestSwarmExpiryScoresAndMovesToHistory(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	scorer := &fakeSwarmScorer{score: ghpkg.SwarmScore{IssuesClosed: 2, PRsMerged: 3, Participants: []string{"alice", "bob"}}}
	store := newSwarmStore(filepath.Join(t.TempDir(), SwarmStateFileName), scorer)
	store.now = func() time.Time { return now }
	store.duration = time.Hour
	if _, err := store.start("acme/api", nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	store.now = func() time.Time { return now.Add(time.Hour + time.Minute) }
	status, err := store.status(context.Background())
	if err != nil {
		t.Fatalf("expired status: %v", err)
	}
	if status.Active != nil {
		t.Fatalf("active after expiry = %+v", status.Active)
	}
	history, err := store.history(context.Background())
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history.History) != 1 || history.History[0].Score.Total() != 5 {
		t.Fatalf("history = %+v", history.History)
	}
	if got := history.History[0].Score.Participants; len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("participants = %v", got)
	}
	if scorer.calls != 1 {
		t.Fatalf("score calls = %d, want 1", scorer.calls)
	}
}

func TestSwarmEndAfterExpiryCapsScoringWindow(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	scorer := &windowCaptureScorer{}
	store := newSwarmStore(filepath.Join(t.TempDir(), SwarmStateFileName), scorer)
	store.now = func() time.Time { return now }
	store.duration = time.Hour
	if _, err := store.start("acme/api", nil); err != nil {
		t.Fatalf("start: %v", err)
	}
	store.now = func() time.Time { return now.Add(2 * time.Hour) }
	rec, err := store.end(context.Background(), "ended")
	if err != nil {
		t.Fatalf("end: %v", err)
	}
	if rec.EndReason != "expired" {
		t.Fatalf("reason = %q, want expired", rec.EndReason)
	}
	if !scorer.end.Equal(now.Add(time.Hour)) {
		t.Fatalf("scoring end = %s, want %s", scorer.end, now.Add(time.Hour))
	}
}

type windowCaptureScorer struct {
	start time.Time
	end   time.Time
}

func (w *windowCaptureScorer) ScoreSwarm(_ context.Context, _ string, start, end time.Time) (ghpkg.SwarmScore, error) {
	w.start = start
	w.end = end
	return ghpkg.SwarmScore{}, nil
}

func TestSwarmAPIConflictAndHistory(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.Org = "acme"
	deps.Config.Project.Repos = []string{"api", "web"}
	s.SetContributorsDir(t.TempDir())
	s.swarm = newSwarmStore(filepath.Join(s.contributorsDirOrDefault(), SwarmStateFileName), &fakeSwarmScorer{score: ghpkg.SwarmScore{IssuesClosed: 1, PRsMerged: 1}})

	rec := doOwnerPost(s, "/api/swarm", map[string]string{"repo": "api"})
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}

	var started SwarmRecord
	if err := json.NewDecoder(rec.Body).Decode(&started); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	if started.Repo != "acme/api" {
		t.Fatalf("repo = %q, want acme/api", started.Repo)
	}
	conflict := doOwnerPost(s, "/api/swarm", map[string]string{"repo": "web"})
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d body=%s", conflict.Code, conflict.Body.String())
	}
	end := doDeleteOwner(s, "/api/swarm", nil)
	if end.Code != http.StatusOK {
		t.Fatalf("end status = %d body=%s", end.Code, end.Body.String())
	}
	history := doOwnerGet(s, "/api/swarm/history")
	if history.Code != http.StatusOK {
		t.Fatalf("history status = %d body=%s", history.Code, history.Body.String())
	}
	var body SwarmHistoryResponse
	if err := json.NewDecoder(history.Body).Decode(&body); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(body.History) != 1 || len(body.Leaderboard) != 1 || body.Leaderboard[0].Score != 2 {
		t.Fatalf("history body = %+v", body)
	}
}

func TestSwarmStartPersistsPrepAfterActiveRecord(t *testing.T) {
	t.Setenv(swarmEnvPrepTimeout, "1s")
	s, deps := apiServer(t)
	deps.Config.Project.Org = "acme"
	deps.Config.Project.Repos = []string{"api"}
	deps.GHClient = nil
	s.SetContributorsDir(t.TempDir())
	s.swarm = newSwarmStore(filepath.Join(s.contributorsDirOrDefault(), SwarmStateFileName), nil)

	rec := doOwnerPost(s, "/api/swarm", map[string]string{"repo": "api"})
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d body=%s", rec.Code, rec.Body.String())
	}
	var started SwarmRecord
	if err := json.NewDecoder(rec.Body).Decode(&started); err != nil {
		t.Fatalf("decode start: %v", err)
	}
	if started.Prep == nil || len(started.Prep.Errors) == 0 {
		t.Fatalf("prep = %+v, want persisted prep with no-gh error", started.Prep)
	}

	reloaded := newSwarmStore(filepath.Join(s.contributorsDirOrDefault(), SwarmStateFileName), nil)
	status, err := reloaded.status(context.Background())
	if err != nil {
		t.Fatalf("reload status: %v", err)
	}
	if status.Active == nil || status.Active.Prep == nil {
		t.Fatalf("reloaded active prep = %+v", status.Active)
	}
}
func TestSwarmIdleGateBlocksAndUnlocksAtThreshold(t *testing.T) {
	t.Setenv(swarmEnvUnlockIdlePct, "75")
	s, deps := apiServer(t)
	deps.Config.Project.Org = "acme"
	deps.Config.Project.Repos = []string{"api"}
	deps.Config.Agents["architect"] = deps.Config.Agents["scanner"]
	s.SetContributorsDir(t.TempDir())
	s.swarm = seededSwarmStore(t, s, []SwarmRecord{{Repo: "acme/web", Start: time.Now().Add(-2 * time.Hour), End: time.Now().Add(-time.Hour)}})
	setSwarmAgents(s, []FrontendAgent{
		{Name: "scanner", Busy: "working", State: "running"},
		{Name: "architect", Busy: "idle", State: "running"},
	})

	locked := doOwnerPost(s, "/api/swarm", map[string]string{"repo": "api"})
	if locked.Code != http.StatusLocked {
		t.Fatalf("locked status = %d body=%s", locked.Code, locked.Body.String())
	}
	var lockBody struct {
		Error         string `json:"error"`
		IdlePct       int    `json:"idle_pct"`
		UnlockIdlePct int    `json:"unlock_idle_pct"`
	}
	if err := json.NewDecoder(locked.Body).Decode(&lockBody); err != nil {
		t.Fatalf("decode lock body: %v", err)
	}
	if lockBody.IdlePct != 50 || lockBody.UnlockIdlePct != 75 || lockBody.Error == "" {
		t.Fatalf("lock body = %+v", lockBody)
	}

	setSwarmAgents(s, []FrontendAgent{
		{Name: "scanner", Busy: "idle", State: "running"},
		{Name: "architect", Busy: "idle", State: "running"},
	})
	unlocked := doOwnerPost(s, "/api/swarm", map[string]string{"repo": "api"})
	if unlocked.Code != http.StatusOK {
		t.Fatalf("unlocked status = %d body=%s", unlocked.Code, unlocked.Body.String())
	}
}

func TestSwarmIdleGateDisabledForceAndFirstSwarm(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project.Org = "acme"
	deps.Config.Project.Repos = []string{"api", "web", "first"}
	s.SetContributorsDir(t.TempDir())
	setSwarmAgents(s, []FrontendAgent{{Name: "scanner", Busy: "working", State: "running"}})

	t.Setenv(swarmEnvUnlockIdlePct, "100")
	s.swarm = seededSwarmStore(t, s, nil)
	first := doOwnerPost(s, "/api/swarm", map[string]string{"repo": "first"})
	if first.Code != http.StatusOK {
		t.Fatalf("first swarm status = %d body=%s", first.Code, first.Body.String())
	}

	_, _ = s.swarmStore().end(context.Background(), "ended")
	forced := doOwnerPost(s, "/api/swarm", map[string]any{"repo": "api", "force": true})
	if forced.Code != http.StatusOK {
		t.Fatalf("force status = %d body=%s", forced.Code, forced.Body.String())
	}

	_, _ = s.swarmStore().end(context.Background(), "ended")
	t.Setenv(swarmEnvUnlockIdlePct, "0")
	disabled := doOwnerPost(s, "/api/swarm", map[string]string{"repo": "web"})
	if disabled.Code != http.StatusOK {
		t.Fatalf("disabled gate status = %d body=%s", disabled.Code, disabled.Body.String())
	}
}

func TestSwarmStatusIncludesIdleUnlockFields(t *testing.T) {
	t.Setenv(swarmEnvUnlockIdlePct, "75")
	s, deps := apiServer(t)
	deps.Config.Agents["architect"] = deps.Config.Agents["scanner"]
	s.SetContributorsDir(t.TempDir())
	s.swarm = seededSwarmStore(t, s, []SwarmRecord{{Repo: "myorg/repo1", Start: time.Now().Add(-2 * time.Hour), End: time.Now().Add(-time.Hour)}})
	setSwarmAgents(s, []FrontendAgent{
		{Name: "scanner", Busy: "working", State: "running"},
		{Name: "architect", Busy: "idle", State: "running"},
	})

	rec := doOwnerGet(s, "/api/swarm")
	if rec.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", rec.Code, rec.Body.String())
	}
	var status SwarmStatus
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.IdlePct != 50 || status.UnlockIdlePct != 75 || status.Unlocked {
		t.Fatalf("status = %+v", status)
	}
}

func seededSwarmStore(t *testing.T, s *Server, history []SwarmRecord) *swarmStore {
	t.Helper()
	store := newSwarmStore(filepath.Join(s.contributorsDirOrDefault(), SwarmStateFileName), nil)
	store.loaded = true
	store.state.History = append([]SwarmRecord(nil), history...)
	return store
}

func setSwarmAgents(s *Server, agents []FrontendAgent) {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if s.status == nil {
		s.status = &StatusPayload{}
	}
	s.status.Agents = append([]FrontendAgent(nil), agents...)
}
