package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func hubWidgetTestServer(t *testing.T) *HubServer {
	t.Helper()
	t.Setenv(hubAdminsEnv, "github:widget-admin")
	cleanup := helperSetupTempDirs(t)
	t.Cleanup(cleanup)
	s := newHubServerForTest(t)
	beat := time.Now().UTC().Format(time.RFC3339)
	s.mu.Lock()
	s.registry.Hives = []RegistryEntry{
		{
			ID: "widget-owned", Name: "acme/api", Owner: "widget-owner", Online: true,
			ACMMLevel: 3, OpenIssues: 20, OpenPRs: 8, ActionableIssues: 1, ActionablePRs: 2,
			AgentCount: 2, Agents: []AgentSummary{{Name: "a", State: "running"}, {Name: "b", State: "running"}, {Name: "c", State: "idle"}},
			LastHeartbeat: beat,
			IssueHistory:  []SparkPoint{{T: 900, V: 3}, {T: 1800, V: 1}},
			PRHistory:     []SparkPoint{{T: 900, V: 0}, {T: 1800, V: 2}},
		},
		{
			ID: "widget-shared", Name: "acme/web", ProjectName: "Acme Web", Owner: "someone-else", Online: true,
			ACMMLevel: 4, OpenIssues: 5, OpenPRs: 1, ActionableIssues: 4, ActionablePRs: 3,
			LastHeartbeat: beat,
			IssueHistory:  []SparkPoint{{T: 1800, V: 4}},
			PRHistory:     []SparkPoint{{T: 1800, V: 3}},
		},
		{
			ID: "widget-foreign", Name: "other/repo", Owner: "someone-else", Online: false,
			OpenIssues: 99, ActionableIssues: 7, LastHeartbeat: beat,
		},
		{
			ID: "hosted-available-widget", Org: placeholderOrgPrefix + "widget", Owner: "widget-owner",
		},
	}
	s.mu.Unlock()

	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "widget-owner", Hives: map[string]string{"widget-shared": "read"}}); err != nil {
		t.Fatalf("save user: %v", err)
	}
	if err := saveSaaSUser(&SaaSUser{GitHubUsername: "widget-stranger", Hives: map[string]string{}}); err != nil {
		t.Fatalf("save user: %v", err)
	}
	return s
}

func getHubWidget(t *testing.T, s *HubServer, token string) (int, hubWidgetResponse) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/hub/widget", s.requireAuth(s.handleHubWidget))
	req := httptest.NewRequest(http.MethodGet, "/api/hub/widget", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var resp hubWidgetResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, resp
}

func hubWidgetIDs(resp hubWidgetResponse) []string {
	ids := make([]string, 0, len(resp.Hives))
	for _, h := range resp.Hives {
		ids = append(ids, h.ID)
	}
	return ids
}

func TestHubWidget_Unauthenticated(t *testing.T) {
	s := hubWidgetTestServer(t)
	if code, _ := getHubWidget(t, s, ""); code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", code)
	}
}

func TestHubWidget_MemberSeesOwnedAndSharedHives(t *testing.T) {
	s := hubWidgetTestServer(t)
	defer helperSetupAuthUser(t, "ghp_widget_owner", "widget-owner")()

	code, resp := getHubWidget(t, s, "ghp_widget_owner")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	ids := hubWidgetIDs(resp)
	// Sorted by act_issues+act_prs, highest first; placeholder and foreign
	// hives are not visible.
	if len(ids) != 2 || ids[0] != "widget-shared" || ids[1] != "widget-owned" {
		t.Fatalf("hives = %v, want [widget-shared widget-owned]", ids)
	}

	shared := resp.Hives[0]
	if shared.Project != "Acme Web" || shared.ACMMLevel != 4 || shared.Issues != 5 || shared.PRs != 1 || shared.ActIssues != 4 || shared.ActPRs != 3 {
		t.Errorf("shared hive = %+v", shared)
	}
	owned := resp.Hives[1]
	if owned.Project != "acme/api" || owned.AgentsRunning != 2 || owned.AgentsTotal != 3 || owned.LastHeartbeat == "" {
		t.Errorf("owned hive = %+v", owned)
	}

	want := hubWidgetTotals{Hives: 2, Online: 2, NeedYou: 2, Issues: 25, PRs: 9, ActIssues: 5, ActPRs: 5, AgentsRunning: 2, AgentsTotal: 3}
	if resp.Totals != want {
		t.Errorf("totals = %+v, want %+v", resp.Totals, want)
	}
}

func TestHubWidget_AdminSeesAllHives(t *testing.T) {
	s := hubWidgetTestServer(t)
	defer helperSetupAuthUser(t, "ghp_widget_admin", "widget-admin")()

	code, resp := getHubWidget(t, s, "ghp_widget_admin")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	ids := hubWidgetIDs(resp)
	if len(ids) != 3 || ids[0] != "widget-foreign" {
		t.Fatalf("hives = %v, want all three non-placeholder hives, widget-foreign first", ids)
	}
	if resp.Totals.Hives != 3 || resp.Totals.Online != 2 || resp.Totals.Issues != 124 || resp.Totals.ActIssues != 12 || resp.Totals.NeedYou != 3 {
		t.Errorf("totals = %+v", resp.Totals)
	}
}

func TestHubWidget_StrangerSeesNothing(t *testing.T) {
	s := hubWidgetTestServer(t)
	defer helperSetupAuthUser(t, "ghp_widget_stranger", "widget-stranger")()

	code, resp := getHubWidget(t, s, "ghp_widget_stranger")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(resp.Hives) != 0 || resp.Totals != (hubWidgetTotals{}) || len(resp.History) != 0 {
		t.Fatalf("stranger got %+v", resp)
	}
}

func TestHubWidgetFleetHistoryCarriesValuesForward(t *testing.T) {
	got := hubWidgetFleetHistory([]RegistryEntry{
		{IssueHistory: []SparkPoint{{T: 900, V: 3}, {T: 1800, V: 1}}, PRHistory: []SparkPoint{{T: 900, V: 0}, {T: 1800, V: 2}}},
		{IssueHistory: []SparkPoint{{T: 1810, V: 4}}, PRHistory: []SparkPoint{{T: 1810, V: 3}}},
	})
	want := []hubWidgetHistoryPoint{
		{T: 900, ActIssues: 3, ActPRs: 0},
		{T: 1800, ActIssues: 5, ActPRs: 5},
	}
	if len(got) != len(want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("history[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
