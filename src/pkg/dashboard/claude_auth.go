package dashboard

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/claude"
)

const (
	// claudeTokenPath is where the hub persists the raw access token for
	// quick status checks, mirroring the /data/gh-user-token pattern.
	claudeTokenPath = "/data/claude-user-token"

	// claudeOAuthFlowExpiryMin is how long a started PKCE flow remains valid.
	claudeOAuthFlowExpiryMin = 10

	// claudeRedirectURI uses Claude's hosted callback page which displays
	// the authorization code directly — no localhost listener needed.
	claudeRedirectURI = "https://platform.claude.com/oauth/code/callback"
)

// Test seams: package-level vars that default to the real implementations.
// Overridden in unit tests to avoid hitting the real filesystem/network.
var (
	claudeCredPath      = claude.CredentialsPath
	claudeTokenFilePath = claudeTokenPath
	claudeReadToken     = claude.ReadAccessToken
	claudeWriteCreds    = claude.WriteCredentials
	claudeExchange      = claude.ExchangeCode
	claudeRemoveFile    = os.Remove
)

// claudeOAuthFlow holds server-side PKCE state for an in-progress login.
type claudeOAuthFlow struct {
	mu           sync.Mutex
	codeVerifier string
	state        string
	expiresAt    time.Time
}

// registerClaudeAuthRoutes wires up the Claude OAuth API endpoints.
func (s *Server) registerClaudeAuthRoutes() {
	s.mux.HandleFunc("GET /api/claude-auth/status", s.handleClaudeAuthStatus)
	s.mux.HandleFunc("POST /api/claude-auth/start", s.handleClaudeAuthStart)
	s.mux.HandleFunc("POST /api/claude-auth/exchange", s.handleClaudeAuthExchange)
	s.mux.HandleFunc("POST /api/claude-auth/logout", s.handleClaudeAuthLogout)
}

// handleClaudeAuthStatus returns the current Claude auth state.
func (s *Server) handleClaudeAuthStatus(w http.ResponseWriter, r *http.Request) {
	token := claudeReadToken(claudeCredPath)
	if token == "" {
		jsonResponse(w, map[string]interface{}{"logged_in": false})
		return
	}
	jsonResponse(w, map[string]interface{}{"logged_in": true})
}

// handleClaudeAuthStart generates PKCE params and returns the authorize URL.
// The redirect goes to http://localhost/callback (the only URI Claude allows).
// After the user authorizes, they paste the resulting URL back to the dashboard,
// which POSTs the code to /api/claude-auth/exchange.
func (s *Server) handleClaudeAuthStart(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	verifier, challenge, err := claude.GeneratePKCE()
	if err != nil {
		jsonError(w, "PKCE generation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	stateBuf := make([]byte, 16)
	if _, err := rand.Read(stateBuf); err != nil {
		jsonError(w, "state generation failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	state := hex.EncodeToString(stateBuf)

	authorizeURL := claude.BuildAuthorizeURL(challenge, claudeRedirectURI, state)

	s.claudeOAuthFlow.mu.Lock()
	s.claudeOAuthFlow.codeVerifier = verifier
	s.claudeOAuthFlow.state = state
	s.claudeOAuthFlow.expiresAt = time.Now().Add(claudeOAuthFlowExpiryMin * time.Minute)
	s.claudeOAuthFlow.mu.Unlock()

	s.auditFromRequest(r, "claude_auth_start", "", "")
	jsonResponse(w, map[string]interface{}{
		"authorize_url": authorizeURL,
	})
}

// handleClaudeAuthExchange receives the authorization code (either extracted
// from the pasted callback URL or sent directly) and exchanges it for tokens.
func (s *Server) handleClaudeAuthExchange(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	var req struct {
		Code        string `json:"code"`
		CallbackURL string `json:"callback_url"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	code := req.Code
	if code == "" && req.CallbackURL != "" {
		parsed, err := url.Parse(req.CallbackURL)
		if err != nil {
			jsonError(w, "invalid callback URL: "+err.Error(), http.StatusBadRequest)
			return
		}
		code = parsed.Query().Get("code")
		if errParam := parsed.Query().Get("error"); errParam != "" {
			errDesc := parsed.Query().Get("error_description")
			jsonError(w, fmt.Sprintf("Authorization denied: %s — %s", errParam, errDesc), http.StatusBadRequest)
			return
		}
	}

	if code == "" {
		jsonError(w, "no authorization code provided", http.StatusBadRequest)
		return
	}

	s.claudeOAuthFlow.mu.Lock()
	verifier := s.claudeOAuthFlow.codeVerifier
	expiresAt := s.claudeOAuthFlow.expiresAt
	s.claudeOAuthFlow.mu.Unlock()

	if verifier == "" || time.Now().After(expiresAt) {
		jsonError(w, "Login session expired — click Login again", http.StatusBadRequest)
		return
	}

	tokens, err := claudeExchange(code, verifier, claudeRedirectURI)
	if err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "invalid_grant") {
			jsonError(w, "Authorization code expired or already used — click Login again", http.StatusBadRequest)
		} else {
			jsonError(w, "Token exchange failed: "+errMsg, http.StatusInternalServerError)
		}
		return
	}

	if err := claudeWriteCreds(tokens, claudeCredPath); err != nil {
		jsonError(w, "Failed to save credentials: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Also write raw token for quick status checks.
	tmpPath := claudeTokenFilePath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(tokens.AccessToken), 0o600); err == nil {
		if err := os.Rename(tmpPath, claudeTokenFilePath); err != nil {
			s.deps.Logger.Warn("Claude status token rename failed", "error", err)
			_ = os.Remove(tmpPath)
		}
	}

	// Tell the agent manager to pick up the new token.
	if s.deps != nil && s.deps.AgentMgr != nil {
		s.deps.AgentMgr.ReloadClaudeToken()
	}

	// Clear the PKCE state.
	s.claudeOAuthFlow.mu.Lock()
	s.claudeOAuthFlow.codeVerifier = ""
	s.claudeOAuthFlow.state = ""
	s.claudeOAuthFlow.mu.Unlock()

	s.auditFromRequest(r, "claude_auth_complete", "", "")
	s.deps.Logger.Info("Claude Code authenticated via OAuth")

	jsonResponse(w, map[string]interface{}{"status": "complete"})
}

// handleClaudeAuthLogout removes the Claude credentials.
func (s *Server) handleClaudeAuthLogout(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}

	var removeErr error
	for _, path := range []string{claudeCredPath, claudeTokenFilePath} {
		if err := claudeRemoveFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			removeErr = err
			s.deps.Logger.Error("Claude credential removal failed", "path", path, "error", err)
		}
	}
	if removeErr != nil {
		jsonError(w, "failed to remove persisted Claude credentials", http.StatusInternalServerError)
		return
	}

	if s.deps != nil && s.deps.AgentMgr != nil {
		s.deps.AgentMgr.ReloadClaudeToken()
	}

	s.auditFromRequest(r, "claude_auth_logout", "", "")
	s.deps.Logger.Info("Claude Code logged out")
	jsonResponse(w, map[string]interface{}{"status": "logged_out"})
}
