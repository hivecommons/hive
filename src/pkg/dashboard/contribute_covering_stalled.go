package dashboard

import (
	"fmt"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// covering_pr_stalled (governor.labels.stage.abandon_after): an issue locked
// by an open pull request whose stage record carries the abandoned flag.
// Surfacing only. The lock stands — an open PR by another author is the
// owner's to settle (hivecommons/hive#9691, #17292) — so admission refuses
// the issue exactly as it does under open_pr_claim / open_pr_linked; only the
// reason and its detail change, so the Withheld list says what the lock is
// waiting on instead of "an open pull request already claims this issue".

// coveringPRStalledDetail reports whether the open PR prRepo#prNumber that
// covers an issue in issueRepoFull is flagged abandoned in the stage store,
// with the Withheld row's detail ("Open PR #N stalled Nd — close it or take
// it over; the owner decides"). prRepo may be empty (the issue's own
// repository), a bare repository name (the issue's owner) or "owner/repo".
func (h *ContributeWSHub) coveringPRStalledDetail(issueRepoFull, prRepo string, prNumber int, now time.Time) (string, bool) {
	if h == nil || prNumber <= 0 {
		return "", false
	}
	repo := coveringPRRepoFull(issueRepoFull, prRepo)
	if repo == "" {
		return "", false
	}
	store := h.stageStoreOrShared()
	if store == nil {
		return "", false
	}
	rec, ok := store.Get(stagelabels.Key(repo, prNumber))
	if !ok || !rec.Abandoned() {
		return "", false
	}
	return coveringPRStalledText(prNumber, rec, now), true
}

// coveringPRStalledText is the Withheld row's detail for an abandoned
// covering PR.
func coveringPRStalledText(prNumber int, rec stagelabels.Record, now time.Time) string {
	since := rec.AbandonedIdleSince
	if since.IsZero() {
		since = rec.IdleSince()
	}
	age := "past the abandon threshold"
	if !since.IsZero() && now.After(since) {
		age = abandonedAge(now.Sub(since))
	}
	text := fmt.Sprintf("Open PR #%d stalled %s — close it or take it over; the owner decides", prNumber, age)
	if blocker := strings.TrimSpace(rec.AbandonedBlocker); blocker != "" {
		text += " (blocked on " + blocker + ")"
	}
	return text
}

// abandonedAge renders an idle duration the way the abandoned reason does
// ("9d", "5h"), taken from stagelabels.AbandonedReasonText.
func abandonedAge(idle time.Duration) string {
	return strings.TrimPrefix(stagelabels.AbandonedReasonText(idle, ""), "abandoned ")
}

// coveringPRRepoFull resolves the covering PR's repository to "owner/repo".
func coveringPRRepoFull(issueRepoFull, prRepo string) string {
	prRepo = strings.TrimSpace(prRepo)
	issueRepoFull = strings.TrimSpace(issueRepoFull)
	switch {
	case strings.Contains(prRepo, "/"):
		return prRepo
	case prRepo == "":
		return issueRepoFull
	}
	if owner, _, ok := strings.Cut(issueRepoFull, "/"); ok && owner != "" {
		return owner + "/" + prRepo
	}
	return ""
}

// coveringPRURL is the covering PR behind a covering_pr_stalled refusal:
// the live link's when the guard caught it, else the ledger claim's.
func (d contributorAdmissionDecision) coveringPRURL() string {
	if d.openPR.PRURL != "" {
		return d.openPR.PRURL
	}
	return d.claim.PRURL
}
