package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// ghUsageRecorded is what ghUsageServer records, per caller.
//
//   - hive:wide_sweep: 30 endpoints, 2 charged calls each (60 charged).
//   - hive:narrow_sweep: one endpoint, 9 charged and 6 free 304s.
//   - agent:builder: one endpoint, 4 charged, one of them refused for an
//     exhausted limit.
//
// 79 requests, 73 of them charged, on 32 caller + method + endpoint rows.
var ghUsageRecorded = map[string]map[string]float64{
	"hive:wide_sweep":   {"requests": 60, "charged": 60, "not_modified": 0, "rate_limited": 0},
	"hive:narrow_sweep": {"requests": 15, "charged": 9, "not_modified": 6, "rate_limited": 0},
	"agent:builder":     {"requests": 4, "charged": 4, "not_modified": 0, "rate_limited": 1},
}

const (
	ghUsageRecordedRequests = 79
	ghUsageRecordedCharged  = 73
	ghUsageRecordedRows     = 32
)

// ghUsageServer is a hive whose GitHub client answers GET /rate_limit from a
// mock, with a fresh REST accounting window holding the requests in
// ghUsageRecorded. The handler's own GET /rate_limit can join that window as
// it is read (charged 0, caller hive), so the tests assert the recorded
// callers' counts exactly and the window totals as at least their sum.
func ghUsageServer(t *testing.T, authToken string) *Server {
	t.Helper()
	t.Cleanup(github.ResetRESTAccountingForTest(nil))
	ghSrv := ghMockServer()
	t.Cleanup(ghSrv.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var s *Server
	if authToken == "" {
		s = NewServer(0, logger)
	} else {
		s = NewServerWithAuth(0, authToken, logger)
	}
	deps := testDeps(t)
	deps.GHClient = github.NewClientForTest(ghSrv.URL, "acme", []string{"widgets"}, logger)
	deps.Config.GitHub.APIReserve = 900
	deps.Config.GitHub.APICritical = 300
	s.RegisterAPI(deps)
	SetAgentGitHubCallsProvider(func() map[string]int { return map[string]int{"scanner": 7} })
	t.Cleanup(func() { SetAgentGitHubCallsProvider(nil) })

	none := http.Header{}
	revalidated := http.Header{}
	revalidated.Set("X-Hive-Cache", "revalidated")
	exhausted := http.Header{}
	exhausted.Set("X-RateLimit-Remaining", "0")
	for i := range 30 {
		for range 2 {
			github.RecordRESTRequest("hive:wide_sweep", http.MethodGet, fmt.Sprintf("/repos/acme/widgets/check%02d", i), http.StatusOK, none)
		}
	}
	for range 9 {
		github.RecordRESTRequest("hive:narrow_sweep", http.MethodGet, "/repos/acme/widgets/issues", http.StatusOK, none)
	}
	for range 6 {
		github.RecordRESTRequest("hive:narrow_sweep", http.MethodGet, "/repos/acme/widgets/issues", http.StatusOK, revalidated)
	}
	for range 3 {
		github.RecordRESTRequest("agent:builder", http.MethodGet, "/repos/acme/widgets/pulls/7", http.StatusOK, none)
	}
	github.RecordRESTRequest("agent:builder", http.MethodGet, "/repos/acme/widgets/pulls/7", http.StatusForbidden, exhausted)
	return s
}

func ghUsageGet(t *testing.T, s *Server, path string) map[string]any {
	t.Helper()
	rec := doGet(s, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// ghUsageChargedDescending fails unless rows are most charged first.
func ghUsageChargedDescending(t *testing.T, what string, rows []any) {
	t.Helper()
	for i := 1; i < len(rows); i++ {
		prev, _ := rows[i-1].(map[string]any)["charged"].(float64)
		cur, _ := rows[i].(map[string]any)["charged"].(float64)
		if cur > prev {
			t.Fatalf("%s row %d has charged %v after %v: not most charged first", what, i, cur, prev)
		}
	}
}

// ghUsageCheckByCaller fails unless by_caller holds each recorded caller with
// exactly the counts recorded, and adds up to window_totals.
func ghUsageCheckByCaller(t *testing.T, byCaller []any, totals map[string]any) {
	t.Helper()
	seen := map[string]bool{}
	sum := map[string]float64{}
	for _, raw := range byCaller {
		row, _ := raw.(map[string]any)
		caller, _ := row["caller"].(string)
		for _, field := range []string{"requests", "charged", "not_modified", "rate_limited"} {
			n, _ := row[field].(float64)
			sum[field] += n
			if want, ok := ghUsageRecorded[caller]; ok && n != want[field] {
				t.Errorf("by_caller %s %s = %v, want %v", caller, field, n, want[field])
			}
		}
		seen[caller] = true
	}
	for caller := range ghUsageRecorded {
		if !seen[caller] {
			t.Errorf("by_caller lacks %s: %v", caller, byCaller)
		}
	}
	for _, field := range []string{"requests", "charged", "not_modified", "rate_limited"} {
		if sum[field] != totals[field] {
			t.Errorf("by_caller %s adds up to %v, window_totals says %v", field, sum[field], totals[field])
		}
	}
	// The read's own GET /rate_limit joins the window as it is read, so the
	// totals are at least what was recorded.
	if sum["charged"] < ghUsageRecordedCharged || sum["requests"] < ghUsageRecordedRequests {
		t.Errorf("by_caller totals = %v, want at least %d charged of %d requests", sum, ghUsageRecordedCharged, ghUsageRecordedRequests)
	}
}

// Without parameters the endpoint answers exactly what it answered before:
// github.RateLimitInfo's fields and nothing else, with ten top_consumers.
func TestGHRateLimitsDefaultPayloadIsUnchanged(t *testing.T) {
	s := ghUsageServer(t, "")
	rec := doGet(s, "/api/gh-rate-limits")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body.String())
	}
	// A field the dashboard's payload never had fails this strict decode.
	var info github.RateLimitInfo
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&info); err != nil {
		t.Fatalf("default payload is no longer github.RateLimitInfo: %v\n%s", err, rec.Body.String())
	}
	if len(info.TopConsumers) != 10 {
		t.Fatalf("default top_consumers = %d rows, want 10", len(info.TopConsumers))
	}
	if info.Core.Limit != 5000 || info.APIBudget.Mode != "normal" {
		t.Fatalf("core = %+v api_budget = %+v", info.Core, info.APIBudget)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"by_caller", "window_totals", "window_seconds", "agent_caps", "api_budget_thresholds", "core_reset_in_seconds", "top_consumers_total"} {
		if _, ok := body[key]; ok {
			t.Errorf("default payload carries the opt-in field %s", key)
		}
	}
	// ?top=10 is the default spelled out.
	if rows, _ := ghUsageGet(t, s, "/api/gh-rate-limits?top=10")["top_consumers"].([]any); len(rows) != 10 {
		t.Fatalf("?top=10 returned %d rows", len(rows))
	}
}

