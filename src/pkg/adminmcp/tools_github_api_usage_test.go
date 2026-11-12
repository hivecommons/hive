package adminmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sampleGitHubAPIUsage is a decoded GET /api/gh-rate-limits?usage=1 answer.
// Its top_consumers are deliberately out of order, so the tests show the tool
// sorts them itself.
func sampleGitHubAPIUsage() map[string]any {
	row := func(caller, endpoint, method string, requests, charged, notModified, rateLimited float64) map[string]any {
		return map[string]any{
			"caller": caller, "endpoint": endpoint, "method": method,
			"requests": requests, "charged": charged, "not_modified": notModified, "rate_limited": rateLimited,
			"last_status": float64(200), "last_seen": "2026-11-12T19:00:00Z",
		}
	}
	return map[string]any{
		"core":                  map[string]any{"limit": float64(15000), "remaining": float64(612), "reset": "2026-11-12T19:40:00Z", "observed_at": "2026-11-12T19:00:05Z"},
		"core_reset_in_seconds": float64(2395),
		"graphql":               map[string]any{"limit": float64(5000), "remaining": float64(4100), "reset": "2026-11-12T19:30:00Z"},
		"search":                map[string]any{"limit": float64(30), "remaining": float64(30), "reset": "2026-11-12T19:01:00Z"},
		"api_budget":            map[string]any{"mode": "conserve", "remaining": float64(612), "limit": float64(15000), "reset": "2026-11-12T19:40:00Z", "since": "2026-11-12T18:52:00Z", "skipped_steps": []any{"recommendations", "duplicate_guard"}},
		"api_budget_thresholds": map[string]any{"reserve": float64(800), "critical": float64(250), "agent_reserve_floor": float64(400)},
		"top_consumers": []any{
			row("hive:issue_unpark", "/repos/{owner}/{repo}/issues/{number}/comments", "GET", 900, 40, 860, 0),
			row("agent:builder", "/repos/{owner}/{repo}/pulls/{number}", "GET", 310, 300, 10, 2),
			row("hive:eval_cycle", "/repos/{owner}/{repo}/pulls", "GET", 500, 120, 380, 0),
			row("hive:metrics_collector", "/repos/{owner}/{repo}/contributors", "GET", 60, 60, 0, 0),
		},
		"top_consumers_total": float64(9),
		"by_caller": []any{
			map[string]any{"caller": "agent:builder", "requests": float64(420), "charged": float64(400), "not_modified": float64(20), "rate_limited": float64(2)},
			map[string]any{"caller": "hive:eval_cycle", "requests": float64(700), "charged": float64(180), "not_modified": float64(520), "rate_limited": float64(0)},
			map[string]any{"caller": "hive:metrics_collector", "requests": float64(60), "charged": float64(60), "not_modified": float64(0), "rate_limited": float64(0)},
			map[string]any{"caller": "hive:issue_unpark", "requests": float64(900), "charged": float64(40), "not_modified": float64(860), "rate_limited": float64(0)},
		},
		"window_totals":  map[string]any{"requests": float64(2080), "charged": float64(680), "not_modified": float64(1400), "rate_limited": float64(2)},
		"window_seconds": float64(3600),
		"agent_caps": map[string]any{"fleet_cap": float64(300), "fleet_cap_set": false, "window_s": float64(3600), "agents": []any{
			map[string]any{"agent": "builder", "cap": float64(300), "source": "fleet", "calls": float64(300)},
		}},
		"etag_cache": map[string]any{"hits": float64(1400), "misses": float64(680), "entries": float64(512)},
	}
}

func ghUsageResult(t *testing.T, payload any, args map[string]any) map[string]any {
	t.Helper()
	out, err := GitHubAPIUsageResult(payload, args)
	if err != nil {
		t.Fatal(err)
	}
	body, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("result = %#v", out)
	}
	return body
}

func ghUsageCallers(rows any) string {
	list, _ := rows.([]any)
	names := make([]string, 0, len(list))
	for _, raw := range list {
		row, _ := raw.(map[string]any)
		name, _ := row["caller"].(string)
		names = append(names, name)
	}
	return strings.Join(names, ",")
}

