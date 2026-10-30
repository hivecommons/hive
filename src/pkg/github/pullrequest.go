package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/forgestate"
)

// forwardMergeHeadRe matches the branches the v5-topup / v6-topup workflows
// (and the humans who hand-resolve their conflicts) use for line-to-line
// forward-merges, e.g. "sync/v5-to-v6", a scanner-namespaced variant like
// "scanner/sync-v5-to-v6-9919", or a "topup/v5-into-v6-20261020" head. Squashing
// one of these erases the source line's ancestry and makes every later top-up
// re-conflict on the same hunks (#9957, #9956, #13404).
var forwardMergeHeadRe = regexp.MustCompile(`sync/?-?v\d+-to-v\d+|^topup/`)

// forwardMergeTitleRe matches top-up / sync PR titles after an optional
// leading emoji, e.g. "🌱 top-up: merge v5 into v6 (51 commits)" (#13404).
var forwardMergeTitleRe = regexp.MustCompile(`(?i)^[^\p{L}\p{N}]*(top-up|sync):`)

// releaseLineBranchRe matches release-line branch names ("v5", "v6").
var releaseLineBranchRe = regexp.MustCompile(`^v\d+$`)

// IsForwardMergeHead reports whether ref is a head branch the forward-merge
// workflows use (forwardMergeHeadRe). Unlike IsForwardMergePR it ignores the
// title, which any contributor controls, so gates that relax on a
// forward-merge key on it (hivecommons/hive#13635).
func IsForwardMergeHead(ref string) bool {
	return forwardMergeHeadRe.MatchString(ref)
}

// IsForwardMergePR reports whether pr is a forward-merge between release
// lines, which must land as a merge commit rather than a squash. It only
// looks at the PR's metadata; ResolveForwardMergeMethod adds the head-history
// check for PRs whose branch and title give no hint.
func IsForwardMergePR(pr *gh.PullRequest) bool {
	if pr == nil {
		return false
	}
	title := strings.TrimSpace(pr.GetTitle())
	return forwardMergeHeadRe.MatchString(pr.GetHead().GetRef()) ||
		forwardMergeTitleRe.MatchString(title) ||
		strings.Contains(strings.ToLower(title), "forward-merge")
}

const releaseLineMergeCacheMax = 1024

var releaseLineMergeCache = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

