package adminmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

type staticProvider struct{ seenTool string }

func (p *staticProvider) Read(_ context.Context, tool string, _ map[string]any) (any, error) {
	p.seenTool = tool
	return map[string]any{"status": "ok", "dashboard_token": "secret-value"}, nil
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
