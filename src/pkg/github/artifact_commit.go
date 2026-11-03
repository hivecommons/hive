package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/effects"
)

// Artifact-repo commit helpers (RFC #11585 part 8, #11701).
//
// The Nous artifact mirror (pkg/nousartifacts) needs four small primitives
// the client did not expose: read a branch head, create a branch, write a
// set of files as one commit, and ask whether a repository is private. They
// all ride the client's own transport, so ETag caching, rate-limit
// accounting and the App identity apply exactly as for every other call.
// The commit is made with createCommitOnBranch, so GitHub signs it as the
// hive's App — no agent token and no owner token is ever involved.

// ArtifactFile is one file written by CommitFilesOnBranch. Path is relative
// to the repository root with forward slashes.
type ArtifactFile struct {
	Path    string
	Content []byte
}

// ErrBranchNotFound is returned by BranchHead when the ref does not exist.
var ErrBranchNotFound = errors.New("branch not found")

// BranchHead returns the commit sha a branch points at. exists is false (and
// err nil) when the branch does not exist.
func (c *Client) BranchHead(ctx context.Context, repo, branch string) (sha string, exists bool, err error) {
	if c == nil || c.client == nil {
		return "", false, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	ref, resp, err := c.client.Git.GetRef(ctx, owner, name, "heads/"+branch)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, fmt.Errorf("reading ref heads/%s in %s/%s: %w", branch, owner, name, err)
	}
	sha = ref.GetObject().GetSHA()
	if sha == "" {
		return "", false, fmt.Errorf("reading ref heads/%s in %s/%s: empty sha", branch, owner, name)
	}
	return sha, true, nil
}

// CreateBranch creates refs/heads/<branch> at fromSHA. It is a branch-push
// effect for the mutation boundary.
func (c *Client) CreateBranch(ctx context.Context, repo, branch, fromSHA string) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	_, err := effects.Execute(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + name,
		Kind:   effects.KindBranchPush,
		Target: branch,
		Inputs: map[string]string{"from_sha": fromSHA},
	}, func(ctx context.Context) (effects.Result, error) {
		_, _, apiErr := c.client.Git.CreateRef(ctx, owner, name, &gh.Reference{
			Ref:    gh.Ptr("refs/heads/" + branch),
			Object: &gh.GitObject{SHA: gh.Ptr(fromSHA)},
		})
		if apiErr != nil {
			return effects.Result{}, apiErr
		}
		return effects.Result{Provenance: owner + "/" + name + "@" + branch}, nil
	})
	if err != nil {
		return fmt.Errorf("creating branch %s at %s in %s/%s: %w", branch, shortSHA(fromSHA), owner, name, err)
	}
	return nil
}

// CommitFilesOnBranch writes files as one GitHub-signed commit on branch,
// which must currently point at expectedHeadOID. It returns the new commit
// sha. Existing paths are overwritten; nothing is deleted.
func (c *Client) CommitFilesOnBranch(ctx context.Context, repo, branch, expectedHeadOID, headline, body string, files []ArtifactFile) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	if len(files) == 0 {
		return "", errors.New("CommitFilesOnBranch: no files")
	}
	owner, name := c.splitRepo(repo)
	changes := signedFileChanges{Additions: make([]signedFileAddition, 0, len(files))}
	for _, f := range files {
		p := strings.TrimPrefix(strings.TrimSpace(f.Path), "/")
		if p == "" {
			return "", errors.New("CommitFilesOnBranch: empty file path")
		}
		changes.Additions = append(changes.Additions, signedFileAddition{Path: p, Contents: base64.StdEncoding.EncodeToString(f.Content)})
	}
	var oid string
	_, err := effects.Execute(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + name,
		Kind:   effects.KindBranchPush,
		Target: branch,
		Inputs: map[string]string{"expected_head": expectedHeadOID, "headline": effects.StableDigest(headline)},
	}, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		oid, apiErr = c.createCommitOnBranch(ctx, owner+"/"+name, branch, expectedHeadOID, headline, body, changes)
		if apiErr != nil {
			return effects.Result{}, apiErr
		}
		return effects.Result{Provenance: oid}, nil
	})
	if err != nil {
		return "", fmt.Errorf("committing %d file(s) on %s in %s/%s: %w", len(files), branch, owner, name, err)
	}
	return oid, nil
}

// RepoIsPrivate reports whether the repository is private. The artifact
// mirror warns (never blocks) when the destination is public.
func (c *Client) RepoIsPrivate(ctx context.Context, repo string) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	r, _, err := c.client.Repositories.Get(ctx, owner, name)
	if err != nil {
		return false, fmt.Errorf("reading repository %s/%s: %w", owner, name, err)
	}
	return r.GetPrivate(), nil
}
