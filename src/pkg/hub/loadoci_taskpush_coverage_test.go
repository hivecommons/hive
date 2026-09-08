package hub

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"sync"
	"testing"
)

// ============================================================
// oci_fss.go — loadOCIConfig
// ============================================================

func resetOCIOnce(t *testing.T) {
	t.Helper()
	oldCfg, oldErr := ociCfg, ociCfgErr
	ociCfg = nil
	ociCfgErr = nil
	ociCfgOnce = sync.Once{}
	t.Cleanup(func() {
		ociCfg = oldCfg
		ociCfgErr = oldErr
		ociCfgOnce = sync.Once{}
	})
}

func pkcs8PEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestLoadOCIConfigMissingCreds(t *testing.T) {
	resetOCIOnce(t)
	t.Setenv(envOCITenancy, "")
	t.Setenv(envOCIUser, "")
	t.Setenv(envOCIFingerprint, "")
	t.Setenv(envOCIPrivateKey, "")
	if _, err := loadOCIConfig(); err == nil {
		t.Error("expected error when OCI creds missing")
	}
}

func TestLoadOCIConfigBadPEM(t *testing.T) {
	resetOCIOnce(t)
	t.Setenv(envOCITenancy, "t")
	t.Setenv(envOCIUser, "u")
	t.Setenv(envOCIFingerprint, "f")
	t.Setenv(envOCIPrivateKey, "not a pem")
	if _, err := loadOCIConfig(); err == nil {
		t.Error("expected error decoding bad PEM")
	}
}

func TestLoadOCIConfigSuccess(t *testing.T) {
	resetOCIOnce(t)
	t.Setenv(envOCITenancy, "tenancy")
	t.Setenv(envOCIUser, "user")
	t.Setenv(envOCIFingerprint, "fp")
	t.Setenv(envOCIPrivateKey, pkcs8PEM(t))
	// Default region/compartment applied via getEnvOrDefault.
	t.Setenv(envOCIRegion, "")
	t.Setenv(envOCICompartmentID, "custom-compartment")

	cfg, err := loadOCIConfig()
	if err != nil {
		t.Fatalf("loadOCIConfig: %v", err)
	}
	if cfg.tenancy != "tenancy" || cfg.region != defaultOCIRegion {
		t.Errorf("unexpected cfg: %+v", cfg)
	}
	if cfg.compartmentID != "custom-compartment" {
		t.Errorf("compartment override not applied: %q", cfg.compartmentID)
	}
	if cfg.privateKey == nil {
		t.Error("expected parsed RSA private key")
	}
}
