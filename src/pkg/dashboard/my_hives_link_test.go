package dashboard

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestMyHivesURL(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		hubURL  string
		want    string
	}{
		{"hub-linked spoke", true, "https://hive.hivecommons.dev", "https://hive.hivecommons.dev" + hubMyHivesPath},
		{"trailing slash and spaces trimmed", true, "  https://hub.example.com/  ", "https://hub.example.com" + hubMyHivesPath},
		{"plain http hub (dev)", true, "http://localhost:3001", "http://localhost:3001" + hubMyHivesPath},
		// Standalone: config defaults fill Hub.URL with the public hub even
		// with no hub link, so the URL alone must not light up the entry.
		{"standalone with defaulted hub url", false, "https://hive.hivecommons.dev", ""},
		{"enabled but empty url", true, "", ""},
		{"javascript scheme refused", true, "javascript:alert(1)", ""},
		{"relative url refused", true, "/dashboard", ""},
		{"websocket scheme refused", true, "wss://hub.example.com", ""},
		{"unparseable url refused", true, "https://%zz", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Hub.Enabled = tc.enabled
			cfg.Hub.URL = tc.hubURL
			if got := myHivesURL(cfg); got != tc.want {
				t.Fatalf("myHivesURL(enabled=%v, url=%q) = %q, want %q", tc.enabled, tc.hubURL, got, tc.want)
			}
		})
	}
	if got := myHivesURL(nil); got != "" {
		t.Fatalf("myHivesURL(nil) = %q, want empty", got)
	}
}

// TestMyHivesPathIsHubMyHivesPage pins the target to the hub route that serves
// the "My Hives" page, so a hub route rename cannot silently strand the link.
func TestMyHivesPathIsHubMyHivesPage(t *testing.T) {
	if hubMyHivesPath != "/dashboard" {
		t.Fatalf("hubMyHivesPath = %q, want the hub My Hives route /dashboard", hubMyHivesPath)
	}
}

func TestConfigEndpointExposesMyHivesURL(t *testing.T) {
	decode := func(t *testing.T, s *Server) string {
		t.Helper()
		rec := doOwnerGet(s, "/api/config")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/config: expected 200, got %d", rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		raw, ok := body["my_hives_url"]
		if !ok {
			t.Fatal("/api/config has no my_hives_url key")
		}
		v, ok := raw.(string)
		if !ok {
			t.Fatalf("my_hives_url is %T, want string", raw)
		}
		return v
	}

	t.Run("hub-linked", func(t *testing.T) {
		s := covApiServer(t)
		s.deps.Config.Hub.Enabled = true
		s.deps.Config.Hub.URL = "https://hub.example.com/"
		if got, want := decode(t, s), "https://hub.example.com"+hubMyHivesPath; got != want {
			t.Fatalf("my_hives_url = %q, want %q", got, want)
		}
	})
	t.Run("standalone", func(t *testing.T) {
		s := covApiServer(t)
		s.deps.Config.Hub.Enabled = false
		s.deps.Config.Hub.URL = "https://hub.example.com"
		if got := decode(t, s); got != "" {
			t.Fatalf("standalone my_hives_url = %q, want empty", got)
		}
	})
}

func TestAvatarMenuMyHivesItemMarkup(t *testing.T) {
	html := indexHTML(t)
	const item = `<a href="#" id="oc-gh-menu-my-hives" class="avatar-menu-item" target="_blank" rel="noopener noreferrer" hidden>My hives ↗</a>`
	idx := strings.Index(html, item)
	if idx < 0 {
		t.Fatal("user menu has no hidden-by-default \"My hives\" avatar-menu-item")
	}
	profile := strings.Index(html, `id="oc-gh-menu-profile"`)
	docs := strings.Index(html, `<a href="/api/docs" target="_blank" class="avatar-menu-item">API Docs ↗</a>`)
	if profile < 0 || docs < 0 || !(profile < idx && idx < docs) {
		t.Fatalf("\"My hives\" must sit between GitHub profile and API Docs (profile=%d item=%d docs=%d)", profile, idx, docs)
	}
	if !strings.Contains(html, "window._myHivesURL = cfg.my_hives_url || '';") {
		t.Error("boot /api/config fetch no longer reads my_hives_url")
	}
}

// TestRenderMyHivesMenuItemBehaviour executes renderMyHivesMenuItem() from
// index.html: it must show the entry pointing at the hub page when a URL is
// known, and hide it (with no href) on a standalone hive or a non-http URL.
func TestRenderMyHivesMenuItemBehaviour(t *testing.T) {
	html := indexHTML(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: renderMyHivesMenuItem was not executed")
	}
	var script strings.Builder
	script.WriteString(`
const assert = require('node:assert/strict');
function makeEl() {
  return { hidden: true, href: '#', removeAttribute(n) { if (n === 'href') this.href = ''; } };
}
let el = makeEl();
const window = {};
const document = { getElementById: id => id === 'oc-gh-menu-my-hives' ? el : null };
`)
	script.WriteString(jsFunc(t, html, "renderMyHivesMenuItem"))
	script.WriteString(`
window._myHivesURL = 'https://hub.example.com/dashboard';
renderMyHivesMenuItem();
assert.equal(el.hidden, false);
assert.equal(el.href, 'https://hub.example.com/dashboard');

window._myHivesURL = '';
renderMyHivesMenuItem();
assert.equal(el.hidden, true, 'standalone hive must hide the entry');
assert.equal(el.href, '');

el = makeEl();
window._myHivesURL = 'javascript:alert(1)';
renderMyHivesMenuItem();
assert.equal(el.hidden, true, 'non-http URL must hide the entry');
assert.equal(el.href, '');
`)
	if out, err := exec.Command(node, "-e", script.String()).CombinedOutput(); err != nil {
		t.Fatalf("renderMyHivesMenuItem check failed: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
