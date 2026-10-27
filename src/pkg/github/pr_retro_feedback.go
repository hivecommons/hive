package github

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Trusted review feedback for the retro lane (hivecommons/hive#14584, part of
// #14551).
//
// The retro analyst only sees rework counts, so the best lesson it can write
// is "this PR needed rework", not why. RetroReviewFeedback returns what the
// trusted reviewers wrote on a reworked hive PR and a short summary of the
// rework that followed, for the retro lane to scan, budget and prompt with.
// It applies the live follow-up path's trust rules (filterHumanPRComments)
// but keeps resolved and outdated threads, and it never scans or trims for a
// prompt: that is the retro lane's job.

// ReviewFeedback is the trusted human review feedback on one PR plus the
// rework that followed its first review. ReworkFiles and ReworkSubjects carry
// file paths and commit subjects only, never diffs.
type ReviewFeedback struct {
	Comments       []PRComment `json:"comments"`
	ReworkFiles    []string    `json:"rework_files,omitempty"`
	ReworkSubjects []string    `json:"rework_subjects,omitempty"`
}

// ReviewFeedbackSource is what the retro lane takes as an injected source.
// ok is false when the PR does not qualify: no follow-up commits and no human
// change requests after its first review, or no trusted comment. The caller
// decides which PRs are hive-attributed.
type ReviewFeedbackSource interface {
	RetroReviewFeedback(ctx context.Context, repo string, number int, mergedAt time.Time) (fb ReviewFeedback, ok bool, err error)
}

var _ ReviewFeedbackSource = (*Client)(nil)

// prRetroCommitWindow bounds the PR commits the retro fetch reads to find
// the rework that followed the first review.
const prRetroCommitWindow = 100

type rawPRRetroCommit struct {
	Commit struct {
		OID             string    `json:"oid"`
		MessageHeadline string    `json:"messageHeadline"`
		AuthoredDate    time.Time `json:"authoredDate"`
		CommittedDate   time.Time `json:"committedDate"`
		Parents         struct {
			TotalCount int `json:"totalCount"`
			Nodes      []struct {
				OID string `json:"oid"`
			} `json:"nodes"`
		} `json:"parents"`
	} `json:"commit"`
}

type rawPRRetroConversation struct {
	rawPRConversation
	Commits struct {
		Nodes []rawPRRetroCommit `json:"nodes"`
	} `json:"commits"`
}

// prRetroConversationQuery is prConversationQuery with the same comment
// windows plus the PR's commits, so the comment fetch also locates the rework
// range and the summary needs only one more call.
var prRetroConversationQuery = fmt.Sprintf(`query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      comments(last: %d) { nodes { databaseId url body createdAt authorAssociation author { __typename login } } }
      reviews(last: %d) { nodes { databaseId url body createdAt authorAssociation state author { __typename login } } }
      reviewThreads(last: %d) {
        nodes {
          isResolved isOutdated path line
          comments(last: %d) { nodes { databaseId url body createdAt authorAssociation author { __typename login } } }
        }
      }
      commits(last: %d) {
        nodes { commit { oid messageHeadline authoredDate committedDate parents(first: 2) { totalCount nodes { oid } } } }
      }
    }
  }
}`, prCommentsConversationWindow, prCommentsReviewWindow, prCommentsThreadWindow, prCommentsPerThreadWindow, prRetroCommitWindow)

// RetroReviewFeedback returns the trusted review feedback on repo#number and
// the rework after its first review. The GitHub cost is bounded: rework counts
// come from the rework cache (computed once with the existing rework fetch on
// a miss); a PR without rework makes no further call; otherwise one comment
// fetch, and only when a trusted comment survives, one compare call for the
// files the rework touched.
func (c *Client) RetroReviewFeedback(ctx context.Context, repo string, number int, mergedAt time.Time) (ReviewFeedback, bool, error) {
	if c == nil {
		return ReviewFeedback{}, false, ErrNoGitHubClient
	}
	if strings.TrimSpace(repo) == "" || number <= 0 {
		return ReviewFeedback{}, false, nil
	}
	rework, err := c.cachedPRRework(ctx, repo, number, mergedAt)
	if err != nil {
		return ReviewFeedback{}, false, err
	}
	if !retroReworkQualifies(rework) {
		return ReviewFeedback{}, false, nil
	}
	owner, name := c.splitRepo(repo)
	var data struct {
		Repository struct {
			PullRequest *rawPRRetroConversation `json:"pullRequest"`
		} `json:"repository"`
	}
	vars := map[string]any{"owner": owner, "repo": name, "number": number}
	if err := c.graphQL(ctx, prRetroConversationQuery, vars, &data); err != nil {
		return ReviewFeedback{}, false, err
	}
	pr := data.Repository.PullRequest
	if pr == nil {
		return ReviewFeedback{}, false, fmt.Errorf("%s/%s#%d: pull request not found", owner, name, number)
	}
	comments := filterRetroPRComments(pr.rawPRConversation, time.Time{}, c.isHumanFeedbackAuthor)
	if len(comments) == 0 {
		return ReviewFeedback{}, false, nil
	}
	fb := ReviewFeedback{Comments: comments}
	var base, head string
	fb.ReworkSubjects, base, head = retroReworkCommits(pr.Commits.Nodes, rework.FirstReviewAt, mergedAt)
	if base != "" && head != "" {
		files, err := c.retroReworkFiles(ctx, owner, name, base, head)
		if err != nil {
			// The comments are the point of the feedback; a missing file
			// list only thins the summary.
			c.logger.Warn("retro review feedback: could not list rework files",
				"repo", repo, "number", number, "error", err)
		}
		fb.ReworkFiles = files
	}
	return fb, true, nil
}

// retroReworkQualifies is the rework half of B1: follow-up commits or human
// change requests after the first review.
func retroReworkQualifies(rework PRReworkStats) bool {
	return !rework.FirstReviewAt.IsZero() && (rework.FollowUpCommits > 0 || rework.HumanChangeRequests > 0)
}

// retroReworkCommits returns the subjects of the non-merge commits authored
// after the first review (and not after the merge), oldest first, plus the
// compare range covering them: the first such commit's parent and the last
// such commit. Merge-from-base commits are maintenance, not rework. When a
// merge commit sits inside that range the range is dropped (empty base and
// head): a compare across it would also list every file the base branch
// gained through the merge, so no file list beats a misleading one.
func retroReworkCommits(commits []rawPRRetroCommit, firstReview, mergedAt time.Time) (subjects []string, base, head string) {
	var mergeAfterBase, mergeInRange bool
	for _, rc := range commits {
		cm := rc.Commit
		at := cm.AuthoredDate
		if at.IsZero() {
			at = cm.CommittedDate
		}
		if cm.Parents.TotalCount > 1 {
			mergeAfterBase = mergeAfterBase || base != ""
			continue
		}
		if !commitAfterFirstReview(firstReview, mergedAt, at) {
			continue
		}
		if base == "" {
			if len(cm.Parents.Nodes) == 0 || cm.Parents.Nodes[0].OID == "" {
				continue
			}
			base = cm.Parents.Nodes[0].OID
		}
		head = cm.OID
		mergeInRange = mergeInRange || mergeAfterBase
		if subject := strings.TrimSpace(cm.MessageHeadline); subject != "" {
			subjects = append(subjects, subject)
		}
	}
	if mergeInRange {
		return subjects, "", ""
	}
	return subjects, base, head
}

// retroReworkFiles lists the paths changed between base and head. Patches in
// the compare response are ignored.
func (c *Client) retroReworkFiles(ctx context.Context, owner, repo, base, head string) ([]string, error) {
	if c.client == nil {
		return nil, ErrNoGitHubClient
	}
	cmp, _, err := c.client.Repositories.CompareCommits(WithRESTCaller(ctx, "hive:retro_review_feedback"), owner, repo, base, head, nil)
	if err != nil {
		return nil, fmt.Errorf("comparing %s/%s %s...%s: %w", owner, repo, base, head, err)
	}
	var files []string
	for _, f := range cmp.Files {
		if name := strings.TrimSpace(f.GetFilename()); name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}
