// Per-user hive listing and lifecycle HTTP handlers: my-hives,
// create/open/delete/rename, status, visibility, hive config proxy,
// and dibs repo listing.
package hub

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

type MyHiveEntry struct {
	RegistryEntry
	Role        string `json:"role"`
	ProvError   string `json:"provError,omitempty"`
	ProvStatus  string `json:"provStatus,omitempty"`
	AutoUpgrade bool   `json:"autoUpgrade"`
	// OwnerName is the resolved human display label for an opaque OIDC Owner
	// identity ("ibmid:5500…" → "Jane Doe"), stamped at serve time from the
	// stored user record. Empty when Owner is already the best label (GitHub
	// logins). Purely cosmetic: grouping labels and tooltips show it; every
	// key, filter and authorization check stays on the raw Owner.
	OwnerName string `json:"ownerName,omitempty"`
	// TrackedChannel is the release channel this hive's image is pinned to
	// ("stable", "candidate", "edge"), or "" for a plain-branch hive. Overlaid
	// at read time from the hub-owned SaaSHive record — deliberately NOT from
	// the registry, whose GitBranch the spoke rewrites every beat with the
	// image's baked-in branch (a channel retag of a v4 build heartbeats "v4").
	// When set, the dashboard's version pill and picker treat it as the
	// current selection (rendered via versionLabel as "stable (v4)") while
	// gitBranch keeps driving everything about the code actually running.
	TrackedChannel string `json:"trackedChannel,omitempty"`
	// AutoUpgradeMode is always sent NORMALIZED (never empty when autoUpgrade is
	// on) so the dashboard can render the effective mode without re-deriving the
	// legacy empty-means-instant rule in JavaScript.
	AutoUpgradeMode     string                 `json:"autoUpgradeMode,omitempty"`
	PendingRequestCount int                    `json:"pendingRequestCount,omitempty"`
	PendingRequests     []PendingAccessRequest `json:"pending_requests,omitempty"`

	// Access lists who can sign in to this hive and with what role, so My Hives
	// can show it on hover without a per-row API call. Same data as
	// GET /hives/{id}/access, and populated under the same authorization rule:
	// only for rows the caller owns (or admin). Nil on rows the caller merely
	// has delegated access to — knowing who else shares a hive is the owner's
	// information, not every reader's.
	Access []HiveAccessEntry `json:"access,omitempty"`

	// Assigning is true while a freshly-assigned placeholder's spoke has not yet
	// reported the real project via heartbeat: the meta.json already records the
	// real project (org/repos/ACMM set, status no longer "available") but the live
	// registry entry still shows the placeholder identity. It flips false — and the
	// dashboard spinner clears — once the spoke reconciles and the registry reports
	// the real project. AssigningTo is the target org so the row can say "Assigning
	// to <org>". This is exactly the condition projectConfigForHiveID keeps sending
	// its reconcile under.
	Assigning   bool   `json:"assigning,omitempty"`
	AssigningTo string `json:"assigningTo,omitempty"`

	// AssignedUnclaimed marks a placeholder wedged at Status=statusAssigned &&
	// !ClaimDelivered: a claim was stamped but the spoke never reported the
	// project back. Unlike Assigning — which is true only while a REACHABLE spoke
	// is actively reporting a DIFFERENT project — this is true even when the spoke
	// is offline or silent (the frozen-image / heartbeat-only case), which is
	// exactly the dead-end the Reset assignment action exists for. It gates that
	// admin-only action in the row menu.
	AssignedUnclaimed bool `json:"assignedUnclaimed,omitempty"`

	// Unassigned marks an unclaimed pool placeholder (statusAvailable or the
	// legacy available-* org fallback). Fleet hides these idle capacity rows by
	// default so real tenant hives drive the attention count.
	Unassigned bool `json:"unassigned,omitempty"`

	// AssignedAt is the RFC3339 timestamp the placeholder was last assigned/claimed
	// (SaaSHive.AssignedAt). It rides the row payload ONLY for a hive that is still
	// AssignedUnclaimed, so the dashboard can render a live "claim pending" counter
	// measuring how long the slot has been wedged. It is the same clock the
	// assign-stuck self-heal sweep measures against, so the row's amber "stuck"
	// threshold cannot drift from the reset timeout. Empty for every other row.
	AssignedAt string `json:"assignedAt,omitempty"`

	// AssignStuckSeconds is assignStuckResetTimeout expressed in whole seconds, so
	// the dashboard's "stuck / about to auto-reset" tint reuses the SAME threshold
	// the self-heal sweep enforces rather than hardcoding a duplicate in JS. Sent
	// only alongside AssignedAt (i.e. for an assigned-but-unclaimed row).
	AssignStuckSeconds int `json:"assignStuckSeconds,omitempty"`

	// Drift holds config-drift signals computed server-side against the fleet
	// norm (see drift.go). It rides this payload rather than a per-row API call
	// so the My Hives table can render the badge and the fleet-exceptions
	// summary from data it already has.
	Drift DriftReport `json:"drift"`

	// RecentEvents carries the newest few timeline events so the My Hives
	// status hover can show recent activity WITHOUT a per-row fetch.
	//
	// Embedding rather than lazy-fetching is deliberate. The hover is a
	// transient, high-frequency interaction: sweeping the pointer down a
	// 42-row table would fire 42 requests, each of which hits the filesystem
	// via loadSaaSHive in handleHiveTimeline. No debounce or TTL cache removes
	// that — scanning the list IS the normal way the table is read, so the
	// requests are the common case, not the edge case.
	//
	// The cost of embedding was measured rather than guessed: at
	// myHivesRecentEventCount events per row a typical fleet of 42 hives adds
	// ~14 KB, and ~50 KB in the pathological case where every event carries a
	// maxed-out timelineMaxDetailRunes detail. That is small beside what a row
	// already ships (health map, drift report, access list, agents,
	// leaderboard and the issue/PR spark histories), and the events are
	// already in memory hub-side, so serving them costs no extra I/O.
	//
	// Populated under the SAME authorization as Access — owner or admin only —
	// because it is the same per-hive operational detail handleHiveTimeline
	// guards. The full 200-event history stays behind that endpoint and its
	// modal; this is only the hover preview.
	RecentEvents []TimelineEvent `json:"recentEvents,omitempty"`

	// AdvisoryIssueActivity is the fleet row's advisory/issue output freshness:
	// the newest successful advisory digest post or actionable-issue count
	// movement already reported to the hub, bucketed on read. Placeholders and
	// old/non-advisory spokes with no signal still get the field with bucket
	// "unknown" so every row renders an explicit n/a instead of disappearing.
	AdvisoryIssueActivity AdvisoryIssueActivity `json:"advisoryIssueActivity"`

	// BudgetHealth is this hive's current governor budget-window usage, bucketed
	// for the fleet row. It includes the underlying spend/limit/window numbers so
	// the UI can explain the dot without reverse-engineering RegistryEntry.
	BudgetHealth BudgetHealth `json:"budgetHealth"`

	// GitHubAppHealth is this hive's GitHub App token/auth health for the fleet
	// row, bucketed server-side so every consumer shares the same thresholds and
	// problem semantics.
	GitHubAppHealth GitHubAppHealth `json:"githubAppHealth"`

	// AdvisoryStale is true when this hive SHOULD be posting advisory digests
	// but its digest has quietly gone stale — computed on read by advisoryStale()
	// so the browser never re-derives the threshold or the gating (advisory-mode
	// + app-can-write) and cannot drift from the Go rule. AdvisoryStaleReason is
	// the tooltip cause. Both stay zero/empty for hives that are not in advisory
	// mode, whose App cannot write, or that report an unknown timestamp — those
	// must never show the pill.
	AdvisoryStale       bool   `json:"advisoryStale,omitempty"`
	AdvisoryStaleReason string `json:"advisoryStaleReason,omitempty"`

	// AutoUpgradeBlocked is true when this hive has auto-upgrade ON but the hub
	// will REFUSE to arm it: upgradeCollectible() is false, so the spoke cannot
	// pull the instruction off its own heartbeat and triggerAutoUpgrades()
	// declines every cycle. AutoUpgradeBlockedReason is the operator-facing
	// cause from uncollectibleUpgradeReason() — the SAME string
	// noteUncollectibleUpgrade() writes to the timeline, and documented there as
	// free of credentials and kubeconfig paths, so it is safe as a tooltip.
	//
	// WHY THIS IS COMPUTED ON READ RATHER THAN RE-DERIVED IN JAVASCRIPT. The
	// fleet row used to render "Queued for auto-upgrade · 1pm ET" from
	// autoUpgradeMode alone, which consults nothing about eligibility. A hive
	// the hub had permanently refused therefore advertised a queued upgrade
	// while the timeline recorded the refusal — two surfaces, opposite stories,
	// and no way to tell "waiting for the window" from "will never fire". The
	// browser must not re-implement the predicate or its staleRemoveAge bound;
	// sending the evaluated decision is what keeps badge and hub in agreement.
	//
	// DELIBERATELY ONLY THE REFUSED GATE. The other gates in
	// triggerAutoUpgrades() (claim in flight, wave full, provisioning, the
	// schedule itself) are TRANSIENT — they clear on their own, so a badge
	// saying "queued" is eventually true. Uncollectible is the one state that
	// never resolves without operator action, which is why it is the one worth
	// naming distinctly.
	AutoUpgradeBlocked       bool   `json:"autoUpgradeBlocked,omitempty"`
	AutoUpgradeBlockedReason string `json:"autoUpgradeBlockedReason,omitempty"`

	// The inference-backend auth-failure signal (InferenceAuthError) is NOT
	// re-declared here: MyHiveEntry embeds RegistryEntry, which already carries
	// the spoke-reported InferenceAuthError verbatim, so the promoted field is
	// what both the alert evaluator (alertHiveFromEntry) and the JSON payload
	// read. The spoke owns the consecutive-failure threshold and the self-heal,
	// so there is nothing to compute on read the way AdvisoryStale is computed.
	CommitsBehindStableV4 *int `json:"commitsBehindStableV4,omitempty"`

	// InactiveAgents is how many of this hive's agents are RUNNING but not
	// doing any work — session gone, sitting on a login prompt, or producing
	// nothing while work is queued. Computed on read by
	// evaluateInactiveAgents() so the browser never re-derives the thresholds
	// or the paused/on-demand gating and cannot drift from the Go rule.
	//
	// Agents the operator deliberately PAUSED are excluded by that rule and
	// never counted here: a pause is a choice, not a fault, and a facet that
	// alarms on it would be wrong on every hive with a parked agent.
	// InactiveAgentsReason is the tooltip cause. Both stay zero/empty for
	// hives with nothing wrong, so the pill and the facet self-suppress.
	InactiveAgents       int    `json:"inactiveAgents,omitempty"`
	InactiveAgentsReason string `json:"inactiveAgentsReason,omitempty"`

	// AllAgentsQuiet is true when EVERY agent this hive reports is deliberately
	// quiet — paused or off-schedule. This is "hive not in use": nothing is
	// broken, nothing will be produced, and the same condition suppresses the
	// advisory-staleness pill (allAgentsQuietByDesign). Computed on read so the
	// browser never re-derives the pause/off-schedule rule; the fleet page
	// renders it as a distinct state chip rather than health or fault.
	AllAgentsQuiet bool `json:"allAgentsQuiet,omitempty"`

	// FleetRollup / AgentVerdicts carry the three-way divergence view — what the
	// governor EXPECTS running, what is ACTUALLY running, and what is ABLE to
	// fulfill its mission — computed on read from the per-agent heartbeat
	// signals + this hive's blocker fields. FleetRollup is the per-spoke header
	// ("expects N · M running · K able"); AgentVerdicts is the per-agent
	// drill-down. Both stay nil for a hive with no reported agents. Computed on
	// read (deriveAgentVerdict/rollupAgents) so the browser never re-derives the
	// state machine and cannot drift from the Go rule.
	FleetRollup   *agentFleetRollup  `json:"fleetRollup,omitempty"`
	AgentVerdicts []AgentVerdictJSON `json:"agentVerdicts,omitempty"`

	// AgentRosterMismatch is an additive warning when the spoke's reported
	// agent list no longer matches the ACMM pack roster for its level. It does
	// not change the red/green hive verdict; red production failures still
	// outrank this yellow configuration-drift signal.
	AgentRosterMismatch *agentRosterMismatch `json:"agentRosterMismatch,omitempty"`

	// HealthVerdict is the at-a-glance hive-health verdict (hive-health): does
	// this spoke have RECENT OUTPUT back to its work source, banded by ACMM
	// level? green/red/unknown with a WHY reason, computed on read from the same
	// rollup/app-health/queue/advisory/repo-activity signals the row already
	// carries. nil for placeholder rows (nothing to judge). Named distinctly
	// from the embedded RegistryEntry.Health (the raw spoke-reported blob) to
	// avoid shadowing it. See health_verdict.go.
	HealthVerdict *HealthVerdict `json:"healthVerdict,omitempty"`

	// URLUnreachable is true when this hive's PUBLIC dashboard URL failed to
	// serve on the last several probes — the link in this very table is dead.
	// Computed on read from the auth-audit loop's observations, so the browser
	// never re-derives the failure threshold. Stays zero/empty for a hive that
	// is serving, that is too new to have converged, or whose whole cluster is
	// out (an outage is one condition, not N broken hives) — those must never
	// show the pill.
	URLUnreachable       bool   `json:"urlUnreachable,omitempty"`
	URLUnreachableReason string `json:"urlUnreachableReason,omitempty"`
	// PrivateURL is true when the hub's public-network probe cannot reach the
	// dashboard URL, but the hive is freshly heartbeating and does not report a
	// self-check failure. It renders as an informational "private URL" chip,
	// not a critical dead-link chip.
	PrivateURL       bool   `json:"privateUrl,omitempty"`
	PrivateURLReason string `json:"privateUrlReason,omitempty"`

	// Quadrant is this hive's four-axis score — trust, efficiency,
	// satisfaction, productivity — computed on read and never persisted.
	//
	// It lives HERE rather than on RegistryEntry (where Journey sits) because
	// unlike every other derived field on a row, a quadrant is not a property
	// of the hive alone: the scores are percentiles against the other hives in
	// the SAME view. Two requests over different filters legitimately produce
	// different numbers for one hive, so caching it on the shared registry
	// entry would let one caller's filtered population leak into another's.
	//
	// Nil when the caller is not entitled to see it, or when the population is
	// too small to rank honestly — the browser renders nothing at all in that
	// case rather than an empty chart.
	Quadrant *Quadrant `json:"quadrant,omitempty"`
}

