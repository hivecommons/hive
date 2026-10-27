package proxy

import (
	"net/http"
	"testing"

	"github.com/hivecommons/hive/crux/agent"
)

func TestAllowedByMode(t *testing.T) {
	tests := []struct {
		name   string
		mode   agent.AgentMode
		method string
		path   string
		want   bool
	}{
		// ── ADVISORY: read-only ──
		{"advisory allows GET issues", agent.ModeAdvisory, "GET", "/repos/org/repo/issues", true},
		{"advisory allows GET pulls", agent.ModeAdvisory, "GET", "/repos/org/repo/pulls", true},
		{"advisory allows HEAD", agent.ModeAdvisory, "HEAD", "/repos/org/repo", true},
		{"advisory allows OPTIONS", agent.ModeAdvisory, "OPTIONS", "/repos/org/repo", true},
		{"advisory blocks POST issues", agent.ModeAdvisory, "POST", "/repos/org/repo/issues", false},
		{"advisory blocks POST pulls", agent.ModeAdvisory, "POST", "/repos/org/repo/pulls", false},
		{"advisory blocks PATCH issue", agent.ModeAdvisory, "PATCH", "/repos/org/repo/issues/42", false},
		{"advisory blocks PUT merge", agent.ModeAdvisory, "PUT", "/repos/org/repo/pulls/42/merge", false},
		{"advisory blocks git push", agent.ModeAdvisory, "POST", "/org/repo.git/git-receive-pack", false},

		// ── ISSUES_ONLY: issues yes, PRs no ──
		{"issues-only allows GET", agent.ModeIssuesOnly, "GET", "/repos/org/repo/issues", true},
		{"issues-only allows POST issue", agent.ModeIssuesOnly, "POST", "/repos/org/repo/issues", true},
		{"issues-only allows PATCH issue", agent.ModeIssuesOnly, "PATCH", "/repos/org/repo/issues/42", true},
		{"issues-only allows comment", agent.ModeIssuesOnly, "POST", "/repos/org/repo/issues/42/comments", true},
		{"issues-only allows label", agent.ModeIssuesOnly, "POST", "/repos/org/repo/issues/42/labels", true},
		{"issues-only blocks POST pulls", agent.ModeIssuesOnly, "POST", "/repos/org/repo/pulls", false},
		{"issues-only blocks PATCH pull", agent.ModeIssuesOnly, "PATCH", "/repos/org/repo/pulls/42", false},
		{"issues-only blocks PUT merge", agent.ModeIssuesOnly, "PUT", "/repos/org/repo/pulls/42/merge", false},
		{"issues-only blocks git push", agent.ModeIssuesOnly, "POST", "/org/repo.git/git-receive-pack", false},
		{"issues-only blocks create ref", agent.ModeIssuesOnly, "POST", "/repos/org/repo/git/refs", false},

		// ── ISSUES_AND_PRS: issues + PRs, no merge ──
		{"prs allows GET", agent.ModeIssuesAndPRs, "GET", "/repos/org/repo/pulls", true},
		{"prs allows POST issue", agent.ModeIssuesAndPRs, "POST", "/repos/org/repo/issues", true},
		// Direct PR creation is a hard deny for every mode — agents use hive-open-pr.
		{"prs blocks direct POST pull", agent.ModeIssuesAndPRs, "POST", "/repos/org/repo/pulls", false},
		{"prs allows PATCH pull", agent.ModeIssuesAndPRs, "PATCH", "/repos/org/repo/pulls/42", true},
		{"prs allows PR review", agent.ModeIssuesAndPRs, "POST", "/repos/org/repo/pulls/42/reviews", true},
		{"prs allows git push", agent.ModeIssuesAndPRs, "POST", "/org/repo.git/git-receive-pack", true},
		{"prs allows create ref", agent.ModeIssuesAndPRs, "POST", "/repos/org/repo/git/refs", true},
		{"prs allows create commit", agent.ModeIssuesAndPRs, "POST", "/repos/org/repo/git/commits", true},
		{"prs allows delete ref", agent.ModeIssuesAndPRs, "DELETE", "/repos/org/repo/git/refs/heads/branch", true},
		{"prs blocks PUT merge", agent.ModeIssuesAndPRs, "PUT", "/repos/org/repo/pulls/42/merge", false},

		// ── ISSUES_PRS_MERGE: everything allowed ──
		{"merge allows GET", agent.ModeIssuesPRsMerge, "GET", "/repos/org/repo/issues", true},
		{"merge allows POST issue", agent.ModeIssuesPRsMerge, "POST", "/repos/org/repo/issues", true},
		// Even merge mode cannot POST /pulls directly — hive-open-pr is the only path.
		{"merge blocks direct POST pull", agent.ModeIssuesPRsMerge, "POST", "/repos/org/repo/pulls", false},
		// H1 (CWE-863): even merge mode cannot PUT /pulls/{n}/merge directly — it
		// is a hard deny for every mode, so agents route through the bound
		// hive-merge relay (SHA-pin + merge-eligible binding) instead.
		{"merge blocks direct PUT merge", agent.ModeIssuesPRsMerge, "PUT", "/repos/org/repo/pulls/42/merge", false},
		{"merge allows PUT update-branch", agent.ModeIssuesPRsMerge, "PUT", "/repos/org/repo/pulls/42/update-branch", true},
		{"merge allows git push", agent.ModeIssuesPRsMerge, "POST", "/org/repo.git/git-receive-pack", true},

		// ── Default deny: unknown operations ──
		{"unknown POST path denied", agent.ModeIssuesPRsMerge, "POST", "/repos/org/repo/unknown", false},
		{"unknown method denied", agent.ModeIssuesPRsMerge, "FOOBAR", "/repos/org/repo/issues", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AllowedByMode(tt.mode, tt.method, tt.path)
			if got != tt.want {
				t.Errorf("AllowedByMode(%v, %q, %q) = %v, want %v",
					tt.mode, tt.method, tt.path, got, tt.want)
			}
		})
	}
}

