package github

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// DefaultTaskListSweepMaxCloses caps the closures a single
// SweepCompletedTaskListIssues tick will perform across all repos, mirroring
// DefaultAutoMergeSweepMaxMerges. A runaway sweep must never spam
// close-and-comment across the fleet.
const DefaultTaskListSweepMaxCloses = 5

// taskListSweepMarker is the invisible HTML-comment tag every sweep-authored
// comment carries as its first line, mirroring the levelHoldNoticePrefix
// pattern in pr_level_hold.go. It exists so ensureSweepComment can find its
// own prior comment on the next cycle and edit it in place instead of stacking
// a fresh progress comment every 15 minutes — the "idempotent progress
// comment" acceptance criterion in #7071.
const taskListSweepMarker = "<!-- hive:task-list-sweep -->"

// taskListSweepScanWindow bounds how far back the merged-PR settle scan
// looks. It matches mergedClaimScanWindow (prclaims.go:84) so that both places
// in the codebase that decide "a merged PR is still relevant" use the same
// 72-hour horizon. Do not widen this without widening the ledger's window too:
// the two are one design.
const taskListSweepScanWindow = mergedClaimScanWindow

const (
	taskListReasonNotHiveFiled  = "not-hive-filed"
	taskListReasonExempt        = "exempt-label"
	taskListReasonHeld          = "held"
	taskListReasonNoBoxes       = "no-task-list"
	taskListReasonHasUnticked   = "unticked-boxes"
	taskListReasonNoMergedPR    = "no-merged-referencing-pr"
	taskListReasonCommentFailed = "comment-failed"
	taskListReasonCloseFailed   = "close-failed"
	taskListReasonPullRequest   = "pull-request"
)

// TaskListSweepOptions mirrors AutoMergeSweepOptions.
type TaskListSweepOptions struct {
	MaxCloses int
	Audit     func(TaskListSweepEvent)
}

// TaskListSweepEvent describes one close. MergedPRs is the list of merged PRs
// whose refs satisfied the "≥1 referencing PR merged" gate — the same list the
// audit comment names, so the dashboard sink can surface exactly what the
// timeline saw.
type TaskListSweepEvent struct {
	Repo       string
	Number     int
	Author     string
	TotalBoxes int
	MergedPRs  []int
}

type TaskListSweepResult struct {
	Closed  []TaskListSweepEvent
	Seen    int
	Skipped int
}

// taskListCheckboxRE matches a GitHub-flavored-markdown task-list item at the
// start of a line, allowing arbitrary leading whitespace so nested / indented
// lists count. Capture group 1 is the box contents — a single space means
// unticked, "x" (case-insensitive) means ticked. Bullet markers `-`, `*` and
// `+` are all valid GFM task-list bullets.
var taskListCheckboxRE = regexp.MustCompile(`^[ \t]*[-*+] \[( |[xX])\](?:\s|$)`)

const issueNeedsHumanLabel = "needs-human"

// fenceOpenRE recognises fenced code blocks. A run of THREE OR MORE backticks
// (or tildes) at the start of a line opens a fence; the same character with
// at least the same count closes it. Boxes inside a fenced block are code
// samples, not task-list items, and must not count — a policy template that
// shows `- [ ]` as an example would otherwise make every hive-filed issue
// that quotes the policy instantly closeable.
var fenceOpenRE = regexp.MustCompile("^[ \t]*(`{3,}|~{3,})")

// countTaskListBoxes returns (checked, unchecked) task-list checkboxes in body,
// ignoring any that appear inside a fenced code block.
func countTaskListBoxes(body string) (checked, unchecked int) {
	inFence := false
	var fenceChar byte
	var fenceLen int
	for _, line := range strings.Split(body, "\n") {
		if m := fenceOpenRE.FindString(line); m != "" {
			trimmed := strings.TrimLeft(m, " \t")
			ch := trimmed[0]
			ln := len(trimmed)
			if !inFence {
				inFence = true
				fenceChar = ch
				fenceLen = ln
				continue
			}
			if ch == fenceChar && ln >= fenceLen {
				inFence = false
				fenceChar = 0
				fenceLen = 0
			}
			continue
		}
		if inFence {
			continue
		}
		m := taskListCheckboxRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[1] == " " {
			unchecked++
		} else {
			checked++
		}
	}
	return checked, unchecked
}

