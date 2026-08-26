package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// These tests pin the #4041 login_patterns de-materialization contract.
//
// Background: Save() marshals the whole Config, and applyDefaults() fills
// LoginPatterns on every load — so any hive that ever saved its config had
// the era's DEFAULT login patterns written into the persisted file as if an
// operator had chosen them. Because defaults only apply to an empty list,
// the #3959 defaults fix (generic English phrases → CLI login chrome) never
// reached those hives: on a live hosted hive the quality agent flapped on
// `(?i)copilot auth` for days (restart_count 83 at first trip) because the
// pre-#3959 list was pinned in its PVC runtime config.
//
// The contract:
//  1. a persisted list byte-identical to the pre-#3959 defaults is migrated
//     (dropped, so the current defaults apply);
//  2. a customized list is preserved verbatim — operator intent is sacred;
//  3. a list equal to the CURRENT defaults loads unchanged;
//  4. Save() never writes a default-equal list, so future default fixes
//     reach existing hives instead of being pinned out forever.

// legacyLoginPatternsYAML is the exact materialized block observed in the
// frostyard incident's /data/hive.yaml.runtime (#4041) — the pre-#3959
// defaults, including the `copilot auth` entry the second hosted hive's
// quality agent flapped on as `(?i)copilot auth`.
const legacyLoginPatternsYAML = `
        - please log in
        - authentication required
        - not logged in
        - login required
        - session expired
        - token expired
        - unauthorized.*401
        - gh auth login
        - claude login
        - copilot auth
`

func writeSensingConfig(t *testing.T, loginPatternsYAML string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	sensing := ""
	if loginPatternsYAML != "" {
		sensing = "  sensing:\n    login_patterns:\n" + loginPatternsYAML
	}
	content := `project:
  org: testorg
  repos:
    - repo1
github:
  app_id: 3568013
agents:
  scanner:
    backend: claude
governor:
` + sensing
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func assertPatternsEqual(t *testing.T, got, want []string, context string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d patterns %v, want %d %v", context, len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s: pattern[%d] = %q, want %q (got %v)", context, i, got[i], want[i], got)
		}
	}
}

// The frostyard regression: a persisted list byte-identical to the pre-#3959
// defaults must be treated as "default" and migrated so the corrected
// defaults apply.
func TestLoginPatterns_LegacyDefaultListMigratedToCurrentDefaults(t *testing.T) {
	path := writeSensingConfig(t, legacyLoginPatternsYAML)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertPatternsEqual(t, cfg.Governor.Sensing.LoginPatterns, defaultLoginPatterns,
		"legacy-default list must migrate to the current defaults")
	// The generic English phrases that caused the false positives must be gone.
	for _, p := range cfg.Governor.Sensing.LoginPatterns {
		switch p {
		case "authentication required", "please log in", "session expired",
			"token expired", "login required", "unauthorized.*401":
			t.Fatalf("pre-#3959 generic pattern %q survived migration", p)
		}
	}
}

// Operator intent is sacred: ANY deviation from the legacy default set —
// even removing a single entry — means the list is customized and must load
// verbatim.
func TestLoginPatterns_CustomizedListPreservedVerbatim(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want []string
	}{
		"fully custom": {
			yaml: "        - my own pattern\n        - another one\n",
			want: []string{"my own pattern", "another one"},
		},
		"legacy list minus one entry": {
			yaml: `
        - please log in
        - authentication required
        - not logged in
        - login required
        - session expired
        - token expired
        - unauthorized.*401
        - gh auth login
        - claude login
`,
			want: []string{"please log in", "authentication required", "not logged in",
				"login required", "session expired", "token expired", "unauthorized.*401",
				"gh auth login", "claude login"},
		},
		"legacy list plus one entry": {
			yaml: legacyLoginPatternsYAML + "        - operator addition\n",
			want: append(append([]string(nil), legacyDefaultLoginPatterns...), "operator addition"),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeSensingConfig(t, tc.yaml)
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			assertPatternsEqual(t, cfg.Governor.Sensing.LoginPatterns, tc.want,
				"customized list must be preserved verbatim")
		})
	}
}

