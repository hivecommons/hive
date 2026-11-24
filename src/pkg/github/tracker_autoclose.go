package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/gfi"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Tracker and umbrella auto-close (hivecommons/hive#19071).
//
// parent_autoclose closes decomposed parents: issues whose sub-issue children
// are all closed. Many umbrella and audit trackers are not decomposed parents.
// They have no sub-issue children and are referenced by `Refs`-only PRs, so
// nothing closed them when the work was finished. Under v5 a merge never
// closes an issue, so "done" is computed here from the state of what the
// tracker references, never from close keywords.
//
// The rule is opt-in per repo (repos.<repo>.parent_autoclose: trackers_too,
// which implies epics_too) and off by default. An issue is a candidate only
// when it announces itself as a tracker: an umbrella by the shared classifier
// (label, marker, title prefix, task list of issue references), an audit or
// roadmap label, or a close_when checklist.
// A design or feature issue that merely has `Refs` PRs is not a candidate, so
// partial work never closes it (the failure that closed #18596).
//
// The set of references is, in order of authority:
//
//  1. a `close_when` checklist in the body, when present: the author lists
//     exactly what must be finished, and the tracker is done when every item
//     is checked or (for an item that names an issue or PR) closed;
//  2. otherwise every task-list item that names an issue or PR, plus every
//     PR that references the tracker with Refs (or a closing keyword).
//
// The tracker is done when that set is non-empty and every PR in it is closed
// or merged and every issue in it is closed. Safety: hive/tracking issues and
// hold-labelled issues are never closed, a person's reopen is final, an
// unreadable reference or timeline means no close that cycle, and the close
// needs the same evidence on two reads trackerSettleInterval apart.

const (
	// trackerSettleInterval is both the per-tracker read cache lifetime and
	// the settle delay: a tracker closes only when a fresh read, at least
	// this long after the first one, sees the same finished evidence, so it
	// is not closed between two PRs landing.
	trackerSettleInterval = 15 * time.Minute
	// trackerMaxReferences bounds the reads one tracker can cost. A tracker
	// that references more is never auto-closed.
	trackerMaxReferences = 50
	// trackerEvidenceMarker identifies the evidence a close comment was
	// written for, so a close that failed after its comment is not repeated.
	trackerEvidenceMarker = "<!-- hive:tracker-autoclose evidence="
)

var (
	trackerTaskRe      = regexp.MustCompile(`^[ \t]*[-*][ \t]+\[([ xX])\][ \t]*(.*)$`)
	trackerRefRe       = regexp.MustCompile(`(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#|(?:^|[^\w&/])#)(\d+)`)
	trackerCloseWhenRe = regexp.MustCompile(`(?i)^[ \t]*(?:#{1,6}[ \t]*)?close[_ ]when\b`)
	trackerLabels      = map[string]bool{"audit": true, "roadmap": true}
)

// trackerTask is one task-list item.
type trackerTask struct {
	Checked bool
	Text    string
	Ref     ChildRef
	HasRef  bool
}

// parseTrackerTask parses one body line as a task-list item.
func parseTrackerTask(line string) (trackerTask, bool) {
	m := trackerTaskRe.FindStringSubmatch(line)
	if m == nil {
		return trackerTask{}, false
	}
	t := trackerTask{Checked: m[1] != " ", Text: strings.TrimSpace(m[2])}
	if r := trackerRefRe.FindStringSubmatch(m[2]); r != nil {
		if n, err := strconv.Atoi(r[2]); err == nil && n > 0 {
			t.HasRef = true
			t.Ref = ChildRef{Repo: strings.ToLower(r[1]), Number: n}
		}
	}
	return t, true
}

// parseTrackerTasks returns every task-list item in body, in order.
func parseTrackerTasks(body string) []trackerTask {
	var out []trackerTask
	for _, line := range strings.Split(body, "\n") {
		if t, ok := parseTrackerTask(strings.TrimRight(line, "\r")); ok {
			out = append(out, t)
		}
	}
	return out
}

// parseCloseWhen returns the checklist under a `close_when` heading (or a
// `close_when:` line) and whether the body has one. The checklist is the
// task-list items that follow it, up to the next heading or prose line.
func parseCloseWhen(body string) ([]trackerTask, bool) {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if !trackerCloseWhenRe.MatchString(strings.TrimRight(line, "\r")) {
			continue
		}
		var out []trackerTask
		for _, next := range lines[i+1:] {
			next = strings.TrimRight(next, "\r")
			if strings.TrimSpace(next) == "" {
				continue
			}
			t, ok := parseTrackerTask(next)
			if !ok {
				break
			}
			out = append(out, t)
		}
		return out, true
	}
	return nil, false
}

