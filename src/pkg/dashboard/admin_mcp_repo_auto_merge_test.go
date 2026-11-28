package dashboard

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

// The admin MCP side of the repository card's auto-merge switch
// (project.repo_policies[].auto_merge): settings_read section auto_merge
// reports it under repo_auto_merge, and repository.auto_merge changes it
// through POST /api/repos/auto-merge. Before them only the card could set it.

// repository.auto_merge sends exactly the body the card sends, so a field
// added to or dropped from the handler's body fails here.
func TestAdminMCPRepoAutoMergeFieldsMatchHandlerBody(t *testing.T) {
	want := append([]string{}, adminmcp.RepoAutoMergeBodyFields()...)
	sort.Strings(want)
	if handler := codeScanningJSONNames(t, reflect.TypeOf(repoAutoMergeRequest{})); !reflect.DeepEqual(handler, want) {
		t.Fatalf("POST /api/repos/auto-merge takes %v\nbut repository.auto_merge sends %v\n(pkg/adminmcp/tools_repo_auto_merge_ops.go)", handler, want)
	}
}

func repoAutoMergeAuditRows(s *Server) []AuditEntry {
	return s.audit.RecentWithPrefixSince(time.Now().Add(-time.Minute), "repo_auto_merge")
}

// The section reports the live switch; a preview writes nothing; a preview
// overtaken by a dashboard toggle is refused at confirm; a confirm does what
// the card's POST does, audited the same way, and reads the switch back.
func TestAdminMCPRepoAutoMergePreviewConfirm(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	srv := newFullServer(t)
	level := config.SelfMergeMinACMMLevel
	srv.deps.Config.ACMMLevel = &level
	srv.authToken = "secret"
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	member := adminMCPAs(t, h, map[string]string{"X-Hive-User": "alice", "X-Hive-Role": config.RoleReadWrite, proxyAuthHeader: "secret"})
	owner := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})

	var read struct {
		Data struct {
			RepoAutoMerge struct {
				ACMMLevel    int `json:"acmm_level"`
				MinACMMLevel int `json:"min_acmm_level"`
				Repos        []struct {
					Repo    string `json:"repo"`
					Enabled bool   `json:"enabled"`
					Value   *bool  `json:"value"`
				} `json:"repos"`
			} `json:"repo_auto_merge"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(owner(codeScanningMCPCall("settings_read", `{"section":"auto_merge"}`))), &read); err != nil {
		t.Fatalf("settings_read section auto_merge: %v", err)
	}
	got := read.Data.RepoAutoMerge
	if got.ACMMLevel != 6 || got.MinACMMLevel != 6 || len(got.Repos) != 1 || got.Repos[0].Repo != "testorg/testrepo" || !got.Repos[0].Enabled || got.Repos[0].Value != nil {
		t.Fatalf("repo_auto_merge = %+v", got)
	}

	preview := func(enabled string) string {
		return codeScanningMCPCall("write_preview", `{"operation":"repository.auto_merge","args":{"repo":"testrepo","enabled":`+enabled+`}}`)
	}
	confirm := func(id string) string {
		return codeScanningMCPCall("write_confirm", `{"confirmation_id":"`+id+`"}`)
	}

	// The op reads the owner-only merge settings first.
	if out := member(preview("false")); !strings.Contains(out, "verified owner") {
		t.Fatalf("a read-write member's preview = %s, want it refused", out)
	}

	// A preview overtaken by the card switching the repository off and on
	// again (which records an explicit override) is refused.
	previewed := owner(preview("false"))
	stale := confirmationID(t, previewed)
	if !strings.Contains(previewed, "Narrowing:") || !srv.deps.Config.RepoAutoMergeEnabled("testrepo") || len(repoAutoMergeAuditRows(srv)) != 0 {
		t.Fatalf("the preview wrote something or did not disclose narrowing: %s", previewed)
	}
	for _, on := range []string{"false", "true"} {
		if w := postRepoPause(t, srv, "/api/repos/auto-merge", `{"repo":"testrepo","enabled":`+on+`}`, true); w.Code != http.StatusOK {
			t.Fatalf("the card's toggle: %d %s", w.Code, w.Body.String())
		}
	}
	if out := owner(confirm(stale)); !strings.Contains(out, "stale") {
		t.Fatalf("a confirm after a dashboard toggle = %s, want it refused as stale", out)
	}
	if !srv.deps.Config.RepoAutoMergeEnabled("testrepo") || len(repoAutoMergeAuditRows(srv)) != 2 {
		t.Fatalf("the stale confirm wrote: enabled %v, audit %+v", srv.deps.Config.RepoAutoMergeEnabled("testrepo"), repoAutoMergeAuditRows(srv))
	}

	// A fresh preview confirms, as the card's POST does.
	out := owner(confirm(confirmationID(t, owner(preview("false")))))
	if !strings.Contains(out, `"write_result"`) || !strings.Contains(out, `"after":{"enabled":false,"repo":"testorg/testrepo","value":false}`) {
		t.Fatalf("the owner's confirm = %s, want a write_result reading the switch back", out)
	}
	if srv.deps.Config.RepoAutoMergeEnabled("testrepo") {
		t.Fatal("the confirmed switch is not in the live config")
	}
	rows := repoAutoMergeAuditRows(srv)
	offs := 0
	for _, row := range rows {
		if strings.Contains(row.Detail, "enabled=false") {
			offs++
		}
	}
	if len(rows) != 3 || offs != 2 {
		t.Fatalf("audit after write_confirm = %+v, want a third repo_auto_merge entry, switching it off", rows)
	}

	// Switching on again is widening, and an unknown repository is refused.
	if previewed := owner(preview("true")); !strings.Contains(previewed, "the hive may merge PRs in testorg/testrepo once its own gates and GitHub's rules pass") {
		t.Fatalf("enabling did not disclose widening: %s", previewed)
	}
	if out := owner(codeScanningMCPCall("write_preview", `{"operation":"repository.auto_merge","args":{"repo":"testorg/nope","enabled":false}}`)); !strings.Contains(out, "is not a repository in project.repos") {
		t.Fatalf("an unknown repository previewed as %s", out)
	}
}
