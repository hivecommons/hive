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
		Agent: "quality", Backend: "codex", Model: "gpt-5.6-terra", Effort: "high",
		Tool: "codex", ToolVersion: "0.5.2", Session: "abc123",
	}
	want := "— hive: agent=quality backend=codex model=gpt-5.6-terra effort=high codex=0.5.2 session=abc123"
	if got := m.Trailer(); got != want {
		t.Errorf("Trailer() = %q, want %q", got, want)
	}
}

func TestAttributionTrailer_EffortVariations(t *testing.T) {
	// Effort present: emitted between model and tool
	mWithEffort := InvocationMeta{
		Agent: "quality", Backend: "agy", Model: "gemini-3.7-flash", Effort: "low", Tool: "agy", ToolVersion: "1.0.0",
	}
	wantWithEffort := "— hive: agent=quality backend=agy model=gemini-3.7-flash effort=low agy=1.0.0"
	if got := mWithEffort.Trailer(); got != wantWithEffort {
		t.Errorf("Trailer() with effort = %q, want %q", got, wantWithEffort)
	}

	// Effort absent: completely omitted, no bare "effort=" token
	mNoEffort := InvocationMeta{
		Agent: "quality", Backend: "claude", Model: "claude-sonnet-5", Tool: "claude", ToolVersion: "2.0.0",
	}
	wantNoEffort := "— hive: agent=quality backend=claude model=claude-sonnet-5 claude=2.0.0"
	if got := mNoEffort.Trailer(); got != wantNoEffort {
		t.Errorf("Trailer() without effort = %q, want %q", got, wantNoEffort)
	}
	if strings.Contains(mNoEffort.Trailer(), "effort") {
		t.Errorf("Trailer() should omit effort completely when empty, got %q", mNoEffort.Trailer())
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

	// A non-nil client with neither an audit sink nor a logger (e.g. a bare
	// NewClient(...) in a test) must not panic on the fallback branch — this is
	// the path QueuePRAutoMerge's new approve-audit exercised.
	bare := NewClient("t", "o", []string{"r"}, nil, "http://127.0.0.1:0")
	bare.recordCreationAudit(AuditActionPRReviewed, InvocationMeta{Agent: "governor"}) // must not panic

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
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/repos/o/r"):
			// DefaultBranch: the requests these tests write omit Base, so
			// CreatePR resolves it here (kubestellar/hive#4928).
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"name":"r","default_branch":"main"}`)
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

// reconcilePRMock returns a test server that serves GET and PATCH on /repos/o/r/pulls/12.
func reconcilePRMock(t *testing.T, initialBody string, editedBody *string, editCount *int) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	currentBody := initialBody
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/repos/o/r/pulls/12"):
			mu.Lock()
			b := currentBody
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   12,
				"html_url": "https://github.com/o/r/pull/12",
				"body":     b,
				"user":     map[string]any{"login": "contributor-user"},
				"base": map[string]any{
					"repo": map[string]any{"full_name": "o/r"},
				},
			})
		case (r.Method == "PATCH" || r.Method == "POST") && strings.HasSuffix(r.URL.Path, "/repos/o/r/pulls/12"):
			raw, _ := io.ReadAll(r.Body)
			var patch map[string]any
			_ = json.Unmarshal(raw, &patch)
			mu.Lock()
			if b, ok := patch["body"].(string); ok {
				currentBody = b
				if editedBody != nil {
					*editedBody = b
				}
			}
			if editCount != nil {
				*editCount++
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   12,
				"html_url": "https://github.com/o/r/pull/12",
				"body":     currentBody,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestReconcilePRAttribution_AppendsTrailer(t *testing.T) {
	var edited string
	var editCount int
	srv := reconcilePRMock(t, "Initial PR description\nFixes #10", &edited, &editCount)
	defer srv.Close()
	c := testClient(t, srv.URL)

	var audits []auditRec
	c.SetAttributionHooks(AttributionHooks{
		TrailerEnabled: func() bool { return true },
		Audit:          func(action, detail, agent string) { audits = append(audits, auditRec{action, detail, agent}) },
	})

	meta := InvocationMeta{
		Agent:       "quality",
		Backend:     "codex",
		Model:       "gpt-5.6-terra",
		Effort:      "high",
		Tool:        "codex",
		ToolVersion: "0.5.2",
	}

	err := c.ReconcilePRAttribution(context.Background(), "https://github.com/o/r/pull/12", meta)
	if err != nil {
		t.Fatalf("ReconcilePRAttribution failed: %v", err)
	}

	if editCount != 1 {
		t.Fatalf("expected 1 edit call, got %d", editCount)
	}
	wantTrailer := "— hive: agent=quality backend=codex model=gpt-5.6-terra effort=high codex=0.5.2"
	if !strings.Contains(edited, wantTrailer) {
		t.Errorf("edited body missing trailer: %q", edited)
	}
	if !strings.Contains(edited, "Initial PR description\nFixes #10") {
		t.Errorf("edited body missing original description: %q", edited)
	}

	if len(audits) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audits))
	}
	if audits[0].action != AuditActionPRAttributionReconciled {
		t.Errorf("audit action = %q, want %q", audits[0].action, AuditActionPRAttributionReconciled)
	}
	// Must NOT be the creation action: the hive did not create this PR, and the
	// audit log's create→merge loop would count it as one that never happened.
	if audits[0].action == AuditActionAgentPRCreated {
		t.Error("a reconciled contributor PR must not be audited as a hive PR creation")
	}
	if !strings.Contains(audits[0].detail, "effort=high") || !strings.Contains(audits[0].detail, "reconciled=true") {
		t.Errorf("audit detail = %q", audits[0].detail)
	}
}

func TestReconcilePRAttribution_IdempotentAlreadyHasTrailer(t *testing.T) {
	var edited string
	var editCount int
	initial := "Fixes #10\n\n— hive: agent=quality backend=codex model=gpt-5.6-terra effort=high codex=0.5.2"
	srv := reconcilePRMock(t, initial, &edited, &editCount)
	defer srv.Close()
	c := testClient(t, srv.URL)

	meta := InvocationMeta{
		Agent:   "quality",
		Backend: "codex",
		Model:   "gpt-5.6-terra",
		Effort:  "high",
	}

	err := c.ReconcilePRAttribution(context.Background(), "https://github.com/o/r/pull/12", meta)
	if err != nil {
		t.Fatalf("ReconcilePRAttribution failed: %v", err)
	}

	// Body already had trailer, so editCount must remain 0 (no edit API call).
	if editCount != 0 {
		t.Errorf("expected 0 edit calls when trailer already present, got %d", editCount)
	}
}

func TestReconcilePRAttribution_AuditsOnlyWhenAnEditLands(t *testing.T) {
	// The reconcile path audits only when it actually edits a PR. This is
	// deliberately UNLIKE the creation path (pr_request_watcher), which audits
	// unconditionally: there a PR is created whether or not the visible trailer
	// is enabled, so there is always a real event. Here a disabled toggle, an
	// already-trailered body, or a failed API call all mean the hive did nothing,
	// and auditing "reconciled=true" for those records events that never
	// happened.
	meta := InvocationMeta{Agent: "quality", Backend: "claude", Model: "claude-sonnet-5"}

	t.Run("trailer disabled", func(t *testing.T) {
		var edited string
		var editCount int
		srv := reconcilePRMock(t, "Fixes #10", &edited, &editCount)
		defer srv.Close()
		c := testClient(t, srv.URL)

		var audits []auditRec
		c.SetAttributionHooks(AttributionHooks{
			TrailerEnabled: func() bool { return false },
			Audit:          func(action, detail, agent string) { audits = append(audits, auditRec{action, detail, agent}) },
		})

		if err := c.ReconcilePRAttribution(context.Background(), "https://github.com/o/r/pull/12", meta); err != nil {
			t.Fatalf("ReconcilePRAttribution failed: %v", err)
		}
		if editCount != 0 {
			t.Errorf("expected 0 edit calls with trailer disabled, got %d", editCount)
		}
		if len(audits) != 0 {
			t.Errorf("nothing was reconciled, so nothing should be audited; got %d entries: %+v", len(audits), audits)
		}
	})

	t.Run("body already carries a trailer", func(t *testing.T) {
		var edited string
		var editCount int
		initial := "Fixes #10\n\n" + AttributionTrailerPrefix + " agent=quality backend=claude model=claude-sonnet-5"
		srv := reconcilePRMock(t, initial, &edited, &editCount)
		defer srv.Close()
		c := testClient(t, srv.URL)

		var audits []auditRec
		c.SetAttributionHooks(AttributionHooks{
			TrailerEnabled: func() bool { return true },
			Audit:          func(action, detail, agent string) { audits = append(audits, auditRec{action, detail, agent}) },
		})

		if err := c.ReconcilePRAttribution(context.Background(), "https://github.com/o/r/pull/12", meta); err != nil {
			t.Fatalf("ReconcilePRAttribution failed: %v", err)
		}
		if editCount != 0 {
			t.Errorf("expected 0 edit calls when the trailer is already present, got %d", editCount)
		}
		if len(audits) != 0 {
			t.Errorf("an idempotent no-op must not be audited; got %d entries: %+v", len(audits), audits)
		}
	})

	t.Run("the PR cannot be fetched", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		c := testClient(t, srv.URL)

		var audits []auditRec
		c.SetAttributionHooks(AttributionHooks{
			TrailerEnabled: func() bool { return true },
			Audit:          func(action, detail, agent string) { audits = append(audits, auditRec{action, detail, agent}) },
		})

		if err := c.ReconcilePRAttribution(context.Background(), "https://github.com/o/r/pull/12", meta); err == nil {
			t.Fatal("expected an error when the PR cannot be fetched")
		}
		if len(audits) != 0 {
			t.Errorf("a failed reconcile must not be audited as a success; got %d entries: %+v", len(audits), audits)
		}
	})
}
