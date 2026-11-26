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
// a test that sets them the "wrong" way round really does (#20066: the carry
// must not depend on mtime at all).
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

// setupOverlayCarry writes the two layer files the way a save leaves them,
// points the package's PVC paths at a temp dir and enters Kubernetes mode.
//
// overlayCurrent: the overlay's header records the config file's content, so
// the config file has not been edited since the overlay was saved and the
// overlay holds the latest edit. Otherwise the header records other content:
// the config file was edited after the overlay was saved.
//
// The mtimes are set the opposite way on purpose (#20066): a current overlay
// is OLDER than the config file, as on every Kubernetes boot, and a stale one
// is newer. Every test built on this helper therefore also shows that the
// decision ignores mtime.
func setupOverlayCarry(t *testing.T, seed, overlay string, overlayCurrent bool) overlayCarryFiles {
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
	headerFor := seed
	if !overlayCurrent {
		headerFor = seed + "# edited after the overlay was saved\n"
	}
	header := overlayBaseHeader(sumBytes([]byte(headerFor)))
	if err := os.WriteFile(f.overlay, append(header, overlay...), 0o600); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	overlayTime, seedTime := base, base.Add(overlayCarryMtimeStep)
	if !overlayCurrent {
		overlayTime, seedTime = seedTime, overlayTime
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

// TestReloadTreatsBlockMissingFromEditedConfigFileAsRemoved: a config file
// whose content changed after the overlay was saved holds the latest edit, so
// a block missing there was removed there. The overlay's copy is not
// resurrected, even though the overlay is the newer file, and the log names
// it.
func TestReloadTreatsBlockMissingFromEditedConfigFileAsRemoved(t *testing.T) {
	f := setupOverlayCarry(t, overlayCarrySeed, overlayCarryOverlay, false)
	logs := captureConfigLog(t)
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if cfg.Ask.Enabled != nil || cfg.Ask.Mode != "" {
		t.Errorf("ask removed from the edited config file came back from the overlay: enabled=%s mode=%q", boolPtrValue(cfg.Ask.Enabled), cfg.Ask.Mode)
	}
	if !strings.Contains(logs.String(), `block "ask" differs`) || !strings.Contains(logs.String(), "counts as removed") {
		t.Errorf("no log line for the overlay block that was not adopted; log:\n%s", logs.String())
	}
}

// TestReloadKeepsEditedConfigFileBlockAndSaysSo: when both files carry a
// block and the config file was edited after the overlay was saved, the
// config file wins, and the log names the overlay block that was not adopted
// (never silent).
func TestReloadKeepsEditedConfigFileBlockAndSaysSo(t *testing.T) {
	seed := overlayCarrySeed + "ask:\n  enabled: true\n  mode: hub\n"
	f := setupOverlayCarry(t, seed, overlayCarryOverlay, false)
	logs := captureConfigLog(t)
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if cfg.Ask.Enabled == nil || !*cfg.Ask.Enabled || cfg.Ask.Mode != "hub" {
		t.Errorf("edited config file's ask lost: enabled=%s mode=%q", boolPtrValue(cfg.Ask.Enabled), cfg.Ask.Mode)
	}
	if !strings.Contains(logs.String(), `block "ask" differs`) {
		t.Errorf("no log line for the overlay ask block that was not adopted; log:\n%s", logs.String())
	}
	if cfg.Ask.Mode == "direct" {
		t.Error("stale overlay's ask.mode adopted over the edited config file")
	}
}

// TestReloadAdoptsOverlayBlockWhole: the overlay block replaces the config
// file's block whole (the review and agent_sandbox rule), so a key removed in
// the overlay stays removed.
func TestReloadAdoptsOverlayBlockWhole(t *testing.T) {
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
	if cfg.Ask.Mode != "" {
		t.Errorf("ask.mode = %q, want empty: the overlay block, which has no mode, wins whole", cfg.Ask.Mode)
	}
}

// TestReloadRefusesOverlayBlockThatDoesNotValidate: an overlay edit must not
// get past the validation the config file gets, or the next persist writes a
// config file the next boot rejects. The block is left out, loudly, and the
// other blocks are still adopted.
func TestReloadRefusesOverlayBlockThatDoesNotValidate(t *testing.T) {
	overlay := overlayCarryOverlay + "ioscan:\n  classifier:\n    fail_mode: bogus\n"
	f := setupOverlayCarry(t, overlayCarrySeed, overlay, true)
	logs := captureConfigLog(t)
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if cfg.Ioscan.Classifier.FailMode == "bogus" {
		t.Error("ioscan.classifier.fail_mode: bogus adopted from the overlay without validation")
	}
	if !strings.Contains(logs.String(), `block "ioscan" is not adopted: it does not validate`) {
		t.Errorf("no loud log for the invalid overlay block; log:\n%s", logs.String())
	}
	assertCarriedBlocks(t, "valid blocks next to an invalid one", cfg)
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := Load(f.seed); err != nil {
		t.Errorf("config file written after the reload does not load: %v", err)
	}
}

// TestReloadReappliesEnvOverAdoptedBlock: env still beats a block adopted
// from the overlay, as it beats the config file. The overlay leaves the
// dashboard token out when it came from env.
func TestReloadReappliesEnvOverAdoptedBlock(t *testing.T) {
	const envToken = "env-dashboard-token"
	t.Setenv("DASHBOARD_AUTH_TOKEN", envToken)
	seed := overlayCarrySeed + "dashboard:\n  port: 3001\n"
	overlay := overlayCarrySeed + "dashboard:\n  port: 3002\n  auth_token: \"\"\n"
	f := setupOverlayCarry(t, seed, overlay, true)
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if cfg.Dashboard.Port != 3002 {
		t.Errorf("dashboard.port = %d, want the overlay's 3002", cfg.Dashboard.Port)
	}
	if cfg.Dashboard.AuthToken != envToken {
		t.Errorf("dashboard.auth_token = %q, want the env token re-applied over the adopted block", cfg.Dashboard.AuthToken)
	}
}

// TestOverlayExpansionIgnoresOverlayVariables: the overlay is expanded with
// the config file's resolver policy, never its own variables block.
func TestOverlayExpansionIgnoresOverlayVariables(t *testing.T) {
	overlay := overlayCarrySeed + `
variables:
  security:
    allow_exec: true
  defs:
    LEAKED:
      type: script
      command: [echo, from-overlay-exec]
      scope: both
ask:
  mode: ${LEAKED}
`
	decoded, err := decodeDashboardOverlay([]byte(overlay), []byte(overlayCarrySeed))
	if err != nil {
		t.Fatalf("decodeDashboardOverlay: %v", err)
	}
	if strings.Contains(decoded.cfg.Ask.Mode, "from-overlay-exec") {
		t.Errorf("overlay variables resolver ran while expanding the overlay: ask.mode = %q", decoded.cfg.Ask.Mode)
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
	written, err := os.ReadFile(f.overlay)
	if err != nil {
		t.Fatal(err)
	}
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
	if !bytes.Equal(data, written) {
		t.Errorf("unparseable overlay overwritten by persist:\n%s", data)
	}
	if _, err := os.Stat(f.runtime); err != nil {
		t.Errorf("runtime config not written while the overlay was kept: %v", err)
	}

	// Once fixed (the operator keeps the header line, which names the config
	// file the save above wrote), the overlay loads and saves normally again.
	seedNow, err := os.ReadFile(f.seed)
	if err != nil {
		t.Fatal(err)
	}
	fixed := string(overlayBaseHeader(sumBytes(seedNow))) + overlayCarrySeed + "ask: {enabled: false}\n"
	if err := os.WriteFile(f.overlay, []byte(fixed), 0o600); err != nil {
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
// keys stay with the config file even when the overlay holds the latest edit.
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

// hostedSpokeFiles lays out a hosted spoke's config files in a temp dir:
// /etc/hive/hive.yaml is a link to the PVC runtime config (#17468), next to
// the dashboard overlay. The package's PVC paths point at it, in Kubernetes
// mode, and the spoke has booted once and saved, so both layers exist with
// the overlay header a save writes.
func hostedSpokeFiles(t *testing.T, seed string) overlayCarryFiles {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "etc"), 0o700); err != nil {
		t.Fatal(err)
	}
	f := overlayCarryFiles{
		seed:    filepath.Join(dir, "etc", "hive.yaml"),
		overlay: filepath.Join(dir, "hive.yaml.dashboard"),
		runtime: filepath.Join(dir, "hive.yaml.runtime"),
	}
	if err := os.WriteFile(f.runtime, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.runtime, f.seed); err != nil {
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

// bootCopySameContent is what the boot chain does to the config file on
// every Kubernetes boot: the content is rewritten unchanged (the entrypoint's
// cp of the runtime config, or the init container's re-link) and the file
// ends up NEWER than the overlay.
func bootCopySameContent(t *testing.T, f overlayCarryFiles) {
	t.Helper()
	data, err := os.ReadFile(f.seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.seed, data, 0o600); err != nil {
		t.Fatal(err)
	}
	overlayTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(f.overlay, overlayTime, overlayTime); err != nil {
		t.Fatal(err)
	}
	later := overlayTime.Add(overlayCarryMtimeStep)
	if err := os.Chtimes(f.seed, later, later); err != nil {
		t.Fatal(err)
	}
}

// TestHostedRestartKeepsDashboardSavedSettings is #20066 as it happened: a
// dashboard PUT changes customers.store and reporter_trust.trusted_logins and
// saves, the boot chain rewrites the config file with the same content and a
// newer mtime, and the hive boots. Both settings must be there after the boot
// and after the boot's own save, in every layer.
func TestHostedRestartKeepsDashboardSavedSettings(t *testing.T) {
	f := hostedSpokeFiles(t, overlayCarrySeed)
	live, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	// PUT /api/config/governor/features and /governor/labels.
	live.Customers.Store = CustomersStoreColony
	live.Project.IssueFilter.ReporterTrust.TrustedLogins = []string{"trusted-maintainer"}
	if err := live.Save(); err != nil {
		t.Fatalf("dashboard save: %v", err)
	}

	bootCopySameContent(t, f)
	booted, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("boot after restart: %v", err)
	}
	assertSavedSettings := func(stage string, cfg *Config) {
		t.Helper()
		if cfg.Customers.Store != CustomersStoreColony {
			t.Errorf("%s: customers.store = %q, want %q", stage, cfg.Customers.Store, CustomersStoreColony)
		}
		if got := cfg.Project.IssueFilter.ReporterTrust.TrustedLogins; len(got) != 1 || got[0] != "trusted-maintainer" {
			t.Errorf("%s: reporter_trust.trusted_logins = %v, want [trusted-maintainer]", stage, got)
		}
	}
	assertSavedSettings("after restart", booted)

	// The boot's own save ("dashboard overlay written") must not erase them.
	if err := booted.Save(); err != nil {
		t.Fatalf("boot save: %v", err)
	}
	for _, layer := range []string{f.overlay, f.runtime} {
		assertSavedSettings("after the boot save, "+filepath.Base(layer), parseLayer(t, layer))
	}
	if fi, err := os.Lstat(f.seed); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("config path is no longer a link to the runtime config: %v", err)
	}
}

// TestHostedRestartKeepsOverlayOnlyBlock: the #19740 case on a hosted spoke.
// After a save, the operator edits a block into the overlay, then the boot
// chain rewrites the config file with the same content and a newer mtime.
// The config file was not edited, so the overlay's block is the latest edit
// and survives the boot and the boot's save.
func TestHostedRestartKeepsOverlayOnlyBlock(t *testing.T) {
	f := hostedSpokeFiles(t, overlayCarrySeed)
	live, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if err := live.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	overlay, err := os.ReadFile(f.overlay)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := parseOverlayBase(overlay); !ok {
		t.Fatalf("save wrote no overlay header:\n%s", overlay)
	}
	edited := append(overlay, "ask:\n  enabled: false\n  mode: direct\n"...)
	if err := os.WriteFile(f.overlay, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	bootCopySameContent(t, f)
	booted, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if booted.Ask.Mode != "direct" || booted.Ask.Enabled == nil || *booted.Ask.Enabled {
		t.Fatalf("overlay ask dropped at boot because the config file was newer: enabled=%s mode=%q", boolPtrValue(booted.Ask.Enabled), booted.Ask.Mode)
	}
	if err := booted.Save(); err != nil {
		t.Fatalf("boot save: %v", err)
	}
	for _, layer := range []string{f.overlay, f.runtime} {
		if got := parseLayer(t, layer); got.Ask.Mode != "direct" {
			t.Errorf("after the boot save, %s: ask.mode = %q, want direct", filepath.Base(layer), got.Ask.Mode)
		}
	}
}

// TestRestartHonoursBlockRemovedFromEditedConfigFile: after a save, the
// operator removes a block from the config file by hand (its content really
// changes). The deletion wins at the next boot, even though the overlay
// still has the block and is the NEWER file, and the boot's save takes the
// block out of the overlay too.
func TestRestartHonoursBlockRemovedFromEditedConfigFile(t *testing.T) {
	seed := overlayCarrySeed + "ask:\n  enabled: false\n  mode: direct\n"
	f := hostedSpokeFiles(t, seed)
	live, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if err := live.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got := parseLayer(t, f.overlay); got.Ask.Mode != "direct" {
		t.Fatalf("setup: overlay ask.mode = %q, want direct", got.Ask.Mode)
	}

	saved, err := os.ReadFile(f.seed)
	if err != nil {
		t.Fatal(err)
	}
	withoutAsk := removeTopLevelKey(t, saved, "ask")
	if err := os.WriteFile(f.seed, withoutAsk, 0o600); err != nil {
		t.Fatal(err)
	}
	configTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(f.seed, configTime, configTime); err != nil {
		t.Fatal(err)
	}
	later := configTime.Add(overlayCarryMtimeStep)
	if err := os.Chtimes(f.overlay, later, later); err != nil {
		t.Fatal(err)
	}

	booted, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("boot: %v", err)
	}
	if booted.Ask.Mode != "" || booted.Ask.Enabled != nil {
		t.Fatalf("ask removed from the config file came back from the overlay: enabled=%s mode=%q", boolPtrValue(booted.Ask.Enabled), booted.Ask.Mode)
	}
	if err := booted.Save(); err != nil {
		t.Fatalf("boot save: %v", err)
	}
	if got := parseLayer(t, f.overlay); got.Ask.Mode != "" {
		t.Errorf("overlay still carries the removed ask block after the boot save: mode = %q", got.Ask.Mode)
	}
}

// removeTopLevelKey returns data without the top-level key.
func removeTopLevelKey(t *testing.T, data []byte, key string) []byte {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			root.Content = append(root.Content[:i], root.Content[i+2:]...)
			out, err := yaml.Marshal(&doc)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	t.Fatalf("key %q not in the config file", key)
	return nil
}

// TestOverlayWithoutHeaderKeepsConfigFileBlocks: an overlay written by an
// older version (or from scratch by hand) says nothing about which config
// file it was saved against, so the config file wins, whatever the mtimes.
func TestOverlayWithoutHeaderKeepsConfigFileBlocks(t *testing.T) {
	f := setupOverlayCarry(t, overlayCarrySeed+"ask:\n  mode: hub\n", overlayCarryOverlay, true)
	if err := os.WriteFile(f.overlay, []byte(overlayCarryOverlay), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(overlayCarryMtimeStep)
	if err := os.Chtimes(f.overlay, later, later); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if cfg.Ask.Mode != "hub" {
		t.Errorf("ask.mode = %q, want the config file's hub: a header-less overlay must not win", cfg.Ask.Mode)
	}
}

// TestAdoptedDashboardBlockKeepsLoadedToken: the overlay blanks
// dashboard.auth_token when the token came from the pod env or the token
// file, so adopting its dashboard block must not blank the token the config
// file loaded.
func TestAdoptedDashboardBlockKeepsLoadedToken(t *testing.T) {
	t.Setenv("DASHBOARD_AUTH_TOKEN", "")
	t.Setenv("HIVE_DASHBOARD_TOKEN", "")
	seed := overlayCarrySeed + "dashboard:\n  port: 3001\n  auth_token: config-file-token\n"
	overlay := overlayCarrySeed + "dashboard:\n  port: 3002\n  auth_token: \"\"\n"
	f := setupOverlayCarry(t, seed, overlay, true)
	cfg, err := LoadWithDashboardOverlay(f.seed)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if cfg.Dashboard.Port != 3002 {
		t.Errorf("dashboard.port = %d, want the overlay's 3002", cfg.Dashboard.Port)
	}
	if cfg.Dashboard.AuthToken != "config-file-token" {
		t.Errorf("dashboard.auth_token = %q, want the config file's token kept", cfg.Dashboard.AuthToken)
	}
}

// TestParseOverlayBase covers the header line on its own.
func TestParseOverlayBase(t *testing.T) {
	sum := sumBytes([]byte("project: {org: o}\n"))
	header := overlayBaseHeader(sum)
	if !strings.HasPrefix(string(header), "#") {
		t.Fatalf("header %q is not a YAML comment", header)
	}
	got, ok := parseOverlayBase(append(header, "project: {org: o}\n"...))
	if !ok || got != sum {
		t.Errorf("round trip: ok=%v got=%x want=%x", ok, got, sum)
	}
	crlf := []byte(strings.TrimSuffix(string(header), "\n") + "\r\nproject: {}\n")
	if got, ok := parseOverlayBase(crlf); !ok || got != sum {
		t.Errorf("CRLF header not read: ok=%v", ok)
	}
	for name, data := range map[string]string{
		"no header":      "project: {org: o}\n",
		"not first line": "project: {}\n" + string(header),
		"bad hex":        overlayBaseHeaderPrefix + "zz\nproject: {}\n",
		"short":          overlayBaseHeaderPrefix + "abcd\n",
		"empty":          "",
	} {
		if _, ok := parseOverlayBase([]byte(data)); ok {
			t.Errorf("%s: header accepted", name)
		}
	}
	var doc map[string]any
	if err := yaml.Unmarshal(append(header, "project: {org: o}\n"...), &doc); err != nil || doc["project"] == nil {
		t.Errorf("overlay with header does not parse as YAML: %v %v", err, doc)
	}
}
