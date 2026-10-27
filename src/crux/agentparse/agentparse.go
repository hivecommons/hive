// Package agentparse provides generic, reusable parsers that turn an agent's
// structured text output (tables, numbered/bulleted lists, and `bd create`
// command lines) into typed items.
//
// These parsers were lifted out of the inception (Level-1 ideation) watcher so
// that other subsystems — notably planning intelligence (epic -> bead-DAG
// decomposition) — can reuse the exact same battle-tested extraction logic
// instead of re-implementing it. Inception continues to use these via thin,
// behavior-preserving adapters.
//
// An Item carries a Title plus optional Default and Category metadata; callers
// map Items into their own domain types (questions, sub-tasks, ...).
package agentparse

import (
	"regexp"
	"strings"
)

// Item is a generic parsed list entry. Title is the primary text (a question,
// a sub-task, ...); Default and Category are optional metadata that some
// parsers populate. Callers translate Items into their own domain types.
type Item struct {
	// Category is a coarse classification label. For the table/numbered parsers
	// it is derived from the caller-supplied categories; empty when none match
	// and no fallback is configured.
	Category string
	// Title is the primary extracted text.
	Title string
	// Default is an optional default/answer column (table parser) — empty when
	// the source has none.
	Default string
}

// ParseTable extracts items from an agent's │-delimited table output.
//
// Each row is split on │. The parser finds the column whose (lower-cased) value
// is present in categories, treats the next column as the Title and the one
// after as the Default. Rows with no category column are treated as
// continuations that append their non-empty columns to the current item's
// Title/Default. Items are de-duplicated by category+title.
//
// This is the generic form of inception's parseQuestionTable.
func ParseTable(lines []string, categories map[string]bool) []Item {
	var items []Item
	seen := make(map[string]bool)

	var currentCat, currentTitle, currentDefault string

	flush := func() {
		if currentCat == "" || currentTitle == "" {
			return
		}
		key := currentCat + ":" + currentTitle
		if seen[key] {
			return
		}
		seen[key] = true
		items = append(items, Item{
			Category: currentCat,
			Title:    strings.TrimSpace(currentTitle),
			Default:  strings.TrimSpace(currentDefault),
		})
	}

	for _, line := range lines {
		if !strings.Contains(line, "│") {
			continue
		}

		cols := strings.Split(line, "│")
		cleaned := make([]string, 0, len(cols))
		for _, c := range cols {
			cleaned = append(cleaned, strings.TrimSpace(c))
		}

		catIdx := -1
		for i, c := range cleaned {
			if categories[strings.ToLower(c)] {
				catIdx = i
				break
			}
		}

		if catIdx >= 0 && catIdx+1 < len(cleaned) {
			flush()
			currentCat = strings.ToLower(cleaned[catIdx])
			currentTitle = cleaned[catIdx+1]
			if catIdx+2 < len(cleaned) {
				currentDefault = cleaned[catIdx+2]
			} else {
				currentDefault = ""
			}
		} else if currentCat != "" {
			// Continuation row — append non-empty columns to title/default.
			nonEmpty := 0
			for _, c := range cleaned {
				if c != "" {
					nonEmpty++
					switch nonEmpty {
					case 1:
						currentTitle += " " + c
					case 2:
						currentDefault += " " + c
					}
				}
			}
		}
	}

	flush()
	return items
}

// numberedListRe matches lines like "1. Primary users — who will use this?"
// or "1. **Primary users** — who?" or "- Primary users: ...". Group 1 is the
// label (word chars, spaces and slashes), group 2 the remaining text.
var numberedListRe = regexp.MustCompile(`^\s*(?:\d+[\.\)]\s*|[-*]\s+)(?:\*\*)?(\w[\w/\s]*?)(?:\*\*)?\s*[-—:]+\s*(.+)`)

// NumberedListRe exposes the numbered/bulleted-list matcher so callers that
// need to strip list decoration from a single line (rather than a whole slice)
// can reuse the exact same pattern.
func NumberedListRe() *regexp.Regexp { return numberedListRe }

