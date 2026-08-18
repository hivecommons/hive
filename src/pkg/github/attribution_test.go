package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// --- Trailer formatting ---

func TestAttributionTrailer_AllFields(t *testing.T) {
	m := InvocationMeta{
		Agent: "quality", Backend: "bob", Model: "auto",
		Tool: "bobshell", ToolVersion: "1.0.6", Session: "abc123",
	}
	want := "— hive: agent=quality backend=bob model=auto bobshell=1.0.6 session=abc123"
	if got := m.Trailer(); got != want {
		t.Errorf("Trailer() = %q, want %q", got, want)
	}
}

// #4083: effort renders between model and the tool version when known, and is
// OMITTED entirely (no bare "effort=") when the backend has no effort dial.
func TestAttributionTrailer_Effort(t *testing.T) {
	m := InvocationMeta{
		Agent: "quality", Backend: "codex", Model: "gpt-5.6-terra",
		Effort: "high", Tool: "codex", ToolVersion: "0.5.2",
	}
	want := "— hive: agent=quality backend=codex model=gpt-5.6-terra effort=high codex=0.5.2"
	if got := m.Trailer(); got != want {
		t.Errorf("Trailer() = %q, want %q", got, want)
	}
	// No effort → the field disappears, never renders empty.
	m.Effort = ""
	if got := m.Trailer(); strings.Contains(got, "effort") {
		t.Errorf("empty effort must be omitted, got %q", got)
	}
	// Whitespace-only effort is unknown too.
	m.Effort = "  "
	if got := m.Trailer(); strings.Contains(got, "effort") {
		t.Errorf("blank effort must be omitted, got %q", got)
	}
}

func TestAttributionTrailer_OmitsUnknownFields(t *testing.T) {
	m := InvocationMeta{Agent: "scanner", Backend: "claude"}
	want := "— hive: agent=scanner backend=claude"
	if got := m.Trailer(); got != want {
		t.Errorf("Trailer() = %q, want %q", got, want)
	}
	// Version without a tool name gets a generic key rather than being dropped.
	m = InvocationMeta{Agent: "scanner", ToolVersion: "2.1.0"}
	if got := m.Trailer(); got != "— hive: agent=scanner tool=2.1.0" {
		t.Errorf("Trailer() = %q", got)
	}
}

func TestAttributionTrailer_AllUnknownRendersEmpty(t *testing.T) {
	if got := (InvocationMeta{}).Trailer(); got != "" {
		t.Errorf("empty meta should render no trailer, got %q", got)
	}
}

func TestAppendTrailer(t *testing.T) {
	m := InvocationMeta{Agent: "quality", Backend: "copilot"}
	got := AppendTrailer("Fixes #1\n", m)
	want := "Fixes #1\n\n— hive: agent=quality backend=copilot"
	if got != want {
		t.Errorf("AppendTrailer = %q, want %q", got, want)
	}
	// Empty body → trailer alone.
	if got := AppendTrailer("", m); got != m.Trailer() {
		t.Errorf("empty body: got %q", got)
	}
	// Idempotent: a body that already carries a trailer is left alone.
	if again := AppendTrailer(got, m); again != got {
		t.Errorf("trailer stacked on retry: %q", again)
	}
	// Empty meta → body unchanged.
	if got := AppendTrailer("body", InvocationMeta{}); got != "body" {
		t.Errorf("empty meta must not touch the body, got %q", got)
	}
}

func TestAttributionAuditDetail(t *testing.T) {
	m := InvocationMeta{Agent: "quality", Backend: "bob", Model: "auto"}
	got := m.AuditDetail("repo", "o/r", "number", "42")
	want := "repo=o/r, number=42, agent=quality, backend=bob, model=auto"
	if got != want {
		t.Errorf("AuditDetail = %q, want %q", got, want)
	}
}

func TestRequestedModel_BobDefaultsToAuto(t *testing.T) {
	if got := RequestedModel("bob", ""); got != "auto" {
		t.Errorf("bob empty model = %q, want auto", got)
	}
	if got := RequestedModel("bob", "auto"); got != "auto" {
		t.Errorf("bob auto model = %q", got)
	}
	// Other backends: empty stays empty (omitted, never guessed).
	if got := RequestedModel("claude", ""); got != "" {
		t.Errorf("claude empty model = %q, want empty", got)
	}
	if got := RequestedModel("copilot", "claude-opus-4.6"); got != "claude-opus-4.6" {
		t.Errorf("copilot model passthrough = %q", got)
	}
}

