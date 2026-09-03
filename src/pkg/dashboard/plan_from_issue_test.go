package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/planning"
)

// acmmL5 returns a pointer to ACMM level 5, the lowest level at which planning is
// enabled (the architect that decomposes epics is scheduled at L5/L6). Tests that
// exercise the mint/kick paths must set this so handlePlanFromIssue's L5+ gate
// admits the request; the gate itself is covered by TestHandlePlanFromIssue_LevelGate.
func acmmL5() *int { l := planning.PlanningMinACMMLevel; return &l }

// planIssueServer builds a Server with an empty "architect" bead store and no
// AgentMgr, so handlePlanFromIssue mints the epic but records kicked=false
// (the kick path is nil-guarded). ACMM level is set to L5 so the L5+ planning
// gate admits the request. Returns the server and store.
func planIssueServer(t *testing.T) (*Server, *beads.Store) {
	t.Helper()
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{
		Config:     &config.Config{Agents: map[string]config.AgentConfig{}, ACMMLevel: acmmL5()},
		Logger:     slog.Default(),
		Ctx:        context.Background(),
		BeadStores: map[string]*beads.Store{"architect": store},
	}
	srv.RegisterAPI(srv.deps)
	return srv, store
}

func postPlanFromIssue(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/plan/from-issue", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func TestHandlePlanFromIssue_MintsEpic(t *testing.T) {
	srv, store := planIssueServer(t)

	w := postPlanFromIssue(t, srv, `{"repo":"kubestellar/hive","number":11,"url":"https://x/11","title":"Plan me","body":"do the thing"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK     bool   `json:"ok"`
		EpicID string `json:"epic_id"`
		Kicked bool   `json:"kicked"`
		Poll   string `json:"poll_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK || resp.EpicID == "" {
		t.Fatalf("bad response: %+v", resp)
	}
	if resp.Kicked {
		t.Error("kicked should be false with no AgentMgr")
	}
	if resp.Poll != "/api/plan/"+resp.EpicID {
		t.Errorf("poll_url = %q", resp.Poll)
	}
	// The epic exists in the store with the right ref + body.
	epic, err := store.Get(resp.EpicID)
	if err != nil {
		t.Fatalf("epic not in store: %v", err)
	}
	if epic.ExternalRef != "gh-kubestellar/hive#11" {
		t.Errorf("ref = %q", epic.ExternalRef)
	}
	if epic.Notes != "do the thing" {
		t.Errorf("notes = %q", epic.Notes)
	}
}

func TestHandlePlanFromIssue_Idempotent(t *testing.T) {
	srv, store := planIssueServer(t)
	body := `{"repo":"org/repo","number":5,"title":"once"}`

	w1 := postPlanFromIssue(t, srv, body)
	w2 := postPlanFromIssue(t, srv, body)
	if w1.Code != 200 || w2.Code != 200 {
		t.Fatalf("codes %d %d", w1.Code, w2.Code)
	}
	var r1, r2 struct {
		EpicID string `json:"epic_id"`
	}
	_ = json.Unmarshal(w1.Body.Bytes(), &r1)
	_ = json.Unmarshal(w2.Body.Bytes(), &r2)
	if r1.EpicID != r2.EpicID {
		t.Fatalf("duplicate epics: %s vs %s", r1.EpicID, r2.EpicID)
	}
	epics := 0
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Type == beads.TypeEpic {
			epics++
		}
	}
	if epics != 1 {
		t.Fatalf("expected 1 epic, got %d", epics)
	}
}

func TestHandlePlanFromIssue_BadBody(t *testing.T) {
	srv, _ := planIssueServer(t)
	w := postPlanFromIssue(t, srv, `not json`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", w.Code)
	}
}

