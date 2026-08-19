package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
)

type advisoryAPIResponse struct {
	MaxFindings   int  `json:"max_findings"`
	ShowAll       bool `json:"show_all"`
	StalenessDays int  `json:"staleness_days"`
	PRAutoClose   bool `json:"pr_autoclose"`
}

func getAdvisorySettings(t *testing.T, s *Server) advisoryAPIResponse {
	t.Helper()
	rec := doOwnerGet(s, "/api/config/governor/advisory")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET advisory settings: %d — %s", rec.Code, rec.Body.String())
	}
	var resp advisoryAPIResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding advisory settings: %v", err)
	}
	return resp
}

// TestGovAdvisory_RequiresOwner pins the gate: these settings decide what the
// hive reports to repo owners, so a non-owner must not be able to read or
// shrink them.
func TestGovAdvisory_RequiresOwner(t *testing.T) {
	s := govServer(t)
	if rec := doGet(s, "/api/config/governor/advisory"); rec.Code != http.StatusForbidden {
		t.Errorf("unauthenticated GET = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// TestGovAdvisory_GetPutRoundTrip exercises the settings surface end to end:
// the GET reports the resolved values, the PUT stores only the fields it was
// sent, and pr_autoclose's tri-state is reported as the boolean the hive acts
// on.
func TestGovAdvisory_GetPutRoundTrip(t *testing.T) {
	s := govServer(t)

	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{
		"max_findings": 25, "show_all": true, "staleness_days": 3, "pr_autoclose": false,
	}); rec.Code != http.StatusOK {
		t.Fatalf("put advisory settings: %d — %s", rec.Code, rec.Body.String())
	}

	got := getAdvisorySettings(t, s)
	if got.MaxFindings != 25 || !got.ShowAll || got.StalenessDays != 3 || got.PRAutoClose {
		t.Fatalf("settings = %+v, want {25 true 3 false}", got)
	}
	if s.deps.Config.Governor.Advisory.PRAutoCloseEnabled() {
		t.Error("pr_autoclose: false must be stored as an explicit false pointer")
	}

	// Partial update: an absent key leaves its setting alone.
	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"max_findings": 5}); rec.Code != http.StatusOK {
		t.Fatalf("partial put: %d", rec.Code)
	}
	got = getAdvisorySettings(t, s)
	if got.MaxFindings != 5 {
		t.Errorf("MaxFindings = %d, want the updated 5", got.MaxFindings)
	}
	if !got.ShowAll || got.StalenessDays != 3 || got.PRAutoClose {
		t.Errorf("partial put changed untouched fields: %+v", got)
	}
}

// TestGovAdvisory_Validation refuses values that would silently discard the
// operator's intent (max_findings 0 resolves back to the default on load) or
// close findings faster than agents can re-report them.
func TestGovAdvisory_Validation(t *testing.T) {
	s := govServer(t)

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"zero max findings", map[string]any{"max_findings": 0}},
		{"negative max findings", map[string]any{"max_findings": -1}},
		{"zero staleness", map[string]any{"staleness_days": 0}},
		{"negative staleness", map[string]any{"staleness_days": -7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := doPut(s, "/api/config/governor/advisory", tc.body); rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d — %s", rec.Code, rec.Body.String())
			}
		})
	}

	if rec := doPutRaw(s, "/api/config/governor/advisory", "not json"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}
}

// TestGovAdvisory_InGovernorConfigGet confirms the aggregated governor config
// carries the advisory section, which is what the dashboard tab prefills from.
func TestGovAdvisory_InGovernorConfigGet(t *testing.T) {
	s := govServer(t)
	rec := doOwnerGet(s, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor config: %d", rec.Code)
	}
	var resp struct {
		Advisory *advisoryAPIResponse `json:"advisory"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding governor config: %v", err)
	}
	if resp.Advisory == nil {
		t.Fatal("governor config GET is missing the advisory section")
	}
}
