package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/hub"
)

// These tests cover the error and skip branches of the cmd/hive App key-file
// helpers that the happy-path tests in appkeyfile_test.go leave unexercised:
// nextInstallationID's nil-config and adopt branches, heldPerAppIDKeyFingerprints'
// skip rules, writePerAppIDKey's refusal paths, and the "no usable key" answers
// of reportedAppKeyFingerprint and hasPerHiveAppKey.

// TestNextInstallationIDNilConfigKeepsCurrent: a heartbeat with no github_app
// section must never disturb the installation the spoke already has.
func TestNextInstallationIDNilConfigKeepsCurrent(t *testing.T) {
	next, reset := nextInstallationID(145050760, nil)
	if next != 145050760 || reset {
		t.Fatalf("nextInstallationID(current, nil) = (%d, %v), want (145050760, false)", next, reset)
	}
}

// TestNextInstallationIDAdoptsPushedValue: a non-zero delivered installation_id
// is adopted verbatim, and adoption is not a reset.
func TestNextInstallationIDAdoptsPushedValue(t *testing.T) {
	next, reset := nextInstallationID(0, &hub.HeartbeatGitHubAppConfig{InstallationID: 99887766})
	if next != 99887766 || reset {
		t.Fatalf("adopt = (%d, %v), want (99887766, false)", next, reset)
	}

	// Adoption also overwrites a stale non-zero current.
	next, reset = nextInstallationID(145050760, &hub.HeartbeatGitHubAppConfig{InstallationID: 99887766})
	if next != 99887766 || reset {
		t.Fatalf("overwrite = (%d, %v), want (99887766, false)", next, reset)
	}
}

// TestNextInstallationIDResetOfZeroIsNotAReset: reset_installation on a spoke
// that already has no installation must not report a reset happened — that
// distinction is what keeps the log honest about state changes.
func TestNextInstallationIDResetOfZeroIsNotAReset(t *testing.T) {
	next, reset := nextInstallationID(0, &hub.HeartbeatGitHubAppConfig{ResetInstallation: true})
	if next != 0 || reset {
		t.Fatalf("reset-of-zero = (%d, %v), want (0, false)", next, reset)
	}
}

// TestHeldPerAppIDKeyFingerprintsMissingDirIsNil: a spoke without the PVC dir
// (or before any delivery) reports nil, never a crash — the hub reads that as
// "holds nothing" and delivers.
func TestHeldPerAppIDKeyFingerprintsMissingDirIsNil(t *testing.T) {
	origDir := spokeAppKeyDir
	spokeAppKeyDir = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { spokeAppKeyDir = origDir })

	if held := heldPerAppIDKeyFingerprints(); held != nil {
		t.Fatalf("held = %v, want nil for a missing key dir", held)
	}
}

