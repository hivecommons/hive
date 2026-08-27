package mint

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Kubernetes ServiceAccount caller authentication for /mint (#3915).
//
// This is the backend the CallerAuthenticator seam in caller.go was added for,
// and the last of the finding's three gaps: the shared secret proves TRUSTED
// NETWORK POSITION, not WHO IS CALLING. A caller here presents its own
// projected ServiceAccount token, the mint asks the Kubernetes API server to
// vouch for it, and the answer — `system:serviceaccount:<ns>:<name>` — is a
// real identity that Entitlements can be keyed on.
//
// NO NEW DEPENDENCY. The seam landed without this backend because TokenReview
// was assumed to need k8s.io/client-go, which this module does not depend on,
// and pulling in a dependency of that weight is a maintainer call. It turns out
// not to be needed: TokenReview is one POST of a small, stable JSON object to
// authentication.k8s.io/v1 (GA since Kubernetes 1.6), so net/http and
// encoding/json cover it. If client-go is ever added for other reasons this
// file can be swapped for it behind the same interface, with no change to
// Server. That is the whole point of the seam.
//
// WHAT MAKES THIS SAFE, AND WHAT WOULD MAKE IT NOT:
//
//	audience-scoped   The caller's token must be projected with the mint's own
//	                  audience, and the review REQUESTS that audience. A token
//	                  minted for the API server cannot be replayed here, and a
//	                  token projected for the mint cannot be replayed at the API
//	                  server. The check that makes this real is on the RESPONSE,
//	                  not the request — see verifyAudience.
//	dedicated header  The reviewed token is read from its own header, never from
//	                  Authorization. In dual-accept deployments Authorization
//	                  still carries the SHARED SECRET, and sending that to the
//	                  API server as "a token to please review" would write the
//	                  mint's secret into someone else's audit log.
//	real TLS          The API server is verified against the in-cluster CA
//	                  bundle. There is no insecure-skip-verify option, not even
//	                  a documented one.
//	bounded           A timeout and a response-size cap, so an API server that
//	                  hangs or floods cannot wedge or exhaust the mint. The
//	                  failure is a refusal — TokenReview failing open would be
//	                  strictly worse than the shared secret it replaces.

const (
	// ServiceAccountTokenHeader carries the caller's projected ServiceAccount
	// token. Deliberately NOT Authorization: that header holds the shared
	// secret in a dual-accept deployment, and this value is forwarded to the
	// Kubernetes API server.
	ServiceAccountTokenHeader = "X-Hive-Mint-SA-Token"

	// KindServiceAccount is the Identity.Kind for TokenReview-verified callers.
	KindServiceAccount = "serviceaccount"

	// serviceAccountUsernamePrefix is what the API server reports for a
	// ServiceAccount: system:serviceaccount:<namespace>:<name>. Any other
	// username shape is a human or node identity and is refused — an
	// entitlement map keyed on ServiceAccount names must not be satisfiable by
	// a kubeconfig user who happens to have a token.
	serviceAccountUsernamePrefix = "system:serviceaccount:"

	// tokenReviewPath is the API server endpoint. authentication.k8s.io/v1 has
	// been GA since Kubernetes 1.6.
	tokenReviewPath = "/apis/authentication.k8s.io/v1/tokenreviews"

	// inClusterTokenPath / inClusterCAPath are the projected paths every pod
	// with a mounted ServiceAccount gets.
	inClusterTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101 -- path, not a credential
	inClusterCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

	// defaultReviewTimeout bounds one TokenReview round trip.
	defaultReviewTimeout = 5 * time.Second

	// maxReviewResponseBytes bounds the API server's response.
	maxReviewResponseBytes = 1 << 20 // 1 MiB
)

// TokenReviewAuthenticator verifies a caller's Kubernetes ServiceAccount token
// by asking the API server, and reports the ServiceAccount as the identity.
//
// Safe for concurrent use.
type TokenReviewAuthenticator struct {
	// apiURL is the API server base, e.g. https://10.0.0.1:443.
	apiURL string
	// audience is the audience the caller's token must be projected for, and
	// the one the review requests.
	audience string
	// client performs the review call, TLS-verified against the cluster CA.
	client *http.Client
	// reviewerToken authenticates the MINT to the API server (not the caller).
	// It is re-read on each use because projected tokens are rotated on disk;
	// caching it forever would start failing an hour into the pod's life.
	reviewerToken func() (string, error)
}

// TokenReviewOption configures a TokenReviewAuthenticator.
type TokenReviewOption func(*TokenReviewAuthenticator)

// WithReviewHTTPClient overrides the HTTP client. Intended for tests, which
// point it at an httptest server; production uses the in-cluster CA bundle.
// A nil client is ignored so the verified default cannot be cleared.
func WithReviewHTTPClient(c *http.Client) TokenReviewOption {
	return func(a *TokenReviewAuthenticator) {
		if c != nil {
			a.client = c
		}
	}
}

// WithReviewerToken overrides how the mint authenticates ITSELF to the API
// server. Default: re-read the pod's projected token from disk on each review.
func WithReviewerToken(f func() (string, error)) TokenReviewOption {
	return func(a *TokenReviewAuthenticator) {
		if f != nil {
			a.reviewerToken = f
		}
	}
}

// NewTokenReviewAuthenticator builds the backend from explicit parameters.
//
// audience MUST be non-empty. An empty audience would send a review with no
// audience constraint, and the response check below would have nothing to
// verify against — which is exactly the replay hole this backend exists to
// close, so it is a construction error rather than a silent downgrade.
func NewTokenReviewAuthenticator(apiURL, audience string, caPEM []byte, opts ...TokenReviewOption) (*TokenReviewAuthenticator, error) {
	if apiURL == "" {
		return nil, fmt.Errorf("mint: TokenReview requires an API server URL")
	}
	if audience == "" {
		return nil, fmt.Errorf("mint: TokenReview requires an audience (an unscoped review would accept a token minted for the API server)")
	}

	a := &TokenReviewAuthenticator{
		apiURL:        strings.TrimRight(apiURL, "/"),
		audience:      audience,
		reviewerToken: readFileTrimmed(inClusterTokenPath),
	}

	if len(caPEM) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("mint: TokenReview CA bundle contains no usable certificate")
		}
		a.client = &http.Client{
			Timeout: defaultReviewTimeout,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
		}
	} else {
		// No explicit bundle: the host trust store. Callers in-cluster should
		// use NewInClusterTokenReviewAuthenticator, which always supplies one.
		a.client = &http.Client{
			Timeout:   defaultReviewTimeout,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}},
		}
	}

	for _, opt := range opts {
		opt(a)
	}
	return a, nil
}

