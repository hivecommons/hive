package automerge

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Never adopt a contributor PR another open PR already covers
// (hivecommons/hive#18900, child of #18890). On #18866 the hive adopted
// contributor PR #18870 as #18879 while the owner's #18873 for the same issue
// was already open; the dashboard then offered "Let hive decide" over two
// mid-CI PRs. Adoption is the hive taking over a contributor's abandoned work,
// and there is nothing abandoned about work a person (or the hive) already has
// in flight: "green first, then earlier-opened" breaks ties between equal
// trust, it never lets a contributor copy win over a person's or the hive's PR.

// AdoptionCoveredMarker opens the one note a covered contributor PR gets when
// the hive closes it as a duplicate instead of adopting it.
const AdoptionCoveredMarker = "<!-- hive:adoption-covered"

// Author associations GitHub reports for an owner or maintainer.
var coveringPRTrustedAssociations = []string{"OWNER", "MEMBER", "COLLABORATOR"}

// coveringPR is an open PR that already covers an issue the contributor PR
// closes and outranks it: a person's or the hive's own.
type coveringPR struct {
	Number int
	// Who is how the closing note names the author: "a person's" or "the
	// hive's".
	Who   string
	Issue int
}

// coveringPRTrust classifies other as a person's or the hive's PR, "" when it
// is neither (another contributor's, which is an equal-trust tie).
func (c *Engine) coveringPRTrust(other *gh.PullRequest, policy StageLabelPolicy, appLogin string) string {
	login := strings.TrimSpace(other.GetUser().GetLogin())
	names := policy.AuthorKindLabels.Normalize()
	var labels []string
	for _, l := range other.Labels {
		labels = append(labels, l.GetName())
	}
	switch {
	case hasLabelFold(labels, names.Hive),
		login != "" && appLogin != "" && strings.EqualFold(login, appLogin),
		login != "" && policy.HiveAuthor != nil && policy.HiveAuthor(login):
		return "the hive's"
	case hasLabelFold(labels, names.Human),
		login != "" && policy.OwnerAuthor != nil && policy.OwnerAuthor(login):
		return "a person's"
	case hasLabelFold(labels, names.Clanker):
		return ""
	}
	if slices.Contains(coveringPRTrustedAssociations, strings.ToUpper(other.GetAuthorAssociation())) {
		return "a person's"
	}
	return ""
}

// adoptionCoveringPR finds an open PR, on the same base line, that closes an
// issue the contributor PR closes and that is a person's or the hive's. The
// lowest number wins so the answer is stable across passes. An open-PR list
// that cannot be read is logged and reads as "not covered": the check adds a
// refusal, it never becomes a new way for adoption to stall.
func (c *Engine) adoptionCoveringPR(ctx context.Context, obs stageObservation, policy StageLabelPolicy) (coveringPR, bool) {
	var cover coveringPR
	found := false
	var mine []int
	for _, ref := range hgithub.ParseClaimedIssues(obs.Title+"\n"+obs.Body, obs.Display) {
		if ref.Issue > 0 && (ref.Repo == "" || strings.EqualFold(ref.Repo, obs.Display)) {
			mine = append(mine, ref.Issue)
		}
	}
	if len(mine) == 0 {
		return coveringPR{}, false
	}
	prs, skip, err := c.openPullRequests(ctx, obs.Owner, obs.Repo, nil)
	if err != nil || skip != "" {
		c.debug("stage labels: open PRs unreadable; not checking for a covering PR", "repo", obs.Display, "pr", obs.Number, "skip", skip, "error", err)
		return coveringPR{}, false
	}
	appLogin := strings.TrimSpace(c.transport.AppBotLogin())
	for _, other := range prs {
		if other == nil || other.GetNumber() <= 0 || other.GetNumber() == obs.Number || (other.State != nil && other.GetState() != "open") {
			continue
		}
		if base := other.GetBase().GetRef(); base != "" && obs.BaseRef != "" && base != obs.BaseRef {
			continue
		}
		// The hive's own copy of this PR (or this PR re-homed onto one) is
		// the same change in flight, not a competing fix.
		if hgithub.PRSupersedes(other.GetBody(), obs.Number) || hgithub.PRSupersedes(obs.Body, other.GetNumber()) {
			continue
		}
		shared := 0
		for _, issue := range mergeTrainClosedIssues(other, obs.Display) {
			if slices.Contains(mine, issue) {
				shared = issue
				break
			}
		}
		if shared == 0 {
			continue
		}
		who := c.coveringPRTrust(other, policy, appLogin)
		if who == "" {
			continue
		}
		if !found || other.GetNumber() < cover.Number {
			cover, found = coveringPR{Number: other.GetNumber(), Who: who, Issue: shared}, true
		}
	}
	return cover, found
}

// AdoptionCoveredComment renders the note: thanks, the covering PR and the
// credit the contributor keeps.
func AdoptionCoveredComment(obs stageObservation, hiveID string, cover coveringPR) string {
	return fmt.Sprintf("%s by=%s covered_by=%d issue=%d head=%s -->\n"+
		"Thank you for this PR. %s PR #%d already covers #%d and the hive keeps one PR per issue, so it is closing this one as a duplicate instead of adopting it. "+
		"Your work is credited on the issue; if #%d does not land, comment here and a maintainer can reopen this PR.",
		AdoptionCoveredMarker, nonEmpty(hiveID, "hive"), cover.Number, cover.Issue, obs.HeadSHA,
		strings.ToUpper(cover.Who[:1])+cover.Who[1:], cover.Number, cover.Issue, cover.Number)
}

// closeCoveredContributorPR closes the contributor PR as a duplicate of the
// covering PR, with one thank-you note. Adoption is refused whatever happens
// here: the block is recorded on the stage record (the tracker row says why)
// and a write-back gate refusal or a failed close leaves the PR open with
// adoption still off. It runs once per head: a recorded block is not
// re-posted. It returns the number of comments posted.
func (c *Engine) closeCoveredContributorPR(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, cover coveringPR, now time.Time) (int, error) {
	rec, _ := store.Get(key)
	if rec.AdoptionBlocked(obs.HeadSHA) == stagelabels.AdoptBlockedCoveredByOpenPR {
		return 0, nil
	}
	logArgs := []any{"repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "covered_by", cover.Number, "issue", cover.Issue}
	store.NoteAdoptionBlocked(key, obs.HeadSHA, stagelabels.AdoptBlockedCoveredByOpenPR, now)
	body := AdoptionCoveredComment(obs, policy.HiveID, cover)
	if d := c.writebackAllowed(obs.Display, writebackItemForObservation(obs), writeback.KindContributorAdopt, body); !d.Allowed {
		c.info("stage labels: covered contributor PR not adopted; close suppressed by the write-back gate, left open", append(logArgs, "writeback_reason", d.Reason, "writeback_detail", d.Detail)...)
		return 0, nil
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		c.warn("stage labels: covered contributor PR note failed; adoption stays off", append(logArgs, "error", err)...)
		return 0, err
	}
	if _, _, err := c.gh.PullRequests.Edit(ctx, obs.Owner, obs.Repo, obs.Number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil { // writeback:gated
		c.warn("stage labels: closing the covered contributor PR failed; adoption stays off", append(logArgs, "error", err)...)
		return 1, err
	}
	c.info("stage labels: contributor PR closed as a duplicate of an open PR that covers the issue; not adopted", logArgs...)
	return 1, nil
}
