package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// governor.stage_labels writes governor.labels.stage.abandon_after and
// abandon_close_repos through the Labels endpoint, owner only, like the
// dashboard control.
func TestAdminMCPStageLabelsOpWritesBothKeysOwnerOnly(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	srv := newFullServer(t)
	srv.authToken = "secret"
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	member := adminMCPAs(t, h, map[string]string{"X-Hive-User": "alice", "X-Hive-Role": config.RoleReadWrite, proxyAuthHeader: "secret"})
	owner := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})
	preview := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"governor.stage_labels","args":{"abandon_after":"168h","abandon_close_repos":{"add":["testrepo"]}}}}}`
	confirm := func(id string) string {
		return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"` + id + `"}}}`
	}
	stage := func() config.StageLabelsConfig { return srv.deps.Config.Governor.Labels.Stage }

	if got := member(confirm(confirmationID(t, member(preview)))); !strings.Contains(got, "owner access required") {
		t.Fatalf("a read-write member's confirm = %s, want the endpoint's owner refusal", got)
	}
	if st := stage(); st.AbandonAfter != "" || len(st.AbandonCloseRepos) != 0 {
		t.Fatalf("a refused write changed the settings: %+v", st)
	}
	if got := owner(confirm(confirmationID(t, owner(preview)))); !strings.Contains(got, `"write_result"`) {
		t.Fatalf("the owner's confirm = %s", got)
	}
	st := stage()
	if st.AbandonAfter != "168h" || len(st.AbandonCloseRepos) != 1 || st.AbandonCloseRepos[0] != "testorg/testrepo" {
		t.Fatalf("after the owner's write: abandon_after=%q abandon_close_repos=%v", st.AbandonAfter, st.AbandonCloseRepos)
	}
}
