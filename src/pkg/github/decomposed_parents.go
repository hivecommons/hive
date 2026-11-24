package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/prstatus"
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

// parentMarkerRe matches both marker forms: the bare same-repo
// `<!-- hive:parent N -->` and the cross-repo `<!-- hive:parent owner/repo#N -->`
// (hivecommons/hive#18680). Group 1 is the repo ("" for the bare form),
// group 2 the number.
var parentMarkerRe = regexp.MustCompile(`<!--\s*hive:parent\s+(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#|#)?(\d+)\s*-->`)

// ParseParentMarker returns the parent issue number a body's bare
// (same-repo) marker names, or 0 when there is none. A cross-repo marker
// names an issue in another repo, so it is not reported here; read it with
// ParseParentMarkerRef.
func ParseParentMarker(body string) int {
	repo, n := ParseParentMarkerRef(body)
	if repo != "" {
		return 0
	}
	return n
}

// withParentMarker appends the parent marker to body unless one for the same
// parent is already present. parentRepo is "" for a parent in the child's own
// repo, or the parent's "owner/name" for a cross-repo parent; a bare marker
// for the same number on a cross-repo child is replaced by the full form.
func withParentMarker(body, parentRepo string, parent int) string {
	if parent <= 0 {
		return body
	}
	parentRepo = strings.TrimSpace(parentRepo)
	want := ParentMarkerFor(parentRepo, parent)
	if repo, n := ParseParentMarkerRef(body); n == parent {
		if strings.EqualFold(repo, parentRepo) {
			return body
		}
		if repo == "" && parentRepo != "" {
			return strings.Replace(body, parentMarkerRe.FindString(body), want, 1)
		}
	}
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return want
	}
	return body + "\n\n" + want
}

// splitChildrenOf inverts the ledger: the children recorded under
// parentRepo#parent, sorted. parentRepo is matched as a full "owner/name"
// (case-insensitive); a child filed in another repo (SplitParent.ParentRepo
// set, hivecommons/hive#18680) is returned with its own repo, a same-repo
// child with Repo "".
func splitChildrenOf(children map[string]SplitParent, parentRepo string, parent int) []ChildRef {
	if len(children) == 0 || parent <= 0 {
		return nil
	}
	want := strings.ToLower(strings.TrimSpace(parentRepo))
	var out []ChildRef
	for key, sp := range children {
		if sp.Parent != parent {
			continue
		}
		i := strings.LastIndex(key, "#")
		if i <= 0 {
			continue
		}
		childRepo := strings.ToLower(key[:i])
		n, err := strconv.Atoi(key[i+1:])
		if err != nil || n <= 0 {
			continue
		}
		ownerRepo := childRepo
		if sp.ParentRepo != "" {
			ownerRepo = strings.ToLower(strings.TrimSpace(sp.ParentRepo))
		}
		if ownerRepo != want {
			continue
		}
		ref := ChildRef{Number: n}
		if childRepo != want {
			ref.Repo = childRepo
		} else if n == parent {
			continue
		}
		out = append(out, ref)
	}
	sortChildRefs(out)
	return out
}

