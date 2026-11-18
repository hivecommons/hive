package automerge

import (
	"context"
	"strings"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

// humanClaimTransport is the optional transport hook that reports a claim an
// owner or maintainer holds on an issue outside the ledger
// (hgithub.Client.IssueHumanClaimReason; hivecommons/hive#18899).
type humanClaimTransport interface {
	IssueHumanClaimReason(ctx context.Context, repo string, number int, holder string) (string, error)
}

// adoptionHumanClaim returns the claim reason ("human claim: assigned by
// andy") when an owner or maintainer holds a claim on an issue the contributor
// PR closes, "" otherwise. The PR's own author is the holder: their assignment
// or comment is the contributor's own and never counts against them. A
// transport that cannot answer, or a lookup failure, is "no claim": adoption
// is a timing decision and the next sweep asks again.
func (c *Engine) adoptionHumanClaim(ctx context.Context, obs stageObservation) string {
	t, ok := c.transport.(humanClaimTransport)
	if !ok {
		return ""
	}
	for _, ref := range hgithub.ParseClaimedIssues(obs.Title+"\n"+obs.Body, obs.Display) {
		if ref.Issue <= 0 || (ref.Repo != "" && !strings.EqualFold(ref.Repo, obs.Display)) {
			continue
		}
		reason, err := t.IssueHumanClaimReason(ctx, obs.Display, ref.Issue, obs.Author)
		if err != nil {
			c.debug("stage labels: human-claim lookup failed; not blocking adoption", "repo", obs.Display, "pr", obs.Number, "issue", ref.Issue, "error", err)
			continue
		}
		if reason != "" {
			return reason
		}
	}
	return ""
}