func TestGitHubAPIUsageToolIsListedReadOnlyAndSaysItCostsNoCoreCall(t *testing.T) {
	if !AllowedTool(ToolGitHubAPIUsage) {
		t.Fatal("github_api_usage is not an allowed tool")
	}
	var found bool
	for _, def := range Tools() {
		if def["name"] != ToolGitHubAPIUsage {
			continue
		}
		found = true
		if ann, _ := def["annotations"].(map[string]any); ann["readOnlyHint"] != true {
			t.Fatalf("annotations = %#v", ann)
		}
		schema, _ := def["inputSchema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props["limit"]; !ok || len(props) != 1 {
			t.Fatalf("github_api_usage takes limit only, schema properties = %#v", props)
		}
		desc, _ := def["description"].(string)
		for _, want := range []string{"/api/gh-rate-limits", "spends no core API call", "GET /rate_limit", "Read-only", "by_caller", "window_seconds"} {
			if !strings.Contains(desc, want) {
				t.Errorf("description does not say %q: %s", want, desc)
			}
		}
	}
	if !found {
		t.Fatal("github_api_usage not listed")
	}
}

// The tool makes one GET: the rate-limit endpoint with as many top rows as
// its limit and the per-caller view. A limit outside 1-50 falls back to the
// default like every other tool's.
func TestGitHubAPIUsageReadPathAsksForLimitRowsAndTheUsageView(t *testing.T) {
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{}, "/api/gh-rate-limits?top=20&usage=1"},
		{map[string]any{"limit": float64(5)}, "/api/gh-rate-limits?top=5&usage=1"},
		{map[string]any{"limit": float64(50)}, "/api/gh-rate-limits?top=50&usage=1"},
		{map[string]any{"limit": float64(500)}, "/api/gh-rate-limits?top=20&usage=1"},
		{map[string]any{"limit": float64(0)}, "/api/gh-rate-limits?top=20&usage=1"},
	} {
		got := GitHubAPIUsageReadPath(tc.args)
		if got != tc.want {
			t.Errorf("path for %v = %q, want %q", tc.args, got, tc.want)
		}
		if err := CheckRequestPath(got); err != nil {
			t.Errorf("path %q refused: %v", got, err)
		}
	}
}

