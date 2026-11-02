// Package frontmatter splits an optional leading YAML front-matter block from
// a Markdown document.
package frontmatter

import "strings"

const delim = "---"

// Split returns the YAML front-matter (without delimiters) and the remaining
// document. If no front-matter block is present, front-matter is "" and the
// whole content is returned as rest.
func Split(content string) (fm, rest string) {
	// Front-matter must be at the very top; tolerate a leading newline only.
	trimmed := strings.TrimLeft(content, "\r\n")
	lines := strings.Split(trimmed, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != delim {
		return "", content
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == delim {
			return strings.Join(lines[1:i], "\n"), strings.Join(lines[i+1:], "\n")
		}
	}
	// Opening delimiter with no close: not valid front-matter, treat as body.
	return "", content
}
