package hivecap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api/callback"
	"github.com/hivecommons/hive/crux/config"
)

// Credential writers for a sidecar runtime (hivecommons/hive#14138 stage 4).
// In-process, hive hands the manager its GitHub App auth (SetAppAuth), the
// opt-in mint issuer (SetAgentMint) and the Bob key resolvers directly; here
// the same three seams are satisfied over the callback. Token material is
// returned to the manager or written to the existing per-agent cache files
// and goes nowhere else: never logged, never kept in these adapters beyond
// the Bob key's short cache.

const (
	// agentTokenCachePerms is the mode a cache file is created with when
	// the entrypoint did not pre-create it (owner-only; the agent cannot
	// read it, which WriteAgentToken says out loud).
	agentTokenCachePerms = 0o600
	// agentTokenCacheDirPerms is the cache directory mode.
	agentTokenCacheDirPerms = 0o755
	// DefaultBobCacheTTL is how long BobCredentials keeps hive's answer:
	// the resolvers are read on the launch path under the manager lock,
	// so one round trip per minute, not per launch.
	DefaultBobCacheTTL = time.Minute
)

// ErrProxyInjectionUnsupported is returned by AppTokenWriter when
// HIVE_PROXY_INJECT_GH_AUTH is on: in-process that flag diverts the real
// token to hive's proxy registry and writes a placeholder; a sidecar has no
// such registry yet (stage 5), and writing the real token would silently
// undo the opt-in, so the write is refused and the agent runs on no token.
var ErrProxyInjectionUnsupported = errors.New("hivecap: proxy-side GitHub credential injection is not available in sidecar mode")

// AppTokenWriter implements crux.AppTokenMinter: WriteAgentToken mints the
// tier-scoped, installation-wide token hive's AppAuth mints in-process
// (POST /v1/tokens with the agent and tier, no repos, hive's TTL) and
// writes it in place into crux.AgentTokenCachePath. In place, not
// temp-and-rename: the entrypoint pre-creates the file dev:hive-<agent>
// 0640 and a rename would replace that inode (see pkg/github.AppAuth).
type AppTokenWriter struct {
	caps callback.TokenMinter
	// CacheDir is the token cache directory; empty means
	// crux.DefaultAgentTokenCacheDir.
	CacheDir string
	// Logger receives the outcome lines (never the token); nil means
	// slog.Default().
	Logger *slog.Logger
	// injectOn reports HIVE_PROXY_INJECT_GH_AUTH; tests replace it.
	injectOn func() bool
}

// NewAppTokenWriter builds a writer over caps.
func NewAppTokenWriter(caps callback.TokenMinter) *AppTokenWriter {
	return &AppTokenWriter{caps: caps, injectOn: config.ProxyInjectGHAuth}
}

func (w *AppTokenWriter) logger() *slog.Logger {
	if w.Logger == nil {
		return slog.Default()
	}
	return w.Logger
}

func (w *AppTokenWriter) dir() string {
	if w.CacheDir == "" {
		return crux.DefaultAgentTokenCacheDir
	}
	return w.CacheDir
}

// WriteAgentToken mints and caches agentName's token. A hive without a
// GitHub App answers callback.ErrNotConfigured and an unreachable hive
// callback.ErrUnavailable; both leave the cache file untouched (a stale
// token is better than none until it expires) and are returned so the
// manager's condition reporting sees them.
func (w *AppTokenWriter) WriteAgentToken(ctx context.Context, agentName, tier string, agentUID int) error {
	if w == nil || w.caps == nil {
		return errors.New("hivecap: no capabilities")
	}
	if w.injectOn != nil && w.injectOn() {
		return fmt.Errorf("%w: agent %s", ErrProxyInjectionUnsupported, agentName)
	}
	resp, err := w.caps.MintToken(ctx, callback.MintTokenRequest{Agent: agentName, Tier: tier})
	if err != nil {
		return fmt.Errorf("minting scoped token for %s: %w", agentName, err)
	}
	if !resp.ExpiresAt.IsZero() && !resp.ExpiresAt.After(time.Now()) {
		return fmt.Errorf("minting scoped token for %s: already expired at %s", agentName, resp.ExpiresAt.Format(time.RFC3339))
	}
	dir := w.dir()
	if err := os.MkdirAll(dir, agentTokenCacheDirPerms); err != nil {
		return fmt.Errorf("creating agent token dir: %w", err)
	}
	cachePath := crux.AgentTokenCachePath(dir, agentName)
	_, statErr := os.Stat(cachePath)
	preCreated := statErr == nil
	f, err := os.OpenFile(cachePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, agentTokenCachePerms)
	if err != nil {
		return fmt.Errorf("opening agent token cache %s: %w", cachePath, err)
	}
	// Shared-UID mode has no private agent group; keep this owner-only even
	// if a previous deployment pre-created a group-readable cache.
	if agentUID == 0 && tier == crux.SharedUIDReadTokenTier {
		if err := f.Chmod(agentTokenCachePerms); err != nil {
			_ = f.Close()
			return fmt.Errorf("securing shared-UID token cache %s: %w", cachePath, err)
		}
	}
	if _, err := f.WriteString(resp.Token); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing agent token cache %s: %w", cachePath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing agent token cache %s: %w", cachePath, err)
	}
	if !preCreated && agentUID > 0 {
		w.logger().Warn("agent token cache was not pre-created by entrypoint — the agent CANNOT read it; ensure the sidecar's entrypoint created this file as dev:hive-<agent> 0640",
			"agent", agentName, "uid", agentUID, "path", cachePath)
	}
	w.logger().Info("per-agent token cached via hive callback", "agent", agentName, "tier", tier, "uid", agentUID, "pre_created", preCreated, "expires_at", resp.ExpiresAt)
	return nil
}

