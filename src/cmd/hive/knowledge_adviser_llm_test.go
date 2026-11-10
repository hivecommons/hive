package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// stubKnowledgeMatcher is the phase-1 adviser with a fixed fact list in
// place of the Primer.
type stubKnowledgeMatcher struct {
	cfg    func() *config.Config
	facts  []knowledge.Fact
	primed []string
	limits []int
}

func (m *stubKnowledgeMatcher) Policy(agent string) crux.KnowledgeAdvisorPolicy {
	return (&knowledgeAdviser{cfg: m.cfg, logger: slog.Default()}).Policy(agent)
}

func (m *stubKnowledgeMatcher) Advise(agent string, scope crux.KnowledgeScope) []crux.KnowledgeAdvice {
	return m.adviseUpTo(agent, scope, knowledgeAdviseMaxPerScope)
}

func (m *stubKnowledgeMatcher) adviseUpTo(_ string, _ crux.KnowledgeScope, limit int) []crux.KnowledgeAdvice {
	m.limits = append(m.limits, limit)
	return selectKnowledgeAdvice(m.facts, m.primed, limit)
}

const llmTestBodyMarker = "BODY-TAIL-NEVER-IN-PROMPT"

func llmTestFacts() []knowledge.Fact {
	return []knowledge.Fact{
		{Slug: "kick-primed", Title: "Already in the kick", Body: "primed body"},
		{Slug: "first", Title: "Pin the tree", Body: "Run the pin script first. " + strings.Repeat("detail ", 80) + llmTestBodyMarker,
			Sources: []knowledge.Source{{DocURL: "https://wiki/first"}}},
		{Slug: "second", Title: "Rebase before merge", Body: "Rebase on the target branch."},
		{Slug: "third", Title: "Label the PR", Body: "Add the area label."},
	}
}

type llmFixture struct {
	d       *knowledgeAdviserLLM
	cfg     *config.Config
	matcher *stubKnowledgeMatcher
	srv     *httptest.Server
	logs    *bytes.Buffer
	clock   time.Time

	// mu guards what the model server's handler goroutine shares with the
	// test.
	mu      sync.Mutex
	calls   int
	reply   string
	block   bool
	handler http.HandlerFunc
	lastReq knowledgeAdviserLLMRequest
	kicks   map[string]scheduler.KickSummary
}

func (f *llmFixture) with(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func (f *llmFixture) setReply(reply string) { f.with(func() { f.reply = reply }) }

func (f *llmFixture) callCount() (n int) {
	f.with(func() { n = f.calls })
	return n
}

func (f *llmFixture) request() (req knowledgeAdviserLLMRequest) {
	f.with(func() { req = f.lastReq })
	return req
}

func (f *llmFixture) setKick(agent string, k scheduler.KickSummary) {
	f.with(func() { f.kicks[agent] = k })
}

func newLLMFixture(t *testing.T, a config.AdvisorLLMConfig) *llmFixture {
	t.Helper()
	f := &llmFixture{
		logs:  &bytes.Buffer{},
		clock: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC),
		kicks: map[string]scheduler.KickSummary{},
		reply: `{"slug": null, "reason": "just reading"}`,
	}
	release := make(chan struct{})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req knowledgeAdviserLLMRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.calls++
		f.lastReq = req
		handler, reply, block := f.handler, f.reply, f.block
		f.mu.Unlock()
		if handler != nil {
			handler(w, r)
			return
		}
		if block {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": reply}}},
			"usage":   map[string]any{"prompt_tokens": 410, "completion_tokens": 17},
		})
	}))
	t.Cleanup(func() { close(release); f.srv.Close() })

	t.Setenv("HIVE_TEST_ADVISOR_KEY", "test-key")
	a.Endpoint = f.srv.URL
	a.APIKeyEnv = "HIVE_TEST_ADVISOR_KEY"
	if a.Model == "" && a.Mode != "" {
		a.Model = "small-ranker"
	}
	f.cfg = &config.Config{Knowledge: config.KnowledgeConfig{Enabled: true, Advisor: true, AdvisorLLM: a}}
	cfgFn := func() *config.Config { return f.cfg }
	f.matcher = &stubKnowledgeMatcher{cfg: cfgFn, facts: llmTestFacts(), primed: []string{"kick-primed"}}
	now := func() time.Time { return f.clock }
	f.d = &knowledgeAdviserLLM{
		inner:      f.matcher,
		candidates: f.matcher.adviseUpTo,
		cfg:        cfgFn,
		kick: func(agent string) (k scheduler.KickSummary, ok bool) {
			f.with(func() { k, ok = f.kicks[agent] })
			return k, ok
		},
		http:      &http.Client{},
		now:       now,
		logger:    slog.New(slog.NewJSONHandler(f.logs, nil)),
		budget:    newKnowledgeAdviserLLMBudget(now),
		delivered: map[string]*knowledgeAdviserLLMKick{},
	}
	f.kicks["developer"] = scheduler.KickSummary{ID: "developer#1", Text: "#42 Fix the tree pin [kind/bug]"}
	return f
}

