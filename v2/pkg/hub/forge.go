package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ============================================================================
// FORGE REGISTRY
// ============================================================================
//
// A "forge" is the code-hosting system a hive's org and repos live on. Today
// every hive is on a GitHub of some kind — public github.com or a GitHub
// Enterprise instance (github.ibm.com, github.cisco.com, …) — but GitLab and
// Gitea are coming, and the hub already has one endpoint (request-provision)
// whose design note calls that out explicitly.
//
// The switch endpoint therefore validates against a KIND plus a HOST rather
// than accepting an arbitrary string. Adding a forge kind later means adding a
// case here and an apiURLFor* helper; it does not mean revisiting the handler,
// the authorization, or the delivery path.

// ForgeKind names a family of code-hosting system. The zero value is invalid:
// a caller must say which forge it means.
type ForgeKind string

const (
	// ForgeGitHub is public github.com — the fleet default.
	ForgeGitHub ForgeKind = "github"
	// ForgeGitHubEnterprise is a self-hosted GitHub Enterprise Server instance.
	// It differs from ForgeGitHub only in host and API path shape, which is why
	// both resolve through the same identity/credential machinery.
	ForgeGitHubEnterprise ForgeKind = "github-enterprise"
)

// publicForgeHost is the canonical host of public GitHub. A hive on this host
// carries GitHubHost == "github.com" (or "", historically) and an empty
// api_url, because the spoke's own default is already api.github.com.
const publicForgeHost = "github.com"

// gheAPIPathSuffix is the API path every GitHub Enterprise Server instance
// serves its v3 REST API on. It is a property of the GHE product, not of any
// particular instance, so it is the same for github.ibm.com and every future
// enterprise host.
const gheAPIPathSuffix = "/api/v3"

// maxForgeHostLen bounds an operator-supplied host label. Real hostnames are
// far shorter; this exists so a pasted blob can never reach the registry, the
// meta.json on the PVC, or a log line.
const maxForgeHostLen = 253 // RFC 1035 maximum length of a DNS name

// ForgeTarget is a validated forge destination: the kind, its bare host label,
// and the derived web/API URLs the spoke needs. It is produced only by
// parseForgeTarget, so every consumer can rely on the values being normalized.
type ForgeTarget struct {
	Kind ForgeKind
	// Host is the bare, lower-cased hostname ("github.com", "github.ibm.com").
	Host string
	// BaseURL is the web base ("https://github.ibm.com"). Empty for public
	// GitHub, matching the codebase-wide convention that "" means public — see
	// effectiveGitHubBaseURL and SaaSHive.GitHubHost.
	BaseURL string
	// APIURL is the REST API base ("https://github.ibm.com/api/v3"). Empty for
	// public GitHub, which the spoke already defaults to.
	APIURL string
}

// IsPublic reports whether this target is public github.com.
func (t ForgeTarget) IsPublic() bool { return t.Kind == ForgeGitHub }

// parseForgeTarget validates an operator-supplied forge host and resolves it to
// a ForgeTarget.
//
// It accepts a bare host ("github.ibm.com") or a full URL
// ("https://github.ibm.com/") and normalizes both to the same target, because
// an operator pasting an org URL into this field is the overwhelmingly likely
// input — the same paste that, in the repo field, produced the malformed value
// that crash-looped a spoke.
//
// It deliberately does NOT accept arbitrary strings. An unrecognized host shape
// is rejected with a message naming what is supported, rather than being
// recorded and silently producing a spoke that talks to nothing.
func parseForgeTarget(raw string) (ForgeTarget, error) {
	// Deliberately NOT githubHostLabel: that helper maps "" to "github.com",
	// which would silently turn a missing/blank host into a switch to public
	// GitHub — a destructive default for an endpoint whose whole job is to move
	// a hive. An absent host must be an error, not an assumption.
	host := strings.ToLower(strings.TrimSpace(raw))
	if host == "" {
		return ForgeTarget{}, fmt.Errorf("forge host is required (e.g. %q or %q)", publicForgeHost, "github.ibm.com")
	}
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	// Strip any path: an operator is most likely to paste an org URL
	// ("https://github.ibm.com/enricom-ibm"), and the host is the only part of
	// it this endpoint is asking for.
	host = strings.SplitN(host, "/", 2)[0]
	// Strip a port and any userinfo, neither of which belongs in a host label.
	host = strings.SplitN(host, ":", 2)[0]
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	if host == "" {
		return ForgeTarget{}, fmt.Errorf("forge host is required (e.g. %q or %q)", publicForgeHost, "github.ibm.com")
	}
	if len(host) > maxForgeHostLen {
		return ForgeTarget{}, fmt.Errorf("forge host is too long (max %d characters)", maxForgeHostLen)
	}
	// api.github.com is the API host, not the web host; an operator naming it
	// means public GitHub. Normalizing it here keeps one canonical record for
	// public rather than two that compare unequal.
	if host == publicForgeHost || host == "api.github.com" {
		return ForgeTarget{Kind: ForgeGitHub, Host: publicForgeHost}, nil
	}
	if !isValidForgeHostLabel(host) {
		return ForgeTarget{}, fmt.Errorf("%q is not a valid forge host — supply a hostname such as %q or %q", raw, publicForgeHost, "github.ibm.com")
	}
	// Everything that is a well-formed host but is not github.com is treated as
	// a GitHub Enterprise Server instance. That is true of every non-public
	// forge the fleet runs today. When GitLab/Gitea arrive they will be selected
	// by an explicit kind rather than inferred from the host, because their API
	// paths differ and a host alone cannot distinguish them.
	return ForgeTarget{
		Kind:    ForgeGitHubEnterprise,
		Host:    host,
		BaseURL: "https://" + host,
		APIURL:  "https://" + host + gheAPIPathSuffix,
	}, nil
}

