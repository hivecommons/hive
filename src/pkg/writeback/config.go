package writeback

import (
	"fmt"
	"sort"
	"strings"
)

// Defaults for the writeback block. All three unsolicited knobs default ON:
// a hive that says nothing about write-back gets the containment.
const (
	// DefaultMinLevel is the floor under which no unsolicited write of any
	// kind lands on an item the hive did not author.
	DefaultMinLevel = 5
	// DefaultOptInLabel is the review opt-in label consulted when
	// review.require_labels is empty.
	DefaultOptInLabel = "hive-review"
	// ConfigKey is the yaml block name.
	ConfigKey = "writeback"
)

// RepoMode is the per-repo override under writeback.per_repo.<repo>.mode.
type RepoMode string

const (
	// RepoModeDefault applies the kind matrix.
	RepoModeDefault RepoMode = ""
	// RepoModeOff blocks every unsolicited write on the repo, including
	// filing new issues.
	RepoModeOff RepoMode = "off"
	// RepoModeIssuesOnly lets the hive file issues but never touch an item
	// it did not open (the torch-spyre case: listed so the deps agent could
	// file regression issues).
	RepoModeIssuesOnly RepoMode = "issues-only"
	// RepoModeLabelsOnly lets label-only kinds through under the matrix and
	// blocks comments, reviews and closes.
	RepoModeLabelsOnly RepoMode = "labels-only"
	// RepoModeFull bypasses the unsolicited gate for this repo: the
	// operator explicitly wants every sweep to write there. The backfill
	// guard still applies.
	RepoModeFull RepoMode = "full"
)

// Valid reports whether m is a known mode.
func (m RepoMode) Valid() bool {
	switch m {
	case RepoModeDefault, RepoModeOff, RepoModeIssuesOnly, RepoModeLabelsOnly, RepoModeFull:
		return true
	}
	return false
}

// RepoModes lists the operator-selectable modes.
func RepoModes() []RepoMode {
	return []RepoMode{RepoModeOff, RepoModeIssuesOnly, RepoModeLabelsOnly, RepoModeFull}
}

// UnsolicitedConfig is writeback.unsolicited.
type UnsolicitedConfig struct {
	// MinLevel, when set, raises every non-own-only kind's floor to at
	// least this level (nil leaves the per-kind matrix as is; the matrix
	// itself puts sentinel/stage/notice kinds at DefaultMinLevel).
	MinLevel *int `yaml:"min_level,omitempty" json:"min_level,omitempty"`
	// RequireLabels gates unsolicited writes on the review opt-in label
	// (review.require_labels, falling back to hive-review). nil means true.
	// false lets a kind's RequireLabel be ignored (level alone decides).
	RequireLabels *bool `yaml:"require_labels,omitempty" json:"require_labels,omitempty"`
	// Backfill lets sweeps write on items created before the hive first saw
	// (or first could write to) their repo. nil means false.
	Backfill *bool `yaml:"backfill,omitempty" json:"backfill,omitempty"`
}

// RepoOverride is writeback.per_repo.<owner/repo>.
type RepoOverride struct {
	Mode RepoMode `yaml:"mode,omitempty" json:"mode,omitempty"`
}

// Config is the writeback block of hive.yaml.
type Config struct {
	Unsolicited UnsolicitedConfig             `yaml:"unsolicited,omitempty" json:"unsolicited"`
	PerRepo     map[string]RepoOverride       `yaml:"per_repo,omitempty" json:"per_repo,omitempty"`
	Kinds       map[string]KindPolicyOverride `yaml:"kinds,omitempty" json:"kinds,omitempty"`
}

// MinLevelOrDefault resolves unsolicited.min_level.
func (c Config) MinLevelOrDefault() int {
	if c.Unsolicited.MinLevel == nil {
		return DefaultMinLevel
	}
	return *c.Unsolicited.MinLevel
}

// RequireLabelsEnabled resolves unsolicited.require_labels (default true).
func (c Config) RequireLabelsEnabled() bool {
	return c.Unsolicited.RequireLabels == nil || *c.Unsolicited.RequireLabels
}

// BackfillEnabled resolves unsolicited.backfill (default false).
func (c Config) BackfillEnabled() bool {
	return c.Unsolicited.Backfill != nil && *c.Unsolicited.Backfill
}

