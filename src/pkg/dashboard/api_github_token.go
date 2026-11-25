package dashboard

import (
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// defaultGitHubTokenRejectCooldown is how long after a rejected token the
// next PUT is refused with 429. Each attempt costs 2 + N GitHub calls made
// with the candidate token, so an owner session that loops on bad tokens is
// slowed to one attempt per window. HIVE_GITHUB_TOKEN_REJECT_COOLDOWN
// overrides it with a Go duration; "0" turns the cooldown off.
const defaultGitHubTokenRejectCooldown = 15 * time.Second

// githubTokenRejectCooldownEnv names the override for the cooldown.
const githubTokenRejectCooldownEnv = "HIVE_GITHUB_TOKEN_REJECT_COOLDOWN"

// githubTokenSealable reports whether the config save would seal the token.
// A var so tests can cover both outcomes without a live keystore.
var githubTokenSealable = config.GitHubTokenSealable

func githubTokenRejectCooldown() time.Duration {
	if v := strings.TrimSpace(os.Getenv(githubTokenRejectCooldownEnv)); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return defaultGitHubTokenRejectCooldown
}

func (s *Server) githubTokenClock() time.Time {
	if s.githubTokenNow != nil {
		return s.githubTokenNow()
	}
	return time.Now()
}

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
	// Sealed is set on a successful PUT only: true when the saved token is a
	// keystore envelope, false when it was written as plaintext (and the
	// config file was kept owner-only, 0600, for it).
	Sealed *bool `json:"sealed,omitempty"`
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

// githubTokenStatusLocked builds the status; s.githubConfigMu must be held.
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
	s.githubConfigMu.Lock()
	defer s.githubConfigMu.Unlock()
	jsonResponse(w, s.githubTokenStatusLocked())
}

// githubTokenRequestAllowed is the CSRF guard for the PUT: a cross-site
// fetch is refused outright (precedent: companionTokenForm), and the body
// must be declared JSON, which a plain HTML form cannot send.
func githubTokenRequestAllowed(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		jsonError(w, "cross-site request refused", http.StatusForbidden)
		return false
	}
	ct := strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
	if !strings.EqualFold(ct, "application/json") {
		jsonError(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	return true
}

// protectTokenFile makes sure the config file that is about to carry the PAT
// in plaintext is not readable by group or others. It tightens an existing
// file to 0600 and creates a missing one as 0600 (Save keeps an existing
// file's mode). created reports that it made the file, so a failed save can
// remove the empty placeholder.
func protectTokenFile(path string) (created bool, err error) {
	fi, statErr := os.Stat(path)
	switch {
	case statErr == nil:
		if fi.Mode().Perm()&0o077 != 0 {
			return false, os.Chmod(path, 0o600)
		}
		return false, nil
	case os.IsNotExist(statErr):
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return false, err
		}
		return true, f.Close()
	default:
		return false, statErr
	}
}

// handleGitHubTokenPut replaces the PAT with {"token": "..."}. A token that
// fails verification is refused with 422 and nothing changes; a save failure
// restores the previous token and answers 500. Every outcome is audited by
// fingerprint. The whole verify, persist and apply sequence runs under
// githubConfigMu, and the App-mode and persistence checks are made under it.
func (s *Server) handleGitHubTokenPut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if !githubTokenRequestAllowed(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
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

	s.githubConfigMu.Lock()
	defer s.githubConfigMu.Unlock()

	cfg := s.deps.Config
	// Rechecked under the lock: an App setup that finished while this request
	// waited must win over a PAT write.
	if cfg.GitHub.HasApp() {
		jsonError(w, "this hive authenticates as a GitHub App (github.app_id is set); there is no personal access token to rotate", http.StatusConflict)
		return
	}
	// saveConfig() returns nil without writing when there is no source path,
	// so the rotation would answer 200 for a token lost on restart. Refuse
	// before verifying anything, as applyGitHubConfigUpdate does.
	if cfg.SourcePath == "" {
		if s.logger != nil {
			s.logger.Error("github token rotation refused: config has no source path, save would be an in-memory no-op")
		}
		jsonError(w, "config not persisted: config has no source path, so the new token would be lost on restart", http.StatusInternalServerError)
		return
	}

	// Cooldown after a rejected token, before any GitHub call is made.
	if cd := githubTokenRejectCooldown(); cd > 0 && !s.githubTokenRejectedAt.IsZero() {
		if wait := cd - s.githubTokenClock().Sub(s.githubTokenRejectedAt); wait > 0 {
			secs := int(math.Ceil(wait.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			jsonError(w, fmt.Sprintf("a token was rejected recently; retry in %d seconds", secs), http.StatusTooManyRequests)
			return
		}
	}

	fp := githubTokenFingerprint(token)
	prevToken := cfg.GitHub.Token
	prevFP := githubTokenFingerprint(s.currentGitHubToken())

	// --- verify before mutating anything ---
	login, err := s.deps.VerifyGitHubToken(r.Context(), token)
	if err != nil {
		s.githubTokenRejectedAt = s.githubTokenClock()
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
	// When Save cannot seal the token it writes it as it is (redactedForPersist
	// falls back to plaintext), and the primary config file keeps whatever
	// mode it had, 0644 by default. Keep that file owner-only first.
	sealed := githubTokenSealable(token)
	createdFile := false
	if !sealed {
		created, perr := protectTokenFile(cfg.SourcePath)
		if perr != nil {
			if s.logger != nil {
				s.logger.Error("github token rotation refused: cannot make the config file owner-only", "path", cfg.SourcePath, "error", perr)
			}
			s.auditFromRequest(r, "github_token_rejected", auditDetail("fingerprint", fp, "reason", "token cannot be sealed and the config file cannot be made owner-only"), "")
			jsonError(w, "the token cannot be sealed and the config file could not be made owner-only, so it was not saved; the GitHub token is unchanged", http.StatusInternalServerError)
			return
		}
		createdFile = created
	}
	cfg.GitHub.Token = token
	if err := s.saveConfig(); err != nil {
		cfg.GitHub.Token = prevToken
		if createdFile {
			_ = os.Remove(cfg.SourcePath) // best-effort: drop the empty placeholder this request made
		}
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
		"sealed", strconv.FormatBool(sealed),
	), "")
	if s.logger != nil {
		s.logger.Info("github token rotated via dashboard API", "fingerprint", fp, "previous_fingerprint", prevFP, "login", login, "sealed", sealed)
	}
	st := s.githubTokenStatusLocked()
	st.Sealed = &sealed
	jsonResponse(w, st)
}