// NewInClusterTokenReviewAuthenticator builds the backend from the pod's own
// ServiceAccount mount and the KUBERNETES_SERVICE_* environment, the same
// inputs client-go's rest.InClusterConfig uses.
//
// It fails rather than degrading: a missing CA bundle or an absent
// KUBERNETES_SERVICE_HOST means this is not running in a cluster, and a mint
// that quietly fell back to a weaker gate on that basis would be the finding
// again in a new costume.
func NewInClusterTokenReviewAuthenticator(audience string, opts ...TokenReviewOption) (*TokenReviewAuthenticator, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("mint: not running in a Kubernetes cluster (KUBERNETES_SERVICE_HOST/PORT unset)")
	}
	caPEM, err := os.ReadFile(inClusterCAPath)
	if err != nil {
		return nil, fmt.Errorf("mint: reading in-cluster CA bundle: %w", err)
	}
	return NewTokenReviewAuthenticator("https://"+net.JoinHostPort(host, port), audience, caPEM, opts...)
}

// Name implements CallerAuthenticator.
func (a *TokenReviewAuthenticator) Name() string { return KindServiceAccount }

// Authenticate implements CallerAuthenticator.
//
// Every failure returns ErrUnauthenticated with no detail: the handler answers
// 401 without telling a prober whether the token was missing, expired, for the
// wrong audience, or belonged to a user rather than a ServiceAccount.
func (a *TokenReviewAuthenticator) Authenticate(r *http.Request) (Identity, error) {
	presented := strings.TrimSpace(r.Header.Get(ServiceAccountTokenHeader))
	// Tolerate a "Bearer " prefix: callers reuse HTTP client middleware that
	// adds one, and refusing it produces a 401 with no way to tell why.
	presented = strings.TrimPrefix(presented, authScheme)
	if presented == "" {
		return Identity{}, ErrUnauthenticated
	}

	ctx := r.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	status, err := a.review(ctx, presented)
	if err != nil {
		return Identity{}, ErrUnauthenticated
	}
	if !status.Authenticated || status.Error != "" {
		return Identity{}, ErrUnauthenticated
	}
	if !a.verifyAudience(status.Audiences) {
		return Identity{}, ErrUnauthenticated
	}
	name := status.User.Username
	if !strings.HasPrefix(name, serviceAccountUsernamePrefix) {
		return Identity{}, ErrUnauthenticated
	}
	// system:serviceaccount:<ns>:<name> — both parts must be present, or the
	// identity is not the shape Entitlements are keyed on.
	rest := strings.TrimPrefix(name, serviceAccountUsernamePrefix)
	ns, sa, ok := strings.Cut(rest, ":")
	if !ok || ns == "" || sa == "" {
		return Identity{}, ErrUnauthenticated
	}
	return Identity{Kind: KindServiceAccount, Name: name}, nil
}

// verifyAudience is the check that makes audience scoping real.
//
// The REQUEST asking for an audience proves nothing on its own: an API server
// whose authenticators do not implement audience validation answers
// `authenticated: true` with the audiences field ABSENT, and a caller that
// looked only at `authenticated` would accept a token minted for the API server
// itself — precisely the replay this backend claims to prevent. The Kubernetes
// API contract puts the obligation on the client: the returned audiences are
// the intersection of what was asked for and what the token carries, and an
// empty intersection means "not validated for your audience".
//
// So: the mint's audience must appear in the RESPONSE, and an empty or absent
// list is a refusal, never a pass.
func (a *TokenReviewAuthenticator) verifyAudience(returned []string) bool {
	for _, aud := range returned {
		if aud == a.audience {
			return true
		}
	}
	return false
}