// childrenOf resolves a parent's tracked children from what enumeration
// already has in hand: GitHub's sub-issue summary first (it counts
// sub-issues in any repo); failing that, the ledger and body markers
// inverted against the open snapshot. repo is the parent's repo as
// "owner/name". In the fallback a same-repo child absent from the open
// snapshot is counted as closed (the snapshot is every open issue in the
// repo), and a body-marked child that is open counts as open — the marker
// cannot name closed children, so a marker-only parent is exempt from
// stalling but is never auto-closed on that evidence alone
// (closeDecomposedParent re-reads the children). A child in another repo
// (hivecommons/hive#18680) is resolved through crossOpen, that repo's
// newest open snapshot; while nothing is known about that repo it counts as
// open, so a parent is never shown done on missing evidence.
func childrenOf(repo string, parent int, summaries map[int]IssueChildren, ledger map[string]SplitParent, open map[int]*gh.Issue, crossOpen crossRepoOpenFunc) *IssueChildren {
	if s, ok := summaries[parent]; ok && s.Total > 0 {
		s := s
		return &s
	}
	seen := map[ChildRef]bool{}
	for _, ref := range splitChildrenOf(ledger, repo, parent) {
		seen[ref] = true
	}
	for n, issue := range open {
		if n != parent && issue != nil && markerNamesParent(issue.GetBody(), repo, parent) {
			seen[ChildRef{Number: n}] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := &IssueChildren{Total: len(seen)}
	for ref := range seen {
		if ref.Repo == "" {
			if _, isOpen := open[ref.Number]; !isOpen {
				out.Completed++
			}
			continue
		}
		if crossOpen == nil {
			continue
		}
		if isOpen, known := crossOpen(ref.Repo, ref.Number); known && !isOpen {
			out.Completed++
		}
	}
	return out
}

// annotateIssueChildren sets Children on every enumerated row that is a
// decomposed parent.
func annotateIssueChildren(repo string, summaries map[int]IssueChildren, ledger map[string]SplitParent, open map[int]*gh.Issue, crossOpen crossRepoOpenFunc, actionable []Issue, details []RepoWorkIssue) {
	for i := range actionable {
		actionable[i].Children = childrenOf(repo, actionable[i].Number, summaries, ledger, open, crossOpen)
	}
	for i := range details {
		if details[i].Number > 0 {
			details[i].Children = childrenOf(repo, details[i].Number, summaries, ledger, open, crossOpen)
		}
	}
}

// subIssueRef is one child as GET .../issues/{parent}/sub_issues reports it.
// RepositoryURL ("https://api.github.com/repos/owner/name") names the
// child's repo, which differs from the parent's for a cross-repo sub-issue
// (hivecommons/hive#18680); empty means the parent's repo.
type subIssueRef struct {
	Number        int    `json:"number"`
	State         string `json:"state"`
	RepositoryURL string `json:"repository_url,omitempty"`
}

// repo returns the child's lowercased "owner/name" from RepositoryURL, or
// "" when the payload did not carry one.
func (r subIssueRef) repo() string {
	u := strings.TrimRight(strings.TrimSpace(r.RepositoryURL), "/")
	i := strings.LastIndex(u, "/repos/")
	if i < 0 {
		return ""
	}
	full := u[i+len("/repos/"):]
	if strings.Count(full, "/") != 1 {
		return ""
	}
	return strings.ToLower(full)
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
// ledger-recorded child, read from its own repo. Returns the sorted
// children and how many are open; ok is false when nothing could be read.
// A child in another repo (hivecommons/hive#18680) carries that repo.
func (c *Client) decomposedChildren(ctx context.Context, owner, repoName string, parent int) (children []ChildRef, open int, ok bool, err error) {
	parentFull := strings.ToLower(owner + "/" + repoName)
	refs, err := c.listSubIssues(ctx, owner, repoName, parent)
	if err != nil {
		return nil, 0, false, fmt.Errorf("listing sub-issues of %s/%s#%d: %w", owner, repoName, parent, err)
	}
	if len(refs) > 0 {
		for _, r := range refs {
			ref := ChildRef{Number: r.Number}
			if childRepo := r.repo(); childRepo != "" && childRepo != parentFull {
				ref.Repo = childRepo
			}
			if r.Number <= 0 || (ref.Repo == "" && r.Number == parent) {
				continue
			}
			children = append(children, ref)
			if !strings.EqualFold(r.State, "closed") {
				open++
			}
		}
		sortChildRefs(children)
		return children, open, len(children) > 0, nil
	}
	return c.splitLedgerChildren(ctx, owner, repoName, parent)
}

// splitLedgerChildren is the ledger half of decomposedChildren: each child
// the split-parents ledger records under parent, read from its own repo.
func (c *Client) splitLedgerChildren(ctx context.Context, owner, repoName string, parent int) (children []ChildRef, open int, ok bool, err error) {
	parentFull := strings.ToLower(owner + "/" + repoName)
	ledger, lerr := LoadSplitParents("")
	if lerr != nil {
		return nil, 0, false, nil
	}
	for _, ref := range splitChildrenOf(ledger, parentFull, parent) {
		childOwner, childName := owner, repoName
		if ref.Repo != "" {
			childOwner, childName = c.splitRepo(ref.Repo)
		}
		child, _, gerr := c.client.Issues.Get(ctx, childOwner, childName, ref.Number)
		if gerr != nil {
			return nil, 0, false, fmt.Errorf("reading child %s/%s#%d: %w", childOwner, childName, ref.Number, gerr)
		}
		children = append(children, ref)
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
// parent it closes. closers maps child → merged PR number (0 = unknown); a
// cross-repo child's PR lives in the child's repo, so both are written in
// full owner/repo#N form.
func decomposedParentCloseComment(children []ChildRef, closers map[ChildRef]int) string {
	parts := make([]string, 0, len(children))
	for _, ref := range children {
		if pr := closers[ref]; pr > 0 {
			parts = append(parts, fmt.Sprintf("%s → PR %s", ref, ChildRef{Repo: ref.Repo, Number: pr}))
		} else {
			parts = append(parts, ref.String())
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
	// Label/config exemptions cost no API call, so they run first.
	if reason := parentAutoCloseExempt(c.parentAutoClosePolicy(in.Repo), in.Labels); reason != "" {
		if logger != nil {
			logger.Debug("issue stage labels: decomposed parent not auto-closed", "repo", in.Repo, "issue", in.Number, "reason", reason)
		}
		return false
	}
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
	// Only now, with every child confirmed closed, spend a timeline read:
	// a parent a person reopened after a hive auto-close stays open.
	if by, kept := c.parentKeptOpenBy(ctx, owner, name, in.Number, logger); kept || by != "" {
		if logger != nil {
			logger.Info("issue stage labels: decomposed parent kept open, reopened by a person after a hive auto-close", "repo", in.Repo, "issue", in.Number, "reopened_by", by)
		}
		return false
	}
	closers := map[ChildRef]int{}
	for _, ref := range children {
		childOwner, childName := owner, name
		if ref.Repo != "" {
			childOwner, childName = c.splitRepo(ref.Repo)
		}
		if closer, found := c.closedByPullRequestReference(ctx, childOwner, childName, ref.Number, 0); found {
			closers[ref] = closer.Number
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
	note := decomposedParentCloseComment(children, closers)
	if _, err := c.PostIssueNote(ctx, in.Repo, in.Number, prstatus.KeyIssueClose, note, func(ctx context.Context) error {
		return c.CreateIssueCommentBudgeted(ctx, in.Repo, in.Number, note)
	}); err != nil && logger != nil {
		logger.Warn("issue stage labels: decomposed parent close comment failed", "repo", in.Repo, "issue", in.Number, "error", err)
	}
	if logger != nil {
		logger.Info("issue stage labels: decomposed parent closed, all children delivered", "repo", in.Repo, "issue", in.Number, "children", children, "closing_prs", closers)
	}
	return true
}

// parentAutoClosePolicy is the repo's configured parent auto-close policy.
func (c *Client) parentAutoClosePolicy(repo string) string {
	if p := c.issueStagePolicy(); p.ParentAutoClose != nil {
		return p.ParentAutoClose(repo)
	}
	return config.ParentAutoCloseOn
}

// parentAutoCloseExempt returns why a parent must not be auto-closed from
// its policy and labels alone, or "" when it may be (hivecommons/hive#18869).
// "false" exempts every parent; unless the policy is "epics_too" or
// "trackers_too" (which implies it), an issue
// labelled epic (any case) or hold is left for a person to close.
func parentAutoCloseExempt(policy string, labels []string) string {
	if policy == config.ParentAutoCloseOff {
		return "repo parent_autoclose is false"
	}
	if policy == config.ParentAutoCloseEpicsToo || policy == config.ParentAutoCloseTrackersToo {
		return ""
	}
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), "epic") {
			return "labelled epic"
		}
	}
	if HasHoldLabel(labels) {
		return "labelled hold"
	}
	return ""
}

// Reopen-verdict cache (hivecommons/hive#18869). A reopen by a person never
// un-happens, so a "kept open" verdict is cached for the process lifetime
// (a restart re-derives it from the timeline, which is the durable record);
// a "no reopen" verdict is cached briefly so a parent whose close is refused
// each cycle does not cost a timeline read per sweep.
const parentReopenNegativeTTL = 30 * time.Minute

type parentReopenVerdict struct {
	by      string
	checked time.Time
}

var (
	parentReopenMu    sync.Mutex
	parentReopenCache = map[string]parentReopenVerdict{}
)

// parentKeptOpenBy reports the login of a person who reopened the parent
// after a hive auto-close. A timeline that cannot be read fails closed: the
// returned login is "(unknown)" so the parent is not closed this cycle.
func (c *Client) parentKeptOpenBy(ctx context.Context, owner, name string, number int, logger *slog.Logger) (string, bool) {
	key := strings.ToLower(owner+"/"+name) + "#" + strconv.Itoa(number)
	parentReopenMu.Lock()
	v, ok := parentReopenCache[key]
	parentReopenMu.Unlock()
	if ok && (v.by != "" || time.Since(v.checked) < parentReopenNegativeTTL) {
		return v.by, v.by != ""
	}
	events, err := c.IssueTimelineSince(ctx, owner+"/"+name, number, time.Time{})
	if err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: parent timeline unreadable, not closing", "repo", owner+"/"+name, "issue", number, "error", err)
		}
		return "(unknown)", true
	}
	by := reopenedByPersonAfterAutoClose(events)
	parentReopenMu.Lock()
	parentReopenCache[key] = parentReopenVerdict{by: by, checked: time.Now()}
	parentReopenMu.Unlock()
	return by, by != ""
}

// reopenedByPersonAfterAutoClose scans a timeline (oldest first) for a
// `reopened` event by a non-bot actor that follows a hive auto-close: the
// close comment carrying parentAutoCloseMarker, or a `closed` event by a
// bot (the comment is best-effort and can be missing). Returns the reopener.
func reopenedByPersonAfterAutoClose(events []IssueEvent) string {
	autoClosed := false
	for _, ev := range events {
		switch {
		case ev.Event == "commented" && ev.Body != "" && strings.Contains(ev.Body, parentAutoCloseMarker):
			autoClosed = true
		case ev.Event == "closed" && ev.ActorIsBot:
			autoClosed = true
		case ev.Event == "reopened" && autoClosed && !ev.ActorIsBot && ev.Actor != "":
			return ev.Actor
		}
	}
	return ""
}
