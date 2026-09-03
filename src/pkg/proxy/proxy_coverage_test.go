package proxy

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/agent"
)

func TestAllowedByModeOAuthDeviceFlow(t *testing.T) {
	if !AllowedByMode(agent.ModeAdvisory, "POST", "/login/device/code") {
		t.Error("ADVISORY should allow POST /login/device/code")
	}
	if !AllowedByMode(agent.ModeAdvisory, "POST", "/login/oauth/access_token") {
		t.Error("ADVISORY should allow POST /login/oauth/access_token")
	}
}

func TestAllowedByModeMerge(t *testing.T) {
	if AllowedByMode(agent.ModeIssuesAndPRs, "PUT", "/repos/org/repo/pulls/123/merge") {
		t.Error("ISSUES_AND_PRS should NOT allow merge")
	}
	// H1 (CWE-863): direct PUT /pulls/{n}/merge is now a HARD DENY for EVERY
	// mode, including merge mode — agents must route through the bound hive-merge
	// relay. Merge mode still authorizes the merge via the relay's authorizer,
	// not via a direct proxy PUT.
	if AllowedByMode(agent.ModeIssuesPRsMerge, "PUT", "/repos/org/repo/pulls/123/merge") {
		t.Error("ISSUES_PRS_MERGE must NOT allow a direct proxy merge (use hive-merge relay)")
	}
}

func TestAllowedByModePROperations(t *testing.T) {
	if AllowedByMode(agent.ModeIssuesOnly, "POST", "/repos/org/repo/pulls") {
		t.Error("ISSUES_ONLY should NOT allow creating PRs")
	}
	// Direct PR creation (POST /pulls) is a HARD DENY for every mode — agents
	// must route through hive-open-pr so the App bot authors the PR. Even a
	// push-capable mode cannot POST /pulls directly.
	if AllowedByMode(agent.ModeIssuesAndPRs, "POST", "/repos/org/repo/pulls") {
		t.Error("POST /pulls must be denied for all modes (use hive-open-pr)")
	}
	if AllowedByMode(agent.ModeIssuesPRsMerge, "POST", "/repos/org/repo/pulls") {
		t.Error("POST /pulls must be denied even for merge mode (use hive-open-pr)")
	}
	if msg, denied := DeniedMessage("POST", "/repos/org/repo/pulls"); !denied || !strings.Contains(msg, "hive-open-pr") {
		t.Errorf("POST /pulls should carry a hive-open-pr deny message; got denied=%v msg=%q", denied, msg)
	}
	// H1 (CWE-863): direct PR merge (PUT /pulls/{n}/merge) is a HARD DENY for
	// every mode — agents route through the bound hive-merge relay, not
	// `gh api -X PUT .../merge`. Even merge mode cannot call it directly.
	if AllowedByMode(agent.ModeIssuesPRsMerge, "PUT", "/repos/org/repo/pulls/42/merge") {
		t.Error("PUT /pulls/{n}/merge must be denied even for merge mode (use hive-merge relay)")
	}
	if msg, denied := DeniedMessage("PUT", "/repos/org/repo/pulls/42/merge"); !denied || !strings.Contains(msg, "hive-merge") {
		t.Errorf("PUT /pulls/{n}/merge should carry a hive-merge deny message; got denied=%v msg=%q", denied, msg)
	}
	// The relay's own branch-sync (PUT /pulls/{n}/update-branch) is NOT denied —
	// only the merge itself is hard-denied.
	if !AllowedByMode(agent.ModeIssuesPRsMerge, "PUT", "/repos/org/repo/pulls/42/update-branch") {
		t.Error("PUT /pulls/{n}/update-branch should still be allowed at merge mode")
	}
	if !AllowedByMode(agent.ModeIssuesAndPRs, "PATCH", "/repos/org/repo/pulls/42") {
		t.Error("ISSUES_AND_PRS should allow patching PRs")
	}
	if !AllowedByMode(agent.ModeIssuesAndPRs, "POST", "/repos/org/repo/pulls/42/reviews") {
		t.Error("ISSUES_AND_PRS should allow posting reviews")
	}
}

