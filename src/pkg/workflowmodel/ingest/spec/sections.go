// Package spec turns a written process description (markdown or plain
// text) into a draft workflow model.
//
// Two stages run. The deterministic stage (no LLM) reads step lists,
// conditional sentences and role nouns into a skeleton. The optional LLM
// stage asks a configured gateway to refine that skeleton; its answer is
// parsed strictly, normalised and compiled, and it is dropped in favour of
// the skeleton when it does not compile after one retry.
//
// This is the only place in the workflow-model stack where an LLM is
// allowed. Whatever the stage, every state, event, gate and role carries
// confidence "inferred", cites the spec section it came from
// (spec:<heading>#L<start>-L<end>), and the model is marked
// provenance ingested-unreviewed so the editor demands review.
package spec

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxBytes is the largest spec accepted.
const MaxBytes = 256 * 1024

// Section is one heading-delimited slice of the spec with its line range.
type Section struct {
	Heading string `json:"heading"`
	Level   int    `json:"level"`
	// StartLine and EndLine are 1-based and inclusive.
	StartLine int      `json:"startLine"`
	EndLine   int      `json:"endLine"`
	Lines     []string `json:"-"`
}

// Ref is the citation path for the section.
func (s Section) Ref() string {
	return fmt.Sprintf("spec:%s#L%d-L%d", s.Heading, s.StartLine, s.EndLine)
}

var (
	atxHeading = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$`)
	setextRule = regexp.MustCompile(`^\s{0,3}(=+|-+)\s*$`)
)

// Split cuts text into sections at markdown headings. Text before the
// first heading is the "Preamble" section; a spec without headings is a
// single "Document" section. Line numbers are 1-based.
func Split(text string) []Section {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var out []Section
	cur := Section{Heading: "Preamble", Level: 0, StartLine: 1}
	flush := func(end int) {
		cur.EndLine = end
		if strings.TrimSpace(strings.Join(cur.Lines, "")) != "" || cur.Level > 0 {
			out = append(out, cur)
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := atxHeading.FindStringSubmatch(line); m != nil {
			flush(i)
			cur = Section{Heading: m[2], Level: len(m[1]), StartLine: i + 1}
			continue
		}
		if i+1 < len(lines) && strings.TrimSpace(line) != "" && setextRule.MatchString(lines[i+1]) && !strings.HasPrefix(strings.TrimSpace(line), "-") {
			flush(i)
			level := 1
			if strings.HasPrefix(strings.TrimSpace(lines[i+1]), "-") {
				level = 2
			}
			cur = Section{Heading: strings.TrimSpace(line), Level: level, StartLine: i + 1}
			i++
			continue
		}
		cur.Lines = append(cur.Lines, line)
	}
	flush(len(lines))
	if len(out) == 1 && out[0].Level == 0 {
		out[0].Heading = "Document"
	}
	return out
}

// headings returns the heading set for citation validation.
func headings(secs []Section) map[string]Section {
	m := map[string]Section{}
	for _, s := range secs {
		m[s.Heading] = s
	}
	return m
}