// listTaskListItems returns the raw text of every task-list item in body, in
// document order, with its checked state. Fenced code blocks are excluded just
// like countTaskListBoxes. The item text is everything after "- [ ]" or
// "- [x]" on the line, trimmed — used by the progress comment to list the
// items still outstanding, so a reader knows what work remains without
// scrolling back to the issue body.
func listTaskListItems(body string) (items []taskListItem) {
	inFence := false
	var fenceChar byte
	var fenceLen int
	itemRE := regexp.MustCompile(`^[ \t]*[-*+] \[( |[xX])\]\s*(.*)$`)
	for _, line := range strings.Split(body, "\n") {
		if m := fenceOpenRE.FindString(line); m != "" {
			trimmed := strings.TrimLeft(m, " \t")
			ch := trimmed[0]
			ln := len(trimmed)
			if !inFence {
				inFence = true
				fenceChar = ch
				fenceLen = ln
				continue
			}
			if ch == fenceChar && ln >= fenceLen {
				inFence = false
				fenceChar = 0
				fenceLen = 0
			}
			continue
		}
		if inFence {
			continue
		}
		m := itemRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		items = append(items, taskListItem{
			Checked: m[1] != " ",
			Text:    strings.TrimSpace(m[2]),
		})
	}
	return items
}

type taskListItem struct {
	Checked bool
	Text    string
}

// issueLabelNames extracts the *gh.Label slice's names as strings so the
// canonical Client.isExempt (which takes []string) can be reused unchanged.
func issueLabelNames(labels []*gh.Label) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l == nil {
			continue
		}
		out = append(out, l.GetName())
	}
	return out
}

// SweepCompletedTaskListIssues closes hive-filed open issues whose task-list
// bodies are fully ticked AND for which at least one merged PR references the
// issue. Matches the acceptance criteria in #7071:
//
//   - only closes when 100% of checkboxes are checked AND ≥1 referencing PR is
//     merged (a pre-ticked task list with no work landed does NOT close);
//   - closure comment names the merged PRs that satisfied it;
//   - partial completion posts an idempotent progress comment (updated in
//     place on later cycles via the taskListSweepMarker) listing the
//     outstanding items and the PRs that landed so far — never a duplicate;
//   - respects the existing hold and exempt gates (Client.isHeld,
//     Client.isExempt) rather than adding a second one — a held issue is
//     hands-off for comments, labels and closure exactly as it is for kicks;
//   - does not touch or fight the 72h weak-claim
//     ledger in prclaims.go — that ledger governs AGENT DISPATCH, this sweep
//     governs ISSUE CLOSURE. When this sweep closes an issue the ledger's
//     72h deferral becomes moot naturally (the issue is closed, no dispatch
//     can happen), which is the intended interaction — see #6867.
//
// Cap MaxCloses per tick (mirrors DefaultAutoMergeSweepMaxMerges).
func (c *Client) SweepCompletedTaskListIssues(ctx context.Context, opts TaskListSweepOptions) (*TaskListSweepResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	maxCloses := opts.MaxCloses
	if maxCloses <= 0 {
		maxCloses = DefaultTaskListSweepMaxCloses
	}
	result := &TaskListSweepResult{}
	now := time.Now()
	mergedCutoff := now.Add(-taskListSweepScanWindow)

	for _, repo := range c.getRepos() {
		if len(result.Closed) >= maxCloses {
			break
		}
		owner, repoName := c.splitRepo(repo)
		issues, err := c.listOpenIssuesForTaskListSweep(ctx, owner, repoName)
		if err != nil {
			return result, err
		}
		mergedByIssue, err := c.collectMergedReferencingPRs(ctx, repo, owner, repoName, mergedCutoff)
		if err != nil {
			return result, err
		}
		for _, issue := range issues {
			if len(result.Closed) >= maxCloses {
				break
			}
			if issue == nil {
				continue
			}
			result.Seen++
			event, reason, err := c.trySweepTaskListIssue(ctx, repo, owner, repoName, issue, mergedByIssue)
			if err != nil {
				if c.logger != nil {
					c.logger.Warn("task-list sweep skipped issue", "repo", repo, "issue", issue.GetNumber(), "reason", reason, "error", err)
				}
				result.Skipped++
				continue
			}
			if reason != "" {
				result.Skipped++
				continue
			}
			result.Closed = append(result.Closed, event)
			if opts.Audit != nil {
				opts.Audit(event)
			}
			if c.logger != nil {
				c.logger.Info("task-list sweep closed issue",
					"repo", repo,
					"issue", event.Number,
					"author", event.Author,
					"boxes", event.TotalBoxes,
					"merged_prs", event.MergedPRs,
				)
			}
		}
	}
	return result, nil
}

