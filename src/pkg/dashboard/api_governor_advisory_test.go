package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

type advisoryAPIResponse struct {
	MaxFindings     int  `json:"max_findings"`
	ShowAll         bool `json:"show_all"`
	StalenessDays   int  `json:"staleness_days"`
	PRAutoClose     bool `json:"pr_autoclose"`
	UpdateIntervalS int  `json:"update_interval_s"`
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
		{"negative update interval", map[string]any{"update_interval_s": -60}},
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

// TestGovAdvisory_UpdateInterval pins the #4820 wiring end to end: the field
// round-trips through PUT and GET, 0 means default (and is what an untouched
// server reports), a sub-floor write is normalized up to the 30s floor so the
// GET reports the cadence the hive actually runs, and an absent key leaves
// the setting alone.
func TestGovAdvisory_UpdateInterval(t *testing.T) {
	s := govServer(t)

	if got := getAdvisorySettings(t, s); got.UpdateIntervalS != 0 {
		t.Fatalf("untouched update_interval_s = %d, want 0 (default cadence)", got.UpdateIntervalS)
	}

	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"update_interval_s": 300}); rec.Code != http.StatusOK {
		t.Fatalf("put update_interval_s: %d — %s", rec.Code, rec.Body.String())
	}
	if got := getAdvisorySettings(t, s); got.UpdateIntervalS != 300 {
		t.Fatalf("update_interval_s = %d, want 300", got.UpdateIntervalS)
	}
	if s.deps.Config.Governor.Advisory.UpdateIntervalS != 300 {
		t.Fatal("PUT did not persist update_interval_s into config")
	}

	// A sub-floor value is clamped up at write time, never stored as typed.
	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"update_interval_s": 10}); rec.Code != http.StatusOK {
		t.Fatalf("put sub-floor interval: %d", rec.Code)
	}
	if got := getAdvisorySettings(t, s); got.UpdateIntervalS != config.MinAdvisoryUpdateIntervalS {
		t.Fatalf("sub-floor write stored %d, want the %d floor", got.UpdateIntervalS, config.MinAdvisoryUpdateIntervalS)
	}

	// Absent key = untouched; explicit 0 = back to default cadence.
	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"max_findings": 5}); rec.Code != http.StatusOK {
		t.Fatalf("unrelated put: %d", rec.Code)
	}
	if got := getAdvisorySettings(t, s); got.UpdateIntervalS != config.MinAdvisoryUpdateIntervalS {
		t.Fatalf("absent key changed update_interval_s to %d", got.UpdateIntervalS)
	}
	if rec := doPut(s, "/api/config/governor/advisory", map[string]any{"update_interval_s": 0}); rec.Code != http.StatusOK {
		t.Fatalf("put zero interval: %d", rec.Code)
	}
	if got := getAdvisorySettings(t, s); got.UpdateIntervalS != 0 {
		t.Fatalf("explicit 0 stored as %d, want 0 (default cadence, gate always open)", got.UpdateIntervalS)
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
