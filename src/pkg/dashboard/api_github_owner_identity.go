package dashboard

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// The hive owner's GitHub identity for remote repos (hivecommons/hive#18197).
//
// Repositories the hive does not manage (project.reference_repos at the write
// tier) are written to as the hive OWNER, never as the App. These four
// owner-only routes are the one place that identity is created and removed:
// a device flow with the spoke's own OAuth client, asking GitHub for
// github.OwnerIdentityScope, whose token is stored by github.OwnerIdentityStore
// on the spoke only. This is deliberately NOT the dashboard login flow
// (api_auth.go): login stays scope-free (#1927) and never persists a token;
// this consent asks for repository access and persists exactly one token, the
// owner's, after the owner has already proved who they are to this dashboard.

// Device-flow seams, swapped by tests; production is the GitHub client.
var (
	ownerIdentityStartFlow = github.StartDeviceFlowWithScope
	ownerIdentityPollFlow  = github.PollDeviceFlowWithScope
	ownerIdentityValidate  = github.ValidateTokenWithContext
)

// ownerIdentityFlowTTL bounds how long a started consent flow waits for its
// poll before a new start is required.
const ownerIdentityFlowTTL = 15 * time.Minute

// ownerIdentityFlow is one in-progress consent: the GitHub device state, the
// per-flow secret the starting caller must present on every poll, who started
// it, and when.
type ownerIdentityFlow struct {
	state     *github.DeviceFlowState
	flowID    string
	startedBy string
	startedAt time.Time
}

// ownerIdentityResponse is the GET body and the terminal poll body.
type ownerIdentityResponse struct {
	github.OwnerIdentityStatus
	// ScopeRequired is the OAuth scope the consent asks for.
	ScopeRequired string `json:"scope_required"`
	// PrivateReposSupported is false: public_repo covers public repositories
	// only; private remote repos would need the wider "repo" scope.
	PrivateReposSupported bool `json:"private_repos_supported"`
	// File is the identity file path, so the owner can see where the token
	// lives on the spoke (never its value).
	File string `json:"file,omitempty"`
}

func (s *Server) ownerIdentityStore(w http.ResponseWriter) *github.OwnerIdentityStore {
	if s.deps == nil || s.deps.OwnerIdentity == nil {
		jsonError(w, "owner identity store unavailable on this hive", http.StatusServiceUnavailable)
		return nil
	}
	return s.deps.OwnerIdentity
}

func ownerIdentityResponseFor(store *github.OwnerIdentityStore) ownerIdentityResponse {
	return ownerIdentityResponse{
		OwnerIdentityStatus: store.Status(),
		ScopeRequired:       github.OwnerIdentityScope,
		File:                store.Path(),
	}
}

// handleOwnerIdentityGet reports the connected identity (never the token).
func (s *Server) handleOwnerIdentityGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	store := s.ownerIdentityStore(w)
	if store == nil {
		return
	}
	jsonResponse(w, ownerIdentityResponseFor(store))
}

// handleOwnerIdentityStart begins the consent: a device flow asking for
// github.OwnerIdentityScope. The response carries the user code and URL to
// show the owner, and the flow_id the poll must present.
func (s *Server) handleOwnerIdentityStart(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.ownerIdentityStore(w) == nil {
		return
	}
	if s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	gh := s.deps.Config.GitHub
	state, err := ownerIdentityStartFlow(gh.OAuthClientIDResolved(), github.OwnerIdentityScope, gh.OAuthBaseURL(), gh.OAuthAPIURL())
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	flowID, err := newDeviceFlowID()
	if err != nil {
		jsonError(w, "failed to start device flow", http.StatusInternalServerError)
		return
	}
	s.ownerIdentityFlowMu.Lock()
	s.ownerIdentityFlow = &ownerIdentityFlow{state: state, flowID: flowID, startedBy: r.Header.Get("X-Hive-User"), startedAt: time.Now()}
	s.ownerIdentityFlowMu.Unlock()
	s.auditFromRequest(r, "github_owner_identity_start", auditDetail("scope", github.OwnerIdentityScope), "")
	jsonResponse(w, map[string]any{
		"user_code":        state.UserCode,
		"verification_uri": state.VerificationURI,
		"expires_in":       state.ExpiresIn,
		"interval":         state.Interval,
		"flow_id":          flowID,
		"scope":            github.OwnerIdentityScope,
	})
}