// RepoModeFor returns the per-repo override for repo (any case, bare or
// owner-qualified key match) or RepoModeDefault.
func (c Config) RepoModeFor(repo string) RepoMode {
	want := strings.ToLower(strings.TrimSpace(repo))
	if want == "" {
		return RepoModeDefault
	}
	for key, ov := range c.PerRepo {
		k := strings.ToLower(strings.TrimSpace(key))
		if k == want || strings.HasSuffix(want, "/"+k) || strings.HasSuffix(k, "/"+want) {
			return ov.Mode
		}
	}
	return RepoModeDefault
}

// Policy resolves the effective policy for kind: the default matrix with
// writeback.kinds.<kind> applied, then the global floor. An unknown kind
// gets the strictest policy (never on foreign items) so a new write path
// cannot slip through by naming a kind the matrix does not know.
func (c Config) Policy(kind Kind) KindPolicy {
	p, ok := DefaultKinds()[kind]
	if !ok {
		return KindPolicy{MinLevel: 99, RequireLabel: true, OwnOnly: true}
	}
	if ov, ok := c.Kinds[string(kind)]; ok {
		if ov.MinLevel != nil {
			p.MinLevel = *ov.MinLevel
		}
		if ov.RequireLabel != nil {
			p.RequireLabel = *ov.RequireLabel
		}
		if ov.UnlabeledMinLevel != nil {
			p.UnlabeledMinLevel = *ov.UnlabeledMinLevel
		}
		if ov.OwnOnly != nil {
			p.OwnOnly = *ov.OwnOnly
		}
	}
	// The global floor is a blunt operator override: only an explicit
	// writeback.unsolicited.min_level raises the per-kind matrix (the matrix
	// already places some own-only / low-risk kinds below DefaultMinLevel).
	if c.Unsolicited.MinLevel != nil && !kind.IsIssueFileKind() && !p.OwnOnly && p.MinLevel < *c.Unsolicited.MinLevel {
		p.MinLevel = *c.Unsolicited.MinLevel
	}
	if !c.RequireLabelsEnabled() {
		p.RequireLabel = false
	}
	return p
}

// EffectiveKinds returns the resolved matrix for every known kind, in
// Kinds() order, for the dashboard and the docs guard.
func (c Config) EffectiveKinds() []KindRow {
	kinds := Kinds()
	out := make([]KindRow, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, KindRow{Kind: k, Policy: c.Policy(k)})
	}
	return out
}

// KindRow pairs a kind with its resolved policy.
type KindRow struct {
	Kind   Kind       `json:"kind"`
	Policy KindPolicy `json:"policy"`
}

// Validate rejects unknown kinds, unknown per-repo modes and negative
// levels. It is called from config load.
func (c Config) Validate() error {
	if c.Unsolicited.MinLevel != nil && (*c.Unsolicited.MinLevel < 0 || *c.Unsolicited.MinLevel > 6) {
		return fmt.Errorf("%s.unsolicited.min_level: %d is outside 0..6", ConfigKey, *c.Unsolicited.MinLevel)
	}
	keys := make([]string, 0, len(c.Kinds))
	for k := range c.Kinds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !IsKind(k) {
			return fmt.Errorf("%s.kinds.%s: unknown write-back kind (known: %s)", ConfigKey, k, joinKinds())
		}
		ov := c.Kinds[k]
		if ov.MinLevel != nil && (*ov.MinLevel < 0 || *ov.MinLevel > 6) {
			return fmt.Errorf("%s.kinds.%s.min_level: %d is outside 0..6", ConfigKey, k, *ov.MinLevel)
		}
		if ov.UnlabeledMinLevel != nil && (*ov.UnlabeledMinLevel < 0 || *ov.UnlabeledMinLevel > 6) {
			return fmt.Errorf("%s.kinds.%s.unlabeled_min_level: %d is outside 0..6", ConfigKey, k, *ov.UnlabeledMinLevel)
		}
	}
	repos := make([]string, 0, len(c.PerRepo))
	for r := range c.PerRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	for _, r := range repos {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("%s.per_repo: empty repo key", ConfigKey)
		}
		if m := c.PerRepo[r].Mode; !m.Valid() {
			return fmt.Errorf("%s.per_repo.%s.mode: %q is not one of off, issues-only, labels-only, full", ConfigKey, r, m)
		}
	}
	return nil
}

func joinKinds() string {
	ks := Kinds()
	s := make([]string, len(ks))
	for i, k := range ks {
		s[i] = string(k)
	}
	return strings.Join(s, ", ")
}
