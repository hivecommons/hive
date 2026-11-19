package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// Tests for the github-user-token ownership proof (#18642, #19056, #19238).
// They reuse the v1 test env but swap its GitHub stub for one that knows
// several per-request tokens as well as the stored sign-in token.

const (
	proofTestGoodToken    = "ghp_proof_good_do_not_leak"
	proofTestNoScopeToken = "ghp_proof_noscope_do_not_leak"
	proofTestFineToken    = "github_pat_proof_fine_do_not_leak"
	proofTestBobToken     = "ghp_proof_bob_do_not_leak"
	proofTestDeadToken    = "ghp_proof_revoked_do_not_leak"
)

// proofStub answers like GitHub for the tokens above. alice/orgrepo is the
// restricted-OAuth-org case: the stored sign-in token sees the repo but not
// its permissions, while the per-request token sees admin.
type proofStub struct {
	calls  atomic.Int64
	writes atomic.Int64
}

func (p *proofStub) serve(w http.ResponseWriter, r *http.Request) {
	p.calls.Add(1)
	if r.Method != http.MethodGet {
		p.writes.Add(1)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	type identity struct {
		login  string
		scopes *string
	}
	repoScope, noScope := "repo, read:org", ""
	tokens := map[string]identity{
		selfServiceTestToken:  {login: selfServiceTestUser, scopes: &noScope},
		proofTestGoodToken:    {login: selfServiceTestUser, scopes: &repoScope},
		proofTestNoScopeToken: {login: selfServiceTestUser, scopes: &noScope},
		proofTestFineToken:    {login: selfServiceTestUser},
		proofTestBobToken:     {login: selfServiceTestOther, scopes: &repoScope},
	}
	id, ok := tokens[token]
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if id.scopes != nil {
		w.Header().Set("X-OAuth-Scopes", *id.scopes)
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/user" {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": id.login})
		return
	}
	repo := strings.TrimPrefix(r.URL.Path, "/repos/")
	perms := func(admin bool) {
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": repo, "permissions": map[string]bool{"admin": admin}})
	}
	switch {
	case token == selfServiceTestToken:
		switch repo {
		case "alice/lab", "alice/tools":
			perms(true)
		case "alice/orgrepo":
			_ = json.NewEncoder(w).Encode(map[string]any{"full_name": repo})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	case token == proofTestGoodToken || token == proofTestBobToken:
		switch repo {
		case "alice/lab", "alice/tools", "alice/orgrepo":
			perms(true)
		case "alice/readonly":
			perms(false)
		case "alice/restricted":
			_ = json.NewEncoder(w).Encode(map[string]any{"full_name": repo})
		case "alice/sso":
			w.Header().Set("X-GitHub-SSO", "required; url=https://github.com/orgs/x/sso")
			w.WriteHeader(http.StatusForbidden)
		case "alice/broken":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	default:
		// The no-scope and fine-grained tokens see no private repo.
		w.WriteHeader(http.StatusNotFound)
	}
}

func newProofTestEnv(t *testing.T, enabled bool) (*selfServiceTestEnv, *proofStub) {
	t.Helper()
	env := newHubV1TestEnv(t)
	if enabled {
		t.Setenv(hubV1ProofGitHubUserTokenEnv, "true")
	} else {
		t.Setenv(hubV1ProofGitHubUserTokenEnv, "")
	}
	stub := &proofStub{}
	srv := httptest.NewServer(http.HandlerFunc(stub.serve))
	t.Cleanup(srv.Close)
	env.s.selfServiceOwnership = &selfServiceOwnershipChecker{apiBase: srv.URL, client: srv.Client()}
	return env, stub
}

func proofHeaders(token string) map[string]string {
	return map[string]string{hubV1ProofMethodHeader: hubV1ProofGitHubUserToken, hubV1ProofTokenHeader: token}
}

func (env *selfServiceTestEnv) v1Create(t *testing.T, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return env.v1(t, selfServiceTestUser, http.MethodPost, hubV1BasePath+"/registrations", body, headers, env.s.handleHubV1Create)
}

func decodeProofRefusal(t *testing.T, rec *httptest.ResponseRecorder) hubV1ProofRefusal {
	t.Helper()
	var body hubV1ProofRefusal
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("refusal is not JSON: %q", rec.Body.String())
	}
	return body
}

