package mailin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/hivecommons/hive/internal/testutil"
	"github.com/hivecommons/hive/pkg/mailin/wire"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigFromEnv(t *testing.T) {
	c, err := ConfigFromEnv(envOf(map[string]string{EnvHubURL: "https://hub.example", EnvMaxRecipients: "bogus"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultListen || c.HealthListen != DefaultHealthListen || c.Domain != DefaultDomain || c.HubTokenFile != DefaultHubTokenFile ||
		c.HostsRefresh != defaultHostsPoll || c.MaxMessageBytes != DefaultMaxMessageBytes || c.MaxRecipients != DefaultMaxRecipients ||
		c.MaxConnections != DefaultMaxConnections || c.IPPerHour != DefaultIPPerHour || c.DomainPerHour != DefaultDomainPerHour ||
		c.Limits != DefaultLimits() {
		t.Fatalf("defaults %+v", c)
	}
	c, err = ConfigFromEnv(envOf(map[string]string{
		EnvHubURL: "http://hub.svc", EnvListen: ":25", EnvMaxRecipients: "3", EnvMaxFileBytes: "1000", EnvMaxFiles: "4", EnvHostsRefreshS: "5",
	}))
	if err != nil || c.Listen != ":25" || c.MaxRecipients != 3 || c.Limits.MaxFileBytes != 1000 || c.Limits.MaxFiles != 4 || c.HostsRefresh != 5*time.Second {
		t.Fatalf("overrides %+v %v", c, err)
	}
	if c.HiveDailyBytes != DefaultHiveDailyBytes || c.MaxConcurrentData != DefaultMaxConcurrentData || c.DeliverTimeout != DefaultDeliverTimeout || c.SpoolDir != "" {
		t.Fatalf("new defaults %+v", c)
	}
	c, err = ConfigFromEnv(envOf(map[string]string{
		EnvHubURL: "http://hub.svc", EnvMaxFilesBytes: "2000", EnvMaxMarkdown: "300", EnvMaxBodyBytes: "400", EnvSpoolDir: "/var/spool/hive-mx",
		EnvMaxConcurrent: "3", EnvHiveDailyBytes: "5000", EnvDeliverTimeoutS: "7", EnvMaxMessageBytes: "1234",
	}))
	if err != nil || c.Limits.MaxFilesBytes != 2000 || c.Limits.MaxMarkdownBytes != 300 || c.Limits.MaxBodyBytes != 400 || c.SpoolDir != "/var/spool/hive-mx" ||
		c.MaxConcurrentData != 3 || c.HiveDailyBytes != 5000 || c.DeliverTimeout != 7*time.Second || c.MaxMessageBytes != 1234 {
		t.Fatalf("size overrides %+v %v", c, err)
	}
	for name, env := range map[string]map[string]string{
		"no hub":       {},
		"bad scheme":   {EnvHubURL: "ftp://hub"},
		"half the tls": {EnvHubURL: "https://hub", EnvTLSCert: "/c.pem"},
	} {
		if _, err := ConfigFromEnv(envOf(env)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// writeCert writes a self-signed certificate and key, returning their paths.
func writeCert(t *testing.T, dir, cn string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, DNSNames: []string{cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, keyPath := filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, keyPath
}

func TestCertReloader(t *testing.T) {
	dir := t.TempDir()
	cert, key := writeCert(t, dir, "mx.one.example")
	r := &certReloader{cert: cert, key: key}
	first, err := r.get(nil)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := r.get(nil); again != first {
		t.Fatal("unchanged files must reuse the loaded pair")
	}
	// A renewed certificate is picked up.
	writeCert(t, dir, "mx.two.example")
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(cert, later, later); err != nil {
		t.Fatal(err)
	}
	second, err := r.get(nil)
	if err != nil || second == first {
		t.Fatalf("renewal not loaded: %v", err)
	}
	// A broken renewal keeps serving the last good pair.
	if err := os.WriteFile(key, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	evenLater := later.Add(time.Minute)
	if err := os.Chtimes(cert, evenLater, evenLater); err != nil {
		t.Fatal(err)
	}
	if kept, err := r.get(nil); err != nil || kept != second {
		t.Fatalf("broken renewal: %v", err)
	}
	// Missing files fail.
	if _, err := (&certReloader{cert: filepath.Join(dir, "none"), key: key}).get(nil); err == nil {
		t.Fatal("missing cert accepted")
	}
	if _, err := (&certReloader{cert: cert, key: key}).get(nil); err == nil {
		t.Fatal("bad key accepted on first load")
	}
	if _, err := (Config{TLSCert: cert, TLSKey: key}).tlsConfig(); err == nil {
		t.Fatal("tlsConfig with a bad key")
	}
	if c, err := (Config{}).tlsConfig(); err != nil || c != nil {
		t.Fatal("no certificate means no STARTTLS")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestRunEndToEnd(t *testing.T) {
	got := make(chan wire.Delivery, 1)
	hub := fakeHubServer(t, []string{testHost}, nil, got)
	cert, key := writeCert(t, t.TempDir(), "mx.test")
	cfg, err := ConfigFromEnv(envOf(map[string]string{
		EnvHubURL: hub.URL, EnvHubTokenFile: tokenFile(t, "s3cret"),
		EnvListen: freeAddr(t), EnvHealthListen: freeAddr(t), EnvTLSCert: cert, EnvTLSKey: key,
	}))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Resolver = &fakeResolver{txt: map[string][]string{"example.com": {"v=spf1 ip4:127.0.0.1 -all"}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg, slog.New(slog.DiscardHandler)) }()

	ready := func() bool {
		resp, err := http.Get("http://" + cfg.HealthListen + "/readyz")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	testutil.Eventually(t, 10*time.Second, ready, "hive-mx ready")
	resp, err := http.Get("http://" + cfg.HealthListen + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz %v", err)
	}
	_ = resp.Body.Close()

	// DialStartTLS fails unless the server offers STARTTLS.
	c, err := smtp.DialStartTLS(cfg.Listen, &tls.Config{InsecureSkipVerify: true}) //nolint:gosec // self-signed test certificate
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, ok := c.TLSConnectionState(); !ok {
		t.Fatal("connection is not encrypted after STARTTLS")
	}
	msg := buildMail("ada@example.com", "notes", part{contentType: "text/markdown", disposition: "attachment; filename=n.md", body: mdData})
	if err := send(t, c, "bounce@example.com", []string{"hive@" + testHost}, msg); err != nil {
		t.Fatal(err)
	}
	if d := <-got; d.Kind != wire.KindFiles {
		t.Fatalf("delivery %+v", d)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestRunNotReadyAndErrors(t *testing.T) {
	// A hub that never answers keeps /readyz at 503. freeAddr releases its port
	// before Run binds it, so another process can take it in between; when Run
	// exits early on a bind error, retry on fresh addresses instead of flaking.
	var got int
	for attempt := 0; ; attempt++ {
		cfg := Config{Listen: freeAddr(t), HealthListen: freeAddr(t), HubURL: "http://127.0.0.1:1", HubTokenFile: "/nonexistent",
			MaxConnections: 1, Limits: DefaultLimits()}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Run(ctx, cfg, slog.New(slog.DiscardHandler)) }()
		deadline := time.Now().Add(10 * time.Second)
		early := false
		for !early {
			resp, err := http.Get("http://" + cfg.HealthListen + "/readyz")
			if err == nil {
				_ = resp.Body.Close()
				got = resp.StatusCode
				break
			}
			select {
			case runErr := <-done:
				if runErr == nil || attempt >= 4 {
					cancel()
					t.Fatalf("run exited before health endpoint came up: %v", runErr)
				}
				early = true
			case <-time.After(10 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				cancel()
				t.Fatal("health endpoint never came up")
			}
		}
		if early {
			cancel()
			continue
		}
		if got != http.StatusServiceUnavailable {
			cancel()
			t.Fatalf("readyz %d", got)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		break
	}

	if err := Run(context.Background(), Config{Listen: "256.0.0.1:x", HealthListen: freeAddr(t), MaxConnections: 1}, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("bad SMTP address accepted")
	}
	if err := Run(context.Background(), Config{Listen: freeAddr(t), HealthListen: "256.0.0.1:x", MaxConnections: 1}, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("bad health address accepted")
	}
	if err := Run(context.Background(), Config{TLSCert: "/none", TLSKey: "/none"}, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("missing certificate accepted")
	}
}
