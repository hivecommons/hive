package hub

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	hivegithub "github.com/kubestellar/hive/pkg/github"
)

func testVisualHiveIdentity(repository string) hivegithub.AppRuntimeIdentity {
	return hivegithub.AppRuntimeIdentity{
		AppID: 71, InstallationID: 72, InstallationOwner: strings.Split(repository, "/")[0],
		BotID: 73, BotLogin: "visual-hive[bot]", BotType: "Bot", Repository: repository, RepositoryID: 74,
		Permissions:      map[string]string{"actions": "write", "statuses": "write", "metadata": "read"},
		PermissionDigest: strings.Repeat("a", 64), BindingDigest: strings.Repeat("b", 64),
	}
}

func withVisualHiveWrapPath(t *testing.T) string {
	t.Helper()
	prior := spokeWrapKeyPath
	path := filepath.Join(t.TempDir(), "hive-wrap-key")
	spokeWrapKeyPath = path
	t.Cleanup(func() { spokeWrapKeyPath = prior })
	return path
}

func TestVisualHiveTokenLeaseIsSealedAndBound(t *testing.T) {
	withVisualHiveWrapPath(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	request, err := NewVisualHiveTokenRequest("owner/repo", now)
	if err != nil {
		t.Fatal(err)
	}
	identity := testVisualHiveIdentity("owner/repo")
	plaintext, err := json.Marshal(visualHiveTokenPlaintext{SchemaVersion: visualHiveTokenLeaseSchema, Token: "secret-installation-token", ExpiresAt: now.Add(time.Hour), Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := parseWrapPublicKey(request.WrapPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	lease := VisualHiveTokenLease{
		SchemaVersion: visualHiveTokenLeaseSchema, Repository: identity.Repository, RepositoryID: identity.RepositoryID,
		AppID: identity.AppID, InstallationID: identity.InstallationID, PermissionDigest: identity.PermissionDigest,
		BindingDigest: identity.BindingDigest, IssuedAt: now, ExpiresAt: now.Add(time.Hour), RecipientKey: request.WrapKeyFingerprint,
	}
	if err := sealVisualHiveToken(pub, "hosted-one", &lease, plaintext); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "secret-installation-token") {
		t.Fatal("Visual Hive token appeared in the heartbeat wire envelope")
	}
	material, err := OpenVisualHiveTokenLease("hosted-one", "owner/repo", &lease, now)
	if err != nil {
		t.Fatal(err)
	}
	if material.Token != "secret-installation-token" || material.Identity.BindingDigest != identity.BindingDigest {
		t.Fatalf("opened material mismatch: %+v", material.Identity)
	}

	tampered := lease
	tampered.RepositoryID++
	if _, err := OpenVisualHiveTokenLease("hosted-one", "owner/repo", &tampered, now); err == nil {
		t.Fatal("tampered repository binding opened")
	}
	if _, err := OpenVisualHiveTokenLease("another-hive", "owner/repo", &lease, now); err == nil {
		t.Fatal("cross-hive lease opened")
	}
	if _, err := OpenVisualHiveTokenLease("hosted-one", "owner/other", &lease, now); err == nil {
		t.Fatal("cross-repository lease opened")
	}
}

func TestVisualHiveTokenBrokerPinsRecipientAndRenewsOnlyNearExpiry(t *testing.T) {
	withVisualHiveWrapPath(t)
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	request, err := NewVisualHiveTokenRequest("owner/repo", now)
	if err != nil {
		t.Fatal(err)
	}
	broker := &VisualHiveTokenBroker{
		appID: 71, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: func() time.Time { return now },
		pinPath: filepath.Join(t.TempDir(), "pins.json"), pins: map[string]visualHiveWrapPin{},
	}
	mintCalls := 0
	broker.mintToken = func(context.Context, string) (hivegithub.AppRuntimeIdentity, string, time.Time, error) {
		mintCalls++
		return testVisualHiveIdentity("owner/repo"), "token", now.Add(time.Hour), nil
	}
	lease, err := broker.Issue(context.Background(), "hosted-one", "owner/repo", request)
	if err != nil || lease == nil || mintCalls != 1 {
		t.Fatalf("first issue: lease=%v calls=%d err=%v", lease != nil, mintCalls, err)
	}
	pins, err := os.ReadFile(broker.pinPath)
	if err != nil || !strings.Contains(string(pins), request.WrapKeyFingerprint) {
		t.Fatalf("recipient pin was not persisted: %v", err)
	}
	if info, err := os.Lstat(broker.pinPath); err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("recipient pin store permissions are not owner-only: info=%v err=%v", info, err)
	}
	request.CurrentAppID = lease.AppID
	request.CurrentInstallationID = lease.InstallationID
	request.CurrentBindingDigest = lease.BindingDigest
	request.CurrentExpiresAt = now.Add(16 * time.Minute)
	lease, err = broker.Issue(context.Background(), "hosted-one", "owner/repo", request)
	if err != nil || lease != nil || mintCalls != 1 {
		t.Fatalf("fresh lease was unnecessarily renewed: lease=%v calls=%d err=%v", lease != nil, mintCalls, err)
	}
	request.CurrentExpiresAt = now.Add(14 * time.Minute)
	lease, err = broker.Issue(context.Background(), "hosted-one", "owner/repo", request)
	if err != nil || lease == nil || mintCalls != 2 {
		t.Fatalf("near-expiry lease was not renewed: lease=%v calls=%d err=%v", lease != nil, mintCalls, err)
	}

	_, otherPublic, err := generateWrapKeypair()
	if err != nil {
		t.Fatal(err)
	}
	request.WrapPublicKey = otherPublic.Hex()
	request.WrapKeyFingerprint = wrapKeyFingerprint(otherPublic)
	request.CurrentExpiresAt = now.Add(30 * time.Minute)
	if _, err := broker.Issue(context.Background(), "hosted-one", "owner/repo", request); err == nil || !strings.Contains(err.Error(), "re-pin") {
		t.Fatalf("recipient key drift on a fresh lease did not fail closed: %v", err)
	}
}

func TestVisualHiveTokenBrokerRejectsUnsafePinStore(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "pins.json")
	valid := `{"schema_version":"hive.visual-hive-wrap-pins.v1","pins":{}}`
	if err := os.WriteFile(path, []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	broker := &VisualHiveTokenBroker{pinPath: path, pins: map[string]visualHiveWrapPin{}}
	if err := broker.loadPins(); err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatalf("broad pin-store permissions were accepted: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(valid+` {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := broker.loadPins(); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing pin-store data was accepted: %v", err)
	}
}

func TestVisualHiveTokenBrokerLoadsBoundedProjectedSecretKey(t *testing.T) {
	directory := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "..data-key.pem")
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(target, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(directory, "app-key.pem")
	if err := os.Symlink(target, keyPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Setenv("HIVE_VISUAL_HIVE_GITHUB_APP_ID", "71")
	t.Setenv("HIVE_VISUAL_HIVE_GITHUB_APP_KEY_FILE", keyPath)
	priorPins := visualHiveTokenPinPath
	visualHiveTokenPinPath = filepath.Join(directory, "pins.json")
	t.Cleanup(func() { visualHiveTokenPinPath = priorPins })
	broker, err := newVisualHiveTokenBrokerFromEnvironment(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || broker == nil || broker.appID != 71 {
		t.Fatalf("projected-secret key was not loaded: broker=%v err=%v", broker != nil, err)
	}

	oversized := filepath.Join(directory, "oversized.pem")
	if err := os.WriteFile(oversized, make([]byte, visualHiveAppKeyMaxBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVE_VISUAL_HIVE_GITHUB_APP_KEY_FILE", oversized)
	if _, err := newVisualHiveTokenBrokerFromEnvironment(slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || !strings.Contains(err.Error(), "bounded") {
		t.Fatalf("oversized App key was accepted: %v", err)
	}
}
