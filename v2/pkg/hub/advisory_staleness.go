package hub

import "time"

// advisoryStaleThreshold is how long a hive's advisory digest may go without a
// successful update before the hub flags it as stale. Advisory digests are
// posted once per governor eval cycle; even a low-ACMM hive (which evaluates
// infrequently by design — on the order of every ~10 min) posts far more often
// than this. 90 minutes is therefore comfortably longer than any realistic
// posting interval, so the pill only lights up on a digest path that has
// GENUINELY wedged (a working App that has quietly stopped posting), never on a
// hive that is merely slow. Kept a named constant with this reasoning so the
// threshold is not a bare magic number.
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
		"wrong-installation", "insufficient-permissions":
		return false
	default:
		// "ok", "unknown", or empty (spoke too old to classify) — the App is
		// not KNOWN to be broken, so writability is not the thing blocking a
		// stale flag here.
		return true
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
//  2. The hive's App can WRITE (appCanWriteForAdvisory). A not-installed hive,
//     one running without credentials, or one whose key was never delivered
//     legitimately cannot post and must not alarm.
//  3. Either the last successful post is older than advisoryStaleThreshold, OR
//     the most recent post attempt errored (AdvisoryError set).
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

	// Gate 2: an App that cannot write is expected to be quiet, not broken.
	if !appCanWriteForAdvisory(e) {
		return false, ""
	}

	// Gate 3a: a failed post attempt is a stale signal on its own, and the most
	// specific one — surface the reported cause.
	if e.AdvisoryError != "" {
		return true, "last advisory post failed: " + e.AdvisoryError
	}

	// Gate 3b: no error, so decide on age. An unparseable timestamp is treated
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
