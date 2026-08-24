package dashboard

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// newConfigHandlerServer builds the smallest Server able to exercise the
// governor-config handlers in api_config_automerge.go and
// api_config_review.go: a real audit log (NewServer), a Config with no
// SourcePath (so saveConfig is a no-op), and no-op refresh/persist funcs.
func newConfigHandlerServer(t *testing.T) *Server {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServer(0, logger)
	s.deps = &Dependencies{
		Config:         &config.Config{},
		Logger:         logger,
		Ctx:            context.Background(),
		RefreshFunc:    func() {},
		PersistFunc:    func() {},
		SkipReloadFunc: func() {},
	}
	return s
}

// ownerReq builds a request carrying the verified owner identity
// requireOwnerRole demands: the role header AND the server-set marker.
func ownerReq(method, target, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(ownerRoleVerifiedHeader, "true")
	return req
}

// ── owner gate ──────────────────────────────────────────────────────────────

// TestAutoMergeAndReviewConfigHandlersAreOwnerGated pins the F16/F22 contract
// on this surface: without a verified owner identity every gated handler must
// return 403 and must not touch the config. handleReviewConfigGet is the
// deliberate exception (the Review struct is secret-free and read-only).
func TestAutoMergeAndReviewConfigHandlersAreOwnerGated(t *testing.T) {
	handlers := []struct {
		name string
		call func(s *Server, w http.ResponseWriter, r *http.Request)
	}{
		{"handleAutoMergeGet", func(s *Server, w http.ResponseWriter, r *http.Request) { s.handleAutoMergeGet(w, r) }},
		{"handleAutoMergePut", func(s *Server, w http.ResponseWriter, r *http.Request) { s.handleAutoMergePut(w, r) }},
		{"handleReviewConfigPut", func(s *Server, w http.ResponseWriter, r *http.Request) { s.handleReviewConfigPut(w, r) }},
	}
	// Role header alone is not enough: the verified marker must also be set
	// (requireOwnerRole demands both, see #4299/F14).
	variants := []struct {
		name   string
		header func(r *http.Request)
	}{
		{"no identity", func(r *http.Request) {}},
		{"role without verified marker", func(r *http.Request) { r.Header.Set("X-Hive-Role", "owner") }},
		{"read-write role", func(r *http.Request) {
			r.Header.Set("X-Hive-Role", "read-write")
			r.Header.Set(ownerRoleVerifiedHeader, "true")
		}},
	}
	for _, h := range handlers {
		for _, v := range variants {
			t.Run(h.name+"/"+v.name, func(t *testing.T) {
				s := newConfigHandlerServer(t)
				s.deps.Config.AutoMerge.MaxMerges = 7
				s.deps.Config.Review.MaxParallelReviews = 7

				req := httptest.NewRequest(http.MethodPut, "/api/config/x", strings.NewReader(`{"max_merges":1,"max_parallel_reviews":1}`))
				v.header(req)
				w := httptest.NewRecorder()
				h.call(s, w, req)

				if w.Code != http.StatusForbidden {
					t.Fatalf("%s without owner identity = %d, want 403", h.name, w.Code)
				}
				if s.deps.Config.AutoMerge.MaxMerges != 7 || s.deps.Config.Review.MaxParallelReviews != 7 {
					t.Error("config mutated by a rejected request")
				}
			})
		}
	}
}

// ── auto-merge ──────────────────────────────────────────────────────────────

