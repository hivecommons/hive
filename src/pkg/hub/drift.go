package hub

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Config-drift detection.
//
// Every incident this exists for was silent: the hub already RECEIVED the data
// that would have flagged it (a spoke reporting a branch nobody else runs, a
// GitHub App that was never installed, a health check failing for hours, an
// upgrade wedged since last week) but nothing in the UI ever compared a hive
// against its fleet or against itself a minute ago. Drift signals do that
// comparison server-side, once per My Hives request, and ride back on the same
// payload so the table can render them without a second call.
//
// Two rules shape the whole model:
//
//  1. Derive the norm, don't hardcode it. "Behind" and "wrong branch" are
//     relative to what the rest of the fleet is actually running (the modal
//     branch/version across online hives), so the model stays correct when the
//     fleet moves to a new branch without anyone editing this file.
//  2. Never flag a placeholder for a claimed-hive concern. An unassigned pool
//     slot legitimately has no GitHub App, no tokens, no agents and ACMM 0;
//     flagging those would bury the real signals under pool noise.

// DriftSeverity ranks a signal so a row can show its worst one and the fleet
// summary can sort by urgency.
type DriftSeverity string

const (
	// DriftInfo is a difference worth knowing but not acting on today
	// (e.g. a hive a couple of commits behind the fleet).
	DriftInfo DriftSeverity = "info"
	// DriftWarn is a real misconfiguration that degrades the hive but has not
	// stopped it (e.g. ACMM unset, no agents, a warning-level health check).
	DriftWarn DriftSeverity = "warn"
	// DriftCritical is a condition that makes the hive not work as configured
	// (e.g. offline while it should be up, App required but not installed,
	// pinned to an immutable tag so it can never upgrade).
	DriftCritical DriftSeverity = "critical"
)

// driftSeverityRank orders severities for "worst wins" comparisons. Higher is
// worse. Unknown severities rank lowest so an unrecognized value can never
// masquerade as critical.
var driftSeverityRank = map[DriftSeverity]int{
	DriftInfo:     1,
	DriftWarn:     2,
	DriftCritical: 3,
}

// Drift signal kinds. These are stable identifiers: the frontend groups the
// fleet-exceptions summary by them and uses them as filter keys, so renaming
// one is a breaking UI change.
const (
	DriftKindVersionBehind  = "version-behind"
	DriftKindBranchMismatch = "branch-mismatch"
	DriftKindPinnedImage    = "pinned-image"
	DriftKindHeartbeatStale = "heartbeat-stale"
	DriftKindAppMissing     = "app-missing"
	DriftKindAppPermIssue   = "app-perm-issue"
	// DriftKindAppCredsOperator marks a hive whose GitHub App credentials are
	// broken in a way only the hub operator can repair. Kept distinct from
	// app-missing/app-perm-issue so operator work is never filed as an
	// owner-facing adoption problem.
	DriftKindAppCredsOperator = "app-creds-operator"
	// DriftKindDuplicateSpoke: two spoke instances are alternating reports as
	// this one hive, so every registry field flips with whichever spoke
	// reported last.
	DriftKindDuplicateSpoke = "duplicate-spoke"
	// DriftKindStatusFlipping: the hive's reported status keeps alternating
	// between two values on a heartbeat cadence — the row cannot be trusted.
	DriftKindStatusFlipping = "status-flipping"
	// DriftKindVersionAbsent: the hive keeps heartbeating but reports no
	// git_hash, so the hub has nothing to compare against the branch target -
	// the row cannot be trusted AND no upgrade instruction is ever issued.
	DriftKindVersionAbsent = "version-absent"
	// DriftKindAppIDPlaceholder marks a hive still authenticating as the
	// placeholder App sentinel (config.PlaceholderAppID). Distinct from
	// app-creds-operator because the fault is the app_id itself, not the key:
	// the App does not exist, so no key and no installation_id can ever make it
	// work, and telling an operator to check credentials would repeat exactly
	// the misdiagnosis this signal exists to end.
	DriftKindAppIDPlaceholder = "app-id-placeholder"
	// DriftKindIdentitySplit marks a hive whose GitHub identity components
	// disagree with each other — most importantly a GHE app_id/app_slug with an
	// api_url that is empty or public, which authenticates as nothing and fails
	// every token request with "404 Integration not found".
	//
	// It is operator-side and critical: the hive was working, a push moved one
	// component of the identity without the others, and no owner action can fix
	// it. Distinct from app-missing (no App installed) and app-id-placeholder
	// (the App does not exist) because here the App is real and the credentials
	// are right — they are simply pointed at the wrong forge.
	DriftKindIdentitySplit  = "identity-split"
	DriftKindHealthDegraded = "health-degraded"
	DriftKindUpgradeStuck   = "upgrade-stuck"
	// DriftKindUpgrading marks a hive whose upgrade is ACTIVELY in progress:
	// Upgrading set, a real (non-zero) start stamp within staleUpgradeTimeout,
	// and no terminal failure recorded. Informational by design — it is status,
	// not alarm. The pill exists so an operator can separate "being fixed right
	// now" from "needs attention": during an upgrade wave, mid-upgrade hives
	// otherwise pollute the version-differs breakdown with drift that is the
	// expected state. Complementary to upgrade-stuck — past staleUpgradeTimeout
	// (or with no usable stamp) that signal takes over, so a hive can never
	// show as harmlessly "Upgrading" forever off a zero or stale timestamp
	// (the same bound #2517 made the elapsed counter and orphan sweep honor).
	DriftKindUpgrading = "upgrading"
	DriftKindACMMUnset = "acmm-unset"
	DriftKindNoAgents  = "no-agents"
)

