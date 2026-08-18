package hub

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	hivegithub "github.com/kubestellar/hive/pkg/github"
)

const (
	visualHiveTokenLeaseDuration = time.Hour
	visualHiveTokenRenewBefore   = 15 * time.Minute
	visualHiveTokenSealInfo      = "hive-wrap-visual-token-v1"
	visualHiveTokenLeaseSchema   = "hive.visual-hive-token-lease.v1"
	visualHiveAppKeyMaxBytes     = 64 << 10
)

var visualHiveTokenPinPath = "/data/saas/visual-hive-token-wrap-pins.json"

// VisualHiveTokenRequest is the non-secret spoke request carried on an
// authenticated heartbeat. The public key belongs to the exact spoke and its
// private half stays on that spoke's persistent volume.
type VisualHiveTokenRequest struct {
	Repository            string    `json:"repository"`
	WrapPublicKey         string    `json:"wrap_public_key"`
	WrapKeyFingerprint    string    `json:"wrap_key_fingerprint"`
	CurrentAppID          int64     `json:"current_app_id,omitempty"`
	CurrentInstallationID int64     `json:"current_installation_id,omitempty"`
	CurrentBindingDigest  string    `json:"current_binding_digest,omitempty"`
	CurrentExpiresAt      time.Time `json:"current_expires_at,omitempty"`
}

// VisualHiveTokenLease is an opaque, repository-bound installation token.
// It contains no plaintext credential; the token is inside Ciphertext and can
// be opened only by the private key on the exact heartbeat-authenticated spoke.
type VisualHiveTokenLease struct {
	SchemaVersion    string    `json:"schema_version"`
	Repository       string    `json:"repository"`
	RepositoryID     int64     `json:"repository_id"`
	AppID            int64     `json:"app_id"`
	InstallationID   int64     `json:"installation_id"`
	PermissionDigest string    `json:"permission_digest"`
	BindingDigest    string    `json:"binding_digest"`
	IssuedAt         time.Time `json:"issued_at"`
	ExpiresAt        time.Time `json:"expires_at"`
	RecipientKey     string    `json:"recipient_key_fingerprint"`
	EphemeralPub     string    `json:"ephemeral_pub"`
	Nonce            string    `json:"nonce"`
	Ciphertext       string    `json:"ciphertext"`
}

// VisualHiveTokenMaterial is returned only inside the spoke process. Token is
// explicitly excluded from JSON so status, audit, or debug serialization cannot
// disclose it accidentally.
type VisualHiveTokenMaterial struct {
	Token     string                        `json:"-"`
	ExpiresAt time.Time                     `json:"expires_at"`
	Identity  hivegithub.AppRuntimeIdentity `json:"identity"`
}

type visualHiveTokenPlaintext struct {
	SchemaVersion string                        `json:"schema_version"`
	Token         string                        `json:"token"`
	ExpiresAt     time.Time                     `json:"expires_at"`
	Identity      hivegithub.AppRuntimeIdentity `json:"identity"`
}

type visualHiveWrapPin struct {
	HiveID      string    `json:"hive_id"`
	Repository  string    `json:"repository"`
	PublicKey   string    `json:"public_key"`
	Fingerprint string    `json:"fingerprint"`
	PinnedAt    time.Time `json:"pinned_at"`
}

type visualHiveWrapPinFile struct {
	SchemaVersion string                       `json:"schema_version"`
	Pins          map[string]visualHiveWrapPin `json:"pins"`
}

// VisualHiveTokenBroker holds the optional App private key on the Hub only.
// Spokes receive one-hour, repository-scoped installation tokens and never the
// App key itself.
type VisualHiveTokenBroker struct {
	appID   int64
	keyPEM  []byte
	logger  *slog.Logger
	now     func() time.Time
	pinPath string

	mu        sync.Mutex
	pins      map[string]visualHiveWrapPin
	mintToken func(context.Context, string) (hivegithub.AppRuntimeIdentity, string, time.Time, error)
}

