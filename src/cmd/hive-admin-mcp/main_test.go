package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/hivectl"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSameToolAnswerMatchesHTTPAndStdioWrapper(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	provider := readProvider{roster: r}

	stdioResult, err := provider.handler(adminmcp.ToolHiveStatus)(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(stdioResult.Content) != 1 {
		t.Fatalf("stdio content = %#v", stdioResult.Content)
	}
	stdioText := mustMarshalContentText(t, stdioResult.Content[0])

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hive_status","arguments":{}}}`))
	adminmcp.NewHandler(provider).ServeHTTP(rec, req)
	httpText := adminResultText(t, rec.Body.String())
	if stdioText != httpText {
		t.Fatalf("stdio %s != http %s", stdioText, httpText)
	}
}

func TestStdioToolResultAndErrorTextAreScrubbed(t *testing.T) {
	githubToken := "gh" + "u_" + strings.Repeat("a", 24)
	bearer := "Bearer " + strings.Repeat("b", 20)
	aws := "AKIA" + strings.Repeat("C", 16)
	jwt := strings.Join([]string{"eyJ" + strings.Repeat("d", 22), strings.Repeat("e", 24), strings.Repeat("f", 24)}, ".")
	privateKey := strings.Join([]string{"-----BEGIN PRIVATE KEY-----", strings.Repeat("A", 64), "-----END PRIVATE KEY-----"}, "\n")
	var fail bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			http.Error(w, "failed "+githubToken+" "+bearer, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"title": strings.Join([]string{githubToken, bearer, aws, jwt, privateKey}, " ")})
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	provider := readProvider{roster: r}
	result, err := provider.handler(adminmcp.ToolHiveStatus)(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	text := mustMarshalContentText(t, result.Content[0])
	for _, forbidden := range []string{githubToken, bearer, aws, jwt, privateKey} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("stdio result retained %q in %s", forbidden, text)
		}
	}
	fail = true
	result, err = provider.handler(adminmcp.ToolHiveStatus)(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	text = mustMarshalContentText(t, result.Content[0])
	if strings.Contains(text, githubToken) || strings.Contains(text, bearer) || !strings.Contains(text, "redacted:github-token") || !strings.Contains(text, "redacted:bearer-token") {
		t.Fatalf("stdio error text = %s", text)
	}
}

func mustMarshalContentText(t *testing.T, c mcp.Content) string {
	t.Helper()
	text, ok := c.(*mcp.TextContent)
	if !ok {
		t.Fatalf("content = %#v", c)
	}
	return text.Text
}

func adminResultText(t *testing.T, body string) string {
	t.Helper()
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
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

func TestReadProviderCapsActiveHiveList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"agents":[{"name":"a"},{"name":"b"}]}`))
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	data, err := (readProvider{roster: r}).Read(context.Background(), adminmcp.ToolAgentsList, map[string]any{"limit": float64(1)})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := data.(map[string]any)
	if !ok {
		t.Fatalf("data = %#v", data)
	}
	agents, ok := m["agents"].([]any)
	if !ok || len(agents) != 1 || m["agents_truncated"] != true {
		t.Fatalf("capped data = %#v", data)
	}
}

// TestReadProviderSendsLimitAsQuery pins #9160: the capped list tools must put
// limit in the query string, not in the path where hivectl would escape the
// "?" into "/api/agents%3Flimit=1" and the hive would answer 404.
func TestReadProviderSendsLimitAsQuery(t *testing.T) {
	for _, tc := range []struct {
		tool string
		path string
	}{
		{adminmcp.ToolAgentsList, "/api/agents"},
		{adminmcp.ToolRunsList, "/api/runs"},
		{adminmcp.ToolLeasesList, "/api/runs"},
		{adminmcp.ToolClaimsList, "/api/claims"},
		{adminmcp.ToolPlansList, "/api/plans"},
		{adminmcp.ToolContributorsList, "/api/contributors"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path || r.URL.Query().Get("limit") != "1" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
			if _, err := (readProvider{roster: r}).Read(context.Background(), tc.tool, map[string]any{"limit": float64(1)}); err != nil {
				t.Fatalf("Read(%s) err = %v", tc.tool, err)
			}
		})
	}
}

func TestRosterActiveHiveOnly(t *testing.T) {
	var hitsA, hitsB int
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsA++
		if got := r.Header.Get("Authorization"); got != "Bearer token-a" {
			t.Fatalf("auth A = %q", got)
		}
		for _, forbidden := range []string{"X-Hive-User", "X-Hive-Role", "X-Hive-Owner-Role-Verified"} {
			if r.Header.Get(forbidden) != "" {
				t.Fatalf("sent forbidden header %s", forbidden)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hive":"a"}`))
	}))
	defer serverA.Close()
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitsB++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hive":"b"}`))
	}))
	defer serverB.Close()
	r := &roster{hives: []hiveConfig{{Name: "a", Address: serverA.URL, Token: "token-a"}, {Name: "b", Address: serverB.URL, Token: "token-b"}}, active: 0, timeout: time.Second}
	provider := readProvider{roster: r}
	data, err := provider.Read(context.Background(), adminmcp.ToolHiveStatus, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := data.(map[string]any)
	if !ok || m["hive"] != "a" {
		t.Fatalf("data = %#v", data)
	}
	if hitsA != 1 || hitsB != 0 {
		t.Fatalf("hits A=%d B=%d, want A only", hitsA, hitsB)
	}
}

func TestSelectHiveDiagnosesWrongToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "wrong", Address: server.URL, Token: "bad"}}, timeout: time.Second}
	_, err := r.selectHive(context.Background(), "wrong")
	if err == nil || !strings.Contains(err.Error(), "wrong dashboard token") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiagnoseSelectionErrorDirectRoute(t *testing.T) {
	err := diagnoseSelectionError(&hivectl.APIError{StatusCode: http.StatusUnauthorized, Message: "direct-route shared token refused"})
	if err == nil || !strings.Contains(err.Error(), "direct-route spoke") {
		t.Fatalf("err = %v", err)
	}
}

func TestDiagnoseSelectionErrorUnreachable(t *testing.T) {
	err := diagnoseSelectionError(&hivectl.ConnectionError{Err: errors.New("dial failed")})
	if err == nil || !strings.Contains(err.Error(), "unreachable host") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadRosterFromEnvSelectsConfiguredActiveHive(t *testing.T) {
	t.Setenv(envHives, `[{"name":"east","address":"http://east.example","token":"east-token"},{"name":"west","address":"http://west.example","token":"west-token"}]`)
	t.Setenv(envActiveHive, "west")

	r, err := loadRosterFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if r.active != 1 || len(r.hives) != 2 {
		t.Fatalf("roster active=%d hives=%#v", r.active, r.hives)
	}
	if r.timeout != defaultTimeout {
		t.Fatalf("timeout = %v", r.timeout)
	}
}

func TestLoadRosterFromEnvRejectsInvalidRosters(t *testing.T) {
	tests := []struct {
		name       string
		hives      string
		activeHive string
		want       string
	}{
		{name: "missing", want: envHives + " must contain"},
		{name: "bad json", hives: `{`, want: "parse " + envHives},
		{name: "empty", hives: `[]`, want: envHives + " must contain at least one hive"},
		{name: "blank field", hives: `[{"name":"east","address":"http://east.example","token":""}]`, want: "require name, address, and token"},
		{name: "duplicate", hives: `[{"name":"east","address":"http://east.example","token":"one"},{"name":"east","address":"http://other.example","token":"two"}]`, want: `duplicate hive name "east"`},
		{name: "unknown active", hives: `[{"name":"east","address":"http://east.example","token":"one"}]`, activeHive: "west", want: `active hive "west" is not in the roster`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(envHives, tt.hives)
			t.Setenv(envActiveHive, tt.activeHive)
			_, err := loadRosterFromEnv()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestWriteProviderPreviewConfirmUsesDashboardTokenOnly(t *testing.T) {
	var paused bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pause/scanner" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("auth = %q", got)
		}
		for _, forbidden := range []string{"X-Hive-User", "X-Hive-Role", "X-Hive-Owner-Role-Verified"} {
			if r.Header.Get(forbidden) != "" {
				t.Fatalf("sent forbidden header %s", forbidden)
			}
		}
		paused = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"changed":true}`))
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	provider := readProvider{roster: r, writesEnabled: true, pendingStore: adminmcp.NewMemoryPendingStore()}
	preview, err := provider.callWrite(context.Background(), adminmcp.ToolWritePreview, map[string]any{"operation": adminmcp.WriteOpAgentPause, "args": map[string]any{"agent": "scanner"}})
	if err != nil {
		t.Fatal(err)
	}
	previewMap, ok := preview.(map[string]any)
	if !ok {
		t.Fatalf("preview = %#v", preview)
	}
	id, _ := previewMap["confirmation_id"].(string)
	if id == "" {
		t.Fatalf("preview = %#v", preview)
	}
	if _, err := provider.callWrite(context.Background(), adminmcp.ToolWriteConfirm, map[string]any{"confirmation_id": id}); err != nil {
		t.Fatal(err)
	}
	if !paused {
		t.Fatal("pause endpoint was not called")
	}
}

func TestAdminMCPWriteEnvHelpers(t *testing.T) {
	t.Setenv(envEnableWrites, "")
	if adminMCPWritesEnabled() {
		t.Fatal("writes enabled by default")
	}
	t.Setenv(envEnableWrites, "true")
	if !adminMCPWritesEnabled() {
		t.Fatal("writes not enabled by env")
	}
	t.Setenv(envPendingFile, "state/pending.json")
	if got := adminMCPPendingPath(); got != "state/pending.json" {
		t.Fatalf("pending path = %q", got)
	}
}

func TestWritePreviewDisabledInStdioProvider(t *testing.T) {
	r := &roster{hives: []hiveConfig{{Name: "active", Address: "http://127.0.0.1", Token: "token"}}, active: 0, timeout: time.Second}
	provider := readProvider{roster: r, pendingStore: adminmcp.NewMemoryPendingStore()}
	_, err := provider.callWrite(context.Background(), adminmcp.ToolWritePreview, map[string]any{"operation": adminmcp.WriteOpAgentPause, "args": map[string]any{"agent": "scanner"}})
	if !errors.Is(err, adminmcp.ErrWritesDisabled) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadPathAgentNudgeStatus(t *testing.T) {
	path, ok := readPath(adminmcp.ToolAgentNudgeStatus, map[string]any{"agent": "team/scanner"})
	if !ok || path != "/api/kick/team%2Fscanner/status" {
		t.Fatalf("path = %q ok=%v", path, ok)
	}
}

func TestStdioHandlerTruncatesOversizedResult(t *testing.T) {
	repos := make([]map[string]any, 4000)
	for i := range repos {
		repos[i] = map[string]any{"name": "repo", "health": strings.Repeat("x", 120)}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "repos": repos})
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}

	result, err := (readProvider{roster: r}).handler(adminmcp.ToolFleetStatus)(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{}`)}})
	if err != nil {
		t.Fatal(err)
	}
	text := mustMarshalContentText(t, result.Content[0])
	if len(text) > adminmcp.MaxTextBytes {
		t.Fatalf("stdio text is %d bytes, cap is %d", len(text), adminmcp.MaxTextBytes)
	}
	if !strings.Contains(text, `"truncated":true`) || !strings.Contains(text, `"repos_truncated":true`) {
		t.Fatalf("stdio result missing truncation disclosure: %.300s", text)
	}
}

