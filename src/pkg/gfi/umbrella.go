package gfi

import (
	"regexp"
	"strings"
	"unicode"
)

// Umbrella and owner-only issues (hivecommons/hive#19078).
//
// An umbrella (epic, tracker, decomposed parent) is coordination work: its
// children carry the code and only the owner or the hive can update or close
// it. A human-only issue waits on something only a person can do. Neither is
// work a contributor relay can finish, so neither is ever handed to one, and
// neither is advertised with `help wanted` / `good first issue`. Both stay
// actionable for the owner and the hive: this vocabulary is deliberately NOT
// the tracking vocabulary (`hive/tracking`, see tracking.go), because a
// tracking issue is a standing archive that no hive path may close, whereas an
// umbrella is closable (parent_autoclose, the owner, the tracker-closing
// sweep). Do not reuse TrackingLabel/TrackingMarker for umbrellas.
//
// Any one of these marks an issue an umbrella:
//
//   - a label from UmbrellaPolicy.Labels (default DefaultUmbrellaLabels),
//     matched case-insensitively and by last segment (`kind/epic`);
//   - UmbrellaMarker in the body;
//   - a title that opens with one of UmbrellaPolicy.TitlePrefixes (default
//     DefaultUmbrellaTitlePrefixes), after any leading emoji;
//   - a task list with at least TaskListMin entries that reference issues or
//     pull requests (`- [ ] #123`, `- [x] owner/repo#12`);
//   - sub-issue children (a decomposed parent).
//
// An issue is human-only when it carries a label from HumanOnlyLabels (and
// `<label>:*` reasons such as `human-only:cla`) or HumanOnlyMarker.
const (
	// UmbrellaMarker is the structured body marker for an umbrella issue.
	UmbrellaMarker = "<!-- hive:umbrella -->"
	// HumanOnlyMarker is the structured body marker for an owner-only issue.
	HumanOnlyMarker = "<!-- hive:human-only -->"

	// SkipUmbrella / SkipHumanOnly are the Decision.Skip prefixes (and the
	// contributor ladder's withheld-reason names) for the two exclusions.
	SkipUmbrella  = "umbrella"
	SkipHumanOnly = "human-only"

	// DefaultUmbrellaTaskListMin is how many issue-referencing checklist
	// entries mark an umbrella. One entry is a blocked-on note; two or more
	// is a list of children.
	DefaultUmbrellaTaskListMin = 2

	// umbrellaTitleLookahead bounds how far into a title a bracketed prefix
	// may start (leading emoji and punctuation are skipped first).
	umbrellaTitleLookahead = 8
)

var (
	// DefaultUmbrellaLabels is the accepted umbrella label set when a hive or
	// repo configures none.
	DefaultUmbrellaLabels = []string{"umbrella", "tracker", "epic", "meta-tracker"}
	// DefaultUmbrellaTitlePrefixes is the accepted title-prefix set when none
	// is configured. Compared lower-case against the title.
	DefaultUmbrellaTitlePrefixes = []string{"[tracking]", "[epic]", "[umbrella]", "[tracker]"}
	// DefaultHumanOnlyLabels is the accepted owner-only label set when none
	// is configured. `needs-human` is the hive's existing park label.
	DefaultHumanOnlyLabels = []string{"needs-human", "human-only"}

	umbrellaMarkerRE  = regexp.MustCompile(`<!--\s*hive:umbrella\s*-->`)
	humanOnlyMarkerRE = regexp.MustCompile(`<!--\s*hive:human-only\s*-->`)
	// codeSpanRE matches fenced blocks and inline code spans, so a marker that
	// is merely documented (an issue that explains `<!-- hive:umbrella -->`)
	// does not mark itself.
	codeSpanRE         = regexp.MustCompile("(?s)```.*?```|`[^`\\n]*`")
	umbrellaTaskListRE = regexp.MustCompile(`(?m)^\s*[-*]\s*\[[ xX]\]\s*(?:[\w.-]+/[\w.-]+)?#\d+`)
)

