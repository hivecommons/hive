package dashboard

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

// advisor.settings and advisor.agent refuse a field they do not know, so a
// field added to the fleet body or to an agent override must be added to the
// ops too, or the MCP could not set it; and a field the ops still offer after
// the handler drops it would be silently ignored. Both directions fail here,
// for both bodies.
func TestAdminMCPAdvisorFieldsMatchHandlerBody(t *testing.T) {
	op := adminmcp.AdvisorBodyFields()
	sort.Strings(op)
	for name, typ := range map[string]reflect.Type{
		"PUT /api/config/advisor":              reflect.TypeOf(advisorFleetBody{}),
		"PUT /api/config/advisor/agent/{name}": reflect.TypeOf(config.AdvisorOverride{}),
	} {
		handler := reviewConfigPutBodyFields(t, typ, "")
		sort.Strings(handler)
		if strings.Join(handler, ",") != strings.Join(op, ",") {
			t.Errorf("%s takes %v, but advisor.settings and advisor.agent offer %v: keep advisorFields (pkg/adminmcp/tools_advisor_ops.go) in step", name, handler, op)
		}
	}
}

// advisor.settings is owner only, as the Advisor Lane panel is: the settings
// read and the endpoint both call requireOwnerRole, and a confirmed write
// runs as the verified caller. A read-write member is refused and nothing
// changes. The owner's confirm sends only the named field: the budget set
// beforehand is kept.
func TestAdminMCPAdvisorSettingsIsOwnerOnly(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	srv := newFullServer(t)
	srv.authToken = "secret"
	srv.deps.Config.Advisor.DailyBudgetTokens = 7000
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	member := adminMCPAs(t, h, map[string]string{"X-Hive-User": "alice", "X-Hive-Role": config.RoleReadWrite, proxyAuthHeader: "secret"})
	owner := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})
	const preview = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"advisor.settings","args":{"enabled":true}}}}`

	if got := member(preview); !strings.Contains(got, "advisor settings could not be read") {
		t.Fatalf("a read-write member's preview = %s, want the owner-only read refused", got)
	}
	if srv.deps.Config.Advisor.IsEnabled() {
		t.Fatal("a refused preview turned the advisor on")
	}

	id := confirmationID(t, owner(preview))
	got := owner(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"` + id + `"}}}`)
	if !strings.Contains(got, `"write_result"`) {
		t.Fatalf("the owner's confirm = %s, want a write_result", got)
	}
	if a := srv.deps.Config.Advisor; !a.IsEnabled() || a.DailyBudgetTokens != 7000 {
		t.Fatalf("advisor = %+v, want enabled with the 7000 budget kept", a)
	}
}
