package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------- extractSNI with real ClientHello ----------

// buildMinimalClientHello constructs the smallest valid TLS 1.2 ClientHello
// carrying a single SNI extension with the given hostname.
func buildMinimalClientHello(host string) []byte {
	hostBytes := []byte(host)

	// SNI extension payload: list-len(2) + type(1) + name-len(2) + name
	sniPayload := make([]byte, 0, 5+len(hostBytes))
	sniListLen := 3 + len(hostBytes)
	sniPayload = append(sniPayload, byte(sniListLen>>8), byte(sniListLen))
	sniPayload = append(sniPayload, 0x00) // host_name type
	sniPayload = append(sniPayload, byte(len(hostBytes)>>8), byte(len(hostBytes)))
	sniPayload = append(sniPayload, hostBytes...)

	// Extensions block: ext-type(2) + ext-len(2) + payload
	extensions := make([]byte, 0, 4+len(sniPayload))
	extensions = append(extensions, 0x00, 0x00) // SNI extension type = 0
	extensions = append(extensions, byte(len(sniPayload)>>8), byte(len(sniPayload)))
	extensions = append(extensions, sniPayload...)

	// ClientHello body: version(2) + random(32) + session-id-len(1) +
	// cipher-suites-len(2) + one suite(2) + compression-len(1) + null(1) +
	// extensions-len(2) + extensions
	chBody := make([]byte, 0, 2+32+1+2+2+1+1+2+len(extensions))
	chBody = append(chBody, 0x03, 0x03) // TLS 1.2
	chBody = append(chBody, make([]byte, 32)...)
	chBody = append(chBody, 0x00)       // session id length = 0
	chBody = append(chBody, 0x00, 0x02) // cipher suites length = 2
	chBody = append(chBody, 0x00, 0x2f) // TLS_RSA_WITH_AES_128_CBC_SHA
	chBody = append(chBody, 0x01, 0x00) // compression methods: 1 method, null
	chBody = append(chBody, byte(len(extensions)>>8), byte(len(extensions)))
	chBody = append(chBody, extensions...)

	// Handshake header: type(1) + length(3)
	handshake := make([]byte, 0, 4+len(chBody))
	handshake = append(handshake, 0x01) // ClientHello
	handshake = append(handshake, byte(len(chBody)>>16), byte(len(chBody)>>8), byte(len(chBody)))
	handshake = append(handshake, chBody...)

	// TLS record header: type(1) + version(2) + length(2)
	record := make([]byte, 0, 5+len(handshake))
	record = append(record, 0x16)       // Handshake
	record = append(record, 0x03, 0x01) // TLS 1.0 record version (common)
	record = append(record, byte(len(handshake)>>8), byte(len(handshake)))
	record = append(record, handshake...)

	return record
}

func TestExtractSNI_RealClientHello(t *testing.T) {
	tests := []struct {
		name string
		host string
	}{
		{"github api", "api.github.com"},
		{"anthropic api", "api.anthropic.com"},
		{"short host", "a.b"},
		{"long subdomain", "very-long-subdomain.internal.example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := buildMinimalClientHello(tt.host)
			got := extractSNI(data)
			if got != tt.host {
				t.Errorf("extractSNI() = %q, want %q", got, tt.host)
			}
		})
	}
}

func TestExtractSNI_TruncatedRecord(t *testing.T) {
	// Build a valid ClientHello then truncate it at various points.
	full := buildMinimalClientHello("api.github.com")

	// Truncate before extensions — should return ""
	// Extensions start after: record-hdr(5) + hs-hdr(4) + version(2) + random(32) +
	// session-id(1) + cipher-suites(4) + compression(2) = 50
	truncated := full[:45]
	if sni := extractSNI(truncated); sni != "" {
		t.Errorf("truncated before extensions should return empty, got %q", sni)
	}
}

