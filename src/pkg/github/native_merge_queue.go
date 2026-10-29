package github

import (
	"context"
	"fmt"
)

// NativeMergeQueueEntry is GitHub's current queue position and provenance.
type NativeMergeQueueEntry struct {
	ID       string
	Position int
	State    string
	Enqueuer struct {
		Typename string `json:"__typename"`
		Login    string
	}
}

// NativeMergeQueueState describes the queue on a PR's target branch.
type NativeMergeQueueState struct {
	ID                  string
	HeadRefOid          string
	IsMergeQueueEnabled bool
	IsInMergeQueue      bool
	MergeQueueEntry     *NativeMergeQueueEntry
	TimelineItems       struct{ Nodes []struct{ ID string } }
}

// NativeMergeQueueAvailable excludes static tokens, notably workflow tokens:
// only App installation events reliably start merge_group workflows.
func (c *Client) NativeMergeQueueAvailable() bool { return c != nil && c.appAuth != nil }

func (c *Client) ReadNativeMergeQueue(ctx context.Context, id string) (NativeMergeQueueState, error) {
	var out struct{ Node *NativeMergeQueueState }
	err := c.graphQL(ctx, `query($id: ID!) { node(id: $id) { ... on PullRequest {
 id headRefOid isMergeQueueEnabled isInMergeQueue
 mergeQueueEntry { id position state enqueuer { __typename login } }
 timelineItems(last: 1, itemTypes: [REMOVED_FROM_MERGE_QUEUE_EVENT]) { nodes { ... on RemovedFromMergeQueueEvent { id } } }
 } } }`, map[string]any{"id": id}, &out)
	if err != nil {
		return NativeMergeQueueState{}, err
	}
	if out.Node == nil || out.Node.ID == "" {
		return NativeMergeQueueState{}, fmt.Errorf("merge queue: pull request %s unavailable", id)
	}
	return *out.Node, nil
}

// EnqueueNativePullRequest pins the exact head that passed Hive's gates.
func (c *Client) EnqueueNativePullRequest(ctx context.Context, id, head string) (*NativeMergeQueueEntry, error) {
	if !c.NativeMergeQueueAvailable() {
		return nil, fmt.Errorf("native merge queue requires App installation authentication")
	}
	var out struct {
		EnqueuePullRequest struct{ MergeQueueEntry *NativeMergeQueueEntry }
	}
	err := c.graphQL(ctx, `mutation($input: EnqueuePullRequestInput!) {
 enqueuePullRequest(input: $input) { mergeQueueEntry { id position state enqueuer { __typename login } } }
 }`, map[string]any{"input": map[string]any{"pullRequestId": id, "expectedHeadOid": head}}, &out)
	if err != nil {
		return nil, err
	}
	if out.EnqueuePullRequest.MergeQueueEntry == nil {
		return nil, fmt.Errorf("enqueue returned no merge queue entry")
	}
	return out.EnqueuePullRequest.MergeQueueEntry, nil
}

func (c *Client) DequeueNativePullRequest(ctx context.Context, id string) error {
	if !c.NativeMergeQueueAvailable() {
		return fmt.Errorf("native merge queue requires App installation authentication")
	}
	return c.graphQL(ctx, `mutation($input: DequeuePullRequestInput!) {
 dequeuePullRequest(input: $input) { clientMutationId }
 }`, map[string]any{"input": map[string]any{"pullRequestId": id}}, nil)
}

// NativeMergeQueueMethod reads the branch-wide merge method: enqueue has no
// per-PR method override, so mixed-authorship PRs must verify this first.
func (c *Client) NativeMergeQueueMethod(ctx context.Context, id, branch string) (string, error) {
	var out struct {
		Node struct {
			Repository struct {
				MergeQueue *struct{ Configuration struct{ MergeMethod string } }
			}
		}
	}
	err := c.graphQL(ctx, `query($id: ID!, $branch: String!) { node(id: $id) {
 ... on PullRequest { repository { mergeQueue(branch: $branch) { configuration { mergeMethod } } } }
 } }`, map[string]any{"id": id, "branch": branch}, &out)
	if err != nil {
		return "", err
	}
	if out.Node.Repository.MergeQueue == nil {
		return "", fmt.Errorf("merge queue configuration unavailable")
	}
	return out.Node.Repository.MergeQueue.Configuration.MergeMethod, nil
}
