package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// A LiteLLM gateway's GET /v1/models advertises the FULL model catalog, but an
// individual API key's team may be scoped to a SUBSET. Selecting a non-entitled
// model makes the agent fail at inference with a 403 whose body lists the
// entitled set, e.g.:
//
//	team not allowed to access model. This team can only access
//	models=['Azure/gpt-4.1','Azure/gpt-4o',...]
//
// This file captures the entitled set for a gateway (keyed by endpoint) from
// two gateway-derived signals — a best-effort per-key info probe and, when that
// is unavailable, the 403 body itself — so the dashboard can offer only usable
// models. Provider names (Azure/aws/gemini) are never assumed; the entitled
// set comes only from the gateway, and ONLY the team-scope 403 counts as an
// entitlement signal: provider-side failures the gateway relays (upstream 404
// not-found, 400 bad-request) say nothing about the key's scope, so a
// provider-broken model stays listed rather than being misread as
// un-entitled. Switching an agent off a non-entitled model is left to the
// dashboard's reconcile policy (#1848), fed by the filtered list.

const (
	// entitlementProbePathKeyInfo and entitlementProbePathUserInfo are the
	// LiteLLM per-key endpoints tried, in order, to learn the entitled model
	// set for a key without waiting for a runtime 403. Both are best-effort:
	// not every LiteLLM deployment exposes them, and some require a
	// master/admin key. A non-200 or an empty/all ("*") list is treated as
	// "no entitlement signal" and the 403-parse path takes over.
	entitlementProbePathKeyInfo  = "/key/info"
	entitlementProbePathUserInfo = "/user/info"
	// entitlementProbeTimeout bounds each key-info probe request.
	entitlementProbeTimeout = 5 * time.Second
	// entitlementTTL is how long a captured entitled set is trusted before a
	// background re-probe is triggered. Team scopes change rarely; a generous
	// TTL avoids re-probing on every discovery while still recovering from a
	// team-scope change within a session. A stale entry keeps being SERVED
	// until fresh data replaces it (stale-while-revalidate) so the filtered
	// model list never flaps back to the full catalog between probes for an
	// unchanged key — flapping would make the dashboard auto-heal (#1848) see
	// models vanish/reappear across refreshes.
	entitlementTTL = 10 * time.Minute
	// entitlementWildcard is the LiteLLM sentinel meaning "all models" — a key
	// carrying it is NOT scoped, so no entitlement filter should be applied.
	entitlementWildcard = "*"
)

// team403Pattern extracts the entitled model ids from a LiteLLM
// "team not allowed to access model" 403 body. It matches the
// models=[...] array (single- or double-quoted ids) that the gateway
// helpfully includes. Model ids are captured verbatim (prefixes intact).
var team403Pattern = regexp.MustCompile(`models\s*=\s*\[([^\]]*)\]`)

// team403Marker is the stable phrase LiteLLM uses for a team-scope denial.
// Only 403 bodies containing it are parsed as entitlement signals, so an
// unrelated 403 (bad key, rate limit) never narrows the dropdown.
const team403Marker = "team not allowed to access model"

// entitlementEntry is a cached entitled set for one gateway endpoint.
type entitlementEntry struct {
	models []string // entitled model ids, verbatim; nil/empty = unknown
	source string   // "key-info" or "403" — which signal produced the set
	keyFP  string   // fingerprint of the API key the set belongs to
	at     time.Time
}

// probeParams remembers how to re-probe an endpoint (the key and CA bundle of
// the last litellm route set for it) so a stale entry can be refreshed in the
// background without waiting for the next SetInferenceRoute.
type probeParams struct {
	apiKey   string
	caBundle string
}

// entitlementStore caches the entitled model set per LiteLLM endpoint URL.
// Endpoints (not agents) are the natural key: every agent routed to the same
// gateway shares one configured key and thus one entitled set, and the 403
// that reveals it carries no agent identity. Each entry records a fingerprint
// of the key that produced it so a key swap invalidates the old scope instead
// of serving it for the new key.
type entitlementStore struct {
	mu       sync.RWMutex
	byEndpt  map[string]entitlementEntry
	probeBy  map[string]probeParams
	inFlight map[string]bool // endpoints with a key-info probe in progress
}

func newEntitlementStore() *entitlementStore {
	return &entitlementStore{
		byEndpt:  make(map[string]entitlementEntry),
		probeBy:  make(map[string]probeParams),
		inFlight: make(map[string]bool),
	}
}

