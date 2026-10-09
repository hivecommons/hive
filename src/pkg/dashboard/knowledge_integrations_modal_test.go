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
