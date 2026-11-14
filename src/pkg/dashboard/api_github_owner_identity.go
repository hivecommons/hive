package dashboard

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// The hive owner's GitHub identity for write-tier reference repos
// (hivecommons/hive#18197).
//
// A reference repo is outside the hive's org, so the hive's App installation
// token is never honoured there; the remote lane writes as the hive OWNER
// instead. These owner-only routes run a device flow with the spoke's
// existing OAuth client asking for github.OwnerIdentityScope, check the
// token's login against the signed-in owner, and keep {login, token, scope,
// connected_at} in a 0600 file on the spoke (github.OwnerIdentityFile). The
// token never leaves the spoke and never appears in a response.
//
// This is NOT dashboard login: /api/gh-user-auth/* stays scope-free (#1927)
// and shares no state with this flow.

// ownerIdentityFlow is the in-progress consent flow, one per spoke.
type ownerIdentityFlow struct {
	mu    sync.Mutex
	state *github.DeviceFlowState
	// id binds the flow to the caller who started it, as the login flow does.
	id string
}

// Seams for tests; production talks to GitHub.
var (
	ownerIdentityStartFlow = github.StartDeviceFlowWithScope
	ownerIdentityPollFlow  = github.PollDeviceFlowWithScope
	ownerIdentityValidate  = github.ValidateTokenWithContext
)

// githubOwnerIdentityResponse is the GET/DELETE body and the poll's success
// body: the identity without its token.
type githubOwnerIdentityResponse struct {
	github.OwnerIdentityStatus
	// ScopeRequested is the scope a connect asks for, so Settings can say so.
	ScopeRequested string `json:"scope_requested"`
}

// ownerIdentityStore is the store boot shares, or one over the default path
// when none was wired (tests, a dashboard without a proxy).
func (s *Server) ownerIdentityStore() *github.OwnerIdentityStore {
	if s.deps != nil && s.deps.GitHubOwnerIdentity != nil {
		return s.deps.GitHubOwnerIdentity
	}
	return github.NewOwnerIdentityStore(github.OwnerIdentityFile(), nil)
}

func (s *Server) ownerIdentityChanged() {
	if s.deps != nil && s.deps.GitHubOwnerIdentityChanged != nil {
		s.deps.GitHubOwnerIdentityChanged()
	}
}

func ownerIdentityBody(st github.OwnerIdentityStatus) githubOwnerIdentityResponse {
	return githubOwnerIdentityResponse{OwnerIdentityStatus: st, ScopeRequested: github.OwnerIdentityScope}
}

// handleGitHubOwnerIdentityGet reports whether the owner's account is
// connected, as which login, and whether GitHub still accepts its token.
func (s *Server) handleGitHubOwnerIdentityGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	jsonResponse(w, ownerIdentityBody(s.ownerIdentityStore().Status()))
}

// handleGitHubOwnerIdentityStart begins the consent device flow.
func (s *Server) handleGitHubOwnerIdentityStart(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	gc := s.deps.Config.GitHub
	f := &s.ownerIdentityFlow
	f.mu.Lock()
	defer f.mu.Unlock()
	state, err := ownerIdentityStartFlow(gc.OAuthClientIDResolved(), github.OwnerIdentityScope, gc.OAuthBaseURL(), gc.OAuthAPIURL())
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	flowID, err := newDeviceFlowID()
	if err != nil {
		jsonError(w, "failed to start device flow", http.StatusInternalServerError)
		return
	}
	f.state, f.id = state, flowID
	s.auditFromRequest(r, "github_owner_identity_start", auditDetail("scope", github.OwnerIdentityScope), "")
	jsonResponse(w, map[string]interface{}{
		"user_code":        state.UserCode,
		"verification_uri": state.VerificationURI,
		"expires_in":       state.ExpiresIn,
		"interval":         state.Interval,
		"flow_id":          flowID,
		"scope":            github.OwnerIdentityScope,
	})
}