// HeadMergesReleaseLine returns the release-line branch (other than pr's base)
// whose history pr's head brings in, or "" when it brings in none. A head
// "brings in" another line when that line's tip is an ancestor of the head, or
// when the head's merge-base with that line is not already in the base — i.e.
// the head carries a merge of the other line (#13404). Only PRs based on a
// release line are checked; candidate lines are the repository's protected
// "vN" branches. Results are cached per base and head SHA.
func HeadMergesReleaseLine(ctx context.Context, client *gh.Client, owner, repo string, pr *gh.PullRequest) (string, error) {
	if client == nil || pr == nil {
		return "", nil
	}
	base := pr.GetBase().GetRef()
	headSHA := pr.GetHead().GetSHA()
	if headSHA == "" || !releaseLineBranchRe.MatchString(base) {
		return "", nil
	}
	key := owner + "/" + repo + "#" + base + "@" + headSHA
	releaseLineMergeCache.Lock()
	cached, ok := releaseLineMergeCache.m[key]
	releaseLineMergeCache.Unlock()
	if ok {
		return cached, nil
	}

	branches, _, err := client.Repositories.ListBranches(ctx, owner, repo, &gh.BranchListOptions{
		Protected:   gh.Ptr(true),
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return "", fmt.Errorf("list protected branches: %w", err)
	}
	found := ""
	for _, b := range branches {
		other := b.GetName()
		if other == base || !releaseLineBranchRe.MatchString(other) {
			continue
		}
		cmp, _, err := client.Repositories.CompareCommits(ctx, owner, repo, other, headSHA, &gh.ListOptions{PerPage: 1})
		if err != nil {
			return "", fmt.Errorf("compare %s...%s: %w", other, headSHA, err)
		}
		if st := cmp.GetStatus(); st == "ahead" || st == "identical" {
			found = other
			break
		}
		mergeBase := cmp.GetMergeBaseCommit().GetSHA()
		if mergeBase == "" {
			continue
		}
		inBase, _, err := client.Repositories.CompareCommits(ctx, owner, repo, base, mergeBase, &gh.ListOptions{PerPage: 1})
		if err != nil {
			return "", fmt.Errorf("compare %s...%s: %w", base, mergeBase, err)
		}
		if st := inBase.GetStatus(); st != "behind" && st != "identical" {
			found = other
			break
		}
	}
	releaseLineMergeCache.Lock()
	if len(releaseLineMergeCache.m) >= releaseLineMergeCacheMax {
		releaseLineMergeCache.m = map[string]string{}
	}
	releaseLineMergeCache.m[key] = found
	releaseLineMergeCache.Unlock()
	return found, nil
}

// ResolveForwardMergeMethod is the one place every hive merge path (the
// automerge lanes and the MergePR relay) decides whether a requested squash or
// rebase must be upgraded to a merge commit: forward-merge / top-up PRs, by
// metadata (IsForwardMergePR) or by head history (HeadMergesReleaseLine), never
// squash. A failed history lookup is logged and keeps the requested method.
func ResolveForwardMergeMethod(ctx context.Context, client *gh.Client, logger *slog.Logger, owner, repo string, pr *gh.PullRequest, method string) string {
	if method == "merge" || pr == nil {
		return method
	}
	if logger == nil {
		logger = slog.Default()
	}
	reason, line := "", ""
	if IsForwardMergePR(pr) {
		reason = "metadata"
	} else {
		var err error
		line, err = HeadMergesReleaseLine(ctx, client, owner, repo, pr)
		if err != nil {
			logger.Warn("forward-merge guard: head-history check failed",
				slog.String("repo", owner+"/"+repo), slog.Int("number", pr.GetNumber()),
				slog.String("error", err.Error()))
			return method
		}
		if line != "" {
			reason = "head-merges-release-line"
		}
	}
	if reason == "" {
		return method
	}
	logger.Warn("forward-merge guard: refusing to squash, forcing merge commit",
		slog.String("repo", owner+"/"+repo), slog.Int("number", pr.GetNumber()),
		slog.String("requested", method), slog.String("reason", reason),
		slog.String("release_line", line), slog.String("head", pr.GetHead().GetRef()))
	return "merge"
}

// CreatePRResult is what CreatePR returns: the opened (or pre-existing) PR.
// It is the runtime's PRResult (crux.PRResult) under this package's name,
// so a *Client satisfies the agent runtime's PRCreator seam without the
// runtime importing this package (hivecommons/hive#13536). DuplicateReason,
// when set, is one of the DuplicateReason* constants below.
type CreatePRResult = crux.PRResult

// CreatePR opens a pull request on behalf of THIS hive's GitHub App — so the PR
// is authored by the App bot ("<slug>[bot]"), deterministically, regardless of
// which agent/CLI produced the branch. This is the hive-opens-the-PR path: an
// agent pushes its branch (the credential helper already uses the App token for
// the push), then the hive opens the PR here rather than the agent running
// `gh pr create` (which would attribute the PR to the Copilot login user).
//
// repo may be "owner/repo" or a bare repo name (owner defaults to the hive org).
// head is the branch the agent pushed; an empty base is RESOLVED from the
// target repository's own default branch (see DefaultBranch) rather than
// assumed to be "main" — a repo whose default is "testing" or "develop" would
// otherwise get every PR based on the wrong branch, carrying the full
// divergence between the two branches in its diff (kubestellar/hive#4928).
//
// A base the caller explicitly passed is always honored as-is and never
// second-guessed against the resolved default. When no base is given and the
// resolution fails, CreatePR returns an error rather than silently falling
// back to "main" — a wrong base is worse than a delayed PR, and the caller
// (the pr-request watcher) already surfaces a returned error into the audit
// log and the request's .result.json, and retries with backoff.
//
// It is idempotent: if an OPEN PR for head already exists, it returns that PR
// with AlreadyExisted=true instead of erroring or opening a duplicate — this
// makes the file-watcher safe to retry a request that partially succeeded.
//
// It also refuses to file the same CONTENT twice (kubestellar/hive#5111): if an
// open PR on the same base already carries an identical tree SHA, that PR is
// returned with AlreadyExisted and DuplicateTree set. Same-branch idempotency
// cannot see this case, because the duplicate arrives on a fresh branch.
func (c *Client) CreatePR(ctx context.Context, repo, head, base, title, body string) (CreatePRResult, error) {
	if c == nil || c.client == nil {
		return CreatePRResult{}, ErrNoGitHubClient
	}
	owner := c.org
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		owner, repo = parts[0], parts[1]
	}
	head = strings.TrimSpace(head)
	if head == "" {
		return CreatePRResult{}, fmt.Errorf("CreatePR: head branch is required")
	}
	if base = strings.TrimSpace(base); base == "" {
		resolved, err := c.DefaultBranch(ctx, owner, repo)
		if err != nil {
			return CreatePRResult{}, fmt.Errorf("CreatePR %s/%s: could not resolve the repository's default branch and no explicit base was given (refusing to guess \"%s\"): %w", owner, repo, FallbackDefaultBranch, err)
		}
		base = resolved
	}
	if strings.TrimSpace(title) == "" {
		return CreatePRResult{}, fmt.Errorf("CreatePR: title is required")
	}
	if leak, ok := c.scanCanaryText(title+"\n"+body, "hive-open-pr:"+repo); ok {
		if c.canaryFailClosed {
			return CreatePRResult{}, fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
		}
	}
	title = logscrub.ScrubString(title)
	body = logscrub.ScrubString(body)

	// Idempotency: don't open a second PR for a branch that already has an open
	// one. GitHub itself 422s on a duplicate, but checking first lets us return
	// the existing PR cleanly (the watcher may re-process a request after a
	// crash between "PR opened" and "request file deleted").
	if existing, err := c.findOpenPRForHead(ctx, owner, repo, head); err != nil {
		if isRetryableGitHubError(err) {
			return CreatePRResult{}, fmt.Errorf("CreatePR: dedupe lookup failed with retryable error for %s/%s head %s: %w", owner, repo, head, err)
		}
		c.logger.Warn("CreatePR: dedupe lookup failed with terminal error, creating without dedupe",
			slog.String("repo", repo), slog.String("head", head), slog.String("error", err.Error()))
	} else if existing != nil {
		c.logger.Info("CreatePR: open PR already exists for head, reusing",
			slog.String("repo", repo), slog.String("head", head), slog.Int("number", existing.GetNumber()))
		return CreatePRResult{Number: existing.GetNumber(), URL: existing.GetHTMLURL(), Author: existing.GetUser().GetLogin(), AlreadyExisted: true}, nil
	}

	// Content identity (#5111): the check above only recognises a duplicate that
	// reuses the SAME branch. Agents also copy a change forward onto a NEW branch
	// and file it again — five such PRs on tuna-os/tromso, two with identical
	// trees, so `git diff` between their tips was empty. Compare the tree SHA
	// against the open PRs on this same base and reuse the match instead.
	//
	// Retryable lookup failures keep the request queued rather than opening
	// blind; terminal lookup failures still degrade so a bad candidate cannot
	// permanently block an unrelated PR.
	if existing, err := c.findOpenPRWithIdenticalTree(ctx, owner, repo, head, base); err != nil {
		if isRetryableGitHubError(err) {
			return CreatePRResult{}, fmt.Errorf("CreatePR: duplicate-tree lookup failed with retryable error for %s/%s head %s: %w", owner, repo, head, err)
		}
		c.logger.Warn("CreatePR: duplicate-tree lookup failed with terminal error, creating without duplicate-tree dedupe",
			slog.String("repo", repo), slog.String("head", head), slog.String("error", err.Error()))
	} else if existing != nil {
		c.logger.Info("CreatePR: an open PR already carries this exact tree, reusing it instead of opening a duplicate",
			slog.String("repo", repo), slog.String("head", head),
			slog.String("base", base), slog.Int("number", existing.GetNumber()))
		return CreatePRResult{
			Number:         existing.GetNumber(),
			URL:            existing.GetHTMLURL(),
			Author:         existing.GetUser().GetLogin(),
			AlreadyExisted: true,
			DuplicateTree:  true,
		}, nil
	}

	// Issue and file-set identity: agents on one issue never collide on head or
	// tree, so also refuse when an open PR already closes the same issue, an
	// open App-bot PR edits an overlapping file set, or a merged PR already
	// fixed the issue. Retryable lookup failures keep the request queued;
	// terminal ones degrade to opening the PR.
	if found, err := c.findDuplicatePR(ctx, owner, repo, head, base, body); err != nil {
		if isRetryableGitHubError(err) {
			return CreatePRResult{}, fmt.Errorf("CreatePR: duplicate guard lookup failed with retryable error for %s/%s head %s: %w", owner, repo, head, err)
		}
		c.logger.Warn("CreatePR: duplicate guard lookup failed with terminal error, creating without it",
			slog.String("repo", repo), slog.String("head", head), slog.String("error", err.Error()))
	} else if found != nil {
		c.logger.Info("CreatePR: an existing PR already covers this request, redirecting to it instead of opening a duplicate",
			slog.String("repo", repo), slog.String("head", head), slog.String("reason", found.reason),
			slog.Int("number", found.pr.GetNumber()))
		c.redirectToExistingPR(ctx, owner, repo, head, found)
		return CreatePRResult{
			Number:          found.pr.GetNumber(),
			URL:             found.pr.GetHTMLURL(),
			Author:          found.pr.GetUser().GetLogin(),
			AlreadyExisted:  true,
			DuplicateReason: found.reason,
		}, nil
	}

	var pr *gh.PullRequest
	// ExecuteReconciling, not Execute: an ambiguous POST failure journals the
	// op Unknown, and without an observation every later create for the same
	// head was refused forever (hivecommons/hive#14366).
	res, err := effects.ExecuteReconciling(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestCreate,
		Target: head,
		Inputs: map[string]string{"head": head, "base": base, "title": effects.StableDigest(title), "body": effects.StableDigest(body)},
	}, func(ctx context.Context) (effects.ExternalState, error) {
		return c.observePullRequestCreate(ctx, owner, repo, head)
	}, effects.DefaultUnresolvedTTL, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		pr, _, apiErr = c.client.PullRequests.Create(ctx, owner, repo, &gh.NewPullRequest{
			Title: gh.Ptr(title),
			Head:  gh.Ptr(head),
			Base:  gh.Ptr(base),
			Body:  gh.Ptr(body),
		})
		if apiErr != nil {
			if isDefiniteIssueRefusal(apiErr) {
				// The PR was not opened; recording Unknown would fence retries.
				return effects.Result{}, effects.NotApplied(apiErr)
			}
			return effects.Result{}, apiErr
		}
		return effects.Result{Provenance: pr.GetHTMLURL()}, nil
	})
	if err != nil {
		// A concurrent creator may have raced us; treat an existing-PR 422 as a
		// reuse rather than a hard failure.
		if strings.Contains(err.Error(), "A pull request already exists") {
			if existing, lookErr := c.findOpenPRForHead(ctx, owner, repo, head); lookErr == nil && existing != nil {
				return CreatePRResult{Number: existing.GetNumber(), URL: existing.GetHTMLURL(), Author: existing.GetUser().GetLogin(), AlreadyExisted: true}, nil
			}
		}
		return CreatePRResult{}, fmt.Errorf("creating PR %s/%s %s->%s: %w", owner, repo, head, base, err)
	}
	if pr == nil {
		// The mutation boundary deduplicated this call as a replay of an
		// already-applied create (#8347): the effect never ran, so there is
		// no API response to read the PR from. Rebuild the typed reference
		// from the journaled provenance (the PR URL) instead of reporting a
		// zero PR number as success; an unparsable provenance is an error,
		// never a fabricated result.
		ref, parseErr := ParsePRURL(res.Provenance)
		if parseErr != nil {
			return CreatePRResult{}, fmt.Errorf("creating PR %s/%s %s->%s: replayed as already applied but recorded provenance %q is not a PR URL: %w", owner, repo, head, base, res.Provenance, parseErr)
		}
		c.logger.Info("CreatePR: mutation boundary replayed an already-applied create, returning the recorded PR",
			slog.String("repo", repo), slog.String("head", head), slog.Int("number", ref.Number))
		return CreatePRResult{Number: ref.Number, URL: res.Provenance, AlreadyExisted: true}, nil
	}
	c.logger.Info("CreatePR: opened PR as the App bot",
		slog.String("repo", repo), slog.String("head", head), slog.Int("number", pr.GetNumber()))
	// GitHub only derives Development-sidebar links from `Fixes #N` for
	// default-branch PRs; release-line PRs get theirs here
	// (hivecommons/hive#11888). Best-effort: the PR is already open.
	c.linkClosingIssuesForPR(ctx, owner, repo, pr.GetNumber(), body)
	// A closing keyword for a tracking/archive issue resolves nothing; say
	// so on the PR before anyone reviews it (hivecommons/hive#14613).
	c.NudgeTrackingIssueClosingRefs(ctx, owner+"/"+repo, pr.GetNumber(), body, c.logger)
	return CreatePRResult{Number: pr.GetNumber(), URL: pr.GetHTMLURL(), Author: pr.GetUser().GetLogin()}, nil
}

