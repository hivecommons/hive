package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// An approve sent through the admin MCP is held to the caller's own rights,
// exactly as the dashboard's Approve button is: the write runs as the
// verified caller and the decision route checks that caller's permission on
// the repository. A caller with read access is refused and nothing is
// written; a caller with write access approves, and the approval names them.
func TestAdminMCPApproveIsHeldToTheCallersRepoPermission(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	mock := &repoItemDecisionMock{}
	srv := newRepoItemDecisionServer(t, mock)
	srv.authToken = "secret"
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	hub := func(user string) mcpCaller {
		return adminMCPAs(t, h, map[string]string{"X-Hive-User": user, "X-Hive-Role": config.RoleReadWrite, proxyAuthHeader: "secret"})
	}
	const preview = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"repository.item_decision","args":{"owner":"testorg","repo":"testrepo","number":8,"type":"pr","decision":"approve"}}}}`
	confirm := func(id string) string {
		return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"` + id + `"}}}`
	}

	reader := hub("reader")
	got := reader(confirm(confirmationID(t, reader(preview))))
	if !strings.Contains(got, "write permission required") {
		t.Fatalf("a read-only caller's approve = %s, want the decision route's permission refusal", got)
	}
	if len(mock.comments) != 0 || len(mock.reviews) != 0 {
		t.Fatalf("a refused approve wrote to GitHub: comments=%q reviews=%v", mock.comments, mock.reviews)
	}

	writer := hub("writer")
	got = writer(confirm(confirmationID(t, writer(preview))))
	if !strings.Contains(got, `"write_result"`) || strings.Contains(got, "permission required") {
		t.Fatalf("a writer's approve = %s", got)
	}
	if len(mock.comments) != 2 || !strings.HasPrefix(mock.comments[0], "/lgtm\n") || !strings.Contains(mock.comments[0], "@writer") {
		t.Fatalf("the approval must be recorded as @writer: %q", mock.comments)
	}
}
