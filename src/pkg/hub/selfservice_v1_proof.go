package hub

// Hub registration protocol v1, rollout step 2: ownership proof methods chosen
// per request (hivecommons/hive#18642, #19056, #19238).
//
// A v1 create or PATCH picks its proof with the X-Hive-Proof-Method header.
// Without it the hub proves ownership as before (hub-session-token: the
// GitHub token stored at hub sign-in). With
//
//	X-Hive-Proof-Method: github-user-token
//	X-Hive-Proof-Token: <a GitHub token>
//
// the hub proves ownership with the token sent on THIS request only. That is
// the RFC's fix for orgs that restrict OAuth apps: the sign-in requests no
// scope, so GitHub hides the admin role from the stored token, while a token
// the user makes with the scopes the repos need shows it.
//
// Invariants:
//   - Off by default. github-user-token is accepted and advertised only while
//     HIVE_HUB_PROOF_GITHUB_USER_TOKEN is on (and the registration API itself
//     is on). With it off, asking for the method is a 400 that lists what the
//     hub offers, never a silent fallback to the stored token.
//   - The per-request token is used for the reads below and then dropped. It
//     is never stored, logged, echoed in a response or compared with the
//     stored sign-in token, and the stored token stays scopeless.
//   - The token must belong to the signed-in GitHub account (GET /user), so a
//     token someone else made cannot prove ownership for this login.
//   - Read-only: GET /user and GET /repos/{owner}/{repo}. Nothing writes to
//     GitHub.
//   - Every refusal names the repo, the method and a reason code, and tells
//     apart "the token cannot see the admin role" from "you are not an admin".
//   - The v6 /api/hub/selfservice routes never read these headers.
//
// The RFC's other methods are not offered here: repo-file is deferred by the
// owner decision on #18642, and github-app-installation needs the hub's App
// installation lookup, which is a separate change.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"unicode"
)

const (
	// hubV1ProofGitHubUserToken proves ownership with a GitHub token the
	// caller sends on the request.
	hubV1ProofGitHubUserToken = "github-user-token"
	// hubV1ProofGitHubUserTokenEnv turns the github-user-token proof on.
	hubV1ProofGitHubUserTokenEnv = "HIVE_HUB_PROOF_GITHUB_USER_TOKEN"

	// hubV1ProofMethodHeader picks the proof method for one request.
	hubV1ProofMethodHeader = "X-Hive-Proof-Method"
	// hubV1ProofTokenHeader carries the github-user-token token. It is a
	// header, not a body field, so it never lands in a stored or echoed body.
	hubV1ProofTokenHeader = "X-Hive-Proof-Token"
	// hubV1ProofTokenMaxLen bounds the token header. GitHub tokens are far
	// shorter; anything longer is not one.
	hubV1ProofTokenMaxLen = 255

	// hubV1ScopeRepo is the classic OAuth scope GitHub needs to show a private
	// repo, and the caller's role on it, to a token.
	hubV1ScopeRepo = "repo"
)

// Reason codes in a github-user-token refusal body and its audit line.
const (
	hubV1ProofReasonTokenRejected    = "token_rejected"
	hubV1ProofReasonTokenUser        = "token_user_mismatch"
	hubV1ProofReasonSSO              = "sso_not_authorized"
	hubV1ProofReasonRepoNotVisible   = "repo_not_visible"
	hubV1ProofReasonMissingScope     = "missing_scope"
	hubV1ProofReasonRoleNotVisible   = "admin_role_not_visible"
	hubV1ProofReasonNotAdmin         = "not_admin"
	selfServiceRefusedProofTokenFail = "proof_token_refused"
)

// hubV1Proof is the proof method a request picked. The zero value is the
// hub-session-token proof.
type hubV1Proof struct {
	method string
	token  string
}

func hubV1GitHubUserTokenEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(hubV1ProofGitHubUserTokenEnv))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// hubV1ProofMethods is the proof methods this hub accepts right now, in the
// order discovery lists them.
func hubV1ProofMethods() []string {
	methods := []string{hubV1ProofHubSessionToken}
	if hubV1GitHubUserTokenEnabled() {
		methods = append(methods, hubV1ProofGitHubUserToken)
	}
	return methods
}

