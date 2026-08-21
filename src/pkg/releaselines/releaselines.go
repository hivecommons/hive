// Package releaselines checks that the branch names hand-written into this
// repository's CI configuration still agree with the repository's live release
// lines.
//
// WHY THIS EXISTS (#4405, under #4188). Nine workflows pin their triggers to a
// hand-maintained list of version-branch names, and docker.yml hand-writes the
// same names again in its push policy. Nothing checked that those lists agreed
// with reality, so when mainline moved from v2 to v4 the Podman contract gate
// kept naming v2 alone and simply stopped running (#4339) — "a guard that never
// executes reports green forever, which is the one failure mode a guard cannot
// have". Adding v4 to that one list fixed the instance, not the class: the day
// v5 is cut, both CI workflows, both security contract gates and all five
// Podman lanes would report nothing at all. Not red. Absent.
//
// .github/release-lines.yaml is the source of truth; this package is what makes
// it load-bearing. Cutting a release line is one edit there, and this check then
// names every file that has not caught up — in both directions, because it
// equally reports a workflow still pinned to a line that has been retired.
//
// The check parses the workflow YAML rather than pattern-matching its text, so
// both spellings of the filter are handled without special cases: the flow
// sequence `branches: [v2, v4]` used by v2-ci.yml and v2-tests.yml, and the
// block sequence everything else uses, are the same node tree once parsed.
package releaselines

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// ConfigPath is the source of truth, relative to the repository root.
	ConfigPath = ".github/release-lines.yaml"
	// WorkflowDir holds the workflows checked against it.
	WorkflowDir = ".github/workflows"
)

// Statuses a release line may declare. Unknown values are rejected rather than
// ignored: a typo in `undecided` would silently switch off the notice that
// keeps an open maintainer question visible, which is the one thing this field
// is for.
const (
	StatusCurrent   = "current"   // the line mainline is cut from
	StatusSupported = "supported" // still built and still gated
	StatusUndecided = "undecided" // support status is an open maintainer question
)

var knownStatuses = map[string]bool{
	StatusCurrent:   true,
	StatusSupported: true,
	StatusUndecided: true,
}

// Line is one release line: a long-lived version branch.
type Line struct {
	Branch string `yaml:"branch"`
	Status string `yaml:"status"`
	Note   string `yaml:"note"`
}

// Workflow declares how one workflow's trigger relates to the release lines.
//
// Pinned true means the trigger filter is a list of literal branch names, and
// must therefore name every release line (bar those listed in Omits) and
// nothing else (bar those listed in Extra). Pinned false means it is a glob,
// which cannot fall behind a new release line; Reason records why that is the
// right shape for that file.
type Workflow struct {
	File   string   `yaml:"file"`
	Pinned bool     `yaml:"pinned"`
	Extra  []string `yaml:"extra"`
	Omits  []string `yaml:"omits"`
	Reason string   `yaml:"reason"`
	Note   string   `yaml:"note"`
}

// BranchList declares a hand-written list of release-line branch names that is
// not a trigger — docker.yml's LONG_LIVED push policy is the motivating case.
type BranchList struct {
	File  string   `yaml:"file"`
	Env   string   `yaml:"env"`
	Extra []string `yaml:"extra"`
	Omits []string `yaml:"omits"`
	Note  string   `yaml:"note"`
}

// Config is the parsed contents of ConfigPath.
type Config struct {
	ReleaseLines []Line       `yaml:"release_lines"`
	Workflows    []Workflow   `yaml:"workflows"`
	BranchLists  []BranchList `yaml:"branch_lists"`
}

// Severity distinguishes a failure from something merely worth saying.
type Severity string

const (
	// Error fails the check.
	Error Severity = "error"
	// Notice is reported on every run and never fails the check.
	Notice Severity = "notice"
)

// Finding is one thing the check has to say about one file.
type Finding struct {
	Severity Severity
	File     string // repo-relative, or "" when the finding is about the config itself
	Where    string // e.g. "on.push.branches", "env LONG_LIVED"
	Message  string
}

func (f Finding) String() string {
	loc := f.File
	if loc == "" {
		loc = ConfigPath
	}
	if f.Where != "" {
		loc += " (" + f.Where + ")"
	}
	return loc + ": " + f.Message
}

// Errors returns only the findings that fail the check.
func Errors(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Severity == Error {
			out = append(out, f)
		}
	}
	return out
}

// Notices returns only the findings that are reported but do not fail.
func Notices(findings []Finding) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Severity == Notice {
			out = append(out, f)
		}
	}
	return out
}

