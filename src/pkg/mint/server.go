package mint

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
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
// Caller authentication on /mint goes through a CallerAuthenticator (caller.go).
// The default is the original shared-secret bearer token, constant-time
// compared, so behaviour is unchanged unless an operator configures otherwise.
//
// What a shared secret proves is "trusted network position", not "who" — see
// caller.go for the finding (#3915) and for what changed. In short: the mint no
// longer treats possession of a credential as permission to mint ANYTHING.
// Where Entitlements are configured the mint is deny-by-default per identity,
// and the verified identity is recorded on every mint and every refusal.
//
// The Kubernetes TokenReview backend now exists behind that seam and needed no
// change here, which was the point: see tokenreview.go for
// TokenReviewAuthenticator (audience-scoped, so a token minted for the API
// server cannot be replayed at the mint and vice versa) and
// MultiAuthenticator, the dual-accept step for migrating off the shared secret
// without a flag day. It needs no new module dependency.
//
// TODO(caller-auth): an mTLS client-certificate backend, for deployments with
// no Kubernetes API server to ask. Same interface, same absence of changes
// here.
//
// TODO(cloud-wif): the returned token is designed to be exchanged at a cloud
// WIF provider (GCP STS / AWS AssumeRoleWithWebIdentity / Azure federated
// credentials / registry token endpoint) configured to trust this issuer +
// JWKS. That exchange is provider-side and out of scope here.
type Server struct {
	minter *Minter
	auth   CallerAuthenticator
	ents   Entitlements
	logger *slog.Logger
}

// ServerOption configures a Server. Options are additive: a Server built
// without any behaves exactly as it did before #3915.
type ServerOption func(*Server)

// WithAuthenticator replaces the caller-authentication mechanism. This is the
// seam for a TokenReview or mTLS backend; passing nil is ignored so a caller
// cannot accidentally disable authentication.
func WithAuthenticator(a CallerAuthenticator) ServerOption {
	return func(s *Server) {
		if a != nil {
			s.auth = a
		}
	}
}

// WithEntitlements bounds what each verified identity may mint. Once a non-empty
// set is supplied the mint is deny-by-default: an identity with no entry may
// mint nothing, and an entitlement's empty dimension allows nothing rather than
// everything. See Entitlement.
func WithEntitlements(e Entitlements) ServerOption {
	return func(s *Server) { s.ents = e }
}

// NewServer builds a mint HTTP server. secret is the shared bearer secret that
// gates /mint; it must be non-empty (fail closed — an empty secret would allow
// anyone to mint). The secret is supplied by config/env, never hardcoded.
//
// The signature is unchanged from before #3915 and the default posture is
// identical: a shared-secret gate with no entitlement bound. Use
// WithAuthenticator and WithEntitlements to tighten it.
func NewServer(minter *Minter, secret string, logger *slog.Logger, opts ...ServerOption) (*Server, error) {
	if minter == nil {
		return nil, fmt.Errorf("mint: nil minter")
	}
	auth, err := NewSharedSecretAuthenticator(secret)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{minter: minter, auth: auth, logger: logger}
	for _, opt := range opts {
		opt(s)
	}

	// Say the posture out loud at construction. An unbounded mint is a
	// deliberate configuration, not an accident to discover from a token that
	// should never have been issued.
	if len(s.ents) == 0 {
		s.logger.Warn("mint: no caller entitlements configured — any authenticated caller may mint any subject, audience and scope (bounded only by the TTL cap)",
			"authenticator", s.auth.Name())
	} else {
		s.logger.Info("mint: caller entitlements active (deny-by-default)",
			"authenticator", s.auth.Name(), "identities", s.ents.identityNames())
	}
	return s, nil
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
	identity, err := s.auth.Authenticate(r)
	if err != nil {
		// Fail closed. Do not distinguish missing vs malformed vs wrong
		// credential — the client learns only that it is not authorized.
		s.logger.Warn("mint: unauthenticated /mint request refused",
			"authenticator", s.auth.Name(), "remote_addr", r.RemoteAddr)
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

	// Deny-by-default once entitlements are configured. This is what stops a
	// credential holder minting for a subject it has no business asserting —
	// the "any subject, any audience, any scope" half of #3915. Refused with
	// 403, not 401: the caller IS authenticated, it is simply not entitled.
	if ok, why := s.ents.permits(identity, req.Subject, req.Audience, req.Scopes); !ok {
		s.logger.Warn("mint: refused, caller not entitled",
			"caller", identity.String(), "caller_kind", identity.Kind,
			"subject", req.Subject, "audience", req.Audience,
			"scopes", req.Scopes, "reason", why)
		writeErr(w, http.StatusForbidden, "not entitled")
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
	// The caller is part of the audit record (#3915): a mint line that names
	// only the subject cannot answer "who asked for this token".
	s.logger.Info("token minted",
		"caller", identity.String(), "caller_kind", identity.Kind,
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

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