// isValidForgeHostLabel reports whether s is a plausible DNS hostname: at least
// one dot, and only characters legal in a host label. It is intentionally strict
// — this value is written to meta.json, rendered in the dashboard, and used to
// build a URL the spoke will call.
func isValidForgeHostLabel(s string) bool {
	if s == "" || !strings.Contains(s, ".") {
		return false
	}
	if strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") || strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '.' || r == '-':
		default:
			return false
		}
	}
	return true
}

// ============================================================================
// SWITCH ENDPOINT
// ============================================================================

// SwitchForgeRequest is the body of POST /api/saas/hives/{id}/forge.
type SwitchForgeRequest struct {
	// Host is the forge to move this hive to: "github.com" or a GitHub
	// Enterprise host. Required.
	Host string `json:"host"`
	// InstallationID is the GitHub App installation ID on the TARGET forge.
	//
	// An installation ID is issued by one forge and is meaningless on any
	// other, so switching forges necessarily invalidates the current one. It is
	// therefore never carried over: it is either supplied here, or left for
	// installation auto-discovery to resolve once the App is installed on the
	// target org.
	//
	// Optional. Empty means "clear it and let discovery re-resolve it" — see
	// the handler comment on why clearing is the safe default rather than
	// keeping a value that is provably wrong.
	InstallationID string `json:"installation_id,omitempty"`
}

// switchForgeResponse is the operator-facing result. It reports the App
// identity the hive will now authenticate as, so the operator can see whether
// the target cluster actually has an App configured before waiting on a beat.
type switchForgeResponse struct {
	Status          string `json:"status"`
	ID              string `json:"id"`
	FromHost        string `json:"from_host"`
	ToHost          string `json:"to_host"`
	ForgeKind       string `json:"forge_kind"`
	APIURL          string `json:"api_url,omitempty"`
	AppID           string `json:"app_id,omitempty"`
	InstallationID  string `json:"installation_id,omitempty"`
	AppInstallNote  string `json:"app_install_note,omitempty"`
	DeliveryPending bool   `json:"delivery_pending"`
}

// forgeSwitchNeedsInstallNote is the operator-facing caveat returned when a
// forge switch leaves the hive with no installation ID. The hive will not
// authenticate until the App is installed on the target org (or an ID is
// supplied), and saying so in the response is what stops an operator concluding
// the switch silently failed.
const forgeSwitchNeedsInstallNote = "installation_id was cleared because it belongs to the previous forge; install the GitHub App on this org on the target forge (or re-run with installation_id) before the hive can authenticate"