// listOpenIssuesForTaskListSweep lists every open issue in the repo. Unlike
// listQueuedPullRequestIssues there is no label to key on: a task-list issue is
// recognised by its body, not its labels. Paged so a repo with a long backlog
// still enumerates fully.
func (c *Client) listOpenIssuesForTaskListSweep(ctx context.Context, owner, repo string) ([]*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var all []*gh.Issue
	for {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing open issues for %s/%s: %w", owner, repo, err)
		}
		all = append(all, issues...)
		if resp == nil || resp.NextPage == 0 {
			return all, nil
		}
		opts.ListOptions.Page = resp.NextPage
	}
}

// mergedPRRef is one merged PR whose title/body references an issue via a
// closing keyword or a bare `Refs #N`. Both flavours count for this sweep's
// gate: #7071 is explicit that a merged `Refs #N` is exactly the case the
// sweep exists to remediate.
type mergedPRRef struct {
	Number     int
	Title      string
	URL        string
	MergedAt   time.Time
	Reference  bool
	Remainder  string
	NeedsHuman bool
}

// collectMergedReferencingPRs walks the repo's closed-PR list once per tick
// (mirroring the FetchClaims merged-PR settle scan at prclaims.go:558-599 for
// pagination shape, cutoff handling and page-halting), extracts closing and
// non-closing issue references from each merged PR, and returns a map keyed by
// referenced-issue number. The map is looked up per candidate in
// trySweepTaskListIssue, so the merged-PR enumeration cost is amortised across
// every open issue in the repo — one list per repo per tick, not one per
// candidate.
func (c *Client) collectMergedReferencingPRs(ctx context.Context, displayRepo, owner, repo string, mergedCutoff time.Time) (map[int][]mergedPRRef, error) {
	out := map[int][]mergedPRRef{}
	opts := &gh.PullRequestListOptions{
		State:       "closed",
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: claimSearchPerPage},
	}
	for page := 0; page < claimSearchMaxPages; page++ {
		prs, resp, err := c.client.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing merged PRs for %s/%s: %w", owner, repo, err)
		}
		pastWindow := false
		for _, pr := range prs {
			if pr == nil {
				continue
			}
			if pr.GetUpdatedAt().Time.Before(mergedCutoff) {
				pastWindow = true
				break
			}
			mergedAt := pr.GetMergedAt().Time
			if mergedAt.IsZero() || mergedAt.Before(mergedCutoff) {
				continue
			}
			text := pr.GetTitle() + "\n" + pr.GetBody()
			bodyRefs := refsRemainderMetadata(pr.GetBody(), displayRepo)
			// Both closing refs (Fixes/Closes) and non-closing refs (Refs)
			// count. #7071 exists precisely because a merged `Refs #N`
			// leaves the issue open; the sweep must therefore accept it.
			refs := append(ParseClaimedIssues(text, displayRepo), ParseReferencedIssues(text, displayRepo)...)
			seen := map[int]bool{}
			for _, ref := range refs {
				if !strings.EqualFold(ref.Repo, displayRepo) {
					continue
				}
				if seen[ref.Issue] {
					continue
				}
				seen[ref.Issue] = true
				meta := bodyRefs[claimKey(strings.ToLower(ref.Repo), ref.Issue)]
				out[ref.Issue] = append(out[ref.Issue], mergedPRRef{
					Number:     pr.GetNumber(),
					Title:      pr.GetTitle(),
					URL:        pr.GetHTMLURL(),
					MergedAt:   mergedAt,
					Reference:  meta.Reference,
					Remainder:  meta.Remainder,
					NeedsHuman: meta.NeedsHuman,
				})
			}
		}
		if pastWindow || resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

type refsRemainderMeta struct {
	Reference          bool
	Remainder          string
	NeedsHuman         bool
	ExplicitNeedsHuman bool
}

var explicitNeedsHumanRefRE = regexp.MustCompile(`(?i)\b(?:refs|ref|references|referencing)\b[^\n#]{0,40}?(?:([\w.-]+/[\w.-]+))?#(\d+)\s*\(\s*needs-human\s*:\s*([^)]+)\)`)