// ParseNumberedList extracts items from numbered or bulleted lists such as:
//
//  1. Primary users — who will use this and how?
//  2. Must-have features — the 2-3 things it must do
//     - Language: what language?
//
// The captured label is matched against categories (substring match); when
// none match, fallbackCategory is used. The remaining text becomes the Title.
// Items are de-duplicated by category+title. This is the generic form of
// inception's parseNumberedQuestions.
func ParseNumberedList(lines []string, categories map[string]bool, fallbackCategory string) []Item {
	var items []Item
	seen := make(map[string]bool)

	for _, line := range lines {
		m := numberedListRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}

		label := strings.TrimSpace(strings.ToLower(m[1]))
		title := strings.TrimSpace(m[2])
		if title == "" {
			continue
		}

		cat := ""
		for kw := range categories {
			if strings.Contains(label, kw) {
				cat = kw
				break
			}
		}
		if cat == "" {
			cat = fallbackCategory
		}

		key := cat + ":" + title
		if seen[key] {
			continue
		}
		seen[key] = true

		items = append(items, Item{Category: cat, Title: title, Default: ""})
	}

	return items
}

// bdCreateTitleRe extracts the --title value from a `bd create` command line.
var bdCreateTitleRe = regexp.MustCompile(`--title\s+"([^"]+)"`)

// defaultPlaceholders are placeholder titles the agent emits when showing a
// command template before filling in real values.
var defaultPlaceholders = []string{
	"<fact title>", "<question title>", "<title>",
	"<your question>", "<description>", "<bead title>",
	"fact_title", "question_title", "your question here",
}

// IsTemplatePlaceholder reports whether title is a template placeholder rather
// than a real value. Any "<...>" angle-bracketed string is a placeholder, as is
// any exact match against defaultPlaceholders. This is the generic form of
// inception's isTemplatePlaceholder.
func IsTemplatePlaceholder(title string) bool {
	lower := strings.ToLower(title)
	if strings.HasPrefix(lower, "<") && strings.HasSuffix(lower, ">") {
		return true
	}
	for _, p := range defaultPlaceholders {
		if lower == p {
			return true
		}
	}
	return false
}

// TitleFromBdCreate extracts the --title value from a `bd create` command line,
// returning ("", false) when there is no title, the title is empty, or it is a
// template placeholder. This is the generic core of inception's
// parseQuestionFromBdCreate (category inference and prefix handling stay with
// the caller).
func TitleFromBdCreate(line string) (string, bool) {
	m := bdCreateTitleRe.FindStringSubmatch(line)
	// len(m) < 2 is defensive: the regex has one capture group, so a non-nil
	// match always has len 2. It cannot be exercised in a test.
	if len(m) < 2 {
		return "", false
	}
	title := strings.TrimSpace(m[1])
	if title == "" {
		return "", false
	}
	if IsTemplatePlaceholder(title) {
		return "", false
	}
	return title, true
}

// taskListRe matches an ordered/bulleted task line, optionally prefixed with a
// task identifier the decomposer references for dependencies, e.g.:
//
//  1. [T1] Scaffold the module skeleton
//     - [T2] Wire config loading (depends: T1)
//
// Group 1 (optional) is the task ID token; group 2 is the task text.
var taskListRe = regexp.MustCompile(`^\s*(?:\d+[\.\)]\s*|[-*]\s+)(?:\[([^\]]+)\]\s*)?(.+)$`)

// depsRe extracts a trailing "(depends: A, B)" / "(deps: A)" clause.
var depsRe = regexp.MustCompile(`(?i)\(\s*depends?(?:\s+on)?\s*:\s*([^)]*)\)`)

// executionRe extracts a trailing execution tag written as a parenthetical:
// "(execution: agent_suitable)", "(exec: human required)", or the bare
// "(agent_suitable)" / "(human_required)" form. The bracketed "[agent_suitable]"
// form is handled separately in ParseTaskList.
var executionRe = regexp.MustCompile(`(?i)\(\s*(?:(?:execution|exec)\s*:\s*)?(agent[_ ]suitable|human[_ ]required)\s*\)`)