// handleSwitchForge moves ONE hive to a different forge.
//
// AUTHORIZATION mirrors handleSwitchBranch / handleToggleAutoUpgrade exactly:
// requireAuth on the route, plus an inner owner-or-admin check. The forge is a
// property of where the owner's code lives, so the owner may set it; an admin
// may set it on anyone's hive because moving a user between forges is precisely
// the operator task this exists for.
//
// # WHY A HUB-SIDE EDIT ALONE IS NOT ENOUGH
//
// Editing github_host in the hive's meta.json and restarting the hub does NOT
// stick — verified on the live fleet. The registry reverts to the spoke's value
// within one beat, because handleHeartbeat records RegistryEntry.GitHubHost
// from the spoke's own HeartbeatPayload.GitHubHost on every beat, and the spoke
// derives that from its OWN config via GitHubConfig.HostLabel(). Nothing the
// hub writes to meta.json changes what the spoke reports, so the hub's edit is
// overwritten by the next report of the unchanged truth.
//
// This is the same class of bug as the ACMM revert fixed in #2354: hub-side
// state that the spoke also reports is always won by the spoke unless the hub
// PUSHES the change and waits for the spoke to report it back.
//
// The fix follows #2354's shape. The switch records the target on the hive and
// re-arms a delivery flag (ForgeDelivered=false); projectConfigForHiveID then
// pushes github_api_url down the heartbeat until the spoke reports a matching
// host, at which point the flag latches true and the push stops for good.
//
// Crucially, HostLabel() reads base_url and FALLS BACK TO api_url, so pushing
// the API URL alone is sufficient to change what the spoke reports — the
// existing GitHubAPIURL field on HeartbeatProjectConfig is a complete delivery
// channel for this, already implemented on both ends.
func (s *HubServer) handleSwitchForge(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isTrustedOrigin(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/json")

	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	if h.Owner != username && username != hubAdminUsername {
		http.Error(w, `{"error":"only the owner can switch forges"}`, http.StatusForbidden)
		return
	}

	var body SwitchForgeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	target, err := parseForgeTarget(body.Host)
	if err != nil {
		http.Error(w, `{"error":`+strconv.Quote(err.Error())+`}`, http.StatusBadRequest)
		return
	}

	// An installation ID, when supplied, must be numeric. Rejecting here rather
	// than storing garbage keeps the spoke from being handed a value it will
	// fail on with a far less obvious error.
	var installID int64
	if raw := strings.TrimSpace(body.InstallationID); raw != "" {
		installID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || installID <= 0 {
			http.Error(w, `{"error":"installation_id must be a positive number"}`, http.StatusBadRequest)
			return
		}
	}

	fromHost := h.GitHubHost
	if fromHost == "" {
		fromHost = publicForgeHost // "" has always meant public github.com
	}

	// Resolve the App identity for the TARGET forge from CLUSTER CONFIG. No App
	// ID is hardcoded anywhere in Go: hive-oke names its github.com App and
	// vllm-d names its github.ibm.com App, both in clusters.json, exactly as
	// resolveProvisionAppID already reads them.
	cluster := s.clusterForHive(h)
	appID := s.forgeAppIDForTarget(cluster, target)

	// Record the target. GitHubHost is what projectConfigForHiveID derives the
	// pushed api_url from; GitHubBaseURL/GitHubAPIURL pin the hive explicitly so
	// a later cluster-default change cannot silently move it back.
	h.GitHubHost = target.Host
	if target.IsPublic() {
		// The sentinel, not "": an explicit public pin must survive a cluster
		// whose defaults point at GHE, which is what githubHostPublic means to
		// effectiveGitHubBaseURL / effectiveGitHubAPIURL / resolveProvisionAppID.
		h.GitHubBaseURL = githubHostPublic
		h.GitHubAPIURL = githubHostPublic
	} else {
		h.GitHubBaseURL = target.BaseURL
		h.GitHubAPIURL = target.APIURL
	}

	// Re-arm delivery. This is the #2354 handshake: the requested host is
	// authoritative until the spoke reports it back, at which point the push
	// stops permanently and the spoke owns the value again.
	h.RequestedGitHubHost = target.Host
	h.ForgeDelivered = false

	if err := saveSaaSHive(h); err != nil {
		s.logger.Error("forge switch: failed to persist hive", "hive_id", id, "error", err)
		http.Error(w, `{"error":"failed to save hive"}`, http.StatusInternalServerError)
		return
	}

	// Keep the in-memory registry in step so the dashboard reflects the switch
	// immediately rather than after the next beat. The spoke's own report will
	// overwrite this within a beat or two — that is the point of the push — but
	// showing the stale host in the meantime reads as "the switch did nothing".
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			s.registry.Hives[i].GitHubHost = target.Host
			break
		}
	}
	s.mu.Unlock()

	// Swap the App identity. installation_id is per-forge and is NEVER carried
	// over: an ID issued by github.com names nothing on github.ibm.com, and
	// pushing it would authenticate as a nonexistent installation — a confusing
	// 401/404 that looks like a key problem rather than a forge problem.
	//
	// Sending InstallationID: 0 is the spoke's documented "not speaking to this
	// field" value, so a bare app_id switch leaves the spoke's stale ID in
	// place. That is why an explicit clear is queued below via the hive's own
	// record rather than relying on the heartbeat field alone.
	deliveryPending := false
	if appID > 0 {
		s.storePendingGitHubAppConfig(id, &HeartbeatGitHubAppConfig{
			AppID: appID,
			// Only a supplied ID is sent. Zero means "leave it alone" on the
			// wire; the operator note below tells the operator it must be
			// installed or supplied, so the failure is never silent.
			InstallationID: installID,
			AppSlug:        forgeAppSlugForCluster(cluster),
			// PrivateKey is deliberately absent. The per-cluster key reconcile
			// (cluster_app_key.go) delivers key material on its own schedule and
			// gates every key push on HasKey(); duplicating it here would risk
			// blanking a working key with an empty string.
		})
		deliveryPending = true
	}

	note := ""
	if installID == 0 {
		note = forgeSwitchNeedsInstallNote
	}

	s.logger.Info("audit: hive forge switched",
		"hive_id", id,
		"actor", username,
		"owner", h.Owner,
		"org", h.Org,
		"from_host", fromHost,
		"to_host", target.Host,
		"forge_kind", string(target.Kind),
		"api_url", target.APIURL,
		"app_id", appID,
		"installation_id_supplied", installID != 0,
		"cluster", clusterIDForHive(h),
	)
	s.recordTimeline(id, TimelineOwnership,
		fmt.Sprintf("forge switched from %s to %s", fromHost, target.Host), username)

	resp := switchForgeResponse{
		Status:          "switched",
		ID:              id,
		FromHost:        fromHost,
		ToHost:          target.Host,
		ForgeKind:       string(target.Kind),
		APIURL:          target.APIURL,
		InstallationID:  strings.TrimSpace(body.InstallationID),
		AppInstallNote:  note,
		DeliveryPending: deliveryPending,
	}
	if appID > 0 {
		resp.AppID = strconv.FormatInt(appID, 10)
	}
	json.NewEncoder(w).Encode(resp)
}

