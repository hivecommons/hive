package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
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
	// ForgeGitLab is a GitLab instance (gitlab.com or self-managed). Unlike the
	// GitHub kinds it does NOT use the GitHub App identity machinery. It is
	// DISPLAY-ONLY today: accepting this kind records the forge family and host
	// on the hive and changes what the dashboard shows. No spoke executes
	// against GitLab. The value matches pkg/forge.KindGitLab so a future wiring
	// need not translate, but pkg/forge is not reached from here.
	ForgeGitLab ForgeKind = "gitlab"
	// ForgeGitea is a Gitea/Forgejo instance (self-managed or Codeberg). Same
	// story as GitLab: display-only, matching pkg/forge.KindGitea by value only.
	ForgeGitea ForgeKind = "gitea"
)

// WHY "DISPLAY-ONLY" IS THE ACCURATE WORD FOR GITLAB/GITEA
//
// src/pkg/forge is a complete, tested adapter package with THREE implementations
// (GitHub, GitLab, Gitea) and ZERO non-test importers:
//
//	$ grep -rn "kubestellar/hive/pkg/forge" --include="*.go" src/ \
//	    | grep -v _test | grep -v "^src/pkg/forge/"
//	(no output)
//
// Nothing in the running system constructs an adapter. Agents reach their forge
// through the gh CLI wrapper, which is GitHub-only. So a hive switched to
// ForgeGitLab gets a recorded kind/host and a different dashboard tile, and its
// agents do not run.
//
// The hub-side SaaSHive.Forge field this endpoint writes is also NOT delivered
// to spokes: no heartbeat struct carries a forge field. Wiring the path would
// mean adding one to HeartbeatResponse, adding a spoke-side consumer that
// constructs the adapter, and extending the interface (below).
//
// THE INTERFACE CEILING, worth knowing before anyone plans that work: the
// pkg/forge.Forge interface exposes GetRepo, ListOpenIssues,
// ListOpenChangeRequests, CreateIssueComment, AddLabels, RemoveLabel and
// SetHold — but NO CreateIssue and NO CreatePR. Merge is a separate, optional
// Merger interface that no adapter implements. Even fully delivered and wired,
// these adapters could comment, label and hold, but could not open the issue or
// change request an agent's work has to land in.
//
// The verified support matrix lives in src/docs/forge-app-setup.md; keep this
// comment and that document in agreement.

// FORGE API-PATH CONTRACT, hub side. The REST API base paths for the non-GitHub
// forges are appended by the pkg/forge adapters themselves (which, per the note
// above, no running code path constructs today)
// (gitLabAPIPath="/api/v4", giteaAPIPath="/api/v1"), so for those kinds a
// ForgeTarget carries the BARE instance URL in BaseURL and leaves APIURL empty —
// the opposite of the GHE case below, where the hub pre-appends /api/v3. The hub
// must not duplicate the GitLab/Gitea suffixes: a second copy here is how the two
// halves would get a chance to disagree.

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
	return parseForgeTargetWithKind("", raw)
}

// parseForgeTargetWithKind is parseForgeTarget with an explicit forge kind.
// An empty kind preserves the original behavior (public github.com, else GHE
// inferred from the host). "gitlab"/"gitea" produce non-GitHub targets whose
// BaseURL is the bare instance root and whose APIURL is left empty — the
// spoke-side pkg/forge adapter appends the /api/v4 or /api/v1 suffix itself
// (see the forge API-path contract note above). "github"/
// "github-enterprise" fall through to the host-based GitHub path.
func parseForgeTargetWithKind(kind, raw string) (ForgeTarget, error) {
	switch ForgeKind(strings.ToLower(strings.TrimSpace(kind))) {
	case ForgeGitLab, ForgeGitea:
		fk := ForgeKind(strings.ToLower(strings.TrimSpace(kind)))
		host := normalizeForgeHostInput(raw)
		if host == "" {
			return ForgeTarget{}, fmt.Errorf("forge host is required for %s (e.g. %q)", fk, defaultHostForKind(fk))
		}
		if len(host) > maxForgeHostLen {
			return ForgeTarget{}, fmt.Errorf("forge host is too long (max %d characters)", maxForgeHostLen)
		}
		if !isValidForgeHostLabel(host) {
			return ForgeTarget{}, fmt.Errorf("%q is not a valid %s host", raw, fk)
		}
		// BaseURL only; the spoke adapter appends the REST API path. APIURL empty.
		return ForgeTarget{Kind: fk, Host: host, BaseURL: "https://" + host}, nil
	}
	// kind is empty or a GitHub kind → the original host-based GitHub resolution.
	return parseGitHubForgeTarget(raw)
}