func TestExtractSNI_NonSNIExtension(t *testing.T) {
	// Build a ClientHello with a non-SNI extension (e.g., supported_versions = 0x002b)
	// followed by the SNI extension — verify we still find SNI.
	hostBytes := []byte("api.github.com")

	sniPayload := make([]byte, 0, 5+len(hostBytes))
	sniListLen := 3 + len(hostBytes)
	sniPayload = append(sniPayload, byte(sniListLen>>8), byte(sniListLen))
	sniPayload = append(sniPayload, 0x00)
	sniPayload = append(sniPayload, byte(len(hostBytes)>>8), byte(len(hostBytes)))
	sniPayload = append(sniPayload, hostBytes...)

	// Non-SNI extension first (supported_versions, 3 bytes of data)
	extensions := make([]byte, 0)
	extensions = append(extensions, 0x00, 0x2b) // supported_versions
	extensions = append(extensions, 0x00, 0x03) // length 3
	extensions = append(extensions, 0x02, 0x03, 0x04)
	// Then SNI extension
	extensions = append(extensions, 0x00, 0x00)
	extensions = append(extensions, byte(len(sniPayload)>>8), byte(len(sniPayload)))
	extensions = append(extensions, sniPayload...)

	chBody := make([]byte, 0, 2+32+1+2+2+1+1+2+len(extensions))
	chBody = append(chBody, 0x03, 0x03)
	chBody = append(chBody, make([]byte, 32)...)
	chBody = append(chBody, 0x00)
	chBody = append(chBody, 0x00, 0x02)
	chBody = append(chBody, 0x00, 0x2f)
	chBody = append(chBody, 0x01, 0x00)
	chBody = append(chBody, byte(len(extensions)>>8), byte(len(extensions)))
	chBody = append(chBody, extensions...)

	handshake := make([]byte, 0, 4+len(chBody))
	handshake = append(handshake, 0x01)
	handshake = append(handshake, byte(len(chBody)>>16), byte(len(chBody)>>8), byte(len(chBody)))
	handshake = append(handshake, chBody...)

	record := make([]byte, 0, 5+len(handshake))
	record = append(record, 0x16, 0x03, 0x01)
	record = append(record, byte(len(handshake)>>8), byte(len(handshake)))
	record = append(record, handshake...)

	got := extractSNI(record)
	if got != "api.github.com" {
		t.Errorf("extractSNI with non-SNI extension first = %q, want %q", got, "api.github.com")
	}
}

// ---------- generateCA property verification ----------

func TestGenerateCA_Properties(t *testing.T) {
	tlsCert, x509Cert, err := generateCA()
	if err != nil {
		t.Fatalf("generateCA() error: %v", err)
	}

	// Verify it's a CA certificate
	if !x509Cert.IsCA {
		t.Error("generated cert is not a CA")
	}
	if !x509Cert.BasicConstraintsValid {
		t.Error("BasicConstraintsValid is false")
	}
	// Go's x509 uses MaxPathLen=-1 when set to 0 without MaxPathLenZero=true
	if x509Cert.MaxPathLen > 0 {
		t.Errorf("MaxPathLen = %d, want 0 or -1", x509Cert.MaxPathLen)
	}

	// Verify key usage
	if x509Cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("CA cert missing KeyUsageCertSign")
	}
	if x509Cert.KeyUsage&x509.KeyUsageCRLSign == 0 {
		t.Error("CA cert missing KeyUsageCRLSign")
	}

	// Verify the subject
	if x509Cert.Subject.CommonName != "Hive ACMM Proxy CA" {
		t.Errorf("Subject.CommonName = %q, want %q", x509Cert.Subject.CommonName, "Hive ACMM Proxy CA")
	}

	// Verify validity period (~1 year)
	if time.Until(x509Cert.NotAfter) < 364*24*time.Hour {
		t.Errorf("NotAfter too soon: %v", x509Cert.NotAfter)
	}

	// Verify the private key is ECDSA P-256
	key, ok := tlsCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatal("private key is not *ecdsa.PrivateKey")
	}
	if key.Curve != elliptic.P256() {
		t.Error("private key curve is not P-256")
	}

	// Verify self-signed (Issuer == Subject)
	if x509Cert.Issuer.CommonName != x509Cert.Subject.CommonName {
		t.Errorf("not self-signed: issuer=%q subject=%q",
			x509Cert.Issuer.CommonName, x509Cert.Subject.CommonName)
	}
}

