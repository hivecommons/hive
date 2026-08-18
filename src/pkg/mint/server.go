package mint

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// MintPath is the endpoint that issues scoped tokens.
	MintPath = "/mint"
	// JWKSPath is the standard public-key discovery endpoint.
	JWKSPath = "/.well-known/jwks.json"

	// jwksCacheMaxAge advertises how long a verifier may cache the JWKS. Keys
	// are long-lived so a generous cache is fine.
	jwksCacheMaxAge = 5 * time.Minute

	// maxMintBodyBytes bounds the request body to avoid unbounded reads.
	maxMintBodyBytes = 16 << 10 // 16 KiB

	// authScheme is the Authorization header scheme for the shared-secret gate.
	authScheme = "Bearer "
)

// Server exposes the mint over HTTP.
//
// Caller authentication on /mint is, for this foundation, a shared-secret
// bearer token (constant-time compared). This is deliberately minimal.
//
// TODO(caller-auth): replace the shared secret with real caller-identity
// verification — e.g. validate a spoke/agent identity assertion (an inbound
// OIDC token from the workload's own SA, or a mutual-TLS client cert), map it
// to an allowed subject + scope set, and issue only scopes that identity is
// entitled to. The shared secret proves "trusted network position", not "who".
//
// TODO(cloud-wif): the returned token is designed to be exchanged at a cloud
// WIF provider (GCP STS / AWS AssumeRoleWithWebIdentity / Azure federated
// credentials / registry token endpoint) configured to trust this issuer +
// JWKS. That exchange is provider-side and out of scope here.
type Server struct {
	minter *Minter
	secret string
	logger *slog.Logger
}

// NewServer builds a mint HTTP server. secret is the shared bearer secret that
// gates /mint; it must be non-empty (fail closed — an empty secret would allow
// anyone to mint). The secret is supplied by config/env, never hardcoded.
func NewServer(minter *Minter, secret string, logger *slog.Logger) (*Server, error) {
	if minter == nil {
		return nil, fmt.Errorf("mint: nil minter")
	}
	if secret == "" {
		return nil, fmt.Errorf("mint: shared secret is required (fail closed)")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{minter: minter, secret: secret, logger: logger}, nil
}

// Handler returns an http.Handler serving MintPath and JWKSPath.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(MintPath, s.handleMint)
	mux.HandleFunc(JWKSPath, s.handleJWKS)
	return mux
}

// MintRequest is the JSON body of a POST /mint call.
type MintRequest struct {
	Subject  string   `json:"subject"`
	Audience string   `json:"audience"`
	Scopes   []string `json:"scopes,omitempty"`
	// TTLSeconds is the requested lifetime; 0 uses the configured max. Values
	// above the cap are clamped down, never honored above the ceiling.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

// MintResponse is the JSON body returned on success.
type MintResponse struct {
	Token         string `json:"token"`
	TokenType     string `json:"token_type"`
	ExpiresInSecs int    `json:"expires_in"`
	Issuer        string `json:"issuer"`
}

func (s *Server) handleMint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.authorized(r) {
		// Fail closed. Do not distinguish missing vs wrong secret.
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxMintBodyBytes)
	var req MintRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ttl := time.Duration(req.TTLSeconds) * time.Second
	token, err := s.minter.Mint(req.Subject, req.Audience, req.Scopes, ttl)
	if err != nil {
		// Mint only errors on caller-supplied invalid input (missing
		// subject/audience) — treat as a 400, never leak internals.
		writeErr(w, http.StatusBadRequest, "cannot mint token")
		return
	}

	// Re-derive the honored TTL for the response (Mint clamps internally).
	honored := s.minter.clampTTL(ttl)
	resp := MintResponse{
		Token:         token,
		TokenType:     "Bearer",
		ExpiresInSecs: int(honored.Seconds()),
		Issuer:        s.minter.issuer,
	}
	writeJSON(w, http.StatusOK, resp)
	s.logger.Info("token minted",
		"subject", req.Subject, "audience", req.Audience,
		"scopes", req.Scopes, "ttl_seconds", int(honored.Seconds()))
}

func (s *Server) handleJWKS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := s.minter.JWKS()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "jwks unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(jwksCacheMaxAge.Seconds())))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// authorized checks the shared-secret bearer token in constant time.
func (s *Server) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, authScheme) {
		return false
	}
	presented := strings.TrimPrefix(h, authScheme)
	// Constant-time compare; length-independent short-circuit is unavoidable
	// but subtle.ConstantTimeCompare returns 0 on length mismatch without
	// leaking which byte differed.
	return subtle.ConstantTimeCompare([]byte(presented), []byte(s.secret)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
