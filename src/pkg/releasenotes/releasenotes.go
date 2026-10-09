// Package releasenotes computes the changelog gap between two repository
// revisions from the content of CHANGELOG.md and changelog.d/ fragments at
// each. It is pure (no I/O) so the dashboard endpoint and its tests can share
// it; fetching the files is the caller's job.
package releasenotes

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Categories lists the recognised changelog subsections in display order.
var Categories = []string{"added", "changed", "fixed", "security", "deprecated"}

// UnreleasedTitle names the synthetic section built from changelog.d fragments.
const UnreleasedTitle = "Unreleased (in this build)"

// DefaultMaxEntries caps the bullets returned for one response.
const DefaultMaxEntries = 400

// Section is one release's notes. Categories is keyed by lower-case
// subsection name (added, changed, fixed, security, deprecated).
type Section struct {
	Version    string              `json:"version"`
	Title      string              `json:"title,omitempty"`
	Date       string              `json:"date,omitempty"`
	Categories map[string][]string `json:"categories"`
}

// Fragment is one changelog.d file at a revision.
type Fragment struct {
	Name string
	Body string
}

// Result is the computed gap between two revisions.
type Result struct {
	Sections   []Section
	Unreleased *Section
	Truncated  bool
}

var (
	releaseHeader = regexp.MustCompile(`^##\s+(?:(\d{4}-\d{2}-\d{2})\s+)?\(v?([^)\s]+)\)\s*$`)
	h2Header      = regexp.MustCompile(`^##\s`)
	h3Header      = regexp.MustCompile(`^###\s+(.*?)\s*$`)
	bulletLine    = regexp.MustCompile(`^[-*]\s+(.*)$`)
)

func (s Section) entryCount() int {
	n := 0
	for _, b := range s.Categories {
		n += len(b)
	}
	return n
}

// Parse splits a CHANGELOG.md into release sections in file order. Headers
// that do not match `## <date> (vX.Y.Z)` (for example `## Unreleased` or a
// malformed one) end the current section and are skipped rather than failing
// the parse; text under unknown `###` subsections is ignored.
func Parse(changelog string) []Section {
	var out []Section
	var cur *Section
	category := ""
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
		category = ""
	}
	for _, line := range strings.Split(strings.ReplaceAll(changelog, "\r\n", "\n"), "\n") {
		if h2Header.MatchString(line) {
			flush()
			if m := releaseHeader.FindStringSubmatch(line); m != nil {
				cur = &Section{Version: "v" + m[2], Date: m[1], Categories: map[string][]string{}}
			}
			continue
		}
		if cur == nil {
			continue
		}
		if m := h3Header.FindStringSubmatch(line); m != nil {
			category = normalizeCategory(m[1])
			continue
		}
		if category == "" {
			continue
		}
		appendBullet(cur.Categories, category, line)
	}
	flush()
	return out
}

func normalizeCategory(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, c := range Categories {
		if name == c {
			return c
		}
	}
	return ""
}

// appendBullet adds a bullet line, or folds an indented continuation line into
// the previous bullet of the same category.
func appendBullet(cats map[string][]string, category, line string) {
	if m := bulletLine.FindStringSubmatch(line); m != nil {
		if text := strings.TrimSpace(m[1]); text != "" {
			cats[category] = append(cats[category], text)
		}
		return
	}
	trimmed := strings.TrimSpace(line)
	n := len(cats[category])
	if trimmed == "" || n == 0 || (line[0] != ' ' && line[0] != '\t') {
		return
	}
	cats[category][n-1] += " " + trimmed
}

// Diff returns the sections present in to and absent from from (matched by
// version), newest first.
func Diff(from, to []Section) []Section {
	have := make(map[string]bool, len(from))
	for _, s := range from {
		have[s.Version] = true
	}
	var out []Section
	seen := map[string]bool{}
	for _, s := range to {
		if have[s.Version] || seen[s.Version] {
			continue
		}
		seen[s.Version] = true
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return newer(out[i], out[j]) })
	return out
}

