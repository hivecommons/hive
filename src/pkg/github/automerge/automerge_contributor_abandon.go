package automerge

import (
	"context"
	"strings"
	"time"
)

// Abandon reasons carried on a ContributorAbandon (hivecommons/hive#15782):
// the contributor-profile vocabulary, not the stage record's AdoptReason*.
const (
	// AbandonReasonOffline: the contributor's relay was offline past the
	// offline window after the nudge.
	AbandonReasonOffline = "offline"
	// AbandonReasonUnresponsive: the grace elapsed with the nudge
	// unanswered, or no hive of the author's is registered here.
	AbandonReasonUnresponsive = "unresponsive"
	// AbandonReasonOperatorTakeover: an operator's Adopt now or an
	// owner-delegate's adoption/supersede request.
	AbandonReasonOperatorTakeover = "operator_takeover"
)

// ContributorAbandon is one adoption of a contributor's PR by the hive: in
// place (AdoptedIntoPR 0) or by supersession onto hive/adopt-<n>.
type ContributorAbandon struct {
	Author        string
	Repo          string // owner/repo
	PR            int
	AdoptedIntoPR int
	Reason        string
	At            time.Time
}

// ContributorAbandonSink is the optional third face of the nudge sink: it
// records an adoption against the author's contributor profile so operators
// can see which clankers keep abandoning PRs. Implementations dedupe per PR;
// the engine reports every adoption, including second takeovers.
type ContributorAbandonSink interface {
	Abandoned(ctx context.Context, a ContributorAbandon)
}

// abandonReason maps a stage-record adoption reason onto the profile's.
func abandonReason(adoptReason string) string {
	switch adoptReason {
	case AdoptReasonOperator, AdoptReasonOperatorForced:
		return AbandonReasonOperatorTakeover
	case AdoptReasonOffline:
		return AbandonReasonOffline
	}
	return AbandonReasonUnresponsive
}

// recordContributorAbandon tells the sink, when it records abandons, that
// the hive adopted obs (into PR into, 0 for in place).
func (c *Engine) recordContributorAbandon(ctx context.Context, obs stageObservation, adoptReason string, into int, now time.Time) {
	sink, ok := c.contributorNudge.(ContributorAbandonSink)
	if !ok || sink == nil || strings.TrimSpace(obs.Author) == "" || obs.Number <= 0 {
		return
	}
	sink.Abandoned(ctx, ContributorAbandon{
		Author:        obs.Author,
		Repo:          obs.Owner + "/" + obs.Repo,
		PR:            obs.Number,
		AdoptedIntoPR: into,
		Reason:        abandonReason(adoptReason),
		At:            now.UTC(),
	})
}
