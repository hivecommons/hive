package llmrouter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/inferencehealth"
	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/turn"
)

type fakeProvider struct {
	mu       sync.Mutex
	calls    int
	lastBody map[string]any
	lastAuth string
	handler  func(w http.ResponseWriter)
}

func (f *fakeProvider) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls++
		f.lastAuth = r.Header.Get("Authorization")
		f.lastBody = map[string]any{}
		_ = json.Unmarshal(raw, &f.lastBody)
		f.mu.Unlock()
		f.handler(w)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func okText(text string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":`+jsonString(text)+`},"finish_reason":"stop"}]}`)
	}
}

func status(code int, retryAfter string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
	}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func prov(name, url string) Provider {
	return Provider{Name: name, BaseURL: url + "/v1", Model: name + "-model", APIKeyEnv: strings.ToUpper(name) + "_KEY"}
}

func allKeys() func(string) string {
	return env(map[string]string{"A_KEY": "ka", "B_KEY": "kb", "C_KEY": "kc"})
}

func msgs() []turn.Message {
	return []turn.Message{{Role: turn.RoleSystem, Content: "sys"}, {Role: turn.RoleUser, Content: "hi"}}
}

func TestFailoverOn429ThenCooldownSkipsProvider(t *testing.T) {
	a := &fakeProvider{handler: status(429, "120")}
	b := &fakeProvider{handler: okText("from b")}
	sa, sb := a.server(t), b.server(t)
	now := t0
	var attempts []Attempt
	r, err := New([]Provider{prov("a", sa.URL), prov("b", sb.URL)}, WithEnv(allKeys()), WithClock(func() time.Time { return now }), WithAttemptHook(func(at Attempt) { attempts = append(attempts, at) }))
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Generate(context.Background(), msgs(), nil)
	if err != nil || got.Content != "from b" {
		t.Fatalf("Generate = %+v, %v", got, err)
	}
	if b.lastAuth != "Bearer kb" || b.lastBody["model"] != "b-model" {
		t.Fatalf("b saw auth=%q model=%v", b.lastAuth, b.lastBody["model"])
	}
	if len(attempts) != 2 || attempts[0].Outcome != OutcomeRateLimited || attempts[1].Outcome != OutcomeServed {
		t.Fatalf("attempts = %+v", attempts)
	}
	// Inside the Retry-After window provider a is not called again.
	now = t0.Add(60 * time.Second)
	if _, err := r.Generate(context.Background(), msgs(), nil); err != nil {
		t.Fatal(err)
	}
	if a.calls != 1 {
		t.Fatalf("a called %d times during cooldown", a.calls)
	}
	// After it, a is retried first.
	now = t0.Add(121 * time.Second)
	a.handler = okText("from a")
	got, err = r.Generate(context.Background(), msgs(), nil)
	if err != nil || got.Content != "from a" {
		t.Fatalf("after cooldown = %+v, %v", got, err)
	}
}

func TestAllExhaustedReturnsCapacityErrorWithEarliestRetry(t *testing.T) {
	a := &fakeProvider{handler: status(429, "300")}
	b := &fakeProvider{handler: status(503, "")}
	sa, sb := a.server(t), b.server(t)
	r, _ := New([]Provider{prov("a", sa.URL), prov("b", sb.URL), prov("c", "https://unused.invalid")},
		WithEnv(env(map[string]string{"A_KEY": "ka", "B_KEY": "kb"})), WithClock(func() time.Time { return t0 }))
	_, err := r.Generate(context.Background(), msgs(), nil)
	var ce *CapacityError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want CapacityError", err)
	}
	if !errors.Is(err, ErrCapacityExhausted) || !errors.Is(err, turn.ErrInferenceCapacity) {
		t.Fatalf("err does not match capacity sentinels: %v", err)
	}
	if want := t0.Add(DefaultUnavailableCooldown); !ce.RetryAt.Equal(want) {
		t.Fatalf("RetryAt = %v, want %v", ce.RetryAt, want)
	}
	if len(ce.Attempts) != 3 || ce.Attempts[2].Outcome != OutcomeSkipped {
		t.Fatalf("attempts = %+v", ce.Attempts)
	}
}

func TestRejectedRequestDoesNotFailOver(t *testing.T) {
	a := &fakeProvider{handler: status(400, "")}
	b := &fakeProvider{handler: okText("never")}
	sa, sb := a.server(t), b.server(t)
	r, _ := New([]Provider{prov("a", sa.URL), prov("b", sb.URL)}, WithEnv(allKeys()))
	_, err := r.Generate(context.Background(), msgs(), nil)
	if err == nil || errors.Is(err, turn.ErrInferenceCapacity) {
		t.Fatalf("err = %v, want non-capacity rejection", err)
	}
	if b.calls != 0 {
		t.Fatal("rejected request was resent to the next provider")
	}
}

