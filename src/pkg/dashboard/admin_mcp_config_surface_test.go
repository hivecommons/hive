package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The issue filter and Sentinel write ops are owner-only, as their Settings
// tabs are: they wrap endpoints that call requireOwnerRole, and a confirmed
// write runs as the verified caller. A read-write member's confirm is refused
// and changes nothing; the verified owner (the dashboard token) makes the
// same change.
func TestAdminMCPIssueFilterAndSentinelOpsAreOwnerOnly(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	srv := newFullServer(t)
	srv.authToken = "secret"
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	member := adminMCPAs(t, h, map[string]string{"X-Hive-User": "alice", "X-Hive-Role": config.RoleReadWrite, proxyAuthHeader: "secret"})
	owner := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})
	preview := func(op, args string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"` + op + `","args":` + args + `}}}`
	}
	confirm := func(id string) string {
		return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"` + id + `"}}}`
	}
	cases := []struct{ op, args string }{
		{"project.issue_filter", `{"reporter_trust":{"trusted_logins":{"add":["ext-maint"]}}}`},
		{"sentinel.settings", `{"exempt_logins":{"add":["renovate[bot]"]}}`},
	}

	for _, tc := range cases {
		if got := member(confirm(confirmationID(t, member(preview(tc.op, tc.args))))); !strings.Contains(got, "owner access required") {
			t.Fatalf("a read-write member's %s confirm = %s, want the endpoint's owner refusal", tc.op, got)
		}
	}
	if got := srv.deps.Config.Project.IssueFilter.ReporterTrust.TrustedLogins; len(got) != 0 {
		t.Fatalf("a refused write changed trusted_logins to %v", got)
	}
	if got := srv.deps.Config.Sentinel.ExemptLogins; len(got) != 0 {
		t.Fatalf("a refused write changed sentinel.exempt_logins to %v", got)
	}

	for _, tc := range cases {
		if got := owner(confirm(confirmationID(t, owner(preview(tc.op, tc.args))))); !strings.Contains(got, `"write_result"`) {
			t.Fatalf("the owner's %s confirm = %s", tc.op, got)
		}
	}
	if got := srv.deps.Config.Project.IssueFilter.ReporterTrust.TrustedLogins; len(got) != 1 || got[0] != "ext-maint" {
		t.Fatalf("trusted_logins = %v, want [ext-maint]", got)
	}
	if got := srv.deps.Config.Sentinel.ExemptLogins; len(got) != 1 || got[0] != "renovate[bot]" {
		t.Fatalf("sentinel.exempt_logins = %v, want [renovate[bot]]", got)
	}
}
