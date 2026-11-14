package github

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// GitHub webhook secret for a self-hosted spoke (hivecommons/hive#18017).
//
// A self-hosted hive verifies X-Hub-Signature-256 on /api/webhook/github
// with a secret the owner also pastes into the GitHub webhook form. It comes
// from GITHUB_WEBHOOK_SECRET when the operator mounts one, otherwise from a
// 0600 file on the data volume that the dashboard's "Generate secret" button
// writes (same pattern as the hub relay key, webhook_relay_key.go). The env
// var always wins: a Secret mount is the deployment's source of truth and the
// dashboard must not pretend it can rotate it.

const defaultWebhookSecretPath = "/data/github-webhook-secret.key"

// Values of WebhookSecretSource: where the active webhook secret comes from.
const (
	WebhookSecretSourceEnv  = "env"
	WebhookSecretSourceFile = "file"
	WebhookSecretSourceNone = ""
)

// webhookSecretBytes is the entropy of a generated secret (256 bits, hex).
const webhookSecretBytes = 32

// ErrWebhookSecretFromEnv is returned by GenerateWebhookSecret when
// GITHUB_WEBHOOK_SECRET is set: the file would never be consulted, so writing
// one would only mislead the owner about which secret is active.
var ErrWebhookSecretFromEnv = errors.New("webhook secret is set by " + WebhookSecretEnvVar + "; rotate it there")

var webhookSecretState = struct {
	mu     sync.Mutex
	path   string
	secret string
	loaded bool
}{path: defaultWebhookSecretPath}

func webhookSecretFileLocked() string {
	st := &webhookSecretState
	if !st.loaded {
		st.loaded = true
		if data, err := os.ReadFile(st.path); err == nil {
			st.secret = strings.TrimSpace(string(data))
		}
	}
	return st.secret
}

// WebhookSecret is the HMAC secret this spoke verifies GitHub-signed
// deliveries with: GITHUB_WEBHOOK_SECRET, else the generated secret file, else
// "".
func WebhookSecret() string {
	if v := strings.TrimSpace(os.Getenv(WebhookSecretEnvVar)); v != "" {
		return v
	}
	webhookSecretState.mu.Lock()
	defer webhookSecretState.mu.Unlock()
	return webhookSecretFileLocked()
}

// WebhookSecretSource reports where WebhookSecret comes from
// (WebhookSecretSource*). Never the secret itself.
func WebhookSecretSource() string {
	if strings.TrimSpace(os.Getenv(WebhookSecretEnvVar)) != "" {
		return WebhookSecretSourceEnv
	}
	if WebhookSecret() != "" {
		return WebhookSecretSourceFile
	}
	return WebhookSecretSourceNone
}

// WebhookSecretPath is the file a generated secret is persisted to.
func WebhookSecretPath() string {
	webhookSecretState.mu.Lock()
	defer webhookSecretState.mu.Unlock()
	return webhookSecretState.path
}

// GenerateWebhookSecret mints a fresh random secret, persists it 0600 to the
// secret file (replacing any previous generated secret) and returns it. This
// is the only time the value leaves the hive; callers show it once. It
// refuses with ErrWebhookSecretFromEnv when the env var is set.
func GenerateWebhookSecret() (string, error) {
	if strings.TrimSpace(os.Getenv(WebhookSecretEnvVar)) != "" {
		return "", ErrWebhookSecretFromEnv
	}
	b := make([]byte, webhookSecretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(b)
	st := &webhookSecretState
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(st.path), 0o700); err != nil {
		return "", err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(secret+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return "", err
	}
	st.secret = secret
	st.loaded = true
	return secret, nil
}

// SetWebhookSecretPathForTest points the secret store at path, forgets any
// secret held in memory, and returns a func that restores the previous state.
func SetWebhookSecretPathForTest(path string) func() {
	st := &webhookSecretState
	st.mu.Lock()
	prevPath, prevSecret, prevLoaded := st.path, st.secret, st.loaded
	st.path, st.secret, st.loaded = path, "", false
	st.mu.Unlock()
	return func() {
		st.mu.Lock()
		st.path, st.secret, st.loaded = prevPath, prevSecret, prevLoaded
		st.mu.Unlock()
	}
}
