package outcomebench

import (
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The maintainer-approved outcome-led README keeps plain prose alongside
// sourced charts. Ratchet its budget down as the copy shrinks.
const (
	readmeMaxProseWords = 600
	readmeMaxSections   = 10
)

var (
	reHTMLComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	reFenced      = regexp.MustCompile("(?s)```.*?```")
	reHTMLTag     = regexp.MustCompile(`(?s)<[^>]+>`)
	reImage       = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	reLinkTarget  = regexp.MustCompile(`\]\([^)]*\)`)
	reAutoLink    = regexp.MustCompile(`<?https?://\S+>?`)
	reInlineCode  = regexp.MustCompile("`[^`]*`")
)

// readmeProse strips everything that is not read as prose: HTML (the
// <picture> cards), comments, fenced code, badges and images, link URLs and
// inline code. Link text stays, because the reader reads it.
func readmeProse(md string) string {
	md = reHTMLComment.ReplaceAllString(md, " ")
	md = reFenced.ReplaceAllString(md, " ")
	md = reImage.ReplaceAllString(md, " ")
	md = reHTMLTag.ReplaceAllString(md, " ")
	md = reLinkTarget.ReplaceAllString(md, "]")
	md = reAutoLink.ReplaceAllString(md, " ")
	md = reInlineCode.ReplaceAllString(md, " ")
	return md
}

// countWords counts whitespace-separated tokens that contain a letter or
// digit, so punctuation-only tokens such as "·" or "---" are not words.
func countWords(s string) int {
	n := 0
	for _, tok := range strings.Fields(s) {
		if strings.IndexFunc(tok, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
			n++
		}
	}
	return n
}

func readRepoReadme(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReadmeProseBudget(t *testing.T) {
	readme := readRepoReadme(t)
	words := countWords(readmeProse(readme))
	t.Logf("README prose: %d words (budget %d)", words, readmeMaxProseWords)
	if words > readmeMaxProseWords {
		t.Errorf("README prose is %d words; budget is %d. Move detail into src/docs/ (what-is-hive.md or the benchmarks doc) instead of the README.", words, readmeMaxProseWords)
	}
	sections := 0
	for _, line := range strings.Split(readme, "\n") {
		if strings.HasPrefix(line, "## ") {
			sections++
		}
	}
	if sections > readmeMaxSections {
		t.Errorf("README has %d H2 sections; budget is %d", sections, readmeMaxSections)
	}
}

// Vocabulary that belongs in the benchmarks report, not the README, plus the
// persuasive words the README copy avoids. Matched case-insensitively on
// word boundaries against the prose and against the generated card text.
var readmeForbiddenWords = []string{
	// statistical
	"wilson", "interval", "intervals", "n=", "median", "cohort", "cohorts",
	"mediated", "unmediated", "hatched", "95%", "confidence", "p-value", "whisker",
	// marketing
	"powerful", "seamless", "seamlessly", "blazing", "revolutionary", "best-in-class",
	"effortless", "effortlessly", "supercharge", "game-changing", "cutting-edge",
	"world-class", "unleash", "amazing", "incredible", "never sleeps",
	// insider nouns: a first-time reader does not know them. The two actor
	// names are "the coding agent" and "the review agent".
	"reviewer", "reviewers", "scanner", "author", "authors", "verdict", "verdicts",
	"change request", "change requests", "changes requested", "house style", "agent card",
	"acmm", "trust level", "trust tier", "hub and spoke", "hosted hub", "spoke", "spokes",
	"contributor relay", "kick", "kicks", "ioscan", "instruction-override",
}

// Legacy benchmark cards use compact dates. The root README uses explicit
// dates so readers can identify the snapshot behind each chart.
// A prose line that is one or two words followed by a colon is a label, not
// a sentence ("Numbers below:"). Headings and link rows are exempt because
// they have no colon.
var readmeLabelLine = regexp.MustCompile(`^\s*[a-z][a-z' -]{0,24}:\s`)

var readmeForbiddenDates = []*regexp.Regexp{
	regexp.MustCompile(`\d{4}-\d{2}-\d{2}`),
	regexp.MustCompile(`\b20\d\d\b`),
}

func TestReadmeVocabulary(t *testing.T) {
	readme := readRepoReadme(t)
	texts := map[string]string{"README prose": strings.ToLower(readmeProse(readme))}
	docDir := filepath.Join("..", "..", "docs", "benchmarks")
	svgs, _ := filepath.Glob(filepath.Join(docDir, "assets", "outcomes-*-card-*.svg"))
	for _, p := range svgs {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		texts[filepath.Base(p)] = strings.ToLower(html.UnescapeString(reHTMLTag.ReplaceAllString(string(b), " ")))
	}
	for name, text := range texts {
		for _, w := range readmeForbiddenWords {
			re := regexp.MustCompile(`(^|[^a-z0-9-])` + regexp.QuoteMeta(w) + `($|[^a-z0-9-])`)
			if re.MatchString(text) {
				t.Errorf("%s contains %q; keep statistical and marketing vocabulary out of the README and its cards", name, w)
			}
		}
		if name != "README prose" {
			for _, re := range readmeForbiddenDates {
				if m := re.FindString(text); m != "" {
					t.Errorf("%s contains date %q; write dates as (oct '26)", name, m)
				}
			}
		}
		// The copy reads like a person talking: short full sentences, one
		// idea each. No semicolon-joined fragments and no "Numbers below:"
		// style labels introducing a list.
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, ";") {
				t.Errorf("%s has a semicolon; write two sentences: %q", name, strings.TrimSpace(line))
			}
			if readmeLabelLine.MatchString(line) {
				t.Errorf("%s has a label introducing a list: %q", name, strings.TrimSpace(line))
			}
		}
		if strings.Contains(text, "!") && name == "README prose" {
			for _, line := range strings.Split(text, "\n") {
				if strings.Contains(line, "!") {
					t.Errorf("README prose has an exclamation mark: %q", strings.TrimSpace(line))
				}
			}
		}
	}
}
