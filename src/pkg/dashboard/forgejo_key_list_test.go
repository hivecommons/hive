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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/forgejwt"
	"github.com/hivecommons/hive/pkg/config"
)

const testKeyListIssuer = "https://hive.example.test"

func writeTestSigningKey(t *testing.T, dir, name string) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// keyListServer is a dashboard with dashboard auth on and a
// forgejo.integration block whose key list it serves when serve is true.
func keyListServer(t *testing.T, serve bool) (*Server, *config.ForgejoIntegrationConfig) {
	t.Helper()
	s := newFullServer(t)
	s.authToken = "shared-secret-token"
	dir := t.TempDir()
	in := &config.ForgejoIntegrationConfig{
		Issuer:       testKeyListIssuer,
		KeyFile:      writeTestSigningKey(t, dir, "current.pem"),
		Audiences:    map[string]string{"contributor": "aud-c"},
		ServeKeyList: serve,
	}
	if serve {
		in.PreviousKeyFiles = []string{writeTestSigningKey(t, dir, "previous.pem")}
	}
	s.deps.Config.Forgejo.Integration = in
	return s, in
}

func getKeyList(s *Server, method, path, remote string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	if remote != "" {
		req.RemoteAddr = remote
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func kidsOf(t *testing.T, body []byte) []string {
	t.Helper()
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		t.Fatalf("key list is not JSON: %v: %s", err, body)
	}
	var kids []string
	for _, k := range set.Keys {
		for _, private := range []string{"d", "p", "q", "dp", "dq", "qi"} {
			if _, ok := k[private]; ok {
				t.Fatalf("served key list carries private member %q", private)
			}
		}
		kids = append(kids, k["kid"])
	}
	return kids
}

func TestForgejoKeyListOffByDefault(t *testing.T) {
	s, _ := keyListServer(t, false)
	for _, path := range []string{forgejoDiscoveryRoute, forgejoKeyListRoute} {
		if s.isPublicPath(path) {
			t.Fatalf("%s is public with serve_key_list off", path)
		}
		if rec := getKeyList(s, http.MethodGet, path, ""); rec.Code == http.StatusOK {
			t.Fatalf("anonymous GET %s = 200 with serve_key_list off", path)
		}
		// Even past authentication the handler refuses while off.
		rec := httptest.NewRecorder()
		s.handleForgejoKeyList(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("handler GET %s = %d with serve_key_list off, want 404", path, rec.Code)
		}
	}
	// No forgejo.integration at all, and the package-level check.
	s = newFullServer(t)
	if s.isPublicPath(forgejoKeyListRoute) || isPublicPath(forgejoKeyListRoute) || isPublicPath(forgejoDiscoveryRoute) {
		t.Fatal("key list paths are public with no forgejo.integration")
	}
}

func TestForgejoKeyListServedAnonymously(t *testing.T) {
	s, in := keyListServer(t, true)
	if forgejoDiscoveryRoute != "/.well-known/openid-configuration" || forgejoKeyListRoute != "/jwks.json" {
		t.Fatalf("routes %q %q; update the HandleFunc literals in server.go", forgejoDiscoveryRoute, forgejoKeyListRoute)
	}
	// The other well-known names stay behind authentication.
	for _, p := range []string{"/.well-known/", "/.well-known/other", "/jwks.json/", "/api/jwks.json"} {
		if s.isPublicPath(p) {
			t.Fatalf("%s must not be public", p)
		}
	}

	disc := getKeyList(s, http.MethodGet, forgejoDiscoveryRoute, "")
	jwks := getKeyList(s, http.MethodGet, forgejoKeyListRoute, "")
	for name, rec := range map[string]*httptest.ResponseRecorder{"discovery": disc, "jwks": jwks} {
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d %q, want 200 with no session", name, rec.Code, rec.Body.String())
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Fatalf("%s redirects to %q; Forgejo follows no redirect", name, loc)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("%s Content-Type = %q", name, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=60" {
			t.Fatalf("%s Cache-Control = %q", name, cc)
		}
		body := rec.Body.String()
		if len(body) > forgejwt.MaxDocumentBytes {
			t.Fatalf("%s is %d bytes, over what Forgejo reads", name, len(body))
		}
		for _, secret := range []string{"PRIVATE KEY", in.KeyFile, in.PreviousKeyFiles[0], filepath.Dir(in.KeyFile)} {
			if strings.Contains(body, secret) {
				t.Fatalf("%s carries %q: %s", name, secret, body)
			}
		}
	}
	var d map[string]any
	if err := json.Unmarshal(disc.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d["issuer"] != testKeyListIssuer || d["jwks_uri"] != testKeyListIssuer+forgejoKeyListRoute {
		t.Fatalf("discovery = %v", d)
	}

	// Both the current and the previous key are listed, and a token from
	// either verifies: the rotation overlap.
	want, _, _, err := forgejwt.KeyListFromFiles(testKeyListIssuer, in.KeyFile, in.PreviousKeyFiles)
	if err != nil {
		t.Fatal(err)
	}
	if got := kidsOf(t, jwks.Body.Bytes()); len(got) != 2 || strings.Join(got, ",") != strings.Join(kidsOf(t, want), ",") {
		t.Fatalf("served kids = %v, want current and previous %v", got, kidsOf(t, want))
	}
}

func TestForgejoKeyListGetOnly(t *testing.T) {
	s, _ := keyListServer(t, true)
	for _, m := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete} {
		if rec := getKeyList(s, m, forgejoKeyListRoute, ""); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s = %d, want 405", m, forgejoKeyListRoute, rec.Code)
		}
	}
}

