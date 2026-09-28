package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	dashboardtheme "github.com/hivecommons/hive/pkg/dashboard/theme"
)

type DashboardConfig struct {
	Port               int    `yaml:"port"`
	AuthToken          string `yaml:"auth_token"`
	AgentPollIntervalS int    `yaml:"agent_poll_interval_s"`
	// SnapshotFrameAncestors is the explicit set of HTTPS origins allowed to
	// embed the public, read-only /snapshot document. Empty keeps the historical
	// fail-closed framing policy (X-Frame-Options: DENY plus CSP
	// frame-ancestors 'none'). Wildcards and paths are deliberately rejected so
	// the configured value is a small, auditable origin allowlist.
	SnapshotFrameAncestors []string `yaml:"snapshot_frame_ancestors,omitempty" json:"snapshot_frame_ancestors,omitempty"`
	// AuthorizedUsers is the allowlist of GitHub usernames permitted to log in
	// to a direct-route (non-hub-proxied) spoke via the device flow. The first
	// entry is treated as the owner (read-write); the rest are granted viewers
	// (read-only) unless an explicit "username:role" suffix is given. On the
	// hub-proxied path, nginx injects X-Hive-User/X-Hive-Role and this list is
	// consulted only by the read-only Access tab, NOT for gating logins.
	// Populated on both hub-proxied hosted hives (for the Access view + hub
	// Manage Access) and standalone direct-route spokes (for device-flow authz).
	AuthorizedUsers []string `yaml:"authorized_users"`
	// AuthorizedUserNames is an OPTIONAL, purely cosmetic map from an
	// AuthorizedUsers entry's raw identity key (the same string before any
	// ":role" suffix, e.g. "ibmid:5500087VJB" or a plain GitHub login) to a
	// human-readable display name. It rides alongside AuthorizedUsers — never
	// inside it — so the identity key used for allowlist matching and grants
	// (AuthorizedRole, IsDirectRouteAuthzEnabled) is completely unaffected by
	// this field's presence, absence, or content. Delivered by the hub in the
	// same heartbeat beat as AuthorizedUsers (mirrors it 1:1) so the read-only
	// Access tab can render "Jane Doe" instead of a raw IBMid/Google/Microsoft
	// subject. A key with no entry here (or an empty value) simply has no known
	// human name yet — the UI falls back to the raw key. omitempty/nil-safe:
	// existing configs and hubs that never send this round-trip unchanged.
	AuthorizedUserNames map[string]string `yaml:"authorized_user_names,omitempty"`
	// HubProxied is true when this hive sits behind the hub's nginx auth-proxy,
	// which authenticates every request and injects trusted X-Hive-User/X-Hive-Role
	// headers (hub-reachable-cluster hosted hives). When true the hive TRUSTS those headers and
	// keeps the shared-token path enabled even if AuthorizedUsers is non-empty —
	// nginx is the gate, and the allowlist is informational (Access tab) only.
	//
	// When false (the default) a non-empty AuthorizedUsers list means this is a
	// STANDALONE direct-route spoke with no nginx in front (the heartbeat-only cluster), so it must
	// strip client-supplied identity headers and enforce per-user device-flow
	// authz itself. Decoupling these two meanings fixes hub-proxied hives being
	// wrongly forced into direct-route mode (which broke their dashboard link and
	// snapshot preview) the moment they were granted an authorized_users list.
	HubProxied bool `yaml:"hub_proxied"`
	// PublicURL is the externally reachable origin of THIS dashboard
	// (scheme + host[:port], no path), used to build OAuth redirect URIs —
	// the Linear agent install and the OpenRouter funding flow — when it
	// differs from the host the request arrived on. Precedence when a
	// callback URL is built: dashboard.public_url, then hub.dashboard_url
	// (kept for hub-hosted spokes, whose hub already knows their public
	// name), then the X-Forwarded-Proto/X-Forwarded-Host/Host of the request.
	// Set it on a hub-less hive whose dashboard is private but whose OAuth
	// callback path is published on a different public hostname, or behind
	// an ingress that rewrites the Host header on the way in (Traefik with a
	// fixed upstream Host, a Cloudflare Tunnel "HTTP Host Header"): with
	// nothing configured, the install leg and the callback leg can derive
	// different origins and the provider rejects the code exchange with
	// "redirect_uri is invalid". Validated at load time by
	// ValidateDashboardPublicURL.
	PublicURL string `yaml:"public_url,omitempty" json:"public_url,omitempty"`
	// Theme names the built-in dashboard theme id ("hive" by default) or
	// "custom" when theme_overrides supplies the complete operator palette.
	Theme string `yaml:"theme,omitempty" json:"theme,omitempty"`
	// ThemeOverrides are layered on the selected built-in theme and are mutable
	// through the owner-only dashboard appearance API.
	ThemeOverrides dashboardtheme.Overrides `yaml:"theme_overrides,omitempty" json:"theme_overrides,omitempty"`
	// IssueBands configures the Repositories card's display-only issue taxonomy.
	// It is intentionally separate from governor/project eligibility labels:
	// queue policy decides what agents may work; these labels only decide which
	// visual band and badges an operator sees on the dashboard.
	IssueBands DashboardIssueBandsConfig `yaml:"issue_bands,omitempty" json:"issue_bands,omitempty"`
}