// ?top=N sets the number of top_consumers rows, most charged first, bounded
// at ghRateLimitsMaxTop; a value that is not a whole number of at least 1 is
// refused.
func TestGHRateLimitsTopParameterIsHonouredAndBounded(t *testing.T) {
	s := ghUsageServer(t, "")
	for _, tc := range []struct {
		query    string
		min, max int
	}{
		{"?top=1", 1, 1},
		{"?top=3", 3, 3},
		{"?top=25", 25, 25},
		// Every row there is: the 32 recorded, plus the read's own
		// GET /rate_limit row when the client's transport records it.
		{"?top=50", ghUsageRecordedRows, ghRateLimitsMaxTop},
		{"?top=5000", ghUsageRecordedRows, ghRateLimitsMaxTop},
	} {
		rows, _ := ghUsageGet(t, s, "/api/gh-rate-limits"+tc.query)["top_consumers"].([]any)
		if len(rows) < tc.min || len(rows) > tc.max {
			t.Fatalf("%s returned %d rows, want %d to %d", tc.query, len(rows), tc.min, tc.max)
		}
		ghUsageChargedDescending(t, tc.query, rows)
		first, _ := rows[0].(map[string]any)
		if first["caller"] != "hive:narrow_sweep" || first["charged"] != float64(9) || first["not_modified"] != float64(6) || first["requests"] != float64(15) {
			t.Fatalf("%s first row = %#v, want hive:narrow_sweep with 9 charged of 15", tc.query, first)
		}
	}
	for _, bad := range []string{"?top=0", "?top=-3", "?top=many", "?top=2.5"} {
		rec := doGet(s, "/api/gh-rate-limits"+bad)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "top must be a whole number from 1 to 50") {
			t.Fatalf("%s = %d %s, want 400 naming the bound", bad, rec.Code, rec.Body.String())
		}
	}
}