func TestGoneAuthAndMalformedFailOver(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    func(w http.ResponseWriter)
		want Outcome
	}{
		{"gone", status(404, ""), OutcomeGone},
		{"auth", status(401, ""), OutcomeAuth},
		{"malformed", func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"choices":[]}`) }, OutcomeMalformed},
		{"error-200", func(w http.ResponseWriter) { _, _ = io.WriteString(w, `{"error":{"message":"overloaded"}}`) }, OutcomeMalformed},
		{"degenerate", okText(strings.Repeat("!", 300)), OutcomeMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeProvider{handler: tc.h}
			b := &fakeProvider{handler: okText("ok")}
			sa, sb := a.server(t), b.server(t)
			var first Attempt
			r, _ := New([]Provider{prov("a", sa.URL), prov("b", sb.URL)}, WithEnv(allKeys()), WithAttemptHook(func(at Attempt) {
				if at.Provider == "a" {
					first = at
				}
			}))
			if got, err := r.Generate(context.Background(), msgs(), nil); err != nil || got.Content != "ok" {
				t.Fatalf("Generate = %+v, %v", got, err)
			}
			if first.Outcome != tc.want {
				t.Fatalf("outcome = %s, want %s", first.Outcome, tc.want)
			}
		})
	}
}

func TestDailyCapSkipsUntilUTCMidnight(t *testing.T) {
	a := &fakeProvider{handler: okText("a")}
	sa := a.server(t)
	p := prov("a", sa.URL)
	p.DailyRequestCap = 2
	now := t0
	r, _ := New([]Provider{p}, WithEnv(allKeys()), WithClock(func() time.Time { return now }))
	for i := 0; i < 2; i++ {
		if _, err := r.Generate(context.Background(), msgs(), nil); err != nil {
			t.Fatal(err)
		}
	}
	_, err := r.Generate(context.Background(), msgs(), nil)
	var ce *CapacityError
	if !errors.As(err, &ce) || !ce.RetryAt.Equal(time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("err = %v", err)
	}
	now = time.Date(2026, 11, 17, 0, 0, 1, 0, time.UTC)
	if _, err := r.Generate(context.Background(), msgs(), nil); err != nil {
		t.Fatalf("after midnight: %v", err)
	}
	if a.calls != 3 {
		t.Fatalf("calls = %d", a.calls)
	}
}

func TestWireTranslationOfToolsAndToolCalls(t *testing.T) {
	a := &fakeProvider{handler: func(w http.ResponseWriter) {
		// Arguments as a bare object and a missing id: both seen from
		// OpenAI-compatible servers in the wild.
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"function":{"name":"repo__read_file","arguments":{"path":"README.md"}}},{"id":"x2","function":{"name":"issue__read","arguments":"{\"number\":7}"}}]},"finish_reason":"tool_calls"}]}`)
	}}
	sa := a.server(t)
	r, _ := New([]Provider{prov("a", sa.URL)}, WithEnv(allKeys()))
	history := append(msgs(),
		turn.Message{Role: turn.RoleAssistant, ToolCalls: []turn.ToolCall{{ID: "c1", Name: "issue.read", Arguments: `{"number":1}`}}},
		turn.Message{Role: turn.RoleTool, ToolCallID: "c1", Name: "issue.read", Content: "body"},
		turn.Message{Role: turn.RoleSubagent, Name: "triage", Content: "done"},
	)
	tools := []toolapprove.ToolRequest{{Tool: "repo.read_file", Content: "Read a file", Arguments: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}, {Tool: "issue.read"}, {Tool: "task.complete"}}
	got, err := r.Generate(context.Background(), history, tools)
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != "tool_calls" || len(got.ToolCalls) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got.ToolCalls[0].ID != "call_0" || got.ToolCalls[0].Name != "repo.read_file" || got.ToolCalls[0].Arguments != `{"path":"README.md"}` ||
		got.ToolCalls[1].Name != "issue.read" || got.ToolCalls[1].Arguments != `{"number":7}` {
		t.Fatalf("tool calls = %+v", got.ToolCalls)
	}

	sent := a.lastBody["messages"].([]any)
	asst := sent[2].(map[string]any)
	if asst["content"] != "" {
		t.Fatalf("assistant tool-call message content = %#v, want \"\" (Workers AI rejects null)", asst["content"])
	}
	if tc := asst["tool_calls"].([]any)[0].(map[string]any); tc["id"] != "c1" || tc["type"] != "function" || tc["function"].(map[string]any)["name"] != "issue__read" {
		t.Fatalf("assistant tool_calls = %#v", tc)
	}
	if tool := sent[3].(map[string]any); tool["role"] != "tool" || tool["tool_call_id"] != "c1" {
		t.Fatalf("tool message = %#v", tool)
	}
	if sub := sent[4].(map[string]any); sub["role"] != "user" || !strings.HasPrefix(sub["content"].(string), "[subagent triage]") {
		t.Fatalf("subagent message = %#v", sub)
	}
	defs := a.lastBody["tools"].([]any)
	if n := defs[0].(map[string]any)["function"].(map[string]any)["name"]; n != "repo__read_file" {
		t.Fatalf("wire tool name = %v", n)
	}
	fn := defs[2].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "task__complete" || fn["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("default parameters = %#v", fn)
	}
}

