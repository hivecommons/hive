package automerge

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Stage-074 adoption (hivecommons/hive#12436).
//
// A contributor PR in stage/072-returned-to-contributor whose author has not
// pushed a new head within the grace after the nudge is adopted: the hive
// re-lanes it through the takeover path so the scanner can finish it. The
// grace is contributor_grace_online (default 2h) when the author's relay
// was reachable at the nudge, contributor_grace_offline (1h) otherwise — an
// external author or an offline relay is the offline case, and a relay
// contributor who was over their fix budget ("queued") counts as online
// because adoption is the pressure-relief valve for THEIR overflow too.
//
// Adopting means: push `hive/adopt-<n>` in the base repo at the contributor
// head (the fork's commits, authorship and sign-off preserved byte for byte),
// open an App PR from it that supersedes the original and carries its
// Closes lines, comment once on the original, and move it to
// stage/074-adopted-by-hive. The App PR is red with the same failures, so it
// lands in ci-failing.json and the scanner's FIX-BEFORE-NEW kick (the
// existing red-PR repair route for hive-authored PRs) rebases it on the base
// and fixes CI on that branch — no second kick mechanism. Once the App PR
// merges the original is closed with a note; if the App PR is closed
// unmerged the adoption is forgotten and the original falls back to 072.
//
// One attempt per contributor head, whatever the outcome; a new head from
// the author before adoption re-arms the nudge instead.

const (
	// adoptBranchPrefix names the App branch that adopts contributor PR <n>.
	adoptBranchPrefix = "hive/adopt-"
	// AdoptionMarkerPrefix opens the one adoption comment on the original PR.
	AdoptionMarkerPrefix = "<!-- hive:adopted"
	// adoptionCloseMarker is stamped on the close note after the App PR merged.
	adoptionCloseMarker = "<!-- hive:adopted-merged -->"
	// adoptCheckInterval bounds how often an adopted PR's superseder is polled.
	adoptCheckInterval = 10 * time.Minute
)

// adoptClosesPattern lifts the original PR's closing keywords so the
// superseding PR closes the same issues.
var adoptClosesPattern = regexp.MustCompile(`(?im)^\s*(?:fixes|closes|resolves)\s+(?:[\w.-]+/[\w.-]+)?#\d+\s*$`)

// adoptionGrace returns the grace that applies to the record's first nudge.
func adoptionGrace(rec stagelabels.Record, policy StageLabelPolicy) time.Duration {
	if rec.NudgeOnline {
		if policy.GraceOnline > 0 {
			return policy.GraceOnline
		}
		return 2 * time.Hour
	}
	if policy.GraceOffline > 0 {
		return policy.GraceOffline
	}
	return time.Hour
}

// adoptionDue reports whether the PR at head has sat nudged past its grace
// with no new head, and was not already adopted at this head.
func adoptionDue(rec stagelabels.Record, head string, policy StageLabelPolicy, now time.Time) bool {
	if head == "" || rec.NudgeHead != head || rec.NudgeCount == 0 || rec.FirstNudgeAt.IsZero() {
		return false
	}
	if rec.Adopted(head) {
		return false
	}
	return now.Sub(rec.FirstNudgeAt) >= adoptionGrace(rec, policy)
}

// adoptionCloses extracts the closing-keyword lines from the original body.
func adoptionCloses(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range adoptClosesPattern.FindAllString(body, -1) {
		line := strings.TrimSpace(m)
		if !seen[strings.ToLower(line)] {
			seen[strings.ToLower(line)] = true
			out = append(out, line)
		}
	}
	return out
}

// AdoptionPRBody is the superseding App PR's body.
func AdoptionPRBody(obs stageObservation, rec stagelabels.Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Supersedes #%d (adopted after CI went red and the contributor was unavailable; original commits by @%s preserved).\n\n", obs.Number, obs.Author)
	for _, c := range adoptionCloses(obs.Body) {
		b.WriteString(c)
		b.WriteString("\n")
	}
	if len(adoptionCloses(obs.Body)) > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "This branch is #%d's head `%s` as pushed by @%s", obs.Number, shortSHA(obs.HeadSHA), obs.Author)
	if obs.HeadRepo != "" && !strings.EqualFold(obs.HeadRepo, obs.Display) {
		fmt.Fprintf(&b, " (fork `%s`)", obs.HeadRepo)
	}
	b.WriteString(", unchanged. Last lane reason on the original: `")
	b.WriteString(nonEmpty(rec.Reason, obs.Reason))
	b.WriteString("`.\n\n")
	b.WriteString("**Hive fix agent:** rebase this branch on `")
	b.WriteString(nonEmpty(obs.BaseRef, "the base branch"))
	b.WriteString("`, fix the failing checks / review asks, and push to THIS branch. Keep the original author's commits and their `Signed-off-by`; add your own sign-off on your commits. Do not open another PR.\n")
	return b.String()
}