// More rows than the bound exist: ?top above it answers with the bound.
func TestGHRateLimitsTopParameterStopsAtTheBound(t *testing.T) {
	s := ghUsageServer(t, "")
	for i := range 40 {
		github.RecordRESTRequest("hive:extra_sweep", http.MethodGet, fmt.Sprintf("/repos/acme/widgets/extra%02d", i), http.StatusOK, http.Header{})
	}
	body := ghUsageGet(t, s, "/api/gh-rate-limits?top=5000&usage=1")
	rows, _ := body["top_consumers"].([]any)
	if ghRateLimitsMaxTop != adminmcp.MaxResultLimit {
		t.Fatalf("ghRateLimitsMaxTop = %d, want the admin MCP row cap %d", ghRateLimitsMaxTop, adminmcp.MaxResultLimit)
	}
	if len(rows) != ghRateLimitsMaxTop {
		t.Fatalf("?top=5000 returned %d rows, want the bound %d", len(rows), ghRateLimitsMaxTop)
	}
	if total, _ := body["top_consumers_total"].(float64); total < ghUsageRecordedRows+40 {
		t.Fatalf("top_consumers_total = %v, want at least %d", total, ghUsageRecordedRows+40)
	}
}

// ?usage=1 adds the per-caller roll-up, the window, the budget thresholds and
// the agent caps, and leaves every github.RateLimitInfo field where it was.
func TestGHRateLimitsUsageViewAddsPerCallerTotals(t *testing.T) {
	s := ghUsageServer(t, "")
	body := ghUsageGet(t, s, "/api/gh-rate-limits?usage=1")
	for _, key := range []string{"api_budget", "core", "search", "graphql", "top_consumers", "etag_cache", "pr_detail_cache", "graphql_pr_batch", "webhooks"} {
		if _, ok := body[key]; !ok {
			t.Errorf("usage view lost the github.RateLimitInfo field %s", key)
		}
	}
	if rows, _ := body["top_consumers"].([]any); len(rows) != 10 {
		t.Fatalf("usage view without top returned %d rows, want the default 10", len(rows))
	}
	byCaller, _ := body["by_caller"].([]any)
	totals, _ := body["window_totals"].(map[string]any)
	ghUsageCheckByCaller(t, byCaller, totals)
	ghUsageChargedDescending(t, "by_caller", byCaller)
	if first, _ := byCaller[0].(map[string]any); first["caller"] != "hive:wide_sweep" {
		t.Fatalf("by_caller leads with %v, want hive:wide_sweep: 60 charged over 30 endpoints, though no row of it is in the top ten by more than 2", first["caller"])
	}
	if body["window_seconds"] != float64(3600) {
		t.Fatalf("window_seconds = %v, want 3600", body["window_seconds"])
	}
	if total, _ := body["top_consumers_total"].(float64); total < ghUsageRecordedRows {
		t.Fatalf("top_consumers_total = %v, want at least %d", total, ghUsageRecordedRows)
	}
	thresholds, _ := body["api_budget_thresholds"].(map[string]any)
	if thresholds["reserve"] != float64(900) || thresholds["critical"] != float64(300) || thresholds["agent_reserve_floor"] != float64(config.DefaultGitHubAgentReserveFloor) {
		t.Fatalf("api_budget_thresholds = %#v, want the configured 900/300 and the default agent floor", thresholds)
	}
	// The mock's core bucket resets in an hour.
	if left, _ := body["core_reset_in_seconds"].(float64); left < 3000 || left > 3600 {
		t.Fatalf("core_reset_in_seconds = %v, want about an hour", body["core_reset_in_seconds"])
	}
	caps, _ := body["agent_caps"].(map[string]any)
	agents, _ := caps["agents"].([]any)
	if len(agents) != 1 || agents[0].(map[string]any)["agent"] != "scanner" || agents[0].(map[string]any)["calls"] != float64(7) ||
		agents[0].(map[string]any)["cap"] != float64(config.DefaultAgentsGitHubAPIHourlyCap) {
		t.Fatalf("agent_caps = %#v, want scanner with 7 calls against the default cap", caps)
	}
	// It is the status payload's ghRateLimits.agent_caps, field for field.
	raw, err := json.Marshal(buildAgentGitHubCaps(s.deps.Config))
	if err != nil {
		t.Fatal(err)
	}
	var statusCaps any
	if err := json.Unmarshal(raw, &statusCaps); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(caps) != fmt.Sprint(statusCaps) {
		t.Fatalf("agent_caps = %v, want the status payload's %v", caps, statusCaps)
	}
}

