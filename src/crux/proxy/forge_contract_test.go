package proxy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agent"
)

// ForgeEndpoint specifies the HTTP method and path for a forge operation.
type ForgeEndpoint struct {
	Method string
	Path   string
}

// ForgeRuleContract defines the expected write-gate behavior across all supported
// forges. Every rule in this contract MUST provide the corresponding endpoint for
// GitHub, GitLab, and Gitea so that any rule added for one forge must be answered
// for all three.
type ForgeRuleContract struct {
	Name       string
	MinMode    agent.AgentMode
	Capability func(agent.AgentCapabilities) bool
	HardDeny   bool

	GitHub ForgeEndpoint
	GitLab ForgeEndpoint
	Gitea  ForgeEndpoint
}

// ForgeRulesContract returns the canonical table-driven rules contract shared
// across GitHub, GitLab, and Gitea.
var ForgeRulesContract = []ForgeRuleContract{
	{
		Name:    "ReadIssues",
		MinMode: agent.ModeAdvisory,
		GitHub:  ForgeEndpoint{Method: "GET", Path: "/repos/org/repo/issues"},
		GitLab:  ForgeEndpoint{Method: "GET", Path: "/api/v4/projects/org%2Frepo/issues"},
		Gitea:   ForgeEndpoint{Method: "GET", Path: "/api/v1/repos/org/repo/issues"},
	},
	{
		Name:    "ReadChangeRequests",
		MinMode: agent.ModeAdvisory,
		GitHub:  ForgeEndpoint{Method: "GET", Path: "/repos/org/repo/pulls"},
		GitLab:  ForgeEndpoint{Method: "GET", Path: "/api/v4/projects/org%2Frepo/merge_requests"},
		Gitea:   ForgeEndpoint{Method: "GET", Path: "/api/v1/repos/org/repo/pulls"},
	},
	{
		Name:    "CreateIssue",
		MinMode: agent.ModeIssuesOnly,
		GitHub:  ForgeEndpoint{Method: "POST", Path: "/repos/org/repo/issues"},
		GitLab:  ForgeEndpoint{Method: "POST", Path: "/api/v4/projects/org%2Frepo/issues"},
		Gitea:   ForgeEndpoint{Method: "POST", Path: "/api/v1/repos/org/repo/issues"},
	},
	{
		Name:    "EditIssue",
		MinMode: agent.ModeIssuesOnly,
		GitHub:  ForgeEndpoint{Method: "PATCH", Path: "/repos/org/repo/issues/42"},
		GitLab:  ForgeEndpoint{Method: "PUT", Path: "/api/v4/projects/org%2Frepo/issues/42"},
		Gitea:   ForgeEndpoint{Method: "PATCH", Path: "/api/v1/repos/org/repo/issues/42"},
	},
	{
		Name:       "CommentIssue",
		MinMode:    agent.ModeIssuesOnly,
		Capability: agent.AgentCapabilities.CanConverse,
		GitHub:     ForgeEndpoint{Method: "POST", Path: "/repos/org/repo/issues/42/comments"},
		GitLab:     ForgeEndpoint{Method: "POST", Path: "/api/v4/projects/org%2Frepo/issues/42/notes"},
		Gitea:      ForgeEndpoint{Method: "POST", Path: "/api/v1/repos/org/repo/issues/42/comments"},
	},
	{
		Name:    "AddLabels",
		MinMode: agent.ModeIssuesOnly,
		GitHub:  ForgeEndpoint{Method: "POST", Path: "/repos/org/repo/issues/42/labels"},
		GitLab:  ForgeEndpoint{Method: "POST", Path: "/api/v4/projects/org%2Frepo/labels"},
		Gitea:   ForgeEndpoint{Method: "POST", Path: "/api/v1/repos/org/repo/issues/42/labels"},
	},
	{
		Name:    "RemoveLabelHoldGate",
		MinMode: agent.ModeIssuesOnly,
		GitHub:  ForgeEndpoint{Method: "DELETE", Path: "/repos/org/repo/issues/42/labels/hold"},
		GitLab:  ForgeEndpoint{Method: "PUT", Path: "/api/v4/projects/org%2Frepo/issues/42"},
		Gitea:   ForgeEndpoint{Method: "DELETE", Path: "/api/v1/repos/org/repo/issues/42/labels/hold"},
	},
	{
		Name:     "CreateChangeRequestHardDeny",
		MinMode:  agent.ModeIssuesAndPRs,
		HardDeny: true,
		GitHub:   ForgeEndpoint{Method: "POST", Path: "/repos/org/repo/pulls"},
		GitLab:   ForgeEndpoint{Method: "POST", Path: "/api/v4/projects/org%2Frepo/merge_requests"},
		Gitea:    ForgeEndpoint{Method: "POST", Path: "/api/v1/repos/org/repo/pulls"},
	},
	{
		Name:     "MergeChangeRequestHardDeny",
		MinMode:  agent.ModeIssuesPRsMerge,
		HardDeny: true,
		GitHub:   ForgeEndpoint{Method: "PUT", Path: "/repos/org/repo/pulls/42/merge"},
		GitLab:   ForgeEndpoint{Method: "PUT", Path: "/api/v4/projects/org%2Frepo/merge_requests/42/merge"},
		Gitea:    ForgeEndpoint{Method: "POST", Path: "/api/v1/repos/org/repo/pulls/42/merge"},
	},
	{
		Name:    "EditChangeRequest",
		MinMode: agent.ModeIssuesAndPRs,
		GitHub:  ForgeEndpoint{Method: "PATCH", Path: "/repos/org/repo/pulls/42"},
		GitLab:  ForgeEndpoint{Method: "PUT", Path: "/api/v4/projects/org%2Frepo/merge_requests/42"},
		Gitea:   ForgeEndpoint{Method: "PATCH", Path: "/api/v1/repos/org/repo/pulls/42"},
	},
	{
		Name:       "ReviewChangeRequest",
		MinMode:    agent.ModeIssuesAndPRs,
		Capability: agent.AgentCapabilities.CanConverse,
		GitHub:     ForgeEndpoint{Method: "POST", Path: "/repos/org/repo/pulls/42/reviews"},
		GitLab:     ForgeEndpoint{Method: "POST", Path: "/api/v4/projects/org%2Frepo/merge_requests/42/notes"},
		Gitea:      ForgeEndpoint{Method: "POST", Path: "/api/v1/repos/org/repo/pulls/42/reviews"},
	},
	{
		Name:    "GitPush",
		MinMode: agent.ModeIssuesAndPRs,
		GitHub:  ForgeEndpoint{Method: "POST", Path: "/repos/org/repo/git/refs"},
		GitLab:  ForgeEndpoint{Method: "POST", Path: "/api/v4/projects/org%2Frepo/repository/branches"},
		Gitea:   ForgeEndpoint{Method: "POST", Path: "/api/v1/repos/org/repo/git/refs"},
	},
	{
		Name:    "UpdateBranchRebase",
		MinMode: agent.ModeIssuesPRsMerge,
		GitHub:  ForgeEndpoint{Method: "PUT", Path: "/repos/org/repo/pulls/42/update-branch"},
		GitLab:  ForgeEndpoint{Method: "PUT", Path: "/api/v4/projects/org%2Frepo/merge_requests/42/rebase"},
		Gitea:   ForgeEndpoint{Method: "POST", Path: "/api/v1/repos/org/repo/pulls/42/update"},
	},
}

