package main

import (
	"os"
	"path/filepath"
	"testing"
)

// testRSAKeyPEM is a throwaway key generated for these tests only. It never
// authenticates anything — these tests assert WHICH path is selected, and the
// key only has to be parseable for AppKeyFingerprintFromFile to succeed.
const testRSAKeyPEM = `-----BEGIN PRIVATE KEY-----
MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDOpynovzSc6LQ9
8/nCv8aPUkgC0/y3wm66LtpqcP5FDQXvcO0lJ64GDZPsNCQa4pOp3D6KBCsVOS6A
PW30xvs4EZqhsXOxnEVzNIvouSqvgQyw6CTw/AUvgHNQBetWxnVjgjlgcXtcagZS
34eTghJB9PowZbH2xl+zpsCAVtnfyqq52we9aeT0KUM8Secd+PXcuiCy2g90gRWm
a1Y56Fmhvr5Jk2CGSAZ68FPBFU4lqqJ0Hf5i6ejTCmaD8XGLGOMvSFz1z86k7Wpe
JuBTMJK9y19LMRLWaVjfiMR8xLH5x8OefwmIeXY72Uf9ndjVyT/zPn9Fo03dOjWs
U56/WjxDAgMBAAECggEABr3ZVih2tO+6gZLmAP50odRTWRRFWFFVf2lr4rEQ+nu0
R91tPxsOSFBFFR2WV/IwUwhGWgZMyYJ2C+T1I1kidO/OFZxOY+rvMRTzw4HW7KbP
HS5Vli8ClEwidufah5gt2DM1X/oTxi4HSsjUCXHi2pf9WXrX1W8fTCMSgJ1UukI5
UJ6ghMvZ9TVpXcIod2nEMWxn5viO/lGYXmPhLU13ckrO15TuqdDp3LyZQbOKLdWl
NML9jIrlRo1+huiAINgfEzuNkbwwT4IhrMn0fLRzf5VRQw2qvlMR3mrov9as0JBT
LRLnvvBO452VYCG7m6BwGko1MROlHjt8VPXt0nzi8QKBgQDrtUnmMQkLnWqigaMK
BtSFF3BYRLNJLnR488N2hu7oikX/5DQ2ifVaPVSjXiFWjIm/uTq2Sni7pkruolAl
LHzW+OqL5hLVa+Ib29p5t3Bx/eyPSYMxjJOJm1Cnpzd4AN+o1JNDa4jNAvcO2/Yo
730g7g9gwWBfORl+orKUA0DIhwKBgQDgcYkXjzUsjLPIV5DODgOt7x/0kYnsAX0V
ZUZ1eDnFaooW9JsA8hN9WF/SHeN1KJh/xxPbqzAPi7pPXJkIMoQcGFtTa/cpJ4Js
kqCLK1LnCBgvg10zbgS5S8oNqFwOPUKoZCJGuvzlGbH4RHxfgyYTrX+lMBrqla/a
qoabFsapZQKBgAaGMBN1DAEMTGVPHUorwjok2fE3hZbi+EpYxPJE7dv159YbZO6V
hvsGc49KDbYtkaqC4AMnsIvRIIXWbE17G8F/hk51AdRydgG7ZiK0VyJwmtmkeUMn
1vWaHPNnB3wE2iv8Jk9ZbKHwERKSOBAOAPKmZDqTX62DEReWPUcnh+WFAoGAaSpJ
xlQ/4iP7iYAeRa6jYriNDJe1PHRmG8Rcg2ZWC36kPaVXi9Xh8/WY0GdY0Oi4rAan
82H/HwmlvtHwkrq41EFFaY1JPmtY3W7G8u7V5ZMRYhH3dcWzSO+OOWAN4k4qEaT5
upKbNO4ZSe8tJ8PX75h4GvqzYf/JanhEoh7F71ECgYEA3RhPTmznvCXfXYTOFK/G
2Ab+QNjA8tkVxn1OpOrmSPqtL0bs+Hjl8nvhRAhUJ1P3eoLC0BglOrArv01gTS8i
DEbf6ZaMYueH1DDUjFLLI+SuvqAiv34uaYkAGMX9wiPyR6YCKB5zTMU3l4qJllja
/R753+9uhRTs+hupixvQlEA=
-----END PRIVATE KEY-----
`

