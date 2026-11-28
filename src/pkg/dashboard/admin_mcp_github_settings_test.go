package dashboard

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// githubSettingsNotInOp are the githubAPISettings fields github.settings
// leaves out, each with why. Only the fleet agent cap has an admin MCP op of
// its own; the three switches are not yet reachable from the admin MCP.
var githubSettingsNotInOp = map[string]string{
	"agent_github_api_hourly_cap": "set by its own op, github.agent_api_hourly_cap",
	"graphql_pr_batch":            "an on/off switch, left out of this op; not yet reachable from the admin MCP",
	"app_authored_prs":            "an authorship switch, left out of this op; not yet reachable from the admin MCP",
	"app_signed_commits":          "an authorship switch, left out of this op; not yet reachable from the admin MCP",
}

// github.settings refuses any field it does not know, so a field added to
// the endpoint's githubAPISettings must be added to the op (or listed above
// as left out, with why), or the MCP could not set it; and a field the op
// still offers after the handler drops it would be silently ignored. Both
// directions fail here.
func TestAdminMCPGitHubSettingsFieldsMatchHandlerBody(t *testing.T) {
	handler := reviewConfigPutBodyFields(t, reflect.TypeOf(githubAPISettings{}), "")
	op := adminmcp.GitHubSettingsBodyFields()
	sort.Strings(handler)
	inOp := map[string]bool{}
	for _, f := range op {
		if inOp[f] {
			t.Errorf("github.settings lists %q twice", f)
		}
		inOp[f] = true
	}
	inHandler := map[string]bool{}
	for _, f := range handler {
		inHandler[f] = true
		_, elsewhere := githubSettingsNotInOp[f]
		if inOp[f] && elsewhere {
			t.Errorf("%q is in github.settings and also listed as set elsewhere", f)
		}
		if !inOp[f] && !elsewhere {
			t.Errorf("PUT /api/config/github takes %q, but github.settings does not: add it to githubSettingsFields (pkg/adminmcp/tools_github_settings_ops.go)", f)
		}
	}
	for _, f := range op {
		if !inHandler[f] {
			t.Errorf("github.settings offers %q, which PUT /api/config/github does not read", f)
		}
	}
	for f := range githubSettingsNotInOp {
		if !inHandler[f] {
			t.Errorf("githubSettingsNotInOp lists %q, which PUT /api/config/github no longer reads", f)
		}
	}
}

// github.settings is owner only, as the Forge App settings are: the endpoint
// calls requireOwnerRole and a confirmed write runs as the verified caller.
// A read-write member's confirm is refused and changes nothing. The owner's
// confirm sends only the named fields, the ETag cache resize reaches the live
// cache, and write_confirm reports the fields as the hive now holds them.
func TestAdminMCPGitHubSettingsIsOwnerOnlyAndReportsAfter(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	_, maxBytes0, maxEntries0 := github.ETagCacheUsage()
	t.Cleanup(func() { github.SetETagCacheLimits(maxBytes0, maxEntries0) })
	srv := newFullServer(t)
	srv.authToken = "secret"
	srv.deps.Config.GitHub.PRDetailTTLS = 600
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	member := adminMCPAs(t, h, map[string]string{"X-Hive-User": "alice", "X-Hive-Role": config.RoleReadWrite, proxyAuthHeader: "secret"})
	owner := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})
	const preview = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"github.settings","args":{"api_reserve":600,"etag_cache_max_mb":256}}}}`
	confirm := func(id string) string {
		return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"` + id + `"}}}`
	}

	if got := member(confirm(confirmationID(t, member(preview)))); !strings.Contains(got, "owner access required") {
		t.Fatalf("a read-write member's confirm = %s, want the endpoint's owner refusal", got)
	}
	if g := srv.deps.Config.GitHub; g.APIReserve != 0 || g.ETagCacheMaxMB != 0 {
		t.Fatalf("a refused write changed the github settings to %+v", g)
	}

	got := owner(confirm(confirmationID(t, owner(preview))))
	if !strings.Contains(got, `"write_result"`) || !strings.Contains(got, `"after":{"api_reserve":600,"etag_cache_max_mb":256}`) {
		t.Fatalf("the owner's confirm = %s, want a write_result with the after state", got)
	}
	g := srv.deps.Config.GitHub
	if g.APIReserve != 600 || g.ETagCacheMaxMB != 256 {
		t.Fatalf("github settings = %+v, want api_reserve 600 and etag_cache_max_mb 256", g)
	}
	if g.PRDetailTTLS != 600 || g.ETagCacheMaxEntries != 0 {
		t.Fatalf("a github.settings write changed a field it did not name: %+v", g)
	}
	if _, maxBytes, _ := github.ETagCacheUsage(); maxBytes != 256<<20 {
		t.Fatalf("etag cache limit not applied live: %d bytes", maxBytes)
	}
}

// The endpoint's range checks are not repeated in the op: an ETag cache
// above its maximum previews, and the endpoint's 400 reaches the caller
// verbatim.
func TestAdminMCPGitHubSettingsPassesHandler400Through(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	srv := newFullServer(t)
	srv.authToken = "secret"
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	owner := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})
	const preview = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"github.settings","args":{"etag_cache_max_mb":4096}}}}`
	id := confirmationID(t, owner(preview))
	got := owner(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"` + id + `"}}}`)
	if !strings.Contains(got, `"hive-refusal"`) || !strings.Contains(got, `"status_code":400`) || !strings.Contains(got, "etag_cache_max_mb must be between 1 and 2048, or 0 for the default (128)") {
		t.Fatalf("confirm = %s, want the endpoint's 400 as a hive refusal", got)
	}
	if g := srv.deps.Config.GitHub; g.ETagCacheMaxMB != 0 {
		t.Fatalf("a refused write changed etag_cache_max_mb to %d", g.ETagCacheMaxMB)
	}
}
