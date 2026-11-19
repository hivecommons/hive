package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/retro"
)

type advisorSummaryResp struct {
	Window struct {
		Since         *string `json:"since"`
		FirstDecision *string `json:"first_decision"`
		LastDecision  *string `json:"last_decision"`
		RankModeStart *string `json:"rank_mode_start"`
		Decisions     int     `json:"decisions"`
	} `json:"window"`
	Settings map[string]interface{} `json:"settings"`
	Counts   struct {
		ByMode    map[string]int `json:"by_mode"`
		ByArm     map[string]int `json:"by_arm"`
		ByOutcome map[string]int `json:"by_outcome"`
	} `json:"counts"`
	Arms  []advisorArmJSON `json:"arms"`
	Mixed *advisorArmJSON  `json:"mixed"`
	None  *advisorArmJSON  `json:"none"`
	Cost  struct {
		LatencySamples     int     `json:"latency_samples"`
		MeanLatencyMS      float64 `json:"mean_latency_ms"`
		TokenSamples       int     `json:"token_samples"`
		MeanPromptTokens   float64 `json:"mean_prompt_tokens"`
		MeanCompletionToks float64 `json:"mean_completion_tokens"`
	} `json:"cost"`
	SmallN bool   `json:"small_n"`
	Caveat string `json:"caveat"`
}

func advisorTestServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "dashboard-secret", logger)
	s.RegisterAPI(testDeps(t))
	return s
}

func getAdvisorSummary(t *testing.T, s *Server, target, role string) (*httptest.ResponseRecorder, advisorSummaryResp) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	s.mux.ServeHTTP(rec, req)
	var out advisorSummaryResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
		}
	}
	return rec, out
}

func TestAdvisorSummaryEmptyStore(t *testing.T) {
	s := advisorTestServer(t)
	s.SetKnowledgeAdvisorSummary(retro.NewAdvisorDecisionStore(""), nil)
	rec, got := getAdvisorSummary(t, s, "/api/knowledge/advisor/summary", config.RoleRead)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	if got.Window.Decisions != 0 || got.Window.FirstDecision != nil || got.Window.LastDecision != nil || got.Window.RankModeStart != nil {
		t.Fatalf("window = %+v, want empty", got.Window)
	}
	if len(got.Counts.ByMode) != 0 || len(got.Counts.ByArm) != 0 || len(got.Counts.ByOutcome) != 0 {
		t.Fatalf("counts = %+v, want empty", got.Counts)
	}
	if len(got.Arms) != 0 || got.Mixed != nil || got.None != nil {
		t.Fatalf("arms = %+v mixed=%v none=%v, want none", got.Arms, got.Mixed, got.None)
	}
	if !got.SmallN || got.Caveat == "" {
		t.Fatalf("empty store must carry the small-n caveat: %+v", got)
	}
	if got.Settings["mode"] != config.AdvisorLLMModeOff {
		t.Fatalf("mode = %v, want off", got.Settings["mode"])
	}
}

func TestAdvisorSummaryNoSourceInstalled(t *testing.T) {
	s := advisorTestServer(t)
	rec, got := getAdvisorSummary(t, s, "/api/knowledge/advisor/summary", config.RoleRead)
	if rec.Code != http.StatusOK || got.Window.Decisions != 0 || !got.SmallN {
		t.Fatalf("code=%d got=%+v", rec.Code, got)
	}
}

