package dashboard

// Live validation ("Test key") for the IBM bobshell ("bob") API key stored by
// bob_key_store.go. bob authenticates with an API key only (its browser SSO
// cannot complete inside a pod — see bob_key_store.go), and until now the
// dashboard had no way to tell a working key from a bad one: owners saved a
// key and found out from bob agents crash-looping at the auth prompt.
//
// The probe is the cheapest authenticated call the bob backend supports —
// GET /inference/v1/model/info — verified live against the production
// backend. There is no pre-existing hive-side bob HTTP client to reuse; the
// wire conventions below were confirmed with a real multi-key test:
//
//   - base URL https://api.us-east.bob.ibm.com
//   - opaque keys (bob_prod_…) authenticate as `Authorization: Apikey <key>`.
//     bobshell itself first sends Bearer, fails to parse the key as a JWT,
//     and flips to Apikey — the probe goes straight to the scheme that
//     matches the key's shape (Bearer only for JWT-shaped keys).
//   - a 401 "Token verification failed: invalid jwt string" means the key
//     reached the backend in a form it tried to parse as a JWT (wrong scheme
//     or a dirty/truncated value) — the key itself may be fine.
//   - a 402 insufficient_quota_or_plan_expired means the credential is valid
//     but has no inference entitlement (wrong key kind/scope).
//   - an HTML 403 is the CDN/edge blocking the request — not a key verdict.
//
// SECURITY: a key under test is never persisted, never logged, and never
// echoed back. Every error string that could embed the key (transport errors,
// backend error bodies) passes through redactSecret before leaving the
// process.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// defaultBobAPIBaseURL is the bob backend's API host, confirmed live.
	// Keys are issued at bob.ibm.com (Scope: Inference); the API itself is
	// served from this regional host.
	defaultBobAPIBaseURL = "https://api.us-east.bob.ibm.com"

	// bobAPIBaseURLEnv optionally overrides the probe's base URL (self-hosted
	// relays, IBM moving the API host, and tests). Only the base URL is
	// configurable — the probe path and auth convention are fixed.
	bobAPIBaseURLEnv = "HIVE_BOB_API_URL"

	// bobProbePath is the cheapest authenticated request the backend
	// supports (confirmed live): no body, no inference cost.
	bobProbePath = "/inference/v1/model/info"

	// bobAuthSchemeAPIKey / bobAuthSchemeBearer are the two Authorization
	// schemes the backend accepts. Opaque keys (bob_prod_…) use Apikey;
	// Bearer is only correct for JWT-shaped credentials.
	bobAuthSchemeAPIKey = "Apikey "
	bobAuthSchemeBearer = "Bearer "

	// bobProbeUserAgent is the User-Agent the probe sends. The Cloudflare WAF
	// in front of api.us-east.bob.ibm.com fingerprint-blocks generic HTTP
	// clients: Go's default "Go-http-client/2.0" (and curl's UA) get an HTML
	// 403 block page before bob's auth ever sees the key, while "bobshell"
	// — the UA of bob's own CLI — passes through to the backend. Confirmed
	// live 2026-08-07 from both a hive pod and a workstation: same request,
	// default UA → Cloudflare 403; "bobshell" UA → backend verdict (200/401).
	bobProbeUserAgent = "bobshell"

	// bobKeyManageURL is where an operator creates a correctly-scoped key —
	// surfaced in entitlement-failure details. Keep in sync with
	// BOB_API_KEY_MANAGE_URL in static/index.html.
	bobKeyManageURL = "https://bob.ibm.com/admin/apikeys"

	// bobProbeTimeout bounds the whole test round-trip so a black-holed host
	// cannot hang the dashboard request.
	bobProbeTimeout = 10 * time.Second

	// bobProbeMaxErrBody caps how much of a backend error body is read and
	// surfaced. Error bodies can be huge; the useful part ("invalid api key",
	// "wrong scope") is at the front.
	bobProbeMaxErrBody = 512
)

// Machine-readable failure classes for the test endpoint. The UI shows the
// human-readable detail; the reason is stable for scripting/tests.
const (
	bobTestReasonUnauthorized  = "unauthorized"
	bobTestReasonNoEntitlement = "no_entitlement"
	bobTestReasonUnreachable   = "unreachable"
	bobTestReasonTimeout       = "timeout"
	bobTestReasonBadStatus     = "bad_status"
	bobTestReasonNoKey         = "no_key"
	bobTestReasonBadKeyFormat  = "bad_key_format"
)

