package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Signed commits for agent PRs (github.app_signed_commits).
//
// Agents commit with plain git in their pane and push over the credential
// helper. Those commits cannot be signed: a GitHub App has no account to hold
// a GPG or SSH key, and the agent pane has no key of its own. A base branch
// with a `required_signatures` ruleset therefore blocks every agent PR from
// merging no matter who approves it — observed on a hive whose eight open,
// approved, green PRs all sat at mergeable_state "blocked".
//
// GitHub does sign commits it creates itself: commits authored through the
// createCommitOnBranch GraphQL mutation "are automatically GPG signed and are
// marked as verified", and "GitHub Apps can use the mutation to author commits
// directly" (GitHub changelog, 2021-09-13). So the PR-request watcher — the one
// server-side choke point every agent PR already passes through, holding the
// App installation token — re-authors the head branch through that mutation
// before it opens the PR:
//
//  1. compare base...head: the changed files and the agents' commits;
//  2. create a scratch ref at the merge base, one createCommitOnBranch on it
//     with every addition/deletion and the agents' original messages (DCO
//     trailers included) as the message;
//  3. force-update head to the new commit and delete the scratch ref.
//
// The result is one commit, signed by GitHub, authored by "<slug>[bot]", whose
// tree is byte-for-byte the tree the agent pushed — so every gate that ran
// before this step (claims, content metadata, outreach) judged exactly what
// lands, and the duplicate-tree guard in CreatePR still recognises it.
//
// WHAT IT REFUSES, and falls back on. The mutation expresses file contents
// only: no modes, so an executable bit, a symlink, or a submodule pointer in
// the change cannot be reproduced; and a request is one HTTP body, so a very
// large change cannot ride in it. Those, a head that moved between compare and
// update (the agent pushed again), and any API failure all skip the rewrite —
// the head branch is left exactly as the agent pushed it (the scratch ref is
// the only thing touched until the final update), the PR opens on the agent's
// own commits, and the reason lands in the result file and the log. The PR is
// never blocked by this step.

// signedCommitResult reports what reauthorBranchSigned did.
type signedCommitResult struct {
	// OID is the new signed commit's sha; empty when Skipped is set.
	OID string
	// Replaced is how many agent commits the signed commit stands in for.
	Replaced int
	// Skipped explains why the branch was left as pushed; empty on success.
	Skipped string
}

// signedCommitMaxBytes caps the decoded size of all blobs sent in one
// createCommitOnBranch request. GitHub accepts far larger GraphQL bodies than
// an agent test PR ever needs; the cap keeps a runaway generated file from
// turning into a multi-minute upload on the watcher goroutine.
const signedCommitMaxBytes = 20 << 20

// signedCommitScratchPrefix names the throwaway ref the signed commit is built
// on. Under refs/heads so createCommitOnBranch (which only takes a branch) can
// target it; namespaced so it can never collide with an agent's own branch and
// is obvious in `git ls-remote` if a crash ever leaves one behind.
const signedCommitScratchPrefix = "hive-signed/"

// errSignedCommitSkip marks a deliberate fall-back (unsupported change, moved
// head) as opposed to an API failure. Both skip; only the message differs.
var errSignedCommitSkip = errors.New("signed commit skipped")

// SetSignedCommits installs the toggle read before every PR-open request. nil
// (or a func returning false) leaves branches exactly as the agent pushed them.
func (c *Client) SetSignedCommits(fn func() bool) {
	if c == nil {
		return
	}
	c.prSignedCommits = fn
}

