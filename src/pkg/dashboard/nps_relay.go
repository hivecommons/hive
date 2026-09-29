package dashboard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/hivecommons/hive/pkg/config"
)

// NPS relay client, spoke side (issue #9619).
//
// A standalone hive (no hub link) that has opted in to NPS forwards responses
// to the hivecommons NPS relay. There is no operator-issued credential: on
// first use the spoke generates an Ed25519 keypair and a random install id,
// persists both in a 0600 file on the data volume, and registers the public
// key with the relay (POST <relay>/register). Every request after that is
// signed, so the relay can tell one install's submissions apart from
// another's and can reject replays. A self-registered key proves continuity
// of an install, not that it is a real or trusted hive; the hub labels these
// responses "unverified install" accordingly.
//
// Wire contract (mirrored by hivecommons/docs netlify/nps-relay/relay.ts):
//
//	X-Hive-Install-Id  the install id (lowercase UUID)
//	X-Hive-Timestamp   unix seconds
//	X-Hive-Nonce       npsRelayNonceBytes random bytes, lowercase hex
//	X-Hive-Signature   base64 Ed25519 signature over npsRelaySigningInput
//
// The signed input binds a purpose ("register" or "submit"), the install id,
// timestamp, nonce and the SHA-256 of the exact body bytes. When the relay
// answers a submission with code npsRelayUnknownInstallCode (for example after
// its store was reset), the spoke re-registers once and retries.
//
// The private key never leaves the identity file: it is not logged, not part
// of any error, and not sent anywhere.

const (
	// npsRelayIdentityFileName is the identity file under the PVC secrets dir.
	npsRelayIdentityFileName = "nps_relay_identity.json"
	// npsRelayIdentityFileMode keeps the private key owner-read/write only.
	npsRelayIdentityFileMode = 0o600
	// npsRelayIdentityDirMode matches the sibling key stores' secrets dir.
	npsRelayIdentityDirMode = 0o700
	// npsRelayMaxIdentityFileBytes bounds the identity file read; a valid one
	// is a few hundred bytes.
	npsRelayMaxIdentityFileBytes = 4096

	// npsRelayRegisterPath is appended to the relay URL for registration.
	npsRelayRegisterPath = "/register"

	// npsRelaySignatureVersion prefixes the signed input so a future format
	// can never be confused with this one.
	npsRelaySignatureVersion = "hive-nps-relay-v1"
	// npsRelayPurposeRegister / npsRelayPurposeSubmit bind a signature to the
	// endpoint it was made for.
	npsRelayPurposeRegister = "register"
	npsRelayPurposeSubmit   = "submit"
	// npsRelayNonceBytes is the random nonce length before hex encoding.
	npsRelayNonceBytes = 16

	// Signed-request headers.
	npsRelayHeaderInstallID = "X-Hive-Install-Id"
	npsRelayHeaderTimestamp = "X-Hive-Timestamp"
	npsRelayHeaderNonce     = "X-Hive-Nonce"
	npsRelayHeaderSignature = "X-Hive-Signature"

	// npsRelayUnknownInstallCode is the relay's error code for a submission
	// from an install id it has no key for.
	npsRelayUnknownInstallCode = "unknown_install"
	// npsRelayMaxReRegistrations bounds re-registration per submission.
	npsRelayMaxReRegistrations = 1
)

// npsRelayInstallIDPattern is the install id shape the relay accepts.
var npsRelayInstallIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// npsRelayIdentityPath is a package var (not a const) so tests can point it
// at a temp dir. The production value never changes at runtime.
var npsRelayIdentityPath = filepath.Join(config.WritableSecretsDir, npsRelayIdentityFileName)

// npsRelayIdentityMu serializes identity creation, registration and signing
// so two concurrent first submissions cannot mint two identities.
var npsRelayIdentityMu sync.Mutex

// errNPSRelayIdentityCorrupt never carries file content (it holds a key).
var errNPSRelayIdentityCorrupt = errors.New("nps relay identity file is unreadable")

