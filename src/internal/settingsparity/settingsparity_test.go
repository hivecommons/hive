package settingsparity

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

const fixtureConfig = `package config

import "time"

type Mode int

func helper() {}

var unused = 1

type Config struct {
	Name     string                    ` + "`yaml:\"name\"`" + `
	Port     *int                      ` + "`yaml:\"port,omitempty\"`" + `
	Agents   map[string]Agent          ` + "`yaml:\"agents\"`" + `
	Hooks    []*Hook                   ` + "`yaml:\"hooks\"`" + `
	Labels   map[string]string         ` + "`yaml:\"labels\"`" + `
	Nested   map[string]map[string]int ` + "`yaml:\"nested\"`" + `
	Matrix   [][]*int                  ` + "`yaml:\"matrix\"`" + `
	Inline   struct {
		Enabled bool ` + "`yaml:\"enabled\"`" + `
	} ` + "`yaml:\"inline\"`" + `
	Rows []struct {
		ID int ` + "`yaml:\"id\"`" + `
	} ` + "`yaml:\"rows\"`" + `
	Theme    theme.Palette   ` + "`json:\"theme\" yaml:\"theme\"`" + `
	Themes   []theme.Palette ` + "`yaml:\"themes\"`" + `
	Clock    time.Duration   ` + "`yaml:\"clock\"`" + `
	Zones    []time.Location ` + "`yaml:\"zones\"`" + `
	Callback func()          ` + "`yaml:\"callback\"`" + `
	Ignored  string          ` + "`yaml:\"-\"`" + `
	Inlined  string          ` + "`yaml:\",omitempty\"`" + `
	JSONOnly string          ` + "`json:\"json_only\"`" + `
	Untagged string
}

type Agent struct {
	Model  string ` + "`yaml:\"model\"`" + `
	Dashed string ` + "`yaml:\"-dashed\"`" + `
}

type Hook struct {
	URL string ` + "`yaml:\"url\"`" + `
}
`

// fixtureConfigTest redefines Hook; if _test.go files were not skipped it
// would replace the real Hook and leak hooks[].leaked into the leaves.
const fixtureConfigTest = `package config

type Hook struct {
	Leaked string ` + "`yaml:\"leaked\"`" + `
}
`

