package openbao

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/keystore"
)

// fakeTransit is a minimal OpenBao: health, token lookup/renew, kubernetes
// login, transit key read/create/encrypt/decrypt. Ciphertext is a reversible
// encoding of (key, version, aad, plaintext) so decrypt can check the AAD.
type fakeTransit struct {
	mu        sync.Mutex
	token     string // accepted client token
	namespace string // required X-Vault-Namespace ("" = none)
	keys      map[string]int
	sealed    bool
	k8sRole   string
	k8sJWT    string
	logins    int
	renewals  int
	encrypts  int
	allowNew  bool // may encrypt/create auto-create keys
	lookupTTL int
	// loginLease and renewLease are the lease_duration seconds the kubernetes
	// login and renew-self replies carry (defaults 60 and 120).
	loginLease, renewLease int
	// Malformed-reply toggles, for the provider's decode guards.
	emptyToken      bool // kubernetes login replies without client_token
	emptyCiphertext bool // encrypt replies without ciphertext
	badPlaintext    bool // decrypt replies with plaintext that is not base64
	// hang, when non-nil, makes authenticated handlers block until the test
	// closes it, so the provider's per-request timeout is what ends the
	// client call (no fixed sleep, and the test releases the handler before
	// the server shuts down).
	hang chan struct{}
}

func newFake() *fakeTransit {
	return &fakeTransit{token: "s.good", keys: map[string]int{}, allowNew: true, lookupTTL: 3600, loginLease: 60, renewLease: 120}
}

func (f *fakeTransit) handler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, status int, msg string) {
		writeJSON(w, status, map[string]any{"errors": []string{msg}})
	}
	authed := func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		hang := f.hang
		f.mu.Unlock()
		if hang != nil {
			<-hang
			return false
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.namespace != r.Header.Get("X-Vault-Namespace") {
			fail(w, http.StatusBadRequest, "wrong namespace")
			return false
		}
		if r.Header.Get("X-Vault-Token") != f.token {
			fail(w, http.StatusForbidden, "permission denied")
			return false
		}
		return true
	}
	mux.HandleFunc("/v1/sys/health", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		sealed := f.sealed
		f.mu.Unlock()
		if sealed {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"sealed": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"sealed": false})
	})
	mux.HandleFunc("/v1/auth/kubernetes/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Role, JWT string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if body.Role != f.k8sRole || body.JWT != f.k8sJWT {
			fail(w, http.StatusForbidden, "invalid role or jwt")
			return
		}
		f.logins++
		tok := f.token
		if f.emptyToken {
			tok = ""
		}
		writeJSON(w, http.StatusOK, map[string]any{"auth": map[string]any{
			"client_token": tok, "lease_duration": f.loginLease, "renewable": true}})
	})
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"ttl": f.lookupTTL, "renewable": f.lookupTTL > 0}})
	})
	mux.HandleFunc("/v1/auth/token/renew-self", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		f.mu.Lock()
		f.renewals++
		lease := f.renewLease
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"auth": map[string]any{"lease_duration": lease, "renewable": true}})
	})
	mux.HandleFunc("/v1/transit/keys/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v1/transit/keys/")
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			v, ok := f.keys[name]
			if !ok {
				fail(w, http.StatusNotFound, "")
				return
			}
			keys := map[string]any{}
			for i := 1; i <= v; i++ {
				keys[string(rune('0'+i))] = 1700000000 + i
			}
			writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{
				"name": name, "type": "aes256-gcm96", "keys": keys, "min_decryption_version": 1, "supports_encryption": true}})
		case http.MethodPost:
			if !f.allowNew {
				fail(w, http.StatusForbidden, "permission denied")
				return
			}
			if _, ok := f.keys[name]; !ok {
				f.keys[name] = 1
			}
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("/v1/transit/encrypt/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v1/transit/encrypt/")
		var body struct {
			Plaintext      string `json:"plaintext"`
			AssociatedData string `json:"associated_data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		v, ok := f.keys[name]
		if !ok {
			fail(w, http.StatusBadRequest, "encryption key not found")
			return
		}
		f.encrypts++
		ct := "vault:v" + string(rune('0'+v)) + ":" + base64.StdEncoding.EncodeToString([]byte(name+"|"+body.AssociatedData+"|"+body.Plaintext))
		if f.emptyCiphertext {
			ct = ""
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"ciphertext": ct, "key_version": v}})
	})
	mux.HandleFunc("/v1/transit/decrypt/", func(w http.ResponseWriter, r *http.Request) {
		if !authed(w, r) {
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/v1/transit/decrypt/")
		var body struct {
			Ciphertext     string `json:"ciphertext"`
			AssociatedData string `json:"associated_data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		parts := strings.SplitN(body.Ciphertext, ":", 3)
		if len(parts) != 3 {
			fail(w, http.StatusBadRequest, "invalid ciphertext")
			return
		}
		raw, err := base64.StdEncoding.DecodeString(parts[2])
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid ciphertext")
			return
		}
		fields := strings.SplitN(string(raw), "|", 3)
		if len(fields) != 3 || fields[0] != name || fields[1] != body.AssociatedData {
			fail(w, http.StatusBadRequest, "cipher: message authentication failed")
			return
		}
		pt := fields[2]
		f.mu.Lock()
		if f.badPlaintext {
			pt = "not*base64"
		}
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"plaintext": pt}})
	})
	return mux
}