func TestForgejoKeyListRateLimitedPerClient(t *testing.T) {
	s, _ := keyListServer(t, true)
	const client = "203.0.113.7:4000"
	for i := range forgejoKeyListBurst {
		if rec := getKeyList(s, http.MethodGet, forgejoKeyListRoute, client); rec.Code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200 within the burst", i, rec.Code)
		}
	}
	rec := getKeyList(s, http.MethodGet, forgejoKeyListRoute, client)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("over the burst = %d (Retry-After %q), want 429", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := getKeyList(s, http.MethodGet, forgejoKeyListRoute, "198.51.100.9:4000"); rec.Code != http.StatusOK {
		t.Fatalf("another client = %d, want 200", rec.Code)
	}
	// The limiter table is bounded.
	for i := range forgejoKeyListMaxClients + 1 {
		s.forgejoKeys.allow("client-" + strconv.Itoa(i))
	}
	if n := len(s.forgejoKeys.limiters); n > forgejoKeyListMaxClients {
		t.Fatalf("limiter table holds %d clients, cap %d", n, forgejoKeyListMaxClients)
	}
}

func TestForgejoKeyListUnreadableKeyFailsClosed(t *testing.T) {
	s, in := keyListServer(t, true)
	if err := os.Chmod(in.KeyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := getKeyList(s, http.MethodGet, forgejoKeyListRoute, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("world-readable current key = %d, want 503", rec.Code)
	}
	if strings.Contains(rec.Body.String(), in.KeyFile) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("503 body %q / Cache-Control %q: must not name the key path and must not be cached", rec.Body.String(), rec.Header().Get("Cache-Control"))
	}
	// A broken previous key is left out; the current key is still served.
	if err := os.Chmod(in.KeyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	in.PreviousKeyFiles = []string{filepath.Join(t.TempDir(), "gone.pem")}
	rec = getKeyList(s, http.MethodGet, forgejoKeyListRoute, "")
	if rec.Code != http.StatusOK || len(kidsOf(t, rec.Body.Bytes())) != 1 {
		t.Fatalf("missing previous key = %d %s, want the current key alone", rec.Code, rec.Body.String())
	}
}

func TestForgejoKeyListFollowsKeyFileReplacement(t *testing.T) {
	s, in := keyListServer(t, true)
	in.PreviousKeyFiles = nil
	before := kidsOf(t, getKeyList(s, http.MethodGet, forgejoKeyListRoute, "").Body.Bytes())
	fresh := writeTestSigningKey(t, t.TempDir(), "next.pem")
	data, err := os.ReadFile(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(in.KeyFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(in.KeyFile, later, later); err != nil {
		t.Fatal(err)
	}
	after := kidsOf(t, getKeyList(s, http.MethodGet, forgejoKeyListRoute, "").Body.Bytes())
	if len(after) != 1 || after[0] == before[0] {
		t.Fatalf("after replacing key_file the list is %v (was %v), want the new key", after, before)
	}
}