func newVisualHiveTokenBrokerFromEnvironment(logger *slog.Logger) (*VisualHiveTokenBroker, error) {
	appIDText := strings.TrimSpace(os.Getenv("HIVE_VISUAL_HIVE_GITHUB_APP_ID"))
	keyPath := strings.TrimSpace(os.Getenv("HIVE_VISUAL_HIVE_GITHUB_APP_KEY_FILE"))
	if appIDText == "" && keyPath == "" {
		return nil, nil
	}
	appID, err := strconv.ParseInt(appIDText, 10, 64)
	if err != nil || appID <= 0 {
		return nil, fmt.Errorf("HIVE_VISUAL_HIVE_GITHUB_APP_ID must be a positive integer")
	}
	if keyPath == "" {
		return nil, fmt.Errorf("HIVE_VISUAL_HIVE_GITHUB_APP_KEY_FILE is required when the Visual Hive App is configured")
	}
	// Kubernetes projected Secrets use a symlink at the configured filename.
	// Open that target once, validate the resulting handle, and read it through
	// a hard bound. The open descriptor remains pinned even if the projected
	// Secret rotates its symlink concurrently.
	keyFile, err := os.Open(keyPath)
	if err != nil {
		return nil, fmt.Errorf("open Visual Hive GitHub App key: %w", err)
	}
	keyInfo, statErr := keyFile.Stat()
	if statErr != nil || !keyInfo.Mode().IsRegular() || keyInfo.Size() <= 0 || keyInfo.Size() > visualHiveAppKeyMaxBytes {
		keyFile.Close()
		return nil, fmt.Errorf("Visual Hive GitHub App key must resolve to one bounded ordinary file")
	}
	keyPEM, readErr := io.ReadAll(io.LimitReader(keyFile, visualHiveAppKeyMaxBytes+1))
	closeErr := keyFile.Close()
	if readErr != nil || closeErr != nil || len(keyPEM) == 0 || len(keyPEM) > visualHiveAppKeyMaxBytes {
		return nil, fmt.Errorf("read Visual Hive GitHub App key safely")
	}
	// Parse once at boot without retaining an AppAuth token cache. The private
	// key remains only in this Hub process.
	if _, err := hivegithub.NewAppAuthFromPEM(appID, 1, keyPEM, logger, ""); err != nil {
		return nil, fmt.Errorf("parse Visual Hive GitHub App key: %w", err)
	}
	broker := &VisualHiveTokenBroker{appID: appID, keyPEM: keyPEM, logger: logger, now: func() time.Time { return time.Now().UTC() }, pinPath: visualHiveTokenPinPath, pins: map[string]visualHiveWrapPin{}}
	if err := broker.loadPins(); err != nil {
		return nil, err
	}
	return broker, nil
}