// myHivesRecentEventCount is how many timeline events ride the My Hives
// payload for the status hover. The hover panel already carries a status word,
// the per-check health lines, the relay line and the user list; three events
// is enough to answer "what just happened to this hive?" without turning a
// transient tooltip into a scrolling log. "See all" opens the full modal.
const myHivesRecentEventCount = 3

func (s *HubServer) handleMyHives(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	user := ensureSaaSUser(username)

	s.mu.Lock()
	offlineEvents := s.markStaleHives()
	allHives := make([]RegistryEntry, len(s.registry.Hives))
	copy(allHives, s.registry.Hives)
	s.mu.Unlock()
	s.flushOfflineEvents(offlineEvents)

	var result []MyHiveEntry

	autoUpgradeMap := make(map[string]bool)
	// Normalized here (empty → instant) so every consumer sees the effective
	// mode rather than the raw legacy blank.
	autoUpgradeModeMap := make(map[string]string)
	saasByID := make(map[string]*SaaSHive)
	for _, sh := range listSaaSHives() {
		shCopy := sh
		autoUpgradeMap[sh.ID] = sh.AutoUpgrade
		autoUpgradeModeMap[sh.ID] = normalizeAutoUpgradeMode(sh.AutoUpgradeMode)
		saasByID[sh.ID] = &shCopy
	}

	// enrichFromSaaSMeta overlays SaaS meta.json fields onto an entry built from
	// a live registry hive (registry entries come from spoke heartbeats and carry
	// NO provStatus, so provStatus/migration/error must come from meta).
	//
	// ACMM level is the subtle case, and getting it wrong caused a regression:
	//   - For a CLAIMED / running hive, the spoke's LIVE heartbeat level is
	//     authoritative — it's what the hive is actually running. Many pre-claim
	//     meta.json records still carry a stale acmm_level: 0 even though the
	//     spoke runs at a real level, so unconditionally taking the meta level
	//     downgraded live hives to L0 on the dashboard.
	//   - For an unclaimed PLACEHOLDER (status: available), there is no
	//     meaningful live level (the slot reports L0), so the INTENDED level from
	//     meta ("L2 Advisory") is what should show, and it also gates the
	//     "Assign" menu (provStatus === 'available').
	// Rule: take the meta level only for placeholders, or as a fallback when the
	// live registry level is 0 (unknown) but meta has a real one. Otherwise the
	// live registry level wins.

	enrichFromSaaSMeta := func(entry *MyHiveEntry) {
		sh := saasByID[entry.ID]
		if sh == nil {
			return
		}
		entry.ProvStatus = sh.Status
		// The tracked release channel comes from meta, never the registry: the
		// spoke's heartbeat rewrites GitBranch with the image's baked-in branch
		// every beat, which is exactly how the channel selection was being
		// forgotten. Read-time overlay also means the pill flips to the channel
		// on the very next poll after the switch, without waiting for a beat.
		entry.TrackedChannel = sh.TrackedChannel
		// Overlay the hosted namespace at read time too, so a placeholder or a
		// hive whose live registry entry predates the field still shows
		// "hive-hosted-<id>" in My Hives. Derived from the SaaSHive record, same
		// as the heartbeat path — the value the operator needs for kubectl exec.
		entry.Namespace = hostedNamespaceForHive(sh)
		// A placeholder wedged between the two claim paths: it was given a real
		// identity (org/repo written) but the spoke never reported the project back.
		// Computed from meta (not the live registry) so it is true even for an
		// offline/silent spoke — the exact dead-end the Reset assignment action
		// targets.
		//
		// The predicate is deliberately broader than "Status == statusAssigned".
		// The current assign paths (handleApproveProvision / handleAssignHive) always
		// stamp Status=statusAssigned alongside the org/repo, but LEGACY/wedged
		// placeholders exist that carry a REAL org/primary_repo with Status left NULL
		// or "" (e.g. the live hosted-available-vllmd-13: org=z-innersource,
		// repo=AutoIPL, status=null) — precisely the wedge the escape hatch was built
		// to rescue. Keying only on statusAssigned HID the Reset item for exactly
		// those slots.
		//
		// A clean available slot is NOT bare — a seeded/reset placeholder carries the
		// synthetic "available-<id>" org (placeholderOrgPrefix) and an empty
		// primary_repo, which is how isPlaceholderEntry recognizes inventory. So "has
		// a real assigned identity" means a non-empty org that is NOT the placeholder
		// prefix, or any primary_repo. Treat a placeholder as assigned-unclaimed
		// whenever it is NOT a delivered claim (that is a LIVE hive) AND is NOT a
		// clean available slot but HAS been handed a real identity (Status==assigned,
		// or a real org / primary_repo written under any/no status).
		hasRealOrg := sh.Org != "" && !strings.HasPrefix(sh.Org, placeholderOrgPrefix)
		notClaimed := !sh.ClaimDelivered
		hasAssignedIdentity := hasRealOrg || sh.PrimaryRepo != ""
		isAssignedStatus := sh.Status == statusAssigned
		isCleanAvailable := sh.Status == statusAvailable && !hasAssignedIdentity
		entry.AssignedUnclaimed = notClaimed && !isCleanAvailable && (isAssignedStatus || hasAssignedIdentity)
		// Ride the assignment clock and the self-heal threshold ONLY on an
		// assigned-but-unclaimed row, so the dashboard can tick a "claim pending"
		// counter and tint it amber as it nears the auto-reset the sweep enforces.
		// Both stay zero/empty otherwise so the counter self-suppresses. Uses the
		// broadened AssignedUnclaimed above, so the counter also covers null-Status
		// wedges — the same rows the Reset action now reaches.
		if entry.AssignedUnclaimed {
			entry.AssignedAt = sh.AssignedAt
			entry.AssignStuckSeconds = int(assignStuckResetTimeout / time.Second)
		}
		if sh.Status == statusAvailable || (entry.ACMMLevel == 0 && sh.ACMMLevel > 0) {
			entry.ACMMLevel = sh.ACMMLevel
		}
		// Show the friendly vanity host for a claimed hive the instant it is
		// claimed, rather than waiting for the spoke to adopt+report it back over
		// the heartbeat. Until then entry.DashboardURL (spoke-reported) is still the
		// raw placeholder host, which is the placeholder-URL-persists bug in My
		// Hives / the row's Open link. Only overlays a validated meta vanity_url;
		// an unclaimed placeholder or a hive with no vanity keeps its own URL.
		if v := claimedVanityURL(sh); v != "" {
			entry.DashboardURL = v
		}
		switch sh.Status {
		case "provisioning":
			entry.GovernorMode = "PROVISIONING"
		case "error":
			entry.GovernorMode = "ERROR"
			entry.ProvError = sh.Error
		}

		// Assigning transient state: after a placeholder is assigned, the meta
		// records the real project but the spoke still reports the old placeholder
		// identity until it reconciles via heartbeat.
		//
		// projectConfigForHiveID returns non-nil whenever meta != what the spoke
		// reports — but that ALSO happens transiently during an UPGRADE (the spoke
		// pod restarts and its heartbeat momentarily reports an empty/stale org),
		// which falsely lit "Assigning to <org>" on already-claimed hives that were
		// merely upgrading. Guard against that:
		//   - never show Assigning while the hive is Upgrading (an upgrade is not
		//     an assignment), and
		//   - only show it when the spoke is genuinely reporting a DIFFERENT,
		//     non-empty project (an empty reported org means the spoke just
		//     restarted / hasn't beaten yet — not a fresh assignment).
		spokeReportsDifferentProject := entry.Org != "" && !strings.EqualFold(entry.Org, sh.Org)
		if !entry.Upgrading && spokeReportsDifferentProject &&
			// Empty curAPIURL deliberately: a GHE API-URL-only push is a config
			// repair, not an assignment, and must never light "Assigning to <org>".
			projectConfigForHiveID(entry.ID, entry.Org, entry.Repos, entry.PrimaryRepo, entry.ACMMLevel, entry.DashboardURL, "") != nil {
			entry.Assigning = true
			entry.AssigningTo = sh.Org
		}
	}

	// resolveGitHubHost fills in the Location-column GitHub host pill when the
	// spoke did not report one over the heartbeat.
	//
	// The spoke-reported value always wins: it is the hive's real runtime
	// GitHub, and it is the only source that is correct when a hive's GitHub
	// differs from its cluster's default (the enricom8 hive is the live
	// example — its host had to be corrected by hand). Heartbeat-only and
	// firewalled spokes upgrade slowly, so for those this falls back to the
	// hive's recorded host, then its cluster's default. Anything still empty
	// is left empty and rendered as "github.com" by the pill, so an old spoke
	// shows a sane default rather than a wrong host.
	resolveGitHubHost := func(entry *MyHiveEntry) {
		if entry.GitHubHost != "" {
			return // spoke-reported — authoritative
		}
		if sh := saasByID[entry.ID]; sh != nil {
			if sh.GitHubHost != "" {
				entry.GitHubHost = githubHostLabel(sh.GitHubHost)
				return
			}
			if c := s.clusterForHive(sh); c != nil && (c.GitHubBaseURL != "" || c.GitHubAPIURL != "") {
				// base-or-api so a GHE cluster recorded with only an api_url
				// (blank base_url — the common state) is recognised as GHE, not
				// mislabelled github.com.
				entry.GitHubHost = clusterGitHubConfig(c).HostLabel()
				return
			}
		}
		// No meta record: fall back to the cluster the registry entry reports.
		// s.clusters is read unlocked here to match every other reader in this
		// package (clusterForHive, clusterNameForID); it is effectively
		// immutable after load, and taking s.mu here would nest inside callers
		// that already hold it.
		if entry.ClusterID != "" {
			if c, ok := s.clusters[entry.ClusterID]; ok && (c.GitHubBaseURL != "" || c.GitHubAPIURL != "") {
				entry.GitHubHost = clusterGitHubConfig(&c).HostLabel()
			}
		}
	}

	isAdmin := isHubAdmin(username)
	for _, h := range allHives {
		if role, ok := user.Hives[h.ID]; ok {
			// A stale/demoted stored role must not hide owner-gated UI (the
			// Upgrade link, auto-upgrade controls) from the hive's TRUE owner.
			// Normalize to owner for the admin (as before) AND for the
			// canonical owner of this hive — owners are only elevated on their
			// OWN hives (#4081).
			if role != "owner" && canonicalEqual(h.Owner, username) {
				role = "owner"
				user.Hives[h.ID] = "owner" // heal the demoted stored role
			}
			if isAdmin && role != "owner" {
				role = "owner"
			}
			entry := MyHiveEntry{RegistryEntry: h, Role: role, AutoUpgrade: autoUpgradeMap[h.ID], AutoUpgradeMode: autoUpgradeModeMap[h.ID]}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
			continue
		}
		if canonicalEqual(h.Owner, username) {
			entry := MyHiveEntry{RegistryEntry: h, Role: "owner", AutoUpgrade: autoUpgradeMap[h.ID], AutoUpgradeMode: autoUpgradeModeMap[h.ID]}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
			user.Hives[h.ID] = "owner"
			continue
		}
		if isAdmin {
			entry := MyHiveEntry{RegistryEntry: h, Role: "owner", AutoUpgrade: autoUpgradeMap[h.ID], AutoUpgradeMode: autoUpgradeModeMap[h.ID]}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
		}
	}

	seen := make(map[string]bool)
	for _, h := range result {
		seen[h.ID] = true
	}
	for hiveID, role := range user.Hives {
		if seen[hiveID] {
			continue
		}
		if strings.HasPrefix(hiveID, "hosted-") || strings.HasPrefix(hiveID, "saas-") {
			sh := loadSaaSHive(hiveID)
			if sh != nil {
				// Same owner normalization as the registry loop above: the
				// meta record's canonical owner outranks a demoted stored
				// role (#4081).
				if role != "owner" && canonicalEqual(sh.Owner, username) {
					role = "owner"
					user.Hives[hiveID] = "owner"
				}
				entry := MyHiveEntry{
					RegistryEntry: RegistryEntry{
						ID:          sh.ID,
						Name:        sh.Org + "/" + sh.PrimaryRepo,
						Org:         sh.Org,
						Repos:       sh.Repos,
						PrimaryRepo: sh.PrimaryRepo,
						ACMMLevel:   sh.ACMMLevel,
						HiveType:    "hosted",
						Namespace:   hostedNamespaceForHive(sh),
						ClusterID:   clusterIDForSaaSHive(*sh),
						ClusterName: s.clusterNameForID(clusterIDForSaaSHive(*sh)),
					},
					Role: role,
				}
				enrichFromSaaSMeta(&entry)
				result = append(result, entry)
				seen[sh.ID] = true
			}
		}
	}

	for _, sh := range listSaaSHives() {
		if (canonicalEqual(sh.Owner, username) || isAdmin) && !seen[sh.ID] {
			user.Hives[sh.ID] = "owner"
			entry := MyHiveEntry{
				RegistryEntry: RegistryEntry{
					ID:          sh.ID,
					Name:        sh.Org + "/" + sh.PrimaryRepo,
					Org:         sh.Org,
					Repos:       sh.Repos,
					PrimaryRepo: sh.PrimaryRepo,
					ACMMLevel:   sh.ACMMLevel,
					HiveType:    "hosted",
					Namespace:   hostedNamespaceForHive(&sh),
					ClusterID:   clusterIDForSaaSHive(sh),
					ClusterName: s.clusterNameForID(clusterIDForSaaSHive(sh)),
				},
				Role: "owner",
			}
			enrichFromSaaSMeta(&entry)
			result = append(result, entry)
			seen[sh.ID] = true
		}
	}

	if len(user.Hives) > 0 {
		if err := saveSaaSUser(user); err != nil {
			s.logger.Warn("handleMyHives: save failed", "user", username, "error", err)
		}
	}

	// Read the user roster ONCE for the access hover rather than per row —
	// listAllSaaSUsers hits the filesystem for every user record, and My Hives
	// can carry dozens of rows.
	var allSaaSUsers []SaaSUser
	for _, h := range result {
		if h.Role == "owner" || isAdmin {
			allSaaSUsers = listAllSaaSUsers()
			break
		}
	}

	saasCount := 0
	for i, h := range result {
		if strings.HasPrefix(h.ID, "hosted-") || strings.HasPrefix(h.ID, "saas-") {
			saasCount++
		}
		// Backfill the Location-column GitHub host for rows whose spoke is too
		// old to report one. Done here, over the assembled set, so no entry
		// construction site can be missed.
		resolveGitHubHost(&result[i])
		// The leaderboard carries per-USER task counts (who did what on this hive).
		// The admin Users engagement card cross-references it by github_username, so
		// it must reach the admin browser — but it is other people's activity, so a
		// non-admin owner must NOT receive it. Scrub it for everyone but admin.
		// (RegistryEntry.Leaderboard has no omitempty, so it would otherwise ship to
		// every my-hives consumer.)
		if !isAdmin {
			result[i].Leaderboard = nil
		}
		// Who-has-access is shown only to owners (and admin), matching
		// handleAccessList's rule. A read/read-write member is deliberately not
		// told who else shares the hive.
		if h.Role == "owner" || isAdmin {
			// Notes is admin-only CRM text; a non-admin owner gets name+Slack only.
			result[i].Access = accessForHive(h.ID, allSaaSUsers, isAdmin)
			// Recent activity for the status hover, same owner/admin rule as
			// the access list and as handleHiveTimeline itself. s.timeline is
			// nil in tests that construct a bare HubServer, and recent() is a
			// read under the store's own leaf mutex — no s.mu is held here.
			if s.timeline != nil {
				result[i].RecentEvents = s.timeline.recent(h.ID, myHivesRecentEventCount)
			}
		}
		if config.RoleAtLeast(h.Role, config.RoleReadWrite) || isAdmin {
			reqs := loadAccessRequests(h.ID)
			var pending []PendingAccessRequest
			for _, req := range reqs {
				if req.Status == "pending" {
					pending = append(pending, PendingAccessRequest{
						Username:    req.Username,
						RequestedAt: req.RequestedAt,
						Note:        req.Note,
					})
				}
			}
			pending = s.decoratePendingAccessRequests(pending)
			result[i].PendingRequestCount = len(pending)
			result[i].PendingRequests = pending
		}
		result[i].Unassigned = isPlaceholderEntry(result[i])
	}

	// Unassigned placeholder rows: auth-class check failures are the pool's
	// DESIGNED state, not degradation, so neutralise them before anything
	// downstream (drift, the fleet alerts, the browser's row dot and
	// failing-checks pill) reads Health. Runs after enrichment for the same
	// provStatus reason annotateDrift documents below, and before it so the
	// drift health signal and the row agree. See placeholder_health.go.
	sanitizePlaceholderRows(result)

	// Config drift, computed once over the caller's full visible set — the
	// fleet norm is derived from that set, so this must run AFTER every row has
	// been collected and enriched (a row still missing its provStatus would be
	// misread as a claimed hive and flagged for having no App).
	annotateDrift(result, getDisplaySHAs(), time.Now())

	// Dead-link pill, computed once over the full visible set for the same
	// reason as drift: the cluster-outage suppression is a property of the SET
	// (most of a cluster failing is an outage, not N broken hives), so it
	// cannot be decided one row at a time. Derived from the same alert list the
	// panel renders, so pill and panel always agree.
	{
		regs := make([]RegistryEntry, 0, len(result))
		for i := range result {
			regs = append(regs, result[i].RegistryEntry)
		}
		urlAlerts := s.urlUnreachableAlerts(regs, time.Now())
		for i := range result {
			if bad, reason := urlUnreachableFacet(urlAlerts, result[i].ID); bad {
				result[i].URLUnreachable = true
				result[i].URLUnreachableReason = reason
			}
			if private, reason := privateURLFacet(urlAlerts, result[i].ID); private {
				result[i].PrivateURL = true
				result[i].PrivateURLReason = reason
			}
		}
	}

	// Attach the user-journey stage to every row so the table can show who is
	// stalled where. Derived on read; never persisted on the registry entry.
	journeyNow := time.Now()
	for i := range result {
		if count, known := commitsBehindStableV4(result[i].GitHash, s.logger); known {
			result[i].CommitsBehindStableV4 = &count
		}

		st := s.journey.get(result[i].ID)
		status := JourneyStatusFor(&result[i].RegistryEntry, st, journeyNow)
		result[i].Journey = &status
		result[i].AdvisoryIssueActivity = advisoryIssueActivityFor(result[i].RegistryEntry, journeyNow)
		result[i].BudgetHealth = budgetHealthFor(result[i].RegistryEntry)
		result[i].GitHubAppHealth = githubAppHealthFor(result[i].RegistryEntry, journeyNow)

		// Advisory-staleness pill, computed on read (same as Journey) so the
		// gating — advisory-mode participation, app-can-write, past-threshold —
		// lives ONLY in Go and the browser just renders the flag.
		if stale, reason := advisoryStale(result[i].RegistryEntry, journeyNow); stale {
			result[i].AdvisoryStale = true
			result[i].AdvisoryStaleReason = reason
		}

		// Auto-upgrade REFUSAL, computed on read for the same reason: the
		// predicate and its staleRemoveAge bound live ONLY in Go, so the fleet
		// badge cannot drift from what triggerAutoUpgrades() will actually do.
		// Gated on AutoUpgrade because the state only means anything for a hive
		// that has asked for auto-upgrades in the first place — a manual hive is
		// not "blocked", it is simply manual.
		if blocked, reason := autoUpgradeBlocked(result[i].AutoUpgrade, result[i].LastHeartbeat, journeyNow); blocked {
			result[i].AutoUpgradeBlocked = true
			result[i].AutoUpgradeBlockedReason = reason
		}

		// Running-but-inactive agents, computed on read for the same reason:
		// the thresholds and the paused/on-demand exclusions live ONLY in Go.
		//
		// The queue gate is the governor's own actionable backlog, which is
		// what makes the idle rule safe on a genuinely quiet hive: with no
		// issues and no PRs waiting, idle agents are CORRECT and nothing is
		// reported. The two unambiguous faults (dead session, login prompt)
		// are independent of it.
		queuedWork := result[i].ActionableIssues + result[i].ActionablePRs
		if rep := evaluateInactiveAgents(result[i].Agents, queuedWork, journeyNow); rep.Count > 0 {
			result[i].InactiveAgents = rep.Count
			result[i].InactiveAgentsReason = rep.Reason
		}

		// "Hive not in use": every reported agent deliberately quiet. Same
		// predicate that suppresses the advisory-stale pill, surfaced as its
		// own state so an entirely-parked hive reads as PARKED, not healthy
		// and not broken.
		result[i].AllAgentsQuiet = allAgentsQuietByDesign(result[i].RegistryEntry)

		// Fleet-divergence view: derive the three-way picture (expected vs
		// actual vs able) and the per-agent verdicts from the same per-agent
		// heartbeat signals plus this hive's blocker fields. Derived on read so
		// the browser never re-runs the state machine (shares classifyInactive‐
		// Agent with the block above, so the two can never disagree).
		//
		// SKIP placeholders/pool hives entirely: an unclaimed placeholder runs
		// its default agents against no real repo, so they legitimately report
		// "expected N · 0 able · N impotent" — a cascade of FALSE alarms that
		// would drown the one signal the view exists for. No verdicts → the
		// frontend has nothing to render for them and they carry no problem
		// count. (isPlaceholderEntry is the same authoritative test computeFleet‐
		// Stats and the alert layer use.)
		if len(result[i].Agents) > 0 && !isPlaceholderEntry(result[i]) {
			blockers := hiveBlockers{
				GitHubAppRequired:       result[i].GitHubAppRequired,
				GitHubAppPermIssue:      result[i].GitHubAppPermIssue,
				GitHubAppState:          result[i].GitHubAppState,
				RepoTargetMisconfigured: result[i].RepoTargetMisconfigured,
				RepoTargetIssue:         result[i].RepoTargetIssue,
				InferenceAuthError:      result[i].InferenceAuthError,
				ProviderLimitReason:     result[i].ProviderLimitReason,
			}
			rollup := rollupAgents(result[i].Agents, blockers, queuedWork, journeyNow)
			result[i].FleetRollup = &rollup
			result[i].AgentVerdicts = buildAgentVerdicts(result[i].Agents, blockers, queuedWork, journeyNow)
			result[i].AgentRosterMismatch = computeAgentRosterMismatch(result[i].ACMMLevel, result[i].Agents)

			// Hive-health verdict: reuse the rollup + app-health + queue depth we
			// just computed. Only for real (non-placeholder) hives with reported
			// agents — a placeholder has nothing to produce.
			verdict := hiveHealthFor(result[i].RegistryEntry, rollup, result[i].GitHubAppHealth, queuedWork, journeyNow)
			result[i].HealthVerdict = &verdict
		}

		// Sparkline history dominated this payload: at 42 hives the two series
		// were ~755 KB of an 818 KB response (92%), yet they are drawn into a
		// 50 px-wide SVG. Downsample on the WIRE only — the registry keeps the
		// full 7-day series, so nothing is lost server-side and a future
		// full-resolution view can still fetch it per hive.
		result[i].IssueHistory = downsampleSpark(result[i].IssueHistory, sparkWirePoints)
		result[i].PRHistory = downsampleSpark(result[i].PRHistory, sparkWirePoints)
	}

	// Score the quadrant last, once every row is populated: the axes read
	// fields the loop above fills in, and the scores are percentiles against
	// this exact set of rows. Ranking against the whole registry instead would
	// let the header polygon disagree with the rows it summarises.
	fleetQuadrant := attachQuadrants(result, isAdmin, journeyNow)

	// Cosmetic owner labels: resolve opaque OIDC owner identities to their
	// stored display names so "Group by owner" headers and tooltips read like
	// people. Memoized — a fleet shares a handful of owners.
	ownerLabel := s.identityLabeler()
	for i := range result {
		if l := ownerLabel(result[i].Owner); l != result[i].Owner {
			result[i].OwnerName = l
		}
	}

	// Server-side scoping (filter/sort/pagination) happens LAST, after every
	// set-wide computation above (drift norm, alerts, outage suppression,
	// quadrant percentiles) has run over the caller's full visible set — the
	// page is a wire-level view, not a different fleet. No query params →
	// full set, exactly as before.
	hivesView := result
	query := parseMyHivesQuery(r.URL.Query())
	matched := len(result)
	if query.active() {
		hivesView, matched = applyMyHivesQuery(result, query)
	}

	resp := map[string]any{
		"hives": hivesView,
		// The fleet average backs the reference polygon drawn behind every
		// row's kite and the aggregate at the top of the dashboard. It is an
		// aggregate over many hives and identifies none of them, so unlike the
		// per-hive scores it is not gated on the caller's role.
		"fleet_quadrant": fleetQuadrant,
		// Summary counts over the FULL visible set (never the page) so
		// dashboard tiles stay truthful under any filter.
		"hives_summary":            myHivesSummary(result),
		"hives_total":              len(result),
		"hives_matched":            matched,
		"saas_quota":               user.SaaSQuota,
		"saas_used":                saasCount,
		"is_admin":                 isAdmin,
		"latest_sha":               getLatestSHA(),
		"stable_v4_sha":            getLatestSHAForBranch(stableReleaseBranch),
		"latest_shas":              getDisplaySHAs(),
		"latest_sha_messages":      getDisplaySHAMessages(),
		"latest_sha_image_status":  getImageStatuses(),
		"latest_sha_build_started": getImageBuildStartTimes(),
		"commit_messages":          getCommitMessages(),
		"hub_git_hash":             s.hubGitHash,
		"hub_git_branch":           s.hubGitBranch,
		"tracked_branches":         s.trackedBranchList(),
		// Release channels are moving tags; the dashboard renders them as their
		// own "channel -> image" block above the per-branch rows, and offers
		// them as branch-switch targets. The association is resolved from
		// registry digests (cached), never hardcoded to a branch name.
		"release_channels":  ReleaseChannels(),
		"channel_targets":   getChannelTargets(getDisplaySHAs(), s.logger),
		"hub_auto_upgrade":  isHubAutoUpgrade(),
		"hub_upgrade_state": s.hubUpgradeState(),
		// Kill-switch state rides the top-level payload (NOT the hive-row
		// shape, so no HIVES_CACHE_VERSION bump): the dashboard shows a
		// prominent banner while anything is paused, and admins get toggles.
		"upgrade_pause": s.upgradePauseSnapshot(),
		"show_my_hives": true,
		// Fleet alerts ship WITH the hive list so the "Attention needed" panel
		// renders in the same paint as the rows it summarises — a second
		// round-trip would make the panel pop in after the list and shift it.
		// Scoped to the hives this caller can already see, so it never leaks
		// the existence of a hive they have no access to.
		"alerts": s.fleetAlerts(result),
	}

	myReq := loadProvisionRequest(username)
	if myReq != nil {
		resp["my_provision_request"] = myReq
	}

	if isAdmin {
		// Admin-only: enrichProvisionRequests attaches other users' hive
		// memberships and roles, so it must stay inside this branch. A non-admin
		// caller never receives provision_requests at all.
		resp["provision_requests"] = enrichProvisionRequests(listProvisionRequests())
		// Who is logged into their hive RIGHT NOW, for the green-dashed avatar
		// treatment. Presence data about other users → admin-only, same as the
		// engagement stats it complements.
		resp["live_hive_users"] = s.liveHiveUsernames()
		// The honest subset of live_hive_users: users whose browser reported
		// focused, recent-input presence. live minus engaged = idle open tabs.
		resp["live_engaged_users"] = s.engagedHiveUsernames()
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *HubServer) handleAccessStatus(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authenticated": false,
			"show_my_hives": false,
		})
		return
	}

	user := loadSaaSUser(username)
	if user == nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authenticated": true,
			"show_my_hives": true,
			"hives":         map[string]string{},
		})
		return
	}

	s.mu.Lock()
	offlineEvents := s.markStaleHives()
	allHives := make([]RegistryEntry, len(s.registry.Hives))
	copy(allHives, s.registry.Hives)
	s.mu.Unlock()
	s.flushOfflineEvents(offlineEvents)

	type hiveAccessInfo struct {
		Role   string `json:"role"`
		Status string `json:"status"`
	}
	hiveAccess := make(map[string]hiveAccessInfo)

	isAdmin := isHubAdmin(username)
	for _, h := range allHives {
		if role, ok := user.Hives[h.ID]; ok {
			// Owner normalization mirroring handleMyHives: a stale/demoted
			// stored role must not mask the hive's TRUE owner (#4081).
			if role != "owner" && canonicalEqual(h.Owner, username) {
				role = "owner"
			}
			hiveAccess[h.ID] = hiveAccessInfo{Role: role, Status: "accepted"}
			continue
		}
		if canonicalEqual(h.Owner, username) {
			hiveAccess[h.ID] = hiveAccessInfo{Role: "owner", Status: "accepted"}
			continue
		}
		if isAdmin {
			hiveAccess[h.ID] = hiveAccessInfo{Role: "owner", Status: "accepted"}
			continue
		}
		reqs := loadAccessRequests(h.ID)
		for _, req := range reqs {
			if req.Username == username && req.Status == "pending" {
				hiveAccess[h.ID] = hiveAccessInfo{Status: "pending"}
				break
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"authenticated":            true,
		"show_my_hives":            true,
		"hives":                    hiveAccess,
		"latest_sha":               getLatestSHA(),
		"latest_shas":              getDisplaySHAs(),
		"latest_sha_messages":      getDisplaySHAMessages(),
		"latest_sha_image_status":  getImageStatuses(),
		"latest_sha_build_started": getImageBuildStartTimes(),
		"commit_messages":          getCommitMessages(),
	})
}

