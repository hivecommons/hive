package upstreamwatch

import (
	"context"
	"fmt"
	"net/http"

	gh "github.com/google/go-github/v72/github"
)

// ForkContents is the slice of the GitHub contents API applicability needs:
// a cheap "does this path exist on the fork's default branch" probe with no
// checkout. Tests substitute a fake.
type ForkContents interface {
	// Exists reports whether path exists on the fork's default branch. A
	// missing path returns (false, nil); only a transport or API fault
	// returns a non-nil error.
	Exists(ctx context.Context, path string) (bool, error)
}

// GitHubForkContents implements ForkContents over the GitHub REST API against
// the fork's default branch. It only reads.
type GitHubForkContents struct {
	client *gh.Client
	owner  string
	repo   string
}

// NewGitHubForkContents returns a ForkContents for owner/repo. Passing a nil
// ref to GetContents resolves against the repository's default branch, which
// is exactly the fork branch applicability checks against.
func NewGitHubForkContents(client *gh.Client, owner, repo string) *GitHubForkContents {
	return &GitHubForkContents{client: client, owner: owner, repo: repo}
}

// Exists implements ForkContents.
func (g *GitHubForkContents) Exists(ctx context.Context, path string) (bool, error) {
	_, _, resp, err := g.client.Repositories.GetContents(ctx, g.owner, g.repo, path, nil)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("check %s/%s path %q: %w", g.owner, g.repo, path, err)
	}
	return true, nil
}

// Applicable reports whether an item still applies to the fork in cheap mode:
// it is applicable when at least one of its touched files exists on the fork's
// default branch. The present files are returned so the renderer can show
// which paths matched. An item with no touched files (every release, and a PR
// whose files the source could not list) is treated as applicable, since
// there is nothing to rule it out.
func Applicable(ctx context.Context, fc ForkContents, item Item) (ok bool, present []string, err error) {
	if len(item.Files) == 0 {
		return true, nil, nil
	}
	for _, f := range item.Files {
		exists, err := fc.Exists(ctx, f)
		if err != nil {
			return false, nil, err
		}
		if exists {
			present = append(present, f)
		}
	}
	return len(present) > 0, present, nil
}

// Judgement is the full verdict on one upstream item: its class, a port
// difficulty estimate, whether it still applies to the fork, and the fork
// files it touches. The renderer prints these fields and the poll loop skips
// an item whose Applicable is false.
type Judgement struct {
	// Class is the kind of change.
	Class Class
	// Difficulty is the port-difficulty estimate.
	Difficulty Difficulty
	// Applicable reports whether the item still applies to the fork.
	Applicable bool
	// Present are the item's touched files that exist on the fork; empty for
	// a release or an item that was not applicable.
	Present []string
	// Reason explains why an item was judged not applicable; empty when it
	// is applicable.
	Reason string
}

// Judge combines classification, difficulty and applicability into one
// verdict for item. The contents probe only runs when the item carries files.
func Judge(ctx context.Context, fc ForkContents, item Item) (Judgement, error) {
	j := Judgement{
		Class:      Classify(item),
		Difficulty: EstimateDifficulty(item),
	}
	ok, present, err := Applicable(ctx, fc, item)
	if err != nil {
		return Judgement{}, err
	}
	j.Applicable = ok
	j.Present = present
	if !ok {
		j.Reason = "none of the upstream files exist on the fork's default branch"
	}
	return j, nil
}