// UmbrellaPolicy is the tunable half of the umbrella / owner-only
// classification. The zero value is the default behaviour: enabled, default
// label and prefix lists, two-entry task lists, advertisement withdrawn. A
// per-repo override is another UmbrellaPolicy (see Merge).
type UmbrellaPolicy struct {
	// Disabled turns the whole classification off for the scope.
	Disabled bool
	// Labels replaces DefaultUmbrellaLabels when non-empty.
	Labels []string
	// TitlePrefixes replaces DefaultUmbrellaTitlePrefixes when non-empty.
	TitlePrefixes []string
	// TaskListMin replaces DefaultUmbrellaTaskListMin when positive.
	TaskListMin int
	// HumanOnlyLabels replaces DefaultHumanOnlyLabels when non-empty.
	HumanOnlyLabels []string
	// KeepAdvertisement stops the sweep from withdrawing `help wanted` /
	// `good first issue` from an umbrella. The default (false) withdraws.
	KeepAdvertisement bool
}

// Merge returns base with every field the override sets laid over it. A
// repo override that sets nothing inherits the hive-wide value.
func (base UmbrellaPolicy) Merge(over UmbrellaPolicy) UmbrellaPolicy {
	out := base
	if len(over.Labels) > 0 {
		out.Labels = over.Labels
	}
	if len(over.TitlePrefixes) > 0 {
		out.TitlePrefixes = over.TitlePrefixes
	}
	if over.TaskListMin > 0 {
		out.TaskListMin = over.TaskListMin
	}
	if len(over.HumanOnlyLabels) > 0 {
		out.HumanOnlyLabels = over.HumanOnlyLabels
	}
	return out
}

func (p UmbrellaPolicy) labels() []string {
	if len(p.Labels) > 0 {
		return p.Labels
	}
	return DefaultUmbrellaLabels
}

func (p UmbrellaPolicy) prefixes() []string {
	if len(p.TitlePrefixes) > 0 {
		return p.TitlePrefixes
	}
	return DefaultUmbrellaTitlePrefixes
}

func (p UmbrellaPolicy) taskListMin() int {
	if p.TaskListMin > 0 {
		return p.TaskListMin
	}
	return DefaultUmbrellaTaskListMin
}

func (p UmbrellaPolicy) humanOnlyLabels() []string {
	if len(p.HumanOnlyLabels) > 0 {
		return p.HumanOnlyLabels
	}
	return DefaultHumanOnlyLabels
}

// UmbrellaReason classifies an issue as an umbrella and says why:
// "label:<name>", "marker", "title-prefix:<prefix>", "task-list" or
// "sub-issues". children is the sub-issue count (0 when unknown).
func (p UmbrellaPolicy) UmbrellaReason(title string, labels []string, body string, children int) (string, bool) {
	if p.Disabled {
		return "", false
	}
	for _, l := range labels {
		if matchedLabel, ok := matchUmbrellaLabel(l, p.labels(), false); ok {
			return "label:" + matchedLabel, true
		}
	}
	if umbrellaMarkerRE.MatchString(codeSpanRE.ReplaceAllString(body, "")) {
		return "marker", true
	}
	if prefix, ok := umbrellaTitlePrefix(title, p.prefixes()); ok {
		return "title-prefix:" + prefix, true
	}
	// Count with a cap: only whether the threshold is reached matters.
	if need := p.taskListMin(); len(umbrellaTaskListRE.FindAllString(body, need)) >= need {
		return "task-list", true
	}
	if children > 0 {
		return "sub-issues", true
	}
	return "", false
}

// HumanOnlyReason classifies an issue as owner-only: "label:<name>" or
// "marker".
func (p UmbrellaPolicy) HumanOnlyReason(labels []string, body string) (string, bool) {
	if p.Disabled {
		return "", false
	}
	for _, l := range labels {
		if matchedLabel, ok := matchUmbrellaLabel(l, p.humanOnlyLabels(), true); ok {
			return "label:" + matchedLabel, true
		}
	}
	if humanOnlyMarkerRE.MatchString(codeSpanRE.ReplaceAllString(body, "")) {
		return "marker", true
	}
	return "", false
}