// forgeAppIDForTarget resolves the GitHub App ID to authenticate as on the
// TARGET forge, from cluster config only.
//
// The cluster's configured app_id is correct exactly when the cluster's own
// GitHub host matches the target — hive-oke is a github.com cluster and vllm-d
// is a github.ibm.com cluster, so a hive switching to the cluster's own forge
// gets the cluster's App. A hive switching to a forge the cluster does NOT
// default to has no App the hub can name, and returning 0 is the honest answer:
// zero means "say nothing about the App", which leaves the spoke's credentials
// untouched rather than pushing an App ID from the wrong forge.
//
// No App ID is hardcoded. This reads ClusterConfig.GitHubAppID, the same
// non-secret field resolveProvisionAppID uses.
func (s *HubServer) forgeAppIDForTarget(cluster *ClusterConfig, target ForgeTarget) int64 {
	if cluster == nil || cluster.GitHubAppID == 0 {
		return 0
	}
	clusterHost := publicForgeHost
	if cluster.GitHubBaseURL != "" {
		clusterHost = strings.ToLower(githubHostLabel(cluster.GitHubBaseURL))
	} else if cluster.GitHubAPIURL != "" {
		clusterHost = strings.ToLower(githubHostLabel(cluster.GitHubAPIURL))
	}
	if !sameGitHubHost(clusterHost, target.Host) {
		return 0
	}
	return cluster.GitHubAppID
}

// forgeAppSlugForCluster returns the cluster's App slug, or "" when it has
// none. Empty is the spoke's "leave my slug alone" value, so this never blanks
// a working install link.
func forgeAppSlugForCluster(cluster *ClusterConfig) string {
	if cluster == nil {
		return ""
	}
	return cluster.GitHubAppSlug
}

// ============================================================================
// FORGE DELIVERY HANDSHAKE
// ============================================================================