// reauthorBranchSigned rewrites req.Head to a single GitHub-signed commit
// carrying the same tree and the agents' messages. It never returns an error:
// a failure of any kind is a Skipped result and the branch is untouched.
func (c *Client) reauthorBranchSigned(ctx context.Context, req PRRequest) signedCommitResult {
	if c == nil || c.client == nil {
		return signedCommitResult{Skipped: "no GitHub client"}
	}
	owner, repo := c.splitRepo(strings.TrimSpace(req.Repo))
	head := strings.TrimSpace(req.Head)
	base := strings.TrimSpace(req.Base)
	if base == "" {
		resolved, err := c.DefaultBranch(ctx, owner, repo)
		if err != nil {
			return signedCommitResult{Skipped: "resolving the base branch: " + err.Error()}
		}
		base = resolved
	}
	// A branch that already has an open PR is someone's review in progress; a
	// force-update under it is not this step's call. CreatePR will reuse that
	// PR exactly as before.
	if existing, err := c.findOpenPRForHead(ctx, owner, repo, head); err == nil && existing != nil {
		return signedCommitResult{Skipped: fmt.Sprintf("open PR #%d already exists for head %s; left the branch as pushed", existing.GetNumber(), head)}
	}
	oid, replaced, err := c.signBranch(ctx, owner, repo, base, head)
	if err != nil {
		return signedCommitResult{Skipped: err.Error()}
	}
	return signedCommitResult{OID: oid, Replaced: replaced}
}

// signBranch does the work of reauthorBranchSigned once the inputs are
// resolved. Errors wrapping errSignedCommitSkip are deliberate refusals; any
// other error is an API failure. Either way the caller falls back.
func (c *Client) signBranch(ctx context.Context, owner, repo, base, head string) (oid string, replaced int, err error) {
	comparison, _, err := c.client.Repositories.CompareCommits(ctx, owner, repo, base, head, nil)
	if err != nil {
		return "", 0, fmt.Errorf("comparing %s...%s: %w", base, head, c.diagnoseCompareFailure(ctx, owner, repo, base, head, err))
	}
	if comparison == nil || comparison.GetMergeBaseCommit().GetSHA() == "" {
		return "", 0, fmt.Errorf("comparing %s...%s: GitHub returned no merge base", base, head)
	}
	if len(comparison.Commits) == 0 || len(comparison.Files) == 0 {
		return "", 0, fmt.Errorf("%w: %s has no changes against %s", errSignedCommitSkip, head, base)
	}
	if len(comparison.Files) >= githubCompareFileLimit {
		return "", 0, fmt.Errorf("%w: the change touches %d or more files, past the compare API's file list", errSignedCommitSkip, githubCompareFileLimit)
	}
	mergeBase := comparison.GetMergeBaseCommit().GetSHA()

	headRef, _, err := c.client.Git.GetRef(ctx, owner, repo, "heads/"+head)
	if err != nil {
		return "", 0, fmt.Errorf("reading ref heads/%s: %w", head, err)
	}
	headSHA := headRef.GetObject().GetSHA()
	if headSHA == "" {
		return "", 0, fmt.Errorf("reading ref heads/%s: empty sha", head)
	}

	changes, err := c.collectSignedFileChanges(ctx, owner, repo, headSHA, comparison.Files)
	if err != nil {
		return "", 0, err
	}
	headline, body := signedCommitMessage(comparison.Commits)

	// Scratch ref at the merge base. Delete a leftover from a crashed run
	// first: CreateRef 422s on an existing ref.
	scratch := signedCommitScratchPrefix + head
	_, _ = c.client.Git.DeleteRef(ctx, owner, repo, "heads/"+scratch)
	if _, _, err := c.client.Git.CreateRef(ctx, owner, repo, &gh.Reference{
		Ref:    gh.Ptr("refs/heads/" + scratch),
		Object: &gh.GitObject{SHA: gh.Ptr(mergeBase)},
	}); err != nil {
		return "", 0, fmt.Errorf("creating scratch ref %s at %s: %w", scratch, mergeBase, err)
	}
	// From here on the scratch ref exists; remove it on every exit.
	defer func() { _, _ = c.client.Git.DeleteRef(ctx, owner, repo, "heads/"+scratch) }()

	newOID, err := c.createCommitOnBranch(ctx, owner+"/"+repo, scratch, mergeBase, headline, body, changes)
	if err != nil {
		return "", 0, fmt.Errorf("createCommitOnBranch on %s: %w", scratch, err)
	}

	// The agent may have pushed again while we worked. Its newer commits are
	// not in the signed commit, so replacing them would silently drop work:
	// re-read the head and refuse if it moved. (Not atomic with the update —
	// GitHub's ref API has no compare-and-swap — but it closes the window to
	// one round trip, and the alternative is a rewrite we cannot verify.)
	current, _, err := c.client.Git.GetRef(ctx, owner, repo, "heads/"+head)
	if err != nil {
		return "", 0, fmt.Errorf("re-reading ref heads/%s before update: %w", head, err)
	}
	if current.GetObject().GetSHA() != headSHA {
		return "", 0, fmt.Errorf("%w: %s moved from %s to %s while the signed commit was being built; left as pushed", errSignedCommitSkip, head, headSHA[:7], current.GetObject().GetSHA()[:7])
	}
	if _, _, err := c.client.Git.UpdateRef(ctx, owner, repo, &gh.Reference{
		Ref:    gh.Ptr("refs/heads/" + head),
		Object: &gh.GitObject{SHA: gh.Ptr(newOID)},
	}, true); err != nil {
		return "", 0, fmt.Errorf("updating heads/%s to signed commit %s: %w", head, newOID, err)
	}
	return newOID, len(comparison.Commits), nil
}

