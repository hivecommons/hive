package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/advisor"
	"github.com/hivecommons/hive/pkg/config"
)

func advisorTestServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(0, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func advisorGet(t *testing.T, s *Server, role, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/advisor/records"+query, nil)
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	rec := httptest.NewRecorder()
	s.handleAdvisorRecords(rec, req)
	return rec
}

func decodeAdvisorList(t *testing.T, rec *httptest.ResponseRecorder) advisorRecordsResponse {
	t.Helper()
	var resp advisorRecordsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v (%s)", err, rec.Body.String())
	}
	return resp
}

func TestAdvisorRecordsRoleFloor(t *testing.T) {
	s := advisorTestServer(t)
	for _, role := range []string{"", config.RoleRead} {
		if rec := advisorGet(t, s, role, ""); rec.Code != http.StatusForbidden {
			t.Errorf("role %q: status = %d, want 403", role, rec.Code)
		}
	}
	for _, role := range []string{config.RoleReadWrite, config.RoleOwner} {
		if rec := advisorGet(t, s, role, ""); rec.Code != http.StatusOK {
			t.Errorf("role %q: status = %d, want 200", role, rec.Code)
		}
	}
}

func TestAdvisorRecordsEmptyWithoutStore(t *testing.T) {
	s := advisorTestServer(t)
	resp := decodeAdvisorList(t, advisorGet(t, s, config.RoleReadWrite, ""))
	if resp.Records == nil || len(resp.Records) != 0 {
		t.Errorf("no store must answer an empty listing, got %+v", resp.Records)
	}
	if resp.Active != nil {
		t.Errorf("active must be absent without an agent filter")
	}
}

func TestAdvisorRecordsListingAndFilters(t *testing.T) {
	s := advisorTestServer(t)
	store := advisor.NewStore("")
	store.Append(advisor.Record{Agent: "scout", Severity: advisor.SeverityConcern, Text: "careful"})
	store.Append(advisor.Record{Agent: "other", Severity: advisor.SeverityAside})
	store.Append(advisor.Record{Agent: "scout", Skipped: advisor.SkipTimeout})
	s.SetAdvisorRecords(store)

	resp := decodeAdvisorList(t, advisorGet(t, s, config.RoleReadWrite, ""))
	if len(resp.Records) != 3 {
		t.Fatalf("want 3 records, got %d", len(resp.Records))
	}

	resp = decodeAdvisorList(t, advisorGet(t, s, config.RoleReadWrite, "?agent=scout"))
	if len(resp.Records) != 2 {
		t.Fatalf("agent filter: want 2, got %d", len(resp.Records))
	}
	// Newest first: the skipped review tops the listing.
	if resp.Records[0].Skipped != advisor.SkipTimeout {
		t.Errorf("ordering: %+v", resp.Records[0])
	}

	resp = decodeAdvisorList(t, advisorGet(t, s, config.RoleReadWrite, "?limit=1"))
	if len(resp.Records) != 1 {
		t.Errorf("limit: want 1, got %d", len(resp.Records))
	}
}

func TestAdvisorRecordsBadParams(t *testing.T) {
	s := advisorTestServer(t)
	if rec := advisorGet(t, s, config.RoleReadWrite, "?since=yesterday"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad since: status = %d, want 400", rec.Code)
	}
	if rec := advisorGet(t, s, config.RoleReadWrite, "?limit=0"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad limit: status = %d, want 400", rec.Code)
	}
	if rec := advisorGet(t, s, config.RoleReadWrite, "?limit=nope"); rec.Code != http.StatusBadRequest {
		t.Errorf("non-numeric limit: status = %d, want 400", rec.Code)
	}
}

func TestAdvisorRecordsActiveStatus(t *testing.T) {
	s := advisorTestServer(t)
	s.SetAdvisorStatusResolver(func(agent string) (bool, string) {
		if agent == "scout" {
			return true, ""
		}
		return false, "advisor is not enabled"
	})

	resp := decodeAdvisorList(t, advisorGet(t, s, config.RoleReadWrite, "?agent=scout"))
	if resp.Active == nil || !*resp.Active {
		t.Errorf("scout must report active: %+v", resp)
	}
	resp = decodeAdvisorList(t, advisorGet(t, s, config.RoleReadWrite, "?agent=other"))
	if resp.Active == nil || *resp.Active || resp.ActiveReason == "" {
		t.Errorf("other must report inactive with a reason: %+v", resp)
	}
	// No agent filter: status fields stay absent.
	resp = decodeAdvisorList(t, advisorGet(t, s, config.RoleReadWrite, ""))
	if resp.Active != nil {
		t.Errorf("fleet listing must not carry a per-agent status")
	}
}