func startFake(t *testing.T, f *fakeTransit) (*httptest.Server, Config) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	return srv, Config{Addr: srv.URL, Token: "s.good", Key: "hive-test", Timeout: 2 * time.Second}
}

func TestRoundTripThroughKeystoreEnvelope(t *testing.T) {
	f := newFake()
	f.keys["hive-test"] = 2
	_, cfg := startFake(t, f)
	ctx := context.Background()
	p, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.Name() != ProviderName || p.KeyID() != "hive-test@v2" {
		t.Fatalf("Name/KeyID = %s/%s", p.Name(), p.KeyID())
	}
	env, err := keystore.Seal(ctx, p, []byte("sk-live"), []byte("file:k"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env, keystore.Prefix+"v1:openbao:hive-test@v2:") || strings.Contains(env, "sk-live") {
		t.Fatalf("envelope = %q", env)
	}
	pt, stale, err := keystore.Open(ctx, p, env, []byte("file:k"))
	if err != nil || stale || string(pt) != "sk-live" {
		t.Fatalf("Open = %q, stale=%v, %v", pt, stale, err)
	}
	// AAD mismatch is an authentication failure, not a transport error.
	if _, _, err := keystore.Open(ctx, p, env, []byte("file:other")); !errors.Is(err, keystore.ErrAuthentication) {
		t.Fatalf("aad mismatch: %v", err)
	}
	// A server-side key rotation shows up as a stale generation.
	f.mu.Lock()
	f.keys["hive-test"] = 3
	f.mu.Unlock()
	env2, _ := keystore.Seal(ctx, p, []byte("x"), nil)
	if p.KeyID() != "hive-test@v3" || !strings.Contains(env2, "@v3:") {
		t.Fatalf("KeyID after rotation = %s", p.KeyID())
	}
	if _, stale, err := keystore.Open(ctx, p, env, []byte("file:k")); err != nil || !stale {
		t.Fatalf("old envelope after rotation: stale=%v, %v", stale, err)
	}
	if f.encrypts != 2 {
		t.Fatalf("encrypt calls = %d", f.encrypts)
	}
}

func TestNewRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		_, err := New(ctx, Config{Addr: srv.URL, Token: "t", Key: "k", Timeout: time.Second})
		if err == nil || !strings.Contains(err.Error(), "unreachable") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("sealed", func(t *testing.T) {
		f := newFake()
		f.sealed = true
		_, cfg := startFake(t, f)
		if _, err := New(ctx, cfg); err == nil || !strings.Contains(err.Error(), "sealed") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("bad token", func(t *testing.T) {
		f := newFake()
		_, cfg := startFake(t, f)
		cfg.Token = "s.wrong"
		_, err := New(ctx, cfg)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden || !strings.Contains(err.Error(), "token rejected") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing key and no create", func(t *testing.T) {
		f := newFake()
		f.allowNew = false
		_, cfg := startFake(t, f)
		if _, err := New(ctx, cfg); err == nil || !strings.Contains(err.Error(), "bao write -f transit/keys/hive-test") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("missing key auto-created", func(t *testing.T) {
		f := newFake()
		_, cfg := startFake(t, f)
		p, err := New(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		if f.keys["hive-test"] != 1 || p.KeyID() != "hive-test@v1" {
			t.Fatalf("key not created: %v %s", f.keys, p.KeyID())
		}
	})
	t.Run("bad addr and names", func(t *testing.T) {
		if _, err := New(ctx, Config{Addr: "ftp://x", Token: "t", Key: "k"}); err == nil {
			t.Fatal("ftp addr accepted")
		}
		if _, err := New(ctx, Config{Addr: "http://x", Token: "t", Key: "a/b"}); err == nil {
			t.Fatal("slash in key accepted")
		}
		if _, err := New(ctx, Config{Addr: "http://x", Token: "t"}); !errors.Is(err, ErrNoHiveID) {
			t.Fatalf("empty key: %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		f := newFake()
		f.keys["hive-test"] = 1
		_, cfg := startFake(t, f)
		p, err := New(ctx, cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer p.Close()
		release := make(chan struct{})
		f.mu.Lock()
		f.hang = release
		f.mu.Unlock()
		// Release the handler however the assertion goes, or the server's
		// Close in cleanup waits on it forever.
		defer close(release)
		p.cfg.Timeout = 50 * time.Millisecond
		if _, err := p.Encrypt(ctx, []byte("x"), nil); err == nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("hung server: err = %v, want deadline exceeded", err)
		}
	})
}

func TestNamespaceHeaderAndNoPlaintextCached(t *testing.T) {
	f := newFake()
	f.namespace = "team-a/"
	f.keys["hive-test"] = 1
	_, cfg := startFake(t, f)
	cfg.Namespace = "team-a/"
	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ct, err := p.Encrypt(context.Background(), []byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ct), "secret") {
		t.Fatal("ciphertext carries plaintext")
	}
	// Without the namespace the server refuses.
	cfg.Namespace = ""
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("missing namespace accepted")
	}
}

func TestKubernetesLoginAndRenewal(t *testing.T) {
	f := newFake()
	f.keys["hive-test"] = 1
	f.k8sRole = "hive"
	f.k8sJWT = "eyJ.test.jwt"
	_, cfg := startFake(t, f)
	jwtFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwtFile, []byte(f.k8sJWT+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Token = ""
	cfg.K8sRole = "hive"
	cfg.K8sJWTFile = jwtFile
	ctx := context.Background()
	p, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if f.logins != 1 {
		t.Fatalf("logins = %d", f.logins)
	}
	if _, err := p.Encrypt(ctx, []byte("x"), nil); err != nil {
		t.Fatal(err)
	}
	// Renewal extends the lease.
	next, err := p.renewOnce(ctx)
	if err != nil || next.seconds != 120 || f.renewals != 1 {
		t.Fatalf("renewOnce = %+v, %v (renewals %d)", next, err, f.renewals)
	}
	// A refused renewal (the token was revoked) falls back to a fresh login.
	f.mu.Lock()
	f.token = "s.rotated"
	f.mu.Unlock()
	next, err = p.renewOnce(ctx)
	if err != nil || f.logins != 2 || next.seconds != 60 {
		t.Fatalf("re-login = %+v, %v (logins %d)", next, err, f.logins)
	}
	if _, err := p.Encrypt(ctx, []byte("y"), nil); err != nil {
		t.Fatalf("encrypt after re-login: %v", err)
	}
	// Wrong role is refused at boot.
	cfg.K8sRole = "other"
	if _, err := New(ctx, cfg); err == nil || !strings.Contains(err.Error(), "kubernetes auth") {
		t.Fatalf("wrong role: %v", err)
	}
	cfg.K8sRole = "hive"
	cfg.K8sJWTFile = filepath.Join(t.TempDir(), "missing")
	if _, err := New(ctx, cfg); err == nil || !strings.Contains(err.Error(), "service account token") {
		t.Fatalf("missing jwt: %v", err)
	}
}

func TestRenewIntervalBounds(t *testing.T) {
	if got := renewInterval(3600); got != 2400*time.Second {
		t.Fatalf("renewInterval(3600) = %v", got)
	}
	if got := renewInterval(1); got != minRenewInterval {
		t.Fatalf("renewInterval(1) = %v", got)
	}
	if versionFromCiphertext("vault:v7:abc") != 7 || versionFromCiphertext("junk") != 0 || versionFromCiphertext("vault:x:abc") != 0 {
		t.Fatal("versionFromCiphertext")
	}
	p := &Provider{}
	p.startRenewal(leaseInfo{seconds: 0, renewable: true})
	p.startRenewal(leaseInfo{seconds: 100, renewable: false})
	if p.renewCancel != nil {
		t.Fatal("renewal started for a token that needs none")
	}
	p.startRenewal(leaseInfo{seconds: 100, renewable: true})
	if p.renewCancel == nil {
		t.Fatal("renewal not started")
	}
	p.Close()
	p.Close()
}

func TestConfigFromEnv(t *testing.T) {
	files := map[string][]byte{"/tok": []byte("s.file\n"), hiveIDFile: []byte("abc123\n")}
	readFile := func(p string) ([]byte, error) {
		if b, ok := files[p]; ok {
			return b, nil
		}
		return nil, os.ErrNotExist
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

	if _, err := ConfigFromEnv(env(nil), readFile); err == nil || !strings.Contains(err.Error(), EnvAddr) {
		t.Fatalf("no addr: %v", err)
	}
	if _, err := ConfigFromEnv(env(map[string]string{EnvAddr: "http://x"}), readFile); err == nil || !strings.Contains(err.Error(), EnvK8sRole) {
		t.Fatalf("no credential: %v", err)
	}
	cfg, err := ConfigFromEnv(env(map[string]string{EnvAddr: "http://x", EnvTokenFile: "/tok"}), readFile)
	if err != nil || cfg.Token != "s.file" || cfg.Key != "hive-abc123" || cfg.Timeout != DefaultTimeout {
		t.Fatalf("token file + hive id file: %+v, %v", cfg, err)
	}
	cfg, err = ConfigFromEnv(env(map[string]string{EnvAddr: "http://x", EnvToken: "s.env", EnvHiveID: "h1", EnvTimeout: "3s", EnvNamespace: "ns/"}), readFile)
	if err != nil || cfg.Token != "s.env" || cfg.Key != "hive-h1" || cfg.Timeout != 3*time.Second || cfg.Namespace != "ns/" {
		t.Fatalf("env: %+v, %v", cfg, err)
	}
	if _, err := ConfigFromEnv(env(map[string]string{EnvAddr: "http://x", EnvToken: "t", EnvKey: "k", EnvTimeout: "soon"}), readFile); err == nil {
		t.Fatal("bad timeout accepted")
	}
	if _, err := ConfigFromEnv(env(map[string]string{EnvAddr: "http://x", EnvTokenFile: "/missing"}), readFile); err == nil {
		t.Fatal("missing token file accepted")
	}
	noID := func(p string) ([]byte, error) { return nil, os.ErrNotExist }
	if _, err := ConfigFromEnv(env(map[string]string{EnvAddr: "http://x", EnvToken: "t"}), noID); !errors.Is(err, ErrNoHiveID) {
		t.Fatalf("no hive id: %v", err)
	}
}

// The provider is selectable through the keystore registry, and a failing
// server refuses boot rather than leaving plaintext behind.
func TestRegisteredWithKeystore(t *testing.T) {
	f := newFake()
	f.keys["hive-h1"] = 1
	srv, _ := startFake(t, f)
	keystore.ResetForTest()
	t.Cleanup(keystore.ResetForTest)
	env := map[string]string{keystore.EnvProviderName: ProviderName, EnvAddr: srv.URL, EnvToken: "s.good", EnvHiveID: "h1"}
	st, err := keystore.Init(context.Background(), func(k string) string { return env[k] }, nil)
	if err != nil || st.Provider != ProviderName || st.KeyID != "hive-h1@v1" || !st.Active() {
		t.Fatalf("Init = %+v, %v", st, err)
	}
	if p, ok := keystore.Active().(*Provider); !ok {
		t.Fatalf("Active = %T", keystore.Active())
	} else {
		p.Close()
	}
	keystore.ResetForTest()
	env[EnvToken] = "s.bad"
	if _, err := keystore.Init(context.Background(), func(k string) string { return env[k] }, nil); err == nil || keystore.Active() != nil {
		t.Fatalf("bad token: err=%v active=%v", err, keystore.Active())
	}
}