// ---------- forgeCert property verification ----------

func TestForgeCert_Properties(t *testing.T) {
	caCert, caX509, err := generateCA()
	if err != nil {
		t.Fatalf("generateCA: %v", err)
	}

	p := &GitHubProxy{
		caCert: caCert,
		caX509: caX509,
	}

	cert, err := p.forgeCert("api.github.com")
	if err != nil {
		t.Fatalf("forgeCert: %v", err)
	}

	// Parse the leaf certificate
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf cert: %v", err)
	}

	// Verify DNS SAN
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != "api.github.com" {
		t.Errorf("DNSNames = %v, want [api.github.com]", leaf.DNSNames)
	}

	// Verify CN
	if leaf.Subject.CommonName != "api.github.com" {
		t.Errorf("CommonName = %q, want %q", leaf.Subject.CommonName, "api.github.com")
	}

	// Verify it's NOT a CA
	if leaf.IsCA {
		t.Error("forged leaf should not be a CA")
	}

	// Verify EKU
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth {
		t.Errorf("ExtKeyUsage = %v, want [ServerAuth]", leaf.ExtKeyUsage)
	}

	// Verify signed by our CA
	pool := x509.NewCertPool()
	pool.AddCert(caX509)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Errorf("leaf cert does not verify against CA: %v", err)
	}

	// Verify the cert chain includes the CA cert
	if len(cert.Certificate) < 2 {
		t.Error("certificate chain should include CA cert")
	}
}

func TestForgeCert_Caching(t *testing.T) {
	caCert, caX509, err := generateCA()
	if err != nil {
		t.Fatalf("generateCA: %v", err)
	}

	p := &GitHubProxy{
		caCert: caCert,
		caX509: caX509,
	}

	cert1, err := p.forgeCert("example.com")
	if err != nil {
		t.Fatalf("first forgeCert: %v", err)
	}

	cert2, err := p.forgeCert("example.com")
	if err != nil {
		t.Fatalf("second forgeCert: %v", err)
	}

	// Same host should return cached cert (same serial number)
	leaf1, _ := x509.ParseCertificate(cert1.Certificate[0])
	leaf2, _ := x509.ParseCertificate(cert2.Certificate[0])
	if leaf1.SerialNumber.Cmp(leaf2.SerialNumber) != 0 {
		t.Error("repeated forgeCert for same host should return cached cert")
	}

	// Different host should return different cert
	cert3, err := p.forgeCert("other.com")
	if err != nil {
		t.Fatalf("forgeCert other host: %v", err)
	}
	leaf3, _ := x509.ParseCertificate(cert3.Certificate[0])
	if leaf1.SerialNumber.Cmp(leaf3.SerialNumber) == 0 {
		t.Error("different hosts should have different serial numbers")
	}
}

func TestForgeCert_InvalidCAKey(t *testing.T) {
	// A proxy whose caCert has no PrivateKey should fail forgeCert.
	p := &GitHubProxy{
		caCert: tls.Certificate{}, // no private key
		caX509: &x509.Certificate{},
	}

	_, err := p.forgeCert("example.com")
	if err == nil {
		t.Error("forgeCert with empty CA key should fail")
	}
}

// ---------- loadOrGenerateCA persistence roundtrip ----------