func TestHandlePlanFromIssue_NoTitleNoResolve(t *testing.T) {
	srv, _ := planIssueServer(t)
	// number+repo with no title and nothing in status to resolve → 400.
	w := postPlanFromIssue(t, srv, `{"repo":"org/repo","number":9}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandlePlanFromIssue_ResolvesTitleFromStatus(t *testing.T) {
	srv, store := planIssueServer(t)
	// Seed the status payload with an actionable issue so a title-less request
	// resolves the title/labels from it.
	srv.statusMu.Lock()
	srv.status = &StatusPayload{
		Repos: []FrontendRepo{
			{
				Name: "hive",
				Full: "kubestellar/hive",
				ActionableIssues: []any{
					github.Issue{Repo: "kubestellar/hive", Number: 21, Title: "resolved title", URL: "https://x/21", Labels: []string{"plan"}},
				},
			},
		},
	}
	srv.statusMu.Unlock()

	w := postPlanFromIssue(t, srv, `{"repo":"kubestellar/hive","number":21}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		EpicID string `json:"epic_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	epic, err := store.Get(resp.EpicID)
	if err != nil {
		t.Fatalf("epic: %v", err)
	}
	if epic.Title != "resolved title" {
		t.Errorf("title = %q, want resolved", epic.Title)
	}
	if epic.Meta(planning.MetaIssueLabels) != "plan" {
		t.Errorf("labels = %q", epic.Meta(planning.MetaIssueLabels))
	}
}

func TestHandlePlanFromIssue_NoStores(t *testing.T) {
	srv := NewServer(0, slog.Default())
	// L5 so the request clears the planning-level gate and reaches the store check.
	srv.deps = &Dependencies{Config: &config.Config{ACMMLevel: acmmL5()}, Logger: slog.Default(), Ctx: context.Background()}
	srv.RegisterAPI(srv.deps)
	w := postPlanFromIssue(t, srv, `{"repo":"a/b","number":1,"title":"t"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", w.Code)
	}
}

func TestHandlePlanFromIssue_WithAgentMgr_KickPath(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	logger := slog.Default()
	cfg := &config.Config{Agents: map[string]config.AgentConfig{"architect": {}}, ACMMLevel: acmmL5()}
	mgr := agent.NewManager(cfg.Agents, logger, agent.ProjectContext{})
	gov := governor.New(cfg.Governor, cfg.Agents, logger)

	srv := NewServer(0, logger)
	srv.deps = &Dependencies{
		Config:     cfg,
		Logger:     logger,
		Ctx:        context.Background(),
		AgentMgr:   mgr,
		Governor:   gov,
		BeadStores: map[string]*beads.Store{"architect": store},
	}
	srv.RegisterAPI(srv.deps)

	// The architect agent has no live tmux session, so SendKick fails and the
	// handler records kicked=false — exercising the AgentMgr-present + kick-error
	// branch while still minting the epic.
	w := postPlanFromIssue(t, srv, `{"repo":"org/repo","number":88,"title":"kick me"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK     bool   `json:"ok"`
		EpicID string `json:"epic_id"`
		Kicked bool   `json:"kicked"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.OK || resp.EpicID == "" {
		t.Fatalf("bad response: %+v", resp)
	}
	if _, err := store.Get(resp.EpicID); err != nil {
		t.Fatalf("epic not minted: %v", err)
	}
}

// TestHandlePlanFromIssue_LevelGate proves the L5+ gate on the HTTP handler:
// below L5 the request is rejected (409) with the gate message and NO epic is
// minted; at L5/L6 it proceeds and mints. Covers the boundary (L4 blocked, L5
// allowed) plus the nil-config edge.
func TestHandlePlanFromIssue_LevelGate(t *testing.T) {
	newSrvAtLevel := func(level *int) (*Server, *beads.Store) {
		store, err := beads.NewStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewStore: %v", err)
		}
		srv := NewServer(0, slog.Default())
		srv.deps = &Dependencies{
			Config:     &config.Config{Agents: map[string]config.AgentConfig{}, ACMMLevel: level},
			Logger:     slog.Default(),
			Ctx:        context.Background(),
			BeadStores: map[string]*beads.Store{"architect": store},
		}
		srv.RegisterAPI(srv.deps)
		return srv, store
	}
	lvl := func(n int) *int { return &n }

	tests := []struct {
		name     string
		level    *int
		wantCode int
		wantMint bool
	}{
		{"nil level (defaults to L1) blocked", nil, http.StatusConflict, false},
		{"L1 blocked", lvl(1), http.StatusConflict, false},
		{"L4 blocked (boundary)", lvl(4), http.StatusConflict, false},
		{"L5 allowed (boundary)", lvl(5), http.StatusOK, true},
		{"L6 allowed", lvl(6), http.StatusOK, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, store := newSrvAtLevel(tc.level)
			w := postPlanFromIssue(t, srv, `{"repo":"org/repo","number":42,"title":"gate me"}`)
			if w.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d: %s", w.Code, tc.wantCode, w.Body.String())
			}
			epics := 0
			for _, b := range store.List(beads.ListFilter{}) {
				if b.Type == beads.TypeEpic {
					epics++
				}
			}
			if tc.wantMint && epics != 1 {
				t.Errorf("expected 1 epic minted, got %d", epics)
			}
			if !tc.wantMint {
				if epics != 0 {
					t.Errorf("expected no epic minted below L5, got %d", epics)
				}
				// The rejection must carry the explanatory gate message.
				var resp struct {
					Error string `json:"error"`
				}
				_ = json.Unmarshal(w.Body.Bytes(), &resp)
				if resp.Error != planning.PlanningLevelGateMessage {
					t.Errorf("error = %q, want the gate message", resp.Error)
				}
			}
		})
	}
}

