package openbao

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/keystore"
)

// Health codes other than active/standby are refused with their meaning.
func TestHealthCodes(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{
		{http.StatusNotImplemented, "not initialised"},
		{http.StatusServiceUnavailable, "sealed"},
		{http.StatusInternalServerError, "HTTP 500"},
	} {
		code := tc.code
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		_, err := New(context.Background(), Config{Addr: srv.URL, Token: "t", Key: "k", Timeout: time.Second})
		srv.Close()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("health %d: err = %v, want %q", tc.code, err, tc.want)
		}
	}
	// Standby (429) is fine: a standby forwards requests.
	f := newFake()
	f.keys["hive-test"] = 1
	mux := f.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sys/health" {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()
	p, err := New(context.Background(), Config{Addr: srv.URL, Token: "s.good", Key: "hive-test", Timeout: time.Second})
	if err != nil {
		t.Fatalf("standby: %v", err)
	}
	p.Close()
}

// A TLS server verified through HIVE_OPENBAO_CA_FILE, plus the CA file error
// paths.
func TestCAFile(t *testing.T) {
	f := newFake()
	f.keys["hive-test"] = 1
	srv := httptest.NewTLSServer(f.handler())
	defer srv.Close()
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Addr: srv.URL, Token: "s.good", Key: "hive-test", Timeout: 2 * time.Second, CAFile: caPath}
	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("TLS with CA file: %v", err)
	}
	p.Close()
	if _, err := p.Encrypt(context.Background(), []byte("x"), nil); err != nil {
		t.Fatalf("encrypt over TLS: %v", err)
	}

	// Without the CA the self-signed server is refused.
	cfg.CAFile = ""
	if _, err := New(context.Background(), cfg); err == nil {
		t.Fatal("self-signed server accepted without a CA file")
	}
	cfg.CAFile = filepath.Join(dir, "missing.pem")
	if _, err := New(context.Background(), cfg); err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing CA file: %v", err)
	}
	notPEM := filepath.Join(dir, "junk.pem")
	_ = os.WriteFile(notPEM, []byte("not a certificate"), 0o600)
	cfg.CAFile = notPEM
	if _, err := New(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatalf("junk CA file: %v", err)
	}
	// The certificate parses, so the PEM round trip above was a real cert.
	if _, err := x509.ParseCertificate(srv.Certificate().Raw); err != nil {
		t.Fatal(err)
	}
}