// normalizeEndpoint trims a trailing slash so http://x and http://x/ share one
// cache entry.
func normalizeEndpoint(endpoint string) string {
	return strings.TrimRight(endpoint, "/")
}

// keyFingerprint derives a non-reversible identity for an API key so entries
// can be bound to the key that produced them without storing the key itself
// alongside the (log-adjacent) entitlement data.
func keyFingerprint(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:8])
}

// rememberProbe records the credentials needed to re-probe an endpoint's
// entitlements later (stale refresh). If the key changed since the cached
// entitled set was captured, the entry is dropped — the old key's scope says
// nothing about the new key's.
func (e *entitlementStore) rememberProbe(endpoint, apiKey, caBundle string) {
	key := normalizeEndpoint(endpoint)
	fp := keyFingerprint(apiKey)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.probeBy[key] = probeParams{apiKey: apiKey, caBundle: caBundle}
	if entry, ok := e.byEndpt[key]; ok && entry.keyFP != fp {
		delete(e.byEndpt, key)
	}
}

// probeParams returns the remembered re-probe credentials for an endpoint.
func (e *entitlementStore) probeParams(endpoint string) (ep, apiKey, caBundle string, ok bool) {
	key := normalizeEndpoint(endpoint)
	e.mu.RLock()
	defer e.mu.RUnlock()
	p, ok := e.probeBy[key]
	if !ok {
		return "", "", "", false
	}
	return key, p.apiKey, p.caBundle, true
}

// beginProbe marks an endpoint as having a key-info probe in flight. It
// returns false when one is already running so concurrent triggers (route
// sets, stale-refresh reads) collapse to a single upstream probe.
func (e *entitlementStore) beginProbe(endpoint string) bool {
	key := normalizeEndpoint(endpoint)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.inFlight[key] {
		return false
	}
	e.inFlight[key] = true
	return true
}

// endProbe clears the in-flight marker set by beginProbe.
func (e *entitlementStore) endProbe(endpoint string) {
	key := normalizeEndpoint(endpoint)
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.inFlight, key)
}

// set records an entitled set for an endpoint, bound to the key that produced
// it. An empty models slice clears the entry (unknown), so a wildcard/
// all-access key never pins a stale subset.
func (e *entitlementStore) set(endpoint, apiKey string, models []string, source string) {
	key := normalizeEndpoint(endpoint)
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(models) == 0 {
		delete(e.byEndpt, key)
		return
	}
	e.byEndpt[key] = entitlementEntry{models: models, source: source, keyFP: keyFingerprint(apiKey), at: time.Now()}
}

// get returns the entitled set for an endpoint, whether one is known, and
// whether it is stale (older than entitlementTTL). A stale entry is still
// returned (known=true) so the served model list stays stable for an
// unchanged key; the caller uses stale=true to trigger a background refresh
// that overwrites the entry only when a fresh signal arrives.
func (e *entitlementStore) get(endpoint string) (models []string, source string, known bool, stale bool) {
	key := normalizeEndpoint(endpoint)
	e.mu.RLock()
	defer e.mu.RUnlock()
	entry, ok := e.byEndpt[key]
	if !ok || len(entry.models) == 0 {
		return nil, "", false, false
	}
	// Return a copy so callers cannot mutate the cached slice.
	out := make([]string, len(entry.models))
	copy(out, entry.models)
	return out, entry.source, true, time.Since(entry.at) > entitlementTTL
}

// entitledModelKey reduces a model id to a drift-insensitive comparison key:
// the segment after the last provider prefix "/" with every "." folded to "-",
// case-insensitively. This is the same tolerance the dashboard's preselect
// matcher applies (prefix-stripped tail + dot/hyphen normalization, #4262) —
// but here it is used to RESOLVE the outbound id, never to mask drift.
func entitledModelKey(id string) string {
	tail := id
	if i := strings.LastIndex(id, "/"); i >= 0 {
		tail = id[i+1:]
	}
	return strings.ToLower(strings.ReplaceAll(tail, ".", "-"))
}

