package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

// radReviewAutoscaleServer serves a hive with two agents of role "reviewer",
// of which review.reviewer_agents names only "reviewer": the swarm dispatches
// no review to "adjudicator".
func radReviewAutoscaleServer(t *testing.T) *Server {
	t.Helper()
	s := covApiServer(t)
	cfg := s.deps.Config
	cfg.Agents["adjudicator"] = config.AgentConfig{Backend: "claude", Enabled: true, Role: "reviewer"}
	cfg.Agents["reviewer"] = config.AgentConfig{Backend: "claude", Enabled: true, Role: "reviewer"}
	cfg.Review.ReviewerAgents = []string{"reviewer"}
	return s
}

func radReviewAutoscaleGet(t *testing.T, s *Server) reviewerAutoscaleResolved {
	t.Helper()
	rec := doGetNoRole(s, "/api/config/review")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET review: expected 200, got %d", rec.Code)
	}
	var got struct {
		ReviewerAutoscale *reviewerAutoscaleResolved `json:"reviewer_autoscale"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ReviewerAutoscale == nil {
		t.Fatalf("GET review has no reviewer_autoscale block: %s", rec.Body.String())
	}
	return *got.ReviewerAutoscale
}

// GET /api/config/review says which agent autoscale scales, next to the one
// that is stored, and why when it scales none.
func TestReviewConfigGet_ReviewerAutoscaleResolvedAgent(t *testing.T) {
	s := radReviewAutoscaleServer(t)
	cfg := s.deps.Config

	cfg.Review.Reviewers.Autoscale = config.ReviewerAutoscaleConfig{Enabled: true, Max: 2}
	got := radReviewAutoscaleGet(t, s)
	if want := (reviewerAutoscaleResolved{Enabled: true, EffectiveAgent: "reviewer"}); got != want {
		t.Fatalf("no agent named: got %+v, want %+v", got, want)
	}

	cfg.Review.Reviewers.Autoscale.Agent = "adjudicator"
	got = radReviewAutoscaleGet(t, s)
	if !got.Enabled || got.Agent != "adjudicator" || got.EffectiveAgent != "" ||
		!strings.Contains(got.Reason, `"adjudicator"`) || !strings.Contains(got.Reason, "review.reviewer_agents") {
		t.Fatalf("named non-reviewer: got %+v, want no effective agent and a reason", got)
	}

	cfg.Review.Reviewers.Autoscale = config.ReviewerAutoscaleConfig{Agent: " reviewer "}
	got = radReviewAutoscaleGet(t, s)
	if want := (reviewerAutoscaleResolved{Agent: "reviewer", EffectiveAgent: "reviewer"}); got != want {
		t.Fatalf("autoscale off: got %+v, want %+v", got, want)
	}

	if got := reviewerAutoscaleResolution(nil); got != (reviewerAutoscaleResolved{}) {
		t.Fatalf("nil config: got %+v", got)
	}

	html := indexHTML(t)
	for _, want := range []string{`rv.reviewer_autoscale`, `id="review-reviewer-autoscale-lane"`, `res.effective_agent`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing the scaled-agent line piece %q", want)
		}
	}
}

// PUT /api/config/review refuses an autoscale agent the review swarm cannot
// dispatch to, before anything is saved.
func TestReviewConfigPut_ReviewerAutoscaleAgentMustTakeReviews(t *testing.T) {
	s := radReviewAutoscaleServer(t)
	cfg := s.deps.Config
	put := func(body map[string]any) (int, string) {
		rec := doPut(s, "/api/config/review", body)
		return rec.Code, rec.Body.String()
	}

	code, body := put(map[string]any{
		"max_parallel_reviews": 7,
		"reviewer_autoscale":   map[string]any{"enabled": true, "agent": "adjudicator"},
	})
	if code != http.StatusBadRequest || !strings.Contains(body, "adjudicator") || !strings.Contains(body, "review.reviewer_agents") {
		t.Fatalf("non-reviewer agent: got %d %s, want 400 naming the agent and the reviewer list", code, body)
	}
	if a := cfg.Review.Reviewers.Autoscale; a.Enabled || a.Agent != "" || cfg.Review.MaxParallelReviews == 7 {
		t.Fatalf("a refused request changed the config: %+v max_parallel_reviews=%d", a, cfg.Review.MaxParallelReviews)
	}
	if code, body := put(map[string]any{"reviewer_autoscale": map[string]any{"enabled": true, "agent": "ghost"}}); code != http.StatusBadRequest || !strings.Contains(body, "not a declared agent") {
		t.Fatalf("unknown agent: got %d %s", code, body)
	}

	if code, body := put(map[string]any{"reviewer_autoscale": map[string]any{"enabled": true, "agent": "reviewer"}}); code != http.StatusOK {
		t.Fatalf("reviewer agent: got %d %s", code, body)
	}
	if a := cfg.Review.Reviewers.Autoscale; !a.Enabled || a.Agent != "reviewer" {
		t.Fatalf("reviewer agent not saved: %+v", a)
	}

	// The agent is checked against the reviewer list the same request
	// leaves in place.
	if code, body := put(map[string]any{
		"reviewer_agents":    []string{"reviewer", "adjudicator"},
		"reviewer_autoscale": map[string]any{"agent": "adjudicator"},
	}); code != http.StatusOK {
		t.Fatalf("agent added to the reviewer list in the same request: got %d %s", code, body)
	}
	if code, body := put(map[string]any{
		"reviewer_agents":    []string{"reviewer"},
		"reviewer_autoscale": map[string]any{"agent": "adjudicator"},
	}); code != http.StatusBadRequest {
		t.Fatalf("agent dropped from the reviewer list in the same request: got %d %s", code, body)
	}
	if got := strings.Join(cfg.Review.ReviewerAgents, ","); got != "reviewer,adjudicator" {
		t.Fatalf("a refused request changed reviewer_agents: %s", got)
	}

	// A config that already names a non-reviewer (a hand-edited hive.yaml)
	// can still save other review settings; only a request that touches
	// the autoscale block is held to the rule.
	cfg.Review.ReviewerAgents = []string{"reviewer"}
	if code, body := put(map[string]any{"max_parallel_reviews": 3}); code != http.StatusOK {
		t.Fatalf("unrelated setting: got %d %s", code, body)
	}
	if code, _ := put(map[string]any{"reviewer_autoscale": map[string]any{"max": 3}}); code != http.StatusBadRequest {
		t.Fatalf("autoscale edit that keeps the non-reviewer agent: got %d, want 400", code)
	}
	// Clearing the name, or turning autoscale off, is always accepted.
	if code, body := put(map[string]any{"reviewer_autoscale": map[string]any{"enabled": false}}); code != http.StatusOK {
		t.Fatalf("turning autoscale off: got %d %s", code, body)
	}
	if code, body := put(map[string]any{"reviewer_autoscale": map[string]any{"enabled": true, "agent": ""}}); code != http.StatusOK {
		t.Fatalf("clearing the agent: got %d %s", code, body)
	}
	if got := radReviewAutoscaleGet(t, s); !got.Enabled || got.Agent != "" || got.EffectiveAgent != "reviewer" {
		t.Fatalf("after clearing the agent: %+v", got)
	}
}

// Through the admin MCP: review.settings hands the endpoint's refusal of a
// non-reviewer autoscale agent back unchanged, and settings_read section
// review shows which agent scales.
func TestAdminMCPReviewSettingsAutoscaleAgent(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	srv := newFullServer(t)
	srv.authToken = "secret"
	cfg := srv.deps.Config
	cfg.Agents["adjudicator"] = config.AgentConfig{Backend: "claude", Enabled: true, Role: "reviewer"}
	cfg.Agents["reviewer"] = config.AgentConfig{Backend: "claude", Enabled: true, Role: "reviewer"}
	cfg.Review.ReviewerAgents = []string{"reviewer"}
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	owner := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})

	const preview = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"review.settings","args":{"reviewer_autoscale":{"enabled":true,"agent":"adjudicator"}}}}}`
	id := confirmationID(t, owner(preview))
	got := owner(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"` + id + `"}}}`)
	if !strings.Contains(got, `"hive-refusal"`) || !strings.Contains(got, `"status_code":400`) ||
		!strings.Contains(got, "adjudicator") || !strings.Contains(got, "is not in review.reviewer_agents") {
		t.Fatalf("confirm = %s, want the endpoint's 400 as a hive refusal", got)
	}
	if as := cfg.Review.Reviewers.Autoscale; as.Enabled || as.Agent != "" {
		t.Fatalf("a refused write changed reviewer autoscale to %+v", as)
	}

	cfg.Review.Reviewers.Autoscale.Enabled = true
	reader := dashboardAdminMCPProvider{server: srv, role: config.RoleOwner, ownerVerified: true}
	data, err := reader.Read(context.Background(), adminmcp.ToolSettingsRead, map[string]any{"section": "review"})
	if err != nil {
		t.Fatalf("settings_read review: %v", err)
	}
	b, err := adminmcp.EncodeResult(data)
	if err != nil {
		t.Fatalf("settings_read review: encode: %v", err)
	}
	if !regexp.MustCompile(`effective_agent\\?":\s*\\?"reviewer\\?"`).Match(b) {
		t.Fatalf("settings_read review does not show the scaled agent: %s", b)
	}
}