// writeKey drops a syntactically valid RSA key at path so
// AppKeyFingerprintFromFile can parse it. Content is irrelevant to these tests
// beyond being parseable; only WHICH path is selected is under test.
func writeKey(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(testRSAKeyPEM), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestResolveAppKeyFileIgnoresStaleGenericPin pins the 2026-07-31 production
// state: every vllm-d spoke carried key_file: /data/gh-app-key.pem holding the
// GHE key, so correcting app_id to the public App still authenticated with the
// wrong key and returned 404 Integration not found.
//
// The generic path names no App, so it cannot be trusted once the app_id we
// claim has a per-app-id key of its own.
func TestResolveAppKeyFileIgnoresStaleGenericPin(t *testing.T) {
	dir := t.TempDir()
	origDir, origGeneric := appKeys.DataDir, appKeys.DataKeyPath
	appKeys.DataDir = dir
	appKeys.DataKeyPath = filepath.Join(dir, "gh-app-key.pem")
	t.Cleanup(func() { appKeys.DataDir, appKeys.DataKeyPath = origDir, origGeneric })

	const publicAppID = 3568013
	writeKey(t, appKeys.DataKeyPath)                  // the stale GHE key
	writeKey(t, appKeys.PerAppIDKeyPath(publicAppID)) // the correct public key

	got := appKeys.Resolve(appKeys.DataKeyPath, "", publicAppID)
	if want := appKeys.PerAppIDKeyPath(publicAppID); got != want {
		t.Fatalf("resolved %q, want %q — a stale generic pin must not shadow the key for the App we claim", got, want)
	}
}

// TestResolveAppKeyFileKeepsOperatorOverride guards the escape hatch. A hive on
// an App this build does not know (daviddiaz0317-visual-hive runs app_id
// 4240368, neither the public nor the GHE App) may legitimately point key_file
// at a bespoke location, and that must still win.
func TestResolveAppKeyFileKeepsOperatorOverride(t *testing.T) {
	dir := t.TempDir()
	origDir, origGeneric := appKeys.DataDir, appKeys.DataKeyPath
	appKeys.DataDir = dir
	appKeys.DataKeyPath = filepath.Join(dir, "gh-app-key.pem")
	t.Cleanup(func() { appKeys.DataDir, appKeys.DataKeyPath = origDir, origGeneric })

	const oddAppID = 4240368
	bespoke := filepath.Join(dir, "custom", "my-app.pem")
	writeKey(t, bespoke)
	writeKey(t, appKeys.PerAppIDKeyPath(oddAppID)) // present, and must NOT be preferred

	if got := appKeys.Resolve(bespoke, "", oddAppID); got != bespoke {
		t.Fatalf("resolved %q, want the operator's %q", got, bespoke)
	}
}

// TestResolveAppKeyFileGenericPinSurvivesWithoutPerAppKey: the override is
// narrow. With no per-app-id key on disk, the generic pin is all there is and
// must be returned rather than dropping the hive to no key at all.
func TestResolveAppKeyFileGenericPinSurvivesWithoutPerAppKey(t *testing.T) {
	dir := t.TempDir()
	origDir, origGeneric := appKeys.DataDir, appKeys.DataKeyPath
	appKeys.DataDir = dir
	appKeys.DataKeyPath = filepath.Join(dir, "gh-app-key.pem")
	t.Cleanup(func() { appKeys.DataDir, appKeys.DataKeyPath = origDir, origGeneric })

	writeKey(t, appKeys.DataKeyPath)
	if got := appKeys.Resolve(appKeys.DataKeyPath, "", 3568013); got != appKeys.DataKeyPath {
		t.Fatalf("resolved %q, want %q — with no per-app key the generic one is still the only key", got, appKeys.DataKeyPath)
	}
}

// TestEnvOverrideStillWins: GH_APP_KEY_FILE outranks everything, unchanged.
func TestEnvOverrideStillWins(t *testing.T) {
	dir := t.TempDir()
	origDir, origGeneric := appKeys.DataDir, appKeys.DataKeyPath
	appKeys.DataDir = dir
	appKeys.DataKeyPath = filepath.Join(dir, "gh-app-key.pem")
	t.Cleanup(func() { appKeys.DataDir, appKeys.DataKeyPath = origDir, origGeneric })

	writeKey(t, appKeys.DataKeyPath)
	writeKey(t, appKeys.PerAppIDKeyPath(3568013))
	env := filepath.Join(dir, "env.pem")
	writeKey(t, env)
	if got := appKeys.Resolve(appKeys.DataKeyPath, env, 3568013); got != env {
		t.Fatalf("resolved %q, want env override %q", got, env)
	}
}

// TestDeliveredKeyPathNamesTheApp pins that a hub-delivered key is stored under
// a filename identifying the App it belongs to.
//
// Writing every delivery to the generic /data/gh-app-key.pem is what made the
// 2026-07-31 outage unrecoverable in place: the file held the GHE key, nothing
// on disk said so, and correcting app_id to the public App kept signing with it.
func TestDeliveredKeyPathNamesTheApp(t *testing.T) {
	dir := t.TempDir()
	origDir, origGeneric := appKeys.DataDir, appKeys.DataKeyPath
	appKeys.DataDir = dir
	appKeys.DataKeyPath = filepath.Join(dir, "gh-app-key.pem")
	t.Cleanup(func() { appKeys.DataDir, appKeys.DataKeyPath = origDir, origGeneric })

	const publicAppID, gheAppID = 3568013, 5686

	pub := deliveredKeyPath(publicAppID)
	ghe := deliveredKeyPath(gheAppID)

	if pub == appKeys.DataKeyPath {
		t.Fatalf("delivered key for app %d landed on the generic path %q — the filename must name the App", publicAppID, pub)
	}
	if pub == ghe {
		t.Fatalf("two different Apps share one key path %q; a key for one App would overwrite the other's", pub)
	}
	if want := appKeys.PerAppIDKeyPath(publicAppID); pub != want {
		t.Fatalf("delivered path = %q, want %q", pub, want)
	}

	// A delivery naming no App must still land somewhere rather than be lost.
	if got := deliveredKeyPath(0); got != appKeys.DataKeyPath {
		t.Fatalf("App-less delivery = %q, want the generic %q", got, appKeys.DataKeyPath)
	}
}
