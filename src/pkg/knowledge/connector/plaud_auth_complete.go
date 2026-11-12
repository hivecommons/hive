package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AuthCompletion carries what the operator pasted to finish a sign-in whose
// OAuth callback cannot reach the hive: either the full redirect URL the
// browser landed on, or the vendor token file contents from a laptop login.
type AuthCompletion struct {
	RedirectURL string `json:"redirect_url,omitempty"`
	Tokens      string `json:"tokens,omitempty"`
}

// AuthCompleter is optionally implemented by Authenticators whose OAuth
// client only allows a fixed localhost redirect (Plaud: localhost:8199). A
// hosted hive never receives that callback, so the dashboard offers two
// completions: forward the pasted redirect URL to the in-pod listener while
// the login session is alive, or install pasted tokens directly.
type AuthCompleter interface {
	CompleteAuth(ctx context.Context, c AuthCompletion) (AuthState, error)
}

var _ AuthCompleter = (*plaudConnector)(nil)

const (
	// plaudCallbackBase is the MCP server's fixed OAuth listener.
	plaudCallbackBase = "http://127.0.0.1:8199"
	// plaudCallbackPath is the path the vendor registered as redirect URI.
	plaudCallbackPath = "/auth/callback"
	// plaudTokenFile is the MCP server's token store relative to HOME.
	plaudTokenFile = ".plaud/tokens-mcp.json"
	// plaudCallbackTimeout bounds the forwarded callback request.
	plaudCallbackTimeout = 20 * time.Second
	// plaudCompleteSettle is how long CompleteAuth waits for the login
	// goroutine to notice the new session before reporting state.
	plaudCompleteSettle = 6 * time.Second
)

// ErrNoPendingLogin is returned when a redirect URL is pasted but no login
// session is alive to receive it (Connect must be called first).
var ErrNoPendingLogin = errors.New("plaud: no sign-in is pending; press Connect first, then paste the redirect URL within the login window")

// CompleteAuth implements AuthCompleter.
func (c *plaudConnector) CompleteAuth(ctx context.Context, comp AuthCompletion) (AuthState, error) {
	switch {
	case strings.TrimSpace(comp.Tokens) != "":
		if err := c.installTokens(comp.Tokens); err != nil {
			return c.AuthStatus(ctx, false), err
		}
		c.mu.Lock()
		if l := c.login; l != nil {
			l.cancel()
			c.login = nil
		}
		c.auth = AuthState{Pending: false, CheckedAt: c.now().UTC(), Message: "tokens installed"}
		c.mu.Unlock()
		return c.AuthStatus(ctx, true), nil
	case strings.TrimSpace(comp.RedirectURL) != "":
		return c.forwardRedirect(ctx, comp.RedirectURL)
	default:
		return c.AuthStatus(ctx, false), errors.New("plaud: redirect_url or tokens is required")
	}
}

// forwardRedirect replays the pasted callback against the MCP server's
// localhost listener inside the pod.
func (c *plaudConnector) forwardRedirect(ctx context.Context, raw string) (AuthState, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.RawQuery == "" {
		return c.AuthStatus(ctx, false), errors.New("plaud: paste the full URL the browser landed on (it contains ?code=...&state=...)")
	}
	if u.Query().Get("code") == "" {
		return c.AuthStatus(ctx, false), errors.New("plaud: the pasted URL has no code parameter")
	}
	c.mu.Lock()
	l := c.login
	c.mu.Unlock()
	if l == nil {
		return c.AuthStatus(ctx, false), ErrNoPendingLogin
	}
	select {
	case <-l.done:
		return c.AuthStatus(ctx, false), ErrNoPendingLogin
	default:
	}
	target := strings.TrimRight(c.callbackBase, "/") + plaudCallbackPath + "?" + u.RawQuery
	rctx, cancel := context.WithTimeout(ctx, plaudCallbackTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, target, nil)
	if err != nil {
		return c.AuthStatus(ctx, false), err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return c.AuthStatus(ctx, false), fmt.Errorf("plaud: forwarding callback to the login session: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode >= 400 {
		return c.AuthStatus(ctx, false), fmt.Errorf("plaud: callback rejected (%s): %s", resp.Status, firstLine(string(body)))
	}
	// The login goroutine polls get_current_user and flips state itself;
	// give it a moment so the caller sees the outcome.
	settle, cancelSettle := context.WithTimeout(ctx, plaudCompleteSettle)
	defer cancelSettle()
	select {
	case <-l.done:
	case <-settle.Done():
	}
	return c.AuthStatus(ctx, false), nil
}

// installTokens validates and writes a pasted tokens-mcp.json into the
// connector's HOME so the MCP server picks it up on next start.
func (c *plaudConnector) installTokens(raw string) error {
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal([]byte(raw), &tok); err != nil {
		return fmt.Errorf("plaud: tokens must be the JSON contents of ~/.plaud/tokens-mcp.json: %w", err)
	}
	if tok.AccessToken == "" && tok.RefreshToken == "" {
		return errors.New("plaud: tokens JSON has neither access_token nor refresh_token")
	}
	home := c.home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("plaud: resolving HOME: %w", err)
		}
		home = h
	}
	path := filepath.Join(home, plaudTokenFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("plaud: %w", err)
	}
	return os.WriteFile(path, []byte(strings.TrimSpace(raw)+"\n"), 0o600)
}

// TokenPath returns where installTokens writes (tests).
func (c *plaudConnector) tokenPath() string {
	home := c.home
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, plaudTokenFile)
}
