package codescanning

import (
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// MaxFilesPerIssue is the most files one new issue covers. A rule whose
// alerts span more files is split by directory (see Cluster).
const MaxFilesPerIssue = 5

// Group is the alerts one issue tracks.
type Group struct {
	Tool string
	// Rules are the rule IDs in the group: one for a code rule, one per
	// advisory (CVE) for a dependency group.
	Rules []string
	// Package is set for a dependency group (every alert names it).
	Package string
	Paths   []string
	Alerts  []Alert
}

// Level is the highest level in the group.
func (g Group) Level() Level {
	var l Level
	for _, a := range g.Alerts {
		l = max(l, a.Level())
	}
	return l
}

// WorkflowOnly reports whether every path is a GitHub Actions workflow file,
// so the fix needs the Workflows permission to push.
func (g Group) WorkflowOnly() bool {
	if len(g.Paths) == 0 {
		return false
	}
	for _, p := range g.Paths {
		if !strings.HasPrefix(p, ".github/workflows/") {
			return false
		}
	}
	return true
}

// Cluster groups new alerts into issues, each sized for one PR:
//
//   - A dependency alert (one whose message names a Package, as Trivy's do)
//     groups with every other alert on the same tool and package: one issue
//     per package, listing each CVE and each flagged artifact. Bumping the
//     package fixes them together.
//   - Any other alert groups by (tool, rule). When the rule's alerts span at
//     most MaxFilesPerIssue files they are one issue. Otherwise the files are
//     taken directory by directory (sorted) and packed into issues of at
//     most MaxFilesPerIssue files, never splitting a directory unless it
//     alone holds more than MaxFilesPerIssue files, which is then split into
//     runs of MaxFilesPerIssue files in path order. A repo-level alert (no
//     file) is its own one-alert-per-rule issue.
//
// The result is ordered highest level first, then tool, rule and first path,
// which is the order a per-pass cap files them in.
func Cluster(alerts []Alert) []Group {
	type key struct{ tool, rule, pkg string }
	buckets := map[key][]Alert{}
	var keys []key
	for _, a := range alerts {
		k := key{tool: a.Tool.Name, rule: a.Rule.ID}
		if d, ok := a.Dependency(); ok {
			k = key{tool: a.Tool.Name, pkg: d.Package}
		}
		if _, seen := buckets[k]; !seen {
			keys = append(keys, k)
		}
		buckets[k] = append(buckets[k], a)
	}
	var out []Group
	for _, k := range keys {
		as := buckets[k]
		sortAlerts(as)
		if k.pkg != "" {
			out = append(out, newGroup(k.tool, k.pkg, as))
			continue
		}
		for _, files := range packFiles(distinctPaths(as)) {
			in := map[string]bool{}
			for _, f := range files {
				in[f] = true
			}
			var sub []Alert
			for _, a := range as {
				if in[a.Path()] {
					sub = append(sub, a)
				}
			}
			out = append(out, newGroup(k.tool, "", sub))
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Level() != b.Level() {
			return a.Level() > b.Level()
		}
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		if ra, rb := strings.Join(a.Rules, ","), strings.Join(b.Rules, ","); ra != rb {
			return ra < rb
		}
		return strings.Join(a.Paths, ",") < strings.Join(b.Paths, ",")
	})
	return out
}

func newGroup(tool, pkg string, alerts []Alert) Group {
	g := Group{Tool: tool, Package: pkg, Alerts: alerts, Paths: distinctPaths(alerts)}
	seen := map[string]bool{}
	for _, a := range alerts {
		if !seen[a.Rule.ID] {
			seen[a.Rule.ID] = true
			g.Rules = append(g.Rules, a.Rule.ID)
		}
	}
	sort.Strings(g.Rules)
	return g
}

func distinctPaths(alerts []Alert) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range alerts {
		p := a.Path()
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// packFiles splits sorted files into bins (see Cluster). No files (all
// repo-level) is one empty bin, so a repo-level rule still gets an issue.
func packFiles(files []string) [][]string {
	if len(files) <= MaxFilesPerIssue {
		return [][]string{files}
	}
	var dirs []string
	byDir := map[string][]string{}
	for _, f := range files {
		d := path.Dir(f)
		if _, ok := byDir[d]; !ok {
			dirs = append(dirs, d)
		}
		byDir[d] = append(byDir[d], f)
	}
	sort.Strings(dirs)
	var out [][]string
	var bin []string
	flush := func() {
		if len(bin) > 0 {
			out = append(out, bin)
			bin = nil
		}
	}
	for _, d := range dirs {
		fs := byDir[d]
		if len(fs) > MaxFilesPerIssue {
			flush()
			for i := 0; i < len(fs); i += MaxFilesPerIssue {
				out = append(out, fs[i:min(i+MaxFilesPerIssue, len(fs))])
			}
			continue
		}
		if len(bin)+len(fs) > MaxFilesPerIssue {
			flush()
		}
		bin = append(bin, fs...)
	}
	flush()
	return out
}

// MarkerPrefix opens the hidden marker every code scanning issue carries.
const MarkerPrefix = "<!-- hive:code-scanning "

// Marker is the dedupe record in an issue body:
//
//	<!-- hive:code-scanning tool=CodeQL rule=cpp/path-injection paths=src/a.cc,src/b.cc alerts=12,15 -->
//
// Alerts is every alert number the issue has ever tracked; an alert number
// on any hive issue, open or closed, is never filed again. Values are
// query-escaped so a path cannot end the comment.
type Marker struct {
	Tool    string
	Rules   []string
	Package string
	Paths   []string
	Alerts  []int
}

var markerRE = regexp.MustCompile(`<!-- hive:code-scanning ([^>]*?) ?-->`)

// String renders the marker.
func (m Marker) String() string {
	esc := func(vs []string) string {
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = url.QueryEscape(v)
		}
		return strings.Join(out, ",")
	}
	nums := make([]string, len(m.Alerts))
	for i, n := range m.Alerts {
		nums[i] = strconv.Itoa(n)
	}
	s := MarkerPrefix + "tool=" + url.QueryEscape(m.Tool) + " rule=" + esc(m.Rules)
	if m.Package != "" {
		s += " package=" + url.QueryEscape(m.Package)
	}
	return s + " paths=" + esc(m.Paths) + " alerts=" + strings.Join(nums, ",") + " -->"
}

// ParseMarker reads the marker from an issue body.
func ParseMarker(body string) (Marker, bool) {
	sm := markerRE.FindStringSubmatch(body)
	if sm == nil {
		return Marker{}, false
	}
	var m Marker
	split := func(v string) []string {
		var out []string
		for _, p := range strings.Split(v, ",") {
			if u, err := url.QueryUnescape(p); err == nil && u != "" {
				out = append(out, u)
			}
		}
		return out
	}
	for _, field := range strings.Fields(sm[1]) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch k {
		case "tool":
			m.Tool, _ = url.QueryUnescape(v)
		case "rule":
			m.Rules = split(v)
		case "package":
			m.Package, _ = url.QueryUnescape(v)
		case "paths":
			m.Paths = split(v)
		case "alerts":
			for _, n := range strings.Split(v, ",") {
				if i, err := strconv.Atoi(n); err == nil && i > 0 {
					m.Alerts = append(m.Alerts, i)
				}
			}
		}
	}
	return m, len(m.Alerts) > 0
}

// covers reports whether a new alert belongs on the issue this marker is
// on: same tool and package for a dependency alert; otherwise same tool and
// rule in a file the issue already lists.
func (m Marker) covers(a Alert) bool {
	if !strings.EqualFold(m.Tool, a.Tool.Name) {
		return false
	}
	if d, ok := a.Dependency(); ok {
		return m.Package != "" && m.Package == d.Package
	}
	if m.Package != "" || !containsString(m.Rules, a.Rule.ID) {
		return false
	}
	p := a.Path()
	if p == "" {
		return len(m.Paths) == 0
	}
	return containsString(m.Paths, p)
}

func containsString(vs []string, s string) bool {
	for _, v := range vs {
		if v == s {
			return true
		}
	}
	return false
}