var llmTestScope = crux.KnowledgeScope{
	Kind: "path", Key: "src/pkg/tree", Path: "src/pkg/tree/pin.go", Keywords: []string{"tree"},
	Action: crux.KnowledgeActionEdit, PaneTail: "⏺ Edit(src/pkg/tree/pin.go)\n  token [REDACTED]",
}

// decisions returns the decoded "knowledge advisor: decision" log lines.
func (f *llmFixture) decisions(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == "knowledge advisor: decision" {
			out = append(out, rec)
		}
	}
	return out
}

func (f *llmFixture) lastDecision(t *testing.T) map[string]any {
	t.Helper()
	d := f.decisions(t)
	if len(d) == 0 {
		t.Fatalf("no decision logged; logs:\n%s", f.logs.String())
	}
	return d[len(d)-1]
}

func slugsOf(advice []crux.KnowledgeAdvice) []string {
	var out []string
	for _, a := range advice {
		out = append(out, a.Slug)
	}
	return out
}

// renderAdviceGolden renders advice the way the runtime types it, for the
// byte-identical comparison with phase 1.
func renderAdviceGolden(advice []crux.KnowledgeAdvice) string {
	var b strings.Builder
	for _, a := range advice {
		fmt.Fprintf(&b, "📚 Knowledge: %s — %s (full: %s)\n", a.Title, a.Summary, a.Ref)
	}
	return b.String()
}

func TestKnowledgeAdviserLLM_OffIsPhaseOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    config.AdvisorLLMConfig
	}{
		{"mode off", config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeOff}},
		{"no mode", config.AdvisorLLMConfig{}},
		{"rank without model", config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, Model: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLLMFixture(t, tc.a)
			if tc.a.Mode == config.AdvisorLLMModeRank {
				f.cfg.Knowledge.AdvisorLLM.Model = ""
			}
			phase1 := &stubKnowledgeMatcher{cfg: f.matcher.cfg, facts: llmTestFacts(), primed: []string{"kick-primed"}}
			want := renderAdviceGolden(phase1.Advise("developer", llmTestScope))
			got := renderAdviceGolden(f.d.Advise("developer", llmTestScope))
			if got != want {
				t.Errorf("delivered lines differ from phase 1:\n got %q\nwant %q", got, want)
			}
			if got == "" || strings.Count(got, "\n") != knowledgeAdviseMaxPerScope {
				t.Errorf("golden = %q, want %d phase-1 lines", got, knowledgeAdviseMaxPerScope)
			}
			if p, w := f.d.Policy("developer"), phase1.Policy("developer"); p != w {
				t.Errorf("policy = %+v, want phase 1 %+v", p, w)
			}
			if f.callCount() != 0 || len(f.decisions(t)) != 0 {
				t.Errorf("off made %d model calls and %d decisions", f.callCount(), len(f.decisions(t)))
			}
		})
	}
}

