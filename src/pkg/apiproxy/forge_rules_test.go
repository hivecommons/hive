package apiproxy

import (
	"testing"

	"github.com/hivecommons/hive/crux"
)

// TestForgeRulesWrappers verifies the apiproxy façade delegates to the shared
// crux/proxy forge rule table: MR/PR creation and merge are hard-denied at
// every tier, while issue comments are allowed from ISSUES_ONLY up. The
// exhaustive table-driven contract, including capability grants, lives in
// crux/proxy/forge_contract_test.go.
func TestForgeRulesWrappers(t *testing.T) {
	modes := []crux.AgentMode{
		crux.ModeAdvisory,
		crux.ModeIssuesOnly,
		crux.ModeIssuesAndPRs,
		crux.ModeIssuesPRsMerge,
	}
	denied := []struct{ method, path string }{
		{"POST", "/repos/org/repo/pulls"},
		{"PUT", "/repos/org/repo/pulls/1/merge"},
		{"POST", "/api/v4/projects/org%2Frepo/merge_requests"},
		{"PUT", "/api/v4/projects/org%2Frepo/merge_requests/1/merge"},
		{"POST", "/api/v1/repos/org/repo/pulls"},
		{"POST", "/api/v1/repos/org/repo/pulls/1/merge"},
	}
	for _, mode := range modes {
		for _, d := range denied {
			if AllowedByMode(mode, d.method, d.path) {
				t.Errorf("%s %s should be denied at mode %v", d.method, d.path, mode)
			}
			if msg, ok := DeniedMessage(d.method, d.path); !ok || msg == "" {
				t.Errorf("DeniedMessage(%s %s) = (%q,%v), want hard-deny directive", d.method, d.path, msg, ok)
			}
		}
	}

	comment := "/api/v4/projects/org%2Frepo/issues/1/notes"
	if AllowedByMode(crux.ModeAdvisory, "POST", comment) {
		t.Errorf("POST %s should be denied at ADVISORY", comment)
	}
	if !AllowedByMode(crux.ModeIssuesOnly, "POST", comment) {
		t.Errorf("POST %s should be allowed at ISSUES_ONLY", comment)
	}
	if _, ok := DeniedMessage("POST", comment); ok {
		t.Errorf("DeniedMessage(POST %s) matched a hard-deny rule unexpectedly", comment)
	}
}