const proofOrgRepoBody = `{"name":"Lab","org":"alice","repos":["alice/lab","alice/orgrepo"]}`

func TestHubV1DiscoveryAdvertisesUserTokenOnlyWhenEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		env, _ := newProofTestEnv(t, enabled)
		rec := httptest.NewRecorder()
		env.s.handleHubV1Discovery(rec, httptest.NewRequest(http.MethodGet, hubV1BasePath, nil))
		var d struct {
			ProofMethods []string               `json:"proof_methods"`
			Details      []hubV1ProofMethodInfo `json:"proof_method_details"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		want := []string{hubV1ProofHubSessionToken}
		if enabled {
			want = append(want, hubV1ProofGitHubUserToken)
		}
		if !reflect.DeepEqual(d.ProofMethods, want) || len(d.Details) != len(want) {
			t.Fatalf("enabled=%v: proof_methods %v, %d details", enabled, d.ProofMethods, len(d.Details))
		}
		if !enabled {
			continue
		}
		ut := d.Details[1]
		if ut.Name != hubV1ProofGitHubUserToken || ut.Scopes == nil ||
			!reflect.DeepEqual(ut.Scopes.PrivateRepos, []string{"repo"}) || len(ut.Scopes.PublicRepos) != 0 ||
			!reflect.DeepEqual(ut.Headers, []string{hubV1ProofMethodHeader, hubV1ProofTokenHeader}) {
			t.Fatalf("github-user-token details = %+v", ut)
		}
	}
}

func TestHubV1UserTokenProvesWhatTheStoredTokenCannot(t *testing.T) {
	env, stub := newProofTestEnv(t, true)

	// The stored sign-in token cannot see the role on alice/orgrepo.
	rec := env.v1Create(t, proofOrgRepoBody, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("stored-token proof: %d %s", rec.Code, rec.Body.String())
	}

	rec = env.v1Create(t, proofOrgRepoBody, proofHeaders(proofTestGoodToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("github-user-token proof: %d %s", rec.Code, rec.Body.String())
	}
	view := decodeV1Registration(t, rec)
	if view.ID != hubV1TestID || len(view.RepoProofs) != 2 || view.RepoProofs[1].CheckedAt.IsZero() {
		t.Fatalf("registration = %+v", view)
	}
	stored, _ := env.s.selfService.get(hubV1TestID)
	raw, _ := json.Marshal(stored)
	for where, text := range map[string]string{"response": rec.Body.String(), "logs": env.logs.String(), "store": string(raw)} {
		if strings.Contains(text, proofTestGoodToken) {
			t.Fatalf("the per-request token leaked into the %s", where)
		}
	}
	if !strings.Contains(env.logs.String(), "proof_method=github-user-token") {
		t.Fatal("the proof method is not in the audit log")
	}
	if stub.writes.Load() != 0 {
		t.Fatal("the proof sent a non-GET request to GitHub")
	}
}

func TestHubV1UserTokenOnPatchProvesOnlyAddedRepos(t *testing.T) {
	env, stub := newProofTestEnv(t, true)
	id, key := env.registerHappy(t)
	etag := env.etagOf(t, id)

	if rec := env.v1Patch(t, selfServiceTestUser, id, etag, `{"repos":{"add":["alice/orgrepo"]}}`); rec.Code != http.StatusForbidden {
		t.Fatalf("stored-token PATCH: %d %s", rec.Code, rec.Body.String())
	}
	before := stub.calls.Load()
	headers := proofHeaders(proofTestGoodToken)
	headers["If-Match"] = etag
	rec := env.v1(t, selfServiceTestUser, http.MethodPatch, hubV1BasePath+"/registrations/"+id, `{"repos":{"add":["alice/orgrepo"]}}`, headers, env.s.handleHubV1Patch)
	if rec.Code != http.StatusOK {
		t.Fatalf("github-user-token PATCH: %d %s", rec.Code, rec.Body.String())
	}
	// One GET /user and one GET /repos for the single added repo.
	if got := stub.calls.Load() - before; got != 2 {
		t.Fatalf("GitHub calls = %d, want 2", got)
	}
	if _, ok := env.s.verifyHeartbeatBearerGeneration(key, id); !ok {
		t.Fatal("the registration key stopped authenticating")
	}
	if strings.Contains(env.logs.String(), proofTestGoodToken) {
		t.Fatal("the per-request token leaked into the logs")
	}
}

func TestHubV1ProofMethodRequestRefusals(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		headers map[string]string
		want    string
	}{
		{"flag off", false, proofHeaders(proofTestGoodToken), "does not offer"},
		{"unknown method", true, map[string]string{hubV1ProofMethodHeader: "repo-file"}, "unknown proof method"},
		{"token without method", true, map[string]string{hubV1ProofTokenHeader: proofTestGoodToken}, "is read only with"},
		{"method without token", true, map[string]string{hubV1ProofMethodHeader: hubV1ProofGitHubUserToken}, "needs a GitHub token"},
		{"implausible token", true, proofHeaders("ghp_has a space"), "does not hold a GitHub token"},
		{"overlong token", true, proofHeaders(strings.Repeat("a", hubV1ProofTokenMaxLen+1)), "does not hold a GitHub token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, stub := newProofTestEnv(t, tc.enabled)
			rec := env.v1Create(t, selfServiceHappyBody, tc.headers)
			var body struct {
				Error        string   `json:"error"`
				ProofMethods []string `json:"proof_methods"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(body.Error, tc.want) || len(body.ProofMethods) == 0 {
				t.Fatalf("%d %s, want 400 containing %q and the offered methods", rec.Code, rec.Body.String(), tc.want)
			}
			if tok := tc.headers[hubV1ProofTokenHeader]; tok != "" && strings.Contains(rec.Body.String(), tok) {
				t.Fatal("the refusal echoed the token")
			}
			if stub.calls.Load() != 0 {
				t.Fatal("a refused proof request reached GitHub")
			}
			if _, ok := env.s.selfService.get(hubV1TestID); ok {
				t.Fatal("a refused proof request stored a registration")
			}
		})
	}
}