// stubKicker is a test DecomposeKicker with a settable paused state.
type stubKicker struct {
	paused bool
	kicks  int
}

func (k *stubKicker) IsPaused(string) bool          { return k.paused }
func (k *stubKicker) SendKick(string, string) error { k.kicks++; return nil }

func TestRequestArchitectDecompose_States(t *testing.T) {
	store, _ := beads.NewStore(t.TempDir())
	epic, _ := store.Create("e", beads.TypeEpic, beads.PriorityMedium, "architect", "gh-a/b#1")
	logger := slog.Default()
	cfg := &config.Config{Agents: map[string]config.AgentConfig{"architect": {}}, ACMMLevel: acmmL5()}

	newSrv := func(k planning.DecomposeKicker) *Server {
		srv := NewServer(0, logger)
		srv.deps = &Dependencies{
			Config:   cfg,
			Logger:   logger,
			Ctx:      context.Background(),
			Governor: governor.New(cfg.Governor, cfg.Agents, logger),
		}
		srv.decomposeKickerOverride = k
		return srv
	}

	// Available architect → kicked.
	avail := &stubKicker{}
	if got := newSrv(avail).requestArchitectDecompose(epic); got != planning.DecomposeKicked {
		t.Errorf("available: got %q, want kicked", got)
	}
	if avail.kicks != 1 {
		t.Errorf("expected 1 kick, got %d", avail.kicks)
	}

	// Paused architect → queued_paused, NOT kicked.
	paused := &stubKicker{paused: true}
	if got := newSrv(paused).requestArchitectDecompose(epic); got != planning.DecomposeQueuedPaused {
		t.Errorf("paused: got %q, want queued_paused", got)
	}
	if paused.kicks != 0 {
		t.Error("paused architect must not be kicked")
	}

	// No kicker → queued_no_agent.
	if got := newSrv(nil).requestArchitectDecompose(epic); got != planning.DecomposeQueuedNoAgent {
		t.Errorf("no agent: got %q, want queued_no_agent", got)
	}
}

func TestHandlePlanFromIssue_PausedArchitectMessage(t *testing.T) {
	store, _ := beads.NewStore(t.TempDir())
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{
		Config:     &config.Config{Agents: map[string]config.AgentConfig{}, ACMMLevel: acmmL5()},
		Logger:     slog.Default(),
		Ctx:        context.Background(),
		BeadStores: map[string]*beads.Store{"architect": store},
	}
	srv.decomposeKickerOverride = &stubKicker{paused: true}
	srv.RegisterAPI(srv.deps)

	w := postPlanFromIssue(t, srv, `{"repo":"org/repo","number":77,"title":"paused plan"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK              bool   `json:"ok"`
		Kicked          bool   `json:"kicked"`
		State           string `json:"state"`
		ArchitectPaused bool   `json:"architectPaused"`
		Message         string `json:"message"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Kicked || !resp.ArchitectPaused {
		t.Fatalf("expected paused/queued, got %+v", resp)
	}
	if resp.State != string(planning.DecomposeQueuedPaused) {
		t.Errorf("state = %q", resp.State)
	}
	if resp.Message != planning.ArchitectPausedMessage {
		t.Errorf("message = %q, want the paused message", resp.Message)
	}
	// The epic is still minted and pending (queued, not dropped).
	epics := 0
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Type == beads.TypeEpic && planning.DecomposePending(b) {
			epics++
		}
	}
	if epics != 1 {
		t.Fatalf("expected 1 pending epic queued, got %d", epics)
	}
}

