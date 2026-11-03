package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

func TestAdminMCPIssuesByBandReadPathOmitsBandAndLimit(t *testing.T) {
	path, ok := adminMCPReadPath(adminmcp.ToolIssuesByBand, map[string]any{"repo": "owner/name", "band": "done", "stale": true, "limit": float64(5)})
	if !ok {
		t.Fatal("expected ok")
	}
	if !strings.HasPrefix(path, "/api/overview/issues.json?") {
		t.Fatalf("path = %q", path)
	}
	if !strings.Contains(path, "repo=owner%2Fname") || !strings.Contains(path, "stale=true") {
		t.Fatalf("path = %q, want repo and stale forwarded", path)
	}
	if strings.Contains(path, "band=") || strings.Contains(path, "limit=") {
		t.Fatalf("path = %q, must not forward band or limit — the tool filters/caps rows itself so bands[] keeps its real counts", path)
	}
}

func TestAdminMCPPrsByBandReadPathWithoutFilters(t *testing.T) {
	path, ok := adminMCPReadPath(adminmcp.ToolPrsByBand, map[string]any{})
	if !ok || path != "/api/overview/prs.json" {
		t.Fatalf("path = %q ok=%v", path, ok)
	}
}

func TestDashboardAdminMCPIssuesByBandFiltersRowsButKeepsBands(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/overview/issues.json", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("band") != "" {
			t.Fatalf("handler saw a band filter %q; the tool must fetch every band", r.URL.Query().Get("band"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"bands": []any{
				map[string]any{"key": "ready", "label": "Unclaimed", "rule": "no other band matched", "count": 1},
				map[string]any{"key": "done", "label": "Confirm & close", "rule": "verify and close", "count": 2},
			},
			// Rows carry the display label, matching overviewIssueCSVRow /
			// overviewPRCSVRow (#10018) — not the "ready"/"done" keys a
			// hand-built key-shaped fixture would use.
			"rows": []any{
				map[string]any{"number": 1, "band": "Unclaimed"},
				map[string]any{"number": 2, "band": "Confirm & close"},
				map[string]any{"number": 3, "band": "Confirm & close"},
			},
		})
	})
	provider := dashboardAdminMCPProvider{server: s}
	result, err := provider.Read(context.Background(), adminmcp.ToolIssuesByBand, map[string]any{"band": "done"})
	if err != nil {
		t.Fatal(err)
	}
	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result = %#v", result)
	}
	bands, ok := body["bands"].([]any)
	if !ok || len(bands) != 2 {
		t.Fatalf("bands = %#v, want both bands even when filtered", body["bands"])
	}
	rows, ok := body["rows"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("rows = %#v, want only the 2 done-band rows", body["rows"])
	}
	for _, row := range rows {
		m := row.(map[string]any)
		if m["band"] != "done" {
			t.Fatalf("row band = %#v, want the rekeyed band key \"done\", not its display label", m["band"])
		}
	}
}

