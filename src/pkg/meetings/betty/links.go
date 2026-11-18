package betty

import (
	"regexp"
	"strconv"
	"strings"
)

// Links behind a spoken answer. A spoken answer never says a URL, so the
// room gets the pages it came from as one compact chat line right after it
// (live Meet test). Only pages of the context the model says it used are
// linked, and the hive's items only when the answer names them: nothing is
// made up.

// Link limits.
const (
	// maxLinks bounds the links of one answer.
	maxLinks = 5
	// maxLinkLabel bounds a link's label.
	maxLinkLabel = 60
	// hiveItemHost is where the hive's issues and PRs live.
	hiveItemHost = "https://github.com/"
)

// Link is one page behind an answer: a short label and its full URL.
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// itemRefInStatus is "org/repo#123", how the hive's live state names an
// open item.
var itemRefInStatus = regexp.MustCompile(`\b([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(\d{1,7})\b`)

// buildLinks is the links for an answer: the page of each context snippet
// listed in used that has one, and the hive items the answer names.
func buildLinks(answer string, used []string, snips []Snippet) []Link {
	var out []Link
	seen := map[string]bool{}
	add := func(label, u string) {
		if u == "" || seen[u] || len(out) >= maxLinks {
			return
		}
		seen[u] = true
		out = append(out, Link{Label: clip(stripHTML(label), maxLinkLabel), URL: u})
	}
	for _, u := range used {
		i, err := strconv.Atoi(strings.Trim(strings.TrimSpace(u), `"[]`))
		if err != nil || i < 1 || i > len(snips) {
			continue
		}
		if s := snips[i-1]; s.Source != HiveStatusSource && httpsURL(s.URL) {
			label := s.Title
			if label == "" {
				label = s.Source
			}
			add(label, s.URL)
		}
	}
	for _, s := range snips {
		if s.Source != HiveStatusSource {
			continue
		}
		for _, ref := range namedItems(answer, s.Text) {
			add(ref, hiveItemHost+strings.Replace(ref, "#", "/issues/", 1))
		}
	}
	return out
}

// httpsURL reports whether u is an absolute https URL.
func httpsURL(u string) bool {
	return strings.HasPrefix(u, "https://") && !strings.ContainsAny(u, " \t\r\n")
}

// namedItems are the "org/repo#N" items of the hive's state that the answer
// names, by full ref or by number ("PR 123", "issue #123") when only one
// item of the state has that number.
func namedItems(answer, status string) []string {
	byNumber := map[string][]string{}
	var order []string
	for _, m := range itemRefInStatus.FindAllStringSubmatch(status, -1) {
		ref := m[1] + "#" + m[2]
		if len(byNumber[m[2]]) == 0 {
			order = append(order, m[2])
		}
		dup := false
		for _, r := range byNumber[m[2]] {
			dup = dup || r == ref
		}
		if !dup {
			byNumber[m[2]] = append(byNumber[m[2]], ref)
		}
	}
	var out []string
	for _, n := range order {
		refs := byNumber[n]
		named := regexp.MustCompile(`(?i)(?:#|\b(?:pull requests?|prs?|issues?|number)\s*)` + n + `\b`).MatchString(NormalizeDomain(answer))
		for _, ref := range refs {
			if strings.Contains(answer, ref) || (named && len(refs) == 1) {
				out = append(out, ref)
			}
		}
	}
	return out
}
