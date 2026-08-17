// Package agentsmd reads a maintained repository's AGENTS.md file (the emerging
// cross-tool convention for per-repo agent instructions) and a lightweight
// "skills" mechanism, turning both into text that Hive prepends to an agent's
// kick prompt.
//
// # AGENTS.md format
//
// AGENTS.md is a plain Markdown file at the repository root. Hive supports two
// additive extensions on top of ordinary Markdown:
//
//  1. Optional YAML front-matter: a block delimited by a line containing only
//     "---" at the very top of the file, closed by a second "---" line. The only
//     key Hive reads is an optional "skills:" list naming skills to request into
//     the agent's context by default. Everything else in the block is ignored.
//     Example:
//
//     ---
//     skills:
//       - go-testing
//       - pr-etiquette
//     ---
//
//  2. Inline skill sections: any section whose heading is "## Skill: <name>"
//     defines a reusable, named instruction snippet. The section body runs from
//     the heading to the next "## " heading (or end of file). Skills may also be
//     defined as standalone files at "skills/<name>.md" relative to the repo
//     root; when both a file and an inline section define the same name, the
//     inline section wins.
//
// The "body" is the Markdown document with the front-matter block and all
// "## Skill:" sections removed — the general instructions that always apply.
//
// # Nested AGENTS.md (closest-wins)
//
// A repository may carry per-directory AGENTS.md files. When resolving
// instructions for a file at a relative path, ParseNearest walks from that
// file's directory up to the repo root and uses the closest AGENTS.md it finds.
// The root AGENTS.md is the required baseline; nested files override it for
// their subtree.
//
// # Tolerance
//
// Parsing never fails a kick. A missing AGENTS.md yields an empty config and a
// nil error. Malformed front-matter is logged and skipped (the body is still
// used). Callers may safely ignore the returned error for injection purposes.
package agentsmd

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// AgentsFileName is the conventional filename Hive looks for.
	AgentsFileName = "AGENTS.md"

	// SkillsDirName is the directory holding standalone "<name>.md" skill files.
	SkillsDirName = "skills"

	// SkillFileExt is the extension for standalone skill files.
	SkillFileExt = ".md"

	// frontMatterDelim is the line (trimmed) that opens and closes the optional
	// YAML front-matter block.
	frontMatterDelim = "---"

	// skillHeadingPrefix marks an inline skill section. A heading of the form
	// "## Skill: <name>" begins a named skill snippet.
	skillHeadingPrefix = "## Skill:"

	// sectionHeadingPrefix is any level-2 Markdown heading; it terminates a
	// skill section.
	sectionHeadingPrefix = "## "

	// injectionHeader titles the injected AGENTS.md block in the kick prompt,
	// mirroring the "# Relevant Knowledge" convention used by the Primer.
	injectionHeader = "# Repository Agent Instructions (AGENTS.md)"

	// injectionSkillsHeader titles the resolved-skills subsection.
	injectionSkillsHeader = "## Requested Skills"
)

// AgentsConfig is the parsed contents of an AGENTS.md file (and any adjacent
// skills/ directory).
type AgentsConfig struct {
	// Body is the Markdown document with front-matter and "## Skill:" sections
	// removed — the general instructions that always apply.
	Body string

	// Skills maps a skill name to its instruction text.
	Skills map[string]string

	// RequestedSkills lists skill names named in the front-matter "skills:" key,
	// in file order. These are the skills the repo asks Hive to include by
	// default.
	RequestedSkills []string
}

// frontMatter is the subset of the YAML front-matter block Hive reads.
type frontMatter struct {
	Skills []string `yaml:"skills"`
}

// logger returns lg or a discarding default so callers may pass nil.
func logger(lg *slog.Logger) *slog.Logger {
	if lg != nil {
		return lg
	}
	return slog.New(slog.DiscardHandler)
}

// Parse finds and parses the root AGENTS.md under repoRoot. It is tolerant:
// a missing file yields an empty (non-nil) config and a nil error; malformed
// front-matter is logged and skipped rather than failing.
func Parse(repoRoot string, lg *slog.Logger) (*AgentsConfig, error) {
	return parseAt(repoRoot, filepath.Join(repoRoot, AgentsFileName), logger(lg))
}

