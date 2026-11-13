package dashboard

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

// The admin MCP side of the code scanning sweep settings: settings_read
// section code_scanning and the code_scanning.settings write operation, both
// through GET/PUT /api/config/code-scanning. Before them the MCP could
// neither read nor change code_scanning.

// codeScanningJSONNames lists a body struct's fields by their JSON names.
func codeScanningJSONNames(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("%s.%s has no JSON name, so the endpoint cannot be sent it by name", typ.Name(), typ.Field(i).Name)
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// code_scanning.settings refuses a field it does not know, so a field added
// to PUT /api/config/code-scanning must be added to the op too, and a field
// the op still offers after the handler drops it would be refused by the
// endpoint. Both directions fail here.
func TestAdminMCPCodeScanningSettingsFieldsMatchHandlerBody(t *testing.T) {
	top, repo := adminmcp.CodeScanningSettingsBodyFields()
	sort.Strings(top)
	sort.Strings(repo)
	if handler := codeScanningJSONNames(t, reflect.TypeOf(codeScanningBody{})); !reflect.DeepEqual(handler, top) {
		t.Fatalf("PUT /api/config/code-scanning takes %v\nbut code_scanning.settings offers %v\n(pkg/adminmcp/tools_code_scanning_ops.go)", handler, top)
	}
	if handler := codeScanningJSONNames(t, reflect.TypeOf(codeScanningRepoSetting{})); !reflect.DeepEqual(handler, repo) {
		t.Fatalf("a repos entry of PUT /api/config/code-scanning takes %v\nbut code_scanning.settings offers %v", handler, repo)
	}
}

func codeScanningMCPCall(tool, arguments string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + arguments + `}}`
}

// The settings are read through settings_read, owner only. The write
// operation changes nothing at write_preview; write_confirm then leaves the
// hive exactly as the same PUT does, audited the same way. A value hive.yaml
// loading refuses is refused at the preview with that message.
func TestAdminMCPCodeScanningSettingsReadPreviewConfirm(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	srv := newFullServer(t)
	srv.authToken = "secret"
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	member := adminMCPAs(t, h, map[string]string{"X-Hive-User": "alice", "X-Hive-Role": config.RoleReadWrite, proxyAuthHeader: "secret"})
	owner := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})

	read := codeScanningMCPCall("settings_read", `{"section":"code_scanning"}`)
	var before struct {
		Data codeScanningSettings `json:"data"`
	}
	if err := json.Unmarshal([]byte(owner(read)), &before); err != nil {
		t.Fatalf("settings_read section code_scanning: %v", err)
	}
	if before.Data.Org != "testorg" || before.Data.Enabled || before.Data.MinSeverityEffective != "high" || len(before.Data.ProjectRepos) != 1 ||
		before.Data.ProjectRepos[0].Repo != "testorg/testrepo" || before.Data.ProjectRepos[0].AlertsAccess != codeScanningAccessOff {
		t.Fatalf("settings_read section code_scanning = %+v", before.Data)
	}
	if got := member(read); !strings.Contains(got, "owner access required") {
		t.Fatalf("a read-write member's settings_read = %s, want the endpoint's owner refusal", got)
	}

	const change = `{"min_severity":"medium","repos":{"testrepo":{"enabled":true}}}`
	preview := codeScanningMCPCall("write_preview", `{"operation":"code_scanning.settings","args":`+change+`}`)
	confirm := func(id string) string {
		return codeScanningMCPCall("write_confirm", `{"confirmation_id":"`+id+`"}`)
	}

	// A caller who is not the verified owner cannot even preview: the op
	// reads the settings first, and that read is owner only.
	if got := member(preview); !strings.Contains(got, "verified owner") {
		t.Fatalf("a read-write member's preview = %s, want it refused", got)
	}

	// write_preview alone writes nothing.
	previewed := owner(preview)
	id := confirmationID(t, previewed)
	if !strings.Contains(previewed, "the code scanning sweep is turned on for testorg/testrepo") {
		t.Fatalf("the preview does not disclose where the sweep is turned on: %s", previewed)
	}
	if !reflect.DeepEqual(srv.deps.Config.CodeScanning, config.CodeScanningConfig{}) {
		t.Fatalf("write_preview changed the config: %+v", srv.deps.Config.CodeScanning)
	}
	if rows := codeScanningAuditRows(srv); len(rows) != 0 {
		t.Fatalf("write_preview was audited: %+v", rows)
	}

	// write_confirm does what the same PUT does.
	if got := owner(confirm(id)); !strings.Contains(got, `"write_result"`) {
		t.Fatalf("the owner's confirm = %s, want a write_result", got)
	}
	direct := newFullServer(t)
	if rec := doPutRaw(direct, codeScanningTestPath, change); rec.Code != http.StatusOK {
		t.Fatalf("the same PUT: %d %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(srv.deps.Config.CodeScanning, direct.deps.Config.CodeScanning) {
		t.Fatalf("after write_confirm the hive holds\n%+v\nafter the same PUT it holds\n%+v", srv.deps.Config.CodeScanning, direct.deps.Config.CodeScanning)
	}
	if _, on := srv.deps.Config.CodeScanning.For("testorg", "testrepo"); !on || srv.deps.Config.CodeScanning.MinSeverity != "medium" {
		t.Fatalf("the confirmed change is not in the live config: %+v", srv.deps.Config.CodeScanning)
	}
	if viaMCP, viaPUT := doOwnerGet(srv, codeScanningTestPath).Body.String(), doOwnerGet(direct, codeScanningTestPath).Body.String(); viaMCP != viaPUT {
		t.Fatalf("GET after write_confirm\n%s\nGET after the same PUT\n%s", viaMCP, viaPUT)
	}
	if viaMCP, viaPUT := codeScanningAuditDetails(srv), codeScanningAuditDetails(direct); len(viaMCP) != 2 || !reflect.DeepEqual(viaMCP, viaPUT) {
		t.Fatalf("audit after write_confirm %v, after the same PUT %v; want the same two entries", viaMCP, viaPUT)
	}

	// The preview refuses what hive.yaml loading refuses, in its words.
	bad := owner(codeScanningMCPCall("write_preview", `{"operation":"code_scanning.settings","args":{"interval":"5m"}}`))
	if !strings.Contains(bad, "code_scanning.interval: must be at least 10m0s, got 5m0s") {
		t.Fatalf("an interval under ten minutes previewed as %s", bad)
	}
	if srv.deps.Config.CodeScanning.Interval != 0 {
		t.Fatal("a refused preview changed the interval")
	}
}
