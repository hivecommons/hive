package compliance

import (
	"fmt"
	"strings"
)

// Doc markers delimiting a generated mapping table in docs/compliance.md.
// src/scripts/render-compliance-tables.py writes the same bytes RenderTable
// returns; TestComplianceDocTablesMatchProfiles keeps them in lock-step.
const (
	tableBeginFmt = "<!-- BEGIN GENERATED: compliance-profile %s -->"
	tableEndFmt   = "<!-- END GENERATED: compliance-profile %s -->"
)

// TableMarkers returns the begin/end comment lines for profile id.
func TableMarkers(id string) (begin, end string) {
	return fmt.Sprintf(tableBeginFmt, id), fmt.Sprintf(tableEndFmt, id)
}

// RenderTable renders a profile's control mapping as a Markdown table: one
// row per mapping, one row for each not-covered control.
func RenderTable(p Profile) string {
	var b strings.Builder
	b.WriteString("| Control | Domain | Hive setting | Recommended | Evaluator | Notes |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, c := range p.Controls {
		control := mdCell(c.ID + " — " + c.Title)
		domain := mdCell(c.Domain)
		notes := mdCell(c.Rationale)
		if c.NotCovered {
			fmt.Fprintf(&b, "| %s | %s | _not covered by Hive_ | — | — | %s |\n", control, domain, notes)
			continue
		}
		for i, m := range c.Mappings {
			n := ""
			if i == 0 {
				n = notes
			}
			fmt.Fprintf(&b, "| %s | %s | `%s` | `%s` | `%s` | %s |\n", control, domain, m.SettingPath, m.Recommended, m.Evaluator, n)
		}
	}
	return b.String()
}

func mdCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", `\|`)
}
