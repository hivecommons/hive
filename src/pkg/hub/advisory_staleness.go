package hub

import (
	"strings"
	"time"
)

// advisoryStaleThreshold is how long a hive's advisory digest may go without a
// successful update before the hub flags it as stale. Advisory digests are
// posted once per governor eval cycle; even a low-ACMM hive (which evaluates
// infrequently by design — on the order of every ~10 min) posts far more often
// than this. Operators may also slow the posting cadence deliberately via
// governor.advisory.update_interval_s (#4820), whose maximum
// (config.MaxAdvisoryUpdateIntervalS = 1h) is capped specifically to stay
// under this threshold — the effective baseline is therefore always
// max(configured interval, default cadence), with 30 minutes of slack on top
// of the slowest legal cadence (pinned by
// TestAdvisoryStaleThresholdCoversMaxUpdateInterval). 90 minutes is therefore
// comfortably longer than any realistic or configurable posting interval, so
// the pill only lights up on a digest path that has GENUINELY wedged (a
// working App that has quietly stopped posting), never on a hive that is
// merely slow. Kept a named constant with this reasoning so the threshold is
// not a bare magic number.
const advisoryStaleThreshold = 90 * time.Minute

// appCanWriteForAdvisory reports whether a hive's GitHub App is in a state that
// COULD post an advisory digest right now. It is the "app can write" gate on
// the staleness alarm: a hive that legitimately cannot post — the App is not
// installed, its installation is minting no tokens, or the operator never
// delivered its key — must NEVER be flagged stale, because its silence is
// expected, not a failure.
//
// The rule mirrors how the spoke and both dashboards already branch on these
// signals: a raised GitHubAppRequired banner, a pending install, or any
// non-"ok" classified app-auth state all mean the App cannot currently write.
// An empty state is treated as writable-unless-proven-otherwise, since a hive
// that is genuinely posting digests reports no app error — the digest post is
// itself the proof it can write, and the AdvisoryLastPostedAt gate handles the
// rest.
func appCanWriteForAdvisory(e RegistryEntry) bool {
	if e.GitHubAppRequired || e.PendingGitHubAppInstall {
		return false
	}
	switch e.GitHubAppState {
	case "not-installed", "key-missing", "key-invalid", "no-app-assigned",
		"wrong-installation", "insufficient-permissions",
		// #2353: a write returned 403 on an otherwise-healthy install — the App
		// demonstrably cannot write this repo, so it is NOT writable here.
		"write-forbidden",
		// #5774: the configured repositories were transferred to another
		// account and the installation covers NOTHING under the configured
		// owner (that is a required clause of the classification, see
		// InstallationCoverage.MovedTo) — so no repo this hive is pointed at,
		// including the advisory repo, is writable.
		//
		// This is deliberately NOT extended to "repo-not-covered", which sits
		// outside this list on purpose: that state fires when ANY configured
		// repo is unticked, which need not be the advisory one, so it is not
		// evidence that the digest post cannot land.
		"repo-moved":
		return false
	default:
		// "ok", "unknown", or empty (spoke too old to classify) — the App is
		// not KNOWN to be broken, so writability is not the thing blocking a
		// stale flag here.
		return true
	}
}

// appAwaitingDelivery reports whether a hive's App has not been DELIVERED yet —
// never installed, never assigned, or its key never handed over. This is the
// narrow subset of "cannot write" whose silence is an ONBOARDING state rather
// than a fault: nobody has finished wiring the hive up, so nothing about it is
// broken and nothing should alarm.
//
// It exists because appCanWriteForAdvisory is too broad to gate a REPORTED post
// error (#4167). A spoke only records AdvisoryError from a real post attempt,
// and the most common such error — a 403 on the digest comment — makes the
// spoke raise GitHubAppRequired and classify itself "write-forbidden" in the
// SAME cycle. Gating the error on appCanWriteForAdvisory therefore let the
// failure suppress its own alarm: the hive proved it is an advisory participant
// AND that its digest is wedged, and the pill went dark anyway. An App that has
// been delivered but cannot write is a fault the operator must see; an App that
// was never delivered is not.
//
// An unclassified state ("" — a spoke too old to report one) counts as awaiting
// delivery only when the install banner is up, which is the one signal such a
// spoke does report.
func appAwaitingDelivery(e RegistryEntry) bool {
	if e.PendingGitHubAppInstall {
		return true
	}
	switch e.GitHubAppState {
	case "not-installed", "no-app-assigned", "key-missing":
		return true
	case "":
		return e.GitHubAppRequired
	default:
		return false
	}
}

// allAgentsQuietByDesign reports whether EVERY agent this hive reports is
// deliberately quiet — paused by the operator, or not expected active in the
// current governor mode. Advisory findings come from agents; with all of them
// intentionally off there is nothing new to digest, so an advisory timestamp
// that merely AGES in that state is the operator's own choice, not a wedge,
// and must not light the stale pill (a reported post ERROR still does — a
// broken post path is broken regardless of who is paused).
//
// Deliberately conservative in the unknowns' favor: a hive reporting NO agents
// (legacy spoke, or the field lost in transit) returns false, so this gate
// never suppresses a real alarm on a spoke we cannot read. A legacy agent
// entry (none of the new protocol fields) also returns false for the same
// reason. The pause detection mirrors deriveAgentVerdict's quiet-by-design
// arms: Paused/state=paused, or a new-protocol agent that is not expected
// active and not running.
func allAgentsQuietByDesign(e RegistryEntry) bool {
	if len(e.Agents) == 0 {
		return false
	}
	for _, a := range e.Agents {
		if legacyAgent(a) {
			return false
		}
		paused := a.Paused || strings.EqualFold(a.State, agentStatePaused)
		// Off-schedule counts as quiet REGARDLESS of session state: spokes
		// keep persistent agent sessions alive between kicks, so an agent the
		// governor expects off still reports state=running while idle (same
		// rule as deriveAgentVerdict's quiet-by-design arm).
		offByDesign := !a.ExpectedActive
		if !paused && !offByDesign {
			return false
		}
	}
	return true
}

