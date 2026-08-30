package hub

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// alertProvStatusError is the SaaS provisioning status that means the provision
// failed outright. handleMyHives sets ProvStatus from the same value (and sets
// GovernorMode "ERROR" alongside it).
const alertProvStatusError = "error"

// Fleet alerts — "what needs a human RIGHT NOW".
//
// This is deliberately a different question from config drift (branch
// feat-config-drift), which answers "how does this hive's CONFIGURATION diverge
// from the fleet norm" (version behind, branch mismatch, ACMM 0, zero agents,
// App missing). Drift is about steady-state configuration; alerts are about
// OPERATIONAL conditions that are transient, time-bounded, and actionable:
// a hive that keeps restarting, one that has gone quiet, an upgrade that never
// finished, a named health check that is failing, an anomalous token burn, a
// provision that errored out.
//
// The two are designed to compose rather than compete: evaluateAlerts takes a
// []Alert of externally-sourced signals (see EvaluateFleetAlerts' driftAlerts
// parameter) and merges them through the same severity ordering,
// acknowledgement and counting pipeline. When drift lands, its signals become
// one more SOURCE feeding this model — no rule here needs to be rewritten, and
// nothing here recomputes a drift signal itself.

// alertAcksPath is the on-disk file where admin alert acknowledgements are
// persisted so a silenced alert stays silenced across a hub restart or upgrade.
// A var (not a const) so tests can point it at a temp dir.
var alertAcksPath = "/data/saas/hub-alert-acks.json"

// Alert severities, ordered most- to least-urgent. Kept as named constants so
// the evaluator, the sort order and the dashboard chips can never drift apart
// on a typo'd string literal.
const (
	// AlertSeverityCritical means the hive is not doing its job at all and a
	// human should look now (crash-looping, offline, provisioning errored).
	AlertSeverityCritical = "critical"
	// AlertSeverityWarning means the hive is degraded or stuck but may still be
	// making progress (failing health check, upgrade overrunning).
	AlertSeverityWarning = "warning"
	// AlertSeverityInfo means something is anomalous but not obviously broken
	// (token burn far off the fleet median).
	AlertSeverityInfo = "info"
)

// alertSeverityRank orders severities for sorting. Lower sorts first (more
// urgent first). An unknown severity sorts last rather than panicking.
var alertSeverityRank = map[string]int{
	AlertSeverityCritical: 0,
	AlertSeverityWarning:  1,
	AlertSeverityInfo:     2,
}

// alertSeverityRankUnknown is the sort rank given to a severity string the hub
// does not recognise, so such an alert lands at the end of the list instead of
// silently sorting as "critical".
const alertSeverityRankUnknown = 99

// Alert type identifiers. These are the keys the UI filters by, so they are
// constants shared between the evaluator and the rendered chips.
const (
	// AlertTypeCrashLoop — the spoke's reported process start time keeps moving
	// forward, i.e. the pod is restarting repeatedly rather than running.
	AlertTypeCrashLoop = "crash-loop"
	// AlertTypeOffline — no heartbeat for longer than alertOfflineThreshold.
	AlertTypeOffline = "offline"
	// AlertTypeStuckUpgrade — Upgrading has been true past staleUpgradeTimeout.
	AlertTypeStuckUpgrade = "stuck-upgrade"
	// AlertTypeFailedUpgrade — the spoke explicitly reported an upgrade it could
	// not complete. Distinct from stuck-upgrade: this one has a known cause the
	// spoke told us, so it is Critical and carries the underlying error.
	AlertTypeFailedUpgrade = "failed-upgrade"
	// AlertTypeHealthCheckFailing — a named check inside Health reports "fail".
	AlertTypeHealthCheckFailing = "health-check-failing"
	// AlertTypeTokenBurn — token consumption is anomalous versus the fleet
	// median (either far above it, or zero on a claimed, online hive).
	AlertTypeTokenBurn = "token-burn"
	// AlertTypeProvisionError — the SaaS provisioning record is in "error".
	AlertTypeProvisionError = "provision-error"
	// AlertTypeAdvisoryStale — the hive should be posting advisory digests but
	// its digest has quietly gone stale. The condition is NOT re-derived here:
	// it is the AdvisoryStale flag advisoryStale() already computed on read,
	// so the panel, the facet and the row pill can never disagree about which
	// hives are affected.
	AlertTypeAdvisoryStale = "advisory-stale"
	// AlertTypeURLUnreachable — the PUBLIC dashboard URL the hub links users to
	// did not serve. Distinct from every other alert here in that it is the only
	// one that can be measured either by the spoke itself or, for stale/offline
	// hives, from OUTSIDE the spoke.
	AlertTypeURLUnreachable = "url-unreachable"
	// AlertTypeURLPrivateNetwork means the hub's public-network probe could not
	// reach the dashboard URL, but the hive is still heartbeating and the spoke
	// either reports the URL healthy from its own network or is too old to send
	// that self-check. It is informational: this is a vantage-point mismatch, not
	// evidence that the hive's link is dead for its intended users.
	AlertTypeURLPrivateNetwork = "url-private-network"
	// AlertTypeAgentsInactive — one or more agents are RUNNING but not doing
	// any work: their tmux session has gone, they are sitting on a login
	// prompt, or they have produced nothing while work is queued. Deliberately
	// paused agents are excluded by the rule itself, never reported here.
	AlertTypeAgentsInactive = "agents-inactive"
	// AlertTypeInferenceAuthFailed — the hive's self-hosted inference backend
	// (LiteLLM/vLLM/llm-d gateway) is rejecting every call with 401, a stale or
	// invalid gateway key. This is the ROOT cause an operator wants to see: the
	// hive still heartbeats and looks online, but its agents are dead in the
	// water because no inference call succeeds. Distinct from advisory-stale
	// (which watches GitHub-POST ability, not inference) so the operator is
	// pointed at the bad KEY, not just told "the digest is stale". Raised
	// verbatim from the spoke-reported InferenceAuthError, which the spoke sets
	// only after several consecutive 401s and clears on the next success — so
	// the alert self-heals when the key is fixed.
	AlertTypeInferenceAuthFailed = "inference-auth-failed"
	// AlertTypeAppCredsUndelivered — a claimed hive requires GitHub App
	// auth but reports an OPERATOR-SIDE credential state (key-missing,
	// key-invalid or no-app-assigned; see operatorSideAppStates in journey.go).
	// The hive owner cannot see, supply or correct the App private key — it is
	// hub-distributed — so every owner-facing surface deliberately stands down
	// for these states (the spoke banner says "no action needed from you", the
	// journey never nudges, advisory staleness is suppressed). That made the
	// condition structurally silent: kelly-headwaters sat Degraded on
	// key-missing for 8 days with zero tokens ever consumed before an operator
	// happened to hover the fleet row (#4316). This alert is the ACTIVE signal
	// to the only actor who can fix it. Critical, because the hive provably
	// cannot work at all until the key lands. The reason names the cluster and
	// carries the exact remedy (PUT /api/saas/admin/cluster-app-keys/{id});
	// FirstSeen shows the operator how long the hive has been stranded. Clears
	// on its own once the key is delivered and the spoke reports a healthy
	// state.
	AlertTypeAppCredsUndelivered = "app-creds-undelivered"
)

