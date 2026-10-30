package prstage

import "strings"

// Defaults for the human-merge rule (hivecommons/hive#15442): a PR whose
// title starts with the breaking-change sign — ⚠️ (U+26A0 U+FE0F) or the
// bare ⚠ (U+26A0) — or that wears needs-owner-merge is never merged by a
// lane or a merge train; the owner merges it.
var (
	DefaultHumanMergeTitlePrefixes = []string{"\u26a0\ufe0f", "\u26a0"}
	DefaultHumanMergeLabels        = []string{"needs-owner-merge"}
)

// HumanMergeRule is the configured shape of the rule: the title prefixes
// and labels that park a PR for a human's merge. A zero value applies the
// defaults; an explicitly empty list switches that half of the rule off.
type HumanMergeRule struct {
	TitlePrefixes []string
	Labels        []string
}

// RequiresHumanMerge reports whether a PR with this title and these labels
// must wait for a person, and the sub-reason: "title" when a configured
// prefix opens the trimmed title, "label:<name>" when a configured label is
// present (case-insensitive). Nil slices mean the defaults.
func (r HumanMergeRule) RequiresHumanMerge(title string, labels []string) (bool, string) {
	prefixes := r.TitlePrefixes
	if prefixes == nil {
		prefixes = DefaultHumanMergeTitlePrefixes
	}
	trimmed := strings.TrimSpace(title)
	for _, p := range prefixes {
		p = strings.TrimSpace(p)
		if p != "" && strings.HasPrefix(trimmed, p) {
			return true, "title"
		}
	}
	want := r.Labels
	if want == nil {
		want = DefaultHumanMergeLabels
	}
	for _, w := range want {
		w = strings.TrimSpace(w)
		if w == "" {
			continue
		}
		for _, l := range labels {
			if strings.EqualFold(strings.TrimSpace(l), w) {
				return true, "label:" + strings.ToLower(w)
			}
		}
	}
	return false, ""
}
