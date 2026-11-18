package reviewpreflight

import (
	"context"
	"path"
	"regexp"
	"strings"
	"time"
)

// Dep is one dependency version a PR adds or bumps.
type Dep struct {
	Ecosystem string // Ecosystem.ID
	Name      string
	Version   string // as written; empty when it cannot be checked (see Note)
	File      string
	// Note says why a dependency cannot be checked ("pinned by commit SHA").
	Note string
}

// ChangedFile is a file a PR changes with its unified-diff patch. Patch is
// empty when GitHub withheld it (a very large or binary diff).
type ChangedFile struct {
	Path  string
	Patch string
}

// pline is one line of a patch: an added line or a context line.
type pline struct {
	text  string
	added bool
}

// Ecosystem is one table entry: how to recognize the manifests of a package
// ecosystem, parse the versions a patch adds, and look the versions up.
//
// To add an ecosystem: append an entry to Ecosystems with its manifest
// patterns and a parser over the patch lines, set OSV to the OSV ecosystem id
// (https://ossf.github.io/osv-schema/#affectedpackage-field) and Age to a
// registry lookup when there is one, then add a case to ecosystems_test.go and
// a row to the table in docs/review-swarm.md. A nil Age or empty OSV means that
// check is reported as "not checked" for the ecosystem, never skipped silently.
type Ecosystem struct {
	ID    string
	Label string
	// OSV is the OSV ecosystem id; empty means no vulnerability source.
	OSV string
	// StripV drops a leading "v" from the version sent to OSV.
	StripV bool
	// Files are patterns for the manifests: a pattern with a slash matches the
	// full path, otherwise the base name. Case insensitive, path.Match syntax.
	Files []string
	// Parse returns the dependencies whose version appears on an added line.
	Parse func(file string, lines []pline) []Dep
	// Age returns when the version was published; nil means unsupported.
	Age func(ctx context.Context, get GetFunc, base string, d Dep) (time.Time, error)
	// Registry is the default registry base URL, overridable through
	// review.preflight.registry_urls.<ID>.
	Registry string
}

// Unsupported names a manifest kind that is recognized but not checked, so a
// PR touching it is told "not checked" instead of looking clean.
type Unsupported struct {
	Label string
	Files []string
}

// UnsupportedManifests are recognized manifests with no ecosystem entry yet.
var UnsupportedManifests = []Unsupported{
	{"Hex (Elixir)", []string{"mix.lock"}},
	{"Dart pub", []string{"pubspec.lock"}},
	{"SwiftPM", []string{"package.resolved"}},
	{"CocoaPods", []string{"podfile.lock"}},
	{"Pipenv", []string{"pipfile.lock"}},
	{"Bun", []string{"bun.lock", "bun.lockb"}},
	{"Deno", []string{"deno.lock"}},
	{"Helm chart dependencies", []string{"chart.lock"}},
	{"Nix flakes", []string{"flake.lock"}},
	{"Terraform providers", []string{".terraform.lock.hcl"}},
	{"Bazel modules", []string{"module.bazel.lock"}},
	{"Conan", []string{"conan.lock"}},
	{"sbt", []string{"build.sbt"}},
}

// Ecosystems is the pluggable table. Order matters only for matching: the
// first entry whose Files match a path owns it.
var Ecosystems = []Ecosystem{
	{ID: "go", Label: "Go modules", OSV: "Go", StripV: true, Files: []string{"go.mod", "go.sum"},
		Parse: parseGo, Age: goAge, Registry: "https://proxy.golang.org"},
	{ID: "npm", Label: "npm, yarn, pnpm", OSV: "npm",
		Files: []string{"package.json", "package-lock.json", "npm-shrinkwrap.json", "yarn.lock", "pnpm-lock.yaml"},
		Parse: parseNpm, Age: npmAge, Registry: "https://registry.npmjs.org"},
	{ID: "pypi", Label: "Python", OSV: "PyPI",
		Files: []string{"requirements*.txt", "pyproject.toml", "poetry.lock", "uv.lock"},
		Parse: parsePyPI, Age: pypiAge, Registry: "https://pypi.org"},
	{ID: "crates", Label: "Rust", OSV: "crates.io", Files: []string{"cargo.lock"},
		Parse: parseCargo, Age: cratesAge, Registry: "https://crates.io"},
	{ID: "maven", Label: "Java (Maven, Gradle)", OSV: "Maven",
		Files: []string{"pom.xml", "gradle.lockfile", "*.gradle.lockfile"},
		Parse: parseMaven, Age: mavenAge, Registry: "https://search.maven.org"},
	{ID: "rubygems", Label: "Ruby", OSV: "RubyGems", Files: []string{"gemfile.lock"},
		Parse: parseRuby, Age: rubygemsAge, Registry: "https://rubygems.org"},
	{ID: "packagist", Label: "PHP", OSV: "Packagist", StripV: true, Files: []string{"composer.lock"},
		Parse: parseComposer, Age: packagistAge, Registry: "https://repo.packagist.org"},
	{ID: "nuget", Label: ".NET", OSV: "NuGet", Files: []string{"packages.lock.json"},
		Parse: parseNuGet, Age: nugetAge, Registry: "https://api.nuget.org"},
	{ID: "actions", Label: "GitHub Actions", OSV: "GitHub Actions", StripV: true,
		Files: []string{".github/workflows/*.yml", ".github/workflows/*.yaml"}, Parse: parseActions},
	{ID: "container", Label: "Container images",
		Files: []string{"dockerfile", "dockerfile.*", "*.dockerfile", "docker-compose*.yml", "docker-compose*.yaml", "compose.yml", "compose.yaml"},
		Parse: parseContainer},
}