func (s *HubServer) handleCreateHive(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	user := loadSaaSUser(username)
	if user == nil || user.Blocked {
		http.Error(w, `{"error":"account blocked or not found"}`, http.StatusForbidden)
		return
	}

	if user.SaaSQuota == 0 {
		http.Error(w, `{"error":"no hosted hive quota — contact the hub admin to request access"}`, http.StatusForbidden)
		return
	}

	const maxCreateHiveBodyBytes = 64 * 1024 // 64 KiB — includes app private key
	r.Body = http.MaxBytesReader(w, r.Body, maxCreateHiveBodyBytes)
	var req CreateHiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if host, org, reposFromOrg := normalizeProjectRef(req.Org); org != "" && (host != "" || len(reposFromOrg) > 0) {
		if host != "" {
			req.GitHubBaseURL = "https://" + host
			req.GitHubAPIURL = forgeAPIURLForHost("", host)
		}
		req.Org = org
		if len(reposFromOrg) > 0 {
			prefix := strings.Join(reposFromOrg, "/")
			if strings.TrimSpace(req.Repos) == "" {
				req.Repos = prefix
			}
			if strings.TrimSpace(req.PrimaryRepo) == "" {
				req.PrimaryRepo = prefix
			}
		}
	} else if originalFirstRepo := firstCSV(req.Repos); originalFirstRepo != "" {
		host, org, reposFromShifted := normalizeProjectRef(req.Org + "/" + originalFirstRepo)
		if host != "" && org != "" && strings.Contains(req.Org, ".") {
			req.GitHubBaseURL = "https://" + host
			req.GitHubAPIURL = forgeAPIURLForHost("", host)
			req.Org = org
			repos := replaceFirstCSV(req.Repos, strings.Join(reposFromShifted, "/"))
			req.Repos = repos
			if strings.TrimSpace(req.PrimaryRepo) == "" || strings.TrimSpace(req.PrimaryRepo) == originalFirstRepo {
				req.PrimaryRepo = firstCSV(repos)
			}
		}
	}

	reposForValidation := splitCSV(req.Repos)
	if len(reposForValidation) == 0 {
		reposForValidation = []string{""}
	}
	primaryForValidation := strings.TrimSpace(req.PrimaryRepo)
	if primaryForValidation == "" && len(reposForValidation) > 0 {
		primaryForValidation = reposForValidation[0]
	}
	if issue := config.ValidateProjectRepoTargets(req.Org, reposForValidation, primaryForValidation, repoTargetForgeHost(req.GitHubBaseURL)); issue != nil {
		writeJSONError(w, http.StatusBadRequest, issue.Message)
		return
	}
	if req.Org == "" || req.Repos == "" {
		http.Error(w, `{"error":"org and repos are required"}`, http.StatusBadRequest)
		return
	}
	if !isValidName(req.Org) {
		http.Error(w, `{"error":"invalid org name — alphanumeric, dashes, dots, underscores only"}`, http.StatusBadRequest)
		return
	}
	for _, r := range strings.Split(req.Repos, ",") {
		if !isValidRepoRef(strings.TrimSpace(r)) {
			http.Error(w, `{"error":"invalid repo name"}`, http.StatusBadRequest)
			return
		}
	}
	hasToken := req.GitHubToken != ""
	hasApp := req.AuthMethod == "app" && req.AppID != "" && req.InstallationID != "" && req.AppPrivateKey != ""
	hasAppLater := req.AuthMethod == "app" && req.AppID != "" && req.InstallationID == "" && req.AppPrivateKey == ""
	if !hasToken && !hasApp && !hasAppLater {
		http.Error(w, `{"error":"provide either a GitHub token or GitHub App credentials"}`, http.StatusBadRequest)
		return
	}
	if hasToken && !strings.HasPrefix(req.GitHubToken, "ghp_") && !strings.HasPrefix(req.GitHubToken, "github_pat_") {
		http.Error(w, `{"error":"token must start with ghp_ or github_pat_"}`, http.StatusBadRequest)
		return
	}
	if hasApp && !strings.HasPrefix(strings.TrimSpace(req.AppPrivateKey), "-----BEGIN") {
		http.Error(w, `{"error":"private key must be PEM format"}`, http.StatusBadRequest)
		return
	}

	if user.SaaSQuota > 0 && countUserHives(username) >= user.SaaSQuota {
		http.Error(w, fmt.Sprintf(`{"error":"quota reached — max %d SaaS hives"}`, user.SaaSQuota), http.StatusBadRequest)
		return
	}

	if maxSaaSHivesTotal > 0 && len(listSaaSHives()) >= maxSaaSHivesTotal {
		http.Error(w, `{"error":"hosted capacity reached — try again later"}`, http.StatusServiceUnavailable)
		return
	}

	repos := strings.Split(req.Repos, ",")
	for i := range repos {
		repos[i] = strings.TrimSpace(repos[i])
	}
	primaryRepo := req.PrimaryRepo
	if primaryRepo == "" && len(repos) > 0 {
		primaryRepo = repos[0]
	}
	acmm := req.ACMMLevel
	if acmm < 1 || acmm > 6 {
		acmm = 1
	}

	// Validate and default the target cluster.
	targetCluster := req.ClusterID
	if targetCluster == "" {
		targetCluster = defaultClusterID
	}
	if _, ok := s.clusters[targetCluster]; !ok {
		http.Error(w, `{"error":"unknown cluster_id"}`, http.StatusBadRequest)
		return
	}

	hiveID := generateHiveID(req.Org, primaryRepo)

	// Determine which cluster to provision on. Default to the hub-reachable cluster if unspecified.
	clusterID := req.ClusterID
	if clusterID == "" {
		clusterID = defaultClusterID
	}
	// Look up the cluster to get its domain for the subdomain.
	cluster, clusterFound := s.clusters[clusterID]
	if !clusterFound {
		http.Error(w, `{"error":"unknown cluster_id"}`, http.StatusBadRequest)
		return
	}
	// Per-cluster ceiling — checked after the global cap so the more specific
	// error wins only when the global gate passes.
	if full, n := clusterAtMaxHives(&cluster); full {
		max := effectiveMaxHives(&cluster)
		s.logger.Warn("provision rejected — cluster at max_hives",
			"cluster", cluster.ID, "count", n, "max_hives", max)
		http.Error(w, fmt.Sprintf(`{"error":"cluster %s is at capacity (%d/%d hives) — pick another cluster or raise max_hives"}`,
			cluster.ID, n, max), http.StatusServiceUnavailable)
		return
	}
	subdomain := hiveID + "." + cluster.Domain

	h := &SaaSHive{
		ID:          hiveID,
		Owner:       username,
		ProjectName: req.ProjectName,
		Org:         req.Org,
		Repos:       repos,
		PrimaryRepo: primaryRepo,
		ACMMLevel:   acmm,
		ClusterID:   targetCluster,
		Status:      "provisioning",
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
		Subdomain:   subdomain,
		// Default to public when the request omits is_public — matches
		// the pre-#1604 template that hardcoded is_public: true. Owners
		// can toggle visibility later from My Hives.
		IsPublic: req.IsPublic == nil || *req.IsPublic,
		// Per-hive GitHub host override (empty = cluster default). Lets a
		// public-GitHub org provision correctly on a GHE-defaulted cluster.
		GitHubBaseURL: req.GitHubBaseURL,
		GitHubAPIURL:  req.GitHubAPIURL,
	}

	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save hive metadata"}`, http.StatusInternalServerError)
		return
	}

	user.Hives[hiveID] = "owner"
	if err := saveSaaSUser(user); err != nil {
		s.logger.Warn("handleCreateHive: owner grant save failed", "hive_id", hiveID, "user", user.GitHubUsername, "error", err)
	}

	provisionHiveRecord := *h
	provisionHiveRecord.Repos = append([]string(nil), h.Repos...)
	provisionReq := req
	// Queued, not spawned: execution is bounded hub-wide and per cluster so a
	// provisioning burst cannot stampede kubectl/OCI (see provision_queue.go).
	enqueueProvision(targetCluster, func() {
		h := &provisionHiveRecord
		cluster := s.clusterForHive(h)
		if cluster == nil {
			h.Status = "error"
			h.Error = "no cluster config available"
			if saveErr := saveSaaSHive(h); saveErr != nil {
				s.logger.Warn("failed to persist hive error status", "hive_id", hiveID, "error", saveErr)
			}
			s.logger.Error("no cluster config for provisioning", "hive_id", hiveID, "cluster_id", h.ClusterID)
			return
		}
		if err := provisionHive(h, &provisionReq, cluster, s.appKeysByAppID(), s.logger); err != nil {
			h.Status = "error"
			h.Error = err.Error()
			if saveErr := saveSaaSHive(h); saveErr != nil {
				s.logger.Warn("failed to persist hive error status", "hive_id", hiveID, "error", saveErr)
			}
			s.logger.Warn("hosted hive provision failed", "hive_id", hiveID, "error", err)
			return
		}
		h.Status = "provisioning"
		if saveErr := saveSaaSHive(h); saveErr != nil {
			s.logger.Warn("failed to persist hive provisioning status", "hive_id", hiveID, "error", saveErr)
		}
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":        hiveID,
		"status":    "provisioning",
		"subdomain": h.Subdomain,
	})
}

