package hub

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newSelfSignedTLSServer starts an HTTPS test server on 127.0.0.1 using a
// FRESHLY generated self-signed certificate, and returns the server together
// with that certificate's DER bytes.
//
// httptest.NewTLSServer is deliberately NOT used here: it serves the same
// built-in certificate for every server it creates, so "trust server A's cert"
// would implicitly trust server B as well and the negative test below could
// never fail. Each server needs its own distinct identity for the CA pinning
// assertion to mean anything.
func newSelfSignedTLSServer(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "hive-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, der
}

// writeCertPEM writes a DER certificate out as a PEM file and returns its path.
func writeCertPEM(t *testing.T, dir, name string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if pemBytes == nil {
		t.Fatal("pem.EncodeToMemory returned nil")
	}
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestInClusterHTTPClient_MissingCAFailsClosed pins the fail-closed contract on
// a missing serviceaccount CA. The serviceaccount CA is the ONLY root that can
// attest the in-cluster API server, so its absence must be a hard error at
// client-construction time — not a silent fall-through to the system pool that
// surfaces later as an opaque x509 error.
func TestInClusterHTTPClient_MissingCAFailsClosed(t *testing.T) {
	cfg := &inClusterConfig{
		BaseURL: "https://10.0.0.1:443",
		CAPath:  filepath.Join(t.TempDir(), "does-not-exist.crt"),
	}
	client, err := inClusterHTTPClient(cfg)
	if err == nil {
		t.Fatal("a missing k8s CA must fail closed, got a usable client and nil error")
	}
	if client != nil {
		t.Errorf("client = %v, want nil when the CA cannot be read", client)
	}
	if !strings.Contains(err.Error(), cfg.CAPath) {
		t.Errorf("error %q should name the CA path so the misconfiguration is diagnosable", err)
	}
}

// TestInClusterHTTPClient_MalformedCAFailsClosed covers the subtler half: a CA
// file that EXISTS and reads fine but contains no certificates. Ignoring
// AppendCertsFromPEM's return value here would leave a client whose only roots
// are the public system CAs, which cannot verify a cluster-internal API cert.
func TestInClusterHTTPClient_MalformedCAFailsClosed(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, []byte("this is not a PEM certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &inClusterConfig{BaseURL: "https://10.0.0.1:443", CAPath: caPath}

	client, err := inClusterHTTPClient(cfg)
	if err == nil {
		t.Fatal("a CA file containing no PEM certificates must fail closed, got nil error")
	}
	if client != nil {
		t.Errorf("client = %v, want nil for an unusable CA", client)
	}
	if !strings.Contains(err.Error(), "PEM") {
		t.Errorf("error %q should explain that the file held no PEM certificates", err)
	}
}

// TestInClusterHTTPClient_ValidCAVerifiesServer is the positive proof that the
// CA is genuinely INSTALLED as a trust root and not merely read and discarded:
// a TLS server presenting that exact certificate must verify successfully.
func TestInClusterHTTPClient_ValidCAVerifiesServer(t *testing.T) {
	srv, der := newSelfSignedTLSServer(t)

	caPath := writeCertPEM(t, t.TempDir(), "ca.crt", der)
	cfg := &inClusterConfig{BaseURL: srv.URL, CAPath: caPath}

	client, err := inClusterHTTPClient(cfg)
	if err != nil {
		t.Fatalf("inClusterHTTPClient with a valid CA = %v, want nil error", err)
	}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("TLS handshake against the server whose cert IS the CA failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
}

// TestInClusterHTTPClient_RejectsServerNotSignedByCA is the negative half of the
// same proof, and the one that actually matters for security: a server whose
// certificate is NOT the configured CA must be rejected. If the implementation
// ever regressed to trusting the system pool, or to InsecureSkipVerify, this is
// the test that catches it.
func TestInClusterHTTPClient_RejectsServerNotSignedByCA(t *testing.T) {
	_, trustedDER := newSelfSignedTLSServer(t)
	untrusted, _ := newSelfSignedTLSServer(t)

	// Trust ONLY the first server's certificate.
	caPath := writeCertPEM(t, t.TempDir(), "ca.crt", trustedDER)
	cfg := &inClusterConfig{BaseURL: untrusted.URL, CAPath: caPath}

	client, err := inClusterHTTPClient(cfg)
	if err != nil {
		t.Fatalf("inClusterHTTPClient = %v, want nil error", err)
	}
	resp, err := client.Get(untrusted.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("connected to a server not signed by the configured CA — TLS verification is not being enforced")
	}
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	if !errors.As(err, &unknownAuthority) && !errors.As(err, &hostnameErr) {
		t.Logf("rejected with a non-x509 error (still a rejection, so not a failure): %v", err)
	}
}
