package adminmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type staticProvider struct{ seenTool string }

func (p *staticProvider) Read(_ context.Context, tool string, _ map[string]any) (any, error) {
	p.seenTool = tool
	return map[string]any{"status": "ok", "dashboard_token": "secret-value"}, nil
}

type errorProvider struct{ err error }

func (p errorProvider) Read(context.Context, string, map[string]any) (any, error) {
	return nil, p.err
}

func TestCapResultBoundsKnownListFields(t *testing.T) {
	input := map[string]any{"agents": []any{"a", "b", "c"}, "other": []any{"x", "y", "z"}}
	capped, ok := CapResult(input, 2).(map[string]any)
	if !ok {
		t.Fatalf("capped = %#v", capped)
	}
	agents, ok := capped["agents"].([]any)
	if !ok || len(agents) != 2 || capped["agents_truncated"] != true {
		t.Fatalf("agents cap = %#v", capped)
	}
	meta, ok := capped["_admin_mcp"].(map[string]any)
	if !ok || meta["truncated"] != true {
		t.Fatalf("missing truncation disclosure: %#v", capped)
	}
	other, ok := capped["other"].([]any)
	if !ok || len(other) != 3 {
		t.Fatalf("unexpected non-list cap = %#v", capped)
	}
}

func TestCapResultDisclosesTopLevelArrayTruncation(t *testing.T) {
	capped, ok := CapResult([]any{"a", "b", "c"}, 2).(map[string]any)
	if !ok {
		t.Fatalf("capped = %#v", capped)
	}
	items, ok := capped["items"].([]any)
	if !ok || len(items) != 2 || capped["truncated"] != true || capped["limit"] != 2 {
		t.Fatalf("top-level array cap = %#v", capped)
	}
}

func TestReadPathCoversPhaseTwoReadSurface(t *testing.T) {
	for _, tool := range []string{
		ToolFleetStatus,
		ToolVersionRead,
		ToolAgentsList,
		ToolLeasesList,
		ToolClaimsList,
		ToolPlansList,
		ToolAuditLog,
		ToolSettingsRead,
		ToolAutonomyReadiness,
		ToolSpendRead,
		ToolContributorsList,
		ToolKnowledgeRead,
		ToolHiveAdvisor,
		ToolAdvisorRecords,
		ToolLogRead,
		ToolLogSources,
	} {
		if !AllowedTool(tool) {
			t.Fatalf("%s is not allowed", tool)
		}
		if path, ok := ReadPath(tool, 2); !ok || path == "" {
			t.Fatalf("%s path = %q, %v", tool, path, ok)
		}
	}
}

func TestHandlerReadToolScrubsAndWraps(t *testing.T) {
	provider := &staticProvider{}
	h := NewHandler(provider)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hive_status","arguments":{}}}`))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	text := resultText(t, rec.Body.Bytes())
	if provider.seenTool != ToolHiveStatus {
		t.Fatalf("tool = %q", provider.seenTool)
	}
	if strings.Contains(text, "secret-value") || !strings.Contains(text, "[masked:token]") {
		t.Fatalf("scrubbed text = %s", text)
	}
}

func TestScrubCoversOutboundTextPatternsAndCounters(t *testing.T) {
	githubSession := "gh" + "s_" + strings.Repeat("a", 24)
	githubOAuth := "gh" + "o_" + strings.Repeat("b", 24)
	githubUser := "gh" + "u_" + strings.Repeat("c", 24)
	jwt := strings.Join([]string{"eyJ" + strings.Repeat("a", 22), strings.Repeat("b", 24), strings.Repeat("c", 24)}, ".")
	aws := "AKIA" + strings.Repeat("A", 16)
	bearer := "Bearer " + strings.Repeat("d", 20)
	privateKey := strings.Join([]string{"-----BEGIN PRIVATE KEY-----", strings.Repeat("A", 64), "-----END PRIVATE KEY-----"}, "\n")
	input := map[string]any{
		"audit_line":  "rotate " + githubSession + " " + githubOAuth + " " + githubUser + " " + jwt + " " + aws,
		"run_title":   "mid " + bearer + " value",
		"private_key": privateKey,
		"totalTokens": float64(1234),
		"tokens":      float64(5678),
	}
	out := Scrub(input).(map[string]any)
	data, _ := json.Marshal(out)
	text := string(data)
	for _, forbidden := range []string{githubSession, githubOAuth, githubUser, jwt, aws, bearer, privateKey} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("scrubbed output retained %q in %s", forbidden, text)
		}
	}
	for _, marker := range []string{"redacted:github-token", "redacted:jwt", "redacted:aws-access-key", "redacted:bearer-token", "redacted:private-key"} {
		if !strings.Contains(text, marker) {
			t.Fatalf("scrubbed output missing %q in %s", marker, text)
		}
	}
	if out["totalTokens"] != float64(1234) || out["tokens"] != float64(5678) {
		t.Fatalf("token counters changed: %#v", out)
	}
}

// Regression for #9161: token-count fields and hive identifiers are data the
// operator must see, not credentials.
func TestScrubKeepsTokenCountFieldsAndHiveIdentifiers(t *testing.T) {
	lease := "hive_mcp_v1." + strings.Repeat("p", 24) + "." + strings.Repeat("s", 24)
	input := map[string]any{
		"tokens": map[string]any{
			"lookbackHours": float64(24),
			"totals":        map[string]any{"input_tokens": float64(10), "output_tokens": float64(20)},
		},
		"total_tokens":         float64(42),
		"max_tokens":           float64(4096),
		"github_token_present": true,
		"hive_id":              "hive_status",
		"tool":                 "hive_advisor",
		"access_token":         "opaque-access",
		"refresh_tokens":       []any{"opaque-refresh"},
		"lease":                lease,
	}
	out := Scrub(input).(map[string]any)
	data, _ := json.Marshal(out)
	text := string(data)

	tokens, ok := out["tokens"].(map[string]any)
	if !ok || tokens["lookbackHours"] != float64(24) {
		t.Fatalf("tokens usage block masked: %s", text)
	}
	totals, _ := tokens["totals"].(map[string]any)
	if totals["input_tokens"] != float64(10) || totals["output_tokens"] != float64(20) {
		t.Fatalf("nested token counters masked: %s", text)
	}
	if out["total_tokens"] != float64(42) || out["max_tokens"] != float64(4096) {
		t.Fatalf("token counters masked: %s", text)
	}
	if out["github_token_present"] != true {
		t.Fatalf("boolean token flag masked: %s", text)
	}
	if out["hive_id"] != "hive_status" || out["tool"] != "hive_advisor" {
		t.Fatalf("hive_ identifiers masked: %s", text)
	}
	for _, forbidden := range []string{"opaque-access", "opaque-refresh", lease} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("credential %q leaked in %s", forbidden, text)
		}
	}
}

