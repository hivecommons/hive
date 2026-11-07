// Package climemory turns the persistent memory the agent CLIs keep for
// themselves (Claude Code auto-memory, Codex memories, Gemini's saved
// GEMINI.md) into draft knowledge entries in a Hive vault, so a CLI's
// private inferences become reviewable, scoped facts instead of a hidden
// per-agent notebook (hivecommons/hive#17033).
//
// The package is split in two: Parse* functions that split one memory file
// into facts, and Drainer, which hashes those facts, deduplicates them
// against the vault and writes the new ones as draft entries.
package climemory

import (
	"regexp"
	"strings"
	"unicode"
)

// CLI names a memory format. They double as the `cli:` frontmatter value and
// the `cli:<name>` tag on drained entries.
const (
	CLIClaude = "claude"
	CLICodex  = "codex"
	CLIGemini = "gemini"
)

// Fact is one unit of CLI memory: a bullet, a paragraph or a short topic
// file. Body is the normalised text; Heading is the nearest markdown heading
// above it, kept for the entry title.
type Fact struct {
	Heading string
	Body    string
}

// maxTitleRunes bounds the generated entry title; the full text stays in the
// body.
const maxTitleRunes = 80

// geminiMemorySection is the heading Gemini CLI's save_memory / `/memory add`
// write under in GEMINI.md. Everything above it is operator-authored
// instruction, not memory, and is not drained.
const geminiMemorySection = "## Gemini Added Memories"

var (
	bulletRe    = regexp.MustCompile(`^(\s*)([-*+]|\d+[.)])\s+(.*)$`)
	headingRe   = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	indexLinkRe = regexp.MustCompile(`^\[[^\]]*\]\([^)]+\.md\)\s*([-—–:]\s*)?`)
	spaceRe     = regexp.MustCompile(`\s+`)
)

// ParseClaudeMemory splits a Claude Code auto-memory file (MEMORY.md or a
// sibling topic file under ~/.claude/projects/<slug>/memory/) into facts.
// MEMORY.md is an index: bullets that are only a link to a sibling topic
// file are skipped, because the topic file itself is drained; a link with a
// trailing summary keeps the summary.
func ParseClaudeMemory(content string) []Fact {
	return parseMarkdown(content, true)
}

// ParseCodexMemories splits one Codex memory markdown file
// ($CODEX_HOME/memories*/… such as memory_summary.md or a rollout summary)
// into facts.
func ParseCodexMemories(content string) []Fact {
	return parseMarkdown(content, false)
}

// ParseGeminiMemory splits ~/.gemini/GEMINI.md into facts. Only the
// "## Gemini Added Memories" section is memory; when the heading is absent
// the file is treated as memory in full (a GEMINI.md that exists only in a
// per-agent Hive home was written by the CLI, not by an operator).
func ParseGeminiMemory(content string) []Fact {
	if idx := strings.Index(content, geminiMemorySection); idx >= 0 {
		content = content[idx:]
	}
	return parseMarkdown(content, false)
}

// ParseFor dispatches on cli; unknown names use the generic markdown rules.
func ParseFor(cli, content string) []Fact {
	switch cli {
	case CLIClaude:
		return ParseClaudeMemory(content)
	case CLIGemini:
		return ParseGeminiMemory(content)
	default:
		return ParseCodexMemories(content)
	}
}

// parseMarkdown is the shared splitter: every top-level list item is a fact
// (indented continuation lines and nested items are folded into it) and
// every paragraph outside a list is a fact. Headings set the context but
// are never facts themselves; fenced code blocks are kept verbatim inside
// the fact they belong to.
func parseMarkdown(content string, skipIndexLinks bool) []Fact {
	var (
		facts   []Fact
		heading string
		cur     []string
		inItem  bool
		inFence bool
	)
	flush := func() {
		text := normalise(strings.Join(cur, "\n"))
		cur = cur[:0]
		inItem = false
		if text == "" {
			return
		}
		if skipIndexLinks {
			if loc := indexLinkRe.FindStringIndex(text); loc != nil {
				text = strings.TrimSpace(text[loc[1]:])
				if text == "" {
					return
				}
			}
		}
		facts = append(facts, Fact{Heading: heading, Body: text})
	}
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			cur = append(cur, line)
			continue
		}
		if inFence {
			cur = append(cur, line)
			continue
		}
		if trimmed == "" {
			flush()
			continue
		}
		if m := headingRe.FindStringSubmatch(trimmed); m != nil {
			flush()
			heading = strings.TrimSpace(m[1])
			continue
		}
		if trimmed == "---" || trimmed == "***" {
			flush()
			continue
		}
		if m := bulletRe.FindStringSubmatch(line); m != nil {
			if m[1] == "" || !inItem {
				flush()
				inItem = true
				cur = append(cur, m[3])
				continue
			}
			// Nested item: fold into the parent as a clause.
			last := strings.TrimRight(cur[len(cur)-1], " :;,")
			cur[len(cur)-1] = last + "; " + m[3]
			continue
		}
		cur = append(cur, trimmed)
	}
	flush()
	return facts
}

// normalise collapses whitespace outside code fences so the same fact
// written with different wrapping hashes identically.
func normalise(text string) string {
	if strings.Contains(text, "```") || strings.Contains(text, "~~~") {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(spaceRe.ReplaceAllString(text, " "))
}

// Title derives an entry title from the fact: the heading (when it is not
// a generic container such as "Memory") followed by the first sentence of
// the body, truncated on a word boundary.
func (f Fact) Title() string {
	body := f.Body
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[:i]
	}
	body = strings.TrimLeft(body, "`#*_ ")
	if end := sentenceEnd(body); end > 0 {
		body = body[:end]
	}
	body = strings.TrimSuffix(body, ".")
	body = truncateWords(body, maxTitleRunes)
	if h := strings.TrimSpace(f.Heading); h != "" && !genericHeading(h) && !strings.HasPrefix(strings.ToLower(body), strings.ToLower(h)) {
		return h + ": " + body
	}
	return body
}

func sentenceEnd(s string) int {
	for i, r := range s {
		if (r == '.' || r == '!' || r == '?') && i+1 < len(s) && unicode.IsSpace(rune(s[i+1])) {
			return i
		}
	}
	return 0
}

func truncateWords(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	cut := string(runes[:max])
	if !unicode.IsSpace(runes[max]) {
		if i := strings.LastIndex(cut, " "); i > max/2 {
			cut = cut[:i]
		}
	}
	return strings.TrimRight(cut, " ,;:-") + "…"
}

var genericHeadings = map[string]bool{
	"memory": true, "memories": true, "notes": true, "summary": true,
	"gemini added memories": true, "memory summary": true, "project memory": true,
}

func genericHeading(h string) bool {
	return genericHeadings[strings.ToLower(strings.TrimSpace(h))]
}
