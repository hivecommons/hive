package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestLookupGitHubInstallationAccountBadKey covers the key-load error branch.
// An EC key passes the fingerprint check that gates the fleet key set (the
// fingerprinter accepts EC keys) but is not an RSA App key, so building the
// App auth must fail with a "loading public GitHub App key" error rather than
// producing a signer that mints garbage JWTs.
func TestLookupGitHubInstallationAccountBadKey(t *testing.T) {
	withTempAppKeyDir(t)

	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	ecPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	if err := storeClusterAppKey("pub-cluster", ecPEM); err != nil {
		t.Fatalf("storeClusterAppKey: %v", err)
	}

	s := &HubServer{
		logger: appKeyTestLogger(),
		clusters: map[string]ClusterConfig{
			"pub-cluster": {ID: "pub-cluster", GitHubAppID: config.PublicGitHubAppID},
		},
	}
	_, err = s.lookupGitHubInstallationAccount(context.Background(), 42)
	if err == nil {
		t.Fatal("lookup with a non-RSA public App key succeeded, want error")
	}
	if !strings.Contains(err.Error(), "loading public GitHub App key") {
		t.Errorf("error = %q, want the key-load failure named", err)
	}
}