func TestGraphQLAllowed(t *testing.T) {
	tests := []struct {
		name       string
		mode       agent.AgentMode
		body       string
		wantAllow  bool
		wantMutate bool
	}{
		{"advisory allows query", agent.ModeAdvisory, `{"query":"query { repository(owner:\"org\",name:\"repo\") { issues { totalCount } } }"}`, true, false},
		{"advisory blocks mutation", agent.ModeAdvisory, `{"query":"mutation { createIssue(input:{repositoryId:\"R_1\",title:\"test\"}) { issue { id } } }"}`, false, true},
		{"issues-only allows mutation", agent.ModeIssuesOnly, `{"query":"mutation { createIssue(input:{repositoryId:\"R_1\",title:\"test\"}) { issue { id } } }"}`, true, true},
		{"issues-and-prs allows mutation", agent.ModeIssuesAndPRs, `{"query":"mutation { createIssue(input:{}) { issue { id } } }"}`, true, true},
		{"advisory blocks malformed json", agent.ModeAdvisory, `not json`, false, false},
		{"advisory allows empty query", agent.ModeAdvisory, `{"query":"{ viewer { login } }"}`, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed, isMutation := GraphQLAllowed(tt.mode, []byte(tt.body))
			if allowed != tt.wantAllow {
				t.Errorf("GraphQLAllowed(%v) allowed = %v, want %v", tt.mode, allowed, tt.wantAllow)
			}
			if isMutation != tt.wantMutate {
				t.Errorf("GraphQLAllowed(%v) isMutation = %v, want %v", tt.mode, isMutation, tt.wantMutate)
			}
		})
	}
}