func (s *HubServer) handleHiveStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	username := s.getAuthUser(r)
	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found"}`, http.StatusNotFound)
		return
	}
	user := loadSaaSUser(username)
	if user == nil {
		http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
		return
	}
	// Owner-only, matching v2's tightening (operator decision 2026-08-13):
	// effective owners (creator, granted owner, hub admin — userIsHiveOwner)
	// read status; granted read/read-write roles do NOT. The full SaaSHive
	// record includes operational metadata beyond what a read grant implies.
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h)
}

// handleOpenHive is the SSO handoff entry point: a hub-authenticated user hits
// this to open a spoke dashboard without a second GitHub login. It confirms the
// user may access the hive, mints a short-lived HMAC token bound to {user, role,
// hiveID} with the shared hub secret, and 302-redirects to the spoke's
// <dashboardURL>/sso?token=… . The spoke verifies the token and its own
// authorized_users allowlist before minting a session (see dashboard.handleSSO).
//
// If SSO can't be used (no hub secret, or the spoke reported no dashboard URL),
// it falls back to redirecting straight to the dashboard URL (or the hub-reachable-cluster
// host), preserving today's behavior — the spoke will then prompt for login.
func (s *HubServer) handleOpenHive(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.Contains(id, "..") || strings.Contains(id, "/") {
		http.Error(w, `{"error":"invalid hive id"}`, http.StatusBadRequest)
		return
	}
	// /open is the link people actually paste into Slack, so serve the Hive
	// preview card to crawlers here rather than letting them follow the 303 to
	// /login. Short-circuiting is the robust option: an unfurler that caps or
	// skips redirects would otherwise never reach the card, and one that follows
	// the chain unauthenticated lands on GitHub's OAuth page and scrapes GitHub's
	// Open Graph tags — which is exactly the bug.
	if isLinkPreviewCrawler(r) {
		writeLinkPreview(w)
		return
	}
	username := s.getAuthUser(r)
	if username == "" {
		// Not logged in — this is a browser navigation, so send the user through
		// the hub login and return them to THIS /open URL afterward, so the SSO
		// handoff completes and they land logged-in on the spoke (instead of the
		// raw {"error":"not authenticated"} JSON dead end).
		self := "/api/saas/hives/" + url.PathEscape(id) + "/open"
		http.Redirect(w, r, "/login?redirect="+url.QueryEscape(self), http.StatusSeeOther)
		return
	}

	// Resolve the spoke's base URL: prefer the heartbeat-reported dashboard URL
	// (correct for firewalled spokes), fall back to the hub-reachable-cluster host pattern.
	base := ""
	s.mu.RLock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			base = s.registry.Hives[i].DashboardURL
			break
		}
	}
	s.mu.RUnlock()
	// For a claimed hive, hand the SSO handoff off to its vanity host rather than
	// the raw placeholder host the spoke may still be reporting — the vanity URL
	// is the validated, user-facing host (and the one the spoke will settle on).
	// Only a validated meta vanity_url is used; unclaimed placeholders are left on
	// their working placeholder host.
	if v := claimedVanityURL(loadSaaSHive(id)); v != "" {
		base = v
	}
	if base == "" && (strings.HasPrefix(id, "hosted-") || strings.HasPrefix(id, "saas-")) {
		base = s.placeholderHostURL(id)
	}
	if base == "" {
		http.Error(w, `{"error":"hive has no reachable dashboard URL yet"}`, http.StatusConflict)
		return
	}
	base = strings.TrimRight(base, "/")

	// Access gate: only the owner, an authorized user, or the hub admin may open
	// the spoke. The role we pass is advisory — the spoke re-checks its own
	// allowlist and uses that role authoritatively.
	// Every branch below either assigns role or rejects the request, so no
	// initializer is needed (and ineffassign flags one as dead).
	var role string
	if isHubAdmin(username) {
		role = saasRoleOwner
	} else {
		user := loadSaaSUser(username)
		if user == nil {
			http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
			return
		}
		if h := loadSaaSHive(id); h != nil && canonicalEqual(h.Owner, username) {
			role = saasRoleOwner
		} else if r, ok := user.Hives[id]; ok {
			role = r
		} else {
			http.Error(w, `{"error":"access denied"}`, http.StatusForbidden)
			return
		}
	}

	// Mint the handoff token. Without a hub secret we can't sign one, so fall
	// back to a plain dashboard redirect (spoke will prompt for login).
	//
	// Ed25519-only: every v4 spoke verifies SSO handoff tokens with an Ed25519
	// PUBLIC key (see MintSSOToken/VerifySSOToken in sso.go). There is no
	// per-hive branching or legacy symmetric fallback here — the hub always
	// mints with its Ed25519 signing seed, and a spoke that cannot verify
	// Ed25519 tokens simply falls through to the plain dashboard redirect below.
	if s.hubSecret != "" {
		if tok := MintSSOToken(s.ssoSigningSeed(), username, role, id, time.Now()); tok != "" {
			http.Redirect(w, r, base+"/sso?token="+url.QueryEscape(tok), http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, base+"/", http.StatusSeeOther)
}

func (s *HubServer) handleDeleteHive(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	id := r.PathValue("id")
	if strings.Contains(id, "..") || strings.Contains(id, "/") {
		http.Error(w, `{"error":"invalid hive id"}`, http.StatusBadRequest)
		return
	}

	h := loadSaaSHive(id)
	if h == nil {
		// SaaS meta.json already gone — still clean up the in-memory registry so
		// the hive disappears from the listing immediately, and best-effort purge
		// any leftover record dir / timeline file so a partial prior delete cannot
		// leave a status:"available" husk that resurrects in the listing.
		s.removeRegistryEntry(id, username)
		removeHiveRecord(id, s.logger)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": deleteStatusDeleted})
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can delete this hive"}`, http.StatusForbidden)
		return
	}

	// Full de-provisioning: namespace, PV, OCI export, OCI file system, disk record, user cleanup.
	//
	// The registry entry is removed unconditionally, even when the cluster
	// teardown cannot run or only partially succeeds. A hive whose namespace is
	// already gone (or whose cluster config has been removed) would otherwise be
	// permanently un-deletable: the handler used to return 500 before reaching
	// removeRegistryEntry, stranding a ghost row in "My Hives" that no amount of
	// re-clicking Delete could clear. Leaving a stranded row is strictly worse
	// than leaving cloud resources behind, because the row is what the user sees
	// and it is the only thing they can act on. Partial failures are reported in
	// the response so the user knows manual cleanup may still be needed.
	cluster := s.clusterForHive(h)
	if cluster != nil {
		deprovisionHive(h, cluster, s.logger)
	} else {
		s.logger.Error("no cluster config for deprovision; removing registry entry anyway",
			"hive_id", id, "cluster_id", h.ClusterID)
	}
	// Durably purge the hub-side record. This is what stops the "resurrection":
	// deprovisionHive removes the hive directory only when a cluster config is
	// available (the else branch above skips it), and NEITHER path removes the
	// timeline file. Without this, a delete with no cluster config left the
	// hives/<id>/ dir — status:"available" — behind, so the hive reappeared in
	// the unassigned/available list forever. removeHiveRecord is idempotent, so
	// calling it after a successful deprovision (which already removed the dir)
	// is harmless and still cleans up the timeline file deprovision never touched.
	//
	// NOTE: this is the genuine, user-initiated delete path. It is deliberately
	// NOT the placeholder-recycle path — resetting a slot to status:"available"
	// (handleResetAssignment / sweepStuckAssignments in saas_reset_assignment.go)
	// rewrites meta.json in place and KEEPS the record, and never reaches this
	// handler. So purging the record here cannot break placeholder recycling.
	removeHiveRecord(id, s.logger)
	s.removeRegistryEntry(id, username)

	s.logger.Info("audit: hosted hive deleted", "hive_id", id, "by", username,
		"deprovisioned", cluster != nil)
	w.Header().Set("Content-Type", "application/json")
	if cluster == nil {
		// deleteStatusPartial tells the UI the registry row is gone but cloud
		// resources may survive and need manual cleanup.
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  deleteStatusPartial,
			"warning": "removed from the hub registry, but no cluster config was available to delete the namespace, PV, or OCI storage — these may need manual cleanup",
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]string{"status": deleteStatusDeleted})
}

