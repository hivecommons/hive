package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Decomposed parent issues (hivecommons/hive#12857).
//
// When an agent splits a roadmap issue into ordered children it files each
// child through the issue-request relay with `parent: N`; the relay links the
// child as a native GitHub sub-issue of N (sub_issues.go) and records the
// link in the split-parents ledger (split_parents.go). The parent itself
// then has nothing left to implement — it is waiting on its children — but
// until this file existed nothing in the pipeline read that relation back:
// hivecommons/hive#12361 sat at stage/010-actionable after all four children
// had merged, the stall sweep put stage/stalled on top because the parent's
// OWN timeline was quiet, and a maintainer closed it by hand.
//
// This file is the read side of that relation:
//
//   - childrenOf(parent) — one resolver. GitHub's list payload carries a
//     `sub_issues_summary` block (total / completed) on every parent, decoded
//     for free by listOpenIssuesWithBlockedCounts next to the dependency
//     summary. When that block is absent (older GHES, a sub-issue link the
//     relay could not make) the ledger and the `<!-- hive:parent N -->`
//     marker the relay writes into each child body are inverted against the
//     open-issue snapshot instead.
//   - In the issue stage sweep (SyncIssueStageLabels): a parent whose
//     children are ALL closed is closed as completed with one comment naming
//     each child and the PR that closed it, and its stage/* labels come off.
//     A parent with an open child is never marked stage/stalled (an existing
//     marker is removed); its stage is otherwise left alone. Parents never
//     move to stage/090-ready-for-human-merge — that stage is for PRs.

// IssueChildren is a decomposed parent's sub-issue progress: how many
// tracked children it has and how many are closed (any state_reason). The
// JSON names match GitHub's `sub_issues_summary` block so the list payload
// decodes straight into it.
type IssueChildren struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
}

// AllClosed reports whether every tracked child is closed.
func (c IssueChildren) AllClosed() bool { return c.Total > 0 && c.Completed >= c.Total }

// Open is the number of children still open.
func (c IssueChildren) Open() int {
	if c.Total <= c.Completed {
		return 0
	}
	return c.Total - c.Completed
}

// ParentMarker is the machine-readable line the relay writes into a child
// issue body when it links the child under a parent, so the child → parent
// relation survives a failed sub-issue link or a lost ledger.
const parentMarkerPrefix = "<!-- hive:parent "

// ParentMarker returns the body marker for parent issue number n.
func ParentMarker(n int) string {
	return parentMarkerPrefix + strconv.Itoa(n) + " -->"
}

var parentMarkerRe = regexp.MustCompile(`<!--\s*hive:parent\s+#?(\d+)\s*-->`)

// ParseParentMarker returns the parent issue number a body's marker names,
// or 0 when there is none.
func ParseParentMarker(body string) int {
	m := parentMarkerRe.FindStringSubmatch(body)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// withParentMarker appends the parent marker to body unless one for the same
// parent is already present.
func withParentMarker(body string, parent int) string {
	if parent <= 0 || ParseParentMarker(body) == parent {
		return body
	}
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return ParentMarker(parent)
	}
	return body + "\n\n" + ParentMarker(parent)
}

