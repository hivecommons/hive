package priorart

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Term is one key term of an issue with its weight.
type Term struct {
	Text string
	// Weight: WeightIdentifier for backticked / dotted / snake / kebab /
	// --flag tokens, WeightTitle for plain title words, WeightBody for the
	// most frequent body words.
	Weight int
	// Identifier marks a term that looks like code, a config key or a flag.
	Identifier bool
	// FromTitle marks a term that appears in the title.
	FromTitle bool
}

const (
	WeightIdentifier = 4
	WeightTitle      = 3
	WeightBody       = 1
	// MaxTerms bounds the vocabulary so a long RFC does not dilute every
	// candidate's score.
	MaxTerms = 12
	// maxBodyTerms is how many body-only words join the title words.
	maxBodyTerms = 6
	// minTermLen drops "a", "of", "ui"-length noise; three-letter
	// identifiers still pass through the identifier path.
	minTermLen = 3
)

var (
	htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
	urlRE         = regexp.MustCompile(`https?://\S+`)
	codeFenceRE   = regexp.MustCompile("(?s)```.*?```")
	backtickRE    = regexp.MustCompile("`([^`\n]{2,80})`")
	// identifierRE: --flag, dotted.key, snake_case, kebab-case, CamelCase
	// with at least one inner upper-case letter, path/with/slash.
	identifierRE = regexp.MustCompile(`(?:--[a-z][a-z0-9-]{2,}|[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+|[a-z0-9]+(?:_[a-z0-9]+)+|[a-z0-9]+(?:-[a-z0-9]+){1,}|[a-z]+[A-Z][A-Za-z0-9]+|[a-z0-9_./-]+/[a-z0-9_.-]+\.[a-z]{1,5})`)
	tokenSplitRE = regexp.MustCompile(`[^a-z0-9]+`)
	// boilerplateLineRE drops the template scaffolding hive-filed and
	// template-filed issues share: "Source:", "Related:", headings, quoted
	// replies, checklists markers.
	boilerplateLineRE = regexp.MustCompile(`(?i)^\s*(?:#+\s|>|source:|related:|refs?:|see also:|cc\b|/hive\b|\*\*(?:source|related|refs?)\*\*:)`)
)

var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for are not but can all any its our you your they them then than
		will just more some such other each been being does doing done way get set has had was were who how why
		this that these those there their what which when where with without into from about like also only make
		need needs able allow allows option options setting settings enable enabled disable disabled new use used
		using want wants would should could please hive hives issue issues feature features add adds added support
		rfc enhancement proposal request requests agent agents currently today already instead maybe still even
		very much many lot lots thing things time true false key keys default defaults http https www com github
		after before back hand epic proposed propose implement implementation implemented change changes changed
		problem problems solution solutions motivation summary context acceptance criteria description describe
		expected actual behaviour behavior steps reproduce version versions example examples note notes
		something anything nothing because while until since within across through over under again
		pull merge merged open opened closed close repo repos repository code file files line lines
		user users person people maintainer maintainers owner owners tell tells told ask asks asked let lets
		show shows give gives provide provides someone anyone everyone`) {
		stopwords[w] = true
	}
}

// ExtractTerms derives the key terms of an issue: identifiers (weight 4),
// plain title words (weight 3) and the most frequent remaining body words
// (weight 1), capped at MaxTerms. Boilerplate — HTML comments, URLs, code
// fences, template lines — is stripped first.
func ExtractTerms(title, body string) []Term {
	title = strings.TrimSpace(stripTitleTags(title))
	body = cleanBody(body)

	byText := map[string]*Term{}
	add := func(text string, weight int, ident, fromTitle bool) {
		if text == "" {
			return
		}
		t, ok := byText[text]
		if !ok {
			t = &Term{Text: text}
			byText[text] = t
		}
		if weight > t.Weight {
			t.Weight = weight
		}
		t.Identifier = t.Identifier || ident
		t.FromTitle = t.FromTitle || fromTitle
	}

	for _, id := range findIdentifiers(title) {
		add(id, WeightIdentifier, true, true)
	}
	for _, id := range findIdentifiers(body) {
		add(id, WeightIdentifier, true, false)
	}
	for _, w := range Tokens(title) {
		add(w, WeightTitle, false, true)
	}

	// Body words: frequency-ranked, title words excluded, top maxBodyTerms.
	freq := map[string]int{}
	for _, w := range Tokens(body) {
		if t, ok := byText[w]; ok && t.FromTitle {
			continue
		}
		freq[w]++
	}
	bodyWords := make([]string, 0, len(freq))
	for w := range freq {
		bodyWords = append(bodyWords, w)
	}
	sort.Slice(bodyWords, func(i, j int) bool {
		if freq[bodyWords[i]] != freq[bodyWords[j]] {
			return freq[bodyWords[i]] > freq[bodyWords[j]]
		}
		return bodyWords[i] < bodyWords[j]
	})
	for i, w := range bodyWords {
		if i == maxBodyTerms {
			break
		}
		if freq[w] < 2 && i > 0 {
			// A word that appears once in the body is noise unless the
			// body is so short nothing else qualifies.
			break
		}
		add(w, WeightBody, false, false)
	}

	out := make([]Term, 0, len(byText))
	for _, t := range byText {
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		if out[i].FromTitle != out[j].FromTitle {
			return out[i].FromTitle
		}
		return out[i].Text < out[j].Text
	})
	if len(out) > MaxTerms {
		out = out[:MaxTerms]
	}
	return out
}

// identifiers returns the identifier terms' text.
func identifiers(terms []Term) []string {
	var out []string
	for _, t := range terms {
		if t.Identifier {
			out = append(out, t.Text)
		}
	}
	return out
}

// kindTags are the leading title tags stripped before term extraction so
// "[RFC] triage: …" keeps "triage" (a real area) and loses "rfc" (a kind).
var kindTags = map[string]bool{"rfc": true, "epic": true, "feat": true, "feature": true, "proposal": true, "enhancement": true, "idea": true, "wip": true, "draft": true}

// stripTitleTags removes leading "[RFC]", "RFC:", "Epic:" style kind tags
// so they do not become terms. Area prefixes such as "triage:" stay.
func stripTitleTags(title string) string {
	for {
		s := strings.TrimSpace(title)
		if strings.HasPrefix(s, "[") {
			if i := strings.Index(s, "]"); i > 0 && i < 24 {
				title = s[i+1:]
				continue
			}
			return s
		}
		i := strings.IndexAny(s, ":—–-")
		if i <= 0 || i > 16 {
			return s
		}
		if tag := strings.ToLower(strings.TrimSpace(s[:i])); kindTags[tag] {
			title = afterSep(s[i:])
			continue
		}
		return s
	}
}

// afterSep drops the leading separator rune (":", "—", "–", "-").
func afterSep(s string) string {
	_, n := utf8.DecodeRuneInString(s)
	return s[n:]
}

func cleanBody(body string) string {
	body = htmlCommentRE.ReplaceAllString(body, " ")
	body = codeFenceRE.ReplaceAllString(body, " ")
	body = urlRE.ReplaceAllString(body, " ")
	var keep []string
	for _, line := range strings.Split(body, "\n") {
		if boilerplateLineRE.MatchString(line) {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// findIdentifiers returns backticked spans and identifier-shaped tokens,
// lower-cased, de-duplicated, in first-seen order.
func findIdentifiers(s string) []string {
	seen := map[string]bool{}
	var out []string
	push := func(v string) {
		v = strings.ToLower(strings.Trim(v, "`\"' .,;:()[]{}"))
		if len(v) < minTermLen || len(v) > 80 || seen[v] || stopwords[v] {
			return
		}
		if strings.Count(v, " ") > 2 {
			return
		}
		if isNumeric(v) {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	for _, m := range backtickRE.FindAllStringSubmatch(s, -1) {
		push(m[1])
	}
	for _, m := range identifierRE.FindAllString(s, -1) {
		if strings.ContainsAny(m, "_.-/") || hasInnerUpper(m) {
			push(m)
		}
	}
	return out
}

