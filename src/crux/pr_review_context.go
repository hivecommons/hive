package crux

import (
	"context"
	"time"
)

// PRReviewContext is what the host's GitHub client knows about one pull
// request a sandboxed reviewer is about to judge (hivecommons/hive#18513).
// A sandbox Job holds no GitHub credential, so `gh pr view` and `gh pr diff`
// fail inside it; the executor writes this snapshot into the workspace
// instead. Every free-text field (title, body, comment bodies, the diff) is
// written by the PR author or a commenter and is untrusted data.
type PRReviewContext struct {
	Number  int    `json:"number"`
	URL     string `json:"url,omitempty"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	Author  string `json:"author"`
	State   string `json:"state,omitempty"`
	Draft   bool   `json:"draft,omitempty"`
	BaseRef string `json:"base_ref"`
	BaseSHA string `json:"base_sha"`
	HeadRef string `json:"head_ref,omitempty"`
	HeadSHA string `json:"head_sha"`
	// HeadRepo is the full name of the repository the head branch lives in.
	// It differs from the base repository for a fork PR; refs/pull/N/head
	// works either way.
	HeadRepo string          `json:"head_repo,omitempty"`
	Fork     bool            `json:"fork,omitempty"`
	Labels   []string        `json:"labels"`
	Files    []PRChangedFile `json:"changed_files"`
	// FilesTruncated is true when the PR changes more files than the client
	// lists.
	FilesTruncated bool `json:"changed_files_truncated,omitempty"`
	// Diff is the unified diff as GitHub renders it. DiffUnavailable names
	// why it is empty (GitHub refuses very large diffs); the reviewer can
	// still run git diff against the checked-out refs.
	Diff            string            `json:"-"`
	DiffUnavailable string            `json:"-"`
	Comments        []PRReviewComment `json:"comments"`
}

// PRChangedFile is one file a PR changes.
type PRChangedFile struct {
	Path         string `json:"path"`
	PreviousPath string `json:"previous_path,omitempty"`
	Status       string `json:"status,omitempty"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
}

// PRReviewComment is one conversation comment, review body or inline review
// thread comment on a PR. Every comment is carried, whoever wrote it, with
// its author association so a reviewer can weigh it; all of it is untrusted.
type PRReviewComment struct {
	Kind              string    `json:"kind"`
	Author            string    `json:"author"`
	AuthorAssociation string    `json:"author_association,omitempty"`
	Body              string    `json:"body"`
	State             string    `json:"state,omitempty"`
	Path              string    `json:"path,omitempty"`
	Line              int       `json:"line,omitempty"`
	Thread            int       `json:"thread,omitempty"`
	Resolved          bool      `json:"resolved,omitempty"`
	Outdated          bool      `json:"outdated,omitempty"`
	URL               string    `json:"url,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
}

// PRReviewContextReader is the optional read capability a host's PR client
// offers alongside PRCreator. The sandbox executor type-asserts the PR client
// it was given; a client without it still gets the PR head checked out, with
// a metadata file that says the metadata was unavailable.
type PRReviewContextReader interface {
	PRReviewContext(ctx context.Context, repo string, number int) (PRReviewContext, error)
}
