package hub

// SaaS user persistence and token-at-rest crypto, extracted verbatim from
// saas.go. This file owns the on-disk user store under saasUsersDir (dual-read
// of canonical and legacy filenames, atomic writes) and the AES-GCM
// encrypt/decrypt helpers keyed by the hub's HMAC key file.

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var saasUsersDir = "/data/saas/users"

var hmacKeyPath = "/data/saas/hmac.key"

const hmacKeySize = 32

func loadOrCreateHMACKey() ([]byte, error) {
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(filepath.Dir(hmacKeyPath), 0o755)
	if data, err := os.ReadFile(hmacKeyPath); err == nil && len(data) == hmacKeySize {
		return data, nil
	}
	key := make([]byte, hmacKeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(hmacKeyPath, key, 0o600); err != nil {
		return nil, fmt.Errorf("write HMAC key: %w", err)
	}
	return key, nil
}

func encryptToken(plaintext string) (string, error) {
	key, err := loadOrCreateHMACKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decryptToken(encoded string) (string, error) {
	key, err := loadOrCreateHMACKey()
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("ciphertext too short")
	}
	plaintext, err := gcm.Open(nil, data[:nonceSize], data[nonceSize:], nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// saaSUserFilePaths returns the candidate on-disk paths for an identity, in
// read/try order: the canonical filename first, then the legacy "<login>.json"
// fallback for a bare or github: identity. The caller has already rejected
// path-traversal characters in the raw username.
func saaSUserFilePaths(username string) []string {
	var paths []string
	if stem, err := encodeUserFilename(username); err == nil {
		paths = append(paths, filepath.Join(saasUsersDir, stem+".json"))
	}
	provider, subject, ok := parseCanonical(username)
	if ok && provider == legacyProvider {
		legacy := filepath.Join(saasUsersDir, subject+".json")
		if len(paths) == 0 || paths[0] != legacy {
			paths = append(paths, legacy)
		}
	}
	return paths
}

// readSaaSUserFile reads a user's JSON, trying the canonical filename then the
// legacy fallback (see saaSUserFilePaths). Returns the first file that reads.
func readSaaSUserFile(username string) ([]byte, error) {
	var firstErr error
	for _, p := range saaSUserFilePaths(username) {
		data, err := os.ReadFile(p)
		if err == nil {
			return data, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		firstErr = os.ErrNotExist
	}
	return nil, firstErr
}

// saveSaaSUserPath returns the file path a user record is written to. A GitHub
// (or bare-legacy) primary keeps its legacy "<login>.json" so existing records
// are updated in place with no rename; a non-GitHub primary writes the canonical
// "<provider>.<subject>.json".
func saveSaaSUserPath(u *SaaSUser) (string, error) {
	// The record's primary identity: the explicit CanonicalID when set (a
	// non-GitHub or newly-created user), else GitHubUsername (a bare login for
	// legacy GitHub users). Either resolves through parseCanonical + the shim.
	id := userCanonicalID(u)
	provider, subject, ok := parseCanonical(id)
	if !ok {
		return "", fmt.Errorf("invalid identity for save: %q", id)
	}
	if provider == legacyProvider {
		return filepath.Join(saasUsersDir, subject+".json"), nil
	}
	stem, err := encodeUserFilename(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(saasUsersDir, stem+".json"), nil
}

func loadSaaSUser(username string) *SaaSUser {
	if strings.Contains(username, "..") || strings.Contains(username, "/") || strings.Contains(username, "\\") {
		return nil
	}
	// Dual-read: try the canonical filename ("google.1078.json") then the legacy
	// "<login>.json". No file is rewritten — existing users resolve via legacy.
	data, err := readSaaSUserFile(username)
	if err != nil {
		return nil
	}
	var u SaaSUser
	if json.Unmarshal(data, &u) != nil {
		return nil
	}
	if u.Hives == nil {
		u.Hives = make(map[string]string)
	}
	// Backfill LoginCount for records that predate the login counter (added with
	// the admin engagement card). Those users have a real LastLogin but a zero
	// LoginCount, which renders as the contradictory "0 logins (last <date>)" on
	// the stats card. A user who has logged in at least once is, at minimum, one
	// login — so a present LastLogin with a zero count normalizes to 1. This is a
	// read-time floor only; the real counter keeps incrementing from here on the
	// next OAuth login (handleOAuthCallback), and it never lowers a genuine count.
	if u.LoginCount == 0 && strings.TrimSpace(u.LastLogin) != "" {
		u.LoginCount = 1
	}
	// On-access expiry enforcement (#4150): drop expired grants at READ time so
	// every consumer of a role — auth gates, accessForHive, the heartbeat's
	// authorized-users push — sees the revocation the instant it is due, on the
	// wall clock. Read-time only, never written here; sweepExpiredAccess
	// (access_expiry.go) persists the prune and stamps the timeline event.
	pruneExpiredHiveGrants(&u, time.Now())
	return &u
}

func saveSaaSUser(u *SaaSUser) error {
	if strings.Contains(u.GitHubUsername, "..") || strings.Contains(u.GitHubUsername, "/") || strings.Contains(u.GitHubUsername, "\\") {
		return fmt.Errorf("invalid username for save: %q", u.GitHubUsername)
	}
	// Best-effort: a failed mkdir surfaces via the WriteFile error below.
	_ = os.MkdirAll(saasUsersDir, 0o755)
	data, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	path, err := saveSaaSUserPath(u)
	if err != nil {
		return err
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func ensureSaaSUser(username string) *SaaSUser {
	now := time.Now().UTC().Format(time.RFC3339)
	u := loadSaaSUser(username)
	if u != nil {
		u.LastLogin = now
		if err := saveSaaSUser(u); err != nil {
			slog.Warn("ensureSaaSUser: save failed", "user", username, "error", err)
		}
		return u
	}
	quota := 0
	if isHubAdmin(username) {
		quota = -1
	}
	u = &SaaSUser{
		GitHubUsername: username,
		CreatedAt:      now,
		LastLogin:      now,
		Hives:          map[string]string{},
		SaaSQuota:      quota,
	}
	if err := saveSaaSUser(u); err != nil {
		slog.Warn("ensureSaaSUser: create failed", "user", username, "error", err)
	}
	return u
}

func listAllSaaSUsers() []SaaSUser {
	// Best-effort: a failed mkdir surfaces via the ReadDir error below.
	_ = os.MkdirAll(saasUsersDir, 0o755)
	entries, err := os.ReadDir(saasUsersDir)
	if err != nil {
		return nil
	}
	var users []SaaSUser
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".json")
		// A canonical filename ("google.1078", "github.foo") decodes to its wire
		// id; a legacy filename ("foo") does not — load it as the bare login.
		key := stem
		if id, ok := decodeUserFilename(stem); ok {
			key = id
		}
		u := loadSaaSUser(key)
		if u != nil {
			users = append(users, *u)
		}
	}
	return users
}
