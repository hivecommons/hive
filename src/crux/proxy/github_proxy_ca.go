package proxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

type cachedCert struct {
	cert      tls.Certificate
	expiresAt time.Time
}

// forgeCert generates a TLS certificate for the given hostname,
// signed by the proxy's CA.
func (p *GitHubProxy) forgeCert(host string) (tls.Certificate, error) {
	p.certMu.RLock()
	if cached, ok := p.certCache[host]; ok && time.Now().Before(cached.expiresAt) {
		p.certMu.RUnlock()
		return cached.cert, nil
	}
	p.certMu.RUnlock()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}

	caKey, ok := p.caCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return tls.Certificate{}, fmt.Errorf("CA key is not ECDSA")
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, p.caX509, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return cert, err
	}
	// Include the CA cert in the chain so clients that load system/extra
	// certs asynchronously (e.g. GitHub Copilot's bundled undici) can
	// verify the forged leaf without having the CA pre-loaded in their
	// trust store.
	cert.Certificate = append(cert.Certificate, p.caX509.Raw)

	p.certMu.Lock()
	if p.certCache == nil {
		p.certCache = make(map[string]cachedCert)
	}
	p.certCache[host] = cachedCert{cert: cert, expiresAt: time.Now().Add(time.Hour)}
	p.certMu.Unlock()

	return cert, nil
}

// generateCA creates a self-signed CA certificate for MITM.
func generateCA() (tls.Certificate, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Hive ACMM Proxy CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	x509Cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	return tlsCert, x509Cert, nil
}

// loadOrGenerateCA reuses an existing CA key pair from disk if present and
// valid, or generates a fresh one and persists both cert and key to the PVC.
// Reusing the CA across pod restarts means the system trust store (populated
// by entrypoint.sh before the Go binary starts) already contains the right
// CA, so clients that load system certs asynchronously (GitHub Copilot's
// bundled undici) can verify forged certificates immediately.
func loadOrGenerateCA(logger *slog.Logger) (tls.Certificate, *x509.Certificate, error) {
	certPEM, certErr := os.ReadFile(CACertPath)
	keyPEM, keyErr := os.ReadFile(caKeyPath)
	if certErr == nil && keyErr == nil {
		tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err == nil {
			block, _ := pem.Decode(certPEM)
			if block != nil {
				x509Cert, err := x509.ParseCertificate(block.Bytes)
				if err == nil && time.Now().Before(x509Cert.NotAfter) {
					logger.Info("reusing persisted MITM CA", "expires", x509Cert.NotAfter)
					return tlsCert, x509Cert, nil
				}
			}
		}
		logger.Warn("persisted CA unusable, generating fresh one", "err", err)
	}

	tlsCert, x509Cert, err := generateCA()
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: x509Cert.Raw})
	if err := os.WriteFile(CACertPath, caPEM, 0644); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("write CA cert to %s: %w", CACertPath, err)
	}

	caKeyDER, err := x509.MarshalECPrivateKey(tlsCert.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("marshal CA key: %w", err)
	}
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: caKeyDER})
	// The parent directory is created with owner-only permissions before the
	// key is written. /data itself is intentionally group-writable for agent
	// workspaces, so 0600 on a file directly under /data is not sufficient:
	// an agent could replace or race the path before the proxy opens it.
	if err := os.MkdirAll(filepath.Dir(caKeyPath), 0700); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("create CA key directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(caKeyPath), 0700); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("restrict CA key directory: %w", err)
	}
	if err := os.WriteFile(caKeyPath, caKeyPEM, 0600); err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("write CA key to %s: %w", caKeyPath, err)
	}

	logger.Info("generated and persisted fresh MITM CA", "certPath", CACertPath, "keyPath", caKeyPath)
	return tlsCert, x509Cert, nil
}
