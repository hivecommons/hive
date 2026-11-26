package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
)

// forgejoRESTTestConfig is a Forgejo config with the env token set and an
// integration whose rest_audience is aud, signing with a fresh key.
func forgejoRESTTestConfig(t *testing.T, baseURL, aud string) *config.Config {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(t.TempDir(), "forgejo-jwt.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_TEST_FORGEJO_REST_TOKEN", "env-token")
	cfg := &config.Config{}
	cfg.Project.Org = "acme"
	cfg.Project.Forge = config.ForgeForgejo
	cfg.Forgejo.URL = baseURL
	cfg.Forgejo.TokenEnv = "HIVE_TEST_FORGEJO_REST_TOKEN"
	cfg.Forgejo.Integration = &config.ForgejoIntegrationConfig{
		Issuer:       "https://keys.example.com",
		KeyFile:      keyFile,
		Audiences:    map[string]string{"contributor": "aud-c"},
		RESTAudience: aud,
	}
	return cfg
}

// jwtClaims decodes the payload of a compact JWT without verifying it.
func jwtClaims(t *testing.T, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q is not a compact JWT", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestForgejoRESTTokenMintsForTheRESTAudience(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	prev := forgejoRESTNow
	forgejoRESTNow = func() time.Time { return now }
	t.Cleanup(func() { forgejoRESTNow = prev })

	cfg := forgejoRESTTestConfig(t, "https://forgejo.example.com", "aud-rest")
	tok, source, err := forgejoRESTToken(cfg)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if source != forgejoRESTSource || tok == "env-token" {
		t.Fatalf("token %q from %q, want a JWT from %q", tok, source, forgejoRESTSource)
	}
	c := jwtClaims(t, tok)
	if c["aud"] != "aud-rest" || c["sub"] != "hive:rest" || c["iss"] != "https://keys.example.com" {
		t.Fatalf("claims %v, want aud aud-rest, sub hive:rest, iss https://keys.example.com", c)
	}

	// Reused while well before expiry.
	now = now.Add(30 * time.Minute)
	again, _, err := forgejoRESTToken(cfg)
	if err != nil || again != tok {
		t.Fatalf("30 minutes in: token changed (%v)", err)
	}
	// Minted again inside the refresh margin.
	now = now.Add(25 * time.Minute)
	fresh, _, err := forgejoRESTToken(cfg)
	if err != nil || fresh == tok {
		t.Fatalf("55 minutes in: token not refreshed (%v)", err)
	}
	if exp, _ := jwtClaims(t, fresh)["exp"].(float64); int64(exp) != now.Add(time.Hour).Unix() {
		t.Fatalf("refreshed exp = %v, want %d", exp, now.Add(time.Hour).Unix())
	}
}

func TestForgejoRESTTokenFallsBackToTheAccessToken(t *testing.T) {
	cfg := forgejoRESTTestConfig(t, "https://forgejo.example.com", "")
	for name, mutate := range map[string]func(*config.Config){
		"no rest_audience": func(*config.Config) {},
		"no integration":   func(c *config.Config) { c.Forgejo.Integration = nil },
	} {
		mutate(cfg)
		tok, source, err := forgejoRESTToken(cfg)
		if err != nil || tok != "env-token" || !strings.Contains(source, "HIVE_TEST_FORGEJO_REST_TOKEN") {
			t.Fatalf("%s: token %q from %q (%v), want the env token", name, tok, source, err)
		}
	}
}

func TestForgejoRESTTokenMintFailureBuildsNoClient(t *testing.T) {
	cfg := forgejoRESTTestConfig(t, "https://forgejo.example.com", "aud-rest")
	cfg.Forgejo.Integration.KeyFile = filepath.Join(t.TempDir(), "missing.pem")
	var built int
	prev := newForgejoSweepAPI
	newForgejoSweepAPI = func(string, forge.Options) (forge.SweepAPI, error) { built++; return nil, nil }
	t.Cleanup(func() { newForgejoSweepAPI = prev })
	if _, err := forgejoSweepAPIForRepo(cfg, "acme/app"); err == nil || built != 0 {
		t.Fatalf("err %v, %d clients built; want a mint error and no client", err, built)
	}
	if _, err := forgeListerForRepo(cfg, "acme/app"); err == nil || !strings.Contains(err.Error(), "rest_audience") {
		t.Fatalf("lister err = %v, want a rest_audience mint error", err)
	}
}