// FileMatches reports whether a table pattern matches a file path.
func FileMatches(pattern, file string) bool {
	pattern = strings.ToLower(pattern)
	file = strings.ToLower(file)
	target := path.Base(file)
	if strings.Contains(pattern, "/") {
		target = file
	}
	ok, err := path.Match(pattern, target)
	return err == nil && ok
}

// ecosystemFor returns the table entry that owns file.
func ecosystemFor(file string) (Ecosystem, bool) {
	for _, e := range Ecosystems {
		for _, pat := range e.Files {
			if FileMatches(pat, file) {
				return e, true
			}
		}
	}
	return Ecosystem{}, false
}

// unsupportedFor names the unsupported manifest kind of file, if any.
func unsupportedFor(file string) (string, bool) {
	for _, u := range UnsupportedManifests {
		for _, pat := range u.Files {
			if FileMatches(pat, file) {
				return u.Label, true
			}
		}
	}
	return "", false
}

// patchLines turns a unified diff into added and context lines in order.
func patchLines(patch string) []pline {
	var out []pline
	for _, raw := range strings.Split(patch, "\n") {
		raw = strings.TrimSuffix(raw, "\r")
		switch {
		case strings.HasPrefix(raw, "@@"), strings.HasPrefix(raw, "+++"), strings.HasPrefix(raw, "---"), strings.HasPrefix(raw, "\\"):
			continue
		case strings.HasPrefix(raw, "+"):
			out = append(out, pline{text: raw[1:], added: true})
		case strings.HasPrefix(raw, "-"):
			continue
		case strings.HasPrefix(raw, " "):
			out = append(out, pline{text: raw[1:]})
		}
	}
	return out
}

// pairParser handles manifests where a name line precedes its version line
// (Cargo.lock, poetry.lock, uv.lock, composer.lock): a dependency is reported
// when its version line is added.
func pairParser(eco, file string, lines []pline, nameRe, verRe *regexp.Regexp) []Dep {
	var out []Dep
	name := ""
	for _, l := range lines {
		if m := nameRe.FindStringSubmatch(l.text); m != nil {
			name = m[1]
			continue
		}
		if m := verRe.FindStringSubmatch(l.text); m != nil && l.added && name != "" {
			out = append(out, Dep{Ecosystem: eco, Name: name, Version: m[1], File: file})
		}
	}
	return out
}

// ---- Go ----

func parseGo(file string, lines []pline) []Dep {
	isSum := strings.HasSuffix(strings.ToLower(file), "go.sum")
	var out []Dep
	for _, l := range lines {
		if !l.added {
			continue
		}
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(l.text), "require "))
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "module", "go", "toolchain", "replace", "exclude", "retract", "require", "(", ")":
			continue
		}
		ver := f[1]
		if isSum && strings.HasSuffix(ver, "/go.mod") {
			continue
		}
		if !strings.HasPrefix(ver, "v") || !strings.Contains(ver, ".") {
			continue
		}
		out = append(out, Dep{Ecosystem: "go", Name: f[0], Version: ver, File: file})
	}
	return out
}

// ---- npm, yarn, pnpm ----

