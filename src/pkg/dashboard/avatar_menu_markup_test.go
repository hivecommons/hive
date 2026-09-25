package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

func TestAvatarMenuMarkupNodeGuard(t *testing.T) {
	html := indexHTML(t)
	script := `
const fs = require('fs');
const html = fs.readFileSync(0, 'utf8');
function fail(msg) { console.error(msg); process.exit(1); }
const menu = html.match(/<div id="oc-gh-user-menu"[\s\S]*?<\/div>\s*<\/div>\s*<button class="hv-btn btn-primary gh-auth-btn/);
if (!menu) fail('avatar menu markup not found');
const block = menu[0];
for (const id of ['oc-gh-menu-profile','oc-gh-menu-download','oc-gh-menu-config-export','oc-gh-menu-backup']) {
  const tag = block.match(new RegExp('<(?:a|button)[^>]*id="' + id + '"[^>]*>'));
  if (!tag) fail('missing menu item ' + id);
  if (!/\bclass="[^"]*\bavatar-menu-item\b/.test(tag[0])) fail(id + ' does not use avatar-menu-item');
}
if (!/<a href="\/api\/docs"[^>]*class="avatar-menu-item"/.test(block)) fail('API docs item does not use avatar-menu-item');
if (!/<button class="avatar-menu-item avatar-menu-signout"[^>]*>Sign out<\/button>/.test(block)) fail('sign out item does not use avatar-menu-item');
if (!/<span class="avatar-menu-caption"[^>]*>Encrypted · needs backup key<\/span>/.test(block)) fail('short backup caption missing');
if (!/id="oc-gh-menu-config-export"[^>]*hidden/.test(block)) fail('config export item is not hidden by default');
if (!html.includes("const isOwner = role === 'owner';")) fail('showGHAuthState no longer computes owner role');
if (!html.includes("exportEl.hidden = !isOwner;")) fail('config export item is not owner-only');
`
	cmd := exec.Command("node", "-e", script)
	cmd.Stdin = strings.NewReader(html)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(err.Error(), "executable file not found") {
			t.Skip("node is not installed")
		}
		t.Fatalf("node avatar menu guard failed: %v\n%s", err, string(out))
	}
}