// --- Thresholds. Every one is a named constant with a rationale. ---

const (
	// alertOfflineThreshold is how long a hive may go without a heartbeat
	// before it is alerted on. Deliberately well above maxHeartbeatAge (which
	// merely flips the Online flag) so a single missed beat or a brief pod
	// reschedule does not page anyone — this is "it has actually gone away".
	alertOfflineThreshold = 30 * time.Minute

	// alertCrashLoopUptimeCeiling is the process uptime below which a restart
	// is considered "recent". A hive whose uptime keeps resetting below this
	// is not settling.
	alertCrashLoopUptimeCeiling = 15 * time.Minute

	// alertCrashLoopMinRestarts is how many DISTINCT start times the hub must
	// have observed inside alertCrashLoopWindow before calling it a crash loop.
	// Two would fire on any single ordinary restart (old start + new start), so
	// three is the first count that actually means "it keeps happening".
	alertCrashLoopMinRestarts = 3

	// alertCrashLoopWindow is the trailing window over which restarts are
	// counted. Long enough to catch a slow crash loop (a pod dying every few
	// minutes), short enough that yesterday's incident does not still fire.
	alertCrashLoopWindow = 2 * time.Hour

	// alertTokenBurnMedianMultiple is how many times the fleet median 24h token
	// spend a hive must exceed before its burn is called anomalous. A runaway
	// agent typically lands an order of magnitude out; 5x is well clear of the
	// normal spread between a quiet hive and a busy one.
	alertTokenBurnMedianMultiple = 5

	// alertTokenBurnMinFleetSize is the smallest number of claimed, online
	// hives from which a median is meaningful. Below this the "median" is
	// dominated by one or two hives, so the burn rule is skipped entirely
	// rather than firing on noise.
	alertTokenBurnMinFleetSize = 5

	// alertTokenBurnMinMedian is the floor the fleet median must clear before
	// the multiple is applied. Without it, a fleet median of 1 token would make
	// any hive above 5 tokens "anomalous".
	alertTokenBurnMinMedian = int64(1000)

	// alertZeroTokenMinAge is how long a claimed hive must have been registered
	// before zero token spend is alerted on. A freshly assigned hive legitimately
	// has not spent anything yet; this gives it a full day to get going.
	alertZeroTokenMinAge = 24 * time.Hour

	// alertAckMaxAge bounds how long an acknowledgement is honored, regardless
	// of whether the condition persists. An alert silenced a week ago should
	// resurface — a permanent mute is how a real problem gets forgotten.
	alertAckMaxAge = 7 * 24 * time.Hour

	// alertStartTimesRetained caps how many distinct start times are kept per
	// hive. Only the trailing window matters, so this is a memory bound on a
	// hive that flaps continuously, not a tuning knob.
	alertStartTimesRetained = 16
)

// Alert is one condition on one hive that a human should act on.
type Alert struct {
	HiveID string `json:"hiveId"`
	// HiveName is carried so the panel can name the hive without the client
	// re-joining against the hive list.
	HiveName string `json:"hiveName,omitempty"`
	Type     string `json:"type"`
	Severity string `json:"severity"`
	// Reason is a human-readable sentence explaining what is wrong. It may
	// embed spoke-reported strings (a failing check name, a provisioning error)
	// and is therefore escaped with esc() on render — never interpolated raw.
	Reason string `json:"reason"`
	// FirstSeen is when the hub first observed this (hive, type) pair
	// continuously. It resets once the condition clears, so a re-occurrence
	// reads as new rather than inheriting the original timestamp.
	FirstSeen time.Time `json:"firstSeen"`
	// Acknowledged is true when an admin has silenced this alert and the
	// acknowledgement is still valid (see alertAckMaxAge).
	Acknowledged bool `json:"acknowledged,omitempty"`
	// AckBy / AckAt record who silenced it and when, so the panel can say so.
	// AckAt is a POINTER because `omitempty` does not elide a zero time.Time —
	// as a value it would serialise a bogus "0001-01-01T00:00:00Z" onto every
	// unacknowledged alert, which the client would have to special-case.
	AckBy string     `json:"ackBy,omitempty"`
	AckAt *time.Time `json:"ackAt,omitempty"`
	// AckByName is the resolved display label when AckBy is an opaque OIDC
	// identity with a stored display name. Serve-time cosmetic only; the ack
	// record itself keeps the raw identity.
	AckByName string `json:"ackByName,omitempty"`
}

// AlertSummary is the whole fleet's alert state, shaped for a single render
// with no follow-up round-trips: the alert list already sorted, counts by
// severity, and counts by type for the filter chips.
type AlertSummary struct {
	Alerts           []Alert        `json:"alerts"`
	CountsBySeverity map[string]int `json:"countsBySeverity"`
	CountsByType     map[string]int `json:"countsByType"`
	// Total counts only UNACKNOWLEDGED alerts — it is what decides whether the
	// panel is prominent or collapsed, and an acknowledged alert must not keep
	// the panel shouting.
	Total int `json:"total"`
	// AcknowledgedTotal is shown as a muted "N acknowledged" affordance so
	// silenced problems remain discoverable.
	AcknowledgedTotal int `json:"acknowledgedTotal"`
}

// alertAck is a persisted acknowledgement of one (hive, alert type) pair.
//
// ConditionFirstSeen is the FirstSeen of the alert instance that was
// acknowledged. It is the mechanism that makes an ack clear when the underlying
// condition resolves: FirstSeen resets whenever the condition goes away and
// comes back, so a re-occurrence carries a different FirstSeen and no longer
// matches the stored ack. Without it, silencing "offline" once would silence
// every future outage on that hive forever.
type alertAck struct {
	HiveID             string    `json:"hiveId"`
	Type               string    `json:"type"`
	By                 string    `json:"by"`
	At                 time.Time `json:"at"`
	ConditionFirstSeen time.Time `json:"conditionFirstSeen"`
}

// alertAckKeySep separates hive ID from alert type inside an alertAckKey. NUL
// because it can appear in neither a hive ID nor an alert type, so the key can
// always be split back apart unambiguously.
const alertAckKeySep = "\x00"

// alertAckKey is the map key for an acknowledgement: one per (hive, type).
func alertAckKey(hiveID, alertType string) string {
	return hiveID + alertAckKeySep + alertType
}

// alertAckKeyHive recovers the hive ID from an alertAckKey.
func alertAckKeyHive(key string) string {
	hiveID, _, _ := strings.Cut(key, alertAckKeySep)
	return hiveID
}

// alertState is the hub's observation memory for alert evaluation. Alerts are
// computed from a point-in-time registry snapshot, but two rules need history:
// crash-loop needs the sequence of observed start times, and FirstSeen needs to
// know when a condition began. This holds both.
//
// All fields are guarded by mu. mu is a leaf lock: nothing here calls back into
// HubServer, so it is never held while acquiring s.mu (a past incident
// deadlocked hub startup by re-locking a non-reentrant mutex from the launch
// path; the rule is that this lock is taken and released within one function).
type alertState struct {
	mu sync.Mutex
	// startTimes records the distinct StartedAt values observed per hive, with
	// the observation time. Key: hive ID.
	startTimes map[string][]alertStartObservation
	// firstSeen records when each (hive, type) condition was first observed
	// continuously. Key: alertAckKey(hiveID, type).
	firstSeen map[string]time.Time
	// acks holds acknowledgements. Key: alertAckKey(hiveID, type).
	acks map[string]alertAck
}

