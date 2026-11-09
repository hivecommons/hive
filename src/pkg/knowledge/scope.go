package knowledge

import (
	"path"
	"strings"
)

// FactScope says where a fact applies (hivecommons/hive#17045). It is the
// frontmatter `scope:` block of a knowledge entry:
//
//	scope:
//	  paths: [src/scripts/**, .github/scripts/integration-governance.cjs]
//	  labels: [area/ci]
//	  lanes: [developer, reviewer]
//
// Each dimension is a list; an empty list places no constraint on that
// dimension. A fact with no scope at all applies everywhere and is selected by
// relevance alone, exactly as before scopes existed.
type FactScope struct {
	// Paths are repo-relative globs; "**" matches any number of segments.
	Paths []string `json:"paths,omitempty"`
	// Labels are issue labels, compared case-insensitively.
	Labels []string `json:"labels,omitempty"`
	// Lanes are base agent / lane names, compared case-insensitively.
	Lanes []string `json:"lanes,omitempty"`
}

// IsZero reports whether the scope constrains nothing.
func (s *FactScope) IsZero() bool {
	return s == nil || (len(s.Paths) == 0 && len(s.Labels) == 0 && len(s.Lanes) == 0)
}

// KickContext is what the primer knows about the work a kick is for. The
// scheduler fills Lane and Labels from the lane name and the primed issues;
// Paths is filled only by callers that know which files are being touched.
type KickContext struct {
	Lane   string
	Labels []string
	Paths  []string
}

// IsZero reports whether the context carries nothing to match on.
func (kc KickContext) IsZero() bool {
	return kc.Lane == "" && len(kc.Labels) == 0 && len(kc.Paths) == 0
}

// Admits reports whether a fact carrying scope s may be shown for kc. An
// unscoped fact is always admitted. A scoped fact is admitted when at least one
// declared dimension matches — the dimensions are OR-ed, so an entry scoped to
// both a path glob and a label surfaces for either trigger. A declared
// dimension the context knows nothing about (no paths at kick time, say) simply
// cannot match; it does not admit by default, because the point of a scope is
// that the fact is not paid for on every kick.
func (s *FactScope) Admits(kc KickContext) bool {
	if s.IsZero() {
		return true
	}
	if kc.Lane != "" && containsFold(s.Lanes, kc.Lane) {
		return true
	}
	for _, l := range kc.Labels {
		if containsFold(s.Labels, l) {
			return true
		}
	}
	for _, p := range kc.Paths {
		for _, pattern := range s.Paths {
			if scopeGlobMatch(pattern, p) {
				return true
			}
		}
	}
	return false
}

// FilterByKickContext drops facts whose scope does not admit kc and reports how
// many it dropped. Unscoped facts pass unchanged; a zero context drops every
// scoped fact.
func FilterByKickContext(facts []Fact, kc KickContext) ([]Fact, int) {
	kept := facts[:0:0]
	dropped := 0
	for _, f := range facts {
		if f.Scope.Admits(kc) {
			kept = append(kept, f)
			continue
		}
		dropped++
	}
	return kept, dropped
}

// scopeGlobMatch matches a repo-relative path against a glob whose segments
// follow path.Match, with "**" matching zero or more whole segments. Both sides
// are normalised to forward slashes without a leading "./".
func scopeGlobMatch(pattern, p string) bool {
	pattern = normaliseScopePath(pattern)
	p = normaliseScopePath(p)
	if pattern == "" || p == "" {
		return false
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func normaliseScopePath(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\\", "/")
	s = strings.TrimPrefix(s, "./")
	return strings.Trim(s, "/")
}

func matchSegments(pattern, segs []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			// "**" at the end matches whatever remains, including nothing.
			if len(pattern) == 1 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pattern[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], segs[0]); err != nil || !ok {
			return false
		}
		pattern, segs = pattern[1:], segs[1:]
	}
	return len(segs) == 0
}

// parseScopeList parses the inline-list value of one scope key ("[a, b]" or a
// bare "a, b"); entries are trimmed and unquoted, empties dropped.
func parseScopeList(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	var out []string
	for _, item := range strings.Split(value, ",") {
		item = strings.Trim(strings.TrimSpace(item), "\"'")
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// setFromLine applies one trimmed frontmatter line inside a `scope:` block and
// reports whether the line was a scope key.
func (s *FactScope) setFromLine(line string) bool {
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return false
	}
	switch strings.TrimSpace(key) {
	case "paths":
		s.Paths = parseScopeList(value)
	case "labels":
		s.Labels = parseScopeList(value)
	case "lanes":
		s.Lanes = parseScopeList(value)
	default:
		return false
	}
	return true
}
