package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/cruxtest"
	"github.com/hivecommons/hive/pkg/config"
)

// POST /v1/knowledge/advise (hivecommons/hive#17157): the hive side of the
// sidecar knowledge adviser. These tests drive the real callback server over
// HTTP against the sidecar host's capability set, with a recording adviser
// standing in for the chain boot installs.

const (
	adviseTestAgent = "developer"
	adviseTestToken = "advise-test-bearer"
	adviseTestTail  = "first line\nsecond line\ntoken ghp_abcdefghijklmnopqrstuvwxyz0123456789 here\n"
)

// recordingKnowledgeAdviser is the adviser chain: it records every scope it is
// asked about and answers a fixed policy and advice list. When release is set
// Advise blocks on it after signalling entered.
type recordingKnowledgeAdviser struct {
	mu      sync.Mutex
	policy  crux.KnowledgeAdvisorPolicy
	advice  []crux.KnowledgeAdvice
	scopes  []crux.KnowledgeScope
	entered chan struct{}
	release chan struct{}
}

func (r *recordingKnowledgeAdviser) Policy(string) crux.KnowledgeAdvisorPolicy { return r.policy }

func (r *recordingKnowledgeAdviser) Advise(_ string, scope crux.KnowledgeScope) []crux.KnowledgeAdvice {
	r.mu.Lock()
	r.scopes = append(r.scopes, scope)
	r.mu.Unlock()
	if r.release != nil {
		r.entered <- struct{}{}
		<-r.release
	}
	return r.advice
}

func (r *recordingKnowledgeAdviser) asked() []crux.KnowledgeScope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.scopes)
}

type adviseHarness struct {
	t       *testing.T
	url     string
	client  *callback.Client
	adviser *recordingKnowledgeAdviser
	rt      *cruxtest.Recorder
	kickAt  time.Time
}

// newAdviseHarness serves the sidecar host's callback capabilities with
// knowledge.sidecar_advise set to sidecarAdvise. The runtime owns one agent,
// developer, in the kick that started at h.kickAt.
func newAdviseHarness(t *testing.T, sidecarAdvise bool, policy crux.KnowledgeAdvisorPolicy) *adviseHarness {
	t.Helper()
	rec := cruxtest.NewRecorder()
	kickAt := time.Date(2026, 11, 18, 12, 0, 0, 123456789, time.UTC)
	rec.Add(adviseTestAgent, "id-1", crux.StateRunning).LastKick = &kickAt
	host := newSidecarHost(&sidecarRuntimeFake{Runtime: rec}, slog.New(slog.DiscardHandler))
	adviser := &recordingKnowledgeAdviser{
		policy: policy,
		advice: []crux.KnowledgeAdvice{
			{Slug: "agent-manager-lock", Title: "Manager lock order", Summary: "Take m.mu before a.mu.", Ref: "knowledge entry agent-manager-lock"},
			{Slug: "", Title: "ignored: no slug"},
		},
	}
	host.SetKnowledgeAdviser(adviser)
	cfg := &config.Config{Knowledge: config.KnowledgeConfig{Enabled: true, Advisor: true, SidecarAdvise: sidecarAdvise}}
	caps := (&boot{agentMgr: host, cfg: cfg}).hiveCallbackCapabilities()
	srv := httptest.NewServer(callback.NewServer(caps, adviseTestToken).Handler())
	t.Cleanup(srv.Close)
	client, err := callback.NewClient(srv.URL, adviseTestToken)
	if err != nil {
		t.Fatal(err)
	}
	return &adviseHarness{t: t, url: srv.URL, client: client, adviser: adviser, rt: rec, kickAt: kickAt}
}

func (h *adviseHarness) request(key string) callback.KnowledgeAdviseRequest {
	return callback.KnowledgeAdviseRequest{
		Agent:  adviseTestAgent,
		KickAt: h.kickAt,
		Scope: callback.KnowledgeScope{
			Kind: "path", Key: key, Path: key + "/manager.go", Keywords: []string{"crux", "agent"}, Action: crux.KnowledgeActionEdit,
		},
	}
}