// hubV1ProofMethodInfo is one discovery entry describing how to use a method.
type hubV1ProofMethodInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Headers are the request headers the method reads.
	Headers []string `json:"headers,omitempty"`
	// Scopes are the classic OAuth scopes a github-user-token needs.
	Scopes *hubV1ProofScopes `json:"scopes,omitempty"`
}

// hubV1ProofScopes says, per kind of repo, which classic OAuth scopes the
// per-request token needs. A fine-grained token needs read access to the
// repos (the Metadata permission) instead.
type hubV1ProofScopes struct {
	PublicRepos  []string `json:"public_repos"`
	PrivateRepos []string `json:"private_repos"`
	FineGrained  string   `json:"fine_grained"`
}

func hubV1ProofMethodDetails() []hubV1ProofMethodInfo {
	details := []hubV1ProofMethodInfo{{
		Name:        hubV1ProofHubSessionToken,
		Description: "the default: the hub checks your admin role on each repo with the GitHub sign-in it already holds",
	}}
	if hubV1GitHubUserTokenEnabled() {
		details = append(details, hubV1ProofMethodInfo{
			Name:        hubV1ProofGitHubUserToken,
			Description: "send a GitHub token of your own on this request; the hub reads your admin role with it and does not store it",
			Headers:     []string{hubV1ProofMethodHeader, hubV1ProofTokenHeader},
			Scopes: &hubV1ProofScopes{
				PublicRepos:  []string{},
				PrivateRepos: []string{hubV1ScopeRepo},
				FineGrained:  "read access (Metadata) to every repo in the request",
			},
		})
	}
	return details
}

// writeHubV1ProofMethodError is the 400 for a proof method the hub does not
// accept, listing the ones it does.
func writeHubV1ProofMethodError(w http.ResponseWriter, msg string) {
	writeHubV1JSON(w, http.StatusBadRequest, "", map[string]any{
		"error":         msg,
		"proof_methods": hubV1ProofMethods(),
	})
}

// hubV1ReadProof reads the proof method a v1 write asked for. It writes the
// refusal itself and returns ok=false when the request is not usable. It makes
// no GitHub call.
func hubV1ReadProof(w http.ResponseWriter, r *http.Request) (hubV1Proof, bool) {
	method := strings.ToLower(strings.TrimSpace(r.Header.Get(hubV1ProofMethodHeader)))
	token := r.Header.Get(hubV1ProofTokenHeader)
	switch method {
	case "", hubV1ProofHubSessionToken:
		if token != "" {
			writeHubV1ProofMethodError(w, fmt.Sprintf("%s is read only with %s: %s; send that header too, or drop the token", hubV1ProofTokenHeader, hubV1ProofMethodHeader, hubV1ProofGitHubUserToken))
			return hubV1Proof{}, false
		}
		return hubV1Proof{method: hubV1ProofHubSessionToken}, true
	case hubV1ProofGitHubUserToken:
		if !hubV1GitHubUserTokenEnabled() {
			writeHubV1ProofMethodError(w, fmt.Sprintf("this hub does not offer the %s proof", hubV1ProofGitHubUserToken))
			return hubV1Proof{}, false
		}
		token = strings.TrimSpace(token)
		if token == "" {
			writeHubV1ProofMethodError(w, fmt.Sprintf("the %s proof needs a GitHub token in %s", hubV1ProofGitHubUserToken, hubV1ProofTokenHeader))
			return hubV1Proof{}, false
		}
		if !hubV1PlausibleToken(token) {
			// The token is never echoed, not even in a refusal.
			writeHubV1ProofMethodError(w, fmt.Sprintf("%s does not hold a GitHub token", hubV1ProofTokenHeader))
			return hubV1Proof{}, false
		}
		return hubV1Proof{method: hubV1ProofGitHubUserToken, token: token}, true
	default:
		writeHubV1ProofMethodError(w, fmt.Sprintf("unknown proof method in %s; this hub accepts %s", hubV1ProofMethodHeader, strings.Join(hubV1ProofMethods(), ", ")))
		return hubV1Proof{}, false
	}
}