// Delete outcome statuses returned by handleDeleteHive.
const (
	// deleteStatusDeleted means the registry entry was removed and the cluster
	// teardown ran.
	deleteStatusDeleted = "deleted"
	// deleteStatusPartial means the registry entry was removed but the cluster
	// teardown could not run; cloud resources may need manual cleanup.
	deleteStatusPartial = "partially_deleted"
)

func (s *HubServer) handleToggleVisibility(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)

	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found — only hosted hives can be toggled from here"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can change visibility"}`, http.StatusForbidden)
		return
	}

	var body struct {
		IsPublic bool `json:"is_public"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// The hub's registry and SaaS store are the source of truth for the
	// dashboard, so persist the change here first — the same pattern used
	// by handleToggleAutoUpgrade. Pushing the new value to the spoke hive
	// is a best-effort, asynchronous notification: the spoke re-reports
	// its is_public value on every heartbeat, so a transient failure to
	// reach it (pod restarting, rollout in progress, etc.) must not block
	// or fail the user-facing toggle.
	h.IsPublic = body.IsPublic
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save"}`, http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	for i, reg := range s.registry.Hives {
		if reg.ID == id {
			s.registry.Hives[i].IsPublic = body.IsPublic
			break
		}
	}
	s.mu.Unlock()

	s.logger.Info("audit: visibility toggled", "hive_id", id, "is_public", body.IsPublic, "by", username)

	go s.pushVisibilityToSpoke(id, body.IsPublic)

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"is_public":%t}`, body.IsPublic)
}

// maxHiveDisplayNameLen bounds a hive's operator-set display name (ProjectName).
// It matches maxContactNameLen's order of magnitude — a human-readable label,
// not a document — and guards both the persisted meta.json and the vanity-host
// label derived from it (hiveNameHostLabel truncates to a DNS label anyway, so
// this only stops an absurdly long value from ever being stored).
const maxHiveDisplayNameLen = 100

// handleRenameHive renames a hive by rewriting its persisted ProjectName. The
// operator explicitly accepted that ProjectName is load-bearing — it feeds the
// namespace identity annotation and (at claim time) the vanity host — so this
// is a rename of the hive's real identity, NOT a separate display_name field.
//
// AUTHORIZATION mirrors handleToggleVisibility exactly: requireAuth on the
// route, then an owner-or-admin check here that is the true security boundary
// (a non-owner is rejected 403 regardless of what the UI shows).
//
// Derived-surface handling on rename:
//   - Namespace identity is RE-STAMPED here (idempotent, best-effort) so the
//     hive.kubestellar.io/display-name annotation tracks the new name.
//   - The vanity host is NOT recomputed. It is set-once at claim time (the
//     assign path only mints one when VanityURL is empty) and doubles as the
//     "placeholder is claimed" marker; minting a fresh random host on every
//     rename would churn URLs and orphan routes. So the vanity host keeps its
//     original label — a known, documented staleness, not a silent one.
func (s *HubServer) handleRenameHive(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if isSameOriginAsHub(origin) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Methods", "PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	id := r.PathValue("id")
	username := s.getAuthUser(r)

	h := loadSaaSHive(id)
	if h == nil {
		http.Error(w, `{"error":"hive not found — only hosted hives can be renamed from here"}`, http.StatusNotFound)
		return
	}
	if !userIsHiveOwner(username, h) {
		http.Error(w, `{"error":"only the owner can rename this hive"}`, http.StatusForbidden)
		return
	}

	var body struct {
		ProjectName string `json:"project_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}

	// Trim and cap. A blank value is a legitimate "clear the custom name" — the
	// dashboard then falls back to today's org/repo-derived label (hiveLabel).
	name := strings.TrimSpace(body.ProjectName)
	if len(name) > maxHiveDisplayNameLen {
		http.Error(w, `{"error":"name too long"}`, http.StatusBadRequest)
		return
	}
	name = sanitizeField(name)

	h.ProjectName = name
	if err := saveSaaSHive(h); err != nil {
		http.Error(w, `{"error":"failed to save"}`, http.StatusInternalServerError)
		return
	}

	// Overlay the new name onto the in-memory registry immediately so the change
	// is visible before the next heartbeat re-overlays it from the SaaS store.
	s.mu.Lock()
	for i := range s.registry.Hives {
		if s.registry.Hives[i].ID == id {
			s.registry.Hives[i].ProjectName = name
			break
		}
	}
	s.mu.Unlock()

	// Re-stamp namespace identity so the display-name annotation tracks the
	// rename. Best-effort — see stampHostedNamespaceIdentity's doc comment; a
	// failed cosmetic patch must not fail the rename.
	stampHostedNamespaceIdentity(s.clusterForHive(h), hostedNamespaceForHive(h), h.ProjectName, h.Org, h.ID, s.logger)

	s.logger.Info("audit: hive renamed", "hive_id", id, "project_name", name, "by", username)
	s.recordTimeline(id, TimelineRenamed, fmt.Sprintf("hive renamed to %q", name), username)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "project_name": name})
}

