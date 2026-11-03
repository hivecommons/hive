package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// Regression coverage for kubestellar/hive#5734.
//
// Antigravity (`agy`) keeps its OAuth session one directory deeper than every
// other CLI's credential: .gemini/antigravity-cli/antigravity-oauth-token
// rather than .gemini/<file>. Two protections were built on the assumption of
// depth 1 and neither reached it:
//
//   - the entrypoint's inotify guard watched /data/home/.gemini/ without -r,
//     and inotifywait reports events only for entries DIRECTLY inside the
//     watched directory, so a write to the token was invisible to it. Measured
//     on a live hive: sixteen minutes and one token rewrite after boot, the
//     .claude guard's inotifywait pid had advanced (its credential sits at
//     depth 1) while .gemini's was still the boot pid.
//   - /data/home/.gemini was absent from WatchedHomeDirs entirely, so there was
//     no Go-side fallback at all.
//
// That left the 5-minute polling sweep as the single mechanism protecting a
// shared credential — and #5730 is the record of that sweep dying silently.
// The entrypoint half is fixed in src/deploy/entrypoint.sh (and asserted by
// src/deploy/test_entrypoint_perm_guard.sh); this file pins the Go half.

// TestWatchedHomeDirsIncludesGemini pins the fallback's existence against the
// production list, the same way TestWatchedHomeDirsIncludesBob does. It reads
// productionWatchedHomeDirs — the snapshot TestMain takes before repointing
// WatchedHomeDirs at the hermetic temp tree — so the pin stays on the real
// default rather than a test override.
func TestWatchedHomeDirsIncludesGemini(t *testing.T) {
	const geminiHome = "/data/home/.gemini"
	for _, dir := range productionWatchedHomeDirs {
		if dir == geminiHome {
			return
		}
	}
	t.Fatalf("WatchedHomeDirs must contain %q so antigravity's shared credential has a "+
		"Go-side fallback; got %v", geminiHome, productionWatchedHomeDirs)
}

// TestPermissionsWatcher_AntigravityNestedCredential is the depth test. A
// fallback that only reached the top of .gemini would repeat the entrypoint's
// mistake in Go: the token the fleet shares lives one level down, so the walk
// has to get there and the credential carve-out has to fire when it does.
func TestPermissionsWatcher_AntigravityNestedCredential(t *testing.T) {
	resetPermWarnDedupe()
	dir := t.TempDir()
	geminiRoot := filepath.Join(dir, ".gemini")
	tokenDir := filepath.Join(geminiRoot, "antigravity-cli")
	token := filepath.Join(tokenDir, "antigravity-oauth-token")

	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 0600 owned by the signing-in agent is what agy leaves behind on refresh.
	if err := os.WriteFile(token, []byte("token-material"), 0o600); err != nil {
		t.Fatal(err)
	}

	origW, origG := WatchedHomeDirs, GooseLogsDir
	WatchedHomeDirs = []string{geminiRoot}
	GooseLogsDir = filepath.Join(dir, "goose", "logs")
	t.Cleanup(func() { WatchedHomeDirs = origW; GooseLogsDir = origG })

	ensureWatchedDirs(discardLogger())
	fixPermissions(discardLogger())

	fi, err := os.Stat(token)
	if err != nil {
		t.Fatalf("stat token: %v", err)
	}
	if fi.Mode().Perm()&0o040 == 0 {
		t.Fatalf("nested antigravity token left at %v — every agent uid but the one that "+
			"signed in is still locked out", fi.Mode().Perm())
	}
	// Read, not write: it is an OAuth token, and agy replaces it by rename.
	if fi.Mode().Perm()&0o020 != 0 {
		t.Errorf("nested token granted group write (%v); read is all the fleet needs",
			fi.Mode().Perm())
	}
	if fi.Mode().Perm()&0o007 != 0 {
		t.Errorf("nested token granted world access (%v)", fi.Mode().Perm())
	}
}

// TestSharedCredentialBasesCoversAntigravityToken keeps the basename the
// watcher recognises tied to the path the entrypoint repairs. If agy moves its
// token again — which is how this bug arrived — the two must not drift apart
// silently.
func TestSharedCredentialBasesCoversAntigravityToken(t *testing.T) {
	const tokenPath = "/data/home/.gemini/antigravity-cli/antigravity-oauth-token"
	if !sharedCredentialBases[filepath.Base(tokenPath)] {
		t.Fatalf("%q is not in sharedCredentialBases, so the Go fallback walks past the "+
			"credential it exists to repair", filepath.Base(tokenPath))
	}
}
