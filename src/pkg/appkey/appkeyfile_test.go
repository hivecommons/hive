package appkey

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// testRSAKeyBits matches the smallest size GitHub Apps actually use. These keys
// never sign anything real — they only exercise path resolution.
const testRSAKeyBits = 2048

func writeTestKey(t *testing.T, path string) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, testRSAKeyBits)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemData := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
	if err := os.WriteFile(path, pemData, testKeys.FileMode); err != nil {
		t.Fatalf("write key %s: %v", path, err)
	}
	fp, err := config.AppKeyFingerprint(string(pemData))
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	return fp
}

// redirectSpokeKeyPaths points the two spoke key locations at a temp dir and
// restores them afterwards, so the real resolution order runs against files this
// test controls rather than against /data and /secrets.
func redirectSpokeKeyPaths(t *testing.T) (dataPath, secretsPath string) {
	t.Helper()
	dir := t.TempDir()
	origData, origSecrets, origDir := testKeys.DataKeyPath, testKeys.ProvisionedKeyPath, testKeys.DataDir
	origProvDir := testKeys.ProvisionedDir
	testKeys.DataKeyPath = filepath.Join(dir, "data-gh-app-key.pem")
	testKeys.ProvisionedKeyPath = filepath.Join(dir, "secrets-gh-app-key.pem")
	// Per-app-id keys land in this same temp dir, so perAppIDKeyPath and
	// heldPerAppIDKeyFingerprints never touch the real /data.
	testKeys.DataDir = dir
	// The provisioning mount gets a SEPARATE dir: the PVC and the read-only
	// Secret hold per-app-id files under identical names, so pointing both at one
	// dir would make it impossible to tell which source a resolution came from.
	testKeys.ProvisionedDir = filepath.Join(dir, "secrets")
	if err := os.MkdirAll(testKeys.ProvisionedDir, 0o700); err != nil {
		t.Fatalf("mkdir provisioned key dir: %v", err)
	}
	t.Cleanup(func() {
		testKeys.DataKeyPath, testKeys.ProvisionedKeyPath, testKeys.DataDir = origData, origSecrets, origDir
		testKeys.ProvisionedDir = origProvDir
	})
	return testKeys.DataKeyPath, testKeys.ProvisionedKeyPath
}