// startNewKick moves the agent into a kick that started at a later time.
func (h *adviseHarness) startNewKick() time.Time {
	h.t.Helper()
	next := h.kickAt.Add(time.Minute)
	h.rt.Procs[adviseTestAgent].LastKick = &next
	return next
}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	var se *api.StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error %v is not an *api.StatusError", err)
	}
	return se.Status
}

func rankPolicy() crux.KnowledgeAdvisorPolicy {
	return crux.KnowledgeAdvisorPolicy{Enabled: true, MaxPerKick: 3, Cooldown: 90 * time.Second, WantsPaneTail: true, PaneLines: 4}
}

func TestSidecarKnowledgeAdvise_KnobOffIsInvisible(t *testing.T) {
	h := newAdviseHarness(t, false, rankPolicy())
	ctx := context.Background()

	health, err := h.client.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(health.Capabilities, callback.CapabilityKnowledgeAdvise) {
		t.Errorf("knob off advertises the capability: %v", health.Capabilities)
	}
	pol, err := h.client.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pol.KnowledgeAdvise {
		t.Error("knob off: the policy snapshot says the route is on")
	}
	_, err = h.client.AdviseKnowledge(ctx, h.request("src/crux/agent"))
	if !errors.Is(err, callback.ErrNotConfigured) {
		t.Fatalf("knob off: err = %v, want not_configured", err)
	}
	if _, err = h.client.AdviseKnowledge(ctx, callback.KnowledgeAdviseRequest{Agent: adviseTestAgent, PolicyOnly: true}); !errors.Is(err, callback.ErrNotConfigured) {
		t.Fatalf("knob off, policy only: err = %v, want not_configured", err)
	}
	if n := len(h.adviser.asked()); n != 0 {
		t.Fatalf("knob off reached the adviser %d times", n)
	}
}

func TestSidecarKnowledgeAdvise_NeedsAdvisorAndKnowledge(t *testing.T) {
	for name, mutate := range map[string]func(*config.KnowledgeConfig){
		"advisor off":   func(k *config.KnowledgeConfig) { k.Advisor = false },
		"knowledge off": func(k *config.KnowledgeConfig) { k.Enabled = false },
	} {
		t.Run(name, func(t *testing.T) {
			svc := newSidecarKnowledgeAdvise(
				func() *config.Config {
					cfg := &config.Config{Knowledge: config.KnowledgeConfig{Enabled: true, Advisor: true, SidecarAdvise: true}}
					mutate(&cfg.Knowledge)
					return cfg
				},
				func() crux.KnowledgeAdviser { return &recordingKnowledgeAdviser{} },
				func(string) (*crux.Process, error) { return &crux.Process{}, nil },
			)
			if svc.Enabled() {
				t.Fatal("route is on without the advisor")
			}
		})
	}
	noAdviser := newSidecarKnowledgeAdvise(
		func() *config.Config {
			return &config.Config{Knowledge: config.KnowledgeConfig{Enabled: true, Advisor: true, SidecarAdvise: true}}
		},
		func() crux.KnowledgeAdviser { return nil },
		nil,
	)
	if noAdviser.Enabled() {
		t.Fatal("route is on before boot installed an adviser")
	}
	nilCfg := newSidecarKnowledgeAdvise(func() *config.Config { return nil }, func() crux.KnowledgeAdviser { return &recordingKnowledgeAdviser{} }, nil)
	if nilCfg.Enabled() {
		t.Fatal("route is on with no config")
	}
}

func TestSidecarKnowledgeAdvise_AdvertisedWhenOn(t *testing.T) {
	h := newAdviseHarness(t, true, rankPolicy())
	ctx := context.Background()
	health, err := h.client.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(health.Capabilities, callback.CapabilityKnowledgeAdvise) {
		t.Errorf("capability not advertised: %v", health.Capabilities)
	}
	pol, err := h.client.Policy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !pol.KnowledgeAdvise {
		t.Error("policy snapshot does not say the route is on")
	}
}