func TestExtractRepo(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/repos/org/repo/issues", "org/repo"},
		{"/repos/org/repo/pulls/42/merge", "org/repo"},
		{"/repos/org/repo/git/refs", "org/repo"},
		{"/repos/org/repo/contents/README.md", "org/repo"},
		{"/org/repo.git/git-receive-pack", "org/repo"},
		{"/org/repo.git/git-upload-pack", "org/repo"},
		{"/group/sub/repo.git/git-receive-pack", "group/sub/repo"},
		{"/group/sub/sub2/repo.git/git-upload-pack", "group/sub/sub2/repo"},
		{"/group/sub/repo.git/info/refs", "group/sub/repo"},
		{"/repo.git/git-receive-pack", ""},
		// ".git/" that is not followed by a smart-HTTP endpoint is some other
		// API path, not a git request: no slug.
		{"/api/v4/projects/org%2Frepo/repository/files/foo.git/raw", "org/repo"},
		{"/org/repo.git/", ""},
		{"/org/repo.git/foo", ""},
		{"/org/repo.git/git-receive-pack/extra", ""},
		// Percent-escaped slug letters decode to the slug the forge sees, so
		// deny-list gates (pause, agent scope) cannot be evaded by escaping.
		{"/repos/o%72g/repo/issues", "org/repo"},
		{"/repos/org/r%65po/pulls/1/merge", "org/repo"},
		{"/o%72g/repo.git/git-receive-pack", "org/repo"},
		{"/api/v4/projects/o%72g/repo/issues", "org/repo"},
		{"/api/v4/projects/o%72g%2Frepo/issues", "org/repo"},
		{"/api/v4/projects/org/repo/iss%75es", "org/repo"},
		{"/api/v1/repos/org/repo/issues", "org/repo"},
		{"/api/v4/projects/org%2Frepo/issues", "org/repo"},
		{"/api/v4/projects/org/repo/issues", "org/repo"},
		{"/api/v4/projects/group/sub/repo/issues", "group/sub/repo"},
		{"/api/v4/projects/group/sub/sub2/repo/merge_requests/3/notes", "group/sub/sub2/repo"},
		{"/api/v4/projects/group%2Fsub%2Frepo/issues", "group/sub/repo"},
		{"/projects/group/sub/repo/repository/branches", "group/sub/repo"},
		{"/api/v4/projects/group/sub/repo", "group/sub/repo"},
		{"/api/v4/projects/group/repo/pipeline", "group/repo"},
		{"/api/v4/projects/group/repo/pipeline_schedules/7/play", "group/repo"},
		{"/api/v4/projects/group/repo/uploads", "group/repo"},
		{"/api/v4/projects/group/repo/-/packages", "group/repo"},
		// Group or project named like a resource word: gated on the plain
		// two-segment reading, never "" (which would skip every repo gate).
		{"/api/v4/projects/acme/releases/merge_requests", "acme/releases"},
		{"/api/v4/projects/org/issues", "org/issues"},
		// Documented limitation: an unencoded subgroup named like a resource
		// word cuts the slug short (the real project is "group/packages/repo");
		// the %2F-encoded spelling resolves exactly.
		{"/api/v4/projects/group/packages/repo/issues", "group/packages"},
		{"/api/v4/projects/group%2Fpackages%2Frepo/issues", "group/packages/repo"},
		{"/api/v4/projects/org/", ""},
		{"/api/v4/projects/123/issues", ""},
		{"/user", ""},
		{"/rate_limit", ""},
		{"/search/issues", ""},
		{"/orgs/myorg/repos", ""},
		{"/graphql", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := ExtractRepo(tt.path)
			if got != tt.want {
				t.Errorf("ExtractRepo(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// TestCanonicalGatePath covers the escaped-structural-segment seam: the gates
// receive the client's escaped path so %2F keeps a GitLab project in one
// segment, which means every OTHER escape must be decoded first or the literal
// prefixes ExtractRepo anchors on (/repos/, .git/, /projects/) can be hidden
// and the write slips every per-repo gate.
func TestCanonicalGatePath(t *testing.T) {
	tests := []struct {
		escaped  string
		want     string
		wantRepo string
	}{
		{"/repos/org/repo/issues", "/repos/org/repo/issues", "org/repo"},
		{"/r%65pos/org/repo/issues", "/repos/org/repo/issues", "org/repo"},
		{"/org/repo%2Egit/git-receive-pack", "/org/repo.git/git-receive-pack", "org/repo"},
		{"/org/repo.git/git-receive-p%61ck", "/org/repo.git/git-receive-pack", "org/repo"},
		{"/api/v4/pro%6Aects/org%2Frepo/issues", "/api/v4/projects/org%2Frepo/issues", "org/repo"},
		{"/api/v4/projects/group%2Fsub%2Frepo/issues", "/api/v4/projects/group%2Fsub%2Frepo/issues", "group/sub/repo"},
		{"/api/v4/projects/o%72g%2Frepo/iss%75es", "/api/v4/projects/org%2Frepo/issues", "org/repo"},
		{"/repos/org/repo/pulls/1/merg%65", "/repos/org/repo/pulls/1/merge", "org/repo"},
		// A doubly-escaped slash decodes once, as the forge decodes it: the
		// segment is the literal "org%2Frepo", not a namespace/project pair,
		// and ExtractRepo's resource-word fallback yields a slug that matches
		// no real repo — the write fails CLOSED rather than skipping the gates.
		{"/api/v4/projects/org%252Frepo/issues", "/api/v4/projects/org%252Frepo/issues", "org%2Frepo/issues"},
		// Undecodable segments are kept verbatim.
		{"/repos/org/re%zzpo/issues", "/repos/org/re%zzpo/issues", "org/re%zzpo"},
		{"/graphql", "/graphql", ""},
	}
	for _, tt := range tests {
		t.Run(tt.escaped, func(t *testing.T) {
			got := CanonicalGatePath(tt.escaped)
			if got != tt.want {
				t.Errorf("CanonicalGatePath(%q) = %q, want %q", tt.escaped, got, tt.want)
			}
			if repo := ExtractRepo(got); repo != tt.wantRepo {
				t.Errorf("ExtractRepo(CanonicalGatePath(%q)) = %q, want %q", tt.escaped, repo, tt.wantRepo)
			}
		})
	}
}

// TestRepoGatesEscapedStructuralSegments is the end-to-end assertion that an
// escaped prefix cannot turn a gated write into an ungated one.
func TestRepoGatesEscapedStructuralSegments(t *testing.T) {
	paused := func(repo string) bool { return repo == "org/repo" }
	serves := func(agent, repo string) bool { return repo == "other/repo" }
	allowed := map[string]bool{"other/repo": true}
	autoMerge := func(repo string) bool { return repo != "org/repo" }

	for _, escaped := range []string{
		"/r%65pos/org/repo/issues",
		"/org/repo%2Egit/git-receive-pack",
		"/api/v4/pro%6Aects/org%2Frepo/issues",
		"/api/v4/projects/o%72g%2Frepo/merge_requests",
	} {
		t.Run(escaped, func(t *testing.T) {
			path := CanonicalGatePath(escaped)
			if _, refused := RepoPauseRefusal(paused, "POST", path); !refused {
				t.Errorf("RepoPauseRefusal let %q through", escaped)
			}
			if _, refused := AgentRepoScopeRefusal(serves, "reviewer", "POST", path); !refused {
				t.Errorf("AgentRepoScopeRefusal let %q through", escaped)
			}
			if RepoFilterAllowed(allowed, "POST", path) {
				t.Errorf("RepoFilterAllowed let %q through", escaped)
			}
		})
	}

	path := CanonicalGatePath("/repos/org/repo/pulls/1/merg%65")
	if _, refused := RepoAutoMergeRefusal(autoMerge, http.MethodPut, path); !refused {
		t.Errorf("RepoAutoMergeRefusal let escaped merge path through")
	}
}

func TestRepoFilterAllowed(t *testing.T) {
	allowed := map[string]bool{
		"myorg/console":     true,
		"myorg/docs":        true,
		"myorg/sub/console": true,
	}

	tests := []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		{"GET any repo passes", "GET", "/repos/myorg/other/issues", true},
		{"HEAD any repo passes", "HEAD", "/repos/myorg/other/pulls", true},
		{"POST allowed repo passes", "POST", "/repos/myorg/console/issues", true},
		{"POST disallowed repo blocked", "POST", "/repos/projectbluefin/documentation/issues", false},
		{"PATCH allowed repo passes", "PATCH", "/repos/myorg/docs/issues/42", true},
		{"PATCH disallowed repo blocked", "PATCH", "/repos/myorg/other/issues/42", false},
		{"PUT disallowed repo blocked", "PUT", "/repos/myorg/other/pulls/1/merge", false},
		{"DELETE disallowed repo blocked", "DELETE", "/repos/myorg/other/git/refs/heads/branch", false},
		{"POST non-repo path passes", "POST", "/graphql", true},
		{"POST user endpoint passes", "POST", "/user", true},
		{"git push allowed repo passes", "POST", "/myorg/console.git/git-receive-pack", true},
		{"git push disallowed repo blocked", "POST", "/myorg/other.git/git-receive-pack", false},
		{"nested-group git push disallowed blocked", "POST", "/myorg/sub/other.git/git-receive-pack", false},
		{"nested-group git push allowed passes", "POST", "/myorg/sub/console.git/git-receive-pack", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RepoFilterAllowed(allowed, tt.method, tt.path)
			if got != tt.want {
				t.Errorf("RepoFilterAllowed(%q, %q) = %v, want %v",
					tt.method, tt.path, got, tt.want)
			}
		})
	}
}

func TestRepoFilterEmptyAllowedPassesAll(t *testing.T) {
	empty := map[string]bool{}
	if !RepoFilterAllowed(empty, "POST", "/repos/any/repo/issues") {
		t.Error("empty allowedRepos should allow all requests")
	}
}

func TestIsGitHubHost(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"api.github.com", true},
		{"github.com", true},
		{"example.com", false},
		{"api.github.com.evil.com", false},
		{"API.GITHUB.COM", true},
		{"Api.GitHub.com.", true},
		{"github.com.", true},
		{"evil-api.github.com", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsGitHubHost(tt.host); got != tt.want {
			t.Errorf("IsGitHubHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestNeedsMITM(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"api.github.com", true},
		{"github.com", false},
		{"example.com", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := NeedsMITM(tt.host); got != tt.want {
			t.Errorf("NeedsMITM(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestHostCanonicalization(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		host string
		want bool
	}{
		{"inspection upper", NeedsInspection, "API.GITHUB.COM", true},
		{"inspection mixed trailing dot", NeedsInspection, "Api.GitHub.com.", true},
		{"inspection lookalike", NeedsInspection, "evil-api.github.com", false},
		{"inspection empty", NeedsInspection, "", false},
		{"mitm trailing dot", NeedsMITM, "api.github.com.", true},
		{"linear upper", IsLinearHost, "API.LINEAR.APP", true},
		{"linear trailing dot", IsLinearHost, "api.linear.app.", true},
		{"linear lookalike", IsLinearHost, "evil-api.linear.app", false},
		{"copilot upper", IsCopilotAPIHost, "API.GITHUBCOPILOT.COM", true},
		{"copilot mixed trailing dot", IsCopilotAPIHost, "Api.GitHubCopilot.com.", true},
		{"copilot lookalike", IsCopilotAPIHost, "evilgithubcopilot.com", false},
		{"copilot empty", IsCopilotAPIHost, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fn(tt.host); got != tt.want {
				t.Errorf("host %q = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}