// pendingForgeAPIURL returns the github_api_url the hub must still push to
// deliver an outstanding forge switch, or "" when there is nothing to deliver.
//
// It is the push half of the handshake described on SaaSHive.RequestedGitHubHost.
// The delivery is bounded and self-limiting: it fires only while
// RequestedGitHubHost is set and ForgeDelivered is false, and adoptSpokeForge
// flips ForgeDelivered the moment the spoke reports the requested host. Once
// that happens this returns "" forever, so a switch cannot become a permanent
// push that overrides a later operator change on the spoke — the failure mode
// #2061 hit with ACMM.
//
// curAPIURL is what the spoke reports it is CURRENTLY running against.
//
// # WHY SWITCHING TO PUBLIC GITHUB NEEDS AN EXPLICIT URL
//
// Everywhere else in this codebase an empty github_api_url means "leave the
// spoke's value unchanged" — which is exactly right when the hub has nothing
// to say. But it makes a switch FROM GitHub Enterprise TO public github.com
// undeliverable: the natural value to send is "", and "" is precisely the
// value the spoke ignores, so a GHE spoke would keep its GHE api_url forever
// and keep reporting github.ibm.com. So the public target is delivered as the
// EXPLICIT config.DefaultGitHubAPIURL rather than as emptiness. The spoke
// already defaults to that string, and it resolves through HostLabel() to
// "github.com", which is what closes the read-back.
func pendingForgeAPIURL(h *SaaSHive, curAPIURL string) string {
	if h == nil || h.ForgeDelivered || h.RequestedGitHubHost == "" {
		return ""
	}
	target, err := parseForgeTarget(h.RequestedGitHubHost)
	if err != nil {
		return "" // an unparseable stored target is never pushed
	}
	want := target.APIURL
	if want == "" {
		// Public github.com — send the explicit default, not "" (see above).
		want = defaultPublicAPIURL
	}
	if strings.EqualFold(strings.TrimSpace(curAPIURL), want) {
		// The spoke is already there. adoptSpokeForge latches ForgeDelivered
		// from the reported HOST; returning "" here simply stops re-sending a
		// value that has demonstrably landed.
		return ""
	}
	return want
}

// defaultPublicAPIURL is the public github.com REST API base. It duplicates
// config.DefaultGitHubAPIURL deliberately: pkg/hub does not import pkg/config
// for this value anywhere else on the heartbeat path, and the string is a
// stable, public, product-level constant.
const defaultPublicAPIURL = "https://api.github.com"

// adoptSpokeForge closes the forge handshake: once the spoke reports the host
// the operator asked for, delivery is complete and the hub stops pushing.
//
// It is the direct analogue of the ACMMDelivered latch in
// adoptSpokeProjectConfig, and it exists because the hub CANNOT simply trust
// its own meta: the spoke re-reports its host on every beat and the registry
// adopts that report, so the only durable evidence a switch landed is the
// spoke saying so.
//
// spokeHost is HeartbeatPayload.GitHubHost — the spoke's own
// GitHubConfig.HostLabel(). An EMPTY report means the spoke is too old to
// report the field, which is UNKNOWN, not a match: latching on it would end the
// delivery without any evidence it succeeded, leaving the hive on the old forge
// with the hub believing otherwise.
//
// Returns true when it changed and persisted the record.
func (s *HubServer) adoptSpokeForge(hiveID, spokeHost string) bool {
	spokeHost = strings.TrimSpace(spokeHost)
	if spokeHost == "" {
		return false // too old to report — unknown, never a match
	}
	h := loadSaaSHive(hiveID)
	if h == nil || h.RequestedGitHubHost == "" || h.ForgeDelivered {
		return false
	}
	if !sameGitHubHost(spokeHost, h.RequestedGitHubHost) {
		s.logger.Info("forge switch not yet applied on spoke; still delivering",
			"hive_id", hiveID,
			"spoke_reports", spokeHost,
			"requested", h.RequestedGitHubHost)
		return false
	}
	h.ForgeDelivered = true
	// Keep GitHubHost in step with what the spoke now actually runs. Normally
	// they already agree (the switch handler set both), but stating it here
	// means the record cannot be left describing the old forge if this ever runs
	// against a hive whose meta was edited out from under it.
	if !sameGitHubHost(h.GitHubHost, spokeHost) {
		h.GitHubHost = strings.ToLower(spokeHost)
	}
	if err := saveSaaSHive(h); err != nil {
		s.logger.Warn("forge switch: failed to persist delivery",
			"hive_id", hiveID, "error", err)
		return false
	}
	s.logger.Info("forge switch delivered to spoke",
		"hive_id", hiveID, "github_host", spokeHost)
	return true
}
