// Package reach implements phase 2b of the PR reach telemetry epic (#3973,
// issue #3994): the PR→component mapping (design decision D3), deploy-window
// co-attribution (D4), the ancestry join between merged PRs and the commits
// hives actually report running, and the read-only /api/reach data model.
//
// The package is deliberately independent of phase 2a (#3993): everything the
// join needs from the fleet arrives through the narrow ReachReporter
// interface (see reporter.go), so this package compiles and tests green
// before the spoke-side counters merge.
package reach

import (
	"sort"
	"strings"
)

// ComponentUnattributable is the honest bucket of design decision D3
// (#3973/#3994): changed files that map to no instrumented component
// (workflows, deploy manifests, docs, scripts, bin, ...) land here and are
// ALWAYS reported as part of the per-PR attribution coverage — the
// unattributable share is never silently dropped.
const ComponentUnattributable = "unattributable"

// ComponentMain is the component name for the hub/spoke entrypoint. Files
// under src/cmd/hive/ belong to the binary's wiring rather than to a single
// package, and phase 1 span naming has no per-file granularity there, so D3
// maps the whole subtree to one "main" component.
const ComponentMain = "main"

// ComponentProxy is the component name for the MITM proxy subtree
// (src/proxy/), which is not laid out under src/pkg/ but is instrumented.
const ComponentProxy = "proxy"

// Path prefixes for the D3 mapping rules (#3994). These are repo-root
// relative paths exactly as the GitHub "list PR files" API returns them.
// #3996 renamed the v2/ source tree to src/, but a merged PR's file list is
// immutable history: PRs merged before the rename report v2/... paths from
// the GitHub API forever, so each rule keeps its pre-rename legacy alias —
// otherwise every pre-rename PR would collapse into the unattributable
// bucket.
const (
	pkgPrefix         = "src/pkg/"
	legacyPkgPrefix   = "v2/pkg/"
	mainPrefix        = "src/cmd/hive/"
	legacyMainPrefix  = "v2/cmd/hive/"
	proxyPrefix       = "src/proxy/"
	legacyProxyPrefix = "v2/proxy/"
)

// MapFile maps one changed-file path (repo-root relative, forward slashes —
// the form the GitHub PR files API returns) to its component per D3:
//
//	src/pkg/<name>/**  → <name>
//	src/cmd/hive/**    → "main"
//	src/proxy/**       → "proxy"
//	everything else    → ComponentUnattributable
//
// with the pre-#3996 v2/... spelling of each rule accepted as a legacy alias
// (see the prefix constants above).
//
// A file directly under src/pkg/ (no package subdirectory) has no component
// and is unattributable.
func MapFile(path string) string {
	switch {
	case strings.HasPrefix(path, mainPrefix), strings.HasPrefix(path, legacyMainPrefix):
		return ComponentMain
	case strings.HasPrefix(path, proxyPrefix), strings.HasPrefix(path, legacyProxyPrefix):
		return ComponentProxy
	case strings.HasPrefix(path, pkgPrefix), strings.HasPrefix(path, legacyPkgPrefix):
		rest := strings.TrimPrefix(strings.TrimPrefix(path, pkgPrefix), legacyPkgPrefix)
		name, _, found := strings.Cut(rest, "/")
		if !found || name == "" {
			// A file directly under src/pkg/ belongs to no package.
			return ComponentUnattributable
		}
		return name
	default:
		return ComponentUnattributable
	}
}

// Attribution is the D3 result for one PR's changed files. Coverage is the
// fraction of changed files that mapped to a component; the unattributable
// remainder is carried explicitly (files listed) so no caller can drop it
// without doing so on purpose.
type Attribution struct {
	// Components is the sorted, de-duplicated list of attributable
	// components the PR touched. ComponentUnattributable never appears here.
	Components []string `json:"components"`
	// UnattributableFiles lists the changed files that mapped to no
	// component, verbatim.
	UnattributableFiles []string `json:"unattributable_files,omitempty"`
	// FilesTotal / FilesAttributed give the raw counts behind Coverage.
	FilesTotal      int `json:"files_total"`
	FilesAttributed int `json:"files_attributed"`
	// Coverage = FilesAttributed / FilesTotal (0 when the PR changed no
	// files). Reported per PR, always — see D3 in #3973.
	Coverage float64 `json:"coverage"`
}

// Attribute maps a PR's changed files to components and computes attribution
// coverage per D3.
func Attribute(files []string) Attribution {
	attr := Attribution{FilesTotal: len(files)}
	seen := map[string]bool{}
	for _, f := range files {
		component := MapFile(f)
		if component == ComponentUnattributable {
			attr.UnattributableFiles = append(attr.UnattributableFiles, f)
			continue
		}
		attr.FilesAttributed++
		if !seen[component] {
			seen[component] = true
			attr.Components = append(attr.Components, component)
		}
	}
	sort.Strings(attr.Components)
	if attr.FilesTotal > 0 {
		attr.Coverage = float64(attr.FilesAttributed) / float64(attr.FilesTotal)
	}
	return attr
}
