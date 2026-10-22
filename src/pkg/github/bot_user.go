package github

import (
	"context"
	"strings"
	"time"
)

// botUserLookupTimeout bounds the one-off bot user lookup behind
// AppBotUserID so an agent launch never hangs on GitHub.
const botUserLookupTimeout = 10 * time.Second

// AppBotUserID returns the GitHub USER id of the App bot account
// ("<slug>[bot]"), or 0 when the client has no App bot or the lookup failed.
// GitHub links `<id>+<login>@users.noreply.github.com` commit emails by this
// user id, which is NOT the App id used for JWT auth (hivecommons-hive[bot] is
// user 280983584 while the App is 3568013), so callers that need linked
// commits must use this value. The first successful lookup is cached for the
// client's lifetime; failures are not cached, so a transient outage is
// retried on the next call.
func (c *Client) AppBotUserID(ctx context.Context) int64 {
	if c == nil {
		return 0
	}
	login := strings.TrimSpace(c.appBotLogin)
	if login == "" {
		return 0
	}
	c.appBotUserMu.Lock()
	defer c.appBotUserMu.Unlock()
	if c.appBotUserID > 0 {
		return c.appBotUserID
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, botUserLookupTimeout)
	defer cancel()
	user, _, err := c.client.Users.Get(ctx, login)
	if err != nil {
		c.logger.Warn("could not resolve App bot user id; agent commits fall back to the lane email until the next launch", "login", login, "error", err)
		return 0
	}
	id := user.GetID()
	if id <= 0 {
		c.logger.Warn("App bot user lookup returned no id", "login", login)
		return 0
	}
	c.appBotUserID = id
	return id
}

// selfLoginRetryAfter spaces out retries of a failed GET /user so sweep loops
// that check marker authors many times per cycle do not hammer GitHub.
const selfLoginRetryAfter = 5 * time.Minute

// resolveSelfLogin returns the login of the account the client's token
// authenticates as, learned from GET /user. It returns "" when the lookup
// failed (App installation tokens cannot call it; App clients carry
// appBotLogin instead). A success is cached for the client's lifetime; a
// failure is cached for selfLoginRetryAfter.
func (c *Client) resolveSelfLogin(ctx context.Context) string {
	if c == nil {
		return ""
	}
	c.selfLoginMu.Lock()
	if c.selfLogin != "" {
		login := c.selfLogin
		c.selfLoginMu.Unlock()
		return login
	}
	if !c.selfLoginFailedAt.IsZero() && time.Since(c.selfLoginFailedAt) < selfLoginRetryAfter {
		c.selfLoginMu.Unlock()
		return ""
	}
	c.selfLoginMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, botUserLookupTimeout)
	defer cancel()
	// The lock is not held across the network call so cachedSelfLogin stays
	// non-blocking; concurrent first callers may each look up (last writer wins).
	user, _, err := c.client.Users.Get(ctx, "")
	login := strings.TrimSpace(user.GetLogin())
	c.selfLoginMu.Lock()
	defer c.selfLoginMu.Unlock()
	if err != nil || login == "" {
		c.selfLoginFailedAt = time.Now()
		if c.logger != nil { // bare clients (e.g. tests) may carry no logger
			c.logger.Warn("could not resolve the authenticated login; once-only markers from any author are trusted until it resolves", "error", err)
		}
		return ""
	}
	c.selfLogin = login
	return login
}

// cachedSelfLogin returns the resolved self-login without triggering a lookup.
func (c *Client) cachedSelfLogin() string {
	if c == nil {
		return ""
	}
	c.selfLoginMu.Lock()
	defer c.selfLoginMu.Unlock()
	return c.selfLogin
}
