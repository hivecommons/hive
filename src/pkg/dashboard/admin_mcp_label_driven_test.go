package dashboard

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// governor.label_driven wraps PUT /api/config/governor/repos, which calls
// requireOwnerRole as Settings → Repos does: a read-write member's confirm
// is refused and changes nothing. The verified owner's writes round-trip
// through the same config the Repos tab saves, and settings_read governor
// reads them back (projectbluefin: mergeraptor[bot] added to the trusted
// bots by the MCP instead of by hand).
func TestAdminMCPLabelDrivenOpIsOwnerOnlyAndRoundTrips(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	srv := newFullServer(t)
	srv.authToken = "secret"
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	member := adminMCPAs(t, h, map[string]string{"X-Hive-User": "alice", "X-Hive-Role": config.RoleReadWrite, proxyAuthHeader: "secret"})
	owner := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})
	preview := func(args string) string {
		return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"governor.label_driven","args":` + args + `}}}`
	}
	confirm := func(id string) string {
		return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"` + id + `"}}}`
	}
	write := func(args string) {
		t.Helper()
		if got := owner(confirm(confirmationID(t, owner(preview(args))))); !strings.Contains(got, `"write_result"`) {
			t.Fatalf("the owner's confirm of %s = %s", args, got)
		}
	}
	readLabelDriven := func() (map[string]any, map[string]any) {
		t.Helper()
		text := owner(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"settings_read","arguments":{"section":"governor"}}}`)
		var out map[string]any
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("settings_read governor = %s", text)
		}
		if data, ok := out["data"].(map[string]any); ok {
			out = data
		}
		approval, _ := out["labelDrivenApproval"].(map[string]any)
		repos, _ := out["repoLabelDriven"].(map[string]any)
		if approval == nil || repos == nil {
			t.Fatalf("settings_read governor has no label-driven settings: %s", text)
		}
		return approval, repos
	}
	addBot := `{"trusted_bots":{"add":["mergeraptor[bot]"]}}`

	if got := member(confirm(confirmationID(t, member(preview(addBot))))); !strings.Contains(got, "owner access required") {
		t.Fatalf("a read-write member's confirm = %s, want the endpoint's owner refusal", got)
	}
	if srv.deps.Config.Project.LabelDrivenTrustedBots != nil {
		t.Fatalf("a refused write changed label_driven_trusted_bots to %v", srv.deps.Config.Project.LabelDrivenTrustedBots)
	}

	write(addBot)
	if got := srv.deps.Config.LabelDrivenTrustedBots(); !slices.Equal(got, []string{"kubestellar-prow[bot]", "mergeraptor[bot]"}) {
		t.Fatalf("label_driven_trusted_bots = %v", got)
	}
	approval, _ := readLabelDriven()
	if bots, _ := approval["trustedBots"].([]any); len(bots) != 2 || bots[1] != "mergeraptor[bot]" || approval["trustedBotsSet"] != true {
		t.Fatalf("settings_read labelDrivenApproval = %v", approval)
	}

	write(`{"trusted_bots":{"clear":true}}`)
	if srv.deps.Config.Project.LabelDrivenTrustedBots != nil || !slices.Equal(srv.deps.Config.LabelDrivenTrustedBots(), config.DefaultLabelDrivenTrustedBots) {
		t.Fatalf("clear left label_driven_trusted_bots = %v", srv.deps.Config.Project.LabelDrivenTrustedBots)
	}

	write(`{"repos":{"testrepo":{"enabled":true,"approval_labels":{"add":["lgtm"]}}}}`)
	if !srv.deps.Config.RepoLabelDriven("testrepo") || !slices.Equal(srv.deps.Config.LabelDrivenApprovalLabels("testrepo"), []string{"lgtm"}) {
		t.Fatalf("testrepo label-driven = %v, labels %v", srv.deps.Config.RepoLabelDriven("testrepo"), srv.deps.Config.LabelDrivenApprovalLabels("testrepo"))
	}
	if _, repos := readLabelDriven(); repos["testorg/testrepo"].(map[string]any)["enabled"] != true {
		t.Fatalf("settings_read repoLabelDriven = %v", repos)
	}

	write(`{"repos":{"testrepo":null}}`)
	if srv.deps.Config.RepoLabelDriven("testrepo") {
		t.Fatal("null left testrepo label-driven")
	}
}