// TestResolveAppKeyFilePrefersDeliveredKey is the test that keeps the fix from
// being cosmetic.
//
// The hub can deliver a correct key and the hive can still authenticate with the
// wrong one: the delivered key lands on the PVC at testKeys.DataKeyPath, but the
// stale operator-provisioned key sits at the read-only /secrets mount that the
// spoke cannot overwrite. If resolution fell through to /secrets whenever
// key_file was unset — which is exactly the state of the three broken GHE hives
// — every restart would silently resume signing with the dead key while the hub
// kept redelivering forever. Delivered-but-unused looks fixed and is not.
func TestResolveAppKeyFilePrefersDeliveredKey(t *testing.T) {
	dataPath, secretsPath := redirectSpokeKeyPaths(t)

	t.Run("stale secrets key present, delivered data key wins", func(t *testing.T) {
		staleFP := writeTestKey(t, secretsPath)
		goodFP := writeTestKey(t, dataPath)
		if staleFP == goodFP {
			t.Fatal("test keys collided; the two paths must hold different keys")
		}

		// key_file unset — the vllmd-01..03 state.
		got := testKeys.Resolve("", "", 0)
		if got != dataPath {
			t.Fatalf("resolveAppKeyFile = %q, want the delivered key at %q", got, dataPath)
		}

		// And the key actually in effect must be the delivered one.
		fp, err := config.AppKeyFingerprintFromFile(got)
		if err != nil {
			t.Fatalf("fingerprint resolved key: %v", err)
		}
		if fp != goodFP {
			t.Fatalf("resolved key fingerprint = %q, want the delivered %q", fp, goodFP)
		}

		// The heartbeat must report that same key, or the hub compares against a
		// key that is not signing anything.
		if reported := testKeys.ReportedFingerprint("", 0); reported != goodFP {
			t.Fatalf("reportedAppKeyFingerprint = %q, want %q", reported, goodFP)
		}

		// And it must stop claiming a per-hive override, so the hub's idempotence
		// check sees a plain match and stops pushing.
		if testKeys.HasPerHiveKey("", 0) {
			t.Error("hasPerHiveAppKey = true after taking delivery; the /secrets key is no longer in effect")
		}
	})

	t.Run("no delivered key falls back to the provisioning mount", func(t *testing.T) {
		dataPath, secretsPath := redirectSpokeKeyPaths(t)
		provisionedFP := writeTestKey(t, secretsPath)
		_ = dataPath // deliberately absent

		got := testKeys.Resolve("", "", 0)
		if got != secretsPath {
			t.Fatalf("resolveAppKeyFile = %q, want the provisioned key at %q", got, secretsPath)
		}
		if reported := testKeys.ReportedFingerprint("", 0); reported != provisionedFP {
			t.Fatalf("reportedAppKeyFingerprint = %q, want %q", reported, provisionedFP)
		}
		// A genuine per-hive credential must still be reported as such, so the
		// hub's protective precedence keeps working.
		if !testKeys.HasPerHiveKey("", 0) {
			t.Error("hasPerHiveAppKey = false; a provisioned key in effect is a per-hive override")
		}
	})

	t.Run("empty data key does not shadow a good provisioned key", func(t *testing.T) {
		dataPath, secretsPath := redirectSpokeKeyPaths(t)
		provisionedFP := writeTestKey(t, secretsPath)
		// An empty /data file is exactly what vllmd-06..09 had. Mere existence
		// must not win — only a parseable key may.
		if err := os.WriteFile(dataPath, nil, testKeys.FileMode); err != nil {
			t.Fatal(err)
		}

		if got := testKeys.Resolve("", "", 0); got != secretsPath {
			t.Fatalf("resolveAppKeyFile = %q, want %q; an empty file is not a key", got, secretsPath)
		}
		if reported := testKeys.ReportedFingerprint("", 0); reported != provisionedFP {
			t.Fatalf("reportedAppKeyFingerprint = %q, want %q", reported, provisionedFP)
		}
	})

	t.Run("explicit key_file and env override still win outright", func(t *testing.T) {
		dataPath, _ := redirectSpokeKeyPaths(t)
		writeTestKey(t, dataPath)
		explicit := filepath.Join(t.TempDir(), "operator-chosen.pem")

		if got := testKeys.Resolve(explicit, "", 0); got != explicit {
			t.Errorf("configured key_file = %q, want %q; an explicit path must not be redirected", got, explicit)
		}
		if got := testKeys.Resolve("", explicit, 0); got != explicit {
			t.Errorf("env override = %q, want %q", got, explicit)
		}
		// Env beats config, matching the original precedence.
		other := filepath.Join(t.TempDir(), "from-config.pem")
		if got := testKeys.Resolve(other, explicit, 0); got != explicit {
			t.Errorf("env override = %q, want it to beat key_file %q", got, explicit)
		}
		// Whitespace is not a path.
		if got := testKeys.Resolve("  ", "  ", 0); got != dataPath {
			t.Errorf("blank inputs = %q, want the delivered key at %q", got, dataPath)
		}
	})
}