func TestGitHubAPIUsageResultCarriesEverySection(t *testing.T) {
	out := ghUsageResult(t, sampleGitHubAPIUsage(), map[string]any{})
	if out["type"] != ToolGitHubAPIUsage || out["usage_view"] != true {
		t.Fatalf("type/usage_view = %v/%v", out["type"], out["usage_view"])
	}
	core, _ := out["core"].(map[string]any)
	if core["limit"] != float64(15000) || core["remaining"] != float64(612) || core["reset"] != "2026-11-12T19:40:00Z" ||
		core["observed_at"] != "2026-11-12T19:00:05Z" || core["reset_in_seconds"] != float64(2395) {
		t.Fatalf("core = %#v", core)
	}
	budget, _ := out["api_budget"].(map[string]any)
	steps, _ := budget["skipped_steps"].([]any)
	if budget["mode"] != "conserve" || budget["since"] != "2026-11-12T18:52:00Z" || len(steps) != 2 ||
		budget["reserve"] != float64(800) || budget["critical"] != float64(250) || budget["agent_reserve_floor"] != float64(400) {
		t.Fatalf("api_budget = %#v", budget)
	}
	if got := ghUsageCallers(out["top_consumers"]); got != "agent:builder,hive:eval_cycle,hive:metrics_collector,hive:issue_unpark" {
		t.Fatalf("top_consumers order = %s, want most charged first", got)
	}
	first := out["top_consumers"].([]any)[0].(map[string]any)
	for _, field := range []string{"caller", "endpoint", "method", "requests", "charged", "not_modified", "rate_limited", "last_status", "last_seen"} {
		if _, ok := first[field]; !ok {
			t.Errorf("top_consumers row lacks %s: %#v", field, first)
		}
	}
	if got := ghUsageCallers(out["by_caller"]); got != "agent:builder,hive:eval_cycle,hive:metrics_collector,hive:issue_unpark" {
		t.Fatalf("by_caller order = %s, want most charged first", got)
	}
	// by_caller is the hive's whole window, not a sum of the rows shown: its
	// charged adds up to window_totals.charged (680), which the four
	// top_consumers rows (520) do not reach.
	var byCallerCharged, topCharged float64
	for _, raw := range out["by_caller"].([]any) {
		byCallerCharged += raw.(map[string]any)["charged"].(float64)
	}
	for _, raw := range out["top_consumers"].([]any) {
		topCharged += raw.(map[string]any)["charged"].(float64)
	}
	totals, _ := out["window_totals"].(map[string]any)
	if byCallerCharged != 680 || totals["charged"] != float64(680) || topCharged != 520 {
		t.Fatalf("charged: by_caller %v, window_totals %v, top_consumers %v", byCallerCharged, totals["charged"], topCharged)
	}
	if out["window_seconds"] != float64(3600) {
		t.Fatalf("window_seconds = %v", out["window_seconds"])
	}
	caps, _ := out["agent_caps"].(map[string]any)
	agents, _ := caps["agents"].([]any)
	if len(agents) != 1 || agents[0].(map[string]any)["agent"] != "builder" || agents[0].(map[string]any)["calls"] != float64(300) {
		t.Fatalf("agent_caps = %#v", caps)
	}
	cache, _ := out["etag_cache"].(map[string]any)
	if cache["hits"] != float64(1400) || cache["misses"] != float64(680) || cache["entries"] != float64(512) {
		t.Fatalf("etag_cache = %#v", cache)
	}
	if _, ok := out["graphql"].(map[string]any); !ok {
		t.Fatalf("graphql bucket missing: %#v", out["graphql"])
	}
	// Nine rows exist and four came back, so the cut is disclosed.
	meta, _ := out["_admin_mcp"].(map[string]any)
	if out["top_consumers_total"] != 9 || out["top_consumers_truncated"] != true || meta["total"] != 9 {
		t.Fatalf("truncation: total=%v truncated=%v meta=%#v", out["top_consumers_total"], out["top_consumers_truncated"], meta)
	}
}

func TestGitHubAPIUsageResultHonoursAndBoundsLimit(t *testing.T) {
	out := ghUsageResult(t, sampleGitHubAPIUsage(), map[string]any{"limit": float64(2)})
	if got := ghUsageCallers(out["top_consumers"]); got != "agent:builder,hive:eval_cycle" {
		t.Fatalf("limit 2 rows = %s, want the two most charged", got)
	}
	if out["limit"] != 2 || out["top_consumers_truncated"] != true {
		t.Fatalf("limit=%v truncated=%v", out["limit"], out["top_consumers_truncated"])
	}
	// by_caller is never cut by limit.
	if rows, _ := out["by_caller"].([]any); len(rows) != 4 {
		t.Fatalf("by_caller rows = %d, want all 4 whatever limit says", len(rows))
	}

	// A limit over the cap is the default, as for the other tools; a hive
	// that answers with more rows than asked is still cut to it.
	many := sampleGitHubAPIUsage()
	rows := make([]any, 0, 60)
	for i := range 60 {
		rows = append(rows, map[string]any{"caller": "hive:sweep", "endpoint": "/repos/{owner}/{repo}/issues", "method": "GET", "charged": float64(i)})
	}
	many["top_consumers"] = rows
	many["top_consumers_total"] = float64(60)
	out = ghUsageResult(t, many, map[string]any{"limit": float64(500)})
	got, _ := out["top_consumers"].([]any)
	if len(got) != DefaultResultLimit || out["limit"] != DefaultResultLimit {
		t.Fatalf("limit 500 returned %d rows with limit %v, want the default %d", len(got), out["limit"], DefaultResultLimit)
	}
	if got[0].(map[string]any)["charged"] != float64(59) || got[DefaultResultLimit-1].(map[string]any)["charged"] != float64(60-DefaultResultLimit) {
		t.Fatalf("rows are not the %d most charged: first %v last %v", DefaultResultLimit, got[0], got[DefaultResultLimit-1])
	}
}