// npsRelayIdentity is this install's relay identity as persisted on disk.
type npsRelayIdentity struct {
	InstallID string `json:"install_id"`
	// PrivateKey is the base64 Ed25519 seed. A secret.
	PrivateKey string `json:"private_key"`
	// RegisteredWith is the relay URL the public key was last registered
	// with; empty (or a different URL) means register before submitting.
	RegisteredWith string `json:"registered_with,omitempty"`

	key ed25519.PrivateKey
}

// publicKey returns the base64 raw Ed25519 public key.
func (id *npsRelayIdentity) publicKey() string {
	pub, ok := id.key.Public().(ed25519.PublicKey)
	if !ok {
		return ""
	}
	return base64.StdEncoding.EncodeToString(pub)
}

// newNPSRelayIdentity mints a fresh install id and keypair.
func newNPSRelayIdentity() (*npsRelayIdentity, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate nps relay key: %w", err)
	}
	return &npsRelayIdentity{
		InstallID:  uuid.NewString(),
		PrivateKey: base64.StdEncoding.EncodeToString(priv.Seed()),
		key:        priv,
	}, nil
}

// parseNPSRelayIdentity decodes and validates a persisted identity.
func parseNPSRelayIdentity(raw []byte) (*npsRelayIdentity, error) {
	var id npsRelayIdentity
	if err := json.Unmarshal(raw, &id); err != nil {
		return nil, errNPSRelayIdentityCorrupt
	}
	seed, err := base64.StdEncoding.DecodeString(id.PrivateKey)
	if err != nil || len(seed) != ed25519.SeedSize || !npsRelayInstallIDPattern.MatchString(id.InstallID) {
		return nil, errNPSRelayIdentityCorrupt
	}
	id.key = ed25519.NewKeyFromSeed(seed)
	return &id, nil
}

// loadOrCreateNPSRelayIdentity returns the persisted identity, creating and
// saving a new one when there is none. An unreadable file is replaced with a
// fresh identity: the old install id is simply orphaned on the relay, and the
// relay never lets the new key take it over.
func loadOrCreateNPSRelayIdentity(path string) (*npsRelayIdentity, error) {
	raw, err := npsReadCapped(path, npsRelayMaxIdentityFileBytes)
	switch {
	case err == nil:
		if id, perr := parseNPSRelayIdentity(raw); perr == nil {
			return id, nil
		}
	case errors.Is(err, os.ErrNotExist), errors.Is(err, errNPSRelayIdentityCorrupt):
		// No identity yet, or an oversized one: mint a fresh identity below.
	default:
		return nil, fmt.Errorf("read nps relay identity: %w", err)
	}
	id, err := newNPSRelayIdentity()
	if err != nil {
		return nil, err
	}
	if err := id.save(path); err != nil {
		return nil, err
	}
	return id, nil
}

// npsReadCapped reads at most maxBytes+1 bytes of path, failing when larger.
func npsReadCapped(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, errNPSRelayIdentityCorrupt
	}
	return raw, nil
}

// save writes the identity atomically with 0600 permissions.
func (id *npsRelayIdentity) save(path string) error {
	data, err := json.Marshal(id)
	if err != nil {
		return errors.New("encode nps relay identity")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, npsRelayIdentityDirMode); err != nil {
		return fmt.Errorf("create nps relay identity dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+npsRelayIdentityFileName+"-*")
	if err != nil {
		return fmt.Errorf("create nps relay identity: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(npsRelayIdentityFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod nps relay identity: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write nps relay identity: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close nps relay identity: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("install nps relay identity: %w", err)
	}
	return nil
}

// npsRelaySigningInput is the exact byte string a relay request's signature
// covers. The relay rebuilds it from the headers and the body it received.
func npsRelaySigningInput(purpose, installID, timestamp, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		npsRelaySignatureVersion, purpose, installID, timestamp, nonce, hex.EncodeToString(sum[:]),
	}, "\n"))
}

