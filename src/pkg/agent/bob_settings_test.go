package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
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

	assertAPIKeyAuth(t, authBlock(t, readBobSettings(t, home)))
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
	if len(got) != 1 || got[0] != wantDir {
		t.Errorf("verifyBobStateDirsWritable() = %v, want [%s]", got, wantDir)
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