// hubV1PlausibleToken rejects header values that cannot be a GitHub token
// before any of them reaches GitHub.
func hubV1PlausibleToken(token string) bool {
	if len(token) > hubV1ProofTokenMaxLen {
		return false
	}
	for _, c := range token {
		if c > unicode.MaxASCII || unicode.IsSpace(c) || unicode.IsControl(c) {
			return false
		}
	}
	return true
}

// hubV1ProveOwnership proves login administers every repo in repos with the
// method proof names. It writes the refusal itself and returns false when any
// repo fails.
func (s *HubServer) hubV1ProveOwnership(w http.ResponseWriter, r *http.Request, login, id string, user *SaaSUser, repos []string, proof hubV1Proof) bool {
	if proof.method != hubV1ProofGitHubUserToken {
		return s.selfServiceProveOwnership(w, r, login, id, user, repos)
	}
	return s.hubV1ProveWithUserToken(w, r, login, id, user, repos, proof.token)
}

// hubV1TokenUser is what GET /user said about a per-request token.
type hubV1TokenUser struct {
	status      int
	login       string
	scopes      []string
	scopesKnown bool
}

// tokenUser asks GitHub whose token this is. A 401 or 403 is a result, not an
// error; any other non-2xx is an error, reported by status only.
func (c *selfServiceOwnershipChecker) tokenUser(ctx context.Context, token string) (hubV1TokenUser, error) {
	var res hubV1TokenUser
	u, err := url.JoinPath(c.apiBase, "user")
	if err != nil {
		return res, fmt.Errorf("build GitHub user URL: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return res, fmt.Errorf("build GitHub token check: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.client.Do(req)
	if err != nil {
		return res, fmt.Errorf("GitHub token check failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	res.status = resp.StatusCode
	res.scopes, res.scopesKnown = selfServiceTokenScopes(resp.Header)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, selfServiceOwnershipErrorBodyLimit))
		return res, nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, selfServiceOwnershipErrorBodyLimit))
		return res, fmt.Errorf("GitHub token check returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return res, fmt.Errorf("decode GitHub token check: %w", err)
	}
	res.login = strings.TrimSpace(payload.Login)
	return res, nil
}

// hubV1ProofRefusal is the body of a failed github-user-token proof.
type hubV1ProofRefusal struct {
	Error          string   `json:"error"`
	ProofMethod    string   `json:"proof_method"`
	Reason         string   `json:"reason"`
	Repo           string   `json:"repo,omitempty"`
	RequiredScopes []string `json:"required_scopes,omitempty"`
	// GrantedScopes is set only when GitHub reported the token's scopes.
	GrantedScopes []string `json:"granted_scopes,omitempty"`
}

func (s *HubServer) writeHubV1ProofRefusal(w http.ResponseWriter, login, id string, status int, body hubV1ProofRefusal) {
	body.ProofMethod = hubV1ProofGitHubUserToken
	extra := []any{"proof_method", hubV1ProofGitHubUserToken, "proof_reason", body.Reason}
	if body.Repo != "" {
		extra = append(extra, "repo", body.Repo)
	}
	s.auditSelfServiceRefused(login, id, selfServiceRefusedProofTokenFail, extra...)
	writeHubV1JSON(w, status, "", body)
}

