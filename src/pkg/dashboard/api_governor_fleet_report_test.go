package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/fleetreport"
)

// Tests for the self-report settings endpoint (GET/PUT
// /api/config/governor/fleet-report, hivecommons/hive#11868).

type fleetReportSectionBody struct {
	Enabled      bool                   `json:"enabled"`
	Explicit     bool                   `json:"explicit"`
	HubManaged   bool                   `json:"hub_managed"`
	Repo         string                 `json:"repo"`
	MaxPerDay    int                    `json:"max_per_day"`
	IncludeRepos bool                   `json:"include_repos"`
	HiveSlug     string                 `json:"hive_slug"`
	OpenReports  []FleetReportOpenEntry `json:"open_reports"`
}

func decodeFleetReportSection(t *testing.T, rec *httptest.ResponseRecorder) fleetReportSectionBody {
	t.Helper()
	var body fleetReportSectionBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode fleet-report response: %v: %s", err, rec.Body.String())
	}
	return body
}

func TestGovernorFleetReportGet_SelfHostedDefaultsToDryRun(t *testing.T) {
	SetFleetReportStatePathForTest(t, filepath.Join(t.TempDir(), "fleet-report-state.json"))
	s := covApiServer(t)
	rec := doOwnerGet(s, "/api/config/governor/fleet-report")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET fleet-report: expected 200, got %d", rec.Code)
	}
	body := decodeFleetReportSection(t, rec)
	if body.Enabled || body.Explicit || body.HubManaged || body.Repo != "hivecommons/hive" || body.MaxPerDay != 5 || body.HiveSlug != "" {
		t.Fatalf("unexpected defaults: %+v", body)
	}
	if rec := doGetNoRole(s, "/api/config/governor/fleet-report"); rec.Code != http.StatusForbidden {
		t.Fatalf("un-gated GET: expected 403, got %d", rec.Code)
	}
}

func TestGovernorFleetReportGet_HubManagedDefaultsOnWithSlug(t *testing.T) {
	SetFleetReportStatePathForTest(t, filepath.Join(t.TempDir(), "fleet-report-state.json"))
	t.Setenv("HIVE_HEARTBEAT_KEY", "hb-key")
	s := covApiServer(t)
	s.deps.Config.HiveID = "hosted-aslom"
	body := decodeFleetReportSection(t, doOwnerGet(s, "/api/config/governor/fleet-report"))
	if !body.Enabled || body.Explicit || !body.HubManaged || body.HiveSlug != "hosted-aslom" {
		t.Fatalf("hub-managed defaults: %+v", body)
	}
	if rec := doPut(s, "/api/config/governor/fleet-report", map[string]any{"enabled": false}); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	body = decodeFleetReportSection(t, doOwnerGet(s, "/api/config/governor/fleet-report"))
	if body.Enabled || !body.Explicit {
		t.Fatalf("explicit off not honoured on hub-managed hive: %+v", body)
	}
}

func TestGovernorFleetReportPut_ValidatesAndApplies(t *testing.T) {
	SetFleetReportStatePathForTest(t, filepath.Join(t.TempDir(), "fleet-report-state.json"))
	s := covApiServer(t)
	if rec := doPutRaw(s, "/api/config/governor/fleet-report", "{nope"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/fleet-report", map[string]any{"repo": "nope"}); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad repo: expected 400, got %d", rec.Code)
	}
	if rec := doPut(s, "/api/config/governor/fleet-report", map[string]any{"max_per_day": -1}); rec.Code != http.StatusBadRequest {
		t.Fatalf("negative cap: expected 400, got %d", rec.Code)
	}
	fr := s.deps.Config.Governor.FleetReport
	if fr.Enabled != nil || fr.Repo != "" || fr.MaxPerDay != 0 {
		t.Fatalf("rejected writes mutated config: %+v", fr)
	}
	rec := doPut(s, "/api/config/governor/fleet-report", map[string]any{"enabled": true, "repo": "acme/hive-fork", "max_per_day": 2})
	if rec.Code != http.StatusOK {
		t.Fatalf("valid put: %d %s", rec.Code, rec.Body.String())
	}
	body := decodeFleetReportSection(t, rec)
	if !body.Enabled || !body.Explicit || body.Repo != "acme/hive-fork" || body.MaxPerDay != 2 {
		t.Fatalf("put response: %+v", body)
	}
	if !s.FleetReportFilesUpstream() {
		t.Fatal("publisher gate did not follow the explicit enable")
	}
	// Partial write keeps the untouched fields.
	if rec := doPut(s, "/api/config/governor/fleet-report", map[string]any{"max_per_day": 0}); rec.Code != http.StatusOK {
		t.Fatalf("partial put: %d", rec.Code)
	}
	fr = s.deps.Config.Governor.FleetReport
	if fr.Enabled == nil || !*fr.Enabled || fr.Repo != "acme/hive-fork" || fr.MaxPerDay != 0 || fr.EffectiveMaxPerDay() != 5 {
		t.Fatalf("partial put clobbered fields: %+v", fr)
	}
}

