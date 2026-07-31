package hub

// GitHub identity as an ATOMIC SET.
//
// ─────────────────────────────────────────────────────────────────────────
// THE RULE
// ─────────────────────────────────────────────────────────────────────────
//
// app_id, app_slug, api_url and base_url together name ONE GitHub App on ONE
// forge. No valid mixture exists:
//
//	GHE:    app_id=<ghe app>    slug=<ghe slug>    api_url=https://<host>/api/v3
//	Public: app_id=<public app> slug=<public slug> api_url="" (or api.github.com)
//
// ─────────────────────────────────────────────────────────────────────────
// WHY THIS FILE EXISTS
// ─────────────────────────────────────────────────────────────────────────
//
// A cluster-config change pushed a GHE app_id to a set of hives WITHOUT also
// pushing api_url. Those spokes ended up with:
//
//	app_id: 5686          <- the GHE App
//	api_url: ""           <- empty, so it defaults to api.github.com
//
// and every token request failed on hives that had been working an hour
// earlier:
//
//	POST https://api.github.com/app/installations/146551814/access_tokens
//	404 Integration not found
//
// The hives that also received api_url=https://github.ibm.com/api/v3 were
// fine. The failure split exactly on that one field.
//
// #2360's forge endpoint already refuses to half-apply an identity — it
// resolves the whole set up front and 409s naming what is missing, writing
// nothing. The cluster-config push path had no equivalent guard, so it could
// deliver one component of the set and leave the rest stale.
//
// This file supplies that guard in the form the transport actually allows.
//
// ─────────────────────────────────────────────────────────────────────────
// WHY THIS IS A PRECONDITION AND NOT A DELIVERY LATCH
// ─────────────────────────────────────────────────────────────────────────
//
// The obvious fix is a "set-valued" delivery latch — push the whole identity,
// wait for the spoke to confirm all of it. That is NOT expressible on the
// current transport, for two independent reasons:
//
//  1. The set is split across two heartbeat payloads with different gating:
//     app_id/app_slug/installation_id ride HeartbeatGitHubAppConfig, api_url
//     rides HeartbeatProjectConfig, and base_url is never pushed at all (the
//     spoke derives its host from base_url with a fallback to api_url, so
//     pushing api_url alone moves a hive between forges).
//
//  2. Until the read-back fields added alongside this file, the hub received
//     back only app_id and the key fingerprint. app_slug and installation_id
//     were structurally unconfirmable — a latch over them could arm and push
//     but could never legitimately confirm.
//
// So the guarantee is provided where it can be: REFUSE TO ARM an inconsistent
// push, and DETECT an inconsistent state that is already live. That gives the
// atomicity property (never half-apply) without pretending three transports
// are one.

import "strings"

// gheAPIPathMarker identifies a GitHub Enterprise API URL. GHE APIs live under
// /api/v3; public GitHub uses api.github.com.
const gheAPIPathMarker = "/api/v3"

// publicGitHubHost is declared in server.go and reused here.

// PendingAppIdentity is a GitHub App identity queued for delivery to a spoke.
//
// It is the durable replacement for the in-memory one-shot map. Every field is
// NON-SECRET: the private key is never persisted here and continues to travel
// only through the per-cluster key reconcile, which gates each push on
// HasKey().
type PendingAppIdentity struct {
	// AppID is the App the spoke should authenticate as.
	AppID int64 `json:"app_id,omitempty"`
	// AppSlug is the App's URL slug on the target forge.
	AppSlug string `json:"app_slug,omitempty"`
	// InstallationID is the installation on the target forge. Zero means
	// "leave the spoke's alone" — an ID from one forge names nothing on
	// another, so a forge switch deliberately never carries it across.
	InstallationID int64 `json:"installation_id,omitempty"`
	// APIURL is the GitHub API base for the target forge. Empty means public
	// GitHub. Recorded so the consistency of the queued SET can be checked
	// before it is armed, even though it travels on a different payload.
	APIURL string `json:"api_url,omitempty"`
	// ArmedAt is when this identity was queued, so an outstanding delivery that
	// never confirms is visible rather than merely absent.
	ArmedAt string `json:"armed_at,omitempty"`
	// Reason records who armed it and why, for diagnosis.
	Reason string `json:"reason,omitempty"`
}

