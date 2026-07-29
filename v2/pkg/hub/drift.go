package hub

import (
	"fmt"
	"sort"
	"strings"
	"time"
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
	DriftKindHealthDegraded = "health-degraded"
	DriftKindUpgradeStuck   = "upgrade-stuck"
	DriftKindACMMUnset      = "acmm-unset"
	DriftKindNoAgents       = "no-agents"
)

// DriftSignal is one detected deviation. Reason is a complete, human-readable
// sentence — the UI renders it verbatim, so it must carry the specifics (which
// branch, which check, how long) rather than deferring to a lookup table.
type DriftSignal struct {
	Kind     string        `json:"kind"`
	Severity DriftSeverity `json:"severity"`
	Reason   string        `json:"reason"`
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
func isPlaceholderEntry(h MyHiveEntry) bool {
	if h.ProvStatus == statusAvailable {
		return true
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
// ("ghcr.io/kubestellar/hive:v2-latest" → "v2-latest"). Returns "" when the ref
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
		name, _ := ck["name"].(string)
		if name == "" {
			name = "unnamed check"
		}
		detail, _ := ck["detail"].(string)
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
		}
	}

	// Image pinned to an immutable tag. The spoke reports its own Deployment's
	// image over the heartbeat; a tag that is not "<branch>-latest" can never
	// receive a rolling upgrade, which is how one spoke sat on hive:63d8902
	// restart-looping while the fleet moved on. Reuses imageTagIsMutable — the
	// SAME predicate UpgradeSelfToSHA uses to decide a restart is a no-op, so
	// the badge can never disagree with the upgrade path about what is pinned.
	//
	// Guarded on a non-empty ref: "unknown image" (old spoke, or not running
	// in-cluster) must never render as "pinned".
	if h.ImageRef != "" && !imageTagIsMutable(h.ImageRef) {
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
	// A placeholder legitimately has no App, no agents and ACMM 0. Flagging it
	// for those would make the pool dominate the exceptions summary and hide
	// the hives a human can actually fix.
	if !placeholder {
		if h.GitHubAppRequired {
			if h.GitHubAppPermIssue != "" {
				add(DriftKindAppPermIssue, DriftCritical,
					fmt.Sprintf("GitHub App is installed but its permissions are insufficient: %s", h.GitHubAppPermIssue))
			} else {
				add(DriftKindAppMissing, DriftCritical,
					"GitHub App is required by this hive but is not installed — agents cannot act on the repo")
			}
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
	// placeholders (which track the pool image, not the fleet).
	if norm.Branch != "" && h.Online && !placeholder {
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
		hives[i].Drift = computeDrift(hives[i], norm, latestSHAs, now)
	}
}
