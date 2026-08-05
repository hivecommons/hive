package github

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestCA writes a self-signed ECDSA CA cert (matching the proxy's CA
// format) to path and returns its parsed certificate.
func writeTestCA(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Test Hive Proxy CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(path, pemBytes, 0o644); err != nil {
		t.Fatalf("write CA: %v", err)
	}
	// Persist the key next to the cert so signLeaf can issue a chaining leaf.
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal CA key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(path+".key", keyPEM, 0o600); err != nil {
		t.Fatalf("write CA key: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return parsed
}

// withProxyCAPath points proxyCAPath at a temp location and resets the shared
// pool for isolation.
func withProxyCAPath(t *testing.T, path string) {
	t.Helper()
	orig := proxyCAPath
	origShared := sharedProxyTrust
	proxyCAPath = path
	sharedProxyTrust = &proxyTrustPool{}
	t.Cleanup(func() {
		proxyCAPath = orig
		sharedProxyTrust = origShared
	})
}

// TestCertPool_SystemOnlyWhenNoCA verifies that with no proxy CA on disk the
// pool falls back to system roots (non-nil on this platform, or nil meaning
// system roots) and never errors — advisory-mode direct HTTPS must keep working.
func TestCertPool_SystemOnlyWhenNoCA(t *testing.T) {
	dir := t.TempDir()
	withProxyCAPath(t, filepath.Join(dir, "does-not-exist.pem"))

	tp := &proxyTrustPool{}
	pool := tp.certPool()
	// A nil pool is valid (means "system roots"); a non-nil pool must at least
	// carry the system roots. We assert the call is safe and the trust does NOT
	// include our test CA (which we never wrote).
	if pool != nil {
		sysPool, _ := x509.SystemCertPool()
		if sysPool != nil && len(pool.Subjects()) < len(sysPool.Subjects()) { //nolint:staticcheck // Subjects OK for count
			t.Errorf("system-only pool smaller than system roots: got %d want >= %d",
				len(pool.Subjects()), len(sysPool.Subjects()))
		}
	}
}

// TestCertPool_TrustsProxyCA verifies the proxy CA is added to the pool once it
// exists on disk, IN ADDITION to the system roots (combined-bundle safety).
func TestCertPool_TrustsProxyCA(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "proxy-ca.pem")
	ca := writeTestCA(t, caPath)
	withProxyCAPath(t, caPath)

	tp := &proxyTrustPool{}
	pool := tp.certPool()
	if pool == nil {
		t.Fatal("expected non-nil pool once proxy CA exists")
	}

	// Verify a leaf signed by the proxy CA chains to it via the pool.
	leaf := signLeaf(t, ca, caPath)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: time.Now()}); err != nil {
		t.Errorf("leaf signed by proxy CA did not verify against pool: %v", err)
	}
}

// TestCertPool_PicksUpCAWrittenAfterStart is the core fresh-PVC boot race:
// the pool is first built with NO CA (system only), then the proxy writes the
// CA, and a later call (after the reload interval) must trust it — without any
// process restart. This is exactly the ordering the fix guarantees.
func TestCertPool_PicksUpCAWrittenAfterStart(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "proxy-ca.pem")
	withProxyCAPath(t, caPath)

	tp := &proxyTrustPool{}
	// First call: CA absent (fresh boot before proxy generated it).
	_ = tp.certPool()

	// Proxy generates the CA at runtime.
	ca := writeTestCA(t, caPath)

	// Force the reload window to elapse without sleeping.
	tp.mu.Lock()
	tp.lastReload = time.Now().Add(-2 * proxyCAReloadInterval)
	tp.mu.Unlock()

	pool := tp.certPool()
	if pool == nil {
		t.Fatal("expected pool to be built after CA appeared")
	}
	leaf := signLeaf(t, ca, caPath)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: time.Now()}); err != nil {
		t.Errorf("CA written after start not trusted on next mint: %v", err)
	}
}

// TestProxyTrustingHTTPClient_UsesPool verifies the constructed client carries a
// TLS config with our RootCAs pool (not the default nil), so the mint path
// actually applies the proxy-CA trust.
func TestProxyTrustingHTTPClient_UsesPool(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "proxy-ca.pem")
	writeTestCA(t, caPath)
	withProxyCAPath(t, caPath)

	client := proxyTrustingHTTPClient(mintClientTimeout)
	if client.Timeout != mintClientTimeout {
		t.Errorf("timeout = %v want %v", client.Timeout, mintClientTimeout)
	}
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T", client.Transport)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.RootCAs == nil {
		t.Fatal("expected RootCAs pool to be set on the mint client")
	}
}

// signLeaf issues a leaf cert signed by the CA at caPath (using the key
// persisted by writeTestCA) so verification tests exercise a real chain.
func signLeaf(t *testing.T, ca *x509.Certificate, caPath string) *x509.Certificate {
	t.Helper()
	keyPath := caPath + ".key"
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read CA key for leaf signing: %v", err)
	}
	block, _ := pem.Decode(keyPEM)
	caKey, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse CA key: %v", err)
	}
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "api.github.com"},
		DNSNames:     []string{"api.github.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("sign leaf: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf
}

