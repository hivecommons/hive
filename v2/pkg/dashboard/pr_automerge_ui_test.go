package dashboard

import (
	"strings"
	"testing"
)

func TestPRAutoMergeButtonGatingPinned(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		"function dashboardRoleAtLeast(role, tier)",
		"dashboardRoleAtLeast(window._hiveRole || 'read', 'merger')",
		"p.author.toLowerCase() === window._hiveUser.toLowerCase()",
		"Queue auto-merge",
		"/queue-automerge",
		"hive/automerge",
	} {
		if !strings.Contains(html, snippet) {
			t.Fatalf("dashboard PR auto-merge UI missing snippet %q", snippet)
		}
	}
}