// MergePRResult is what MergePR returns after a successful merge.
type MergePRResult struct {
	SHA     string // the resulting merge commit SHA
	Merged  bool
	Message string
}

// MergePR merges an open pull request on behalf of THIS hive's GitHub App, over
// REST (PUT /repos/{owner}/{repo}/pulls/{n}/merge).
//
// This exists because the GitHub GraphQL `mergePullRequest` mutation — which the
// GitHub MCP server's merge_pull_request tool issues — returns "Resource not
// accessible by integration" for App installation tokens even when the token
// holds contents:write + pull_requests:write. The equivalent REST call with the
// SAME token succeeds. Routing merges through the hive over REST (mirroring the
// hive-open-pr authorship relay) makes the merge deterministic and App-authored,
// and sidesteps that GraphQL limitation.
//
// It does NOT force-merge: with admin bypass disabled, GitHub still enforces
// branch protection (required checks like build-gate), so a PR whose required
// checks are not green returns an error here rather than merging. mergeMethod
// defaults to "squash" when empty. When expectSHA is non-empty it is passed as
// the required head SHA, so the merge fails cleanly (409) instead of merging an
// unexpected commit if the head moved after eligibility was computed.
//
// MergePR is the hive-merge relay's entry point and audits the merge as
// path=relay; other callers name their own path through MergePRVia.
func (c *Client) MergePR(ctx context.Context, repo string, number int, mergeMethod, expectSHA string) (MergePRResult, error) {
	return c.MergePRVia(ctx, repo, number, mergeMethod, expectSHA, PRAuditPathRelay)
}

