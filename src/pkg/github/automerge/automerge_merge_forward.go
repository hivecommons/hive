package automerge

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

// MergeForwardConflictMarkerPrefix opens the one note a trusted human author
// gets when the hive tried to merge the base into their branch and git hit a
// conflict (hivecommons/hive#13636); the head keeps it idempotent per push.
const MergeForwardConflictMarkerPrefix = "<!-- hive:merge-forward-conflict"

// mergeForwardConflictFilesMax bounds the file list in the conflict note.
const mergeForwardConflictFilesMax = 20

func (c *Engine) mergeForwardTrustedHumanFor(repo string) bool {
	if c == nil || c.mergeForwardTrustedHuman == nil {
		return false
	}
	return c.mergeForwardTrustedHuman(repo)
}

// mergeForwardKey namespaces the per-head attempt in heldRunApprovals so it
// never collides with the rebase-nudge or bot-takeover entries.
func mergeForwardKey(prKey, headSHA string) string {
	return prKey + "@" + headSHA + "#merge-forward"
}

func (c *Engine) markMergeForwardAttempted(owner, repo string, number int, headSHA string) bool {
	key := mergeForwardKey(c.evaluatedHeadKey(owner, repo, number), headSHA)
	c.heldRunApprovalsMu.Lock()
	defer c.heldRunApprovalsMu.Unlock()
	if c.heldRunApprovals == nil {
		c.heldRunApprovals = make(map[string]bool)
	}
	if c.heldRunApprovals[key] {
		return false
	}
	c.heldRunApprovals[key] = true
	return true
}

// mergeForwardOrConflict handles a trusted-author PR GitHub reports as
// dirty. When auto_merge.merge_forward_trusted_human lists the repo and the
// PR is an `author/human` same-repo branch, the base is merged into the head
// as a new App-authored, signed-off merge commit (the GitHub merges API; no
// human commit is rewritten) and the merge waits for CI on the new head. A
// real conflict leaves the branch untouched and posts one note naming the
// files both sides changed. Every other case keeps the "conflicting" hold.
// It is attempted once per head SHA.
func (c *Engine) mergeForwardOrConflict(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest, number int) (AutoMergeSweepEvent, string, error) {
	if !c.mergeForwardTrustedHumanFor(owner+"/"+repo) || pr == nil || c.isForkPR(pr) {
		return AutoMergeSweepEvent{}, "conflicting", nil
	}
	if authorkind.FromLabels(labelNames(pr.Labels), c.stagePolicy().AuthorKindLabels) != authorkind.Human {
		return AutoMergeSweepEvent{}, "conflicting", nil
	}
	headRef := strings.TrimSpace(pr.GetHead().GetRef())
	baseRef := strings.TrimSpace(pr.GetBase().GetRef())
	headSHA := strings.TrimSpace(pr.GetHead().GetSHA())
	appLogin := ""
	if c.transport != nil {
		appLogin = strings.TrimSpace(c.transport.AppBotLogin())
	}
	if headRef == "" || baseRef == "" || headSHA == "" || appLogin == "" {
		return AutoMergeSweepEvent{}, "conflicting", nil
	}
	if c.requireCurrentBaseFor(owner + "/" + repo) {
		if reason, err := c.admitBasePreparation(ctx, owner, repo, baseRef, number, headSHA, stageLaneTrusted, true); reason != "" || err != nil {
			if err != nil {
				return AutoMergeSweepEvent{}, "merge-forward", err
			}
			return AutoMergeSweepEvent{}, reason, nil
		}
		if err := c.claimBasePreparationUpdate(ctx, owner, repo, number, headSHA); err != nil {
			// A conflicting outsider belongs to the existing repair path,
			// not to final preparation or a ready merge-train seat.
			if wait := preparationWaitReason(err); wait != "" {
				return AutoMergeSweepEvent{}, wait, nil
			}
			return AutoMergeSweepEvent{}, "merge-forward", err
		}
	}
	if !c.markMergeForwardAttempted(owner, repo, number, headSHA) {
		return AutoMergeSweepEvent{}, "conflicting", nil
	}
	message := mergeForwardCommitMessage(baseRef, headRef, number, appLogin)
	var status int
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindBranchPush,
		Target: headRef,
		Actor:  "automerge",
		Inputs: map[string]string{"pr": strconv.Itoa(number), "head_sha": headSHA, "base": baseRef, "reason": "merge-forward"},
	}, func(ctx context.Context) (effects.Result, error) {
		commit, resp, apiErr := c.gh.Repositories.Merge(hgithub.WithRESTCaller(ctx, "hive:automerge_merge_forward"), owner, repo, &gh.RepositoryMergeRequest{
			Base:          gh.Ptr(headRef),
			Head:          gh.Ptr(baseRef),
			CommitMessage: gh.Ptr(message),
		})
		if resp != nil {
			status = resp.StatusCode
		}
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: commit.GetSHA()}, nil
	})
	switch {
	case err == nil && status == http.StatusNoContent:
		// Nothing to merge: the head already contains the base, so GitHub's
		// dirty state is stale. Wait for it to recompute.
		c.info("automerge merge-forward found nothing to merge", "repo", displayRepo, "pr", number, "head", headSHA)
		return AutoMergeSweepEvent{}, "check-pending", nil
	case err == nil:
		c.info("automerge merged the base forward into a trusted human branch", "repo", displayRepo, "pr", number, "base", baseRef, "head", headSHA)
		return AutoMergeSweepEvent{Repo: displayRepo, Number: number, HeadSHA: headSHA, BranchUpdated: true}, "check-pending", nil
	case status == http.StatusConflict:
		files := c.mergeForwardConflictFiles(ctx, owner, repo, baseRef, headSHA)
		c.postMergeForwardConflictNote(ctx, displayRepo, owner, repo, number, hgithub.SafeGetLogin(pr.GetUser()), headSHA, baseRef, files)
		return AutoMergeSweepEvent{}, "conflicting", nil
	case status == http.StatusForbidden || status == http.StatusNotFound:
		// TODO(hivecommons/hive#14589): a 403 on a same-repo branch is the
		// App lacking contents: write (markUnauthorized, step
		// merge-forward); on a fork head it is the fork's "allow edits by
		// maintainers" setting, which no App permission cures. Left as a
		// skip until the two are separated.
		c.info("automerge merge-forward skipped: branch not writable by the app", "repo", displayRepo, "pr", number, "status", status)
		return AutoMergeSweepEvent{}, "conflicting", nil
	}
	return AutoMergeSweepEvent{}, "merge-forward", err
}

