package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/writeback"
)

// agent.cadences and writeback.kind_policy go through the endpoints the
// dashboard saves with, which are owner only: a read-write member's confirm
// is refused and changes nothing. The owner's confirm changes exactly the
// named mode and kind: the other modes keep their cadence and ownership, and
// the other kind's override survives, although the Features endpoint
// replaces the whole writebackKinds map it is sent.
func TestAdminMCPCadenceAndWritebackKindOpsAreOwnerOnlyAndMerge(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	srv := newFullServer(t)
	cfg := srv.deps.Config
	cfg.Governor.Modes = map[string]config.ModeConfig{
		"idle":  {Cadences: map[string]config.Cadence{"scanner": "4h"}},
		"quiet": {Threshold: 2, Cadences: map[string]config.Cadence{"scanner": "4h"}},
		"busy":  {Threshold: 10, Cadences: map[string]config.Cadence{"scanner": "4h"}},
		"surge": {Threshold: 20, Cadences: map[string]config.Cadence{"scanner": "4h"}},
	}
	off := false
	cfg.Writeback.Kinds = map[string]writeback.KindPolicyOverride{"stage_label": {RequireLabel: &off}}

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
		{"agent.cadences", `{"agent":"scanner","modes":{"surge":"1h"}}`},
		{"writeback.kind_policy", `{"kinds":{"base_refresh":{"unlabeled_min_level":6}}}`},
	}

	for _, tc := range cases {
		if got := member(confirm(confirmationID(t, member(preview(tc.op, tc.args))))); !strings.Contains(got, "owner access required") {
			t.Fatalf("a read-write member's %s confirm = %s, want the endpoint's owner refusal", tc.op, got)
		}
	}
	if got := cfg.Governor.Modes["surge"].Cadences["scanner"]; got != "4h" || cfg.Governor.CadenceIsOperatorOwned("surge", "scanner") {
		t.Fatalf("a refused write changed the surge cadence to %q (operator-owned %v)", got, cfg.Governor.CadenceIsOperatorOwned("surge", "scanner"))
	}
	if _, ok := cfg.Writeback.Kinds["base_refresh"]; ok || len(cfg.Writeback.Kinds) != 1 {
		t.Fatalf("a refused write changed writeback.kinds to %+v", cfg.Writeback.Kinds)
	}

	for _, tc := range cases {
		if got := owner(confirm(confirmationID(t, owner(preview(tc.op, tc.args))))); !strings.Contains(got, `"write_result"`) {
			t.Fatalf("the owner's %s confirm = %s", tc.op, got)
		}
	}
	if got := cfg.Governor.Modes["surge"].Cadences["scanner"]; got != "1h" {
		t.Fatalf("surge cadence = %q, want 1h", got)
	}
	if !cfg.Governor.CadenceIsOperatorOwned("surge", "scanner") {
		t.Fatal("the surge cadence the owner set is not operator-owned, so a level change would reset it")
	}
	for _, mode := range []string{"busy", "quiet", "idle"} {
		if got := cfg.Governor.Modes[mode].Cadences["scanner"]; got != "4h" || cfg.Governor.CadenceIsOperatorOwned(mode, "scanner") {
			t.Fatalf("%s cadence = %q (operator-owned %v), want the untouched 4h", mode, got, cfg.Governor.CadenceIsOperatorOwned(mode, "scanner"))
		}
	}
	stage, ok := cfg.Writeback.Kinds["stage_label"]
	if !ok || stage.RequireLabel == nil || *stage.RequireLabel {
		t.Fatalf("stage_label's require_label: false override did not survive the base_refresh change: %+v", cfg.Writeback.Kinds)
	}
	refresh, ok := cfg.Writeback.Kinds["base_refresh"]
	if !ok || refresh.UnlabeledMinLevel == nil || *refresh.UnlabeledMinLevel != 6 || refresh.MinLevel != nil || refresh.RequireLabel != nil {
		t.Fatalf("base_refresh override = %+v, want only unlabeled_min_level: 6", refresh)
	}
	if got := cfg.Writeback.Policy(writeback.KindBaseRefresh); got.UnlabeledMinLevel != 6 {
		t.Fatalf("effective base_refresh policy = %+v", got)
	}
}