func TestScrubPendingKeepsBudgetTokenCount(t *testing.T) {
	out := scrubPendingMap(map[string]any{
		"details": map[string]any{"periodDays": 30, "totalTokens": 5000000},
		"tokens":  map[string]any{"input_tokens": 7},
		"token":   "opaque-secret",
	}).(map[string]any)
	details := out["details"].(map[string]any)
	if details["totalTokens"] != 5000000 {
		t.Fatalf("budget totalTokens masked in preview: %#v", out)
	}
	if tokens, ok := out["tokens"].(map[string]any); !ok || tokens["input_tokens"] != 7 {
		t.Fatalf("tokens block masked in preview: %#v", out)
	}
	if out["token"] != "[masked:token]" {
		t.Fatalf("credential token not masked: %#v", out)
	}
}

func TestHTTPErrorMessageIsScrubbed(t *testing.T) {
	githubToken := "gh" + "s_" + strings.Repeat("e", 24)
	bearer := "Bearer " + strings.Repeat("f", 20)
	aws := "AKIA" + strings.Repeat("G", 16)
	jwt := strings.Join([]string{"eyJ" + strings.Repeat("h", 22), strings.Repeat("i", 24), strings.Repeat("j", 24)}, ".")
	privateKey := strings.Join([]string{"-----BEGIN PRIVATE KEY-----", strings.Repeat("A", 64), "-----END PRIVATE KEY-----"}, "\n")
	h := NewHandler(errorProvider{err: fmt.Errorf("read failed for %s %s %s %s %s", githubToken, bearer, aws, jwt, privateKey)})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hive_status","arguments":{}}}`))
	h.ServeHTTP(rec, req)
	for _, forbidden := range []string{githubToken, bearer, aws, jwt, privateKey} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("rpc error body retained %q: %s", forbidden, rec.Body.String())
		}
	}
	if !strings.Contains(rec.Body.String(), "redacted:github-token") || !strings.Contains(rec.Body.String(), "redacted:bearer-token") {
		t.Fatalf("rpc error body = %s", rec.Body.String())
	}
}

func TestExclusionCatalogueIsAskable(t *testing.T) {
	h := NewHandler(nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"exclusion_catalogue","arguments":{}}}`))
	h.ServeHTTP(rec, req)
	text := resultText(t, rec.Body.Bytes())
	if !strings.Contains(text, "POST /api/self-upgrade") || !strings.Contains(text, RefusalKindMechanical) {
		t.Fatalf("catalogue text = %s", text)
	}
}

func TestRefusalReasonsMatchDesignDoc(t *testing.T) {
	doc, err := os.ReadFile("../../docs/design/admin-mcp.md")
	if err != nil {
		t.Fatal(err)
	}
	normalizedDoc := strings.ToLower(normalizeWhitespace(string(doc)))
	for _, ex := range Exclusions() {
		if !strings.Contains(normalizedDoc, strings.ToLower(normalizeWhitespace(ex.Operation))) {
			t.Fatalf("design doc does not record operation %q", ex.Operation)
		}
		if ex.Reason == "" {
			t.Fatalf("empty runtime reason for %q", ex.Operation)
		}
		for _, sentence := range strings.Split(ex.Reason, ".") {
			sentence = strings.TrimSpace(sentence)
			if len(sentence) < 24 {
				continue
			}
			if !strings.Contains(normalizedDoc, strings.ToLower(normalizeWhitespace(sentence))) {
				t.Fatalf("runtime reason for %q is not recorded in design doc: %q", ex.Operation, sentence)
			}
		}
	}
}

func resultText(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatalf("rpc error: %#v body=%s", resp.Error, body)
	}
	if len(resp.Result.Content) != 1 {
		t.Fatalf("content = %#v", resp.Result.Content)
	}
	return resp.Result.Content[0].Text
}

func normalizeWhitespace(s string) string { return strings.Join(strings.Fields(s), " ") }

type writeProvider struct {
	staticProvider
	requests []WriteRequest
	err      error
}

func (p *writeProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	p.requests = append(p.requests, req)
	if p.err != nil {
		return nil, p.err
	}
	return map[string]any{"ok": true}, nil
}

func TestWritePreviewDisabledByDefault(t *testing.T) {
	h := NewHandler(&writeProvider{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"agent.pause","args":{"agent":"scanner"}}}}`))
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), ErrWritesDisabled.Error()) {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestWriteConfirmationPersistsAndExecutesAfterRestart(t *testing.T) {
	path := t.TempDir() + "/pending.json"
	store := NewFilePendingStore(path)
	provider := &writeProvider{}
	preview := NewHandler(provider, WithWritesEnabled(true), WithPendingStore(store), WithHiveID("hive-a"))
	rec := httptest.NewRecorder()
	preview.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"agent.pause","args":{"agent":"scanner"}}}}`)))
	text := resultText(t, rec.Body.Bytes())
	var env struct {
		Data struct {
			ConfirmationID string `json:"confirmation_id"`
			Preview        struct {
				WideningDisclosure string `json:"widening_disclosure"`
			} `json:"preview"`
		} `json:"data"`
	}

	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.ConfirmationID == "" || !strings.Contains(env.Data.Preview.WideningDisclosure, "No widening") {
		t.Fatalf("preview envelope = %s", text)
	}

	restartedProvider := &writeProvider{}
	restarted := NewHandler(restartedProvider, WithWritesEnabled(true), WithPendingStore(NewFilePendingStore(path)), WithHiveID("hive-a"))
	rec = httptest.NewRecorder()
	restarted.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"`+env.Data.ConfirmationID+`"}}}`)))
	_ = resultText(t, rec.Body.Bytes())
	if len(restartedProvider.requests) != 1 || restartedProvider.requests[0].Path != "/api/pause/scanner" || restartedProvider.requests[0].Method != http.MethodPost {
		t.Fatalf("requests = %#v", restartedProvider.requests)
	}

	reuse := httptest.NewRecorder()
	restarted.ServeHTTP(reuse, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"`+env.Data.ConfirmationID+`"}}}`)))
	if !strings.Contains(reuse.Body.String(), ErrConfirmationMissing.Error()) {
		t.Fatalf("reuse body = %s", reuse.Body.String())
	}
}

func TestWritePreviewRefusesStoredCredentialFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	token := "Bearer " + strings.Repeat("f", 20)
	h := NewHandler(&writeProvider{}, WithWritesEnabled(true), WithPendingStore(NewFilePendingStore(path)), WithHiveID("hive-a"))
	rec := httptest.NewRecorder()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"governor.feature_settings","args":{"otelHeaders":{"Authorization":"` + token + `"}}}}}`
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(body)))
	if strings.Contains(rec.Body.String(), token) {
		t.Fatalf("preview response retained header value: %s", rec.Body.String())
	}
	if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), token) {
		t.Fatalf("pending file retained header value: %s", data)
	}
}

func TestFilePendingStorePrunesExpiredEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pending.json")
	now := time.Now().UTC()
	items := map[string]PendingConfirmation{
		"expired": {ID: "expired", Operation: WriteOpAgentPause, Args: map[string]any{"agent": "old"}, Hive: "hive-a", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)},
		"active":  {ID: "active", Operation: WriteOpAgentPause, Args: map[string]any{"agent": "new"}, Hive: "hive-a", CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
	}
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := NewFilePendingStore(path).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "active" {
		t.Fatalf("list = %#v", list)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "expired") || !strings.Contains(string(saved), "active") {
		t.Fatalf("saved pending file = %s", saved)
	}
}

func TestWriteConfirmPassesHiveRefusalBodyThrough(t *testing.T) {
	provider := &writeProvider{err: &HiveRefusalError{StatusCode: http.StatusForbidden, Message: "owner access required", Body: []byte(`{"error":"owner access required"}`)}}
	h := NewHandler(provider, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
	preview := httptest.NewRecorder()
	h.ServeHTTP(preview, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"agent.resume","args":{"agent":"scanner"}}}}`)))
	text := resultText(t, preview.Body.Bytes())
	var env struct {
		Data struct {
			ConfirmationID string `json:"confirmation_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &env); err != nil {
		t.Fatal(err)
	}
	confirm := httptest.NewRecorder()
	h.ServeHTTP(confirm, httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"`+env.Data.ConfirmationID+`"}}}`)))
	text = resultText(t, confirm.Body.Bytes())
	if !strings.Contains(text, `"type":"hive-refusal"`) || !strings.Contains(text, `"error":"owner access required"`) {
		t.Fatalf("refusal text = %s", text)
	}
}

type agentsProvider struct{ agents any }

func (p agentsProvider) Read(context.Context, string, map[string]any) (any, error) {
	return p.agents, nil
}

// cappedAgents builds an agents list longer than DefaultResultLimit with the
// named agent kept inside the first page.
func cappedAgents(name, owner string) []any {
	agents := []any{map[string]any{"name": name, "pauseOwner": owner}}
	for i := 0; i < DefaultResultLimit+5; i++ {
		agents = append(agents, map[string]any{"name": fmt.Sprintf("other-%d", i)})
	}
	return agents
}

func TestAgentResumePreviewHedgesWhenCappedListHidesAgent(t *testing.T) {
	agents := cappedAgents("other", "")
	capped := CapResult(agents, DefaultResultLimit)
	preview, err := agentResumeOp{}.PreviewWithProvider(context.Background(), map[string]any{"agent": "missing"}, agentsProvider{capped})
	if err != nil {
		t.Fatal(err)
	}
	effects := strings.Join(preview.Effects, "\n")
	if !strings.Contains(effects, "resume may claim operator ownership") || strings.Contains(effects, "\nIf the agent is paused, resume claims operator ownership") {
		t.Fatalf("effects = %v", preview.Effects)
	}
}

func TestAgentResumePreviewDisclosesPauseOwnershipClaim(t *testing.T) {
	args := map[string]any{"agent": "scanner"}
	claim := "pause_owner: operator"
	cases := []struct {
		name     string
		provider Provider
		want     bool
	}{
		{"unowned", agentsProvider{[]any{map[string]any{"name": "scanner"}}}, true},
		{"unreadable", errorProvider{fmt.Errorf("boom")}, true},
		{"owned", agentsProvider{[]any{map[string]any{"name": "scanner", "pauseOwner": "operator"}}}, false},
		{"capped owned", agentsProvider{CapResult(cappedAgents("scanner", "operator"), DefaultResultLimit)}, false},
		{"capped unowned", agentsProvider{CapResult(cappedAgents("scanner", ""), DefaultResultLimit)}, true},
	}
	for _, tc := range cases {
		preview, err := agentResumeOp{}.PreviewWithProvider(context.Background(), args, tc.provider)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(strings.Join(preview.Effects, "\n"), claim); got != tc.want {
			t.Fatalf("%s: claim disclosed = %v, effects = %v", tc.name, got, preview.Effects)
		}
	}
}

func TestAgentWriteOpsEscapeAgentPathSegment(t *testing.T) {
	preview, err := agentPauseOp{}.Preview(context.Background(), map[string]any{"agent": "team/scanner"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Request.Path != "/api/pause/team%2Fscanner" {
		t.Fatalf("pause path = %q", preview.Request.Path)
	}
	preview, err = agentResumeOp{}.Preview(context.Background(), map[string]any{"agent": "team/scanner"})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Request.Path != "/api/resume/team%2Fscanner" {
		t.Fatalf("resume path = %q", preview.Request.Path)
	}
}

func TestPhase6WriteOpsRegistered(t *testing.T) {
	registry := DefaultWriteRegistry()
	for _, name := range []string{
		WriteOpRepoPause,
		WriteOpRepoResume,
		WriteOpRepoAutoMerge,
		WriteOpRepositoryItemHold,
		WriteOpBudgetUpdate,
		WriteOpBudgetReset,
		WriteOpBudgetIgnore,
		WriteOpContributorTrust,
		WriteOpContributorAgentRole,
		WriteOpContributorRoleGrants,
		WriteOpContributorRevoke,
		WriteOpContributorRequeue,
		WriteOpContributorDelete,
		WriteOpBackupCreate,
		WriteOpCircuitBreakerEngage,
		WriteOpCircuitBreakerRelease,
	} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("write op %q is not registered", name)
		}
	}
}

func TestFleetWriteOpsRegistered(t *testing.T) {
	registry := DefaultWriteRegistry()
	for _, name := range []string{
		WriteOpFleetAutonomyLevel,
		WriteOpPlanPropose,
		WriteOpPlanApprove,
		WriteOpPlanReject,
		WriteOpFeatureSettings,
	} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("%s is not registered", name)
		}
	}
}

func TestPhase6WriteOpPreviews(t *testing.T) {
	tests := []struct {
		name string
		op   WriteOp
		args map[string]any
		want WriteRequest
	}{
		{
			name: "repo pause body",
			op:   repoPauseOp{},
			args: map[string]any{"repo": "hivecommons/hive", "reason": "maintenance"},
			want: WriteRequest{Method: http.MethodPost, Path: "/api/repos/pause", Body: map[string]any{"repo": "hivecommons/hive", "reason": "maintenance"}},
		},
		{
			name: "repo item hold escapes path",
			op:   repositoryItemHoldOp{},
			args: map[string]any{"owner": "hive commons", "repo": "hive/api", "number": float64(42), "held": true},
			want: WriteRequest{Method: http.MethodPost, Path: "/api/repos/hive%20commons/hive%2Fapi/items/42/hold", Body: map[string]any{"held": true}},
		},
		{
			name: "budget update",
			op:   budgetUpdateOp{},
			args: map[string]any{"totalTokens": float64(1000000), "criticalPct": float64(90)},
			want: WriteRequest{Method: http.MethodPut, Path: "/api/config/governor/budget", Body: map[string]any{"totalTokens": 1000000, "criticalPct": 90}},
		},
		{
			name: "contributor trust",
			op:   contributorTrustOp{},
			args: map[string]any{"contributor_id": "alice", "tier": "trusted"},
			want: WriteRequest{Method: http.MethodPut, Path: "/api/contributors/alice/trust", Body: map[string]any{"tier": "trusted"}},
		},
		{
			name: "breaker release",
			op:   circuitBreakerReleaseOp{},
			args: map[string]any{},
			want: WriteRequest{Method: http.MethodPost, Path: "/api/breaker/release"},
		},
		{
			name: "backup create",
			op:   backupCreateOp{},
			args: map[string]any{},
			want: WriteRequest{Method: http.MethodPost, Path: "/api/backup"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preview, err := tt.op.Preview(context.Background(), tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if preview.Request.Method != tt.want.Method || preview.Request.Path != tt.want.Path {
				t.Fatalf("request = %#v, want %#v", preview.Request, tt.want)
			}
			gotBody, _ := json.Marshal(preview.Request.Body)
			wantBody, _ := json.Marshal(tt.want.Body)
			if string(gotBody) != string(wantBody) {
				t.Fatalf("body = %s, want %s", gotBody, wantBody)
			}
			if preview.WideningDisclosure == "" || preview.ConfirmationMessage == "" {
				t.Fatalf("missing preview safety text: %#v", preview)
			}
		})
	}
}

func TestFleetAutonomyLevelPreviewIncludesCapabilityDisclosure(t *testing.T) {
	preview, err := fleetAutonomyLevelOp{}.Preview(context.Background(), map[string]any{"level": float64(6)})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/packs/level" {
		t.Fatalf("request = %#v", preview.Request)
	}
	body, ok := preview.Request.Body.(map[string]any)
	if !ok || body["level"] != 6 {
		t.Fatalf("body = %#v", preview.Request.Body)
	}
	if !strings.Contains(preview.WideningDisclosure, "Widening:") || !strings.Contains(preview.WideningDisclosure, "merge") {
		t.Fatalf("widening disclosure = %q", preview.WideningDisclosure)
	}
	if !strings.Contains(preview.ConfirmationMessage, "L6") {
		t.Fatalf("confirmation = %q", preview.ConfirmationMessage)
	}
}

func TestPlanWriteOpsPreviewPathsAndBodies(t *testing.T) {
	propose, err := planProposeOp{}.Preview(context.Background(), map[string]any{"repo": "org/repo", "number": float64(12), "title": "Plan this", "body": "details"})
	if err != nil {
		t.Fatal(err)
	}
	if propose.Request.Method != http.MethodPost || propose.Request.Path != "/api/plan/from-issue" {
		t.Fatalf("propose request = %#v", propose.Request)
	}
	proposeBody, ok := propose.Request.Body.(map[string]any)
	if !ok || proposeBody["number"] != 12 || proposeBody["title"] != "Plan this" {
		t.Fatalf("propose body = %#v", propose.Request.Body)
	}

	approve, err := planApproveOp{}.Preview(context.Background(), map[string]any{"epic_id": "epic/team"})
	if err != nil {
		t.Fatal(err)
	}
	if approve.Request.Path != "/api/plan/epic%2Fteam/approve" || !strings.Contains(approve.WideningDisclosure, "release child work") {
		t.Fatalf("approve preview = %#v", approve)
	}
	reject, err := planRejectOp{}.Preview(context.Background(), map[string]any{"epic_id": "epic/team"})
	if err != nil {
		t.Fatal(err)
	}
	if reject.Request.Path != "/api/plan/epic%2Fteam/reject" || strings.Contains(reject.WideningDisclosure, "Widening:") {
		t.Fatalf("reject preview = %#v", reject)
	}
}

func TestFeatureSettingsPreviewDisclosesProtectionChanges(t *testing.T) {
	preview, err := featureSettingsOp{}.Preview(context.Background(), map[string]any{"ioscanEnabled": false, "autonomyAutoPromote": true})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/config/governor/features" {
		t.Fatalf("request = %#v", preview.Request)
	}
	if !strings.Contains(preview.WideningDisclosure, "ioscanEnabled") || !strings.Contains(preview.WideningDisclosure, "autonomyAutoPromote") {
		t.Fatalf("disclosure = %q", preview.WideningDisclosure)
	}
	if _, err := (featureSettingsOp{}).Preview(context.Background(), map[string]any{"unknown": true}); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestAgentPhaseFourWriteOpsBuildRequests(t *testing.T) {
	cases := []struct {
		name   string
		op     WriteOp
		args   map[string]any
		method string
		path   string
	}{
		{name: "nudge", op: agentNudgeOp{}, args: map[string]any{"agent": "team/scanner", "prompt": "ship it"}, method: http.MethodPost, path: "/api/kick/team%2Fscanner"},
		{name: "restart", op: agentRestartOp{}, args: map[string]any{"agent": "team/scanner"}, method: http.MethodPost, path: "/api/restart/team%2Fscanner"},
		{name: "add", op: agentAddOp{}, args: map[string]any{"name": "new-worker", "config": map[string]any{"backend": "copilot"}}, method: http.MethodPost, path: "/api/agents"},
		{name: "remove", op: agentRemoveOp{}, args: map[string]any{"agent": "team/scanner"}, method: http.MethodDelete, path: "/api/agents/team%2Fscanner"},
		{name: "model", op: agentModelOp{}, args: map[string]any{"agent": "team/scanner", "model": "gpt-5"}, method: http.MethodPost, path: "/api/model/team%2Fscanner/gpt-5"},
		{name: "backend", op: agentBackendOp{}, args: map[string]any{"agent": "team/scanner", "backend": "copilot"}, method: http.MethodPost, path: "/api/switch/team%2Fscanner/copilot"},
		{name: "effort", op: agentEffortOp{}, args: map[string]any{"agent": "team/scanner", "effort": "high"}, method: http.MethodPost, path: "/api/effort/team%2Fscanner/high"},
		{name: "interaction", op: agentInteractionTierOp{}, args: map[string]any{"agent": "team/scanner", "tier": "ISSUES_AND_PRS"}, method: http.MethodPut, path: "/api/config/agent/team%2Fscanner/general"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			preview, err := tc.op.Preview(context.Background(), tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if preview.Request.Method != tc.method || preview.Request.Path != tc.path {
				t.Fatalf("request = %s %s", preview.Request.Method, preview.Request.Path)
			}
			if preview.ConfirmationMessage == "" || preview.WideningDisclosure == "" || len(preview.Effects) == 0 {
				t.Fatalf("incomplete preview = %#v", preview)
			}
		})
	}
}

func TestAgentNudgePreviewRequiresVerbatimPrompt(t *testing.T) {
	preview, err := (agentNudgeOp{}).Preview(context.Background(), map[string]any{"agent": "scanner", "prompt": "\nplease check the flaky test\n"})
	if err != nil {
		t.Fatal(err)
	}
	body, ok := preview.Request.Body.(map[string]any)
	if !ok || body["prompt"] != "\nplease check the flaky test\n" {
		t.Fatalf("body = %#v", preview.Request.Body)
	}
	if !strings.Contains(preview.ConfirmationMessage, "\nplease check the flaky test\n") || !strings.Contains(preview.Details["outcome_lookup"].(string), ToolAgentNudgeStatus) {
		t.Fatalf("preview = %#v", preview)
	}
	if _, err := (agentNudgeOp{}).Preview(context.Background(), map[string]any{"agent": "scanner"}); err == nil {
		t.Fatal("expected missing prompt error")
	}
}

func TestAgentInteractionTierDefaultClearsMode(t *testing.T) {
	preview, err := (agentInteractionTierOp{}).Preview(context.Background(), map[string]any{"agent": "scanner", "tier": "default"})
	if err != nil {
		t.Fatal(err)
	}
	body, ok := preview.Request.Body.(map[string]any)
	if !ok || body["mode"] != "" {
		t.Fatalf("body = %#v", preview.Request.Body)
	}
	if !strings.Contains(preview.WideningDisclosure, "Potential widening") {
		t.Fatalf("disclosure = %q", preview.WideningDisclosure)
	}
}

func TestDefaultWriteRegistryIncludesPhaseFourAgentOps(t *testing.T) {
	registry := DefaultWriteRegistry()
	for _, name := range []string{WriteOpAgentNudge, WriteOpAgentRestart, WriteOpAgentAdd, WriteOpAgentRemove, WriteOpAgentModel, WriteOpAgentBackend, WriteOpAgentEffort, WriteOpAgentInteractionTier} {
		if _, ok := registry.Get(name); !ok {
			t.Fatalf("missing write op %s", name)
		}
	}
}

func TestAgentNudgeStatusToolIsListedAsReadOnly(t *testing.T) {
	var found bool
	for _, tool := range ToolsWithWritesEnabled(true) {
		if tool["name"] != ToolAgentNudgeStatus {
			continue
		}
		found = true
		annotations, _ := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != true {
			t.Fatalf("annotations = %#v", annotations)
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		required, _ := schema["required"].([]string)
		if len(required) != 1 || required[0] != "agent" {
			t.Fatalf("schema = %#v", schema)
		}
	}
	if !found {
		t.Fatal("agent nudge status tool not listed")
	}
}

// TestVersionReadToolReadsAPIVersion: version_read is a listed read-only tool
// answered by the shared /api/version route, so the HTTP endpoint and stdio
// hive-admin-mcp both return the running build (#11220).
func TestVersionReadToolReadsAPIVersion(t *testing.T) {
	if path, ok := ReadPath(ToolVersionRead, 0); !ok || path != "/api/version" {
		t.Fatalf("version_read path = %q, %v", path, ok)
	}
	var found bool
	for _, tool := range Tools() {
		if tool["name"] != ToolVersionRead {
			continue
		}
		found = true
		annotations, _ := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != true {
			t.Fatalf("annotations = %#v", annotations)
		}
	}
	if !found {
		t.Fatal("version_read tool not listed")
	}
}

func TestLimitSchemaDescribesCap(t *testing.T) {
	for _, tool := range Tools() {
		schema, _ := tool["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		limit, ok := props["limit"].(map[string]any)
		if !ok {
			continue
		}
		desc, _ := limit["description"].(string)
		if desc == "" || !strings.Contains(desc, "50") {
			t.Errorf("%v: limit description = %q, want non-empty mentioning 50", tool["name"], desc)
		}
	}
}

// TestClaudeAuthStatusToolReadsStatusEndpoint: claude_auth_status is a listed,
// argument-free read-only tool answered by GET /api/claude-auth/status, so the
// in-hive provider and stdio hive-admin-mcp report the same auth mode the
// dashboard does (#17759).
func TestClaudeAuthStatusToolReadsStatusEndpoint(t *testing.T) {
	if !AllowedTool(ToolClaudeAuthStatus) {
		t.Fatal("claude_auth_status is not an allowed tool")
	}
	if path, ok := ReadPath(ToolClaudeAuthStatus, 0); !ok || path != "/api/claude-auth/status" {
		t.Fatalf("claude_auth_status path = %q, %v", path, ok)
	}
	var found bool
	for _, tool := range Tools() {
		if tool["name"] != ToolClaudeAuthStatus {
			continue
		}
		found = true
		annotations, _ := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != true {
			t.Fatalf("annotations = %#v", annotations)
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		if props, _ := schema["properties"].(map[string]any); len(props) != 0 {
			t.Fatalf("claude_auth_status takes no arguments, schema properties = %#v", props)
		}
	}
	if !found {
		t.Fatal("claude_auth_status tool not listed")
	}
}

// pr_models_read, contributor_model_stats_read and effective_models_read are
// listed read-only, take no write role, and describe their window argument,
// its bounds, limit and offset.
func TestModelEffectivenessToolsAreListedReadOnly(t *testing.T) {
	want := map[string]string{ToolPRModelsRead: "window", ToolContributorModelStatsRead: "days", ToolEffectiveModelsRead: "filter"}
	for _, tool := range Tools() {
		name, _ := tool["name"].(string)
		arg, ok := want[name]
		if !ok {
			continue
		}
		delete(want, name)
		if !AllowedTool(name) {
			t.Errorf("%s is listed but not allowed", name)
		}
		annotations, _ := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != true {
			t.Errorf("%s annotations = %#v, want read-only", name, annotations)
		}
		if meta, _ := tool["metadata"].(ToolMetadata); meta.Writes {
			t.Errorf("%s metadata says it writes", name)
		}
		schema, _ := tool["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props[arg]; !ok {
			t.Errorf("%s schema has no %q: %#v", name, arg, props)
		}
		if _, ok := props["limit"]; !ok {
			t.Errorf("%s schema has no limit: %#v", name, props)
		}
		if _, ok := props["offset"]; !ok {
			t.Errorf("%s schema has no offset: %#v", name, props)
		}
	}
	for name := range want {
		t.Errorf("%s is not listed", name)
	}
	window, _ := inputSchema(ToolPRModelsRead)["properties"].(map[string]any)["window"].(map[string]any)
	if enum, _ := window["enum"].([]string); strings.Join(enum, ",") != "7d,30d,all" {
		t.Errorf("window enum = %#v, want 7d, 30d, all", window["enum"])
	}
	days, _ := inputSchema(ToolContributorModelStatsRead)["properties"].(map[string]any)["days"].(map[string]any)
	if days["minimum"] != 0 || days["maximum"] != 365 {
		t.Errorf("days bounds = %v-%v, want 0-365", days["minimum"], days["maximum"])
	}
	effProps, _ := inputSchema(ToolEffectiveModelsRead)["properties"].(map[string]any)
	filter, _ := effProps["filter"].(map[string]any)
	if enum, _ := filter["enum"].([]string); strings.Join(enum, ",") != "all,hive,contributor" {
		t.Errorf("filter enum = %#v, want all, hive, contributor", filter["enum"])
	}
	effWindow, _ := effProps["window"].(map[string]any)
	if enum, _ := effWindow["enum"].([]string); strings.Join(enum, ",") != "7d,30d,all" {
		t.Errorf("effective_models_read window enum = %#v, want 7d, 30d, all", effWindow["enum"])
	}
	if _, ok := effProps["min_merged_prs"]; ok {
		t.Error("effective_models_read offers min_merged_prs, which the Operations panel has no control for")
	}
}

// Each tool reads the endpoint the dashboard reads, with the dashboard's
// default window when none is given: 7d for PRs by model, 7 days for the
// contributor run stats.
func TestModelEffectivenessReadPaths(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{ToolPRModelsRead, nil, "/api/governor/pr-models?window=7d"},
		{ToolPRModelsRead, map[string]any{}, "/api/governor/pr-models?window=7d"},
		{ToolPRModelsRead, map[string]any{"window": ""}, "/api/governor/pr-models?window=7d"},
		{ToolPRModelsRead, map[string]any{"window": "30d"}, "/api/governor/pr-models?window=30d"},
		{ToolPRModelsRead, map[string]any{"window": " ALL "}, "/api/governor/pr-models?window=all"},
		{ToolContributorModelStatsRead, nil, "/api/contribute/run-stats/models?days=7"},
		{ToolContributorModelStatsRead, map[string]any{"days": float64(0)}, "/api/contribute/run-stats/models?days=0"},
		{ToolContributorModelStatsRead, map[string]any{"days": float64(365)}, "/api/contribute/run-stats/models?days=365"},
		{ToolContributorModelStatsRead, map[string]any{"days": 30}, "/api/contribute/run-stats/models?days=30"},
		{ToolContributorModelStatsRead, map[string]any{"days": "14"}, "/api/contribute/run-stats/models?days=14"},
		{ToolContributorModelStatsRead, map[string]any{"days": float64(0), "offset": float64(50)}, "/api/contribute/run-stats/models?days=0"},
		{ToolEffectiveModelsRead, nil, "/api/contribute/effective-models?window=7d&filter=all"},
		{ToolEffectiveModelsRead, map[string]any{"window": "30d", "filter": "hive"}, "/api/contribute/effective-models?window=30d&filter=hive"},
		{ToolEffectiveModelsRead, map[string]any{"window": "all", "filter": " Contributor "}, "/api/contribute/effective-models?window=all&filter=contributor"},
		{ToolEffectiveModelsRead, map[string]any{"filter": ""}, "/api/contribute/effective-models?window=7d&filter=all"},
	} {
		got, err := ModelEffectivenessReadPath(tc.tool, tc.args)
		if err != nil || got != tc.want {
			t.Errorf("%s %v path = %q, %v; want %q", tc.tool, tc.args, got, err, tc.want)
		}
	}
	for tool, want := range map[string]string{
		ToolPRModelsRead:              "/api/governor/pr-models?window=7d",
		ToolContributorModelStatsRead: "/api/contribute/run-stats/models?days=7",
		ToolEffectiveModelsRead:       "/api/contribute/effective-models?window=7d&filter=all",
	} {
		if got, ok := ReadPath(tool, 5); !ok || got != want {
			t.Errorf("ReadPath(%s) = %q, %v; want %q", tool, got, ok, want)
		}
	}
}

// The endpoints answer an unknown window with their default, so the tools
// refuse one with its reason instead of passing it through.
func TestModelEffectivenessRefusesBadWindows(t *testing.T) {
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{ToolPRModelsRead, map[string]any{"window": "14d"}, "window must be one of 7d, 30d, all"},
		{ToolPRModelsRead, map[string]any{"window": "7d&x=1"}, "window must be one of"},
		{ToolPRModelsRead, map[string]any{"window": float64(7)}, "window must be one of"},
		{ToolContributorModelStatsRead, map[string]any{"days": float64(-1)}, "days must be a whole number from 0 to 365"},
		{ToolContributorModelStatsRead, map[string]any{"days": float64(366)}, "days must be a whole number from 0 to 365"},
		{ToolContributorModelStatsRead, map[string]any{"days": 1.5}, "days must be a whole number"},
		{ToolContributorModelStatsRead, map[string]any{"days": "week"}, "days must be a whole number"},
		{ToolContributorModelStatsRead, map[string]any{"days": true}, "days must be a whole number"},
		{ToolContributorModelStatsRead, map[string]any{"offset": float64(-1)}, "offset must be a non-negative integer"},
		{ToolPRModelsRead, map[string]any{"offset": "two"}, "offset must be a non-negative integer"},
		{ToolEffectiveModelsRead, map[string]any{"window": "90d"}, "window must be one of 7d, 30d, all"},
		{ToolEffectiveModelsRead, map[string]any{"filter": "agents"}, "filter must be one of all, hive, contributor"},
		{ToolEffectiveModelsRead, map[string]any{"filter": "hive&x=1"}, "filter must be one of"},
		{ToolEffectiveModelsRead, map[string]any{"filter": true}, "filter must be one of"},
	} {
		path, err := ModelEffectivenessReadPath(tc.tool, tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %v = %q, %v; want a refusal containing %q", tc.tool, tc.args, path, err, tc.want)
		}
	}
}

// An answer within limit is returned as the endpoint sent it. A longer one
// has its per-model rows cut to limit and says so.
func TestModelEffectivenessResultCapsModelRows(t *testing.T) {
	rows := []any{map[string]any{"model": "a"}, map[string]any{"model": "b"}, map[string]any{"model": "c"}}
	body := map[string]any{"window": "30d", "total": float64(9), "buckets": rows, "fixer_queue": map[string]any{}}
	same, err := ModelEffectivenessResult(ToolPRModelsRead, body, map[string]any{"limit": float64(3)})
	if err != nil {
		t.Fatal(err)
	}
	if a, b := mustJSON(t, same), mustJSON(t, body); a != b {
		t.Fatalf("uncapped answer changed:\n got %s\nwant %s", a, b)
	}
	capped, err := ModelEffectivenessResult(ToolPRModelsRead, body, map[string]any{"limit": float64(2)})
	if err != nil {
		t.Fatal(err)
	}
	out := capped.(map[string]any)
	if got, _ := out["buckets"].([]any); len(got) != 2 || out["buckets_total"] != 3 {
		t.Fatalf("capped buckets = %#v, total %v", out["buckets"], out["buckets_total"])
	}
	meta, _ := out["_admin_mcp"].(map[string]any)
	if meta["truncated"] != true || meta["limit"] != 2 || meta["total"] != 3 || !strings.Contains(fmt.Sprint(meta["note"]), "first 2 of 3") {
		t.Fatalf("capped disclosure = %#v", meta)
	}
	if len(body["buckets"].([]any)) != 3 {
		t.Fatal("capping modified the endpoint's answer")
	}
	stats := map[string]any{"window_days": float64(7), "models": rows}
	cappedStats, err := ModelEffectivenessResult(ToolContributorModelStatsRead, stats, map[string]any{"limit": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := cappedStats.(map[string]any)["models"].([]any); len(got) != 1 {
		t.Fatalf("capped models = %#v", cappedStats)
	}
	if _, err := ModelEffectivenessResult(ToolPRModelsRead, []any{}, nil); err == nil {
		t.Fatal("a non-object answer was accepted")
	}
}

// offset pages the per-model rows past the limit cap: each page says where
// it starts, how many rows there are and where the next page starts, and a
// page past the end is empty and says so. effective_models_read pages
// insufficient and returns ranked whole.
func TestModelEffectivenessResultPagesWithOffset(t *testing.T) {
	rows := []any{}
	for _, m := range []string{"a", "b", "c", "d", "e"} {
		rows = append(rows, map[string]any{"model": m})
	}
	stats := map[string]any{"window_days": float64(0), "models": rows}
	page := func(args map[string]any) map[string]any {
		t.Helper()
		got, err := ModelEffectivenessResult(ToolContributorModelStatsRead, stats, args)
		if err != nil {
			t.Fatal(err)
		}
		return got.(map[string]any)
	}
	models := func(out map[string]any) string {
		var names []string
		for _, r := range out["models"].([]any) {
			names = append(names, r.(map[string]any)["model"].(string))
		}
		return strings.Join(names, ",")
	}

	first := page(map[string]any{"limit": float64(2)})
	if models(first) != "a,b" || first["next_offset"] != 2 || first["models_total"] != 5 || first["offset"] != 0 {
		t.Fatalf("first page = %#v", first)
	}
	middle := page(map[string]any{"limit": float64(2), "offset": float64(2)})
	meta, _ := middle["_admin_mcp"].(map[string]any)
	if models(middle) != "c,d" || middle["next_offset"] != 4 || meta["truncated"] != true || !strings.Contains(fmt.Sprint(meta["note"]), "rows 3-4 of 5") {
		t.Fatalf("middle page = %#v", middle)
	}
	last := page(map[string]any{"limit": float64(2), "offset": float64(4)})
	if models(last) != "e" {
		t.Fatalf("last page = %#v", last)
	}
	if _, ok := last["next_offset"]; ok {
		t.Fatalf("last page has a next_offset: %#v", last)
	}
	past := page(map[string]any{"offset": float64(9)})
	pastMeta, _ := past["_admin_mcp"].(map[string]any)
	if len(past["models"].([]any)) != 0 || !strings.Contains(fmt.Sprint(pastMeta["note"]), "past the last") {
		t.Fatalf("page past the end = %#v", past)
	}
	if len(stats["models"].([]any)) != 5 {
		t.Fatal("paging modified the endpoint's answer")
	}

	ranked := []any{map[string]any{"model": "r1"}, map[string]any{"model": "r2"}, map[string]any{"model": "r3"}}
	eff := map[string]any{"window": "30d", "filter": "hive", "ranked": ranked, "insufficient": rows, "coverage": map[string]any{"closed_prs_partial": true}}
	got, err := ModelEffectivenessResult(ToolEffectiveModelsRead, eff, map[string]any{"limit": float64(1), "offset": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	out := got.(map[string]any)
	if len(out["ranked"].([]any)) != 3 {
		t.Fatalf("ranked was paged: %#v", out["ranked"])
	}
	if ins := out["insufficient"].([]any); len(ins) != 1 || ins[0].(map[string]any)["model"] != "b" || out["insufficient_total"] != 5 || out["next_offset"] != 2 {
		t.Fatalf("insufficient page = %#v", out)
	}
	if out["coverage"] == nil {
		t.Fatal("coverage was dropped")
	}
	if _, err := ModelEffectivenessResult(ToolContributorModelStatsRead, stats, map[string]any{"offset": float64(-3)}); err == nil {
		t.Fatal("a negative offset was accepted")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// governor.ci's workflow-approval keys are writable through
// governor.feature_settings, and the preview discloses turning approval on
// and changing who is trusted.
func TestFeatureSettingsWorkflowApproval(t *testing.T) {
	preview, err := featureSettingsOp{}.Preview(context.Background(), map[string]any{"ciAutoApproveWorkflows": true, "ciAutoApproveWorkflowActors": "dependabot[bot], github-actions[bot]"})
	if err != nil {
		t.Fatal(err)
	}
	body := preview.Request.Body.(map[string]any)
	if body["ciAutoApproveWorkflows"] != true || body["ciAutoApproveWorkflowActors"] != "dependabot[bot], github-actions[bot]" {
		t.Fatalf("body = %#v", body)
	}
	for _, want := range []string{"ciAutoApproveWorkflows", "ciAutoApproveWorkflowActors"} {
		if !strings.Contains(preview.WideningDisclosure, want) {
			t.Fatalf("disclosure %q does not name %s", preview.WideningDisclosure, want)
		}
	}
	if p, err := (featureSettingsOp{}).Preview(context.Background(), map[string]any{"ciAutoApproveWorkflows": false, "ciAutoApproveWorkflowActors": "none"}); err != nil || strings.Contains(p.WideningDisclosure, "ciAutoApprove") {
		t.Fatalf("turning approval off and trusting no one is no widening: %q, %v", p.WideningDisclosure, err)
	}
}

// shared_ci_rerun.enabled is writable through governor.feature_settings;
// turning the sweep on is disclosed, turning it off is not.
func TestFeatureSettingsSharedCIRerun(t *testing.T) {
	preview, err := featureSettingsOp{}.Preview(context.Background(), map[string]any{"sharedCiRerunEnabled": true})
	if err != nil {
		t.Fatal(err)
	}
	if body := preview.Request.Body.(map[string]any); body["sharedCiRerunEnabled"] != true {
		t.Fatalf("body = %#v", body)
	}
	if !strings.Contains(preview.WideningDisclosure, "sharedCiRerunEnabled") {
		t.Fatalf("disclosure %q does not name sharedCiRerunEnabled", preview.WideningDisclosure)
	}
	if p, err := (featureSettingsOp{}).Preview(context.Background(), map[string]any{"sharedCiRerunEnabled": false}); err != nil || strings.Contains(p.WideningDisclosure, "sharedCiRerunEnabled") {
		t.Fatalf("turning the rerun sweep off is no widening: %q, %v", p.WideningDisclosure, err)
	}
}

// Write-back containment's unsolicited knobs (Settings → Features →
// Write-back containment) are writable here too, and the preview names
// each change that lets the hive write to more items (#18846 follow-up).
func TestFeatureSettingsWritebackUnsolicited(t *testing.T) {
	preview, err := featureSettingsOp{}.Preview(context.Background(), map[string]any{"writebackBackfill": true, "writebackRequireLabels": false, "writebackMinLevel": 3})
	if err != nil {
		t.Fatal(err)
	}
	body := preview.Request.Body.(map[string]any)
	if body["writebackBackfill"] != true || body["writebackRequireLabels"] != false || body["writebackMinLevel"] != 3 {
		t.Fatalf("body = %#v", body)
	}
	for _, want := range []string{"writebackBackfill", "writebackRequireLabels", "writebackMinLevel"} {
		if !strings.Contains(preview.WideningDisclosure, want) {
			t.Fatalf("disclosure %q does not name %s", preview.WideningDisclosure, want)
		}
	}
	if p, err := (featureSettingsOp{}).Preview(context.Background(), map[string]any{"writebackBackfill": false}); err != nil || strings.Contains(p.WideningDisclosure, "writebackBackfill") {
		t.Fatalf("turning backfill off is no widening: %q, %v", p.WideningDisclosure, err)
	}
}