func TestAllowedByModeGitOperations(t *testing.T) {
	if !AllowedByMode(agent.ModeAdvisory, "POST", "/org/repo.git/git-upload-pack") {
		t.Error("ADVISORY should allow git fetch (upload-pack)")
	}
	if AllowedByMode(agent.ModeAdvisory, "POST", "/org/repo.git/git-receive-pack") {
		t.Error("ADVISORY should NOT allow git push (receive-pack)")
	}
	if !AllowedByMode(agent.ModeIssuesAndPRs, "POST", "/org/repo.git/git-receive-pack") {
		t.Error("ISSUES_AND_PRS should allow git push")
	}
	if !AllowedByMode(agent.ModeIssuesAndPRs, "POST", "/repos/org/repo/git/refs") {
		t.Error("ISSUES_AND_PRS should allow creating refs")
	}
	if !AllowedByMode(agent.ModeIssuesAndPRs, "POST", "/repos/org/repo/git/commits") {
		t.Error("ISSUES_AND_PRS should allow creating commits")
	}
	if !AllowedByMode(agent.ModeIssuesAndPRs, "DELETE", "/repos/org/repo/git/refs/heads/branch") {
		t.Error("ISSUES_AND_PRS should allow deleting refs")
	}
}

func TestAllowedByModeIssueOperations(t *testing.T) {
	if AllowedByMode(agent.ModeAdvisory, "POST", "/repos/org/repo/issues") {
		t.Error("ADVISORY should NOT allow creating issues")
	}
	if !AllowedByMode(agent.ModeIssuesOnly, "POST", "/repos/org/repo/issues") {
		t.Error("ISSUES_ONLY should allow creating issues")
	}
	if !AllowedByMode(agent.ModeIssuesOnly, "PATCH", "/repos/org/repo/issues/42") {
		t.Error("ISSUES_ONLY should allow patching issues")
	}
	if !AllowedByMode(agent.ModeIssuesOnly, "POST", "/repos/org/repo/issues/42/comments") {
		t.Error("ISSUES_ONLY should allow commenting on issues")
	}
	if !AllowedByMode(agent.ModeIssuesOnly, "POST", "/repos/org/repo/issues/42/labels") {
		t.Error("ISSUES_ONLY should allow adding labels")
	}
}

func TestAllowedByModeReadOperations(t *testing.T) {
	if !AllowedByMode(agent.ModeAdvisory, "GET", "/repos/org/repo") {
		t.Error("ADVISORY should allow GET")
	}
	if !AllowedByMode(agent.ModeAdvisory, "HEAD", "/repos/org/repo") {
		t.Error("ADVISORY should allow HEAD")
	}
	if !AllowedByMode(agent.ModeAdvisory, "OPTIONS", "/repos/org/repo") {
		t.Error("ADVISORY should allow OPTIONS")
	}
}

func TestAllowedByModeUnknownMethod(t *testing.T) {
	if AllowedByMode(agent.ModeIssuesPRsMerge, "TRACE", "/repos/org/repo") {
		t.Error("unknown method TRACE should be denied")
	}
}

func TestGraphQLAllowedQuery(t *testing.T) {
	body := []byte(`{"query":"{ viewer { login } }"}`)
	allowed, isMutation := GraphQLAllowed(agent.ModeAdvisory, body)
	if !allowed {
		t.Error("ADVISORY should allow GraphQL queries")
	}
	if isMutation {
		t.Error("query should not be flagged as mutation")
	}
}

func TestGraphQLAllowedMutation(t *testing.T) {
	body := []byte(`{"query":"mutation { addStar(input: {starrableId: \"abc\"}) { starrable { id } } }"}`)
	allowed, isMutation := GraphQLAllowed(agent.ModeAdvisory, body)
	if allowed {
		t.Error("ADVISORY should NOT allow GraphQL mutations")
	}
	if !isMutation {
		t.Error("should be flagged as mutation")
	}

	allowed2, _ := GraphQLAllowed(agent.ModeIssuesOnly, body)
	if !allowed2 {
		t.Error("ISSUES_ONLY should allow GraphQL mutations")
	}
}

