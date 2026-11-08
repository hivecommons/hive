package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/config"
)

// readBobSettings decodes the settings file written under home.
func readBobSettings(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(bobSettingsPath(home))
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return got
}

func readBobV2Settings(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(bobV2SettingsPath(home))
	if err != nil {
		t.Fatalf("read v2 settings: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal v2 settings: %v", err)
	}
	return got
}

// authBlock digs out security.auth, failing if the shape is wrong.
func authBlock(t *testing.T, settings map[string]any) map[string]any {
	t.Helper()
	security, ok := settings[config.BobSettingsSecurityKey].(map[string]any)
	if !ok {
		t.Fatalf("settings[%q] is not an object: %#v", config.BobSettingsSecurityKey, settings)
	}
	auth, ok := security[config.BobSettingsAuthKey].(map[string]any)
	if !ok {
		t.Fatalf("security[%q] is not an object: %#v", config.BobSettingsAuthKey, security)
	}
	return auth
}

// assertAPIKeyAuth pins both hive-owned values.
func assertAPIKeyAuth(t *testing.T, auth map[string]any) {
	t.Helper()
	for _, key := range []string{
		config.BobSettingsSelectedTypeKey,
		config.BobSettingsEnforcedTypeKey,
	} {
		if got, _ := auth[key].(string); got != config.BobAuthTypeAPIKey {
			t.Errorf("auth[%q] = %q, want %q", key, got, config.BobAuthTypeAPIKey)
		}
	}
}

func assertBobLegacyUpdateDisabled(t *testing.T, settings map[string]any) {
	t.Helper()
	for _, key := range []string{
		config.BobLegacyDisableAutoUpdateKey,
		config.BobLegacyDisableUpdateNagKey,
	} {
		if got, _ := settings[key].(bool); !got {
			t.Errorf("settings[%q] = %#v, want true", key, settings[key])
		}
	}
	general, ok := settings[config.BobLegacyGeneralKey].(map[string]any)
	if !ok {
		t.Fatalf("settings[%q] is not an object: %#v", config.BobLegacyGeneralKey, settings[config.BobLegacyGeneralKey])
	}
	for _, key := range []string{
		config.BobLegacyDisableAutoUpdateKey,
		config.BobLegacyDisableUpdateNagKey,
	} {
		if got, _ := general[key].(bool); !got {
			t.Errorf("general[%q] = %#v, want true", key, general[key])
		}
	}
}

func assertBobV2AutoUpdateDisabled(t *testing.T, settings map[string]any) {
	t.Helper()
	shell, ok := settings[config.BobV2ShellKey].(map[string]any)
	if !ok {
		t.Fatalf("v2 settings[%q] is not an object: %#v", config.BobV2ShellKey, settings[config.BobV2ShellKey])
	}
	if got, _ := shell[config.BobV2AutoUpdateKey].(bool); got {
		t.Errorf("bobShell.autoUpdate = true, want false")
	} else if _, ok := shell[config.BobV2AutoUpdateKey]; !ok {
		t.Errorf("bobShell.autoUpdate missing, want false")
	}
}

// writeBobSettings seeds a settings file with the given raw JSON.
func writeBobSettings(t *testing.T, home, raw string) {
	t.Helper()
	path := bobSettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), config.BobSettingsDirMode); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(raw), config.BobSettingsFileMode); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
}

// TestEnsureBobAuthSettingsCreatesFile covers the fresh-PVC case: no .bob dir
// at all. bob must still find api-key auth already selected and enforced.
func TestEnsureBobAuthSettingsCreatesFile(t *testing.T) {
	home := t.TempDir()
	m := &Manager{logger: discardLogger()}

	m.ensureBobAuthSettings("tester", home)

	gotV1 := readBobSettings(t, home)
	assertAPIKeyAuth(t, authBlock(t, gotV1))
	assertBobLegacyUpdateDisabled(t, gotV1)
	gotV2 := readBobV2Settings(t, home)
	if got := gotV2[config.BobV2ProviderKey]; got != config.BobV2ProviderHarness {
		t.Errorf("v2 provider = %#v, want %q", got, config.BobV2ProviderHarness)
	}
	assertBobV2AutoUpdateDisabled(t, gotV2)
}