// alertStartObservation is one observed spoke process start time.
type alertStartObservation struct {
	// StartedAt is the spoke-reported process start (parsed from RFC3339).
	StartedAt time.Time `json:"startedAt"`
	// ObservedAt is when the hub first saw this start time. Restarts are
	// counted by ObservedAt so a spoke reporting a bogus clock cannot forge a
	// long history.
	ObservedAt time.Time `json:"observedAt"`
}

func newAlertState() *alertState {
	return &alertState{
		startTimes: make(map[string][]alertStartObservation),
		firstSeen:  make(map[string]time.Time),
		acks:       make(map[string]alertAck),
	}
}

// observeStart records a spoke-reported start time and returns how many
// distinct restarts have been observed inside alertCrashLoopWindow.
//
// Returns 0 for an unparseable or zero start time — an unknown uptime is not
// evidence of a crash loop.
func (a *alertState) observeStart(hiveID, startedAt string, now time.Time) int {
	if a == nil || hiveID == "" || strings.TrimSpace(startedAt) == "" {
		return 0
	}
	st, err := time.Parse(time.RFC3339, strings.TrimSpace(startedAt))
	if err != nil || st.IsZero() {
		return 0
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	obs := a.startTimes[hiveID]
	// Drop observations that have aged out of the counting window.
	kept := obs[:0]
	seen := false
	for _, o := range obs {
		if now.Sub(o.ObservedAt) > alertCrashLoopWindow {
			continue
		}
		if o.StartedAt.Equal(st) {
			seen = true
		}
		kept = append(kept, o)
	}
	if !seen {
		kept = append(kept, alertStartObservation{StartedAt: st, ObservedAt: now})
	}
	// Bound memory for a hive that flaps continuously: keep the most recent.
	if len(kept) > alertStartTimesRetained {
		kept = kept[len(kept)-alertStartTimesRetained:]
	}
	// Copy out of the shared backing array so a later append cannot alias it.
	final := make([]alertStartObservation, len(kept))
	copy(final, kept)
	a.startTimes[hiveID] = final
	return len(final)
}

// markCondition records that a condition is currently true and returns when it
// was first observed continuously. Repeated calls while the condition holds
// return the original timestamp.
func (a *alertState) markCondition(hiveID, alertType string, now time.Time) time.Time {
	key := alertAckKey(hiveID, alertType)
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.firstSeen[key]; ok && !t.IsZero() {
		return t
	}
	a.firstSeen[key] = now
	return now
}

// retainConditions drops firstSeen entries (and any acknowledgement attached to
// them) for conditions that are no longer firing. This is what makes an
// acknowledgement clear when the underlying problem resolves: once the
// condition goes away its FirstSeen is forgotten, so when it returns it gets a
// fresh FirstSeen that no stored ack matches.
//
// active is the set of currently-firing alertAckKey values. evaluated is the
// set of hive IDs that were actually PRESENT in the snapshot this evaluation
// ran over — and eviction is limited to those hives. That scoping is the fix
// for the bug that made Acknowledge fail with "no active alert of that type
// for this hive" on every click: evaluateAlerts runs against the CALLER's
// visible hives (handleMyHives scopes the list per user), but this state is
// shared hub-wide. A non-admin's dashboard poll, whose snapshot legitimately
// omits every hive it cannot see, used to evict the firstSeen entries (and
// stored acks) for ALL of those hives — so by the time the admin's ack POST
// arrived, the condition it targeted had been wiped from shared state and
// setAck refused it. A hive that is absent from the snapshot is no evidence
// its condition cleared; only a hive that was evaluated and did NOT fire is.
func (a *alertState) retainConditions(evaluated, active map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key := range a.firstSeen {
		if active[key] {
			continue
		}
		if !evaluated[alertAckKeyHive(key)] {
			// This hive was outside the evaluated snapshot (a scoped caller
			// cannot see it) — leave its state for an evaluation that can.
			continue
		}
		delete(a.firstSeen, key)
		delete(a.acks, key)
	}
}

// ackFor returns the acknowledgement for a (hive, type) pair if it is still
// valid: it must not have expired (alertAckMaxAge) and it must have been made
// against THIS occurrence of the condition (matching ConditionFirstSeen).
func (a *alertState) ackFor(hiveID, alertType string, firstSeen, now time.Time) (alertAck, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ack, ok := a.acks[alertAckKey(hiveID, alertType)]
	if !ok {
		return alertAck{}, false
	}
	if now.Sub(ack.At) > alertAckMaxAge {
		return alertAck{}, false
	}
	if !ack.ConditionFirstSeen.Equal(firstSeen) {
		// The condition cleared and came back — this ack belongs to the old
		// occurrence and must not silence the new one.
		return alertAck{}, false
	}
	return ack, true
}

// setAck stores an acknowledgement against the condition's current FirstSeen.
// Returns false when there is no such live condition to acknowledge, so a
// client cannot pre-silence an alert that has not fired.
func (a *alertState) setAck(hiveID, alertType, by string, now time.Time) bool {
	key := alertAckKey(hiveID, alertType)
	a.mu.Lock()
	defer a.mu.Unlock()
	fs, ok := a.firstSeen[key]
	if !ok || fs.IsZero() {
		return false
	}
	a.acks[key] = alertAck{
		HiveID:             hiveID,
		Type:               alertType,
		By:                 by,
		At:                 now,
		ConditionFirstSeen: fs,
	}
	return true
}

// clearAck removes an acknowledgement (the "un-silence" action).
func (a *alertState) clearAck(hiveID, alertType string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.acks, alertAckKey(hiveID, alertType))
}

// snapshotAcks copies the acknowledgement map for persistence.
func (a *alertState) snapshotAcks() map[string]alertAck {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]alertAck, len(a.acks))
	for k, v := range a.acks {
		out[k] = v
	}
	return out
}

// loadAcks replaces the acknowledgement map, dropping entries that have already
// expired. Returns true if anything was dropped, so the caller can rewrite the
// pruned file.
func (a *alertState) loadAcks(stored map[string]alertAck, now time.Time) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	pruned := false
	for k, v := range stored {
		if v.HiveID == "" || v.Type == "" {
			pruned = true
			continue
		}
		if now.Sub(v.At) > alertAckMaxAge {
			pruned = true
			continue
		}
		a.acks[k] = v
		// Restore the condition's FirstSeen too. Without this, the first
		// evaluation after a restart would mint a fresh FirstSeen that no
		// longer matches the stored ack, and every acknowledgement would be
		// silently voided by an ordinary hub upgrade.
		if !v.ConditionFirstSeen.IsZero() {
			a.firstSeen[k] = v.ConditionFirstSeen
		}
	}
	return pruned
}

// --- Evaluation ---