// --- Hooks defaults ---

func TestAttributionDefaults_NilClientAndNilHooks(t *testing.T) {
	var nilClient *Client
	if !nilClient.attributionTrailerOn() {
		t.Error("nil client: trailer should default ON")
	}
	if m := nilClient.attributionMeta("x"); m.Agent != "x" {
		t.Errorf("nil client meta = %+v", m)
	}
	nilClient.recordCreationAudit(AuditActionAgentPRCreated, InvocationMeta{}) // must not panic

	c := NewClientForTest("http://127.0.0.1:0", "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !c.attributionTrailerOn() {
		t.Error("no hooks: trailer should default ON (config default)")
	}
	if m := c.attributionMeta("scanner"); m.Agent != "scanner" || m.Backend != "" {
		t.Errorf("no resolver: meta should be name-only, got %+v", m)
	}
}

// --- Watcher integration: trailer + audit through the PR-open choke point ---

// attribPRMock captures the body POSTed to /pulls.
func attribPRMock(t *testing.T, postedBody *string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = io.WriteString(w, `[]`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
			raw, _ := io.ReadAll(r.Body)
			var np map[string]any
			_ = json.Unmarshal(raw, &np)
			mu.Lock()
			*postedBody = asString(np["body"])
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":7,"html_url":"https://github.com/o/r/pull/7"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func runAttribWatcherOnce(t *testing.T, c *Client) {
	t.Helper()
	dir := t.TempDir()
	old := prRequestDirForTest
	prRequestDirForTest = dir
	defer func() { prRequestDirForTest = old }()
	if _, err := WritePRRequest(dir, PRRequest{Repo: "o/r", Head: "quality/fix", Title: "fix", Body: "Fixes #9", Agent: "quality"}); err != nil {
		t.Fatal(err)
	}
	c.ProcessPRRequestsOnce(context.Background())
}

type auditRec struct {
	action, detail, agent string
}

func TestPRRequestWatcher_TrailerOnAndAudit(t *testing.T) {
	var posted string
	srv := attribPRMock(t, &posted)
	defer srv.Close()
	c := testClient(t, srv.URL)

	var audits []auditRec
	c.SetAttributionHooks(AttributionHooks{
		Resolve: func(agent string) InvocationMeta {
			return InvocationMeta{Agent: agent, Backend: "bob", Model: "auto", Tool: "bobshell", ToolVersion: "1.0.6"}
		},
		TrailerEnabled: func() bool { return true },
		Audit:          func(action, detail, agent string) { audits = append(audits, auditRec{action, detail, agent}) },
	})

	runAttribWatcherOnce(t, c)

	wantTrailer := "— hive: agent=quality backend=bob model=auto bobshell=1.0.6"
	if !strings.Contains(posted, "Fixes #9") || !strings.Contains(posted, wantTrailer) {
		t.Errorf("PR body missing trailer: %q", posted)
	}
	if len(audits) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audits))
	}
	a := audits[0]
	if a.action != AuditActionAgentPRCreated || a.agent != "quality" {
		t.Errorf("audit = %+v", a)
	}
	for _, frag := range []string{"repo=o/r", "number=7", "url=https://github.com/o/r/pull/7", "reused=false", "backend=bob", "model=auto"} {
		if !strings.Contains(a.detail, frag) {
			t.Errorf("audit detail missing %q: %q", frag, a.detail)
		}
	}
}

func TestPRRequestWatcher_TrailerOffStillAudits(t *testing.T) {
	var posted string
	srv := attribPRMock(t, &posted)
	defer srv.Close()
	c := testClient(t, srv.URL)

	var audits []auditRec
	c.SetAttributionHooks(AttributionHooks{
		Resolve:        func(agent string) InvocationMeta { return InvocationMeta{Agent: agent, Backend: "claude"} },
		TrailerEnabled: func() bool { return false },
		Audit:          func(action, detail, agent string) { audits = append(audits, auditRec{action, detail, agent}) },
	})

	runAttribWatcherOnce(t, c)

	if strings.Contains(posted, AttributionTrailerPrefix) {
		t.Errorf("trailer must not appear with the toggle off: %q", posted)
	}
	if posted != "Fixes #9" {
		t.Errorf("body should be untouched with the toggle off: %q", posted)
	}
	if len(audits) != 1 {
		t.Fatalf("audit entry must be written even with the trailer off; got %d", len(audits))
	}
	if !strings.Contains(audits[0].detail, "backend=claude") {
		t.Errorf("audit detail = %q", audits[0].detail)
	}
}