func hasInnerUpper(s string) bool {
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

func isNumeric(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) && r != '.' {
			return false
		}
	}
	return true
}

// Tokens lower-cases s, splits it on anything that is not a letter or
// digit and drops stopwords and words shorter than minTermLen. A trailing
// plural "s" is trimmed so "labels" and "label" match.
func Tokens(s string) []string {
	var out []string
	for _, t := range tokenSplitRE.Split(strings.ToLower(s), -1) {
		t = singular(t)
		if len(t) < minTermLen || stopwords[t] || isNumeric(t) {
			continue
		}
		out = append(out, t)
	}
	return out
}

func singular(t string) string {
	if len(t) > 4 && strings.HasSuffix(t, "s") && !strings.HasSuffix(t, "ss") {
		return strings.TrimSuffix(t, "s")
	}
	return t
}

// Rank scores a candidate against the issue's terms: the share of weighted
// terms the candidate's title+text contain. A candidate must match at
// least two terms including one title term (or one identifier) to count.
// It returns false when the candidate does not qualify.
func Rank(terms []Term, c Candidate) (Finding, bool) {
	if len(terms) == 0 {
		return Finding{}, false
	}
	hay := strings.ToLower(c.Title + "\n" + c.Text)
	hayTokens := map[string]bool{}
	for _, w := range tokenSplitRE.Split(hay, -1) {
		if w != "" {
			hayTokens[w] = true
			hayTokens[singular(w)] = true
		}
	}
	total, got := 0, 0
	var matched []string
	anchored := false
	for _, t := range terms {
		total += t.Weight
		hit := false
		if t.Identifier {
			hit = strings.Contains(hay, t.Text)
		} else {
			hit = hayTokens[t.Text]
		}
		if !hit {
			continue
		}
		got += t.Weight
		matched = append(matched, t.Text)
		if t.FromTitle || t.Identifier {
			anchored = true
		}
	}
	if total == 0 || len(matched) < 2 || !anchored {
		return Finding{}, false
	}
	sort.Strings(matched)
	return Finding{
		Candidate:  c,
		Confidence: (got*100 + total/2) / total,
		Matched:    matched,
		Quote:      bestLine(c, matched),
	}, true
}

// bestLine is the candidate line that contains the most matched terms,
// trimmed to SnippetMax runes; the title when no body line matches.
func bestLine(c Candidate, matched []string) string {
	best, bestN := "", 0
	for _, line := range strings.Split(c.Text, "\n") {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "<!--") {
			continue
		}
		low := strings.ToLower(l)
		n := 0
		for _, m := range matched {
			if strings.Contains(low, m) {
				n++
			}
		}
		if n > bestN {
			best, bestN = l, n
		}
	}
	if best == "" {
		best = strings.TrimSpace(c.Title)
	}
	best = strings.Join(strings.Fields(best), " ")
	if r := []rune(best); len(r) > SnippetMax {
		best = string(r[:SnippetMax-1]) + "…"
	}
	return best
}