// mergeForwardCommitMessage is the merge commit's message with the App's DCO
// sign-off.
func mergeForwardCommitMessage(baseRef, headRef string, number int, appLogin string) string {
	return fmt.Sprintf("Merge %s into %s\n\nMerge-forward by the hive App so #%d is current with its base; no author commit is rewritten.\n\n%s",
		baseRef, headRef, number, hgithub.BotSignedOffByTrailer(appLogin))
}

// mergeForwardConflictFiles returns the sorted files changed on both sides
// since the merge base, the likely conflicts. Best effort: nil on any error.
func (c *Engine) mergeForwardConflictFiles(ctx context.Context, owner, repo, baseRef, headSHA string) []string {
	restCtx := hgithub.WithRESTCaller(ctx, "hive:automerge_merge_forward")
	headSide, _, err := c.gh.Repositories.CompareCommits(restCtx, owner, repo, baseRef, headSHA, nil)
	if err != nil || headSide.GetMergeBaseCommit().GetSHA() == "" {
		return nil
	}
	baseSide, _, err := c.gh.Repositories.CompareCommits(restCtx, owner, repo, headSide.GetMergeBaseCommit().GetSHA(), baseRef, nil)
	if err != nil {
		return nil
	}
	ours := map[string]bool{}
	for _, f := range headSide.Files {
		ours[f.GetFilename()] = true
	}
	var both []string
	for _, f := range baseSide.Files {
		if name := f.GetFilename(); name != "" && ours[name] {
			both = append(both, name)
		}
	}
	sort.Strings(both)
	return both
}

// MergeForwardConflictComment renders the note a trusted human author gets
// when merging the base into their branch conflicts.
func MergeForwardConflictComment(author, headSHA, baseRef string, files []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s -->\n", MergeForwardConflictMarkerPrefix, headSHA)
	fmt.Fprintf(&b, "@%s this PR conflicts with `%s`. The hive tried to merge `%s` into your branch and git stopped on a conflict, so nothing was pushed.\n\n", author, baseRef, baseRef)
	if len(files) > 0 {
		b.WriteString("**Files changed on both sides:**\n")
		for i, f := range files {
			if i == mergeForwardConflictFilesMax {
				fmt.Fprintf(&b, "- … and %d more\n", len(files)-i)
				break
			}
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Next: merge or rebase onto `%s`, resolve the conflict and push; CI and the review swarm re-run automatically.\n", baseRef)
	return b.String()
}

func (c *Engine) postMergeForwardConflictNote(ctx context.Context, displayRepo, owner, repo string, number int, author, headSHA, baseRef string, files []string) {
	if author == "" {
		return
	}
	body := MergeForwardConflictComment(author, headSHA, baseRef, files)
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"head_sha": headSHA, "body": effects.StableDigest(MergeForwardConflictMarkerPrefix, headSHA)},
	}, func(ctx context.Context) (effects.Result, error) {
		posted, _, apiErr := c.gh.Issues.CreateComment(hgithub.WithRESTCaller(ctx, "hive:automerge_merge_forward"), owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)})
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: posted.GetHTMLURL()}, nil
	})
	if err != nil {
		c.warn("automerge merge-forward conflict note failed", "repo", displayRepo, "pr", number, "error", err)
	}
}