func TestKnowledgeAdviserLLM_PolicyWantsPaneTail(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, PaneLines: 40})
	p := f.d.Policy("developer")
	if !p.Enabled || !p.WantsPaneTail || p.PaneLines != 40 {
		t.Errorf("policy = %+v", p)
	}
	f.cfg.Knowledge.Advisor = false
	if p := f.d.Policy("developer"); p.Enabled || p.WantsPaneTail {
		t.Errorf("advisor off policy = %+v", p)
	}
}

func TestKnowledgeAdviserLLM_RankDeliversPick(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank})
	f.setReply("Sure: {\"slug\": \"second\", \"reason\": \"about to merge\"}")
	got := f.d.Advise("developer", llmTestScope)
	if s := slugsOf(got); len(s) != 1 || s[0] != "second" {
		t.Fatalf("advice = %v, want [second]", s)
	}
	if lim := f.matcher.limits; len(lim) != 1 || lim[0] != config.DefaultAdvisorLLMCandidates {
		t.Errorf("candidate limits asked = %v, want [%d]", lim, config.DefaultAdvisorLLMCandidates)
	}
	d := f.lastDecision(t)
	for k, want := range map[string]any{
		"agent": "developer", "kick": "developer#1", "scope_kind": "path", "scope_key": "src/pkg/tree",
		"scope_action": "edit", "candidates": "first,second,third", "pick": "second", "reason": "about to merge",
		"outcome": "delivered", "prompt_tokens": float64(410), "completion_tokens": float64(17),
	} {
		if d[k] != want {
			t.Errorf("decision %s = %v, want %v", k, d[k], want)
		}
	}
	if _, ok := d["latency_ms"]; !ok {
		t.Error("decision has no latency_ms")
	}

	// The same kick never gets the delivered slug again.
	f.setReply(`{"slug": "second", "reason": "again"}`)
	if got := f.d.Advise("developer", llmTestScope); len(got) != 0 {
		t.Errorf("re-delivered %v", slugsOf(got))
	}
	if d := f.lastDecision(t); d["candidates"] != "first,third" || d["outcome"] != "invalid" {
		t.Errorf("second decision = %v", d)
	}
}

func TestKnowledgeAdviserLLM_SlugOutsideCandidatesIsInvalid(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, OnError: config.AdvisorLLMOnErrorMatcher})
	for _, reply := range []string{
		`{"slug": "kick-primed", "reason": "x"}`,
		`{"slug": "invented-entry", "reason": "x"}`,
		`not json at all`,
	} {
		f.setReply(reply)
		if got := f.d.Advise("developer", llmTestScope); len(got) != 0 {
			t.Errorf("reply %q delivered %v", reply, slugsOf(got))
		}
		if d := f.lastDecision(t); d["outcome"] != "invalid" {
			t.Errorf("reply %q outcome = %v, want invalid", reply, d["outcome"])
		}
	}
}

func TestKnowledgeAdviserLLM_NullNeverFallsBack(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, OnError: config.AdvisorLLMOnErrorMatcher})
	f.setReply(`{"slug": null, "reason": "only grepping"}`)
	if got := f.d.Advise("developer", llmTestScope); len(got) != 0 {
		t.Errorf("null delivered %v", slugsOf(got))
	}
	if d := f.lastDecision(t); d["pick"] != "null" || d["outcome"] != "null" || d["reason"] != "only grepping" {
		t.Errorf("decision = %v", d)
	}
}

