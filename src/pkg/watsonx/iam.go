// Package watsonx handles IBM watsonx.ai-specific concerns that the generic
// OpenAI-compatible gateway path cannot: exchanging an IBM Cloud API key for a
// short-lived IAM bearer token (watsonx's model gateway authenticates with an
// IAM access token, NOT the raw API key), and the small constants (Granite
// fallback ids, the project-id header) the gateway wiring shares.
//
// The model gateway (https://<region>.ml.cloud.ibm.com/ml/gateway/v1) is
// OpenAI-compatible for /chat/completions and /models, so once a valid IAM
// bearer is minted, hive's existing OpenAI-compatible inference and discovery
// paths work unchanged. This package supplies only the bearer + the project-id
// header; it does not translate requests.
package watsonx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"encoding/json"
)

const (
	// IAMTokenURL is IBM Cloud's IAM token endpoint. A POST here exchanges an
	// IBM Cloud API key for a short-lived access token (JWT). This host is the
	// same across watsonx regions (only the ml.cloud.ibm.com data-plane host is
	// regional), so it is a constant rather than derived from the gateway URL.
	IAMTokenURL = "https://iam.cloud.ibm.com/identity/token"

	// iamAPIKeyGrantType is the OAuth grant type that exchanges an IBM Cloud API
	// key for an IAM access token.
	iamAPIKeyGrantType = "urn:ibm:params:oauth:grant-type:apikey"

	// iamTokenRequestTimeout bounds the token-exchange HTTP call. The IAM
	// endpoint is fast; this is generous headroom, not a tuned value.
	iamTokenRequestTimeout = 10 * time.Second

	// iamTokenExpirySkew is subtracted from a cached token's expiry before it is
	// reused: a token within this window of expiring is treated as already
	// expired so an in-flight request cannot 401 on a token that lapses mid
	// round-trip. IAM tokens live ~1h, so this margin costs at most one extra
	// mint per skew window.
	iamTokenExpirySkew = 5 * time.Minute

	// iamTokenFallbackTTL is used only if the IAM response omits expires_in
	// (it never should). Deliberately short so a missing TTL re-mints often
	// rather than serving a token past its real (unknown) lifetime.
	iamTokenFallbackTTL = 10 * time.Minute

	// iamErrBodyLimit caps how much of an IAM error body is folded into the
	// returned error. IAM error bodies are small JSON; this only guards against
	// a pathological upstream. The API key is never echoed by IAM, but we still
	// bound the text.
	iamErrBodyLimit = 512

	// ProjectIDHeader is the header watsonx reads the project (or space) id
	// from on gateway requests — the id an OpenAI client would not otherwise
	// send. Verified against IBM's watsonx-ai API reference
	// (https://cloud.ibm.com/apidocs/watsonx-ai), where project-scoped
	// operations take X-IBM-Project-ID.
	ProjectIDHeader = "X-IBM-Project-ID"
)

// GraniteFallbackModels is the small static list offered when live model
// discovery against the watsonx gateway cannot run (no key/project configured
// yet) or fails, so the Model Gateway dropdown is never empty. Live discovery
// via the gateway /v1/models is strongly preferred; this is only a floor.
// Current IBM Granite instruct ids (as of 2026-08), verified against IBM's
// Granite docs and watsonx model catalog. hive-owned models carry the "ibm/"
// provider prefix watsonx uses for its foundation models.
var GraniteFallbackModels = []string{
	"ibm/granite-4-h-small",
	"ibm/granite-4-h-micro",
	"ibm/granite-3-3-8b-instruct",
	"ibm/granite-3-3-2b-instruct",
}

// iamToken is a cached, minted IAM access token with its effective expiry.
type iamToken struct {
	token  string
	expiry time.Time
}

// TokenMinter exchanges an IBM Cloud API key for an IAM access token and caches
// the result per API key until it nears expiry. It is safe for concurrent use.
// One minter instance is shared process-wide (see DefaultMinter) so a single
// token is reused across every watsonx request rather than re-minted per call.
type TokenMinter struct {
	mu    sync.Mutex
	cache map[string]iamToken // keyed by API key; value is the minted token

	// endpoint and now are seams so tests can point the exchange at an httptest
	// server and control the clock without touching real IAM or wall time.
	endpoint string
	now      func() time.Time
	client   *http.Client
}

// NewTokenMinter builds a minter that talks to the real IBM IAM endpoint.
func NewTokenMinter() *TokenMinter {
	return &TokenMinter{
		cache:    make(map[string]iamToken),
		endpoint: IAMTokenURL,
		now:      time.Now,
		client:   &http.Client{Timeout: iamTokenRequestTimeout},
	}
}

// DefaultMinter is the process-wide minter. Using one shared instance means the
// cached IAM token is reused across all watsonx gateway requests (inference,
// probes, discovery) instead of each caller minting its own.
var DefaultMinter = NewTokenMinter()

// NewTokenMinterForTest builds a minter whose token exchange targets endpoint
// instead of the real IBM IAM host, so tests (including callers in other
// packages that swap DefaultMinter) can exercise the watsonx auth path without
// touching the network. A nil client uses a default-timeout client. This is a
// test-only seam; production code uses NewTokenMinter / DefaultMinter.
func NewTokenMinterForTest(endpoint string, client *http.Client) *TokenMinter {
	if client == nil {
		client = &http.Client{Timeout: iamTokenRequestTimeout}
	}
	return &TokenMinter{
		cache:    make(map[string]iamToken),
		endpoint: endpoint,
		now:      time.Now,
		client:   client,
	}
}

// Token returns a valid IAM bearer for apiKey, minting a fresh one only when
// the cache is empty or the cached token is within iamTokenExpirySkew of
// expiring. The apiKey is never logged and never returned; only the minted
// bearer is. A blank apiKey is an error (a watsonx gateway requires a key).
func (m *TokenMinter) Token(ctx context.Context, apiKey string) (string, error) {
	if strings.TrimSpace(apiKey) == "" {
		return "", fmt.Errorf("watsonx: no IBM Cloud API key configured")
	}

	m.mu.Lock()
	if tok, ok := m.cache[apiKey]; ok && m.now().Add(iamTokenExpirySkew).Before(tok.expiry) {
		t := tok.token
		m.mu.Unlock()
		return t, nil
	}
	m.mu.Unlock()

	tok, err := m.mint(ctx, apiKey)
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	m.cache[apiKey] = tok
	m.mu.Unlock()
	return tok.token, nil
}

// mint performs the IAM token exchange for apiKey. It never logs or echoes the
// key; IAM error bodies (which do not contain the key) are truncated.
func (m *TokenMinter) mint(ctx context.Context, apiKey string) (iamToken, error) {
	form := url.Values{}
	form.Set("grant_type", iamAPIKeyGrantType)
	form.Set("apikey", apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return iamToken{}, fmt.Errorf("watsonx: build IAM token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return iamToken{}, fmt.Errorf("watsonx: IAM token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, iamErrBodyLimit))
		return iamToken{}, fmt.Errorf("watsonx: IAM token exchange returned HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return iamToken{}, fmt.Errorf("watsonx: decode IAM token response: %w", err)
	}
	if parsed.AccessToken == "" {
		return iamToken{}, fmt.Errorf("watsonx: IAM token response had no access_token")
	}

	ttl := time.Duration(parsed.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = iamTokenFallbackTTL
	}
	return iamToken{token: parsed.AccessToken, expiry: m.now().Add(ttl)}, nil
}