// normalizeForgeHostInput strips scheme, path, port, and userinfo from an
// operator-supplied forge host/URL, returning the bare lower-cased host. Shared
// by the GitHub and non-GitHub parse paths.
func normalizeForgeHostInput(raw string) string {
	host := strings.ToLower(strings.TrimSpace(raw))
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.SplitN(host, "/", 2)[0]
	host = strings.SplitN(host, ":", 2)[0]
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	return host
}

// defaultTokenEnvForKind names the env var the spoke reads its access token
// from for a non-GitHub forge, used in the operator-facing switch note. These
// match pkg/config's DefaultGitLabTokenEnv / the Gitea default.
func defaultTokenEnvForKind(k ForgeKind) string {
	switch k {
	case ForgeGitLab:
		return "GITLAB_TOKEN"
	case ForgeGitea:
		return "GITEA_TOKEN"
	default:
		return "GH_TOKEN"
	}
}

// defaultHostForKind returns the public default host for a non-GitHub forge kind,
// used only in error messages.
func defaultHostForKind(k ForgeKind) string {
	switch k {
	case ForgeGitLab:
		return "gitlab.com"
	case ForgeGitea:
		return "codeberg.org"
	default:
		return publicForgeHost
	}
}

// parseGitHubForgeTarget is the original host-based GitHub/GHE resolution.
func parseGitHubForgeTarget(raw string) (ForgeTarget, error) {
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
	// Kind optionally names the forge family: "github" / "github-enterprise"
	// (the default when omitted, inferred from Host), or "gitlab" / "gitea".
	// GitLab and Gitea MUST be selected explicitly here because a host alone
	// cannot distinguish them from a GitHub Enterprise instance (they differ
	// only in API path shape). Empty preserves the historical GitHub behavior.
	Kind string `json:"kind,omitempty"`
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
	Status    string `json:"status"`
	ID        string `json:"id"`
	FromHost  string `json:"from_host"`
	ToHost    string `json:"to_host"`
	ForgeKind string `json:"forge_kind"`
	APIURL    string `json:"api_url,omitempty"`
	AppID     string `json:"app_id,omitempty"`
	// AppSlug is the App slug now in effect on the target forge. Reported so an
	// operator can see the install link will resolve, rather than discovering a
	// 404 through the user.
	AppSlug         string `json:"app_slug,omitempty"`
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
	if isSameOriginAsHub(origin) {
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
	if !canonicalEqual(h.Owner, username) && !isHubAdmin(username) {
		http.Error(w, `{"error":"only the owner can switch forges"}`, http.StatusForbidden)
		return
	}

	var body SwitchForgeRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	target, err := parseForgeTargetWithKind(body.Kind, body.Host)
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

	// NON-GITHUB FORGES (GitLab / Gitea): these authenticate with a
	// PRIVATE-TOKEN via pkg/forge, not a GitHub App, so the App-identity
	// resolution, PendingAppConfig, and IdentitySet machinery below do not
	// apply. Record the forge family + host durably and re-arm the same
	// requested/delivered handshake used for GitHub, then return. GitHub App
	// fields are cleared so a stale app_id/installation from a prior GitHub
	// forge can't be sent to a spoke that no longer talks to GitHub.
	if target.Kind == ForgeGitLab || target.Kind == ForgeGitea {
		h.Forge = string(target.Kind)
		h.GitHubHost = target.Host
		h.GitHubBaseURL = target.BaseURL
		h.GitHubAPIURL = "" // adapter appends /api/v4 or /api/v1 to BaseURL
		h.RequestedGitHubHost = target.Host
		h.ForgeDelivered = false
		h.PendingAppConfig = nil // no GitHub App on GitLab/Gitea
		if err := saveSaaSHive(h); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to persist forge switch: "+err.Error())
			return
		}
		s.logger.Info("forge switch queued (non-GitHub)",
			"hive_id", id, "kind", target.Kind, "host", target.Host)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":   true,
			"kind": string(target.Kind),
			"host": target.Host,
			"note": fmt.Sprintf("hive %s will switch to %s (%s) on its next heartbeat. Ensure the spoke has a %s access token in its environment.", id, target.Host, target.Kind, defaultTokenEnvForKind(target.Kind)),
		})
		return
	}

	// Resolve the App identity for the TARGET forge from CLUSTER CONFIG. No App
	// ID or slug is hardcoded anywhere in Go: the hub-reachable cluster names its github.com App
	// and the heartbeat-only cluster names its github.ibm.com App, both in clusters.json, exactly as
	// resolveProvisionAppID already reads them.
	cluster := s.clusterForHive(h)
	identity, missing := s.forgeIdentityForTarget(cluster, target)

	// REFUSE RATHER THAN HALF-APPLY. Verified the hard way: a hive was moved to
	// github.ibm.com by editing github_host alone. The host changed and nothing
	// else did — it kept the github.com app_id, an empty app_slug, and a
	// installation_id belonging to the old forge. The owner then clicked
	// "Install GitHub App" and got a GitHub 404, because an empty app_slug falls
	// back to config.DefaultGitHubAppSlug ("kubestellar-hive"), which is the
	// github.com App's slug and names nothing on GHE. A wrong install link is
	// worse than no link: it tells the user the product is broken.
	//
	// A forge switch is therefore an ATOMIC SWAP OF AN IDENTITY SET, not a
	// single field. If the target forge's identity is not fully known for this
	// cluster, nothing is written at all and the response names exactly what is
	// missing so an operator can populate clusters.json and re-run.
	if len(missing) > 0 {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": fmt.Sprintf("refusing to switch %s to %s: the target forge identity is incomplete for cluster %q, and a partial switch produces a broken hive (a wrong App install link 404s)",
				id, target.Host, clusterIDForHive(h)),
			"missing": missing,
			"remedy":  forgeIdentityRemedy,
		})
		s.logger.Warn("forge switch refused: incomplete target identity",
			"hive_id", id, "to_host", target.Host,
			"cluster", clusterIDForHive(h), "missing", strings.Join(missing, ","))
		return
	}
	appID := identity.AppID

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

	// Refuse to arm an identity whose components disagree about their forge.
	// This handler already resolves the whole set up front and 409s rather than
	// half-applying; this is the same guarantee expressed as a check, so it also
	// covers a cluster config that names, say, a GHE slug with a public api_url.
	wantIdentity := IdentitySet{
		AppID:          appID,
		AppSlug:        identity.AppSlug,
		InstallationID: installID,
		APIURL:         target.APIURL,
		BaseURL:        target.BaseURL,
	}
	if reason := ValidateIdentityPush(wantIdentity); reason != "" {
		s.logger.Warn("forge switch: refusing inconsistent identity",
			"hive_id", id, "reason", reason)
		writeJSONError(w, http.StatusConflict, reason)
		return
	}

	// Re-arm delivery. This is the #2354 handshake: the requested host is
	// authoritative until the spoke reports it back, at which point the push
	// stops permanently and the spoke owns the value again.
	h.RequestedGitHubHost = target.Host
	h.ForgeDelivered = false

	// Queue the identity DURABLY on the hive record. The in-memory one-shot map
	// this replaced was dropped on the first beat that carried it and lost
	// entirely on a hub restart, so a missed beat meant the change silently
	// never happened. Persisting it means the push repeats until the spoke
	// reports the identity back — which the app_slug/installation_id read-back
	// fields make verifiable for the first time.
	h.PendingAppConfig = &PendingAppIdentity{
		AppID: appID,
		// Only a supplied ID is sent. Zero means "leave it alone" on the
		// wire; the operator note below tells the operator it must be
		// installed or supplied, so the failure is never silent.
		InstallationID: installID,
		AppSlug:        identity.AppSlug,
		APIURL:         target.APIURL,
		BaseURL:        target.BaseURL,
		ArmedAt:        time.Now().UTC().Format(time.RFC3339),
		Reason:         "forge switch to " + target.Host,
		// PrivateKey is deliberately absent, and this record never stores key
		// material. The per-cluster key reconcile (cluster_app_key.go) delivers
		// keys on its own schedule and gates every push on HasKey();
		// duplicating it here would risk blanking a working key.
	}

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
	//
	// The SLUG travels with the app_id, and that is the whole point of the
	// precondition above: identity.AppSlug is non-empty by construction here, so
	// the spoke can never be left falling back to the github.com default slug on
	// a GHE host — the state that produced the live install-link 404.
	deliveryPending := true

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
		"app_slug", identity.AppSlug,
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
		AppSlug:         identity.AppSlug,
		InstallationID:  strings.TrimSpace(body.InstallationID),
		AppInstallNote:  note,
		DeliveryPending: deliveryPending,
	}
	if appID > 0 {
		resp.AppID = strconv.FormatInt(appID, 10)
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// forgeIdentity is the COMPLETE App identity for one forge. A forge switch
// swaps all of it or none of it.
type forgeIdentity struct {
	// AppID is the numeric GitHub App ID registered on the target forge.
	AppID int64
	// AppSlug is that App's URL slug ON THAT FORGE. It is part of the identity,
	// not a cosmetic extra: it is what the "Install GitHub App" link is built
	// from, and an empty value silently falls back to the public github.com slug
	// (config.ResolvedAppSlug -> DefaultGitHubAppSlug), which on a GHE host
	// names an App that does not exist and 404s.
	AppSlug string
}

// forgeIdentityRemedy is the operator-facing instruction attached to a refused
// switch. It names the file and the fields, because the fix is a config edit and
// nothing about the request itself is wrong.
const forgeIdentityRemedy = "populate the target cluster's entry in /data/saas/clusters.json (github_app_id and github_app_slug for that forge), then re-run this switch — it is idempotent"

// forgeIdentityForTarget resolves the FULL App identity for the target forge
// from CLUSTER CONFIG ONLY, and reports everything that is missing.
//
// PER-HIVE ELECTION. A cluster's forge is a DEFAULT, not a pin: a cluster may
// carry identity for BOTH forges (see ClusterConfig.Forges) and a hive elects
// one. So this refuses only a target the cluster has no identity FOR, rather
// than every target differing from the cluster's own host. That is the change
// that unblocks a public election on the GHE-default heartbeat-only cluster, which
// previously 409'd with "the target forge identity is incomplete" — leaving
// seven public hives that had already elected github.com unrepairable.
//
// # WHY MISSING FIELDS ARE AN ERROR AND NOT A DEFAULT
//
// Every field here has a plausible-looking fallback, and every one of those
// fallbacks is wrong on the target forge:
//   - app_id: falling back to the cluster's other App authenticates as an App
//     that does not exist on this forge.
//   - app_slug: falling back to DefaultGitHubAppSlug builds an install link at
//     <ghe-host>/github-apps/kubestellar-hive/... — the exact live 404.
//
// So this returns the missing field NAMES and the caller refuses the switch.
// Nothing is hardcoded: both values come from ClusterConfig, the same
// non-secret fields resolveProvisionAppID and the provisioning template read.
//
// The refusal itself is preserved exactly. An incomplete identity still returns
// missing field names and still writes nothing: refusing rather than
// half-applying is the property that stopped the 2026-07-31 incident from
// getting worse, and it survives this change untouched.
func (s *HubServer) forgeIdentityForTarget(cluster *ClusterConfig, target ForgeTarget) (forgeIdentity, []string) {
	if cluster == nil {
		return forgeIdentity{}, []string{"cluster config (this hive's cluster is unknown to the hub)"}
	}
	targetHost := forgeHostKey(target.Host)
	forges := forgesForCluster(cluster)

	// Look the target forge up directly. sameGitHubHost keeps the comparison
	// tolerant of spelling (bare host vs URL) the way the old cluster-host
	// comparison was.
	var identity ClusterForgeIdentity
	found := false
	for host, id := range forges {
		if sameGitHubHost(host, targetHost) {
			identity, found = id, true
			break
		}
	}
	// NOTE ON THE ONE-RESOLVER RULE (#2383). Every other identity decision
	// routes through ResolveHiveIdentity, and this one deliberately does not.
	//
	// ResolveHiveIdentity answers "what identity does this hive have NOW",
	// resolving a hive's existing election against its cluster's default. This
	// function answers a different question: "can the cluster serve the forge
	// this hive is trying to elect, and with which App?" — asked BEFORE any
	// election exists. Synthesising a SaaSHive from the target to feed the
	// resolver would make it answer its own input, which is why the lookup over
	// forgesForCluster above is the direct expression of the question.
	//
	// The resolver still owns the outcome: the elected host is written to the
	// hive record and every later read — heartbeat answer, key lookup,
	// provisioning — goes through ResolveHiveIdentity as normal.
	if !found {
		// The cluster names no App on the requested forge. This is the same
		// class of refusal as before, but the remedy is now additive: name the
		// forge in the cluster's `forges` map rather than move the whole
		// cluster. Listing what the cluster CAN serve makes that actionable.
		served := make([]string, 0, len(forges))
		for host := range forges {
			served = append(served, host)
		}
		sort.Strings(served)
		if len(served) == 0 {
			return forgeIdentity{}, []string{
				fmt.Sprintf("a GitHub App registered on %s (cluster %q names no GitHub App at all)", target.Host, cluster.ID),
			}
		}
		return forgeIdentity{}, []string{
			fmt.Sprintf("a GitHub App registered on %s (cluster %q serves %s)",
				target.Host, cluster.ID, strings.Join(served, ", ")),
		}
	}

	// An identity that exists must still be COMPLETE. Both halves are required
	// on both forges: the public default slug happens to be correct on
	// github.com, but accepting an unset value would mean the atomicity
	// guarantee holds on one forge and not the other, and the operator would
	// get no signal that the cluster entry is half-filled.
	//
	// Name the field the operator must actually edit. A legacy entry carries the
	// identity in the flat github_app_id/github_app_slug keys; an entry using
	// the per-forge map carries it under forges.<host>. Pointing at the wrong
	// one sends the operator to a key that does not exist in their file.
	idField, slugField := "github_app_id", "github_app_slug"
	if _, viaMap := cluster.Forges[targetHost]; viaMap || clusterDefaultForge(cluster) != targetHost {
		idField = fmt.Sprintf("forges.%s.app_id", targetHost)
		slugField = fmt.Sprintf("forges.%s.app_slug", targetHost)
	}
	var missing []string
	if identity.AppID == 0 {
		missing = append(missing, fmt.Sprintf("%s for cluster %q", idField, cluster.ID))
	}
	if strings.TrimSpace(identity.AppSlug) == "" {
		missing = append(missing, fmt.Sprintf("%s for cluster %q (an empty slug falls back to %q, which names no App on %s)",
			slugField, cluster.ID, defaultPublicAppSlug, target.Host))
	}
	if len(missing) > 0 {
		return forgeIdentity{}, missing
	}
	return forgeIdentity{
		AppID:   identity.AppID,
		AppSlug: strings.TrimSpace(identity.AppSlug),
	}, nil
}

// defaultPublicAppSlug mirrors config.DefaultGitHubAppSlug. It appears here
// only inside an operator-facing error message explaining what an empty slug
// would fall back to — it is never used as a value.
const defaultPublicAppSlug = "kubestellar-hive"

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
