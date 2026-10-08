// Package compliance is Hive's control registry (hivecommons/hive#11078): it
// maps the requirements of compliance frameworks (SOC 2, later FedRAMP and
// ISO 27001) onto the Hive settings that implement them, and evaluates a live
// config against a framework profile's recommended posture.
//
// Profiles are data, not code: one YAML file per framework under profiles/,
// embedded at build time. Each control names its framework citation, the
// Hive setting(s) that implement it, the recommended value and the evaluator
// that compares current to recommended — or is explicitly marked not covered
// by Hive, with a rationale.
//
// Nothing here is a certification. Evaluate reports whether the settings an
// operator's own control owners point to are in the profile's recommended
// posture; whether that satisfies an auditor is the operator's call. The
// package is read-only: it never changes a setting.
package compliance

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed profiles/*.yaml
var profilesFS embed.FS

const profilesDir = "profiles"

// Disclaimer is carried on every evaluated report so no consumer can render
// the statuses without it.
const Disclaimer = "Hive is not certified for SOC 2, FedRAMP, ISO 27001 or any other framework. " +
	"These statuses show whether Hive settings match a profile's recommended posture; " +
	"your organisation's control owners and auditors decide whether that satisfies a control."

// Profile is one compliance framework's control mapping.
type Profile struct {
	ID       string    `yaml:"id" json:"id"`
	Name     string    `yaml:"name" json:"name"`
	Version  string    `yaml:"version" json:"version"`
	Controls []Control `yaml:"controls" json:"controls"`
}

// Control is one framework requirement and the Hive settings that implement
// it. A NotCovered control has no mappings and a Rationale saying why Hive
// cannot evidence it.
type Control struct {
	ID         string    `yaml:"id" json:"id"`
	Title      string    `yaml:"title" json:"title"`
	Text       string    `yaml:"text" json:"text"`
	Citation   string    `yaml:"citation" json:"citation"`
	Domain     string    `yaml:"domain" json:"domain"`
	Mappings   []Mapping `yaml:"mappings,omitempty" json:"mappings,omitempty"`
	NotCovered bool      `yaml:"not_covered,omitempty" json:"not_covered,omitempty"`
	Rationale  string    `yaml:"rationale,omitempty" json:"rationale,omitempty"`
}

// Mapping ties a control to one Hive setting, the value the profile
// recommends for it, and the evaluator that compares the two.
type Mapping struct {
	SettingPath string `yaml:"setting_path" json:"setting_path"`
	Recommended string `yaml:"recommended" json:"recommended"`
	Evaluator   string `yaml:"evaluator" json:"evaluator"`
}

// Domains is the closed set of control domains the Compliance tab groups
// controls under.
var Domains = []string{
	"Access control",
	"Change management",
	"Data handling",
	"Incident response",
	"Logging & monitoring",
	"Risk management",
	"Segregation of duties",
	"Vulnerability management",
}

func knownDomain(d string) bool {
	for _, k := range Domains {
		if k == d {
			return true
		}
	}
	return false
}

var (
	loadOnce   sync.Once
	loaded     []Profile
	loadErr    error
	profilesFn = func() fs.FS { return profilesFS }
)

// Profiles returns every embedded profile sorted by ID. The embedded files
// are validated by the package tests, so an error here means a broken build.
func Profiles() ([]Profile, error) {
	loadOnce.Do(func() {
		loaded, loadErr = loadProfiles(profilesFn(), profilesDir)
	})
	if loadErr != nil {
		return nil, loadErr
	}
	return append([]Profile(nil), loaded...), nil
}

// ProfileByID returns the embedded profile with the given ID
// (case-insensitive).
func ProfileByID(id string) (Profile, bool) {
	ps, err := Profiles()
	if err != nil {
		return Profile{}, false
	}
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range ps {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

func loadProfiles(fsys fs.FS, dir string) ([]Profile, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("compliance: reading %s: %w", dir, err)
	}
	var out []Profile
	seen := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != ".yaml" {
			continue
		}
		name := path.Join(dir, e.Name())
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("compliance: reading %s: %w", name, err)
		}
		p, err := parseProfile(raw)
		if err != nil {
			return nil, fmt.Errorf("compliance: %s: %w", name, err)
		}
		if want := strings.TrimSuffix(e.Name(), ".yaml"); p.ID != want {
			return nil, fmt.Errorf("compliance: %s: profile id %q must match the file name %q", name, p.ID, want)
		}
		if prev, dup := seen[p.ID]; dup {
			return nil, fmt.Errorf("compliance: %s: duplicate profile id %q (also in %s)", name, p.ID, prev)
		}
		seen[p.ID] = name
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// parseProfile strictly decodes one profile (unknown keys are errors, so a
// misspelt `not_coverd:` cannot silently drop a marker) and validates it.
func parseProfile(raw []byte) (Profile, error) {
	var p Profile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&p); err != nil {
		return Profile{}, fmt.Errorf("decoding: %w", err)
	}
	if err := p.Validate(); err != nil {
		return Profile{}, err
	}
	return p, nil
}

// Validate checks the profile schema: identifying fields present, control
// IDs unique, domains known, and every mapping naming a known setting, a
// known evaluator and a recommended value that evaluator accepts. A control
// is either covered (≥1 mapping) or NotCovered with a Rationale — never both
// or neither.
func (p Profile) Validate() error {
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Version) == "" {
		return fmt.Errorf("profile id, name and version are required")
	}
	if len(p.Controls) == 0 {
		return fmt.Errorf("profile %s has no controls", p.ID)
	}
	ids := map[string]bool{}
	for i, c := range p.Controls {
		where := fmt.Sprintf("profile %s controls[%d]", p.ID, i)
		if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Title) == "" || strings.TrimSpace(c.Citation) == "" {
			return fmt.Errorf("%s: id, title and citation are required", where)
		}
		if ids[c.ID] {
			return fmt.Errorf("%s: duplicate control id %q", where, c.ID)
		}
		ids[c.ID] = true
		if strings.TrimSpace(c.Text) == "" {
			return fmt.Errorf("%s (%s): text is required", where, c.ID)
		}
		if !knownDomain(c.Domain) {
			return fmt.Errorf("%s (%s): unknown domain %q", where, c.ID, c.Domain)
		}
		if c.NotCovered {
			if len(c.Mappings) > 0 {
				return fmt.Errorf("%s (%s): a not_covered control must not have mappings", where, c.ID)
			}
			if strings.TrimSpace(c.Rationale) == "" {
				return fmt.Errorf("%s (%s): a not_covered control needs a rationale", where, c.ID)
			}
			continue
		}
		if len(c.Mappings) == 0 {
			return fmt.Errorf("%s (%s): has no mappings; mark it not_covered with a rationale instead", where, c.ID)
		}
		for j, m := range c.Mappings {
			if err := m.validate(); err != nil {
				return fmt.Errorf("%s (%s) mappings[%d]: %w", where, c.ID, j, err)
			}
		}
	}
	return nil
}

func (m Mapping) validate() error {
	s, ok := settings[m.SettingPath]
	if !ok {
		return fmt.Errorf("unknown setting_path %q", m.SettingPath)
	}
	ev, ok := evaluators[m.Evaluator]
	if !ok {
		return fmt.Errorf("unknown evaluator %q", m.Evaluator)
	}
	if !kindAllowed(ev, s.Kind) {
		return fmt.Errorf("evaluator %s cannot evaluate %s setting %q", m.Evaluator, s.Kind, m.SettingPath)
	}
	if strings.TrimSpace(m.Recommended) == "" {
		return fmt.Errorf("recommended is required")
	}
	if ev.checkRecommended != nil {
		if err := ev.checkRecommended(m.Recommended); err != nil {
			return fmt.Errorf("evaluator %s: %w", m.Evaluator, err)
		}
	}
	return nil
}