var (
	npmDepRe      = regexp.MustCompile(`^\s*"((?:@[\w.\-]+/)?[\w.\-]+)"\s*:\s*"[~^]?=?\s*v?(\d+\.\d+\.\d+[\w.+\-]*)"`)
	npmResolvedRe = regexp.MustCompile(`"resolved":\s*"https?://[^"/]+/((?:@[^/"]+/)?[^/"]+)/-/[^"/]*?-(\d+\.\d+\.\d+[^"/]*?)\.tgz`)
	yarnHeaderRe  = regexp.MustCompile(`^"?((?:@[^@\s"/]+/)?[^@\s"/]+)@[^\s]*.*:$`)
	yarnVersionRe = regexp.MustCompile(`^\s{2}version:?\s+"?(\d[^"\s]*)"?\s*$`)
	pnpmKeyRe     = regexp.MustCompile(`^\s*'?/?((?:@[\w.\-]+/)?[\w.\-]+)@(\d+\.\d+\.\d+[\w.+\-]*)[^:]*:\s*$`)
	npmSkipKeys   = map[string]bool{"version": true, "name": true, "node": true, "npm": true, "yarn": true, "pnpm": true, "license": true, "main": true, "module": true, "type": true}
)

func parseNpm(file string, lines []pline) []Dep {
	base := strings.ToLower(path.Base(file))
	var out []Dep
	switch base {
	case "package.json":
		for _, l := range lines {
			if m := npmDepRe.FindStringSubmatch(l.text); m != nil && l.added && !npmSkipKeys[strings.ToLower(m[1])] {
				out = append(out, Dep{Ecosystem: "npm", Name: m[1], Version: m[2], File: file})
			}
		}
	case "package-lock.json", "npm-shrinkwrap.json":
		for _, l := range lines {
			if m := npmResolvedRe.FindStringSubmatch(l.text); m != nil && l.added {
				out = append(out, Dep{Ecosystem: "npm", Name: m[1], Version: m[2], File: file})
			}
		}
	case "yarn.lock":
		name := ""
		for _, l := range lines {
			if !strings.HasPrefix(l.text, " ") && !strings.HasPrefix(l.text, "#") {
				if m := yarnHeaderRe.FindStringSubmatch(strings.TrimSpace(l.text)); m != nil {
					name = m[1]
					continue
				}
			}
			if m := yarnVersionRe.FindStringSubmatch(l.text); m != nil && l.added && name != "" {
				out = append(out, Dep{Ecosystem: "npm", Name: name, Version: m[1], File: file})
			}
		}
	case "pnpm-lock.yaml":
		for _, l := range lines {
			if m := pnpmKeyRe.FindStringSubmatch(l.text); m != nil && l.added {
				out = append(out, Dep{Ecosystem: "npm", Name: m[1], Version: m[2], File: file})
			}
		}
	}
	return out
}

// ---- Python ----

var (
	pyReqRe     = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._\-]*)(?:\[[^\]]*\])?\s*===?\s*([0-9][\w.+!\-]*)`)
	pyQuotedRe  = regexp.MustCompile(`"([A-Za-z0-9][A-Za-z0-9._\-]*)(?:\[[^\]]*\])?\s*===?\s*([0-9][\w.+!\-]*)"`)
	tomlNameRe  = regexp.MustCompile(`^name\s*=\s*"([^"]+)"\s*$`)
	tomlVerRe   = regexp.MustCompile(`^version\s*=\s*"([^"]+)"\s*$`)
	composerNme = regexp.MustCompile(`^\s*"name":\s*"([\w.\-]+/[\w.\-]+)",?\s*$`)
	composerVer = regexp.MustCompile(`^\s*"version":\s*"v?(\d[^"]*)",?\s*$`)
)

func parsePyPI(file string, lines []pline) []Dep {
	base := strings.ToLower(path.Base(file))
	switch base {
	case "poetry.lock", "uv.lock":
		return pairParser("pypi", file, lines, tomlNameRe, tomlVerRe)
	}
	re := pyReqRe
	if base == "pyproject.toml" {
		re = pyQuotedRe
	}
	var out []Dep
	for _, l := range lines {
		if m := re.FindStringSubmatch(l.text); m != nil && l.added {
			out = append(out, Dep{Ecosystem: "pypi", Name: strings.ToLower(m[1]), Version: m[2], File: file})
		}
	}
	return out
}

// ---- Rust ----

func parseCargo(file string, lines []pline) []Dep {
	return pairParser("crates", file, lines, tomlNameRe, tomlVerRe)
}

// ---- Java ----

var (
	gradleLockRe = regexp.MustCompile(`^([\w.\-]+):([\w.\-]+):([\w.+\-]+)=`)
	pomGroupRe   = regexp.MustCompile(`<groupId>\s*([^<\s$]+)\s*</groupId>`)
	pomArtifact  = regexp.MustCompile(`<artifactId>\s*([^<\s$]+)\s*</artifactId>`)
	pomVersionRe = regexp.MustCompile(`<version>\s*([^<\s$]+)\s*</version>`)
)

