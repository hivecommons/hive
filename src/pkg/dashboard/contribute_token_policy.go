package dashboard

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

const contributorTokenExpired = "token expired — run `hivectl contribute login` (POST /api/contribute/reissue-token with GitHub authentication)"

type contributorTokenPolicy struct{ maxAge, idleMax time.Duration }

func tokenPolicyDuration(value, env string, fallback time.Duration) time.Duration {
	if override := os.Getenv(env); override != "" {
		value = override
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 || (d == 0 && fallback > 0) {
		return fallback
	}
	return d
}

func defaultContributorTokenPolicy() contributorTokenPolicy {
	return contributorTokenPolicy{
		tokenPolicyDuration("", "HIVE_CONTRIBUTE_TOKEN_MAX_AGE", 90*24*time.Hour),
		tokenPolicyDuration("", "HIVE_CONTRIBUTE_TOKEN_IDLE_MAX", 0),
	}
}

func (s *Server) contributorTokenPolicy() contributorTokenPolicy {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return defaultContributorTokenPolicy()
	}
	c := s.deps.Config.Contribute
	return contributorTokenPolicy{
		tokenPolicyDuration(c.TokenMaxAge, "HIVE_CONTRIBUTE_TOKEN_MAX_AGE", 90*24*time.Hour),
		tokenPolicyDuration(c.TokenIdleMax, "HIVE_CONTRIBUTE_TOKEN_IDLE_MAX", 0),
	}
}

func policyOrDefault(policies []contributorTokenPolicy) contributorTokenPolicy {
	if len(policies) > 0 {
		return policies[0]
	}
	return defaultContributorTokenPolicy()
}

func (policy contributorTokenPolicy) issue(p *ContributorProfile, now time.Time) {
	p.TokenIssuedAt = now
	p.TokenExpiresAt = now.Add(policy.maxAge)
	p.TokenLastUsedAt = now
}

// migrate persists a single migration timestamp, never deriving it from registration
// age or resetting it on restart. Conflicting writes fail closed and can be retried.
func (policy contributorTokenPolicy) migrate(p *ContributorProfile, now time.Time) error {
	if p.RegistrationToken == "" || !p.TokenIssuedAt.IsZero() {
		return nil
	}
	policy.issue(p, now)
	return saveContributorProfileTokenCAS(p, false, true)
}

func (policy contributorTokenPolicy) expired(p *ContributorProfile, now time.Time) bool {
	if p.TokenIssuedAt.IsZero() || p.TokenExpiresAt.IsZero() {
		return true
	}
	// A policy reduction takes effect immediately; increasing it never extends an
	// already-issued credential beyond its stored expiry.
	expiry := p.TokenExpiresAt
	if cap := p.TokenIssuedAt.Add(policy.maxAge); cap.Before(expiry) {
		expiry = cap
	}
	if !now.Before(expiry) {
		return true
	}
	last := p.TokenLastUsedAt
	if last.IsZero() {
		last = p.TokenIssuedAt
	}
	return policy.idleMax > 0 && !now.Before(last.Add(policy.idleMax))
}

func (policy contributorTokenPolicy) rotationDue(p *ContributorProfile, now time.Time) bool {
	return p.RegistrationToken != "" && p.TrustTier != "revoked" && !p.TokenIssuedAt.IsZero() &&
		!now.Before(p.TokenIssuedAt.Add(policy.maxAge*4/5))
}

func authenticateContributorToken(token string, policy contributorTokenPolicy) (*ContributorProfile, string) {
	if token == "" {
		return nil, "Invalid registration token"
	}
	hash := sha256Hex(token)
	for _, profile := range listContributorProfiles() {
		p := profile
		if !secureCompare(p.RegistrationToken, hash) {
			continue
		}
		now := time.Now().UTC()
		if err := policy.migrate(&p, now); err != nil {
			return nil, "Token state changed; retry authentication"
		}
		if policy.expired(&p, now) {
			return nil, contributorTokenExpired
		}
		// A revoked contributor is returned, not rejected: callers audit the
		// refusal themselves (handshake, knowledge export, token status).
		policy.touchLastUsed(&p, hash, now)
		return &p, ""
	}
	return nil, "Invalid registration token"
}