func TestGovernorFleetReportGet_ListsOpenReportsNewestFirst(t *testing.T) {
	SetFleetReportStatePathForTest(t, filepath.Join(t.TempDir(), "fleet-report-state.json"))
	s := covApiServer(t)
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	s.UpdateFleetReportState(func(st *fleetreport.State) {
		st.Open["fp-old"] = fleetreport.OpenIssue{Number: 10, URL: "https://github.com/hivecommons/hive/issues/10", Class: fleetreport.ClassRestartStorm, Trigger: fleetreport.TriggerRuntimeError, PostedAt: now.Add(-time.Hour)}
		st.Open["fp-new"] = fleetreport.OpenIssue{Number: 11, URL: "https://github.com/hivecommons/hive/issues/11", Class: fleetreport.ClassPanicRecovered, Trigger: fleetreport.TriggerRuntimeError, Joined: true, PostedAt: now}
		st.Open["fp-closed"] = fleetreport.OpenIssue{Number: 12, ClosedAt: now}
		st.Open["fp-recovered"] = fleetreport.OpenIssue{Number: 13, Recovered: true, PostedAt: now}
		st.Open["fp-unposted"] = fleetreport.OpenIssue{}
	})
	body := decodeFleetReportSection(t, doOwnerGet(s, "/api/config/governor/fleet-report"))
	if len(body.OpenReports) != 2 || body.OpenReports[0].Number != 11 || !body.OpenReports[0].Joined || body.OpenReports[1].Number != 10 {
		t.Fatalf("open_reports = %+v", body.OpenReports)
	}
}

func TestGovernorFleetReportIncludeReposToggleGatesWatchedRepos(t *testing.T) {
	SetFleetReportStatePathForTest(t, filepath.Join(t.TempDir(), "fleet-report-state.json"))
	s := covApiServer(t)
	s.deps.Config.Project.Repos = []string{"HiveCommons/hive", "acme/widgets", "hivecommons/hive"}
	body := decodeFleetReportSection(t, doOwnerGet(s, "/api/config/governor/fleet-report"))
	if !body.IncludeRepos {
		t.Fatalf("include_repos should default to true: %+v", body)
	}
	if got := s.FleetReportRepos(); len(got) != 2 || got[0] != "acme/widgets" || got[1] != "hivecommons/hive" {
		t.Fatalf("FleetReportRepos default = %v", got)
	}

	rec := doPut(s, "/api/config/governor/fleet-report", map[string]any{"include_repos": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT include_repos=false: %d %s", rec.Code, rec.Body.String())
	}
	if body := decodeFleetReportSection(t, rec); body.IncludeRepos {
		t.Fatalf("include_repos not persisted: %+v", body)
	}
	if got := s.FleetReportRepos(); got != nil {
		t.Fatalf("opted-out hive still names repos: %v", got)
	}
}

// TestGovernorFleetReportIncludeOwnersToggleGatesOwnerLogins: the owners a
// self-report names are the authorized users holding the owner role plus
// the device-flow operator; include_owners=false hides them (#12529).
func TestGovernorFleetReportIncludeOwnersToggleGatesOwnerLogins(t *testing.T) {
	SetFleetReportStatePathForTest(t, filepath.Join(t.TempDir(), "fleet-report-state.json"))
	prev := fleetReportOperatorLogin
	fleetReportOperatorLogin = func(string) string { return "Operator" }
	t.Cleanup(func() { fleetReportOperatorLogin = prev })
	s := covApiServer(t)
	s.deps.Config.Dashboard.AuthorizedUsers = []string{"github:Alice", "viewer", "bob:owner", "ibmid:5500087VJB:owner"}

	rec := doOwnerGet(s, "/api/config/governor/fleet-report")
	var body struct {
		IncludeOwners bool     `json:"include_owners"`
		Owners        []string `json:"owners"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || !body.IncludeOwners {
		t.Fatalf("include_owners should default to true: %s (%v)", rec.Body.String(), err)
	}
	if got := s.FleetReportOwners(); len(got) != 3 || got[0] != "alice" || got[1] != "bob" || got[2] != "operator" {
		t.Fatalf("FleetReportOwners = %v (api: %v)", got, body.Owners)
	}
	if len(body.Owners) != 3 {
		t.Fatalf("api owners = %v", body.Owners)
	}

	rec = doPut(s, "/api/config/governor/fleet-report", map[string]any{"include_owners": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT include_owners=false: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.IncludeOwners || body.Owners != nil {
		t.Fatalf("include_owners not persisted: %s", rec.Body.String())
	}
	if got := s.FleetReportOwners(); got != nil {
		t.Fatalf("opted-out hive still names owners: %v", got)
	}
}
