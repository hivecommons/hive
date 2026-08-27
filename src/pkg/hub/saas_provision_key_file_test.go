package hub

import (
	"bytes"
	"strings"
	"testing"
	"text/template"
)

// #4368: the provisioning seed pinned github.key_file at /secrets/gh-app-key.pem
// for every App-using hive, while the hive-secrets Secret created that entry only
// for hives provisioned with an inline private key. The four hives in the
// 2026-08-12 batch got a config naming a file that would never exist — and
// because an explicit key_file short-circuits resolveAppKeyFile, the key that
// DID arrive at /data/gh-app-key-<app_id>.pem was never tried. github_auth: fail
// with the correct key already on the PVC.
//
// These tests render the REAL template, so they fail on the template text rather
// than on a copy of it.

// renderProvisionManifest executes k8sManifestTemplate with the App-related
// fields under test and inert values for everything else.
func renderProvisionManifest(t *testing.T, useApp, useAppFull bool) string {
	t.Helper()
	tmpl, err := template.New("manifests").Parse(k8sManifestTemplate)
	if err != nil {
		t.Fatalf("template does not parse: %v", err)
	}
	data := map[string]any{
		"ID":                "test-hive",
		"Namespace":         "hive-hosted-test-hive",
		"ImageTag":          "v4-latest",
		"ImagePullPolicy":   "Always",
		"RequiresSCC":       false,
		"InitContainerUID":  "1001",
		"InitContainerGID":  "1000",
		"TerminalPort":      7681,
		"DashboardPort":     3002,
		"CPURequest":        "500m",
		"MemRequest":        "1Gi",
		"CPULimit":          "2",
		"MemLimit":          "4Gi",
		"StorageSize":       "10Gi",
		"StorageClass":      "standard",
		"HeartbeatKey":      "hb",
		"SessionKey":        "sk",
		"SSOPublicKey":      "pk",
		"TerminalKey":       "tk",
		"DashboardToken":    "dt",
		"AuthorizedUsers":   "alice:owner",
		"IsNginxIngress":    false,
		"Host":              "test-hive.hive.kubestellar.io",
		"Owner":             "alice",
		"Org":               "kubestellar",
		"Repos":             "[]",
		"PrimaryRepo":       "kubestellar/hive",
		"HubURL":            "https://hive.kubestellar.io",
		"DashboardURL":      "https://test-hive.hive.kubestellar.io",
		"HiveType":          "hosted",
		"IsPublic":          false,
		"ACMMLevel":         3,
		"UseApp":            useApp,
		"UseAppFull":        useAppFull,
		"AppID":             3568013,
		"InstallationID":    "12345",
		"AppPrivateKey":     "    -----BEGIN RSA PRIVATE KEY-----\n    dGVzdA==\n    -----END RSA PRIVATE KEY-----",
		"AdditionalAppKeys": []provisionAppKey{},
		"Token":             "ghp_test",
		"HasPlaceholderIDs": false,
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		t.Fatalf("template does not execute: %v", err)
	}
	return buf.String()
}

// TestProvisionSeedDoesNotPinKeyFile is the direct regression guard. key_file is
// derivable from app_id, and resolveAppKeyFile's last fallback is the very path
// the seed used to name — so a hive provisioned WITH an inline key resolves to
// the same file it used to be pinned to, and one provisioned without a key can
// now reach the key that is actually delivered.
func TestProvisionSeedDoesNotPinKeyFile(t *testing.T) {
	for _, tc := range []struct {
		name               string
		useApp, useAppFull bool
	}{
		{"app-with-inline-key", true, true},
		{"app-without-inline-key", true, false},
		{"token-auth", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manifest := renderProvisionManifest(t, tc.useApp, tc.useAppFull)
			if pins := manifestKeyFilePins(manifest); len(pins) != 0 {
				t.Errorf("provisioning seed pins github.key_file=%v; it is derivable from app_id "+
					"and an explicit value short-circuits resolveAppKeyFile (#4368)", pins)
			}
		})
	}
}