// pushVisibilityToSpoke best-effort notifies a hosted hive's own governor
// config of a visibility change made from the hub dashboard. It never
// affects the outcome of the toggle request — the hub's registry/SaaS
// store already reflect the change; this just keeps the spoke's local
// config in sync so it doesn't overwrite the hub's value on its next
// heartbeat. Failures are logged, not surfaced to the user.
func (s *HubServer) pushVisibilityToSpoke(id string, isPublic bool) {
	const goAPIPort = 3002
	const visibilityPushTimeout = 10 * time.Second
	ns := "hive-hosted-" + id
	svcURL := fmt.Sprintf("http://hive.%s.svc.cluster.local:%d/api/config/governor/hub", ns, goAPIPort)
	payload := fmt.Sprintf(`{"is_public":%t}`, isPublic)
	req, err := http.NewRequest("PUT", svcURL, strings.NewReader(payload))
	if err != nil {
		s.logger.Warn("visibility spoke push: failed to create request", "hive", id, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: visibilityPushTimeout}
	spokeResp, err := client.Do(req)
	if err != nil {
		s.logger.Warn("visibility spoke push failed, will resync on next heartbeat", "hive", id, "error", err)
		return
	}
	defer func() { _ = spokeResp.Body.Close() }()
	if spokeResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(spokeResp.Body)
		s.logger.Warn("visibility spoke push rejected", "hive", id, "status", spokeResp.StatusCode, "body", string(respBody))
	}
}