// matchUmbrellaLabel reports whether label is in set. Matching is
// case-insensitive; an umbrella label is also matched by its last
// `/`- or `:`-separated segment (`kind/epic`), and an owner-only label by its
// `<name>:<reason>` family (`human-only:cla`).
func matchUmbrellaLabel(label string, set []string, family bool) (string, bool) {
	l := strings.ToLower(strings.TrimSpace(label))
	if l == "" {
		return "", false
	}
	for _, s := range set {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if l == s || (family && strings.HasPrefix(l, s+":")) {
			return l, true
		}
	}
	if !family {
		if i := strings.LastIndexAny(l, "/:"); i >= 0 {
			seg := strings.TrimSpace(l[i+1:])
			for _, s := range set {
				if seg != "" && seg == strings.ToLower(strings.TrimSpace(s)) {
					return l, true
				}
			}
		}
	}
	return "", false
}

// umbrellaTitlePrefix reports the configured prefix a title opens with,
// skipping leading emoji and punctuation but not an opening bracket.
func umbrellaTitlePrefix(title string, prefixes []string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(title))
	runes := []rune(t)
	start := 0
	for start < len(runes) && start < umbrellaTitleLookahead {
		r := runes[start]
		if r == '[' || unicode.IsLetter(r) || unicode.IsDigit(r) {
			break
		}
		start++
	}
	t = string(runes[start:])
	for _, p := range prefixes {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" && strings.HasPrefix(t, p) {
			return p, true
		}
	}
	return "", false
}

// HasAdvertisement reports whether labels carry `help wanted` or
// `good first issue`: the public invitations a withdrawn umbrella loses.
func HasAdvertisement(labels []string) bool {
	set := lowerSet(labels)
	return set[Label] || set[HelpWantedLabel]
}

// UmbrellaSkip renders a Decision.Skip value for an umbrella reason.
func UmbrellaSkip(reason string) string { return SkipUmbrella + ":" + reason }

// UmbrellaReasonText renders an UmbrellaReason for a human-facing note.
func UmbrellaReasonText(why string) string {
	switch {
	case strings.HasPrefix(why, "label:"):
		return "it carries the `" + strings.TrimPrefix(why, "label:") + "` label"
	case why == "marker":
		return "it carries the `" + UmbrellaMarker + "` marker"
	case strings.HasPrefix(why, "title-prefix:"):
		return "its title opens with `" + strings.TrimPrefix(why, "title-prefix:") + "`"
	case why == "task-list":
		return "its body is a checklist of other issues or pull requests"
	case why == "sub-issues":
		return "it is a parent with sub-issues"
	}
	return "it is an umbrella issue"
}

// scoreUmbrella is Score's umbrella / owner-only gate. It reports a skipped
// Decision when the issue is an umbrella or owner-only for its repo's policy;
// WithdrawAd is set when the issue wears an invitation the policy withdraws.
func scoreUmbrella(c Candidate, p Policy, d Decision) (Decision, bool) {
	up := p.umbrellaPolicy(c.Repo)
	if up.Disabled {
		return d, false
	}
	advertised := !up.KeepAdvertisement && HasAdvertisement(c.Labels)
	if why, ok := up.UmbrellaReason(c.Title, c.Labels, c.Body, c.Children); ok {
		d.WithdrawAd = advertised
		return skip(d, UmbrellaSkip(why)), true
	}
	if why, ok := up.HumanOnlyReason(c.Labels, c.Body); ok {
		d.WithdrawAd = advertised
		// The hive's own park labels keep their long-standing skip name.
		if name := strings.TrimPrefix(why, "label:"); name != why && alwaysExcluded(name) {
			return skip(d, "hard-suppress:"+name), true
		}
		return skip(d, SkipHumanOnly+":"+why), true
	}
	return d, false
}

// alwaysExcluded reports whether label is one of AlwaysExcludeLabels.
func alwaysExcluded(label string) bool {
	for _, l := range AlwaysExcludeLabels {
		if strings.EqualFold(l, label) {
			return true
		}
	}
	return false
}
