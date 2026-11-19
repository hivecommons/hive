package inbox

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// DefaultTitlePrefixes are the prefixes the UI offers when a rule has none.
var DefaultTitlePrefixes = []string{"RFC:", "Hive:"}

// Rule is one auto-routing rule (mirrors config.InboxRule).
type Rule = config.InboxRule

// RuleMatches reports whether r applies to it: source (when set) must equal
// the item source, and at least one of title_prefix / tag must match.
// A rule with neither filter never matches, so a bad config cannot route
// every recording.
func RuleMatches(r Rule, it Item) bool {
	if src := strings.TrimSpace(r.Source); src != "" && !strings.EqualFold(src, it.Source) {
		return false
	}
	prefix := strings.TrimSpace(r.TitlePrefix)
	tag := strings.TrimSpace(r.Tag)
	if prefix == "" && tag == "" {
		return false
	}
	if prefix != "" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(it.Title)), strings.ToLower(prefix)) {
		return true
	}
	if tag != "" {
		for _, t := range it.Tags {
			if strings.EqualFold(strings.TrimSpace(t), tag) {
				return true
			}
		}
	}
	return false
}

// FirstRule returns the first rule matching it.
func FirstRule(rules []Rule, it Item) (Rule, bool) {
	for _, r := range rules {
		if RuleMatches(r, it) {
			return r, true
		}
	}
	return Rule{}, false
}

// StripPrefix removes a matched routing prefix ("RFC: Foo" → "Foo") so the
// filed title reads naturally; unmatched titles are returned trimmed.
func StripPrefix(title string, prefixes []string) string {
	t := strings.TrimSpace(title)
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p != "" && len(t) > len(p) && strings.EqualFold(t[:len(p)], p) {
			return strings.TrimSpace(t[len(p):])
		}
	}
	return t
}