// splitChildrenOf inverts the ledger: the child numbers recorded under
// repo#parent, sorted.
func splitChildrenOf(children map[string]SplitParent, repo string, parent int) []int {
	if len(children) == 0 || parent <= 0 {
		return nil
	}
	prefix := strings.ToLower(strings.TrimSpace(repo)) + "#"
	var out []int
	for key, sp := range children {
		if sp.Parent != parent || !strings.HasPrefix(key, prefix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(key, prefix))
		if err != nil || n <= 0 || n == parent {
			continue
		}
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// childrenOf resolves a parent's tracked children from what enumeration
// already has in hand: GitHub's sub-issue summary first; failing that, the
// ledger and body markers inverted against the open snapshot. In the
// fallback a child absent from the open snapshot is counted as closed (the
// snapshot is every open issue in the repo), and a body-marked child that
// is open counts as open — the marker cannot name closed children, so a
// marker-only parent is exempt from stalling but is never auto-closed on
// that evidence alone (closeDecomposedParent re-reads the children).
func childrenOf(repo string, parent int, summaries map[int]IssueChildren, ledger map[string]SplitParent, open map[int]*gh.Issue) *IssueChildren {
	if s, ok := summaries[parent]; ok && s.Total > 0 {
		s := s
		return &s
	}
	seen := map[int]bool{}
	for _, n := range splitChildrenOf(ledger, repo, parent) {
		seen[n] = true
	}
	for n, issue := range open {
		if n != parent && issue != nil && ParseParentMarker(issue.GetBody()) == parent {
			seen[n] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := &IssueChildren{Total: len(seen)}
	for n := range seen {
		if _, isOpen := open[n]; !isOpen {
			out.Completed++
		}
	}
	return out
}

// annotateIssueChildren sets Children on every enumerated row that is a
// decomposed parent.
func annotateIssueChildren(repo string, summaries map[int]IssueChildren, ledger map[string]SplitParent, open map[int]*gh.Issue, actionable []Issue, details []RepoWorkIssue) {
	for i := range actionable {
		actionable[i].Children = childrenOf(repo, actionable[i].Number, summaries, ledger, open)
	}
	for i := range details {
		if details[i].Number > 0 {
			details[i].Children = childrenOf(repo, details[i].Number, summaries, ledger, open)
		}
	}
}

// subIssueRef is one child as GET .../issues/{parent}/sub_issues reports it.
type subIssueRef struct {
	Number int    `json:"number"`
	State  string `json:"state"`
}

// listSubIssues reads a parent's native sub-issue list. go-github v72 has no
// wrapper for the endpoint, so the request goes through the same
// NewRequest/Do escape hatch AddSubIssue uses.
func (c *Client) listSubIssues(ctx context.Context, owner, repoName string, parent int) ([]subIssueRef, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	var out []subIssueRef
	page := 1
	for {
		path := fmt.Sprintf("repos/%s/%s/issues/%d/sub_issues?per_page=100&page=%d", owner, repoName, parent, page)
		req, err := c.client.NewRequest(http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		var batch []subIssueRef
		resp, err := c.client.Do(ctx, req, &batch)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return out, nil
}

// decomposedChildren re-reads a parent's children live, right before a
// close: the native sub-issue list when it has entries, otherwise each
// ledger-recorded child by number. Returns the sorted child numbers and
// how many are open; ok is false when nothing could be read.
func (c *Client) decomposedChildren(ctx context.Context, owner, repoName string, parent int) (children []int, open int, ok bool, err error) {
	refs, err := c.listSubIssues(ctx, owner, repoName, parent)
	if err != nil {
		return nil, 0, false, fmt.Errorf("listing sub-issues of %s/%s#%d: %w", owner, repoName, parent, err)
	}
	if len(refs) > 0 {
		for _, r := range refs {
			if r.Number <= 0 || r.Number == parent {
				continue
			}
			children = append(children, r.Number)
			if !strings.EqualFold(r.State, "closed") {
				open++
			}
		}
		sort.Ints(children)
		return children, open, len(children) > 0, nil
	}
	ledger, lerr := LoadSplitParents("")
	if lerr != nil {
		return nil, 0, false, nil
	}
	for _, n := range splitChildrenOf(ledger, owner+"/"+repoName, parent) {
		child, _, gerr := c.client.Issues.Get(ctx, owner, repoName, n)
		if gerr != nil {
			return nil, 0, false, fmt.Errorf("reading child %s/%s#%d: %w", owner, repoName, n, gerr)
		}
		children = append(children, n)
		if !strings.EqualFold(child.GetState(), "closed") {
			open++
		}
	}
	return children, open, len(children) > 0, nil
}

// parentAutoCloseMarker tags the close comment so a reader (or a later
// sweep) can tell it from an agent's prose.
const parentAutoCloseMarker = "<!-- hive:parent-autoclose -->"

// decomposedParentCloseComment is the one comment the sweep leaves on a
// parent it closes. closers maps child → merged PR number (0 = unknown).
func decomposedParentCloseComment(children []int, closers map[int]int) string {
	parts := make([]string, 0, len(children))
	for _, n := range children {
		if pr := closers[n]; pr > 0 {
			parts = append(parts, fmt.Sprintf("#%d → PR #%d", n, pr))
		} else {
			parts = append(parts, fmt.Sprintf("#%d", n))
		}
	}
	return parentAutoCloseMarker + "\nAll children delivered: " + strings.Join(parts, ", ") + ".\n\nEvery tracked child of this issue is closed, so the hive is closing it as completed. Reopen it if something it asked for is still missing."
}

// closeDecomposedParent closes an open parent whose enumeration row says
// every child is closed. It re-reads the children first (the snapshot can
// lag a child's close or reopen), then strips the hive-owned labels, closes
// the issue as completed and leaves one budgeted comment. Returns true when
// the parent was closed. A failure is logged and retried next cycle; the
// comment is posted only after a successful close so a close the reporter
// gate refuses does not leave a "delivered" note every cycle.
func (c *Client) closeDecomposedParent(ctx context.Context, in IssueStageInput, store *stagelabels.Store, key string, logger *slog.Logger) bool {
	owner, name := c.splitRepo(in.Repo)
	children, open, ok, err := c.decomposedChildren(ctx, owner, name, in.Number)
	if err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: decomposed parent children unreadable, not closing", "repo", in.Repo, "issue", in.Number, "error", err)
		}
		return false
	}
	if !ok || open > 0 {
		return false
	}
	closers := map[int]int{}
	for _, n := range children {
		if closer, found := c.closedByPullRequestReference(ctx, owner, name, n, 0); found {
			closers[n] = closer.Number
		}
	}
	if _, err := stagelabels.Enforce(ctx, c.StageLabeler(), stagelabels.IssueItem, owner, name, in.Number, in.Labels, stagelabels.Desired{}); err != nil && logger != nil {
		logger.Warn("issue stage labels: clearing stage labels on decomposed parent failed", "repo", in.Repo, "issue", in.Number, "error", err)
	}
	if err := c.CloseIssue(ctx, in.Repo, in.Number, IssueCloseOptions{StateReason: IssueStateReasonCompleted}); err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: closing decomposed parent failed", "repo", in.Repo, "issue", in.Number, "children", children, "error", err)
		}
		return false
	}
	if store != nil {
		store.Forget(key)
	}
	if err := c.CreateIssueCommentBudgeted(ctx, in.Repo, in.Number, decomposedParentCloseComment(children, closers)); err != nil && logger != nil {
		logger.Warn("issue stage labels: decomposed parent close comment failed", "repo", in.Repo, "issue", in.Number, "error", err)
	}
	if logger != nil {
		logger.Info("issue stage labels: decomposed parent closed, all children delivered", "repo", in.Repo, "issue", in.Number, "children", children, "closing_prs", closers)
	}
	return true
}