func advisorLane(t *testing.T, store *retro.AdvisorDecisionStore, closed map[string]int) *retro.Lane {
	t.Helper()
	source, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	retroStore, err := beads.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for issue, fix := range closed {
		b, err := source.Create("work "+issue, beads.TypeTask, beads.PriorityMedium, "scanner", issue)
		if err != nil {
			t.Fatal(err)
		}
		fixStr := strconv.Itoa(fix)
		if err := source.Update(b.ID, func(bd *beads.Bead) {
			bd.Status = beads.StatusClosed
			bd.Metadata = map[string]interface{}{"pr_ref": issue + "-pr", "pr_state": "merged", "fix_attempts": fixStr, "kicks_received": "2"}
		}); err != nil {
			t.Fatal(err)
		}
	}
	lane := retro.NewLane(map[string]*beads.Store{"scanner": source, retro.Actor: retroStore}, retroStore, nil, nil, retro.Config{ScanIntervalS: 1}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	lane.SetAdvisorDecisions(store)
	return lane
}

func TestAdvisorSummaryMixedArmsMatchLane(t *testing.T) {
	store := retro.NewAdvisorDecisionStore("")
	rec := func(ts, kick, ref, mode, outcome string, latency int64, prompt, completion int) {
		store.Record(retro.AdvisorDecisionRecord{Timestamp: ts, Agent: "dev", Kick: kick, Refs: []string{ref}, Mode: mode, Outcome: outcome, Slug: "secret-slug-" + kick, LatencyMS: latency, PromptTokens: prompt, CompletionTokens: completion})
	}
	rec("2026-11-01T10:00:00Z", "k1", "o/r#1", "shadow", "gated", 0, 0, 0)
	rec("2026-11-10T10:00:00Z", "k2", "o/r#2", "rank", "held-out", 100, 200, 10)
	rec("2026-11-11T10:00:00Z", "k3", "o/r#3", "rank", "delivered", 300, 400, 30)
	// o/r#4 is both delivered and held out: the mixed arm.
	rec("2026-11-12T10:00:00Z", "k4", "o/r#4", "rank", "delivered", 0, 0, 0)
	rec("2026-11-12T11:00:00Z", "k5", "o/r#4", "rank", "held-out", 0, 0, 0)
	lane := advisorLane(t, store, map[string]int{"o/r#2": 4, "o/r#3": 1, "o/r#4": 2, "o/r#5": 7})

	s := advisorTestServer(t)
	s.deps.Config.Knowledge = config.KnowledgeConfig{Advisor: true, AdvisorLLM: config.AdvisorLLMConfig{Mode: "rank", Model: "small-model", HoldoutPercent: 20, APIKeyEnv: "SHOULD_NOT_APPEAR", Endpoint: "https://llm.example.invalid"}}
	s.SetKnowledgeAdvisorSummary(store, lane.AdvisorArmSummary)

	httpRec, got := getAdvisorSummary(t, s, "/api/knowledge/advisor/summary", config.RoleRead)
	if httpRec.Code != http.StatusOK {
		t.Fatalf("code = %d body=%s", httpRec.Code, httpRec.Body.String())
	}
	if got.Window.Decisions != 5 || *got.Window.FirstDecision != "2026-11-01T10:00:00Z" || *got.Window.LastDecision != "2026-11-12T11:00:00Z" || *got.Window.RankModeStart != "2026-11-10T10:00:00Z" {
		t.Fatalf("window = %+v", got.Window)
	}
	if !reflect.DeepEqual(got.Counts.ByMode, map[string]int{"shadow": 1, "rank": 4}) {
		t.Fatalf("by_mode = %v", got.Counts.ByMode)
	}
	if !reflect.DeepEqual(got.Counts.ByArm, map[string]int{"shadow": 1, "holdout": 2, "deliver": 2}) {
		t.Fatalf("by_arm = %v", got.Counts.ByArm)
	}
	if !reflect.DeepEqual(got.Counts.ByOutcome, map[string]int{"gated": 1, "held-out": 2, "delivered": 2}) {
		t.Fatalf("by_outcome = %v", got.Counts.ByOutcome)
	}
	if got.Cost.LatencySamples != 2 || got.Cost.MeanLatencyMS != 200 || got.Cost.TokenSamples != 2 || got.Cost.MeanPromptTokens != 300 || got.Cost.MeanCompletionToks != 20 {
		t.Fatalf("cost = %+v", got.Cost)
	}

	// Numbers equal Lane.AdvisorArmSummary(), mixed and none listed separately.
	want := lane.AdvisorArmSummary()
	var wantArms []advisorArmJSON
	for _, a := range want {
		row := toAdvisorArmJSON(a)
		switch a.Arm {
		case retro.AdvisorArmMixed:
			if got.Mixed == nil || *got.Mixed != row {
				t.Fatalf("mixed = %+v, want %+v", got.Mixed, row)
			}
		case retro.AdvisorArmNone:
			if got.None == nil || *got.None != row {
				t.Fatalf("none = %+v, want %+v", got.None, row)
			}
		default:
			wantArms = append(wantArms, row)
		}
	}
	if got.Mixed == nil || got.None == nil {
		t.Fatalf("fixture must produce mixed and none arms: %+v", want)
	}
	if !reflect.DeepEqual(got.Arms, wantArms) {
		t.Fatalf("arms = %+v, want %+v", got.Arms, wantArms)
	}
	if len(got.Arms) != 2 {
		t.Fatalf("arms = %+v, want deliver and holdout", got.Arms)
	}

	if got.Settings["mode"] != "rank" || got.Settings["model"] != "small-model" || got.Settings["holdout_percent"] != float64(20) || got.Settings["enabled"] != true {
		t.Fatalf("settings = %v", got.Settings)
	}
	body := httpRec.Body.String()
	for _, banned := range []string{"secret-slug", "o/r#", "SHOULD_NOT_APPEAR", "llm.example.invalid", "dev", "refs", "slug"} {
		if strings.Contains(body, banned) {
			t.Fatalf("response leaks %q: %s", banned, body)
		}
	}
}

func TestAdvisorSummarySinceNarrowsDecisionsOnly(t *testing.T) {
	store := retro.NewAdvisorDecisionStore("")
	store.Record(retro.AdvisorDecisionRecord{Timestamp: "2026-11-01T10:00:00Z", Agent: "dev", Kick: "k1", Mode: "shadow", Outcome: "gated"})
	store.Record(retro.AdvisorDecisionRecord{Timestamp: "2026-11-10T10:00:00Z", Agent: "dev", Kick: "k2", Mode: "rank", Outcome: "delivered"})
	s := advisorTestServer(t)
	s.SetKnowledgeAdvisorSummary(store, nil)

	_, got := getAdvisorSummary(t, s, "/api/knowledge/advisor/summary?since=2026-11-05T00:00:00Z", config.RoleRead)
	if got.Window.Decisions != 1 || got.Window.Since == nil || *got.Window.Since != "2026-11-05T00:00:00Z" || got.Counts.ByMode["rank"] != 1 || got.Counts.ByMode["shadow"] != 0 {
		t.Fatalf("got = %+v", got)
	}
	rec, _ := getAdvisorSummary(t, s, "/api/knowledge/advisor/summary?since=yesterday", config.RoleRead)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad since = %d, want 400", rec.Code)
	}
}