func (broker *VisualHiveTokenBroker) loadPins() error {
	info, err := os.Lstat(broker.pinPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read Visual Hive token recipient pins: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() <= 0 || info.Size() > 1<<20 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("Visual Hive token recipient pin store must be one bounded owner-only ordinary file")
	}
	opened, err := os.Open(broker.pinPath)
	if err != nil {
		return fmt.Errorf("open Visual Hive token recipient pins: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(opened, (1<<20)+1))
	closeErr := opened.Close()
	if readErr != nil || closeErr != nil || len(data) == 0 || len(data) > 1<<20 {
		return fmt.Errorf("read Visual Hive token recipient pins safely")
	}
	after, err := os.Lstat(broker.pinPath)
	if err != nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(info, after) {
		return fmt.Errorf("Visual Hive token recipient pin store changed while it was read")
	}
	var stored visualHiveWrapPinFile
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil || stored.SchemaVersion != "hive.visual-hive-wrap-pins.v1" || stored.Pins == nil || len(stored.Pins) > 10000 {
		return fmt.Errorf("Visual Hive token recipient pin store is malformed")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("Visual Hive token recipient pin store has trailing content")
	}
	for hiveID, pin := range stored.Pins {
		pub, err := parseWrapPublicKey(pin.PublicKey)
		if err != nil || pin.HiveID != hiveID || pin.Fingerprint != wrapKeyFingerprint(pub) || normalizeFullRepository(pin.Repository) == "" {
			return fmt.Errorf("Visual Hive token recipient pin store contains an invalid binding for %q", hiveID)
		}
	}
	broker.pins = stored.Pins
	return nil
}

func (broker *VisualHiveTokenBroker) persistPinsLocked() error {
	data, err := json.MarshalIndent(visualHiveWrapPinFile{SchemaVersion: "hive.visual-hive-wrap-pins.v1", Pins: broker.pins}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(broker.pinPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".visual-hive-wrap-pins-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, broker.pinPath)
}

func (broker *VisualHiveTokenBroker) pinRecipient(hiveID, repository string, request *VisualHiveTokenRequest) (wrapPublicKey, error) {
	pub, err := parseWrapPublicKey(strings.TrimSpace(request.WrapPublicKey))
	if err != nil || request.WrapKeyFingerprint != wrapKeyFingerprint(pub) {
		return wrapPublicKey{}, fmt.Errorf("Visual Hive token recipient key is invalid")
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if existing, ok := broker.pins[hiveID]; ok {
		if !strings.EqualFold(existing.Repository, repository) || existing.PublicKey != request.WrapPublicKey || existing.Fingerprint != request.WrapKeyFingerprint {
			return wrapPublicKey{}, fmt.Errorf("Visual Hive token recipient binding changed; an owner-approved re-pin is required")
		}
		return pub, nil
	}
	broker.pins[hiveID] = visualHiveWrapPin{HiveID: hiveID, Repository: repository, PublicKey: request.WrapPublicKey, Fingerprint: request.WrapKeyFingerprint, PinnedAt: broker.now()}
	if err := broker.persistPinsLocked(); err != nil {
		delete(broker.pins, hiveID)
		return wrapPublicKey{}, fmt.Errorf("persist Visual Hive token recipient pin: %w", err)
	}
	return pub, nil
}

// Issue returns nil when the spoke already has a matching lease with more than
// fifteen minutes remaining. trustedRepository must come from Hub-owned SaaS
// state; the heartbeat body is never allowed to choose a different repository.
func (broker *VisualHiveTokenBroker) Issue(ctx context.Context, hiveID, trustedRepository string, request *VisualHiveTokenRequest) (*VisualHiveTokenLease, error) {
	if broker == nil || request == nil {
		return nil, nil
	}
	repository := normalizeFullRepository(trustedRepository)
	if hiveID == "" || repository == "" || !strings.EqualFold(repository, normalizeFullRepository(request.Repository)) {
		return nil, fmt.Errorf("Visual Hive token request does not match the Hub-owned hive repository")
	}
	recipient, err := broker.pinRecipient(hiveID, repository, request)
	if err != nil {
		return nil, err
	}
	now := broker.now()
	if request.CurrentAppID == broker.appID && request.CurrentInstallationID > 0 && validSHA256Hex(request.CurrentBindingDigest) && request.CurrentExpiresAt.After(now.Add(visualHiveTokenRenewBefore)) {
		return nil, nil
	}
	mintToken := broker.mintToken
	if mintToken == nil {
		mintToken = broker.mint
	}
	identity, token, expiresAt, err := mintToken(ctx, repository)
	if err != nil {
		return nil, err
	}
	if expiresAt.Before(now.Add(visualHiveTokenRenewBefore)) {
		return nil, fmt.Errorf("Visual Hive App minted a token with insufficient lifetime")
	}
	plain, err := json.Marshal(visualHiveTokenPlaintext{SchemaVersion: visualHiveTokenLeaseSchema, Token: token, ExpiresAt: expiresAt.UTC(), Identity: identity})
	if err != nil {
		return nil, err
	}
	lease := VisualHiveTokenLease{
		SchemaVersion: visualHiveTokenLeaseSchema, Repository: identity.Repository, RepositoryID: identity.RepositoryID,
		AppID: identity.AppID, InstallationID: identity.InstallationID, PermissionDigest: identity.PermissionDigest, BindingDigest: identity.BindingDigest,
		IssuedAt: now, ExpiresAt: expiresAt.UTC(), RecipientKey: request.WrapKeyFingerprint,
	}
	if err := sealVisualHiveToken(recipient, hiveID, &lease, plain); err != nil {
		return nil, err
	}
	return &lease, nil
}

func (broker *VisualHiveTokenBroker) mint(ctx context.Context, repository string) (hivegithub.AppRuntimeIdentity, string, time.Time, error) {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok {
		return hivegithub.AppRuntimeIdentity{}, "", time.Time{}, fmt.Errorf("invalid Visual Hive repository")
	}
	discovery, err := hivegithub.NewAppAuthFromPEM(broker.appID, 1, broker.keyPEM, broker.logger, "")
	if err != nil {
		return hivegithub.AppRuntimeIdentity{}, "", time.Time{}, err
	}
	installationID, err := discovery.DiscoverInstallationID(ctx, owner)
	if err != nil {
		return hivegithub.AppRuntimeIdentity{}, "", time.Time{}, fmt.Errorf("discover Visual Hive App installation for %s: %w", owner, err)
	}
	if installationID <= 0 {
		return hivegithub.AppRuntimeIdentity{}, "", time.Time{}, fmt.Errorf("discover Visual Hive App installation for %s: no installation is available", owner)
	}
	auth, err := hivegithub.NewAppAuthFromPEM(broker.appID, installationID, broker.keyPEM, broker.logger, "")
	if err != nil {
		return hivegithub.AppRuntimeIdentity{}, "", time.Time{}, err
	}
	client := hivegithub.NewClientFromApp(auth, owner, []string{name}, broker.logger)
	identity, err := client.ResolveAppRuntimeIdentity(ctx, repository)
	if err != nil {
		return hivegithub.AppRuntimeIdentity{}, "", time.Time{}, err
	}
	if err := identity.RequireVisualHiveExecutionPermissions(); err != nil {
		return hivegithub.AppRuntimeIdentity{}, "", time.Time{}, err
	}
	permissions := &gh.InstallationPermissions{Actions: gh.Ptr("write"), Statuses: gh.Ptr("write"), Metadata: gh.Ptr("read")}
	token, expiresAt, err := auth.MintInstallationToken(ctx, &gh.InstallationTokenOptions{Repositories: []string{name}, Permissions: permissions})
	if err != nil {
		return hivegithub.AppRuntimeIdentity{}, "", time.Time{}, err
	}
	return identity, token, expiresAt, nil
}

func normalizeFullRepository(repository string) string {
	repository = strings.TrimSpace(repository)
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || !isValidName(owner) || !isValidName(name) {
		return ""
	}
	return owner + "/" + name
}

func visualHiveRepositoryForHostedHive(hive *SaaSHive) string {
	if hive == nil || !isPublicForgeHost(hive.GitHubHost) {
		return ""
	}
	primary := strings.TrimSpace(hive.PrimaryRepo)
	if primary == "" && len(hive.Repos) > 0 {
		primary = strings.TrimSpace(hive.Repos[0])
	}
	if !strings.Contains(primary, "/") && strings.TrimSpace(hive.Org) != "" {
		primary = strings.TrimSpace(hive.Org) + "/" + primary
	}
	return normalizeFullRepository(primary)
}

func validSHA256Hex(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func visualHiveTokenAAD(hiveID string, lease *VisualHiveTokenLease, fingerprint string) []byte {
	return []byte(strings.Join([]string{
		visualHiveTokenSealInfo, hiveID, strings.ToLower(lease.Repository), strconv.FormatInt(lease.RepositoryID, 10),
		strconv.FormatInt(lease.AppID, 10), strconv.FormatInt(lease.InstallationID, 10), lease.PermissionDigest,
		lease.BindingDigest, lease.IssuedAt.UTC().Format(time.RFC3339Nano), lease.ExpiresAt.UTC().Format(time.RFC3339Nano), fingerprint,
	}, "\x00"))
}

func visualHiveTokenAEADKey(shared []byte, ephemeral, recipient wrapPublicKey) []byte {
	mac := hmac.New(sha256.New, shared)
	mac.Write([]byte(visualHiveTokenSealInfo))
	mac.Write([]byte{0})
	mac.Write(ephemeral.raw)
	mac.Write([]byte{0})
	mac.Write(recipient.raw)
	return mac.Sum(nil)[:wrapAEADKeyLen]
}

func sealVisualHiveToken(recipient wrapPublicKey, hiveID string, lease *VisualHiveTokenLease, plaintext []byte) error {
	if !recipient.valid() || hiveID == "" || lease == nil {
		return errWrapKeyMalformed
	}
	recipientKey, err := ecdh.X25519().NewPublicKey(recipient.raw)
	if err != nil {
		return errWrapKeyMalformed
	}
	ephPrivate, ephPublic, err := generateWrapKeypair()
	if err != nil {
		return err
	}
	shared, err := ephPrivate.key.ECDH(recipientKey)
	if err != nil {
		return err
	}
	block, err := aes.NewCipher(visualHiveTokenAEADKey(shared, ephPublic, recipient))
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	nonce := make([]byte, wrapNonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	lease.EphemeralPub = ephPublic.Hex()
	lease.Nonce = hex.EncodeToString(nonce)
	lease.Ciphertext = hex.EncodeToString(gcm.Seal(nil, nonce, plaintext, visualHiveTokenAAD(hiveID, lease, wrapKeyFingerprint(recipient))))
	return nil
}

// OpenVisualHiveTokenLease opens and validates a lease against the exact local
// hive/repository identity. It never persists the resulting token.
func OpenVisualHiveTokenLease(hiveID, repository string, lease *VisualHiveTokenLease, now time.Time) (VisualHiveTokenMaterial, error) {
	if lease == nil || lease.SchemaVersion != visualHiveTokenLeaseSchema || !strings.EqualFold(normalizeFullRepository(repository), normalizeFullRepository(lease.Repository)) || lease.ExpiresAt.Before(now.Add(time.Minute)) {
		return VisualHiveTokenMaterial{}, errors.New("Visual Hive token lease binding or lifetime is invalid")
	}
	keys, err := loadSpokeWrapKeys(spokeWrapKeyPath, now)
	if err != nil {
		return VisualHiveTokenMaterial{}, errors.New("Visual Hive token recipient key is unavailable")
	}
	open := func(private wrapPrivateKey) ([]byte, error) {
		if !private.valid() || lease.RecipientKey != wrapKeyFingerprint(private.publicKey()) {
			return nil, errWrapOpenFailed
		}
		ephRaw, err := hex.DecodeString(lease.EphemeralPub)
		if err != nil || len(ephRaw) != wrapKeyLen {
			return nil, errWrapOpenFailed
		}
		eph, err := ecdh.X25519().NewPublicKey(ephRaw)
		if err != nil {
			return nil, errWrapOpenFailed
		}
		nonce, err := hex.DecodeString(lease.Nonce)
		if err != nil || len(nonce) != wrapNonceLen {
			return nil, errWrapOpenFailed
		}
		ciphertext, err := hex.DecodeString(lease.Ciphertext)
		if err != nil {
			return nil, errWrapOpenFailed
		}
		shared, err := private.key.ECDH(eph)
		if err != nil {
			return nil, errWrapOpenFailed
		}
		ownPublic := private.publicKey()
		block, err := aes.NewCipher(visualHiveTokenAEADKey(shared, wrapPublicKey{raw: ephRaw}, ownPublic))
		if err != nil {
			return nil, errWrapOpenFailed
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, errWrapOpenFailed
		}
		return gcm.Open(nil, nonce, ciphertext, visualHiveTokenAAD(hiveID, lease, wrapKeyFingerprint(ownPublic)))
	}
	var plaintext []byte
	if keys.current.valid() {
		plaintext, err = open(keys.current)
	}
	if err != nil && keys.previous.valid() && now.Before(keys.previousExpires) {
		plaintext, err = open(keys.previous)
	}
	if err != nil {
		return VisualHiveTokenMaterial{}, errWrapOpenFailed
	}
	var decoded visualHiveTokenPlaintext
	if err := json.Unmarshal(plaintext, &decoded); err != nil || decoded.SchemaVersion != visualHiveTokenLeaseSchema || strings.TrimSpace(decoded.Token) == "" {
		return VisualHiveTokenMaterial{}, errors.New("Visual Hive token lease plaintext is invalid")
	}
	if !decoded.ExpiresAt.Equal(lease.ExpiresAt) || decoded.Identity.AppID != lease.AppID || decoded.Identity.InstallationID != lease.InstallationID ||
		decoded.Identity.RepositoryID != lease.RepositoryID || !strings.EqualFold(decoded.Identity.Repository, lease.Repository) ||
		decoded.Identity.PermissionDigest != lease.PermissionDigest || decoded.Identity.BindingDigest != lease.BindingDigest {
		return VisualHiveTokenMaterial{}, errors.New("Visual Hive token lease identity does not match its sealed envelope")
	}
	if err := decoded.Identity.RequireVisualHiveExecutionPermissions(); err != nil {
		return VisualHiveTokenMaterial{}, err
	}
	return VisualHiveTokenMaterial{Token: decoded.Token, ExpiresAt: decoded.ExpiresAt, Identity: decoded.Identity}, nil
}

// NewVisualHiveTokenRequest creates or loads the spoke's persistent recipient
// key and returns the public, non-secret heartbeat request.
func NewVisualHiveTokenRequest(repository string, now time.Time) (*VisualHiveTokenRequest, error) {
	repository = normalizeFullRepository(repository)
	if repository == "" {
		return nil, fmt.Errorf("Visual Hive token repository is invalid")
	}
	keys, _, err := ensureSpokeWrapKeys(spokeWrapKeyPath, now)
	if err != nil {
		return nil, err
	}
	public := keys.current.publicKey()
	return &VisualHiveTokenRequest{Repository: repository, WrapPublicKey: public.Hex(), WrapKeyFingerprint: wrapKeyFingerprint(public)}, nil
}