func TestAdvisorRecordsTimeWindow(t *testing.T) {
	s := advisorTestServer(t)
	store := advisor.NewStore("")
	store.Append(advisor.Record{Agent: "scout", Timestamp: "2026-01-01T00:00:00Z", Text: "jan"})
	store.Append(advisor.Record{Agent: "scout", Timestamp: "2026-03-01T00:00:00Z", Text: "mar"})
	store.Append(advisor.Record{Agent: "other", Timestamp: "2026-03-02T00:00:00Z", Text: "mar-other"})
	store.Append(advisor.Record{Agent: "scout", Timestamp: "2026-06-01T00:00:00Z", Text: "jun"})
	s.SetAdvisorRecords(store)

	resp := decodeAdvisorList(t, advisorGet(t, s, config.RoleReadWrite,
		"?agent=scout&since=2026-02-01T00:00:00Z&until=2026-04-01T00:00:00Z"))
	if len(resp.Records) != 1 || resp.Records[0].Text != "mar" {
		t.Fatalf("agent+window must return only that agent's records in the window: %+v", resp.Records)
	}
	resp = decodeAdvisorList(t, advisorGet(t, s, config.RoleReadWrite, "?until=2026-04-01T00:00:00Z"))
	if len(resp.Records) != 3 {
		t.Fatalf("until-only window: want 3, got %+v", resp.Records)
	}
}

