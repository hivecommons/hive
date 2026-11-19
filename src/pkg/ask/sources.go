package ask

import (
	"net/url"
	"strings"
	"unicode"
)

const (
	// DocsHost is the published docs site.
	DocsHost = "docs.hivecommons.dev"
	// githubHost and githubDocsPrefix name the one GitHub path help links
	// already point at (dashboardDocsHref): the docs under hivecommons/hive.
	githubHost = "github.com"

	maxSources     = 5
	maxTitleChars  = 120
	maxSourceChars = 512
)

// AllowedSourceURL reports whether raw is a link an answer may cite: https,
// no credentials, no explicit port, and either the docs site host or a docs
// page under hivecommons/hive on GitHub. Anything else, including a lookalike
// host, a javascript: or data: URL, or a path with a dot segment, is refused.
func AllowedSourceURL(raw string) bool {
	if raw == "" || len(raw) > maxSourceChars || strings.ContainsFunc(raw, unicode.IsControl) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || u.Port() != "" {
		return false
	}
	for _, seg := range strings.Split(u.Path, "/") {
		if seg == ".." || seg == "." {
			return false
		}
	}
	switch strings.ToLower(u.Hostname()) {
	case DocsHost:
		return true
	case githubHost:
		// /hivecommons/hive/(blob|tree)/<ref>/src/docs/...
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		return len(parts) >= 6 &&
			parts[0] == "hivecommons" && parts[1] == "hive" &&
			(parts[2] == "blob" || parts[2] == "tree") &&
			parts[3] != "" && parts[4] == "src" && parts[5] == "docs"
	}
	return false
}

// FilterSources keeps the allowlisted, de-duplicated sources of a vendor
// response, drops any marked as internal data, caps the count and cleans each
// title into plain text.
func FilterSources(in []VendorSource) []Source {
	var out []Source
	seen := map[string]bool{}
	for _, v := range in {
		if v.ContainsInternalData || !AllowedSourceURL(v.SourceURL) || seen[v.SourceURL] {
			continue
		}
		seen[v.SourceURL] = true
		title := PlainText(v.Title, maxTitleChars)
		if title == "" {
			title = v.SourceURL
		}
		out = append(out, Source{URL: v.SourceURL, Title: title})
		if len(out) == maxSources {
			break
		}
	}
	return out
}

// PlainText removes control characters (keeping newline and tab), collapses
// nothing else, and caps the result at max runes. The answer is returned as a
// JSON string and rendered as text by the panel; this is defence in depth, not
// HTML sanitising.
func PlainText(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r == '�' || (unicode.IsControl(r) && r != '\n' && r != '\t') {
			continue
		}
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}