// trackerSignal reports whether an issue announces itself as a tracker: a
// close_when checklist, anything the shared umbrella classifier recognises
// (umbrella/tracker/epic label, umbrella marker, [Tracker]-style title prefix
// or a task list of two or more issue references, hivecommons/hive#19078), or
// an audit or roadmap label.
func trackerSignal(title string, labels []string, body string) bool {
	for _, l := range labels {
		if trackerLabels[strings.ToLower(strings.TrimSpace(l))] {
			return true
		}
	}
	if _, ok := parseCloseWhen(body); ok {
		return true
	}
	_, umbrella := gfi.UmbrellaPolicy{}.UmbrellaReason(title, labels, body, 0)
	return umbrella
}

// trackerRefersTo reports whether a PR body references issue number in repo
// with Refs or a closing keyword (a bare #N or the full owner/repo#N).
func trackerRefersTo(prBody, repoFull string, number int) bool {
	re := regexp.MustCompile(`(?i)\b(?:refs?|references?|fix(?:es|ed)?|close[sd]?|resolve[sd]?|part of|towards?)\b[ \t]*:?[ \t]+(?:` +
		regexp.QuoteMeta(repoFull) + `)?#` + strconv.Itoa(number) + `\b`)
	return re.MatchString(prBody)
}

// trackerItem is one piece of referenced work and whether it is finished.
type trackerItem struct {
	Ref   ChildRef
	PR    bool
	State string
	Done  bool
	// Text is set for a close_when checklist line that names no issue or PR;
	// it is finished only when its box is ticked.
	Text string
}

// trackerTextMax bounds how much of a checklist line the evidence quotes.
const trackerTextMax = 80

func (i trackerItem) String() string {
	if i.Text != "" {
		text := i.Text
		if len(text) > trackerTextMax {
			text = text[:trackerTextMax] + "..."
		}
		return fmt.Sprintf("checklist item %q: %s", text, i.State)
	}
	kind := "issue"
	if i.PR {
		kind = "PR"
	}
	return fmt.Sprintf("%s %s: %s", kind, i.Ref, i.State)
}

// trackerEvidenceHash fingerprints the evidence so two reads can be compared.
func trackerEvidenceHash(items []trackerItem) string {
	lines := make([]string, 0, len(items))
	for _, it := range items {
		lines = append(lines, it.String())
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:8])
}

func trackerAllDone(items []trackerItem) bool {
	if len(items) == 0 {
		return false
	}
	for _, it := range items {
		if !it.Done {
			return false
		}
	}
	return true
}

// trackerEvidenceComment is the audit comment left before the close.
func trackerEvidenceComment(items []trackerItem, explicit bool, hash string) string {
	var b strings.Builder
	b.WriteString(parentAutoCloseMarker + "\n" + trackerEvidenceMarker + hash + " -->\n")
	b.WriteString("Everything this tracker references is finished:\n\n")
	for _, it := range items {
		b.WriteString("- " + it.String() + "\n")
	}
	if explicit {
		b.WriteString("\nThe list above is this issue's `close_when` checklist, so the hive is closing it as completed.")
	} else {
		b.WriteString("\nThe list above is every issue and PR this tracker references (task-list items and PRs that reference it), so the hive is closing it as completed.")
	}
	b.WriteString(" Reopen it if something is still missing; a reopen by a person is never overridden.")
	return b.String()
}

// trackerReadRecord is the per-tracker cache: the last read's evidence and
// when it was taken, plus the fingerprint of the previous finished read.
type trackerReadRecord struct {
	at       time.Time
	hash     string
	doneHash string
	doneAt   time.Time
}

var (
	trackerMu    sync.Mutex
	trackerReads = map[string]trackerReadRecord{}
)

// trackerReadDue reports whether the tracker has to be read again.
func trackerReadDue(key string, now time.Time) bool {
	trackerMu.Lock()
	defer trackerMu.Unlock()
	rec, ok := trackerReads[key]
	return !ok || now.Sub(rec.at) >= trackerSettleInterval
}