// resolveEntitledModelID maps a stored model id onto the exact entitled
// gateway id it drifted from. LiteLLM matches the request model VERBATIM
// against the team's entitled set — "claude-sonnet-4.6" is refused with a
// team-scope 403 even when "aws/claude-sonnet-4-6" is entitled (#4400: a
// default agent carried its copilot-spelled dotted model id into a litellm
// backend switch and 403'd forever, while an agent added later with an id
// picked from discovery worked — same model, different spelling).
//
// Resolution is deliberately conservative:
//   - an id already present verbatim in the entitled set is returned as-is;
//   - otherwise, if EXACTLY ONE entitled id shares the model's comparison key
//     (entitledModelKey: prefix-stripped, dot/hyphen-folded, case-folded),
//     that entitled id is returned;
//   - zero or multiple candidates return the input unchanged — an ambiguous
//     rewrite could silently bill a different provider's deployment.
func resolveEntitledModelID(entitled []string, model string) (string, bool) {
	if model == "" || len(entitled) == 0 {
		return model, false
	}
	for _, id := range entitled {
		if id == model {
			return model, false
		}
	}
	key := entitledModelKey(model)
	match := ""
	for _, id := range entitled {
		if entitledModelKey(id) != key {
			continue
		}
		if match != "" {
			return model, false // ambiguous — never guess between providers
		}
		match = id
	}
	if match == "" {
		return model, false
	}
	return match, true
}

// parseTeam403Models extracts the entitled model ids from a LiteLLM
// "team not allowed to access model" 403 body. It returns nil when the body is
// not a team-scope denial or carries no models=[...] list, so callers can
// distinguish "no entitlement signal" from "entitled to nothing". Ids are
// returned verbatim with provider prefixes intact.
func parseTeam403Models(body string) []string {
	if !strings.Contains(body, team403Marker) {
		return nil
	}
	m := team403Pattern.FindStringSubmatch(body)
	if len(m) < 2 {
		return nil
	}
	return splitQuotedModelList(m[1])
}

// splitQuotedModelList parses a comma-separated list of single- or
// double-quoted model ids (the inside of a models=[...] array) into a slice.
// Empty entries and whitespace are ignored; quotes are stripped but the id
// itself — including provider prefixes and hyphens/dots — is kept verbatim.
func splitQuotedModelList(inner string) []string {
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		id := strings.TrimSpace(p)
		id = strings.Trim(id, `'"`)
		id = strings.TrimSpace(id)
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// keyInfoResponse is the subset of LiteLLM's GET /key/info and /user/info
// payloads carrying the entitled model set. LiteLLM nests it under an "info"
// object (key_info / user_info shapes both use this), with the allowed models
// in "models". A models list of ["*"] (or ["all-proxy-models"]) means the key
// is unrestricted.
type keyInfoResponse struct {
	Info struct {
		Models []string `json:"models"`
	} `json:"info"`
	// Some deployments return models at the top level instead of under info.
	Models []string `json:"models"`
}

// entitledModelsFromKeyInfo returns the entitled model set carried by a
// /key/info or /user/info payload, or nil when the payload grants all models
// (wildcard) or carries no usable list.
func entitledModelsFromKeyInfo(body []byte) []string {
	var r keyInfoResponse
	if json.Unmarshal(body, &r) != nil {
		return nil
	}
	models := r.Info.Models
	if len(models) == 0 {
		models = r.Models
	}
	return sanitizeEntitledModels(models)
}

// sanitizeEntitledModels drops empty ids and returns nil when the set grants
// all models (a "*" or "all-proxy-models" wildcard), signalling "no
// restriction" rather than "entitled to a literal wildcard id".
func sanitizeEntitledModels(models []string) []string {
	out := make([]string, 0, len(models))
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if m == entitlementWildcard || m == "all-proxy-models" || m == "all-team-models" {
			// Unrestricted key — no subset to enforce.
			return nil
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// probeEntitledModels best-effort-queries a LiteLLM gateway's per-key info
// endpoints for the model set the given key is entitled to. It tries
// /key/info then /user/info and returns the first non-wildcard set found, or
// nil when neither endpoint yields one (the caller then relies on the 403-parse
// path). It never returns an error: a probe failure is simply "no signal".
func probeEntitledModels(endpoint, apiKey string, transport http.RoundTripper) []string {
	if apiKey == "" {
		return nil
	}
	client := &http.Client{Timeout: entitlementProbeTimeout}
	if transport != nil {
		client.Transport = transport
	}
	base := normalizeEndpoint(endpoint)
	for _, path := range []string{entitlementProbePathKeyInfo, entitlementProbePathUserInfo} {
		req, err := http.NewRequest("GET", base+path, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		if models := entitledModelsFromKeyInfo(body); len(models) > 0 {
			return models
		}
	}
	return nil
}
