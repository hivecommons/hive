package askai

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// DocsHost is the published docs site, the source of record.
	DocsHost = "docs.hivecommons.dev"
	// DocsHomeURL is the fallback link when no section page applies.
	DocsHomeURL = "https://" + DocsHost + "/"
	// githubHost and githubDocsRepoPrefix admit the GitHub docs path the
	// dashboard's own help marks point at (dashboardDocsHref).
	githubHost           = "github.com"
	githubDocsRepoPrefix = "/hivecommons/hive/"
	githubDocsPathMarker = "/src/docs/"

	// MaxSources caps the sources shown with one answer.
	MaxSources = 5
	// maxSourceTitleRunes caps a source title.
	maxSourceTitleRunes = 120
	// MaxAnswerRunes caps the answer text returned to the page.
	MaxAnswerRunes = 4000
	// answerTruncatedSuffix marks a capped answer.
	answerTruncatedSuffix = " [answer shortened]"
	// removedLinkText replaces a URL that is not on the allowlist.
	removedLinkText = "(link removed)"
)

// githubDocsKinds are the GitHub URL kinds that show a docs file.
var githubDocsKinds = []string{"blob/", "tree/"}

// Source is a citation that passed the allowlist.
type Source struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// AllowedSourceURL reports whether raw is an https link to the docs site or
// to a docs page under hivecommons/hive on GitHub, and returns it in a
// canonical form (no userinfo, no port, no query).
func AllowedSourceURL(raw string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Opaque != "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case DocsHost:
	case githubHost:
		rest, ok := strings.CutPrefix(u.Path, githubDocsRepoPrefix)
		if !ok {
			return "", false
		}
		kindOK := false
		for _, kind := range githubDocsKinds {
			if strings.HasPrefix(rest, kind) {
				kindOK = true
			}
		}
		if !kindOK || !strings.Contains(rest, githubDocsPathMarker) || strings.Contains(u.Path, "..") {
			return "", false
		}
	default:
		return "", false
	}
	clean := url.URL{Scheme: "https", Host: host, Path: u.Path, Fragment: u.Fragment}
	return clean.String(), true
}

// FilterSources keeps allowlisted, non-internal sources, deduplicated and
// capped at MaxSources.
func FilterSources(in []UpstreamSource) []Source {
	seen := map[string]bool{}
	var out []Source
	for _, s := range in {
		if s.Internal {
			continue
		}
		u, ok := AllowedSourceURL(s.URL)
		if !ok || seen[u] {
			continue
		}
		seen[u] = true
		title := cleanText(s.Title, maxSourceTitleRunes, "")
		if title == "" {
			title = u
		}
		out = append(out, Source{Title: title, URL: u})
		if len(out) == MaxSources {
			break
		}
	}
	return out
}

var (
	// markdownLinkPattern matches [text](target) and ![alt](target).
	markdownLinkPattern = regexp.MustCompile(`!?\[([^\]\n]*)\]\([^)\n]*\)`)
	// bareURLPattern matches a scheme URL in running text.
	bareURLPattern = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s<>()"']+`)
)

// SanitizeAnswer turns vendor answer text into plain text for the panel:
// control characters dropped, markdown links reduced to their text (links are
// shown only from the filtered sources), any URL not on the allowlist removed,
// and the length capped. The page renders the result as text, never as HTML.
func SanitizeAnswer(s string) string {
	s = markdownLinkPattern.ReplaceAllString(s, "$1")
	s = bareURLPattern.ReplaceAllStringFunc(s, func(m string) string {
		if u, ok := AllowedSourceURL(strings.TrimRight(m, ".,;:!?")); ok {
			return u
		}
		return removedLinkText
	})
	return cleanText(s, MaxAnswerRunes, answerTruncatedSuffix)
}

// cleanText drops control characters (keeping newlines and tabs in answers),
// trims, and caps at maxRunes with suffix appended when it cut.
func cleanText(s string, maxRunes int, suffix string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	var b strings.Builder
	keepLayout := suffix != ""
	for _, r := range s {
		if r == '\n' || r == '\t' {
			if keepLayout {
				b.WriteRune(r)
			} else {
				b.WriteRune(' ')
			}
			continue
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if utf8.RuneCountInString(out) <= maxRunes {
		return out
	}
	runes := []rune(out)
	return strings.TrimSpace(string(runes[:maxRunes])) + suffix
}
