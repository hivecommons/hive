package dashboard

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/knowledge/connector"
)

func TestKnowledgeCardUsesGenericIntegrationsButton(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	buttonRowStart := strings.Index(html, "// Action buttons")
	if buttonRowStart < 0 {
		t.Fatal("knowledge action button row marker not found")
	}
	buttonRowEnd := strings.Index(html[buttonRowStart:], "// Main layout")
	if buttonRowEnd < 0 {
		t.Fatal("knowledge action button row end marker not found")
	}
	row := html[buttonRowStart : buttonRowStart+buttonRowEnd]
	if strings.Contains(row, "Obsidian Setup") || strings.Contains(row, "kbOpenObsidianSetup") {
		t.Fatalf("knowledge action row still contains the Obsidian-specific setup button:\n%s", row)
	}
	for _, want := range []string{
		`data-action="kbOpenIntegrations"`,
		`>🔌 Integrations</button>`,
		`function kbOpenIntegrations()`,
		`data-integration-type="obsidian"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing integrations modal snippet %q", want)
		}
	}
}

func TestKnowledgeIntegrationsModalCoversRegistryConnectorTypes(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	got := parseJSStringArray(t, html, "KB_CONNECTOR_TYPE_ORDER")
	want := connector.DefaultRegistry().Types()
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("KB_CONNECTOR_TYPE_ORDER = %v, want registry types %v", got, want)
	}
	for _, typ := range want {
		for _, snippet := range []string{
			`'` + typ + `':`,
			`data-connector-type="' + escapeHtml(t) + '"`,
		} {
			if !strings.Contains(html, snippet) {
				t.Errorf("index.html missing connector modal snippet %q for %s", snippet, typ)
			}
		}
	}
}

func parseJSStringArray(t *testing.T, html, name string) []string {
	t.Helper()
	re := regexp.MustCompile(`const\s+` + regexp.QuoteMeta(name) + `\s*=\s*\[([^\]]*)\]`)
	m := re.FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("%s declaration not found", name)
	}
	var out []string
	for _, part := range strings.Split(m[1], ",") {
		part = strings.TrimSpace(part)
		part = strings.Trim(part, "'\"")
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// TestKnowledgeIntegrationsModalUsesVendorIcons guards the vendor/project
// glyph mapping: every registry connector type (plus Obsidian) must have an
// inline SVG path in KC_TYPE_ICONS so the sidebar never falls back to the
// generic plug for a known connector.
func TestKnowledgeIntegrationsModalUsesVendorIcons(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)

	start := strings.Index(html, "const KC_TYPE_ICONS = {")
	if start < 0 {
		t.Fatal("KC_TYPE_ICONS declaration not found")
	}
	end := strings.Index(html[start:], "};")
	if end < 0 {
		t.Fatal("KC_TYPE_ICONS declaration not terminated")
	}
	icons := html[start : start+end]
	for _, typ := range append([]string{"obsidian"}, connector.DefaultRegistry().Types()...) {
		if !strings.Contains(icons, `'`+typ+`': 'M`) {
			t.Errorf("KC_TYPE_ICONS missing an SVG path for connector type %q", typ)
		}
	}
	for _, want := range []string{
		`function kbIntegrationIcon(type)`,
		`'<span class="kb-layer-label">' + kbIntegrationIcon(t) + escapeHtml(kbIntegrationLabel(t)) + '</span>'`,
		`class="kb-integration-icon" viewBox="0 0 24 24" aria-hidden="true"`,
		`.kb-integration-icon { width: 16px; height: 16px;`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing vendor icon snippet %q", want)
		}
	}
	if strings.Contains(html, `(t === 'obsidian' ? '🔮 ' : '🔌 ')`) {
		t.Error("integrations sidebar still renders the generic plug emoji per connector")
	}
}

