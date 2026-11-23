package askai

import (
	"os"
	"regexp"
	"testing"
)

// TestSectionVocabularyCoversTheHiveDashboard keeps the closed vocabulary in
// step with the hive dashboard's own section registry: every section that has
// a "?" help mark can be named in a question, and nothing else can.
func TestSectionVocabularyCoversTheHiveDashboard(t *testing.T) {
	data, err := os.ReadFile("../dashboard/static-src/js/sections.js")
	if err != nil {
		t.Fatal(err)
	}
	ids := regexp.MustCompile(`'([a-z0-9-]+-section)': \{ heading: '([^']*)'`).FindAllStringSubmatch(string(data), -1)
	if len(ids) == 0 {
		t.Fatal("no DASHBOARD_SECTION_HELP entries found")
	}
	for _, m := range ids {
		label, ok := SectionLabel(m[1])
		if !ok {
			t.Errorf("dashboard section %q is missing from the Ask vocabulary", m[1])
			continue
		}
		if label != m[2] {
			t.Errorf("section %q label %q, dashboard heading %q", m[1], label, m[2])
		}
	}
}