// carryAdvisoryPostTime preserves the last known advisory-post time on a
// heartbeat that reports none (#4167).
//
// advisoryLastPostedAt lives ONLY in the spoke's memory, so every restart
// reports it empty until the next SUCCESSFUL post — and an empty value is
// exactly what the hub's staleness gate reads as "not an advisory participant".
// A hive whose digest path wedges across a restart therefore went silent
// forever: it could never post (so the field never came back) and the hub could
// never tell it apart from a healthy PR-only hive. Preserving the previous value
// lets the timestamp keep AGEING across the restart, which is the whole signal.
//
// Guarded on the hive still pointing at the same primary repo: a reclaimed or
// reassigned placeholder is a DIFFERENT project on the same hive ID, and must
// start from a clean advisory history rather than inherit — and be alarmed for —
// the previous tenant's post time. A fresh report always wins; the spoke is the
// source of truth whenever it has one.
func carryAdvisoryPostTime(entry *RegistryEntry, prev RegistryEntry) {
	if entry == nil {
		return
	}
	if entry.AdvisoryLastPostedAt == "" && prev.AdvisoryLastPostedAt != "" &&
		entry.PrimaryRepo == prev.PrimaryRepo {
		entry.AdvisoryLastPostedAt = prev.AdvisoryLastPostedAt
	}
}

// advisoryStale reports whether a hive's advisory digest should be flagged as
// stale as of now, together with a short human-readable reason for the pill's
// tooltip. It is THE discriminator between "genuinely broken" (the signal an
// operator wants) and "expected quiet" (which must never alarm), and every gate
// below is required — the alarm fires only when ALL of them hold:
//
//  1. The hive is actually IN the advisory-posting business. The spoke reports
//     AdvisoryLastPostedAt only after it has successfully posted at least once,
//     and AdvisoryError only from a real post attempt — so a pure PR/merge-mode
//     hive (no advisory agents, never reaches the post path) reports NEITHER and
//     is skipped here. This is why the gate needs no separate "is advisory mode"
//     flag from the hub: participation is proven by the spoke having reported
//     one of these fields at all.
//  2. Either the last successful post is older than advisoryStaleThreshold, OR
//     the most recent post attempt errored (AdvisoryError set).
//  3. The suppression that applies to the branch taken:
//     - a reported ERROR is suppressed only while the App is still awaiting
//     delivery (appAwaitingDelivery) — an undelivered App's failure is an
//     onboarding symptom, but a delivered App that cannot write is a fault the
//     operator must see, even though the same failure raises the App banner.
//     - an AGED timestamp is suppressed whenever the App cannot write at all
//     (appCanWriteForAdvisory): with no error reported there is no proof the
//     hive even tried, so its silence is expected. It is also suppressed when
//     EVERY agent is deliberately quiet (allAgentsQuietByDesign): paused/off
//     agents produce no findings, so an ageing digest is the operator's own
//     pause, not a wedge.
//
// An empty/unparseable AdvisoryLastPostedAt with no AdvisoryError is UNKNOWN and
// returns false — never a false alarm — matching the rule the codebase already
// applies to StartedAt and GitHubAPIURL.
func advisoryStale(e RegistryEntry, now time.Time) (stale bool, reason string) {
	// Gate 1: the hive must participate in advisory posting at all. No reported
	// post time AND no reported error means "not advisory mode / old spoke" —
	// UNKNOWN, never stale.
	if e.AdvisoryLastPostedAt == "" && e.AdvisoryError == "" {
		return false, ""
	}

	// Gate 2a: a failed post attempt is a stale signal on its own, and the most
	// specific one — surface the reported cause. Only an App that was never
	// delivered suppresses it (#4167): gating this on the broader
	// appCanWriteForAdvisory let a write-forbidden digest failure raise the App
	// banner and thereby silence the very pill it should have lit.
	if e.AdvisoryError != "" {
		if appAwaitingDelivery(e) {
			return false, ""
		}
		return true, "last advisory post failed: " + e.AdvisoryError
	}

	// Gate 2b: with no error reported, an App that cannot write is expected to
	// be quiet, not broken.
	if !appCanWriteForAdvisory(e) {
		return false, ""
	}

	// Gate 2c: with no error reported and EVERY agent deliberately quiet
	// (paused / off-schedule), an ageing digest is the expected consequence of
	// the operator's own pause — findings come from agents, and none are
	// running to produce them. Not a fault, never a pill.
	if allAgentsQuietByDesign(e) {
		return false, ""
	}

	// Gate 3: no error, so decide on age. An unparseable timestamp is treated
	// as UNKNOWN (not stale) so a malformed spoke value never false-alarms.
	postedAt, err := time.Parse(time.RFC3339, e.AdvisoryLastPostedAt)
	if err != nil {
		return false, ""
	}
	if now.Sub(postedAt) > advisoryStaleThreshold {
		return true, "advisory digest has not updated since " + postedAt.UTC().Format(time.RFC3339)
	}
	return false, ""
}