func TestAdvisorSummarySmallNCaveatClearsAtThreshold(t *testing.T) {
	store := retro.NewAdvisorDecisionStore("")
	closed := map[string]int{}
	for i := 0; i < advisorSmallNBeads; i++ {
		d, h := "o/r#"+strconv.Itoa(1000+i), "o/r#"+strconv.Itoa(2000+i)
		closed[d], closed[h] = 1, 3
		store.Record(retro.AdvisorDecisionRecord{Timestamp: time.Now().UTC().Format(time.RFC3339), Agent: "dev", Kick: "d" + strconv.Itoa(i), Refs: []string{d}, Mode: "rank", Outcome: "delivered"})
		store.Record(retro.AdvisorDecisionRecord{Timestamp: time.Now().UTC().Format(time.RFC3339), Agent: "dev", Kick: "h" + strconv.Itoa(i), Refs: []string{h}, Mode: "rank", Outcome: "held-out"})
	}
	s := advisorTestServer(t)
	s.SetKnowledgeAdvisorSummary(store, advisorLane(t, store, closed).AdvisorArmSummary)
	_, got := getAdvisorSummary(t, s, "/api/knowledge/advisor/summary", config.RoleRead)
	if got.SmallN || got.Caveat != "" {
		t.Fatalf("at %d beads per arm the caveat should clear: %+v", advisorSmallNBeads, got)
	}

	closed["o/r#2000"] = 3
	delete(closed, "o/r#2001")
	s.SetKnowledgeAdvisorSummary(store, advisorLane(t, store, closed).AdvisorArmSummary)
	_, got = getAdvisorSummary(t, s, "/api/knowledge/advisor/summary", config.RoleRead)
	if !got.SmallN || got.Caveat == "" {
		t.Fatalf("one bead short in the holdout arm should keep the caveat: %+v", got)
	}
}

func TestAdvisorSummaryRoleGating(t *testing.T) {
	s := advisorTestServer(t)
	s.SetKnowledgeAdvisorSummary(retro.NewAdvisorDecisionStore(""), nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/knowledge/advisor/summary", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/knowledge/advisor/summary", nil)
	req.Header.Set("Authorization", "Bearer dashboard-secret")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	for _, role := range []string{config.RoleRead, config.RoleReadWrite, config.RoleOwner} {
		if rec, _ := getAdvisorSummary(t, s, "/api/knowledge/advisor/summary", role); rec.Code != http.StatusOK {
			t.Fatalf("role %q = %d, want 200", role, rec.Code)
		}
	}
	if rec, _ := getAdvisorSummary(t, s, "/api/knowledge/advisor/summary", "stranger"); rec.Code != http.StatusForbidden {
		t.Fatalf("unknown role = %d, want 403", rec.Code)
	}

	// Read-only route: a write verb is not routed to it.
	rec = httptest.NewRecorder()
	s.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/knowledge/advisor/summary", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}
}