// A hive older than the per-caller view ignores ?usage=1. The tool still
// answers with what that hive returns and says what is missing.
func TestGitHubAPIUsageResultFromAHiveWithoutTheUsageView(t *testing.T) {
	old := sampleGitHubAPIUsage()
	for _, key := range []string{"core_reset_in_seconds", "api_budget_thresholds", "top_consumers_total", "by_caller", "window_totals", "window_seconds", "agent_caps"} {
		delete(old, key)
	}
	delete(old["api_budget"].(map[string]any), "skipped_steps")
	out := ghUsageResult(t, old, map[string]any{})
	if out["usage_view"] != false || !strings.Contains(out["note"].(string), "Upgrade the hive") {
		t.Fatalf("usage_view=%v note=%v", out["usage_view"], out["note"])
	}
	for _, key := range []string{"by_caller", "window_totals", "window_seconds", "agent_caps"} {
		if _, ok := out[key]; ok {
			t.Errorf("%s must be absent when the hive did not send it", key)
		}
	}
	if rows, _ := out["top_consumers"].([]any); len(rows) != 4 || out["top_consumers_total"] != 4 {
		t.Fatalf("top_consumers = %d rows, total %v", len(rows), out["top_consumers_total"])
	}
	if _, ok := out["top_consumers_truncated"]; ok {
		t.Fatal("nothing says the list was cut, so it must not be marked truncated")
	}
	budget, _ := out["api_budget"].(map[string]any)
	if steps, ok := budget["skipped_steps"].([]any); !ok || len(steps) != 0 {
		t.Fatalf("skipped_steps = %#v, want an empty list when the hive skipped nothing", budget["skipped_steps"])
	}
	if _, err := GitHubAPIUsageResult([]any{}, map[string]any{}); err == nil {
		t.Fatal("a non-object answer must be an error")
	}
}

type ghUsageProvider struct {
	payload map[string]any
	paths   []string
}

func (p *ghUsageProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if tool != ToolGitHubAPIUsage {
		return nil, ErrForbidden
	}
	p.paths = append(p.paths, GitHubAPIUsageReadPath(args))
	return GitHubAPIUsageResult(p.payload, args)
}

// Through the handler the answer passes the admin MCP's normal scrub. Caller
// and endpoint names are operational data: none of the tool's fields is
// masked, while a credential-shaped value in a row still is.
func TestGitHubAPIUsageThroughTheHandlerIsScrubbedNotMasked(t *testing.T) {
	payload := sampleGitHubAPIUsage()
	leaked := "gh" + "p_" + strings.Repeat("a", 36)
	// Row 1 is agent:builder, the most charged, so it survives the limit.
	rows := payload["top_consumers"].([]any)
	rows[1].(map[string]any)["endpoint"] = "/repos/{owner}/{repo}/pulls/{number}?access=" + leaked
	provider := &ghUsageProvider{payload: payload}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"github_api_usage","arguments":{"limit":3}}}`))
	NewHandler(provider).ServeHTTP(rec, req)
	text := resultText(t, rec.Body.Bytes())
	if len(provider.paths) != 1 || provider.paths[0] != "/api/gh-rate-limits?top=3&usage=1" {
		t.Fatalf("paths = %q", provider.paths)
	}
	if strings.Contains(text, leaked) || !strings.Contains(text, "redacted:github-token") {
		t.Fatalf("a credential-shaped value survived the scrub: %s", text)
	}
	if strings.Contains(text, "[masked:") {
		t.Fatalf("a field of the answer was masked by its key: %s", text)
	}
	var envelope struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatal(err)
	}
	data := envelope.Data
	for _, key := range []string{"core", "api_budget", "top_consumers", "by_caller", "window_totals", "window_seconds", "agent_caps", "etag_cache"} {
		if _, ok := data[key]; !ok {
			t.Errorf("answer lacks %s", key)
		}
	}
	if got := ghUsageCallers(data["top_consumers"]); got != "agent:builder,hive:eval_cycle,hive:metrics_collector" {
		t.Fatalf("top_consumers through the handler = %s", got)
	}
	budget, _ := data["api_budget"].(map[string]any)
	if budget["mode"] != "conserve" || budget["reserve"] != float64(800) {
		t.Fatalf("api_budget through the handler = %#v", budget)
	}
}