func TestNewValidates(t *testing.T) {
	good := Provider{Name: "a", BaseURL: "https://x", Model: "m"}
	for _, tc := range []struct {
		name string
		ps   []Provider
	}{
		{"empty", nil},
		{"dup", []Provider{good, good}},
		{"bad-url", []Provider{{Name: "a", BaseURL: "x", Model: "m", APIKeyEnv: "K"}}},
		{"no-model", []Provider{{Name: "a", BaseURL: "https://x"}}},
		{"neg-cap", []Provider{{Name: "a", BaseURL: "https://x", Model: "m", APIKeyEnv: "K", DailyRequestCap: -1}}},
	} {
		if _, err := New(tc.ps); err == nil {
			t.Errorf("%s: want error", tc.name)
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	if d := parseRetryAfter("7", t0); d != 7*time.Second {
		t.Fatalf("seconds: %v", d)
	}
	if d := parseRetryAfter(t0.Add(90*time.Second).Format(http.TimeFormat), t0); d != 90*time.Second {
		t.Fatalf("date: %v", d)
	}
	if d := parseRetryAfter("soon", t0); d != 0 {
		t.Fatalf("junk: %v", d)
	}
}

func TestDailyQuota429CoolsDownUntilUTCMidnight(t *testing.T) {
	a := &fakeProvider{handler: func(w http.ResponseWriter) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"message":"Rate limit reached for model on requests per day (RPD): Limit 1000, Used 1000"}}`)
	}}
	b := &fakeProvider{handler: okText("b")}
	sa, sb := a.server(t), b.server(t)
	r, _ := New([]Provider{prov("a", sa.URL), prov("b", sb.URL)}, WithEnv(allKeys()), WithClock(func() time.Time { return t0 }))
	if _, err := r.Generate(context.Background(), msgs(), nil); err != nil {
		t.Fatal(err)
	}
	ok, until, _ := r.ledger.Acquire("a", 0, t0.Add(2*time.Hour))
	if ok || !until.Equal(time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily-quota cooldown: ok=%v until=%v", ok, until)
	}
}

func TestKeylessAndEmptyKeyProviders(t *testing.T) {
	a := &fakeProvider{handler: okText("never")}
	b := &fakeProvider{handler: okText("keyless")}
	sa, sb := a.server(t), b.server(t)
	pa := prov("a", sa.URL) // configured key source that resolves empty -> skipped
	pb := Provider{Name: "local-vllm", BaseURL: sb.URL + "/v1", Model: "m"}
	r, _ := New([]Provider{pa, pb}, WithEnv(env(nil)))
	got, err := r.Generate(context.Background(), msgs(), nil)
	if err != nil || got.Content != "keyless" {
		t.Fatalf("Generate = %+v, %v", got, err)
	}
	if a.calls != 0 || b.lastAuth != "" {
		t.Fatalf("a.calls=%d keyless auth=%q", a.calls, b.lastAuth)
	}
}

func TestFromGatewayUsesGatewayKeyAndSharedURLRule(t *testing.T) {
	f := &fakeProvider{handler: okText("gw")}
	srv := f.server(t)
	t.Setenv("LLMROUTER_TEST_GW_KEY", "gwkey")
	// Endpoint without /v1: the shared rule appends /v1/chat/completions,
	// exactly as the inference translator does.
	p, err := FromGateway(cruxconfig.GatewayConfig{Name: "workers-ai", Kind: cruxconfig.GatewayKindCustom, Endpoint: srv.URL, APIKeyEnv: "LLMROUTER_TEST_GW_KEY", DefaultModel: "@cf/x"}, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := New([]Provider{p})
	if got, err := r.Generate(context.Background(), msgs(), nil); err != nil || got.Content != "gw" {
		t.Fatalf("Generate = %+v, %v", got, err)
	}
	if f.lastAuth != "Bearer gwkey" || f.lastBody["model"] != "@cf/x" {
		t.Fatalf("auth=%q model=%v", f.lastAuth, f.lastBody["model"])
	}
	if _, err := FromGateway(cruxconfig.GatewayConfig{Name: "g", Endpoint: "https://x"}, "", 0); err == nil {
		t.Fatal("gateway with no model must not validate")
	}
}

func TestHealthHookFeedsInferenceHealth(t *testing.T) {
	store := inferencehealth.NewStore()
	hook := HealthHook(store, func() time.Time { return t0 })
	hook(Attempt{Provider: "groq", Outcome: OutcomeRateLimited, Status: 429, Detail: "slow down"})
	hook(Attempt{Provider: "cf", Outcome: OutcomeUnavailable, Detail: "dial tcp: timeout"})
	hook(Attempt{Provider: "or", Outcome: OutcomeSkipped})
	snap := store.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	hook(Attempt{Provider: "groq", Outcome: OutcomeServed, Status: 200})
	for _, st := range store.Snapshot() {
		if st.Name == "groq" {
			t.Fatalf("served attempt did not clear groq: %+v", st)
		}
	}
}

func TestToolNameMapping(t *testing.T) {
	n, err := newToolNames([]toolapprove.ToolRequest{{Tool: "repo.read_file"}, {Tool: "task.complete"}, {Tool: "Bash"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for hive, wire := range map[string]string{"repo.read_file": "repo__read_file", "task.complete": "task__complete", "Bash": "Bash"} {
		if n.wire(hive) != wire || n.hive(wire) != hive {
			t.Errorf("%s <-> %s: got wire=%s hive=%s", hive, wire, n.wire(hive), n.hive(wire))
		}
	}
	if n.hive("unknown_tool") != "unknown_tool" {
		t.Error("unknown wire names must pass through unchanged")
	}
	if _, err := newToolNames([]toolapprove.ToolRequest{{Tool: "a.b"}, {Tool: "a__b"}}, nil); err == nil {
		t.Error("colliding wire names must be rejected")
	}
	if got := wireToolName("x y/" + strings.Repeat("z", 80)); len(got) != maxWireToolName || strings.ContainsAny(got, " /") {
		t.Errorf("sanitised = %q", got)
	}
}

func TestDegenerate(t *testing.T) {
	for s, want := range map[string]bool{
		strings.Repeat("!", 300):                 true,
		strings.Repeat("!", 40):                  false, // too short to judge
		strings.Repeat("ab", 100):                false,
		"The root cause is in dependency_bot.go": false,
		strings.Repeat("! ", 200) + "ok":         true,
	} {
		if got := degenerate(s); got != want {
			t.Errorf("degenerate(%.20q…) = %v, want %v", s, got, want)
		}
	}
}

// A finish phase offers only task.complete, but the model may still emit a
// name it saw earlier; it must map back to the Hive name (seen live:
// repo__search_code reached toolapprove unmapped).
func TestHistoryNamesMapBackWhenNotOffered(t *testing.T) {
	a := &fakeProvider{handler: func(w http.ResponseWriter) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"z","function":{"name":"repo__search_code","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	}}
	sa := a.server(t)
	r, _ := New([]Provider{prov("a", sa.URL)}, WithEnv(allKeys()), WithToolChoice("required"))
	history := append(msgs(),
		turn.Message{Role: turn.RoleAssistant, ToolCalls: []turn.ToolCall{{ID: "c1", Name: "repo.search_code", Arguments: `{}`}}},
		turn.Message{Role: turn.RoleTool, ToolCallID: "c1", Name: "repo.search_code", Content: "hits"},
	)
	got, err := r.Generate(context.Background(), history, []toolapprove.ToolRequest{{Tool: "task.complete"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.ToolCalls[0].Name != "repo.search_code" {
		t.Fatalf("name = %q, want repo.search_code", got.ToolCalls[0].Name)
	}
	if a.lastBody["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v", a.lastBody["tool_choice"])
	}
}

// A 400 that blames the model's own tool-call output fails over (Workers AI
// code 3030 seen live); a plain 400 still does not (TestRejectedRequestDoesNotFailOver).
func TestModelOutputFault400FailsOver(t *testing.T) {
	a := &fakeProvider{handler: func(w http.ResponseWriter) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"errors":[{"message":"AiError: 1 validation error for list[function-wrap[__log_extra_fields__()]] Invalid JSON: expected value [type=json_invalid, input_value='<tool_call>repo__list_di...</tool_call>']","code":3030}]}`)
	}}
	b := &fakeProvider{handler: okText("b")}
	sa, sb := a.server(t), b.server(t)
	var first Attempt
	r, _ := New([]Provider{prov("a", sa.URL), prov("b", sb.URL)}, WithEnv(allKeys()), WithAttemptHook(func(at Attempt) {
		if at.Provider == "a" {
			first = at
		}
	}))
	if got, err := r.Generate(context.Background(), msgs(), nil); err != nil || got.Content != "b" {
		t.Fatalf("Generate = %+v, %v", got, err)
	}
	if first.Outcome != OutcomeMalformed {
		t.Fatalf("outcome = %s", first.Outcome)
	}
}
