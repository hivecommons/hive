package dashboard

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// forgeTestKeyBits keeps test key generation fast; strength is irrelevant —
// the key only has to be parseable for fingerprinting.
const forgeTestKeyBits = 2048

// writeForgeTestKey writes a parseable RSA private key PEM to dir and returns
// its path and the PEM body (so tests can assert the material never appears in
// a response).
func writeForgeTestKey(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, forgeTestKeyBits)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path, string(pemBytes)
}

func getForgeApps(t *testing.T, s *Server) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/config/github/forge-apps", nil)
	markOwnerRequest(req)
	s.mux.ServeHTTP(rec, req)
	var body map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}
	return rec, body
}

// TestForgeApps_ProviderInjected_FingerprintsNeverKeyMaterial feeds the
// provider real key paths and asserts the response carries fingerprints and
// paths ONLY — no fragment of the PEM may appear anywhere in the body.
func TestForgeApps_ProviderInjected_FingerprintsNeverKeyMaterial(t *testing.T) {
	srv := newFullServer(t)
	dir := t.TempDir()
	activePath, activePEM := writeForgeTestKey(t, dir, "gh-app-key.pem")
	heldPath, heldPEM := writeForgeTestKey(t, dir, "gh-app-key-777.pem")
	heldFP, err := config.AppKeyFingerprintFromFile(heldPath)
	if err != nil || heldFP == "" {
		t.Fatalf("fingerprint held key: %v", err)
	}
	activeFP, err := config.AppKeyFingerprintFromFile(activePath)
	if err != nil || activeFP == "" {
		t.Fatalf("fingerprint active key: %v", err)
	}

	srv.SetForgeAppInventoryFn(func() ForgeAppInventory {
		return ForgeAppInventory{
			ActiveKeyFile: activePath,
			HeldKeys: []ForgeAppKey{
				{AppID: "777", Path: heldPath, Fingerprint: heldFP},
			},
		}
	})
	srv.deps.Config.GitHub.AppID = 123456
	srv.deps.Config.GitHub.InstallationID = 42980

	rec, body := getForgeApps(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	raw := rec.Body.String()
	// The load-bearing assertion: key MATERIAL is absent even though the
	// provider was fed real key paths.
	if strings.Contains(raw, "PRIVATE KEY") {
		t.Fatalf("response contains PEM header — key material leaked: %s", raw)
	}
	for _, pemBody := range []string{activePEM, heldPEM} {
		// Check a distinctive base64 chunk from the middle of the key too, in
		// case a future edit strips headers but leaks the payload.
		lines := strings.Split(strings.TrimSpace(pemBody), "\n")
		if len(lines) > 2 {
			if strings.Contains(raw, lines[len(lines)/2]) {
				t.Fatalf("response contains key material payload")
			}
		}
	}

	active, _ := body["active"].(map[string]any)
	if active == nil {
		t.Fatalf("missing active in %s", raw)
	}
	if active["key_file"] != activePath {
		t.Errorf("active key_file = %v, want %s", active["key_file"], activePath)
	}
	if active["key_fingerprint"] != activeFP {
		t.Errorf("active key_fingerprint = %v, want %s", active["key_fingerprint"], activeFP)
	}
	held, _ := body["held_keys"].([]any)
	if len(held) != 1 {
		t.Fatalf("held_keys = %v, want 1 row", body["held_keys"])
	}
	row := held[0].(map[string]any)
	if row["app_id"] != "777" || row["fingerprint"] != heldFP || row["path"] != heldPath {
		t.Errorf("held key row = %v, want app 777 with fingerprint %s at %s", row, heldFP, heldPath)
	}
}

// TestForgeApps_AuthStateVerbatim pins the classified wire token passing
// through unprettified — it is the debugging surface.
func TestForgeApps_AuthStateVerbatim(t *testing.T) {
	srv := newFullServer(t)
	srv.SetGitHubAppRequired(true)
	srv.SetGitHubAppState("wrong-installation")

	rec, body := getForgeApps(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	active := body["active"].(map[string]any)
	if active["auth_state"] != "wrong-installation" {
		t.Errorf("auth_state = %v, want the wire token verbatim", active["auth_state"])
	}
}

// TestForgeApps_EditableOnlyWhenForgeMatchesRepos drives the editable rule
// through the endpoint: a config whose forge matches the repos' forge (from
// the base/api URL host, empty = github.com) is editable; a config whose
// explicit forge names a different host is not.
func TestForgeApps_EditableOnlyWhenForgeMatchesRepos(t *testing.T) {
	t.Run("default github.com config is editable", func(t *testing.T) {
		srv := newFullServer(t)
		rec, body := getForgeApps(t, srv)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		active := body["active"].(map[string]any)
		if active["editable"] != true {
			t.Errorf("editable = %v, want true — repos and App both resolve to github.com", active["editable"])
		}
		if body["repos_forge"] != "github.com" {
			t.Errorf("repos_forge = %v, want github.com for empty URLs", body["repos_forge"])
		}
	})

	t.Run("GHE config with GHE repos is editable", func(t *testing.T) {
		srv := newFullServer(t)
		srv.deps.Config.GitHub.APIURL = "https://github.ibm.com/api/v3"
		rec, body := getForgeApps(t, srv)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		active := body["active"].(map[string]any)
		if active["editable"] != true {
			t.Errorf("editable = %v, want true — App forge and repos forge are both github.ibm.com", active["editable"])
		}
	})

	t.Run("explicit forge differing from repos host is read-only", func(t *testing.T) {
		srv := newFullServer(t)
		// Repos live on github.com (empty URLs) but the config claims an App
		// on a different forge: the one case that must NOT be editable.
		srv.deps.Config.GitHub.Forge_ = "github.ibm.com"
		rec, body := getForgeApps(t, srv)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		active := body["active"].(map[string]any)
		if active["editable"] != false {
			t.Errorf("editable = %v, want false — App forge github.ibm.com does not match repos forge github.com", active["editable"])
		}
	})
}

// TestForgeAppEditable is the unit truth table for the pure comparison.
func TestForgeAppEditable(t *testing.T) {
	cases := []struct {
		name                 string
		appForge, reposForge string
		want                 bool
	}{
		{"both empty means github.com", "", "", true},
		{"empty vs explicit github.com", "", "github.com", true},
		{"case-insensitive match", "GitHub.com", "github.com", true},
		{"GHE match", "github.ibm.com", "github.ibm.com", true},
		{"GHE vs public mismatch", "github.ibm.com", "github.com", false},
		{"public vs GHE mismatch", "", "github.ibm.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := forgeAppEditable(tc.appForge, tc.reposForge); got != tc.want {
				t.Errorf("forgeAppEditable(%q, %q) = %v, want %v", tc.appForge, tc.reposForge, got, tc.want)
			}
		})
	}
}