// AdoptionComment is the one note left on the original PR when adopted.
func AdoptionComment(obs stageObservation, superseding int, grace time.Duration, online bool) string {
	presence := "no relay session was reachable"
	if online {
		presence = "the relay nudge went unanswered"
	}
	return fmt.Sprintf("%s by=%d head=%s -->\n@%s the hive has adopted this PR (`%s`): CI stayed red for %s after the nudge and %s. "+
		"It is being finished as #%d, which carries your commits (and sign-off) unchanged; the hive will rebase and fix CI there. "+
		"This PR is closed automatically once #%d merges. If you would rather finish it yourself, push a new head here and say so on #%d.",
		AdoptionMarkerPrefix, superseding, obs.HeadSHA, obs.Author, stagelabels.AdoptedByHive.Label(), formatGrace(grace), presence, superseding, superseding, superseding)
}

func formatGrace(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return d.Round(time.Minute).String()
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// adoptContributorPR runs the stage-074 check for one PR in stage 072 and,
// when the grace has elapsed, performs the adoption. It returns the number
// of comments posted. Failures are logged and the attempt is still recorded
// (one per head) so a broken API never loops on the same PR.
func (c *Engine) adoptContributorPR(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, now time.Time) (int, error) {
	rec, ok := store.Get(key)
	if !ok || obs.Lane == stageLaneSelf || obs.Author == "" || obs.BaseRef == "" {
		return 0, nil
	}
	if !adoptionDue(rec, obs.HeadSHA, policy, now) {
		return 0, nil
	}
	grace := adoptionGrace(rec, policy)
	appLogin := strings.TrimSpace(c.transport.AppBotLogin())
	// Record the attempt first: whatever happens below, this head is tried once.
	store.MarkAdopted(key, obs.HeadSHA, 0, now)
	if appLogin == "" {
		c.warn("stage labels: cannot adopt contributor PR, App login unknown", "repo", obs.Display, "pr", obs.Number)
		return 0, nil
	}
	c.info("stage labels: adopting contributor PR", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "grace", grace.String(), "online", rec.NudgeOnline, "reason", obs.Reason)

	owner, repo, number := obs.Owner, obs.Repo, obs.Number
	branch := adoptBranchPrefix + strconv.Itoa(number)
	fullRepo := owner + "/" + repo
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo: fullRepo, Kind: effects.KindBranchPush, Target: branch, Actor: "stage_labels",
		Inputs: map[string]string{"pr": strconv.Itoa(number), "head_sha": obs.HeadSHA, "adopt": "true"},
	}, func(ctx context.Context) (effects.Result, error) {
		_, _, apiErr := c.gh.Git.CreateRef(ctx, owner, repo, &gh.Reference{
			Ref:    gh.Ptr("refs/heads/" + branch),
			Object: &gh.GitObject{SHA: gh.Ptr(obs.HeadSHA)},
		})
		if apiErr != nil && isGitHubStatus(apiErr, http.StatusUnprocessableEntity) {
			// The branch exists from an earlier attempt: point it at this head.
			_, _, apiErr = c.gh.Git.UpdateRef(ctx, owner, repo, &gh.Reference{
				Ref:    gh.Ptr("refs/heads/" + branch),
				Object: &gh.GitObject{SHA: gh.Ptr(obs.HeadSHA)},
			}, true)
		}
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: obs.HeadSHA}, nil
	})
	if err != nil {
		c.warn("stage labels: adoption failed to push branch", "repo", obs.Display, "pr", number, "branch", branch, "error", err)
		return 0, err
	}

	var created *gh.PullRequest
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo: fullRepo, Kind: effects.KindPullRequestCreate, Target: branch, Actor: "stage_labels",
		Inputs: map[string]string{"base": obs.BaseRef, "supersedes": strconv.Itoa(number), "head_sha": obs.HeadSHA},
	}, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		created, _, apiErr = c.gh.PullRequests.Create(ctx, owner, repo, &gh.NewPullRequest{
			Title: gh.Ptr(nonEmpty(strings.TrimSpace(obs.Title), fmt.Sprintf("Adopt #%d", number))),
			Head:  gh.Ptr(branch),
			Base:  gh.Ptr(obs.BaseRef),
			Body:  gh.Ptr(AdoptionPRBody(obs, rec)),
		})
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: created.GetHTMLURL()}, nil
	})
	if err != nil || created == nil {
		c.warn("stage labels: adoption failed to open App PR", "repo", obs.Display, "pr", number, "branch", branch, "error", err)
		return 0, err
	}
	superseding := created.GetNumber()
	store.MarkAdopted(key, obs.HeadSHA, superseding, now)
	c.info("stage labels: contributor PR adopted", "repo", obs.Display, "pr", number, "head", obs.HeadSHA, "superseded_by", superseding, "branch", branch)

	body := AdoptionComment(obs, superseding, grace, rec.NudgeOnline)
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: adoption comment failed", "repo", obs.Display, "pr", number, "superseded_by", superseding, "error", err)
		return 0, err
	}
	return 1, nil
}