// TestResolveAppKeyFilePrefersPerAppIDKey is the both-keys fix's central
// guarantee on the spoke: a hive configured for a specific app_id signs with the
// per-app-id key delivered for THAT app, even when a different (cluster-default)
// key sits at the generic /data path.
//
// This is exactly the github.com-hive-on-a-GHE-cluster case: /data/gh-app-key.pem
// holds the cluster's GHE key, but the hive claims the github.com app_id, and
// /data/gh-app-key-<githubAppID>.pem holds the github.com key. Resolution must
// pick the per-app-id file.
func TestResolveAppKeyFilePrefersPerAppIDKey(t *testing.T) {
	const githubAppID int64 = 3568013

	t.Run("per-app-id key wins over the generic data key", func(t *testing.T) {
		redirectSpokeKeyPaths(t)
		clusterKeyFP := writeTestKey(t, testKeys.DataKeyPath) // GHE cluster default at /data
		perAppPath := testKeys.PerAppIDKeyPath(githubAppID)
		githubKeyFP := writeTestKey(t, perAppPath) // github.com key, per-app-id file
		if clusterKeyFP == githubKeyFP {
			t.Fatal("test keys collided; the two paths must hold different keys")
		}

		got := testKeys.Resolve("", "", githubAppID)
		if got != perAppPath {
			t.Fatalf("resolveAppKeyFile = %q, want the per-app-id key at %q", got, perAppPath)
		}
		// The key actually in effect — and the fingerprint the heartbeat reports —
		// must be the github.com one, not the cluster default.
		if reported := testKeys.ReportedFingerprint("", githubAppID); reported != githubKeyFP {
			t.Fatalf("reportedAppKeyFingerprint = %q, want the github.com key %q", reported, githubKeyFP)
		}
	})

	t.Run("a different app_id still gets the generic data key", func(t *testing.T) {
		redirectSpokeKeyPaths(t)
		clusterKeyFP := writeTestKey(t, testKeys.DataKeyPath)
		// A per-app-id key exists for github.com, but this hive claims the GHE app.
		writeTestKey(t, testKeys.PerAppIDKeyPath(githubAppID))

		const gheAppID int64 = 5686
		got := testKeys.Resolve("", "", gheAppID)
		if got != testKeys.DataKeyPath {
			t.Fatalf("resolveAppKeyFile = %q, want the generic data key at %q (no per-app file for this app_id)", got, testKeys.DataKeyPath)
		}
		if reported := testKeys.ReportedFingerprint("", gheAppID); reported != clusterKeyFP {
			t.Fatalf("reportedAppKeyFingerprint = %q, want the cluster key %q", reported, clusterKeyFP)
		}
	})

	t.Run("empty per-app-id file does not shadow the generic key", func(t *testing.T) {
		redirectSpokeKeyPaths(t)
		clusterKeyFP := writeTestKey(t, testKeys.DataKeyPath)
		// A truncated per-app file must never win — only a parseable key may.
		if err := os.WriteFile(testKeys.PerAppIDKeyPath(githubAppID), nil, testKeys.FileMode); err != nil {
			t.Fatal(err)
		}
		if got := testKeys.Resolve("", "", githubAppID); got != testKeys.DataKeyPath {
			t.Fatalf("resolveAppKeyFile = %q, want the generic key at %q; an empty per-app file is not a key", got, testKeys.DataKeyPath)
		}
		if reported := testKeys.ReportedFingerprint("", githubAppID); reported != clusterKeyFP {
			t.Fatalf("reportedAppKeyFingerprint = %q, want %q", reported, clusterKeyFP)
		}
	})

	t.Run("explicit key_file still beats a per-app-id key", func(t *testing.T) {
		redirectSpokeKeyPaths(t)
		writeTestKey(t, testKeys.PerAppIDKeyPath(githubAppID))
		explicit := filepath.Join(t.TempDir(), "operator-chosen.pem")
		if got := testKeys.Resolve(explicit, "", githubAppID); got != explicit {
			t.Errorf("configured key_file = %q, want %q; an explicit path must not be redirected", got, explicit)
		}
	})
}