// TestHeldPerAppIDKeyFingerprintsSkipsNonKeyEntries: only files named
// gh-app-key-<positive int>.pem with parseable key material are reported.
// Everything else on the shared /data PVC — subdirs, unrelated files,
// malformed ids, garbage contents — is silently skipped, because the worst
// case of a skip is an idempotent re-delivery, never a crash.
func TestHeldPerAppIDKeyFingerprintsSkipsNonKeyEntries(t *testing.T) {
	redirectSpokeKeyPaths(t)

	// A subdirectory whose name matches the key pattern.
	if err := os.MkdirAll(filepath.Join(spokeAppKeyDir, "gh-app-key-777.pem"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Wrong prefix / suffix.
	for _, name := range []string{"state.json", "gh-app-key-123.txt", "other-key-123.pem"} {
		if err := os.WriteFile(filepath.Join(spokeAppKeyDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Unparseable app ids: non-numeric, negative, zero.
	for _, name := range []string{"gh-app-key-abc.pem", "gh-app-key--5.pem", "gh-app-key-0.pem"} {
		if err := os.WriteFile(filepath.Join(spokeAppKeyDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Well-named file holding garbage instead of a key.
	if err := os.WriteFile(filepath.Join(spokeAppKeyDir, "gh-app-key-424242.pem"), []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	// One genuinely usable key, which must be the only entry reported.
	wantFP := writeTestKey(t, perAppIDKeyPath(3568013))

	held := heldPerAppIDKeyFingerprints()
	if len(held) != 1 {
		t.Fatalf("held = %v, want exactly the one usable key", held)
	}
	if got := held["3568013"]; got != wantFP {
		t.Fatalf("held[3568013] = %q, want %q", got, wantFP)
	}
}

// TestWritePerAppIDKeyRefusesNonPEM: hub-delivered content that is not PEM must
// be rejected before anything touches disk — a spoke must never sign with, or
// even store, material it cannot verify is a key.
func TestWritePerAppIDKeyRefusesNonPEM(t *testing.T) {
	redirectSpokeKeyPaths(t)

	if _, err := writePerAppIDKey(3568013, "  not pem at all  "); err == nil {
		t.Error("non-PEM content accepted; want an error and no file")
	}
	if _, statErr := os.Stat(perAppIDKeyPath(3568013)); !os.IsNotExist(statErr) {
		t.Errorf("a key file exists after a refused write: %v", statErr)
	}
}

// TestWritePerAppIDKeyRefusesUnusablePEM: content that LOOKS like PEM but does
// not parse as a private key is refused for the same reason — the fingerprint
// gate runs before the write.
func TestWritePerAppIDKeyRefusesUnusablePEM(t *testing.T) {
	redirectSpokeKeyPaths(t)

	bogus := "-----BEGIN RSA PRIVATE KEY-----\nbm90IGEga2V5\n-----END RSA PRIVATE KEY-----\n"
	if _, err := writePerAppIDKey(3568013, bogus); err == nil {
		t.Error("unusable PEM accepted; want an error and no file")
	}
	if _, statErr := os.Stat(perAppIDKeyPath(3568013)); !os.IsNotExist(statErr) {
		t.Errorf("a key file exists after a refused write: %v", statErr)
	}
}

// TestWritePerAppIDKeyReportsDirCreationFailure: when the key dir cannot be
// created (here: its path is occupied by a regular file) the error surfaces to
// the caller instead of being swallowed, so the hub learns the delivery failed.
func TestWritePerAppIDKeyReportsDirCreationFailure(t *testing.T) {
	dir := t.TempDir()
	origDir := spokeAppKeyDir
	spokeAppKeyDir = filepath.Join(dir, "blocked")
	t.Cleanup(func() { spokeAppKeyDir = origDir })
	if err := os.WriteFile(spokeAppKeyDir, []byte("a file where the dir should be"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := writePerAppIDKey(3568013, testRSAKeyPEM); err == nil {
		t.Error("writePerAppIDKey succeeded with an uncreatable key dir; want an error")
	}
}

// TestReportedAppKeyFingerprintNoUsableKey: with no key anywhere — and with the
// zero app_id producing empty per-app-id candidates that must be skipped, not
// fingerprinted — the heartbeat reports "", the hub's signal to deliver.
func TestReportedAppKeyFingerprintNoUsableKey(t *testing.T) {
	redirectSpokeKeyPaths(t)

	if fp := reportedAppKeyFingerprint("", 0); fp != "" {
		t.Fatalf("reportedAppKeyFingerprint = %q, want \"\" when no key exists", fp)
	}

	// An unusable file on every candidate path still reports "" — no file, empty
	// file, and garbage all mean the same thing to the hub.
	if err := os.WriteFile(spokeAppKeyPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fp := reportedAppKeyFingerprint(spokeAppKeyPath, 0); fp != "" {
		t.Fatalf("reportedAppKeyFingerprint = %q, want \"\" for an unusable key", fp)
	}
}

// TestHasPerHiveAppKeyFalseWithoutProvisionedKey: the per-hive override claim
// requires real PEM content at the provisioned mount; a missing or unusable
// file means no claim.
func TestHasPerHiveAppKeyFalseWithoutProvisionedKey(t *testing.T) {
	redirectSpokeKeyPaths(t)

	if hasPerHiveAppKey("", 0) {
		t.Error("hasPerHiveAppKey = true with no provisioned key at all")
	}

	if err := os.WriteFile(spokeProvisionedAppKeyPath, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hasPerHiveAppKey("", 0) {
		t.Error("hasPerHiveAppKey = true with unusable provisioned content")
	}
}