func TestKnowledgeIntegrationsModalContext7Card(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`'context7': [['library_ids'`,
		`data-kb-ctx7-key-state=`,
		`data-setting-key="knowledge.context7.auto_import[].id"`,
		`data-action="kbOpenContext7Integration"`,
		`function kbOpenContext7Integration()`,
		`label: 'Context7 connector'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing Context7 card snippet %q", want)
		}
	}
}

// TestKnowledgeIntegrationsModalPlaudCard guards the Plaud entry: the card
// exposes Connect/Disconnect/Check wired to the connector auth endpoints,
// the Knowledge pane links to it, and no browser-native dialog is used.
func TestKnowledgeIntegrationsModalPlaudCard(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`'plaud': { label: 'Plaud voice notes'`,
		`'plaud': [['command', 'MCP command', 'npx -y @plaud-ai/mcp@0.3.14']`,
		`else if (type === 'plaud') html += kbPlaudAuthLine(c.name, type) + kbPlaudPasteHtml(c.name);`,
		`data-action="kbPlaudPasteComplete"`,
		`'/api/knowledge/connectors/' + encodeURIComponent(name) + '/auth/complete'`,
		`if (KB_AUTH_TYPES[type]) html += kbPlaudAuthButtons(c.name, notLoaded ? disabledAttr : '', type);`,
		`data-action="kbPlaudConnect"`,
		`data-action="kbPlaudDisconnect"`,
		`data-action="kbPlaudRefreshAuth"`,
		`data-action="kbOpenPlaudIntegration"`,
		`'/api/knowledge/connectors/' + encodeURIComponent(name) + '/auth/connect'`,
		`'/api/knowledge/connectors/' + encodeURIComponent(name) + '/auth/disconnect'`,
		`async function kbPlaudConnect(name)`,
		`async function kbPlaudDisconnect(name)`,
		`async function kbPlaudRefreshAuth(name)`,
		`function kbOpenPlaudIntegration()`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing Plaud card snippet %q", want)
		}
	}
	start := strings.Index(html, "const _kbPlaudAuth = {};")
	end := strings.Index(html, "function kbOpenPlaudIntegration()")
	if start < 0 || end < start {
		t.Fatal("Plaud sign-in block not found")
	}
	block := html[start:end]
	for _, native := range []string{"window.confirm(", "window.alert(", " confirm(", " alert(", "prompt("} {
		if strings.Contains(block, native) {
			t.Errorf("Plaud sign-in block uses a browser-native dialog %q", native)
		}
	}
}

// TestKnowledgeIntegrationsModalMeetingCards guards the Zoom and Teams
// entries: they share the Plaud sign-in line and buttons (type-aware
// labels), Zoom offers the pasted-redirect fallback and Teams the
// admin-consent status, both carry the app-registration form whose fields
// reference settings keys, and no browser-native dialog is used.
func TestKnowledgeIntegrationsModalMeetingCards(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`'zoom': { label: 'Zoom recordings'`,
		`'teams': { label: 'Microsoft Teams recordings'`,
		`'zoom': [['users', 'Recording owners (me, or user ids/emails for Server-to-Server)', 'me']`,
		`'teams': [['users', 'Meeting organizers (object ids or UPNs, comma-separated)'`,
		`const KB_AUTH_TYPES = { plaud: 'Plaud', zoom: 'Zoom', teams: 'Teams', discord: 'Discord', 'google-meet': 'Google Meet' };`,
		`else if (type === 'zoom') html += kbPlaudAuthLine(c.name, type) + kbZoomPasteHtml(c.name, c);`,
		`else if (type === 'teams') html += kbPlaudAuthLine(c.name, type) + kbTeamsConsentHtml(c.name, c);`,
		`if (type === 'zoom' || type === 'teams') html += kbMeetingAuthFormHTML(type, c);`,
		`function kbAuthLabel(name, type)`,
		`function kbZoomPasteHtml(name, c)`,
		`function kbTeamsConsentHtml(name, c)`,
		`function kbMeetingAuthFormHTML(type, c)`,
		`function kbOpenZoomIntegration()`,
		`function kbOpenTeamsIntegration()`,
		`data-kb-zoom-paste`,
		`data-kb-teams-consent`,
		`Requires Entra admin consent for OnlineMeetingRecording.Read.All and OnlineMeetingTranscript.Read.All`,
		`client_id_env: 'knowledge.connectors[].auth.client_id_env',`,
		`tenant_id_env: 'knowledge.connectors[].auth.tenant_id_env'`,
		`data-setting-key="' + KB_MEETING_AUTH_SETTING_KEYS[key] + '"`,
		`data-setting-key="knowledge.connectors[].auth.mode"`,
		`box.querySelectorAll('[data-kb-kc-auth-field]')`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing meeting card snippet %q", want)
		}
	}
	start := strings.Index(html, "function kbZoomPasteHtml(name, c)")
	end := strings.Index(html, "async function kbPlaudAuthLoad(name, refresh)")
	if start < 0 || end < start {
		t.Fatal("meeting card block not found")
	}
	block := html[start:end]
	for _, native := range []string{"window.confirm(", "window.alert(", " confirm(", " alert(", "prompt("} {
		if strings.Contains(block, native) {
			t.Errorf("meeting card block uses a browser-native dialog %q", native)
		}
	}
}

// TestKnowledgeIntegrationsModalDiscordCard guards the Discord entry: it
// shares the sign-in line and buttons (Connect probes the bot token), the
// card explains the bot setup and offers the invite link when the bot lacks
// access, the form takes the bot token by environment variable name, and no
// browser-native dialog is used.
func TestKnowledgeIntegrationsModalDiscordCard(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`'discord': { label: 'Discord meetings'`,
		`'discord': [['guild', 'Server (guild) id, for scheduled events'`,
		`else if (type === 'discord') html += kbPlaudAuthLine(c.name, type) + kbDiscordHelpHtml(c.name, c);`,
		`function kbDiscordHelpHtml(name, c)`,
		`function kbOpenDiscordIntegration()`,
		`data-kb-discord-access`,
		`data-kb-discord-help`,
		`data-kb-discord-note`,
		`type === 'discord' ? ' bot invite' : ' authorization'`,
		`discord: '🎧'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing Discord card snippet %q", want)
		}
	}
	start := strings.Index(html, "function kbDiscordHelpHtml(name, c)")
	end := strings.Index(html, "async function kbPlaudAuthLoad(name, refresh)")
	if start < 0 || end < start {
		t.Fatal("Discord card block not found")
	}
	block := html[start:end]
	for _, native := range []string{"window.confirm(", "window.alert(", " confirm(", " alert(", "prompt("} {
		if strings.Contains(block, native) {
			t.Errorf("Discord card block uses a browser-native dialog %q", native)
		}
	}
}

// TestKnowledgeIntegrationsModalGoogleMeetCard guards the Google Meet entry:
// it shares the sign-in line and buttons, offers the pasted-redirect
// fallback (user-level) or the domain-wide delegation note (service
// account), carries the app-registration form, and uses no browser-native
// dialog.
func TestKnowledgeIntegrationsModalGoogleMeetCard(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`'google-meet': { label: 'Google Meet recordings'`,
		`'google-meet': [['users', 'Users to read (service account only: Workspace addresses, comma-separated)'`,
		`'github-wiki', 'google-drive', 'google-meet', 'notion',`,
		`else if (type === 'google-meet') html += kbPlaudAuthLine(c.name, type) + kbGoogleMeetPasteHtml(c.name, c);`,
		`if (type === 'google-meet') html += kbMeetingAuthFormHTML(type, c);`,
		`} else if (type === 'google-meet') {`,
		`function kbGoogleMeetPasteHtml(name, c)`,
		`function kbOpenGoogleMeetIntegration()`,
		`data-kb-google-meet-paste`,
		`data-kb-google-meet-sa`,
		`data-kb-google-meet-note`,
		`'google-meet': '🎥'`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing Google Meet card snippet %q", want)
		}
	}
	start := strings.Index(html, "function kbGoogleMeetPasteHtml(name, c)")
	end := strings.Index(html, "async function kbPlaudAuthLoad(name, refresh)")
	if start < 0 || end < start {
		t.Fatal("Google Meet card block not found")
	}
	block := html[start:end]
	for _, native := range []string{"window.confirm(", "window.alert(", " confirm(", " alert(", "prompt("} {
		if strings.Contains(block, native) {
			t.Errorf("Google Meet card block uses a browser-native dialog %q", native)
		}
	}
}

// TestKnowledgeIntegrationsModalWisprFlowCard guards the Wispr Flow entry:
// it is a plain local-file connector (no sign-in buttons), the card shows
// the configured path, the Knowledge pane links to it and documents the
// direct-dictation path, and no browser-native dialog is used.
func TestKnowledgeIntegrationsModalWisprFlowCard(t *testing.T) {
	b, err := staticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("reading embedded static/index.html: %v", err)
	}
	html := string(b)
	for _, want := range []string{
		`'wisprflow': { label: 'Wispr Flow dictation'`,
		`'wisprflow': [['path', 'flow.sqlite or export folder (empty = app default)'`,
		`['group', 'Fact per dictation or day', 'dictation']`,
		`if (type === 'wisprflow') html += kbWisprFlowSourceLine(c);`,
		`data-kb-wisprflow-note`,
		`data-action="kbOpenWisprFlowIntegration"`,
		`function kbWisprFlowSourceLine(c)`,
		`function kbOpenWisprFlowIntegration()`,
		`Wispr Flow dictation</summary>`,
		`+ New Fact</strong> and hold the Flow key`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing Wispr Flow card snippet %q", want)
		}
	}
	if !strings.Contains(html, `'sharepoint', 'teams', 'wisprflow', 'zoom']`) {
		t.Error("wisprflow missing from KB_CONNECTOR_TYPE_ORDER")
	}
	start := strings.Index(html, "function kbWisprFlowSourceLine(c)")
	end := strings.Index(html, "function kbOpenWisprFlowIntegration()")
	if start < 0 || end < start {
		t.Fatal("Wispr Flow block not found")
	}
	block := html[start:end]
	for _, native := range []string{"window.confirm(", "window.alert(", " confirm(", " alert(", "prompt("} {
		if strings.Contains(block, native) {
			t.Errorf("Wispr Flow block uses a browser-native dialog %q", native)
		}
	}
}