// refsRemainderMetadata extracts the auditable remainder text for non-closing
// Refs lines. The explicit needs-human marker is authoritative; prose only
// counts when it appears in a deliberately named remainder section.
func refsRemainderMetadata(body, defaultRepo string) map[string]refsRemainderMeta {
	out := map[string]refsRemainderMeta{}
	refsByKey := map[string]ClaimedRef{}
	section := extractRemainderSection(body)
	for _, line := range strings.Split(body, "\n") {
		for _, ref := range ParseReferencedIssues(line, defaultRepo) {
			key := claimKey(strings.ToLower(ref.Repo), ref.Issue)
			refsByKey[key] = ref
			meta := out[key]
			meta.Reference = true
			if meta.Remainder == "" {
				meta.Remainder = refsLineReason(line, ref, defaultRepo)
			}
			out[key] = meta
		}
	}
	for _, match := range explicitNeedsHumanRefRE.FindAllStringSubmatch(body, -1) {
		if len(match) < 4 {
			continue
		}
		issue, err := strconv.Atoi(match[2])
		if err != nil || issue <= 0 {
			continue
		}
		repo := match[1]
		if repo == "" {
			repo = defaultRepo
		}
		key := claimKey(strings.ToLower(repo), issue)
		refsByKey[key] = ClaimedRef{Repo: repo, Issue: issue}
		meta := out[key]
		meta.Reference = true
		meta.NeedsHuman = true
		meta.ExplicitNeedsHuman = true
		if reason := strings.TrimSpace(match[3]); reason != "" {
			meta.Remainder = reason
		}
		out[key] = meta
	}
	if strings.TrimSpace(section) != "" {
		for key, meta := range out {
			sectionForRef := ""
			if sectionHasIssueScopedBullets(section) {
				if ref, ok := refsByKey[key]; ok {
					sectionForRef = issueScopedRemainderSection(section, ref, defaultRepo)
				}
			} else if len(out) == 1 {
				sectionForRef = strings.TrimSpace(section)
			}
			if sectionForRef != "" {
				meta.Remainder = sectionForRef
				meta.NeedsHuman = meta.NeedsHuman || remainderSectionNeedsHuman(sectionForRef)
			}
			out[key] = meta
		}
	}
	return out
}

func sectionHasIssueScopedBullets(section string) bool {
	for _, line := range strings.Split(section, "\n") {
		if isMarkdownBullet(line) && issueMentionRE.MatchString(line) {
			return true
		}
	}
	return false
}

func issueScopedRemainderSection(section string, ref ClaimedRef, defaultRepo string) string {
	var scoped []string
	lines := strings.Split(section, "\n")
	for i := 0; i < len(lines); i++ {
		if !isMarkdownBullet(lines[i]) || !lineMentionsIssue(lines[i], ref, defaultRepo) {
			continue
		}
		scoped = append(scoped, lines[i])
		for j := i + 1; j < len(lines); j++ {
			if isMarkdownBullet(lines[j]) {
				break
			}
			scoped = append(scoped, lines[j])
		}
	}
	return strings.TrimSpace(strings.Join(scoped, "\n"))
}

func isMarkdownBullet(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "-") || strings.HasPrefix(trimmed, "*") || strings.HasPrefix(trimmed, "+")
}

var issueMentionRE = regexp.MustCompile(`(?:([\w.-]+/[\w.-]+))?#(\d+)`)

func lineMentionsIssue(line string, ref ClaimedRef, defaultRepo string) bool {
	for _, match := range issueMentionRE.FindAllStringSubmatch(line, -1) {
		if len(match) < 3 {
			continue
		}
		issue, err := strconv.Atoi(match[2])
		if err != nil || issue != ref.Issue {
			continue
		}
		repo := match[1]
		if repo == "" {
			repo = defaultRepo
		}
		if strings.EqualFold(repo, ref.Repo) {
			return true
		}
	}
	return false
}

func refsLineReason(line string, ref ClaimedRef, defaultRepo string) string {
	matches := referenceRefPattern.FindAllStringSubmatchIndex(line, -1)
	for _, idx := range matches {
		if len(idx) < 8 {
			continue
		}
		issue, err := strconv.Atoi(line[idx[6]:idx[7]])
		if err != nil || issue != ref.Issue {
			continue
		}
		repo := defaultRepo
		if idx[4] >= 0 {
			repo = line[idx[4]:idx[5]]
		}
		if !strings.EqualFold(repo, ref.Repo) {
			continue
		}
		return cleanRemainderReason(line[idx[1]:])
	}
	return ""
}

