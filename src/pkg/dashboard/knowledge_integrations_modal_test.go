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
		`'plaud': [['command', 'MCP command', 'npx -y @plaud-ai/mcp@latest']`,
		`if (type === 'plaud') html += kbPlaudAuthLine(c.name) + kbPlaudPasteHtml(c.name);`,
		`data-action="kbPlaudPasteComplete"`,
		`'/api/knowledge/connectors/' + encodeURIComponent(name) + '/auth/complete'`,
		`if (type === 'plaud') html += kbPlaudAuthButtons(c.name);`,
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
	if !strings.Contains(html, `'sharepoint', 'wisprflow']`) {
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