// TestEnsureBobAuthSettingsOverwritesSSO is the core regression guard: this is
// the exact on-disk state observed on the live spoke, where one agent had
// picked SSO at the prompt and thereby parked every bob agent on the hive.
// Unrelated keys must survive untouched — clobbering licenseConsent in
// particular would make bob hard-error before it does any work.
func TestEnsureBobAuthSettingsOverwritesSSO(t *testing.T) {
	home := t.TempDir()
	writeBobSettings(t, home, `{
	  "instanceId": "inst-123",
	  "teamId": "team-456",
	  "licenseConsent": true,
	  "isNotFirstTime": true,
	  "security": {"auth": {"selectedType": "sso"}},
	  "ibm_secrets": {},
	  "prev_version": "1.0.6"
	}`)

	m := &Manager{logger: discardLogger()}
	m.ensureBobAuthSettings("tester", home)

	got := readBobSettings(t, home)
	assertAPIKeyAuth(t, authBlock(t, got))
	assertBobLegacyUpdateDisabled(t, got)

	for key, want := range map[string]any{
		"instanceId":     "inst-123",
		"teamId":         "team-456",
		"licenseConsent": true,
		"isNotFirstTime": true,
		"prev_version":   "1.0.6",
	} {
		if got[key] != want {
			t.Errorf("preserved key %q = %#v, want %#v", key, got[key], want)
		}
	}
	if _, ok := got["ibm_secrets"]; !ok {
		t.Error("ibm_secrets key was dropped; unrelated keys must be preserved")
	}
}

