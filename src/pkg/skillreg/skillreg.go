// Package skillreg is a local, shareable registry of named, versioned agent
// "skills" plus a minimal bring-your-own-agent (BYO) contract. It extends the
// agentsmd package, which parses skills inline in a single AGENTS.md, into a
// reusable registry so skills can be named, versioned, listed, searched, and
// resolved across repos and agents — the seed of a community catalog.
//
// # Relationship to agentsmd
//
// agentsmd.AgentsConfig carries skills as a flat map[name]body, defined inline
// in one AGENTS.md (or adjacent skills/*.md files) with no version or metadata.
// This package does not re-parse AGENTS.md; instead it holds richer Skill values
// (name, version, description, tags, source) and provides a bridge
// (ResolveRequested) that folds an AgentsConfig's inline skills together with
// registry skills under a documented precedence.
//
// # Skill file format
//
// A registry loads skills from a directory of "<file>.md" files. Each file may
// carry optional YAML front-matter delimited by "---" lines at the very top:
//
//	---
//	name: go-testing
//	version: 1.2.0
//	description: How this repo writes Go tests.
//	tags: [go, testing]
//	---
//	Prefer t.TempDir over manual temp dirs.
//	Never hit the network in unit tests.
//
// The text after the front-matter is the skill body. When "name" is omitted it
// defaults to the file's base name (without ".md"). When "version" is omitted it
// defaults to DefaultVersion ("0.0.0"). Files are tolerated: a file that cannot
// be read or whose front-matter is malformed is skipped (with a log line), never
// fatal, so one bad file cannot break a load.
//
// # Precedence (ResolveRequested)
//
// For each requested name, the registry is consulted first (a curated,
// versioned, shareable definition wins), and the AgentsConfig's inline skill is
// used only as a fallback when the registry has no such skill. This lets a
// shared catalog override ad-hoc inline snippets while still honoring repo-local
// skills the catalog has never heard of.
//
// # Agent SDK contract (BYO-agent)
//
// AgentSpec is the minimal contract a third party implements (or declares in
// YAML) to plug a custom agent into Hive: a name, a backend identifier, a model,
// an operating mode/policy, and a list of default skills. LoadAgentSpec reads
// such a declaration from YAML. The contract is intentionally small and stable.
//
// # Concurrency
//
// Registry is safe for concurrent use by multiple goroutines.
package skillreg

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/kubestellar/hive/pkg/agentsmd"
)

const (
	// SkillFileExt is the extension for registry skill files.
	SkillFileExt = ".md"

	// DefaultVersion is assigned to a skill whose file omits "version".
	DefaultVersion = "0.0.0"

	// frontMatterDelim opens and closes the optional YAML front-matter block.
	frontMatterDelim = "---"

	// injectionHeader titles the resolved-skills block produced by InjectionText.
	injectionHeader = "## Requested Skills"

	// versionWildcard, as a constraint, matches any version.
	versionWildcard = "*"

	// versionPrefixCaret requests the newest version sharing a major with the
	// constraint (semver-caret style, e.g. "^1.2.0").
	versionPrefixCaret = "^"

	// versionPrefixGTE requests the newest version at or above the constraint.
	versionPrefixGTE = ">="
)

// SkillSource identifies where a Skill came from, for provenance and precedence.
type SkillSource string

const (
	// SourceRepo means the skill was loaded from a repository's AGENTS.md or
	// skills/ directory (an inline or repo-local definition).
	SourceRepo SkillSource = "repo"

	// SourceBuiltin means the skill was contributed programmatically (e.g. a
	// curated default baked into a binary).
	SourceBuiltin SkillSource = "builtin"

	// SourceURLLocalFile means the skill was loaded from a local skill file on
	// disk (the registry's own skills/ directory).
	SourceURLLocalFile SkillSource = "url-local-file"
)

// Skill is a named, versioned instruction snippet plus metadata.
type Skill struct {
	// Name is the stable identifier used to request the skill. Required.
	Name string
	// Version is a dotted numeric version ("major.minor.patch"); missing parts
	// read as zero. Defaults to DefaultVersion.
	Version string
	// Description is a one-line human summary.
	Description string
	// Body is the instruction text injected into an agent's context.
	Body string
	// Source records provenance (see SkillSource).
	Source SkillSource
	// Tags are free-form labels for Search.
	Tags []string
}

// skillFrontMatter is the subset of a skill file's front-matter the loader reads.
type skillFrontMatter struct {
	Name        string   `yaml:"name"`
	Version     string   `yaml:"version"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags"`
}

// logger returns lg or a discarding default so callers may pass nil.
func logger(lg *slog.Logger) *slog.Logger {
	if lg != nil {
		return lg
	}
	return slog.New(slog.DiscardHandler)
}

// Registry is a concurrency-safe collection of skills keyed by name. When more
// than one version of a name is added, all are retained so a version constraint
// can select among them; Get and unconstrained resolution return the highest
// version.
type Registry struct {
	mu sync.RWMutex
	// byName maps a skill name to its versions, unordered.
	byName map[string][]Skill
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string][]Skill{}}
}