func parseMaven(file string, lines []pline) []Dep {
	var out []Dep
	if strings.HasSuffix(strings.ToLower(file), ".lockfile") {
		for _, l := range lines {
			if m := gradleLockRe.FindStringSubmatch(l.text); m != nil && l.added {
				out = append(out, Dep{Ecosystem: "maven", Name: m[1] + ":" + m[2], Version: m[3], File: file})
			}
		}
		return out
	}
	group, artifact := "", ""
	for _, l := range lines {
		t := l.text
		switch {
		case strings.Contains(t, "<dependency>"), strings.Contains(t, "<plugin>"), strings.Contains(t, "<parent>"):
			group, artifact = "", ""
		}
		if m := pomGroupRe.FindStringSubmatch(t); m != nil {
			group = m[1]
		}
		if m := pomArtifact.FindStringSubmatch(t); m != nil {
			artifact = m[1]
		}
		if m := pomVersionRe.FindStringSubmatch(t); m != nil && l.added && group != "" && artifact != "" {
			out = append(out, Dep{Ecosystem: "maven", Name: group + ":" + artifact, Version: m[1], File: file})
		}
	}
	return out
}

// ---- Ruby ----

var gemRe = regexp.MustCompile(`^ {4}([\w.\-]+) \((\d[\w.]*)(?:-[a-zA-Z][\w\-.]*)?\)\s*$`)

func parseRuby(file string, lines []pline) []Dep {
	var out []Dep
	for _, l := range lines {
		if m := gemRe.FindStringSubmatch(l.text); m != nil && l.added {
			out = append(out, Dep{Ecosystem: "rubygems", Name: m[1], Version: m[2], File: file})
		}
	}
	return out
}

// ---- PHP ----

func parseComposer(file string, lines []pline) []Dep {
	return pairParser("packagist", file, lines, composerNme, composerVer)
}

// ---- .NET ----

var (
	nugetHeaderRe   = regexp.MustCompile(`^\s*"([A-Za-z0-9_.\-]+)":\s*\{\s*$`)
	nugetResolvedRe = regexp.MustCompile(`^\s*"resolved":\s*"(\d[^"]*)"`)
)

func parseNuGet(file string, lines []pline) []Dep {
	var out []Dep
	name := ""
	for _, l := range lines {
		if m := nugetHeaderRe.FindStringSubmatch(l.text); m != nil {
			name = m[1]
			continue
		}
		if m := nugetResolvedRe.FindStringSubmatch(l.text); m != nil && l.added && name != "" {
			out = append(out, Dep{Ecosystem: "nuget", Name: name, Version: m[1], File: file})
		}
	}
	return out
}

// ---- GitHub Actions ----

var (
	actionsUsesRe = regexp.MustCompile(`\buses:\s*['"]?([A-Za-z0-9_.\-]+/[A-Za-z0-9_.\-]+)(?:/[^@\s'"]*)?@([^\s'"#]+)`)
	sha40Re       = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)
	fullSemverRe  = regexp.MustCompile(`^v?\d+\.\d+\.\d+([\-+][\w.\-]+)?$`)
)

func parseActions(file string, lines []pline) []Dep {
	var out []Dep
	for _, l := range lines {
		m := actionsUsesRe.FindStringSubmatch(l.text)
		if m == nil || !l.added {
			continue
		}
		d := Dep{Ecosystem: "actions", Name: m[1], File: file}
		switch ref := m[2]; {
		case sha40Re.MatchString(ref):
			d.Note = "pinned by commit SHA, no version to look up"
		case fullSemverRe.MatchString(ref):
			d.Version = ref
		default:
			d.Note = "floating ref " + sanitizeLine(ref) + ", no exact version to look up"
		}
		out = append(out, d)
	}
	return out
}

// ---- Container images ----

var (
	dockerFromRe = regexp.MustCompile(`(?i)^\s*FROM\s+(?:--\S+\s+)?([^\s]+)`)
	imageLineRe  = regexp.MustCompile(`^\s*image:\s*['"]?([^\s'"#]+)`)
)

func parseContainer(file string, lines []pline) []Dep {
	var out []Dep
	for _, l := range lines {
		if !l.added {
			continue
		}
		ref := ""
		if m := dockerFromRe.FindStringSubmatch(l.text); m != nil {
			ref = m[1]
		} else if m := imageLineRe.FindStringSubmatch(l.text); m != nil {
			ref = m[1]
		}
		if ref == "" || strings.Contains(ref, "$") || strings.EqualFold(ref, "scratch") {
			continue
		}
		name, ver := ref, "latest"
		if i := strings.Index(ref, "@"); i >= 0 {
			name, ver = ref[:i], ref[i+1:]
		} else if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
			name, ver = ref[:i], ref[i+1:]
		}
		out = append(out, Dep{Ecosystem: "container", Name: name, Version: ver, File: file, Note: "no vulnerability source for container images"})
	}
	return out
}