// A list equal to the CURRENT defaults loads unchanged — migration only ever
// fires on the legacy set.
func TestLoginPatterns_NewDefaultListLoadsUnchanged(t *testing.T) {
	var b strings.Builder
	for _, p := range defaultLoginPatterns {
		b.WriteString("        - \"" + p + "\"\n")
	}
	path := writeSensingConfig(t, b.String())
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	assertPatternsEqual(t, cfg.Governor.Sensing.LoginPatterns, defaultLoginPatterns,
		"current-default list must load unchanged")
}

// The materialization source itself: Save() must not write a default-equal
// login_patterns list into the persisted config. This is what lets a FUTURE
// defaults fix reach existing hives — the exact mechanism that pinned the
// pre-#3959 list into every hive's PVC config forever.
func TestSave_DoesNotMaterializeDefaultLoginPatterns(t *testing.T) {
	path := writeSensingConfig(t, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Defaults are live in memory (the detector needs them)...
	assertPatternsEqual(t, cfg.Governor.Sensing.LoginPatterns, defaultLoginPatterns,
		"defaults must be applied in memory")

	// Keep the PVC-runtime side-write inside the sandboxed temp dir.
	origRuntime := RuntimeConfigFile
	RuntimeConfigFile = filepath.Join(t.TempDir(), "hive.yaml.runtime")
	defer func() { RuntimeConfigFile = origRuntime }()

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading saved config: %v", err)
	}
	var persisted struct {
		Governor struct {
			Sensing struct {
				LoginPatterns []string `yaml:"login_patterns"`
			} `yaml:"sensing"`
		} `yaml:"governor"`
	}
	if err := yaml.Unmarshal(raw, &persisted); err != nil {
		t.Fatalf("parsing saved config: %v", err)
	}
	if len(persisted.Governor.Sensing.LoginPatterns) != 0 {
		t.Fatalf("Save materialized default login_patterns into the config: %v",
			persisted.Governor.Sensing.LoginPatterns)
	}

	// ...and a reload still gets the (current) defaults.
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	assertPatternsEqual(t, reloaded.Governor.Sensing.LoginPatterns, defaultLoginPatterns,
		"defaults must apply again after a save/reload cycle")
}

// Positive control for the persist rule: a genuinely customized list IS
// written, survives the round-trip verbatim, and is never migrated.
func TestSave_PersistsCustomizedLoginPatternsVerbatim(t *testing.T) {
	path := writeSensingConfig(t, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	custom := []string{"Device code: [A-Z0-9]{4}-[A-Z0-9]{4}", "my-forge login"}
	cfg.Governor.Sensing.LoginPatterns = append([]string(nil), custom...)

	origRuntime := RuntimeConfigFile
	RuntimeConfigFile = filepath.Join(t.TempDir(), "hive.yaml.runtime")
	defer func() { RuntimeConfigFile = origRuntime }()

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	assertPatternsEqual(t, reloaded.Governor.Sensing.LoginPatterns, custom,
		"customized list must survive a save/reload cycle verbatim")
}

// IsLegacyDefaultLoginPatterns is exported for cmd/hive's state-overrides
// replay; pin its exactness here so the migration can only ever fire on a
// byte-identical match.
func TestIsLegacyDefaultLoginPatterns_Exactness(t *testing.T) {
	if !IsLegacyDefaultLoginPatterns(append([]string(nil), legacyDefaultLoginPatterns...)) {
		t.Fatal("exact legacy list not recognized")
	}
	reordered := append([]string(nil), legacyDefaultLoginPatterns...)
	reordered[0], reordered[1] = reordered[1], reordered[0]
	if IsLegacyDefaultLoginPatterns(reordered) {
		t.Fatal("reordered list must NOT be treated as the legacy defaults")
	}
	if IsLegacyDefaultLoginPatterns(legacyDefaultLoginPatterns[:len(legacyDefaultLoginPatterns)-1]) {
		t.Fatal("truncated list must NOT be treated as the legacy defaults")
	}
	if IsLegacyDefaultLoginPatterns(nil) {
		t.Fatal("empty list must NOT be treated as the legacy defaults")
	}
	if IsLegacyDefaultLoginPatterns(defaultLoginPatterns) {
		t.Fatal("the CURRENT defaults must NOT be treated as the legacy defaults")
	}
}
