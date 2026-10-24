// Package openrouter implements the OpenRouter OAuth PKCE "scan-to-fund" flow:
// a sponsor scans a QR (or clicks a link), authorizes on openrouter.ai, and the
// hive receives a user-controlled, scoped OpenRouter API key which it stores as
// a model gateway named "openrouter". The key is minted by OpenRouter against
// the sponsor's own account/credits — the hive never sees the sponsor's login.
//
// This package is transport-agnostic: it holds the PKCE crypto, the URL
// builders, the key-exchange + credit HTTP calls, the QR PNG encoder, and an
// in-memory single-use state store. The hub and the spoke dashboard both import
// it and wire it to their own HTTP routes (see pkg/hub and pkg/dashboard).
//
// SECURITY MODEL:
//   - PKCE S256. The code_verifier NEVER leaves the server.
//   - state is single-use and short-lived (StateTTL); it binds a returning
//     callback to the hive it was started for, so a code can't fund a
//     different hive.
//   - The received key is a secret VALUE — callers store it via the existing
//     per-gateway secret-file store; it is never logged, echoed, or inlined.
package openrouter

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// OpenRouter endpoint constants. All OpenRouter URLs live here as named consts
// (no magic strings): the base API, the OAuth authorize page, the PKCE
// key-exchange endpoint, and the key-info (credit) endpoint.
// OpenRouter endpoints. Declared as var (not const) so tests can point them at
// an httptest server; production code never reassigns them.
var (
	// BaseURL is the OpenAI-compatible base URL a created gateway routes through.
	BaseURL = "https://openrouter.ai/api/v1"
	// AuthURL is the OAuth authorize page the sponsor is sent to (via QR/link).
	AuthURL = "https://openrouter.ai/auth"
	// KeyExchangeURL trades the returned code + PKCE verifier for the user key.
	KeyExchangeURL = "https://openrouter.ai/api/v1/auth/keys"
	// KeyInfoURL returns the current key's credit limit/usage (Bearer-authed).
	KeyInfoURL = "https://openrouter.ai/api/v1/key"
)

const (
	// codeChallengeMethod is the PKCE method. S256 (SHA-256) is required; plain
	// is never used.
	codeChallengeMethod = "S256"

	// verifierBytes is the entropy length of the PKCE code_verifier. 32 random
	// bytes base64url-encode to a 43-char verifier, at the low end of the
	// RFC 7636 43–128 range and matching the Claude OAuth helper elsewhere.
	verifierBytes = 32

	// stateBytes is the entropy length of the single-use OAuth state token.
	stateBytes = 24

	// StateTTL bounds how long a started flow may be completed. A returning
	// callback after this window is rejected (the sponsor must re-scan). Short
	// enough to limit replay, long enough for a phone scan + login.
	StateTTL = 10 * time.Minute

	// keyExchangeTimeout bounds the code→key POST.
	keyExchangeTimeout = 30 * time.Second
	// creditTimeout bounds the GET /key credit check.
	creditTimeout = 15 * time.Second
	// maxErrBody caps how much of an OpenRouter error body we read/surface.
	maxErrBody = 4 << 10

	// qrPNGSize is the QR PNG side length in pixels — large enough to scan from
	// a screen, small enough to inline as a data-URI without bloat.
	qrPNGSize = 320
)

// DefaultModel is the gateway default_model used when the sponsor picks none.
// A cheap, widely-available model so a freshly-funded hive works out of the box
// without spending on a premium model by accident.
const DefaultModel = "deepseek/deepseek-chat"

// SuggestedModel is one curated option offered on the funding screen: an id and
// a short human label. The list is a starting point — the UI also fetches the
// live model list and allows manual entry.
type SuggestedModel struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// SuggestedModels is the curated short list shown on the funding screen. It
// spans a premium option (Claude Opus) and cheaper ones (DeepSeek, Llama), so a
// sponsor can pick a sensible default without fetching the full catalog. IDs are
// OpenRouter model ids.
var SuggestedModels = []SuggestedModel{
	{ID: "anthropic/claude-opus-4.8", Label: "Claude Opus 4.8 (premium)"},
	{ID: "anthropic/claude-3.5-sonnet", Label: "Claude 3.5 Sonnet"},
	{ID: "deepseek/deepseek-chat", Label: "DeepSeek Chat (cheap)"},
	{ID: "meta-llama/llama-3.3-70b-instruct", Label: "Llama 3.3 70B (cheap)"},
	{ID: "google/gemini-flash-1.5", Label: "Gemini Flash 1.5 (cheap)"},
}