type DashboardIssueBandsConfig struct {
	WaitingLabels []string `yaml:"waiting_labels,omitempty" json:"waiting_labels,omitempty"`
	DoneLabels    []string `yaml:"done_labels,omitempty" json:"done_labels,omitempty"`
	StaleDays     int      `yaml:"stale_days,omitempty" json:"stale_days,omitempty"`
}

// ValidateDashboardPublicURL validates and normalizes dashboard.public_url:
// an absolute http(s) URL naming an origin only — no path, query, fragment or
// credentials — returned with any trailing slash removed. Empty is valid and
// means "unset".
func ValidateDashboardPublicURL(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("dashboard.public_url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("dashboard.public_url %q: must be an absolute http:// or https:// URL", raw)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("dashboard.public_url %q: missing host", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("dashboard.public_url %q: credentials are not allowed", raw)
	}
	if strings.Trim(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", fmt.Errorf("dashboard.public_url %q: must be an origin only (scheme://host[:port]) with no path, query or fragment", raw)
	}
	return u.Scheme + "://" + u.Host, nil
}

var snapshotFrameAncestorHostPattern = regexp.MustCompile(`(?i)^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\.?$`)

// ValidateSnapshotFrameAncestors validates and normalizes the /snapshot framing
// allowlist. Only exact HTTPS origins are accepted: scheme + host with optional
// port, and no path, query, fragment, credentials, or wildcard hostnames.
func ValidateSnapshotFrameAncestors(origins []string) ([]string, error) {
	normalized := make([]string, 0, len(origins))
	seen := map[string]struct{}{}
	for _, raw := range origins {
		origin := strings.TrimSpace(raw)
		if origin == "" {
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("snapshot_frame_ancestors entry %q must be an https origin", raw)
		}
		if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.Contains(u.Hostname(), "*") {
			return nil, fmt.Errorf("snapshot_frame_ancestors entry %q must be an exact https origin with no path, credentials, query, fragment, or wildcard", raw)
		}
		if !validSnapshotFrameAncestorHost(u.Hostname()) {
			return nil, fmt.Errorf("snapshot_frame_ancestors entry %q has an invalid host", raw)
		}
		if port := u.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("snapshot_frame_ancestors entry %q has an invalid port", raw)
			}
		}
		origin = "https://" + u.Host
		if _, ok := seen[origin]; ok {
			continue
		}
		seen[origin] = struct{}{}
		normalized = append(normalized, origin)
	}
	return normalized, nil
}

func validSnapshotFrameAncestorHost(host string) bool {
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	return snapshotFrameAncestorHostPattern.MatchString(host)
}

// SnapshotFrameAncestorsCSP returns the CSP source list for /snapshot framing.
func (d DashboardConfig) SnapshotFrameAncestorsCSP() string {
	if len(d.SnapshotFrameAncestors) == 0 {
		return "'none'"
	}
	return strings.Join(d.SnapshotFrameAncestors, " ")
}

// Role strings used for direct-route spoke authorization. These mirror the
// roles the hub injects via X-Hive-Role on the proxied path so the read-only
// gating in the dashboard behaves identically on both paths.
const (
	RoleOwner = "owner"
	RoleRead  = "read"
	// RoleReadWrite is granted by the hub's Manage Access screen (see the role
	// validation in hub.handleAccessAdd, which accepts read / read-write /
	// owner). It was missing here, so splitAuthorizedEntry treated
	// "user:read-write" as an unknown suffix and folded the whole string into
	// the username — the allowlist then contained a user literally named
	// "cbrooker27:read-write", no lookup could ever match, and every login was
	// rejected as unauthorized despite the grant being present and correct.
	RoleReadWrite = "read-write"
	// RoleMerger can do everything read-write can, plus approve/queue other
	// people's PRs for Hive's auto-merge-on-green sweep. Both the dashboard
	// queue endpoint and the sweep enforce the "never your own PR" rule
	// server-side.
	RoleMerger = "merger"
)

