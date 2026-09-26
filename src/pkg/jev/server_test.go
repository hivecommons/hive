package jev

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

type recordedUsage struct {
	agent, model string
	in, out      int64
}

type fakeUsage struct {
	mu   sync.Mutex
	recs []recordedUsage
}

func (f *fakeUsage) Record(agent, model string, in, out int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, recordedUsage{agent, model, in, out})
}

type auditRec struct {
	actor, action, agent string
	fields               map[string]any
}

type fakeAudit struct {
	mu   sync.Mutex
	recs []auditRec
}

func (f *fakeAudit) Record(actor, action, agent string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recs = append(f.recs, auditRec{actor, action, agent, fields})
}

func newTestServer(t *testing.T, fp *fakeProvider, identity string, enabled map[string]bool, key string) (*Server, *fakeUsage, *fakeAudit) {
	usage := &fakeUsage{}
	audit := &fakeAudit{}
	srv := &Server{
		Identify: func(*http.Request) string { return identity },
		Enabled:  func(a string) bool { return enabled[a] },
		Key:      func() string { return key },
		Timeout:  2 * time.Second,
		Client:   newFakeClient(t, fp),
		Usage:    usage,
		Audit:    audit,
	}
	return srv, usage, audit
}

func post(h http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, DecidePath, bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const goodBody = `{"type":"choice","question":"dup?","options":["yes","no"]}`

// TestServer_RefusesUnidentifiedAndDisabled: no identity → 403; identified but
// jev_mode off → 403. Neither resolves the key nor touches the provider — that
// is the "default off: no network calls" acceptance.
func TestServer_RefusesUnidentifiedAndDisabled(t *testing.T) {
	fp := &fakeProvider{answer: `{}`}
	keyCalls := 0
	for _, tc := range []struct {
		name, identity string
		enabled        map[string]bool
		wantMsg        string
	}{
		{"unidentified", "", map[string]bool{"scanner": true}, "could not be identified"},
		{"disabled", "scanner", map[string]bool{"scanner": false}, "jev_mode is off for agent scanner"},
		{"unknown agent", "ghost", map[string]bool{"scanner": true}, "jev_mode is off for agent ghost"},
	} {
		srv, usage, audit := newTestServer(t, fp, tc.identity, tc.enabled, "k")
		srv.Key = func() string { keyCalls++; return "k" }
		rec := post(srv.Handler(), goodBody)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), tc.wantMsg) {
			t.Errorf("%s: got %d %s", tc.name, rec.Code, rec.Body.String())
		}
		if len(usage.recs) != 0 || len(audit.recs) != 0 {
			t.Errorf("%s: refused call must not be budgeted or audited", tc.name)
		}
	}
	if fp.calls != 0 || keyCalls != 0 {
		t.Errorf("refused calls reached the key (%d) or provider (%d)", keyCalls, fp.calls)
	}
}

func TestServer_NotReadyWithoutKey(t *testing.T) {
	fp := &fakeProvider{answer: `{}`}
	srv, _, _ := newTestServer(t, fp, "scanner", map[string]bool{"scanner": true}, "  ")
	rec := post(srv.Handler(), goodBody)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "JEV_API_KEY") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if fp.calls != 0 {
		t.Fatal("no key must mean no provider call")
	}
}

