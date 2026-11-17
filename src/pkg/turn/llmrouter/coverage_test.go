package llmrouter

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cruxconfig "github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/toolapprove"
	"github.com/hivecommons/hive/pkg/turn"
)

func TestKeyFileAuthAndExtraHeaders(t *testing.T) {
	f := &fakeProvider{handler: okText("ok")}
	srv := f.server(t)
	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("filekey\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var gotHeader string
	p := Provider{Name: "f", BaseURL: srv.URL + "/v1", Model: "m", APIKeyEnv: "UNSET_ENV", APIKeyFile: keyFile,
		Headers: map[string]string{"X-Title": "hive"}}
	q := Provider{Name: "auth", BaseURL: srv.URL + "/v1", Model: "m",
		Auth: func() (string, map[string]string) { return "minted", map[string]string{"X-IBM-Project-ID": "p1"} }}
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotHeader = r.Header.Get("X-Title") + "|" + r.Header.Get("X-IBM-Project-ID")
		return http.DefaultTransport.RoundTrip(r)
	})}
	r, _ := New([]Provider{p}, WithEnv(env(nil)), WithHTTPClient(client))
	if _, err := r.Generate(context.Background(), msgs(), nil); err != nil {
		t.Fatal(err)
	}
	if f.lastAuth != "Bearer filekey" || gotHeader != "hive|" {
		t.Fatalf("auth=%q headers=%q", f.lastAuth, gotHeader)
	}
	r2, _ := New([]Provider{q}, WithHTTPClient(client))
	if _, err := r2.Generate(context.Background(), msgs(), nil); err != nil {
		t.Fatal(err)
	}
	if f.lastAuth != "Bearer minted" || gotHeader != "|p1" {
		t.Fatalf("auth=%q headers=%q", f.lastAuth, gotHeader)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTransportErrorFailsOverAndContextCancel(t *testing.T) {
	b := &fakeProvider{handler: okText("b")}
	sb := b.server(t)
	dead := Provider{Name: "dead", BaseURL: "http://127.0.0.1:1/v1", Model: "m"}
	r, _ := New([]Provider{dead, prov("b", sb.URL)}, WithEnv(allKeys()))
	if got, err := r.Generate(context.Background(), msgs(), nil); err != nil || got.Content != "b" {
		t.Fatalf("Generate = %+v, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Generate(ctx, msgs(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx err = %v", err)
	}
}

type failingLedger struct{ acquireErr, cooldownErr error }

func (l failingLedger) Acquire(string, int, time.Time) (bool, time.Time, error) {
	return l.acquireErr == nil, time.Time{}, l.acquireErr
}
func (l failingLedger) Cooldown(string, time.Time) error { return l.cooldownErr }

func TestLedgerErrorsSurface(t *testing.T) {
	a := &fakeProvider{handler: status(429, "")}
	sa := a.server(t)
	r, _ := New([]Provider{prov("a", sa.URL)}, WithEnv(allKeys()), WithLedger(failingLedger{acquireErr: errors.New("db down")}))
	if _, err := r.Generate(context.Background(), msgs(), nil); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("acquire err = %v", err)
	}
	r, _ = New([]Provider{prov("a", sa.URL)}, WithEnv(allKeys()), WithLedger(failingLedger{cooldownErr: errors.New("db down")}))
	if _, err := r.Generate(context.Background(), msgs(), nil); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("cooldown err = %v", err)
	}
}

func TestMemoryLedgerCooldownNeverShortens(t *testing.T) {
	l := NewMemoryLedger()
	_ = l.Cooldown("a", t0.Add(time.Hour))
	_ = l.Cooldown("a", t0.Add(time.Minute))
	if ok, until, _ := l.Acquire("a", 0, t0.Add(30*time.Minute)); ok || !until.Equal(t0.Add(time.Hour)) {
		t.Fatalf("ok=%v until=%v", ok, until)
	}
}

func TestParseResponseEdges(t *testing.T) {
	names := toolNames{}
	for name, body := range map[string]string{
		"bad json":    `{`,
		"no func":     `{"choices":[{"message":{"tool_calls":[{"id":"x","function":{"arguments":"{}"}}]}}]}`,
		"empty":       `{"choices":[{"message":{"content":""}}]}`,
		"error field": `{"error":{"message":"boom"}}`,
	} {
		if _, err := parseResponse([]byte(body), names); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
	for raw, want := range map[string]string{``: "{}", `null`: "{}", `""`: "{}", `"{\"a\":1}"`: `{"a":1}`, `{"a":1}`: `{"a":1}`} {
		if got := normalizeArguments([]byte(raw)); got != want {
			t.Errorf("normalizeArguments(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestBuildRequestBodyRejects(t *testing.T) {
	if _, err := buildRequestBody([]turn.Message{{Role: "weird"}}, nil, toolNames{}, ""); err == nil {
		t.Error("unknown role must be rejected")
	}
	if _, err := buildRequestBody(msgs(), []toolapprove.ToolRequest{{Tool: ""}}, toolNames{}, ""); err == nil {
		t.Error("nameless tool must be rejected")
	}
	if _, err := New([]Provider{{Name: "a", BaseURL: "https://x", Model: "m"}}, WithEnv(env(nil))); err != nil {
		t.Fatal(err)
	}
	// withModel tolerates a body it cannot parse.
	if got := withModel([]byte("not json"), "m", 0); string(got) != "not json" {
		t.Fatalf("withModel = %q", got)
	}
}

func TestCapacityErrorMessage(t *testing.T) {
	e := &CapacityError{RetryAt: t0, Attempts: []Attempt{{Provider: "a", Outcome: OutcomeRateLimited}}}
	if msg := e.Error(); !strings.Contains(msg, "a=rate_limited") || !strings.Contains(msg, "2026-11-16T12:00:00Z") {
		t.Fatalf("msg = %q", msg)
	}
	if (&CapacityError{}).Error() == "" {
		t.Fatal("empty message")
	}
}

func TestFromGatewayModelOverrideAndKeyless(t *testing.T) {
	p, err := FromGateway(cruxconfig.GatewayConfig{Name: "local", Endpoint: "http://vllm:8000", DefaultModel: "d"}, "override", 5)
	if err != nil || p.Model != "override" || p.Auth != nil || p.DailyRequestCap != 5 || p.hasKeySource() {
		t.Fatalf("p=%+v err=%v", p, err)
	}
}

func TestUntilUTCMidnightAndDailyQuotaMarkers(t *testing.T) {
	if d := untilUTCMidnight(t0); d != 12*time.Hour {
		t.Fatalf("d = %v", d)
	}
	if dailyQuotaBody("slow down please") || !dailyQuotaBody("Daily neuron allocation exhausted") {
		t.Fatal("dailyQuotaBody")
	}
	if modelOutputFault("bad request: messages[0].content must be a string") {
		t.Fatal("plain 400 misclassified as a model-output fault")
	}
}

func TestReadBodyErrorAfter200(t *testing.T) {
	a := &fakeProvider{handler: func(w http.ResponseWriter) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, `{"choices":`) // short body
	}}
	b := &fakeProvider{handler: okText("b")}
	sa, sb := a.server(t), b.server(t)
	r, _ := New([]Provider{prov("a", sa.URL), prov("b", sb.URL)}, WithEnv(allKeys()))
	if got, err := r.Generate(context.Background(), msgs(), nil); err != nil || got.Content != "b" {
		t.Fatalf("Generate = %+v, %v", got, err)
	}
}