// Load reads and parses ConfigPath under repoRoot.
func Load(repoRoot string) (*Config, error) {
	path := filepath.Join(repoRoot, filepath.FromSlash(ConfigPath))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ConfigPath, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigPath, err)
	}
	return &cfg, nil
}

// Check reports every way the repository's CI configuration has fallen out of
// sync with its declared release lines. An empty Errors() result means in sync.
func Check(repoRoot string) ([]Finding, error) {
	cfg, err := Load(repoRoot)
	if err != nil {
		return nil, err
	}
	return CheckConfig(repoRoot, cfg), nil
}

// CheckConfig is Check against an already-loaded config, so a caller can ask
// what WOULD be reported for a hypothetical set of release lines.
func CheckConfig(repoRoot string, cfg *Config) []Finding {
	var out []Finding
	out = append(out, cfg.validate()...)

	lines := map[string]bool{}
	for _, l := range cfg.ReleaseLines {
		lines[l.Branch] = true
	}

	declared := map[string]bool{}
	for _, wf := range cfg.Workflows {
		declared[wf.File] = true
		out = append(out, checkWorkflow(repoRoot, wf, cfg.ReleaseLines, lines)...)
	}
	out = append(out, undeclaredWorkflows(repoRoot, declared)...)

	for _, bl := range cfg.BranchLists {
		out = append(out, checkBranchList(repoRoot, bl, cfg.ReleaseLines, lines)...)
	}
	return out
}

// validate checks the source of truth against itself, and raises the notices
// that keep an undecided line visible.
func (c *Config) validate() []Finding {
	var out []Finding
	if len(c.ReleaseLines) == 0 {
		out = append(out, Finding{Severity: Error, Message: "no release_lines declared; every pinned workflow would be reported as naming branches that are not release lines"})
	}
	seen := map[string]bool{}
	for _, l := range c.ReleaseLines {
		switch {
		case strings.TrimSpace(l.Branch) == "":
			out = append(out, Finding{Severity: Error, Message: "a release_lines entry has an empty branch"})
			continue
		case seen[l.Branch]:
			out = append(out, Finding{Severity: Error, Message: fmt.Sprintf("release line %q is declared twice", l.Branch)})
			continue
		}
		seen[l.Branch] = true
		if !knownStatuses[l.Status] {
			out = append(out, Finding{Severity: Error, Message: fmt.Sprintf(
				"release line %q has status %q; expected one of %s", l.Branch, l.Status, strings.Join(sortedKeys(knownStatuses), ", "))})
			continue
		}
		if l.Status == StatusUndecided {
			out = append(out, Finding{Severity: Notice, Message: fmt.Sprintf(
				"release line %q is marked %s — %s", l.Branch, StatusUndecided, flatten(l.Note))})
		}
	}
	seenWF := map[string]bool{}
	for _, wf := range c.Workflows {
		if seenWF[wf.File] {
			out = append(out, Finding{Severity: Error, Message: fmt.Sprintf("workflow %q is declared twice", wf.File)})
		}
		seenWF[wf.File] = true
		if !wf.Pinned && strings.TrimSpace(wf.Reason) == "" {
			out = append(out, Finding{Severity: Error, File: workflowPath(wf.File), Message: "declared `pinned: false` with no `reason`; an unpinned trigger has to say why widening is deliberate"})
		}
	}
	return out
}

func checkWorkflow(repoRoot string, wf Workflow, releaseLines []Line, lines map[string]bool) []Finding {
	rel := workflowPath(wf.File)
	path := filepath.Join(repoRoot, filepath.FromSlash(rel))

	filters, err := branchFilters(path)
	if err != nil {
		return []Finding{{Severity: Error, File: rel, Message: err.Error()}}
	}
	if len(filters) == 0 {
		return []Finding{{Severity: Error, File: rel, Message: fmt.Sprintf(
			"declared in %s but has no branch filter on push/pull_request/pull_request_target; a workflow with no filter runs on every branch and cannot fall out of sync, so remove the entry", ConfigPath)}}
	}

	var out []Finding
	union := map[string]bool{}
	for _, f := range filters {
		for _, v := range f.values {
			union[v] = true
		}
		if wf.Pinned {
			out = append(out, checkPinnedFilter(rel, wf, f, releaseLines, lines)...)
			continue
		}
		if !anyPattern(f.values) {
			out = append(out, Finding{Severity: Error, File: rel, Where: f.path, Message: fmt.Sprintf(
				"declared `pinned: false` but the filter names only literal branches (%s); a literal list can fall behind a new release line, so declare it `pinned: true`", strings.Join(f.values, ", "))})
		}
	}
	out = append(out, checkDeclarations(rel, wf, union, lines)...)
	return out
}