// The route refuses an unauthenticated call with the status every other
// callback route gives, and never reaches the adviser.
func TestSidecarKnowledgeAdvise_RefusesUnauthenticated(t *testing.T) {
	h := newAdviseHarness(t, true, rankPolicy())
	post := func(path, auth string) int {
		req, err := http.NewRequest(http.MethodPost, h.url+path, strings.NewReader(`{"agent":"developer"}`))
		if err != nil {
			t.Fatal(err)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	want := post(callback.PathUsage, "")
	for _, auth := range []string{"", "Bearer wrong", "Basic " + adviseTestToken} {
		if got := post(callback.PathKnowledgeAdvise, auth); got != http.StatusUnauthorized || got != want {
			t.Errorf("auth %q: status %d, other callbacks give %d, want %d", auth, got, want, http.StatusUnauthorized)
		}
	}
	if n := len(h.adviser.asked()); n != 0 {
		t.Fatalf("unauthenticated calls reached the adviser %d times", n)
	}
}

func TestSidecarKnowledgeAdvise_ServesAdviceAndRedactsPaneTail(t *testing.T) {
	h := newAdviseHarness(t, true, rankPolicy())
	req := h.request("src/crux/agent")
	req.Scope.PaneTail = adviseTestTail
	resp, err := h.client.AdviseKnowledge(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Advice) != 1 || resp.Advice[0].Slug != "agent-manager-lock" || resp.Advice[0].Title != "Manager lock order" ||
		resp.Advice[0].Summary != "Take m.mu before a.mu." || resp.Advice[0].Ref != "knowledge entry agent-manager-lock" {
		t.Fatalf("advice = %+v (an entry with no slug must be dropped)", resp.Advice)
	}
	want := callback.KnowledgeAdvisorPolicy{Enabled: true, MaxPerKick: 3, CooldownMS: 90000, WantsPaneTail: true, PaneLines: 4}
	if resp.Policy != want {
		t.Errorf("policy = %+v, want %+v", resp.Policy, want)
	}
	asked := h.adviser.asked()
	if len(asked) != 1 {
		t.Fatalf("adviser asked %d times", len(asked))
	}
	got := asked[0]
	if got.Key != "src/crux/agent" || got.Path != "src/crux/agent/manager.go" || got.Action != crux.KnowledgeActionEdit || !slices.Equal(got.Keywords, []string{"crux", "agent"}) {
		t.Errorf("scope = %+v", got)
	}
	if strings.Contains(got.PaneTail, "ghp_abcdefghijklmnopqrstuvwxyz") || !strings.Contains(got.PaneTail, "ghp_abc***") {
		t.Errorf("the hive did not scrub the pane tail on receipt: %q", got.PaneTail)
	}
}

// A wanted pane tail longer than pane_lines is a 400 and nothing is evaluated.
func TestSidecarKnowledgeAdvise_RejectsOversizePaneTail(t *testing.T) {
	h := newAdviseHarness(t, true, rankPolicy())
	req := h.request("src/crux/agent")
	req.Scope.PaneTail = strings.Repeat("line\n", 5) // pane_lines is 4
	_, err := h.client.AdviseKnowledge(context.Background(), req)
	if got := statusOf(t, err); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", got)
	}
	if n := len(h.adviser.asked()); n != 0 {
		t.Fatalf("an oversize request reached the adviser %d times", n)
	}
	// Exactly pane_lines lines, trailing newline and all, is fine.
	req.Scope.PaneTail = strings.Repeat("line\n", 4)
	if _, err := h.client.AdviseKnowledge(context.Background(), req); err != nil {
		t.Fatalf("pane_lines lines rejected: %v", err)
	}
}

// With no pane tail wanted, none crosses to the adviser.
func TestSidecarKnowledgeAdvise_DropsUnwantedPaneTail(t *testing.T) {
	policy := rankPolicy()
	policy.WantsPaneTail = false
	h := newAdviseHarness(t, true, policy)
	req := h.request("src/crux/agent")
	req.Scope.PaneTail = adviseTestTail
	if _, err := h.client.AdviseKnowledge(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if asked := h.adviser.asked(); len(asked) != 1 || asked[0].PaneTail != "" {
		t.Fatalf("scope = %+v, want no pane tail", asked)
	}
}

func TestSidecarKnowledgeAdvise_RejectsOversizeBody(t *testing.T) {
	h := newAdviseHarness(t, true, rankPolicy())
	body := `{"agent":"developer","scope":{"kind":"path","key":"a","pane_tail":"` + strings.Repeat("x", 80<<10) + `"}}`
	req, err := http.NewRequest(http.MethodPost, h.url+callback.PathKnowledgeAdvise, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adviseTestToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if n := len(h.adviser.asked()); n != 0 {
		t.Fatalf("an oversize body reached the adviser %d times", n)
	}
}

// The agent must belong to the calling runtime and the kick must be its
// current one; anything else is 403 and no advice.
func TestSidecarKnowledgeAdvise_ChecksAgentAndKickIdentity(t *testing.T) {
	h := newAdviseHarness(t, true, rankPolicy())
	ctx := context.Background()

	req := h.request("src/crux/agent")
	req.Agent = "someone-elses-agent"
	if _, err := h.client.AdviseKnowledge(ctx, req); !errors.Is(err, callback.ErrForbidden) {
		t.Errorf("unknown agent: err = %v, want forbidden", err)
	}
	if _, err := h.client.AdviseKnowledge(ctx, callback.KnowledgeAdviseRequest{Agent: "someone-elses-agent", PolicyOnly: true}); !errors.Is(err, callback.ErrForbidden) {
		t.Errorf("unknown agent, policy only: err = %v, want forbidden", err)
	}
	req = h.request("src/crux/agent")
	req.KickAt = h.kickAt.Add(-time.Hour)
	if _, err := h.client.AdviseKnowledge(ctx, req); !errors.Is(err, callback.ErrForbidden) {
		t.Errorf("stale kick: err = %v, want forbidden", err)
	}
	req.KickAt = time.Time{}
	if _, err := h.client.AdviseKnowledge(ctx, req); statusOf(t, err) != http.StatusBadRequest {
		t.Errorf("missing kick: err = %v, want 400", err)
	}
	if n := len(h.adviser.asked()); n != 0 {
		t.Fatalf("identity failures reached the adviser %d times", n)
	}

	// The same call is fine for the current kick, and refused once the agent
	// has moved on to the next one.
	if _, err := h.client.AdviseKnowledge(ctx, h.request("src/crux/agent")); err != nil {
		t.Fatalf("current kick refused: %v", err)
	}
	h.startNewKick()
	if _, err := h.client.AdviseKnowledge(ctx, h.request("src/crux/agent")); !errors.Is(err, callback.ErrForbidden) {
		t.Errorf("previous kick after the agent moved on: err = %v, want forbidden", err)
	}
}

func TestSidecarKnowledgeAdvise_RejectsMalformedScopes(t *testing.T) {
	h := newAdviseHarness(t, true, rankPolicy())
	long := strings.Repeat("a", knowledgeAdviseMaxKeyRunes+1)
	for name, mutate := range map[string]func(*callback.KnowledgeScope){
		"unknown kind":      func(s *callback.KnowledgeScope) { s.Kind = "url" },
		"empty key":         func(s *callback.KnowledgeScope) { s.Key = "" },
		"long key":          func(s *callback.KnowledgeScope) { s.Key = long },
		"multi-line key":    func(s *callback.KnowledgeScope) { s.Key = "a\nb" },
		"absolute path":     func(s *callback.KnowledgeScope) { s.Path = "/etc/passwd" },
		"home path":         func(s *callback.KnowledgeScope) { s.Path = "~/.ssh/id_rsa" },
		"parent path":       func(s *callback.KnowledgeScope) { s.Path = "src/../../secrets/key.pem" },
		"unknown action":    func(s *callback.KnowledgeScope) { s.Action = "delete" },
		"too many keywords": func(s *callback.KnowledgeScope) { s.Keywords = make([]string, knowledgeAdviseMaxKeywords+1) },
		"long keyword": func(s *callback.KnowledgeScope) {
			s.Keywords = []string{strings.Repeat("k", knowledgeAdviseMaxKeywordRunes+1)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := h.request("src/crux/agent")
			mutate(&req.Scope)
			_, err := h.client.AdviseKnowledge(context.Background(), req)
			if got := statusOf(t, err); got != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", got)
			}
		})
	}
	if n := len(h.adviser.asked()); n != 0 {
		t.Fatalf("malformed scopes reached the adviser %d times", n)
	}
}

func TestSidecarKnowledgeAdvise_PolicyOnlyEvaluatesNothing(t *testing.T) {
	h := newAdviseHarness(t, true, rankPolicy())
	// No kick and no scope: a policy request needs neither.
	resp, err := h.client.AdviseKnowledge(context.Background(), callback.KnowledgeAdviseRequest{Agent: adviseTestAgent, PolicyOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Advice) != 0 || !resp.Policy.Enabled || !resp.Policy.WantsPaneTail || resp.Policy.PaneLines != 4 {
		t.Fatalf("policy-only response = %+v", resp)
	}
	if n := len(h.adviser.asked()); n != 0 {
		t.Fatalf("a policy request evaluated %d scopes", n)
	}
}

// A pane tail wanted without a length gets the one default both ends use.
func TestSidecarKnowledgeAdvise_PolicyDefaultsPaneLines(t *testing.T) {
	h := newAdviseHarness(t, true, crux.KnowledgeAdvisorPolicy{Enabled: true, WantsPaneTail: true})
	resp, err := h.client.AdviseKnowledge(context.Background(), callback.KnowledgeAdviseRequest{Agent: adviseTestAgent, PolicyOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Policy.PaneLines != config.DefaultAdvisorLLMPaneLines {
		t.Fatalf("pane_lines = %d, want %d", resp.Policy.PaneLines, config.DefaultAdvisorLLMPaneLines)
	}
}

// A policy that is off for the agent (excluded, or the advisor off) means no
// evaluation and no advice, with the policy saying so.
func TestSidecarKnowledgeAdvise_DisabledPolicyGivesNoAdvice(t *testing.T) {
	h := newAdviseHarness(t, true, crux.KnowledgeAdvisorPolicy{})
	resp, err := h.client.AdviseKnowledge(context.Background(), h.request("src/crux/agent"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Advice) != 0 || resp.Policy.Enabled {
		t.Fatalf("response = %+v", resp)
	}
	if n := len(h.adviser.asked()); n != 0 {
		t.Fatalf("a disabled agent reached the adviser %d times", n)
	}
}

// A scope is evaluated at most twice per kick; a new kick starts over; other
// scopes are unaffected.
func TestSidecarKnowledgeAdvise_LimitsEvaluationsPerScopePerKick(t *testing.T) {
	h := newAdviseHarness(t, true, rankPolicy())
	ctx := context.Background()
	for i := 0; i < knowledgeAdviseMaxEvalsPerScope; i++ {
		if _, err := h.client.AdviseKnowledge(ctx, h.request("src/crux/agent")); err != nil {
			t.Fatalf("evaluation %d refused: %v", i+1, err)
		}
	}
	if _, err := h.client.AdviseKnowledge(ctx, h.request("src/crux/agent")); !errors.Is(err, callback.ErrRateLimited) {
		t.Fatalf("third evaluation: err = %v, want rate_limited", err)
	}
	if n := len(h.adviser.asked()); n != knowledgeAdviseMaxEvalsPerScope {
		t.Fatalf("adviser asked %d times, want %d", n, knowledgeAdviseMaxEvalsPerScope)
	}
	if _, err := h.client.AdviseKnowledge(ctx, h.request("src/pkg/config")); err != nil {
		t.Fatalf("another scope refused: %v", err)
	}
	next := h.startNewKick()
	req := h.request("src/crux/agent")
	req.KickAt = next
	if _, err := h.client.AdviseKnowledge(ctx, req); err != nil {
		t.Fatalf("new kick refused: %v", err)
	}
}

func TestSidecarKnowledgeAdvise_LimitsScopesPerKick(t *testing.T) {
	policy := rankPolicy()
	svc := newSidecarKnowledgeAdvise(
		func() *config.Config {
			return &config.Config{Knowledge: config.KnowledgeConfig{Enabled: true, Advisor: true, SidecarAdvise: true}}
		},
		func() crux.KnowledgeAdviser { return &recordingKnowledgeAdviser{policy: policy} },
		func(string) (*crux.Process, error) {
			kick := time.Unix(1, 0)
			return &crux.Process{LastKick: &kick}, nil
		},
	)
	req := callback.KnowledgeAdviseRequest{Agent: adviseTestAgent, KickAt: time.Unix(1, 0), Scope: callback.KnowledgeScope{Kind: "gh", Key: "gh:pr"}}
	for i := 0; i < knowledgeAdviseMaxScopesPerKick; i++ {
		req.Scope.Key = fmt.Sprintf("scope-%d", i)
		if _, err := svc.AdviseKnowledge(context.Background(), req); err != nil {
			t.Fatalf("scope %d refused: %v", i, err)
		}
	}
	req.Scope.Key = "one-scope-too-many"
	if _, err := svc.AdviseKnowledge(context.Background(), req); !errors.Is(err, callback.ErrRateLimited) {
		t.Fatalf("err = %v, want rate_limited", err)
	}
}

// One call in flight per agent: a second, whatever its scope, is refused while
// the first runs, and goes through once it finishes.
func TestSidecarKnowledgeAdvise_OneCallInFlightPerAgent(t *testing.T) {
	h := newAdviseHarness(t, true, rankPolicy())
	h.adviser.entered = make(chan struct{}, 1)
	h.adviser.release = make(chan struct{})
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		_, err := h.client.AdviseKnowledge(ctx, h.request("src/crux/agent"))
		done <- err
	}()
	select {
	case <-h.adviser.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first call never reached the adviser")
	}
	if _, err := h.client.AdviseKnowledge(ctx, h.request("src/pkg/config")); !errors.Is(err, callback.ErrRateLimited) {
		t.Fatalf("second call while one is in flight: err = %v, want rate_limited", err)
	}
	close(h.adviser.release)
	if err := <-done; err != nil {
		t.Fatalf("first call: %v", err)
	}
	if _, err := h.client.AdviseKnowledge(ctx, h.request("src/pkg/config")); err != nil {
		t.Fatalf("call after the first finished: %v", err)
	}
	// The refused call never counted as an evaluation of its scope.
	if n := len(h.adviser.asked()); n != 2 {
		t.Fatalf("adviser asked %d times, want 2", n)
	}
}

func TestSidecarKnowledgeAdvise_CapsAdvicePerResponse(t *testing.T) {
	h := newAdviseHarness(t, true, rankPolicy())
	var many []crux.KnowledgeAdvice
	for i := 0; i < knowledgeAdviseMaxAdvice+5; i++ {
		many = append(many, crux.KnowledgeAdvice{Slug: strings.Repeat("s", i+1), Title: "t"})
	}
	h.adviser.advice = many
	resp, err := h.client.AdviseKnowledge(context.Background(), h.request("src/crux/agent"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Advice) != knowledgeAdviseMaxAdvice {
		t.Fatalf("advice entries = %d, want %d", len(resp.Advice), knowledgeAdviseMaxAdvice)
	}
}

func TestKnowledgePaneLineCount(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int
	}{
		{"", 0}, {"\n\n", 0}, {"a", 1}, {"a\n", 1}, {"a\nb", 2}, {"a\nb\n", 2}, {"a\n\nb", 3},
	} {
		if got := knowledgePaneLineCount(tt.in); got != tt.want {
			t.Errorf("knowledgePaneLineCount(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// Free-text scope fields are redacted too, so a token-shaped path segment
// never reaches a log line or the model prompt.
func TestCruxKnowledgeScope_RedactsFreeText(t *testing.T) {
	token := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	sc, err := cruxKnowledgeScope(callback.KnowledgeScope{
		Kind: "path", Key: "src/" + token, Path: "src/" + token + "/a.go", Keywords: []string{token},
	}, callback.KnowledgeAdvisorPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	joined := sc.Key + sc.Path + strings.Join(sc.Keywords, ",")
	if strings.Contains(joined, token) {
		t.Fatalf("token survived: %s", joined)
	}
}