// Task is a parsed sub-task emitted by ParseTaskList. Ref is the task's local
// identifier (e.g. "T1") used to resolve DependsOn references; it is empty when
// the line carried no [ID] token. DependsOn holds the referenced local ids.
// Execution is the normalized execution tag ("agent_suitable" or
// "human_required"), or "" when the line carried none.
type Task struct {
	Ref       string
	Title     string
	Execution string
	DependsOn []string
}

// Normalized execution tag values.
const (
	ExecutionAgentSuitable = "agent_suitable"
	ExecutionHumanRequired = "human_required"
)

// normalizeExecution maps loose forms ("agent suitable", "human-required") onto
// the canonical constants, returning "" for anything unrecognized.
func normalizeExecution(s string) string {
	switch strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), " ", "_") {
	case ExecutionAgentSuitable:
		return ExecutionAgentSuitable
	case ExecutionHumanRequired:
		return ExecutionHumanRequired
	default:
		return ""
	}
}

// ParseTaskList parses an agent's ordered sub-task breakdown into Tasks. Each
// task line may carry an optional [ID] token, a trailing "(depends: ...)"
// clause referencing other ids, and a trailing execution tag. Lines that are
// not list items are ignored. Tasks are de-duplicated by title.
//
// Example input:
//
//  1. [T1] Design the data model [agent_suitable]
//  2. [T2] Get legal sign-off (human_required)
//  3. [T3] Implement persistence (depends: T1) [agent_suitable]
func ParseTaskList(lines []string) []Task {
	var tasks []Task
	seen := make(map[string]bool)

	for _, line := range lines {
		m := taskListRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		ref := strings.TrimSpace(m[1])
		rest := strings.TrimSpace(m[2])
		// Defensive: taskListRe's group 2 is `(.+)`, so a matched line always has
		// non-empty rest before trimming. This guard covers the whitespace-only
		// edge and cannot be exercised via the regex.
		if rest == "" {
			continue
		}

		var deps []string
		if dm := depsRe.FindStringSubmatch(rest); dm != nil {
			for _, d := range strings.Split(dm[1], ",") {
				if d = strings.TrimSpace(d); d != "" {
					deps = append(deps, d)
				}
			}
			rest = strings.TrimSpace(depsRe.ReplaceAllString(rest, ""))
		}

		execution := ""
		if em := executionRe.FindStringSubmatch(rest); em != nil {
			execution = normalizeExecution(em[1])
			rest = strings.TrimSpace(executionRe.ReplaceAllString(rest, ""))
		} else {
			// Also accept a bare bracketed [agent_suitable]/[human_required] tag.
			for _, tag := range []string{ExecutionAgentSuitable, ExecutionHumanRequired} {
				bracket := "[" + tag + "]"
				if idx := strings.Index(strings.ToLower(rest), bracket); idx >= 0 {
					execution = tag
					rest = strings.TrimSpace(rest[:idx] + rest[idx+len(bracket):])
					break
				}
			}
		}

		title := strings.TrimSpace(rest)
		if title == "" {
			continue
		}
		if seen[title] {
			continue
		}
		seen[title] = true

		tasks = append(tasks, Task{
			Ref:       ref,
			Title:     title,
			Execution: execution,
			DependsOn: deps,
		})
	}

	return tasks
}

// SplitLines splits raw agent output into trimmed-right lines, dropping the
// trailing empty element from a final newline. Provided so callers do not each
// re-implement the same normalization before calling the line-based parsers.
func SplitLines(raw string) []string {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	parts := strings.Split(raw, "\n")
	// Drop a single trailing empty line from a terminal newline.
	if n := len(parts); n > 0 && parts[n-1] == "" {
		parts = parts[:n-1]
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = strings.TrimRight(p, " \t")
	}
	return out
}
