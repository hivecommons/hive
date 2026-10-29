package github

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// The signing step for the fixer's rebase of a contributor PR
// (hivecommons/hive#15182, follow-through of #15104).
//
// The rebase-then-merge follow-through counts a contributor PR's new head as
// the rebase only when CommitByHive says the hive committed it, and since
// #15182 an email-only pane-identity committer needs a GitHub-verified
// signature. The fixer rebases with plain git, so its head is unsigned by
// construction and would settle every such follow-through as foreign-head.
//
// SignFixerRebase seals that rebase as a GitHub-signed commit without
// re-authoring anybody. GitHub signs a commit the App creates through the
// REST API only when the request carries no custom author or committer, so
// the signed commit is the App's own: a merge commit whose first parent is
// the head the operator approved (the contributor's commits, byte for byte,
// with their own authorship and sign-offs) and whose second parent is the
// base commit the fixer rebased onto, carrying the fixer's resolved tree.
// The squash-attribution DCO check skips merge commits, and the PR diff
// (base...head) is the same as the fixer's rebase.
//
// Provenance comes from GitHub, never from the commit's own fields: the PR
// timeline must show the App bot force-pushing exactly this head over the
// approved one. A head a person pushed with the pane identity's address is
// refused, so the signing step cannot launder a forged committer into a
// verified one.

// fixerRebaseForcePushWindow is how many recent force-push events are read.
const fixerRebaseForcePushWindow = 20

// errFixerRebaseNotSigned marks a candidate head the hive refused to sign
// (wrong pusher, unexpected history). It wraps errSignedCommitSkip.
var errFixerRebaseNotSigned = fmt.Errorf("%w: fixer rebase not signed", errSignedCommitSkip)

// IsSignedHeadMoved reports whether err is the transient "head moved while
// the signed commit was being built" refusal; the caller retries next tick.
func IsSignedHeadMoved(err error) bool {
	var moved signedHeadMovedError
	return errors.As(err, &moved)
}

type fixerRebaseForcePush struct {
	Actor struct {
		Typename string `json:"__typename"`
		Login    string `json:"login"`
	} `json:"actor"`
	BeforeCommit struct {
		OID string `json:"oid"`
	} `json:"beforeCommit"`
	AfterCommit struct {
		OID string `json:"oid"`
	} `json:"afterCommit"`
}