func (s *Server) handleContributeTokenStatus(w http.ResponseWriter, r *http.Request) {
	p, reason := authenticateContributorToken(registrationTokenFromAuthorization(r), s.contributorTokenPolicy())
	if p == nil {
		jsonError(w, reason, http.StatusUnauthorized)
		return
	}
	if p.TrustTier == "revoked" {
		jsonError(w, "Access has been revoked", http.StatusUnauthorized)
		return
	}
	jsonResponse(w, map[string]any{"issued_at": p.TokenIssuedAt, "expires_at": p.TokenExpiresAt,
		"rotation_due": s.contributorTokenPolicy().rotationDue(p, time.Now().UTC())})
}

// registrationTokenSessionValid fences live sessions independently of profile refreshes.
func (h *ContributeWSHub) registrationTokenSessionValid(c *ContributorConnection) bool {
	c.mu.Lock()
	username, hash := c.profile.GitHubUsername, c.registrationTokenHash
	c.mu.Unlock()
	// Synthetic/internal connections without a registration credential are not
	// registration-token sessions. Real handshakes always bind their token hash.
	if hash == "" {
		return true
	}
	p, err := loadContributorProfile(username)
	if err != nil || p.RegistrationToken == "" || !secureCompare(hash, p.RegistrationToken) || p.TrustTier == "revoked" {
		return false
	}
	now := time.Now().UTC()
	policy := h.server.contributorTokenPolicy()
	if policy.expired(p, now) {
		return false
	}
	// A live, pong-responsive connection is activity.
	policy.touchLastUsed(p, hash, now)
	return true
}

// touchLastUsed records token activity. It is bookkeeping only and never fails
// authentication: it reloads the current profile, updates only TokenLastUsedAt
// while the same token hash is still stored, and treats a version conflict from
// a concurrent writer as a retry (then gives up silently). Writes are throttled
// to avoid persisting on every request, but never enough to consume the idle window.
func (policy contributorTokenPolicy) touchLastUsed(p *ContributorProfile, hash string, now time.Time) {
	interval := time.Minute
	if policy.idleMax > 0 && policy.idleMax/2 < interval {
		interval = policy.idleMax / 2
	}
	for attempt := 0; attempt < 3; attempt++ {
		cur, err := loadContributorProfile(p.GitHubUsername)
		if err != nil || cur.RegistrationToken == "" || !secureCompare(cur.RegistrationToken, hash) || now.Sub(cur.TokenLastUsedAt) < interval {
			return
		}
		base := cur.Version
		cur.TokenLastUsedAt = now
		err = saveContributorProfileTokenCAS(cur, false, true)
		if err == nil {
			p.TokenLastUsedAt = now
			// Only our own touch moved the version: keep the caller's copy
			// current so its later CAS saves do not conflict with the touch.
			if p.Version == base {
				p.Version = cur.Version
			}
			return
		}
		if !errors.Is(err, errProfileConflict) {
			return
		}
	}
}

// Token revocation preserves trust; GitHub-authenticated reissue restores access.
func revokeContributorToken(p *ContributorProfile) error {
	p.RegistrationToken = ""
	p.TokenPlain = ""
	return saveContributorProfileTokenCAS(p, false, true)
}

func (s *Server) handleContributorTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	id := r.PathValue("id")
	profiles := listContributorProfiles()
	if id != "" {
		p := findContributor(id)
		if p == nil {
			jsonError(w, "Contributor not found", http.StatusNotFound)
			return
		}
		profiles = []ContributorProfile{*p}
	}
	revoked := 0
	for i := range profiles {
		p := &profiles[i]
		if err := revokeContributorToken(p); err != nil {
			jsonError(w, fmt.Sprintf("Revocation stopped after %d profiles; retry: %v", revoked, err), http.StatusConflict)
			return
		}
		revoked++
		if s.contributeHub != nil {
			s.contributeHub.DisconnectContributor(p.ContributorID, "Registration token revoked; run hivectl contribute login")
		}
	}
	jsonResponse(w, map[string]any{"revoked": revoked})
}