// Malformed replies are errors, never silently empty secrets.
func TestMalformedReplies(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	f.keys["hive-test"] = 1
	_, cfg := startFake(t, f)
	p, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	f.mu.Lock()
	f.emptyCiphertext = true
	f.mu.Unlock()
	if _, err := p.Encrypt(ctx, []byte("x"), nil); err == nil || !strings.Contains(err.Error(), "no ciphertext") {
		t.Fatalf("empty ciphertext: %v", err)
	}
	f.mu.Lock()
	f.emptyCiphertext = false
	f.mu.Unlock()

	ct, err := p.Encrypt(ctx, []byte("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.badPlaintext = true
	f.mu.Unlock()
	if _, err := p.Decrypt(ctx, "", ct, nil); err == nil || !strings.Contains(err.Error(), "not base64") {
		t.Fatalf("bad plaintext: %v", err)
	}
	f.mu.Lock()
	f.badPlaintext = false
	f.mu.Unlock()

	// A non-400 API error on decrypt is passed through, not read as a
	// tamper.
	f.mu.Lock()
	f.token = "s.rotated"
	f.mu.Unlock()
	_, err = p.Decrypt(ctx, "", ct, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden || errors.Is(err, keystore.ErrAuthentication) {
		t.Fatalf("403 on decrypt: %v", err)
	}
	if (&APIError{Status: 500, Path: "/x"}).Error() != "openbao: /x: HTTP 500: Internal Server Error" {
		t.Fatal("APIError without an errors list should fall back to the status text")
	}

	// Kubernetes login without a client token.
	f2 := newFake()
	f2.keys["hive-test"] = 1
	f2.k8sRole, f2.k8sJWT, f2.emptyToken = "hive", "jwt", true
	_, cfg2 := startFake(t, f2)
	jwtFile := filepath.Join(t.TempDir(), "jwt")
	_ = os.WriteFile(jwtFile, []byte("jwt"), 0o600)
	cfg2.Token, cfg2.K8sRole, cfg2.K8sJWTFile = "", "hive", jwtFile
	if _, err := New(ctx, cfg2); err == nil || !strings.Contains(err.Error(), "no client token") {
		t.Fatalf("empty client token: %v", err)
	}

	// A reply that is not JSON at all.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sys/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()
	if _, err := New(ctx, Config{Addr: srv.URL, Token: "t", Key: "k", Timeout: time.Second}); err == nil || !strings.Contains(err.Error(), "decode response") {
		t.Fatalf("non-JSON reply: %v", err)
	}
}

// The renewal loop renews on schedule, re-logs in after a refusal and stops
// on Close; driven at speed through minRenewInterval.
func TestRenewLoop(t *testing.T) {
	prev := minRenewInterval
	minRenewInterval = time.Millisecond
	t.Cleanup(func() { minRenewInterval = prev })

	f := newFake()
	f.keys["hive-test"] = 1
	f.k8sRole, f.k8sJWT = "hive", "jwt"
	// One-second leases from both login and renewal keep every wait in the
	// loop at the (test-shortened) minimum instead of two thirds of a minute.
	f.loginLease, f.renewLease = 1, 1
	_, cfg := startFake(t, f)
	jwtFile := filepath.Join(t.TempDir(), "jwt")
	_ = os.WriteFile(jwtFile, []byte("jwt"), 0o600)
	cfg.Token, cfg.K8sRole, cfg.K8sJWTFile = "", "hive", jwtFile

	// Build without New's own renewal so the loop under test is the only one.
	// Close is also a cleanup so a failed assertion cannot leave the loop
	// reading minRenewInterval while the restore above writes it.
	ctx := context.Background()
	p, err := New(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	t.Cleanup(p.Close)

	outcomes := make(chan error, 16)
	p.onRenewed = func(_ leaseInfo, err error) { outcomes <- err }
	p.startRenewal(leaseInfo{seconds: 1, renewable: true})

	waitFor := func(want func(error) bool, what string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case err := <-outcomes:
				if want(err) {
					return
				}
			case <-deadline:
				t.Fatalf("renewal loop never %s", what)
			}
		}
	}
	waitFor(func(err error) bool { return err == nil }, "renewed")

	// Revoke the token: renew-self is refused, the loop re-logs in.
	f.mu.Lock()
	f.token = "s.second"
	f.mu.Unlock()
	waitFor(func(err error) bool { return err == nil }, "re-logged in after a refusal")
	f.mu.Lock()
	logins := f.logins
	f.mu.Unlock()
	if logins < 2 {
		t.Fatalf("logins = %d, want a re-login", logins)
	}

	// Make the server reject everything: the loop keeps trying on the short
	// cadence instead of exiting.
	f.mu.Lock()
	f.token, f.k8sRole = "s.third", "other"
	f.mu.Unlock()
	waitFor(func(err error) bool { return err != nil }, "reported a failed attempt")
	waitFor(func(err error) bool { return err != nil }, "retried after a failed attempt")
	p.Close()
}

// A static token that is not renewable, or has no TTL, needs no loop; a
// renewable one through lookup-self does.
func TestStaticTokenRenewalDecision(t *testing.T) {
	f := newFake()
	f.keys["hive-test"] = 1
	f.lookupTTL = 0
	_, cfg := startFake(t, f)
	p, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.renewCancel != nil {
		t.Fatal("a token without TTL started renewal")
	}
	f.mu.Lock()
	f.lookupTTL = 3600
	f.mu.Unlock()
	p2, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p2.renewCancel == nil {
		t.Fatal("a renewable token did not start renewal")
	}
	p2.Close()
	p.noteVersion(0)
	if p.KeyID() != "hive-test@v1" {
		t.Fatalf("noteVersion(0) changed KeyID to %s", p.KeyID())
	}
}

// Factory wires ConfigFromEnv into New, so a bad environment is one error.
func TestFactory(t *testing.T) {
	f := newFake()
	f.keys["hive-x"] = 1
	srv, _ := startFake(t, f)
	env := map[string]string{EnvAddr: srv.URL, EnvToken: "s.good", EnvKey: "hive-x"}
	p, err := Factory(context.Background(), func(k string) string { return env[k] })
	if err != nil || p.KeyID() != "hive-x@v1" {
		t.Fatalf("Factory = %v, %v", p, err)
	}
	if prov, ok := p.(*Provider); ok {
		prov.Close()
	}
	delete(env, EnvAddr)
	if _, err := Factory(context.Background(), func(k string) string { return env[k] }); err == nil {
		t.Fatal("Factory without an address succeeded")
	}
}