type fixerRebasePullRequest struct {
	HeadRefName    string `json:"headRefName"`
	BaseRefName    string `json:"baseRefName"`
	HeadRepository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"headRepository"`
	TimelineItems struct {
		Nodes []fixerRebaseForcePush `json:"nodes"`
	} `json:"timelineItems"`
}

type fixerRebasePR struct {
	Repository struct {
		PullRequest fixerRebasePullRequest `json:"pullRequest"`
	} `json:"repository"`
}

const fixerRebasePRQuery = `query($owner: String!, $name: String!, $number: Int!, $last: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      headRefName
      baseRefName
      headRepository { nameWithOwner }
      timelineItems(last: $last, itemTypes: [HEAD_REF_FORCE_PUSHED_EVENT]) {
        nodes {
          ... on HeadRefForcePushedEvent {
            actor { __typename login }
            beforeCommit { oid }
            afterCommit { oid }
          }
        }
      }
    }
  }
}`

// SignFixerRebase seals head — the fixer's plain-git rebase of PR number,
// replacing prevHead (the approved head, or the last head the follow-through
// accepted) — as one GitHub-signed merge commit on the PR's head branch and
// returns its sha. ("", nil) means head is not a fixer rebase (its committer
// is not an unverified, email-only pane identity): nothing was written and
// CommitByHive decides as before. An error means a candidate the hive could
// not sign — a push it cannot attribute to the App, or a head repository
// (a contributor's fork) the App cannot write — and nothing was written; the
// follow-through then fails closed on CommitByHive's verdict.
func (c *Client) SignFixerRebase(ctx context.Context, repo string, number int, prevHead, head string) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	prevHead, head = strings.TrimSpace(prevHead), strings.TrimSpace(head)
	if head == "" || prevHead == "" || head == prevHead {
		return "", nil
	}
	owner, repoName := c.splitRepo(repo)
	ctx = WithRESTCaller(ctx, "hive:rebase_merge_sign")
	rc, _, err := c.client.Repositories.GetCommit(ctx, owner, repoName, head, nil)
	if err != nil {
		return "", fmt.Errorf("reading %s/%s@%s: %w", owner, repoName, shortSHA(head), err)
	}
	committerLogin := safeGetLogin(rc.GetCommitter())
	if committerLogin != "" || rc.GetCommit().GetVerification().GetVerified() || !c.hiveCommitIdentity("", rc.GetCommit().GetCommitter().GetEmail()) {
		return "", nil
	}
	tree := rc.GetCommit().GetTree().GetSHA()
	if tree == "" {
		return "", fmt.Errorf("reading %s/%s@%s: no tree", owner, repoName, shortSHA(head))
	}

	var pr fixerRebasePR
	if err := c.graphQL(ctx, fixerRebasePRQuery, map[string]any{"owner": owner, "name": repoName, "number": number, "last": fixerRebaseForcePushWindow}, &pr); err != nil {
		return "", fmt.Errorf("reading %s/%s#%d force-pushes: %w", owner, repoName, number, err)
	}
	p := pr.Repository.PullRequest
	if !c.fixerRebasePushedByHive(p, prevHead, head) {
		return "", fmt.Errorf("%w: GitHub shows no force-push of %s over %s by the App on %s/%s#%d", errFixerRebaseNotSigned, shortSHA(head), shortSHA(prevHead), owner, repoName, number)
	}
	headRef := strings.TrimSpace(p.HeadRefName)
	headOwner, headName, ok := strings.Cut(strings.TrimSpace(p.HeadRepository.NameWithOwner), "/")
	if headRef == "" || !ok || headOwner == "" || headName == "" {
		return "", fmt.Errorf("%w: %s/%s#%d has no readable head branch", errFixerRebaseNotSigned, owner, repoName, number)
	}

	cmp, _, err := c.client.Repositories.CompareCommits(ctx, owner, repoName, p.BaseRefName, head, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return "", fmt.Errorf("comparing %s...%s: %w", p.BaseRefName, shortSHA(head), err)
	}
	onto := cmp.GetMergeBaseCommit().GetSHA()
	if onto == "" || onto == prevHead {
		return "", fmt.Errorf("%w: no base commit under %s to merge", errFixerRebaseNotSigned, shortSHA(head))
	}

	ref, _, err := c.client.Git.GetRef(ctx, headOwner, headName, "heads/"+headRef)
	if err != nil {
		return "", fmt.Errorf("reading %s:%s: %w", headOwner+"/"+headName, headRef, err)
	}
	if got := ref.GetObject().GetSHA(); got != head {
		return "", signedHeadMovedError{fmt.Errorf("%w: %s moved from %s to %s before the fixer's rebase was signed", errSignedCommitSkip, headRef, shortSHA(head), shortSHA(got))}
	}
	// No Author / Committer: GitHub signs only an App commit without them.
	signed, _, err := c.client.Git.CreateCommit(ctx, headOwner, headName, &gh.Commit{
		Message: gh.Ptr(fixerRebaseSignedMessage(p.BaseRefName, headRef, number, prevHead, head, c.appBotLogin)),
		Tree:    &gh.Tree{SHA: gh.Ptr(tree)},
		Parents: []*gh.Commit{{SHA: gh.Ptr(prevHead)}, {SHA: gh.Ptr(onto)}},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("creating the signed commit in %s: %w", headOwner+"/"+headName, err)
	}
	if signed.GetSHA() == "" {
		return "", fmt.Errorf("creating the signed commit in %s: no sha returned", headOwner+"/"+headName)
	}
	if _, _, err := c.client.Git.UpdateRef(ctx, headOwner, headName, &gh.Reference{
		Ref:    gh.Ptr("refs/heads/" + headRef),
		Object: &gh.GitObject{SHA: gh.Ptr(signed.GetSHA())},
	}, true); err != nil {
		return "", fmt.Errorf("updating %s:%s to signed commit %s: %w", headOwner+"/"+headName, headRef, shortSHA(signed.GetSHA()), err)
	}
	return signed.GetSHA(), nil
}

// fixerRebasePushedByHive: the newest force-push to head replaced prevHead
// and was made by the App bot. GraphQL reports a Bot's login without the
// "[bot]" suffix REST uses, so both spellings match.
func (c *Client) fixerRebasePushedByHive(p fixerRebasePullRequest, prevHead, head string) bool {
	bot := strings.TrimSpace(c.appBotLogin)
	if bot == "" {
		return false
	}
	nodes := p.TimelineItems.Nodes
	for i := len(nodes) - 1; i >= 0; i-- {
		n := nodes[i]
		if n.AfterCommit.OID != head {
			continue
		}
		login := strings.TrimSpace(n.Actor.Login)
		byBot := strings.EqualFold(login, bot) || (n.Actor.Typename == "Bot" && strings.EqualFold(login+"[bot]", bot))
		return byBot && n.BeforeCommit.OID == prevHead
	}
	return false
}

func fixerRebaseSignedMessage(base, headRef string, number int, prevHead, head, bot string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Merge %s into %s\n\n", base, headRef)
	fmt.Fprintf(&b, "The hive's rebase of #%d onto %s (%s), signed by GitHub as a merge on top of the approved head %s so the contributor's commits stay theirs.", number, base, shortSHA(head), shortSHA(prevHead))
	if bot = strings.TrimSpace(bot); bot != "" {
		b.WriteString("\n\n" + BotSignedOffByTrailer(bot))
	}
	return b.String()
}