// Add inserts or replaces a skill. A skill with the same name and version as an
// existing entry replaces it; otherwise it is added as an additional version.
// A skill with an empty Name is rejected.
func (r *Registry) Add(s Skill) error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("skillreg: skill has empty name")
	}
	if strings.TrimSpace(s.Version) == "" {
		s.Version = DefaultVersion
	}
	if s.Source == "" {
		s.Source = SourceBuiltin
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	versions := r.byName[s.Name]
	for i, existing := range versions {
		if existing.Version == s.Version {
			versions[i] = s
			r.byName[s.Name] = versions
			return nil
		}
	}
	r.byName[s.Name] = append(versions, s)
	return nil
}

// Load reads every "<file>.md" in dir as a skill and Adds it. Missing or
// unreadable directories, unreadable files, and files with malformed
// front-matter are tolerated (logged and skipped); Load returns an error only
// for a truly unexpected condition, never for a bad individual file. The number
// of skills successfully loaded is returned.
func (r *Registry) Load(dir string, lg *slog.Logger) (int, error) {
	lg = logger(lg)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// No directory — nothing to load, not an error.
			return 0, nil
		}
		lg.Warn("skillreg: cannot read skills dir, skipping", "dir", dir, "error", err)
		return 0, nil
	}
	loaded := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), SkillFileExt) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		raw, rErr := os.ReadFile(path)
		if rErr != nil {
			lg.Warn("skillreg: cannot read skill file, skipping", "file", e.Name(), "error", rErr)
			continue
		}
		skill := parseSkillFile(e.Name(), string(raw), lg)
		if skill.Name == "" {
			// Unrecoverable (should not happen: name defaults to file base).
			continue
		}
		if aErr := r.Add(skill); aErr != nil {
			lg.Warn("skillreg: cannot add skill, skipping", "file", e.Name(), "error", aErr)
			continue
		}
		loaded++
	}
	return loaded, nil
}

// parseSkillFile turns a single skill file's contents into a Skill. It never
// fails: malformed front-matter is logged and skipped, and name/version default
// sensibly. fileName is used to default the skill name.
func parseSkillFile(fileName, content string, lg *slog.Logger) Skill {
	base := strings.TrimSuffix(fileName, SkillFileExt)
	s := Skill{
		Name:    base,
		Version: DefaultVersion,
		Body:    strings.TrimSpace(content),
		Source:  SourceURLLocalFile,
	}

	fm, rest := splitFrontMatter(content)
	if fm != "" {
		var meta skillFrontMatter
		if yErr := yaml.Unmarshal([]byte(fm), &meta); yErr != nil {
			lg.Warn("skillreg: malformed skill front-matter, using defaults", "file", fileName, "error", yErr)
		} else {
			if n := strings.TrimSpace(meta.Name); n != "" {
				s.Name = n
			}
			if v := strings.TrimSpace(meta.Version); v != "" {
				s.Version = v
			}
			s.Description = strings.TrimSpace(meta.Description)
			s.Tags = normalizeTags(meta.Tags)
		}
		s.Body = strings.TrimSpace(rest)
	}
	return s
}