// GeneratePKCE creates a random code_verifier and its S256 code_challenge.
// The verifier is a secret held server-side; only the challenge is sent to
// OpenRouter.
func GeneratePKCE() (verifier, challenge string, err error) {
	buf := make([]byte, verifierBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	challenge = ChallengeFor(verifier)
	return verifier, challenge, nil
}

// ChallengeFor returns the S256 code_challenge for a given verifier:
// base64url(sha256(verifier)). Exported so tests can assert the exact
// transform independently of GeneratePKCE's randomness.
func ChallengeFor(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// generateState returns a random, URL-safe single-use state token.
func generateState() (string, error) {
	buf := make([]byte, stateBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// BuildAuthorizeURL builds the openrouter.ai/auth URL the sponsor is sent to.
//
// callbackURL is THIS server's callback endpoint; the state token is appended
// to it as a query param so it round-trips even though OpenRouter's spec only
// echoes the callback_url (it preserves any query already on callback_url). The
// code_challenge is the S256 challenge; code_challenge_method is always S256.
func BuildAuthorizeURL(callbackURL, codeChallenge, state string) (string, error) {
	cb, err := url.Parse(callbackURL)
	if err != nil {
		return "", fmt.Errorf("invalid callback url: %w", err)
	}
	// Bind state to the callback_url itself so it survives the round-trip.
	q := cb.Query()
	q.Set("state", state)
	cb.RawQuery = q.Encode()

	v := url.Values{
		"callback_url":          {cb.String()},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {codeChallengeMethod},
		// Also pass state as a top-level authorize param; harmless if ignored.
		"state": {state},
	}
	return AuthURL + "?" + v.Encode(), nil
}

// ExchangeCode trades the returned authorization code + the stored PKCE verifier
// for a user-controlled OpenRouter API key. On success it returns the key VALUE
// — the caller MUST store it as a secret file and never log or echo it.
func ExchangeCode(code, verifier string) (string, error) {
	if code == "" || verifier == "" {
		return "", fmt.Errorf("missing code or verifier")
	}
	body, err := json.Marshal(map[string]string{
		"code":                  code,
		"code_verifier":         verifier,
		"code_challenge_method": codeChallengeMethod,
	})
	if err != nil {
		return "", fmt.Errorf("encode key-exchange body: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, KeyExchangeURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build key-exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: keyExchangeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("key-exchange request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return "", fmt.Errorf("openrouter key exchange returned HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var out struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrBody)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode key-exchange response: %w", err)
	}
	if strings.TrimSpace(out.Key) == "" {
		return "", fmt.Errorf("openrouter key exchange returned an empty key")
	}
	return strings.TrimSpace(out.Key), nil
}

// Credit is the subset of GET /api/v1/key we surface to the UI. Limit and
// LimitRemaining are pointers because OpenRouter returns null for an unlimited
// key; a nil value means "unlimited / not set". The raw key is NEVER included.
type Credit struct {
	Label          string   `json:"label,omitempty"`
	Limit          *float64 `json:"limit"`
	LimitRemaining *float64 `json:"limit_remaining"`
	Usage          float64  `json:"usage"`
}

// FetchCredit calls GET /api/v1/key with the given key and returns its credit
// limit/usage. The key is used only as a Bearer credential for this call and is
// never returned in the result.
func FetchCredit(key string) (*Credit, error) {
	if strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("no key configured")
	}
	req, err := http.NewRequest(http.MethodGet, KeyInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build key-info request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)

	client := &http.Client{Timeout: creditTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("key-info request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter key-info returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Data Credit `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrBody)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode key-info response: %w", err)
	}
	return &out.Data, nil
}

// modelsListTimeout bounds the public /v1/models fetch used by the funding
// picker. Kept short — it's a best-effort enrichment, not on the critical path.
const modelsListTimeout = 5 * time.Second

// PublicModelIDs best-effort fetches OpenRouter's public model catalog (no key
// required) and returns the list of model ids. On any failure it returns nil, so
// callers fall back to the curated SuggestedModels + manual entry. The response
// is the standard OpenAI-style {data:[{id}]} shape.
func PublicModelIDs() []string {
	req, err := http.NewRequest(http.MethodGet, BaseURL+"/models", nil)
	if err != nil {
		return nil
	}
	client := &http.Client{Timeout: modelsListTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// QRPNG returns a scannable QR code PNG encoding the given text (an authorize
// URL). It is a pure-Go encoder (no external service, no vendored JS), so the
// dashboard's CSP stays clean and the image is same-origin.
func QRPNG(text string) ([]byte, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("no data to encode")
	}
	qr, err := qrcode.New(text, qrcode.Medium)
	if err != nil {
		return nil, fmt.Errorf("build QR: %w", err)
	}
	img := qr.Image(qrPNGSize)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode QR PNG: %w", err)
	}
	return buf.Bytes(), nil
}

// PendingFlow is the server-side state for one in-progress funding flow, keyed
// by its single-use state token. It binds the flow to a specific hive so a
// returning callback can only fund the hive the flow was started for.
type PendingFlow struct {
	Verifier  string
	HiveID    string
	Model     string
	CreatedAt time.Time
}

// StateStore is an in-memory, single-use, TTL-bounded store of pending flows.
// A state is created at flow start and consumed (deleted) on callback; a
// missing, expired, or already-consumed state is rejected. Safe for concurrent
// use. Nothing here is persisted — an in-flight flow simply doesn't survive a
// restart, which is acceptable for a 10-minute human-in-the-loop scan.
type StateStore struct {
	mu      sync.Mutex
	flows   map[string]PendingFlow
	ttl     time.Duration
	nowFunc func() time.Time
}

// NewStateStore returns a StateStore with the given TTL (use StateTTL in
// production).
func NewStateStore(ttl time.Duration) *StateStore {
	return &StateStore{
		flows:   make(map[string]PendingFlow),
		ttl:     ttl,
		nowFunc: time.Now,
	}
}

// now returns the store's clock (overridable in tests via SetClock).
func (s *StateStore) now() time.Time {
	if s.nowFunc != nil {
		return s.nowFunc()
	}
	return time.Now()
}

// SetClock overrides the store's clock. Test-only; production uses time.Now.
func (s *StateStore) SetClock(f func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nowFunc = f
}

// Create records a new pending flow for hiveID + model and returns its random,
// single-use state token. It also opportunistically evicts expired flows so the
// map does not grow without bound.
func (s *StateStore) Create(verifier, hiveID, model string) (string, error) {
	state, err := generateState()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	s.flows[state] = PendingFlow{
		Verifier:  verifier,
		HiveID:    hiveID,
		Model:     model,
		CreatedAt: s.now(),
	}
	return state, nil
}

// Consume looks up and DELETES the flow for state (single-use), returning it
// only when present and not expired. A missing, expired, or replayed state
// returns ok=false. Deleting before returning guarantees a code can be redeemed
// at most once even under concurrent callbacks.
func (s *StateStore) Consume(state string) (PendingFlow, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.flows[state]
	if !ok {
		return PendingFlow{}, false
	}
	delete(s.flows, state)
	if s.now().Sub(f.CreatedAt) > s.ttl {
		return PendingFlow{}, false
	}
	return f, true
}

// evictExpiredLocked removes expired flows. Caller must hold s.mu.
func (s *StateStore) evictExpiredLocked() {
	cutoff := s.now().Add(-s.ttl)
	for k, f := range s.flows {
		if f.CreatedAt.Before(cutoff) {
			delete(s.flows, k)
		}
	}
}