// MergePRVia is MergePR with the pr_merged audit path= named by the caller
// (one of the PRAuditPath* values), so a merge is attributed to the code path
// that asked for it rather than always to the relay.
func (c *Client) MergePRVia(ctx context.Context, repo string, number int, mergeMethod, expectSHA, auditPath string) (MergePRResult, error) {
	if c == nil || c.client == nil {
		return MergePRResult{}, ErrNoGitHubClient
	}
	owner := c.org
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		owner, repo = parts[0], parts[1]
	}
	if number <= 0 {
		return MergePRResult{}, fmt.Errorf("MergePR: pr number is required")
	}
	if err := CheckIrreversible(ctx, owner+"/"+repo, number, true); err != nil {
		return MergePRResult{}, fmt.Errorf("MergePR: %w", err)
	}
	if mergeMethod = strings.TrimSpace(mergeMethod); mergeMethod == "" {
		mergeMethod = "squash"
	}
	// Forward-merges keep ancestry no matter which caller (sweep, the
	// hive-merge relay or the forge adapter) asked for the merge. A failed lookup falls through to
	// the requested method.
	var (
		fetched      *gh.PullRequest
		fetchedTried bool
	)
	if mergeMethod != "merge" {
		restCtx := WithRESTCaller(ctx, "hive:merge_pr")
		fetchedTried = true
		if pr, _, err := c.client.PullRequests.Get(restCtx, owner, repo, number); err == nil {
			fetched = pr
			mergeMethod = ResolveForwardMergeMethod(restCtx, c.client, c.logger, owner, repo, pr, mergeMethod)
		}
	}

	opts := &gh.PullRequestOptions{MergeMethod: mergeMethod}
	if expectSHA = strings.TrimSpace(expectSHA); expectSHA != "" {
		opts.SHA = expectSHA
	}
	commitMessage := ""
	// Issue-author credit (hivecommons/hive#14581) rides the PR already
	// fetched above; a merge-commit request fetches it only when the credit
	// is on for the repo.
	if creditor := c.MergeCreditor(); creditor.Enabled(owner + "/" + repo) {
		if fetched == nil && !fetchedTried {
			if pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, mergeCreditCaller), owner, repo, number); err == nil {
				fetched = pr
			}
		}
		credit := creditor.Prepare(ctx, owner, repo, fetched, mergeMethod)
		commitMessage = credit.CommitMessage
		c.NoteMergeCredit(owner+"/"+repo, number, credit)
	}
	var res *gh.PullRequestMergeResult
	boundary := c.mutationBoundary()
	claim := effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindPullRequestMerge,
		Target: strconv.Itoa(number),
		Inputs: map[string]string{"method": mergeMethod, "expect_sha": expectSHA},
	}
	merge := func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		res, _, apiErr = c.client.PullRequests.Merge(ctx, owner, repo, number, commitMessage, opts)
		if apiErr != nil {
			if isDefiniteMergeRefusal(apiErr) {
				return effects.Result{}, effects.NotApplied(apiErr)
			}
			return effects.Result{}, apiErr
		}
		return effects.Result{Provenance: res.GetSHA()}, nil
	}
	// The tracker's "Merging" state (hivecommons/hive#15245) spans exactly
	// the merge call, whichever caller issued it.
	BeginMergeAttemptOnHead(owner+"/"+repo, number, auditPath, expectSHA)
	out, err := effects.Execute(ctx, boundary, claim, merge)
	if errors.Is(err, effects.ErrNeedsReconciliation) {
		out, err = c.reconcileMerge(ctx, boundary, claim, owner, repo, number, merge, err)
	}
	if err != nil {
		EndMergeAttempt(owner+"/"+repo, number, false, err)
		return MergePRResult{}, fmt.Errorf("merging PR %s/%s#%d (%s): %w", owner, repo, number, mergeMethod, err)
	}
	sha, merged := res.GetSHA(), res.GetMerged()
	if res == nil {
		// Dedup replay (#8347): the boundary skipped the effect and returned
		// the journaled merge SHA as provenance; surface it as the typed
		// result rather than an empty, unmerged-looking success.
		sha, merged = out.Provenance, out.Provenance != ""
	}
	if merged {
		EndMergeAttempt(owner+"/"+repo, number, true, nil)
	} else {
		EndMergeAttempt(owner+"/"+repo, number, false, fmt.Errorf("GitHub did not merge the pull request: %s", strings.TrimSpace(res.GetMessage())))
	}
	if merged {
		c.forgeWriteThrough(owner+"/"+repo, number, forgestate.Mutation{Op: forgestate.OpMerge, Kind: forgestate.KindPR})
	}
	c.logger.Info("MergePR: merged PR as the App bot over REST",
		slog.String("repo", owner+"/"+repo), slog.Int("number", number),
		slog.String("method", mergeMethod), slog.String("sha", sha))
	// Audit the merge UNCONDITIONALLY so the dashboard audit log shows the full
	// create→merge loop. The merge is performed by the hive itself (not a single
	// coding agent), so it is attributed to the governor flow, mirroring the
	// hive-issue-created attribution in advisory.go. path= is the caller's:
	// relay for the merge-request watcher (MergePR), forge for the forge
	// adapter.
	c.RecordPRMergedAudit(owner+"/"+repo, number, mergeMethod, sha, auditPath)
	return MergePRResult{SHA: sha, Merged: merged, Message: res.GetMessage()}, nil
}

