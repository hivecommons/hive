package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hivecommons/hive/pkg/compliance"
	"github.com/hivecommons/hive/pkg/config"
)

func complianceStatusRequest(t *testing.T, s *Server, role string, verified bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/compliance/status", nil)
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	rr := httptest.NewRecorder()
	s.handleComplianceStatus(rr, req)
	return rr
}

func complianceTestServer() *Server {
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{
		Compliance: config.ComplianceConfig{Frameworks: []string{"soc2-type2"}},
		Dashboard:  config.DashboardConfig{AuthorizedUsers: []string{"alice", "bob:merger"}},
	}}
	return s
}

func TestHandleComplianceStatusAuth(t *testing.T) {
	cases := []struct {
		name     string
		role     string
		verified bool
		want     int
	}{
		{name: "anonymous", want: http.StatusForbidden},
		{name: "read", role: config.RoleRead, want: http.StatusForbidden},
		{name: "read-write", role: config.RoleReadWrite, want: http.StatusForbidden},
		{name: "unverified owner header", role: config.RoleOwner, want: http.StatusForbidden},
		{name: "merger", role: config.RoleMerger, want: http.StatusOK},
		{name: "verified owner", role: config.RoleOwner, verified: true, want: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := complianceStatusRequest(t, complianceTestServer(), tc.role, tc.verified)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestHandleComplianceStatusNoConfig(t *testing.T) {
	rr := complianceStatusRequest(t, newTestServer(), config.RoleOwner, true)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

func TestHandleComplianceStatusBody(t *testing.T) {
	orig := complianceGetenv
	t.Cleanup(func() { complianceGetenv = orig })
	complianceGetenv = func(name string) string {
		if name == config.ProxyInjectGHAuthEnv {
			return "true"
		}
		return ""
	}
	rr := complianceStatusRequest(t, complianceTestServer(), config.RoleMerger, false)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d (body=%s)", rr.Code, rr.Body.String())
	}
	var rep compliance.Report
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	if rep.Disclaimer == "" || len(rep.Frameworks) != 1 || rep.Frameworks[0] != "soc2-type2" {
		t.Fatalf("report header = %+v", rep)
	}
	byID := map[string]compliance.ControlStatus{}
	for _, c := range rep.Controls {
		byID[c.ControlID] = c
	}
	// alice (implicit owner) + bob:merger, injection on → CC6.1 meets.
	if got := byID["CC6.1"].Status; got != compliance.StatusMeets {
		t.Fatalf("CC6.1 = %s, want meets (controls=%+v)", got, byID["CC6.1"])
	}
	if got := byID["CC6.4"].Status; got != compliance.StatusNotCovered {
		t.Fatalf("CC6.4 = %s, want not_covered", got)
	}
	if rep.Summary != compliance.Summarize(rep.Controls) {
		t.Fatalf("summary %+v does not match controls", rep.Summary)
	}
}

func TestHandleComplianceStatusNoFrameworks(t *testing.T) {
	s := newTestServer()
	s.deps = &Dependencies{Config: &config.Config{}}
	rr := complianceStatusRequest(t, s, config.RoleOwner, true)
	var rep compliance.Report
	if err := json.Unmarshal(rr.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Controls) != 0 || len(rep.Frameworks) != 0 || len(rep.Available) == 0 {
		t.Fatalf("unselected report = %+v", rep)
	}
}

// TestComplianceAuditRetentionMatchesAuditLog pins the compliance registry's
// reported audit retention to the audit log's real lumberjack MaxAge, so the
// CC7.2 mapping can never report a value the hive does not actually keep.
func TestComplianceAuditRetentionMatchesAuditLog(t *testing.T) {
	if compliance.BuiltinAuditRetentionDays != auditMaxAgeDays {
		t.Fatalf("compliance.BuiltinAuditRetentionDays = %d, audit log MaxAge = %d", compliance.BuiltinAuditRetentionDays, auditMaxAgeDays)
	}
}
