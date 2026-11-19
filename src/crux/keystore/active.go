package keystore

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

// Environment variables that select and key the active provider.
const (
	// EnvProviderName selects the provider: ProviderEnv (default), ProviderNone
	// or any name registered through Register.
	EnvProviderName = "HIVE_KEYSTORE_PROVIDER"
	// EnvSecretsKey is operator-chosen key material for the env provider.
	EnvSecretsKey = "HIVE_SECRETS_KEY"
	// EnvSecretsKeyFile names a file holding the key material instead.
	EnvSecretsKeyFile = "HIVE_SECRETS_KEY_FILE"
	// EnvSecretsKeyPrevious holds retired key material, accepted for decrypt
	// only so sealed values can be re-sealed under the current key.
	EnvSecretsKeyPrevious = "HIVE_SECRETS_KEY_PREVIOUS"
	// EnvHeartbeatKey is the hub-provisioned per-hive key the env provider
	// falls back to when no HIVE_SECRETS_KEY is set. It is only ever used
	// through HKDF; see envkey.go.
	EnvHeartbeatKey = "HIVE_HEARTBEAT_KEY"
	// EnvHeartbeatKeyPrevious is the previous heartbeat key, decrypt-only,
	// for a hub master-key rotation.
	EnvHeartbeatKeyPrevious = "HIVE_HEARTBEAT_KEY_PREVIOUS"

	// ProviderNone is the explicit legacy plaintext mode.
	ProviderNone = "none"
)

// Factory builds a registered provider from the environment. It is called at
// most once per process, from Init; an error refuses boot.
type Factory func(ctx context.Context, getenv func(string) string) (Provider, error)

// Status is what Init reports: enough for the boot log line and the
// dashboard, nothing secret.
type Status struct {
	// Provider is the active provider name, or ProviderNone.
	Provider string
	// KeyID is the current key generation ("" when no provider is active).
	KeyID string
	// Generations lists every generation the provider can open, current
	// first, when the provider exposes that.
	Generations []string
	// Source says where the env provider's key material came from
	// ("env:HIVE_SECRETS_KEY", "file:/run/secrets/key", "env:HIVE_HEARTBEAT_KEY").
	Source string
	// Legacy is true when secrets are stored in plaintext: either
	// HIVE_KEYSTORE_PROVIDER=none or the env provider found no key material.
	Legacy bool
	// Note explains a Legacy status in operator terms.
	Note string
}

// Active reports whether a provider encrypts new values.
func (s Status) Active() bool { return !s.Legacy && s.Provider != "" && s.Provider != ProviderNone }

var (
	registryMu sync.Mutex
	registry   = map[string]Factory{}

	activeMu   sync.RWMutex
	active     Provider
	activeSet  bool
	initStatus Status
	initErr    error
	initOnce   sync.Once
	logOnce    sync.Once
)

// Register makes a provider selectable by name through HIVE_KEYSTORE_PROVIDER.
// Registering ProviderEnv or ProviderNone, or a name twice, panics: both are
// wiring mistakes that must fail the build's first test, not a boot.
func Register(name string, f Factory) {
	name = strings.TrimSpace(name)
	if name == "" || name == ProviderEnv || name == ProviderNone || f == nil {
		panic(fmt.Sprintf("keystore: cannot register provider %q", name))
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("keystore: provider %q registered twice", name))
	}
	registry[name] = f
}

