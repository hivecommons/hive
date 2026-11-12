package dashboard

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

// reviewConfigPutBodyFields lists every field of a body struct by its JSON
// name, dotted, descending into any struct (or pointer to one) a field holds.
func reviewConfigPutBodyFields(t *testing.T, typ reflect.Type, prefix string) []string {
	t.Helper()
	var out []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Fatalf("%s%s has no JSON name, so the endpoint cannot be sent it by name", prefix, f.Name)
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct {
			out = append(out, reviewConfigPutBodyFields(t, ft, prefix+name+".")...)
			continue
		}
		out = append(out, prefix+name)
	}
	return out
}

// review.settings refuses any field it does not know, so a field added to
// PUT /api/config/review must be added to the op too, or the MCP could not
// set it; and a field the op still offers after the handler drops it would
// be silently ignored. Both directions fail here.
func TestAdminMCPReviewSettingsFieldsMatchHandlerBody(t *testing.T) {
	handler := reviewConfigPutBodyFields(t, reflect.TypeOf(reviewConfigPutBody{}), "")
	op := adminmcp.ReviewSettingsBodyFields()
	sort.Strings(handler)
	sort.Strings(op)
	inOp := map[string]bool{}
	for _, f := range op {
		if inOp[f] {
			t.Errorf("review.settings lists %q twice", f)
		}
		inOp[f] = true
	}
	inHandler := map[string]bool{}
	for _, f := range handler {
		inHandler[f] = true
		if !inOp[f] {
			t.Errorf("PUT /api/config/review takes %q, but review.settings does not: add it to reviewSettingsFields (pkg/adminmcp/tools_review_settings_ops.go)", f)
		}
	}
	for _, f := range op {
		if !inHandler[f] {
			t.Errorf("review.settings offers %q, which PUT /api/config/review does not read", f)
		}
	}
}

// review.settings is owner only, as the Review Gate settings are: the
// endpoint calls requireOwnerRole and a confirmed write runs as the verified
// caller. A read-write member's confirm is refused and changes nothing. The
// verified owner's confirm sends only the named fields, and write_confirm
// reports them as the hive now holds them.
func TestAdminMCPReviewSettingsIsOwnerOnlyAndReportsAfter(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	srv := newFullServer(t)
	srv.authToken = "secret"
	srv.deps.Config.Review.FanOut = true
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	member := adminMCPAs(t, h, map[string]string{"X-Hive-User": "alice", "X-Hive-Role": config.RoleReadWrite, proxyAuthHeader: "secret"})
	owner := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})
	const preview = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"review.settings","args":{"reviewer_autoscale":{"enabled":true,"max":2}}}}}`
	confirm := func(id string) string {
		return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"` + id + `"}}}`
	}

	if got := member(confirm(confirmationID(t, member(preview)))); !strings.Contains(got, "owner access required") {
		t.Fatalf("a read-write member's confirm = %s, want the endpoint's owner refusal", got)
	}
	if as := srv.deps.Config.Review.Reviewers.Autoscale; as.Enabled || as.Max != 0 {
		t.Fatalf("a refused write changed reviewer autoscale to %+v", as)
	}

	got := owner(confirm(confirmationID(t, owner(preview))))
	if !strings.Contains(got, `"write_result"`) || !strings.Contains(got, `"after":{"reviewer_autoscale.enabled":true,"reviewer_autoscale.max":2}`) {
		t.Fatalf("the owner's confirm = %s, want a write_result with the after state", got)
	}
	if as := srv.deps.Config.Review.Reviewers.Autoscale; !as.Enabled || as.Max != 2 || as.Min != 0 {
		t.Fatalf("reviewer autoscale = %+v, want enabled with max 2 and min untouched", as)
	}
	if !srv.deps.Config.Review.FanOut {
		t.Fatal("a review.settings write that did not name fan_out turned it off")
	}
}

// The endpoint's own validation is not repeated in the op: a band with min
// above max previews, and the endpoint's 400 reaches the caller verbatim.
func TestAdminMCPReviewSettingsPassesHandler400Through(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	srv := newFullServer(t)
	srv.authToken = "secret"
	h := srv.authenticate(srv.roleEnforcement(srv.securityHeaders(srv.mux)))
	owner := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})
	const preview = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"review.settings","args":{"reviewer_autoscale":{"enabled":true,"min":3,"max":2}}}}}`
	id := confirmationID(t, owner(preview))
	got := owner(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"` + id + `"}}}`)
	if !strings.Contains(got, `"hive-refusal"`) || !strings.Contains(got, `"status_code":400`) || !strings.Contains(got, "review.reviewers.autoscale: min 3 is greater than max 2") {
		t.Fatalf("confirm = %s, want the endpoint's 400 as a hive refusal", got)
	}
	if as := srv.deps.Config.Review.Reviewers.Autoscale; as.Enabled || as.Min != 0 || as.Max != 0 {
		t.Fatalf("a refused write changed reviewer autoscale to %+v", as)
	}
}