// DriftSignal is one detected deviation. Reason is a complete, human-readable
// sentence — the UI renders it verbatim, so it must carry the specifics (which
// branch, which check, how long) rather than deferring to a lookup table.
type DriftSignal struct {
	Kind     string        `json:"kind"`
	Severity DriftSeverity `json:"severity"`
	Reason   string        `json:"reason"`
	// FirstSeen is when the hub first observed this (hive, kind) signal,
	// RFC3339. It is stable while the signal persists across recomputes and
	// resets only after the signal clears, so the hover can say "since 3:42 PM"
	// instead of drifting to "now" on every beat. Empty on reports the tracker
	// has not stamped (defensive: a caller that builds a DriftReport directly).
	FirstSeen string `json:"firstSeen,omitempty"`
}

// DriftReport is the per-hive drift result attached to a My Hives row.
// WorstSeverity is empty exactly when Signals is empty.
type DriftReport struct {
	Signals       []DriftSignal `json:"signals,omitempty"`
	Count         int           `json:"count"`
	WorstSeverity DriftSeverity `json:"worstSeverity,omitempty"`
}

// fleetNorm is the derived "what the fleet is running" baseline that relative
// signals compare against. Empty fields mean "no norm could be established"
// (too few online hives, or no clear majority) — in which case the relative
// signals are skipped entirely rather than guessed at.
type fleetNorm struct {
	// Branch is the modal git branch across eligible hives.
	Branch string
	// SHA is the modal git hash among hives ON the norm branch. Scoped to the
	// branch because a v3 hive's SHA is not evidence about v2's norm.
	SHA string
	// Eligible is how many hives contributed to the norm. Reported so callers
	// can require a minimum sample before trusting it.
	Eligible int
}

// minFleetNormSample is the smallest number of eligible hives from which a
// fleet norm is derived. With fewer than this, "the majority" is not a
// meaningful statement — a two-hive fleet where the hives disagree would flag
// one of them arbitrarily — so relative signals (branch/version drift) are
// suppressed instead of guessed.
const minFleetNormSample = 3

// driftHeartbeatStaleAfter is how old the last heartbeat may be before the hub
// treats the hive as not reporting. It matches maxHeartbeatAge, which is the
// same threshold markStaleHives uses to clear Online — deriving it keeps the
// drift signal and the online dot from ever disagreeing.
const driftHeartbeatStaleAfter = maxHeartbeatAge

// driftACMMUnsetLevel is the ACMM level value meaning "never configured". A
// claimed hive at this level is running with no maturity target set.
const driftACMMUnsetLevel = 0

// driftNoAgentsCount is the agent count meaning "nothing is running". A claimed
// hive here consumes a slot and does no work.
const driftNoAgentsCount = 0

