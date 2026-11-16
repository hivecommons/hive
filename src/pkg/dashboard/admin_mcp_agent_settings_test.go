package dashboard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

// agentSettingsNotOffered are keys handleAgentConfigGeneral reads that
// agent.settings neither offers nor refuses with a pointer: it rejects them
// as unknown fields. A key the handler starts reading must be added to the
// op's fields, its refused list, or here.
var agentSettingsNotOffered = []string{"cavemanMode", "jevMode", "explainMode"}

// handleAgentConfigGeneralBodyKeys returns every string key the General
// handler reads from its body (body["..."]), from its source, plus the
// GitHub API cap key, which it reads through agentGitHubCapFromBody.
func handleAgentConfigGeneralBodyKeys(t *testing.T) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "api_agent_config.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing api_agent_config.go: %v", err)
	}
	seen := map[string]bool{agentGitHubCapField: true}
	found := false
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "handleAgentConfigGeneral" {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			idx, ok := n.(*ast.IndexExpr)
			if !ok {
				return true
			}
			if id, ok := idx.X.(*ast.Ident); !ok || id.Name != "body" {
				return true
			}
			if lit, ok := idx.Index.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if key, err := strconv.Unquote(lit.Value); err == nil {
					seen[key] = true
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("handleAgentConfigGeneral not found in api_agent_config.go")
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) < 20 {
		t.Fatalf("found only %d body keys (%v); the scan no longer reads the handler", len(keys), keys)
	}
	return keys
}

// Every field agent.settings offers or refuses is a key the General handler
// reads, so the op never offers a field the endpoint would silently ignore;
// and every key the handler reads is offered, refused or listed in
// agentSettingsNotOffered, so a new handler field forces a decision here.
func TestAdminMCPAgentSettingsFieldsMatchHandler(t *testing.T) {
	handler := handleAgentConfigGeneralBodyKeys(t)
	offered := adminmcp.AgentSettingsFields()
	refused := adminmcp.AgentSettingsRefusedFields()
	for _, k := range offered {
		if !slices.Contains(handler, k) {
			t.Errorf("agent.settings offers %q, which handleAgentConfigGeneral does not read: keep agentSettingsFields (pkg/adminmcp/tools_agent_settings_ops.go) in step", k)
		}
		if slices.Contains(refused, k) {
			t.Errorf("%q is both offered and refused", k)
		}
	}
	for _, k := range refused {
		if !slices.Contains(handler, k) {
			t.Errorf("agent.settings refuses %q, which handleAgentConfigGeneral does not read", k)
		}
	}
	for _, k := range handler {
		if !slices.Contains(offered, k) && !slices.Contains(refused, k) && !slices.Contains(agentSettingsNotOffered, k) {
			t.Errorf("handleAgentConfigGeneral reads %q, which agent.settings neither offers nor refuses: add it to agentSettingsFields or agentSettingsRefused (pkg/adminmcp), or to agentSettingsNotOffered here", k)
		}
	}
}

// agent.settings checks restartStrategy and beadRole at preview against
// copies of the endpoint's lists; they must stay equal. The op also takes ""
// for beadRole, which the endpoint skips.
func TestAdminMCPAgentSettingsEnumsMatchEndpoint(t *testing.T) {
	enums := adminmcp.AgentSettingsEnums()
	for name, endpoint := range map[string]map[string]bool{"restartStrategy": validRestartStrategies, "beadRole": validBeadRoles} {
		want := make([]string, 0, len(endpoint)+1)
		for k := range endpoint {
			want = append(want, k)
		}
		if name == "beadRole" {
			want = append(want, "")
		}
		got := slices.Clone(enums[name])
		sort.Strings(want)
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("agent.settings takes %s %q, the endpoint %q", name, got, want)
		}
	}
}

// agent.settings goes through the endpoint the agent's General tab saves
// with, which is owner only: a read-write member's confirm is refused and
// changes nothing. The owner's preview and confirm of enabled: false turn the
// agent off, keep every field the call did not name, and are audited as a
// config_agent_general change. A refused field never reaches the endpoint.
func TestAdminMCPAgentSettingsIsOwnerOnly(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	srv := newFullServer(t)
	srv.authToken = "secret"
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	member := adminMCPAs(t, h, map[string]string{"X-Hive-User": "alice", "X-Hive-Role": config.RoleReadWrite, proxyAuthHeader: "secret"})
	owner := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})
	preview := func(args string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"agent.settings","args":` + args + `}}}`
	}
	confirm := func(id string) string {
		return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"` + id + `"}}}`
	}
	audited := func() int {
		n := 0
		for _, e := range srv.GetAudit().Recent(0) {
			if e.Action == "config_agent_general" && e.Agent == "scanner" {
				n++
			}
		}
		return n
	}
	const off = `{"agent":"scanner","enabled":false}`

	if got := member(confirm(confirmationID(t, member(preview(off))))); !strings.Contains(got, "owner access required") {
		t.Fatalf("a read-write member's confirm = %s, want the endpoint's owner refusal", got)
	}
	if !srv.deps.Config.Agents["scanner"].Enabled || audited() != 0 {
		t.Fatalf("a refused write changed the agent (enabled %v) or was audited", srv.deps.Config.Agents["scanner"].Enabled)
	}

	if got := owner(preview(`{"agent":"scanner","launchCmd":"claude --yolo"}`)); !strings.Contains(got, "change it on the dashboard") {
		t.Fatalf("a refused field's preview = %s, want it refused with the dashboard pointer", got)
	}

	got := owner(confirm(confirmationID(t, owner(preview(off)))))
	if !strings.Contains(got, `"write_result"`) {
		t.Fatalf("the owner's confirm = %s, want a write_result", got)
	}
	a := srv.deps.Config.Agents["scanner"]
	if a.Enabled {
		t.Fatal("the owner's confirm did not turn the agent off")
	}
	if a.DisplayName != "Scanner" || a.Model != "sonnet" || a.Backend != "claude" || a.Role != "scanner" {
		t.Fatalf("fields the call did not name changed: %+v", a)
	}
	if audited() != 1 {
		t.Fatalf("config_agent_general audit entries for scanner = %d, want 1", audited())
	}
}