// alertHive is the minimal per-hive view the evaluator needs. It is built from
// MyHiveEntry so the evaluator can be unit-tested without constructing a whole
// HubServer, and so the rules never reach for fields they should not use.
type alertHive struct {
	ID   string
	Name string
	// IsPlaceholder marks an UNASSIGNED pool slot. Placeholders are excluded
	// from every rule that only makes sense for a CLAIMED hive (no tokens, no
	// GitHub App, ACMM 0, zero agents) — an idle pool slot legitimately has
	// none of those and flagging it would bury the real problems. Mirrors the
	// dashboard's isPlaceholderHive() exactly.
	IsPlaceholder    bool
	Online           bool
	LastHeartbeat    string
	StartedAt        string
	RegisteredAt     string
	Upgrading        bool
	UpgradeStartedAt time.Time
	UpgradeTarget    string
	UpgradeFailed    bool
	UpgradeError     string
	TotalTokens24h   int64
	Health           map[string]any
	ProvStatus       string
	ProvError        string
	// AdvisoryStale / AdvisoryStaleReason are carried through verbatim from
	// the entry rather than recomputed. advisoryStale() owns the threshold and
	// the advisory-mode + app-can-write gating; duplicating either here is
	// exactly how the panel and the row pill would start disagreeing.
	AdvisoryStale       bool
	AdvisoryStaleReason string

	// InactiveAgents / InactiveAgentsReason are carried through verbatim from
	// the entry rather than recomputed, exactly as the advisory pair above is:
	// evaluateInactiveAgents owns the thresholds and the paused/on-demand
	// gating, and duplicating either here is how the facet and the row pill
	// would start disagreeing about which hives are affected.
	InactiveAgents       int
	InactiveAgentsReason string

	// InferenceAuthError is the spoke-reported log-safe cause set when the
	// hive's inference backend is rejecting every call with 401 (a stale
	// gateway key). Carried verbatim from the entry: the spoke owns the
	// consecutive-failure threshold and the self-heal, so the hub just raises
	// the alert when it is non-empty and drops it when it clears. Never carries
	// key material.
	InferenceAuthError string

	// GitHubAppRequired / GitHubAppState are the spoke's own report of whether
	// its config demands App auth and, when App auth is failing, WHY (the
	// github.AppAuthState wire token). Carried verbatim: the spoke owns the
	// classification, and appStateIsOperatorSide (journey.go) owns which tokens
	// mean "only the hub operator can fix this". ClusterID names the cluster
	// whose key store is the remedy. ClusterHasKey is hub-observed fact — set
	// by fleetAlerts from the per-cluster PEM store, never by
	// alertHiveFromEntry — so the reason can say whether the fix is "upload a
	// key" (none exists) or "the delivered key is wrong / not arriving".
	GitHubAppRequired bool
	GitHubAppState    string
	ClusterID         string
	ClusterHasKey     bool
}

// alertHiveFromEntry projects a MyHiveEntry into the evaluator's view.
func alertHiveFromEntry(h MyHiveEntry) alertHive {
	return alertHive{
		ID:               h.ID,
		Name:             h.Name,
		IsPlaceholder:    isPlaceholderEntry(h),
		Online:           h.Online,
		LastHeartbeat:    h.LastHeartbeat,
		StartedAt:        h.StartedAt,
		RegisteredAt:     h.RegisteredAt,
		Upgrading:        h.Upgrading,
		UpgradeStartedAt: h.UpgradeStartedAt,
		UpgradeTarget:    h.UpgradeTarget,
		UpgradeFailed:    h.UpgradeFailed,
		UpgradeError:     h.UpgradeError,
		TotalTokens24h:   h.TotalTokens24h,
		Health:           h.Health,
		ProvStatus:       h.ProvStatus,
		ProvError:        h.ProvError,

		AdvisoryStale:       h.AdvisoryStale,
		AdvisoryStaleReason: h.AdvisoryStaleReason,

		InactiveAgents:       h.InactiveAgents,
		InactiveAgentsReason: h.InactiveAgentsReason,

		InferenceAuthError: h.InferenceAuthError,

		GitHubAppRequired: h.GitHubAppRequired,
		GitHubAppState:    h.GitHubAppState,
		ClusterID:         h.ClusterID,
		// ClusterHasKey is NOT set here: it is hub-observed (the PEM store on
		// disk), not entry-carried. fleetAlerts fills it in.
	}
}

// placeholderOrgPrefix is the org-name prefix a pre-provisioned pool slot
// carries before it is claimed.
const placeholderOrgPrefix = "available-"

// isPlaceholderEntry lives in drift.go — it is the Go mirror of the dashboard's
// isPlaceholderHive() and is shared by drift and alerts so the two can never
// disagree about what counts as a claimed hive.

// failingHealthChecks extracts the names of checks reporting "fail" from the
// spoke-reported Health map.
//
// Health arrives off the wire as map[string]any and its shape is NOT
// guaranteed. The shape the heartbeat actually carries today is built by the
// spoke's HealthSummary() (pkg/dashboard/server.go): "checks" is an ARRAY of
// {name, status, detail} objects, and that is the primary branch here — the
// hub dashboard's healthBadge() already reads it that way.
//
// A SECOND, different shape exists in the same spoke package:
// handleHealthDeep() builds "checks" as a map of name→object (with the "agents"
// check nested one level as a map of per-agent objects). That one serves
// /api/health/deep and is NOT what the hub receives — but the field is an
// untyped passthrough all the way into RegistryEntry.Health, so a spoke on a
// different version could send either. Both are handled, and anything else
// yields no names.
//
// Every access uses the two-value type assertion and every iteration is
// nil-guarded, so a malformed payload yields no names rather than a panic in
// the hub's request path.
//
// Returns names sorted, so the generated reason string is stable across polls
// (Go map iteration order is randomised, and an unstable Reason would make the
// alert look like it kept changing).
func failingHealthChecks(health map[string]any) []string {
	if health == nil {
		return nil
	}
	raw, ok := health["checks"]
	if !ok || raw == nil {
		return nil
	}

	var names []string
	// collect walks one check entry, recording its name if it reports "fail".
	// nested is true when this entry came from inside a parent check, so a
	// failing per-agent check is reported as "agents/scanner" rather than a
	// bare, ambiguous "scanner".
	var collect func(name string, v any, prefix string)
	collect = func(name string, v any, prefix string) {
		obj, ok := v.(map[string]any)
		if !ok {
			return
		}
		full := name
		if prefix != "" {
			full = prefix + "/" + name
		}
		if st, ok := obj["status"].(string); ok && st == healthCheckStatusFail {
			names = append(names, full)
			return
		}
		// No status of its own: it may be a container of sub-checks (the
		// "agents" check is a map of per-agent objects). Recurse one level.
		if _, hasStatus := obj["status"]; hasStatus {
			return
		}
		if prefix != "" {
			// Already one level deep — do not recurse unboundedly into
			// arbitrary spoke-supplied nesting.
			return
		}
		for subName, subV := range obj {
			collect(subName, subV, full)
		}
	}

	switch checks := raw.(type) {
	case []any:
		// The shape the heartbeat actually carries: each element names itself.
		for _, v := range checks {
			obj, ok := v.(map[string]any)
			if !ok {
				continue
			}
			st, ok := obj["status"].(string)
			if !ok || st != healthCheckStatusFail {
				continue
			}
			name, _ := obj["name"].(string)
			if name == "" {
				name = unnamedHealthCheck
			}
			names = append(names, name)
		}
	case map[string]any:
		// The /api/health/deep shape, handled defensively (see doc comment).
		for name, v := range checks {
			collect(name, v, "")
		}
	default:
		return nil
	}

	sort.Strings(names)
	return names
}