// ParseNearest resolves the closest-wins AGENTS.md for a file at relPath
// (relative to repoRoot). It walks from relPath's directory up to repoRoot and
// parses the first AGENTS.md it finds; if none exist in the subtree, it falls
// back to the root AGENTS.md. Behaves tolerantly like Parse.
func ParseNearest(repoRoot, relPath string, lg *slog.Logger) (*AgentsConfig, error) {
	lg = logger(lg)

	// Normalize and confine relPath to the repo. A path escaping the root falls
	// back to the root file.
	clean := filepath.Clean(relPath)
	dir := filepath.Dir(filepath.Join(repoRoot, clean))

	rootAbs, err := filepath.Abs(repoRoot)
	if err != nil {
		rootAbs = repoRoot
	}

	for {
		candidate := filepath.Join(dir, AgentsFileName)
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return parseAt(dir, candidate, lg)
		}

		dirAbs, absErr := filepath.Abs(dir)
		if absErr != nil {
			break
		}
		if dirAbs == rootAbs || !strings.HasPrefix(dirAbs, rootAbs) {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// Fall back to the root baseline.
	return Parse(repoRoot, lg)
}

// parseAt parses the AGENTS.md at agentsPath, treating baseDir as the directory
// whose skills/ subdirectory supplies standalone skill files.
func parseAt(baseDir, agentsPath string, lg *slog.Logger) (*AgentsConfig, error) {
	cfg := &AgentsConfig{Skills: map[string]string{}}

	raw, err := os.ReadFile(agentsPath)
	if err != nil {
		if os.IsNotExist(err) {
			// Missing file is not an error — empty config.
			return cfg, nil
		}
		// Unreadable file: tolerate, log, return empty config.
		lg.Warn("agentsmd: cannot read AGENTS.md, skipping", "path", agentsPath, "error", err)
		return cfg, nil
	}

	content := string(raw)

	// 1. Strip and parse optional front-matter.
	fm, rest := splitFrontMatter(content)
	if fm != "" {
		var meta frontMatter
		if yErr := yaml.Unmarshal([]byte(fm), &meta); yErr != nil {
			// Malformed front-matter: log and skip, keep the body.
			lg.Warn("agentsmd: malformed front-matter, ignoring", "path", agentsPath, "error", yErr)
		} else {
			for _, s := range meta.Skills {
				if s = strings.TrimSpace(s); s != "" {
					cfg.RequestedSkills = append(cfg.RequestedSkills, s)
				}
			}
		}
	}

	// 2. Extract inline "## Skill: <name>" sections; the remainder is the body.
	body, inline := extractSkillSections(rest)
	cfg.Body = strings.TrimSpace(body)

	// 3. Load standalone skills/<name>.md files first, then let inline sections
	//    override them (inline wins on name collision).
	loadSkillFiles(baseDir, cfg.Skills, lg)
	for name, text := range inline {
		cfg.Skills[name] = text
	}

	return cfg, nil
}

// splitFrontMatter returns the YAML front-matter (without delimiters) and the
// remaining document. If no front-matter block is present, front-matter is ""
// and the whole content is returned as rest.
func splitFrontMatter(content string) (fm, rest string) {
	// Front-matter must be at the very top (after an optional BOM/leading
	// whitespace-free start). Tolerate a leading newline only.
	trimmed := strings.TrimLeft(content, "\r\n")
	lines := strings.Split(trimmed, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != frontMatterDelim {
		return "", content
	}

	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == frontMatterDelim {
			fm = strings.Join(lines[1:i], "\n")
			rest = strings.Join(lines[i+1:], "\n")
			return fm, rest
		}
	}

	// Opening delimiter with no close: not valid front-matter, treat as body.
	return "", content
}

// extractSkillSections splits a Markdown body into (body-without-skills, skills).
// A skill section starts at a "## Skill: <name>" heading and runs until the next
// "## " heading or end of document.
func extractSkillSections(body string) (string, map[string]string) {
	skills := map[string]string{}
	lines := strings.Split(body, "\n")

	var bodyLines []string
	var curName string
	var curLines []string

	flush := func() {
		if curName != "" {
			skills[curName] = strings.TrimSpace(strings.Join(curLines, "\n"))
		}
		curName = ""
		curLines = nil
	}

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, skillHeadingPrefix) {
			flush()
			curName = strings.TrimSpace(strings.TrimPrefix(trimmed, skillHeadingPrefix))
			continue
		}
		if curName != "" && strings.HasPrefix(line, sectionHeadingPrefix) {
			// A new non-skill section ends the current skill.
			flush()
			bodyLines = append(bodyLines, line)
			continue
		}
		if curName != "" {
			curLines = append(curLines, line)
			continue
		}
		bodyLines = append(bodyLines, line)
	}
	flush()

	return strings.Join(bodyLines, "\n"), skills
}

// loadSkillFiles reads standalone "<name>.md" files from baseDir/skills into
// dst. Unreadable files are skipped tolerantly.
func loadSkillFiles(baseDir string, dst map[string]string, lg *slog.Logger) {
	skillsDir := filepath.Join(baseDir, SkillsDirName)
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		// No skills dir (or unreadable) — nothing to load.
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, SkillFileExt) {
			continue
		}
		text, rErr := os.ReadFile(filepath.Join(skillsDir, name))
		if rErr != nil {
			lg.Warn("agentsmd: cannot read skill file, skipping", "file", name, "error", rErr)
			continue
		}
		skillName := strings.TrimSuffix(name, SkillFileExt)
		dst[skillName] = strings.TrimSpace(string(text))
	}
}

// Resolve concatenates the instruction text of the named skills, in the order
// requested. Unknown names are silently skipped. Returns "" when nothing
// resolves.
func (c *AgentsConfig) Resolve(names []string) string {
	if c == nil || len(names) == 0 {
		return ""
	}
	var parts []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if text, ok := c.Skills[n]; ok && strings.TrimSpace(text) != "" {
			parts = append(parts, "### "+n+"\n\n"+strings.TrimSpace(text))
		}
	}
	return strings.Join(parts, "\n\n")
}

// InjectionText renders the config's body plus the resolved skills as a Markdown
// block ready to prepend to an agent kick. requestedSkills are the skills the
// caller wants included; when nil, the config's own RequestedSkills (from
// front-matter) are used. Returns "" when there is nothing to inject.
func (c *AgentsConfig) InjectionText(requestedSkills []string) string {
	if c == nil {
		return ""
	}

	skills := requestedSkills
	if skills == nil {
		skills = c.RequestedSkills
	}
	resolved := c.Resolve(skills)

	if strings.TrimSpace(c.Body) == "" && resolved == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString(injectionHeader)
	b.WriteString("\n\n")
	if body := strings.TrimSpace(c.Body); body != "" {
		b.WriteString(body)
		b.WriteString("\n")
	}
	if resolved != "" {
		if strings.TrimSpace(c.Body) != "" {
			b.WriteString("\n")
		}
		b.WriteString(injectionSkillsHeader)
		b.WriteString("\n\n")
		b.WriteString(resolved)
		b.WriteString("\n")
	}
	return b.String()
}