func cleanRemainderReason(reason string) string {
	reason = strings.TrimSpace(reason)
	reason = explicitNeedsHumanRefRE.ReplaceAllString(reason, "")
	reason = strings.TrimSpace(reason)
	reason = strings.TrimLeft(reason, "—-:;., \t")
	return strings.TrimSpace(reason)
}

func extractRemainderSection(body string) string {
	lines := strings.Split(body, "\n")
	start := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}
		heading := strings.TrimSpace(strings.TrimLeft(trimmed, "#"))
		heading = strings.TrimSpace(strings.Trim(heading, "*:"))
		lower := strings.ToLower(heading)
		if strings.Contains(lower, "what remains") || strings.Contains(lower, "what this deliberately leaves undone") {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := len(lines)
	for i := start; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			end = i
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n"))
}

func remainderSectionNeedsHuman(section string) bool {
	lower := strings.ToLower(section)
	for _, phrase := range []string{
		"needs a human",
		"need a human",
		"requires a human",
		"human-only",
		"human must",
		"must be applied by hand",
		"must be done by a human",
		"must be made by a human",
	} {
		if containsAffirmativeHumanOnlyPhrase(lower, phrase) {
			return true
		}
	}
	return false
}

func containsAffirmativeHumanOnlyPhrase(text, phrase string) bool {
	for start := 0; ; {
		idx := strings.Index(text[start:], phrase)
		if idx < 0 {
			return false
		}
		abs := start + idx
		if !directlyNegatesHumanPhrase(text[:abs]) {
			return true
		}
		start = abs + len(phrase)
	}
}

func directlyNegatesHumanPhrase(prefix string) bool {
	prefix = strings.TrimSpace(prefix)
	for _, negation := range []string{"does not", "doesn't", "do not", "don't", "not", "no", "never"} {
		if strings.HasSuffix(prefix, negation) {
			return true
		}
	}
	return false
}

func hasIssueNeedsHumanLabel(labels []string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), issueNeedsHumanLabel) {
			return true
		}
	}
	return false
}

func (c *Client) tryAnnotateNonTaskListRefs(ctx context.Context, owner, repo string, number int, labels []string, mergedRefs []mergedPRRef) error {
	refs := make([]mergedPRRef, 0, len(mergedRefs))
	needsHuman := false
	for _, ref := range mergedRefs {
		if !ref.Reference {
			continue
		}
		refs = append(refs, ref)
		needsHuman = needsHuman || ref.NeedsHuman
	}
	if len(refs) == 0 {
		return nil
	}
	if err := c.ensureRefsRemainderComment(ctx, owner, repo, number, renderRefsRemainderComment(refs)); err != nil {
		return err
	}
	if needsHuman && !hasIssueNeedsHumanLabel(labels) {
		_, _, err := c.client.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{issueNeedsHumanLabel})
		if err != nil {
			return fmt.Errorf("adding %s label to %s/%s#%d: %w", issueNeedsHumanLabel, owner, repo, number, err)
		}
	}
	return nil
}

