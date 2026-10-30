package github

import (
	"context"
	"fmt"
	"strings"
)

const integrationGroupMaxEntries = 100

// NativeIntegrationGroup is an observation of GitHub's documented queue prefix,
// not a queue-entry ID repurposed as a group ID and not a merge authorization.
type NativeIntegrationGroup struct {
	Repo    string
	Base    string
	BaseSHA string
	HeadRef string
	HeadSHA string
	Tree    string
	Members []NativeIntegrationMember
}

type NativeIntegrationMember struct {
	Entry      string
	Position   int
	PR         int
	Head       string
	BaseCommit string
	HeadCommit string
}

type nativeIntegrationQueue struct {
	Repository struct {
		MergeQueue *struct {
			Entries struct {
				PageInfo struct{ HasNextPage bool }
				Nodes    []struct {
					ID          string
					Position    int
					BaseCommit  struct{ OID string }
					HeadCommit  struct{ OID string }
					PullRequest struct {
						Number      int
						HeadRefOID  string
						BaseRefName string
					}
				}
			}
		}
	}
}

func (c *Client) ReadNativeIntegrationGroup(ctx context.Context, repo, base, baseSHA, headRef, headSHA string) (NativeIntegrationGroup, error) {
	out := NativeIntegrationGroup{Repo: repo, Base: base, BaseSHA: baseSHA, HeadRef: headRef, HeadSHA: headSHA}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || base == "" || !integrationOID(headSHA) || baseSHA != "" && !integrationOID(baseSHA) || !strings.HasPrefix(headRef, "refs/heads/gh-readonly-queue/"+base+"/") {
		return out, fmt.Errorf("native integration group identity incomplete")
	}
	var q nativeIntegrationQueue
	err := c.graphQL(ctx, `query($owner:String!,$name:String!,$branch:String!,$limit:Int!) {
 repository(owner:$owner,name:$name) { mergeQueue(branch:$branch) {
 entries(first:$limit) { pageInfo { hasNextPage } nodes {
 id position baseCommit { oid } headCommit { oid }
 pullRequest { number headRefOid baseRefName }
 } } } } }`, map[string]any{"owner": parts[0], "name": parts[1], "branch": base, "limit": integrationGroupMaxEntries}, &q)
	if err != nil {
		return out, err
	}
	queue := q.Repository.MergeQueue
	if queue == nil || queue.Entries.PageInfo.HasNextPage {
		return out, fmt.Errorf("native queue membership unavailable or exceeds bounded page")
	}
	// Bounded missed-event reconciliation can recover the root from the
	// authoritative queue chain, never from a run's author head or tree guess.
	if baseSHA == "" && len(queue.Entries.Nodes) > 0 {
		baseSHA = queue.Entries.Nodes[0].BaseCommit.OID
		out.BaseSHA = baseSHA
	}
	if !integrationOID(baseSHA) {
		return out, fmt.Errorf("native root SHA unavailable")
	}
	parent := baseSHA
	found := false
	for index, n := range queue.Entries.Nodes {
		if n.Position != index+1 || n.BaseCommit.OID != parent || n.PullRequest.BaseRefName != base ||
			n.ID == "" || n.PullRequest.Number < 1 || !integrationOID(n.PullRequest.HeadRefOID) || !integrationOID(n.HeadCommit.OID) {
			return out, fmt.Errorf("native queue prefix chain is incomplete or changed")
		}
		out.Members = append(out.Members, NativeIntegrationMember{Entry: n.ID, Position: n.Position, PR: n.PullRequest.Number, Head: n.PullRequest.HeadRefOID, BaseCommit: n.BaseCommit.OID, HeadCommit: n.HeadCommit.OID})
		parent = n.HeadCommit.OID
		if parent == headSHA {
			found = true
			break
		}
	}
	if !found {
		return out, fmt.Errorf("tested group is not an authoritative contiguous queue prefix")
	}
	commit, _, err := c.client.Git.GetCommit(ctx, parts[0], parts[1], headSHA)
	if err != nil {
		return out, err
	}
	out.Tree = commit.GetTree().GetSHA()
	if commit.GetSHA() != headSHA || !integrationOID(out.Tree) {
		return out, fmt.Errorf("native tested commit/tree unavailable")
	}
	ref, _, err := c.client.Git.GetRef(ctx, parts[0], parts[1], strings.TrimPrefix(headRef, "refs/"))
	if err != nil {
		return out, err
	}
	if ref.GetObject().GetSHA() != headSHA {
		return out, fmt.Errorf("native group synthetic ref changed during observation")
	}
	return out, nil
}
