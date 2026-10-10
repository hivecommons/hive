package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

const (
	defaultTokenIdentityTTL         = time.Hour
	defaultTokenIdentityNegativeTTL = 5 * time.Minute
	tokenIdentityCacheMaxEntries    = 1024
	tokenIdentityTTLEnv             = "HIVE_GITHUB_TOKEN_IDENTITY_TTL"
	tokenIdentityNegativeTTLEnv     = "HIVE_GITHUB_TOKEN_IDENTITY_NEGATIVE_TTL"
)

type tokenIdentityCache struct {
	mu          sync.Mutex
	entries     map[tokenIdentityKey]tokenIdentityEntry
	now         func() time.Time
	ttl         time.Duration
	negativeTTL time.Duration
}

type tokenIdentityKey struct {
	tokenHash string
	apiURL    string
}

type tokenIdentityEntry struct {
	user      *GitHubUser
	err       error
	expiresAt time.Time
}

var sharedTokenIdentityCache = newTokenIdentityCache()

func newTokenIdentityCache() *tokenIdentityCache {
	return &tokenIdentityCache{
		entries:     map[tokenIdentityKey]tokenIdentityEntry{},
		now:         time.Now,
		ttl:         durationFromEnv(tokenIdentityTTLEnv, defaultTokenIdentityTTL),
		negativeTTL: durationFromEnv(tokenIdentityNegativeTTLEnv, defaultTokenIdentityNegativeTTL),
	}
}

// ValidateTokenCached validates a GitHub token and caches the resolved identity.
// Successful identities are cached for HIVE_GITHUB_TOKEN_IDENTITY_TTL (default
// 1h). Failed validations are cached briefly (default 5m) so revoked tokens do
// not get re-probed on every dashboard poll or heartbeat.
func ValidateTokenCached(token, apiURL string) (*GitHubUser, error) {
	return validateTokenCached(context.Background(), token, apiURL)
}

func validateTokenCached(ctx context.Context, token, apiURL string) (*GitHubUser, error) {
	key := tokenIdentityCacheKey(token, apiURL)
	if ent, ok := sharedTokenIdentityCache.get(key); ok {
		return cloneGitHubUser(ent.user), ent.err
	}
	user, err := ValidateTokenWithContext(ctx, token, apiURL)
	sharedTokenIdentityCache.put(key, user, err)
	return cloneGitHubUser(user), err
}

// InvalidateTokenIdentity removes all cached identities for token, regardless
// of API URL. Logout calls this after deleting /data/gh-user-token so a later
// re-login cannot observe a stale cached identity for the same token string.
func InvalidateTokenIdentity(token string) {
	tokenHash := tokenIdentityHash(token)
	if tokenHash == "" {
		return
	}
	sharedTokenIdentityCache.invalidateTokenHash(tokenHash)
}

func (c *tokenIdentityCache) get(key tokenIdentityKey) (tokenIdentityEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ent, ok := c.entries[key]
	if !ok {
		return tokenIdentityEntry{}, false
	}
	now := c.now()
	if !now.Before(ent.expiresAt) {
		delete(c.entries, key)
		return tokenIdentityEntry{}, false
	}
	ent.user = cloneGitHubUser(ent.user)
	return ent, true
}

func (c *tokenIdentityCache) put(key tokenIdentityKey, user *GitHubUser, err error) {
	ttl := c.ttl
	if err != nil {
		ttl = c.negativeTTL
	}
	if ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.pruneLocked(now)
	c.entries[key] = tokenIdentityEntry{user: cloneGitHubUser(user), err: err, expiresAt: now.Add(ttl)}
	c.evictLocked()
}

func (c *tokenIdentityCache) invalidateTokenHash(tokenHash string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if key.tokenHash == tokenHash {
			delete(c.entries, key)
		}
	}
}

func (c *tokenIdentityCache) pruneLocked(now time.Time) {
	for key, ent := range c.entries {
		if !now.Before(ent.expiresAt) {
			delete(c.entries, key)
		}
	}
}

func (c *tokenIdentityCache) evictLocked() {
	for len(c.entries) > tokenIdentityCacheMaxEntries {
		var victim tokenIdentityKey
		var oldest time.Time
		first := true
		for key, ent := range c.entries {
			if first || ent.expiresAt.Before(oldest) {
				victim = key
				oldest = ent.expiresAt
				first = false
			}
		}
		if first {
			return
		}
		delete(c.entries, victim)
	}
}

func tokenIdentityCacheKey(token, apiURL string) tokenIdentityKey {
	return tokenIdentityKey{tokenHash: tokenIdentityHash(token), apiURL: strings.TrimRight(strings.TrimSpace(apiURL), "/")}
}

func tokenIdentityHash(token string) string {
	token = strings.TrimSpace(token)
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func cloneGitHubUser(user *GitHubUser) *GitHubUser {
	if user == nil {
		return nil
	}
	cp := *user
	return &cp
}

func resetTokenIdentityCacheForTest(now func() time.Time, ttl, negativeTTL time.Duration) func() {
	old := sharedTokenIdentityCache
	sharedTokenIdentityCache = &tokenIdentityCache{
		entries:     map[tokenIdentityKey]tokenIdentityEntry{},
		now:         now,
		ttl:         ttl,
		negativeTTL: negativeTTL,
	}
	return func() { sharedTokenIdentityCache = old }
}