// TestAutoMergeGetRendersDefaults pins the tri-state rendering contract: an
// unset self_authored resolves to its effective default (enabled) while
// self_authored_set tells the UI no explicit choice was made, and a nil
// RequiredChecks renders as [] rather than null.
func TestAutoMergeGetRendersDefaults(t *testing.T) {
	s := newConfigHandlerServer(t)

	w := httptest.NewRecorder()
	s.handleAutoMergeGet(w, ownerReq(http.MethodGet, "/api/config/auto-merge", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", w.Code)
	}

	var resp struct {
		SelfAuthored    bool     `json:"self_authored"`
		SelfAuthoredSet bool     `json:"self_authored_set"`
		MaxMerges       int      `json:"max_merges"`
		RequiredChecks  []string `json:"required_checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.SelfAuthored || resp.SelfAuthoredSet {
		t.Errorf("unset self_authored should render effective default true with set=false; got %+v", resp)
	}
	if resp.RequiredChecks == nil {
		t.Error("required_checks must render as [], not null")
	}
	if !strings.Contains(w.Body.String(), `"required_checks":[]`) {
		t.Errorf("body = %s, want an explicit empty required_checks array", w.Body.String())
	}
}

// TestAutoMergeGetUnavailableWithoutConfig covers the 503 guard.
func TestAutoMergeGetUnavailableWithoutConfig(t *testing.T) {
	s := newConfigHandlerServer(t)
	s.deps.Config = nil

	w := httptest.NewRecorder()
	s.handleAutoMergeGet(w, ownerReq(http.MethodGet, "/api/config/auto-merge", ""))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET with nil config = %d, want 503", w.Code)
	}
}

// TestAutoMergePutPartialUpdate pins the "only what you send is changed"
// contract: absent keys leave settings untouched, present keys are applied,
// and required_checks entries are trimmed with blanks dropped.
func TestAutoMergePutPartialUpdate(t *testing.T) {
	s := newConfigHandlerServer(t)
	off := false
	s.deps.Config.AutoMerge.SelfAuthored = &off
	s.deps.Config.AutoMerge.MaxMerges = 3

	body := `{"required_checks":[" ci/build ","","  ","ci/test"]}`
	w := httptest.NewRecorder()
	s.handleAutoMergePut(w, ownerReq(http.MethodPut, "/api/config/auto-merge", body))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	am := s.deps.Config.AutoMerge
	if am.SelfAuthored == nil || *am.SelfAuthored {
		t.Error("absent self_authored key must leave the explicit false untouched")
	}
	if am.MaxMerges != 3 {
		t.Errorf("absent max_merges key mutated the value to %d, want 3", am.MaxMerges)
	}
	if len(am.RequiredChecks) != 2 || am.RequiredChecks[0] != "ci/build" || am.RequiredChecks[1] != "ci/test" {
		t.Errorf("required_checks = %v, want trimmed [ci/build ci/test]", am.RequiredChecks)
	}

	// The response reflects the explicit false and its set-ness.
	if !strings.Contains(w.Body.String(), `"self_authored":false`) ||
		!strings.Contains(w.Body.String(), `"self_authored_set":true`) {
		t.Errorf("response = %s, want the explicit self_authored=false echoed", w.Body.String())
	}
}

// TestAutoMergePutAppliesAllFields covers the full-body write path.
func TestAutoMergePutAppliesAllFields(t *testing.T) {
	s := newConfigHandlerServer(t)

	body := `{"self_authored":false,"max_merges":5,"required_checks":["ci"]}`
	w := httptest.NewRecorder()
	s.handleAutoMergePut(w, ownerReq(http.MethodPut, "/api/config/auto-merge", body))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", w.Code)
	}
	am := s.deps.Config.AutoMerge
	if am.SelfAuthored == nil || *am.SelfAuthored {
		t.Error("self_authored=false not applied")
	}
	if am.MaxMerges != 5 {
		t.Errorf("max_merges = %d, want 5", am.MaxMerges)
	}
	if len(am.RequiredChecks) != 1 || am.RequiredChecks[0] != "ci" {
		t.Errorf("required_checks = %v, want [ci]", am.RequiredChecks)
	}
}

// TestAutoMergePutValidation pins the validate-before-mutate contract: a
// negative max_merges or an undecodable body is a 400 and nothing changes.
func TestAutoMergePutValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"negative max_merges", `{"max_merges":-1,"self_authored":false}`},
		{"invalid body", `{not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newConfigHandlerServer(t)

			w := httptest.NewRecorder()
			s.handleAutoMergePut(w, ownerReq(http.MethodPut, "/api/config/auto-merge", tc.body))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s = %d, want 400", tc.name, w.Code)
			}
			if s.deps.Config.AutoMerge.SelfAuthored != nil {
				t.Error("rejected request must not mutate the config")
			}
		})
	}
}