// TestGraphQLAllowedMutationTiers verifies that GraphQL mutations are gated by
// the same capability tier as the equivalent REST route, so an ISSUES_ONLY
// agent cannot bypass merge-gating (or open PRs / push code) via GraphQL.
func TestGraphQLAllowedMutationTiers(t *testing.T) {
	cases := []struct {
		name       string
		query      string
		minAllowed agent.AgentMode // lowest mode that should be allowed
	}{
		{"mergePullRequest needs merge mode", `mutation { mergePullRequest(input: {pullRequestId: "x"}) { pullRequest { id } } }`, agent.ModeIssuesPRsMerge},
		{"enablePullRequestAutoMerge needs merge mode", `mutation { enablePullRequestAutoMerge(input: {pullRequestId: "x"}) { clientMutationId } }`, agent.ModeIssuesPRsMerge},
		{"createPullRequest needs PR mode", `mutation { createPullRequest(input: {}) { pullRequest { id } } }`, agent.ModeIssuesAndPRs},
		{"createCommitOnBranch needs PR mode", `mutation { createCommitOnBranch(input: {}) { commit { oid } } }`, agent.ModeIssuesAndPRs},
		{"updateRef needs PR mode", `mutation { updateRef(input: {}) { clientMutationId } }`, agent.ModeIssuesAndPRs},
		{"addComment stays at issues", `mutation { addComment(input: {}) { clientMutationId } }`, agent.ModeIssuesOnly},
		{"addLabelsToLabelable stays at issues", `mutation { addLabelsToLabelable(input: {}) { clientMutationId } }`, agent.ModeIssuesOnly},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Marshal so the query's inner quotes are escaped into valid JSON.
			body, err := json.Marshal(graphQLRequest{Query: tc.query})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			// Just below the required tier → denied.
			if tc.minAllowed > agent.ModeAdvisory {
				if ok, _ := GraphQLAllowed(tc.minAllowed-1, body); ok {
					t.Errorf("mode %d should be DENIED (needs %d)", tc.minAllowed-1, tc.minAllowed)
				}
			}
			// At the required tier → allowed, and flagged as a mutation.
			ok, isMut := GraphQLAllowed(tc.minAllowed, body)
			if !ok {
				t.Errorf("mode %d should be ALLOWED", tc.minAllowed)
			}
			if !isMut {
				t.Error("should be flagged as a mutation")
			}
		})
	}
}

func TestGraphQLAllowedBelowAdvisory(t *testing.T) {
	body := []byte(`{"query":"{ viewer { login } }"}`)
	allowed, _ := GraphQLAllowed(agent.AgentMode(-1), body)
	if allowed {
		t.Error("mode below ADVISORY should deny everything")
	}
}

func TestGraphQLAllowedInvalidJSON(t *testing.T) {
	allowed, _ := GraphQLAllowed(agent.ModeAdvisory, []byte("not json"))
	if allowed {
		t.Error("invalid JSON should be denied")
	}
}

func TestGraphQLAllowedMultilineMutation(t *testing.T) {
	body := []byte(`{"query":"\nmutation {\n  addComment(input: {}) { id }\n}"}`)
	allowed, isMutation := GraphQLAllowed(agent.ModeAdvisory, body)
	if allowed {
		t.Error("multiline mutation should be denied for ADVISORY")
	}
	if !isMutation {
		t.Error("multiline mutation should be detected")
	}
}

func TestExtractSNIEmpty(t *testing.T) {
	if sni := extractSNI(nil); sni != "" {
		t.Errorf("nil data should return empty, got %q", sni)
	}
	if sni := extractSNI([]byte{1, 2, 3}); sni != "" {
		t.Errorf("too-short data should return empty, got %q", sni)
	}
}

func TestExtractSNIPartialData(t *testing.T) {
	data := make([]byte, 10)
	data[0] = 0x16
	data[3] = 0
	data[4] = 4
	sni := extractSNI(data)
	if sni != "" {
		t.Errorf("partial ClientHello should return empty, got %q", sni)
	}
}

func TestExtractAgentNameFromHeader(t *testing.T) {
	tests := []struct {
		name     string
		authHdr  string
		wantName string
	}{
		{"basic auth with agent name", "Basic " + base64.StdEncoding.EncodeToString([]byte("scanner:token")), "scanner"},
		{"no auth header", "", ""},
		{"bearer token", "Bearer ghp_abc123", ""},
		{"basic with empty user", "Basic " + base64.StdEncoding.EncodeToString([]byte(":token")), ""},
		{"invalid base64", "Basic not-valid-base64!!!", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "https://api.github.com/repos", nil)
			if tt.authHdr != "" {
				req.Header.Set("Proxy-Authorization", tt.authHdr)
			}
			got := extractAgentName(req)
			if got != tt.wantName {
				t.Errorf("extractAgentName() = %q, want %q", got, tt.wantName)
			}
		})
	}
}

func TestReadAgentMode(t *testing.T) {
	tmpDir := t.TempDir()
	origPrefix := modeFilePrefix
	defer func() {
		// restore if needed - but since it's a const, just test via actual file
	}()

	modeFile := filepath.Join(tmpDir, "mode-test-agent")
	os.WriteFile(modeFile, []byte("ISSUES_AND_PRS\n"), 0644)

	got := readAgentMode("nonexistent-agent-xyz")
	if got != agent.ModeAdvisory {
		t.Errorf("missing mode file should default to ADVISORY, got %v", got)
	}
	_ = origPrefix
}