// DefaultAutoMergeLabel is the label applied when a hive does not configure
// `governor.labels.automerge`. `lgtm` is the long-standing Prow/Kubernetes
// convention for "a second person signed off on this", which is exactly the
// decision the merger tier records, so a managed repository usually already
// has it and already understands it.
const DefaultAutoMergeLabel = "lgtm"

var roleRanks = map[string]int{
	RoleRead:      1,
	RoleReadWrite: 2,
	RoleMerger:    3,
	RoleOwner:     4,
}

// ValidRole reports whether role is one of the hive access tiers.
func ValidRole(role string) bool {
	_, ok := roleRanks[strings.ToLower(strings.TrimSpace(role))]
	return ok
}

// RoleAtLeast reports whether role includes all capabilities of tier.
func RoleAtLeast(role, tier string) bool {
	roleRank, okRole := roleRanks[strings.ToLower(strings.TrimSpace(role))]
	tierRank, okTier := roleRanks[strings.ToLower(strings.TrimSpace(tier))]
	return okRole && okTier && roleRank >= tierRank
}

// AuthorizedRole resolves a username against the spoke's authorized-users
// allowlist and returns the user's role and whether they are authorized.
//
// Each entry is either "username" or "username:role" (role = "owner" or "read").
// An entry without an explicit role defaults to "owner" for the first entry
// (the hive owner) and "read" for the rest (granted viewers). Matching is
// case-insensitive because GitHub usernames are case-insensitive, and tolerant
// of the hub's canonical identity form: a bare login and its "github:<login>"
// wire form denote the SAME user (the hub's legacy shim), so an allowlist entry
// in either form matches a session username in either form. Without this, a
// hub-delivered "github:alice:owner" entry silently failed to authorize the
// device-flow login "alice" — one more variant of the recurring
// "granted owner still gets 'owner access required'" class.
func (d DashboardConfig) AuthorizedRole(username string) (string, bool) {
	if username == "" {
		return "", false
	}
	want := identityMatchKey(username)
	for i, entry := range d.AuthorizedUsers {
		name, role := splitAuthorizedEntry(entry)
		if name == "" {
			continue
		}
		if identityMatchKey(name) != want {
			continue
		}
		if role == "" {
			if i == 0 {
				role = RoleOwner
			} else {
				role = RoleRead
			}
		}
		return role, true
	}
	return "", false
}

// identityMatchKey normalizes an identity string for allowlist comparison:
// lower-cased (GitHub logins are case-insensitive, and the pre-existing
// allowlist match always folded case), with the legacy-GitHub provider prefix
// stripped so "github:alice" and "alice" compare equal. Other providers'
// prefixes ("ibmid:", "google:", ...) are kept — those subjects are only ever
// delivered and presented in their full canonical form.
func identityMatchKey(id string) string {
	key := strings.ToLower(strings.TrimSpace(id))
	return strings.TrimPrefix(key, "github:")
}

// IsDirectRouteAuthzEnabled reports whether this hive must enforce per-user
// authorization on device-flow logins ITSELF because there is no hub nginx in
// front of it. True only for a STANDALONE spoke: it has an authorized-users
// allowlist AND is not hub-proxied.
//
// A hub-proxied hive (HubProxied=true) can also carry an allowlist — for the
// read-only Access tab and the hub's Manage Access screen — but nginx is the
// gate there, so it must NOT flip into direct-route mode (which would strip the
// trusted X-Hive-User/X-Hive-Role headers nginx injects and disable the shared
// token, breaking the dashboard link and the snapshot preview).
func (d DashboardConfig) IsDirectRouteAuthzEnabled() bool {
	return !d.HubProxied && len(d.AuthorizedUsers) > 0
}

// splitAuthorizedEntry parses a "username" or "username:role" allowlist entry.
func splitAuthorizedEntry(entry string) (name, role string) {
	entry = strings.TrimSpace(entry)
	if idx := strings.LastIndex(entry, ":"); idx >= 0 {
		name = strings.TrimSpace(entry[:idx])
		role = strings.ToLower(strings.TrimSpace(entry[idx+1:]))
		if !ValidRole(role) {
			// Unknown role suffix — treat the whole thing as a bare username so
			// a stray colon can never silently downgrade or escalate access.
			return strings.TrimSpace(entry), ""
		}
		return name, role
	}
	return entry, ""
}