// The admin MCP tool returns every section from the endpoint's usage view,
// with top_consumers cut to its limit and by_caller whole.
func TestAdminMCPGitHubAPIUsageReadsTheUsageView(t *testing.T) {
	s := ghUsageServer(t, "")
	provider := dashboardAdminMCPProvider{server: s, user: "alice", role: config.RoleRead}

	got, err := provider.Read(context.Background(), adminmcp.ToolGitHubAPIUsage, map[string]any{"limit": float64(5)})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := got.(map[string]any)
	if out["type"] != adminmcp.ToolGitHubAPIUsage || out["usage_view"] != true {
		t.Fatalf("type/usage_view = %v/%v", out["type"], out["usage_view"])
	}
	rows, _ := out["top_consumers"].([]any)
	if len(rows) != 5 || out["limit"] != 5 || out["top_consumers_truncated"] != true {
		t.Fatalf("limit 5: %d rows, limit %v, truncated %v", len(rows), out["limit"], out["top_consumers_truncated"])
	}
	ghUsageChargedDescending(t, "top_consumers", rows)
	first, _ := rows[0].(map[string]any)
	for _, field := range []string{"caller", "endpoint", "method", "requests", "charged", "not_modified", "rate_limited", "last_status", "last_seen"} {
		if _, ok := first[field]; !ok {
			t.Errorf("top_consumers row lacks %s: %#v", field, first)
		}
	}
	if first["caller"] != "hive:narrow_sweep" || first["endpoint"] != "/repos/{owner}/{repo}/issues" || first["charged"] != float64(9) {
		t.Fatalf("first row = %#v", first)
	}
	byCaller, _ := out["by_caller"].([]any)
	totals, _ := out["window_totals"].(map[string]any)
	ghUsageCheckByCaller(t, byCaller, totals)

	core, _ := out["core"].(map[string]any)
	if core["limit"] != float64(5000) || core["remaining"] == nil || core["reset"] == nil || core["observed_at"] == nil {
		t.Fatalf("core = %#v", core)
	}
	if left, _ := core["reset_in_seconds"].(float64); left < 3000 || left > 3600 {
		t.Fatalf("core.reset_in_seconds = %v, want about an hour", core["reset_in_seconds"])
	}
	budget, _ := out["api_budget"].(map[string]any)
	if budget["mode"] != "normal" || budget["since"] == nil || budget["skipped_steps"] == nil || budget["reserve"] != float64(900) || budget["critical"] != float64(300) {
		t.Fatalf("api_budget = %#v", budget)
	}
	if out["window_seconds"] != float64(3600) {
		t.Fatalf("window_seconds = %v", out["window_seconds"])
	}
	caps, _ := out["agent_caps"].(map[string]any)
	if agents, _ := caps["agents"].([]any); len(agents) != 1 || agents[0].(map[string]any)["calls"] != float64(7) {
		t.Fatalf("agent_caps = %#v", caps)
	}
	cache, _ := out["etag_cache"].(map[string]any)
	for _, field := range []string{"hits", "misses", "entries"} {
		if _, ok := cache[field]; !ok {
			t.Errorf("etag_cache lacks %s: %#v", field, cache)
		}
	}

	// The default limit is 20, and a limit over the cap falls back to it.
	for _, args := range []map[string]any{{}, {"limit": float64(500)}} {
		got, err := provider.Read(context.Background(), adminmcp.ToolGitHubAPIUsage, args)
		if err != nil {
			t.Fatal(err)
		}
		if rows, _ := got.(map[string]any)["top_consumers"].([]any); len(rows) != adminmcp.DefaultResultLimit {
			t.Fatalf("args %v returned %d rows, want %d", args, len(rows), adminmcp.DefaultResultLimit)
		}
	}
}