func TestLookupUIDByLocalPortFrom(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "tcp")
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F91 0100007F:0050 01 00000000:00000000 00:00000000 00000000  1001        0 12345
   1: 0100007F:4E21 0100007F:0050 01 00000000:00000000 00:00000000 00000000  1002        0 12346
`
	os.WriteFile(tmpFile, []byte(content), 0644)

	uid, err := lookupUIDByLocalPortFrom(tmpFile, 8081)
	if err != nil {
		t.Fatalf("lookup port 8081 failed: %v", err)
	}
	if uid != 1001 {
		t.Errorf("uid = %d, want 1001", uid)
	}

	uid2, err := lookupUIDByLocalPortFrom(tmpFile, 20001)
	if err != nil {
		t.Fatalf("lookup port 20001 failed: %v", err)
	}
	if uid2 != 1002 {
		t.Errorf("uid = %d, want 1002", uid2)
	}

	_, err = lookupUIDByLocalPortFrom(tmpFile, 9999)
	if err == nil {
		t.Error("expected error for non-existent port")
	}
}

func TestLookupUIDByLocalPortFromEmpty(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "tcp")
	os.WriteFile(tmpFile, []byte(""), 0644)

	_, err := lookupUIDByLocalPortFrom(tmpFile, 8080)
	if err == nil {
		t.Error("expected error for empty file")
	}
}

func TestLookupUIDByLocalPortFromMissing(t *testing.T) {
	_, err := lookupUIDByLocalPortFrom("/nonexistent/path/tcp", 8080)
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestLookupUIDByLocalPortFromMalformed(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "tcp")
	content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid
   0: baddata
   1: short 0100007F:0050
`
	os.WriteFile(tmpFile, []byte(content), 0644)

	_, err := lookupUIDByLocalPortFrom(tmpFile, 8080)
	if err == nil {
		t.Error("expected error for malformed data")
	}
}

func TestPrefixConnRead(t *testing.T) {
	prefix := []byte("hello")
	pc := &prefixConn{prefix: prefix}
	buf := make([]byte, 3)
	n, err := pc.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || string(buf[:n]) != "hel" {
		t.Errorf("first read = %q, want 'hel'", buf[:n])
	}
	n, err = pc.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || string(buf[:n]) != "lo" {
		t.Errorf("second read = %q, want 'lo'", buf[:n])
	}
}

func TestIsGitHubHostComplete(t *testing.T) {
	positives := []string{"api.github.com", "github.com"}
	negatives := []string{"gitlab.com", "bitbucket.org", "github.io", "", "api.github.com.evil.com"}

	for _, h := range positives {
		if !IsGitHubHost(h) {
			t.Errorf("IsGitHubHost(%q) = false, want true", h)
		}
	}
	for _, h := range negatives {
		if IsGitHubHost(h) {
			t.Errorf("IsGitHubHost(%q) = true, want false", h)
		}
	}
}

func TestAllowedByModeEscalation(t *testing.T) {
	// PATCH /pulls/<n> is a mode-gated PR operation (unlike POST /pulls, which is
	// a hard deny for every mode): advisory/issues-only cannot, issues-and-prs and
	// above can. This exercises the mode-escalation path itself.
	path := "/repos/org/repo/pulls/42"
	method := "PATCH"

	modes := []agent.AgentMode{
		agent.ModeAdvisory,
		agent.ModeIssuesOnly,
		agent.ModeIssuesAndPRs,
		agent.ModeIssuesPRsMerge,
	}

	for i, mode := range modes {
		allowed := AllowedByMode(mode, method, path)
		if i < 2 && allowed {
			t.Errorf("mode %v should NOT allow %s %s", mode, method, path)
		}
		if i >= 2 && !allowed {
			t.Errorf("mode %v should allow %s %s", mode, method, path)
		}
	}

	// POST /pulls (direct PR creation) is denied for EVERY mode — no escalation
	// unlocks it. Agents must use hive-open-pr.
	for _, mode := range modes {
		if AllowedByMode(mode, "POST", "/repos/org/repo/pulls") {
			t.Errorf("mode %v must NOT allow direct POST /pulls (use hive-open-pr)", mode)
		}
	}
}
