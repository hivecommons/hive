package dashboard

import (
	"strings"
	"testing"
)

func TestTrustedAuthorSecurityLink(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`Who holds these roles is defined by Authorized users on the Security tab`,
		`hosted hives manage read, read-write, merger, and owner roles on the hub's Manage Access screen`,
		`data-action="openTrustedAuthorSecurityAccess"`,
		`function openTrustedAuthorSecurityAccess()`,
		`switchConfigTab('Security');`,
		`id="access-users-list" tabindex="-1"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("trusted-author authorized-users helper missing %q", want)
		}
	}
}