// checkPinnedFilter is the core assertion: a literal branch list must name
// every release line, and must not name anything else.
func checkPinnedFilter(rel string, wf Workflow, f filter, releaseLines []Line, lines map[string]bool) []Finding {
	var out []Finding
	have := map[string]bool{}
	for _, v := range f.values {
		have[v] = true
		if isPattern(v) {
			out = append(out, Finding{Severity: Error, File: rel, Where: f.path, Message: fmt.Sprintf(
				"declared `pinned: true` but the filter contains the pattern %q; either spell the branches out or declare it `pinned: false` with a reason", v)})
		}
	}
	omits := setOf(wf.Omits)
	extra := setOf(wf.Extra)

	for _, l := range releaseLines {
		if have[l.Branch] || omits[l.Branch] {
			continue
		}
		out = append(out, Finding{Severity: Error, File: rel, Where: f.path, Message: fmt.Sprintf(
			"release line %q is missing; this workflow does not run on it at all. Add it to the filter, or declare `omits: [%s]` in %s with a note saying why it is deliberate", l.Branch, l.Branch, ConfigPath)})
	}
	for _, v := range f.values {
		if isPattern(v) || lines[v] || extra[v] {
			continue
		}
		out = append(out, Finding{Severity: Error, File: rel, Where: f.path, Message: fmt.Sprintf(
			"names %q, which is not a release line in %s; remove it from the filter, or declare `extra: [%s]` there", v, ConfigPath, v)})
	}
	return out
}

// checkDeclarations keeps the escape hatches from rotting: an `omits` or
// `extra` that no longer describes the file is itself out of sync.
func checkDeclarations(rel string, wf Workflow, union map[string]bool, lines map[string]bool) []Finding {
	var out []Finding
	for _, o := range wf.Omits {
		if !lines[o] {
			out = append(out, Finding{Severity: Error, File: rel, Message: fmt.Sprintf(
				"declares `omits: [%s]` but %q is not a release line; drop the declaration", o, o)})
			continue
		}
		if union[o] {
			out = append(out, Finding{Severity: Error, File: rel, Message: fmt.Sprintf(
				"declares `omits: [%s]` but the filter does name %s; drop the declaration", o, o)})
		}
	}
	for _, e := range wf.Extra {
		if !union[e] {
			out = append(out, Finding{Severity: Error, File: rel, Message: fmt.Sprintf(
				"declares `extra: [%s]` but the filter does not name it; drop the declaration", e)})
		}
	}
	return out
}

// undeclaredWorkflows catches the next instance of this bug rather than the
// current one: a workflow added later that pins branch names and is never
// declared would otherwise be invisible to the check that exists to find it.
func undeclaredWorkflows(repoRoot string, declared map[string]bool) []Finding {
	dir := filepath.Join(repoRoot, filepath.FromSlash(WorkflowDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []Finding{{Severity: Error, File: WorkflowDir, Message: fmt.Sprintf("read workflow directory: %v", err)}}
	}
	var out []Finding
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml")) {
			continue
		}
		if declared[name] {
			continue
		}
		filters, err := branchFilters(filepath.Join(dir, name))
		if err != nil {
			out = append(out, Finding{Severity: Error, File: workflowPath(name), Message: err.Error()})
			continue
		}
		if len(filters) == 0 {
			continue
		}
		out = append(out, Finding{Severity: Error, File: workflowPath(name), Where: filters[0].path, Message: fmt.Sprintf(
			"has a branch filter (%s) but is not declared in %s; every workflow that names branches has to be checked against the release lines, or it is the next one to silently stop running",
			strings.Join(filters[0].values, ", "), ConfigPath)})
	}
	return out
}

func checkBranchList(repoRoot string, bl BranchList, releaseLines []Line, lines map[string]bool) []Finding {
	rel := workflowPath(bl.File)
	path := filepath.Join(repoRoot, filepath.FromSlash(rel))
	where := "env " + bl.Env

	raw, err := envValue(path, bl.Env)
	if err != nil {
		return []Finding{{Severity: Error, File: rel, Where: where, Message: err.Error()}}
	}
	values := strings.Fields(raw)
	have := setOf(values)
	omits := setOf(bl.Omits)
	extra := setOf(bl.Extra)

	var out []Finding
	for _, l := range releaseLines {
		if have[l.Branch] || omits[l.Branch] {
			continue
		}
		out = append(out, Finding{Severity: Error, File: rel, Where: where, Message: fmt.Sprintf(
			"release line %q is missing from the list (%s). Add it, or declare `omits: [%s]` in %s", l.Branch, raw, l.Branch, ConfigPath)})
	}
	for _, v := range values {
		if lines[v] || extra[v] {
			continue
		}
		out = append(out, Finding{Severity: Error, File: rel, Where: where, Message: fmt.Sprintf(
			"names %q, which is not a release line in %s; remove it, or declare `extra: [%s]` there", v, ConfigPath, v)})
	}
	for _, e := range bl.Extra {
		if !have[e] {
			out = append(out, Finding{Severity: Error, File: rel, Where: where, Message: fmt.Sprintf(
				"declares `extra: [%s]` but the list does not name it; drop the declaration", e)})
		}
	}
	for _, o := range bl.Omits {
		if have[o] {
			out = append(out, Finding{Severity: Error, File: rel, Where: where, Message: fmt.Sprintf(
				"declares `omits: [%s]` but the list does name it; drop the declaration", o)})
		}
	}
	return out
}

