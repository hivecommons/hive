package credsidecar

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Minter mints a tier-scoped GitHub installation token. github.AppAuth
// satisfies it (ScopedToken), which is the exact mint the in-process #3967
// lane uses, so a sidecar-served agent gets the same permissions it would
// have had in-process.
type Minter interface {
	ScopedToken(ctx context.Context, tier string) (string, error)
}

// TokenTTL is how long the sidecar reuses a minted tier token. GitHub
// installation tokens live one hour; reusing one for 45 minutes leaves a
// 15-minute margin so a request never carries a token about to expire, while
// minting at most a handful of tokens per hour per tier instead of one per
// request.
const TokenTTL = 45 * time.Minute

// knownTiers are the tiers the sidecar mints for - the set
// agent.TokenTierForRole produces and github.AppAuth.ScopedTokenForRepos
// scopes. A signed request naming any other tier is refused rather than
// minted with the mint's metadata-only default, so a protocol drift between
// proxy and sidecar is loud.
var knownTiers = map[string]bool{
	"newcomer":    true,
	"contributor": true,
	"trusted":     true,
	"merger":      true,
	"reviewer":    true,
	"advisor":     true,
}

// ErrUnknownTier is returned for a tier outside knownTiers.
var ErrUnknownTier = fmt.Errorf("unknown token tier")

type cachedToken struct {
	token   string
	expires time.Time
}

// tokenCache holds one minted token per tier. It lives ONLY in the sidecar
// process; nothing in it is ever logged or returned to the proxy.
type tokenCache struct {
	minter Minter
	now    func() time.Time

	mu     sync.Mutex
	tokens map[string]cachedToken
}

func newTokenCache(m Minter, now func() time.Time) *tokenCache {
	return &tokenCache{minter: m, now: now, tokens: make(map[string]cachedToken)}
}

// token returns a live token for tier, minting one when none is cached or the
// cached one has aged past TokenTTL. The lock is held across the mint so a
// burst of requests for a cold tier mints once, not once per request.
func (c *tokenCache) token(ctx context.Context, tier string) (string, error) {
	if !knownTiers[tier] {
		return "", fmt.Errorf("%w %q", ErrUnknownTier, tier)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if t, ok := c.tokens[tier]; ok && now.Before(t.expires) {
		return t.token, nil
	}
	tok, err := c.minter.ScopedToken(ctx, tier)
	if err != nil {
		return "", fmt.Errorf("minting %s token: %w", tier, err)
	}
	if tok == "" {
		return "", fmt.Errorf("minting %s token: empty token", tier)
	}
	c.tokens[tier] = cachedToken{token: tok, expires: now.Add(TokenTTL)}
	return tok, nil
}