func TestAdvisorRecordsWindowParams(t *testing.T) {
	s := advisorTestServer(t)
	for _, q := range []string{
		"?until=tomorrow",
		"?hours=0",
		"?hours=721",
		"?hours=x",
		"?hours=2&since=2026-01-01T00:00:00Z",
		"?since=2026-02-01T00:00:00Z&until=2026-01-01T00:00:00Z",
	} {
		if rec := advisorGet(t, s, config.RoleReadWrite, q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
	}
	store := advisor.NewStore("")
	store.Append(advisor.Record{Agent: "scout", Text: "now"})
	store.Append(advisor.Record{Agent: "scout", Timestamp: "2020-01-01T00:00:00Z", Text: "old"})
	s.SetAdvisorRecords(store)
	resp := decodeAdvisorList(t, advisorGet(t, s, config.RoleReadWrite, "?hours=24"))
	if len(resp.Records) != 1 || resp.Records[0].Text != "now" {
		t.Errorf("hours lookback must keep only recent records: %+v", resp.Records)
	}
}

func advisorSpendGet(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/advisor/spend"+query, nil)
	rec := httptest.NewRecorder()
	s.handleAdvisorSpend(rec, req)
	return rec
}

func TestAdvisorSpendBesideAgentSpend(t *testing.T) {
	s := advisorTestServer(t)
	store := advisor.NewStore("")
	store.Append(advisor.Record{Agent: "scout", InputTokens: 100, OutputTokens: 20, CostUSD: 0.5})
	store.Append(advisor.Record{Agent: "scout", InputTokens: 50, OutputTokens: 10, CostUSD: 0.25})
	store.Append(advisor.Record{Agent: "scout", Skipped: advisor.SkipBudgetExhausted})
	store.Append(advisor.Record{Agent: "other", CostUSD: 1})
	store.Append(advisor.Record{Agent: "scout", Timestamp: "2020-01-01T00:00:00Z", CostUSD: 100})
	s.SetAdvisorRecords(store)

	rec := advisorSpendGet(t, s, "?range=daily&agent=scout")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp advisorSpendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Range != "daily" || resp.Since == "" || resp.Until == "" {
		t.Errorf("preset range must report its bounds: %+v", resp)
	}
	if len(resp.Agents) != 1 || resp.Agents[0].Agent != "scout" {
		t.Fatalf("agent filter: %+v", resp.Agents)
	}
	scout := resp.Agents[0]
	if scout.Reviews != 3 || scout.Skipped != 1 || scout.InputTokens != 150 || scout.OutputTokens != 30 {
		t.Errorf("aggregate: %+v", scout)
	}
	// Equals the sum of the cost figures on that agent's records for the
	// same period, as the REST listing returns them.
	var sum float64
	for _, r := range store.ListWindow("scout", time.Now().Add(-30*24*time.Hour), time.Time{}, 0) {
		sum += r.CostUSD
	}
	if scout.CostUSD != sum || resp.TotalCostUSD != sum || sum != 0.75 {
		t.Errorf("spend %v / total %v must equal the listing sum %v", scout.CostUSD, resp.TotalCostUSD, sum)
	}

	rec = advisorSpendGet(t, s, "")
	var all advisorSpendResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if all.Range != "all" || len(all.Agents) != 2 || all.TotalCostUSD != 101.75 {
		t.Errorf("unbounded spend must cover every retained record: %+v", all)
	}
}

func TestAdvisorSpendParams(t *testing.T) {
	s := advisorTestServer(t)
	for _, q := range []string{"?range=yearly", "?range=daily&since=2026-01-01T00:00:00Z", "?since=nope"} {
		if rec := advisorSpendGet(t, s, q); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", q, rec.Code)
		}
	}
	for key := range advisorSpendRanges {
		if rec := advisorSpendGet(t, s, "?range="+key); rec.Code != http.StatusOK {
			t.Errorf("range %s: status = %d", key, rec.Code)
		}
	}
	var resp advisorSpendResponse
	if err := json.Unmarshal(advisorSpendGet(t, s, "?since=2026-01-01T00:00:00Z").Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Range != "custom" || resp.Agents == nil {
		t.Errorf("custom window without a store: %+v", resp)
	}
}

// TestAdvisorSpendRangesMatchCostSelector pins the preset windows to the Cost
// section's COST_RANGES so the two never report different "daily" spans.
func TestAdvisorSpendRangesMatchCostSelector(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"hourly:  { label: 'hourly',  bucketMs: 3600e3,        windowMs: 24 * 3600e3,",
		"daily:   { label: 'daily',   bucketMs: 24 * 3600e3,   windowMs: 30 * 24 * 3600e3,",
		"weekly:  { label: 'weekly',  bucketMs: 7 * 24 * 3600e3, windowMs: 12 * 7 * 24 * 3600e3,",
		"monthly: { label: 'monthly', bucketMs: 30 * 24 * 3600e3, windowMs: 12 * 30 * 24 * 3600e3,",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("COST_RANGES changed; update advisorSpendRanges to match: missing %q", want)
		}
	}
	if advisorSpendRanges["hourly"] != 24*time.Hour || advisorSpendRanges["daily"] != 30*24*time.Hour ||
		advisorSpendRanges["weekly"] != 12*7*24*time.Hour || advisorSpendRanges["monthly"] != 12*30*24*time.Hour {
		t.Errorf("advisorSpendRanges drifted from COST_RANGES: %v", advisorSpendRanges)
	}
}

func TestCostCarriesAdvisorByAgent(t *testing.T) {
	s := advisorTestServer(t)
	rec := httptest.NewRecorder()
	s.handleCost(rec, httptest.NewRequest(http.MethodGet, "/api/cost", nil))
	var empty costResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatal(err)
	}
	if empty.AdvisorByAgent == nil || len(empty.AdvisorByAgent) != 0 {
		t.Errorf("advisor_by_agent must be an empty array without a store: %#v", empty.AdvisorByAgent)
	}

	store := advisor.NewStore("")
	store.Append(advisor.Record{Agent: "scout", CostUSD: 0.4})
	s.SetAdvisorRecords(store)
	rec = httptest.NewRecorder()
	s.handleCost(rec, httptest.NewRequest(http.MethodGet, "/api/cost", nil))
	var resp costResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.AdvisorByAgent) != 1 || resp.AdvisorByAgent[0].Agent != "scout" || resp.AdvisorByAgent[0].CostUSD != 0.4 {
		t.Errorf("advisor_by_agent: %+v", resp.AdvisorByAgent)
	}
}