// bobProbeBaseURL resolves the backend base URL, honoring the env override.
func bobProbeBaseURL() string {
	if v := strings.TrimSpace(os.Getenv(bobAPIBaseURLEnv)); v != "" {
		return v
	}
	return defaultBobAPIBaseURL
}

// bobBackendSays extracts everything useful the backend included with a
// failure — error body, WWW-Authenticate challenge, request id — so the
// operator sees the backend's own words ("invalid api key", "key expired",
// "wrong scope: needs Inference"), not just a classification. The body is
// truncated to bobProbeMaxErrBody, whitespace-collapsed to a single line,
// and redacted so key material can never ride along.
func bobBackendSays(resp *http.Response, key string) string {
	var parts []string
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bobProbeMaxErrBody))
	if msg := strings.Join(strings.Fields(string(body)), " "); msg != "" {
		parts = append(parts, msg)
	}
	if wa := strings.TrimSpace(resp.Header.Get("WWW-Authenticate")); wa != "" {
		parts = append(parts, "WWW-Authenticate: "+wa)
	}
	// Both spellings observed across IBM services; first one present wins.
	for _, h := range []string{"X-Request-Id", "X-Global-Transaction-Id"} {
		if rid := strings.TrimSpace(resp.Header.Get(h)); rid != "" {
			parts = append(parts, "request-id "+rid)
			break
		}
	}
	return redactSecret(strings.Join(parts, "; "), key)
}

// bobAuthHeaderValue picks the Authorization scheme by the key's shape:
// Bearer only for JWT-shaped credentials (three dot-separated segments,
// "eyJ" header), Apikey for everything else — including bob_prod_ opaque
// keys, whose confirmed wire scheme is Apikey. This skips bobshell's own
// Bearer-then-flip dance and its guaranteed "invalid jwt string" 401.
func bobAuthHeaderValue(key string) string {
	if strings.HasPrefix(key, "eyJ") && strings.Count(key, ".") == 2 {
		return bobAuthSchemeBearer + key
	}
	return bobAuthSchemeAPIKey + key
}

// bobJWTVerdictDetail is the decoded explanation for the backend's
// "Token verification failed: invalid jwt string" 401 — which means the key
// reached the backend in a form it tried to parse as a JWT, NOT necessarily
// that the key is bad.
const bobJWTVerdictDetail = "the backend received the key in a form it tried to parse as a JWT — " +
	"usually a stray newline/truncation in the stored value, or bob fell back to browser auth " +
	"because it could not read the key file (perms must allow the agent UID, e.g. 440)"

// bobEntitlementDetail is the decoded explanation for a 402: the credential
// authenticates but cannot run inference (wrong key kind or scope).
const bobEntitlementDetail = "key authenticates but has no inference entitlement — " +
	"create an API key with Scope: Inference at " + bobKeyManageURL

// probeBobAPIKey makes one authenticated model-info request with the given
// key. An empty reason means the backend accepted the key; otherwise reason
// is one of the bobTestReason* classes and detail is a human-readable,
// key-free explanation that includes — and where possible decodes — whatever
// the backend itself said.
func probeBobAPIKey(key string) (reason, detail string) {
	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(bobProbeBaseURL(), "/")+bobProbePath, nil)
	if err != nil {
		return bobTestReasonUnreachable, "building request: " + redactSecret(err.Error(), key)
	}
	req.Header.Set("Authorization", bobAuthHeaderValue(key))
	req.Header.Set("User-Agent", bobProbeUserAgent)
	client := &http.Client{Timeout: bobProbeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return bobTestReasonTimeout,
				fmt.Sprintf("the bob backend did not respond within %s", bobProbeTimeout)
		}
		// Transport errors can embed header values (e.g. Go's invalid-header
		// rejection quotes the offending value) — always redact.
		return bobTestReasonUnreachable, "cannot reach the bob backend: " + redactSecret(err.Error(), key)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return "", ""
	}

	says := bobBackendSays(resp, key)
	withSays := func(msg string) string {
		if says != "" {
			return msg + ": " + says
		}
		return msg
	}

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		detail = withSays(fmt.Sprintf("the bob backend rejected the key (HTTP %d)", resp.StatusCode))
		// Decode the known "invalid jwt string" verdict: it fingers the key's
		// DELIVERY, not its validity.
		if strings.Contains(says, "invalid jwt string") || strings.Contains(says, "Token verification failed") {
			detail += " — " + bobJWTVerdictDetail
		}
		return bobTestReasonUnauthorized, detail
	case http.StatusPaymentRequired:
		// 402 insufficient_quota_or_plan_expired: a real credential of the
		// wrong kind (e.g. a bob-user key instead of an Inference API key).
		return bobTestReasonNoEntitlement, withSays(bobEntitlementDetail)
	case http.StatusForbidden:
		// An HTML 403 is the CDN/edge (Cloudflare) blocking the request —
		// bob's auth never evaluated the key, so it is not a key verdict.
		if bobLooksLikeEdgeHTML(resp, says) {
			return bobTestReasonUnreachable,
				"the request was blocked at the network edge (HTML 403 from the CDN/proxy), " +
					"not by bob's auth — the key was likely never evaluated"
		}
		return bobTestReasonUnauthorized,
			withSays(fmt.Sprintf("the bob backend rejected the key (HTTP %d)", resp.StatusCode))
	default:
		return bobTestReasonBadStatus,
			withSays(fmt.Sprintf("the bob backend returned HTTP %d", resp.StatusCode))
	}
}

