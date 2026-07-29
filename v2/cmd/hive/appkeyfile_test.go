package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
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
	if err := os.WriteFile(path, pemData, spokeAppKeyFileMode); err != nil {
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
	origData, origSecrets := spokeAppKeyPath, spokeProvisionedAppKeyPath
	spokeAppKeyPath = filepath.Join(dir, "data-gh-app-key.pem")
	spokeProvisionedAppKeyPath = filepath.Join(dir, "secrets-gh-app-key.pem")
	t.Cleanup(func() {
		spokeAppKeyPath, spokeProvisionedAppKeyPath = origData, origSecrets
	})
	return spokeAppKeyPath, spokeProvisionedAppKeyPath
}

// TestResolveAppKeyFilePrefersDeliveredKey is the test that keeps the fix from
// being cosmetic.
//
// The hub can deliver a correct key and the hive can still authenticate with the
// wrong one: the delivered key lands on the PVC at spokeAppKeyPath, but the
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
		got := resolveAppKeyFile("", "")
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
		if reported := reportedAppKeyFingerprint(""); reported != goodFP {
			t.Fatalf("reportedAppKeyFingerprint = %q, want %q", reported, goodFP)
		}

		// And it must stop claiming a per-hive override, so the hub's idempotence
		// check sees a plain match and stops pushing.
		if hasPerHiveAppKey("") {
			t.Error("hasPerHiveAppKey = true after taking delivery; the /secrets key is no longer in effect")
		}
	})

	t.Run("no delivered key falls back to the provisioning mount", func(t *testing.T) {
		dataPath, secretsPath := redirectSpokeKeyPaths(t)
		provisionedFP := writeTestKey(t, secretsPath)
		_ = dataPath // deliberately absent

		got := resolveAppKeyFile("", "")
		if got != secretsPath {
			t.Fatalf("resolveAppKeyFile = %q, want the provisioned key at %q", got, secretsPath)
		}
		if reported := reportedAppKeyFingerprint(""); reported != provisionedFP {
			t.Fatalf("reportedAppKeyFingerprint = %q, want %q", reported, provisionedFP)
		}
		// A genuine per-hive credential must still be reported as such, so the
		// hub's protective precedence keeps working.
		if !hasPerHiveAppKey("") {
			t.Error("hasPerHiveAppKey = false; a provisioned key in effect is a per-hive override")
		}
	})

	t.Run("empty data key does not shadow a good provisioned key", func(t *testing.T) {
		dataPath, secretsPath := redirectSpokeKeyPaths(t)
		provisionedFP := writeTestKey(t, secretsPath)
		// An empty /data file is exactly what vllmd-06..09 had. Mere existence
		// must not win — only a parseable key may.
		if err := os.WriteFile(dataPath, nil, spokeAppKeyFileMode); err != nil {
			t.Fatal(err)
		}

		if got := resolveAppKeyFile("", ""); got != secretsPath {
			t.Fatalf("resolveAppKeyFile = %q, want %q; an empty file is not a key", got, secretsPath)
		}
		if reported := reportedAppKeyFingerprint(""); reported != provisionedFP {
			t.Fatalf("reportedAppKeyFingerprint = %q, want %q", reported, provisionedFP)
		}
	})

	t.Run("explicit key_file and env override still win outright", func(t *testing.T) {
		dataPath, _ := redirectSpokeKeyPaths(t)
		writeTestKey(t, dataPath)
		explicit := filepath.Join(t.TempDir(), "operator-chosen.pem")

		if got := resolveAppKeyFile(explicit, ""); got != explicit {
			t.Errorf("configured key_file = %q, want %q; an explicit path must not be redirected", got, explicit)
		}
		if got := resolveAppKeyFile("", explicit); got != explicit {
			t.Errorf("env override = %q, want %q", got, explicit)
		}
		// Env beats config, matching the original precedence.
		other := filepath.Join(t.TempDir(), "from-config.pem")
		if got := resolveAppKeyFile(other, explicit); got != explicit {
			t.Errorf("env override = %q, want it to beat key_file %q", got, explicit)
		}
		// Whitespace is not a path.
		if got := resolveAppKeyFile("  ", "  "); got != dataPath {
			t.Errorf("blank inputs = %q, want the delivered key at %q", got, dataPath)
		}
	})
}
