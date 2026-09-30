package config

import (
	"os"
	"strings"
	"testing"
)

// TestBackendSupportTiersAdvisorEntryCoversEveryBackend pins the design
// requirement (#9638, #9725) that the backend support documentation carries
// an advisor entry for every backend hive knows, and that no row contradicts
// the code: a backend the advisor is active on is never documented "No", and
// one documented "Yes" is active. "Planned" rows are free to land either way.
func TestBackendSupportTiersAdvisorEntryCoversEveryBackend(t *testing.T) {
	raw, err := os.ReadFile("../../docs/backend-support-tiers.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	start := strings.Index(doc, "## Advisor lane")
	if start < 0 {
		t.Fatal("backend-support-tiers.md has no \"## Advisor lane\" section")
	}
	section := doc[start+len("## Advisor lane"):]
	if end := strings.Index(section, "\n## "); end >= 0 {
		section = section[:end]
	}
	rows := map[string]string{}
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		rows[strings.Trim(strings.TrimSpace(cells[1]), "`")] = strings.TrimSpace(cells[2])
	}
	for _, backend := range SupportedBackends() {
		support, ok := rows[backend]
		if !ok {
			t.Errorf("advisor entry missing a row for backend %q", backend)
			continue
		}
		if AdvisorSupportedBackend(backend) && strings.HasPrefix(support, "No") {
			t.Errorf("backend %q is advisor-supported in code but its doc row says %q", backend, support)
		}
		if !AdvisorSupportedBackend(backend) && strings.HasPrefix(support, "Yes") {
			t.Errorf("backend %q is documented as advisor-supported but the advisor is not active on it", backend)
		}
	}
}