// signedFileChanges is the fileChanges argument of createCommitOnBranch.
type signedFileChanges struct {
	Additions []signedFileAddition `json:"additions,omitempty"`
	Deletions []signedFileDeletion `json:"deletions,omitempty"`
}

type signedFileAddition struct {
	Path     string `json:"path"`
	Contents string `json:"contents"` // base64
}

type signedFileDeletion struct {
	Path string `json:"path"`
}

// collectSignedFileChanges turns the compare file list into the mutation's
// additions and deletions, fetching each surviving file's bytes at headSHA.
// It refuses (errSignedCommitSkip) anything the mutation cannot express: a
// changed path whose tree entry is not a regular, non-executable blob, a
// truncated tree listing (the mode check would be blind), or a change larger
// than signedCommitMaxBytes.
func (c *Client) collectSignedFileChanges(ctx context.Context, owner, repo, headSHA string, files []*gh.CommitFile) (signedFileChanges, error) {
	tree, _, err := c.client.Git.GetTree(ctx, owner, repo, headSHA, true)
	if err != nil {
		return signedFileChanges{}, fmt.Errorf("reading tree %s: %w", headSHA[:7], err)
	}
	if tree.GetTruncated() {
		return signedFileChanges{}, fmt.Errorf("%w: tree %s is too large for GitHub to list, so file modes cannot be checked", errSignedCommitSkip, headSHA[:7])
	}
	entries := make(map[string]*gh.TreeEntry, len(tree.Entries))
	for _, e := range tree.Entries {
		if e != nil {
			entries[e.GetPath()] = e
		}
	}

	var out signedFileChanges
	var total int
	for _, f := range files {
		if f == nil {
			continue
		}
		path := f.GetFilename()
		switch f.GetStatus() {
		case "removed":
			out.Deletions = append(out.Deletions, signedFileDeletion{Path: path})
			continue
		case "renamed":
			if prev := f.GetPreviousFilename(); prev != "" {
				out.Deletions = append(out.Deletions, signedFileDeletion{Path: prev})
			}
		case "added", "modified", "changed", "copied":
			// surviving file: added below
		default:
			return signedFileChanges{}, fmt.Errorf("%w: %s has compare status %q, which this rewrite does not handle", errSignedCommitSkip, path, f.GetStatus())
		}
		entry, ok := entries[path]
		if !ok {
			return signedFileChanges{}, fmt.Errorf("%w: %s is in the diff but not in tree %s", errSignedCommitSkip, path, headSHA[:7])
		}
		if entry.GetType() != "blob" || entry.GetMode() != "100644" {
			return signedFileChanges{}, fmt.Errorf("%w: %s is %s mode %s; createCommitOnBranch can only express regular files", errSignedCommitSkip, path, entry.GetType(), entry.GetMode())
		}
		raw, _, err := c.client.Git.GetBlobRaw(ctx, owner, repo, entry.GetSHA())
		if err != nil {
			return signedFileChanges{}, fmt.Errorf("reading blob for %s: %w", path, err)
		}
		total += len(raw)
		if total > signedCommitMaxBytes {
			return signedFileChanges{}, fmt.Errorf("%w: the change exceeds %d bytes, too large for one createCommitOnBranch request", errSignedCommitSkip, signedCommitMaxBytes)
		}
		out.Additions = append(out.Additions, signedFileAddition{Path: path, Contents: base64.StdEncoding.EncodeToString(raw)})
	}
	if len(out.Additions) == 0 && len(out.Deletions) == 0 {
		return signedFileChanges{}, fmt.Errorf("%w: no file changes to commit", errSignedCommitSkip)
	}
	return out, nil
}