// IdentitySet is a complete GitHub identity, from any source: what a spoke
// reports, what the hub intends to push, or what a cluster config declares.
type IdentitySet struct {
	AppID          int64
	AppSlug        string
	InstallationID int64
	APIURL         string
	BaseURL        string
}

// IsGHE reports whether this identity names a GitHub Enterprise forge, judged
// on the API URL — the field that actually decides where token requests go.
func (s IdentitySet) IsGHE() bool {
	return strings.Contains(strings.ToLower(s.APIURL), gheAPIPathMarker)
}

// IdentitySetIssues returns human-readable descriptions of every internal
// inconsistency in an identity set, or nil when it is coherent.
//
// It DETECTS and NAMES; it never mutates. Callers decide whether to refuse
// (arming a new push) or merely report (an already-live spoke).
//
// It is deliberately conservative: a field that is absent is treated as
// UNKNOWN, never as a mismatch, because the majority of the fleet legitimately
// runs public GitHub with an empty api_url. Firing on that would make the
// signal noise on ~48 hives and it would be ignored.
func IdentitySetIssues(s IdentitySet) []string {
	// No App configured: a token-auth hive has no identity to be inconsistent
	// about.
	if s.AppID == 0 {
		return nil
	}

	var issues []string
	apiIsGHE := s.IsGHE()
	slugIsGHE := strings.Contains(strings.ToLower(s.AppSlug), "ghe")
	baseIsGHE := s.BaseURL != "" && !strings.Contains(strings.ToLower(s.BaseURL), publicGitHubHost)

	// THE LIVE REGRESSION. A GHE App presented to api.github.com returns
	// "404 Integration not found" on every token request.
	if slugIsGHE && !apiIsGHE {
		issues = append(issues, "app_slug "+s.AppSlug+" names a GitHub Enterprise App but api_url is "+
			describeAPIURL(s.APIURL)+
			" — a GHE app_id presented to api.github.com returns 404 Integration not found")
	}

	// base_url and api_url must name the same forge. base_url is never pushed,
	// so a mismatch here means a push moved api_url and left base_url stale
	// (or the reverse).
	if baseIsGHE && !apiIsGHE {
		issues = append(issues, "base_url is "+s.BaseURL+" but api_url is "+describeAPIURL(s.APIURL)+
			" — base_url and api_url must name the same forge")
	}
	if apiIsGHE && s.BaseURL != "" && !baseIsGHE {
		issues = append(issues, "api_url is "+s.APIURL+" but base_url is "+s.BaseURL+
			" — base_url and api_url must name the same forge")
	}

	return issues
}

// describeAPIURL renders an api_url for an operator message, making the empty
// case explicit. An empty api_url is precisely the condition that caused the
// outage, so it must read as a stated value rather than as a blank.
func describeAPIURL(apiURL string) string {
	if strings.TrimSpace(apiURL) == "" {
		return "empty (which defaults to api.github.com)"
	}
	return apiURL
}

// ValidateIdentityPush reports why a queued identity must not be armed, or ""
// when it is safe.
//
// This is the refusal half, and it is the guard the cluster-config push path
// lacked. `want` is the identity the hub intends the spoke to end up with,
// assembled from every component the push will change plus the spoke's current
// values for the components it will not.
//
// Refusing here mirrors #2360's forge endpoint, which resolves the whole
// identity up front and writes NOTHING when it cannot — an atomic refusal is
// always preferable to a half-applied identity, because the half-applied state
// authenticates as nothing and is invisible to the hub.
func ValidateIdentityPush(want IdentitySet) string {
	issues := IdentitySetIssues(want)
	if len(issues) == 0 {
		return ""
	}
	return "refusing to push an inconsistent GitHub identity: " + strings.Join(issues, "; ")
}