func TestServer_BadRequests(t *testing.T) {
	fp := &fakeProvider{answer: `{}`}
	srv, _, _ := newTestServer(t, fp, "scanner", map[string]bool{"scanner": true}, "k")
	for name, body := range map[string]string{
		"not json":   `{`,
		"one option": `{"type":"choice","question":"q","options":["a"]}`,
		"no type":    `{"question":"q"}`,
	} {
		rec := post(srv.Handler(), body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, DecidePath, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d", rec.Code)
	}
	if fp.calls != 0 {
		t.Fatal("invalid requests must not reach the provider")
	}
}

// TestServer_SuccessBudgetsAndAudits: a good call is answered, its input tokens
// land in the usage sink under the agent and model, and the audit entry carries
// type/confidence/tokens but never the question or state.
func TestServer_SuccessBudgetsAndAudits(t *testing.T) {
	fp := &fakeProvider{answer: `{"model":"typesafe/jev-test","answers":{"decision":{"type":"choice","choice":"yes","confidence":0.77,"probabilities":{"yes":0.8,"no":0.2}}},"usage":{"input_tokens":150}}`}
	srv, usage, audit := newTestServer(t, fp, "scanner", map[string]bool{"scanner": true}, "k-secret")
	rec := post(srv.Handler(), `{"type":"choice","question":"SECRET-QUESTION","options":["yes","no"],"state":{"body":"SECRET-STATE"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	var res Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Answer != "yes" || res.Confidence != 0.77 || res.InputTokens != 150 {
		t.Errorf("result = %+v", res)
	}
	if fp.lastAuth != "Bearer k-secret" {
		t.Errorf("provider auth = %q", fp.lastAuth)
	}
	if len(usage.recs) != 1 || usage.recs[0] != (recordedUsage{"scanner", "typesafe/jev-test", 150, 0}) {
		t.Errorf("usage = %+v", usage.recs)
	}
	if len(audit.recs) != 1 {
		t.Fatalf("audit = %+v", audit.recs)
	}
	a := audit.recs[0]
	if a.action != "jev_decision" || a.agent != "scanner" || a.fields["outcome"] != "success" || a.fields["question_type"] != "choice" || a.fields["input_tokens"] != 150 {
		t.Errorf("audit entry = %+v", a)
	}
	for _, v := range a.fields {
		if s, ok := v.(string); ok && strings.Contains(s, "SECRET") {
			t.Errorf("audit must not carry question/state content: %v", a.fields)
		}
	}
}

func TestServer_ProviderFailureAuditedNotBudgeted(t *testing.T) {
	fp := &fakeProvider{status: http.StatusBadGateway, answer: `upstream down`}
	srv, usage, audit := newTestServer(t, fp, "scanner", map[string]bool{"scanner": true}, "k")
	rec := post(srv.Handler(), goodBody)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "HTTP 502") {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if len(usage.recs) != 0 {
		t.Error("a failed call has no tokens to budget")
	}
	if len(audit.recs) != 1 || audit.recs[0].fields["outcome"] != "failure" {
		t.Errorf("audit = %+v", audit.recs)
	}
}

// TestServer_ProviderTimeoutIs504: a provider slower than Server.Timeout is
// cut off with 504 (not 502), audited as a failure and not budgeted.
func TestServer_ProviderTimeoutIs504(t *testing.T) {
	// The handler blocks until released; release before Close (LIFO defers) so
	// Close's wait for in-flight handlers cannot deadlock.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	defer close(release)
	usage, audit := &fakeUsage{}, &fakeAudit{}
	var logs bytes.Buffer
	srv := &Server{
		Identify: func(*http.Request) string { return "scanner" },
		Enabled:  func(string) bool { return true },
		Key:      func() string { return "k" },
		Timeout:  50 * time.Millisecond,
		Client:   NewClient(func() config.JevConfig { return config.JevConfig{Endpoint: slow.URL} }, slow.Client()),
		Usage:    usage,
		Audit:    audit,
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
	}
	rec := post(srv.Handler(), goodBody)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if len(usage.recs) != 0 || len(audit.recs) != 1 || audit.recs[0].fields["outcome"] != "failure" {
		t.Errorf("usage=%+v audit=%+v", usage.recs, audit.recs)
	}
	if !strings.Contains(logs.String(), "jev decision failed") || !strings.Contains(logs.String(), "agent=scanner") {
		t.Errorf("failure must be logged with the agent: %s", logs.String())
	}
}

// TestServer_MinimalWiring: with no Usage, no Audit and a zero Timeout the
// server still answers (default 5s bound) and logs the decision; nothing to
// budget or audit is not an error.
func TestServer_MinimalWiring(t *testing.T) {
	fp := &fakeProvider{answer: `{"answers":{"decision":{"type":"choice","choice":"yes","confidence":0.7}},"usage":{"input_tokens":3}}`}
	var logs bytes.Buffer
	srv := &Server{
		Identify: func(*http.Request) string { return "scanner" },
		Enabled:  func(string) bool { return true },
		Key:      func() string { return "k" },
		Client:   newFakeClient(t, fp),
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
	}
	rec := post(srv.Handler(), goodBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	var res Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil || res.Answer != "yes" {
		t.Errorf("body = %s (%v)", rec.Body.String(), err)
	}
	if !strings.Contains(logs.String(), "jev decision") || !strings.Contains(logs.String(), "input_tokens=3") {
		t.Errorf("decision must be logged with token count: %s", logs.String())
	}
}

// TestServer_ServeRoutesOnlyDecide: over a real listener the endpoint answers
// POST /v1/decide (here: 403, unidentified caller) and nothing else; closing
// the listener ends Serve. The ephemeral port keeps the test independent of
// whatever holds DecidePort on the host.
func TestServer_ServeRoutesOnlyDecide(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	srv := &Server{Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	base := "http://" + ln.Addr().String()

	resp, err := http.Post(base+DecidePath, "application/json", strings.NewReader(goodBody))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("unidentified caller: got %d, want 403", resp.StatusCode)
	}
	resp, err = http.Get(base + DecidePath)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: got %d, want 405", resp.StatusCode)
	}
	if !strings.Contains(logs.String(), "jev decision endpoint starting") || !strings.Contains(logs.String(), ln.Addr().String()) {
		t.Errorf("start must be logged with the bound addr: %s", logs.String())
	}

	_ = ln.Close()
	select {
	case err := <-errc:
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("Serve after close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after the listener closed")
	}
}