// sign sets the signed-request headers on req for body.
func (id *npsRelayIdentity) sign(req *http.Request, purpose string, body []byte, now time.Time) error {
	nonceRaw := make([]byte, npsRelayNonceBytes)
	if _, err := rand.Read(nonceRaw); err != nil {
		return fmt.Errorf("nps relay nonce: %w", err)
	}
	nonce := hex.EncodeToString(nonceRaw)
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := ed25519.Sign(id.key, npsRelaySigningInput(purpose, id.InstallID, ts, nonce, body))
	req.Header.Set(npsRelayHeaderInstallID, id.InstallID)
	req.Header.Set(npsRelayHeaderTimestamp, ts)
	req.Header.Set(npsRelayHeaderNonce, nonce)
	req.Header.Set(npsRelayHeaderSignature, base64.StdEncoding.EncodeToString(sig))
	return nil
}

// npsRelayRegisterRequest is the registration body.
type npsRelayRegisterRequest struct {
	InstallID   string `json:"install_id"`
	PublicKey   string `json:"public_key"`
	HiveVersion string `json:"hive_version"`
}

// npsRelayErrorReply is the relay's error body.
type npsRelayErrorReply struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// npsRelayPost sends one signed POST and returns the status and (capped)
// reply body.
func npsRelayPost(ctx context.Context, client *http.Client, endpoint, purpose string, id *npsRelayIdentity, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := id.sign(req, purpose, body, time.Now()); err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("post: %w", err)
	}
	defer closeHTTPBody(resp.Body)
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, npsMaxHubResponseBytes))
	return resp.StatusCode, reply, nil
}

// npsRelayRegister registers id's public key with the relay. The relay
// answers 201 for a new registration and 200 when this exact (install id,
// key) pair is already registered; anything else is an error.
func npsRelayRegister(ctx context.Context, client *http.Client, relayURL string, id *npsRelayIdentity) (int, error) {
	body, err := json.Marshal(npsRelayRegisterRequest{
		InstallID:   id.InstallID,
		PublicKey:   id.publicKey(),
		HiveVersion: versionShort,
	})
	if err != nil {
		return 0, fmt.Errorf("marshal registration: %w", err)
	}
	status, _, err := npsRelayPost(ctx, client, relayURL+npsRelayRegisterPath, npsRelayPurposeRegister, id, body)
	if err != nil {
		return status, fmt.Errorf("register: %w", err)
	}
	if !npsIsSuccess(status) {
		return status, fmt.Errorf("relay refused registration (%d)", status)
	}
	return status, nil
}

// npsRelayUnknownInstall reports whether a relay reply says it has no key for
// this install.
func npsRelayUnknownInstall(status int, reply []byte) bool {
	if status != http.StatusUnauthorized {
		return false
	}
	var e npsRelayErrorReply
	return json.Unmarshal(reply, &e) == nil && e.Code == npsRelayUnknownInstallCode
}

// npsRelayForward delivers one NPS payload to the relay: it loads (or
// creates) this install's identity, registers it when the relay has not seen
// it yet, and POSTs the signed payload. If the relay reports the install as
// unknown, it re-registers once and retries. Returns the relay status (0 when
// no response) and an error for anything but 2xx.
func npsRelayForward(ctx context.Context, client *http.Client, relayURL string, payload []byte) (int, error) {
	npsRelayIdentityMu.Lock()
	defer npsRelayIdentityMu.Unlock()
	id, err := loadOrCreateNPSRelayIdentity(npsRelayIdentityPath)
	if err != nil {
		return 0, err
	}
	for attempt := 0; ; attempt++ {
		if id.RegisteredWith != relayURL {
			if status, err := npsRelayRegister(ctx, client, relayURL, id); err != nil {
				return status, err
			}
			id.RegisteredWith = relayURL
			if err := id.save(npsRelayIdentityPath); err != nil {
				return 0, err
			}
		}
		status, reply, err := npsRelayPost(ctx, client, relayURL, npsRelayPurposeSubmit, id, payload)
		if err != nil {
			return status, err
		}
		if npsIsSuccess(status) {
			return status, nil
		}
		if npsRelayUnknownInstall(status, reply) && attempt < npsRelayMaxReRegistrations {
			// The relay lost this install's key (e.g. its store was reset).
			id.RegisteredWith = ""
			continue
		}
		return status, fmt.Errorf("upstream answered %d", status)
	}
}