func TestHubV1UserTokenRefusalsSayWhy(t *testing.T) {
	cases := []struct {
		name          string
		token         string
		repo          string
		status        int
		reason        string
		want          string
		wantRequired  []string
		wantGranted   []string
		grantedAbsent bool
	}{
		{"revoked token", proofTestDeadToken, "alice/lab", http.StatusForbidden, hubV1ProofReasonTokenRejected, "GitHub rejected the token", nil, nil, true},
		{"someone else's token", proofTestBobToken, "alice/lab", http.StatusForbidden, hubV1ProofReasonTokenUser, "different GitHub account", nil, nil, true},
		{"not an admin", proofTestGoodToken, "alice/readonly", http.StatusForbidden, hubV1ProofReasonNotAdmin, "not an admin of alice/readonly", nil, []string{"repo", "read:org"}, false},
		{"role hidden by the org", proofTestGoodToken, "alice/restricted", http.StatusForbidden, hubV1ProofReasonRoleNotVisible, "did not show your role", nil, []string{"repo", "read:org"}, false},
		{"sso not authorized", proofTestGoodToken, "alice/sso", http.StatusForbidden, hubV1ProofReasonSSO, "single sign-on", nil, []string{"repo", "read:org"}, false},
		{"missing repo scope", proofTestNoScopeToken, "alice/lab", http.StatusForbidden, hubV1ProofReasonMissingScope, "needs a token with the repo scope", []string{"repo"}, nil, true},
		{"fine-grained token without the repo", proofTestFineToken, "alice/lab", http.StatusForbidden, hubV1ProofReasonRepoNotVisible, "must list the repo", nil, nil, true},
		{"unknown repo with full scopes", proofTestGoodToken, "alice/nowhere", http.StatusForbidden, hubV1ProofReasonRepoNotVisible, "cannot see alice/nowhere", nil, []string{"repo", "read:org"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, _ := newProofTestEnv(t, true)
			rec := env.v1Create(t, `{"name":"Lab","repos":["`+tc.repo+`"]}`, proofHeaders(tc.token))
			body := decodeProofRefusal(t, rec)
			if rec.Code != tc.status || body.Reason != tc.reason || !strings.Contains(body.Error, tc.want) || body.ProofMethod != hubV1ProofGitHubUserToken {
				t.Fatalf("%d %s, want %d reason %q containing %q", rec.Code, rec.Body.String(), tc.status, tc.reason, tc.want)
			}
			if !reflect.DeepEqual(body.RequiredScopes, tc.wantRequired) {
				t.Fatalf("required_scopes = %v, want %v", body.RequiredScopes, tc.wantRequired)
			}
			if tc.grantedAbsent && body.GrantedScopes != nil {
				t.Fatalf("granted_scopes = %v, want absent", body.GrantedScopes)
			}
			if !tc.grantedAbsent && !reflect.DeepEqual(body.GrantedScopes, tc.wantGranted) {
				t.Fatalf("granted_scopes = %v, want %v", body.GrantedScopes, tc.wantGranted)
			}
			if strings.Contains(rec.Body.String(), tc.token) || strings.Contains(env.logs.String(), tc.token) {
				t.Fatal("the token leaked into the refusal or the logs")
			}
			if !strings.Contains(env.logs.String(), "proof_reason="+tc.reason) {
				t.Fatal("the refusal reason is not audited")
			}
			if _, ok := env.s.selfService.get(hubV1TestID); ok {
				t.Fatal("a failed proof stored a registration")
			}
		})
	}
}

