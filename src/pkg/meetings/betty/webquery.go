package betty

import (
	"regexp"
	"strings"
)

// Web query cleaning and the public-web guard. A question reaches the public
// web (SearXNG or a direct source) only as a cleaned search query, and a
// question about this hive never reaches it at all: sending "how many open
// issues" to a search engine leaks what the meeting is about and cannot
// answer it anyway.

// maxFramingPasses bounds the framing-stripping loop.
const maxFramingPasses = 8

// framingCut is what separates a stripped framing phrase from the query.
const framingCut = " \t,.:;!-"

// webExplicit are phrasings that ask for a web search by name. A question
// that opens with one may go to the web even when it is about the hive: the
// speaker asked for exactly that.
var webExplicit = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(?:do|run|try|make) (?:a |an )?(?:quick )?(?:web|internet|online|google) search\b(?: for| on| about)?`),
	regexp.MustCompile(`(?i)^(?:search|check|look|browse|query) (?:on |up on |up in |in |through )?(?:the )?(?:web|internet|net)\b(?: for| about| on)?`),
	regexp.MustCompile(`(?i)^(?:search|look|check) online\b(?: for| about)?`),
	regexp.MustCompile(`(?i)^(?:web|internet|online) search\b(?: for| about| on)?`),
	regexp.MustCompile(`(?i)^google\b(?: for| about)?`),
}

// webSearchVerb opens a request to look something up; with a trailing "on
// the web" or "online" it is an explicit web search too.
var webSearchVerb = regexp.MustCompile(`(?i)^(?:look up|find out|figure out|search for|look for|look into|check|find|research)\b`)

// webTrailingExplicit closes a request with where to look.
var webTrailingExplicit = regexp.MustCompile(`(?i)\s+(?:(?:on|from|using) the (?:web|internet)|online)$`)

// webFraming are the polite or conversational openings that are not part
// of the question.
var webFraming = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(?:um+|uh+|er+|so|well|and|also|okay|ok|hey|hi|hello|please|kindly|quick question|one more thing|by the way)\b`),
	regexp.MustCompile(`(?i)^(?:can|could|would|will|might) you\b(?: please| kindly| possibly| maybe| just| quickly)*`),
	regexp.MustCompile(`(?i)^(?:i'd|i would|we'd|we would) (?:like|love) (?:you )?to\b(?: know| hear| ask| find out)?`),
	regexp.MustCompile(`(?i)^(?:i|we) (?:want|need|wanted|was hoping|were hoping) (?:you )?to\b(?: know| hear| ask| find out)?`),
	regexp.MustCompile(`(?i)^do you (?:happen to )?know\b(?: if| whether)?`),
	regexp.MustCompile(`(?i)^(?:tell|show|give|remind|inform|let|help) (?:me|us)\b(?: know| understand| find out)?`),
	regexp.MustCompile(`(?i)^(?:i'm|i am|i was|we're|we are|we were) (?:just )?(?:wondering|curious)\b(?: if| whether)?`),
	regexp.MustCompile(`(?i)^(?:what i want to know is|my question is|the question is)\b`),
}

// webAbout is the "about" left after "tell me about ..." is stripped.
var webAbout = regexp.MustCompile(`(?i)^about\b`)

// webTrailing are closings that are not part of the question.
var webTrailing = regexp.MustCompile(`(?i)(?:[\s,]+(?:please|thanks|thank you|for me|for us|if you can|if possible|real quick)|[\s?.!,;:]+)$`)

// cleanedQuery is a question reduced to a search query.
type cleanedQuery struct {
	// Query is the text to search for.
	Query string
	// Explicit: the speaker asked for a web search by name.
	Explicit bool
	// About: the question was "tell me about <Query>".
	About bool
}

// cutPrefix removes re's match from the start of s.
func cutPrefix(s string, re *regexp.Regexp) (string, bool) {
	loc := re.FindStringIndex(s)
	if loc == nil || loc[0] != 0 || loc[1] == 0 {
		return s, false
	}
	return strings.TrimLeft(s[loc[1]:], framingCut), true
}

