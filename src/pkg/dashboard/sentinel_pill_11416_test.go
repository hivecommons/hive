package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

// TestHandleRoleExposesSentinelLabel: /api/role tells every role the
// configured sentinel alert label so Projects cards can draw flagged PRs red.
func TestHandleRoleExposesSentinelLabel(t *testing.T) {
	cases := []struct {
		name, configured, want string
	}{
		{"default", "", "sentinel-alert"},
		{"custom", "  security-hold ", "security-hold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFullServer(t)
			srv.deps.Config.Sentinel.Label = tc.configured
			req := httptest.NewRequest("GET", "/api/role", nil)
			req.Header.Set("X-Hive-Role", "read")
			req.Header.Set("X-Hive-User", "reader")
			w := httptest.NewRecorder()
			srv.handleRole(w, req)

			var result map[string]string
			if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if result["role"] != "read" {
				t.Fatalf("role = %q, want read", result["role"])
			}
			if result["sentinel_label"] != tc.want {
				t.Errorf("sentinel_label = %q, want %q", result["sentinel_label"], tc.want)
			}
		})
	}
}

func TestSentinelLabelNilSafe(t *testing.T) {
	var s *Server
	if got := s.sentinelLabel(); got != "sentinel-alert" {
		t.Errorf("nil server sentinelLabel = %q, want sentinel-alert", got)
	}
}

// TestSentinelFlaggedPRPillWiring pins the red sentinel tint on Projects PR
// pills and its pill-legend entry (#11416).
func TestSentinelFlaggedPRPillWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		".repo-pr-pill.sentinel-flagged { --component-status: var(--status-error); --pill-c: var(--status-error); }",
		"if (roleData.sentinel_label) window._hiveSentinelLabel = roleData.sentinel_label;",
		"const sentinelFlagged = prSentinelFlagged(p);",
		"const tintClass = sentinelFlagged ? ' sentinel-flagged' : (needsHuman ? ' needs-human' : mergeClass);",
		"if (prSentinelFlagged(pr || {})) kinds.push('pr:sentinel');",
		"{ cls: 'repo-pr-pill sentinel-flagged', kind: 'pr:sentinel'",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("sentinel pill wiring missing %q", want)
		}
	}
	// The role read must not depend on a signed-in user.
	roleRead := strings.Index(html, "if (roleData.sentinel_label)")
	userGate := strings.Index(html, "if (roleData.user) {\n          if (roleData.automerge_label)")
	if roleRead < 0 || userGate < 0 || roleRead > userGate {
		t.Errorf("sentinel label must be read before the roleData.user gate")
	}
	// The CSS rule must follow the held rules so red beats the grey hold tint.
	if strings.Index(html, ".repo-pr-pill.sentinel-flagged {") < strings.Index(html, ".repo-issue-pill.held, .repo-pr-pill.held { --component-status") {
		t.Errorf("sentinel-flagged CSS rule must come after the held rule")
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: prSentinelFlagged was not executed")
	}
	var script strings.Builder
	script.WriteString(`
const assert = require('node:assert/strict');
const window = {};
`)
	script.WriteString(jsFunc(t, html, "prLabelSet"))
	script.WriteByte('\n')
	script.WriteString(jsFunc(t, html, "prSentinelFlagged"))
	script.WriteString(`
const flagged = { labels: ['bug', 'Sentinel-Alert'] };
const custom = { labels: ['security-hold'] };
assert.equal(prSentinelFlagged(flagged), false, 'no row is red before the label name is known');
window._hiveSentinelLabel = 'sentinel-alert';
assert.equal(prSentinelFlagged(flagged), true);
assert.equal(prSentinelFlagged({ labels: ['bug'] }), false);
assert.equal(prSentinelFlagged({}), false);
window._hiveSentinelLabel = 'Security-Hold';
assert.equal(prSentinelFlagged(custom), true);
assert.equal(prSentinelFlagged(flagged), false, 'only the configured name counts');
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("prSentinelFlagged check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