// handleOwnerIdentityPoll polls the consent. On completion it validates the
// token, checks the granted scope and that the account is the logged-in
// owner's (when the request carries a user), stores the identity and tells
// the reference-repo wiring to re-resolve. Terminal bodies carry status
// "complete" or "error"; "pending" and "slow_down" mean poll again.
func (s *Server) handleOwnerIdentityPoll(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	store := s.ownerIdentityStore(w)
	if store == nil {
		return
	}
	var pollReq struct {
		FlowID string `json:"flow_id"`
	}
	_ = decodeBody(r, &pollReq)

	s.ownerIdentityFlowMu.Lock()
	defer s.ownerIdentityFlowMu.Unlock()
	flow := s.ownerIdentityFlow
	if flow == nil || time.Since(flow.startedAt) > ownerIdentityFlowTTL {
		s.ownerIdentityFlow = nil
		jsonError(w, "no owner identity consent in progress — call /api/github-owner-identity/start first", http.StatusBadRequest)
		return
	}
	if subtle.ConstantTimeCompare([]byte(pollReq.FlowID), []byte(flow.flowID)) != 1 {
		jsonError(w, "no owner identity consent in progress — call /api/github-owner-identity/start first", http.StatusBadRequest)
		return
	}
	gh := s.deps.Config.GitHub
	token, status, granted, err := ownerIdentityPollFlow(gh.OAuthClientIDResolved(), flow.state.DeviceCode, gh.OAuthBaseURL(), gh.OAuthAPIURL())
	if err != nil {
		s.ownerIdentityFlow = nil
		jsonResponse(w, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	switch status {
	case "authorization_pending":
		jsonResponse(w, map[string]any{"status": "pending"})
		return
	case "slow_down":
		jsonResponse(w, map[string]any{"status": "slow_down"})
		return
	}
	// Terminal: the flow is spent whatever happens next.
	s.ownerIdentityFlow = nil
	fail := func(msg string) {
		s.auditFromRequest(r, "github_owner_identity_rejected", auditDetail("reason", msg), "")
		jsonResponse(w, map[string]any{"status": "error", "error": msg})
	}
	if !github.ScopeGrantsPublicRepo(granted) {
		fail("GitHub granted scope " + strings.TrimSpace(granted) + "; the hive needs " + github.OwnerIdentityScope + " to open issues and PRs on public repositories it does not manage")
		return
	}
	user, err := ownerIdentityValidate(r.Context(), token, gh.OAuthAPIURL())
	if err != nil || user == nil || user.Login == "" {
		fail("could not verify the GitHub account the token belongs to")
		return
	}
	// The consent is the OWNER's: a logged-in owner may only connect their
	// own account, never someone else's token they happen to hold.
	if by := strings.TrimSpace(flow.startedBy); by != "" && by != "local" && !strings.EqualFold(by, user.Login) {
		fail("the authorized GitHub account (" + user.Login + ") is not the logged-in owner (" + by + "); connect your own account")
		return
	}
	if err := store.Save(github.OwnerIdentity{Login: user.Login, Token: token, Scope: strings.TrimSpace(granted), ConnectedAt: time.Now().UTC()}); err != nil {
		if s.logger != nil {
			s.logger.Error("owner identity: store failed", "error", err)
		}
		fail("failed to store the owner identity on this hive: " + err.Error())
		return
	}
	s.auditFromRequest(r, "github_owner_identity_connected", auditDetail("login", user.Login, "scope", strings.TrimSpace(granted)), "")
	if s.logger != nil {
		s.logger.Info("owner GitHub identity connected for remote repos", "login", user.Login, "scope", granted)
	}
	s.notifyOwnerIdentityChanged()
	resp := ownerIdentityResponseFor(store)
	jsonResponse(w, map[string]any{"status": "complete", "login": user.Login, "identity": resp})
}

// handleOwnerIdentityDelete disconnects the account: the token file is
// removed and every write-tier reference repo drops to read-only on the next
// pass. GitHub-side revocation is the owner's own step (Settings →
// Applications on github.com); the response says so.
func (s *Server) handleOwnerIdentityDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	store := s.ownerIdentityStore(w)
	if store == nil {
		return
	}
	prev := store.Status()
	if err := store.Clear(); err != nil {
		jsonError(w, "failed to remove the owner identity: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.ownerIdentityFlowMu.Lock()
	s.ownerIdentityFlow = nil
	s.ownerIdentityFlowMu.Unlock()
	s.auditFromRequest(r, "github_owner_identity_disconnected", auditDetail("login", prev.Login), "")
	if s.logger != nil {
		s.logger.Info("owner GitHub identity disconnected", "login", prev.Login)
	}
	s.notifyOwnerIdentityChanged()
	jsonResponse(w, map[string]any{
		"status":   "disconnected",
		"login":    prev.Login,
		"identity": ownerIdentityResponseFor(store),
		"note":     "The token was removed from this hive. To revoke it on GitHub as well, remove the authorization under Settings → Applications on github.com.",
	})
}

// notifyOwnerIdentityChanged runs the wiring hook so reference-repo policies
// re-resolve against the new identity without a restart.
func (s *Server) notifyOwnerIdentityChanged() {
	if s.deps != nil && s.deps.OwnerIdentityChanged != nil {
		s.deps.OwnerIdentityChanged()
	}
}