// TestCertPool_CachesWithinReloadInterval verifies the second call within the
// reload window returns the SAME pool instance without re-reading disk, so the
// hot mint path is not stat-ing the CA on every call.
func TestCertPool_CachesWithinReloadInterval(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "proxy-ca.pem")
	writeTestCA(t, caPath)
	withProxyCAPath(t, caPath)

	tp := &proxyTrustPool{}
	first := tp.certPool()
	if first == nil {
		t.Fatal("expected non-nil pool")
	}
	// Immediately call again — inside proxyCAReloadInterval, so the cached
	// pointer must be returned verbatim (no rebuild).
	second := tp.certPool()
	if first != second {
		t.Error("expected the cached pool to be returned within the reload interval")
	}
}

// TestCertPool_NoRebuildWhenFileUnchanged verifies that after the reload window
// elapses but the CA file's mtime and size are unchanged, the established pool
// is reused rather than rebuilt — the mtime/size short-circuit.
func TestCertPool_NoRebuildWhenFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "proxy-ca.pem")
	writeTestCA(t, caPath)
	withProxyCAPath(t, caPath)

	tp := &proxyTrustPool{}
	first := tp.certPool()
	if first == nil {
		t.Fatal("expected non-nil pool")
	}
	// Elapse the reload window without touching the file. The next call re-stats
	// but must find mtime+size unchanged and return the same pool.
	tp.mu.Lock()
	tp.lastReload = time.Now().Add(-2 * proxyCAReloadInterval)
	tp.mu.Unlock()

	second := tp.certPool()
	if first != second {
		t.Error("expected pool reuse when the CA file is unchanged after reload window")
	}
}

// TestCertPool_UnparseableCAKeepsExistingTrust verifies that if the CA file is
// later overwritten with garbage, an established (system+CA) pool is preserved
// rather than downgraded to an empty/partial pool.
func TestCertPool_UnparseableCAKeepsExistingTrust(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "proxy-ca.pem")
	writeTestCA(t, caPath)
	withProxyCAPath(t, caPath)

	tp := &proxyTrustPool{}
	first := tp.certPool()
	if first == nil {
		t.Fatal("expected non-nil pool")
	}

	// Corrupt the CA file with non-PEM bytes and change its size, then elapse the
	// reload window so certPool re-reads it.
	if err := os.WriteFile(caPath, []byte("not a certificate at all"), 0o644); err != nil {
		t.Fatalf("corrupt CA: %v", err)
	}
	tp.mu.Lock()
	tp.lastReload = time.Now().Add(-2 * proxyCAReloadInterval)
	tp.mu.Unlock()

	second := tp.certPool()
	if second != first {
		t.Error("expected the previously-established pool to be preserved on an unparseable CA")
	}
}

// TestCertPool_UnreadableCAKeepsExistingTrust verifies that if os.Stat succeeds
// but os.ReadFile then fails (e.g. the CA path becomes a directory), an
// established pool is preserved rather than downgraded.
func TestCertPool_UnreadableCAKeepsExistingTrust(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "proxy-ca.pem")
	writeTestCA(t, caPath)
	withProxyCAPath(t, caPath)

	tp := &proxyTrustPool{}
	first := tp.certPool()
	if first == nil {
		t.Fatal("expected non-nil pool")
	}

	// Replace the CA file with a directory: os.Stat still succeeds (a dir has a
	// size/mtime) but os.ReadFile fails, exercising the read-error fallback.
	if err := os.Remove(caPath); err != nil {
		t.Fatalf("remove CA file: %v", err)
	}
	if err := os.Mkdir(caPath, 0o755); err != nil {
		t.Fatalf("mkdir at CA path: %v", err)
	}
	tp.mu.Lock()
	tp.lastReload = time.Now().Add(-2 * proxyCAReloadInterval)
	tp.caModTime = time.Time{}
	tp.caSize = -1
	tp.mu.Unlock()

	second := tp.certPool()
	if second != first {
		t.Error("expected established pool to be preserved when the CA becomes unreadable")
	}
}

// TestCertPool_UnparseableCAOnFirstLoadFallsBackToSystem verifies that when the
// very first read encounters a garbage CA file (no prior pool), certPool falls
// back to system roots instead of returning nil unexpectedly or panicking.
func TestCertPool_UnparseableCAOnFirstLoadFallsBackToSystem(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "proxy-ca.pem")
	if err := os.WriteFile(caPath, []byte("garbage, not PEM"), 0o644); err != nil {
		t.Fatalf("write garbage CA: %v", err)
	}
	withProxyCAPath(t, caPath)

	tp := &proxyTrustPool{}
	// Must not panic; a nil pool is valid (means "system roots"), and a non-nil
	// pool must carry at least the system roots.
	pool := tp.certPool()
	if pool != nil {
		if sysPool, _ := x509.SystemCertPool(); sysPool != nil &&
			len(pool.Subjects()) < len(sysPool.Subjects()) { //nolint:staticcheck // Subjects OK for count
			t.Errorf("fallback pool smaller than system roots: got %d want >= %d",
				len(pool.Subjects()), len(sysPool.Subjects()))
		}
	}
}