// newer orders by date then semver, both descending.
func newer(a, b Section) bool {
	if a.Date != b.Date {
		return a.Date > b.Date
	}
	return compareVersions(a.Version, b.Version) > 0
}

func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	var out []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			n = 0
		}
		out = append(out, n)
	}
	return out
}

// Latest returns the newest version in sections, or "" when there are none.
func Latest(sections []Section) string {
	best := ""
	var bs Section
	for _, s := range sections {
		if best == "" || newer(s, bs) {
			best, bs = s.Version, s
		}
	}
	return best
}

// FragmentCategory returns the changelog category named by a fragment file's
// prefix (`added-123-foo.md` -> added), or "" when it has none.
func FragmentCategory(name string) string {
	base := name
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	prefix, _, _ := strings.Cut(strings.TrimSuffix(base, ".md"), "-")
	return normalizeCategory(prefix)
}

// FragmentGap lists the fragments in to whose file name is not in fromNames,
// categorised by filename prefix, as the synthetic unreleased section. README
// files, non-markdown files, uncategorised names and empty bodies are skipped.
// It returns nil when no fragment qualifies.
func FragmentGap(fromNames []string, to []Fragment) *Section {
	have := make(map[string]bool, len(fromNames))
	for _, n := range fromNames {
		have[n] = true
	}
	sec := Section{Version: "unreleased", Title: UnreleasedTitle, Categories: map[string][]string{}}
	frags := append([]Fragment(nil), to...)
	sort.SliceStable(frags, func(i, j int) bool { return frags[i].Name < frags[j].Name })
	for _, f := range frags {
		if have[f.Name] || !strings.HasSuffix(f.Name, ".md") || strings.EqualFold(f.Name, "README.md") {
			continue
		}
		cat := FragmentCategory(f.Name)
		if cat == "" {
			continue
		}
		for _, line := range strings.Split(strings.ReplaceAll(f.Body, "\r\n", "\n"), "\n") {
			appendBullet(sec.Categories, cat, line)
		}
	}
	if sec.entryCount() == 0 {
		return nil
	}
	return &sec
}

// Truncate keeps at most max bullets across the sections in order, dropping
// sections that end up empty, and reports whether anything was cut. max <= 0
// means DefaultMaxEntries.
func Truncate(sections []Section, max int) ([]Section, bool) {
	if max <= 0 {
		max = DefaultMaxEntries
	}
	remaining := max
	truncated := false
	var out []Section
	for _, s := range sections {
		cut := Section{Version: s.Version, Title: s.Title, Date: s.Date, Categories: map[string][]string{}}
		for _, c := range Categories {
			bullets := s.Categories[c]
			if len(bullets) > remaining {
				bullets = bullets[:remaining]
				truncated = true
			}
			if len(bullets) > 0 {
				cut.Categories[c] = bullets
				remaining -= len(bullets)
			}
		}
		if cut.entryCount() > 0 {
			out = append(out, cut)
		}
	}
	return out, truncated
}

// Build computes the full gap. fromFragments and toFragments are only used
// when includeUnreleased is set (the target revision is untagged).
func Build(fromChangelog, toChangelog string, fromFragments []string, toFragments []Fragment, includeUnreleased bool, max int) Result {
	sections := Diff(Parse(fromChangelog), Parse(toChangelog))
	var unreleased *Section
	if includeUnreleased {
		unreleased = FragmentGap(fromFragments, toFragments)
	}
	all := sections
	if unreleased != nil {
		all = append([]Section{*unreleased}, sections...)
	}
	capped, truncated := Truncate(all, max)
	res := Result{Truncated: truncated}
	for _, s := range capped {
		if unreleased != nil && s.Title == UnreleasedTitle {
			s := s
			res.Unreleased = &s
			continue
		}
		res.Sections = append(res.Sections, s)
	}
	return res
}