func TestEnsureBobAuthSettingsOverwritesV2ProviderAndDisablesAutoUpdate(t *testing.T) {
	home := t.TempDir()
	path := bobV2SettingsPath(home)
	if err := os.MkdirAll(filepath.Dir(path), config.BobSettingsDirMode); err != nil {
		t.Fatalf("mkdir v2 settings: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"provider":"openrouter","bobShell":{"autoUpdate":true,"lastRunVersion":"2.0.4"},"session":{"maxTurns":7}}`), config.BobSettingsFileMode); err != nil {
		t.Fatalf("seed v2 settings: %v", err)
	}

	m := &Manager{logger: discardLogger()}
	m.ensureBobAuthSettings("tester", home)

	got := readBobV2Settings(t, home)
	if got[config.BobV2ProviderKey] != config.BobV2ProviderHarness {
		t.Errorf("v2 provider = %#v, want %q", got[config.BobV2ProviderKey], config.BobV2ProviderHarness)
	}
	assertBobV2AutoUpdateDisabled(t, got)
	if shell, _ := got[config.BobV2ShellKey].(map[string]any); shell["lastRunVersion"] != "2.0.4" {
		t.Errorf("v2 bobShell sibling settings were not preserved: %#v", got[config.BobV2ShellKey])
	}
	if session, _ := got["session"].(map[string]any); session["maxTurns"] != float64(7) {
		t.Errorf("v2 session settings were not preserved: %#v", got["session"])
	}
}

func TestEnsureBobAuthSettingsPreservesLegacyUpdateSiblings(t *testing.T) {
	home := t.TempDir()
	writeBobSettings(t, home, `{
	  "general": {"theme": "dark", "disableAutoUpdate": false},
	  "disableUpdateNag": false,
	  "security": {"auth": {"selectedType": "api-key", "enforcedType": "api-key"}}
	}`)

	m := &Manager{logger: discardLogger()}
	m.ensureBobAuthSettings("tester", home)

	got := readBobSettings(t, home)
	assertBobLegacyUpdateDisabled(t, got)
	general := got[config.BobLegacyGeneralKey].(map[string]any)
	if general["theme"] != "dark" {
		t.Errorf("general.theme = %#v, want dark", general["theme"])
	}
}

// TestEnsureBobAuthSettingsPreservesSiblingAuthKeys checks that the merge is
// scoped to the two keys hive owns, not to the whole auth object.
func TestEnsureBobAuthSettingsPreservesSiblingAuthKeys(t *testing.T) {
	home := t.TempDir()
	writeBobSettings(t, home, `{"security":{"auth":{"selectedType":"sso","useExternal":true},"folderTrust":{"x":1}}}`)

	m := &Manager{logger: discardLogger()}
	m.ensureBobAuthSettings("tester", home)

	got := readBobSettings(t, home)
	auth := authBlock(t, got)
	assertAPIKeyAuth(t, auth)
	if auth["useExternal"] != true {
		t.Errorf("security.auth.useExternal = %#v, want true", auth["useExternal"])
	}
	security := got[config.BobSettingsSecurityKey].(map[string]any)
	if _, ok := security["folderTrust"]; !ok {
		t.Error("security.folderTrust was dropped")
	}
}

// TestEnsureBobAuthSettingsIsIdempotent verifies re-assertion on every launch
// is cheap and stable: an already-correct file is left byte-identical, so
// repeated launches neither churn mtime nor race each other.
func TestEnsureBobAuthSettingsIsIdempotent(t *testing.T) {
	home := t.TempDir()
	m := &Manager{logger: discardLogger()}

	m.ensureBobAuthSettings("tester", home)
	first, err := os.ReadFile(bobSettingsPath(home))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	m.ensureBobAuthSettings("tester", home)
	second, err := os.ReadFile(bobSettingsPath(home))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(first) != string(second) {
		t.Errorf("second call rewrote the file:\nfirst  = %s\nsecond = %s", first, second)
	}
}

// TestEnsureBobAuthSettingsCorruptFile ensures a truncated/garbage file does
// not wedge every bob agent forever — it is rebuilt rather than propagated.
func TestEnsureBobAuthSettingsCorruptFile(t *testing.T) {
	home := t.TempDir()
	writeBobSettings(t, home, `{"security": {"auth": `)

	m := &Manager{logger: discardLogger()}
	m.ensureBobAuthSettings("tester", home)

	assertAPIKeyAuth(t, authBlock(t, readBobSettings(t, home)))
}

func TestEnsureBobAuthSettingsMalformedJSONRewritesWithSharedMode(t *testing.T) {
	home := t.TempDir()
	writeBobSettings(t, home, `{"security":`)
	if err := os.Chmod(bobSettingsPath(home), 0o600); err != nil {
		t.Fatalf("chmod seed settings: %v", err)
	}

	m := &Manager{logger: discardLogger()}
	m.ensureBobAuthSettings("tester", home)

	assertAPIKeyAuth(t, authBlock(t, readBobSettings(t, home)))
	info, err := os.Stat(bobSettingsPath(home))
	if err != nil {
		t.Fatalf("stat settings: %v", err)
	}
	if got := info.Mode().Perm(); got != config.BobSettingsFileMode {
		t.Errorf("rewritten malformed settings mode = %o, want %o", got, config.BobSettingsFileMode)
	}
}

// TestEnsureBobAuthSettingsWrongTypes covers intermediates of the wrong JSON
// type. There is no sane merge, so they are replaced rather than crashing.
func TestEnsureBobAuthSettingsWrongTypes(t *testing.T) {
	for name, raw := range map[string]string{
		"security is a string": `{"security": "yes"}`,
		"auth is a number":     `{"security": {"auth": 7}}`,
		"auth is null":         `{"security": {"auth": null}}`,
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			writeBobSettings(t, home, raw)

			m := &Manager{logger: discardLogger()}
			m.ensureBobAuthSettings("tester", home)

			assertAPIKeyAuth(t, authBlock(t, readBobSettings(t, home)))
		})
	}
}

