package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// hivecommons/hive#19740: a block hand-edited into the dashboard overlay must
// survive the watcher's reload and the next persist, for every block the
// schema knows, and a block the hive cannot load must never be erased.

// overlayCarryMtimeStep separates the two layer files' modification times so
// "which file is newer" never depends on filesystem timestamp granularity.
const overlayCarryMtimeStep = time.Minute

// overlayCarrySeed is the config file (/etc/hive/hive.yaml, a link to the
// runtime config on hosted spokes). It has no ask, chat or customers block.
const overlayCarrySeed = `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
agents:
  scanner:
    backend: copilot
    model: claude-sonnet-4-6
`

// overlayCarryOverlay is the overlay after the operator appended blocks by
// hand: ask (the incident), chat and customers.
const overlayCarryOverlay = overlayCarrySeed + `
ask:
  enabled: false
  mode: direct
chat:
  grounded_answers:
    enabled: true
    model: overlay-model
customers:
  enabled: false
`

type overlayCarryFiles struct {
	seed, overlay, runtime string
}

// setupOverlayCarry writes the two layer files, points the package's PVC
// paths at a temp dir, enters Kubernetes mode and makes the overlay newer
// than the config file (overlayNewer) or older.
func setupOverlayCarry(t *testing.T, seed, overlay string, overlayNewer bool) overlayCarryFiles {
	t.Helper()
	dir := t.TempDir()
	f := overlayCarryFiles{
		seed:    filepath.Join(dir, "hive.yaml"),
		overlay: filepath.Join(dir, "hive.yaml.dashboard"),
		runtime: filepath.Join(dir, "hive.yaml.runtime"),
	}
	if err := os.WriteFile(f.seed, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.overlay, []byte(overlay), 0o600); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	seedTime, overlayTime := base, base.Add(overlayCarryMtimeStep)
	if !overlayNewer {
		seedTime, overlayTime = overlayTime, seedTime
	}
	if err := os.Chtimes(f.seed, seedTime, seedTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(f.overlay, overlayTime, overlayTime); err != nil {
		t.Fatal(err)
	}
	origOverlay, origRuntime, origAgents := DashboardOverlayFile, RuntimeConfigFile, DefaultAgentOverlayDir
	DashboardOverlayFile, RuntimeConfigFile = f.overlay, f.runtime
	DefaultAgentOverlayDir = filepath.Join(dir, "agent-configs")
	t.Cleanup(func() {
		DashboardOverlayFile, RuntimeConfigFile, DefaultAgentOverlayDir = origOverlay, origRuntime, origAgents
	})
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	resetOverlayWarnings(t)
	return f
}

// resetOverlayWarnings clears the once-per-content warning record so each
// test sees its own warnings.
func resetOverlayWarnings(t *testing.T) {
	t.Helper()
	reset := func() {
		overlayWarnedMu.Lock()
		overlayWarned = map[string]contentSum{}
		overlayWarnedMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func boolPtrValue(p *bool) string {
	if p == nil {
		return "nil"
	}
	if *p {
		return "true"
	}
	return "false"
}

// assertCarriedBlocks checks the three hand-edited blocks in a loaded config.
func assertCarriedBlocks(t *testing.T, stage string, cfg *Config) {
	t.Helper()
	if cfg.Ask.Enabled == nil || *cfg.Ask.Enabled {
		t.Errorf("%s: ask.enabled = %s, want false (hand edit dropped)", stage, boolPtrValue(cfg.Ask.Enabled))
	}
	if cfg.Ask.Mode != "direct" {
		t.Errorf("%s: ask.mode = %q, want direct", stage, cfg.Ask.Mode)
	}
	ga := cfg.Chat.GroundedAnswers
	if ga.Enabled == nil || !*ga.Enabled || ga.Model != "overlay-model" {
		t.Errorf("%s: chat.grounded_answers = {enabled:%s model:%q}, want {true overlay-model}", stage, boolPtrValue(ga.Enabled), ga.Model)
	}
	if cfg.Customers.Enabled == nil || *cfg.Customers.Enabled {
		t.Errorf("%s: customers.enabled = %s, want false (hand edit dropped)", stage, boolPtrValue(cfg.Customers.Enabled))
	}
}

// parseLayer decodes a layer file the way the loader does.
func parseLayer(t *testing.T, path string) *Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return &cfg
}

// TestReloadCarriesHandEditedOverlayBlocksThroughPersist is the incident: the
// reload must carry ask (and chat, customers) from the overlay, and the next
// persist must write them to every layer instead of erasing them.
func TestReloadCarriesHandEditedOverlayBlocksThroughPersist(t *testing.T) {
	f := setupOverlayCarry(t, overlayCarrySeed, overlayCarryOverlay, true)

	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	assertCarriedBlocks(t, "reload", cfg)
	if _, ok := cfg.Agents["scanner"]; !ok {
		t.Fatalf("seed agents lost: %v", cfg.Agents)
	}

	// The governor tick's persist.
	if err := cfg.ReconcilePausedAndSave(nil); err != nil {
		t.Fatalf("persist: %v", err)
	}
	for _, layer := range []string{f.overlay, f.seed, f.runtime} {
		assertCarriedBlocks(t, "after persist, "+filepath.Base(layer), parseLayer(t, layer))
	}

	// And the next reload (now from the rewritten files) still has them.
	again, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("second reload: %v", err)
	}
	assertCarriedBlocks(t, "second reload", again)
}

// TestReloadCarriesOverlayBlocksForHubLoader: the hub loader shares the path.
func TestReloadCarriesOverlayBlocksForHubLoader(t *testing.T) {
	f := setupOverlayCarry(t, overlayCarrySeed, overlayCarryOverlay, true)
	cfg, err := LoadWithDashboardOverlayForHub(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlayForHub: %v", err)
	}
	assertCarriedBlocks(t, "hub reload", cfg)
}

// TestReloadCarriesOverlayBlockMissingFromConfigFileEvenWhenOlder: a block
// the config file does not have has nothing to lose, so it is carried even
// when the config file was written later (a read-only seed mount, a
// ConfigMap remount).
func TestReloadCarriesOverlayBlockMissingFromConfigFileEvenWhenOlder(t *testing.T) {
	f := setupOverlayCarry(t, overlayCarrySeed, overlayCarryOverlay, false)
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	assertCarriedBlocks(t, "reload with older overlay", cfg)
}

// TestReloadKeepsNewerConfigFileBlockAndSaysSo: when both files carry a
// block and the config file is the newer edit, the config file wins, and the
// log names the overlay block that was not adopted (never silent).
func TestReloadKeepsNewerConfigFileBlockAndSaysSo(t *testing.T) {
	seed := overlayCarrySeed + "ask:\n  enabled: true\n  mode: hub\n"
	f := setupOverlayCarry(t, seed, overlayCarryOverlay, false)
	logs := captureConfigLog(t)
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if cfg.Ask.Enabled == nil || !*cfg.Ask.Enabled || cfg.Ask.Mode != "hub" {
		t.Errorf("newer config file's ask lost: enabled=%s mode=%q", boolPtrValue(cfg.Ask.Enabled), cfg.Ask.Mode)
	}
	if !strings.Contains(logs.String(), `block "ask" differs`) {
		t.Errorf("no log line for the overlay ask block that was not adopted; log:\n%s", logs.String())
	}
	// customers is not in the config file, so it is still carried.
	if cfg.Customers.Enabled == nil || *cfg.Customers.Enabled {
		t.Errorf("customers.enabled = %s, want false", boolPtrValue(cfg.Customers.Enabled))
	}
}

// TestReloadMergesPartialOverlayBlockOntoConfigFileBlock: a partial hand edit
// (`ask: {enabled: false}`) changes only what it names.
func TestReloadMergesPartialOverlayBlockOntoConfigFileBlock(t *testing.T) {
	seed := overlayCarrySeed + "ask:\n  mode: hub\n"
	overlay := overlayCarrySeed + "ask: {enabled: false}\n"
	f := setupOverlayCarry(t, seed, overlay, true)
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if cfg.Ask.Enabled == nil || *cfg.Ask.Enabled {
		t.Errorf("ask.enabled = %s, want false", boolPtrValue(cfg.Ask.Enabled))
	}
	if cfg.Ask.Mode != "hub" {
		t.Errorf("ask.mode = %q, want the config file's hub kept", cfg.Ask.Mode)
	}
}

// TestPersistKeepsUnknownAndInvalidOverlayBlocks: a key this version does
// not know and a known key whose value does not parse are kept, as written,
// through the next persist. Neither costs the rest of the overlay.
func TestPersistKeepsUnknownAndInvalidOverlayBlocks(t *testing.T) {
	overlay := overlayCarrySeed + `
future_feature:
  knob: 7
ask:
  enabled: not-a-bool
customers:
  enabled: false
`
	f := setupOverlayCarry(t, overlayCarrySeed, overlay, true)
	logs := captureConfigLog(t)
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if cfg.Customers.Enabled == nil || *cfg.Customers.Enabled {
		t.Errorf("customers lost because another block did not parse: %s", boolPtrValue(cfg.Customers.Enabled))
	}
	for _, key := range []string{`"future_feature"`, `"ask"`} {
		if !strings.Contains(logs.String(), "block "+key+" is not loaded") {
			t.Errorf("no loud log for overlay block %s; log:\n%s", key, logs.String())
		}
	}

	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	var written map[string]any
	data, err := os.ReadFile(f.overlay)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &written); err != nil {
		t.Fatalf("overlay after save is not YAML: %v\n%s", err, data)
	}
	ff, ok := written["future_feature"].(map[string]any)
	if !ok || ff["knob"] != 7 {
		t.Errorf("unknown block erased by persist: future_feature = %#v", written["future_feature"])
	}
	ask, ok := written["ask"].(map[string]any)
	if !ok || ask["enabled"] != "not-a-bool" {
		t.Errorf("unparseable ask block overwritten by persist: ask = %#v", written["ask"])
	}
	cust, ok := written["customers"].(map[string]any)
	if !ok || cust["enabled"] != false {
		t.Errorf("customers block lost by persist: %#v", written["customers"])
	}

	// A second load and save cycle keeps them too.
	again, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("second reload: %v", err)
	}
	if err := again.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	data, err = os.ReadFile(f.overlay)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("future_feature")) || !bytes.Contains(data, []byte("not-a-bool")) {
		t.Errorf("preserved blocks lost on the second cycle:\n%s", data)
	}
}