const (
	// healthCheckStatusFail is the status value the spoke's deep health
	// endpoint writes for a check that is failing ("pass"/"warn"/"skip" are the
	// others; only "fail" is alert-worthy).
	healthCheckStatusFail = "fail"
	// unnamedHealthCheck labels a failing check that arrived without a name, so
	// the reason string still says something rather than rendering blank.
	unnamedHealthCheck = "unnamed check"
	// maxNamedFailingChecks bounds how many check names are listed in a single
	// reason string. A hive with everything failing should not produce a
	// paragraph; the count still reflects the true total.
	maxNamedFailingChecks = 3

	// healthCheckGitHubAuth is the spoke's GitHub credential check
	// (pkg/dashboard/server.go builds it in both HealthSummary and
	// handleHealthDeep). It fails whenever no App or token is configured.
	healthCheckGitHubAuth = "github_auth"
	// healthCheckGitHubAppPrefix catches any GitHub-App-scoped check name a
	// spoke version may report (e.g. a future github_app_key). Every check in
	// that family verifies credentials a placeholder cannot have yet.
	healthCheckGitHubAppPrefix = "github_app"
)

// isAuthClassHealthCheck reports whether a named health check verifies GitHub
// credentials (App or token) rather than the spoke process itself. The split
// matters for placeholders: an UNASSIGNED pool slot has no GitHub auth BY
// DESIGN — credentials only arrive when a project is claimed — so an auth
// check failing on a placeholder is the pool working as designed, not a fault.
func isAuthClassHealthCheck(name string) bool {
	return name == healthCheckGitHubAuth ||
		strings.HasPrefix(name, healthCheckGitHubAppPrefix)
}

// withoutAuthClassChecks filters auth-class names out of a failing-checks list.
// Used only for placeholders; a claimed hive's list passes through untouched.
func withoutAuthClassChecks(names []string) []string {
	kept := names[:0:0]
	for _, n := range names {
		if !isAuthClassHealthCheck(n) {
			kept = append(kept, n)
		}
	}
	return kept
}

// appCredsUndeliveredReason builds the operator-facing reason for an
// app-creds-undelivered alert. It names the failing state, the cluster, and —
// critically — the exact remedy, because the upload endpoint was undocumented
// for long enough that a stranded hive sat unnoticed for 8 days (#4316). The
// string is deterministic for a given hive state, so the alert never appears
// to churn between polls. It never carries key material; ClusterHasKey and the
// state token are the only key-related facts referenced.
func appCredsUndeliveredReason(h alertHive) string {
	cluster := strings.TrimSpace(h.ClusterID)
	clusterLabel := cluster
	if clusterLabel == "" {
		clusterLabel = "its cluster"
	}
	pathID := cluster
	if pathID == "" {
		// No cluster reported (a spoke too old, or a registry gap): the remedy
		// still names the endpoint, with the placeholder the operator must
		// fill in.
		pathID = "{clusterID}"
	}
	remedy := "PUT /api/saas/admin/cluster-app-keys/" + pathID + " with the App's PEM private key"

	switch strings.TrimSpace(h.GitHubAppState) {
	case appStateKeyInvalidToken:
		return "GitHub App private key on this spoke does not match the App it authenticates as, so GitHub rejects every JWT it signs — " +
			"only an operator can fix this: replace the key for " + clusterLabel + " via " + remedy
	case appStateNoAppAssignedToken:
		return "No real GitHub App was ever assigned to this hive (it still carries the placeholder app_id) — " +
			"only an operator can fix this: assign the cluster's App and upload its key via " + remedy
	default: // appStateKeyMissingToken, and any future operator-side token.
		if h.ClusterHasKey {
			return "GitHub App private key has not reached this spoke even though the hub holds a key for " + clusterLabel + " — " +
				"check heartbeat delivery, or re-upload via " + remedy
		}
		return "GitHub App private key was never delivered: the hub holds no key for " + clusterLabel + " — " +
			"only an operator can fix this: upload it via " + remedy
	}
}