// settleAdoption polls the superseding App PR for a PR in stage 074 (at
// most every adoptCheckInterval): merged → close the original with a note;
// closed unmerged → forget the adoption so the original returns to 072.
func (c *Engine) settleAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, now time.Time) (int, error) {
	rec, ok := store.Get(key)
	if !ok || rec.AdoptedBy <= 0 {
		return 0, nil
	}
	if !rec.AdoptCheckedAt.IsZero() && now.Sub(rec.AdoptCheckedAt) < adoptCheckInterval {
		return 0, nil
	}
	store.MarkAdoptChecked(key, now)
	sup, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, rec.AdoptedBy)
	if err != nil {
		c.warn("stage labels: adoption superseder lookup failed", "repo", obs.Display, "pr", obs.Number, "superseded_by", rec.AdoptedBy, "error", err)
		return 0, err
	}
	switch {
	case sup.GetMerged():
		body := fmt.Sprintf("%s\nClosing: #%d (the hive's adoption of this PR, with @%s's commits) has merged. Thank you!", adoptionCloseMarker, rec.AdoptedBy, obs.Author)
		if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			c.warn("stage labels: adoption close note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
			return 0, err
		}
		if _, _, err := c.gh.PullRequests.Edit(ctx, obs.Owner, obs.Repo, obs.Number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil {
			c.warn("stage labels: closing adopted PR failed", "repo", obs.Display, "pr", obs.Number, "error", err)
			return 1, err
		}
		c.info("stage labels: adopted PR closed after superseder merged", "repo", obs.Display, "pr", obs.Number, "superseded_by", rec.AdoptedBy)
		return 1, nil
	case strings.EqualFold(sup.GetState(), "closed"):
		c.warn("stage labels: adoption superseder closed unmerged; returning PR to the contributor", "repo", obs.Display, "pr", obs.Number, "superseded_by", rec.AdoptedBy)
		store.ClearAdoption(key)
	}
	return 0, nil
}