func TestKnowledgeAdviserLLM_Timeout(t *testing.T) {
	for _, tc := range []struct {
		onError string
		want    []string
		outcome string
	}{
		{config.AdvisorLLMOnErrorMatcher, []string{"first"}, "fallback-matcher"},
		{config.AdvisorLLMOnErrorSilence, nil, "timeout"},
	} {
		t.Run(tc.onError, func(t *testing.T) {
			f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, OnError: tc.onError})
			f.with(func() { f.block = true })
			f.d.http = &http.Client{Timeout: 50 * time.Millisecond}
			got := slugsOf(f.d.Advise("developer", llmTestScope))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("advice = %v, want %v", got, tc.want)
			}
			d := f.lastDecision(t)
			if d["outcome"] != tc.outcome || d["verdict"] != "timeout" {
				t.Errorf("decision = %v, want outcome %s verdict timeout", d, tc.outcome)
			}
		})
	}
}

func TestKnowledgeAdviserLLM_EndpointErrorFallsBackPerOnError(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank})
	f.with(func() {
		f.handler = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }
	})
	if got := f.d.Advise("developer", llmTestScope); len(got) != 0 {
		t.Errorf("silence delivered %v", slugsOf(got))
	}
	if d := f.lastDecision(t); d["outcome"] != "error" {
		t.Errorf("outcome = %v, want error", d["outcome"])
	}
}

func TestKnowledgeAdviserLLMURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.githubcopilot.com":                  "https://api.githubcopilot.com/v1/chat/completions",
		"https://x.example/v1":                           "https://x.example/v1/chat/completions",
		"https://x.example/v1/":                          "https://x.example/v1/chat/completions",
		"https://api.githubcopilot.com/chat/completions": "https://api.githubcopilot.com/chat/completions",
	} {
		if got := knowledgeAdviserLLMURL(in); got != want {
			t.Errorf("knowledgeAdviserLLMURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKnowledgeAdviserLLM_PerAgentHourBudget(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, OnError: config.AdvisorLLMOnErrorSilence})
	for i := 0; i < config.AdvisorLLMMaxCallsPerAgentHourCap; i++ {
		f.d.Advise("developer", llmTestScope)
		f.clock = f.clock.Add(time.Minute)
	}
	if f.callCount() != 6 {
		t.Fatalf("calls = %d, want 6", f.callCount())
	}
	if got := f.d.Advise("developer", llmTestScope); len(got) != 0 {
		t.Errorf("over budget delivered %v", slugsOf(got))
	}
	if f.callCount() != 6 {
		t.Errorf("7th call within the hour reached the model (calls = %d)", f.callCount())
	}
	if d := f.lastDecision(t); d["outcome"] != "over-budget" || d["budget"] != "agent-hour" {
		t.Errorf("decision = %v", d)
	}
	// Another agent still has its own allowance.
	f.setKick("scanner", scheduler.KickSummary{ID: "scanner#1"})
	f.d.Advise("scanner", llmTestScope)
	if f.callCount() != 7 {
		t.Errorf("other agent calls = %d, want 7", f.callCount())
	}
	// The window rolls: an hour after the first call, one slot frees up.
	f.clock = f.clock.Add(time.Hour - 5*time.Minute)
	f.d.Advise("developer", llmTestScope)
	if f.callCount() != 8 {
		t.Errorf("after the window rolled calls = %d, want 8", f.callCount())
	}
}

func TestKnowledgeAdviserLLM_FleetDayBudget(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, OnError: config.AdvisorLLMOnErrorMatcher})
	f.clock = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for i := 0; i < config.AdvisorLLMMaxCallsPerDayCap; i++ {
		agent := fmt.Sprintf("agent-%d", i/config.AdvisorLLMMaxCallsPerAgentHourCap)
		f.d.Advise(agent, llmTestScope)
	}
	if f.callCount() != 300 {
		t.Fatalf("calls = %d, want 300", f.callCount())
	}
	got := f.d.Advise("fresh-agent", llmTestScope)
	if f.callCount() != 300 {
		t.Errorf("301st fleet call reached the model (calls = %d)", f.callCount())
	}
	if d := f.lastDecision(t); d["outcome"] != "fallback-matcher" || d["verdict"] != "over-budget" || d["budget"] != "fleet-day" {
		t.Errorf("decision = %v", d)
	}
	if s := slugsOf(got); len(s) != 1 || s[0] != "first" {
		t.Errorf("on_error matcher over budget = %v, want [first]", s)
	}
	f.clock = time.Date(2026, 11, 9, 0, 0, 1, 0, time.UTC)
	f.d.Advise("fresh-agent", llmTestScope)
	if f.callCount() != 301 {
		t.Errorf("new UTC day calls = %d, want 301", f.callCount())
	}
}

