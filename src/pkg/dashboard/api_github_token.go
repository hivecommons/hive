package dashboard

import (
	"net/http"
	"os"
	"strings"
	"time"
)

// Write-only GitHub PAT rotation for PAT-mode hives (hivecommons/hive#19633).
//
// A standalone hive that authenticates with a personal access token persists
// it in hive.yaml.runtime (keystore-sealed since #18214), so rotating it used
// to mean stopping the hive, editing the data volume and restarting; a
// revoked token in the meantime only showed as 401s and an empty backlog.
// These two owner-only routes replace that: PUT takes a new token, verifies
// it against the configured api_url before anything changes, saves it through
// the normal config save path and adopts it live. Neither route ever returns
// the token; the status names it by the same sha256 prefix /api/config/export
// uses. App-mode hives (github.app_id set) refuse the PUT: their credential
// is the App key, not a PAT.

// githubTokenVerification is what the hive remembers about the last token it
// verified in this process. It never holds the token.
type githubTokenVerification struct {
	fingerprint string
	login       string
	verifiedAt  time.Time
}

// githubTokenStatus is the GET body and the successful PUT body.
type githubTokenStatus struct {
	// Mode is "pat" or "app"; only a PAT-mode hive can rotate here.
	Mode string `json:"mode"`
	// Rotatable reports whether PUT /api/github-token is accepted.
	Rotatable bool `json:"rotatable"`
	// Configured reports whether a PAT is in use (github.token or
	// HIVE_GITHUB_TOKEN).
	Configured bool `json:"configured"`
	// Fingerprint is the first 12 hex characters of the token's sha256.
	Fingerprint string `json:"fingerprint,omitempty"`
	// Login and VerifiedAt are set when the current token was verified by
	// this process (a rotation since boot).
	Login      string     `json:"login,omitempty"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

// githubTokenFingerprint is the redaction marker's sha256 prefix, so a
// rotated token can be matched against a config export.
func githubTokenFingerprint(token string) string {
	if token == "" {
		return ""
	}
	return redactMarker(token).SHA256
}

// currentGitHubToken is the PAT the hive authenticates with, resolved the way
// boot does: github.token, then $HIVE_GITHUB_TOKEN.
func (s *Server) currentGitHubToken() string {
	if t := s.deps.Config.GitHub.Token; t != "" {
		return t
	}
	return os.Getenv("HIVE_GITHUB_TOKEN")
}

// githubTokenStatusLocked builds the status; s.githubTokenMu must be held.
func (s *Server) githubTokenStatusLocked() githubTokenStatus {
	gh := s.deps.Config.GitHub
	if gh.HasApp() {
		return githubTokenStatus{Mode: "app"}
	}
	st := githubTokenStatus{Mode: "pat", Rotatable: true}
	if fp := githubTokenFingerprint(s.currentGitHubToken()); fp != "" {
		st.Configured = true
		st.Fingerprint = fp
		if v := s.githubTokenVerified; v != nil && v.fingerprint == fp {
			at := v.verifiedAt
			st.Login, st.VerifiedAt = v.login, &at
		}
	}
	return st
}

// handleGitHubTokenGet reports which token is in use (never the token).
func (s *Server) handleGitHubTokenGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	s.githubTokenMu.Lock()
	defer s.githubTokenMu.Unlock()
	jsonResponse(w, s.githubTokenStatusLocked())
}

// handleGitHubTokenPut replaces the PAT with {"token": "..."}. A token that
// fails verification is refused with 422 and nothing changes; a save failure
// restores the previous token and answers 500. Every outcome is audited by
// fingerprint.
func (s *Server) handleGitHubTokenPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	cfg := s.deps.Config
	if cfg.GitHub.HasApp() {
		jsonError(w, "this hive authenticates as a GitHub App (github.app_id is set); there is no personal access token to rotate", http.StatusConflict)
		return
	}
	if s.deps.VerifyGitHubToken == nil || s.deps.ApplyGitHubToken == nil {
		jsonError(w, "GitHub token rotation is unavailable on this hive", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	token := strings.TrimSpace(body.Token)
	if token == "" {
		jsonError(w, `token is required: send {"token": "<new personal access token>"}`, http.StatusBadRequest)
		return
	}

	s.githubTokenMu.Lock()
	defer s.githubTokenMu.Unlock()

	fp := githubTokenFingerprint(token)
	prevToken := cfg.GitHub.Token
	prevFP := githubTokenFingerprint(s.currentGitHubToken())

	// --- verify before mutating anything ---
	login, err := s.deps.VerifyGitHubToken(r.Context(), token)
	if err != nil {
		// The verifier never names the token, but its error crosses a
		// package boundary on its way to the response and the audit log.
		msg := strings.ReplaceAll(err.Error(), token, "[redacted]")
		s.auditFromRequest(r, "github_token_rejected", auditDetail("fingerprint", fp, "reason", msg), "")
		if s.logger != nil {
			s.logger.Warn("github token rotation refused: verification failed", "fingerprint", fp, "error", msg)
		}
		jsonError(w, "the new token was not saved: "+msg, http.StatusUnprocessableEntity)
		return
	}

	// --- persist ---
	cfg.GitHub.Token = token
	if err := s.saveConfig(); err != nil {
		cfg.GitHub.Token = prevToken
		if s.logger != nil {
			s.logger.Error("failed to persist config after github token rotation", "error", err)
		}
		s.auditFromRequest(r, "github_token_rejected", auditDetail("fingerprint", fp, "reason", "config save failed"), "")
		jsonError(w, "failed to save config; the GitHub token is unchanged", http.StatusInternalServerError)
		return
	}

	// --- apply live ---
	s.deps.ApplyGitHubToken(token)
	s.githubTokenVerified = &githubTokenVerification{fingerprint: fp, login: login, verifiedAt: time.Now().UTC()}
	s.auditFromRequest(r, "github_token_rotated", auditDetail(
		"fingerprint", fp,
		"previous_fingerprint", prevFP,
		"login", login,
	), "")
	if s.logger != nil {
		s.logger.Info("github token rotated via dashboard API", "fingerprint", fp, "previous_fingerprint", prevFP, "login", login)
	}
	jsonResponse(w, s.githubTokenStatusLocked())
}