// cleanQuery strips the framing around a question: greetings and requests
// ("can you tell me", "do you know", "please"), an explicit "search the web
// for", and trailing punctuation and politeness.
func cleanQuery(text string) cleanedQuery {
	var c cleanedQuery
	s := strings.TrimLeft(strings.Join(strings.Fields(text), " "), framingCut)
	searchVerb := false
	for pass := 0; pass < maxFramingPasses; pass++ {
		changed := false
		for _, re := range webExplicit {
			if rest, ok := cutPrefix(s, re); ok {
				s, changed, c.Explicit = rest, true, true
			}
		}
		for _, re := range webFraming {
			if rest, ok := cutPrefix(s, re); ok {
				s, changed, c.About = rest, true, false
			}
		}
		if rest, ok := cutPrefix(s, webSearchVerb); ok {
			s, changed, searchVerb = rest, true, true
		}
		if rest, ok := cutPrefix(s, webAbout); ok && rest != "" {
			s, changed, c.About = rest, true, true
		}
		if !changed {
			break
		}
	}
	for {
		loc := webTrailing.FindStringIndex(s)
		if loc == nil {
			break
		}
		s = s[:loc[0]]
	}
	if loc := webTrailingExplicit.FindStringIndex(s); loc != nil && searchVerb {
		s, c.Explicit = s[:loc[0]], true
	}
	c.Query = strings.TrimSpace(s)
	return c
}

// CleanWebQuery is the search query for a question, and whether the
// speaker asked for a web search by name. It does not strip the persona's
// name; WebGuard does.
func CleanWebQuery(text string) (query string, explicit bool) {
	c := cleanQuery(text)
	return c.Query, c.Explicit
}

// internalPhrases mark a question as being about this hive, its repos or
// this meeting rather than the world: first person plural and the hive's
// own vocabulary.
var internalPhrases = []string{
	"our", "ours", "we", "we re", "we ve",
	"this hive", "the hive", "this repo", "the repo", "this repository", "the repository",
	"this project", "the project", "this codebase", "the codebase",
	"this meeting", "the meeting", "this call", "the transcript", "action items",
	"the backlog", "the board", "work item", "work items", "bead", "beads", "the dashboard",
}

// plainText is text as lowercase words joined by single spaces, with a
// space at each end, so " phrase " finds whole words.
func plainText(text string) string {
	return " " + strings.Join(strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}), " ") + " "
}

// hasPhrase reports whether plain (from plainText) holds one of phrases.
func hasPhrase(plain string, phrases []string) bool {
	for _, p := range phrases {
		if p != "" && strings.Contains(plain, " "+p+" ") {
			return true
		}
	}
	return false
}

// WebGuard decides what may reach the public web. A nil guard still keeps
// hive questions off the web; it only lacks the configured names.
type WebGuard struct {
	match *Matcher
	// names are the persona's names and the hive's org and repo names, as
	// plain phrases.
	names []string
}

// NewWebGuard builds a guard for a persona (its name and aliases) and the
// hive's private names: the configured org and repositories ("org/repo" or
// bare).
func NewWebGuard(name string, aliases, private []string) *WebGuard {
	g := &WebGuard{match: NewMatcher(name, aliases)}
	seen := map[string]bool{}
	add := func(s string) {
		p := strings.TrimSpace(plainText(s))
		if len(p) < minPrivateNameLen || seen[p] {
			return
		}
		seen[p] = true
		g.names = append(g.names, p)
	}
	add(name)
	for _, a := range aliases {
		add(a)
	}
	for _, p := range private {
		add(p)
		for _, part := range strings.Split(p, "/") {
			add(part)
		}
	}
	return g
}

// minPrivateNameLen is the shortest name the guard matches on.
const minPrivateNameLen = 2

// Reasons a question is kept off the public web.
const (
	webBlockedEmpty   = "empty"
	webBlockedStatus  = "hive_status"
	webBlockedProduct = "hive_product"
	webBlockedName    = "private_name"
	webBlockedInside  = "internal"
)

// check cleans a question into a search query and says whether it may go
// to the public web; blocked names why not.
func (g *WebGuard) check(text string) (c cleanedQuery, blocked string) {
	if g != nil && g.match != nil {
		// "Betty, ..." addresses her: the name is not part of the question.
		t := g.match.Match(text, false)
		if t.Intro {
			// "who is Betty": about the persona, answered by her intro.
			return c, webBlockedName
		}
		if t.Addressed {
			text = t.Rest
		}
	}
	c = cleanQuery(text)
	if c.Query == "" {
		return c, webBlockedEmpty
	}
	if c.Explicit {
		return c, ""
	}
	plain := plainText(c.Query)
	switch {
	case IsHiveStatusQuestion(c.Query):
		return c, webBlockedStatus
	case IsProductQuestion(c.Query):
		return c, webBlockedProduct
	case g != nil && hasPhrase(plain, g.names):
		return c, webBlockedName
	case hasPhrase(plain, internalPhrases):
		return c, webBlockedInside
	}
	return c, ""
}

// Query is the cleaned search query for a question, and whether it may be
// sent to the public web. Questions about this hive's own state or product,
// the persona, the configured org or repositories, or this meeting may not,
// unless the speaker asked for a web search by name.
func (g *WebGuard) Query(text string) (query string, ok bool) {
	c, blocked := g.check(text)
	return c.Query, blocked == ""
}