func TestKnowledgeAdviserLLM_OverBudgetSilence(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, MaxCallsPerAgentHour: 1})
	f.d.Advise("developer", llmTestScope)
	if got := f.d.Advise("developer", llmTestScope); len(got) != 0 || f.callCount() != 1 {
		t.Errorf("advice = %v calls = %d", slugsOf(got), f.callCount())
	}
	if d := f.lastDecision(t); d["outcome"] != "over-budget" {
		t.Errorf("outcome = %v", d["outcome"])
	}
}

func TestKnowledgeAdviserLLM_ShadowDeliversMatcherTopAndRecordsPick(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeShadow})
	f.setReply(`{"slug": "third", "reason": "labelling next"}`)
	got := f.d.Advise("developer", llmTestScope)
	if s := slugsOf(got); len(s) != 1 || s[0] != "first" {
		t.Fatalf("shadow advice = %v, want phase-1 top-1 [first]", s)
	}
	if f.callCount() != 1 {
		t.Errorf("shadow calls = %d, want 1", f.callCount())
	}
	d := f.lastDecision(t)
	if d["mode"] != "shadow" || d["pick"] != "third" || d["delivered"] != "first" || d["outcome"] != "delivered" {
		t.Errorf("decision = %v", d)
	}
}

func TestKnowledgeAdviserLLM_RankHoldout(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank, HoldoutPercent: 100, OnError: config.AdvisorLLMOnErrorMatcher})
	f.setReply(`{"slug": "second", "reason": "about to merge"}`)
	if got := f.d.Advise("developer", llmTestScope); len(got) != 0 {
		t.Errorf("holdout delivered %v", slugsOf(got))
	}
	if f.callCount() != 1 {
		t.Errorf("holdout calls = %d, want 1", f.callCount())
	}
	if d := f.lastDecision(t); d["outcome"] != "held-out" || d["pick"] != "second" {
		t.Errorf("decision = %v", d)
	}
}

func TestKnowledgeAdvisorHoldoutIsPerKick(t *testing.T) {
	if knowledgeAdvisorHoldout("a", "a#1", 0) || !knowledgeAdvisorHoldout("a", "a#1", 100) {
		t.Fatal("0% / 100% arms wrong")
	}
	held := 0
	for i := 0; i < 1000; i++ {
		kick := fmt.Sprintf("developer#%d", i)
		h := knowledgeAdvisorHoldout("developer", kick, 50)
		if h != knowledgeAdvisorHoldout("developer", kick, 50) {
			t.Fatalf("kick %s changed arm", kick)
		}
		if h {
			held++
		}
	}
	if held < 400 || held > 600 {
		t.Errorf("50%% holdout put %d of 1000 kicks in the holdout arm", held)
	}
}