// TestForgejoRESTAdaptersSendTheJWT covers each REST adapter: with
// rest_audience set, none of them sends the access token.
func TestForgejoRESTAdaptersSendTheJWT(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	cfg := forgejoRESTTestConfig(t, srv.URL, "aud-rest")
	want, _, err := forgejoRESTToken(cfg)
	if err != nil {
		t.Fatal(err)
	}

	l, err := forgeListerForRepo(cfg, "acme/app")
	if err != nil {
		t.Fatalf("lister: %v", err)
	}
	_, _ = l.ListOpenIssues(context.Background(), "acme/app")
	mu.Lock()
	if len(auths) == 0 || auths[0] != "token "+want {
		t.Fatalf("lister sent %q, want the minted JWT", auths)
	}
	mu.Unlock()

	var sweepToken string
	prevSweep := newForgejoSweepAPI
	newForgejoSweepAPI = func(token string, _ forge.Options) (forge.SweepAPI, error) { sweepToken = token; return nil, nil }
	t.Cleanup(func() { newForgejoSweepAPI = prevSweep })
	if _, err := forgejoSweepAPIForRepo(cfg, "acme/app"); err != nil || sweepToken != want {
		t.Fatalf("sweep client built with %q (%v), want the minted JWT", sweepToken, err)
	}

	var mergeToken string
	prevMerge := newForgeMerger
	newForgeMerger = func(_ forge.Kind, token string, _ forge.Options) (forge.Merger, error) {
		mergeToken = token
		return nil, nil
	}
	t.Cleanup(func() { newForgeMerger = prevMerge })
	if _, err := forgeMergerForRepo(cfg, "acme/app"); err != nil || mergeToken != want {
		t.Fatalf("merger built with %q (%v), want the minted JWT", mergeToken, err)
	}
}

// TestForgejoRESTAudienceMakesTheAccessTokenOptional: with rest_audience set
// and the access token unset, the minted JWT satisfies requireForgeToken and
// every REST adapter builds; without rest_audience the empty access token is
// still refused with errForgeTokenEmpty, exactly as before.
func TestForgejoRESTAudienceMakesTheAccessTokenOptional(t *testing.T) {
	cfg := forgejoRESTTestConfig(t, "https://forgejo.example.com", "aud-rest")
	t.Setenv("HIVE_TEST_FORGEJO_REST_TOKEN", "")
	prevSweep := newForgejoSweepAPI
	newForgejoSweepAPI = func(string, forge.Options) (forge.SweepAPI, error) { return nil, nil }
	t.Cleanup(func() { newForgejoSweepAPI = prevSweep })

	if _, err := forgeListerForRepo(cfg, "acme/app"); err != nil {
		t.Errorf("lister with rest_audience and no access token: %v", err)
	}
	if _, err := forgeMergerForRepo(cfg, "acme/app"); err != nil {
		t.Errorf("merger with rest_audience and no access token: %v", err)
	}
	if _, err := forgejoSweepAPIForRepo(cfg, "acme/app"); err != nil {
		t.Errorf("sweep with rest_audience and no access token: %v", err)
	}
	if got := forgeKindOf(t, forgeForRepo(cfg, "acme/app", nil, discardLogger())); got != forge.KindForgejo {
		t.Errorf("writer with rest_audience and no access token: kind %q, want forgejo", got)
	}

	cfg.Forgejo.Integration.RESTAudience = ""
	if _, err := forgeListerForRepo(cfg, "acme/app"); !errors.Is(err, errForgeTokenEmpty) {
		t.Errorf("lister without rest_audience: err %v, want errForgeTokenEmpty", err)
	}
	if _, err := forgeMergerForRepo(cfg, "acme/app"); !errors.Is(err, errForgeTokenEmpty) {
		t.Errorf("merger without rest_audience: err %v, want errForgeTokenEmpty", err)
	}
	if forgeForRepo(cfg, "acme/app", nil, discardLogger()) != nil {
		t.Error("writer without rest_audience and no access token: built, want none")
	}
}