func TestHubV1UserTokenGitHubErrorIsBadGateway(t *testing.T) {
	env, _ := newProofTestEnv(t, true)
	rec := env.v1Create(t, `{"name":"Lab","repos":["alice/broken"]}`, proofHeaders(proofTestGoodToken))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "try again") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestHubV6RouteIgnoresProofHeaders(t *testing.T) {
	env, _ := newProofTestEnv(t, true)
	req := httptest.NewRequest(http.MethodPost, "/api/hub/selfservice/hives", strings.NewReader(proofOrgRepoBody))
	req.Header.Set("Origin", selfServiceTestOrigin)
	for k, v := range proofHeaders(proofTestGoodToken) {
		req.Header.Set(k, v)
	}
	req.AddCookie(&http.Cookie{
		Name:  "hive_hub_user",
		Value: mintHubUserCookieValueV2(deriveDomainKey(grantableTestSecret, infoSessionEd25519Seed), selfServiceTestUser),
	})
	rec := httptest.NewRecorder()
	env.s.requireAuth(env.s.handleSelfServiceRegister)(rec, req)
	// The legacy route proves with the stored token, which cannot see the
	// role on alice/orgrepo, and keeps its pinned not-admin body.
	if rec.Code != http.StatusForbidden || decodeError(t, rec)["error"] != "your GitHub account is not an admin of alice/orgrepo" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestSelfServiceTokenScopes(t *testing.T) {
	h := http.Header{}
	if scopes, known := selfServiceTokenScopes(h); known || scopes != nil {
		t.Fatalf("absent header: %v %v", scopes, known)
	}
	h.Set("X-OAuth-Scopes", "")
	if scopes, known := selfServiceTokenScopes(h); !known || len(scopes) != 0 {
		t.Fatalf("empty header: %v %v", scopes, known)
	}
	h.Set("X-OAuth-Scopes", " repo,  read:org ,")
	if scopes, known := selfServiceTokenScopes(h); !known || !reflect.DeepEqual(scopes, []string{"repo", "read:org"}) {
		t.Fatalf("scopes: %v %v", scopes, known)
	}
}
