package automerge

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stageengine"
)

const preparationMaxPredecessors = 3

var preparationReference = regexp.MustCompile(`#([1-9][0-9]*)|/(pull|issues)/([1-9][0-9]*)`)

// Missing evidence is different from a known pending verdict. Only the latter
// can yield priority to an independently admitted successor.
func (c *Engine) preparationReadiness(ctx context.Context, owner, repo string, pr *gh.PullRequest, lane string) (ready, known bool) {
	if lane != stageLaneSelf && lane != stageLaneTrusted {
		return false, false
	}
	green, reason, err := c.commitGreenForPR(ctx, owner, repo, pr.GetBase().GetRef(), pr.GetHead().GetSHA(), pr.GetNumber(), time.Time{}, newExpectedCheckCache())
	if err != nil {
		return false, false
	}
	if !green {
		return false, stageengine.ClassifyCIReason(reason) == stageengine.CIReasonPending && strings.HasPrefix(reason, "required-check-pending:")
	}
	if gate := c.currentReviewGate(); gate != nil && gate.effectiveMode() == ReviewGateModeEnforce {
		if gate.Verdict == nil {
			return false, false
		}
		verdict := gate.Verdict(owner+"/"+repo, pr.GetNumber(), pr.GetHead().GetSHA())
		if !verdict.Loaded || !verdict.Found {
			return false, false
		}
		return verdict.Approved, true
	}
	return true, true
}

func (c *Engine) preparationCompleted(ctx context.Context, owner, repo string, pr *gh.PullRequest, attempt preparationAttempt) bool {
	if !attempt.AuthoritativeBase || attempt.Head == "" || attempt.Base == "" || pr.GetHead().GetSHA() == attempt.Head {
		return false
	}
	for _, ancestor := range []string{attempt.Head, attempt.Base} {
		cmp, _, err := c.gh.Repositories.CompareCommits(ctx, owner, repo, ancestor, pr.GetHead().GetSHA(), &gh.ListOptions{PerPage: 1})
		if err != nil || (cmp.GetStatus() != "ahead" && cmp.GetStatus() != "identical") {
			return false
		}
	}
	return true
}

func (c *Engine) preparationFiles(ctx context.Context, owner, repo string, pr *gh.PullRequest) ([]string, error) {
	if pr.GetChangedFiles() <= 0 {
		return nil, fmt.Errorf("unknown changed-file count")
	}
	var files []string
	count := 0
	opts := &gh.ListOptions{PerPage: 100}
	for page := 0; page < mergeTrainFilesPages; page++ {
		list, resp, err := c.gh.PullRequests.ListFiles(ctx, owner, repo, pr.GetNumber(), opts)
		if err != nil {
			return nil, err
		}
		for _, f := range list {
			if f.GetFilename() == "" {
				return nil, fmt.Errorf("unknown changed path")
			}
			count++
			files = append(files, f.GetFilename())
			if f.GetPreviousFilename() != "" {
				files = append(files, f.GetPreviousFilename())
			}
		}
		if resp == nil || resp.NextPage == 0 {
			if count != pr.GetChangedFiles() {
				return nil, fmt.Errorf("incomplete changed paths")
			}
			return files, nil
		}
		opts.Page = resp.NextPage
	}
	return nil, fmt.Errorf("changed paths exceed bounded pagination")
}

func preparationClaims(pr *gh.PullRequest, fullRepo string) ([]int, bool) {
	text := pr.GetTitle() + "\n" + pr.GetBody()
	claims := hgithub.ParseClaimedIssues(text, fullRepo)
	if len(claims) == 0 || len(hgithub.ParseReferencedIssues(text, fullRepo)) != 0 {
		return nil, false
	}
	var numbers []int
	for _, ref := range claims {
		if ref.Repo != "" && !strings.EqualFold(ref.Repo, fullRepo) {
			return nil, false
		}
		numbers = append(numbers, ref.Issue)
	}
	// An additional reference is ambiguous, not an inferred dependency.
	// The existing native issue graph must explain the relationships.
	for _, match := range preparationReference.FindAllStringSubmatch(text, -1) {
		n, _ := strconv.Atoi(match[1])
		if match[2] == "issues" {
			n, _ = strconv.Atoi(match[3])
		}
		if match[2] == "pull" || !slices.Contains(numbers, n) {
			return nil, false
		}
	}
	return numbers, true
}

