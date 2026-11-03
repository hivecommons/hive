package automerge

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// A contributor PR the hive adopted can still be merged by a person. Nothing
// else closes the hive's adoption PR then: settleAdoption runs on the
// original only while it is open at 074 and reacts to the adoption PR
// merging or closing, never to the original merging. projectbluefin/utah#897
// (hive/adopt-885) stayed open after a person merged #885 at the same head,
// flipping between 070 and 085 as main moved, until a person closed it.
//
// The self-authored lane therefore checks an App PR on hive/adopt-<n>: when
// #n has merged at exactly this PR's head, the adoption PR carries nothing
// that is not already merged, and the lane closes it with one note. An
// adoption PR whose head differs (the hive added commits) stays open for a
// person: closing it could drop work.

// adoptionOriginalMergedMarker tags the note on an adoption PR closed
// because its original merged.
const adoptionOriginalMergedMarker = "<!-- hive:adoption-original-merged -->"

// adoptedOriginalNumber returns n when pr is an adoption PR for #n: its head
// is the hive/adopt-<n> branch in the base repository. Otherwise 0.
func adoptedOriginalNumber(pr *gh.PullRequest, owner, repo string) int {
	head := pr.GetHead()
	if head == nil || !strings.HasPrefix(head.GetRef(), adoptBranchPrefix) {
		return 0
	}
	if !strings.EqualFold(head.GetRepo().GetFullName(), owner+"/"+repo) {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(head.GetRef(), adoptBranchPrefix))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// adoptionOriginalCheckDue reports whether the adoption PR's original is due
// a lookup (at most every adoptCheckInterval per PR) and records the check.
func (c *Engine) adoptionOriginalCheckDue(owner, repo string, number int) bool {
	key := strings.ToLower(owner + "/" + repo + "#" + strconv.Itoa(number))
	now := c.clock()
	c.adoptionOriginalChecksMu.Lock()
	defer c.adoptionOriginalChecksMu.Unlock()
	if c.adoptionOriginalChecks == nil {
		c.adoptionOriginalChecks = make(map[string]time.Time)
	}
	if last, ok := c.adoptionOriginalChecks[key]; ok && now.Sub(last) < adoptCheckInterval {
		return false
	}
	c.adoptionOriginalChecks[key] = now
	return true
}

// closeAdoptionPRIfOriginalMerged closes pr, an App adoption PR, when its
// original merged at pr's head, and reports whether it did. A failed lookup
// or close is logged and leaves the PR to the lane until the next check.
func (c *Engine) closeAdoptionPRIfOriginalMerged(ctx context.Context, displayRepo, owner, repo string, pr *gh.PullRequest) bool {
	original := adoptedOriginalNumber(pr, owner, repo)
	number := pr.GetNumber()
	if original <= 0 || !c.adoptionOriginalCheckDue(owner, repo, number) {
		return false
	}
	restCtx := hgithub.WithRESTCaller(ctx, "hive:automerge_sweep")
	orig, _, err := c.gh.PullRequests.Get(restCtx, owner, repo, original)
	if err != nil {
		c.warn("automerge: adoption original lookup failed", "repo", displayRepo, "pr", number, "original", original, "error", err)
		return false
	}
	if !orig.GetMerged() {
		return false
	}
	headSHA := pr.GetHead().GetSHA()
	if headSHA == "" || !strings.EqualFold(orig.GetHead().GetSHA(), headSHA) {
		c.info("automerge: adoption original merged at a different head; left for a person", "repo", displayRepo, "pr", number, "original", original, "head", headSHA, "original_head", orig.GetHead().GetSHA())
		return false
	}
	item := writeback.Item{Number: number, IsPR: true, HiveAuthored: true, Author: pr.GetUser().GetLogin(), CreatedAt: pr.GetCreatedAt().Time}
	for _, label := range pr.Labels {
		item.Labels = append(item.Labels, label.GetName())
	}
	if d := c.writebackAllowed(owner+"/"+repo, item, writeback.KindSupersessionClose, "original merged at the adoption head"); !d.Allowed {
		c.info("automerge: adoption close withheld by write-back gate", "repo", displayRepo, "pr", number, "reason", d.Reason)
		return false
	}
	if _, _, err := c.gh.PullRequests.Edit(restCtx, owner, repo, number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil { // writeback:gated
		c.warn("automerge: closing adoption PR after its original merged failed", "repo", displayRepo, "pr", number, "original", original, "error", err)
		return false
	}
	c.info("automerge: closed adoption PR, its original merged at the same head", "repo", displayRepo, "pr", number, "original", original, "head", headSHA)
	comment := fmt.Sprintf("%s\nClosing: #%d, the PR this one adopted, was merged at the same head (`%s`), so this copy has nothing left to merge.", adoptionOriginalMergedMarker, original, shortSHA(headSHA))
	if d := c.writebackAllowed(owner+"/"+repo, item, writeback.KindSupersessionNotice, comment); !d.Allowed {
		c.info("automerge: adoption close note withheld by write-back gate", "repo", displayRepo, "pr", number, "reason", d.Reason)
		return true
	}
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindIssueComment,
		Target: strconv.Itoa(number),
		Actor:  "automerge",
		Inputs: map[string]string{"head_sha": headSHA, "body": effects.StableDigest(comment)},
	}, func(ctx context.Context) (effects.Result, error) {
		posted, _, apiErr := c.gh.Issues.CreateComment(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), owner, repo, number, &gh.IssueComment{Body: gh.Ptr(comment)}) // writeback:gated
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: posted.GetHTMLURL()}, nil
	})
	if err != nil {
		c.warn("automerge: adoption PR closed but its note failed", "repo", displayRepo, "pr", number, "original", original, "error", err)
	}
	return true
}