func TestDashboardAdminMCPIssuesByBandRefusesUnknownBand(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/overview/issues.json", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"bands": []any{}, "rows": []any{}})
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"issues_by_band","arguments":{"band":"nope"}}}`))
	adminmcp.NewHandler(dashboardAdminMCPProvider{server: s}).ServeHTTP(rec, req)
	// A tools/call result carries the tool payload as a JSON string in
	// content[0].text, so decode it before asserting on the refusal fields.
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rpc); err != nil || len(rpc.Result.Content) == 0 {
		t.Fatalf("body = %s (err %v)", rec.Body.String(), err)
	}
	var env struct {
		Data adminmcp.RefusalData `json:"data"`
	}
	if err := json.Unmarshal([]byte(rpc.Result.Content[0].Text), &env); err != nil {
		t.Fatalf("tool text = %s (err %v)", rpc.Result.Content[0].Text, err)
	}
	if env.Data.Type != "refusal" || env.Data.Kind != adminmcp.RefusalKindInvalidArgument || !strings.Contains(env.Data.Reason, "ready") {
		t.Fatalf("refusal = %+v", env.Data)
	}
}

// TestDashboardAdminMCPBandFilterMatchesRowKeyNotLabel feeds the real
// overviewIssueCSVRow/overviewPRCSVRow row builders through the dashboard's
// Overview endpoints (rather than hand-built "band": "done"-shaped fixtures)
// so a regression where rows carry the display label instead of the band
// key (#10018) fails here instead of only in production.
func TestDashboardAdminMCPBandFilterMatchesRowKeyNotLabel(t *testing.T) {
	s := NewServerWithAuth(0, "secret", nil)
	s.deps = &Dependencies{Config: &config.Config{HiveID: "h1", Dashboard: config.DashboardConfig{AuthToken: "secret"}}}
	s.statusMu.Lock()
	s.status = parityStatus9102(t)
	s.statusMu.Unlock()

	provider := dashboardAdminMCPProvider{server: s, authorization: "Bearer secret"}

	issues, err := provider.Read(context.Background(), adminmcp.ToolIssuesByBand, map[string]any{"band": "done"})
	if err != nil {
		t.Fatal(err)
	}
	issueBody, ok := issues.(map[string]any)
	if !ok {
		t.Fatalf("issues result = %#v", issues)
	}
	issueRows, ok := issueBody["rows"].([]any)
	if !ok || len(issueRows) == 0 {
		t.Fatalf("rows = %#v, want the hive/likely-done issue (#6) to match band=done", issueBody["rows"])
	}
	for _, row := range issueRows {
		m := row.(map[string]any)
		if m["band"] != "done" {
			t.Fatalf("row band = %#v, want the band key \"done\" not its display label", m["band"])
		}
	}

	prs, err := provider.Read(context.Background(), adminmcp.ToolPrsByBand, map[string]any{"band": "blocked"})
	if err != nil {
		t.Fatal(err)
	}
	prBody, ok := prs.(map[string]any)
	if !ok {
		t.Fatalf("prs result = %#v", prs)
	}
	prRows, ok := prBody["rows"].([]any)
	if !ok || len(prRows) == 0 {
		t.Fatalf("rows = %#v, want the failing-CI PR (#12) to match band=blocked", prBody["rows"])
	}
	for _, row := range prRows {
		m := row.(map[string]any)
		if m["band"] != "blocked" {
			t.Fatalf("row band = %#v, want the band key \"blocked\" not its display label", m["band"])
		}
	}
}

// TestDashboardAdminMCPReviewQueueUsesRealOverviewRows runs review_queue over
// the real Overview row builders, so the queue sees the rows exactly as
// production serves them (display-label bands, #10018) and lists the your-turn
// items the server decided, nothing it derives from labels or bands.
func TestDashboardAdminMCPReviewQueueUsesRealOverviewRows(t *testing.T) {
	s := NewServerWithAuth(0, "secret", nil)
	s.deps = &Dependencies{Config: &config.Config{HiveID: "h1", Dashboard: config.DashboardConfig{AuthToken: "secret"}}}
	status := parityStatus9102(t)
	decideRepoOwnership(status.Repos, config.DashboardIssueBandsConfig{}, ownershipEnv11565(5))
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()

	result, err := dashboardAdminMCPProvider{server: s}.Read(context.Background(), adminmcp.ToolReviewQueue, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Total  int `json:"total"`
		Counts struct {
			YourTurn int `json:"your_turn"`
			ToClose  int `json:"to_close"`
		} `json:"counts"`
		Rows []struct {
			Kind       string  `json:"kind"`
			Number     float64 `json:"number"`
			Verb       string  `json:"verb"`
			ReasonCode string  `json:"reason_code"`
			Reason     string  `json:"reason"`
			InboxRank  int     `json:"inbox_rank"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatalf("decode %s: %v", encoded, err)
	}

	// The expected rows are the stored decisions, in inbox_rank order.
	type want struct{ verb, code string }
	wantRows := map[string]want{}
	ranks := map[int]string{}
	var n, m int
	for _, repo := range status.Repos {
		for key, o := range repo.Ownership {
			if !o.YourTurn {
				continue
			}
			kind, num, _ := strings.Cut(key, "#")
			id := fmt.Sprintf("%s#%s", kind, num)
			wantRows[id] = want{o.Verb, o.ReasonCode}
			ranks[o.InboxRank] = id
			if o.Verb == OwnershipVerbClose {
				m++
			} else {
				n++
			}
		}
	}
	if len(wantRows) == 0 {
		t.Fatal("fixture has no your-turn items")
	}
	if body.Total != len(wantRows) || len(body.Rows) != len(wantRows) || body.Counts.YourTurn != n || body.Counts.ToClose != m {
		t.Fatalf("queue = %s, want %d rows, N=%d M=%d", encoded, len(wantRows), n, m)
	}
	for i, row := range body.Rows {
		id := fmt.Sprintf("%s#%d", row.Kind, int(row.Number))
		if row.Reason == "" || row.InboxRank != i+1 || ranks[row.InboxRank] != id || wantRows[id] != (want{row.Verb, row.ReasonCode}) {
			t.Fatalf("row %d = %+v, want %s %+v in inbox order", i, row, id, wantRows[id])
		}
	}
}

func TestDashboardAdminMCPGovernorSetupReadsGovernorSettings(t *testing.T) {
	s := &Server{mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /api/config/governor", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"repoCount":           12,
			"cadenceScope":        "aggregate",
			"thresholdScaling":    "linear",
			"effectiveThresholds": map[string]any{"quiet": 24, "busy": 120, "surge": 240},
			"pinnedThresholds":    map[string]any{},
			"budget":              map[string]any{"totalTokens": 0, "periodDays": 7, "criticalPct": 90},
		})
	})
	result, err := dashboardAdminMCPProvider{server: s}.Read(context.Background(), adminmcp.ToolGovernorSetup, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"type":"governor_setup_proposal"`, `"operation":"governor.threshold_scaling"`, `"scaling":"sqrt"`, `"owner_input":["totalTokens"]`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("proposal = %s, missing %s", encoded, want)
		}
	}
}