// ── review gate ─────────────────────────────────────────────────────────────

// TestReviewConfigGetReturnsSection: the read side is intentionally ungated
// (the struct is secret-free) and returns the config as-is.
func TestReviewConfigGetReturnsSection(t *testing.T) {
	s := newConfigHandlerServer(t)
	s.deps.Config.Review = config.ReviewConfig{
		RequireApproval:    true,
		MaxParallelReviews: 4,
		FixerAgent:         "fixer",
	}

	w := httptest.NewRecorder()
	s.handleReviewConfigGet(w, httptest.NewRequest(http.MethodGet, "/api/config/review", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", w.Code)
	}
	var got config.ReviewConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.RequireApproval || got.MaxParallelReviews != 4 || got.FixerAgent != "fixer" {
		t.Errorf("GET returned %+v, want the configured section", got)
	}
}

// TestReviewConfigPutPartialUpdate pins the pointer-field contract: absent
// keys leave settings untouched, reviewer_agents entries are trimmed with
// blanks dropped, and fixer_agent is trimmed.
func TestReviewConfigPutPartialUpdate(t *testing.T) {
	s := newConfigHandlerServer(t)
	s.deps.Config.Review = config.ReviewConfig{
		RequireApproval:    true,
		FanOut:             true,
		MaxParallelReviews: 2,
	}

	body := `{"reviewer_agents":[" alpha ",""," beta"],"fixer_agent":"  fixer  "}`
	w := httptest.NewRecorder()
	s.handleReviewConfigPut(w, ownerReq(http.MethodPut, "/api/config/review", body))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	rv := s.deps.Config.Review
	if !rv.RequireApproval || !rv.FanOut || rv.MaxParallelReviews != 2 {
		t.Errorf("absent keys mutated settings: %+v", rv)
	}
	if len(rv.ReviewerAgents) != 2 || rv.ReviewerAgents[0] != "alpha" || rv.ReviewerAgents[1] != "beta" {
		t.Errorf("reviewer_agents = %v, want trimmed [alpha beta]", rv.ReviewerAgents)
	}
	if rv.FixerAgent != "fixer" {
		t.Errorf("fixer_agent = %q, want trimmed %q", rv.FixerAgent, "fixer")
	}
}

// TestReviewConfigPutAppliesBooleansAndCap covers the explicit-write path,
// including flipping require_approval off — the merge-eligibility switch the
// owner gate exists to protect.
func TestReviewConfigPutAppliesBooleansAndCap(t *testing.T) {
	s := newConfigHandlerServer(t)
	s.deps.Config.Review.RequireApproval = true

	body := `{"require_approval":false,"fan_out":true,"max_parallel_reviews":6}`
	w := httptest.NewRecorder()
	s.handleReviewConfigPut(w, ownerReq(http.MethodPut, "/api/config/review", body))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", w.Code)
	}
	rv := s.deps.Config.Review
	if rv.RequireApproval || !rv.FanOut || rv.MaxParallelReviews != 6 {
		t.Errorf("review config = %+v, want require_approval=false fan_out=true max=6", rv)
	}
}

// TestReviewConfigPutValidation: a negative max_parallel_reviews or an
// undecodable body is a 400 and nothing changes.
func TestReviewConfigPutValidation(t *testing.T) {
	for _, tc := range []struct {
		name, body string
	}{
		{"negative max_parallel_reviews", `{"max_parallel_reviews":-2,"fan_out":true}`},
		{"invalid body", `{not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newConfigHandlerServer(t)

			w := httptest.NewRecorder()
			s.handleReviewConfigPut(w, ownerReq(http.MethodPut, "/api/config/review", tc.body))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("PUT %s = %d, want 400", tc.name, w.Code)
			}
			if s.deps.Config.Review.FanOut {
				t.Error("rejected request must not mutate the config")
			}
		})
	}
}