// The endpoint asks for a signed-in caller and nothing more, and so does the
// tool: a caller with no credentials is refused both the same way, and a
// read-only caller reads both.
func TestAdminMCPGitHubAPIUsageNeedsWhatTheEndpointNeeds(t *testing.T) {
	s := ghUsageServer(t, "secret")
	h := s.authenticate(s.roleEnforcement(s.securityHeaders(s.mux)))
	const call = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"github_api_usage","arguments":{"limit":3}}}`

	serve := func(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		h.ServeHTTP(rec, req)
		return rec
	}

	endpoint := serve(http.MethodGet, "/api/gh-rate-limits?usage=1", "", nil)
	tool := serve(http.MethodPost, adminmcp.EndpointPath, call, nil)
	if endpoint.Code != http.StatusUnauthorized || tool.Code != endpoint.Code {
		t.Fatalf("no credentials: endpoint %d, tool %d, want 401 for both", endpoint.Code, tool.Code)
	}
	if strings.TrimSpace(tool.Body.String()) != strings.TrimSpace(endpoint.Body.String()) || strings.Contains(tool.Body.String(), "hive:narrow_sweep") {
		t.Fatalf("no credentials: endpoint answered %q, tool %q, want the same refusal and no accounting rows", endpoint.Body.String(), tool.Body.String())
	}

	// A read-only caller reads the endpoint through the full auth chain.
	reader := map[string]string{"X-Hive-User": "alice", "X-Hive-Role": config.RoleRead, proxyAuthHeader: "secret"}
	if rec := serve(http.MethodGet, "/api/gh-rate-limits?usage=1", "", reader); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hive:narrow_sweep") {
		t.Fatalf("read-only caller on the endpoint = %d %s", rec.Code, rec.Body.String())
	}
	// The tool makes that same GET as the caller, so the same caller reads it.
	got, err := dashboardAdminMCPProvider{server: s, user: "alice", role: config.RoleRead}.Read(context.Background(), adminmcp.ToolGitHubAPIUsage, map[string]any{"limit": float64(3)})
	if err != nil {
		t.Fatalf("read-only caller on the tool: %v", err)
	}
	if rows, _ := got.(map[string]any)["top_consumers"].([]any); len(rows) != 3 {
		t.Fatalf("read-only caller on the tool got %d rows, want 3", len(rows))
	}

	// Over the HTTP endpoint, through the full chain and the outbound scrub.
	text := adminMCPAs(t, h, map[string]string{"X-Hive-User": "bob", "X-Hive-Role": config.RoleReadWrite, proxyAuthHeader: "secret"})(call)
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("tools/call github_api_usage: %v: %s", err, text)
	}
	rows, _ := envelope.Data["top_consumers"].([]any)
	if len(rows) != 3 || rows[0].(map[string]any)["caller"] != "hive:narrow_sweep" {
		t.Fatalf("tools/call github_api_usage = %s", text)
	}
	if byCaller, _ := envelope.Data["by_caller"].([]any); len(byCaller) < len(ghUsageRecorded) {
		t.Fatalf("by_caller over HTTP = %v", envelope.Data["by_caller"])
	}
	if strings.Contains(text, "[masked:") || strings.Contains(text, "redacted:") {
		t.Fatalf("the scrub changed an accounting field: %s", text)
	}
}