// modalString returns the most common non-empty value in vals and how many
// hives held it. Ties break on the lexically smaller value so the norm is
// deterministic across requests (a norm that flickered between two equally
// popular branches would flag half the fleet on alternating page loads).
func modalString(vals []string) (string, int) {
	counts := make(map[string]int, len(vals))
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		counts[v]++
	}
	if len(counts) == 0 {
		return "", 0
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	best, bestN := "", 0
	for _, k := range keys {
		if counts[k] > bestN {
			best, bestN = k, counts[k]
		}
	}
	return best, bestN
}

// driftEligibleForNorm reports whether a hive's self-reported version should
// count toward the fleet norm. Only ONLINE, non-placeholder hives vote: an
// offline hive's last-known branch may be months stale, and a placeholder runs
// whatever the pool image happens to be, so neither is evidence about what the
// fleet is meant to be running.
func driftEligibleForNorm(h MyHiveEntry) bool {
	return h.Online && !isPlaceholderEntry(h)
}

// isPlaceholderEntry mirrors the frontend's isPlaceholderHive: provStatus
// "available" is authoritative, with an "available-" org prefix as the fallback
// for placeholders that have not reported provStatus yet. Kept in lockstep with
// the JS so server-computed drift and client-rendered sections can never
// disagree about what counts as a claimed hive.
//
// An assigned slot short-circuits to false before the prefix fallback runs: Org
// is spoke-reported and still reads "available-<id>" until the spoke adopts the
// pushed config, so the fallback alone would keep calling a freshly-approved
// hive inventory for that whole window. ProvStatus and AssignedUnclaimed are
// both meta-derived and flip the moment the approval is recorded.
func isPlaceholderEntry(h MyHiveEntry) bool {
	if h.ProvStatus == statusAvailable {
		return true
	}
	if h.ProvStatus == statusAssigned || h.AssignedUnclaimed {
		return false
	}
	return strings.HasPrefix(h.Org, placeholderOrgPrefix)
}

// computeFleetNorm derives the baseline from the hives the caller can see.
// Returns a zero norm when the eligible sample is too small to be meaningful.
func computeFleetNorm(hives []MyHiveEntry) fleetNorm {
	branches := make([]string, 0, len(hives))
	for _, h := range hives {
		if !driftEligibleForNorm(h) {
			continue
		}
		branches = append(branches, h.GitBranch)
	}
	if len(branches) < minFleetNormSample {
		return fleetNorm{}
	}
	branch, _ := modalString(branches)
	if branch == "" {
		return fleetNorm{}
	}
	// Scope the SHA norm to hives on the norm branch — a hive on another
	// branch has a legitimately different SHA and must not drag the modal.
	shas := make([]string, 0, len(hives))
	for _, h := range hives {
		if !driftEligibleForNorm(h) || !strings.EqualFold(h.GitBranch, branch) {
			continue
		}
		shas = append(shas, shortSHA(h.GitHash))
	}
	sha, _ := modalString(shas)
	return fleetNorm{Branch: branch, SHA: sha, Eligible: len(branches)}
}

// driftHumanDuration renders a duration for a reason string at the coarsest
// unit that still reads precisely ("3 min", "4 hours", "2 days"). Sub-minute
// durations round up to "1 min" — no drift reason is ever about seconds.
func driftHumanDuration(d time.Duration) string {
	const hoursPerDay = 24
	if d < time.Hour {
		mins := int(d.Minutes())
		if mins < 1 {
			mins = 1
		}
		return pluralize(mins, "min", "min")
	}
	if d < hoursPerDay*time.Hour {
		return pluralize(int(d.Hours()), "hour", "hours")
	}
	return pluralize(int(d.Hours())/hoursPerDay, "day", "days")
}