func TestRosterReusesClientPerHive(t *testing.T) {
	r := &roster{hives: []hiveConfig{{Name: "a", Address: "http://127.0.0.1:1", Token: "t"}, {Name: "b", Address: "http://127.0.0.1:2", Token: "t"}}, timeout: time.Second}
	first, _, err := r.activeClient()
	if err != nil {
		t.Fatal(err)
	}
	second, _, _ := r.activeClient()
	if first != second {
		t.Fatal("activeClient built a new client for the same hive")
	}
	r.active = 1
	other, _, _ := r.activeClient()
	if other == first {
		t.Fatal("different hives share one client")
	}
}

func TestReadPathAdvisorRecords(t *testing.T) {
	path, ok := readPath(adminmcp.ToolAdvisorRecords, map[string]any{"agent": "scout", "hours": float64(24)})
	if !ok || !strings.HasPrefix(path, "/api/advisor/records?") || !strings.Contains(path, "agent=scout") || !strings.Contains(path, "hours=24") {
		t.Fatalf("path = %q ok=%v", path, ok)
	}
}

func TestReadPathBandTools(t *testing.T) {
	cases := []struct {
		tool   string
		prefix string
	}{
		{adminmcp.ToolIssuesByBand, "/api/overview/issues.json"},
		{adminmcp.ToolPrsByBand, "/api/overview/prs.json"},
	}
	for _, tc := range cases {
		path, ok := readPath(tc.tool, map[string]any{"repo": "owner/name", "band": "done", "stale": true, "limit": float64(5)})
		if !ok {
			t.Fatalf("%s: expected ok", tc.tool)
		}
		if !strings.HasPrefix(path, tc.prefix+"?") {
			t.Fatalf("%s: path = %q", tc.tool, path)
		}
		if !strings.Contains(path, "repo=owner%2Fname") || !strings.Contains(path, "stale=true") {
			t.Fatalf("%s: path = %q, want repo and stale forwarded", tc.tool, path)
		}
		if strings.Contains(path, "band=") || strings.Contains(path, "limit=") {
			t.Fatalf("%s: path = %q, must not forward band or limit", tc.tool, path)
		}
	}
}