// fleetMedianTokens returns the median TotalTokens24h across claimed, online
// hives, and how many hives contributed. Placeholders are excluded — an idle
// pool slot at zero would drag the median down and make ordinary hives look
// like runaways.
func fleetMedianTokens(hives []alertHive) (int64, int) {
	var vals []int64
	for _, h := range hives {
		if h.IsPlaceholder || !h.Online {
			continue
		}
		vals = append(vals, h.TotalTokens24h)
	}
	if len(vals) == 0 {
		return 0, 0
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	mid := len(vals) / 2
	if len(vals)%2 == 1 {
		return vals[mid], len(vals)
	}
	// Even count: mean of the two middle values.
	return (vals[mid-1] + vals[mid]) / 2, len(vals)
}

// evaluateAlerts applies every alert rule to a fleet snapshot and returns the
// summary. driftAlerts carries alerts produced by another source (the
// config-drift evaluator, when it lands) — they are merged into the same
// ordering, acknowledgement and counting pipeline rather than recomputed here.
//
// now is injected so the rules are testable at boundary conditions.
func evaluateAlerts(state *alertState, hives []alertHive, driftAlerts []Alert, now time.Time) AlertSummary {
	summary := AlertSummary{
		CountsBySeverity: map[string]int{},
		CountsByType:     map[string]int{},
	}
	if state == nil {
		return summary
	}

	medianTokens, fleetSize := fleetMedianTokens(hives)

	var alerts []Alert
	// active tracks every condition firing this pass, so retainConditions can
	// forget (and un-acknowledge) the ones that have cleared.
	active := make(map[string]bool)
	// evaluated tracks which hives this snapshot actually contained, so
	// retention never evicts state for a hive a SCOPED caller simply could not
	// see (see retainConditions). Drift-sourced alerts count too: their hive
	// fired, so it was observed this pass.
	evaluated := make(map[string]bool, len(hives))

	add := func(hiveID, hiveName, alertType, severity, reason string) {
		firstSeen := state.markCondition(hiveID, alertType, now)
		active[alertAckKey(hiveID, alertType)] = true
		a := Alert{
			HiveID:    hiveID,
			HiveName:  hiveName,
			Type:      alertType,
			Severity:  severity,
			Reason:    reason,
			FirstSeen: firstSeen,
		}
		if ack, ok := state.ackFor(hiveID, alertType, firstSeen, now); ok {
			a.Acknowledged = true
			a.AckBy = ack.By
			ackAt := ack.At
			a.AckAt = &ackAt
		}
		alerts = append(alerts, a)
	}

	for _, h := range hives {
		if h.ID == "" {
			continue
		}
		evaluated[h.ID] = true

		// --- Rule: provisioning errored. Applies to placeholders too: a pool
		// slot that failed to provision is exactly the admin's problem. ---
		if h.ProvStatus == alertProvStatusError {
			reason := "Provisioning failed"
			if e := strings.TrimSpace(h.ProvError); e != "" {
				reason = "Provisioning failed: " + e
			}
			add(h.ID, h.Name, AlertTypeProvisionError, AlertSeverityCritical, reason)
		}

		// --- Rule: crash-looping. Distinct start times observed inside the
		// window, with the current uptime still below the ceiling (a hive that
		// restarted a few times and has now been up for hours has recovered
		// and should not keep alerting). ---
		restarts := state.observeStart(h.ID, h.StartedAt, now)
		if restarts >= alertCrashLoopMinRestarts && recentlyStarted(h.StartedAt, now) {
			add(h.ID, h.Name, AlertTypeCrashLoop, AlertSeverityCritical,
				"Restarted "+itoa(restarts)+" times in the last "+
					alertCrashLoopWindow.String()+" and has been up under "+
					alertCrashLoopUptimeCeiling.String())
		}

		// --- Rule: offline beyond the threshold. Checked for placeholders too:
		// a pool slot that has stopped heartbeating is genuinely broken, not
		// merely idle. ---
		if !h.Online {
			if age, ok := heartbeatAge(h.LastHeartbeat, now); ok && age > alertOfflineThreshold {
				add(h.ID, h.Name, AlertTypeOffline, AlertSeverityCritical,
					"No heartbeat for "+roundedDuration(age))
			}
		}

		// --- Rule: upgrade in flight past staleUpgradeTimeout. Reuses the
		// EXISTING constant rather than introducing a second, divergent notion
		// of "stuck upgrade". ---
		// --- Rule: the spoke told us the upgrade FAILED. Reported ahead of the
		// stuck-upgrade rule below because a known cause beats a timeout guess,
		// and Critical because it never resolves on its own. ---
		if h.UpgradeFailed {
			reason := "Upgrade failed"
			if t := strings.TrimSpace(h.UpgradeTarget); t != "" {
				reason += " (target " + t + ")"
			}
			if e := strings.TrimSpace(h.UpgradeError); e != "" {
				reason += ": " + e
			}
			add(h.ID, h.Name, AlertTypeFailedUpgrade, AlertSeverityCritical, reason)
		}

		if h.Upgrading && !h.UpgradeStartedAt.IsZero() {
			if age := now.Sub(h.UpgradeStartedAt); age > staleUpgradeTimeout {
				reason := "Upgrade in flight for " + roundedDuration(age) +
					" (limit " + staleUpgradeTimeout.String() + ")"
				if t := strings.TrimSpace(h.UpgradeTarget); t != "" {
					reason += ", target " + t
				}
				add(h.ID, h.Name, AlertTypeStuckUpgrade, AlertSeverityWarning, reason)
			}
		}

		// --- Rule: a named health check is failing. ---
		// Placeholders keep this rule — drift.go deliberately treats health as
		// NOT claimed-only, because a pool slot runs a real spoke process that
		// can genuinely be degraded — but auth-class checks are exempt for
		// them, mirroring drift's rule 2 ("never flag a placeholder for a
		// claimed-hive concern"): an unassigned slot has no GitHub auth by
		// design, and every live placeholder alerting github_auth was burying
		// a real alert under pool noise. A claimed hive's list is untouched.
		failing := failingHealthChecks(h.Health)
		if h.IsPlaceholder {
			failing = withoutAuthClassChecks(failing)
		}
		if len(failing) > 0 {
			shown := failing
			suffix := ""
			if len(shown) > maxNamedFailingChecks {
				shown = shown[:maxNamedFailingChecks]
				suffix = " (+" + itoa(len(failing)-maxNamedFailingChecks) + " more)"
			}
			noun := "check"
			if len(failing) != 1 {
				noun = "checks"
			}
			add(h.ID, h.Name, AlertTypeHealthCheckFailing, AlertSeverityWarning,
				itoa(len(failing))+" health "+noun+" failing: "+strings.Join(shown, ", ")+suffix)
		}

		// --- Rule: the advisory digest has gone stale. ---
		// The flag is precomputed by advisoryStale(), which already gates on
		// advisory-mode participation AND app-can-write, and already excludes
		// unknown timestamps — so there is nothing to re-check here beyond the
		// placeholder guard every claimed-hive rule applies. Severity is
		// warning, not critical: the hive is still running and doing work, but
		// the digest path a human relies on has quietly wedged.
		if !h.IsPlaceholder && h.AdvisoryStale {
			reason := h.AdvisoryStaleReason
			if reason == "" {
				// Only reached if a spoke reports the flag without a reason.
				// The server-supplied reason is preferred wherever it exists.
				reason = "Advisory digest has gone stale"
			}
			add(h.ID, h.Name, AlertTypeAdvisoryStale, AlertSeverityWarning, reason)
		}

		// --- Rule: the inference backend is failing auth. ---
		// The condition is NOT re-derived here: the spoke reports
		// InferenceAuthError only after several CONSECUTIVE 401s (a single blip
		// never trips it) and clears it on the next successful inference call,
		// so the alert self-heals with no threshold or window on the hub side to
		// keep in sync. Non-placeholder only — an unassigned pool slot routes no
		// inference. Critical because it is the ROOT cause of an otherwise
		// silent outage: the hive heartbeats and looks online while every agent
		// is dead in the water on a stale gateway key. This is the signal the
		// advisory-stale detector could not give (it watches GitHub-POST
		// ability, not inference), so an operator is pointed at the bad KEY.
		if !h.IsPlaceholder {
			if cause := strings.TrimSpace(h.InferenceAuthError); cause != "" {
				add(h.ID, h.Name, AlertTypeInferenceAuthFailed, AlertSeverityCritical, cause)
			}
		}

		// --- Rule: GitHub App credentials are in an operator-side state. ---
		// Claimed only: a placeholder has no App by design. Gated on the
		// spoke's OWN GitHubAppRequired verdict plus an operator-side
		// GitHubAppState (key-missing / key-invalid / no-app-assigned) — the
		// exact states every owner-facing surface deliberately silences, which
		// is why an operator alert is the ONLY active signal for them (#4316).
		//
		// Deliberately NOT gated on the hive being online (Danathar, #4322):
		// an offline hive raises its own offline alert, but that alert cannot
		// say WHY the hive never worked — and gating here would drop the
		// credential alert exactly when the hive is worst off, still keyless
		// and now not even heartbeating. The two answer different questions
		// and an operator needs both. This also matches every other
		// spoke-state rule above (health-check, advisory, inference), none of
		// which gate on Online.
		//
		// Critical: the hive provably cannot work until the hub delivers a
		// usable key. Self-clears once the key lands and the spoke stops
		// reporting the state; the standard retention pass drops the
		// condition and its ack.
		if !h.IsPlaceholder && h.GitHubAppRequired && appStateIsOperatorSide(h.GitHubAppState) {
			add(h.ID, h.Name, AlertTypeAppCredsUndelivered, AlertSeverityCritical,
				appCredsUndeliveredReason(h))
		}

		// --- Rule: agents are running but not working. ---
		// The condition is NOT re-derived here: it is precomputed by
		// evaluateInactiveAgents(), which already excludes paused and
		// on-demand agents, already requires queued work before calling an
		// agent idle, and already treats unknown timestamps as unknown. There
		// is nothing to re-check beyond the placeholder guard every
		// claimed-hive rule applies — an unclaimed pool slot has no agents.
		//
		// Warning, not critical: the hive is up and its other agents may be
		// working, but capacity the operator believes they have is doing
		// nothing.
		if !h.IsPlaceholder && h.InactiveAgents > 0 {
			reason := h.InactiveAgentsReason
			if reason == "" {
				// Only reached if the count arrives without a sentence.
				reason = "Agents are running but not working"
			}
			add(h.ID, h.Name, AlertTypeAgentsInactive, AlertSeverityWarning, reason)
		}

		// --- Rule: token burn anomaly. Claimed hives only — an unassigned
		// placeholder has no agents and therefore legitimately spends nothing. ---
		if !h.IsPlaceholder {
			if fleetSize >= alertTokenBurnMinFleetSize && medianTokens >= alertTokenBurnMinMedian &&
				h.TotalTokens24h > medianTokens*alertTokenBurnMedianMultiple {
				add(h.ID, h.Name, AlertTypeTokenBurn, AlertSeverityInfo,
					"Burned "+itoa64(h.TotalTokens24h)+" tokens in 24h, over "+
						itoa(alertTokenBurnMedianMultiple)+"x the fleet median of "+itoa64(medianTokens))
			} else if h.Online && h.TotalTokens24h <= 0 && registeredLongerThan(h.RegisteredAt, alertZeroTokenMinAge, now) {
				add(h.ID, h.Name, AlertTypeTokenBurn, AlertSeverityInfo,
					"Online but has spent no tokens in 24h")
			}
		}
	}

	// Merge externally-sourced alerts (config drift) through the same pipeline.
	for _, d := range driftAlerts {
		if d.HiveID == "" || d.Type == "" {
			continue
		}
		evaluated[d.HiveID] = true
		add(d.HiveID, d.HiveName, d.Type, d.Severity, d.Reason)
	}

	// Conditions that were evaluated this pass but did not fire are forgotten,
	// which also clears their acknowledgement so a re-occurrence is not
	// permanently silenced. Hives OUTSIDE this snapshot are untouched — a
	// scoped caller must never evict shared state for hives it cannot see.
	state.retainConditions(evaluated, active)

	// Most urgent first; within a severity, group by type then hive so the list
	// is stable across polls.
	sort.SliceStable(alerts, func(i, j int) bool {
		ri, rj := severityRank(alerts[i].Severity), severityRank(alerts[j].Severity)
		if ri != rj {
			return ri < rj
		}
		if alerts[i].Type != alerts[j].Type {
			return alerts[i].Type < alerts[j].Type
		}
		return alerts[i].HiveID < alerts[j].HiveID
	})

	for _, a := range alerts {
		if a.Acknowledged {
			summary.AcknowledgedTotal++
			continue
		}
		summary.CountsBySeverity[a.Severity]++
		summary.CountsByType[a.Type]++
		summary.Total++
	}
	summary.Alerts = alerts
	return summary
}

// severityRank orders a severity for sorting, with unknown values last.
func severityRank(s string) int {
	if r, ok := alertSeverityRank[s]; ok {
		return r
	}
	return alertSeverityRankUnknown
}

// recentlyStarted reports whether the spoke's process uptime is below
// alertCrashLoopUptimeCeiling. An unparseable or future start time returns
// false — unknown uptime is not evidence of a crash loop.
func recentlyStarted(startedAt string, now time.Time) bool {
	st, err := time.Parse(time.RFC3339, strings.TrimSpace(startedAt))
	if err != nil {
		return false
	}
	age := now.Sub(st)
	if age < 0 {
		return false
	}
	return age < alertCrashLoopUptimeCeiling
}

// heartbeatAge parses LastHeartbeat and returns its age. ok is false when the
// value is missing or unparseable — a hive that has never reported a heartbeat
// is not alerted on as "offline for N", because there is no N.
func heartbeatAge(lastHeartbeat string, now time.Time) (time.Duration, bool) {
	hb, err := time.Parse(time.RFC3339, strings.TrimSpace(lastHeartbeat))
	if err != nil {
		return 0, false
	}
	age := now.Sub(hb)
	if age < 0 {
		return 0, false
	}
	return age, true
}

// registeredLongerThan reports whether a hive was registered more than d ago.
// An unparseable RegisteredAt returns false, so a hive with unknown age is
// given the benefit of the doubt rather than alerted on immediately.
func registeredLongerThan(registeredAt string, d time.Duration, now time.Time) bool {
	rt, err := time.Parse(time.RFC3339, strings.TrimSpace(registeredAt))
	if err != nil {
		return false
	}
	return now.Sub(rt) > d
}

// roundedDuration formats a duration for a human-readable reason string,
// rounded to the minute so the text does not churn every poll.
func roundedDuration(d time.Duration) string {
	return d.Round(time.Minute).String()
}

// itoa / itoa64 avoid pulling strconv into every call site's readability.
func itoa(n int) string { return itoa64(int64(n)) }
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	var buf [20]byte
	i := len(buf)
	// Accumulate negatively so math.MinInt64 does not overflow on negation.
	if !neg {
		n = -n
	}
	for n != 0 {
		i--
		buf[i] = byte('0' - n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// --- HubServer integration ---

// fleetAlerts evaluates alerts for the hives the caller can see.
//
// It takes NO HubServer lock: the caller passes an already-copied snapshot, and
// alertState has its own leaf mutex. This keeps the evaluator off the s.mu
// critical path entirely.
func (s *HubServer) fleetAlerts(entries []MyHiveEntry) AlertSummary {
	if s == nil || s.alerts == nil {
		return AlertSummary{CountsBySeverity: map[string]int{}, CountsByType: map[string]int{}}
	}
	hives := make([]alertHive, 0, len(entries))
	for _, e := range entries {
		hives = append(hives, alertHiveFromEntry(e))
	}
	// ClusterHasKey is hub-observed fact (the per-cluster PEM store on disk),
	// so it is stamped here rather than in alertHiveFromEntry, which must stay
	// constructible in unit tests without a HubServer or a filesystem. One
	// read per DISTINCT cluster per evaluation, not per hive — a fleet shares
	// a handful of clusters.
	keyByCluster := map[string]bool{}
	for i := range hives {
		cid := strings.TrimSpace(hives[i].ClusterID)
		if cid == "" {
			continue
		}
		hasKey, seen := keyByCluster[cid]
		if !seen {
			hasKey = loadClusterAppKey(cid) != ""
			keyByCluster[cid] = hasKey
		}
		hives[i].ClusterHasKey = hasKey
	}
	// driftAlerts is nil today. When the config-drift evaluator lands, its
	// signals are passed here and flow through the same ordering, ack and
	// counting pipeline — see the file header.
	//
	// urlUnreachableAlerts is exactly such a signal: it is measured by the
	// auth-audit loop from OUTSIDE the spoke, so it cannot be re-derived from
	// the spoke-reported fields the evaluator sees.
	now := time.Now()
	regs := make([]RegistryEntry, 0, len(entries))
	for _, e := range entries {
		regs = append(regs, e.RegistryEntry)
	}
	summary := evaluateAlerts(s.alerts, hives, s.urlUnreachableAlerts(regs, now), now)
	// Cosmetic: resolve opaque OIDC ack identities to display names for the
	// "— ack by …" note. The stored ack keeps the raw identity.
	label := s.identityLabeler()
	for i := range summary.Alerts {
		if by := summary.Alerts[i].AckBy; by != "" {
			if l := label(by); l != by {
				summary.Alerts[i].AckByName = l
			}
		}
	}
	return summary
}

// alertAcksFileMu serialises writers of the acks file. Two concurrent ack
// POSTs used to run saveAlertAcks concurrently, and both wrote the SAME tmp
// path with os.WriteFile — the second open truncates while the first is
// mid-write, so the interleaved bytes that got renamed into place were not
// JSON. That is exactly the corruption the live hub logged ("failed to parse
// persisted alert acks: invalid character ..."), which then cost every
// persisted ack on the next restart.
var alertAcksFileMu sync.Mutex

// saveAlertAcks persists acknowledgements to the PVC so a silenced alert stays
// silenced across a hub restart or upgrade. Mirrors saveHubBanners: atomic
// tmp+rename, failures logged not fatal (a lost ack merely un-silences an
// alert, which is the safe direction).
func (s *HubServer) saveAlertAcks() {
	if s == nil || s.alerts == nil {
		return
	}
	alertAcksFileMu.Lock()
	defer alertAcksFileMu.Unlock()
	snapshot := s.alerts.snapshotAcks()
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		s.logger.Error("failed to marshal alert acks for persistence", "error", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(alertAcksPath), 0o755); err != nil {
		s.logger.Error("failed to create alert acks dir", "error", err)
		return
	}
	tmpPath := alertAcksPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		s.logger.Error("failed to write alert acks tmp file", "error", err)
		return
	}
	if err := os.Rename(tmpPath, alertAcksPath); err != nil {
		s.logger.Error("failed to rename alert acks file", "error", err)
	}
}

// alertAcksQuarantineSuffix is appended to alertAcksPath when the file on disk
// is not parseable, so the bad bytes are preserved for inspection instead of
// being silently overwritten — and so the ack system starts FRESH rather than
// staying wedged. Only the most recent corrupt file is kept: repeated
// corruption overwrites the previous quarantine rather than growing the PVC.
const alertAcksQuarantineSuffix = ".corrupt"

// loadAlertAcks reads persisted acknowledgements on startup, dropping expired
// ones and rewriting the file if any were pruned. A missing file is normal.
//
// A CORRUPT file must not disable acking: the stored acks are a convenience
// (they only re-silence alerts across a restart), so the recovery is to
// quarantine the bad file and continue with an empty ack store — the next ack
// writes a fresh, valid file. Before this, the parse failure left the corrupt
// bytes in place and the error re-logged on every boot.
func (s *HubServer) loadAlertAcks() {
	if s == nil || s.alerts == nil {
		return
	}
	data, err := os.ReadFile(alertAcksPath)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logger.Error("failed to read persisted alert acks", "error", err)
		}
		return
	}
	var stored map[string]alertAck
	if err := json.Unmarshal(data, &stored); err != nil {
		quarantine := alertAcksPath + alertAcksQuarantineSuffix
		if renameErr := os.Rename(alertAcksPath, quarantine); renameErr != nil {
			s.logger.Error("failed to quarantine corrupt alert acks file",
				"error", renameErr, "parseError", err)
			return
		}
		s.logger.Error("persisted alert acks were corrupt — quarantined and starting fresh",
			"error", err, "quarantined", quarantine)
		return
	}
	if s.alerts.loadAcks(stored, time.Now()) {
		s.saveAlertAcks()
	}
}