// trackerSettled records a fresh read and reports whether the same finished
// evidence was already seen by an earlier read at least trackerSettleInterval
// ago. Unfinished evidence clears the settle state.
func trackerSettled(key string, now time.Time, hash string, done bool) bool {
	trackerMu.Lock()
	defer trackerMu.Unlock()
	rec := trackerReads[key]
	settled := done && rec.doneHash == hash && !rec.doneAt.IsZero() && now.Sub(rec.doneAt) >= trackerSettleInterval
	next := trackerReadRecord{at: now, hash: hash}
	switch {
	case !done:
	case rec.doneHash == hash && !rec.doneAt.IsZero():
		next.doneHash, next.doneAt = rec.doneHash, rec.doneAt
	default:
		next.doneHash, next.doneAt = hash, now
	}
	trackerReads[key] = next
	return settled
}

// readTrackerItem reads one referenced issue or PR from its own repo.
func (c *Client) readTrackerItem(ctx context.Context, owner, name string, ref ChildRef) (trackerItem, *gh.Issue, error) {
	itemOwner, itemName := owner, name
	if ref.Repo != "" {
		itemOwner, itemName = c.splitRepo(ref.Repo)
	}
	issue, _, err := c.client.Issues.Get(ctx, itemOwner, itemName, ref.Number)
	if err != nil {
		return trackerItem{}, nil, fmt.Errorf("reading %s/%s#%d: %w", itemOwner, itemName, ref.Number, err)
	}
	state := strings.ToLower(issue.GetState())
	return trackerItem{Ref: ref, PR: issue.IsPullRequest(), State: state, Done: state == "closed"}, issue, nil
}

// trackerEvidence resolves what the tracker references and the state of each
// reference. explicit is true when the set came from a close_when checklist.
// Any unreadable reference is an error: the tracker is not closed that cycle.
func (c *Client) trackerEvidence(ctx context.Context, owner, name string, number int, body string) (items []trackerItem, explicit bool, err error) {
	repoFull := strings.ToLower(owner + "/" + name)
	seen := map[ChildRef]bool{{Number: number}: true}
	add := func(t trackerTask) (bool, error) {
		ref := t.Ref
		if strings.EqualFold(ref.Repo, repoFull) {
			ref.Repo = ""
		}
		if seen[ref] {
			return true, nil
		}
		seen[ref] = true
		it, _, rerr := c.readTrackerItem(ctx, owner, name, ref)
		if rerr != nil {
			return false, rerr
		}
		it.Done = it.Done || t.Checked
		items = append(items, it)
		return true, nil
	}

	if closeWhen, ok := parseCloseWhen(body); ok {
		explicit = true
		for _, t := range closeWhen {
			if len(items) >= trackerMaxReferences {
				return nil, explicit, fmt.Errorf("close_when lists more than %d items", trackerMaxReferences)
			}
			if !t.HasRef {
				// A checklist line with no issue or PR is done only when ticked.
				it := trackerItem{Text: t.Text, State: "unchecked", Done: t.Checked}
				if t.Checked {
					it.State = "checked"
				}
				items = append(items, it)
				continue
			}
			if _, aerr := add(t); aerr != nil {
				return nil, explicit, aerr
			}
		}
		sortTrackerItems(items)
		return items, explicit, nil
	}

	for _, t := range parseTrackerTasks(body) {
		if !t.HasRef {
			continue
		}
		if len(items) >= trackerMaxReferences {
			return nil, false, fmt.Errorf("tracker references more than %d items", trackerMaxReferences)
		}
		// Inferred items are done only when closed, never merely ticked.
		t.Checked = false
		if _, aerr := add(t); aerr != nil {
			return nil, false, aerr
		}
	}

	events, terr := c.IssueTimelineSince(ctx, owner+"/"+name, number, time.Time{})
	if terr != nil {
		return nil, false, fmt.Errorf("reading timeline of %s/%s#%d: %w", owner, name, number, terr)
	}
	for _, ev := range events {
		if ev.Event != "cross-referenced" || ev.SourcePR <= 0 {
			continue
		}
		if ev.SourceRepo != "" && !strings.EqualFold(ev.SourceRepo, repoFull) {
			continue
		}
		ref := ChildRef{Number: ev.SourcePR}
		if seen[ref] {
			continue
		}
		it, pr, rerr := c.readTrackerItem(ctx, owner, name, ref)
		if rerr != nil {
			return nil, false, rerr
		}
		if !trackerRefersTo(pr.GetBody(), repoFull, number) {
			continue
		}
		seen[ref] = true
		if len(items) >= trackerMaxReferences {
			return nil, false, fmt.Errorf("tracker references more than %d items", trackerMaxReferences)
		}
		items = append(items, it)
	}
	sortTrackerItems(items)
	return items, false, nil
}

