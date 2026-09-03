package hub

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// testECAppKeyPEM generates a PKCS8 ECDSA key. config.AppKeyFingerprint accepts
// it (the public key is derivable), but ghauth.NewAppAuthFromPEM rejects it
// because GitHub App keys must be RSA — exactly the mismatch the bad-key branch
// of lookupGitHubInstallationAccount guards against.
func testECAppKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// publicAppHub builds a hub whose single cluster owns the public GitHub App ID
// with the given PEM stored in a temp key dir (never the live /data store).
func publicAppHub(t *testing.T, pemData string) *HubServer {
	t.Helper()
	withTempAppKeyDir(t)
	if err := storeClusterAppKey("pub-cluster", pemData); err != nil {
		t.Fatalf("store key: %v", err)
	}
	return &HubServer{
		logger: appKeyTestLogger(),
		clusters: map[string]ClusterConfig{
			"pub-cluster": {ID: "pub-cluster", GitHubAppID: config.PublicGitHubAppID, GitHubAppSlug: "pub"},
		},
	}
}

func TestLookupGitHubInstallationAccount_InjectedResolver(t *testing.T) {
	s := &HubServer{logger: appKeyTestLogger()}
	s.githubInstallationAccount = func(_ context.Context, id int64) (string, error) {
		if id != 77 {
			return "", fmt.Errorf("unexpected installation %d", id)
		}
		return "ResolvedOrg", nil
	}
	got, err := s.lookupGitHubInstallationAccount(context.Background(), 77)
	if err != nil || got != "ResolvedOrg" {
		t.Fatalf("lookupGitHubInstallationAccount = %q, %v; want ResolvedOrg, nil", got, err)
	}
}

func TestLookupGitHubInstallationAccount_NoPublicKey(t *testing.T) {
	// No clusters at all: appKeysByAppID is empty, so there is no public App
	// key and the lookup must fail loudly instead of minting with a zero key.
	s := &HubServer{logger: appKeyTestLogger()}
	_, err := s.lookupGitHubInstallationAccount(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "public GitHub App key is not available") {
		t.Fatalf("err = %v, want missing public App key error", err)
	}

	// A cluster for a *different* App ID must not satisfy the public lookup.
	withTempAppKeyDir(t)
	otherPEM := testAppKeyPEM(t)
	if err := storeClusterAppKey("other", otherPEM); err != nil {
		t.Fatalf("store key: %v", err)
	}
	s = &HubServer{
		logger: appKeyTestLogger(),
		clusters: map[string]ClusterConfig{
			"other": {ID: "other", GitHubAppID: config.PublicGitHubAppID + 1},
		},
	}
	_, err = s.lookupGitHubInstallationAccount(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "public GitHub App key is not available") {
		t.Fatalf("err = %v, want missing public App key error for non-public cluster", err)
	}
}

func TestLookupGitHubInstallationAccount_NonRSAKeyRejected(t *testing.T) {
	s := publicAppHub(t, testECAppKeyPEM(t))
	_, err := s.lookupGitHubInstallationAccount(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), "loading public GitHub App key") {
		t.Fatalf("err = %v, want PEM load error for non-RSA key", err)
	}
}

func TestLookupGitHubInstallationAccount_InvalidInstallationID(t *testing.T) {
	// A valid RSA key reaches InstallationAccountLogin, which rejects a
	// non-positive installation ID before any network I/O — hermetic proof the
	// happy path wires the stored key through to the App auth.
	s := publicAppHub(t, testAppKeyPEM(t))
	_, err := s.lookupGitHubInstallationAccount(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "positive") {
		t.Fatalf("err = %v, want positive-installation-ID error", err)
	}
}