const fixtureTheme = `package theme

type Palette struct {
	Primary string ` + "`yaml:\"primary\"`" + `
}
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "src", "pkg", "config")
	writeFile(t, filepath.Join(configDir, "config.go"), fixtureConfig)
	writeFile(t, filepath.Join(configDir, "config_test.go"), fixtureConfigTest)
	writeFile(t, filepath.Join(configDir, "README.md"), "not go")
	writeFile(t, filepath.Join(root, "src", "pkg", "dashboard", "theme", "theme.go"), fixtureTheme)
	writeFile(t, filepath.Join(root, "src", "crux", "config", "doc.go"), "package config\n")
	return root
}

func TestConfigLeaves(t *testing.T) {
	leaves, err := ConfigLeaves(fixtureRepo(t))
	if err != nil {
		t.Fatalf("ConfigLeaves: %v", err)
	}
	want := []Leaf{
		{Path: "agents.<key>.model", Type: "string"},
		{Path: "agents.github_api_hourly_cap", Type: "int"},
		{Path: "agents.session_recycle.enabled", Type: "*bool"},
		{Path: "agents.session_recycle.max_age", Type: "time.Duration"},
		{Path: "agents.session_recycle.max_rss_mb", Type: "int"},
		{Path: "agents.session_recycle.min_idle", Type: "time.Duration"},
		{Path: "callback", Type: "unknown"},
		{Path: "clock", Type: "time.Duration"},
		{Path: "hooks[].url", Type: "string"},
		{Path: "inline.enabled", Type: "bool"},
		{Path: "labels.<key>", Type: "string"},
		{Path: "matrix[]", Type: "[]*int"},
		{Path: "name", Type: "string"},
		{Path: "nested.<key>", Type: "map[string]int"},
		{Path: "port", Type: "int"},
		{Path: "rows[].id", Type: "int"},
		{Path: "theme.primary", Type: "string"},
		{Path: "themes[].primary", Type: "string"},
		{Path: "zones[]", Type: "time.Location"},
	}
	if !reflect.DeepEqual(leaves, want) {
		t.Fatalf("ConfigLeaves mismatch\n got: %v\nwant: %v", leaves, want)
	}
}

func TestConfigLeavesErrors(t *testing.T) {
	t.Run("missing config dir", func(t *testing.T) {
		if _, err := ConfigLeaves(t.TempDir()); err == nil {
			t.Fatal("want an error when src/pkg/config does not exist")
		}
	})

	t.Run("unparseable source", func(t *testing.T) {
		root := fixtureRepo(t)
		writeFile(t, filepath.Join(root, "src", "pkg", "config", "broken.go"), "package config\nfunc {")
		if _, err := ConfigLeaves(root); err == nil {
			t.Fatal("want a parse error for a malformed Go file")
		}
	})
}

func TestConfigLeavesRealRepo(t *testing.T) {
	leaves, err := ConfigLeaves(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("ConfigLeaves on the real repo: %v", err)
	}
	if len(leaves) == 0 {
		t.Fatal("the real config.Config must yield leaves")
	}
	found := false
	for i, leaf := range leaves {
		if i > 0 && leaves[i-1].Path >= leaf.Path {
			t.Fatalf("leaves not sorted/unique at %q after %q", leaf.Path, leaves[i-1].Path)
		}
		if leaf.Path == "agents.github_api_hourly_cap" {
			found = true
		}
	}
	if !found {
		t.Fatal("agents.github_api_hourly_cap must always be reported")
	}
}

func TestAllowlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allow.txt")
	writeFile(t, path, "# header comment\n\n  a.b  \nc.d # why it is allowed\n")
	got, err := Allowlist(path)
	if err != nil {
		t.Fatalf("Allowlist: %v", err)
	}
	want := map[string]string{
		"a.b": "a.b",
		"c.d": "c.d # why it is allowed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Allowlist = %v, want %v", got, want)
	}

	if _, err := Allowlist(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Fatal("want an error for a missing allowlist file")
	}
}

// A field typed as another package's Config (config.Config.Writeback is a
// writeback.Config) must resolve to that package's struct, not back to the
// root Config by bare name — that collision recursed without bound.
func TestConfigLeavesQualifiedTypeDoesNotCollideWithRootConfig(t *testing.T) {
	root := fixtureRepo(t)
	writeFile(t, filepath.Join(root, "src", "go.mod"), "module example.com/hive\n")
	writeFile(t, filepath.Join(root, "src", "pkg", "config", "gate.go"), `package config

import (
	"time"

	"example.com/hive/pkg/writeback"
	tone "example.com/hive/pkg/dashboard/theme"
)

type Gate struct {
	Writeback writeback.Config `+"`yaml:\"writeback\"`"+`
	Tone      tone.Palette     `+"`yaml:\"tone\"`"+`
	Clock     time.Duration    `+"`yaml:\"clock\"`"+`
}
`)
	writeFile(t, filepath.Join(root, "src", "pkg", "writeback", "config.go"), `package writeback

type Config struct {
	Mode string `+"`yaml:\"mode\"`"+`
}
`)
	// Point the fixture Config at Gate so the walk reaches the qualified types.
	writeFile(t, filepath.Join(root, "src", "pkg", "config", "config.go"), `package config

type Config struct {
	Gate Gate `+"`yaml:\"gate\"`"+`
}
`)
	leaves, err := ConfigLeaves(root)
	if err != nil {
		t.Fatalf("ConfigLeaves: %v", err)
	}
	got := map[string]string{}
	for _, l := range leaves {
		got[l.Path] = l.Type
	}
	for path, typ := range map[string]string{
		"gate.writeback.mode": "string",
		"gate.tone.primary":   "string",
		"gate.clock":          "time.Duration",
	} {
		if got[path] != typ {
			t.Errorf("leaf %q = %q, want %q (all: %v)", path, got[path], typ, leaves)
		}
	}
	if _, ok := got["gate.writeback.gate"]; ok {
		t.Fatal("writeback.Config resolved to the root Config and recursed")
	}
}