func TestKnowledgeAdviserLLM_GatedWhenKickMoves(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank})
	f.setReply(`{"slug": "second", "reason": "x"}`)
	f.with(func() {
		f.handler = func(w http.ResponseWriter, _ *http.Request) {
			f.setKick("developer", scheduler.KickSummary{ID: "developer#2"})
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"slug\":\"second\",\"reason\":\"x\"}"}}]}`))
		}
	})
	if got := f.d.Advise("developer", llmTestScope); len(got) != 0 {
		t.Errorf("stale kick delivered %v", slugsOf(got))
	}
	if d := f.lastDecision(t); d["outcome"] != "gated" {
		t.Errorf("outcome = %v", d["outcome"])
	}
}

func TestKnowledgeAdviserLLM_NoCandidatesNoCall(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank})
	f.matcher.facts = llmTestFacts()[:1] // only the kick-primed entry
	if got := f.d.Advise("developer", llmTestScope); got != nil {
		t.Errorf("advice = %v", got)
	}
	if f.callCount() != 0 || len(f.decisions(t)) != 0 {
		t.Errorf("zero candidates made %d calls", f.callCount())
	}
}

func TestKnowledgeAdviserLLM_Prompt(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank})
	scope := llmTestScope
	secret := "ghp_" + strings.Repeat("a", 36)
	scope.PaneTail = logscrub.RedactPaneTokens("$ gh pr view 7\nexport TOKEN=" + secret + "\n⏺ Edit(src/pkg/tree/pin.go)")
	f.d.Advise("developer", scope)
	req := f.request()
	if req.Temperature != 0 || req.MaxTokens != knowledgeAdviserLLMMaxTokens || req.User != "hive-knowledge-advisor:developer" || req.Model != "small-ranker" {
		t.Errorf("request = %+v", req)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d", len(req.Messages))
	}
	prompt := req.Messages[1].Content
	if !strings.Contains(prompt, "\n"+scope.PaneTail+"\n") {
		t.Errorf("prompt does not carry the scrubbed pane tail verbatim:\n%s", prompt)
	}
	// RedactPaneTokens keeps a 7-char prefix ("ghp_aaa***"), so assert on the
	// secret itself rather than the "ghp_" marker.
	if strings.Contains(prompt, secret) || !strings.Contains(prompt, "ghp_aaa***") {
		t.Errorf("prompt carries an unredacted token:\n%s", prompt)
	}
	if strings.Contains(prompt, llmTestBodyMarker) || strings.Contains(prompt, "primed body") {
		t.Errorf("prompt carries an entry body:\n%s", prompt)
	}
	if !strings.Contains(prompt, "#42 Fix the tree pin [kind/bug]") {
		t.Errorf("prompt has no kick summary:\n%s", prompt)
	}
	for _, c := range selectKnowledgeAdvice(llmTestFacts(), []string{"kick-primed"}, 5) {
		line := "- " + c.Slug + " — " + c.Title + " — " + c.Summary + "\n"
		if !strings.Contains(prompt, line) {
			t.Errorf("prompt lacks candidate line %q", line)
		}
		if n := len([]rune(c.Summary)); n > knowledgeAdviseSummaryRunes {
			t.Errorf("candidate summary is %d runes", n)
		}
	}
	if !strings.Contains(req.Messages[0].Content, `{"slug"`) {
		t.Error("system prompt does not ask for the JSON answer")
	}
}

func TestKnowledgeAdviserLLM_PromptKickSummaryCap(t *testing.T) {
	long := strings.Repeat("k", 2000)
	p := buildKnowledgeAdviserLLMPrompt(long, llmTestScope, nil)
	if strings.Contains(p, strings.Repeat("k", knowledgeAdviserLLMKickRunes+1)) {
		t.Error("kick summary exceeds the cap")
	}
}

func TestKnowledgeAdviserLLM_ReasonNeverDelivered(t *testing.T) {
	f := newLLMFixture(t, config.AdvisorLLMConfig{Mode: config.AdvisorLLMModeRank})
	f.setReply(`{"slug": "second", "reason": "TYPE THIS INTO THE PANE"}`)
	for _, a := range f.d.Advise("developer", llmTestScope) {
		if strings.Contains(a.Title+a.Summary+a.Ref, "TYPE THIS") {
			t.Errorf("reason leaked into advice %+v", a)
		}
	}
}