// pendingAppIdentityForHeartbeat returns the App config to deliver on this
// beat, or nil when nothing is outstanding.
//
// It supersedes the in-memory one-shot map. The difference that matters: this
// keeps delivering until the spoke REPORTS THE IDENTITY BACK, rather than
// dropping the queued config on the first beat that carries it. A beat that
// is lost, or that the spoke fails to apply, is retried; a hub restart no
// longer discards the request.
//
// The in-memory map is still consulted first so anything queued by an older
// code path in the same process is not stranded.
func (s *HubServer) pendingAppIdentityForHeartbeat(p *HeartbeatPayload) *HeartbeatGitHubAppConfig {
	if p == nil {
		return nil
	}
	// Legacy in-memory queue takes precedence: it may carry a private key,
	// which the durable record deliberately never stores.
	if cfg := s.consumePendingGitHubAppConfig(p.HiveID); cfg != nil {
		return cfg
	}

	h := loadSaaSHive(p.HiveID)
	if h == nil || h.PendingAppConfig == nil {
		return nil
	}
	pend := h.PendingAppConfig

	// Confirmed? Clear the request and stop pushing. Confirmation requires the
	// spoke to report back every component the hub asked for — this is exactly
	// what the app_slug/installation_id read-back fields make possible.
	if identityDelivered(pend, p) {
		h.PendingAppConfig = nil
		if err := saveSaaSHive(h); err != nil {
			s.logger.Warn("identity: failed to clear delivered app config",
				"hive_id", p.HiveID, "error", err)
		}
		s.logger.Info("identity: app identity delivered and confirmed by spoke",
			"hive_id", p.HiveID, "app_id", pend.AppID, "app_slug", pend.AppSlug)
		return nil
	}

	return &HeartbeatGitHubAppConfig{
		AppID:          pend.AppID,
		InstallationID: pend.InstallationID,
		AppSlug:        pend.AppSlug,
		// PrivateKey deliberately empty: key material is owned by the
		// per-cluster reconcile, which gates every push on HasKey(). An empty
		// key already means "leave the spoke's key alone", so this can never
		// blank a working credential.
	}
}

// identityDelivered reports whether the spoke's report matches everything the
// hub asked for.
//
// Only NON-ZERO requested components are checked: a zero installation_id means
// "leave the spoke's alone", so it is not something to confirm. An empty
// report for a requested field is UNKNOWN — a spoke too old to report it must
// never be treated as having confirmed, which is the silent-failure case.
func identityDelivered(want *PendingAppIdentity, p *HeartbeatPayload) bool {
	if want == nil || p == nil {
		return false
	}
	if want.AppID != 0 && p.GitHubAppID != want.AppID {
		return false
	}
	if want.AppSlug != "" && !strings.EqualFold(p.GitHubAppSlug, want.AppSlug) {
		return false
	}
	if want.InstallationID != 0 && p.GitHubInstallationID != want.InstallationID {
		return false
	}
	return true
}

// SpokeIdentityFromPayload assembles the identity a spoke reports it is
// running, for drift detection.
//
// Before the read-back fields this could not be built: the hub received only
// app_id and the key fingerprint, so a half-applied identity was
// indistinguishable from a healthy one.
func SpokeIdentityFromPayload(p *HeartbeatPayload) IdentitySet {
	if p == nil {
		return IdentitySet{}
	}
	return IdentitySet{
		AppID:          p.GitHubAppID,
		AppSlug:        p.GitHubAppSlug,
		InstallationID: p.GitHubInstallationID,
		APIURL:         p.GitHubAPIURL,
		BaseURL:        p.GitHubBaseURL,
	}
}