// sortTrackerItems orders evidence by reference so the fingerprint and the
// comment do not depend on enumeration order.
func sortTrackerItems(items []trackerItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Ref.Repo != b.Ref.Repo {
			return a.Ref.Repo < b.Ref.Repo
		}
		if a.Ref.Number != b.Ref.Number {
			return a.Ref.Number < b.Ref.Number
		}
		return a.Text < b.Text
	})
}

// closeDoneTracker closes an open tracker/umbrella issue with no sub-issue
// children once everything it references is finished (hivecommons/hive#19071).
// It returns true when the issue was closed. It costs nothing unless the repo
// opted in with trackers_too and the issue announces itself as a tracker; the
// reads are cached per tracker, and the close needs a second fresh read with
// the same finished evidence. The evidence comment is posted before the
// close; a failed or refused comment means no close.
func (c *Client) closeDoneTracker(ctx context.Context, in IssueStageInput, store *stagelabels.Store, key string, now time.Time, logger *slog.Logger) bool {
	if in.Held || !in.TrackerSignal || c.parentAutoClosePolicy(in.Repo) != config.ParentAutoCloseTrackersToo {
		return false
	}
	// Label/title exemptions cost no API call. Hold is respected here even
	// though trackers_too implies epics_too: a hold is an owner's pause.
	if HasHoldLabel(in.Labels) || gfi.IsTracking(in.Title, in.Labels, "") {
		return false
	}
	if !trackerReadDue(key, now) {
		return false
	}
	owner, name := c.splitRepo(in.Repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, in.Number)
	if err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: tracker unreadable, not closing", "repo", in.Repo, "issue", in.Number, "error", err)
		}
		return false
	}
	if !strings.EqualFold(issue.GetState(), "open") || issue.IsPullRequest() {
		return false
	}
	if why, tracking := isTrackingIssue(issue); tracking {
		if logger != nil {
			logger.Debug("issue stage labels: tracker not auto-closed, marked as a tracking issue", "repo", in.Repo, "issue", in.Number, "why", why)
		}
		return false
	}
	items, explicit, err := c.trackerEvidence(ctx, owner, name, in.Number, issue.GetBody())
	if err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: tracker references unreadable, not closing", "repo", in.Repo, "issue", in.Number, "error", err)
		}
		// Forget the settle state: the evidence is unknown.
		trackerSettled(key, now, "", false)
		return false
	}
	hash := trackerEvidenceHash(items)
	done := trackerAllDone(items)
	if !trackerSettled(key, now, hash, done) {
		return false
	}
	if by, kept := c.parentKeptOpenBy(ctx, owner, name, in.Number, logger); kept || by != "" {
		if logger != nil {
			logger.Info("issue stage labels: tracker kept open, reopened by a person after a hive auto-close", "repo", in.Repo, "issue", in.Number, "reopened_by", by)
		}
		return false
	}
	note := trackerEvidenceComment(items, explicit, hash)
	needle := trackerEvidenceMarker + hash + " -->"
	posted, cerr := c.IssueCommentsContain(ctx, in.Repo, in.Number, needle)
	if cerr != nil {
		if logger != nil {
			logger.Warn("issue stage labels: tracker comments unreadable, not closing", "repo", in.Repo, "issue", in.Number, "error", cerr)
		}
		return false
	}
	if !posted {
		if err := c.CreateIssueCommentBudgeted(ctx, in.Repo, in.Number, note); err != nil {
			if logger != nil {
				logger.Warn("issue stage labels: tracker evidence comment failed, not closing", "repo", in.Repo, "issue", in.Number, "error", err)
			}
			return false
		}
	}
	if _, err := stagelabels.Enforce(ctx, c.StageLabeler(), stagelabels.IssueItem, owner, name, in.Number, in.Labels, stagelabels.Desired{}); err != nil && logger != nil {
		logger.Warn("issue stage labels: clearing stage labels on tracker failed", "repo", in.Repo, "issue", in.Number, "error", err)
	}
	if err := c.CloseIssue(ctx, in.Repo, in.Number, IssueCloseOptions{StateReason: IssueStateReasonCompleted}); err != nil {
		if logger != nil {
			logger.Warn("issue stage labels: closing tracker failed", "repo", in.Repo, "issue", in.Number, "error", err)
		}
		return false
	}
	if store != nil {
		store.Forget(key)
	}
	if logger != nil {
		logger.Info("issue stage labels: tracker closed, all referenced work finished", "repo", in.Repo, "issue", in.Number, "evidence", hash, "references", len(items))
	}
	return true
}