// TestEnsureBobAuthSettingsGroupWritable pins the mode. The file is SHARED at
// /data/home/.bob (drwxrwx--- dev:node) and bob runs as agent UIDs in group
// node, so it must stay group-writable or the next agent's own startup write
// fails. This also documents why the file is NOT made read-only.
func TestEnsureBobAuthSettingsGroupWritable(t *testing.T) {
	home := t.TempDir()
	// Pre-create with a tight mode: WriteFile's perm argument only applies at
	// CREATE time, so without an explicit chmod this would stay 0600.
	writeBobSettings(t, home, `{"security":{"auth":{"selectedType":"sso"}}}`)
	if err := os.Chmod(bobSettingsPath(home), 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	m := &Manager{logger: discardLogger()}
	m.ensureBobAuthSettings("tester", home)

	info, err := os.Stat(bobSettingsPath(home))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != config.BobSettingsFileMode {
		t.Errorf("mode = %o, want %o", got, config.BobSettingsFileMode)
	}
}

// TestBobSettingsPathMatchesBundle pins the location bob itself computes:
// path.join(os.homedir(), ".bob", "settings.json").
func TestBobSettingsPathMatchesBundle(t *testing.T) {
	if got, want := bobSettingsPath("/data/home"), "/data/home/.bob/settings.json"; got != want {
		t.Errorf("bobSettingsPath() = %q, want %q", got, want)
	}
	if got, want := bobV2SettingsPath("/data/home"), "/data/home/.bob/settings/settings.json"; got != want {
		t.Errorf("bobV2SettingsPath() = %q, want %q", got, want)
	}
	// The shared HOME must match what agentEnvPairs exports, or hive would
	// write the auth block to a file bob never reads.
	if bobSharedHome != "/data/home" {
		t.Errorf("bobSharedHome = %q, want /data/home", bobSharedHome)
	}
}

// TestSetBobAuthBlockReportsChange guards the change-detection that makes the
// idempotent path a no-op.
func TestSetBobAuthBlockReportsChange(t *testing.T) {
	tests := []struct {
		name     string
		settings map[string]any
		want     bool
	}{
		{"empty needs write", map[string]any{}, true},
		{"sso needs write", map[string]any{
			config.BobSettingsSecurityKey: map[string]any{
				config.BobSettingsAuthKey: map[string]any{
					config.BobSettingsSelectedTypeKey: "sso",
				},
			},
		}, true},
		{"only selectedType set still needs enforcedType", map[string]any{
			config.BobSettingsSecurityKey: map[string]any{
				config.BobSettingsAuthKey: map[string]any{
					config.BobSettingsSelectedTypeKey: config.BobAuthTypeAPIKey,
				},
			},
		}, true},
		{"already correct is a no-op", map[string]any{
			config.BobSettingsSecurityKey: map[string]any{
				config.BobSettingsAuthKey: map[string]any{
					config.BobSettingsSelectedTypeKey: config.BobAuthTypeAPIKey,
					config.BobSettingsEnforcedTypeKey: config.BobAuthTypeAPIKey,
				},
			},
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := setBobAuthBlock(tc.settings); got != tc.want {
				t.Errorf("setBobAuthBlock() = %v, want %v", got, tc.want)
			}
			// Regardless of the return, the end state must be correct.
			assertAPIKeyAuth(t, authBlock(t, tc.settings))
		})
	}
}

// TestVerifyBobKeyReadable is the regression guard for the PRIMARY production
// bug: a dev-owned mode-0600 key file that the hive process can read but the
// agent UID cannot. ResolveAPIKey succeeds and the dashboard shows "key set",
// while bob gets EACCES and silently falls back to the SSO prompt.
func TestVerifyBobKeyReadable(t *testing.T) {
	tests := []struct {
		name     string
		dirMode  os.FileMode
		fileMode os.FileMode
		uid      int
		want     bool
	}{
		{"the production bug: 0700 dir, 0600 file", 0o700, 0o600, 2004, false},
		{"file readable but dir not traversable", 0o700, 0o640, 2004, false},
		{"dir traversable but file unreadable", 0o710, 0o600, 2004, false},
		{"the fix: 0710 dir, 0640 file", 0o710, 0o640, 2004, true},
		{"world-readable still works", 0o711, 0o644, 2004, true},
		{"root agent bypasses the check", 0o700, 0o600, 0, true},
		{"unset uid bypasses the check", 0o700, 0o600, -1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			keyDir := filepath.Join(dir, "secrets")
			if err := os.MkdirAll(keyDir, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			keyPath := filepath.Join(keyDir, "bob_api_key")
			if err := os.WriteFile(keyPath, []byte("not-a-real-key"), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := os.Chmod(keyPath, tc.fileMode); err != nil {
				t.Fatalf("chmod file: %v", err)
			}
			if err := os.Chmod(keyDir, tc.dirMode); err != nil {
				t.Fatalf("chmod dir: %v", err)
			}
			// Restore traversal so t.TempDir cleanup can remove the tree.
			t.Cleanup(func() { _ = os.Chmod(keyDir, 0o700) })

			m := &Manager{logger: discardLogger()}
			if got := m.verifyBobKeyReadable("tester", keyPath, tc.uid); got != tc.want {
				t.Errorf("verifyBobKeyReadable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVerifyBobKeyReadableMissingFile ensures a missing key file is reported
// rather than treated as fine.
func TestVerifyBobKeyReadableMissingFile(t *testing.T) {
	m := &Manager{logger: discardLogger()}
	if m.verifyBobKeyReadable("tester", filepath.Join(t.TempDir(), "nope"), 2004) {
		t.Error("verifyBobKeyReadable() = true for a missing file, want false")
	}
}

// TestVerifyBobKeyReadableNoPath covers the env-var key case: there is no file
// to permission-check, so the probe must not report a false failure.
func TestVerifyBobKeyReadableNoPath(t *testing.T) {
	m := &Manager{logger: discardLogger()}
	if !m.verifyBobKeyReadable("tester", "", 2004) {
		t.Error("verifyBobKeyReadable() = false for an env-var key, want true")
	}
}

// TestBobKeyFilePathParsesSource pins the source-string contract: only
// "file:" sources yield a path to permission-check.
func TestBobKeyFilePathParsesSource(t *testing.T) {
	tests := []struct {
		source string
		want   string
	}{
		{"file:/data/secrets/bob_api_key", "/data/secrets/bob_api_key"},
		{"env:HIVE_BOB_API_KEY", ""},
		{"", ""},
	}
	for _, tc := range tests {
		t.Run(tc.source, func(t *testing.T) {
			m := &Manager{logger: discardLogger()}
			src := tc.source
			m.SetBobKeySourceResolver(func() string { return src })
			if got := m.bobKeyFilePath(); got != tc.want {
				t.Errorf("bobKeyFilePath() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBobKeyFilePathNoResolver covers the tests/bare-setup path where no
// resolver was injected.
func TestBobKeyFilePathNoResolver(t *testing.T) {
	m := &Manager{logger: discardLogger()}
	if got := m.bobKeyFilePath(); got != "" {
		t.Errorf("bobKeyFilePath() = %q with no resolver, want empty", got)
	}
}

// TestVerifyBobStateDirsWritable pins the probe behind issue #2253: bob
// reports "error saving your latest settings changes" and "Failed to
// initialize logger:" only inside its own TUI, so hive must detect the
// underlying permission problem itself.
//
// The modes are the real ones: 0770 is what /data/home/.bob actually carries
// in production (drwxrwx--- dev:node), and 0700 is the failure mode where the
// dir is owned by dev but carries no group bits, which is exactly what locks
// out an agent UID that reaches it through group `node`.
func TestVerifyBobStateDirsWritable(t *testing.T) {
	tests := []struct {
		name           string
		mode           os.FileMode
		uid            int
		wantUnwritable bool
	}{
		{"production-correct 0770 is writable", 0o770, 2004, false},
		{"0700 locks out the agent UID", 0o700, 2004, true},
		{"0750 is readable but not writable", 0o750, 2004, true},
		{"0760 lacks group traverse", 0o760, 2004, true},
		{"root agent bypasses the check", 0o700, 0, false},
		{"unset uid bypasses the check", 0o700, -1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			bobDir := filepath.Dir(bobSettingsPath(home))
			if err := os.MkdirAll(bobDir, 0o770); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.Chmod(bobDir, tc.mode); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			// Restore traversal so t.TempDir cleanup can remove the tree.
			t.Cleanup(func() { _ = os.Chmod(bobDir, 0o770) })

			m := &Manager{logger: discardLogger()}
			// workDir points at a non-existent path on purpose: a state dir
			// bob has not created yet must NOT be reported, PROVIDED its
			// parent is group-writable so bob's recursive mkdir can succeed.
			// The parent is made 0770 explicitly (t.TempDir is 0700) to keep
			// this case isolating the $HOME/.bob mode under test — otherwise
			// the unwritable-parent check would fire on every row.
			workRoot := t.TempDir()
			if err := os.Chmod(workRoot, 0o770); err != nil {
				t.Fatalf("chmod workRoot: %v", err)
			}
			got := m.verifyBobStateDirsWritable("tester", home,
				filepath.Join(workRoot, "agents", "tester"), tc.uid)

			if gotUnwritable := len(got) > 0; gotUnwritable != tc.wantUnwritable {
				t.Errorf("verifyBobStateDirsWritable() unwritable=%v (%v), want %v",
					gotUnwritable, got, tc.wantUnwritable)
			}
		})
	}
}

// TestVerifyBobStateDirsWritableIgnoresMissingDirsWithWritableParent covers the
// fresh-PVC case where the state dirs do not exist yet but their parents ARE
// group-writable. bob creates them with mkdirSync recursive on first use, so a
// missing dir with a writable parent is not a fault and must not be reported.
func TestVerifyBobStateDirsWritableIgnoresMissingDirsWithWritableParent(t *testing.T) {
	home := t.TempDir()
	workDir := t.TempDir()
	// t.TempDir() is 0700; production parents are group-writable (2775).
	for _, d := range []string{home, workDir} {
		if err := os.Chmod(d, 0o770); err != nil {
			t.Fatalf("chmod %s: %v", d, err)
		}
	}
	m := &Manager{logger: discardLogger()}
	got := m.verifyBobStateDirsWritable("tester", home, workDir, 2004)
	if len(got) != 0 {
		t.Errorf("verifyBobStateDirsWritable() = %v for missing dirs with writable parents, want none", got)
	}
}

// TestVerifyBobStateDirsWritableFlagsMissingDirWithUnwritableParent is the
// regression test for #2284. On the reporting hive $HOME/.bob did not exist and
// $HOME itself was root-owned 0755, so bob's first-run mkdir failed with EACCES.
// The original probe skipped missing dirs outright and therefore reported "all
// clear" for exactly the failure it was added to catch.
func TestVerifyBobStateDirsWritableFlagsMissingDirWithUnwritableParent(t *testing.T) {
	home := t.TempDir()
	workDir := t.TempDir()
	// 0755: world-readable, NOT group-writable — the production $HOME mode.
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatalf("chmod home: %v", err)
	}
	if err := os.Chmod(workDir, 0o770); err != nil {
		t.Fatalf("chmod workDir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o770) })

	m := &Manager{logger: discardLogger()}
	got := m.verifyBobStateDirsWritable("tester", home, workDir, 2004)
	wantDir := filepath.Dir(bobSettingsPath(home))
	wantV2Dir := filepath.Dir(bobV2SettingsPath(home))
	if len(got) != 2 || got[0] != wantDir || got[1] != wantV2Dir {
		t.Errorf("verifyBobStateDirsWritable() = %v, want [%s %s]", got, wantDir, wantV2Dir)
	}
}

func TestVerifyBobStateDirsWritableFlagsMissingNestedParent(t *testing.T) {
	home := t.TempDir()
	homeBob := filepath.Dir(bobSettingsPath(home))
	if err := os.MkdirAll(homeBob, 0o770); err != nil {
		t.Fatalf("mkdir home .bob: %v", err)
	}
	if err := os.Chmod(homeBob, 0o770); err != nil {
		t.Fatalf("chmod home .bob: %v", err)
	}

	workRoot := t.TempDir()
	if err := os.Chmod(workRoot, 0o750); err != nil {
		t.Fatalf("chmod workRoot: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(workRoot, 0o770) })
	workDir := filepath.Join(workRoot, "agents", "tester")

	m := &Manager{logger: discardLogger()}
	got := m.verifyBobStateDirsWritable("tester", home, workDir, 2004)
	wantDir := filepath.Join(workDir, config.BobStateDirName)
	if len(got) != 1 || got[0] != wantDir {
		t.Errorf("verifyBobStateDirsWritable() = %v, want [%s]", got, wantDir)
	}
}

// TestVerifyBobStateDirsWritableFlagsWorkspaceDir pins that the per-agent
// workspace .bob (the "Failed to initialize logger:" target, seen in
// production at /data/agents/<name>/.bob/.bob-errors/) is probed too, not just
// the shared $HOME/.bob.
func TestVerifyBobStateDirsWritableFlagsWorkspaceDir(t *testing.T) {
	home := t.TempDir()
	homeBob := filepath.Dir(bobSettingsPath(home))
	if err := os.MkdirAll(homeBob, 0o770); err != nil {
		t.Fatalf("mkdir home .bob: %v", err)
	}
	// MkdirAll's mode is masked by umask, so set the production mode
	// explicitly — this dir must be the WRITABLE one for the assertion below
	// to prove the workspace dir is probed independently.
	if err := os.Chmod(homeBob, 0o770); err != nil {
		t.Fatalf("chmod home .bob: %v", err)
	}
	workDir := t.TempDir()
	stateDir := filepath.Join(workDir, config.BobStateDirName)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("mkdir workspace .bob: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o770) })

	m := &Manager{logger: discardLogger()}
	got := m.verifyBobStateDirsWritable("tester", home, workDir, 2004)
	if len(got) != 1 || got[0] != stateDir {
		t.Errorf("verifyBobStateDirsWritable() = %v, want [%s]", got, stateDir)
	}
}

func TestVerifyBobStateDirsWritableIgnoresNonDirectoryStatePath(t *testing.T) {
	home := t.TempDir()
	homeBob := filepath.Dir(bobSettingsPath(home))
	if err := os.MkdirAll(homeBob, 0o770); err != nil {
		t.Fatalf("mkdir home .bob: %v", err)
	}
	if err := os.Chmod(homeBob, 0o770); err != nil {
		t.Fatalf("chmod home .bob: %v", err)
	}
	workDir := t.TempDir()
	statePath := filepath.Join(workDir, config.BobStateDirName)
	if err := os.WriteFile(statePath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write workspace .bob file: %v", err)
	}

	m := &Manager{logger: discardLogger()}
	got := m.verifyBobStateDirsWritable("tester", home, workDir, 2004)
	if len(got) != 0 {
		t.Errorf("verifyBobStateDirsWritable() = %v for non-directory state path, want advisory warning only", got)
	}
}

// TestBobStateHomeIsPerAgentHome pins that the preflight probes the per-agent
// ~/.bob bob actually writes to, not the shared /data/home/.bob.
func TestBobStateHomeIsPerAgentHome(t *testing.T) {
	t.Setenv("HIVE_SHARED_AGENT_HOME", "")
	sharedRoot := t.TempDir()
	orig := sharedAgentHome
	sharedAgentHome = sharedRoot
	t.Cleanup(func() { sharedAgentHome = orig })

	got := bobStateHome("tester", 2004, bobBackend)
	if want := interactiveHomePath("tester"); got != want {
		t.Fatalf("bobStateHome() = %q, want per-agent home %q", got, want)
	}
	if got == bobSharedHome || got == sharedRoot {
		t.Errorf("bobStateHome() = %q, must not be the shared home", got)
	}

	// An unwritable per-agent ~/.bob is reported against the per-agent dir.
	agentBob := filepath.Dir(bobSettingsPath(got))
	if err := os.MkdirAll(agentBob, 0o770); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(agentBob, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(agentBob, 0o770) })
	workDir := t.TempDir()
	if err := os.Chmod(workDir, 0o770); err != nil {
		t.Fatalf("chmod workDir: %v", err)
	}
	m := &Manager{logger: discardLogger()}
	unwritable := m.verifyBobStateDirsWritable("tester", got, workDir, 2004)
	if len(unwritable) == 0 || unwritable[0] != agentBob {
		t.Errorf("verifyBobStateDirsWritable() = %v, want first entry %s", unwritable, agentBob)
	}
}

// TestVerifyBobStateDirsWritableAcceptsAgentOwnedDir pins that a per-agent
// ~/.bob created by bob itself — owned by the agent UID with mkdirSync's
// 0755/0700 and no group write bit — is NOT reported as unwritable. The probe
// must judge an agent-owned dir by its owner bits, otherwise every launch after
// first run logs a false "bob state dir is not writable" error. The same holds
// for a missing dir whose nearest existing parent is agent-owned 0700.
func TestVerifyBobStateDirsWritableAcceptsAgentOwnedDir(t *testing.T) {
	uid := os.Getuid()
	if uid <= 0 {
		t.Skip("probe is a no-op for uid 0; cannot model an agent-owned dir as root")
	}
	home := t.TempDir()
	workDir := t.TempDir()
	// t.TempDir() is 0700 and owned by this process, which is exactly the
	// shape bob's own mkdirSync leaves behind for the agent UID.
	agentBob := filepath.Dir(bobSettingsPath(home))
	if err := os.Mkdir(agentBob, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(agentBob, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	m := &Manager{logger: discardLogger()}
	if got := m.verifyBobStateDirsWritable("tester", home, workDir, uid); len(got) != 0 {
		t.Errorf("verifyBobStateDirsWritable() = %v for agent-owned 0700 dirs, want none", got)
	}

	// An agent-owned dir with the owner write bit stripped IS still flagged:
	// ownership does not short-circuit the check, it only selects which bits
	// are consulted.
	if err := os.Chmod(agentBob, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(agentBob, 0o700) })
	// Both ~/.bob itself and the missing ~/.bob/settings (whose nearest
	// existing parent is that same 0500 dir) are reported; nothing else is.
	got := m.verifyBobStateDirsWritable("tester", home, workDir, uid)
	wantV2 := filepath.Dir(bobV2SettingsPath(home))
	if len(got) != 2 || got[0] != agentBob || got[1] != wantV2 {
		t.Errorf("verifyBobStateDirsWritable() = %v for agent-owned 0500 dir, want [%s %s]", got, agentBob, wantV2)
	}

	// A dir owned by someone OTHER than the agent UID still falls back to the
	// group test, so the dev-owned shared-home behaviour is unchanged.
	if err := os.Chmod(agentBob, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if got := m.verifyBobStateDirsWritable("tester", home, workDir, uid+1); len(got) == 0 || got[0] != agentBob {
		t.Errorf("verifyBobStateDirsWritable() = %v for foreign-owned 0700 dir, want [%s ...]", got, agentBob)
	}
}

// TestVerifyBobStateDirsWritableSkipsHomeProbesWhenHomeMissing pins the
// first-launch shape of a per-UID bob agent: bobPreflightUnlocked runs BEFORE
// launchInTmux, and the per-agent home is only created by setupInteractiveHome
// inside the launch. With the home absent, the ancestor walk would land on the
// home root — deliberately 0755 dev-owned, nothing is written there — and emit
// two bogus "chmod 2775 <root>" errors on every new agent. The $HOME/.bob
// probes must be skipped in that shape; the workspace dir is still probed.
func TestVerifyBobStateDirsWritableSkipsHomeProbesWhenHomeMissing(t *testing.T) {
	root := t.TempDir()
	// The production /data/home/agents mode: traversable, not group-writable.
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatalf("chmod root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o770) })
	home := filepath.Join(root, "tester")

	workDir := t.TempDir()
	if err := os.Chmod(workDir, 0o770); err != nil {
		t.Fatalf("chmod workDir: %v", err)
	}

	m := &Manager{logger: discardLogger()}
	if got := m.verifyBobStateDirsWritable("tester", home, workDir, 2004); len(got) != 0 {
		t.Errorf("verifyBobStateDirsWritable() = %v for not-yet-created home, want none", got)
	}

	// The workspace dir is NOT exempt: a missing home only silences the two
	// $HOME/.bob probes, never the <workdir>/.bob one.
	if err := os.Chmod(workDir, 0o750); err != nil {
		t.Fatalf("chmod workDir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(workDir, 0o770) })
	got := m.verifyBobStateDirsWritable("tester", home, workDir, 2004)
	wantDir := filepath.Join(workDir, config.BobStateDirName)
	if len(got) != 1 || got[0] != wantDir {
		t.Errorf("verifyBobStateDirsWritable() = %v for missing home with unwritable workDir, want [%s]", got, wantDir)
	}
}

// TestBobStateDirRemedy pins that the logged remedy matches the branch that
// failed: an agent-owned dir needs its OWNER bits restored, and must never be
// told to widen group access (which would undo per-agent isolation), while a
// dev-owned dir reached through group `node` keeps the group-mode advice.
func TestBobStateDirRemedy(t *testing.T) {
	uid := os.Getuid()
	if uid <= 0 {
		t.Skip("cannot model an agent-owned dir as root")
	}
	dir := t.TempDir()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	got := bobStateDirRemedy(info, dir, uid, "770")
	if !strings.HasPrefix(got, "chmod u+rwx "+dir) || strings.Contains(got, "node") {
		t.Errorf("agent-owned remedy = %q, want owner-bit chmod with no group widening", got)
	}

	for _, mode := range []string{"770", "2775"} {
		got := bobStateDirRemedy(info, dir, uid+1, mode)
		if want := "chmod " + mode + " " + dir + " and ensure it is group-owned by node"; got != want {
			t.Errorf("foreign-owned remedy(%s) = %q, want %q", mode, got, want)
		}
	}
}