// bobLooksLikeEdgeHTML reports whether a 403 response is an HTML edge/CDN
// block page rather than a JSON auth verdict from bob itself.
func bobLooksLikeEdgeHTML(resp *http.Response, says string) bool {
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return true
	}
	trimmed := strings.TrimSpace(says)
	return strings.HasPrefix(trimmed, "<!DOCTYPE") || strings.HasPrefix(trimmed, "<html")
}

// handleGovernorBobKeyTest is POST /api/config/governor/bob/test — live
// validation of a bob API key. A body carrying a non-empty apiKey tests THAT
// key (pre-save validation; the value is never persisted, logged, or echoed);
// an empty/absent body tests the SAVED key via the same resolution agents use
// (ResolveAPIKey), so the key material never leaves the process.
//
// The response is always HTTP 200 with the verdict in the body —
// {"status":"ok"} or {"status":"error","reason":...,"detail":...} — so the UI
// distinguishes "the test ran and failed" from transport/auth failures of the
// dashboard itself. Session auth and the read-only write-gate come from the
// same authenticate/roleEnforcement middleware as every other /api/config
// route (this is a POST, so read-only roles are rejected before the handler).
func (s *Server) handleGovernorBobKeyTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey *string `json:"apiKey"`
	}
	// The body is OPTIONAL (absent/empty means "test the saved key"), so an
	// immediate EOF is not an error — only malformed JSON is.
	if err := decodeBody(r, &body); err != nil && !errors.Is(err, io.EOF) {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	mode := "saved"
	key := ""
	if body.APIKey != nil {
		key = strings.TrimSpace(*body.APIKey)
	}
	if key != "" {
		mode = "pasted"
		if len(key) > bobKeyMaxLen {
			jsonError(w, fmt.Sprintf("apiKey is too long (limit %d characters)", bobKeyMaxLen),
				http.StatusBadRequest)
			return
		}
		// Interior whitespace/newlines can never be part of a bob key, and a
		// broken paste is a proven "invalid jwt string" 401 — refuse with the
		// real explanation instead of testing a value that cannot work.
		// (Leading/trailing whitespace was already trimmed above.)
		if strings.ContainsAny(key, " \t\r\n") {
			jsonResponse(w, map[string]interface{}{
				"status": "error",
				"reason": bobTestReasonBadKeyFormat,
				"detail": "the pasted value contains interior whitespace or line breaks — a bob API key is a single unbroken token; re-copy it from bob.ibm.com and paste it again",
			})
			return
		}
	} else {
		key = s.deps.Config.Governor.Bob.ResolveAPIKey()
		if key == "" {
			jsonResponse(w, map[string]interface{}{
				"status": "error",
				"reason": bobTestReasonNoKey,
				"detail": "no bob API key is configured — paste a key to test, or save one first",
			})
			return
		}
	}

	reason, detail := probeBobAPIKey(key)
	if reason == "" {
		// mode/reason only — never key material, never backend bodies.
		s.logger.Info("bob api key test ok", "mode", mode)
		jsonResponse(w, map[string]interface{}{
			"status": "ok",
			"detail": "the bob backend accepted the key",
		})
		return
	}
	s.logger.Info("bob api key test failed", "mode", mode, "reason", reason)
	jsonResponse(w, map[string]interface{}{
		"status": "error",
		"reason": reason,
		"detail": detail,
	})
}