// hiveConfigSSRFGuard is the SSRF check for handleProxyHiveConfig. It is a var
// (not a direct isPrivateURL call) solely so tests can override it to reach a
// loopback httptest server; production always uses isPrivateURL.
var hiveConfigSSRFGuard = isPrivateURL

func (s *HubServer) handleProxyHiveConfig(w http.ResponseWriter, r *http.Request) {
	hiveID := r.PathValue("hiveID")
	caller := s.getAuthUser(r)
	s.mu.RLock()
	var dashURL, owner string
	for _, h := range s.registry.Hives {
		if h.ID == hiveID && h.DashboardURL != "" {
			dashURL = h.DashboardURL
			owner = h.Owner
			break
		}
	}
	s.mu.RUnlock()
	if dashURL == "" {
		http.Error(w, `{"error":"hive not found or no dashboard URL"}`, http.StatusNotFound)
		return
	}
	// Ownership: a hive's config is private to its owner (and site admins). This
	// endpoint proxies a server-side fetch of a self-reported DashboardURL, so
	// without this any authenticated user could pull any hive's config.
	//
	// F9 (CWE-862): an OWNERLESS registry entry (owner == "") must NOT be treated
	// as world-readable. Previously the check was gated on `owner != ""`, so a
	// hive with no owner fell through and its raw config was fetchable by ANY
	// authenticated hub user. Fail closed: only the site admin may pull an
	// ownerless hive's config.
	if !isHubAdmin(caller) && (owner == "" || !canonicalEqual(caller, owner)) {
		http.Error(w, `{"error":"not authorized for this hive"}`, http.StatusForbidden)
		return
	}
	// SSRF guard: DashboardURL is self-reported by the spoke, so refuse to fetch
	// internal / link-local / private targets (e.g. 169.254.169.254 cloud
	// metadata, cluster-internal services). Uses the same guard as the public
	// registry (see registry rendering ~server.go:1626). Indirected through a
	// var so tests can point at a loopback httptest server.
	if hiveConfigSSRFGuard(r.Context(), dashURL) {
		http.Error(w, `{"error":"dashboard URL not permitted"}`, http.StatusForbidden)
		return
	}
	const proxyConfigTimeout = 10 * time.Second
	const maxConfigResponseBytes = 1 << 20
	client := &http.Client{
		Timeout: proxyConfigTimeout,
		// Do NOT follow redirects — a 30x could send us from a public
		// DashboardURL to an internal host, re-opening the SSRF the guard closes.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(strings.TrimRight(dashURL, "/") + "/api/config/download")
	if err != nil {
		slog.Warn("hive config proxy failed", "hiveID", hiveID, "error", err)
		http.Error(w, `{"error":"could not reach hive"}`, http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxConfigResponseBytes))
	w.Header().Set("Content-Type", "application/x-yaml")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// dibsRepoEntry is one hive-managed repo in the feed dibs's registry syncs
// from (GET /api/saas/dibs/repos, #4193). The field set and JSON names match
// dibs's pkg/registry RepoProfile contract exactly:
//
//	[{"repoID":"org/name","hiveID":"...","owner":"github-login","description":"..."}]
//
// Owner-editable dibs fields (topics, acceptingIdeas, appetite) are dibs-local
// state and deliberately absent here.
type dibsRepoEntry struct {
	RepoID      string `json:"repoID"`
	HiveID      string `json:"hiveID"`
	Owner       string `json:"owner"`
	Description string `json:"description,omitempty"`
	// ContributeURL is the hive's public /contribute page (ClankeR, the
	// contributor relay), built from the claimed vanity URL when present or
	// the heartbeat-reported dashboard URL otherwise (#4238). Empty when the
	// hive has reported no public base — dibs falls back to the hub's public
	// hive directory. Public info: the hub's own public-hives table renders
	// this same link to anonymous visitors.
	ContributeURL string `json:"contributeURL,omitempty"`
}

// handleDibsRepos lists the PUBLIC hive-managed repos for dibs's idea-matching
// registry (#4193, policy revised in #4233). Dibs polls this server-to-server
// with no browser session, so the endpoint is deliberately unauthenticated —
// which is safe only because every fact it returns is already public, and the
// inclusion rules below are what make that true. A hive contributes a repo
// only when ALL hold:
//
//   - the repo is PUBLIC: either the operator set is_public (the registry-
//     visibility opt-in, an immediate include with no API call), or the repo
//     is verifiably public on github.com right now — an unauthenticated
//     GET api.github.com/repos/{owner}/{repo} answering 200 with
//     "private": false. Verdicts are cached in memory with a TTL and checked
//     lazily in the background (dibs_public_check.go), so this handler never
//     blocks on the GitHub API: an unverified repo is excluded until its
//     verdict lands and the feed converges across dibs's 5-minute polls.
//     The opt-in-only policy this replaces could never populate the feed —
//     is_public is false on every production hive;
//   - it lives on PUBLIC github.com. github_host "" and "github.com" both
//     mean public GitHub (the sameGitHubHost normalization; production
//     records store the EXPLICIT "github.com" the spoke heartbeats, which the
//     original empty-only check wrongly excluded as GHE). A real GHE host is
//     excluded outright — an enterprise repo's very NAME can be confidential.
//     A cluster-level GHE default also excludes, unless the hive's own
//     github_host explicitly says github.com (the spoke-reported truth
//     outranks the cluster fallback);
//   - it is GitHub-family (not the gitlab/gitea forge adapters) and has a
//     real assigned identity: a non-placeholder org (the synthetic
//     "available-<id>" inventory org never names a repo) and at least one
//     repo recorded.
//
// owner is the hive owner's stable identity key — bare GitHub login for
// GitHub users, canonical provider:sub otherwise — byte-identical to the
// username /api/saas/whoami reports, so dibs can match a signed-in user to
// the repos they own.
//
// Cache-Control allows short shared caching: the answer is public, identical
// for every caller, and dibs re-syncs every ~5 minutes anyway.
func (s *HubServer) handleDibsRepos(w http.ResponseWriter, r *http.Request) {
	entries := []dibsRepoEntry{}
	seen := map[string]bool{}
	// Heartbeat-reported dashboard bases, snapshotted once so the hive loop
	// never holds the registry lock while doing per-repo work.
	dashByID := map[string]string{}
	s.mu.RLock()
	for i := range s.registry.Hives {
		if u := s.registry.Hives[i].DashboardURL; u != "" {
			dashByID[s.registry.Hives[i].ID] = u
		}
	}
	s.mu.RUnlock()
	for _, sh := range listSaaSHives() {
		// GitHub family only — the pkg/forge adapters (gitlab/gitea) never
		// point at github.com repos.
		if sh.Forge != "" && sh.Forge != "github" {
			continue
		}
		// "" and "github.com" both mean public GitHub; anything else is a
		// real GHE host and excludes the hive. Production meta.json records
		// carry the explicit "github.com" the spoke heartbeats (#4233).
		explicitPublicHost := sh.GitHubHost != "" && sameGitHubHost(sh.GitHubHost, publicGitHubHost)
		if sh.GitHubHost != "" && !explicitPublicHost {
			continue
		}
		// A GHE pin elsewhere in the resolution chain (hive-level
		// github_base_url, or the cluster default) also excludes — except
		// that an explicit github.com github_host outranks the CLUSTER
		// fallback: the host is spoke-reported truth, the cluster value only
		// a default for hives that never said.
		cluster := s.clusters[sh.ClusterID]
		if explicitPublicHost {
			cluster = ClusterConfig{}
		}
		if effectiveGitHubBaseURL(&sh, &cluster) != "" {
			continue
		}
		if sh.Org == "" || strings.HasPrefix(sh.Org, placeholderOrgPrefix) {
			continue
		}
		// Same stable-key normalization as handleSaaSWhoami, so the two
		// endpoints can never disagree about who a user is.
		owner := sh.Owner
		if provider, subject, ok := parseCanonical(canonicalizeLegacy(owner)); ok && provider == legacyProvider {
			owner = subject
		}
		// The hive's public contribute page: claimed vanity URL first (the
		// validated, user-facing host), else the heartbeat-reported dashboard
		// URL. Private/unset bases yield no link — same guard the hub's own
		// contribute proxy applies (findContributeHive).
		contributeURL := ""
		base := claimedVanityURL(&sh)
		if base == "" {
			base = dashByID[sh.ID]
		}
		if base != "" && !isPrivateURL(r.Context(), base) {
			contributeURL = strings.TrimRight(base, "/") + "/contribute"
		}
		for _, repo := range append([]string{sh.PrimaryRepo}, sh.Repos...) {
			if repo == "" {
				continue
			}
			// A primary_repo may already carry "owner/repo" (GHE/legacy
			// records do) — that pair IS the repo ID; otherwise the hive's
			// org is the owner half.
			repoID := repo
			if !strings.Contains(repo, "/") {
				repoID = sh.Org + "/" + repo
			}
			if seen[repoID] {
				continue
			}
			seen[repoID] = true
			// is_public stays an immediate include (the operator already
			// published the identity); everything else must be verifiably
			// public on github.com per the cached verdict (#4233). isPublic
			// never blocks — it answers from the cache and refreshes lazily.
			if !sh.IsPublic && !s.dibsChecker().isPublic(repoID) {
				continue
			}
			entries = append(entries, dibsRepoEntry{
				RepoID:        repoID,
				HiveID:        sh.ID,
				Owner:         owner,
				Description:   sh.ProjectName,
				ContributeURL: contributeURL,
			})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].RepoID < entries[j].RepoID })
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		s.logger.Warn("dibs repos: encoding response", "error", err)
	}
}