// TestPersistLeavesUnparseableOverlayFileAlone: an overlay that is not YAML
// at all is an edit in progress. The reload falls back to the config file,
// says so loudly, and the persist writes the other layers but not this one.
func TestPersistLeavesUnparseableOverlayFileAlone(t *testing.T) {
	broken := overlayCarrySeed + "ask: {enabled: false\n"
	f := setupOverlayCarry(t, overlayCarrySeed, broken, true)
	logs := captureConfigLog(t)
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if !strings.Contains(logs.String(), "is not valid YAML") {
		t.Errorf("no loud log for the unparseable overlay; log:\n%s", logs.String())
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(f.overlay)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != broken {
		t.Errorf("unparseable overlay overwritten by persist:\n%s", data)
	}
	if _, err := os.Stat(f.runtime); err != nil {
		t.Errorf("runtime config not written while the overlay was kept: %v", err)
	}

	// Once fixed, the overlay loads and saves normally again.
	fixed := overlayCarrySeed + "ask: {enabled: false}\n"
	if err := os.WriteFile(f.overlay, []byte(fixed), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(overlayCarryMtimeStep)
	if err := os.Chtimes(f.overlay, later, later); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("reload after fix: %v", err)
	}
	if reloaded.Ask.Enabled == nil || *reloaded.Ask.Enabled {
		t.Errorf("fixed overlay's ask not adopted: %s", boolPtrValue(reloaded.Ask.Enabled))
	}
	if err := reloaded.Save(); err != nil {
		t.Fatalf("Save after fix: %v", err)
	}
	if got := parseLayer(t, f.overlay); got.Ask.Enabled == nil || *got.Ask.Enabled {
		t.Errorf("overlay after fix and save: ask.enabled = %s, want false", boolPtrValue(got.Ask.Enabled))
	}
}

// TestGenericCarryNeverAdoptsSeedOwnedBlocks: the security and hub-managed
// keys stay with the config file even when the overlay is newer.
func TestGenericCarryNeverAdoptsSeedOwnedBlocks(t *testing.T) {
	seed := overlayCarrySeed + "hive_id: seed-id\n"
	overlay := overlayCarrySeed + "hive_id: overlay-id\nvariables:\n  security:\n    allow_exec: true\n"
	f := setupOverlayCarry(t, seed, overlay, true)
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if cfg.HiveID != "seed-id" {
		t.Errorf("hive_id = %q, want seed-id (seed-owned)", cfg.HiveID)
	}
	if cfg.Variables.Security.AllowExec {
		t.Error("overlay variables.security.allow_exec adopted; variables must stay seed-only")
	}
}

// TestOverlayCarryKeyListsMatchSchema keeps the two exclusion lists honest:
// every excluded key is a real top-level key (a typo would silently stop
// excluding it), and the blocks the dashboard writes that #19740 is about
// are carried generically, not excluded.
func TestOverlayCarryKeyListsMatchSchema(t *testing.T) {
	known := configTopLevelKeys()
	for key := range overlayHandledKeys {
		if _, ok := known[key]; !ok {
			t.Errorf("overlayHandledKeys has %q, which is not a top-level config key", key)
		}
	}
	for key := range overlaySeedOwnedKeys {
		if _, ok := known[key]; !ok {
			t.Errorf("overlaySeedOwnedKeys has %q, which is not a top-level config key", key)
		}
		if overlayHandledKeys[key] {
			t.Errorf("%q is in both exclusion lists", key)
		}
	}
	for _, key := range []string{"ask", "chat", "customers", "notifications", "dashboard", "backends", "providers"} {
		if _, ok := known[key]; !ok {
			t.Errorf("schema walk missed top-level key %q", key)
		}
		if _, owned := overlaySeedOwnedKeys[key]; owned || overlayHandledKeys[key] {
			t.Errorf("%q must be carried generically, not excluded", key)
		}
	}
}

// TestSpliceOverlayBlocksReplacesAndAppends covers the splice on its own.
func TestSpliceOverlayBlocksReplacesAndAppends(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("ask:\n  enabled: maybe\nextra: [1, 2]\n"), &doc); err != nil {
		t.Fatal(err)
	}
	vals := mappingValues(doc.Content[0])
	blocks := []preservedOverlayBlock{{key: "ask", value: vals["ask"]}, {key: "extra", value: vals["extra"]}}
	out, err := spliceOverlayBlocks([]byte("project:\n    org: o\nask:\n    mode: hub\n"), blocks)
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("spliced output is not YAML: %v", err)
	}
	if ask, _ := got["ask"].(map[string]any); ask["enabled"] != "maybe" || ask["mode"] != nil {
		t.Errorf("ask not replaced by the preserved block: %#v", got["ask"])
	}
	if extra, _ := got["extra"].([]any); len(extra) != 2 {
		t.Errorf("extra not appended: %#v", got["extra"])
	}
	if proj, _ := got["project"].(map[string]any); proj["org"] != "o" {
		t.Errorf("marshaled keys lost: %#v", got["project"])
	}
	if same, err := spliceOverlayBlocks([]byte("a: 1\n"), nil); err != nil || string(same) != "a: 1\n" {
		t.Errorf("no blocks must return the input unchanged: %q %v", same, err)
	}
}