// TestWritePerAppIDKeyAndReport exercises the spoke's storage of a hub-delivered
// additional key and the held-fingerprints report the hub uses for idempotence.
func TestWritePerAppIDKeyAndReport(t *testing.T) {
	redirectSpokeKeyPaths(t)
	const appID int64 = 3568013

	// Generate a key PEM the way the hub would deliver it.
	k, err := rsa.GenerateKey(rand.Reader, testRSAKeyBits)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemData := string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	}))
	wantFP, err := config.AppKeyFingerprint(pemData)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	fp, err := testKeys.WritePerAppIDKey(appID, pemData)
	if err != nil {
		t.Fatalf("writePerAppIDKey: %v", err)
	}
	if fp != wantFP {
		t.Fatalf("writePerAppIDKey fingerprint = %q, want %q", fp, wantFP)
	}

	// The file must be 0600 — signing material is never group/other readable.
	info, err := os.Stat(testKeys.PerAppIDKeyPath(appID))
	if err != nil {
		t.Fatalf("stat per-app key: %v", err)
	}
	if info.Mode().Perm() != testKeys.FileMode {
		t.Fatalf("per-app key mode = %o, want %o", info.Mode().Perm(), testKeys.FileMode)
	}

	// heldPerAppIDKeyFingerprints must surface exactly this app_id → fingerprint,
	// which is what the hub diffs against to stop re-delivering.
	held := testKeys.HeldFingerprints()
	if got := held["3568013"]; got != wantFP {
		t.Fatalf("heldPerAppIDKeyFingerprints[3568013] = %q, want %q (full map: %v)", got, wantFP, held)
	}

	// A non-positive app_id is refused rather than writing an ambiguous file.
	if _, err := testKeys.WritePerAppIDKey(0, pemData); err == nil {
		t.Error("testKeys.WritePerAppIDKey(0, ...) succeeded; a non-positive app_id must be refused")
	}
}

// TestResolveAppKeyFileUsesProvisionedPerAppIDKey covers the birth of a hive:
// provisioning has written the fleet's keys into the read-only Secret, and no
// heartbeat has run yet, so the PVC holds nothing. The spoke must still sign as
// the App it is configured as rather than falling through to the generic
// single-file paths — which on a GHE cluster hold the OTHER forge's key and
// produce "404 Integration not found".
func TestResolveAppKeyFileUsesProvisionedPerAppIDKey(t *testing.T) {
	redirectSpokeKeyPaths(t)
	const publicAppID = 3568013

	provisioned := testKeys.provisionedPerAppIDKeyPath(publicAppID)
	writeTestKey(t, provisioned)

	if got := testKeys.Resolve("", "", publicAppID); got != provisioned {
		t.Fatalf("want provisioned per-app-id key %s, got %s", provisioned, got)
	}
}

// A heartbeat-delivered key on the PVC must outrank the copy frozen into the
// Secret at provision time: that is what makes key ROTATION possible, since the
// Secret cannot be rewritten in place by the spoke.
func TestResolveAppKeyFilePrefersPVCOverProvisionedPerAppID(t *testing.T) {
	redirectSpokeKeyPaths(t)
	const publicAppID = 3568013

	writeTestKey(t, testKeys.provisionedPerAppIDKeyPath(publicAppID))
	pvc := testKeys.PerAppIDKeyPath(publicAppID)
	writeTestKey(t, pvc)

	if got := testKeys.Resolve("", "", publicAppID); got != pvc {
		t.Fatalf("rotated PVC key must win, want %s, got %s", pvc, got)
	}
}

// An explicit operator override still beats both per-app-id sources.
func TestResolveAppKeyFileExplicitOverrideBeatsProvisionedPerAppID(t *testing.T) {
	redirectSpokeKeyPaths(t)
	const publicAppID = 3568013

	writeTestKey(t, testKeys.provisionedPerAppIDKeyPath(publicAppID))
	named := filepath.Join(t.TempDir(), "operator-named.pem")
	writeTestKey(t, named)

	if got := testKeys.Resolve(named, "", publicAppID); got != named {
		t.Fatalf("explicit key_file must win, want %s, got %s", named, got)
	}
}

// A truncated per-app-id file in the Secret must not shadow a usable generic
// key: existence is not enough, it has to be a parseable key.
func TestResolveAppKeyFileIgnoresUnusableProvisionedPerAppIDKey(t *testing.T) {
	dataPath, _ := redirectSpokeKeyPaths(t)
	const publicAppID = 3568013

	if err := os.WriteFile(testKeys.provisionedPerAppIDKeyPath(publicAppID),
		[]byte("PLACEHOLDER-awaiting-github-app-key"), 0o600); err != nil {
		t.Fatalf("write placeholder: %v", err)
	}
	writeTestKey(t, dataPath)

	if got := testKeys.Resolve("", "", publicAppID); got != dataPath {
		t.Fatalf("unusable per-app-id key must be skipped, want %s, got %s", dataPath, got)
	}
}