// ── YAML ─────────────────────────────────────────────────────────────────────

// filter is one branch filter found in a workflow's trigger block.
type filter struct {
	path   string // e.g. "on.push.branches"
	values []string
}

// triggerEvents are the events whose branch filter decides whether a workflow
// runs on a release line at all.
var triggerEvents = []string{"push", "pull_request", "pull_request_target"}

// branchFilters returns every branches / branches-ignore filter in the
// workflow's trigger block.
//
// The document is walked as a node tree rather than decoded into a struct for
// two reasons. `on` is a YAML 1.1 boolean, so a decode can turn the key into
// `true` depending on the library's schema; walking sees the key exactly as it
// is written. And the two spellings of a branch list — `[v2, v4]` and a block
// sequence — are the same SequenceNode once parsed, so both are handled with no
// special case, which is what #4405 asks for.
func branchFilters(path string) ([]filter, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("declared in %s but does not exist", ConfigPath)
		}
		return nil, fmt.Errorf("read: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	root := documentRoot(&doc)
	if root == nil {
		return nil, nil
	}
	on := mapValue(root, "on")
	if on == nil || on.Kind != yaml.MappingNode {
		// `on: push` or `on: [push]` carries no branch filter.
		return nil, nil
	}
	var out []filter
	for _, event := range triggerEvents {
		ev := mapValue(on, event)
		if ev == nil || ev.Kind != yaml.MappingNode {
			continue
		}
		for _, key := range []string{"branches", "branches-ignore"} {
			node := mapValue(ev, key)
			if node == nil {
				continue
			}
			out = append(out, filter{path: "on." + event + "." + key, values: scalars(node)})
		}
	}
	return out, nil
}

// envValue finds a value in any `env:` mapping in the document — the workflow's
// own, a job's, or a step's.
func envValue(path, name string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("declared in %s but does not exist", ConfigPath)
		}
		return "", fmt.Errorf("read: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	root := documentRoot(&doc)
	if root == nil {
		return "", fmt.Errorf("declared in %s but the file is empty", ConfigPath)
	}
	if v, ok := findEnv(root, name); ok {
		return v, nil
	}
	return "", fmt.Errorf("declared in %s as holding a release-line list, but no `env:` block sets %s; if it was renamed, rename it there too", ConfigPath, name)
}

func findEnv(n *yaml.Node, name string) (string, bool) {
	if n.Kind == yaml.MappingNode {
		if env := mapValue(n, "env"); env != nil && env.Kind == yaml.MappingNode {
			if v := mapValue(env, name); v != nil && v.Kind == yaml.ScalarNode {
				return v.Value, true
			}
		}
	}
	for _, c := range n.Content {
		if v, ok := findEnv(c, name); ok {
			return v, true
		}
	}
	return "", false
}

func documentRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		doc = doc.Content[0]
	}
	if doc.Kind != yaml.MappingNode {
		return nil
	}
	return doc
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalars(n *yaml.Node) []string {
	switch n.Kind {
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		out := make([]string, 0, len(n.Content))
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode {
				out = append(out, c.Value)
			}
		}
		return out
	default:
		return nil
	}
}

// ── small helpers ────────────────────────────────────────────────────────────

// isPattern reports whether a filter entry is a glob rather than a literal
// branch name, using GitHub's filter-pattern metacharacters.
func isPattern(s string) bool {
	return strings.HasPrefix(s, "!") || strings.ContainsAny(s, "*?+[]")
}

func anyPattern(values []string) bool {
	for _, v := range values {
		if isPattern(v) {
			return true
		}
	}
	return false
}

func setOf(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[v] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func workflowPath(file string) string {
	return WorkflowDir + "/" + file
}

// flatten collapses a folded YAML note to one line so it survives being emitted
// as a GitHub Actions annotation.
func flatten(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