// RunForgeRulesContractTest executes the shared write-gate contract test across
// GitHub, GitLab, and Gitea.
func RunForgeRulesContractTest(t *testing.T) {
	t.Helper()

	forges := []struct {
		name string
		get  func(ForgeRuleContract) ForgeEndpoint
	}{
		{"GitHub", func(r ForgeRuleContract) ForgeEndpoint { return r.GitHub }},
		{"GitLab", func(r ForgeRuleContract) ForgeEndpoint { return r.GitLab }},
		{"Gitea", func(r ForgeRuleContract) ForgeEndpoint { return r.Gitea }},
	}

	for _, rule := range ForgeRulesContract {
		rule := rule
		// Enforce contract completeness: every rule must have non-empty endpoints for all forges.
		if rule.GitHub.Method == "" || rule.GitHub.Path == "" {
			t.Fatalf("contract rule %q must define GitHub endpoint", rule.Name)
		}
		if rule.GitLab.Method == "" || rule.GitLab.Path == "" {
			t.Fatalf("contract rule %q must define GitLab endpoint", rule.Name)
		}
		if rule.Gitea.Method == "" || rule.Gitea.Path == "" {
			t.Fatalf("contract rule %q must define Gitea endpoint", rule.Name)
		}

		t.Run(rule.Name, func(t *testing.T) {
			for _, f := range forges {
				ep := f.get(rule)
				t.Run(f.name, func(t *testing.T) {
					if rule.HardDeny {
						// Hard-denied operations MUST be refused at EVERY agent mode,
						// and MUST return a directive message pointing to the request-file path.
						for _, mode := range allModes {
							if AllowedByMode(mode, ep.Method, ep.Path) {
								t.Errorf("%s %s: hard-deny %s must be refused at mode %v",
									f.name, rule.Name, ep.Path, mode)
							}
						}
						msg, denied := DeniedMessage(ep.Method, ep.Path)
						if !denied || msg == "" {
							t.Errorf("%s %s: expected DeniedMessage for hard-deny %s %s, got denied=%v msg=%q",
								f.name, rule.Name, ep.Method, ep.Path, denied, msg)
						}
						return
					}

					// Standard mode-ladder enforcement.
					for _, mode := range allModes {
						allowed := AllowedByMode(mode, ep.Method, ep.Path)
						want := mode >= rule.MinMode
						if allowed != want {
							t.Errorf("%s %s at %v: got allowed=%v, want %v (%s %s)",
								f.name, rule.Name, mode, allowed, want, ep.Method, ep.Path)
						}
					}

					// Capability checks (e.g. converse widening conversation routes).
					if rule.Capability != nil {
						converseCaps := agent.AgentCapabilities{Converse: true}
						noCaps := agent.AgentCapabilities{}

						// Below MinMode, holding converse MUST grant access.
						if !AllowedByModeCaps(agent.ModeAdvisory, converseCaps, ep.Method, ep.Path) {
							t.Errorf("%s %s: converse capability must grant access at ModeAdvisory for %s %s",
								f.name, rule.Name, ep.Method, ep.Path)
						}
						// Below MinMode, zero capabilities MUST still be denied.
						if AllowedByModeCaps(agent.ModeAdvisory, noCaps, ep.Method, ep.Path) {
							t.Errorf("%s %s: zero capabilities must NOT grant access at ModeAdvisory for %s %s",
								f.name, rule.Name, ep.Method, ep.Path)
						}
					}
				})
			}
		})
	}

	// Default-deny (fail-closed) contract test for all three forges:
	// Unmatched or unknown paths MUST be denied for all agent modes.
	t.Run("DefaultDenyUnmatchedRoutes", func(t *testing.T) {
		unmatchedPaths := []struct {
			forge  string
			method string
			path   string
		}{
			{"GitHub", "POST", "/repos/org/repo/unknown_endpoint"},
			{"GitHub", "DELETE", "/repos/org/repo/unknown_endpoint"},
			{"GitLab", "POST", "/api/v4/projects/org%2Frepo/unknown_endpoint"},
			{"GitLab", "PUT", "/api/v4/projects/org%2Frepo/unknown_endpoint"},
			{"Gitea", "POST", "/api/v1/repos/org/repo/unknown_endpoint"},
			{"Gitea", "PATCH", "/api/v1/repos/org/repo/unknown_endpoint"},
		}

		for _, tc := range unmatchedPaths {
			name := fmt.Sprintf("%s_%s_%s", tc.forge, tc.method, strings.TrimPrefix(tc.path, "/"))
			t.Run(name, func(t *testing.T) {
				for _, mode := range allModes {
					if AllowedByMode(mode, tc.method, tc.path) {
						t.Errorf("%s: unmatched route %s %s must be denied at mode %v",
							tc.forge, tc.method, tc.path, mode)
					}
				}
			})
		}
	})

	// Variations and path equivalences across forges.
	t.Run("ForgePathVariations", func(t *testing.T) {
		variations := []struct {
			name     string
			method   string
			path     string
			minMode  agent.AgentMode
			hardDeny bool
		}{
			// Gitea without /api/v1 prefix
			{"Gitea_Pulls_Bare_Deny", "POST", "/repos/org/repo/pulls", agent.ModeIssuesAndPRs, true},
			{"Gitea_Merge_Bare_Deny", "POST", "/repos/org/repo/pulls/1/merge", agent.ModeIssuesPRsMerge, true},
			{"Gitea_Update_Bare", "POST", "/repos/org/repo/pulls/1/update", agent.ModeIssuesPRsMerge, false},
			{"Gitea_UpdateBranch_Bare", "POST", "/repos/org/repo/pulls/1/update-branch", agent.ModeIssuesPRsMerge, false},

			// GitLab with numeric project ID
			{"GitLab_NumericID_MR_Deny", "POST", "/api/v4/projects/12345/merge_requests", agent.ModeIssuesAndPRs, true},
			{"GitLab_NumericID_Merge_PUT_Deny", "PUT", "/api/v4/projects/12345/merge_requests/1/merge", agent.ModeIssuesPRsMerge, true},
			{"GitLab_NumericID_Merge_POST_Deny", "POST", "/api/v4/projects/12345/merge_requests/1/merge", agent.ModeIssuesPRsMerge, true},
			{"GitLab_NumericID_Rebase", "PUT", "/api/v4/projects/12345/merge_requests/1/rebase", agent.ModeIssuesPRsMerge, false},

			// GitLab discussions
			{"GitLab_Issue_Discussions", "POST", "/api/v4/projects/org%2Frepo/issues/1/discussions", agent.ModeIssuesOnly, false},
			{"GitLab_MR_Discussions", "POST", "/api/v4/projects/org%2Frepo/merge_requests/1/discussions", agent.ModeIssuesAndPRs, false},
		}

		for _, v := range variations {
			t.Run(v.name, func(t *testing.T) {
				if v.hardDeny {
					for _, mode := range allModes {
						if AllowedByMode(mode, v.method, v.path) {
							t.Errorf("%s: hard deny failed for %s %s at mode %v", v.name, v.method, v.path, mode)
						}
					}
					return
				}
				for _, mode := range allModes {
					allowed := AllowedByMode(mode, v.method, v.path)
					want := mode >= v.minMode
					if allowed != want {
						t.Errorf("%s at %v: got %v, want %v (%s %s)", v.name, mode, allowed, want, v.method, v.path)
					}
				}
			})
		}
	})
}

func TestForgeRulesContract(t *testing.T) {
	RunForgeRulesContractTest(t)
}