// TestForgeApps_NoProvider proves the endpoint degrades gracefully when
// cmd/hive has not injected an inventory (tests, early boot): config values
// still serve, with no held keys.
func TestForgeApps_NoProvider(t *testing.T) {
	srv := newFullServer(t)
	rec, body := getForgeApps(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 without a provider", rec.Code)
	}
	if held, ok := body["held_keys"].([]any); ok && len(held) != 0 {
		t.Errorf("held_keys = %v, want empty without a provider", held)
	}
}

// TestForgeApps_InstallURLIsServerBuilt locks the tab's install affordance to
// the server-side builder (config.AppInstallURL — the same one the install
// banner uses). The post-reset shape matters most: raw base_url/app_slug
// blank, forge known only via api_url, installation_id 0 — the exact vllmd-13
// state in which the tab used to be a dead end.
func TestForgeApps_InstallURLIsServerBuilt(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.GitHub = config.GitHubConfig{
		AppID:          config.EnterpriseGitHubAppID,
		InstallationID: 0,
		APIURL:         "https://github.ibm.com/api/v3",
	}
	rec, body := getForgeApps(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	active, ok := body["active"].(map[string]any)
	if !ok {
		t.Fatalf("no active object in response: %v", body)
	}
	want := "https://github.ibm.com/github-apps/" + config.EnterpriseGitHubAppSlug + "/installations/new"
	if got, _ := active["install_url"].(string); got != want {
		t.Errorf("active.install_url = %q, want %q (resolved forge, despite blank raw base_url/app_slug)", got, want)
	}
	if got, _ := active["installation_id"].(float64); got != 0 {
		t.Errorf("active.installation_id = %v, want 0 (the trigger for the tab's install link)", got)
	}
}