// alertAckRequest is the POST body for acknowledging or un-acknowledging.
type alertAckRequest struct {
	HiveID string `json:"hiveId"`
	Type   string `json:"type"`
	// Clear un-silences instead of silencing.
	Clear bool `json:"clear"`
}

// handleAlertAck acknowledges (or clears) one alert for one hive. Admin-only:
// silencing a fleet-wide signal is an operator action, not something a hive
// owner should be able to do to the admin's view.
//
// Failures go through writeJSONError, not http.Error. http.Error forces
// Content-Type: text/plain even when the body is JSON, so the dashboard's
// `await resp.json()` threw, its .catch() swallowed the reason, and the
// operator got a generic "Failed to update alert" that never said why. The
// reason is the whole point of the message when an ack silently does nothing.
func (s *HubServer) handleAlertAck(w http.ResponseWriter, r *http.Request) {
	var req alertAckRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPayloadBytes)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.HiveID = strings.TrimSpace(req.HiveID)
	req.Type = strings.TrimSpace(req.Type)
	if req.HiveID == "" || req.Type == "" {
		writeJSONError(w, http.StatusBadRequest, "hiveId and type are required")
		return
	}
	if !isKnownAlertType(req.Type) {
		writeJSONError(w, http.StatusBadRequest, "unknown alert type: "+req.Type)
		return
	}

	if req.Clear {
		s.alerts.clearAck(req.HiveID, req.Type)
		s.saveAlertAcks()
		writeAlertAckOK(w, false)
		return
	}

	// Acknowledging requires a LIVE condition. Refusing otherwise prevents a
	// client from pre-silencing an alert that has not fired yet.
	if !s.alerts.setAck(req.HiveID, req.Type, s.getAuthUser(r), time.Now()) {
		writeJSONError(w, http.StatusConflict, "no active alert of that type for this hive")
		return
	}
	s.saveAlertAcks()
	writeAlertAckOK(w, true)
}