func TestReadProviderIssuesByBandFiltersRowsButKeepsBands(t *testing.T) {
	// Each tool has its own band vocabulary (issueBandKeys vs prBandKeys), so
	// pick a filter band and a filler band that are valid for that tool.
	cases := []struct {
		tool   string
		want   string
		filler string
	}{
		{adminmcp.ToolIssuesByBand, "done", "ready"},
		{adminmcp.ToolPrsByBand, "in-review", "open"},
	}
	for _, tc := range cases {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("band") != "" {
				t.Fatalf("%s: server saw a band filter %q; the tool must fetch every band", tc.tool, r.URL.Query().Get("band"))
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"bands": []any{
					map[string]any{"key": tc.filler, "label": "Filler", "rule": "no other band matched", "count": 1},
					map[string]any{"key": tc.want, "label": "Wanted", "rule": "matches the filter", "count": 2},
				},
				"rows": []any{
					map[string]any{"number": 1, "band": tc.filler},
					map[string]any{"number": 2, "band": tc.want},
					map[string]any{"number": 3, "band": tc.want},
				},
			})
		}))
		defer server.Close()
		r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
		result, err := (readProvider{roster: r}).Read(context.Background(), tc.tool, map[string]any{"band": tc.want})
		if err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		body, ok := result.(map[string]any)
		if !ok {
			t.Fatalf("%s: result = %#v", tc.tool, result)
		}
		bands, ok := body["bands"].([]any)
		if !ok || len(bands) != 2 {
			t.Fatalf("%s: bands = %#v, want both bands even when filtered", tc.tool, body["bands"])
		}
		rows, ok := body["rows"].([]any)
		if !ok || len(rows) != 2 {
			t.Fatalf("%s: rows = %#v, want only the 2 %s-band rows", tc.tool, body["rows"], tc.want)
		}
	}
}

// TestReadProviderBandFilterMatchesLabelShapedRows pins #10018 on stdio: the
// Overview export puts the band display label in each row, and the stdio
// provider must still match band=<key> the way the dashboard provider does.
func TestReadProviderBandFilterMatchesLabelShapedRows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"bands": []any{
				map[string]any{"key": "ready", "label": "Unclaimed"},
				map[string]any{"key": "done", "label": "Confirm & close"},
			},
			"rows": []any{
				map[string]any{"number": 1, "band": "Unclaimed"},
				map[string]any{"number": 2, "band": "Confirm & close"},
			},
		})
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	result, err := (readProvider{roster: r}).Read(context.Background(), adminmcp.ToolIssuesByBand, map[string]any{"band": "done"})
	if err != nil {
		t.Fatal(err)
	}
	rows, ok := result.(map[string]any)["rows"].([]any)
	if !ok || len(rows) != 1 || rows[0].(map[string]any)["band"] != "done" {
		t.Fatalf("rows = %#v, want the one Confirm & close row rekeyed to done", result)
	}
}

func TestReadProviderReviewQueueMergesIssuesAndPRs(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/overview/issues.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"bands": []any{map[string]any{"key": "done", "label": "Confirm & close"}},
				"rows":  []any{map[string]any{"repo": "o/a", "number": 1, "band": "Confirm & close", "updated_at": "2026-01-01T00:00:00Z"}},
			})
		case "/api/overview/prs.json":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"bands": []any{map[string]any{"key": "waiting", "label": "Needs human"}},
				"rows":  []any{map[string]any{"repo": "o/a", "number": 2, "band": "Needs human", "held": true, "hold_reason": "hold", "updated_at": "2026-01-02T00:00:00Z"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	r := &roster{hives: []hiveConfig{{Name: "active", Address: server.URL, Token: "token"}}, active: 0, timeout: time.Second}
	result, err := (readProvider{roster: r}).Read(context.Background(), adminmcp.ToolReviewQueue, map[string]any{"repo": "o/a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || paths[0] != "/api/overview/issues.json?repo=o%2Fa" || paths[1] != "/api/overview/prs.json?repo=o%2Fa" {
		t.Fatalf("paths = %#v", paths)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Rows []struct {
			Kind       string `json:"kind"`
			ReasonCode string `json:"reason_code"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Rows) != 2 || body.Rows[0].Kind != "pr" || body.Rows[0].ReasonCode != adminmcp.ReviewReasonHold || body.Rows[1].ReasonCode != adminmcp.ReviewReasonConfirmClose {
		t.Fatalf("queue = %s, want the held PR before the confirm-and-close issue", encoded)
	}
}

func TestReadPathGovernorSetup(t *testing.T) {
	path, ok := readPath(adminmcp.ToolGovernorSetup, map[string]any{})
	if !ok || path != "/api/config/governor" {
		t.Fatalf("path = %q ok=%v", path, ok)
	}
}