func (c *Engine) preparationIndependent(ctx context.Context, owner, repo string, a, b *gh.PullRequest) bool {
	fullRepo := owner + "/" + repo
	if mergeTrainAdoptionPair(a, b) || a.GetHead().GetSHA() == b.GetHead().GetSHA() {
		return false
	}
	ac, ak := preparationClaims(a, fullRepo)
	bc, bk := preparationClaims(b, fullRepo)
	if !ak || !bk {
		return false
	}
	for _, n := range ac {
		if slices.Contains(bc, n) {
			return false
		}
	}
	for _, n := range append(ac, bc...) {
		blocked, known, err := hgithub.ReadIssueDependencyEvidence(ctx, c.gh, owner, repo, n)
		if err != nil || !known || blocked {
			return false
		}
	}
	af, err := c.preparationFiles(ctx, owner, repo, a)
	if err != nil {
		return false
	}
	bf, err := c.preparationFiles(ctx, owner, repo, b)
	if err != nil {
		return false
	}
	for _, file := range af {
		if slices.Contains(bf, file) {
			return false
		}
	}
	cmp, _, err := c.gh.Repositories.CompareCommits(ctx, owner, repo, a.GetHead().GetSHA(), b.GetHead().GetSHA(), &gh.ListOptions{PerPage: 1})
	return err == nil && cmp.GetStatus() == "diverged"
}

func (c *Engine) preparationMayPass(ctx context.Context, owner, repo, base string, candidate, predecessor *gh.PullRequest, callerLane, lane string) bool {
	tip, _, err := c.gh.Repositories.GetBranch(ctx, owner, repo, base, 0)
	if err != nil || tip.GetCommit().GetSHA() == "" {
		return false
	}
	candidateReady, candidateKnown := c.preparationReadiness(ctx, owner, repo, candidate, callerLane)
	if !candidateReady || !candidateKnown {
		return false
	}
	ready, known := c.preparationReadiness(ctx, owner, repo, predecessor, lane)
	if !known || ready || !c.preparationIndependent(ctx, owner, repo, candidate, predecessor) {
		return false
	}
	// Paths and dependency reads are not a head/base admission proof.
	// Recheck both PR payloads, including mutable bodies and labels.
	for _, before := range []*gh.PullRequest{candidate, predecessor} {
		after, _, err := c.gh.PullRequests.Get(ctx, owner, repo, before.GetNumber())
		if err != nil || after.GetHead().GetSHA() != before.GetHead().GetSHA() ||
			after.GetBody() != before.GetBody() || after.GetTitle() != before.GetTitle() ||
			after.GetChangedFiles() != before.GetChangedFiles() || !slices.Equal(labelNames(after.Labels), labelNames(before.Labels)) ||
			c.preparationInvalidPR(after, base, false) != "" {
			return false
		}
	}
	ready, known = c.preparationReadiness(ctx, owner, repo, predecessor, lane)
	if !known || ready {
		return false
	}
	ac, ak := preparationClaims(candidate, owner+"/"+repo)
	bc, bk := preparationClaims(predecessor, owner+"/"+repo)
	if !ak || !bk {
		return false
	}
	for _, n := range append(ac, bc...) {
		blocked, known, err := hgithub.ReadIssueDependencyEvidence(ctx, c.gh, owner, repo, n)
		if err != nil || !known || blocked {
			return false
		}
	}
	candidateReady, candidateKnown = c.preparationReadiness(ctx, owner, repo, candidate, callerLane)
	liveTip, _, err := c.gh.Repositories.GetBranch(ctx, owner, repo, base, 0)
	return candidateReady && candidateKnown && err == nil && liveTip.GetCommit().GetSHA() == tip.GetCommit().GetSHA()
}
