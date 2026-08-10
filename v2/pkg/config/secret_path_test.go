package config

import (
	"os"
	"path/filepath"
	"testing"
)

// N8 (CWE-200/918): api_key_file must be confined to the managed secrets dirs.
//
// The gateway upsert stores whatever absolute path it is handed, and the
// save-time probe then reads that file and ships its contents to the gateway
// endpoint as an `Authorization: Bearer` header. Unconfined, that is an
// arbitrary file read wired to an arbitrary outbound request — the GitHub App
// private key under /data/secrets, /proc/self/environ, App token caches.
//
// The gate lives in ResolveAPIKey rather than only in the HTTP handler, so a
// path that reached hive.yaml some other way (hand-edited config, older build,
// restored backup) is also refused.

func TestSecretFilePathAllowed(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		// The two legitimate roots.
		{"read-only secret mount", "/secrets/litellm_api_key", true},
		{"writable PVC secrets dir", "/data/secrets/gateway_openrouter_api_key", true},
		{"nested under an allowed root", "/data/secrets/sub/key", true},

		// The exfiltration targets the audit reproduced.
		{"github app private key outside the roots", "/data/gh-app-key.pem", false},
		{"proc environ", "/proc/self/environ", false},
		{"app token cache", "/var/run/hive-metrics/gh-app-token.cache", false},
		{"etc shadow", "/etc/shadow", false},
		{"ssh key", "/root/.ssh/id_rsa", false},

		// Traversal and prefix tricks.
		{"traversal out of an allowed root", "/data/secrets/../gh-app-key.pem", false},
		{"deep traversal", "/secrets/../../etc/shadow", false},
		{"sibling dir sharing a name prefix", "/data/secrets-evil/key", false},
		{"prefix of a root without separator", "/data/secretsevil", false},

		// Shape requirements.
		{"relative path", "data/secrets/key", false},
		{"empty", "", false},
		{"whitespace only", "   ", false},
		{"the root directory itself is not a key file", "/secrets", false},
		{"the writable root itself is not a key file", "/data/secrets", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SecretFilePathAllowed(tc.path); got != tc.want {
				t.Errorf("SecretFilePathAllowed(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestSecretFilePathSymlinkEscape covers the bypass a purely lexical check would
// miss: a symlink INSIDE an allowed root pointing out of it. The check resolves
// symlinks before the prefix test, so the link's target is what is judged.
func TestSecretFilePathSymlinkEscape(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "stolen.pem")
	if err := os.WriteFile(target, []byte("-----BEGIN PRIVATE KEY-----"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	// Point the allowed root at a temp dir so the test does not need /data.
	realRoots := secretFileRoots
	root := filepath.Join(tmp, "secrets")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	secretFileRoots = []string{root}
	defer func() { secretFileRoots = realRoots }()

	// A regular file inside the root is fine.
	inside := filepath.Join(root, "ok_key")
	if err := os.WriteFile(inside, []byte("k"), 0o600); err != nil {
		t.Fatalf("seed inside: %v", err)
	}
	if !SecretFilePathAllowed(inside) {
		t.Error("a real file inside the allowed root should be permitted")
	}

	// A symlink inside the root pointing OUT of it must be refused.
	link := filepath.Join(root, "escape")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if SecretFilePathAllowed(link) {
		t.Error("a symlink inside the allowed root that resolves OUTSIDE it must be refused — " +
			"otherwise the confinement is bypassed by planting one link")
	}
}

// TestResolveAPIKeyRefusesUnconfinedFile is the end-to-end assertion that
// matters: the exfiltration primitive is ResolveAPIKey returning file contents
// the caller then sends upstream. An out-of-root path must resolve to "" so the
// probe carries no credential at all.
func TestResolveAPIKeyRefusesUnconfinedFile(t *testing.T) {
	tmp := t.TempDir()
	secret := filepath.Join(tmp, "app-key.pem")
	if err := os.WriteFile(secret, []byte("SUPER-SECRET-KEY"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	gw := GatewayConfig{Name: "evil", Endpoint: "http://attacker.example", APIKeyFile: secret}
	if got := gw.ResolveAPIKey(); got != "" {
		t.Fatalf("ResolveAPIKey read an out-of-root file and returned %q — this is the N8 "+
			"exfiltration primitive: the save-time probe ships it to the attacker's endpoint "+
			"as an Authorization: Bearer header", got)
	}

	// Control: a file inside an allowed root still resolves, or the fix would
	// have broken every legitimate gateway.
	realRoots := secretFileRoots
	root := filepath.Join(tmp, "secrets")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	secretFileRoots = []string{root}
	defer func() { secretFileRoots = realRoots }()

	good := filepath.Join(root, "gateway_key")
	if err := os.WriteFile(good, []byte("  legit-key\n"), 0o600); err != nil {
		t.Fatalf("seed good: %v", err)
	}
	gw2 := GatewayConfig{Name: "ok", APIKeyFile: good}
	if got := gw2.ResolveAPIKey(); got != "legit-key" {
		t.Fatalf("ResolveAPIKey(confined file) = %q, want the trimmed key — the fix must not "+
			"break legitimate gateways", got)
	}
}