// signedCommitMessage folds the agents' commit messages into the one signed
// commit's headline and body. The oldest commit's subject is the headline;
// its body, then every later commit's full message, form the body — so
// nothing the agent wrote (issue references, DCO sign-offs) is lost, and a
// single-commit branch reads exactly as the agent committed it.
func signedCommitMessage(commits []*gh.RepositoryCommit) (headline, body string) {
	var parts []string
	for i, rc := range commits {
		msg := strings.TrimSpace(rc.GetCommit().GetMessage())
		if msg == "" {
			continue
		}
		if i == 0 {
			subject, rest, _ := strings.Cut(msg, "\n")
			headline = strings.TrimSpace(subject)
			if rest = strings.TrimSpace(rest); rest != "" {
				parts = append(parts, rest)
			}
			continue
		}
		parts = append(parts, msg)
	}
	if headline == "" {
		headline = "Signed re-author of agent commits"
	}
	return headline, strings.Join(parts, "\n\n")
}

// createCommitOnBranch runs the mutation and returns the new commit's oid.
// The request is a plain JSON POST to the GraphQL endpoint over the client's
// own (App-authenticated) transport — the REST client's BaseURL tells us which
// GraphQL endpoint pairs with it, for github.com, GHE, and the test server.
func (c *Client) createCommitOnBranch(ctx context.Context, nameWithOwner, branch, expectedHeadOID, headline, body string, changes signedFileChanges) (string, error) {
	const mutation = `mutation($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) { commit { oid } }
}`
	message := map[string]string{"headline": headline}
	if body != "" {
		message["body"] = body
	}
	payload := map[string]any{
		"query": mutation,
		"variables": map[string]any{
			"input": map[string]any{
				"branch":          map[string]string{"repositoryNameWithOwner": nameWithOwner, "branchName": branch},
				"expectedHeadOid": expectedHeadOID,
				"message":         message,
				"fileChanges":     changes,
			},
		},
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, graphQLEndpoint(c.client.BaseURL), bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	resp, err := c.client.Client().Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("graphql HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var parsed struct {
		Data struct {
			CreateCommitOnBranch struct {
				Commit struct {
					OID string `json:"oid"`
				} `json:"commit"`
			} `json:"createCommitOnBranch"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("graphql response: %w", err)
	}
	if len(parsed.Errors) > 0 {
		msgs := make([]string, 0, len(parsed.Errors))
		for _, e := range parsed.Errors {
			msgs = append(msgs, e.Message)
		}
		return "", fmt.Errorf("graphql: %s", strings.Join(msgs, "; "))
	}
	if parsed.Data.CreateCommitOnBranch.Commit.OID == "" {
		return "", errors.New("graphql: createCommitOnBranch returned no commit oid")
	}
	return parsed.Data.CreateCommitOnBranch.Commit.OID, nil
}

// graphQLEndpoint maps a REST base URL to its GraphQL endpoint:
// https://api.github.com/ → https://api.github.com/graphql;
// https://ghe.example/api/v3/ → https://ghe.example/api/graphql;
// anything else (a test server) → <base>graphql.
func graphQLEndpoint(base *url.URL) string {
	if base == nil {
		return "https://api.github.com/graphql"
	}
	if strings.EqualFold(base.Host, "api.github.com") {
		return base.Scheme + "://" + base.Host + "/graphql"
	}
	if strings.HasSuffix(strings.TrimSuffix(base.Path, "/"), "/api/v3") {
		return base.Scheme + "://" + base.Host + "/api/graphql"
	}
	return strings.TrimSuffix(base.String(), "/") + "/graphql"
}