func TestPlanEpicStore_FallbackToAnyStore(t *testing.T) {
	store, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	srv := NewServer(0, slog.Default())
	// No "architect" store — planEpicStore must fall back to the only store.
	srv.deps = &Dependencies{
		Config:     &config.Config{Agents: map[string]config.AgentConfig{}, ACMMLevel: acmmL5()},
		Logger:     slog.Default(),
		Ctx:        context.Background(),
		BeadStores: map[string]*beads.Store{"scanner": store},
	}
	srv.RegisterAPI(srv.deps)

	w := postPlanFromIssue(t, srv, `{"url":"https://x/501","title":"fallback epic"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	epics := 0
	for _, b := range store.List(beads.ListFilter{}) {
		if b.Type == beads.TypeEpic {
			epics++
		}
	}
	if epics != 1 {
		t.Fatalf("expected epic in fallback store, got %d", epics)
	}
}

func TestHandlePlanFromIssue_TitleButNoRef(t *testing.T) {
	srv, _ := planIssueServer(t)
	// A title with no repo/number/url passes the title guard but has no stable
	// ref, so EpicFromIssue returns an error → 400 (covers the mint-error path).
	w := postPlanFromIssue(t, srv, `{"title":"orphan title"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandlePlanFromIssue_ResolveFillsRepoAndURL(t *testing.T) {
	srv, store := planIssueServer(t)
	// Request carries only a URL (no repo/number/title); resolve must fill in
	// repo, title, and labels from the status payload.
	srv.statusMu.Lock()
	srv.status = &StatusPayload{
		Repos: []FrontendRepo{{
			Name:             "hive",
			Full:             "kubestellar/hive",
			ActionableIssues: []any{github.Issue{Repo: "kubestellar/hive", Number: 44, Title: "filled in", URL: "https://x/44", Labels: []string{"epic"}}},
		}},
	}
	srv.statusMu.Unlock()

	w := postPlanFromIssue(t, srv, `{"url":"https://x/44"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		EpicID string `json:"epic_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	epic, err := store.Get(resp.EpicID)
	if err != nil {
		t.Fatalf("epic: %v", err)
	}
	if epic.Meta(planning.MetaIssueRepo) != "kubestellar/hive" {
		t.Errorf("repo not filled: %q", epic.Meta(planning.MetaIssueRepo))
	}
	if epic.Title != "filled in" {
		t.Errorf("title = %q", epic.Title)
	}
}

func TestResolveActionableIssue_ByURLAndMiss(t *testing.T) {
	srv, _ := planIssueServer(t)
	srv.statusMu.Lock()
	srv.status = &StatusPayload{
		Repos: []FrontendRepo{{
			Name:             "hive",
			Full:             "kubestellar/hive",
			ActionableIssues: []any{github.Issue{Repo: "kubestellar/hive", Number: 33, Title: "by url", URL: "https://x/33"}},
		}},
	}
	srv.statusMu.Unlock()

	// URL match.
	if iss, ok := srv.resolveActionableIssue("", 0, "https://x/33"); !ok || iss.Title != "by url" {
		t.Errorf("URL resolve failed: %+v ok=%v", iss, ok)
	}
	// Miss.
	if _, ok := srv.resolveActionableIssue("other/repo", 999, ""); ok {
		t.Error("expected miss")
	}
}

func TestResolveActionableIssue_NilStatus(t *testing.T) {
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Logger: slog.Default(), Ctx: context.Background()}
	if _, ok := srv.resolveActionableIssue("a/b", 1, ""); ok {
		t.Error("nil status should return not-ok")
	}
}

func TestPlanEpicStore_EmptyAndNil(t *testing.T) {
	// Non-nil but empty BeadStores → (nil, "").
	srv := NewServer(0, slog.Default())
	srv.deps = &Dependencies{Logger: slog.Default(), BeadStores: map[string]*beads.Store{}}
	if store, name := srv.planEpicStore(); store != nil || name != "" {
		t.Errorf("empty stores: got (%v,%q), want (nil,\"\")", store, name)
	}
	// Nil deps → (nil, "").
	srv2 := NewServer(0, slog.Default())
	if store, _ := srv2.planEpicStore(); store != nil {
		t.Error("nil deps should yield nil store")
	}
}

func TestBuildPlanning_PendingAndPaused(t *testing.T) {
	store, _ := beads.NewStore(t.TempDir())
	// Two issue-sourced epics → both decompose_pending, plan_status=draft.
	if _, err := planning.EpicFromIssue(store, github.Issue{Repo: "a/b", Number: 1, Title: "p1"}, ""); err != nil {
		t.Fatalf("epic1: %v", err)
	}
	if _, err := planning.EpicFromIssue(store, github.Issue{Repo: "a/b", Number: 2, Title: "p2"}, ""); err != nil {
		t.Fatalf("epic2: %v", err)
	}

	// Architect NOT paused → pending counted, ArchitectPaused false.
	fp := BuildPlanning(map[string]*beads.Store{"architect": store}, false, 6)
	if fp.PendingDecompose != 2 {
		t.Errorf("PendingDecompose = %d, want 2", fp.PendingDecompose)
	}
	if fp.AwaitingReview != 2 {
		t.Errorf("AwaitingReview = %d, want 2 (draft epics)", fp.AwaitingReview)
	}
	if fp.ArchitectPaused {
		t.Error("ArchitectPaused should be false when architect is not paused")
	}

	// Architect paused AND pending>0 → flag set.
	fpPaused := BuildPlanning(map[string]*beads.Store{"architect": store}, true, 6)
	if !fpPaused.ArchitectPaused {
		t.Error("ArchitectPaused should be true when architect paused with pending work")
	}
}

func TestBuildPlanning_PausedButNoPendingIsQuiet(t *testing.T) {
	store, _ := beads.NewStore(t.TempDir())
	// A decomposed (non-pending) epic; architect paused but nothing queued.
	epic, _ := store.Create("done", beads.TypeEpic, beads.PriorityHigh, "architect", "")
	planning.DecomposeFromOutput(store, epic, "1. [T1] a [agent_suitable]\n", planning.Options{})
	fp := BuildPlanning(map[string]*beads.Store{"architect": store}, true, 6)
	if fp.ArchitectPaused {
		t.Error("no pending work → ArchitectPaused must stay false even if paused")
	}
}

func TestArchitectPausedFromStatuses(t *testing.T) {
	if architectPausedFromStatuses(nil) {
		t.Error("nil statuses → false")
	}
	statuses := map[string]*agent.AgentProcess{"architect": {Paused: true}}
	if !architectPausedFromStatuses(statuses) {
		t.Error("paused architect status → true")
	}
	statuses["architect"].Paused = false
	if architectPausedFromStatuses(statuses) {
		t.Error("unpaused architect → false")
	}
}

func TestClassifierSectionResponse(t *testing.T) {
	section := classifierSectionResponse()
	simple, ok := section["simpleKeywords"].([]string)
	if !ok || len(simple) == 0 {
		t.Fatalf("simpleKeywords missing/empty: %#v", section["simpleKeywords"])
	}
	complex, ok := section["complexSignals"].([]string)
	if !ok || len(complex) == 0 {
		t.Fatalf("complexSignals missing/empty: %#v", section["complexSignals"])
	}
}

func TestGovernorConfigGet_IncludesClassifier(t *testing.T) {
	srv, _ := planIssueServer(t)
	// Governor config GET needs a minimal governor config; the default zero
	// Config is sufficient for the classifier section.
	req := httptest.NewRequest("GET", "/api/config/governor", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cls, ok := resp["classifier"].(map[string]any)
	if !ok {
		t.Fatalf("classifier section missing: %#v", resp["classifier"])
	}
	if _, ok := cls["simpleKeywords"]; !ok {
		t.Error("simpleKeywords missing from governor config")
	}
	if _, ok := cls["complexSignals"]; !ok {
		t.Error("complexSignals missing from governor config")
	}
}