// AgentMint implements crux.AgentMintIssuer over POST /v1/tokens/mint.
// Enabled reports whether the capability exists on this side at all; a hive
// whose mint is off answers an empty token (the in-process "" contract) or
// not_configured, both of which MintAgentToken turns into "" with no error
// so the manager's additive mint path stays a no-op. An unreachable hive
// is an error the manager logs and swallows (the App token is unaffected).
type AgentMint struct {
	caps    callback.AgentMinter
	timeout time.Duration
}

// NewAgentMint builds the issuer over caps; nil when caps cannot mint.
func NewAgentMint(caps any, timeout time.Duration) AgentMint {
	if timeout <= 0 {
		timeout = callback.DefaultTimeout
	}
	m, _ := caps.(callback.AgentMinter)
	return AgentMint{caps: m, timeout: timeout}
}

// Enabled is true when the callback can be asked.
func (m AgentMint) Enabled() bool { return m.caps != nil }

// MintAgentToken asks hive for agentName's mint credential at tier.
func (m AgentMint) MintAgentToken(agentName, tier string) (string, error) {
	if m.caps == nil {
		return "", nil
	}
	ctx, cancel := background(m.timeout)
	defer cancel()
	resp, err := m.caps.MintAgentToken(ctx, callback.AgentMintRequest{Agent: agentName, Tier: tier})
	if errors.Is(err, callback.ErrNotConfigured) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("hivecap: minting agent token: %w", err)
	}
	if !resp.ExpiresAt.IsZero() && !resp.ExpiresAt.After(time.Now()) {
		return "", fmt.Errorf("hivecap: minted agent token already expired at %s", resp.ExpiresAt.Format(time.RFC3339))
	}
	return resp.Token, nil
}

// BobCredentials serves SetBobAPIKeyResolver (Key) and
// SetBobKeySourceResolver (Source) from GET /v1/bob/credential, caching
// hive's answer for TTL. Unreachable or not configured is an empty
// credential: no key is injected and no source file is checked, exactly
// the unconfigured in-process behaviour.
type BobCredentials struct {
	caps    callback.BobCredentials
	timeout time.Duration
	// TTL bounds the cache; non-positive means DefaultBobCacheTTL.
	TTL time.Duration
	// now is the clock; tests replace it.
	now func() time.Time

	mu      sync.Mutex
	cred    callback.BobCredential
	fetched time.Time
}

// NewBobCredentials builds the resolver over caps; a caps without the Bob
// half resolves empty.
func NewBobCredentials(caps any, timeout time.Duration) *BobCredentials {
	if timeout <= 0 {
		timeout = callback.DefaultTimeout
	}
	b, _ := caps.(callback.BobCredentials)
	return &BobCredentials{caps: b, timeout: timeout, now: time.Now}
}

func (b *BobCredentials) ttl() time.Duration {
	if b.TTL <= 0 {
		return DefaultBobCacheTTL
	}
	return b.TTL
}

// resolve returns the cached credential, refreshing it when stale. A failed
// refresh keeps serving the previous answer until TTL, then resolves empty.
func (b *BobCredentials) resolve() callback.BobCredential {
	if b == nil || b.caps == nil {
		return callback.BobCredential{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if !b.fetched.IsZero() && now.Sub(b.fetched) < b.ttl() {
		return b.cred
	}
	ctx, cancel := background(b.timeout)
	defer cancel()
	cred, err := b.caps.BobCredential(ctx)
	if err != nil {
		if !b.fetched.IsZero() && now.Sub(b.fetched) < 2*b.ttl() {
			return b.cred
		}
		b.cred, b.fetched = callback.BobCredential{}, time.Time{}
		return b.cred
	}
	b.cred, b.fetched = cred, now
	return cred
}

// Key is the Bob API key ("" when none).
func (b *BobCredentials) Key() string { return b.resolve().APIKey }

// Source is where hive found the key ("file:<path>", "env:<NAME>", "").
func (b *BobCredentials) Source() string { return b.resolve().Source }