func pluralize(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// parseRFC3339 parses a timestamp, reporting ok=false for empty or malformed
// input so callers skip the signal rather than computing an age from the zero
// time (which would make every hive look decades stale).
func parseRFC3339(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// imageTagOf extracts the tag from an image reference
// ("ghcr.io/hivecommons/hive:v2-latest" → "v2-latest"). Returns "" when the ref
// carries no tag or is a digest pin, since neither is a tag-drift question.
// The registry host may itself contain a ':' port, so only a colon AFTER the
// last '/' delimits a tag.
func imageTagOf(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	// A digest pin ("...@sha256:...") has no mutable tag to speak of.
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash {
		return ""
	}
	return ref[colon+1:]
}

// imageRefIsPinned reports whether an image reference is DEFINITIVELY pinned to
// something a rolling upgrade can never advance.
//
// This is deliberately not the same question as imageTagIsMutable. That
// predicate asks "can a restart pick up new code?", and its safe answer for
// anything it cannot parse is false — the caller then takes the harmless
// image-patch path. The drift rule asks something stronger: "do I have positive
// evidence this hive is pinned, enough to page a human with a CRITICAL badge?"
// An unparseable ref is not that evidence.
//
// The distinction is not academic. A ref that lost its tag separator in transit
// (the sanitizer used to strip ':') is a MALFORMED value, not a legitimate SHA
// pin — yet the old rule, built directly on !imageTagIsMutable, read eleven
// healthy "-latest" hives as critically pinned. So parse failure degrades to
// silence here, exactly as an empty ref already did. A future mangling of this
// field can then only ever cost the hub a signal, never invent a false one.
//
// Returns false (no signal) for: an empty ref, and a ref with no tag separator
// at all — both are "unknown", not "pinned".
func imageRefIsPinned(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false // unknown image: old spoke, or not running in-cluster
	}
	// A digest pin ("...@sha256:...") is unambiguous positive evidence.
	if strings.Contains(ref, "@") {
		return true
	}
	// Otherwise a tag must be parseable before any claim can be made. Only a
	// colon AFTER the last '/' is a tag; an earlier one is a registry port.
	if imageTagOf(ref) == "" {
		return false // malformed or untagged — unknown, so stay silent
	}
	return !imageTagIsMutable(ref)
}

// healthFailingChecks returns the names of checks that are not passing, with
// their status, in the order the spoke reported them. Every access is guarded:
// Health is a free-form map[string]any decoded from spoke JSON, so any level of
// it can be a different type (or missing) on an old or misbehaving spoke, and a
// type assertion that panicked here would take down the whole My Hives request.
func healthFailingChecks(health map[string]any) []string {
	if health == nil {
		return nil
	}
	raw, ok := health["checks"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range raw {
		ck, ok := item.(map[string]any)
		if !ok {
			continue
		}
		status, _ := ck["status"].(string)
		if status == "" || status == "pass" || status == "skip" {
			continue
		}
		// Health rides the heartbeat as a free-form map and is stored
		// unsanitized, so the strings composed into a hub-rendered reason are
		// neutralized here — with the PROSE sanitizer, because a check detail
		// is a sentence and the identifier sanitizer would strip its spaces.
		status = sanitizeProseField(status)
		name, _ := ck["name"].(string)
		name = sanitizeProseField(name)
		if name == "" {
			name = "unnamed check"
		}
		detail, _ := ck["detail"].(string)
		detail = sanitizeProseField(detail)
		if detail != "" {
			out = append(out, fmt.Sprintf("%s (%s: %s)", name, status, detail))
			continue
		}
		out = append(out, fmt.Sprintf("%s (%s)", name, status))
	}
	return out
}

// healthStatusOf reads health.status, defaulting to "unknown".
func healthStatusOf(health map[string]any) string {
	if health == nil {
		return "unknown"
	}
	st, ok := health["status"].(string)
	if !ok || st == "" {
		return "unknown"
	}
	return st
}

// maxFailingChecksInReason bounds how many failing check names a single reason
// string names before summarizing the rest. A hive with a dozen failures would
// otherwise produce a reason too long to read in a hover panel.
const maxFailingChecksInReason = 3

// computeDrift builds the drift report for one hive against a fleet norm and a
// branch→latest-SHA map (the same map the My Hives payload already ships as
// latest_shas, so no new data source is introduced).
//
// now is passed in rather than read from the clock so every hive in one request
// is evaluated against the same instant and tests are deterministic.
func computeDrift(h MyHiveEntry, norm fleetNorm, latestSHAs map[string]string, now time.Time) DriftReport {
	var signals []DriftSignal
	add := func(kind string, sev DriftSeverity, reason string) {
		signals = append(signals, DriftSignal{Kind: kind, Severity: sev, Reason: reason})
	}

	placeholder := isPlaceholderEntry(h)

	// --- Signals that apply to every hive, claimed or placeholder ----------

	// Heartbeat stale / offline. Only meaningful once the hive has beaten at
	// least once: a slot that has never reported is being provisioned, not
	// drifting.
	if last, ok := parseRFC3339(h.LastHeartbeat); ok {
		if age := now.Sub(last); age > driftHeartbeatStaleAfter {
			add(DriftKindHeartbeatStale, DriftCritical,
				fmt.Sprintf("No heartbeat for %s — the hub has no live reading for this hive", driftHumanDuration(age)))
		}
	}

	// Upgrade wedged. Reuses staleUpgradeTimeout, the same bound the hub's own
	// upgrade-state machine uses to call an in-flight upgrade stuck, so the
	// drift badge and the row's upgrade spinner agree on when to give up.
	//
	// activelyUpgrading is true only in the healthy remainder: a real start
	// stamp, within the timeout, and no recorded failure. It raises the
	// informational "upgrading" signal and suppresses the fleet-relative
	// version/branch signals below — mid-upgrade drift from the fleet is the
	// expected state, not a deviation. The zero-stamp and past-timeout arms
	// keep firing upgrade-stuck instead, so this signal can never pin a hive
	// as "Upgrading" forever off a lost or stale timestamp (#2517).
	activelyUpgrading := false
	if h.Upgrading {
		started, ok := parseRFC3339OrTime(h.UpgradeStartedAt, "")
		if !ok || started.IsZero() {
			add(DriftKindUpgradeStuck, DriftWarn,
				"Upgrade in flight with no recorded start time — the hub cannot tell how long it has been running")
		} else if age := now.Sub(started); age > staleUpgradeTimeout {
			target := h.UpgradeTarget
			if target == "" {
				target = "an unrecorded target"
			}
			add(DriftKindUpgradeStuck, DriftCritical,
				fmt.Sprintf("Upgrade to %s has been in flight for %s (limit %s) — it is stuck, not progressing",
					target, driftHumanDuration(age), driftHumanDuration(staleUpgradeTimeout)))
		} else if !h.UpgradeFailed && h.UpgradeFailedAt.IsZero() {
			// A reported failure is terminal, not in-flight (the heartbeat
			// handler clears Upgrading when a spoke reports one), so a row
			// carrying failure state is defensively excluded here rather than
			// shown as making progress it is not making.
			activelyUpgrading = true
			target := h.UpgradeTarget
			if target == "" {
				target = "an unrecorded target"
			}
			add(DriftKindUpgrading, DriftInfo,
				fmt.Sprintf("Upgrade to %s has been in flight for %s — version drift from the fleet is expected until it lands (called stuck after %s)",
					target, driftHumanDuration(age), driftHumanDuration(staleUpgradeTimeout)))
		}
	}

	// Image pinned to an immutable tag. The spoke reports its own Deployment's
	// image over the heartbeat; a tag that is not "<branch>-latest" can never
	// receive a rolling upgrade, which is how one spoke sat on hive:63d8902
	// restart-looping while the fleet moved on.
	//
	// imageRefIsPinned — not a bare !imageTagIsMutable — because raising a
	// CRITICAL badge demands positive evidence of a pin. An empty or
	// unparseable ref is "unknown", and unknown must be silent: that is the
	// difference between reporting a real pin and telling an operator eleven
	// healthy rolling hives can never be upgraded. A genuine pin (SHA tag,
	// release tag, or digest) still fires, and still fires critical.
	if imageRefIsPinned(h.ImageRef) {
		label := imageTagOf(h.ImageRef)
		if label == "" {
			label = h.ImageRef
		}
		add(DriftKindPinnedImage, DriftCritical,
			fmt.Sprintf("Image %q is not a rolling tag — this hive can never receive a rolling upgrade (expected a *%s tag)",
				label, mutableTagSuffix))
	}

	// Health. Placeholders run a real spoke process and can genuinely be
	// degraded, so this is not claimed-only.
	switch st := healthStatusOf(h.Health); st {
	case "degraded", "critical":
		failing := healthFailingChecks(h.Health)
		sev := DriftWarn
		if st == "critical" {
			sev = DriftCritical
		}
		if len(failing) == 0 {
			add(DriftKindHealthDegraded, sev,
				fmt.Sprintf("Health is %s but the spoke reported no failing check — the reason is not visible to the hub", st))
			break
		}
		named := failing
		suffix := ""
		if len(named) > maxFailingChecksInReason {
			extra := len(named) - maxFailingChecksInReason
			named = named[:maxFailingChecksInReason]
			noun := "checks"
			if extra == 1 {
				noun = "check"
			}
			suffix = fmt.Sprintf(" and %d more %s", extra, noun)
		}
		add(DriftKindHealthDegraded, sev,
			fmt.Sprintf("Health is %s: %s%s", st, strings.Join(named, ", "), suffix))
	}

	// --- Claimed-hive-only signals ---------------------------------------
	//
	// Placeholder app_id sentinel. Raised for CLAIMED hives only — an unassigned
	// slot carrying the sentinel is the pool working as designed, and flagging
	// all of them would bury the one hive an operator must actually fix (five of
	// the six live sentinel hives are unassigned).
	//
	// Deliberately NOT gated on GitHubAppRequired: that flag is the spoke's own
	// verdict, and a spoke too old — or too wedged — to classify its failure
	// never sets it. The app_id is a raw number the spoke reports regardless, so
	// this fires on hub-observed fact rather than on the spoke agreeing that it
	// is broken. That is the difference between the fault being visible and the
	// weeks of silence that made this bug expensive.
	if !placeholder && h.GitHubAppID == config.PlaceholderAppID {
		add(DriftKindAppIDPlaceholder, DriftCritical,
			"This hive is authenticating as the placeholder App ID, which is not a real GitHub App — "+
				"no installation ID or private key can make it work. The hub must assign the cluster's real "+
				"github_app_id; the hive owner cannot fix this and should not be asked to")
	}

	// Two instances alternating as one hive poisons every other signal in
	// this entry — each drift row below reflects whichever instance reported
	// last, and the dashboard flips with them. Raised first and always (even
	// on a placeholder): the fix is shedding the stale instance, and the hub
	// has a delivered path for that (Restart Spoke arms RolloutRestartSelf on
	// every instance via the heartbeat).
	if h.ConflictingReporters != "" {
		add(DriftKindDuplicateSpoke, DriftCritical,
			"two spoke instances are reporting as this hive ("+h.ConflictingReporters+
				") and their states alternate every beat — use Restart Spoke to shed the stale instance")
	} else if h.StatusFlipping {
		// Suppressed when duplicate-spoke already fired: that signal carries
		// the same fact PLUS the culprit pod names, and two rows for one
		// oscillation would double-count the hive in the summary strip.
		add(DriftKindStatusFlipping, DriftWarn,
			"reported status keeps flipping between two values on a heartbeat cadence — "+
				"usually two spoke instances alternating as this hive (a spoke too old to report "+
				"its pod name cannot be told apart) or an auth check oscillating; "+
				"use Restart Spoke, or upgrade the spoke so the instances identify themselves")
	}

	// Heartbeats arriving with no version. The hub decides whether to instruct
	// an upgrade by comparing the reported git_hash against the branch target;
	// with no hash there is nothing to compare, so it instructs nothing and the
	// hive freezes at whatever build it runs while still counting as online.
	// Nothing else on any operator surface distinguishes that from health.
	//
	// Skipped for offline hives and placeholders for the same reason the
	// fleet-relative signals below skip them: an offline hive is already
	// flagged as not reporting, so "its beats carry no version" is a
	// restatement of silence rather than a second fault, and a placeholder is
	// pool inventory whose version nobody is upgrading toward anything.
	if h.VersionAbsent && h.Online && !placeholder {
		add(DriftKindVersionAbsent, DriftCritical,
			fmt.Sprintf("Heartbeats are arriving but carry no version: the last %d beats reported an empty git_hash. "+
				"The hub compares that hash against the branch target to decide on an upgrade, so it is sending this "+
				"hive no upgrade instruction at all - the row cannot be trusted and the hive is not being upgraded. "+
				"Check the spoke for stats-collection timeouts, then restart it",
				versionAbsentBeatsToConfirm))
	}

	// A placeholder legitimately has no App, no agents and ACMM 0. Flagging it
	// for those would make the pool dominate the exceptions summary and hide
	// the hives a human can actually fix.
	if !placeholder {
		if h.GitHubAppRequired {
			// Operator-side first: a hive whose App key we never delivered (or
			// delivered wrong) is not an owner-facing "installed but under-
			// permissioned" problem, and must not be described as one. It is
			// still critical drift — the hive cannot work — but the text has to
			// point at the operator so nobody chases the owner about an
			// installation that is already correct.
			if appStateIsOperatorSide(h.GitHubAppState) {
				detail := "the App private key has not been delivered to this spoke"
				if strings.TrimSpace(h.GitHubAppState) == appStateKeyInvalidToken {
					detail = "the App private key on this spoke does not match the App it authenticates as, so GitHub rejects its JWT"
				}
				add(DriftKindAppCredsOperator, DriftCritical,
					"GitHub App credentials are not valid and only an operator can fix it: "+detail+
						" — the hive owner cannot supply or correct this key")
			} else if h.GitHubAppPermIssue != "" {
				add(DriftKindAppPermIssue, DriftCritical,
					fmt.Sprintf("GitHub App is installed but its permissions are insufficient: %s", h.GitHubAppPermIssue))
			} else if h.GitHubAppID != config.PlaceholderAppID {
				add(DriftKindAppMissing, DriftCritical,
					"GitHub App is required by this hive but is not installed — agents cannot act on the repo")
			}
			// When the app_id IS the sentinel, "App not installed" is the exact
			// misdiagnosis that sent the owner to correct an installation ID that
			// was already right. app-id-placeholder above already says the true
			// cause, so this row is suppressed rather than duplicated.
		}
		// A split GitHub identity: the components disagree about which forge
		// they name. Raised on the spoke's OWN report of the whole set, which
		// is only possible now that app_slug/api_url/base_url are reported
		// back — with app_id alone, a half-applied identity was
		// indistinguishable from a healthy one.
		for _, issue := range IdentitySetIssues(IdentitySet{
			AppID:          h.GitHubAppID,
			AppSlug:        h.GitHubAppSlug,
			InstallationID: h.GitHubInstallationID,
			APIURL:         h.GitHubAPIURL,
			BaseURL:        h.GitHubBaseURL,
		}) {
			add(DriftKindIdentitySplit, DriftCritical,
				"GitHub identity is half-applied and only an operator can fix it: "+issue)
		}
		if h.ACMMLevel <= driftACMMUnsetLevel {
			add(DriftKindACMMUnset, DriftWarn,
				"ACMM level is unset (0) on a claimed hive — the governor has no maturity target to work toward")
		}
		if h.AgentCount <= driftNoAgentsCount {
			add(DriftKindNoAgents, DriftWarn,
				"No agents are running on this claimed hive — it holds a slot but does no work")
		}
	}

	// --- Fleet-relative signals ------------------------------------------
	//
	// Skipped entirely when no norm could be derived, and skipped for offline
	// hives (whose last-reported version is not evidence of current state) and
	// placeholders (which track the pool image, not the fleet). Also skipped
	// while the hive is ACTIVELY upgrading — mid-upgrade version and branch
	// drift is the expected state the upgrade exists to resolve, and flagging
	// it would file "being fixed" under "needs attention" (same suppression
	// discipline as status-flipping yielding to duplicate-spoke above). The
	// moment the upgrade lands, fails, or exceeds staleUpgradeTimeout,
	// activelyUpgrading drops and any remaining drift is flagged again.
	if norm.Branch != "" && h.Online && !placeholder && !activelyUpgrading {
		hiveBranch := strings.TrimSpace(h.GitBranch)
		if hiveBranch != "" && !strings.EqualFold(hiveBranch, norm.Branch) {
			add(DriftKindBranchMismatch, DriftWarn,
				fmt.Sprintf("Running branch %s while %d of the fleet's online hives run %s",
					hiveBranch, norm.Eligible, norm.Branch))
		}

		// Version drift is only asked WITHIN a branch: a hive on another
		// branch is already flagged above, and "behind" across branches is
		// not a defined relation.
		if hiveBranch == "" || strings.EqualFold(hiveBranch, norm.Branch) {
			hiveSHA := shortSHA(h.GitHash)
			// Prefer the branch's true latest build (what the hive SHOULD be
			// on); fall back to the fleet's modal SHA when the hub has not
			// resolved a latest for this branch.
			target := shortSHA(latestSHAs[norm.Branch])
			targetLabel := "the latest build on " + norm.Branch
			if target == "" {
				target = norm.SHA
				targetLabel = "the version most of the fleet runs"
			}
			if hiveSHA != "" && target != "" && !sameCommit(hiveSHA, target) {
				add(DriftKindVersionBehind, DriftInfo,
					fmt.Sprintf("Running %s, which differs from %s (%s)", hiveSHA, targetLabel, target))
			}
		}
	}

	report := DriftReport{Signals: signals, Count: len(signals)}
	for _, s := range signals {
		if driftSeverityRank[s.Severity] > driftSeverityRank[report.WorstSeverity] {
			report.WorstSeverity = s.Severity
		}
	}
	return report
}

// parseRFC3339OrTime accepts an already-parsed time.Time (the registry stores
// UpgradeStartedAt as one) and falls back to parsing a string form. ok is false
// only when neither yields a usable instant.
func parseRFC3339OrTime(t time.Time, s string) (time.Time, bool) {
	if !t.IsZero() {
		return t, true
	}
	return parseRFC3339(s)
}

// annotateDrift computes the fleet norm across the caller's hives once and
// attaches a drift report to each. Mutating in place (rather than returning a
// parallel map) keeps the report on the row the frontend already renders.
func annotateDrift(hives []MyHiveEntry, latestSHAs map[string]string, now time.Time) {
	if len(hives) == 0 {
		return
	}
	norm := computeFleetNorm(hives)
	for i := range hives {
		report := computeDrift(hives[i], norm, latestSHAs, now)
		stampDriftFirstSeen(hives[i].ID, &report, now)
		hives[i].Drift = report
	}
}

// driftFirstSeen tracks when each (hive, kind) drift signal was first observed,
// keyed by driftFirstSeenKey. Guarded by driftFirstSeenMu: annotateDrift runs
// on every My Hives request, and two requests may recompute concurrently.
//
// The map is IN-MEMORY ONLY — a hub restart re-baselines every "since" to the
// first recompute after boot. That is an accepted trade: the timestamp is a
// triage aid ("did this start before or after my change?"), not an audit
// record, and persisting it would put a disk write on the hottest read path.
var (
	driftFirstSeenMu sync.Mutex
	driftFirstSeen   = map[string]time.Time{}
)

// driftFirstSeenKeySep joins hive ID and signal kind into one map key. NUL can
// appear in neither side (hive IDs are isValidName-validated, kinds are
// compile-time constants), so the key can never be ambiguous.
const driftFirstSeenKeySep = "\x00"

func driftFirstSeenKey(hiveID, kind string) string {
	return hiveID + driftFirstSeenKeySep + kind
}

// stampDriftFirstSeen annotates each signal in report with the time this
// (hive, kind) was FIRST observed, keeping that instant stable across
// recomputes for as long as the signal stays present. Entries for kinds that
// no longer fire on this hive are cleared, so a signal that goes away and
// later returns gets a fresh first-seen rather than resurrecting the old one.
//
// Keyed by kind, not by reason: a reason legitimately mutates while the
// condition persists ("No heartbeat for 3 min" becomes "... for 4 min"), and
// re-stamping on every wording change is exactly the drift-to-now this
// tracker exists to prevent. Multiple same-kind signals (identity-split can
// raise several) share one first-seen, which is the honest reading: the hub
// first saw that KIND of trouble then.
func stampDriftFirstSeen(hiveID string, report *DriftReport, now time.Time) {
	if hiveID == "" {
		return
	}
	driftFirstSeenMu.Lock()
	defer driftFirstSeenMu.Unlock()
	present := make(map[string]bool, len(report.Signals))
	for i := range report.Signals {
		kind := report.Signals[i].Kind
		present[kind] = true
		key := driftFirstSeenKey(hiveID, kind)
		first, ok := driftFirstSeen[key]
		if !ok {
			first = now
			driftFirstSeen[key] = now
		}
		report.Signals[i].FirstSeen = first.UTC().Format(time.RFC3339)
	}
	// Clear cleared signals so a future recurrence is dated to its recurrence.
	prefix := hiveID + driftFirstSeenKeySep
	for key := range driftFirstSeen {
		if strings.HasPrefix(key, prefix) && !present[strings.TrimPrefix(key, prefix)] {
			delete(driftFirstSeen, key)
		}
	}
}