// TestProvisionManifestKeyFileAssertion is the invariant rather than the current
// value: a future pin is allowed, but only if the manifest also creates the file
// it names. This is the check that would have stopped the 2026-08-12 batch.
func TestProvisionManifestKeyFileAssertion(t *testing.T) {
	for _, tc := range []struct {
		name               string
		useApp, useAppFull bool
	}{
		{"app-with-inline-key", true, true},
		{"app-without-inline-key", true, false},
		{"token-auth", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := assertSpokeManifestKeyFile(renderProvisionManifest(t, tc.useApp, tc.useAppFull)); err != nil {
				t.Errorf("rendered manifest rejected: %v", err)
			}
		})
	}
}

// TestProvisionManifestKeyFileAssertionCatchesTheBug reproduces the exact defect
// by putting the old line back into a rendered manifest, and proves the guard
// fails it. Without this the assertion could be vacuous and nobody would know.
func TestProvisionManifestKeyFileAssertionCatchesTheBug(t *testing.T) {
	// UseApp without UseAppFull: app_id present, no inline key, so the Secret
	// carries no gh-app-key.pem entry. This is the 2026-08-12 shape.
	manifest := renderProvisionManifest(t, true, false)
	if strings.Contains(manifest, "gh-app-key.pem:") {
		t.Fatal("fixture is wrong: a hive provisioned without an inline key must not get a gh-app-key.pem Secret entry")
	}
	broken := strings.Replace(manifest,
		"      installation_id: 12345",
		"      installation_id: 12345\n      key_file: /secrets/gh-app-key.pem", 1)
	if broken == manifest {
		t.Fatal("could not inject the historical key_file pin — the seed's shape changed")
	}

	err := assertSpokeManifestKeyFile(broken)
	if err == nil {
		t.Fatal("assertSpokeManifestKeyFile accepted a seed pinning /secrets/gh-app-key.pem " +
			"when the manifest creates no such Secret entry — this is exactly #4368")
	}
	if !strings.Contains(err.Error(), "gh-app-key.pem") {
		t.Errorf("error does not name the missing entry: %v", err)
	}

	// The same pin is legitimate when the manifest DOES create the file, which
	// keeps the guard about the coupling rather than about the literal path.
	full := renderProvisionManifest(t, true, true)
	if !strings.Contains(full, "gh-app-key.pem: |") {
		t.Fatal("fixture is wrong: a hive provisioned WITH an inline key must get a gh-app-key.pem Secret entry")
	}
	okPin := strings.Replace(full,
		"      installation_id: 12345",
		"      installation_id: 12345\n      key_file: /secrets/gh-app-key.pem", 1)
	if err := assertSpokeManifestKeyFile(okPin); err != nil {
		t.Errorf("a pin the manifest actually creates was rejected: %v", err)
	}
}

// TestManifestSecretKeysScopedToHiveSecrets pins the scoping. The manifest holds
// several Secrets; reading key names out of the wrong one would make the
// assertion above agree with something it never checked.
func TestManifestSecretKeysScopedToHiveSecrets(t *testing.T) {
	manifest := renderProvisionManifest(t, true, true)
	keys := manifestSecretKeys(manifest, "hive-secrets")
	if _, ok := keys["gh-app-key.pem"]; !ok {
		t.Errorf("hive-secrets keys %v do not include gh-app-key.pem", keys)
	}
	if _, ok := keys["dashboard-token"]; !ok {
		t.Errorf("hive-secrets keys %v do not include dashboard-token", keys)
	}
	// PEM body lines are indented further than an entry name and must not be
	// mistaken for one.
	for k := range keys {
		if strings.Contains(k, "BEGIN") || strings.Contains(k, "END") {
			t.Errorf("PEM body line %q was read as a Secret key name", k)
		}
	}
	if len(manifestSecretKeys(manifest, "no-such-secret")) != 0 {
		t.Error("manifestSecretKeys matched a Secret that does not exist")
	}
}
