package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

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