// Registered lists the registered provider names, sorted, plus the built-in
// ones. For error messages.
func Registered() []string {
	registryMu.Lock()
	defer registryMu.Unlock()
	names := []string{ProviderEnv, ProviderNone}
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Init selects and builds the provider from the environment exactly once,
// installs it as the active provider, and returns its Status. getenv and
// readFile default to the process environment and filesystem. The first call
// wins; later calls return the same result. A provider error (an unregistered
// name, unusable key material, a factory that cannot reach its backend) is
// returned and NO provider is installed: callers refuse to boot, because
// falling back to plaintext would silently defeat the operator's choice.
func Init(ctx context.Context, getenv func(string) string, readFile func(string) ([]byte, error)) (Status, error) {
	initOnce.Do(func() {
		st, p, err := build(ctx, getenv, readFile)
		activeMu.Lock()
		defer activeMu.Unlock()
		initStatus, initErr = st, err
		if err == nil && !activeSet {
			active, activeSet = p, true
		}
	})
	activeMu.RLock()
	defer activeMu.RUnlock()
	return initStatus, initErr
}

// build is Init without the once: pure, so tests can exercise every branch.
func build(ctx context.Context, getenv func(string) string, readFile func(string) ([]byte, error)) (Status, Provider, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	if readFile == nil {
		readFile = os.ReadFile
	}
	name := strings.TrimSpace(getenv(EnvProviderName))
	if name == "" {
		name = ProviderEnv
	}
	switch name {
	case ProviderNone:
		return Status{Provider: ProviderNone, Legacy: true, Note: EnvProviderName + "=none: secrets are stored in plaintext"}, nil, nil
	case ProviderEnv:
		return buildEnv(getenv, readFile)
	}
	registryMu.Lock()
	f, ok := registry[name]
	registryMu.Unlock()
	if !ok {
		return Status{}, nil, fmt.Errorf("keystore: %s=%q is not a provider this binary knows (have %s)", EnvProviderName, name, strings.Join(Registered(), ", "))
	}
	p, err := f(ctx, getenv)
	if err != nil {
		return Status{}, nil, fmt.Errorf("keystore: provider %q: %w", name, err)
	}
	if p == nil {
		return Status{}, nil, fmt.Errorf("keystore: provider %q returned no provider", name)
	}
	return Status{Provider: p.Name(), KeyID: p.KeyID()}, p, nil
}

// buildEnv resolves the env provider's key material: HIVE_SECRETS_KEY_FILE,
// then HIVE_SECRETS_KEY, then HIVE_HEARTBEAT_KEY. Every other non-empty
// candidate (and the *_PREVIOUS variables) is accepted for decrypt only, so a
// hive that moves from the heartbeat default to an explicit key, or absorbs a
// hub rotation, can still open what it wrote before.
func buildEnv(getenv func(string) string, readFile func(string) ([]byte, error)) (Status, Provider, error) {
	type candidate struct {
		material string
		source   string
	}
	var cands []candidate
	if path := strings.TrimSpace(getenv(EnvSecretsKeyFile)); path != "" {
		b, err := readFile(path)
		if err != nil {
			return Status{}, nil, fmt.Errorf("keystore: %s: %w", EnvSecretsKeyFile, err)
		}
		cands = append(cands, candidate{strings.TrimSpace(string(b)), "file:" + path})
	}
	for _, name := range []string{EnvSecretsKey, EnvHeartbeatKey, EnvSecretsKeyPrevious, EnvHeartbeatKeyPrevious} {
		cands = append(cands, candidate{strings.TrimSpace(getenv(name)), "env:" + name})
	}
	var current *candidate
	var previous [][]byte
	for i := range cands {
		c := cands[i]
		if c.material == "" {
			continue
		}
		if current == nil {
			current = &c
			continue
		}
		previous = append(previous, []byte(c.material))
	}
	if current == nil {
		return Status{
			Provider: ProviderNone,
			Legacy:   true,
			Note: "no key material: set " + EnvSecretsKey + " (or " + EnvSecretsKeyFile + ") to encrypt secrets at rest; " +
				"hub-hosted spokes receive " + EnvHeartbeatKey + " automatically. Secrets are stored in plaintext until then.",
		}, nil, nil
	}
	p, err := NewEnvKeyProvider([]byte(current.material), previous...)
	if err != nil {
		return Status{}, nil, fmt.Errorf("keystore: %s: %w", current.source, err)
	}
	return Status{Provider: ProviderEnv, KeyID: p.KeyID(), Generations: p.Generations(), Source: current.source}, p, nil
}

// Active returns the provider new values are sealed with, or nil when secrets
// are stored in plaintext. A process that never called Init gets one built
// from the real environment on first use, so the configuration loader and
// the CLIs open envelopes without boot wiring; a build error there leaves no
// provider active and is reported by InitError.
//
// Inside a test binary the lazy path is OFF: a provider is active only when
// the test installed one with SetActive or Init. Hive's test suites set
// HIVE_HEARTBEAT_KEY and friends freely (the hub credential tests, say), and
// under `go test -shuffle` whichever test ran first would otherwise decide
// whether every later secret-file write in the process is sealed, making
// tests that read a key file back fail depending on order.
func Active() Provider {
	activeMu.RLock()
	set := activeSet
	activeMu.RUnlock()
	if !set && !testing.Testing() {
		_, _ = Init(context.Background(), nil, nil)
	}
	activeMu.RLock()
	defer activeMu.RUnlock()
	return active
}

// InitError returns the error Init (or the lazy Active) hit, if any.
func InitError() error {
	activeMu.RLock()
	defer activeMu.RUnlock()
	return initErr
}

// SetActive installs p (nil for plaintext) as the active provider, bypassing
// Init. It is for tests and for a host that builds its provider itself;
// production boot uses Init.
func SetActive(p Provider) {
	activeMu.Lock()
	defer activeMu.Unlock()
	active, activeSet = p, true
	if p == nil {
		initStatus = Status{Provider: ProviderNone, Legacy: true}
	} else {
		initStatus = Status{Provider: p.Name(), KeyID: p.KeyID()}
	}
	initErr = nil
}

// ResetForTest clears the active provider and the init state so the next
// Init runs again. Tests only.
func ResetForTest() {
	activeMu.Lock()
	defer activeMu.Unlock()
	active, activeSet = nil, false
	initStatus, initErr = Status{}, nil
	initOnce = sync.Once{}
	logOnce = sync.Once{}
}

// LogStatus writes the one boot line this package owes the operator: INFO
// naming the provider and key generation when encryption is active, WARN once
// when secrets are stored in plaintext. Later calls are no-ops.
func LogStatus(logger *slog.Logger, st Status) {
	if logger == nil {
		logger = slog.Default()
	}
	logOnce.Do(func() {
		if st.Active() {
			logger.Info("keystore: secrets at rest are encrypted",
				"provider", st.Provider, "key_generation", st.KeyID, "generations", len(st.Generations), "source", st.Source)
			return
		}
		logger.Warn("keystore: secrets at rest are stored in PLAINTEXT (legacy mode)",
			"provider", st.Provider, "why", st.Note, "env", EnvProviderName)
	})
}

// OpenActive opens envelope with the active provider. A value that is not an
// envelope is returned unchanged with legacy=true, so callers accept the
// plaintext a pre-keystore hive wrote. An envelope with no active provider is
// an error that names the init failure when there was one.
func OpenActive(ctx context.Context, value string, aad []byte) (plaintext string, legacy bool, stale bool, err error) {
	if !IsEnvelopeString(value) {
		return value, true, false, nil
	}
	p := Active()
	pt, stale, err := Open(ctx, p, value, aad)
	if err != nil {
		if errors.Is(err, ErrNoProvider) {
			if ierr := InitError(); ierr != nil {
				err = fmt.Errorf("%w: %w", err, ierr)
			}
		}
		return "", false, false, err
	}
	return string(pt), false, stale, nil
}

// SealActive seals value with the active provider, or returns it unchanged
// when no provider is active (plaintext legacy mode). A value that is already
// an envelope is returned as is.
func SealActive(ctx context.Context, value string, aad []byte) (string, error) {
	if value == "" || IsEnvelopeString(value) {
		return value, nil
	}
	p := Active()
	if p == nil {
		return value, nil
	}
	return Seal(ctx, p, []byte(value), aad)
}
