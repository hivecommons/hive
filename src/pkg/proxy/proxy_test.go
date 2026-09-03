package proxy

import (
	"log/slog"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

func TestIsGitHubHostExtended(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"api.github.com", true},
		{"github.com", true},
		{"gitlab.com", false},
		{"api.github.com.evil.com", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := IsGitHubHost(tt.host); got != tt.want {
			t.Errorf("IsGitHubHost(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestAllowedByModeExtended(t *testing.T) {
	tests := []struct {
		name   string
		mode   agent.AgentMode
		method string
		path   string
		want   bool
	}{
		{"GET always allowed", agent.ModeAdvisory, "GET", "/repos/org/repo/issues", true},
		{"HEAD always allowed", agent.ModeAdvisory, "HEAD", "/anything", true},
		{"OPTIONS always allowed", agent.ModeAdvisory, "OPTIONS", "/anything", true},

		{"advisory cannot create issues", agent.ModeAdvisory, "POST", "/repos/org/repo/issues", false},
		{"advisory cannot create PRs", agent.ModeAdvisory, "POST", "/repos/org/repo/pulls", false},

		{"issues-only can create issues", agent.ModeIssuesOnly, "POST", "/repos/org/repo/issues", true},
		{"issues-only can comment", agent.ModeIssuesOnly, "POST", "/repos/org/repo/issues/1/comments", true},
		{"issues-only cannot create PRs", agent.ModeIssuesOnly, "POST", "/repos/org/repo/pulls", false},

		// Direct PR creation is a hard deny for EVERY mode — agents must use
		// hive-open-pr so the App bot authors the PR (never the login user).
		{"issues-and-prs cannot create PRs directly (use hive-open-pr)", agent.ModeIssuesAndPRs, "POST", "/repos/org/repo/pulls", false},
		{"issues-and-prs can push", agent.ModeIssuesAndPRs, "POST", "/repos/org/repo.git/git-receive-pack", true},
		{"issues-and-prs cannot merge", agent.ModeIssuesAndPRs, "PUT", "/repos/org/repo/pulls/1/merge", false},

		// H1 (CWE-863): merge mode cannot merge directly either — PUT /pulls/{n}/merge
		// is a hard deny for every mode; agents route through the hive-merge relay.
		{"merge mode cannot merge directly (use hive-merge relay)", agent.ModeIssuesPRsMerge, "PUT", "/repos/org/repo/pulls/1/merge", false},
		{"merge mode cannot create PRs directly (use hive-open-pr)", agent.ModeIssuesPRsMerge, "POST", "/repos/org/repo/pulls", false},

		{"git upload-pack always allowed", agent.ModeAdvisory, "POST", "/repos/org/repo.git/git-upload-pack", true},

		{"oauth login allowed in advisory", agent.ModeAdvisory, "POST", "/login/device/code", true},
		{"oauth token allowed in advisory", agent.ModeAdvisory, "POST", "/login/oauth/access_token", true},

		{"unknown method denied", agent.ModeIssuesPRsMerge, "PATCH", "/unknown/path", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AllowedByMode(tt.mode, tt.method, tt.path); got != tt.want {
				t.Errorf("AllowedByMode(%v, %q, %q) = %v, want %v", tt.mode, tt.method, tt.path, got, tt.want)
			}
		})
	}
}

func TestGraphQLAllowedExtended(t *testing.T) {
	tests := []struct {
		name       string
		mode       agent.AgentMode
		body       string
		wantOK     bool
		wantMutate bool
	}{
		{"query in advisory", agent.ModeAdvisory, `{"query":"{ viewer { login } }"}`, true, false},
		{"mutation in advisory", agent.ModeAdvisory, `{"query":"mutation { createIssue(...) { ... } }"}`, false, true},
		{"mutation in issues-only", agent.ModeIssuesOnly, `{"query":"mutation { createIssue(...) { ... } }"}`, true, true},
		{"invalid json", agent.ModeAdvisory, `not json`, false, false},
		{"empty query", agent.ModeAdvisory, `{"query":""}`, true, false},
		{"multi-line mutation", agent.ModeAdvisory, `{"query":"query Q { viewer { login } }\nmutation M { createIssue { id } }"}`, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, isMut := GraphQLAllowed(tt.mode, []byte(tt.body))
			if ok != tt.wantOK {
				t.Errorf("allowed = %v, want %v", ok, tt.wantOK)
			}
			if isMut != tt.wantMutate {
				t.Errorf("isMutation = %v, want %v", isMut, tt.wantMutate)
			}
		})
	}
}

func TestIsGraphQLPath(t *testing.T) {
	if !IsGraphQLPath("/graphql") {
		t.Error("/graphql should be true")
	}
	if IsGraphQLPath("/api/graphql") {
		t.Error("/api/graphql should be false")
	}
}

func TestExtractAgentName(t *testing.T) {
	req, _ := http.NewRequest("GET", "https://api.github.com/repos/org/repo", nil)
	req.Header.Set("Proxy-Authorization", "hive scanner")
	name := extractAgentName(req)
	if name != "scanner" {
		t.Errorf("extractAgentName hive prefix = %q, want %q", name, "scanner")
	}

	req2, _ := http.NewRequest("GET", "https://api.github.com/repos/org/repo", nil)
	name2 := extractAgentName(req2)
	if name2 != "" {
		t.Errorf("extractAgentName without header = %q, want empty", name2)
	}

	req3, _ := http.NewRequest("GET", "https://api.github.com/repos/org/repo", nil)
	req3.Header.Set("Proxy-Authorization", "unknown-scheme value")
	name3 := extractAgentName(req3)
	if name3 != "" {
		t.Errorf("extractAgentName unknown scheme = %q, want empty", name3)
	}
}

func TestExtractSNI(t *testing.T) {
	if sni := extractSNI(nil); sni != "" {
		t.Errorf("nil data should return empty, got %q", sni)
	}
	if sni := extractSNI([]byte{1, 2, 3}); sni != "" {
		t.Errorf("short data should return empty, got %q", sni)
	}
	if sni := extractSNI(make([]byte, 10)); sni != "" {
		t.Errorf("zeroed data should return empty, got %q", sni)
	}
}

func TestRecordViolation(t *testing.T) {
	p := &GitHubProxy{
		violations: make(map[string]int),
		logger:     slog.Default(),
	}
	p.recordViolation("scanner", "POST", "/repos/org/repo/pulls")
	if p.violations["scanner"] != 1 {
		t.Errorf("violations = %d, want 1", p.violations["scanner"])
	}
	p.recordViolation("scanner", "POST", "/repos/org/repo/pulls")
	if p.violations["scanner"] != 2 {
		t.Errorf("violations = %d, want 2", p.violations["scanner"])
	}
}

func TestViolationsSnapshot(t *testing.T) {
	p := &GitHubProxy{
		violations: map[string]int{"scanner": 3, "quality": 1},
	}
	snap := p.Violations()
	if snap["scanner"] != 3 {
		t.Errorf("scanner = %d, want 3", snap["scanner"])
	}
	snap["scanner"] = 999
	if p.violations["scanner"] != 3 {
		t.Error("snapshot should be a copy")
	}
}

func TestAgentViolations(t *testing.T) {
	p := &GitHubProxy{
		violations: map[string]int{"scanner": 5},
	}
	if got := p.AgentViolations("scanner"); got != 5 {
		t.Errorf("AgentViolations = %d, want 5", got)
	}
	if got := p.AgentViolations("unknown"); got != 0 {
		t.Errorf("AgentViolations unknown = %d, want 0", got)
	}
}