// isDefiniteMergeRefusal reports whether a merge API error is GitHub
// definitively refusing the merge — 405 not mergeable, 409 head moved past the
// expected SHA, 422 invalid — so the merge certainly did not happen and the
// same logical merge may retry once the PR's state changes (#10536).
func isDefiniteMergeRefusal(err error) bool {
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return false
	}
	switch ghErr.Response.StatusCode {
	case http.StatusMethodNotAllowed, http.StatusConflict, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// reconcileMerge resolves a merge the mutation journal holds as unresolved
// (an earlier attempt errored ambiguously) from the PR's authoritative state,
// then replays the same logical merge once: an already-merged PR replays as
// idempotent success carrying the merge SHA, an unmerged PR retries the merge
// so the requester sees GitHub's own answer rather than the journal sentinel.
// When the PR cannot be read the original refusal stands.
func (c *Client) reconcileMerge(ctx context.Context, boundary effects.Boundary, claim effects.Claim, owner, repo string, number int, merge func(context.Context) (effects.Result, error), journalErr error) (effects.Result, error) {
	pr, _, getErr := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:reconcile_merge"), owner, repo, number)
	if getErr != nil {
		return effects.Result{}, fmt.Errorf("%w (reconciliation lookup failed: %v)", journalErr, getErr)
	}
	state := effects.ExternalState{Applied: pr.GetMerged(), Provenance: pr.GetMergeCommitSHA()}
	if recErr := effects.Reconcile(ctx, boundary, claim, state); recErr != nil {
		return effects.Result{}, fmt.Errorf("%w (reconciliation failed: %v)", journalErr, recErr)
	}
	c.logger.Info("MergePR: reconciled unresolved merge against GitHub",
		slog.String("repo", owner+"/"+repo), slog.Int("number", number),
		slog.Bool("merged", state.Applied), slog.String("sha", state.Provenance))
	return effects.Execute(ctx, boundary, claim, merge)
}

// UpdateBranch syncs a PR's head branch with its base (PUT
// /repos/{owner}/{repo}/pulls/{n}/update-branch), resolving the common
// "behind main" case before a merge. Like MergePR it goes over REST as the App.
//
// GitHub answers this endpoint with 202 Accepted (the merge is queued), which
// go-github surfaces as *gh.AcceptedError; that is the success path here, not
// a failure, so it is swallowed rather than returned.
func (c *Client) UpdateBranch(ctx context.Context, repo string, number int) error {
	return c.updateBranch(ctx, repo, number, "", "")
}

// UpdateBranchExpectedHead syncs a PR's head branch only if GitHub still sees
// expectedHeadSHA as the PR head. observedBaseSHA is part of the mutation
// identity so a later base move is not deduped against an earlier update
// attempt. Together they prevent a stale enumeration from adding a base-sync
// merge commit on top of contributor commits the hive has not evaluated yet.
func (c *Client) UpdateBranchExpectedHead(ctx context.Context, repo string, number int, expectedHeadSHA string, observedBaseSHA string) error {
	return c.updateBranch(ctx, repo, number, expectedHeadSHA, observedBaseSHA)
}

// IsUpdateBranchStaleHead reports whether err is GitHub refusing an
// UpdateBranchExpectedHead call because the PR head is no longer the expected
// SHA: a 422 naming the expected head. Callers treat it as "the head moved
// since enumeration, re-evaluate on the next snapshot", not as a merge
// conflict.
func IsUpdateBranchStaleHead(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	var apiErr *gh.ErrorResponse
	is422 := errors.As(err, &apiErr) && apiErr.Response != nil && apiErr.Response.StatusCode == http.StatusUnprocessableEntity
	if !is422 && !strings.Contains(msg, "422") && !strings.Contains(msg, "validation failed") {
		return false
	}
	return strings.Contains(msg, "expected_head_sha") ||
		strings.Contains(msg, "expected head") ||
		strings.Contains(msg, "head sha") ||
		strings.Contains(msg, "head_sha") ||
		strings.Contains(msg, "does not match")
}

func (c *Client) updateBranch(ctx context.Context, repo string, number int, expectedHeadSHA string, observedBaseSHA string) error {
	if c == nil || c.client == nil {
		return ErrNoGitHubClient
	}
	owner := c.org
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		owner, repo = parts[0], parts[1]
	}
	var inputs map[string]string
	if c.baseRefreshScheduled(owner + "/" + repo) {
		return fmt.Errorf("base-refresh:awaiting-admission: %s/%s#%d is refreshed only by the automerge preparation front", owner, repo, number)
	}
	if expectedHeadSHA != "" || observedBaseSHA != "" {
		inputs = map[string]string{"expected_head_sha": expectedHeadSHA, "observed_base_sha": observedBaseSHA}
	}
	observe := func(ctx context.Context) (effects.ExternalState, error) {
		pr, _, getErr := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:reconcile_branch_update"), owner, repo, number)
		if getErr != nil {
			return effects.ExternalState{}, getErr
		}
		return BranchUpdateState(pr, expectedHeadSHA, owner+"/"+repo+"#"+strconv.Itoa(number)), nil
	}
	_, err := effects.ExecuteReconciling(ctx, c.mutationBoundary(), effects.Claim{
		Repo:   owner + "/" + repo,
		Kind:   effects.KindBranchUpdate,
		Target: strconv.Itoa(number),
		Inputs: inputs,
	}, observe, effects.DefaultUnresolvedTTL, func(ctx context.Context) (effects.Result, error) {
		var opts *gh.PullRequestBranchUpdateOptions
		if expectedHeadSHA != "" {
			opts = &gh.PullRequestBranchUpdateOptions{ExpectedHeadSHA: gh.Ptr(expectedHeadSHA)}
		}
		_, _, apiErr := c.client.PullRequests.UpdateBranch(ctx, owner, repo, number, opts)
		var accepted *gh.AcceptedError
		if errors.As(apiErr, &accepted) {
			apiErr = nil
		}
		return effects.Result{Provenance: owner + "/" + repo + "#" + strconv.Itoa(number)}, apiErr
	})
	if err != nil {
		return fmt.Errorf("updating branch for PR %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// BranchUpdateState is the authoritative observation of a branch update the
// mutation journal holds as unresolved. With an expected head, a PR whose head
// has moved off it is the update having landed (or the branch having moved on,
// which makes the same guarded update moot either way). With no expected head,
// or a head still at it, the update is reported not applied: update-branch is
// safe to repeat — GitHub answers an up-to-date branch with a refusal rather
// than a second merge commit — so a retry cannot duplicate anything.
func BranchUpdateState(pr *gh.PullRequest, expectedHeadSHA, provenance string) effects.ExternalState {
	expected := strings.TrimSpace(expectedHeadSHA)
	head := strings.TrimSpace(pr.GetHead().GetSHA())
	if expected != "" && head != "" && !strings.EqualFold(head, expected) {
		return effects.ExternalState{Applied: true, Provenance: provenance}
	}
	return effects.ExternalState{}
}

// observePullRequestCreate is the authoritative observation of a PR create the
// mutation journal holds as unresolved: the earlier attempt applied when an
// open PR already exists for the head branch, and its URL is the provenance.
func (c *Client) observePullRequestCreate(ctx context.Context, owner, repo, head string) (effects.ExternalState, error) {
	if c == nil || c.client == nil {
		return effects.ExternalState{}, ErrNoGitHubClient
	}
	pr, err := c.findOpenPRForHead(ctx, owner, repo, head)
	if err != nil {
		return effects.ExternalState{}, err
	}
	if pr == nil {
		return effects.ExternalState{}, nil
	}
	return effects.ExternalState{Applied: true, Provenance: pr.GetHTMLURL()}, nil
}

// findOpenPRForHead returns the open PR whose head branch matches, or nil. The
// head filter is "owner:branch" per the GitHub API.
func (c *Client) findOpenPRForHead(ctx context.Context, owner, repo, head string) (*gh.PullRequest, error) {
	prs, _, err := c.client.PullRequests.List(ctx, owner, repo, &gh.PullRequestListOptions{
		State:       "open",
		Head:        owner + ":" + head,
		ListOptions: gh.ListOptions{PerPage: 5},
	})
	if err != nil {
		return nil, err
	}
	if len(prs) > 0 {
		return prs[0], nil
	}
	return nil, nil
}