// handleGitHubOwnerIdentityPoll completes the flow: on approval it checks the
// granted scope and the token's login, then stores the identity and
// re-resolves the reference repos.
func (s *Server) handleGitHubOwnerIdentityPoll(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		FlowID string `json:"flow_id"`
	}
	_ = decodeBody(r, &body)

	const noFlow = "no GitHub account connection in progress — call /api/github-owner-identity/start first"
	f := &s.ownerIdentityFlow
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.state == nil || subtle.ConstantTimeCompare([]byte(body.FlowID), []byte(f.id)) != 1 {
		jsonError(w, noFlow, http.StatusBadRequest)
		return
	}
	gc := s.deps.Config.GitHub
	token, status, scope, err := ownerIdentityPollFlow(gc.OAuthClientIDResolved(), f.state.DeviceCode, gc.OAuthBaseURL(), gc.OAuthAPIURL())
	if err != nil {
		f.state, f.id = nil, ""
		jsonResponse(w, map[string]interface{}{"status": "error", "error": err.Error()})
		return
	}
	switch status {
	case "authorization_pending":
		jsonResponse(w, map[string]interface{}{"status": "pending"})
		return
	case "slow_down":
		jsonResponse(w, map[string]interface{}{"status": "slow_down"})
		return
	}
	f.state, f.id = nil, ""
	fail := func(reason string) {
		s.auditFromRequest(r, "github_owner_identity_rejected", auditDetail("reason", reason), "")
		jsonResponse(w, map[string]interface{}{"status": "error", "error": reason})
	}
	if !github.OwnerIdentityScopeGranted(scope) {
		fail("GitHub did not grant the " + github.OwnerIdentityScope + " scope; approve the requested access to connect")
		return
	}
	user, err := ownerIdentityValidate(r.Context(), token, gc.OAuthAPIURL())
	if err != nil || user == nil || user.Login == "" {
		fail("could not verify the GitHub identity of the approved token")
		return
	}
	// The account must be the signed-in owner's own, when the dashboard
	// knows who that is: an owner must not connect someone else's account.
	if viewer := s.resolveViewerUsername(r); viewer != "" && !strings.EqualFold(viewer, user.Login) {
		fail("the approved GitHub account (" + user.Login + ") is not the signed-in owner (" + viewer + "); sign in to GitHub as " + viewer + " and connect again")
		return
	}
	id := github.OwnerIdentity{Login: user.Login, Token: token, Scope: scope, ConnectedAt: time.Now().UTC()}
	if err := s.ownerIdentityStore().Save(id); err != nil {
		if s.logger != nil {
			s.logger.Error("failed to store the owner GitHub identity", "error", err)
		}
		jsonResponse(w, map[string]interface{}{"status": "error", "error": "failed to store the GitHub identity on the hive"})
		return
	}
	s.auditFromRequest(r, "github_owner_identity_connect", auditDetail("login", user.Login, "scope", scope), "")
	if s.logger != nil {
		s.logger.Info("owner GitHub identity connected for write-tier reference repos", "login", user.Login)
	}
	s.ownerIdentityChanged()
	jsonResponse(w, map[string]interface{}{"status": "complete", "identity": ownerIdentityBody(s.ownerIdentityStore().Status())})
}

// handleGitHubOwnerIdentityDelete disconnects: the file is removed and the
// reference repos are re-resolved without it.
func (s *Server) handleGitHubOwnerIdentityDelete(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	store := s.ownerIdentityStore()
	prev := store.Status()
	if err := store.Remove(); err != nil {
		if s.logger != nil {
			s.logger.Error("failed to remove the owner GitHub identity", "error", err)
		}
		jsonError(w, "failed to remove the GitHub identity", http.StatusInternalServerError)
		return
	}
	s.auditFromRequest(r, "github_owner_identity_disconnect", auditDetail("login", prev.Login), "")
	s.ownerIdentityChanged()
	jsonResponse(w, ownerIdentityBody(store.Status()))
}