// tokenReviewRequest / tokenReviewResponse are the minimal shapes of
// authentication.k8s.io/v1 TokenReview. Only the fields this backend acts on
// are modelled; the API server ignores what it does not need and we ignore what
// we do not read.
type tokenReviewRequest struct {
	APIVersion string                 `json:"apiVersion"`
	Kind       string                 `json:"kind"`
	Spec       tokenReviewRequestSpec `json:"spec"`
}

type tokenReviewRequestSpec struct {
	Token     string   `json:"token"`
	Audiences []string `json:"audiences,omitempty"`
}

type tokenReviewResponse struct {
	Status tokenReviewStatus `json:"status"`
}

type tokenReviewStatus struct {
	Authenticated bool            `json:"authenticated"`
	User          tokenReviewUser `json:"user"`
	Audiences     []string        `json:"audiences"`
	Error         string          `json:"error"`
}

type tokenReviewUser struct {
	Username string `json:"username"`
	UID      string `json:"uid"`
}

// review performs one TokenReview round trip.
//
// The presented token is placed in the request BODY and never in a header, a
// log line, or an error. The only Authorization on this call is the mint's own
// reviewer token.
func (a *TokenReviewAuthenticator) review(ctx context.Context, token string) (tokenReviewStatus, error) {
	var zero tokenReviewStatus

	body, err := json.Marshal(tokenReviewRequest{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec: tokenReviewRequestSpec{
			Token:     token,
			Audiences: []string{a.audience},
		},
	})
	if err != nil {
		return zero, err
	}

	ctx, cancel := context.WithTimeout(ctx, defaultReviewTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiURL+tokenReviewPath, bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	reviewer, err := a.reviewerToken()
	if err != nil {
		return zero, fmt.Errorf("mint: reading reviewer token: %w", err)
	}
	if reviewer == "" {
		return zero, fmt.Errorf("mint: empty reviewer token")
	}
	req.Header.Set("Authorization", authScheme+reviewer)

	resp, err := a.client.Do(req)
	if err != nil {
		return zero, err
	}
	defer func() {
		// Drain (bounded) so the connection can be reused, then close.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxReviewResponseBytes))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		// A 401/403 here means the MINT cannot call TokenReview (its RBAC is
		// missing), not that the caller is bad. Both refuse the caller — fail
		// closed — but the distinction matters when reading logs, so it is in
		// the error the server logs rather than swallowed.
		return zero, fmt.Errorf("mint: TokenReview returned HTTP %d (the mint's own ServiceAccount may lack create on tokenreviews)", resp.StatusCode)
	}

	var out tokenReviewResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReviewResponseBytes)).Decode(&out); err != nil {
		return zero, err
	}
	return out.Status, nil
}

// readFileTrimmed returns a loader that reads path fresh on each call. Fresh,
// not cached: projected ServiceAccount tokens are rotated on disk (hourly by
// default), so a token read once at construction stops working while the
// process is still healthy.
func readFileTrimmed(path string) func() (string, error) {
	return func() (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
}

// MultiAuthenticator tries several backends in order and returns the first
// identity established.
//
// This is the migration path the finding's rollout needs: run TokenReview
// alongside the shared secret, watch the audit log until every caller is
// arriving as a ServiceAccount, then drop the secret from the list. Without it
// the cutover is a flag day.
//
// Order matters, and TokenReview should come FIRST. It reads its own header, so
// putting it ahead of the shared secret means a caller that presents both is
// recorded under its real identity rather than as "any-holder" — which is the
// difference between an audit log that shows the migration finishing and one
// that shows nothing changing.
type MultiAuthenticator struct {
	backends []CallerAuthenticator
}

// NewMultiAuthenticator builds a dual-accept authenticator. Nil backends are
// dropped; an empty list is an error, because an authenticator that
// authenticates nothing would answer 401 to everything and look like an outage
// rather than a misconfiguration.
func NewMultiAuthenticator(backends ...CallerAuthenticator) (*MultiAuthenticator, error) {
	var kept []CallerAuthenticator
	for _, b := range backends {
		if b != nil {
			kept = append(kept, b)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("mint: MultiAuthenticator needs at least one backend")
	}
	return &MultiAuthenticator{backends: kept}, nil
}

// Name implements CallerAuthenticator, listing the backends in the order tried
// so a startup log says which mechanisms are live.
func (m *MultiAuthenticator) Name() string {
	names := make([]string, 0, len(m.backends))
	for _, b := range m.backends {
		names = append(names, b.Name())
	}
	return strings.Join(names, "+")
}

// Authenticate implements CallerAuthenticator, returning the first identity any
// backend establishes. It does not report WHICH backend failed for a refused
// caller — the handler must not turn a 401 into an oracle for which mechanisms
// are configured.
func (m *MultiAuthenticator) Authenticate(r *http.Request) (Identity, error) {
	for _, b := range m.backends {
		if id, err := b.Authenticate(r); err == nil {
			return id, nil
		}
	}
	return Identity{}, ErrUnauthenticated
}