// hubV1ProveWithUserToken is the github-user-token proof: the token must be
// the signed-in GitHub account's, and must show permissions.admin on every
// repo.
func (s *HubServer) hubV1ProveWithUserToken(w http.ResponseWriter, r *http.Request, login, id string, user *SaaSUser, repos []string, token string) bool {
	githubLogin := topRepoProfileLogin(user)
	if githubLogin == "" {
		s.auditSelfServiceRefused(login, id, selfServiceRefusedNoGitHubLogin)
		writeJSONError(w, http.StatusBadRequest, "a GitHub identity is required to register a self-hosted hive")
		return false
	}
	checker := s.ownershipChecker()
	who, err := checker.tokenUser(r.Context(), token)
	if err != nil {
		s.auditSelfServiceRefused(login, id, selfServiceRefusedOwnershipError, "proof_method", hubV1ProofGitHubUserToken)
		s.logger.Warn("self-service registration: token check failed", "user", login, "hive", id, "proof_method", hubV1ProofGitHubUserToken, "error", err)
		writeJSONError(w, http.StatusBadGateway, "could not check the token in "+hubV1ProofTokenHeader+" with GitHub; try again")
		return false
	}
	if who.status == http.StatusUnauthorized || who.status == http.StatusForbidden || who.login == "" {
		s.writeHubV1ProofRefusal(w, login, id, http.StatusForbidden, hubV1ProofRefusal{
			Error:  "GitHub rejected the token in " + hubV1ProofTokenHeader + " (expired, revoked or mistyped); make a new token and retry",
			Reason: hubV1ProofReasonTokenRejected,
		})
		return false
	}
	if !strings.EqualFold(who.login, githubLogin) {
		// Neither login is the secret; naming the signed-in one helps the
		// caller see which account the token must come from.
		s.writeHubV1ProofRefusal(w, login, id, http.StatusForbidden, hubV1ProofRefusal{
			Error:  fmt.Sprintf("the token in %s belongs to a different GitHub account; make it while signed in to GitHub as %s", hubV1ProofTokenHeader, githubLogin),
			Reason: hubV1ProofReasonTokenUser,
		})
		return false
	}

	for _, repo := range repos {
		owner, name, _ := strings.Cut(repo, "/")
		res, err := checker.repoAdminDetail(r.Context(), token, owner, name)
		if err != nil {
			s.auditSelfServiceRefused(login, id, selfServiceRefusedOwnershipError, "repo", repo, "proof_method", hubV1ProofGitHubUserToken)
			s.logger.Warn("self-service registration: ownership check failed", "user", login, "hive", id, "repo", repo, "proof_method", hubV1ProofGitHubUserToken, "error", err)
			writeJSONError(w, http.StatusBadGateway, fmt.Sprintf("could not verify your permission on %s; try again", repo))
			return false
		}
		if res.admin {
			continue
		}
		status, body := hubV1UserTokenRefusal(repo, res)
		s.writeHubV1ProofRefusal(w, login, id, status, body)
		return false
	}
	s.logger.Info("audit: selfservice_ownership_proven", "user", login, "hive", id, "proof_method", hubV1ProofGitHubUserToken, "repos", len(repos))
	return true
}

// hubV1UserTokenRefusal turns a failed ownership read into a refusal that says
// why it failed. It never includes anything GitHub sent back except the
// token's scope names.
func hubV1UserTokenRefusal(repo string, res selfServiceRepoAdminResult) (int, hubV1ProofRefusal) {
	body := hubV1ProofRefusal{Repo: repo}
	if res.scopesKnown {
		body.GrantedScopes = res.scopes
	}
	switch {
	case res.status == http.StatusUnauthorized:
		body.Reason = hubV1ProofReasonTokenRejected
		body.Error = "GitHub rejected the token in " + hubV1ProofTokenHeader + " (expired, revoked or mistyped); make a new token and retry"
	case res.status == http.StatusForbidden && res.ssoRequired:
		body.Reason = hubV1ProofReasonSSO
		body.Error = fmt.Sprintf("the token is not authorized for the single sign-on of the org that owns %s; authorize it for that org on GitHub and retry", repo)
	case res.status == http.StatusForbidden || res.status == http.StatusNotFound:
		if res.scopesKnown && !hubV1HasScope(res.scopes, hubV1ScopeRepo) {
			body.Reason = hubV1ProofReasonMissingScope
			body.RequiredScopes = []string{hubV1ScopeRepo}
			body.Error = fmt.Sprintf("the token cannot see %s; a private repo needs a token with the %s scope", repo, hubV1ScopeRepo)
		} else {
			body.Reason = hubV1ProofReasonRepoNotVisible
			body.Error = fmt.Sprintf("the token cannot see %s; check the repo name and that the token has access to it (a fine-grained token must list the repo)", repo)
		}
	case !res.permissionsReported:
		body.Reason = hubV1ProofReasonRoleNotVisible
		body.Error = fmt.Sprintf("the token can see %s but GitHub did not show your role on it (is the org restricting access for this token?)", repo)
	default:
		body.Reason = hubV1ProofReasonNotAdmin
		body.Error = fmt.Sprintf("your GitHub account is not an admin of %s", repo)
	}
	return http.StatusForbidden, body
}

func hubV1HasScope(scopes []string, want string) bool {
	for _, scope := range scopes {
		if strings.EqualFold(scope, want) {
			return true
		}
	}
	return false
}