func writeAlertAckOK(w http.ResponseWriter, acked bool) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "acknowledged": acked})
}

// knownAlertTypes is the closed set of types the ack endpoint accepts, so a
// client cannot wedge arbitrary keys into the persisted ack map.
var knownAlertTypes = map[string]bool{
	AlertTypeCrashLoop:    true,
	AlertTypeOffline:      true,
	AlertTypeStuckUpgrade: true,
	// AlertTypeFailedUpgrade was omitted when the type was introduced (#2308),
	// so acking a genuinely failed upgrade returned 400 and the row stayed in
	// the panel. Every AlertType* constant MUST appear here — see
	// TestKnownAlertTypesCoversEveryAlertType, which fails if one is added
	// without being registered.
	AlertTypeFailedUpgrade:       true,
	AlertTypeHealthCheckFailing:  true,
	AlertTypeTokenBurn:           true,
	AlertTypeProvisionError:      true,
	AlertTypeAdvisoryStale:       true,
	AlertTypeAgentsInactive:      true,
	AlertTypeURLUnreachable:      true,
	AlertTypeURLPrivateNetwork:   true,
	AlertTypeInferenceAuthFailed: true,
	AlertTypeAppCredsUndelivered: true,
}

func isKnownAlertType(t string) bool { return knownAlertTypes[t] }