// normalizeTags trims and drops empty tags.
func normalizeTags(tags []string) []string {
	var out []string
	for _, t := range tags {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// splitFrontMatter returns the YAML front-matter (without delimiters) and the
// remaining document. If no front-matter block is present, front-matter is ""
// and the whole content is returned as rest.
func splitFrontMatter(content string) (fm, rest string) {
	trimmed := strings.TrimLeft(content, "\r\n")
	lines := strings.Split(trimmed, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != frontMatterDelim {
		return "", content
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == frontMatterDelim {
			fm = strings.Join(lines[1:i], "\n")
			rest = strings.Join(lines[i+1:], "\n")
			return fm, rest
		}
	}
	// Opening delimiter with no close: not valid front-matter, treat as body.
	return "", content
}

// Get returns the highest-versioned skill registered under name.
func (r *Registry) Get(name string) (Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.highestLocked(name)
}

// highestLocked returns the highest-versioned skill for name. Caller holds mu.
func (r *Registry) highestLocked(name string) (Skill, bool) {
	versions := r.byName[name]
	if len(versions) == 0 {
		return Skill{}, false
	}
	best := versions[0]
	for _, s := range versions[1:] {
		if compareVersions(s.Version, best.Version) > 0 {
			best = s
		}
	}
	return best, true
}

// Resolve selects the skill named name whose version satisfies constraint. An
// empty constraint or the wildcard "*" selects the highest version. A "^X.Y.Z"
// constraint selects the highest version sharing X's major component. A ">=X.Y.Z"
// constraint selects the highest version at or above X.Y.Z. Any other constraint
// is treated as an exact version match. Returns false when nothing satisfies it.
func (r *Registry) Resolve(name, constraint string) (Skill, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	constraint = strings.TrimSpace(constraint)
	if constraint == "" || constraint == versionWildcard {
		return r.highestLocked(name)
	}

	versions := r.byName[name]
	if len(versions) == 0 {
		return Skill{}, false
	}

	var best Skill
	found := false
	for _, s := range versions {
		if !satisfies(s.Version, constraint) {
			continue
		}
		if !found || compareVersions(s.Version, best.Version) > 0 {
			best = s
			found = true
		}
	}
	return best, found
}

// satisfies reports whether version meets constraint. See Resolve for the
// supported constraint grammar.
func satisfies(version, constraint string) bool {
	switch {
	case strings.HasPrefix(constraint, versionPrefixCaret):
		want := strings.TrimSpace(strings.TrimPrefix(constraint, versionPrefixCaret))
		return sameMajor(version, want) && compareVersions(version, want) >= 0
	case strings.HasPrefix(constraint, versionPrefixGTE):
		want := strings.TrimSpace(strings.TrimPrefix(constraint, versionPrefixGTE))
		return compareVersions(version, want) >= 0
	default:
		return compareVersions(version, constraint) == 0
	}
}

// sameMajor reports whether a and b share a major version component.
func sameMajor(a, b string) bool {
	return versionParts(a)[0] == versionParts(b)[0]
}

// versionParts splits a dotted version into three numeric components; missing or
// non-numeric parts read as zero.
func versionParts(v string) [3]int {
	var parts [3]int
	for i, seg := range strings.SplitN(strings.TrimSpace(v), ".", 3) {
		if i > 2 {
			break
		}
		n := 0
		for _, c := range seg {
			if c < '0' || c > '9' {
				n = 0
				break
			}
			n = n*10 + int(c-'0')
		}
		parts[i] = n
	}
	return parts
}

// compareVersions returns -1, 0, or 1 as a is less than, equal to, or greater
// than b, comparing major.minor.patch numerically.
func compareVersions(a, b string) int {
	pa, pb := versionParts(a), versionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return 0
}

// List returns every skill (all versions) sorted by name then descending
// version, so the newest version of each name comes first.
func (r *Registry) List() []Skill {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Skill
	for _, versions := range r.byName {
		out = append(out, versions...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return compareVersions(out[i].Version, out[j].Version) > 0
	})
	return out
}

// Search returns the highest version of every skill whose name, description, or
// any tag contains term (case-insensitively). An empty term matches everything.
// Results are sorted by name.
func (r *Registry) Search(term string) []Skill {
	term = strings.ToLower(strings.TrimSpace(term))
	r.mu.RLock()
	defer r.mu.RUnlock()

	var out []Skill
	for name := range r.byName {
		best, ok := r.highestLocked(name)
		if !ok {
			continue
		}
		if term == "" || skillMatches(best, term) {
			out = append(out, best)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// skillMatches reports whether s contains term (already lowercased) in its name,
// description, or any tag.
func skillMatches(s Skill, term string) bool {
	if strings.Contains(strings.ToLower(s.Name), term) ||
		strings.Contains(strings.ToLower(s.Description), term) {
		return true
	}
	for _, t := range s.Tags {
		if strings.Contains(strings.ToLower(t), term) {
			return true
		}
	}
	return false
}

// ResolveRequested bridges an agentsmd.AgentsConfig to the registry. For each
// requested name it prefers a registry skill (curated, versioned, shareable) and
// falls back to the config's inline skill only when the registry has none. When
// requested is nil, the config's own front-matter RequestedSkills are used.
// Unknown names (in neither the registry nor the config) are skipped. The
// returned slice preserves request order; ok is false only for a nil registry.
//
// This is the documented precedence: registry overrides/augments inline.
func (r *Registry) ResolveRequested(cfg *agentsmd.AgentsConfig, requested []string) []Skill {
	if r == nil {
		return nil
	}
	if requested == nil && cfg != nil {
		requested = cfg.RequestedSkills
	}

	var out []Skill
	for _, name := range requested {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if s, ok := r.Get(name); ok {
			out = append(out, s)
			continue
		}
		if cfg != nil {
			if body, ok := cfg.Skills[name]; ok && strings.TrimSpace(body) != "" {
				out = append(out, Skill{
					Name:    name,
					Version: DefaultVersion,
					Body:    strings.TrimSpace(body),
					Source:  SourceRepo,
				})
			}
		}
	}
	return out
}

// InjectionText renders resolved skills as a Markdown block ready to fold into an
// agent kick prompt. Returns "" for an empty slice, so a caller with no registry
// contributes nothing. This is the guarded wire-in point for the kick-assembly
// path.
//
// The kick-assembly pipeline calls this via the scheduler's primeSkills, which
// resolves the skills an agent declares in config against a registry loaded from
// the hive-host-local skills directory and prepends the result to ${KNOWLEDGE}.
// It remains a standalone helper so other callers (e.g. a BYO-agent launcher
// resolving AgentSpec.DefaultSkills) can render the same block.
func InjectionText(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(injectionHeader)
	b.WriteString("\n\n")
	for i, s := range skills {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString("### ")
		b.WriteString(s.Name)
		if v := strings.TrimSpace(s.Version); v != "" && v != DefaultVersion {
			b.WriteString(" (v")
			b.WriteString(v)
			b.WriteString(")")
		}
		b.WriteString("\n\n")
		b.WriteString(strings.TrimSpace(s.Body))
	}
	b.WriteString("\n")
	return b.String()
}
