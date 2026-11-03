package config

import (
	"errors"
	"fmt"
	"log"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// ExperimentLab exposes the experimental Nous/Experiment Lab dashboard
	// section. It stays hidden by default while the v6 workstream remains
	// experimental.
	ExperimentLab bool `yaml:"experiment_lab,omitempty" json:"experiment_lab,omitempty"`
	// StrategyLab is the pre-rename alias of ExperimentLab (RFC #11585 part
	// 2, #11695). It is read ONLY so an operator's explicit
	// dashboard.strategy_lab: true keeps working for one release; applyDefaults
	// folds it into ExperimentLab via migrateDeprecatedStrategyLab and clears
	// it so a re-save writes the documented key.
	//
	// Deprecated: set dashboard.experiment_lab instead.
	StrategyLab bool `yaml:"strategy_lab,omitempty" json:"strategy_lab,omitempty"`
	// IssueBands configures the Repositories card's display-only issue taxonomy.
	// It is intentionally separate from governor/project eligibility labels:
	// queue policy decides what agents may work; these labels only decide which
	// visual band and badges an operator sees on the dashboard.
	IssueBands DashboardIssueBandsConfig `yaml:"issue_bands,omitempty" json:"issue_bands,omitempty"`
	// StageTracker tunes the Hive activity monitor's server-side payload cache
	// (hivecommons/hive#12682).
	StageTracker DashboardStageTrackerConfig `yaml:"stage_tracker,omitempty" json:"stage_tracker,omitempty"`
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
	want := IdentityMatchKey(username)
	for i, entry := range d.AuthorizedUsers {
		name, role := splitAuthorizedEntry(entry)
		if name == "" {
			continue
		}
		if IdentityMatchKey(name) != want {
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

// IdentityMatchKey normalizes an identity string for allowlist comparison:
// lower-cased (GitHub logins are case-insensitive, and the pre-existing
// allowlist match always folded case), with the legacy-GitHub provider prefix
// stripped so "github:alice" and "alice" compare equal. Other providers'
// prefixes ("ibmid:", "google:", ...) are kept — those subjects are only ever
// delivered and presented in their full canonical form.
func IdentityMatchKey(id string) string {
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

// ValidateSnapshotURL validates and normalizes hub.snapshot_url:
// an absolute http(s) URL with an optional path, returning the URL with any
// trailing slash trimmed when the path is otherwise root/empty. Query,
// fragment and credentials are not allowed. Empty is valid and means "unset".
func ValidateSnapshotURL(raw string) (string, error) {
	return validateSnapshotURL(raw, false)
}

// legacySnapshotURLWarned records raw legacy URLs already warned about, so
// repeated Validate calls (dashboard saves, reloads) do not re-log the warning.
var (
	legacySnapshotURLWarnedMu sync.Mutex
	legacySnapshotURLWarned   = map[string]struct{}{}
)

// Loading tolerates legacy query/fragment values accepted by hub registration,
// while dashboard writes continue to enforce the stricter URL shape.
func validateSnapshotURL(raw string, allowLegacyQueryFragment bool) (string, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "", nil
	}
	// Errors echo a redacted form only: query strings and userinfo may carry secrets.
	shown := redactSnapshotURL(v)
	u, err := url.Parse(v)
	if err != nil {
		// *url.Error.Error() embeds the raw URL, so unwrap to the bare cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return "", fmt.Errorf("hub.snapshot_url %q: %w", shown, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("hub.snapshot_url %q: must be an absolute http:// or https:// URL", shown)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("hub.snapshot_url %q: missing host", shown)
	}
	if u.User != nil {
		return "", fmt.Errorf("hub.snapshot_url %q: credentials are not allowed", shown)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		if !allowLegacyQueryFragment {
			return "", fmt.Errorf("hub.snapshot_url %q: query and fragment are not allowed", shown)
		}
		// Do not log the URL: legacy query strings may contain credentials.
		legacySnapshotURLWarnedMu.Lock()
		_, seen := legacySnapshotURLWarned[v]
		legacySnapshotURLWarned[v] = struct{}{}
		legacySnapshotURLWarnedMu.Unlock()
		if !seen {
			log.Printf("[config] warning: hub.snapshot_url contains a legacy query or fragment; remove it before updating this field through the dashboard")
		}
		// Preserve path and suffix semantics rather than normalizing a legacy URL.
		return u.String(), nil
	}
	if u.Path == "" || u.Path == "/" {
		return u.Scheme + "://" + u.Host, nil
	}
	// Rebuild from parsed parts so a bare trailing "#" (which url.Parse
	// reports as an empty fragment) and an uppercase scheme do not survive.
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// redactSnapshotURL returns raw with any query/fragment suffix and any
// userinfo masked, so validation errors can name the value without echoing
// secrets. It works on the raw text so it is safe to use even when parsing
// failed.
func redactSnapshotURL(raw string) string {
	v := raw
	if i := strings.IndexAny(v, "?#"); i >= 0 {
		v = v[:i+1] + "[redacted]"
	}
	start := 0
	if s := strings.Index(v, "://"); s >= 0 {
		start = s + len("://")
	}
	authEnd := len(v)
	if slash := strings.Index(v[start:], "/"); slash >= 0 {
		authEnd = start + slash
	}
	if at := strings.LastIndex(v[start:authEnd], "@"); at >= 0 {
		v = v[:start] + "[redacted]@" + v[start+at+1:]
	}
	return v
}

// DashboardStageTrackerConfig is dashboard.stage_tracker.
type DashboardStageTrackerConfig struct {
	// CacheTTLS is how long, in seconds, GET /api/stage-tracker serves the
	// payload it last built from the status snapshot before rebuilding. The
	// endpoint never calls GitHub either way; the TTL only bounds how often
	// the in-memory snapshot is re-read. 0 means governor.eval_interval_s.
	CacheTTLS int `yaml:"cache_ttl_s,omitempty" json:"cache_ttl_s,omitempty"`
	// WebhookRefresh lets a verified GitHub webhook delivery re-read just the
	// PR or issue it named (one GET) and patch that row of the activity
	// monitor at once, instead of waiting for the next eval cycle to
	// re-enumerate the repository. Default true.
	WebhookRefresh *bool `yaml:"webhook_refresh,omitempty" json:"webhook_refresh,omitempty"`
	// WebhookRefreshDebounceMS is how long, in milliseconds, deliveries for
	// the same item are coalesced before that one read is made, so a check
	// fan-out costs one GET. 0 means the default (2000).
	WebhookRefreshDebounceMS int `yaml:"webhook_refresh_debounce_ms,omitempty" json:"webhook_refresh_debounce_ms,omitempty"`
	// WebhookRefreshReplayWindowS is how long, in seconds, a webhook re-read
	// is remembered and re-applied to every status snapshot the governor
	// publishes afterwards, so a cycle that enumerated the repository before
	// the item closed cannot bring the row back when it publishes minutes
	// later. Must cover the longest eval cycle. 0 means the default (1800).
	WebhookRefreshReplayWindowS int `yaml:"webhook_refresh_replay_window_s,omitempty" json:"webhook_refresh_replay_window_s,omitempty"`
	// ClosedSweepIntervalS is how often, in seconds, the tracker asks GitHub
	// for the repository's items closed since its last look (one list call
	// per repo) and drops any it still shows as open: the backstop for a
	// webhook delivery that never arrived or whose confirming read was
	// refused. Each status publish starts a sweep when one is due. 0
	// means the default (300); a negative value disables it.
	ClosedSweepIntervalS int `yaml:"closed_sweep_interval_s,omitempty" json:"closed_sweep_interval_s,omitempty"`
	// WaitingPushStaleH is how many hours a PR row in the CI / Review
	// column may wait for its author to push before the waiting-on pill's
	// age takes the muted-stale tone (hivecommons/hive#15784). 0 means the
	// default (4); a negative value never marks the wait stale.
	WaitingPushStaleH int `yaml:"waiting_push_stale_h,omitempty" json:"waiting_push_stale_h,omitempty"`
	// ToastInfoS is how many seconds the activity monitor's informational
	// toasts (action results, waiting-on-human nudges, the merge banner)
	// stay up before dismissing themselves (hivecommons/hive#15813).
	// 0 means the default (15); values are clamped to 3..120.
	ToastInfoS int `yaml:"toast_info_s,omitempty" json:"toast_info_s,omitempty"`
	// ToastErrorS is the same for error toasts. 0 means the default (30);
	// values are clamped to 3..120.
	ToastErrorS int `yaml:"toast_error_s,omitempty" json:"toast_error_s,omitempty"`
}

// defaultStageTrackerClosedSweepInterval is the default
// dashboard.stage_tracker.closed_sweep_interval_s.
const defaultStageTrackerClosedSweepInterval = 5 * time.Minute

// StageTrackerClosedSweepInterval is the effective
// dashboard.stage_tracker.closed_sweep_interval_s as a duration; 0 means
// the sweep is disabled.
func (c *Config) StageTrackerClosedSweepInterval() time.Duration {
	if c == nil || c.Dashboard.StageTracker.ClosedSweepIntervalS == 0 {
		return defaultStageTrackerClosedSweepInterval
	}
	if c.Dashboard.StageTracker.ClosedSweepIntervalS < 0 {
		return 0
	}
	return time.Duration(c.Dashboard.StageTracker.ClosedSweepIntervalS) * time.Second
}

// defaultStageTrackerWebhookRefreshDebounce is the default
// dashboard.stage_tracker.webhook_refresh_debounce_ms.
const defaultStageTrackerWebhookRefreshDebounce = 2000 * time.Millisecond

// defaultStageTrackerWebhookRefreshReplayWindow is the default
// dashboard.stage_tracker.webhook_refresh_replay_window_s: the governor's
// capped eval interval, so a snapshot enumerated at the start of the longest
// cycle still has its webhook patches re-applied when it publishes.
const defaultStageTrackerWebhookRefreshReplayWindow = 30 * time.Minute

// StageTrackerWebhookRefreshReplayWindow is the effective
// dashboard.stage_tracker.webhook_refresh_replay_window_s as a duration.
func (c *Config) StageTrackerWebhookRefreshReplayWindow() time.Duration {
	if c != nil && c.Dashboard.StageTracker.WebhookRefreshReplayWindowS > 0 {
		return time.Duration(c.Dashboard.StageTracker.WebhookRefreshReplayWindowS) * time.Second
	}
	return defaultStageTrackerWebhookRefreshReplayWindow
}

// StageTrackerWebhookRefresh is the effective
// dashboard.stage_tracker.webhook_refresh (default true).
func (c *Config) StageTrackerWebhookRefresh() bool {
	if c == nil || c.Dashboard.StageTracker.WebhookRefresh == nil {
		return true
	}
	return *c.Dashboard.StageTracker.WebhookRefresh
}

// StageTrackerWebhookRefreshDebounce is the effective
// dashboard.stage_tracker.webhook_refresh_debounce_ms as a duration.
func (c *Config) StageTrackerWebhookRefreshDebounce() time.Duration {
	if c != nil && c.Dashboard.StageTracker.WebhookRefreshDebounceMS > 0 {
		return time.Duration(c.Dashboard.StageTracker.WebhookRefreshDebounceMS) * time.Millisecond
	}
	return defaultStageTrackerWebhookRefreshDebounce
}

// StageTrackerCacheTTL is the effective dashboard.stage_tracker.cache_ttl_s:
// the configured value, else the governor eval interval, else its default.
func (c *Config) StageTrackerCacheTTL() time.Duration {
	if c != nil && c.Dashboard.StageTracker.CacheTTLS > 0 {
		return time.Duration(c.Dashboard.StageTracker.CacheTTLS) * time.Second
	}
	if c != nil && c.Governor.EvalIntervalS > 0 {
		return time.Duration(c.Governor.EvalIntervalS) * time.Second
	}
	return time.Duration(defaultEvalIntervalS) * time.Second
}

// WebhookWakeDebounce and WebhookWakeMinGap are the effective
// governor.webhook_wake_debounce_s / governor.webhook_wake_min_gap_s, with
// the Load defaults substituted for a zero (or nil-config) value.
func (c *Config) WebhookWakeDebounce() time.Duration {
	if c != nil && c.Governor.WebhookWakeDebounceS > 0 {
		return time.Duration(c.Governor.WebhookWakeDebounceS) * time.Second
	}
	return DefaultWebhookWakeDebounceS * time.Second
}

func (c *Config) WebhookWakeMinGap() time.Duration {
	if c != nil && c.Governor.WebhookWakeMinGapS > 0 {
		return time.Duration(c.Governor.WebhookWakeMinGapS) * time.Second
	}
	return DefaultWebhookWakeMinGapS * time.Second
}

// CIFailureWebhookKick is governor.ci_failure_webhook_kick (default true).
func (c *Config) CIFailureWebhookKick() bool {
	if c == nil || c.Governor.CIFailureWebhookKick == nil {
		return true
	}
	return *c.Governor.CIFailureWebhookKick
}

// CIFailureKickCooldown resolves governor.ci_failure_kick_cooldown_s.
func (c *Config) CIFailureKickCooldown() time.Duration {
	if c != nil && c.Governor.CIFailureKickCooldownS > 0 {
		return time.Duration(c.Governor.CIFailureKickCooldownS) * time.Second
	}
	return DefaultCIFailureKickCooldownS * time.Second
}

// MergeTrainStallWarn resolves governor.merge_train_stall_warn_s.
func (c *Config) MergeTrainStallWarn() time.Duration {
	if c != nil && c.Governor.MergeTrainStallWarnS > 0 {
		return time.Duration(c.Governor.MergeTrainStallWarnS) * time.Second
	}
	return DefaultMergeTrainStallWarnS * time.Second
}

// OwnerLogins returns the AuthorizedUsers entries that hold the owner role
// (the first entry without an explicit role, plus any ":owner" entry), in
// their raw identity form ("alice", "github:alice"); non-GitHub identities
// are included as-is and filtered by the caller.
func (d DashboardConfig) OwnerLogins() []string {
	var out []string
	for i, entry := range d.AuthorizedUsers {
		name, role := splitAuthorizedEntry(entry)
		if name == "" {
			continue
		}
		if role == "" && i == 0 {
			role = RoleOwner
		}
		if role == RoleOwner {
			out = append(out, name)
		}
	}
	return out
}

// Defaults and bounds for dashboard.stage_tracker.toast_info_s and
// toast_error_s. The bounds are shared with the settings modal's number
// fields and the PUT validation so every path agrees on what is legal.
const (
	DefaultStageTrackerToastInfoS  = 15
	DefaultStageTrackerToastErrorS = 30
	MinStageTrackerToastS          = 3
	MaxStageTrackerToastS          = 120
)

// stageTrackerToastSeconds applies the shared default/clamp rule for a
// toast duration knob.
func stageTrackerToastSeconds(configured, def int) int {
	if configured == 0 {
		return def
	}
	return min(max(configured, MinStageTrackerToastS), MaxStageTrackerToastS)
}

// StageTrackerToastInfo is the effective dashboard.stage_tracker.toast_info_s
// as a duration.
func (c *Config) StageTrackerToastInfo() time.Duration {
	if c == nil {
		return DefaultStageTrackerToastInfoS * time.Second
	}
	return time.Duration(stageTrackerToastSeconds(c.Dashboard.StageTracker.ToastInfoS, DefaultStageTrackerToastInfoS)) * time.Second
}

// StageTrackerToastError is the effective
// dashboard.stage_tracker.toast_error_s as a duration.
func (c *Config) StageTrackerToastError() time.Duration {
	if c == nil {
		return DefaultStageTrackerToastErrorS * time.Second
	}
	return time.Duration(stageTrackerToastSeconds(c.Dashboard.StageTracker.ToastErrorS, DefaultStageTrackerToastErrorS)) * time.Second
}

// defaultStageTrackerWaitingPushStale is the default
// dashboard.stage_tracker.waiting_push_stale_h.
const defaultStageTrackerWaitingPushStale = 4 * time.Hour

// StageTrackerWaitingPushStale is the effective
// dashboard.stage_tracker.waiting_push_stale_h as a duration; 0 means a
// wait is never marked stale.
func (c *Config) StageTrackerWaitingPushStale() time.Duration {
	if c == nil || c.Dashboard.StageTracker.WaitingPushStaleH == 0 {
		return defaultStageTrackerWaitingPushStale
	}
	if c.Dashboard.StageTracker.WaitingPushStaleH < 0 {
		return 0
	}
	return time.Duration(c.Dashboard.StageTracker.WaitingPushStaleH) * time.Hour
}