func TestLoadOrGenerateCA_Roundtrip(t *testing.T) {
	tmpDir := t.TempDir()

	// Override paths for test
	origCert := CACertPath
	origKey := caKeyPath
	CACertPath = filepath.Join(tmpDir, "ca.pem")
	caKeyPath = filepath.Join(tmpDir, "ca-key.pem")
	defer func() {
		CACertPath = origCert
		caKeyPath = origKey
	}()

	logger := testDiscardLogger()

	// First call — generates and persists
	tlsCert1, x509Cert1, err := loadOrGenerateCA(logger)
	if err != nil {
		t.Fatalf("first loadOrGenerateCA: %v", err)
	}
	if !x509Cert1.IsCA {
		t.Error("generated cert is not a CA")
	}

	// Files should exist
	if _, err := os.Stat(CACertPath); err != nil {
		t.Errorf("CA cert not written: %v", err)
	}
	if _, err := os.Stat(caKeyPath); err != nil {
		t.Errorf("CA key not written: %v", err)
	}

	// Second call — should reload from disk (same serial)
	tlsCert2, x509Cert2, err := loadOrGenerateCA(logger)
	if err != nil {
		t.Fatalf("second loadOrGenerateCA: %v", err)
	}
	if x509Cert1.SerialNumber.Cmp(x509Cert2.SerialNumber) != 0 {
		t.Error("second call should reuse persisted CA")
	}
	_ = tlsCert1
	_ = tlsCert2
}

func TestLoadOrGenerateCA_CorruptFile(t *testing.T) {
	tmpDir := t.TempDir()

	origCert := CACertPath
	origKey := caKeyPath
	CACertPath = filepath.Join(tmpDir, "ca.pem")
	caKeyPath = filepath.Join(tmpDir, "ca-key.pem")
	defer func() {
		CACertPath = origCert
		caKeyPath = origKey
	}()

	// Write garbage to the cert file
	os.WriteFile(CACertPath, []byte("not a cert"), 0644)
	os.WriteFile(caKeyPath, []byte("not a key"), 0600)

	logger := testDiscardLogger()

	// Should generate fresh despite corrupt files
	_, x509Cert, err := loadOrGenerateCA(logger)
	if err != nil {
		t.Fatalf("loadOrGenerateCA with corrupt files: %v", err)
	}
	if !x509Cert.IsCA {
		t.Error("regenerated cert is not a CA")
	}
}

func TestLoadOrGenerateCA_ExpiredCA(t *testing.T) {
	tmpDir := t.TempDir()

	origCert := CACertPath
	origKey := caKeyPath
	CACertPath = filepath.Join(tmpDir, "ca.pem")
	caKeyPath = filepath.Join(tmpDir, "ca-key.pem")
	defer func() {
		CACertPath = origCert
		caKeyPath = origKey
	}()

	// Generate a CA that expired in the past
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		NotBefore:             time.Now().Add(-48 * time.Hour),
		NotAfter:              time.Now().Add(-24 * time.Hour), // expired
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	certDER, _ := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, _ := x509.MarshalECPrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	os.WriteFile(CACertPath, certPEM, 0644)
	os.WriteFile(caKeyPath, keyPEM, 0600)

	logger := testDiscardLogger()

	// Should regenerate because the existing CA is expired
	_, x509Cert, err := loadOrGenerateCA(logger)
	if err != nil {
		t.Fatalf("loadOrGenerateCA with expired CA: %v", err)
	}
	if time.Now().After(x509Cert.NotAfter) {
		t.Error("loadOrGenerateCA returned an expired certificate")
	}
}

// ---------- extractAgentName — hive prefix ----------

func TestExtractAgentName_HivePrefix(t *testing.T) {
	tests := []struct {
		name    string
		authHdr string
		want    string
	}{
		{"hive prefix", "hive scanner", "scanner"},
		{"hive prefix with dashes", "hive quality-agent", "quality-agent"},
		{"hive prefix empty name", "hive ", ""},
		{"basic auth", "Basic " + base64.StdEncoding.EncodeToString([]byte("contribute:tok")), "contribute"},
		{"no header", "", ""},
		{"unknown scheme", "Bearer abc123", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "https://api.github.com/repos", nil)
			if tt.authHdr != "" {
				req.Header.Set("Proxy-Authorization", tt.authHdr)
			}
			got := extractAgentName(req)
			if got != tt.want {
				t.Errorf("extractAgentName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------- helpers ----------

func testDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
