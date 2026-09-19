package knowledge

import "strings"

// sanitizeFrontmatterValue makes a string safe to embed as a single scalar
// value in the hand-rolled YAML frontmatter written by the vault fact
// writers. parseObsidianFile terminates frontmatter at the FIRST "\n---" and
// matches keys by line prefix, so any embedded newline lets the value inject
// forged frontmatter lines (e.g. "confidence: 0.99") or truncate the block,
// discarding the legitimate metadata written after it (issue #7688).
//
// Carriage returns are stripped and newlines collapsed to a single space;
// the result is always a single line.
func sanitizeFrontmatterValue(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return strings.TrimSpace(s)
	}
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// sanitizeFrontmatterList sanitizes each entry and joins them for embedding
// inside a "[a, b, c]" inline frontmatter list. Commas inside an entry are
// replaced so one entry cannot smuggle additional list items.
func sanitizeFrontmatterList(items []string) string {
	cleaned := make([]string, 0, len(items))
	for _, it := range items {
		v := strings.ReplaceAll(sanitizeFrontmatterValue(it), ",", " ")
		v = strings.TrimSpace(v)
		if v != "" {
			cleaned = append(cleaned, v)
		}
	}
	return strings.Join(cleaned, ", ")
}