func (c *Client) ensureRefsRemainderComment(ctx context.Context, owner, repo string, number int, desiredBody string) error {
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return err
	}
	desiredPRs := sweepCommentPRNumbers(desiredBody)
	for _, cm := range comments {
		if cm == nil || !strings.Contains(cm.GetBody(), taskListSweepMarker) {
			continue
		}
		if cm.GetBody() == desiredBody {
			return nil
		}
		currentPRs := sweepCommentPRNumbers(cm.GetBody())
		if hasPRsOutsideSet(currentPRs, desiredPRs) {
			desiredBody = mergeRefsRemainderCommentBodies(cm.GetBody(), desiredBody)
			if cm.GetBody() == desiredBody {
				return nil
			}
		}
		_, _, err := c.client.Issues.EditComment(ctx, owner, repo, cm.GetID(), &gh.IssueComment{Body: gh.Ptr(desiredBody)})
		if err != nil {
			return fmt.Errorf("editing Refs remainder comment on %s/%s#%d: %w", owner, repo, number, err)
		}
		return nil
	}
	_, _, err = c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(desiredBody)})
	if err != nil {
		return fmt.Errorf("creating Refs remainder comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

var sweepCommentPRLineRE = regexp.MustCompile(`(?m)^- #(\d+)\b.*https?://\S+/pull/\d+`)

func sweepCommentPRNumbers(body string) map[int]bool {
	out := map[int]bool{}
	for _, match := range sweepCommentPRLineRE.FindAllStringSubmatch(body, -1) {
		if len(match) < 2 {
			continue
		}
		n, err := strconv.Atoi(match[1])
		if err == nil && n > 0 {
			out[n] = true
		}
	}
	return out
}

func hasPRsOutsideSet(have, want map[int]bool) bool {
	for n := range have {
		if !want[n] {
			return true
		}
	}
	return false
}

func mergeRefsRemainderCommentBodies(existingBody, desiredBody string) string {
	existingBlocks := sweepCommentPRBlocks(existingBody)
	desiredPRs := sweepCommentPRNumbers(desiredBody)
	var agedOut []int
	for n := range existingBlocks {
		if !desiredPRs[n] {
			agedOut = append(agedOut, n)
		}
	}
	if len(agedOut) == 0 {
		return desiredBody
	}
	sort.Ints(agedOut)
	footer := "_This comment is edited in place by the task-list sweep on every cycle; it is not duplicated._"
	insertAt := strings.Index(desiredBody, footer)
	if insertAt < 0 {
		insertAt = len(desiredBody)
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(desiredBody[:insertAt], "\n"))
	b.WriteString("\n")
	for _, n := range agedOut {
		b.WriteString("\n")
		b.WriteString(strings.TrimRight(existingBlocks[n], "\n"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(desiredBody[insertAt:])
	return b.String()
}

func sweepCommentPRBlocks(body string) map[int]string {
	blocks := map[int]string{}
	lines := strings.Split(body, "\n")
	for i := 0; i < len(lines); i++ {
		match := sweepCommentPRLineRE.FindStringSubmatch(lines[i])
		if len(match) < 2 {
			continue
		}
		n, err := strconv.Atoi(match[1])
		if err != nil || n <= 0 {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if sweepCommentPRLineRE.MatchString(lines[j]) || strings.HasPrefix(lines[j], "_This comment is edited in place") {
				end = j
				break
			}
		}
		blocks[n] = strings.Join(lines[i:end], "\n")
	}
	return blocks
}

func renderRefsRemainderComment(merged []mergedPRRef) string {
	var b strings.Builder
	fmt.Fprintln(&b, taskListSweepMarker)
	fmt.Fprintln(&b, "A merged PR referenced this issue without closing it. Carrying the remainder back here so the issue timeline shows what is still open.")
	fmt.Fprintln(&b)
	for _, ref := range sortedMergedRefs(merged) {
		title := strings.TrimSpace(ref.Title)
		fmt.Fprintf(&b, "- #%d", ref.Number)
		if title != "" {
			fmt.Fprintf(&b, " — %s", title)
		}
		if ref.URL != "" {
			fmt.Fprintf(&b, " (%s)", ref.URL)
		}
		if ref.NeedsHuman {
			fmt.Fprint(&b, " — needs human")
		}
		fmt.Fprintln(&b)
		if strings.TrimSpace(ref.Remainder) != "" {
			fmt.Fprintln(&b)
			fmt.Fprintln(&b, "Remainder from the PR body:")
			fmt.Fprintln(&b)
			fmt.Fprintln(&b, strings.TrimSpace(ref.Remainder))
			fmt.Fprintln(&b)
		}
	}
	fmt.Fprintln(&b, "_This comment is edited in place by the task-list sweep on every cycle; it is not duplicated._")
	return b.String()
}

func sortedMergedRefs(merged []mergedPRRef) []mergedPRRef {
	sorted := append([]mergedPRRef(nil), merged...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
	return sorted
}

// trySweepTaskListIssue evaluates one issue against every gate in order. reason
// is a non-empty skip code when the issue is ineligible; err is non-nil only
// for API failures the caller should log.
func (c *Client) trySweepTaskListIssue(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, mergedByIssue map[int][]mergedPRRef) (TaskListSweepEvent, string, error) {
	if issue.IsPullRequest() {
		return TaskListSweepEvent{}, taskListReasonPullRequest, nil
	}
	if !c.isHiveFiledIssue(issue) {
		return TaskListSweepEvent{}, taskListReasonNotHiveFiled, nil
	}
	labels := issueLabelNames(issue.Labels)
	// Hold outranks everything below: a held issue must not be commented on,
	// relabelled, or closed by this sweep any more than it may be dispatched
	// (#8927). Client.isHeld is the enumeration predicate, so the generic
	// hold substrings and the exact dashboard `hive-pause/<hive-id>` both
	// count.
	if c.isHeld(labels) {
		return TaskListSweepEvent{}, taskListReasonHeld, nil
	}
	if c.isExempt(labels) || hasIssueNeedsHumanLabel(labels) {
		return TaskListSweepEvent{}, taskListReasonExempt, nil
	}
	body := issue.GetBody()
	checked, unchecked := countTaskListBoxes(body)
	number := issue.GetNumber()

	mergedRefs := mergedByIssue[number]
	if checked+unchecked == 0 {
		if err := c.tryAnnotateNonTaskListRefs(ctx, owner, repo, number, issueLabelNames(issue.Labels), mergedRefs); err != nil {
			return TaskListSweepEvent{}, taskListReasonCommentFailed, fmt.Errorf("Refs remainder comment on %s#%d: %w", displayRepo, number, err)
		}
		return TaskListSweepEvent{}, taskListReasonNoBoxes, nil
	}
	if len(mergedRefs) == 0 {
		// #7071 gate: no merged referencing PR ⇒ the task list has not been
		// answered by anything landed on main yet. A pre-ticked list with no
		// work behind it must not auto-close.
		return TaskListSweepEvent{}, taskListReasonNoMergedPR, nil
	}

	if unchecked > 0 {
		// Partial completion: post/update the idempotent progress comment,
		// then leave the issue open. This path exists so a maintainer looking
		// at the timeline sees a live "N of M done, PRs so far" without a
		// duplicate comment on every 15-minute cycle.
		progress := renderProgressComment(listTaskListItems(body), mergedRefs)
		if err := c.ensureSweepComment(ctx, owner, repo, number, progress); err != nil {
			return TaskListSweepEvent{}, taskListReasonCommentFailed, fmt.Errorf("progress comment on %s#%d: %w", displayRepo, number, err)
		}
		return TaskListSweepEvent{}, taskListReasonHasUnticked, nil
	}

	// Full completion. Update the sweep's marker comment to a closure body
	// naming the merged PRs, then close.
	closure := renderClosureComment(checked, mergedRefs)
	if err := c.ensureSweepComment(ctx, owner, repo, number, closure); err != nil {
		if githubStatusError(err, http.StatusNotFound) {
			return TaskListSweepEvent{}, "gone", nil
		}
		return TaskListSweepEvent{}, taskListReasonCommentFailed, fmt.Errorf("closure comment on %s#%d: %w", displayRepo, number, err)
	}
	if _, _, err := c.client.Issues.Edit(ctx, owner, repo, number, &gh.IssueRequest{State: gh.Ptr("closed")}); err != nil {
		if githubStatusError(err, http.StatusNotFound) {
			return TaskListSweepEvent{}, "gone", nil
		}
		return TaskListSweepEvent{}, taskListReasonCloseFailed, fmt.Errorf("closing %s#%d after task-list sweep comment: %w", displayRepo, number, err)
	}

	prNums := make([]int, 0, len(mergedRefs))
	for _, r := range mergedRefs {
		prNums = append(prNums, r.Number)
	}
	sort.Ints(prNums)

	return TaskListSweepEvent{
		Repo:       displayRepo,
		Number:     number,
		Author:     safeGetLogin(issue.GetUser()),
		TotalBoxes: checked,
		MergedPRs:  prNums,
	}, "", nil
}

// renderProgressComment builds the marker-prefixed body posted while an issue's
// task list is only partially checked. The layout is stable — one bullet per
// task, ✅ / 🔲 to make the outstanding items scannable at a glance, then a
// PRs-so-far list — so the equality check inside ensureSweepComment can short
// out and skip the API edit call on ticks where nothing has changed.
func renderProgressComment(items []taskListItem, merged []mergedPRRef) string {
	var b strings.Builder
	fmt.Fprintln(&b, taskListSweepMarker)
	checked := 0
	for _, it := range items {
		if it.Checked {
			checked++
		}
	}
	fmt.Fprintf(&b, "task-list sweep: **%d of %d** items ticked. Not closing yet — outstanding boxes remain.\n\n", checked, len(items))
	fmt.Fprintln(&b, "Outstanding items:")
	for _, it := range items {
		if it.Checked {
			continue
		}
		text := it.Text
		if text == "" {
			text = "(unnamed item)"
		}
		fmt.Fprintf(&b, "- 🔲 %s\n", text)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "Merged PRs referencing this issue so far:")
	writeMergedList(&b, merged)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "_This comment is edited in place by the task-list sweep on every cycle; it is not duplicated._")
	return b.String()
}

// renderClosureComment builds the marker-prefixed body posted immediately
// before the close call. Names the merged PRs that satisfied the gate so the
// action is auditable from the timeline alone.
func renderClosureComment(boxes int, merged []mergedPRRef) string {
	var b strings.Builder
	fmt.Fprintln(&b, taskListSweepMarker)
	fmt.Fprintf(&b, "task-list sweep: closing this issue. All %d task-list box(es) in the body are ticked and the following merged PR(s) reference it:\n\n", boxes)
	writeMergedList(&b, merged)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "If this was premature, reopen the issue and uncheck one of the boxes.")
	return b.String()
}

// writeMergedList renders merged PRs deterministically (ascending by number) so
// two ticks whose merged set is unchanged produce byte-identical bodies — the
// property ensureSweepComment relies on to elide no-op edits.
func writeMergedList(b *strings.Builder, merged []mergedPRRef) {
	for _, r := range sortedMergedRefs(merged) {
		title := strings.TrimSpace(r.Title)
		if r.URL != "" {
			fmt.Fprintf(b, "- #%d — %s\n", r.Number, title)
		} else {
			fmt.Fprintf(b, "- #%d — %s\n", r.Number, title)
		}
	}
}

// ensureSweepComment posts desiredBody as the sweep's marker comment on issue
// number, or edits its prior marker comment in place if the desired body
// differs. Never posts twice.
//
// Follows the pattern in ensureLevelHoldNotice (pr_level_hold.go): find the
// marker in the comment list, decide create vs edit vs no-op accordingly. The
// marker is at the top of every sweep-authored body so the scan is a simple
// substring check.
func (c *Client) ensureSweepComment(ctx context.Context, owner, repo string, number int, desiredBody string) error {
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return err
	}
	for _, cm := range comments {
		if cm == nil || !strings.Contains(cm.GetBody(), taskListSweepMarker) {
			continue
		}
		// Same body ⇒ already up to date, nothing to do.
		if cm.GetBody() == desiredBody {
			return nil
		}
		_, _, err := c.client.Issues.EditComment(ctx, owner, repo, cm.GetID(), &gh.IssueComment{Body: gh.Ptr(desiredBody)})
		if err != nil {
			return fmt.Errorf("editing task-list sweep comment on %s/%s#%d: %w", owner, repo, number, err)
		}
		return nil
	}
	_, _, err = c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(desiredBody)})
	if err != nil {
		return fmt.Errorf("creating task-list sweep comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}

// isHiveFiledIssue reports whether issue was filed by the hive itself.
// Fail-CLOSED: only returns true when hive authorship is affirmatively
// provable — ambiguity keeps the issue open.
//
// Two independent positive signals, either sufficient:
//
//  1. Body carries AttributionTrailerPrefix ("— hive:"). Every hive-mediated
//     create is stamped by AppendTrailer with that greppable marker
//     (attribution.go:40, and see the comment at pr_request_claims.go
//     confirming "Every hive-mediated create is stamped by AppendTrailer with
//     that greppable marker").
//  2. User.Type is "Bot" (case-insensitive) AND the login matches this Hive
//     App's configured bot login. App-installation-token-authored issues carry
//     User.Type == "Bot", but other GitHub Apps can also file issues in the
//     same repo; those are not ours to close.
//
// Do NOT reuse isHumanFiledBugReport as an inverse test: it is fail-OPEN on
// ambiguity (its own doc says "an agent's bug finding stays closeable, and a
// maintainer's bug is protected"), and it returns false immediately for any
// issue lacking a bug-family label, so a human-filed issue with no `bug` label
// would slip past — the exact outcome this sweep must never produce.
func (c *Client) isHiveFiledIssue(issue *gh.Issue) bool {
	if issue == nil {
		return false
	}
	if HasAttributionTrailer(issue.GetBody()) {
		return true
	}
	if issue.User != nil && strings.EqualFold(issue.User.GetType(), "Bot") && strings.TrimSpace(c.appBotLogin) != "" && strings.EqualFold(safeGetLogin(issue.User), c.appBotLogin) {
		return true
	}
	return false
}
