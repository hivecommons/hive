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
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":null,"tool_calls":[{"function":{"name":"repo.read_file","arguments":{"path":"README.md"}}},{"id":"x2","function":{"name":"issue.read","arguments":"{\"number\":7}"}}]},"finish_reason":"tool_calls"}]}`)
	}}
	sa := a.server(t)
	r, _ := New([]Provider{prov("a", sa.URL)}, WithEnv(allKeys()))
	history := append(msgs(),
		turn.Message{Role: turn.RoleAssistant, ToolCalls: []turn.ToolCall{{ID: "c1", Name: "issue.read", Arguments: `{"number":1}`}}},
		turn.Message{Role: turn.RoleTool, ToolCallID: "c1", Name: "issue.read", Content: "body"},
		turn.Message{Role: turn.RoleSubagent, Name: "triage", Content: "done"},
	)
	tools := []toolapprove.ToolRequest{{Tool: "repo.read_file", Content: "Read a file", Arguments: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}}}, {Tool: "task.complete"}}
	got, err := r.Generate(context.Background(), history, tools)
	if err != nil {
		t.Fatal(err)
	}
	if got.StopReason != "tool_calls" || len(got.ToolCalls) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got.ToolCalls[0].ID != "call_0" || got.ToolCalls[0].Arguments != `{"path":"README.md"}` || got.ToolCalls[1].Arguments != `{"number":7}` {
		t.Fatalf("tool calls = %+v", got.ToolCalls)
	}

	sent := a.lastBody["messages"].([]any)
	asst := sent[2].(map[string]any)
	if asst["content"] != nil {
		t.Fatalf("assistant tool-call message content = %#v, want null", asst["content"])
	}
	if tc := asst["tool_calls"].([]any)[0].(map[string]any); tc["id"] != "c1" || tc["type"] != "function" {
		t.Fatalf("assistant tool_calls = %#v", tc)
	}
	if tool := sent[3].(map[string]any); tool["role"] != "tool" || tool["tool_call_id"] != "c1" {
		t.Fatalf("tool message = %#v", tool)
	}
	if sub := sent[4].(map[string]any); sub["role"] != "user" || !strings.HasPrefix(sub["content"].(string), "[subagent triage]") {
		t.Fatalf("subagent message = %#v", sub)
	}
	defs := a.lastBody["tools"].([]any)
	fn := defs[1].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "task.complete" || fn["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("default parameters = %#v", fn)
	}
}

func TestNewValidates(t *testing.T) {
	good := Provider{Name: "a", BaseURL: "https://x", Model: "m", APIKeyEnv: "K"}
	for _, tc := range []struct {
		name string
		ps   []Provider
	}{
		{"empty", nil},
		{"dup", []Provider{good, good}},
		{"no-key-env", []Provider{{Name: "a", BaseURL: "https://x", Model: "m"}}},
		{"bad-url", []Provider{{Name: "a", BaseURL: "x", Model: "m", APIKeyEnv: "K"}}},
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
